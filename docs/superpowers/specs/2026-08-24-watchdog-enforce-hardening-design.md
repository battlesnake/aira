# AIRA watchdog — enforce-path hardening (multi-offender pursuit + audit honesty)

Status: PLAN v3 (builder-ready) — v2 folded Sol GATE-FAIL; v3 folds the re-gate: Sol
GATE-PASS-WITH-NITS + Fable GATE-PASS-WITH-NITS incl. one P1 (a defer/failed re-kill must NOT
unset an earned `latch` or it swallows the `recovered` event — §2 act step-4). Both gates now
pass; the remaining items are the precise folds below (F2 escalation wording, `!memOK` clears
`criticalRun`, existing-test updates). Original v2 folds: cooldown-derivation-as-const,
a third overclaiming outcome string, snapshot-error-during-re-arm, prefix preservation.
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
| `!memOK` | 0 | **false** | (keep) | 0 | emit `unevaluated`; return — a failed mem read breaks the continuous-`<8G` evidence, so the next dip re-debounces; `latched` kept (recovery unknown, don't emit `recovered`) |
| `available >= recover` | 0 | false | false | 0 | if was `latched`: emit `recovered`; return |
| `low <= available < recover` (HOLD) | 0 | **false** | (keep) | 0 | return — **no new kill**; clearing `criticalRun` forces re-debounce on the next dip |
| `available < low` **and** `criticalRun` | (0) | true | — | see act | if `cooldown>0`: `cooldown--`, return; else `act(entry=false)` |
| `available < low` **and** `!criticalRun` | `++`; if `<debounce` return | — | — | — | `act(entry=true)` once `armCount>=debounce` |

`act(entry)`:
1. if `entry`: emit `trip` (a re-kill within a `criticalRun` is NOT a fresh trip — so `trip`
   count == number of debounced episode-entries, one per **successfully maintained** episode;
   see the trip/recovered note below).
2. snapshot procs. **On error: emit `unevaluated`; keep `criticalRun`/`latched` (a live episode
   is not ended by a transient snapshot failure — the `<8G` read already succeeded this poll);
   `armCount=0`; return** (retry next poll).
3. read PSI (observe only — off the enforce kill path, unchanged from #64); `acted :=
   handleArmed(...)`.
4. if `acted`: `latched=true; criticalRun=true; cooldown=watchdogReArmCooldown`.
   else (defer / all-retryable-failure): `criticalRun=false; cooldown=0` — **leave `latched`
   untouched** (Fable P1). The #64 rule is "a defer/failed round does not *set* `latched`": in the
   ENTRY path `latched` is already `false`, so this drops the episode and the next `<8G` poll
   re-debounces; but in a RE-KILL path (already `latched` from a prior kill) it must NOT *unset*
   an earned `latch`, or the `recovered` event for a real multi-kill episode is swallowed. Since
   `latched` gates ONLY the `recovered` emission (no kill-path consequence), keeping it is free.
5. `armCount=0`.

**Trip/recovered are not 1:1 (doc, harmless):** a latched period may contain several debounced
entries (each a `trip`) separated by HOLD/`!memOK` excursions that cleared `criticalRun`, all
closed by a single `recovered` at ≥16 GiB. So `trip` count ≥ `recovered` count within a period is
expected — an audit reader must not read it as a lost event. (Grep confirms no non-test consumer
of any watchdog decision string outside `watchdog.go`.)

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
  alive and MemAvailable remains < 8 GiB"` (Sol P2 — the escalation is gated on
  `pressureStillTripped`, so state the recheck), preserving the degraded prefix, i.e. `:347/:352`
  become `"degraded_to_observe; would_signal: SIGTERM; SIGKILL after grace if still alive and
  MemAvailable remains < 8 GiB"`. `Decision` stays `"would_signal"`.
- **Existing test to update (Fable P2):** `TestSignalErrorsAreHonest` (`watchdog_test.go:483-504`)
  asserts substring `"failure"`, which `"signal_failed"` does NOT contain → update its want to
  `"signal_failed"`, and pin `"already_exited"` for the ESRCH case (the current `"exited"`
  substring is non-discriminating — it matches both old and new). Grep for any other test
  asserting these strings.

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
- **F1 defer preserves an earned latch (Fable P1):** during a latched run (a prior kill),
  a re-act that defers (no qualifying offender) clears `criticalRun` (stops re-killing) but keeps
  `latched`, so when memory later reaches ≥ 16 GiB a `recovered` IS still emitted (RED vs the
  latched=false-on-defer bug that swallows it). Separately, an ENTRY-path defer (never killed)
  does NOT set `latched` and re-debounces next poll — the #64 rule. (`TestDeferDoesNotLatchAndRearms`
  stays green under the amendment.)
- **F1 `!memOK` mid-criticalRun re-debounces:** a mem-read failure during a `criticalRun` clears
  `criticalRun` (keeps `latched`), so the next single `< 8 GiB` poll does NOT re-kill without a
  fresh K=3 (RED vs keeping `criticalRun` across an `!memOK`).
- **F1 restart zeroing:** a fresh `watchdogState{}` has no criticalRun/latched/cooldown — the
  observe→enforce flip restarts the daemon, so no stale episode carries.
- **F2:** ESRCH → `"already_exited"`; other error → `"signal_failed"`; observe outcome →
  `"would_signal: SIGTERM; SIGKILL after grace if still alive"` (each RED vs the old string).
- **Existing test — keep as-is + document (Fable verified):** `TestTriggerLatchesUntilMemoryRecoveryThreshold`
  (`watchdog_test.go:219-244`, wants would_signal==2, recovered==1) **passes unchanged under v2**
  and gains discriminating power (a broken cooldown → 3; v1-style skip-debounce-whenever-latched
  → 4). Full v2 trace to add as a comment: polls 1-3 debounce→act (would_signal#1, criticalRun set,
  cooldown=1); poll 4 (10) absorbed by the **cooldown**; poll 5 (1999) is a **HOLD-band** sample
  (low=1000/recover=2000) — no kill, `criticalRun` cleared; poll 6 (2000 ≥ recover) → `recovered`#1;
  polls 7-9 re-debounce → would_signal#2. Keep the test + expectations; add the trace comment so the
  cooldown/HOLD absorption is explicit.
- `go build ./... && go vet ./... && go test ./internal/daemon/ -race` green.

## 5. After merge (unchanged endgame, owner-gated)

Rebuild `~/.local/bin/aira` + restart the daemon (still `--watchdog=observe`). **Re-run the
cross-lineage flip-review** (Opus build-review Workflow + Sol + DeepSeek, + Gemini if available)
on the hardened enforce code; flip to `--watchdog=enforce` only when **all lineages approve**.
Then stop+disable whale-watchdog (interlock releases → AIRA live) → retire whale-watchdog. Box
protected throughout by whale-watchdog + systemd-oomd + the aira.slice cap.
