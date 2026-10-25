# AIRA-314 — a command's own recorded peak floors its unpinned reserve; the p90 prior read stops timing out

Ticket: AIRA-314 (P2 bug, milestone v0.40). Source: the 2026-10-25 rant triage
(RANT-52, RANT-55; RANT-54 is AIRA-273). Investigator report:
`~/tmp/aira-rant-verify/RANT-52_RANT-54_RANT-55.md` (re-checked against the code
below; every file:line in this spec was opened on `ff03d8aa`).

Process class: **correctness-critical admission core** (it changes what
`resolveAdmitReserve` returns). Full two-loop: Opus builds TDD, Fable reviews in a
throwaway detached worktree. Daemon-side change: the shared-daemon restart care
applies at cutover.

## 1. The problem, in plain terms

An unpinned `aira confine -- <cmd>` asks the daemon to size its memory reserve. The
daemon looks up this exact command line's recorded peaks:

- with **three or more** recorded peaks it reserves `max peak x 1.15`;
- with **one or two** recorded peaks it throws them away and uses a machine-wide
  guess (the p90 of other commands' peaks, about 1.71 GiB on this box today) or, if
  that guess cannot be read, the 1 GiB client default.

So a command that has just run once at 1.07 GiB (RANT-55, pinned at 3G) is re-run
unpinned, gets 1 GiB, and is OOM-killed. A command that ran once at 2.75 GiB
(RANT-52) gets the 1.71 GiB guess, or a 1.5x OOM escalation that is still below
its own clean peak, and is OOM-killed. The evidence that would have prevented the
kill was in the database and was ignored.

Second, the machine-wide guess itself is unreliable. Its query is a full-table
`GROUP BY` over 185k rows / 124k signatures, takes 0.17-0.32 s (sqlite3 CLI;
0.24-0.30 s warm in Go, reported by the investigator), and runs under the 250 ms
admit deadline. A failed read is not cached, so admission silently drops to the
1 GiB default, which is what turned RANT-55 into a kill.

Measured yield on a copy of the live `state.db` (2026-10-25,
`docs/dev/aira-314-p90-index-measurement-repro.sh` for the index figures; the
counts are one-off queries on the same copy):

| Fact | Value |
|---|---|
| signatures with 1-2 recorded peaks | 103,806 |
| ... whose largest peak is clean and `x1.15` exceeds 1 GiB (affected when the p90 is unreadable) | 12,236 |
| ... whose largest peak is clean and `x1.15` exceeds today's p90 `x1.15` (affected even when the p90 works) | 6,906 |
| ... whose `x1.15` exceeds 60 GiB (would newly be refused on `aira.slice`) | 0 |
| signatures in retained history where an UNPINNED run was OOM-killed directly after one of its first two clean runs, below 1.15x that clean peak (each a kill fix 1 prevents) | 37 |
| p90 read, old index (two runs of the script) | 0.208 / 0.212 / 0.315 s; 0.246 / 0.230 / 0.165 s |
| p90 read, covering index | 0.042 / 0.071 / 0.053 s; 0.046 / 0.048 / 0.032 s |
| one-time covering-index build on the first open | 3.86 s; 0.60 s |

## 2. Greenfield minimum

If we solved only this problem from scratch:

1. Whenever a command has any recorded peak, its unpinned reserve is never below
   `own max peak x 1.15`. One `max()` at the end of the reserve resolution.
2. The machine-wide prior is a cheap, cached number that does not vanish on a slow
   read: one index that lets the p90 query read only the index, and a cache that
   keeps its last good value.

Nothing else. No new wire field, no new table, no schema migration, no new error
code, no change to recording.

### The gap between that minimum and this plan, each item justified

| Item beyond the minimum | Why it is kept |
|---|---|
| The floor applies only while `0 < SampleCount < 3` | At `n >= 3` the ordinary estimate is `GrowByEstimatorMargin(PeakMax)` byte for byte (`resource_estimate.go:137-149` vs `:110-122`), so a floor there is a no-op except inside AIRA-151/153's OOM clamp, which is a separate decision (deferral D3). Scoping it to `n < 3` keeps every `n >= 3` row of the existing test suite byte-identical. |
| The floor applies only when the largest peak is a CLEAN one (`PeakMax > MaxOOMPeak`) | When the largest peak is an OOM peak, the OOM escalation (`1.5 x MaxOOMPeak`) already exceeds `1.15 x` that peak in every outcome except the AIRA-153 clamp to `fit`. There, an OOM peak is censored evidence (the job was killed AT its cap), and applying `1.15 x` it would undo the clamp rung AIRA-153 built (`TestTheAcceptedR3BandReturnsMoreThanMasterAndIsRefused` rows would change basis). A clean peak is an uncensored measurement, so it is the evidence that is being ignored today. Losing nothing: see §5.1, case C. |
| A basis token naming the floor (`estimate:own-max=<peak>,n=<n>,f=115`, plus `,oom-on-record` when the signature has an OOM record) | AIRA-149's rule: `reserve-basis` names the term that set the number. Without it the trailer would print `estimate:p90-prior` or `fallback:insufficient-samples` for a value neither produced. `,oom-on-record` keeps AIRA-128's attribution property (an OOM record was consulted). |
| The covering index REPLACES the two existing indexes instead of being added | The minimum adds an index; this plan removes two and adds one. `(kind, signature, peak_rss)` has `(kind, signature)` as its prefix, so every reader that used the old composite index still has it. `confine_peak_history_signature` (signature alone) has no reader: every query on the table filters `kind=?` first or scans it in full (`confine_peak_history.go:157-160, 180-185, 204-206, 251-252`, and the three stream/subject readers at `:326, :364, :380`). Removing it takes one index write off every `RecordConfinePeak`. |
| Keep-last-good also stamps the refresh time on a failed read when a last good value exists | Without the stamp, every admission during a slow spell re-runs the failing read and pays up to 250 ms each, which is today's behaviour. With it, the failing read is retried at most once per `admitPriorRefresh` (1 minute). One assignment. With NO last good value (cold daemon) nothing is stamped, so a cold start keeps retrying on each admission exactly as today. |
| A committed measurement reproduction script | Repo rule: a published measurement has a committed, executable reproduction. |

**Removed from the brief's options, with the measured reason:** a separate
context budget for the p90 read. The per-signature read that shares the 250 ms
context costs about 1 ms on the live data before and after the index (repro
script, "per-signature history read"), so the shared budget costs the p90 read
about 1 ms. Two extra lines buy nothing measurable and would double the
worst-case admission latency to 500 ms. Not built (deferral D5).

## 3. Scope

In:

- **Fix 1** `internal/daemon/admit.go: resolveAdmitReserve`: the own-peak floor.
- **Fix 2a** `internal/store/store.go`: replace the two `confine_peak_history`
  indexes with `confine_peak_history_kind_signature_peak ON (kind, signature, peak_rss)`.
- **Fix 2b** `internal/daemon/admit.go: cachedAdmitPeakP90`: keep serving the last
  good value when a refresh fails.
- **Skill text** `internal/core/skill.go` (the generated agent guide): describe
  the new basis token and the real thresholds (one or two recorded peaks now floor
  the reserve; from three the ordinary estimate takes over).
- **Ticket notes** on AIRA-246 and AIRA-242 recording the measurement in §1 (the
  figures AIRA-246's retirement said were missing; AIRA-242's title claim
  "admission unaffected" missed the p90 read). Status unchanged on both.
- **Over-reserve hint** `internal/runner/confine.go:1155-1157`: wording KEPT,
  with a test that pins the property making it true (§7, T9). Decision recorded
  in §5.3.

Out: everything in §10.

## 4. Invariants

- **I1 Monotone.** For every input, the new resolution is `>=` today's. The
  floor is a `max()`; nothing lowers a value. (This deliberately breaks AIRA-153's
  I3 "never grants more than master" for the own-floor rows; §6.4.)
- **I2 Pinned untouched.** `request.pinned` returns `pinned:client` at the first
  line, before any floor exists (`admit.go:1399-1401`).
- **I3 No floor without own clean evidence.** The floor is zero unless the
  per-signature read succeeded, `0 < SampleCount < 3`, and `PeakMax > MaxOOMPeak`
  (so `PeakMax > 0`). No-signature, history-unavailable, no-history and
  capture-unavailable (`SampleCount == 0`) paths are byte-identical to today.
- **I4 A floor is a measurement, never fitted.** The floor is not reduced to
  `fit` or the ceiling. Over the ceiling it is returned as is and
  `admitConnection` refuses it `E_ADMIT_TOO_LARGE` (`admit.go:1792-1795`), exactly
  as AIRA-153 treats an over-ceiling ordinary estimate, with the existing
  `estimate:` advice arm ("this command's OWN measured peak history",
  `admit.go:3033-3034`), which is true for this basis.
- **I5 Label names the term that acted.** The basis is the floor's token only
  when the floor STRICTLY raised the value; on an exact tie the existing basis is
  kept (AIRA-149's strict tie-break). When the floor wins, `,ceiling-fitted` and
  `,ceiling-clamped` are dropped (the value is no longer the fitted prior or the
  clamp) and `,oom-on-record` is kept iff `OOMCount > 0`.
- **I6 Recording unchanged.** `shouldRecordConfinePeak` (`confine_linux.go`,
  AIRA-264) is not touched: non-OOM failed runs still leave no row, so they never
  reach the floor. Pinned clean runs and OOM runs are recorded today and feed it.
- **I7 Ledger conservation unchanged.** The floor changes only the requested
  reserve handed to `admitConnection`; the `reserve > ceiling` refusal, the
  enqueue re-check and the grant gate (`Σgranted <= cap - headroom`) are
  downstream and untouched.
- **I8 Restart paths unchanged.** The ARDR re-declare re-declares the GRANTED
  reserve verbatim through `enqueueReDeclare` and never calls
  `resolveAdmitReserve` (`admit.go:2056-2058`, `server.go:1027`); daemon reload
  rebuilds the p90 cache from scratch (it is in-memory).
- **I9 Schema needs no migration.** Opening any existing `state.db` (old indexes
  present) ends with exactly the covering index and neither old index, rows
  intact, through the real `store.OpenDB` / `Open` path.
- **I10 Stale prior is the last SUCCESSFUL read.** A failed refresh never
  invents a value: it serves the last successful `(peak, ok)` pair (which may be
  `(0, false)` if the last success found no qualifying signatures) or, with no
  success yet, `(0, false)` as today.

## 5. Design

### 5.1 Fix 1 — the floor

Inside the successful-read branch (`admit.go:1456-1594`), compute once:

```go
// ownFloor > 0 only for 0 < n < 3 with a clean largest peak (I3).
if stats.SampleCount > 0 && stats.SampleCount < 3 && stats.PeakMax > stats.MaxOOMPeak {
    ownFloor = runner.GrowByEstimatorMargin(stats.PeakMax)
    ownBasis = fmt.Sprintf("estimate:own-max=%d,n=%d,f=115", stats.PeakMax, stats.SampleCount)
    if stats.OOMCount > 0 { ownBasis += ",oom-on-record" }
}
```

(`3` should be an exported runner constant or accessor rather than a second
literal; `runner.memoryEstimateMinSamples` is unexported today. Builder's choice:
export it, or add `runner.EstimateMinSamples()`. Not a new threshold.)

Apply `floor(value, basis)` = `if ownFloor > value { return ownFloor, ownBasis }`
at **every return after the history read** (`:1587`, `:1589`, `:1592`, `:1611`,
`:1613`, `:1619`, `:1622`, `:1625`, `:1627`). `ownFloor` is zero on every path
I3 excludes, so applying it uniformly is a no-op there and cannot miss a site.
Structurally the cleanest form is to move today's body into an inner function and
apply the floor once in `resolveAdmitReserve`; builder's choice, the reviewer
checks that no return escapes it.

Outcome table at `n in {1, 2}` with a clean largest peak `P`, floor `F = 1.15P`:

| Today's path | Today's value | New value | New basis |
|---|---|---|---|
| no OOM, p90 readable | `p` = p90 x 1.15 (or `fit` if `p >= ceiling`) | `max(that, F)` | own-max iff `F >` it |
| no OOM, p90 unreadable | client default (or `fit`) | `max(that, F)` | own-max iff `F >` it |
| OOM on record, escalation `E = 1.5 x MaxOOMPeak` did not raise | client default (or `fit`) | `max(that, F)` | own-max`,oom-on-record` iff `F >` it |
| OOM on record, `E` raised, `E <= ceiling` | `E` | `max(E, F)` | own-max`,oom-on-record` iff `F > E` (RANT-52: `E` = 2.67G, `F` = 3.39G) |
| OOM on record, `E > ceiling`, guard `MaxOOMPeak < fit` holds | `fit` (`,ceiling-clamped`) | `max(fit, F)`; if `F > ceiling` it is refused | own-max`,oom-on-record` iff `F > fit` |
| OOM on record, `E > ceiling`, guard fails | `E` (refused) | `max(E, F)` (refused) | `E`'s basis unless `F > E` |

Why the clamp row is right: AIRA-151's rule is that an OOM record must not make
the same number behave differently. The same clean history with no OOM yields
`max(prior, F)`, refused when `F > ceiling`; the clamp must not quietly grant
`fit < F` just because an OOM is also on record. And the clamp's own guard
(`MaxOOMPeak < fit`) is about the OOM peak, not the clean one.

Case C, what the clean-only condition gives up: an OOM-only signature whose
`MaxOOMPeak` lies in `(fit/1.15, fit)` is clamped to `fit`, which is below
`1.15 x MaxOOMPeak` but above `MaxOOMPeak`. That is AIRA-153's deliberate rung
for censored evidence, unchanged.

### 5.2 Fix 2 — the prior

**2a, index.** In `ensureConfinePeakHistorySubject` (`store.go:1312-1327`):

```sql
CREATE INDEX IF NOT EXISTS confine_peak_history_kind_signature_peak
    ON confine_peak_history(kind, signature, peak_rss);
DROP INDEX IF EXISTS confine_peak_history_kind_signature;
DROP INDEX IF EXISTS confine_peak_history_signature;
```

Create before drop, so no window exists with no `(kind, signature)` prefix index.
Also delete the DDL-block `CREATE INDEX IF NOT EXISTS confine_peak_history_signature`
(`store.go:881-882`), or every open would recreate it and drop it again. With the
covering index the p90 plan is `SEARCH ... USING COVERING INDEX
confine_peak_history_kind_signature_peak (kind=?)` (measured, §1), and the
per-(kind, signature) readers and the retention `DELETE` keep their prefix lookup.
Hoist the p90 SQL into a package constant so the plan test (T12) checks the
exact text `ConfinePeakP90` runs.

**2b, keep last good.** `cachedAdmitPeakP90` (`admit.go:1630-1654`), on
`err != nil`: under `admitPriorMu`, if `!admitPriorAt.IsZero()` return the cached
`(admitPriorPeak, admitPriorOK)` and set `admitPriorAt = now`; otherwise return
`(0, false)` and stamp nothing. Success path unchanged. The basis stays
`estimate:p90-prior`: a value up to a minute old is already served today, and the
prior is a slow-moving guess about other commands.

### 5.3 The over-reserve hint (`confine.go:1155-1157`)

It fires only for a pinned, clean, exit-0 run with reserve `>= 2 GiB` and peak
`< 60 %` of it. Such a run is recorded (I6). In that domain `P < 0.6 x reserve
<= 0.6 x ceiling`, so `F = 1.15P < 0.69 x ceiling < fit`: the floor is always
admissible and the clamp can never undercut it. So after fix 1 the next unpinned
run of the same argv is sized at no less than `1.15P` at `n = 1` and `n = 2` (the
new floor) and at `n >= 3` (the ordinary estimate). The clause "let aira estimate
it from this command's history" is therefore true as advice: aira's sizing now
honours this command's history at every sample count. One nuance, accepted and
written down: at `n < 3` when the machine-wide prior is larger than `F`, the
basis says `estimate:p90-prior`, because the prior acted. The reserve is still no
lower than the history demands. The hint is advice, not a provenance label. If
the plan gate rules the nuance makes the clause false, the fallback is to delete
the clause (a one-line change), not to add sample-count plumbing to the client.

## 6. False-pass and false-fail analysis

### 6.1 False pass: a reserve that is too SMALL

| Path | Can it still return `< 1.15 x own clean max` at `n in {1,2}`? |
|---|---|
| p90 prior, p90 fitted, insufficient-samples fallback, default fitted | No: the floor is applied at every return. T1, T4, T5. |
| OOM branch, escalation did not raise | No. T2. |
| OOM branch, escalation raised below `F` | No. T2 (RANT-52 replay). |
| OOM clamp to `fit` with `F > fit` | No: `max(fit, F)`; over the ceiling it is refused, not fitted. T3. |
| history read fails | Yes, by design: no stats, so no floor; basis says `fallback:history-unavailable`. Unchanged and honest. |
| largest peak is an OOM peak | The floor is not applied (I3); the escalation or AIRA-153's clamp governs (§5.1 case C). T6. |
| pinned run | Untouched (I2). T7. |
| failed run recorded and later trusted | Cannot happen: AIRA-264's send-side gate is unchanged (`TestShouldRecordConfinePeakTracksTheClassifier`); the floor reads only what is recorded. |
| label lies about which term acted | Strict tie-break, suffixes dropped on a floor win, `,oom-on-record` kept. T1-T5 assert value AND exact basis. |
| stale p90 served after a failure | Serves a real earlier read, never a fabricated one (I10). T10. |

### 6.2 False fail: a reserve that is too LARGE, or a new refusal

- **Zero samples:** unchanged (I3). T1 row `n = 0`.
- **`n >= 3`:** unchanged. T1 rows `n = 3, 20`.
- **New `E_ADMIT_TOO_LARGE`:** a command whose one or two clean peaks `x1.15`
  exceed the ceiling, which today got a 1 GiB or ~1.7 GiB guess and would almost
  certainly have been OOM-killed. Now refused immediately, naming both numbers.
  This is AIRA-153's rule for ordinary estimates, applied one or two samples
  earlier. On the live data the count above 60 GiB is 0 (§1); it bites only on
  small slices (ci-shim budgets, fixtures). T3, T8.
- **Bimodal command:** one or two big pinned runs make the next cheap unpinned run
  reserve the big figure. Over-reservation is the safe direction, and is exactly
  what `estimate:max` does from three samples. Accepted.
- **Fix 2 makes the p90 prior apply more often.** Today a failed p90 read falls
  to the 1 GiB default; after fix 2 nearly every cold start gets the prior
  (1598423040 x 1.15 = 1838186496, ~1.71 GiB, on today's copy). That raises the
  paper reservation of cold-start jobs and runs against the owner's AIRA-304
  choice of a 1 GiB default. AIRA-317 (held, owner) asks whether to DELETE the
  p90 rung. **Sequencing requirement:** fix 2 is built as its own commit, so it
  can be dropped without touching fix 1 if the owner decides AIRA-317(2) first.
  The coordinator should surface this before merge; it is not this plan's call.
- **Ledger:** unchanged (I7).
- **Index build on first open after upgrade:** 0.60-3.86 s measured on the copy, under
  the store's `busy_timeout(5000)`. A concurrent opener (CLI fallback, detached
  supervisor) waits up to that long once. Risk R3.

### 6.3 Restart and reload

Re-declare (I8): unchanged. Daemon start: the index swap runs inside `Open`
before the daemon serves, so admissions only wait at the socket for the build.
The p90 cache starts empty after a restart (I10, cold path unchanged).

### 6.4 Superseded property

`TestNoAdmissibleResolutionIsLargerThanMasters` (AIRA-153 I3,
`admit_ceiling_fitted_test.go:384`) asserts the resolution never grants more than
pre-AIRA-153 master. Fix 1 deliberately grants more where own evidence demands it
(for example shape `{n=1, PeakMax=56360960}` with hint `1 MiB`). The test must be
amended, not deleted: rows whose basis is `estimate:own-max=` are exempt from the
`got > master` assertion and instead assert `got == runner.GrowByEstimatorMargin(stats.PeakMax)`;
every other row keeps the I3 assertion. The amendment cites AIRA-314 I1.

## 7. Test plan

All new admission tests use a **real store** (`store.OpenDB` in `t.TempDir()`),
rows written through `RecordConfinePeak` (or the daemon's `confineReport`
handler where noted), and `server.db = db` with **no** `admitPeakHistory` /
`admitPeakP90` seam, so the real readers run. The prior is produced by seeding
other signatures with three rows each (the real `ConfinePeakP90`). For every new
test the builder records in the commit message that the named mutation was
applied, the test went red, and the mutation was reverted.

Fixture numbers (one table, used throughout): request hint = 1 GiB (unpinned
default), prior signatures put p90 at 1.5 GiB (so `p` = 1.725 GiB), ceiling =
8 GiB (`fit` = 7469508340). Peak classes: **below prior** 500 MiB; **above
prior** 2947493888 (RANT-52's clean peak); **above ceiling** 7.5 GiB
(`F` = 8.625 GiB > ceiling). History kinds: **clean** = every row clean at the class peak; **OOM-only** =
every row an OOM at the class peak; **mixed** = one OOM row at half the class
peak, the rest clean at the class peak (so the largest peak is clean).

| # | Test (package) | What it asserts | Mutation that must turn it red |
|---|---|---|---|
| T1 | `TestOwnPeakFloorTable` (daemon) | Table over samples {0, 1, 2, 3, 20} x history {clean, OOM-only, mixed} x peak class {below prior, above prior, above ceiling} (impossible cells, such as `n = 1` mixed or `n = 0` with a class, are listed and skipped by name). Each row asserts the exact value AND exact basis, written as literal expected values, not recomputed by a copy of the algorithm. Rows `n = 0, 3, 20` and every OOM-only row equal today's literal outputs. | (a) delete the floor; (b) change `< 3` to `<= 3` or `< 2`; (c) drop the `PeakMax > MaxOOMPeak` condition (OOM-only rows change basis); (d) non-strict `>=` tie-break (tie row, below). |
| T2 | `TestOwnPeakFloorBeatsAnEscalationBelowTheCleanPeak` (daemon) | RANT-52 replay rows: OOM at 1778917376 then clean at 2947493888 → `3389617971`, basis `estimate:own-max=2947493888,n=2,f=115,oom-on-record`; and a row where the escalation does not raise (OOM 500 MiB, clean 1.2 GiB) → `F`, same basis shape. | Apply the floor only outside the OOM branch. |
| T3 | `TestOwnPeakFloorIsNeverFittedOrClamped` (daemon) | (i) clean 6.5 GiB + OOM 5.5 GiB at ceiling 8 GiB: escalation 8.25 GiB > ceiling, guard holds, today → `fit` (7469508340); new → 8026220134, no `,ceiling-clamped`. (ii) clean 7.5 GiB, no OOM: returns 9261023232 > ceiling. (iii) clean 7.5 GiB + OOM 5.5 GiB: returns 9261023232, not `fit`. | Apply the floor before the clamp (or fit it to `fit`). |
| T4 | `TestOwnPeakFloorReplacesAFittedPrior` (daemon) | Ceiling 1.5 GiB with the 1.725 GiB prior → `fit` 1400532813, `estimate:p90-prior,ceiling-fitted` today; clean 1.2 GiB at `n = 1` → `F` = 1481763716 with basis lacking `,ceiling-fitted`. Same for the SITE-1 fitted default: ceiling 896 MiB, NO prior signatures, clean 700 MiB at `n = 1` → today `fit` 816977474 `fallback:insufficient-samples,ceiling-fitted`, new 844103680 `estimate:own-max=734003200,n=1,f=115`. | Keep the fitted suffix on a floor win. |
| T5 | `TestOwnPeakFloorWithNoReadablePrior` (daemon) | RANT-55 replay, no prior signatures: clean 1099423744 at `n = 1` → 1264337305 `estimate:own-max=1099423744,n=1,f=115`, not 1 GiB. Control row `n = 2` (that clean row plus the OOM row at 1073741824): the escalation 1610612736 exceeds `F`, so the result and basis stay today's `estimate:oom-escalated` (the floor must not relabel a value it did not set). | Delete the floor at the post-block fallback returns only. |
| T6 | existing `TestTheAcceptedR3BandReturnsMoreThanMasterAndIsRefused`, `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes`, `TestPostBlockInsufficientSamplesFallbackIsUnchanged` | Stay green unmodified. | (c) above turns the R3 rows red. |
| T7 | existing `TestFittingNeverTouchesAPinnedRequest` + one new row in T1 | A pinned request with a large own peak returns `pinned:client` and the pinned value. | Move the floor above the pinned return. |
| T8 | `TestOwnPeakFloorOverTheCeilingIsRefusedWithTheOwnHistoryAdvice` (daemon, wire) | Through `admitConnection` over `net.Pipe` with the real store, slice maximum chosen so `cap - headroom` is 8 GiB: clean 7.5 GiB at `n = 1` → `E_ADMIT_TOO_LARGE` with `required=9261023232`, basis `estimate:own-max=...`, and the "OWN measured peak history" advice; no queue created. | Fit the floor to `fit`. |
| T9 | `TestHintEligibleRunFloorsTheNextUnpinnedReserve` (daemon) | Record a pinned clean run through the real `confineReport` handler (`peak_rss` 1.1 GiB, budget 3 GiB, `cap:operator:--memory-reserve`), then resolve the same signature unpinned, with and without a readable prior: result `>= GrowByEstimatorMargin(peak)` at `n = 1` and `n = 2`. Pairs with the existing runner test that the hint fires for that status. | Delete the floor. |
| T10 | `TestCachedP90ServesLastGoodAfterARealReadFailure` (daemon) | Real store with prior signatures; first call caches `p`; close the DB; advance `admitNow` past `admitPriorRefresh`; call returns the old `(p, true)`. A second call inside the new minute does not re-read (assert via the closed DB: the result is still served and no error path is taken, plus a seam-counted variant for the "at most once per minute" half). A cold server whose first read fails returns `(0, false)`. | Return `(0, false)` on error; or stamp `admitPriorAt` on a cold failure. |
| T11 | `TestOpenReplacesTheOldPeakHistoryIndexes` (store) | Real `OpenDB`; then, with raw SQL on the same file, recreate both old indexes and drop the new one (the pre-change shape), insert rows, close; reopen through `OpenDB`: `sqlite_master` lists exactly `confine_peak_history_kind_signature_peak` for the table, rows intact, `ConfinePeakP90` and `ConfinePeakHistory` return the same values as before the reopen. | Remove either `DROP`; remove the `CREATE`. |
| T12 | `TestConfinePeakP90ReadsOnlyTheCoveringIndex` (store) | `EXPLAIN QUERY PLAN` of the hoisted p90 constant on a real opened DB reports `USING COVERING INDEX confine_peak_history_kind_signature_peak`. Deterministic, replaces a wall-clock volume test. | Restore the old `(kind, signature)` index only. |
| T13 | `TestNoAdmissibleResolutionIsLargerThanMasters` (amended, §6.4) | Own-max rows assert `got == GrowByEstimatorMargin(PeakMax)`; all other rows keep I3. | Delete the floor (own-max rows then fail the equality). |
| T14 | skill text test (`internal/core/skill_test.go`) | The guide names `estimate:own-max` and states that one or two recorded peaks floor the reserve. | Drop the sentence. |
| T15 | `TestOwnPeakFloorRealCgroup` (daemon, real cgroup, skips without delegation like `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission`) | The RANT-55 reproduction end to end: pinned run of a workload allocating ~1.1x of 1 GiB, then the identical argv unpinned: no OOM, trailer `reserve-basis=estimate:own-max=...`. | Delete the floor (the second run is OOM-killed at 1 GiB). |

Tie row for T1(d): clean peak chosen so `F` equals the p90 value exactly → basis
stays `estimate:p90-prior`.

Gate: `aira confine -- make ci` from the worktree, exact exit code recorded;
`-race` on `./internal/daemon/... ./internal/store/...`.

## 8. Risks

- **R1 Label regressions in a dense function.** `resolveAdmitReserve` carries
  AIRA-149/151/153's basis-provenance rules. Mitigation: exact-basis assertions
  in T1-T5, the uniform single application point, and Fable's review.
- **R2 Fix 2 raises cold-start reserves** (§6.2) against AIRA-304's intent until
  AIRA-317 is decided. Mitigation: separate commit; flagged to the coordinator.
- **R3 One-time index build** (0.60-3.86 s on the copy) inside `Open`, under a
  5 s `busy_timeout`. A slower disk could push a concurrent opener to
  `SQLITE_BUSY` once at cutover. Mitigation: cutover on a quiet box per the
  shared-daemon restart care; the builder re-measures the build inside the real
  `OpenDB` on a copy and records it.
- **R4 The OOM advisory text** (`confine_linux.go`, daemon-reserve arm) says the
  re-run is refused only where the OOM peak is at what the slice can give. With
  the floor, a re-run can also be refused when an earlier clean peak `x1.15`
  exceeds the ceiling. That needs a clean run recorded AFTER the OOM run printed
  its advisory (a clean run before it would already have been refused), so the
  advisory was true when printed. Accepted, noted, no change.
- **R5 Stale prior persists** while reads keep failing. Acceptable for a slow
  machine-wide guess; bounded by the next successful read.

## 9. Expected yield

- The 37 measured kill-after-one-or-two-clean-runs signatures would not have
  been killed; RANT-52 and RANT-55 are prevented.
- The p90 read drops from 0.17-0.32 s to 0.03-0.07 s (sqlite3), inside the 250 ms
  budget with margin; a transient failure no longer drops the prior for that
  admission.
- One index fewer on every `RecordConfinePeak`.
- The over-reserve hint's "drop the pin" advice becomes safe at every sample count.

## 10. Explicit deferrals (not built)

- **D1 AIRA-273 option A** (escalate to `max(1.5 x MaxOOMPeak, p90)`): held by
  the owner; RANT-54 is its third instance. Fix 2 is its prerequisite, nothing
  more.
- **D2 Deleting the p90 rung**: AIRA-317, owner decision. This plan neither
  deletes it nor argues for it; it makes the existing rung honest and fast.
- **D3 The floor at `n >= 3` inside the OOM clamp**: with three or more samples
  and an OOM on record, the AIRA-153 clamp can return `fit` below
  `1.15 x` a clean peak still in the window. Pre-existing, rare (needs a clean
  peak above ~0.76 x ceiling plus an over-ceiling escalation), and it changes
  AIRA-151/153 tested behaviour. Recommend a held follow-up ticket; not built.
- **D4 AIRA-246's total-row cap**: stays retired. The index plus keep-last-good
  cover the admission cost; the measurement is recorded on the ticket.
- **D5 A separate context budget for the p90 read**: measured as worth ~1 ms
  (§2); not built.
- **D6 Collapsing `fallback:insufficient-samples`**: it remains reachable (own
  floor below the client default with no readable prior) and is an honest label.
  Not removed.
- **D7 Gating the hint on sample count**: not needed (§5.3); the client cannot see
  the sample count without new plumbing.
