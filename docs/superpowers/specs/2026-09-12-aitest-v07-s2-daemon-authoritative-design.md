# aitest v0.7 Stage S2 — daemon-authoritative quota (design)

**Status:** DRAFT for owner review (2026-09-12). Owner-designed across a live design
dialogue; this document records that design and grounds it against the current code
(master `61745b9`, verified by a 7-agent read-only fact-find, run `wf_9a9f12da-c13`).

**Relationship to the S1 spec.** This supersedes the *S2* portions of
[`2026-09-12-aitest-v07-class-sized-workers-design.md`](2026-09-12-aitest-v07-class-sized-workers-design.md).
That document's class-sized-workers + client-side outer-cap model was a review-era
reversal of the owner's batch design; the owner has since reinstated the batch **and**
moved all quota authority into the daemon. S1's three landed slices (the `aira_mem`
reader, the measurement channel, and — with one asterisk, see §14 — the v7-1 guard)
stand. Everything S1 deferred to "S2" is re-specified here.

**The one-line reframe.** Quota is managed by the daemon **only**; `aira confine` and
`aitest` merely *request reservations*. With the daemon as the sole authority
serialising grants against the one machine-wide cap, over-admission is impossible by
construction — so the client-side machinery the review accreted (the aggregate guard,
the sibling-sum, the multi-supervisor "gap", the cross-process TOCTOU) is deleted, not
hardened. This is the operating form of "AIRA is primitives, not judgement."

---

## 1. Problem

The v0.6 delegate model sizes a `--delegate-ram` suite's outer cgroup scope to a
whole-subtree envelope (48 GiB default, or peak-RSS-history-sized) and nests every
pytest worker *inside* it. That envelope is held for the suite's entire lifetime even
though the suite uses a fraction of it most of the time — so a suite that peaks at
40 GiB blocks other jobs needing a few GiB for hours. Short-lived, individually-admitted
workers exist precisely to end that: each worker reserves only what it needs, when it
runs, and releases on finish.

Two latent hazards motivated the S1 work and are re-solved here, at the right layer:

- **AIRA-229** (whole-suite kill): because workers are nested under the outer scope,
  Σ(worker memory.max) is bounded only by that outer cap, and a breach fires the outer
  `oom.group=1`, group-killing the whole suite. S1 patched this *client-side* (v7-1).
- **AIRA-232** (multi-supervisor): N supervisors under one outer scope each guard only
  their own Σ, so their combined Σ can still breach.

Both **dissolve** under the model below (§4, §11) rather than needing more guard code.

## 2. Global constraints

Copied verbatim from the project and the S1 spec; every task inherits these.

- **No cgo; one static Go binary.** Linux real-cgroup behaviour is the target; `ci-shim`
  (no cgroup) mode must degrade honestly (no scope, no `memory.max`, no kill backstop —
  admission is ledger-only/advisory there).
- **A check that cannot establish its result reports `unevaluated`, never a fake pass or
  zero.** All new reads are `honest()`-wrapped.
- **No backwards-compat obligation** (AIRA has no users/data): schema, protocol, and
  config may change freely. The daemon protocol bumps **11 → 12** here (§7).
- **`aira_mem` marker semantics** (from S1 §4.1): a test's `@pytest.mark.aira_mem("512M")`
  declares its **incremental** peak RSS on top of the worker warm-import baseline. The
  default is 256 MiB (still to be confirmed by measurement; see §12).
- Two-loop is mandatory for the correctness-critical slices here (topology, ledger,
  OOM detection): Opus builds, Fable gates and build-reviews, every guard mutation-verified.

## 3. The model

**Invariant (the whole point).** `aira.slice` is the one cap. Every reservation on the
machine — each short-lived worker, each confine job — is requested from the daemon and
charged against that one signed ledger, released on the holder's socket EOF. The daemon
serialises grants, so `Σ(live reservations) ≤ ceiling` holds by construction; no client
counts anything. (The ledger is *already* this today — see §4.)

The model is the following seven mechanisms, each detailed below:

1. **Sibling worker scopes** under the slice, not nested under the parent (§4).
2. **`--delegate-ram` = an ordinary confine job** that publishes aitest coordinates and
   gets sibling workers; no bespoke delegate envelope (§4).
3. **Worker-held batch dispatch + prune-to-fit + ~10 s age cap** (§5).
4. **Non-blocking try-acquire** that returns available headroom on refusal (§6).
5. **Peak-vs-reservation under-reservation warning** (§7).
6. **OOM-penalty phantom reservation** as machine-wide back-pressure (§8).
7. **A ledger-only `aira confine` verb** to release the phantoms (§9).

## 4. Sibling worker scopes; `--delegate-ram` is an ordinary confine job

> **Refined by GATE-1 (§16).** Nesting silently provided four things the sibling model must
> provide explicitly — unique naming/enumeration, kill propagation, escape-attestation
> containment, and sub-reservation marking. §16 specifies each; read it alongside this section.

**What is already true (do not rebuild it).** Since v0.6 S15 the daemon already treats
each worker as a *separate signed-ledger lease against the one slice ledger*
(`worker_admit.go` resolves `DefaultConfineSlice`, enqueues the worker as an ordinary
waiter, releases it on the relay connection's EOF). The old outer-scope aggregate scan
was deleted. The `--delegate-ram` outer job already books only framework overhead on the
ledger (`ResolveConfineReserve`), **not** peak-of-children. So "each worker individually
reserved against the slice, no parent pre-reservation" is a property of the ledger today.

**What changes — the cgroup topology.** Today a worker scope is created *nested* under
the outer confine scope (`<outer>/.aira-worker-N`, via `CreateWorkerScope(outerScope, …)`),
so worker RAM charges hierarchically up to the outer cap and the outer `oom.group` is the
aggregate backstop. **Target: each worker scope is a sibling directly under `aira.slice`.**

This is *not* new machinery: an ordinary `aira confine` scope is already created as a
direct child of the slice (`confine_linux.go`), with the slice's controller delegation
and the common-ancestor `cgroup.procs` migration that a worker's `place_self` needs. The
change is to **create each worker via that ordinary confine-scope path under the slice**,
instead of as a child of the outer scope.

Consequences, all folded into the topology slice:

- **Parent↔worker linkage moves from topology to env.** Today the parent scope-id is
  recovered by stripping `.aira-` off the outer directory basename (feeds the exclusivity
  exemption). With siblings, the worker carries its parent's scope-id in an env var
  (`parent_scope_id`) instead.
- **Worker scopes are reaped and listed by construction** because they are ordinary
  confine scopes (the orphan reaper globs `.aira-CONFINE-*` and `confine --list` reads the
  ledger). *Do not* assert the old reaper covers a differently-named sibling — reuse the
  confine scope naming so coverage is automatic.

**`--delegate-ram` collapses to an ordinary confine job** (the greenfield minimum). A
`--delegate-ram` job is an ordinary confine job that additionally (a) publishes the aitest
worker-pool coordinates to its children and (b) has its workers placed as siblings. It
gets the ordinary confine reserve (peak-RSS-history-estimated, `--memory-reserve` /
`--memory-max` override, modest default), `memory.max = reserve`, `oom_score_adj = 500`,
1 declared core. This **deletes**:

- the 512 MiB pinned-overhead rule (`DefaultDelegateRAMOverhead`),
- the 48 GiB whole-subtree envelope and its history-sizing
  (`DefaultDelegateRAMScopeCeiling`, `resolveDelegateRAMScopeCeiling`),
- the `@dr` delegate oom-class `oom_score_adj = 800`,
- the `.aira-supervisor` drain + controller delegation in `aitest_bootstrap_linux.go`
  (it exists *only* because a cgroup-v2 scope may not hold both processes and
  controller-enabled children; a parent with no child scopes doesn't need it).

**The non-worker tree.** The parent scope now bounds the supervisor *and* whatever else
runs directly in the job (the `make`, the shell, a heavy compile/link step). Because the
parent is an ordinary confine job, its cap is the ordinary history-estimated reserve, and
`--memory-max` / `--memory-reserve` size it explicitly — so `aira confine --delegate-ram
-- make ci` sizes the parent for its heaviest *non-worker* step the same way any confine
job is sized. **Accepted first-run cost:** the peak-RSS history for a signature was
recorded against the *old whole-subtree* scope, so the first post-cutover run over-reserves
the parent once, then self-corrects as parent-only peaks are recorded. We accept the one
wasteful run rather than fold a cutover marker into the signature (AIRA has no users; a
single over-reserve on first run is cheaper than signature machinery). **This is the one
decision in §12 the owner was asked about; recorded as "parent = ordinary confine job".**

## 5. Worker-held batch dispatch, prune-to-fit, and the age cap

**As-built (the gap to close).** The current worker holds exactly one in-flight test and
lazy-pulls one nodeid at a time from a supervisor-side flat FIFO; there is no worker-held
batch, no same-class pull, no per-class sizing, and the age cap is 600 s. The only
queue-return path is a mid-test crash via `requeue_once` (front-insert, retry-once). The
"class-sized workers" section of the S1 spec was design intent; it was not built.

**Target dispatch.** The supervisor assembles a **batch** of nodeids for a worker and
sizes the worker's reservation as:

```
reservation  =  warm-import baseline  +  max(aira_mem over the batch)
```

`max`, not sum: the tests run one at a time inside the worker, so the worker's peak is the
largest single test on top of the baseline. The baseline is the measured warm-import
figure (S1 v7-4 measured ~11–12 MiB; confirmed per §12).

**Prune-to-fit (the batch's real job).** The supervisor issues a non-blocking admit for
that reservation (§6). If the daemon refuses, it returns the **available** headroom; the
supervisor **prunes** the oversized test(s) out of the batch — lowering `max(batch)` until
the reservation fits the returned headroom — forks the worker for what fits, and **kicks
the pruned test(s) back to the queue** for a later worker when the slice has room. The
prune can only target a number it was given, which is why the daemon returns available
(§6).

**The ~10 s age cap.** A worker runs its batch, checking its age **after each test**
(between tests only — never interrupting a running test, so a >10 s single test still
yields a real result, never `unevaluated`). If age > ~10 s it **returns the not-yet-run
tests to the master queue and exits.** Its job is turnover / cross-suite fairness, not an
RSS bound (the RSS bound is the per-worker reservation + the kernel `oom.group`). The
value flips from 600 s to ~10 s here (confirm against measurement, §12).

**One shared "return tests to the master queue" mechanism, three callers.** Prune
kick-back, age-cap leftovers, and OOM-kill loss all re-queue nodeids. The existing
`requeue_once` is generalised to:

- accept a **set** of nodeids (not one), and
- distinguish a **non-failure return** (prune / age-cap — the test never ran or was
  deliberately deferred) from a **crash return** (the worker died with the test in
  flight). A non-failure return must **not** consume the failure-retry-once budget;
  otherwise a test repeatedly deferred for sizing reasons is wrongly marked `unevaluated`
  after two deferrals with zero real failures.

## 6. Daemon protocol: non-blocking try-acquire (protocol 11 → 12)

The supervisor's worker admit becomes **non-blocking**: the daemon either grants the
reservation or refuses and **returns the current available headroom** for that slice, so
the supervisor can prune to fit (§5) rather than block a forked worker on RAM. This is the
try-acquire that v0.6 S15 removed, restored — and it is a wire change, so the daemon
protocol bumps **11 → 12**. (The signed ledger already supports a negative `available`, so
no ledger arithmetic changes; see §8.)

## 7. Peak-RSS-vs-reservation under-reservation warning

**Inputs already exist and are persisted.** The `aira confine` exit summary already
carries both `reserve=` and `peak-rss=`; the aitest supervisor already reads each worker's
`memory.peak` and oom flag at retirement; both are persisted to `confine_peak_history`
(peak, budget, budget-basis, oom, keyed by kind+signature).

**What is added — the under-reservation signal only.** At a job/worker finish, if the peak
RSS reached or exceeded the granted reservation (or the scope was oom-killed), warn to
stderr and record it (re-derivable at query time from `confine_peak_history`, the way
`aira confine --budget` already reads it — no new write plumbing; a stored boolean is one
nullable column if wanted). Because the parent and each worker are now ordinary scopes
with `memory.max = reserve`, "peak vs reservation" and the existing "peak vs cap ≥ 90 % /
OOM" advisory coincide — so this largely *reuses* the existing advisory rather than adding
a new comparison.

- **aitest attribution:** a worker runs several tests, so a near-cap/OOM worker's warning
  names **the batch's nodeids** ("one or more of [a, b, c] needs a larger `aira_mem`"),
  never a false-precise single test.
- **The over-provision (peak ≪ reserve) branch is dropped.** The owner asked only for the
  "increase it" direction; the wasteful-over-reservation warning is the only genuinely new
  comparison and is not built.

The db aggregate is the payoff: real runs self-report when a reservation was short, which
**calibrates** the still-unevaluated tunables (§12) from production instead of a synthetic
workload. Advisory only — it warns, never auto-resizes (auto-resize is the live actuator,
AIRA-178, owner-elevated and separate).

## 8. OOM-penalty phantom reservation (machine-wide back-pressure)

**Rationale.** Reservations are sized from markers/defaults and can under-estimate. When a
scope is OOM-killed, the naive retry would re-OOM at the same size. The daemon reacts to an
OOM by **reducing its own available quota**, creating headroom for a larger retry and
applying back-pressure while the machine is genuinely over-committed.

**Detection (new — the daemon has none today).** Today all OOM observation is client-side
at teardown, reported post-hoc and used only to bias the *next* admission of the same
signature. Add a daemon-side live detector: poll the **hierarchical `oom_kill` (and
`oom_group_kill`) counter on `aira.slice/memory.events`** — "any OOM anywhere in the slice
subtree", matching "if anything is OOM-killed". Delta-detect against a **baseline captured
at daemon start** (the counter is monotonic across the slice's life, which outlives the
daemon, so an un-baselined read would re-penalise on every boot). Watchdog / `cgroup.kill`
/ deadline kills never bump `oom_kill`, so a positive delta is kernel-OOM-only.

- **Own cadence.** Run the detector on its own always-on loop (or fold one read into the
  existing slice-ceiling loop). **Do not** gate it behind the watchdog or oomsteer modes —
  both are default-off and mean something else (that is the "a flag that means two things"
  smell).
- **Coarseness, owned.** A *worker's own-cap* OOM (an under-sized marker, not slice
  over-subscription) still bumps the hierarchical counter and so fires a slice-wide
  phantom. That is the owner's stated policy ("if *anything* is OOM-killed"); the spec owns
  that it is deliberately coarse — the peak-vs-reservation warning (§7) is the precise,
  per-signature signal, the phantom is the blunt machine-wide brake.

**The phantom reservation.** On a positive delta of N, add N phantom reservations of ~1 GiB
each. A phantom is a granted ledger lease with `reserve = 1 GiB`, `cpu = 0`, **no cgroup
scope and no process** — a pure ledger entry (the `newEstablishedWaiter` shape, inserted
the way the establish-granted branch inserts, which skips the ceiling and max-waiters
gates). It may drive `available` **negative** — which is supported by design and simply
makes further admission wait until live reservations release. Lifecycle:

- **Vanishes on daemon restart** (recovery re-declares live clients' leases onto an empty
  in-memory ledger; a phantom has no client, so it is never re-declared).
- **Exempt from drain/exclusive convergence.** `sliceProvablyEmpty` is `outstandingJobs == 0`;
  a phantom must carry a synthetic parent marker so it is *not* counted as a job, otherwise
  `--exclusive` and `drain wait` could never converge while back-pressure is active.
- It costs `reserve + one per-job headroom` and can flip a borderline new job to
  `E_ADMIT_TOO_LARGE` — that *is* the intended back-pressure.

**aitest pairing:** on a worker OOM-kill, the supervisor returns the worker's lost tests to
the master queue (the §5 mechanism) for a later, larger-reserved retry.

## 9. `aira confine` ledger-only release verb

Release is **operator-driven** — the daemon applies back-pressure mechanically but does not
*guess* when the cause is fixed; a human lifts it (primitives, not judgement). Add a thin
`aira confine` verb that releases the OOM-penalty phantoms via the daemon's existing
unconditional ledger-release path. It must be **ledger-only**: `aira confine --kill` opens
the scope directory and ENOENTs (a phantom has none), so the phantoms need a release that
touches the ledger and no cgroup. Home it on the `confine` family (the RAM-admission
surface), **not** `aira quota` (which is the unrelated compute/API-spend subsystem).

**Open (recommend no):** whether phantoms *also* auto-decay (time- or MemAvailable-based).
Recommendation: manual + restart only — simplest, and it keeps the daemon from
second-guessing the operator. Flagged for the owner in §12.

## 10. v7-1 guard excision — in the topology slice, not a follow-up

The S1 v7-1 client-side outer-cap guard is not a harmless leftover under this model: **it
becomes actively harmful the moment workers are siblings.** `_effective_outer_cap` reads
the min `memory.max` over the parent scope's ancestry — now the ~1 GiB parent cap instead of
the 48 GiB envelope — while `_sum_live_worker_caps` still sums every live worker against it.
With the S1 constants (base 64 / per-relay 8 / margin 32 MiB, 256 MiB request), the fourth
worker needs 768 + 88 + 256 = 1112 MiB > 992 MiB and is refused: the pool **silently
skip-ticks to ~3 workers on a 32-core box**, with no crash and no error — exactly the porous
failure shape the two-loop exists to catch.

Therefore the guard is **excised in the same slice as the topology change** (Q7 inventory
makes this a clean excision: its terminal dispositions flow through shared
`WorkerAdmitTerminal` handlers, so only one exception *source* disappears). The removal
also drops three `_emit_measurement_report` keys (else a `MEASURE_DIR` run `NameError`s) and
**relocates** one surviving test (`test_env_bytes_accepts_bare_zero_and_warns_on_garbage`,
which exercises the still-used `_env_bytes`). AIRA-229 and AIRA-232 are resolved by the
topology change, not by any replacement guard (§11).

## 11. AIRA-229 / AIRA-232 dissolve

- **AIRA-229** (whole-suite kill): the kill only existed because workers were sub-caps under
  a smaller-than-slice outer cap. Siblings under the slice have no shared parent cap to
  breach → no aggregate oom.group kill. The residual requirement — each reservation ≥ the
  worker's real peak — is kept honest by §7 and the measured default (§12).
- **AIRA-232** (multi-supervisor): N supervisors (or any mix of jobs) all reserve against the
  one slice via the one daemon, serialised → they cannot jointly breach. No client aggregate,
  no sibling-sum, no cross-process TOCTOU.

Both tickets are closed by the S2 topology slice with a note pointing here; neither needs a
guard.

## 12. Decision log

- **Parent cap under `--delegate-ram` = an ordinary confine job** (history-estimated reserve,
  `--memory-max` override, modest default), *not* a bespoke envelope. Owner was offered three
  options; this is simpler than all three and deletes the most code. **Overturnable at this
  gate.**
- **Refusal semantics = the daemon returns available headroom and the client prunes** (not the
  daemon fits the batch). Forced by the prune needing a target (§5, §6).
- **Phantom release = manual `aira confine` verb + natural clear on restart.** Auto-decay is
  an open recommend-no (§9).
- **OOM signal = hierarchical `oom_kill`+`oom_group_kill` on the slice** ("any OOM in the
  slice"), not the slice-local `oom` counter (§8); coarse by design.
- **Tunables still unevaluated** (carried from S1 v7-4; a heavy-allocation, many-test workload
  is an S2 prerequisite): the 256 MiB `aira_mem` default, the warm-import baseline, the ~10 s
  age cap value, the `MAX_TESTS` fork, and the phantom size (1 GiB) / cadence. §7's warnings
  will calibrate these from real runs once S2 lands.

## 13. Accepted gaps

- **Heavy-test starvation** (carried from the S1 spec's "OD3 Tier 2" note): a test whose
  `baseline + aira_mem` never fits the currently-free headroom is kicked back indefinitely
  under sustained load. We **document** this rather than build anti-starvation; the observable
  is the per-nodeid kick-back count surfaced in the §7 warning output, so the owner can decide
  later whether it warrants a scheduler.
- **First post-cutover run over-reserves the parent once** (§4) — accepted.
- **The OOM phantom is coarse** (§8) — a worker's own-cap OOM fires a slice-wide brake;
  accepted, with §7 as the precise complement.
- **CLOSED gap:** the S1 "supervisor RSS is an unreserved outer-cap charge" gap closes — the
  supervisor is now part of a properly-reserved ordinary parent scope.

## 14. What stands from S1, and the asterisk

- **v7-2** (`aira_mem` reader) stands — it is the sizing input §5 consumes.
- **v7-4** (measurement channel) stands — its reads feed §7 and §12.
- **v7-1** (outer-cap guard) is **removed in the S2 topology slice** (§10), not kept. So
  "S1 artifacts stand" carries this one asterisk: the guard was correct for v0.6 topology and
  is wrong for S2 topology.

## 15. Staging (S2 slices, ordered)

Each slice is an independently testable, two-loop-gated deliverable. Detailed tasks live in
the S2 implementation plan (writing-plans).

- **S2a — Topology + guard excision (the load-bearing correctness slice).** Place worker
  scopes as siblings under the slice via the ordinary confine-scope path; move parent↔worker
  linkage to env; collapse `--delegate-ram` to an ordinary confine job (delete the envelope /
  overhead / oom-class-800 / `.aira-supervisor` drain); **excise v7-1 in the same slice**
  (§10). Closes AIRA-229 and AIRA-232. Real-cgroup e2e gates.
- **S2b — Worker-held batch + prune + ~10 s age cap + generalised requeue** (§5). Protocol
  11→12 non-blocking try-acquire (§6).
- **S2c — Peak-vs-reservation under-reservation warning + batch attribution** (§7).
- **S2d — OOM detection + phantom reservation + the `aira confine` release verb** (§8, §9).
- **S2e — Heavy-allocation measurement workload** to fix the §12 tunables; then flip the
  confirmed defaults.

Build order is S2a first (it is the correctness fix and it makes the rest coherent); S2c/S2d
can follow in either order; S2e gates the final tunable values.

---

## 16. GATE-1 refinements (2026-09-12, Fable plan-gate)

The first plan-gate affirmed the architecture (the topology move is the right fix;
the ledger is already slice-authoritative; the cgroup-v2 drain reasoning is sound) but found
that the old *nesting* was silently doing four jobs. The sibling model must provide each
explicitly; these are design-level and supersede the lighter treatment in §4/§8/§12.

**(a) Worker scopes are first-class confine scopes.** A worker scope is created under the
slice with a unique, parseable, pid-bearing confine name (e.g.
`CONFINE-aitest-w<seq>-<supervisor-pid>-<stamp>`), **not** `worker-N`. Rationale: the id
allocator was keyed per-outer-scope (`workerScopeFor`/`allocateWorkerScopeID` re-seed by
scanning the *outer* dir); two `--delegate-ram` jobs under the one slice would both mint
`worker-1` → `EEXIST` → the reseed finds nothing → the second suite spins `contended` forever.
And `worker-N` is not parseable by `parseConfineScopeID`, so neither the orphan reaper nor
`confine --list` would see it. Fix: mint through the real confine grammar, key the counter on
the slice (or make ids unique by construction), and set the lease `scopeID` = the scope
dir-name-minus-`.aira-` so the reaper's `hasLiveLease` veto and `oomsteer`'s
`confineScopeDirName` line up. Workers thus become reaped / listed / killable like any confine
job — by construction, not by a bespoke path.

**(b) Kill propagation is the daemon's job, on peer-EOF.** `cgroup.kill` is subtree-recursive
only; with siblings, `confine --kill` / `--timeout` / Ctrl-C / the parent's own `oom.group`
kill the supervisor + relays but leave worker *processes* running in sibling scopes while
relay-EOF has already freed the ledger lease. Fix: the daemon `cgroup.kill`s **and** rmdirs a
worker scope on that worker relay's **peer-EOF** (`peerCtx.Done()`) — and **never on daemon
`s.stopping`** (a daemon restart EOFs every relay, but workers must survive and re-declare, per
the v0.6 reconnect contract). This is new daemon logic, not supervisor cleanup.

**(c) Escape-attestation must exempt the worker migration.** `monitorScopeMembership` witnesses
any process once seen in the parent's `cgroup.procs` and later alive outside the subtree as an
escape → it would stamp `scope-integrity=descendant-escaped` on essentially every delegate run
(a worker forks in the parent, then `place_self`s into its sibling scope). Fix: exempt a
migration **into a live-leased sibling worker scope whose `parent_scope_id` == this scope**
(positive identification, not a name-prefix guess). A delegate run must attest
`scope-integrity=contained`.

**(d) Workers must stay sub-reservations.** Today `workerParentScopeID` is always non-empty, so
`isSubReservation` holds and a worker never counts in `outstandingJobs` (headroom scaling,
`sliceProvablyEmpty`, drain / `--exclusive` convergence, `confine --list` "N jobs"). Moving
linkage to an explicit `parent_scope_id` field creates an empty-value path (the e2e harness and
ci-shim publish no scope id) that would make each worker a *job* — and the gates would pass
anyway. Fix: the daemon **refuses an empty `parent_scope_id`** (`E_DAEMON_PROTOCOL`) or
substitutes a synthetic non-holder marker; ci-shim publishes a sentinel; the supervisor sends
`AIRA_CONFINE_SCOPE_ID` (the *id*, not `self.outer_scope` which is a path).

**Decision-log updates (supersede §12 where they differ):**
- **`--memory-reserve` on a `--delegate-ram` job now behaves exactly as on any confine job**
  (sets the ledger reserve and, absent `--memory-max`, the parent `memory.max`). The old
  `--delegate-ram --memory-reserve 512M` idiom ("reserve 512 MiB framework overhead, workers
  separate") is **retired** — under the new model it would cap the parent (and its `make`) at
  512 MiB. The `internal/core/skill.go` prose and confine help that teach the old idiom must be
  updated. (Consequence of "delegate = ordinary confine job"; no compat obligation.)
- **§4's "first run over-reserves once" was wrong; corrected.** An ordinary history estimate is
  *never* fitted — over the ceiling it is **refused** terminally (`E_ADMIT_TOO_LARGE`); only the
  p90-prior/default is fitted. A signature whose *old whole-subtree* peak × safety ≥
  ceiling−headroom would get a **refused** first run. Fix: **namespace the parent signature** so
  a parent-only scope starts with fresh history (it genuinely measures a different thing — the
  supervisor/make tree, not the whole subtree), avoiding both the refusal and a stale
  over-estimate. (Replaces the "accept one wasteful run" note.)
- **Daemon-down fallback pool must cap at 1 under a finite parent cap.**
  `_spawn_fallback_worker` forks unconfined workers *in the supervisor's cgroup* = the parent,
  now reserve-sized; `NumCPU` fallback workers in a reserve-sized parent is a whole-suite
  `oom.group` kill by construction. Cap fallback at 1 when the parent cap is finite, or require
  `--require-admission`.

**Simplification taken (owner's hard rule).** With the drain gone, the `aitest-bootstrap`
subprocess has no job left but "echo outer + admission mode". Delete it: the launcher publishes
`AIRA_AITEST_OUTER_SCOPE` (→ `parent_scope_id`) and a new `AIRA_AITEST_ADMISSION`
(`cgroup-sub-scope` | `ledger-only`) at launch; remove the `aitest-bootstrap` verb,
`BootstrapAitestSupervisor`, `drainIntoScope`, `moveIntoScope`, `scopeHasFiniteMemoryMax`, and
the supervisor's `bootstrap()` subprocess. The supervisor runs directly in the parent confine
scope (legal: with sibling workers the parent has no controller-enabled children, so the
cgroup-v2 "no internal processes" rule no longer bites).

**Measurement channel** (§7 carry): `_emit_measurement_report` read `supervisor_scope/memory.peak`;
that scope is gone. Redirect to the parent's `memory.peak` (now supervisor-only) and drop the
`supervisor_scope=` token + `_cleanup_supervisor_scope`.

**e2e gates must not run on the production slice.** The harness daemon's default `sliceResolver`
resolves the real `aira.slice`; post-change, test worker scopes would land on the production
slice under production naming (forbidden by `cgrouptest`). Gates must use the `admitResolveSlice`
testing seam to point the daemon at the harness parent (with a finite `memory.max`), drive a
real alloc-and-hold testdata fixture (the existing fixtures allocate nothing or self-OOM), and
assert via a **deterministic** anchor (each granted `scope_path` from `pool-report.json` is a
direct child of the slice), not a "walk the tree while a worker is live" timing race.

### 16.1 GATE-2 refinements (2026-09-12, Fable re-gate)

GATE-2 affirmed the architecture again and confirmed the redraft closed the bulk (P1-3, P1-5,
all P2s, and P0-1/P1-1/P1-4 on the fresh-admit path). Two gaps remained, both on the
**daemon-restart path** — which the model's own §16b invokes but no gate exercised:

- **Restart-path parity (§16a + §16b, after a restart).** A worker relay re-declares its lease
  across a daemon restart keyed by `ScopePath` (a path), which would undo the dirname-key
  alignment §16a needs; and `serveReDeclare` reinstalls the lease with **no kill hook**, so a
  post-restart parent-kill re-orphans mid-test workers (§16b re-opened). Fix, no wire change:
  the relay re-declares with the key `TrimPrefix(Base(ScopePath), ".aira-")`, and
  `serveReDeclare` installs the same peer-EOF `cgroup.kill`+rmdir when
  `scopeID != "" && parentScopeID != ""` (that shape is uniquely a worker lease — a scoped
  ordinary admit carries no `parent_scope_id`). The S18 restart merge-gate asserts the
  re-anchored key-set (reds on the first half) and must gain a "parent kill after restart leaves
  no orphan" case (the second).

- **Escape-exemption mechanism (§16c) — local, not daemon-coupled.** "Keyed on the live lease's
  `parent_scope_id`" would require a daemon round-trip per membership sample and *still*
  false-flags on the kill path (relays die → leases release → workers live for the sub-second
  before the daemon kills them → teardown witnesses a live pid in an *un-leased* sibling →
  `descendant-escaped` on exactly the killed-run case). Fix: **mint the worker-name pid slot with
  the parent supervisor's pid** (`= os.Getpid()` of the monitor process, foreground and
  `--detach` alike). The exemption is then a purely local positive check —
  `parseConfineScopeID(basename)`: name prefix `aitest-w` **and** embedded `pid == os.Getpid()`
  — no daemon coupling, holding through teardown; a genuine escape to any other cgroup stays
  witnessed. Worker-admit validates `parent_scope_id` parseability (shim sentinel exempt).

**Consequences now mandatory (not optional):**
- **`confine --kill <supervisor-pid>` is ambiguous** across a delegate job's sibling workers
  (they embed the same supervisor pid) → would hit `E_SELECTOR_AMBIGUOUS`. Worker rows must be
  labelled / filtered from the default `--list`/`--kill` selector. (Was plan Task 10 Step 1,
  "decide"; now required.)
- The allocator's `(seq, parent-pid, time-stamp)` id is **unique by construction** → drop the
  reseed/`EEXIST` machinery entirely. The `@dr` marker's only consumers (`bindConfineScopeID`,
  the oomsteer class) both go → drop it.

**New accepted gap (§13): daemon-down parent-kill.** Nesting's `cgroup.kill` was
daemon-independent (workers were in the parent's subtree); sibling kill depends on the daemon
being alive at parent-death. If the daemon is down **and** the parent is killed **and** a worker
is mid-test, that worker survives until it finishes its current test and hits nodeid-pipe EOF
(the supervisor is gone), then exits on its own — a bounded "one more test" leak, not permanent.
Accepted for S2a. The §16.1 parent-pid slot makes a future reaper fix cheap (a *populated*
worker scope whose embedded pid is dead is positive orphan proof → a reaper-side `cgroup.kill`),
if the bounded leak ever proves to matter.

### 16.2 GATE-3 corrections (2026-09-12, Fable re-gate — then build)

GATE-3 confirmed every GATE-2 item closes on the code and the load-bearing pid identity holds
(the daemon mints the worker *name* but **copies the pid out of `parent_scope_id`**, never its
own — `bindConfineScopeID` guarantees that pid == the monitor's `os.Getpid()`). Two corrections
to §16.1, then this design is buildable (no fourth gate round — the build-review is the next
quality gate):

- **Escape attestation: the honest verdict is `unverified`, not `contained`.** `contained` is
  **leader-only** by the #20 descendant-escape attestation design
  (`2026-08-24-aira-descendant-escape-attestation-design.md`); a pytest supervisor always has
  relay descendants → `HadDescendants` → `classifyLaunchScopeIntegrity` returns `ScopeUnverified`
  under an observed teardown, by design. So §16c's "attest `contained`" is unattainable and
  wrong. The exemption's real, correct effect: the escape check precedes the `HadDescendants`
  rule, so **without** it a delegate run reads `descendant-escaped`/`migrated`; **with** it, that
  verdict is absent and the run reads `unverified`. Re-spec the goal as **"`unverified` with no
  `descendant_escape`; never `descendant-escaped`/`migrated`"** (the mutation check — drop the
  exemption, see the escape verdict return — still reds). The exemption lives in the **single
  `witnessedEscape` chokepoint** (it backs the sampler and both teardown paths and already holds
  `observation.Cgroup`), not in two places. **Builder hazard to avoid:** a permanently-red
  "expect `contained`" gate must NOT be "fixed" by weakening the #20 leader-only rule — the gate
  asserts `unverified`-without-escape, full stop.

- **The `serveReDeclare` kill hook must be anchor-gated, not bare-`peerCtx.Done()`.** The lease
  keeper's `adopt()` closes the previous connection before the re-declare exchange, and a late
  ack (> the 5 s `leaseKeeperExchangeGrace`) triggers a redial: conn1 is closed while conn2
  re-anchors the **live** worker lease. A kill keyed on bare EOF would then `cgroup.kill` a
  mid-test worker. A release is idempotent; a kill is not. Fix: `cgroup.kill`+rmdir **only when
  the anchored release actually discharged** — `releaseAdmitWaiterAnchored` already returns that
  bool (the ledger was hardened for exactly this old-conn-EOF-is-a-no-op ordering). **New accepted
  gap:** the reverse order (conn1's EOF processed *before* conn2's frame → a legitimate discharge
  → kill) re-runs that one test via the supervisor's crash/retry path, bounded by
  5 s × `maxNoAck`(=10); accepted.

*Authoritative for S2, as refined by §16, §16.1 and §16.2. The S1 spec remains authoritative for
S1 history and the `aira_mem` marker grammar.*
