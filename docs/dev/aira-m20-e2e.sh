#!/usr/bin/env bash
set -euo pipefail

aira_bin=${1:-./aira}
case "$aira_bin" in
  /*) ;;
  *) aira_bin="$(pwd)/$aira_bin" ;;
esac

work=$(mktemp -d "${TMPDIR:-/tmp}/aira-m20-e2e.XXXXXX")
cleanup() {
  if [[ -n ${run_id:-} ]]; then
    "$aira_bin" run-kill "$run_id" --steal >/dev/null 2>&1 || true
  fi
  rm -rf -- "$work"
}
trap cleanup EXIT

# cgroup.procs is a kernel pseudo-file: stat() reports st_size == 0 even when it
# has member PIDs, so `[ -s ... ]` is ALWAYS false here. Emptiness must be tested
# by reading the file, not by its stat size.
scope_populated() {
  local sc=$1
  [[ -n $sc && -r $sc/cgroup.procs ]] || return 1
  grep -q '[0-9]' "$sc/cgroup.procs"
}

cd "$work"
# Isolate the machine-wide state (prefix-ownership registry, ledger, DB) so this
# e2e never collides with the real AIRA project's registrations on this box.
export XDG_STATE_HOME="$work/state"
mkdir -p "$XDG_STATE_HOME"
git init -q
"$aira_bin" init --project m20-e2e --prefix DTCH >/dev/null

"$aira_bin" run --detach --json -- /bin/sh -c 'sleep 30' >handle.json &
launcher_pid=$!
wait "$launcher_pid"
if kill -0 "$launcher_pid" 2>/dev/null; then
  echo "launcher still alive after returning the detached handle" >&2
  exit 1
fi

run_id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' handle.json)
handle_status=$(sed -n 's/.*"status":"\([^"]*\)".*/\1/p' handle.json | head -n1)
if [[ -z $run_id ]]; then
  echo "detached handle did not contain a run id" >&2
  exit 1
fi
# The launcher returns at "starting": readiness is signalled after the ledger
# "starting" record but BEFORE the shim's (possibly long) admission + scope
# create + Start. So poll for the shim to place the child in the scope
# (starting -> running) instead of assuming it is already scoped.
if [[ $handle_status != starting && $handle_status != running ]]; then
  echo "detached handle had unexpected status: $handle_status" >&2
  exit 1
fi

scope=""
supervisor_pid=""
for _ in $(seq 1 100); do
  "$aira_bin" --json reconcile >reconcile.json
  scope=$(sed -n 's/.*"cgroup_scope":"\([^"]*\)".*/\1/p' reconcile.json | head -n1)
  supervisor_pid=$(sed -n 's/.*"supervisor_pid":{"pid":\([0-9][0-9]*\).*/\1/p' reconcile.json | head -n1)
  if scope_populated "$scope"; then
    break
  fi
  sleep 0.1
done
if [[ -z $scope || ! -r $scope/cgroup.procs ]]; then
  echo "detached scope was not observable" >&2
  exit 1
fi
if ! scope_populated "$scope"; then
  echo "detached child was not placed in the scope after polling for running" >&2
  exit 1
fi
if [[ -n $supervisor_pid ]] && grep -qx "$supervisor_pid" "$scope/cgroup.procs"; then
  echo "supervisor incorrectly entered the child kill scope" >&2
  exit 1
fi

# run-kill on a run it successfully kills reports the killed terminal as
# {"ok":false,"code":"E_RUN_KILLED"} and exits non-zero (a killed run is not a
# clean success) — that IS the success signal here; the killed-status poll below
# is the real assertion.
"$aira_bin" run-kill "$run_id" >/dev/null 2>&1 || true
for _ in $(seq 1 100); do
  "$aira_bin" --json reconcile >reconcile.json
  if grep -q '"status":"killed"' reconcile.json; then
    echo "PASS: detached launcher exited, child was scoped, shim stayed outside, run-kill finalized killed"
    exit 0
  fi
  sleep 0.05
done

echo "detached run did not finalize killed" >&2
exit 1
