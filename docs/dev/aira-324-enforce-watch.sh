#!/usr/bin/env bash
# AIRA-324 enforce watch: one line of the rollback-trigger inputs, read-only.
#
# Spec: docs/superpowers/specs/2026-11-13-history-charge-design.md section 6 item 2.
# Report: docs/dev/aira-324-enforce-gate-report.md (sections 8 to 10).
#
#   docs/dev/aira-324-enforce-watch.sh [--minutes N] [--store]   one sample, one JSON line
#   docs/dev/aira-324-enforce-watch.sh --baseline                 write/print the pre-enforce baseline
#
# It reads aira.slice's cgroup files, /proc/meminfo, `aira confine --list`, the daemon's and
# systemd-oomd's journal, and writes ONLY under ~/tmp/aira-324-watch/ (or $AIRA_324_WATCH_DIR):
# samples, the baseline and the pinned rows it has seen (rows.tsv: start, pin, peak, oom and
# run length; no command). With --store it copies the store there, reads the copy and deletes
# it. It changes nothing in aira, systemd or the store, and it ROLLS NOTHING BACK: it prints
# what is met, and the operator acts (report section 10). Safe from cron or a loop, every 5
# minutes (the 2-hour triggers need samples at most 15 minutes apart), with --store once an
# hour (the copy is about 340 MB). Cron gives a bare environment: the script sets
# XDG_RUNTIME_DIR=/run/user/<uid> itself when it is unset (systemctl --user and journalctl
# --user need it), and the cron line must put aira on PATH (else the queue fields read
# unevaluated):
#   */5 * * * * PATH="$HOME/.local/bin:/usr/bin:/bin" /path/to/docs/dev/aira-324-enforce-watch.sh >> "$HOME/tmp/aira-324-watch/watch.log" 2>&1
#   7 * * * *   PATH="$HOME/.local/bin:/usr/bin:/bin" /path/to/docs/dev/aira-324-enforce-watch.sh --store >> "$HOME/tmp/aira-324-watch/watch.log" 2>&1
#
# Fields (cumulative counters since the slice was created; d_* = change since the last
# sample, "unevaluated" on the first sample or after a counter reset):
#   oom_local, max_local  aira.slice's OWN memory.events.local `oom` / `max`: the SLICE's limit
#                         was hit (the kernel counts `oom` against the cgroup whose limit was
#                         exceeded). Which job died, and its peak against its pin, is not
#                         attributable from cgroup files: any rise counts.
#   oom_kill_local        counted against the VICTIM's cgroup (a job scope), so 0 even for a
#                         slice-level kill: shown, not used.
#   oom_kill, high        hierarchical memory.events: oom_kill includes every job killed at its
#                         own cap (daily, not a trigger); high is the MemoryHigh trigger.
#   some_avg10            memory.pressure (hierarchical: a job stalling at its own cap raises it).
#   mem_available_gib     /proc/meminfo MemAvailable. The daemon's watchdog trips below 8 GiB
#                         (watchdog.go watchdogLowMemAvailable) and acts on UNCONFINED processes.
#   queued/running/granted_gib/ceiling_gib/ceiling_basis   from `aira confine --list`;
#                         ceiling_basis "system-pressure" means host memory binds, not the slice.
#   observe/enforce/discounted/phys_unfit/enforce_factors/watchdog_trips   daemon log lines in
#                         the last N minutes ("unevaluated" if journalctl fails: never a fake 0).
#   oomd_kills, oomd_kills_aira   systemd-oomd `Killed` lines in the last N minutes (all / under
#                         aira.slice); "unevaluated" if its journal cannot be read.
#   above_charge (--store)  the spec's 5% trigger on the DISCOUNTED grants: every `pin-charge
#                         enforce` line with charged < pin in the current enforce epoch (back to
#                         the last observe line or factor change) and the last 7 days, against the
#                         pinned rows of the same pin started within 100 ms (delegate parents left
#                         out: never discounted). A row has no scope id, so the count is an interval
#                         [above_low, above_high]: grants with no row (failed, killed, still
#                         running, or the row evicted by the 20-row retention before an hourly
#                         --store saw it) and components that do not pair one to one widen it (the
#                         replay script's attribute(), selftested there). met above 5% even at the
#                         low end, not met when even the high end is <= 5%, else unevaluated.
#                         share_one_to_one_estimate is for the owner's judgement only.
#   met                   rollback triggers that are MET: the operator rolls back (report 9 step 5).
#   unevaluated           triggers that cannot be judged from this sample, each with its reason.
set -euo pipefail

minutes=10
store=0
baseline=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --minutes) minutes="${2:-}"; shift 2 ;;
    --store) store=1; shift ;;
    --baseline) baseline=1; shift ;;
    -h|--help) sed -n '2,56p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
case "$minutes" in ''|*[!0-9]*) echo "--minutes must be a whole number" >&2; exit 2 ;; esac

dir="${AIRA_324_WATCH_DIR:-$HOME/tmp/aira-324-watch}"
mkdir -p "$dir"
# AIRA_324_WATCH_CGROUP (a directory holding memory.events(.local) and memory.pressure) and
# AIRA_324_WATCH_MEMINFO (a meminfo file) are test seams, never needed in normal use.
# cron runs without XDG_RUNTIME_DIR; systemctl --user and journalctl --user need it.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
if [ -n "${AIRA_324_WATCH_CGROUP:-}" ]; then
  cg="$AIRA_324_WATCH_CGROUP"
else
  slice_cg="$(systemctl --user show aira.slice -p ControlGroup --value 2> /dev/null)" || slice_cg=""
  [ -n "$slice_cg" ] || { echo "cannot find aira.slice's cgroup (systemctl --user failed)" >&2; exit 3; }
  cg="/sys/fs/cgroup$slice_cg"
fi
[ -r "$cg/memory.events.local" ] || { echo "cannot read $cg/memory.events.local" >&2; exit 3; }
meminfo="${AIRA_324_WATCH_MEMINFO:-/proc/meminfo}"
slice_since="$(systemctl --user show aira.slice -p ActiveEnterTimestamp --value 2> /dev/null)" || slice_since="unevaluated"

list_json="$dir/list.json"
if ! aira confine --list > "$list_json" 2> /dev/null; then
  : > "$list_json"
fi
journal="$dir/journal.txt"
journal_ok=1
if ! journalctl --user -u aira-daemon.service --since "${minutes} min ago" -o cat > "$journal" 2> /dev/null; then
  journal_ok=0
  : > "$journal"
fi
oomd="$dir/oomd.txt"
oomd_ok=1
if ! journalctl -t systemd-oomd --since "${minutes} min ago" -o cat > "$oomd" 2> /dev/null; then
  oomd_ok=0
  : > "$oomd"
fi
pincharge="$dir/pincharge.txt"
pincharge_ok=0
: > "$pincharge"
if [ "$store" -eq 1 ]; then
  # The unit's journal for 8 days, to find the current enforce epoch and its grants.
  if journalctl --user -u aira-daemon.service --since "8 days ago" -o short-unix > "$pincharge" 2> /dev/null; then
    pincharge_ok=1
  fi
  cp "$HOME/.local/state/aira/state.db" "$dir/state.db"
  if [ -e "$HOME/.local/state/aira/state.db-wal" ]; then
    cp "$HOME/.local/state/aira/state.db-wal" "$dir/state.db-wal"
  else
    rm -f "$dir/state.db-wal"
  fi
fi

replay="$(dirname "$0")/aira-324-enforce-gate-replay.py"
[ -r "$replay" ] || { echo "cannot read $replay (the 5% trigger arithmetic lives there)" >&2; exit 3; }
exec python3 -I - "$cg" "$dir" "$list_json" "$journal" "$minutes" "$store" "$baseline" "$pincharge" "$slice_since" \
  "$journal_ok" "$pincharge_ok" "$replay" "$meminfo" "$oomd" "$oomd_ok" <<'PY'
import importlib.util, json, os, re, sqlite3, sys, time

(cg, d, list_json, journal, minutes, store, baseline, pincharge, slice_since, journal_ok, pincharge_ok, replay,
 meminfo, oomd, oomd_ok) = sys.argv[1:]
spec = importlib.util.spec_from_file_location("aira324replay", replay)
R = importlib.util.module_from_spec(spec)
spec.loader.exec_module(R)
now = time.time()
MAX_GAP = 900                  # samples further apart than this break a 2-hour window
WATCHDOG_TRIP = 8 * 2**30      # watchdog.go watchdogLowMemAvailable
TRIAL_FACTOR = "0.90"          # the only factor the report's trial covers
COUNTERS = ("oom_local", "max_local", "oom_kill_local", "oom_kill", "high")
samples_path = os.path.join(d, "samples.tsv")
rows_path = os.path.join(d, "rows.tsv")
PARENT = R.PARENT_PREFIX       # delegate-parent signatures: never discounted


def cleanup():
    # The list, the journal excerpts and the store copy name other sessions' commands: keep none.
    for p in (list_json, journal, oomd, pincharge, os.path.join(d, "state.db"), os.path.join(d, "state.db-wal"),
              os.path.join(d, "state.db-shm")):
        if os.path.exists(p):
            os.remove(p)


def events(name):
    out = {}
    with open(os.path.join(cg, name)) as f:
        for line in f:
            k, v = line.split()
            out[k] = int(v)
    return out


def avg10():
    with open(os.path.join(cg, "memory.pressure")) as f:
        for line in f:
            if line.startswith("some "):
                return float(re.search(r"avg10=([0-9.]+)", line).group(1))
    return float("nan")


def mem_available():
    try:
        with open(meminfo) as f:
            for line in f:
                if line.startswith("MemAvailable:"):
                    return int(line.split()[1]) * 1024
    except OSError:
        pass
    return None


def p99(xs):
    s = sorted(xs)
    return s[min(len(s) - 1, int(0.99 * len(s)))] if s else None


def read_samples():
    """[(t, {counter: value}, avg10)] in time order; lines of another format are skipped."""
    rows = []
    if os.path.exists(samples_path):
        with open(samples_path) as f:
            for line in f:
                p = line.rstrip("\n").split("\t")
                if len(p) == 2 + len(COUNTERS) and p[0][:1].isdigit():
                    rows.append((float(p[0]), dict(zip(COUNTERS, map(int, p[1:-1]))), float(p[-1])))
    rows.sort(key=lambda r: r[0])
    return rows


def continuous(rows, lo, hi):
    """Samples cover [lo, hi] with no gap over MAX_GAP (including the edges)."""
    w = [r for r in rows if lo - MAX_GAP <= r[0] <= hi]
    if not w or w[0][0] > lo or w[-1][0] < hi - MAX_GAP:
        return False
    return all(b[0] - a[0] <= MAX_GAP for a, b in zip(w, w[1:]))


def at_or_before(rows, t):
    c = [r for r in rows if r[0] <= t]
    return c[-1] if c else None


def window_delta(rows, k, lo, hi):
    """Counter change over [lo, hi] using the last sample at or before each edge, so an
    increment just after a boundary is not lost; None if the window is not covered or a
    counter reset (a fall) happened inside it."""
    a, b = at_or_before(rows, lo), at_or_before(rows, hi)
    if a is None or b is None or not continuous(rows, lo, hi):
        return None
    inside = [r[1][k] for r in rows if a[0] <= r[0] <= b[0]]
    if any(y < x for x, y in zip(inside, inside[1:])):
        return None
    return b[1][k] - a[1][k]


ev, evl = events("memory.events"), events("memory.events.local")
cur = {"oom_local": evl["oom"], "max_local": evl["max"], "oom_kill_local": evl["oom_kill"],
       "oom_kill": ev["oom_kill"], "high": ev["high"]}
a10 = avg10()
rows = read_samples()

if baseline == "1":
    out = {"at": int(now), "slice_active_since": slice_since}
    # Counters are cumulative since the slice was created: a cumulative 0 proves every
    # hourly delta since then was 0, so its p99 is 0 over that span (not the spec's week).
    if cur["high"] == 0:
        out["high_p99_per_hour"] = 0
        out["high_basis"] = "cumulative high = 0 since slice creation: every hourly delta was 0 (span shorter than the spec's week)"
    else:
        deltas = []
        t = rows[0][0] if rows else now
        while t + 3600 <= now:
            dlt = window_delta(rows, "high", t, t + 3600)
            if dlt is not None:
                deltas.append(dlt)
            t += 3600
        out["high_p99_per_hour"] = p99(deltas) if len(deltas) >= 24 else "unevaluated"
        out["high_basis"] = "%d covered sampled hours (need >= 24)" % len(deltas)
    vals = [r[2] for r in rows if r[2] == r[2]]
    span_h = (rows[-1][0] - rows[0][0]) / 3600 if len(rows) > 1 else 0.0
    if len(vals) >= 24 and span_h >= 2 and continuous(rows, rows[0][0], rows[-1][0]):
        out["some_avg10_p99"] = p99(vals)
        out["some_avg10_basis"] = "%d samples over %.1f h, no gap over %d s (the spec wants a week: weaker)" % (
            len(vals), span_h, MAX_GAP)
    else:
        out["some_avg10_p99"] = "unevaluated"
        out["some_avg10_basis"] = ("no memory.pressure history exists before this script's samples; have %d samples "
                                   "over %.1f h, need >= 24 over >= 2 h with no gap over %d s" % (len(vals), span_h, MAX_GAP))
    out["oom_local_since_slice_creation"] = cur["oom_local"]
    with open(os.path.join(d, "baseline.json"), "w") as f:
        json.dump(out, f)
    cleanup()
    print(json.dumps(out))
    sys.exit(0)

met, unev = [], []
last = rows[-1] if rows else None
line = {"at": time.strftime("%Y-%m-%dT%H:%M:%S%z", time.localtime(now))}
for k in COUNTERS:
    line[k] = cur[k]
    if last is None:
        line["d_" + k] = "unevaluated"
    elif cur[k] < last[1][k]:
        line["d_" + k] = "unevaluated (counter reset: slice re-created)"
    else:
        line["d_" + k] = cur[k] - last[1][k]
line["since_last_s"] = int(now - last[0]) if last else "unevaluated"
line["some_avg10"] = a10
ma = mem_available()
line["mem_available_gib"] = round(ma / 2**30, 1) if ma is not None else "unevaluated"
try:
    with open(list_json) as f:
        sr = json.load(f)["data"]["slice_reserve"]
    line["queued"], line["running"] = sr.get("queued"), sr.get("jobs")
    line["granted_gib"] = round(sr["granted_bytes"] / 2**30, 1)
    line["ceiling_gib"] = round(sr["ceiling_bytes"] / 2**30, 1)
    line["ceiling_basis"] = sr.get("ceiling_basis", "unevaluated")
except Exception:
    for k in ("queued", "running", "granted_gib", "ceiling_gib", "ceiling_basis"):
        line[k] = "unevaluated"
obs = enf = disc = unfit = trips = 0
factors = set()
with open(journal, errors="replace") as f:
    for l in f:
        if "pin-charge observe:" in l:
            obs += 1
        m = re.search(r"pin-charge enforce: scope=\S+ pin=(\d+) charged=(\d+) factor=([0-9.]+)", l)
        if m:
            enf += 1
            disc += int(m.group(2)) < int(m.group(1))
            factors.add(m.group(3))
        if "pin-charge" in l and "physical-fit=false" in l:
            unfit += 1
        if "aira daemon: watchdog trip:" in l:
            trips += 1
line["minutes"] = int(minutes)
if journal_ok == "1":
    line.update({"observe": obs, "enforce": enf, "discounted": disc, "phys_unfit": unfit,
                 "enforce_factors": sorted(factors), "watchdog_trips": trips})
else:
    for k in ("observe", "enforce", "discounted", "phys_unfit", "enforce_factors", "watchdog_trips"):
        line[k] = "unevaluated"
if oomd_ok == "1":
    with open(oomd, errors="replace") as f:
        kills = [l for l in f if l.startswith("Killed ")]
    line["oomd_kills"], line["oomd_kills_aira"] = len(kills), sum(1 for l in kills if "/aira.slice/" in l)
else:
    line["oomd_kills"] = line["oomd_kills_aira"] = "unevaluated"


def pinned_rows():
    """Pinned non-parent rows of the last 8 days: the store copy's, unioned with every row an
    earlier --store run saw (rows are insert-only, so a row the 20-row retention has since
    evicted is still counted). Run on every --store, in an enforce epoch or not. rows.tsv
    keeps 8 days and no command text."""
    seen = set()
    if os.path.exists(rows_path):
        with open(rows_path) as f:
            for l in f:
                p = l.split()
                if len(p) == 5:
                    seen.add(tuple(map(int, p)))
    c = sqlite3.connect("file:%s?mode=ro" % os.path.join(d, "state.db"), uri=True)
    seen.update(c.execute(
        "select started_at_ms, budget, coalesce(peak_rss, 0), oom, coalesce(wall_ms, -1) from confine_peak_history "
        "where kind = 'confine' and budget_basis = ? and started_at_ms >= ? and budget is not null "
        "and instr(signature, ?) != 1",          # instr, not substr: the prefix ends in a NUL
        (R.PIN_BASIS, int((now - 8 * 86400) * 1000), PARENT)).fetchall())
    c.close()
    keep = sorted(r for r in seen if r[0] >= int((now - 8 * 86400) * 1000))
    with open(rows_path + ".tmp", "w") as f:
        f.writelines("%d %d %d %d %d\n" % r for r in keep)
    os.replace(rows_path + ".tmp", rows_path)
    return keep


if store == "1":
    seen_rows = pinned_rows()
    epoch, fac, grants = None, None, []
    with open(pincharge, errors="replace") as f:
        for l in f:
            m = re.search(r"pin-charge (observe|enforce): scope=\S+ pin=(\d+) charged=(\d+) .*factor=([0-9.]+)", l)
            if not m:
                continue
            t = float(l.split(" ", 1)[0])
            if m.group(1) == "observe":
                epoch, fac, grants = None, None, []
                continue
            if fac != m.group(4):
                epoch, fac, grants = t, m.group(4), []
            if int(m.group(3)) < int(m.group(2)):          # discounted
                grants.append((int(t * 1000), int(m.group(2)), int(m.group(3))))
    if pincharge_ok != "1":
        line["above_charge"] = "unevaluated (journalctl failed)"
    elif epoch is None:
        line["above_charge"] = "unevaluated (the journal's latest pin-charge line is not enforce, or none in 8 days)"
    else:
        lo = max(epoch, now - 7 * 86400)
        lo_ms = int(lo * 1000)            # the same truncation as each grant's time: the epoch's first line counts
        grants = [g for g in grants if g[0] >= lo_ms]
        prow = [r for r in seen_rows if r[0] >= lo_ms - R.TOL]
        jobs = [R.Job(i, st, st, 0, pk, b, R.PIN_BASIS, oom) for i, (st, b, pk, oom, _) in enumerate(prow)]
        charged = {g[1]: g[2] for g in grants}            # one factor per epoch: one charge per pin
        a = R.attribute(grants, jobs, charged.__getitem__)
        n = len(grants)
        line["above_charge"] = {
            "factor": float(fac), "since": int(lo), "grants": n, "paired": a["paired"], "rowless": a["rowless"],
            "above_low": a["low"], "above_high": a["high"],
            "share_low": round(a["low"] / n, 4) if n else "unevaluated",
            "share_high": round(a["high"] / n, 4) if n else "unevaluated",
            # an ESTIMATE over the one-to-one pairs only; not the trigger
            "one_to_one": a["single"],
            "share_one_to_one_estimate": round(a["single_above"] / a["single"], 4) if a["single"] else "unevaluated",
            "rows_seen": len(prow),
            "trigger": R.trigger_verdict(a["low"], a["high"] - a["low"], n)}

if isinstance(line["d_oom_local"], int):
    if line["d_oom_local"] > 0:
        met.append("slice-level oom (memory.events.local oom rose; victim and its peak/pin unattributed)")
else:
    unev.append("slice-level oom: %s (no previous sample to compare)" % line["d_oom_local"]
                if last is None else "slice-level oom: unevaluated (counter reset: slice re-created)")
if ma is None:
    unev.append("MemAvailable: unevaluated (%s unreadable)" % meminfo)
elif ma < WATCHDOG_TRIP:
    met.append("MemAvailable below 8 GiB (the watchdog trip; it acts on unconfined processes)")
allrows = rows + [(now, cur, a10)]
bl_path = os.path.join(d, "baseline.json")
if os.path.exists(bl_path):
    with open(bl_path) as f:
        bl = json.load(f)
    if isinstance(bl.get("some_avg10_p99"), (int, float)):
        w = [r for r in allrows if r[0] >= now - 7200]
        if not continuous(allrows, now - 7200, now):
            unev.append("pressure: unevaluated (samples do not cover the last 2 h)")
        elif all(r[2] > bl["some_avg10_p99"] for r in w):
            met.append("memory.pressure some avg10 over baseline p99 for 2 h")
    else:
        unev.append("pressure: unevaluated (no baseline p99)")
    if isinstance(bl.get("high_p99_per_hour"), (int, float)):
        h2 = window_delta(allrows, "high", now - 7200, now - 3600)
        h1 = window_delta(allrows, "high", now - 3600, now)
        if h1 is None or h2 is None:
            unev.append("high: unevaluated (samples do not cover the last 2 h, or a counter reset)")
        elif h1 > bl["high_p99_per_hour"] and h2 > bl["high_p99_per_hour"]:
            met.append("memory.events high over baseline p99 for 2 h")
    else:
        unev.append("high: unevaluated (no baseline p99)")
else:
    unev.append("pressure/high: unevaluated (run --baseline first)")
if journal_ok != "1":
    unev.append("daemon log: unevaluated (journalctl failed): watchdog trips and enforce factor unread")
else:
    if trips:
        met.append("daemon watchdog tripped (MemAvailable under 8 GiB)")
    if [x for x in factors if x != TRIAL_FACTOR]:
        met.append("enforce at a factor other than %s (the report covers 0.9 only)" % TRIAL_FACTOR)
if oomd_ok != "1":
    unev.append("systemd-oomd log: unevaluated (journalctl -t systemd-oomd failed)")
elif line["oomd_kills"]:
    met.append("systemd-oomd killed a cgroup (user-slice memory pressure)")
ac = line.get("above_charge")
if isinstance(ac, dict) and ac["trigger"] == "met":
    met.append("above-charge share > 5% of discounted grants")
elif isinstance(ac, str):
    unev.append("above-charge: " + ac)
elif isinstance(ac, dict) and ac["trigger"] == "unevaluated":
    unev.append("above-charge: unevaluated (%s)" % (
        "between %d and %d of %d discounted grants are above the charge (%d with no row)" % (
            ac["above_low"], ac["above_high"], ac["grants"], ac["rowless"])
        if ac["grants"] else "no discounted grant in the epoch"))
line["met"], line["unevaluated"] = met, unev

with open(samples_path, "a") as f:
    f.write("%.0f\t%s\t%s\n" % (now, "\t".join(str(cur[k]) for k in COUNTERS), a10))
cleanup()
print(json.dumps(line))
PY
