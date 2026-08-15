# M20 — Detached-run supervisor shim (`aira run --detach`)

Status: PLAN (pre-review). Phase 5. Resolves the design-authority "decide
shim-vs-daemon first" open question (§21) and delivers `--detach` (§14 line 153).

Design authority: [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §5.2
(daemon-optional runtime), §14 (subprocess runner — Launch, Detach, resource
accounting), §20/§21 (phasing + open questions). Builds on the foreground
runner delivered by M12/M16/M17/M18a/M18b/M19 and the run-kill ownership guard
(#27) and admission gate (#29).

---

## 1. Goal and the one decision this milestone makes

`aira run --detach -- <argv>` starts a run that **outlives the launching
process**: the caller gets a run handle immediately and exits, while the child
keeps running under AIRA's kill scope, its output captured, and its exit
faithfully recorded. This is the foundational Phase-5 piece — the design says
"everything else in the runner hangs off it" (§14 line 153).

### 1.1 The shim-vs-daemon decision (resolved: shim, daemonless)

The design authority states the principle plainly (§5.2): **"Core correctness
never requires a running service."** A detached run is the *one* feature that
needs a supervisor process, and the design already names the answer twice — a
**"tiny per-run supervisor shim (`aira` re-execing itself)… this works
daemonless; the daemon is the alternative"** (§14 line 153), served by "a tiny
per-run supervisor *shim* (works daemonless) **or** the daemon" (§5.2).

**Decision: v1 detach ships the per-run shim, daemonless.** Each detached run is
owned by its own short-lived `aira` supervisor process that lives exactly as long
as the child. There is no long-running service.

Rationale, and reconciliation with the roadmap's earlier "daemon-as-supervisor"
note:

- **Daemonless is the AIRA invariant.** Gating `--detach` on a running daemon
  would violate §5.2 — a fresh worktree or sub-agent must be able to launch a
  detached run without first standing up a service.
- **The shim is strictly smaller and crash-simpler.** One process per run, no
  shared mutable service state, no reaper loop, no lifecycle to manage. Crash
  recovery is the *existing* `Reconcile` path (§6) — no new machinery.
- **The daemon is the *alternative*, deferred.** When the daemon lands (a later
  optional cut: load-once session, `aira watch`, continuous reconciler,
  heartbeat reaper, and the cross-session **admission fairness-queue** that #29
  deferred), it becomes an *alternative* supervisor form and can adopt/queue
  detached runs. Nothing in this milestone precludes it; the shim is the
  daemonless floor the daemon later optimises, exactly as the design frames it.

**Non-goals of M20 (deferred, see §9):** the daemon itself; `run-input` (live
stdin push — §14 line 156 already defers it to "post-shim"); the cross-session
admission fairness-queue; detached **telemetry auto-wiring** (the M19
`--report`/`--tool`/`--usage` flags — §9 D1).

---

## 2. Architecture: launcher + shim, single-writer-in-the-shim

Two `aira` processes cooperate for one detached run:

```
 aira run --detach -- <argv>          (the LAUNCHER — the user's process)
   │  validate request (pure, no side effects)
   │  write control file  (0600, req JSON)         outputDir/detach-<nonce>.ctrl
   │  spawn  aira __supervise --control <path> --ready-fd 3
   │           SysProcAttr{Setsid:true}; stdio→/dev/null; ExtraFiles=[readyW]
   │  read readiness on readyR  (bounded wait)
   │  print handle {id,status}  ·  exit 0     (writes NOTHING to the ledger)
   ▼
 aira __supervise --control <path>     (the SHIM — the per-run supervisor)
   read+delete control file
   runner.Launch(ctx, req, detach=true, readySignal)     ← ALL ledger writes here
     reserveID → append "starting" (Detached=true)
     → SIGNAL READINESS {id} on ready-fd, close it   ← launcher may now exit
     → admission gate (may wait) → scope Create → dup2 capture files
     → clone3 child INTO scope → append "running"
     → waitpid → cgroup usage-at-exit → TERMINAL CAS → remove empty scope
   exit
```

**Single-writer-in-the-shim.** Every ledger mutation for a detached run happens
in the shim, exactly as every mutation for a foreground run happens in the run
process. The launcher writes nothing to the ledger; it is a pure
spawner-and-relay. This keeps the durable state machine identical to foreground
and avoids two-process ownership races.

**The shim is outside the kill scope.** The shim *creates* the scope and clone3's
only the **child** into it (`CLONE_INTO_CGROUP` via the existing
`SysProcAttr{UseCgroupFD, CgroupFD: scope.FD()}`). The shim itself never enters
the scope. `aira run-kill` cgroup-kills the scope → kills the child (and
descendants) but **not** the shim → the shim's `waitpid` returns → it records
`killed`. This is precisely why "a systemd scope is not a supervisor" (§14): the
shim, not the scope, holds the fds, waits, and writes the exit record.

### 2.1 The readiness handshake and why it is early

`--detach` must return **promptly** — but the admission gate (#29) can legitimately
wait up to `admission_max_wait` (default 30 min) for memory. Therefore readiness
is signalled **after `starting`, before admission/scope/Start**:

1. The **launcher** performs only the **detach-specific flag-compatibility**
   checks synchronously (§5 — e.g. `--detach --pty`), which cost nothing and
   need no runner state, then spawns the shim.
2. The **shim** runs `Launch`, whose head does the single-home argv/cwd/env/
   prefix validation. A **pre-reserve** validation failure is relayed as
   `{code, error}` on the ready-fd **before** any id is reserved, so the common
   user errors still surface **synchronously** to the launcher (sub-second) with
   the existing `E_RUN_*` codes and leave **no** ledger id reserved — validation
   is not duplicated across processes.
3. On success the shim reserves the id and appends `starting`, then **signals
   readiness `{id}`** and closes the ready-fd. This is sub-second work (a counter
   reserve + one ledger append).
4. The launcher reads readiness, prints `{id, status: starting}` (or surfaces the
   relayed failure code), and exits.
5. The shim continues in the background: admission (may wait) → scope → Start →
   `running` → wait → terminal.

Consequence — **post-`starting` failures are asynchronous and honest.** If
admission times out, the scope cannot be created, or Start fails, the shim runs
the *existing* `failBeforeLaunch`/terminal machinery and records the failure
durably; the user observes it via `aira run ls` / `run-log` / `reconcile`, exactly
as a background job's late failure should surface. This is not a regression: a
foreground run that fails after launch is also only knowable by its record.

### 2.2 Readiness protocol

The ready-fd (inherited as fd 3 in the shim) carries a single JSON line, then EOF:

- success: `{"id":"RUN-5","status":"starting"}`
- pre-reserve failure: `{"code":"E_RUN_...","error":"<msg>"}`

The launcher reads to EOF and parses:

- valid success → print handle, exit 0.
- valid failure → surface `code` (non-zero exit), same taxonomy as foreground.
- **EOF with no message** (shim died before signalling) → `E_RUN_DETACH_FAILED`
  "supervisor exited before reporting readiness".
- **bounded readiness timeout** (`detachReadyTimeout`, constant 10 s — reserve +
  one append is far under this) → `E_RUN_DETACH_FAILED`. Documented residual: a
  shim that is merely *slow* to first-signal past 10 s is reported failed while
  its run may proceed; it then appears in `run ls`. This window is
  reserve+append only (admission/Start are *after* readiness), so it is not
  reachable by a legitimately slow (memory-waiting) run.

The launcher never kills the shim on timeout (the child may be legitimately
starting); it only declines to report the handle.

### 2.3 The `__supervise` subcommand

`aira __supervise --control <path> --ready-fd <n>` is a **hidden** subcommand
(prefixed `__`, excluded from generated help, MCP schemas, and the agent guide —
it is not a user verb). It is the shim entrypoint. Invoked with a missing or
malformed control file it fails with `E_RUN_ARGUMENT_INVALID` and a non-success
readiness message. It builds the same core/runner/store as any command (it needs
the runner to launch and the store for future wiring) and calls
`runner.Launch(..., detach)`.

**Daemonisation.** The launcher spawns the shim with `SysProcAttr{Setsid: true}`
and stdin/stdout/stderr redirected to `/dev/null`, so the shim is in a new
session with no controlling terminal and is not SIGHUP'd when the launcher exits.
The ready-fd is passed via `ExtraFiles` (fd 3) and closed by the shim
immediately after the readiness signal. `selfExe` is resolved from
`/proc/self/exe` (robust to `$PATH`).

### 2.4 The control file

The launcher serialises the resolved `Request` to JSON (`argv`, `cwd`, env
overrides, `ticket`/`phase`/`label`, `merge`, `realtime`, `stdin_path`,
`store_stdin`, `timeout`, `grace`, `prefix`, `no_admit`) and writes it
`0600` to `outputDir/detach-<nonce>.ctrl` (machine-local, gitignored). The env
overrides may carry secrets, so: mode `0600`, and the **shim deletes the control
file immediately after reading it** (before Start). The shim inherits the
launcher's ambient environment (it is a child of the launcher), so
`effectiveEnvironment(req.Env)` in the shim resolves the identical child
environment and `env_digest` the launcher would have computed — no separate env
snapshot is needed.

---

## 3. Capture model: dup2-direct, no live tee

Per §14 line 153, detached runs **`dup2` the child's stdout/stderr directly onto
the capture files at exec** — they do **not** use pipes, drain goroutines, or the
M17 live tee. This is both mandated and *simpler and more robust*: with the
child writing straight to the files via the kernel, a briefly-stalled shim can
never backpressure or SIGPIPE the child (the failure mode §14 warns of), and the
shim's only job between Start and exit is `waitpid`.

Concretely, in `Launch`'s detach branch:

- `openOutputs(dir, id, merge)` opens the same capture files (`RUN-n.out`/`.err`,
  or `RUN-n.log` under `--merge`).
- Set `cmd.Stdout` / `cmd.Stderr` **directly to the capture `*os.File`s**
  (Go dup2's them at exec). `--merge` → both point at the one `log` file — a
  real `dup2(err→out)` ordering, same as foreground merge.
- **No** `setupPipes`/`setupPTYCapture`, **no** `drain` goroutines, **no**
  `liveStreams`, **no** `collectCapture`.
- `OutputRef.State` starts `OutputPartial`; at terminal the shim `stat`s +
  `digestFile`s each capture file and finalises the refs to `OutputComplete`
  with bytes+digest — the same finalisation discipline the foreground path
  applies, so gate/report consumers see identical `complete`+digest semantics.

**Byte cap.** The foreground per-run byte cap is enforced inside the drain;
dup2-direct has no drain, so a detached capture file is **complete up to the
disk-eviction cap** (§14 storage/eviction). The per-read head+tail elision and
§19 overflow discipline still apply at **read** time via `run-log` (unchanged —
`ReadOutput` already bounds and marks truncation). A hard shim-enforced
mid-run truncation cap is deferred (§9 D2); it is not a correctness property, and
eviction already bounds disk.

**Buffering tactic compatibility.**

- `--pty` is **rejected** with `--detach` (`E_RUN_ARGUMENT_INVALID`): a pty
  requires a live master reader, which a detached run has by definition none of.
- `--realtime` is permitted (it is pure child-env injection, no live reader
  needed) but is effectively a no-op for a non-tty file sink; `buffering` is
  still recorded honestly (`realtime` iff injection applied, else `none`), matching
  the existing "no-op elsewhere" honesty of M18a.
- `--follow` is **rejected** with `--detach` (`E_RUN_ARGUMENT_INVALID`): they are
  opposite lifecycle modes and the design lists them as alternatives
  (`[--follow|--detach]`, §14). Tailing a detached run is `aira run-log <id>
  --follow`, which already works (`ReadOutput.Follow` waits until terminal;
  incremental `--since` re-reads the live-growing file at any time).
- `--stdin -` (the caller's stdin) is **rejected** with `--detach`
  (`E_RUN_ARGUMENT_INVALID`): the caller exits, so there is no live stdin.
  `--no-stdin` and `--stdin <file>` are fine (the shim opens the file).

---

## 4. Durable protocol changes

Two additive `RunRecord` fields (JSON-additive; legacy records normalise to the
zero value on read, like `buffering`/`admission`):

- `Detached bool` (`json:"detached,omitempty"`) — set by `Launch`'s detach branch
  in the `starting` record and carried forward. Distinguishes the run for honest
  reporting and for `run-log --follow` semantics; correctness (see §6) does **not**
  depend on it.
- `SupervisorPID PIDIdentity` (`json:"supervisor_pid,omitempty"`) — the shim's own
  pid + start-tick, recorded on the `running` event. **Diagnostic only** in v1
  (Reconcile does not consult it — §6). It lets `run ls` show whether the
  supervisor is alive and gives a future daemon an adoption anchor. (`PIDIdentity`
  continues to hold the **child** leader's identity, unchanged.)

`mergeEvidence` must carry both **monotonically** (the recurring field-loss trap
— M16/#27): `if candidate.Detached { base.Detached = true }`; carry
`SupervisorPID` when `candidate.SupervisorPID.PID != 0`. Regression test covers a
candidate that omits each field not clobbering a set base.

`Request` gains `Detach bool`. No other runner-config surface changes.

---

## 5. Flag-compatibility validation (launcher, synchronous)

Rejected combinations, all `E_RUN_ARGUMENT_INVALID`, evaluated in the launcher
before any spawn (so they are synchronous and cost nothing):

| Combination | Reason |
|---|---|
| `--detach --follow` | opposite lifecycle modes (§14 `[--follow\|--detach]`) |
| `--detach --pty` | pty needs a live master reader |
| `--detach --stdin -` | caller exits; no live stdin |
| `--detach` + any M19 wiring flag (`--report`, `--report-stream`, `--suite`, `--shard`, `--retry`, `--usage`, `--provider`, `--tool`, `--config-env`, `--strict-wiring`) | detached telemetry wiring is deferred (§9 D1) — reject rather than silently drop |

Permitted with `--detach`: `--ticket`, `--phase`, `--label` (pure record
metadata, no post-run wiring), `--merge`, `--realtime`, `--no-stdin`,
`--stdin <file>`, `--no-admit`, `--timeout`, `--prefix`.

---

## 6. Crash recovery and the `lost` honesty (reuses existing Reconcile)

The central honesty property: **`Reconcile`'s terminalisation keys on
scope-emptiness, not on whether a supervising process is alive** (runner_linux.go
Reconcile — it opens `CgroupScope`, tests `Empty()`, and `decideReconcile`s). This
already covers every detached-shim crash mode correctly, with **no new
machinery**:

| Scenario | Scope state | Reconcile outcome | Honest? |
|---|---|---|---|
| A. shim alive, child running | non-empty | preserved `running` | yes |
| B. shim alive, child exited | (shim writes terminal) | `exited`/`killed`/`oom-killed` | yes |
| C. shim died (crash/reboot/OOM-of-shim), child still running | non-empty | preserved `running` (now **unsupervised**) — when the child later exits and the scope empties, the next Reconcile marks it `lost`+`U_RUN_EXIT_UNKNOWN`; the exit code is genuinely lost | yes (`lost` is exactly this state) |
| D. shim died after child exit, before terminal CAS | empty, no terminal | `lost`+`U_RUN_EXIT_UNKNOWN` | yes (narrow window; we truly cannot know the code) |

Key points:

- In the detach model there is **no drain** — the child writes directly to the
  capture files via its dup2'd fds, so scenario C loses *no* output while
  unsupervised (the kernel keeps writing to the files); only the *exit code* and
  *cgroup-usage-at-exit* are lost, which is exactly what `lost` +
  `U_RUN_EXIT_UNKNOWN` and `peak_rss=unevaluated` already represent. No fake exit
  code, no fake usage number is ever written.
- A shim killed between `starting` and Start leaves a non-terminal
  `starting`/`scope-created` record with an empty-or-absent scope → Reconcile
  marks it `lost` (the existing preserve/lost decision). Same as a foreground run
  process killed mid-prep.
- `SupervisorPID` is **not** consulted by v1 Reconcile — scope-emptiness is the
  authority and is already proven. (A future daemon could corroborate supervisor
  liveness to distinguish "unsupervised-but-running" from "running-normally"; out
  of scope.)
- The descendant-migration residual (#20) is unchanged by detach — a descendant
  that migrates out of the scope and outlives the leader is the same pre-existing
  containment residual, neither improved nor worsened here.

---

## 7. run-kill and run-log on a detached run (already work)

- **`aira run-kill <id>`** already reads the record, opens `CgroupScope`, and
  cgroup-kills it (TERM→grace→KILL) with the #27 ownership guard. For a detached
  run this kills the child; the shim's `waitpid` returns; the shim's terminal CAS
  carries the recorded `KillIntent` forward → status `killed` (not bare `exited`),
  same classification as foreground. The ownership guard (#27) applies unchanged:
  a foreign worktree is refused without `--steal`.
- **`aira run-log <id> [--stream …] [--tail/--head/--grep/--since] [--follow]`**
  re-reads the same stored bytes; it already works against a live-growing
  dup2-direct file. `--follow` waits until terminal (`ReadOutput.Follow`), and
  `--since <offset>` gives incremental reads while the run is live.

No changes to run-kill or run-log are required — they operate on the durable
`RunRecord` + capture files, which the shim maintains identically.

---

## 8. Faces and routing

- **CLI:** `aira run --detach` — the `run` verbSpec gains a `boolSpec("detach",
  …)`; `main.go` maps `--detach`; the launcher path branches on it. The launcher
  prints the handle (`{id, status}`) and returns; `--json` emits the same
  `RunRecord`-shaped handle (status `starting`).
- **MCP:** `aira_run` gains `detach` (bool). Because a detached run returns before
  the child completes, the MCP tool returns the handle immediately (status
  `starting`); the caller polls `aira_run_output`/`aira_get`. Dispatch-table
  metadata + default-fill + delimiter want-maps updated for the new arg (the M19
  parity checklist).
- **`__supervise`:** hidden — **not** added to the CLI help table, MCP schema, or
  agent guide. A guard test asserts it is absent from generated help/MCP/skill
  surfaces and present only as an internal dispatch entry.
- Skill examples: add one `run --detach` example; MCP/skill parity tests updated.

---

## 9. Deferrals (explicit)

- **D1 — Detached telemetry auto-wiring.** The M19 wiring (`--report`→
  AddTestReport, `--tool`→AddComputeEvent, `tests_green_observed`) currently runs
  in the *foreground* core handler after `Launch`. For a detached run it must run
  in the **shim** after the terminal CAS (the shim already builds the core/store).
  Deferred to **M20b** to keep this milestone on the supervisor mechanics; v1
  **rejects** the wiring flags with `--detach` (§5) rather than silently dropping
  them. No honesty gap — the rejection is explicit.
- **D2 — Hard mid-run capture cap for dup2-direct.** v1 is "complete up to the
  disk-eviction cap"; read-time §19 elision applies. A shim-enforced truncation
  cap with a head+tail marker is a follow-up.
- **D3 — `aira run-input` (live stdin push).** Already deferred to post-shim by
  §14 line 156; now unblocked (the shim exists) but out of scope for M20 — it
  needs a per-run control plane (socket/FIFO on the shim) and has no dated pain.
- **D4 — The daemon.** Load-once session, `aira watch`, continuous reconciler,
  heartbeat reaper, and the cross-session **admission fairness-queue** (#29's
  deferred ordered admission). The shim is the daemonless floor; the daemon is
  the later optional alternative (§1.1).
- **D5 — Non-Linux.** The runner is Linux-only (`runner_stub.go`); `--detach`
  follows suit — the stub rejects it with the existing unsupported-platform code.

---

## 10. Tests and the Sol build-review checklist

TDD. Real-cgroup integration tests run under `AIRA_REAL_CGROUP=1 whale-run`
(the sandbox cannot; Opus verifies on real hardware). A committed executable
real-binary e2e (`~/tmp/aira-m20-e2e.sh`) drives the real CLI
`config→launcher→shim→runner→ledger` path — the load-bearing check the Go seam
tests structurally cannot see (the recurring e2e lesson).

**Correctness properties to test (each must fail against a plausible wrong impl):**

1. **Detach returns promptly with a handle**, child continues; `run ls`/`get`
   shows `starting`→`running`→`exited` over time; the launcher process has exited
   while the child is still running (discriminator: launcher pid gone, child pid
   in the scope).
2. **The shim is outside the scope**: `run-kill` kills the child but the shim
   observes it and records `killed` (not `lost`, not `exited`); the shim was never
   in `cgroup.procs`.
3. **dup2-direct capture is faithful and complete**: child stdout/stderr land in
   `RUN-n.out`/`.err` (or merged `.log`), digest+`complete` set at terminal;
   `--merge` interleaves in kernel order.
4. **Crash → honest `lost`**: kill the shim while the child runs, let the child
   exit, then `reconcile` → `lost`+`U_RUN_EXIT_UNKNOWN`, `peak_rss` unevaluated,
   **no fake exit code**. (Scenario C/D.) A separate case: kill the shim between
   `starting` and Start → `lost`.
5. **exit code + usage faithful on the normal path**: a detached run that exits N
   records `exit_code=N`, `status=exited`, `peak_rss`/`cpu_*` from the cgroup at
   exit (real-cgroup), `terminal_complete=true`.
6. **Flag rejection matrix** (§5): each rejected combination →
   `E_RUN_ARGUMENT_INVALID`, synchronously, no ledger id reserved.
7. **Readiness failure surfacing**: a shim forced to fail before reserve →
   launcher surfaces the code; a shim killed before signalling → EOF →
   `E_RUN_DETACH_FAILED`.
8. **mergeEvidence monotonic carry** of `Detached` and `SupervisorPID` (a
   candidate omitting each does not clobber a set base).
9. **Control file hygiene**: mode `0600`; deleted by the shim before Start (assert
   absent after the run starts).
10. **`__supervise` is hidden**: absent from generated CLI help, MCP schema, and
    the agent guide; MCP/skill parity holds for the new `detach` arg.

**Sol build-review checklist (adversarial, false-fail AND false-pass):**

- **Single-writer**: confirm the launcher writes **zero** ledger events; all
  mutations are the shim's. No two-process race on `starting`/`running`/terminal.
- **Readiness ordering**: readiness is signalled **after** `starting` and
  **before** admission/Start, so a memory-waiting run never blocks the launcher;
  post-readiness failures are durably recorded and surfaced via the record, not
  lost.
- **Scope containment of the shim**: prove the shim is never a member of the
  child scope (so `run-kill` cannot kill it) and that it still reads
  cgroup-usage-at-exit and writes the terminal record from outside.
- **`lost` honesty**: no path writes a fabricated exit code or usage number for a
  supervisor-crash; scenario C preserves `running` then terminalises to `lost`
  only on scope-empty; scenario D is `U_RUN_EXIT_UNKNOWN`.
- **Capture**: dup2-direct wiring closes the parent's copies of the capture fds
  after Start (no fd leak keeping the file "busy"); terminal finalisation sets
  digest+`complete` identically to foreground; no drain/live-tee path is reachable
  under detach.
- **Daemonisation**: the shim survives launcher exit (Setsid, stdio→/dev/null, no
  controlling tty); the ready-fd is closed after signalling (no launcher hang on a
  never-closed pipe).
- **Reconcile reuse**: no detach-specific terminalisation logic duplicates or
  diverges from the existing Reconcile/`appendTerminalLocked` CAS; the flock per-run
  discipline is intact cross-process.
- **Porous-test check**: every real-cgroup test fails (not skips) when
  `AIRA_REAL_CGROUP=1`; discriminators genuinely distinguish detached-shim from
  foreground (launcher-exited-while-child-runs is the load-bearing one).

**New stable codes:** `E_RUN_DETACH_FAILED` (supervisor could not be spawned or
did not report readiness). All other failures reuse the existing `E_RUN_*` /
`U_RUN_*` taxonomy surfaced through the readiness relay.
