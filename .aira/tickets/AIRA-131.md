---
{"schema":1,"id":"AIRA-131","project":"aira","title":"Detached run timeout that fires against an already-empty scope still returns U_RUN_RECONCILE_REQUIRED without draining its wait outcome","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["detach","honesty","runner"],"hold":false,"relations":[]}
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

## Resolution

Full two-loop: plan (Opus) → Fable plan-gate (FAIL v1 on one blocking finding,
PASS v2) → build. The complete plan is committed at
`docs/superpowers/plans/2026-09-06-aira131-detached-timeout-arbitration-plan.md`
and is the authoritative record of the design, the §5 CAS-strength
investigation, and the full test rationale; this section records the build.

**The change, in one sentence.** `internal/runner/detach_linux.go`'s timeout
branch now applies `decideTimeoutIntentNotExecuted` (reused verbatim from
AIRA-126) to the detached path: when the leader is proved dead at the instant
`killWithIntent` finds the scope already empty, it drains `waitCh` (bounded by
the existing `arbitrationWaitBound(r.grace)`) and publishes the disposition as
a new durable ledger event via a new helper, `appendDetachedNotExecutedLocked`,
rather than discarding the leader's real exit evidence.

**The §5 open design question, answered in code, not just in the plan.**
`appendDetachedEvidenceLocked`'s existing `Status.Terminal()`-only guard is
provably too weak here (`killWithIntent` releases the per-run lock before
returning, leaving a genuine unlocked window a concurrent, non-terminal
`kill-completed` write can land in). `appendDetachedNotExecutedLocked`
therefore re-decides under its own lock with `decideNotExecutedDisposition`
(also reused verbatim), so a concurrently-completed kill can never be
downgraded to `NotExecuted`.

**The one control-flow subtlety, verified with a real reproduction, not
assumed.** `leaderReaped` must be set `true` at the drain's successful receive
— *before* attempting the disposition, and regardless of whether that
disposition is later honoured or refused. Skipping this (the plan v1 defect the
Fable plan-gate caught) leaves the `--stdin-connect` input-plane abort defer
(`detach_linux.go:374-392`) believing the leader was never reaped: it then
blocks 2 real (unscaled) seconds on an already-drained, permanently-empty
`waitCh`, times out, and appends an unlocked `input-abort-incomplete` event
that `replay` adopts wholesale — durably erasing the very kill-intent this
launch just published. Confirmed by deliberately reverting just the
`leaderReaped = true` line and re-running the suite: only T7 fails, with
exactly this signature (elapsed balloons past the drain bound into the
defer's fixed 2s stall).

**Tests** (`internal/runner/detached_timeout_arbitration_linux_test.go`, all
new — the branch had zero coverage before this ticket):

| Test | What it pins |
| --- | --- |
| T1 `...ArbitratesToExited` | the committed reproduction, now green unconditionally (env gate removed) |
| T2 `...LiveLeaderAtDeadlineStillTimesOut` | an escaped-but-alive leader is never arbitrated away |
| T3 `...ForeignKillIntentIsNotDismissed` | an intent this launch did not create is never dispositioned |
| T4 `...CompletedIntentIsNotDowngradedByNotExecuted` | the §5 CAS: a concurrent real kill survives byte for byte |
| T5 `...ArbitrationDrainBoundLeavesNoDisposition` | a drain that expires publishes nothing and stays byte-for-byte today's |
| T6 `...TimeoutAgainstLiveLeaderStillKills` | the pre-existing proven-kill arm, now covered for the first time |
| T7 `...StdinConnectRefusedDispositionLeavesLedgerIntact` | the plan-gate's blocking finding, pinned |

Every test drives the real `launchDetachedValidated` through the production
`detachReady`/`detachAck` handshake (no reimplemented harness), reusing
AIRA-126's `livenessScope`/`livenessBackend`/`gatedStdin`/`aira126Scale`. T6
adds a small `signallingScope` (embeds `livenessScope`, forwards
`Terminate`/`Kill` to a real `syscall.Kill`) since it is the one test that
needs the adopted leader genuinely signalled.

**Non-porosity, verified by mutation, not asserted:**
- Reverting the `leaderReaped = true` line at the drain receive → only T7
  fails, with the exact stranded-defer signature above.
- Bypassing `appendDetachedNotExecutedLocked`'s CAS refusal (`if false {`) →
  T4 fails, reporting the concurrent actor's `Completed:true` kill silently
  overwritten to `Completed:false, NotExecuted:true`.
- (T2 was also tried against a call-site mutation forcing `processDead`
  unconditionally; it did not go red. This is not porosity — with a real OS
  process, `waitCh` can only ever deliver once the leader has genuinely
  exited, so "drain succeeds" and "leader dead" are tautologically linked in
  this harness. The `processDead` conjunct itself is pinned at the pure
  function level by the existing, unmodified `TestAIRA126TimeoutIntentNotExecutedRule`,
  which AIRA-131 relies on rather than duplicates, per the plan's own risk
  table.)

**Gate**, foreground, exact exit codes:
`aira confine -- go build ./...` 0; `aira confine -- go vet ./...` 0;
`AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` 0, all 15 packages
ok. `internal/runner` also re-run clean under `-race` (3x) and `-count=8`
(no `-race`) with zero flakes. `AIRA126_SOAK=1` run once per the plan as a
non-regression check on the (unmodified) foreground soak:
`timeout_arbitration_linux_test.go` has zero diff from this branch, and the
soak reported **vacuous** (0/800 iterations reproduced the race window) rather
than a wrong-value assertion — an accepted, environment-dependent outcome on
this heavily-loaded shared box tonight, not a regression.

**Deferrals** — see plan §8 for the full, individually-justified list
(a refused disposition after a successful drain still discards the drained
outcome; an already-terminal record at disposition time still returns
reconcile-required; the proven-kill arm's own `<-waitCh` stays unbounded; the
pre-existing `Reconcile`-can-steal window is untouched; no face renders
`KillIntent`; `--cpu-timeout` stays refused on `--detach`; no real-cgroup
detached straddle soak — the hermetic, deterministic reproduction covers the
same state without the observability problem a separate-process straddle
soak would have).
