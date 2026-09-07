---
{"schema":1,"id":"AIRA-143","project":"aira","title":"PTY capture-teardown's killScope call still leaf-gated, missing a nested-cgroup descendant after the PTY leader exits","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
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