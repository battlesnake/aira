---
{"schema":1,"id":"AIRA-143","project":"aira","title":"PTY capture-teardown's killScope call still leaf-gated, missing a nested-cgroup descendant after the PTY leader exits","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Accepted gap recorded during AIRA-140's Fable review (PR #89, merged `c662e6c`),
filed here as its own ticket per the review's own recommendation.

## The gap

AIRA-140 fixed `Runner.killScope`'s own refusal gate to require BOTH a leaf
`cgroup.procs` read AND a subtree-aware `cgroup.events` `populated` read to
agree before refusing to signal, closing the case where a job's real
processes live in a child cgroup it created inside its own scope (the
`--delegate-ram`/aitest/`podman --cgroups=split` shape).

But the PR's Resolution over-claimed that this single change fixes ALL FOUR
of `killScope`'s callers. It does not: the PTY capture-teardown caller
(`internal/runner/runner_linux.go` ~L837-839) still PRE-GATES on a LEAF-only
read before ever calling `killScope`:

```go
members, _ := scope.Members()
if len(members) > 0 {
    r.killScope(...)
}
```

So a descendant lingering in a child cgroup after the PTY leader itself has
exited is still never reached by this path — `killScope`'s own gate is now
correct, but this caller never gets that far because its OWN pre-check is
leaf-only and short-circuits first.

Not a regression from AIRA-140 (this pre-check predates it and is outside
`killScope`'s own gate, which was that ticket's stated scope) — but it is the
same defect class, one caller away from being fully closed. The non-PTY path
(`attestScopeTeardown`, `runner_linux.go` ~L1877-1900) is already
subtree-aware via `scope.Empty()`, so this is specifically the PTY capture
path's own pre-check that needs the same two-read treatment.

## Fix

Mirror AIRA-140's own fix at this call site: replace the leaf-only
`len(members) > 0` pre-check with the same two-agreeing-reads logic (or, more
simply, drop the pre-check entirely and let `killScope`'s own — now correct —
gate decide, if the pre-check was only ever an optimization to avoid calling
`killScope` on an already-empty scope).

## Test that must exist

Reuse AIRA-138/AIRA-140's `nestedWorkloadScope` fake (or the real-cgroup
lane pattern from `TestAIRA140RealCgroupTimeoutKillsARunLivingInAChildCgroup`)
driven through the PTY capture-teardown path specifically, proving a
nested-cgroup descendant is reached and killed after the PTY leader exits.

## Resolution

The PTY branch of capture teardown (`internal/runner/runner_linux.go`) no longer
pre-gates its `killScope` call on anything. The leaf-only

```go
members, membersErr := scope.Members()
if membersErr == nil && len(members) > 0 { … }
```

is gone; `killScope`'s own AIRA-140 gate — leaf `cgroup.procs` via `Members()`
AND subtree-aware `cgroup.events populated` via `Empty()`, which must AGREE
before it refuses to signal — is now the single authority at this call site, as
it already is at the other three.

Removing the pre-check rather than duplicating AIRA-140's two-read logic in
front of it is the point: **two gates in front of one decision is what created
this hole.** The pre-check was only ever an optimisation that avoided entering
`killScope` on an already-empty scope, and `killScope` returns from exactly that
state (`{Empty:true}`) BEFORE any write, so nothing is lost by removing it — one
extra `cgroup.events` read on an empty scope is the whole cost.

The result is read exactly as before: only a COMPLETED kill attests a reclaimed
descendant (`ScopeDescendantKilled` + `E_RUN_DESCENDANT_KILLED` + a populated
`ScopeKill`), and every other outcome leaves a forced capture abandon reported
as `ScopeHandoffUnverified` + `E_RUN_SCOPE_HANDOFF`.

### Behaviour delta, case by case

| Scope state at the PTY teardown | Before | After |
| --- | --- | --- |
| Leaf-populated | `killScope` → TERM → grace → `cgroup.kill` | unchanged |
| Leaf-empty **and** subtree-empty | pre-check skipped `killScope`; `else if forced` → handoff-unverified | `killScope` returns `{Empty:true}` before any write; same handoff-unverified |
| `Members()` unreadable | pre-check skipped `killScope`; `else if forced` → handoff-unverified | `killScope` returns the read error; same handoff-unverified |
| **Leaf-empty, subtree-POPULATED** | **pre-check skipped `killScope` entirely — the descendant survived the run** | **`killScope`'s nested arm: straight to the recursive `cgroup.kill`, confirmed on the subtree-aware read** |
| Leaf-populated, `killScope` errored or incomplete, capture forced | nothing recorded at all | handoff-unverified |

The last row is the one delta beyond the defect itself. It is strictly the more
conservative direction and it is the honest reading: the capture was abandoned
and the kill could not be completed, so the scope's handoff is exactly
*unverified*. Nothing that previously reported a clean handoff now reports one
less cleanly, and nothing that previously reported a kill now reports none.

As with AIRA-140's nested arm there is **no SIGTERM grace** on the nested shape,
deliberately: `Terminate` takes leaf pids and there are none, so "terminate
nothing, wait `termGrace`, then kill" would be a pure delay in front of the only
signal that reaches the job.

### Tests

`internal/runner/pty_teardown_nested_kill_gate_linux_test.go`, driven through
the **real `Launch`** PTY path (real child, real PTY, real drains, the real
teardown branch) over an injected scope whose two reads are INDEPENDENT sources,
as a real cgroup's are — the one state `livenessScope` structurally cannot
express, because its `Empty()` is derived from the same `membersLocked()` its
`Members()` returns:

- `Members()` is LEAF `cgroup.procs`: the real child while it is alive
  (liveness-checked exactly as `livenessScope` does it), empty once it is reaped
  — which is why the leaf read alone says "nothing to kill" at teardown time;
- `Empty()` is `cgroup.events populated`, SUBTREE-aware: false while the nested
  descendant lives, whatever the leaf says.

That is the ticket's shape verbatim: *a descendant lingering in a child cgroup
after the PTY leader itself has exited*. Keeping the leader in the leaf while it
is alive also keeps the launch's membership monitor honest about it, so the fake
cannot manufacture a leaf-empty-with-a-live-leader `migrated` verdict of its own
(an earlier draft did, and was flaky under load for exactly that reason).

| Test | Direction it guards |
| --- | --- |
| `TestAIRA143PTYCaptureTeardownKillsALeafEmptySubtreePopulatedScope` | the defect: a nested descendant in front of the PTY teardown is reached, killed without a `Terminate` on the empty leaf, and attested |
| `TestAIRA143PTYCaptureTeardownStillRefusesToSignalAScopeBothReadsCallEmpty` | the over-correction: dropping the pre-check must not make the teardown an unconditional kill-and-claim; the `else if forced` handoff-unverified arm must survive |

**Why a fake scope and not a real-cgroup lane.** `quiescePTYScope` runs BEFORE
this teardown and writes `cgroup.kill` unconditionally, and a real `cgroup.kill`
is recursive — so on a healthy scope there is by construction nothing left for
the teardown to reach, and a real-cgroup lane could not tell the old code from
the new. The teardown branch exists precisely for the quiesce that did NOT work
(its write failed, or its confirmation timed out — that is what sets
`ptyCleanupErr` and forces the abandon). A scope whose FIRST `cgroup.kill` write
fails is that state exactly, and it is the only honest way to put a live nested
descendant in front of this call site. The test asserts the failed-quiesce
precondition explicitly (`U_RUN_RECONCILE_REQUIRED`) so it can never pass
vacuously by never entering the branch.

**Non-porousness proved by executed mutation, not asserted.** Both directions
were run:

- **Revert** (the leaf-only pre-check restored):
  `TestAIRA143PTYCaptureTeardownKillsALeafEmptySubtreePopulatedScope` FAILED —
  `cgroup.kill` writes = 1 (the failed quiesce write only, the teardown never
  entered `killScope`), record `ScopeKill:{Requested:false Started:false
  Completed:false}` and `ErrorCodes:[U_RUN_RECONCILE_REQUIRED
  E_RUN_SCOPE_HANDOFF]` — the ticket's defect observed end to end. T2 PASSED, as
  designed: it exists to fail on a widening, which a revert is not.
- **Widening** (the gate mutated to always claim a completed kill):
  `TestAIRA143PTYCaptureTeardownStillRefusesToSignalAScopeBothReadsCallEmpty`
  FAILED — `ScopeKill{Requested:true Started:true Completed:true}` and
  `E_RUN_DESCENDANT_KILLED` fabricated for a scope that had nothing in it.

### Evidence

Foreground, exact exit codes, full suite under `AIRA_REAL_CGROUP=1`:

```
aira confine -- go build ./...                                       -> 0
aira confine -- go vet ./...                                         -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -timeout 25m -> 0
```

`TestAIRA143*` were additionally run `-count=10` (20 executions, all PASS) before
the suite, because an earlier draft of the fake was load-flaky and a single green
run is not evidence that the replacement is not.

An earlier whole-suite attempt on this branch was red for two reasons, both
recorded rather than glossed:

- the earlier draft of the fake (leaf ALWAYS empty, leader not adopted) let the
  membership monitor see a live leader absent from `cgroup.procs` and classify
  the run `migrated`, which outranks `descendant-killed`. That was the FAKE
  lying about the leader, not the fix; the committed fake keeps the leader in
  the leaf while it is alive and the race is gone by construction.
- `internal/store`'s `TestCommandCheckerTimeoutAndTestsGreenZeroCountAreUnevaluated`
  went red once under load with `U_RUN_RECONCILE_REQUIRED: kill intent won before
  terminal evidence`, and another attempt hit the 600s per-package cap inside
  SQLite's `initDB`. Both are pre-existing load flakes on a heavily contended
  box and cannot be reached by this change: the gate command checker never sets
  `Request.PTY` (grepped: no `PTY` in `internal/store` or `internal/gate`
  non-test sources), and the diff is confined to the `req.PTY` branch. The test
  passes `-count=5` in isolation and the whole suite passed on the re-run.

### Accepted gaps

- **`quiescePTYScope`'s `hadDescendants` is still a LEAF-only read**
  (`runner_linux.go`: `members, membersErr := scope.Members(); hadDescendants :=
  len(members) > 0`). This is not a kill gate — that function writes
  `cgroup.kill` unconditionally, so a nested descendant IS reclaimed there — but
  it is the same leaf-only read in the same PTY path, and it decides only
  whether the reclamation is REPORTED (`ScopeDescendantKilled` /
  `E_RUN_DESCENDANT_KILLED`). A PTY run that quiesced a nested descendant
  therefore still reports a clean success. Observed while fixing this call site,
  deliberately left outside this ticket's stated scope (an honesty/reporting
  defect, not an inert-kill one) and worth its own small ticket.
- The `forced`-and-incomplete-kill row of the table above is a widening of when
  `E_RUN_SCOPE_HANDOFF` is emitted. It is the conservative direction, but it is
  a behaviour change beyond the literal defect and is recorded here rather than
  left to be discovered.
- `ScopeKill.GraceMS` still records the runner's CONFIGURED `termGrace` on the
  nested arm, where no grace was waited. Inherited unchanged from AIRA-140,
  which accepted it as a config echo rather than a measurement.
