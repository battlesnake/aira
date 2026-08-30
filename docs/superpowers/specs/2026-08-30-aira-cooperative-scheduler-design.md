# AIRA cooperative cross-session scheduler — design

- **Status:** designed (this document); not yet built. Owner-directed 2026-08-30.
- **Supersedes the CPU governor mechanism:** the machine-wide flock slot dir (#49) —
  a flat concurrency cap — is replaced by a daemon-managed cooperative scheduler.
- **Folds in:** AIRA-14 (the AIRA-11 CPU-side bootstrap residual) as its first slice.

## 1. Problem

Two mechanisms govern confined pytest work today, and both have hit real limits:

1. **CPU: the flock slot dir** (`aira_xdist_governor`, #49). N slot files = N
   concurrent active workers; a worker `flock`s a slot before each test, releases on
   finish. Virtues: crash-safe for free (kernel releases the lock on process death),
   zero-coordinator, hot-path-cheap (a syscall, no IPC). Limits: (a) it is a **flat
   cap** — it gives *proportional-by-contention* sharing, not fairness or young-first
   latency; a big multi-worker job re-acquires slots continuously and a small newcomer
   only gets its ~1/N share. (b) It governs **execution only** — worker *bootstrap*
   (execnet spinning up N interpreters, each importing the suite) runs before the
   plugin's per-test hooks and is ungoverned. (c) It is **CPU-count-shaped**; it cannot
   express "admit by RAM headroom."

2. **RAM: per-test `confine-reserve`** (#67/#69, AIRA-12). Each test reserves
   `max(aira_mem, measured_RSS + headroom)` against the slice ledger. Works, but is a
   separate mechanism from the CPU flock, with no shared notion of an "active worker."

**Why a cooperative scheduler, not more knobs:**

- **AIRA-14 / the RAM-bound regime.** Letting the *kernel* prioritise (via `nice`)
  only works when we oversubscribe — more runnable workers than cores, so CFS has
  something to weigh. But when **RAM** is the binding constraint, workers are
  RAM-gated: we cannot oversubscribe, so there is nothing for CFS to prioritise. In
  that regime only an *active* scheduler that admits by RAM headroom (and can prefer a
  small-next-test worker over a big one that does not fit yet) keeps cores busy. The
  flat flock cannot do this.
- **AIRA-14 residual** (the AIRA-11 CPU-side bootstrap blank): `ionice` idle→best-effort-7
  (done) un-starved the bootstrap *I/O*, but the interpreter spin-up + import
  *execution* still starves at `nice 19` under peak load, so `-n auto` still blanks
  intermittently. The fix is `nice 19→5` for the young/bootstrap window with age-decay
  back to 19 — a priority-aging policy the flock has no place to express.
- **Fairness/latency.** Long jobs should yield to newer/shorter jobs; the flock can't.

## 2. Goals / non-goals

**Goals.** One daemon-owned scheduler that decides the *active worker set* across all
sessions by **CPU** (≈ncores active), **RAM** (Σ active ≤ slice budget), and
**fairness** (young-first, aging). `-n auto` becomes a genuinely safe default on a busy
box: it never blanks at bootstrap and never over-admits RAM. Cooperative preemption is
cheap because a worker parks *between tests* (alive, no re-bootstrap).

**Non-goals.** Not a general OS scheduler; it schedules AIRA-confined test workers (and,
later, other governed jobs) only. Not hard preemption (no SIGSTOP mid-test) — parking
happens at the worker's cooperative checkpoint. Not a replacement for the slice
`memory.max` + `oom.group` backstop, which remain the final containment.

## 3. Design

### 3.1 Priority baseline + aging (Slice 1 — closes AIRA-14, ships first)

Mechanism (owner-decided 2026-08-30): **cgroup `cpu.weight` decay**, not re-nicing.

- The confine supervisor sets the scope's `cpu.weight` **HIGH at launch** so a young /
  bootstrapping scope competes well for CPU — the interpreter spin-up + imports get
  scheduled, closing the AIRA-14 CPU-side residual (complementing the already-shipped
  `ionice` best-effort-7 that fixed the bootstrap *I/O*).
- It **decays** the scope's `cpu.weight` toward a low value over the job's lifetime
  (e.g. steps at 10s, 30s, 1m, 5m, 10m, 30m), so a long-running job yields CPU to the
  desktop and to newer jobs — the desktop protection today's flat nice-19 gives, but
  age-graded.
- `cpu.weight` is one cgroup knob and covers **all** workers, including those execnet
  forks after launch — cleaner than re-nicing descendants. `nice` stays 19 (threads
  share the scope's `cpu.weight` allocation; no per-descendant re-nice needed).
- Validate in the build that a *young* scope's high weight doesn't disturb the desktop.
- Slice 1 keeps the flock unchanged — a pure priority change, independently shippable,
  and exactly what AIRA-14 needs. Its own two-loop; ships first.

### 3.2 The daemon cooperative scheduler (Slice 2 — replaces the flock)

- **Active-set authority.** The daemon owns the set of *active* workers machine-wide.
  A worker is either **active** (may run tests) or **parked** (blocked between tests).
  The daemon sizes the active set by: active-count ≤ CPU capacity **and** Σ(active
  workers' reserved RAM) ≤ slice budget **and** fairness policy.
- **The checkpoint protocol.** The `aira_xdist_governor` plugin, at its existing ~10s
  gc/compaction checkpoint (and at worker start), sends the daemon a small message over
  a Unix socket: `{worker_id, job_id, session, age, held_rss, next_test_rss_estimate}`.
  The daemon replies **continue** or **park**. On **park**, the worker blocks on the
  socket (long-poll) until the daemon wakes it — reusing the D4 admission-queue pattern
  exactly. Steady state: a worker checks in ≤1/10s, so ~2–3 req/s total — the hot-path
  cost the flock avoided is bounded and small (NOT per-test).
- **Cooperative preemption.** When the daemon wants to admit a younger/smaller waiter
  and the active set is full, it parks an *older* active worker at its next checkpoint
  and wakes the waiter. Parking is between tests → alive, no re-bootstrap → nearly free.
- **RAM-aware admission (the AIRA-4 backfill, at test granularity).** When RAM is near
  the slice limit, the daemon activates parked workers whose `next_test_rss_estimate`
  *fits* the remaining headroom, rather than head-of-line-blocking small tests behind a
  big one that does not fit yet. Uses the same `confine-reserve`/#67 ledger as the RAM
  authority — the CPU governor and RAM reserve unify into one scheduler.
- **Crash-safety.** A worker's *active* grant is **connection-held**: the worker holds
  the socket; if it dies, the connection drops and the daemon frees the grant — exactly
  confine-reserve's model. This restores the crash-safety the flock gave for free,
  daemon-side.
- **Bootstrap.** Bootstrap still precedes the plugin (execnet-internal), so the daemon
  cannot gate it — bootstrap safety remains Slice 1's `nice 5` + `ionice` best-effort.
  The scheduler governs *execution*; the priority baseline governs *bootstrap*.

### 3.3 Fail-open

If the daemon is unreachable, the plugin runs **ungoverned** (as today on any governor
failure): workers proceed, bounded only by the slice `memory.max` + `oom.group`. The
scheduler is an optimisation over the hard backstop, never a correctness dependency.

## 4. Hard invariants (the plan-review two-loop must nail these)

1. **Crash-safety.** A dead/killed/OOM'd worker's active grant is always reclaimed
   (connection-drop), and a leaked grant never permanently shrinks the active set.
2. **Anti-starvation of long jobs.** Young-first + aging must never park a long job
   (e.g. a merge-gate) *forever*. Bounded aging + a guaranteed **minimum share** so
   every job keeps ≥1 worker progressing; a long job finishes, just slower.
3. **Liveness / no deadlock.** The active set is never empty while any worker wants to
   run; the park/activate protocol always makes progress; fail-open on daemon
   uncertainty rather than parking everyone.
4. **RAM-aware honesty.** Best-fit packing fails open (never fabricates a fit) and must
   not itself reintroduce head-of-line blocking; an unknown next-test estimate is
   treated conservatively, never as zero.
5. **Hot-path bound.** Steady-state check-in is ≤1/10s/worker; a parked worker
   long-polls (no busy-wait); a freed slot wakes a waiter without a full poll cycle.
6. **Fail-open daemon-down.** No test-execution correctness depends on the daemon.

## 5. Staging

1. **Slice 1 — priority-aging (closes AIRA-14). DONE + DEPLOYED 2026-08-30 (`3708af5`).**
   Implemented as cgroup `cpu.weight` decay (owner-decided, not re-nicing): fresh scope at
   weight 100 decaying `100→70→50→30→20→10` over `10s,30s,1m,5m,10m,30m`; best-effort `+cpu`
   delegation repair (fail-open, memory stays fail-closed); honest `cpu-weight=aging|unavailable`
   facet; FD-anchored fail-open decay writer stopped-and-joined before teardown. Flock unchanged.
   Live-verified (scope `cpu.weight=100`, parent propagates `+cpu`). See the Slice 1 plan spec.
2. **Slice 2 — the daemon scheduler. DONE + DEPLOYED (observe mode) 2026-08-30**
   (Pass 1 `ea16f09` + Pass 2 `4790bce`). Checkpoint protocol + active-set management +
   connection-held grants + cooperative park/activate; the flock is REMOVED. Job identity =
   the confine scope; a HARD per-job floor of 1 + soft capacity (NumCPU−reserve),
   youngest-first above the floor; `parkRequested` so preemption never over-admits (capacity
   holds until a worker confirms park at its checkpoint); single-reader state machine +
   close-to-reactivate (no #72 deadlock); worker-UUID idempotent re-acquire; bounded fail-open
   (`AIRA_GOVERNOR_MAX_WAIT`, generous 300s default → a wedged daemon degrades to ungoverned,
   never a hang). Plugin: the governor relay runs at worker-start + at a checkpoint placed
   AFTER the per-test RAM reservation is released (so a park holds no RAM). Deployed default
   `AIRA_SCHED_MODE=observe` (capacity-enforce, fairness-observe); **the enforce flip (full
   young-first preemption) follows a soak.** Daemon-down / `AIRA_GOVERNOR=off` → the plugin
   fails open (ungoverned, bounded by the slice cap + `oom.group`) — the only fallback (no flock).
   **Honest scope (field-confirmed): this governs test EXECUTION, not pre-plugin execnet
   BOOTSTRAP — so it does not directly fix the simultaneous-cold-start bootstrap blank; that is
   AIRA-17 (whole-suite bootstrap-admission).**
3. **Slice 3 — RAM-aware admission (unify, owner-decided).** Fold the per-test
   `confine-reserve` RAM ledger into the scheduler's active-set sizing
   (small-next-test-first). One scheduler owns CPU + RAM, not two mechanisms.

Each slice is a full plan→plan-review→build→build-review→verify→deploy cycle. Slices 2
and 3 are daemon changes → a daemon restart deploys them (batch the `done→in-progress`
reopen transition, already merged, with the first such restart).

## 6. Open design questions (resolve in the per-slice specs)

- **Resolved (owner 2026-08-30):** aging mechanism = cgroup `cpu.weight` decay; the
  flock is REMOVED (daemon is the sole scheduler, fail-open ungoverned when down);
  CPU + RAM are unified into one scheduler (Slice 3).
- **Aging schedule + young-weight desktop impact:** validate the desktop stays smooth
  with a fresh scope at high `cpu.weight` + `ionice` best-effort-7; tune the decay curve.
- **CPU capacity signal:** how the daemon derives "≈ncores active" (NumCPU − reserve,
  as the flock did) and whether it accounts for non-AIRA load.
- **Fairness policy:** exact young-first weighting + the minimum-share floor for long
  jobs; how "age" is measured (job age vs worker age vs held-RSS).
- **Non-pytest governed jobs:** whether/how `aira confine -- <non-pytest>` participates
  (today only the pytest plugin checkpoints; a whole-job confine has no per-test seam).

## 7. Test plan

- **Slice 1:** unit — the aging schedule maps age→nice/weight correctly; discriminating —
  a young job gets `nice 5`, an aged job `nice 19`. Real-cgroup — a bootstrapping worker
  under induced contention completes (the AIRA-14 regression, from various' repro).
- **Slice 2:** unit — active-set sizing by CPU; connection-drop frees a grant; park/wake
  ordering. Real — two sessions' workers cooperatively share the active set; a killed
  worker's grant is reclaimed; a long job keeps ≥1 active worker under a stream of short
  jobs (anti-starvation); daemon-down → fail-open ungoverned.
- **Slice 3:** RAM-bound — near the slice limit, a small-next-test worker is admitted
  ahead of a big one that does not fit; Σ(active RAM) never exceeds the budget; unknown
  estimate handled conservatively. Discriminating — a workload that head-of-line-blocks
  under flat admission but packs under RAM-aware.

## 8. Deploy

Slice 1: binary rebuild + swap (confine supervisor + embedded governor), no daemon
restart. Slices 2–3: daemon changes → `systemctl --user restart aira-daemon` (preserve
`WATCHDOG_MODE=enforce`; reserves reconstruct per #74, so the restart is safe), batching
the merged `done→in-progress` reopen transition. Notify sessions; the change is
machine-wide (every confined pytest suite).
