#!/usr/bin/env python3
"""Read-only journal measurement for AIRA-350 (fixed small-job lane), spec round 3
(docs/superpowers/specs/2026-11-18-fixed-small-lane-design.md, sections 2.0 and 6).

Questions:
  1. How far, and how often, does the pressure ceiling E fall below its own recent peak?
     Under the partition with the lane gated on today's whole-slice room W (round 2), a
     lane grant is refused once E falls by more than about x - L after main filled its
     room. The journal has no booked-bytes series, so this measures CEILING DROPS, not
     lane closure: it is an upper estimate of the closed share, conditional on main being
     full when the drop starts.
  2. How low did MemAvailable actually go (the realistic host floor today)?
  3. How often did the daemon restart?

Input: the daemon's journal text on stdin. Every line this reads carries Go's log
timestamp ("2026/11/15 00:00:44") inside the message, so the journalctl output format
does not matter. Reproduce with a pinned window:

  journalctl --user -u aira-daemon.service --since "2026-11-15 00:00" \\
      --until "2026-11-18 21:12" --no-pager \\
    | aira confine -- python3 -I docs/dev/aira-350-ceiling-drop.py

Lines used:
  "slice ceiling throttled (enforce): <E> effective / <max> configured, sliceAnon=..
   MemAvailable=<m> bound-by=<term> ..."   one per minute while the ceiling is throttled;
  "admission fairness-freeze hold on ..."    a head was waiting (main room short);
  "slice ceiling unevaluated: warming up"     a daemon start.
Minutes with no throttled line (the ceiling at the static cap, or a daemon restart's
warm-up) are absent, so drops FROM an unthrottled minute are not seen (understated).
"""
import re
import statistics
import sys
from datetime import datetime

GIB = 1 << 30
TS = re.compile(r"(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) aira daemon: ")
THROTTLED = re.compile(r"slice ceiling throttled \(enforce\): (\d+) effective / (\d+) configured.*?"
                       r"MemAvailable=(\d+) bound-by=(\S+)")
HOLD = "admission fairness-freeze hold on "
WARMUP = "slice ceiling unevaluated: warming up"


def pct(values, q):
    values = sorted(values)
    if not values:
        return float("nan")
    k = max(0, min(len(values) - 1, int(round(q / 100.0 * (len(values) - 1)))))
    return values[k]


def main():
    samples = []   # (t seconds, E bytes, MemAvailable bytes, bound-by)
    holds = []     # t seconds
    starts = []
    for line in sys.stdin:
        m = TS.search(line)
        if not m:
            continue
        t = datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S").timestamp()
        rest = line[m.end():]
        th = THROTTLED.match(rest)
        if th:
            samples.append((t, int(th.group(1)), int(th.group(3)), th.group(4)))
        elif rest.startswith(HOLD):
            holds.append(t)
        elif rest.startswith(WARMUP):
            starts.append(t)
    if not samples:
        print("no throttled samples on stdin")
        return 1
    samples.sort()
    t0, t1 = samples[0][0], samples[-1][0]
    span_min = (t1 - t0) / 60.0 + 1
    print(f"window {datetime.fromtimestamp(t0):%Y-%m-%d %H:%M} .. {datetime.fromtimestamp(t1):%Y-%m-%d %H:%M} "
          f"local ({span_min / 60:.1f} h)")
    bound = {}
    for s in samples:
        bound[s[3]] = bound.get(s[3], 0) + 1
    print(f"throttled samples {len(samples)} of about {span_min:.0f} minutes "
          f"({len(samples) / span_min:.1%}); bound-by {bound}")
    print(f"daemon starts (warm-up lines) {len(starts)}; fairness-freeze hold lines {len(holds)}")

    es = [s[1] / GIB for s in samples]
    ma = [s[2] / GIB for s in samples]
    print("\n## E, the effective ceiling while throttled (GiB)")
    print(f"min {min(es):.2f}  p10 {pct(es, 10):.2f}  p50 {pct(es, 50):.2f}  p90 {pct(es, 90):.2f}  max {max(es):.2f}")
    print("\n## MemAvailable while throttled (GiB): the realistic host floor today")
    print(f"min {min(ma):.2f}  p1 {pct(ma, 1):.2f}  p5 {pct(ma, 5):.2f}  p50 {pct(ma, 50):.2f}")
    below = sum(1 for v in ma if v < 8.0)
    print(f"samples below the watchdog's 8 GiB trip: {below}")

    print("\n## E below its trailing peak (share of throttled samples; an ESTIMATE of how often a")
    print("## partition lane gated on W could be shut, IF main was full when the drop began)")
    holds.sort()
    near_hold = []
    j = 0
    for s in samples:
        # a head was waiting within the previous 5 minutes: main's room was short
        while j < len(holds) and holds[j] < s[0] - 300:
            j += 1
        near_hold.append(j < len(holds) and holds[j] <= s[0])
    print("window | drop >= d GiB | all samples | samples within 5 min after a hold line")
    n_hold = sum(near_hold)
    for window_min in (2, 10, 60):
        w = window_min * 60
        dq = []  # monotone deque of (t, E) for the trailing max
        peaks = []
        for t, e, _, _ in samples:
            while dq and dq[0][0] < t - w:
                dq.pop(0)
            while dq and dq[-1][1] <= e:
                dq.pop()
            dq.append((t, e))
            peaks.append(dq[0][1])
        for d in (2.88, 4.0):
            hit = [(p - s[1]) / GIB >= d for p, s in zip(peaks, samples)]
            all_share = sum(hit) / len(samples)
            hold_share = (sum(1 for h, nh in zip(hit, near_hold) if h and nh) / n_hold) if n_hold else float("nan")
            print(f"{window_min:>4} min | {d:>4.2f} | {all_share:6.2%} | {hold_share:6.2%} (of {n_hold})")
    drops = [(es[i - 1] - es[i]) for i in range(1, len(es)) if samples[i][0] - samples[i - 1][0] <= 90]
    print(f"\nminute-to-minute falls (consecutive samples): p99 {pct(drops, 99):.2f} GiB, max {max(drops):.2f} GiB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
