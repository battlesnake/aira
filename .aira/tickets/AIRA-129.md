---
{"schema":1,"id":"AIRA-129","project":"aira","title":"aira run: ci-shim support (AIRA-121 deferred it; only aira confine has a shim path)","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["ci","confine","run"],"hold":false,"relations":[]}
---
Follow-up recorded by AIRA-121 (ci-shim mode for systemd/cgroup-unavailable
containers), taken as an explicit, written decision rather than a silent trim —
AIRA-121's plan section 5.7 and residual 5 name this fork.

AIRA-121 gave `aira confine` an advisory ci-shim launch path: no cgroup scope,
admission gated by an in-daemon RAM-budget ledger, honest `containment=advisory`
reporting, and process-GROUP signal forwarding. `aira run` did NOT get the same
treatment and REFUSES in shim mode with `E_RUN_SCOPE_UNAVAILABLE`, naming
`aira confine` as the shim-capable verb.

Why it was deferred rather than built:

- `Runner.Launch` carries a far larger surface than `confineWithDeps` — the
  project run ledger, telemetry, PTY, `--detach`, per-run scope caps, output
  capture, and the AIRA-20 descendant-escape attestation — and every one of the
  cgroup-derived facets is keyed on a scope that does not exist in shim mode.
- The deployment shape AIRA-121 exists for (a GCP Batch container running
  `aira confine -- make <gate>`) does not use `aira run` at all.
- Attempting the launch and failing at `backend.Create` deep inside
  `Runner.Launch` would leave a half-written run record behind, which is worse
  than refusing at the door.

What this ticket should build:

1. A shim launch path for `Runner.Launch` mirroring `confineShim`: no scope
   creation attempted up front, admission through the same ledger, `Setpgid` +
   process-group signal forwarding with the same deliver-then-grace-then-SIGKILL
   ordering, and the same owned-stdio treatment so a setsid'd descendant cannot
   block the supervisor's wait.
2. Every cgroup-derived facet on `RunRecord` reporting its ESTABLISHED
   unevaluated value, plus the `containment=advisory(...)` projection, so a run
   record from a shim box is distinguishable from a real-slice one at a glance
   rather than looking like a failed run.
3. A decision, written down, on `--detach` in shim mode: the detached supervisor
   currently mints its own scope id and binds it, which still works, but its
   `--status` reporting and the AIRA-72 reaper's assumptions must be re-checked
   against a mode with no scopes.
4. Tests in the same shape AIRA-121 used: a `newBackend`-panics harness proving
   no cgroup seam is reached, and a real end-to-end run in shim mode.

## Resolution

Built. `aira run` has a ci-shim launch path; the blanket
`E_RUN_SCOPE_UNAVAILABLE` refusal is gone.

### 1. The shim launch path

`internal/runner/runner_shim_linux.go`. `Launch` branches to `launchShim` after
every argument/environment/cwd/prefix/identity check and BEFORE
`backend.Probe`, so every cgroup seam lives on the other side of the branch and
"skipped entirely, never attempted-and-failed" is structural rather than a
promise.

Shared by CALLING, never by copying: `r.admit` (the same admission ledger),
`openOutputs` / `setupStdin` / `setupPipes` / `setupPTYCapture` / `drain` /
`collectCapture` / `collectPTYCapture`, `startDeadlineSource`,
`killWithIntentUsing`, `confineSignalSource` / `forwardConfineSignals` /
`confineCommand`, and `r.append` / `appendTerminalLocked` / `mergeEvidence`.

Signal forwarding is `confineShim`'s, with `confineCommand{group: true}`:
`Setpgid`, deliver-the-received-signal-then-grace-then-SIGKILL (AIRA-121 gate
condition C9's ordering, so a job's own handler gets to run), one delivery per
received signal, and `markReaped` as the pgid-recycling cut-off. Under `--pty`
the child gets `Setsid` instead — `setpgid(2)` fails with `EPERM` on a session
leader, and `setsid` already yields a group whose pgid is the child's pid, so
the group reach is unchanged. The forwarder is installed BEFORE the launch: in
this mode it is the sole delivery path, so a signal in the window between
`Start` and installation would kill the supervisor by default disposition and
orphan the whole job tree.

Owned stdio needed no equivalent of `shimChildStream`: `setupPipes` already
hands the child `*os.File` pipe ends, so `os/exec` creates no copier,
`cmd.Wait()` joins nothing, and `collectCapture`/`collectPTYCapture` abandon the
drain on a bound. A setsid'd descendant holding a write end costs one grace
period, not a hang.

### 2. Cgroup-derived facets, and the projection

Facets and their established ci-shim values: `cgroup_scope` empty (and Reconcile
and Kill key off exactly that emptiness); `scope_integrity` a new
`ScopeAdvisory` ("advisory"), set on the record's first line and never
reclassified; `peak_rss` / `cpu_user` / `cpu_sys` nil (so nothing is fed to the
AIRA-67 estimator — the residual `confineShim` already records);
`scope_memory_max` / `scope_memory_high` nil with the request preserved as a new
`U_RUN_SCOPE_CAP_UNENFORCED`; `descendant_escape` nil; no scope-monitor,
teardown-attestation or OOM classification. A `--cpu-timeout` is recorded as
`U_RUN_CPU_BUDGET_UNENFORCED`, which that code's own catalogue entry already
names ("a shim mode where there IS no cgroup").

The projection is a new `RunRecord.Containment`, carrying
`advisory(ci-shim,no-cgroup,no-kill-backstop)` and written by the shim path
alone. The real path deliberately leaves it EMPTY rather than stamping
`enforced`: `enforced` means "a per-job scope under a FINITE-CAPPED slice", and
`aira run` has never checked the slice cap, so claiming it would be a new
containment claim this verb does not verify.

`ScopeAdvisory` is admissible for command gates (`admissibleScopeIntegrity`) and
for `CleanSuccess`, on the argument the existing `ScopeUnverified` relaxation
already makes: the verdict rests on the leader's exit code and a
completely-captured output, both established identically here. Refusing it would
make every gate on a shim box permanently unevaluated in the one deployment
shape AIRA-121 exists for.

### 3. `--detach` in ci-shim mode: REFUSED, explicitly

`aira run --detach` refuses with `E_RUN_SCOPE_UNAVAILABLE`, naming
`aira confine --detach` as the alternative, and refuses at the door — before an
ID is reserved or a `starting` event appended, so no half-written record is left
(asserted by a test).

The reason is not effort. A detached run is one whose supervisor is not the
process you later talk to, and `run kill` / `reconcile` / crash recovery all
reach it through its cgroup: a durable named object any process can open and
`cgroup.kill`, whose emptiness is a two-sided proof. ci-shim has none. The only
reach a shim launch has is `kill(-pgid)` from the supervisor that made the
group, and a pgid is a recycled pid, so handing it to a second process turns
`run kill` into "signal whatever owns that group now". Detaching would produce a
run that can be started and never honestly stopped, killed, or reconciled.

The same reasoning makes cross-process `Runner.Kill` refuse in shim mode, also
before publishing an intent (publishing one nothing can satisfy would drive the
launching supervisor into `U_RUN_RECONCILE_REQUIRED` for a kill never
attempted). Ending a shim run means signalling its supervisor, which forwards to
the group and escalates.

Re-checked, as the ticket asks: the **AIRA-72 reaper** needs no change in either
mode. It sweeps orphaned `.aira-CONFINE-*` cgroup DIRECTORIES, and in ci-shim
mode neither `confine` nor `run` creates one, so its walk finds nothing — inert,
not wrong. `confine --detach` is unchanged: its supervisor's minted scope ID is
a pure identity (admission key, `--list` row, `--kill` target), never a cgroup
path, and its `--status` is the stored `ConfineStatus`, whose `Containment`
facet already reads `advisory`.

`Reconcile` skips `backend.Open` entirely for an advisory record — there is no
cgroup to open — but it still ASKS the question the real path puts to
`cgroup.procs`: is anything still running? The record answers it itself. The
`running` event carries the leader's `PIDIdentity`, and `processLive` is a
boot-aware, start-tick-checked observation of it, so a recycled pid cannot be
mistaken for the original leader. The three answers map onto the real path's
three:

- `processAlive` is the analogue of a non-empty scope — PRESERVE. It never
  escalates to a kill the way a non-empty scope under a kill intent does,
  because the only reach here is `kill(-pgid)` through the launching
  supervisor's own `confineCommand`, which holds the `markReaped` cut-off that
  makes that delivery safe. Reconcile has no such cut-off, so a kill from here
  could land on a reissued pgid belonging to a stranger.
- `processUnknown` is the analogue of an unreadable scope — PRESERVE, flagged
  `U_RUN_RECONCILE_REQUIRED`. Unestablished liveness is not proof of death, so
  it may not become a terminal. That is the disposition
  `reconcileDetachedLocked` already gives an unreadable supervisor.
- `processDead` is the analogue of an empty scope — the absent-scope branch, so
  an unfinished shim run becomes `lost`. That is honest here: the supervisor
  held the only reach and went with the leader, so there is no second party
  that could observe or end it.

Asking is load-bearing, not defensive; the first version did not, and the
build-review section below records what that cost.

### 4. Tests

`internal/runner/runner_shim_linux_test.go`, plus one in
`internal/store/gate_command_test.go`. `shimPanicBackend` panics on Probe,
Create AND Open — stricter than `unavailableBackend`, which returns an error
from Probe and so could not tell "skipped" from "attempted and recovered".

Each was verified non-porous by reverting the behaviour it covers:

- process-group reach — `confineCommand{group: false}` fails it (the grandchild
  survives and holds the capture pipe open past the grace);
- C9 signal ordering — sending SIGKILL before the received signal fails it (the
  grandchild's 0.3s TERM handler never completes);
- cgroup-free reconcile — removing the advisory branch panics on the backend;
- live-run reconcile and unestablished-leader reconcile (added by the
  build-review fix, below) — deleting the liveness switch fails both;
- gate admissibility — the two-value `admissibleScopeIntegrity` fails it.

### What was NOT built, and why

- `run --detach` and cross-process `run kill` in shim mode: refused with a
  written reason above, not deferred silently.
- No OOM classification: `memory.events` is a cgroup file. A container-runtime
  OOM kill is invisible to AIRA here and is NOT guessed at from the wait status.
  Accepted gap.
- The setsid'd-descendant escape is inherited from AIRA-121 unchanged: out of
  reach of any non-cgroup mechanism, documented rather than papered over.
- A SIGTERM-IGNORING IN-GROUP DESCENDANT can outlive a "completed" timeout kill.
  Named explicitly here rather than left inside the general "`Completed` ≠
  subtree empty" statement, because it is the one case where the survivor is
  reachable in principle. `shimGroupKill` sends the group SIGTERM and returns
  `Completed` as soon as the LEADER is proved dead within `termGrace`; the
  escalating group SIGKILL is never reached, and `markReaped` closes the group
  off the instant the leader is reaped, so no later delivery is possible either.
  Such a child survives with `CaptureForcedClosed` as its only trace, and no
  trace at all if it closed its stdio. It is NOT closed by sending the group
  SIGKILL first: `shimGroupKill`'s leading return exists because a pgid whose
  leader has been reaped may have been REISSUED, and nothing available here can
  distinguish "the original group still has members" from "the pgid was
  recycled". Signalling on that ambiguity could kill an unrelated job, which is
  strictly worse than a survivor. Accepted gap, recorded in the `shimGroupKill`
  doc comment as well as here. The real path has no such gap — `cgroup.kill` is
  a set operation on a durable named object.
- The daemon-down flock fallback resolves the CONFIGURED slice name rather than
  the ci-shim sentinel `confineShim` passes explicitly. In a genuine shim
  container no such cgroup path exists, so it answers `unevaluated` and the
  launch says so; `r.admit` was left untouched rather than refactored.

### Build review (PR #87 head b6b56cb) and its fixes

BLOCKed on one P1 and two P3s. All three are addressed.

**P1 (CONFIRMED, executable repro) — `Reconcile` terminalised a LIVE ci-shim
run.** The advisory branch set `openErr = errKillTargetAbsent` for EVERY
advisory record unconditionally, so
`decideReconcile(waitObserved=false, killIntent=false, scopeEmpty=true, false)`
turned a healthy, mid-flight run into `status=lost` +
`U_RUN_RECONCILE_REQUIRED`, reachable from the routine `aira check` and `aira
reconcile` verbs. The damage did not stop at the reconcile output: the launching
supervisor re-reads the ledger and honours any terminal it finds
(`latest.Status.Terminal()` in `runner_shim_linux.go`), so it then returned that
fabricated `lost` with `err == nil` and DISCARDED the run's real exit 0 and
complete capture. On the real path the same instant yields `PreserveOpen`,
because `scope.Empty()` is false — the shim branch had simply dropped the
question. The ticket's own justification ("the supervisor that held the only
reach is gone") was asserted, never checked, when the record carried a
boot-aware, start-tick-checked `PIDIdentity` all along.

Fixed by the three-arm liveness switch documented in section 3. Regression
tests, both verified non-porous by deleting the switch:
`TestShimRunReconcilePreservesALiveRun` (the reviewer's repro, made
deterministic: the child blocks on a marker file the test creates only AFTER
`Reconcile` returns, so the run is provably live at the moment of inspection,
and both halves are asserted — reconcile leaves it alone AND the launch still
lands its own exit 0 with a complete capture) and
`TestShimRunReconcilePreservesAnUnestablishedLeader` (a zero `PIDIdentity` is
preserved and flagged, and no `terminal` event is appended). The existing
abandoned-run test now supplies a cross-boot `PIDIdentity` — proof of death from
the kernel's own facts, needing no seam — and is renamed
`TestShimRunReconcileTerminalisesADeadLeaderWithoutTouchingTheBackend`.

**P3 — the PTY branch's `deliver(syscall.SIGKILL)` was a proven no-op.** The
wait goroutine calls `command.markReaped()` before publishing on `waitCh`, and
`confineCommand.signal` returns nil once reaped, so by the time that line ran
nothing could be delivered; the comment claiming it was "the equivalent" of the
real path's `cgroup.kill` quiesce was false. Removed, and the comment rewritten
to say what is actually true: ci-shim has NO equivalent, the candidate is not
merely weak but unreachable, and `collectPTYCapture`'s bounded abandon is the
only thing that terminates the drain.

**P3 — the SIGTERM-ignoring in-group descendant was not named as an accepted
gap.** Now named explicitly, above and in the `shimGroupKill` doc comment, with
the argument for accepting rather than closing it.

**Suite evidence (not a PR regression).** The reviewer's full-suite run exited 1
on `TestConfineScanReadsTheSupervisorCommandAndSaysUnevaluatedWhenItCannot`
(`internal/runner/confine_manage_linux_test.go`, AIRA-135, a file this PR does
not touch): the live `/proc` cmdline read raced the child's exec under full-suite
load, `Command:<nil>`, and it passed 3/3 in isolation. A pre-existing wall-clock
flake on master, in the same class as AIRA-20 and AIRA-112; it wants its own
ticket.

### Verification

Foreground, exact exit codes, on `origin/master` c7b3e32.

Original build:

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0

Re-run after the build-review fixes:

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0

Independent re-confirmation of the fix round, foreground, exact exit codes, same
three commands on the same base:

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0 (every
  package `ok`; the AIRA-135 `TestConfineScanReadsTheSupervisorCommand…` flake
  the reviewer hit did NOT recur on this run. That is what a wall-clock flake
  looks like from one sample — it does not retire the reviewer's observation,
  and that test still wants its own ticket.)

The non-porosity of the P1 regression tests was re-verified rather than taken on
the fix commit's word: deleting the `processLive` switch from `Reconcile`, so the
advisory branch falls straight through to `openErr = errKillTargetAbsent` (the
pre-fix behaviour), fails `TestShimRunReconcilePreservesALiveRun` and
`TestShimRunReconcilePreservesAnUnestablishedLeader`, while
`TestShimRunReconcileTerminalisesADeadLeaderWithoutTouchingTheBackend` still
passes. The two new tests discriminate exactly the wrong behaviour, and the
third is not a duplicate of either.
