---
{"schema":1,"id":"AIRA-261","project":"aira","title":"aitest @aira_cpu admission accounting + @aira_time LPT scheduling consumers","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","telemetry"],"hold":false,"relations":[]}
---

Implements the owner-greenlit slice of [[AIRA-260]] (see AIRA-260 Input 6 for the settled
sequence + the design provenance). Two consumers, shipped as SEPARATE PRs + two-loops:

- **Phase A — `@aira_cpu` admission accounting** (Go wire + Python supervisor). The
  load-bearing annotation; deploy's CI-image re-pin + lite=16 flip are gated ONLY on this.
  **Ships as its own release (v0.16, proto 12→13); tag it before Phase B if B drags.**
- **Phase B — `@aira_time` LPT scheduling** (Python supervisor only, no Go change). Lower
  priority; auto-learned from a per-test duration profile FILE deploy's gate materialises.

Two-loop mandatory (Opus builds, Fable reviews in a detached worktree — dispatch/wire/ledger).

## Phase A — the 2-D reservation (NOT "promote one constant"; thread {bytes,cpu} everywhere a worker is sized/dispatched)

Grounded map (advisor pass + source reads):

**Go wire (proto 12→13):**
- `protocol.go:144` ProtocolVersion 12→13; `admission_linux.go:68` DaemonProtocolVersion
  12→13 (a test enforces equality). **NOT drop-in — first proto bump since v0.7.**
- CLI `parseWorkerAdmitArgs` (`main.go:1166`): add `estimated-cpu` to the valid map
  (OPTIONAL — absent ⇒ daemon default). Its caller maps `options["estimated-cpu"]` →
  `req.EstimatedCPU`.
- Client `WorkerAdmitClientRequest` (`worker_admit_client_linux.go:60`): add
  `EstimatedCPU int64`; send `"estimated_cpu"` in the args map (`:155`). ALWAYS send it
  (default 1) for a uniform, testable frame.
- Re-declare honesty (`worker_admit_client_linux.go:251`): `CPUCores:
  uint32(req.EstimatedCPU)` NOT the hardcoded `DefaultConfineCPUCores` — else a cpu=4
  worker re-declares cpu=1 after a daemon restart → rebuilt ledger under-charges → storm.
  (charged == requested since we refuse-not-clamp, so req is correct.)
- Daemon parse (`worker_admit.go` ~:171, beside estimated_bytes): parse `estimated_cpu` via
  `exactAdmitInt64`, default `DefaultConfineCPUCores` when absent, reject `<1`. Store
  `req.estimatedCPU`.
- Daemon charge (`worker_admit.go:424`): `cpu: req.estimatedCPU` instead of the hardcoded
  `DefaultConfineCPUCores`. (The admit path already fits cpu at `admit.go:2251` and the
  grant already echoes AvailableCPU — the ledger dimension exists.)
- **Ceiling wedge (item 1):** mirror the bytes `EXCEEDS_CEILING` pre-check
  (`worker_admit.go:256,372`) for cpu — refuse `estimated_cpu > cpuCeiling()` (=2×NumCPU,
  `admit.go:2435`) with `WorkerAdmitReasonExceedsCeiling` + a cpu-specific detail. The
  supervisor's `_bootstrap_from_empty_pool` already special-cases that reason (mark
  unevaluated + pop + continue) so a blocking bootstrap of an over-ceiling cpu test can't
  wedge the run; extend its knob-hint to mention `@aira_cpu`. Test: cpu>ceiling bootstrap
  terminates.

**Python supervisor:**
- `__init__.py pytest_configure`: register `aira_cpu(cores)` beside `aira_mem`, BEFORE the
  `--aitest-workers` early-return. `_aira_cpu_cores_for_item(item, default=1)`: read one
  positive int, malformed → default + stderr warning (AIRA-223 rule), never silent.
- `collect()`: build `cpu_need[nodeid]` = declared `aira_cpu` if annotated else 1 (ABSOLUTE,
  not floor+increment). `_cpu_need_for` mirrors `_need_for`.
- **The reservation is now a (bytes, cpu) PAIR everywhere:** thread `cpu_need` through
  `spawn_worker`/`acquire_worker`/`_spawn_admit_relay` (append `--estimated-cpu`); store
  `state["cpu"]` beside `state["reservation"]`; apply the cpu fit at BOTH `_largest_fitting`
  call sites (`:1994` dispatch-to-existing — a cpu=1 worker must NOT be handed a cpu=4 test
  or the ledger under-charges; `:2536` size-new); the growth gate (`:2526`,
  `available_cpu<1`) becomes `available_cpu >= cpu_need`; check `_pool_covers_the_queue()`
  (if bytes-only, a queue of cpu=4 tests looks covered by cpu=1 workers → growth never
  fires); retire-on-no-fit = nothing fits EITHER dimension. Probe sends default cpu.
- Trace (AIRA-259): record the real charged cpu (`cpu_need`), never the hardcoded 1.

**Tests that must fail against the wrong impl (mutation-verify):** daemon charges
`estimated_cpu=N` decrements cpuOutstanding by N not 1; drop the 2-D cpu filter clause →
red; cpu>ceiling bootstrap terminates; malformed mark → default+warning. `git add` every
new Python test file (TestEmbeddedTreesMatchTrackedSources).

**Release/install care:** proto 12→13 is NOT drop-in. Swap `~/.local/bin/aira` + restart
`aira-daemon.service` back-to-back in a quiet window, after `aira confine --list` + a
heads-up (a relay speaking 13 to a daemon on 12 mismatches — check terminality). Peer
notify must LEAD with "proto 12→13, NOT drop-in", not "drop-in".

## Phase B — `@aira_time` LPT (supervisor-only)

Read `timeDesign` in full first (`~/tmp/ci-cp/resource-annotation-workflow-output.json`) —
it names both re-key sites + the two invariants.
- Re-key `_largest_fitting`'s ORDER to `(time, need, FIFO)` — LPT primary, RAM bin-pack
  secondary, FIFO tertiary. Keep the fit-FILTER. Preserve the `attempts[best]+=1` increment
  (crash-retry cap). Fit-filter BEFORE time-order (a heavy-time RAM-oversized test is
  excluded, never wedges a fitting light test). Byte-identical with zero marks
  (tie-collapse when all time_cost=1). Ordering-ONLY, never a deadline.
- Auto-learn: `time_cost` reads a per-nodeid duration PROFILE FILE supplied via an
  env/config coordinate that deploy's gate materialises from its GCS capture. **AIRA NEVER
  reads GCS** — keep it a primitive: mark = bootstrap/override, file = steady-state,
  neutral (1) otherwise.
  **DEFERRED to v0.17+ (NOT in the Phase B build).** The v0.17 Phase B ships the MARK +
  LPT re-key ONLY (mark-or-neutral); the profile-file auto-learn is a separate follow-on
  because per-test duration measurement is not yet in aira (the AIRA-259 trace is
  per-WORKER; per-test spans are themselves a deferred tier). The `@aira_time` mark is the
  bootstrap/override input that works standalone.

## Phase B DONE (2026-09-18, `f4e2231`)

Register `@aira_time` + `_aira_time_for_item` reader (relative unitless int rank, default 1,
malformed warns incl OverflowError); `time_cost` map in collect(); `_time_for` accessor;
re-key `_largest_fitting`'s ORDER to `(aira_time, need, -queue_index)` at its single shared
definition (both call sites get LPT), keeping the 2-D fit FILTER before the order and the
`attempts` increment. Byte-identical with zero marks (Fable measured 0 mismatches over 50k
random queues). Two-loop Fable = **APPROVE-WITH-NITS** (nits: 3 P3 test-gaps ported —
collect() warning-emission, bool/multi-arg, named FIFO-tie regression — for BOTH time and
the shared cpu emission gap; docstring de-staled; this deferral note). Accepted design
trades (documented, not fixed — bounded, slowdown-only, never deadlock): (a) a worker sized
to a small-RAM heavy-time test is less versatile → at most one extra retire+replace per such
test (sparse); (b) demoting RAM to the secondary key can fragment a big-RAM/short-time test
out of a concurrent fit on a saturated box (mitigation: also mark the big-RAM test). Ships as
v0.17 (supervisor-only, DROP-IN — no proto/wire change).

## Status
Phase A in progress (TDD), on `aira-261-cpu-time-consumers`. Increments:
- **A.1 DONE** (`2e27482`): daemon parses + charges worker-admit `estimated_cpu` (optional,
  absent ⇒ DefaultConfineCPUCores floor) + the cpu-ceiling pre-check. 3 tests, both
  behavioural ones mutation-verified. **Finding worth keeping (CORRECTED by the Fable review
  — my first measurement was wrong):** the cpu pre-check is LOAD-BEARING because the
  worker-admit path's ONLY cpu ceiling check is this pre-check (`admit.go:1569`'s
  `request.cpu > cpuCeiling` sits in the *confine* admit verb path; worker-admit calls
  `enqueueResolvedConfineAdmit` directly, whose only ceiling check is bytes). WITHOUT the
  pre-check an over-ceiling cpu claim ENQUEUES AND BLOCKS FOREVER (a claim has no max-wait;
  `admit.go:2251` is a plain `waiter.cpu <= ceiling−outstanding` wait) — it is NOT granted,
  the ledger stays 0, and other/later fitting waiters on the slice are UNAFFECTED (the wedge
  is local to the one supervisor's blocking bootstrap). The pre-check refuses it up front
  with ExceedsCeiling so the supervisor marks that one test unevaluated + continues.
  (My earlier "GRANTED → ledger goes negative → stalls everything" was a mutation-measurement
  error: I mutated the charge and the pre-check together, so the charge-to-1 mutation made it
  grant cpu=1; with only the pre-check disabled it blocks.)
- **A.2–A.5 DONE** (`223a3db`, `0d8419c`): CLI `--estimated-cpu` + `WorkerAdmitClientRequest.EstimatedCPU` + the frame's `estimated_cpu` + re-declare honesty; proto 12→13; the supervisor `@aira_cpu` consumer (register + `cpu_need` map + `--estimated-cpu` relay + the 2-D fit at both `_largest_fitting` sites + `_smallest_ready` + `_pool_covers_the_queue` greedy 2-D matching + the cpu knob-hint).
- **Review (`1728fb1`):** two-loop Fable = BLOCK (porous wire chain) → fixed (4 mutation-killing wire-path tests + P2 wedge-text correction + P3 OverflowError + porous-pool_cover fix + gate simplify) → **APPROVE-WITH-NITS, BLOCK cleared**. `aira confine -- make ci` GREEN (all packages).

**Accepted coverage gaps (v0.16; close in v0.17):**
- The `workerReDeclareRecord(grant, estimatedCPU)` CALL SITE is unpinned — the builder is unit-tested, but a mutant passing `0` there (→ floors to 1) survives; a keeper-reconnect test decoding the worker re-declare frame end-to-end closes it.
- `declared_cpu_cores` not yet in the AIRA-259 trace (needs the trace-builder + its test).
- LATENCY (not under-charge): a small-bytes/high-cpu test tends to run LAST (cpu is a FILTER, bytes the RANK) — the natural fix is Phase B LPT.
- The widest `@aira_cpu(N)` must fit deploy's CI shape's 2×NumCPU or it is refused/unevaluated there (deploy's lane check).

**Release:** Phase A ships as **v0.16** (proto 12→13, NOT drop-in — atomic reinstall + daemon restart, `aira confine --list` + heads-up first). Then Phase B (`@aira_time` LPT, supervisor-only).
