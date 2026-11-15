#!/usr/bin/env python3
"""Evidence and downside replay behind the cap-headroom addendum
(docs/superpowers/specs/2026-11-15-cap-headroom-design.md).

Question: if an auto-sized (history-sized) confine job kept its admission BOOKING C but ran
under a cgroup LIMIT L = k x C, how many OOM kills would be avoided, what would it cost the
slice, and what does it do to the estimator?

Reads a COPY of the state DB, read-only. Never point it at the live file:

  sqlite3 -readonly ~/.local/state/aira/state.db ".backup $HOME/tmp/aira-cap-headroom-data/state-copy.db"
  aira confine -- python3 -I docs/dev/aira-cap-headroom-replay.py \\
      --db ~/tmp/aira-cap-headroom-data/state-copy.db
  (aira sizes the reserve from this command's history; a first run on a new box may pin
  --memory-reserve 512M: the measured peak is about 400 MiB, mostly page cache of the DB copy.)

Parts
  1  the owner's quoted evidence, re-derived (every cap source): OOM rows with a budget, how many
     have a clean run of the same signature, and the clean max peak against the killed budget.
  2  the same, split by cap source, by whether the clean run came before or after the kill, by
     the clean run's own cap, and (auto rows) by a walk-forward reconstruction of the estimator
     rung that sized the killed run. Only auto rows (cap:auto:daemon-reserve) are what the rule
     touches; operator pins are AIRA-324's population.
  3  the AIRA-318 interaction: clean auto rows whose peak reached >= 95% of their cap (a cap the
     page cache filled). Under L = k x C such a run records a peak near k x C, and the estimator
     books 1.15 x that next time. Then, for each such row, what the NEXT retained row of the same
     signature did (unestablished / does not follow the cap / candidate ratchet step / real
     growth), at the 95% and 99% thresholds. Cap proximity cannot separate page cache filling a
     cap from anonymous growth to it, so the ratchet pairs are CANDIDATES, not established
     cache-fillers; the part also bounds what a learning rule alone at k = 1 would cost.
  4  the downside: 20 concurrent auto jobs (mixed, and 20 of one command+subcommand "burst",
     and bursts of commands that have an OOM on record), sum of use over sum of bookings, today
     and under k. With the ledger full, sum(bookings) ~ the ceiling, so P(ratio > 1) is the
     chance the slice would be driven over its ceiling by the jobs' own use.
  5  the page-cache ratchet by arithmetic under three learning rules (a hypothetical job whose
     cache fills any limit), the one-time step of a finite cache under the booking rule and the
     recommended limit (including a large booking, where the 1 GiB term binds), and a k lowered
     after history was written at a higher k (rev 2's unconditional rule against rev 3's).
  6  the additive alternative L = min(k x C, C + H): killed caps by size, the within-k proxy at
     several H, and the total headroom it hands out relative to L = k x C.

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
    store has no record of real use at each grant. Part 4 is a fixed-k scenario without the physical-fit term.
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
    # the 1.5x-per-kill creep, in kills: a run of consecutive AUTO OOM rows of one signature, ended
    # by a clean row of any source. A non-auto OOM row (or any other row) ends the auto run; a
    # non-auto OOM starts no auto run, so a later auto OOM starts a fresh one.
    runs = collections.Counter()
    streak = {}                                      # sig -> length of the current auto OOM run
    for _rid, sig, peak, oom, _at, _b, bb in rows:
        if oom and bb == AUTO:
            streak[sig] = streak.get(sig, 0) + 1
            continue
        n = streak.pop(sig, 0)
        if n and not oom and peak:
            runs[n] += 1
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
    # These are CANDIDATES only: cap proximity cannot tell page cache filling the cap from anonymous
    # memory that grew ~15% per run to the next cap; the store holds no anon/cache split.
    prev = {}
    grow = []
    pair_sigs = set()
    for _rid, sig, peak, oom, _at, bud, bb in rows:
        atcap = bb == AUTO and not oom and peak and bud and peak >= 0.95 * bud
        if atcap and sig in prev:
            grow.append(bud / prev[sig])
            pair_sigs.add(sig)
        if atcap:
            prev[sig] = bud
        else:
            prev.pop(sig, None)
    if grow:
        grow.sort()
        print("  consecutive at-cap clean runs of one signature (candidate ratchet pairs; cache and anon growth"
              " not separable): %d pairs over %d signatures, cap ratio median %.3f, p90 %.3f" % (
                  len(grow), len(pair_sigs), grow[len(grow) // 2], grow[int(0.9 * len(grow))]))
    print("  where an at-cap row sits: peak >= 99.9%% of cap %d; 99-99.9%% %d; 95-99%% %d" % (
        sum(1 for r in at if r[2] >= 0.999 * r[5]),
        sum(1 for r in at if 0.99 * r[5] <= r[2] < 0.999 * r[5]),
        sum(1 for r in at if r[2] < 0.99 * r[5])))
    for thr in (0.95, 0.99):
        next_row(rows, thr)


def next_row(rows, thr):
    """What the NEXT retained row of the same signature did after a clean auto row at >= thr of its
    cap. Separates jobs that fill any limit (the ratchet step 1 stops) from jobs whose next run
    genuinely needed more than the old cap (which step 1 at k = 1 would turn into an OOM kill if
    the growth was anonymous memory; anon and cache are not separable in the store)."""
    bysig = collections.defaultdict(list)
    for r in rows:
        bysig[r[1]].append(r)
    out = collections.Counter()
    risk = []
    for rs in bysig.values():
        for i, r in enumerate(rs):
            _id, _s, peak, oom, _at, bud, bb = r
            if not (bb == AUTO and not oom and peak and bud and peak >= thr * bud):
                continue
            if i + 1 >= len(rs):
                out["a  no later retained row (unestablished)"] += 1
                continue
            _n, _s2, npeak, noom, _a2, nbud, nbb = rs[i + 1]
            if nbb != AUTO:
                out["b  next row not auto"] += 1
            elif noom:
                out["c  next row an auto OOM"] += 1
            elif not npeak or not nbud:
                out["d  next row has no peak/budget"] += 1
            elif nbud <= bud * 1.01:
                out["e  next cap did not grow (uninformative)"] += 1
            elif npeak <= bud:
                out["f  next cap grew, next peak <= old cap (does not follow the cap)"] += 1
            elif npeak >= thr * nbud:
                out["g  next peak > old cap and at its own new cap (candidate ratchet step: cache or anon growth)"] += 1
            else:
                out["h  next peak > old cap, below thr of new cap (real growth; step 1 at k=1 risks a kill)"] += 1
                risk.append(npeak / bud)
    tot = sum(out.values())
    print("  next retained row after a clean auto row at >= %.0f%% of its cap (%d rows):" % (100 * thr, tot))
    for key in sorted(out):
        print("    %s: %s" % (key, pct(out[key], tot)))
    if risk:
        risk.sort()
        print("    (h) next peak / old cap: median %.3f p90 %.3f max %.3f; fit under 1.5 x old cap: %d of %d" % (
            risk[len(risk) // 2], risk[int(0.9 * len(risk))], risk[-1], sum(1 for v in risk if v <= 1.5), len(risk)))


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
    # A FINITE cache: the job reads F GiB of files once per run (anonymous need tiny), so its peak
    # is min(limit, F), under the RECOMMENDED limit L = max(C, min(k x C, C + 1 GiB)). Under the
    # booking rule the first run's F is below 95% of its limit, so it reads as a real peak: a
    # one-time step to 1.15 x F, then stable. The worst case is F just under 0.95 x L: one step to
    # 1.15 x 0.95 x L = 1.0925 x min(k x C, C + 1 GiB). The last case is a large booking, where the
    # 1 GiB term binds and the step exceeds C + 1.15 GiB (C = 10 GiB steps to 12.01 GiB).
    print("  finite cache under L = max(C, min(k x C, C + 1 GiB)); bound 1.0925 x L of the first run:")
    for k, c0, files in ((2.0, 1.0, 1.5), (2.0, 1.0, 1.89), (1.5, 1.0, 1.42), (2.0, 10.0, 10.44)):
        c = c0
        seq = []
        for _ in range(6):
            limit = rule_limit(c, k)
            peak = min(limit, files)
            est = 1.15 * peak
            if peak >= 0.95 * limit:
                est = min(est, c)
            c = max(est, 0.0)
            seq.append(c)
        print("  k=%.1f C=%.0f GiB booking, finite cache %.2f GiB: %s (bound %.3f)" % (
            k, c0, files, " ".join("%.3f" % v for v in seq), 1.0925 * rule_limit(c0, k)))
    part5_rollback()


def rule_limit(c, k):
    """The recommended limit, in GiB: L = max(C, min(k x C, C + 1 GiB))."""
    return max(c, min(k * c, c + 1.0))


def part5_rollback():
    """k lowered after history was written at a higher k (a rollback to k = 1, or 2 -> 1.5).
    The job's need is ANONYMOUS and fixed at 1.95 GiB; C0 = 1 GiB. Three runs at k = 2, then three
    at the lowered k. Each run records (reserve, budget = its limit, peak, oom). The estimate is
    max over clean rows of 1.15 x effective peak, raised to 1.5 x the largest OOM peak when an
    OOM is retained (D2), never below C0.
      master     no headroom ever (k = 1 throughout): the reference
      rev2       a clean row at >= 95% of a limit above its booking counts as at most its booking
      rev3       the same, ONLY while the current rule's limit for that row's booking is at least
                 the limit it ran under (rule_limit(reserve, k_now) >= budget); else read raw"""
    need = 1.95
    print("  k lowered after history written at k = 2 (anonymous need %.2f GiB, C0 = 1 GiB):" % need)
    for k_low in (1.0, 1.5):
        for name in ("master", "rev2", "rev3"):
            hist = []
            out = []
            for i in range(6):
                k_now = 1.0 if name == "master" else (2.0 if i < 3 else k_low)
                est = 1.0
                for reserve, budget, peak, oom in hist:
                    if oom:
                        continue
                    eff = peak
                    if name != "master" and reserve < budget and peak >= 0.95 * budget:
                        if name == "rev2" or rule_limit(reserve, k_now) >= budget:
                            eff = min(peak, reserve / 1.15)
                    est = max(est, 1.15 * eff)
                ooms = [p for _r, _b, p, o in hist if o]
                if ooms:
                    est = max(est, 1.5 * max(ooms))
                limit = rule_limit(est, k_now)
                oom = need > limit
                peak = limit if oom else need
                hist.append((est, limit, peak, oom))
                out.append("%.2f/%.2f%s" % (est, limit, " OOM" if oom else ""))
            print("    lowered to k=%.1f %-6s booking/limit per run: %s" % (k_low, name, "  ".join(out)))


def part6(rows):
    """The plan gate's alternative to the physical-fit term: an additive bound, L = min(k x C, C + H).
    Uses the same within-k proxy as part 2 (largest clean peak above the killed cap)."""
    MiB = 1 << 20
    clean = collections.defaultdict(list)
    for r in rows:
        if not r[3] and r[2]:
            clean[r[1]].append(r[2])
    auto = [r for r in rows if r[3] and r[5] and r[6] == AUTO]
    print("\nPART 6  additive alternative L = min(k x C, C + H): auto OOM rows by killed cap, and the within proxy")
    edges = [(0, 256 * MiB, "<256M"), (256 * MiB, GiB, "256M-1G"), (GiB, GiB + 1, "==1G"),
             (GiB + 1, 2 * GiB, "1G-2G"), (2 * GiB, 4 * GiB, "2G-4G"), (4 * GiB, 8 * GiB, "4G-8G"),
             (8 * GiB, 1 << 62, ">=8G")]
    for lo, hi, nm in edges:
        sel = [r for r in auto if lo <= r[5] < hi]
        print("    killed cap %-8s %s" % (nm, pct(len(sel), len(auto))))

    def within(r, lim):
        ps = [p for p in clean.get(r[1], []) if p > r[5]]
        return bool(ps) and max(ps) <= lim

    base_n = sum(1 for r in auto if within(r, 2 * r[5]))
    print("    within at L = 2C: %s" % pct(base_n, len(auto)))
    for h in (256 * MiB, 512 * MiB, GiB, 2 * GiB):
        n = sum(1 for r in auto if within(r, min(2 * r[5], r[5] + h)))
        print("    within at L = min(2C, C + %4dM): %s; %.1f%% of the L = 2C figure" % (
            h // MiB, pct(n, len(auto)), 100.0 * n / base_n if base_n else 0))
    jobs = [r for r in rows if r[6] == AUTO and r[5] and r[4] >= RECENT and (r[3] or r[2])]
    sc = sum(r[5] for r in jobs)
    print("    auto jobs since %s: %d; total headroom sum(L - C) / sum(C): L = 2C 1.00; min(2C, C + 1G) %.2f; min(2C, C + 2G) %.2f" % (
        RECENT, len(jobs), sum(min(r[5], GiB) for r in jobs) / sc, sum(min(r[5], 2 * GiB) for r in jobs) / sc))


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
    part6(rows)


if __name__ == "__main__":
    main()
