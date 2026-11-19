#!/usr/bin/env python3
"""AIRA-354 measured overcommit: how far may the booking ledger exceed the ceiling?

Spec: docs/superpowers/specs/2026-11-19-measured-overcommit-design.md (section 2).

HOST-SAFETY CRITERION, fixed before this script was first run (spec section 2):
  The rule under test lets the ledger book up to k x L, where L = effective ceiling less
  headroom (today k = 1). X(t) = (k - 1) x L(t) is the extra booking it allows. Each model
  below gives a counterfactual host MemAvailable MA'(t); the criterion counts seconds.
    HS1  model Q (the jobs really queued at t, admitted FIFO into X(t), each at its own
         held memory.peak): 0 seconds with MA' < 10 GiB (8 GiB watchdog trip + 2 GiB).
    HS2  model I (X(t) filled at the p99 bootstrapped concurrent peak/booking ratio):
         0 seconds with MA' < 10 GiB.
    HS3  model W (X(t) fully used at once, the running jobs as measured): 0 seconds with
         MA' < 8 GiB.
  The chosen k is the largest grid value meeting HS1-HS3. The static worst case (every job at
  its cap at once) is reported, never gated: no k > 1 can meet it.

Inputs (all read-only copies; never point --db at the live file):
  sqlite3 ~/.local/state/aira/state.db ".backup $HOME/tmp/aira-overcommit-store/state-copy.db"
  journalctl --user -u aira-daemon.service --since 2026-11-12 -o short-unix --no-pager \\
      > ~/tmp/aira-overcommit-store/journal.txt
  optional --live: lines "<epoch ms>\\t<aira confine --list JSON>" (sampler in the spec, 11)
  optional --watch: a copy of the AIRA-324 watcher's watch.log (JSON lines)
  aira confine -- python3 -I docs/dev/aira-354-overcommit-replay.py --db ... --journal ...

LIMITS (printed again at the end)
  - journal lines: one per >= 1 GiB move of the ceiling or of (ceiling - sliceAnon), at most
    one a minute (logSliceCeiling); between lines the value is held. Sub-minute spikes are
    invisible. sliceAnon excludes the slice's reclaimable cache.
  - the store keeps 20 rows per exact signature and only OOM or clean runs (AIRA-264); aitest
    worker rows carry no timeline; jobs still running at the copy are absent. The visible
    timeline therefore holds about half the booked load (AIRA-324 gate report, section 3).
  - memory.peak counts page cache (AIRA-318) and is held for the whole run: an over-estimate
    of each job's real use at any instant. OOM rows' peaks are censored at the cap.
  - models Q/I/W add the extra use on top of the measured MemAvailable and never remove a job
    later, so they ignore that a job admitted earlier also ends earlier (conservative).
  - the CPU ledger (32 slots) is not in the store; the yield is RAM-only unless stated.
  - models W and I add the extra booking on top of the MEASURED use and keep the observed running
    set, so they cannot see that overbooking changes which jobs overlap. The AIRA-324 gate report
    (section 6.1) rejected the same approximation as a bound. Their passes are not a host-safety
    bound (plan-fix round 1); part 3 (d) gives the margin form instead.
  - part 6's first rows stack the factor on AIRA-324's live 0.9 discount; the "net" rows book
    full pins, the configuration overbook enforce would run in (plan-fix round 1).
"""
import argparse
import bisect
import json
import random
import re
import sqlite3
import sys

GIB = 1 << 30
MIB = 1 << 20
H0 = 2 * GIB + 64 * MIB          # admitSliceHeadroom(1): the smallest headroom, so the largest L
FLOOR = 8 * GIB                  # watchdogLowMemAvailable == sliceCeilingFreeMinDefault
MARGIN = 2 * GIB                 # admitSliceHeadroomBaseDefault
STEP = 10                        # grid, seconds
CPU_SLOTS = 32                   # journal: "CPU admission ceiling 32 slots (2 per core x 16 cores)"
KS = [1.0, 1.1, 1.15, 1.2, 1.25, 1.3, 1.4, 1.5, 1.75, 2.0]
XS = [4, 8, 12, 16]              # additive variant, GiB

CEIL_RE = re.compile(r"^(\d+(?:\.\d+)?) .*slice ceiling (?:un)?throttled \(\w+\): (\d+) effective / (\d+) configured, "
                     r"sliceAnon=(\d+) MemAvailable=(\d+)")


def q(xs, p):
    if not xs:
        return float("nan")
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(p * (len(xs) - 1) + 0.5))]


def g(b):
    return "%.2f" % (b / GIB)


def extra(k, E, U, M, bounded):
    """The booking the rule allows above L = E - H0. 'k only': (k-1)L. The spec's rule adds
    two bounds: the limit never exceeds the slice's memory.max less headroom (M - H0), and the
    over-ceiling part never exceeds the physical room under the ceiling (L - sliceAnon)."""
    L = max(0, E - H0)
    x = (k - 1) * L
    if bounded:
        x = min(x, M - H0 - L, L - U)
    return max(0, x)


def breach_ratio(k, E):
    """Use / bookings at which MemAvailable reaches FLOOR with the ledger full at k L (L = E - H0)."""
    L = E - H0
    return E / (k * L) if L > 0 else float("inf")


def live_admit(queued, room, phys, free, fifo):
    """Queued jobs (list order) a live room admits, CPU-capped: FIFO stops at the first misfit,
    skip-misfit (an upper bound: no head budget, no freeze) passes over it."""
    n = used = 0
    for w in queued:
        if n >= free:
            break
        c = w["reserve_bytes"]
        if used + c <= room and c <= phys:
            n += 1
            used += c
        elif fifo:
            break
    return n


def load_journal(lines):
    out = []
    for line in lines:
        m = CEIL_RE.match(line)
        if m:
            # (t, effective ceiling E, sliceAnon U, MemAvailable MA, configured slice memory.max M)
            out.append((float(m.group(1)), int(m.group(2)), int(m.group(4)), int(m.group(5)), int(m.group(3))))
    out.sort()
    return out


class Series:
    """The journal's (E, U, MA, M) held from each line to the next."""

    def __init__(self, rows):
        self.t = [r[0] for r in rows]
        self.v = [r[1:] for r in rows]

    def at(self, t):
        i = bisect.bisect_right(self.t, t) - 1
        return self.v[i] if i >= 0 else None


class Job:
    __slots__ = ("a", "s", "e", "b", "p", "oom", "sig", "pool_max")

    def __init__(self, a, s, e, b, p, oom, sig):
        self.a, self.s, self.e, self.b, self.p, self.oom, self.sig = a, s, e, b, p, oom, sig
        self.pool_max = p


def load_jobs(db):
    con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = con.execute(
        "select started_at_ms, wait_ms, wall_ms, budget, peak_rss, oom, signature from confine_peak_history "
        "where kind='confine' and started_at_ms is not null and wall_ms is not null and wait_ms is not null "
        "and budget is not null and budget > 0").fetchall()
    pools = {}
    for sig, peak in con.execute(
            "select signature, peak_rss from confine_peak_history where kind='confine' and peak_rss is not null "
            "and signature in (select signature from confine_peak_history where kind='confine' and wall_ms is not null)"):
        pools.setdefault(sig, []).append(peak)
    con.close()
    jobs, unknown = [], 0
    for start, wait, wall, budget, peak, oom, sig in rows:
        if peak is None:
            unknown += 1
            peak = budget              # an unknown peak is bounded at its cap, never 0
        s = start / 1000.0
        jobs.append(Job(s - wait / 1000.0, s, s + wall / 1000.0, budget, min(peak, budget) if not oom else budget, oom, sig))
    for j in jobs:
        pool = pools.get(j.sig) or [j.p]
        j.pool_max = min(max(pool), j.b)
    return jobs, pools, unknown


def sweep(jobs, lo, hi, step=STEP):
    """Yield (t, running, queued) on the grid; queued sorted by arrival."""
    jobs = sorted(jobs, key=lambda j: j.a)
    active, i = [], 0
    t = lo
    while t < hi:
        while i < len(jobs) and jobs[i].a <= t:
            active.append(jobs[i])
            i += 1
        active = [j for j in active if j.e > t]
        running = [j for j in active if j.s <= t]
        queued = sorted((j for j in active if j.s > t), key=lambda j: j.a)
        yield t, running, queued
        t += step


def admit_prefix(queued, room, fifo=True, slots=None):
    """Jobs the extra room admits: FIFO stops at the first that does not fit; backfill skips it."""
    out, used = [], 0
    for j in queued:
        if slots is not None and len(out) >= slots:
            break
        if used + j.b <= room:
            out.append(j)
            used += j.b
        elif fifo:
            break
    return out


def report(jobs, pools, unknown, series, lo, hi, live=None, watch=None, seed=354, hidden_jobs=None):
    P = print
    rng = random.Random(seed)
    P("# AIRA-354 overcommit replay")
    P("window %d .. %d (unix s), %.1f h; grid %d s; %d timeline rows (%d with an unknown peak, bounded at cap)"
      % (lo, hi, (hi - lo) / 3600, STEP, len(jobs), unknown))

    # ---- part 1: the journal, every job (hidden ones included)
    P("\n## 1. Journal (all jobs): host MemAvailable, slice anon, effective ceiling")
    pts = [series.at(t) for t in range(int(lo), int(hi), STEP)]
    pts = [p for p in pts if p]
    ma = [p[2] for p in pts]
    u = [p[1] for p in pts]
    e = [p[0] for p in pts]
    P("samples=%d  MemAvailable GiB: min %s p1 %s p5 %s p50 %s" % (len(pts), g(min(ma)), g(q(ma, .01)), g(q(ma, .05)), g(q(ma, .5))))
    P("sliceAnon GiB: p50 %s p99 %s max %s;  effective ceiling GiB: min %s p50 %s max %s"
      % (g(q(u, .5)), g(q(u, .99)), g(max(u)), g(min(e)), g(q(e, .5)), g(max(e))))
    for thr in (8, 10, 12, 16, 20):
        P("observed seconds with MemAvailable < %2d GiB: %d" % (thr, STEP * sum(1 for x in ma if x < thr * GIB)))
    ul = [p[1] / (p[0] - H0) for p in pts if p[0] > H0]
    P("sliceAnon / L (L = ceiling - %s GiB): p50 %.3f p90 %.3f p99 %.3f max %.3f" % (g(H0), q(ul, .5), q(ul, .9), q(ul, .99), max(ul)))

    # ---- part 2: the visible timeline
    P("\n## 2. Visible timeline (store rows): sum of held peaks / sum of bookings of the RUNNING set")
    grid = []
    for t, running, queued in sweep(jobs, lo, hi):
        j = series.at(t)
        if j is None:
            continue
        grid.append((t, running, queued, j))
    rho, cov, nvis, hour = [], [], [], {}
    for t, running, queued, (E, U, MA, M) in grid:
        B = sum(x.b for x in running)
        Pk = sum(x.p for x in running)
        nvis.append(len(running))
        if B >= 8 * GIB:
            r = Pk / B
            rho.append(r)
            hour.setdefault(int(t // 3600), []).append(r)
        if U > 0:
            cov.append(Pk / U)
    P("instants with visible booking >= 8 GiB: %d of %d" % (len(rho), len(grid)))
    P("held-peak ratio rho_vis: p50 %.3f p90 %.3f p99 %.3f max %.3f" % (q(rho, .5), q(rho, .9), q(rho, .99), max(rho)))
    P("visible held peaks / real sliceAnon: p10 %.2f p50 %.2f p90 %.2f (above 1: held peaks and page cache overstate; below 1: hidden jobs)"
      % (q(cov, .1), q(cov, .5), q(cov, .9)))
    P("visible running jobs: p50 %d p90 %d max %d (the live list shows the full count, part 6)" % (q(nvis, .5), q(nvis, .9), max(nvis)))

    # ---- part 3: bootstraps
    P("\n## 3. Correlated-peak estimates")
    hours = sorted(hour)
    p99s = []
    for _ in range(1000):
        sample = []
        for h in (rng.choice(hours) for _ in hours):
            sample.extend(hour[h])
        p99s.append(q(sample, .99))
    P("(a) block bootstrap by clock hour (%d hours, 1000 draws): p99 of rho_vis 90%% interval [%.3f, %.3f]"
      % (len(hours), q(p99s, .05), q(p99s, .95)))
    big = [(t, r) for t, r, qd, j in grid if sum(x.b for x in r) >= 8 * GIB]
    ceil_at = {t: j[0] for t, r, qd, j in grid}
    draws, draws_e = [], []
    for _ in range(400):
        t, running = rng.choice(big)
        B = sum(x.b for x in running)
        for _ in range(200):
            draws.append(sum(min(rng.choice(pools.get(x.sig) or [x.p]), x.b) if not x.oom else x.b for x in running) / B)
            draws_e.append(ceil_at[t])
    rho_i = q(draws, .99)
    P("(b) each running job's peak redrawn from its own signature's retained peaks (400 instants x 200): "
      "p50 %.3f p99 %.3f p99.9 %.3f max %.3f" % (q(draws, .5), rho_i, q(draws, .999), max(draws)))
    smax = [sum(x.pool_max for x in r) / sum(x.b for x in r) for t, r in big]
    P("(c) every running job at its signature's LARGEST retained peak at once: p50 %.3f p99 %.3f max %.3f"
      % (q(smax, .5), q(smax, .99), max(smax)))
    rho_i = max(rho_i, q(rho, .99))
    P("model I uses rho = %.3f (the larger of (b) p99 and rho_vis p99)" % rho_i)
    # (d) Added in plan-fix round 1: the margin form of the criterion, reconciled with (b) and (c).
    # With the ledger full at k L and memory outside the slice unchanged, host MemAvailable reaches
    # the 8 GiB trip when the running set's real use / bookings reaches r*(k) = (L + H) / (k L).
    P("(d) breach ratio r*(k) = (L + H) / (k L): the running set's use / bookings at which host MemAvailable "
      "reaches 8 GiB with the ledger full at k L (outside memory unchanged). A margin, not a probability.")
    ue = [p[1] / p[0] for p in pts if p[0] > 0]
    for k in (1.1, 1.25, 1.4):
        rb = [breach_ratio(k, E) for E in e]
        sb = sum(1 for d, E in zip(draws, draws_e) if d >= breach_ratio(k, E)) / len(draws)
        sc = sum(1 for (t, r), s in zip(big, smax) if s >= breach_ratio(k, ceil_at[t])) / len(smax)
        P("  k=%.2f: r* p50 %.3f (min %.3f); share at or above r*: (b) %.4f of draws, (c) %.4f of instants; "
          "same-mix model (all use x k): max sliceAnon/E %.3f against 1/k = %.3f"
          % (k, q(rb, .5), min(rb), sb, sc, max(ue), 1 / k))

    # ---- part 4: counterfactual host MemAvailable per k
    P("\n## 4. Counterfactual host MemAvailable (seconds below a threshold; min MA')")
    P("models: Q = real queued jobs admitted FIFO into X(t) at held peaks; Qb = same, backfill; "
      "I = X(t) x rho; W = X(t) at 100%%; S = (k-1) x real sliceAnon (same mix scaled)")
    P("rules: 'k only' X = (k-1)L (the pre-registered test); 'spec' X = min((k-1)L, slice memory.max - H - L, L - sliceAnon)")
    chosen = {}
    for rule in ("k only", "spec"):
        bounded = rule == "spec"
        P("\n[%s]" % rule)
        P("%-6s %-8s %-8s %-8s %-8s %-8s %-8s %-8s %-8s %-8s  %s" %
          ("k", "Q<10", "Qb<10", "I<10", "W<8", "W<10", "S<10", "minW", "minQb", "medX", "HS1 HS2 HS3"))
        for k in KS:
            c = {"Q": 0, "Qb": 0, "I": 0, "W8": 0, "W10": 0, "S": 0}
            mins = {"W": float("inf"), "Qb": float("inf")}
            xs = []
            for t, running, queued, (E, U, MA, M) in grid:
                X = extra(k, E, U, M, bounded)
                xs.append(X)
                q_f = sum(j.p for j in admit_prefix(queued, X, True))
                q_b = sum(j.p for j in admit_prefix(queued, X, False))
                c["Q"] += MA - q_f < FLOOR + MARGIN
                c["Qb"] += MA - q_b < FLOOR + MARGIN
                c["I"] += MA - rho_i * X < FLOOR + MARGIN
                c["W8"] += MA - X < FLOOR
                c["W10"] += MA - X < FLOOR + MARGIN
                c["S"] += MA - (k - 1) * U < FLOOR + MARGIN
                mins["W"] = min(mins["W"], MA - X)
                mins["Qb"] = min(mins["Qb"], MA - q_b)
            if c["Q"] == 0 and c["I"] == 0 and c["W8"] == 0:
                chosen[rule] = k
            P("%-6.2f %-8d %-8d %-8d %-8d %-8d %-8d %-8s %-8s %-8s  %s %s %s" % (
                k, STEP * c["Q"], STEP * c["Qb"], STEP * c["I"], STEP * c["W8"], STEP * c["W10"], STEP * c["S"],
                g(mins["W"]), g(mins["Qb"]), g(q(xs, .5)), "pass" if c["Q"] == 0 else "FAIL",
                "pass" if c["I"] == 0 else "FAIL", "pass" if c["W8"] == 0 else "FAIL"))
        P("CHOSEN k [%s] (largest meeting HS1-HS3): %s" % (rule, chosen.get(rule)))
    P("additive variant, X fixed (GiB): seconds W<8, I<10")
    for x in XS:
        w8 = sum(1 for t, r, qd, (E, U, MA, M) in grid if MA - x * GIB < FLOOR)
        i10 = sum(1 for t, r, qd, (E, U, MA, M) in grid if MA - rho_i * x * GIB < FLOOR + MARGIN)
        P("  X=%2d GiB: W<8 %d s, I<10 %d s" % (x, STEP * w8, STEP * i10))
    Lmax = max(E for t, r, qd, (E, U, MA, M) in grid) - H0
    P("static worst case (every job at its cap at once), at the window's largest L = %s GiB:" % g(Lmax))
    for k in KS:
        P("  k=%.2f: MemAvailable >= %s GiB" % (k, g(FLOOR + H0 - (k - 1) * Lmax)))
    room = [E - H0 - U for t, r, qd, (E, U, MA, M) in grid]
    P("physical room L - sliceAnon over the window: min %s p1 %s p50 %s GiB (the 'backed' term binds when it is below (k-1)L)"
      % (g(min(room)), g(q(room, .01)), g(q(room, .5))))
    # The journal-only models (I, W, S) need no store rows, so they also run over the whole
    # journal span (every line since the dump's --since), not just the store's window.
    j0, j1 = series.t[0], series.t[-1]
    full = [v for v in (series.at(t) for t in range(int(j0) + 1, int(j1), STEP)) if v is not None]
    P("journal-only models over the whole journal, %.1f h (%d samples): seconds I<10, W<8, W<10; min W"
      % ((j1 - j0) / 3600, len(full)))
    for rule in ("k only", "spec"):
        for k in KS:
            i10 = w8 = w10 = 0
            mw = float("inf")
            for E, U, MA, M in full:
                X = extra(k, E, U, M, rule == "spec")
                i10 += MA - rho_i * X < FLOOR + MARGIN
                w8 += MA - X < FLOOR
                w10 += MA - X < FLOOR + MARGIN
                mw = min(mw, MA - X)
            P("  [%s] k=%.2f: I<10 %d s, W<8 %d s, W<10 %d s, min W %s GiB" % (rule, k, STEP * i10, STEP * w8, STEP * w10, g(mw)))

    # ---- part 5: yield
    P("\n## 5. Queue wait removed (model Q, FIFO, static per instant; RAM only, then a CPU-slot cap)")
    waited = [j for j in jobs if lo <= j.a < hi and j.s - j.a >= 1]
    total = sum(j.s - j.a for j in waited) / 3600
    P("visible jobs that waited >= 1 s: %d, wait-hours %.1f" % (len(waited), total))
    if hidden_jobs is None:
        hidden_jobs = max(0, (q([x[3] for x in live_recs(live)], .5) if live else 0) - q(nvis, .5))
    P("CPU cap: free slots = 32 - visible running - %d hidden (live median jobs less the visible median; ESTIMATED)" % hidden_jobs)
    for k in KS[1:]:
        for cap in (None, "cpu"):
            first = {}
            for t, running, queued, (E, U, MA, M) in grid:
                X = extra(k, E, U, M, True)
                slots = None if cap is None else max(0, CPU_SLOTS - len(running) - hidden_jobs)
                for j in admit_prefix(queued, X, True, slots):
                    first.setdefault(id(j), t)
            removed = sum(max(0, j.s - first[id(j)]) for j in waited if id(j) in first) / 3600
            P("  k=%.2f %-14s wait-hours removed %.1f (%.0f%%)" % (k, "RAM only" if cap is None else "RAM+CPU (est.)",
                                                                 removed, 100 * removed / total if total else 0))

    # ---- part 6: live samples
    if live:
        P("\n## 6. Live `aira confine --list` samples (every job: totals from the daemon)")
        recs = live_recs(live)
        ratio = [x[5] / x[1] for x in recs if x[1] > 0]
        disc = [sum(sc["pin_bytes"] - sc["reserve_bytes"] for sc in d["scopes"] if sc.get("pin_bytes")) for t, d in live]
        ratio_pin = [x[5] / (x[1] + dd) for x, dd in zip(recs, disc) if x[1] + dd > 0]
        P("AIRA-324 enforce 0.9 was on: pin discount in the ledger p50 %s GiB max %s GiB; real use / full-pin bookings p50 %.3f max %.3f"
          % (g(q(disc, .5)), g(max(disc)), q(ratio_pin, .5), max(ratio_pin)))
        P("samples %d over %.1f min" % (len(recs), (recs[-1][0] - recs[0][0]) / 60000 if len(recs) > 1 else 0))
        P("real use / booked (slice current - reclaimable over granted): p50 %.3f p90 %.3f max %.3f" % (q(ratio, .5), q(ratio, .9), max(ratio)))
        P("granted GiB p50 %s; ceiling p50 %s; real use p50 %s max %s; MemAvailable min %s"
          % (g(q([x[1] for x in recs], .5)), g(q([x[2] for x in recs], .5)), g(q([x[5] for x in recs], .5)), g(max(x[5] for x in recs)), g(min(x[6] for x in recs))))
        ramb = sum(1 for x in recs if x[4] > 0 and x[1] >= x[2] - GIB)
        P("samples with a queue: %d; of them RAM-bound (granted >= ceiling - 1 GiB): %d" % (sum(1 for x in recs if x[4] > 0), ramb))
        P("jobs (CPU slots, 1 each by default): p50 %d max %d of %d; queued p50 %d max %d"
          % (q([x[3] for x in recs], .5), max(x[3] for x in recs), CPU_SLOTS, q([x[4] for x in recs], .5), max(x[4] for x in recs)))
        util = []
        for a, b in zip(recs, recs[1:]):
            if a[7] is not None and b[7] is not None and b[8]:
                util.append((b[7] - a[7]) / ((b[0] - a[0]) * 1000.0 * b[8]))
        if util:
            P("slice CPU utilisation (of %d cores): p50 %.2f p90 %.2f max %.2f" % (recs[-1][8], q(util, .5), q(util, .9), max(util)))
        per = []
        for t, d in live:
            for s in d["scopes"]:
                if s.get("rss_bytes") is not None and s.get("reserve_bytes"):
                    per.append(s["rss_bytes"] / s["reserve_bytes"])
        P("per-job memory.current / reserve (cache included), all samples: p50 %.2f p90 %.2f p99 %.2f" % (q(per, .5), q(per, .9), q(per, .99)))
        P("queued bytes per sample: p50 %s GiB max %s GiB" % (g(q([sum(w["reserve_bytes"] for w in d.get("queued") or []) for t, d in live], .5)),
                                                             g(max(sum(w["reserve_bytes"] for w in d.get("queued") or []) for t, d in live))))
        for k in (1.1, 1.15, 1.2, 1.25, 1.3, 1.4):
            n_ram, n_cpu, b_cpu, ma_after = [], [], [], []
            for (t, d), x in zip(live, recs):
                L = x[2]                                   # ceiling_bytes is already less the headroom
                hr = (x[10] - L) if x[10] else H0          # the headroom the daemon subtracted
                limit = min(int(k * L), (x[9] - hr) if x[9] else int(k * L), 2 * L - x[5])
                room = min(limit - x[1], L - x[5])         # the spec's rule: bounded ledger at k, physical at 1
                phys = L - x[5]
                ok_ram, used = [], 0
                for w in d.get("queued") or []:                      # the list's queued order (FIFO for normal priority)
                    c = w["reserve_bytes"]
                    if used + c <= room and c <= phys:
                        ok_ram.append(c)
                        used += c
                    else:
                        break
                n_ram.append(len(ok_ram))
                free = max(0, CPU_SLOTS - x[3])
                n_cpu.append(min(len(ok_ram), free))
                b_cpu.append(sum(ok_ram[:free]))
                ma_after.append(x[6] - sum(ok_ram[:free]))
            P("  k=%.2f: queued jobs admitted now, RAM only p50 %d max %d; RAM+CPU p50 %d max %d (%s GiB p50); "
              "MemAvailable if all of those used their whole booking at once: min %s GiB"
              % (k, q(n_ram, .5), max(n_ram), q(n_cpu, .5), max(n_cpu), g(q(b_cpu, .5)), g(min(ma_after))))
        # Added in plan-fix round 1. The rows above subtract the ledger as the daemon booked it, in
        # AIRA-324 charge bytes, so they stack the factor on 324's 0.9 discount. Overbook enforce
        # requires pin-charge off, so the deployable comparison books every running job at its full
        # pin (each scope's pin_bytes - reserve_bytes added back). A queued row's reserve_bytes is
        # its pin (admit.go: queuedEntries carry waiter.reserve).
        P("net of track 1 (AIRA-324 discount added back: full pins booked, the configuration overbook enforce requires):")
        withq = [x for x in recs if x[4] > 0]
        ram_free_cpu = sum(1 for x in withq if x[1] >= x[2] - GIB and x[3] < CPU_SLOTS)
        cpu_full = sum(1 for x in withq if x[3] >= CPU_SLOTS)
        P("  samples with a queue %d: RAM-bound with a free CPU slot %d; CPU slots full %d "
          "(slots estimated as one per granted waiter: the list carries no CPU-ledger total)" % (len(withq), ram_free_cpu, cpu_full))
        cpu_only = [min(x[4], max(0, CPU_SLOTS - x[3])) for x in recs]
        P("  ceiling for ANY RAM rule (RAM unlimited, CPU slots only): queued jobs admitted p50 %d mean %.2f max %d, "
          "none in %d of %d (CPU slots full)" % (q(cpu_only, .5), sum(cpu_only) / len(cpu_only), max(cpu_only),
                                                sum(1 for n in cpu_only if n == 0), len(cpu_only)))
        for k in (1.1, 1.15, 1.2, 1.25, 1.3, 1.4):
            rows = {("stacked", True): [], ("net", True): [], ("net", False): []}
            netx = []
            for (t, d), x, dd in zip(live, recs, disc):
                L = x[2]
                hr = (x[10] - L) if x[10] else H0
                limit = min(int(k * L), (x[9] - hr) if x[9] else int(k * L), 2 * L - x[5])
                phys = L - x[5]
                free = max(0, CPU_SLOTS - x[3])
                netx.append(limit - L - dd)
                for name, fifo in rows:
                    booked = x[1] + (dd if name == "net" else 0)
                    rows[(name, fifo)].append(live_admit(d.get("queued") or [], min(limit - booked, phys), phys, free, fifo))

            def s(v):
                return "p50 %d mean %.2f max %d, none admitted in %d of %d" % (q(v, .5), sum(v) / len(v), max(v), sum(1 for n in v if n == 0), len(v))
            P("  k=%.2f: extra room net of the 324 discount GiB min %s p50 %s max %s" % (k, g(min(netx)), g(q(netx, .5)), g(max(netx))))
            P("          stacked FIFO+CPU %s" % s(rows[("stacked", True)]))
            P("          net     FIFO+CPU %s" % s(rows[("net", True)]))
            P("          net     skip-misfit+CPU (upper bound) %s" % s(rows[("net", False)]))

    if watch:
        P("\n## 7. AIRA-324 watcher (factor 0.9 enforce, 2026-11-16/17): inferred sliceAnon = ceiling + 8 - MemAvailable")
        ws = [w for w in watch if w.get("granted_gib") and w.get("ceiling_gib") and w.get("mem_available_gib")]
        r = [(w["ceiling_gib"] + 8 - w["mem_available_gib"]) / w["granted_gib"] for w in ws]
        if r:
            P("samples %d: inferred real use / booked p50 %.3f p90 %.3f max %.3f; queued max %d; running max %d"
              % (len(ws), q(r, .5), q(r, .9), max(r), max(w.get("queued", 0) for w in ws), max(w.get("running", 0) for w in ws)))
    P("\nLIMITS: see the module docstring (journal held values, 20-row retention, hidden jobs, held cache-inclusive peaks, CPU).")
    return chosen


def live_recs(live):
    recs = []
    for t, d in live:
        r = d["slice_reserve"]
        eff = r["slice_current_bytes"] - r["slice_reclaimable_bytes"]
        recs.append((t, r["granted_bytes"], r["ceiling_bytes"], r["jobs"], r["queued"], eff, r["mem_available_bytes"],
                     r.get("slice_cpu_usage_usec"), r.get("cpu_cores"), r.get("slice_max_bytes"),
                     r.get("ceiling_effective_bytes")))
    return recs


def selftest():
    fails = 0

    def check(name, got, want):
        nonlocal fails
        ok = got == want
        fails += not ok
        print("%s %s: got %r want %r" % ("ok  " if ok else "FAIL", name, got, want))

    lines = ["1000.5 host aira[1]: 2026/11/19 00:00:00 aira daemon: slice ceiling throttled (enforce): "
             "%d effective / 68719476736 configured, sliceAnon=%d MemAvailable=%d bound-by=system-pressure" % (40 * GIB, 10 * GIB, 30 * GIB),
             "1060.0 host aira[1]: unrelated line"]
    js = load_journal(lines)
    check("journal line parsed", js, [(1000.5, 40 * GIB, 10 * GIB, 30 * GIB, 64 * GIB)])
    s = Series(js)
    check("held value after the line", s.at(1059), (40 * GIB, 10 * GIB, 30 * GIB, 64 * GIB))
    check("no value before the first line", s.at(999), None)
    a = Job(0, 100, 200, 4 * GIB, 1 * GIB, 0, "a")
    b = Job(1, 100, 200, 2 * GIB, 2 * GIB, 0, "b")
    c = Job(2, 100, 200, 1 * GIB, 1 * GIB, 0, "c")
    check("FIFO stops at the first misfit", [x.sig for x in admit_prefix([a, b, c], 3 * GIB, True)], [])
    check("backfill skips it", [x.sig for x in admit_prefix([a, b, c], 3 * GIB, False)], ["b", "c"])
    check("slot cap", [x.sig for x in admit_prefix([b, c], 9 * GIB, True, 1)], ["b"])
    E = 42 * GIB + H0                              # L = 42 GiB
    check("extra, k only", extra(1.25, E, 10 * GIB, 64 * GIB, False), 10.5 * GIB)
    check("extra, k=1 is zero", extra(1.0, E, 10 * GIB, 64 * GIB, True), 0)
    check("extra, slice memory.max binds", extra(1.5, E, 10 * GIB, 64 * GIB, True), 64 * GIB - H0 - 42 * GIB)
    check("extra, physical room binds", extra(1.25, E, 36 * GIB, 64 * GIB, True), 6 * GIB)
    check("extra, real use over L gives 0", extra(1.25, E, 43 * GIB, 64 * GIB, True), 0)
    check("breach ratio at L = 42 GiB, k = 1.25", round(breach_ratio(1.25, E), 6), round((42 * GIB + H0) / (1.25 * 42 * GIB), 6))
    qd = [{"reserve_bytes": 4 * GIB}, {"reserve_bytes": 1 * GIB}, {"reserve_bytes": 1 * GIB}]
    check("live FIFO stops at the misfit", live_admit(qd, 3 * GIB, 9 * GIB, 9, True), 0)
    check("live skip-misfit passes it", live_admit(qd, 3 * GIB, 9 * GIB, 9, False), 2)
    check("live CPU cap", live_admit(qd, 9 * GIB, 9 * GIB, 1, False), 1)
    check("live per-job physical fit", live_admit(qd, 9 * GIB, 3 * GIB, 9, True), 0)
    st = list(sweep([a, b], 0, 210, 50))
    check("sweep: queued then running then gone", [(t, len(r), len(qd)) for t, r, qd in st],
          [(0, 0, 1), (50, 0, 2), (100, 2, 0), (150, 2, 0), (200, 0, 0)])
    print("selftest: %d failures" % fails)
    return fails


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db")
    ap.add_argument("--journal")
    ap.add_argument("--live")
    ap.add_argument("--watch")
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()
    if args.selftest:
        sys.exit(1 if selftest() else 0)
    jobs, pools, unknown = load_jobs(args.db)
    with open(args.journal, errors="replace") as f:
        series_rows = load_journal(f)
    series = Series(series_rows)
    lo = min(j.s for j in jobs) + 3600              # jobs started before the window are invisible
    hi = min(max(j.e for j in jobs), series_rows[-1][0]) - 3600   # jobs running at the copy are absent
    live = None
    if args.live:
        live = []
        with open(args.live) as f:
            for line in f:
                t, _, js = line.partition("\t")
                try:
                    live.append((int(t), json.loads(js)["data"]))
                except (ValueError, KeyError):
                    pass
    watch = None
    if args.watch:
        watch = []
        with open(args.watch) as f:
            for line in f:
                try:
                    watch.append(json.loads(line))
                except ValueError:
                    pass
    report(jobs, pools, unknown, series, lo, hi, live, watch)


if __name__ == "__main__":
    main()
