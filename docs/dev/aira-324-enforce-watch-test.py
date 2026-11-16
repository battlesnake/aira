#!/usr/bin/env python3
"""AIRA-324 enforce watch: end-to-end checks of docs/dev/aira-324-enforce-watch.sh.

Runs the watch against fakes only: a fake HOME holding a tiny store, a fake cgroup
directory, and shims for journalctl, systemctl and aira on PATH. It never reads the live
store, the real journal or the real slice. Everything goes under a fresh directory in
~/tmp (or $AIRA_324_WATCH_TEST_DIR), removed at the end.

    python3 -I docs/dev/aira-324-enforce-watch-test.py [path/to/aira-324-enforce-watch.sh]

The watch finds aira-324-enforce-gate-replay.py next to itself, so a copy of the watch
must sit next to a copy of the replay. Exit 0: every check passed; 1: a check failed.

Scenarios (the environment is cron-like: env -i, HOME and PATH only, no XDG_RUNTIME_DIR;
the shims fail like the real tools when XDG_RUNTIME_DIR is missing):
  met       3 of 40 discounted grants above their charge, every grant paired: 7.5% > 5%
            with 40 rows (no row minimum)
  not-met   2 of 40 = exactly 5%: not over. An ineligible pinned row (no line within
            0.5 s) peaking above the charge, and a non-discounted enforce line, are left out
  straddle  2 of 40 above, 1 grant with no row: [5%, 7.5%] is unevaluated
  no-log    journalctl fails: the log fields and the trigger are unevaluated, never 0
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

SHIM_NEEDS_XDG = """if [ -z "${XDG_RUNTIME_DIR:-}" ]; then echo "Failed to connect to bus: No medium found" >&2; exit 1; fi
"""


def shim(path, body):
    path.write_text("#!/bin/sh\n" + body)
    path.chmod(0o755)


def build(root, scenario, now):
    home = root / "home"
    (home / ".local/state/aira").mkdir(parents=True)
    cg = root / "cg"
    cg.mkdir()
    (cg / "memory.events").write_text("low 0\nhigh 0\nmax 0\noom 0\noom_kill 7\n")
    (cg / "memory.events.local").write_text("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n")
    (cg / "memory.pressure").write_text("some avg10=0.10 avg60=0.00 avg300=0.00 total=1\n"
                                        "full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
    t1 = now - 3600
    above = {"met": 3, "not-met": 2, "straddle": 2, "no-log": 3}[scenario]
    rowless = 1 if scenario == "straddle" else 0
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
    (root / "recent.txt").write_text("\n".join(recent) + "\n")
    db = sqlite3.connect(str(home / ".local/state/aira/state.db"))
    db.execute("create table confine_peak_history (signature text not null, peak_rss integer, oom integer not null, "
               "at text not null, kind text not null default 'confine', budget integer, budget_basis text, "
               "wall_ms integer, wait_ms integer, started_at_ms integer, repo text)")
    for i, t in enumerate(grant_times[rowless:]):
        peak = int(9.5 * GIB) if i < above else GIB
        db.execute("insert into confine_peak_history values ('x', ?, 0, 'a', 'confine', ?, ?, 1, 0, ?, 'r')",
                   (peak, PIN, BASIS, int(t * 1000) + 1))
    # an ineligible pinned row (no line within 0.5 s) above the charge: must not count
    db.execute("insert into confine_peak_history values ('x', ?, 0, 'a', 'confine', ?, ?, 1, 0, ?, 'r')",
               (int(9.9 * GIB), PIN, BASIS, int((t1 + 5) * 1000)))
    db.commit()
    db.close()
    bin_ = root / "bin"
    bin_.mkdir()
    fail = "exit 1\n" if scenario == "no-log" else ""
    shim(bin_ / "journalctl", SHIM_NEEDS_XDG + fail +
         'case "$*" in *short-unix*) cat %s ;; *) cat %s ;; esac\n' % (root / "pincharge.txt", root / "recent.txt"))
    shim(bin_ / "systemctl", SHIM_NEEDS_XDG + 'case "$*" in *ControlGroup*) echo /fake ;; *) echo "Mon 2026-11-16 08:00:00 EET" ;; esac\n')
    shim(bin_ / "aira", "echo '%s'\n" % json.dumps({"data": {"slice_reserve": {
        "queued": 3, "jobs": 5, "granted_bytes": 40 * GIB, "ceiling_bytes": 48 * GIB}}}))
    return home, cg, bin_


def run(watch, root, scenario):
    now = time.time()
    home, cg, bin_ = build(root, scenario, now)
    env = {"HOME": str(home), "PATH": "%s:/usr/bin:/bin" % bin_, "AIRA_324_WATCH_CGROUP": str(cg)}
    p = subprocess.run(["env", "-i"] + ["%s=%s" % kv for kv in env.items()] + ["bash", str(watch), "--store"],
                       capture_output=True, text=True)
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

    try:
        res = {}
        for sc in ("met", "not-met", "straddle", "no-log"):
            d = top / sc
            d.mkdir()
            res[sc] = run(watch, d, sc)
        for sc, (code, line, err) in res.items():
            check("%s: exit 0 in a cron-like environment" % sc, (code, err.strip()[:200] if code else ""), (0, ""))
        if all(r[1] for r in res.values()):
            m, n, s, f = (res[k][1] for k in ("met", "not-met", "straddle", "no-log"))
            key = lambda ac: (ac["grants"], ac["paired"], ac["unknown"], ac["above"], ac["trigger"]) \
                if isinstance(ac, dict) else ac
            check("met: 3 of 40 above, all paired, no row minimum", key(m["above_charge"]), (40, 40, 0, 3, "met"))
            check("met: named in triggers", "above-charge share > 5% of discounted grants" in m["triggers"], True)
            check("not-met: exactly 5%, ineligible row and undiscounted line left out", key(n["above_charge"]),
                  (40, 40, 0, 2, "not met"))
            check("not-met: no above-charge entry in triggers", [t for t in n["triggers"] if "above-charge" in t], [])
            check("straddle: one grant without a row is unevaluated", key(s["above_charge"]), (40, 39, 1, 2, "unevaluated"))
            check("straddle: named unevaluated in triggers",
                  any(t.startswith("above-charge: unevaluated") for t in s["triggers"]), True)
            check("log fields from the journal (XDG_RUNTIME_DIR set by the script)",
                  (m["observe"], m["enforce"], m["discounted"], m["phys_unfit"]), (1, 2, 1, 1))
            check("queue fields from aira confine --list", (m["queued"], m["running"], m["ceiling_gib"]), (3, 5, 48.0))
            check("no-log: log fields unevaluated, not 0",
                  (f["observe"], f["enforce"], f["discounted"], f["phys_unfit"]), ("unevaluated",) * 4)
            check("no-log: above-charge unevaluated", f["above_charge"], "unevaluated (journalctl failed)")
            check("no-log: named in triggers", "daemon log: unevaluated (journalctl failed)" in f["triggers"], True)
    finally:
        shutil.rmtree(top, ignore_errors=True)
    print("watch test:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
