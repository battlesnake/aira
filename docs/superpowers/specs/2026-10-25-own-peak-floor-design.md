# AIRA-314 — a command's own clean peak floors its unpinned reserve

Ticket: AIRA-314 (P2 bug, milestone v0.40). Source: the 2026-10-25 rant triage
(RANT-52, RANT-55; RANT-54 is AIRA-273). Investigator report:
`~/tmp/aira-rant-verify/RANT-52_RANT-54_RANT-55.md` (re-checked against the code;
every file:line in this spec was opened on `ff03d8aa`).

Process class: **correctness-critical admission core** (it changes what
`resolveAdmitReserve` returns). Full two-loop: Opus builds TDD, Fable reviews in a
throwaway detached worktree. Daemon-side change: the shared-daemon restart care
applies at cutover.

Revision 3 (this text) answers the plan review (APPROVE) and plan gate (REVISE)
of revision 2. The main change: **fix 2 (the p90 prior's covering index and
keep-last-good cache) is cut from this build** and kept, fully designed, in
Appendix A as a follow-up gated on the owner's AIRA-317(2) answer. This build is
fix 1 only: the floor, its basis token, the skill text. Change log in §11.

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
be below a clean peak the command already reached. Only reachable on fixtures and
small real slices: no signature on the live box can reach it on the 60 GiB
`aira.slice` (table below), and ci-shim mode records no peak at all (AIRA-121 C10,
`confine_shim_linux.go:511-529`), so there is no row, no floor and no clamp band
there.

**A second, separate problem, NOT fixed here.** The machine-wide guess is itself
unreliable. Its query is a full-table `GROUP BY` over ~185k rows / ~124k
signatures and runs under the 250 ms admit deadline (`admit.go:39`). sqlite3 CLI:
0.15-0.32 s. Reported by the plan review, measured in Go through the real store
on a fresh copy of the live DB: `ConfinePeakP90` under a 250 ms context failed
**10 of 10** times. A failed read is not cached, so each admission drops to the
1 GiB default. The probe run made for this revision itself came back
`reserve-basis=fallback:insufficient-samples` (1 GiB). The p90 rung is in practice
off on this box, so the box is effectively running at the owner's AIRA-304 1 GiB
default. Making the rung reliable would raise ~96k signatures' cold-start reserves
from 1 GiB to ~1.71 GiB, which is exactly the question AIRA-317(2) (held, owner
decision) asks: keep the p90 rung or delete it. So fix 2 waits for that answer
(§3, D2, Appendix A). The floor never reads the prior, so cutting fix 2 loses
nothing for RANT-52/55.

Measured on fresh copies of the live `state.db` (2026-10-25). Every count below is
reproduced by `docs/dev/aira-314-p90-index-measurement-repro.sh` (revision 3 run,
exit 0, rows 185,624, signatures 124,231).

| Fact | Value |
|---|---|
| p90 peak / prior `p` = p90 x 1.15 | 1599184896 / 1839062630 |
| signatures with 1-2 recorded peaks | 103,961 |
| ... whose largest CLEAN peak `x1.15` exceeds 1 GiB (raised while the p90 is unreadable, i.e. today) | 12,288 |
| ... whose largest CLEAN peak `x1.15` exceeds `p` (raised even if the p90 worked) | 6,937 |
| ... of those 6,937, largest clean peak came from a `--memory-reserve` pinned run | 5,274 |
| ... of those 6,937, largest clean peak `>= 90 %` of that run's cap (§6.2 at-cap) | 258 (203 of them pinned) |
| signatures whose largest clean peak `x1.15` exceeds 60 GiB (would newly be refused on `aira.slice`) | 0 |
| signatures whose largest clean peak is `<=` their largest OOM peak (what a `PeakMax > MaxOOMPeak` test misses) | 163 |
| signatures with an OOM peak whose `x1.5` exceeds 60 GiB (can reach the AIRA-153 clamp on `aira.slice`) | 0 (largest peak recorded: 45178662912) |
| signatures where an UNPINNED run was OOM-killed with a reserve below 1.15x a clean peak recorded before it, 1-2 peaks before | 40 (an upper bound, see below) |
| ... same, 3 or more peaks before | 211 (confound gauge, see below) |
| unpinned clean rows with a recorded cap / of those at `>= 95 %` of the cap (today's ratchet, §6.2) | 65,889 / 897 |

How to read the two kill counts. A row's `at` is its COMPLETION time, and the
table stores no start time. A clean run that finished after the OOM run was
admitted (two runs of one command in parallel) still counts as "before". At 3 or
more peaks the reserve was already `>= 1.15x` every clean peak known at admission,
except through the clamp band on a small slice, a history read that timed out, or
rows older than the estimator. So most of the 211 are those confounds, and the
same confounds inflate the 40. The 40 is an upper bound on kills fix 1 would have
prevented in retained history; RANT-52 and RANT-55 are two verified sequential
instances.

## 2. Greenfield minimum

If we solved only this problem (a command's own clean peak is ignored) from
scratch: **an unpinned reserve is never below `GrowByEstimatorMargin(largest clean
recorded peak)` whenever a clean peak is recorded.** One aggregate (the largest
clean peak, one SQL term, one struct field) and one `max()` applied once after the
existing resolution. No new wire field, no new table, no schema or index change,
no new error code, no change to recording.

### The gap between that minimum and this plan, each item justified

| Item beyond the minimum | Why it is kept |
|---|---|
| A basis token naming the floor (`estimate:own-max=<MaxCleanPeak>,n=<SampleCount>,f=115`, plus `,oom-on-record` iff `OOMCount > 0 && MaxOOMPeak > 0`) | AIRA-149's rule: `reserve-basis` names the term that set the number. Without it the trailer would print `estimate:p90-prior`, `fallback:insufficient-samples` or `estimate:oom-escalated,ceiling-clamped` for a value none of them produced. `,oom-on-record` keeps AIRA-128's attribution property under exactly the OOM branch's own entry condition (`admit.go:1484`), so AIRA-149 I3's negative direction (`admit_oom_basis_test.go:256-280`) still holds. |
| Skill-text sentences (§5.3) | The generated agent guide states the "fewer than three usable" rule this changes; leaving it would teach a false threshold. |
| The committed measurement reproduction script, now with at-cap counts | Repo rule: a published measurement has a committed, executable reproduction. It reproduces every count in §1 and §6.2. Its timing half serves Appendix A and the AIRA-246/242 ticket notes; it is a docs file, not product code. |

The "aggregate" is not gap: it IS the minimum's "largest clean recorded peak". No
existing field carries it. `PeakMax > MaxOOMPeak` identifies a clean peak only
when the clean peak is the largest, and misses 163 live signatures (§1) where a
clean peak sits beside a larger OOM peak; in the clamp band those still get
`fit < 1.15 x` the clean peak (row X3). `resource_budget.go:197-204` already splits
clean and OOM peaks the same way.

**Removed in revision 3:** fix 2 (covering index, keep-last-good cache, its tests
T10-T12, risks R2/R3/R5, deferral D5). It solves a different problem whose
necessity is pending AIRA-317(2), and it raises live charging on the shared slice
against AIRA-304's direction (§1). Kept as Appendix A. Also removed: the
four-value inner function (§5.1 now uses a closure; same single application
point, no signature change).

**Removed in revision 2:** the `0 < SampleCount < 3` bound. At `n >= 3` the floor
equals the ordinary estimate or loses to it everywhere except inside the
AIRA-151/153 clamp (proof in §5.1), so dropping the bound changes behaviour only
where the bound left a reserve below a clean peak already reached, removes a
downward step from 2 samples to 3, and needs no exported
`memoryEstimateMinSamples`.

## 3. Scope

In:

- **Fix 1a** `internal/store/confine_peak_history.go: (*DB).ResourcePeakHistory`
  and `internal/runner/types.go: PeakRSSStats`: the `MaxCleanPeak` aggregate.
- **Fix 1b** `internal/daemon/admit.go: (*Server).resolveAdmitReserve`: the floor.
- **Skill text** `internal/core/skill.go:335` (the generated agent guide): name
  the new basis token and correct the thresholds (§5.3).
- **Over-reserve hint** `internal/runner/confine.go:1155-1157`: wording KEPT, with
  a test pinning the property that makes it true (§5.2, T9).
- **Ticket notes** (via `aira` note/edit, status unchanged): on AIRA-246 and
  AIRA-242, the §1 / Appendix A measurement (the figures AIRA-246's retirement
  said were missing; AIRA-242's "admission unaffected" missed the p90 read); on
  AIRA-314, that fix 2 is deferred and gated on AIRA-317(2), pointing at
  Appendix A.
- Filed by this planner (held, done): **AIRA-318**, the page-cache ratchet (§6.2).

**Fix 2 is NOT built (D2).** The ticket text mandates it; the plan gate (P1) cut
it because it pre-empts the owner's held AIRA-317(2) decision and changes live
charging. If the coordinator holds that the ticket text overrides the gate, the
question goes to the owner as one line, not into a merged PR: "AIRA-317(2): keep
the machine-wide p90 prior (then AIRA-314 fix 2 makes it reliable, ~+0.7 GiB on
~96k signatures' cold starts) or delete it?" Keep → build Appendix A as its own
change; delete → Appendix A is moot.

Out: everything in §10.

## 4. Invariants

- **I1 Floor.** For every unpinned request whose per-signature read succeeded with
  `MaxCleanPeak > 0`, the result is `>= F := runner.GrowByEstimatorMargin(MaxCleanPeak)`.
- **I2 Monotone.** For every input, the new resolution is `>=` today's: the floor
  is a `max()` over today's value. (Deliberately breaks AIRA-153's I3 "never
  grants more than master" on rows where `F >` master's value; §6.4.)
- **I3 Pinned untouched.** `request.pinned` returns `pinned:client` before any
  read (`admit.go:1399-1401`), so the floor never sees a pinned request.
- **I4 No floor without own clean evidence.** The floor is zero unless the
  per-signature read succeeded and `MaxCleanPeak > 0`. No-signature,
  history-unavailable, no-history, capture-unavailable (`SampleCount == 0`) and
  OOM-only paths are byte-identical to today, at every sample count. First-run
  (zero-sample) behaviour is unchanged.
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
- **I7 Recording unchanged; a clean row proves fit, not an uncensored peak.**
  `shouldRecordConfinePeak` (`confine_linux.go:2909`, AIRA-264) is not touched: a
  non-OOM failed run leaves no row, so a failed run never teaches the floor. A
  clean row is an exit-0, unsignalled run: it proves the job fit within its cap.
  It does NOT prove the peak is uncensored: `peak_rss` is the scope's
  `memory.peak` (`usage_linux.go:182`), which counts page cache, and the kernel
  holds `memory.current` at `memory.max` by reclaim, so a cache-heavy run records a
  peak near its cap whatever its anonymous need (§6.2, AIRA-318). Pinned clean runs
  are recorded today and feed the floor.
- **I8 Ledger conservation unchanged.** The floor changes only the requested
  reserve handed to `admitConnection`; the `reserve > ceiling` refusal, the enqueue
  re-check and the grant gate (`Σgranted <= cap - headroom`) are downstream and
  untouched.
- **I9 Restart paths unchanged.** The ARDR re-declare re-declares the GRANTED
  reserve through `enqueueReDeclare` and never calls `resolveAdmitReserve` (its
  single caller is `admitConnection`, `admit.go:1792`); resize and the
  version-frozen reconnect do not call it either. No schema, index or wire change,
  so daemon start and reload are byte-identical.

## 5. Design

### 5.1 The floor

**1a, the aggregate.** `runner.PeakRSSStats` gains `MaxCleanPeak int64`.
`ResourcePeakHistory` (`confine_peak_history.go:180-186`) adds one term and one
Scan target:

```sql
COALESCE(MAX(CASE WHEN oom=0 AND peak_rss>0 THEN peak_rss END),0)
```

No other producer of `PeakRSSStats` feeds `resolveAdmitReserve` (the only readers
are the `admitPeakHistory` seam and `db.ConfinePeakHistory`, `admit.go:1445-1448`).
The runs-table historian (`runner/peak_rss_history.go`, `aira run`) and the budget
classifier's own stats do not fill it and do not need it (D10). The struct has no
wire encoding.

**1b, the floor, one application point via a closure.** `resolveAdmitReserve`
keeps its signature. Its current body moves unchanged into a closure; the closure
assigns two captured locals right where the per-signature read succeeds
(`stats, err := read(readCtx, request.signature)`, `err == nil` arm,
`admit.go:~1452`); the floor is applied once to the closure's result:

```go
func (s *Server) resolveAdmitReserve(request admitRequest, ceiling int64) (int64, string) {
    var own runner.PeakRSSStats
    ownRead := false
    reserve, basis := func() (int64, string) {
        // ... today's body, verbatim, except one added line in the read's
        // success arm:   own, ownRead = stats, true
    }()
    if !ownRead || own.MaxCleanPeak <= 0 { // pinned returns before the read
        return reserve, basis
    }
    floor := runner.GrowByEstimatorMargin(own.MaxCleanPeak)
    if floor <= reserve { // strict: a tie keeps today's basis
        return reserve, basis
    }
    b := fmt.Sprintf("estimate:own-max=%d,n=%d,f=115", own.MaxCleanPeak, own.SampleCount)
    if own.OOMCount > 0 && own.MaxOOMPeak > 0 {
        b += ",oom-on-record"
    }
    return floor, b
}
```

The closure keeps `request` a value copy (SITE 1's fitting still mutates only the
copy) and its `defer cancel()` still runs at closure exit. Every return of today's
body returns from the closure, so no return can escape the floor. `n` is
`SampleCount`, the usable-sample count, the same meaning as in
`estimate:max=%d,n=%d,f=115`. The floor needs no context: it uses the stats the
existing read already returned.

**Where the floor can act.** Let `C` = `MaxCleanPeak`, `F` = `1.15C`.

- `n < 3`: no usable ordinary estimate exists, so today's value is a prior (`p`,
  the hint, or their fitted forms) or the OOM escalation `E = 1.5 x MaxOOMPeak`,
  possibly clamped to `fit`. Any of these can be below `F`; the floor raises them.
- `n >= 3`: the ordinary estimate is `GrowByEstimatorMargin(PeakMax)`
  (`resource_estimate.go:126-149`, `:110-122`, same arithmetic) and
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
| OOM branch, `E > ceiling`, guard `MaxOOMPeak < fit` holds, `F > fit` | `fit` (`,ceiling-clamped`) | `F`; refused if `F > ceiling` | own-max`,oom-on-record` (any n) |
| OOM branch, `E > ceiling`, guard holds, floor below the clamp value (`F <= fit`) | `fit` (`,ceiling-clamped`) | `fit`: clamp value and basis unchanged (I2; X10, X11) | `estimate:oom-escalated,ceiling-clamped` |
| OOM branch, `E > ceiling`, guard fails | `E` (refused) | `max(E, F)` (refused) | `E`'s basis unless `F > E` |
| usable ordinary estimate, no OOM branch | `>= F` | unchanged | unchanged (tie or loss) |

Why the clamp row is right, at every n: AIRA-151's rule is that an OOM record must
not make the same number behave differently. The same clean history with no OOM
on record yields at least `F`, refused when `F > ceiling`; the clamp must not
quietly grant `fit < F` because an OOM is also on record. The clamp's guard
(`MaxOOMPeak < fit`) is about the OOM peak, not the clean one, and is untouched:
the floor is applied after the clamp decision, never inside it, so the guard's
inputs are unchanged.

Case C, unchanged on purpose: an OOM-only signature (`C == 0`) whose `MaxOOMPeak`
lies in `(fit/1.15, fit)` is clamped to `fit`, which is above `MaxOOMPeak` and
below `1.15 x MaxOOMPeak`. An OOM peak is censored evidence (the job was killed at
its cap), so AIRA-153's rung stands. No clean peak, no floor.

Note on `fit < F <= ceiling`: an own-evidence value in `(fit, ceiling]` is
admissible and may wait for room; AIRA-153 already accepts that for an escalation
and an ordinary estimate landing there (`admit.go:1558-1561`).

### 5.2 The over-reserve hint (`confine.go:1138-1157`)

It fires only for a pinned, clean, exit-0 run with reserve `>= 2 GiB` and peak
`< 60 %` of it. Such a run is recorded as a clean row (I7), so after it
`MaxCleanPeak >=` this run's peak. By I1, the next unpinned run of the same argv
is sized at no less than `1.15 x` this run's peak, at every sample count, clamp
band included. That is the property the advice "drop --memory-reserve and let aira
estimate it from this command's history" needs, and it holds. Wording kept; T9
pins it.

What is NOT claimed: that the next unpinned reserve is admissible or close to this
run's peak. `MaxCleanPeak` is the maximum over the whole retained window for the
argv (up to 20 rows, any slice, any pin), so an older, larger clean peak can set a
larger `F`, over-reserving or even refused on a small slice. That is the floor
doing its job (the command did reach that peak), and the hint's text makes no
promise about the size. The basis may say `estimate:p90-prior` when the
machine-wide prior is larger than `F` (the prior acted), but the reserve is never
below this command's own history.

### 5.3 Skill text (`internal/core/skill.go:335`)

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
- on a box with history a `fallback:` basis can also appear when the machine-wide
  p90 could not be read within the admission deadline (true today on a large
  `state.db`, §1); it means the same thing, the unpinned default was used;
- the `E_ADMIT_TOO_LARGE` sentence already covers "this command's own measured
  peak-history estimate"; add that it now also applies after one or two clean runs
  whose peak `x1.15` exceeds the ceiling, and after an OOM when an earlier clean
  peak `x1.15` exceeds it (R3).

## 6. False-pass and false-fail analysis

### 6.1 False pass: a reserve that is too SMALL

| Path | Can it still return `< 1.15 x own largest clean peak`? |
|---|---|
| p90 prior, p90 fitted, insufficient-samples fallback, default fitted | No: the floor wraps every return of the body. T1, T4, T5. |
| OOM branch, escalation did not raise | No. T1 (n=2 mixed below/above), T2. |
| OOM branch, escalation raised below `F` | No. T1, T2 (RANT-52 replay). |
| OOM clamp to `fit` with `F > fit`, any n | No: `max(fit, F)`; over the ceiling it is refused, not fitted. T1 X2-X6. |
| clean peak `<=` a larger OOM peak, clamp band | No: keyed on `MaxCleanPeak`, not on `PeakMax`. T1 X3. |
| OOM row with an unknown peak beside a clean row | No: floor applies; basis has no `,oom-on-record` (OOM branch not entered). T1 n=1 mixed. |
| history read fails | Yes, by design: no stats, so no floor; basis says `fallback:history-unavailable`. Unchanged and honest. |
| OOM-only history | No floor (no clean peak, I4); escalation or AIRA-153's clamp governs (§5.1 case C). T1 OOM-only rows, X1. |
| pinned run | Untouched (I3). T1 X8, T6. |
| failed run recorded and later trusted | Cannot happen: AIRA-264's send-side gate is unchanged (`TestShouldRecordConfinePeakTracksTheClassifier`); the floor reads only clean rows. |
| hand-built seam fixture omits `MaxCleanPeak` | The floor would be silently off in that test. Mitigation: every new floor test uses the real store reader; the amended property grids (T13, T17) build their shapes from row lists through the real store, so an inconsistent shape cannot be expressed; T18 pins the reader. Existing seam tests keep `MaxCleanPeak = 0` only on OOM-only / no-clean shapes, where it is the store's true answer. |
| label lies about which term acted | Strict tie-break, suffixes dropped on a floor win, `,oom-on-record` only under the OOM branch condition. T1 asserts value AND exact basis on every cell; T13/T17 assert the label invariants over grids. |
| ceiling-fit vs clamp confusion | The floor is applied after the whole resolution, so it never feeds `fit`, the clamp or the clamp guard `MaxOOMPeak < fit`; a floor win drops both suffixes. T1 X2-X6, T17. |

### 6.2 False fail: a reserve that is too LARGE, or a new refusal

- **Zero samples:** unchanged (I4). T1 rows `n = 0`.
- **`n >= 3` outside the clamp band:** unchanged (proof §5.1). T1 rows `n = 3, 20`.
- **New `E_ADMIT_TOO_LARGE`:** (i) a command whose clean peak `x1.15` exceeds the
  ceiling after one or two samples, which today got a 1 GiB or ~1.71 GiB guess and
  would almost certainly have been OOM-killed; (ii) at any n with an OOM on record,
  a clean peak above `fit` where today the clamp granted `fit` below that clean
  peak. Both now refused immediately, naming both numbers. This is AIRA-153's rule
  for ordinary estimates. On the live data the count above 60 GiB is 0 and no
  signature can reach the clamp band on `aira.slice` (§1); ci-shim mode records no
  peak. So new refusals are confined to **fixtures and small real slices**. T1 X6,
  T8.
- **At-cap peaks (page cache), accepted explicitly.** `peak_rss` is `memory.peak`,
  which counts page cache, and the kernel holds `memory.current` at `memory.max`,
  so a cache-heavy run records a peak near its cap however small its anonymous
  need (I7). Today's `n >= 3` estimate already turns such a peak into
  `1.15 x cap` for the next run, whose peak again lands at its new cap: a 15 %
  per-run ratchet toward `E_ADMIT_TOO_LARGE`. The floor does not create this
  ratchet; it brings it in **two runs earlier** (from `n = 1`), starting from
  whatever cap the first run had, often a pin. Example: pinned
  `--memory-reserve 16G`, cache-heavy, real need 2G, records peak 16G. Today run 2
  gets ~1.71 GiB (or 1 GiB) and runs, the cache reclaims; with the floor run 2 gets
  18.4G, run 3 21.2G, and so on; today the same climb starts at run 4.
  Population (§1): 258 of the 6,937 signatures the floor raises above `p` have
  their largest clean peak at `>= 90 %` of that run's cap (203 from pins); 897 of
  65,889 unpinned clean rows already sit at `>= 95 %` of their cap (today's ratchet
  at `n >= 3`). Accepted: the floor's direction (over-reserve) is the safe one; the
  same at-cap evidence already feeds `estimate:max` at `n >= 3`; distinguishing
  cache from anonymous memory is a separate estimator-input design, filed held as
  **AIRA-318** and out of scope here. Pinned runs are unaffected (I3), so an agent
  that sees the climb can pin.
- **`fit < F <= ceiling`:** admissible, may wait where `fit` would have been
  granted sooner (§5.1 note). Accepted, as AIRA-153 accepts it for own evidence.
- **Bimodal command:** one big pinned run makes the next cheap unpinned run
  reserve the big figure for up to 20 retained rows. Over-reservation is the safe
  direction and is what `estimate:max` does from three samples. Accepted.
- **Ledger:** unchanged (I8). A larger request is a larger booking, still under
  `Σgranted <= cap - headroom`.
- **Daemon start, reload, re-declare:** unchanged (I9); no schema change.

### 6.3 Restart and reload

Re-declare (I9): unchanged; it carries the granted figure and never re-resolves.
Resize (AIRA-312) changes a granted lease, never re-resolves. No schema, index or
wire change, so a mixed-version restart has nothing to flip-flop. (Appendix A's
index change would; its own section covers it.)

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

**Isolation (T1, and every test that writes three or more peaks under a
signature):** each cell uses a **fresh store and a fresh server** (fresh p90
cache), with the ten prior seeds re-written per cell. `ConfinePeakP90` ranks
`MAX(peak_rss)` over every confine signature with `>= 3` peaks, so a shared DB
would let the `n = 3/20` and X4-X6 rows move the p90 off 1610612736 and make
results depend on cell order; recomputing the literal to match would make the
test porous.

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
| X9 `F > E > ceiling`, guard fails | ceiling 1610612736 (`fit` 1400532813); n=2: OOM 1778917376, clean 2947493888 | 2668376064 `estimate:oom-escalated` (refused) | 3389617971 `estimate:own-max=2947493888,n=2,f=115,oom-on-record` (refused, T8) |
| X10 floor below the clamp value | n=2: clean 1073741824, OOM 6442450944 | 7469508340 `estimate:oom-escalated,ceiling-clamped` | unchanged |
| X11 floor below the clamp value | n=3: clean 1073741824 x2, OOM 6442450944 | 7469508340 `estimate:oom-escalated,ceiling-clamped` | unchanged |

Mutations that must each turn T1 red:
(a) delete the floor (n=1/2 above and over-ceiling clean and mixed cells, X2-X6);
(b) reintroduce `SampleCount < 3` (X4, X5, X6);
(c) key the floor on `PeakMax` instead of `MaxCleanPeak` (X1, and OOM-only below
at n=1/2, whose `F` beats `H`);
(c') key it on `PeakMax > MaxOOMPeak` (X3);
(d) non-strict `>=` tie-break (X7);
(e) append `,oom-on-record` iff `OOMCount > 0` alone (n=1 mixed above and over-ceiling);
(f) keep `,ceiling-clamped` / `,ceiling-fitted` on a floor win (X2-X5);
(g) fit the floor to `fit` or apply it before the clamp (X2-X6 values);
(h) move the floor above the pinned return (X8).
(i) skip the floor where the unclamped escalation is over the ceiling,
`if floor <= reserve || (basis == "estimate:oom-escalated" && reserve > ceiling)` (X9).
(j) let the floor replace any clamped value, shrinking it below today's `fit`,
`if floor <= reserve && !strings.Contains(basis, ",ceiling-clamped")` (X10, X11).

### Other tests

| # | Test (package) | What it asserts | Mutation that must turn it red |
|---|---|---|---|
| T2 | `TestOwnPeakFloorBeatsAnEscalationBelowTheCleanPeak` (daemon) | RANT-52 replay, rows in time order: OOM 1778917376 then clean 2947493888 → 3389617971, `estimate:own-max=2947493888,n=2,f=115,oom-on-record` (today `E` 2668376064); and OOM 524288000 + clean 1288490188 (escalation does not raise `H`) → 1481763716, same basis shape. | Apply the floor only outside the OOM branch. |
| T4 | `TestOwnPeakFloorReplacesAFittedPrior` (daemon) | Ceiling 1610612736 with the prior → today `fit` 1400532813 `estimate:p90-prior,ceiling-fitted`; clean 1288490188 at n=1 → 1481763716 `estimate:own-max=1288490188,n=1,f=115`. SITE-1 fitted default: ceiling 939524096, NO prior signatures, clean 734003200 at n=1 → today 816977474 `fallback:insufficient-samples,ceiling-fitted`, new 844103680 `estimate:own-max=734003200,n=1,f=115`. | Keep the fitted suffix on a floor win. |
| T5 | `TestOwnPeakFloorWithNoReadablePrior` (daemon) | RANT-55 replay, no prior signatures, rows in time order: clean 1099382784 at n=1 → 1264290201 `estimate:own-max=1099382784,n=1,f=115`, not `H` (today: `H`, `fallback:insufficient-samples`, the kill). Control row n=2 (then the OOM at 1073741824): `E` 1610612736 > `F`, so result and basis stay `estimate:oom-escalated`. | Delete the floor on the post-block fallback path only; or relabel when `F` loses. |
| T6 | existing `TestTheAcceptedR3BandReturnsMoreThanMasterAndIsRefused`, `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes`, `TestPostBlockInsufficientSamplesFallbackIsUnchanged`, `TestFittingNeverTouchesAPinnedRequest` | Stay green unmodified. Their seam fixtures are OOM-only or no-clean shapes, where `MaxCleanPeak = 0` is the store's true answer, so they pin today's behaviour where the floor must not act. The OOM-basis negative direction holds with the floor active because `,oom-on-record` follows the OOM branch's entry condition. | (e) above would break the negative direction once a clean peak is present; that case is T1's n=1 mixed cell. |
| T8 | `TestOwnPeakFloorOverTheCeilingIsRefusedWithTheOwnHistoryAdvice` (daemon, wire) | Through `admitConnection` over `net.Pipe` with the real store, slice maximum chosen so `cap - headroom` = 8 GiB: (i) clean 8053063680 at n=1 → `E_ADMIT_TOO_LARGE`, `required=9261023232`, basis `estimate:own-max=8053063680,n=1,f=115`, the "OWN measured peak history" advice, no queue created, ledger unchanged; (ii) X6's history → `required=8643621683` with the `,oom-on-record` basis. (iii) X9's history at ceiling 1610612736 → `required=3389617971`, basis `estimate:own-max=2947493888,n=2,f=115,oom-on-record`, the own-history advice (not the OOM-escalated arm). | Fit the floor to `fit`. |
| T9 | `TestHintEligibleRunFloorsTheNextUnpinnedReserve` (daemon, real producer) | Record a pinned clean run through the real `confineReport` handler (`peak_rss` 1181116006, budget 3 GiB, `cap:operator:--memory-reserve`), then resolve the same signature unpinned (ceiling 8 GiB), with and without a readable prior: result `>=` 1358283406 (`1.15 x` peak) after one, two and three such runs. Second sub-case, an OLDER LARGER clean row: first an unpinned clean run at 2947493888 through the same handler, then the hint-eligible pinned run → result 3389617971, basis `estimate:own-max=2947493888,n=2,f=115`, asserted `>=` 1.15 x the older row and `>=` 1358283406. Pairs with the existing runner test that the hint fires for that status. | Delete the floor; key the floor on the newest row instead of the maximum (second sub-case). |
| T13 | `TestNoAdmissibleResolutionIsLargerThanMasters` (amended) | Shapes are built from **row lists through the real store**: a helper `storeShape(t, rows)` writes the rows to a `t.TempDir()` DB with `RecordConfinePeak` and returns `ConfinePeakHistory`; the grid then runs over `staticHistory(thatStats)` as today (one store write per shape, not per cell). Row lists for every current store-expressible shape (for example `{clean 56360960}`; `{clean 40 GiB x4, OOM 10 GiB}`; `{OOM 40 GiB, clean 40 GiB x3}`), plus X3, X4 and X9 (with ceiling 1610612736 in the grid). The deliberately malformed seam shapes (`{5,5,PeakMax 0}`, `{5,5,PeakMax 0,OOM 10 GiB}`) move to a named `exemptMalformedShapes` list, kept on the seam with `MaxCleanPeak = 0`, and are excluded from (a)-(d). Per row, `F = GrowByEstimatorMargin(MaxCleanPeak)` from the inputs: (a) `MaxCleanPeak > 0` ⇒ `got >= F`; (b) `F <= master` (or no clean peak) and both admissible ⇒ `got <= master` (AIRA-153 I3 survives off the floor); (c) `F > master` ⇒ `got == F` with an `estimate:own-max=` basis, unless the row is in the R3 band, decided from the INPUTS: `OOMCount > 0 && MaxOOMPeak > 0`, `E = MaxOOMPeak + MaxOOMPeak/2 > ceiling`, `E >= F` and `got == E` (refused anyway), where the basis must then be `estimate:oom-escalated`; (d) the number of rows that took (c)'s equality branch is `> 0`. | Delete the floor ((a) and (c) red); key on `PeakMax` ((b) red on OOM-only shapes); an empty floor-row set ((d) red); hand-set `MaxCleanPeak = 0` in the helper (store-built shapes then disagree with the reader: (a) and (d) red). |
| T14 | skill text test (`internal/core/skill_test.go`) | The guide names `estimate:own-max` and states that a recorded clean peak floors the reserve from the first sample. | Drop the sentence. |
| T15 | `TestOwnPeakFloorRealCgroup` (daemon, real cgroup, skips without delegation like `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission`) | RANT-55 end to end: pinned run of a workload allocating ~1.1x of 1 GiB (anonymous memory, not page cache), then the identical argv unpinned: no OOM, trailer `reserve-basis=estimate:own-max=...`. | Delete the floor (the second run is OOM-killed at 1 GiB). |
| T16 | `TestTooLargeAdviceIsDecidedByTheTermNotTheTrailingTokens` (amended, `admit_too_large_advice_test.go:233`) | New rows `estimate:own-max=2947493888,n=2,f=115` and `estimate:own-max=2947493888,n=2,f=115,oom-on-record`, both `measuredAdvicePhrase`. | Route `estimate:own-max` to the blind or p90 arm. |
| T17 | `TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped` "no basis ever carries both tokens" grid (amended) | Shapes built through `storeShape` exactly as T13 (same exempt list), plus X3's shape; new assertion: a basis starting `estimate:own-max=` never carries `,ceiling-fitted` or `,ceiling-clamped`, and carries `,oom-on-record` only when `OOMCount > 0 && MaxOOMPeak > 0`. | (f) and (e) above. |
| T18 | `TestResourcePeakHistoryReportsTheLargestCleanPeak` (store) | Real rows: clean 6979321856 + OOM 7247757312 → `MaxCleanPeak` 6979321856, `PeakMax` 7247757312; OOM with no peak + clean → `MaxOOMPeak` 0, `MaxCleanPeak` = clean; OOM-only → 0; an aitest-kind clean row under the same signature string is not counted by `ConfinePeakHistory`; after 21 clean writes the evicted oldest (largest) row no longer counts (retention window). | Drop `oom=0` from the CASE (returns `PeakMax`); omit the Scan target. |

(T3, T7, T10-T12 are retired numbers: T3/T7 folded into T1/T6 in revision 2;
T10-T12 belong to fix 2 and live in Appendix A.)

Existing tests that compare a whole `PeakRSSStats` read from the store (for
example in `internal/store/confine_peak_history_test.go`,
`cmd/aira/confine_report_cli_test.go`) get the new field in their expected
literals; that is the field's arrival, not a behaviour change.

Gate: `aira confine -- make ci` from the worktree, exact exit code recorded;
`-race` on `./internal/daemon/... ./internal/store/...`.

## 8. Risks

- **R1 Label regressions in a dense function.** `resolveAdmitReserve` carries
  AIRA-149/151/153's basis rules. Mitigation: the body is moved verbatim into a
  closure with one added assignment, and the floor sits after it; exact value and
  basis on every T1 cell; label invariants over store-built grids (T13, T17);
  Fable's review.
- **R2 Page-cache ratchet two runs earlier** (§6.2): 258 raised signatures at
  `>= 90 %` of cap. Accepted; AIRA-318 holds the estimator-input question.
- **R3 The OOM advisory text** (`confine_linux.go`, daemon-reserve arm, near
  `:1780`) says a re-run is refused only where the OOM peak is at what the slice
  can give. With the floor a re-run can also be refused when an earlier clean peak
  `x1.15` exceeds the ceiling. At n<3 that needs a clean run recorded after the
  OOM run printed its advisory; at n>=3 the clean peak can predate the OOM (the
  clamp-band case, fixtures and small slices only). Accepted: the skill text gains
  the sentence (§5.3); the advisory string is not changed.
- **R4 Seam fixtures without `MaxCleanPeak`** silently disable the floor in a
  test. Mitigation in §6.1 (real reader in every new test; store-built grid
  shapes with a named exempt list; T18).

## 9. Expected yield

- Up to 40 measured kill-after-a-clean-run signatures in retained history would
  not have been killed (an upper bound, §1); RANT-52 and RANT-55 are prevented.
  12,288 signatures with 1-2 peaks get a reserve sized from their own clean peak
  instead of the 1 GiB the box actually hands out today.
- On fixtures and small slices, a job whose clean peak exceeds `fit` is refused
  immediately instead of being granted `fit` and killed; one whose clean peak fits
  gets `F` instead of `fit`.
- The over-reserve hint's "drop the pin" advice is safe at every sample count
  (never sized below `1.15 x` the peak the hint quoted).

## 10. Explicit deferrals (not built)

- **D1 AIRA-273 option A** (escalate to `max(1.5 x MaxOOMPeak, p90)`): held by the
  owner; RANT-54 is its third instance.
- **D2 Fix 2 (covering index + keep-last-good p90 cache), gated on AIRA-317(2).**
  Deleting the p90 rung is the owner's AIRA-317(2) decision; fix 2 makes the rung
  reliable, which turns it on in practice (~96k signatures' cold starts from 1 GiB
  to ~1.71 GiB on a slice that runs near full). This build neither deletes the
  rung nor makes it reliable. Keep → build Appendix A as its own change (its
  design, measurements and gate amendments are recorded there); delete → moot. A
  separate p90 context budget (old D5) goes with it.
- **D4 AIRA-246's total-row cap**: stays retired (Appendix A covers the admission
  cost if the rung is kept); the measurement goes on the ticket.
- **D6 Collapsing `fallback:insufficient-samples`**: it remains reachable (OOM-only
  or unknown peaks, or `F` below the hint with no readable prior) and is honest.
- **D7 Gating the hint on sample count**: not needed (§5.2).
- **D8 Persisting the reserve basis per row** so a future measurement can count how
  often each rung acted (the gap that left Appendix A's admission count
  unmeasured): not part of this problem.
- **D9 Floor for OOM-only histories**: an OOM peak is censored evidence; AIRA-153's
  rung governs (§5.1 case C).
- **D10 The `aira run` estimator** (`internal/core/resource_estimate.go:14-45`,
  `runner.EstimateMemoryReserve`, `memoryEstimateMinSamples = 3` over the runs
  table): keeps its 1-2-sample gap. Its result is sent pinned, so this floor never
  reaches it. The ticket title covers it in words, but the rants are all
  `aira confine`; a run-side floor is a separate small change if wanted.
- **D11 Estimating from anonymous memory instead of `memory.peak`**: AIRA-318
  (held). (D3 and D5 are retired numbers.)

## 11. Change log

### Build review round 2 fix

| Finding | Change |
|---|---|
| P2 porous: I2 (never below today) untested in the clamp band when `F < fit`; every clamp cell had `F > fit` and T13 (b) bounds only from above | Cells X10 (n=2) and X11 (n=3) added to T1 (clean 1073741824, OOM 6442450944, ceiling 8 GiB: `fit` with `,ceiling-clamped`); clamp row of the §5.1 outcome table split; mutation (j) added. Mutation (j) red in T1 X10 and X11 (reserve 1234803097, own-max basis); green at the fix. |

### Build review round 1 fix

| Finding | Change |
|---|---|
| P2 porous: outcome row "OOM branch, `E > ceiling`, guard fails -> `max(E, F)`" untested; T13's R3 exemption keyed on the returned basis | Cell X9 added to T1 and T8; X9's shape and ceiling 1610612736 added to T13; T13's R3 exemption derived from the inputs, with the basis asserted inside the band. Mutation (i) red in T1 X9, T8 X9 and T13 (a); green at the fix. |

### Revision 3 (plan review APPROVE, plan gate REVISE)

| Finding | Change |
|---|---|
| Gate P1: fix 2 pre-empts held AIRA-317(2) and raises live charging | Fix 2 cut (2a, 2b, T10-T12, R2/R3/R5 old, D5); kept as Appendix A, gated (§3, D2). Coordinator one-line owner question recorded in §3. |
| P2 I7 "uncensored" false at the cap (page cache) | I7 restated (fit, not uncensored); §6.2 at-cap population (258/6,937, 203 pinned; 897/65,889 today) and the two-run acceleration accepted; AIRA-318 filed held; at-cap counts in the repro script; R2. |
| P2 T13/T17 guard incomplete | Shapes built from row lists through the real store (`storeShape`); malformed seam shapes in `exemptMalformedShapes`; helper mutation added. |
| P3 T1 shared DB moves the p90 | Fresh store and server per cell, seeds re-written (§7 Isolation). |
| P3 §5.3 "always admissible" false | Narrowed to "never below 1.15 x this run's peak" (§5.2); T9 gains an older larger clean row. |
| P3 shim reports no peak | Scope sentence corrected in §1, §6.2: fixtures and small real slices. |
| P3 `aira run` estimator unstated | D10. |
| P3 T10 missing success-(0,false)-then-failure; retry wording; `aira init` opener; Go numbers | Recorded in Appendix A (fix 2 is deferred). Go numbers also in §1. |
| P3 simplify: four-value inner function | Closure with captured locals (§5.1). |

### Revision 2 (plan review + plan gate of revision 1, both REVISE)

`,oom-on-record` iff `OOMCount > 0 && MaxOOMPeak > 0`; the `n < 3` bound
dropped; `MaxCleanPeak` aggregate replaced the `PeakMax > MaxOOMPeak` proxy; T13
classifies rows from inputs with a non-vacuity count; case-C cell X1; T10 counting
seam; T11 schema_version; R3 restated; yield counts reproduced; T16.

## Appendix A — deferred fix 2 (build only if AIRA-317(2) answers "keep")

Not part of this build. Recorded so the follow-up starts from measured facts.

**A.1 Problem.** The p90 read (`store.ConfinePeakP90`, called through
`cachedAdmitPeakP90`, `admit.go:1630-1654`) shares the 250 ms admit context and
times out on the live DB; a failure is not cached.

**A.2 Measurements.** sqlite3 CLI, repro script, revision 3 run: before 0.182 /
0.180 / 0.272 s (plan `USING INDEX confine_peak_history_kind_signature`); covering
index build 0.586 s; after 0.061 / 0.050 / 0.045 s (plan `USING COVERING INDEX
confine_peak_history_kind_signature_peak`); per-signature history read ~1 ms both.
Earlier runs: before 0.15-0.32 s, after 0.03-0.07 s, build 0.60-3.86 s. Reported
by the plan review, Go through the real store on a fresh copy of the live DB
(185,576 rows): before, `ConfinePeakP90` under 250 ms failed 10 of 10; after,
76-101 ms (10 of 10 ok), about 35-40 % of the shared budget; the real `OpenDB` on
an old-index copy built the covering index in 1.69 s and ended with exactly
`[confine_peak_history_kind_signature_peak]`, rows intact, `schema_version` 1 → 4
on that open and stable at 4 on reopen (master's plain reopen also leaves it
unchanged). The plan gate re-verified the OpenDB rebuild. The follow-up builder
re-measures these through a committed reproduction before publishing them.

**A.3 Population it moves.** Repro script: 96,354 signatures with 1-2 peaks, no
OOM row and `F < p` would get `p` reliably instead of `p`-or-1-GiB; for 91,080 of
them `F < 1 GiB`, so each admission where today's read fails rises from 1 GiB to
~1.71 GiB. Plus every genuine cold start. How many admissions that is today is not
measured (the basis is not persisted, D8).

**A.4 Design (2a).** In `ensureConfinePeakHistorySubject` (`store.go:1312-1327`):
`CREATE INDEX IF NOT EXISTS confine_peak_history_kind_signature_peak ON
confine_peak_history(kind, signature, peak_rss)`, then `DROP INDEX IF EXISTS` for
`confine_peak_history_kind_signature` and `confine_peak_history_signature`
(create before drop). Delete the DDL-block `CREATE INDEX IF NOT EXISTS
confine_peak_history_signature` (`store.go:881-882`). The `(kind, signature)`
prefix still serves every per-subject reader and the retention `DELETE`; the
signature-only index has no reader. Hoist the p90 SQL into a constant for the plan
test. No migration: `initDB` runs only in `OpenDB` (`store.go:547`).

**A.5 Design (2b).** `cachedAdmitPeakP90` on `err != nil`: under `admitPriorMu`,
if `!admitPriorAt.IsZero()` return the cached `(admitPriorPeak, admitPriorOK)` and
set `admitPriorAt = now`; else return `(0, false)` and stamp nothing. Invariant: a
failed refresh serves the last SUCCESSFUL `(peak, ok)` pair (possibly
`(0, false)`) and never invents one. Claim, worded per the plan review: once a
failure has been stamped, no further read until the minute passes; callers that
arrive together just after expiry may each run an overlapping read (today's
behaviour on success refresh too, bounded by `admitGlobalMax`); no single-flight
machinery. A separate p90 context budget is a one-line option the follow-up may
take given the 76-101 ms Go figure.

**A.6 Tests.** T10 `TestCachedP90ServesLastGoodAndRetriesAColdFailure`: counting
seam over the REAL `db.ConfinePeakP90`, switchable to fail; warm, cold and
real-store-closed-DB sequences as designed in revision 2, PLUS the sequence
success `(0,false)` at t0 → failing at t0+61s → failing at t0+71s, asserting
reads=2 (catches stale-serve keyed on `admitPriorOK` instead of `admitPriorAt`).
T11 `TestOpenReplacesTheOldPeakHistoryIndexes`: real `OpenDB` on a DB recreated in
the old shape; exactly the covering index after; rows intact; readers' values
unchanged; `schema_version` unchanged across a reopen of the migrated DB. T12
`TestConfinePeakP90ReadsOnlyTheCoveringIndex`: `EXPLAIN QUERY PLAN` of the hoisted
constant.

**A.7 Risks.** Daemon listen-ready delayed once by the build (0.6-3.9 s; 1.69 s
in Go) inside `OpenDB` before `net.Listen` (`server.go:446`); clients redial with
no deadline (`admission_linux.go:39-44`). **Second opener:** `aira init`
(`internal/app/project.go:344`) opens the machine-wide `state.db` through
`store.OpenDB` beside a running daemon; a new-binary `aira init` under an old
daemon runs the build and drops under the live daemon, holding the write lock for
~1.7-3.9 s, so a confine-report in that window may fail its 250 ms context and the
job prints "could not confirm recorded" (honest). An old daemon's restart then
recreates the old indexes (flip-flop). Accepted under no-compat; named so the
follow-up does not rediscover it. Stale prior persists while reads keep failing,
bounded by the next success.
