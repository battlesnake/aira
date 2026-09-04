#!/usr/bin/env bash
# AIRA-24: what one queue-position probe costs the daemon.
#
# A job blocked in confine admission asks the daemon for its queue position
# once per progress line (every 15s) by issuing one `confine-list` request.
# Build review asked whether that is the AIRA-61 class of defect (an O(tree)
# scan on a hot path, measured there at 25-65% CPU). This measures it instead
# of arguing about it: the daemon's OWN utime+stime from /proc, before and
# after N requests, divided by N.
#
# Read-only. Uses the installed `aira` binary, never a worktree build, and
# never restarts or stops anything.
#
#   ./docs/dev/aira24-probe-cost.sh [requests]        # default 200
#
# Recorded result, 2026-09-05, 3 live scopes on the shared box:
#   daemon CPU per confine-list: 1.550 ms
#   => worst case at the queue's own admitMaxWaiters=256 cap:
#      256 waiters / 15s * 1.55ms = 26ms/s = 2.6% of one core.
#   => realistic contended case (a handful of waiters): under 0.1%.
set -euo pipefail
requests="${1:-200}"
# `|| true`: under `set -euo pipefail` a pgrep that matches nothing fails the
# whole pipeline and would abort the script BEFORE the message below, which is
# the one case this guard exists for.
pid=$(pgrep -u "$(id -u)" -f 'aira-daemon|aira daemon' | head -1 || true)
if [ -z "${pid}" ]; then
	echo "no aira daemon running for this user; nothing to measure" >&2
	exit 1
fi
cpu_ticks() { awk '{print $14+$15}' "/proc/${pid}/stat"; }
hz=$(getconf CLK_TCK)
before=$(cpu_ticks)
for _ in $(seq "${requests}"); do
	aira confine --list >/dev/null 2>&1
done
after=$(cpu_ticks)
ticks=$((after - before))
echo "daemon pid=${pid} requests=${requests} ticks=${ticks} CLK_TCK=${hz}"
awk -v t="${ticks}" -v n="${requests}" -v hz="${hz}" \
	'BEGIN { printf "daemon CPU per confine-list: %.3f ms\n", (t / hz) * 1000 / n }'
