---
{"schema":1,"id":"AIRA-261","project":"aira","title":"aitest @aira_cpu admission accounting + @aira_time LPT scheduling consumers","status":"in-progress","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","telemetry"],"hold":false,"relations":[]}
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

## Status
Phase A in progress (TDD), on `aira-261-cpu-time-consumers`. Increments:
- **A.1 DONE** (`2e27482`): daemon parses + charges worker-admit `estimated_cpu` (optional,
  absent ⇒ DefaultConfineCPUCores floor) + the cpu-ceiling pre-check. 3 tests, both
  behavioural ones mutation-verified. **Finding worth keeping:** the cpu pre-check is
  LOAD-BEARING, not redundant with the admit path's own `request.cpu > cpuCeiling` check —
  the cpu ledger is SIGNED, so WITHOUT the pre-check an over-ceiling worker-admit is GRANTED
  (charges the ledger negative → every later admission on the slice then stalls). The
  pre-check refuses it up front with ExceedsCeiling so the supervisor marks it unevaluated.
- **A.2 TODO:** CLI `--estimated-cpu` (`main.go` parseWorkerAdmitArgs) + `WorkerAdmitClientRequest.EstimatedCPU` + send `"estimated_cpu"` in the frame + re-declare uses `req.EstimatedCPU` (honesty).
- **A.3 TODO:** proto 12→13 (`protocol.go` + `admission_linux.go`, enforced-equal test).
- **A.4 TODO:** supervisor — register `aira_cpu`, `cpu_need` map, `--estimated-cpu` on the relay, extend the bootstrap knob-hint.
- **A.5 TODO:** supervisor 2-D fit — `state["cpu"]`, both `_largest_fitting` sites, growth gate `available_cpu >= cpu_need`, `_pool_covers_the_queue`, probe default cpu.
- Then `make ci` green → Phase A PR → two-loop → tag v0.16. Phase B (LPT) after.
