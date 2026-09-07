---
{"schema":1,"id":"AIRA-144","project":"aira","title":"executeScopeKill leaves empty child-cgroup directories behind after a nested kill (scope.Remove is not deepest-first)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Accepted gap recorded during AIRA-140's Fable review (PR #89, merged `c662e6c`),
filed here as its own ticket per the review's own recommendation.

## The gap

AIRA-140 made `Runner.killScope` correctly reach and `cgroup.kill` a job
whose real processes live in a child cgroup it created inside its own scope
(the leaf-empty/subtree-populated shape). The kill itself works — but the
cleanup afterward does not fully remove the resulting cgroup tree.

`executeScopeKill` (`internal/runner/runner_linux.go` ~L2499-2501) does a
SINGLE `scope.Remove()` — an `rmdir` of the run's own top-level scope
directory. `rmdir` fails `EBUSY` while a child cgroup directory still exists
inside it, even when that child is itself empty (`cgroup.kill` empties the
PROCESSES in the subtree; it does not remove the child cgroup DIRECTORIES
the workload created). That error is discarded.

Reviewer's dogfood run observed this directly: after a nested kill, both
`.aira-RUN-8` (the run scope) and `.aira-RUN-8/.aira-nested` (the child
cgroup the workload created before being killed) were left behind on disk,
both reading `populated 0` (confirmed empty of processes, just not removed).
The reviewer manually `rmdir`'d them to clean up.

This is not new: the SAME thing already happens today for any nested job
that exits NORMALLY (its child cgroup directory is never cleaned up either) —
AIRA-140 only makes the leftover-directory case reachable from the KILL path
too, in addition to the existing normal-exit path. No process leaks in
either case, only empty cgroup directory entries.

## Fix

Deepest-first removal of the scope's own child cgroup subtree before
removing the scope itself, the way `internal/cgrouptest`'s
`removeScopeTree` test helper already does it (walk descendant cgroups,
`rmdir` deepest first, then the parent). `executeScopeKill`'s single
`scope.Remove()` call is the immediate site; the SAME leftover-directory gap
on the normal-exit path (outside this ticket's specific kill-path trigger)
may be worth checking too while this is being fixed, since it is the same
root cause.

## Test that must exist

A real-cgroup lane (reusing AIRA-140's nested-child-cgroup workload pattern)
that kills a job with a child cgroup it created, then asserts via a real
`os.Stat`/directory-listing read that NO cgroup directories remain under
the run's own scope path afterward — not just that the kill's own process
count is empty.