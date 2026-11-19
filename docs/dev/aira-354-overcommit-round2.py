#!/usr/bin/env python3
"""AIRA-354 plan-fix round 2: the figures the collapse memo cites beyond the replay.

Spec: docs/superpowers/specs/2026-11-19-measured-overcommit-design.md (sections 3 and 4).
Imports the round-1 replay (docs/dev/aira-354-overcommit-replay.py) for its loaders and constants.
Read-only: every input is a copy (never point --db at the live state.db).

  aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-round2.py --selftest
  aira confine --memory-reserve 256M -- python3 -I docs/dev/aira-354-overcommit-round2.py \\
      --live <frozen list.tsv> [--db <state copy> --journal <journal copy>]

Part A (live hour, 180 `aira confine --list` samples, AIRA-324 enforce 0.9 on):
  - what the queue looked like (freeze phase, head size, 10-minute medians);
  - use / booking by job class (pinned, other scope, aitest worker);
  - queued jobs admitted at one instant, beyond the deployed ledger, under three queue models,
    each capped by free CPU slots (one slot per granted waiter, ESTIMATED):
      fifo    stops at the first job that does not fit (lower bracket);
      budget  the first misfit becomes the head; later fits are granted while the bytes that
              passed it stay within half the head's booking (admitBypassBudget,
              internal/daemon/admit.go:485-490 and 2687-2699). Static: nothing has passed the
              head yet, so it is biased up;
      skip    passes every misfit, no head rule (upper bracket);
    for: track 1 as deployed; a factor k on the full-pin ledger (k = 1.10, 1/0.9, 1.15, 1.25);
    and AIRA-324's 0.9 on every non-worker scope (workers stay at full, as AIRA-324 has them).
Part B (83 h store + journal): the replay's part-5 wait-hour model (FIFO, static per instant,
  RAM then RAM + CPU with 24 hidden jobs), with a fixed extra room standing for track 1 and for
  0.9 on every non-worker scope, and the factor rows net of track 1. An optimistic model that sees
  about half the load (replay docstring, LIMITS); every figure is an ESTIMATE. The fixed rooms are
  the live hour's p50 and max relief applied to all 83 h, a stand-in, not a per-instant figure.
Part C (store only): visible wait in equal windows before, during and after the 2026-11-16 0.9
  trial, and the timeline rows by budget basis. Confounded by load; a first look, not evidence.
Limits: "other" scopes are every non-pinned, non-worker scope (auto-sized jobs, but also
  `--memory-max` jobs, `--delegate-ram` parents and pins re-declared at full after the 00:10
  restart), so the 0.9-on-every-non-worker-scope relief and share are UPPER figures for a build
  that keeps those exclusions. Aitest workers are recognised by the `worker` flag or the
  `aitest-w` name prefix.
"""
import argparse
import importlib.util
import json
import math
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("replay", os.path.join(HERE, "aira-354-overcommit-replay.py"))
rp = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(rp)
GIB = rp.GIB
FACTOR = 0.9


def charge(b, factor=FACTOR):
    """AIRA-324's booking: clamp(ceil(factor x b), 1 MiB, b) (pin_charge.go discountedPinCharge)."""
    return max(1 << 20, min(b, math.ceil(factor * b)))


def is_worker(w):
    return bool(w.get("worker")) or (w.get("name") or "").startswith("aitest-w")


def admit(queued, room, phys, free, model, chargefn=lambda w: w["reserve_bytes"]):
    """Queued jobs (list order) granted from `room` ledger bytes; each job's full reserve must
    also fit `phys` (the physical-fit rule). See the module docstring for the three models."""
    n = used = byp = 0
    head = None
    for w in queued:
        if n >= free:
            break
        c = chargefn(w)
        fit = used + c <= room and w["reserve_bytes"] <= phys
        if model == "fifo":
            if not fit:
                break
            n, used = n + 1, used + c
        elif model == "skip":
            if fit:
                n, used = n + 1, used + c
        elif head is None:
            if fit:
                n, used = n + 1, used + c
            else:
                head = c
        elif fit and byp + c <= head // 2:
            n, used, byp = n + 1, used + c, byp + c
    return n


def stats(v):
    return "p50 %d mean %.2f max %d, none in %d of %d" % (rp.q(v, .5), sum(v) / len(v), max(v), sum(1 for n in v if n == 0), len(v))


def part_a(live):
    P = print
    recs = rp.live_recs(live)
    P("## A. Live hour: %d samples" % len(live))
    phases = {}
    for _, d in live:
        ph = d["slice_reserve"].get("freeze_phase")
        phases[ph] = phases.get(ph, 0) + 1
    P("freeze_phase counts: %s" % ", ".join("%s %d" % kv for kv in sorted(phases.items(), key=lambda kv: -kv[1])))
    heads = [d["queued"][0]["reserve_bytes"] / GIB for _, d in live if d.get("queued")]
    P("first queued row's reserve GiB: p10 %.2f p50 %.2f p90 %.2f max %.2f" % (rp.q(heads, .1), rp.q(heads, .5), rp.q(heads, .9), max(heads)))
    for i in range(0, len(live), 30):
        b = recs[i:i + 30]
        P("10-min block %d: queued p50 %d, jobs p50 %d, MemAvailable p50 %.1f GiB" % (
            i // 30 + 1, rp.q([x[4] for x in b], .5), rp.q([x[3] for x in b], .5), rp.q([x[6] for x in b], .5) / GIB))
    cls = {}
    for _, d in live:
        tot = {}
        for s in d["scopes"]:
            full = s.get("pin_bytes") or s["reserve_bytes"]
            c = "pinned" if s.get("pin_bytes") else ("worker" if is_worker(s) else "other")
            u, bk = tot.get(c, (0, 0))
            tot[c] = (u + (s.get("rss_bytes") or 0), bk + full)
        for c, v in tot.items():
            cls.setdefault(c, []).append(v)
    P("use / full booking by class (scope memory.current incl. cache over pin or reserve), per sample:")
    for c, v in sorted(cls.items()):
        r = [u / bk for u, bk in v if bk]
        P("  %-6s booking p50 %.2f GiB; use/booking p50 %.3f max %.3f" % (c, rp.q([bk for _, bk in v], .5) / GIB, rp.q(r, .5), max(r)))
    share = {"pins": [], "every non-worker scope": []}
    for _, d in live:
        sc = d["scopes"]
        tot = sum(s.get("pin_bytes") or s["reserve_bytes"] for s in sc)
        if tot:
            share["pins"].append(sum(s["pin_bytes"] for s in sc if s.get("pin_bytes")) / tot)
            share["every non-worker scope"].append(sum(s.get("pin_bytes") or s["reserve_bytes"] for s in sc if not is_worker(s)) / tot)
    for k, v in share.items():
        P("share of full bookings discounted, %s: p50 %.2f min %.2f max %.2f" % (k, rp.q(v, .5), min(v), max(v)))
    relief = {"track 1 (pins at 0.9)": [], "0.9 on every non-worker scope": [], "k = 1/0.9 extra": []}
    rows = {}
    for (t, d), x in zip(live, recs):
        L, phys, free = x[2], x[2] - x[5], max(0, rp.CPU_SLOTS - x[3])
        sc, q = d["scopes"], d.get("queued") or []
        dd = sum(s["pin_bytes"] - s["reserve_bytes"] for s in sc if s.get("pin_bytes"))
        full = x[1] + dd
        du = sum((s.get("pin_bytes") or s["reserve_bytes"]) - charge(s.get("pin_bytes") or s["reserve_bytes"]) for s in sc if not is_worker(s))
        hr = (x[10] - L) if x[10] else rp.H0
        relief["track 1 (pins at 0.9)"].append(dd)
        relief["0.9 on every non-worker scope"].append(du)
        cases = [("track 1 as deployed (queued rows at full)", min(L - x[1], phys), None)]
        for k in (1.10, 1 / 0.9, 1.15, 1.25):
            limit = min(int(k * L), (x[9] - hr) if x[9] else int(k * L), 2 * L - x[5])
            if abs(k - 1 / 0.9) < 1e-9:
                relief["k = 1/0.9 extra"].append(limit - L)
            cases.append(("k=%.3f on the full-pin ledger" % k, min(limit - full, phys), None))
        cases.append(("0.9 on every non-worker scope", min(L - (full - du), phys),
                      lambda w: w["reserve_bytes"] if is_worker(w) else charge(w["reserve_bytes"])))
        for name, room, cf in cases:
            for m in ("fifo", "budget", "skip"):
                rows.setdefault((name, m), []).append(admit(q, room, phys, free, m, cf or (lambda w: w["reserve_bytes"])))
    P("ledger relief against the full-pin ledger, GiB:")
    for k, v in relief.items():
        P("  %-32s p50 %.2f max %.2f" % (k, rp.q(v, .5) / GIB, max(v) / GIB))
    P("queued jobs admitted at one instant beyond what was granted (CPU-capped):")
    for (name, m), v in rows.items():
        P("  %-42s %-6s %s" % (name, m, stats(v)))


def part_b(db, journal):
    P = print
    jobs, pools, unknown = rp.load_jobs(db)
    with open(journal, errors="replace") as f:
        rows = rp.load_journal(f)
    series = rp.Series(rows)
    lo = min(j.s for j in jobs) + 3600
    hi = min(max(j.e for j in jobs), rows[-1][0]) - 3600
    grid = [(t, run, qd, series.at(t)) for t, run, qd in rp.sweep(jobs, lo, hi) if series.at(t) is not None]
    waited = [j for j in jobs if lo <= j.a < hi and j.s - j.a >= 1]
    total = sum(j.s - j.a for j in waited) / 3600
    P("\n## B. Wait-hours (replay part-5 model, %d grid instants, %d jobs that waited, %.1f wait-hours)" % (len(grid), len(waited), total))

    def removed(roomfn, cpu):
        first = {}
        for t, running, queued, v in grid:
            slots = max(0, rp.CPU_SLOTS - len(running) - 24) if cpu else None
            for j in rp.admit_prefix(queued, roomfn(*v), True, slots):
                first.setdefault(id(j), t)
        return sum(max(0, j.s - first[id(j)]) for j in waited if id(j) in first) / 3600

    for cpu in (False, True):
        tag = "RAM+CPU" if cpu else "RAM    "
        fixed = {}
        for name, room in (("track 1 p50", 1.41), ("track 1 max", 3.24), ("0.9 non-worker p50", 3.50), ("0.9 non-worker max", 4.88)):
            fixed[name] = removed(lambda E, U, MA, M, r=room: r * GIB, cpu)
            P("%s fixed room %.2f GiB (%s): removed %.1f h (%.0f%%)" % (tag, room, name, fixed[name], 100 * fixed[name] / total))
        for lab, a, b in (("paired p50", "0.9 non-worker p50", "track 1 p50"), ("paired max", "0.9 non-worker max", "track 1 max"),
                          ("crossed low", "0.9 non-worker p50", "track 1 max"), ("crossed high", "0.9 non-worker max", "track 1 p50")):
            P("%s 0.9 on every non-worker scope, net of track 1 (%s rooms): %.1f h (%.0f%%)" % (tag, lab, fixed[a] - fixed[b], 100 * (fixed[a] - fixed[b]) / total))
        for k in (1.10, 1 / 0.9, 1.15, 1.25):
            r = removed(lambda E, U, MA, M, k=k: rp.extra(k, E, U, M, True), cpu)
            P("%s k=%.3f: removed %.1f h; net of track 1 %.1f to %.1f h (%.0f%% to %.0f%%)" % (
                tag, k, r, r - fixed["track 1 max"], r - fixed["track 1 p50"],
                100 * (r - fixed["track 1 max"]) / total, 100 * (r - fixed["track 1 p50"]) / total))


TRIAL = (1794846960, 1794873900)   # AIRA-324 enforce 0.9: 2026-11-16 16:36 to 2026-11-17 00:05 UTC


def part_c(db):
    """Visible wait before, during and after the 0.9 trial, equal windows. A first look only:
    the load differs between the windows, so this is not a measured effect of the discount."""
    import sqlite3
    P = print
    w = TRIAL[1] - TRIAL[0]
    con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    P("\n## C. Store rows (started_at_ms, wait_ms, wall_ms) around the 0.9 trial, %.2f h windows" % (w / 3600))
    for name, lo, hi in (("before", TRIAL[0] - w, TRIAL[0]), ("trial", TRIAL[0], TRIAL[1]), ("after", TRIAL[1], TRIAL[1] + w)):
        n, wait, waited, run = con.execute(
            "select count(*), coalesce(sum(wait_ms), 0), sum(case when wait_ms >= 1000 then 1 else 0 end), coalesce(sum(wall_ms), 0) "
            "from confine_peak_history where wait_ms is not null and started_at_ms >= ? and started_at_ms < ?",
            (lo * 1000, hi * 1000)).fetchone()
        P("%-6s visible jobs %d, waited >= 1 s %d, wait-hours %.1f (mean %.1f s), run-hours %.1f" % (
            name, n, waited or 0, wait / 3.6e6, wait / 1000 / n if n else 0, run / 3.6e6))
    P("timeline rows by budget basis (started_at_ms and wall_ms present):")
    for basis, n in con.execute("select coalesce(budget_basis, '(none)'), count(*) from confine_peak_history "
                                "where started_at_ms is not null and wall_ms is not null group by 1 order by 2 desc"):
        P("  %-34s %d" % (basis, n))


def selftest():
    fails = 0

    def check(name, got, want):
        nonlocal fails
        ok = got == want
        fails += not ok
        print("%s %s: got %r want %r" % ("ok  " if ok else "FAIL", name, got, want))

    G = GIB
    qd = [{"reserve_bytes": 4 * G}, {"reserve_bytes": 1 * G}, {"reserve_bytes": 1 * G}, {"reserve_bytes": 1 * G}]
    check("fifo stops at the head", admit(qd, 3 * G, 9 * G, 9, "fifo"), 0)
    # head 4 GiB: budget 2 GiB, so two of the three 1 GiB jobs pass (a budget of the whole head would give 3)
    check("budget is half the head", admit(qd, 3 * G, 9 * G, 9, "budget"), 2)
    check("skip passes every fit", admit(qd, 3 * G, 9 * G, 9, "skip"), 3)
    check("CPU slots cap", admit(qd, 9 * G, 9 * G, 2, "skip"), 2)
    check("physical fit on the full reserve, not the charge", admit([{"reserve_bytes": 4 * G}], 9 * G, 3.8 * G, 9, "fifo", lambda w: charge(w["reserve_bytes"])), 0)
    check("charge rounds up", charge(10), 1 << 20)
    check("charge is 0.9 of a pin", charge(10 * G), math.ceil(9 * G))
    check("a worker is never discounted", is_worker({"name": "aitest-w12"}), True)
    print("selftest: %d failures" % fails)
    return fails


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--live")
    ap.add_argument("--db")
    ap.add_argument("--journal")
    ap.add_argument("--selftest", action="store_true")
    a = ap.parse_args()
    if a.selftest:
        sys.exit(1 if selftest() else 0)
    if a.live:
        live = []
        with open(a.live) as f:
            for line in f:
                t, _, js = line.partition("\t")
                try:
                    live.append((int(t), json.loads(js)["data"]))
                except (ValueError, KeyError):
                    pass
        part_a(live)
    if a.db and a.journal:
        part_b(a.db, a.journal)
    if a.db:
        part_c(a.db)


if __name__ == "__main__":
    main()
