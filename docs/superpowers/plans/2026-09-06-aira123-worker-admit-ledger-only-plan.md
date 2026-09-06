# AIRA-123 — worker-admit: ledger-only admission when no cgroup sub-scope exists

Status: built. Stacked on AIRA-121 (`aira121-ci-shim-mode`, PR #72), which is
where ci-shim mode, the install-mode record and the job-level RAM-budget ledger
live. AIRA-121 is NOT on master at the time of writing, so this branch is based
on it; see "Dependency" below.

## The separation this ticket makes

`worker-admit` did one indivisible thing: create a kernel-enforced cgroup
sub-scope for a forked pytest worker and grant against the sum of those scopes'
caps. That is two jobs wearing one coat.

- **Enforcement** — a kill backstop when a worker exceeds its cap. Needs a cgroup.
- **Admission** — allow or queue, against declared-need-vs-available-budget.
  Needs a ledger. Needs no cgroup at all.

Over-subscription (several workers admitted whose declared needs together exceed
available RAM) causes most real OOMs, and admission alone prevents it. AIRA-121
refused every shim-mode worker-admit because the two were fused; this ticket
splits them and keeps the half that works.

## What is lost, stated first

Nothing kills a ledger-only worker that exceeds what it declared. It is not
recycled on a memory watermark either (that read needs `memory.current`/
`memory.max`). The container's own OOM killer is what is left.

That trade is right on a single-tenant CI runner — disposable VM, no desktop to
protect, no sibling sessions to collaterally kill, so losing the backstop costs
one job rerun — and wrong on a shared desktop, which is why it is reachable only
in ci-shim mode. The alternative it replaces is worse for the target case: with
`AIRA_AITEST_LIB` withheld, a consumer's guarded conftest.py falls through to
plain pytest-xdist, where per-worker RAM is invisible to everything and nothing
prevents over-subscription at all (this project's own AIRA-11 incident class —
a parallel leg observed peaking ~35 GiB RSS with no per-test awareness).

## Design

### 1. Daemon: `evaluateShimWorkerAdmit` (internal/daemon/worker_admit_shim.go)

Mirrors the real evaluator's order and verdict classes so a reader comparing the
two sees exactly one difference: nothing here creates, reads or names a cgroup.

1. Live reading through the existing `readShimMemory` seam (container cgroup
   `memory.current` when install recorded one, else `/proc/meminfo`, clamped to
   the recorded budget). Failure → `unevaluated`/`contended`/
   `ledger-budget-unreadable`, i.e. **retriable**: a shim daemon refuses to
   start without a positive recorded budget, so a failed reading is transient.
2. `ceiling = budget − workerAdmitHeadroom`.
3. `estimated > ceiling` → `denied`/`request-invalid`/`exceeds-ceiling`.
4. Live-usage check `estimated + used ≤ ceiling` → else `insufficient-headroom`
   (contended).
5. Ledger check + booking under **one lock hold** (`shimWorkerLedger.admit`) →
   else `ledger-budget-exceeded` (contended).

The two checks are separate, never summed: they measure overlapping things (a
worker grown into its booking is in both), and adding them would double-charge
every settled worker. The real path splits them for the same reason.

**Lease lifetime = the connection.** This is the one deliberate divergence from
the real path, whose ledger is pointedly *not* connection-scoped (AIRA-41: a
killed relay must not free capacity while its worker lives under an intact cap).
AIRA-41 could rely on the cgroup as a better source of truth; there is none here,
and the daemon never learns the forked worker's pid. So a killed relay does free
its booking early — a named weakness, bounded by the live-usage term, which still
sees the un-booked worker's RSS on the next admission.

**Restart**: the in-memory ledger does not survive one. The live-usage term is
again the mitigation. Weaker than AIRA-74's real-path reserve reconstruction,
which had cgroup `memory.max` values to reconstruct from.

**CPU governance**: `cpuSlotsDecide` counts populated worker cgroups, which
cannot exist here, so grants report `cpu_slots=unevaluated` and supervisor.py
says so once. Honest, not silent.

### 2. Mode agreement, both directions

`outer_scope` is the ci-shim sentinel exactly when the *client* resolved shim
mode. A mismatch → `unavailable`/`admission-unusable`/`confine-mode-mismatch`,
terminal, because waiting cannot make two durable records agree.

### 3. Honesty: the `containment` token

Required on **every** granted line, both grades, and it decides which other keys
are required *and forbidden*:

| grade | carries | must not carry |
|---|---|---|
| `enforced` | `scope`, `memory_max` | `reserved` |
| `advisory(ci-shim,no-cgroup,no-kill-backstop)` | `reserved` | `scope`, `memory_max` |

One shared validator (`workerAdmitGrantShapeProblem`) is called by the renderer,
the parser and the client, so producer and consumer cannot drift. An absent grade
is a contract violation, never a default: absence must not read as the strong
claim. The values are the same strings `aira confine` already prints for a whole
job, so an operator reads one vocabulary.

`aitest-bootstrap` reports `admission=cgroup-sub-scope` / `admission=ledger-only`
on both paths for the same reason, and supervisor.py refuses a grant whose grade
disagrees with the backend the run bootstrapped into — so containment can never
be silently downgraded mid-suite. A supervisor that has not bootstrapped defaults
to expecting the *enforced* backend, which is the fail-closed direction.

### 4. Python: caller side

- `fork_worker(None)` skips `place_self`. "No scope" becomes a first-class input
  rather than a `None` crashing into `place_self` and being reported as a
  placement FAILURE (which strips containment for the whole run).
- `_should_recycle(None, …)` already skipped the watermark; comment extended.
- `spawn_worker` skips the placement ack for a scope-less grant. Deliberate: the
  ack exists to *prove* the cgroup join happened; with no cgroup there is nothing
  to prove, and reusing its failure path would produce `WorkerPlacementFailed` —
  "the local cgroup mechanism is broken" — a diagnosis this mode cannot support.
  A ledger-only child that dies at startup is handled where every other mid-run
  death is. The ack block moved to `_await_placement_ack` unchanged.
- Everything else — admission, the relay lease, retirement, recycling by
  time/test-count — is unchanged, which is the difference from the daemon-down
  fallback pool: that one is also *ungoverned*.

### 5. AIRA-121 requirement 7

`AitestBackendCanFunction(mode) (backend string, ok bool)` — the one gate, now
returning which backend serves the mode. True for ci-shim, so `--delegate-ram`
publishes the aitest coordinates. `AIRA_AITEST_OUTER_SCOPE` is deliberately *not*
published (there is no outer scope; the shim bootstrap branch answers with the
sentinel itself). The non-delegate arm keeps AIRA-121's unconditional strip.

## Deferred

- Sizing aitest's worker `auto` count from the shim ledger (AIRA-121 req 5's own
  noted follow-up). Concurrency here is still bounded by the requested worker
  count.
- Reconstructing the ledger across a daemon restart. There is no kernel object to
  reconstruct from; the live-usage term is the accepted mitigation.
- Per-worker CPU governance in shim mode. Structurally needs worker cgroups.

## Accepted coverage gaps

Written down and accepted rather than left silent (build-review, AIRA-123
round 1). Neither is closed by this change.

- **No real-binary end-to-end run in shim mode.** Nothing exercises a real shim
  daemon + a real `aira worker-admit` relay + a real `supervisor.py` together.
  The Python e2e
  (`test_ci_shim_ledger_only_admission_governs_the_whole_suite_without_a_cgroup`)
  drives stub bootstrap/admit scripts, and the real-daemon Go e2e tests in
  `internal/pylib/pytest_aitest_e2e_test.go` are gated real-cgroup-only, so they
  are structurally the wrong harness for a mode defined by having no cgroup.
  What IS covered is each hop separately: the daemon's evaluator and its wire
  frame (`internal/daemon/worker_admit_shim_test.go`), the daemon→Go-client JSON
  hop for both grades (`internal/runner/worker_admit_classify_linux_test.go`),
  the Go↔Python outcome line and its catalogues
  (`internal/pylib/worker_admit_channel_test.go`), and the supervisor's own
  handling of a scope-less grant. The residual risk is a seam none of those
  four pins: the real CLI relay's argv/stdin wiring in shim mode. Closing it
  needs an e2e harness that runs the real binaries with cgroups deliberately
  unavailable, which is its own piece of work.
- **The ledger is not reconstructed across a daemon restart** (also listed under
  Deferred). A restart forgets every booking; the live-usage term is what bounds
  the resulting over-admission window, and it is a weaker bound than the real
  path's, whose cgroups survive the restart.

## Tests

Go: `internal/daemon/worker_admit_shim_test.go` (grant shape; ledger really
gates + frees on release; live-usage denial with an empty ledger; over-budget
request terminal; unreadable ledger retriable; mode mismatch both directions;
connection-scoped release end to end; sentinel validation).
`internal/runner/worker_admit_outcome_test.go` (both grades round-trip; seven
refusal rows for contradictory shapes).
`internal/runner/worker_admit_classify_linux_test.go` (the daemon→client JSON
hop for an ADVISORY grant, fed as a raw wire literal so a renamed tag fails
rather than round-tripping through one struct, plus four contradictory advisory
payloads still refused). `internal/runner/confine_shim_linux_test.go`
(the child environment really carries a freshly published `AIRA_AITEST_LIB` and
no outer scope; the non-delegate strip survives). `internal/pylib/worker_admit_channel_test.go`
(containment + admission catalogues and their mapping held equal across Go and
Python).

Python: `internal/pylib/aitest/test_supervisor.py` — the ledger-only suite runs
daemon-backed with every worker admitted (counterexample: AIRA-121's shipped
behaviour finishes the suite on the fallback pool and fails `daemon_available`
and the admit count); grant carries an explicit `scope: None`; grade/backend
disagreement refused both ways; missing grade refused; advisory-with-scope
refused; bootstrap without a grade disables the daemon; a ledger-only worker
registers with no placement handshake; and a ledger-only worker SIGKILLed before
reporting anything takes the mid-run-death path — retired, replacement
requested, containment not stripped — rather than any placement diagnosis.

## Dependency — RESOLVED

AIRA-121 (PR #72) was open and unmerged when this branch was cut, so this branch
was stacked on `aira121-ci-shim-mode`. #72 merged as `8f134ee` on 2026-09-06;
this branch is now rebased onto `master` and targets `master`, which is what
makes it mergeable and AIRA-123 markable as done there.

The rebase picked up one AIRA-121 commit this branch had never seen, `019def9`
("review round 3 — F4 shim budget floor missing"), which floors a declared or
cgroup-derived shim budget at 4 GiB and re-sized the round-3 `readShimMemory`
tests to 8 GiB. It touches `internal/install/mode.go` and
`internal/daemon/shim_test.go`, both of which this change also edits; the rebase
merged cleanly and the whole gate was re-run against the rebased tree rather
than trusting the pre-rebase run.
