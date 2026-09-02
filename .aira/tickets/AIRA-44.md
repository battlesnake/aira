---
{"schema":1,"id":"AIRA-44","project":"aira","title":"aitest-bootstrap discovers the wrong outer scope when a confine job runs aitest-enabled pytest twice","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
---
Found by Fable build-review (re-gate, AIRA-30). The immediate symptom
(a deterministic hang) is already mitigated by classifying an
"unevaluated: unbounded" worker-admit response as WorkerAdmitUnavailable
(terminal, falls back safely) instead of retriable -- see
`internal/pylib/aitest/supervisor.py`'s `acquire_worker` and the amended
spec §3.7. This ticket is the ROOT CAUSE that mitigation papers over, not
a duplicate of it.

**The bug.** `runAitestBootstrapCommand` (`cmd/aira/main.go:959`)
self-discovers the "outer" scope via `runner.CurrentCgroupPath()` --
whatever cgroup the CALLING process currently happens to be in -- with no
guard against that cgroup already being a PRIOR run's own
`.aira-supervisor` scope rather than the real, top-level, daemon-admitted
confine scope.

**Failure scenario.** `aira confine --delegate-ram -- make test`, where
`make` runs aitest-enabled pytest twice (an ordinary tox/multi-suite
Makefile pattern; `AppendAitestChildEnvironment`'s injected
`AIRA_AITEST_*` coordinates persist to all descendants, so BOTH pytest
invocations attempt to bootstrap). Run 1's `BootstrapAitestSupervisor`
drains EVERY pid currently in `outer` -- not just its own supervisor pid,
per `drainIntoScope`'s own documented behavior ("a transient child racing
the bootstrap moment can otherwise leave outer non-empty") -- which means
`make` itself (and its shell) get relocated into `<outer>/.aira-supervisor`
along with run 1's own pytest process. Run 1's own end-of-run rmdir of
that scope EBUSYs by design (the supervisor process is still a live
member while calling it) and is left for the AIRA-36 reaper, which does
not fire on a seconds-long test run. When `make` launches pytest #2, its
own current cgroup is now `<outer>/.aira-supervisor` (where it was
relocated), not `<outer>` -- so pytest #2's bootstrap discovers THAT as
its own "outer" scope. The pid-in-scope guard (AIRA-38 fix,
`aitest_bootstrap_linux.go:46`) passes trivially (pytest #2's own process
genuinely is a member of the discovered scope), `backend.Create` is
EEXIST-tolerant and nests `<outer>/.aira-supervisor/.aira-supervisor`,
and delegation succeeds -- but every subsequent worker-admit call against
this nested, uncapped "outer" now gets the `unevaluated: unbounded`
verdict, since `.aira-supervisor` deliberately never has its own
`memory.max` (it is meant to be contained transitively by the real outer
scope, not capped individually).

**Why the existing mitigation isn't a fix.** Classifying `unbounded` as
`WorkerAdmitUnavailable` stops the hang (pytest #2 now correctly falls
back to unconfined workers, still hierarchically bounded by the REAL
outer job's own cap), but pytest #2 still never gets real per-worker
admission/containment at all -- the confined path silently doesn't work
for this ordinary usage pattern, with only the one-time fallback warning
as a clue.

**Candidate fix directions (not designed here).**
- Walk UP the cgroup tree from `CurrentCgroupPath()` to find the actual
  top-level `.aira-CONFINE-*`-named ancestor scope, rather than trusting
  the immediate current cgroup is the real outer scope.
- Or: make `drainIntoScope` selective -- only drain the supervisor's OWN
  pid (and its direct exec lineage), not every pid transiently present in
  `outer`, so `make`/a shell is never relocated in the first place. The
  existing "drain everything" behavior was itself a deliberate fix for a
  different race (a transient child present at bootstrap time) --
  reconciling both needs real design care, not a quick patch.
- Or: make `BootstrapAitestSupervisor` detect and refuse (not silently
  proceed into) an already-supervisor-shaped outer scope, converting this
  into a clear, immediate, diagnosable error rather than the current
  silent-fallback-without-explanation.

The Go doc comment ("Safe for exactly one call per process tree") already
flags a related constraint but does not cover a SECOND, independent
supervisor process appearing later in the same tree.

relates AIRA-30, AIRA-38.
