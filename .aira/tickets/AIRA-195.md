---
{"schema":1,"id":"AIRA-195","project":"aira","title":"Killing an outer confine scope orphans self-confined children in sibling scopes -- no window to react, no listing signal to find them afterwards","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["confine","reaper","safety"],"hold":false,"relations":[]}
---

Peer report (fastest-ee-4b, 2026-09-09), derived from [[AIRA-187]]'s own
finding, verified from source. Not a hypothetical: reachable via the
sanctioned "kill/Ctrl-C the outer job" path, not just `kill -9` misuse.

## The hazard

A parallel `make merge-gate` run wraps the whole gate in one outer `aira
confine`, with each leg self-confining via its own nested `aira confine`
call. Per [[AIRA-187]]: a nested confine does not stay inside its
parent's cgroup -- it moves into a brand-new SIBLING scope under
`aira.slice`. So the leg's SUPERVISOR process lives inside the outer
scope's cgroup, but the leg's actual WORK lives in a separate sibling
cgroup. If the operator kills the outer scope (the sanctioned way to
stop a confine job), that is a `cgroup.kill` write on the OUTER scope --
which reaches every process inside *that* cgroup and its own
descendants, killing the leg supervisors, but does **not** reach a
sibling cgroup at all. Result: the gate appears to die: the leg
supervisors are gone. The leg *work* -- a full test suite, a slowbuild --
keeps running, still capped, now with no supervisor and no operator
watching it, silently burning slice capacity.

## Verified from source

**Q1: is there any window for an inner supervisor to react and clean up
its own child scope before dying?** No, by construction, twice over:

1. `cleanupConfineScope`'s teardown "goes straight to `scope.Kill()`" --
   deliberately NO SIGTERM grace (`internal/runner/confine_linux.go
   :2276-2282`, matching CLAUDE.md's documented contract: "a confined job
   has NO graceful shutdown -- Ctrl-C/SIGTERM hard-kills the whole job
   tree instantly"). `scope.Kill()` writes the cgroup's `cgroup.kill` file
   -- a kernel primitive delivering SIGKILL, which by POSIX definition
   cannot be caught, blocked, or handled by any process. There is no
   signal-handler window, ever, for this class of kill.
2. Even if there were: `cgroup.kill` is recursive **over the target
   cgroup's own descendants only**. A sibling scope, by definition, is
   not a descendant of the outer scope's cgroup -- so the outer's kill
   physically cannot reach it, independent of whether the inner
   supervisor could react. The orphaning is not a missed opportunity to
   clean up; it is structurally unreachable from the outer kill.

**Q2: does `confine --list` (or the existing reaper) let an operator
find and reap these orphans afterwards?** Not reliably, verified two ways:

- `pidIsDead` (`internal/runner/confine_manage_linux.go:444-446`, a
  standard `kill(pid, 0)` liveness probe) exists in the codebase but has
  **zero non-test callers** -- `confine --list`'s `LIVE` column renders
  `SubtreePopulated` (cgroup population) only, with no cross-check
  against whether `SupervisorPID` itself is still a live process. An
  orphaned sibling scope (dead supervisor, still-running work) and a
  healthy one (live supervisor, running work) render **identically** as
  `LIVE=yes` -- there is no listing signal that distinguishes them today.
- AIRA-72's orphan reaper does not help here either: it only acts on
  scopes that are already **empty** (dead supervisor AND all processes
  exited) -- exactly the class this ticket is NOT about. A running
  orphan is invisible to the reaper until its own work finishes on its
  own and the scope becomes empty, at which point the reaper cleans up
  the directory but nothing ever stopped or flagged the unattended run
  while it was happening.

No correct, reliable command sequence exists today to identify "a scope
whose logical parent gate is gone but which is still running" versus an
ordinary independent scope. This is distinct from [[AIRA-183]] (LIVE=no
ambiguity between dead-and-empty-pending-reap vs momentarily-idle-but-
alive) -- that ticket is about an EMPTY scope's ambiguous state; this one
is about a POPULATED scope whose supervisor is dead, which AIRA-183's
scope does not cover.

## Not designed here

Whether the fix is: surfacing supervisor-PID liveness as a genuine field
in `confine --list` (wiring up the already-dead-code `pidIsDead`) so an
orphan is distinguishable from a healthy scope by owner-liveness alone;
some form of the [[AIRA-194]] zero-reserve pass-through orchestrator
(which would need its own answer to "how does a group-kill reach
self-confined siblings" to be a complete fix, not just an admission-
charging change); or a narrower reaper extension that treats
"supervisor confirmed dead, scope still populated" as its own,
faster-than-5-minutes flagged state (not necessarily auto-killed, given
this project's preference to never autonomously decide a job is
disposable) -- is left for whoever picks this up. All three are
compatible with each other, not alternatives.
