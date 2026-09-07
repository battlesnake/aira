# AIRA-131 — detached run-timeout kill/terminal arbitration

**Status:** plan v2 (plan-gate fix applied). Implementation is NOT in this commit.
**Branch:** `aira131-detached-timeout-arbitration`
**Base:** `origin/master` @ `dc37705`
**Ticket:** AIRA-131, filed from AIRA-126
(`docs/superpowers/plans/2026-09-06-aira126-kill-terminal-arbitration-plan.md` §6).
**Relates:** AIRA-126 (foreground path, merged), AIRA-136 (`--cpu-timeout`,
refused with `--detach`).

**Plan-gate revision (v1 → v2).** The Fable plan gate FAILed v1 on one blocking
finding: v1's §6.3 broke its own "byte-for-byte today's behaviour" promise on the
**drained-then-refused** path when `StdinConnect` is set, because it returned with
`leaderReaped` still false after having emptied `waitCh`, which drove the
input-plane defer (`detach_linux.go:374-392`) into a 2s stall followed by an
unlocked `input-abort-incomplete` append of a stale, intent-less `record` that
`replay` adopts wholesale — durably erasing the very kill-intent this branch had
just published. The finding was re-verified against the real source at
`019ab4b` before being folded in (§2.3 records the verification). v2 sets
`leaderReaped = true` the instant the bounded drain succeeds, adds §8(7) for the
one residual delta that produces, and adds test T7. §5 also gains the ledger-level
backstop the gate noted, §6.3 gains the required control-flow structure, and §7
gains the explicit statement of what T4's concurrent writer must be. Nothing else
in v1 changed: the gate verified the rest of the plan against source and reported
it sound.

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
| **input-plane abort defer** (`StdinConnect` only) | `internal/runner/detach_linux.go:374-392` |
| `leaderReaped` declaration / the shared set at `:523` | `internal/runner/detach_linux.go:355`, `:523` |
| `waitCh` creation + its single-send goroutine | `internal/runner/detach_linux.go:435-439` |
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

### 2.3 The input-plane abort defer — the fact v1 missed, verified line by line

This is the constraint the plan gate's blocking finding rests on, and it is
load-bearing for §6.3. Read verbatim at `detach_linux.go:374-392`:

```go
defer func() {
	inputPlane.closeTerminal()
	if !childStarted || leaderReaped {
		return
	}
	killDetachedInputChild(scope, cmd)
	if waitCh == nil {
		return
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-waitCh:
		leaderReaped = true
	case <-timer.C:
		record.ErrorCodes = appendUnique(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED")
		_, _ = r.append(ledgerEvent{Kind: "input-abort-incomplete", Run: record})
	}
}()
```

Four facts, each checked against source rather than assumed:

1. **The defer exists only under `StdinConnect`** — it is installed inside the
   `if req.StdinConnect` arm (`:357-392`). On the non-connect path (`:402-419`,
   which is what the §4 reproduction and T1-T6 as written use) there is no such
   defer and `leaderReaped` is read only at `:376`, so it is inert.
2. **It fires on EVERY return from `launchDetachedValidated` after `:357`**,
   including the timer branch's `U_RUN_RECONCILE_REQUIRED` returns. It is a
   `defer`, not a branch of the happy path.
3. **`waitCh` is buffered `1` and its goroutine sends EXACTLY ONCE**
   (`:435-439`: `waitCh = make(chan waitOutcome, 1)`, then one `waitCh <- …`
   after `cmd.Wait()`). So a value received from `waitCh` is gone forever;
   **nothing ever refills it.**
4. **Today the timer branch never drains `waitCh` before returning**
   (`:503-509`), so whenever this defer runs after a timeout the buffered outcome
   is still there: the defer's `case <-waitCh` wins immediately, sets
   `leaderReaped = true`, and appends **nothing**. That is the "byte for byte"
   behaviour §1 promises to preserve.

**The consequence for any design that drains.** Once the arbitrated arm receives
from `waitCh`, fact 3 makes that channel permanently empty. If the arm then
returns without setting `leaderReaped`, fact 2 puts the defer into the `!leaderReaped`
path with an empty channel: it calls `killDetachedInputChild`, **blocks the full
2 seconds**, then executes the `<-timer.C` arm — an UNLOCKED `r.append` of the
launch-local `record`, which is the pre-timeout snapshot and carries **no
`KillIntent`** at all. `replay` adopts a plain event's `Run` wholesale
(`ledger.go:415`), so that append durably **erases** the `kill-intent`
`killWithIntent` published, and any concurrent actor's `kill-completed` with it.
That is both a new 2s stall and a durable-evidence regression, on exactly the
path the ticket says must not change. §6.3 closes it.

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

**A second, ledger-level backstop behind the helper's own terminal check.**
`ledger.append` itself refuses any non-telemetry append after a terminal record
(`ledger.go:208-213`: `case event.Kind != "telemetry": return event,
fmt.Errorf("E_JOURNAL_CORRUPT: record after terminal run %s", …)`). So even if
`appendDetachedNotExecutedLocked`'s `current.Status.Terminal()` guard were ever
bypassed, a disposition could not be written over a committed terminal record —
it would surface as an `E_JOURNAL_CORRUPT` error, which the caller turns into
`U_RUN_RECONCILE_REQUIRED` (the honest arm), never into a silent corruption. This
is defence in depth, not a substitute for the guard: the guard is what makes the
already-terminal case a clean refusal rather than an error, and the sequence /
`Completed` CAS below is what the ledger level does **not** provide.

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
	arbitrated := false
	if killErr != nil || !attempt.Kill.Completed {
		// AIRA-131. The deadline can fire against a scope the leader has already
		// left: killWithIntent has published a durable intent that killScope
		// refused to execute (it returned at len(pids)==0 before Terminate and
		// before Kill), so no signal reached anything. With the leader also proved
		// dead at that instant, the established facts are the child's own exit and
		// the absence of any delivered kill — honour the pending wait rather than
		// discard it. Every other shape keeps the unchanged unevaluated arm.
		if decideTimeoutIntentNotExecuted(killErr, attempt, processLive(record.PIDIdentity)) {
			// Drain BEFORE publishing: a drain that expires must leave no durable
			// disposition behind, so the timed-out case is byte-for-byte today's.
			// A dead leader's cmd.Wait() is already blocked in wait4 on our own
			// child, so only the reap and one scheduler wakeup remain; the bound is
			// a pure anti-hang guard whose expiry can only produce today's outcome.
			drain := time.NewTimer(arbitrationWaitBound(r.grace))
			select {
			case outcome = <-waitCh:
				if !drain.Stop() {
					select {
					case <-drain.C:
					default:
					}
				}
				// The drain SUCCEEDED, so wait4 has reaped the leader and this
				// supervisor now holds its exit evidence. That is a KERNEL FACT,
				// independent of whether the disposition below is honoured — and
				// waitCh is buffered 1 with a single-send goroutine (:435-439), so
				// nothing will ever refill it. Record the reap HERE, before any
				// path that can return: otherwise a refused disposition returns
				// with leaderReaped false and drives the input-plane defer
				// (:374-392) into a 2s block on an empty channel followed by an
				// unlocked input-abort-incomplete append of the pre-timeout
				// `record`, which replay adopts wholesale (ledger.go:415),
				// durably ERASING the kill intent just published. See §2.3.
				leaderReaped = true
				honoured, dispositionErr := r.appendDetachedNotExecutedLocked(id, true, attempt.IntentSequence, attempt.Current)
				if dispositionErr != nil {
					return nil, launchErr("U_RUN_RECONCILE_REQUIRED", dispositionErr)
				}
				arbitrated = honoured
			case <-drain.C:
				// Expired: waitCh is UNTOUCHED and no durable disposition exists,
				// so leaderReaped stays false and this path — including the
				// input-plane defer's kill-then-receive — is byte-for-byte today's.
			}
		}
		if !arbitrated {
			if killErr == nil {
				killErr = errors.New("timeout kill was not proven complete")
			}
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", killErr)
		}
	}
	if !arbitrated {
		completed := attempt.Current
		... the four unchanged assignment lines ...
		if err := r.appendDetachedEvidenceLocked(id, "kill-completed", completed); err != nil {
			return nil, launchErr("U_RUN_RECONCILE_REQUIRED", err)
		}
		outcome = <-waitCh
	}
```

**Required control flow, not a stylistic preference.** v1's illustrative `break`
sat inside a *nested* `select` and would therefore have broken out of the inner
select, falling into the proven-kill continuation with `attempt.Kill.Completed`
false — publishing a fabricated `kill-completed` and then blocking forever on an
already-drained `waitCh`. The implementation MUST structure the arm so that the
arbitrated case:

1. exits the **outer** `select` with `outcome` already set, and
2. does **not** execute the `completed := attempt.Current` proven-kill
   continuation at `:511-518`.

The `arbitrated` flag hoisted above the `if` (shown here) achieves both while
leaving the proven-kill statements textually unchanged — only re-indented. Any
other structure is acceptable if it satisfies (1) and (2); T1 and T6 together
pin the outcome either way.

**`leaderReaped = true` on drain success — the invariant, stated once.**
`leaderReaped` means "this supervisor has taken the leader's outcome out of
`waitCh`". It is a statement about the *channel*, not about whether the run was
successfully dispositioned, so it must be set at the receive and nowhere later.
The shared `leaderReaped = true` at `:523` is then redundant on the arbitrated
path and unchanged on every other — it stays exactly as it is.

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
is discarded and the launch returns `U_RUN_RECONCILE_REQUIRED` — the same
*reported* outcome as today, since today never drains at all. With
`leaderReaped = true` set at the receive there is exactly **one** residual
behavioural delta, and only under `StdinConnect`: the input-plane defer takes its
early-return arm, so it no longer calls `killDetachedInputChild`. That is written
down in full as §8(7) rather than waved through. The `--stdin-connect` +
refused-disposition shape is pinned by T7.

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
| T7 | `TestAIRA131DetachedStdinConnectRefusedDispositionLeavesLedgerIntact` | **`StdinConnect: true`** + `Timeout` + T4's refused disposition, i.e. the drain SUCCEEDS and the disposition is then refused. Asserts: (a) **no `input-abort-incomplete` event** anywhere in the journal; (b) the ledger's `kill-intent` — and the concurrent `kill-completed` written into the window — **survives byte for byte** (`KillIntent.Present`, `Completed:true`, its `Sequence` unchanged, `!NotExecuted`); (c) the launch returns `U_RUN_RECONCILE_REQUIRED` **within the drain bound**, not after an extra ~2s (assert wall-clock < `arbitrationWaitBound(grace)` + a generous margin that is still well under 2s); (d) 0 terminal records | `leaderReaped = true` at the drain receive (§6.3). Without it the defer blocks 2s and then durably erases the intent — the plan gate's blocking finding |

**T4's concurrent writer must be a direct locked `r.append`, not a real
`Kill()`.** In the arbitrated state the scope is empty by construction, so a real
`Kill()` → `killScope` returns at `len(pids) == 0` and `finishDetachedKill`
(`runner_linux.go:2410-2434`) appends **nothing**: a `Kill()`-driven T4 would be
vacuous. The test therefore takes the per-run lock itself and appends a
non-terminal `kill-completed` record carrying `KillIntent{Present, Completed:true,
Sequence: <the launch's>}` plus a completed `ScopeKill` — *simulating* what
`finishDetachedKill` writes when the scope is populated. This is deliberate and
must not be mistaken for a real-`Kill()` race; the assertion is about what
`appendDetachedNotExecutedLocked` does when it reads such a record, which is
precisely the CAS §5 adds. T7 reuses the same device.

T4's and T7's window is ≥ the bounded drain (≈600ms in the harness), so a ledger
poller lands inside it reliably; no new production test hook is introduced. If
review judges the poll racy, the fallback is an existing-style
`r.beforeRunningAppendFn` sibling hook — but only if needed, and it must not be
reachable in production.

**T7's harness shape — verified feasible, not assumed.** T1's `startFn` already
overwrites `cmd.Stdin` with `gatedStdin` immediately before `cmd.Start()`
(`detached_timeout_arbitration_linux_test.go:56-67`). That override also applies
under `StdinConnect`, where production sets `cmd.Stdin = inputPlane.inputR`
(`detach_linux.go:393`) — a `*os.File`, which `os/exec` would dup rather than
copy, giving no goroutine to hold `Wait()` open. Overwriting it with the
non-`*os.File` `gatedStdin` restores the copy goroutine and hence the
reaped-but-outcome-pending window T1 depends on, while the `if req.StdinConnect`
arm (and therefore the `:374-392` defer) is genuinely taken. The plane's own
`inputPlane.inputR` is simply closed unused at `:433`, and `inputPlane.serve()`
runs with no client dialling it. T7 sets `Config.InputRuntimeDir` (or
`r.inputRuntimeDir`) from the existing `newRunInputRuntimeDir(t)` helper
(`run_input_server_linux_test.go:403`), as `TestRunInputPathFailureOccursBeforeChildStart`
already does, so no new harness is written.

**T1-T6 stay on the non-connect path** (no `StdinConnect`), where the
`:374-392` defer is not installed at all — so they are unaffected by this fix and
remain exactly as v1 specified them. T5 in particular (drain expired, `waitCh`
NOT drained) is byte-for-byte unchanged.

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
7. **The one residual behavioural delta of the fix, stated exactly.** Setting
   `leaderReaped = true` at the drain receive (§6.3) means that, under
   **`--stdin-connect` only**, a *refused* disposition after a *successful* drain
   no longer runs `killDetachedInputChild(scope, cmd)` from the `:374-392`
   defer — the defer takes its early-return arm after `inputPlane.closeTerminal()`.

   That call is **signal-less and not evidence-bearing in this state**: the
   arbitrated arm is only reached when `attempt.Kill.Empty && !attempt.Kill.Started`
   (the scope was found empty by two independent reads, so `scope.Kill()` has
   nothing to signal) and `processLive(record.PIDIdentity) == processDead` and
   `cmd.Wait()` has returned (so the leader is reaped, and the subsequent
   `syscall.Kill(-pid, SIGKILL)` / `cmd.Process.Kill()` can only return `ESRCH`).
   It writes nothing to the ledger. Dropping it is therefore a no-op in observable
   behaviour, and marginally *safer*: it removes a raw negative-PID `SIGKILL`
   aimed at an already-reaped process-group id, which is the one component of
   that helper that a PID recycle could in principle mis-target.

   What is **not** dropped: `inputPlane.closeTerminal()` still runs
   unconditionally as the defer's first statement, so the input socket and fd0
   are torn down exactly as today on every return.

   This delta is confined to `StdinConnect` + `Timeout` + arbitration-eligible +
   refused-disposition. On the drain-**expired** path (T5) and on every
   non-arbitrated path, `leaderReaped` stays false and the defer behaves byte for
   byte as today. Accepted, pinned by T7.

8. **Real-cgroup detached soak.** The reproduction is hermetic and
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
| **A drain that empties `waitCh` strands the input-plane defer** — a 2s stall plus an unlocked `input-abort-incomplete` append that durably erases the kill intent (the plan gate's blocking finding) | `leaderReaped = true` at the drain receive, before any return (§6.3); §2.3 records the mechanism; T7 pins it |
| The arbitrated arm falls into the proven-kill continuation (v1's nested-`select` `break`) | §6.3's required control-flow contract (1)+(2); T6 would report a fabricated `kill-completed`, T1 would hang on the drained `waitCh` |

---

## 10. Expected yield

One correctness class closed: a detached run whose child exited cleanly can no
longer be reported as reconcile-required because a deadline fired against an
already-empty scope, and the branch that decides it gains its first tests
(T1-T7) where it had none. One durable-evidence CAS brought to parity with the
foreground path, with the parity question answered from the actual lock scope
rather than assumed. Additionally, the `--stdin-connect` abort defer's
interaction with a drained `waitCh` is now specified and tested (T7) — closing a
durable-evidence hazard that v1 of this plan would itself have introduced.

---

## 11. Two-loop record

- **Plan review:** Codex (Sol) + DeepSeek-pro, orthogonal lineages; Fable is the
  plan gate. Correctness-critical (kill/terminal CAS) → full two-loop, not the
  light path.
- **Plan gate, round 1: FAIL** — one blocking finding (the drained-then-refused
  `StdinConnect` path stranding the input-plane defer). Everything else verified
  sound against source at `019ab4b`; the reproduction was re-run by the gate
  (red 3/3, exit 1, ~0.34s each) and judged honest, so **the reproduction is
  unchanged** — the finding was in the fix design, not in the repro. v2 folds in
  `leaderReaped = true` on drain success (§6.3), the §2.3 mechanism record,
  §8(7), T7, the §5 ledger-level backstop, the §6.3 control-flow contract, and
  the §7 statement of what T4's concurrent writer must be. Awaiting re-gate.
- **Build:** TDD from T1's recorded red; Opus builds (per the owner's standing
  direction for correctness-critical, architecturally subtle AIRA work).
- **Build review:** adversarial, both false-fail and false-pass directions, with
  every confirmed counterexample becoming a regression test.
- **Build gate:** Fable, then PR and merge.
