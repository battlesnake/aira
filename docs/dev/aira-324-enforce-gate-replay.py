#!/usr/bin/env python3
"""AIRA-324 enforce gate: counterfactual replay of the real confine timeline.

Spec: docs/superpowers/specs/2026-11-13-history-charge-design.md, section 6 item 2.
Report: docs/dev/aira-324-enforce-gate-report.md. Committed output:
docs/dev/aira-324-enforce-gate-replay.out.txt.

Inputs (both read-only; never point --db at the live store):
  --db       a COPY of ~/.local/state/aira/state.db (copy state.db and state.db-wal)
  --journal  a saved `journalctl --user -u aira-daemon.service -o short-unix` dump;
             only the 'slice ceiling throttled' and 'pin-charge observe' lines are read

Run (confined, measured peak 54-69M):
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
  * Part 5 adds a first-order estimate on the REAL slice (journal sliceAnon, which includes
    the hidden jobs; an estimate, not a bound) and the conditional sum-of-caps algebra.
  * Part 4b links the daemon's discountable-grant lines to rows (same pin, start within
    100 ms; a row has no scope id, so a link is a guess) to get the eligible population of
    the 5% trigger. attribute() keeps that uncertainty: lines with no row, and components
    that do not pair one to one, widen an interval, and the trigger may be unevaluated.
  * Part 5's sum-of-caps line is the slice-limit view. On this box host memory binds
    (bound-by=system-pressure): the report, section 6.1, gives the host-terms worst case.
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
FREE_MIN = 8 * GIB              # the slice ceiling's freeMin on this box (journal: freeMin=8589934592)
PIN_BASIS = "cap:operator:--memory-reserve"
FLOOR = MIB                     # pinChargeFloor
PARENT_PREFIX = "aitest-parent\x00"   # runner.AitestParentSignaturePrefix: a delegate parent, never discounted


def charge_for(pin, factor):
    """discountedPinCharge: ceil(pin x factor) clamped to [1 MiB, pin], in whole percent."""
    pct = int(round(factor * 100))
    c = -(-pin * pct // 100)
    return min(max(c, FLOOR), pin)


class Job:
    """One confine row. peak None (peak_rss NULL) is UNKNOWN: `known` is False and `peak` reads
    0 in the timeline sums (excluded; the report states the effect), while attribute() and
    above_charge() keep it as an unknown, never as a known-low sample."""
    __slots__ = ("idx", "arrival", "start", "dur", "peak", "known", "pin", "pin_class", "oom", "basis")

    def __init__(self, idx, arrival, start, dur, peak, pin, basis, oom):
        self.idx, self.arrival, self.start, self.dur = idx, arrival, start, dur
        self.known = peak is not None
        self.peak = peak if peak is not None else 0
        self.pin, self.basis, self.oom = pin, basis, oom
        self.pin_class = basis == PIN_BASIS


def load_jobs(db):
    if os.path.realpath(db) == os.path.realpath(os.path.expanduser("~/.local/state/aira/state.db")):
        sys.exit("refusing to read the live store; copy it to ~/tmp first")
    c = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = c.execute(
        "select rowid, started_at_ms, wait_ms, wall_ms, peak_rss, budget, budget_basis, oom "
        "from confine_peak_history where kind = 'confine' and started_at_ms is not null "
        "and wait_ms is not null and wall_ms is not null and budget is not null "
        "order by started_at_ms - wait_ms, rowid").fetchall()
    jobs = []
    for i, (_rid, st, wait, wall, peak, budget, basis, oom) in enumerate(rows):
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


START_RE = re.compile(r"systemd\[\d+\]: Started aira-daemon\.service")


def load_journal(path):
    """(ceiling series [(ms, effective, sliceAnon)], observe lines [(ms, pin, would, free, fit)],
    daemon starts [ms] (systemd's 'Started aira-daemon.service' lines))."""
    ceil, obs, starts = [], [], []
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
                continue
            if START_RE.search(line):
                starts.append(ms)
    ceil.sort()
    return ceil, obs, starts


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
    """{minute_start_ms: max level reached in that minute}, each minute clipped to [lo, hi):
    a partial first or last minute sees only the part inside the window."""
    out = {}
    lvl = 0
    i = 0
    m = lo - lo % 60000
    while m < hi:
        a, b = max(m, lo), min(m + 60000, hi)
        while i < len(st) and st[i][0] <= a:     # an event AT the boundary belongs to this minute's start
            lvl = st[i][1]
            i += 1
        mx = lvl
        j = i
        while j < len(st) and st[j][0] < b:
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


def memory_view(jobs, start, lo, hi=None, common_hi=None, busiest=None, value=None):
    """The spec's memory test: concurrent sum of memory.peak (each job holding its peak for
    its whole run), duration-weighted and job(burst)-weighted, over every minute and in the
    busiest clock hour (the hour with the highest duration-weighted mean).
    The window runs from lo to THIS timeline's final completion (hi, if not given): a replay
    that delays a job past the observed final completion must still see its peak.
    common_mean is the duration-weighted mean on [lo, common_hi), the same span for every
    timeline, for comparison only. busiest (an hour start, ms) fixes the hour for every timeline
    (the report fixes it once, by sliceAnon); None picks this timeline's own. value(j) replaces
    j.peak (the unknown-peak bound counts a NULL peak at its pin)."""
    if hi is None:
        hi = max(start[j.idx] + j.dur for j in jobs)
    st = step(jobs, start, value or (lambda j: j.peak))
    hours = {}
    h = lo - lo % 3600000
    while h < hi:
        hours[h] = level_stats(st, max(h, lo), min(h + 3600000, hi))
        h += 3600000
    # busiest among WHOLE clock hours inside the window: a clipped first or last hour's mean
    # is over a shorter span and would win on a short burst (any hour if none is whole)
    whole = [k for k in hours if k >= lo and k + 3600000 <= hi] or list(hours)
    if busiest is None:
        busiest = max(whole, key=lambda k: hours[k][0])
    elif busiest not in hours:
        hours[busiest] = level_stats(st, busiest, busiest + 3600000)
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
        "common_mean": level_stats(st, lo, common_hi)[0] if common_hi else None,
        "step": st,
    }


def moved(jobs, s, ref, only=None):
    """(earlier, later): jobs whose start in s is more than 1 s before / after ref."""
    js = [j for j in jobs if only is None or only(j)]
    return (sum(1 for j in js if s[j.idx] < ref[j.idx] - 1000),
            sum(1 for j in js if s[j.idx] > ref[j.idx] + 1000))


def queued(j):
    """The job really queued: its OBSERVED wait was at least 1 s."""
    return j.start - j.arrival >= 1000


def counterfactual(jobs, ce, pol, hid, fac, base, m_jobs, mlo, common_hi, busiest=None):
    """One factor row: the clairvoyant replay for the queue numbers (against the same
    config's baseline replay AND against the observed timeline), the blind replay for the
    memory view (its own window, through its own final completion)."""
    stt = {}
    s = simulate(jobs, ce, fac, pol, "clairvoyant", hid, stats=stt)
    obs = {j.idx: j.start for j in jobs}
    e, l = moved(jobs, s, base)
    eq, _ = moved(jobs, s, base, queued)
    oe, ol = moved(jobs, s, obs)
    _, olq = moved(jobs, s, obs, queued)
    blind = simulate(jobs, ce, fac, pol, "blind", hid)
    return {"start": s, "earlier": e, "later": l, "earlier_queued": eq, "obs_earlier": oe, "obs_later": ol,
            "obs_later_queued": olq,
            "waits": [s[j.idx] - j.arrival for j in m_jobs], "stats": stt,
            "mem": memory_view(jobs, blind, mlo, None, common_hi, busiest), "blind": blind}


def above_charge(pins, fac):
    """(rows with a KNOWN peak over ceil(fac x pin), rows with an unknown peak, rows): the spec's
    5% trigger arithmetic. The share lies in [above / n, (above + unknown) / n]."""
    return (sum(1 for j in pins if j.known and j.peak > charge_for(j.pin, fac)),
            sum(1 for j in pins if not j.known), len(pins))


def busiest_anon_hour(ceil_series, lo, hi, min_lines=30):
    """The clock hour (start, ms) with the highest mean sliceAnon over the journal's throttle
    lines in [lo, hi], among hours with at least min_lines lines (one a minute: a clipped or
    gappy hour shorter than 30 minutes is skipped). Fixed once: every memory row reports this
    hour. None if no hour qualifies."""
    hours = collections.defaultdict(list)
    for t, _eff, anon in ceil_series:
        if lo <= t <= hi:
            hours[t - t % 3600000].append(anon)
    ok = [h for h, v in hours.items() if len(v) >= min_lines]
    return max(ok, key=lambda h: sum(hours[h]) / len(hours[h])) if ok else None


def plus_anon(st, ceil_series, lo, hi):
    """max over minutes of (the timeline's minute max + the slice's real sliceAnon in that
    minute). A minute's sliceAnon is the largest of the reading held at its start (the last
    throttle line before it, which holds until the next line) and every line inside it, as
    minute_max carries the level held at the minute's start: max of a sum <= sum of the
    maxima, so no instant of the minute is understated. sliceAnon already holds the visible
    jobs' real use, so this double counts them: an upper bound on 'the visible replay plus
    everything the replay cannot see', over the minutes the lines cover. Minutes before the
    first line are skipped, and in the minute of the first line only the lines inside it count
    (nothing is known before it)."""
    mm = minute_max(st, lo, hi)
    ts = [c[0] for c in ceil_series]
    best = 0
    for m, lvl in mm.items():
        a = bisect.bisect_left(ts, m)
        b = bisect.bisect_left(ts, m + 60000)
        vals = [c[2] for c in ceil_series[max(a - 1, 0):b]]   # a - 1: the reading held at m
        if not vals:
            continue
        best = max(best, lvl + max(vals))
    return best


def trigger_verdict(above, unknown, n):
    """The 5% trigger with `unknown` discounted grants whose peak is not known (no row):
    the true share lies in [above / n, (above + unknown) / n]. Integer comparison, no
    rounding: met when above / n > 5%, not met when (above + unknown) / n <= 5%, else
    unevaluated. n = 0 is unevaluated."""
    if n <= 0:
        return "unevaluated"
    if 20 * above > n:
        return "met"
    if 20 * (above + unknown) <= n:
        return "not met"
    return "unevaluated"


TOL = 100   # ms. Measured grant-line to row-start offsets: -39 to +8 ms over 768 pairs (median 1 ms)


def attribute(grants, jobs, charge, tol=TOL):
    """The above-charge count over discounted grants (journal lines (ms, pin, ...)), with the
    attribution uncertainty kept. A row carries no scope id, so a grant's row can only be
    guessed: a pinned row of the same pin whose start is within tol ms of the line. Grants
    and rows linked that way form components (one pin each, so one charge(pin)). In a
    component of G grants and rows of which a are above the charge and b are not, a
    nearest-first one-to-one matching pairs k grants (k is at most the largest possible
    matching). Whichever rows are really the grants' own, at least k - b and at most a of the
    k paired grants are above, and the G - k unpaired ones are unknown. So the component adds
    max(0, k - b - u) to `low` and min(a + u, k) + G - k to `high`, where u counts the rows whose
    peak is unknown (peak_rss NULL): an unknown row may be either side of the charge. A one-to-one
    component (one grant, one row) with a known peak is the only case where low == high without
    a rowless grant.
    Not detectable, and so not covered: a grant whose own row is missing (failed, killed,
    still running, evicted) with an INELIGIBLE row of the same pin starting within tol of it
    looks exactly like a pair (the accepted gap; rows need the scope id to close it).
    Returns {grants, paired, rowless, unknown_peak, low, high, single, single_above, rows_used};
    single counts the one-to-one components with a known peak."""
    by = collections.defaultdict(list)
    for j in jobs:
        if j.pin_class:
            by[j.pin].append(j)
    edges = []
    for i, g in enumerate(grants):
        for j in by[g[1]]:
            d = abs(j.start - g[0])
            if d <= tol:
                edges.append((d, i, j.idx, j))
    parent = {}

    def find(x):
        while parent.get(x, x) != x:
            x = parent[x]
        return x
    for _, i, k, _ in edges:
        a, b = find(("g", i)), find(("r", k))
        if a != b:
            parent[a] = b
    edges.sort(key=lambda e: e[:3])
    used, match = set(), set()
    for _, i, k, _ in edges:
        if i in match or k in used:
            continue
        match.add(i)
        used.add(k)
    comps = collections.defaultdict(lambda: ([], {}))      # component -> (grant indices, {row idx: row})
    for i in range(len(grants)):
        comps[find(("g", i))][0].append(i)
    for _, i, k, j in edges:
        comps[find(("g", i))][1][k] = j
    out = {"grants": len(grants), "paired": len(match), "rowless": len(grants) - len(match), "unknown_peak": 0,
           "low": 0, "high": 0, "single": 0, "single_above": 0, "rows_used": set(k for _, _, k, _ in edges)}
    for gs, rows in comps.values():
        c = charge(grants[gs[0]][1])
        a = sum(1 for j in rows.values() if j.known and j.peak > c)
        u = sum(1 for j in rows.values() if not j.known)
        b = len(rows) - a - u
        k = sum(1 for i in gs if i in match)
        out["unknown_peak"] += u
        out["low"] += max(0, k - b - u)
        out["high"] += min(a + u, k) + len(gs) - k
        if len(gs) == 1 and len(rows) == 1 and u == 0:
            out["single"] += 1
            out["single_above"] += a
    return out


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


def report(jobs, extra, ceil_series, obs, out, slice_peak=None, starts=()):
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
    unk = [j for j in jobs if not j.known]
    P("rows with an UNKNOWN peak (peak_rss NULL): %d (pinned %d); the timeline sums count them as 0 (excluded), "
      "part 3 bounds them at their pin, part 4 keeps them unknown" % (len(unk), sum(1 for j in unk if j.pin_class)))
    P("daemon restarts in the window (systemd 'Started aira-daemon.service' lines): %d" %
      sum(1 for t in starts if lo <= t <= hi))
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
    bb = simulate(jobs, best[3], 1.0, best[2], "clairvoyant", best[4])
    zero = [j for j in m_jobs if not queued(j)]
    zw = [bb[j.idx] - j.arrival for j in zero]
    dev = [abs(bb[j.idx] - j.start) for j in m_jobs]
    P("per job, best fit: of %d jobs that did NOT queue (observed wait < 1 s), %s wait > 1 s in the baseline replay "
      "(their replayed wait p50 %.1fs p90 %.1fs): the residual is sampled once a minute, so the replay's capacity "
      "is the booking seen at the last tick and a job that started at once waits for the next tick. "
      "|replayed start - observed start|: p50 %.1fs p90 %.1fs" % (
          len(zero), pct(sum(1 for w in zw if w > 1000) / len(zero)), wait_s(zw, .5), wait_s(zw, .9),
          q(dev, .5) / 1000, q(dev, .9) / 1000))
    P("")

    # ---- part 2: counterfactual
    P("## 2. Counterfactual: pin rows booked at ceil(factor x pin), against the SAME config's baseline replay")
    P("AND against the observed timeline. earlier/later = jobs whose start moved by more than 1 s versus the")
    P("config's own baseline replay; e-queued = the earlier ones among jobs that REALLY queued (observed wait >= 1 s,")
    P("%d jobs); obs-e/obs-l = earlier/later than the OBSERVED start, obs-l-q = the later ones that REALLY queued" %
      sum(1 for j in jobs if queued(j)))
    P("(the rest never queued: the minute-tick artifact). d = change in wait (s) versus the baseline")
    P("replay, metric window; >60s = share waiting over 60 s (observed %s). phys-held = grants the physical-fit" %
      pct(sum(1 for w in ow if w > 60000) / len(ow)))
    P("rule held back (clairvoyant reading).")
    targets = [(best[1], best[2], best[3], best[4])]
    for name, ce, hid in configs:
        if name.startswith("journal - 1.00") or name in ("const 44 GiB", "const 48 GiB", "const 62 GiB"):
            targets.append((name, "bypass", ce, hid))
    for pol in ("fifo", "bypass", "backfill"):
        targets.append((best[1], pol, best[3], best[4]))
    seen = set()
    P("%-26s %-8s %4s %7s %6s %8s %6s %6s %7s %7s %7s %7s %6s %9s %8s" % (
        "ceiling", "policy", "fac", "earlier", "later", "e-queued", "obs-e", "obs-l", "obs-l-q", "p50 d", "p90 d",
        "p99 d", ">60s", "phys-held", "cpu-held"))
    mem_rows = []
    bh = busiest_anon_hour(ceil_series, mlo, hi) if ceil_series else None
    best_blind = {}
    for name, pol, ce, hid in targets:
        if (name, pol) in seen:
            continue
        seen.add((name, pol))
        bst = {}
        base = simulate(jobs, ce, 1.0, pol, "clairvoyant", hid, stats=bst)
        bw = [base[j.idx] - j.arrival for j in m_jobs]
        oe, ol = moved(jobs, base, obs_start)
        P("%-26s %-8s %4.1f %7s %6s %8s %6d %6d %7d %7s %7s %7s %6s %9s %8d" % (
            name, pol, 1.0, "-", "-", "-", oe, ol, moved(jobs, base, obs_start, queued)[1], "-", "-", "-",
            pct(sum(1 for x in bw if x > 60000) / len(bw)), "-", bst["cpu_held"]))
        mem_rows.append((name, pol, 1.0, memory_view(jobs, base, mlo, None, hi, bh)))
        for fac in (0.9, 0.8):
            r = counterfactual(jobs, ce, pol, hid, fac, base, m_jobs, mlo, hi, bh)
            if (name, pol) == (best[1], best[2]):
                best_blind[fac] = r["blind"]
            cw = r["waits"]
            P("%-26s %-8s %4.1f %7d %6d %8d %6d %6d %7d %+7.1f %+7.1f %+7.1f %6s %9d %8d" % (
                name, pol, fac, r["earlier"], r["later"], r["earlier_queued"], r["obs_earlier"], r["obs_later"],
                r["obs_later_queued"],
                wait_s(cw, .5) - wait_s(bw, .5), wait_s(cw, .9) - wait_s(bw, .9), wait_s(cw, .99) - wait_s(bw, .99),
                pct(sum(1 for x in cw if x > 60000) / len(cw)), r["stats"]["phys_refused"], r["stats"]["cpu_held"]))
            mem_rows.append((name, pol, fac, r["mem"]))
    P("")
    ov = memory_view(jobs, obs_start, mlo, hi, hi, bh)
    P("## 3. Memory in the replay: concurrent sum of memory.peak of the VISIBLE jobs, each holding its peak for")
    P("its whole run (blind physical reading), as a fraction of MemoryHigh (60 GiB). Each timeline is checked from")
    P("the metric start through ITS OWN final completion (mins = minutes checked; the observed timeline has %d)." %
      ov["minutes"])
    P("dur = duration-weighted (a random instant); job = burst-weighted (the instant each job starts). busiest = ONE")
    P("clock hour for every row, fixed once: the hour with the highest mean sliceAnon (the real slice, all jobs)")
    P("among hours with >= 30 throttle lines. over54 = minutes whose maximum exceeds 0.9 x MemoryHigh")
    P("(54 GiB). common = duration-weighted mean over the observed window only, the same span for every row.")
    P("%-26s %-8s %4s %-14s %8s %8s %8s %8s %9s %7s %6s %5s %6s" % (
        "ceiling", "policy", "fac", "busiest hour", "dur mean", "dur p99", "job p99", "hour max", "all job99",
        "minmax", "over54", "mins", "common"))
    for name, pol, fac, m in [("OBSERVED", "-", 1.0, ov)] + mem_rows:
        P("%-26s %-8s %4.1f %-14s %8.3f %8.3f %8.3f %8.3f %9.3f %7.3f %6d %5d %6.3f" % (
            name, pol, fac, m["busiest_hour"], m["hour_dur_mean"] / MEMORY_HIGH, m["hour_dur_p99"] / MEMORY_HIGH,
            m["hour_job_p99"] / MEMORY_HIGH, m["hour_max"] / MEMORY_HIGH, m["all_job_p99"] / MEMORY_HIGH,
            m["minute_max"] / MEMORY_HIGH, m["minutes_over_safe"], m["minutes"], m["common_mean"] / MEMORY_HIGH))
    best_blind[1.0] = simulate(jobs, best[3], 1.0, best[2], "blind", best[4])
    tl = [("observed timeline", 1.0, obs_start)] + [("best fit (%s, %s)" % (best[1], best[2]), f, best_blind[f])
                                                    for f in (1.0, 0.9, 0.8)]
    unk = [j for j in jobs if not j.known]
    if unk:
        at_pin = lambda j: j.peak if j.known else j.pin
        P("unknown peaks: %d rows have no peak and count as 0 above (excluded). Bounded at their pin instead, the "
          "minute max becomes: %s" % (len(unk), "; ".join(
              "%s %.1f: %.3f" % (n, f, memory_view(jobs, s_, mlo, None, hi, bh, at_pin)["minute_max"] / MEMORY_HIGH)
              for n, f, s_ in tl)))
    else:
        P("unknown peaks: none (every row in the window has a peak): the tables above exclude nothing")
    if ceil_series:
        P("visible replay PLUS the real slice (each minute's max of the timeline above + that minute's sliceAnon, "
          "which already holds the visible jobs: a double-counting upper bound), max over minutes with a throttle "
          "line, of MemoryHigh: %s" % "; ".join(
              "%s %.1f: %.3f" % (n, f, plus_anon(step(jobs, s_, lambda j: j.peak), ceil_series, mlo, hi) / MEMORY_HIGH)
              for n, f, s_ in tl))
    P("")

    # ---- part 4: the 5% trigger
    P("## 4. Discounted jobs against their charge")
    P("4a. Proxy population: every pin row in the window (eligibility NOT established: a row cannot tell a")
    P("discountable grant from a delegate parent, an exclusive job or a restart survivor).")
    for fac in (0.9, 0.8):
        above, unknown, n = above_charge(pins, fac)
        clean = [j for j in pins if not j.oom]
        above_clean, unknown_clean, nc = above_charge(clean, fac)
        atpin = sum(1 for j in pins if j.known and j.peak >= j.pin)
        P("factor %.1f: peak > charge between %d and %d of %d = [%s, %s] (%d peaks unknown); OOM rows excluded "
          "[%s, %s] of %d; peak >= pin %d = %s (OOM rows among them %d)" % (
              fac, above, above + unknown, n, pct(above / n), pct((above + unknown) / n), unknown,
              pct(above_clean / nc), pct((above_clean + unknown_clean) / nc), nc,
              atpin, pct(atpin / len(pins)), sum(1 for j in pins if j.known and j.peak >= j.pin and j.oom)))
    if obs:
        o_lo = min(o[0] for o in obs)
        n = len(obs)
        a9 = attribute(obs, jobs, lambda pin: charge_for(pin, 0.9))
        unmatched_rows = [j for j in pins if j.start >= o_lo - TOL and j.idx not in a9["rows_used"]]
        P("4b. Eligible population: the %d discountable grants the daemon logged (observe lines, from %s). A row" %
          (n, iso(o_lo)))
        P("carries no scope id, so a grant's row is a guess: a pinned row of the same pin started within %d ms of the "
          "line (measured offsets -39..+8 ms). Grants and rows linked that way form components; a one-to-one component "
          "is known, a component whose rows and grants do not pair one to one contributes an interval, and a grant "
          "with no row (failed, killed, still running at the copy, or its row evicted by the 20-row retention) is "
          "unknown, and so is a row with no peak (peak_rss NULL). Paired %d (%d in one-to-one components with a known "
          "peak); %d grants with no row; %d linked rows with an unknown peak; %d pinned rows started since have no "
          "line (not discountable, or a line more than %d ms away)." % (
              TOL, a9["paired"], a9["single"], a9["rowless"], a9["unknown_peak"], len(unmatched_rows), TOL))
        for fac in (0.9, 0.8):
            a = attribute(obs, jobs, lambda pin: charge_for(pin, fac))
            P("factor %.1f: above the charge between %d and %d of %d grants = [%s, %s]: trigger %s; "
              "among the %d one-to-one pairs %d = %s" % (
                  fac, a["low"], a["high"], n, pct(a["low"] / n), pct(a["high"] / n),
                  trigger_verdict(a["low"], a["high"] - a["low"], n), a["single"], a["single_above"],
                  pct(a["single_above"] / a["single"]) if a["single"] else "n/a"))
        P("the trigger counts discounted grants; a whole-window figure on this population does not exist (observe "
          "began at %s)" % iso(o_lo))
    by_hour = collections.defaultdict(lambda: [0, 0, 0])
    for j in pins:
        if not j.known:
            continue                           # unknown peak: in neither count
        h = by_hour[j.start // 3600000]
        h[0] += 1
        h[1] += j.peak > charge_for(j.pin, 0.9)
        h[2] += j.peak > charge_for(j.pin, 0.8)
    r9 = [v[1] / v[0] for v in by_hour.values() if v[0] >= 20]
    r8 = [v[2] / v[0] for v in by_hour.values() if v[0] >= 20]
    P("per clock hour with >= 20 pin rows with a known peak (%d hours): above-charge share at 0.9 median %s max %s; at 0.8 median %s max %s" % (
        len(r9), pct(statistics.median(r9)), pct(max(r9)), pct(statistics.median(r8)), pct(max(r8))))
    ratio = sorted(j.peak / j.pin for j in pins if j.pin > 0 and j.known)
    P("pin rows peak/pin: p50 %.2f p90 %.2f p95 %.2f p99 %.2f" % (q(ratio, .5), q(ratio, .9), q(ratio, .95), q(ratio, .99)))
    P("")

    # ---- part 5: first-order counterfactual on the REAL slice
    if ceil_series:
        first_order(jobs, ceil_series, mlo, hi, P, bh)
    return best


def first_order(jobs, ceil_series, lo, hi, P, busiest=None):
    """Minute by minute, on the observed state: the booking the discount frees from the visible
    running pin jobs, filled from the observed queue in arrival order (pins at their charge),
    and the peaks of the jobs it admits added to the REAL slice anon of that minute (all jobs,
    hidden ones included). A first-order estimate only, NOT a bound: it keeps the observed
    running set, and a discount changes which jobs overlap (two jobs that ran one after the
    other may run together), which no per-minute addition to the observed level can see.
    busiest fixes the hour reported (the report's one hour); None picks the highest mean
    sliceAnon among hours with >= 30 lines. A queued job with an unknown peak adds 0."""
    P("## 5. First-order estimate on the real slice (journal sliceAnon, every minute): NOT a bound")
    P("est = sliceAnon + peaks of the queued jobs the freed booking admits at that minute. It keeps the observed")
    P("running set, so it cannot see a discount making jobs overlap that ran apart: it can understate.")
    Lmax = max(c[1] for c in ceil_series if lo <= c[0] <= hi) - HEADROOM_BASE - HEADROOM_PER_JOB
    for fac in (1.0, 0.9, 0.8):
        P("conditional sum-of-caps bound, factor %.1f (needs no timeline): at a grant sum(charge) <= L, so with every "
          "running job at its full cap at once sum of caps <= L / f; at the window's largest ledger ceiling "
          "L = %s GiB that is %s GiB = %.3f of MemoryHigh (MemoryMax 64 GiB = 1.067); at the static ceiling "
          "(throttle lifted, L = 61.9 GiB) %s GiB. Valid only while L stays there; spec exceptions apply" % (
              fac, fmt_g(Lmax), fmt_g(Lmax / fac), Lmax / fac / MEMORY_HIGH,
              fmt_g((64 * GIB - HEADROOM_BASE - HEADROOM_PER_JOB) / fac)))
        # host terms (report 6.1): throttled, the ceiling is sliceAnon + MemAvailable - freeMin, so with
        # every job at its cap MemAvailable falls to freeMin + headroom - L (1/f - 1)
        P("host terms, factor %.1f: extra caps beyond factor 1.0 at L = %s GiB: %s GiB; worst-case MemAvailable "
          "freeMin + headroom - extra = %s GiB (watchdog trip 8 GiB)" % (
              fac, fmt_g(Lmax), fmt_g(Lmax * (1 / fac - 1)),
              fmt_g(FREE_MIN + HEADROOM_BASE + HEADROOM_PER_JOB - Lmax * (1 / fac - 1))))
    rows = []
    bh = busiest if busiest is not None else busiest_anon_hour(ceil_series, lo, hi)
    for fac in (0.9, 0.8):
        est, admitted_min, contended = [], 0, 0
        hours = collections.defaultdict(list)
        for t, eff, anon in ceil_series:
            if not (lo <= t <= hi):
                continue
            running = [j for j in jobs if j.start <= t < j.start + j.dur]
            waiting = sorted((j for j in jobs if j.arrival <= t < j.start and j.start - j.arrival >= 1000),
                             key=lambda j: (j.arrival, j.idx))
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
            est.append(e)
            hours[t // 3600000].append((anon, e))
        bk = bh // 3600000 if bh is not None else max(hours, key=lambda h: len(hours[h]))
        hb = hours[bk]
        P("factor %.1f: contended minutes %d, with >= 1 waiter admitted at once %d (%s); "
          "est max %.3f of MemoryHigh; minutes over 54 GiB: est %d of %d; "
          "busiest hour %sZ (by sliceAnon): est mean %.3f max %.3f" % (
              fac, contended, admitted_min, pct(admitted_min / contended if contended else 0),
              max(est) / MEMORY_HIGH, sum(1 for x in est if x > SAFE), len(est), iso(bk * 3600000)[:13],
              sum(x[1] for x in hb) / len(hb) / MEMORY_HIGH, max(x[1] for x in hb) / MEMORY_HIGH))
        rows.append((fac, max(est)))
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
    # the window's partial first and last minutes are clipped to [lo, hi): a 55 GiB job at
    # 0..10 s is before a window opening at 30 s, and one at 100..110 s is after a window
    # closing at 90 s, so neither minute (0, 60 s) reaches 54 GiB; inside the window they count
    clip = [Job(0, 0, 0, 10000, 55 * G, 60 * G, "x", 0), Job(1, 100000, 100000, 10000, 55 * G, 60 * G, "x", 0)]
    cst = step(clip, {0: 0, 1: 100000}, lambda j: j.peak)
    check("minute max: clipped to [lo, hi) at both partial minutes", minute_max(cst, 30000, 90000), {0: 0, 60000: 0})
    check("minute max: the same jobs inside the window count", minute_max(cst, 5000, 105000), {0: 55 * G, 60000: 55 * G})
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
    # 0.9 GiB, peak 0.5 GiB) is admitted, a second one is not, and FIFO stops there, so a
    # tiny third one is not admitted either; a running 10 GiB AUTO job frees nothing; anon
    # 5 GiB on the minute
    fo = [Job(0, 0, 0, 200000, 2 * G, 10 * G, PIN_BASIS, 0), Job(1, 10000, 150000, 1000, G // 2, G, PIN_BASIS, 0),
          Job(2, 20000, 150000, 1000, G // 2, G, PIN_BASIS, 0), Job(3, 30000, 150000, 1000, 64 * MIB, 64 * MIB, "x", 0),
          Job(4, 0, 0, 200000, G, 10 * G, "cap:auto:daemon-reserve", 0)]
    rows = first_order(fo, [(60000, 30 * G, 5 * G)], 0, 200000, lambda *a: None)
    check("first order est at 0.9: anon + one admitted peak", rows[0][1], 5 * G + G // 2)
    # 12. the 54 GiB line, positive and negative: a 55 GiB job over two minutes, an empty
    # minute, then a 53.9 GiB job (under the line); the window ends at the last completion
    big = [Job(0, 0, 0, 120000, 55 * G, 60 * G, "x", 0), Job(1, 180000, 180000, 60000, int(53.9 * G), 60 * G, "x", 0)]
    mv = memory_view(big, {0: 0, 1: 180000}, 0)
    check("over 54 GiB: two minutes of 55 GiB count, 53.9 GiB does not", mv["minutes_over_safe"], 2)
    check("minute max is the largest minute (an empty minute is 0)", mv["minute_max"], 55 * G)
    check("window runs to the final completion", mv["minutes"], 4)
    check("the line is 0.9 x 60 GiB: 54.5 GiB is over it",
          memory_view([Job(0, 0, 0, 60000, int(54.5 * G), 60 * G, "x", 0)], {0: 0}, 0)["minutes_over_safe"], 1)
    # 13. busiest hour: the hour with the LARGER duration-weighted mean, wherever it falls
    hrs = [Job(0, 0, 0, 1800000, 2 * G, 4 * G, "x", 0), Job(1, 3600000, 3600000, 1800000, 5 * G, 8 * G, "x", 0),
           Job(2, 7200000, 7200000, 1800000, 3 * G, 4 * G, "x", 0)]
    mv = memory_view(hrs, {0: 0, 1: 3600000, 2: 7200000}, 0)
    check("busiest hour is the second (5 GiB), not the first or last", (mv["busiest_hour"], mv["hour_max"]),
          ("1970-01-01T01Z", 5 * G))
    # 14. a replay that delays a job past the OBSERVED final completion still sees its peak:
    # observed, both start at 0 and the last ends at 100 s; replayed at 1.0 on 10 GiB, the
    # 55 GiB job waits for the first and runs 100..160 s
    dl = [Job(0, 0, 0, 100000, G, 6 * G, "x", 0), Job(1, 0, 0, 60000, 55 * G, 6 * G, "x", 0)]
    base = simulate(dl, c10, 1.0, "fifo")
    r = counterfactual(dl, c10, "fifo", 0, 1.0, base, dl, 0, 100000)
    check("delayed job: its minutes over 54 GiB are counted", (r["mem"]["minutes_over_safe"], r["mem"]["minutes"]), (2, 3))
    check("delayed job: the common window mean stays on the observed span", r["mem"]["common_mean"], G)
    # 15. counterfactual row: two 6 GiB pins (peak 3 GiB) arrive at 0; the second REALLY
    # queued (observed start 100 s); a third, unrelated job never moves. At 0.8 both fit
    # (2 x 4.8 GiB): one job earlier than its baseline, and earlier than observed; the
    # memory view is of the 0.8 timeline (6 GiB overlap), not the baseline (3 GiB)
    cf = [Job(0, 0, 0, 100000, 3 * G, 6 * G, PIN_BASIS, 0), Job(1, 0, 100000, 100000, 3 * G, 6 * G, PIN_BASIS, 0),
          Job(2, 300000, 300000, 1000, G, G, "x", 0)]
    base = simulate(cf, c10, 1.0, "bypass")
    r = counterfactual(cf, c10, "bypass", 0, 0.8, base, cf, 0, 301000)
    check("counterfactual: earlier, later, earlier among queued, earlier/later than observed",
          (r["earlier"], r["later"], r["earlier_queued"], r["obs_earlier"], r["obs_later"]), (1, 0, 1, 1, 0))
    check("counterfactual: the memory view is the factor's own timeline", r["mem"]["minute_max"], 6 * G)
    r = counterfactual(cf, c10, "bypass", 0, 1.0, base, cf, 0, 301000)
    check("counterfactual at 1.0: nothing moves, memory 3 GiB", (r["earlier"], r["obs_earlier"], r["mem"]["minute_max"]),
          (0, 0, 3 * G))
    # later than observed, split by whether the job really queued: three 6 GiB jobs arrive at 0
    # on a 10 GiB ledger (fifo, 1.0), so they run one after another (0, 100, 200 s). Job 1 never
    # queued (observed start 0), job 2 did (observed start 50 s): both are later than observed,
    # and only job 2 counts among the later ones that really queued
    lq = [Job(0, 0, 0, 100000, G, 6 * G, "x", 0), Job(1, 0, 0, 100000, G, 6 * G, "x", 0),
          Job(2, 0, 50000, 100000, G, 6 * G, "x", 0)]
    rq = counterfactual(lq, c10, "fifo", 0, 1.0, simulate(lq, c10, 1.0, "fifo"), lq, 0, 300000)
    check("counterfactual: later than observed, and the later ones that really queued",
          (rq["obs_later"], rq["obs_later_queued"]), (2, 1))
    # 16. hidden load alone makes the second job wait: 4 + 4 booked + 4 hidden > 10 GiB
    hl = [Job(0, 0, 0, 100000, G, 4 * G, "x", 0), Job(1, 0, 0, 100000, G, 4 * G, "x", 0)]
    check("hidden load holds the second job", (simulate(hl, c10, 1.0, "fifo", "blind", 4 * G)[1],
                                               simulate(hl, c10, 1.0, "fifo", "blind", 0)[1]), (100000, 0))
    # 17. the 5% trigger arithmetic: 10 GiB pins, charge 9 GiB at 0.9 and 8 GiB at 0.8
    tp = [Job(i, 0, 0, 1000, pk, 10 * G, PIN_BASIS, 0) for i, pk in
          enumerate((int(9.5 * G), int(8.5 * G), G, 10 * G))]
    check("above charge at 0.9 and 0.8 (against the charge, not the pin)",
          (above_charge(tp, 0.9), above_charge(tp, 0.8)), ((2, 0, 4), (3, 0, 4)))
    # an unknown peak (peak_rss NULL) is neither above nor below: it widens the interval
    tpn = tp + [Job(4, 0, 0, 1000, None, 10 * G, PIN_BASIS, 0)]
    check("above charge: an unknown peak is counted unknown, not as a 0-byte peak", above_charge(tpn, 0.9), (2, 1, 5))
    check("trigger: exactly 5% is not over; 2/39 is; 500/9992 is (no rounding); interval straddling; no rows",
          (trigger_verdict(1, 0, 20), trigger_verdict(2, 0, 39), trigger_verdict(500, 0, 9992),
           trigger_verdict(1, 2, 20), trigger_verdict(0, 0, 0)),
          ("not met", "met", "met", "unevaluated", "unevaluated"))
    # 18. attribution: same pin, start within TOL (100 ms), pinned rows only, one to one
    lines = [(1000, 6 * G), (1000, 6 * G), (5000, 2 * G), (9000, 6 * G)]
    rws = [Job(0, 0, 1001, 1, 0, 6 * G, PIN_BASIS, 0), Job(1, 0, 1090, 1, 0, 6 * G, PIN_BASIS, 0),
           Job(2, 0, 5000, 1, 0, 3 * G, PIN_BASIS, 0), Job(3, 0, 9150, 1, 0, 6 * G, PIN_BASIS, 0),
           Job(4, 0, 9000, 1, 0, 6 * G, "cap:auto:daemon-reserve", 0)]
    att = attribute(lines, rws, lambda pin: charge_for(pin, 0.9))
    check("attribution: two pairs, no pin mismatch, nothing past 100 ms, no auto row",
          (att["paired"], att["rowless"], sorted(att["rows_used"])), (2, 2, [0, 1]))
    ch = lambda pin: charge_for(pin, 0.9)
    lo_ = lambda i: Job(i, 0, 0, 1, G, 10 * G, PIN_BASIS, 0)

    def pop(above, rows_at, extra=()):
        """40 grants of 10 GiB 10 s apart; the row of grant t (if t in rows_at) starts 1 ms
        after it, peaking 9.5 GiB (above the 9 GiB charge) for the first `above` of them."""
        gl = [(10000 * t, 10 * G) for t in range(40)]
        js = []
        for n, t in enumerate(rows_at):
            js.append(Job(n, 0, 10000 * t + 1, 1, int(9.5 * G) if n < above else G, 10 * G, PIN_BASIS, 0))
        for st, pk in extra:
            js.append(Job(len(js), 0, st, 1, pk, 10 * G, PIN_BASIS, 0))
        a = attribute(gl, js, ch)
        return a["low"], a["high"], trigger_verdict(a["low"], a["high"] - a["low"], 40)
    check("attribution: 3 of 40 above, all one to one: met", pop(3, range(40)), (3, 3, "met"))
    # review P2-1: 3 of 40 above, the third's peak is NULL: [5%, 7.5%] unevaluated, never 2 of 40
    # = exactly 5% 'not met' (a NULL read as 0 is a known-safe sample)
    gl40 = [(10000 * t, 10 * G) for t in range(40)]
    jn = [Job(n, 0, 10000 * n + 1, 1, int(9.5 * G) if n < 2 else (None if n == 2 else G), 10 * G, PIN_BASIS, 0)
          for n in range(40)]
    an = attribute(gl40, jn, ch)
    check("attribution: a NULL peak keeps the grant unknown: [2, 3] of 40, unevaluated",
          (an["low"], an["high"], an["unknown_peak"], an["single"], trigger_verdict(an["low"], an["high"] - an["low"], 40)),
          (2, 3, 1, 39, "unevaluated"))
    # in a burst: two grants, rows one known below and one unknown: between 0 and 1 above
    bu = attribute([(0, 10 * G)] * 2, [lo_(0), Job(1, 0, 1, 1, None, 10 * G, PIN_BASIS, 0)], ch)
    check("attribution: an unknown row in a burst may be either side", (bu["low"], bu["high"]), (0, 1))
    # ... and a burst of one known above and one unknown: at least 1, at most 2
    ba = attribute([(0, 10 * G)] * 2, [Job(0, 0, 1, 1, int(9.5 * G), 10 * G, PIN_BASIS, 0),
                                       Job(1, 0, 1, 1, None, 10 * G, PIN_BASIS, 0)], ch)
    check("attribution: a known-above row stays above beside an unknown one", (ba["low"], ba["high"]), (1, 2))
    # review counterexample 1: two known above, the third above-charge job has no row, and
    # an INELIGIBLE low-peak row starts 300 ms from its grant: it must not stand in for the
    # missing row (the old 0.5 s pairing read 2 of 40 = 'not met' while the truth is 7.5%)
    check("attribution: a nearby ineligible row does not hide a rowless grant",
          pop(2, [t for t in range(40) if t != 39], [(10000 * 39 + 300, G)]), (2, 3, "unevaluated"))
    # review counterexample 2: 2 of 40 above (exactly 5%), and an ineligible HIGH-peak row
    # nearer to a low-peak grant than its own row: the old nearest pairing read 3 of 40 'met'
    js2 = [Job(n, 0, 10000 * t + (5 if t == 39 else 1), 1, int(9.5 * G) if n < 2 else G, 10 * G, PIN_BASIS, 0)
           for n, t in enumerate(range(40))]
    js2.append(Job(40, 0, 10000 * 39 + 1, 1, int(9.9 * G), 10 * G, PIN_BASIS, 0))
    a2 = attribute([(10000 * t, 10 * G) for t in range(40)], js2, ch)
    check("attribution: a row that disagrees with the grant's other candidate keeps it unknown",
          (a2["low"], a2["high"], a2["single"], trigger_verdict(a2["low"], a2["high"] - a2["low"], 40)),
          (2, 3, 39, "unevaluated"))
    # a burst: three grants of one pin in the same ms and three rows 1-3 ms later, all below:
    # every grant is known below (not thrown to unknown); with only two rows, one above, the
    # burst holds between 1 and 2 above
    burst = [(0, 10 * G)] * 3
    b3 = attribute(burst, [lo_(0), lo_(1), lo_(2)], ch)
    check("attribution: an all-below burst is known", (b3["paired"], b3["low"], b3["high"], b3["single"]), (3, 0, 0, 0))
    b2 = attribute(burst, [lo_(0), Job(1, 0, 2, 1, int(9.5 * G), 10 * G, PIN_BASIS, 0)], ch)
    check("attribution: a burst with a missing row is an interval", (b2["paired"], b2["low"], b2["high"]), (2, 1, 2))
    # load_jobs and load_journal on tiny files under ~/tmp: a NULL peak_rss stays unknown (not
    # 0), and systemd's daemon start lines are counted
    import shutil
    import tempfile
    base = os.path.expanduser("~/tmp")
    os.makedirs(base, exist_ok=True)
    td = tempfile.mkdtemp(prefix="aira-324-selftest-", dir=base)
    try:
        dbp = os.path.join(td, "s.db")
        db = sqlite3.connect(dbp)
        db.execute("create table confine_peak_history (kind text, started_at_ms integer, wait_ms integer, wall_ms integer, "
                   "peak_rss integer, budget integer, budget_basis text, oom integer, at text)")
        db.executemany("insert into confine_peak_history values ('confine', ?, 0, 1000, ?, ?, ?, 0, 'a')",
                       [(1000, None, 10 * G, PIN_BASIS), (2000, 5 * G, 10 * G, PIN_BASIS)])
        db.commit()
        db.close()
        lj, _ = load_jobs(dbp)
        check("load_jobs: a NULL peak_rss is unknown, a real one known",
              [(j.known, j.peak) for j in lj], [(False, 0), (True, 5 * G)])
        jp = os.path.join(td, "j.txt")
        with open(jp, "w") as f:
            f.write("1000.5 host systemd[1]: Started aira-daemon.service - AIRA daemon\n"
                    "2000.0 host systemd[1]: Stopped aira-daemon.service - AIRA daemon\n"
                    "3000.0 host aira[2]: aira daemon: slice ceiling throttled (enforce): 5 effective / 9 configured, "
                    "sliceAnon=3 MemAvailable=1\n")
        jc, jo, js = load_journal(jp)
        check("load_journal: daemon starts counted, ceiling lines read", (js, jc, jo), ([1000500], [(3000000, 5, 3)], []))
    finally:
        shutil.rmtree(td, ignore_errors=True)
    # the unknown-peak bound: a value function replaces the peak (the default counts it as 0)
    nu = [Job(0, 0, 0, 60000, None, 7 * G, "x", 0)]
    check("memory view: an unknown peak is 0 by default, its pin under the bound",
          (memory_view(nu, {0: 0}, 0)["minute_max"], memory_view(nu, {0: 0}, 0, value=lambda j: j.pin)["minute_max"]),
          (0, 7 * G))
    # review P2-2 counterexample: a THROTTLED 61 GiB ceiling (below the machine term) admits ten
    # idle 6.45 GiB pins at 0.9 (charges 58.05 GiB + headroom 2.625 GiB <= 61): their caps sum
    # to 64.5 GiB, over MemoryMax (64 GiB), so a slice-level OOM is reachable while throttled
    tenp = [Job(i, 0, 0, 100000, 0, int(6.45 * G), PIN_BASIS, 0) for i in range(10)]
    st10 = simulate(tenp, Ceiling(const=61 * G), 0.9, "fifo", "blind")
    check("61 GiB throttled ceiling: all ten 6.45 GiB pins start at once at 0.9, caps over 64 GiB",
          (sorted(set(st10.values())), sum(j.pin for j in tenp) > 64 * G), ([0], True))
    # the busiest hour is fixed once by sliceAnon, among hours with >= 30 lines: hour 0 has 40
    # lines at 10 GiB, hour 1 has 20 lines at 30 GiB (too short), hour 2 has 30 lines at 5 GiB
    cs3 = ([(60000 * i, 50 * G, 10 * G) for i in range(40)] + [(3600000 + 60000 * i, 50 * G, 30 * G) for i in range(20)]
           + [(7200000 + 60000 * i, 50 * G, 5 * G) for i in range(30)])
    check("busiest hour by sliceAnon: the highest mean among hours >= 30 min long",
          (busiest_anon_hour(cs3, 0, 10800000), busiest_anon_hour(cs3, 0, 10800000, min_lines=20)), (0, 3600000))
    hv = [Job(0, 0, 0, 600000, 2 * G, 4 * G, "x", 0), Job(1, 3600000, 3600000, 600000, 9 * G, 10 * G, "x", 0)]
    check("memory view: a fixed busiest hour is reported, not the timeline's own",
          (memory_view(hv, {0: 0, 1: 3600000}, 0, 7200000, None, 0)["busiest_hour"],
           memory_view(hv, {0: 0, 1: 3600000}, 0, 7200000)["busiest_hour"]), ("1970-01-01T00Z", "1970-01-01T01Z"))
    # visible + real slice: minute 0 has a line (anon 5 GiB) and the replay at 3 GiB; minute 1
    # has no line, so the last one is held (5 GiB), and the replay is at 4 GiB there
    pa = [Job(0, 0, 0, 60000, 3 * G, 4 * G, "x", 0), Job(1, 60000, 60000, 60000, 4 * G, 4 * G, "x", 0)]
    check("visible + sliceAnon: the last line's anon is held into a minute without one",
          plus_anon(step(pa, {0: 0, 1: 60000}, lambda j: j.peak), [(1000, 50 * G, 5 * G)], 0, 120000), 9 * G)
    # review round 7: anon 50 GiB from 1 s, 0 from 90 s; the replay holds 10 GiB over 60-80 s.
    # Over 60-90 s the held 50 GiB and the replay's 10 GiB coexist: 60 GiB, over SAFE (54 GiB).
    # Taking only minute 1's own line (0) would report 50 GiB, under SAFE.
    pc = [Job(0, 60000, 60000, 20000, 10 * G, 10 * G, "x", 0)]
    got = plus_anon(step(pc, {0: 60000}, lambda j: j.peak), [(1000, 50 * G, 50 * G), (90000, 50 * G, 0)], 0, 120000)
    check("visible + sliceAnon: a minute with a line still counts the reading held at its start",
          (got, got > SAFE), (60 * G, True))
    check("queued means an observed wait of at least 1 s",
          (queued(Job(0, 0, 999, 1, 0, G, "x", 0)), queued(Job(0, 0, 1000, 1, 0, G, "x", 0))), (False, True))
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
    ceil, obs, starts = load_journal(a.journal) if a.journal else ([], [], [])
    report(jobs, extra, ceil, obs, sys.stdout, a.slice_peak, starts)
    return 0


if __name__ == "__main__":
    sys.exit(main())
