#!/usr/bin/env bash
# AIRA-68 reproduction: is the admission reserve ledger leaking?
#
# Samples the daemon's own ledger summary against an independent, process-level
# count of LIVE admission clients. Every granted ledger entry is held open by
# exactly one client connection, so:
#
#   jobs  >  live clients          => ghost entries: a real reserve leak
#   jobs  <= live clients, and
#   jobs/granted fall as well as rise => the ledger is releasing correctly
#
# The two populations `aira confine --list` counts as "admitted jobs" are
# deliberately counted separately here, because they are NOT comparable to the
# scope table the same command prints above the summary: an `aira confine-reserve`
# reservation (AIRA-69's per-test pytest lease) holds ledger bytes but creates NO
# cgroup scope, so it never appears as a table row. Reading the job count against
# the table's row count is what produced AIRA-68's P0 misdiagnosis.
#
# Usage: docs/dev/aira68-ledger-sample.sh [samples] [interval-seconds]
# Needs jq. GRANTED and CEILING are bytes (read from the JSON slice_reserve).
# Read-only: it starts nothing, kills nothing, and never restarts the daemon.

set -uo pipefail

samples=${1:-10}
interval=${2:-20}

printf '%-12s %-16s %-10s %-6s %-8s %-8s %-8s\n' \
  EPOCH GRANTED CEILING JOBS RESERVE CONFINE CLIENTS

for ((i = 0; i < samples; i++)); do
  # AIRA-214: piped, `aira confine --list` is the JSON envelope (the table is for
  # a terminal), so read the ledger summary from its slice_reserve object. Bytes.
  listing=$(aira confine --list --json 2>/dev/null)
  read -r granted ceiling jobs < <(printf '%s\n' "$listing" |
    jq -r '.data.slice_reserve // empty | "\(.granted_bytes) \(.ceiling_bytes) \(.jobs)"')

  # Live admission clients, counted from the process table, independently of
  # anything the daemon reports.
  reserve_clients=$(pgrep -cf 'aira confine-reserve')
  confine_clients=$(pgrep -af 'aira confine ' | grep -v 'confine-reserve' | grep -cv '\-\-list')

  printf '%-12s %-16s %-10s %-6s %-8s %-8s %-8s\n' \
    "$(date +%s)" "${granted:-?}" "${ceiling:-?}" "${jobs:-?}" \
    "$reserve_clients" "$confine_clients" "$((reserve_clients + confine_clients))"

  if ((i + 1 < samples)); then sleep "$interval"; fi
done
