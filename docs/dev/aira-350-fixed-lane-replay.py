#!/usr/bin/env python3
"""Read-only replay of the fixed small-job lane (AIRA-350,
docs/superpowers/specs/2026-11-18-fixed-small-lane-design.md, section 6).

Question: had a separately booked lane of fixed size x (jobs whose reserve is at most y,
lane first, overflow to the main pool) existed, how much small-job wait would it have
removed, how full would it have been, and how much of it would long-lived jobs hold?

Inputs (read-only; never point --db at the live file):

  sqlite3 ~/.local/state/aira/state.db ".backup $HOME/tmp/aira-lane2-store/state-copy.db"
  aira confine --list --json > ~/tmp/aira-lane2-store/snapshot.json      # optional
  aira confine -- python3 -I docs/dev/aira-350-fixed-lane-replay.py \\
      --db ~/tmp/aira-lane2-store/state-copy.db [--snapshot ~/tmp/aira-lane2-store/snapshot.json]

The snapshot holds other sessions' job names and commands, so it is not committed; this
script prints only aggregates from it.

Model (one row of confine_peak_history = one finished confine job):
  arrival a = started_at_ms - wait_ms; historical grant g = started_at_ms; hold = wall_ms;
  lane footprint f = budget + 64 MiB (the per-job supervisor headroom, admit.go
  admitSliceHeadroomSupervisorDefault). A job is ELIGIBLE when budget <= y and its
  signature does not start with the --delegate-ram parent prefix "aitest-parent\\0"
  (runner.AitestParentSignaturePrefix); the "+parents" variant drops that exclusion.
  Every eligible job is tried in the lane first, at every instant from a to g, in arrival
  order, first fit (a job that does not fit is skipped and a later smaller one may fit).
  Admitted at t: it runs in the lane over [t, t + hold] and its wait becomes t - a.
  Not admitted by g: it runs in the main pool at g as it historically did, and holds no
  lane room. Ineligible jobs are untouched.

Sections
  0   population: small (reserve <= y) and large rows, wait-hours; peak/reserve and run time.
  1   the replay at each x, parents excluded (the spec's eligibility) and included.
  1b  the same with a constant lane occupancy held throughout (--silt), standing in for the
      long-lived jobs (servers) the store cannot see because they have not ended.
  1c  the cut alternative "main pool first": a job granted within 1 s never enters the lane.
  1d  per UTC day, to show how the retention cap thins the earlier days.
  1e  CPU-slot sensitivity: a lane grant is also refused while the retained jobs running at
      that instant (each holding one slot), plus an ALLOWANCE for jobs the store does not
      hold (aitest workers have no timing, capped signatures lost rows, servers have not
      ended), would fill the slot ceiling. Allowance 0 is the retained rows alone.
  1f  the partition's cost to the main pool: main's booking room is x smaller at every
      instant, less the bookings of the jobs the lane moved out of main at that instant.
  1g  the largest retained reserves, against the main pool's room under the partition.
  1h  spec round 3's derived accounting: every small lease counts against x however it was
      granted, so freed room goes first to overflowed small jobs and small suite parents.
      1h' leaves suite parents out (round 3's rule: a lease with a live sub-reservation is
      not small; this assumes a parent has a live worker for its whole run).
  1i  spec round 4 (--journal): the lane INSIDE the one ceiling, with admission priority.
      Footprint = booking (budget; the window ran pin-charge observe, so booking ==
      reserve), x caps bookings only; S counts every small job-level lease however granted
      (1h's accounting). Two gates: "none" (a lane grant whenever S has room: the UPPER
      end, since it assumes the one ceiling always had room) and "holds" (a lane grant only
      inside a fairness hold read from the journal, the one state in which today's
      evaluator withholds a small job that FITS; the arm proves room for one withheld
      job, not for every one the replay admits). Then CPU allowances on "holds", the
      incident window, and how often guard G would have bound against the head each hold
      line names, at the ceiling sampled within 120 s before it. Run it as
        aira confine -- python3 -I docs/dev/aira-350-fixed-lane-replay.py --db ... \\
            --journal ~/tmp/aira-lane2-store/journal-r3.txt
      (the journal capture is the one aira-350-ceiling-drop.py documents).
  2   the 2026-11-18 incident window.
  3   aggregates from a live `aira confine --list --json` snapshot, if given.

Every figure is a SIMULATED ESTIMATE, not a bound: first fit in arrival order is not
monotone (removing a gate can admit a long-lived job that later displaces a long-waiting
one; section 1c's main-first variant saves more than lane-first with fewer lane jobs).
Known biases, by direction:
  UP (the saving is overstated):
  - no CPU slot ceiling in sections 1-1d (section 1e varies it), no physical-room gate, and
    no reservation of the head's CPU and VRAM;
  - no ceiling series: a lane gated on today's whole-slice room W (round 2's partition) is
    shut while the ceiling has fallen below what main already booked; the journal measures
    how often the ceiling falls (docs/dev/aira-350-ceiling-drop.py), not this closure;
  - the 20-row retention cap thins frequent commands, so lane occupancy is understated and
    the lane is full less often than it would be;
  - jobs still running at the copy (servers) are absent (section 1b adds them back as a
    constant);
  DOWN (the saving is understated):
  - secondary effects: jobs leaving the main pool for the lane free main room for others.
  NOT MODELLED: the partition's cost to large and overflow jobs (1f sizes the room lost,
  not the wait it causes), so every saving here is GROSS.
Other limits:
  - --snapshot reports what is running now (servers absent from the store);
  - the store keeps 20 rows per (kind, signature) (internal/store/confine_peak_history.go:13),
    so frequent commands lost rows: the population is the retained sample;
  - pytest-worker rows carry no timing and are sub-reservations (never lane-eligible);
  - budget is the scope cap, the reserve for an ordinary confine job; pin-charge is in
    observe mode, so booking == reserve at grant.
"""
import argparse
import bisect
import heapq
import json
import re
import sqlite3
import statistics
from datetime import datetime, timedelta, timezone

GIB = 1 << 30
MIB = 1 << 20
PER_JOB = 64 * MIB
PARENT_PREFIX = "aitest-parent\x00"
IMMEDIATE_MS = 1000  # a grant within 1 s of arrival counts as immediate
UTC = timezone.utc


def fmt_t(ms):
    return datetime.fromtimestamp(ms / 1000, UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def pct(values, q):
    if not values:
        return float("nan")
    values = sorted(values)
    k = max(0, min(len(values) - 1, int(round(q / 100.0 * (len(values) - 1)))))
    return values[k]


def load(db):
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    rows = con.execute(
        "SELECT budget, wait_ms, started_at_ms, wall_ms, peak_rss, signature FROM confine_peak_history "
        "WHERE kind='confine' AND wait_ms IS NOT NULL AND started_at_ms IS NOT NULL "
        "AND wall_ms IS NOT NULL AND budget IS NOT NULL").fetchall()
    lost_sigs = con.execute(
        "SELECT count(*) FROM (SELECT signature, count(*) c, min(started_at_ms) m FROM confine_peak_history "
        "WHERE kind='confine' GROUP BY signature HAVING c >= 20 AND m >= "
        "(SELECT min(started_at_ms) FROM confine_peak_history WHERE wait_ms IS NOT NULL))").fetchone()[0]
    no_timing = con.execute(
        "SELECT kind, count(*) FROM confine_peak_history WHERE at >= "
        "(SELECT min(at) FROM confine_peak_history WHERE wait_ms IS NOT NULL) "
        "AND (wait_ms IS NULL OR started_at_ms IS NULL OR wall_ms IS NULL) GROUP BY kind").fetchall()
    con.close()
    jobs = []
    for budget, wait, start, wall, peak, sig in rows:
        jobs.append(dict(budget=budget, wait=max(0, wait), g=start, a=start - max(0, wait),
                         hold=max(0, wall), peak=peak or 0, parent=sig.startswith(PARENT_PREFIX)))
    return jobs, lost_sigs, no_timing


def simulate(jobs, x, y, include_parents, silt=0, main_first=False, slots=None, allowance=0, derived=False,
             extra=PER_JOB, gate=None):
    """Event-driven lane replay. Returns per-run metrics.

    silt: a constant lane occupancy held throughout (long-lived jobs absent from the store).
    main_first: the cut alternative; a job the main pool granted within 1 s never enters the lane.
    slots: if set, a lane grant also needs a CPU slot: the retained jobs running at that
    instant (lane-moved jobs at their lane times, every other job at its historical times,
    one slot each) plus `allowance` plus the candidate must not exceed `slots`.
    derived: spec round 3's derived accounting. Lane occupancy S counts EVERY small lease
    (reserve <= y, parentless), however it was granted: an eligible job that overflowed to
    main counts from its historical grant g to its end, and so does every small suite
    parent (not lane-eligible, granted in main at g). Freed lane room therefore goes to
    those leases first, not to new waiters.
    extra: the per-job addition to a lane footprint (64 MiB in rounds 1-3; 0 in round 4,
    where x caps bookings and the per-job headroom stays in the one ceiling).
    gate: if set, sorted non-overlapping (start, end) ms intervals; a lane grant is made only
    at an instant inside one (round 4's hold-gated variant: the fairness holds, the only
    state in which today's evaluator withholds a waiter that fits)."""
    elig = [j for j in jobs if j["budget"] <= y and (include_parents or not j["parent"])
            and not (main_first and j["wait"] < IMMEDIATE_MS)]
    elig.sort(key=lambda j: (j["a"], j["g"]))
    for idx, j in enumerate(elig):
        j["_id"] = idx
        j["_f"] = j["budget"] + extra
    gate_starts = [s for s, _ in gate] if gate is not None else []
    gate_ends = [e for _, e in gate] if gate is not None else []

    def in_gate(t):
        k = bisect.bisect_right(gate_starts, t) - 1
        return k >= 0 and t < gate_ends[k]
    # events: (time, order, id); order 0 release, 1 arrival, 2 slot re-scan, 3 a derived
    # occupancy change ext[id]; deadlines are checked after the scan
    events = []
    ext = []
    for j in elig:
        heapq.heappush(events, (j["a"], 1, j["_id"]))
    if derived and derived != "overflow-only" and not include_parents:
        for j in jobs:
            if j["budget"] <= y and j["parent"]:
                f = j["budget"] + extra
                ext.append(f)
                heapq.heappush(events, (j["g"], 3, len(ext) - 1))
                ext.append(-f)
                heapq.heappush(events, (j["g"] + j["hold"], 3, len(ext) - 1))
    if slots is not None:
        starts = sorted(j["g"] for j in jobs)
        ends = sorted(j["g"] + j["hold"] for j in jobs)
        for e in ends:  # a historical job ending frees a slot: re-scan then
            heapq.heappush(events, (e, 2, -1))
    for s in gate_starts:  # a hold opening: re-scan then
        heapq.heappush(events, (s, 2, -1))
    adj = 0           # historical intervals vacated by lane-moved jobs
    adj_heap = []
    cpu_denied = set()  # jobs refused a lane grant at least once for want of a slot
    waiting = []  # ids in arrival order
    deadline_heap = []
    occ = silt
    residents = {}
    admitted_at = {}
    # occupancy integration
    last_t = None
    t0 = elig[0]["a"] if elig else 0
    area = 0.0          # GiB*ms of occupancy
    area_long = 0.0     # occupancy held by residents with hold >= 1 h
    area_10m = 0.0
    full_ms = 0         # time a 1 GiB job (y + 64 MiB) would not fit
    blocked_ms = 0      # time an eligible job was waiting and fitted nowhere in the lane
    peak_occ = 0
    peak_real = 0
    real_samples = []   # (duration, real) for a time-weighted p99
    peak_count = 0
    count_samples = []
    occ_samples = []
    segs = []           # (start, duration, occupancy, someone-waiting) for the per-day split

    def account(t):
        nonlocal last_t, area, area_long, area_10m, full_ms, blocked_ms
        if last_t is not None and t > last_t:
            dt = t - last_t
            area += occ * dt
            long_occ = sum(elig[i]["_f"] for i in residents if elig[i]["hold"] >= 3_600_000)
            m10 = sum(elig[i]["_f"] for i in residents if elig[i]["hold"] >= 600_000)
            area_long += long_occ * dt
            area_10m += m10 * dt
            if occ + y + extra > x:
                full_ms += dt
            if waiting:
                blocked_ms += dt
            real = sum(elig[i]["peak"] for i in residents)
            real_samples.append((dt, real))
            count_samples.append((dt, len(residents)))
            occ_samples.append((dt, occ))
            segs.append((last_t, dt, occ, bool(waiting)))
        last_t = t

    while events or deadline_heap:
        t = min(events[0][0] if events else float("inf"), deadline_heap[0][0] if deadline_heap else float("inf"))
        account(t)
        # releases and arrivals at t
        while events and events[0][0] == t:
            _, kind, i = heapq.heappop(events)
            if kind == 2:
                continue
            if kind == 3:
                occ += ext[i]
                continue
            if kind == 0:
                occ -= elig[i]["_f"]
                del residents[i]
            else:
                waiting.append(i)
                heapq.heappush(deadline_heap, (elig[i]["g"], i))
        # first-fit scan in arrival order
        while adj_heap and adj_heap[0][0] <= t:
            adj += heapq.heappop(adj_heap)[1]
        still = []
        scan = waiting if (gate is None or in_gate(t)) else []
        if not scan:
            still = list(waiting)
        for i in scan:
            j = elig[i]
            if occ + j["_f"] <= x:
                if slots is not None:
                    running = bisect.bisect_right(starts, t) - bisect.bisect_right(ends, t) + adj + len(residents)
                    self_counted = 1 if j["g"] <= t < j["g"] + j["hold"] else 0
                    if running - self_counted + allowance + 1 > slots:
                        cpu_denied.add(i)
                        still.append(i)
                        continue
                occ += j["_f"]
                residents[i] = True
                admitted_at[i] = t
                heapq.heappush(events, (t + j["hold"], 0, i))
                if slots is not None and j["hold"] > 0:
                    # its historical interval no longer holds a slot
                    if j["g"] <= t:
                        adj -= 1
                    else:
                        heapq.heappush(adj_heap, (j["g"], -1))
                    heapq.heappush(adj_heap, (j["g"] + j["hold"], 1))
            else:
                still.append(i)
        waiting = still
        peak_occ = max(peak_occ, occ)
        peak_count = max(peak_count, len(residents))
        peak_real = max(peak_real, sum(elig[i]["peak"] for i in residents))
        # deadlines at or before t: still waiting -> main pool at g
        while deadline_heap and deadline_heap[0][0] <= t:
            _, i = heapq.heappop(deadline_heap)
            if i not in admitted_at and i in waiting:
                waiting.remove(i)
                if derived:
                    # granted in main at g, but counted against the lane until it ends
                    f = elig[i]["_f"]
                    occ += f
                    ext.append(-f)
                    heapq.heappush(events, (elig[i]["g"] + elig[i]["hold"], 3, len(ext) - 1))

    span = (last_t - t0) if last_t is not None else 0

    def tw_pct(samples, q):
        tot = sum(d for d, _ in samples)
        if not tot:
            return 0
        acc = 0
        for d, v in sorted(samples, key=lambda s: s[1]):
            acc += d
            if acc >= q / 100.0 * tot:
                return v
        return samples[-1][1]

    waited = [j for j in elig if j["wait"] >= 10_000]
    saved = sum(j["g"] - admitted_at[j["_id"]] for j in elig if j["_id"] in admitted_at)
    full_saves = sum(1 for j in waited if admitted_at.get(j["_id"]) == j["a"])
    part_saves = sum(1 for j in waited if j["_id"] in admitted_at and admitted_at[j["_id"]] != j["a"])
    lane_immediate = sum(1 for j in elig if j["_id"] in admitted_at and j["wait"] < IMMEDIATE_MS)
    overflow = [j for j in elig if j["_id"] not in admitted_at]
    overflow_waited = sum(1 for j in overflow if j["wait"] >= 10_000)
    return dict(
        n=len(elig), wait_h=sum(j["wait"] for j in elig) / 3.6e6, saved_h=saved / 3.6e6,
        admitted=len(admitted_at), lane_immediate=lane_immediate,
        overflow=len(overflow), overflow_waited=overflow_waited, overflow_wait_h=sum(j["wait"] for j in overflow) / 3.6e6,
        waited=len(waited), full_saves=full_saves, part_saves=part_saves,
        mean_occ=area / span / GIB if span else 0, peak_occ=peak_occ / GIB,
        p50_occ=tw_pct(occ_samples, 50) / GIB, p90_occ=tw_pct(occ_samples, 90) / GIB,
        full_share=full_ms / span if span else 0, blocked_share=blocked_ms / span if span else 0,
        long_share=area_long / area if area else 0, m10_share=area_10m / area if area else 0,
        peak_real=peak_real / GIB, p99_real=tw_pct(real_samples, 99) / GIB, p50_real=tw_pct(real_samples, 50) / GIB,
        peak_count=peak_count, p99_count=tw_pct(count_samples, 99), span_h=span / 3.6e6,
        admitted_at=admitted_at, elig=elig, segs=segs, cpu_denied=len(cpu_denied))


def per_day(jobs, xs, y):
    """Per UTC day: retained rows per hour (the 20-row cap drops older rows of frequent
    commands, so earlier days are thinner), and the lane's fill and saving that day."""
    DAY = 86_400_000
    runs = {x: simulate(jobs, x, y, False) for x in xs}
    elig = runs[xs[0]]["elig"]
    days = sorted({j["a"] // DAY for j in elig})
    head = "day        | elig rows/h | small wait-h | " + " | ".join(
        f"x={x / GIB:.0f}: saved-h, full-for-1G, someone-waiting, occ mean" for x in xs)
    lines = [head]
    for d in days:
        lo, hi = d * DAY, (d + 1) * DAY
        rows = [j for j in jobs if lo <= j["a"] < hi and j["budget"] <= y and not j["parent"]]
        span_lo = max(lo, min(j["a"] for j in elig))
        span_hi = min(hi, max(j["g"] for j in elig))
        hours = max(1e-9, (span_hi - span_lo) / 3.6e6)
        parts = []
        for x in xs:
            r = runs[x]
            adm = r["admitted_at"]
            saved = sum(j["g"] - adm[j["_id"]] for j in r["elig"] if lo <= j["a"] < hi and j["_id"] in adm)
            tot = full = blk = area = 0
            for st, dt, occ, w in r["segs"]:
                a, b = max(st, lo), min(st + dt, hi)
                if b <= a:
                    continue
                seg = b - a
                tot += seg
                area += occ * seg
                if occ + y + PER_JOB > x:
                    full += seg
                if w:
                    blk += seg
            parts.append(f"{saved / 3.6e6:5.1f}, {full / tot if tot else 0:5.1%}, {blk / tot if tot else 0:5.1%}, "
                         f"{area / tot / GIB if tot else 0:.2f}")
        lines.append(f"{fmt_t(lo)[:10]} | {len(rows) / hours:11.1f} | {sum(j['wait'] for j in rows) / 3.6e6:12.1f} | " + " | ".join(parts))
    return lines


def partition_cost(r, x):
    """Section 1f. Under the partition the main pool's booking room is x smaller at every
    instant; the jobs the lane admitted no longer book main over their historical interval
    [g, g + hold], which gives back their footprint there. Returns time-weighted quantiles of
    the net loss x - (moved footprint at t), in GiB, over the replay span."""
    ev = []
    for j in r["elig"]:
        if j["_id"] in r["admitted_at"] and j["hold"] > 0:
            ev.append((j["g"], j["_f"]))
            ev.append((j["g"] + j["hold"], -j["_f"]))
    ev.sort()
    lo = min(j["a"] for j in r["elig"])
    hi = max(j["g"] + j["hold"] for j in r["elig"])
    samples, back, last = [], 0, lo
    for t, d in ev:
        if t > last:
            samples.append((t - last, x - back))
            last = t
        back += d
    if hi > last:
        samples.append((hi - last, x - back))
    tot = sum(dt for dt, _ in samples)
    mean = sum(dt * v for dt, v in samples) / tot

    def q(p):
        acc = 0
        for dt, v in sorted(samples, key=lambda s: s[1]):
            acc += dt
            if acc >= p * tot:
                return v
        return samples[-1][1]
    return mean / GIB, q(0.10) / GIB, q(0.50) / GIB, q(0.90) / GIB


def incident(jobs, xs, y, lo, hi):
    """The 2026-11-18 incident window, in UTC milliseconds [lo, hi)."""
    out = []
    sel = [j for j in jobs if lo <= j["a"] < hi and j["budget"] <= y and not j["parent"]]
    waited = [j for j in sel if j["wait"] >= 10_000]
    out.append(f"eligible jobs arriving in the window: {len(sel)}; waited >= 10 s: {len(waited)}, "
               f"their wait {sum(j['wait'] for j in waited) / 60000:.1f} min, "
               f"p50 {pct([j['wait'] for j in waited], 50) / 1000:.0f} s, max {max([j['wait'] for j in waited] or [0]) / 1000:.0f} s")
    for x in xs:
        r = simulate(jobs, x, y, include_parents=False)
        adm = r["admitted_at"]
        after = []
        for j in r["elig"]:
            if lo <= j["a"] < hi and j["wait"] >= 10_000:
                after.append((j["g"] - adm[j["_id"]]) if j["_id"] in adm else 0)
                # remaining wait
        rem = []
        for j in r["elig"]:
            if lo <= j["a"] < hi and j["wait"] >= 10_000:
                rem.append((adm[j["_id"]] - j["a"]) if j["_id"] in adm else j["wait"])
        out.append(f"x={x / GIB:.0f} GiB: of those, lane-admitted {sum(1 for j in r['elig'] if lo <= j['a'] < hi and j['wait'] >= 10_000 and j['_id'] in adm)}; "
                   f"remaining wait {sum(rem) / 60000:.1f} min, p50 {pct(rem, 50) / 1000:.0f} s, max {max(rem or [0]) / 1000:.0f} s")
    return out


EET = timezone(timedelta(hours=2))  # the journal's local time; EET has no DST in November
JTS = re.compile(r"(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) aira daemon: ")
JTHROTTLED = re.compile(r"slice ceiling throttled \(enforce\): (\d+) effective / (\d+) configured")
JFREEZE = re.compile(r"admission fairness-freeze (hold|yield|idle) on ")
JHEAD = re.compile(r": head seq=\d+ reserve=(\d+) ")
MAX_HOLD_MS = 5 * 60 * 1000  # the daemon's fairness hold (a hold line is followed by yield 5 min later)


def parse_journal(path):
    """Round 4. Hold intervals [hold line, min(next yield/idle/warm-up line, hold + 5 min)) with
    the named head's reserve, and the throttled-ceiling samples, from the daemon journal
    capture that aira-350-ceiling-drop.py reads. Times in UTC ms."""
    holds, ceil, events = [], [], []
    with open(path, errors="replace") as fh:
        for line in fh:
            m = JTS.search(line)
            if not m:
                continue
            t = int(datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S").replace(tzinfo=EET).timestamp() * 1000)
            c = JTHROTTLED.search(line)
            if c:
                ceil.append((t, int(c.group(1))))
                continue
            f = JFREEZE.search(line)
            if f:
                h = JHEAD.search(line)
                events.append((t, f.group(1), int(h.group(1)) if h else None))
            elif "slice ceiling unevaluated: warming up" in line:
                events.append((t, "start", None))
    open_hold = None
    for t, kind, reserve in events:
        if open_hold is not None and kind != "hold":
            holds.append((open_hold[0], min(t, open_hold[0] + MAX_HOLD_MS), open_hold[1]))
            open_hold = None
        if kind == "hold":
            if open_hold is not None:
                holds.append((open_hold[0], min(t, open_hold[0] + MAX_HOLD_MS), open_hold[1]))
            open_hold = (t, reserve)
    if open_hold is not None:
        holds.append((open_hold[0], open_hold[0] + MAX_HOLD_MS, open_hold[1]))
    holds.sort()
    return holds, ceil


def round4(jobs, y, xs, holds, ceil, slots, allowances, small_wait_h, lo_i, hi_i):
    """Section 1i: the round-4 design (lane inside the one ceiling, with priority)."""
    out = []
    gate = [(s, e) for s, e, _ in holds]
    lo = min(j["a"] for j in jobs)
    hi = max(j["g"] + j["hold"] for j in jobs)
    in_window = [(max(s, lo), min(e, hi)) for s, e in gate if e > lo and s < hi]
    hold_ms = sum(e - s for s, e in in_window)
    out.append(f"hold intervals in the replay window: {len(in_window)}, {hold_ms / 3.6e6:.1f} h of {(hi - lo) / 3.6e6:.1f} h "
               f"({hold_ms / (hi - lo):.1%})")
    gs = [s for s, _ in in_window]
    ge = [e for _, e in in_window]

    def overlap(a, b):
        tot = 0
        k = max(0, bisect.bisect_right(gs, a) - 1)
        while k < len(gs) and gs[k] < b:
            tot += max(0, min(b, ge[k]) - max(a, gs[k]))
            k += 1
        return tot
    elig = [j for j in jobs if j["budget"] <= y and not j["parent"]]
    ov = sum(overlap(j["a"], j["g"]) for j in elig)
    elig_wait_h = sum(j['wait'] for j in elig) / 3.6e6
    out.append(f"eligible small jobs' wait that fell inside a hold (overlap, not cause): {ov / 3.6e6:.1f} h of their "
               f"{elig_wait_h:.1f} h ({ov / 3.6e6 / elig_wait_h:.1%}); {ov / 3.6e6 / small_wait_h:.1%} of all "
               f"{small_wait_h:.1f} small wait-hours")
    out.append("x GiB | gate | slot allowance | saved-h | saved/all-small | lane-adm | full-for-1G share | occ mean/p90/peak GiB")
    for x in xs:
        for label, g in (("none", None), ("holds", gate)):
            r = simulate(jobs, x, y, False, derived=True, extra=0, gate=g)
            out.append(f"{x / GIB:>5.0f} | {label:<5} | {'-':>14} | {r['saved_h']:.1f} | {r['saved_h'] / small_wait_h:.1%} | "
                       f"{r['admitted']} | {r['full_share']:.1%} | {r['mean_occ']:.2f}/{r['p90_occ']:.2f}/{r['peak_occ']:.2f}")
        for allowance in allowances:
            r = simulate(jobs, x, y, False, derived=True, extra=0, gate=gate, slots=slots, allowance=allowance)
            out.append(f"{x / GIB:>5.0f} | holds | {allowance:>14} | {r['saved_h']:.1f} | {r['saved_h'] / small_wait_h:.1%} | "
                       f"{r['admitted']} | {r['full_share']:.1%} | {r['mean_occ']:.2f}/{r['p90_occ']:.2f}/{r['peak_occ']:.2f}")
    # the incident window under the hold-gated rule
    for x in xs:
        r = simulate(jobs, x, y, False, derived=True, extra=0, gate=gate)
        adm = r["admitted_at"]
        rem = [(adm[j["_id"]] - j["a"]) if j["_id"] in adm else j["wait"]
               for j in r["elig"] if lo_i <= j["a"] < hi_i and j["wait"] >= 10_000]
        out.append(f"incident window, holds-gated, x={x / GIB:.0f} GiB: {len(rem)} jobs that waited >= 10 s; "
                   f"remaining wait {sum(rem) / 60000:.1f} min, p50 {pct(rem, 50) / 1000:.0f} s, max {max(rem or [0]) / 1000:.0f} s")
    # guard G: would a lane at x have kept the named head from fitting beside it?
    cts = [t for t, _ in ceil]
    sizes = sorted(r for _, _, r in holds if r is not None)
    out.append(f"hold lines naming a head: {len(sizes)}; head reserve p50 {pct(sizes, 50) / GIB:.2f} GiB, "
               f"p99 {pct(sizes, 99) / GIB:.2f}, max {sizes[-1] / GIB if sizes else 0:.2f} GiB")
    for x in xs:
        binds = unseen = 0
        margins = []
        for s, _, reserve in holds:
            if reserve is None:
                continue
            k = bisect.bisect_right(cts, s) - 1
            if k < 0 or s - cts[k] > 120_000:
                unseen += 1
                continue
            jobs_in_lane = max(1, x // (64 * MIB))  # the most jobs a lane of x can hold (64 MiB each), plus the head
            room = ceil[k][1] - (2 * GIB + 64 * MIB * min(jobs_in_lane + 1, 33)) - x
            margins.append(room - reserve)
            if reserve > room:
                binds += 1
        out.append(f"x={x / GIB:.0f} GiB: holds where G would bind (head reserve > E - (2 GiB + 64 MiB x (lane jobs + 1)) - x, "
                   f"lane jobs <= 32 slots): {binds} of {len(margins)} with a throttled sample within 120 s "
                   f"({unseen} without); smallest margin {min(margins) / GIB if margins else float('nan'):.2f} GiB")
    return out


def snapshot_report(path, y):
    with open(path) as fh:
        data = json.load(fh)["data"]
    sr = data.get("slice_reserve") or {}
    scopes = data.get("scopes") or []
    lines = []
    cur = sr.get("slice_current_bytes", 0) - sr.get("slice_reclaimable_bytes", 0)
    lines.append(f"ledger granted {sr.get('granted_bytes', 0) / GIB:.2f} GiB of ceiling {sr.get('ceiling_bytes', 0) / GIB:.2f} GiB "
                 f"(effective {sr.get('ceiling_effective_bytes', 0) / GIB:.2f}); jobs {sr.get('jobs')}; queued {sr.get('queued')}; "
                 f"phase {sr.get('freeze_phase')}; slice current-less-reclaimable {cur / GIB:.2f} GiB; MemAvailable "
                 f"{sr.get('system_mem_available_bytes', 0) / GIB:.2f} GiB; ceiling basis {sr.get('ceiling_basis')}")
    job_level = [s for s in scopes if not s.get("worker")]
    workers = [s for s in scopes if s.get("worker")]
    small = [s for s in job_level if 0 < (s.get("reserve_bytes") or 0) <= y]
    lines.append(f"scopes {len(scopes)}: job-level {len(job_level)}, worker {len(workers)} "
                 f"(worker reserve {sum(s.get('reserve_bytes') or 0 for s in workers) / GIB:.2f} GiB)")
    for age_h in (1, 13, 24):
        old = [s for s in small if (s.get("age_seconds") or 0) >= age_h * 3600]
        lines.append(f"small job-level scopes (reserve <= y) older than {age_h:>2} h: {len(old)}, "
                     f"lane footprint {sum((s.get('reserve_bytes') or 0) + PER_JOB for s in old) / GIB:.2f} GiB, "
                     f"rss now {sum(s.get('rss_bytes') or 0 for s in old) / GIB:.2f} GiB")
    lines.append(f"all small job-level scopes: {len(small)}, footprint {sum((s.get('reserve_bytes') or 0) + PER_JOB for s in small) / GIB:.2f} GiB, "
                 f"rss now {sum(s.get('rss_bytes') or 0 for s in small) / GIB:.2f} GiB")
    return lines


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", required=True)
    ap.add_argument("--snapshot")
    ap.add_argument("--journal", help="daemon journal capture (aira-350-ceiling-drop.py input); enables section 1i")
    ap.add_argument("--y", type=float, default=1.0, help="job size limit, GiB")
    ap.add_argument("--x", type=float, nargs="+", default=[2, 4, 6, 8], help="lane sizes, GiB")
    ap.add_argument("--silt", type=float, default=1.56, help="lane GiB held throughout by long-lived jobs (section 1b)")
    ap.add_argument("--slots", type=int, default=32, help="CPU slot ceiling (section 1e)")
    ap.add_argument("--allowance", type=int, nargs="+", default=[0, 10, 16, 20, 24, 27],
                    help="slots held by jobs the store does not show (section 1e)")
    args = ap.parse_args()
    y = int(args.y * GIB)
    xs = [int(v * GIB) for v in args.x]
    jobs, lost_sigs, no_timing = load(args.db)
    lo = min(j["a"] for j in jobs)
    hi = max(j["g"] + j["hold"] for j in jobs)
    print(f"window {fmt_t(lo)} .. {fmt_t(hi)} ({(hi - lo) / 3.6e6:.1f} h); confine rows with wait/start/wall/budget: {len(jobs)}")
    print(f"limits: signatures at the 20-row cap whose oldest retained row is inside the window "
          f"(older rows lost): {lost_sigs}; rows in the window without timing, by kind: {dict(no_timing)}")
    small = [j for j in jobs if j["budget"] <= y]
    large = [j for j in jobs if j["budget"] > y]
    parents = [j for j in small if j["parent"]]
    print(f"\n## 0 population (y = {args.y:g} GiB)")
    for name, pop in (("small (all)", small), ("small parents", parents), ("large", large)):
        w = [j["wait"] for j in pop]
        print(f"{name:<14} n={len(pop):>6} waited>=10s={sum(1 for v in w if v >= 10_000):>5} wait-hours={sum(w) / 3.6e6:7.1f} "
              f"p50(>=10s)={pct([v for v in w if v >= 10_000], 50) / 1000:.0f}s")
    sw = [j for j in small if not j["parent"]]
    ratios = [j["peak"] / j["budget"] for j in sw if j["peak"] and j["budget"]]
    print(f"eligible small jobs' peak/reserve: p50 {pct(ratios, 50):.2f} p90 {pct(ratios, 90):.2f} p99 {pct(ratios, 99):.2f}; "
          f"run time p50 {pct([j['hold'] for j in sw], 50) / 1000:.0f}s p90 {pct([j['hold'] for j in sw], 90) / 1000:.0f}s; "
          f"ran >= 1 h: {sum(1 for j in sw if j['hold'] >= 3_600_000)}")
    small_wait_h = sum(j["wait"] for j in small) / 3.6e6
    for include_parents in (False, True):
        label = "+parents" if include_parents else "eligible (parents excluded)"
        print(f"\n## 1 lane replay, {label}")
        print("x GiB | jobs | wait-h | saved-h | saved/all-small | lane-adm (of which immediate) | overflow (waited>=10s, their wait-h) "
              "| waits>=10s fully/partly saved | occ mean/p50/p90/peak GiB | full-for-1G share | someone-waiting share "
              "| occ by jobs >=10m / >=1h | real(peak-rss sum) p50/p99/peak GiB | lane jobs p99/peak")
        for x in xs:
            r = simulate(jobs, x, y, include_parents)
            print(f"{x / GIB:>5.0f} | {r['n']} | {r['wait_h']:.1f} | {r['saved_h']:.1f} | {r['saved_h'] / small_wait_h:.1%} | "
                  f"{r['admitted']} ({r['lane_immediate']}) | {r['overflow']} ({r['overflow_waited']}, {r['overflow_wait_h']:.1f}) | "
                  f"{r['full_saves']}/{r['part_saves']} of {r['waited']} | "
                  f"{r['mean_occ']:.2f}/{r['p50_occ']:.2f}/{r['p90_occ']:.2f}/{r['peak_occ']:.2f} | {r['full_share']:.1%} | {r['blocked_share']:.1%} | "
                  f"{r['m10_share']:.1%} / {r['long_share']:.1%} | {r['p50_real']:.2f}/{r['p99_real']:.2f}/{r['peak_real']:.2f} | "
                  f"{r['p99_count']}/{r['peak_count']}")
    silt = int(args.silt * GIB)
    print(f"\n## 1b eligible, with {args.silt:g} GiB of the lane held throughout by long-lived jobs (the live snapshot's servers, section 3)")
    print("x GiB | saved-h | saved/all-small | overflow (waited>=10s) | full-for-1G share")
    for x in xs:
        r = simulate(jobs, x, y, False, silt=silt)
        print(f"{x / GIB:>5.0f} | {r['saved_h']:.1f} | {r['saved_h'] / small_wait_h:.1%} | {r['overflow']} ({r['overflow_waited']}) | {r['full_share']:.1%}")
    def lane_loss(r, x):
        """The partition's net main booking room lost, x - min(S, x), time-weighted (GiB)."""
        loss = [(dt, (x - min(occ, x)) / GIB) for _, dt, occ, _ in r["segs"]]
        tot = sum(dt for dt, _ in loss)
        mean = sum(dt * v for dt, v in loss) / tot if tot else 0
        acc = 0
        for dt, v in sorted(loss, key=lambda s: s[1]):
            acc += dt
            if acc >= 0.9 * tot:
                return mean, v
        return mean, 0

    for variant, title in ((True, "1h derived accounting (spec round 3): overflowed small jobs and small suite parents granted in main\n"
                                  "## count against x until they end, so freed lane room goes to them first (eligible; parents never lane-admitted)"),
                           ("overflow-only", "1h' the same with small suite parents NOT counted (the round-3 rule: a lease with a live\n"
                                             "## sub-reservation is not small); only overflowed small jobs return first")):
        print(f"\n## {title}")
        print("x GiB | saved-h | saved/all-small | lane-adm | overflow (waited>=10s) | full-for-1G share | occ mean/p90/peak GiB "
              "| with silt: saved-h, saved/all-small | partition's main room lost x - min(S, x): mean / p90 GiB")
        for x in xs:
            r = simulate(jobs, x, y, False, derived=variant)
            rs = simulate(jobs, x, y, False, silt=silt, derived=variant)
            loss_mean, loss_p90 = lane_loss(r, x)
            print(f"{x / GIB:>5.0f} | {r['saved_h']:.1f} | {r['saved_h'] / small_wait_h:.1%} | {r['admitted']} | "
                  f"{r['overflow']} ({r['overflow_waited']}) | {r['full_share']:.1%} | "
                  f"{r['mean_occ']:.2f}/{r['p90_occ']:.2f}/{r['peak_occ']:.2f} | {rs['saved_h']:.1f}, {rs['saved_h'] / small_wait_h:.1%} | "
                  f"{loss_mean:.2f} / {loss_p90:.2f}")
    print("\n## 1c cut alternative: main first (a job granted within 1 s never enters the lane), eligible")
    print("x GiB | lane jobs | saved-h | saved/all-small | occ mean/p90 GiB | occ by jobs >=10m / >=1h")
    for x in xs:
        r = simulate(jobs, x, y, False, main_first=True)
        print(f"{x / GIB:>5.0f} | {r['admitted']} | {r['saved_h']:.1f} | {r['saved_h'] / small_wait_h:.1%} | "
              f"{r['mean_occ']:.2f}/{r['p90_occ']:.2f} | {r['m10_share']:.1%} / {r['long_share']:.1%}")
    print("\n## 1d per UTC day, eligible (the 20-row cap drops the OLDEST rows of frequent commands, so earlier days can be thinner; daily load also varies)")
    for line in per_day(jobs, [4 * GIB, 8 * GIB], y):
        print(line)
    print(f"\n## 1e CPU-slot sensitivity, eligible ({args.slots} slots, one per job; allowance = slots held by jobs the store does not show)")
    print("x GiB | allowance | saved-h | saved/all-small | lane-adm | jobs refused a lane grant at least once for want of a slot")
    for x in (4 * GIB, 8 * GIB):
        for allowance in args.allowance:
            r = simulate(jobs, x, y, False, slots=args.slots, allowance=allowance)
            print(f"{x / GIB:>5.0f} | {allowance:>9} | {r['saved_h']:.1f} | {r['saved_h'] / small_wait_h:.1%} | {r['admitted']} | {r['cpu_denied']}")
    starts = sorted(j["g"] for j in jobs)
    ends = sorted(j["g"] + j["hold"] for j in jobs)
    conc = max(bisect.bisect_right(starts, t) - bisect.bisect_right(ends, t) for t in starts)
    print(f"retained rows' peak concurrency (historical times): {conc} jobs")
    for hh, mm in ((11, 13), (11, 20), (11, 25)):
        t = int(datetime(2026, 11, 18, hh, mm, tzinfo=UTC).timestamp() * 1000)
        print(f"retained rows running at 2026-11-18 {hh:02d}:{mm:02d} UTC (the incident; reported live: 32 jobs): "
              f"{bisect.bisect_right(starts, t) - bisect.bisect_right(ends, t)}")
    print("\n## 1f the partition's cost to the main pool: x less the footprint the lane moved out of main, time-weighted (GiB)")
    print("x GiB | net main booking room lost: mean / p10 / p50 / p90")
    for x in (4 * GIB, 8 * GIB):
        mean, p10, p50, p90 = partition_cost(simulate(jobs, x, y, False), x)
        print(f"{x / GIB:>5.0f} | {mean:.2f} / {p10:.2f} / {p50:.2f} / {p90:.2f}")
    print("\n## 1g the largest retained reserves (a job above the main pool's room under the partition waits for the ceiling to rise)")
    top = sorted((j["budget"] for j in jobs), reverse=True)
    print(f"largest: {', '.join(f'{b / GIB:.1f}' for b in top[:5])} GiB; rows >= 20 GiB: {sum(1 for b in top if b >= 20 * GIB)}; "
          f">= 30 GiB: {sum(1 for b in top if b >= 30 * GIB)}")
    lo_i = int(datetime(2026, 11, 18, 11, 9, tzinfo=UTC).timestamp() * 1000)
    hi_i = int(datetime(2026, 11, 18, 11, 30, tzinfo=UTC).timestamp() * 1000)
    if args.journal:
        holds, ceil = parse_journal(args.journal)
        print("\n## 1i round 4: the lane INSIDE the one ceiling, with priority (x caps bookings, no 64 MiB per job in x;\n"
              "## derived S: every small job-level lease counts, however granted, suite parents included; no ceiling-room gate)")
        for line in round4(jobs, y, [4 * GIB, 8 * GIB], holds, ceil, args.slots, [16, 24, 27], small_wait_h, lo_i, hi_i):
            print(line)
    print("\n## 2 the 2026-11-18 incident (11:09-11:30 UTC, 13:09-13:30 EET), eligible small jobs")
    for line in incident(jobs, [4 * GIB, 8 * GIB], y, lo_i, hi_i):
        print(line)
    if args.snapshot:
        print("\n## 3 live snapshot (aggregates only)")
        for line in snapshot_report(args.snapshot, y):
            print(line)


if __name__ == "__main__":
    main()
