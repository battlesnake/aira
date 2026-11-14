#!/usr/bin/env python3
"""Walk-forward replay behind docs/superpowers/specs/2026-11-13-history-charge-design.md.

Reads ~/.local/state/aira/state.db READ-ONLY (confine_peak_history). Every job is
predicted only from jobs that finished EARLIER. Two parts:

  part 1  coverage and under-prediction rate of five command keys (exact argv ..
          command name) at n>=3 samples, predictor max*1.15 (today's estimator).
  part 2  for key chains x predictors, jobs finished since RECENT: coverage, the
          share of jobs whose actual peak exceeded the prediction, the capacity
          multiplier (sum of pins / sum of charges), and a Monte-Carlo of 20
          concurrent jobs: P(sum of peaks > sum of charges), mixed jobs and 20 jobs
          of one command+subcommand ("burst").

  part 3  the challenge row: a FLAT discount (charge = factor x pin, no history at all)
          against history and the combination, on --memory-reserve pins only.

Run (confined, ~1G):  aira confine --memory-reserve 1G -- python3 -I docs/dev/aira-history-charge-replay.py

Measured 2026-11-13/14 on the live box. LIMITS: the table holds job-END rows only (no
start time or duration), so (a) real clustering in time is NOT captured -- jobs are
drawn independently and not duration-weighted; (b) rows are ordered by END time, so a job
is predicted from rows that finished while it was still running (optimistic, worst for
same-command bursts); (c) store retention is 20 rows per EXACT signature, so a coarse key
sees more history here than the build's window query would. A peak is the cgroup's
memory.peak. "Above" is not an OOM: the pin stays the hard cap. After AIRA-323, replay by
started_at and weight by duration.
"""
import collections
import os
import random
import re
import sqlite3

DB = os.path.expanduser("~/.local/state/aira/state.db")
RECENT = "2026-10-20"
c = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)



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


TIME_ARG = {"-o", "-f", "--output", "--format"}          # options that take a value
TIME_FLAG = {"-p", "-v", "-a", "--portability", "--verbose", "--append"}


def unwrap(tk):
    """Strip leading `env` + VAR=val and `time` + its KNOWN options; anything else (including
    an unrecognised time option) leaves the argv as is. aitest-parent, timeout, nice, flock
    are never unwrapped."""
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
            return tk  # unknown option: do not guess
        break
    return tk[i:] or tk


def keys(sig):
    tk = sig.split("\x00")
    u = unwrap(tk)
    a0 = base(u[0])
    rest = [mask(t) for t in u[1:]]
    pos = [t for t in rest if not t.startswith("-")]
    return {
        "N0_exact": sig,
        "N1_masked_all": a0 + "|" + " ".join(rest[:14]),
        "N2_first4": a0 + "|" + " ".join(rest[:3]),
        "N3_cmd+sub": a0 + "|" + (pos[0] if pos else ""),
        "N4_cmd": a0,
    }


LV = ["N0_exact", "N1_masked_all", "N2_first4", "N3_cmd+sub", "N4_cmd"]


def q(h, p):
    s = sorted(h)
    return s[min(len(s) - 1, int(p * len(s)))]


PRED = {
    "max*1.15": lambda h: max(h) * 1.15,
    "p90": lambda h: q(h, 0.90),
    "p90*1.25": lambda h: q(h, 0.90) * 1.25,
    "max*1.25": lambda h: max(h) * 1.25,
}
CHAINS = {
    "N1>N2>N3": ["N1_masked_all", "N2_first4", "N3_cmd+sub"],
    "N2>N3": ["N2_first4", "N3_cmd+sub"],
    "N2 only": ["N2_first4"],
}
# OOM rows are INCLUDED: a key with any OOM in its last 20 rows is vetoed (charged its pin), as the
# spec requires. Only clean rows (oom=0, peak and budget known) are scored.
SQL = ("select signature,peak_rss,budget,budget_basis,oom,at from confine_peak_history "
       "where kind='confine' order by at,rowid")


def push(hist, ks, peak, oom):
    for l in LV:
        lst = hist[l][ks[l]]
        lst.append((peak, oom))
        if len(lst) > 20:
            del lst[0]  # the store keeps the newest 20 per key


def usable(h):
    """clean peaks of a key's history, or None when it must not be predicted from:
    fewer than 3 clean samples, or any OOM on record in the window."""
    if any(o for _p, o in h):
        return None
    peaks = [p for p, o in h if p]
    return peaks if len(peaks) >= 3 else None


def part1():
    hist = {l: collections.defaultdict(list) for l in LV}
    res = collections.defaultdict(list)
    tot = collections.Counter()
    for sig, peak, bud, bb, oom, _at in c.execute(SQL):
        ks = keys(sig)
        scored = peak is not None and bud and not oom
        if scored:
            cls = "pinned" if "operator" in bb else "auto"
            tot[cls] += 1
            for l in LV:
                h = usable(hist[l].get(ks[l], []))
                if h:
                    res[(cls, l)].append((peak, min(max(h) * 1.15, 64 << 30), bud))
        push(hist, ks, peak, oom)
    print("PART 1  n>=3, predictor max*1.15")
    for (cls, l), x in sorted(res.items()):
        print("%-7s %-14s cover %.2f  above-prediction %5.2f%%  sum(pin)/sum(pred) %.2f" % (
            cls, l, len(x) / tot[cls], 100 * sum(a > p for a, p, b in x) / len(x),
            sum(b for a, p, b in x) / sum(p for a, p, b in x)))


def part2():
    hist = {l: collections.defaultdict(list) for l in LV}
    out = collections.defaultdict(list)
    for sig, peak, bud, bb, oom, at in c.execute(SQL):
        ks = keys(sig)
        scored = peak is not None and bud and not oom
        cls = "pinned" if scored and "operator" in bb else "auto"
        if scored and at >= RECENT:
            for cn, chain in CHAINS.items():
                hit = None
                for l in chain:
                    h = usable(hist[l].get(ks[l], []))
                    if h:
                        hit = h
                        break
                for pn, pf in PRED.items():
                    if hit is None:
                        out[(cls, cn, pn)].append((peak, bud, bud, ks["N3_cmd+sub"], False))
                    else:
                        # a pin is a cap: the charge may fall below it, never rise above it
                        ch = min(pf(hit), bud) if cls == "pinned" else pf(hit)
                        out[(cls, cn, pn)].append((peak, ch, bud, ks["N3_cmd+sub"], True))
        push(hist, ks, peak, oom)
    random.seed(1)

    def trial(pool, col):
        s = random.sample(pool, 20)
        return sum(t[0] for t in s) / sum(t[col] for t in s)

    print("\nPART 2  jobs since %s; K=20 concurrent" % RECENT)
    print("%-7s %-9s %-9s %6s %5s %7s %6s | mixed P(>1) p99 | burst P(>1) p99" % (
        "class", "chain", "pred", "jobs", "cover", "above%", "capx"))
    for (cls, cn, pn), x in sorted(out.items()):
        cov = sum(t[4] for t in x) / len(x)
        above = sum(t[0] > t[1] for t in x) / len(x)
        capx = sum(t[2] for t in x) / sum(t[1] for t in x)
        r = [trial(x, 1) for _ in range(2000)]
        pm, qm = sum(v > 1 for v in r) / len(r), sorted(r)[int(0.99 * len(r))]
        groups = collections.defaultdict(list)
        for t in x:
            groups[t[3]].append(t)
        big = [g for g in groups.values() if len(g) >= 60]
        r = [trial(random.choice(big), 1) for _ in range(2000)]
        pb, qb = sum(v > 1 for v in r) / len(r), sorted(r)[int(0.99 * len(r))]
        print("%-7s %-9s %-9s %6d %5.2f %7.2f %6.2f | %7.3f %5.2f | %7.3f %5.2f" % (
            cls, cn, pn, len(x), cov, 100 * above, capx, pm, qm, pb, qb))
    for cls in ("pinned", "auto"):
        x = out[(cls, "N2 only", "p90")]
        m = [trial(x, 2) for _ in range(2000)]
        print("%s BASELINE charge=pin: p99 sum(peak)/sum(pin) %.2f, max %.2f" % (cls, sorted(m)[int(0.99 * len(m))], max(m)))


def part3():
    """Challenge row: flat discount vs history, --memory-reserve pins only."""
    hist = {l: collections.defaultdict(list) for l in LV}
    rows = []
    for sig, peak, bud, bb, oom, at in c.execute(SQL):
        ks = keys(sig)
        scored = peak is not None and bud and not oom
        if scored and at >= RECENT and "operator:--memory-reserve" in bb:
            pred = None
            for l in CHAINS["N1>N2>N3"]:
                h = usable(hist[l].get(ks[l], []))
                if h:
                    pred = max(h) * 1.25
                    break
            rows.append((peak, bud, pred, ks["N3_cmd+sub"]))
        push(hist, ks, peak, oom)
    random.seed(1)

    def stats(name, charge):
        x = [(p, charge(b, pr), b, k) for p, b, pr, k in rows]
        above = sum(p > ch for p, ch, b, k in x) / len(x)
        capx = sum(b for p, ch, b, k in x) / sum(ch for p, ch, b, k in x)

        def trial(pool):
            s = random.sample(pool, 20)
            return sum(t[0] for t in s) / sum(t[1] for t in s)
        r = [trial(x) for _ in range(2000)]
        g = collections.defaultdict(list)
        for t in x:
            g[t[3]].append(t)
        big = [v for v in g.values() if len(v) >= 60]
        rb = [trial(random.choice(big)) for _ in range(2000)]
        print("%-30s above %5.2f%% capx %.2f | mixed P>1 %.3f p99 %.2f | burst P>1 %.3f p99 %.2f" % (
            name, 100 * above, capx, sum(v > 1 for v in r) / len(r), sorted(r)[int(.99 * len(r))],
            sum(v > 1 for v in rb) / len(rb), sorted(rb)[int(.99 * len(rb))]))
    print("\nPART 3  --memory-reserve pins only, %d jobs, chain coverage %.2f" % (len(rows), sum(1 for r in rows if r[2]) / len(rows)))
    for f in (0.9, 0.8, 0.7, 0.6, 0.5):
        stats("flat %.1f x pin" % f, lambda b, pr, f=f: b * f)
    stats("history max*1.25 (else pin)", lambda b, pr: min(b, pr) if pr else b)
    for f in (0.8, 0.7):
        stats("min(%.1f x pin, history)" % f, lambda b, pr, f=f: min(b * f, pr) if pr else b * f)


if __name__ == "__main__":
    part1()
    part2()
    part3()
