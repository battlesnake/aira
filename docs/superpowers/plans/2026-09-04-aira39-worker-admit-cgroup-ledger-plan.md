# AIRA-39 / AIRA-41 / AIRA-63 — worker-admit ledger tracks the real cgroup tree

**Date:** 2026-09-04
**Status:** v3 — revised after two independent plan reviews (Sol/Codex and
DeepSeek-pro), both **GATE-FAIL** on v1, plus a Sol re-gate that **GATE-FAIL**ed
v2 with four narrower findings. Accepted findings are marked `[v2]` / `[v3]`;
the findings **not** accepted are answered explicitly in §5 rather than
silently dropped.
**Tier:** A (full two-loop) — correctness-critical daemon admission code every
session on this shared machine depends on.
**Parent plan:** [`2026-09-04-backlog-remediation-plan.md`](2026-09-04-backlog-remediation-plan.md) §3.1 (Phase 1, Fix 1).
**Base:** `origin/master` `8996999` (Phase 0 merged) + the AIRA-91 probe commit
`2751ad3` (tests-only, parent plan §8's ordering correction).

## 1. Problem

`internal/daemon/worker_admit.go` accounts aitest worker-scope memory in a pure
in-memory ledger:

- `workerJobState.grants map[workerID]*workerGrant` holds `memoryMax` per grant.
- `committed = Σ job.grants[*].memoryMax` is the aggregate worst-case guard.
- A grant is inserted by `evaluateWorkerAdmit`; the CLI
  (`cmd/aira/main.go:1111`, `runWorkerAdmitCommand`) then creates the actual
  cgroup with `runner.CreateWorkerScope`.
- A grant is dropped by `releaseWorkerGrant` when the relay connection closes.

Three confirmed defects follow:

**AIRA-39 (P0).** A daemon restart voids the map. The first `worker-admit`
afterwards sees `committed = 0` even though N live worker scopes with real
`memory.max` caps still exist under the outer scope, so the guard admits an
(N+1)th worker whose cap plus the existing caps can exceed the outer ceiling —
the outer scope's `memory.oom.group` then kills the whole run (supervisor plus
every sibling worker). `workerScopeOwner` is also reset, so an unrelated
`job_id` can claim a still-active `outer_scope`.

**AIRA-41 (P2).** Relay death is not worker death. An abnormally killed
`aira worker-admit` relay frees the ledger entry while the worker itself is
still alive under its still-intact cap — the same over-commit window.

**AIRA-63 (P2).** `workerAdmitConnection` retains a connection and a polling
goroutine per waiter with **no** `admitSlots` gate, unlike `admitConnection`.

There is also a grant→creation window: the grant is recorded by the daemon and
the scope is created afterwards by a *different process*, so a client that dies
in between leaves a ledger entry with no scope.

## 2. Design

**The ledger becomes the cgroup tree.** `committed` is `Σ memory.max` over the
existing `.aira-worker-*` children of the outer scope, read from cgroupfs — the
same source of truth the slice-wide reserve ledger already adopted in #74
(`evaluateAdmitQueue`'s adopted scan, `admit.go:851-965`). Nothing about the
count survives only in RAM, so a restart reconstructs it and a dead relay cannot
free it.

### 2.0 `[v2]` Outer-scope identity is canonicalised before anything keys on it

`validateWorkerAdmitArgs` accepts `outer_scope` as any non-empty string today
(`worker_admit.go:314-330`), while `linuxScopeBackend.Create` `filepath.Clean`s
its parent (`cgroup_linux.go:188-192`). Under v1's design the per-outer-scope
mutex was the *only* serialisation, so `/x`, `/x/`, and `/x/.` would take three
different mutexes while mutating one cgroup — v1's unconditional
"ownership deletion is safe" claim was wrong for that reason.

So `validateWorkerAdmitArgs` now **requires an absolute path and stores
`filepath.Clean(outer_scope)`**. Every downstream use — the memory read, the
supervisor-scope path, the map key, the mkdir parent — is then the same byte
string by construction. (Symlink aliasing is not a live concern: cgroupfs
exposes no symlinks, and the value originates from `CurrentCgroupPath()` /
`AIRA_AITEST_OUTER_SCOPE`, which `cmd/aira/main.go:1042` already cleans and
requires absolute.)

### 2.1 State: keyed by outer scope, not by (job, outer scope)

`workerJobs map[jobID\x00outerScope]*workerJobState` and
`workerScopeOwner map[outerScope]jobID` collapse into

```go
type workerScopeState struct {
    lock        chan struct{} // 1-buffered; a mutex you can select on  [v2]
    nextSeq     int
    committed   int64
    committedAt time.Time // last scan ATTEMPT, successful or not
    scanned     bool
    scanErr     string
}
// Server: workerScopes map[string]*workerScopeState  (key: cleaned outer scope)
```

Why ownership tracking goes away rather than being ported:

- `workerScopeOwner` existed because the guard summed **one job's own** grants,
  so a second job on the same outer scope was invisible to it. Summing the
  scope's actual children removes that hole at the source. `reject:outer-scope-owned-by-another-job` is deleted with it.
- This is a behavioural **improvement**, not just a deletion: a second
  aitest-enabled pytest run inside one confine job is a real, documented
  scenario (`cmd/aira/main.go:1020-1029`). Today it is refused terminally and
  its queue is marked `unevaluated`; under this design both runs share one
  correctly-summed ledger.
- The *lock* must then be per **outer scope**, not per job — otherwise two jobs
  on one outer scope would evaluate and create concurrently under two different
  locks and both grant against the same pre-create sum. Re-keying is what makes
  ownership-free accounting safe; it is load-bearing, not cosmetic.
- `nextSeq` becomes per-outer-scope, which is what it always effectively was.

`workerScopes` is still never pruned — the same accepted slow-growth gap as
today, now one entry per outer scope instead of one per (job, outer scope)
pair, i.e. strictly smaller.

### 2.2 The scan, its rules, and its cadence bound

```go
type workerScopeChildren struct {
    committed int64 // Σ memory.max over .aira-worker-* children
    maxIndex  int   // largest numeric suffix seen, for nextSeq reconstruction
    count     int
}
func scanWorkerScopeChildren(outerScope string) (workerScopeChildren, error)
```

Rules, each fail-closed in the direction that cannot silently under-count:

- Enumerate `outerScope`'s entries; consider every **directory** whose name has
  the prefix `.aira-worker-`. `[v2]` **The cap of every such directory is
  charged, whatever its suffix looks like** — v1 charged only numeric suffixes,
  which under-counts an externally created or oddly named worker scope
  (`WorkerScopeChildPath` accepts any slashless id, `worker_scope.go:16-20`).
  The suffix is parsed **only** for `maxIndex`, and a non-numeric or
  out-of-range suffix simply does not contribute to `maxIndex`.
- `.aira-supervisor` is excluded by the prefix (it is deliberately uncapped and
  is charged separately by the existing supervisor-RSS read).
- `[v2]` `memory.max` read fails with `ENOENT` → **re-`Stat` the child
  directory**. Directory gone → skip (the benign race with `supervisor.py`'s
  `_retire_worker` rmdir). Directory still present → **error**: an existing
  child with no `memory.max` means the memory controller is not delegated, which
  is an anomaly, not a vanished worker. (v1 treated every `ENOENT` as "vanished",
  which would have silently under-charged in exactly that case.)
- Any other read error, or a value that is not a finite non-negative integer
  (`max`, malformed, negative) → **error**, and the admit answers `unevaluated`.
  This is the project's own rule (a check that cannot establish its result never
  reports a fake zero) and it cannot arise in the normal flow: every worker
  scope is now created by the daemon itself with a verified finite `memory.max`.
- `[v2]` Sums use the existing overflow-safe `addClamp`; `maxIndex` is clamped
  and a `nextSeq` that cannot advance without overflowing is a create failure,
  not a wrap.

**Cadence (the AIRA-61 CPU-regression class).** `evaluateWorkerAdmit` runs once
per poll per waiter (default 200 ms). An unbounded per-poll walk is exactly the
O(tree)-scan-per-poll regression AIRA-61 already found and fixed once (25–65 %
supervisor CPU, `af407be`). So:

- the per-outer-scope sum is **cached**, refreshed at most once per
  `workerScopeScanInterval` (default `admitConfineScanIntervalDefault`, 1 s) —
  the same throttle shape `evaluateAdmitQueue` uses;
- `committedAt` records the last scan **attempt**, successful or not, so a
  failing filesystem does not turn every poll into another scan;
- the cache is invalidated **synchronously** on the daemon's own successful
  create, and `[v2]` **on an `EEXIST` create failure** (see §2.3) — the one
  interleaving where the cache is proven stale-low;
- otherwise a stale cache is stale-**high** (a retired worker still counted),
  which over-charges: it denies, it never over-admits.

The scan and the create are both reached only while holding the outer scope's
lock, so one lock serialises "read the sum, then add to it".

`[v3]` **A cached sum may never be the basis of a GRANT.** Sol's re-gate found
that `EEXIST` only detects a *same-path* race: a `.aira-worker-foo` appearing
after the last scan is invisible to the cache and does not collide with the
numeric id the daemon is about to create, so the cache can be stale-**low** on
the one path where that is unsafe. Resolved without giving up the cadence bound,
because the two paths have completely different frequencies:

- the **denial** path (contention, re-polled every 200 ms per waiter) uses the
  cached sum, refreshed at most once per second — this is the hot path AIRA-61's
  regression was about;
- the **grant** path — reached at most once per worker actually admitted, i.e.
  a handful of times per suite, never per poll — **forces a fresh scan and
  re-checks the aggregate guard against it** before creating anything. If the
  fresh sum no longer leaves room, the request is denied retriably
  (`fallback:aggregate-cap-exceeded`) exactly as if the cache had been current.

So the cache can only ever cause an extra *denial*, never an admission, and the
poll loop still performs at most one scan per second per outer scope. The
property test's claim is stated in those terms.

`[v3]` **Overflow: the guards compare by addition, not subtraction.** Sol's
re-gate found the existing `req.estimatedBytes > ceiling-committed-supervisorUsed`
form (`worker_admit.go:279`) can wrap **positive** once `committed` saturates at
`MaxInt64` (e.g. `ceiling=0`, `committed=MaxInt64`, `supervisorUsed=2` →
right-hand side wraps to `MaxInt64` → grant). A cgroup `memory.max` near
`MaxInt64` is a finite value the scan legitimately charges, so this is reachable
once the sum is tree-derived rather than bounded by `admitMaxReserve` per grant.
Both guards are therefore rewritten in the all-non-negative, saturating form:

```go
if addClamp(req.estimatedBytes, used) > ceiling { /* fallback:insufficient-headroom */ }
if addClamp(addClamp(req.estimatedBytes, committed), supervisorUsed) > ceiling { /* fallback:aggregate-cap-exceeded */ }
```

`addClamp` (`admit.go:254-258`) saturates at `MaxInt64`, so a saturated total is
always `> ceiling` and always denies. A dedicated overflow test pins it.

### 2.3 Daemon-side scope creation closes the grant→creation window

`evaluateWorkerAdmit`, still holding the outer scope's lock, calls **the
existing** `runner.CreateWorkerScope(ctx, outerScope, workerID, memoryMax,
memoryHigh)` — which already writes `memory.max`, the `memory.high` guard and
`memory.oom.group=1`, and already removes the directory if the cap write fails.
No second daemon-local scope-creation implementation is written. The daemon
package already imports `internal/runner` and a `!linux` stub of
`CreateWorkerScope` exists, so no build-tag work is needed.

Because the scope exists before the response is written, a grant is never paired
with a missing scope, and the ledger's source of truth is populated by the same
critical section that decided to grant.

**Worker id allocation.** `nextSeq = max(state.nextSeq, scan.maxIndex) + 1`.

`[v2]` **`EEXIST` is not a terminal failure and is not "retry with the next
id".** Both reviewers independently found v1's bounded-retry-on-`EEXIST` unsafe:
an `.aira-worker-N` the cache did not see proves the cached sum is **stale-low**,
so creating `worker-N+1` would grant against a sum that omits N — the exact
over-admit AIRA-39 is about. Instead, on `fs.ErrExist`:

1. invalidate the cache for this outer scope (mark `committedAt` zero), and
2. return `{State: "denied", Reason: "fallback:worker-scope-id-collision"}`.

That is retriable, so the caller's own poll loop re-evaluates within one
interval against a **fresh** scan that now includes the child, and either grants
correctly or denies correctly. No retry loop, no attempt counter — strictly
less code than v1.

**Fail-closed denial class for every other create failure.** No grant is
recorded and the response is

```
reject:worker-scope-create-failed:<cause>
```

`reject:`-prefixed and therefore terminal: it routes through `supervisor.py`'s
classifier (`:652-659`) to `WorkerAdmitRequestTooLarge`, which marks the queue
`unevaluated` rather than retrying. `[v2]` The rationale is corrected: not every
such failure is *provably* non-transient (`ENOMEM`, `EINTR` exist), but
`BootstrapAitestSupervisor` (`aitest_bootstrap_linux.go:14-31`) has already
enabled `cgroup.subtree_control` before any worker-admit call runs, so the
realistic cause is broken daemon-side cgroupfs access; and a non-`reject:`
reason would be retried **indefinitely** by `_wait_for_admission_or_disable`,
stalling every aitest run on the machine rather than the one job that hit it.
Terminal is the fail-closed choice under that asymmetry.

### 2.4 `[v2]` Protocol version IS bumped: 5 → 6

v1 claimed no bump was needed because the JSON shape is unchanged. **Both
reviewers rejected that as the plan's most dangerous claim, and they are right
against source.** Moving *who creates the cgroup* changes wire **semantics**,
and each mixed-version direction ends in a silent loss of containment:

- **old client + new daemon:** the daemon creates the scope; the old client then
  calls `CreateWorkerScope` on the same path, gets `EEXIST`, and prints
  `worker-admit local-placement-failed` (`cmd/aira/main.go:1113-1126`) →
  `supervisor.py` raises `WorkerPlacementFailed` → `_disable_daemon`
  (`supervisor.py:1095-1096`) → **the whole suite runs unconfined.**
- **new client + old daemon:** neither side creates the scope; `fork_worker`
  fails to place → `WorkerPlacementFailed` → the same `_disable_daemon`.

With `daemon.ProtocolVersion = 6` (and `runner.DaemonProtocolVersion = 6`, which
`internal/runner/protocol_pin_linux_test.go` already forces to move together —
Phase 0's AIRA-83 item 3), both directions instead hit `server.go:539-540`'s
mismatch frame carrying `E_DAEMON_PROTOCOL`, which the classifier treats as
terminal and the run reports `unevaluated`. **A loud `unevaluated` beats a
silent unconfined suite on a RAM-capped shared machine** — that is this
project's own honesty rule, and it is the whole reason AIRA-83(a) (already
merged) made stale clients refuse cleanly.

**Consequence for deploy, and an escalation to the orchestrating session:** the
parent plan §8 assigns the protocol bump to Fix 2 and lets Fix 1 "deploy on its
own restart". That assignment was made without analysing Fix 1's own
mixed-version semantics; it does not hold. Fix 1's deploy must therefore be an
**atomic reinstall of `~/.local/bin/aira` + daemon restart**, exactly like the
one §8 already specifies for Fix 2, and Fix 2 will then bump 6 → 7. This is a
deviation from the parent plan, deliberately taken and reported rather than
silently absorbed.

### 2.5 AIRA-63 — bound the connection on `admitSlots`

`workerAdmitConnection` acquires the existing `admitSlots` semaphore
(`admitGlobalMax = 1024`) non-blockingly at entry and releases it on return; the
lazy-initialisation dance currently inlined in `admitConnection`
(`admit.go:633-646`) is factored into one shared
`acquireAdmitSlot()`/`releaseAdmitSlot()` pair used by both — a deletion of
duplication, not new machinery.

**Saturation must NOT be an error frame.** `admitConnection` answers saturation
with `writeAdmitError(conn, CodeBusy, …)`. Doing the same here would reach
`RequestWorkerAdmit`'s `response.Code != "OK"` branch
(`worker_admit_client_linux.go:97`), producing
`E_CONFINE_UNAVAILABLE: worker-admit request rejected: …` — a message matching
none of `supervisor.py`'s denial substrings, so it falls through to
`WorkerAdmitUnavailable` → `_disable_daemon` → **the whole suite runs
unconfined.** That is a safety regression, not a denial.

So saturation is emitted through the ordinary terminal-response path as
`WorkerAdmitResponse{State: "denied", Reason: "fallback:admit-slots-saturated"}`
(`core.Response{OK:false, Code:"OK"}`). The client renders `worker-admit denied:
fallback:admit-slots-saturated`; `supervisor.py` sees `"worker-admit denied"`
**without** `reject:` and raises the retriable `WorkerAdmitDenied`, which
`_wait_for_admission_or_disable` retries indefinitely with periodic stderr
warnings — the intended behaviour for transient slot saturation, bounded by
slots freeing up. The `fallback:` prefix is a human-readable convention, not a
matched token; Fix 2 folds it into a structured `state=`/`reason=` channel later.

`[v2]` **Convoy mitigation (both reviewers, P1).** Sharing one semaphore means a
waiter blocked on a hot outer scope's lock holds an admit slot. Two mitigations,
both cheap:

- the per-outer-scope lock is a 1-buffered channel acquired with `select` on the
  peer-disconnect / daemon-stopping contexts, so a waiter whose peer has gone
  releases its slot immediately instead of blocking uninterruptibly. (Today's
  `job.mu` is explicitly documented as "uninterruptible and not itself
  deadline-aware", `worker_admit.go:192-213`; this is a strict improvement over
  the status quo, not a new hazard introduced by the scan.)

  `[v3]` **Lifecycle, specified rather than left implicit** (Sol re-gate P1):
  `workerScopeFor(outerScope)` does an atomic get-or-create under the existing
  `workerJobsMu` (renamed `workerScopesMu`), exactly as `workerJobFor` does
  today (`worker_admit.go:87-109`), and constructs `lock` as
  `make(chan struct{}, 1)` at that moment — so two concurrent first callers can
  never end up with two states or a nil channel. Acquisition is
  `select { case st.lock <- struct{}{}: ... case <-ctx.Done(): return false }`,
  and the **only** statement that may follow a successful acquisition is
  `defer func() { <-st.lock }()`, mirroring today's `defer job.mu.Unlock()`
  (`:212-213`). A test asserts that a waiter cancelled while queued on the lock
  leaves the token untaken and the next waiter proceeds.
- the scan is cached, so the lock is normally held for two cgroupfs file reads
  plus a map lookup; the mkdir + three small writes happen once per *granted*
  worker, not once per poll.

The residual coupling — a saturated aitest run consuming ordinary `aira confine`
admission slots — is real, accepted, and recorded: reaching it needs ~1024
concurrent worker-admit waiters, two orders of magnitude above any observed
suite. A dedicated worker-admit semaphore is the escape hatch if it ever bites;
adding one now would be new machinery against the parent plan's explicit
"gate on the existing `admitSlots` semaphore".

**Not done in this fix:** unifying `workerAdmitWaitCeilingMs` with
`runner.AdmitWaitCeiling`. `TestWorkerAdmitCeilingStaysBelowTheSharedAdmitCeiling`
stays; only its rationale comment is updated to say the bound now exists and the
unification is a deliberate follow-up.

### 2.6 Release path

**No daemon-side rmdir, on lease close or on a failed response write.** Under
`committed = Σ memory.max` over existing children, a worker scope that dies
without being rmdir'd keeps charging — the safe direction. The primary release
is `supervisor.py`'s existing `_retire_worker` (`:1040-1062`): it reaps the
worker, closes the relay stdin, then `os.rmdir(grant["scope"])`; the scope is
already empty by then. A daemon-side rmdir on lease close would be actively
wrong in the AIRA-41 case (relay killed, worker alive): the cgroup is still
populated, the rmdir would fail `EBUSY`, and the scope *should* keep charging
until the worker is reaped. Backstops: the outer confine scope's own client-side
teardown (`attestScopeTeardown`, `confine_linux.go:856`) removes the whole
subtree at job exit, and the #72 reaper covers a supervisor crash mid-retire.

`releaseWorkerGrant`, `workerGrant`, `workerJobKey`, `workerJobFor` and
`job.grants` are therefore **deleted**.

`[v2]` **v1 proposed one exception — removing the just-created scope when the
grant response write fails — and it is dropped.** The reviewers split on it
(Sol: drop; DeepSeek: keep with an `EBUSY` fallback), and Sol's reasoning is
correct against source: `writeFrameBytes` (`protocol.go:195-207`) surfaces the
underlying `Write` error and discards `n`, so "the client provably never learned
the path" is **false** — a fully delivered frame followed by a deadline or reset
error is possible. If the daemon removed the scope in that case, the client
would fork its worker into a removed cgroup → `WorkerPlacementFailed` →
`_disable_daemon` → the whole suite unconfined. Not removing it costs at worst
one empty scope charging until the job exits — an over-charge that produces a
*loud, retriable* denial, never a silent loss of containment. The existing
`TestWorkerAdmitConnectionReleasesGrantWhenResponseWriteFails` is rewritten to
assert this deliberate new behaviour (the scope survives and keeps charging)
rather than deleted.

### 2.7 What does NOT change

- The JSON shape of `WorkerAdmitResponse` and the CLI's
  `granted scope=… worker_id=… memory_max=… memory_high=…` line — so
  `internal/pylib/aitest/supervisor.py` needs **no change** (only a stale
  comment naming the deleted ownership rejection is corrected). The version bump
  in §2.4 is about semantics, not shape.
- `cmd/aira/main.go`'s `runWorkerAdmitCommand` drops its
  `runner.CreateWorkerScope` call and reports `lease.ScopePath`; everything else
  (the 1 MiB/1 PiB argument floor, the stdin-hold release) is untouched. The
  `local-placement-failed` marker becomes unreachable from this path and is
  removed with its call site.
- The live-usage check, the reclaimable-cache discount, and the supervisor-RSS
  term of the aggregate guard.

## 3. Tests (TDD; each named with the defect it would catch)

Unit, `internal/daemon` (seams: `workerScopeScan`, `workerScopeCreate`,
`admitNow`):

1. `TestWorkerAdmitLedgerReconstructsCommittedFromCgroupTreeAcrossRestart` —
   AIRA-39. A fresh `Server` (no in-memory state) whose scan reports two live
   700-byte worker scopes under a 1000-byte ceiling denies a third with
   `fallback:aggregate-cap-exceeded`. Against today's code this grants.
2. `TestWorkerAdmitLedgerKeepsChargingAfterRelayCloses` — AIRA-41. Grant, close
   the lease; the scan still reports the child → the next request is still
   denied. Asserts the *reason*, not merely non-grant.
3. `TestWorkerAdmitScanIsThrottledToAtMostOncePerInterval` — the AIRA-61
   regression class. 20 evaluations inside one interval perform exactly one
   scan (call counter); advancing the clock past the interval scans again; a
   successful create invalidates so the very next evaluation scans again.
4. `TestWorkerAdmitCreatesWorkerScopeBeforeGranting` — the granted response's
   `ScopePath` equals the path `workerScopeCreate` was called with, and a
   `workerScopeCreate` that is never called cannot produce a grant.
5. `TestWorkerAdmitDeniesTerminallyWhenScopeCreateFails` — reason is
   `reject:`-prefixed (so the classifier terminates) and names the cause.
6. `TestWorkerAdmitReconstructsNextWorkerIDFromExistingChildren` — with
   `.aira-worker-1..3` present and no in-memory seq, the next created id is 4.
7. `[v2] TestWorkerAdmitEEXISTInvalidatesCacheAndDeniesRetriably` — the
   collision returns `fallback:worker-scope-id-collision` (**not** `reject:`),
   forces a rescan on the next evaluation, and the retry after the rescan is
   denied when the unseen child pushes the sum over the ceiling. This is the
   over-admit v1 would have shipped.
8. `TestWorkerAdmitReturnsUnevaluatedWhenChildCapUnreadable` — a non-`ENOENT`
   scan error is `unevaluated`, never a zero sum.
9. `[v2] TestWorkerAdmitScanSkipsVanishedChildButNotACaplessOne` — a child whose
   directory is gone is skipped **and the committed sum excludes it** (asserted
   numerically, not just "still grants"); a child that still exists but whose
   `memory.max` is `ENOENT` is an error → `unevaluated`.
10. `[v2] TestWorkerAdmitChargesNonNumericWorkerScopeChildren` — a
    `.aira-worker-foo` child with a finite cap is charged, and does not
    contribute to `maxIndex`.
11. `TestWorkerAdmitConnectionDeniesRetriablyWhenAdmitSlotsSaturated` — AIRA-63.
    Fill `admitSlots`; assert `Code == "OK"`, `State == "denied"`,
    `Reason == "fallback:admit-slots-saturated"`, and — the load-bearing half —
    that the reason does **not** contain `reject:`.
12. `[v2]` Extend the existing end-to-end
    `TestWorkerAdmitCLIStderrClassificationMatchesSupervisorBoundary`
    (`worker_admit_cli_boundary_test.go`, which drives the real built CLI
    against a real `Serve`) with the saturation case, so the retriable
    classification is proven through the actual client and its actual stderr
    text — not only against the in-process response struct.
13. `TestWorkerAdmitConnectionKeepsScopeChargedWhenResponseWriteFails` — the
    rewrite of the existing write-failure test against §2.6's deliberate
    behaviour.
14. `[v2] TestWorkerAdmitTwoJobsShareOneOuterScopeAndAreCountedTogether` — a
    second `job_id` on the same outer scope is denied with the exact
    `fallback:aggregate-cap-exceeded` reason (never
    `reject:outer-scope-owned-by-another-job`, which no longer exists), proving
    the ownership deletion is covered by the sum.
15. `[v2] TestValidateWorkerAdmitArgsCleansAndRequiresAbsoluteOuterScope` —
    `/outer/` and `/outer/.` normalise to `/outer`; a relative path is refused.
16. `[v2] TestStoreWriteRelayUsesProtocolVersionSix` — `[v3]` this **updates the
    existing** `TestStoreWriteRelayUsesProtocolVersionFive`
    (`internal/daemon/protocol_test.go:64-67`, which currently asserts
    `ProtocolVersion == 5` and would otherwise conflict), rather than adding a
    second pin. Its comment names §2.4's reason so a later "consistency" revert
    cannot silently reopen the unconfined window.
    (`internal/runner/protocol_pin_linux_test.go` already forces the runner
    constant to move with it.)
17. `[v3] TestWorkerAdmitGrantAlwaysScansFreshBeforeGranting` — with a cache
    seeded stale-low and a scan seam that reports a newly appeared
    `.aira-worker-foo`, the request that the cached sum would have granted is
    denied instead, and the scan call count shows the forced refresh happened on
    the grant path only (the denial path still honours the 1 s throttle).
18. `[v3] TestWorkerAdmitAggregateGuardDoesNotWrapOnSaturatedCommitted` —
    `ceiling` small, `committed` at `MaxInt64`, `supervisorUsed > 0`: the
    request must be denied. Against the subtraction form this grants.
19. `[v3] TestWorkerScopeLockReleasesOnCancelledWaiter` — a waiter cancelled
    while queued on the outer-scope lock leaves the token untaken and the next
    acquirer proceeds immediately.

Real-cgroup, `internal/daemon` (guarded by `cgrouptest.IsolatedScopeParent` +
`SkipOrFailRealCgroup`):

20. `TestScanWorkerScopeChildrenSumsRealMemoryMax` — real children with real
    `memory.max` values sum exactly; `.aira-supervisor` and non-worker
    directories are excluded; a child with `memory.max=max` errors.
21. `[v2] TestEvaluateWorkerAdmitCreatesARealWorkerScope` — end-to-end against a
    real delegated cgroup: the grant's `ScopePath` exists, carries the exact
    `memory.max` granted and `memory.oom.group=1`, and a second evaluation sees
    it in the scanned sum.

Property test (parent plan §6 item 3, "ledger drift from the kernel object it
tracks"): `TestWorkerAdmitCommittedNeverBelowSumOfExistingCappedChildren` — over
randomised interleavings of grant / create / release / restart / rmdir /
external-create against a fake cgroupfs, the charged total is never below the
sum of the capped children that actually exist. `[v2]` The generator includes
external creates (the `EEXIST` path), non-numeric suffixes, and a child whose
`memory.max` read fails.

**Mutation checks to run and record** (build-review evidence, not a code read):
flip the throttle to "always scan" (test 3 fails); make the scan return 0 on
error instead of erroring (test 8 fails); drop the `reject:` prefix from the
create-failure reason (test 5 fails); *add* a `reject:` prefix to the saturation
reason (tests 11 and 12 fail); skip the create call while still granting
(test 4 fails); treat `EEXIST` as "advance the id and grant" (test 7 fails);
charge only numeric suffixes (test 10 fails); revert the version bump (test 16
fails); `[v3]` let the grant path use the cached sum (test 17 fails); revert the
guards to the subtraction form (test 18 fails).

## 4. Risks and explicit non-goals

- **CPU regression** — mitigated by §2.2's cache; test 3 is the evidence, and
  the PR records it.
- **Over-charging** — a stale-high cache, un-rmdir'd dead scopes, and the
  undelivered-grant scope all deny rather than over-admit. Accepted and stated
  on the PR; bounded by the outer confine job's own lifetime, since its
  client-side teardown removes the whole subtree at job exit.
- **`unevaluated` on an unreadable child cap** could stall a run. Accepted: it
  cannot occur in the normal flow (the daemon writes every cap itself), and a
  fabricated zero is exactly the AIRA-39 failure this fix exists to remove.
- **Shared `admitSlots` coupling** — §2.5, accepted with the escape hatch named.
- **Not in scope:** the structured outcome channel (Fix 2), the `memory.high`
  removal (AIRA-35, gated on an owner decision), the wait-ceiling unification
  (AIRA-63 follow-up), and any trailer/kill-attribution work (Fix 5).

## 5. `[v2]` Review findings NOT accepted, and why

- **DeepSeek P1, "the daemon should rmdir the scope when the response write
  fails (EBUSY → leave and log)".** Not accepted; see §2.6. The rmdir cannot be
  proven safe (`writeFrameBytes` discards `n`), and its failure mode is a
  *silent* unconfined suite, whereas the failure mode of leaving the scope is a
  *loud, retriable* denial. Sol's opposite finding is taken instead.
- **DeepSeek P1, "any job that knows an outer scope can now create workers under
  it and starve the legitimate owner".** Not accepted as a regression: any
  process inside the job could always `mkdir` there directly without the daemon,
  and `workerScopeOwner` never prevented that — it only bound `job_id` →
  `outer_scope` for *accounting*, which the tree sum now does correctly for all
  comers. The behaviour change (a second job is served rather than terminally
  refused) is the intended improvement documented in §2.1.
- **DeepSeek P2, "no GC for supervisor-crash-leaked empty scopes".** Accepted as
  a *known bound*, not a fix: leaked worker scopes are bounded by the outer
  confine job's lifetime and removed by its client-side teardown. Recorded in §4
  rather than answered with new reaper machinery (the #72 reaper deliberately
  scans `.aira-CONFINE-*`, not worker children).
