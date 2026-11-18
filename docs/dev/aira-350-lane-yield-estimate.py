#!/usr/bin/env python3
"""Expected-yield estimate behind the small-job lane design (AIRA-350,
docs/superpowers/specs/2026-11-18-small-job-lane-design.md, section "Expected yield").

Question: of recent confine admission waits, how many belong to small jobs (booking <= y)
that were held by the head-of-line freeze while they fitted, i.e. the population the lane
targets, and how often would the lane's guards have closed it?

Inputs (both read-only; never point --db at the live file):

  sqlite3 -readonly ~/.local/state/aira/state.db ".backup $HOME/tmp/aira-350/state-copy.db"
  journalctl --user -u aira-daemon.service --since '2026-11-15 00:00' --no-pager -o short-unix \\
      | grep 'fairness-freeze\\|slice ceiling throttled' > ~/tmp/aira-350/freeze-journal.txt
  aira confine -- python3 -I docs/dev/aira-350-lane-yield-estimate.py \\
      --db ~/tmp/aira-350/state-copy.db --journal ~/tmp/aira-350/freeze-journal.txt

Parts
  1  population: confine rows carrying wait_ms and started_at_ms (AIRA-323, from 2026-11-15),
     split small (budget <= y) / large, with wait counts, wait-hours and percentiles.
  2  freeze-released small jobs: a small job whose grant (started_at_ms) falls within
     [-1 s, +3 s] of a journal "fairness-freeze yield" line was granted by the AIRA-307
     backstop pass, the signature of a wait held by the freeze while the job fitted. A
     shifted-window control (+60 s and -60 s) measures chance coincidence.
  3  per backstop pass: how many small jobs it released and their summed lane cost
     (booking + 64 MiB per-job headroom), against x.
  4  heads: every "fairness-freeze hold" line names its head's reserve. With the effective
     ceiling from the last "slice ceiling throttled" sample before it, the lane's guard G3
     (head.reserve + x <= effective - headroom(1)) and the budget/lane comparison are counted.

LIMITS
  - the store keeps 20 rows per exact signature, so frequent commands have lost rows: counts
    are of the retained sample, not of every job that ran.
  - `budget` is the job's scope cap (its reserve for an ordinary confine job), used as the
    booking proxy; pin-charge enforce is off (observe), so booking == reserve at grant.
  - a match in part 2 says the job was granted on a backstop pass; it does not say how long
    before that it first fitted, so the lane's saving per job is bounded above by its wait,
    not measured. The ceiling room at each instant is not in the store.
  - the CPU ledger is not logged anywhere, so a CPU-refused small job cannot be separated
    from a freeze-held one, except that a backstop-pass grant needs CPU to fit at that pass.
"""
import argparse
import re
import sqlite3
import statistics

GIB = 1 << 30
MIB = 1 << 20
PER_JOB_HEADROOM = 64 * MIB
BASE_HEADROOM = 2 * GIB

FREEZE_RE = re.compile(r"^(\d+\.\d+) .*admission fairness-freeze (hold|yield|idle) on ")
HEAD_RE = re.compile(r": head seq=(\d+) reserve=(\d+)")
MAX_HOLD_MS = 300_000  # defaultAdmitFreezeMaxHold (internal/daemon/paths.go), 5 minutes
CEIL_RE = re.compile(r"^(\d+\.\d+) .*slice ceiling throttled \(enforce\): (\d+) effective")


def pct(values, q):
    if not values:
        return float("nan")
    values = sorted(values)
    k = max(0, min(len(values) - 1, int(round(q / 100.0 * (len(values) - 1)))))
    return values[k]


def load_rows(db, since, until):
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    rows = con.execute(
        "SELECT budget, wait_ms, started_at_ms, wall_ms, at FROM confine_peak_history "
        "WHERE kind='confine' AND wait_ms IS NOT NULL AND started_at_ms IS NOT NULL "
        "AND budget IS NOT NULL AND at >= ? AND at < ?", (since, until)).fetchall()
    con.close()
    return [dict(budget=b, wait=w, grant=s, wall=wl or 0, at=a) for b, w, s, wl, a in rows]


def load_journal(path):
    """Returns backstop-pass instants, holds as (start, end, head_reserve), and ceiling samples.

    A hold runs from its "hold" line to the next freeze transition line, capped at
    MAX_HOLD_MS (the drain reset can end a hold without a line of its own)."""
    yields, transitions, holds_raw, ceilings = [], [], [], []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            m = FREEZE_RE.match(line)
            if m:
                ts = float(m.group(1)) * 1000.0
                transitions.append(ts)
                if m.group(2) == "yield":
                    yields.append(ts)
                elif m.group(2) == "hold":
                    h = HEAD_RE.search(line)
                    holds_raw.append((ts, int(h.group(2)) if h else None))
                continue
            m = CEIL_RE.match(line)
            if m:
                ceilings.append((float(m.group(1)) * 1000.0, int(m.group(2))))
    transitions.sort()
    holds = []
    for ts, reserve in sorted(holds_raw):
        end = ts + MAX_HOLD_MS
        for t in transitions:
            if t > ts:
                end = min(end, t)
                break
        holds.append((ts, end, reserve))
    return sorted(yields), holds, sorted(ceilings)


def overlap_ms(lo, hi, windows):
    total = 0.0
    for a, b, _ in windows:
        if b <= lo:
            continue
        if a >= hi:
            break
        total += min(hi, b) - max(lo, a)
    return total


def matched(grant, yields, lo, hi):
    # yields sorted; linear scan is fine at this size
    for y in yields:
        if y + lo <= grant <= y + hi:
            return y
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--journal", required=True)
    ap.add_argument("--since", default="2026-11-15T00:00")
    ap.add_argument("--until", default="2026-11-18T12:00")
    ap.add_argument("--y", type=int, default=GIB, help="lane job bound, bytes (default 1 GiB)")
    ap.add_argument("--x", type=int, default=4 * GIB, help="lane cap, bytes (default 4 GiB)")
    args = ap.parse_args()

    rows = load_rows(args.db, args.since, args.until)
    yields, holds, ceilings = load_journal(args.journal)
    small = [r for r in rows if r["budget"] <= args.y]
    large = [r for r in rows if r["budget"] > args.y]

    print(f"window {args.since} .. {args.until}; y={args.y / GIB:.2f} GiB x={args.x / GIB:.2f} GiB")
    print(f"journal: {len(yields)} backstop passes (yield), {len(holds)} holds, {len(ceilings)} ceiling samples")
    print("\n## 1 population (retained rows with wait_ms + started_at_ms)")
    for name, pop in (("small", small), ("large", large)):
        waits = [r["wait"] for r in pop]
        w10 = [w for w in waits if w >= 10_000]
        print(f"{name:5s} n={len(pop):5d} waited>=10s={len(w10):5d} ({100.0 * len(w10) / max(1, len(pop)):.1f}%)"
              f" >=60s={sum(w >= 60_000 for w in waits):5d} >=240s={sum(w >= 240_000 for w in waits):5d}"
              f" wait-hours={sum(waits) / 3.6e6:7.1f} p50(>=10s)={pct(w10, 50) / 1000:.0f}s p90(>=10s)={pct(w10, 90) / 1000:.0f}s")

    print("\n## 2 small jobs granted on a backstop pass (grant within [-1s,+3s] of a yield line)")
    waited_small = [r for r in small if r["wait"] >= 10_000]
    for label, lo, hi in (("on-pass", -1000, 3000), ("control+60s", 59_000, 63_000), ("control-60s", -61_000, -57_000)):
        hits = [r for r in waited_small if matched(r["grant"], yields, lo, hi) is not None]
        hrs = sum(r["wait"] for r in hits) / 3.6e6
        print(f"{label:12s} {len(hits):5d} of {len(waited_small)} small waits >=10s"
              f" ({100.0 * len(hits) / max(1, len(waited_small)):.1f}%), their wait-hours={hrs:.1f}"
              f" of {sum(r['wait'] for r in waited_small) / 3.6e6:.1f}, p50 wait={pct([r['wait'] for r in hits], 50) / 1000:.0f}s")

    print("\n## 2b small-job wait spent while a fairness hold was active (upper bound on freeze-held time)")
    for name, pop in (("small", small), ("large", large)):
        waited = [r for r in pop if r["wait"] >= 10_000]
        under = [overlap_ms(r["grant"] - r["wait"], r["grant"], holds) for r in waited]
        touched = sum(u > 0 for u in under)
        total = sum(r["wait"] for r in waited)
        print(f"{name:5s} waits>=10s={len(waited)}: {touched} ({100.0 * touched / max(1, len(waited)):.1f}%) overlap a hold;"
              f" wait-hours under a hold={sum(under) / 3.6e6:.1f} of {total / 3.6e6:.1f}"
              f" ({100.0 * sum(under) / max(1, total):.1f}%)")

    print("\n## 3 per backstop pass: small jobs released and their lane cost (booking + 64 MiB)")
    per_pass = {}
    for r in waited_small:
        y = matched(r["grant"], yields, -1000, 3000)
        if y is not None:
            per_pass.setdefault(y, []).append(r)
    costs = sorted(sum(r["budget"] + PER_JOB_HEADROOM for r in rs) for rs in per_pass.values())
    counts = sorted(len(rs) for rs in per_pass.values())
    if costs:
        within = sum(c <= args.x for c in costs)
        print(f"passes releasing >=1 small job: {len(costs)}; jobs per pass p50={pct(counts, 50)} max={counts[-1]};"
              f" lane cost p50={pct(costs, 50) / GIB:.2f} GiB p90={pct(costs, 90) / GIB:.2f} GiB max={costs[-1] / GIB:.2f} GiB;"
              f" passes whose released small jobs fit x at once: {within} ({100.0 * within / len(costs):.0f}%)")

    print("\n## 4 heads named by hold lines: budget vs lane, and guard G3")
    ci = 0
    g3_closed = 0
    budget_ge_x = 0
    sizes = []
    for ts, _end, reserve in holds:
        if reserve is None:
            continue
        while ci + 1 < len(ceilings) and ceilings[ci + 1][0] <= ts:
            ci += 1
        effective = ceilings[ci][1] if ceilings and ceilings[ci][0] <= ts else None
        sizes.append(reserve)
        if reserve // 2 >= args.x:
            budget_ge_x += 1
        if effective is not None and reserve + args.x > effective - (BASE_HEADROOM + PER_JOB_HEADROOM):
            g3_closed += 1
    if sizes:
        print(f"holds={len(sizes)} head reserve p50={pct(sizes, 50) / GIB:.2f} GiB p90={pct(sizes, 90) / GIB:.2f} GiB"
              f" max={max(sizes) / GIB:.2f} GiB; heads whose budget (R/2) is already >= x: {budget_ge_x}"
              f" ({100.0 * budget_ge_x / len(sizes):.0f}%); heads for which G3 closes the lane: {g3_closed}")
        hist = {}
        for s in sizes:
            hist[s / GIB] = hist.get(s / GIB, 0) + 1
        top = sorted(hist.items(), key=lambda kv: -kv[1])[:8]
        print("most common head reserves (GiB: holds): " + ", ".join(f"{k:.2f}: {v}" for k, v in top))


if __name__ == "__main__":
    main()
