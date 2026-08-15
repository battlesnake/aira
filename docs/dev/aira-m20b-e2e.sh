#!/usr/bin/env bash
set -euo pipefail

aira_bin=${1:-./aira}
case "$aira_bin" in
  /*) ;;
  *) aira_bin="$(pwd)/$aira_bin" ;;
esac

work=$(mktemp -d /home/user/tmp/aira-m20b-e2e.XXXXXX)
cleanup() {
  rm -rf -- "$work"
}
trap cleanup EXIT

cd "$work"
export XDG_STATE_HOME="$work/state"
mkdir -p "$XDG_STATE_HOME"
git init -q
git config user.name "AIRA M20b e2e"
git config user.email "aira-m20b@example.invalid"
printf 'before\n' >fixture.txt
git add fixture.txt
git commit -qm before
launch_commit=$(git rev-parse HEAD)
"$aira_bin" init --project m20b-e2e --prefix DTEL >/dev/null

cat >usage.json <<'JSON'
{"usage":{"input_tokens":100,"cached_input_tokens":25,"output_tokens":20,"reasoning_output_tokens":7}}
JSON

child='printf '\''%s\n'\'' '\''{"Action":"start","Package":"example/pkg"}'\'' '\''{"Action":"run","Package":"example/pkg","Test":"TestPass"}'\'' '\''{"Action":"pass","Package":"example/pkg","Test":"TestPass","Elapsed":0.001}'\'' '\''{"Action":"pass","Package":"example/pkg"}'\''; git commit --allow-empty -qm after'
"$aira_bin" run --detach --json \
  --report go-json --suite unit --config-env MODE=e2e --shard 1/1 \
  --tool codex --usage "$work/usage.json" --provider codex \
  -- /bin/sh -c "$child" >handle.json

run_id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' handle.json | head -n1)
if [[ -z $run_id ]] || ! grep -q '"telemetry":"pending"' handle.json; then
  echo "detached handle did not durably advertise pending telemetry" >&2
  exit 1
fi

for _ in $(seq 1 200); do
  "$aira_bin" --json get "$run_id" >run.json
  if grep -q '"telemetry":"complete"' run.json; then
    break
  fi
  sleep 0.05
done
if ! grep -q '"status":"exited"' run.json || ! grep -q '"telemetry":"complete"' run.json; then
  echo "detached report+usage run did not terminalize with complete telemetry" >&2
  cat run.json >&2
  exit 1
fi

"$aira_bin" --json test-report ls >reports.json
"$aira_bin" --json spend ls >compute.json
python3 - "$launch_commit" <<'PY'
import json, sys
launch_commit = sys.argv[1]
reports = json.load(open("reports.json"))["data"]
rows = reports["rows"]
assert reports["total"] == 1, reports
report = rows[0]
assert report["commit"] == launch_commit, report
assert report["parser_complete"] is True, report
assert [r["name"] for r in report["results"]] == ["example/pkg/TestPass"], report
compute = json.load(open("compute.json"))["data"]
assert compute["total"] == 1, compute
event = compute["rows"][0]
assert event["model"] == "codex" and event["provider"] == "codex", event
assert event["buckets"]["fresh_input"] == 75 and event["buckets"]["cache_read"] == 25, event
assert event["buckets"]["output"] == 20 and event["buckets"]["reasoning"] == 7, event
PY

"$aira_bin" run --detach --json --tool codex -- /bin/true >tool-handle.json
tool_run=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' tool-handle.json | head -n1)
for _ in $(seq 1 200); do
  "$aira_bin" --json get "$tool_run" >tool-run.json
  if grep -q '"telemetry":"complete"' tool-run.json; then
    break
  fi
  sleep 0.05
done
"$aira_bin" --json spend ls >compute-two.json
python3 <<'PY'
import json
compute = json.load(open("compute-two.json"))["data"]
assert compute["total"] == 2, compute
assert any(row["conservation"] == "unevaluated" and not row["buckets"] for row in compute["rows"]), compute
PY

echo "PASS: detached pending→complete, pre-launch VCS provenance, report parsing, authoritative usage, and tool-only unevaluated tokens"
