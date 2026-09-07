---
{"schema":1,"id":"AIRA-149","project":"aira","title":"An OOM-escalated reserve clamped to EXACTLY the slice ceiling is ungrantable, and reports a fabricated \"slice contended\"","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","honesty"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-149","to":"AIRA-150"},{"kind":"relates","from":"AIRA-149","to":"AIRA-151"},{"kind":"relates","from":"AIRA-149","to":"AIRA-152"},{"kind":"relates","from":"AIRA-149","to":"AIRA-153"},{"kind":"relates","from":"AIRA-149","to":"AIRA-154"},{"kind":"relates","from":"AIRA-149","to":"AIRA-155"},{"kind":"relates","from":"AIRA-149","to":"AIRA-156"},{"kind":"relates","from":"AIRA-149","to":"AIRA-157"},{"kind":"relates","from":"AIRA-149","to":"AIRA-158"},{"kind":"relates","from":"AIRA-149","to":"AIRA-159"}]}
---

Found while root-causing AIRA-139 (a test flake) and deliberately NOT fixed
there: the flake was removed at fixture level because what it exposed in
production is a sizing/diagnosis decision on the machine-wide admission gate,
not a test concern. Two facets, one cause, both in
`internal/daemon/admit.go`'s `resolveAdmitReserve`.

## Facet 1 — `estimate:oom-escalated` is reported for a value that is not an escalation

```go
reserve := request.reserve                       // UNPINNED => always exactly 4 GiB
basis := "fallback:insufficient-samples"
ordinary := stats; ordinary.OOMCount = 0
if estimated, ok, b := runner.EstimateMemoryReserve(ordinary, 0); ok {
    reserve, basis = estimated, b                // needs >= 3 usable samples
}
if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
    escalated := stats.MaxOOMPeak * 3 / 2
    if escalated > reserve { reserve = escalated }   // a MAX, not a replacement
    ...
    return reserve, "estimate:oom-escalated"
}
```

By `runner.ResolveConfineReserve`, an UNPINNED request's reserve is *always*
exactly `runner.DefaultConfineMemoryReserve` (4 GiB) — an explicit
`--memory-reserve`, an explicit `--memory-max` and `--delegate-ram` each set
`pinned`, and a pinned request returns at the first line of
`resolveAdmitReserve`. So when a signature has an OOM record but fewer than
three clean samples — which is the NORMAL state immediately after the first
OOM, i.e. exactly when the self-heal is supposed to act — the ordinary estimate
is unusable, `reserve` stays the blind 4 GiB constant, and the `max` leaves it
untouched unless 1.5 x the OOM peak exceeds 4 GiB (i.e. an OOM peak above
2.7 GiB).

Measured on the AIRA-139 fixture:

    stats={TotalCount:1 SampleCount:1 PeakMax:56360960 OOMCount:1 MaxOOMPeak:56360960}
    req.reserve=4294967296 -> returned 1031798784 with basis "estimate:oom-escalated"

The 1.5x escalation of that peak is 84541440. Neither 84541440 nor anything
derived from it appears in the answer. `reserve-basis=estimate:oom-escalated`
is operator-facing (it is printed in the confine trailer), and here it names a
provenance the number does not have. That is the class of dishonesty AIRA's own
rules exist to prevent.

## Facet 2 — a reserve clamped to the ceiling can never be granted on a live slice

The same branch clamps a too-large reserve to the ceiling:

```go
// An OOM observed at the present ceiling is genuinely too large. Earlier
// censored caps are allowed to climb to the ceiling so a runnable job is
// never permanently wedged.
if stats.MaxOOMPeak < ceiling && reserve > ceiling { reserve = ceiling }
```

but the grant gate is strictly tighter than the admission gate:

```go
// checkedAvailable
effectiveCurrent := max(current - reclaimable, 0)
charge := max(effectiveCurrent, outstanding)
return ceiling - charge            // waiter granted iff reserve <= this
```

`admitConnection` refuses terminally at `reserve > ceiling`, so `ceiling` is the
largest ADMISSIBLE reserve — but the largest GRANTABLE one is
`ceiling - max(current - reclaimable, outstanding)`. A reserve equal to the
ceiling is therefore grantable only while the slice's charge reads **byte-exact
zero**. On a shared, live `aira.slice` that never happens. The stated intent of
the clamp ("so a runnable job is never permanently wedged") is the opposite of
its effect in that case.

Note the documented, deliberate part: `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`
already says a clamped reserve is "accepted, so the request waits". Waiting is
by design. What is not by design is that the wait can never end.

Measured (AIRA-139, 1 GiB fixture slice, ceiling 1031798784):

    granted:     current=0     -> available == reserve
    never granted: current=4096 for 107 consecutive evaluator passes (~30s)

One residual 4 KiB page decided it.

## Facet 2b — and the refusal that follows is a fabricated diagnosis

When the wait expires the caller gets:

    E_ADMIT_SATURATED: confine: admission rejected after 30s — slice contended,
    no memory admission within the wait (reserve 4G/unknown)

with the wait diagnostic reading `queue position 1 of 1 by enqueue order, 0B
queued ahead`. The slice was EMPTY and nothing was contending: the request could
not fit a ceiling it was itself equal to. "Slice contended" is a manufactured
cause, and the operator has nothing to act on — the one number that would
explain it (the resolved reserve vs the ceiling) is not in the message.

## Reachability

Narrow but real, and it widens as the slice shrinks:

- On the production 64 GiB `aira.slice` the clamp needs a resolved reserve above
  roughly the 62 GiB ceiling, i.e. an OOM peak above ~41 GiB (or a smaller
  configured slice).
- On any small slice — a CI slice, a shim-configured ceiling, a fixture, a box
  with a smaller `aira.slice` — the blind 4 GiB default alone exceeds the
  ceiling, so EVERY unpinned request whose signature carries an OOM and fewer
  than three clean samples resolves to exactly the ceiling and then wedges. That
  is precisely what AIRA-139 was.
- The victim is by construction a job that has just been OOM-killed, i.e. the
  self-heal path, i.e. the case the escalation exists to serve.

## Directions (NOT a plan — this needs the two-loop)

Each has real blast radius on the machine-wide admission gate; pick
deliberately, do not merge one as a "cleanup":

1. Make the escalation a REPLACEMENT rather than a `max` with the blind default
   when no usable ordinary estimate exists. Fixes facet 1 exactly and removes
   most of facet 2's reachability. Cost: it changes self-heal SIZING — the
   AIRA-128 real-cgroup fixture's second run currently succeeds on the 4 GiB
   default and would then get ~80 MiB and be OOM-killed again, converging over
   several rounds instead of one. That fixture would need retuning, and the
   "the very next run succeeds" claim would need restating as a ladder.
2. Leave sizing alone and make the clamp target something the grant gate can
   reach. Needs a principled quantity; `ceiling` minus an arbitrary slack is a
   kludge and should be refused on the simplicity rule.
3. Leave both alone and fix the DIAGNOSIS only: when a waiter times out with
   nothing queued ahead and nothing outstanding, say what actually happened
   (resolved reserve == ceiling, slice charge X) instead of "slice contended".
   Cheapest, honest, and fixes nothing structural — but it converts a silent
   30-minute hang into an actionable message, which is the AIRA-71 lesson.

Recording it, unfixed, is deliberate: see the AIRA-139 resolution for why the
flake was closed at fixture level rather than by changing this.

## Resolution (2026-09-07) — direction 3 (diagnosis) plus the facet-1 labelling fix

Plan: `docs/superpowers/plans/2026-09-07-aira149-admit-oom-clamp-honesty-plan.md`
(revision 3, plan-gate approved). Branch `aira149-admit-oom-clamp-honesty`.

**No resolved reserve VALUE and no admission or grant decision changed.** Every
row of the new basis table asserts the value as well as the label, and the only
things that moved anywhere in the tree are operator-facing STRINGS and two new
diagnosis-only wire fields. `checkedAvailable`, the `reserve > ceiling` terminal
boundary, the OOM clamp arithmetic (including its `math.MaxInt64` overflow
guard), the AIRA-59 freeze, the AIRA-114 aggregate bound,
`enqueueAdmitInternal`'s ceiling check and the reserve ledger are untouched.

### Facet 1 — the basis names the term that determined the value

`resolveAdmitReserve`'s OOM branch returned `estimate:oom-escalated`
unconditionally, which is true of exactly ONE of its five outcomes. It now names
the provenance of the number actually returned:

| # | Condition | Value | Basis |
| --- | --- | --- | --- |
| a | `escalated > reserve`, no clamp | 1.5x MaxOOMPeak | `estimate:oom-escalated` *(unchanged)* |
| b | `escalated > reserve`, clamped | ceiling | `estimate:oom-escalated,ceiling-clamped` |
| c | `escalated <= reserve`, ordinary estimate usable | the estimate | *the estimator's own basis* + `,oom-on-record` |
| d | `escalated <= reserve`, no usable estimate | the client's reserve | *the estimator's own `!ok` basis* + `,oom-on-record` |
| e | as (d), then clamped | ceiling | as (d) + `,ceiling-clamped` |

Row (e) is this ticket's measured case and now reads
`fallback:insufficient-samples:n=1,oom-on-record,ceiling-clamped`.

`,oom-on-record` is not decoration. `estimate:oom-escalated` welded together
ATTRIBUTION ("an OOM record for THIS signature was consulted") and PROVENANCE
("the number is 1.5x the OOM peak"). Only the provenance half was false, and the
attribution half is load-bearing: AIRA-128's real-cgroup fixture uses it as the
proof that a real kernel OOM travelled `memory.events` -> teardown ->
`RecordConfinePeak` -> `ConfinePeakHistory` -> `resolveAdmitReserve`. Dropping it
would have deleted a verified property while fixing a false one, and left that
fixture's assertion comparing a string reachable from many paths.

**D4, same function, same rule:** the estimator's own `!ok` basis is no longer
overwritten with a hardcoded `fallback:insufficient-samples`, so a genuine
`fallback:malformed` is reported as such.

### Facet 2b — an established diagnosis instead of a manufactured one

The saturated rejection carried only `Basis` and `Exclusive`, so the terminal
message printed the CLIENT'S OWN unresolved request under the word "reserve",
`unknown` for the ceiling, and asserted "slice contended, no memory admission
within the wait" for every non-exclusive rejection — including one where nothing
else was ever in the way. It now carries:

- `required` / `cap_minus_headroom` — the daemon-resolved reserve and the
  request-entry ceiling, both already in scope at the rejection site;
- `contention` — a LATCHED three-valued reading, joined with `max()` over every
  evaluator pass on the monotone lattice `observed > unevaluated > none-observed`;
- `grantable_bytes` — a POINTER to the `checkedAvailable` figure the capacity
  gate last computed for this waiter, so a measured zero stays distinguishable
  from an absent field.

The reading is derived STRUCTURALLY from `sliceProvablyEmpty` (subtree-aware,
AIRA-101) plus the pass's own `overSubscribed` and a `queuedAhead` counter —
deliberately NOT from `outstandingJobs`/`adoptedJobs`, which `admit.go` itself
forbids for emptiness judgements because a skipped scope is still a running job.
A leaf-drained aitest outer scope has both counters at zero while using memory,
and a counter-derived rule would have printed "nothing else was in the way"
beside a running suite: this ticket's own defect, reintroduced by its fix.
`TestSoloRefusalBesideALeafDrainedScopeReportsContention` drives exactly that
shape on both refusal disjuncts.

The measured case now reports:

    E_ADMIT_SATURATED: confine: admission rejected after 30s — nothing else was
    running in this slice or queued ahead of this request at any evaluation; the
    resolved reserve 984M did not fit the admission ceiling 984M (largest
    grantable reserve 1007612K at the last evaluation). Pin --memory-reserve or
    --memory-max to size this job yourself.

`1007612K` is `ceiling - 4096`, re-derived from the arithmetic rather than
asserted; an earlier plan revision claimed `0B` there, which would have been a
fabricated number inside a change about fabricated numbers. A MEASURED zero
renders `0B` and never through `FormatConfineBytes`, whose output for 0 is the
string `unknown` — this codebase's word for "not established". An ABSENT field
omits the parenthetical entirely and keeps today's wording, so an older daemon
degrades to the existing message rather than to a wrong one.

AIRA-101 exclusivity keeps precedence over the new clause, and `Basis` keeps its
exact `reject:saturated` spelling, which `validRunnerAdmitRejection` pins.

### Facet 2 proper — NOT fixed here

A resolved reserve equal to the ceiling remains grantable only on a
byte-exactly-empty slice. It now fails with an accurate message inside the wait's
own bound instead of a fabricated one; it still fails. That is AIRA-150, with the
two candidate fixes filed separately (AIRA-151, AIRA-153) precisely so a SIZING
change on the machine-wide admission gate is never merged inside an honesty fix.

### Tests

New: `internal/daemon/admit_oom_basis_test.go` (the five-row table across every
estimator basis it can produce, the overflow-guard row, the OOM-attribution
property and its negative direction, D4, and the F8 post-block pin);
`internal/daemon/admit_saturated_diagnosis_test.go` (the wire-path diagnosis,
the latch, the lattice join, the leaf-drained hole, the three refusal sites);
`internal/runner/admission_saturated_message_test.go` (the rendered sentence,
including the measured-zero and absent-field cases and the AIRA-101 precedence);
a new case in `internal/store/admission_insight_test.go` pinning that the OOM
branch's bases stay OUTSIDE the AIRA-52 gauge's evaluable population.

Updated: `TestConfineEstimatorAndOOMEscalationClamp` and
`TestSliceCeilingDoesNotReachTheOOMEscalationClamp` (now assert
`estimate:oom-escalated,ceiling-clamped`, which strengthens the latter's own
stated purpose); the AIRA-128 self-heal fixture and its unit twin (now assert
`fallback:insufficient-samples:n=1,oom-on-record`, with the comment rewritten to
say what the token proves and what it does not); the agent guide and its pin.

`TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins` is unchanged: it is a
verified row (a) with no clamp.

### Deferrals — filed, not silent

AIRA-150 (F1, the structural wedge), AIRA-151 (F2, narrow the clamp),
AIRA-152 (F3, replacement escalation), AIRA-153 (F4, the unconditioned unpinned
default), AIRA-154 (F5, `unevaluated` does not name which of three causes),
AIRA-155 (F6, the in-wait progress line), AIRA-156 (F7, accepted coverage gap:
no real-cgroup test drives the wedge), AIRA-157 (F8, the post-block label
asymmetry, pinned green), AIRA-158 (F9, the dated AIRA-67 spec bullet),
AIRA-159 (F10, the ~1s scan staleness).

### Gate — exact exit codes, all under `aira confine`, serialised

Run on the committed tree (`630d8cf`), in this order, never concurrently:

| Command | Exit |
| --- | --- |
| `aira confine -- go build ./...` | **0** |
| `aira confine -- go vet ./...` | **0** |
| `aira confine -- go test -race ./internal/daemon/... -count=1` (R3) | **0** (ok 112.3s) |
| `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` | **0** (every package ok) |

`gofmt -l internal/ cmd/` is clean. Log: `~/tmp/aira149/gate2.log`.

`aira check` reports `E_JOURNAL_CORRUPT: invalid run ledger record` — reproduced
identically from the repository root on `master`, so it is pre-existing,
machine-wide and about the run ledger, not this change or these tickets.

### Mutation evidence — the load-bearing tests are not porous

Each mutation applied alone, in a detached throwaway worktree at `630d8cf`, then
reverted. All four RED (exit 1):

1. **Drop the `,oom-on-record` append** ->
   `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes` FAILS
   ("stats[2] ... basis=`estimate:max=42949672960,n=5,f=115` names no OOM
   record"). This is the mutant that would have silently degraded AIRA-128's
   attribution proof.
2. **Overwrite the latch instead of joining it** -> both
   `TestSaturatedContentionIsLatchedAcrossTheWholeWaitNotSampledAtRejection`
   (got `none-observed`) and both arms of
   `TestObservedOutranksUnestablishedAndUnestablishedOutranksNoneObserved` FAIL.
3. **Derive contention from `outstandingJobs`/`adoptedJobs`** (plan revision 2's
   rule, with the AIRA-114 belt-and-braces check removed) -> both ordinary-disjunct
   arms of `TestSoloRefusalBesideALeafDrainedScopeReportsContention` FAIL with
   `contention="none-observed"` beside a running leaf-drained suite. The
   aggregate-disjunct arm correctly survives, because `overSubscribed`
   short-circuits to `observed` before the mutated check — which is what that
   third arm is separately for.
4. **Render a measured zero grantable through `FormatConfineBytes`** ->
   `TestSaturatedMessageNamesTheUnfittableReserveInsteadOfContention` FAILS on the
   message "largest grantable reserve **unknown** at the last evaluation".

### One honest note on a test's RED direction

`TestSaturatedMessagePrintsTheDaemonsResolvedReserveNotTheClientsRequest` (plan
T13) is GREEN against master by construction: it is a client-side render test
driven with an explicit `Required`, and master's defect is that the DAEMON never
sends one. The RED-first coverage of that defect is
`TestSaturatedRejectionCarriesTheResolvedReserveAndCeiling` (T7), which was
observed red (`required=0, want the DAEMON-resolved reserve 4939212390`). T13 is
a pin on the render half, not a demonstration.

## Merged (2026-09-07) — PR #100, merge commit `8a07a17`

`8a07a176d6aa23baeea2c938cd5f4703370dacee` on `origin/master`, merged by the
Fable work-review below (the final gate) after verification from source, not
from the build summary.

### Fable work-review record — MERGE

Reviewed at PR head `c7dbfa2` against base `19c6bf2` (== `origin/master` and
local `master` at review time). Every item below was established by reading the
diff and the surrounding source, or by an independent run under `aira confine`,
serialised — never from the builder's claims.

1. **Scope matches the approved revision-3 plan; no silent widening.** The
   touched file set equals plan §9's table plus the ten deferral tickets and the
   plan document. No protocol version, CLI flag, MCP/Skill surface, or
   `internal/store/admission_insight.go` change.
2. **No resolved reserve VALUE and no admission or grant decision changed** —
   confirmed by reading the whole of `resolveAdmitReserve`, the rejection sites
   in `admitConnection`, and the whole of `evaluateAdmitQueue` at the PR head:
   `reserve` is computed by the identical statements in the identical order
   (including the `math.MaxInt64` overflow guard); `checkedAvailable`, the
   `reserve > ceiling` terminal boundary and `enqueueResolvedConfineAdmit` have no
   hunk; the evaluator's grant path is untouched, with every new statement inside
   one of the three pre-existing refusal branches plus one `queuedAhead` counter
   declaration. `EstimateMemoryReserve` returns a non-empty basis on every `!ok`
   path, so `basis + ",oom-on-record"` can never yield a bare token. Under all
   three mutations below, T1's VALUE assertion never fired — only its basis line.
3. **Both honesty facets are genuinely fixed.** Facet 1: the five-row basis table
   was re-derived by hand for rows (b), (c-capped), (d-malformed) and the
   overflow row. Facet 2b: T8 drives the exact measured shape (one waiter,
   position 1 of 1, nothing queued ahead, `current=4096`, resolved reserve ==
   ceiling) through the real `admitConnection` wire path and receives
   `contention="none-observed"`, `required == cap_minus_headroom == ceiling`, and
   `grantable_bytes == ceiling - 4096`; the client renders the resolved reserve,
   the ceiling and `1007612K` with no "slice contended". The `reserve`/`ceiling`
   locals at the rejection sites are the request-entry figures from lines
   1896/1902 and are never reassigned between there and the write; the write is
   built under `queue.mu`.
4. **Tests are non-porous — three mutations reproduced independently** in a
   detached throwaway worktree at `c7dbfa2`, each alone, each restored (all
   `go test` exit 1): (M-a) counter-derived contention -> both ordinary-disjunct
   arms of `TestSoloRefusalBesideALeafDrainedScopeReportsContention` RED with
   `contention="none-observed"`; (M-b) overwrite instead of join ->
   `TestSaturatedContentionIsLatchedAcrossTheWholeWaitNotSampledAtRejection` and
   both arms of `TestObservedOutranksUnestablishedAndUnestablishedOutranksNoneObserved`
   RED; (M-c) drop `,oom-on-record` -> T1 rows (c)-(e), T2 and
   `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` RED. The second T12 arm
   isolates `sliceProvablyEmpty` by leaving the aggregate unestablished, and
   `reject()` refuses a grant frame, so the aggregate arm cannot pass vacuously.
5. **AIRA-128's self-heal convergence is untouched.** The real-cgroup
   `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` and its unit
   twin were re-run on this box with `AIRA_REAL_CGROUP=1` under `aira confine`:
   both PASS (0.52s, actually executed, not skipped), exit 0. In the diff their
   value assertions (`ScopeMemoryMax >= 1.5 x oomPeak`, second run succeeds) are
   unchanged; only the basis string and its comment moved.

Independent gate on the PR head, `aira confine`, serialised:
`go test ./internal/daemon/ ./internal/runner/ ./internal/store/ ./internal/core/ -count=1`
-> every package `ok`, exit 0; `gofmt -l internal/ cmd/` -> clean.

One non-blocking observation, recorded rather than acted on: in a mixed wait
where an early pass establishes solitude and every later pass returns before the
waiter loop on a failed slice-memory read, `none-observed` stands and the
sentence's "at any evaluation" is literally true of the evaluations that
occurred; F5 (AIRA-154) names the whole-wait read failure but not this mixed
shape. Not a fabrication, so not a finding against the merge.
