#!/usr/bin/env bash
# AIRA-324 enforce watch: one line of the rollback-trigger inputs, read-only.
#
# Spec: docs/superpowers/specs/2026-11-13-history-charge-design.md section 6 item 2.
# Report: docs/dev/aira-324-enforce-gate-report.md (section "Monitoring").
#
#   docs/dev/aira-324-enforce-watch.sh [--minutes N] [--store]   one sample, one JSON line
#   docs/dev/aira-324-enforce-watch.sh --baseline                 write/print the pre-enforce baseline
#
# It reads aira.slice's cgroup files, `aira confine --list` and the daemon journal, and
# writes ONLY under ~/tmp/aira-324-watch/ (or $AIRA_324_WATCH_DIR): state, samples and the
# baseline. With --store it copies the store there, reads the copy and deletes it. It
# changes nothing in aira, systemd or the store. Safe from cron or a loop, for example
# every 5 minutes (the 2-hour triggers need samples at most 15 minutes apart):
#   */5 * * * * /path/to/docs/dev/aira-324-enforce-watch.sh >> ~/tmp/aira-324-watch/watch.log 2>&1
#
# Fields (cumulative counters since the slice was created; d_* = change since the last
# sample, "unevaluated" on the first sample or after a counter reset):
#   oom_local, max_local  aira.slice's OWN memory.events.local `oom` / `max`: the SLICE's
#                         limit was hit. The kernel counts `oom` against the cgroup whose
#                         limit was exceeded, so a slice-level OOM shows here; the slice-level
#                         rollback trigger is any rise of oom_local.
#   oom_kill_local        counted against the VICTIM's cgroup (always a job scope, never the
#                         slice itself), so it stays 0 even for a slice-level kill: shown, not
#                         used. Which job a slice-level OOM killed, and whether its peak was
#                         under its pin, is not attributable from cgroup files: unevaluated.
#   oom_kill, high        hierarchical memory.events: oom_kill includes every job killed at
#                         its own cap (daily, not a trigger); high is the MemoryHigh trigger.
#   some_avg10            memory.pressure (hierarchical: a job stalling at its own cap raises it).
#   queued/running/granted_gib/ceiling_gib   from `aira confine --list` (ceiling = ledger ceiling;
#                         the report's condition C4 fires when it stays above 57.6 GiB for an hour).
#   observe/enforce/discounted/phys_unfit    daemon log lines in the last N minutes.
#   above_charge (--store)  pinned rows (cap:operator:--memory-reserve) that STARTED in the
#                         current enforce epoch (the run of enforce lines at the current
#                         factor, back to the last observe line or factor change) and in the
#                         last 7 days, whose peak exceeds ceil(factor x budget). OOM rows are
#                         kept. Rows cannot tell a discountable pin from a delegate parent or
#                         exclusive job, and failed or evicted runs have no row: stated, not fixed.
#   triggers              rollback triggers that are MET, plus the ones that are unevaluated.
set -euo pipefail

minutes=10
store=0
baseline=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --minutes) minutes="${2:-}"; shift 2 ;;
    --store) store=1; shift ;;
    --baseline) baseline=1; shift ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
case "$minutes" in ''|*[!0-9]*) echo "--minutes must be a whole number" >&2; exit 2 ;; esac

dir="${AIRA_324_WATCH_DIR:-$HOME/tmp/aira-324-watch}"
mkdir -p "$dir"
# AIRA_324_WATCH_CGROUP points at another directory holding memory.events(.local) and
# memory.pressure: a test seam (a fake cgroup), never needed in normal use.
cg="${AIRA_324_WATCH_CGROUP:-/sys/fs/cgroup$(systemctl --user show aira.slice -p ControlGroup --value)}"
[ -r "$cg/memory.events.local" ] || { echo "cannot read $cg/memory.events.local" >&2; exit 3; }
slice_since="$(systemctl --user show aira.slice -p ActiveEnterTimestamp --value)"

list_json="$dir/list.json"
if ! aira confine --list > "$list_json" 2> /dev/null; then
  : > "$list_json"
fi
journal="$dir/journal.txt"
journalctl --user -u aira-daemon.service --since "${minutes} min ago" -o cat > "$journal" 2> /dev/null || : > "$journal"
pincharge="$dir/pincharge.txt"
: > "$pincharge"
if [ "$store" -eq 1 ]; then
  # Every pin-charge line of the unit, to find the current enforce epoch.
  journalctl --user -u aira-daemon.service --since "8 days ago" -o short-unix 2> /dev/null \
    | awk '/pin-charge (observe|enforce):/' > "$pincharge" || true
  cp "$HOME/.local/state/aira/state.db" "$dir/state.db"
  if [ -e "$HOME/.local/state/aira/state.db-wal" ]; then
    cp "$HOME/.local/state/aira/state.db-wal" "$dir/state.db-wal"
  else
    rm -f "$dir/state.db-wal"
  fi
fi

exec python3 -I - "$cg" "$dir" "$list_json" "$journal" "$minutes" "$store" "$baseline" "$pincharge" "$slice_since" <<'PY'
import json, os, re, sqlite3, sys, time

cg, d, list_json, journal, minutes, store, baseline, pincharge, slice_since = sys.argv[1:]
now = time.time()
MAX_GAP = 900          # samples further apart than this break a 1- or 2-hour window
C4_CEILING = int(57.6 * 2**30)   # ledger ceiling above which L / 0.9 exceeds MemoryMax (64 GiB)
COUNTERS = ("oom_local", "max_local", "oom_kill_local", "oom_kill", "high")
samples_path = os.path.join(d, "samples.tsv")


def cleanup():
    # The list, the journal excerpts and the store copy name other sessions' commands: keep none.
    for p in (list_json, journal, pincharge, os.path.join(d, "state.db"), os.path.join(d, "state.db-wal"),
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


def p99(xs):
    s = sorted(xs)
    return s[min(len(s) - 1, int(0.99 * len(s)))] if s else None


def read_samples():
    """[(t, {counter: value}, avg10, ledger ceiling bytes or -1)] in time order; lines of
    another format are skipped."""
    rows = []
    if os.path.exists(samples_path):
        with open(samples_path) as f:
            for line in f:
                p = line.rstrip("\n").split("\t")
                if len(p) == 3 + len(COUNTERS) and p[0][:1].isdigit():
                    rows.append((float(p[0]), dict(zip(COUNTERS, map(int, p[1:-2]))), float(p[-2]), int(p[-1])))
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
try:
    with open(list_json) as f:
        sr = json.load(f)["data"]["slice_reserve"]
    line["queued"], line["running"] = sr.get("queued"), sr.get("jobs")
    line["granted_gib"] = round(sr["granted_bytes"] / 2**30, 1)
    line["ceiling_gib"] = round(sr["ceiling_bytes"] / 2**30, 1)
    ceiling = int(sr["ceiling_bytes"])
except Exception:
    line["queued"] = line["running"] = line["granted_gib"] = line["ceiling_gib"] = "unevaluated"
    ceiling = -1
obs = enf = disc = unfit = 0
with open(journal, errors="replace") as f:
    for l in f:
        if "pin-charge observe:" in l:
            obs += 1
        m = re.search(r"pin-charge enforce: scope=\S+ pin=(\d+) charged=(\d+)", l)
        if m:
            enf += 1
            disc += int(m.group(2)) < int(m.group(1))
        if "pin-charge" in l and "physical-fit=false" in l:
            unfit += 1
line.update({"minutes": int(minutes), "observe": obs, "enforce": enf, "discounted": disc, "phys_unfit": unfit})

if store == "1":
    epoch, fac = None, None
    with open(pincharge, errors="replace") as f:
        for l in f:
            m = re.search(r"pin-charge (observe|enforce): .*factor=([0-9.]+)", l)
            if not m:
                continue
            t = float(l.split(" ", 1)[0])
            if m.group(1) == "observe":
                epoch, fac = None, None
            elif fac != m.group(2):
                epoch, fac = t, m.group(2)
    if epoch is None:
        line["above_charge"] = "unevaluated (the journal's latest pin-charge line is not enforce, or none in 8 days)"
    else:
        pct = int(round(float(fac) * 100))
        lo = max(epoch, now - 7 * 86400)
        c = sqlite3.connect("file:%s?mode=ro" % os.path.join(d, "state.db"), uri=True)
        n, above, oom = c.execute(
            "select count(*), sum(peak_rss > (budget * ? + 99) / 100), sum(oom) from confine_peak_history "
            "where kind = 'confine' and budget_basis = 'cap:operator:--memory-reserve' and started_at_ms >= ?",
            (pct, int(lo * 1000))).fetchone()
        c.close()
        line["above_charge"] = {"factor": float(fac), "since": int(lo), "rows": n, "above": above or 0,
                                "oom": oom or 0, "share": round((above or 0) / n, 4) if n else "unevaluated"}

trig = []
if isinstance(line["d_oom_local"], int) and line["d_oom_local"] > 0:
    trig.append("slice-level oom (memory.events.local oom rose; victim and its peak/pin unattributed)")
allrows = rows + [(now, cur, a10, ceiling)]
# C4 (report section 8): the ledger ceiling above 57.6 GiB for a whole, continuously sampled hour.
hour = [r for r in allrows if r[0] >= now - 3600]
if not continuous(allrows, now - 3600, now) or any(r[3] < 0 for r in hour):
    trig.append("C4 ceiling: unevaluated (samples or ceiling readings do not cover the last hour)")
elif all(r[3] > C4_CEILING for r in hour):
    trig.append("C4: ledger ceiling above 57.6 GiB for 1 h (switch OOM steering on or roll back)")
bl_path = os.path.join(d, "baseline.json")
if os.path.exists(bl_path):
    with open(bl_path) as f:
        bl = json.load(f)
    if isinstance(bl.get("some_avg10_p99"), (int, float)):
        w = [r for r in allrows if r[0] >= now - 7200]
        if continuous(allrows, now - 7200, now) and all(r[2] > bl["some_avg10_p99"] for r in w):
            trig.append("memory.pressure some avg10 over baseline p99 for 2 h")
        elif not continuous(allrows, now - 7200, now):
            trig.append("pressure: unevaluated (samples do not cover the last 2 h)")
    else:
        trig.append("pressure: unevaluated (no baseline p99)")
    if isinstance(bl.get("high_p99_per_hour"), (int, float)):
        h2 = window_delta(allrows, "high", now - 7200, now - 3600)
        h1 = window_delta(allrows, "high", now - 3600, now)
        if h1 is None or h2 is None:
            trig.append("high: unevaluated (samples do not cover the last 2 h)")
        elif h1 > bl["high_p99_per_hour"] and h2 > bl["high_p99_per_hour"]:
            trig.append("memory.events high over baseline p99 for 2 h")
    else:
        trig.append("high: unevaluated (no baseline p99)")
else:
    trig.append("pressure/high: unevaluated (run --baseline first)")
ac = line.get("above_charge")
if isinstance(ac, dict) and isinstance(ac.get("share"), float) and ac["rows"] >= 100 and ac["share"] > 0.05:
    trig.append("above-charge share > 5%")
line["triggers"] = trig

with open(samples_path, "a") as f:
    f.write("%.0f\t%s\t%s\t%d\n" % (now, "\t".join(str(cur[k]) for k in COUNTERS), a10, ceiling))
cleanup()
print(json.dumps(line))
PY
