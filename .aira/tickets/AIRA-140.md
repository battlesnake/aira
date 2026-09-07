---
{"schema":1,"id":"AIRA-140","project":"aira","title":"aira run's killScope leaf-only empty gate makes --timeout / --cpu-timeout inert against a nested-cgroup job","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["runner","timeout"],"hold":false,"relations":[]}
---

Filed at AIRA-138's plan-fix, from that ticket's plan-gate P0. It is explicitly
NOT a rider on AIRA-138 (which is scoped to `aira confine`), and it must not be
silently dropped either, which is why it is a ticket rather than a paragraph.

## The defect

`Runner.killScope` (`internal/runner/runner_linux.go:2244-2252`) gates its kill
on LEAF membership alone:

```go
pids, err := scope.Members()          // reads LEAF cgroup.procs
if err != nil { return killResult{}, err }
if len(pids) == 0 {
    empty, emptyErr := scope.Empty()  // subtree-aware, but only consulted here
    return killResult{Empty: empty && emptyErr == nil}, emptyErr
}
```

`Scope.Members()` reads leaf `cgroup.procs`; `Scope.Empty()` reads
`cgroup.events` `populated`, which is SUBTREE-aware (`cgroup_linux.go:248-301`).
They legitimately disagree in one direction: a job whose processes live in child
cgroups it created inside its own scope reads **leaf-empty while fully busy**.
That is not a corner case on this machine -- it is the shape of every
`--delegate-ram`/aitest job (`BootstrapAitestSupervisor` drains EVERY pid into
`<outer>/.aira-supervisor` and `.aira-worker-N`), of `podman --cgroups=split`,
and of any nested-cgroup workload. The repository already documents it at
`ConfineRecord.SubtreePopulated` ("a fully busy suite reads `Populated == 0`
while `SubtreePopulated` is true") and already fixed it once, in `confine --kill`
(`confine_manage_linux.go:533-549`).

Consequence for `aira run`: when `--timeout` or `--cpu-timeout` fires against
such a job, `killScope` writes no signal at all, returns `{Empty:false,
Started:false}`, and the arbitration reports a kill that never happened as
"unevaluated" -- while the job runs on. The bound is inert exactly where a bound
is most wanted.

## Fix

The same correction AIRA-138 makes to `killConfineScope`: require BOTH reads to
agree before refusing to signal (leaf empty AND subtree empty => refuse, and
that refusal is what AIRA-126's `Empty && !Started` proof consumes); a
leaf-empty, subtree-populated scope falls through to `Terminate`/`Kill`.
`cgroup.kill` is recursive, so the subtree is the correct unit for the gate and
for the confirmation alike. An `Empty()` error becomes `unevaluated`, not a
half-populated result returned alongside an error.

Care is needed that `killScope`'s SIGTERM grace stays coherent: `Terminate(pids)`
takes leaf pids, which are empty in this shape, so the nested case must go
straight to the recursive `cgroup.kill` rather than "terminate nothing, wait the
grace, then kill".

## Test that must exist

A fake whose `Members()` is empty while `Empty()` is false -- `livenessScope`
cannot express this, because its `Empty()` is derived from the same
`membersLocked()` as its `Members()`, which is precisely why no existing test
caught this. AIRA-138 commits such a fake (`nestedWorkloadScope`) and an
executable reproduction
(`TestAIRA138LeafOnlyKillGateIsInertAgainstANestedWorkload`); reuse both. Add a
real-cgroup lane where the run's payload migrates itself into a child cgroup and
the timeout must still end it.

## Related

- AIRA-138 (`docs/superpowers/plans/2026-09-07-aira138-confine-deadline-plan.md`
  section 5.2.1) -- the same defect, found by its plan gate, fixed for confine.
- AIRA-126 -- the arbitration whose `Empty && !Started` proof this gate feeds;
  the fix strengthens that proof (two agreeing reads) rather than weakening it.
- AIRA-101 -- where `SubtreePopulated` was added to the scan for the same reason.

## Resolution

`Runner.killScope` (`internal/runner/runner_linux.go`) now gates its refusal to
signal on **two independent reads that must agree** -- leaf `cgroup.procs` via
`Members()` AND subtree-aware `cgroup.events populated` via `Empty()` -- exactly
the correction AIRA-138 landed in `killConfineScope`. A leaf-empty,
subtree-POPULATED scope is a busy job living in child cgroups it created inside
its own scope (aitest / `--delegate-ram` / `podman --cgroups=split`) and is now
KILLED rather than shrugged at.

The change is strictly one-directional: it can only produce a signal in cases
that previously refused one, never the reverse.

- Leaf-empty **and** subtree-empty: unchanged -- returns `{Empty:true}` BEFORE
  any write. That is the sole input to AIRA-126's `Empty && !Started` proof, and
  it is now backed by two agreeing reads rather than one, which is strictly
  stronger evidence for the same claim.
- `Empty()` error: unchanged -- the zero result plus the error (`unevaluated`).
  The old `killResult{Empty: empty && emptyErr == nil}` already yielded
  `Empty:false` on that path, so the returned value is byte-identical; only its
  derivation is now explicit rather than incidental.
- Leaf-populated: unchanged -- full `Terminate` -> TERM grace -> `cgroup.kill`
  escalation.
- Leaf-empty, subtree-populated: **the only behaviour change.** Straight to the
  recursive `cgroup.kill`, with NO SIGTERM grace, because `Terminate` takes leaf
  pids and there are none: "terminate nothing, wait the grace, then kill" would
  be a pure delay in front of the only signal that reaches the job. This is the
  care point the ticket names. A failed `Kill()` write on this arm returns
  `Started:false`, because nothing was sent before it -- unlike the
  leaf-populated arm, which has already delivered SIGTERM by the time it reaches
  its own `Kill()`.

The gate is shared by all four `killScope` callers (the deadline via
`killWithIntent` -> `executeScopeKill`, `aira run kill`, PTY capture teardown,
and reconcile); each of them previously mis-read a nested job as "nothing to
kill", so each is fixed by the one change.

### Tests

`internal/runner/run_nested_kill_gate_linux_test.go`, reusing AIRA-138's
`nestedWorkloadScope` fake and the bracketing shape of
`TestAIRA138LeafOnlyKillGateIsInertAgainstANestedWorkload` rather than
re-deriving them. `livenessScope` still cannot express this state (its `Empty()`
is derived from the same `membersLocked()` as its `Members()`), which is why no
pre-existing test caught the defect.

| Test | Direction it guards |
| --- | --- |
| `TestAIRA140KillScopeKillsALeafEmptySubtreePopulatedRunScope` | the defect: the gate must not re-narrow to `len(pids)==0`; also asserts `Terminate` was NOT called, and that AIRA-126 reads the result as a delivered kill |
| `TestAIRA140KillScopeStillRefusesToSignalAScopeBothReadsCallEmpty` | the over-correction: the gate must not widen to always-kill, or AIRA-126's not-executed arm silently disappears and its fabrication returns |
| `TestAIRA140KillScopeTreatsAnUnreadablePopulationAsUnevaluated` | a failed population read is `unevaluated`, with no write attempted and no half-populated result |
| `TestAIRA140KillScopeKeepsTheTermGraceEscalationForALeafPopulatedScope` | the untouched arm keeps TERM -> grace -> KILL |
| `TestAIRA140RealCgroupTimeoutKillsARunLivingInAChildCgroup` | the real-cgroup lane: the payload nests a child cgroup, migrates itself in, then `exec sleep 30`; the record AND the kernel are both checked |

**Non-porousness proved by executed revert, not asserted.** `killScope` was
restored to the leaf-only gate and the suite re-run:

- `TestAIRA140KillScopeKillsALeafEmptySubtreePopulatedRunScope` FAILED --
  `{Started:false Completed:false Empty:false}`, no `cgroup.kill` written.
- `TestAIRA140RealCgroupTimeoutKillsARunLivingInAChildCgroup` FAILED --
  `Launch` returned `U_RUN_RECONCILE_REQUIRED`, the record carried
  `ScopeKill:{Requested:true Started:false Completed:false}` and
  `ErrorCodes:[E_RUN_TIMEOUT U_RUN_RECONCILE_REQUIRED]`, and the payload's
  `sleep 30` was still running afterwards as a real orphan on the box (killed by
  hand). That is the ticket's defect observed end-to-end, not argued.
- The three anti-over-correction tests passed in BOTH directions, as intended:
  they exist to fail on a widening, which the revert is not.

The real-cgroup lane reports `unavailable` (skip, or fatal under
`AIRA_REAL_CGROUP=1`) rather than degrading into an ordinary leaf-populated
timeout if the environment cannot nest a cgroup: the payload exits 9 instead of
sleeping.

### Doc comments corrected

`killConfineScope` and `leafOnlyKillDraft` both described `aira run`'s
`killScope` as leaf-only in the present tense. Both are now historical notes, so
neither states something false about current code, and
`decideTimeoutIntentNotExecuted`'s conjunct comment now says what a
leaf-empty/subtree-populated scope actually produces (`Started:true`, the
ordinary killed-by-timeout outcome).

### Evidence

Foreground, exact exit codes, full suite under `AIRA_REAL_CGROUP=1`:

```
aira confine -- go build ./...                            -> 0
aira confine -- go vet ./...                              -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -> 0
```

### Accepted gaps

- The nested arm has no SIGTERM grace at all (deliberate, argued above). A job
  that would have exited cleanly on SIGTERM gets SIGKILL if its pids live in a
  child cgroup. `Scope` has no recursive terminate, and adding one to buy a
  grace on this arm is new machinery for no honesty gain; `aira confine`'s
  deadline already documents the same choice.
- `ScopeKill.GraceMS` still records the runner's CONFIGURED `termGrace` on every
  arm, including the nested one where no grace was waited. It is a config
  echo, not a measurement, and was so before this change; left alone rather
  than given a second meaning.
- Contention (a scope repopulating between the two reads) is reasoned about but
  not reproduced in a test; the outcome in that window is the ordinary
  killed-by-timeout arm, which is the safe direction.

## Build review (Fable, 2026-09-07) — MERGE; PR #89 merged as `c662e6c`

Independently re-run in a detached review worktree at `bcab10c`, all foreground
under `aira confine`, exact exit codes:

```
aira confine -- go build ./...                                   -> 0
aira confine -- go vet ./...                                     -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1        -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner -race -count=1  -> 0 (x2, no flake)
```

**Non-porousness re-proved by an executed revert, not taken from the ticket.**
`killScope` was restored to the leaf-only gate in the review worktree and
`-run 'AIRA140|AIRA126|AIRA131|AIRA138'` re-run under `AIRA_REAL_CGROUP=1`:
exactly `TestAIRA140KillScopeKillsALeafEmptySubtreePopulatedRunScope`
(`{Started:false Completed:false Empty:false}`, no write) and
`TestAIRA140RealCgroupTimeoutKillsARunLivingInAChildCgroup`
(`U_RUN_RECONCILE_REQUIRED`, `ScopeKill{Requested:true Started:false}`) went
red; T2/T3/T4 passed in both directions as designed; every AIRA-126, AIRA-131
and AIRA-138 test passed unchanged on BOTH the old and the new gate. The fix
therefore did not touch the already-empty / genuinely-dead-leader shape
`decideTimeoutIntentNotExecuted` consumes: the `Empty && !Started` refusal still
returns before any write and now needs both reads to agree (strictly stronger).

**Dogfood, real nested cgroup, PR binary:** `aira run --timeout 2s -- /bin/sh
payload.sh` where the payload mkdirs `.aira-nested` under its own run scope,
moves `$$` in, proves it via `/proc/self/cgroup`, then `exec sleep 31.14`.
Ended at 2.14s with `status:killed`, `scope_kill{started,completed}`,
`kill_intent{completed,empty_scope}`, `error_codes:[E_RUN_TIMEOUT]` only, and
no surviving `sleep`. On master's binary this job would have run its full 31s.

Verified from source: the nested arm goes straight to the recursive
`cgroup.kill` (no `Terminate` on an empty leaf list, no `termGrace` wait) and
`waitEmpty(grace)` confirms on the subtree-aware read; a failed `Kill()` write
keeps `Started:false`; an `Empty()` error returns the zero result with the error
(byte-identical to the old gate's value on that path). The diff is confined to
`killScope`, three doc comments that had gone false, the ticket, and tests that
reuse AIRA-138's `nestedWorkloadScope` rather than re-deriving it.

### Findings (non-blocking, recorded as accepted gaps)

- **Over-claim in the Resolution above:** "each of the four callers ... is fixed
  by the one change" is not quite true of the PTY capture teardown.
  `runner_linux.go` (the `req.PTY` branch of capture teardown) still pre-gates
  its `killScope` call on `len(scope.Members()) > 0` — a LEAF-only read — so a
  descendant lingering in a child cgroup after a PTY leader exits is still not
  reached by that path. Pre-existing, outside `killScope`'s gate (the ticket's
  scope), and the non-PTY path (`attestScopeTeardown`) is already subtree-aware
  via `scope.Empty()`. Worth its own small ticket.
- **Empty child-cgroup dirs survive a nested kill:** after the nested arm
  completes, `executeScopeKill`'s `scope.Remove()` is a single `rmdir` of the
  run scope, which fails (EBUSY) while the job's now-empty child cgroup is still
  inside it, and the error is discarded. The dogfood run left
  `.aira-RUN-8/.aira-nested` behind, both `populated 0`. The same happens today
  for any nested job that exits normally, so this PR only makes it reachable
  from the kill path; no processes are leaked. Also worth a small ticket
  (deepest-first removal, as `cgrouptest.removeScopeTree` already does).
- The builder's worktree `~/tmp/aira-wt-AIRA-140` still holds the merged local
  branch; `gh pr merge --delete-branch` removed the remote branch only.
