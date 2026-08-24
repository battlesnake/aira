# AIRA watchdog — enforce-path hardening (multi-offender pursuit + audit honesty)

Status: PLAN v2 — folds Sol GATE-FAIL (transient-dip re-arm, imprecise no-over-kill claim,
contradictory cooldown reset) and Fable GATE-PASS-WITH-NITS (cooldown derivation vs the deps
seam, a third overclaiming outcome string, snapshot-error-during-re-arm, prefix preservation).
Pre-flip hardening (#65, follow-up to #64) after a cross-lineage flip-review of the LIVE enforce
code (Opus build-review: no live defect; **Sol + DeepSeek both BLOCK**, independently).
Safety-critical KILL logic → full two-loop, then re-run the cross-lineage flip-review; flip only
when all three lineages approve.

## 1. The findings (what the flip-review converged on)

The #64 MemAvailable-authoritative trigger was accepted by every lineage. The blocks are on the
**enforce kill path inherited from #59, which has never run live** (shipped OFF/observe).

**F1 (P0/P1 — load-bearing): the latch is too sticky for a MULTI-offender event.** After
`handleArmed` kills one offender's subtree it returns `true`, so `evaluateWatchdog` sets
`state.latched` and then returns early on every poll until MemAvailable ≥ 16 GiB
(`watchdog.go:225-233`). On this box's real workload — several concurrent agent/worktree jobs,
the Σ=69 GiB / 156-proc fleet that caused the 17:32 event — killing ONE subtree usually does not
recover memory (the OTHER heavy offenders keep eating), and AIRA then sits latched forever.
whale-watchdog re-evaluates every poll and keeps reaping until recovery. `handleArmed` reaps
exactly ONE offender per call (`selectOffender` returns a single max-RSS proc; `offenderSubtree`
is only that offender's tree — confirmed `watchdog.go:301-322,357`).

**F2 (P2 — honesty): three audit outcomes overclaim a signal was sent.**
`"signal_sent,exited"` on ESRCH (`:417,:460` — no signal sent, target already exited),
`"signal_sent,failure"` on other errors (`:423,:465` — EPERM etc., no signal delivered), and
observe's `"WOULD SIGKILL"` (`:341,347,352` — the real path is SIGTERM → grace → SIGKILL).

**Verified NOT defects (document, do not change):**
- Subtree kill reaps the offender's whole tree (oom.group-style); the SAFETY predicates
  (uncapped / not-AIRA / not-protected) ARE re-checked fresh per-target in
  `revalidateWatchdogTarget` before each signal, so a now-capped/protected member is skipped.
  Only RSS/claude-ancestry aren't re-checked, and they hold by construction for the offender's
  descendants. Add a clarifying comment.
- cgroup/protection TOCTOU: `revalidateWatchdogTarget` re-reads cgroup + protection fresh right
  before each signal; residual revalidate→signal gap is unavoidable and acceptable. Document.
- After a kill the same offender can be re-selected next act if not yet reaped (RSS still
  reported); benign — revalidation + ESRCH + the cooldown absorb it. Add a comment.

## 2. Fix F1 — pursue offenders while continuously critical (two-flag state machine)

`evaluateWatchdog` is driven by a synchronous poll loop (`runWatchdog:174-184`); in enforce
`handleArmed` blocks through grace(5 s) + postKillSettle(1 s) per kill. Extend `watchdogState`
with two fields (it is a local in `runWatchdog`, passed by pointer — no other wiring):

```
type watchdogState struct {
    armCount    int   // debounce counter (episode ENTRY only)
    latched     bool  // in an episode: acted, not yet fully recovered to 16 GiB (gates the "recovered" event)
    criticalRun bool  // continuous <8 GiB run in which we have already acted → skip debounce, cooldown-paced re-kills
    cooldown    int   // settle polls remaining before the next re-kill
}
```

New package const (interval is NOT in `watchdogDeps`, so derive from the default — it evaluates
to 1 for every legal interval since postKillSettle ≤ min interval):

```
watchdogReArmCooldown = (watchdogPostKillSettle + defaultWatchdogInterval - 1) / defaultWatchdogInterval // = 1, min 1
```

Per-poll transition table (after reading `available`; `low`=8 GiB, `recover`=16 GiB):

| condition | armCount | criticalRun | latched | cooldown | action |
|---|---|---|---|---|---|
| `!memOK` | 0 | (keep) | (keep) | 0 | emit `unevaluated`; return |
| `available >= recover` | 0 | false | false | 0 | if was `latched`: emit `recovered`; return |
| `low <= available < recover` (HOLD) | 0 | **false** | (keep) | 0 | return — **no new kill**; clearing `criticalRun` forces re-debounce on the next dip |
| `available < low` **and** `criticalRun` | (0) | true | — | see act | if `cooldown>0`: `cooldown--`, return; else `act(entry=false)` |
| `available < low` **and** `!criticalRun` | `++`; if `<debounce` return | — | — | — | `act(entry=true)` once `armCount>=debounce` |

`act(entry)`:
1. if `entry`: emit `trip` (a re-kill within a `criticalRun` is NOT a fresh trip — so `trip`
   count == number of debounced episode-entries, one per continuous critical run).
2. snapshot procs. **On error: emit `unevaluated`; keep `criticalRun`/`latched` (a live episode
   is not ended by a transient snapshot failure); `armCount=0`; return** (retry next poll).
3. read PSI (observe only — off the enforce kill path, unchanged from #64); `acted :=
   handleArmed(...)`.
4. if `acted`: `latched=true; criticalRun=true; cooldown=watchdogReArmCooldown`.
   else (defer / all-retryable-failure): `latched=false; criticalRun=false; cooldown=0` (the #64
   "defer/failed round does not latch" rule — the episode drops; a still-critical next poll
   re-debounces).
5. `armCount=0`.

**Cooldown transitions are now unambiguous (Sol P1):** `cooldown` is SET (>0) only in `act` step 4
on `acted`; DECREMENTED only in the `criticalRun` + `<low` branch; ZEROED on `!memOK`, `recover`,
HOLD, and defer/unlatch. An ordinary `armCount=0` never touches it. It cannot wedge (its sole
decrement site is also its sole consult site).

**No-over-kill, stated precisely (Sol P1):** AIRA *initiates* an offender action only from a
`< 8 GiB` sample; the moment memory reaches the 8–16 GiB HOLD band it initiates no new kills, so
it reaps exactly enough to clear the critical zone. An action already begun completes under
`handleArmed`'s own logic (`pressureStillTripped` still suppresses the SIGKILL escalation in-call
if memory recovered ≥ 8 GiB during grace — intentional; a SIGTERM that sufficed is not
gratuitously escalated).

**Anti-flap (Sol P1):** because `criticalRun` is cleared whenever `available >= low`, a dip back
below 8 GiB after any HOLD/recover excursion must re-satisfy the K=3 debounce before the next
kill — so threshold oscillation (7.9↔8.1 GiB) cannot cause repeated un-debounced kills.

## 3. Fix F2 — honest audit outcomes

- ESRCH (`:417,:460`): `"already_exited"` (no signal sent). pidfd_open ESRCH stays `"exited"`.
- Other signal error (`:423,:465`): `"signal_failed"` (no signal delivered).
- Observe / interlock-degraded outcome: `"would_signal: SIGTERM; SIGKILL after grace if still
  alive"`, preserving the degraded prefix, i.e. `:347/:352` become
  `"degraded_to_observe; would_signal: SIGTERM; SIGKILL after grace if still alive"`. `Decision`
  stays `"would_signal"`.

## 4. Tests (TDD; pure via the deps seam; proven RED both directions)

The seam (`baseWatchdogDeps`) already overrides `readMemAvailable`/`snapshotProcs` per-poll and
records signalled pids via the fd↔pid stub; a mutating shared `procs` map between polls models
"offender A reaped, B remains".

- **F1 multi-offender (load-bearing):** enforce; a proc set with two qualifying offenders in
  distinct subtrees; MemAvailable stays < 8 GiB; A disappears from the snapshot after its kill.
  Assert both A and B are signalled across successive polls (pursues the second) — **RED** vs the
  current latch-until-16G impl (acts once, then idles).
- **F1 no-over-kill:** once MemAvailable returns ≥ 8 GiB (HOLD band) no further action even while
  latched and < 16 GiB; at ≥ 16 GiB → `recovered` + full reset. RED vs a naive "kill every poll
  while latched".
- **F1 anti-flap / re-debounce after HOLD:** [<8G×3 → act] → one ≥8G HOLD poll → single <8G poll
  does NOT act (criticalRun cleared → needs K=3 again); RED vs "skip debounce whenever latched".
- **F1 cooldown/settle:** two consecutive <8G polls right after an act do not both act (cooldown
  holds the second). Assert exact cadence, not just "paced".
- **F1 observe cadence:** sustained <8G in observe emits `would_signal` paced by the cooldown
  (~every other poll), pursuing distinct offenders — not one per poll.
- **F1 trip-once-per-run (Fable P2):** across a multi-act continuous critical run, exactly ONE
  `trip` decision is emitted (re-kills are not trips).
- **F1 snapshot-error during re-arm (Fable P2):** a snapshot error while `criticalRun` emits
  `unevaluated` and stays latched (does NOT unlatch/end the episode); next poll retries.
- **F1 defer still unlatches:** latched + < 8 GiB but no qualifying offender → defer → unlatch →
  next poll re-debounces (the #64 rule preserved).
- **F1 restart zeroing:** a fresh `watchdogState{}` has no criticalRun/latched/cooldown — the
  observe→enforce flip restarts the daemon, so no stale episode carries.
- **F2:** ESRCH → `"already_exited"`; other error → `"signal_failed"`; observe outcome →
  `"would_signal: SIGTERM; SIGKILL after grace if still alive"` (each RED vs the old string).
- **Existing-test review (Fable P1):** `TestTriggerLatchesUntilMemoryRecoveryThreshold`
  (`watchdog_test.go:219-243`, wants would_signal==2) must be re-read under the new semantics and
  confirmed to still assert the INTENDED behaviour (two distinct debounced episodes separated by a
  ≥16 GiB recovery), not merely pass because cooldown≥1 absorbs a stray poll. Update its comment/
  shape if it now proves the wrong thing.
- `go build ./... && go vet ./... && go test ./internal/daemon/ -race` green.

## 5. After merge (unchanged endgame, owner-gated)

Rebuild `~/.local/bin/aira` + restart the daemon (still `--watchdog=observe`). **Re-run the
cross-lineage flip-review** (Opus build-review Workflow + Sol + DeepSeek, + Gemini if available)
on the hardened enforce code; flip to `--watchdog=enforce` only when **all lineages approve**.
Then stop+disable whale-watchdog (interlock releases → AIRA live) → retire whale-watchdog. Box
protected throughout by whale-watchdog + systemd-oomd + the aira.slice cap.
