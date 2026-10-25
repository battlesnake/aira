#!/usr/bin/env bash
# AIRA-314: reproduce every figure the spec publishes
# (docs/superpowers/specs/2026-10-25-own-peak-floor-design.md §1 and §6.2):
#   1. the yield counts for fix 1 (own clean peak floor) and the population fix 2
#      (the p90 prior read stops timing out) moves;
#   2. the machine-wide p90 prior read cost before and after replacing the
#      confine_peak_history indexes with one covering (kind, signature, peak_rss)
#      index.
#
# It works on a COPY: the source database is opened read-only and copied with
# sqlite3's online .backup, then every change is made to the copy. Run it confined:
#
#   aira confine --memory-reserve 512M -- \
#     docs/dev/aira-314-p90-index-measurement-repro.sh ~/.local/state/aira/state.db ~/tmp/aira-314-measure
#
# Published result, counts (2026-10-25 revision run, fresh copy of the live state.db):
#   p90 peak 1598423040, prior p = p90 x 1.15 = 1838186496
#   signatures with 1-2 usable peaks                                103890
#   ... with a clean peak whose x1.15 exceeds 1 GiB                 12272
#   ... with a clean peak whose x1.15 exceeds p                      6929
#   any signature whose clean peak x1.15 exceeds 60 GiB                 0
#   clean peak <= OOM peak (a PeakMax > MaxOOMPeak proxy misses it)   161
#   fix 2 population: 1-2 peaks, no OOM row, F < p                  96292
#   ... of which F < 1 GiB (reserve 1 GiB -> p when the read failed) 91026
#   unpinned OOM below 1.15x an earlier clean peak, 1-2 peaks before   40  (upper bound, see spec §1)
#   ... same, 3+ peaks before                                         210  (confound gauge, see spec §1)
# Published result, timings (2026-10-25, two runs on fresh copies, sqlite3 CLI, WSL2):
#   rows 185317-185335, signatures ~124k, single-row subjects ~102.65k, p90-qualifying ~8.63k
#   BEFORE  plan: SEARCH USING INDEX confine_peak_history_kind_signature (kind=?)
#           p90 read 0.208 / 0.212 / 0.315 s; 0.246 / 0.230 / 0.165 s
#   index build 3.859 s; 0.604 s (one time, on the first open after the change)
#   AFTER   plan: SEARCH USING COVERING INDEX confine_peak_history_kind_signature_peak (kind=?)
#           p90 read 0.042 / 0.071 / 0.053 s; 0.046 / 0.048 / 0.032 s
#   per-signature history read ~0.001 s both before and after
# Third run (revision, rows 185490, signatures 124134, p90-qualifying 8647):
#   BEFORE 0.152 / 0.207 / 0.204 s; build 0.599 s; AFTER 0.064 / 0.057 / 0.064 s
set -euo pipefail
src="${1:?usage: $0 <source state.db> <scratch dir under ~/tmp>}"
scratch="${2:?usage: $0 <source state.db> <scratch dir under ~/tmp>}"
mkdir -p "$scratch"
db="$scratch/aira-314-copy.db"
rm -f "$db"
sqlite3 -readonly "$src" ".backup $db"

echo "== counts"
# F = a signature's largest CLEAN peak x 1.15 (integer, as runner.GrowByEstimatorMargin);
# p = the p90 prior x 1.15 (nearest rank over signatures with >= 3 usable peaks,
# as store.ConfinePeakP90). Unpinned rows are budget_basis 'cap:auto:daemon-reserve'.
sqlite3 "$db" <<'SQL'
.headers off
.mode list
CREATE TEMP TABLE s AS
SELECT signature,
       SUM(CASE WHEN peak_rss>0 THEN 1 ELSE 0 END) AS n,
       SUM(CASE WHEN oom=1 THEN 1 ELSE 0 END) AS oomc,
       COALESCE(MAX(CASE WHEN oom=1 AND peak_rss>0 THEN peak_rss END),0) AS moom,
       COALESCE(MAX(CASE WHEN oom=0 AND peak_rss>0 THEN peak_rss END),0) AS mclean
FROM confine_peak_history WHERE kind='confine' GROUP BY signature;
CREATE TEMP TABLE prior AS
WITH q AS (SELECT MAX(peak_rss) AS pm FROM confine_peak_history
           WHERE kind='confine' AND peak_rss>0 GROUP BY signature HAVING COUNT(peak_rss)>=3),
     r AS (SELECT pm, ROW_NUMBER() OVER (ORDER BY pm) AS rn, COUNT(*) OVER () AS c FROM q)
SELECT pm AS p90, pm + pm*15/100 AS p FROM r WHERE rn = (9*c+9)/10;
SELECT 'p90 peak / prior p', p90, p FROM prior;
SELECT 'signatures with 1-2 usable peaks', COUNT(*) FROM s WHERE n BETWEEN 1 AND 2;
SELECT '... with a clean peak, F > 1 GiB', COUNT(*) FROM s WHERE n BETWEEN 1 AND 2 AND mclean>0 AND mclean + mclean*15/100 > 1073741824;
SELECT '... with a clean peak, F > p', COUNT(*) FROM s, prior WHERE n BETWEEN 1 AND 2 AND mclean>0 AND mclean + mclean*15/100 > p;
SELECT 'any n, F > 60 GiB', COUNT(*) FROM s WHERE mclean>0 AND mclean + mclean*15/100 > 64424509440;
SELECT 'clean peak <= OOM peak', COUNT(*) FROM s WHERE mclean>0 AND moom>0 AND mclean<=moom;
SELECT 'fix 2 population: 1-2 peaks, no OOM row, F < p', COUNT(*) FROM s, prior WHERE n BETWEEN 1 AND 2 AND oomc=0 AND mclean + mclean*15/100 < p;
SELECT '... of which F < 1 GiB', COUNT(*) FROM s WHERE n BETWEEN 1 AND 2 AND oomc=0 AND mclean + mclean*15/100 < 1073741824;
CREATE TEMP TABLE kills AS
SELECT o.signature AS signature,
       (SELECT COUNT(*) FROM confine_peak_history b WHERE b.kind='confine' AND b.signature=o.signature
          AND b.peak_rss>0 AND (b.at<o.at OR (b.at=o.at AND b.rowid<o.rowid))) AS before_n,
       (SELECT MAX(b.peak_rss) FROM confine_peak_history b WHERE b.kind='confine' AND b.signature=o.signature
          AND b.oom=0 AND b.peak_rss>0 AND (b.at<o.at OR (b.at=o.at AND b.rowid<o.rowid))) AS before_clean,
       o.budget AS budget
FROM confine_peak_history o
WHERE o.kind='confine' AND o.oom=1 AND o.budget_basis='cap:auto:daemon-reserve' AND o.budget IS NOT NULL;
SELECT 'unpinned OOM below 1.15x an earlier clean peak, 1-2 peaks before', COUNT(DISTINCT signature) FROM kills WHERE before_n BETWEEN 1 AND 2 AND before_clean>0 AND budget < before_clean + before_clean*15/100;
SELECT '... same, 3+ peaks before', COUNT(DISTINCT signature) FROM kills WHERE before_n >= 3 AND before_clean>0 AND budget < before_clean + before_clean*15/100;
SQL

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
