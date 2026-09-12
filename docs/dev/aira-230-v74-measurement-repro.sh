#!/usr/bin/env bash
# AIRA-230 v0.7 S1 slice v7-4 — measurement harness (dogfood, real cgroup).
#
# Drives the branch-exit-gate measurement test TestRealPytestAitestMeasurementReport
# (internal/pylib/pytest_aitest_measurement_e2e_test.go), which stands up a real
# private protocol-11 daemon + a real delegated outer cgroup, runs a real CONFINED
# aitest pool with the measurement report enabled (AIRA_AITEST_MEASURE_DIR), and
# logs the structured pool-report.json + per-worker memory.current sidecars. It
# reuses the reads the pool already makes (per-worker memory.peak/oom at retirement,
# per-test memory.current) plus ONE new read (.aira-supervisor/memory.peak); it is
# NOT a time-series sampler.
#
# Two sub-runs (plan D3):
#   1. DEFAULT window (600 s / 200 tests) — the residue-over-a-worker's-life run.
#   2. SHORT window (AIRA_AITEST_WORKER_MAX_SECONDS=10) — the turnover run; the age
#      cap recycles a worker mid-suite, so the report shows >1 scoped worker. This
#      sets ONLY that sub-run's window; it does NOT change the shipped default.
#
# The numbers this prints set v0.7's field-tunables (docs/dev/aira-230-v74-measurement-report.md
# records a captured baseline): the 256 MiB unannotated default, the per-worker
# headroom, the outer-cap allowance base/per_relay + margin, the watermark fraction,
# and the MAX_TESTS fork.
#
# HONESTY: every value the kernel does not expose is reported "unevaluated", never a
# fabricated 0 — do not read a missing reading as zero.
#
# The measurement is heavy: run the WHOLE script under `aira confine --`, e.g.
#   aira confine -- docs/dev/aira-230-v74-measurement-repro.sh
# and stop a stuck run with `kill <the aira confine PID>` you started — NEVER
# `systemctl --user stop aira.slice`, which kills every other session's jobs too.
#
# -e is omitted deliberately (as in aira-173-flake-rate-repro.sh): each `go test`
# exit code is captured and reported rather than aborting the harness, so a
# non-zero run still prints whatever report it produced.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
log_dir="${AIRA_230_V74_REPRO_DIR:-${HOME}/tmp/aira230-v74-measure}"
mkdir -p "${log_dir}"

test_name=TestRealPytestAitestMeasurementReport
pkg=./internal/pylib/

cd "${repo_root}" || { echo "cannot cd to repo root ${repo_root}" >&2; exit 1; }

# Extract the go-test -v log blocks the measurement test emits (pool-report.json
# and each worker-<pid>.tsv), stripping the leading indentation `go test` adds.
print_report() {
  local log="$1"
  echo "--- pool-report.json ---"
  sed -n '/pool-report.json:/,/^[^[:space:]].*\.go:/p' "${log}" \
    | sed -n '/{/,/}/p' | sed 's/^[[:space:]]*//'
  echo "--- per-worker memory.current sidecars (nodeid<TAB>memory.current-or-unevaluated<TAB>test#) ---"
  # Select the sidecar header (worker-<pid>.tsv:) and each TAB-delimited data
  # line, NOT pytest's own "<nodeid> passed" progress lines. awk's regex \t is a
  # real tab (grep -E's is not), so a data line is "::test_" AND a tab.
  awk '/worker-[0-9]+\.tsv:/ || (/::test_/ && /\t/) { sub(/^[[:space:]]+/, ""); print }' "${log}"
}

run_one() {
  local label="$1"; shift
  local log="${log_dir}/${label}.log"
  echo "=============================================================="
  echo "AIRA-230 v7-4 measurement — ${label} window"
  echo "log: ${log}"
  # "$@" carries any per-sub-run env assignments (e.g. window overrides) via env.
  env "$@" go test -run "${test_name}" -v -count=1 -timeout 10m "${pkg}" > "${log}" 2>&1
  local exit_code=$?
  echo "go test exit: ${exit_code}"
  if grep -q -- '--- SKIP' "${log}"; then
    echo "SKIPPED (no real-cgroup delegation on this host — the measurement is UNEVALUATED here, not zero):"
    grep -A2 -- '--- SKIP' "${log}" | sed 's/^[[:space:]]*//' | head -6
    return 0
  fi
  print_report "${log}"
  return "${exit_code}"
}

overall=0
# 1 worker: the residue-over-a-worker's-life baseline + the allowance base datapoint.
run_one "default"     || overall=1
# 10 s window: turnover (the age cap recycles a worker mid-suite -> >1 scoped worker).
run_one "short10s" AIRA_AITEST_WORKER_MAX_SECONDS=10 || overall=1
# 4 workers: the FULL pool under this harness's 256 MiB outer cap + v7-1 guard
# constants (the guard admits ~4x32 MiB workers). supervisor_peak_rss here minus
# the 1-worker run isolates the per-relay allowance term (relays are charged to
# .aira-supervisor). See docs/dev/aira-230-v74-measurement-report.md.
run_one "fullpool4" AIRA_AITEST_MEASURE_WORKERS=4 || overall=1

echo "=============================================================="
echo "overall harness exit: ${overall}"
echo "logs under: ${log_dir}"
exit "${overall}"
