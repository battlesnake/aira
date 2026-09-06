# AIRA-126 — arbitrating a timeout kill intent that was published against an already-empty scope

Status: planned. Branch `aira126-kill-terminal-arbitration` off `origin/master`
at `9464320`. Two-loop work per CLAUDE.md (kill/terminal-CAS arbitration): this
document is the plan artifact for plan-review and the plan gate. No production
code is changed by this commit.

Line references below are `origin/master` at `9464320`.

## 1. The state that produces the defect

`internal/runner/runner_linux.go` Launch, timer branch (lines 586-600):

```go
case <-timer.C:
        attempt, killErr := r.killWithIntent(ctx, id, "run-timeout", killPolicy{Enforce: false})
        timeoutKill = attempt
        if killErr != nil || !attempt.IntentPublished {
                timedOut = killErr != nil || !attempt.WaitPublished
        } else {
                timedOut = true
        }
```

`killWithIntent` (2186-2261) publishes `KillIntent{Present:true}` durably
*before* touching the scope, then calls `killScope` (2153-2176), which opens
with:

```go
pids, err := scope.Members()
...
if len(pids) == 0 {
        empty, emptyErr := scope.Empty()
        return killResult{Empty: empty && emptyErr == nil}, emptyErr
}
```

So when the leader has already exited, `killScope` returns
`{Started:false, Completed:false, Empty:true}` with a nil error — deliberately
refusing to treat emptiness as proof that *this* kill won (its own comment says
so, and §6.3(4) of the runner-lite spec forbids exactly that inference).

Launch then sets `timedOut = true` (killErr nil, intent published), the timeout
block at 800-812 takes the `!timeoutKill.Kill.Completed` arm — appending
`U_RUN_RECONCILE_REQUIRED` and downgrading `ScopeIntegrity` to
`handoff-unverified` — and the terminal CAS at 851 hits
`latest.KillIntent.Present && !latest.KillIntent.Completed`, appends
`capture-finalized`, and returns a **non-terminal** record plus
`U_RUN_RECONCILE_REQUIRED: kill intent won before terminal evidence`.

The wait outcome is sitting unread in `waitCh` at that moment and is discarded.

The exact production state, all four facts required, is therefore:

1. the timer branch was taken;
2. `killErr == nil` and `attempt.IntentPublished`;
3. `attempt.Kill.Started == false && attempt.Kill.Empty == true` — two
   independent reads (`Members()` empty, `Empty()` true) and, by control flow,
   **no signal was sent by this kill to anything**;
4. the child's own wait result is pending in `waitCh`.

Measured at ~2% of the deadline-straddling scenario in isolation (iteration 43
of an 800-iteration probe, AIRA-112's soak), ~0.25% per full-suite run.

Root asymmetry worth naming: the spec's arbitration (§6.3(1)) has the waiter
publish `wait-observed` *immediately at reap*, which is what
`killWithIntent`'s `waitPublished` check (2216-2222) consumes. The foreground
implementation publishes `wait-observed` only **after** the select (line 637),
so in a foreground run that check can essentially never fire and the wait
evidence has no way to win the race it is supposed to win. §7 below explains why
this plan does not move that publication.

## 2. Decision — arbitrate to `exited` with the real exit code

**Reading (1) of the ticket is adopted.** When the four facts above hold *and*
the leader is provably dead at the instant the kill was refused, Launch honours
the pending wait outcome, commits a normal terminal `exited` record with the
real exit code, and records the published intent as **not executed** — durably,
in the record, not by deleting it.

Reading (2) — treat a provably-empty scope as a vacuously-completed kill and
commit `killed` — is rejected.

### 2.1 The spec is not neutral here

`docs/superpowers/specs/2026-08-11-aira-m12-runner-lite-design.md` §6.3:

> 3. Otherwise the kill intent blocks the waiter from committing `exited`.
>    `run-kill` performs the bounded grace and whole-scope `cgroup.kill`, then
>    verifies the scope is empty. **Only after both conditions** does it acquire
>    the terminal slot and append `killed` once.
> 4. … It **never appends `killed` merely because intent exists.**

In the AIRA-126 state exactly one of those two conditions holds: the scope is
verified empty; the bounded grace and `cgroup.kill` were never performed at all.
Reading (2) would append `killed` on half the required proof, which is the
inference both the spec and `killScope`'s comment refuse. It would additionally
discard a real, observed exit code in favour of a synthetic status — and §6.3
closes with "no path fabricates an exit code for a kill or lost outcome"; the
mirror of that rule is that no path discards a real one for a kill that never
happened.

### 2.2 The counter-case, taken seriously

*Could the kill signal have been delivered a moment before the wait observed the
exit, making "killed" the more honest answer?*

For **this** code path: no, and not as a matter of timing inference but of
control flow. `killScope` returns at `len(pids) == 0` **before** `Terminate` and
before `Kill` — `Started == false` is a positive record that this kill operation
emitted no signal to any process. The record `killed` would attribute the death
to a signal AIRA can prove it never sent. There is no window to lose here,
because there is no signal.

Three adjacent scenarios where something *was* signalled, and how they are told
apart:

- **The kill did fire.** `Members()` returned pids, `Terminate`/`Kill` ran,
  `Started == true` (and `Completed == true` on success). That is the existing,
  untouched `timedOut` path and it correctly reports `killed`. This plan changes
  nothing there.
- **Someone else's signal killed the child** (an external `aira run kill`, an
  operator SIGKILL, the OOM killer) a moment before the deadline. Then the
  kernel tells us so through the wait status: `waitEvidence` (1361-1377) returns
  a signal name, the record carries `Signal`, and `classifyOOMKilled` (usage_
  linux.go:239) still promotes an OOM death to `oom-killed` because the new
  branch leaves `explicitKill` false. The honest outcome is reported from the
  evidence that actually exists — the kernel's — not from our undelivered
  intent. A foreign *intent* is separately protected: see §4.1.
- **The child was still alive at the deadline and the scope was empty anyway**
  (escaped/migrated leader, or an unverified placement). Here the deadline was
  genuinely breached and AIRA genuinely failed to kill anything. Reporting
  `exited` when the process later finishes on its own would be the dishonest
  answer. This case *is* distinguishable, and §3 excludes it explicitly by
  requiring kernel proof of death at the moment the kill was refused.

What remains architecturally indistinguishable is only this: whether the child's
exit fell a few microseconds *before* or *after* the deadline instant. We hold
no exit timestamp, so that ordering is unestablished and must not be asserted in
either direction. This is where a default has to be chosen, and the project's
honesty discipline chooses the established fact over the plausible one: the
process exited by itself with status N (established, by `wait4`), and no kill
was delivered (established, by control flow). "Timed out and killed" would
assert a cause that did not occur. So the run reports its real exit, and the
fired-deadline fact is preserved in the record as an intent that was published
and provably not executed — see §4.2 on why no `E_RUN_TIMEOUT` is appended.

### 2.3 Why not simply widen `killScope`

Because `killScope` is right. Its refusal protects every other caller
(`Kill`, `failBeforeLaunch`, `Reconcile`, the detached finaliser), several of
which have no wait evidence at all and for whom an empty scope really is
uninformative. The arbitration belongs at the one call site that *does* hold the
missing evidence — Launch, which owns the child and its `wait4`.

## 3. The arbitration rule

New pure function in `internal/runner/decisions.go`, beside `decideReconcile`
(the file is already the home for exactly this kind of rule):

```go
// decideTimeoutIntentNotExecuted reports whether a run-timeout kill intent this
// launch published was provably never delivered to anything, so the child's own
// wait result is the established outcome.
func decideTimeoutIntentNotExecuted(killErr error, attempt killAttempt, leader processLiveness) bool {
        return killErr == nil &&
                attempt.IntentPublished && attempt.IntentCreated &&
                attempt.Kill.Empty && !attempt.Kill.Started &&
                leader == processDead
}
```

Each conjunct carries its own weight:

- `killErr == nil` — an errored kill is unevaluated, never dismissed.
- `IntentPublished && IntentCreated` — the intent is **ours**, created by this
  timeout, not adopted from a concurrent external kill (§4.1).
- `Kill.Empty && !Kill.Started` — the only `killScope` return shape that proves
  no signal was emitted *and* that the scope was verified empty by two reads.
  A scope repopulated between `Members()` and `Empty()` yields `Empty:false` and
  falls through to today's behaviour, fail-closed.
- `leader == processDead` — `processLive(record.PIDIdentity)` evaluated
  **immediately after `killWithIntent` returns**, i.e. at the instant the kill
  found nothing to signal. Post-AIRA-112 this is authoritative for `ENOENT`,
  `ESRCH`, a start-tick mismatch, and a `Z`/`X` state (a not-yet-reaped zombie
  reads dead), and returns `processUnknown` rather than a guess for a read it
  cannot interpret. `processAlive` or `processUnknown` → no arbitration; the run
  keeps today's `timedOut` outcome. This is the conjunct that separates "already
  gone before any signal" from "still running past its deadline and unkillable".

Only when all five hold does Launch take the pending wait:

```go
case <-timer.C:
        attempt, killErr := r.killWithIntent(ctx, id, "run-timeout", killPolicy{Enforce: false})
        timeoutKill = attempt
        intentNotExecuted := decideTimeoutIntentNotExecuted(killErr, attempt, processLive(record.PIDIdentity))
        ...
```

with the receive bounded purely as an anti-hang guard:

```go
if intentNotExecuted {
        select {
        case outcome := <-waitCh:
                waitErr, waitState = outcome.err, outcome.state
                timedOut = false
        case <-time.After(r.grace):
                intentNotExecuted = false   // fail closed: today's outcome
        }
}
```

A dead leader's `cmd.Wait()` is already blocked in `wait4` on our own child, so
only reaping and one scheduler wakeup remain; `r.grace` (2s default) is orders
of magnitude more than that and is the runner's existing "how long we wait for
evidence" knob. Expiry cannot produce a wrong answer, only today's answer.

## 4. Exact code changes

`internal/runner/types.go`

- `killAttempt` (2178-2185) gains `IntentCreated bool`, set true only inside
  `killWithIntent`'s `if !current.KillIntent.Present` branch after the
  `kill-intent` append succeeds (2223-2231). It is false when the intent was
  already present — a foreign kill or a steal.
- `KillIntent` (types.go:67-72) gains
  `NotExecuted bool \`json:"not_executed"\`` with a doc comment stating: the
  intent was published durably and then provably delivered no signal, because
  `killScope` found the scope empty (`Members()` empty **and** `Empty()` true)
  and returned before `Terminate`/`Kill`, with the leader proved dead at that
  instant. It is a *disposition*, never a claim that a kill happened, and it is
  set only by the launch that created the intent. `Completed` keeps its exact
  present meaning and is **never** set on this path.

`internal/runner/decisions.go` — `decideTimeoutIntentNotExecuted` as above.

`internal/runner/runner_linux.go`

- Timer branch (586-600): the arbitration above. `timedOut` stays true in every
  other case, so the existing behaviour is byte-for-byte preserved outside the
  five-conjunct state.
- Timeout status block (800-812): unchanged; it is skipped because
  `timedOut == false`. Control reaches
  `else if waitObserved && (scopeVerified || unobservedPlacedExit)` (813) and
  commits `StatusExited` with the real `waitExit`/`waitSignal`. `E_RUN_FAILED`
  for a non-zero exit still applies (838).
- Immediately before the terminal lock, when `intentNotExecuted`:
  `record.KillIntent = KillIntent{Present: true, Sequence: timeoutKill.IntentSequence, NotExecuted: true}`.
  This is load-bearing: `mergeEvidence` (evidence.go:87-89) copies the
  candidate's `KillIntent` only when `candidate.KillIntent.Present`, so without
  this assignment the merged record keeps the ledger's undisposed intent and the
  CAS still blocks. `record.ScopeKill` is deliberately left zero so the merge
  keeps the durable `{Requested:true, Started:false, Completed:false}` — a scope
  kill was requested and never started, which is what happened.
- Terminal CAS (851). Capture the pre-merge fact first, then gate on it:

  ```go
  intentAlreadyCompleted := latestErr == nil && latest.KillIntent.Completed
  latest = mergeEvidence(latest, record)
  honourNotExecuted := latest.KillIntent.NotExecuted && !intentAlreadyCompleted &&
          latest.KillIntent.Sequence == timeoutKill.IntentSequence
  if latestErr == nil && latest.KillIntent.Present && !latest.KillIntent.Completed && !honourNotExecuted {
          // unchanged: capture-finalized + U_RUN_RECONCILE_REQUIRED
  }
  ```

  `intentAlreadyCompleted` closes the one merge hazard: a concurrent actor that
  durably completed the intent between our arbitration and the CAS must not have
  that evidence overwritten by our disposition. The sequence equality is
  belt-and-braces on top of `IntentCreated`. The single-terminal-slot invariant
  is untouched — the CAS still runs under the per-run lock and still returns
  early when `latest.Status.Terminal()` (846-853).

`internal/runner/detach_linux.go`

- One defensive guard only: the detached finaliser's
  `case current.KillIntent.Present:` arm (749-753) becomes
  `case current.KillIntent.Present && !current.KillIntent.NotExecuted:` so the
  new field can never mean `killed` anywhere in the tree. See §6.

### 4.1 A foreign kill intent is never discarded

`IntentCreated` is the guard the ticket asks for ("the first must not silently
discard a durable kill intent"). If an external `aira run kill` published the
intent, `killWithIntent` adopts it, `IntentCreated` is false, no arbitration
happens, and the run keeps today's outcome — the external killer completes its
own kill and its terminal record, or reconcile resolves it. Only the intent this
launch itself created, for its own deadline, can be dispositioned by it.

### 4.2 What `KillIntent.Completed` does, and does not, become

It stays `false`, permanently, for a non-executed intent. `Completed` means
`cgroup.kill` executed and the scope was proved empty *afterwards*
(`killScope`'s `{Started:true, Completed:true}`), and nothing in this plan
weakens that. The three-state disposition of a published intent is therefore:

| `Present` | `Completed` | `NotExecuted` | meaning |
| --- | --- | --- | --- |
| true | true | false | the kill ran and the scope was proved empty after it |
| true | false | true | the kill was refused with nothing to signal; the leader was already dead |
| true | false | false | unresolved — blocks the terminal CAS, needs reconcile (unchanged) |

`{Completed:true, NotExecuted:true}` is not reachable and no code sets it.

## 5. What the record says, and why no `E_RUN_TIMEOUT`

The committed record is: `status=exited`, the real exit code (or signal),
`ScopeIntegrity` from the ordinary classifier, `ScopeKill{Requested:true}`,
`KillIntent{Present:true, Sequence:N, NotExecuted:true}`, `TerminalComplete`,
one terminal event, no `E_RUN_TIMEOUT`, no `U_RUN_RECONCILE_REQUIRED`.

`E_RUN_TIMEOUT` asserts that a run was terminated for exceeding its deadline.
Here nothing was terminated, and whether the exit preceded the deadline is
unestablished (§2.2). Appending it would be an assertion AIRA cannot support;
the fired deadline is preserved as the durable `kill-intent` ledger event plus
the `NotExecuted` disposition on the record, which is the honest shape: *the
timer fired, an intent was published, it delivered nothing*.

Consequently `CleanSuccess()` (types.go:144-147) can be true for such a run
(exit 0, contained, capture complete, no error codes). That is correct: the
command succeeded and the deadline had no effect on it. No new error code is
introduced, so `internal/codes` is untouched.

## 6. The detached path — analysed, bounded, deferred

The same state is structurally reachable in `detach_linux.go`: the child is
placed with `CLONE_INTO_CGROUP` (338) while the supervisor stays outside the
scope, so the run scope can be empty when the detached timeout fires. That
branch (504-517) is stricter than the foreground one — it returns
`U_RUN_RECONCILE_REQUIRED` on `!attempt.Kill.Completed` *without even draining
`waitCh`*.

This plan does **not** fix it, for scope reasons: it has no reproduction here,
it needs its own durable-evidence event (the detached finaliser reads the ledger
rather than in-process state), and mixing it in would make the reproduction
evidence for this ticket ambiguous. Instead:

- the one-line guard in §4 makes it impossible for a `NotExecuted` intent to be
  read as `killed` by the detached finaliser, with a unit test pinning it;
- a follow-up ticket is filed at implementation time with `aira create` (no
  hand-picked ID), carrying the trace above, and linked `relates` to AIRA-126.

## 7. Deliberate non-changes

- **`wait-observed` is still published after the select** (line 637), and the
  gate `!current.KillIntent.Present` still suppresses it on this path, so the
  spec's §6.3(1) "publish immediately at reap" deviation stands. Moving it into
  the wait goroutine would put a per-run flock acquisition on the reap path of
  *every* run, would only shrink — not close — the race (the child can still
  exit between the `kill-intent` append and `killScope`'s `Members()` read,
  which is precisely the state this plan arbitrates), and would append a
  `wait-observed` event *after* a `kill-intent` event, muddying the ordering
  fact the ledger exists to record. It is not needed once the arbitration is
  grounded in control flow plus kernel liveness. If a reviewer wants spec
  conformance for its own sake, it is a separate ticket, not a rider on this
  one.
- **`killScope`, `decideReconcile`, `Reconcile`, `Kill`, `failBeforeLaunch`,
  `mergeEvidence`'s general semantics, and the OOM classifier are untouched.**
- **No new ledger event kind.** The `kill-intent` event and the terminal event
  carry the whole story.

## 8. Accepted coverage gaps, written down

1. **Crash window.** If Launch dies between the arbitration and the terminal
   append, the ledger holds `kill-intent` with no `wait-observed` and no
   terminal. `decideReconcile(false, true, empty=true, false)` (decisions.go:15)
   then commits `lost` + `U_RUN_EXIT_UNKNOWN`. That is honest — AIRA never
   published the exit — conservative, and it never fabricates `killed`. It is
   the same outcome this state produces today.
2. **Bounded receive expiry.** A `processDead` leader whose wait result does not
   arrive within `r.grace` falls back to today's `U_RUN_RECONCILE_REQUIRED`.
   Not expected to fire; it is the fail-closed edge, not a target behaviour.
3. **Concurrent foreign intent.** A run whose timeout coincides with an external
   `aira run kill` keeps today's outcome (§4.1), including the reconcile-
   required record if the foreign killer also finds the scope empty. Narrowing
   that needs the external killer's own evidence and is out of scope.
4. **Exit-vs-deadline ordering** stays unknowable (§2.2) and is deliberately not
   asserted in the record.

## 9. Tightening AIRA-112's accommodation

`runner_test.go` lines 868-903 (the `if err != nil` arm of the third scenario,
which begins at line 867) currently accept the reconcile-required outcome
of the third scenario against a seven-part evidence signature, with a pointer
here. **The whole accommodation is removed** — any `LaunchError` from that
launch fails the test again — because after this change the reconcile-required
outcome requires an escape, a kill error, or a foreign intent, none of which
`/bin/sleep 0.04` in a private real cgroup can honestly produce. Keeping the
tolerance would leave the test blind to a regression of exactly this fix.

It is replaced by a strictly stronger assertion in the surviving branches:

- `StatusExited`: as today, plus — if `KillIntent.Present` (the timer did fire)
  then `KillIntent.NotExecuted && !KillIntent.Completed && !ScopeKill.Started`
  and no `E_RUN_TIMEOUT`;
- `StatusKilled`: unchanged (`E_RUN_TIMEOUT`, `KillIntent.Completed`,
  `ScopeKill.Completed`);
- exactly one terminal record, in both.

The previously-tolerated outcome thereby becomes a positively-asserted one.

## 10. Test plan

TDD; every test below is written before its production change and **proved
against the wrong behaviour by executed revert**, not by reading.

### 10.1 Pure rule (deterministic, `internal/runner/decisions_test.go`)

`TestAIRA126TimeoutIntentNotExecutedRule` — one row per conjunct, each differing
from the honouring row in exactly one input, so no row can pass for the wrong
reason: `{nil err, published, created, Empty, !Started, processDead}` → true;
and false for each of `killErr != nil`, `!IntentPublished`, `!IntentCreated`
(foreign intent), `Started` (a signal was sent), `!Empty` (scope repopulated),
`processAlive`, `processUnknown`.

### 10.2 Intent provenance (`internal/runner/runner_test.go`)

`TestAIRA126KillWithIntentReportsIntentCreatedOnlyForItsOwnIntent` — against a
seeded ledger: no prior intent → `IntentCreated` true; a pre-existing
`kill-intent` event → `IntentCreated` false with `IntentPublished` still true.

### 10.3 Hermetic Launch, both directions (no cgroups needed)

Using the existing `memoryBackend`/`memoryScope` (runner_test.go:609-626, whose
`Members()` is empty and `Empty()` true — the AIRA-126 scope state by
construction) plus the existing `r.startFn` hook to start a real child without
`CLONE_INTO_CGROUP`, and `readProcStatFn` (1496) flipped to `ESRCH` after start
where a dead leader must be simulated (AIRA-112's technique):

- `TestAIRA126TimeoutAgainstAlreadyExitedLeaderArbitratesToExited` — short-lived
  child, small timeout, looped in-process (~100 iterations). Invariant asserted
  on **every** iteration, valid whichever select branch wins: no `LaunchError`;
  `Status == StatusExited` with the real exit code; exactly one terminal record;
  and if `KillIntent.Present` then `NotExecuted && !Completed`. Against the
  pre-fix code the timer-branch iterations return `U_RUN_RECONCILE_REQUIRED`
  with a non-terminal record — the production symptom.
- `TestAIRA126LiveLeaderAtDeadlineStillTimesOut` — the mutation guard, and the
  most important test in the set: a child that is **still running** at the
  deadline with the same empty fake scope. `processLive` reads alive, so the
  arbitration must refuse: the record must keep `E_RUN_TIMEOUT` and today's
  reconcile-required/`handoff-unverified` shape, and must **not** report
  `exited`. This is the test that stops the fix being widened into "an empty
  scope means the run finished".
- `TestAIRA126ForeignKillIntentIsNotDismissed` — a `kill-intent` event seeded in
  the ledger before Launch's deadline fires; the record must not gain
  `NotExecuted` and must keep today's outcome.

### 10.4 Terminal CAS

- `TestAIRA126NotExecutedIntentCommitsOneTerminalExited` — a record carrying
  `KillIntent{Present, Sequence:N, NotExecuted:true}` passes the CAS, commits
  `exited`, one terminal event.
- `TestAIRA126UndisposedIntentStillBlocksTerminal` — `KillIntent{Present}` with
  neither `Completed` nor `NotExecuted` still yields the non-terminal
  `capture-finalized` + `U_RUN_RECONCILE_REQUIRED` path. (Passes both before and
  after the change by design: it is the over-widening guard, the analogue of
  AIRA-112's `EACCES` test.)
- `TestAIRA126CompletedIntentIsNotDowngradedByNotExecuted` — pre-merge
  `Completed:true` in the ledger, `NotExecuted:true` in the candidate: the CAS
  must not honour the disposition.

### 10.5 Detached guard

`TestAIRA126DetachedFinaliserDoesNotReportKilledForNotExecutedIntent` — the
`case` guard from §4, pinned so the field cannot silently acquire "killed"
semantics on the other path.

### 10.6 Real-cgroup reproduction of the original race (the load-bearing one)

Per AIRA-112's own technique, looped, real cgroup, `AIRA_REAL_CGROUP=1`, run
serialised and under `aira confine --`:

1. **Pre-fix, on this branch before the production change**, an isolated probe
   of the third scenario alone (`/bin/sleep 0.04`, `Timeout: 50ms`), 800
   iterations, must **fail** with the ticket's signature
   (`U_RUN_RECONCILE_REQUIRED: kill intent won before terminal evidence`,
   non-terminal record). Recorded with its iteration number and exit code. This
   is what proves the end-to-end test is not porous; without an observed pre-fix
   failure the soak proves nothing.
2. **Post-fix**, the same 800-iteration probe: exit 0.
3. **Post-fix**, the standing test at `-count=400`
   (`-run TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration`), matching
   AIRA-112's soak depth: exit 0.
4. The probe is committed as an executable reproduction (a `-count`-driven test
   guarded by `AIRA_REAL_CGROUP`, in the existing real-cgroup style) rather than
   left as a shell transcript, per CLAUDE.md's "a published measurement must
   have a committed, executable reproduction".

### 10.7 Executed revert check

After the suite is green, the production change is reverted in the working tree
(the arbitration call plus the CAS gate) and `go test ./internal/runner/ -run
'TestAIRA126' -count=1` must exit non-zero, with the *named* tests that fail
recorded — 10.3's first test and 10.4's first test at minimum, and 10.3's guard
test explicitly expected to pass on both sides. The tree is then restored and
`git status --porcelain` confirmed empty before the gate is re-run.

### 10.8 Merge gate (foreground, exact exit codes recorded)

```
aira confine -- go build ./...
aira confine -- go vet ./...
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1
gofmt -l internal/ cmd/
```

## 11. Files touched

| file | change |
| --- | --- |
| `internal/runner/types.go` | `KillIntent.NotExecuted`; `killAttempt.IntentCreated` |
| `internal/runner/decisions.go` | `decideTimeoutIntentNotExecuted` |
| `internal/runner/runner_linux.go` | timer-branch arbitration; `record.KillIntent` disposition; terminal-CAS gate; `IntentCreated` in `killWithIntent` |
| `internal/runner/detach_linux.go` | one-condition guard in the detached finaliser |
| `internal/runner/runner_test.go` | AIRA-112 accommodation removed and tightened; new tests |
| `internal/runner/decisions_test.go` | rule table |
| `internal/runner/detach_linux_test.go` | detached guard test |
| `.aira/tickets/AIRA-126.md` | status + resolution at PR time |

No changes to `internal/codes`, `internal/store`, `internal/core`, or any face:
`KillIntent` is confined to `internal/runner` and its ledger JSON, and no new
error code is introduced.

## 12. Gate conditions — amendments made at implementation time

The plan gate returned GATE-PASS-WITH-CONDITIONS. Each condition and how the
built change satisfies it:

**C1/C2 — the terminal CAS must read the ledger's KillIntent BEFORE the merge.**
The §4 sketch was a tautology and its pre-merge `record.KillIntent` assignment
would have clobbered a concurrently-completed intent. Implemented instead as a
pure rule, `decideNotExecutedDisposition(intentNotExecuted, ledgerErr,
ledgerIntent, publishedSequence)` in `decisions.go`, evaluated on `latest`
**before** `mergeEvidence`. `record.KillIntent` is left zero throughout, so the
merge preserves the ledger's own intent (sequence, requested scope kill) and the
disposition is the single field the arbitration adds afterwards. A concurrent
`Completed:true`, a foreign sequence, or a ledger read error all refuse it and
leave the unchanged reconcile-required arm in charge.
`TestAIRA126CompletedIntentIsNotDowngradedByNotExecuted` drives a real
concurrent completion through a live Launch and asserts the ledger, not the
return path.

**C3 — the spec forbade reading (1) as literally as reading (2).** Amended in
this PR: runner-lite §6.3 gains clause (5) for an intent that finds nothing to
signal, plus a note on the `kill_intent` record field. Clauses (3) and (4) are
unchanged and still govern every intent that has something to signal.

**C4 — §10.3's hermetic tests were not viable as written.** Option (a) is taken,
with the scope fake rebuilt rather than seeded: `livenessScope.Members()` models
`cgroup.procs` against real kernel state (a task is a member exactly while its
`/proc` entry exists with the recorded start tick, so a zombie is still listed
and a reaped task is not). Launch therefore reaches `scopeVerified == true` and
appends `running`, and the scope genuinely empties on reap, so `killScope`
really returns at `len(pids) == 0` with `Started == false`. `startFn` strips
`UseCgroupFD`. The `readProcStatFn → ESRCH` device is dropped entirely, as the
condition required.

The remaining problem — that the timer branch versus the wait branch is decided
by chance — is solved without faking any evidence: `gatedStdin` holds
`Cmd.Wait()` open past the deadline through its stdin copy goroutine, which
os/exec joins only *after* reaping. The child really exits, is really reaped and
the scope really empties before the deadline; only the moment Launch learns of
the wait outcome is controlled. The timer branch then fires deterministically
(observed 6/6 iterations), against a genuinely empty scope and a genuinely dead
leader.

**C5 — non-vacuity.** Both loops count the iterations that actually took the
timer branch, log the count, and fail if it is zero.

**C6 — bounded receive.** The arbitration receive has its own bound,
`arbitrationWaitBound(grace) = max(grace, 250ms)`; `r.grace` alone (1ms in the
memory-runner tests) would expire spuriously and make the fail-closed fallback
indistinguishable from a regression.

**C7 — orphan cleanup.** The harness records the child's `*os.Process` and
`t.Cleanup` kills it, so the mutation-guard test cannot leak a live process on a
failing run.

**C8 — the second detached site.** Both `detach_linux.go` sites are guarded, not
one: `terminalizeDetachedNoChild` (which also stops deriving
`KillIntent.Completed` from `Present` alone) and the detached finaliser's
`case`. Each has its own test.

**C9 — honest deviation record.** Written into the ticket resolution and §5:
exit-versus-deadline ordering is unestablished and `E_RUN_TIMEOUT` is
deliberately omitted, so an `exited 0` record carrying
`KillIntent.NotExecuted` means the deadline DID fire and killed nothing. No face
reads `KillIntent`, so the surfacing note is a one-line addition to the spec's
run-record field table rather than a rendering change.

### 12.1 §10.6's committed reproduction — re-specified

The real-cgroup probe is committed as `TestAIRA126RealCgroupDeadlineStraddleSoak`
and is opt-in (`AIRA126_SOAK=1`, `AIRA126_SOAK_ITERATIONS`, default 800). The
race is ~2% per iteration, so a loop short enough for the standard suite would
pass vacuously most of the time — exactly the porous shape C5 rejects — and a
loop long enough to be non-vacuous costs a minute of suite time on every run.
Making it opt-in keeps the measurement's reproduction executable and honest
about what it proves, and the always-on coverage of the same arbitration is the
hermetic pair, whose device makes the timer branch deterministic rather than
lucky.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
