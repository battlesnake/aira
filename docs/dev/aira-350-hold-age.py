#!/usr/bin/env python3
"""AIRA-350 round 3: how old is the head when a fairness hold arms, and how much hold time a
lane closed T seconds after the head's enqueue would leave open (Alternative B of the spec,
docs/superpowers/specs/2026-11-18-small-job-lane-design.md, section 3.8).

Input: the freeze-journal extract described in docs/dev/aira-350-lane-yield-estimate.py
(journalctl lines matching 'fairness-freeze', with a unix timestamp first).

    aira confine --name <unique> -- python3 -I docs/dev/aira-350-hold-age.py \
        ~/tmp/aira-350/freeze-journal.txt

A hold runs from its "hold" line to the next freeze line, capped at maxHold (5 minutes). The
head's enqueue is the hold instant less the line's queued-for. Measured from the journal; the
share of hold time is a share of wall time, not of waiting jobs.
"""
import re, sys, statistics
pat = re.compile(r"^(\d+\.\d+) .*admission fairness-freeze (hold|yield|idle) on [^:]+(?:: head seq=(\d+) reserve=(\d+) queued-for=([0-9hms.]+))?")
def dur(s):
    t = 0.0
    for v, u in re.findall(r"([0-9.]+)([hms])", s):
        t += float(v) * {"h": 3600, "m": 60, "s": 1}[u]
    return t
ev = []
for line in open(sys.argv[1]):
    m = pat.match(line)
    if m:
        ev.append((float(m.group(1)), m.group(2), m.group(5)))
holds = []
for i, (t, ph, q) in enumerate(ev):
    if ph != "hold" or q is None:
        continue
    end = t + 300
    for t2, ph2, _ in ev[i + 1:]:
        end = min(end, t2)
        break
    holds.append((t, end, dur(q)))
ages = [q for _, _, q in holds]
print("holds", len(holds), "head queued-for at arm: p50 %.0fs p90 %.0fs; share >=300s %.0f%%, >=600s %.0f%%" % (
    statistics.median(ages), sorted(ages)[int(0.9 * (len(ages) - 1))],
    100 * sum(a >= 300 for a in ages) / len(ages), 100 * sum(a >= 600 for a in ages) / len(ages)))
tot = sum(e - s for s, e, _ in holds)
for T in (300, 600, 900):
    cov = sum(max(0.0, min(e, s - q + T) - s) for s, e, q in holds)
    print("T=%4ds lane-open share of hold time (enqueue-anchored): %.0f%%" % (T, 100 * cov / tot))
