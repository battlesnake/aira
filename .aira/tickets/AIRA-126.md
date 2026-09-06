---
{"schema":1,"id":"AIRA-126","project":"aira","title":"Run timeout that fires against an already-empty scope publishes a kill intent it can never complete, leaving a non-terminal U_RUN_RECONCILE_REQUIRED record","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["flake","honesty","runner"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-131","to":"AIRA-126"}]}
---
Found while fixing AIRA-112 (see that ticket for the separate, already-fixed ESRCH defect). This is a DIFFERENT, independent defect in the same test.

## Reproduction

An isolated probe of the third scenario of TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration (a deliberately deadline-straddling run: /bin/sleep 0.04 with a 50ms timeout), looped in internal/runner under AIRA_REAL_CGROUP=1, failed at iteration 43 of 800 (~2% in isolation; ~0.25% per full-suite run, observed once in a -count=400 run of the real test):

    U_RUN_RECONCILE_REQUIRED: kill intent won before terminal evidence
    Status:running TerminalComplete:false ScopeIntegrity:handoff-unverified
    ScopeKill:{Requested:true Started:false Completed:false}
    KillIntent:{Present:true Sequence:4 Completed:false Empty:false}
    ErrorCodes:[E_RUN_TIMEOUT U_RUN_RECONCILE_REQUIRED]

## Mechanism (traced against source, not inferred)

When the timeout timer and the child's own exit are simultaneously ready, Launch's select may take the timer branch even though the process has already exited and the scope is already empty:

1. internal/runner/runner_linux.go killWithIntent publishes KillIntent{Present:true} durably BEFORE touching the scope (by design).
2. killScope (runner_linux.go) then finds len(pids)==0 and returns killResult{Empty:true} with Started:false, Completed:false and a nil error - deliberately refusing to treat an empty scope as proof that this kill operation won (its own comment says so).
3. Launch sees killErr==nil and IntentPublished==true, so timedOut=true; but timeoutKill.Kill.Completed is false, so it records U_RUN_RECONCILE_REQUIRED and ScopeHandoffUnverified and never marks the intent completed.
4. The terminal CAS then hits 'latest.KillIntent.Present && !latest.KillIntent.Completed', appends capture-finalized, and returns the error with a NON-TERMINAL record.

Net effect: a run that finished cleanly and on time is left non-terminal, marked timed-out, and requires reconcile - because a kill intent was published for a process that had already exited a moment earlier. Launch also still holds the pending wait outcome in waitCh at that point and discards it.

## Why this needs a design decision, not a patch

The two candidate readings are genuinely different and both defensible:
- Honour the pending wait evidence: the timer lost the race, so arbitrate to StatusExited with the real exit code, and record the published intent as a non-executed intent.
- Treat a provably-empty scope as a vacuously-completed kill (killResult.Empty is already computed and currently unused on this path) and arbitrate to StatusKilled.

The first must not silently discard a durable kill intent; the second must not become the 'empty scope alone proves the kill won' inference killScope explicitly refuses. This is kill/terminal-CAS arbitration, so it is two-loop work per CLAUDE.md, not a quick fix.

## Handling in AIRA-112

Not worked around in production code. AIRA-112's test change accepts this as an explicitly-evidenced third outcome of the deadline-straddling scenario (LaunchError code U_RUN_RECONCILE_REQUIRED plus a record carrying an unproven kill intent and E_RUN_TIMEOUT, and zero terminal records), with a pointer to this ticket, so the merge gate stops going red for it while the arbitration question is decided here. Any other error, or a missing piece of that evidence, still fails the test. When this ticket lands, that accommodation should be tightened back.

## Resolution (in-review)

Reading (1) is adopted. Plan: `docs/superpowers/plans/2026-09-06-aira126-kill-terminal-arbitration-plan.md` (§12 records how each plan-gate condition was satisfied, including the two conditions that changed the design).

When a run-timeout kill intent this launch created was provably never delivered — `killErr == nil`, `IntentPublished && IntentCreated`, `killScope` returned `{Empty:true, Started:false}` (so it returned before `Terminate` and before `Kill`, and the scope was verified empty by two reads), and `processLive` proves the leader was already dead at that instant — Launch drains the pending wait, commits a normal terminal `exited` record with the child's real exit code, and dispositions the intent as `KillIntent.NotExecuted`. Every other case keeps today's behaviour byte for byte.

Changes: `KillIntent.NotExecuted` and `killAttempt.IntentCreated` (types.go); `decideTimeoutIntentNotExecuted` and `decideNotExecutedDisposition` (decisions.go); the timer-branch arbitration, its own bounded receive, and the terminal-CAS gate (runner_linux.go); two defensive guards on the detached read sites (detach_linux.go). Runner-lite spec §6.3 gains clause (5) and a `kill_intent` field note, because the existing clause (3) read literally forbade this reading too.

### Deviations recorded honestly

- **Exit-versus-deadline ordering is unestablished.** AIRA holds no exit timestamp, so whether the child's exit fell microseconds before or after the deadline instant cannot be decided and is asserted in neither direction. What IS established is that the child exited by itself with status N (`wait4`) and that no signal was delivered (control flow). The default is therefore the established fact, not the plausible one.
- **`E_RUN_TIMEOUT` is deliberately omitted** from such a record. It asserts that a run was terminated for exceeding its deadline; nothing was terminated here. A reader of an `exited 0` record carrying `KillIntent.NotExecuted` should understand that **the deadline DID fire** and killed nothing — the durable `kill-intent` ledger event plus the disposition are the record of it. `CleanSuccess()` can be true for such a run, which is correct: the command succeeded and the deadline had no effect on it.
- **No face reads `KillIntent`** (verified: nothing outside `internal/runner` references the field), so the surfacing note is one line in the spec's run-record field table rather than a rendering change in `aira run` / `aira get`.
- **Accepted coverage gaps** (plan §8): a crash between the arbitration and the terminal append still reconciles to `lost` + `U_RUN_EXIT_UNKNOWN`; a `processDead` leader whose wait result does not arrive inside the bound falls back to today's outcome; a timeout that coincides with a foreign `aira run kill` keeps today's outcome.
- **The detached path is NOT fixed** — same state, no reproduction, needs its own durable-evidence event. Filed as AIRA-131 (`relates`). Both detached read sites are already guarded so `NotExecuted` can never mean `killed` there.

### AIRA-112's accommodation

Removed in full. `TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration`'s third scenario fails again on any `LaunchError`, and its `exited` branch now positively asserts that a fired deadline left `NotExecuted && !Completed && !ScopeKill.Started` and no `E_RUN_TIMEOUT`. The previously tolerated outcome is now an asserted one.

### Evidence

- Executed revert (plan §10.7): with only `intentNotExecuted` and `honourNotExecuted` forced false, `go test ./internal/runner/ -run TestAIRA126` FAILS, and `TestAIRA126TimeoutAgainstAlreadyExitedLeaderArbitratesToExited` reproduces the ticket's exact signature — `U_RUN_RECONCILE_REQUIRED: kill intent won before terminal evidence`, `Status:running`, `ScopeIntegrity:handoff-unverified`, `ScopeKill:{Requested:true Started:false Completed:false}`, `KillIntent:{Present:true Sequence:4 Completed:false}`, `ErrorCodes:[E_RUN_TIMEOUT U_RUN_RECONCILE_REQUIRED]`. Both detached guard tests fail too. The three over-widening guards (live leader, foreign intent, concurrently-completed intent) pass on both sides, as designed.
- Pre-fix real-cgroup soak (`AIRA126_SOAK=1`, 800 iterations, same revert): FAIL at iteration 181 with that signature. Exit non-zero.
- Post-fix same soak: PASS, 800/800, 241.7s; the deadline fired in 5 of 800 iterations. Exit 0.
- Post-fix `-run TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration -count=400`: PASS, 398.2s. Exit 0.
- The hermetic loop reached the timer branch against an already-empty scope in 6/6 iterations, and refuses to pass if it ever reaches it in none.
