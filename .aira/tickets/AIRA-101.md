---
{"schema":1,"id":"AIRA-101","project":"aira","title":"aira confine --exclusive: let a job request exclusive use of its slice for uncontended benchmarking","status":"done","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","dogfood","scheduler"],"hold":false,"relations":[]}
---
Direct owner request (2026-09-05): "we need a way for jobs launched with 'aira confine' to request exclusive use of the slice, so they get scheduled alone with nothing else running in the slice" — for benchmarking, where contention from other admitted jobs (or aitest workers) invalidates a measurement.

## Decided by the owner — binding

**Active drain, not passive wait.** The moment a job requests exclusive access, the daemon stops admitting NEW work into that slice — already-running jobs finish naturally (not killed/interrupted), and once the slice goes empty the exclusive job is admitted alone. This is a bounded wait for the requester, at the cost of holding up other sessions' `aira confine`/`worker-admit` calls in that slice while it drains. Given this machine runs many concurrent sessions, that tradeoff was made deliberately over the alternative (never block anyone, but an exclusive request could wait indefinitely on a busy box) — do not revisit this call without asking again.

## Scope of "exclusive" — needs the plan's own precise definition, but the intent is

Nothing else running in the target slice while the exclusive job runs: no other `aira confine` job-level admission, and no `worker-admit` grants under any OTHER outer scope (an exclusive job's own aitest workers are fine — it's alone in the slice, so its own worker-admit calls trivially pass whatever governance already exists). The slice is whatever the job resolves via `--slice` (default `aira.slice`), matching every other per-slice admission concept in this codebase.

## Things the plan must resolve, not decided here

1. **Flag name and surface** — `aira confine --exclusive` (or better, if the plan finds one) on the CLI, MCP, and whatever daemon RPC/admission-request shape carries it.
2. **What other sessions see while draining.** Their `aira confine`/`worker-admit` calls should wait/retry using the SAME shape as ordinary contention (not a hard error) — but the reason reported must be honest and distinct from ordinary saturation (an operator waiting behind a drain needs to know a benchmark is running, not think the slice is merely full). New reason code(s), not an overload of an existing one.
3. **Crash-safety of the exclusive state itself.** This is the load-bearing correctness question. A pure in-memory "slice X is draining/held exclusive" flag that does not survive a daemon restart, or does not get released when the exclusive job's supervisor dies uncleanly, can wedge the WHOLE shared slice for every session on the machine indefinitely — a far worse failure mode than an ordinary admission bug, because it is global and has no self-healing path once stuck. This project's own precedent (AIRA-39/41's worker-admit ledger, AIRA-74/97's migration guards) is "the cgroup tree is the state, not memory" — the plan must apply the same discipline here: exclusivity should be re-derivable from live process/scope state (who currently holds it, is that process still alive) rather than trusted as a standalone daemon-memory bit. Work out exactly what "holds exclusivity" means in terms of the confine scope/supervisor PID and how a daemon restart, or an abandoned/killed exclusive requester, correctly and promptly un-wedges the slice.
4. **The exclusive requester's own abandonment.** If the process asking for exclusive access is Ctrl-C'd, crashes, or its own wait exceeds whatever ceiling applies, the drain must release rather than hold the slice hostage for a benchmark that was itself aborted.
5. **Fairness among multiple exclusive requesters**, and interaction with the existing RAM/reserve admission (AIRA-29's ledger) and AIRA-64's CPU-slots gate — exclusivity is an additional orthogonal gate layered alongside those, not a replacement; an exclusive job still needs to pass its own RAM/CPU admission checks on the merits.
6. **Visibility.** `aira confine --list` (and whatever else reports slice state) should say plainly when a slice is draining for / held by an exclusive job, matching this project's existing honesty-first precedent (AIRA-73's `--list` reserve-summary line) — an operator whose job is waiting needs to see why, not guess.

## Why this needs the full two-loop

Daemon-side admission/scheduling work is already this project's own named correctness-critical class (CLAUDE.md: "mandatory for ID allocation, crash recovery, lease CAS, and other correctness-critical work"). This is a new coordination primitive that, done wrong, can deadlock a shared machine-wide resource for every concurrent session — treat with at least the rigor of AIRA-64/74/97 tonight (plan, Fable gate, TDD build, adversarial build-review, merge only once both loops are satisfied).

## Resolution

Built by Opus (correctness-critical class), plan-gated by Fable over two rounds,
adversarially build-reviewed by Fable (BLOCK → fixes), with an orthogonal plan
review from DeepSeek and mid-build field input from peer session `field`.

Plan: `docs/superpowers/plans/2026-09-05-aira101-confine-exclusive-plan.md`.

### The design actually built

Exclusivity is a **derived property of an `admitWaiter`**, never a stored flag.
One field (`exclusive`) plus `exclusiveActive()` — which names `admitQueued` and
`admitGranted` *only* — and two predicates recomputed from `queue.waiters` on
every evaluator pass: `draining` (queued) and `held` (granted). There is no
`queue.exclusiveHeld` bit, no per-slice map, no on-disk marker and no new reaper.

That is the whole crash-safety argument: exclusivity has no representation that
can outlive the admission connection, the wait ceiling, or the daemon process,
so **a wedge of the machine-wide slice is not representable** rather than merely
avoided. Bounds are stated honestly rather than absolutely (§2.2 of the plan):
immediate on requester death / kill / restart, the exclusive ceiling for a queued
waiter, and `defaultStaleLeaseReleaseGrace` (15 min) in the codebase's own
documented zombie-socket case — **worst case ≈ 30 minutes, no operator step on
any path**, because there is no state for an operator to clear.

### Resolution of each open question

1. **Flag name and surface.** `aira confine --exclusive`, valueless, launch-form
   only (`parseConfineManagementArgs` still rejects it, so `--exclusive` without
   `--` argv is an argument error rather than a silent no-op). Wire: three
   optional `admit` args — `exclusive`, `exclusive_holder`, `parent_scope_id`.
   No MCP change: confine *launching* is CLI-only by design, and
   `aira_confine_list` picks up the new state automatically.
   `DaemonProtocolVersion` deliberately NOT bumped — safe only because of the
   fail-closed rule below, which turns an older daemon's rejection into a loud
   refusal instead of a silently non-exclusive launch.

2. **What other sessions see.** They WAIT, exactly as under ordinary contention
   — no error frame, so a blocked bystander never routes into the flock fallback
   (which would launch outside the ledger and defeat the exclusivity it waits
   for). Distinct, honest reasons: the AIRA-24 progress line gains a "slice
   draining for / held exclusively by" clause; `E_ADMIT_SATURATED` gains an
   additive `Exclusive` payload field (`Basis` untouched, so
   `validRunnerAdmitRejection` is unaffected); `worker-admit` gets its own new
   exact-match token `slice-exclusive` as `Class: contended` — **retriable**, which
   is load-bearing, since a terminal class there would make a whole pytest suite
   run unconfined (the AIRA-63 regression shape).

3. **Crash-safety.** As above. Proved by `TestSIGKILLingTheExclusiveHolderUnwedgesTheSlice`:
   a real helper **subprocess** claims the slice exclusively over a **real unix
   socket** against a real `Server`, is confirmed held, blocks a second
   admission, and is then **SIGKILLed** — the uncleanest death available, no
   cleanup, nothing told to the daemon. The test asserts the blocked waiter is
   granted and `Exclusive == nil`. Only a real process death exercises the kernel
   fd-close that actually un-wedges the slice; a mocked connection would prove
   nothing. Disabling the gate makes it fail, so it observes the hold for real.

4. **Requester abandonment.** Same mechanism — the admission connection IS the
   lease and `admitConnection`'s deferred release runs on every return path.
   Covered by tests for peer-close during drain and during hold, own-`max_wait`
   expiry, daemon shutdown, and a 50-round wedge probe rotating four
   structurally distinct release paths.

5. **Fairness + RAM/CPU interaction.** Made **unrepresentable** rather than
   arbitrated: at most one exclusive waiter per slice, refused at enqueue under
   `queue.mu` with `E_ADMIT_EXCLUSIVE_ACTIVE`. This also removes the
   back-to-back-exclusive starvation of ordinary work. Exclusivity is an
   ADDITIONAL gate placed before the existing checks and never replaces them: an
   exclusive job still faces the same `checkedAvailable` arithmetic, and AIRA-64's
   CPU gate is untouched. Blocked waiters `continue` before the freeze branch, so
   a drain never arms the AIRA-59 anchor.

6. **Visibility.** `confine --list` prints `slice exclusive:` **unconditionally**,
   including `none`, in the AIRA-68 "of which:" idiom — a line that vanished
   would be indistinguishable from an old daemon, so an operator could not use
   its absence to rule a benchmark out. Rendered only inside the existing
   `SliceReserve != nil` guard, since the daemon-down fallback establishes
   nothing and "none" there would be fabricated.

### Beyond the ticket: the must-know result (field input)

Peer session `field` reported an hour of contaminated Rust-FDTD-vs-openEMS
throughput numbers — the incident that motivated this ticket. That reshaped the
deliverable: a feature that silently degrades produces numbers that *look* clean,
which is worse than no feature. So `--exclusive` is **fail-closed end to end**
(refuses on any non-grant: no flock, no `unevaluated` launch, no
disabled/bypassed), the job can check `$AIRA_CONFINE_EXCLUSIVE` from inside
(attests ACQUISITION), and the trailer carries
`exclusive=granted|lost|unevaluated` plus `drained-for=` (attests the RUN), always
rendered when requested, in AIRA-70/91's `terminated-by` idiom.

### Coverage limits — documented, never implied away

- **Docker containers are NOT covered.** `dockerd` is in
  `/system.slice/docker.service` and each container is
  `/system.slice/docker-<id>.scope`, a SIBLING of the service — so `docker run`
  inside `aira confine` confines only the client. Holds in every configuration:
  even `--cgroup-parent` into `aira.slice` would not produce a `.aira-CONFINE-*`
  name. (Separately investigated by `field`; not fixed here, only not overclaimed.)
- Processes placed in the slice by hand are outside this by construction.
- Exclusivity is advisory to the same degree the RAM ledger is: a bystander that
  cannot reach the daemon takes the pre-existing flock fallback. (With the daemon
  down there is no hold either, so the common case is self-consistent.)
- `aira run` scopes are `.aira-RUN-*` under a project's own cgroup parent and are
  invisible to the confine scan; while such a run holds its connection it is
  still counted, so only a post-restart run is missed.
- The mid-run loss watcher sees the lease CLOSING; a ledger-only release leaves
  the socket open, so the honest claim is "never silently downgraded *when the
  lease closes*".
- Exclusivity does not survive a daemon restart — deliberately fail-OPEN, since
  the alternative failure direction wedges the slice for everyone. Pinned by
  `TestADaemonRestartReleasesExclusivityRatherThanWedgingTheSlice` so a future
  change making it fail CLOSED breaks a test instead of the machine.

### Verification

| Command | Exit |
|---|---|
| `aira confine -- go build ./...` | **0** |
| `aira confine -- go vet ./...` | **0** |
| `aira confine -- go test ./... -count=1` (FULL suite) | **0** |
| `aira confine -- go test -race ./internal/daemon ./internal/runner` | **0**, zero data races |

Live check against the real running (pre-deploy) daemon: `--exclusive` refused
with **exit 4** and the true cause — `E_CONFINE_UNAVAILABLE: --exclusive requires
a daemon admission grant; refusing to launch non-exclusively (E_DAEMON_PROTOCOL:
unexpected admit field "exclusive")` — while ordinary `confine` was unaffected.
That is the fail-closed path working end to end against a real old daemon.

**Mutation testing** (a green suite is not evidence): six mutations of
load-bearing assertions, all killed — the `exclusiveActive` predicate, inverted
subtree-populated, reverting the lease-deadline fix, removing the
sub-reservation exemption, dropping fail-closed emptiness, and restoring the
token strip.

**Honest caveat on flakes.** Two heavily-loaded sweeps (full suite plus a
`-race` sweep back to back, on a box already running other sessions' jobs)
produced intermittent failures in three timing-sensitive real-cgroup tests:
`TestRealCgroupCleanMultiProcessRunIsUnverified`,
`TestRealCgroupDescendantMigrationBeforeLeaderExitResidual` and
`TestRealPytestForkDoesNotHoldGovernorRelay`. All three are pre-existing and none
is reachable from this diff:

- The two descendant-escape tests are the documented AIRA-70 sampler-window gap
  (50ms `scopeMembershipSampleInterval` against a 250ms dwell), de-flaked in
  `11a5056` and accepted as #70's coverage gap. `realRunner` builds a Runner with
  no memory slice or socket, so `r.admit` returns `disabled` before the daemon
  path and never reaches this change at all.
- The relay test asserts a nanosecond ORDERING between a relay release and a
  child's 1.0s sleep (it failed by ~1.1ms). It drives `AIRA_GOVERNOR_CMD` with a
  Python helper script, so the `confine-reserve` change is not executed by it.

Each passed 3/3 on rerun, unmodified `origin/master` passed the same packages,
and the clean headline run above is exit 0. Recorded rather than omitted, and
the reviewer independently reached the same conclusion.

### Review findings and how each was handled

**Plan gate (Fable), round 1: GATE-FAIL** on four defects around a sound core —
all fixed in v2: the drain blocked `confine-reserve` sub-reservations (a running
`--delegate-ram` suite would stall 5 min/test then run *uncharged*, so the drain
could never converge); leaf-only emptiness would read a running aitest suite as
an empty slice after a restart (my v1 justification was factually wrong —
`BootstrapAitestSupervisor` drains every pid out of the outer scope);
`--exclusive` could reach the flock fallback; and the hold-time `worker-admit`
exemption missed nested holder-token scopes.

**Round 2: GATE-PASS** with two mandatory amendments, both applied: the
single-exclusive refusal predicate had to name `admitQueued`/`admitGranted` only
(the obvious `!= admitReleased` would let an aborted zombie waiter refuse every
future exclusive request until a daemon restart — a feature-level wedge
introduced by the guard meant to prevent starvation), and the scan-failure anchor
had to arm on the FIRST failure (arming only "after a success" would never fire
when the slice is unreadable from the queue's first pass — this rule's own
primary case).

**Adversarial build review (Fable): BLOCK** on three P1s, all fixed:
1. The admission **transport deadline survived onto the lease**. AIRA-101 added
   the first client-side reader of that connection, which made the latent
   deadline load-bearing: the read failed `i/o timeout` at `maxWait+grace` on a
   perfectly healthy connection, so **every exclusive benchmark outliving its own
   admission budget (30 min by default) reported `exclusive=lost`** and warned its
   measurement was contended when nothing had happened — inverting the honesty
   facet on exactly the long runs it exists for, and silencing the watcher so a
   real loss then went unreported. Fixed by clearing the deadline; the regression
   test lives at the fix site, because a test that cleared it itself would pass
   against a revert.
2. A non-exclusive nested launch **stripped the inherited holder token**, so at
   depth three (holder → `make` → `pytest`) the innermost job sent no token, was
   blocked by its own holder's hold, and stalled the holder behind it while the
   slice stayed held against every other session. Fixed by deleting the strip; a
   stale token is harmless because `belongsToHolder` matches only the live
   holder's unique scope id.
3. The `SubtreePopulated` **producer had no test at all** — an inversion or a
   leaf-only regression (v1's own P1) passed the whole suite. Added four producer
   tests including a real-cgroup one with a real process in a real
   `.aira-supervisor` child, plus the end-to-end facet-wiring test that would
   have caught (1).

**Re-review: APPROVE-WITH-FIXES**, with one required item that was a genuine
"shipped inert" finding: `runnerAdmitRejection` had no `Exclusive` field, so the
daemon's new drain/held reason was DROPPED on unmarshal and the operator's
terminal message still gave a memory diagnosis ("no memory admission within the
wait") for what was a benchmark holding the slice — making the daemon-side fix
inert at the one surface it existed for. Fixed with distinct wording for `held`
and `draining`, plus three client tests including a positive control asserting
ordinary contention KEEPS its memory wording (so the new clauses cannot be
satisfied by rewording every rejection). Four test-claim honesty nits were also
taken: the e2e deadline test no longer calls itself "the regression test" (it
clears the deadline in a fake dep, so it would pass against a revert — the
fix-site test is the real one); the wedge probe now counts three distinct paths
rather than claiming four; the stale-lease test's first half is relabelled as the
absent-scope direction it actually is; and the ceiling deviation is recorded in
the plan's deferrals.

P2s handled: the exclusive requester's own timeout no longer misreports as plain
saturation; `exclusive=unevaluated` is now reachable rather than a reader trap;
`parent_scope_id` is refused alongside `scope_id`; the ceiling override warns
instead of silently substituting; the wedge probe now rotates four genuinely
distinct release paths (the previous three all collapsed to one); the helper
process keeps its fd reachable; plan items #25/#26 are now tested; and the
coverage limits above are written into the code.

**Self-found, via `-race`:** 45 data-race reports, all test-induced — the tests
drove `evaluateAdmitQueue` concurrently with the queue's own evaluator goroutine,
breaking the single-writer property that function documents, and read waiter
fields without the lock. Production paths were clean, but a racy test is not
evidence, so a `sliceQueue.stopped` seam now lets a test become the sole
evaluator. The suite was green before this was found.

## Known boundary: exclusivity is slice-scoped, not machine-scoped (field, 2026-09-06)

Real adversarial measurement against a live, deployed grant (Rust FDTD benchmark,
9950X3D, five interleaved passes, grant held throughout) — quoted verbatim, this
project's own real-world validation of the coverage limits already stated in the
`--exclusive` help text:

Exclusive admission removes contention from *other confined jobs*. It cannot
remove contention from processes outside `aira.slice` (agent processes, `dockerd`
under `/system.slice/docker.service`, anything unconfined), and it cannot
partition the shared L3.

| bed | median MC/s | spread |
|---|---:|---:|
| shared bed, non-exclusive, load 12.8–24.2 | 474.4 | 1.46× |
| shared bed, exclusive, load 4.4–5.0 | 675.8 | 1.33× |
| 255³ DRAM-bound, exclusive | 351.6 | **1.03×** |
| 127³ cache-resident, exclusive | 2460.8 | **3.14×** |

Exclusivity moved the shared-bed median **+42%** and narrowed spread 1.46× →
1.33×. It did not remove it.

The split is mechanistic, not noise. The DRAM-bound bed's contended resource —
memory bandwidth — *was* held by the jobs exclusivity excludes, so it becomes
reproducible to 3%. The 127³ bed is 59 MB of f32 field arrays inside a 96 MB L3:
its throughput depends on a resource exclusivity does not own, so the grant buys
it nothing.

Evidence the residual is external rather than three independently noisy rows:
**the slow samples are time-aligned across rows.** One interleaved pass produced
the minimum of `coupled 4` (1010.3), `coupled 8` (1004.7) *and* `coupled 16`
(951.6) simultaneously, all near 1000 MC/s. Rows are measured round-robin, so one
pass being uniformly slow across three rows is one disturbance, not three.

**Guidance for callers.** Under an exclusive grant, a DRAM-bound measurement is
trustworthy to ~3%. A cache-resident measurement is not: on this machine it still
spreads 3.1× with the grant held, and any single-figure quote from such a row can
be wrong by up to 3×. Cache-resident beds need many more repeats or a quieter
machine — the grant is not a substitute for either. The failure mode this
guidance exists to prevent is not a bad number; it's a bad number combined with
the belief that the grant made it safe.
