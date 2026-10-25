#!/usr/bin/env bash
# AIRA-314 fix 2: reproduce the machine-wide p90 prior read cost before and after
# replacing the confine_peak_history indexes with one covering
# (kind, signature, peak_rss) index.
#
# It works on a COPY: the source database is opened read-only and copied with
# sqlite3's online .backup, then every change is made to the copy. Run it confined:
#
#   aira confine --memory-reserve 384M -- \
#     docs/dev/aira-314-p90-index-measurement-repro.sh ~/.local/state/aira/state.db ~/tmp/aira-314-measure
#
# Published result (2026-10-25, two runs on fresh copies of the live state.db,
# sqlite3 CLI, WSL2):
#   rows 185317-185335, signatures ~124k, single-row subjects ~102.65k, p90-qualifying ~8.63k
#   BEFORE  plan: SEARCH USING INDEX confine_peak_history_kind_signature (kind=?)
#           p90 read 0.208 / 0.212 / 0.315 s; 0.246 / 0.230 / 0.165 s
#   index build 3.859 s; 0.604 s (one time, on the first open after the change)
#   AFTER   plan: SEARCH USING COVERING INDEX confine_peak_history_kind_signature_peak (kind=?)
#           p90 read 0.042 / 0.071 / 0.053 s; 0.046 / 0.048 / 0.032 s
#   per-signature history read ~0.001 s both before and after
set -euo pipefail
src="${1:?usage: $0 <source state.db> <scratch dir under ~/tmp>}"
scratch="${2:?usage: $0 <source state.db> <scratch dir under ~/tmp>}"
mkdir -p "$scratch"
db="$scratch/aira-314-copy.db"
rm -f "$db"
sqlite3 -readonly "$src" ".backup $db"

p90="SELECT MAX(peak_rss) AS peak_max FROM confine_peak_history WHERE kind='confine' AND peak_rss>0 GROUP BY signature HAVING COUNT(peak_rss)>=3 ORDER BY peak_max ASC;"
hist="SELECT COUNT(*),SUM(CASE WHEN peak_rss>0 THEN 1 ELSE 0 END),MAX(peak_rss) FROM confine_peak_history WHERE kind='confine' AND signature=(SELECT signature FROM confine_peak_history ORDER BY rowid DESC LIMIT 1);"

timed() { printf '.timer on\n%s\n' "$1" | sqlite3 "$db" | grep '^Run Time'; }

echo "== shape"
sqlite3 "$db" "SELECT 'rows', COUNT(*), 'signatures', COUNT(DISTINCT signature) FROM confine_peak_history;"
sqlite3 "$db" "SELECT 'single-row subjects', COUNT(*) FROM (SELECT 1 FROM confine_peak_history GROUP BY kind,signature HAVING COUNT(*)=1);"
sqlite3 "$db" "SELECT 'p90-qualifying', COUNT(*) FROM (SELECT 1 FROM confine_peak_history WHERE kind='confine' AND peak_rss>0 GROUP BY signature HAVING COUNT(peak_rss)>=3);"
sqlite3 "$db" "SELECT name FROM sqlite_master WHERE tbl_name='confine_peak_history' AND type='index';"

run() {
	echo "== $1"
	sqlite3 "$db" "EXPLAIN QUERY PLAN $p90"
	for _ in 1 2 3; do timed "$p90"; done
	echo "-- per-signature history read"
	for _ in 1 2; do timed "$hist"; done
}

run "BEFORE"
echo "== build covering index"
timed "CREATE INDEX IF NOT EXISTS confine_peak_history_kind_signature_peak ON confine_peak_history(kind, signature, peak_rss);"
sqlite3 "$db" "DROP INDEX IF EXISTS confine_peak_history_kind_signature; DROP INDEX IF EXISTS confine_peak_history_signature;"
run "AFTER"
rm -f "$db"
