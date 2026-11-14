# AIRA-329: "reserve too small" rants are cold-start kills, not a sizing bug

Date: 2026-11-14. Evidence: `docs/dev/aira-329-cold-start-measure.py` (reproduces every
number here; read-only on `~/.local/state/aira/state.db`) and the per-signature rows of
`confine_peak_history` for RANT-69, 78 and 81.

## Question

Eleven rants (RANT-64, 67, 69, 70, 78, 81, 109, 116, 124, 128, 129), all filed after the v0.40
own-peak floor (AIRA-314), say a repeat command was sized `fallback:no-history` 1 GiB and
OOM-killed although the same command had just run. Is the floor not live, or is something
else going on?

## Findings

1. **The floor is not the cause.** `reserve-basis=fallback:no-history` means the daemon found
   zero rows for that exact argv (`EstimateMemoryReserve`: `TotalCount == 0`). AIRA-314's floor
   applies when rows exist; here none did.
2. **The "same command" was a different argv.** Rows for RANT-69's argv
   (`cargo test -p fs-solve --lib execution::preparation`): the FIRST row is the auto-sized
   1 GiB run that was OOM-killed (2026-10-29 00:24); the pinned 9 GiB runs the rant remembers
   come AFTER it (00:30, 00:35). For RANT-78/81 the killed run's argv carries flags the earlier
   pinned runs did not (`--offline --locked --target-dir ...`, `env TMPDIR=...`), so it is a new
   signature. The signature is the exact argv; any change in flags, paths or an `env` prefix is
   a new command with no history.
3. **Size of the effect.** Since 2026-10-20, 19,899 auto-sized runs were the first ever run of
   their exact argv; 16,299 of those were sized at the 1 GiB cold default; 931 (4.7%) were
   OOM-killed.
4. **The 1 GiB default is an owner decision** (AIRA-304, v0.35, 4 GiB -> 1 GiB, to stop
   over-reserving). This 4.7% is its measured price, not a regression.
5. **A coarser key would have helped about a third of them.** For 394 of the 931 cold OOMs a
   coarser key (masked argv, first 4 tokens, or command + subcommand, the AIRA-323 key chain)
   had history; for 312 (34% of all cold OOMs) its prediction (max x 1.25) exceeded the 1 GiB
   cap, so the kill was plausibly avoidable (the OOM row's peak is censored at its cap, so
   this is a plausibility test). Cost: of 16,003 cold runs that did succeed and had a coarse
   prediction, 4,804 (30%) would have been given a larger cap than the 1 GiB default, at a
   median 2.9 x their real peak: more reserved capacity for those jobs.
6. **RANT-129 is a separate cause**: `fallback:history-unavailable` means the per-signature
   history read returned an error (timeout at the 250 ms `admitHistoryTimeout`, or a DB error),
   not an absent history (`admit.go:1469-1475` sets `historyUnavailable` and discards the error
   without logging it). So after a WSL reboot the read failed once; the cause (cold page cache,
   lock, slow disk) is not established because nothing records the error or the elapsed time.

## Decision for the owner

Trade 1 GiB-default capacity savings against 4.7% cold kills. Not changed here (it re-opens
AIRA-304). The measured option, filed held as AIRA-331: when the exact argv has no history, size
the cap as `max(1 GiB, coarse-key prediction)`, using the AIRA-323 key column. Expected: about a
third of cold OOMs avoided, about 30% of cold runs reserve more.

## Actions

- AIRA-329: closed with this diagnosis.
- AIRA-331 (held, owner decision): coarse-key cold-start cap.
- AIRA-332: log the elapsed time and error when a history read hits its deadline (RANT-129).
