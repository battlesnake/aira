---
{"schema":1,"id":"AIRA-126","project":"aira","title":"Run timeout that fires against an already-empty scope publishes a kill intent it can never complete, leaving a non-terminal U_RUN_RECONCILE_REQUIRED record","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["flake","honesty","runner"],"hold":false,"relations":[]}
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
