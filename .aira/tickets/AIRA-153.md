---
{"schema":1,"id":"AIRA-153","project":"aira","title":"The unpinned 4 GiB confine reserve default is unconditioned by the slice ceiling","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-153","to":"AIRA-150"},{"kind":"relates","from":"AIRA-153","to":"AIRA-151"},{"kind":"relates","from":"AIRA-153","to":"AIRA-152"},{"kind":"relates","from":"AIRA-153","to":"AIRA-162"},{"kind":"relates","from":"AIRA-153","to":"AIRA-163"},{"kind":"relates","from":"AIRA-153","to":"AIRA-164"},{"kind":"relates","from":"AIRA-153","to":"AIRA-165"},{"kind":"relates","from":"AIRA-153","to":"AIRA-166"},{"kind":"relates","from":"AIRA-153","to":"AIRA-167"},{"kind":"relates","from":"AIRA-153","to":"AIRA-168"}]}
---

AIRA-149 deferral **F4**, and the actual reason AIRA-150 is a certainty on a
small slice and near-unreachable on the production one.

`runner.ResolveConfineReserve` leaves an unpinned request at
`DefaultConfineMemoryReserve` (4 GiB) regardless of how large the slice it is
about to be admitted into actually is. On the 64 GiB production `aira.slice` the
default is well under the ceiling and nothing is visible; on any small slice — a
CI slice, a shim-configured ceiling, a fixture — the blind default alone exceeds
the ceiling, so EVERY unpinned request resolves to the ceiling (with an OOM
record) or is refused terminally (without one).

Note the asymmetry that frames the question: the no-OOM path ALREADY refuses
such a request terminally with `E_ADMIT_TOO_LARGE`. So the design question is
not "should the default be clamped" but "what should a small slice do with a
client that has no idea how big it is".

Sizing decision on the machine-wide gate: full two-loop.

## Sharpened by AIRA-151, unanswered (2026-09-07)

AIRA-151 shipped and removed the last accidental mitigation of this defect
(plan §3.7 / G2). `ResolveConfineReserve` still hands the daemon exactly
`DefaultConfineMemoryReserve` for every unpinned request regardless of how large
the slice is, so on any slice whose ceiling is below 4 GiB that value is over the
ceiling. What changed is only the consequence:

- **before AIRA-151:** a signature with **no** OOM record was refused terminally
  (`E_ADMIT_TOO_LARGE`), while a signature **with** one was clamped onto the
  ceiling and then usually wedged until the wait expired — two different
  outcomes for one defect, and the OOM record made the outcome worse;
- **after AIRA-151:** both are refused terminally, immediately, with `required`
  and `cap_minus_headroom`. The defect is unchanged; its consequence is now
  uniform, certain and immediate rather than sometimes-wedged.

The design question is unchanged and unanswered: **what should a small slice do
with a client that has no idea how big it is.** The candidate answers —
condition the default on the ceiling, make the daemon publish the admissible
size, refuse at install time — all remain open and all remain sizing decisions
needing their own two-loop.

One consequence AIRA-151 accepted and recorded rather than fixed, which belongs
to this ticket: on a slice whose ceiling is below the 4 GiB default, AIRA-128's
cold-start self-heal ("re-run the identical command" after `terminated-by=oom`)
no longer works for that command without a pinned reserve. AIRA-151's generated
agent-guide clause says so; the underlying fault is this unconditioned default.

## Resolution (2026-09-08)

**Built as planned**, with two named deviations recorded below. Plan:
`docs/superpowers/plans/2026-09-08-aira153-condition-unpinned-default-on-ceiling-plan.md`
(revision 2, plan-gate approved). Full two-loop, not the light path: this changes
SIZING at two call sites on the machine-wide admission gate every `aira confine`
job on this box passes through.

### The answer to the design question

**"What should a small slice do with a client that has no idea how big it is?"**
— bound the client's GUESS by an established fact about the slice, and never
bound a measurement or an operator's own number that way.

One quantity does it, `runner.SliceFittedReserve(ceiling)` in
`internal/runner/resource_estimate.go`:

    FIT(c) = floor(100c/115)

the INVERSE of the estimator's own `memoryEstimateSafetyPct` growth margin — the
largest figure that, grown the way every history-derived estimate here is, still
fits inside `c`. So no new constant enters the codebase. It is strictly below the
ceiling for every `c >= 1` (which is what keeps an auto-sized value off AIRA-150's
ungrantable equality), it cannot degenerate when the configurable headroom terms
are set to zero, and it returns 0 below `runner.MinPinnedScopeCap` so a
degenerately small slice still refuses terminally rather than being handed a
sub-megabyte `memory.max` that would instant-OOM a job on placement.

Why a fix was warranted at all, rather than "use `--memory-reserve`": `aira
install` with no `--memory-max` sizes the slice at
`MemTotal - min(MemTotal/4, 16 GiB)`, floored to whole GiB. **On an ordinary
8 GiB box that is a 6 GiB slice, whose entry ceiling is 4227858432 — below the
4 GiB unpinned default — so every unpinned `aira confine -- <cmd>` on a DEFAULT
install was refused `E_ADMIT_TOO_LARGE` at request entry**, while `SKILL.md`
mandates exactly that unpinned form for every heavy command.

### Three sites, one quantity

All in `resolveAdmitReserve` (`internal/daemon/admit.go`); no other function in
the file changed.

1. **The client's unpinned prior**, GATED on `request.reserve >= ceiling`, so
   only a value that is REFUSED today (`>`) or that lands exactly on the
   ungrantable ceiling (`==`, AIRA-150 route 3) is touched. Applied BEFORE
   anything reads `request.reserve`, which is load-bearing in two directions: all
   six sites that return the hint verbatim inherit it, and the OOM escalation's
   `escalated > reserve` comparison sees the fitted value as its FLOOR, so the
   escalation can still raise the reserve to whatever this command's own OOM
   evidence justifies. `request` is a value copy, so the assignment cannot escape.
2. **The machine-wide `estimate:p90-prior`**, under the same gate. Without it
   AIRA-128's cold start is still terminally refused on any slice whose ceiling is
   below the box's p90 — the ordinary shape of a small ci-shim budget beside a
   large `aira.slice`, since one machine-wide `state.db` serves both.
3. **AIRA-151's OOM-escalation clamp**, whose TARGET and GUARD both move from
   `ceiling` to `fit`. Everything else about it is byte-for-byte AIRA-151's: the
   nesting inside `escalated > reserve`, the strict tie-break, one condition
   governing both the value and the basis, the `,ceiling-clamped` spelling, and
   the AIRA-103 static ceiling.

### Why the clamp could NOT be deferred (the derivation the plan gate forced)

A job admitted at a fitted `memory.max` of `FIT(c)` and OOM-killed there records
`MaxOOMPeak ~= FIT(c)`, because the peak comes from the scope's own `memory.peak`
and the store keeps `MAX(peak_rss) WHERE oom=1`. `FIT(c)/c = 0.8696` lies inside
AIRA-151's clamp band `(2/3, 1)` **by construction**. So with the guard left at
`ceiling`, the next admission would escalate to `1.304c`, clamp to exactly `c`,
be admissible but grantable only inside AIRA-150's residual band (byte-exact zero
on a slice entered empty), wait out the default 30-minute window, and be refused
`E_ADMIT_SATURATED` — which the agent guide documents as "owed a RETRY, nothing
about the request is wrong". Because the job never runs, no new peak is recorded:
the state is PERMANENT, not a rung. Shipping the fit alone would have converted
master's honest immediate refusal into precisely the wedge AIRA-151 shipped to
remove, on the population this ticket exists to serve.
`TestAnOOMAtTheFittedCapIsRefusedImmediatelyAndNeverEnqueues` and mutation M8 are
the executable form of that derivation.

### What is NOT fitted, and why

- **An ORDINARY per-signature estimate** (`estimate:max=…`, `estimate:capped`) —
  this command's own MEASURED evidence. Over the ceiling it is still refused
  terminally, naming both numbers. *A guess may be bounded by an established
  fact; a measurement may not be silently reduced to fit one.*
- **A PINNED reserve** (`--memory-reserve`, `--memory-max`, `--delegate-ram`) —
  the operator's own number, returned at the function's first line as
  `pinned:client` before either site exists.
- **Any prior already below the ceiling** — the gate. On the production 64 GiB
  slice the fit is unreachable at any plausible occupancy (it would need ~918
  outstanding jobs, and the headroom term self-limits long before that), so
  **production sizing for the fit is a strict no-op by construction, not by
  measurement.**

### The basis, and the accepted production cost

`,ceiling-fitted` is a new token in the existing `family:name[:params]` grammar.
It is carried by PROVENANCE, never by comparing numbers: it lives in a `suffix`
local that is cleared the moment `estimateUsable` is true and is unreachable from
the escalation branch, which rebuilds its basis from scratch. So it and
`,ceiling-clamped` are mutually exclusive by construction, and neither can attach
to an estimate.

The CLAMP's production effect is real and is accepted (64 GiB `aira.slice`, entry
ceiling 66504884224, `FIT` 57830334107):

| recorded `MaxOOMPeak` | before | after |
| --- | --- | --- |
| < 44336589484 | no clamp | unchanged |
| 44336589484 .. 57830334106 | clamped to 66504884224 | clamped to **57830334107**, and grantable |
| 57830334107 .. 66504884223 | clamped to 66504884224 | **no clamp — terminal `E_ADMIT_TOO_LARGE`** |
| >= 66504884224 | no clamp | unchanged |

Accepted on AIRA-151's own rationale: a wait that ends in a refusal is worse than
an honest refusal, and a job whose last kill was at 54-62 GiB against a 62 GiB
ceiling is genuinely too large for that slice. Recorded on AIRA-151 as well.

### Two deviations from the plan, named rather than silent

1. **T4's monotonicity claim is NOT asserted, because it is false against
   revision 2's own design.** The plan specifies T4 as "never returns a LARGER
   value for a SMALLER hint". That was true of revision 1's UNGATED fit
   (`min(hint, FIT(ceiling))`); revision 2's `>= ceiling` gate (the plan-gate's
   own F3) introduces a deliberate discontinuity there — at ceiling 1031798784 a
   hint of 1031798783 resolves to 1031798783 while a hint of 1031798784 resolves
   to 897216333. Asserting it would have been a fabricated pin.
2. **T4's "never returns more than master would for the same inputs" is
   asserted only for ADMISSIBLE outcomes**, because it is likewise false in
   exactly the band R3 accepts: where `FIT(ceiling) <= MaxOOMPeak < ceiling` and
   the escalation exceeds the ceiling, master CLAMPED to the ceiling and this
   change does not clamp at all, so the returned number is LARGER — and is then
   refused terminally instead of admitted onto an ungrantable ceiling. I3's own
   words are about what is GRANTED, so that is what
   `TestNoAdmissibleResolutionIsLargerThanMasters` pins, over a grid of 11 stats
   shapes x 15 ceilings x 8 hints, against an inline transcription of master's
   shipped formula. The R3 band is then driven EXPLICITLY by
   `TestTheAcceptedR3BandReturnsMoreThanMasterAndIsRefused`, so the deviation is
   executable evidence rather than a note.

A third, smaller correction: the plan's U3 says only the two row-(e) entries of
`admit_oom_basis_test.go` move. Row **(b)** of that same table (ceiling 55 GiB,
the escalation clamped) also moves, to `51352869843`, because it is the same
shape as the clamp-scope table's row (b) — pre-existing duplication the plan
undercounted. Both are updated.

### Tests

New, each written and observed RED against `995d4e3` before the code existed
(T4, T7 and T8 are stated as PINS, green by construction, never as
demonstrations):

- `TestSliceFittedReserve` and `TestSliceFittedReserveInvertsTheEstimatorsOwnMargin`
  (`internal/runner/resource_estimate_fit_test.go`) — the pure quantity as a
  table of 19 real ceilings, the degenerate floor on the byte
  (`1205863 -> MinPinnedScopeCap` vs `1205862 -> 0`), strictly-below on every
  row, `math.MaxInt64 -> 8020323510308500701` where the naive `c*100/115`
  overflows to 0, and the inverse-margin rationale asserted rather than argued.
  *(The maximality half is stated in the exact rational form `115x <= 100c`, not
  as "the largest x whose grown value fits" — the estimator floors `peak*15/100`,
  so `fit+1` sometimes still fits by a byte or two, and the stronger claim would
  be false.)*
- `TestEveryUnpinnedPriorAtOrOverTheCeilingIsFittedAndSaysSo`
  (`internal/daemon/admit_ceiling_fitted_test.go`) — all SEVEN prior routes at
  ceiling 1031798784, each asserting the value 897216333 AND the exact basis;
  plus the GATE, taken at each route's OWN prior because the two priors are
  different numbers.
- `TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped` — the
  provenance property in both directions, plus a grid of 8 stats shapes x 10
  ceilings x 2 p90 states in which no basis may carry both tokens, no
  `estimate:max=` basis may carry the fit token, and every basis stays `^\S+$`.
- `TestTheOOMClampTargetsWhatTheSliceCanGrantNotTheCeiling` — the retarget's
  boundary at three ceilings (the 1 GiB fixture, a default 8 GiB install, and
  the production slice), three rows each: one byte under the fit clamps TO the
  fit; at the fit and one byte over it are unguarded and terminal. Plus I6 across
  a grid: every clamped value is strictly above the peak that produced it and
  strictly below the ceiling.
- `TestSmallSliceUnpinnedRequestIsAdmittedAndGrantedInsteadOfRefused`
  (`internal/daemon/admit_ceiling_fitted_wire_test.go`) — the negative of
  AIRA-151's own "no waiter enqueued" assertion, on the same fixture, driven
  through the real `admitConnection`: a waiter IS enqueued, one evaluator pass
  GRANTS it at 897216333 against the fixture's residual 4 KiB page (which the
  ceiling itself would not have been).
- `TestDefaultInstallCeilingAdmitsANovelUnpinnedCommand` — the ordinary-machine
  case, with PRODUCTION headroom rather than the fixture's: a 6 GiB slice,
  ceiling 4227858432, granted at 3676398636 / `fallback:no-history,ceiling-fitted`.
- `TestAnOOMAtTheFittedCapIsRefusedImmediatelyAndNeverEnqueues` — the wedge
  derivation end to end: `required=1345824499 cap_minus_headroom=1031798784`,
  no waiter, no 30-minute wait.
- `TestSmallSliceOOMSelfHealConvergesOnTheFittedBaseline` and
  `TestSmallSliceOOMAboveTheFittedCapIsTerminalAtTheKernel`
  (`internal/daemon/confine_selfheal_small_slice_real_cgroup_linux_test.go`) —
  **both ends of the ladder proven against a REAL cgroup and a REAL daemon**, not
  in unit arithmetic. T9: a 3 GiB fixture slice, three real seeding runs to
  establish a p90, a cold start OOM-killed at that prior, and the IDENTICAL argv
  re-run with no operator action SUCCEEDING at `ScopeMemoryMax == FIT(ceiling)`
  floored to a page. T13: a 640 MiB slice, phase A pinned at 580 MiB (above the
  fit, below the ceiling) so the kernel really records `MaxOOMPeak > FIT`, and
  phase B refused `E_ADMIT_TOO_LARGE` with `ran=no admission=too_large` inside a
  second against a 30 s window.
- `TestSmallSliceSelfHealFixtureActuallyExercisesTheFitAndStaysOffTheCeiling` —
  the unit guard on both fixtures' constants, with both margins named in the
  assertion messages (1115684864 B of drift before the fit stops firing;
  414689013 B between ceiling and fit, i.e. 79144693 B of slack beyond one whole
  target workload).

One fixture defect of my own, found by the suite and fixed rather than papered
over: T9's phase-3 entry ceiling is `maximum - admitSliceHeadroom(outstandingJobs
+ 1)`, and the previous phase's admission is released ASYNCHRONOUSLY when its
supervisor's connection closes — so phase 3 sometimes arrived with one job still
outstanding and resolved against 3170893824 instead of 3179282432. Fixed by
`waitForDrainedAdmitLedger`, which waits on the daemon's own ledger rather than
on a sleep.

Updates forced by the change: `admit_oom_clamp_wire_test.go` (U1, re-based onto
an ordinary ESTIMATE, which a prior can no longer be); `admit_oom_clamp_scope_test.go`
(U2, four rows, the `tie` row re-based onto an exact estimate/escalation tie at
3221225472); `admit_oom_basis_test.go` (U3, rows (b) and both (e)s);
`skill.go` + `skill_test.go` (U4); `admission_insight_test.go` (U5, nine newly
producible strings plus two forward-defensive negatives);
`confine_admit_test.go` (U7, plus a NEW row at one byte under the fit so the new
boundary is pinned where the old one was); `sliceceiling_test.go` (U8);
`admit_saturated_diagnosis_test.go` (U9, `oomClampedHistory()` renamed
`ceilingExactEstimateHistory()` and re-based onto AIRA-150 route 3's ESTIMATE
half — `PeakMax 3698281962` grows to 4253024256, byte-exactly the 4 GiB fixture
ceiling — chosen over the gate-suggested route 2 because a pinned reserve equals
what the client asked for, which that helper's own contract forbids).

### Mutation evidence

Each applied ALONE in a detached throwaway worktree at `c2cff3f`, then reverted.
All ten went RED, with the exact failing tests recorded:

| # | mutation | RED |
| --- | --- | --- |
| M1 | fit AFTER the resolution instead of before it | T3's dedicated M1 row: answer becomes `897216333`/`fallback:...,ceiling-fitted` instead of `1006632960`/`estimate:oom-escalated` — value AND basis both move, so it cannot pass by coincidence. Also T11 and T13 |
| M2 | fit the ordinary estimate as well as the priors | T3(i) and T3's grid (I2), U1's wire test, the clamp table's `c-prime` and `tie` rows, and two facet-2b fixtures that stop refusing |
| M3 | drop the `,ceiling-fitted` suffix | T2, all 14 rows — the mutant that ships a changed number under an unchanged label |
| M4 | remove the `MinPinnedScopeCap` floor | T1's floor row and T8 (the request is admitted and waits instead of being refused) |
| M5 | fit to the ceiling itself | T1's strictly-below rows, T2, T3, T4's I5 assertion, T11, T5's grant — the mutant that reintroduces AIRA-150 |
| M6 | apply the fit BEFORE the pinned return | T7, all three pinning arms — an operator's own number silently resized |
| M7 | widen AIRA-151's tie-break to `escalated >= reserve` | **exactly** U2's re-based `tie` row and nothing else |
| M8 | leave the clamp's GUARD at `MaxOOMPeak < ceiling` | T11's at-the-fit and over-the-fit rows at all three ceilings, T11's I6 grid, T12, and **T13 at the kernel** (phase B is admitted and OOM-killed instead of refused) — the mutant that reintroduces the 30-minute wedge |
| M9 | leave the clamp's TARGET at `ceiling` | T3, T4's I5 assertion, T11's clamped rows and I5 grid, U2 row (b), U3 row (b), U7 both arms, U8 |
| M10 | drop the `>= ceiling` gate (revision 1's ungated form) | T2's gate rows at ceiling 4294967297 for all seven routes — the mutant that silently under-provisions a job that works today |

### Must-stay-green, verified as RUN rather than inferred

Under `AIRA_REAL_CGROUP=1`, verbosely, with **no SKIP**:
`TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` PASS (0.45s)
and `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` PASS — AIRA-128's shipped
self-heal claim and its basis string, unedited, on a 6 GiB fixture where neither
site fires. `internal/runner/confine_reserve_resolution_linux_test.go` is
untouched and green, which is the evidence that this change is daemon-side only
and that the client still sends the unconditioned default with all three pinning
arms still pinning.

### Gate (2026-09-08, worktree `aira153-condition-default-on-ceiling`)

Serialised, never concurrent, each under `aira confine`, exact exit codes:

| command | exit | verdict |
| --- | --- | --- |
| `aira confine -- go build ./...` | 0 | pass |
| `aira confine -- go vet ./...` | 0 | pass |
| `aira confine -- go fmt ./internal/... ./cmd/...` | 0, no files rewritten | pass |
| `aira confine -- go test ./internal/daemon/... ./internal/runner/... ./internal/core/... ./internal/store/... -count=1` | 0 | pass |
| `aira confine -- go test -race ./internal/daemon/... -count=1` | 0 | pass (AIRA-20 keeps `-race` off CI, so this is local evidence) |
| `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -timeout 25m` | 0 | pass — 14 packages `ok`, 0 `FAIL` |

`gofmt` is not on PATH on this box, so the formatting leg is `go fmt`, which
rewrites and names any file it changes; it named none.

The branch was then rebased onto `origin/master` at `299198d` (AIRA-161, which
touched only `internal/pylib/aitest` and a ticket file — disjoint from
everything here) and the load-bearing leg was **re-run on the rebased tree**:
`AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -timeout 25m` exit
**0**, 14 packages `ok`, 0 `FAIL`, alongside `go build ./...` exit 0 and
`go vet ./...` exit 0. So the recorded gate describes the tree actually
proposed for merge, not a superseded one.

### Deferrals, filed not silent

Ten, all recorded above or on their own tickets. Seven new tickets, IDs
allocated with `aira id AIRA` and never hand-picked: **AIRA-162** (G1, the top
rung can cost one extra real run), **AIRA-163** (G2, the admitted-but-certainly-
OOM band above the degenerate floor), **AIRA-164** (G3, ci-shim's fitted booking
is advisory), **AIRA-165** (G6, and AIRA-151's own G3 — the `E_ADMIT_TOO_LARGE`
message), **AIRA-166** (G7, the guide's `estimate:p90-prior` claim when no p90
exists), **AIRA-167** (G8, install-time refusal considered and refused),
**AIRA-168** (G9, no real-cgroup coverage of the degenerate refusal). G4, G5 and
G10 are records on existing tickets: **AIRA-150 stays open, narrowed twice and
NOT closed**; **AIRA-151** records that its clamp's target and guard moved;
**AIRA-152** stays superseded, with the `max()` rule its rejection rested on
recorded as refined rather than removed.
