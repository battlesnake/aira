# AIRA-136 — a job deadline expressed in cumulative CPU-time, not wall clock

Status: planned. Branch `aira136-cpu-time-timeout` off `origin/master` at
`7a43050`. Two-loop work per CLAUDE.md: this touches the kill/terminal-CAS
arbitration AIRA-126 landed hours ago, so this document is the plan artifact for
plan-review and the plan gate. No production code is changed by this commit.

Every line reference below is `origin/master` at `7a43050`.

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
type deadlineSource struct {
	C    chan deadlineFire
	stop chan struct{}
	done chan struct{}

	mu          sync.Mutex
	established bool // any cpu.stat read succeeded (baseline included)
	last        time.Duration
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
		established: cfg.CPUBaseOK,
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
				src.observe(used)
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

// decideCPUBudgetUnevaluated reports whether a REQUESTED CPU-time budget cannot
// be asserted to have held. Two states reach it:
//
//   - nothing was ever established: no cpu.stat read succeeded during the run
//     AND the teardown read did not succeed either, so AIRA has no idea how
//     much CPU the job used;
//   - the final total DID cross the budget and nothing was killed: sampling was
//     degraded for long enough to miss the crossing.
//
// A run whose final established total is under the budget is fully evaluated
// even if the sampler was blind for part of it — the teardown read is a
// two-sided proof that the bound held, and AIRA already collects it.
func decideCPUBudgetUnevaluated(budget time.Duration, fired bool, finalConsumed time.Duration, finalEstablished bool) bool {
	if budget <= 0 || fired {
		return false
	}
	if !finalEstablished {
		return true
	}
	return finalConsumed >= budget
}
```

When it reports true, Launch appends `U_RUN_CPU_BUDGET_UNEVALUATED` (exit class
3, with the other `U_RUN_*` codes). `finalConsumed` is `snapshotUsage`'s
already-taken teardown read (runner_linux.go:862, `record.CPUUser + record.CPUSys`)
minus the same baseline — no new read, no new timing.

**This is the plan's one genuinely debatable addition and the plan gate should
rule on it.** Against it: [[architectural-simplicity]] says prefer "keep the
primitive, document the gap" over new machinery, and on any box AIRA can run on
at all (cgroup v2 delegation is a hard precondition) `cpu.stat` is essentially
always readable, so the code is near-unreachable. For it: near-unreachable *and
would report a clean success under an unenforced bound if it happened* is exactly
the shape AIRA's honesty rule exists for, and the cost is one catalogue entry and
one pure function with no branching in the hot path. The plan adopts it; a
GATE-FAIL on this point would be answered by deleting the code and the rule and
writing the gap into §9 instead.

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
whole life of the supervisor. It is three orders of magnitude below the
membership sampler's cost, whose own comment (runner_linux.go:1593-1603) records
~0.1 % of a core for a single-process supervisor and ~112 % at 2 ms for a
31-process tree — because *that* sampler walks `cgroup.procs` plus O(tree size)
`/proc` entries per tick. This one walks nothing. Sampler cost is therefore not
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
| `internal/runner/decisions.go` | `decideCPUBudgetExceeded`, `decideCPUBudgetUnevaluated` |
| `internal/runner/runner_linux.go` | baseline read in `launchPrep`; `deadlineSource` replaces the bare timer at 582-632; `fired.Actor` at the one `killWithIntent` call; `fired.Code` / `fired.Actor` in the status block at 823-835; `U_RUN_CPU_BUDGET_UNEVALUATED` append |
| `internal/codes/codes.go` | `E_RUN_CPU_TIMEOUT: 3`, `U_RUN_CPU_BUDGET_UNEVALUATED: 3` |
| `internal/store/gate_command.go` | line 93 also maps `E_RUN_CPU_TIMEOUT` → `U_GATE_COMMAND_TIMEOUT` |
| `internal/core/core.go` | `cpu_timeout` ArgSpec on `run`; parse; `--cpu-timeout` + `--detach` refusal |
| `cmd/aira/main.go` | `cpu-timeout` in the run option whitelist (1673) and the arg mapping (1854) |
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
- **Generated surfaces need no separate edit.** Help, MCP schemas and the agent
  guide come from the `core.go` dispatch table; adding the `ArgSpec` is the whole
  change. There is no committed golden to regenerate (verified: nothing outside
  `core.go` carries the run flag list).

`parseRunTimeout` (core.go:639) is refactored into a shared
`parsePositiveDuration(value, label string)` so `--timeout`'s existing message
("timeout must be a positive duration") is preserved byte-for-byte and
`--cpu-timeout` gets its own naming its own flag.

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
final unestablished → true.

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
| B | `/bin/sh -c 'sleep 0.5'` | `CPUTimeout: 100ms`, no wall bound | `exited 0`, `CleanSuccess()`, no `E_RUN_CPU_TIMEOUT`, no `U_RUN_CPU_BUDGET_UNEVALUATED`, one terminal |

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

`TestAIRA136RealCgroupBudgetIsCumulativeAcrossTheScope` — the assertion that the
budget is a *cgroup-wide* sum, which no wall-clock implementation can satisfy.
Two runs on the same box, same `CPUTimeout`, one spinner vs four spinners
(`for i in 1 2 3 4; do (while :; do :; done) & done; wait`); assert the
four-spinner run's **wall** duration is materially shorter, because four
processes reach the same cumulative budget in less elapsed time.

Contention honesty, because this is the one test a loaded box can distort:
compute the four-spinner run's achieved parallelism from its own record,
`(CPUUser+CPUSys) / wall`. If it is below 1.5 the box did not actually give the
job parallelism, the comparison is **unevaluated**, and the test `t.Skip`s with
the measured value logged — an honest unevaluated, never a fake pass and never a
flaky fail. `runtime.NumCPU() < 2` skips for the same reason.

### 7.5 Scenario (c): the AIRA-126 arbitration race, for the CPU source

This is the ticket's hardest requirement and the reason for §4's design. **The
race resolves the same principled way as AIRA-126 not by a parallel rule but
because it is literally the same code path**, entered from the same select branch
with the same `killWithIntent`, the same `decideTimeoutIntentNotExecuted`, the
same `processLive` proof and the same bounded receive. The tests prove that
claim rather than assuming it.

Hermetic and deterministic, reusing AIRA-126's own harness
(`internal/runner/timeout_arbitration_linux_test.go`: `livenessScope`,
`gatedStdin`, `aira126Harness`, extended with a `cpuBudget` option and an
injected `readCgroupCPUFn`). `gatedStdin` holds the wait outcome past the fire
instant, so the deadline branch is taken deterministically against a genuinely
empty scope and a genuinely dead leader — no faked evidence, only the moment
Launch learns of the wait:

- `TestAIRA136CPUBudgetAgainstAlreadyExitedLeaderArbitratesToExited` — the CPU
  budget fires against an already-empty scope. Required outcome, **identical to
  AIRA-126's**: no `LaunchError`; `StatusExited` with the child's real exit code;
  `KillIntent.NotExecuted && !Completed`; `!ScopeKill.Started`; **no
  `E_RUN_CPU_TIMEOUT`** (nothing was terminated, and whether the exit fell before
  or after the budget crossing is unestablished — §8); exactly one terminal
  record. Non-vacuity: the loop counts the iterations that actually took the
  deadline branch and fails if that count is zero (AIRA-126 gate condition C5).
- `TestAIRA136CPUBudgetWithLiveLeaderStillKills` — **the mutation guard, and the
  most important test in the set.** Same empty fake scope, but the leader is
  still alive at the fire (`emptyAfterFirstMembers`). `processLive` reads alive,
  the arbitration must refuse, and the record must be `killed` with
  `E_RUN_CPU_TIMEOUT` and today's reconcile-required/`handoff-unverified` shape.
  This is what stops the fix widening into "an empty scope means the run
  finished" through the new source.
- `TestAIRA136CPUBudgetDoesNotDismissAForeignKillIntent` — `seedForeignIntent`
  plus a CPU fire: `IntentCreated` is false, no disposition, today's outcome.
- `TestAIRA136DeadlineBranchTakenOnceUnderBothBounds` — both bounds set and both
  breached; exactly one `kill-intent` event in the ledger and exactly one
  terminal record. **This is the ledger-level proof that the CPU budget did not
  become a second kill trigger.**

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

**Executed revert (§7 is worthless without it).** With only the CPU wiring
neutered — `startDeadlineSource` ignoring `cfg.CPU` — run
`go test ./internal/runner/ ./internal/core/ ./internal/store/ -run 'AIRA136' -count=1`
and record the exact exit code and the **named** failing tests. Expected to fail
at minimum: §7.3 run B's pair partner (the CPU bound stops existing), §7.4's
spinner kill, §7.5's arbitration and mutation-guard pair. Expected to **pass on
both sides**, and recorded as such: `TestAIRA136UnreadableCPUStatNeverFires`,
`TestAIRA136RealCgroupWallBoundStillWinsOnASpinner`,
`TestAIRA136CPUBudgetDoesNotDismissAForeignKillIntent` — the over-widening guards.

A second revert, of the *multiplexing* only (restoring a bare wall timer and
adding an independent CPU-kill goroutine), must fail
`TestAIRA136DeadlineBranchTakenOnceUnderBothBounds`. That is the executed proof
that the single-trigger property is actually enforced by a test and not merely
asserted by this document.

Then restore, confirm `git status --porcelain` is empty, and re-run the gate.

### 7.7 Merge gate — foreground, exact exit codes recorded

```
aira confine -- go build ./...
aira confine -- go vet ./...
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1
gofmt -l internal/ cmd/
```

Run serially, in the foreground, each to completion, with its exact exit code
recorded. No truncated-output green claims.

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

`CleanSuccess()` can therefore be true for a run whose CPU budget fired and
delivered nothing. That is correct and is the same conclusion AIRA-126 reached:
the command succeeded and the bound had no effect on it.

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
   other is not asserted, because no ordering was established.
6. **Everything AIRA-126 §8 already accepts** applies unchanged to the CPU
   source, because it is the same code: the crash window between arbitration and
   terminal append, the bounded-receive expiry, and a foreign concurrent intent.
7. **Contention is not itself reproduced in tests** (§7.3). The property under
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
