---
{"schema":1,"id":"AIRA-131","project":"aira","title":"Detached run timeout that fires against an already-empty scope still returns U_RUN_RECONCILE_REQUIRED without draining its wait outcome","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["detach","honesty","runner"],"hold":false,"relations":[]}
---
Filed from AIRA-126 (`docs/superpowers/plans/2026-09-06-aira126-kill-terminal-arbitration-plan.md` §6). AIRA-126 fixed the foreground `Launch` path only; this is the same state on the detached path, deliberately left out of that PR's scope.

## The state

`internal/runner/detach_linux.go` timeout branch:

```go
case <-timer.C:
        attempt, killErr := r.killWithIntent(ctx, id, "run-timeout", killPolicy{Enforce: false})
        if killErr != nil || !attempt.Kill.Completed {
                if killErr == nil {
                        killErr = errors.New("timeout kill was not proven complete")
                }
                return nil, launchErr("U_RUN_RECONCILE_REQUIRED", killErr)
        }
```

The detached child is placed with `CLONE_INTO_CGROUP` while the supervisor stays outside the scope, so the run scope can be empty when the detached deadline fires — exactly the AIRA-126 shape: `killWithIntent` publishes a durable intent, `killScope` returns at `len(pids)==0` before `Terminate` and before `Kill` (`Started:false`, `Empty:true`, nil error), and `attempt.Kill.Completed` is false.

This branch is stricter than the foreground one was: it returns `U_RUN_RECONCILE_REQUIRED` **without even draining `waitCh`**, so the child's real exit evidence is discarded rather than merely unread.

## Why it was not fixed in AIRA-126

- No reproduction exists here, and AIRA-126's load-bearing evidence was a reproduction of the foreground race. Mixing an unreproduced second path in would have made that evidence ambiguous.
- The detached finaliser reads the ledger rather than in-process state, so the disposition needs its own durable-evidence event rather than the foreground path's in-process `intentNotExecuted`.

## What AIRA-126 did leave in place

`KillIntent.NotExecuted` exists, and both detached read sites are already guarded against reading it as `killed` (`terminalizeDetachedNoChild` and `finalizeDetachedTerminalLocked`), with tests pinning both. So the field cannot acquire "killed" semantics on this path while this ticket is open; what is missing is the detached timeout branch ever *setting* it.

## Shape of the fix

Apply the same arbitration rule (`decideTimeoutIntentNotExecuted`) with the leader's liveness proved at the instant the kill was refused, drain `waitCh`, and publish the disposition as a durable event the finaliser reads. Needs its own plan and its own reproduction; it is kill/terminal-CAS arbitration, so it is two-loop work per CLAUDE.md.
