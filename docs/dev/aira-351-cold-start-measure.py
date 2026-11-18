#!/usr/bin/env python3
"""AIRA-351 step 0: would warm-starting a cold confine subject from the same repo's history
for the same command shape (option A) have reduced over-booking, and at what false-low risk?

Pre-registered rule: decision-rule.md (written before this script ran).

READ-ONLY on a backup copy of the live store (opened mode=ro). Usage:
    aira confine --name aira351-measure -- python3 -I measure.py [state-copy.db]

Walk-forward: every job is judged only from rows that FINISHED before the job STARTED
(started_at_ms, AIRA-323; before AIRA-323 the end time `at` stands in for the start, which
is optimistic). Baseline = what today's code does (see decision-rule.md for file:line):
  - cold = unpinned (budget_basis cap:auto:daemon-reserve) with < 3 usable own samples;
  - the cold booking is the row's recorded budget (scope memory.max = the reserve).
Option A estimate = 1.15 x max peak over same-repo, same-shape rows (own + siblings), >= 3
usable samples, at least one from a different exact signature; then today's own-max floor.
"""
import bisect
import collections
import glob
import hashlib
import os
import re
import sqlite3
import sys
from datetime import datetime, timezone

DB = sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser("~/tmp/aira-351/state-copy.db")
GIB = float(1 << 30)
MIN_SAMPLES = 3            # internal/runner/resource_estimate.go:11
GROW = 1.15                # internal/runner/resource_estimate.go:12 (memoryEstimateSafetyPct=15)
RETENTION = 20             # internal/store/confine_peak_history.go:13
AUTO = "cap:auto:daemon-reserve"
PARENT = "aitest-parent"   # internal/runner/resource_estimate.go:77

WT = re.compile(r"/home/[^/\x00]+/(?:tmp|claude)/[^/\s:=,\x00]+|/tmp/[^/\s:=,\x00]+")


def ms_of(iso):
    s = iso.rstrip("Z")
    if "." in s:
        head, frac = s.split(".", 1)
        s = head + "." + frac[:6]
    return int(datetime.fromisoformat(s).replace(tzinfo=timezone.utc).timestamp() * 1000)


def unwrap(tk):
    i = 0
    while i < len(tk) and (tk[i] in ("env", "/usr/bin/env") or re.match(r"[A-Za-z_][A-Za-z0-9_]*=", tk[i])):
        i += 1
    return tk[i:] if i < len(tk) else tk


def k1(sig):
    """Primary shape: env-unwrapped argv, worktree-root path prefixes masked."""
    return "\x00".join(WT.sub("<wt>", t) for t in unwrap(sig.split("\x00")))


def k2(sig):
    """Loose shape (AIRA-329 mask): every path-like token and long number masked."""
    out = []
    for t in unwrap(sig.split("\x00")):
        if re.fullmatch(r"[0-9a-f]{7,}|[0-9]+", t):
            out.append("<n>")
        elif "/" in t or t.endswith((".py", ".rs", ".sh", ".txt", ".json", ".toml")):
            out.append("<p>")
        elif re.search(r"[0-9]{3,}", t):
            out.append("<n>")
        else:
            out.append(t)
    return "\x00".join(out)


def repo_labels():
    labels = {"none": "(not a git repo)"}
    for d in glob.glob(os.path.expanduser("~/*/.git")) + glob.glob(os.path.expanduser("~/claude/*/.git")) \
            + glob.glob(os.path.expanduser("~/*/*/.git")):
        if os.path.isdir(d):
            p = os.path.realpath(d)
            labels[hashlib.sha256(p.encode()).hexdigest()[:16]] = os.path.basename(os.path.dirname(p))
    return labels


def p90(vals):
    s = sorted(vals)
    return s[min(len(s) - 1, int(0.9 * (len(s) - 1) + 0.999999))]


class Pool:
    """Rows of one (repo, shape) key, ordered by finish time, for prefix queries."""

    def __init__(self):
        self.rows = []  # (at_ms, peak, sig)

    def freeze(self):
        self.rows.sort()
        self.at = [r[0] for r in self.rows]
        self.pmax, self.n = [], []
        m = n = 0
        for _, peak, _ in self.rows:
            m, n = max(m, peak), n + 1
            self.pmax.append(m)
            self.n.append(n)
        # prefix count of distinct signatures (to require a sibling, not only own history)
        seen, self.nsig = set(), []
        for _, _, sig in self.rows:
            seen.add(sig)
            self.nsig.append(len(seen))

    def before(self, t):
        return bisect.bisect_left(self.at, t)


def main():
    con = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)
    labels = repo_labels()
    rows = []
    for kind, sig, peak, oom, at, budget, basis, wall, start, repo in con.execute(
            "SELECT kind,signature,peak_rss,oom,at,budget,budget_basis,wall_ms,started_at_ms,repo "
            "FROM confine_peak_history"):
        end = ms_of(at)
        st = start if start is not None else (end - wall if wall is not None else end)
        rows.append((kind, sig, peak or 0, oom, end, budget, basis, wall, st, repo))
    con.close()
    conf = [r for r in rows if r[0] == "confine"]
    workers = [r for r in rows if r[0] == "pytest-worker"]

    # own history per exact signature
    own = collections.defaultdict(list)
    for r in conf:
        own[r[1]].append(r)
    for v in own.values():
        v.sort(key=lambda r: r[4])
    own_at = {s: [r[4] for r in v] for s, v in own.items()}
    own_usable_prefix = {}
    for s, v in own.items():
        c, out, m, mx = 0, [], 0, []
        for r in v:
            if r[2] > 0:
                c += 1
                m = max(m, r[2])
            out.append(c)
            mx.append(m)
        own_usable_prefix[s] = (out, mx)

    def own_prior(sig, t):
        i = bisect.bisect_left(own_at[sig], t)
        if i == 0:
            return 0, 0
        out, mx = own_usable_prefix[sig]
        return out[i - 1], mx[i - 1]

    repo_era = min(r[4] for r in conf if r[9] is not None)
    last = max(r[4] for r in conf)

    # pools: (repo, K1) and (repo, K2) over repo-era rows; repo-blind K1/K2 over all rows
    pools = {name: collections.defaultdict(Pool) for name in ("repoK1", "repoK2", "blindK1", "blindK2")}
    for r in conf:
        if r[2] <= 0:
            continue
        item = (r[4], r[2], r[1])
        a, b = k1(r[1]), k2(r[1])
        if r[9] not in (None, "none"):
            pools["repoK1"][(r[9], a)].rows.append(item)
            pools["repoK2"][(r[9], b)].rows.append(item)
        pools["blindK1"][a].rows.append(item)
        pools["blindK2"][b].rows.append(item)
    for p in pools.values():
        for pool in p.values():
            pool.freeze()

    def warm(poolmap, key, t, own_n, want_p90):
        pool = poolmap.get(key)
        if pool is None:
            return None
        i = pool.before(t)
        if i == 0 or pool.n[i - 1] < MIN_SAMPLES or pool.nsig[i - 1] < 2 or pool.n[i - 1] <= own_n:
            return None
        est_max = GROW * pool.pmax[i - 1]
        if not want_p90:
            return est_max, est_max, pool.n[i - 1]
        vals = [pool.rows[j][1] for j in range(i)]
        return est_max, GROW * p90(vals), pool.n[i - 1]

    def classify(r):
        """None if not an unpinned job; 'ambiguous' if retention hides its priors; else
        (cold?, own_n, own_max)."""
        if r[6] != AUTO or r[5] is None:
            return None
        n, mx = own_prior(r[1], r[8])
        if n < MIN_SAMPLES and len(own[r[1]]) >= RETENTION:
            return "ambiguous"
        return (n < MIN_SAMPLES, n, mx)

    def gibh(budget, wall):
        return budget / GIB * wall / 3.6e6

    out = []
    P = out.append
    P("AIRA-351 step 0 measurement. DB=%s" % DB)
    P("rows: confine=%d pytest-worker=%d; repo era (first row with repo) = %s; last row = %s"
      % (len(conf), len(workers), datetime.fromtimestamp(repo_era / 1000, timezone.utc).isoformat(),
         datetime.fromtimestamp(last / 1000, timezone.utc).isoformat()))

    def window_report(title, sel, poolname, shape, with_hours):
        days = (last - min(r[8] for r in sel)) / 86400000.0 if sel else 0
        P("")
        P("=== %s: %d confine rows, %.2f days, shape=%s pool=%s" % (title, len(sel), days, shape.__name__, poolname))
        auto = amb = cold = 0
        tot_h = cold_h = 0.0
        ctr = collections.Counter()
        rep = collections.defaultdict(lambda: collections.Counter())
        lower_saved = lower_saved_p90 = both_net = raise_extra = 0.0
        lowered = lowered_kill = lowered_kill_p90 = lowered_p90 = 0
        lowered_newkill = 0
        cold_slack = 0.0
        raised = raised_oom = 0
        cold_oom = 0
        cov = 0
        ratios = []
        examples_kill = []
        nohours = 0
        for r in sel:
            if with_hours and r[5] and r[7] is not None:
                tot_h += gibh(r[5], r[7])
            c = classify(r)
            if c is None:
                continue
            auto += 1
            if c == "ambiguous":
                amb += 1
                continue
            is_cold, own_n, own_max = c
            if not is_cold:
                continue
            cold += 1
            parent = r[1].split("\x00", 1)[0] == PARENT
            label = labels.get(r[9], r[9] or "(no repo column)")
            grp = ("parent " if parent else "") + label
            rep[grp]["cold"] += 1
            if r[3]:
                cold_oom += 1
                rep[grp]["cold_oom"] += 1
            h_ok = with_hours and r[7] is not None
            if with_hours and not h_ok:
                nohours += 1
            if h_ok:
                cold_h += gibh(r[5], r[7])
                if r[2] > 0 and not r[3]:
                    cold_slack += gibh(max(0, r[5] - r[2]), r[7])
            ctr["budget_MiB=%d" % (r[5] >> 20)] += 1
            if poolname.startswith("repo"):
                if r[9] in (None, "none"):
                    rep[grp]["no_repo"] += 1
                    continue
                key = (r[9], shape(r[1]))
            else:
                key = shape(r[1])
            w = warm(pools[poolname], key, r[8], own_n, with_hours)
            if w is None:
                continue
            cov += 1
            rep[grp]["covered"] += 1
            est_max, est_p90, _ = w
            floor = GROW * own_max if own_max else 0
            est_max, est_p90 = max(est_max, floor), max(est_p90, floor)
            booked = r[5]
            peak = r[2]
            if peak > 0 and not r[3]:
                ratios.append(est_max / peak)
            # A-lower
            if est_max < booked:
                lowered += 1
                rep[grp]["lowered"] += 1
                if h_ok:
                    lower_saved += gibh(booked - est_max, r[7])
                    rep[grp]["saved_mGiBh"] += int(1000 * gibh(booked - est_max, r[7]))
                if peak > est_max and not r[3]:
                    lowered_newkill += 1
                if peak > est_max or r[3]:
                    lowered_kill += 1
                    rep[grp]["lowered_kill"] += 1
                    if len(examples_kill) < 8:
                        examples_kill.append((peak >> 20, int(est_max) >> 20, booked >> 20,
                                              k1(r[1]).replace("\x00", " ")[:110]))
            if est_p90 < booked:
                lowered_p90 += 1
                if h_ok:
                    lower_saved_p90 += gibh(booked - est_p90, r[7])
                if peak > est_p90 or r[3]:
                    lowered_kill_p90 += 1
            # A-both
            if h_ok:
                both_net += gibh(booked - est_max, r[7])
            if est_max > booked:
                raised += 1
                if r[3]:
                    raised_oom += 1
                if h_ok:
                    raise_extra += gibh(est_max - booked, r[7])
        P("unpinned (cap:auto) rows=%d  retention-ambiguous=%d  cold=%d  cold OOM-killed=%d (%.1f%%)"
          % (auto, amb, cold, cold_oom, 100.0 * cold_oom / cold if cold else 0))
        P("cold booking sizes (top): %s" % ctr.most_common(4))
        P("cold with a same-shape warm pool (>=3 samples, >=1 sibling signature) = covered: %d (%.1f%% of cold)"
          % (cov, 100.0 * cov / cold if cold else 0))
        P("A-lower (max x1.15): lowered=%d, would-be kills (peak>est or OOM)=%d (%.2f%% of lowered)"
          % (lowered, lowered_kill, 100.0 * lowered_kill / lowered if lowered else 0))
        P("  of those, NEW kills (job was NOT OOM-killed at its cold booking today): %d (%.2f%% of lowered)"
          % (lowered_newkill, 100.0 * lowered_newkill / lowered if lowered else 0))
        P("A-lower (p90 x1.15): lowered=%d, would-be kills=%d (%.2f%%)"
          % (lowered_p90, lowered_kill_p90, 100.0 * lowered_kill_p90 / lowered_p90 if lowered_p90 else 0))
        P("A-both (max x1.15): raised=%d (of which OOM-killed at their cold booking: %d)" % (raised, raised_oom))
        if ratios:
            s = sorted(ratios)
            P("warm estimate / real peak, non-OOM covered jobs: median %.2f, p10 %.2f, p90 %.2f"
              % (s[len(s) // 2], s[len(s) // 10], s[(9 * len(s)) // 10]))
        if with_hours:
            P("booked GiB-h: all confine rows %.1f (%.1f/day); cold rows %.1f (%.1f/day); cold rows w/o wall_ms: %d"
              % (tot_h, tot_h / days, cold_h, cold_h / days, nohours))
            P("UPPER BOUND for any cold-sizing scheme: sum (booked - real peak) x wall over non-OOM cold rows = %.2f GiB-h (%.2f/day)"
              % (cold_slack, cold_slack / days))
            P("A-lower saved: max %.2f GiB-h (%.2f/day, %.1f%% of cold GiB-h); p90 %.2f GiB-h (%.2f/day)"
              % (lower_saved, lower_saved / days, 100 * lower_saved / cold_h if cold_h else 0,
                 lower_saved_p90, lower_saved_p90 / days))
            P("A-both: net saved %.2f GiB-h (%.2f/day); raises add %.2f GiB-h" % (both_net, both_net / days, raise_extra))
            P("decision inputs: G1 coverage %.1f%% (>=30); G2 %.2f GiB-h/day (>=24) and %.1f%% of cold GiB-h (>=10);"
              " G3 kills %.2f%% of lowered (<=1) and %.2f/day (<=2)"
              % (100.0 * cov / cold if cold else 0, lower_saved / days,
                 100 * lower_saved / cold_h if cold_h else 0,
                 100.0 * lowered_kill / lowered if lowered else 0, lowered_kill / days))
        P("by repo (cold, cold_oom, no_repo, covered, lowered, lowered_kill, saved_GiBh):")
        for grp, c in sorted(rep.items(), key=lambda kv: -kv[1]["cold"])[:14]:
            P("  %-34s %5d %4d %5d %5d %5d %4d %8.2f" % (grp[:34], c["cold"], c["cold_oom"], c["no_repo"], c["covered"],
                                                       c["lowered"], c["lowered_kill"], c["saved_mGiBh"] / 1000.0))
        if examples_kill:
            P("would-be kills (peak MiB, est MiB, booked MiB, K1 shape):")
            for e in examples_kill:
                P("  %r" % (e,))
        return days

    win = [r for r in conf if r[8] >= repo_era]
    window_report("REPO WINDOW (AIRA-323 era)", win, "repoK1", k1, True)
    window_report("REPO WINDOW, loose shape (sensitivity)", win, "repoK2", k2, True)
    window_report("REPO WINDOW, repo-blind K1 (what canonicalisation alone buys)", win, "blindK1", k1, True)
    whole = [r for r in conf if r[6] is not None]
    window_report("WHOLE HISTORY with a budget_basis, repo-blind K1 PROXY (no repo before %s; no wall_ms -> counts only)"
                  % datetime.fromtimestamp(repo_era / 1000, timezone.utc).date(), whole, "blindK1", k1, False)

    # context: how much of today's cold-booking waste is cold at all, and what would a bare default change do
    P("")
    P("=== CONTEXT (not in the decision rule): repo window, all cold rows, lowering the cold default itself")
    cold_rows = []
    for r in win:
        c = classify(r)
        if c and c != "ambiguous" and c[0]:
            cold_rows.append(r)
    days = (last - min(r[8] for r in win)) / 86400000.0
    for default in (768 << 20, 512 << 20, 256 << 20):
        saved = kills = n = 0
        for r in cold_rows:
            if r[5] == 1 << 30:
                n += 1
                if r[7] is not None:
                    saved += gibh((1 << 30) - default, r[7])
                if r[2] > default or r[3]:
                    kills += 1
        P("cold default %d MiB instead of 1024: rows at 1024=%d, saved %.2f GiB-h/day, would-be kills %d (%.1f%%)"
          % (default >> 20, n, saved / days, kills, 100.0 * kills / n if n else 0))
    peaks = sorted(r[2] for r in cold_rows if r[2] > 0 and not r[3])
    if peaks:
        P("cold non-OOM real peak MiB: p10 %d, p50 %d, p90 %d, p99 %d" % tuple(
            peaks[int(q * (len(peaks) - 1))] >> 20 for q in (0.1, 0.5, 0.9, 0.99)))

    P("")
    P("=== aitest WORKER pools (kind pytest-worker): NOT sized from history today (worker reserve ="
      " overhead env + @aira_mem), shown as context only")
    wwin = [r for r in workers if r[4] >= repo_era]
    for title, sel in (("repo window", wwin), ("whole history", workers)):
        withb = [r for r in sel if r[5] and r[2] > 0]
        over = sum(1 for r in withb if r[5] > 2 * r[2])
        basis = collections.Counter(r[6] for r in sel)
        keys = collections.Counter(r[1] for r in sel)
        roots = collections.Counter(r[1].split("\x1f", 1)[0] for r in sel)
        args_shapes = collections.Counter(WT.sub("<wt>", r[1].split("\x1f", 1)[1] if "\x1f" in r[1] else "") for r in sel)
        P("%s: pool runs=%d distinct keys=%d distinct rootdirs=%d distinct args (wt-masked)=%d;"
          " runs whose largest worker cap > 2x pool peak: %d of %d; repo column set: %d; wall_ms set: %d"
          % (title, len(sel), len(keys), len(roots), len(args_shapes), over, len(withb),
             sum(1 for r in sel if r[9]), sum(1 for r in sel if r[7] is not None)))
        P("  budget basis: %s" % basis.most_common(4))
        shp = collections.Counter((r[6], (r[5] or 0) >> 20, WT.sub("<wt>", r[1].split("\x1f", 1)[0])) for r in sel)
        P("  top (basis, largest worker cap MiB, rootdir wt-masked): %s" % shp.most_common(5))
    print("\n".join(out))


if __name__ == "__main__":
    main()
