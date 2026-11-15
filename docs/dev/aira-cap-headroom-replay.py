#!/usr/bin/env python3
"""Evidence and downside replay behind the cap-headroom addendum
(docs/superpowers/specs/2026-11-15-cap-headroom-design.md).

Question: if an auto-sized (history-sized) confine job kept its admission BOOKING C but ran
under a cgroup LIMIT L = k x C, how many OOM kills would be avoided, what would it cost the
slice, and what does it do to the estimator?

Reads a COPY of the state DB, read-only. Never point it at the live file:

  sqlite3 -readonly ~/.local/state/aira/state.db ".backup $HOME/tmp/aira-cap-headroom-data/state-copy.db"
  aira confine --memory-reserve 384M -- python3 -I docs/dev/aira-cap-headroom-replay.py \\
      --db ~/tmp/aira-cap-headroom-data/state-copy.db

Parts
  1  the owner's quoted evidence, re-derived (every cap source): OOM rows with a budget, how many
     have a clean run of the same signature, and the clean max peak against the killed budget.
  2  the same, split by cap source, by whether the clean run came before or after the kill, by
     the clean run's own cap, and (auto rows) by a walk-forward reconstruction of the estimator
     rung that sized the killed run. Only auto rows (cap:auto:daemon-reserve) are what the rule
     touches; operator pins are AIRA-324's population.
  3  the AIRA-318 interaction: clean auto rows whose peak reached >= 95% of their cap (a cap the
     page cache filled). Under L = k x C such a run records a peak near k x C, and the estimator
     books 1.15 x that next time.
  4  the downside: 20 concurrent auto jobs (mixed, and 20 of one command+subcommand "burst",
     and bursts of commands that have an OOM on record), sum of use over sum of bookings, today
     and under k. With the ledger full, sum(bookings) ~ the ceiling, so P(ratio > 1) is the
     chance the slice would be driven over its ceiling by the jobs' own use.

LIMITS (stated in the spec, section "Evidence"):
  - peak is the scope's memory.peak, which counts page cache (AIRA-318), so a peak is an upper
    bound on anonymous need, and a peak at the cap is censored, not a measurement.
  - the store keeps 20 rows per exact signature, so frequent commands have lost rows and the
    walk-forward rung reconstruction sees only retained rows.
  - an OOM row's true need is CENSORED (only "> cap" is known). "Saved by k" uses the largest
    clean peak of the same signature as a proxy for the need: optimistic when the killed run was
    a bigger mode than any clean run, pessimistic when the clean peak is cache-inflated or came
    from a run under a much bigger cap.
  - failed (non-OOM) runs are not recorded (AIRA-264).
  - draws are independent and not duration-weighted (no start times before AIRA-323; 323 rows
    began 2026-11-15 and are too few to replay a timeline).
  - the physical-fit term (L <= ceiling - headroom - real use at grant) cannot be replayed: the
    store has no record of real use at each grant. Part 4 replays the fixed-k upper bound.
"""
import argparse
import collections
import os
import random
import re
import sqlite3

AUTO = "cap:auto:daemon-reserve"
PIN = "cap:operator:--memory-reserve"
RECENT = "2026-10-20"
KS = (1.0, 1.5, 2.0, 3.0, 4.0)
GiB = 1 << 30


# --- coarse command key, copied from docs/dev/aira-history-charge-replay.py (N3_cmd+sub) -----
# That script connects to the live DB at import, so it is not imported. No frozen contract.
def base(t):
    return t.rsplit("/", 1)[-1]


def mask(t):
    if re.fullmatch(r"[0-9a-f]{7,}|[0-9]+", t):
        return "<n>"
    if "/" in t or t.endswith((".py", ".rs", ".sh", ".txt", ".json", ".toml")):
        return "<p>"
    if re.search(r"[0-9]{3,}", t):
        return "<n>"
    return t


TIME_ARG = {"-o", "-f", "--output", "--format"}
TIME_FLAG = {"-p", "-v", "-a", "--portability", "--verbose", "--append"}


def unwrap(tk):
    i = 0
    while i < len(tk):
        t = tk[i]
        if t in ("env", "/usr/bin/env") or re.match(r"[A-Za-z_][A-Za-z0-9_]*=", t):
            i += 1
            continue
        if t in ("time", "/usr/bin/time"):
            j = i + 1
            while j < len(tk):
                if tk[j] in TIME_ARG and j + 1 < len(tk):
                    j += 2
                elif tk[j] in TIME_FLAG:
                    j += 1
                else:
                    break
            if j < len(tk) and not tk[j].startswith("-"):
                i = j
                continue
            return tk
        break
    return tk[i:] or tk


def cmdsub(sig):
    u = unwrap(sig.split("\x00"))
    rest = [mask(t) for t in u[1:]]
    pos = [t for t in rest if not t.startswith("-")]
    return base(u[0]) + "|" + (pos[0] if pos else "")


# ---------------------------------------------------------------------------------------------
def load(db):
    if os.path.realpath(db) == os.path.realpath(os.path.expanduser("~/.local/state/aira/state.db")):
        raise SystemExit("refusing the live state.db: replay a copy (see the docstring)")
    c = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = c.execute(
        "select rowid, signature, peak_rss, oom, at, budget, coalesce(budget_basis,'') "
        "from confine_peak_history where kind='confine' order by at, rowid").fetchall()
    return rows


def pct(a, b):
    return "%d (%.1f%%)" % (a, 100.0 * a / b) if b else "%d (n/a)" % a


def part1(rows):
    clean = collections.defaultdict(list)          # sig -> [(peak, at, budget, basis)]
    for _r, sig, peak, oom, at, bud, bb in rows:
        if not oom and peak:
            clean[sig].append((peak, at, bud, bb))
    ooms = [r for r in rows if r[3] and r[5]]
    print("PART 1  the quoted evidence, re-derived (all cap sources)")
    print("  confine rows %d; OOM rows %d; OOM rows with a budget %d" % (
        len(rows), sum(1 for r in rows if r[3]), len(ooms)))
    print("  distinct signatures among all OOM rows %d; among OOM rows with a budget %d" % (
        len({r[1] for r in rows if r[3]}), len({r[1] for r in ooms})))
    have = [r for r in ooms if clean.get(r[1])]
    print("  OOM rows (with budget) whose signature has a clean run recorded: %s" % pct(len(have), len(ooms)))
    for f in (1.5, 2.0, 4.0):
        n = sum(1 for r in have if max(p for p, *_ in clean[r[1]]) <= f * r[5])
        print("    clean max peak <= %.1fx killed budget: %s of those" % (f, pct(n, len(have))))
    return clean


def rung_at_kill(rows):
    """Walk-forward: for each OOM row, the estimator rung its signature's RETAINED earlier rows
    would select (oom-escalated if an earlier OOM row exists, estimate:max with >= 3 earlier
    clean rows, own-max with 1-2, else cold start). Retention hides evicted rows."""
    seen = collections.defaultdict(lambda: [0, 0])   # sig -> [clean, oom] so far
    out = {}
    for rid, sig, peak, oom, _at, _bud, _bb in rows:
        cl, om = seen[sig]
        if oom:
            out[rid] = ("oom-escalated" if om else "estimate:max" if cl >= 3
                        else "own-max" if cl >= 1 else "cold-start")
        if oom:
            seen[sig][1] += 1
        elif peak:
            seen[sig][0] += 1
    return out


def part2(rows, clean):
    rung = rung_at_kill(rows)
    print("\nPART 2  split by cap source, clean-run timing and cap, and (auto) estimator rung")
    for src in (AUTO, PIN, "cap:operator:--memory-max", "(other)"):
        sel = [r for r in rows if r[3] and r[5] and (r[6] == src or (src == "(other)" and r[6] not in (AUTO, PIN, "cap:operator:--memory-max")))]
        if not sel:
            continue
        have = [r for r in sel if clean.get(r[1])]
        line = "  %-28s OOM rows %5d, with a clean run %s;" % (src, len(sel), pct(len(have), len(sel)))
        for f in (1.5, 2.0, 4.0):
            n = sum(1 for r in have if max(p for p, *_ in clean[r[1]]) <= f * r[5])
            line += " <=%.1fx %s" % (f, pct(n, len(sel)))
        print(line)
    auto = [r for r in rows if r[3] and r[5] and r[6] == AUTO]
    print("  auto OOM rows: share of ALL auto OOM rows 'saved' at k (largest clean peak of the signature <= k x killed budget)")
    print("    %-34s %6s | %s" % ("subset", "rows", "  ".join("k=%.1f" % k for k in KS[1:])))

    def row(name, sel, peaks_of):
        cells = []
        for k in KS[1:]:
            n = 0
            for r in sel:
                ps = peaks_of(r)
                if ps and max(ps) <= k * r[5]:
                    n += 1
            cells.append("%5.1f%%" % (100.0 * n / len(sel) if sel else 0))
        print("    %-34s %6d | %s" % (name, len(sel), "  ".join(cells)))

    row("any clean run (as quoted)", auto, lambda r: [p for p, *_ in clean.get(r[1], [])])
    row("clean runs BEFORE the kill only", auto, lambda r: [p for p, at, *_ in clean.get(r[1], []) if at < r[4]])
    row("clean runs AFTER the kill only", auto, lambda r: [p for p, at, *_ in clean.get(r[1], []) if at > r[4]])
    row("clean, not at its own cap (<95%)", auto, lambda r: [p for p, _a, b, _ in clean.get(r[1], []) if b and p < 0.95 * b])
    print("    (no clean run at all: %s of auto OOM rows: unknowable, counted as not saved)" % pct(
        sum(1 for r in auto if not clean.get(r[1])), len(auto)))
    print("    NOTE: a clean peak at or below the killed cap says nothing about the killed run (its need was")
    print("    > cap), so the rows above are NOT the estimate. The three-way split below is.")
    by = collections.defaultdict(list)
    for r in auto:
        by[rung[r[0]]].append(r)
    hist_rungs = by.get("own-max", []) + by.get("estimate:max", []) + by.get("oom-escalated", [])
    print("  three-way split of auto OOM rows by the largest clean peak P of the signature (any time):")
    print("    within k   = P in (cap, k x cap]  -> proxy: counted as saved")
    print("    beyond k   = P > k x cap          -> a mode of this command does not fit k x cap")
    print("    undetermined = no clean P > cap   -> need censored; the high end of the proxy range counts these as saved")
    print("    %-30s %5s | %s" % ("population", "rows", " | ".join("k=%.1f within/beyond/undet" % k for k in (1.5, 2.0, 3.0))))

    def split(name, sel):
        cells = []
        for k in (1.5, 2.0, 3.0):
            w = b = u = 0
            for r in sel:
                ps = [p for p, *_ in clean.get(r[1], []) if p > r[5]]
                if not ps:
                    u += 1
                elif max(ps) <= k * r[5]:
                    w += 1
                else:
                    b += 1
            n = len(sel) or 1
            cells.append("%5.1f%% %5.1f%% %5.1f%%" % (100.0 * w / n, 100.0 * b / n, 100.0 * u / n))
        print("    %-30s %5d | %s" % (name, len(sel), " | ".join(cells)))

    split("all auto", auto)
    split("history rungs (own/max/oom-esc)", hist_rungs)
    split("no retained earlier row", by.get("cold-start", []))
    one_gib = sum(1 for r in by.get("cold-start", []) if r[5] == GiB)
    print("  retained-evidence category, NOT the estimator's actual basis (walk-forward over RETAINED rows; 'no retained earlier row' includes evicted history):")
    for name in ("cold-start", "own-max", "estimate:max", "oom-escalated"):
        print("    %-14s %5d" % ("no-earlier-row" if name == "cold-start" else name, len(by.get(name, []))))
    print("    no-earlier-row auto OOM rows killed at exactly 1 GiB (the client default): %d" % one_gib)
    # the 1.5x-per-kill creep, in kills: consecutive OOM rows of one signature (EVERY cap source:
    # an intervening row of any source breaks the streak), counted when the streak's rows are all
    # auto and it ended with a clean row of any source.
    runs = collections.Counter()
    streak = {}                                      # sig -> [length, all_auto]
    for _rid, sig, peak, oom, _at, _b, bb in rows:
        if oom:
            st = streak.setdefault(sig, [0, True])
            st[0] += 1
            st[1] = st[1] and bb == AUTO
            continue
        st = streak.pop(sig, None)
        if st and st[1] and peak:
            runs[st[0]] += 1
    print("    auto-only OOM streaks ended by a clean run (any source), by length: %s" % dict(sorted(runs.items())))


def part3(rows):
    cl = [r for r in rows if r[6] == AUTO and not r[3] and r[2] and r[5]]
    at = [r for r in cl if r[2] >= 0.95 * r[5]]
    sigs = collections.Counter(r[1] for r in at)
    print("\nPART 3  AIRA-318 interaction: clean auto rows at >= 95% of their cap")
    print("  clean auto rows %d; at >= 95%% of cap %s; signatures %d; signatures with >= 2 such rows %d" % (
        len(cl), pct(len(at), len(cl)), len(sigs), sum(1 for v in sigs.values() if v >= 2)))
    for f in (0.5, 0.75, 0.9):
        print("  clean auto rows at >= %.0f%% of cap: %s" % (100 * f, pct(sum(1 for r in cl if r[2] >= f * r[5]), len(cl))))
    # growth of the cap across CONSECUTIVE rows of one signature (a row of any source in between
    # breaks the pair) that are both clean auto rows at >= 95% of their cap.
    prev = {}
    grow = []
    for _rid, sig, peak, oom, _at, bud, bb in rows:
        atcap = bb == AUTO and not oom and peak and bud and peak >= 0.95 * bud
        if atcap and sig in prev:
            grow.append(bud / prev[sig])
        if atcap:
            prev[sig] = bud
        else:
            prev.pop(sig, None)
    if grow:
        grow.sort()
        print("  consecutive at-cap clean runs of one signature: %d pairs, cap ratio median %.3f, p90 %.3f" % (
            len(grow), grow[len(grow) // 2], grow[int(0.9 * len(grow))]))


def part4(rows, clean):
    random.seed(1)
    jobs = []
    for _rid, sig, peak, oom, at, bud, bb in rows:
        if bb != AUTO or not bud or at < RECENT:
            continue
        if not oom and not peak:
            continue
        known = [p for p, *_ in clean.get(sig, [])]
        jobs.append((sig, peak or bud, bool(oom), bud, max(known) if known else None, cmdsub(sig)))
    print("\nPART 4  downside: 20 concurrent auto jobs since %s (%d jobs, %d OOM-killed)" % (
        RECENT, len(jobs), sum(1 for j in jobs if j[2])))
    print("  use under k: a clean run's peak unchanged (variant 'cache': a run at >= 95% of its cap fills k x C);")
    print("  an OOM run uses min(k x C, largest clean peak of its signature), or k x C when none is known.")

    def use(j, k, cache):
        sig, peak, oom, bud, known, _key = j
        if oom:
            # a kill proves need > cap: censored. Use the largest clean peak only when it is above
            # the cap; otherwise the job is assumed to fill its whole limit (worst case).
            return min(k * bud, known) if known and known > bud else k * bud
        if cache and peak >= 0.95 * bud:
            return k * bud
        return peak

    groups = collections.defaultdict(list)
    for j in jobs:
        groups[j[5]].append(j)
    big = [g for g in groups.values() if len(g) >= 60]
    killers = [g for g in groups.values() if len(g) >= 20 and any(j[2] for j in g)]

    def mc(pool_pick, k, cache, n=4000):
        r = []
        for _ in range(n):
            s = pool_pick()
            r.append(sum(use(j, k, cache) for j in s) / sum(j[3] for j in s))
        r.sort()
        return "%d/%d" % (sum(v > 1 for v in r), n), r[int(0.99 * n)], r[-1]

    print("  %-6s %-6s | mixed n>1/draws p99 max | burst n>1 p99 max | OOM-cmd burst n>1 p99 max  (%d / %d groups)" % (
        "k", "cache", len(big), len(killers)))
    for k in KS:
        for cache in (False, True):
            m = mc(lambda: random.sample(jobs, 20), k, cache)
            b = mc(lambda: random.sample(random.choice(big), 20), k, cache)
            o = mc(lambda: random.sample(random.choice(killers), 20), k, cache)
            print("  %-6.1f %-6s | %9s %5.2f %5.2f | %9s %5.2f %5.2f | %9s %5.2f %5.2f" % (
                k, "yes" if cache else "no", *m, *b, *o))
    sl = sorted(sum(j[3] for j in random.sample(jobs, 20)) / GiB for _ in range(2000))
    print("  20 mixed auto jobs book a median %.1f GiB (p90 %.1f): the ledger fills with ~%d such jobs at a 45 GiB ceiling" % (
        sl[1000], sl[1800], int(45 / (sl[1000] / 20))))

    # Stacked with AIRA-324: auto AND --memory-reserve pinned jobs in their real proportion.
    # Pinned: charge 0.8 x pin, use = peak (cap stays the pin, so a pinned kill uses its pin).
    both = [("auto",) + j for j in jobs]
    for _rid, sig, peak, oom, at, bud, bb in rows:
        if bb == PIN and bud and at >= RECENT and (oom or peak):
            both.append(("pin", sig, peak or bud, bool(oom), bud, None, cmdsub(sig)))
    bgroups = collections.defaultdict(list)
    for j in both:
        bgroups[j[6]].append(j)
    bbig = [g for g in bgroups.values() if len(g) >= 60]

    def use2(j, k, cache):
        if j[0] == "pin":
            return j[4] if j[3] else j[2]
        return use(j[1:], k, cache)

    def charge2(j, f324):
        return f324 * j[4] if j[0] == "pin" else j[4]

    def mc2(pick, k, f324, n=4000):
        r = []
        for _ in range(n):
            s = pick()
            r.append(sum(use2(j, k, False) for j in s) / sum(charge2(j, f324) for j in s))
        r.sort()
        return "%d/%d" % (sum(v > 1 for v in r), n), r[int(0.99 * n)], r[-1]

    npin = sum(1 for j in both if j[0] == "pin")
    print("\n  stacked with AIRA-324 (pinned charge 0.8 x pin): %d jobs, %d pinned (%.0f%%)" % (
        len(both), npin, 100.0 * npin / len(both)))
    print("  %-6s %-5s | mixed n>1/draws p99 max | burst n>1 p99 max  (%d groups)" % ("k", "324", len(bbig)))
    for k in (1.0, 2.0, 3.0):
        for f in (1.0, 0.8):
            m = mc2(lambda: random.sample(both, 20), k, f)
            b = mc2(lambda: random.sample(random.choice(bbig), 20), k, f)
            print("  %-6.1f %-5s | %9s %5.2f %5.2f | %9s %5.2f %5.2f" % (k, "on" if f < 1 else "off", *m, *b))


def part5():
    """The AIRA-318 ratchet under L = k x C, by arithmetic: a job whose page cache fills any
    limit it is given (anonymous need tiny), so every run's memory.peak equals its limit.
    Three learning rules for a clean run that reached its limit:
      today     estimate = 1.15 x peak
      318-cap   estimate = min(1.15 x peak, the cap it ran under)   (AIRA-318's proposed rule)
      booking   estimate = min(1.15 x peak, the BOOKING it ran under) (needs the booking per row)"""
    print("\nPART 5  page-cache ratchet, booking C over six runs from C=1 GiB (cache fills the limit)")
    for k in (1.0, 2.0):
        for name in ("today", "318-cap", "booking"):
            c = 1.0
            seq = []
            for _ in range(6):
                limit = k * c
                peak = limit
                est = 1.15 * peak
                if name == "318-cap":
                    est = min(est, limit)
                elif name == "booking":
                    est = min(est, c)
                c = est
                seq.append(c)
            print("  k=%.0f %-8s %s" % (k, name, " ".join("%.2f" % v for v in seq)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True, help="a COPY of state.db (never the live file)")
    a = ap.parse_args()
    rows = load(os.path.expanduser(a.db))
    clean = part1(rows)
    part2(rows, clean)
    part3(rows)
    part4(rows, clean)
    part5()


if __name__ == "__main__":
    main()
