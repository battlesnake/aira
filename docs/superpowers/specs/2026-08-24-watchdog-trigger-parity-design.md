# AIRA watchdog — MemAvailable-authoritative trip (whale-watchdog parity) + observe visibility

Status: PLAN v3 — folds the Sol v2 re-gate (GATE-FAIL, no P0; core confirmed sound; P1
honesty + P1 latch-spec + P2 invariant/placement) and the Fable v2 re-gate
(GATE-PASS-WITH-NITS; same honesty P1 + compile/dead-code/nil-guard/test specifics). Fixes a
real inertness the observe-mode deployment surfaced (#64, follow-up to #59). Safety-critical
KILL-trigger logic → full two-loop, re-gated to convergence.

## 1. The gap (evidence)

Deployed on this box (aira-daemon, `--watchdog=observe`), AIRA's watchdog emitted **zero
events** across its whole life — including through a real 2026-08-24 17:32 memory event
(MemAvailable 7.5 GiB; a claude worker-fleet Σ=69 GiB / 156 procs) that fired the live
whale-watchdog, which SIGKILLed to recover. AIRA never armed.

Root cause (`watchdog.go:232`): `trip := avg >= 10.0 && delta > 0 && available < 8 GiB` —
requires PSI `full` avg10 ≥ 10 **and** a positive PSI-total delta **and** low MemAvailable,
ALL three (and `:235` resets the debounce count whenever avg is calm, so a mem-low-without-
stall poll can never accumulate). whale-watchdog (weeks-proven) trips on **`MemAvailable <
8 GiB` alone** and recovers at **`MemAvailable ≥ 16 GiB`**. On a swap-backed box, real
exhaustion drops MemAvailable below 8 GiB without a ≥10% full-stall, so whale-watchdog fires
and AIRA does not — AIRA's watchdog is **inert for this box's real pressure pattern** and
cannot replace whale-watchdog until fixed.

Second gap: observe-mode decisions emit only as audit events routed to *ready project
scopes* (`emitWatchdogEvent`, `watchdog.go:854-880`), journal only on `unrouted`. An operator
cannot see what would have been killed — the whole point of observe mode, and what blocks a
confident enforce-flip.

## 2. Direction (why MemAvailable-authoritative, PSI observational-only)

Sol's plan-review rejected an OR-hybrid (`memLow || psiStall`): a PSI-only trip can SIGKILL a
**legitimate heavy claude job** during an IO/CPU-driven PSI spike while memory is fine, and
`selectOffender` does NOT fence it — a heavy uncapped claude job that did not cause the stall
still satisfies all four offender predicates. For a **memory** killer, `MemAvailable < 8 GiB`
*is* the exhaustion signal; a PSI full-stall without low MemAvailable is not memory
exhaustion (it is IO/CPU, or a cgroup-confined job hitting its own `memory.max` — which is
*capped*, already excluded by the `uncapped` predicate and owned by the kernel/oomd). memLow
covers every real memory case, so a PSI trip adds only a false-positive kill path.

**Decision: the trip, recover, and escalation gates key on MemAvailable only (whale-watchdog
8 GiB/16 GiB parity). PSI is demoted to a best-effort, honestly-nullable observability
field.** A deliberate departure from #59's PSI-primary thesis; AIRA is not deployed
([[aira-not-live-no-compat]]) and the goal is parity with the proven killer we are retiring.

Both gates confirmed the core is sound: 8 GiB/16 GiB hysteresis does not flap, dropping the
`delta` conjunct from recovery is safe, MemAvailable-only escalation with failed reads
producing no escalation is conservative and honest, and grep confirms no code outside the
trigger path consumes the PSI baseline state.

## 3. Implementation

### 3.1 Trigger — MemAvailable-authoritative (`evaluateWatchdog`, rewrite `watchdog.go:191-253`)

MemAvailable is read first and is the SOLE gate; the memory decision + state update never
wait on PSI:

```
available, memOK, memReason := deps.readMemAvailable()   // authoritative
base := watchdogEvent{At: deps.now(), Mode: mode, MemAvailable: available}
if !memOK { state.armCount = 0; base.Decision,base.Reason = "unevaluated","memavailable:"+memReason; emit(withPSI); return }
if state.latched {
    if available >= deps.recoverMemAvailable { state.latched = false; state.armCount = 0; base.Decision = "recovered"; emit(withPSI) }
    return
}
if available < deps.lowMemAvailable { state.armCount++ } else { state.armCount = 0 }   // strict K-consecutive
if state.armCount < deps.debounce { return }
base.Decision = "trip"; emit(withPSI)
procs, err := deps.snapshotProcs()
if err != nil { state.armCount = 0; base.Decision,base.Reason = "unevaluated","process-snapshot:"+err.Error(); emit(withPSI); return }
acted := handleArmed(ctx, mode, deps, psi, available, procs)   // NOTE: no delta param (§3.4)
state.armCount = 0
state.latched = acted
```

- **Recover keys on `available >= 16 GiB` only.** New const `watchdogRecoverMemAvailable =
  int64(16 << 30)`; new `watchdogDeps.recoverMemAvailable`; **wired in `realWatchdogDeps`
  (`watchdog.go:556-578`)** and asserted by a test (§3.6). Drops the `delta == 0` conjunct
  (Sol v1 P0) — 8 GiB/16 GiB hysteresis, no flap.
- **Strict K=3 consecutive debounce** replaces the band-reset (`:233-237`). Arm latency
  ≈ K × interval = 3 × 2 s ≈ 6 s of *sustained* sub-8 GiB. Justified: the events we must catch
  are sustained seconds-to-minutes; 6 s buys transient-dip immunity, strictly safer than a
  hair-trigger. (Coverage boundary — see §5.)

### 3.2 PSI read placement — emit-time only, never in the decision path (Sol v2 P2)

`readPressure()` is read best-effort **only on polls that emit an event**, after the memory
decision + state update are computed, purely to populate diagnostic fields. Idle no-emit
polls do not read PSI at all. A slow/failed PSI read therefore cannot delay arm/recover/trip
or the state machine; it only affects the (already-decided) event's diagnostics. `readPressure`
is a bounded `/proc/pressure/memory` read (no network/lock). Read once per emitting poll and
pass the resulting `psi` sample into `handleArmed` so all armed-path events share one
consistent snapshot.

### 3.3 Honesty — PSI fields are nullable; no fabricated zero (Sol v2 P1 + Fable v2 P1)

The `watchdogEvent` PSI fields currently serialize unconditionally (`watchdog.go:66-68`, no
`omitempty`), so an unset/failed PSI read emits `psi_full_avg10: 0` / `psi_full_total_us: 0`
indistinguishable from a genuine calm reading — the "fake zero" AIRA's honesty rule bans, on
the very events the enforce-flip judgment reads. Fix:

- **Delete `PSIDelta` from `watchdogEvent`** (no longer computed anywhere) and drop the
  `psi_full_delta_us` JSON field.
- **Make `PSIAvg10 *float64` and `PSITotal *uint64` (`omitempty`)** — set both (from the
  `psi` sample) only when the PSI read succeeded; leave nil otherwise so an unavailable
  reading is *absent* from the JSON, never a measured 0. `emit(withPSI)` is the single place
  that stamps them.
- Fix B's log line prints `psi_avg10=?` when nil (§3.5).

### 3.4 Escalation gate — MemAvailable-only, signature change (Sol v1 P1 + Fable v2 P2-a)

`pressureStillTripped` (`watchdog.go:465-472`) → **`func pressureStillTripped(deps
watchdogDeps) bool`** returning bare bool:

```
available, ok, _ := deps.readMemAvailable()
return ok && available < deps.lowMemAvailable   // no PSI; mem read failure → false (conservative)
```

The caller (`watchdog.go:402`) drops the `currentTotal` return and the now-deleted
`intent.PSIDelta = currentTotal - total` line (`:420`); the `about_to_sigkill` intent's PSI
fields come from the shared `psi` snapshot (nullable, §3.3), never a subtraction — removing
the uint64 underflow → fabricated ~2⁶⁴ delta (Fable v1 P2-1). `handleArmed`'s `delta` param
is removed (Fable v2 P2-a); it takes the `psi` sample for enrichment.

### 3.5 Fix B — operator-visible decisions via an injectable, nil-guarded seam (Fable v2 P2-c)

Add `logf func(format string, args ...any)` to `watchdogDeps`, wired to `log.Printf` in
`realWatchdogDeps`. `emitWatchdog` calls it once per emitted event, **guarded `if deps.logf
!= nil`** (existing tests build deps without it):

```
aira daemon: watchdog <Decision>: <Outcome/Reason> mem_avail=<GiB> psi_avg10=<avg|?> [victim pid=<> comm=<> rss=<>]
```

Additive to the audit-event emit + `unrouted` fallback. Makes observe mode observable via
`journalctl --user -u aira-daemon.service` and lets enforce be verified from the journal.

### 3.6 Latch transition table — explicit (Sol v2 P1)

`state.latched = acted` where `acted` is `handleArmed`'s return. Specify each outcome (this
MATCHES current `handleArmed` behaviour — no code change, but it must be pinned + tested):

| outcome (mode) | latches? | rationale |
|---|---|---|
| `defer` — no qualifying offender (observe or enforce) | **NO** | pressure persists but claude isn't the cause; keep evaluating so a *newly-appearing* offender is caught. Re-emits a `defer` each debounce period — intended. |
| `would_signal` (observe) | YES | one would_signal per episode until recover |
| `would_signal` — interlock-degraded (enforce) | YES | same as observe; interlock held |
| signal attempted incl. `signal_sent,failure` (enforce) | YES | best-effort attempt made; retrying a failing signal every 6 s won't help; oomd + kernel oom.group are backstops |
| `degraded_no_signal` — pidfd unsupported | YES | nothing more AIRA can do this episode |

So "one decision per episode" holds **once an offender is actioned**; a sustained non-claude
low-mem period emits a `defer` each debounce period (correct — catches a later offender).

### 3.7 Excise dead PSI machinery (Fable v2 P2-b) + construction-time invariant (Sol v2 P2)

- **Delete** (grep-confirmed no external consumer): `watchdogState.haveTotal` / `lastTotal`;
  `watchdogDeps.tripPSIFullAvg10` / `recoverPSIFullAvg10`; consts `watchdogTripPSIFullAvg10`
  / `watchdogRecoverPSIFullAvg10`; their wiring in `realWatchdogDeps` (`:570-571`) and the
  test deps (`:100-101`). Leaving them would falsely imply PSI thresholds still gate.
  (`readPressure` stays — still used for observability.)
- **Validate the deps invariant fail-loud** at watchdog start (`runWatchdog`, before the
  loop): `lowMemAvailable > 0 && recoverMemAvailable > lowMemAvailable && debounce >= 1` and
  the required func fields (`readMemAvailable`, `readPressure`, `snapshotProcs`, `emitEvent`,
  pidfd*, `now`, `sleep`) non-nil. On violation → `log.Printf` a loud error and **do not run
  the watchdog loop** (visibly-off, never silently-inert). This is the general guard against
  the #59 silent-inertness class (a zero/miswired threshold → `available >= 0` vacuously true).

## 4. Tests (TDD; pure via the deps seam; proven RED both directions)

`baseWatchdogDeps` (`watchdog_test.go:56-110`) injects `readMemAvailable`/`readPressure`/
`emitEvent`; a varying mem read is a per-test field override.

- **Trigger table** (`evaluateWatchdog`): mem-low-alone (avail < 8 G) trips after **K=3
  consecutive** — RED vs current (never arms it); boundary avail == 8 G does NOT trip; single
  dip (K=1) no trip; latch holds until avail ≥ 16 G and unlatches there; mem read failure →
  `unevaluated` + armCount reset; **PSI read failure with low memory STILL trips** (parity —
  RED vs current `:194-199` which returns `unevaluated` before reading mem).
- **Honesty**: a poll with a failed PSI read emits an event with `PSIAvg10`/`PSITotal` **nil**
  (JSON field absent), never 0; `PSIDelta` no longer exists on the struct.
- **Latch table** (§3.6): a `defer` (no offender) does NOT latch — re-arms and re-emits next
  episode; an observe `would_signal` DOES latch — no second decision until avail ≥ 16 G.
- **Escalation** (`pressureStillTripped` → bool): mem-low → true (RED vs `:467`); avail ≥ 8 G
  → false; mem read failure → false.
- **Production wiring / invariant (Sol v2 P2 + Fable v1 P1-1):** assert `realWatchdogDeps(s)`
  carries `recoverMemAvailable == watchdogRecoverMemAvailable`, `lowMemAvailable ==
  watchdogLowMemAvailable`, `debounce == watchdogDebounce`, `logf != nil`, and that the
  invariant `low > 0 && recover > low && debounce >= 1` holds. A `runWatchdog` given
  invariant-violating deps logs + does not loop (does not emit/act).
- **Existing test inversions named** (Fable v2 P2-3 + P2-d): `watchdog_test.go:132` ("in band
  holds") and `:155-171` (`TestTriggerLatchesUntilGenuineRecovery`) both encode PSI-gated
  semantics being removed; replaced by the memory-trigger + latch tables. Deliberate.
- **Observability**: a fake `logf` records one line per emitted decision; an idle no-trip poll
  logs nothing (and reads no PSI).
- **Offender selection unchanged** (regression): capped / non-descendant / light / protected
  never selected; "defer — pressure elsewhere" when no offender qualifies.
- `go build ./... && go vet ./... && go test ./internal/daemon/ -race` green.

## 5. Scoped limitations (stated, not silent)

- **PSI observational-only** (design change from #59). A cgroup-confined stall with healthy
  machine MemAvailable is intentionally NOT actioned by AIRA — that job is capped
  (kernel/oomd owns it) and excluded by the `uncapped` predicate.
- **K=3 coverage boundary** (Sol v2 P2): a sub-~6 s memory collapse may be missed. This is a
  chosen debounce trade (transient-dip immunity), not hair-trigger parity; the sustained
  17:32-class events are caught comfortably. Tunable via `watchdogIntervalFromEnv` / debounce.
- **One actioned decision per episode** (§3.6): after an offender is actioned, latched until
  avail ≥ 16 GiB — whale-parity hysteresis. A non-claude low-mem period still emits a `defer`
  each debounce period, so the observe cadence is not mistaken for inertness.

## 6. After merge (owner-gated, unchanged endgame)

Rebuild `~/.local/bin/aira` + `aira install --watchdog=observe` (idempotent; if the unit is
unchanged it will NOT restart the daemon — so explicitly `systemctl --user restart
aira-daemon.service` to load the new binary). **Verify it now fires:** watch `journalctl
--user -u aira-daemon.service -f` (Fix B) through a real pressure event and confirm AIRA logs
a `would_signal` selecting the right offender. ONLY THEN: `--watchdog=enforce` → stop+disable
whale-watchdog (interlock releases) → verify AIRA is the live killer → retire whale-watchdog.
Box stays protected throughout by whale-watchdog + systemd-oomd + the aira.slice cap.
