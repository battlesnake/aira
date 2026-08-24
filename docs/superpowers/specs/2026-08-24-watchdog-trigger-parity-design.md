# AIRA watchdog — MemAvailable-authoritative trip (whale-watchdog parity) + observe visibility

Status: PLAN v2 — reconciles the Sol plan-review (GATE-FAIL on the OR-hybrid's causality +
a real recover-latch P0) and the Fable code-grounded gate (GATE-PASS-WITH-NITS; P1 wiring
test + underflow + seam nits). Fixes a real inertness the observe-mode deployment surfaced
(#64, follow-up to #59). Safety-critical KILL-trigger logic → full two-loop, re-gated.

## 1. The gap (evidence)

Deployed on this box (aira-daemon, `--watchdog=observe`), AIRA's watchdog emitted **zero
events** across its whole life — including through a real 2026-08-24 17:32 memory event
(MemAvailable 7.5 GiB; a claude worker-fleet Σ=69 GiB / 156 procs) that fired the live
whale-watchdog, which SIGKILLed to recover. AIRA never armed.

Root cause (`watchdog.go:232`): `trip := avg >= 10.0 && delta > 0 && available < 8 GiB` —
it requires PSI `full` avg10 ≥ 10 **and** a positive PSI-total delta **and** low
MemAvailable, ALL three (and `:235` even resets the debounce count whenever avg is calm, so
a mem-low-without-stall poll can never accumulate). whale-watchdog (weeks-proven) trips on
**`MemAvailable < 8 GiB` alone** and recovers at **`MemAvailable ≥ 16 GiB`**. On a box with
swap, real memory exhaustion drops MemAvailable below 8 GiB without necessarily producing a
≥10% *full-stall*, so whale-watchdog fires and AIRA does not. AIRA's watchdog is effectively
**inert for this box's real pressure pattern** — it cannot replace whale-watchdog until this
is fixed.

Second gap: observe-mode decisions are emitted only as audit events routed to *ready
project scopes* (`emitWatchdog` / `emitWatchdogEvent`, `watchdog.go:854-880`), with a
journal line only on `unrouted`. So an operator cannot see what the watchdog would have
killed — which is the whole point of observe mode, and what blocks a confident enforce-flip.

## 2. Direction (why MemAvailable-authoritative, not an OR-hybrid)

The v1 plan proposed `trip := memLow || psiStall`. Sol's plan-review correctly rejected it:
a PSI-only trip can SIGKILL a **legitimate heavy claude job** during an IO/CPU-driven PSI
spike while memory is fine — and `selectOffender` does NOT fence this, because a heavy
uncapped claude job that did *not* cause the stall still satisfies all four offender
predicates. For a **memory** killer, `MemAvailable < 8 GiB` *is* the exhaustion signal; a
PSI full-stall without low MemAvailable is not memory exhaustion (it is IO/CPU, or a
cgroup-confined job hitting its own `memory.max` — which is *capped*, so already excluded by
the `uncapped` predicate and handled by the kernel/oomd). memLow already covers every real
memory case, so the PSI trip adds only a false-positive kill path with no compensating
benefit. **Decision: the trip, recover, and escalation gates key on MemAvailable only,
matching the weeks-proven whale-watchdog. PSI is demoted to an observability field.**

This is a deliberate departure from #59's PSI-primary thesis. AIRA is not deployed
([[aira-not-live-no-compat]]), and the whole point of this change is parity with the proven
killer we are about to retire — so matching its exact MemAvailable 8 GiB/16 GiB trigger is
the target, not an unproven refinement.

## 3. Fix A — MemAvailable-authoritative trigger

Rewrite `evaluateWatchdog` (`watchdog.go:191-253`) so **MemAvailable is read independently
and is the sole gate**; PSI is read best-effort only to populate the event's diagnostic
fields, and a PSI read failure NEVER discards the memory signal (fixes Sol "MemAvailable is
not actually primary" + Fable P2-4):

```
available, memOK, memReason := deps.readMemAvailable()   // authoritative
avg, total, psiOK, _        := deps.readPressure()        // best-effort, observability only
base := watchdogEvent{At: now, Mode: mode, MemAvailable: available}
if psiOK { base.PSIAvg10, base.PSITotal = avg, total }
if !memOK { state.armCount = 0; base.Decision,base.Reason = "unevaluated","memavailable:"+memReason; emit; return }

if state.latched {
    if available >= deps.recoverMemAvailable { state.latched = false; state.armCount = 0; base.Decision = "recovered"; emit }
    return
}
if available < deps.lowMemAvailable { state.armCount++ } else { state.armCount = 0 }   // strict K-consecutive
if state.armCount < deps.debounce { return }
base.Decision = "trip"; emit
// snapshot + handleArmed unchanged (delta arg becomes 0 / unused)
```

- **Recover keys on MemAvailable ≥ 16 GiB only** — drops the `delta == 0` conjunct that made
  the latch stick forever (Sol P0) and drops PSI from recover. New constant
  `watchdogRecoverMemAvailable = int64(16 << 30)`, new `watchdogDeps.recoverMemAvailable`
  field, **wired in `realWatchdogDeps` (`watchdog.go:556-578`)** and asserted by a test
  (Fix in §5, Fable P1-1). Hysteresis 8 GiB/16 GiB ⇒ no flap.
- **Strict K=3 consecutive debounce** (`if memLow { armCount++ } else { armCount = 0 }`)
  replaces the current band-reset (`:233-237`). Arm latency = K × interval ≈ 3 × 2 s ≈ 6 s
  of *sustained* sub-8 GiB. Justified: the events we must catch (a 69 GiB fleet) are
  sustained for many seconds-to-minutes; a 6 s debounce buys immunity to a transient dip the
  kernel self-reclaims and is strictly safer than a hair-trigger. (If real events are ever
  observed shorter, interval/debounce tighten — configurable via `watchdogIntervalFromEnv`.)
- The PSI bootstrap (`haveTotal`), counter-reset handling, and `delta` gating are **removed
  from the trigger path** (PSI no longer gates anything), so the memory trip is not delayed a
  poll behind a PSI baseline and cannot be skipped by a PSI read failure.
- Everything downstream is untouched: `selectOffender` + the four predicates, the "defer —
  pressure elsewhere" path when no offender qualifies, observe→`would_signal`, the interlock
  degrade-to-observe, the pidfd TOCTOU-safe SIGTERM→grace→SIGKILL, revalidation-before-signal.

## 4. Fix A cont. — escalation gate (pressureStillTripped), no underflow

`pressureStillTripped` (`watchdog.go:465-472`) gates SIGTERM→SIGKILL escalation with the old
strict AND. Make it **MemAvailable-only** so a mem-low arm actually escalates:

```
available, ok, _ := deps.readMemAvailable()
return ok && available < deps.lowMemAvailable    // no PSI required (Sol P1)
```

The sigkill-intent event's PSI fields are best-effort (re-read PSI for display if desired)
and are **never computed by subtraction** — the current `intent.PSIDelta = currentTotal -
total` (`watchdog.go:420`) is deleted, removing the uint64-underflow → fabricated ~2⁶⁴ delta
that a PSI counter-reset during grace could now produce (Fable P2-1, honesty).

## 5. Fix B — operator-visible decisions (journal), via an injectable seam

`emitWatchdog` is called only on decision-level events (trip / recovered / would_signal /
defer / intent / outcome / unevaluated-on-error), never per idle poll. Add a
`logf func(format string, args ...any)` field to `watchdogDeps`, wired to `log.Printf` in
`realWatchdogDeps` (Fable P2-2 — an injectable dep, not a flaky global `log.SetOutput`
capture). `emitWatchdog` calls `deps.logf` once per emitted event:

```
aira daemon: watchdog <Decision>: <Outcome/Reason> mem_avail=<GiB> psi_avg10=<avg> [victim pid=<> comm=<> rss=<>]
```

Additive to the existing audit-event emit + `unrouted` fallback. Makes observe mode actually
observable via `journalctl --user -u aira-daemon.service` and lets `--watchdog=enforce` be
verified from the journal.

## 6. Tests (TDD; pure via the deps seam; proven RED both directions)

The `baseWatchdogDeps` seam (`watchdog_test.go:56-110`) injects `readPressure` /
`readMemAvailable` / `emitEvent`; a *varying* mem read is a per-test field override (trivial).

- **Trigger table** (`evaluateWatchdog`): mem-low-alone (avail < 8 G) trips after **K=3
  consecutive** — **RED** against the current AND-gated + band-reset impl (which never arms
  it); boundary avail == 8 G does NOT trip (`<`, not `<=`); a single-poll dip (K=1) does not
  trip; latch holds until avail ≥ 16 G and unlatches there; a mem read failure → `unevaluated`
  + armCount reset, never a trip; **a PSI read failure with low memory STILL trips** (the
  parity fix — RED against current `:194-199` which returns `unevaluated` before reading mem).
- **Escalation** (`pressureStillTripped`): mem-low-alone → still-tripped (RED vs `:467`); avail
  ≥ 8 G → false; mem read failure → false (conservative, no escalation on unknown state).
- **Production wiring (Fable P1-1, the #59 silent-inert class):** assert `realWatchdogDeps(s)`
  carries `recoverMemAvailable == watchdogRecoverMemAvailable` (and, cheaply, `lowMemAvailable`
  / `debounce`) — a forgotten wire → zero-value 0 → `available >= 0` vacuously true → mem
  hysteresis silently inert on the real host while injected-deps tests stay green. This is the
  exact fixtures-mask-inertness failure that bit #59; a wiring assertion is mandatory.
- **Existing test inversion (Fable P2-3):** `watchdog_test.go:132` "in band holds" encodes the
  band-reset being removed; it is replaced by memory-trigger cases (the PSI-reading tests
  become observability-field assertions, not trigger assertions). Listed as a deliberate
  semantic tightening.
- **Observability (Fix B):** a fake `logf` records one line per emitted decision; an idle
  no-trip poll logs nothing.
- **Offender selection unchanged** (regression): capped / non-descendant / light / protected
  never selected; "defer — pressure elsewhere" when no offender qualifies even though tripped.
- `go build ./... && go vet ./... && go test ./internal/daemon/ -race` green.

## 7. Scoped limitations (stated, not silent)

- **PSI is observational-only** now (design change from #59). A cgroup-confined stall with
  machine MemAvailable healthy is intentionally NOT actioned by AIRA — that job is capped
  (kernel/oomd owns it) and excluded by the `uncapped` predicate anyway.
- **One decision per episode** (Fable P2-5): after a `trip`/`would_signal`, the watchdog is
  latched until avail ≥ 16 GiB, so a long sub-16 GiB period suppresses subsequent decisions.
  This is whale-watchdog-parity hysteresis (its recoveryTarget is also 16 GiB) and is
  intended; noted so the observe-mode event cadence is not mistaken for inertness.

## 8. After merge (owner-gated, unchanged endgame)

Rebuild `~/.local/bin/aira` + `aira install --watchdog=observe` (idempotent; if the unit is
unchanged it will not restart the daemon — so explicitly `systemctl --user restart
aira-daemon.service` to load the new binary). **Verify it now fires:** watch `journalctl
--user -u aira-daemon.service -f` (Fix B) through a real pressure event and confirm AIRA logs
a `would_signal` selecting the right offender. ONLY THEN: `--watchdog=enforce` → stop+disable
whale-watchdog (interlock releases) → verify AIRA is the live killer → retire whale-watchdog.
Box stays protected throughout by whale-watchdog + systemd-oomd + the aira.slice cap.
