# AIRA-351 step 0: pre-registered decision rule

Written 2026-11-18 BEFORE running measure.py. Seen so far: only table-level counts (rows per kind,
budget_basis families, worker budget distribution), no answer to the question.

## What the code says today (the baseline being compared against)

- Confine signature = exact effective argv joined by NUL, no cwd/worktree term
  (internal/runner/resource_estimate.go:82-92). So `make test` in a fresh worktree is NOT cold:
  it shares the signature of every other `make test` on the box (any repo). A confine job is
  cold across worktrees only when its argv carries a path (e.g. `python3 /home/.../wt-x/x.py`,
  `--target-dir ...`, `-C <dir>`).
- Unpinned sizing: <3 usable own samples -> the client hint (1 GiB default), basis
  `fallback:no-history` / `fallback:insufficient-samples` (internal/daemon/admit.go:1717-1728,
  internal/runner/resource_estimate.go:126-136); >=3 -> 1.15 x own max (resource_estimate.go:137-158);
  own max clean peak x 1.15 floors every unpinned reserve (admit.go:1730-1737).
  Pinned (`--memory-reserve`) is never touched (admit.go:1495-1497).
- aitest WORKERS are not sized from history at all: reserve = AIRA_AITEST_WORKER_OVERHEAD_BYTES
  (default 512 MiB) + @aira_mem, granted verbatim (internal/pylib/aitest/supervisor.py:1419-1452,
  internal/daemon/worker_admit.go:79-81,455,525). So option A cannot change a worker booking
  unless history-driven worker sizing is built first. Workers are reported separately, as context.

## Definitions

- Cold booking: a `kind='confine'` row with budget_basis `cap:auto:daemon-reserve` (unpinned,
  scope memory.max = the daemon's reserve) whose own signature had < 3 usable (peak>0) rows that
  FINISHED before this job STARTED. Rows of a signature at the 20-row retention cap whose prior
  count cannot be established are excluded and counted as `ambiguous`.
- Command shape (the option-A key), pre-registered:
  - K1 (primary, what a build could defend): argv with leading `env`/VAR=val unwrapped and every
    worktree-root path prefix (`/home/<u>/tmp/<dir>`, `/home/<u>/claude/<dir>`, `/tmp/<dir>`)
    replaced by a placeholder. Siblings = rows with the SAME repo (git common dir) and same K1,
    different exact signature, finished before this job started.
  - K2 (sensitivity, loose): AIRA-329's mask (every path token and long number masked).
- Warm estimate (option A) = 1.15 x max peak over (siblings + own prior rows), needs >= 3 usable
  samples; then today's own-max floor. Reported also with p90 instead of max, as sensitivity.
  The decision uses the max variant (it is what the live estimator does).
- Booked GiB-hours = budget x wall_ms. Only rows with started_at_ms and wall_ms (AIRA-323, since
  v0.43) have a duration; the whole-history window gets counts only (GiB-hours unevaluated).
- Under-reservation (false-low): the job's recorded peak > warm estimate. Cap = reserve for an
  unpinned job, so this is a would-be OOM kill (conservative: memory.peak includes reclaimable cache).
- Two variants: A-both (warm estimate replaces the cold booking up or down) and A-lower
  (min(cold booking, warm estimate): only lowers).

## GO requires ALL of (3.5-day repo window, K1, max variant)

- G1 coverage: >= 30% of cold confine bookings have a same-repo K1 sibling set with >= 3 samples.
- G2 benefit: A-lower net booked GiB-hours saved >= 24 GiB-h per day (1 GiB of slice freed on
  average, ~2% of the ~46 GiB ceiling), AND >= 10% of all cold-booking GiB-hours.
- G3 false-low (hard limit): among jobs A-lower would lower, would-be kills (peak > estimate)
  <= 1% of them AND <= 2 per day.
- G4 false-high (A-both only, reported): extra GiB-h from raises are reported; A-both is
  preferred over A-lower only if the raises it adds are each matched by an OOM at 1 GiB
  (otherwise raising is AIRA-331's owner decision, not this ticket's).

## NO-GO if G1 or G2 fails by more than 30% of its threshold, or G3 fails.

## INCONCLUSIVE if: fewer than 200 cold confine bookings carry a repo in the window; or G1/G2 are
within 30% of their thresholds; or > 20% of window rows lack wall_ms/started_at_ms.
