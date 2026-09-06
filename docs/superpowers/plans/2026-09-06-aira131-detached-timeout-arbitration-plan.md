# AIRA-131 — detached run-timeout kill/terminal arbitration

**Status:** plan (plan-review pending). Implementation is NOT in this commit.
**Branch:** `aira131-detached-timeout-arbitration`
**Base:** `origin/master` @ `dc37705`
**Ticket:** AIRA-131, filed from AIRA-126
(`docs/superpowers/plans/2026-09-06-aira126-kill-terminal-arbitration-plan.md` §6).
**Relates:** AIRA-126 (foreground path, merged), AIRA-136 (`--cpu-timeout`,
refused with `--detach`).

---

## 1. Scope in one sentence

Apply AIRA-126's already-landed arbitration rule — a run-timeout kill intent
that provably delivered **no signal to anything** against a leader that was
**proved dead** does not turn the child's real exit into a
`U_RUN_RECONCILE_REQUIRED` — to the **detached** launch path, reusing
`decideTimeoutIntentNotExecuted` and `decideNotExecutedDisposition` verbatim and
publishing the disposition as a **durable ledger event** because the detached
finalisers read the ledger rather than in-process state.

Nothing else about the detached path changes. Every non-arbitrated outcome keeps
today's behaviour byte for byte.

---

## 2. Verified current state (read at HEAD `dc37705`, not taken on faith)

The ticket's line numbers were approximate. Confirmed positions:

| Thing | File:line at `dc37705` |
| --- | --- |
| detached wait/timeout region | `internal/runner/detach_linux.go:492-522` |
| the timer branch itself | `internal/runner/detach_linux.go:503-519` |
| `appendDetachedEvidenceLocked` | `internal/runner/detach_linux.go:650-665` |
| `terminalizeDetachedNoChild` read-site | `internal/runner/detach_linux.go:667-704` |
| `finalizeDetachedTerminalLocked` read-site | `internal/runner/detach_linux.go:715-794` |
| `decideTimeoutIntentNotExecuted` | `internal/runner/decisions.go:57-62` |
| `decideNotExecutedDisposition` | `internal/runner/decisions.go:75-79` |
| foreground use of both | `internal/runner/runner_linux.go:602-644`, `:918-925` |
| `arbitrationWaitBound` / floor | `internal/runner/runner_linux.go:955-969` |
| `KillIntent.NotExecuted` | `internal/runner/types.go:67-86` |
| `killWithIntent` (lock + release) | `internal/runner/runner_linux.go:2285-2362` |
| `killScope` empty-return | `internal/runner/runner_linux.go:2249-2252` |
| `mergeEvidence` wholesale `KillIntent` replace | `internal/runner/evidence.go:88-90` |
| `replay` wholesale record replace | `internal/runner/ledger.go:415` |

The current timer branch, verbatim:

```go
case <-timer.C:
	attempt, killErr := r.killWithIntent(ctx, id, "run-timeout", killPolicy{Enforce: false})
	if killErr != nil || !attempt.Kill.Completed {
		if killErr == nil {
			killErr = errors.New("timeout kill was not proven complete")
		}
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", killErr)
	}
	completed := attempt.Current
	completed.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, GraceMS: r.termGrace.Milliseconds(), Actor: "run-timeout", At: nowString(r.now)}
	completed.KillIntent.Completed, completed.KillIntent.Empty = true, true
	completed.ErrorCodes = appendUnique(completed.ErrorCodes, "E_RUN_TIMEOUT")
	if err := r.appendDetachedEvidenceLocked(id, "kill-completed", completed); err != nil {
		return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	outcome = <-waitCh
```

### 2.1 What AIRA-126 left in place — verified live

- `KillIntent.NotExecuted` exists (`types.go:67-86`) with the doc contract that
  `{Completed:true, NotExecuted:true}` is unreachable.
- `terminalizeDetachedNoChild` (`detach_linux.go:682-688`) refuses to read a
  `NotExecuted` intent as killed:
  `if current.KillIntent.Present && !current.KillIntent.NotExecuted { killed = true; code = "E_RUN_KILLED" }`,
  and `current.KillIntent.Completed = current.KillIntent.Present && !current.KillIntent.NotExecuted`.
- `finalizeDetachedTerminalLocked` (`detach_linux.go:751-758`) does the same in
  its `switch`: `case current.KillIntent.Present && !current.KillIntent.NotExecuted:`
  → `StatusKilled`; `default:` → `StatusExited` with the leader's own exit code.
- Both are pinned by `TestAIRA126DetachedNoChildDoesNotReportKilledForNotExecutedIntent`
  and `TestAIRA126DetachedFinaliserDoesNotReportKilledForNotExecutedIntent`.

So the field cannot acquire "killed" semantics on this path. **What is missing
is any detached code path that ever SETS it** — confirmed by
`grep -rn "NotExecuted" internal/runner/*.go`: the only writer is
`runner_linux.go:924` on the foreground path.

### 2.2 Additional facts established while reading, that shape the fix

1. **`req.CPUTimeout` never reaches the detached path.** `internal/core/core.go:1584`
   refuses `--cpu-timeout` together with `--detach` (AIRA-136 §3.3 deferral,
   pinned by `TestAIRA136CPUTimeoutWithDetachIsRefused`), and
   `startDeadlineSource` is only called from `runner_linux.go:596`. The detached
   deadline is therefore always wall-clock, actor `"run-timeout"`, code
   `E_RUN_TIMEOUT`. **No `deadlineFire` plumbing is needed here.**
2. **The detached timeout branch has ZERO test coverage today.** No test in the
   tree sets `Request.Timeout` together with `Detach`. Both arms — the arbitrated
   one this ticket adds and the pre-existing proven-kill one — are covered by §6.
3. **`replay` adopts a plain event's `Run` wholesale** (`ledger.go:415`), so an
   appended evidence event is the complete surviving state; anything not in it is
   erased. This is why the disposition must be published as
   `mergeEvidence(ledgerCurrent, candidate)` under a lock held across the read,
   as `appendDetachedEvidenceLocked` already does.
4. **`killWithIntent` releases the run lock before returning**
   (`runner_linux.go:2290 defer unlockFile(lock)`). This is the fact §5 turns on.

---

## 3. The bug, stated exactly

The detached child is placed with `CLONE_INTO_CGROUP` while the supervisor stays
outside the scope. When the wall-clock deadline fires at the instant the leader
has already exited and been reaped:

- `killWithIntent` publishes a durable `kill-intent`;
- `killScope` returns at `len(pids) == 0` **before `Terminate` and before
  `Kill`** → `killResult{Empty:true, Started:false, Completed:false}`, `nil` error;
- `attempt.Kill.Completed` is false, so the branch returns
  `U_RUN_RECONCILE_REQUIRED` — **without draining `waitCh`**.

The leader's real exit evidence is therefore not merely unread, it is
**discarded**: the supervisor returns, the run record stays non-terminal with an
undisposed kill intent, and `aira` reports a reconcile-required run for a child
that exited cleanly. No signal was ever delivered to anything.

---

## 4. The committed reproduction

`internal/runner/detached_timeout_arbitration_linux_test.go` →
`TestAIRA131DetachedTimeoutAgainstAlreadyExitedLeaderArbitratesToExited`.

**Device.** Deterministic, not a soak. It reuses AIRA-126's `livenessScope` /
`livenessBackend` (a `cgroup.procs` model backed by real `/proc` state, so a
reaped task really leaves the scope) and `gatedStdin`. `os/exec.Cmd.Wait` reaps
the child with `wait4` **first** and only then joins the stdin copy goroutine, so
at the deadline the child is genuinely reaped (`processLive → processDead`), the
scope is genuinely empty, and the wait OUTCOME is still pending in `waitCh` — the
exact production state, with only the moment the supervisor *learns* of the exit
controlled. `launchDetachedValidated` is driven directly through the
`detachReady`/`detachAck` pipe handshake, as `TestM20...` already does.

Timings: child `/bin/sleep 0.03`, `Timeout` 300ms, stdin hold 900ms, `Grace` 2s
(all through `aira126Scale`, so `-race` and a loaded box widen the windows rather
than collapsing them).

**Exact red output at `dc37705`** (`AIRA131_REPRO=1 aira confine -- go test ./internal/runner/ -run TestAIRA131DetachedTimeoutAgainstAlreadyExitedLeaderArbitratesToExited -v -count=1`, exit code 1):

```
=== RUN   TestAIRA131DetachedTimeoutAgainstAlreadyExitedLeaderArbitratesToExited
    detached_timeout_arbitration_linux_test.go:109: the detached deadline discarded a real exit: err=U_RUN_RECONCILE_REQUIRED: timeout kill was not proven complete record=<nil>
--- FAIL: TestAIRA131DetachedTimeoutAgainstAlreadyExitedLeaderArbitratesToExited (0.34s)
FAIL
FAIL	aira/internal/runner	0.345s
FAIL
```

`-count=5`: **5/5 failures**, each in ~0.32-0.34s — i.e. the branch returns at
the 300ms deadline without waiting for the 900ms hold, which is itself direct
evidence that `waitCh` is never drained. The reproduction is deterministic, not
lucky.

**Red-in-tree choice.** The test is committed **env-gated**
(`AIRA131_REPRO=1`, otherwise `t.Skip`) rather than red. Rationale: this is a
plan-phase commit on a shared repo where other agents rebase and run
`go test ./...`; a red test in the tree is indistinguishable from a real
regression for anyone who lands on this branch, and the two-loop's plan gate
needs to *run* the reproduction, which a `t.Skip` still allows via the env var.
The exact red output is recorded above so the gate has the evidence without
having to trust a re-run. **The implementation phase deletes the gate** (§6.1);
that deletion is a required, reviewable line of the build diff, and the plan
review should reject any implementation that leaves the gate in.

Verified: with the gate in place `aira confine -- go test ./internal/runner/ -run TestAIRA131Detached -v`
reports `--- SKIP` and `ok`, so the branch's suite is green as committed.

---

## 5. The open design question, resolved: the CAS **is** too weak

**Question (from the ticket).** `appendDetachedEvidenceLocked` refuses to append
only when `current.Status.Terminal()` (`detach_linux.go:660-662`). It does not
check that the ledger's `KillIntent.Sequence` is the one *this* launch published,
the way `decideNotExecutedDisposition` does for the foreground path. Is the
weaker guard provably sufficient here — e.g. because the whole call already holds
the same per-run lock the finalisers hold?

**Answer: no. It is not sufficient, and the "already holds the lock" premise is
false.** Evidence, all from source:

1. **The lock is NOT held across the decision.** `killWithIntent` takes the
   per-run lock at `runner_linux.go:2286` and releases it on return
   (`defer unlockFile(lock)`, `:2290`). `appendDetachedEvidenceLocked` takes the
   *same* lock file itself (`detach_linux.go:651`) and releases it on return.
   They are two **disjoint** critical sections. The launch flock taken at
   `detach_linux.go:229` was already released at `detach_linux.go:486`
   (`unlock()`), well before the wait/timeout region. So there is a genuine
   unlocked window between "we decided the intent delivered nothing" and "we
   publish that disposition".
2. **A concurrent, non-terminal writer can legally land in that window.**
   `Kill()` → `finishDetachedKill` (`runner_linux.go:2410-2434`) appends a
   **non-terminal** `kill-completed` event carrying
   `KillIntent.Completed = true, Empty = true` and a completed `ScopeKill`, then
   *waits* for the supervisor to finalise. `current.Status.Terminal()` is false
   for that record, so `appendDetachedEvidenceLocked`'s only guard does not fire.
3. **The append would then ERASE it.** `mergeEvidence` replaces the base
   `KillIntent` **wholesale** whenever the candidate's is `Present`
   (`evidence.go:88-90`), and `replay` adopts a plain event's `Run` wholesale
   (`ledger.go:415`). A candidate built from `attempt.Current` with
   `NotExecuted = true` carries `Completed:false`, so publishing it over a
   concurrently-completed intent would durably downgrade a real, executed kill to
   "not executed" — the precise failure `decideNotExecutedDisposition` was
   written to prevent, and the one the foreground path's
   `TestAIRA126CompletedIntentIsNotDowngradedByNotExecuted` pins.

**What is proved and what is not — stated honestly.** Steps 1-3 are structural
and certain. What is *not* proved is that step 2's interleaving is reachable in
the pure AIRA-131 state: our arbitration requires `attempt.Kill.Empty` (two
independent reads found the scope empty), and a concurrent killer running *after*
our intent append would then also find it empty and report `Completed:false`, so
`finishDetachedKill` would append nothing. Reaching a concurrent `Completed:true`
requires the scope to be **repopulated** after our two empty reads, which needs an
external writer to `cgroup.procs`.

That residual is exactly why the guard must be added rather than argued away.
AIRA does not elsewhere assume "no task ever moves into or out of a run scope" —
`ScopeMigrated` / `ScopeDescendantEscaped` and the AIRA-#20 descendant-escape
attestation exist because tasks demonstrably do move between cgroups. Resting a
kill/terminal CAS on the negation of an invariant the codebase spends real
machinery refusing to assume is not a proof; it is the same
"an empty scope means the kill won" inference `killScope` itself refuses. The
disposition therefore re-decides under the same lock as the append, and
`decideNotExecutedDisposition` is reused verbatim so both paths share one rule.

**Sub-question — is the read/append pair itself atomic?** Yes. Within
`appendDetachedEvidenceLocked`, `r.ledger.current(id)` and `r.append(...)` are
both inside the per-run flock, and every writer of a run record in the codebase
(`killWithIntent`, `finishDetachedKill`, `failBeforeLaunch`, `Reconcile` /
`reconcileDetachedLocked`, `appendDetachedLeaderExit`, `forceDetachedQuiesce`,
`finalizeDetachedTerminal`) takes that same `<id>.lock` first. So the *only*
hole is the earlier release-then-reacquire, which is what §6.2 closes.

**Consequence for the fix:** do **not** weaken or re-purpose
`appendDetachedEvidenceLocked` (its other two call sites — `kill-completed` at
`:515` and `running-failure` at `:536` — are correct as they are, and the
`kill-completed` candidate sets `Completed:true` itself so it can never
downgrade). Add a **separate** helper that re-decides under the lock.

---

## 6. The change, function by function

### 6.1 `internal/runner/detached_timeout_arbitration_linux_test.go`

Delete the `AIRA131_REPRO` env gate (the four lines of `os.Getenv` + `t.Skip`,
and the now-unused `os` import if it becomes unused) so the reproduction runs in
the standard suite. This is a required line of the build diff.

### 6.2 `internal/runner/detach_linux.go` — new helper `appendDetachedNotExecutedLocked`

Placed immediately after `appendDetachedEvidenceLocked` (`:665`):

```go
// appendDetachedNotExecutedLocked publishes the AIRA-131 disposition of a
// run-timeout kill intent that provably delivered no signal (AIRA-126's rule on
// the detached path). The disposition is re-decided against the PRE-merge ledger
// state under the SAME per-run lock as the append, because killWithIntent
// released that lock before returning: mergeEvidence replaces KillIntent
// wholesale whenever the candidate's is Present, so publishing without this
// re-read could durably downgrade a concurrent actor's completed kill.
//
// It reports whether the disposition was honoured. Every refusal — a ledger read
// error, a record another actor already terminalized, an absent or already
// completed intent, or an intent whose sequence is not the one THIS launch
// published — leaves the caller's unchanged U_RUN_RECONCILE_REQUIRED arm in
// charge. A refusal is never an error in itself.
func (r *Runner) appendDetachedNotExecutedLocked(id string, intentNotExecuted bool, publishedSequence uint64, candidate RunRecord) (bool, error) {
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
	if err != nil {
		return false, err
	}
	defer unlockFile(lock)
	current, readErr := r.ledger.current(id)
	if readErr == nil && current.Status.Terminal() {
		return false, nil
	}
	if !decideNotExecutedDisposition(intentNotExecuted, readErr, current.KillIntent, publishedSequence) {
		return false, readErr
	}
	merged := mergeEvidence(current, candidate)
	// The ledger's own intent (sequence, requested scope kill) is preserved;
	// NotExecuted is the ONLY field this arbitration adds. Completed is never set.
	merged.KillIntent.NotExecuted = true
	if _, err := r.append(ledgerEvent{Kind: "kill-not-executed", Run: merged}); err != nil {
		return false, err
	}
	return true, nil
}
```

Notes that are load-bearing, not stylistic:

- `readErr` is passed **into** `decideNotExecutedDisposition` rather than being
  short-circuited, so the shared rule's `ledgerErr == nil` conjunct is genuinely
  exercised on this path too (a zero `RunRecord` on a read failure also fails
  `Present`, so the refusal is doubly grounded).
- `lockFile` (blocking), not `boundedRunLock`, matching every other
  `appendDetached*` helper. Changing the lock discipline is out of scope.
- New event kind `"kill-not-executed"`. `replay` has no per-kind handling for it
  and adopts the record wholesale (`ledger.go:415`); `ledger.append` special-cases
  only `"terminal"` and `"telemetry"`. No ledger change is required, and the
  record stays non-terminal (`Status` is inherited from `current`, i.e. `running`).

### 6.3 `internal/runner/detach_linux.go:503-519` — the timer branch

```go
case <-timer.C:
	attempt, killErr := r.killWithIntent(ctx, id, "run-timeout", killPolicy{Enforce: false})
	if killErr != nil || !attempt.Kill.Completed {
		// AIRA-131. The deadline can fire against a scope the leader has already
		// left: killWithIntent has published a durable intent that killScope
		// refused to execute (it returned at len(pids)==0 before Terminate and
		// before Kill), so no signal reached anything. With the leader also proved
		// dead at that instant, the established facts are the child's own exit and
		// the absence of any delivered kill — honour the pending wait rather than
		// discard it. Every other shape keeps the unchanged unevaluated arm.
		arbitrated := false
		if decideTimeoutIntentNotExecuted(killErr, attempt, processLive(record.PIDIdentity)) {
			// Drain BEFORE publishing: a drain that expires must leave no durable
			// disposition behind, so the timed-out case is byte-for-byte today's.
			// A dead leader's cmd.Wait() is already blocked in wait4 on our own
			// child, so only the reap and one scheduler wakeup remain; the bound is
			// a pure anti-hang guard whose expiry can only produce today's outcome.
			select {
			case outcome = <-waitCh:
				honoured, dispositionErr := r.appendDetachedNotExecutedLocked(id, true, attempt.IntentSequence, attempt.Current)
				if dispositionErr != nil {
					return nil, launchErr("U_RUN_RECONCILE_REQUIRED", dispositionErr)
				}
				arbitrated = honoured
			case <-time.After(arbitrationWaitBound(r.grace)):
			}
		}
		if !arbitrated {
			if killErr == nil {
				killErr = errors.New("timeout kill was not proven complete")
			}
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", killErr)
		}
		break // fall through to the shared post-wait continuation
	}
	completed := attempt.Current
	... unchanged ...
	outcome = <-waitCh
```

(The literal `break` is illustrative — in Go the arm simply ends; the
implementation will structure this so the arbitrated arm falls out of the
`select` with `outcome` already set, without duplicating the continuation.)

**Ordering is deliberate and is the one place this differs in shape from the
foreground path.** Foreground arbitrates → drains → merges → decides the
disposition at its single terminal CAS. Detached has no single terminal CAS in
the launch body, so the disposition is its own durable event; publishing it
*before* a drain that then expires would leave a durable `NotExecuted` on a run
whose exit was never observed, while the launch still returned
`U_RUN_RECONCILE_REQUIRED`. Draining first makes the expiry path leave **no new
durable state at all**, which is exactly the ticket's "must NOT change today's
behaviour".

**Cost of a refused disposition after a successful drain:** the drained outcome
is discarded and the launch returns `U_RUN_RECONCILE_REQUIRED` — identical to
today, since today never drains at all. Accepted, and listed in §8.

**What happens after `arbitrated`:** control reaches the existing shared
continuation unchanged — `leaderReaped = true`, `inputPlane.closeTerminal()`,
`waitEvidence`, `appendDetachedLeaderExit`, `waitEmpty`, and
`finalizeDetachedTerminal`. `finalizeDetachedTerminalLocked`'s
`case current.KillIntent.Present && !current.KillIntent.NotExecuted:` is false, so
the `default:` arm runs: `StatusExited` with the leader's own exit code, plus
`E_RUN_FAILED` if that code is non-zero. `E_RUN_TIMEOUT` is **not** appended and
`ScopeKill.Started/Completed` are **not** set — matching the foreground, where a
honoured arbitration sets `timedOut = false` and never reaches
`runner_linux.go:843-845`. The durable evidence that the deadline fired and killed
nothing is `KillIntent{Present, Sequence, NotExecuted}` and nothing else, exactly
as AIRA-126 §12 C9 recorded for the foreground.

### 6.4 What is explicitly NOT changed

- `decideTimeoutIntentNotExecuted` and `decideNotExecutedDisposition` —
  **reused verbatim, not copied, not parameterised**. If either needs an edit,
  that is a signal the detached path is being given a different rule, and the
  build review should treat it as a defect.
- `appendDetachedEvidenceLocked` and its two existing call sites.
- `arbitrationWaitBound` / `arbitrationWaitFloor` (`runner_linux.go:955-969`) —
  reused as-is; the 250ms floor exists precisely so a millisecond `Grace` in a
  test cannot silently collapse the bound.
- `killScope`, `killWithIntent`, `mergeEvidence`, `replay`, the ledger schema,
  the `RunRecord` shape, both detached finalisers, every face (CLI/MCP/TUI/Skill).
- The proven-kill arm of the timer branch, including its unbounded
  `outcome = <-waitCh` at `:518` (see §8).

---

## 7. Tests

Every test lives in `internal/runner/detached_timeout_arbitration_linux_test.go`
unless stated. All reuse `livenessScope`, `livenessBackend`, `gatedStdin` and
`aira126Scale` from `timeout_arbitration_linux_test.go` (same package) — no
duplicated harness.

| # | Test | Asserts | Fails without |
| --- | --- | --- | --- |
| T1 | `TestAIRA131DetachedTimeoutAgainstAlreadyExitedLeaderArbitratesToExited` (the §4 repro, gate removed) | `StatusExited`, exit 0, `KillIntent.NotExecuted`, `!Completed`, `!ScopeKill.Started`, no `E_RUN_TIMEOUT`, no `U_RUN_RECONCILE_REQUIRED`, no signal reached the scope, exactly 1 terminal record, and the ledger agrees with the returned record | the whole fix |
| T2 | `TestAIRA131DetachedLiveLeaderAtDeadlineStillTimesOut` | `livenessScope{emptyAfterFirstMembers:true}` → same `{Empty:true, Started:false}` kill shape but a **live** leader: must still return `U_RUN_RECONCILE_REQUIRED`, no `NotExecuted` anywhere in the ledger, 0 terminal records | the `processDead` conjunct — stops the fix widening into "an empty scope means the run finished" |
| T3 | `TestAIRA131DetachedForeignKillIntentIsNotDismissed` | a kill intent seeded from outside after `running` and before the deadline → `IntentCreated == false` → `U_RUN_RECONCILE_REQUIRED`, no `NotExecuted`, 0 terminal records | the `IntentCreated` conjunct |
| T4 | `TestAIRA131DetachedCompletedIntentIsNotDowngradedByNotExecuted` | a concurrent actor appends a **non-terminal** `kill-completed` (`KillIntent.Completed:true` + completed `ScopeKill`) in the window between the `kill-intent` append and the disposition append → the launch returns `U_RUN_RECONCILE_REQUIRED` and the concurrent intent survives **byte for byte** in the ledger | §5's added sequence/`Completed` CAS. **This is the test that pins §5's answer** |
| T5 | `TestAIRA131DetachedArbitrationDrainBoundLeavesNoDisposition` | `Grace` at the floor and a stdin hold far beyond it → the bounded drain expires → `U_RUN_RECONCILE_REQUIRED`, **no** `kill-not-executed` event in the ledger, no `NotExecuted`, 0 terminal records | drain-before-publish ordering (§6.3) |
| T6 | `TestAIRA131DetachedTimeoutAgainstLiveLeaderStillKills` | a scope whose `Terminate`/`Kill` really signal the adopted leader, deadline fires against a populated scope → the **pre-existing** arm: `StatusKilled`, `E_RUN_TIMEOUT`, `KillIntent.Completed`, `ScopeKill.Completed` | any regression of the branch this ticket touches. Closes §2.2(2)'s coverage gap |

T4's window is ≥ the bounded drain (≈600ms in the harness), so a ledger poller
lands inside it reliably; no new production test hook is introduced. If review
judges the poll racy, the fallback is an existing-style `r.beforeRunningAppendFn`
sibling hook — but only if needed, and it must not be reachable in production.

Also run, unchanged and expected green: `TestAIRA126*` (all), the whole
`internal/runner` package, and `go build ./... && go vet ./...`. The AIRA-126
real-cgroup soak (`AIRA126_SOAK=1`) is run once as a non-regression check.

**Command form (CLAUDE.md):** every build/test is prefixed
`aira confine -- `, exit codes recorded exactly, `pass` / `fail` /
`unevaluated` distinguished, and no green claimed from truncated output.

---

## 8. Deferrals and accepted gaps — written down, not silent

1. **A refused disposition after a successful drain discards the drained
   outcome** and returns `U_RUN_RECONCILE_REQUIRED` (§6.3). This is exactly
   today's behaviour and is the conservative direction; making the supervisor
   also *report* another actor's completed kill would mean terminalising from
   this branch, which is a larger change than this ticket.
2. **An already-terminal ledger record at disposition time returns
   `U_RUN_RECONCILE_REQUIRED`** rather than the concurrently-committed terminal
   record. Today's branch does the same (it returns before reading the ledger at
   all), so this is unchanged behaviour, not a new gap.
3. **The proven-kill arm's `outcome = <-waitCh` at `:518` stays unbounded.** A
   kill proved complete means the scope is empty and the leader is dead, so
   `wait4` returns; bounding it is unrelated to this ticket's failure mode and
   would change the behaviour of a path this ticket does not otherwise touch.
4. **A concurrent `Reconcile` can still steal a detached run.** Verified:
   `reconcileDetachedLocked` (`runner_linux.go:2736-2796`) goes through
   `decideDetachedReader` with `processLive(record.SupervisorPID)`, so a live
   supervisor is preserved; a dead one is finalised or marked lost. This ticket
   neither creates nor closes that window — it is the pre-existing detached
   contract and every `appendDetached*` helper already no-ops against a terminal
   record.
5. **No face renders `KillIntent`.** As in AIRA-126 §12 C9, an `exited 0` record
   carrying `KillIntent.NotExecuted` means "the deadline fired and killed
   nothing"; surfacing it is a spec field-table note, not a rendering change.
   Deferred identically here.
6. **`--cpu-timeout` on the detached path** remains refused at
   `internal/core/core.go:1584` (AIRA-136 §3.3). When that deferral is lifted,
   the detached branch will need the `deadlineFire` actor/code plumbing the
   foreground already has; this plan deliberately does not pre-build it.
7. **Real-cgroup detached soak.** The reproduction is hermetic and
   deterministic. A real-cgroup detached straddle soak (the analogue of
   `TestAIRA126RealCgroupDeadlineStraddleSoak`) is **not** added: the detached
   supervisor is a separate process, so the straddle cannot be observed in-process
   the way the foreground soak observes it, and an env-gated probe that can only
   fail vacuously is the porous shape the review policy rejects. Recorded as an
   accepted coverage gap; T1's device covers the same state deterministically.

---

## 9. Risks

| Risk | Mitigation |
| --- | --- |
| The fix widens into "an empty scope means the run finished" | T2 + the reused `decideTimeoutIntentNotExecuted`, whose per-conjunct table is already pinned by `TestAIRA126TimeoutIntentNotExecutedRule` |
| A real completed kill is downgraded to `NotExecuted` | §5's CAS + T4 |
| A durable disposition is left behind on a path that then reports unevaluated | drain-before-publish (§6.3) + T5 |
| The rule silently forks between foreground and detached | both decision functions are reused verbatim; §6.4 makes any edit to them a review defect |
| The reproduction passes vacuously | T1 fails explicitly with "vacuous: the detached timer branch never fired" if `KillIntent.Present` is false; the red run above shows the branch really is taken |
| A red test blocks other agents | §4's env gate, deleted in the implementation phase |

---

## 10. Expected yield

One correctness class closed: a detached run whose child exited cleanly can no
longer be reported as reconcile-required because a deadline fired against an
already-empty scope, and the branch that decides it gains its first tests
(T1-T6) where it had none. One durable-evidence CAS brought to parity with the
foreground path, with the parity question answered from the actual lock scope
rather than assumed.

---

## 11. Two-loop record

- **Plan review:** Codex (Sol) + DeepSeek-pro, orthogonal lineages; Fable is the
  plan gate. Correctness-critical (kill/terminal CAS) → full two-loop, not the
  light path.
- **Build:** TDD from T1's recorded red; Opus builds (per the owner's standing
  direction for correctness-critical, architecturally subtle AIRA work).
- **Build review:** adversarial, both false-fail and false-pass directions, with
  every confirmed counterexample becoming a regression test.
- **Build gate:** Fable, then PR and merge.
