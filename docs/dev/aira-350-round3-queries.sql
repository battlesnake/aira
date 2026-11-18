-- AIRA-350 round 3: the queries behind the spec's section 1.2 timeline and section 7 figures.
-- Read-only, against a COPY of the store (never the live file):
--   sqlite3 -readonly ~/.local/state/aira/state.db ".backup $HOME/tmp/aira-350/state-copy.db"
--   aira confine --name <unique> -- sqlite3 -readonly ~/tmp/aira-350/state-copy.db \
--       < docs/dev/aira-350-round3-queries.sql
-- Rows are written when a job ends and the store keeps 20 rows per exact signature, so every
-- figure is of the retained sample. Signatures are left out (they name other sessions' jobs).
.mode tabs
.headers on

-- 1. Incident window 2026-11-18 13:10-13:32 EET (11:10-11:32 UTC): confine grants with their
--    enqueue time (grant - wait), in EET.
SELECT time(started_at_ms/1000, 'unixepoch', '+2 hours') AS granted_eet,
       time((started_at_ms - wait_ms)/1000, 'unixepoch', '+2 hours') AS enqueued_eet,
       wait_ms/1000 AS wait_s, budget/1048576 AS budget_mib, budget_basis
FROM confine_peak_history
WHERE kind = 'confine' AND wait_ms IS NOT NULL
  AND started_at_ms >= strftime('%s', '2026-11-18 11:10:00')*1000
  AND started_at_ms <  strftime('%s', '2026-11-18 11:32:00')*1000
ORDER BY started_at_ms;

-- 2. Small confine rows (budget <= 1 GiB) started in the yield window 2026-11-15 00:00 to
--    2026-11-18 12:00 UTC: run time, GiB-hours, and the aitest-parent share (a lower bound on
--    suite parents: a `make test-engine` parent does not carry that signature).
SELECT count(*) AS n,
       sum(wall_ms >= 3600000) AS over_1h,
       round(sum(budget/1073741824.0 * wall_ms/3600000.0), 1) AS gib_hours_all,
       round(sum(CASE WHEN wall_ms >= 3600000 THEN budget/1073741824.0 * wall_ms/3600000.0 ELSE 0 END), 1) AS gib_hours_over_1h,
       round(sum(CASE WHEN wall_ms >= 600000 THEN budget/1073741824.0 * wall_ms/3600000.0 ELSE 0 END), 1) AS gib_hours_over_10min,
       sum(signature LIKE 'aitest-parent%') AS aitest_parent_rows
FROM confine_peak_history
WHERE kind = 'confine' AND budget IS NOT NULL AND budget <= 1073741824
  AND started_at_ms >= strftime('%s', '2026-11-15 00:00:00')*1000
  AND started_at_ms <  strftime('%s', '2026-11-18 12:00:00')*1000;

-- 3. Their run-time median and 90th percentile, in seconds.
WITH small AS (
  SELECT wall_ms FROM confine_peak_history
  WHERE kind = 'confine' AND budget IS NOT NULL AND budget <= 1073741824
    AND started_at_ms >= strftime('%s', '2026-11-15 00:00:00')*1000
    AND started_at_ms <  strftime('%s', '2026-11-18 12:00:00')*1000)
SELECT (SELECT wall_ms/1000 FROM small ORDER BY wall_ms LIMIT 1 OFFSET (SELECT count(*)/2 FROM small)) AS wall_p50_s,
       (SELECT wall_ms/1000 FROM small ORDER BY wall_ms LIMIT 1 OFFSET (SELECT count(*)*9/10 FROM small)) AS wall_p90_s;

-- 4. Average release rate over the window: bookings of every confine row started in it, per
--    minute (a lower bound on the true rate, since rows are lost).
SELECT count(*) AS n,
       round(sum(budget)/1073741824.0, 1) AS gib_booked_total,
       round(sum(budget)/1e9 / ((strftime('%s', '2026-11-18 12:00:00') - strftime('%s', '2026-11-15 00:00:00'))/60.0), 2) AS gb_per_min_avg
FROM confine_peak_history
WHERE kind = 'confine' AND budget IS NOT NULL
  AND started_at_ms >= strftime('%s', '2026-11-15 00:00:00')*1000
  AND started_at_ms <  strftime('%s', '2026-11-18 12:00:00')*1000;
