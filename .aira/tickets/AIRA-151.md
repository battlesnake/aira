---
{"schema":1,"id":"AIRA-151","project":"aira","title":"Candidate: apply the OOM ceiling clamp only when the escalation determined the value","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-151","to":"AIRA-152"},{"kind":"relates","from":"AIRA-151","to":"AIRA-153"},{"kind":"relates","from":"AIRA-165","to":"AIRA-151"},{"kind":"relates","from":"AIRA-169","to":"AIRA-151"}]}
---

AIRA-149 deferral **F2**, filed WITH its counter-argument.

The clamp's stated justification is that "earlier censored caps are allowed to
climb to the ceiling so a runnable job is never permanently wedged". That
rationale is about a value DERIVED FROM THE OOM PEAK. It does not apply to the
blind unpinned 4 GiB client default, which has nothing to do with the OOM — and
that is the case AIRA-149 measured (basis
`fallback:insufficient-samples:n=1,oom-on-record,ceiling-clamped`).

So a minimal, principled sizing fix exists: clamp only on rows (a)/(b) of the
AIRA-149 basis table, i.e. only when `escalated > reserve`. It would make the
OOM path consistent with the no-OOM path, where the identical over-ceiling value
is already refused terminally with `E_ADMIT_TOO_LARGE` — an honest, immediate,
actionable message carrying both numbers.

**The counter-argument, which is why this is not a cleanup.** It converts an
occasionally-grantable wait into a CERTAIN terminal refusal for a job whose
ordinary estimate exceeds the ceiling but which today can still be granted on a
quiet slice. That is an availability decision on shared infrastructure.

Blocked on nothing, but must go through the full two-loop.

## Resolution (2026-09-07)

**Built exactly as planned.** Plan:
`docs/superpowers/plans/2026-09-07-aira151-narrow-oom-ceiling-clamp-plan.md`
(revision 2, plan-gate approved). Full two-loop, not the light path: this
changes SIZING on the machine-wide admission gate every `aira confine` job on
this box passes through.

### The change

One brace level in `resolveAdmitReserve` (`internal/daemon/admit.go`): the OOM
branch's ceiling clamp now sits INSIDE the `escalated > reserve` block, so it
applies only when the escalation strictly raised the reserve. No other line of
that function changed, and no other production file changed except one clause of
the generated agent guide.

Nesting rather than an `escalationDetermined` boolean is the data-model-first
form: it makes "clamped without the escalation having set the value"
unrepresentable rather than merely untrue, and keeps ONE condition governing
both the basis and the value, so the label and the number can never disagree
about which term acted.

### What moved, and what did not

Rows are AIRA-149 §3.1's, extended by this plan with row (c'):

| row | escalation determined the value? | before | after |
| --- | --- | --- | --- |
| (a) escalated, `MaxOOMPeak >= ceiling` or the value fits | yes | unclamped | **unchanged** |
| (b) escalated, over the ceiling | yes | clamped to the ceiling | **unchanged — still clamped, still waits** |
| (c) ordinary estimate, under the ceiling | no | n/a | unchanged |
| (c') ordinary estimate, **over** the ceiling | no | clamped | **terminal `E_ADMIT_TOO_LARGE`** |
| (d) unpinned client default, under the ceiling | no | n/a | **unchanged** (the AIRA-128 fixture) |
| (e) unpinned client default, **over** the ceiling | no | clamped | **terminal `E_ADMIT_TOO_LARGE`** |

**I1, structural rather than argued: no request is granted a different reserve
and no job's scope `memory.max` changes.** The clamp only ever fired when
`reserve > ceiling`; skipping it therefore leaves `reserve > ceiling`; and every
`reserve > ceiling` is refused at the request-entry boundary BEFORE
`enqueueResolvedConfineAdmit`, so nothing is charged, no waiter exists and no
scope is created. The whole behavioural consequence is *grant-or-wait ->
immediate terminal refusal* for exactly the population above.

The measured case, byte for byte:

    E_ADMIT_TOO_LARGE: required=4294967296 cap_minus_headroom=1031798784 basis=fallback:insufficient-samples:n=1,oom-on-record

instead of being placed on the ceiling and refused ~30 s later after 107
evaluator passes.

### The accepted cost

- **Production (64 GiB `aira.slice`): no availability lost, because essentially
  no production request is in the population** — reaching it needs an unpinned
  resolved reserve above the ~62 GiB ceiling AND an OOM peak below it. Not
  because the grant window there is shut; the residual band is up to 64 MiB per
  job the request entered behind.
- **A small or fixture slice: the change is real.** An unpinned request on a
  slice smaller than the 4 GiB default whose signature carries an OOM record now
  **cannot run at all** without `--memory-reserve` / `--memory-max`, where today
  it sometimes could. In particular AIRA-128's cold-start self-heal stops working
  for that command on such a slice. Accepted: the alternative it replaces is a
  30-second wait that ends in a refusal anyway outside the band, the first run of
  a novel command on such a slice is ALREADY refused by the identical boundary,
  and the underlying fault is the unconditioned default (AIRA-153).
- **The transient-boundary cost.** `cap_minus_headroom` is the ENTRY ceiling,
  which moves by 64 MiB per outstanding job at arrival, so a request marginally
  over it is now refused with advice not to retry even though the ceiling may
  rise once a neighbour finishes. This is already true of the no-OOM path at the
  identical boundary; it is new for the OOM path. Mitigated by the agent-guide
  clause; a message that distinguishes a transient ceiling from a permanent one
  is deferral G3.

Two costs the change REMOVES: a job that just OOM-killed at tens of MiB is no
longer eligible for a hard scope `memory.max` equal to the entire slice; and a
30-second dead-end becomes an immediate answer carrying both numbers and the
OOM-attributed basis.

### Deferrals, filed not silent

- **G1 — AIRA-150 stays open, narrowed.** The SYSTEMATIC route into it is gone;
  three residual routes remain and are recorded on that ticket. **This merge does
  not close it**, and the plan gate ruled explicitly on that (§10 Q5).
- **G2 — AIRA-153 is untouched and now uniformly visible.** Recorded there.
- **G3 — the `E_ADMIT_TOO_LARGE` message names no escape hatch and prints raw
  bytes.** Not fixed here: the string is shared with PINNED requests, where "pin
  `--memory-reserve`" is exactly wrong advice, and `cap_minus_headroom` is a
  transient on a busy slice. Both complications need a case analysis inside a
  message renderer, which is machinery a sizing change has no business adding.
  `TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis` pins the current string
  so the successor starts from evidence.
- **G4 — no real-cgroup test drives the newly-terminal path.** Reproducing it
  needs a slice whose ceiling is below the 4 GiB default AND a signature carrying
  an OOM record, i.e. re-creating the knife edge AIRA-139 removed. Accepted
  coverage gap, inherited from AIRA-149 F7 / AIRA-156, not newly introduced.
- **G5 — the AIRA-67 design spec's basis-vocabulary bullet stays unedited**, for
  the reason AIRA-158 recorded: a dated milestone design record is history, and
  the authoritative live surface is the generated agent guide, which this change
  does update.
- **G6 — AIRA-152 remains an open, compatible candidate.** Recorded there,
  including that the owner chose AIRA-151 over it and why.

### Tests

New, each written and observed RED against `6125156` before the code existed:

- `TestCeilingClampAppliesOnlyWhenTheEscalationDeterminedTheValue`
  (`internal/daemon/admit_oom_clamp_scope_test.go`) — the whole row table, value
  AND basis, including the new row (c') and the `tie` row that pins the strict
  comparison as a SIZING decision rather than an accident. Rows (a)/(b) were
  green before the change and stayed green; rows (c')/(e-default)/(e-malformed)/
  (tie) were RED.
- `TestAnOOMRecordNoLongerChangesWhetherAnOverCeilingEstimateIsClamped` (same
  file) — the consistency claim as a PAIRING: identical peak history with the OOM
  record set and cleared now resolves to the same value (49392123904); only the
  basis differs. Master returned 47244640256 with the record and 49392123904
  without it.
- `TestOverCeilingUnescalatedReserveIsRefusedTerminallyInsteadOfClamped`
  (`internal/daemon/admit_oom_clamp_wire_test.go`) — the measured shape end to
  end through the real `admitConnection` over `net.Pipe`, asserting an immediate
  `E_ADMIT_TOO_LARGE` frame carrying `required=4294967296`,
  `cap_minus_headroom=1031798784` and the OOM-attributed basis, **and that no
  waiter was ever enqueued**. The queue is pre-created without its evaluator and
  the wait timer never fires, so "no waiter" distinguishes a genuine entry
  refusal from a fast timeout rather than racing one.

Pins, GREEN by construction and stated as pins, not demonstrations:

- `TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis` — records the exact
  operator-facing string this ticket routes traffic onto, so G3 rests on a
  recorded string.
- `TestTooLargeRejectionForAnUnescalatedOverCeilingReserveIsAcceptedByTheClient`
  — I5: the payload satisfies `validRunnerAdmitRejection`, so the client takes
  the terminal branch and never falls into `fail()` and the flock fallback.

Updates forced by the change:

- `admit_oom_basis_test.go`: the two row-(e) entries move to their post-change
  values and bases, with a comment naming AIRA-151. Every other row and all of
  the other tests in that file are untouched.
- `admit_saturated_diagnosis_test.go`: `oomClampedHistory()` re-based onto row
  (b) (`MaxOOMPeak` 3.5 GiB, escalation 5637144576) and the **eight**
  `const maximum = int64(1) << 30` declarations raised to `4 << 30`. The two
  8 GiB declarations are untouched — neither calls `oomClampedHistory()`. Every
  assertion is preserved verbatim; `run.ceiling` is derived from each test's own
  `maximum`, so they scale.
- `admission_insight_test.go`: comment only. All ten basis strings are retained
  as forward-defensive negatives even though four became unproducible.
- `skill_test.go`: one new required phrase pinning the guide clause.

Mutation evidence, each applied alone in a throwaway copy at the implementation
tree and then reverted:

1. clamp moved back OUTSIDE the escalation `if` (master's shape) → T1 rows
   (c')/(e-default)/(e-malformed)/(tie), T2, T3 and both re-based
   `admit_oom_basis_test.go` row-(e) entries RED;
2. tie-break widened to `escalated >= reserve` → **exactly** T1's `tie` row RED
   and nothing else — the mutant that proves the strict comparison is a decision;
3. clamp deleted rather than narrowed → `TestConfineEstimatorAndOOMEscalationClamp`,
   `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`, T1 row (b) and
   `admit_oom_basis_test.go` row (b) RED — which is what proves this is a
   narrowing and not a deletion.

### Gate (2026-09-07, worktree `aira151-narrow-oom-ceiling-clamp`)

Serialised, each under `aira confine`, exact exit codes:

| command | exit | verdict |
| --- | --- | --- |
| `aira confine -- go build ./...` | 0 | pass |
| `aira confine -- go vet ./...` | 0 | pass |
| `gofmt -l internal/ cmd/` | 0, no output | pass |
| `aira confine -- go test ./internal/daemon/... ./internal/runner/... ./internal/core/... ./internal/store/... -count=1` | 0 | pass |
| `aira confine -- go test -race ./internal/daemon/... -count=1` | 0 | pass (AIRA-20 keeps `-race` off CI, so this is local evidence) |
| `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -timeout 20m` | 0 | pass — 14 packages `ok`, 0 `FAIL` |

The real-cgroup leg is load-bearing because §1.3's claim about the AIRA-128
fixture is, so it was verified as RUN rather than inferred from a package `ok`:
a verbose targeted run with `AIRA_REAL_CGROUP=1` shows
`--- PASS: TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission
(0.47s)` and `--- PASS: TestOOMSelfHealFixtureStaysOffTheCeilingClamp`, with **no
SKIP**, alongside the other six §7.3 must-stay-green tests. A skip would have
been reported as `unevaluated`, never as a pass.

## Merged (2026-09-07)

PR #101 merged to master as **`030b8cb`** (merge commit; branch head
`5c20036`, base `16b9141`).

### Fable work-review record (final gate)

Verdict **MERGE**. Verified from source and by re-running, not from the build
summary:

- **Scope exact.** Production diff is one hunk in `internal/daemon/admit.go`
  (the clamp nested inside `escalated > reserve`, strict comparison and the
  `MaxOOMPeak < ceiling && reserve > ceiling` guard retained verbatim) plus one
  clause in `internal/core/skill.go`. `ResolveConfineReserve` /
  `DefaultConfineMemoryReserve` (AIRA-153's territory), `checkedAvailable`,
  `admitConnection`, the enqueue-time re-check and
  `resolveDelegateRAMScopeCeiling` are untouched.
- **Expectations moved: exactly four sites**, each the plan's own accepted
  cost — `admit_oom_basis_test.go` rows e/measured and e/malformed (T4c);
  `admit_saturated_diagnosis_test.go` `oomClampedHistory()` + eight
  `const maximum` (T4; the ninth `4 << 30` in that file is the pre-existing
  `peak` constant, and the two 8 GiB declarations are untouched, no assertion
  changed); `admission_insight_test.go` comment only (T4b); `skill_test.go` one
  added pin (T7).
- **AIRA-128's real-cgroup fixture unedited and PASS** under
  `AIRA_REAL_CGROUP=1` (0.71s, no SKIP) — row (d), as §1.3 predicted. AIRA-149's
  facet-2b fixtures affected exactly as R3 predicted: re-based onto row (b),
  asserting `Required == run.ceiling` and `Grantable == run.ceiling - 4096`
  derived from each test's own `maximum`.
- **Mutation evidence reproduced independently** in a detached throwaway
  worktree: M1 (master's `admit.go`) reddens exactly T1 rows
  e-default/e-malformed/c-prime/tie, T2, T3 (5.02s deadline, not a hang) and
  the two moved basis rows, with rows (a)/(b) and every `TestSaturated*`
  fixture green; M2 (`>=`) reddens exactly the `tie` row; M3 (clamp deleted)
  reddens row (b) in both tables, `TestConfineEstimatorAndOOMEscalationClamp`,
  `TestSliceCeilingDoesNotReachTheOOMEscalationClamp` and every re-based
  facet-2b fixture — a narrowing, not a deletion.
- **Bookkeeping follows the approved plan**, not the build brief's parenthetical:
  AIRA-150 stays `planned`, narrowed (G1, plan-gate ruling §10 Q5); AIRA-152
  stays `planned` with the owner decision recorded (G6). Both are the truthful
  treatment — AIRA-150's defect persists on row (b), and AIRA-152 is compatible
  rather than superseded. If the owner overrides the gate, each is a one-line
  status flip.
- **Incidental, pre-existing, not this branch:** `aira reconcile` fails with
  `E_JOURNAL_CORRUPT: invalid run ledger record` (exit 4) identically from the
  master root at `16b9141`. Machine state (the run ledger); worth a dogfood
  ticket.

## Amended by AIRA-153 (2026-09-08) — the clamp's TARGET and GUARD moved

AIRA-153 shipped
(`docs/superpowers/plans/2026-09-08-aira153-condition-unpinned-default-on-ceiling-plan.md`,
§3.3 / G5). This ticket's shipped behaviour changed, so it must not read as
untouched.

**What is unchanged.** The rule, the NESTING inside `escalated > reserve`, the
STRICT tie-break, the single condition governing both the value and the basis,
the `,ceiling-clamped` spelling, and the use of the STATIC ceiling (AIRA-103).
The condition to ENTER the clamp is still `reserve > ceiling`, so an escalation
landing in `(FIT(ceiling), ceiling]` is left exactly as it is.

**What changed.** Both halves of the clamp now use
`runner.SliceFittedReserve(ceiling)` — `floor(100*ceiling/115)`, the largest
reserve the slice can actually GRANT one job — instead of `ceiling`:

    before: if stats.MaxOOMPeak < ceiling && reserve > ceiling { reserve = ceiling }
    after:  if fit > 0 && stats.MaxOOMPeak < fit && reserve > ceiling { reserve = fit }

**Why the TARGET had to move.** This ticket kept the clamp so "earlier censored
caps are allowed to climb ... so a runnable job is never permanently wedged". A
value equal to the ENTRY ceiling is not one such a job can be GRANTED: that is
AIRA-150, byte-exact zero residual charge on a slice the request entered empty.
So the clamp was producing a number the slice could not give.

**Why the GUARD had to move, and why it could not be deferred.** AIRA-153 admits
a small-slice job at a fitted `memory.max` of `FIT(c) = 0.8696c`. A job
OOM-killed there records `MaxOOMPeak ~= 0.8696c`, which lies inside this clamp's
band `(2/3, 1)` BY CONSTRUCTION. With the guard left at `ceiling`, the next
admission would escalate to `1.304c`, clamp to exactly `c`, wait out the default
30-minute window, and be refused `E_ADMIT_SATURATED` — whose documented meaning
to an agent is "owed a RETRY, nothing about the request is wrong". Because the
job never runs, no new peak is recorded, so the state is PERMANENT rather than a
rung. That is precisely the wedge-then-refuse outcome this ticket shipped to
remove, reintroduced on the population AIRA-153 exists to serve.

**The production cost, stated exactly** (64 GiB `aira.slice`, entry ceiling
66504884224 at zero occupancy, `FIT` 57830334107):

| recorded `MaxOOMPeak` | before | after |
| --- | --- | --- |
| < 44336589484 (41.29 GiB) | escalation under the ceiling, no clamp | unchanged |
| 44336589484 .. 57830334106 | clamped to 66504884224 (61.94 GiB) | clamped to **57830334107** (53.86 GiB), and grantable |
| 57830334107 .. 66504884223 | clamped to 66504884224 | **no clamp — terminal `E_ADMIT_TOO_LARGE`** |
| >= 66504884224 | no clamp | unchanged |

Accepted on this ticket's OWN rationale: a wait that ends in a refusal is worse
than an honest refusal, and a job whose last kill was at 54-62 GiB against a
62 GiB ceiling is genuinely too large for that slice. The refusal names
`required` and `cap_minus_headroom`, so the operator gets a number to pin at.

Two properties the retarget preserves and one it adds: every clamped value is
still strictly ABOVE the OOM peak that produced it (the guard), and is now also
strictly BELOW the ceiling by ~13% (the quantity), so a clamped rung is a real,
grantable one.

Deferral **G3** of this ticket (the `E_ADMIT_TOO_LARGE` message prints raw bytes
and names no escape hatch) is carried forward and now has its own ticket,
AIRA-165, with the population narrowed by AIRA-153 recorded there.
