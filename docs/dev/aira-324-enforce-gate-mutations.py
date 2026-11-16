#!/usr/bin/env python3
"""AIRA-324 enforce gate: every named mutation of the replay or the watch must turn its test red.

A point-in-time record for the enforce-gate PR (the snippets match the files as committed by
it). Each mutation edits ONE snippet in a copy of the file (under a fresh directory in ~/tmp,
never the worktree), runs the test meant to catch it, and reports RED (caught) or SURVIVED.

    python3 -I docs/dev/aira-324-enforce-gate-mutations.py

Replay mutations run `aira-324-enforce-gate-replay.py --selftest`; watch mutations run
`aira-324-enforce-watch-test.py` against the mutated watch. Exit 0: every mutation caught;
1: one survived or a snippet no longer matches (the list must track the code).

Equivalent mutant, left out on purpose: charge_for's `int(round(factor * 100))` as
`int(factor * 100)` gives the same percent for every factor the script uses (0.8, 0.9, 1.0).
"""
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
REPLAY = "aira-324-enforce-gate-replay.py"
WATCH = "aira-324-enforce-watch.sh"
WTEST = "aira-324-enforce-watch-test.py"

R = [
    # the review's list (2026-11-16, round 4)
    ("over-54 comparator x10", "sum(1 for v in mm.values() if v > SAFE)", "sum(1 for v in mm.values() if v > 10 * SAFE)"),
    ("busiest hour by min", "busiest = max(whole, key=lambda k: hours[k][0])", "busiest = min(whole, key=lambda k: hours[k][0])"),
    ("minute max is the min", '"minute_max": max(mm.values()) if mm else 0', '"minute_max": min(mm.values()) if mm else 0'),
    ("memory view at factor 1.0", 'blind = simulate(jobs, ce, fac, pol, "blind", hid)', 'blind = simulate(jobs, ce, 1.0, pol, "blind", hid)'),
    ("trigger against the pin", "j.known and j.peak > charge_for(j.pin, fac)),", "j.known and j.peak > j.pin),"),
    ("earlier counted as <=", "sum(1 for j in js if s[j.idx] < ref[j.idx] - 1000)", "sum(1 for j in js if s[j.idx] <= ref[j.idx])"),
    ("hidden load dropped from the ledger", "avail = ceil - max(booked + hidden, seen + hidden)", "avail = ceil - max(booked, seen + hidden)"),
    ("SAFE 1.9 x MemoryHigh", "SAFE = 0.9 * MEMORY_HIGH", "SAFE = 1.9 * MEMORY_HIGH"),
    ("MemoryHigh 64 GiB", "MEMORY_HIGH = 60 * GIB", "MEMORY_HIGH = 64 * GIB"),
    ("burst level before start", "    i = bisect.bisect_right(keys, t) - 1", "    i = bisect.bisect_left(keys, t) - 1"),
    # this round's fixes
    ("memory window stops at the observed end", '"mem": memory_view(jobs, blind, mlo, None, common_hi, busiest)',
     '"mem": memory_view(jobs, blind, mlo, common_hi, common_hi, busiest)'),
    ("busiest hour may be a clipped hour", "whole = [k for k in hours if k >= lo and k + 3600000 <= hi] or list(hours)",
     "whole = list(hours)"),
    ("trigger rounds before comparing", "    if 20 * above > n:", "    if round(above / n, 2) > 0.05:"),
    ("trigger ignores the unknown grants", "    if 20 * (above + unknown) <= n:", "    if 20 * above <= n:"),
    ("trigger at exactly 5% fires", "    if 20 * above > n:", "    if 20 * above >= n:"),
    ("attribution takes any basis", "        if j.pin_class:\n            by[j.pin].append(j)", "        by[j.pin].append(j)"),
    ("attribution tolerance x10", "            if d <= tol:", "            if d <= 10 * tol:"),
    ("attribution not one to one", "        if i in match or k in used:", "        if i in match:"),
    # round 5: attribution keeps its uncertainty; minutes clipped to the window
    ("attribution tolerance 500 ms", "TOL = 100 ", "TOL = 500 "),
    ("component low counts matched rows as above", 'out["low"] += max(0, k - b - u)', 'out["low"] += min(a, k)'),
    ("component high drops rowless grants", 'out["high"] += min(a + u, k) + len(gs) - k', 'out["high"] += min(a + u, k)'),
    ("component high ignores disagreeing rows", 'out["high"] += min(a + u, k) + len(gs) - k',
     'out["high"] += max(0, k - b - u) + len(gs) - k'),
    ("components never merge", "            parent[a] = b", "            pass"),
    ("minute scan unclipped at lo", "a, b = max(m, lo), min(m + 60000, hi)", "a, b = m, min(m + 60000, hi)"),
    ("minute scan unclipped at hi", "a, b = max(m, lo), min(m + 60000, hi)", "a, b = max(m, lo), m + 60000"),
    ("queued from 0 s", "    return j.start - j.arrival >= 1000", "    return j.start - j.arrival >= 0"),
    # round 6: unknown peaks kept unknown; the busiest hour fixed once; visible + real slice
    ("NULL peak read as 0 on load", '"select rowid, started_at_ms, wait_ms, wall_ms, peak_rss, ',
     '"select rowid, started_at_ms, wait_ms, wall_ms, coalesce(peak_rss, 0), '),
    ("unknown peak taken as known", "self.known = peak is not None", "self.known = True"),
    ("unknown row counted below (low)", 'out["low"] += max(0, k - b - u)', 'out["low"] += max(0, k - b)'),
    ("unknown row dropped (high)", 'out["high"] += min(a + u, k) + len(gs) - k', 'out["high"] += min(a, k) + len(gs) - k'),
    ("unknown row counted as known below", "b = len(rows) - a - u", "b = len(rows) - a"),
    ("one-to-one estimate includes unknown peaks", "len(rows) == 1 and u == 0:", "len(rows) == 1:"),
    ("proxy drops unknown peaks", "sum(1 for j in pins if not j.known), len(pins))", "0, len(pins))"),
    ("busiest sliceAnon hour may be short", "ok = [h for h, v in hours.items() if len(v) >= min_lines]", "ok = list(hours)"),
    ("busiest sliceAnon hour by min", "return max(ok, key=lambda h: sum(hours[h]) / len(hours[h])) if ok else None",
     "return min(ok, key=lambda h: sum(hours[h]) / len(hours[h])) if ok else None"),
    ("fixed busiest hour ignored", "    if busiest is None:\n        busiest = max(whole", "    if True:\n        busiest = max(whole"),
    ("unknown-peak bound ignored", "st = step(jobs, start, value or (lambda j: j.peak))", "st = step(jobs, start, lambda j: j.peak)"),
    ("visible + slice: no held anon", "        elif a > 0:\n            anon = ceil_series[a - 1][2]",
     "        elif False:\n            anon = 0"),
    ("later-than-observed split ignores queued", "_, olq = moved(jobs, s, obs, queued)", "_, olq = moved(jobs, s, obs)"),
    ("daemon starts not counted", "            if START_RE.search(line):\n                starts.append(ms)", "            pass"),
    # earlier rounds
    ("ceil -> floor", "c = -(-pin * pct // 100)", "c = pin * pct // 100"),
    ("no physical fit", "fits = w.pin <= physfree", "fits = True"),
    ("discount auto too", "self.pin_class = basis == PIN_BASIS", "self.pin_class = True"),
    ("bypass budget full", "> book[head.idx] // 2:", "> book[head.idx]:"),
    ("headroom jobs not +1", "HEADROOM_PER_JOB * (njobs + 1)", "HEADROOM_PER_JOB * njobs"),
    ("no idle fill", "segs.append((0, hi - lo - covered))", "pass"),
    ("ignore ceiling changes", "if ti < len(times) and queue:", "if False:"),
    ("fifo passes head", '                head = w\n                if policy == "fifo":\n                    break', "                head = w"),
    ("blind sees peaks", 'seen = peaks if phys == "clairvoyant" else 0', "seen = peaks"),
    ("blind ledger sees peaks", "avail = ceil - max(booked + hidden, seen + hidden)", "avail = ceil - max(booked + hidden, peaks + hidden)"),
    ("minute boundary", "st[i][0] <= a:", "st[i][0] < a:"),
    ("floor ignored", "return min(max(c, FLOOR), pin)", "return min(c, pin)"),
    ("pin cap ignored", "return min(max(c, FLOOR), pin)", "return max(c, FLOOR)"),
    ("residual ignores visible", "- level_at(sb, t, kb))", ")"),
    ("first order no FIFO stop", "                    break                      # FIFO", "                    continue                   # FIFO"),
    ("first order frees auto too", "for j in running if j.pin_class)", "for j in running)"),
]

W = [
    ("no XDG_RUNTIME_DIR default", 'export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"', ":"),
    ("failed journal read is 0", 'if journal_ok == "1":', "if True:"),
    ("failed store-journal read ignored", 'if pincharge_ok != "1":', "if False:"),
    ("undiscounted grants counted", "if int(m.group(3)) < int(m.group(2)):          # discounted", "if True:"),
    ("above against the pin", "charged = {g[1]: g[2] for g in grants}", "charged = {g[1]: g[1] for g in grants}"),
    ("100-row minimum restored", 'if isinstance(ac, dict) and ac["trigger"] == "met":',
     'if isinstance(ac, dict) and ac["trigger"] == "met" and ac["grants"] >= 100:'),
    ("epoch's first grant dropped", "grants = [g for g in grants if g[0] >= lo_ms]", "grants = [g for g in grants if g[0] >= lo * 1000]"),
    # round 5: evicted rows persist, delegate parents excluded, every trigger tested
    ("no row persistence", "                    seen.add(tuple(map(int, p)))", "                    pass"),
    ("delegate parents kept", '"and instr(signature, ?) != 1",', '"and instr(signature, ?) != -1",'),
    ("oom trigger off", '    if line["d_oom_local"] > 0:', "    if False:"),
    ("oom first sample dropped silently",
     '    unev.append("slice-level oom: %s (no previous sample to compare)" % line["d_oom_local"]\n'
     '                if last is None else "slice-level oom: unevaluated (counter reset: slice re-created)")', "    pass"),
    ("high trigger off", 'elif h1 > bl["high_p99_per_hour"] and h2 > bl["high_p99_per_hour"]:', "elif False:"),
    ("high one hour enough", 'elif h1 > bl["high_p99_per_hour"] and h2 > bl["high_p99_per_hour"]:', 'elif h1 > bl["high_p99_per_hour"]:'),
    ("high counter reset ignored", "    if any(y < x for x, y in zip(inside, inside[1:])):", "    if False:"),
    ("pressure trigger off", 'elif all(r[2] > bl["some_avg10_p99"] for r in w):', "elif False:"),
    ("pressure any sample enough", 'elif all(r[2] > bl["some_avg10_p99"] for r in w):', 'elif any(r[2] > bl["some_avg10_p99"] for r in w):'),
    ("pressure gap ignored", "        if not continuous(allrows, now - 7200, now):", "        if False:"),
    ("MemAvailable trigger off", "elif ma < WATCHDOG_TRIP:", "elif False:"),
    ("unreadable meminfo read as 0", "    return None\n\n\ndef p99", "    return 0\n\n\ndef p99"),
    ("watchdog trip ignored", "    if trips:", "    if False:"),
    ("oomd kill ignored", 'elif line["oomd_kills"]:', "elif False:"),
    ("unreadable oomd journal read as 0", 'if oomd_ok == "1":', "if True:"),
    ("enforce factor not checked", "    if [x for x in factors if x != TRIAL_FACTOR]:", "    if False:"),
    # round 6: NULL peaks, the pressure window's left edge, met.log latch, baseline guard, scratch
    ("watch reads a NULL peak as 0", "coalesce(peak_rss, -1)", "coalesce(peak_rss, 0)"),
    ("watch takes -1 as a known peak", "pk if pk >= 0 else None", "pk"),
    ("pressure drops the reading held at the left edge",
     "w = ([held] if held else []) + [r for r in allrows if r[0] > now - 7200]", "w = [r for r in allrows if r[0] > now - 7200]"),
    ("met not latched", "if met:\n    # LATCH", "if False:\n    # LATCH"),
    ("baseline overwrite allowed after enforce", 'if os.path.exists(enforce_flag) and force_baseline != "1":', "if False:"),
    ("--force-baseline ignored", 'if os.path.exists(enforce_flag) and force_baseline != "1":', "if os.path.exists(enforce_flag):"),
    ("enforce-seen never written", "if seen_enforce and not os.path.exists(enforce_flag):", "if False:"),
    ("baseline ignores this run's enforce lines", 'if any("pin-charge enforce:" in l for l in f) and not os.path.exists(enforce_flag):',
     "if False:"),
    ("scratch left behind", """trap 'rm -rf "$scratch"' EXIT""", ":"),
]


def main():
    base = pathlib.Path(os.path.expanduser("~/tmp"))
    base.mkdir(parents=True, exist_ok=True)
    work = pathlib.Path(tempfile.mkdtemp(prefix="aira-324-mutations-", dir=str(base)))
    survived, broken, red = [], [], 0
    try:
        for f in (REPLAY, WATCH, WTEST):
            shutil.copy2(HERE / f, work / f)
        for target, muts in ((REPLAY, R), (WATCH, W)):
            src = (HERE / target).read_text()
            for name, old, new in muts:
                if src.count(old) != 1:
                    broken.append("%s: snippet matched %d times in %s" % (name, src.count(old), target))
                    print("UNAPPLIED %s" % name, flush=True)
                    continue
                (work / target).write_text(src.replace(old, new, 1))
                if target == REPLAY:
                    cmd = [sys.executable, "-I", str(work / REPLAY), "--selftest"]
                else:
                    cmd = [sys.executable, "-I", str(work / WTEST), str(work / WATCH)]
                p = subprocess.run(cmd, capture_output=True, text=True)
                fails = [l for l in p.stdout.splitlines() if l.startswith("FAIL")]
                caught = p.returncode != 0
                red += caught
                print("%s %s: %s" % ("RED     " if caught else "SURVIVED", name, (fails[:1] or [p.stderr.strip()[-120:]])[0][:110]),
                      flush=True)
                if not caught:
                    survived.append(name)
                (work / target).write_text(src)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    total = len(R) + len(W)
    print("\n%d mutations: %d red, %d survived, %d unapplied" % (total, red, len(survived), len(broken)))
    for s in survived:
        print("SURVIVED:", s)
    for b in broken:
        print("UNAPPLIED:", b)
    return 1 if survived or broken else 0


if __name__ == "__main__":
    sys.exit(main())
