# AIRA-314 — a command's own clean peak floors its unpinned reserve; the p90 prior read stops timing out

Ticket: AIRA-314 (P2 bug, milestone v0.40). Source: the 2026-10-25 rant triage
(RANT-52, RANT-55; RANT-54 is AIRA-273). Investigator report:
`~/tmp/aira-rant-verify/RANT-52_RANT-54_RANT-55.md` (re-checked against the code;
every file:line in this spec was opened on `ff03d8aa`).

Process class: **correctness-critical admission core** (it changes what
`resolveAdmitReserve` returns). Full two-loop: Opus builds TDD, Fable reviews in a
throwaway detached worktree. Daemon-side change: the shared-daemon restart care
applies at cutover.

Revision 2 (this text) answers the plan review and plan gate of revision 1. The
rule is now ONE sentence (§2) at every sample count, keyed on a new
max-clean-peak aggregate; the `n < 3` bound, the `PeakMax > MaxOOMPeak` proxy,
the exported-constant plumbing and deferral D3 are gone. Change log in §11.

## 1. The problem, in plain terms

An unpinned `aira confine -- <cmd>` asks the daemon to size its memory reserve. The
daemon looks up this exact command line's recorded peaks:

- with **three or more** recorded peaks it reserves `max peak x 1.15`;
- with **one or two** recorded peaks it throws them away and uses a machine-wide
  guess (the p90 of other commands' peaks x 1.15, about 1.71 GiB on this box
  today) or, if that guess cannot be read, the 1 GiB client default. With an OOM
  on record it uses `1.5 x` the OOM peak if that is larger, and never looks at the
  clean peak beside it.

So a command that has just run once cleanly at 1.02 GiB (RANT-55, pinned at 3G)
is re-run unpinned, gets 1 GiB, and is OOM-killed. A command that ran cleanly at
2.75 GiB (pinned) after an OOM at 1.66 GiB (RANT-52) is next sized at the 1.5x
escalation, 2.49 GiB, below its own clean peak. The evidence that would have
prevented the kill was in the database and was ignored.

A narrower form of the same gap exists at **three or more** samples: when an OOM
is on record and `1.5 x` the OOM peak is over the slice ceiling, AIRA-153 clamps
the reserve to `fit` (the largest reserve the slice can grant one job), which can
be below a clean peak the command already reached. Only reachable on small slices
(no signature on the live box can reach it on the 60 GiB `aira.slice`, §1 table).

Second, the machine-wide guess itself is unreliable. Its query is a full-table
`GROUP BY` over 185k rows / 124k signatures, takes 0.15-0.32 s (sqlite3 CLI;
reported by the investigator as 0.24-0.30 s warm in Go), and runs under the 250 ms
admit deadline (`admit.go:39`). A failed read is not cached, so admission drops to
the 1 GiB default for that request and the next request pays the slow read again.
Reported by the investigator: in isolation the Go read failed 2 of 3 times under a
250 ms context, and 3 of 3 fresh unpinned probes on the live daemon got
`fallback:no-history` (1 GiB) instead of `estimate:p90-prior`. The p90 rung is
effectively off on this box today.

Measured on fresh copies of the live `state.db` (2026-10-25). Every figure below is
reproduced by `docs/dev/aira-314-p90-index-measurement-repro.sh`; the counts are
from the revision run.

| Fact | Value |
|---|---|
| p90 peak / prior `p` = p90 x 1.15 | 1598423040 / 1838186496 |
| signatures with 1-2 recorded peaks | 103,890 |
| ... whose largest CLEAN peak `x1.15` exceeds 1 GiB (raised when the p90 is unreadable) | 12,272 |
| ... whose largest CLEAN peak `x1.15` exceeds `p` (raised even when the p90 works) | 6,929 |
| signatures whose largest clean peak `x1.15` exceeds 60 GiB (would newly be refused on `aira.slice`) | 0 |
| signatures whose largest clean peak is `<=` their largest OOM peak (what a `PeakMax > MaxOOMPeak` test misses) | 161 |
| signatures with an OOM peak whose `x1.5` exceeds 60 GiB (can reach the AIRA-153 clamp on `aira.slice`) | 0 (largest peak recorded: 45178662912) |
| signatures where an UNPINNED run was OOM-killed with a reserve below 1.15x a clean peak recorded before it, 1-2 peaks before | 40 (an upper bound, see below) |
| ... same, 3 or more peaks before | 210 (confound gauge, see below) |
| p90 read, old index (three runs of the script) | 0.208 / 0.212 / 0.315 s; 0.246 / 0.230 / 0.165 s; 0.152 / 0.207 / 0.204 s |
| p90 read, covering index | 0.042 / 0.071 / 0.053 s; 0.046 / 0.048 / 0.032 s; 0.064 / 0.057 / 0.064 s |
| one-time covering-index build | 3.86 s; 0.60 s; 0.60 s |

How to read the two kill counts. A row's `at` is its COMPLETION time, and the
table stores no start time. A clean run that finished after the OOM run was
admitted (two runs of one command in parallel) still counts as "before". At 3 or
more peaks the reserve was already `>= 1.15x` every clean peak known at admission,
except through the clamp band on a small slice, a history read that timed out, or
rows older than the estimator. So most of the 210 are those confounds, and the
same confounds inflate the 40. The 40 is an upper bound on kills fix 1 would have
prevented in retained history; RANT-52 and RANT-55 are two verified sequential
instances.

## 2. Greenfield minimum

If we solved only this problem from scratch:

1. **An unpinned reserve is never below `GrowByEstimatorMargin(largest clean
   recorded peak)` whenever a clean peak is recorded.** One `max()` applied once,
   after the existing resolution, plus the one aggregate it needs.
2. The machine-wide prior is a cheap, cached number that does not vanish on a slow
   read: one index that lets the p90 query read only the index, and a cache that
   keeps its last good value.

No new wire field, no new table, no schema migration, no new error code, no change
to recording.

### The gap between that minimum and this plan, each item justified

| Item beyond the minimum | Why it is kept |
|---|---|
| A new `MaxCleanPeak` field on `runner.PeakRSSStats` and one SQL term in `ResourcePeakHistory` | It IS the minimum's "largest clean recorded peak"; no existing field carries it. `PeakMax > MaxOOMPeak` identifies a clean peak only when the clean peak is the largest, and misses 161 live signatures (§1) where a clean peak sits beside a larger OOM peak; in the clamp band those still get `fit < 1.15 x` the clean peak (§6.1, row X3). `resource_budget.go:197-204` already splits clean and OOM peaks the same way. |
| A basis token naming the floor (`estimate:own-max=<MaxCleanPeak>,n=<SampleCount>,f=115`, plus `,oom-on-record` iff `OOMCount > 0 && MaxOOMPeak > 0`) | AIRA-149's rule: `reserve-basis` names the term that set the number. Without it the trailer would print `estimate:p90-prior`, `fallback:insufficient-samples` or `estimate:oom-escalated,ceiling-clamped` for a value none of them produced. `,oom-on-record` keeps AIRA-128's attribution property, under exactly the OOM branch's own entry condition (`admit.go:1484`) so AIRA-149 I3's negative direction (`admit_oom_basis_test.go:256-280`) still holds. |
| The covering index REPLACES the two existing indexes instead of being added | `(kind, signature, peak_rss)` has `(kind, signature)` as its prefix, so every reader that used the old composite index still has it. `confine_peak_history_signature` (signature alone) has no reader: every query on the table filters `kind=?` first or scans it in full (`confine_peak_history.go` readers). Removing it takes one index write off every `RecordConfinePeak`. |
| Keep-last-good also stamps the refresh time on a failed read when a last good value exists | Without the stamp, every admission during a slow spell re-runs the failing read and pays up to 250 ms, which is today's behaviour. With it, the failing read is retried at most once per `admitPriorRefresh` (1 minute, `admit.go:66`). One assignment. With NO last good value (cold daemon) nothing is stamped, so a cold start keeps retrying exactly as today. |
| The committed measurement reproduction script | Repo rule: a published measurement has a committed, executable reproduction. It now reproduces every count in §1 and §6.2, not only the timings. |

**Removed from the brief's options, with the measured reason:** a separate
context budget for the p90 read. The per-signature read that shares the 250 ms
context costs about 1 ms before and after the index (repro script, "per-signature
history read"), so the shared budget costs the p90 read about 1 ms. A second
budget buys nothing measurable and doubles worst-case admission latency to 500 ms.
Not built (deferral D5).

**Removed in revision 2:** the `0 < SampleCount < 3` bound (§11). At `n >= 3` the
floor equals the ordinary estimate or loses to it everywhere except inside the
AIRA-151/153 clamp (proof in §5.1), so dropping the bound changes behaviour only
where the bound left a reserve below a clean peak already reached, removes a
downward step from 2 samples to 3, and deletes the need to export
`memoryEstimateMinSamples`.

## 3. Scope

In:

- **Fix 1a** `internal/store/confine_peak_history.go: ResourcePeakHistory` and
  `internal/runner/types.go: PeakRSSStats`: the `MaxCleanPeak` aggregate.
- **Fix 1b** `internal/daemon/admit.go: resolveAdmitReserve`: the floor.
- **Fix 2a** `internal/store/store.go`: replace the two `confine_peak_history`
  indexes with `confine_peak_history_kind_signature_peak ON (kind, signature, peak_rss)`.
- **Fix 2b** `internal/daemon/admit.go: cachedAdmitPeakP90`: keep serving the last
  good value when a refresh fails.
- **Skill text** `internal/core/skill.go:335` (the generated agent guide): name
  the new basis token and correct the thresholds (§5.4).
- **Ticket notes** on AIRA-246 and AIRA-242 recording the §1 measurement (the
  figures AIRA-246's retirement said were missing; AIRA-242's "admission
  unaffected" missed the p90 read). Status unchanged on both.
- **Over-reserve hint** `internal/runner/confine.go:1155-1157`: wording KEPT,
  with a test pinning the property that makes it true (§5.3, T9).

Commit sequencing: fix 1 (1a, 1b, skill text) and fix 2 (2a, 2b) are separate
commits, so fix 2 can be dropped without touching fix 1 if the owner decides
AIRA-317 first (§6.2). The coordinator surfaces §6.2's population before merge.

Out: everything in §10.

## 4. Invariants

- **I1 Floor.** For every unpinned request whose per-signature read succeeded with
  `MaxCleanPeak > 0`, the result is `>= F := runner.GrowByEstimatorMargin(MaxCleanPeak)`.
- **I2 Monotone.** For every input, the new resolution is `>=` today's: the floor
  is a `max()` over today's value. (Deliberately breaks AIRA-153's I3 "never
  grants more than master" on rows where `F >` master's value; §6.4.)
- **I3 Pinned untouched.** `request.pinned` returns `pinned:client` before any
  floor exists (`admit.go:1399-1401`).
- **I4 No floor without own clean evidence.** The floor is zero unless the
  per-signature read succeeded and `MaxCleanPeak > 0`. No-signature,
  history-unavailable, no-history, capture-unavailable (`SampleCount == 0`) and
  OOM-only paths are byte-identical to today, at every sample count.
- **I5 A floor is a measurement, never fitted.** `F` is not reduced to `fit` or the
  ceiling. Over the ceiling it is returned as is and `admitConnection` refuses it
  `E_ADMIT_TOO_LARGE` (`admit.go:1792-1795`), exactly as AIRA-153 treats an
  over-ceiling ordinary estimate, with the existing `estimate:` advice arm ("this
  command's OWN measured peak history", `admit.go:3035`), which is true for it.
- **I6 Label names the term that acted.** The basis is the floor's token only when
  `F` STRICTLY exceeds today's value; on an exact tie today's basis is kept
  (AIRA-149's strict tie-break). On a floor win `,ceiling-fitted` and
  `,ceiling-clamped` are dropped (the value is neither the fitted prior nor the
  clamp), and `,oom-on-record` is appended iff `OOMCount > 0 && MaxOOMPeak > 0`
  (the OOM branch's own entry condition, so nothing outside that branch names an
  OOM record).
- **I7 Recording unchanged.** `shouldRecordConfinePeak` (`confine_linux.go:2909`,
  AIRA-264) is not touched: a non-OOM failed run leaves no row; a clean row is an
  exit-0, unsignalled run, i.e. an uncensored measurement. Pinned clean runs are
  recorded today and feed the floor.
- **I8 Ledger conservation unchanged.** The floor changes only the requested
  reserve handed to `admitConnection`; the `reserve > ceiling` refusal, the enqueue
  re-check and the grant gate (`Σgranted <= cap - headroom`) are downstream and
  untouched.
- **I9 Restart paths unchanged.** The ARDR re-declare re-declares the GRANTED
  reserve through `enqueueReDeclare` and never calls `resolveAdmitReserve` (its
  single caller is `admitConnection`, `admit.go:1792`); the p90 cache is in-memory
  and starts cold after a restart.
- **I10 Schema needs no migration.** Opening any existing `state.db` (old indexes
  present) through the real `store.OpenDB` (`initDB` runs only there,
  `store.go:547`) ends with exactly the covering index and neither old index, rows
  intact. Reopening an already-migrated DB runs no index DDL.
- **I11 A stale prior is the last SUCCESSFUL read.** A failed refresh never invents
  a value: it serves the last successful `(peak, ok)` pair (possibly `(0, false)`
  if that success found no qualifying signatures) or, with no success yet,
  `(0, false)` as today.

## 5. Design

### 5.1 Fix 1 — the floor

**1a, the aggregate.** `runner.PeakRSSStats` gains `MaxCleanPeak int64`.
`ResourcePeakHistory` (`confine_peak_history.go:180-186`) adds one term and one
Scan target:

```sql
COALESCE(MAX(CASE WHEN oom=0 AND peak_rss>0 THEN peak_rss END),0)
```

No other producer of `PeakRSSStats` feeds `resolveAdmitReserve` (the only readers
are the `admitPeakHistory` seam and `db.ConfinePeakHistory`, `admit.go:1445-1448`).
The runs-table historian (`runner/peak_rss_history.go`, `aira run`) and the budget
classifier's own stats do not fill it and do not need it. The struct has no wire
encoding.

**1b, the floor.** Move today's body unchanged into an inner function that also
returns the stats it read and whether the read succeeded, and apply the floor
once in `resolveAdmitReserve`:

```go
func (s *Server) resolveAdmitReserve(request admitRequest, ceiling int64) (int64, string) {
    reserve, basis, stats, read := s.resolveAdmitReserveBeforeFloor(request, ceiling)
    if !read || stats.MaxCleanPeak <= 0 { // pinned returns read=false
        return reserve, basis
    }
    floor := runner.GrowByEstimatorMargin(stats.MaxCleanPeak)
    if floor <= reserve { // strict: a tie keeps today's basis
        return reserve, basis
    }
    own := fmt.Sprintf("estimate:own-max=%d,n=%d,f=115", stats.MaxCleanPeak, stats.SampleCount)
    if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
        own += ",oom-on-record"
    }
    return floor, own
}
```

One application point, outside every return of the body, so no return can escape
it. `n` is `SampleCount`, the usable-sample count, with the same meaning as in
`estimate:max=%d,n=%d,f=115`.

**Where the floor can act.** Let `C` = `MaxCleanPeak`, `F` = `1.15C`.

- `n < 3`: no usable ordinary estimate exists, so today's value is a prior (`p`,
  the hint, or their fitted forms) or the OOM escalation `E = 1.5 x MaxOOMPeak`,
  possibly clamped to `fit`. Any of these can be below `F`; the floor raises them.
- `n >= 3`: the ordinary estimate is `GrowByEstimatorMargin(PeakMax)`
  (`resource_estimate.go:137-149`, `:110-122`, same arithmetic) and
  `PeakMax >= C`, so it is `>= F`. The OOM branch only raises it (`E > reserve`),
  except for AIRA-153's clamp to `fit` (`admit.go:1583`). So at `n >= 3` the floor
  acts only when the clamp fired and `F > fit`. (Above
  `MaxMemoryEstimateReserve` both sides cap at the same value: a tie.)

Outcome table, every sample count, `C > 0`:

| Today's path | Today's value | New value | New basis |
|---|---|---|---|
| no usable estimate, no OOM branch, p90 readable | `p` (or `fit` if `p >= ceiling`) | `max(that, F)` | own-max iff `F >` it |
| no usable estimate, no OOM branch, p90 unreadable | hint (or `fit`) | `max(that, F)` | own-max iff `F >` it |
| OOM branch, escalation did not raise | hint / `fit` (n<3), ordinary (n>=3, `>= F`) | `max(that, F)` | own-max`,oom-on-record` iff `F >` it (never at n>=3) |
| OOM branch, `E` raised, `E <= ceiling` | `E` | `max(E, F)` | own-max`,oom-on-record` iff `F > E` (RANT-52: `E` = 2.49 GiB, `F` = 3.16 GiB); never at n>=3 |
| OOM branch, `E > ceiling`, guard `MaxOOMPeak < fit` holds | `fit` (`,ceiling-clamped`) | `max(fit, F)`; refused if `F > ceiling` | own-max`,oom-on-record` iff `F > fit` (any n) |
| OOM branch, `E > ceiling`, guard fails | `E` (refused) | `max(E, F)` (refused) | `E`'s basis unless `F > E` |
| usable ordinary estimate, no OOM branch | `>= F` | unchanged | unchanged (tie or loss) |

Why the clamp row is right, at every n: AIRA-151's rule is that an OOM record must
not make the same number behave differently. The same clean history with no OOM
on record yields at least `F`, refused when `F > ceiling`; the clamp must not
quietly grant `fit < F` because an OOM is also on record. The clamp's guard
(`MaxOOMPeak < fit`) is about the OOM peak, not the clean one, and is untouched.

Case C, unchanged on purpose: an OOM-only signature (`C == 0`) whose `MaxOOMPeak`
lies in `(fit/1.15, fit)` is clamped to `fit`, which is above `MaxOOMPeak` and
below `1.15 x MaxOOMPeak`. An OOM peak is censored evidence (the job was killed at
its cap), so AIRA-153's rung stands. No clean peak, no floor.

Note on `fit < F <= ceiling`: an own-evidence value in `(fit, ceiling]` is
admissible and may wait for room; AIRA-153 already accepts that for an escalation
and an ordinary estimate landing there (`admit.go:1558-1561`).

### 5.2 Fix 2 — the prior

**2a, index.** In `ensureConfinePeakHistorySubject` (`store.go:1312-1327`):

```sql
CREATE INDEX IF NOT EXISTS confine_peak_history_kind_signature_peak
    ON confine_peak_history(kind, signature, peak_rss);
DROP INDEX IF EXISTS confine_peak_history_kind_signature;
DROP INDEX IF EXISTS confine_peak_history_signature;
```

Create before drop, so no window exists with no `(kind, signature)` prefix index.
Also DELETE the DDL-block `CREATE INDEX IF NOT EXISTS confine_peak_history_signature`
(`store.go:881-882`); otherwise every open would rebuild it and drop it again (T11
pins this). With the covering index the p90 plan is `SEARCH ... USING COVERING
INDEX confine_peak_history_kind_signature_peak (kind=?)` (measured, §1); the
per-(kind, signature) readers and the retention `DELETE` keep their prefix lookup.
Hoist the p90 SQL into a package constant so the plan test (T12) checks the exact
text `ConfinePeakP90` runs.

**2b, keep last good.** `cachedAdmitPeakP90` (`admit.go:1630-1654`), on
`err != nil`: under `admitPriorMu`, if `!admitPriorAt.IsZero()` return the cached
`(admitPriorPeak, admitPriorOK)` and set `admitPriorAt = now`; otherwise return
`(0, false)` and stamp nothing. Success path unchanged. The basis stays
`estimate:p90-prior`: a value up to a minute old is already served today, and the
prior is a slow-moving guess about other commands.

### 5.3 The over-reserve hint (`confine.go:1155-1157`)

It fires only for a pinned, clean, exit-0 run with reserve `>= 2 GiB` and peak
`< 60 %` of it. Such a run is recorded as a clean row (I7). In that domain, on the
same slice, `C < 0.6 x reserve <= 0.6 x ceiling`, so `F = 1.15C < 0.69 x ceiling <
fit`: the floor is always admissible. By I1 the next unpinned run of the same argv
is sized at no less than `F`, at every sample count, clamp band included. So
"drop --memory-reserve and let aira estimate it from this command's history" is
true advice. The basis may say `estimate:p90-prior` when the machine-wide prior
is larger than `F` (the prior acted), but the reserve is never lower than this
command's history demands. Wording kept; T9 pins the property.

### 5.4 Skill text (`internal/core/skill.go:335`)

Today it teaches that one or two recorded observations get a prior or a fallback
basis ("fewer than three usable"). New text, in the same paragraph:

- once a command has a recorded CLEAN peak (even one, including a pinned run), its
  unpinned reserve is never below `1.15 x` that peak, and when that figure is what
  set the reserve the basis is `estimate:own-max=<peak>,n=<samples>,f=115`
  (plus `,oom-on-record` when an OOM record was consulted);
- from three usable samples the ordinary estimate (`estimate:max=...`) normally
  sets it;
- `fallback:insufficient-samples` remains, for a command whose recorded peaks are
  all OOM peaks or unknown, or whose clean peak `x1.15` is below the prior;
- the `E_ADMIT_TOO_LARGE` sentence already covers "this command's own measured
  peak-history estimate"; add that it now also applies after one or two clean runs
  whose peak `x1.15` exceeds the ceiling, and after an OOM when an earlier clean
  peak `x1.15` exceeds it (R4).

## 6. False-pass and false-fail analysis

### 6.1 False pass: a reserve that is too SMALL

| Path | Can it still return `< 1.15 x own largest clean peak`? |
|---|---|
| p90 prior, p90 fitted, insufficient-samples fallback, default fitted | No: the floor wraps every return of the body. T1, T4, T5. |
| OOM branch, escalation did not raise | No. T1 (n=2 mixed below/above). |
| OOM branch, escalation raised below `F` | No. T1, T2 (RANT-52 replay). |
| OOM clamp to `fit` with `F > fit`, any n | No: `max(fit, F)`; over the ceiling it is refused, not fitted. T1 X2-X6. |
| clean peak `<=` a larger OOM peak, clamp band | No: keyed on `MaxCleanPeak`, not on `PeakMax`. T1 X3. |
| OOM row with an unknown peak beside a clean row | No: floor applies; basis has no `,oom-on-record` (OOM branch not entered). T1 n=1 mixed. |
| history read fails | Yes, by design: no stats, so no floor; basis says `fallback:history-unavailable`. Unchanged and honest. |
| OOM-only history | No floor (no clean peak, I4); escalation or AIRA-153's clamp governs (§5.1 case C). T1 OOM-only rows, X1. |
| pinned run | Untouched (I3). T1 X8, T7. |
| failed run recorded and later trusted | Cannot happen: AIRA-264's send-side gate is unchanged (`TestShouldRecordConfinePeakTracksTheClassifier`); the floor reads only clean rows. |
| hand-built seam fixture omits `MaxCleanPeak` | The floor is silently off in that test. Mitigation: every new floor test uses the real store reader; the two amended property grids (T13, T17) carry `MaxCleanPeak` and fail on a store-inconsistent shape; T18 pins the reader. Existing seam tests keep `MaxCleanPeak = 0` and pin today's behaviour for OOM-only / no-clean shapes. |
| label lies about which term acted | Strict tie-break, suffixes dropped on a floor win, `,oom-on-record` only under the OOM branch condition. T1 asserts value AND exact basis on every cell; T13/T17 assert the label invariants over grids. |
| stale p90 served after a failure | Serves a real earlier read, never a fabricated one (I11). T10. |

### 6.2 False fail: a reserve that is too LARGE, or a new refusal

- **Zero samples:** unchanged (I4). T1 rows `n = 0`.
- **`n >= 3` outside the clamp band:** unchanged (proof §5.1). T1 rows `n = 3, 20`.
- **New `E_ADMIT_TOO_LARGE`:** (i) a command whose clean peak `x1.15` exceeds the
  ceiling after one or two samples, which today got a 1 GiB or ~1.71 GiB guess and
  would almost certainly have been OOM-killed; (ii) at any n with an OOM on record,
  a clean peak above `fit` where today the clamp granted `fit` below that clean
  peak. Both now refused immediately, naming both numbers. This is AIRA-153's rule
  for ordinary estimates. On the live data the count above 60 GiB is 0 and no
  signature can reach the clamp band on `aira.slice` (§1); it bites only on small
  slices (ci-shim budgets, fixtures). One honest caveat: `peak_rss` is the scope's
  memory peak, which can include reclaimable page cache, so a job refused here
  might have survived at `fit`. The same is already true of every over-ceiling
  ordinary estimate; accepted under the same rule. T1 X6, T8.
- **`fit < F <= ceiling`:** admissible, may wait where `fit` would have been
  granted sooner (§5.1 note). Accepted, as AIRA-153 accepts it for own evidence.
- **Bimodal command:** one big pinned run makes the next cheap unpinned run
  reserve the big figure. Over-reservation is the safe direction and is what
  `estimate:max` does from three samples. Accepted.
- **Fix 2 makes the p90 prior apply far more often** (for the owner's AIRA-317
  decision). Today a failed p90 read falls to the 1 GiB default and is not cached.
  After fix 2 the read is fast and a failure serves the last good value, so the
  prior (`p` = 1838186496, ~1.71 GiB) applies essentially always where it is
  consulted. Population on the live copy: **96,292** signatures with 1-2 peaks, no
  OOM row and `F < p` would get `p` reliably instead of `p`-or-1-GiB; for
  **91,026** of them `F < 1 GiB`, so on every admission where today's read failed
  the reserve rises from 1 GiB to ~1.71 GiB. Plus every genuine cold start. The
  failure rate of today's read is not recorded anywhere (the reserve basis is not
  persisted; `budget_basis` holds only `cap:auto:daemon-reserve`), so the number of
  admissions moved is not measured. The investigator's probes (§1: 3 of 3 live
  cold starts fell to 1 GiB; 2 of 3 isolated reads timed out) say the rung is
  mostly off today, so in practice fix 2 turns it ON. On a slice that runs near full (the probe trailers in this round
  showed 50.5G of 51.5G granted with 10 jobs queued), +0.7 GiB per such job is
  material. It runs against AIRA-304's 1 GiB default; AIRA-317 (held) asks whether
  to delete the p90 rung. Hence the separate commit (§3).
- **Ledger:** unchanged (I8).
- **Daemon start after upgrade:** the one-time index build (0.60-3.86 s on the
  copy) runs inside `OpenDB`, which the daemon calls before `net.Listen`
  (`server.go:446`). So daemon listen-ready is delayed by that much once. Clients
  and keepers redial with no deadline; the re-declare freeze is armed at
  listen-ready, so the build does not eat into it; the build cannot hit a 250 ms
  request context because no request is served yet. R3.

### 6.3 Restart and reload

Re-declare (I9): unchanged. The p90 cache starts empty after a restart (I11,
cold path unchanged). Mixed binary versions on one state dir (older daemon
recreates the old indexes, newer drops them again) would flip-flop the indexes on
each start; accepted under the no-compat rule (one daemon per state dir since
v0.34; whether any older binary opens the box `state.db` writable is not verified).

### 6.4 Superseded property

`TestNoAdmissibleResolutionIsLargerThanMasters` (AIRA-153 I3,
`admit_ceiling_fitted_test.go:384`) asserts the resolution never grants more than
pre-AIRA-153 master. Fix 1 deliberately grants more where own evidence demands it.
It is amended (T13), not deleted. Rows are classified from their INPUTS, not from
the returned basis.

## 7. Test plan

All new admission tests use a **real store** (`store.OpenDB` in `t.TempDir()`),
rows written through `RecordConfinePeak` (or the daemon's `confineReport` handler
where noted), and `server.db = db` with **no** `admitPeakHistory` /
`admitPeakP90` seam, so the real readers run. The prior is produced by seeding ten
other signatures with three rows each at 1.5 GiB (the real `ConfinePeakP90`). For
every new test the builder records in the commit message that the named mutation
was applied, the test went red, and the mutation was reverted. Expected values are
literals computed by hand (given below), never recomputed by a copy of the
algorithm.

Fixture: hint `H` = 1073741824 (unpinned default); prior p90 = 1610612736, so
`p` = 1852204646; ceiling = 8589934592 (8 GiB), `fit` = 7469508340. Peak classes:
**below** 524288000 (`F` 602931200); **above** 2947493888 (RANT-52's clean peak,
`F` 3389617971); **over-ceiling** 8053063680 (`F` 9261023232 > ceiling). History
kinds: **clean** = every row clean at the class peak; **OOM-only** = every row an
OOM at the class peak; **mixed** = at `n = 1` an OOM row with NO peak (capture
failed) plus one clean row at the class peak; at `n >= 2` one OOM row at half the
class peak plus clean rows at the class peak.

### T1 `TestOwnPeakFloorTable` (daemon), the grid

| n | kind | below | above | over-ceiling |
|---|---|---|---|---|
| 0 | none / one row with no peak / one OOM row with no peak | `p`, `estimate:p90-prior` (all three) | n/a | n/a |
| 1 | clean | `p`, `estimate:p90-prior` | 3389617971, `estimate:own-max=2947493888,n=1,f=115` | 9261023232, `estimate:own-max=8053063680,n=1,f=115` |
| 1 | OOM-only | `H`, `fallback:insufficient-samples:n=1,oom-on-record` | 4421240832, `estimate:oom-escalated` | 12079595520, `estimate:oom-escalated` (guard fails) |
| 1 | mixed (OOM, no peak) | `p`, `estimate:p90-prior` | 3389617971, `estimate:own-max=2947493888,n=1,f=115` (NO `,oom-on-record`) | 9261023232, `estimate:own-max=8053063680,n=1,f=115` |
| 2 | clean | `p`, `estimate:p90-prior` | 3389617971, `...own-max=2947493888,n=2,f=115` | 9261023232, `...own-max=8053063680,n=2,f=115` |
| 2 | OOM-only | `H`, `fallback:insufficient-samples:n=2,oom-on-record` | 4421240832, `estimate:oom-escalated` | 12079595520, `estimate:oom-escalated` |
| 2 | mixed | `H`, `fallback:insufficient-samples:n=2,oom-on-record` | 3389617971, `...own-max=2947493888,n=2,f=115,oom-on-record` (today `E` 2210620416) | 9261023232, `...own-max=8053063680,n=2,f=115,oom-on-record` (today `E` 6039797760) |
| 3, 20 | clean | 602931200, `estimate:max=524288000,n=N,f=115` | 3389617971, `estimate:max=2947493888,n=N,f=115` | 9261023232, `estimate:max=8053063680,n=N,f=115` |
| 3, 20 | OOM-only | 786432000, `estimate:oom-escalated` | 4421240832, `estimate:oom-escalated` | 12079595520, `estimate:oom-escalated` |
| 3, 20 | mixed | 602931200, `estimate:max=524288000,n=N,f=115,oom-on-record` | 3389617971, `estimate:max=2947493888,n=N,f=115,oom-on-record` | 9261023232, `estimate:max=8053063680,n=N,f=115,oom-on-record` |

Every `n = 0`, OOM-only and `n >= 3` cell equals today's output (the floor ties or
does not apply). Explicit extra cells, ceiling 8 GiB unless stated:

| Cell | History | Today | New |
|---|---|---|---|
| X1 case C | OOM-only n=1 at 7247757312 | 7469508340 `estimate:oom-escalated,ceiling-clamped` | unchanged |
| X2 | n=2: clean 6979321856, OOM 5905580032 | 7469508340 `...,ceiling-clamped` | 8026220134 `estimate:own-max=6979321856,n=2,f=115,oom-on-record` |
| X3 clean < OOM | n=2: clean 6979321856, OOM 7247757312 | 7469508340 `...,ceiling-clamped` | 8026220134 `estimate:own-max=6979321856,n=2,f=115,oom-on-record` |
| X4 | n=3: clean 7247757312 x2, OOM 6442450944 | 7469508340 `...,ceiling-clamped` | 8334920908 `estimate:own-max=7247757312,n=3,f=115,oom-on-record` |
| X5 | n=20: clean 7247757312 x19, OOM 6442450944 | 7469508340 `...,ceiling-clamped` | 8334920908 `estimate:own-max=7247757312,n=20,f=115,oom-on-record` |
| X6 | n=3: clean 7516192768 x2, OOM 6442450944 | 7469508340 `...,ceiling-clamped` | 8643621683 `estimate:own-max=7516192768,n=3,f=115,oom-on-record` (over the ceiling: refused, T8) |
| X7 tie | n=1 clean at 1610612736 (`F` == `p`) | 1852204646 `estimate:p90-prior` | unchanged |
| X8 pinned | clean 8053063680 n=1; pinned request 2147483648 | 2147483648 `pinned:client` | unchanged |

Mutations that must each turn T1 red:
(a) delete the floor (n=1/2 above and over-ceiling clean and mixed cells, X2-X6);
(b) reintroduce `SampleCount < 3` (X4, X5, X6);
(c) key the floor on `PeakMax` instead of `MaxCleanPeak` (X1, and OOM-only below
at n=1/2, whose `F` beats `H`; also the existing `escalation over the ceiling,
clamped` row of `TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped`
once that seam fixture is touched, see T17);
(c') key it on `PeakMax > MaxOOMPeak` (X3);
(d) non-strict `>=` tie-break (X7);
(e) append `,oom-on-record` iff `OOMCount > 0` alone (n=1 mixed above and over-ceiling);
(f) keep `,ceiling-clamped` / `,ceiling-fitted` on a floor win (X2-X5);
(g) fit the floor to `fit` or apply it before the clamp (X2-X6 values);
(h) move the floor above the pinned return (X8).

### Other tests

| # | Test (package) | What it asserts | Mutation that must turn it red |
|---|---|---|---|
| T2 | `TestOwnPeakFloorBeatsAnEscalationBelowTheCleanPeak` (daemon) | RANT-52 replay, rows in time order: OOM 1778917376 then clean 2947493888 → 3389617971, `estimate:own-max=2947493888,n=2,f=115,oom-on-record` (today `E` 2668376064); and OOM 524288000 + clean 1288490188 (escalation does not raise `H`) → 1481763716, same basis shape. | Apply the floor only outside the OOM branch. |
| T4 | `TestOwnPeakFloorReplacesAFittedPrior` (daemon) | Ceiling 1610612736 with the prior → today `fit` 1400532813 `estimate:p90-prior,ceiling-fitted`; clean 1288490188 at n=1 → 1481763716 `estimate:own-max=1288490188,n=1,f=115`. SITE-1 fitted default: ceiling 939524096, NO prior signatures, clean 734003200 at n=1 → today 816977474 `fallback:insufficient-samples,ceiling-fitted`, new 844103680 `estimate:own-max=734003200,n=1,f=115`. | Keep the fitted suffix on a floor win. |
| T5 | `TestOwnPeakFloorWithNoReadablePrior` (daemon) | RANT-55 replay, no prior signatures, rows in time order: clean 1099382784 at n=1 → 1264290201 `estimate:own-max=1099382784,n=1,f=115`, not `H` (today: `H`, `fallback:insufficient-samples`, the kill). Control row n=2 (then the OOM at 1073741824): `E` 1610612736 > `F`, so result and basis stay `estimate:oom-escalated`. | Delete the floor on the post-block fallback path only; or relabel when `F` loses. |
| T6 | existing `TestTheAcceptedR3BandReturnsMoreThanMasterAndIsRefused`, `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes`, `TestPostBlockInsufficientSamplesFallbackIsUnchanged`, `TestFittingNeverTouchesAPinnedRequest` | Stay green unmodified. Their seam fixtures carry `MaxCleanPeak = 0` (OOM-only or no-clean shapes), so they pin today's behaviour where the floor must not act. The OOM-basis negative direction holds even with the floor active, because `,oom-on-record` follows the OOM branch's entry condition (measured by the plan gate with the floor on every shape). | (e) above would break the negative direction once a clean peak is present; that case is T1's n=1 mixed cell. |
| T8 | `TestOwnPeakFloorOverTheCeilingIsRefusedWithTheOwnHistoryAdvice` (daemon, wire) | Through `admitConnection` over `net.Pipe` with the real store, slice maximum chosen so `cap - headroom` = 8 GiB: (i) clean 8053063680 at n=1 → `E_ADMIT_TOO_LARGE`, `required=9261023232`, basis `estimate:own-max=8053063680,n=1,f=115`, the "OWN measured peak history" advice, no queue created; (ii) X6's history → `required=8643621683` with the `,oom-on-record` basis. | Fit the floor to `fit`. |
| T9 | `TestHintEligibleRunFloorsTheNextUnpinnedReserve` (daemon) | Record a pinned clean run through the real `confineReport` handler (`peak_rss` 1181116006, budget 3 GiB, `cap:operator:--memory-reserve`), then resolve the same signature unpinned, with and without a readable prior: result `>=` 1358283406 (`1.15 x` peak) after one, two and three such runs. Pairs with the existing runner test that the hint fires for that status. | Delete the floor. |
| T10 | `TestCachedP90ServesLastGoodAndRetriesAColdFailure` (daemon) | A counting seam wraps the REAL `db.ConfinePeakP90` and can be switched to fail. Warm: t0 real → `(p,true)`, reads=1; t0+61s failing → `(p,true)`, reads=2; t0+71s failing → `(p,true)`, reads=2 (failed refresh stamped: at most one read per minute); t0+122s real → `(p,true)`, reads=3. Cold (new server): t0 failing → `(0,false)`, reads=1; t0+1s real → `(p,true)`, reads=2. Plus a real-store variant with no seam: prime, close the DB, advance past the minute → `(p,true)` (the real error path reaches keep-last-good). | Return `(0,false)` on error (warm t0+61s); don't stamp on a warm failure (reads=3 at t0+71s); stamp `admitPriorAt` on a cold failure (cold t0+1s returns `(0,false)`, reads=1). |
| T11 | `TestOpenReplacesTheOldPeakHistoryIndexes` (store) | Real `OpenDB`; then raw SQL on the same file recreates both old indexes and drops the new one (the pre-change shape), inserts rows, closes; reopen through `OpenDB`: `sqlite_master` lists exactly `confine_peak_history_kind_signature_peak` for the table, rows intact, `ConfinePeakP90` and `ConfinePeakHistory` return the same values as before. Then close and reopen the now-migrated DB: `PRAGMA schema_version` is unchanged across that reopen (no index DDL ran). The builder first confirms on `master` that a plain reopen leaves `schema_version` unchanged. | Remove either `DROP`; remove the `CREATE`; keep the DDL-block `CREATE INDEX ... confine_peak_history_signature` (`store.go:881-882`). |
| T12 | `TestConfinePeakP90ReadsOnlyTheCoveringIndex` (store) | `EXPLAIN QUERY PLAN` of the hoisted p90 constant on a real opened DB reports `USING COVERING INDEX confine_peak_history_kind_signature_peak`. Deterministic, replaces a wall-clock test. | Restore the old `(kind, signature)` index only. |
| T13 | `TestNoAdmissibleResolutionIsLargerThanMasters` (amended) | Shapes gain `MaxCleanPeak` where they model clean rows (for example `{n=1, PeakMax 56360960}` → `MaxCleanPeak 56360960`; `{n=5, PeakMax 40 GiB, OOM 10 GiB}` → `40 GiB`); a guard fails the test on a store-inconsistent shape (`MaxCleanPeak > PeakMax`; `PeakMax > MaxOOMPeak` without `MaxCleanPeak == PeakMax`; `MaxCleanPeak > 0` with `SampleCount == 0`). Add X3 and X4's shapes. Per row, `F = GrowByEstimatorMargin(MaxCleanPeak)` from the inputs: (a) `MaxCleanPeak > 0` ⇒ `got >= F`; (b) `F <= master` (or no clean peak) and both admissible ⇒ `got <= master` (AIRA-153 I3 survives off the floor); (c) `F > master` ⇒ `got == F` with an `estimate:own-max=` basis, unless `got > ceiling` with basis `estimate:oom-escalated` (the R3 band, refused anyway); (d) the number of rows that took (c)'s equality branch is `> 0`. | Delete the floor ((a) and (c) red); key on `PeakMax` ((b) red on OOM-only shapes); an empty floor-row set ((d) red). |
| T14 | skill text test (`internal/core/skill_test.go`) | The guide names `estimate:own-max` and states that a recorded clean peak floors the reserve from the first sample. | Drop the sentence. |
| T15 | `TestOwnPeakFloorRealCgroup` (daemon, real cgroup, skips without delegation like `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission`) | RANT-55 end to end: pinned run of a workload allocating ~1.1x of 1 GiB, then the identical argv unpinned: no OOM, trailer `reserve-basis=estimate:own-max=...`. | Delete the floor (the second run is OOM-killed at 1 GiB). |
| T16 | `TestTooLargeAdviceIsDecidedByTheTermNotTheTrailingTokens` (amended, `admit_too_large_advice_test.go:233`) | New rows `estimate:own-max=2947493888,n=2,f=115` and `estimate:own-max=2947493888,n=2,f=115,oom-on-record`, both `measuredAdvicePhrase`. | Route `estimate:own-max` to the blind or p90 arm. |
| T17 | `TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped` "no basis ever carries both tokens" grid (amended) | Shapes gain store-consistent `MaxCleanPeak` plus X3's shape; new assertion: a basis starting `estimate:own-max=` never carries `,ceiling-fitted` or `,ceiling-clamped`, and carries `,oom-on-record` only when `OOMCount > 0 && MaxOOMPeak > 0`. | (f) and (e) above. |
| T18 | `TestResourcePeakHistoryReportsTheLargestCleanPeak` (store) | Real rows: clean 6979321856 + OOM 7247757312 → `MaxCleanPeak` 6979321856, `PeakMax` 7247757312; OOM with no peak + clean → `MaxOOMPeak` 0, `MaxCleanPeak` = clean; OOM-only → 0; an aitest-kind clean row under the same signature string is not counted by `ConfinePeakHistory`. | Drop `oom=0` from the CASE (returns `PeakMax`); omit the Scan target. |

Existing tests that compare a whole `PeakRSSStats` read from the store (for
example in `internal/store/confine_peak_history_test.go`,
`cmd/aira/confine_report_cli_test.go`) get the new field in their expected
literals; that is the field's arrival, not a behaviour change.

Gate: `aira confine -- make ci` from the worktree, exact exit code recorded;
`-race` on `./internal/daemon/... ./internal/store/...`.

## 8. Risks

- **R1 Label regressions in a dense function.** `resolveAdmitReserve` carries
  AIRA-149/151/153's basis rules. Mitigation: the body is moved unchanged and the
  floor sits in one wrapper; exact value and basis on every T1 cell; label
  invariants over grids (T13, T17); Fable's review.
- **R2 Fix 2 raises cold-start and 1-2-sample reserves** (§6.2, ~96k signatures)
  against AIRA-304's intent until AIRA-317 is decided. Mitigation: separate commit;
  the coordinator surfaces it before merge.
- **R3 Daemon start latency, once.** The index build (0.60-3.86 s on the copy)
  delays daemon listen-ready by that much at the first start after upgrade
  (§6.2). Clients and keepers redial without a deadline. Mitigation: cutover on a
  quiet box per the shared-daemon restart care; the builder re-measures the build
  inside the real `OpenDB` on a copy of the live DB and records it.
- **R4 The OOM advisory text** (`confine_linux.go`, daemon-reserve arm, near
  `:1780`) says a re-run is refused only where the OOM peak is at what the slice can
  give. With the floor a re-run can also be refused when an earlier clean peak
  `x1.15` exceeds the ceiling. At n<3 that needs a clean run recorded after the
  OOM run printed its advisory; at n>=3 the clean peak can predate the OOM (the
  clamp-band case, small slices only). Accepted: the skill text gains the sentence
  (§5.4); the advisory string is not changed.
- **R5 Stale prior persists** while reads keep failing. Acceptable for a slow
  machine-wide guess; bounded by the next successful read.
- **R6 Seam fixtures without `MaxCleanPeak`** silently disable the floor in a
  test. Mitigation in §6.1 (real reader in every new test; grid guards; T18).

## 9. Expected yield

- Up to 40 measured kill-after-a-clean-run signatures in retained history would
  not have been killed (an upper bound, §1); RANT-52 and RANT-55 are prevented.
- On small slices, a job whose clean peak exceeds `fit` is refused immediately
  instead of being granted `fit` and killed; one whose clean peak fits gets `F`
  instead of `fit`.
- The p90 read drops from 0.15-0.32 s to 0.03-0.07 s (sqlite3), inside the 250 ms
  budget with margin; a transient failure no longer drops the prior.
- One index fewer on every `RecordConfinePeak`.
- The over-reserve hint's "drop the pin" advice is safe at every sample count.

## 10. Explicit deferrals (not built)

- **D1 AIRA-273 option A** (escalate to `max(1.5 x MaxOOMPeak, p90)`): held by the
  owner; RANT-54 is its third instance. Fix 2 is its prerequisite, nothing more.
- **D2 Deleting the p90 rung**: AIRA-317, owner decision. This plan neither deletes
  it nor argues for it; it makes the existing rung fast and stable, and §6.2 gives
  the population the decision needs.
- **D3 (retired in revision 2)**: the floor inside the clamp at `n >= 3` is now
  built (§5.1).
- **D4 AIRA-246's total-row cap**: stays retired. The index plus keep-last-good
  cover the admission cost; the measurement goes on the ticket.
- **D5 A separate context budget for the p90 read**: measured as worth ~1 ms (§2).
- **D6 Collapsing `fallback:insufficient-samples`**: it remains reachable (OOM-only
  or unknown peaks, or `F` below the hint with no readable prior) and is honest.
- **D7 Gating the hint on sample count**: not needed (§5.3).
- **D8 Persisting the reserve basis per row** so a future measurement can count how
  often each rung acted (the gap that left §6.2's admission count unmeasured): not
  part of this problem.
- **D9 Floor for OOM-only histories**: an OOM peak is censored evidence; AIRA-153's
  rung governs (§5.1 case C).

## 11. Revision 2 change log (plan review + plan gate, both REVISE)

| Finding | Change |
|---|---|
| P1 `,oom-on-record` iff `OOMCount > 0` contradicts AIRA-149 I3's negative direction | Now iff `OOMCount > 0 && MaxOOMPeak > 0` (I6, §5.1); T1 n=1 mixed cell (OOM row with no peak beside a clean row) pins the bare basis; T6 claim corrected. |
| P2 `n < 3` bound: untestable, 2→3 step down, n>=3 clamp band below a clean peak | Bound dropped; floor at every n; D3 retired; no exported constant; T1 X4-X6 and T13 cover n>=3. |
| P2 `PeakMax > MaxOOMPeak` proxy misses a clean peak below a larger OOM peak | New `MaxCleanPeak` aggregate (§5.1 1a); X3; T18; 161 live signatures counted. |
| P2 T13 exempted rows by the returned basis (porous) | Rows classified from inputs; floor assertion `got >= F`; non-vacuity count (T13). |
| P2 mutation (c) cannot turn R3 rows red; §2 sentence wrong | Case-C cell X1; mutation (c) pointed at X1 and the existing clamped row; §2 sentence removed with the proxy. |
| P2 T10 cannot catch stamp-on-cold-failure; closed-DB proof ambiguous | Counting seam over the real reader; cold failure followed by a success in the same minute; once-per-minute claim on the read count. |
| P3 fix 2 impact understated | Population quantified in §6.2 (96,292 / 91,026). |
| P3 T11 end-state only | `schema_version` unchanged across a reopen of a migrated DB. |
| P3 R3 stale (concurrent opener) | Restated as daemon start latency; mixed-version flip-flop accepted (§6.3). |
| P3 yield counts had no reproduction | All counts added to the repro script. |
| P3 advice table missing the new basis | T16. |
