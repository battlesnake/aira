# AIRA-64 — machine-wide CPU-concurrency governance for aitest workers

Status: **plan v6 / BUILT** (3 plan-review rounds + 1 adversarial build review, all `BLOCK`ed and all applied)
Ticket: `.aira/tickets/AIRA-64.md` (P1, owner-escalated 2026-09-04)
Branch: `aira64-aitest-cpu-concurrency`
Base: `76095d5`

Review history for this plan is recorded in §12. Every v1 claim the reviews
falsified is corrected in place and named there; nothing is quietly dropped.

## 1. Problem

Two concurrent `make merge-gate` runs on this box produced, in one of them, an
82-minute engine leg (normally a few minutes) and 44 "NEW FAILURES" in code the
diff provably does not touch. Same night, a second reporter saw 10
`pytest-timeout` (>300 s) failures on heavy corpus tests that pass in ~90 s on a
quiet slice.

The failures are real in the sense that the tests genuinely did not finish
inside their wall-clock deadline. The **cause** is machine load, not a defect in
the code under test. This is a **false RED** of exactly the shape this project
has been hunting elsewhere: a black-box wall-clock signal that cannot
distinguish "broken" from "starved".

## 2. Mechanism, re-verified against source

Every claim below was checked against the tree at `76095d5`.

1. **aitest has no CPU-concurrency governance whatsoever.** `grep` over
   `internal/pylib/aitest/*.py` finds no CPU/slot/governor interaction. The only
   governance aitest performs is RAM, via `worker-admit`.

2. **`worker-admit` is entirely intra-job.** `evaluateWorkerAdmit`
   (`internal/daemon/worker_admit.go:385`) reads the request's *outer scope*
   `memory.current`/`memory.max`, sums `memory.max` over that scope's own
   `.aira-worker-*` children, and adds the supervisor scope's RSS. Every term is
   scoped to one confine job. **There is no cross-job coordination in this verb
   at all.**

3. **Each concurrent job independently sizes its pool at `NumCPU`.**
   `_resolve_worker_count` (`internal/pylib/aitest/__init__.py:112`):
   `"auto" -> max(1, os.cpu_count() or 1)`. On this 16-core box two concurrent
   merge-gates ask for **32 CPU-heavy worker processes on 16 cores**; three ask
   for 48. Nothing refuses.

4. **The one cooperative CPU governor that exists has no client.**
   `internal/daemon/governor.go` (`governorSet`) implements a machine-wide CPU
   active set: capacity `NumCPU - reserve` (`cpuslots.go:desiredCPUSlots`), a
   per-job floor, youngest-job-first above the floor, cooperative park/resume at
   test boundaries. It is **live in enforce mode on this box**
   (`AIRA_SCHED_MODE=enforce`). Its only ever client was the xdist plugin
   (`internal/pylib/aira_xdist_governor`) that aitest replaced.

5. **That plugin additionally disables itself in forked children.**
   `aira_xdist_governor/__init__.py:63`,
   `os.register_at_fork(after_in_child=_close_inherited_streams)` sets
   `_governor_disabled = True`.

6. **`pytest-timeout` is third-party and wall-clock.** aitest neither configures
   nor observes it; its deadline starts at `pytest_runtest_call`, i.e. after
   dispatch. The ticket's earlier "admission-wait counted against the timeout"
   hypothesis is therefore not the mechanism, as the ticket itself corrected.

**Conclusion: the defect is unbounded worker concurrency.** Bound it.

## 3. Scope

**In scope:** a bound on how many aitest workers run concurrently, enforced at
worker admission, with a liveness floor, plus the client-side changes §4.9 shows
are *required* for the bound not to be a performance regression.

**Out of scope (owner fork, §8):** anything that reinterprets, rescales, or
reclassifies a `pytest-timeout` failure.

## 4. Design

### 4.1 The decision: fold a CPU-concurrency dimension into `worker-admit`

`worker-admit` already is the "may I add one worker?" verb. It runs once per
worker spawn and once per replacement; it is polled on a 200 ms cadence with a
client-declared `max_wait_ms`; and it has a fully-specified structured outcome
channel (AIRA-42/45) whose `class=contended` disposition means exactly "not now,
keep trying, containment preserved".

So the CPU bound becomes **one more denial condition in `evaluateWorkerAdmit`**,
`state=denied class=contended reason=cpu-slots-saturated`.

### 4.2 Capacity

`desiredCPUSlots(runtime.NumCPU())` — the existing function in
`internal/daemon/cpuslots.go`, already used by the governor, already overridable
with `AIRA_DAEMON_CPU_RESERVE` (default reserve 1, floor 1). One capacity
concept for the machine, not a second one. On this box: `16 - 1 = 15`.

**Accepted gap (DeepSeek P1-4):** `runtime.NumCPU()` honours
`sched_getaffinity` but not a cgroup `cpu.max` quota, so inside a CPU-quota'd
container capacity would be overstated. This is *pre-existing* — the governor
sizes itself the same way — and is not made worse here. Deriving capacity from
`cpu.max` is a separate change to `desiredCPUSlots` affecting both clients;
recorded, not built (§10).

### 4.3 The live count is derived from the kernel, not from connections

**Decision: the live-worker count is derived by scanning the cgroup tree**, not
by tracking `worker-admit` connections. This is the source of truth AIRA-39 and
AIRA-41 deliberately moved the RAM ledger onto: *the kernel object IS the
state*. Consequences, all of them the reason for the choice:

- **Restart-safe by construction** — no over-admission window after a daemon
  restart, and no reconstruction machinery of the kind AIRA-74 needed.
- **A killed relay cannot free capacity while its worker still runs** — the
  exact bug AIRA-41 fixed for RAM. Connection-tied slots would reintroduce it.
- **A leaked relay cannot permanently consume capacity**, because capacity
  tracks directories, which `_retire_worker` removes.

### 4.4 Two counts, each failing in its own safe direction

**This is a correction forced by Sol P0-2**, which found that a single
directory-count would let one empty orphan directory disable the liveness floor
*permanently*. The daemon creates a worker scope **before** delivering the grant
(`worker_admit.go:611-652`), and a granted response whose write fails is
deliberately left on the tree (`worker_admit.go:851-870`); the client
independently converts an unresponsive relay into a retriable denial
(`supervisor.py:672-699`) while that empty directory remains. A floor keyed on
directory existence would then never open, and a zero-worker run would retry
forever (`supervisor.py:1277-1315`) — turning "slow" into "**stalled**", which
is precisely the regression §4.5's invariant forbids.

So the two questions get two different counts:

| question | count | fails toward |
|---|---|---|
| "how busy is the machine?" (the cap test) | **directory** count of `.aira-worker-*` under every `.aira-CONFINE-*` scope in the slice | over-count → **denies** growth → safe |
| "does this scope have any worker at all?" (the floor) | **live** count — `.aira-worker-*` children of *this* outer scope reporting `cgroup.events: populated 1`, gated by §4.4.3's `lastGrantAt` | under-count → **opens** the floor → safe |

An empty orphan therefore makes the machine look slightly busier (denying
above-floor growth, harmless) but can never block a job's floor worker.

**One further correction from the build review (§13(d)):** "the directory count
never undercounts" was only true if every confine scope can actually be read.
The first implementation skipped any scope whose listing failed, which made a
persistently unreadable **busy** scope look empty and let the gate admit past
capacity while still claiming `cpu_slots=ok`. Only a scope *proven* to have
vanished is skipped now; any other read failure makes the whole snapshot
`unevaluated`.

> **SUPERSEDED IN BUILD (v6) — read §4.4.3 first.** §4.4.1 and §4.4.2 below
> describe the *directory-age* mechanism that closed the grant-to-placement
> window. The build review falsified its justification a second time, and it has
> been **removed** in favour of daemon-owned state. The sections are kept because
> the reasoning that led there is what the reviews acted on; §4.4.3 is what
> shipped.

#### 4.4.1 The placement grace, and why the floor is not populated-only

A populated-only floor is **not** safe, as Sol's second round demonstrated: the
daemon creates the scope inside its own gate but the client places the child
*after* `acquire_worker` returns (`supervisor.py:949-970`), so between grant and
placement the directory reads unpopulated. §4.5 explicitly permits several
supervisors under one outer scope, so N supervisors paused in that window would
each see "zero live workers", each take a floor grant, and then all place —
exceeding the bound by `N-1` with no ordering constraint violated.

So a child counts as live when it is **populated OR younger than
`placementGrace`**, and `placementGrace` is pinned to the client's own
`_PLACEMENT_ACK_TIMEOUT_SECONDS` (60 s, `supervisor.py:58`). This is a
positive-proof-of-both-facets test in the same discipline as AIRA-36's reaper
(empty **and** PID-dead **and** aged). It closes both directions with one rule:

- a scope created moments ago and not yet placed is **young** → counted → no
  second floor grant (closes the round-2 P0);
- a scope abandoned by a lost grant is **old and empty** → not counted → the
  floor opens (closes the round-1 P0).

**What `mtime` actually means here — CORRECTED IN BUILD (v5).** v3 and v4 said
directory `mtime` "is the creation time". The real-cgroup test written for
§9.18 **refuted that on its first run**: a child-cgroup `mkdir` or `rmdir`
inside a worker scope moves its `mtime`. Measured precisely
(`TestWorkerScopeMtimeMovesOnlyOnDirectoryEntryChanges`):

| mutation | moves `mtime`? |
|---|---|
| population — a `cgroup.procs` write | **no** |
| a control-file write (`memory.high`, …) | **no** |
| a child-cgroup `mkdir` / `rmdir` | **yes** |

`mtime` is therefore *the time of the last directory-entry change in the scope*.
The gate needs exactly one property and these three deliver it:

> **An abandoned scope's `mtime` is frozen** — an abandoned scope holds no
> process that could create or remove a child inside it, so it genuinely ages
> out and the floor genuinely opens.

And the exception is one-directional in the safe sense: a **live** worker that
nests cgroups refreshes its own `mtime` and so looks *younger*, which counts it
live — and it is live. There is no mutation that makes an abandoned scope look
*older* than it is, which is the only direction that could break the bound.

Also corrected (Sol round 3 P2): v3 cited `ConfineRecord.AgeSeconds` as
precedent. That is wrong — it derives from the timestamp encoded in the scope
*id*, not from `mtime` (`confine_manage_linux.go:82-95`). The claim now rests
solely on the measurement above, which is a committed, executable test rather
than a note.

**Population that cannot be established** (an unreadable `cgroup.events`) counts
the child as **live** — the direction that does not fabricate an open floor.

#### 4.4.2 One floor grant per outer scope per grace window

Sol's third round showed the age gate alone still does not bound the total: the
grace runs from *scope creation*, but the client's ack timer starts only after
`acquire_worker` returns and the fork begins (`supervisor.py:949-994`). A
supervisor stalled longer than the grace between grant and fork lets its
directory age out while its grant is still valid, so a second supervisor under
the same outer scope becomes floor-eligible again — once per grace window,
repeatedly.

Two things address this, and neither is a claim protocol:

**(a) A per-outer-scope floor rate limit.** `workerScopeState` already exists
per outer scope and is already held under that scope's lock, so it gains one
field: the time of the last floor grant. **A floor grant is issued at most once
per outer scope per grace window.** Three lines, no new lifetime, no restart
story — after a daemon restart the field is zero, which permits exactly one
extra floor grant, i.e. the residual below and nothing worse.

**(b) The claim is retracted where it cannot be supported.** See §4.6.

**Residual, quantified honestly:** with (a), a stalled-before-placement
supervisor can draw **at most one extra worker per outer scope per grace
window**, and only while its own child has neither placed nor been killed —
which the client does at its own ack deadline. It is bounded per window, not
bounded over unbounded time. It is fully RAM-checked in every case.

**The grace is read from the environment, not assumed — and the two halves live
in different processes.** Sol correctly noted `_PLACEMENT_ACK_TIMEOUT_SECONDS`
is operator-adjustable via `AIRA_AITEST_PLACEMENT_ACK_TIMEOUT`
(`supervisor.py:75-95,991-994`). The daemon reads the same variable *name*, but
out of its own environment — it is a systemd service, not a child of the pytest
run.

**Corrected during the build:** v4 said this meant "the two halves cannot drift
apart silently". That is false, and the false version is the more dangerous one
to leave in a comment. Setting the variable for a pytest invocation does **not**
move the daemon's grace. Both default to 60 s, each is overridable where it
runs, and a mismatch is bounded in both directions:

| mismatch | consequence | bounded by |
|---|---|---|
| client grace **longer** than the daemon's | a scope still legitimately placing can age out daemon-side and draw one extra floor grant | §4.4.2's rate limit — one per outer scope per window |
| client grace **shorter** | an abandoned scope counts live slightly past the client's own kill | floor recovery is delayed, never withheld |

Neither direction breaks an invariant, so the mismatch is documented rather than
engineered away with cross-process plumbing.

#### 4.4.3 WHAT SHIPPED: the window is closed by daemon-owned state

The build review killed the age gate outright, and the way it died is worth
recording because it took two refutations.

1. §4.4.1 claimed directory `mtime` is creation time. The real-cgroup test
   refuted that on its first run: a child-cgroup `mkdir`/`rmdir` moves it.
2. The salvaged argument was "an abandoned scope has no process able to create
   children inside it, so its `mtime` is frozen". **That is also false, and the
   same test proves it**: the test process creates a child inside a worker scope
   it is not a member of. Cgroup membership does not govern who may `mkdir` in
   the directory — write permission does.

Rather than defend a third version of a filesystem inference, the inference is
gone. The floor is now:

> **`liveForFloor` = the count of this outer scope's `.aira-worker-*` children
> that report `cgroup.events: populated 1`** (an unestablished reading counts as
> live), **AND** the daemon has not granted under this outer scope within the
> grace window.

The second clause is `workerScopeState.lastGrantAt`, recorded on **every** grant
— not only floor grants, because a normal under-capacity grant opens the same
grant-to-placement window. It lives on a cell that already exists per outer
scope and is already held under that scope's lock at every decision point, so it
needs no lifetime, expiry, or restart story: after a restart it is zero, which
permits exactly one extra floor grant per scope and nothing worse.

This is strictly better than the age gate on every axis. It cannot be perturbed
by anything outside the daemon, it needs no timestamp semantics, it closes both
round-1's orphan hole and round-2's placement-window hole with one comparison,
and it **deleted more code than it added**. It is also, finally, the shape
`architectural-simplicity` was pointing at all along — the two review rounds
spent defending `mtime` were the detour.

### 4.5 The liveness floor — the most important invariant

> **Every outer scope with queued work is always entitled to at least one
> worker, regardless of CPU saturation.**

Without it the fix is a *regression*: a job arriving while another holds all 15
slots would get zero workers and stall until the other run finished. Today that
job merely runs slowly. **"Slow" must never become "stalled".**

Implementation: if the requesting outer scope's **`liveForFloor`** count is zero
— the single named predicate used everywhere in this plan and in the code,
defined in §4.4.1 as *populated OR younger than the placement grace* — the CPU
gate does not deny, subject to §4.4.2's once-per-grace-window rate limit.
(v3 said "populated count is zero" here, which contradicted §4.4.1; Sol round 3
P2. One predicate, one name, one definition.)

The floor is keyed on **outer scope**, matching AIRA-39's deliberate decision to
key the worker ledger on outer scope alone. Named consequence: two pytest
supervisors sharing one outer scope share one floor. In practice one `aira
confine` job runs one pytest invocation.

### 4.6 The guarantee, stated exactly (CORRECTED — v1 was false)

Sol P0-1 falsified v1's `max(capacity, jobs)` claim: admission cannot revoke an
incumbent's workers, so an incumbent that already holds `capacity` workers is
*added to*, not displaced by, each newcomer's floor grant.

**`J` is defined as the number of outer scopes in the slice holding at least one
`.aira-worker-*` child** — a quantity the snapshot itself observes. Sol's second
round rejected v2's "outer scopes with queued work": `_dispatch_to_idle_workers`
pops a nodeid before its worker finishes (`supervisor.py:1160-1164`), so a scope
can leave "has queued work" while still holding live workers, and the expression
degenerates at `J = 0`.

With `C` = capacity, the true worst case over all arrival orderings is:

> **live workers ≤ `C + max(0, J - 1)`**
> **— provided at most one supervisor is concurrently admitting under each
> outer scope.**

(one job reaches `C` while alone, then each of the other `J-1` jobs takes its
floor worker; at `J = 0` there are no workers at all). In the common ordering
where jobs arrive together it is `max(C, J)`.

**The proviso is a retraction, not a footnote (Sol round 3).** Where several
supervisors admit *concurrently* under one outer scope, §4.4.2's rate limit caps
the excess at one extra worker per outer scope per grace window, but that is
bounded per window rather than absolutely, so the inequality above is not
claimed for that configuration. It is not the deployed one: `aira confine` runs
one pytest at a time, and the *sequential* multi-suite re-run that
`BootstrapAitestSupervisor` explicitly supports
(`aitest_bootstrap_linux.go:22-29`) is not concurrent. A `make -j` launching two
pytest processes inside one confine job would be, and would get the weaker
guarantee. Stated rather than assumed away.

The corrected table, 16 cores, `--aitest-workers=auto`:

| jobs | today | after (worst case `C+J-1`) |
|---|---|---|
| 1 | 16 | 15 |
| 2 | 32 | 16 |
| 3 | 48 | 17 |
| 20 | 320 | 34 |

Oversubscription stops scaling with `#jobs × pool_size` and starts scaling with
`#jobs` **additively**. The 2-job case that produced the reported incident goes
from 32 workers on 16 cores to 16.

This is the "bounded over-subscription with a backstop" shape the work was asked
for, and the plan now claims exactly it and nothing wider.

### 4.7 Evaluation order, staleness and locking (CORRECTED)

Sol P1-5 found v1's ordering self-defeating: putting the CPU check *after* the
forced RAM rescan (`worker_admit.go:558-578`) would force a full RAM tree scan on
**every 200 ms saturated poll** — the AIRA-61 CPU-regression shape — since a
CPU-denied request has ample RAM and always reaches that line.

Corrected order inside `evaluateWorkerAdmit`, all under the existing per-outer-
scope lock:

1. existing cached-RAM checks → may deny;
2. **NEW: cached CPU check** → may deny `cpu-slots-saturated` and **return
   before any forced scan**;
3. existing forced RAM rescan → may deny;
4. **NEW: forced CPU check** under the gate mutex → may deny;
5. `CreateWorkerScope`, still inside both.

**One snapshot, two numbers.** A single machine snapshot carries the slice-wide
directory total *and* the per-outer-scope populated counts, refreshed at most
once per `admitConfineScanIntervalDefault` (1 s) for step 2 and forced fresh for
step 4. A stale-high total only denies (safe); a stale-low total merely lets the
request reach step 4, which rechecks. This is the same two-phase
cached/`force` pattern `workerCommitted` already uses, so its staleness argument
carries over.

**Lock order is `outer-scope lock → CPU-gate → CreateWorkerScope`, never the
reverse.** Acquiring the gate first would let one slow outer scope (whose own
lock is held across a cgroupfs scan and a scope creation) pin admission
machine-wide — the hazard `worker_admit.go:110-115` records for `admitSlots`.
Nothing acquires an outer-scope lock while holding the gate, so no cycle exists.

**The gate is abandonable** (DeepSeek P1-5): a 1-buffered channel selected
against `ctx.Done()` and `s.stopping`, exactly like `acquireWorkerScope`
(`worker_admit.go:164-188`), so a vanished peer or a stopping daemon never waits
on it. A gate acquisition that is abandoned returns the same "peer gone" path
the outer-scope lock already uses.

### 4.8 Scope of the guarantee is PER-SLICE, not literally machine-wide

Sol P1-6 / DeepSeek P1-2: `runner.listConfines` enumerates one `slicePath`
(`confine_manage_linux.go:69-80`) and custom slices are supported, so two jobs in
*different* slices would each receive a full `C`.

**Decision: state the guarantee as per-slice and say so, rather than build a
cross-slice authority.** The scan root is `filepath.Dir(outerScope)`, accepted
only if (a) `filepath.Base(outerScope)` carries the `.aira-CONFINE-` prefix — the
same membership test `confine_manage_linux.go:79` uses — and (b) the derived
root canonicalises through the existing `s.admitResolveSlice` seam, which also
closes DeepSeek P1-6's crafted-outer-scope concern. Otherwise §4.9 applies.

`aira install` bakes exactly one `aira.slice` and every `aira confine` job lands
in it, so **per-slice == machine-wide in the deployed configuration**. A
multi-slice deployment gets `C` per slice; that is written down here and in the
ticket, and is a follow-up, not a silent hole.

### 4.9 An unestablished reading, and how it is made visible

If the CPU reading cannot be established (not a `.aira-CONFINE-` child, slice
unresolvable, `readdir` failure), the CPU dimension is **`unevaluated`** and
admission proceeds on the RAM decision alone.

Deliberate asymmetry with the RAM checks, which fail *closed*: an unestablished
RAM reading risks an outer-scope `memory.oom.group` kill of an entire run —
catastrophic, so deny. An unestablished CPU reading risks over-subscription —
degradation, and exactly today's behaviour — so denying instead would convert a
diagnostic failure into a stall of every aitest run on the machine.

**Sol P1-7 is right that a daemon-journal-only warning can leave this
operationally inert**, and this project has shipped an inert governance
subsystem before (the AIRA-59 watchdog). So the state is made *observable to the
person whose run it is*:

- **one** new token, `cpu_slots=<ok|unevaluated>`, on the granted outcome line.
  The Python parser collects arbitrary `key=value` tokens and only *requires*
  the four grant fields (`supervisor.py:341,747-753`), so it is additive and
  cannot break the contract;
- the supervisor emits **one** stderr line per run when a grant reports
  `cpu_slots=unevaluated`;
- the daemon logs the condition once per outer scope.

Sol's round 2 correctly noted this must traverse every hop or it is inert:
daemon `WorkerAdmitResponse` → `runner.WorkerAdmitGrantFields` →
`WorkerAdmitOutcomeLine` → `worker_admit_client_linux.go`'s grant unmarshal and
lease → `supervisor.py`. All five hops are in scope, and test 20 asserts the
token end-to-end rather than at any single hop.

**Partial acceptance, recorded.** v2 proposed three fields
(`cpu_slots_state`/`live`/`capacity`); v3 ships **one**. `live` and `capacity`
are telemetry, and `architectural-simplicity` is explicit that telemetry-only
signals do not justify machinery — five hops of plumbing for two numbers nobody
branches on is exactly that. The one bit that changes a reader's conclusion
("was CPU governance actually applied to this run?") is kept. The *saturated*
case needs no new field at all: it already reaches the run's stderr as the
`cpu-slots-saturated` reason inside the existing `WorkerAdmitDenied` message.

A reading that could not be established is therefore never rendered as a zero,
never as a pass, and never silently.

### 4.10 Client changes — REQUIRED (v1's "no Python change" was false)

Sol P1-3 and P1-4 falsified v1's claim that the existing client already does the
right thing. Verified against source:

- `run()`'s startup pool loop **`break`s permanently** on the first denial
  (`supervisor.py:1807-1817`), and its follow-up wait only fires when the pool is
  **empty** (`:1835`). A run that gets 1 of 15 workers therefore **never grows
  again**;
- `_replace_worker` replaces **one** worker on a retirement (`:1349-1359`); it
  does not restore the pool to its target size;
- the idle `select()` tick does no growth work at all (`:1877-1883`);
- `_replace_worker` blocks on `self._run_max_wait` (30 s) even when other workers
  are alive (`:1351-1354`), and the daemon polls to that deadline
  (`worker_admit.go:772-845`). The supervisor is **single-threaded**
  (`:26-30`), so under CPU contention — where denials become the *common* case —
  every retirement would freeze dispatch for up to 30 s.

Without fixing these, this feature would make a contended run permanently
single-worker *and* stall its dispatch loop. Three focused changes:

1. **Speculative growth probe.** On **every** iteration of `run()`'s dispatch
   loop — not only the `select()` timeout branch — rate-limited to once per
   second by a monotonic deadline: if the daemon is available, queue work
   remains, `_pool_covers_the_queue()` is false and
   `len(self.workers) < self._run_worker_count`, attempt **one** speculative
   spawn. A denial is ignored; the next probe tries again. This is what makes
   "grows later" true, and it is what stops an incumbent monopolising its own
   recycled slots — the newcomer probes once a second.

   **Corrected from v2 (Sol round 2):** v2 probed only on the `select()`
   timeout. A run whose tests complete in well under a second keeps result and
   pidfd descriptors continuously ready (`supervisor.py:1877-1883`), so the
   timeout branch may never be taken and the run would never grow. Probing every
   iteration under a monotonic rate limit removes the dependence on being idle.

2. **Non-blocking replacement while the pool is non-empty.** `_replace_worker`
   uses a speculative request when `self.workers` is non-empty, keeping the
   indefinite `_wait_for_admission_or_disable` path **only** for the last-worker
   case where waiting is the honest thing to do.

3. **One-shot `cpu_slots=unevaluated` warning** (§4.9).

#### 4.10.1 "Speculative" needs a real zero-wait admission — two corrections

v2 said "`max_wait="0s"`". Sol's round 2 falsified that in the worst possible
direction, and both halves must be fixed:

**(a) The CLI currently rejects it as TERMINAL.** `cmd/aira/main.go:1408-1414`
refuses `maxWait <= 0` with `state=argument-invalid class=request-invalid`,
which the supervisor maps to `WorkerAdmitRequestInvalid` — a
`WorkerAdmitTerminal` that **drains the remaining queue to `unevaluated`**
(`supervisor.py:238-260, 572-590`). Shipping v2 as written would have converted
every speculative probe into a run-destroying verdict. The daemon's own
validator already accepts zero (`worker_admit.go:701-707`); only the CLI
disagrees. **Fix: the CLI accepts `--max-wait 0`, rejecting only negatives**,
with a boundary test at the CLI→daemon seam (test 22) rather than a mock.

**(b) Zero-wait must not merely mean "one evaluation".** `evaluateWorkerAdmit`
runs *before* the deadline check (`worker_admit.go:774-825`) and can block on
the outer-scope lock or the CPU gate — i.e. on **another job's** critical
section — on the supervisor's single thread. **Fix: `max_wait_ms == 0` selects
try-acquire semantics** — both locks are taken with a non-blocking
`select`/`default`, and a miss returns `denied`/`contended`/`admit-locks-busy`
immediately. Both are already 1-buffered channels (`worker_admit.go:164-188`),
so this is a `default:` arm, not new machinery.

**What "speculative" therefore does and does not promise (Sol round 3 P1 —
v3's "never blocks" was an overclaim, now retracted).** A speculative request:

- **never waits on a lock held by another job**, and **never polls** — those
  were the two unbounded terms, and try-acquire removes both;
- **does** perform bounded cgroup I/O before the lock
  (`worker_admit.go:385-390`) and, on a grant, a scope creation, a `fork`, and
  the placement-ack read (`supervisor.py:991-1043`).

So the honest invariant is **bounded, small, and never dependent on another
job's progress** — not "non-blocking". The grant-path cost is the same fork and
ack every existing spawn already pays (`run()` startup and `_replace_worker`),
so the probe adds frequency, not a new class of stall. Sol's alternative —
moving speculative spawning asynchronously into the selector loop — is a
restructure of the single-threaded dispatch loop that the aitest design
deliberately keeps single-threaded (`supervisor.py:26-30`); refused for v1 and
recorded in §5h.

### 4.11 Rotation and fairness (CORRECTED)

A slot is held for a worker's life and released on retirement. aitest workers
retire on their existing recycle triggers (`worker.py:244`): 600 s elapsed, 200
tests, or `memory.current` above 80% of `memory.high`.

v1 called 600 s the worst-case fairness quantum. Sol P0-1 corrected this:
`_should_recycle` is only evaluated **after a test completes**
(`worker.py:485-487`), so the real quantum is
`max(recycle trigger, duration of the test currently running)`. A 40-minute test
holds its slot for 40 minutes. Recorded honestly; the floor is what guarantees
the other job still makes progress, and §4.10's probe is what lets it grow the
moment a slot actually frees.

Preemption is **not** built (§5g).

### 4.12 Coverage boundary — what this structurally CANNOT govern

Added after a field finding from the peer session `split` (2026-09-05), and
stated as a boundary rather than left to be inferred, because "aitest now
governs CPU" is exactly the kind of sentence that would be read as more than it
is.

**This change governs the CPU concurrency of aitest WORKER PROCESSES, and
nothing else.** It hooks worker admission. Two real, observed classes of CPU
load fall outside it entirely:

**(a) Recipes that never engage aitest.** `fastest-ee`'s
`make test-lite-slowbuild` (`Makefile:350`) runs `uv run pytest -q` with the lite
project's own `-n auto` addopts, bypassing `pytest_worker_flags.sh` and
therefore aitest altogether. It was observed OOM-killed three times under real
contention — confirmed `systemd-oomd` PSI-pressure kills with zero test
failures, even under `aira confine --delegate-ram --memory-max 24G` with 58 GB
machine-wide free. **A recipe that does not route through aitest gets no
CPU governance from this change**, and that routing gap is on the consumer
project's side, not AIRA's.

**(b) Build subprocesses spawned inside test bodies.** The same recipe's
heaviest work is not pytest workers at all — it is `uv` wheel builds and
`docker build` invocations spawned as ordinary subprocesses from inside test
functions. A worker-spawn/retire hook **structurally cannot see them**: they are
not workers, they are not admitted, and they would not be even if that recipe
were wired through aitest correctly. One admitted worker can fan out into an
arbitrary number of CPU-heavy children, and this gate counts it as one.

**Not fixed here, and deliberately not attempted.** Governing arbitrary
subprocess CPU is a different problem from bounding worker concurrency, with a
different mechanism (it would need cgroup-level CPU accounting or weighting at
the confine-scope level, not a count at an admission verb). Folding it in would
blow this change's scope and blur the one thing it does cleanly. The peer
session is filing it separately.

**Consequence for §4.6's guarantee:** the bound is over *aitest workers in a
slice*, not over CPU-heavy processes on the machine. §11 states the yield in
those terms and no wider.

## 5. Rejected alternatives

**(a) Make aitest workers speak the existing `governor` verb.** Best fairness
(preemptive park/resume, youngest-first). Rejected: it needs a Go relay process
per worker *in addition to* the admit relay each worker already has, a park
protocol inside the forked child's loop, and it mixes park-capable and
park-incapable clients in one active set — `evaluate()` would set
`parkRequested` and log `preempt-requested` for workers that can never park,
which is a misleading log rather than a mechanism. Its checkpoint/park machinery
and its per-test RAM ordering are unusable for a model that reserves per
*worker*. Large machinery for a fairness refinement on top of a bound that does
not exist yet.

**(b) A separate CPU-slot verb acquired alongside `worker-admit`.** Two
independently-acquired resources per worker is a two-resource ordering problem
(job holding RAM waits for CPU while another holds CPU and waits for RAM).
Folding both into one atomic decision removes the class entirely.

**(c) In-memory slot ledger keyed by connection lifetime.** Reintroduces AIRA-41
and needs AIRA-74-style restart reconstruction. Kernel-derived counting has
neither problem.

**(d) Ask the daemon for a pool size once at startup.** Static; cannot shrink
when another job starts or grow when one finishes.

**(e) Scale `PYTEST_TIMEOUT` by measured load.** The ticket's option 2 —
judgement layered on telemetry, treating the symptom. See §8.

**(f) Serialise heavy gates at slice level.** The ticket's option 3 — defeats the
point of an admission system built to make safe concurrency possible.

**(g) NEW, added in v2: add preemption/rebalancing so the bound becomes a hard
`max(C, J)`.** This is the only way to make v1's claimed bound true (Sol P0-1's
first branch). Rejected for v1: it is (a) by another name — it requires a
cooperative park protocol in the worker loop and a daemon-side active set — and
`architectural-simplicity` is explicit that the primitive plus a documented gap
beats new machinery. **The bound is weakened and stated honestly instead**
(§4.6), and §4.10's probe recovers most of the practical convergence without any
preemption. Revisit only with field data showing `C + J - 1` is insufficient.

**(h) NEW in v4: make speculative spawning asynchronous inside the selector
loop.** Sol round 3's alternative to accepting bounded blocking. It would remove
the fork/ack cost from the dispatch path entirely — but the aitest supervisor is
deliberately single-threaded (`supervisor.py:26-30`), and every blocking read on
that loop has already been individually bounded by AIRA-92 rather than made
concurrent. Introducing asynchrony there is a redesign of the dispatch loop, not
a CPU-governance change, and it would reopen a class of races that design
closed. Refused for v1; the bounded-cost invariant in §4.10.1(b) is claimed
instead.

**(i) NEW in v4: an exclusive per-outer-scope pending-floor claim spanning grant
through placement acknowledgement.** Sol's preferred fix across rounds 2 and 3.
Refused twice; see §12's standing-disagreement note. §4.4.2's rate limit is the
cheap 90% of it — one timestamp on state that already exists, with no lifetime,
expiry or reconstruction story — and §4.6 retracts the claim the remaining 10%
would have supported rather than asserting it without the machinery.

## 6. Interaction with AIRA-33 and the simplification programme

`2026-09-04-simplification-programme-plan.md` §4.1 candidate 4 grades
`daemon/governor.go` + `cpuslots.go` **UNCERTAIN**, with the recorded reason
being, verbatim, that "**AIRA-64** is filed as input to the next scheduler
milestone" and that deleting "closes a door on two open tickets", requiring an
"**Owner tick**".

**This plan resolves that uncertainty in the direction of deletion:**

- AIRA-64 needs the **capacity concept** (`cpuslots.go:desiredCPUSlots`, 27
  lines) and the **per-job floor idea**. Both are used here. `cpuslots.go`
  becomes **KEEP**, and for the first time since aitest landed it has a client
  that exists.
- AIRA-64 does **not** need `governor.go`'s park/active-set scheduler, its
  checkpoint protocol, or its per-test RAM ordering (§5a). After this lands,
  candidate 4 splits: **`cpuslots.go` KEEP, `governor.go` CUT**.
- The grep sweep AIRA-33 requires ("unconfirmed, **not assumed**") was performed
  for this plan: the only non-test callers of `governorSet` are
  `server.go:169/261-275/391/617-624` and `governor.go` itself. There is **no
  non-pytest client**.

A finding to relay, not a change this branch makes. Retiring `governor.go` is
AIRA-33's own work and its own review.

## 7. Invariants

1. Every outer scope with queued work is always entitled to ≥1 worker (§4.5),
   and no orphaned directory can withhold it (§4.4).
2. Live workers in a slice ≤ `capacity + max(0, J - 1)`, `J` = outer scopes
   holding ≥1 worker child, whenever the reading can be established, **and
   provided at most one supervisor admits concurrently under each outer scope**
   (§4.6's proviso, which invariant 2 previously omitted — Sol build-review P2).
   **Not** `max(capacity, J)`.
3. A CPU denial is `state=denied class=contended` — retriable, containment
   preserved. Never `admission-unusable` (which strips RAM containment for a
   whole run) and never `request-invalid` (which marks queued work unevaluated).
4. An unestablished CPU reading is reported as `unevaluated`, surfaced to the
   run's own stderr, and logged. Never zero, never a pass (§4.9).
5. The RAM decision is unchanged; no path becomes *more* permissive on RAM.
6. Lock order `outer-scope → CPU gate`, never the reverse; the gate is
   abandonable (§4.7).
7. A saturated run must still **grow** when capacity frees, and must not freeze
   its dispatch loop while trying (§4.10).

## 8. Owner-level decision fork — FLAGGED, NOT DECIDED

**Fork: should aitest reclassify a wall-clock test timeout observed under
measured CPU starvation as `unevaluated` rather than `failed`?**

§4 prevents most contention. It cannot guarantee no test ever wall-clock-times-out
while making progress: non-aitest CPU load (Go builds, agents, browsers) is not
counted, the floor deliberately oversubscribes to `C + J - 1`, and a single test
may itself spawn many CPU-heavy subprocesses.

**For:** it is the honest answer — a starved test's result was not established,
and AIRA's own rule is that such a check reports `unevaluated`. aitest already
has a first-class `unevaluated` outcome and already synthesizes reports for it.

**Against (why this plan does not build it):**

1. **Detection would require string-matching third-party prose.** aitest receives
   a `pytest-timeout` failure as an ordinary `failed` `TestReport` whose only
   distinguishing feature is `longrepr` text. Classifying on that is exactly the
   anti-pattern AIRA-42 spent a milestone deleting from `supervisor.py` (eleven
   substring probes over a prose channel, six recorded recurrences). **This
   objection alone is close to decisive.**
2. It would report a genuinely hung test as `unevaluated` rather than `failed`.
   **Correction (Sol P2):** this is *not* a false-green in the run-result sense —
   `_synthesize_unevaluated_reports` renders unevaluated as `outcome="failed"`
   for junit (`supervisor.py:1610-1616`) and `session.testsfailed` counts
   unevaluated (`__init__.py:108`), so the run still fails. The real cost is
   diagnostic: a hang would be labelled "could not establish" rather than
   "broken".
3. It is judgement layered on a telemetry-ish signal, the class
   `architectural-simplicity` is explicitly wary of.

**A middle option, also not built:** have each worker read its own scope's
`cpu.stat usage_usec` around each test and *report* CPU-time alongside
wall-time, so a starved failure is visibly starved — pure telemetry, no
reclassification.

**Recommendation:** land §4 and measure. If phantom timeouts survive a bounded
machine, revisit with field data. **No default is silently chosen: this branch
changes nothing about timeout classification.**

## 9. Test plan

Every test must be able to fail against a wrong implementation.

**Unit — gate arithmetic and disposition** (`internal/daemon`, seams, no cgroups):

1. under capacity → granted (the gate does not deny what RAM would grant);
2. at capacity, scope has ≥1 populated worker → `denied`/`contended`/
   `cpu-slots-saturated`;
3. at capacity, scope has **zero** populated workers → **granted** (floor);
4. **orphan regression (Sol round 1 P0-2):** scope has one `.aira-worker-*`
   directory that is empty **and older than the placement grace** → floor still
   grants. Fails against a directory-count floor;
5. **placement-window regression (Sol round 2 P0-2):** scope has one
   `.aira-worker-*` directory that is empty **and younger than the grace** →
   floor does **not** grant. Fails against a populated-only floor. Tests 4 and 5
   together are what make the age gate load-bearing rather than decorative;
6. the machine-wide total **does** count both of those directories (the two
   counts are genuinely different, each failing safe);
7. unreadable `cgroup.events` counts the child as live (no fabricated floor);
8. `AIRA_DAEMON_CPU_RESERVE` changes capacity; an invalid value falls back to
   capacity 1 with a log — **corrected from v1**, which wrongly said "refused"
   (`server.go:261-275`);
9. scan error / non-`.aira-CONFINE-` outer scope / unresolvable slice → CPU
   dimension `unevaluated`, RAM decision stands, log emitted **once**, and the
   granted outcome carries `cpu_slots=unevaluated`;
10. the CPU gate never turns a RAM **denial** into a grant;
11. guard test pinning the exact `state`/`class`/`reason` triple, so a reword
    cannot silently change the client's disposition (invariant 3).

**Ordering / cost:**

12. a **saturated** request denies from the cache and performs **neither** a
    forced RAM rescan **nor** a forced CPU snapshot — **corrected from v2 (Sol
    round 2 P2), which counted only the RAM seam and would have let a
    per-poll CPU scan survive.** Both seams are counted;
13. a **granting** request always uses a forced fresh snapshot, never the ≤1 s
    cache.

**Concurrency:**

14. **corrected twice:** each racing requester is seeded with a *populated*
    worker so none is floor-entitled, `N` is set precisely relative to `C`, and
    the racers are released from a **barrier placed after the pre-lock cached
    read and before the gate**, so an implementation that reads the count
    outside the gate loses deterministically rather than by luck. Asserted on
    the resulting cgroup tree, not on the count of returned grants;
15. **shared-outer-scope placement race (Sol rounds 2 and 3):** `N` requesters
    under **one** outer scope race while saturated; **exactly one** is granted
    via the floor and the other `N-1` receive `cpu-slots-saturated`, with the
    first grant left outstanding and unplaced. Fails against a populated-only
    floor. The test then **advances the clock past the grace window with that
    first grant still outstanding** and asserts §4.4.2's rate limit permits at
    most one further floor grant, not one per requester — the exact round-3
    construction;
16. gate acquisition is abandoned when the peer context is cancelled and when
    the daemon is stopping; a `max_wait_ms == 0` request never blocks on either
    lock (try-acquire), returning `admit-locks-busy` when they are held.

**Real cgroup (`_linux_test.go`) — anti-INERT:**

17. with a real slice, a real outer scope and real populated `.aira-worker-*`
    children, the gate actually denies at capacity **with ample RAM**, asserting
    the exact `cpu-slots-saturated` reason. This is the test that would have
    caught the AIRA-59 "shipped inert on every real host" failure and is the
    single most important one here;
18. **worker-directory `mtime` lifecycle (Sol round 3 P2):** a real
    `.aira-worker-*` directory's `mtime` still reports creation time *after* it
    is populated and after a child cgroup is created inside it — not merely for
    an outer scope at rest. Plus the unhappy paths: a `stat` failure counts the
    child as live, a malformed `cgroup.events` counts it as live, and an
    `mtime` in the future (clock skew) counts it as young rather than aged out.
    This pins the §4.4.1 assumption so a kernel or mount change fails a test
    instead of silently disabling the age gate;
19. multi-slice: two outer scopes under **different** slices are each bounded
    separately — pins §4.8's documented limit so it cannot silently change.

**CLI / relay boundary:**

20. **real CLI→daemon boundary test (Sol round 2 P0-1):** `--max-wait 0` is
    accepted, produces exactly one evaluation, and yields a *contended* (never
    `request-invalid`) outcome when saturated. Asserted through the real
    argument parser, **not** by mocking `spawn_worker`'s arguments — a mock
    would have passed against the shipping bug;
21. the `cpu_slots` token survives every hop: daemon response →
    `WorkerAdmitGrantFields` → outcome line → runner client grant/lease → the
    supervisor's parsed fields. One end-to-end assertion, not five per-hop ones.

**Client (`internal/pylib`, Python):**

22. a run denied at startup **grows later** when capacity frees — fails against
    today's permanent `break`;
23. **the probe fires when the loop is never idle (Sol round 2 P1):** every
    dispatch-loop iteration completes immediately (continuous sub-second test
    completions keep the fds ready), capacity frees, and the pool still grows.
    Fails against a probe attached only to the `select()` timeout branch;
24. the probe is rate-limited to ~1/s and never issues more than one in-flight
    admission at a time;
25. the last-worker case still waits indefinitely and never falls back to
    unconfined (unchanged behaviour, pinned);
26. one-shot stderr warning on `cpu_slots=unevaluated`, not one per worker.

**End-to-end:**

27. two aitest supervisors under two outer scopes in one slice, capacity pinned
    low: live worker scopes never exceed `C + max(0, J-1)`, and **both runs
    complete** — the regression test for "slow must not become stalled".

**Mutation testing** on at least: the capacity comparison operator and boundary;
the floor's zero-test; the populated-vs-directory distinction (swap one count
for the other); the age-gate comparison and its grace constant; the `force` flag
on the granting snapshot; the gate's lock scope; the try-acquire `default:`
arms; and the CLI's `max-wait` sign test. Each mutant must be killed by a named
test above; any survivor becomes a new test.

## 10. Deferrals, recorded

- Preemptive rotation / park (§5a, §5g) — recycle-boundary rotation plus the
  §4.10 probe is v1's fairness mechanism.
- Timeout reclassification and CPU-time telemetry (§8) — owner decision.
- Cross-slice CPU authority (§4.8) — per-slice today; one slice in the deployed
  configuration.
- Capacity from cgroup `cpu.max` rather than `runtime.NumCPU()` (§4.2) — affects
  the governor equally; separate change.
- Weighting a worker as more than one slot when a test fans out into
  subprocesses — no signal to size it with.
- The daemon-unavailable fallback (`_spawn_fallback_worker`) stays ungoverned
  for CPU, exactly as it is already ungoverned for RAM.
- `governor.go` retirement — AIRA-33's work, informed by §6.

## 11. Expected yield

Removes the dominant cause of contention-induced phantom `pytest-timeout`
failures **for aitest-routed suites** on this box: two concurrent merge-gates go
from 32 heavy workers on 16 cores to 16.

Stated in the narrowest true terms, because three review rounds and a build
review were each spent deleting a version of this sentence that claimed more
than the code delivers:

- it bounds **aitest worker processes in one slice**, not CPU-heavy processes on
  the machine (§4.12);
- it does **not** cover recipes that bypass aitest, nor build subprocesses
  spawned from inside test bodies — both observed in the field, both
  structurally outside a worker-admission hook (§4.12a, §4.12b);
- it does **not** claim CPU is never oversubscribed: the floor deliberately
  oversubscribes to `C + max(0, J-1)`, and non-aitest load is uncounted (§4.6);
- it does **not** change how a `pytest-timeout` failure is classified (§8).

## 12. Review history

**v1 → v2. Sol (GPT-5.6, read-only repo access): `BLOCK`, 2 × P0, 6 × P1, 1 × P2.
DeepSeek-pro (context supplied inline): `APPROVE-WITH-CHANGES`, 6 × P1, 5 × P2.**

Accepted and fixed:

| finding | disposition |
|---|---|
| Sol P0-1 — the stated bound `max(C,J)` is mathematically false; admission cannot revoke an incumbent's workers | **Accepted.** §4.6 rewritten to `C + J - 1` with a corrected table; §7.2, §9.21, §11 follow. §5g records the preemption alternative and why it is refused rather than silently required. |
| Sol P0-2 — a directory count does not prove a live worker; one empty orphan defeats the liveness floor permanently | **Accepted.** §4.4 splits the count in two — populated for the floor, directory for the cap — each failing in its safe direction. New tests 4, 5, 6. |
| Sol P1-3 — "no Python change" is false; startup `break` is permanent and `_replace_worker` never restores pool size | **Accepted.** §4.10 adds the speculative growth probe; test 17. |
| Sol P1-4 — a 30 s blocking replace would freeze the single-threaded dispatch loop under contention | **Accepted.** §4.10 uses `max_wait="0s"` whenever another worker survives; test 18. |
| Sol P1-5 — the v1 evaluation order forces a RAM tree scan on every saturated poll | **Accepted.** §4.7 moves the cached CPU check ahead of the forced RAM rescan and uses one snapshot for both numbers; test 11. |
| Sol P1-6 / DeepSeek P1-2, P1-6 — slice-wide, not machine-wide; derived root is trusted | **Accepted, scope narrowed rather than machinery added.** §4.8 states the guarantee as per-slice, validates the derived root through `admitResolveSlice`, and pins the limit with test 16. |
| Sol P1-7 — fail-open CPU-unevaluated is unreportable and may be operationally inert | **Accepted.** §4.9 adds `cpu_slots_*` fields on the granted outcome plus a one-shot supervisor warning; tests 8, 15, 20. |
| Sol P1-8 — concurrency test 13 was invalid (all-zero-worker scopes are floor-entitled) | **Accepted.** Test 13 seeds each requester and asserts on the tree. |
| Sol P2 — invalid `AIRA_DAEMON_CPU_RESERVE` is not "refused" | **Accepted.** Test 7 corrected against `server.go:261-275`. |
| Sol P2 — the §8 "false GREEN" argument conflicts with unevaluated rendering as a failure | **Accepted.** §8.2 corrected; the objection is now diagnostic quality, and objection 1 carries the argument. |
| DeepSeek P1-1 — a stale-low cached count must never grant by itself | **Accepted** (already the design; now pinned by test 12). |
| DeepSeek P1-3 — the floor still oversubscribes with many jobs | **Accepted**; this is Sol P0-1's arithmetic, now stated as `C + J - 1`. |
| DeepSeek P1-5 — the gate mutex must be bounded/context-aware | **Accepted.** §4.7 makes it abandonable like `acquireWorkerScope`; test 14. |
| DeepSeek P2-2 — stale worker dirs can false-deny | **Accepted, documented** (§4.4): they inflate the cap count only, which denies growth; AIRA-36's reaper is the backstop. |

Considered and deliberately **not** taken:

| finding | why not |
|---|---|
| Sol P0-1 first branch / DeepSeek — add preemption or rebalancing so the bound is hard | §5g: it is §5a's machinery by another name. `architectural-simplicity` prefers the primitive plus a documented gap. The bound is weakened and stated honestly instead. |
| DeepSeek P1-4 — derive capacity from cgroup `cpu.max` | §4.2: pre-existing in `desiredCPUSlots` and shared with the governor; fixing it here would change the governor's behaviour as a side effect. Deferred (§10). |
| DeepSeek P2-1 — per-job cap / telemetry for idle workers holding slots | Idle-worker detection needs a liveness signal aitest does not have. §4.11 documents the quantum honestly instead. |
| DeepSeek P2-10 — add metrics before deciding the §8 fork | Reasonable, but it is the §8 middle option, which is itself deferred to the owner rather than pre-empted here. |

### v2 → v3. Sol re-gate: `BLOCK`, 2 × P0, 4 × P1, 1 × P2

The second round found that two of v2's *fixes* were themselves wrong. Both were
verified against source before being accepted.

| finding | disposition |
|---|---|
| **P0-1 — `max_wait="0s"` is rejected by the CLI as `request-invalid`, which is TERMINAL and drains the queue to `unevaluated`** (`cmd/aira/main.go:1408-1414`) | **Accepted; this was the most dangerous defect in v2.** Verified: the daemon accepts zero (`worker_admit.go:701-707`), only the CLI refuses it. §4.10.1(a) makes the CLI accept `0` and reject only negatives; test 20 asserts at the real CLI→daemon seam because a mock would have passed against the bug. |
| **P0-2 — `C + J - 1` is still exceedable: N supervisors sharing one outer scope, each paused between grant and placement, all read "zero populated" and all take a floor grant** | **Accepted.** §4.4.1 replaces the populated-only floor with **populated OR younger than the placement grace**, pinned to the client's own 60 s ack timeout. One rule closes both this and round 1's orphan hole. The `mtime` age source was verified empirically on this host. Tests 4, 5 and 15. |
| P1 — `J` = "scopes with queued work" does not support the bound; a scope leaves the queue while holding live workers (`supervisor.py:1160-1164`), and `J=0` degenerates | **Accepted.** §4.6 redefines `J` as scopes holding ≥1 worker child — a quantity the snapshot observes — and the bound becomes `C + max(0, J-1)`. |
| P1 — zero wait means "one evaluation", not "never blocks"; `evaluateWorkerAdmit` can still block on the outer lock, the gate, or cgroup I/O | **Accepted.** §4.10.1(b) gives `max_wait_ms == 0` **try-acquire** semantics on both locks. Both are already 1-buffered channels, so this is a `default:` arm rather than new machinery. Test 16. |
| P1 — probing only on the `select()` timeout starves when result fds stay continuously ready (`supervisor.py:1877-1883`) | **Accepted.** §4.10.1 probes on every loop iteration under a monotonic rate limit. Test 23 constructs the never-idle loop explicitly. |
| P1 — CPU observability is inert unless it traverses the runner client and lease (`worker_admit_client_linux.go:58-67,184-190`) | **Accepted in substance, trimmed in scope.** All five hops are in scope and test 21 asserts end-to-end. But v3 ships **one** token (`cpu_slots`) rather than v2's three fields: `live`/`capacity` are telemetry nobody branches on, and `architectural-simplicity` forbids five hops of plumbing for them. Recorded in §4.9. |
| P2 — tests 11/13/21 remain porous (RAM seam only; no barrier; distinct scopes cannot expose the shared-scope race) | **Accepted.** Test 12 counts both seams; test 14 adds a barrier placed between the cached read and the gate and asserts on the tree; test 15 is the new shared-outer-scope test. |

### v3 → v4. Sol re-gate: `BLOCK`, 1 × P0, 1 × P1, 2 × P2

| finding | disposition |
|---|---|
| **P0 — the age gate still does not establish the bound: the grace runs from scope creation but the client's ack timer starts only after the fork begins, so a supervisor stalled past the grace lets its directory age out while its grant is live, and a second supervisor under the same outer scope becomes floor-eligible again — repeatedly** | **Accepted, and answered in two parts rather than with a claim protocol.** §4.4.2 adds a per-outer-scope floor rate limit (one field on the already-locked `workerScopeState`; at most one floor grant per grace window), which removes the "arbitrarily many" term. §4.6 then **retracts** the inequality for the only configuration where the residual survives — several supervisors admitting *concurrently* under one outer scope — rather than asserting a bound the mechanism does not deliver. Test 15 extended to advance past the grace with the first grant outstanding. Also accepted: the grace is operator-adjustable, so the daemon now reads `AIRA_AITEST_PLACEMENT_ACK_TIMEOUT` instead of hardcoding 60 s. |
| **P1 — try-acquire does not make `max_wait=0` "never block": there is cgroup I/O before the lock, and a granted probe still forks and waits for the placement ack** | **Accepted; the claim is retracted.** §4.10.1(b) now claims only what is true — never waits on another job's lock, never polls, bounded cgroup I/O, and on a grant the same fork+ack every existing spawn already pays. §5h records the asynchronous-selector alternative and why it is refused. |
| P2 — the `mtime` precedent is source-contradicted (`AgeSeconds` comes from the scope id, not `mtime`) | **Accepted.** The precedent claim is deleted; only the measurement remains, and test 18 now exercises the real worker-directory lifecycle plus `stat` failure, malformed `cgroup.events`, and clock skew. |
| P2 — §4.5 still said "populated count is zero", contradicting §4.4.1; test 15 was internally inconsistent | **Accepted.** One named predicate, `liveForFloor`, defined once in §4.4.1 and used everywhere; test 15 reworded to "exactly one grants, `N-1` get `cpu-slots-saturated`". |

**Standing disagreement, recorded rather than resolved (twice raised, twice
refused).** Sol's preferred fix in rounds 2 and 3 was "an exclusive per-outer
pending-floor claim spanning grant through placement acknowledgement, with safe
orphan expiry and reconstruction". That is a new distributed-claim protocol with
its own lifetime, expiry and restart story — the shape
`architectural-simplicity` names explicitly ("prefer keeping the primitive and
documenting the gap over new machinery"). v4 takes the cheap 90% of it
(§4.4.2's one timestamp) and **retracts the claim** the remaining 10% would have
supported (§4.6's proviso), which is the project's own stated preference for
handling exactly this trade. If the build-review or field use shows the residual
is reachable in practice, the claim protocol is the named escalation.

**Plan-review loop closed at three rounds.** Rounds 1 and 2 changed the
mechanism materially (the bound, the floor's predicate, the evaluation order,
the client changes, the terminal `max_wait=0` defect). Round 3 changed no
mechanism: it corrected two overclaims, one precedent citation and one naming
inconsistency, and its one P0 is answered by a retraction plus three lines. The
remaining disagreement is recorded above rather than iterated further.

## 13. What the build itself falsified

Three things the plan asserted turned out to be wrong, and each was caught by a
test written specifically to be able to catch it rather than by review.

**(a) `mtime` is not creation time (caught by the anti-INERT tier, on its first
run).** §4.4.1 is rewritten with the measured semantics. The mechanism survives
because the movement is one-directional in the safe sense, but the claim as
written was false. This is the clearest argument for the real-cgroup tier: every
seam-level test in the change would have passed against the wrong claim.

**(b) The CPU gate added a second cancellation checkpoint before scope
creation, and it made an existing test flaky (caught by the full-suite run, not
by the targeted one).** `acquireWorkerScope` checks `ctx.Done()` before its own
select, and `acquireCPUSlotsGate` copied that. Between the RAM decision and
`CreateWorkerScope` there is now a second place a vanished peer can abort, which
widened the pre-existing race that
`TestWorkerAdmitConnectionKeepsScopeChargedWhenResponseWriteFails` pins (AIRA-41's
invariant that a grant whose response write fails still leaves its scope
charged). Fixed by taking a FREE gate unconditionally and consulting `ctx` only
when the gate is genuinely contended — which is also the only case where
abandonability was ever the point. Note a plain three-way `select` would not
have fixed it: it chooses uniformly among ready cases, so a free gate plus a
cancelled context would still abort about half the time.

**(c) One test was porous, and mutation testing found it.**
`TestCPUGateGrantForcesAFreshSnapshot` asserted that a granting request
increased the scan counter. The mutant that removes `force` **survived**,
because the cached read also scans whenever the cache is cold — the counter rose
either way and the test proved nothing about `force`. Replaced with a test that
makes the cache deliberately stale-LOW and asserts on the **verdict**: the cache
says there is room, the tree says there is not, and only a forced rescan can
tell the difference.

**(d) The scan silently swallowed every unreadable outer scope.** The first
implementation `continue`d past any `ReadDir` error on a confine scope, with a
comment claiming that was as safe as skipping a vanished one. It is not: one
persistently unreadable **busy** scope made the whole slice read as emptier than
it is, so the gate admitted past capacity **while still reporting
`cpu_slots=ok`** — a false claim of governance, invisible from outside, and a
direct contradiction of the "total never undercounts" property the two-count
split rests on. Only a scope *proven* to have vanished (ENOENT confirmed by a
re-stat, mirroring `sumWorkerScopeChildren`) is skipped now; every other error
fails the snapshot into `unevaluated`.

**(e) `cpuSlotsMu` was held across the whole cgroup walk, so speculative
requests could still block behind another job.** That mutex sits in front of
both try-acquired locks on the cached path, so the try-acquire bought nothing:
a slow scan under one job froze another job's dispatch loop. The mutex now
guards only map access; two callers may scan the same root concurrently, which
is a duplicated `readdir` rather than a correctness problem.

**(f) The plan promised slice-resolver validation that the code did not do.**
§4.8 said the derived root would be canonicalised through `admitResolveSlice`;
the first implementation checked only the basename prefix lexically. A caller
naming `/anywhere/.aira-CONFINE-x` would have had the gate count an unrelated
directory, find nothing, and admit freely. Now the derived root is resolved
through the real resolver (which requires a real directory inside the cgroup2
mount) and symlinks are resolved *before* the parent is taken. **This one is
recorded as a process failure as much as a code one: the plan asserted a
behaviour and the build did not deliver it, which is the fourth time in this
change that prose ran ahead of code.**

**Deliberate behaviour change to an existing contract, recorded.**
`--max-wait 0` was `argument-invalid`; it is now valid and means *speculative*.
`TestRunWorkerAdmitCommandAlwaysWritesOneStructuredOutcome`'s "non-positive max
wait" case is rewritten as "negative max wait". This is the one place the change
edits an existing assertion rather than adding to it, and §4.10.1(a) is why.

## 14. Mutation testing

Reproduction: `~/tmp/aira64-mutants.sh` and `~/tmp/aira64-mutants2.sh`. Each
mutant flips one load-bearing decision; a mutant that SURVIVES means the tests
cannot tell correct from wrong there.

**Final: 27 mutants, 27 killed.** Three rounds were needed, and the two survivors
of round 1 are the finding, not the score.

| # | mutant | result |
|---|---|---|
| M1–M2 | capacity boundary `<`→`<=`; capacity check removed entirely | killed |
| M3–M4 | floor always open; floor never opens | killed |
| M5 | floor rate limit removed | killed |
| M6–M7 | age gate removed (populated-only floor); populated check removed (directory-count floor) | killed |
| M8–M9 | unestablished population / `stat` failure read as NOT live | killed |
| M10 | scan error becomes an empty snapshot (a fabricated zero) | killed |
| M11 | grant path does not force a fresh snapshot | **survived → fixed → killed** |
| M12 | gate not acquired on the grant path | killed |
| M13 | speculative gate blocks instead of try-acquire | killed |
| M14 | cached CPU check removed (per-poll rescan returns) | killed |
| M15 | CPU denial uses the terminal `request-invalid` class | killed |
| M16 | CLI refuses `--max-wait 0` again | killed |
| M17 | `cpu_slots` token never rendered | killed |
| M18 | `cpu_slots` dropped by the runner client | **survived → fixed → killed** |
| M19 | growth probe attached to the idle branch only | killed |
| M20–M21 | replacement / probe block on the full `max_wait` | killed |
| M22 | growth probe rate limit removed | killed |
| M23–M24 | `cpu_slots` warning per worker / never | killed |
| M25–M26 | `cpu_slots` dropped by the CLI grant fields / never set by the daemon | killed |
| M27 | free gate consults `ctx` again (reintroduces the §13(b) flake) | **survived → fixed → killed** |

**M11 — a porous assertion.** `TestCPUGateGrantForcesAFreshSnapshot` asserted a
scan counter increased. The cached read scans too whenever the cache is cold, so
the counter rose with or without `force`. Replaced by an assertion on the
**verdict** against a deliberately stale-low cache (§13(c)).

**M18 — an uncovered hop.** Nothing crossed
`worker_admit_client_linux.go`'s grant unmarshal and lease, so the CPU-governance
signal could be dropped there and every test stayed green — exactly the
"operationally inert" shape Sol's round-2 P1 warned about. Fixed by asserting
`cpu_slots` in the real-CLI-against-real-daemon-and-real-cgroup boundary test,
which is the only place all five hops are crossed at once. M25 and M26 were added
to cover the two neighbouring hops for the same reason.

**M27 — a race is not a detector.** The §13(b) regression was originally caught
by a full-suite *flake*. Reintroducing it survived the suite, because a flake
fails only sometimes. Fixed with a deterministic regression test that cancels the
peer at an injected point inside the outer-scope lock, immediately before the
gate (`TestCPUGateTakesAFreeGateWithoutConsultingContext`). **A bug found by a
flake needs a deterministic test, or the fix is unprotected.**

### Rounds 3–5, after the build review

The build review changed the mechanism (the age gate was replaced by
`lastGrantAt`), so the suite was re-derived: **41 mutants, all resolved.** New
ones cover the two-count split, the `lastGrantAt` window, the unreadable-scope
P0, the outer-lock try-acquire in both directions, the resolver, and the scan
mutex. Three more survivors, and the third is the most interesting result in this
whole change:

**M29 / M29b / M29c — the resolver was called and then effectively ignored.**
Removing the resolver call survived, because the only test touching it called it
*directly* rather than through the gate; and even after adding a gate-level test,
ignoring the resolver's *canonicalised answer* still survived, because the
fixture's resolver returned its input unchanged. Fixed with a resolver that
canonicalises to a genuinely different path which is the only one the tree
fixture knows.

**M31 — `cpuSlotsInvalidate` was deleted, because mutation testing proved it
earned nothing.** Removing it survived every attempt to write a test that could
tell the difference, and working out why showed no such test can exist: the cache
is read only by the CHEAP path, whose sole power is to deny early, so a stale-low
total can never cause a wrong admission — only a failure to deny early, which
sends the request to the grant path, which forces a fresh scan *and re-caches*.
The cost was exactly one extra scan per grant; the price was a second code path
that had to derive the same cache key as `cpuSlotsLocate` and agree with it
forever (M31b was written to exploit precisely that). **The honest response to a
mutant that cannot be killed is to ask whether the code is doing anything — and
here it was not.** Deleting it removed a whole class of key-agreement bug and
left the correctness guarantee where it actually lives: every grant is preceded
by a forced fresh scan.

**Final tally: 41 mutants, 38 killed, 3 retired by deleting the code they
targeted.** No mutant remains alive against shipped code.
