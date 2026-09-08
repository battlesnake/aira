#!/usr/bin/env bash
# Reproduction for the AIRA-173 failure-rate measurement.
#
# TestRunInputServerACKReconnectAppendAndExplicitClose loses a race between its
# reconnect's HELLO write and the server's BUSY refuse-and-close. The lever is
# CPU oversubscription — the client goroutine must be descheduled between
# connect() and its first write() — so this harness runs the test at high count
# against N busy loops rather than trying to schedule the whole suite.
#
# Measured with this script on a 16-core box (see the AIRA-173 ticket):
#
#   before the fix   32 burners x 20000 -> 4 failures
#                    64 burners x 30000 -> 3 failures      (7 / 50,000, 0.014%)
#   after the fix    32 burners x 20000 -> 0 failures
#                    64 burners x 30000 -> 0 failures      (0 / 50,000)
#
# Rates are per test execution and are hardware- and load-dependent; treat the
# numbers as the order of magnitude they establish, not a constant. A run that
# reports 0 failures on unfixed code is NOT evidence of absence at these rates —
# 20,000 executions at p=1.4e-4 miss it about 6% of the time.
#
# Usage: docs/dev/aira-173-flake-rate-repro.sh [count] [burners]
set -uo pipefail

count="${1:-20000}"
burners="${2:-32}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
log_dir="${AIRA_173_REPRO_DIR:-${HOME}/tmp/aira173-repro}"
mkdir -p "${log_dir}"
log="${log_dir}/run-${burners}burners-${count}.log"

test_name=TestRunInputServerACKReconnectAppendAndExplicitClose

echo "AIRA-173: ${count} executions of ${test_name} against ${burners} CPU burners"
echo "log: ${log}"

pids=()
for _ in $(seq 1 "${burners}"); do
  ( while :; do :; done ) &
  pids+=($!)
done
# Burners are memory-trivial but must never outlive the measurement.
trap 'kill "${pids[@]}" 2>/dev/null' EXIT INT TERM

cd "${repo_root}"
go test ./internal/runner/ -run "${test_name}" -count="${count}" -timeout 60m > "${log}" 2>&1
go_test_exit=$?

kill "${pids[@]}" 2>/dev/null
wait 2>/dev/null

failures="$(grep -c -- '--- FAIL' "${log}")"
echo
echo "go test exit: ${go_test_exit}"
echo "executions:   ${count}"
echo "failures:     ${failures}"
if [[ "${failures}" -gt 0 ]]; then
  echo "signatures:"
  sed 's/case-[0-9]*/case-N/; s/RUN-1-[0-9a-f]*/RUN-1-NONCE/' "${log}" \
    | grep 'run_input_server_linux_test.go:' | sort | uniq -c
fi

# The measurement itself is heavy: run it under `aira confine`, e.g.
#   aira confine -- docs/dev/aira-173-flake-rate-repro.sh 20000 32
# and stop a stuck run with `kill <the aira confine PID>`, never by stopping
# aira.slice, which would kill every other session's jobs too.
