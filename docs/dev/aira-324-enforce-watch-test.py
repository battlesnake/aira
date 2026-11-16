#!/usr/bin/env python3
"""AIRA-324 enforce watch: end-to-end checks of docs/dev/aira-324-enforce-watch.sh.

Runs the watch against fakes only: a fake HOME holding a tiny store, a fake cgroup
directory, a fake meminfo, back-dated samples and a baseline in the fake watch directory,
and shims for journalctl, systemctl and aira on PATH. It never reads the live store, the
real journal or the real slice. Everything goes under a fresh directory in ~/tmp (or
$AIRA_324_WATCH_TEST_DIR), removed at the end.

    python3 -I docs/dev/aira-324-enforce-watch-test.py [path/to/aira-324-enforce-watch.sh]

The watch finds aira-324-enforce-gate-replay.py next to itself, so a copy of the watch
must sit next to a copy of the replay. Exit 0: every check passed; 1: a check failed.

The environment is cron-like (env -i, HOME and PATH only, no XDG_RUNTIME_DIR; the shims
fail like the real tools without it). Unless a case changes them, every case has a full,
quiet 2-hour sample history, a baseline (high p99 0, pressure p99 1.0), 40 GiB available
and no kills, so each case can assert its `met` list EXACTLY: a trigger that fires when it
should not, or stays silent when it should fire, fails the case.

5% trigger cases (--store; 40 discounted 10 GiB grants at 0.9, charge 9 GiB, 10 s apart):
  met        3 of 40 above, every grant paired: 7.5%
  not-met    2 of 40 = exactly 5%; an ineligible row 5 s from any line, and an undiscounted
             enforce line, are left out
  straddle   2 above, 1 grant with no row: [5%, 7.5%] is unevaluated
  decoy      review counterexample 1: the rowless grant is the third above-charge job, and an
             ineligible LOW row starts 300 ms from its line: still unevaluated, never 'not met'
  displace   review counterexample 2: exactly 5%, and an ineligible HIGH row starts nearer to a
             low grant than its own row: unevaluated, never 'met'
  parent     a delegate-parent row (never discounted) 1 ms from the rowless grant's line is
             not taken for its row
  evicted    3 of 40 above; a second --store run after the retention evicted an above row
             still counts it (rows.tsv)
  no-log     journalctl fails: log fields and the trigger are unevaluated, never 0
Trigger cases (one sample each): slice-level oom (first sample, rise, flat, reset), high
(2 hours, 1 hour, reset, gap), pressure (2 hours, a dip, gap), MemAvailable (low, missing),
a watchdog trip, a systemd-oomd kill or an unreadable oomd journal, an enforce factor of 0.80.
"""
import json
import os
import pathlib
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time

GIB = 1 << 30
PIN = 10 * GIB
CHARGE = 9 * GIB            # ceil(0.9 x 10 GiB)
BASIS = "cap:operator:--memory-reserve"
PARENT = "aitest-parent\x00make\x00test"

SHIM_NEEDS_XDG = """if [ -z "${XDG_RUNTIME_DIR:-}" ]; then echo "Failed to connect to bus: No medium found" >&2; exit 1; fi
"""
COUNTERS = ("oom_local", "max_local", "oom_kill_local", "oom_kill", "high")


def shim(path, body):
    path.write_text("#!/bin/sh\n" + body)
    path.chmod(0o755)


def history(now, high=None, avg=None, drop=None, oom=0):
    """25 samples 300 s apart ending 300 s ago; high(t) / avg(t) give each sample's value."""
    out = []
    for i in range(25):
        t = now - 7500 + 300 * i
        if drop and drop[0] < t < drop[1]:
            continue
        c = {"oom_local": oom, "max_local": 0, "oom_kill_local": 0, "oom_kill": 7, "high": high(t) if high else 0}
        out.append("%.0f\t%s\t%s\n" % (t, "\t".join(str(c[k]) for k in COUNTERS), avg(t) if avg else 0.1))
    return "".join(out)


def build(root, now, c):
    home = root / "home"
    (home / ".local/state/aira").mkdir(parents=True)
    wdir = home / "tmp/aira-324-watch"
    wdir.mkdir(parents=True)
    cg = root / "cg"
    cg.mkdir()
    (cg / "memory.events").write_text("low 0\nhigh %d\nmax 0\noom 0\noom_kill 7\n" % c.get("high", 0))
    (cg / "memory.events.local").write_text("low 0\nhigh 0\nmax 0\noom %d\noom_kill 0\n" % c.get("oom", 0))
    (cg / "memory.pressure").write_text("some avg10=%.2f avg60=0.00 avg300=0.00 total=1\n"
                                        "full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" % c.get("avg10", 0.1))
    meminfo = root / "meminfo"
    if c.get("memavail_kib", 40 * 1024 * 1024) is not None:
        meminfo.write_text("MemTotal: 82000000 kB\nMemAvailable: %d kB\n" % c.get("memavail_kib", 40 * 1024 * 1024))
    if c.get("samples", True) is not False:
        (wdir / "samples.tsv").write_text(c.get("samples") or history(now))
    if c.get("baseline", True):
        (wdir / "baseline.json").write_text(json.dumps({"high_p99_per_hour": 0, "some_avg10_p99": 1.0}))

    t1 = now - 3600
    grant_times = [t1 + 10 * i for i in range(40)]
    pc = ["%.6f host aira[1]: aira daemon: pin-charge observe: scope=S0 pin=%d charged=%d would-charge=%d "
          "factor=0.90 physical-free=1 physical-fit=true" % (t1 - 100, PIN, PIN, CHARGE)]
    for t in grant_times:
        pc.append("%.6f host aira[1]: aira daemon: pin-charge enforce: scope=S pin=%d charged=%d factor=0.90 "
                  "physical-free=%d physical-fit=true" % (t, PIN, CHARGE, 40 * GIB))
    # a non-discounted enforce grant (charged == pin): not in the population
    pc.append("%.6f host aira[1]: aira daemon: pin-charge enforce: scope=T pin=%d charged=%d factor=0.90 "
              "physical-free=%d physical-fit=true" % (t1 + 405, 2 * 1024 * 1024 // 2, 2 * 1024 * 1024 // 2, 40 * GIB))
    (root / "pincharge.txt").write_text("\n".join(pc) + "\n")
    recent = ["aira daemon: pin-charge observe: scope=A pin=1 charged=1 would-charge=1 factor=0.90 physical-free=1 physical-fit=true",
              "aira daemon: pin-charge enforce: scope=B pin=%d charged=%d factor=0.90 physical-free=1 physical-fit=true" % (PIN, CHARGE),
              "aira daemon: pin-charge enforce: scope=C pin=%d charged=%d factor=0.90 physical-free=1 physical-fit=false" % (PIN, PIN)]
    recent += c.get("recent", [])
    (root / "recent.txt").write_text("\n".join(recent) + "\n")
    (root / "oomd.txt").write_text("".join(l + "\n" for l in c.get("oomd", [])))

    db = sqlite3.connect(str(home / ".local/state/aira/state.db"))
    db.execute("create table confine_peak_history (signature text not null, peak_rss integer, oom integer not null, "
               "at text not null, kind text not null default 'confine', budget integer, budget_basis text, "
               "wall_ms integer, wait_ms integer, started_at_ms integer, repo text)")
    rowless = c.get("rowless", ())
    n = 0
    for g, t in enumerate(grant_times):
        if g in rowless:
            continue
        peak = int(9.5 * GIB) if n < c.get("above", 0) else GIB
        db.execute("insert into confine_peak_history values ('x', ?, 0, 'a', 'confine', ?, ?, 1, 0, ?, 'r')",
                   (peak, PIN, BASIS, int(t * 1000) + c.get("offset", {}).get(g, 1)))
        n += 1
    # an ineligible pinned row 5 s from any line, above the charge: never paired
    db.execute("insert into confine_peak_history values ('x', ?, 0, 'a', 'confine', ?, ?, 1, 0, ?, 'r')",
               (int(9.9 * GIB), PIN, BASIS, int((t1 + 5) * 1000)))
    for g, off, peak, sig in c.get("extra", ()):
        db.execute("insert into confine_peak_history values (?, ?, 0, 'a', 'confine', ?, ?, 1, 0, ?, 'r')",
                   (sig, peak, PIN, BASIS, int(grant_times[g] * 1000) + off))
    db.commit()
    db.close()

    bin_ = root / "bin"
    bin_.mkdir()
    jfail = "exit 1\n" if c.get("journal_fail") else ""
    ofail = "exit 1\n" if c.get("oomd_fail") else ""
    shim(bin_ / "journalctl", SHIM_NEEDS_XDG +
         'case "$*" in *systemd-oomd*) %s cat %s ;; *short-unix*) %s cat %s ;; *) %s cat %s ;; esac\n' % (
             ofail.strip() + ";" if ofail else "", root / "oomd.txt", jfail.strip() + ";" if jfail else "",
             root / "pincharge.txt", jfail.strip() + ";" if jfail else "", root / "recent.txt"))
    shim(bin_ / "systemctl", SHIM_NEEDS_XDG + 'case "$*" in *ControlGroup*) echo /fake ;; *) echo "Mon 2026-11-16 08:00:00 EET" ;; esac\n')
    shim(bin_ / "aira", "echo '%s'\n" % json.dumps({"data": {"slice_reserve": {
        "queued": 3, "jobs": 5, "granted_bytes": 40 * GIB, "ceiling_bytes": 48 * GIB, "ceiling_basis": "system-pressure"}}}))
    return home, cg, bin_, meminfo


def invoke(watch, home, cg, bin_, meminfo, store):
    env = {"HOME": str(home), "PATH": "%s:/usr/bin:/bin" % bin_, "AIRA_324_WATCH_CGROUP": str(cg),
           "AIRA_324_WATCH_MEMINFO": str(meminfo)}
    p = subprocess.run(["env", "-i"] + ["%s=%s" % kv for kv in env.items()] + ["bash", str(watch)] +
                       (["--store"] if store else []), capture_output=True, text=True)
    out = p.stdout.strip().splitlines()
    return p.returncode, (json.loads(out[-1]) if out else None), p.stderr


def main():
    here = pathlib.Path(__file__).resolve().parent
    watch = pathlib.Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else here / "aira-324-enforce-watch.sh"
    base = pathlib.Path(os.environ.get("AIRA_324_WATCH_TEST_DIR", os.path.expanduser("~/tmp")))
    base.mkdir(parents=True, exist_ok=True)
    top = pathlib.Path(tempfile.mkdtemp(prefix="aira-324-watch-test-", dir=str(base)))
    ok = True

    def check(name, got, want):
        nonlocal ok
        if got != want:
            ok = False
            print("FAIL %s: got %r want %r" % (name, got, want))
        else:
            print("ok   %s" % name)

    def run(name, store=False, **c):
        now = time.time()
        d = top / name
        d.mkdir()
        if callable(c.get("samples")):
            c["samples"] = c["samples"](now)
        home, cg, bin_, meminfo = build(d, now, c)
        code, line, err = invoke(watch, home, cg, bin_, meminfo, store)
        check("%s: exit 0 in a cron-like environment" % name, (code, err.strip()[:200] if code else ""), (0, ""))
        if c.get("evict_then_rerun") and line:
            db = sqlite3.connect(str(home / ".local/state/aira/state.db"))
            db.execute("delete from confine_peak_history where peak_rss > ? and peak_rss < ?", (9 * GIB, int(9.6 * GIB)))
            db.execute("insert into confine_peak_history values ('x', 1, 0, 'a', 'confine', 1, ?, 1, 0, 1, 'r')", (BASIS,))
            db.commit()
            db.close()
            code, line, err = invoke(watch, home, cg, bin_, meminfo, store)
            check("%s: second run exit 0" % name, (code, err.strip()[:200] if code else ""), (0, ""))
        return line or {"met": None, "unevaluated": None}

    def ac(line):
        a = line.get("above_charge")
        return (a["grants"], a["paired"], a["rowless"], a["above_low"], a["above_high"], a["trigger"]) \
            if isinstance(a, dict) else a

    def unev(line, prefix):
        return any(u.startswith(prefix) for u in line["unevaluated"] or [])

    AC_MET = "above-charge share > 5% of discounted grants"
    try:
        # ---- the 5% trigger
        m = run("met", True, above=3)
        check("met: 3 of 40 above, all paired, no row minimum", ac(m), (40, 40, 0, 3, 3, "met"))
        check("met: the only met trigger", m["met"], [AC_MET])
        check("met: log fields from the journal (XDG_RUNTIME_DIR set by the script)",
              (m["observe"], m["enforce"], m["discounted"], m["phys_unfit"], m["enforce_factors"], m["watchdog_trips"]),
              (1, 2, 1, 1, ["0.90"], 0))
        check("met: queue fields from aira confine --list",
              (m["queued"], m["running"], m["ceiling_gib"], m["ceiling_basis"]), (3, 5, 48.0, "system-pressure"))
        check("met: host fields", (m["mem_available_gib"], m["oomd_kills"], m["oomd_kills_aira"]), (40.0, 0, 0))
        check("met: a quiet history leaves nothing unevaluated", m["unevaluated"], [])
        n = run("not-met", True, above=2)
        check("not-met: exactly 5%, ineligible row and undiscounted line left out", ac(n), (40, 40, 0, 2, 2, "not met"))
        check("not-met: nothing met or unevaluated", (n["met"], n["unevaluated"]), ([], []))
        s = run("straddle", True, above=2, rowless=(39,))
        check("straddle: one grant without a row is unevaluated", ac(s), (40, 39, 1, 2, 3, "unevaluated"))
        check("straddle: named unevaluated, not met", (s["met"], unev(s, "above-charge: unevaluated")), ([], True))
        dc = run("decoy", True, above=2, rowless=(39,), extra=[(39, 300, GIB, "y")])
        check("decoy: a low ineligible row 300 ms away does not stand in for the rowless grant", ac(dc),
              (40, 39, 1, 2, 3, "unevaluated"))
        dp = run("displace", True, above=2, offset={39: 5}, extra=[(39, 1, int(9.9 * GIB), "y")])
        check("displace: a high ineligible row nearer than the grant's own keeps it unknown", ac(dp),
              (40, 40, 0, 2, 3, "unevaluated"))
        check("displace: not met", dp["met"], [])
        pa = run("parent", True, above=2, rowless=(39,), extra=[(39, 1, GIB, PARENT)])
        check("parent: a delegate-parent row is never a grant's row", ac(pa), (40, 39, 1, 2, 3, "unevaluated"))
        ev = run("evicted", True, above=3, evict_then_rerun=True)
        check("evicted: a row seen by an earlier --store run still counts", ac(ev), (40, 40, 0, 3, 3, "met"))
        f = run("no-log", True, above=3, journal_fail=True)
        check("no-log: log fields unevaluated, not 0",
              (f["observe"], f["enforce"], f["discounted"], f["phys_unfit"], f["watchdog_trips"]), ("unevaluated",) * 5)
        check("no-log: above-charge unevaluated", f["above_charge"], "unevaluated (journalctl failed)")
        check("no-log: named unevaluated, nothing met",
              (f["met"], unev(f, "daemon log: unevaluated"), unev(f, "above-charge: unevaluated (journalctl")), ([], True, True))

        # ---- slice-level oom
        o = run("oom-first", samples=False)
        check("oom first sample: unevaluated, not silently dropped", (o["met"], unev(o, "slice-level oom: unevaluated")),
              ([], True))
        o = run("oom-rise", oom=1)
        check("oom rise: met", o["met"], ["slice-level oom (memory.events.local oom rose; victim and its peak/pin unattributed)"])
        o = run("oom-flat", oom=1, samples=lambda now: history(now, oom=1))
        check("oom flat: neither met nor unevaluated", (o["met"], o["unevaluated"]), ([], []))
        o = run("oom-reset", oom=0, samples=lambda now: history(now, oom=2))
        check("oom reset: unevaluated", (o["met"], unev(o, "slice-level oom: unevaluated (counter reset")), ([], True))

        # ---- memory.events high, baseline p99 0: an event in each of the last 2 hours
        HM = "memory.events high over baseline p99 for 2 h"
        h = run("high-2h", high=2, samples=lambda now: history(now, high=lambda t: 0 if t < now - 6000 else 1 if t < now - 1800 else 2))
        check("high both hours: met", h["met"], [HM])
        h = run("high-1h", high=1, samples=lambda now: history(now, high=lambda t: 0 if t < now - 1800 else 1))
        check("high last hour only: neither met nor unevaluated", (h["met"], h["unevaluated"]), ([], []))
        h = run("high-reset", high=1, samples=lambda now: history(now, high=lambda t: 3 if t < now - 5000 else 0 if t < now - 1800 else 1))
        check("high counter reset: unevaluated", (h["met"], unev(h, "high: unevaluated")), ([], True))
        h = run("high-gap", high=2, samples=lambda now: history(now, high=lambda t: 0 if t < now - 6000 else 1 if t < now - 1800 else 2,
                                                                drop=(now - 5000, now - 3000)))
        check("high with a 40-minute gap: unevaluated", (h["met"], unev(h, "high: unevaluated")), ([], True))

        # ---- memory.pressure, baseline p99 1.0: every sample of the last 2 hours over it
        PM = "memory.pressure some avg10 over baseline p99 for 2 h"
        p = run("pressure-2h", avg10=5.0, samples=lambda now: history(now, avg=lambda t: 5.0))
        check("pressure 2 hours: met", p["met"], [PM])
        p = run("pressure-dip", avg10=5.0, samples=lambda now: history(now, avg=lambda t: 0.5 if abs(t - (now - 3000)) < 1 else 5.0))
        check("pressure with one dip: neither met nor unevaluated", (p["met"], p["unevaluated"]), ([], []))
        p = run("pressure-gap", avg10=5.0, samples=lambda now: history(now, avg=lambda t: 5.0, drop=(now - 5000, now - 3000)))
        check("pressure with a gap: unevaluated", (p["met"], unev(p, "pressure: unevaluated")), ([], True))

        # ---- host memory
        x = run("memavail-low", memavail_kib=7 * 1024 * 1024)
        check("MemAvailable 7 GiB: met", x["met"], ["MemAvailable below 8 GiB (the watchdog trip; it acts on unconfined processes)"])
        x = run("memavail-missing", memavail_kib=None)
        check("meminfo unreadable: unevaluated", (x["met"], x["mem_available_gib"], unev(x, "MemAvailable: unevaluated")),
              ([], "unevaluated", True))
        x = run("watchdog", recent=["aira daemon: watchdog trip: offender=- mem_avail=7.50GiB psi_avg10=3"])
        check("watchdog trip line: met", (x["met"], x["watchdog_trips"]), (["daemon watchdog tripped (MemAvailable under 8 GiB)"], 1))
        x = run("oomd-kill", oomd=["Killed /user.slice/user-1000.slice/user@1000.service/aira.slice/.aira-CONFINE-x@y due to memory pressure"])
        check("systemd-oomd kill: met", (x["met"], x["oomd_kills"], x["oomd_kills_aira"]),
              (["systemd-oomd killed a cgroup (user-slice memory pressure)"], 1, 1))
        x = run("oomd-fail", oomd_fail=True)
        check("oomd journal unreadable: unevaluated", (x["met"], x["oomd_kills"], unev(x, "systemd-oomd log: unevaluated")),
              ([], "unevaluated", True))
        x = run("factor-080", recent=["aira daemon: pin-charge enforce: scope=D pin=%d charged=%d factor=0.80 physical-free=1 "
                                      "physical-fit=true" % (PIN, 8 * GIB)])
        check("enforce at 0.80: met", (x["met"], x["enforce_factors"]),
              (["enforce at a factor other than 0.90 (the report covers 0.9 only)"], ["0.80", "0.90"]))
    finally:
        shutil.rmtree(top, ignore_errors=True)
    print("watch test:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
