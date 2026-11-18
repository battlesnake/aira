#!/usr/bin/env python3
"""Round-4 figures behind the small-job lane design (AIRA-350,
docs/superpowers/specs/2026-11-18-small-job-lane-design.md, sections "Round 4", 3.3, 9 and 13).

Inputs (both read-only; never point --db at the live file), the same as
docs/dev/aira-350-lane-yield-estimate.py:

  sqlite3 -readonly ~/.local/state/aira/state.db ".backup $HOME/tmp/aira-350/state-copy.db"
  journalctl --user -u aira-daemon.service --since '2026-11-15 00:00' --no-pager -o short-unix \\
      | grep 'fairness-freeze\\|slice ceiling throttled' > ~/tmp/aira-350/freeze-journal.txt
  aira confine -- python3 -I docs/dev/aira-350-round4-queries.py \\
      --db ~/tmp/aira-350/state-copy.db --journal ~/tmp/aira-350/freeze-journal.txt

Parts
  1  heads named by "fairness-freeze hold" lines, by size: how many are no larger than y, no
     larger than x, and how many cannot be made room for within ONE hold from a full ledger at
     the window's average release rate (round-3 query part 4), today and with a per-head
     allowance of min(x, R) taken inside the hold. An order of magnitude, not a bound: the rate
     is an average (busy periods release faster), and a head rarely starts from a full ledger.
  2  aitest-parent rows (suite parents) among small confine rows in the window: their booking
     distribution, and the chance that k small jobs passing a head include at least one parent,
     1 - (1 - p)^k, with p the parent share (a lower bound: a `make test-engine` parent does not
     carry the aitest-parent signature).

LIMITS
  - the store keeps 20 rows per exact signature, so counts are of the retained sample.
  - `budget` (the scope cap) stands in for the booking; pin-charge is in observe, so they agree.
"""
import argparse
import re
import sqlite3

GIB = 1 << 30
MIB = 1 << 20
MAX_HOLD_MIN = 5.0  # defaultAdmitFreezeMaxHold (internal/daemon/paths.go), 5 minutes

HOLD_RE = re.compile(r"^(\d+\.\d+) .*admission fairness-freeze hold on .*: head seq=(\d+) reserve=(\d+)")


def pct(values, q):
    if not values:
        return float("nan")
    values = sorted(values)
    k = max(0, min(len(values) - 1, int(round(q / 100.0 * (len(values) - 1)))))
    return values[k]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--journal", required=True)
    ap.add_argument("--since", default="2026-11-15T00:00", help="UTC, inclusive")
    ap.add_argument("--until", default="2026-11-18T12:00", help="UTC, exclusive")
    ap.add_argument("--rate-gb-per-min", type=float, default=2.71,
                    help="average release rate (round-3 query part 4)")
    args = ap.parse_args()

    # Part 1. The journal extract starts at the window start and was taken before its end.
    heads = []
    with open(args.journal, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            m = HOLD_RE.match(line)
            if m:
                heads.append(int(m.group(3)))
    per_hold = args.rate_gb_per_min * 1e9 * MAX_HOLD_MIN
    print(f"## 1 heads named by hold lines (n={len(heads)}); one hold at "
          f"{args.rate_gb_per_min} GB/min x {MAX_HOLD_MIN:.0f} min = {per_hold / GIB:.2f} GiB")
    for label, bound in (("<= 1 GiB (y)", 1 * GIB), ("<= 2 GiB", 2 * GIB), ("<= 4 GiB", 4 * GIB)):
        n = sum(1 for r in heads if r <= bound)
        print(f"heads {label}: {n} ({100.0 * n / len(heads):.1f}%)")
    for x_gib in (0, 2, 4):
        x = x_gib * GIB
        miss = [r for r in heads if r + min(x, r) > per_hold]
        print(f"allowance min({x_gib} GiB, R) taken in the hold: heads that need more than one hold "
              f"from a full ledger: {len(miss)} ({100.0 * len(miss) / len(heads):.1f}%), "
              f"smallest such head {min(miss) / GIB if miss else float('nan'):.2f} GiB")

    # Part 2.
    con = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    rows = con.execute(
        "SELECT budget, signature LIKE 'aitest-parent%' FROM confine_peak_history "
        "WHERE kind = 'confine' AND budget IS NOT NULL AND budget <= ? "
        "AND started_at_ms >= CAST(strftime('%s', ?) AS INTEGER) * 1000 "
        "AND started_at_ms <  CAST(strftime('%s', ?) AS INTEGER) * 1000",
        (GIB, args.since.replace("T", " ") + ":00", args.until.replace("T", " ") + ":00")).fetchall()
    con.close()
    parents = [b for b, is_parent in rows if is_parent]
    p = len(parents) / len(rows) if rows else float("nan")
    print(f"\n## 2 small confine rows n={len(rows)}; aitest-parent rows {len(parents)} (p={p:.3f})")
    if parents:
        print(f"parent booking MiB: p10={pct(parents, 10) / MIB:.0f} p50={pct(parents, 50) / MIB:.0f} "
              f"p90={pct(parents, 90) / MIB:.0f} max={max(parents) / MIB:.0f}; "
              f"<= 512 MiB: {sum(1 for b in parents if b <= 512 * MIB)}; "
              f"== 1024 MiB: {sum(1 for b in parents if b == GIB)}")
    for k in (1, 2, 4, 8):
        print(f"k={k} small jobs past a head: P(at least one suite parent) >= {1 - (1 - p) ** k:.3f}")


if __name__ == "__main__":
    main()
