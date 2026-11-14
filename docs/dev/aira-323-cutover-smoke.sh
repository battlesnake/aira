#!/usr/bin/env bash
# AIRA-323 cutover smoke test. Run it after BOTH the client install and the
# daemon restart (`systemctl --user restart aira-daemon.service`).
#
# It runs two tiny probes, each with a fresh nonce in its argv so its signature
# is its own, and reads back only the row that probe wrote (exact signature and
# rowid above the pre-probe maximum), so another session's report can neither
# fake a pass nor cause a failure:
#
#   1. from a git checkout: repo must be 16 lowercase hex digits;
#   2. from /: repo must be the literal `none`.
#
# Each row must have 1000 <= wall_ms <= 10000 (the probe sleeps 1 s), wait_ms >=
# 0, and started_at_ms inside [launch, return] of the probe on this machine's
# wall clock. Exit 0 = pass, 1 = fail. The row is printed before it is judged,
# so a run against a daemon that predates AIRA-323 shows the measured skew
# behaviour (the row is kept, the four facts are NULL).
#
# Usage: docs/dev/aira-323-cutover-smoke.sh [git-checkout-dir]
#   AIRA=<path to aira binary> overrides the client (default: aira on PATH).
#   AIRA_STATE_DB=<path> overrides the store (default: the XDG state dir).
set -euo pipefail

aira_bin=${AIRA:-aira}
db=${AIRA_STATE_DB:-"${XDG_STATE_HOME:-$HOME/.local/state}/aira/state.db"}
checkout=${1:-$(git rev-parse --show-toplevel)}
log_dir=${HOME}/tmp
mkdir -p "$log_dir"

fail=0
columns=$(sqlite3 -readonly "$db" "select count(*) from pragma_table_info('confine_peak_history')
	where name in ('wall_ms','wait_ms','started_at_ms','repo')")

probe() {
	local dir=$1 want_repo=$2
	local nonce before start_ms end_ms sig_sql row
	nonce=aira-323-smoke-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
	before=$(sqlite3 -readonly "$db" "select coalesce(max(rowid),0) from confine_peak_history")
	start_ms=$(date +%s%3N)
	# A pinned 32M reserve: sleep needs a few MB, and an unpinned first run of a
	# never-seen argv would book the 1G fallback and may queue.
	if ! (cd "$dir" && "$aira_bin" confine --memory-reserve 32M -- sh -c 'sleep 1' "$nonce") >"$log_dir/$nonce.log" 2>&1; then
		echo "FAIL ($dir): aira confine failed; see $log_dir/$nonce.log"
		fail=1
		return
	fi
	end_ms=$(date +%s%3N)
	sig_sql="'sh'||char(0)||'-c'||char(0)||'sleep 1'||char(0)||'$nonce'"
	if [ "$columns" != 4 ]; then
		# The store has not been opened by a 323 daemon yet: show the row the
		# probe wrote (the pre-323 shape) and fail.
		row=$(sqlite3 -readonly -separator '|' "$db" \
			"select count(*), coalesce(min(peak_rss),'NULL') from confine_peak_history
			 where rowid > $before and kind='confine' and signature = $sig_sql")
		echo "probe from $dir: count|peak_rss = $row; the store has no AIRA-323 columns (daemon not restarted)"
		echo "FAIL ($dir): the daemon predates AIRA-323"
		fail=1
		return
	fi
	row=$(sqlite3 -readonly -separator '|' "$db" \
		"select count(*), coalesce(min(wall_ms),'NULL'), coalesce(min(wait_ms),'NULL'),
		        coalesce(min(started_at_ms),'NULL'), coalesce(min(repo),'NULL'), coalesce(min(peak_rss),'NULL')
		 from confine_peak_history where rowid > $before and kind='confine' and signature = $sig_sql")
	echo "probe from $dir: count|wall_ms|wait_ms|started_at_ms|repo|peak_rss = $row (launch $start_ms, return $end_ms)"
	local count wall wait started repo
	IFS='|' read -r count wall wait started repo _ <<<"$row"
	local bad=()
	[ "$count" = 1 ] || bad+=("expected exactly 1 new row, got $count")
	if [ "$count" = 1 ]; then
		[[ $wall =~ ^[0-9]+$ ]] && [ "$wall" -ge 1000 ] && [ "$wall" -le 10000 ] || bad+=("wall_ms=$wall not in 1000..10000")
		[[ $wait =~ ^[0-9]+$ ]] || bad+=("wait_ms=$wait not >= 0")
		[[ $started =~ ^[0-9]+$ ]] && [ "$started" -ge "$start_ms" ] && [ "$started" -le "$end_ms" ] ||
			bad+=("started_at_ms=$started not in [$start_ms, $end_ms]")
		if [ "$want_repo" = hex ]; then
			[[ $repo =~ ^[0-9a-f]{16}$ ]] || bad+=("repo=$repo is not 16 lowercase hex digits")
		else
			[ "$repo" = none ] || bad+=("repo=$repo, want none")
		fi
	fi
	if [ "${#bad[@]}" -eq 0 ]; then
		echo "PASS ($dir)"
	else
		echo "FAIL ($dir): ${bad[*]}"
		fail=1
	fi
}

probe "$checkout" hex
probe / none
exit "$fail"
