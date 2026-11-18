#!/usr/bin/env python3
"""AIRA-350 small-job lane: a MODEL simulation of the admission rules, lane on versus off.

This is a model of the rules in internal/daemon/admit.go evaluateAdmitQueue (worktree at
ae446ef1), written to test the design spec
docs/superpowers/specs/2026-11-18-small-job-lane-design.md. It is NOT the Go evaluator and
proves nothing about the Go code; the evaluator-seam tests of the spec's section 6 do that.
What it does is run the reviewers' counterexamples (round 2) with the production duty cycle:
a 5-minute hold, one backstop pass per hold, a head budget of half its reserve.

Modelled, from admit.go:
  - one pass per second; releases first, then arrivals, then the phase, then waiters in order
    (pushed latest first, then high FIFO, then normal FIFO: admitOrderLocked);
  - fit = RAM booking <= ceiling - headroom(jobs+1) - outstanding (ledger only, no physical
    floor), and CPU and VRAM ledgers (admit.go:2663-2674);
  - head = first job-level waiter refused on capacity that could fit an empty slice
    (admit.go:2770, admitCouldFitEmptyLocked);
  - protected = head exists and the waiter is job-level; sub-reservations are granted on fit
    alone (admit.go:2688);
  - pushed head: nothing passes; backstop pass (first pass at or after armed+maxHold): every
    fit passes; otherwise a fit passes only within bypassed + charge <= reserve/2, else it is
    withheld and arms the hold from idle (admit.go:2689-2698, 2762-2769, 2525-2540);
  - a suite parent's sub-reservations end when the parent ends.

Lane designs (each a set of switches):
  spec        the round-2 spec as written: concurrent cap on the lane tree, guard G (RAM with
              the per-job headroom term C2, and CPU), GPU jobs ineligible (C3), lane members'
              children counted (C7), lane sum cached and refreshed only after lane grants.
  spec-fresh  the same with the lane sum recomputed before every lane decision (finding 1).
  tb-min      concurrent cap on lane MEMBERS only, closed for a head once now - head.enqueued
              >= T; no G, no C2, no C3, no C7.
  tb-full     tb-min plus G (RAM with C2, CPU), C3 and C7.
  cum-min     a cumulative allowance per head: at most x bytes of small jobs pass each head
              through the lane (head.laneBypassed, the AIRA-307 counter pattern); no G, C2,
              C3 or C7.
  cum-g       cum-min plus guard G on the concurrent lane members.
In tb-* and cum-* the lane case is evaluated AFTER the backstop case, so a backstop pass is
exactly today's; in spec* it is before, as the spec wrote it.

Run (pure Python, no input files):
    aira confine --name <unique> -- python3 -I docs/dev/aira-350-lane-sim.py
Output is committed beside it as docs/dev/aira-350-lane-sim.out.txt.
"""

import heapq

INF = 10 ** 12
MAXHOLD = 300
X = 4096   # MiB
Y = 1024   # MiB
T_BOX = 600  # seconds, 2 x maxHold


class Job:
    def __init__(self, name, size, life, arrive=0, cpu=1, vram=0, sub=False, parent=None,
                 spent=False, workers=(), pre=False, lane=False, respawn=False, push=0):
        self.name, self.size, self.life, self.arrive = name, size, life, arrive
        self.cpu, self.vram, self.sub, self.parent = cpu, vram, sub, parent
        self.workers, self.pre, self.lane, self.respawn, self.push = workers, pre, lane, respawn, push
        self.state = "pending"
        self.grant = None
        self.end = None
        self.enq = None
        self.seq = 0
        self.bypassed = size // 2 if spent else 0
        self.lane_bypassed = 0


DESIGNS = {
    "off": None,
    "spec": dict(mode="conc", T=None, c7=True, c3=True, g=True, c2=True, gcpu=True, cached=True, before=True),
    "spec-fresh": dict(mode="conc", T=None, c7=True, c3=True, g=True, c2=True, gcpu=True, cached=False, before=True),
    "tb-min": dict(mode="conc", T=T_BOX, c7=False, c3=False, g=False, c2=False, gcpu=False, cached=False, before=False),
    "tb-full": dict(mode="conc", T=T_BOX, c7=True, c3=True, g=True, c2=True, gcpu=True, cached=False, before=False),
    "cum-min": dict(mode="cum", T=None, c7=False, c3=False, g=False, c2=False, gcpu=False, cached=False, before=False),
    "cum-g": dict(mode="cum", T=None, c7=False, c3=False, g=True, c2=True, gcpu=True, cached=False, before=False),
}


class Sim:
    def __init__(self, jobs, C, base=0, perjob=0, cpu_ceil=1 << 20, vram_ceil=0, armed0=None, design=None):
        self.C, self.base, self.perjob = C, base, perjob
        self.cpu_ceil, self.vram_ceil, self.armed, self.d = cpu_ceil, vram_ceil, armed0, design
        self.seq = 0
        self.count = 0
        self.pending = []  # heap of (arrive, insertion count, job)
        self.live = []     # queued and granted jobs
        for j in jobs:
            if j.pre:
                self.live.append(j)
                self._grant(j, 0)
            else:
                self._add(j)

    def _add(self, j):
        self.count += 1
        heapq.heappush(self.pending, (j.arrive, self.count, j))

    def headroom(self, jobs):
        return self.base + jobs * self.perjob

    def granted(self):
        return [j for j in self.live if j.state == "granted"]

    def _grant(self, j, t):
        j.state, j.grant = "granted", t
        j.end = t + j.life if j.life < INF else INF
        for (delay, size, life, vram, respawn) in j.workers:
            self._add(Job(j.name + ".w", size, life, arrive=t + delay, vram=vram, sub=True,
                          parent=j, respawn=respawn))

    def tree(self):
        members = [j for j in self.live if j.state == "granted" and j.lane]
        if self.d and self.d["c7"]:
            ids = set(id(m) for m in members)
            members = members + [j for j in self.live if j.state == "granted" and j.sub and id(j.parent) in ids]
        return (sum(j.size for j in members), sum(j.cpu for j in members), len(members))

    def lane_admits(self, head, w, t, cache):
        d = self.d
        if d is None or w.sub or w.size > Y:
            return False
        if d["c3"] and w.vram > 0:
            return False
        if d["T"] is not None and t - head.enq >= d["T"]:
            return False
        size, cpu, n = cache[0] if d["cached"] else self.tree()
        if d["mode"] == "cum":
            if head.lane_bypassed + w.size > X:
                return False
        elif size + w.size > X:
            return False
        if d["g"]:
            extra = (n + 1) * self.perjob if d["c2"] else 0
            if head.size + size + w.size + extra > self.C - self.headroom(1):
                return False
        if d["gcpu"] and head.cpu + cpu + w.cpu > self.cpu_ceil:
            return False
        return True

    def could_fit_empty(self, w):
        return (w.size <= self.C - self.headroom(1) and w.cpu <= self.cpu_ceil
                and (w.vram == 0 or w.vram <= self.vram_ceil))

    def step(self, t):
        # releases: a lease past its end, or whose parent has ended; a respawning worker is
        # replaced while its parent lives; a queued child of an ended parent is dropped
        changed = True
        while changed:
            changed = False
            for j in self.live:
                if j.state == "granted" and (j.end <= t or (j.parent is not None and j.parent.state != "granted")):
                    j.state = "released"
                    changed = True
                    if j.respawn and j.parent is not None and j.parent.state == "granted":
                        self._add(Job(j.name, j.size, j.life, arrive=t, vram=j.vram, sub=True,
                                      parent=j.parent, respawn=True))
                elif j.state == "queued" and j.parent is not None and j.parent.state == "released":
                    j.state = "released"
                    changed = True
        self.live = [j for j in self.live if j.state != "released"]
        while self.pending and self.pending[0][0] <= t:
            _, _, j = heapq.heappop(self.pending)
            if j.parent is not None and j.parent.state != "granted":
                continue
            j.state, j.enq = "queued", t
            self.seq += 1
            j.seq = self.seq
            self.live.append(j)
        phase = "idle"
        if self.armed is not None:
            if t >= self.armed + MAXHOLD:
                self.armed, phase = None, "yield"
            else:
                phase = "hold"
        queued = [j for j in self.live if j.state == "queued"]
        pushed = sorted([j for j in queued if j.push], key=lambda j: -j.push)
        rest = sorted([j for j in queued if not j.push], key=lambda j: j.seq)
        head = None
        cache = [self.tree()]
        g = self.granted()
        out, cpu_out, vram_out = sum(j.size for j in g), sum(j.cpu for j in g), sum(j.vram for j in g)
        njobs = len(g)
        for w in pushed + rest:
            available = self.C - self.headroom(njobs + 1) - out
            fits = (w.size <= available and w.cpu <= self.cpu_ceil - cpu_out
                    and (w.vram == 0 or w.vram <= self.vram_ceil - vram_out))
            protected = head is not None and not w.sub
            withheld, via = False, None
            if protected and fits:
                if head.push:
                    fits = False
                elif self.d is not None and self.d["before"] and self.lane_admits(head, w, t, cache):
                    via = "lane"
                elif phase == "yield":
                    pass
                elif self.d is not None and not self.d["before"] and self.lane_admits(head, w, t, cache):
                    via = "lane"
                elif head.bypassed + w.size > head.size // 2:
                    fits, withheld = False, True
            if fits:
                self._grant(w, t)
                out, cpu_out, vram_out, njobs = out + w.size, cpu_out + w.cpu, vram_out + w.vram, njobs + 1
                if via == "lane":
                    w.lane = True
                    head.lane_bypassed += w.size
                    cache[0] = self.tree()
                elif protected:
                    head.bypassed += w.size
                continue
            if withheld and phase == "idle":
                self.armed, phase = t, "hold"
            if head is None and not w.sub and self.could_fit_empty(w):
                head = w

    def run(self, t_end, watch):
        for t in range(t_end):
            self.step(t)
            if watch.state == "granted":
                return watch.grant
        return None


def fmt(v, t_end):
    return "never (>%ds)" % t_end if v is None else "%ds" % v


# ---- witnesses -------------------------------------------------------------------------------

def w1(design):
    """Finding 1, one pass: a lane member's exempt child grants inside the pass, then a lane
    candidate is judged on the cached sum. Net ceiling 12 GiB, head 8 GiB, ordinary 6 GiB, lane
    parent 1 GiB."""
    parent = Job("P", 1024, INF, pre=True, lane=True)
    jobs = [Job("ord", 6144, INF, pre=True), parent,
            Job("head", 8192, INF, spent=True),
            Job("P.child", 3072, INF, sub=True, parent=parent),
            Job("cand", 1024, INF)]
    sim = Sim(jobs, C=12288, armed0=-60, design=DESIGNS[design])
    sim.step(0)
    size, _, _ = sim.tree()
    cand = jobs[4]
    return "cand %s; lane tree %d MiB (x=4096); head+tree %d MiB vs ceiling 12288" % (
        "granted" if cand.state == "granted" else "held", size, 8192 + size)


def w2(design):
    """Finding 2: finite zero-VRAM lane parents with staggered finite GPU children keep 1 GiB of
    VRAM occupied while an 8 GiB-VRAM head needs the whole 8 GiB ceiling."""
    jobs = [Job("gpu0", 1024, 30, vram=1024, pre=True), Job("head", 4096, INF, vram=8192, spent=True)]
    for k in range(200):
        jobs.append(Job("p%d" % k, 1024, 30, arrive=1 + 15 * k, workers=[(1, 256, INF, 1024, False)]))
    sim = Sim(jobs, C=49152, vram_ceil=8192, design=DESIGNS[design])
    return sim.run(3600, jobs[1])


def w3(design, offset=0, t_end=3000):
    """Finding 3: three finite 5 GiB ordinary leases rotate through backstop refills (releases
    350 s apart, each refill lives 1050 s); 1 GiB small jobs arrive every 5 s from `offset` and
    live 20 s. Ceiling 20 GiB, head 8 GiB with its budget spent."""
    jobs = [Job("o1", 5120, 350, pre=True), Job("o2", 5120, 700, pre=True), Job("o3", 5120, 1050, pre=True),
            Job("head", 8192, INF, spent=True)]
    jobs += [Job("r%d" % k, 5120, 1050, arrive=1) for k in range(20)]
    jobs += [Job("s%d" % k, 1024, 20, arrive=offset + 5 * k) for k in range((t_end - offset) // 5)]
    sim = Sim(jobs, C=20480, design=DESIGNS[design])
    return sim.run(t_end, jobs[3])


def w8(design, spent=True, pool=False):
    """Finding 8: ceiling 46 GiB, head 4 GiB; 44 x 1 GiB non-lane leases end one per 10 s; a
    1 GiB suite parent arrives every 60 s and lives 900 s. Without `pool`, each suite keeps 4
    workers of 2 GiB that live 60 s and are replaced (worker turnover gives the head its room).
    With `pool`, each suite requests 32 workers of 2 GiB, one every 10 s from 5 s after its
    grant, that live as long as the suite (the 13:55 snapshot's worker ages, 399-660 s beside a
    663 s parent, are of this shape), so its workers want more than the slice and take every
    freed 2 GiB."""
    jobs = [Job("n%d" % k, 1024, 10 * (k + 1), pre=True) for k in range(44)]
    head = Job("head", 4096, INF, spent=spent)
    jobs.append(head)
    for k in range(40):
        if pool:
            workers = [(5 + 10 * i, 2048, INF, 0, False) for i in range(32)]
        else:
            workers = [(5, 2048, 60, 0, True)] * 4
        jobs.append(Job("suite%d" % k, 1024, 900, arrive=60 * k, workers=workers))
    sim = Sim(jobs, C=47104, design=DESIGNS[design])
    return sim.run(6000, head)


def w12(design):
    """Finding 12: 1.5 GiB of leases that never end, 10 GiB of transient leases ending one per
    30 s, a 39 GiB head (fits an empty slice: ceiling net 44 GiB with 2 GiB + 64 MiB/job
    headroom), 512 MiB small jobs every 5 s living 40 s (demand about x)."""
    jobs = [Job("perm%d" % k, 512, INF, pre=True) for k in range(3)]
    jobs += [Job("tr%d" % k, 1024, 30 * (k + 1), pre=True) for k in range(10)]
    head = Job("head", 39936, INF, spent=True)
    jobs.append(head)
    jobs += [Job("s%d" % k, 512, 40, arrive=5 * k) for k in range(1200)]
    sim = Sim(jobs, C=47168, base=2048, perjob=64, design=DESIGNS[design])
    return sim.run(6000, head)


def w0(design):
    """Illustrative only (not evidence): the 13:55 queue on a model slice. Ceiling and headroom
    as the 13:55 snapshot (48640 MiB effective, 2 GiB + 64 MiB/job, 32 CPU slots, 1 slot per
    lease); 8 never-ending servers (1536 MiB); the other 20 leases (40907 MiB) end one every
    60 s in a fixed shuffled order; queue 5120 (head, budget spent), 2154, 2700, 1024, 256, 512,
    512, 512, 1024; then a 256 MiB job every 30 s living 120 s. Returns the head's grant time,
    the two mid-size jobs' grant times, and the small jobs' mean wait."""
    servers = [128, 512, 64, 64, 512, 128, 64, 64]
    transient = [2048, 3211, 256, 5120, 1024, 4096, 4096, 4096, 4096, 2048, 1024, 1024, 2048, 2048,
                 1024, 1024, 1024, 1024, 512, 64]
    order = [7, 2, 15, 0, 11, 18, 4, 13, 9, 1, 19, 6, 16, 3, 10, 14, 8, 17, 5, 12]
    jobs = [Job("srv%d" % k, s, INF, pre=True) for k, s in enumerate(servers)]
    jobs += [Job("t%d" % k, transient[k], 60 * (1 + order.index(k)), pre=True) for k in range(20)]
    head = Job("head", 5120, INF, spent=True)
    jobs.append(head)
    mids = [Job("mid2154", 2154, 600), Job("mid2700", 2700, 600)]
    jobs += mids
    smalls = [Job("q%d" % k, s, 300) for k, s in enumerate([1024, 256, 512, 512, 512, 1024])]
    jobs += smalls
    stream = [Job("st%d" % k, 256, 120, arrive=30 * (k + 1)) for k in range(40)]
    jobs += stream
    sim = Sim(jobs, C=48640, base=2048, perjob=64, cpu_ceil=32, armed0=-60, design=DESIGNS[design])
    for t in range(3600):
        sim.step(t)
    waits = [(j.grant if j.grant is not None else 3600) - (j.enq if j.enq is not None else j.arrive)
             for j in smalls + stream]
    return "head %s, 2154 %s, 2700 %s, small mean wait %.0fs" % (
        fmt(head.grant, 3600), fmt(mids[0].grant, 3600), fmt(mids[1].grant, 3600), sum(waits) / len(waits))


def main():
    names = list(DESIGNS)
    print("AIRA-350 lane model simulation (a model of the rules, not the Go evaluator)")
    print("maxHold=%ds x=%dM y=%dM time-box T=%ds (tb-*), one pass per second" % (MAXHOLD, X, Y, T_BOX))
    print()
    print("## W1 finding 1 (one pass; spec designs only)")
    for name in ("spec", "spec-fresh"):
        print("%-10s %s" % (name, w1(name)))
    print()
    rows = [("W2 finding 2, VRAM children of lane parents", w2, 3600),
            ("W3 finding 3, backstop refill of ordinary leases", w3, 3000),
            ("W8 finding 8, lane-admitted suites (4 workers each), head budget spent", lambda d: w8(d, True), 6000),
            ("W8b the same with the head's budget fresh (today's exposure)", lambda d: w8(d, False), 6000),
            ("W8c finding 8, suites with a long-lived 32-worker pool, head budget spent",
             lambda d: w8(d, True, True), 6000),
            ("W8d the same with the head's budget fresh (today's exposure)", lambda d: w8(d, False, True), 6000),
            ("W12 finding 12, never-ending non-lane leases", w12, 6000)]
    for title, fn, t_end in rows:
        print("## " + title + ": the head's grant time")
        for name in names:
            print("%-10s %s" % (name, fmt(fn(name), t_end)))
        print()
    print("## W3 phase sweep: small jobs start at offset 0, 40, ..., 280 s (8 runs, horizon 3000 s)")
    print("%-10s %s" % ("offset", " ".join("%6d" % off for off in range(0, 300, 40))))
    for name in names:
        res = [w3(name, off) for off in range(0, 300, 40)]
        print("%-10s %s   starved in %d of 8" % (
            name, " ".join("%6s" % ("never" if r is None else "%ds" % r) for r in res), sum(r is None for r in res)))
    print()
    print("## W0 illustrative, the 13:55 queue on a model slice (not evidence)")
    for name in names:
        print("%-10s %s" % (name, w0(name)))


if __name__ == "__main__":
    main()
