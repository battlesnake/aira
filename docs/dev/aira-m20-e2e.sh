#!/usr/bin/env bash
set -euo pipefail

aira_bin=${1:-./aira}
case "$aira_bin" in
  /*) ;;
  *) aira_bin="$(pwd)/$aira_bin" ;;
esac

work=$(mktemp -d /home/user/tmp/aira-m20-e2e.XXXXXX)
cleanup() {
  if [[ -n ${run_id:-} ]]; then
    "$aira_bin" run-kill "$run_id" --steal >/dev/null 2>&1 || true
  fi
  rm -rf -- "$work"
}
trap cleanup EXIT

cd "$work"
git init -q
"$aira_bin" init --project m20-e2e --prefix AIRA >/dev/null

"$aira_bin" run --detach --json -- /bin/sh -c 'sleep 30' >handle.json &
launcher_pid=$!
wait "$launcher_pid"
if kill -0 "$launcher_pid" 2>/dev/null; then
  echo "launcher still alive after returning the detached handle" >&2
  exit 1
fi

run_id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' handle.json)
if [[ -z $run_id ]]; then
  echo "detached handle did not contain a run id" >&2
  exit 1
fi

"$aira_bin" --json reconcile >reconcile.json
scope=$(sed -n 's/.*"cgroup_scope":"\([^"]*\)".*/\1/p' reconcile.json | head -n1)
supervisor_pid=$(sed -n 's/.*"supervisor_pid":{"pid":\([0-9][0-9]*\).*/\1/p' reconcile.json | head -n1)
if [[ -z $scope || ! -r $scope/cgroup.procs ]]; then
  echo "detached scope was not observable" >&2
  exit 1
fi
if [[ ! -s $scope/cgroup.procs ]]; then
  echo "launcher exited but detached child is not in the scope" >&2
  exit 1
fi
if [[ -n $supervisor_pid ]] && grep -qx "$supervisor_pid" "$scope/cgroup.procs"; then
  echo "supervisor incorrectly entered the child kill scope" >&2
  exit 1
fi

"$aira_bin" run-kill "$run_id" >/dev/null
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
