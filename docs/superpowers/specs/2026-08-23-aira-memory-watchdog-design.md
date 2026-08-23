# AIRA memory watchdog — host-OOM bypass-killer (whale→AIRA)

Status: PLAN v3 — **Fable RE-GATE GATE-PASS** on the hardened kill-safety (v2 machinery
confirmed buildable+correct against the code; the 3 re-gate must-fixes now folded:
interlock **cannot-confirm→refuse** [`is-active` empty/bus-fail must NOT read as
inactive], `/proc/stat` **parse-after-last-`)`** + **ENOSYS→degrade-not-`kill(2)`**,
and MemAvailable/latch spec-consistency + tests). Prior: **Fable code-gate GATE-PASS**
(folded the safety-critical predicate
fix: "uncapped" = no finite `memory.max` ancestor, else capped whale jobs are
false-kill victims; + the host-global audit contract) and **Sol adversarial
plan-review** (folded: hard machine-wide interlock vs whale-watchdog; pidfd/TOCTOU
re-validation; latch-per-episode; PSI `total`-delta + MemAvailable corroboration;
intent→outcome honest audit facets; probabilistic-attribution framing; parameterised
RSS threshold). **Owner steered (2026-08-23):** KILL is the accepted, weeks-proven
policy; the goal is protecting the outer layers (slice → WSL VM ~80G → Windows host
96G) from *escaped* (uncapped) work — so **enforce**, hardened + interlocked, not
observe-only. Base design from the ultracode understand+design workflow
`wf_ad46311b-fa5` (4 code-grounded readers + 2 approaches; the synth agent hit a weekly
subagent quota, so Opus synthesised). Milestone #59 — the whale-watchdog (agentmux
layer 2) reimplemented in AIRA toward the whale→aira flip.

## 0. Purpose + threat model

A conservative **last-resort backstop** protecting the **outer containment layers**
(owner): the confinement slice (64G) sits inside the WSL Linux VM (~80G) inside the
Windows host (96G); each layer leaves headroom for the next so an OOM is contained to
the offending layer. This watchdog exists for the **escape** case — heavy work an
agent started OUTSIDE any capped scope, which can drive the *VM/host* (not just a
slice) toward OOM. When the VM is genuinely thrashing on memory it kills the single
biggest **agent-spawned, uncapped, heavy** process. Work that IS inside a capped
slice/scope is already bounded by its cap (its own memcg OOM fires first) and is NOT
this watchdog's job. **KILL is the accepted, weeks-proven policy** (agentmux's
whale-watchdog has run this on this box for weeks); AIRA's admission/quota management
makes in-slice OOMs rarer, so this backstop is chiefly for the escaped-work case, and
AIRA's watchdog **replaces** whale-watchdog at the whale→aira flip.

**Attribution is probabilistic, not absolute (Sol).** `comm=="claude"` ancestry is
spoofable and an agent can legitimately spawn things you care about (an MCP server, a
dev server, a database). The four-predicate guard makes a false-kill *unlikely*, not
impossible; this is why enforce is opt-in + machine-wide-interlocked (§4), and why
authoritative agent/job identity (the whale→aira identity model, §8) is the path to a
stronger guarantee. We do not claim "can never kill user work".

**A false kill destroys the user's real work.** So every choice favours *never
killing the wrong thing* over acting early: a PSI-sustained trigger with hysteresis +
debounce, THREE independent predicates that must ALL hold on a candidate, one offender
per sustained episode then re-observe, off-by-default, dry-run before enforce,
fail-open, and `unevaluated` (never a fabricated pass or trigger) on any unreadable
signal.

**Coexistence (hard):** agentmux's `whale-watchdog` is the *live* memory watchdog on
this box. Two watchdogs must never fight. So AIRA's is **off by default**; enabling it
is an explicit opt-in that the operator does only once whale-watchdog is stopped (a
documented interlock; auto-detect-and-refuse is deferred). This is the "whale
functionality implemented in AIRA, ready for the flip" piece.

## 1. Trigger — PSI, sustained (not a single MemAvailable sample)

- **Signal — contemporaneous, not just an average (Sol #6):** `/proc/pressure/memory`
  `full`. `avg10` is an *exponentially-decayed historical* average — sampling it 3×
  does NOT prove a fresh episode and can fire after pressure already subsided. So the
  PRIMARY trigger is the **`total` counter DELTA** across the evaluate window (µs of
  full-stall actually accrued *in the last interval* — proof we are stalling NOW),
  gated by `avg10 >= trip`, and **corroborated by low VM `MemAvailable`** (a
  `/proc/meminfo` read; MemAvailable is no longer deferred — Sol wants it as
  corroboration so PSI history alone can't fire). A **new** `readHostPressureFull()`
  returns `(avg10 float, totalMicros uint64, ok, reason)` — the existing
  `parseCgroupKeyValues` (`usage_linux.go:56`) hard-requires 2 fields/line and silently
  drops the 5-field PSI line, so a new parser is required; it returns `(…, ok, reason)`
  honestly like `readSliceMemory`. (Accepted false-negative, stated: PSI can miss an
  *abrupt* allocation spike that OOMs before a stall registers — the kernel OOM/oomd
  remains the layer below us.)
- **Hysteresis band** (anti-thrash): TRIP when `full avg10 >= tripPSIFullAvg10`
  (=10.0); RECOVER only when `<= recoverPSIFullAvg10` (=1.0); in-band → hold.
- **Debounce:** require the TRIP condition on **K=3 consecutive** evaluate-then-sleep
  passes before ANY action (~6s at the 2s default) — a transient spike never fires.
  `armCount` lives in the `runWatchdog` closure (no shared Server state): `++` when
  `>=trip`, `=0` when `<=recover`, hold in-band, act at `>=K`. Unreadable PSI resets
  `armCount=0` (unknown is not "calm", but we may NEVER act on unknown — the
  conservative direction). **After acting → LATCH the episode (Sol #2):** re-enable
  action ONLY after PSI **recovers** (`avg10 <= recover` AND the `total`-delta
  subsides) — merely resetting `armCount` would permit a SECOND kill K samples later
  while pressure is still elevated (over-killing a single episode). So: `postKillSettle`
  (1s), re-observe, and stay latched (no further kill) until a genuine recovery, which
  a fresh sustained window must then re-trip. Truly one offender per episode.
- **`unevaluated` on unreadable/absent PSI** (`CONFIG_PSI=n`, no `/proc/pressure`):
  log once, no action, never fabricate pressure (needless kill) or calm (suppressed
  kill). Per-pass errors log + no-op, never abort `Serve`.

## 2. Offender — ALL predicates must hold (the false-positive guard)

A candidate is killed only if it satisfies EVERY one of:
1. **Uncapped** (the load-bearing fix — Fable) — the candidate's cgroup has **NO
   finite `memory.max` anywhere in its ancestry** (reuse `effectiveCapFrom`/
   `hasFiniteCapAncestor`, `confine_linux.go:741`), AND no `/.aira-` component in its
   `0::` relpath. **A `.aira-`-only test is WRONG:** heavy agent work runs under
   `whale-run` in whale.slice (capped, `whale-run-whale-<PID>.scope`, no `.aira-`
   component), so a `.aira-`-only "unconfined" check would make CAPPED whale jobs the
   top-RSS victims in enforce mode — the exact false-kill the threat model forbids. A
   finite `memory.max` ancestor (whale.slice, aira scopes, ANY capped slice) means the
   work is already bounded and **cannot drive host-OOM**, so it is NEVER a victim.
   Only genuinely uncapped heavy work qualifies. (This closes the §0 threat model:
   "work inside a capped slice/scope is not this watchdog's job.")
2. **Agent-attributable** — it is a BFS descendant of a process whose `comm=="claude"`
   (exact match on `/proc/<pid>/status` `Name:`), EXCLUDING the claude procs
   themselves. This is the blast-radius guard: a browser/IDE/user-session process is
   not a claude descendant, so it can never be a victim.
3. **Heavy** — `VmRSS >= minVictimRSS` (default 2 GiB) — an **injected parameter/dep**
   (Sol #8) so tests set a tiny threshold against a small fixture process rather than
   allocating real gigabytes.
4. **Not protected** — not the AIRA daemon's own pid/cgroup, not PID 1, not a
   system/session-critical unit.

Pick the biggest-RSS qualifier (pid-asc tiebreak). **If none qualifies → DEFER**: kill
nothing, log "pressure is elsewhere — deferring to oomd/kernel" (the agentmux guard).
The daemon protecting itself is mandatory (agentmux uses `OOMScoreAdjust=-900` on its
service; AIRA excludes its own pid + cgroup). The expensive /proc victim scan runs
**only when armed** (`armCount >= K`), never on every 2s pass.

**Recorded limitations of agent-attribution (Fable):** (a) `comm=="claude"` misses
**codex/Terra/Luna builder** processes — the owner's heavy builds are codex procs, not
claude-descended; but a MISS is safe (falls through to oomd/kernel — the conservative
direction), a false-KILL is not, so erring narrow is correct for v1. Broadening the
agent identity set (claude + codex + rename-proof attestation) is folded into the
whale→aira identity model (§8). (b) Orphan reparenting (a runaway whose agent parent
died → reparented to PID 1) breaks the descendance BFS → missed (again a safe miss).
Both are safe-direction gaps, not false-kill risks.

## 3. Kill — graduated, subtree, re-confirmed

**pidfd-based, TOCTOU-safe, per-target-guarded (Sol #4 + #5).** Between the scan and
the signal a PID can be reused — signalling a raw PID could hit an unrelated (possibly
important) process. So:
- At selection, **`pidfd_open(2)`** each target (the offender root + each subtree
  member); the pidfd is a stable handle to *that* process, immune to PID reuse. Signal
  via **`pidfd_send_signal(2)`** (`PidfdOpen`/`PidfdSendSignal` in the vendored
  `golang.org/x/sys/unix`, cgo-free; kernel ≥ 5.3, box is 6.x). **`ENOSYS` from either
  → recorded honest degradation (no kill), NEVER a raw `kill(2)` fallback** (Fable — a
  fallback reintroduces exactly the TOCTOU this closes).
- **Re-validate immediately before BOTH SIGTERM and SIGKILL** (not just "still alive"
  — TERM is already destructive): the process start-time (`/proc/<pid>/stat` field 22,
  **parsed AFTER the last `)`** — comm (field 2) may contain spaces/parens, so naive
  field-splitting mis-indexes — Fable) still matches the selection, its cgroup is STILL
  uncapped (no finite `memory.max` ancestor), and it is STILL not-protected. Any
  mismatch / unreadable `/proc` data → **disqualify that target, do not signal it**.
- **Protection applies to EVERY target in the subtree, not just the root (Sol #4):**
  each subtree member is independently checked (not the AIRA daemon, not PID 1, not a
  system/session-critical unit — "critical" defined mechanically: PID 1, the daemon's
  own pid/cgroup, and anything whose cgroup PATH has a `/system.slice/` or
  `/init.scope/` component (Fable: a literal `-.slice` never appears in a cgroup path).
  A reparented or foreign process that drifted into the tree is skipped.
- Sequence: SIGTERM the qualifying targets → **5s ctx-aware grace** → re-validate +
  re-check PSI still tripped → **SIGKILL** the survivors that still qualify. Not
  `cgroup.kill` (foreign cgroup we don't own); not SIGKILL-only (graceful chance
  first). ESRCH (already gone) is success-by-exit; any other error (EPERM…) is a
  recorded **failure**, never a success; never crashes `Serve`.
- Known limitation (recorded): subtree SIGTERM→SIGKILL is not atomic — a fast
  re-forking fleet can outrun it (contrast a capped scope's atomic `OOMPolicy=kill`);
  the latch + one-offender-per-episode bound the damage; the full fix is the whale→aira
  confinement model, not this backstop.

## 4. Mode + config (off by default)

- `AIRA_DAEMON_WATCHDOG_MODE` ∈ `{off (default), observe, enforce}`:
  - **off** — the goroutine parks on `<-ctx.Done()` and does nothing (the disabled
    sentinel; zero cost).
  - **observe** — evaluates + audits every trip/defer/**"WOULD kill"** decision, sends
    NO signals (agentmux `--dry-run` parity). The safe way to watch the policy on real
    load before trusting it.
  - **enforce** — real SIGTERM→SIGKILL, but gated by a **hard machine-wide interlock
    (Sol #1 — off-by-default + docs cannot prevent two killers):** on entering enforce
    the daemon (a) acquires a machine-wide **kill-authority `flock`** (non-blocking) on
    `$XDG_RUNTIME_DIR/aira-memory-watchdog.lock` (same `XDG_RUNTIME_DIR` fallback as
    `PathsFromEnv`, `paths.go:121-123`; refuse enforce if unset) and (b) **positively
    confirms whale-watchdog is inactive — CANNOT-CONFIRM = REFUSE (Fable MUST-FIX,
    probed live):** `systemctl --user is-active whale-watchdog` prints `active`/exit 0
    when running, `inactive`/exit 4 for an unknown unit, but on a **bus failure prints
    EMPTY stdout/exit 1** — so a "≠ active" or exit-code-only reading would treat
    *cannot-determine* as *inactive* and authorise a SECOND killer on a bus-less
    daemon. Authorise enforce ONLY on a **clean exec whose stdout is EXACTLY `inactive`
    or `failed`**; any exec/bus error, empty stdout, or other value → cannot-confirm →
    **degrade to observe**. If the lock is held or whale-watchdog is (or may be)
    active, enforce runs as **observe** (honest degradation), never a second concurrent
    killer. Re-checked each armed episode. Recorded limitation: a **manually-run
    (non-unit) whale-watchdog is invisible** to `is-active` and the flock — the
    interlock covers the systemd-managed unit (the normal case). Clean whale→aira
    handoff: stop whale-watchdog, then AIRA enforce takes authority.
- `AIRA_DAEMON_WATCHDOG_INTERVAL` — Go duration, default 2s, validated `[1s, 30s)`
  **at startup per the fail-fast `*FromEnv` pattern** (a malformed/out-of-range value
  fails daemon startup regardless of mode — not silently ignored when off — Fable P2).
  Modeled on `registryDiscoveryIntervalFromEnv` (`paths.go:92-105`). Thresholds are
  constants (a couple of env overrides at most — YAGNI).

## 5. Honesty + audit (never a fake action)

Every decision — trip, defer, WOULD-kill (observe), kill (enforce), unevaluated —
emits a **store event** carrying: the PSI value, offender pid/comm/RSS, the four
predicate verdicts, the mode, and the escalation outcome. Auditable in `watch`/the TUI
events tail, not just `log.Printf`. A failed non-ESRCH kill is recorded as a failure.

**Emission contract (Fable MUST-FIX 2 — events are project-scoped, `watch.go:19`
`WHERE project_id=?`, but this is a host-global decision):**
- **Kill BEFORE audit, never gate the kill on the write.** A SQLite write can stall
  under the very thrash that tripped the watchdog. Decide + act (or defer) first;
  audit after, with a **bounded emit timeout**; a failed/timed-out emit is logged
  (honest degradation), never blocks the safety action.
- **Which scope(s):** the event is written to every ready project scope the daemon
  knows (broadcast — the reaper's `readyScope` snapshot, `server.go:328-337`), so it
  surfaces in whichever project's `watch`/TUI the operator has open.
- **Zero-ready-scope fallback:** an idle daemon (plausible exactly during a bypass)
  may have NO ready scope to emit into → this is a **recorded honest degradation**
  (logged as "audit unrouted: no ready scope"), NEVER silent loss.
- **New exported Store method** built on the internal `nextSequence` +
  `insertEventActor` substrate (`store.go:2733-2760`) — the spec names it
  (`AppendWatchdogEvent` or similar); the daemon owns the single writer, so this
  preserves single-writer.
- **Intent-then-outcome, with honest kill facets (Sol #7).** Emit a best-effort
  **intent** record ("about to SIGTERM pid N, start-time S, RSS R") *before* signalling
  (bounded timeout — if it can't persist under thrash, log and proceed; the kill is the
  safety action and must NOT be blocked on a DB write — reconciling Sol's
  fail-closed-on-persistence with Fable's don't-gate-the-kill: the *audit* degrades, the
  *safety action* proceeds). Then record the outcome as **separate honest facets**:
  `signal_sent` (the syscall was issued), `delivered` (returned nil — delivered, NOT
  died), `exited` (re-validated gone after grace), `escalated_sigkill`, and
  `unresolved` (still present after SIGKILL+grace). **NEVER label "killed" from
  `pidfd_send_signal` returning nil** — that proves delivery, not death; "killed" is
  claimed only after the target is re-validated gone. **Reading rule (Fable P2):** an
  intent record with NO matching outcome (the daemon crashed between intent and
  outcome) reads as **`unknown`**, never "killed". Nothing is reported as done that was
  not done.

## 6. Daemon integration (grounded)

- New `internal/daemon/watchdog.go`: `func (s *Server) runWatchdog(ctx, mode, interval)`
  copying the **evaluate-then-sleep `time.NewTimer`** shape of `runRegistryDiscovery`
  (`discovery.go:89-109`), NOT `runReaper`'s ticker (a slow victim scan must never
  stack). Opens with `if mode==off || interval==0 { <-ctx.Done(); return }`.
- `server.go`: read mode+interval alongside the other `*FromEnv` calls
  (~`:119-132`); spawn the goroutine after the discovery block (~`:241-246`) with
  `watchdogCtx`/`cancelWatchdog`/`watchdogDone`; `cancelWatchdog()` at ~`:277-279`;
  **`<-watchdogDone` in the drained-goroutine join BEFORE `close(drained)`**
  (~`:285-287`) or it leaks/hangs shutdown.
- `paths.go`: `defaultWatchdogInterval` + `watchdogModeFromEnv()`/
  `watchdogIntervalFromEnv()` modeled on the registry-discovery pair.
- **Injected `watchdogDeps` seam** (mirror agentmux `whale.go:384`): fields for
  `readPSI`, `snapshotProcs`, `kill`, `sleep`, `emitEvent`, `now`, `mode` — so every
  trip/defer/kill/unevaluated decision is table-testable offline against `/proc`
  fixtures, in BOTH false-fail and false-pass directions.

## 7. Tests (TDD; pure decision logic + gated real-proc)

- **Pure (fixtures via the deps seam):** the PSI parser (`full` avg10 **AND `total`**
  extraction, malformed/absent → unevaluated); the trigger state machine (trip only
  after K sustained on `total`-delta+avg10+low-MemAvailable; recover; in-band hold;
  unreadable resets; **LATCH after acting** — a SECOND sustained window while still
  unrecovered must NOT re-kill; re-arm only after a genuine recovery — Fable MUST-FIX);
  the four-predicate offender selection (each predicate independently gates — a
  **capped [`.aira-` OR finite-`memory.max`-ancestor]** / non-agent / light / protected
  candidate is NEVER selected; none-qualifies → defer); mode gating (off = no-op,
  observe = WOULD-kill-no-signal, enforce = kill); the audit event content. Each proven
  RED against the wrong impl (both directions).
- **Kill mechanics (unit, fake kill dep):** SIGTERM→grace→SIGKILL ordering; ESRCH
  tolerated; EPERM → recorded failure; self/PID1/protected never signalled.
- **Real-proc (gated), with a TINY `minVictimRSS` (Sol #8 — no real-GiB alloc):** a
  fixture process tree (a real `claude`-comm'd parent → a small child above a
  KiB-scale injected threshold) under `observe` produces the WOULD-kill decision with
  the right pid; a capped child (finite `memory.max` ancestor) is never selected;
  pidfd re-validation rejects a start-time mismatch (simulate PID reuse). Never kill
  for real in CI.
- **pidfd + TOCTOU (unit, fake syscall dep):** `pidfd_open`/`pidfd_send_signal`
  wrappers; a start-time-mismatch / cgroup-now-capped / protected target is NOT
  signalled; each subtree member independently protection-checked; ESRCH = exited,
  EPERM = recorded failure.
- **Interlock (unit):** enforce with whale-watchdog "active" (fake systemctl) or a held
  flock → degrades to observe, no signal.
- **Daemon lifecycle:** off-mode parks + drains cleanly; `-race` across the goroutine
  + shutdown join.

## 8. Deferrals (recorded)

Per-cgroup `memory.pressure` victim
ranking + recursive cgroup-tree scan; a daemon-published authoritative live `.aira-*`
scope registry (the `/proc/<pid>/cgroup` test suffices for now); auto-detect-and-refuse
when whale-watchdog is running (documented interlock only); `nice`/`memory.high`
squeeze before killing; rename-proof agent identity beyond `comm=="claude"` (folded
into the whale→aira identity model); configurable protect-lists / per-threshold env
knobs; `cgroup.kill` of AIRA-owned scopes.

## 9. Open question (owner)

**Default mode.** v1 defaults to **off** (agentmux's whale-watchdog is live; two must
not fight, and off is the only zero-risk default). The alternative is defaulting to
**observe** (audits pressure/offenders without killing — useful telemetry, still no
signals). I recommend **off**; flip to **observe**/**enforce** deliberately once
whale-watchdog is retired in the whale→aira migration. (Not blocking — off is safe.)
