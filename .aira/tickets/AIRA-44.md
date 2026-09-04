---
{"schema":1,"id":"AIRA-44","project":"aira","title":"aitest-bootstrap discovers the wrong outer scope when a confine job runs aitest-enabled pytest twice","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","hardening"],"hold":false,"relations":[]}
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

## Resolution (2026-09-04, backlog-remediation Phase 0, plan §2)

**Direction taken: hand the outer scope down, don't discover it.** None of the
three candidate directions in the body were adopted literally — walking up the
cgroup tree looking for a `.aira-CONFINE-*`-named ancestor guesses at a fact the
launcher already knows; making `drainIntoScope` selective would reopen the
transient-child race it was written to fix; refusing on a supervisor-shaped outer
would turn a working second run into a hard error. The launcher holds the real
scope in `scope.Reference()`, so it publishes it and the discovery step goes away.

- `AppendAitestChildEnvironment` (`internal/pylib/env.go`) gains an `outerScope`
  argument and publishes **`AIRA_AITEST_OUTER_SCOPE`**, registered in
  `aitestEnvironmentKeys` so it is stripped from every non-delegate launch
  exactly like the other four coordinates.
- `confine_linux.go` passes `scope.Reference()` — this job's own outer scope.
- `runAitestBootstrapCommand` (`cmd/aira/main.go`) prefers that value and falls
  back to `runner.CurrentCgroupPath()` only when it is unset (a launcher
  predating the coordinate, or a hand invocation). Self-discovery is still
  correct for a single-run job, so the fallback is honest rather than a hazard.
- `BootstrapAitestSupervisor`'s membership guard now accepts a supervisor pid in
  **either** `outerScope` **or** `<outer>/.aira-supervisor`.

That last part is load-bearing, not cosmetic. By the second bootstrap the whole
process tree — `make`, its shell, everything run 1's drain swept up — lives in
`<outer>/.aira-supervisor`, so the old guard (`member of outer` only) would have
turned the fix into a hard refusal: passing the REAL outer scope is exactly the
case the old guard rejects.

**Build-review correction (Sol) — an earlier version of this fix claimed the
second disjunct "does not widen the guard's protection". That was false and is
retracted.** `.aira-supervisor` is a predictable name, so "the caller's pid is in
`<X>/.aira-supervisor`" alone is not evidence that `X` is this job's scope: a
shared cgroup holding an IDE, other shells and other agents can acquire such a
child from a partial or hand-run bootstrap, and every direct member of `X` would
then be drained. The second route therefore now requires **positive proof** that
`outerScope` is a daemon-admitted confine scope — a finite `memory.max`. That is
the exact invariant the aitest design already leans on (a real confine-launched
outer scope is given a finite `memory.max` in the same atomic grant that launches
it), while a shared session scope and a nested `.aira-supervisor` are both
deliberately uncapped. `scopeHasFiniteMemoryMax` is fail-closed in every
uncertain direction: unreadable, `"max"`, empty, zero and unparseable all read as
"no proof". The first route (pid genuinely in `outerScope`) is untouched.

The env value is not trusted blindly: the guard still refuses any scope the
supervisor is not inside, the second route additionally demands the finite cap,
and the verb refuses a non-absolute `AIRA_AITEST_OUTER_SCOPE` outright rather
than resolving it against pytest's working directory (which would mutate one
cgroup and report an `outer=` the daemon resolves elsewhere — silently wrong
accounting instead of a clean error).

### Regression test

Two seams, because the fix has two halves and — as build-review (Sol) pointed
out about an earlier draft — a runner-level test alone cannot see the CLI half:

**Runner seam.** `TestBootstrapAitestSupervisorIsIdempotentForASecondRunInTheSameJob`
(`internal/runner/aitest_bootstrap_linux_test.go`, real cgroups): bootstrap once,
assert the stand-in supervisor really has left `outer` for the supervisor child
(the precondition, asserted not assumed), then bootstrap again with the same
outer and require it to (a) succeed, (b) return the *same* scope, and (c) leave
no nested `.aira-supervisor/.aira-supervisor` — the nesting whose worker-admit
calls answered `unevaluated: unbounded`. Then it uncaps `outer` and requires the
same call to REFUSE, pinning the positive-proof requirement above.
Mutation-verified: reverting the guard to `member of outer` only fails it.

**CLI seam.** `cmd/aira/aitest_bootstrap_command_linux_test.go` covers the
selection the runner test cannot see: the verb prefers the published
`AIRA_AITEST_OUTER_SCOPE`, falls back to self-discovery when it is unset, and
refuses a relative one. All three use pid 1 as the supervisor pid — always alive,
never a member of the test's own cgroup — so the membership guard (which runs
before anything is moved) always refuses and the refusal names the path that was
selected. Nothing is mutated, deliberately: a pid that IS a member would drain
the test binary's own cgroup. Mutation-verified: removing the env preference from
`runAitestBootstrapCommand` fails two of the three.

`TestAppendAitestChildEnvironmentOmitsAnUnknownOuterScope` pins the blank case:
an empty scope publishes no key at all rather than `AIRA_AITEST_OUTER_SCOPE=`,
which the verb would read as "set" and hand to the guard as a path.

### Spec

`docs/superpowers/specs/2026-09-01-aitest-design.md` described this exact
scenario as the motivating example for classifying an `unbounded` outer scope as
fallback-triggering. Amended in the same commit rather than left silently
stale: the discovery step is gone, so that classification is now a backstop for a
hand invocation or a coordinate-less launcher, not the expected path here. The
classification itself is unchanged.

AIRA-44 -> done. `make ci`: exit 0.
