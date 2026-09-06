---
{"schema":1,"id":"AIRA-136","project":"aira","title":"aira run/confine: express a job timeout in cumulative CPU-time, not just wall-clock","status":"planned","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["confine","runner"],"hold":false,"relations":[]}
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
