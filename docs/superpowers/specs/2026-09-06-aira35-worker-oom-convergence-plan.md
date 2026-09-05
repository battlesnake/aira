# AIRA-35 — aitest worker scopes: make `memory.max` actually contain, and converge in milliseconds

Status: plan v3 (post Fable plan-gate rounds 1 and 2)
Ticket: AIRA-35 (P1, bug). Relates: AIRA-30 (aitest Slice 1), AIRA-32 (closed; watermark tuning).
Amends: `docs/superpowers/specs/2026-09-01-aitest-design.md` §3.3, §3.4, §3.7, §5, §6, §7.

## 1. What was measured, before deciding anything

The ticket offered three candidate fixes — narrow the `memory.high`/`memory.max` gap, add a
daemon-side escalation/kill path, or establish that this is WSL2-specific. The brief mandated
measuring first. A probe was built and run on this host before any code was touched.

**Probe** (`~/tmp/aira35-probe/main.go`; committed as tests — §6): creates a real cgroup-v2
parent as a sibling of the ambient `aira confine` scope, enables `+memory`, then per variant
creates a child scope with `memory.oom.group=1`, `memory.max`, an optional `memory.high`, and
an optional `memory.swap.max=0`; launches a Python allocator into it at
`clone3(CLONE_INTO_CGROUP)` time; and times launch → the scope's **own** `memory.events`
`oom_group_kill` increment. Host: WSL2, kernel 6.18.33.2, 80 GiB RAM, 20 GiB swap partition
active (9.3 GiB in use), inside `aira.slice` (`MemoryMax=64G`, `MemorySwapMax=8G`).

### 1.1 Does the scope contain at all? — 32 MiB cap, 512 MiB allocation, 3 reps

| `memory.high` | `memory.swap.max` | outcome | swap peak |
|---|---|---|---|
| 80% of max (**production today**) | unset (**production today**) | **never OOM-killed; child exits 0** | 524–538 MiB |
| 95% of max | unset | **never OOM-killed; child exits 0** | 519–534 MiB |
| unset | unset | **never OOM-killed; child exits 0** | 516–523 MiB |

A slow leaker (8 MiB/100 ms) and a 256 MiB-cap/1 GiB-allocation run reproduce this exactly
(exit 0, 512 MiB and 820 MiB swapped respectively).

### 1.2 With swap capped, how fast does it converge? — time to `oom_group_kill`

| `memory.max` | `memory.high` = 80% | `memory.high` = 95% | **no `memory.high`** |
|---|---|---|---|
| 32 MiB | **not converged in 420 s** (1 rep, 5475 `high` events, **0** `max` events) | 0.73 / 1.37 / 1.71 s | **0.029 / 0.031 / 0.033 s** |
| 64 MiB | — | 0.82 / 2.86 s | **0.061 / 0.061 s** |
| 128 MiB | — | 4.52 / 5.18 s | **0.080 / 0.101 s** |
| 256 MiB | not converged in 45 s (2 reps) | 11.50 / 13.28 s | **0.141 / 0.182 s** |
| **512 MiB — the production cap** | — | **16.42 / 18.42 s** | **0.361 / 0.481 s** |

Slow-leaker (8 MiB/100 ms, 32 MiB cap): 80% not converged in 45 s; 95% 1.31/1.99 s;
no-`memory.high` **0.34/0.34 s**.

512 MiB is not a hypothetical: `internal/pylib/aitest/__init__.py:137-148`
(`_resolve_estimated_bytes`) returns a flat `512 << 20` when `AIRA_AITEST_ESTIMATED_BYTES` is
unset, and per-suite peak-history sizing is explicitly deferred there. So the bottom row is
the default production configuration.

### 1.3 What the numbers establish

**(A) The ticket understates the defect: production worker scopes do not contain a runaway at
all.** No production path writes `memory.swap.max` — a repo-wide grep returns nine hits, every
one in a `_test.go` file. cgroup-v2's `memory.max` bounds *memory*, not memory + swap; swap is
bounded separately by `memory.swap.max`, defaulting to `max`. On any host with swap a worker
that exceeds its `memory.max` is therefore **not** killed: it is reclaimed into swap and runs
to completion (§1.1, every row, at every cap size and both allocation shapes). The aitest
design spec's own §7 invariant — "a worker that genuinely exceeds its `memory.max` is actually
OOM-killed" — is false in production today. It holds in the existing e2e only because the
*test harness* writes `memory.swap.max=0` on the ancestor (`pytest_aitest_e2e_test.go:285`):
the test proves the harness, not the product. This is also, directly, the "dead memory held
for long durations" the aitest design exists to prevent — a leaker converts its overage into
hundreds of megabytes of swap I/O against `aira.slice`'s shared 8 GiB swap budget.

**(B) `memory.high` is the convergence stall, and narrowing it moves the stall rather than
removing it.** With swap capped, 80% did not converge in **420 seconds** — 5475 `high` events
and **zero** `max` events, i.e. the kernel held the cgroup pinned below its hard cap
indefinitely, which is exactly what `memory.high` promises to do. At a fixed 95% the delay
tracks the *absolute* width of the throttle window, which grows with the cap: 0.7–2.9 s at
32–64 MiB, 4.5–5.2 s at 128 MiB, 11.5–13.3 s at 256 MiB, **16.4–18.4 s at the production
512 MiB cap** — a ~20× spread across a 16× cap range, at one unchanged fraction. Removing
`memory.high` converges in 0.03–0.48 s across the same range, and what growth remains there is
just the time to allocate more memory. (Five cap sizes, 2–3 reps each; the scaling is a
well-supported inference, not a measured law, but the decision does not rest on it — the
no-`memory.high` column is 34–45× faster than 95% at *every* size, including the production one.)

Mechanism, documented mainline behaviour: above `memory.high` the kernel enters
`mem_cgroup_handle_over_high` on each return to userspace, runs direct reclaim, and sleeps for
a penalty that grows with the relative overage. `memory.high` is built for a cgroup whose
*userspace* supervisor acts on the resulting pressure signal. Nothing above an aitest worker
scope acts on it, so it is a livelock by construction. The unkillable D-state the ticket
reports is a hazard of that same throttle path; removing the path removes the hazard.

**(C) Not WSL2-specific.** Both mechanisms are generic cgroup-v2/mm behaviour. WSL2 only
contributes swap-on-by-default, which is why (A) surfaced here first.

## 2. Decision

Adopt the simplest variant the measurement supports. It is also the one that *removes*
machinery rather than adding it — the project's architectural-simplicity rule and the brief's
stated preference for shrinking the throttle window over new kill machinery.

1. **Stop writing `memory.high` on aitest worker scopes**, and remove `memory_high` from the
   worker-admit grant contract end to end.
2. **Write `memory.swap.max=0` on every aitest worker scope**, so `memory.max` is a real
   containment boundary — with an honest, machine-readable disposition when it cannot be set.
3. **Move the proactive-recycle watermark onto `memory.max`**, preserving today's *effective*
   trigger point exactly.

Rejected, with reasons:

- *Narrow to 95%* — measurably not a fix at the production cap (16–18 s, §1.2), keeps the
  D-state hazard, and keeps a kernel throttle nothing acts on.
- *Daemon-side over-high escalation/kill* — none exists, the architectural-simplicity rule
  disfavours adding one, and it would be strictly slower and strictly more machinery than the
  ~30 ms the kernel already achieves once the throttle is gone.
- *Keep `memory_high` on the wire while not writing `memory.high`* — a field named after a
  cgroup control that is deliberately not written is a lie in the protocol. AIRA has no
  backwards-compatibility obligation, so the field goes.

## 3. What removing `memory.high` costs, honestly

Its two stated jobs in the aitest design are covered elsewhere — but not for free, and the
"free" claim in v1 of this plan was wrong. Both halves are stated here.

**Covered: protecting the outer scope's Σ.** The aggregate admission guard, not the throttle,
is what bounds the outer scope: `Σ(committed worker memory.max) + supervisor live
memory.current + this request ≤ outerMax − headroom`, evaluated per grant
(`internal/daemon/worker_admit.go`, `scanWorkerScopeChildren` summing each child's
`memory.max`). A worker growing to its own `memory.max` is already fully reserved against the
ceiling. That guard is only *sound* once swap is capped — a worker with unlimited swap has a
footprint the Σ over `memory.max` does not bound — so change (2) repairs the aggregate guard
as well as the containment invariant.

**Not free: post-grant slack shrinks, and worker pages stop being reclaimable.** Today
`memory.high` empirically pins workers *below* their caps (§1.2's "zero `max` events"),
leaving roughly the top 20% of each worker's Σ contribution as de-facto slack, and a worker's
anon pages remain swappable if the outer scope comes under pressure. After this change,
workers can occupy their full committed `memory.max`, and their anon pages are unreclaimable.
The remaining protection against an outer-scope OOM is `workerAdmitHeadroomDefault`
(64 MiB, `worker_admit.go:25`) plus the fact that `supervisor.memory.current` is re-read at
every grant. The exposure is therefore the supervisor's growth *after the last grant of the
run* — unchanged in size by this PR, but changed in failure mode: previously it degraded via
swap, now it reaches `oom.group`. **This PR does not retune the 64 MiB headroom** (that needs
its own measurement of real supervisor growth and is not what the ticket asked for); it is
recorded as a named residual risk in §7 rather than hidden behind "loses nothing".

**Covered mechanically, but weakened in practice: the proactive-recycle watermark.**
`_should_recycle` is a *userspace* check that needs a *number* to compare `memory.current`
against; it never needed the kernel to throttle on that number, so reading `memory.max` with
the fraction folded in gives the identical decision at each check point. What changes is how
often the worker *reaches* a check point: `_should_recycle` runs **only between tests**
(`worker.py:244-265`, called from `run_worker_loop:487`), and today `memory.high` throttles a
worker that is past 64% of its cap so hard (§1.2) that it very often survives to the next
check and retires cleanly. With the throttle gone, a worker that accumulates *within a single
test* reaches `memory.max` mid-test and is group-killed instead. Spec §7's "a worker crossing
its watermark self-recycles *before* that" therefore becomes materially less reliable, not
merely re-keyed to a different file, and §4.5 amends the clause's **strength**, not just the
filename. This is not a regression being smuggled in: the between-tests check never could
help a genuinely mid-test leaker (the ticket says so itself), and what the throttle was
providing was not containment but concealment — it made a leak slow enough to look like a
graceful retirement while the process sat in an unkillable reclaim path for minutes.

Removing it also makes worker scopes consistent with every other AIRA scope: `aira confine`
writes `memory.high` only when `--memory-high` is passed, so today the aitest worker scope is
the one scope kind on the machine that always carries a kernel throttle.
`writeScopeMemoryCap`'s `high > 0` branch stays for that flag; aitest simply stops using it.

## 4. Changes, atomically in one PR

### 4.1 `internal/runner` — scope creation and the swap cap

- `CreateWorkerScope(ctx, outerScope, workerID string, memoryMax int64) (path, swapCap string, err error)`:
  drop the `memoryHigh` parameter; call `writeScopeMemoryCap(scope, memoryMax, 0, true)`;
  return the swap disposition so the daemon can put it on the grant line.
  **The `memoryHigh >= memoryMax` guard (`worker_scope_linux.go:31-33`) is deleted, not
  retained with a zero argument**, and so is the test that exists solely to pin it
  (`TestCreateWorkerScopeRejectsMemoryHighAtOrAboveMemoryMax`,
  `worker_scope_linux_test.go:55-88`). Stating this explicitly because the guard was added by
  a Sol build-review and its doc comment cites spec §3.3: an implementer who did not know the
  spec clause is being repealed would keep both the parameter and the guard. The `!linux` stub
  (`worker_scope_stub.go:10`) changes with it.
- New `writeWorkerScopeSwapCap(scope)`, called **after `writeScopeMemoryCap` has succeeded**
  (ordering is load-bearing: run before it, and an undelegated memory controller — which has
  no `memory.*` files at all — yields `ENOENT` and would be misread as "this kernel has no
  swap"). Writes `0` to `memory.swap.max` through the scope directory FD (`unix.Openat`, the
  `writeScopeMemoryValue` idiom) and verifies the read-back. Three honest dispositions:
  - write + verify succeed → **`enforced`**.
  - `ENOENT` on open, **and** `/proc/swaps` is proved absent → **`not-applicable`**:
    `/proc/swaps` is registered by the same `CONFIG_SWAP` build that registers
    `memory.swap.*`, so its absence means this kernel cannot swap and `memory.max` already
    bounds the whole footprint. No warning; the token still states the fact.
    **"Proved absent" means exactly that**: `stat("/proc/swaps")` failed with precisely
    `ENOENT` *and* a control path (`/proc/self/stat`) is readable, so `/proc` itself is
    demonstrably mounted. A missing `/proc`, an unmounted `/proc`, or any other `stat` errno
    establishes nothing and falls to `unavailable` — claiming "this kernel cannot swap" from a
    failure to look would be exactly the fake pass CLAUDE.md forbids.
  - `ENOENT` on open in any other circumstance → **`unavailable`**: a swap-capable kernel on
    which the control is absent (legacy `swapaccount=0`), or a host where the question could
    not be answered. The grant proceeds — denying would stall every aitest run on such a host
    — but it is **reported**, not swallowed (§4.6).
  - **any other error, including a verify mismatch → fail closed**: remove the scope, deny
    with `worker-scope-create-failed`, exactly as a failed `memory.max` write does. A scope
    whose controls are unreliable must never be handed out claiming containment.

### 4.2 `internal/daemon` — grant

- `worker_admit.go`: delete `memoryHigh := req.estimatedBytes * 4 / 5`; call
  `create(ctx, req.outerScope, workerID, req.estimatedBytes)`; drop `MemoryHigh` from the
  response struct and its literal; carry the returned swap disposition into `SwapCap`.
- `server.go` / `testing_seams.go`: `workerScopeCreate func(context.Context, string, string, int64) (string, string, error)`.

### 4.3 `internal/runner` — wire contract (the protocol edit)

- `workerAdmitGrant`: drop `MemoryHigh`/`memory_high`; add `SwapCap`/`swap_cap`.
- `WorkerAdmitLease`: drop `MemoryHigh`; add `SwapCap`.
- `workerAdmitGrantProblem`: drop both `MemoryHigh` cases; keep `MemoryMax <= 0`.
- `worker_admit_outcome.go`: `WorkerAdmitGrantFields` loses `MemoryHigh`, gains `SwapCap`;
  `WorkerAdmitOutcomeLine` renders `swap_cap=` (omitted when empty, mirroring `cpu_slots`);
  `ParseWorkerAdmitOutcomeLine` matches. New `WorkerAdmitSwapCap{Enforced,NotApplicable,Unavailable}`
  constants alongside the `WorkerAdmitCPUSlots*` ones.
- `worker_admit_client_stub.go`: matching fields.
- `cmd/aira/main.go`: `MemoryHigh: lease.MemoryHigh` → `SwapCap: lease.SwapCap`.
  (`--memory-high` on `aira confine`/`aira run` is an unrelated flag; untouched.)

### 4.4 `internal/pylib/aitest` — supervisor and worker

- `supervisor.py`: `_OUTCOME_GRANT_FIELDS = ("scope", "worker_id", "memory_max")`; the
  positive-integer loop covers `memory_max` only.
- `supervisor.py`: `_note_swap_cap_state`, modelled directly on the existing
  `_note_cpu_slots_state` — say **once**, on the suite's own output, when `swap_cap` is
  `unavailable`: workers are memory-capped but their swap is not bounded on this host, so a
  worker may exceed its cap without being killed. Silent for `enforced` and `not-applicable`.
- `worker.py` `_should_recycle`: read `memory.max` instead of `memory.high`; rename
  `_DEFAULT_HIGH_WATERMARK_PCT` → `_DEFAULT_MEMORY_WATERMARK_PCT` and
  `AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT` → `AIRA_AITEST_WORKER_MEMORY_WATERMARK_PCT`.
  **Default becomes 64, and that is a no-op by construction**: today the check is
  `memory.current / memory.high > 80%` with `memory.high = 0.8 × memory.max`, firing at
  `0.8 × 0.8 = 64%` of `memory.max`. This deliberately does not retune the fraction — spec §6
  lists it as an open question needing field data, and AIRA-32's closing note (which this PR
  amends) already recorded that AIRA-35 would move the read to `memory.max` if removal won.
  Existing fail-open branches (unreadable, `max`, non-positive) are kept verbatim.
- `supervisor.py` `_handle_worker_exit` (`:1778`): **before** `_retire_worker` (`:1229`,
  which reaches `_forget_worker_scope`'s `os.rmdir` at `:1297`) removes the scope, read the
  worker scope's `memory.events`; when `oom_group_kill` > 0, use an unevaluated reason that
  names the cap and the knob — "worker N was killed by its own H-byte per-worker memory cap
  (raise `AIRA_AITEST_ESTIMATED_BYTES`), and the one retry was too" — instead of today's
  generic "stopped reporting". This is required by, not incidental to, the change: §1.3(A)
  means a worker over 512 MiB silently swaps and *passes* today and will be killed and
  reported `unevaluated` after this PR, so the report has to say why and what to turn.
  Attribution is sound because `memory.events` counters are per-scope and only propagate
  upward, so `oom_group_kill > 0` on *this* scope means *this* scope's `oom.group` fired.
  **Fail-safe, stated so it cannot be implemented as a guess**: if `state.get("grant")` is
  absent (a containment-stripped/fallback worker, `:1073-1086`), the scope directory is
  already gone, `memory.events` is unreadable, or the `oom_group_kill` key is missing (older
  kernels), fall back to today's generic reason. A fabricated OOM claim is worse than a vague
  true one.

### 4.5 Spec amendment — same PR, no silent drift

`docs/superpowers/specs/2026-09-01-aitest-design.md`:

- §3.3 (:184-188): the `memory.high`/`memory.max` split bullet is **replaced** — each worker
  scope gets `memory.max` (hard containment, `oom.group=1`) and `memory.swap.max=0`, no
  `memory.high` — with the measured justification and the "the aggregate guard, not the
  throttle, protects the outer Σ; and the guard is only sound once swap is capped" reasoning.
- §3.3 (:189-192): note that per-suite peak-history sizing remains **deferred** and the
  shipped default is a flat 512 MiB (`__init__.py:137-148`), so the spec no longer reads as if
  history-based sizing were live.
- §3.4 (:312): watermark is a fraction of `memory.max`.
- §3.7 wire sketch (:209): `memory_high` out, `swap_cap` in.
- §5 Slice 3 (:491): `memory.high` is retired for worker scopes; the remaining tuning surface
  is the `memory.max` watermark.
- §6 (:502): the open question is retained and re-pointed at `memory.max`.
- §7 (:524-527): **both** clauses — the containment invariant now states its dependence on
  `memory.swap.max=0` and that the test asserting it must not pre-cap swap on the ancestor;
  and the self-recycle clause is re-keyed to the `memory.max` watermark **and re-stated at its
  true strength** (§3): it is a between-tests best effort that a mid-test grower can outrun,
  not a guarantee that a worker retires before its cap.

`.aira/tickets/AIRA-32.md`: append a short amendment noting the env-var rename and the
`memory.max` read, so its closing resolution does not name a variable that no longer exists.

### 4.6 Honest reporting of the swap disposition

`swap_cap` on the grant line, consumed by `_note_swap_cap_state` — the same shape
`worker_admit_outcome.go` already argues for `cpu_slots` ("a governance dimension whose
fail-open is invisible to the run it affects is how a subsystem ships inert"). One field is
removed and one added; the net wire size is unchanged and the honesty rule is satisfied
without a `log.Printf` that the affected pytest run would never see.

## 5. Test plan (TDD order)

Each written before its implementation, each stated with what makes it *non-porous*.

1. **`internal/runner`, real-cgroup, un-gated — `TestWorkerScopeOOMGroupKillConvergesPromptly`.**
   The committed reproduction of §1.2. Builds a scope through the *production*
   `runner.CreateWorkerScope` under a `+memory` parent with **no ancestor swap cap**, launches
   a real allocator into it via `clone3(CLONE_INTO_CGROUP)`, asserts `memory.events`
   `oom_group_kill` ≥ 1 **within 5 s**. Corroborating: `memory.swap.peak` == 0 *if that file
   exists* (added ~6.5) — reported as unevaluated on older kernels rather than skipped
   silently or asserted falsely.

   **Run as a table over two caps: 32 MiB and 512 MiB — the shipped production default
   (`__init__.py:137-148`).** A 32 MiB-only test is not a regression guard for this product:
   §1.2 shows `memory.high` at 95% takes 0.73–1.71 s there, comfortably inside a 5 s bound, so
   a future re-add of the *runner-up fix this plan rejected* would pass it. The rejection
   rests entirely on the 512 MiB row (16.42/18.42 s), so the test must exercise that row. At
   512 MiB the no-`memory.high` measurement is 0.361/0.481 s — a ~10× margin under the same
   5 s bound — while **both** 80% and 95% fail it. Each case's footprint is bounded by the
   scope's own `memory.max` and lasts under half a second; the scope is a sibling of, not a
   child of, the enclosing `aira confine` scope, so the 512 MiB is charged to `aira.slice`
   (64 GiB) and cannot OOM the test job itself (verified during measurement: the probe ran
   512 MiB children while its own confine reserve was 9.7 MiB).
2. **`internal/runner`, real-cgroup — the negative control for finding (A).** Small (8 MiB
   cap, 64 MiB allocation), and deliberately **not** built through `CreateWorkerScope` — after
   this PR that function always writes the cap, so the control must construct an
   uncapped-swap scope by hand. It is asserting a fact about the *platform* ("`memory.max`
   alone does not contain when swap is available"), which is the premise the production change
   rests on; it is not testing AIRA code.
   The child **holds its allocation** and signals the parent, so the observation window is
   parent-controlled rather than a race: the parent then reads `memory.swap.peak` where that
   file exists (this host is 6.18) and falls back to a polled peak of `memory.swap.current`
   where it does not, asserting > 0 — positive proof the leaker escaped `memory.max` into
   swap. Asserting *"no `oom_group_kill`"* was rejected: it false-FAILs on a swapless host or
   when `aira.slice`'s shared 8 GiB swap budget is exhausted by another session. Gated on
   positive evidence that swap is available (an active device in `/proc/swaps` and a
   permissive ancestor `memory.swap.max`), reporting a skip with its reason otherwise —
   never a pass it could not establish. Kept small deliberately: it writes to shared swap.
3. **`internal/runner`, unit (fake scope, no real cgroup)** — `CreateWorkerScope` writes
   `memory.swap.max=0` and writes **no** `memory.high`; returns `enforced`; returns
   `not-applicable` vs `unavailable` correctly for the two `ENOENT` cases; and **fails closed,
   removing the scope**, for a non-`ENOENT` failure and for a verify mismatch.
4. **`internal/daemon/worker_admit_cli_granted_linux_test.go` (existing, real-cgroup)** — the
   only test crossing daemon → client → lease → `WorkerAdmitGrantFields` → rendered line →
   cgroupfs. Its `memory_high` parse (`:241-243`) and its `{memory.high, floorPage(...)}`
   kernel row (`:280`) are **replaced** by a `swap_cap` field assertion and a
   `{memory.swap.max, "0"}` kernel row. This is the cheapest non-porous proof that the
   production path really writes the cap, and it is where a half-applied protocol edit dies.
   **The expected token is derived, never hardcoded**: the fixture stats `memory.swap.max` in
   its own control parent cgroup; present ⇒ expect `enforced` and the `"0"` row, absent ⇒
   expect one of the two non-enforced tokens and skip the kernel row with its reason. A
   hardcoded `enforced` would false-FAIL on a `swapaccount=0` or `CONFIG_SWAP=n` host. The
   derivation is non-circular — it probes the *parent*, not the worker scope under test.
5. **`internal/runner`, unit** — `WorkerAdmitOutcomeLine`/`ParseWorkerAdmitOutcomeLine`
   round-trip with `swap_cap` and without `memory_high`; the exact `swap_cap` value is pinned
   (not "one of the three"), per the mutation-testing lesson recorded at
   `worker_admit_cli_granted_linux_test.go:245-260`.
6. **`internal/daemon`, unit** — the grant carries no `memory_high` and the correct `swap_cap`;
   the `workerScopeCreate` seam is called with exactly `(outerScope, workerID, estimatedBytes)`.
7. **`internal/pylib/aitest`, pytest** — `_should_recycle` fires at 64% of `memory.max` and not
   below, fails open on `max`/missing/non-positive, honours the renamed env var; the
   supervisor accepts a granted line with no `memory_high`, still rejects one missing
   `memory_max`, and warns exactly once on `swap_cap=unavailable` and never on the other two.
   **Existing tests that assert the opposite contract are inverted, not supplemented.** The
   complete list, enumerated from the source rather than sampled:
   - Python: `test_supervisor.py:329-340` (`assert "memory_high" in str(exc)`) and `:1141`,
     plus the ~30 fixture lines carrying `memory_high=`; `test_worker.py:706,744-814`
     (watermark against `memory.high`); `test_cpu_growth.py:229`.
   - Go seam arity / signature: `internal/daemon/worker_admit_test.go:112,128,297,1315,1373,1446`;
     `internal/daemon/cpuslots_gate_test.go:79-80`;
     `internal/daemon/cpuslots_real_cgroup_linux_test.go:88,162,208,211`;
     `internal/runner/worker_scope_linux_test.go:36,76,114`.
   - Go wire/field: `internal/runner/worker_admit_outcome_test.go:30,58,90,130,150,216`;
     `internal/runner/worker_admit_client_linux_test.go:62`;
     `internal/runner/worker_admit_classify_linux_test.go:299-315,340,349`.
   - **Deleted outright**: `TestCreateWorkerScopeRejectsMemoryHighAtOrAboveMemoryMax`
     (`worker_scope_linux_test.go:55-88`), with its guard (§4.1).
   Untouched, and checked to confirm it: every `ScopeMemoryHigh` site
   (`confine_linux_test.go:457`, `detach_linux_test.go:257,272`, `memory_size_test.go:132-140`)
   belongs to `aira confine`/`aira run`'s unrelated `--memory-high` flag.
8. **`internal/pylib`, e2e — un-gate `TestRealPytestAitestEndToEndRealDaemonAndCgroup`**:
   delete the `AIRA_AITEST_SLOW_E2E` skip, re-bound 4 min → **60 s**, and
   **delete the harness's ancestor `memory.swap.max=0` write (`:285`, inside the shared
   `newRealDaemonAndCgroupTestHarness`, `:262`)**. That deletion is what makes the un-gated
   test non-porous: with it, the test passes even against a revert of the production swap cap.
   Verified that nothing else pre-empts the mechanism on this path — `IsolatedScopeParent`
   sets no swap limit, and `aira.slice` carries `MemorySwapMax=8G`, not 0.
   On a host that cannot set `memory.swap.max` at all, this test's `test_oom.py` case would
   never converge and the 60 s `CommandContext` would expire into a spurious failure, so it
   **skips with its reason** when a control-cgroup probe shows the control is absent — the
   same derivation test (4) uses, never an assertion the host cannot satisfy.

Whole-suite `aira confine -- go test ./...` plus the aitest pytest suite; exact exit codes
recorded, `pass`/`fail`/`unevaluated` distinguished.

## 6. Committed reproduction

The `~/tmp` probe is scratch. Tests (1) and (2) are the committed, executable reproduction of
§1 — same mechanism, same measurement source (the scope's own `memory.events`), running in the
default suite on any real-cgroup host.

## 7. Risks

| Risk | Handling |
|---|---|
| **A worker whose real working set exceeds the flat 512 MiB default now dies instead of swapping — converting silent passes into `unevaluated`.** Sizing is a pinned constant, not peak-RSS history (`__init__.py:137-148`); v1 of this plan wrongly claimed otherwise. | This is the design working (spec §3.4: worker OOM is normal, requeue-once) and the containment the product claims. Made actionable rather than mysterious: §4.4's diagnostic names the cap and `AIRA_AITEST_ESTIMATED_BYTES`. The spec's stale "history-based sizing" wording is corrected in the same PR (§4.5). |
| Losing the soft throttle shrinks post-grant slack and makes worker pages unreclaimable to the outer scope. | Stated, not denied (§3). Residual exposure is supervisor growth after the last grant, covered by the 64 MiB headroom; deliberately not retuned here, and named so it is visible. |
| **Proactive self-recycle becomes less reliable**: without the throttle, a worker that grows within one test reaches `memory.max` mid-test and is killed rather than retiring at the between-tests check. | Stated in §3, and spec §7's clause is amended to its true strength (§4.5) rather than silently re-keyed. The between-tests check never could catch a mid-test leaker; the throttle was concealing them, not containing them, at the price of a minutes-long unkillable reclaim path. Killed-and-requeued is the design's own stated normal outcome (spec §3.4), and §4.4's diagnostic makes it actionable. |
| Un-gating the e2e re-exposes the shared machine to a multi-minute hang. | The hang lives in the `memory.high` throttle path this PR removes. The e2e keeps a hard `CommandContext` bound (60 s); test (1) keeps a 5 s bound with `cgroup.kill` cleanup. |
| Host without a settable `memory.swap.max`. | §4.1's three dispositions: proved-`not-applicable`, reported-`unavailable`, or fail-closed. Never a silent claim of containment. |
| Capping swap raises the enclosing confine job's measured peak RSS, feeding `confine_peak_history`/`resolveAdmitReserve` toward larger future reserves and fewer concurrent jobs machine-wide. | Named. It is also *correct* — the reserve now reflects the job's true RAM need instead of a number deflated by swap — but it is a real machine-wide effect and is recorded rather than discovered later. |
| Protocol edit lands half-applied. | Both sides plus the spec change in one PR; tests (4), (5) and (8) each fail if either half is missed. |
| A sibling agent lands conflicting work on `worker_admit.go`. | Re-check `origin/master` and rebase immediately before merge (standing rule 12). |
| Test (2) writes to shared swap in the default tier. | Kept to a 64 MiB allocation and gated on positive evidence swap is available. |

## 8. Explicit deferrals

- **The watermark fraction is not retuned** (§4.4). AIRA-32 owns it; this PR only records that
  the surviving key is `memory.max`.
- **The 64 MiB aggregate headroom is not retuned** (§3).
- **The outer/supervisor scope's swap is not capped.** The supervisor is deliberately given no
  `memory.max` (spec §3.3); capping its swap is a separate decision with a different blast
  radius.
- **`aira confine`'s own scopes are not given `memory.swap.max=0`.** They have the same latent
  "`memory.max` does not bound swap" property — with `aira.slice` allowing 8 GiB of swap, a
  confined job's `memory.max` is not the bound it appears to be. That is a materially larger
  change touching every confined job on the machine, so it is filed as its own ticket
  (allocated with `aira id`, never hand-picked) rather than smuggled in here.
