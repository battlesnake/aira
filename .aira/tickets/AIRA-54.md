---
{"schema":1,"id":"AIRA-54","project":"aira","title":"aira gate check reports a fake pass for an empty gate set","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","gate","honesty"],"hold":false,"relations":[]}
---
## Symptom

With zero gates registered in a project, `aira gate check` reports `verdict: pass` (`Passed: 0, Failed: 0, Unevaluated: 0`). Reported by peer session `field` while dogfooding gates; verified directly against source.

## Root cause (verified by direct source read)

`internal/store/gate_eval.go:604-613`:
```go
func (s *Store) GateCheck(ctx context.Context) (GateCheckReport, error) {
	_ = ctx
	discovered, err := s.discoverGates()
	if err != nil {
		return GateCheckReport{}, err
	}
	report := GateCheckReport{Verdict: gate.VerdictPass, Results: []GateCheckResult{}}
	if len(discovered) == 0 {
		return report, nil
	}
	...
```
An empty discovered-gate set short-circuits straight to a `VerdictPass` report before any evaluation logic runs.

## Why this matters (this is a hard-rule violation, not just a rough edge)

This project's own CLAUDE.md states as a hard engineering rule: "AIRA is primitives, not judgement. A check that cannot establish its result reports `unevaluated`, never a fake pass or zero." An unpopulated gate set is precisely a case where no result can be established — there is nothing to check, so "pass" asserts a positive fact (nothing failed) that was never actually evaluated. This is the same "absent reads as clean" hazard the project's own three-state verdict model (pass/fail/unevaluated) and canary-must-fire discipline exist specifically to prevent elsewhere in the codebase (see e.g. the `unevaluated`-as-`failed` synthesis in aitest's JUnit fidelity work, or the reap-only-on-kernel-proof discipline in the confine reaper). A caller that gates a merge or a release on `aira gate check` passing, on a project that has not yet had its gates authored (or where they were accidentally all removed), gets a silent green light instead of a loud "nothing was actually checked."

## Observed, not just hypothetical — composes with AIRA-53 into a silent no-op green board

`field` hit this for real, not as an edge case they went looking for: their task's verification step was "run `aira gate check`, expect `pass`." Because `gate add` creates nothing (AIRA-53), gate registration for their task silently failed — and because an empty set returns `pass` (this ticket), the verification of that failed registration reported success anyway. The only reason it was caught is that the implementer went and read the store source rather than trusting the exit code.

The two defects compose: AIRA-53 makes registration silently fail; this ticket makes the verification of that failure report success. Either alone is survivable (a failed `add` that errors loudly would be caught; an empty-set-pass with no way to accidentally reach an empty set would be inert). Together they produce a fully green gate board with zero gates actually behind it, in the ordinary course of following the documented workflow — not a contrived scenario. This is why the two tickets should probably land together, or at minimum AIRA-53's fix (making `add` create something, or making it error loudly if it can't) should not be considered sufficient on its own without this one, since a future variant of the same silent-registration-failure could recur through a different path and still be masked by this ticket's fake pass.

## Suggested direction

Return `gate.VerdictUnevaluated` (with a distinguishing code, e.g. `U_GATE_SET_EMPTY`) for a zero-gate discovery, or at minimum a verdict distinguishable from a genuine all-gates-passed result, so a caller relying on this for a go/no-go decision cannot mistake "there was nothing here" for "everything here is fine."

## Done — merged `cf81344` (PR #8), 2026-09-03

`GateCheck` no longer short-circuits an empty discovery to `pass`. A zero-gate
set now returns `gate.VerdictUnevaluated` with the distinguishing code
`U_GATE_SET_EMPTY` (`store.GateSetEmptyCode`), and `GateCheckReport` gained a
`Code` field to carry the reason.

This bites where it matters: `verdictExit` maps `unevaluated` to exit 3, so
`aira gate check && merge` now stops instead of proceeding with nothing behind
it. The exit-code transition is asserted at the core boundary, not just in the
payload.

The same fabricated pass existed through a SECOND face, which this ticket's
investigation uncovered: the aggregate `aira check` PRE-SEEDS
`Dimensions["gates"] = "pass"` (check.go:131), so a gate-less project did not get
an absent dimension, it got an affirmative fabricated claim. Captured live on
this repo (which has no `.aira/gates` at all) before the fix:

    $ aira gate check
    verdict: pass
    {"Verdict":"pass","Results":[],"Failed":0,"Unevaluated":0,"Passed":0}
    EXIT=0
    $ aira check | jq .dimensions.gates
    "pass"

Both now report unevaluated. The deciding precedent was in-repo and not
invented: the sibling `traceability` dimension already treats an empty
requirement registry as unevaluated, and `traceability_test.go:312` already
asserted that this makes the AGGREGATE verdict unevaluated. Gates now behave the
same way, with no "feature not in use" exemption.

A prerequisite fix was load-bearing: `checkGatesReadOnly` stamped every non-fail
result as `unevaluated`, discarding a proof-validated PASS and flipping the
aggregate verdict — a false-fail in the opposite direction. Without fixing it the
gates dimension was unevaluated in nearly every scenario, which would have made
the new empty-set signal unobservable and the test vacuous.

Also fixed: `ready` recorded gate-evidence errors only when a selector was
present, so an unselected `aira ready` listed every ticket green over an
unreadable gate file.

Deferred with written rationale to AIRA-56: `ready` treats an unpopulated (as
opposed to unreadable) gate set as no constraint. Unlike `check` it makes no
affirmative claim when there are no gates, and there is no sound filesystem
predicate for "this project uses gates" — `hasGateContent()` is false for an
accidentally emptied directory, exactly the case that matters. The sound signal
is prior activity in the authenticated audit ledger.

Test fallout was 11 tests, corrected rather than suppressed: nine used the
aggregate verdict as a proxy for "warned but nothing failed" and now assert
`len(report.Findings) != 0` directly (stricter, and independent of unrelated
dimensions); one was an over-reach of this change, reverted; and one — the
routing probe hitting `E_GATE_EXISTS` because it pointed `add` at an
already-seeded gate — was itself evidence the AIRA-53 fix works.

Evidence: build 0, vet 0, targeted 0, full suite 0 (12/12 packages, zero
failures). Mutation-tested: reverting the empty-set fix fails the store, the
aggregate-check and the core exit-code tests; all 7 mutations failed as
required, no porous tests.
