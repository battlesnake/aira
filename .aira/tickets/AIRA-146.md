---
{"schema":1,"id":"AIRA-146","project":"aira","title":"quiescePTYScope's hadDescendants read is leaf-only, so a nested-cgroup descendant it actually reclaims is reported as a clean success (false-green)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
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