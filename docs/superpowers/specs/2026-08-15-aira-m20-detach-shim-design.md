# M20 — Detached-run supervisor shim (`aira run --detach`)

Status: PLAN v2 (post Sol round 1). Phase 5. Resolves the design-authority
"decide shim-vs-daemon first" open question (§21) and delivers `--detach`
(§14 line 153).

Design authority: [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §5.2
(daemon-optional runtime), §14 (subprocess runner), §20/§21. Builds on the
foreground runner (M12/M16/M17/M18a/M18b/M19), the run-kill ownership guard
(#27), and the memory-admission gate (#29).

**v2 changelog (Sol round 1):** SupervisorPID is now **load-bearing** — Reconcile
consults supervisor liveness as a durable lease (fixes queued-run-mislabelled-lost
and Reconcile-races-live-shim). Two-phase **readiness+ACK** handshake replaces the
timing-assumption timeout (shim cancels the reserved run on missing ACK).
Kill-intent is honoured during the admission wait. Post-leader-exit **quiesce**
(wait scope-empty) precedes usage/digest/terminal. `--merge` uses one shared
open-file description; the shim closes its capture-fd copies after Start. Every
failure terminalizes through the single existing path. Detach **structurally
skips** M19 wiring (not merely flag-rejection).

---

## 1. Goal and the one decision this milestone makes

`aira run --detach -- <argv>` starts a run that **outlives the launching
process**: the caller gets a run handle immediately and exits, while the child
runs under AIRA's kill scope, its output captured, its exit faithfully recorded.
This is the foundational Phase-5 piece — "everything else in the runner hangs off
it" (§14 line 153).

### 1.1 The shim-vs-daemon decision (resolved: shim, daemonless)

Design authority, §5.2: **"Core correctness never requires a running service."**
The design already names the answer twice — a **"tiny per-run supervisor shim
(`aira` re-execing itself)… this works daemonless; the daemon is the
alternative"** (§14 line 153), "a tiny per-run supervisor *shim* (works
daemonless) **or** the daemon" (§5.2).

**Decision: v1 detach ships the per-run shim, daemonless.** Each detached run is
owned by its own short-lived `aira` supervisor process that lives exactly as long
as the child needs supervising. There is no long-running service.

Rationale / reconciliation with the roadmap's earlier "daemon-as-supervisor" note:

- **Daemonless is the AIRA invariant** (§5.2). Gating `--detach` on a daemon would
  break a fresh worktree / sub-agent launching a detached run.
- **The shim is strictly smaller and crash-simpler**: one process per run, no
  shared service state, no reaper. Crash recovery is the *existing* `Reconcile`
  path (§6), augmented with a supervisor-liveness lease.
- **The daemon is the deferred *alternative*** (§9 D4): load-once session, `aira
  watch`, continuous reconciler, heartbeat reaper, and the cross-session
  **admission fairness-queue** #29 deferred. It later *adopts/queues* detached
  runs; the shim is the daemonless floor it optimises.

**Non-goals (deferred, §9):** the daemon; `run-input` (§14 line 156); the
admission fairness-queue; detached **telemetry auto-wiring** (M19 flags → M20b).

---

## 2. Architecture: launcher + shim, single-writer-in-the-shim

```
 aira run --detach -- <argv>              (LAUNCHER — the user's process)
   │  detach-specific flag-compat checks (synchronous, §5)
   │  create readyPipe(shim→launcher) + ackPipe(launcher→shim)
   │  write control file (0600, req JSON)      outputDir/detach-<nonce>.ctrl
   │  spawn  aira __supervise --control <p> --ready-fd 3 --ack-fd 4
   │           SysProcAttr{Setsid:true}; stdio→/dev/null; ExtraFiles=[readyW,ackR]
   │  read readiness on readyR (UX-bounded)
   │      failure {code} → surface, exit nonzero
   │      success {id}   → write ACK byte on ackW → print handle → exit 0
   │      timeout/EOF    → close ackW WITHOUT the byte (signals cancel) → E_RUN_DETACH_FAILED
   ▼
 aira __supervise --control <path>         (SHIM — the per-run supervisor)
   read+delete control file
   Launch(ctx, req, detach=true, ready/ack fds)          ← ALL ledger writes here
     validate argv/cwd/env/prefix
       pre-reserve failure → relay {code} on readyW, exit  (no id reserved)
     reserveID → append "starting" (Detached=true, SupervisorPID=self)
     → signal readiness {id} on readyW
     → BLOCK for ACK on ackR:
         ACK byte  → proceed
         EOF/no-byte (launcher gave up or died) → terminalize CANCELLED, exit
     → admission gate (may wait; polls kill-intent, ctx-cancellable)
     → RE-READ kill-intent under run lock; present → terminalize KILLED (no child)
     → scope Create → append "scope-created"
     → RE-READ kill-intent under run lock; present → terminalize KILLED, remove scope
     → dup2 capture files → clone3 child INTO scope → Start → append "running"
        (record child PIDIdentity; SupervisorPID already durable from "starting")
     → close the shim's own capture-fd copies
     → waitpid(leader) → QUIESCE: wait scope populated=0 (bounded; cgroup.kill on
        timeout) → snapshot cgroup usage → finalise OutputRefs (digest+complete)
        → appendTerminalLocked → remove empty scope
   exit
```

**Single-writer-in-the-shim.** Every ledger mutation for a detached run happens
in the shim, as every mutation for a foreground run happens in the run process.
The launcher writes nothing to the ledger; it is a pure spawner-and-relay. This
keeps the durable state machine identical to foreground.

**The shim is outside the kill scope.** The shim *creates* the scope and clone3's
only the **child** into it (`SysProcAttr{UseCgroupFD, CgroupFD: scope.FD()}`). The
shim itself never enters the scope. `run-kill` cgroup-kills the scope → kills the
child, not the shim → the shim's `waitpid` returns → it records `killed`. This is
why "a systemd scope is not a supervisor" (§14).

### 2.1 The two-phase readiness+ACK handshake (correctness, not timing)

The launcher must return **promptly** even though admission may wait up to
`admission_max_wait` (default 30 min). The handshake makes the launcher's view and
the shim's action **always consistent**, with no reliance on a timing assumption:

1. **Launcher**: detach-specific flag-compat checks (§5); create two pipes; spawn
   the shim; wait to read a readiness message on `readyR`.
2. **Shim** runs `Launch` (single-home argv/cwd/env/prefix validation). A
   **pre-reserve** validation failure is relayed `{code,error}` on `readyW`
   **before** any id is reserved → surfaced synchronously, **no ledger id
   reserved**. On success: reserveID → append `starting` (`Detached=true`,
   `SupervisorPID=self`) → write readiness `{id}` on `readyW` → **block reading
   `ackR`**.
3. **Launcher** on `{id}`: write **one ACK byte** on `ackW`, print the handle
   `{id,status:starting}`, exit 0. On a failure `{code}`: surface it, exit nonzero.
   On a UX-bounded timeout **or** shim EOF-without-message: **close `ackW`
   without the byte** and return `E_RUN_DETACH_FAILED`.
4. **Shim** reading `ackR`: an ACK byte → proceed to admission/Start. **EOF or no
   byte** (launcher timed out, crashed, or otherwise gave up) → **terminalize the
   reserved run as `cancelled`** (a clean terminal via the standard path; **no
   child is ever started**, no scope created) → exit.

Consequences:

- **No false-negative orphan** (P0-3 fixed). If the launcher times out and closes
  `ackW` without the byte, the shim terminalizes the reserved run and starts
  nothing — the launcher's "failed" and the ledger's "cancelled" agree.
- **No timing-assumption correctness** (P1-6 fixed). The launcher's timeout is a
  **UX bound only**; correctness is the ACK. A generous default (config
  `run.detach_ready_timeout`, default 60 s) makes a spurious cancel of a healthy
  run vanishingly unlikely, and even then it is *consistent* (cancelled, no
  orphan), never a silent orphan.
- **Launcher-crash-after-read** is handled: the OS closes `ackW` on the launcher's
  death → the shim sees EOF-without-byte → cancels. The user got no handle and the
  run did not proceed — consistent.

`cancelled` is a new terminal status (`Status.Terminal()==true`) distinct from
`lost`: it means "the launch handshake was abandoned before the child started; no
child ran." It carries `U_RUN_DETACH_CANCELLED`, never a fake exit code.

### 2.2 The `__supervise` subcommand and daemonisation

`aira __supervise --control <path> --ready-fd <n> --ack-fd <m>` is a **hidden**
subcommand (prefixed `__`, excluded from generated help, MCP schemas, and the
agent guide — not a user verb). Invoked with a missing/malformed control file it
fails `E_RUN_ARGUMENT_INVALID` with a non-success readiness message. It builds the
same core/runner as any command and calls `Launch(detach)`.

Daemonisation: the launcher spawns the shim with `SysProcAttr{Setsid: true}` and
stdio → `/dev/null` (new session, no controlling terminal, not SIGHUP'd on
launcher exit). `readyW`/`ackR` are passed via `ExtraFiles` (fds 3/4); the shim
closes `readyW` after signalling and `ackR` after reading. `selfExe` =
`/proc/self/exe`.

### 2.3 The control file

The launcher serialises the resolved `Request` to JSON (`argv`, `cwd`, env
overrides, `ticket`/`phase`/`label`, `merge`, `realtime`, `stdin_path`,
`store_stdin`, `timeout`, `grace`, `prefix`, `no_admit`) `0600` to
`outputDir/detach-<nonce>.ctrl`. Env overrides may carry secrets → mode `0600`,
and the **shim deletes the control file immediately after reading it** (before
Start). The shim inherits the launcher's ambient environment (it is the
launcher's child), so `effectiveEnvironment(req.Env)` in the shim resolves the
identical child environment and `env_digest` — no separate env snapshot.

---

## 3. Capture model: dup2-direct, one merge OFD, quiesce before finalise

Per §14 line 153, detached runs **`dup2` the child's stdout/stderr directly onto
the capture files at exec** — no pipes, drain goroutines, or M17 live tee. This is
mandated and simpler/robust: a briefly-stalled shim can never backpressure or
SIGPIPE the child, and the shim's only job between Start and leader-exit is to
hold nothing and `waitpid`.

In `Launch`'s detach branch:

- `openOutputs(dir, id, merge)` opens the capture files. **`--merge` opens ONE
  file and sets `cmd.Stdout == cmd.Stderr` to the *same* `*os.File`** — Go dup2's
  the one fd onto both child fd 1 and 2, so they share a single **open-file
  description** and the kernel interleaves in real order (a real `dup2(err→out)`).
  It must never be two independent opens of the same path (independent offsets →
  interleaving corruption).
- **No** `setupPipes`/`setupPTYCapture`, drain goroutines, `liveStreams`, or
  `collectCapture` on the detach path.
- **After Start the shim closes its own copies of the capture `*os.File`s** — the
  child holds its dup'd fds; the shim keeps nothing open (no fd leak, no
  "file busy"). At terminal the shim re-opens each file to `stat` + `digestFile`.
- **Quiesce before finalise** (P1-5 fixed): with dup2-direct there is **no pipe
  EOF barrier**, so a descendant that inherited the capture fd may keep writing
  after the leader exits. After `waitpid(leader)` the shim **waits (bounded) for
  the scope to become empty** (`populated==0`, reusing the M18b quiesce with the
  bounded run-kill ctx); on timeout it best-effort `cgroup.kill`s and re-waits.
  **Only once the scope is empty** does it snapshot usage, digest, and mark
  `OutputComplete`. If it cannot reach empty (a D-state residual, tracked with
  #20), it terminalizes with `OutputPartial` + `U_RUN_CAPTURE_INCOMPLETE` and
  **does not** publish a `complete` digest — no false "complete".
- Byte cap: detached capture is "complete up to the disk-eviction cap"; per-read
  head+tail elision (§19) applies at **read** time via `run-log` (unchanged). If
  eviction/truncation ever touches the file, the ref is reported `evicted`/
  `unavailable` (existing `U_RUN_OUTPUT_UNAVAILABLE`), never a fake `complete`. A
  hard mid-run truncation cap is deferred (§9 D2).

**Buffering / lifecycle flag compatibility** — see §5.

---

## 4. Durable protocol changes

Additive `RunRecord` fields (JSON-additive; legacy records normalise to zero on
read, like `buffering`/`admission`):

- `Detached bool` — set in the `starting` record on the detach path and carried
  forward. Distinguishes the run for reporting, `run-log --follow` semantics, and
  Reconcile's lease check (§6).
- `SupervisorPID PIDIdentity` — the **shim's own** pid + start-tick, recorded in
  the `starting` record (so it is present for the entire life of a detached run,
  including the pre-scope admission window). **Load-bearing** (Sol P0-1/P0-4):
  Reconcile consults its liveness as a durable supervisor **lease** (§6).
  (`PIDIdentity` continues to hold the **child** leader's identity, unchanged.)

New terminal status `cancelled` (`Status.Terminal()==true`): the launch handshake
was abandoned before any child started (§2.1). Distinct from `lost` (a child
*may* have run but its exit is unknown) — `cancelled` asserts **no child ran**.

`mergeEvidence` carries all three **monotonically** (the field-loss trap —
M16/#27): `if candidate.Detached { base.Detached = true }`; carry `SupervisorPID`
when `candidate.SupervisorPID.PID != 0`. Regression test: a candidate omitting
each field must not clobber a set base.

`Request` gains `Detach bool`. No other runner-config surface changes beyond the
new `run.detach_ready_timeout` config (§2.1).

---

## 5. Flag-compat + wiring suppression (launcher-synchronous + structural)

Rejected combinations, all `E_RUN_ARGUMENT_INVALID`, evaluated in the launcher
before any spawn (synchronous, no id reserved):

| Combination | Reason |
|---|---|
| `--detach --follow` | opposite lifecycle modes (§14 `[--follow\|--detach]`) |
| `--detach --pty` | pty needs a live master reader |
| `--detach --stdin -` | caller exits; no live stdin |
| `--detach` + any M19 wiring flag | detached wiring deferred (§9 D1) — reject, not silently drop |

M19 wiring flags: `--report`, `--report-stream`, `--suite`, `--shard`, `--retry`,
`--usage`, `--provider`, `--tool`, `--config-env`, `--strict-wiring`. Permitted
with `--detach`: `--ticket`, `--phase`, `--label` (pure record metadata),
`--merge`, `--realtime` (pure env; no-op for a file sink; `buffering` recorded
honestly), `--no-stdin`, `--stdin <file>`, `--no-admit`, `--timeout`, `--prefix`.

**Structural wiring suppression (Sol P2).** Flag rejection is necessary but not
sufficient — the M19 wiring lives in the **core run handler** (run_wiring.go),
which runs *after* Launch on the foreground path. The detach path must **not reach
that block at all**:

- The core `run` handler branches on `detach` **before** the wiring block: it
  spawns the shim, prints the handle, and **returns without calling
  `wireTerminalRun`**. There is no code path by which a detached launcher invokes
  M19 wiring.
- The shim's `__supervise` path calls `runner.Launch` (which itself never wires —
  wiring is a core-handler concern) and **does not** invoke `wireTerminalRun`.
- All wiring *activation sources* are resolved and rejected **before readiness**:
  the flags above, plus any config/env default that would set a report format,
  tool, or usage source. The detached handle reports telemetry
  **`unevaluated`** explicitly (never silently absent), matching AIRA honesty.

---

## 6. Crash recovery: Reconcile + a supervisor-liveness lease

The v1 change (Sol P0-1/P0-4): **Reconcile treats an alive supervisor as a durable
lease** and does not terminalize a detached run out from under it. For a
**detached, non-terminal** run, Reconcile (under the per-run flock, after
re-read) evaluates:

1. If `SupervisorPID` is **alive** (`processLive` — pid + start-tick, PID-reuse
   safe, the existing leader-liveness primitive): **preserve** (`running`/
   `starting`). The shim is still admitting, launching, or about to write the
   exact terminal — Reconcile must not race it. This covers both the pre-scope
   admission window and the post-exit "about to write terminal" window.
2. If `SupervisorPID` is **dead**: apply the existing scope-emptiness
   terminalization — open `CgroupScope`; scope non-empty → preserve `running`
   (child outlived a dead shim, now unsupervised; a later reconcile, once the
   child exits and the scope empties, marks it `lost`); scope empty/absent → mark
   `lost` + `U_RUN_EXIT_UNKNOWN` (no fake exit code; `peak_rss` unevaluated). If a
   kill-intent was recorded and the scope kill completes, `killed` as today.

For a **foreground** run, Reconcile is unchanged (no `SupervisorPID`, scope-
emptiness authority as today).

Scenario table (detached):

| Scenario | Supervisor | Scope | Reconcile | Honest? |
|---|---|---|---|---|
| Queued in admission | alive | absent | preserve `starting` | yes — no false lost (P0-1) |
| Running normally | alive | non-empty | preserve `running` | yes |
| About to write terminal (child just exited) | alive | empty | **preserve** (lease) — shim wins the CAS with the real exit | yes — no false lost (P0-4) |
| Shim died, child running | dead | non-empty | preserve `running` (unsupervised) → later `lost` when scope empties | yes |
| Shim died, child exited | dead | empty | `lost` + `U_RUN_EXIT_UNKNOWN` | yes |
| Shim died mid-prep (pre-scope) | dead | absent | `lost` | yes |
| Handshake abandoned | (terminalized by shim) | — | `cancelled` + `U_RUN_DETACH_CANCELLED` | yes — asserts no child ran |

Because the child writes directly to the capture files (no drain), an unsupervised
child loses **no output** while running; only its exit code + cgroup usage are
lost, which is exactly what `lost` + `U_RUN_EXIT_UNKNOWN` + `peak_rss=unevaluated`
represent. No fabricated number is ever written. The descendant-migration residual
(#20) is unchanged by detach.

### 6.1 Kill-intent during admission (Sol P0-2)

`run-kill` on a detached run in the admission window has no scope to cgroup-kill,
but records kill-intent under the run flock (existing `killWithIntent`). The shim
must honour it rather than launch the child:

- The shim's **admission loop polls** the current record each iteration
  (cross-process cancellable) and aborts if `KillIntent.Present`.
- The shim **re-reads kill-intent under the run flock immediately before scope
  `Create` and again immediately before `Start`**; if present, it terminalizes
  `killed` (via the standard path, §6.2) and starts **no** child (removing any
  scope it created). Once the scope exists and the child has started, ordinary
  `run-kill` cgroup-kills it and the shim's `waitpid` records `killed` as usual.

### 6.2 One terminalization path (Sol P1-8)

Every shim failure/abort — pre-reserve validation, missing ACK (`cancelled`),
kill-intent abort (`killed`), admission timeout, output-open, scope-create, Start,
timeout setup — terminalizes through the **existing**
`failBeforeLaunch`/`appendTerminalLocked` machinery (which `Launch` already
routes foreground failures through), never an ad-hoc direct append. The detach
additions (ACK-cancel, kill-intent-abort) reuse this single deferred cleanup path.
A dedicated test exercises it.

---

## 7. run-kill and run-log on a detached run (already work)

- **`aira run-kill <id>`** reads the record, opens `CgroupScope`, cgroup-kills
  (TERM→grace→KILL) with the #27 ownership guard. For a detached run this kills
  the child; the shim's `waitpid` returns and its terminal CAS carries the
  recorded `KillIntent` → `killed`. The #27 ownership guard is unchanged (foreign
  worktree refused without `--steal`). The admission-window case is §6.1.
- **`aira run-log <id> [--stream …] [--tail/--head/--grep/--since] [--follow]`**
  re-reads the same stored bytes against the live-growing dup2-direct file;
  `--follow` waits until terminal (`ReadOutput.Follow`); `--since <offset>` gives
  incremental reads while the run is live. No changes required.

---

## 8. Faces and routing

- **CLI:** `run` verbSpec gains `boolSpec("detach", …)`; `main.go` maps `--detach`;
  the core handler branches to the launcher path (which returns before wiring, §5).
  Handle `{id, status:starting}`; `--json` emits the same `RunRecord`-shaped
  handle.
- **MCP:** `aira_run` gains `detach` (bool); returns the handle immediately
  (status `starting`); the caller polls `aira_run_output`/`aira_get`. Dispatch
  metadata + default-fill + delimiter want-maps updated (the M19 parity checklist).
- **`__supervise`, `--ready-fd`, `--ack-fd`:** hidden — asserted absent from
  generated CLI help, MCP schema, and the agent guide; present only as an internal
  dispatch entry.
- Skill: one `run --detach` example; MCP/skill parity tests updated.

---

## 9. Deferrals (explicit)

- **D1 — Detached telemetry auto-wiring (M20b).** The shim runs the M19 wiring
  after the terminal CAS (it already builds the core/store). v1 **rejects** the
  wiring flags and **structurally skips** the wiring block (§5); telemetry is
  reported `unevaluated`. No honesty gap.
- **D2 — Hard mid-run capture cap for dup2-direct.** v1 is "complete up to the
  disk-eviction cap"; read-time §19 elision applies. Shim-enforced truncation with
  a head+tail marker is a follow-up.
- **D3 — `aira run-input`.** Deferred to post-shim by §14 line 156; unblocked but
  out of scope — needs a per-run control plane and has no dated pain.
- **D4 — The daemon.** Load-once session, `watch`, continuous reconciler, reaper,
  cross-session admission fairness-queue (#29). The shim is the daemonless floor;
  the daemon is the later optional alternative (§1.1).
- **D5 — Non-Linux.** The runner is Linux-only (`runner_stub.go`); `--detach`
  follows suit (the stub rejects it with the existing unsupported-platform code).

---

## 10. Tests and the Sol build-review checklist

TDD. Real-cgroup integration tests run under `AIRA_REAL_CGROUP=1 whale-run` (the
sandbox cannot; Opus verifies on real hardware). A committed executable
real-binary e2e (`~/tmp/aira-m20-e2e.sh`) drives the real CLI
`config→launcher→shim→runner→ledger` path — the load-bearing check the Go seam
tests structurally cannot see.

**Correctness properties (each must fail against a plausible wrong impl):**

1. **Detach returns promptly with a handle**; the launcher process exits while the
   child is still running (discriminator: launcher pid gone, child pid in the
   scope); `get` shows `starting`→`running`→`exited` over time.
2. **Shim outside the scope**: `run-kill` kills the child but the shim records
   `killed` (not `lost`/`exited`); the shim pid is never in `cgroup.procs`.
3. **dup2-direct faithful + complete**: stdout/stderr land in the capture files;
   `--merge` interleaves in kernel order via one OFD; digest+`complete` set at
   terminal **after** scope-empty quiesce; a lingering descendant delays finalise
   and, if it cannot be quiesced, yields `partial`+`U_RUN_CAPTURE_INCOMPLETE`
   (no false complete).
4. **Supervisor-lease Reconcile** (P0-1/P0-4): (a) a run parked in admission with a
   **live** shim is **preserved**, not lost; (b) Reconcile racing a live shim
   right after child-exit **preserves** and the shim writes the real `exit_code`
   (Reconcile must not win with `lost`); (c) a **dead** shim + empty scope → `lost`
   + `U_RUN_EXIT_UNKNOWN`, no fake exit/usage; (d) dead shim + non-empty scope →
   preserved then `lost` when it empties.
5. **Kill-intent during admission** (P0-2): `run-kill` while the shim waits on
   admission → the shim starts **no** child and records `killed`; re-check before
   Start closes the create/Start race.
6. **Two-phase ACK** (P0-3): launcher timeout/EOF → shim sees missing ACK →
   terminalizes `cancelled` + `U_RUN_DETACH_CANCELLED`, **no child started**;
   normal ACK → run proceeds. Launcher-crash-after-read → shim cancels.
7. **Normal-path faithfulness**: exit N → `exit_code=N`, `status=exited`,
   `peak_rss`/`cpu_*` from cgroup-at-exit (real-cgroup), `terminal_complete=true`.
8. **Flag rejection matrix** (§5): each rejected combo → `E_RUN_ARGUMENT_INVALID`
   synchronously, **no id reserved**; wiring block unreachable on detach.
9. **mergeEvidence monotonic carry** of `Detached`/`SupervisorPID`.
10. **Control file hygiene**: `0600`; deleted by the shim before Start (absent
    after the run starts).
11. **`__supervise` hidden** from help/MCP/agent-guide; MCP/skill parity for
    `detach`.

**Sol build-review checklist (adversarial, false-fail AND false-pass):**

- **Supervisor lease**: Reconcile never terminalizes a detached run whose
  `SupervisorPID` is alive; a dead-shim + scope-empty is the only `lost` path; the
  live-shim-vs-Reconcile post-exit race resolves to the shim's real terminal.
- **Handshake**: readiness after `starting`; the shim blocks for ACK and cancels
  (no child) on EOF/no-byte; correctness independent of the UX timeout; ready/ack
  fds closed (no launcher hang); pre-reserve failures reserve no id.
- **Kill-intent in admission**: cancellable admission; re-read under flock before
  Create and before Start; killed run starts no child.
- **Single-writer**: launcher writes zero ledger events; all mutations are the
  shim's; no two-process race on `starting`/`running`/terminal.
- **Capture**: one shared merge OFD; shim closes its capture-fd copies after
  Start; quiesce (scope-empty) precedes usage/digest/complete; eviction/truncation
  → unavailable/partial, never fake complete; no drain/live-tee path reachable
  under detach.
- **Scope containment of the shim**: prove the shim is never a member of the child
  scope; a clone3-into-cgroup failure leaves no junk (routes through the single
  terminalization path).
- **`lost` honesty**: no path writes a fabricated exit/usage; `cancelled` asserts
  no child ran; `lost` vs `killed` vs `exited` never mislabelled for a detached
  run.
- **One terminalization path**: every failure/abort uses
  `failBeforeLaunch`/`appendTerminalLocked`, never a direct append.
- **Wiring suppression**: no config/env/default/reused-Launch path activates M19
  wiring under detach; telemetry reported `unevaluated`.
- **Porous-test check**: every real-cgroup test fails (not skips) under
  `AIRA_REAL_CGROUP=1`; the load-bearing discriminator is launcher-exited-while-
  child-runs and Reconcile-preserves-under-live-lease.

**New stable codes:** `E_RUN_DETACH_FAILED` (supervisor unspawnable or no
readiness), `U_RUN_DETACH_CANCELLED` (handshake abandoned, no child ran),
`U_RUN_CAPTURE_INCOMPLETE` (dup2-direct capture could not quiesce). All other
failures reuse the existing `E_RUN_*`/`U_RUN_*` taxonomy via the readiness relay.
