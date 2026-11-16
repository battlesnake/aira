#!/usr/bin/env python3
"""AIRA-324 enforce gate: counterfactual replay of the real confine timeline.

Spec: docs/superpowers/specs/2026-11-13-history-charge-design.md, section 6 item 2.
Report: docs/dev/aira-324-enforce-gate-report.md. Committed output:
docs/dev/aira-324-enforce-gate-replay.out.txt.

Inputs (both read-only; never point --db at the live store):
  --db       a COPY of ~/.local/state/aira/state.db (copy state.db and state.db-wal)
  --journal  a saved `journalctl --user -u aira-daemon.service -o short-unix` dump;
             only the 'slice ceiling throttled' and 'pin-charge observe' lines are read

Run (confined, measured peak 53M):
  cp ~/.local/state/aira/state.db ~/tmp/aira-324-gate-state.db
  cp ~/.local/state/aira/state.db-wal ~/tmp/aira-324-gate-state.db-wal
  journalctl --user -u aira-daemon.service --since "2026-11-15 08:00" -o short-unix \
      > ~/tmp/aira-324-journal.txt
  peak=$(cat /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/aira.slice/memory.peak)
  aira confine --memory-reserve 128M -- python3 -I docs/dev/aira-324-enforce-gate-replay.py \
      --db ~/tmp/aira-324-gate-state.db --journal ~/tmp/aira-324-journal.txt --slice-peak "$peak"
  python3 -I docs/dev/aira-324-enforce-gate-replay.py --selftest

Model (every simplification is listed in the report, section "What the replay ignores"):
  * A job is one confine row with all four AIRA-323 facts: arrival = started_at_ms -
    wait_ms, start = started_at_ms, end = start + wall_ms. Its duration is fixed: starting
    it earlier does not change how long it runs.
  * Booking: pin rows (budget_basis 'cap:operator:--memory-reserve') are the discountable
    class and are booked ceil(factor x budget); every other row is booked its budget.
  * Ledger: a waiter fits when booking <= ceiling(t) - headroom - max(booked + H,
    peaks + H), where 'peaks' is the sum of the running jobs' memory.peak (each job is
    assumed to hold its peak from start to end) and H is the hidden load, the bytes booked
    by jobs the table cannot see (aitest workers, failed runs, long-running services).
    H is swept as a constant and, as the 'residual' model, put back minute by minute from
    the journal (ledger ceiling - visible booked); the calibration picks the model that
    reproduces the observed waits. Headroom is the daemon's 2 GiB + 64 MiB x (jobs + 1).
  * Physical fit (discounted waiters only): pin <= ceiling(t) - headroom - (peaks + H).
    Two readings of 'peaks', used in BOTH the ledger max() and the physical-fit check:
    'clairvoyant' (running jobs' FUTURE peaks: the daemon cannot see them, it reads
    memory.current) and 'blind' (0: every grant of a burst sees an idle box, the most
    permissive admission for these arrivals). The memory numbers are quoted for 'blind'.
  * CPU: 32 slots (2 per core x 16 cores), one per job, hidden jobs not counted.
  * Part 5 adds a first-order estimate and a hard bound on the REAL slice (journal sliceAnon,
    memory.peak), which include the hidden jobs.
  * Order: three policies bound the daemon's real one (head protection, AIRA-307 bypass
    budget, --front pushes, priority tiers, the fairness freeze): 'fifo' (nothing passes a
    blocked head), 'bypass' (later jobs may pass a blocked head while their bookings sum
    to at most half the head's booking: the AIRA-307 budget), 'backfill' (any fitting job
    goes).
"""
import argparse
import bisect
import collections
import heapq
import math
import os
import re
import sqlite3
import statistics
import sys

GIB = 1 << 30
MIB = 1 << 20
MEMORY_HIGH = 60 * GIB          # aira.slice MemoryHigh on this box (64424509440)
SAFE = 0.9 * MEMORY_HIGH        # the spec's 0.9 x MemoryHigh (54 GiB)
CPU_SLOTS = 32
HEADROOM_BASE = 2 * GIB         # admitSliceHeadroomBaseDefault
HEADROOM_PER_JOB = 64 * MIB     # admitSliceHeadroomSupervisorDefault
PIN_BASIS = "cap:operator:--memory-reserve"
FLOOR = MIB                     # pinChargeFloor


def charge_for(pin, factor):
    """discountedPinCharge: ceil(pin x factor) clamped to [1 MiB, pin], in whole percent."""
    pct = int(round(factor * 100))
    c = -(-pin * pct // 100)
    return min(max(c, FLOOR), pin)


class Job:
    __slots__ = ("idx", "arrival", "start", "dur", "peak", "pin", "pin_class", "oom", "basis")

    def __init__(self, idx, arrival, start, dur, peak, pin, basis, oom):
        self.idx, self.arrival, self.start, self.dur = idx, arrival, start, dur
        self.peak, self.pin, self.basis, self.oom = peak, pin, basis, oom
        self.pin_class = basis == PIN_BASIS


def load_jobs(db):
    if os.path.realpath(db) == os.path.realpath(os.path.expanduser("~/.local/state/aira/state.db")):
        sys.exit("refusing to read the live store; copy it to ~/tmp first")
    c = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = c.execute(
        "select rowid, started_at_ms, wait_ms, wall_ms, coalesce(peak_rss, 0), budget, budget_basis, oom "
        "from confine_peak_history where kind = 'confine' and started_at_ms is not null "
        "and wait_ms is not null and wall_ms is not null and budget is not null "
        "order by started_at_ms - wait_ms, rowid").fetchall()
    jobs = []
    for i, (rid, st, wait, wall, peak, budget, basis, oom) in enumerate(rows):
        jobs.append(Job(i, st - wait, st, wall, peak, budget, basis, oom))
    extra = c.execute(
        "select kind, count(*) from confine_peak_history where at >= ? and at <= ? "
        "and (kind != 'confine' or started_at_ms is null) group by kind",
        (iso(min(j.arrival for j in jobs)), iso(max(j.start + j.dur for j in jobs)))).fetchall() if jobs else []
    return jobs, extra


def iso(ms):
    import datetime
    return datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")


CEIL_RE = re.compile(r"slice ceiling throttled \(enforce\): (\d+) effective / (\d+) configured, sliceAnon=(\d+)")
OBS_RE = re.compile(r"pin-charge observe: scope=\S+ pin=(\d+) charged=(\d+) would-charge=(\d+) factor=\S+ "
                    r"physical-free=(-?\d+) physical-fit=(\w+)")


def load_journal(path):
    """(ceiling series [(ms, effective, sliceAnon)], observe lines [(ms, pin, would, free, fit)])."""
    ceil, obs = [], []
    with open(path, errors="replace") as f:
        for line in f:
            head = line.split(" ", 1)[0]
            try:
                ms = int(float(head) * 1000)
            except ValueError:
                continue
            m = CEIL_RE.search(line)
            if m:
                ceil.append((ms, int(m.group(1)), int(m.group(3))))
                continue
            m = OBS_RE.search(line)
            if m:
                obs.append((ms, int(m.group(1)), int(m.group(3)), int(m.group(4)), m.group(5) == "true"))
    ceil.sort()
    return ceil, obs


class Ceiling:
    """Ledger ceiling as a function of time: a constant, or the journal's effective
    ceiling (step function, last value held) less the daemon's headroom."""

    def __init__(self, const=None, series=None, headroom=True):
        self.const, self.headroom = const, headroom
        self.t = [s[0] for s in series] if series else []
        self.v = [s[1] for s in series] if series else []

    def at(self, t, njobs):
        if self.const is not None:
            base = self.const
        else:
            i = bisect.bisect_right(self.t, t) - 1
            base = self.v[max(i, 0)]
        if self.headroom:
            base -= HEADROOM_BASE + HEADROOM_PER_JOB * (njobs + 1)
        return base

    def changes(self, lo, hi):
        if self.const is not None:
            return []
        return [t for t in self.t if lo < t < hi]


def simulate(jobs, ceiling, factor=1.0, policy="bypass", phys="clairvoyant", hidden=0, cpu_slots=CPU_SLOTS,
             stats=None):
    """Event-driven replay. Returns {idx: start_ms}. Arrival order is FIFO by (arrival, idx).
    stats (a dict, optional) receives 'phys_refused' (grants the physical-fit rule held back)
    and 'forced' (waiters granted with nothing running although they did not fit: in the
    real timeline they ran, so the hidden load cannot hold them forever)."""
    if stats is None:
        stats = {}
    stats.setdefault("phys_refused", 0)
    stats.setdefault("forced", 0)
    stats.setdefault("cpu_held", 0)
    arrivals = sorted(jobs, key=lambda j: (j.arrival, j.idx))
    book = {j.idx: (charge_for(j.pin, factor) if j.pin_class else j.pin) for j in jobs}
    disc = {j.idx: book[j.idx] < j.pin for j in jobs}
    times = sorted(set(ceiling.changes(-1, 1 << 62)))   # ceiling changes: a waiter may fit at one
    ends = []                      # heap of (end, idx)
    running = {}                   # idx -> job
    booked = peaks = 0
    queue = []                     # FIFO list of jobs
    bypassed = collections.defaultdict(int)
    start = {}
    ai = 0
    ti = 0

    def evaluate(t):
        nonlocal booked, peaks
        head = None
        granted = []
        for w in queue:
            n = len(running)
            ceil = ceiling.at(t, n)
            # the daemon's checkedAvailable: the larger of the ledger and the physical
            # reading. 'blind' reads the physical term as 0 here too (an idle box).
            seen = peaks if phys == "clairvoyant" else 0
            avail = ceil - max(booked + hidden, seen + hidden)
            c = book[w.idx]
            fits = c <= avail and n < cpu_slots
            if c <= avail and n >= cpu_slots:
                stats["cpu_held"] += 1
            if fits and disc[w.idx]:
                physfree = ceil - (seen + hidden)
                fits = w.pin <= physfree
                if not fits:
                    stats["phys_refused"] += 1
            if not fits and not running and head is None:
                fits = True
                stats["forced"] += 1
            if head is not None and fits:      # fifo never gets here: it stops at the head
                if policy == "bypass":
                    if bypassed[head.idx] + c > book[head.idx] // 2:
                        fits = False
                    else:
                        bypassed[head.idx] += c
            if fits:
                granted.append(w)
                start[w.idx] = t
                running[w.idx] = w
                booked += c
                peaks += w.peak
                heapq.heappush(ends, (t + w.dur, w.idx))
            elif head is None:
                head = w
                if policy == "fifo":
                    break
        if granted:
            gs = set(id(g) for g in granted)
            queue[:] = [w for w in queue if id(w) not in gs]

    while ai < len(arrivals) or ends or queue:
        nxt = []
        if ai < len(arrivals):
            nxt.append(arrivals[ai].arrival)
        if ends:
            nxt.append(ends[0][0])
        if ti < len(times) and queue:
            nxt.append(times[ti])
        if not nxt:
            # queue non-empty but nothing will ever change: refuse rather than spin
            raise RuntimeError("replay wedged: %d queued, nothing running" % len(queue))
        t = min(nxt)
        while ends and ends[0][0] <= t:
            _, idx = heapq.heappop(ends)
            j = running.pop(idx)
            booked -= book[idx]
            peaks -= j.peak
        while ai < len(arrivals) and arrivals[ai].arrival <= t:
            queue.append(arrivals[ai])
            ai += 1
        while ti < len(times) and times[ti] <= t:
            ti += 1
        evaluate(t)
    return start


def q(xs, p):
    if not xs:
        return float("nan")
    s = sorted(xs)
    return s[min(len(s) - 1, int(p * len(s)))]


def waits(jobs, start):
    return [start[j.idx] - j.arrival for j in jobs]


def step(jobs, start, value):
    """Sorted step function of sum(value(j)) over running jobs: [(t, level_after_t)]."""
    ev = []
    for j in jobs:
        s = start[j.idx]
        v = value(j)
        if j.dur <= 0 or v == 0:
            continue
        ev.append((s, v))
        ev.append((s + j.dur, -v))
    ev.sort(key=lambda e: (e[0], e[1]))   # ends (negative) first at equal t
    out, lvl = [], 0
    for t, d in ev:
        lvl += d
        if out and out[-1][0] == t:
            out[-1] = (t, lvl)
        else:
            out.append((t, lvl))
    return out


def level_stats(st, lo, hi):
    """Duration-weighted mean, p99 and max of a step function on [lo, hi)."""
    segs = []
    for i, (t, lvl) in enumerate(st):
        t2 = st[i + 1][0] if i + 1 < len(st) else hi
        a, b = max(t, lo), min(t2, hi)
        if b > a:
            segs.append((lvl, b - a))
    covered = sum(d for _, d in segs)
    if hi - lo > covered:                  # before the first event: nothing running
        segs.append((0, hi - lo - covered))
    total = hi - lo
    if total <= 0:
        return 0.0, 0, 0
    mean = sum(l * d for l, d in segs) / total
    acc, p99 = 0, 0
    for l, d in sorted(segs):
        acc += d
        if acc >= 0.99 * total:
            p99 = l
            break
    return mean, p99, max(l for l, _ in segs)


def minute_max(st, lo, hi):
    """{minute_start_ms: max level reached in that minute}."""
    out = {}
    lvl = 0
    i = 0
    m = lo - lo % 60000
    while m < hi:
        while i < len(st) and st[i][0] <= m:     # an event AT the boundary belongs to this minute's start
            lvl = st[i][1]
            i += 1
        mx = lvl
        j = i
        while j < len(st) and st[j][0] < m + 60000:
            mx = max(mx, st[j][1])
            j += 1
        out[m] = mx
        m += 60000
    return out


def level_at(st, t, keys=None):
    """Level just after every event at t (a job starting at t counts itself)."""
    keys = keys if keys is not None else [s[0] for s in st]
    i = bisect.bisect_right(keys, t) - 1
    return st[i][1] if i >= 0 else 0


def memory_view(jobs, start, lo, hi):
    """The spec's memory test: concurrent sum of memory.peak (each job holding its peak for
    its whole run), duration-weighted and job(burst)-weighted, over every minute and in the
    busiest clock hour (the hour with the highest duration-weighted mean)."""
    st = step(jobs, start, lambda j: j.peak)
    hours = {}
    h = lo - lo % 3600000
    while h < hi:
        hours[h] = level_stats(st, max(h, lo), min(h + 3600000, hi))
        h += 3600000
    busiest = max(hours, key=lambda k: hours[k][0])
    mm = minute_max(st, lo, hi)
    keys = [e[0] for e in st]
    at_grant = [level_at(st, start[j.idx], keys) for j in jobs if lo <= start[j.idx] < hi]
    in_hour = [level_at(st, start[j.idx], keys) for j in jobs if busiest <= start[j.idx] < busiest + 3600000]
    dmean, dp99, dmax = hours[busiest]
    return {
        "busiest_hour": iso(busiest)[:13] + "Z",
        "hour_dur_mean": dmean, "hour_dur_p99": dp99, "hour_max": dmax,
        "hour_job_p50": q(in_hour, 0.5), "hour_job_p99": q(in_hour, 0.99), "hour_jobs": len(in_hour),
        "all_job_p99": q(at_grant, 0.99),
        "minute_max": max(mm.values()) if mm else 0,
        "minutes_over_safe": sum(1 for v in mm.values() if v > SAFE),
        "minutes": len(mm),
        "step": st,
    }


def concurrency(jobs, start, lo, hi):
    n = step(jobs, start, lambda j: 1)
    b = step(jobs, start, lambda j: j.pin)
    nm, _, nx = level_stats(n, lo, hi)
    bm, _, bx = level_stats(b, lo, hi)
    return nm, nx, bm, bx


def fmt_g(b):
    return "%.1f" % (b / GIB)


def pct(x):
    return "%.1f%%" % (100 * x)


def wait_s(xs, p):
    return q(xs, p) / 1000.0


# --------------------------------------------------------------------------- report

def residual_series(jobs, ceil_series, frac):
    """The journal ceiling less frac x the RESIDUAL: ledger ceiling - visible booked at each
    throttle line, on the observed timeline. The residual is the booking of every job the
    table cannot see, plus any capacity the daemon left idle (the fairness-freeze hold, a
    blocked head): an upper bound on the hidden load, so frac = 1 overstates it."""
    obs = {j.idx: j.start for j in jobs}
    sb, sn = step(jobs, obs, lambda j: j.pin), step(jobs, obs, lambda j: 1)
    kb, kn = [e[0] for e in sb], [e[0] for e in sn]
    out = []
    for t, eff, anon in ceil_series:
        n = level_at(sn, t, kn)
        gap = max(0, eff - HEADROOM_BASE - HEADROOM_PER_JOB * (n + 1) - level_at(sb, t, kb))
        out.append((t, eff - int(frac * gap), anon))
    return out


def score_of(rep, obs):
    """Mean |log ratio| of wait p50/p90/p99 (+1 s) and of the share that waited > 60 s."""
    terms = [abs(math.log((q(rep, p) + 1000) / (q(obs, p) + 1000))) for p in (.5, .9, .99)]
    r60 = sum(1 for x in rep if x > 60000) / len(rep)
    o60 = sum(1 for x in obs if x > 60000) / len(obs)
    terms.append(abs(math.log((r60 + 0.001) / (o60 + 0.001))))
    return sum(terms) / len(terms)


def report(jobs, extra, ceil_series, obs, out, slice_peak=None):
    P = lambda *a: print(*a, file=out)
    lo = min(j.arrival for j in jobs)
    hi = max(j.start + j.dur for j in jobs)
    # metrics start one hour in: jobs that started before the window (old clients, no
    # facts) are invisible, so the first hour under-counts the load.
    mlo = lo + 3600000
    obs_start = {j.idx: j.start for j in jobs}
    pins = [j for j in jobs if j.pin_class]
    m_jobs = [j for j in jobs if j.start >= mlo]
    ow = [j.start - j.arrival for j in m_jobs]
    P("# AIRA-324 enforce-gate replay")
    P("window %s .. %s UTC (%.1f h); metrics from %s (first hour dropped: warm-up)" %
      (iso(lo), iso(hi), (hi - lo) / 3.6e6, iso(mlo)))
    P("jobs %d: pin(--memory-reserve) %d, auto %d, --memory-max %d; oom rows %d (pin %d)" % (
        len(jobs), len(pins), sum(1 for j in jobs if j.basis == "cap:auto:daemon-reserve"),
        sum(1 for j in jobs if j.basis == "cap:operator:--memory-max"), sum(j.oom for j in jobs),
        sum(j.oom for j in pins)))
    P("rows in the window WITHOUT a timeline (part of the hidden load): %s" % (
        ", ".join("%s=%d" % e for e in extra) or "none"))
    P("")

    # ---- part 0: what the journal and the store say directly
    P("## 0. Observed facts (journal, store)")
    nm, nx, bm, bx = concurrency(jobs, obs_start, mlo, hi)
    P("observed, visible jobs: wait p50 %.1fs p90 %.1fs p99 %.1fs; waited >60s %s; concurrency mean %.1f max %d; "
      "visible booked mean %s max %s GiB" % (wait_s(ow, .5), wait_s(ow, .9), wait_s(ow, .99),
                                             pct(sum(1 for w in ow if w > 60000) / len(ow)), nm, nx, fmt_g(bm), fmt_g(bx)))
    if ceil_series:
        eff = [c[1] for c in ceil_series if mlo <= c[0] <= hi]
        anon = [c[2] for c in ceil_series if mlo <= c[0] <= hi]
        P("effective ceiling (throttle lines, one a minute, %d in the window): min %s median %s max %s GiB "
          "(static 64 GiB; the ledger ceiling is this less 2 GiB + 64 MiB x (jobs + 1))" % (
              len(eff), fmt_g(min(eff)), fmt_g(statistics.median(eff)), fmt_g(max(eff))))
        P("slice anon memory (sliceAnon, same lines, ALL jobs incl. hidden): median %s p99 %s max %s GiB "
          "= max %.3f of MemoryHigh" % (fmt_g(statistics.median(anon)), fmt_g(q(anon, 0.99)), fmt_g(max(anon)),
                                         max(anon) / MEMORY_HIGH))
        st = step(jobs, obs_start, lambda j: j.peak)
        sb = step(jobs, obs_start, lambda j: j.pin)
        sn = step(jobs, obs_start, lambda j: 1)
        ks, kb, kn = [e[0] for e in st], [e[0] for e in sb], [e[0] for e in sn]
        pairs = [(level_at(st, t, ks), a, level_at(sb, t, kb), e, level_at(sn, t, kn))
                 for t, e, a in ceil_series if mlo <= t <= hi]
        ratio = [p[0] / p[1] for p in pairs if p[1] > 0]
        P("held-peak sum of VISIBLE jobs / sliceAnon of ALL jobs, same minute: p10 %.2f median %.2f; "
          "sliceAnon above it in %d of %d minutes (largest excess %s GiB)" % (
              q(ratio, 0.10), statistics.median(ratio), sum(1 for p in pairs if p[1] > p[0]), len(pairs),
              fmt_g(max(0, max(p[1] - p[0] for p in pairs)))))
        # a waiting interval as a pseudo-run: arrival .. start
        wjobs = [Job(j.idx, j.arrival, j.arrival, j.start - j.arrival, 0, 0, "", 0) for j in jobs if j.start - j.arrival >= 1000]
        wst = step(wjobs, {j.idx: j.arrival for j in wjobs}, lambda j: 1)
        kw = [e[0] for e in wst]
        gap_q = [p[3] - HEADROOM_BASE - HEADROOM_PER_JOB * (p[4] + 1) - p[2]
                 for (t, e, a), p in zip([c for c in ceil_series if mlo <= c[0] <= hi], pairs) if level_at(wst, t, kw) > 0]
        P("residual = ledger ceiling - visible booked, in the %d minutes a visible job was waiting >= 1 s: "
          "p10 %s median %s p90 %s GiB (hidden jobs + idle-held capacity; the replay cannot see it)" % (
              len(gap_q), fmt_g(q(gap_q, .1)), fmt_g(q(gap_q, .5)), fmt_g(q(gap_q, .9))))
    if obs:
        o_lo = min(o[0] for o in obs)
        marg = [o[3] - o[1] for o in obs]
        pins_since = sum(1 for j in pins if j.start >= o_lo)
        P("observe lines %d (from %s): physical-fit true %d; physical-free minus pin: min %s p01 %s median %s GiB" % (
            len(obs), iso(o_lo), sum(1 for o in obs if o[4]), fmt_g(min(marg)), fmt_g(q(marg, 0.01)),
            fmt_g(statistics.median(marg))))
        P("discountable grants (observe lines) %d vs pin rows started since %d: ratio %.2f. More grants than rows "
          "means at least %d discountable grants left no row (failed, killed or still running runs)" % (
              len(obs), pins_since, len(obs) / pins_since if pins_since else float("nan"),
              max(0, len(obs) - pins_since)))
    if slice_peak:
        P("aira.slice memory.peak (anon + cache, since the slice was created): %s GiB = %.3f of MemoryHigh" % (
            fmt_g(slice_peak), slice_peak / MEMORY_HIGH))
    P("")

    # ---- part 1: calibration
    P("## 1. Calibration: baseline (factor 1.0) replay of the same arrivals vs observed")
    P("score = mean |log ratio| replayed/observed of wait p50, p90, p99 (+1 s) and the share waiting > 60 s "
      "(0 = exact). hour r = Pearson r of the hourly mean wait, replayed vs observed (1 = same hours busy).")
    P("%-26s %-8s %8s %8s %8s %7s %5s %6s %6s %6s" % ("ceiling", "policy", "p50 s", "p90 s", "p99 s", ">60s",
                                                     "maxc", "hour r", "score", "forced"))
    P("%-26s %-8s %8.1f %8.1f %8.1f %7s %5d %6s %6s %6s" % ("OBSERVED", "-", wait_s(ow, .5), wait_s(ow, .9),
                                                          wait_s(ow, .99), pct(sum(1 for w in ow if w > 60000) / len(ow)),
                                                          nx, "-", "-", "-"))
    configs = []
    for c in (36, 40, 44, 48, 62):
        configs.append(("const %d GiB" % c, Ceiling(const=c * GIB, headroom=False), 0))
    if ceil_series:
        for hgib in (0, 8, 12, 16):
            configs.append(("journal, H=%d GiB" % hgib, Ceiling(series=ceil_series), hgib * GIB))
        for frac in (0.5, 0.75, 1.0):
            configs.append(("journal - %.2f x residual" % frac, Ceiling(series=residual_series(jobs, ceil_series, frac)), 0))
    hourly_obs = hourly(m_jobs, ow)
    cal = []
    for name, ce, hid in configs:
        for pol in ("fifo", "bypass", "backfill"):
            stt = {}
            s = simulate(jobs, ce, 1.0, pol, "clairvoyant", hid, stats=stt)
            wm = [s[j.idx] - j.arrival for j in m_jobs]
            mc = concurrency(jobs, s, mlo, hi)[1]
            r = pearson(hourly_obs, hourly(m_jobs, wm))
            sc = score_of(wm, ow)
            cal.append((sc, name, pol, ce, hid, r))
            P("%-26s %-8s %8.1f %8.1f %8.1f %7s %5d %6.2f %6.2f %6d" % (
                name, pol, wait_s(wm, .5), wait_s(wm, .9), wait_s(wm, .99),
                pct(sum(1 for x in wm if x > 60000) / len(wm)), mc, r, sc, stt["forced"]))
    cal.sort(key=lambda c: c[0])
    best = cal[0]
    P("best fit by score: %s, %s (score %.2f, hour r %.2f)" % (best[1], best[2], best[0], best[5]))
    P("")

    # ---- part 2: counterfactual
    P("## 2. Counterfactual: pin rows booked at ceil(factor x pin), against the SAME config's baseline replay")
    P("earlier/later = jobs whose replayed start moved by more than 1 s; d = change in wait (s), metric window."
      " phys-held = grants the physical-fit rule held back (clairvoyant reading; 0 means it never bound,"
      " so the blind reading gives the same timeline).")
    targets = [(best[1], best[2], best[3], best[4])]
    for name, ce, hid in configs:
        if name.startswith("journal - 1.00") or name in ("const 44 GiB", "const 48 GiB", "const 62 GiB"):
            targets.append((name, "bypass", ce, hid))
    for pol in ("fifo", "bypass", "backfill"):
        targets.append((best[1], pol, best[3], best[4]))
    seen = set()
    P("%-26s %-8s %4s %7s %6s %7s %7s %7s %6s %9s %8s" % (
        "ceiling", "policy", "fac", "earlier", "later", "p50 d", "p90 d", "p99 d", ">60s", "phys-held", "cpu-held"))
    mem_rows = []
    for name, pol, ce, hid in targets:
        if (name, pol) in seen:
            continue
        seen.add((name, pol))
        bst = {}
        base = simulate(jobs, ce, 1.0, pol, "clairvoyant", hid, stats=bst)
        bw = [base[j.idx] - j.arrival for j in m_jobs]
        P("%-26s %-8s %4.1f %7s %6s %7s %7s %7s %6s %9s %8d" % (name, pol, 1.0, "-", "-", "-", "-", "-",
                                                             pct(sum(1 for x in bw if x > 60000) / len(bw)), "-",
                                                             bst["cpu_held"]))
        mem_rows.append((name, pol, 1.0, memory_view(jobs, base, mlo, hi)))
        for fac in (0.9, 0.8):
            stt = {}
            s = simulate(jobs, ce, fac, pol, "clairvoyant", hid, stats=stt)
            cw = [s[j.idx] - j.arrival for j in m_jobs]
            earlier = sum(1 for j in jobs if s[j.idx] < base[j.idx] - 1000)
            later = sum(1 for j in jobs if s[j.idx] > base[j.idx] + 1000)
            P("%-26s %-8s %4.1f %7d %6d %+7.1f %+7.1f %+7.1f %6s %9d %8d" % (
                name, pol, fac, earlier, later, wait_s(cw, .5) - wait_s(bw, .5),
                wait_s(cw, .9) - wait_s(bw, .9), wait_s(cw, .99) - wait_s(bw, .99),
                pct(sum(1 for x in cw if x > 60000) / len(cw)), stt["phys_refused"], stt["cpu_held"]))
            blind = simulate(jobs, ce, fac, pol, "blind", hid)
            mem_rows.append((name, pol, fac, memory_view(jobs, blind, mlo, hi)))
    P("")
    P("## 3. Memory in the replay: concurrent sum of memory.peak of the VISIBLE jobs, each holding its peak for")
    P("its whole run (blind physical reading), as a fraction of MemoryHigh (60 GiB). dur = duration-weighted (a")
    P("random instant); job = burst-weighted (the instant each job starts). busiest = the clock hour with the")
    P("highest duration-weighted mean. over54 = minutes whose maximum exceeds 0.9 x MemoryHigh (54 GiB), of %d." %
      mem_rows[0][3]["minutes"])
    ov = memory_view(jobs, obs_start, mlo, hi)
    P("%-26s %-8s %4s %-14s %8s %8s %8s %8s %9s %7s %6s" % (
        "ceiling", "policy", "fac", "busiest hour", "dur mean", "dur p99", "job p99", "hour max", "all job99",
        "minmax", "over54"))
    for name, pol, fac, m in [("OBSERVED", "-", 1.0, ov)] + mem_rows:
        P("%-26s %-8s %4.1f %-14s %8.3f %8.3f %8.3f %8.3f %9.3f %7.3f %6d" % (
            name, pol, fac, m["busiest_hour"], m["hour_dur_mean"] / MEMORY_HIGH, m["hour_dur_p99"] / MEMORY_HIGH,
            m["hour_job_p99"] / MEMORY_HIGH, m["hour_max"] / MEMORY_HIGH, m["all_job_p99"] / MEMORY_HIGH,
            m["minute_max"] / MEMORY_HIGH, m["minutes_over_safe"]))
    P("")

    # ---- part 4: the 5% trigger
    P("## 4. Discounted jobs against their charge (population: every pin row; the replay discounts them all)")
    for fac in (0.9, 0.8):
        above = sum(1 for j in pins if j.peak > charge_for(j.pin, fac))
        clean = [j for j in pins if not j.oom]
        above_clean = sum(1 for j in clean if j.peak > charge_for(j.pin, fac))
        atpin = sum(1 for j in pins if j.peak >= j.pin)
        P("factor %.1f: peak > charge %d of %d = %s; OOM rows excluded %d of %d = %s; "
          "peak >= pin %d = %s (OOM rows among them %d)" % (
              fac, above, len(pins), pct(above / len(pins)), above_clean, len(clean), pct(above_clean / len(clean)),
              atpin, pct(atpin / len(pins)), sum(1 for j in pins if j.peak >= j.pin and j.oom)))
    by_hour = collections.defaultdict(lambda: [0, 0, 0])
    for j in pins:
        h = by_hour[j.start // 3600000]
        h[0] += 1
        h[1] += j.peak > charge_for(j.pin, 0.9)
        h[2] += j.peak > charge_for(j.pin, 0.8)
    r9 = [v[1] / v[0] for v in by_hour.values() if v[0] >= 20]
    r8 = [v[2] / v[0] for v in by_hour.values() if v[0] >= 20]
    P("per clock hour with >= 20 pin rows (%d hours): above-charge share at 0.9 median %s max %s; at 0.8 median %s max %s" % (
        len(r9), pct(statistics.median(r9)), pct(max(r9)), pct(statistics.median(r8)), pct(max(r8))))
    ratio = sorted(j.peak / j.pin for j in pins if j.pin > 0)
    P("pin rows peak/pin: p50 %.2f p90 %.2f p95 %.2f p99 %.2f" % (q(ratio, .5), q(ratio, .9), q(ratio, .95), q(ratio, .99)))
    P("")

    # ---- part 5: first-order counterfactual on the REAL slice
    if ceil_series:
        first_order(jobs, ceil_series, mlo, hi, P, slice_peak)
    return best


def first_order(jobs, ceil_series, lo, hi, P, slice_peak):
    """Minute by minute, on the observed state: the booking the discount frees from the visible
    running pin jobs, filled from the observed queue in arrival order (pins at their charge),
    and the peaks of the jobs it admits added to the REAL slice anon of that minute (all jobs,
    hidden ones included). The bound column instead adds L x (1 - f) / f, the most pin bytes
    a full ledger L can hold beyond today's, each at its full pin."""
    P("## 5. First-order counterfactual on the real slice (journal sliceAnon, every minute)")
    P("est = sliceAnon + peaks of the queued jobs the freed booking admits; bound = sliceAnon + L(1-f)/f.")
    P("Neither is a proof: both keep the observed running set and add to it. The hard bound below is.")
    Lmax = max(c[1] for c in ceil_series if lo <= c[0] <= hi) - HEADROOM_BASE - HEADROOM_PER_JOB
    for fac in (1.0, 0.9, 0.8):
        P("hard bound, factor %.1f: every running job at its full cap at once, sum of caps <= L / f; at the window's "
          "largest ledger ceiling L = %s GiB that is %s GiB = %.3f of MemoryHigh (MemoryMax 64 GiB = 1.067); at the "
          "static ceiling (throttle lifted, L = 61.9 GiB) %s GiB" % (
              fac, fmt_g(Lmax), fmt_g(Lmax / fac), Lmax / fac / MEMORY_HIGH,
              fmt_g((64 * GIB - HEADROOM_BASE - HEADROOM_PER_JOB) / fac)))
    rows = []
    for fac in (0.9, 0.8):
        est, bnd, admitted_min, contended = [], [], 0, 0
        hours = collections.defaultdict(list)
        for t, eff, anon in ceil_series:
            if not (lo <= t <= hi):
                continue
            running = [j for j in jobs if j.start <= t < j.start + j.dur]
            waiting = sorted((j for j in jobs if j.arrival <= t < j.start and j.start - j.arrival >= 1000),
                             key=lambda j: (j.arrival, j.idx))
            L = eff - HEADROOM_BASE - HEADROOM_PER_JOB * (len(running) + 1)
            freed = sum(j.pin - charge_for(j.pin, fac) for j in running if j.pin_class)
            extra = n = 0
            for w in waiting:
                c = charge_for(w.pin, fac) if w.pin_class else w.pin
                if c > freed:
                    break                      # FIFO: the head that does not fit stops the pass
                freed -= c
                extra += w.peak
                n += 1
            if waiting:
                contended += 1
                admitted_min += n > 0
            e = anon + extra
            b = anon + L * (1 - fac) / fac
            est.append(e)
            bnd.append(b)
            hours[t // 3600000].append((anon, e, b))
        busiest = max(hours, key=lambda h: sum(x[0] for x in hours[h]) / len(hours[h]))
        hb = hours[busiest]
        P("factor %.1f: contended minutes %d, with >= 1 waiter admitted at once %d (%s); "
          "est max %.3f, bound max %.3f of MemoryHigh; minutes over 54 GiB: est %d, bound %d of %d; "
          "busiest hour %sZ (by sliceAnon): est mean %.3f max %.3f, bound max %.3f" % (
              fac, contended, admitted_min, pct(admitted_min / contended if contended else 0),
              max(est) / MEMORY_HIGH, max(bnd) / MEMORY_HIGH, sum(1 for x in est if x > SAFE),
              sum(1 for x in bnd if x > SAFE), len(est), iso(busiest * 3600000)[:13],
              sum(x[1] for x in hb) / len(hb) / MEMORY_HIGH, max(x[1] for x in hb) / MEMORY_HIGH,
              max(x[2] for x in hb) / MEMORY_HIGH))
        if slice_peak:
            P("  cache-inclusive bound: slice memory.peak %s + Lmax x (1-f)/f %s = %s GiB = %.3f of MemoryHigh" % (
                fmt_g(slice_peak), fmt_g(Lmax * (1 - fac) / fac), fmt_g(slice_peak + Lmax * (1 - fac) / fac),
                (slice_peak + Lmax * (1 - fac) / fac) / MEMORY_HIGH))
        rows.append((fac, max(est), max(bnd)))
    return rows


def hourly(jobs, w):
    acc = collections.defaultdict(list)
    for j, x in zip(jobs, w):
        acc[(j.arrival // 3600000)].append(x)
    return {k: sum(v) / len(v) for k, v in acc.items()}


def pearson(a, b):
    ks = sorted(set(a) & set(b))
    if len(ks) < 3:
        return float("nan")
    x = [a[k] for k in ks]
    y = [b[k] for k in ks]
    mx, my = sum(x) / len(x), sum(y) / len(y)
    sx = math.sqrt(sum((v - mx) ** 2 for v in x))
    sy = math.sqrt(sum((v - my) ** 2 for v in y))
    if sx == 0 or sy == 0:
        return float("nan")
    return sum((u - mx) * (v - my) for u, v in zip(x, y)) / (sx * sy)


# --------------------------------------------------------------------------- selftest

def selftest():
    G = GIB
    ok = True

    def check(name, got, want):
        nonlocal ok
        if got != want:
            ok = False
            print("FAIL %s: got %r want %r" % (name, got, want))
        else:
            print("ok   %s" % name)

    c10 = Ceiling(const=10 * G, headroom=False)
    # 1. two 6 GiB pins (peak 3 GiB, 100 s) arriving together on a 10 GiB ledger
    a = [Job(0, 0, 0, 100000, 3 * G, 6 * G, PIN_BASIS, 0), Job(1, 0, 0, 100000, 3 * G, 6 * G, PIN_BASIS, 0)]
    check("baseline: second waits a full run", simulate(a, c10, 1.0)[1], 100000)
    check("0.9: 2 x 5.4 GiB > 10 GiB still waits", simulate(a, c10, 0.9)[1], 100000)
    check("0.8: 2 x 4.8 GiB fits, physical 6 <= 10-3", simulate(a, c10, 0.8)[1], 0)
    # 2. physical fit blocks: peaks 5 GiB, so free = 10-5 = 5 < 6 even at 0.8 ...
    b = [Job(0, 0, 0, 100000, 5 * G, 6 * G, PIN_BASIS, 0), Job(1, 0, 0, 100000, 5 * G, 6 * G, PIN_BASIS, 0)]
    check("0.8 clairvoyant: physical fit refuses", simulate(b, c10, 0.8, phys="clairvoyant")[1], 100000)
    # ... but the blind reading cannot see the running job's peak
    check("0.8 blind: admitted", simulate(b, c10, 0.8, phys="blind")[1], 0)
    # 2b. blind also drops the physical term of the LEDGER check (checkedAvailable): two
    # 6 GiB pins whose peaks are 6 GiB both fit an idle 10 GiB box at 0.8 (2 x 4.8)
    bb = [Job(0, 0, 0, 100000, 6 * G, 6 * G, PIN_BASIS, 0), Job(1, 0, 0, 100000, 6 * G, 6 * G, PIN_BASIS, 0)]
    check("0.8 blind: ledger check sees no peaks either", simulate(bb, c10, 0.8, phys="blind")[1], 0)
    # 3. auto rows are never discounted
    c = [Job(0, 0, 0, 100000, 3 * G, 6 * G, "cap:auto:daemon-reserve", 0),
         Job(1, 0, 0, 100000, 3 * G, 6 * G, "cap:auto:daemon-reserve", 0)]
    check("0.8: auto rows booked in full", simulate(c, c10, 0.8)[1], 100000)
    # 4. order policies: head 8 GiB blocked behind a running 4 GiB; a later 2 GiB job
    d = [Job(0, 0, 0, 100000, G, 4 * G, "x", 0), Job(1, 10, 0, 50000, G, 8 * G, "x", 0),
         Job(2, 20, 0, 1000, G, 2 * G, "x", 0), Job(3, 30, 0, 1000, G, 2 * G, "x", 0),
         Job(4, 40, 0, 1000, G, 2 * G, "x", 0)]
    s = simulate(d, c10, 1.0, "fifo")
    check("fifo: nothing passes the head", (s[1], s[2]), (100000, 100000))
    s = simulate(d, c10, 1.0, "backfill")
    check("backfill: small jobs pass", (s[2], s[3], s[4]), (20, 30, 40))
    s = simulate(d, c10, 1.0, "bypass")
    # budget = 8/2 = 4 GiB: two 2 GiB jobs pass, the third waits for the head
    check("bypass: AIRA-307 budget lets 4 GiB pass", (s[2], s[3], s[4] >= 100000), (20, 30, True))
    # 5. headroom: 2 GiB + 64 MiB x (jobs+1) is taken from the ceiling
    h = Ceiling(const=10 * G, headroom=True)
    check("headroom with 0 jobs", h.at(0, 0), 10 * G - 2 * G - 64 * MIB)
    # 6. memory view: two 3 GiB peaks overlapping 50 s in one hour
    e = [Job(0, 0, 0, 100000, 3 * G, 6 * G, PIN_BASIS, 0), Job(1, 0, 50000, 100000, 3 * G, 6 * G, PIN_BASIS, 0)]
    st = {0: 0, 1: 50000}
    mv = memory_view(e, st, 0, 3600000)
    check("memory: max concurrent", mv["hour_max"], 6 * G)
    check("memory: duration-weighted mean over the hour",
          round(mv["hour_dur_mean"]), round((3 * G * 50000 + 6 * G * 50000 + 3 * G * 50000) / 3600000))
    check("memory: burst-weighted level at the second start", mv["hour_job_p99"], 6 * G)
    # 3450 s idle, 100 s at 3 GiB, 50 s at 6 GiB: the 99th percentile instant is in the 6 GiB stretch
    check("memory: duration-weighted p99 counts the idle time", mv["hour_dur_p99"], 6 * G)
    check("memory: minutes over 54 GiB", mv["minutes_over_safe"], 0)
    # a level that ends exactly on a minute boundary does not count in the next minute
    edge = [Job(0, 0, 0, 60000, 3 * G, 6 * G, PIN_BASIS, 0)]
    mm = minute_max(step(edge, {0: 0}, lambda j: j.peak), 0, 120000)
    check("minute max: boundary end", (mm[0], mm[60000]), (3 * G, 0))
    # a window that opens 50 min before the first job: 100 s at 3 GiB is 2.8% of the hour
    late = [Job(0, 3000000, 3000000, 100000, 3 * G, 6 * G, PIN_BASIS, 0)]
    check("memory: idle time before the first job is in the hour", memory_view(late, {0: 3000000}, 0, 3600000)["hour_dur_p99"], 3 * G)
    check("charge never below the floor nor above the pin", (charge_for(MIB // 2, 0.8), charge_for(MIB + 1, 0.5)), (MIB // 2, MIB))
    # 7. charge arithmetic
    check("charge ceil", charge_for(10, 0.8), 10)            # below the floor: the pin
    # the daemon's own observe lines: pin=1073741824 would-charge=858993460 (factor 0.80),
    # pin=134217728 would-charge=107374183, pin=2147483648 would-charge=1717986919
    check("charge 0.8 matches the daemon", (charge_for(G, 0.8), charge_for(128 * MIB, 0.8), charge_for(2 * G, 0.8)),
          (858993460, 107374183, 1717986919))
    check("charge 1.0 is the pin", charge_for(6 * G, 1.0), 6 * G)
    # 8. ceiling series step function with headroom off
    cs = Ceiling(series=[(0, 10 * G, 0), (1000, 20 * G, 0)], headroom=False)
    check("series step", (cs.at(999, 0), cs.at(1000, 0)), (10 * G, 20 * G))
    # 9. a ceiling rise mid-queue admits the waiter at the change, not at the next end
    f = [Job(0, 0, 0, 100000, G, 6 * G, "x", 0), Job(1, 0, 0, 100000, G, 6 * G, "x", 0)]
    check("ceiling rise admits at the change", simulate(f, cs, 1.0)[1], 1000)
    # 10. residual: frac 1 leaves the replayed ledger exactly the visible booking
    g = [Job(0, 0, 0, 100000, G, 4 * G, "x", 0)]
    rs = residual_series(g, [(50000, 20 * G, 0)], 1.0)
    check("residual 1.0 leaves the visible booking", Ceiling(series=rs).at(50000, 1), 4 * G)
    rs = residual_series(g, [(50000, 20 * G, 0)], 0.5)
    check("residual 0.5 removes half the gap", Ceiling(series=rs).at(50000, 1),
          4 * G + (20 * G - 2 * G - 2 * 64 * MIB - 4 * G) // 2)
    # 11. first order: a running 10 GiB pin frees 1 GiB at 0.9; a queued 1 GiB pin (charge
    # 0.9 GiB, peak 0.5 GiB) is admitted, a second one is not; anon 5 GiB on the minute
    # 0.9 GiB, peak 0.5 GiB) is admitted, a second one is not, and FIFO stops there, so a
    # tiny third one is not admitted either; a running 10 GiB AUTO job frees nothing
    fo = [Job(0, 0, 0, 200000, 2 * G, 10 * G, PIN_BASIS, 0), Job(1, 10000, 150000, 1000, G // 2, G, PIN_BASIS, 0),
          Job(2, 20000, 150000, 1000, G // 2, G, PIN_BASIS, 0), Job(3, 30000, 150000, 1000, 64 * MIB, 64 * MIB, "x", 0),
          Job(4, 0, 0, 200000, G, 10 * G, "cap:auto:daemon-reserve", 0)]
    rows = first_order(fo, [(60000, 30 * G, 5 * G)], 0, 200000, lambda *a: None, None)
    check("first order est at 0.9: anon + one admitted peak", rows[0][1], 5 * G + G // 2)
    check("first order bound at 0.9: anon + L x 0.1/0.9",
          rows[0][2], 5 * G + (30 * G - 2 * G - 3 * 64 * MIB) * (1 - 0.9) / 0.9)
    print("selftest:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db")
    ap.add_argument("--journal")
    ap.add_argument("--slice-peak", type=int, help="aira.slice memory.peak in bytes, read when the journal was saved")
    ap.add_argument("--selftest", action="store_true")
    a = ap.parse_args()
    if a.selftest:
        return selftest()
    if not a.db:
        ap.error("--db is required (a COPY of the store)")
    jobs, extra = load_jobs(a.db)
    if not jobs:
        print("no rows with AIRA-323 facts: unevaluated")
        return 3
    ceil, obs = load_journal(a.journal) if a.journal else ([], [])
    report(jobs, extra, ceil, obs, sys.stdout, a.slice_peak)
    return 0


if __name__ == "__main__":
    sys.exit(main())
