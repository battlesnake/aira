# AIRA watchdog — enforce-path hardening (multi-offender latch + audit honesty)

Status: PLAN — pre-flip hardening (#65, follow-up to #64). A cross-lineage flip-review of the
LIVE enforce kill code (Opus build-review: no live defect; **Sol (OpenAI) BLOCK + DeepSeek
BLOCK**, converging independently) surfaced real enforce-path gaps before making AIRA the sole
live machine memory killer. Safety-critical KILL logic → full two-loop, then re-run the
cross-lineage flip-review; flip only when all three lineages approve.

## 1. The findings (what the flip-review converged on)

The #64 trigger fix (MemAvailable-authoritative) was accepted by all lineages. The blocks are
on the **enforce kill path inherited from #59, which has never run live** (it shipped
OFF/observe), so the flip is its first going-live scrutiny.

**F1 (P0/P1 — load-bearing): the latch is too sticky for a MULTI-offender event.** After
`handleArmed` kills one offender's subtree it returns `true`, so `evaluateWatchdog` sets
`state.latched = true` and then, on every subsequent poll, returns early until MemAvailable ≥
16 GiB (`watchdog.go` latched branch). But on this box's real workload — several concurrent
agent/worktree jobs, exactly the Σ=69 GiB / 156-proc fleet that caused the 17:32 event —
killing ONE subtree frequently does not recover memory, because the OTHER heavy offenders are
still eating. AIRA then sits latched, never pursuing them, until a recovery that will not come.
whale-watchdog (the proven killer) re-evaluates every poll and keeps reaping until recovery.
Both external lineages flagged this as the primary blocker.

**F2 (P2 — honesty): audit outcomes overclaim.** On `ESRCH` the outcome is recorded as
`"signal_sent,exited"` although no signal was sent (the target had already exited). Observe
always reports `"WOULD SIGKILL"` even though the real sequence is SIGTERM → grace → SIGKILL.
Both lineages flagged both.

**Verified NOT defects (both reviewers under-weighted the code; document, do not change):**
- Subtree kill of the offender's light/non-heavy children: `offenderSubtree` reaps the whole
  runaway job tree (oom.group-style; leaving orphans holding memory is the real bug). The
  **safety-critical** predicates ARE re-checked fresh per-target immediately before each signal
  — `revalidateWatchdogTarget` re-reads `cgroupOf` and rejects `!uncapped`, `hasAIRAComponent`,
  and `watchdogProtected` — so a now-capped / AIRA / protected member is skipped. Only RSS and
  claude-ancestry are not re-checked, and those hold by construction for the offender's own
  descendants. No change; add a clarifying comment so this is not re-flagged.
- cgroup/protection TOCTOU: `revalidateWatchdogTarget` re-reads cgroup + protection fresh right
  before each signal, minimising the window. The residual revalidate→signal gap is unavoidable
  without kernel-atomic classification and is acceptable. No change; document.

## 2. Fix F1 — pursue multiple offenders while critically low (three-way latched branch)

Redefine the `latched` state as "an active kill episode is in progress" and make the latched
branch of `evaluateWatchdog` a three-way on MemAvailable, using the existing 8 GiB / 16 GiB
thresholds as a natural stop:

```
if state.latched {
    switch {
    case available >= deps.recoverMemAvailable:          // ≥16 GiB — recovered
        state.latched, state.armCount, state.cooldown = false, 0, 0
        base.Decision = "recovered"; emit(withPSI); return
    case available < deps.lowMemAvailable:               // <8 GiB — still CRITICAL: pursue next offender
        if state.cooldown > 0 { state.cooldown--; return }   // settle since last action
        act(...)                                          // snapshot + handleArmed, WITHOUT re-debounce
        return
    default:                                             // 8–16 GiB — recovering: HOLD, do not kill more
        return
    }
}
```

- **Kills only while `available < lowMemAvailable` (8 GiB).** The moment a kill pushes
  MemAvailable to ≥ 8 GiB the watchdog stops acting (enters the "recovering" hold band), so it
  reaps exactly enough to clear the critical zone — **no over-kill** — then holds until full
  16 GiB recovery. This is the key safety property that makes "keep reaping" safe.
- **Re-arm skips the K=3 debounce** (we are already in a confirmed episode); the debounce only
  gates *entering* an episode, matching whale-watchdog (debounce to start, then reap until
  recovery).
- **Cooldown (settle) between actions.** New `state.cooldown` counter, set after every action to
  `watchdogReArmCooldown` polls (derived from `postKillSettle`, ≈ `ceil(postKillSettle/interval)`,
  min 1). In enforce, `handleArmed` already blocks through grace (5 s) + postKillSettle (1 s) per
  kill, so the SIGKILL has time to free memory before the next selection; the cooldown mainly
  paces OBSERVE (where `handleArmed` does not sleep) so it does not emit a would_signal every
  2 s. Bounds the event/log rate in both modes to one action per ~settle.
- If `handleArmed` returns `false` (defer — no qualifying offender, or all-retryable-failure),
  `state.latched` becomes `false` and the next poll re-debounces — unchanged, consistent with
  #64's "defer/failed-round does not latch" rule.

Refactor so the initial-arm path and the re-arm-while-latched path share one `act` helper
(emit trip on first entry only, snapshot, `handleArmed`, set `latched`/`cooldown`). Reset
`state.cooldown = 0` wherever `armCount` is reset (mem-read failure, recovered, defer) so a stale
cooldown never suppresses a fresh episode.

**Why this fixes F1 without over-killing:** a multi-offender event (avail 5 GiB, offenders A,B,C)
now reaps A (kill, cooldown), still <8 GiB → reaps B, still <8 GiB → reaps C, until avail ≥ 8 GiB,
then holds through 8–16 GiB, then exits at 16 GiB — instead of killing only A and latching.

## 3. Fix F2 — honest audit outcomes

- `ESRCH` (target exited before the signal): outcome `"already_exited"` (not
  `"signal_sent,exited"`) — no signal was sent. Applies to both the SIGTERM and SIGKILL loops and
  the pidfd_open ESRCH path (already `"exited"`, keep).
- Observe (and interlock-degraded) `Outcome`: `"would_signal: SIGTERM→SIGKILL"` (not
  `"WOULD SIGKILL"`) — reflect the real escalation sequence. Keep `Decision = "would_signal"`.

## 4. Tests (TDD; pure via the deps seam; proven RED both directions)

- **F1 multi-offender (the load-bearing test):** enforce mode, `readMemAvailable` returns a
  sequence that stays < 8 GiB across several polls with a proc set containing ≥2 qualifying
  offenders (distinct subtrees); assert `handleArmed` / the signal path is invoked for **each**
  offender across successive polls (pursues the second offender) — **RED** against the current
  latch-until-16G impl (which acts once then idles). Use an injected `pidfdSignal`/`snapshotProcs`
  recording which pids were signalled; drive polls via `evaluateWatchdog` with a mutating snapshot
  (offender A gone after its kill, B still present).
- **F1 no-over-kill:** once `readMemAvailable` returns ≥ 8 GiB (recovering band), no further action
  is taken even though still latched and < 16 GiB; at ≥ 16 GiB → `recovered` + unlatch. RED against
  a naive "kill every poll while latched" impl (which would keep killing in the 8–16 band).
- **F1 cooldown/settle:** two consecutive < 8 GiB polls right after an action do not both act (the
  cooldown holds the second) — bounds the action rate.
- **F1 observe cadence:** a sustained < 8 GiB episode in observe emits `would_signal` paced by the
  cooldown, not one per 2 s poll; and still pursues distinct offenders.
- **F1 defer still unlatches:** latched + < 8 GiB but no qualifying offender → defer → unlatch →
  next poll re-debounces (unchanged rule preserved).
- **F2:** an `ESRCH` on `pidfdSignal` yields outcome `"already_exited"` (RED vs `"signal_sent,exited"`);
  observe outcome is `"would_signal: SIGTERM→SIGKILL"` (RED vs `"WOULD SIGKILL"`).
- Regression: the #64 trigger/recover/debounce/honesty/invariant tests stay green; single-offender
  enforce still latches-then-recovers exactly as before once memory recovers.
- `go build ./... && go vet ./... && go test ./internal/daemon/ -race` green.

## 5. After merge (unchanged endgame, owner-gated)

Rebuild `~/.local/bin/aira` + restart the daemon (still `--watchdog=observe`). **Re-run the
cross-lineage flip-review** (Opus build-review Workflow + Sol + DeepSeek, ideally Gemini if
available) on the hardened enforce code; flip to `--watchdog=enforce` only when **all three
lineages approve**. Then stop+disable whale-watchdog (interlock releases → AIRA live) → retire
whale-watchdog. Box protected throughout by whale-watchdog + systemd-oomd + the aira.slice cap.
