#!/usr/bin/env bash
# aira204-loadloop.sh — run one compiled Go test binary in a loop beside CPU burners
# pinned to the same CPUs, and print pass/fail/skip counts (AIRA-204 / AIRA-199).
#
# Usage (always under confinement, one heavy job at a time):
#   go test -c -o ~/tmp/aira-flakes/bin/daemon.race.test -race ./internal/daemon
#   aira confine --name <unique> -- docs/dev/aira204-loadloop.sh \
#       <label> <package-dir> <cpus> <burners> <test-binary> <run-regex> <count> [extra test flags...]
#
#   label        name of the log file written under $AIRA_FLAKE_OUT (default ~/tmp/aira-flakes/runs)
#   package-dir  the test's package source directory (Go tests expect it as the cwd)
#   cpus         taskset CPU list shared by the test and the burners, e.g. 2,3 or 0-15
#   burners      number of busy-loop processes pinned to <cpus> (0 = no artificial load)
#   count        -test.count; the binary is always run with -test.v so each run is counted
#
# The test binary inherits the CPU affinity, so Go sets GOMAXPROCS to the number of
# CPUs in <cpus> (2 for "2,3"). The burners are plain `sh` busy loops; they are
# killed on exit, including when the test binary fails or times out.
#
# Counts: expected = (top-level tests matching <run-regex>) x <count>; evaluated =
# pass + fail; skip is reported apart (a skip evaluates nothing); aborted = expected
# minus every counted outcome (a panic or timeout ends the binary early).
# Exit status: the test binary's own status when it is non-zero; otherwise 3 when
# nothing was evaluated (zero failures out of zero runs proves nothing) and 4 when
# runs are missing (aborted > 0); 0 only when every expected run was evaluated or
# skipped and the binary itself exited 0.
set -euo pipefail

if (($# < 7)); then
	sed -n '2,16p' "$0" >&2
	exit 2
fi
label=$1 pkgdir=$2 cpus=$3 burners=$4 bin=$5 regex=$6 count=$7
shift 7
outdir=${AIRA_FLAKE_OUT:-$HOME/tmp/aira-flakes/runs}
mkdir -p "$outdir"
log="$outdir/$label.log"

if ! listing=$(cd "$pkgdir" && "$bin" -test.list "$regex"); then
	printf '%s: cannot list the tests in %s\n' "$label" "$bin" >&2
	exit 2
fi
n_tests=$(grep -c '^Test' <<<"$listing" || true)
if ((n_tests == 0)); then
	printf '%s: no top-level test matches %s\n' "$label" "$regex" >&2
	exit 2
fi
expected=$((n_tests * count))

pids=()
cleanup() {
	local pid
	for pid in "${pids[@]}"; do
		kill "$pid" 2>/dev/null || true
	done
}
trap cleanup EXIT

for _ in $(seq 1 "$burners"); do
	taskset -c "$cpus" sh -c 'while :; do :; done' &
	pids+=("$!")
done

start=$(date +%s)
set +e
(cd "$pkgdir" && taskset -c "$cpus" "$bin" -test.run "$regex" -test.count "$count" -test.timeout 30m -test.v "$@") >"$log" 2>&1
rc=$?
set -e
end=$(date +%s)

pass=$(grep -c -- '^--- PASS' "$log" || true)
fail=$(grep -c -- '^--- FAIL' "$log" || true)
skip=$(grep -c -- '^--- SKIP' "$log" || true)
evaluated=$((pass + fail))
aborted=$((expected - pass - fail - skip))
printf '%s rc=%d expected=%d evaluated=%d fail=%d skip=%d aborted=%d wall=%ss cpus=%s burners=%s count=%s log=%s\n' \
	"$label" "$rc" "$expected" "$evaluated" "$fail" "$skip" "$aborted" "$((end - start))" "$cpus" "$burners" "$count" "$log"
# The failure message is printed by -test.v BEFORE its "--- FAIL" line.
grep -B3 -- '^--- FAIL' "$log" | grep -E '_test\.go:[0-9]+:' | sed 's/^[[:space:]]*//' | sort | uniq -c || true
grep -m3 -E '^panic:|test timed out' "$log" || true

if ((rc != 0)); then
	exit "$rc"
fi
if ((evaluated == 0)); then
	printf '%s UNEVALUATED: no run was evaluated (expected=%d skip=%d)\n' "$label" "$expected" "$skip" >&2
	exit 3
fi
if ((aborted != 0)); then
	printf '%s INCOMPLETE: %d expected runs have no outcome\n' "$label" "$aborted" >&2
	exit 4
fi
