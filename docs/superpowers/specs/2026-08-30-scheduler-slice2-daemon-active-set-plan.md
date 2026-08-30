# Scheduler Slice 2 — the daemon active-set cooperative scheduler (replaces the flock)

- **Status:** plan v2 (Sol + DeepSeek plan-review BLOCK folded; Fable code-gate pending). Owner-approved
  design: `2026-08-30-aira-cooperative-scheduler-design.md` §3.2, §4, §5.2. **Read the "Plan-review
  round 1" section at the bottom — v2 supersedes several v1 body claims (min-share, preemption latency,
  the connection-held read model, crash-safety symmetry, worker identity).**
- Depends on Slice 1 (DONE — cpu.weight aging governs *bootstrap*; this slice governs *execution*).
- **Deploys via a daemon restart** (daemon-side change) → batch the already-merged `done→in-progress`
  reopen transition; reserves reconstruct per #74, so the restart is safe.

## Problem

Today confined pytest CPU concurrency is governed by a machine-wide **flock slot dir** (#49): N slot
files = N concurrent active workers; a worker `flock`s a slot before each test, releases on finish.
Virtues — crash-safe for free (kernel drops the lock on death), zero-coordinator, hot-path-cheap.
Limits — it is a **flat cap**: proportional-by-contention sharing, no fairness, no young-first latency
(a big multi-worker job re-acquires slots continuously; a small newcomer gets only its ~1/N share); it
governs execution only; and it is CPU-count-shaped — it cannot express "admit by RAM headroom" (Slice 3).

Owner-decided 2026-08-30: **replace the flock with a daemon-owned cooperative scheduler** — the daemon
is the *sole* scheduler (no parallel fallback mechanism); daemon-down → the plugin runs ungoverned
(bounded by the slice `memory.max` + `oom.group`), a trivial safety net for a transient restart.

## Design (owner-approved §3.2)

The daemon owns the **active worker set** machine-wide. A worker is either **active** (may run tests)
or **parked** (blocked between tests). This slice sizes the active set by **CPU capacity** (≈ncores)
+ **fairness** (young-first, aging); RAM-aware sizing is Slice 3.

- **Checkpoint protocol.** The `aira_xdist_governor` plugin, at its existing ~10s gc/compaction
  checkpoint *and at worker start*, sends the daemon a small message over the existing Unix socket:
  `{worker_id, job_id, session, worker_age, held_rss, next_test_rss_estimate}`. The daemon replies
  **continue** or **park**. On **park** the worker blocks on the socket (long-poll) until the daemon
  wakes it — reusing the D4 admission connection-held long-poll pattern exactly. Steady state: ≤1
  check-in / 10s / worker ⇒ a few req/s total (NOT per-test — the hot-path cost the flock avoided
  stays bounded and small). `next_test_rss_estimate` is carried now (cheap) but only *consumed* in
  Slice 3; Slice 2 sizes by CPU + fairness only.
- **Active-set sizing (CPU).** `active-count ≤ CPU capacity`, where capacity = `NumCPU − reserve`
  (the same signal the flock's slot count used — reuse it verbatim, do not re-derive). A checkpoint
  from a worker already counted active returns **continue** while the set has room; a checkpoint from
  a parked/new worker is admitted when a slot is free, else it parks.
- **Cooperative preemption + fairness.** When a younger/smaller waiter wants in and the active set is
  full, the daemon marks an **older** active worker to **park at its next checkpoint** and wakes the
  waiter. Parking happens *between tests* (the worker is alive, no re-bootstrap) → nearly free. "Age"
  is measured as **worker/job age** (older = has-run-longer = yields first); exact tiebreak in Open
  decisions.
- **Anti-starvation (the load-bearing fairness invariant).** Young-first + aging must never park a
  long job (e.g. a merge-gate) *forever*. Guarantee a **minimum share**: every distinct job keeps
  **≥1 worker active** at all times, regardless of age — a long job slows under a stream of newcomers
  but never halts. Aging is bounded (a parked older worker is re-activated within a bounded interval
  even if newcomers keep arriving).
- **Crash-safety.** A worker's *active* grant is **connection-held**: the worker holds the socket
  open; if it dies (crash / OOM-kill / SIGKILL), the connection drops and the daemon frees the grant —
  exactly confine-reserve's model. A freed grant immediately triggers an active-set re-evaluation
  (wake a waiter). No leaked grant may permanently shrink the active set.
- **Bootstrap stays Slice 1.** execnet bootstrap precedes the plugin (execnet-internal) → the daemon
  cannot gate it; bootstrap safety remains Slice 1's cpu.weight-100 launch + `ionice` best-effort.
  The scheduler governs *execution*; the priority baseline governs *bootstrap*.

## Hard invariants (the two-loop MUST nail these)

1. **Crash-safety.** A dead/killed/OOM'd worker's active grant is always reclaimed (connection-drop),
   and a leaked/half-open grant never permanently shrinks the active set. A daemon restart with live
   workers reconstructs the active set from reconnecting workers (see Restart).
2. **Anti-starvation.** Bounded aging + a guaranteed **≥1 active worker per job** so every job keeps
   progressing; a long job finishes, just slower. No job is parked in full forever.
3. **Liveness / no deadlock.** The active set is never empty while any worker wants to run; park/
   activate always makes progress; on daemon *uncertainty* the reply is **continue** (fail-open),
   never park-everyone. No two-lock ordering that can deadlock the daemon (the #72/#74 lesson).
4. **Hot-path bound.** Steady-state check-in ≤1/10s/worker; a parked worker **long-polls** (no
   busy-wait, no re-poll loop); a freed slot wakes a waiter directly (no full poll cycle). A continue
   reply is prompt (no needless round-trip cost on the common path).
5. **Fail-open daemon-down.** NO test-execution correctness depends on the daemon. Socket refused /
   timeout / malformed reply → the plugin proceeds ungoverned (bounded by slice cap + oom.group).
   This is the ONLY behaviour when the daemon is unreachable — there is no flock fallback (removed).

## Flock removal (owner-decided)

Cut the CPU governor from the flock slot dir to the daemon and **remove the flock** entirely: the slot
dir, `_acquire_slot`, and the daemon `cpuslots` flock accounting (whatever of it is flock-backed).
Retain any daemon-side capacity signal that the scheduler still needs (e.g. the NumCPU−reserve
derivation) — decide in the buildable section from the map which of `cpuslots.go` is reusable vs
flock-specific and deleted. The Skill/docs references to the flock governor are updated.

## Restart / reconstruction

The active grant is connection-held, so a daemon restart drops every worker's connection → on the
first post-restart checkpoint each live worker re-registers and the daemon rebuilds the active set from
who reconnects (bounded by CPU capacity; older workers may briefly all be "new" to the daemon — that is
safe, it just re-sizes). No persistent ledger to reconstruct for the CPU set (unlike #74's RAM
reserves, which still reconstruct as today). State the transient: immediately post-restart the daemon
may over- or under-admit for one checkpoint interval (≤10s) until re-registration settles — acceptable
(bounded, self-healing, fail-open).

## Buildable design (code-grounded from the daemon map)

### Transport — mirror confine-reserve, do NOT teach Python the socket

The plugin today does not open the daemon socket; it shells out to `aira confine-reserve` (a Go
subprocess that holds the socket) and holds that subprocess's **stdin** as the release handle
(`__init__.py:312-344`; the CLI blocks on `io.Copy(io.Discard, stdin)` at `main.go:823-832`). Slice 2
**reuses this exact idiom** rather than duplicating the length-prefixed-JSON envelope + `ProtocolVersion`
in Python:

- New client subcommand **`aira governor-slot`** (name TBD) — a long-lived Go relay, ONE per xdist
  worker (fewer than confine-reserve's one-per-test). It resolves the socket via `PathsFromEnv()`
  (`paths.go:174-221`) exactly as confine-reserve does, dials the daemon, and holds the connection as
  the worker's **active grant** for the worker's whole lifetime. It speaks a tiny **line protocol** to
  the plugin over stdin/stdout: plugin writes a checkpoint line to the subprocess stdin, reads
  `continue` from stdout; the subprocess relays to the daemon and blocks the reply while parked.
- New env `AIRA_GOVERNOR_CMD` (the `aira` self-binary, set beside the existing `AIRA_CONFINE_RESERVE_CMD`
  in `env.go:93-95` / `confine_linux.go:653-665`) + `AIRA_GOVERNOR=daemon|off` kill-switch (default
  `daemon`; `off` ⇒ plugin runs ungoverned). No `AIRA_CPU_SLOTS_DIR` (removed with the flock).

### Daemon handler — a new connection-held verb built on the admit/watch skeleton

Add a `governor`/`schedule` branch to the `serveConnection` switch beside `admit` (`server.go:545-556`):
set `wrote = true`, `conn.SetReadDeadline(time.Time{})`, hand `conn` to a new
`Server.governorConnection(conn, args)` modelled on `admitConnection` (`admit.go:351-489`) for the
connection-hold + disconnect-detector (`admit.go:419-423`) and on `watch` (`server.go:580-591`) for the
read-loop shape. New daemon state: a machine-wide **active set** (genuinely new — no existing structure,
per the map) — a struct holding, per active/parked worker, its `grantedCh`/`wakeCh`, `jobID` (=confine
scope), `workerSeq`, `jobAge`, guarded by one mutex, with a single evaluator goroutine (clone
`runEvaluator`/`evaluateAdmitQueue` `admit.go:544-708`). **One lock, evaluate-then-signal** — no
scan-under-queue-lock (the #72/#74 deadlock lesson).

Handler loop (connection held throughout; any disconnect → deferred release → re-evaluate):
1. Read `acquire{jobID, jobAge}`. Enqueue as **parked**; when the evaluator admits it (capacity free
   AND fairness allows), flip to **active**, `writeFrame(active)`.
2. Loop: read `checkpoint{jobAge, heldRSS, nextTestEst}` (the ~10s ping). The evaluator may have set a
   **park-requested** flag on this worker (to admit a younger job). If set → mark **parked**, free the
   CPU slot, `signal()` (a waiter wakes), then block until re-activated → `writeFrame(continue)`. Else
   `writeFrame(continue)` immediately (the cheap common path). `nextTestEst` is parsed and stored but
   **not consumed** in Slice 2 (Slice 3 uses it).
3. Disconnect at any point (`conn.Read` goroutine → `cancelPeer`) → deferred release drops the worker
   from active/parked, `signal()`s → the freed slot wakes a waiter. Crash-safety identical to admit.

### Active-set sizing + fairness

- **Capacity** = `NumCPU − AIRA_DAEMON_CPU_RESERVE` — reuse `desiredCPUSlots` (`cpuslots.go:21-35`);
  lift that helper out before deleting the inode factory. `active-count ≤ capacity`.
- **Job identity = the confine scope.** All N workers of one `aira confine -- pytest -n K` share one
  `CONFINE-…` scope; the daemon already resolves the scope for the admit path (`validateAdmitArgs`
  `admit.go:835-912`). The relay passes its scope ID (resolved from its own cgroup, as confine already
  does) as `jobID`. This is the fairness + min-share unit — no `worker_id` from xdist needed (the map
  confirmed the plugin has none).
- **Young-first + aging.** Order waiters by job age (younger job's workers admitted first); an older
  active worker is the park target when a younger waiter needs a slot.
- **Anti-starvation (min-share, load-bearing).** The evaluator NEVER parks a job's **last** active
  worker: every distinct `jobID` with ≥1 worker present keeps ≥1 active regardless of age. A long job
  slows but never halts. Aging is bounded: a parked worker is guaranteed re-activation within a bounded
  number of evaluator passes (round-robin among parked workers of admitted jobs), so no worker parks
  forever even under a steady newcomer stream.

### Flock removal

Delete the Python flock (`_acquire_slot`/`_release_slot`/`_try_slots`/`_visible_slots`
`__init__.py:109-170`, the per-test bracket `:363/:386-390`), the inode factory
`cpuslots.go` + its `ensureCPUSlots` call (`server.go:219-236`), and the `AIRA_CPU_SLOTS_DIR` env
(`env.go:83`). Keep `desiredCPUSlots`'s NumCPU−reserve derivation (relocated). Update the Skill/docs
that describe the flock governor. **No flock fallback remains** (owner-decided): daemon-down or
`AIRA_GOVERNOR=off` ⇒ ungoverned, bounded by the slice cap + `oom.group`.

## Resolved decisions (folded above) + remaining for plan-review

**Resolved (folded):** transport = subprocess relay (not Python-direct socket); protocol =
connection-held with a checkpoint read-loop; fairness unit = confine scope; capacity = NumCPU−reserve;
preemption decided only at checkpoint (no push-park; ~10s bounds latency); cpuslots.go deleted, its
capacity helper kept; restart = reconnect-at-next-checkpoint (no scan), fail-open meanwhile; a
`AIRA_GOVERNOR=off` kill-switch (not a flock fallback).

**Still open — the plan-review two-loop decides:**
- **Min-share vs capacity conflict.** If `#jobs > capacity`, "≥1 active per job" exceeds the CPU
  budget — the active set would exceed ncores. Resolve: cap active at `max(capacity, #jobs)` (oversubscribe
  CPU when jobs outnumber cores, accepting nice/cpu.weight to arbitrate) OR round-robin the single
  shares so instantaneous active = capacity but every job gets a turn within a bounded window. Lean:
  the latter (respect capacity; time-slice the min-share). **This is the load-bearing liveness/fairness
  tension — reviewers must nail it.**
- **Worker-start admission granularity.** Does the relay acquire before the worker's FIRST test (so N
  fresh workers don't all run test 1 unbounded), and is the acquire itself parkable? Lean: yes, acquire
  at start, parkable.
- **jobAge source.** Scope creation time (daemon-side, authoritative) vs a client-supplied age. Lean:
  daemon derives it from the scope (client can't be trusted for fairness input).
- **Deploy rollout.** Remove-the-flock means the daemon restart flips every session at once. Is the
  `AIRA_GOVERNOR=off` kill-switch enough, or do we want a brief observe-only daemon mode
  (grant-everything + log) to shake out the scheduler before it actually parks? Lean: ship an
  `observe` mode for the first deploy, flip to `enforce` after a soak — cheap insurance for a
  machine-wide change.

## Test plan (discriminating — the two-loop hardens these)

- **Unit (daemon):** active-set sizing by CPU capacity (N slots → N active, N+1th parks); a
  connection-drop frees a grant and wakes exactly one waiter; park/activate ordering is FIFO-fair
  within an age class; the minimum-share rule keeps ≥1 active per job under a stream of newcomers
  (discriminating: remove the min-share guard → a long job is starved to 0 active in the test).
- **Real / integration:** two sessions' workers cooperatively share the active set (Σ active ≤
  capacity); a **killed** worker's grant is reclaimed and a waiter proceeds (crash-safety); a long
  job keeps ≥1 active worker under a burst of short jobs (anti-starvation); **daemon-down → fail-open**
  ungoverned (plugin proceeds, no hang) — discriminating: point the plugin at a dead socket, assert
  the suite still runs to completion.
- **Hot-path:** steady-state check-in rate ≤1/10s/worker (assert no per-test round-trip); a parked
  worker consumes no CPU while blocked (long-poll, not spin).
- **-race:** the daemon scheduler state under concurrent checkpoints + disconnects is race-clean
  (`make race`).

## Deploy

Daemon-side change → `systemctl --user restart aira-daemon` (preserve `WATCHDOG_MODE=enforce`;
reserves reconstruct per #74). Binary rebuild + swap (both daemon and the client/plugin extract).
Batch the merged `done→in-progress` reopen transition. **Consider an observe-first / opt-in rollout**
(a config or env gate so the scheduler can be enabled without forcing every session onto it the instant
the daemon restarts) — decide in plan-review, because this restart flips EVERY session's confined
suite from flock to daemon-scheduler at once. Notify sessions; the change is machine-wide.

## Seam anchors (code-grounded — build against these)

- **Connection-held pattern to clone:** `internal/daemon/admit.go` — `admitWaiter{grantedCh}` (:49-65),
  `sliceQueue{waiters,kick}` (:67-83), `admitConnection` (:351-489) with the disconnect goroutine
  (:419-423) + deferred `release()` (:425-433) + block `select` (:448-456) + hold `select` (:485-488);
  evaluator `runEvaluator`/`evaluateAdmitQueue` (:544-708), grant = `close(grantedCh)` (:696-706),
  `releaseAdmitWaiter` (:739-761) + `pruneAdmitQueue` (:763-773).
- **Dispatch + long-poll verb:** `internal/daemon/server.go` — switch in `serveConnection` (:487-615),
  add a branch beside `admit` (:545-556); `wrote=true` + `conn.SetReadDeadline(time.Time{})`. Framing:
  `readFrame`/`writeFrame` length-prefixed JSON (`protocol.go:131-178`), envelope `RequestFrame`
  (`protocol.go:63-67`), `ProtocolVersion=5` (`protocol.go:21`). Simpler streaming template: `watch`
  (`server.go:580-591`).
- **Capacity signal to keep:** `desiredCPUSlots` (`cpuslots.go:21-35`); delete the inode factory
  `ensureCPUSlots` (:40-72) + its call (`server.go:219-236`).
- **Plugin seams:** `internal/pylib/aira_xdist_governor/__init__.py` — flock `_acquire_slot`/`_try_slots`
  (:129-170), release (:36-38), per-test bracket (:363/:386-390), the ~10s checkpoint site (:371-380),
  the confine-reserve subprocess idiom to mirror (:312-344), RSS read (:223-235). Go glue:
  `internal/pylib/env.go` `AppendConfineChildEnvironment` (:62-101), `reserveCommand`
  (`confine_linux.go:653-665`).
- **Client relay to mirror:** `cmd/aira/main.go` confine-reserve dispatch (:108-113), parse (:633-658),
  `runConfineReserveCommand` (:781-833, the hold-stdin-until-EOF at :823-832); `runner.ConfineReserve`
  (`confine_reserve.go`, `confine_reserve_linux.go:31-70`); client admit `admitThroughDaemon`
  (`admission_linux.go:240-409`, `release: conn` at :408).
- **Restart:** no active-set scan analog (unlike #74's `evaluateAdmitQueue` adopted-scan
  `admit.go:566-660`) — active set rebuilds as workers reconnect at their next checkpoint (≤10s window,
  fail-open). RAM reserves still reconstruct as today.

## Plan-review round 1 — Sol BLOCK + DeepSeek BLOCK → v2 (folded below)

Both lineages BLOCKED with convergent, real correctness holes. v2 resolutions:

### A. Min-share vs capacity contradiction (both) → per-job floor is HARD, capacity is SOFT

v1's "≥1 active per job" *and* "active ≤ capacity" are incompatible when `#jobs > capacity`. **v2:** the
per-job floor of **1 active worker** is a HARD FLOOR; `capacity = NumCPU−reserve` is a SOFT TARGET.
- `#jobs ≤ capacity` (the real case — `#jobs` = concurrent confine *suites*, ~1–4; capacity ~14):
  active = capacity; the slots ABOVE the 1-per-job floor go youngest-job-first.
- `#jobs > capacity` (pathological): **oversubscribe** to `#jobs` (one active per job). Accepted —
  CPU over-admission is *degraded throughput, not OOM* (unlike RAM), and Slice-1 `cpu.weight` aging
  arbitrates the oversubscription. A job is NEVER fully parked ⇒ **starvation is structurally
  impossible** ⇒ invariant #2 holds trivially, and park/activate only ever shuffles ABOVE-FLOOR slots
  (never a job's last worker). This is *simpler* than a time-slice round-robin (no quantum, no
  park-the-last-worker lost-wakeup edge — architectural-simplicity). **Supersedes** the v1 "still open"
  min-share tension.

### B. Preemption latency is bounded by the longest TEST, not ~10s (both) → state honestly

A worker only checkpoints *between* tests, so a worker inside a long test does not yield until that test
ends (owner non-goal explicitly forbids hard mid-test SIGSTOP preemption). **v2 honest bound:** an
ABOVE-FLOOR slot is reclaimed within `≤ max(current test durations of the active workers)`, NOT ≤10s.
Acceptable because: (i) the hard floor means no job is EVER fully parked regardless of latency (slow to
yield ≠ starved); (ii) Slice-1 cpu.weight aging shifts CPU toward young jobs *continuously*, independent
of the checkpoint cadence; (iii) real pytest tests are short. Delete every false "≤10s"/"bounded passes"
re-activation claim from the v1 body.

### C. Connection read-ownership race (Sol) + lost-wakeup race (DeepSeek) → single-reader state machine

The admit clone is unsound for a checkpoint *loop*: admit has ONE reader until grant then only
EOF-watches; a governor needs continuous checkpoint reads, so a concurrent disconnect `conn.Read`
goroutine would STEAL framed bytes. **v2 handler shape:** ONE reader goroutine owns `conn.Read` in a
loop, decoding frames onto a `frameCh` (a checkpoint frame, or channel-CLOSE = EOF/disconnect). The
handler state machine `select`s over `{frameCh, wakeCh, s.stopping}` — no second concurrent reader.
Park transition (evaluate-then-signal, ONE mutex, no scan-under-lock — the #72/#74 rule): under the lock,
**register `wakeCh` into the parked set AND free the slot AND set state=parked, THEN unlock and signal**
the evaluator. Register-before-free-before-signal ⇒ a reactivation can never be lost. The evaluator
reactivates only by sending on a registered `wakeCh`.

### D. Crash-safety is asymmetric & incomplete (Sol: parked leak; DeepSeek: over-admit) → both directions

- **Parked worker killed → relay blocked on the daemon read never sees stdin EOF → grant leaks
  forever (Sol).** v2: the relay runs a CONCURRENT stdin-EOF watcher goroutine (always reading its
  stdin from the plugin); on EOF (worker death) it closes the daemon connection and exits — so even
  while parked (state machine on `wakeCh`), the daemon sees `frameCh` close and releases. Belt:
  `PR_SET_PDEATHSIG(SIGKILL)` on the relay so worker death kernel-kills it.
- **Governor channel dies while the worker LIVES → daemon frees the slot, worker keeps running →
  over-admit (DeepSeek).** v2 DECISION: for CPU this is **fail-open-CONTINUE + RECONNECT**, NOT
  DeepSeek's fail-stop. Rationale (defended): the daemon must NEVER be a liveness dependency (owner
  rule — fail-stopping a live merge-gate on a daemon blip is unacceptable); CPU over-admit is
  degraded-perf not OOM; the overshoot is BOUNDED by (workers active at loss) and TRANSIENT. The relay
  actively **reconnects** (see E) to re-sync and minimise the overshoot window, rather than going
  ungoverned-forever. This explicitly rejects fail-stop for CPU.

### E. No stable per-worker session ID (both) → client-generated worker UUID + idempotent re-acquire

Scope = job ≠ worker; daemon-assigned `workerSeq` can't dedupe reconnects. **v2:** the relay generates
a stable **worker session UUID** at start (held for its life). Acquire/re-acquire carry `{workerUUID,
jobID}`; the daemon keys the active set by `workerUUID` and REPLACES any stale record on re-acquire
(idempotent) — no double-count. On daemon-channel loss while alive, an ACTIVE relay reconnects and
re-acquires (D); a PARKED relay treats daemon loss as "continue" (releases the worker, re-acquires
fresh) rather than staying blocked (DeepSeek). Post-daemon-restart the active set rebuilds keyed by
reconnecting `workerUUID`s — safe, no scan needed, no double-count.

### F. Seam corrections (Sol) → two plugin call sites + inject the scope ID

- The `__init__.py:371-380` site is an after-test **GC condition**, not a loop or worker-start hook.
  v2: the plugin ADDS (1) a **pre-first-test worker-start acquire** (in `pytest_runtest_protocol`
  before the first `yield`, parkable) and (2) a **periodic post-test checkpoint** co-located with the
  ~10s GC condition. Two sites, not one.
- The child env does NOT export a scope ID ("as confine already does" was FALSE). v2: the confine
  supervisor **injects `AIRA_CONFINE_SCOPE_ID`** into the child env (`AppendConfineChildEnvironment`
  `env.go:62-101` / `confine_linux.go`), authoritative (the supervisor owns the scope ID). The relay
  reads it as `jobID`. (Rejected: derive-from-cgroup, more fragile.)

### G. Fail-open honesty (Sol) + nextTestEst guard (both)

Fail-open loses CPU GOVERNANCE — `memory.max` + `oom.group` are memory-only backstops; they do NOT
bound CPU fairness or oversubscription. State it as honest *degraded CPU scheduling*, transient,
accepted; it applies on pre-grant daemon-unreachable, malformed/timeout, and `AIRA_GOVERNOR=off`.
`nextTestEst` is validated+bounded on receipt, STORED but never ranked/gated on in Slice 2; a
discriminating test asserts CPU selection is invariant to `nextTestEst` (Slice-3 seam, no Slice-2 trap).

### Deploy (v2) — observe-then-enforce

Add a daemon `AIRA_SCHED_MODE=observe|enforce` (default `observe` on first deploy): `observe` grants
everything + logs the park/activate decisions it WOULD make (so the scheduler is exercised live without
parking anyone); flip to `enforce` after a soak. Cheap insurance for a machine-wide flip with no flock
fallback. `AIRA_GOVERNOR=off` (client) remains the per-session escape hatch.

### v2 status

Fable code-gate pending (adjudicate A–G against the real admit.go/server.go/plugin; confirm the
single-reader state machine + register-before-signal + the PDEATHSIG/stdin-watch crash-safety are
buildable on the cited seams). On GATE-PASS → Terra build.
