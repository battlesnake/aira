# AIRA watchdog — MemAvailable trip parity + observe visibility

Status: PLAN — fixes a real gap the observe-mode deployment surfaced (#64, follow-up to
#59). Safety-critical KILL-trigger logic → full two-loop.

## 1. The gap (evidence)

Deployed on this box (aira-daemon, `--watchdog=observe`), AIRA's watchdog emitted **zero
events** across its whole life — including through a real 2026-08-24 17:32 memory event
(MemAvailable 7.5 GiB; a claude worker-fleet Σ=69 GiB / 156 procs) that fired the live
whale-watchdog, which SIGKILLed to recover. AIRA never armed.

Root cause (`watchdog.go:232`): `trip := avg >= 10.0 && delta > 0 && available < 8 GiB` —
it requires PSI `full` avg10 ≥ 10 **and** a positive PSI-total delta **and** low
MemAvailable, ALL three. whale-watchdog (weeks-proven) trips on **`MemAvailable < 8 GiB`
alone**. On a box with swap, a real memory-exhaustion event drops MemAvailable below 8 GiB
without necessarily producing ≥10% *full-stall* time, so whale-watchdog fires and AIRA
does not. AIRA's watchdog is effectively **inert for this box's real pressure pattern** —
it cannot replace whale-watchdog until this is fixed.

Second gap: observe-mode decisions are emitted only as audit events routed to *ready
project scopes* (`emitWatchdog`, `watchdog.go:860-875`), with a journal line only on
`unrouted`. So an operator cannot see what the watchdog would have killed — which is the
whole point of observe mode, and what blocks a confident enforce-flip.

## 2. Fix A — MemAvailable-primary trip (whale-watchdog parity), PSI retained as OR

In `evaluateWatchdog` (`watchdog.go:222-237`), replace the AND-gated trip with a trip that
fires on EITHER signal, so AIRA is at least as sensitive as whale-watchdog AND its own
current PSI path:

```
memLow   := available < deps.lowMemAvailable                 // 8 GiB — whale-watchdog parity (proactive)
psiStall := avg >= deps.tripPSIFullAvg10 && delta > 0        // real full-stall (existing)
trip     := memLow || psiStall
```

Debounce stays K=3 **consecutive** (`if trip { armCount++ } else { armCount = 0 }` —
replacing the current confusing `avg<=recover || avg>=trip` reset at :235-237, which is not
a clean consecutive-count). Latch-until-recover stays.

**Hysteresis for the mem path (new constant + dep):** `watchdogRecoverMemAvailable =
16 GiB` (whale-watchdog's recoveryTarget), added to `watchdogDeps.recoverMemAvailable`.
Unlatch only when memory AND psi have BOTH calmed (conservative — avoids flapping):

```
recovered := available >= deps.recoverMemAvailable && avg <= deps.recoverPSIFullAvg10 && delta == 0
```

Rationale for OR (not replacing PSI with mem): a machine-wide memory killer must catch
"machine is running out of RAM" (MemAvailable — the proven signal) AND a genuine
full-stall even if MemAvailable reads OK (cache-heavy edge). Over-trip risk is bounded by
the unchanged offender selection: it only kills **uncapped + claude-descendant + heavy +
not-protected** processes and DEFERS ("pressure elsewhere") when claude isn't the cause —
so an early trip with no qualifying offender is a logged no-op, never a wrong kill.

**Reconcile the escalation gate (REQUIRED — else arm-then-abort).** `pressureStillTripped`
(`watchdog.go:465-471`) gates SIGTERM→SIGKILL escalation with the SAME strict AND
(`avg >= trip && total > prev && available < low`). If only the arm-trip becomes OR, a
mem-low-alone trip would SIGTERM and then, at the post-grace recheck, find `avg < trip` →
not-still-tripped → the offender that ignored SIGTERM survives. So `pressureStillTripped`
must use the identical OR:

```
memLow   := available < deps.lowMemAvailable
psiStall := avg >= deps.tripPSIFullAvg10 && total > previousTotal
stillTripped := ok && memOK && (memLow || psiStall)
```

(the `total <= previousTotal` early-out is folded into `psiStall`; a mem-low read failure →
not-still-tripped, conservative — no escalation on unknown state).

Nothing else changes: `selectOffender`, the pidfd TOCTOU-safe kill, the interlock, the
four predicates, and the observe/enforce/off gating are all untouched.

## 3. Fix B — operator-visible decisions (journal)

`emitWatchdog` is already called only on decision-level events (trip / recovered /
would_signal / signalled / deferred / unevaluated-on-error), never per idle poll. Add, for
every emitted event, a concise `log.Printf` so `journalctl --user -u aira-daemon.service`
shows watchdog activity without needing the audit stream:

```
aira daemon: watchdog <Decision>: <Outcome/Reason> psi_avg10=<avg> mem_avail=<GiB> [victim pid=<> comm=<> rss=<>]
```

This is additive to the existing event-emit (keep the audit events + the `unrouted`
fallback). It makes observe mode actually observable and lets `--watchdog=enforce` be
verified from the journal.

## 4. Tests (TDD; pure via the deps seam; proven RED both directions)

- Trigger table (`evaluateWatchdog` with injected readPressure/readMemAvailable): **mem-low
  alone (avail < 8G, avg10 = 0, delta = 0) trips after K=3** (the 17:32 case — RED against
  the current AND-gated impl); psi-stall alone (avg10 ≥ 10, delta > 0, avail = 20G) trips;
  neither → no trip; a single-poll dip (K=1) does NOT trip (debounce); latch holds until
  BOTH mem ≥ 16G AND psi calm (mem-recovered-but-psi-high stays latched, and vice versa);
  a psi/mem read failure → `unevaluated`, armCount reset, never a trip.
- `pressureStillTripped`: mem-low-alone (avail < 8G, avg10 = 0) returns still-tripped
  (RED against the current AND) so a mem-low arm actually escalates to SIGKILL; psi-stall
  alone still-tripped; neither → false; a mem read failure → false (conservative).
- Offender selection unchanged (regression): capped / non-descendant / light / protected
  never selected; deferred when no qualifying offender even though tripped.
- Observability: a fake logf/logger records a line for each emitted decision; an idle
  no-trip poll logs nothing.
- `go build ./... && go vet ./... && go test ./internal/daemon/ -race` green.

## 5. After merge (owner-gated, unchanged from the endgame)

Rebuild `~/.local/bin/aira` + `aira install` (idempotent; picks up the new daemon binary —
a `--watchdog` unchanged run won't restart it, so explicitly `systemctl --user restart
aira-daemon.service` to load the new binary, OR `aira install --watchdog=observe` which
restarts on a changed unit... note: unit unchanged → add an explicit restart step).
**Verify it now fires:** watch `journalctl --user -u aira-daemon.service -f` (Fix B) through
a real pressure event and confirm AIRA logs a `would_signal` selecting the right offender.
ONLY THEN: `--watchdog=enforce` → stop+disable whale-watchdog (interlock releases) → verify
AIRA is the live killer → retire whale-watchdog. Box stays protected throughout by
whale-watchdog + systemd-oomd + the aira.slice cap.
