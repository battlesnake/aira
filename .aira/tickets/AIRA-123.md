---
{"schema":1,"id":"AIRA-123","project":"aira","title":"aitest worker-admit: degrade to ledger-only admission when no cgroup sub-scope is available","status":"in-review","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","aitest","ci"],"hold":false,"relations":[]}
---
Requested by the owner via peer session 'deploy', 2026-09-06, correcting AIRA-121's
requirement 7 before it was built.

## What requirement 7 got wrong (as a final answer, not as an interim one)

AIRA-121 req. 7 says shim mode must not export AIRA_AITEST_LIB on
--delegate-ram, because today's worker-admit hard-requires a real nested
cgroup sub-scope per worker and none exists in shim mode. That is correct as
a statement about the CURRENT implementation, but wrong as a permanent design
conclusion: it collapses two different things worker-admit currently does
into one. Cgroups give ENFORCEMENT (a kill backstop); a ledger gives
ADMISSION (queue/allow based on a declared-vs-available RAM budget), and only
the first genuinely needs a cgroup. An in-daemon ledger summing admitted
per-worker reservations against a total RAM budget already prevents
over-subscription -- the actual cause of most real OOMs -- without needing
enforcement at all.

That trade is much better on a CI runner than on Mark's shared desktop box:
single-tenant, disposable VM, no desktop to protect, no sibling sessions to
collaterally kill, so losing the kill backstop costs one job rerun, not
someone else's in-flight work. And the ledger's input (total system RAM) is
CLEANER on a dedicated single-tenant VM (a flat number) than reasoning about
a shared slice cap under a dozen contending sessions. Falling back to plain
pytest-xdist -n auto (unset AIRA_AITEST_LIB, per-worker RAM invisible to
anything) gives up real value: this project's own AIRA-11 class of incident
is exactly a heavy parallel leg spiking RAM with no per-test awareness --
observed for real, a local engine leg peaking ~35GB RSS.

## What to build

Extend `worker-admit` (the daemon RPC aitest's supervisor.py calls to admit
each forked worker) to support a DEGRADED mode: when no cgroup v2 subtree is
available (shim/CI mode, per AIRA-121), still make a real ADMISSION decision
-- admit now or make the caller wait -- based on the worker's declared RAM
need against an in-daemon ledger of currently-admitted workers' declared
needs vs the outer job's total RAM budget (the same shim-mode ledger AIRA-121
already builds at the job level; this is that same primitive one level
deeper, per worker) -- but do NOT attempt to create/nest a cgroup sub-scope,
and do NOT claim one exists. supervisor.py's worker.py side needs the
equivalent of today's granted-scope-path handling to instead accept an
admission-only grant (no scope_path, or a distinct 'admitted, no scope'
marker) and skip the cgroup-dependent pieces (place_self's cgroup.procs
write, the memory.current/memory.max watermark read in _should_recycle --
time/test-count recycling still applies, exactly like today's daemon-down
path) while still being GOVERNED by admission (unlike daemon-down, which is
fully ungoverned and only reached when the daemon itself is unreachable).

Once this exists: AIRA_AITEST_LIB should be exported in shim mode's
--delegate-ram path whenever this degraded backend can actually function --
NOT flatly withheld. Update AIRA-121 requirement 7 to this conditional rule
once this ticket lands (or build them together if that is the more natural
shape -- decide at build time; they are tightly coupled but this is real
Python+Go cross-boundary work in a different subsystem (aitest's supervisor,
not confine/install), so keeping it a separate ticket avoids ballooning
AIRA-121's already-large scope).

## Honesty requirement (same discipline as AIRA-121 throughout)

Every place that currently reports a worker's cgroup-backed state (any
diagnostic surface reading scope_path, memory.current, placement status) must
distinguish 'admission-only, no enforcement backstop' from a real granted
cgroup sub-scope -- never let admission-only look like the real thing.

## Interim state until this lands

AIRA-121 requirement 7's 'leave AIRA_AITEST_LIB unset in shim mode' stands as
the correct INTERIM behaviour (a broken backend -- today's cgroup-requiring
worker-admit with no cgroup -- is worse than plain xdist), but is a deliberate
DEFERRAL pending this ticket, not the intended end state. Do not let a future
reader treat requirement 7 as case-closed.

## Resolution (in-review)

Built on `aira121-ci-shim-mode` (PR #72), which is where ci-shim mode and the
job-level ledger live and which is NOT yet on master; this branch is stacked on
it. Plan:
`docs/superpowers/plans/2026-09-06-aira123-worker-admit-ledger-only-plan.md`.

### What was built, against each requirement

**Degraded worker-admit** (`internal/daemon/worker_admit_shim.go`). In ci-shim
mode `evaluateWorkerAdmit` delegates to `evaluateShimWorkerAdmit`, which makes a
real admit-or-wait decision and creates/names NO cgroup: the container's live
usage comes through AIRA-121's existing `readShimMemory` seam, and an in-daemon
`shimWorkerLedger` sums currently-admitted workers' DECLARED needs against
`budget − headroom`. The live-usage check and the ledger check are separate, not
summed (they overlap, and summing would double-charge every settled worker) —
the same split the real path makes. The check and the booking happen under one
lock hold, so two concurrent requests cannot both grant against the same
pre-booking sum. The ledger is container-wide, not per job: one container, one
budget. Every verdict class mirrors the real evaluator's, so an unreadable
ledger is retriable (`ledger-budget-unreadable`, contended), a full ledger is
retriable (`ledger-budget-exceeded`, contended) and an impossible request is
terminal-but-daemon-healthy (`exceeds-ceiling`, request-invalid).

**Caller side** (`supervisor.py`, `worker.py`). `aitest-bootstrap` now SUCCEEDS
in shim mode, reporting `outer=ci-shim admission=ledger-only` and no
supervisor_scope. `fork_worker(None)` skips `place_self`; `spawn_worker` skips
the placement ack for a scope-less grant (the ack exists to PROVE a cgroup join
— with no cgroup there is nothing to prove, and reusing its failure path would
produce `WorkerPlacementFailed`, i.e. "the local cgroup mechanism is broken",
a diagnosis this mode cannot support); `_should_recycle` already skipped the
watermark for a None scope and still applies time/test-count recycling.
Admission, the relay lease, retirement and dispatch are otherwise unchanged —
which is exactly the difference from the daemon-down fallback pool, which is
also UNGOVERNED.

**Honesty requirement.** A required `containment` token now rides every granted
line in both grades, and it decides which other keys are required AND FORBIDDEN:
an enforced grant carries `scope`+`memory_max` and no `reserved`; an advisory one
carries `reserved` and NO `scope`, NO `memory_max` (absent, not zeroed — a zero
would be a value, and every consumer tests `scope` for presence). One shared
validator, `workerAdmitGrantShapeProblem`, is called by the renderer, the parser
and the client, so producer and consumer cannot drift. An absent grade is a
contract violation, never a default — absence must not read as the strong claim.
The values reuse `aira confine`'s own `enforced` /
`advisory(ci-shim,no-cgroup,no-kill-backstop)` strings so an operator reads one
vocabulary. `aitest-bootstrap` states `admission=` on BOTH paths, and
supervisor.py refuses a grant whose grade disagrees with the backend the run
bootstrapped into, in both directions, so containment cannot be silently
downgraded mid-suite; a supervisor that has not bootstrapped expects the ENFORCED
backend, which is the fail-closed default. Ledger-only grants report
`cpu_slots=unevaluated` (the CPU gate counts populated worker cgroups, which
cannot exist here) and the suite prints one LEDGER-ONLY warning on its own
stderr.

**Mode agreement.** `outer_scope` is the ci-shim sentinel exactly when the CLIENT
resolved shim mode; a mismatch in either direction is refused terminally
(`confine-mode-mismatch`, admission-unusable), because waiting cannot make two
durable install-mode records agree.

**AIRA-121 requirement 7 updated** (in this commit): the rule becomes
"export when the backend can function", the condition is met, and
`AitestBackendCanFunction(mode) (backend, ok)` — still the one gate, one home —
is true for ci-shim. `AIRA_AITEST_OUTER_SCOPE` is deliberately still not
published; the non-delegate arm keeps its unconditional strip. Ticket test (f) is
inverted and still proves itself from the actual child environment.

### What is deliberately NOT bought, recorded rather than hidden

- No kill backstop, no memory-watermark recycling: a ledger-only worker that
  grows past what it declared is not killed and not recycled. The container's own
  OOM killer is what is left. Right on a single-tenant CI runner, wrong on a
  shared desktop, which is why it is reachable only in ci-shim mode.
- The lease is the CONNECTION, unlike the real path's cgroup-backed ledger
  (AIRA-41 moved deliberately away from connection scoping). There is no cgroup
  to charge and the daemon never learns the forked worker's pid, so a killed
  relay frees its booking early. Bounded by the live-usage term, which still sees
  the un-booked worker's RSS on the next admission.
- The ledger does not survive a daemon restart. Same mitigation; weaker than
  AIRA-74's real-path reconstruction, which had `memory.max` values to
  reconstruct from.
- No per-worker CPU governance in shim mode (structurally needs worker cgroups);
  reported `unevaluated`, never as a pass.
- Sizing aitest's `auto` worker count from the shim ledger stays deferred
  (AIRA-121 requirement 5's own noted follow-up).

## Review round 1 — findings and what was done

Build-review of PR #73 at `2adffc5` confirmed the code satisfies every
requirement above and that the load-bearing tests are non-porous. It raised two
BLOCKING items and four non-blocking ones. All six are addressed here except the
merge-target one, which is not this branch's to fix.

1. **BLOCKING (defect) — `.aira/tickets/AIRA-121.md` frontmatter `status` was
   flipped from `in-review` back to `planned`.** FIXED. Merging as-is would have
   silently reverted PR #72's own ticket-status record. The requirement-7 text
   edit in that file is in scope and correct and is unchanged; only the
   frontmatter regression is undone, so this branch now touches AIRA-121's body
   and not its status.
2. **BLOCKING (merge target) — the base is `aira121-ci-shim-mode` (PR #72,
   still open), not `master`.** NOT FIXED HERE, deliberately: the two available
   fixes are to merge #72 (not this agent's to do) or to rebase this branch onto
   master carrying #72's ~5.4k lines, which would land AIRA-121's code through
   AIRA-123's PR and bypass #72's own merge record — the same class of dishonesty
   as finding 1, one size larger. The correct sequence is unchanged and stated in
   the PR body: merge #72 first, then this branch retargets to master (GitHub
   retargets automatically) and rebases. Until then this PR is **not mergeable to
   master** and AIRA-123 must not be marked done on master.
3. **Coverage gap — the daemon→Go-client JSON hop was untested for the ADVISORY
   grade.** CLOSED. `internal/runner` may not import `internal/daemon`, so the
   two grant structs are independent declarations and a tag renamed on one side
   only would have turned every shim grant into a client-side contract violation
   with no failing test. Both halves are now pinned to the WIRE BYTES rather
   than to a struct round-trip: a raw-literal advisory acceptance case (plus four
   contradictory advisory payloads still refused) in
   `TestRequestWorkerAdmitClassifiesEndToEnd`, and raw-key assertions on
   `containment` / `reserved` — and the ABSENCE of `scope_path` / `memory_max` —
   in `TestShimWorkerAdmitConnectionReleasesTheBookingWhenThePeerDisconnects`.
   Verified by renaming each side's tag in turn: each rename fails its test.
4. **Coverage gap — no real-binary end-to-end run in shim mode.** RECORDED, not
   closed, under a new "Accepted coverage gaps" heading in the plan, with what
   IS covered per hop, the one seam none of those hops pins (the real CLI
   relay's argv/stdin wiring in shim mode), and why closing it is its own piece
   of work.
5. **Test overclaim — `test_ci_shim_worker_death_before_any_result_is_not_a_
   placement_failure` never killed a worker.** FIXED, by splitting it. It is
   renamed `test_ci_shim_worker_registers_with_no_placement_handshake` and its
   docstring now claims only what it proves (still non-porous: the shipped
   pre-AIRA-123 code raises `WorkerPlacementFailed` from `place_self(None)`). The
   untested claim is now a real test —
   `test_ci_shim_worker_killed_before_any_result_takes_the_mid_run_death_path` —
   which SIGKILLs a real forked ledger-only worker, waits for the death on its
   own fds and drives `_service_ready_workers`, asserting the worker is retired,
   `daemon_available` stays True (containment not stripped), the undispatched
   nodeid stays queued, and a replacement is requested. Non-porosity verified:
   removing `_describe_worker_death`'s scope-None guard fails this test with
   `TypeError: expected str, bytes or os.PathLike object, not NoneType` while the
   registration test still passes.
6. **Stale comment — `cmd/aira/main.go`'s AIRA-121 gate comment still said the
   verb "must fail CLEANLY and immediately".** FIXED. It now states the C1
   reasoning that still holds (never reach `CurrentCgroupPath`'s self-discovery)
   and says explicitly that AIRA-123 replaced the failure disposition with an
   honest degraded success.

Validation after the fixes, from this worktree, exact exit codes:
`aira confine -- go build ./...` 0; `aira confine -- go vet ./...` 0;
`AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` 0.
