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
# writes ONLY under ~/tmp/aira-324-watch/ (state, samples, baseline, and with --store a
# copy of the store). It changes nothing in aira, systemd or the store. Safe from cron or a
# loop, for example every 5 minutes:
#   */5 * * * * /path/to/docs/dev/aira-324-enforce-watch.sh >> ~/tmp/aira-324-watch/watch.log 2>&1
#
# Fields: oom_kill_local / high_local are aira.slice's OWN events (memory.events.local:
# the slice hit its limit; an oom_kill here is a slice-level kill, the rollback trigger),
# oom_kill / high are hierarchical (memory.events: they include every job killed at its
# own --memory-reserve cap, which happens today and is not a trigger). d_* are deltas
# since the previous sample. some_avg10 is memory.pressure (also hierarchical: a job
# stalling at its own cap raises it). queued / granted / ceiling are from confine --list.
# observe / enforce / discounted / phys_unfit count daemon log lines in the last N minutes.
# With --store: above_charge share of pin rows (cap:operator:--memory-reserve) that ended
# since the first enforce line, at the factor that line states (the 5%/week trigger).
# triggers lists any rollback trigger that is MET; "unevaluated" ones are named as such.
set -euo pipefail

minutes=10
store=0
baseline=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --minutes) minutes="$2"; shift 2 ;;
    --store) store=1; shift ;;
    --baseline) baseline=1; shift ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
case "$minutes" in ''|*[!0-9]*) echo "--minutes must be a whole number" >&2; exit 2 ;; esac

dir="${AIRA_324_WATCH_DIR:-$HOME/tmp/aira-324-watch}"
mkdir -p "$dir"
cg="/sys/fs/cgroup$(systemctl --user show aira.slice -p ControlGroup --value)"
[ -r "$cg/memory.events" ] || { echo "cannot read $cg/memory.events" >&2; exit 3; }
slice_since="$(systemctl --user show aira.slice -p ActiveEnterTimestamp --value)"

list_json="$dir/list.json"
if ! aira confine --list > "$list_json" 2> /dev/null; then
  : > "$list_json"
fi
journal="$dir/journal.txt"
journalctl --user -u aira-daemon.service --since "${minutes} min ago" -o cat > "$journal" 2> /dev/null || : > "$journal"
first_enforce=""
if [ "$store" -eq 1 ]; then
  # The first enforce line in the unit's journal: its timestamp and factor. awk stops
  # early, so journalctl may die of SIGPIPE: tolerated.
  first_enforce="$(journalctl --user -u aira-daemon.service -o short-unix 2> /dev/null \
    | awk '/pin-charge enforce:/ {print; exit}' || true)"
  cp "$HOME/.local/state/aira/state.db" "$dir/state.db"
  if [ -e "$HOME/.local/state/aira/state.db-wal" ]; then
    cp "$HOME/.local/state/aira/state.db-wal" "$dir/state.db-wal"
  else
    rm -f "$dir/state.db-wal"
  fi
fi

exec python3 -I - "$cg" "$dir" "$list_json" "$journal" "$minutes" "$store" "$baseline" "$first_enforce" "$slice_since" <<'PY'
import json, os, re, sqlite3, sys, time

cg, d, list_json, journal, minutes, store, baseline, first_enforce, slice_since = sys.argv[1:]
now = time.time()


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
    return None


def p99(xs):
    s = sorted(xs)
    return s[min(len(s) - 1, int(0.99 * len(s)))] if s else None


samples_path = os.path.join(d, "samples.tsv")


def cleanup():
    # The list, the journal excerpt and the store copy name other sessions' commands: keep none.
    for p in (list_json, journal, os.path.join(d, "state.db"), os.path.join(d, "state.db-wal")):
        if os.path.exists(p):
            os.remove(p)


def read_samples():
    rows = []
    if os.path.exists(samples_path):
        with open(samples_path) as f:
            for line in f:
                p = line.split("\t")
                if len(p) >= 6 and p[0][0].isdigit():
                    rows.append((float(p[0]), int(p[1]), int(p[2]), int(p[3]), int(p[4]), float(p[5])))
    return rows


if baseline == "1":
    rows = read_samples()
    ev = events("memory.events")
    evl = events("memory.events.local")
    out = {"at": int(now), "slice_active_since": slice_since}
    # memory.events counts are cumulative since the slice was created: a cumulative 0 proves
    # every hourly delta since then was 0, so its p99 is 0 over that span (not a week).
    if ev["high"] == 0:
        out["high_p99_per_hour"] = 0
        out["high_basis"] = "cumulative high = 0 since slice creation, so every hourly delta was 0 (span shorter than the spec's week)"
    else:
        hours = {}
        for r in rows:
            hours.setdefault(int(r[0] // 3600), []).append(r[3])
        deltas = [max(v) - min(v) for v in hours.values() if len(v) > 1]
        out["high_p99_per_hour"] = p99(deltas) if len(deltas) >= 24 else "unevaluated"
        out["high_basis"] = "%d sampled hours" % len(deltas)
    span_h = (rows[-1][0] - rows[0][0]) / 3600 if len(rows) > 1 else 0
    if len(rows) >= 24 and span_h >= 2:
        out["some_avg10_p99"] = p99([r[5] for r in rows])
        out["some_avg10_basis"] = "%d samples over %.1f h (spec wants a week: weaker baseline)" % (len(rows), span_h)
    else:
        out["some_avg10_p99"] = "unevaluated"
        out["some_avg10_basis"] = ("no memory.pressure history exists before this script's samples; "
                                   "have %d samples over %.1f h, need >= 24 over >= 2 h" % (len(rows), span_h))
    out["oom_kill_local_since_slice_creation"] = evl["oom_kill"]
    with open(os.path.join(d, "baseline.json"), "w") as f:
        json.dump(out, f)
    cleanup()
    print(json.dumps(out))
    sys.exit(0)

ev = events("memory.events")
evl = events("memory.events.local")
a10 = avg10()
state_path = os.path.join(d, "last.json")
last = {}
if os.path.exists(state_path):
    with open(state_path) as f:
        last = json.load(f)
cur = {"at": now, "oom_kill": ev["oom_kill"], "oom_kill_local": evl["oom_kill"], "high": ev["high"],
       "high_local": evl["high"]}
line = {"at": time.strftime("%Y-%m-%dT%H:%M:%S%z", time.localtime(now))}
for k in ("oom_kill_local", "oom_kill", "high_local", "high"):
    line[k] = cur[k]
    line["d_" + k] = (cur[k] - last[k]) if k in last else "unevaluated"
line["since_last_s"] = int(now - last["at"]) if "at" in last else "unevaluated"
line["some_avg10"] = a10
try:
    with open(list_json) as f:
        sr = json.load(f)["data"]["slice_reserve"]
    line["queued"] = sr.get("queued")
    line["running"] = sr.get("jobs")
    line["granted_gib"] = round(sr["granted_bytes"] / 2**30, 1)
    line["ceiling_gib"] = round(sr["ceiling_bytes"] / 2**30, 1)
except Exception:
    line["queued"] = line["running"] = line["granted_gib"] = line["ceiling_gib"] = "unevaluated"
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
    if not first_enforce:
        line["above_charge"] = "unevaluated (no enforce line in the journal)"
    else:
        m = re.search(r"factor=([0-9.]+)", first_enforce)
        t0 = float(first_enforce.split(" ", 1)[0])
        if not m:
            line["above_charge"] = "unevaluated (the first enforce line states no factor)"
        else:
            fl = float(m.group(1))
            pct = int(round(fl * 100))
            c = sqlite3.connect("file:%s?mode=ro" % os.path.join(d, "state.db"), uri=True)
            # Rows that STARTED after the first enforce grant: a factor change after that is
            # not seen here (the line names the factor it judged against).
            n, above, oom = c.execute(
                "select count(*), sum(peak_rss > (budget * ? + 99) / 100), sum(oom) from confine_peak_history "
                "where kind = 'confine' and budget_basis = 'cap:operator:--memory-reserve' and started_at_ms >= ?",
                (pct, int(t0 * 1000))).fetchone()
            line["above_charge"] = {"factor": fl, "rows": n, "above": above or 0, "oom": oom or 0,
                                    "share": round((above or 0) / n, 4) if n else "unevaluated"}

trig = []
if line["d_oom_kill_local"] not in ("unevaluated", 0):
    trig.append("slice-level oom_kill")
bl_path = os.path.join(d, "baseline.json")
rows = read_samples()
if os.path.exists(bl_path):
    with open(bl_path) as f:
        bl = json.load(f)
    window = [r for r in rows if r[0] >= now - 7200] + [(now, 0, 0, cur["high"], 0, a10 or 0.0)]
    span = window[-1][0] - window[0][0]
    if isinstance(bl.get("some_avg10_p99"), (int, float)):
        if span >= 6900 and all(r[5] > bl["some_avg10_p99"] for r in window):
            trig.append("memory.pressure some avg10 over baseline p99 for 2 h")
    else:
        trig.append("pressure: unevaluated (no baseline)")
    if isinstance(bl.get("high_p99_per_hour"), (int, float)):
        h1 = [r for r in window if r[0] >= now - 3600]
        h2 = [r for r in window if r[0] < now - 3600]
        if span >= 6900 and h1 and h2 and (h1[-1][3] - h1[0][3]) > bl["high_p99_per_hour"] \
                and (h2[-1][3] - h2[0][3]) > bl["high_p99_per_hour"]:
            trig.append("memory.events high over baseline p99 for 2 h")
else:
    trig.append("pressure/high: unevaluated (run --baseline first)")
ac = line.get("above_charge")
if isinstance(ac, dict) and isinstance(ac.get("share"), float) and ac["rows"] >= 100 and ac["share"] > 0.05:
    trig.append("above-charge share > 5%")
line["triggers"] = trig

with open(samples_path, "a") as f:
    f.write("%.0f\t%d\t%d\t%d\t%d\t%s\n" % (now, cur["oom_kill_local"], cur["oom_kill"], cur["high"], cur["high_local"],
                                            a10 if a10 is not None else "nan"))
with open(state_path + ".tmp", "w") as f:
    json.dump(cur, f)
os.replace(state_path + ".tmp", state_path)
cleanup()
print(json.dumps(line))
PY
