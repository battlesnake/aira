---
{"schema":1,"id":"AIRA-136","project":"aira","title":"aira run/confine: express a job timeout in cumulative CPU-time, not just wall-clock","status":"in-review","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["confine","runner"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-138","to":"AIRA-136"}]}
---
Requested directly by the owner, 2026-09-06.

## The ask

On a contended shared machine (this box, most of tonight — load averages up to
48), a job can lose most of its wall-clock time to CPU contention rather than
to its own real work: it gets fewer cores, or has to share/fight for the ones
it has, with other concurrent jobs. A pure wall-clock timeout then kills it
for an external reason (starvation) rather than the reason a timeout is
supposed to catch (the job itself is taking too long / hung). A timeout
expressed in CUMULATIVE CPU-TIME CONSUMED (summed across every thread/process
in the job's cgroup, the same quantity cgroup v2 already accounts) only
counts time the job actually got to run ON a cpu, so contention that reduces
its share does not by itself trigger it -- a starved job just takes longer in
wall-clock terms to reach the SAME cpu-time budget, and only a job that is
truly consuming that much cpu (working or spinning) gets killed.

## What already exists (verified against source; reuse, do not rebuild)

- The wall-clock timeout in `Launch` (internal/runner/runner_linux.go:582-606)
  is a single `time.NewTimer(req.Timeout)` (req.Timeout is a bare
  `time.Duration`, internal/runner/types.go:175) racing the job's own wait
  completion in one `select`. On fire, it calls `r.killWithIntent(...)`, the
  SAME kill/publish-intent path a real signal-driven kill uses.
- **This exact select block is what AIRA-126 (merged tonight) just fixed** --
  the kill/terminal-CAS arbitration around a timeout racing the job's own
  clean exit is fresh, deliberately reasoned, two-loop-reviewed code. Any
  CPU-time timeout MUST integrate with this SAME decision point rather than
  add a second, independent kill trigger racing the first -- a second
  concurrent kill SOURCE targeting the same job is exactly the class of
  intent-arbitration hazard AIRA-126 exists to get right, and duplicating the
  mechanism risks reintroducing it in a new shape. Read AIRA-126's ticket and
  plan doc (docs/superpowers/plans/2026-09-06-aira126-kill-terminal-arbitration-plan.md)
  before touching this code.
- Cumulative cgroup CPU-time reading ALREADY EXISTS and is already correct:
  `readCgroupUsage` (internal/runner/usage_linux.go:185) parses `cpu.stat`
  (via `parseCPUStat`) into user+sys usec, the same numbers already shown in
  every confine trailer's `cpu=Xs+Ys` field. Today it is called ONCE, at
  teardown (per the comment at confine_linux.go:1253) -- not periodically
  during the job's life. This ticket's real new work is invoking that same
  read PERIODICALLY while the job is still running (a ticker, the same shape
  as the existing 50ms scope-membership sampler AIRA-20/34 already run for
  descendant-escape detection -- reuse that polling pattern, do not invent a
  second one), and feeding the accumulated total into the SAME timeout
  decision point named above.

## Scope decision needed at plan time, not assumed here

- Whether this is a NEW flag (e.g. `--cpu-timeout DURATION`) alongside the
  existing wall-clock `--timeout`, usable independently or together (whichever
  fires first wins, both going through the SAME kill path with a distinguishing
  reason string, e.g. `run-timeout` vs `run-cpu-timeout`), or a MODE FLAG that
  changes what the existing `--timeout` value measures. The former is
  probably right (does not change existing --timeout's meaning for anyone
  already using it -- AIRA is pre-release so a meaning change costs nothing
  compatibility-wise, but a new orthogonal flag is still the more honest,
  discoverable shape: an operator asking for a cpu-time bound should not have
  to remember that --timeout secretly means something different now) -- confirm
  at plan time, do not assume.
- Which verbs get it: `aira run` certainly (Launch is shared); check whether
  `aira confine` should too, and whether the detached-run path (AIRA-131,
  filed tonight, is the exact detached analogue of AIRA-126's foreground fix,
  not yet built) needs to land first or can be built alongside.
- Polling interval: cheap enough to run for a job's whole life without being
  a real cost itself (the existing 50ms scope sampler is a plausible starting
  point) but coarse enough that the CPU-time budget cannot overshoot by an
  operator-surprising amount. Make this a real, reasoned choice in the plan,
  not a guess.

## Correctness-critical: full two-loop, not the light build+review path

This touches the SAME kill/terminal-CAS arbitration CLAUDE.md already names as
mandatory two-loop territory, freshly so given AIRA-126. Plan, then a real
plan-gate review, before implementation -- do not skip straight to a build.

## Tests

Real-cgroup tests (per this subsystem's existing convention, e.g.
runner_linux_test.go's real-cgroup timeout-race tests) proving: (a) a CPU-bound
job that would exceed a wall-clock timeout under contention but stays under
its CPU-time budget is NOT killed; (b) a job that genuinely consumes its full
CPU-time budget (spinning, or real work) IS killed, with the same honest
kill/terminal-record shape a wall-clock timeout produces today; (c) the new
timeout source integrates with AIRA-126's arbitration without reintroducing
a race -- a CPU-time-budget-exceeded event racing the job's own clean exit at
the same instant must resolve the same principled way AIRA-126 already
resolves that race for the wall-clock source, not a hand-wavy 'good enough'.

## Resolution (in-review)

Plan: `docs/superpowers/plans/2026-09-06-aira136-cpu-time-timeout-plan.md`
(GATE-PASS-WITH-CONDITIONS; §0 of the plan maps each of C1-C8 to where it landed).

`aira run --cpu-timeout DURATION` bounds a run by the CUMULATIVE user+system CPU
time charged to its cgroup, read from `cpu.stat` -- the same pair the record
already stores as `cpu_user`/`cpu_sys`, so the number a kill was decided on and
the number in the record are the same quantity by construction.

The mechanism is deliberately NOT a second kill trigger. Both bounds are
multiplexed into ONE `deadlineSource` (`internal/runner/deadline_linux.go`) that
emits at most one value, so Launch keeps exactly one deadline branch, exactly one
`killWithIntent` call site, and AIRA-126's kill/terminal arbitration is reached
identically for either bound -- the only change inside that block is the literal
`"run-timeout"` becoming `fired.Actor`. The executed multiplexing revert (below)
shows what the rejected shape actually costs.

Sampling is 100ms, its own constant, chosen on OVERSHOOT rather than cost
(interval x achieved parallelism, ~1.6 cpu-s on a 16-way box). Every mechanism
here errs the same way -- a coarse interval, a lost baseline, an unevaluated
sample -- and the error is always "fires later than the budget", never "kills a
job that had not reached it".

New codes: `E_RUN_CPU_TIMEOUT` (exit 3, the CPU twin of `E_RUN_TIMEOUT`; the gate
command lane maps both to `U_GATE_COMMAND_TIMEOUT`) and
`U_RUN_CPU_BUDGET_UNENFORCED` (exit 3), which says a REQUESTED budget was not
applied and covers two states stated in full in `codes.go` and spec §6.4: never
measured, or measured-breached-but-not-enforced. Only an EXECUTED CPU-budget kill
suppresses it, so a wall-clock kill whose final total also crosses the CPU budget
carries BOTH codes with `scope_kill.actor == "run-timeout"`, and an AIRA-126
arbitrated exit over budget carries it too. Suppressing on "the deadline fired"
would have made the record depend on sampler phase.

### Deferrals, filed not silent

- **`aira confine` gets no CPU bound** -- filed as **AIRA-138** (`relates`
  AIRA-136) in this PR, carrying the full analysis. confine has NO job deadline
  of any kind today: its wait is an unconditional `waitConfineCommand`, with no
  select, timer, kill trigger, kill-intent ledger or terminal CAS. Adding one is
  building confine's FIRST deadline-and-kill path in a supervisor with different
  kill semantics and deliberately no run ledger -- the whole AIRA-126 class of
  question again, in a second location. This is surfaced explicitly because the
  ticket title named `aira run/confine` and `aira confine` is where heavy suites
  on a loaded box actually run: **the owner can override this deferral before
  merge rather than discover it after.**
- **`--cpu-timeout` with `--detach` is REFUSED** (`E_RUN_ARGUMENT_INVALID`), not
  silently ignored, until AIRA-131 gives the detached branch AIRA-126's
  arbitration. A bound the operator asked for and did not get is a fake pass.

### Evidence

Executed reverts, both run rather than read (plan §7.6 has the full named lists):

- CPU wiring neutered: exit 1, nine named AIRA136 failures including the
  scenario (b) spinner kill, the cumulative-across-the-scope row, and both
  arbitration tests' non-vacuity counters.
- Multiplexing only reverted (bare wall timer + an independent CPU-kill
  goroutine): exit 1 on `TestAIRA136DeadlineBranchTakenOnceUnderBothBounds`, and
  the mutant did not merely mis-attribute the kill -- it returned
  `U_RUN_RECONCILE_REQUIRED: kill intent won before terminal evidence` with a
  NON-TERMINAL `running` record. That is the AIRA-126 hazard reproduced in the
  new shape.

The build also corrected one incomplete claim in the plan: `cmd/aira/mcp.go`
hand-maintains a default for every `run` argument so the MCP and CLI faces
construct an identical `core.Request`, and the plan's "generated surfaces need no
separate edit" missed it. The existing MCP/CLI parity tests caught it. Recorded
in plan §6.1 rather than quietly fixed.

Accepted coverage gaps are plan §9 (sampling overshoot; sub-interval budgets;
baseline skew; confine and detach; simultaneous breach; no-kernel-cgroup
enforcement; contention itself not reproduced in tests).
