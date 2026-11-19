#!/usr/bin/env python3
"""AIRA-356 auto-resize replay: should the daemon lower a long-running job's booking toward its
observed peak, and how does that compare with AIRA-354 (bounded ledger overbooking)?

Spec: docs/superpowers/specs/2026-11-19-auto-resize-design.md (cited there as "replay sN").

POLICIES (fixed before the first run)
  For a granted, accounted, job-level confine lease (no aitest worker, no sub-reservation) whose
  age is >= t0 minutes, the booking becomes
      B'(t) = min(B, max(FLOOR, ceil(m x P(t))))        FLOOR = 256 MiB
  where B is the booking the job was granted (the AIRA-324 0.9 discount included) and P(t) is the
  scope's memory.peak so far. t0 in {5, 15} min, m in {1.25, 1.5, 2.0}. Two variants:
    one-shot  P is read once, at the first tick with age >= t0 (what a scripted `--resize` does).
    ratchet   P is re-read every tick; B' follows it up (never above B), lagging one tick.
  REGRET: a later peak above the booking in force (one-shot: above B'(t0); ratchet: above the
  booking set from the previous tick). memory.max is unchanged by every policy, so regret bytes
  are ledger under-count, not an OOM: the job keeps running; the host exposure is that the ledger
  admitted other work against bytes this job then used.

INPUTS (read-only copies; never point --db at the live file)
  sqlite3 'file:$HOME/.local/state/aira/state.db?mode=ro' ".backup $HOME/tmp/aira-auto-resize-data/state-copy.db"
  journalctl --user -u aira-daemon.service --since 2026-11-12 -o short-unix --no-pager > .../journal.txt
  --live: lines "<epoch ms>\\t<aira confine --list JSON>" (sampler: spec section 11)
  aira confine -- python3 -I docs/dev/aira-356-auto-resize-replay.py --db ... --journal ... --live a.tsv b.tsv

LIMITS (printed again at the end)
  - The store (confine_peak_history) keeps ONE peak per finished job (memory.peak at teardown) and
    no timeline, at most 20 rows per exact signature, only OOM or clean runs; jobs still running at
    the copy (the days-long services) are absent. Store sections therefore use the FINAL peak as
    P(t0): exact for the ratchet's steady state, a LOWER bound on what the ratchet frees before the
    peak is reached, and silent on regret. Regret comes only from the live trajectories.
  - memory.peak counts page cache (AIRA-318): a file-heavy job reads at or near its cap without
    needing it, so the policy frees nothing there. That makes every yield here conservative.
  - Live samples are the only per-job trajectories: a few hours, one sample every 15-20 s.
  - Queue-wait savings are a static per-instant estimate (the queued jobs admitted FIFO into the
    room the policy frees), ignoring cascades, the head-of-queue hold and fairness rules: an
    ESTIMATE, stated as one.
"""
import argparse
import bisect
import json
import math
import re
import sqlite3
import statistics
import sys

GIB = 1 << 30
MIB = 1 << 20
FLOOR = 256 * MIB
H0 = 2 * GIB + 64 * MIB          # admitSliceHeadroom: the smallest headroom (354 replay's H0)
CPU_SLOTS = 32                   # journal: "CPU admission ceiling 32 slots"
SMALL_Y = 1 * GIB                # AIRA-350 small-lease bound y (booking <= 1 GiB)
LANE_X = 8 * GIB                 # AIRA-350 lane capacity x
PIN_FACTOR = 0.9                 # AIRA-324 enforce factor on the box
PIN_BASIS = "cap:operator:--memory-reserve"
T0S = [5, 15]
MS = [1.25, 1.5, 2.0]
# live policies: the grid, plus a bounded-shrink variant (never below half the booking), added after
# the first run showed the regret jobs were idle at t0 (multi-phase jobs); reported as a variant.
LIVE_POLS = [(t0, m, 0.0) for t0 in T0S for m in MS] + [(15, 1.5, 0.5), (5, 1.25, 0.5)]
KS = [1.10, 1.25]                # AIRA-354 comparison points
WINDOW_S = 7 * 86400
STEP = 30                        # store grid, seconds

CEIL_RE = re.compile(r"^(\d+(?:\.\d+)?) .*slice ceiling (?:un)?throttled \(\w+\): (\d+) effective / (\d+) configured, "
                     r"sliceAnon=(\d+) MemAvailable=(\d+)")
ENF_RE = re.compile(r"^(\d+(?:\.\d+)?) .*pin-charge enforce: ")
RESIZE_RE = re.compile(r"^(\d+(?:\.\d+)?) .*confine-resize: .*target-scope=(\S+) .*old=(\d+) new=(\d+) usage=(\d+)")


def q(xs, p):
    if not xs:
        return float("nan")
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(p * (len(xs) - 1) + 0.5))]


def g(b):
    return "%.2f" % (b / GIB)


def booked(m, peak, b, f=0.0):
    """The policy's booking: m x peak, at least FLOOR and at least f x B, never above B."""
    return min(b, max(FLOOR, int(math.ceil(m * peak)), int(math.ceil(f * b))))


def lab(t0, m, f):
    return "t0=%d m=%.2f" % (t0, m) + (" min %.1fB" % f if f else "")


# ---------------------------------------------------------------- store


class Job:
    __slots__ = ("a", "s", "e", "b", "cap", "p", "oom", "sig", "basis")


def load_store(db):
    con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = con.execute(
        "select started_at_ms, wait_ms, wall_ms, budget, peak_rss, oom, signature, budget_basis "
        "from confine_peak_history where kind='confine' and started_at_ms is not null and wall_ms is not null "
        "and wait_ms is not null and budget is not null and budget > 0").fetchall()
    con.close()
    return rows


def store_jobs(rows, enforce_since):
    jobs = []
    for start, wait, wall, budget, peak, oom, sig, basis in rows:
        j = Job()
        j.s = start / 1000.0
        j.a = j.s - wait / 1000.0
        j.e = j.s + wall / 1000.0
        j.cap = budget
        j.basis = basis or ""
        # AIRA-324 enforce books an operator pin at ceil(0.9 x pin) from the first enforce line on.
        j.b = budget
        if enforce_since is not None and j.basis == PIN_BASIS and j.a >= enforce_since:
            j.b = int(math.ceil(PIN_FACTOR * budget))
        # An OOM row's peak is censored at the cap; an unknown peak is bounded at the cap, never 0.
        j.p = budget if (oom or peak is None) else min(peak, budget)
        j.oom, j.sig = oom, sig
        jobs.append(j)
    return jobs


def store_section(jobs, t_end, out):
    t_start = t_end - WINDOW_S
    win = [j for j in jobs if j.e > t_start and j.a < t_end]
    p = lambda *a: print(*a, file=out)
    p("## 1. Store: finished jobs, last 7 days (%.0f .. %.0f unix s)" % (t_start, t_end))

    def ov(j, lo, hi):
        return max(0.0, min(hi, t_end) - max(lo, t_start))

    allgh = sum(j.b * ov(j, j.s, j.e) for j in win) / GIB / 3600
    p("visible jobs %d; booked GiB-hours (all jobs, running time in window) %.1f; mean visible booking %.2f GiB"
      % (len(win), allgh, allgh / (WINDOW_S / 3600)))
    for thr in T0S:
        lj = [j for j in win if (j.e - j.s) >= thr * 60]
        gh = sum(j.b * ov(j, j.s, j.e) for j in lj) / GIB / 3600
        ratios = [j.p / j.b for j in lj]
        p("")
        p("### jobs that ran >= %d min: %d (%d OOM), %.1f%% of visible booked GiB-hours" % (
            thr, len(lj), sum(1 for j in lj if j.oom), 100 * gh / allgh if allgh else 0))
        p("booking GiB p50 %s p90 %s max %s; wall min p50 %.1f p90 %.1f max %.1f" % (
            g(q([j.b for j in lj], .5)), g(q([j.b for j in lj], .9)), g(max(j.b for j in lj)),
            q([(j.e - j.s) / 60 for j in lj], .5), q([(j.e - j.s) / 60 for j in lj], .9), max((j.e - j.s) / 60 for j in lj)))
        p("final peak / booking: p10 %.2f p50 %.2f p90 %.2f; share with peak >= booking/1.25 %.0f%%, /1.5 %.0f%%, /2 %.0f%%" % (
            q(ratios, .1), q(ratios, .5), q(ratios, .9),
            100 * sum(r >= 1 / 1.25 for r in ratios) / len(ratios),
            100 * sum(r >= 1 / 1.5 for r in ratios) / len(ratios),
            100 * sum(r >= 1 / 2.0 for r in ratios) / len(ratios)))
        bys = sorted((j.b * ov(j, j.s, j.e) for j in lj), reverse=True)
        tot = sum(bys)
        top = lambda n: 100 * sum(bys[:n]) / tot if tot else 0
        p("concentration of these jobs' booked GiB-hours: top 1 job %.1f%%, top 5 %.1f%%, top 10 %.1f%%, top 20 %.1f%%, top 10%% of jobs (%d) %.1f%%" % (
            top(1), top(5), top(10), top(20), max(1, len(bys) // 10), top(max(1, len(bys) // 10))))
        p("policy (t0=%d min)   freed GiB-h  mean GiB  share of long GiB-h  top-10 jobs' share of freed  jobs freed>0" % thr)
        for m in MS:
            fr = []
            for j in lj:
                nb = booked(m, j.p, j.b)
                fr.append((j.b - nb) * ov(j, j.s + thr * 60, j.e))
            fs = sorted(fr, reverse=True)
            ft = sum(fs)
            p("  m=%.2f              %8.1f   %6.2f     %5.1f%%              %5.1f%%                      %d" % (
                m, ft / GIB / 3600, ft / GIB / WINDOW_S, 100 * ft / tot if tot else 0,
                100 * sum(fs[:10]) / ft if ft else 0, sum(1 for f in fr if f > 0)))
    return win


def store_timeline(win, t_end, ceil_series, out):
    """Grid over the window: freed(t) per policy, (k-1)L for AIRA-354, and the queue-wait estimate."""
    p = lambda *a: print(*a, file=out)
    t_start = t_end - WINDOW_S
    pols = [(t0, m) for t0 in T0S for m in MS]
    freed = {pol: [] for pol in pols}
    xk = {k: [] for k in KS}
    removed = {pol: 0.0 for pol in pols}
    removed_k = {k: 0.0 for k in KS}
    waited = 0.0
    hasq = []
    ev = sorted(win, key=lambda j: j.a)
    nb = {pol: {} for pol in pols}
    for j in win:
        for pol in pols:
            nb[pol][id(j)] = booked(pol[1], j.p, j.b)
    active, i = [], 0
    t = t_start
    while t < t_end:
        while i < len(ev) and ev[i].a <= t:
            active.append(ev[i])
            i += 1
        active = [j for j in active if j.e > t]
        running = [j for j in active if j.s <= t]
        queued = [j for j in active if j.s > t]          # already sorted by arrival
        waited += STEP * len(queued)
        hasq.append(bool(queued))
        for pol in pols:
            t0 = pol[0] * 60
            f = sum(j.b - nb[pol][id(j)] for j in running if t - j.s >= t0)
            freed[pol].append(f)
            if queued and f > 0:
                room, n = f, 0
                for j in queued:
                    if j.b > room:
                        break
                    room -= j.b
                    n += 1
                removed[pol] += STEP * n
        cv = ceil_series(t) if ceil_series else None
        for k in KS:
            x = (k - 1) * max(0, cv[0] - H0) if cv else 0
            xk[k].append(x)
            if queued and x > 0:
                room, n = x, 0
                for j in queued:
                    if j.b > room:
                        break
                    room -= j.b
                    n += 1
                removed_k[k] += STEP * n
        t += STEP
    p("")
    p("## 2. Store timeline (%d s grid, 7 days): ledger room each rule frees, visible jobs only" % STEP)
    p("visible queue present at %.0f%% of instants" % (100 * sum(hasq) / len(hasq)))
    p("rule                         room GiB p50   p90    max    mean   mean-when-queued   queue wait-hours removed (static FIFO, RAM only, ESTIMATE)")
    mq = lambda xs: sum(x for x, h in zip(xs, hasq) if h) / max(1, sum(hasq))
    for pol in pols:
        fs = freed[pol]
        p("auto-resize t0=%2d m=%.2f       %6s %6s %6s %6s   %6s             %6.1f of %.1f (%.0f%%)" % (
            pol[0], pol[1], g(q(fs, .5)), g(q(fs, .9)), g(max(fs)), g(sum(fs) / len(fs)), g(mq(fs)),
            removed[pol] / 3600, waited / 3600, 100 * removed[pol] / waited if waited else 0))
    for k in KS:
        xs = xk[k]
        p("AIRA-354 k=%.2f ('k only')     %6s %6s %6s %6s   %6s             %6.1f of %.1f (%.0f%%)" % (
            k, g(q(xs, .5)), g(q(xs, .9)), g(max(xs)), g(sum(xs) / len(xs)), g(mq(xs)),
            removed_k[k] / 3600, waited / 3600, 100 * removed_k[k] / waited if waited else 0))
    p("(354's room X = (k-1)(E - H0) from the journal's effective ceiling E; its spec adds two min() bounds that only")
    p(" lower X. Both columns assume the queue was RAM-bound at that instant; the live section tests that.)")


# ---------------------------------------------------------------- live


def load_live(paths):
    snaps = []
    for path in paths:
        with open(path) as fh:
            for line in fh:
                ts, _, js = line.partition("\t")
                try:
                    d = json.loads(js)["data"]
                except (ValueError, KeyError):
                    continue
                snaps.append((int(ts) / 1000.0, d))
    snaps.sort(key=lambda x: x[0])
    # de-duplicate identical timestamps (the 354 store kept a frozen copy of the same file)
    out, last = [], None
    for t, d in snaps:
        if t != last:
            out.append((t, d))
        last = t
    return out


def apply_grant_age(snaps, spans):
    # AGE SINCE GRANT. The list's age_seconds counts from the scope id's launch stamp, so it includes
    # the queue wait; the policy keys on grantedAt. A job first seen running after a span's first
    # sample was granted within one gap before that sighting: its grant time is the sighting. A job
    # already running at a span's first sample keeps its launch age (an upper bound on its age since
    # grant: it can only make the rule act early, the pessimistic direction for regret).
    first_t = {a for a, _ in spans}
    grant_t, launch_aged = {}, set()
    for t, d in snaps:
        for sc in d.get("scopes", []):
            sid = sc.get("scope_id")
            if sid in grant_t or sc.get("age_seconds") is None:
                continue
            if t in first_t:
                grant_t[sid] = t - sc["age_seconds"]
                launch_aged.add(sid)
            else:
                grant_t[sid] = t
    for t, d in snaps:
        for sc in d.get("scopes", []):
            if sc.get("scope_id") in grant_t:
                sc["launch_age_seconds"] = sc["age_seconds"]
                sc["age_seconds"] = max(0, int(t - grant_t[sc["scope_id"]]))
    return len(grant_t) - len(launch_aged), len(launch_aged)


def eligible(sc):
    return (not sc.get("worker")) and sc.get("reserve_bytes") and sc.get("peak_rss") is not None \
        and sc.get("age_seconds") is not None


def live_section(snaps, out):
    p = lambda *a: print(*a, file=out)
    p("")
    p("## 3. Live `aira confine --list` samples: every running job, per-job memory.peak trajectories")
    if not snaps:
        p("no live samples")
        return
    gaps = [b[0] - a[0] for a, b in zip(snaps, snaps[1:])]
    spans, cur = [], [snaps[0][0], snaps[0][0]]
    for (t1, _), (t2, _) in zip(snaps, snaps[1:]):
        if t2 - t1 > 300:
            spans.append(cur)
            cur = [t2, t2]
        cur[1] = t2
    spans.append(cur)
    p("samples %d in %d spans (%s); gap p50 %.0f s" % (
        len(snaps), len(spans), ", ".join("%.0f min" % ((b - a) / 60) for a, b in spans), q(gaps, .5)))

    n_seen, n_launch = apply_grant_age(snaps, spans)
    p("age = time since grant: %d scopes timed from their first running sighting, %d already running at a span's"
      " start keep their launch age (includes queue wait; an upper bound)" % (n_seen, n_launch))

    # per-scope series: (t, age, peak, current, booking, cap)
    series = {}
    for t, d in snaps:
        for sc in d.get("scopes", []):
            if not eligible(sc):
                continue
            cap = int(sc["cap"]) if sc.get("cap") not in (None, "", "max") else sc["reserve_bytes"]
            series.setdefault(sc["scope_id"], []).append(
                (t, sc["age_seconds"], sc["peak_rss"], sc.get("rss_bytes") or 0, sc["reserve_bytes"], cap,
                 sc.get("name", ""), sc.get("command", "")[:60]))

    # 3a. booking concentration and long-job share per sample
    p("")
    p("### 3a. what the long jobs hold, per sample")
    for thr in T0S:
        sh, top3, nlong, big = [], [], [], []
        for t, d in snaps:
            scs = [sc for sc in d.get("scopes", []) if sc.get("reserve_bytes")]
            tot = sum(sc["reserve_bytes"] for sc in scs)
            lj = sorted((sc["reserve_bytes"] for sc in scs if eligible(sc) and sc["age_seconds"] >= thr * 60), reverse=True)
            if tot:
                sh.append(sum(lj) / tot)
                top3.append(sum(lj[:3]) / tot)
            nlong.append(len(lj))
        p("age >= %2d min: jobs p50 %d max %d; their share of all booked bytes p50 %.2f p90 %.2f; top-3 long jobs' share p50 %.2f p90 %.2f" % (
            thr, q(nlong, .5), max(nlong), q(sh, .5), q(sh, .9), q(top3, .5), q(top3, .9)))
    # days-long services (absent from the store)
    svc = {}
    for sid, ser in series.items():
        if ser[-1][1] >= 86400:
            svc[sid] = ser[-1]
    p("jobs older than 1 day (services; absent from the store): %d, booking %s GiB, peak %s GiB" % (
        len(svc), g(sum(v[4] for v in svc.values())), g(sum(v[2] for v in svc.values()))))

    # 3b. trajectories: how much of the final peak is reached by t0
    p("")
    p("### 3b. trajectories: peak at t0 vs the largest peak seen later (jobs seen both at <= t0+2 min and later)")
    for thr in T0S:
        rs = []
        for sid, ser in series.items():
            first = [r for r in ser if r[1] >= thr * 60]
            if not first or first[0][1] > thr * 60 + 120:
                continue
            p0 = first[0][2]
            pmax = max(r[2] for r in first)
            if first[-1][1] - first[0][1] < 300:
                continue
            rs.append(pmax / p0 if p0 else float("inf"))
        if rs:
            p("t0=%2d min: %d jobs observed from t0 for >= 5 min; later peak / peak at t0: p50 %.2f p90 %.2f p99 %.2f max %.2f; grew > 1.25x %d, > 1.5x %d, > 2x %d" % (
                thr, len(rs), q(rs, .5), q(rs, .9), q(rs, .99), max(rs),
                sum(r > 1.25 for r in rs), sum(r > 1.5 for r in rs), sum(r > 2.0 for r in rs)))
        else:
            p("t0=%2d min: no job observed from t0" % thr)

    # 3c. per-policy: room freed, regret, lane interaction, cache-bound share
    p("")
    p("### 3c. policies on the live samples")
    p("one-shot: shrink once at the first sample with age >= t0; ratchet: booking = min(B, max(FLOOR, m x peak at the previous sample))")
    p("regret jobs = jobs whose peak later exceeded the booking in force; bytes = the largest excess per job")
    hdr = ("policy            room GiB p50  p90   max   | one-shot regret: jobs/of  excess GiB p50 max  sum-at-worst | "
           "ratchet regret: jobs  events  excess GiB max | <=1GiB from >1GiB pins: n p50 GiB p50 | no-shrink (peak>=B/m) | one-shot regret of shrunk")
    p(hdr)
    tindex = {t: i for i, (t, _) in enumerate(snaps)}
    drill = []
    for t0, m, fr in LIVE_POLS:
        room_os = [0] * len(snaps)
        room_rt = [0] * len(snaps)
        small_n = [0] * len(snaps)
        small_b = [0] * len(snaps)
        os_reg, os_n, os_exc = 0, 0, []
        rt_reg, rt_ev, rt_exc = 0, 0, []
        noshrink, considered = 0, 0
        for sid, ser in series.items():
            idx = [k for k, r in enumerate(ser) if r[1] >= t0 * 60]
            if not idx:
                continue
            k0 = idx[0]
            B = ser[k0][4]
            considered += 1
            b1 = booked(m, ser[k0][2], B, fr)
            if b1 >= B:
                noshrink += 1
            os_n += 1
            # Regret is measured below the booking the job had anyway (B): a peak above B is the
            # AIRA-324 discount's own overrun (cap = pin > 0.9 x pin), not this policy's.
            later = max(min(r[2], r[4]) for r in ser[k0:])
            if later > b1:
                os_reg += 1
                os_exc.append(later - b1)
                if (t0, m, fr) == (15, 1.5, 0.0):
                    drill.append((later - b1, ser[k0][6], ser[k0][7], B, ser[k0][2], later, ser[k0][1], ser[-1][1]))
            jr, prev = False, None
            worst = 0
            for k in idx:
                t, age, pk, curr, Bk, cap = ser[k][:6]
                ti = tindex[t]
                room_os[ti] += max(0, Bk - min(Bk, b1))
                bk = booked(m, prev if prev is not None else pk, Bk, fr)
                room_rt[ti] += max(0, Bk - bk)
                if bk <= SMALL_Y < cap:
                    small_n[ti] += 1
                    small_b[ti] += bk
                if prev is not None and min(pk, Bk) > bk:
                    rt_ev += 1
                    jr = True
                    worst = max(worst, min(pk, Bk) - bk)
                prev = pk
            if jr:
                rt_reg += 1
                rt_exc.append(worst)
        p("%-17s %5s %5s %5s | %4d/%-4d %5.1f%%   %5s %5s   %6s       | %4d (%4.1f%%) %5d  %6s     | %3d   %5s           | %d of %d | %d/%d %.1f%%" % (
            lab(t0, m, fr), g(q(room_rt, .5)), g(q(room_rt, .9)), g(max(room_rt)),
            os_reg, os_n, 100 * os_reg / os_n if os_n else 0,
            g(q(os_exc, .5)) if os_exc else "-", g(max(os_exc)) if os_exc else "-", g(sum(os_exc)),
            rt_reg, 100 * rt_reg / os_n if os_n else 0, rt_ev, g(max(rt_exc)) if rt_exc else "-",
            q(small_n, .5), g(q(small_b, .5)), noshrink, considered,
            os_reg, considered - noshrink, 100 * os_reg / max(1, considered - noshrink)))
        p("    one-shot room p50 %s p90 %s GiB (the left column is the ratchet's)" % (g(q(room_os, .5)), g(q(room_os, .9))))
    p("")
    p("one-shot regret jobs at t0=15 m=1.50 (excess GiB, name, command, booking B, peak at t0, later peak, age at t0 and last seen, min):")
    for exc, name, cmd, B, p0, later, a0, a1 in sorted(drill, reverse=True):
        p("  %s  %-22s %-40s B %s  p(t0) %s  later %s  age %.0f..%.0f" % (
            g(exc), name[:22], cmd[:40], g(B), g(p0), g(later), a0 / 60, a1 / 60))

    # 3d. head to head with AIRA-354 on the same samples: queued jobs admitted now
    p("")
    p("### 3d. head to head on the same samples: queued jobs that would be admitted at that instant")
    p("room = ceiling - headroom - granted + extra (extra = ratchet room, or 354's (k-1)L); FIFO over the real queue;")
    p("CPU: free slots = 32 - running jobs (1 slot each by default). ESTIMATE, static per sample.")
    # ratchet room: from the peak now; one-shot room: from the peak at the first sample with age >= t0
    rt_room, os_room = {}, {}
    for t0, m, fr in LIVE_POLS:
        rr, orr = [], []
        fixed, prevpk = {}, {}
        for t, d in snaps:
            tot = otot = 0
            for sc in d.get("scopes", []):
                if not eligible(sc) or sc["age_seconds"] < t0 * 60:
                    continue
                b = sc["reserve_bytes"]
                # ratchet: booked from the PREVIOUS sample's peak, as its regret accounting is
                tot += b - booked(m, prevpk.get(sc["scope_id"], sc["peak_rss"]), b, fr)
                if sc["scope_id"] not in fixed:
                    fixed[sc["scope_id"]] = booked(m, sc["peak_rss"], b, fr)
                otot += max(0, b - fixed[sc["scope_id"]])
            for sc in d.get("scopes", []):
                if eligible(sc):
                    prevpk[sc["scope_id"]] = sc["peak_rss"]
            rr.append(tot)
            orr.append(otot)
        rt_room[(t0, m, fr)] = rr
        os_room[(t0, m, fr)] = orr
    bound_cpu = 0
    with_queue = 0
    for t, d in snaps:
        sr = d.get("slice_reserve", {})
        if sr.get("queued"):
            with_queue += 1
            if sr.get("jobs", 0) >= CPU_SLOTS:
                bound_cpu += 1
    p("samples with a queue %d of %d; of them CPU slots full (jobs >= 32) %d" % (with_queue, len(snaps), bound_cpu))
    p("rule (jobs admitted per sample)    RAM-only p50 max mean | RAM+CPU p50 max mean | queued GiB admitted p50 (RAM+CPU)")

    def admit(extra_of):
        a_r, a_c, gb = [], [], []
        for i, (t, d) in enumerate(snaps):
            sr = d.get("slice_reserve", {})
            qd = d.get("queued", []) or []
            if not qd:
                continue
            room0 = sr.get("ceiling_bytes", 0) - H0 - sr.get("granted_bytes", 0)
            room = room0 + extra_of(i, sr)
            slots = CPU_SLOTS - sr.get("jobs", 0)
            nr = nc = 0
            gsum = 0
            r1 = room
            for w in qd:
                b = w.get("reserve_bytes") or 0
                if b > r1:
                    break
                r1 -= b
                nr += 1
            r2 = room
            for w in qd:
                b = w.get("reserve_bytes") or 0
                if b > r2 or nc >= slots:
                    break
                r2 -= b
                nc += 1
                gsum += b
            base_r = base_c = 0
            a_r.append(nr)
            a_c.append(nc)
            gb.append(gsum)
        return a_r, a_c, gb

    def row(label, a):
        if not a[0]:
            p("%-34s no sample with a queue" % label)
            return
        p("%-34s %3d %3d  mean %4.1f | %3d %3d  mean %4.1f | %s" % (
            label, q(a[0], .5), max(a[0]), statistics.mean(a[0]), q(a[1], .5), max(a[1]), statistics.mean(a[1]), g(q(a[2], .5))))

    row("today (no change)", admit(lambda i, sr: 0))
    for key, rr in rt_room.items():
        row("ratchet " + lab(*key), admit(lambda i, sr, rr=rr: rr[i]))
    for key, rr in os_room.items():
        row("one-shot " + lab(*key), admit(lambda i, sr, rr=rr: rr[i]))
    for k in KS:
        row("AIRA-354 k=%.2f" % k, admit(lambda i, sr, k=k: (k - 1) * max(0, sr.get("ceiling_bytes", 0) - H0)))
    for k in KS:
        rr = os_room[(15, 1.5, 0.0)]
        row("both: 354 k=%.2f + one-shot 15/1.5" % k,
            admit(lambda i, sr, k=k, rr=rr: rr[i] + (k - 1) * max(0, sr.get("ceiling_bytes", 0) - H0)))

    # 3f. where the booked-but-unused bytes sit: what a peak-based per-job rule can reach at all
    p("")
    p("### 3f. where the booked bytes go, per sample (GiB, p50 over samples)")
    p("granted = workers + jobs younger than t0 + long jobs; long = kept (min(B, m x peak)) + room;")
    p("peak = memory.peak (page cache included, held); current = memory.current (cache included); real = slice current - reclaimable")
    for t0, m in [(5, 1.25), (15, 1.5)]:
        cols = {k: [] for k in ("granted", "workers", "young", "long", "kept", "room", "long_peak", "long_cur", "all_cur", "real")}
        for t, d in snaps:
            sr = d.get("slice_reserve", {})
            w = y = lb = kept = lp = lc = ac = 0
            for sc in d.get("scopes", []):
                b = sc.get("reserve_bytes") or 0
                ac += sc.get("rss_bytes") or 0
                if sc.get("worker"):
                    w += b
                elif not eligible(sc) or sc["age_seconds"] < t0 * 60:
                    y += b
                else:
                    lb += b
                    k = booked(m, sc["peak_rss"], b)
                    kept += k
                    lp += min(sc["peak_rss"], b)
                    lc += sc.get("rss_bytes") or 0
            cols["granted"].append(sr.get("granted_bytes", 0))
            cols["workers"].append(w)
            cols["young"].append(y)
            cols["long"].append(lb)
            cols["kept"].append(kept)
            cols["room"].append(lb - kept)
            cols["long_peak"].append(lp)
            cols["long_cur"].append(lc)
            cols["all_cur"].append(ac)
            cols["real"].append(sr.get("slice_current_bytes", 0) - sr.get("slice_reclaimable_bytes", 0))
        p("t0=%2d m=%.2f: " % (t0, m) + "  ".join("%s %s" % (k, g(q(v, .5))) for k, v in cols.items()))

    # 3e. host side: real use against bookings, and exposure
    p("")
    p("### 3e. host exposure on the same samples")
    ma = [d.get("slice_reserve", {}).get("system_mem_available_bytes", 0) for _, d in snaps]
    real = [(d["slice_reserve"].get("slice_current_bytes", 0) - d["slice_reserve"].get("slice_reclaimable_bytes", 0)) for _, d in snaps if d.get("slice_reserve")]
    gr = [d["slice_reserve"].get("granted_bytes", 0) for _, d in snaps if d.get("slice_reserve")]
    p("host MemAvailable GiB min %s p50 %s; slice real use (current - reclaimable) GiB p50 %s max %s; granted GiB p50 %s" % (
        g(min(ma)), g(q(ma, .5)), g(q(real, .5)), g(max(real)), g(q(gr, .5))))
    caps = []
    for _, d in snaps:
        c = 0
        for sc in d.get("scopes", []):
            if sc.get("cap") not in (None, "", "max"):
                c += int(sc["cap"])
            elif sc.get("reserve_bytes"):
                c += sc["reserve_bytes"]
        caps.append(c)
    smax = [d.get("slice_reserve", {}).get("slice_max_bytes", 0) for _, d in snaps]
    p("sum of running caps GiB p50 %s max %s; slice memory.max GiB %s; caps above memory.max - H0 today: %d of %d samples" % (
        g(q(caps, .5)), g(max(caps)), g(q(smax, .5)), sum(c > s - H0 for c, s in zip(caps, smax)), len(snaps)))
    p("if each rule's room were filled at full booking, caps would exceed memory.max - H0 in (samples):")
    for key in LIVE_POLS:
        for kind, rr in (("ratchet", rt_room[key]), ("one-shot", os_room[key])):
            p("  auto-resize %-8s %s: %d (caps + room p50 %s GiB)" % (
                kind, lab(*key), sum(c + r > s - H0 for c, r, s in zip(caps, rr, smax)), g(q([c + r for c, r in zip(caps, rr)], .5))))
    for k in KS:
        xr = [(k - 1) * max(0, d.get("slice_reserve", {}).get("ceiling_bytes", 0) - H0) for _, d in snaps]
        p("  AIRA-354 k=%.2f ('k only', before its memory.max bound): %d" % (k, sum(c + r > s - H0 for c, r, s in zip(caps, xr, smax))))
    p("worst case either rule adds (every freed / overbooked byte used at once) = the room column itself: the ledger")
    p("under-counts what jobs may legally use by exactly that much (memory.max unchanged in both). The difference is")
    p("whose bytes: auto-resize lends bytes a job has not touched for t0 minutes; 354 lends bytes of no particular job.")


def coverage_section(win, snaps, t_end, out):
    """How much of the live booked load the store's finished rows show, at the same instants."""
    p = lambda *a: print(*a, file=out)
    p("")
    p("## 5. Store coverage: store-visible running bookings / live granted, at live samples inside the store window")
    rs = []
    for t, d in snaps:
        if t > t_end - 600:
            continue
        gr = d.get("slice_reserve", {}).get("granted_bytes", 0)
        if not gr:
            continue
        vis = sum(j.b for j in win if j.s <= t < j.e)
        rs.append(vis / gr)
    if rs:
        p("samples %d: ratio p10 %.2f p50 %.2f p90 %.2f (the store sections see this share of the real load;" % (
            len(rs), q(rs, .1), q(rs, .5), q(rs, .9)))
        p(" aitest workers are kind=pytest-worker and excluded by design, as the policy excludes them)")
    else:
        p("no live sample inside the store window")


def journal_section(lines, out):
    p = lambda *a: print(*a, file=out)
    rs = []
    for line in lines:
        m = RESIZE_RE.match(line)
        if m:
            rs.append((float(m.group(1)), m.group(2), int(m.group(3)), int(m.group(4)), int(m.group(5))))
    p("")
    p("## 4. Hand resizes in the journal (AIRA-312, `aira confine --resize`)")
    if not rs:
        p("none")
        return
    p("resizes %d over %.1f days; GiB handed back per resize p50 %s max %s, total %s; new/old p50 %.2f; usage/new p50 %.2f" % (
        len(rs), (rs[-1][0] - rs[0][0]) / 86400, g(q([o - n for _, _, o, n, _ in rs], .5)),
        g(max(o - n for _, _, o, n, _ in rs)), g(sum(o - n for _, _, o, n, _ in rs)),
        q([n / o for _, _, o, n, _ in rs], .5), q([u / n for _, _, o, n, u in rs if n], .5)))
    p("distinct scopes %d" % len({s for _, s, _, _, _ in rs}))


def selftest():
    """The policy arithmetic and the regret accounting on synthetic inputs (no data needed)."""
    import io
    b = 4 * GIB
    assert booked(1.5, 0, b) == FLOOR                       # an idle job books the floor, never 0
    assert booked(1.5, 1 * GIB, b) == int(1.5 * GIB)        # m x peak
    assert booked(1.5, 3 * GIB, b) == b                     # never above the booking it had
    assert booked(1.5, 0, b, 0.5) == 2 * GIB                # the bounded-shrink variant
    # One job, booked 4 GiB under a 4.44 GiB cap (a 0.9 discount): idle at t0 (peak 0.1 GiB), later
    # 2 GiB, finally 4.3 GiB, above its booking (the discount's own overrun, not this policy's).
    snaps = []
    for i, pk in enumerate([0.1, 0.1, 2.0, 4.3]):
        snaps.append((1000.0 + 20 * i, {
            "slice_reserve": {"granted_bytes": b, "ceiling_bytes": 2 * b, "jobs": 1, "queued": 1},
            "queued": [{"reserve_bytes": GIB}],
            "scopes": [{"scope_id": "s", "name": "n", "command": "c", "age_seconds": 900 + 20 * i,
                        "peak_rss": int(pk * GIB), "rss_bytes": int(pk * GIB), "reserve_bytes": b,
                        "cap": str(int(4.44 * GIB))}]}))
    buf = io.StringIO()
    live_section(snaps, buf)
    line = [ln for ln in buf.getvalue().splitlines() if ln.startswith("t0=15 m=1.50 ")][0]
    # The one-shot books the 256 MiB floor at t0; the later peak counts only up to B, so the excess
    # is 4 GiB - 256 MiB = 3.75 GiB (counting the overrun past B would give 4.05).
    m = re.search(r"\|\s+1/1\s+100\.0%\s+(\S+)\s+(\S+)", line)
    assert m and m.group(1) == "3.75" and m.group(2) == "3.75", line
    assert "0 scopes timed from their first running sighting, 1 already running" in buf.getvalue()
    # a job first seen at the second sample of a span is aged from that sighting, not from its launch
    # stamp (which includes its queue wait): 600 s of launch age at sighting, 20 s later it is 20 s old
    late = [(1000.0, {"slice_reserve": {}, "queued": [], "scopes": []}),
            (1020.0, {"slice_reserve": {}, "queued": [], "scopes": [{"scope_id": "q", "age_seconds": 600,
                                                                      "peak_rss": GIB, "reserve_bytes": b}]}),
            (1040.0, {"slice_reserve": {}, "queued": [], "scopes": [{"scope_id": "q", "age_seconds": 620,
                                                                      "peak_rss": GIB, "reserve_bytes": b}]})]
    assert apply_grant_age(late, [[1000.0, 1040.0]]) == (1, 0)
    assert late[2][1]["scopes"][0]["age_seconds"] == 20, late[2][1]["scopes"][0]
    print("selftest: pass")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--selftest", action="store_true")
    ap.add_argument("--db")
    ap.add_argument("--journal")
    ap.add_argument("--live", nargs="*", default=[])
    a = ap.parse_args()
    if a.selftest:
        selftest()
        return
    if not a.db or not a.journal:
        ap.error("--db and --journal are required")
    out = sys.stdout
    with open(a.journal, errors="replace") as fh:
        jl = fh.readlines()
    ceil = []
    enforce_since = None
    for line in jl:
        m = CEIL_RE.match(line)
        if m:
            ceil.append((float(m.group(1)), int(m.group(2))))
        elif enforce_since is None:
            m2 = ENF_RE.match(line)
            if m2:
                enforce_since = float(m2.group(1))
    ceil.sort()
    ct = [c[0] for c in ceil]

    def ceil_series(t):
        i = bisect.bisect_right(ct, t) - 1
        return (ceil[i][1],) if i >= 0 else None

    rows = load_store(a.db)
    jobs = store_jobs(rows, enforce_since)
    t_end = max(j.e for j in jobs)
    print("# AIRA-356 auto-resize replay", file=out)
    print("store rows %d; AIRA-324 enforce first seen %s; FLOOR %d MiB; policies t0 %s min x m %s" % (
        len(jobs), "%.0f" % enforce_since if enforce_since else "never", FLOOR // MIB, T0S, MS), file=out)
    print("", file=out)
    win = store_section(jobs, t_end, out)
    store_timeline(win, t_end, ceil_series if ceil else None, out)
    snaps = load_live(a.live)
    live_section(snaps, out)
    journal_section(jl, out)
    coverage_section(win, snaps, t_end, out)
    print("", file=out)
    print("LIMITS: see the module docstring (no store timeline: final peak used as P(t0); page cache in memory.peak;", file=out)
    print("20-row retention and absent running jobs; a few hours of live trajectories; static queue estimates).", file=out)


if __name__ == "__main__":
    main()
