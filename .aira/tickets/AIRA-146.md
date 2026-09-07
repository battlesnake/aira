---
{"schema":1,"id":"AIRA-146","project":"aira","title":"quiescePTYScope's hadDescendants read is leaf-only, so a nested-cgroup descendant it actually reclaims is reported as a clean success (false-green)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Accepted gap recommended for its own ticket during AIRA-143's build (PR #91,
merged `e8bd968`), by the builder's own explicit recommendation: "RECOMMEND
FILING ITS OWN TICKET — this is a false-green, arguably more consequential
than the inert-kill this ticket fixed."

## The gap

AIRA-143 fixed the PTY capture-teardown's `killScope` call to stop pre-gating
on a leaf-only read before ever calling `killScope` (whose own AIRA-140 gate
is correct). But `quiescePTYScope` -- the function that runs BEFORE that
teardown and writes `cgroup.kill` unconditionally -- has a SEPARATE,
still-leaf-only read: `hadDescendants` (`internal/runner/runner_linux.go`
~L1389-1391, verify current line).

`quiescePTYScope` is not itself a kill GATE -- it writes `cgroup.kill`
unconditionally regardless of what `hadDescendants` says, so a nested
descendant in a child cgroup genuinely IS reclaimed by that unconditional
write. The problem is narrower and more insidious: `hadDescendants` decides
only whether that reclamation gets REPORTED (`ScopeDescendantKilled` +
`E_RUN_DESCENDANT_KILLED`). Because the read is leaf-only, a PTY run that
silently reclaimed a live nested descendant still reports a CLEAN SUCCESS --
the kill happened, but the run record says nothing did.

This is a FALSE-GREEN, not a missed kill: the job's own tree was correctly
terminated, but the record lies about it by omission. Same PTY path, same
defect class as AIRA-143 and AIRA-140, deliberately left outside AIRA-143's
stated scope (its own call site, not this one).

## Fix

Give `hadDescendants` the same two-agreeing-reads treatment AIRA-140 and
AIRA-143 already established for this codebase's other kill-adjacent gates
(leaf `cgroup.procs` via `Members()` AND subtree-aware `cgroup.events`
`populated` via `Empty()`) -- here used purely for REPORTING accuracy, not to
gate the write (which stays unconditional).

## Test that must exist

A real or fake nested-cgroup scope whose PTY leader has already exited (leaf
empty) but whose child cgroup still holds a live descendant at the moment
`quiescePTYScope` runs, asserting the run record correctly reports
`ScopeDescendantKilled`/`E_RUN_DESCENDANT_KILLED` rather than a clean
success. Reuse AIRA-143's `ptyNestedScope` pattern (an independent-reads fake
that keeps the leader liveness-checked in the leaf) rather than
AIRA-138/140's `nestedWorkloadScope`, per AIRA-143's own finding that the
latter's unconditionally-nil `Members()` lets the real launch's membership
monitor manufacture a `migrated` verdict that outranks `descendant-killed`.

## Related

- AIRA-143 (`internal/runner/pty_teardown_nested_kill_gate_linux_test.go`) --
  the sibling defect this was found while fixing, same PTY path.
- AIRA-140 -- the pattern (`killScope`'s own two-read gate) this reporting
  fix should mirror, for consistency across the codebase's kill-adjacent
  reads.
## Resolution

`quiescePTYScope` (`internal/runner/runner_linux.go`) now derives its descendant
REPORT from **two independent reads** — leaf `cgroup.procs` via `Members()` AND
subtree-aware `cgroup.events populated` via `Empty()` — the same pair AIRA-140
established for `killScope`'s own gate and AIRA-143 deferred to at the capture
teardown. The `cgroup.kill` write itself is **unchanged and still
unconditional**: it is recursive, so the descendant was always reclaimed, and
making the write conditional on a read is the mistake this whole family of
tickets exists to avoid. What the second read fixes is whether the reclamation
is RECORDED (`ScopeDescendantKilled` + `E_RUN_DESCENDANT_KILLED` + a populated
`ScopeKill`).

The rule, stated once:

- either read observing a population is **positive evidence** and is reported;
- only **two readable, agreeing empty reads** may conclude that nothing was here;
- with no positive evidence and an unreadable read the answer is **not
  established**, so the error is returned — the caller turns that into
  `ScopeHandoffUnverified` + `U_RUN_RECONCILE_REQUIRED` — rather than a
  fabricated clean success.

### Behaviour delta, case by case

| Scope state at the quiesce | Before | After |
| --- | --- | --- |
| Leaf-populated | descendant reported | unchanged |
| Leaf-empty **and** subtree-empty | no descendant reported | unchanged (now on two agreeing reads, strictly stronger evidence for the same claim) |
| **Leaf-empty, subtree-POPULATED** | **the descendant was killed and the record said nothing — a CLEAN SUCCESS** | **`ScopeDescendantKilled` + `E_RUN_DESCENDANT_KILLED` + `ScopeKill{Requested,Started,Completed}`** |
| Leaf-empty, population unreadable | no descendant reported (clean success on one unread fact) | `U_RUN_RECONCILE_REQUIRED` + `ScopeHandoffUnverified` — unevaluated, not clean |
| `Members()` unreadable, subtree readable-empty | `U_RUN_RECONCILE_REQUIRED` | unchanged |
| `Members()` unreadable, subtree POPULATED | `U_RUN_RECONCILE_REQUIRED`, no descendant reported | descendant reported |

Two deltas beyond the literal defect, both recorded rather than left to be
discovered:

- **Row 4 is a widening of when `U_RUN_RECONCILE_REQUIRED` is emitted.** An
  unreadable `cgroup.events` used to leave the leaf read as the sole authority
  for "nothing was here"; that is exactly the fabricated-pass shape the
  repository forbids, so it is now unevaluated. This is the conservative
  direction and it is one extra failure mode only on a scope whose population
  file cannot be read at all.
- **Row 6 is a narrowing of it.** The subtree read positively establishes the
  population the unreadable leaf could not, and the kill and its confirmation
  both completed, so the reclamation is *proved* — reporting it is more precise
  than downgrading a known fact to an unverified handoff. Neither before nor
  after is this a clean success.

`Members()` returning both a non-empty slice and an error no longer counts as
positive evidence (`membersErr == nil && len(members) > 0`). The old code's
`len(members) > 0` on an errored read was discarded by the `membersErr` arm
below it anyway, so no reachable behaviour changes; the derivation is now
explicit rather than incidental.

### Tests

`internal/runner/pty_quiesce_descendant_report_linux_test.go`, reusing
AIRA-143's `ptyNestedScope` fake and its `aira143PTYRunner` real-`Launch`
harness verbatim (per this ticket, and per AIRA-143's own finding that
AIRA-138/140's `nestedWorkloadScope` — whose `Members()` is unconditionally nil
— would let the launch's membership monitor manufacture a `migrated` verdict
that outranks `descendant-killed`). The difference from AIRA-143's lane is the
one that matters here: the quiesce's `cgroup.kill` write **succeeds**, so the
capture teardown is never entered and what is under test is the quiesce's own
report. Both full-launch lanes assert that precondition explicitly — no
`U_RUN_RECONCILE_REQUIRED`, and exactly ONE `cgroup.kill` write — so neither can
pass vacuously through AIRA-143's neighbouring path.

| Test | Direction it guards |
| --- | --- |
| `TestAIRA146PTYQuiesceReportsAReclaimedNestedDescendant` | the defect: leader exited, descendant alive in a child cgroup, reclaimed by the unconditional write — and now reported |
| `TestAIRA146PTYQuiesceReportsNoDescendantWhenBothReadsCallTheScopeEmpty` | the over-correction: an ordinary PTY run over a scope both reads call empty must stay a `CleanSuccess`, with no fabricated `ScopeKill` |
| `TestAIRA146QuiescePTYScopeDescendantReportReadMatrix` | all seven read shapes at the function itself, plus the two things a record cannot show: the write stays UNCONDITIONAL in every row, and an unestablished answer is an error rather than a fabricated "no descendants" |

**Non-porousness proved by executed mutation, not asserted.** Three mutations,
all run under `AIRA_REAL_CGROUP=1`:

- **Revert** (`hadDescendants := len(members) > 0`, the leaf-only read
  restored): `TestAIRA146PTYQuiesceReportsAReclaimedNestedDescendant` FAILED
  with `integrity="contained" codes=[]` — the ticket's false-green observed end
  to end, a live nested descendant killed and the record silent about it — plus
  the three matrix rows that depend on the subtree read. T2 PASSED, as designed:
  it exists to fail on a widening, which a revert is not.
- **Widening** (`hadDescendants := true`): T2 FAILED with
  `integrity="descendant-killed" codes=[E_RUN_DESCENDANT_KILLED]` fabricated for
  a scope that had nothing in it, plus the three empty matrix rows. T1 passed.
- **Gating the write** (`if hadDescendants { scope.Kill() }`, the mistake the
  fix must not introduce): T2 and three matrix rows FAILED on
  `cgroup.kill writes=0`.

### Evidence

Foreground, exact exit codes, full suite under `AIRA_REAL_CGROUP=1`:

```
aira confine -- go build ./...                                        -> 0
aira confine -- go vet ./...                                          -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -timeout 25m -> 0
```

`TestAIRA146*` additionally passed `-count=5`.

One red run on the way there, recorded rather than glossed: a `-count=5` pass
over `AIRA146|AIRA143|AIRA140|AIRA138|AIRA126|PTYScopeQuiescence` failed once in
`TestAIRA138NaiveConfineDeadlineFabricatesAKill` at its *precondition* — "the
scope was not empty at the fire: members=[2117024]", i.e. the kernel had not yet
dropped a provably-dead leader from `cgroup.procs` under load. That is a
wall-clock-tight flake in the confine deadline reproduction, unreachable from
this diff (no PTY, no `quiescePTYScope`), and it passed `-count=5` in isolation.

### Accepted gaps

- **The two reads are not atomic.** A scope that repopulates between
  `Members()` and `Empty()` is reported as populated, which is the safe
  direction; the reverse window (a descendant exiting between the reads) reports
  a descendant that was already gone, which over-reports rather than under-. The
  same contention gap AIRA-140 accepted for the gate, for the same reason: a
  consistent snapshot of two kernel files does not exist.
- **`ScopeKill.GraceMS` is still the runner's CONFIGURED grace** on this arm,
  where the quiesce waited no TERM grace at all. Inherited unchanged from
  AIRA-140 and AIRA-143, which both accepted it as a config echo rather than a
  measurement.
- **The report cannot distinguish a nested descendant from a leaf one.**
  `E_RUN_DESCENDANT_KILLED` says a population was reclaimed, not where it lived.
  Recording the two reads separately would be new record surface for no
  additional honesty about what happened.
