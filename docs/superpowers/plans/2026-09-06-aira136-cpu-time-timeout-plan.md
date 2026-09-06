# AIRA-136 — a job deadline expressed in cumulative CPU-time, not wall clock

Status: **built**. Branch `aira136-cpu-time-timeout` off `origin/master` at
`3f4f54a`. Two-loop work per CLAUDE.md: this touches the kill/terminal-CAS
arbitration AIRA-126 landed hours ago, so this document is the plan artifact for
plan-review and the plan gate.

The plan gate returned **GATE-PASS-WITH-CONDITIONS** (C1–C8). Every condition is
applied in the implementation and folded into the body of this document; §0
below records where each one landed and what changed from the gated draft, so a
reviewer can check the conditions against the code rather than against a promise.

---

## 0. Gate conditions, and where each is discharged

| # | Condition | Where it landed |
|---|---|---|
| C1 | `decideCPUBudgetUnevaluated` must not short-circuit on `fired`; only an EXECUTED CPU kill suppresses the rule | §4.5 rewritten: the input is `killedByCPUBudget = timedOut && fired.Code == "E_RUN_CPU_TIMEOUT"`. Pinned by `TestAIRA136CPUBudgetUnenforcedRule`, `TestAIRA136WallKillDoesNotSuppressTheUnenforcedCode`, the §7.5 arbitrated-exited test, and the §7.4 both-codes row. §8 narrowed accordingly. |
| C2 | The code covers two states; `UNEVALUATED` misdescribes the second | Renamed **`U_RUN_CPU_BUDGET_UNENFORCED`**, with both states stated precisely in the `codes.go` entry and in spec §6.4. |
| C3 | Define `finalConsumed` when the pre-start baseline read failed | `decideFinalCPUConsumed` returns the absolute counter — an upper bound, erring only toward reporting the code. Pinned by `TestAIRA136FinalCPUConsumedUsesTheAbsoluteCounterWithoutABaseline`. §4.3/§4.5. |
| C4 | The cumulative-scope test compared two runs' wall durations: porous | Replaced by a single-run assertion, §7.4. |
| C5 | Surface the `aira confine` deferral to the owner in this PR | Follow-up ticket filed with `aira create` in this PR, `relates` AIRA-136; recorded in the AIRA-136 resolution and named in the PR body. §3.2. |
| C6 | Pin the both-codes record shape | §4.5/§9(5) state it; `TestAIRA136RealCgroupWallKillOverTheCPUBudgetCarriesBothCodes` pins it against a real cgroup. |
| C7 | The arbitration tests' injected reader must gate on the child's real exit, not on a wall-clock coincidence | `overBudgetAfterLeaderExit` gates on the harness observing leader death through `readProcStatFn`; the non-vacuity counter is kept; no test that swaps `readCgroupCPUFn` is `t.Parallel`. §7.5. |
| C8 | Record the executed reverts | §7.6, with exit codes and named failing tests, repeated in the PR body. |

Every line reference below is `origin/master` at `3f4f54a`, and every one of
them was re-read against the tree rather than carried over: `3f4f54a` adds only
`.aira/tickets/AIRA-136.md` on top of `7a43050`, so no line moved. §6.1 records
the greps that establish the *completeness* of the change set, which is the
claim a line reference alone cannot support.

---

## 1. What is being added, and what problem it actually solves

`Request.Timeout` (types.go:175) is wall clock. On a contended box a job loses
wall-clock time to *other people's* jobs, so a wall-clock deadline fires for a
reason a deadline is not supposed to catch — starvation, not the job's own
misbehaviour. A deadline expressed in **cumulative CPU-time consumed by the
job's cgroup** counts only time the job actually spent on a CPU, so contention
makes a starved job take longer in wall-clock terms to reach the *same* budget
and only a job genuinely burning that much CPU is killed.

The quantity already exists and is already correct: cgroup v2's `cpu.stat`
`user_usec` + `system_usec`, hierarchical over the scope and all its
descendants, parsed today by `parseCPUStat` (usage_linux.go:208) via
`readCgroupUsage` (usage_linux.go:177) and displayed in every confine trailer's
`cpu=Xs+Ys` and in the run record's `cpu_user`/`cpu_sys` (types.go:143-144).
It is read exactly once today, at teardown. The new work is reading it
**periodically while the job runs** and feeding the result into the deadline
decision Launch already has.

---

## 2. Flag shape — a new orthogonal `--cpu-timeout`, not a mode on `--timeout`

**Decision: a new flag, `--cpu-timeout DURATION`, alongside the existing
`--timeout DURATION`. Both may be given. Both may be omitted. Whichever fires
first wins, through the same kill path, with a distinguishing actor and error
code.**

The ticket suspected this was right; it is, and for one reason that is stronger
than discoverability:

**A CPU-time budget cannot catch a hang.** A deadlocked process, a process
blocked forever on a socket, a process waiting on a lock nobody releases — all
consume *zero* CPU-time and will never reach any CPU budget, however small. That
is the single most classic thing a timeout exists to catch. A mode flag that
changed what `--timeout` measures would therefore silently *remove* the ability
to bound a hang from anyone who set the mode, in exchange for the ability to
bound a spin. These are complementary bounds on genuinely different failures and
an operator frequently wants both at once:

```
aira run --timeout 30m --cpu-timeout 10m -- ./suite
#          ^ catch a hang            ^ catch a spin/runaway, contention-immune
```

Supporting arguments, in descending weight:

- **Record legibility.** With a mode flag the meaning of a recorded deadline
  would depend on a second field; a run record read a week later would be
  ambiguous about what bound was actually applied. Two flags means the error
  code alone (`E_RUN_TIMEOUT` vs `E_RUN_CPU_TIMEOUT`) says which bound fired.
- **No change of meaning for the existing flag.** AIRA is pre-release, so
  compatibility costs nothing ([[aira-not-live-no-compat]]), but an operator
  asking for a CPU bound should not have to remember that `--timeout` secretly
  means something else now. Silent meaning changes are exactly what AIRA's
  honesty discipline is against.
- **`--timeout`'s existing semantics stay byte-for-byte.** Its timer is created
  at the same instant, its `time.NewTimer` is unchanged, and every existing
  timeout test continues to exercise the identical path.

Rejected alternative — `--timeout-basis wall|cpu`: fails the hang argument above,
and creates a two-field deadline whose record is ambiguous.

Rejected alternative — a single `--timeout` that fires on *either* — same
problem, plus the operator cannot size the two bounds independently, which is
the whole point (a suite's honest CPU budget and its honest wall budget differ
by the machine's parallelism).

### 2.1 Naming

| surface | name |
|---|---|
| CLI | `--cpu-timeout D` |
| core / MCP arg | `cpu_timeout` |
| `runner.Request` | `CPUTimeout time.Duration` |
| kill actor (`ScopeKill.Actor`, `killWithIntent`) | `run-cpu-timeout` |
| error code | `E_RUN_CPU_TIMEOUT` (exit class 3, matching `E_RUN_TIMEOUT`) |

`E_RUN_CPU_TIMEOUT` takes exit class **3**, the same as `E_RUN_TIMEOUT`
(codes.go:179): a run terminated at a deadline produced no evaluated result, so
it belongs with the unevaluated family, not with `E_RUN_FAILED` (1).

---

## 3. Which verbs get it

### 3.1 `aira run` — yes (foreground path)

`Launch` (runner_linux.go) is the shared foreground implementation and already
owns the deadline, the kill trigger and the terminal CAS. This is the whole of
the ticket's implementation.

### 3.2 `aira confine` — **no**, deferred to its own ticket

This is a real decision, not a shrug. **`aira confine` has no job deadline of
any kind today.** Its accepted options are `slice`, `name`, `owner`,
`memory-reserve`, `memory-max`, `memory-high`, `admit-timeout`, and the valueless
`delegate-ram` / `detach` / `exclusive` (main.go:792) — and `--admit-timeout`
bounds the *admission wait*, not the job. Its wait is an unconditional blocking
`waitConfineCommand(cmd)` (confine_linux.go:1230): no select, no timer, no kill
trigger, no kill-intent ledger, no terminal CAS.

So adding `--cpu-timeout` to confine is not "the same feature on a second verb".
It is building confine's **first** deadline-and-kill path from scratch, which
means answering, in a supervisor with different kill semantics (`cgroup.kill`,
`confine --kill`, the `supervisorSignal` cut-off at confine_linux.go:1243-1247)
and deliberately *no* run ledger, the entire class of arbitration question
AIRA-126 spent a full two-loop cycle on. Doing that as a rider on this ticket
would be the exact mistake the ticket warns against, in a second location.

Deferred, and filed at implementation time with `aira create` (no hand-picked ID),
`relates` AIRA-136, carrying this analysis. If confine ever gets a deadline it
should get *both* bounds in one design, sharing the primitives §4 introduces
(`readCgroupCPUUsed`, `deadlineSource`, and the pure rules), which are written to
be reusable without being pre-generalised.

### 3.3 `aira run --detach` — **no**, and the combination is refused rather than ignored

The detached timeout branch (detach_linux.go:499-517) is a *different, stricter*
branch that has **not** received AIRA-126's arbitration: it returns
`U_RUN_RECONCILE_REQUIRED` on `!attempt.Kill.Completed` without even draining
`waitCh`. Fixing that is **AIRA-131**, filed last night as the exact detached
analogue of AIRA-126 and not yet built.

Wiring a second deadline source into a branch whose racing arbitration is
known-broken and about to be rewritten would (i) double the surface AIRA-131 must
fix, and (ii) make this ticket's test (c) — "resolves the same principled way
AIRA-126 resolves it" — unsatisfiable in the detached shape, because the
detached shape does not yet resolve it that way at all.

**Therefore `--cpu-timeout` together with `--detach` is refused at argument
validation with `E_RUN_ARGUMENT_INVALID`, not silently ignored.** A bound the
operator asked for and did not get is precisely the fake-pass AIRA forbids. The
guard goes beside the existing `--stdin-connect requires --detach` check
(core.go:1562-1564) so it is reachable from every face at once.

Once AIRA-131 lands, extending the detached branch is a small follow-on: the
same `deadlineSource`, the same `deadlineFire`, and the guard is deleted. Filed
as a follow-up `relates` both tickets.

### 3.4 `aira time` — no

`runTimedCommand` (core/command.go:65) is a `context.WithTimeout` wrapper with no
cgroup at all. There is no cumulative CPU quantity to read. Out of scope, stated
so nobody has to re-derive it.

---

## 4. The mechanism — one deadline channel, one kill trigger, one arbitration

This is the most important section of the plan. **The CPU budget must not be a
second, independent kill trigger.** Two goroutines both calling `killWithIntent`
for the same run is the intent-arbitration hazard AIRA-126 exists to get right,
in a new shape.

The design is therefore: **multiplex every deadline that can end a run early into
exactly one channel, from which exactly one value is ever delivered.** Launch's
select keeps exactly two branches, exactly one `killWithIntent` call site, and
AIRA-126's arbitration block is reached identically no matter which bound fired.

### 4.1 The fire value and the source

New file `internal/runner/deadline_linux.go`:

```go
// deadlineFire is the single value a deadline source ever emits. It names the
// bound that fired so the ONE kill site can attribute the kill honestly without
// a second decision.
type deadlineFire struct {
	Actor    string        // "run-timeout" | "run-cpu-timeout"
	Code     string        // "E_RUN_TIMEOUT" | "E_RUN_CPU_TIMEOUT"
	Budget   time.Duration // the bound that was breached
	Observed time.Duration // CPU-time consumed at the deciding sample; zero for the wall bound
}

// deadlineSource multiplexes the wall-clock deadline and the cumulative
// CPU-time budget into ONE channel. At most one value is ever sent: the
// goroutine returns immediately after a send, so Launch keeps exactly one
// deadline branch, exactly one killWithIntent call site, and AIRA-126's
// arbitration remains the sole decision point for BOTH bounds.
//
// C is buffered 1, so a fire that loses the race to the child's own wait is
// discarded without blocking the goroutine — the same discard the existing
// timer.Stop()/drain performs today.
// The source holds NO sampled state Launch reads back. An earlier draft of this
// plan carried a mutex-guarded "last established sample" on it; it is deleted,
// because the authoritative final CPU total is the teardown read AIRA already
// takes (§4.5) and a mid-run sample could only ever be a lower bound, which
// proves nothing a budget check needs — a lower bound under the budget does not
// establish the bound held, and a sample AT or over the budget cannot exist
// without the source having fired. Keeping it would have been shared mutable
// state carrying no evidence: exactly the machinery [[architectural-simplicity]]
// says to refuse.
type deadlineSource struct {
	C    chan deadlineFire
	stop chan struct{}
	done chan struct{}
}

type deadlineConfig struct {
	Wall      time.Duration
	CPU       time.Duration
	CPUBase   time.Duration // baseline read before the child started
	CPUBaseOK bool
	ScopePath string
	Interval  time.Duration
	ReadCPU   func(string) (time.Duration, bool)
}
```

```go
func startDeadlineSource(cfg deadlineConfig) *deadlineSource {
	if cfg.Wall <= 0 && cfg.CPU <= 0 {
		return nil
	}
	src := &deadlineSource{
		C: make(chan deadlineFire, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go func() {
		defer close(src.done)
		var wall <-chan time.Time
		if cfg.Wall > 0 {
			timer := time.NewTimer(cfg.Wall)
			defer timer.Stop()
			wall = timer.C
		}
		var tick <-chan time.Time
		baseline, haveBaseline := cfg.CPUBase, cfg.CPUBaseOK
		if cfg.CPU > 0 {
			ticker := time.NewTicker(cfg.Interval)
			defer ticker.Stop()
			tick = ticker.C
		}
		for {
			select {
			case <-src.stop:
				return
			case <-wall:
				src.C <- deadlineFire{Actor: "run-timeout", Code: "E_RUN_TIMEOUT", Budget: cfg.Wall}
				return
			case <-tick:
				used, ok := cfg.ReadCPU(cfg.ScopePath)
				if !ok {
					// UNEVALUATED. An unreadable counter is never treated as
					// zero: fabricating "no CPU used" would silently disable
					// the bound, and fabricating a large value would kill a
					// job on no evidence. §4.5 records the absence honestly.
					continue
				}
				if !haveBaseline {
					// The pre-start baseline read failed; adopt the first
					// successful sample instead. This UNDERCOUNTS by whatever
					// the child burned before it, so the bound fires LATE,
					// never early.
					baseline, haveBaseline = used, true
					continue
				}
				consumed := used - baseline
				if !decideCPUBudgetExceeded(consumed, cfg.CPU) {
					continue
				}
				src.C <- deadlineFire{Actor: "run-cpu-timeout", Code: "E_RUN_CPU_TIMEOUT",
					Budget: cfg.CPU, Observed: consumed}
				return
			}
		}
	}()
	return src
}

// halt closes the source and JOINS its goroutine, the same shape as
// monitorStop/monitorResult, so no sampler outlives the launch that owns it.
func (s *deadlineSource) halt() { close(s.stop); <-s.done }
```

Properties that make this safe, each of which is a test in §7:

1. **At most one fire, ever.** The goroutine `return`s immediately after any
   send. Launch therefore cannot take the deadline branch twice and cannot call
   `killWithIntent` twice for one run.
2. **No goroutine leak and no blocked send.** `C` is buffered 1; a fire that
   loses to the wait branch sits in the buffer and is discarded when Launch drops
   the source, exactly as today's unread `timer.C` is drained at
   runner_linux.go:626-631.
3. **The sampler is dead before anything can remove the scope.** After a fire the
   goroutine has already returned, so `killWithIntent`'s `scope.Remove()`
   (runner_linux.go:2322) cannot race a live sampler. On the wait branch, `halt()`
   runs before any teardown. The only reads that could ever hit a removed path
   return `ENOENT` → unevaluated → no fire.
4. **Both bounds simultaneously breached** sends exactly one value, chosen by
   Go's select. That is honest: both bounds were breached in the same instant,
   the record names the one that won, and the other is not asserted. Fabricating
   a "both" state would assert an ordering AIRA does not hold.

### 4.2 The CPU read — reuse `parseCPUStat`, not `readCgroupUsage`

```go
// readCgroupCPUUsed returns the cgroup's CUMULATIVE user+system CPU time —
// hierarchical over this cgroup and every descendant, which is exactly the
// quantity a job-wide budget means — and whether it was ESTABLISHED.
//
// A missing file, an unparseable file, or a missing user_usec or system_usec
// key is unevaluated (false), never zero.
//
// It reads ONE file. readCgroupUsage opens four (memory.peak, cpu.stat,
// memory.events, memory.events.local); three are irrelevant to a CPU budget and
// would quadruple the sampler's syscall rate for no evidence. The primitive
// being reused is parseCPUStat, which is what the ticket's "reuse, do not
// rebuild" points at.
func readCgroupCPUUsed(scopePath string) (time.Duration, bool) {
	if scopePath == "" {
		return 0, false
	}
	data, err := os.ReadFile(filepath.Join(scopePath, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	user, system := parseCPUStat(data)
	if user == nil || system == nil {
		return 0, false
	}
	return time.Duration(*user+*system) * time.Microsecond, true
}

// readCgroupCPUFn is the injection point, matching the existing
// readProcStatFn / readProcCgroupFn / readBootIDFn convention.
var readCgroupCPUFn = readCgroupCPUUsed
```

**Why `user_usec + system_usec` and not `usage_usec`.** They are the same pair
the confine trailer already prints and the run record already stores as
`cpu_user`/`cpu_sys`. Enforcing the budget against that exact pair means the
number in the record and the number the kill was decided on are the same quantity
by construction, so an operator can audit a `E_RUN_CPU_TIMEOUT` kill directly
against the record it produced. `usage_usec` can differ from their sum by
rounding, which would make an audit read as an inconsistency.

**Availability.** `cpu.stat`'s base statistics are provided by the cgroup v2 core
in every non-root cgroup regardless of whether the `cpu` controller is enabled —
which is already relied on: the confine trailer prints `cpu=Xs+Ys` today for
scopes with no `cpu` controller. The unevaluated path (§4.5) covers the case
where it is nevertheless unreadable.

### 4.3 The baseline

Read **synchronously, before the child starts**, inside `launchPrep` immediately
after `applyScopeMemoryCap` (runner_linux.go:432-435) — the scope exists there and
no process is in it yet, so the baseline is exact and the skew is zero.

The budget therefore means *CPU-time consumed by this run*, not the scope's
absolute counter. For `aira run` the scope is created fresh per run so the
baseline is ~0 either way; taking it explicitly costs one read, is exact rather
than assumed, and keeps the semantics correct for any future reused or nested
scope. If the baseline read fails, the first successful sample is adopted (see
the sketch), which undercounts and therefore fires **late, never early** — the
safe direction, written down rather than discovered.

That adopted baseline lives entirely inside the sampler goroutine and is
deliberately not published back to Launch: it is a lower bound taken after the
child was already running, so it carries no evidence Launch could use, and
publishing it would be shared mutable state for nothing. Gate condition C3 asks
what `finalConsumed` then means, and `decideFinalCPUConsumed` answers it: with no
ESTABLISHED baseline, the absolute counter. That is an upper bound on this run's
consumption, so it can only err toward reporting
`U_RUN_CPU_BUDGET_UNENFORCED` — never toward a kill, since that value is not an
input to any kill decision. `TestAIRA136FinalCPUConsumedUsesTheAbsoluteCounterWithoutABaseline`
is the rule row.

### 4.4 Launch — the actual diff at the arbitration block

`startDeadlineSource` is called at exactly the point `time.NewTimer(req.Timeout)`
is created today (runner_linux.go:583), so the wall bound's zero instant is
unchanged. The select is:

```go
	timedOut := false
	intentNotExecuted := false
	var timeoutKill killAttempt
	var fired deadlineFire
	deadlines := startDeadlineSource(deadlineConfig{
		Wall: req.Timeout, CPU: req.CPUTimeout,
		CPUBase: cpuBaseline, CPUBaseOK: cpuBaselineOK,
		ScopePath: scope.Reference(), Interval: cpuBudgetSampleInterval,
		ReadCPU: readCgroupCPUFn,
	})
	if deadlines != nil {
		select {
		case outcome := <-waitCh:
			waitErr, waitState = outcome.err, outcome.state
		case fired = <-deadlines.C:
			attempt, killErr := r.killWithIntent(ctx, id, fired.Actor, killPolicy{Enforce: false})
			timeoutKill = attempt
			// AIRA-126, UNCHANGED and now shared by both bounds. A deadline —
			// wall clock or CPU budget — can fire against a scope the leader has
			// already left. killWithIntent has then published a durable intent
			// that killScope refused to execute (it returned at len(pids)==0
			// before Terminate and before Kill), so no signal was sent to
			// anything. When the leader is also proved dead at that instant, the
			// established facts are the child's own exit and the absence of any
			// delivered kill, and this launch honours the pending wait instead
			// of reporting a termination that did not happen.
			intentNotExecuted = decideTimeoutIntentNotExecuted(killErr, attempt, processLive(record.PIDIdentity))
			if killErr != nil || !attempt.IntentPublished {
				timedOut = killErr != nil || !attempt.WaitPublished
			} else {
				timedOut = true
			}
			waitDrained := false
			if intentNotExecuted {
				select {
				case outcome := <-waitCh:
					waitErr, waitState = outcome.err, outcome.state
					timedOut, waitDrained = false, true
				case <-time.After(arbitrationWaitBound(r.grace)):
					intentNotExecuted = false
				}
			}
			if !timedOut && !waitDrained {
				outcome := <-waitCh
				waitErr, waitState = outcome.err, outcome.state
			}
		}
		deadlines.halt()
	} else {
		outcome := <-waitCh
		waitErr, waitState = outcome.err, outcome.state
	}
```

Everything from `attempt, killErr :=` to the closing brace of the branch is the
**existing AIRA-126 code, character for character**, with the single literal
`"run-timeout"` replaced by `fired.Actor`. That is the entire point of the
design: there is no second arbitration to reason about, review, or regress,
because there is no second trigger.

The status block (runner_linux.go:823-835) changes in exactly two literals:

```go
	if timedOut {
		record.Status = StatusKilled
		record.ExitCode, record.Signal = nil, ""
		record.ErrorCodes = appendUnique(record.ErrorCodes, fired.Code)
		if timeoutKill.Kill.Completed {
			record.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true,
				GraceMS: r.termGrace.Milliseconds(), Actor: fired.Actor, At: nowString(r.now)}
			record.KillIntent = KillIntent{Present: true, Sequence: timeoutKill.IntentSequence, Completed: true, Empty: true}
		} else {
			// unchanged
		}
	}
```

`fired` is the zero value on every path that does not take the deadline branch,
and `timedOut` can only be true when the deadline branch was taken, so `fired.Code`
is never empty where it is used. A guard test pins that (§7.2).

Nothing else in Launch changes. `decideTimeoutIntentNotExecuted` and
`decideNotExecutedDisposition` are **untouched** — neither reads the actor, both
are already bound-agnostic — so the terminal CAS at runner_linux.go:881-885 is
also untouched.

### 4.5 Honesty when the budget could not be enforced

A requested bound that AIRA silently failed to apply would be a fake pass. The
rule, as a pure function in `decisions.go`:

```go
// decideCPUBudgetExceeded is the whole comparison, isolated so it cannot
// silently become an absolute-counter test or an off-by-one. An unestablished
// sample never reaches it (the caller skips), and a non-positive budget means
// no budget was requested.
func decideCPUBudgetExceeded(consumed, budget time.Duration) bool {
	return budget > 0 && consumed >= budget
}

// decideFinalCPUConsumed is the run's final consumed CPU-time (gate condition
// C3). With no ESTABLISHED pre-start baseline the absolute counter is used: an
// upper bound on this run's consumption, so it errs only toward reporting the
// code and never toward a kill, since this value is not an input to any kill.
// The sampler's own adopted baseline is deliberately private to its goroutine.
func decideFinalCPUConsumed(total, baseline time.Duration, baselineEstablished bool) time.Duration {
	if !baselineEstablished {
		return total
	}
	return total - baseline
}

// decideCPUBudgetUnenforced reports whether a REQUESTED CPU-time budget cannot
// be asserted to have been APPLIED. Two states reach it:
//
//   - nothing was ever established: no cpu.stat read succeeded during the run
//     AND the teardown read did not succeed either, so AIRA has no idea how
//     much CPU the job used;
//   - the final total DID cross the budget and no CPU-budget kill was executed:
//     sampling was degraded for long enough to miss the crossing, or another
//     bound ended the run first.
//
// A run whose final established total is under the budget is fully evaluated
// even if the sampler was blind for part of it — the teardown read is a
// two-sided proof that the bound held, and AIRA already collects it.
//
// killedByCPUBudget, NOT "the CPU deadline fired", is the suppressing input
// (gate condition C1). A fire that killed nothing — AIRA-126's arbitrated exit —
// leaves the breach a two-sided ESTABLISHED fact with no enforcement against it,
// which is exactly what this code is for; unlike AIRA-126's wall case this is
// not an unknowable ordering. Suppressing on the fire alone would make the
// record depend on sampler PHASE: the same job burning the same CPU would report
// a clean success if a tick happened to land after its exit and an unenforced
// budget if it did not.
func decideCPUBudgetUnenforced(budget time.Duration, killedByCPUBudget bool, finalConsumed time.Duration, finalEstablished bool) bool {
	if budget <= 0 || killedByCPUBudget {
		return false
	}
	if !finalEstablished {
		return true
	}
	return finalConsumed >= budget
}
```

When it reports true, Launch appends `U_RUN_CPU_BUDGET_UNENFORCED` (exit class
3, with the other `U_RUN_*` codes). `finalConsumed` is `snapshotUsage`'s
already-taken teardown read (`record.CPUUser + record.CPUSys`) through
`decideFinalCPUConsumed` — no new read, no new timing.

The name is **UNENFORCED**, not UNEVALUATED (gate condition C2), because the
second state above is a two-sided measured fact and only the enforcement is
missing. `codes.go` and spec §6.4 both state the two states in full, so a reader
of the record cannot take the code as "no measurement".

`killedByCPUBudget` is `timedOut && fired.Code == "E_RUN_CPU_TIMEOUT"`. One
consequence is stated here rather than left to be discovered (gate condition C6):
**a wall-clock kill whose teardown total also crosses the CPU budget carries BOTH
`E_RUN_TIMEOUT` and `U_RUN_CPU_BUDGET_UNENFORCED`**, with `ScopeKill.Actor ==
"run-timeout"`. That is the honest record — the wall bound did the killing, and
the CPU bound the operator also asked for was measured, breached, and not
applied. §7.4 pins it against a real cgroup.

`finalEstablished` has an exact definition, and it must be read off the pointer
fields rather than off a summed number, because a nil in either is what "the
counter could not be read" actually looks like: **`record.CPUUser != nil &&
record.CPUSys != nil` after the teardown `snapshotUsage`.** Both are `*int64`
(types.go:143-144); `readCgroupUsage` leaves them nil on a failed or unparseable
`cpu.stat` read (usage_linux.go:177-196) and `snapshotUsage` only overwrites a
field when the new read produced a value (runner_linux.go:997-1010), so a nil
here is a genuine "never established", never a zero. Summing first and testing
the sum against zero would turn an unreadable counter into a *measured* zero —
the fake-zero the whole ticket's honesty rule forbids — so the plan requires the
nil test, and §7.1's table pins it with a row whose final read is unestablished
and whose budget is large: the correct implementation reports the code there,
while the summed-to-zero implementation reports a clean, fully-evaluated run
(`0 >= 10s` is false) and therefore fails that row. The row exists solely to
fail against that specific wrong implementation.

**This was the plan's one genuinely debatable addition and the plan gate ruled on
it: kept, with C1's correction and C2's rename.** Against it: [[architectural-simplicity]] says prefer "keep the
primitive, document the gap" over new machinery, and on any box AIRA can run on
at all (cgroup v2 delegation is a hard precondition) `cpu.stat` is essentially
always readable, so the code is near-unreachable. For it: near-unreachable *and
would report a clean success under an unenforced bound if it happened* is exactly
the shape AIRA's honesty rule exists for, and the cost is one catalogue entry and
one pure function with no branching in the hot path. The gate agreed, and made it
carry more weight than the draft gave it: under C1 the code is not a rare
non-kernel-scope curiosity but the ordinary record of a wall-clock kill that also
crossed the CPU budget, and of an arbitrated exit that did.

---

## 5. The polling interval — 100 ms, and why

```go
// cpuBudgetSampleInterval is the period of the CPU-time budget sampler. It is
// deliberately its OWN constant and not scopeMembershipSampleInterval: the two
// have unrelated costs and unrelated coverage consequences, and coupling them
// would make a future change to either silently retune the other.
const cpuBudgetSampleInterval = 100 * time.Millisecond
```

**Cost.** One `cpu.stat` read plus a five-line parse: a small pseudo-file read,
on the order of a few microseconds. At 100 ms that is ~0.005 % of a core for the
whole life of the supervisor. The membership sampler's own comment
(runner_linux.go:1593-1603) is the calibration: measured 2026-09-03, at 2 ms an
idle single-process supervisor burned ~13 % of a core and a 31-process tree
~112 %, and at 50 ms those fall to ~0.1 % and ~2 %. That sampler is expensive
because it walks `cgroup.procs` plus O(scope tree size) `/proc` entries every
tick, and its cost therefore scales with the job's process count. The CPU
sampler reads one fixed-size pseudo-file whatever the tree looks like, so it has
neither that scaling term nor that magnitude. Sampler cost is therefore not
the binding constraint here, which frees the interval to be chosen on overshoot.

**Overshoot, which is the binding constraint.** Between the last sample under
budget and the sample that fires, a job can consume up to
`interval × (cores it is actually getting)` of CPU-time. The overshoot is
measured in CPU-seconds and scales with the job's parallelism, not with the
interval alone:

| interval | worst-case overshoot, 1 core | 16 cores (this box) | 64 cores |
|---|---:|---:|---:|
| 50 ms | 0.05 cpu-s | 0.8 cpu-s | 3.2 cpu-s |
| **100 ms** | **0.1 cpu-s** | **1.6 cpu-s** | **6.4 cpu-s** |
| 250 ms | 0.25 cpu-s | 4 cpu-s | 16 cpu-s |
| 1 s | 1 cpu-s | 16 cpu-s | 64 cpu-s |

100 ms keeps the worst case at ~1.6 cpu-s on this machine — under a second and a
half of surprise on a bound an operator will realistically set in minutes.
250 ms and above start producing double-digit cpu-second overshoot on a large box,
which is operator-surprising. 50 ms halves a term that is already dominated by
parallelism while doubling the read rate for no useful resolution gain, and would
also invite the false inference that the two 50 ms constants are related.

**Resolution, stated rather than enforced.** A budget smaller than one interval
is accepted, not refused: it can only fire **late**, never early, so accepting it
asserts nothing false, and a floor would be a refusal AIRA does not need. The
guaranteed resolution is one interval and that is documented on the flag. This
matches how `--timeout` already accepts any positive duration.

**Overshoot direction is always late.** Every mechanism in this design errs the
same way — a coarse interval, a lost baseline, an unevaluated sample — and the
error is always "fires later than the budget", never "kills a job that had not
reached it". That one-directional bias is deliberate and is the property that
makes the whole thing safe to run by default.

---

## 6. Exact code changes

| file | change |
|---|---|
| `internal/runner/types.go` | `Request.CPUTimeout time.Duration` |
| `internal/runner/deadline_linux.go` (new) | `deadlineFire`, `deadlineSource`, `startDeadlineSource`, `halt`, `readCgroupCPUUsed`, `readCgroupCPUFn`, `cpuBudgetSampleInterval` |
| `internal/runner/decisions.go` | `decideCPUBudgetExceeded`, `decideFinalCPUConsumed`, `decideCPUBudgetUnenforced` |
| `internal/runner/runner_linux.go` | baseline read in `launchPrep`; `deadlineSource` replaces the bare timer at 582-632; `fired.Actor` at the one `killWithIntent` call; `fired.Code` / `fired.Actor` in the status block at 823-835; `U_RUN_CPU_BUDGET_UNENFORCED` append |
| `internal/codes/codes.go` | `E_RUN_CPU_TIMEOUT: 3`, `U_RUN_CPU_BUDGET_UNENFORCED: 3` |
| `internal/store/gate_command.go` | line 93 also maps `E_RUN_CPU_TIMEOUT` → `U_GATE_COMMAND_TIMEOUT` |
| `internal/core/core.go` | `cpu_timeout` ArgSpec on `run`; parse; `--cpu-timeout` + `--detach` refusal |
| `cmd/aira/main.go` | `cpu-timeout` in the run option whitelist (1673) and the arg mapping (1854) |
| `cmd/aira/mcp.go` | `cpu_timeout` default in the MCP `run` argument fill — **the plan missed this**, see below |
| `docs/superpowers/specs/2026-08-11-aira-m12-runner-lite-design.md` | §6.3(5) note that "the deadline" covers both bounds; §6.4 note for the two new codes |
| `.aira/tickets/AIRA-136.md` | status + resolution at PR time |

Notes on three of these:

- **`internal/codes` is not optional.** `produced_test.go` (AIRA-87) checks the
  catalogue against a literal scan of the tree in **both** directions, so an
  uncatalogued emitted code fails the suite and a catalogued code that nothing
  emits fails it too. That check is a free non-porous guard that both new codes
  are real, and the plan relies on it rather than adding a bespoke one.
- **The gate mapping is load-bearing.** `gate_command.go:93` maps
  `E_RUN_TIMEOUT` → `U_GATE_COMMAND_TIMEOUT` (unevaluated). Without the same
  mapping, a gate command killed by a CPU budget would fall through to
  `hasRunnerCodeExcept` → `U_GATE_COMMAND_RUN_UNEVALUATED`: still unevaluated, so
  not *dishonest*, but it loses the "it hit its deadline" distinction the code
  exists to carry.
- **Generated surfaces need no separate edit — but the MCP face's ARGUMENT FILL
  did, and the plan was wrong about it.** Help, MCP schemas and the agent guide
  do all come from the `core.go` dispatch table, and there is no committed golden
  to regenerate. What the plan missed is that `cmd/aira/mcp.go` separately fills
  a default for every `run` argument so the MCP and CLI faces construct the
  *identical* `core.Request`, and that list is hand-maintained. Omitting
  `cpu_timeout` there was caught immediately, in the build, by the existing
  parity tests `TestMCPRunnerLaunchMatchesCLIRequestAndPreservesTargetOptions`,
  `TestMCPRunnerPTYMatchesCLIRequest` and
  `TestMCPRunnerTelemetryArgumentsMatchCLIRequest`. It is recorded here rather
  than quietly fixed because §6.1's whole point is that a "files touched" table
  is a claim about the tree, and this is the one place the claim was incomplete.

`parseRunTimeout` (core.go:639) is refactored into a shared
`parsePositiveDuration(value, label string)` so `--timeout`'s existing message
("timeout must be a positive duration") is preserved byte-for-byte and
`--cpu-timeout` gets its own naming its own flag.

### 6.1 Why that table is COMPLETE, not merely plausible

A "files touched" table is a claim about the whole tree, and a missed consumer of
a value this change generalises is exactly how a new kill source acquires a
silent second behaviour. Each claim below is a grep executed against `3f4f54a`,
recorded so a reviewer can re-run it rather than trust it.

- **Nothing outside the runner reads the kill actor string, so a new actor value
  `run-cpu-timeout` cannot change any downstream decision.**
  `grep -rn '"run-timeout"' --include=*.go .` (excluding tests) returns exactly
  four sites, all assignments and all inside `internal/runner`:
  runner_linux.go:588 and :828 (the foreground pair this plan edits) and
  detach_linux.go:504 and :512 (the detached pair §3.3 refuses to touch). No
  site anywhere *compares* `ScopeKill.Actor` to a literal — the only other
  `.Actor` writes are the fixed `"run-kill"`, `"aira"` and `"reconcile"` strings
  at runner_linux.go:1097, :2361, :2388 and :2672 — so the actor is a record
  field for humans and the ledger, never a branch input. **This is the fact that
  makes "one literal becomes `fired.Actor`" a safe change rather than an
  optimistic one.**
- **The consumer set of `E_RUN_TIMEOUT` is closed and has exactly one member
  outside the runner.** `grep -rn 'E_RUN_TIMEOUT' --include=*.go .` (excluding
  tests) returns four sites: the catalogue entry (codes.go:179), the two
  emitters (runner_linux.go:826, detach_linux.go:514), and one reader —
  `gate_command.go:93`. So the gate mapping in the table above is not a
  precaution; it is the *entire* set of behaviour that a second deadline code
  would otherwise miss, and there is provably no second reader to find later.
  No TUI, `aira top`, trailer, or report path branches on it.
- **The codes catalogue's convention tests need no edit for either new code, and
  this was checked rather than assumed.**
  `TestCataloguedExitsFollowThePrefixConvention` requires every `U_` code to exit
  3, which `U_RUN_CPU_BUDGET_UNENFORCED: 3` satisfies, and asserts nothing about
  `E_` codes (its own comment says they are bucketed by kind), so
  `E_RUN_CPU_TIMEOUT: 3` is unconstrained there and aligns with its sibling
  `E_RUN_TIMEOUT: 3` by argument rather than by rule.
  `TestRebucketedCodesFollowTheKindConvention` is a named allowlist of AIRA-107's
  twelve decisions plus their argued neighbours, not a rule quantified over the
  catalogue, so a new code is outside it.
  `TestDivergenceTablesAreCurrent` only polices the two staleness maps, and
  neither new code belongs in either (both are catalogued *and* produced), so no
  entry is added — an entry would in fact fail that test.
  What *does* apply, and is the free non-porous guard this plan leans on, is the
  bidirectional pair `TestEveryProducedCodeIsCatalogued` /
  `TestEveryCataloguedCodeIsProduced`: catalogue a code and never emit it and the
  suite goes red; emit one and never catalogue it and the suite goes red.

---

## 7. Test plan

TDD throughout: every test is written before its production change and proved
against the wrong behaviour by an **executed** revert (§7.6), never by reading.

The pervasive mutation to defend against is the obvious wrong implementation:
**a "CPU timeout" that is really a second wall-clock timer.** Several tests below
exist specifically because they cannot pass against it, and each says so.

### 7.1 Pure rules (`internal/runner/decisions_test.go`, deterministic)

`TestAIRA136CPUBudgetExceededRule` — table: `{consumed: budget-1ns}` false;
`{consumed: budget}` true; `{consumed: budget+1}` true; `{budget: 0}` false for
any consumed (including a huge one); `{budget: -1}` false.

`TestAIRA136CPUBudgetUnevaluatedRule` — one row per conjunct, each differing from
its neighbour in exactly one input: no budget → false; fired → false; established
final under budget → false; established final at/over budget, not fired → true;
**final unestablished with a large budget → true**. That last row is the one
§4.5 names: it is written with a budget the fake-zero implementation would sail
under, so an implementation that sums nil `CPUUser`/`CPUSys` to a measured zero
returns false and fails the row, while the correct nil test returns true. A row
with a small budget would pass both and prove nothing.

The caller-side half of that rule gets its own test, because the table cannot
see it: `TestAIRA136UnevaluatedIsDerivedFromTheNilCounters` drives Launch with a
teardown `cpu.stat` read that fails (injected `readCgroupCPUFn`, and the scope's
`cpu.stat` absent), a requested budget, and no fire, and asserts the record
carries `U_RUN_CPU_BUDGET_UNENFORCED` with `CPUUser == nil && CPUSys == nil` —
proving the code is raised from the absence of evidence rather than from a
zero.

### 7.2 The multiplexer (`internal/runner/deadline_linux_test.go`, hermetic, no cgroups)

- `TestAIRA136DeadlineSourceEmitsAtMostOneFire` — both bounds set, both
  immediately breachable (1 ns wall, a reader already over budget). Receive one
  value; assert a second receive blocks for a bounded window; assert `halt()`
  returns (goroutine exited). **Kills the two-triggers design directly.**
- `TestAIRA136DeadlineSourceCPUFireCarriesCPUActorAndCode` — CPU-only source
  fires with `Actor=="run-cpu-timeout"`, `Code=="E_RUN_CPU_TIMEOUT"`, and
  `Observed >= Budget`.
- `TestAIRA136DeadlineSourceWallFireCarriesWallActorAndCode` — the mirror, so a
  swapped pair cannot pass both.
- `TestAIRA136UnreadableCPUStatNeverFires` — the reader returns
  `(1 * time.Hour, false)`: a value far over the budget, marked **unevaluated**.
  A implementation that ignores `ok` fires instantly; the correct one never fires.
  Assert no fire in a bounded window. **This is the non-porous unevaluated test:
  it fails against the fabricating implementation rather than against nothing.**
- `TestAIRA136CPUBudgetIsMeasuredFromItsBaseline` — baseline 10 s, budget 1 s;
  reader returns 10 s, then 10.5 s, then 11 s. Must not fire on the first two and
  must fire on the third. An implementation using the absolute counter fires
  immediately.
- `TestAIRA136LostBaselineAdoptsFirstSampleAndFiresLate` — `CPUBaseOK` false;
  the first successful sample becomes the baseline and the fire is measured from
  it (never earlier).
- `TestAIRA136DeadlineSourceHaltJoinsItsGoroutine` — `halt()` after a discarded
  fire returns and leaves nothing running (`-race` + a `done` assertion).

### 7.3 Real cgroup, scenario (a): wall-clock-elapsed ≫ CPU-consumed is NOT killed

`TestAIRA136RealCgroupSleepingJobIsNotKilledByCPUBudget` — the ticket asks for "a
CPU-bound job that would exceed a wall-clock timeout under contention but stays
under its CPU-time budget". **Deliberate substitution, with the reason stated in
the test:** inducing genuine load-average-48 contention on a shared box is
antisocial, not reproducible, and not what is under test. The *property* under
test is "wall-clock elapsed far exceeding CPU-time consumed does not fire the CPU
bound", and a sleeping job produces exactly that state deterministically and for
free.

The test is a **pair on the same argv**, which is what makes it non-porous:

| run | argv | bounds | required outcome |
|---|---|---|---|
| A | `/bin/sh -c 'sleep 0.5'` | `Timeout: 100ms` | `killed`, `E_RUN_TIMEOUT`, one terminal |
| B | `/bin/sh -c 'sleep 0.5'` | `CPUTimeout: 100ms`, no wall bound | `exited 0`, `CleanSuccess()`, no `E_RUN_CPU_TIMEOUT`, no `U_RUN_CPU_BUDGET_UNENFORCED`, one terminal |

Same command, same duration, opposite outcomes, and the only difference is which
bound was requested. **A CPU timeout implemented as a wall-clock timer fails run
B.** Additionally assert `record.CPUUser + record.CPUSys < CPUTimeout` on B, so
the pass is grounded in the measured quantity rather than in the absence of a
kill.

### 7.4 Real cgroup, scenario (b): a job that really burns its budget IS killed

`TestAIRA136RealCgroupSpinnerExceedsCPUBudgetAndIsKilled` — `/bin/sh -c 'while :;
do :; done'`, `CPUTimeout: testdeadline.Wait(300ms)`, plus a generous
`Timeout: 30s` wall backstop so a broken build cannot hang the suite. Assert the
**full honest kill shape**, identical to what a wall-clock timeout produces today:

- `Status == StatusKilled`, `!CleanSuccess()`;
- `E_RUN_CPU_TIMEOUT` present **and `E_RUN_TIMEOUT` absent** — proves which source
  fired and that neither is mislabelled;
- `KillIntent.Present && KillIntent.Completed && !KillIntent.NotExecuted`;
- `ScopeKill.Requested && Started && Completed`, `Actor == "run-cpu-timeout"`;
- `ExitCode == nil && Signal == ""` (no fabricated exit for a kill);
- exactly one terminal record;
- `record.CPUUser + record.CPUSys >= CPUTimeout` — **positive proof the kill was
  grounded in real cgroup accounting**, not in elapsed time.

`TestAIRA136RealCgroupWallBoundStillWinsOnASpinner` — the mirror false-fail
guard: the same spinner with a large `CPUTimeout` (30 s) and a small
`Timeout: 100ms` must produce `E_RUN_TIMEOUT` and **not** `E_RUN_CPU_TIMEOUT`,
with `Actor == "run-timeout"`. Neither source may claim the other's kill.

`TestAIRA136RealCgroupWallKillOverTheCPUBudgetCarriesBothCodes` — gate condition
C6's record shape: spinner, `Timeout: 100ms`, `CPUTimeout: 1ms`, asserting
`E_RUN_TIMEOUT` **and** `U_RUN_CPU_BUDGET_UNENFORCED` with
`Actor == "run-timeout"`, plus `E_RUN_CPU_TIMEOUT` absent and the measured total
at or over the budget so the "measured, breached" arm is the one exercised.

The sampler is injected to report an honest, established, permanently-zero
consumption. That is not a convenience: with a 1 ms budget and a 100 ms sample
interval, an uninjected version would be racing the wall timer against the first
tick, and the record would depend on which channel `select` happened to find
ready — the coincidence a test must not rest on. The injection also models the
exact production state §4.5 names (sampling degraded for long enough to miss the
crossing), while the teardown read stays the real kernel one, so the counters the
assertion rests on are genuine cgroup accounting.

`TestAIRA136RealCgroupBudgetIsCumulativeAcrossTheScope` — the assertion that the
budget is a *cgroup-wide* sum, which no wall-clock implementation can satisfy.

Gate condition C4 replaced the draft's two-run wall comparison, which asserted a
kernel scheduling fact against an unstated "materially shorter" threshold and was
flaky under load. It is now a **single run** of four spinners
(`for i in 1 2 3 4; do (while :; do :; done) & done; wait`) with
`CPUTimeout: 1500ms` — comfortably more than one 100 ms sampling interval, so a
single interval of overshoot cannot by itself push the elapsed wall past the
budget — asserting:

- `record.CPUUser + record.CPUSys >= CPUTimeout` (the kill was grounded in the
  cumulative total), and
- elapsed wall **< `CPUTimeout`**.

A wall-clock mutant always yields elapsed >= budget, so it fails that second row
deterministically. Contention honesty, because this is the one test a loaded box
can distort: the achieved parallelism is computed from the run's own record,
`(CPUUser+CPUSys) / elapsed`, and below 1.5 the claim is **unevaluated** and the
test `t.Skip`s with the measured value logged — an honest unevaluated, never a
fake pass and never a flaky fail. `runtime.NumCPU() < 2` skips for the same
reason.

### 7.5 Scenario (c): the AIRA-126 arbitration race, for the CPU source

This is the ticket's hardest requirement and the reason for §4's design. **The
race resolves the same principled way as AIRA-126 not by a parallel rule but
because it is literally the same code path**, entered from the same select branch
with the same `killWithIntent`, the same `decideTimeoutIntentNotExecuted`, the
same `processLive` proof and the same bounded receive. The tests prove that
claim rather than assuming it.

Hermetic and deterministic, reusing AIRA-126's own harness
(`internal/runner/timeout_arbitration_linux_test.go`: `livenessScope`,
`gatedStdin`, `aira126Harness`) with an injected `readCgroupCPUFn`. `gatedStdin`
holds the wait outcome past the fire instant, so the deadline branch is taken
deterministically against a genuinely empty scope and a genuinely dead leader —
no faked evidence, only the moment Launch learns of the wait.

Gate condition C7 governs the injected reader: the budget crossing must become
observable **only after the child's real exit**, never on a wall-clock
coincidence. `overBudgetAfterLeaderExit` returns an honest, established 0 until
the harness observes the leader's death through `readProcStatFn` — the same real
kernel liveness `livenessScope` models `cgroup.procs` with — and only then a
value far over the budget. The pre-start baseline read therefore lands on the
honest 0, and the first sample that can fire is one taken after the child was
already reaped. Per the existing `readProcStatFn` convention, no test that swaps
`readCgroupCPUFn` is `t.Parallel`.

- `TestAIRA136CPUBudgetAgainstAlreadyExitedLeaderArbitratesToExited` — the CPU
  budget fires against an already-empty scope. Required outcome, **identical to
  AIRA-126's**: no `LaunchError`; `StatusExited` with the child's real exit code;
  `KillIntent.NotExecuted && !Completed`; `!ScopeKill.Started`; **no
  `E_RUN_CPU_TIMEOUT`** (nothing was terminated, and whether the exit fell before
  or after the budget crossing is unestablished — §8); exactly one terminal
  record. Non-vacuity: the loop counts the iterations that actually took the
  deadline branch and fails if that count is zero (AIRA-126 gate condition C5).
  Additionally, per gate condition C1, **`U_RUN_CPU_BUDGET_UNENFORCED` IS
  present**: the requested budget was not applied, and this harness has no kernel
  cgroup, so the "never measured" arm is the one that fires — the test asserts
  `CPUUser == nil && CPUSys == nil` alongside it, so the code is provably raised
  from the absence of evidence rather than from a measured zero. An
  implementation that suppressed the code merely because the CPU deadline FIRED
  reports a clean, fully-evaluated run here and fails this row. The "measured,
  breached" arm is pinned separately by §7.4's both-codes row.
- `TestAIRA136CPUBudgetWithLiveLeaderStillKills` — **the mutation guard, and the
  most important test in the set.** Same empty fake scope, but the leader is
  still alive at the fire (`emptyAfterFirstMembers`). `processLive` reads alive,
  the arbitration must refuse, and the record must be `killed` with
  `E_RUN_CPU_TIMEOUT` and today's reconcile-required/`handoff-unverified` shape.
  This is what stops the fix widening into "an empty scope means the run
  finished" through the new source. It is also C1's complement: this run IS
  reported as terminated by its CPU budget, so `U_RUN_CPU_BUDGET_UNENFORCED` must
  be **absent** — the one state that suppresses it.
- `TestAIRA136CPUBudgetDoesNotDismissAForeignKillIntent` — `seedForeignIntent`
  plus a CPU fire: `IntentCreated` is false, no disposition, today's outcome.
- `TestAIRA136DeadlineBranchTakenOnceUnderBothBounds` — **the ledger-level proof
  that the CPU budget did not become a second kill trigger.** Both bounds are
  requested. The CPU budget is breached from the first sample (~100 ms); the wall
  bound is set an order of magnitude beyond it. Under the multiplexed design the
  source sends ONE value and returns, so the wall timer is structurally dead from
  that instant and Launch's single deadline branch attributes the kill to the CPU
  bound: `E_RUN_CPU_TIMEOUT` present, `E_RUN_TIMEOUT` absent,
  `ScopeKill.Actor == "run-cpu-timeout"`, exactly one `kill-intent` event, exactly
  one terminal record.

  The attribution is what makes it non-porous, and it is worth saying why the
  event count alone would not be. Against the two-independent-triggers design — a
  bare wall timer left in Launch's select plus a separate goroutine that kills on
  the CPU budget by itself — the independent goroutine's `killWithIntent` ADOPTS
  the run's existing intent rather than creating a second one, so the ledger still
  holds exactly one `kill-intent` event. What the mutant cannot fake is the
  attribution: Launch's own select contains only the wall timer, so the record it
  writes says `run-timeout`, an order of magnitude of wall clock later. Every
  assertion on the CPU attribution fails against it, deterministically.

Opt-in real-cgroup soak, for the same reason and in the same shape as
`TestAIRA126RealCgroupDeadlineStraddleSoak` (plan §12.1: a ~2 % race needs a long
loop to be non-vacuous, and a long loop is a minute of suite time on every run):

- `TestAIRA136RealCgroupCPUBudgetStraddleSoak` — guarded by `AIRA136_SOAK=1`,
  `AIRA136_SOAK_ITERATIONS` (default 800). A short spinner sized so its total CPU
  lands within ~one sample of its own exit. Both outcomes accepted, each against
  its **full** evidence signature (the killed shape from §7.4, the
  arbitrated-exited shape from above), plus the non-vacuity counter and a
  `t.Logf` of how often the budget fired and how often it found the scope empty.
  Committed as an executable reproduction per CLAUDE.md, not left as a transcript.

### 7.6 Argument surface, gate, and the executed revert

- `TestAIRA136CPUTimeoutRequiresAPositiveDuration` — `""` → no bound; `"0"`,
  `"-1s"`, `"banana"` → `E_RUN_ARGUMENT_INVALID` naming `--cpu-timeout`.
- `TestAIRA136CPUTimeoutWithDetachIsRefused` — `E_RUN_ARGUMENT_INVALID`, from
  core so both faces get it. The complement, `--cpu-timeout` without `--detach`,
  is accepted, so the guard cannot be a blanket rejection.
- `TestAIRA136CLIAcceptsCPUTimeoutOnce` — the CLI whitelist accepts it, rejects a
  duplicate, and rejects it as a `confine` option.
- `TestAIRA136GateMapsCPUTimeoutToCommandTimeout` (`internal/store`) — a record
  carrying `E_RUN_CPU_TIMEOUT` yields `U_GATE_COMMAND_TIMEOUT` and
  `PredicateUnevaluated`.
- Catalogue coverage is `internal/codes/produced_test.go`, bidirectionally, with
  no new test needed.

**Executed reverts (§7 is worthless without them).** Both were run, not read.
Gate condition C8 requires the exact exit codes and named failing tests, so they
are recorded here.

**Revert 1 — the CPU wiring neutered.** `startDeadlineSource` builds the source
as usual but the CPU branch can never fire (`tick = nil` after the ticker is
created, so the wall bound and the source's lifecycle are untouched and no test
dies on a nil source instead of on the property under test).

```
AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner/ ./internal/core/ ./internal/store/ -run 'AIRA136' -count=1
exit 1
```

Failing tests, named:

| test | what it proves the mutant lacks |
|---|---|
| `TestAIRA136DeadlineSourceCPUFireCarriesCPUActorAndCode` | the CPU bound exists at all |
| `TestAIRA136CPUBudgetIsMeasuredFromItsBaseline` | it is measured from the baseline |
| `TestAIRA136LostBaselineAdoptsFirstSampleAndFiresLate` | the lost-baseline adoption |
| `TestAIRA136UnreadableCPUStatNeverFires` | its own non-vacuity counter: a blind sampler is not the same as a sampler that never runs |
| `TestAIRA136CPUBudgetAgainstAlreadyExitedLeaderArbitratesToExited` | the non-vacuity counter — the deadline branch was never taken |
| `TestAIRA136CPUBudgetWithLiveLeaderStillKills` | the CPU bound kills a live leader |
| `TestAIRA136DeadlineBranchTakenOnceUnderBothBounds` | the CPU bound wins over a far-away wall bound |
| `TestAIRA136RealCgroupSpinnerExceedsCPUBudgetAndIsKilled` | scenario (b) |
| `TestAIRA136RealCgroupBudgetIsCumulativeAcrossTheScope` | the cumulative-across-the-scope claim |

`TestAIRA136UnreadableCPUStatNeverFires` was expected to pass on both sides and
did **not**: its `reader.count() == 0` non-vacuity guard fails when the sampler
never runs at all. That is the guard doing its job and is recorded rather than
smoothed over. Everything else in the `AIRA136` set passed on both sides — the
pure rules, the wall-bound tests, and, as the plan predicted, the over-widening
guards `TestAIRA136RealCgroupWallBoundStillWinsOnASpinner` and
`TestAIRA136CPUBudgetDoesNotDismissAForeignKillIntent`, plus
`TestAIRA136RealCgroupSleepingJobIsNotKilledByCPUBudget` (a sleeping job is not
killed either way) and `TestAIRA136RealCgroupWallKillOverTheCPUBudgetCarriesBothCodes`
(its sampler is deliberately blind already).

**Revert 2 — the multiplexing only.** Launch's source keeps the wall bound
(`CPU: 0`) and the CPU budget becomes a second, independent goroutine with its
own `killWithIntent` call site — the exact shape §4 refuses.

```
AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner/ -run 'AIRA136DeadlineBranchTakenOnceUnderBothBounds' -count=1
exit 1
```

`TestAIRA136DeadlineBranchTakenOnceUnderBothBounds` failed, and the failure is
worth reading rather than merely counting: the mutant returned
`U_RUN_RECONCILE_REQUIRED: kill intent won before terminal evidence` with a
**non-terminal `running` record** carrying
`[E_RUN_SCOPE_MIGRATION E_RUN_TIMEOUT U_RUN_RECONCILE_REQUIRED U_RUN_CPU_BUDGET_UNENFORCED]`.
The independent trigger published an intent Launch's own deadline branch could
not disposition, the terminal CAS refused, and the run was left unevaluated and
mis-attributed to the wall bound. That is the intent-arbitration hazard AIRA-126
exists to prevent, reproduced in the new shape, and it is why the CPU budget is
multiplexed rather than added.

Both reverts were then restored and the tree confirmed clean before the gate was
re-run.

### 7.7 Merge gate — foreground, exact exit codes recorded

```
aira confine -- go build ./...
aira confine -- go vet ./...
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1
gofmt -l internal/ cmd/
```

Run serially, in the foreground, each to completion, with its exact exit code
recorded. No truncated-output green claims.

**Executed, on the restored tree:**

| command | exit |
|---|---:|
| `aira confine -- go build ./...` | 0 |
| `aira confine -- go vet ./...` | 0 |
| `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` | 0 |
| `gofmt -l internal/ cmd/` | 0, empty output |

---

## 8. How the CPU-budget-versus-clean-exit race resolves, stated explicitly

The ticket requires that this match AIRA-126's principled resolution rather than
a new ad-hoc one. It does, and the reasoning transfers without weakening:

When the CPU budget's fire reaches `killWithIntent` and `killScope` returns
`{Empty: true, Started: false}` — meaning it returned at `len(pids) == 0`
**before** `Terminate` and before `Kill`, so no signal was sent to anything —
and `processLive` proves the leader was already dead at that instant, then the
five conjuncts of `decideTimeoutIntentNotExecuted` hold and the run commits the
child's real exit with the intent dispositioned `NotExecuted`. **No
`E_RUN_CPU_TIMEOUT` is appended**, for exactly AIRA-126 §5's reason: the code
asserts that a run was terminated for exceeding its bound, and nothing was
terminated. The fired budget is preserved as the durable `kill-intent` ledger
event plus the `NotExecuted` disposition.

The one thing that is *different* about the CPU source, named rather than
glossed: its crossing is observed by **sampling**, so in addition to AIRA-126's
unestablished exit-versus-deadline ordering, the exact instant the budget was
crossed is itself known only to within one sample interval. That makes the
ordering *more* unestablished, not less — and the resolution is the same one
AIRA-126 already chose for an unestablished ordering: assert neither direction,
report the established facts (the child exited by itself with status N, by
`wait4`; no signal was delivered, by control flow), and let the durable intent
carry the record that a bound fired and killed nothing.

**Where the CPU source's reasoning STOPS transferring, and gate condition C1.**
The paragraphs above are about the *ordering* — whether the child's exit fell
before or after the budget crossing — and that is genuinely unestablished, so no
`E_RUN_CPU_TIMEOUT` is appended. But the CPU source has a second fact AIRA-126's
wall source does not: the **final total**, read at teardown, is a two-sided
measurement of what the job actually consumed. When that total reaches the
budget, the breach itself is established even though the ordering is not, and
nothing enforced it. That is a different claim from the ordering, and it is the
claim `U_RUN_CPU_BUDGET_UNENFORCED` carries.

So the draft's rule — suppress on "the CPU deadline fired" — was wrong, and the
counterexample is exact. Budget 300 ms; a job burns 350 ms of CPU and exits. If
it exits between ticks, no fire ever happens and the teardown total 350 >= 300
raises the code. If a tick lands one interval later against the now-empty scope,
AIRA-126's arbitration commits the exited-0 record and the draft rule returned
false, so the record was a clean success with no code at all. Identical job
behaviour, different records, decided by sampler phase — and the second is a fake
pass. The rule therefore suppresses only on `killedByCPUBudget`, an EXECUTED CPU
kill; an arbitrated exit, a wall-clock kill and a plain exit are all decided by
the teardown total.

`CleanSuccess()` is therefore true for such a run **only when the final total is
under the budget**. A run whose CPU budget fired, delivered nothing, and whose
measured total reached the budget carries `U_RUN_CPU_BUDGET_UNENFORCED` and is
consequently not a clean success — which is the honest report, because the
operator asked for a bound and did not get it. Where the total is under the
budget, AIRA-126's conclusion stands unchanged: the command succeeded and the
bound had no effect on it.

---

## 9. Accepted coverage gaps, written down

1. **Sampling overshoot.** A job can exceed its CPU budget by up to
   `cpuBudgetSampleInterval × achieved parallelism` before the fire. Always in
   the late direction. Quantified in §5 and documented on the flag; not a defect.
2. **Sub-interval budgets.** A budget smaller than one sample interval is
   accepted and can only fire late. Its guaranteed resolution is one interval.
3. **Baseline skew if the pre-start read fails.** The first successful sample
   becomes the baseline, undercounting by whatever the child burned before it.
   Late direction only.
4. **`aira confine` has no CPU bound** (§3.2), and `aira run --detach` refuses the
   combination (§3.3) until AIRA-131 lands. Both are filed, not silently absent.
5. **Simultaneous breach of both bounds** records the one Go's select picked. The
   other is not asserted, because no ordering was established. This is about
   which bound did the KILLING, and is separate from the record shape gate
   condition C6 pins: a wall-clock kill whose final measured total also crosses
   the CPU budget carries `E_RUN_TIMEOUT` **and**
   `U_RUN_CPU_BUDGET_UNENFORCED`, with `ScopeKill.Actor == "run-timeout"`. There
   is no ambiguity there and nothing is suppressed — one bound killed the run and
   the other was measured, breached, and not applied.
6. **Everything AIRA-126 §8 already accepts** applies unchanged to the CPU
   source, because it is the same code: the crash window between arbitration and
   terminal append, the bounded-receive expiry, and a foreign concurrent intent.
7. **No kernel cgroup, no enforcement — and it says so.** Where the run's scope
   is not a real kernel cgroup (the in-memory backends the hermetic tests use
   today, and any future run-side analogue of confine's `ci-shim` mode, which
   `confine.go:177-198` already describes as "there IS no scope"), every
   `cpu.stat` read is unevaluated, the budget can never fire, and a *requested*
   budget therefore ends the run with `U_RUN_CPU_BUDGET_UNENFORCED` (§4.5).
   That is the intended outcome and the reason §4.5's rule is worth its code: the
   alternative is a run that reports clean success under a bound the machine was
   structurally incapable of applying. It is stated here so the code is read as
   "the environment could not enforce this", not as a defect. `aira confine`'s
   shim mode is unaffected, because §3.2 gives confine no CPU bound at all.
8. **Contention is not itself reproduced in tests** (§7.3). The property under
   test — wall-clock elapsed decoupled from CPU-time consumed — is reproduced
   exactly and deterministically; the load average that motivates it is not, and
   inducing it on a shared box would be antisocial and unreliable.

---

## 10. Deliberate non-changes

- **`decideTimeoutIntentNotExecuted`, `decideNotExecutedDisposition`,
  `killScope`, `killWithIntent`'s logic, `mergeEvidence`, `decideReconcile`, the
  OOM classifier, and the terminal CAS are untouched.** The CPU source enters
  through the existing branch and changes one string.
- **No new ledger event kind.** `kill-intent` plus the terminal event carry the
  whole story; the actor already distinguishes the source in `ScopeKill.Actor`.
- **`readCgroupUsage` is untouched** and still reads all four files at teardown.
  Only `parseCPUStat` is shared.
- **`RunRecord` gains no budget field.** It does not record `Timeout` today
  either; the record's `cpu_user`/`cpu_sys` plus the error code are the evidence,
  and adding a second CPU number (the deciding sample) would invite confusion
  with the authoritative teardown total.
- **`scopeMembershipSampleInterval` is not reused, retuned, or merged with the
  new sampler.** Separate concerns, separate costs, separate coverage
  consequences.
- **The wall-clock bound's behaviour, timing, and every existing timeout test are
  unchanged.**

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
