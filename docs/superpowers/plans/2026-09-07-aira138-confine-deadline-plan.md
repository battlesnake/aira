# AIRA-138 — `aira confine`: a job deadline, including cumulative CPU-time, for a supervisor with no run ledger

Status: **plan, awaiting plan-gate review**. Correctness-critical kill/terminal
arbitration on a genuinely new code path; full two-loop per `CLAUDE.md`, not the
light path.

Ticket: `.aira/tickets/AIRA-138.md`. Deferred out of AIRA-136
(`docs/superpowers/plans/2026-09-06-aira136-cpu-time-timeout-plan.md`, PR #83,
merged `96edada`). Prior art this plan builds on and must not re-litigate:
AIRA-126 (`docs/superpowers/plans/2026-09-06-aira126-kill-terminal-arbitration-plan.md`)
and AIRA-131 (`docs/superpowers/plans/2026-09-06-aira131-detached-timeout-arbitration-plan.md`).

Branch: `aira138-confine-deadline`. Reproduction/danger-proof artifact:
`internal/runner/confine_deadline_danger_linux_test.go` (committed with this
plan; §8 says how the implementation inverts it).

---

## 0. Source read, at the commit this plan was written against

Every line reference below was read fresh in this worktree at `c7b3e32`. The
ticket's own references were checked and are current except where noted.

| Fact | Where | Confirmed |
| --- | --- | --- |
| confine's wait is unconditional | `internal/runner/confine_linux.go:1253` — `exitCode, termination := waitConfineCommand(cmd)` | yes; no select, no timer, no channel |
| `waitConfineCommand` is a bare `cmd.Wait()` decoder | `confine_linux.go:2066-2081` | yes |
| confine's accepted launch options | `cmd/aira/main.go:785-802` — valueless `delegate-ram`/`detach`/`exclusive`; valued `slice`/`name`/`owner`/`memory-reserve`/`memory-max`/`memory-high`/`admit-timeout` | yes |
| `--admit-timeout` bounds admission only | `cmd/aira/main.go:1038-1053`, `ConfineRequest.AdmissionMaxWait` | yes |
| confine's kill mechanism is `scope.Kill()` = one write to `cgroup.kill` | `cgroup_linux.go:321-331`; teardown at `cleanupConfineScope` (`confine_linux.go:2246-2257`) | yes |
| the supervisor-signal witness and its `runEnded` cut-off | `confine_linux.go:845-884`, snapshot at `1266-1269` | yes |
| confine holds a verified, boot-aware leader identity | `confine_linux.go:1217-1224` — `PIDIdentity{PID, StartTick, BootID}`, aborts the launch if it cannot be established | yes |
| confine has NO ledger, NO kill intent, NO terminal CAS | grep: nothing in `confine_linux.go` touches `ledgerEvent`, `KillIntent`, `killWithIntent` | yes |
| `ConfineResult` is `{Exit int; Status ConfineStatus}` — **no `ErrorCodes` field** | `confine.go:635-638` | yes; this is load-bearing for §5.4 |
| confine DOES have `--detach`, with a durable record | `confine_detach.go`, `confine_detach_linux.go`; `ConfineDetachRecord` stores `Status *ConfineStatus` and `Exit *int` | yes — **the ticket's "no ledger" framing is right for the foreground path and needs qualifying for detach; see §6** |
| the detached supervisor runs the SAME code | `confine_detach_linux.go:702` — `result, confineErr := Confine(ctx, request)` | yes |
| shim mode branches before any cgroup work | `confine_linux.go:491` — `if deps.resolveMode() == ConfineModeShim { return confineShim(...) }` | yes |
| an existing test asserts confine REFUSES `--cpu-timeout` today | `cmd/aira/cpu_timeout_cli_test.go:37-39` | yes — this test is inverted by this ticket |
| AIRA-136's primitives | `deadline_linux.go` (`deadlineSource`, `startDeadlineSource`, `deadlineConfig`, `deadlineFire`, `readCgroupCPUUsed`, `readCgroupCPUFn`, `cpuBudgetSampleInterval`); `decisions.go:81-141` (`decideCPUBudgetExceeded`, `decideFinalCPUConsumed`, `decideCPUBudgetUnenforced`) | yes, all present and reusable |

---

## 1. Decision 1 — does `aira confine` get a deadline at all? **Yes.**

The ticket asks this as a genuine question, so it is answered with reasons
rather than assumed.

**1.1 `timeout(1)` does not cover the wall case honestly here, let alone the CPU
case.** `timeout 30m aira confine -- suite` sends SIGTERM to the *confine
supervisor*. That is caught by `forwardConfineSignals`
(`confine_linux.go:849`), recorded as the supervisor signal, and rendered as
`terminated-by=supervisor-signal:SIGTERM` — **byte-identical to an operator's
Ctrl-C**. AIRA-70 built that facet precisely so a job's end could be attributed;
wrapping in `timeout(1)` re-collapses two different causes into one verdict. The
external wrapper is not merely inelegant, it degrades an honesty facet AIRA
already ships.

**1.2 The CPU bound has no external equivalent at all.** `ulimit -t` is
`RLIMIT_CPU`: **per-process**, delivered as SIGXCPU to whichever process crossed
it, not cumulative across a job tree — the wrong quantity for a suite that forks
hundreds of processes. cgroup v2 `cpu.max` is a **bandwidth throttle** (a quota
per period), not a budget: it slows a job down forever, it never ends it. The
quantity AIRA-136 chose — cumulative `user_usec + system_usec` over the scope and
all descendants — is only expressible where confine already stands.

**1.3 confine is the mandated entry point.** `CLAUDE.md` requires every heavy
command to run under `aira confine`. AIRA-136's motivating scenario is a heavy
suite on a load-48 box. A bound available on `aira run` and absent on `aira
confine` is a bound absent exactly where the problem lives.

**1.4 confine already owns every primitive.** A cgroup scope path; a `cpu.stat`
read it already performs at teardown (`readCgroupUsage`, rendered as
`cpu=Xs+Ys`); `cgroup.kill`, which reaches every descendant including `setsid`'d
ones; a decoded wait status; a verified boot-aware leader identity; and a
trailer that is already the single operator-facing honesty projection. Nothing
new has to be invented except the arbitration itself.

**1.5 Both bounds, in one design.** Building the deadline path and wiring only
one bound into it would guarantee a second visit to the same arbitration code
for the other — the exact mistake AIRA-136's ticket warns against, in a third
location. `--timeout` and `--cpu-timeout` land together, through **one**
`deadlineSource` and **one** kill site.

---

## 2. Decision 2 — flag shape

**`--timeout DURATION` and `--cpu-timeout DURATION`, names identical to `aira
run`'s.** No divergence.

- One concept, one name. `aira run --timeout` already means "wall-clock bound on
  the job"; `aira run --cpu-timeout` already means "cumulative CPU-time bound".
- The `--admit-timeout` collision is examined and dismissed: `--admit-timeout`
  says `admit` in it, and it is today's *only* `timeout`-suffixed confine option,
  which is exactly what makes an operator reach for it expecting a job bound.
  Adding the correctly-named option removes the ambiguity rather than creating
  it. The three are disambiguated in the generated help and the SKILL.
- Accepted **only in the launch form** (before `--`), in the valued branch of
  `parseConfineArgs`, each at most once, each requiring a positive duration.
  `parseConfineManagementArgs` keeps rejecting them, so `aira confine --timeout
  5m --list` is an argument error and never a silently ignored no-op — the same
  discipline `--exclusive` already follows (`cmd/aira/main.go:781-784`).
- Validation: `time.ParseDuration`, must be `> 0`. A zero or negative value is
  `E_CONFINE_ARGUMENT_INVALID`, never "no bound" — a bound the operator asked for
  and silently did not get is a fake pass.
- **Lower bound.** `--cpu-timeout` below `cpuBudgetSampleInterval` (100ms) is
  accepted but cannot be enforced within one sample; it is not refused (a
  sub-interval budget is a legitimate, if odd, request and the overshoot is in
  the honest late direction), and the `unenforced` trailer state (§5.3) is how
  such a run reports itself. Recorded as an accepted gap in §9, matching
  AIRA-136 §9.

---

## 3. Decision 3 — where the deadline is refused

**3.1 ci-shim mode (`ConfineModeShim`) refuses BOTH bounds, fail-closed.**
`E_CONFINE_ARGUMENT_INVALID`, raised in `confineShim` at the point mode is
already known. Reasons, both structural:
- There is no cgroup, so there is no `cpu.stat` — the CPU bound is not merely
  hard there, it is **unmeasurable**. `readCgroupCPUUsed` would return
  unevaluated forever and the sampler would never fire, i.e. a silently disabled
  bound.
- There is no `cgroup.kill`. Shim mode's reach is `kill(-pgid, …)`, which
  `TestShimConfineSignalDoesNotReachASetsidDescendant` already pins as unable to
  reach a `setsid`'d descendant. A wall bound there could not honour its own
  promise on the exact job shapes (test runners, container CLIs) that need it.

A bound AIRA cannot honour must be refused, not degraded. Filed as a named
deferral (§9) rather than left implicit.

**3.2 `--detach` is NOT refused.** This diverges from AIRA-136, which refused
`--cpu-timeout --detach` for `aira run`, and the divergence is justified by
source, not by preference: run's detached path had its **own** timeout branch in
`detach_linux.go` lacking AIRA-126's arbitration (that was AIRA-131's whole
subject). Detached **confine** has no second branch — `SuperviseConfineDetached`
calls `Confine(ctx, request)` (`confine_detach_linux.go:702`), which is
`confineWithDeps`, which is the single arbitration site this plan builds. The
detached case is therefore *strictly better served* than the foreground one: the
outcome additionally lands in a durable `ConfineDetachRecord` carrying `Exit` and
the whole `ConfineStatus` (hence the new facets). This must be **verified at
implement time by a test that drives the detached path end to end** (§7, T9),
not asserted from this reading.

---

## 4. Decision 4 — primitives reused, verbatim

**4.1 Reused with no change whatsoever** (the reviewer can diff and confirm zero
edits):

| Primitive | File | Used for |
| --- | --- | --- |
| `startDeadlineSource` / `deadlineSource` / `deadlineConfig` / `halt()` | `deadline_linux.go` | the ONE multiplexed source; confine gets exactly one deadline branch and one kill site, for the same reason run does |
| `cpuBudgetSampleInterval` (100ms) | `deadline_linux.go:40` | unchanged; its overshoot argument applies identically |
| `readCgroupCPUUsed` / `readCgroupCPUFn` | `deadline_linux.go:176-193` | the baseline read and the sampler |
| `decideCPUBudgetExceeded` | `decisions.go:86` | inside the source; not called by confine directly |
| `decideFinalCPUConsumed` | `decisions.go:101` | confine's final consumed total, from `usage.CPUUser+usage.CPUSys` and the baseline |
| `decideCPUBudgetUnenforced` | `decisions.go:133` | the `unenforced` trailer state |
| `processLive` / `processLiveness` | `runner_linux.go:2065` | leader liveness at the instant the kill found nothing |
| `waitConfineCommand` | `confine_linux.go:2066` | **unchanged**; it is shared with the shim path and is merely moved behind a goroutine at the call site |

`internal/runner/decisions.go` and `internal/runner/deadline_linux.go` are
touched **only** by the one additive change in 4.2 and the one new rule in 5.2.

**4.2 The one additive change to a freshly-gated file.** `deadlineFire` gains a
typed discriminator:

```go
type deadlineKind uint8
const (
    deadlineKindUnset deadlineKind = iota
    deadlineKindWall
    deadlineKindCPU
)
// on deadlineFire:
Kind deadlineKind
```

set alongside the existing `Actor`/`Code` at both send sites. `Launch` reads
neither and is behaviourally untouched (asserted by a test, §7 T10).

Why this rather than the alternatives:
- **String-matching `fired.Code == "E_RUN_CPU_TIMEOUT"` from confine** couples
  confine's trailer to a *run*-flavoured error code it never emits, and is a
  latent break the compiler cannot see.
- **Discriminating on `Observed > 0`** is refused outright: `deadlineFire`'s own
  doc says `Observed` is "zero for the wall bound", and inferring a fact from a
  zero-valued field is exactly the fake-evidence pattern this package refuses
  everywhere else (`LocalOOM`, `PeakRSS`, `CPUUser`). The reproduction test
  `TestAIRA138DeadlineSourcePrimitivesAreReusableFromConfine` pins this
  reasoning in code.
- **Parameterising `deadlineConfig` with four label strings** is more surface for
  the same result.

Additive, three lines, zero behaviour change, compiler-checked.

---

## 5. Decision 5 — the kill/terminal arbitration for a supervisor with no run ledger

This is the ticket's hard, novel part, and the section a plan gate should read
hardest.

### 5.1 What AIRA-126 was actually protecting, split into its two halves

AIRA-126's failure had two distinct halves, and they do **not** transplant
together:

- **(a) A durable artifact asserting a kill that never happened.**
  `killWithIntent` writes `KillIntent{Present:true}` to the ledger *before*
  touching the scope; `killScope` then refuses to claim a win on an empty scope;
  the terminal CAS sees `Present && !Completed` and emits a **non-terminal**
  `U_RUN_RECONCILE_REQUIRED` record.
- **(b) The child's real exit evidence being discarded**, sitting unread in
  `waitCh` while a fabricated termination is reported.

**Half (a) does not exist in foreground confine.** There is no ledger, no
pre-kill publication, and therefore no artifact that can be left asserting an
unfinished kill. The specific AIRA-126 *record* pathology is structurally absent,
and this plan does **not** transplant `decideTimeoutIntentNotExecuted`, its
`IntentPublished`/`IntentCreated` conjuncts, or `decideNotExecutedDisposition`.
Passing `true, true` for ledger conjuncts confine has no referent for would be a
lie wearing the costume of reuse.

**Half (b) transplants exactly, and is the real hazard.** confine's durable
evidence is its **exit code and its trailer** — that is the answer to the
ticket's "what plays AIRA-126's role". Those two artifacts are just as capable of
asserting a termination that did not occur, and the operator has *less* recourse,
because `ConfineResult` has no `ErrorCodes` field (§0) and no reconcile pass to
correct the record later. **A confine fabrication is final on the instant it is
printed.**

`internal/runner/confine_deadline_danger_linux_test.go` proves this is real, not
theoretical: the naive first draft reports `exit 137` and
`terminated-by=deadline:…` for a child that exited `7` normally, having
signalled nothing, **while every input needed to refuse was already available**
(scope empty by two reads, `cgroup.kill` written against nothing,
`processLive == processDead`, the real outcome pending in the wait channel). It
reproduces 5/5 deterministically.

### 5.2 The rule

The whole arbitration is **in-memory and happens before any output**, because
nothing confine produces is durable until the trailer is written and the exit
code returned. That is strictly *simpler* than AIRA-126's problem and must not be
inflated into a ledger confine deliberately does not have
(`[[architectural-simplicity]]`).

**New: `killConfineScope(ctx, scope) (confineKillResult, error)`**, in
`confine_linux.go`, following `killScope`'s refusal discipline exactly
(`runner_linux.go:2244-2267`) but with confine's own semantics:

```go
type confineKillResult struct {
    Empty     bool // the scope was verified empty by TWO independent reads
    Started   bool // cgroup.kill was written and the write returned nil
    Completed bool // waitEmpty then CONFIRMED the scope empty
}

func killConfineScope(ctx context.Context, scope Scope) (confineKillResult, error) {
    pids, err := scope.Members()
    if err != nil {
        return confineKillResult{}, err            // unevaluated, never "empty"
    }
    if len(pids) == 0 {
        empty, emptyErr := scope.Empty()
        return confineKillResult{Empty: empty && emptyErr == nil}, emptyErr
        // returns BEFORE any write: no signal was emitted, provably.
    }
    if err := scope.Kill(); err != nil {
        return confineKillResult{}, err            // the write failed: unevaluated
    }
    if err := waitEmpty(ctx, scope, confineDeadlineKillGrace); err != nil {
        return confineKillResult{Started: true}, err
    }
    return confineKillResult{Started: true, Completed: true, Empty: true}, nil
}
```

**No SIGTERM grace, deliberately.** `killScope` does `Terminate` → grace →
`Kill`; confine's own teardown (`cleanupConfineScope`) goes straight to
`scope.Kill()`, and `CLAUDE.md` documents confine's contract as "**A confined job
has NO graceful shutdown** — Ctrl-C / SIGTERM hard-kills the whole job tree
instantly." A deadline kill that behaved *more gently* than a Ctrl-C would
contradict the documented contract and would add a third outcome shape (a child
that catches SIGTERM and exits 0 during the grace), doubling the arbitration
surface for no honesty gain. Keep the primitive; the graceful variant is a named
deferral (§9).

**New pure rule, sited next to AIRA-126's in `decisions.go`:**

```go
// decideConfineDeadlineNotExecuted is AIRA-126's IDEA in a supervisor with no
// ledger. It keeps AIRA-126's two EVIDENCE conjuncts verbatim and drops its two
// LEDGER conjuncts, which have no referent here — confine publishes nothing
// before it kills, so there is no intent to be foreign, and none to have been
// concurrently completed. Dropping them is sound BECAUSE the artifact they
// guard does not exist; it is not a relaxation of the evidence bar.
//
//   - killErr == nil        : an errored kill is unevaluated, never dismissed.
//   - Empty && !Started     : the only killConfineScope shape that proves no
//                             signal was emitted (it returned before the
//                             cgroup.kill write) AND that the scope was verified
//                             empty by two independent reads.
//   - leader == processDead : kernel proof the leader was already gone at the
//                             instant the kill found nothing to signal.
//                             processAlive and processUnknown both refuse.
//
// It never reports true for an empty scope alone: emptiness is not proof that
// any kill won, which is exactly the inference killScope refuses.
func decideConfineDeadlineNotExecuted(killErr error, attempt confineKillResult, leader processLiveness) bool {
    return killErr == nil && attempt.Empty && !attempt.Started && leader == processDead
}
```

### 5.3 The three arms, and what each reports

The wait moves behind a channel (`waitConfineCommand` itself unchanged):

```go
waitCh := make(chan confineWaitOutcome, 1)
go func() { e, t := waitConfineCommand(cmd); waitCh <- confineWaitOutcome{e, t} }()
deadlines := startDeadlineSource(deadlineConfig{
    Wall: request.Timeout, CPU: request.CPUTimeout,
    CPUBase: cpuBaseline, CPUBaseOK: cpuBaselineOK,
    ScopePath: scope.Reference(), Interval: cpuBudgetSampleInterval,
    ReadCPU: readCgroupCPUFn,
})
```

exactly as `Launch` does, including `deadlines == nil` keeping today's bare
receive so a confine with no bound is byte-for-byte unchanged, and
`deadlines.halt()` after the select so no sampler outlives the launch.

| Arm | Condition | Exit reported | `terminated-by` | trailer bound-state |
| --- | --- | --- | --- | --- |
| **A — killed** | fire; `Started` (the `cgroup.kill` write returned nil) | the child's real wait-derived code (`137`) | `deadline:wall` / `deadline:cpu` when the classifier would otherwise say `unattributed-sigkill` (§5.5); otherwise the classifier's own, higher-priority verdict | `fired-killed` (also `Completed`) or `fired-kill-unconfirmed` |
| **B — not executed** | fire; `decideConfineDeadlineNotExecuted` true | **the child's own real exit code**, drained | the classifier's honest verdict on the real wait status (typically `normal`) | `fired-not-executed` |
| **C — unevaluated** | anything else: kill errored, `Members()` errored, leader `processAlive`/`processUnknown`, `Empty` false with no successful write | the child's real wait-derived code | the classifier's own verdict on real evidence | `fired-unevaluated` |

**Arm B is the AIRA-126 answer, and it is a drain-then-report, exactly as the
ticket anticipated — with no ledger write at the end.** The drain is bounded by
the same anti-hang reasoning AIRA-131 used: a dead leader's `cmd.Wait()` is
already blocked in `wait4` on our own child, so only the reap and one scheduler
wakeup remain. On expiry the arm degrades to **C**, never to A: an expired bound
can only produce an honest "unevaluated", never a wrong kill claim.

Arms A and C **also** drain the wait, unconditionally and without a bound: the
child is this process's own child and `cmd.Wait()` is the only way to reap it.
Not draining would leak a zombie and lose the exit code — the shape AIRA-131
explicitly called out as *stricter than the foreground one was*, because it
returned without even draining.

**AIRA-126's own deviation applies verbatim to arm B and is restated here so a
reviewer sees it was not overlooked:** no deadline is asserted in
`terminated-by`, and no timeout error is raised, because *nothing was
terminated*. The fired-but-ineffective deadline is still recorded — on the
`timeout=`/`cpu-timeout=` field as `fired-not-executed`, which is confine's
analogue of AIRA-126's durable `kill-intent` event plus disposition. The reader
of an `exit 0` trailer carrying `cpu-timeout=10m:fired-not-executed` should
understand that **the deadline did fire and killed nothing**.

### 5.4 The exit code: a pass-through, not an error. (The most likely gate objection.)

**Decision: a deadline never causes `Confine` to return an error.** It sets
facets and returns `result.Exit` from the real wait status.

Reasons:
1. **confine's exit code is a pass-through of the job's, and that is a hard
   contract** every Makefile on this box depends on. `runConfineCommand` returns
   `result.Exit` on success and `codes.ExitForCode(...)` on error
   (`cmd/aira/main.go:1078-1083`), so returning an error *replaces* the job's
   exit code.
2. **confine reserves errors for "the confinement could not be established"**
   (`confineUnavailable`). A job that was successfully confined and then hit its
   bound is not a confinement failure; collapsing the two into one channel loses
   a real distinction.
3. `codes.ExitForCode` for a new `E_CONFINE_*_TIMEOUT` would land on **3**, which
   collides with `waitConfineCommand`'s own undecoded-status `3`
   (`confine_linux.go:2080`) — a genuinely ambiguous number.
4. `137` is already the true, conventional answer for a SIGKILLed job, and the
   ambiguity with an OOM kill is resolved on the **always-rendered**
   `terminated-by` facet, where `oom` already outranks everything (§5.5).

**This is a deliberate divergence from AIRA-136**, which gave `aira run`
`E_RUN_CPU_TIMEOUT` at exit 3 — but run's exit code is the *record's*, not a
pass-through, and run has an `ErrorCodes` array to carry the code in.
`ConfineResult` has neither (§0). Consequence, stated plainly: **the trailer
field of §5.6 is the only machine-readable carrier of "a bound fired", so it is
load-bearing, not decorative** — which is precisely why the ticket's fourth
question is answered yes.

The alternative (raise `E_CONFINE_TIMEOUT` / `E_CONFINE_CPU_TIMEOUT` and mint
exit codes) was considered and is recorded here so the gate can overrule rather
than rediscover it. If the gate prefers it, the change is confined to §5.4 and
`internal/codes/codes.go`; nothing in §5.2–5.3 moves.

### 5.5 `classifyConfineTermination`: one new step, at position 7

The classifier's documented order (`confine_linux.go:2090-2096`) is load-bearing
and is changed **minimally**: steps 1–6 are untouched, so every existing row of
`TestClassifyConfineTermination` stays byte-identical. The new branch takes over
exactly the bucket that today says "a SIGKILL this supervisor cannot attribute":

```
 1. no decoded wait status                       -> unevaluated      (unchanged)
 2. SIGKILLed and LOCAL oom_kill readable and >0  -> oom              (unchanged)
 3. this supervisor was signalled                 -> supervisor-signal:<NAME> (unchanged)
 4. not signalled                                 -> normal           (unchanged)
 5. signal is not SIGKILL                         -> child-signal:<NAME> (unchanged)
 6. local oom_kill not readable                   -> unevaluated      (unchanged)
 7. NEW: a deadline kill was STARTED              -> deadline:wall | deadline:cpu
 8. SIGKILL with local oom_kill == 0              -> unattributed-sigkill (was 7)
```

The placement is **proved from the existing code, not chosen by taste**:
`formatConfineTerminationAdvisory` (`confine_linux.go:2209-2212`) tells the
operator, for the `unattributed-sigkill` verdict, that the supervisor "sent no
signal itself". **That sentence becomes false the instant a deadline kill wrote
`cgroup.kill`.** So leaving a deadline kill in step 8 would make a shipped
advisory lie. Step 7 is where it must go.

The gate on step 7 is **`Started`, not `Started && Completed`**, for the same
reason: `Started` means the write returned nil, i.e. SIGKILL *was* delivered to
every member, so the advisory's claim is already false. Whether `waitEmpty` then
confirmed emptiness is a separate fact and is reported separately, on the
trailer's `fired-kill-unconfirmed` state.

**Why the deadline sits BELOW supervisor-signal (step 3), not above.** An
operator's Ctrl-C independently tears the scope down (the handler calls
`cleanup()` → `scope.Kill()`), so a Ctrl-C racing a deadline makes both true and
the true cause is not decidable. AIRA-70 exists so an operator's signal is never
invisible; ordering the deadline above would re-hide it. **The conflict dissolves
because the deadline gets its own always-rendered-when-requested field (§5.6):
nothing is lost by letting `terminated-by` prefer the operator's signal.** That
field is what makes the minimal classifier change sufficient.

**Why `oom` (step 2) stays above the deadline.** `cgroup.kill` never increments
`oom_kill`, so a positive LOCAL counter is never our doing — the same argument
that already puts step 2 above step 3. A job that genuinely OOMed at its cap
while the deadline was firing really was OOM-killed, and `oom` is the actionable
verdict.

**The graceful middle case falls out for free, correctly.** If the fire finds
members, we write `cgroup.kill`, but the child had *already* exited between
`Members()` and the write, the wait status is a clean `exit 0` → step 4 →
`normal`. That is the honest answer (the child exited by itself; the kill
signalled a scope it had left), and `cpu-timeout=…:fired-killed` still records
that AIRA acted. No extra machinery.

### 5.6 The trailer: two new fields, present only when the bound was requested

Answering the ticket's fourth question: **yes, and it is load-bearing (§5.4).**

Two independent fields on `ConfineStatus`, rendered by `FormatConfineStatus`,
each present **only if that bound was requested** — the discipline `Exclusive`
already follows ("Empty for a run that never asked"). Every existing trailer
stays byte-identical, which is itself asserted (§7 T11). The
"silence-is-indistinguishable" argument that drives the always-rendered facets
does not apply: there is no fabrication risk in omitting a field for a bound
nobody asked for.

```
timeout=30m:not-reached
cpu-timeout=10m:fired-killed
```

Two keys rather than one shared `deadline=` key, so that requesting both bounds
does not produce duplicate keys, and so the field name **is** the flag name — an
operator reading the trailer knows which flag to change.

Closed state vocabulary (a value AIRA cannot establish renders `unevaluated`,
never a guess):

| State | Meaning |
| --- | --- |
| `not-reached` | requested; the job ended first, and (CPU only) the final established total is under budget — a two-sided proof the bound held |
| `fired-killed` | fired; the kill was written and `waitEmpty` confirmed the scope empty |
| `fired-kill-unconfirmed` | fired; the write succeeded but emptiness was not confirmed |
| `fired-not-executed` | the §5.2 arm: provably no signal delivered, leader proved dead, the child's own exit is reported |
| `fired-unevaluated` | fired; AIRA cannot establish what its kill did |
| `unenforced` | **CPU only.** `decideCPUBudgetUnenforced` is true: never measured, or measured-breached with no executed kill |

**The wall bound has no `unenforced` state, deliberately.** A wall timer either
fired or the job ended first; there is no measurement that can be unavailable.
The asymmetry is stated rather than papered over with a vacuous value.

**`decideCPUBudgetUnenforced` is reused verbatim, with a stricter
`killedByCPUBudget` than run's.** confine passes `fired ∧ Kind==CPU ∧
attempt.Started` — "a CPU-budget kill *executed*". AIRA-136's Fable build review
recorded, as a non-blocking nit, that run's `killedByCPUBudget` is true for a CPU
fire whose kill did **not** complete, which "overclaims killed". confine gets it
right from the start; the divergence is named here so a reviewer does not read it
as drift. Aligning `aira run` is a named follow-up (§9), not a rider.

### 5.7 The CPU baseline: read after the handshake, before the release

`request.CPUTimeout > 0` ⇒ `cpuBaseline, cpuBaselineOK =
readCgroupCPUFn(scope.Reference())`, placed **after** the membership and
identity verification (`confine_linux.go:1210-1224`) and **before** the release
write (`:1244`).

Not earlier, and the reason is the one invariant AIRA-136 established — *every
error is in the late direction, never the early one*. confine re-execs itself as
`aira confine-setup` inside the scope; that shim parses args, verifies its
cgroup, applies `oom_score_adj`/nice/ionice and writes the handshake, all
**inside the scope and therefore charged to `cpu.stat`**. A baseline read before
`cmd.Start()` would charge the shim's setup cost to the job's budget, making the
bound fire **early** — the wrong direction. Reading after the handshake, while
the shim is blocked on the release pipe, excludes it.

**The residual is stated rather than claimed absent:** the shim's post-release
path — one `read(2)` return and an `execve` — is still charged. That is
microseconds against a budget measured in minutes, and it is the one remaining
early-direction term in the design.

`decideFinalCPUConsumed(total, cpuBaseline, cpuBaselineOK)` computes the final
consumed total from the teardown `usage.CPUUser + usage.CPUSys`, which confine
already reads at `confine_linux.go:1270-1281`. `finalEstablished` is read off the
**pointer fields**, never off a sum, for the reason AIRA-136 states at
`runner_linux.go:883-891`: summing first turns an unreadable counter into a
measured zero.

---

## 6. The ticket's "no run ledger" framing, qualified

The ticket says confine has "deliberately NO run ledger". That is exactly right
for the **foreground** path and needs one qualification a plan gate should have
in front of it: the **detached** path writes a durable `ConfineDetachRecord`
(`confine_detach.go`) that stores `Exit *int` and the whole `Status
*ConfineStatus`. So detached confine *does* have durable evidence — and it is
`ConfineStatus`, the very struct the new facets live on.

This **strengthens** the design rather than complicating it:
- The arbitration is unchanged, because both paths run the same
  `confineWithDeps` (§3.2).
- The new facets reach the durable record for free, via the existing `Status`
  field, so a detached job killed at its CPU budget is auditable long after its
  session ends.
- `classifyConfineDetachRecord`'s existing honesty gate — `Terminal:true` with
  neither an exit code nor an error code is `outcome-unknown` — is untouched,
  because arm A/B/C all set `Exit`.

No new durable field, no schema bump, no migration. (`[[aira-not-live-no-compat]]`
would permit a bump; none is needed.)

---

## 7. Tests

Package `internal/runner` unless stated. Every one names its ticket in a
`verifies:` comment. The hermetic seam is AIRA-126's and is reused rather than
rebuilt: `livenessScope` / `livenessBackend` / `gatedStdin` / `aira126Scale`
(`timeout_arbitration_linux_test.go`), `scriptedCPUReader` /
`swapCPUReader` (`deadline_linux_test.go`, `cpu_budget_arbitration_linux_test.go`).
`confineDeps` already exposes `newBackend`, `start`, `readHandshake`,
`writeOOMGroup`, `writeScopeSwapCap`, `writeScopeMemoryCap`, `readUsage` and
`signalSource`, so a fully hermetic confine harness needs no new production seam.

**Determinism over soak, on measured grounds.** AIRA-136's gate review recorded
that its 800-iteration real-cgroup soak reached the arbitrated arm **0 times** on
an idle box. This plan does not repeat that: the arbitrated arm is covered by a
deterministic hermetic pair driving the real `confineWithDeps` with a real child,
real `wait4` evidence and real kernel liveness, and the real-cgroup lane covers
the killed arm, where a soak is not needed.

| # | Test | What it pins | Goes red against |
| --- | --- | --- | --- |
| T1 | `…CPUBudgetKillsASpinningConfinedJob` (real cgroup) | arm A end to end: `terminated-by=deadline:cpu`, `cpu-timeout=…:fired-killed`, and the trailer's own `cpu=` counters ≥ the budget | the CPU wiring neutered |
| T2 | `…WallTimeoutKillsButCPUBudgetDoesNot` (real cgroup) | the FEATURE's point: one argv, two runs — a 0.5s sleep dies at `--timeout 100ms` and survives `--cpu-timeout 100ms` | a CPU bound implemented on wall-clock |
| T3 | `…DeadlineAgainstAlreadyExitedChildReportsTheRealExit` (hermetic) | **arm B**: exit 7 survives, `terminated-by=normal`, `cpu-timeout=…:fired-not-executed`, `scope.signalled()` false. The committed reproduction, inverted (§8) | the naive draft; any missing conjunct |
| T4 | `…DeadlineWithLiveLeaderStillKills` (hermetic) | the over-widening guard: empty scope but leader `processAlive` ⇒ arm C/A, never B. Stops "an empty scope means the job finished" | `processDead` forced at the call site |
| T5 | `…DeadlineWithUnknownLeaderStaysUnevaluated` (hermetic) | `processUnknown` refuses arm B | a `!= processAlive` guard instead of `== processDead` |
| T6 | `…DeadlineDrainBoundExpiryDegradesToUnevaluated` (hermetic) | the bounded drain expires ⇒ arm C, never a fabricated kill | an unbounded or unguarded drain |
| T7 | `TestAIRA138ConfineDeadlineNotExecutedRule` (pure) | `decideConfineDeadlineNotExecuted` in both directions, one row per conjunct | any conjunct dropped |
| T8 | `TestAIRA138ClassifyConfineTerminationDeadlineOrder` (pure) | the new step 7 in both directions, **and** that `oom` and `supervisor-signal` still outrank it | the branch inserted above step 2 or 3 |
| T9 | `…DetachedConfineHonoursTheCPUBudget` (real cgroup, detached) | §3.2 verified, not assumed: the durable record carries `Exit` and the new facets | a deadline wired only into the foreground call site |
| T10 | `TestAIRA138DeadlineFireKindDoesNotChangeRun` (pure) | `deadlineFire.Kind` is additive: both send sites still set the same `Actor`/`Code` `Launch` reads | any behavioural edit to `deadline_linux.go` |
| T11 | `TestAIRA138TrailerUnchangedWithoutABound` | a confine with no bound renders a byte-identical trailer | an always-rendered `timeout=` field |
| T12 | `…ShimModeRefusesBothBounds` | §3.1 fail-closed refusal | a bound silently ignored in shim mode |
| T13 | `cmd/aira`: `parseConfineArgs` accepts each bound once, requires a value, rejects non-positive, and `parseConfineManagementArgs` still refuses both | the flag surface | a valueless or repeatable option |
| T14 | `internal/core` + `cmd/aira` MCP/CLI parity | AIRA-136's build found `cmd/aira/mcp.go` hand-maintains a default per argument; confine's own faces must be checked the same way | a face that silently drops the bound |

**Mutation (executed reverts, run not read), recorded in the ticket's Evidence
section at build time:**
1. Force `decideConfineDeadlineNotExecuted` to `false` ⇒ T3 must fail with the
   reproduction's exact signature (`exit 137`, a deadline attribution).
2. Force it to `true` ⇒ T4, T5 must fail.
3. Move step 7 above step 3 in the classifier ⇒ T8 must fail.
4. Neuter the CPU wiring (never sample) ⇒ T1, T2, T9 must fail.
5. Replace `killConfineScope`'s two-read empty check with `len(pids)==0` alone
   ⇒ a test must go red, or the guard is porous and the plan is wrong.

---

## 8. How the committed reproduction is inverted, not deleted

`internal/runner/confine_deadline_danger_linux_test.go` ships with this plan and
passes 5/5 today. At implement time:

- `TestAIRA138ConfineHasNoJobDeadlineToday` is **inverted** into a positive
  assertion that `ConfineRequest.Timeout` / `.CPUTimeout` and the two
  `ConfineStatus` facets exist and are correctly typed. Its failure message
  already instructs this.
- `TestAIRA138NaiveConfineDeadlineFabricatesAKill` becomes **T3**: the same
  hermetic construction, driven through the real `confineWithDeps` instead of
  the local `naiveConfineDeadlineDraft`, asserting `exit 7` / `normal` /
  `fired-not-executed`. `naiveConfineDeadlineDraft` is **kept** in the file as
  the mutation target for revert #1 — it is the executed proof that T3 is not
  porous.
- `TestAIRA138DeadlineSourcePrimitivesAreReusableFromConfine` is kept as-is and
  extended to assert `fired.Kind == deadlineKindCPU` (§4.2).

---

## 9. Deferrals and accepted coverage gaps, filed not silent

1. **ci-shim mode gets no bounds** (§3.1). Fail-closed refusal, not a silent
   no-op. A wall bound there is buildable on `kill(-pgid, …)` with the documented
   `setsid` escape; a CPU bound is not buildable at all without a cgroup. File as
   a follow-up ticket at merge.
2. **No graceful SIGTERM grace before the deadline's SIGKILL** (§5.2). Justified
   by `CLAUDE.md`'s documented confine contract; a `--kill-grace` option is a
   separate, additive feature.
3. **`aira run`'s `killedByCPUBudget` still overclaims** (§5.6) — AIRA-136's own
   recorded nit. confine gets it right; aligning run is a follow-up, not a rider
   on this ticket.
4. **Sampling overshoot** — inherited verbatim from AIRA-136 §9: up to
   `interval × achieved parallelism` of CPU-time past the budget, always in the
   late direction.
5. **Sub-100ms `--cpu-timeout`** cannot be enforced within one sample (§2); such
   a run reports `unenforced` rather than a fake pass.
6. **Baseline residual** (§5.7): the setup shim's post-release `read` + `execve`
   is charged to the job — microseconds, and the one early-direction term.
7. **A Ctrl-C racing a deadline** resolves to `supervisor-signal:` (§5.5). The
   true cause is not decidable; the `timeout=`/`cpu-timeout=` field still records
   that the bound fired, so nothing is lost, but the single `terminated-by`
   verdict prefers the operator.
8. **Contention itself is not reproduced in tests** — inherited from AIRA-136 §9.
   T2 proves the *quantity* is CPU-time, not that a contended box behaves as
   claimed.
9. **The irreducible signal window** at `confine_linux.go:1255-1265` (a signal
   between the child's exit and the snapshot's `Lock`) is unchanged by this
   ticket and remains AIRA-70's recorded deferral.

---

## 10. Files touched at implement time

| File | Change |
| --- | --- |
| `internal/runner/confine.go` | `ConfineRequest.Timeout` / `.CPUTimeout`; `ConfineStatus` two facets + their vocabulary; `FormatConfineStatus` renders them when requested |
| `internal/runner/confine_linux.go` | the wait behind a channel; the one deadline branch and one kill site; `killConfineScope`; the CPU baseline read; step 7 in `classifyConfineTermination`; a `formatConfineDeadlineAdvisory` |
| `internal/runner/decisions.go` | `decideConfineDeadlineNotExecuted` (new, sited beside AIRA-126's) |
| `internal/runner/deadline_linux.go` | `deadlineKind` + `deadlineFire.Kind` — additive only (§4.2) |
| `cmd/aira/main.go` | `parseConfineArgs` accepts and validates both options; `runConfineCommand` transcribes them |
| `cmd/aira/mcp.go` + skill/help surfaces | per AIRA-136's build finding: hand-maintained per-argument defaults must be checked, not assumed generated |
| `cmd/aira/cpu_timeout_cli_test.go:37-39` | the assertion that confine refuses `--cpu-timeout` is **inverted** |
| tests | §7 |

Explicitly **not** touched: `waitConfineCommand`, `killScope`, `killWithIntent`,
`mergeEvidence`, the run terminal CAS, `decideTimeoutIntentNotExecuted`,
`decideNotExecutedDisposition`, `ConfineDetachSchema`.

---

## 11. Gate questions this plan expects to be pressed on

1. **§5.4** — exit-code pass-through versus a new `E_CONFINE_*_TIMEOUT`. The
   deliberate divergence from AIRA-136. Both readings are stated; the gate may
   overrule, and the change is contained.
2. **§5.5** — the classifier's step-7 placement, and specifically letting
   `supervisor-signal` outrank the deadline.
3. **§4.2** — whether `deadlineFire.Kind` is warranted at all, given
   `[[architectural-simplicity]]`, or whether confine should string-match the run
   code.
4. **§3.2** — that detached confine needs no separate arbitration. Asserted from
   `confine_detach_linux.go:702` and to be **verified by T9**, not by reading.
5. **§5.2** — that dropping AIRA-126's two ledger conjuncts is sound because the
   artifact they guard does not exist, and is not a quiet relaxation of the
   evidence bar.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
