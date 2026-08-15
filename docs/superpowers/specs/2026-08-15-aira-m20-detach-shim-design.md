# M20 — Detached-run supervisor shim (`aira run --detach`)

Status: PLAN v4 (post Sol rounds 1–3). Phase 5. Resolves the design-authority
"decide shim-vs-daemon first" open question (§21) and delivers `--detach`
(§14 line 153).

Design authority: [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md) §5.2,
§14, §20/§21. Builds on the foreground runner (M12/M16/M17/M18a/M18b/M19), the
run-kill ownership guard (#27), and the memory-admission gate (#29).

**v4 changelog (Sol round 3):** a durable **`quiesce-forced`** event is written
**before** the forced `cgroup.kill`, so no finalization race can lose the forced
outcome (P0-1). A **single shared `finalizeDetachedTerminal`** (usage snapshot +
output digest + evidence) is used by both the shim and Reconcile, so whoever wins
the CAS produces an identical complete terminal (P0-2). The `leader-exited`
evidence gains a monotonic **`LeaderExitObserved`** flag with presence-bearing
`ExitCode`/`Signal`, and `mergeEvidence` carries it on the flag (a clean `exit 0`
no longer collapses to `lost` — P0-3). Liveness is **three-valued**
(alive/dead/**unknown**); boot_id read-failure → unknown→preserve, and →
`E_RUN_IDENTITY_UNAVAILABLE` at identity creation (P1). The stalled-supervisor
recovery claim is corrected to **manual** (no `--steal` reuse; fenced lease
deferred, P1). run-kill/Reconcile use **bounded** flock acquisition with
`U_RUN_LAUNCH_STALLED`, never a false kill over a stuck launch (P1).

**v3 changelog (Sol round 2):** the per-run flock is **held across
Create→Start→`running`** so run-kill can never inject intent into the launch race
(P0-1). The launcher **prints+flushes the handle before ACK** (P0-2). A durable
**`leader-exited` evidence event** (exit code + signal, written immediately after
`waitpid`) is the linchpin: terminalization — by the shim or by Reconcile —
always has the real exit and never races or fabricates (P0-4, P1 wedged-shim). A
forced-quiesce kill is recorded as **`ScopeDescendantKilled` + `U_RUN_QUIESCE_FORCED`**,
leader-exit kept as evidence, never a clean `exited` (P0-3). A detached run is
**never** terminalized over a non-empty scope (P0-4). `PIDIdentity` gains
`boot_id` and treats zombies as dead (P1). Pipe-close discipline + EPIPE-as-cancel
(P1). `cancelled` yields to an existing terminal or a recorded kill-intent (P2).

**v2 changelog (Sol round 1):** SupervisorPID load-bearing lease; two-phase
readiness+ACK; kill-intent honoured during admission; quiesce before finalise;
one shared merge OFD; single terminalization path; structural M19-wiring skip.

---

## 1. Goal and the one decision this milestone makes

`aira run --detach -- <argv>` starts a run that **outlives the launching
process**: the caller gets a run handle immediately and exits, while the child
runs under AIRA's kill scope, captured, its exit faithfully recorded. The
foundational Phase-5 piece — "everything else in the runner hangs off it"
(§14 line 153).

### 1.1 The shim-vs-daemon decision (resolved: shim, daemonless)

Design authority §5.2: **"Core correctness never requires a running service."**
§14 line 153 names the answer: a **"tiny per-run supervisor shim (`aira`
re-execing itself)… this works daemonless; the daemon is the alternative."**

**Decision: v1 detach ships the per-run shim, daemonless.** Each detached run is
owned by its own short-lived `aira` supervisor process that lives exactly as long
as the child needs supervising. No long-running service.

Reconciliation with the roadmap's earlier "daemon-as-supervisor" note: daemonless
is the AIRA invariant (§5.2); the shim is smaller and crash-simpler; the daemon is
the deferred *alternative* (§9 D4: load-once session, `watch`, continuous
reconciler, reaper, the #29 cross-session admission fairness-queue) that later
adopts/queues detached runs. The shim is the daemonless floor it optimises.

**Non-goals (deferred, §9):** the daemon; `run-input` (§14 line 156); the
admission fairness-queue; detached telemetry auto-wiring (M19 flags → M20b).

---

## 2. Architecture: launcher + shim, single-writer-in-the-shim

```
 aira run --detach -- <argv>                (LAUNCHER — the user's process)
   flag-compat checks (synchronous, §5)
   create readyPipe(shim→launcher) + ackPipe(launcher→shim)
   write control file (0600, req JSON)         outputDir/detach-<nonce>.ctrl
   spawn aira __supervise --control p --ready-fd 3 --ack-fd 4
     SysProcAttr{Setsid:true}; stdio→/dev/null; ExtraFiles=[readyW, ackR]
   CLOSE own readyW + ackR immediately (so readR/ackR EOF track shim/launcher death)
   read readiness on readyR (UX-bounded: run.detach_ready_timeout, default 60s)
     {code} failure  → surface, exit nonzero
     {id} success    → PRINT+FLUSH handle to stdout FIRST
                         ok  → write ACK byte on ackW → exit 0
                         err → close ackW WITHOUT byte → exit nonzero
     timeout | EOF-no-message → close ackW WITHOUT byte → E_RUN_DETACH_FAILED
   ─────────────────────────────────────────────────────────────────────────
 aira __supervise --control p               (SHIM — the per-run supervisor)
   read+delete control file
   Launch(ctx, req, detach=true, readyW=fd3, ackR=fd4)     ← ALL ledger writes
     validate argv/cwd/env/prefix
       pre-reserve failure → relay {code} on readyW (EPIPE→just exit; no id)
     reserveID → append "starting" (Detached=true, SupervisorPID=self{pid,tick,boot})
     write readiness {id} on readyW   (EPIPE/partial write → treat as missing ACK)
     BLOCK reading ackR:
       ACK byte → proceed
       EOF/no-byte → terminalize (CAS precedence: existing-terminal >
                     kill-intent→killed > cancelled) — NO child, NO scope → exit
     admission gate (may wait):
       loop polls current record; KillIntent.Present → terminalize KILLED → exit
     ┌── acquire per-run flock (held through "running") ─────────────────────┐
     │  RE-READ kill-intent; present → release flock → terminalize KILLED    │
     │  openOutputs (—merge: ONE shared OFD) → Create scope → "scope-created" │
     │  cmd dup2 capture files; UseCgroupFD → setupStdin → Start (clone3 in)  │
     │  placement verify → append "running" (child PIDIdentity)               │
     │  (Create/Start failure → release flock → failBeforeLaunch, remove scope)│
     └── release per-run flock ──────────────────────────────────────────────┘
     close the shim's own capture-fd copies
     waitpid(leader) → append durable "leader-exited" evidence {exit,signal}
     QUIESCE: wait scope populated=0 (bounded); timeout → cgroup.kill → re-wait
       empty → snapshot usage → finalise OutputRefs (digest+complete)
             → terminalize from evidence (+forced-quiesce integrity if killed)
             → remove empty scope
       never-empty (D-state) → stay NON-TERMINAL: OutputPartial +
             U_RUN_CAPTURE_INCOMPLETE; leader-exited evidence durable; run-kill can
             still target; Reconcile finalises from evidence when it empties
   exit
```

**Single-writer-in-the-shim.** Every ledger mutation for a detached run happens in
the shim, as every mutation for a foreground run happens in the run process. The
launcher writes nothing to the ledger.

**The shim is outside the kill scope.** It *creates* the scope and clone3's only
the **child** into it (`SysProcAttr{UseCgroupFD, CgroupFD: scope.FD()}`); it never
enters the scope. `run-kill` cgroup-kills the scope → kills the child, not the
shim → the shim's `waitpid` returns → it records `killed`.

### 2.1 The two-phase readiness+ACK handshake (correctness, not timing)

The launcher must return promptly even though admission may wait up to
`admission_max_wait` (30 min). The handshake keeps the launcher's view and the
shim's action **always consistent**, independent of any timing assumption:

- **Pipe hygiene (P1).** The launcher creates both pipes and passes only `readyW`
  (fd 3) and `ackR` (fd 4) to the shim via `ExtraFiles`; it **closes its own
  `readyW` and `ackR` immediately after spawn** so `readyR` EOFs on shim death and
  the shim's `ackR` EOFs on launcher death. The shim closes `readyW` after
  signalling and `ackR` after reading. `.lock`/pipe fds are `O_CLOEXEC` so the
  child inherits none.
- **Pre-reserve failure** relayed `{code,error}` on `readyW` **before** any id is
  reserved → synchronous, no ledger id. An EPIPE on this write (launcher already
  gone) → the shim just exits (nothing reserved).
- **Success:** shim appends `starting`, writes `{id}` on `readyW`, then **blocks
  reading `ackR`**. A failed/partial `{id}` write (EPIPE) is treated exactly like
  a missing ACK (below).
- **Launcher on `{id}`:** **print + flush the handle to stdout first** (P0-2); only
  on a *successful* flush write the ACK byte on `ackW`; a broken stdout (EPIPE)
  → close `ackW` without the byte. This guarantees the shim proceeds only if the
  user actually received the handle.
- **Shim on `ackR`:** an ACK byte → proceed to admission/Start. **EOF or no byte**
  (launcher timed out, crashed, broke stdout, or otherwise gave up — the OS closes
  `ackW` on launcher death) → **terminalize the reserved run** and start **no**
  child. Terminal cause precedence under the CAS (P2): an already-authoritative
  terminal wins (idempotent); else a recorded `KillIntent` → `killed` (no child);
  else `cancelled` + `U_RUN_DETACH_CANCELLED`.

Consequences: no false-negative orphan (the launcher's "failed" and the ledger's
`cancelled`/`killed` agree); the launcher's `detach_ready_timeout` is a **UX bound
only** (a spurious cancel of a healthy run is vanishingly unlikely and, if it
happens, is *consistent* — no silent orphan), never a correctness dependency.

`cancelled` is a new terminal status (`Status.Terminal()==true`) asserting **no
child started** — distinct from `lost` (a child *may* have run, exit unknown).

### 2.2 `__supervise` and daemonisation

`aira __supervise --control <path> --ready-fd <n> --ack-fd <m>` is a **hidden**
subcommand (`__`-prefixed; excluded from generated help, MCP schemas, agent
guide). Missing/malformed control file → `E_RUN_ARGUMENT_INVALID` + non-success
readiness. It builds the same core/runner and calls `Launch(detach)`.
Daemonisation: `SysProcAttr{Setsid: true}`, stdio→`/dev/null`; `selfExe` =
`/proc/self/exe`.

### 2.3 Control file

Launcher serialises the resolved `Request` to JSON `0600` at
`outputDir/detach-<nonce>.ctrl`; env overrides may carry secrets → the **shim
deletes it immediately after reading, before Start**. The shim inherits the
launcher's ambient env (it is the launcher's child), so `effectiveEnvironment`
resolves the identical child env + `env_digest` — no separate snapshot.

---

## 3. Capture model: dup2-direct, one merge OFD, quiesce before finalise

Per §14 line 153, detached runs **`dup2` the child's stdout/stderr directly onto
the capture files** — no pipes, drain goroutines, or M17 live tee. Simpler and
robust: a stalled shim cannot backpressure/SIGPIPE the child.

- `openOutputs`. **`--merge` opens ONE file and sets `cmd.Stdout == cmd.Stderr` to
  the *same* `*os.File`** — one shared open-file description; Go dup2's the one fd
  onto child fd 1 and 2 → kernel-ordered interleaving. Never two opens of one path.
- No `setupPipes`/PTY/drain/`liveStreams`/`collectCapture` on the detach path.
- **After Start the shim closes its own copies of the capture `*os.File`s** (the
  child holds its dup'd fds) — no fd leak, no "file busy". At terminal the shim
  re-opens to `stat` + `digestFile`.
- **`leader-exited` evidence, then quiesce, then finalise** (P0-3/P0-4). After
  `waitpid(leader)` the shim first appends a durable `leader-exited` evidence event
  (`LeaderExitObserved=true`, presence-bearing `exit_code`/`signal` — §4). Because
  dup2-direct has **no pipe-EOF barrier**, a descendant that inherited the capture
  fd may still be writing, so the shim **waits (bounded, reusing the M18b quiesce
  with the run-kill ctx) for the scope to become `populated==0`**. **It never
  terminalizes while the scope is non-empty** (P0-4):
  - Scope empties **cleanly** (no forced kill) → `finalizeDetachedTerminal`
    (below) → `exited N` (or `killed` if a `KillIntent` completed),
    `ScopeContained`.
  - Quiesce times out with descendants still present → **the shim durably appends
    a `quiesce-forced` event BEFORE issuing `cgroup.kill`** (Sol P0-1 r3), then
    kills and re-waits. Because the forced-quiesce fact is durable *before* the
    scope can empty, **any** finalizer (shim or a racing Reconcile) incorporates
    it: the outcome is `ScopeIntegrity = ScopeDescendantKilled` +
    `U_RUN_QUIESCE_FORCED`, leader exit preserved as **evidence** —
    `CleanSuccess()` is **false**. We never report a clean `exited` when we
    force-killed surviving workload (P0-3), and never *lose* the forced fact to a
    finalization race.
  - Scope **never** empties (a D-state uninterruptible descendant, #20 family) →
    the run stays **non-terminal** with the durable evidence + `OutputPartial` +
    `U_RUN_CAPTURE_INCOMPLETE`; it remains **killable** and a later Reconcile
    finalises from the evidence once the scope empties. Never a terminal `exited`
    over live descendants.
- **One shared finalizer** (Sol P0-2 r3): `finalizeDetachedTerminal(record, scope,
  evidence)` — used by **both** the shim (post-quiesce) and Reconcile (evidence +
  empty scope) — re-checks scope-empty under the per-run flock, snapshots cgroup
  usage (readable until the empty cgroup is removed), stats + `digestFile`s the
  capture files → `OutputComplete`, folds the `quiesce-forced` fact, builds the
  terminal from the evidence, and only then `appendTerminalLocked` + removes the
  empty scope. Whoever wins the CAS produces an **identical, complete** terminal
  — no usage/digest is discarded by a Reconcile that beats the shim. (A
  re-population between the empty-check and the CAS is closed by re-checking empty
  under the same held flock immediately before the CAS.)
- Byte cap: "complete up to the disk-eviction cap"; §19 head+tail elision applies
  at **read** time (`run-log`). Eviction/truncation → `evicted`/`unavailable`
  (`U_RUN_OUTPUT_UNAVAILABLE`), never fake `complete`. Hard mid-run cap → §9 D2.

---

## 4. Durable protocol changes

Additive `RunRecord` fields (JSON-additive; legacy → zero on read):

- `Detached bool` — set in `starting`; carried forward.
- `SupervisorPID PIDIdentity` — the **shim's own** pid + start-tick + **boot_id**,
  recorded in `starting` (present for the whole life incl. the pre-scope admission
  window). **Load-bearing**: Reconcile consults its liveness as a durable
  supervisor **lease** (§6). (`PIDIdentity` continues to hold the child leader.)

`PIDIdentity` gains `BootID string` (from `/proc/sys/kernel/random/boot_id`),
applied to **both** the leader and the supervisor (P1 r2). Liveness is now
**three-valued** `alive | dead | unknown` (Sol P1 r3), replacing the boolean:
- **alive** — boot_id present + matches the running kernel, pid present, `/proc`
  state not zombie(Z).
- **dead** — boot_id present but **mismatches** (survives a reboot / cross-boot PID
  collision), or the pid is absent, or `/proc` state is **zombie(Z)**.
- **unknown** — boot_id unreadable at check time, or the record's boot_id is empty
  (a legacy/pre-M20 record). Reconcile treats **unknown as preserve** (never
  fabricate `dead`) and surfaces it as **unevaluated**, never terminalizing on it.

At **identity creation** (the shim stamping `SupervisorPID` before `reserveID`), an
unreadable boot_id **aborts before reservation** with `E_RUN_IDENTITY_UNAVAILABLE`
(the lease would otherwise be unreliable; boot_id is virtually always readable, so
this is a safety net, not a common path).

Two new durable, **non-terminal** ledger event kinds:
- **`leader-exited`** — the authoritative exit fact, written immediately after
  `waitpid`, **before** quiesce/terminal: `LeaderExitObserved bool` (a **monotonic**
  presence flag) + presence-bearing `ExitCode *int` + `Signal string`. It lets
  either the shim (post-quiesce) or Reconcile (if the shim dies/wedges) finalise
  with the **real** exit rather than `U_RUN_EXIT_UNKNOWN`.
- **`quiesce-forced`** — written *before* a forced `cgroup.kill` (§3), so the
  forced-quiesce outcome cannot be lost to a finalization race.

New terminal status `cancelled` (`Status.Terminal()==true`): the launch handshake
was abandoned before any child started (§2.1); asserts **no child ran**.

`mergeEvidence` carries `Detached`, `SupervisorPID`, and — critically — the
leader-exit evidence **on the monotonic `LeaderExitObserved` flag, not on a
non-zero exit** (Sol P0-3 r3 — the recurring field-loss trap: a clean `exit 0`
would otherwise collapse to "no exit observed" and mislabel `lost`). When
`candidate.LeaderExitObserved`, `base` takes `LeaderExitObserved=true` and the
presence-bearing `ExitCode`/`Signal`. A replay + both-terminalizer test asserts
`exit 0` survives. `Request` gains `Detach bool`. New config
`run.detach_ready_timeout` (§2.1).

---

## 5. Flag-compat + structural wiring suppression

Rejected combinations, `E_RUN_ARGUMENT_INVALID`, in the launcher before any spawn
(synchronous, no id reserved):

| Combination | Reason |
|---|---|
| `--detach --follow` | opposite lifecycle modes (§14 `[--follow\|--detach]`) |
| `--detach --pty` | pty needs a live master reader |
| `--detach --stdin -` | caller exits; no live stdin |
| `--detach` + any M19 wiring flag | detached wiring deferred (§9 D1) — reject, not silently drop |

M19 wiring flags: `--report`, `--report-stream`, `--suite`, `--shard`, `--retry`,
`--usage`, `--provider`, `--tool`, `--config-env`, `--strict-wiring`. Permitted:
`--ticket`, `--phase`, `--label`, `--merge`, `--realtime` (pure env; no-op for a
file sink; `buffering` honest), `--no-stdin`, `--stdin <file>`, `--no-admit`,
`--timeout`, `--prefix`.

**Structural wiring suppression (P2 r1).** The M19 wiring lives in the core `run`
handler *after* Launch. The detach path must not reach it:

- The core handler branches on `detach` **before** the wiring block: spawn shim,
  print handle, **return without `wireTerminalRun`**.
- The shim's `__supervise` path calls `runner.Launch` (which never wires) and does
  not invoke `wireTerminalRun`.
- All wiring activation sources (the flags above **plus** any config/env default
  setting a report format, tool, or usage source) are resolved and rejected
  before readiness. Detached telemetry is reported **`unevaluated`** explicitly.

---

## 6. Crash recovery: Reconcile + supervisor lease + exit evidence

Reconcile, for a **detached, non-terminal** run (under the per-run flock, after
re-read), decides by the **`leader-exited` evidence first**, then the supervisor
lease:

1. **`leader-exited` evidence present** (the shim reached `waitpid`):
   - scope **empty** → **`finalizeDetachedTerminal`** (§3 — the shared finalizer:
     snapshot usage + stat/digest outputs + fold the `quiesce-forced` fact + build
     the terminal from the evidence, then CAS) — **regardless of supervisor
     liveness**. This avoids racing a live shim to a wrong `lost` (P0-4), finishes
     a shim that recorded the exit then wedged (P1), and (via the shared finalizer,
     Sol P0-2 r3) never discards usage/digest the shim would have collected. No
     fabricated exit.
   - scope **non-empty** → descendants still live → **preserve** (quiescing);
     `run-kill` can target it.
2. **No `leader-exited` evidence:**
   - `SupervisorPID` **alive** → **preserve** (admitting / launching / pre-
     waitpid). Narrow residual: alive shim + empty scope + no evidence (a shim
     wedged in the tiny window before it records the exit) → preserve **and
     surface `U_RUN_SUPERVISOR_STALLED`** on `reconcile`/`get` (never a fake exit).
     v1 has **no automated recovery** for this rare state (Sol P1 r3 — `--steal`
     only overrides *ownership* for a cgroup-kill; it kills neither the
     outside-scope shim nor an empty cgroup, so it is **not** recovery here);
     manual recovery is to kill the wedged shim (its pid is in `SupervisorPID`),
     after which Reconcile sees a dead supervisor + empty scope → `lost`. An
     expiring **fenced** lease + a guarded supervisor-abandon operation are
     deferred (§9 D4).
   - `SupervisorPID` **unknown** (boot_id unreadable / legacy record) → **preserve
     + unevaluated** (§4) — never terminalized on unknown liveness.
   - `SupervisorPID` **dead** → existing scope-emptiness terminalization: scope
     non-empty → preserve `running` (unsupervised child; later reconcile
     terminalizes when it empties, still no evidence → `lost`); scope empty/absent
     → `lost` + `U_RUN_EXIT_UNKNOWN` (no fake exit/usage; `peak_rss` unevaluated).

**Bounded lock acquisition** (Sol P1 r3). The shim holds the per-run flock across
the launch section (§6.1); `cmd.Start` returns after fork+execve dispatch (fast),
but a pathological `execve`/exe-lookup on a stuck FS (NFS/FUSE) could block it.
Therefore the operations that contend for the flock — **`run-kill` and
Reconcile** — acquire it with a **bounded** wait (non-blocking + deadline, the #29
`LOCK_NB` precedent); on timeout they return an honest diagnostic
`U_RUN_LAUNCH_STALLED` (never a false kill-success, never a fabricated terminal,
never an infinite block). `run-kill` **never** reports success while it could not
acquire the lock or prove scope termination. A stuck-FS launch stall is a
documented residual surfaced by this diagnostic.

Foreground Reconcile is unchanged (no `SupervisorPID`/evidence; scope-emptiness
authority as today). The descendant-migration residual (#20) is unchanged by
detach; an unsupervised child loses no output while running (direct-file capture).

### 6.1 Kill-intent during and around launch (P0-1, P0-2 r1)

- During the (long) **admission** wait, the shim's poll loop aborts on
  `KillIntent.Present` → terminalize `killed`, no child.
- The shim then **holds the per-run flock across the launch section** — from the
  final kill-intent re-read through scope `Create`, `Start`, placement
  verification, and the `running` append (P0-1). `run-kill` (`killWithIntent`)
  takes the *same* per-run flock, so it serialises to **strictly before** creation
  (its intent is seen by the shim's re-read → no child) or **strictly after**
  placement (the scope exists → `cgroup.kill` reaches the child). There is no
  window where intent is recorded yet the child starts unkilled. (The `.lock` fd
  is `O_CLOEXEC`; the held section uses ledger-level `append`, not
  `appendTerminalLocked`, so no self-deadlock; a failure in the section releases
  the flock before terminalizing via `failBeforeLaunch`.)

### 6.2 One terminalization path (P1 r1)

Every shim failure/abort — pre-reserve, missing-ACK (`cancelled`/`killed`),
kill-intent abort, admission timeout, output-open, scope-create, Start,
timeout-setup, and the post-quiesce finalise — terminalizes through the existing
`failBeforeLaunch`/`appendTerminalLocked` machinery (with `mergeEvidence`), never
an ad-hoc direct append. A dedicated test exercises the deferred cleanup path.

---

## 7. run-kill and run-log (already work)

- **`run-kill <id>`** reads the record, opens `CgroupScope`, cgroup-kills
  (TERM→grace→KILL) with the #27 ownership guard (foreign worktree refused without
  `--steal`); the shim's `waitpid` records `killed`. Admission-window case: §6.1.
- **`run-log <id> …`** re-reads the same bytes against the live-growing dup2-direct
  file; `--follow` waits until terminal; `--since` gives incremental live reads. No
  changes required.

---

## 8. Faces and routing

- **CLI:** `run` verbSpec gains `boolSpec("detach", …)`; the core handler branches
  to the launcher path (returns before wiring, §5); handle `{id, status:starting}`,
  `--json` the same `RunRecord`-shaped handle.
- **MCP:** `aira_run` gains `detach` (bool); returns the handle immediately; caller
  polls `aira_run_output`/`aira_get`; dispatch metadata + default-fill + delimiter
  want-maps updated (M19 parity).
- **`__supervise`/`--ready-fd`/`--ack-fd`:** hidden — asserted absent from help/MCP/
  agent-guide; present only as an internal dispatch entry.
- Skill: one `run --detach` example; MCP/skill parity updated.

---

## 9. Deferrals (explicit)

- **D1 — Detached telemetry auto-wiring (M20b):** the shim runs the M19 wiring
  after terminal. v1 rejects the flags + structurally skips the block; telemetry
  `unevaluated`.
- **D2 — Hard mid-run capture cap** for dup2-direct (read-time §19 elision covers
  v1).
- **D3 — `aira run-input`** (§14 line 156): unblocked but out of scope.
- **D4 — The daemon** + cross-session admission fairness-queue (#29) + an expiring
  fenced supervisor lease (the §6 `U_RUN_SUPERVISOR_STALLED` residual's stronger
  form).
- **D5 — Non-Linux:** the stub rejects `--detach` with the unsupported-platform
  code.

---

## 10. Tests and the Sol build-review checklist

TDD. Real-cgroup integration tests under `AIRA_REAL_CGROUP=1 whale-run` (Opus
verifies on real hardware). Committed executable e2e (`~/tmp/aira-m20-e2e.sh`)
drives the real CLI `config→launcher→shim→runner→ledger` path.

**Correctness properties (each must fail against a plausible wrong impl):**

1. Detach returns promptly; launcher exits while the child runs (discriminator:
   launcher pid gone, child pid in the scope); `get` shows the state progression.
2. Shim outside the scope: `run-kill` kills the child; the shim records `killed`;
   the shim pid never in `cgroup.procs`.
3. dup2-direct faithful; `--merge` kernel-ordered via one OFD; digest+`complete`
   only after clean scope-empty quiesce.
4. **Forced quiesce** (P0-3): leader exits 0 but a descendant lingers and is
   `cgroup.kill`ed → the `quiesce-forced` event is durable **before** the kill →
   outcome `ScopeDescendantKilled`+`U_RUN_QUIESCE_FORCED`, `CleanSuccess()==false`,
   leader exit preserved as evidence — **even when a racing Reconcile finalizes**
   the run instead of the shim (the forced fact is never lost, Sol P0-1 r3).
5. **Never terminal over a non-empty scope** (P0-4): a run whose scope cannot be
   quiesced stays non-terminal + killable; ordinary `run-kill` still targets it.
6. **Supervisor-lease + exit-evidence Reconcile**: (a) queued run, live shim →
   preserved not lost; (b) Reconcile after child-exit with `leader-exited`
   evidence + empty scope → `finalizeDetachedTerminal` produces the **real**
   `exit_code` **plus usage + digest** (never `lost`, never a usage/digest-stripped
   terminal), whether or not the shim is alive, and identically to the shim
   (shared-finalizer equivalence, Sol P0-2 r3); (c) dead shim + empty scope + no
   evidence → `lost`+`U_RUN_EXIT_UNKNOWN`; (d) alive shim + empty scope + no
   evidence → preserved + `U_RUN_SUPERVISOR_STALLED`; (e) **unknown** liveness
   (boot_id unreadable / legacy record) → preserved + unevaluated, never `lost`.
7. **Create/Start race** (P0-1): `run-kill` intent injected concurrently with the
   flock-held launch section → either no child starts (intent seen) or the child
   starts and is cgroup-killed; never a started-but-unkilled child under a killed
   record.
8. **Two-phase ACK** (P0-2/P0-3 r1): launcher timeout/EOF or broken-stdout →
   missing ACK → shim terminalizes (`cancelled`, or `killed` if intent recorded,
   never erasing an accepted kill) with **no** child; handle printed **before**
   ACK; normal ACK → run proceeds.
9. **Three-valued liveness** (P1 r2/r3): a recycled pid+start-tick with a different
   `boot_id` reads **dead**; a zombie reads dead; an **unreadable** boot_id reads
   **unknown** (→ preserve, not dead); an unreadable boot_id at identity creation →
   `E_RUN_IDENTITY_UNAVAILABLE` before reserve.
10. **Field-loss trap: clean exit 0 survives** (P0-3 r3) — a `leader-exited`
    evidence with `exit_code=0`, `LeaderExitObserved=true` survives `replay` and is
    finalized as `exited 0` (not collapsed to `lost`) by **both** the shim and
    Reconcile terminalizers.
11. Normal-path faithfulness: exit N → `exit_code=N`, `status=exited`,
    `peak_rss`/`cpu_*` from cgroup-at-exit, `terminal_complete=true`.
12. **Bounded-lock launch-stall** (P1 r3): a run-kill/Reconcile that cannot acquire
    the per-run flock within the bound → `U_RUN_LAUNCH_STALLED`, never a false
    kill-success or fabricated terminal.
13. Flag rejection matrix (§5): each → `E_RUN_ARGUMENT_INVALID` synchronously, no
    id; wiring block unreachable on detach.
14. mergeEvidence monotonic carry of `Detached`/`SupervisorPID`/leader-evidence
    (incl. `LeaderExitObserved` gating, not non-zero-exit).
15. Control file `0600`, deleted before Start. `__supervise` hidden; MCP/skill
    parity for `detach`.

**Sol build-review checklist (false-fail AND false-pass):**

- **Launch-race flock**: the per-run flock is genuinely held from the final
  kill-intent re-read through the `running` append; run-kill cannot inject intent
  mid-race; no self-deadlock; `.lock` fd is `O_CLOEXEC`.
- **Handshake**: handle printed+flushed before ACK; readyW/ackR closed by the
  launcher post-spawn; EPIPE on a ready write == missing ACK; `cancelled` yields to
  an existing terminal or a recorded kill-intent.
- **Exit evidence + quiesce**: `leader-exited` durable before quiesce; no terminal
  over a non-empty scope; forced quiesce ⇒ `ScopeDescendantKilled`+
  `U_RUN_QUIESCE_FORCED` (not clean `exited`); Reconcile finalises from evidence
  with the real exit, alive-or-dead shim.
- **Lease honesty**: no fabricated exit/usage; `U_RUN_SUPERVISOR_STALLED` for the
  wedged-alive-empty-no-evidence residual, not a fake terminal; boot_id/zombie
  guard on `processLive` for both leader and supervisor.
- **Capture**: one shared merge OFD; shim closes its capture-fd copies after Start;
  eviction/truncation → unavailable/partial not fake complete; no drain/live-tee
  path reachable under detach.
- **Single-writer + one terminalization path**: launcher writes zero ledger
  events; every failure/abort via `failBeforeLaunch`/`appendTerminalLocked`.
- **Wiring suppression**: no config/env/default/reused-Launch path activates M19
  wiring under detach; telemetry `unevaluated`.
- **Porous-test check**: real-cgroup tests fail (not skip) under
  `AIRA_REAL_CGROUP=1`; the load-bearing discriminators are launcher-exited-while-
  child-runs and Reconcile-finalises-from-evidence.

**New stable codes:** `E_RUN_DETACH_FAILED` (supervisor unspawnable or no
readiness), `E_RUN_IDENTITY_UNAVAILABLE` (boot_id unreadable at identity creation
→ abort before reserve), `U_RUN_DETACH_CANCELLED` (handshake abandoned, no child
ran), `U_RUN_QUIESCE_FORCED` (surviving descendants force-killed after leader
exit), `U_RUN_CAPTURE_INCOMPLETE` (dup2-direct capture could not quiesce),
`U_RUN_SUPERVISOR_STALLED` (alive supervisor, empty scope, no exit evidence),
`U_RUN_LAUNCH_STALLED` (bounded per-run flock acquisition timed out — a launch may
be stuck; never a false kill/terminal). All other failures reuse the existing
`E_RUN_*`/`U_RUN_*` taxonomy via the relay.
