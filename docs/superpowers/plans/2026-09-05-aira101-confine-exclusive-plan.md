# AIRA-101 — `aira confine --exclusive`: slice-exclusive scheduling for uncontended benchmarking

Status: **v2**, revised after a GATE-FAIL on v1 (Fable), an orthogonal review
(DeepSeek), and field input from a live contaminated benchmark
Ticket: `.aira/tickets/AIRA-101.md`
Branch: `aira101-confine-exclusive` off `2bfd12c`

## 0. What changed from v1, and why

v1's centre of gravity (§2: exclusivity as a *derived* property of a
connection-held `admitWaiter`) was gate-PASSED and is kept verbatim. v1 failed
the gate on four defects *around* that core:

| # | Defect in v1 | Fix |
|---|---|---|
| P0 | The drain blocked `aira confine-reserve` **sub-reservations**, so every test of a running `--delegate-ram` suite would block 5 min then run uncharged — the drain would not converge, and a `--exclusive --delegate-ram` benchmark would stall its **own** suite | §5.1 classifies waiters; sub-reservations carry `parent_scope_id` and are exempt |
| P1 | The emptiness predicate was leaf-only, and v1's justification was **factually wrong** — `BootstrapAitestSupervisor` drains *every* pid out of the outer scope, so a running aitest suite reads `Populated == 0` and, after a daemon restart, would be declared "empty" | §6.1 makes liveness subtree-aware via `cgroup.events` |
| P1 | `--exclusive` could fall through to the flock fallback or launch on an `unevaluated` grant — a benchmark believing it was alone when it never was | §9 fails closed on every non-grant |
| P1 | The hold-time `worker-admit` exemption covered only the holder's *own* outer scope, so a nested `aira confine --delegate-ram` under the holder had its workers denied | §12.2 extends it to holder-token waiters |

Field input (peer session `field`, from an hour of contaminated
Rust-FDTD-vs-openEMS throughput numbers — the incident that motivated this
ticket) adds one requirement that reshapes the deliverable rather than merely
correcting it:

- **§9 — the job must be able to KNOW whether it got exclusivity.** A feature
  that silently degrades produces numbers that *look* clean, which is worse than
  no feature. This is now a first-class, machine-readable output, not just an
  internal refusal rule.
- §3 — an explicit statement that exclusivity **cannot** cover Docker
  containers, which structurally escape `aira.slice` entirely.

And two changes of my own, from the orthogonal review and from tracing the
stale-lease reaper: **at most one exclusive waiter per slice** (§5.3), and **a
drain that cannot establish emptiness aborts rather than stalls the machine**
(§6.2).

## 1. Problem and the owner's binding decision

A benchmark under `aira confine` shares `aira.slice` with every other session on
this machine. Contention invalidates the measurement — as it did, unnoticed, for
an hour of real throughput numbers.

**Binding, not revisited: ACTIVE DRAIN.** On request, the daemon stops admitting
NEW job-level work to that slice. Already-running jobs finish naturally — never
killed. Once the slice is empty, the exclusive job is admitted alone. The owner
chose this over passive-wait; field's own framing independently agrees that
never preempting running work is right.

Two places below refine what "new work" means. Neither challenges the decision;
both are the difference between a drain that converges and one that cannot:
§5.1 (sub-reservations) and §12.1 (`worker-admit`). The literal alternative
stalls every running delegate-ram suite, which is precisely the "interrupted"
outcome the owner's decision forbids.

## 2. The load-bearing question: where does exclusivity live?

A standalone in-memory "slice X is draining" bit that survives its requester's
death wedges `aira.slice` for **every session on this machine** with no
self-healing path.

### 2.1 The answer: a property of an admission waiter, not a flag

The codebase already has a crash-safe lease primitive; this design adds no
second one. **The admission connection IS the lease**: `admitConnection`
enqueues an `*admitWaiter` onto the per-slice `sliceQueue` and holds the
client's socket for the whole life of the job. Its `defer release()`
(`admit.go:772`, registered immediately after enqueue with no early return
between) → `releaseAdmitWaiterLocked` (`:1227`) removes the waiter on **every**
return path.

So the whole mechanism is one field:

```go
type admitWaiter struct {
    ...
    exclusive bool   // AIRA-101
}
```

and two predicates DERIVED under `queue.mu`, never stored:

- **draining**: the waiter with `exclusive && state == admitQueued`
- **held**: the waiter with `exclusive && state == admitGranted`

There is no `queue.exclusiveHeld bool`, no `map[slice]owner`, no on-disk marker
and no new reaper. This is AIRA-59's lesson applied verbatim: that freeze duty
cycle is derived from one anchor precisely because a stored phase plus a mutable
deadline made "freeze becomes permanent" defects *expressible*. Here the stakes
are higher, so the same discipline applies: **a wedge must not be
representable**, not merely avoided by careful coding.

### 2.2 The bounds, stated honestly rather than absolutely

v1 claimed "exclusivity cannot outlive the process that requested it". True on
every kernel-closed path, and the gate verified each — peer close, timeout, write
failure, `s.stopping`, panic; admit sockets are CLOEXEC and only two pipes reach
`ExtraFiles` (`confine_linux.go:887`), so a launched job cannot inherit and hold
the lease. But the codebase records a not-fully-explained daemon-side
zombie-socket case (`confine_reaper.go:163-169`), so the honest claim is a
**bound**, not an impossibility:

| Failure | Released by | Worst case |
|---|---|---|
| Requester Ctrl-C / crash / OOM / `kill -9` | fd close → `peerCtx` → `defer release()` | immediate |
| Requester's wait ceiling expires | `timeoutAdmitWaiter` (`admit.go:1190`) — the drain predicate stops matching the instant state becomes `admitRejected`, before removal | the exclusive ceiling (§10) |
| **Daemon restart / stop** | fresh empty `admitQueues` map | immediate, **fail-OPEN** |
| `aira confine --kill` | `releaseActiveConfine` + supervisor death closing the socket | immediate |
| Socket still open with a dead requester (the unexplained case) | *queued*: `timeoutAdmitWaiter` regardless of the socket. *held*: the AIRA-49/68 stale-lease pass, on reaped-empty or vanished proof | `defaultStaleLeaseReleaseGrace` = 15 min (`paths.go:90`) |

**Worst case ≈ 30 minutes, no operator intervention on any path.** There is no
state for an operator to clear, because there is no state outside the waiter list.

### 2.3 Interaction with the AIRA-49/68 stale-lease reaper — verified safe

An exclusive **holder** is a granted waiter with a scope id, so it *is* eligible
for `staleGrantedLeases` (`confine_reaper.go:187`). Safe in both directions:

- The sweep never reclaims on age alone — it requires positive proof the scope
  was physically reaped as empty, or seen and then observed absent. A running
  benchmark has a populated scope, so neither proof holds.
- If a proof *does* hold (the job is over, or its leader escaped its scope), the
  sweep releases the hold — the **fail-open** direction, which is correct.

A **draining** (queued) waiter is never a candidate: the sweep skips
`state != admitGranted` and `scopeID == ""` (`:199`).

### 2.4 The accepted gap, named and mitigated

A daemon restart mid-benchmark loses the HOLD. **The slice is not wedged** (the
important direction), but the run becomes contended. Per field's item 1 that
must never be silent — §9.3 makes it machine-readable.

**Rejected alternative, with reasoning:** persist exclusivity on the cgroup tree
by marking the scope id (as `@dr` marks a delegate-ram scope) and re-derive the
hold from the adopted scan. Rejected on a structural ground: **after a restart
nothing drives the scan.** A `sliceQueue` — and therefore its evaluator goroutine
and its `ListConfines` scan — exists only while it has waiters
(`pruneAdmitQueue`). With no waiters there is no queue, so a `worker-admit`
arriving post-restart has nothing to consult. Closing that needs a new
daemon-level per-slice exclusivity cache with its own lifetime, refresh and
staleness rules — and *its* bugs would be in the **wedge** direction (a
populated-but-orphaned marked scope holds the slice for its lifetime). The gate
confirmed this is defensible rather than rationalisation. Recorded as the upgrade
path; pinned by test #34.

## 3. Scope of "exclusive" — and what it provably does NOT cover

While a job holds exclusivity on slice `S`:

1. No other **job-level** admission is granted on `S` — `aira confine` jobs and
   `aira run`-shaped scope-less admissions alike.
2. No `worker-admit` grant under any outer scope in `S` not belonging to the
   holder (§12.2 defines "belong").
3. **Sub-reservations of already-running jobs are NOT blocked** (§5.1). Blocking
   them does not make the slice more exclusive; it only stops running jobs from
   finishing, so the drain never converges.
4. The holder's own work is unaffected: its aitest workers, its `confine-reserve`
   sub-reservations, and nested `aira confine` calls from its own process tree.

### 3.1 Coverage limits — stated in the flag help and the trailer, never implied away

Field's item 4 makes this non-optional: a user who believes exclusivity covers
more than it does gets contaminated numbers that look clean, which is the exact
failure this ticket exists to end.

- **Docker containers structurally escape the slice entirely.** `dockerd` lives
  in `/system.slice/docker.service`, and — verified on this box, cgroup v2 with
  the systemd cgroup driver — each container gets its own
  `/system.slice/docker-<id>.scope`, a **sibling of the service**, not a child of
  the daemon's cgroup. A `docker run` invoked inside `aira confine` therefore
  confines only the **client** process, which merely talks to a socket; the
  container lands outside `aira.slice` entirely. **Exclusive slice acquisition
  cannot and will not exclude a running container.** This holds in *every*
  configuration: even a container placed under `aira.slice` with
  `--cgroup-parent` would not carry a `.aira-CONFINE-*` name and so would stay
  invisible to every scan here. (Investigated separately by peer session `field`;
  not in this ticket's scope to fix — only to not overclaim.)
- Exclusivity is over **AIRA-admitted work**, not every process on the machine. A
  process placed into `aira.slice` by hand is not covered.
- It is **advisory to exactly the degree the RAM ledger is**: a bystander client
  that cannot reach the daemon, or is refused an admission slot
  (`E_DAEMON_BUSY`), takes the pre-existing flock fallback and lands in the slice
  regardless. (With the daemon fully down there is no hold either, so the common
  case is self-consistent.) Exclusive *requesters* no longer use that fallback at
  all — §9.1.

## 4. Surface

### 4.1 CLI
`aira confine --exclusive -- <argv>`, a valueless launch-form flag beside
`--delegate-ram` / `--detach` in `parseConfineArgs`. `parseConfineManagementArgs`
(the `--list` / `--kill` form) keeps rejecting it, so `aira confine --exclusive`
with no `--` argv is an argument error, not a silent no-op.

### 4.2 MCP
No new MCP surface. Confine *launching* is CLI-only by design; only
`confine --list` / `--kill` are exposed. `aira_confine_list` gains the exclusive
line automatically via the same `ConfineListResult`.

### 4.3 Daemon wire
Three optional args on `admit`, validated in `validateAdmitArgs` (whose strict
allow-list and `len(args)` bound both widen): `exclusive` (bool),
`exclusive_holder` (string, §8), `parent_scope_id` (string, §5.1).

`DaemonProtocolVersion` is **not** bumped. v1 justified this by claiming
old-daemon compatibility "vice versa", which was **wrong**: an older daemon
rejects an unknown field with `CodeProtocol`, which v1's client turned into a
flock fallback and a silently non-exclusive launch. §9.1's fail-closed rule is
what actually makes not bumping correct — the client refuses rather than
degrading, so a version skew is loud.

## 5. Daemon: the grant rule

All inside `evaluateAdmitQueue`, under `queue.mu`, in the existing single
per-queue evaluator goroutine: no new lock, no new goroutine, no new
lock-ordering obligation.

### 5.1 First, classify each waiter — the P0 fix

Three structurally different populations share one queue, and v1 fatally
conflated the second with the first:

| Population | On the wire | Drain / hold treatment |
|---|---|---|
| **Job-level scoped** — `aira confine` | `scope_id` + `name` + `owner` | Blocked |
| **Job-level scope-less** — `aira run` | no `scope_id`, no `parent_scope_id` | **Blocked** — genuinely new work entering the slice |
| **Sub-reservation** — `aira confine-reserve` | `parent_scope_id` set | **Exempt from the drain unconditionally**; exempt from the hold iff its parent is the holder or a holder-token waiter |

"scope-less ⇒ exempt" would be **wrong** — `aira run` is also scope-less and must
stay blocked. The discriminator has to be `parent_scope_id`, which does not exist
today and is added here.

`aira confine-reserve` runs inside the job tree and already inherits
`AIRA_CONFINE_SCOPE_ID` (`pylib/env.go:163`; the `worker-admit` CLI already reads
it). It sends that value as `parent_scope_id`. This also finally lets
`confine --list` attribute scope-less reservations to their job — a standing
AIRA-68 wish, obtained as a side effect rather than as new machinery.

Why exempt: `aira_xdist_governor/__init__.py` spawns `aira confine-reserve
--max-wait 300s` per test under a running `--delegate-ram` suite and, on timeout,
logs "running ungoverned" and proceeds. Blocking these means every test of every
running suite blocks 5 minutes then runs **uncharged** — a 100-test suite would
take ~8 hours to "finish naturally", the exclusive requester's own ceiling would
expire first, and the RAM governance the reservation exists to provide would be
silently dropped.

### 5.2 Then the gate, before the existing RAM fit check

Derive once per pass (one walk of the append-ordered `queue.waiters`):

```go
holder   := the waiter with exclusive && state == admitGranted
draining := the waiter with exclusive && state == admitQueued
```

For each queued waiter `W`:

1. `W` is a sub-reservation (§5.1) → **exempt**; fall through to the RAM check.
   Under a hold, additionally require its parent to be the holder or a
   holder-token waiter.
2. `holder != nil && W != holder` → stay queued, `W.waited = true`.
3. `draining != nil && W != draining` → stay queued, `W.waited = true`.
4. `W == draining` → require the slice **provably empty** (§6); if not, stay
   queued; if yes, fall through to the RAM check.
5. Otherwise unchanged.

`holder`/`draining` are re-derived against live counters as the loop grants, so
one pass can never grant two exclusive waiters.

**No new arrival can starve a registered exclusive request** (field's item 2,
second half). Rule 3 blocks *every* other queued waiter the instant an exclusive
waiter is enqueued, so a stream of short unrelated jobs cannot keep jumping in
ahead of it. Only work **already running** at request time delays the drain, and
that is exactly the owner's decision. The exclusive request has absolute priority
from the moment it registers.

**AIRA-59 freeze — verified against source.** Waiters blocked by rules 2–3
`continue` *before* `if waiter.reserve > available` (`admit.go:1070`), so they
never reach the arm at `:1094` and a drain never arms or advances the freeze
anchor. The drain head, if it does not fit, is subject to the freeze like any
other head. The anchor surviving a drain phase-shifts one duty cycle — the
existing holder-fit behaviour (`:1129-1135`), not a new defect.

**RAM/CPU admission (ticket item 5).** Exclusivity is a gate placed *before* the
existing checks and never replaces them. The drain head falls through to the same
`checkedAvailable` arithmetic and the same `E_ADMIT_TOO_LARGE` pre-checks. An
exclusive job that does not fit on its own merits is not granted merely because
the slice is empty. AIRA-64's CPU gate lives on `worker-admit` and is untouched.

### 5.3 At most one exclusive waiter per slice (ticket item 5, answered by construction)

`enqueueAdmitInternal` refuses an `exclusive` request when a waiter matching

```go
exclusive && (state == admitQueued || state == admitGranted)
```

already exists on that queue (`E_ADMIT_EXCLUSIVE_ACTIVE`, terminal).

**The predicate is exactly those two states, and that is load-bearing.** The
obvious spelling — `state != admitReleased` — also matches `admitRejected`, and a
timed-out or §6.2-aborted exclusive waiter whose handler is a zombie (the
unexplained socket case §2.2 itself bounds at ~30 min) would sit in the list as
`admitRejected` and refuse **every** future exclusive request on that slice until
a daemon restart. That is an unbounded, feature-level wedge — precisely the class
this plan says must be unrepresentable — reintroduced through the back door of the
guard meant to simplify things. Naming the same two states the derived
`draining`/`held` predicates use keeps the refusal and the gate reasoning about
identically the same set, so they cannot drift.

The refusal is race-free: taken under `queue.mu` inside `enqueueAdmitInternal`,
beside the existing duplicate-scope-id check.

Without this guard, exclusive requesters chain: E2 becomes the drain head the
instant E1 releases, and ordinary waiters starve indefinitely. The gate deemed
that acceptable-if-documented; I take the stronger option because it is *also*
simpler — "fairness among multiple simultaneous exclusive requesters" becomes
unrepresentable rather than something to arbitrate.

Cost: two benchmarks cannot queue behind each other; the second gets an immediate
typed refusal and must retry itself, which beats sitting for 30 minutes. The help
text states the retry idiom.

### 5.4 Argument validation — three refusals

- `exclusive` without the `scope_id`/`name`/`owner` tuple → refuse. A scope-less
  exclusive could never be attributed or reaped.
- `exclusive && exclusive_holder != ""` → refuse. A nested exclusive inside a
  hold can never satisfy rule 4 (`outstandingJobs >= 1` by construction) and
  would sit until its ceiling.
- `AIRA_ADMIT_EXCLUSIVE_WAIT_CEILING > AdmitWaitCeiling` → refuse at startup.
- `parent_scope_id`, when present, must parse with `runner.ParseConfineScopeID`,
  exactly as `scope_id` does. It is only ever compared for equality, so a
  malformed value would be harmless in practice — but this codebase has one
  scope-id grammar and one parser precisely because a second, looser acceptance
  path once admitted ids the scanner then could not see.

## 6. "Slice is empty" — subtree-aware, and fail-closed

The drain head is granted only when, in the same locked pass:

- `queue.outstandingJobs == 0` — **strict, with no discount for exempt
  sub-reservations**. A discount would be unnecessary (a running job's own scoped
  lease already keeps `outstandingJobs >= 1`, and a post-restart adopted parent is
  caught by subtree-aware `liveScopes`) and it would *remove* a belt-and-braces
  signal: a live reservation whose parent is an escapee or a zombie is exactly the
  case where the strict count is the only thing still objecting.
- `queue.liveScopes == 0`,
- `queue.liveScopesKnown` — the scan **succeeded**.

The third is the fail-closed rule and the most important line here. Granting on
an unestablished emptiness would tell a benchmark "you are alone" when the daemon
does not know that — a fabricated pass. Ordinary admission keeps degrading as it
does today; the asymmetry is deliberate and safe in each direction.

### 6.1 Liveness must be SUBTREE-aware — the P1 fix

v1 counted `ConfineRecord.Populated`, which comes from `scope.Members()` — leaf
`cgroup.procs` (`confine_manage_linux.go:107`, `cgroup_linux.go:248`) — and
justified it with "an aitest outer scope keeps its supervisor in the leaf".
**That is false.** `BootstrapAitestSupervisor` calls `drainIntoScope(outerScope,
scope)` (`aitest_bootstrap_linux.go:99`), moving *every* pid out of the outer
scope into `<outer>/.aira-supervisor`; workers go to `<outer>/.aira-worker-N`. A
running aitest outer scope therefore reads `Populated == 0`.

While the suite's own connection is held, `outstandingJobs > 0` still catches it.
But after a daemon restart — which `aira install` performs on every daemon
rebuild, routine on this box — the suite becomes an *adopted* scope,
`outstandingJobs == 0`, `liveScopes == 0`, and the exclusive requester would be
granted **alongside a running aitest suite** and told it was alone: exactly the
contention this ticket exists to eliminate, reported as its opposite.

Fix, with in-tree precedent: `killConfine` already observes population
subtree-aware via `cgroup.events` `populated` (`confine_manage_linux.go:432-445`),
and `linuxScope.Empty()` opens `cgroup.events` lazily off the already-open scope
fd. Add `SubtreePopulated *bool` to `ConfineRecord`, computed in the same scan;
count a scope as live when `SubtreePopulated == nil` (unevaluated is not empty)
or `*SubtreePopulated`.

`record.Pending` is deliberately **not** in that predicate: the evaluator's scan
calls `ListConfines` with a **nil registry** (`admit.go:921`), so `Pending` rows
can never appear there, and a connection-held pending grant is already covered by
the strict `outstandingJobs == 0`. Including it would be a vacuous clause that
reads like a safety property — worse than its absence, because a later reader
would trust it.

Cost: one extra ~100-byte `pread` per scope per second. The measured scan slope is
~16µs/scope (`BenchmarkListConfinesByScopeCount`), so this is noise, nowhere near
AIRA-61's per-poll O(tree) class.

`liveScopes` deliberately is **not** `adoptedJobs`: that figure skips
non-finite-cap and connection-held scopes, correct for *reserve* accounting and
wrong for *emptiness* — a skipped scope is still a running job.

### 6.2 A drain that cannot establish emptiness ABORTS — it must not stall the machine

If the scan keeps failing, the drain head cannot be granted (§6) while rules 2–3
block everyone else: the **whole shared slice stalls for the full ceiling**
because of a cgroupfs read error — a machine-wide outage from a diagnostic
failure.

So: one anchor instant, `queue.scanFailingSince`, in AIRA-59's
derive-from-one-anchor idiom. **The arm rule is:**

```
on a FAILED scan:  if scanFailingSince.IsZero() { scanFailingSince = now }   // never renewed
on a SUCCESS:      scanFailingSince = time.Time{}
```

"Set when a scan fails **after a success**" — the v2 wording — would never arm in
this rule's own primary case: a slice that is unreadable from the queue's very
first pass (the likeliest persistent failure, since the queue is created on demand
and its first scan is its first contact with the path) has no preceding success,
so the anchor would stay zero, the abort would never fire, and the whole slice
would stall for the full ceiling — defeating the section entirely. Arming on the
first failure when the anchor is zero, and never renewing it on later failures,
is the same shape `freezeArmedAt` uses and for the same reason: a renewed anchor
can postpone its own deadline forever.

If the scan has been failing longer than `exclusiveEstablishGrace` (30s) while an
exclusive waiter is draining, the waiter is **rejected**
(`U_ADMIT_EXCLUSIVE_UNESTABLISHED`), releasing the drain so ordinary admission
resumes on the next pass. Because the grace is measured from when the scan
**started** failing — not from when the request arrived — an exclusive request
arriving during an already-long-failing scan aborts on its first pass. That is
desirable: the daemon already knows it cannot establish emptiness, so making the
requester wait 30s to be told so would be theatre.

The abort leaks nothing. It takes the identical path as `timeoutAdmitWaiter`:
state → `admitRejected`, `close(grantedCh)`, and the handler's deferred
`release()` removes the waiter. The `draining` predicate stops matching the
instant the state changes, so the drain lifts before the waiter is even removed.

**Two mechanics the implementation must get right:**

1. `admitConnection`'s rejection write site (`admit.go:797-801`) **hardcodes
   `E_ADMIT_SATURATED` for every `admitRejected` waiter**. It must branch on
   `waiter.outcome`, or an abort would reach the client mislabelled as saturation
   — reporting "the slice was busy" for what is actually "the daemon could not
   read the slice". That is a fabricated diagnosis of exactly the kind §6 exists
   to prevent.
2. The client's §9.1 refusal must carry **the daemon's own code and message**
   when one was answered, not only the generic "requires a daemon admission
   grant". An operator must be able to tell "another benchmark holds the slice"
   (`E_ADMIT_EXCLUSIVE_ACTIVE`) from "the daemon could not establish emptiness"
   (`U_ADMIT_EXCLUSIVE_UNESTABLISHED`) from "your daemon is too old"
   (`CodeProtocol`) from "the daemon is saturated" (`E_DAEMON_BUSY`). Both new
   codes join the terminal pre-payload block beside `E_ADMIT_WAIT_TOO_LONG`
   (`admission_linux.go:408`), so they are handled *before* the structured-payload
   branch and can never degrade into the flock fallback.

Direction: **the benchmark fails loudly; the machine keeps working.**

## 7. What everyone else sees — honest, distinct, retriable

### 7.1 Blocked clients WAIT, exactly as under ordinary contention
A blocked waiter is simply not granted, the daemon holds its connection, and the
client stays blocked in `readRunnerAdmitFrame`. **No error frame is sent**, so a
drained-out bystander never routes into `admitWithFlock` — which would launch it
outside the ledger and defeat the exclusivity being waited for.

### 7.2 The waiting client's progress line
AIRA-24 already gives a blocked launcher a periodic progress line from a
short-lived `confine-list` probe on a **separate** connection (never multiplex the
admit socket — the daemon reads one byte from it purely as peer-death detection).
`ConfineSliceReserve` gains:

```go
// nil means NO exclusivity is active — a positive fact derived in the same
// locked snapshot as everything else here, never "unknown".
Exclusive *ConfineExclusiveState `json:"exclusive,omitempty"`

type ConfineExclusiveState struct {
    State       string `json:"state"`         // "draining" | "held"
    Name        string `json:"name,omitempty"`
    Owner       string `json:"owner,omitempty"`
    ScopeID     string `json:"scope_id,omitempty"`
    WaitingJobs int    `json:"waiting_jobs"`
}
```

giving `…, queue position 2 of 5 by enqueue order, 8GiB queued ahead, slice
draining for exclusive job "bench-fft" (mark)`.

### 7.3 `confine --list` (ticket item 6)
One extra line in `renderConfineListResponse`, inside the existing
`if result.SliceReserve != nil` block, in AIRA-73's idiom:

```
slice reserve: 12GiB granted / 48GiB ceiling across 3 admitted jobs
slice exclusive: held by "bench-fft" (mark), 4 jobs waiting
```

**Printed unconditionally including `slice exclusive: none`**, following the
AIRA-68 "of which:" line below it: a line that vanishes when there is no
exclusivity is indistinguishable from one that vanishes because the daemon
predates the feature. The enclosing `SliceReserve != nil` guard already covers
the one case where nothing can be established — `dispatcher.go`'s daemon-down
fallback produces no `SliceReserve`, and printing "none" there would be fabricated.

### 7.4 A blocked client whose own wait expires
Still `E_ADMIT_SATURATED`: `validRunnerAdmitRejection` pins `Basis ==
"reject:saturated"`, and an expired wait *is* saturation from the client's side.
What changes is that the payload says **why**: `admitRejection` /
`runnerAdmitRejection` gain `Exclusive string` (`"draining"` / `"held"` / absent),
rendering `— slice held exclusively by another job for benchmarking`. `Basis` and
the validation predicate are untouched, so this is additive.

### 7.5 `worker-admit` under a foreign outer scope
A genuine new exact-match token, `WorkerAdmitReasonSliceExclusive =
"slice-exclusive"`, as `State: denied, Class: contended` — **retriable**, the same
shape as ordinary contention, so `supervisor.py` raises the retriable
`WorkerAdmitDenied` and polls rather than hitting `_disable_daemon` and running a
whole suite unconfined. AIRA-63 records that a wrongly-classed denial once did
exactly that. Verified: `supervisor.py` validates state and class but not reason,
and `internal/pylib/worker_admit_channel_test.go` pairs only states and classes,
so a new reason token needs no Python change.

## 8. Nested `aira confine` from inside the exclusive job

Without this the feature self-deadlocks on its primary use case: CLAUDE.md (HARD)
requires every heavy command be prefixed `aira confine`, and a nested call
resolves to the *same* slice and creates a **sibling** scope (`confineWithDeps` →
`deps.newBackend(path)`, `confine_linux.go:446`), which the holder's own gate then
blocks until that inner call's `max_wait` expires.

- On an exclusive grant, confine puts `AIRA_CONFINE_EXCLUSIVE=<scope-id>` into
  the job's environment; the whole process tree inherits it.
- A nested `aira confine` reads it and sends `exclusive_holder: "<scope-id>"`.
- The daemon exempts a request from rules 2–3 **only** when `exclusive_holder`
  equals the live holder's `scopeID` on that queue.

Three plumbing constraints, each of which would otherwise silently break it:

1. `AIRA_CONFINE_EXCLUSIVE` must **not** be added to `governorEnvironmentKeys`
   (`pylib/env.go:95-105`) — verified that `AIRA_CONFINE_SCOPE_ID` *is* in that
   map, so `StripGovernorEnvironment` on every nested launch would drop the token
   and two-level nesting would deadlock.
2. It must be set **unconditionally** on an exclusive grant, not behind the
   `runtimeDir`/`extractForChild` gate `AIRA_CONFINE_SCOPE_ID` sits behind
   (`env.go:150-163`).
3. The detach path (`confine_detach_linux.go:569,664`) must carry it too.

Reusing `AIRA_CONFINE_SCOPE_ID` is **wrong**: it is upserted per nesting level, so
a grandchild would carry the intermediate id, not the holder's.

**Honest security note:** a session that learns a live holder's scope id could
forge the variable and be admitted during a benchmark. Blast radius is a degraded
measurement, never a containment failure. The unforgeable alternative is
`SO_PEERCRED` → `/proc/<pid>/cgroup` ancestry; this codebase has already declined
that machinery once for a comparable case (`confine_reaper.go:135-145`), so it is
the recorded upgrade path, not built.

## 9. The MUST-KNOW result — exclusivity is never silent

This section is field's item 1, and it is the highest-priority property in the
plan: *a feature that silently degrades to non-exclusive is actively worse than
no feature, because it produces contaminated numbers that look clean.* Three
mechanisms, covering acquisition, in-run loss, and machine-readability.

### 9.1 Fail closed on acquisition — refuse, never proceed non-exclusively

v1 left the flock fallback untouched. Traced, that is a silent false pass in five
distinct ways: an older daemon rejects the unknown `exclusive` field with
`CodeProtocol` → no terminal-code match in `admission_linux.go:400-451` →
`fail()` → `admitWithFlock` → **the job launches outside the ledger, not
exclusive, with no warning**. The same on daemon-down, on `E_DAEMON_BUSY`, on
`s.stopping` mid-wait, and on the two `unevaluated` grant paths (`admit.go:717`,
`:731`) which `validRunnerAdmitGrant` accepts.

So in `Runner.admit`, when the request is exclusive, **any** outcome other than a
daemon `immediate`/`waited` grant is a terminal refusal
(`E_CONFINE_UNAVAILABLE: --exclusive requires a daemon admission grant`) — never
flock, never an `unevaluated` launch; `NoAdmit`/`disabled` are refusals too. A
benchmark that cannot be given exclusivity must fail, not run and be believed.

### 9.2 The job can check, from inside, whether it got exclusivity

`AIRA_CONFINE_EXCLUSIVE` (§8) is already exported into the job's environment on
— and only on — a granted exclusive admission. It is therefore *also* the
job-visible attestation, at zero extra machinery: a benchmark script can gate
itself with

```sh
[ -n "$AIRA_CONFINE_EXCLUSIVE" ] || { echo "not exclusive; refusing to record"; exit 1; }
```

Because §9.1 makes a failed acquisition a refusal to launch at all, the variable
is present whenever the job runs under `--exclusive`. Its dual role (nesting
token and attestation) is deliberate and documented — one fact, one carrier.

### 9.3 The machine-readable outcome on the exit path

The confine trailer (`confine: slice=… admission=… terminated-by=…`) gains an
`exclusive=` facet, **always rendered when `--exclusive` was requested**, in the
exact idiom AIRA-70/91 established for `terminated-by` ("Always rendered: before
this facet existed, a SIGKILLed job's trailer was byte-identical to a clean run's
… an unset field reads as unevaluated, never as normal"):

| Value | Meaning |
|---|---|
| `exclusive=granted` | held for the whole run; the measurement is uncontended as far as §3.1's limits allow |
| `exclusive=lost` | granted, then the lease closed mid-run (§9.4) — **treat the measurement as contended** |
| `exclusive=unevaluated` | requested, but the outcome could not be established |

`granted` additionally carries the drain wait (`drained-for=…`), which is already
computed as `waitedMS` — so the result carries its own acquisition conditions at
no cost. This is the "trailer field or exit code" signal field asked for; a
harness greps one line rather than inferring from prose.

### 9.4 Losing exclusivity mid-run

After a grant, `admissionResult.release` holds the lease connection. Confine reads
it; EOF/ECONNRESET before teardown means the lease is gone. Under `--exclusive`
only, confine writes a warning **and** sets `exclusive=lost` in the trailer:

```
aira: warning: exclusivity lost (admission lease closed) — this run was no longer
scheduled alone; treat any measurement from it as contended
```

The job is **not** killed — killing a benchmark because the daemon restarted is
worse than reporting it.

**Builder trap that would invert this facet.** `releaseAdmission` closes the
lease connection on the way out of `confineWithDeps`, so after a perfectly clean
run the watcher's read fails with `use of closed network connection` — and a naive
implementation would stamp `exclusive=lost` on **every successful exclusive run**,
turning the honesty facet into a permanent false alarm. The watcher must be
stopped, or its error classified behind a teardown-started flag, *before* that
close. Test #32's `granted` case must therefore drive a **real full exit path**,
not a synthetic grant, or it will not catch this.

**Precision on what attests what:** the env var (§9.2) attests **acquisition** —
it cannot be unset inside an already-running process after a mid-run loss. The
trailer (§9.3) attests **the run**. They are complementary, not redundant, and a
harness that needs "was this whole run clean?" must read the trailer.

**Stated limit:** this covers daemon restart/stop, where the socket actually
closes. It is blind to a **ledger-only** release (the stale-lease reaper or
`releaseActiveConfine`), where the waiter is discharged but the handler stays
parked on `peerCtx`/`stopping` (`admit.go:824-827`) so the socket stays open and
the client's read never returns. v1 claimed "never silently downgraded"; the
honest claim is "never silently downgraded *when the lease connection closes*".

## 10. Bounding the wait (field item 2) — no new flag needed

`AdmitWaitCeiling` is **24 hours** (`confine.go:42`). An exclusive request at that
ceiling would hold `aira.slice` against every session for a day. Two bounds:

- **User-facing**: the existing `--admit-timeout` already sets this job's
  admission wait and is transcribed to `AdmissionMaxWait`. `aira confine
  --exclusive --admit-timeout=10m` is exactly field's "refuses cleanly after a
  bound" — a typed refusal (`E_ADMIT_SATURATED`, §7.4, now naming the exclusive
  reason). Reusing it beats a second timeout flag with its own precedence rules.
- **Systemic**: exclusive requests get their own ceiling (default 30 min,
  `AIRA_ADMIT_EXCLUSIVE_WAIT_CEILING`) so nobody can request a day-long drain,
  enforced by **refusal, never silent substitution** (AIRA-58's rule, which exists
  because a silent clamp made a caller wait the wrong duration with no way to
  learn the truth). Refused with `CodeAdmitWaitTooLong`, already catalogued at
  exit 2 and already handled as terminal *before* the structured-payload branch,
  so it can never degrade into the flock fallback.

The HOLD itself is unbounded, like any confine job's runtime, ended by the job or
by `aira confine --kill`.

## 11. Deadlock, livelock and convergence

- **Drain converges.** Granted jobs are never blocked; their sub-reservations
  (§5.1) and workers (§12.1) keep flowing, so they finish. With both exemptions
  the drain converges to "the longest-running admitted job finishes", bounded for
  the requester by §10. *Without* the §5.1 fix it provably does not converge
  inside the ceiling whenever a delegate-ram suite is running.
- **Every wait is bounded**: ordinary waiters by their own `max_wait`; the drain
  head by §10 and by §6.2's abort; the hold by the job ending.
- **No cycle exists.** The drain head waits on running jobs; running jobs wait on
  nothing the drain controls.
- **No new lock.** Everything is derived under the existing `queue.mu` in the
  existing evaluator goroutine. The worker-admit read takes `admitRegistryMu` →
  `queue.mu` in the established order, *before* the outer-scope lock and the CPU
  gate, so no new nesting.
- **Self-deadlock closed** by §8 and §12.2. **New-arrival starvation of the
  exclusive request closed** by §5.2.

Inherited non-convergence cases, stated rather than discovered later — both
fail-closed and bounded by the exclusive ceiling: a granted waiter that never
creates its scope holds `outstandingJobs > 0` until its socket closes
(`confine_reaper.go:118-128`); a scope whose `cgroup.events` cannot be opened
counts as live indefinitely.

## 12. `worker-admit` and exclusivity

### 12.1 During a DRAIN: allowed
A worker is an already-running job's internal progress. Structurally safe:
`CreateWorkerScope(ctx, outerScope, workerID, …)` only ever creates
`.aira-worker-*` **children inside an existing outer scope**, and an unreadable
outer scope yields `unevaluated`. So `worker-admit` **cannot** introduce new
top-level work into the slice — it can only let admitted jobs progress, which is
what makes the drain converge.

### 12.2 During a HOLD: denied except for the holder's own work — the P1 fix

v1 exempted only the holder's own outer scope. But §8 lets a nested `aira confine`
under the holder be admitted, and that creates a **sibling** scope; if it is
`--delegate-ram`, its `worker-admit` carries `outer_scope = <nested scope>`
(`AIRA_AITEST_OUTER_SCOPE` = `scope.Reference()`, `confine_linux.go:872`), whose
base is not the holder's. So `aira confine --exclusive -- make test` → `aira
confine --delegate-ram -- pytest` — the shape CLAUDE.md's own rule produces —
would hang its workers until the nested job's ceiling.

**The mapping, spelled out** (v1 never said how an outer scope maps to a waiter):
a scope directory is named `.aira-<scopeID>`. An outer scope belongs to the holder
iff `filepath.Base(outerScope) == ".aira-" + W.scopeID` for some granted waiter
`W` on that queue where `W == holder || W.exclusiveHolder == holder.scopeID`.

### 12.3 Making sure the gate is not inert
`outer_scope` is `filepath.Clean`ed but not symlink-resolved, while `admitQueues`
is keyed by `resolveAdmitSlicePath`'s `EvalSymlinks`ed path. A mismatch would make
the gate silently never fire — the "shipped operationally inert" failure this
project has had once (the reason AIRA-64 reports `CPUSlots` at all). So resolve
the outer scope's parent with `EvalSymlinks` (falling back to `Clean`), **and**
prove the gate fires with a real-cgroup test (§13.3 #33).

## 13. Tests

Every heavy command runs `aira confine -- …`, serialised, never concurrently.

### 13.1 Unit — the gate (fake clock, fake scan)

False-fail (must NOT block what it should allow):
1. No exclusive waiter → ordinary admission byte-identical to today.
2. Exclusive granted when the slice is empty and it passes its own RAM check.
3. **A `confine-reserve` sub-reservation from a running FOREIGN suite is granted during a drain** (P0).
4. **The holder's own sub-reservation is granted under a hold** (P0).
5. The holder's nested request (`exclusive_holder` matching) granted under hold.
6. `worker-admit` under the holder's own outer scope granted under hold.
7. **`worker-admit` under a holder-TOKEN nested scope granted under hold** (P1, §12.2).
8. `worker-admit` under a foreign outer scope granted during a DRAIN (§12.1).

False-pass (must actually block, and must not fake a pass):
9. **An `aira run`-shaped scope-less job-level admit is BLOCKED during a drain** —
   this is what stops #3 from being porous; without it an implementation that
   exempted every scope-less waiter would pass #3.
10. Drain queued → an ordinary waiter that WOULD fit is not granted.
11. Hold granted → an ordinary waiter that would fit is not granted.
12. Slice non-empty (`outstandingJobs > 0`) → exclusive not granted.
13. `liveScopes > 0` → not granted.
14. **Scan failed → not granted** (fail-closed, §6).
15. `SubtreePopulated == nil` counts as live → not granted.
16. A second exclusive request refused at enqueue (§5.3); nothing enqueued.
17. Over-ceiling exclusive refused with `E_ADMIT_WAIT_TOO_LONG`; nothing enqueued
    (a refusal must not itself start a drain).
18. The three §5.4 argument refusals.
19. `worker-admit` foreign scope under hold → denied, **`Class: contended`**,
    `Reason: slice-exclusive` — asserting the CLASS, since a wrong class is the
    unconfined-suite safety regression.
20. §6.2: persistent scan failure → the drain **aborts**; blocked waiters granted.
21. A stream of newly-arriving ordinary waiters never overtakes a registered
    exclusive waiter (§5.2, field item 2).

Un-wedge / crash-safety:
22. Drain queued → peer closed → drain releases; blocked ordinary waiter granted.
23. Hold granted → peer closed → hold releases.
24. Exclusive times out on its own `max_wait` → drain releases.
25. `s.stopping` during a drain → releases.
26. The stale-lease reaper does **not** reclaim a held exclusive whose scope is
    populated, and **does** when its scope has vanished (§2.3).
27. After each of the above, `Exclusive == nil` and the queue is pruned.
28. **Anti-porousness wedge probe**: 50 request→abandon→re-request cycles, each
    asserting an ordinary waiter is still granted. A test exercising one release
    path would pass against an implementation that leaks on another.

Assertions must be non-porous: "not granted" is asserted as `state ==
admitQueued && waited` after N evaluator passes driven through the
`admitAfter`/kick seams — never a sleep.

### 13.2 The must-know result (§9) — field item 1
29. Daemon absent → `--exclusive` refuses and **never reaches `lockAttemptFn`**.
30. Daemon answers `CodeProtocol` (old-daemon simulation) → refuses, no flock.
31. Daemon answers `unevaluated` → refuses, does not launch.
32. Trailer renders `exclusive=granted` with `drained-for=` on a held run;
    `exclusive=lost` after a mid-run lease close; and **the trailer of a
    `--exclusive` run is never byte-identical to a non-exclusive one** — the
    AIRA-91 shape of assertion, which is what stops the facet shipping inert.
    Plus: `AIRA_CONFINE_EXCLUSIVE` is present in the job env on a granted
    exclusive run and absent otherwise (§9.2).

### 13.3 Test isolation and real-cgroup tests

Many concurrent sessions are running real `aira confine` work on this machine
right now; a test that drained the real `aira.slice` would starve them. Isolation
uses two existing mechanisms unchanged:

- `cgrouptest.IsolatedScopeParent(t)` creates a `.aira-test-<TestName>-XXXX`
  directory. Note what that genuinely is: a **sibling of live production scopes**,
  since the parent of the test binary's own cgroup *is* `aira.slice` when the
  suite runs under `aira confine`. Isolation is carried entirely by the **name** —
  every production scan enumerates `.aira-CONFINE-*` and nothing else, and
  `cgrouptest` asserts the prefixes cannot collide. No test may create a
  `.aira-CONFINE-*` directory outside its own throwaway parent.
- The daemon's `admitResolveSlice` / `admitConfineScan` / `admitReadMemory` seams
  point a test `Server` at the throwaway directory as its slice path, so the queue
  under test is never the real `aira.slice` queue.

All skip via `cgrouptest.SkipOrFailRealCgroup` rather than passing vacuously.

33. **The un-wedge against a real process death.** A real `Server` on a real unix
    socket over a real throwaway slice. A **helper subprocess** (the
    `os.Args[0] -test.run=…` pattern in `watchdog_test.go:1268`; real sockets in
    `protocol_test.go:25`) opens a real admission connection, requests
    `exclusive`, is granted. Assert held-exclusive and that a second admission is
    not granted. Then **`SIGKILL`** that subprocess — the uncleanest death
    available, nothing the daemon is told. Assert the blocked admission is granted
    promptly and `Exclusive == nil`. **This is the test that proves the slice
    un-wedges with no operator intervention**, on the real kernel path.
33b. **`worker-admit` gate is not inert (§12.3), as its own explicit assertion** —
    a real `.aira-CONFINE-*` outer scope inside the throwaway slice, a real hold,
    and a `worker-admit` naming that path → denied with `slice-exclusive`.
    Deliberately *not* folded into #33 as an "also covers": "also covers" is
    exactly how a gate ships inert, which is the failure §12.3 exists to prevent.
34. **Subtree-aware emptiness against a real tree** (§6.1): a real adopted scope
    whose **leaf is empty but whose `.aira-supervisor` child is populated** — the
    real aitest layout — must **block** the exclusive grant. This is the exact
    shape v1 got wrong, so it replaces v1's "test that the blind spot exists".
35. Restart fail-open, encoded as a test: a `Server` holding an exclusive lease is
    stopped and a fresh one started on the same real slice; admission is granted.
    This pins the accepted gap (§2.4) so a future change silently making restart
    fail-CLOSED — the wedge direction — breaks a test instead of a machine.
36. `confine --list` renders the exclusive line for a real held slice, and
    `slice exclusive: none` otherwise.

### 13.4 Full suite
`aira confine -- go build ./...`, `go vet ./...`, and the full
`aira confine -- go test ./...` — exit codes recorded exactly, never inferred from
truncated output, reported as `pass` / `fail` / `unevaluated`.

## 14. Files touched

| File | Change |
|---|---|
| `internal/daemon/admit.go` | `admitWaiter.exclusive` / `exclusiveHolder` / `parentScopeID`; derived drain/hold; §5.2 gate; §5.3 refusal; §5.4 validation; `liveScopes`/`liveScopesKnown`/`scanFailingSince`; §6.2 abort; exclusive ceiling; `admitRejection.Exclusive`; snapshot exposes exclusive state |
| `internal/daemon/worker_admit.go` | §12.2 gate before the outer-scope lock; new reason token |
| `internal/daemon/confine_manage.go` | populate `ConfineSliceReserve.Exclusive` from the same snapshot |
| `internal/daemon/protocol.go`, `internal/codes/codes.go` | `U_ADMIT_EXCLUSIVE_UNESTABLISHED` (3), `E_ADMIT_EXCLUSIVE_ACTIVE` (1) — `produced_test.go` enforces both directions |
| `internal/runner/confine_manage.go` / `_linux.go` | `ConfineExclusiveState`; `ConfineSliceReserve.Exclusive`; `ConfineRecord.SubtreePopulated` + subtree-aware scan (§6.1) |
| `internal/runner/confine.go` | `ConfineRequest.Exclusive`; `AdmitExclusiveWaitCeiling`; trailer `exclusive=` facet (§9.3) |
| `internal/runner/confine_linux.go` | `AIRA_CONFINE_EXCLUSIVE` into the job env (§8 constraints); §9.4 lease watcher |
| `internal/runner/confine_reserve*.go` | send `parent_scope_id` (§5.1) |
| `internal/runner/admission_linux.go` | send the three new args; §9.1 fail-closed; `runnerAdmitRejection.Exclusive` |
| `internal/runner/worker_admit_outcome.go` | `WorkerAdmitReasonSliceExclusive` |
| `internal/runner/confine_queue_position_linux.go` | exclusive clause on the progress line |
| `internal/pylib/env.go` | ensure `AIRA_CONFINE_EXCLUSIVE` is NOT stripped |
| `cmd/aira/main.go` | `--exclusive` parse; transcription; `--list` line |
| `internal/core/core.go`, `internal/core/skill.go` | verb spec + operator guidance (paired `skill_test.go`) |

## 15. Explicit deferrals

- Tree-persisted exclusivity surviving a daemon restart (§2.4) — deferred with
  reasoning and upgrade path; pinned by test #35.
- `SO_PEERCRED` ancestry instead of the §8 env token — deferred; forgery costs a
  degraded measurement only.
- The flock fallback is untouched **for bystanders** (§3.1). Exclusive clients no
  longer use it at all (§9.1). Closing it for bystanders is a much larger redesign
  of advisory degradation.
- **Ambient contention telemetry (load average at grant/release)** — field's item
  3, explicitly judged out of scope. The trailer already carries `drained-for=`
  (free, already computed), which is the acquisition condition that matters most.
  A loadavg facet is a new telemetry source with its own unevaluated-handling
  obligations; cheap to add later against §9.3's existing facet if wanted.
- **Docker containment** (§3.1) — not fixed here; documented so exclusivity is
  never overclaimed. Handled separately by peer session `field`.
- **Accepted deviation from §5.4:** `AIRA_ADMIT_EXCLUSIVE_WAIT_CEILING` warns
  once and substitutes (the default, or the shared ceiling) rather than refusing
  at startup. The AIRA-58 sin is the SILENT substitution, which is gone — a bad
  value is now loud in the daemon log. Refusing at startup would take down the
  daemon for every session over one operator's typo in a tunable nobody sets.
- Exclusivity across different slices — out of scope; per-slice, matching every
  other admission concept here.

## 16. Expected yield

A coordination primitive whose failure mode is fail-open with a stated ~30-minute
worst-case bound and no operator step; whose un-wedge is proved against a real
kernel with a `SIGKILL`ed supervisor; which **refuses to run rather than fake
exclusivity it could not obtain**, tells the job from inside whether it has it,
and stamps the answer on the exit path so a contaminated run can never be mistaken
for a clean one. Risk retired: a global self-perpetuating wedge of a machine-wide
shared resource, and the silent-contamination failure that motivated the ticket.
Risk accepted: a silently-lost hold across a daemon restart, mitigated by a loud
warning plus `exclusive=lost`, and pinned by a test.
