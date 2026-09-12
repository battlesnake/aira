<!-- Ordered implementation slice plan for aitest v0.7 STAGE S1 (of the design's §11).
     Source of truth for the slice-by-slice two-loop build. Produced 2026-09-12,
     advisor-checked (S1c-first ordering; the Σ(pending) porous-test fix; the age-cap
     move to S2; the ci-shim mode story). -->

# aitest v0.7 — Stage S1 implementation plan

> **For agentic workers:** each slice is ONE two-loop — an Opus sub-agent builds (TDD), a
> Fable sub-agent reviews, and each is mutation-verified — per [[aira-complex-work-opus-fable]].
> This is correctness-critical work (a wrong outer-cap read = a whole-suite `oom.group` kill),
> so the two-loop is MANDATORY. Steps are slice-level (Goal/Files/Tests+Mutation/Deps/Parallel),
> matching the established `docs/superpowers/plans/2026-09-01-aitest-slice1.md` +
> `docs/superpowers/specs/2026-09-10-admission-counter-slice-plan.md` pattern, NOT the generic
> bite-sized form.

**Goal:** land the v0.7 *foundation + the load-bearing correctness guard + the measurement
harness*, on the shipped v0.6 flat one-at-a-time worker loop — WITHOUT yet flipping worker sizing
to per-class or adding per-class dispatch/growth (those land coupled in S2, so there is never an
unsafe "sized-to-class-C but handed a class-D test" intermediate).

**Architecture:** a client-side (aitest supervisor, Python) aggregate outer-cap guard fixes the
latent v0.6 whole-suite-kill hazard (AIRA-229) using the *flat* per-worker request the shipped
model already computes; then the `aira_mem` marker reader + size-class ladder are added as pure,
unwired foundation; then a committed measurement harness gathers the numbers that fix v0.7's
tunables (256 MiB default, per-worker headroom, watermark fraction, supervisor allowance, the
MAX_TESTS fork). No daemon protocol change; no Go behaviour change (one stale doc-comment fix).

**Tech Stack:** Python (pytest plugin: `internal/pylib/aitest/{supervisor,worker,__init__}.py`);
Go (one doc-comment fix in `internal/runner/worker_scope_linux.go`); cgroup-v2 `memory.max`
ancestry reads; `aira confine` for all heavy/real-cgroup test runs.

**Spec:** `docs/superpowers/specs/2026-09-12-aitest-v07-class-sized-workers-design.md` (§4.1, §4.2,
§7 item 1, §8, OD3, §11). Ticket for the correctness fix: **AIRA-229**.

## Global Constraints (verbatim from the spec + repo rules)

- **Reservation = containment cap = ledger charge, per worker** — one number, no decoupling that
  sizes `memory.max` above the ledger charge (spec §8 inv 3, §4.2).
- **`aira_mem` declares INCREMENTAL peak RSS** (on top of the worker warm-import baseline), not
  total worker RSS (spec §4.1). A worker's cap is therefore `class-cap + per-worker headroom`, never
  bare `max(annotation)`.
- **No protocol bump; no new wire fields** (spec §9). Stays protocol 11.
- **Size grammar is shared** — `aira_mem` uses the SAME parser as `AIRA_AITEST_ESTIMATED_BYTES`
  (`_parse_estimated_bytes`, `__init__.py:246`) and Go `runner.parseMemorySize`: `512M`, `1.5G`,
  `512MiB`, a bare byte count all mean the same. 1024-based. `_ESTIMATED_BYTES_MIN = 1<<20`,
  `_MAX = 1<<50` (`__init__.py:225,228`) apply.
- **Invalid input never silently swallowed** — warn once, treat as unannotated/default (the AIRA-223
  discipline, `__init__.py:281-295`).
- **256 MiB default + per-worker headroom + watermark fraction + supervisor allowance are NOT
  load-bearing pre-measurement** — they are field-tunable starting points that S1's measurement
  slice (v7-4) exists to set. Do not hard-code any of them as if measured (spec OD4, §4.2, §8).
- **Heavy/real-cgroup test runs go under `aira confine -- `**; record exact exit codes, never
  green-from-truncation; the local pre-push `make test` gates master and flakes on a contended box
  (retry the push — see the RANT follow-up).
- **Attribution:** commits end `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## Decision gates (resolve before the noted slice — not code)

- **D1 — RESOLVED (this plan, advisor-confirmed): S1 sizes NOTHING per-class at spawn.** The spec
  §11 lists "size-class worker sizing" under S1, but per-class *dispatch* is explicitly S2. Sizing a
  worker to class C without dispatch that hands it only class-C tests would let it pull a bigger test
  and OOM mid-run. So S1 builds the reader (v7-2) and the ladder (v7-3) as **pure, unwired
  foundation** and leaves the actual spawn request on the shipped flat `_run_estimated_bytes`; S2
  flips sizing to per-class **and** adds per-class dispatch **together**. The aggregate guard (v7-1)
  is correct regardless — it guards whatever request is passed.
- **D2 — RESOLVED (this plan, advisor-confirmed): the `Σ(pending requests)` guard term is OWED BY
  S2, not built here.** In S1's single-threaded supervisor the spawn/admit path has at most ONE
  admission in flight (`_try_grow_one` is 1 Hz and issues one at a time; blocking `spawn_worker`
  waits on its relay), so `Σ(pending) ≤ 1` and the term is INERT — a test cannot make its removal
  change a decision, i.e. the mutation would be porous. S1's guard is `Σ(live caps) + supervisor
  allowance + request`; its mutation guard is "drop `Σ(live caps)` → the over-admission test reds."
  S2 adds the `Σ(pending)` term WITH concurrent per-class claims that make its mutation reachable.
- **D3 — RESOLVED (this plan, advisor-confirmed): the ~10 s age-cap flip is DEFERRED to S2** (spec
  §11 put it in S1). Rationale (spec §6): flipping `AIRA_AITEST_WORKER_MAX_SECONDS` 600→10 before
  S2's growth claim exists buys NO fairness, multiplies relay forks + lost-race scope churn on the
  AIRA-36 reaper ~60×, AND truncates v7-4's residue measurement, which needs the 600 s / 200-test
  window to observe cross-test accumulation over hundreds of tests. v7-4 sets
  `AIRA_AITEST_WORKER_MAX_SECONDS=10` only inside the specific short-window measurement harness that
  needs it. This is the plan correcting the spec's coarse staging — the `SEQUENCING NOTE` pattern.
- **D4 — the aggregate guard is MODE-DEPENDENT (mirrors the v0.6 admission-counter D4).** In
  `advisory(ci-shim,no-cgroup,no-kill-backstop)` mode (`supervisor.py:488`) there is NO outer cgroup
  to read and NO `oom.group` to fire — the whole-suite-kill hazard does not exist — so the guard
  reports `unevaluated` and SKIPS (spawn proceeds), it does NOT fail closed. It refuses only in the
  real-cgroup delegate path where `effective_outer_cap` is readable and finite. The spec's "the outer
  scope always has a finite memory.max" (§7) is a real-cgroup-mode statement.

---

## SLICES (ordered; each = one two-loop, mutation-verified)

### v7-1 — The aggregate outer-cap Σ-guard + Tier-1 dual-ceiling (fixes AIRA-229). ★ load-bearing correctness slice ★  **LANDS FIRST**

1. **Goal:** before spawning ANY worker, the supervisor refuses an over-admitting spawn:
   `Σ(this suite's live worker memory.max) + supervisor_allowance + request ≤ effective_outer_cap − headroom`,
   AND marks a single request that alone exceeds a hard ceiling **terminal-`unevaluated` up front**
   (Tier 1): `request > (slice_ceiling − headroom)` OR `request > effective_outer_cap`. Here
   `request` is the number the daemon writes as the worker scope `memory.max` — in S1 that is the
   flat `self._run_estimated_bytes` the shipped code already computes; the guard does NOT need the
   ladder. `effective_outer_cap` = the **min finite `memory.max` over the outer scope's cgroup
   ancestry** (outer_scope → … → cgroup2 mount root), read ONCE at startup — a Python replica of
   `effectiveConfineCap`/`effectiveCapFrom` (`internal/runner/confine_linux.go:2914-2947`): walk
   parents via the dirname up to the unified-mount root, take the minimum finite `>0` value;
   **uncapped (no finite cap anywhere) → `(0, not-found)`**. **MODE (D4):** in
   `advisory(ci-shim,no-cgroup)` mode (`supervisor.py:488`) there is no outer cgroup and no
   `oom.group` — the guard reports `unevaluated`/skips (spawn proceeds), it NEVER refuses; it binds
   only in the real-cgroup delegate path. **`supervisor_allowance`** = a fixed reserve-with-margin
   for the supervisor's own `memory.current` (it runs in `.aira-supervisor` with no `memory.max`, so
   its RSS charges the outer cap) — a starting constant here, MEASURED in v7-4 (spec §8 second
   accepted-gap); bounded, not airtight, and documented as such. The **`Σ(pending requests)` term is
   OWED BY S2** (D2 — inert in S1). Also **fix the stale doc comment** at
   `internal/runner/worker_scope_linux.go:52` (it still promises "the daemon's aggregate admission
   guard," which S15 deleted at `worker_admit.go:461-466`) — replace it with the as-built truth: the
   daemon checks only the slice ceiling (`worker_admit.go:473`), and the aggregate outer-cap bound
   is now the client-side guard this slice adds.
2. **Files:**
   - `internal/pylib/aitest/supervisor.py`: a `_effective_outer_cap()` min-over-ancestry reader
     (called once, cached on the supervisor); a `_would_breach_outer_cap(request)` guard consulted
     immediately before every `_spawn_admit_relay(...)` call site that grants a worker — in
     `acquire_worker` (:883/:916), `_try_grow_one` (:1911/:1948) and any other spawn path; a
     `supervisor_allowance` constant; the Tier-1 fail-fast that dispositions a too-large request as
     terminal-`unevaluated` (reuse the existing worker-admit outcome vocabulary / classifier at
     :440-564, do NOT invent a new disposition); track `Σ(live worker memory.max)` from the grants
     the supervisor already records (`_observe_worker_usage`, :1704-1733, already sees `memory_max`
     per grant — extend it to a live SUM keyed by worker, decremented on worker exit, not just the
     `max` it tracks today).
   - `internal/runner/worker_scope_linux.go:~52`: the stale doc-comment fix (Go, comment-only).
3. **Tests (real-cgroup, under `aira confine -- `):**
   - A delegate suite whose outer scope has a SMALL `--memory-max` and enough workers that
     `Σ(caps)` would exceed it: assert the guard REFUSES the over-admitting spawn and the outer
     `oom.group` **NEVER fires** (drive it to the edge; the whole-suite-kill is the hazard). This is
     THE load-bearing new test.
   - A single `request > effective_outer_cap` (min over ancestry, e.g. a small parent cap above a
     large scope cap) → **terminal-`unevaluated` up front**, no launch.
   - A `request > (slice_ceiling − headroom)` → terminal-`unevaluated` (reuse the shipped
     `WorkerAdmitReasonExceedsCeiling` disposition surface).
   - **ci-shim/advisory mode:** the guard reports `unevaluated` and the spawn PROCEEDS (does not
     refuse) — the no-outer-cgroup case.
   - The ancestry read: a scope under a capped PARENT but itself uncapped is bounded by the parent
     (mirror `effectiveConfineCap`'s hierarchical semantics); a fully-uncapped ancestry → the guard
     treats the outer cap as absent → real-cgroup mode with no finite cap behaves like advisory
     (unevaluated/skip), NEVER a divide-by-nothing refuse-all.
   - **Mutation:** removing the `Σ(live caps)` term from the guard (so it checks only
     `request ≤ cap − headroom`) MUST red the over-admission-refusal test. (The `Σ(pending)`
     mutation is OWED by S2 — named here, not built.)
4. **Deps:** none. **Lands first** (it is a v0.6 correctness fix on the pinned release and survives
   even an owner veto of the whole class-sized model).
5. **Parallel:** N (first).

### v7-2 — the `aira_mem` marker reader + `nodeid → bytes` map

1. **Goal:** register the marker and read it at collection into a map, WITHOUT yet driving sizing.
   `config.addinivalue_line("markers", "aira_mem(size): declares the test's incremental peak RSS …")`
   in `pytest_configure` (`__init__.py:30`) so `--strict-markers` suites don't fail and it shows in
   `pytest --markers`. At collection, for each `session.items` entry (alongside the existing
   `items_by_nodeid` build at `supervisor.py:818`), `item.get_closest_marker("aira_mem")` → parse
   its single size argument via `_parse_estimated_bytes` → attach to a `nodeid → bytes` map;
   unannotated nodeids map to `AIRA_AITEST_DEFAULT_BYTES` (**256 MiB starting point**, a new env with
   the shared parser, NOT load-bearing pre-measurement); an invalid argument warns once and falls to
   the default (AIRA-223). The map is computed before any worker is admitted and inherited by forked
   workers via COW; it is UNUSED by admission in S1 (foundation for v7-3/S2).
2. **Files:** `internal/pylib/aitest/__init__.py` (the `addinivalue_line` registration; the
   `AIRA_AITEST_DEFAULT_BYTES` resolver reusing `_parse_estimated_bytes`); `internal/pylib/aitest/
   supervisor.py:~818` (build the `nodeid → bytes` map beside `items_by_nodeid`).
3. **Tests:** `aira_mem("512M"/"1.5G"/"512MiB"/bare-int)` all parse to the right byte counts;
   unannotated nodeid → default; invalid arg (`"4GB"`-style garbage) → warn-once + default (assert
   the warning fires exactly once, AIRA-223 style); `--strict-markers` suite stays green; marker
   closest-wins precedence (class vs function) via `get_closest_marker`. **Mutation:** dropping the
   parse-and-store line (so every nodeid silently maps to the default) MUST red a
   map-content assertion on an annotated nodeid.
4. **Deps:** v7-1 (ordered after, so the correctness fix ships first; functionally independent).
5. **Parallel:** could run disjoint from v7-1's regions, but keep serial for review clarity.

### v7-3 — size-class ladder + per-worker request sizing (pure function, unwired)

1. **Goal:** a PURE function mapping the `nodeid → bytes` map to a coarse **power-of-2 size-class
   ladder** (`≤256 MiB, ≤512 MiB, ≤1 GiB, ≤2 GiB, …`, each annotation rounded UP to its bucket cap)
   and a per-class worker request `= class_cap + per_worker_headroom`. Coarse ladder ONLY — no
   exact-value buckets (they fragment: 256/300/512/600 → four tiny buckets that never fill, spec
   OD2). This function is BUILT AND TESTED but **not wired into the spawn request** in S1 (D1 — that
   plus per-class dispatch is S2). `per_worker_headroom` is a starting constant, measured in v7-4.
2. **Files:** `internal/pylib/aitest/supervisor.py` (or a small new pure module, e.g.
   `internal/pylib/aitest/sizeclass.py`, if it keeps `supervisor.py` focused — reviewer's call).
3. **Tests:** 300 MiB → the 512 MiB bucket (round UP, ~70 % over-reserve, bounded to one step);
   exact-cap boundary (512 MiB annotation → the 512 MiB bucket, not 1 GiB); the ladder is coarse
   (assert a fixed small set of buckets, no per-value buckets); request = `class_cap + headroom`
   (assert the sum, not the bare cap — the incremental-peak semantics of §4.1). **Mutation:**
   flipping round-UP to round-DOWN MUST red a boundary test (a 300 MiB test would then get a 256 MiB
   cap and the invariant "the cap covers the annotation" breaks).
4. **Deps:** v7-2 (consumes the `nodeid → bytes` map).
5. **Parallel:** N.

### v7-4 — measurement instrumentation + committed repro harness

1. **Goal:** instrument and MEASURE, on AIRA's own suite (dogfood, spec §8/§11), the numbers that
   fix v0.7's tunables: (a) per-worker **RSS-vs-cap** over a worker's life; (b) the **OOM-kill /
   requeue rate** (read the worker scope's `memory.events` `oom_kill` counter where available, else
   infer from the `pidfd`-observed death + requeue path); (c) **cross-test residue on a fast unit
   suite** across the **600 s / 200-test** window (KEPT — do NOT flip to 10 s here; the short window
   truncates the residue signal, D3); (d) the **supervisor `memory.current` under load** (to set the
   v7-1 `supervisor_allowance`). Emit a structured measurement report; commit an EXECUTABLE
   reproduction (CLAUDE.md: "a published measurement must have a committed, executable
   reproduction"). Where short-window (~10 s) data is needed for the MAX_TESTS-vs-watermark fork, the
   harness sets `AIRA_AITEST_WORKER_MAX_SECONDS=10` **for that harness run only** — it does NOT
   change the default.
2. **Files:** `internal/pylib/aitest/supervisor.py` + `worker.py` (sampling hooks — RSS,
   `memory.events`, requeue counters); a committed harness script under the repo's measurement
   convention (mirroring existing committed measurement repros); a short measurement report doc.
3. **Tests:** the instrumentation emits the four metric families on a controlled run; the harness is
   runnable under `aira confine -- ` and produces the report; a smoke assertion that the sampler
   reads a non-nil RSS/`memory.events` where the kernel exposes them and reports `unevaluated`
   (never a fake 0) where it does not (the AIRA honesty rule).
4. **Deps:** v7-1, v7-2, v7-3.
5. **Parallel:** N.

---

## §11 REFINEMENT NOTE (this plan vs the spec's coarse staging)

The spec §11 bundled the ~10 s age-cap flip and "size-class worker sizing" into S1. This plan
refines that (advisor-confirmed), for the reasons in D1/D3 above:
- **size-class worker sizing that DRIVES the spawn request + per-class dispatch move to S2** (never
  an unsafe sized-but-not-dispatched intermediate). S1 lands the reader + ladder as unwired
  foundation.
- **the ~10 s age-cap default flip moves to S2** (it needs S2's growth claim to buy fairness, and
  flipping it early truncates v7-4's residue measurement + multiplies scope churn ~60×).
- **the `Σ(pending requests)` guard term + its mutation guard are OWED BY S2** (inert in S1's
  single-threaded ≤ 1-pending model).

## Self-review (against the spec)

- §4.1 `aira_mem` reader, registration, grammar, default, invalid-handling, incremental semantics →
  v7-2 (+ default doc). ✓
- §4.2 size-class sizing, `class-cap + headroom`, reservation=cap=charge → v7-3 (pure) + Global
  Constraints; wired in S2 per D1. ✓
- §7 item 1 / OD3 aggregate Σ-guard, effective-outer-cap min-over-ancestry, Tier-1 dual-ceiling,
  mode-dependence → v7-1 (Σ(pending) deferred per D2). ✓
- §7 item 2 per-size-class pool-usage sample → S3 (feedback), not S1 (noted, out of scope here). ✓
- §8 accepted gaps (mid-test accumulation OOM; supervisor allowance bounded-not-airtight) →
  MEASURED in v7-4; allowance constant + doc in v7-1. ✓
- OD4 tunables (256 MiB, headroom, watermark, MAX_TESTS fork) → starting constants + MEASURED in
  v7-4; not fixed as load-bearing. ✓
- Stale `worker_scope_linux.go:52` comment → v7-1. ✓
- Coverage GAP accepted here: the actual per-class spawn sizing, per-class dispatch, growth claim,
  age-cap flip, and `Σ(pending)` term are all S2 — explicitly deferred, not silent.
