---
{"schema":1,"id":"AIRA-140","project":"aira","title":"aira run's killScope leaf-only empty gate makes --timeout / --cpu-timeout inert against a nested-cgroup job","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["runner","timeout"],"hold":false,"relations":[]}
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
