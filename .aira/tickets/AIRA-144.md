---
{"schema":1,"id":"AIRA-144","project":"aira","title":"executeScopeKill leaves empty child-cgroup directories behind after a nested kill (scope.Remove is not deepest-first)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["cgroup","runner"],"hold":false,"relations":[]}
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

## Resolution

The fix is in `linuxScope.Remove()` (`internal/runner/cgroup_linux.go`), not at
`executeScopeKill`'s call site.

`Remove()` proved the subtree held no PROCESS (`Empty()` reads `cgroup.events
populated`, which is subtree-aware) and then did a single `rmdir` of the scope.
The kernel refuses `rmdir` on a cgroup that still has a child cgroup — even an
empty one — and `cgroup.kill` drains processes, not directories. So the rmdir
failed `EBUSY` and every call site discarded the error. `Remove()` now removes
its own child cgroup subtree deepest-first before its own rmdir.

The site the ticket names, `executeScopeKill`, is unchanged: it already called
`scope.Remove()`, and that call is now correct. Fixing `Remove()` itself is what
also closes the ticket's "same root cause" note about the NORMAL-exit path
(`runner_linux.go`'s `if empty { scope.Remove() }` on the terminal-commit path),
which predates AIRA-140 entirely — and closes it for `detach`, `confine` and
every other `Remove()` caller in one change rather than five.

### The walk

`removeChildCgroups` reuses AIRA-72's orphan-reaper walk
(`readConfineReapTree` / `removeConfineReapTree`, `confine_manage_linux.go`)
rather than re-deriving one: the whole post-order plan is read before the first
unlink, every `Openat`/`Unlinkat` is anchored to an already-open `O_NOFOLLOW`
directory fd rather than a rebuilt path, and the depth is bounded
(`confineReapMaxDepth`). Only the scope's CHILDREN are removed; the scope's own
rmdir stays where it was. The helpers carry `confine` in their names because the
reaper was their first caller; nothing in them is confine-specific, and the doc
comment says so.

Deepest-first, not a single-level sweep: a child that has children of its own —
aitest's `--delegate-ram` layout (outer -> `.aira-supervisor` / `.aira-worker-N`),
`podman --cgroups=split`, or any workload nesting more than one level — cannot be
rmdir'd until its own children are gone. `internal/cgrouptest`'s `removeScopeTree`
already makes this point for tests; this is the production counterpart.

### The direction the change must NOT drift in

`Remove()` was previously non-destructive by construction: a single rmdir, which
the kernel itself refuses on a live cgroup, so its `Empty()` gate was
belt-and-braces. It now DELETES directories first, which the kernel will happily
do while the scope around them is busy — so that gate has become **load-bearing**
and is now tested as such (T4 below). Removal is not a second emptiness proof and
does not weaken the first one: a child repopulated between `Empty()` and the
unlink fails its `Unlinkat` and `Remove()` returns that error rather than
reporting a teardown that did not happen.

### Tests

`internal/runner/nested_scope_cleanup_linux_test.go`, reusing AIRA-140's
nested-child-cgroup payload shape. The payload nests TWO levels and places the
process in the DEEPEST one, because a single level cannot distinguish a
post-order walk from a flat one.

| Test | Direction it guards |
| --- | --- |
| `TestAIRA144NestedKillRemovesTheChildCgroupTree` | the ticket's trigger: real-cgroup lane, `--timeout` kills a job living two cgroups down, and a real `os.Stat` shows nothing of the tree left |
| `TestAIRA144NestedNormalExitRemovesTheChildCgroupTree` | the same leftover tree on the NORMAL-exit path, which predates AIRA-140 — the ticket's "same root cause" note |
| `TestAIRA144RemoveChildCgroupsIsPostOrderAndLeavesInterfaceFiles` | the walk is post-order, and never touches non-directory entries (a cgroup is mostly interface files); runs on an ordinary filesystem, so it is not gated on cgroup delegation |
| `TestAIRA144RemoveRefusesALiveScopeAndStripsNoChildCgroup` | the anti-over-correction: a live process in the scope's own leaf with an EMPTY child cgroup beside it (aitest's outer scope with a worker cgroup created but not yet populated) — `Remove()` must refuse, must not strip that worker cgroup, and must leave the scope's fd usable for the retry |

**Non-porousness proved by three executed mutations, not asserted.** Each was
applied to the real source and the AIRA-144 tests re-run under
`AIRA_REAL_CGROUP=1`:

1. *The original defect* — `removeChildCgroups` call deleted from `Remove()`.
   T1 and T2 FAILED, reporting the exact shape the ticket describes:
   `the run scope survived its own teardown: .../.aira-RUN-1` and
   `leftover child cgroup: .../.aira-RUN-1/.aira-nested`. T3 passed, correctly:
   it tests the walk, which that mutation does not touch.
2. *A depth-1 sweep* — the recursive `removeConfineReapTree` replaced by a flat
   `Unlinkat(AT_REMOVEDIR)` over the immediate children. All three of T1, T2 and
   T3 FAILED (`T3: directory not empty`). A single-level sweep is not enough.
3. *The gate removed* — the `Empty()` check deleted from `Remove()`. T4 FAILED:
   `Remove() stripped a LIVE job's empty child cgroup .../.aira-worker-1 instead
   of refusing at its gate`. T1-T3 passed, correctly.

Mutation 3 is why T4 has the shape it does. An earlier draft put the live process
in a CHILD cgroup (AIRA-140's leaf-empty/subtree-populated shape) and passed
mutation 3 — the kernel refused the populated child's unlink, so the gate was
never the thing under test. That draft was porous and was replaced rather than
kept alongside; the recorded version fails when the gate goes.

### Evidence

Foreground, exact exit codes, in the worktree at `aira144-nested-cgroup-cleanup`:

```
aira confine -- go build ./...                            -> 0
aira confine -- go vet ./...                              -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -> 0
```

The first full-suite attempt exited 1 on three unrelated wall-clock-tight tests
(`gitremote/TestRealRunTimeoutKillsProcessGroup`, a 50ms deadline;
`runner/TestAIRA131DetachedArbitrationDrainBoundLeavesNoDisposition`, a drain
bound; and on a separate run `runner/TestAIRA138NaiveConfineDeadlineFabricatesAKill`,
a startup-window assertion) while the box was heavily loaded — `internal/store`
took 441s in that run against ~200s in a quiet one. This is recorded rather than
hidden. It is the AIRA-20 flake class, not a regression:

- `internal/gitremote` does not depend on `internal/runner` at all
  (`go list -deps ./internal/gitremote | grep -c aira/internal/runner` -> 0), so
  that failure cannot be caused by this change;
- a full suite on UNMODIFIED `origin/master` (`cb1e3b9`) in a detached baseline
  worktree exited 0 under the lighter load, and the branch's own full suite then
  exited 0 under that same lighter load — the failures track load, not the diff;
- no failing test was in a set the diff can reach: this change alters only what
  `Remove()` does to directories AFTER the subtree is already proven empty.

### Accepted gaps

- **No retry.** `cgrouptest.removeScopeTree` retries its removal for 2s because
  it fires `cgroup.kill` and races the drain; `Remove()` is past that — its
  `Empty()` gate has already observed `populated 0` — so a bounded retry loop
  here would be machinery for a window that the gate has closed. If an unlink
  does fail, `Remove()` returns the error rather than looping, exactly as before.
- **The error is still discarded at every call site.** `_ = scope.Remove()` is
  unchanged in `runner_linux.go`, `detach_linux.go` and `confine_linux.go`.
  Turning a failed teardown into surfaced evidence is a real question, but it is
  a different one (what would a run record say? does a leftover directory make a
  run `unevaluated`?), and answering it inside a P2 cleanup ticket would be
  scope creep. Filed as an observation, not fixed here.
- **The depth bound is inherited, not chosen.** A subtree deeper than
  `confineReapMaxDepth` (32) makes the walk return an error and the scope is
  left behind — the reaper's existing behaviour, reused deliberately rather than
  re-tuned for this caller.

## Fable review record (2026-09-07) — MERGE, PR #92 merged `b142fac`

Reviewed from source in a detached worktree at `dc9dba8`; merge against
`origin/master` (`9ff6531`) clean with no overlapping files.

Independent evidence, foreground, exact exit codes:

```
aira confine -- go build ./...                            -> 0
aira confine -- go vet ./...                              -> 0
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1 -> 0
```

Both load-bearing mutations re-executed by the reviewer, not taken on report:

1. `removeChildCgroups()` call deleted from `Remove()`: T1 and T2 FAILED with
   the ticket's exact shape (`the run scope survived its own teardown:
   .../.aira-RUN-1`, `leftover child cgroup: .../.aira-RUN-1/.aira-nested`);
   T3, T4 passed. Source restored, `git status` clean.
2. `Empty()` gate deleted from `Remove()`: T4 FAILED (`Remove() stripped a
   LIVE job's empty child cgroup .../.aira-worker-1 instead of refusing at its
   gate`); T1-T3 passed. Source restored.

Source checks that closed the reviewer's remaining questions:

- `testdeadline.Wait(300ms)` floors at `MinBackstop` (5s) before scaling, so
  T1's kill cannot fire before the payload's four shell commands have nested —
  the "killed before nesting, passes vacuously" race is closed by the floor,
  the same way AIRA-140's T5 relies on it. Residual: T1 does not independently
  attest that the nested shape was built (only the exit-9 sentinel guards the
  environment). Accepted, same as AIRA-140.
- `removedMeansEmpty` is set only on the confine-kill confirmation scope
  (`confine_manage_linux.go`), which goes to `waitEmpty` and never to
  `Remove()`, so the `Empty()`-true-on-ENOENT → `openat(fd, ".")` on a dead
  cgroup path is unreachable in production.
- The daemon never rmdirs worker scopes on lease close
  (`worker_admit.go` ~L1097), so the outer confine scope's new sweep cannot
  collide with a daemon-side removal.

Observation (non-blocking, consistent with the stated design that a failed
`Remove()` leaves the scope usable): when the child walk fails, `Remove()` now
returns BEFORE `s.fd.Close()`, whereas the old EBUSY-rmdir path closed the fd
first. Every caller discards the error and drops the scope, so in a long-lived
process that fd would linger until the finalizer; today all `Remove()` callers
are short-lived CLI/supervisor processes.

The PR's three accepted gaps above are accepted by the reviewer.
