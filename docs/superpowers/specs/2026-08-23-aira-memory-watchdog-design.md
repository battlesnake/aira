# AIRA memory watchdog — host-OOM bypass-killer (whale→AIRA)

Status: PLAN v1 (synthesised from the ultracode understand+design workflow
`wf_ad46311b-fa5` — 4 code-grounded readers + 2 design approaches; the synth agent
hit a weekly subagent quota so Opus synthesised from the completed material).
Milestone #59. Implements the whale-watchdog (agentmux layer 2) functionality in AIRA
toward the eventual whale→aira flip.

## 0. Purpose + threat model

A conservative **last-resort backstop**: when the host is *genuinely thrashing on
memory*, kill the single biggest **agent-spawned, unconfined, heavy** process so the
machine does not hard-OOM the desktop. It backstops the admission model for the
runtime *bypass* case — heavy work an agent started OUTSIDE an AIRA confinement scope
(so no cap applies). Work that IS inside a capped slice/scope is already bounded by
its cap and is NOT this watchdog's job.

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

- **Signal:** `/proc/pressure/memory` **`full avg10`** — the % of the last 10s during
  which ALL tasks stalled on memory reclaim: the honest "we are actually thrashing"
  signal, far better than a single `MemAvailable` reading a fast spike outruns.
  (MemAvailable corroboration is a recorded deferral.) A **new** `readHostPressureFull()`
  parser is required — the existing `parseCgroupKeyValues` (`usage_linux.go:56`)
  hard-requires 2 fields/line and silently drops PSI lines; the new one parses the
  `full` line's `avg10` as a float and returns `(avg10, ok, reason)` honestly (like
  `readSliceMemory`).
- **Hysteresis band** (anti-thrash): TRIP when `full avg10 >= tripPSIFullAvg10`
  (=10.0); RECOVER only when `<= recoverPSIFullAvg10` (=1.0); in-band → hold.
- **Debounce:** require the TRIP condition on **K=3 consecutive** evaluate-then-sleep
  passes before ANY action (~6s at the 2s default) — a transient spike never fires.
  `armCount` lives in the `runWatchdog` closure (no shared Server state): `++` when
  `>=trip`, `=0` when `<=recover`, hold in-band, act at `>=K`. Unreadable PSI resets
  `armCount=0` (unknown is not "calm", but we may NEVER act on unknown — the
  conservative direction). After acting → `armCount=0` (re-arm): at most ONE offender
  per sustained episode, then `postKillSettle` (1s) + re-observe; a second kill needs a
  fresh sustained window.
- **`unevaluated` on unreadable/absent PSI** (`CONFIG_PSI=n`, no `/proc/pressure`):
  log once, no action, never fabricate pressure (needless kill) or calm (suppressed
  kill). Per-pass errors log + no-op, never abort `Serve`.

## 2. Offender — ALL predicates must hold (the false-positive guard)

A candidate is killed only if it satisfies EVERY one of:
1. **Unconfined** — `/proc/<pid>/cgroup` (the `0::` unified relpath) contains NO
   `/.aira-` component: it is NOT inside an AIRA confinement scope (nothing AIRA
   accounts/caps bounds it).
2. **Agent-attributable** — it is a BFS descendant of a process whose `comm=="claude"`
   (exact match on `/proc/<pid>/status` `Name:`), EXCLUDING the claude procs
   themselves. This is the blast-radius guard: a browser/IDE/user-session process is
   not a claude descendant, so it can never be a victim.
3. **Heavy** — `VmRSS >= minVictimRSS` (=2 GiB).
4. **Not protected** — not the AIRA daemon's own pid/cgroup, not PID 1, not a
   system/session-critical unit.

Pick the biggest-RSS qualifier (pid-asc tiebreak). **If none qualifies → DEFER**: kill
nothing, log "pressure is elsewhere — deferring to oomd/kernel" (the agentmux guard).
The daemon protecting itself is mandatory (agentmux uses `OOMScoreAdjust=-900` on its
service; AIRA excludes its own pid + cgroup).

## 3. Kill — graduated, subtree, re-confirmed

SIGTERM the offender's **process subtree** → **5s ctx-aware grace** → re-confirm the
subtree is still alive AND PSI still tripped → **SIGKILL** survivors. (Not `cgroup.kill`
— the offender is in a foreign/unconfined cgroup we do not own; not SIGKILL-only —
give a graceful chance.) A vanished pid (ESRCH) is tolerated; any other kill error
(e.g. EPERM) is recorded as a **failure**, never a success, and does not crash `Serve`.
Known limitation (recorded): SIGTERM→SIGKILL of a subtree is not atomic — a fast
re-forking fleet can outrun it (contrast a capped scope's atomic `OOMPolicy=kill`);
one-offender-per-episode + re-observe bounds the damage; a full fix is the whale→aira
confinement model, not this backstop.

## 4. Mode + config (off by default)

- `AIRA_DAEMON_WATCHDOG_MODE` ∈ `{off (default), observe, enforce}`:
  - **off** — the goroutine parks on `<-ctx.Done()` and does nothing (the disabled
    sentinel; zero cost).
  - **observe** — evaluates + audits every trip/defer/**"WOULD kill"** decision, sends
    NO signals (agentmux `--dry-run` parity). The safe way to watch the policy on real
    load before trusting it.
  - **enforce** — real SIGTERM→SIGKILL.
- `AIRA_DAEMON_WATCHDOG_INTERVAL` — Go duration, default 2s, validated `[1s, 30s)`;
  consulted only when `mode != off`. Modeled on `registryDiscoveryIntervalFromEnv`
  (`paths.go:92-105`). Thresholds are constants (a couple of env overrides at most —
  YAGNI).

## 5. Honesty + audit (never a fake action)

Every decision — trip, defer, WOULD-kill (observe), kill (enforce), unevaluated —
emits a **store event** through a ready-scope daemon view (like the reaper,
`server.go:328`) carrying: the PSI value, offender pid/comm/RSS, the four predicate
verdicts, the mode, and the escalation outcome. Auditable in `watch`/the TUI events
tail, not just `log.Printf`. A failed non-ESRCH kill is recorded as a failure. Nothing
is ever reported as done that was not done.

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

- **Pure (fixtures via the deps seam):** the PSI parser (`full avg10` extraction,
  malformed/absent → unevaluated); the hysteresis+debounce state machine (trip only
  after K sustained; recover; in-band hold; unreadable resets; re-arm after acting);
  the four-predicate offender selection (each predicate independently gates — a
  confined / non-agent / light / protected candidate is NEVER selected; none-qualifies
  → defer); mode gating (off = no-op, observe = WOULD-kill-no-signal, enforce = kill);
  the audit event content. Each proven RED against the wrong impl (both directions).
- **Kill mechanics (unit, fake kill dep):** SIGTERM→grace→SIGKILL ordering; ESRCH
  tolerated; EPERM → recorded failure; self/PID1/protected never signalled.
- **Real-proc (gated):** a fixture process tree (a real `claude`-comm'd parent → heavy
  child) under `observe` produces the WOULD-kill decision with the right pid; a
  confined (`.aira-` cgroup) child is never selected. Never kill for real in CI.
- **Daemon lifecycle:** off-mode parks + drains cleanly; `-race` across the goroutine
  + shutdown join.

## 8. Deferrals (recorded)

MemAvailable corroboration of the PSI trigger; per-cgroup `memory.pressure` victim
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
