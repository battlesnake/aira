---
{"schema":1,"id":"AIRA-121","project":"aira","title":"aira install/confine: ci-shim mode for systemd/cgroup-unavailable containers (GCP Batch, similar CI/batch containers)","status":"done","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","ci","confine","install"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-121","to":"AIRA-123"}]}
---
Requested directly by the owner, 2026-09-06, as a follow-up question on AIRA-120.

## The problem AIRA-120 does not cover

AIRA-120 assumes a systemd + delegated-cgroup-v2-available host. A
containerised batch execution shape (GCP Batch container runnables, and
similarly AWS Batch/Fargate, Cloud Build, a Kubernetes Job, a container-mode
CI runner) typically has NO systemd running inside the container at all --
not even for the one-time `aira install` step. Verified against current
source, 2026-09-06:

- `aira install` hard-requires `systemctl --user daemon-reload` /
  `enable --now` to create `aira.slice` and register `aira-daemon.service`
  (internal/install/install.go). No systemd user manager -> install fails
  outright, before any confine/run ever gets a chance to work.
- Per-job `aira confine` itself does NOT re-invoke systemd at request time
  (no systemd-run/StartTransientUnit anywhere in internal/runner) -- it writes
  cgroupfs directly (mkdir a scope dir under the already-delegated aira.slice
  subtree, write memory.max, cgroup.procs, etc). So the ONLY hard systemd
  dependency is the one-time slice+daemon-service setup at install, not every
  confine call -- but that one dependency is still a full, current blocker in
  a systemd-less container.
- The daemon binary has no systemd dependency of its own: `aira daemon serve`
  (cmd/aira/daemon_command.go) is a plain subcommand; systemd is only how
  `aira install` currently chooses to supervise it.
- `aitest`'s pytest worker pool (internal/pylib/aitest) already has a real,
  tested 'daemon-down fallback mode' (Task 16 / AIRA-37 residue --
  test_daemon_down_fallback_completes_suite_with_one_warning_no_admit_subprocess
  in test_supervisor.py): when the daemon is unavailable it runs the whole
  suite via a bare os.fork() worker pool with ONE honest warning, no cgroup
  placement attempted, and time/test-count-based worker recycling still
  governing (RAM-watermark recycling is the only piece that needs a granted
  cgroup scope and is skipped). This already substantially satisfies 'aitest
  concurrency governing' for the systemd-less case -- it does not need a
  redesign, only confirming/keeping it wired up under the new mode below.

## What to build

`aira install --ci=shim` (or auto-detected when `--ci` is given and the
daemon-target probe below fails -- decide which at build time, but whichever
is chosen, ALWAYS report explicitly which mode actually got installed; never
let two boxes running nominally 'the same' install command end up silently
different):

1. A capability probe at install time: can we actually create/own a delegated
   cgroup v2 subtree and talk to a systemd user manager? If not (or if
   `--ci=shim` is explicit), skip ALL systemd unit creation (no aira.slice
   unit, no aira-daemon.service unit) and instead start `aira daemon serve`
   directly as a plain background process, appropriate for that container's
   own lifetime (no persistence expected or required across container
   restarts -- each fresh Batch/CI container is expected to install+run once).
2. In this mode, `aira confine --`/`aira run` runs the job as a normal
   subprocess with NO cgroup scope creation attempted at all (skip it
   entirely up front -- do not attempt and fail, which would be noisy and
   pointless in an environment already known not to support it).
3. Containment in this mode is ADVISORY-ONLY: an in-daemon RAM-budget ledger,
   reusing the EXISTING per-signature reservation-estimate machinery from
   AIRA-67 (peak-RSS history-based estimator) purely for ADMISSION GATING
   (queue a new job if the ledger says the container's own declared/probed
   total RAM is already fully booked by admitted jobs) -- with NO real
   per-process kill backstop, since there is no cgroup to kill. This is a
   genuinely weaker guarantee than the real slice and MUST be reported
   honestly everywhere containment is claimed (`confine --list`/`--status`,
   any 'scope-integrity'-style field) -- e.g. 'containment: advisory
   (ci-shim, no cgroup)' rather than anything implying a hard bound. This
   project's own existing idiom for exactly this class of tradeoff is
   aitest's 'marking unevaluated rather than running unconfined silently' --
   follow the same honesty discipline here, do not invent a new one.
4. Read the container's own total RAM budget for the ledger the same way
   AIRA-120's --ci reads free RAM (MemAvailable at install time; if the
   container's cgroup memory.max IS readable even though we cannot write our
   own nested cgroup, prefer that as the more accurate 'what am I actually
   allowed' figure over host-wide MemAvailable -- decide which source is more
   correct for a container context during build and document the choice).
5. aitest's daemon-down fallback needs NO changes to keep working under this
   mode (it already tolerates no daemon); confirm this with a real end-to-end
   test rather than assuming it. A reasonable, separate enhancement (not
   required for this ticket to close, note as a follow-up if not done here):
   let aitest's worker-count 'auto' sizing consult the new shim RAM-budget
   ledger when a daemon IS present in shim mode, rather than staying
   deliberately CPU-only-sized in that case.

## Consumer-verified requirements (peer session `deploy`, GCP Batch CI runner, 2026-09-06)

Raised while this ticket was still in scoping, with the failure mode each one
prevents. Verified against current source before being added here (three
confirmed as described; the fourth's framing corrected against what the code
actually does today):

6. **`confine` must accept-and-ignore existing resource flags, not reject
   them.** Real recipes already pass `--memory-max`, `--memory-reserve`,
   `--delegate-ram` etc. (`aira confine --memory-max 32G --memory-reserve
   512M -- make merge-gate` is a real invocation in the consumer's merge
   gate). Shim mode MUST parse these through the SAME flag surface the real
   mode uses and make them inert (no cgroup write attempted), not treat them
   as unknown/rejected arguments -- otherwise every existing recipe using them
   breaks on first run, exactly the `if CI` branching this ticket exists to
   avoid. Requirement 7 below is the one exception: `--delegate-ram`'s flag
   parsing stays accepted, but one of its current SIDE EFFECTS must not fire.
7. **INTERIM ONLY (superseded design correction below) — do NOT export
   `AIRA_AITEST_LIB` when `--delegate-ram` runs in shim mode**, as the
   behaviour to ship in THIS ticket, pending AIRA-123. This is a deliberate,
   explicit deferral, not the intended final answer -- do not read it as
   "aitest never gets RAM-aware concurrency in shim mode." The owner (via
   `deploy`) corrected the original framing: it conflated cgroups'
   ENFORCEMENT (needs a real scope) with a ledger's ADMISSION (does not) --
   an in-daemon ledger can genuinely prevent over-subscription without any
   cgroup, which is most of the real value on a single-tenant CI box where
   losing the kill backstop costs one job rerun, not collateral damage to a
   shared session. AIRA-123 is the follow-up: extend `worker-admit` to a
   degraded ledger-only admission mode with no cgroup sub-scope, honestly
   reported as advisory. Once AIRA-123 lands, export `AIRA_AITEST_LIB`
   whenever that degraded backend can function -- the rule is conditional
   ("export if the backend can actually work in this mode"), not a flat
   never. Verified in source: `internal/pylib/env.go`'s delegate-ram path
   sets `AIRA_AITEST_LIB` in the child env unconditionally today, and a
   consumer's `conftest.py` uses ITS PRESENCE ALONE as the guard that
   activates the `aitest` pytest plugin (documented in this repo's own
   generated Skill text, core/skill.go, as the two preconditions for aitest:
   the project registers the plugin when this var is set, then launches with
   `--aitest-workers`). aitest's per-worker RAM containment is not optional
   machinery -- `worker-admit` places each forked worker in its OWN
   kernel-enforced cgroup sub-scope nested under the outer job's scope; there
   is no sub-scope to nest under in shim mode. If shim mode still exported
   this var, a consumer's conftest.py would activate aitest, aitest would
   attempt real per-worker cgroup admission that structurally cannot succeed
   the way it does on the real path, and (worst case) heavy suites would run
   under an apparent governance mechanism with no actual backstop --
   `deploy`'s framing: "invisible until something OOMs." Leaving the var
   unset in shim mode is correct AND sufficient FOR THIS TICKET's scope: it
   makes the consumer's own existing guard fall through to plain pytest-xdist
   as a safe interim default (a broken RAM-aware backend is worse than a
   RAM-blind one) -- see the INTERIM ONLY note above and AIRA-123 for the
   real target.
8. **Forward the received signal to the child's process GROUP in shim mode,
   not just the single direct child.** Correction to how this was originally
   raised: current (real-mode) `confine` code already forwards SIGINT/SIGTERM
   directly to its immediate child (`forwardConfineSignals`,
   internal/runner/confine_linux.go, calls `child.Signal(received)`) --
   it is not true that the real path only relies on cgroup.kill and eats the
   signal otherwise. The genuine shim-mode gap is narrower but still real:
   cgroup.kill's actual value on the real path is catching DESCENDANTS beyond
   the direct child (a forked/reparented grandchild that `Signal()` on one
   PID cannot reach) -- exactly the class of escape AIRA-20's descendant-
   escape attestation work exists to detect on the cgroup path. Shim mode has
   no cgroup.kill backstop for that class at all. Mitigate as far as a
   non-cgroup mechanism reasonably can: start the child in its own process
   group (setpgid) and signal the group (`kill(-pgid, sig)`), which reaches
   any descendant that has not itself deliberately detached/setsid'd, and
   document plainly (do not silently imply full parity) that a deliberately
   double-forked/detached descendant still is not reachable without a real
   cgroup -- matching this project's honesty convention rather than pretending
   an equivalent guarantee. This matters concretely for GCP Batch: Batch sends
   SIGTERM on job timeout and on preemption, and a real workload that never
   sees it (or whose own children never see it) loses whatever graceful
   teardown it had, until Batch's own harder kill lands later.
9. **`aira install` must split cleanly into a build-time step (place
   binary+config, no network, no side effects, safe to run inside a `docker
   build` layer) and a start-time step (start whatever the resolved mode
   needs).** The consumer's actual deployment shape bakes aira into a Docker
   image at build time, then runs jobs in fresh containers from that image
   later -- there is no daemon and no systemd at image-build time either.
   Related and separately testable: if shim mode's daemon runs as a plain
   background process, it must never be something the container's entrypoint
   waits ON -- a Batch job must complete (and the container exit) the moment
   the actual workload process exits, not hang on a backgrounded daemon that
   is still technically alive. (Reaping the daemon once the container's PID 1
   exits is the container init's job and is NOT this ticket's concern --
   correct backgrounding so nothing waits on it while the container is still
   running is.)

## Deliberately out of scope here

Do not try to make the real cgroup slice mechanism work inside a container via
elevated privileges, --cgroupns=host, or similar -- that is fragile and
provider-specific (Docker/containerd cgroup v2 delegation behaviour is
inconsistent across GCP Batch/AWS Batch/Fargate/k8s), and is exactly the kind
of per-provider special-casing this project's architectural-simplicity rule
warns against. The shim's job is to degrade honestly, not to chase real
containment into every possible container runtime.

## Tests

Real end-to-end coverage: (a) install with the systemd/cgroup probe forced to
fail resolves to shim mode, starts a plain-process daemon, and confine/run
jobs execute successfully with no cgroup scope created; (b) the RAM-budget
ledger actually gates admission (a second job queues when the first has
'used' the whole budget per the estimator, and is admitted once the first
completes); (c) confine --list/--status report the advisory-only containment
state honestly in shim mode, distinguishably from the real-slice case; (d) a
real aitest suite run completes successfully in shim mode end-to-end; (e)
`confine --memory-max/--memory-reserve/--delegate-ram` all parse successfully
and run the command in shim mode (no rejection); (f) shim-mode
`--delegate-ram` does NOT set `AIRA_AITEST_LIB` in the child env, proven by
inspecting the actual child environment, not by inspecting the flag-parsing
code path alone; (g) a shim-mode job with a child that forks a grandchild:
signalling the confine process delivers the signal to the grandchild too
(process-group signal), with a documented, tested exception for a
deliberately detached/setsid'd descendant; (h) a `docker build`-shaped
invocation of the build-time install step succeeds with no network access and
no running daemon/systemd present, and the separate start-time step is what
actually launches the shim daemon; (i) a shim-mode container run where the
daemon is left running in the background exits promptly when the workload
process exits, not blocked on the daemon.

## Resolution (in-review)

Built on `aira121-ci-shim-mode`. Plan:
`docs/superpowers/plans/2026-09-06-aira121-ci-shim-mode-plan.md`, published as
**v2** with all twelve gate conditions folded in (section 12 tabulates each
condition against the section it changed).

### The nine numbered requirements

1. **Capability probe + explicit mode.** `internal/install/capability.go`:
   `ProbeCapability` records systemd reachability, the unified cgroup, this
   process's own cgroup and its `memory.max`, and MemTotal — each as an
   established fact or an explicit `unevaluated: <reason>`. The mode decision
   reads exactly one field. `--ci` keeps AIRA-120's meaning unchanged, `--ci=shim`
   forces, `--ci=auto` opts in to host-dependence; there is no path through
   `install` that does not PRINT the resolved mode, its budget and its
   containment. The decision is recorded durably in
   `<state-home>/aira/install-mode.json`.
2. **No cgroup step attempted.** `confineShim`
   (`internal/runner/confine_shim_linux.go`) is entered from `confineWithDeps`
   after identity normalisation and BEFORE slice resolution — above every
   function that issues a cgroup syscall — so "skipped entirely up front" is
   structural, not a promise. The test harness makes every cgroup seam PANIC if
   called.
3. **Advisory-only ledger, reported honestly.** The existing `sliceQueue` IS the
   ledger; shim mode re-sources its three injectable seams
   (`internal/daemon/shim.go`). No new ledger data structure, and the AIRA-67
   per-signature estimator is reused by not editing it. A new always-rendered
   `containment=` facet carries
   `advisory(ci-shim,no-cgroup,no-kill-backstop)` vs `enforced` vs `unevaluated`,
   through the single `FormatConfineStatus` projection; `--list`'s reserve
   summary carries the same wording on the same line as the numbers.
4. **Budget source.** Declared (`--memory-max`) → container `memory.max` →
   `/proc/meminfo` MemTotal → **install fails**. The container's own limit is
   preferred over MemAvailable because `/proc/meminfo` is not namespaced; the
   reasoning is documented at `resolveShimBudget`.
5. **aitest's fallback needs no changes, proven.** Two new tests in
   `internal/pylib/aitest/test_supervisor.py` drive the exact wire answers a shim
   install produces and assert the fallback pool ran.
6. **Resource flags accepted, never rejected.** `parseConfineArgs` and
   `ConfineRequest` are untouched, and the shim branch sits BELOW both the parser
   and `ResolveConfineReserve`, so a shim-specific rejection has nowhere to live.
   `--memory-max`/`--memory-reserve` stay LIVE as ledger declarations and are
   inert only as cgroup writes.
7. **`AIRA_AITEST_LIB` not exported (INTERIM).** Gated by the one named predicate
   `runner.AitestBackendCanFunction`, which is the single line AIRA-123 flips. In
   shim mode the child's environment is actively STRIPPED of every
   `AIRA_AITEST_*`, and `AIRA_CONFINE_SCOPE_ID` is not published either.
8. **Process-group signals.** `Setpgid` on the shim launch plus
   `confineCommand.signal`, so `forwardConfineSignals` carries no mode branch at
   all. The setsid/double-fork escape is documented in the code, the SKILL text
   and the plan, and asserted by a NEGATIVE test.
9. **Build/start split.** `--stage=build|start`, defaulting to both. The build
   stage places bytes only; the start stage is the only thing that starts or
   contacts anything. The shim daemon is spawned setsid, with `/dev/null` stdin
   and a LOG FILE (never an inherited pipe) for stdout/stderr, `Release()`d and
   never waited on.

### The twelve gate conditions

- **C1** worker-admit answers `state=unavailable`/`class=admission-unusable`
  (not `unevaluated`, which is retriable and would make the aitest supervisor
  wait forever); `WorkerAdmitStateUnavailable` added to the poll loop's break
  set; `aitest-bootstrap` refuses in shim mode so `_disable_daemon` fires at
  bootstrap. Test (d) asserts the fallback pool ran and is bounded by a deadline.
- **C2** `resolveConfineManagementPath` returns the sentinel in shim mode;
  `runner.ShimConfineList` renders `--list` from the granted-waiter registry with
  the advisory wording; `runner.ShimConfineKill` REFUSES and names the supervisor
  PID to signal (ownership guard first, so no PID leaks); SKILL text updated.
- **C3** the mode decision precedes the euid branch, so a root shim install never
  reaches `runRootInstall`; `--stage=build` never reaches
  `installSystemDropins`/`enable-linger` in any mode; `reexecRequestFor` forwards
  `--ci=<value>` and `--stage=<value>`.
- **C4** new socket-based `waitShimDaemonReachable` over `daemon.Status`
  (`waitDaemonReachable` is pure `systemctl --user show`); the "≤10s" figure is
  corrected in the plan.
- **C5** `resolveDaemonConfineMode` reads `install-mode.json` in `Serve`, so the
  dispatcher spawn and a hand-run `daemon serve` also yield a shim daemon; env
  retained only as a validated override seam. A shim job whose daemon is down
  runs with `admission=unevaluated` on the trailer, and that is asserted.
- **C6** `--exclusive` refused in `admitConnection` before enqueue;
  `liveScopesKnown` untouched; the test asserts no scan-failure state is armed.
- **C7** an injectable `lookPath` seam establishes absence BEFORE the run, and
  the run is classified on its ANSWER not its exit status (so `degraded` is
  correctly reachable); unit tests for systemctl-absent, timeout-absent and
  D-Bus-failure.
- **C8** `--memory-max` accepted under `--ci=shim` as the declared budget
  (`shim_budget_source=declared`), refused under `--ci=auto`; residual 6
  corrected.
- **C9** shim `onSignal` delivers to the group synchronously, THEN starts the
  grace, THEN escalates; no group signal after `cmd.Wait()`. Test (g) case 1 uses
  a grandchild whose SIGTERM handler takes measurable time.
- **C10** `ru_maxrss` feedback DROPPED; `peak-rss=unevaluated` in shim mode,
  recorded as residual 2.
- **C11** record at `<state-home>/aira/install-mode.json`, beside `state.db`.
- **C12** watchdog, slice ceiling, oom steerer AND the AIRA-72 scope reaper (with
  the stale-lease sweep it shares a pass with) are all off in shim mode,
  enumerated in `Serve`; the admission confine scan stays live and returns an
  honest empty success, so the log carries no periodic scan-failure noise.

### Found during the build, and fixed

A shim job whose descendant setsid's out of the process group would have HUNG the
supervisor at `cmd.Wait()`: `os/exec` bridges a non-file writer through a pipe
whose copy goroutine `Wait` joins, and the escaped descendant holds its write end
forever. The real path never sees this because `cgroup.kill` removes the escapee.
Shim mode now owns those pipes itself (`shimChildStream`) so the copier is
abandonable and the supervisor reports the job's real outcome and exits.

### Build-review round 2 — the BLOCK on PR #72, and what changed

Review of `07de08e` gated build/vet/test green and confirmed all nine
requirements and tests (a)–(i), but BLOCKed on two confirmed findings. Both are
fixed here, each with a regression test proven to FAIL against the old code.

**F1 — the declared-budget + unbounded-cgroup case was inoperable.** With a
DECLARED `--memory-max` budget and the container's own `memory.max = max` — the
multi-container-per-node case the flag exists for, GCP Batch with
`taskCountPerNode > 1` — `readShimMemory` delegated to `readSliceMemory`, which
refuses an unbounded limit as `"unbounded"`. It then fell through to host-wide
`/proc/meminfo`, which is NOT namespaced: `current` became `MemTotal -
MemAvailable` for the whole node against a 4 GiB budget, `checkedAvailable`
answered 0, and every job in the container got `E_ADMIT_TOO_LARGE
cap_minus_headroom=0` for the container's whole life. Fail-closed, so never a
false pass — but the feature's headline scenario did not work, and the refusal
read as misconfiguration.

`readSliceMemory` is now the unbounded REFUSAL wrapped around a new
`readSliceMemoryUsage`, which reads `memory.current` and the `memory.stat`
file-LRU discount and reports `limit == 0` for `max` instead of refusing. The
real path is byte-for-byte unchanged, and that is asserted inside the same test.
Shim mode reads the recorded cgroup whenever its `memory.current` reads, still
bounded by `min(live memory.max, recorded budget)`, and reaches meminfo only when
there is no own-cgroup reading at all. The routing is now on WHETHER a cgroup was
recorded, never on which source the budget came from — which is what made the
declared case fall off the path in the first place.

Alongside it, `ownCgroupDir` answers `""` — no recorded path — when the probe
established no cgroup of its own (no `/proc/self/cgroup`, or a cgroup-v1-only
host), rather than returning the cgroupfs mount root as a guess. The daemon then
honestly uses meminfo instead of reading a cgroup not known to be this
container's.

Tests: `TestReadShimMemoryUsesTheOwnCgroupWhenItsLimitIsUnbounded` (against the
old routing it fails with `current=22420447232` on this box),
`TestReadShimMemoryFallsBackToMeminfoWithNoOwnCgroup`,
`TestShimRecordsTheOwnCgroupPathForADeclaredBudgetOverAnUnboundedContainer`,
`TestShimRecordsNoCgroupPathWhenTheProbeEstablishedNone` (fails against the old
`ownCgroupDir`).

**F2 — the setsid negative test leaked a process on every run.**
`TestShimConfineSignalDoesNotReachASetsidDescendant` cleaned up with `pkill -f
<dir>/job.sh`, which matches the PARENT — already dead from the group signal —
while the escaped `grandchild.sh` busy-loop is, by the very property the test
asserts, reachable by nothing but its pid. Under an outer `aira confine` the
scope kill swept it up; a plain `go test` leaked one process per run. The
grandchild now records its pid before announcing readiness, the test kills THAT
pid, and the kill is CONFIRMED by `waitProcessGone` (a zombie counts as gone —
the process holds no memory or CPU and its reaping is its parent's business). The
confirmation IS the regression test: with the old `pkill` cleanup restored it
fails with "the escaped grandchild (pid …) is still running at the end of the
test".

**Non-blocking notes, all three fixed.**

- `reportShimMode` printed `4.00GiB (4294967296 bytes) (4294967296 bytes)` —
  `formatCeilingBytes` already appends the byte count. Duplicate removed, and
  asserted against in the install test above.
- The shim `onSignal` delivered to the group synchronously AND
  `forwardConfineSignals` delivered again after the callback returned. The shim
  path now constructs the forwarder with a nil `deliver`, so there is exactly one
  delivery; plan §4 (requirement 8) is corrected, having planned the second send
  as "a harmless duplicate". **Accepted coverage gap, written down rather than
  left silent:** no automated test can distinguish one send from two, because
  non-realtime signals coalesce in the kernel — a second SIGTERM arriving while
  the first is pending is discarded, which is exactly why the reviewer's probes
  observed a single delivery. Pinning it would need a new injectable delivery
  seam in production code existing only for the assertion, which is not worth it
  for a strict removal of a redundant send. It is covered by code reading and by
  the existing forwarding tests, which still assert the one delivery arrives.
- The `Setpgid` comment claimed Setsid was avoided to keep an interactive job's
  tty stdin working. That reasoning is wrong: a non-foreground process group
  reading its controlling tty earns SIGTTIN either way. The comment now gives the
  real reason — reach: a job in its own session is out of range of
  `kill(-pgid, …)`, which is precisely the escape the negative test documents.

**Requirement 5's optional follow-up, noted explicitly as the reviewer asked.**
Requirement 5's *required* half is done and tested (aitest's daemon-down fallback
needs no changes, proven by two tests). Its optional half — letting aitest's
worker-count `auto` sizing consult the shim RAM ledger when a daemon IS present
in shim mode, instead of staying deliberately CPU-only-sized — is NOT built here
and is carried by **AIRA-123**, the ticket that gives `worker-admit` a degraded
ledger-only admission mode. Sizing `auto` off that ledger is the natural
increment on top of AIRA-123 and has nothing to consult until it lands.

**Coverage gap carried forward, accepted by the reviewer.** No single automated
test composes install → real shim daemon → `aira confine` admission over the
socket. Each seam is tested separately, and the composed path was verified by an
isolated-HOME manual e2e (build/start stages, real daemon, confine with
`--memory-max`/`--memory-reserve`, `--delegate-ram` child env stripped of
`AIRA_AITEST_*`, `--exclusive` refused, `--list` advisory wording, `--kill`
refusal naming the supervisor PID under its ownership guard, SIGTERM/SIGINT
forwarded to the group).

### Deliberate deferral, reported rather than silently taken

`aira run` is NOT supported in shim mode: it refuses with
`E_RUN_SCOPE_UNAVAILABLE` naming `aira confine`, and the follow-up is
**AIRA-129**. The plan's section 5.7 recorded this fork as a decision point; it
was taken because `Runner.Launch`'s ledger/telemetry/PTY/detach surface is all
keyed on a real scope and the deployment shape this ticket targets does not use
it. Ticket test (a) therefore covers `confine` only.

### Build-review round 3 — the BLOCK on PR #72, and what changed

**F3 — the meminfo fallback was inoperable for the ticket's own headline
targets.** `readShimMemory`'s host-wide `/proc/meminfo` fall-back (reached
whenever there is no readable own-cgroup `memory.current` — `CgroupPath ""`
from the probe, or a cgroup-v1-only host with no memory controller file, e.g.
Amazon Linux 2 / legacy Fargate for AWS Batch) computed `current` as host-wide
`MemTotal - MemAvailable` while `maximum` stayed the container-scoped declared
or cgroup-derived budget. `checkedAvailable`'s `charge = max(current,
outstanding)` then pinned to the host-wide figure: on any multi-tenant node
where host usage exceeds the small declared budget (the normal state of such a
node), `available` was permanently 0 and every shim job answered
`E_ADMIT_TOO_LARGE cap_minus_headroom=0` for the container's entire life.
Fail-closed, never a false pass, but silently and permanently inoperable
rather than a transient contention wait.

**Fix direction chosen: pair `current` with the budget's own scope
(reviewer's preferred option), not an install-time refusal.** `readShimMemory`
now routes on the budget's `Source`, not merely on whether a cgroup path was
recorded: the meminfo fall-back's `current = MemTotal - MemAvailable`
computation is reached ONLY when the budget's own source is ITSELF host-wide
(`ShimBudgetSourceMemTotal` — both sides of `checkedAvailable` are then the
same scope, the AIRA-120 shape, which already worked end to end). For a
CONTAINER-scoped budget (`ShimBudgetSourceDeclared` or
`ShimBudgetSourceCgroupMemoryMax`) with no own-cgroup usage reading,
`readShimMemory` now reports `current=0, reclaimable=0` — booked-reserve-only
admission against the declared/derived budget. `checkedAvailable` itself is
untouched: its `outstanding`-charge logic still correctly gates on whatever
this ledger has actually granted; it is simply honest that it cannot see usage
outside what this ledger itself booked, which is the true state of knowledge
with no readable own-cgroup number. The install-time-refusal alternative was
rejected because it would turn away exactly the documented cgroup-v1-only
targets this ticket names, where booked-reserve-only still lets admission
function.

Tests (`internal/daemon/shim_test.go`):
`TestReadShimMemoryReportsBookedReserveOnlyForADeclaredBudgetWithNoOwnCgroup`
(a declared 2GiB budget against an injected 56GiB of host-wide usage; fails
against the old routing with `current=60129542144`, which zeroes
`checkedAvailable`) and
`TestReadShimMemoryReportsBookedReserveOnlyWhenTheRecordedCgroupHasNoMemoryController`
(the same fix for a recorded cgroup path with no `memory.current` file — the
cgroup-v1-only shape — rather than an empty path). Both were run against the
pre-fix code and confirmed to fail before the fix landed.

**Also fixed: the sibling fallback test was porous.**
`TestReadShimMemoryFallsBackToMeminfoWithNoOwnCgroup` only asserted
`maximum==budget` and `current>=0 && reclaimable==0`, `t.Skip`-ing outright
whenever the real host's `/proc/meminfo` made the fall-back inconvenient to
exercise — it passed against the broken routing on any host with light memory
pressure and proved nothing about admission actually working. `readMemTotal`
and `readMemAvailable` are now reached through two new injectable seams on
`Server` (`shimReadMemTotal`, `shimReadMemAvailable`, defaulting to the package
funcs on the same nil-checks-to-default rule as `admitResolveSlice` /
`admitReadMemory`; test setter `SetShimMeminfoForTest`). The test now injects a
deterministic MemTotal/MemAvailable pair and asserts
`checkedAvailable(current, maximum, reclaimable, 0, 0) > 0`, matching the round-2
F1 test's assertion style, instead of skipping.

**Non-blocking note, taken.** `internal/runner/confine_mode.go`'s
`InstallModeFileEnv` (`AIRA_INSTALL_MODE_FILE`) comment is corrected: this env
override is a same-user test/tooling convenience, not a security boundary —
any process that could set the env var could equally write or replace the
record file it names, so pointing it at a crafted record is not a bypass of
anything the record's contents are relied on to prove. The override itself is
unchanged (kept for tests and tooling, as before).

**Non-blocking notes already resolved by the round-2 commit, reconfirmed
untouched.** `reportShimMode`'s doubled byte count
(`internal/install/mode.go`) and the `Setpgid`/tty comment
(`internal/runner/confine_shim_linux.go`) were fixed in `a1248a9` and verified
still correct; no further change needed for either.

Build `aira confine -- go build ./...`, vet `aira confine -- go vet ./...`,
and the full suite `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1`
all green (exit 0) after this round's changes.

### Build-review round 4 — the BLOCK on PR #72, and what changed

**F4 — the shim budget floor did not mirror the real path's, so an entirely
ordinary container size wedged every job forever.** `resolveShimBudget`
(`internal/install/mode.go`) applied NO floor to either of its first two
sources: the declared path went through `sizeBytes` → `validateSize(...,
floor=false)`, and the cgroup-derived path parsed any positive `memory.max`.
The real (non-shim) `--ci` path floors `MemoryMax` at 4GiB specifically to
avoid this (`minimumCeilingGiB=4`, `resolveCIMemoryMax`,
`internal/install/install.go`), but the shipped shim path mirrored only the
unevaluated-MemAvailable half of that refusal, not the floor half — exactly
what the plan's own §4.2 said should happen ("mirrors AIRA-120's
`resolveCIMemoryMax` refusal ... failing at install is one loud failure in one
place, versus a per-job wedge later").

The daemon's admission headroom is 2GiB base + 64MiB/job
(`admitSliceHeadroomBaseDefault`/`admitSliceHeadroomSupervisorDefault`,
`internal/daemon/admit.go`); `checkedAvailable` answers a bare 0 whenever
`maximum <= headroom`. So any declared or cgroup-derived budget at or below
roughly 2GiB — an entirely ordinary CI/k8s-Job container size, needing no
unusual operator input — installed cleanly, printed a healthy-looking "shim
ledger budget: 2.00GiB ... advisory admission ledger active" message, and then
refused EVERY job forever with `E_ADMIT_TOO_LARGE cap_minus_headroom=0`.
Fail-closed, never a false pass, but silently and permanently inoperable —
the same shape of bug as F3, one layer up.

**Fix.** `resolveShimBudget` now refuses any declared or cgroup-derived budget
below the existing `minimumCeilingGiB` (4GiB) constant, in the same
`E_INSTALL_UNAVAILABLE` refusal class `resolveCIMemoryMax` uses for its own
floor refusal (not `E_INSTALL_ARGUMENT_INVALID`, even for the declared-budget
case, per the reviewer's own framing of this as an environment-usability
refusal rather than a malformed-argument one). Each message names the
offending value and the floor; the cgroup-derived message additionally points
at `--memory-max` as the workaround, since an operator does not always control
how the surrounding container runtime sized the container's own cgroup. The
weakest source, the `meminfo-memtotal` fallback, is deliberately NOT floored
here — it already reports the whole host's memory, which is essentially never
below 4GiB, and flooring it was not asked for by the finding.

Tests (`internal/install/ci_shim_mode_test.go`):
`TestShimInstallRefusesADeclaredBudgetBelowTheFloor` and
`TestShimInstallRefusesACgroupDerivedBudgetBelowTheFloor` (both confirmed to
FAIL against the pre-fix `resolveShimBudget`, which accepted a 2GiB budget from
either source with no error), and
`TestShimInstallAcceptsABudgetExactlyAtTheFloor` (both sources, at exactly
4GiB, must still install — the fix refuses BELOW the floor, never AT it).

**The F3 tests adjusted to stay realistic.** The round-3 tests
`TestReadShimMemoryReportsBookedReserveOnlyForADeclaredBudgetWithNoOwnCgroup`
and `TestReadShimMemoryReportsBookedReserveOnlyWhenTheRecordedCgroupHasNoMemoryController`
(`internal/daemon/shim_test.go`) used a 2GiB budget and called `readShimMemory`
and `checkedAvailable` directly with `headroom=0`, bypassing install and the
real evaluator's production headroom entirely — which is exactly why they kept
passing even though, after this fix, a 2GiB budget can no longer be installed
at all. Both are bumped to an 8GiB budget (above the new floor, and matching
the reviewer's own reproduction's choice of value) so they continue to test a
realistic post-fix scenario rather than a smaller instance of the same gap;
their assertions and the injected host-wide usage (64GiB total / 8GiB
available) are otherwise unchanged, since the property under test — a
container-scoped budget must not be zeroed by unrelated host-wide usage — does
not depend on the budget's exact size.

**Plan doc corrected.** §4.3 of
`docs/superpowers/plans/2026-09-06-aira121-ci-shim-mode-plan.md` still
described the PRE-F3 routing ("selected by whether a cgroup was recorded, NOT
by which source the budget came from") without ever recording F3's own
routing correction (onto the budget's Source, not merely cgroup-path
presence) — round 1's F1 correction had been folded in, F3's was not. Added a
matching correction note in the same place and style as the F1 one, and
updated the bulleted behaviour list to describe the shipped post-F3 routing.
Residual 7 added to plan §10 for the one-sentence note below.

**Non-blocking note, taken.** On a container sized exactly at the new 4GiB
floor, an UNPINNED first job (no `--memory-reserve`, no per-project peak-RSS
history) also gets `E_ADMIT_TOO_LARGE`: the no-history default reserve is
itself 4GiB (`runner.DefaultConfineMemoryReserve`,
`internal/runner/confine.go:19`), leaving nothing above the ceiling once
headroom is subtracted. Accepted rather than fixed — it is exact parity with a
real 4GiB `--ci` slice, which refuses the identical unpinned request the
identical way, and the ticket's own consumer invocation already pins
`--memory-reserve 512M` on every job. Documented as plan §10 residual 7 rather
than left implicit.

Build `aira confine -- go build ./...`, vet `aira confine -- go vet ./...`,
and the full suite `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1`
all green (exit 0) after this round's changes.

