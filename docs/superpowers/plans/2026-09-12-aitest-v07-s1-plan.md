<!-- Ordered implementation slice plan for aitest v0.7 STAGE S1 (of the design's §11).
     Source of truth for the slice-by-slice two-loop build. Produced 2026-09-12.
     v1 advisor-checked (guard-first ordering; the Σ(pending) porous-test fix; the age-cap
     move to S2; the ci-shim mode story). v2 folds the Fable plan-gate (PASS-WITH-NITS):
     4 P1s reshaping v7-1 (Σ derived at check time; two dispositions collapsing Tier-1; no
     client-side slice-ceiling replica; allowance = base + N×relay), v7-3 (ladder) moved to
     S2 as premature, v7-4 reuses existing reads (no new sampler), + P2/P3 refinements. -->

# aitest v0.7 — Stage S1 implementation plan

> **For agentic workers:** each slice is ONE two-loop — an Opus sub-agent builds (TDD), a
> Fable sub-agent + external sidecars review, and each is mutation-verified — per
> [[aira-complex-work-opus-fable]]. This is correctness-critical work (a wrong outer-cap read =
> a whole-suite `oom.group` kill), so the two-loop is MANDATORY. Steps are slice-level
> (Goal/Files/Tests+Mutation/Deps/Parallel), matching the established
> `docs/superpowers/plans/2026-09-01-aitest-slice1.md` +
> `docs/superpowers/specs/2026-09-10-admission-counter-slice-plan.md` pattern.

**Goal:** land the v0.7 *load-bearing correctness guard* (fixing AIRA-229), the *`aira_mem`
marker+reader* as a documented-inert foundation primitive, and the *measurement harness* that
fixes v0.7's tunables — all on the shipped v0.6 flat one-at-a-time worker loop, WITHOUT flipping
worker sizing to per-class, adding per-class dispatch, or building the size-class ladder (those
land coupled in S2, so there is never an unsafe "sized-to-class-C but handed a class-D test"
intermediate, and the ladder is built only after v7-4 measures its bucket/headroom shape).

**Architecture:** a client-side (aitest supervisor, Python) aggregate outer-cap guard fixes the
latent v0.6 whole-suite-kill hazard (AIRA-229) using the *flat* per-worker request the shipped
model already computes; the `aira_mem` reader + `nodeid→bytes` map are added as a pure, unwired
foundation; and a measurement harness (reusing the RSS/oom reads the code already has) gathers the
numbers that fix v0.7's tunables (256 MiB default, per-worker headroom, watermark fraction, the
`outer_cap_allowance` base + per-relay term, the MAX_TESTS fork). No daemon protocol change; no Go
behaviour change (three stale doc-comment fixes only).

**Tech Stack:** Python (pytest plugin: `internal/pylib/aitest/{supervisor,worker,__init__}.py`);
Go (comment-only fixes in `internal/runner/worker_scope_linux.go` + `internal/pylib/aitest/
worker.py`); cgroup-v2 `memory.max` ancestry reads; `aira confine` for all heavy/real-cgroup runs.

**Spec:** `docs/superpowers/specs/2026-09-12-aitest-v07-class-sized-workers-design.md` (§4.1, §7
item 1, §8, OD3, OD4, §11). Correctness ticket: **AIRA-229**. Umbrella: **AIRA-230**.

## Global Constraints (verbatim from the spec + repo rules)

- **Reservation = containment cap = ledger charge, per worker** — one number, no decoupling that
  sizes `memory.max` above the ledger charge (spec §8 inv 3).
- **`aira_mem` declares INCREMENTAL peak RSS** (on top of the warm-import baseline), not total
  worker RSS (spec §4.1).
- **No protocol bump; no new wire fields** (spec §9). Stays protocol 11.
- **Size grammar is shared** — `aira_mem` uses the SAME parser as `AIRA_AITEST_ESTIMATED_BYTES`
  (`_parse_estimated_bytes`, `__init__.py:246`; note it `.strip()`s a *string* — a bare int must be
  `str()`-ed first). 1024-based. `_ESTIMATED_BYTES_MIN=1<<20`, `_MAX=1<<50` (`__init__.py:225,228`).
- **Invalid / malformed input never silently swallowed** — warn once, treat as unannotated/default
  (the AIRA-223 discipline, `__init__.py:281-295`).
- **256 MiB default, per-worker headroom, watermark fraction, `outer_cap_allowance`, and
  `outer_cap_margin` are NOT load-bearing pre-measurement** — field-tunable starting points that
  v7-4 exists to set (spec OD4, §8). Do not hard-code any as if measured.
- **Heavy/real-cgroup runs go under `aira confine -- `**; record exact exit codes, never
  green-from-truncation. The local pre-push `make test` gates master and flakes under box
  contention (RANT-44) — retry the push, don't route around the gate.
- **Attribution:** commits end `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.

---

## Decision gates (resolve before the noted slice — not code)

- **D1 — RESOLVED (advisor + Fable-gate confirmed): S1 sizes NOTHING per-class and builds NO
  ladder.** Per-class *dispatch* is S2; sizing a worker to class C without it would let it pull a
  bigger test and OOM. And the size-class ladder's bucket/headroom shape depends on v7-4's measured
  numbers, so building it in S1 is premature (Fable OVER-BUILD). So S1 keeps the spawn request on the
  shipped flat `_run_estimated_bytes`; **S2** builds the ladder AND per-class sizing AND per-class
  dispatch together, using v7-4's numbers. The guard (v7-1) is correct regardless of the request's
  provenance.
- **D2 — RESOLVED (Fable-gate VERIFIED against code, no P0): the `Σ(pending requests)` guard term
  is OWED BY S2, not built here.** The gate confirmed every admission is synchronous on the
  single-threaded loop (`_try_grow_one` probes at `supervisor.py:1931` then `spawn_worker`→
  `acquire_worker` forks ONE relay and reads its ONE outcome line `:916,:921`; a timed-out growth
  read terminates the relay before returning `:935`; `_maybe_grow_pool` is 1 Hz `:2032`; startup
  fill `:2505-2509` and the empty-pool path `:1893-1909` each issue one spawn per iteration). So at
  every guard check `Σ(pending)=0` — the term is INERT and its mutation would be porous. S1's guard
  is `Σ(live caps) + outer_cap_allowance + request`; its mutation guard is deterministic (see v7-1).
  S2 adds the `Σ(pending)` term WITH concurrent per-class claims that make its mutation reachable.
- **D3 — RESOLVED (advisor-confirmed): the ~10 s age-cap flip is DEFERRED to S2** (spec §11 put it
  in S1). Flipping `AIRA_AITEST_WORKER_MAX_SECONDS` 600→10 before S2's growth claim buys NO fairness
  (spec §6), multiplies relay forks + lost-race scope churn on the AIRA-36 reaper ~60×, AND
  truncates v7-4's residue measurement (which needs the 600 s / 200-test window to observe cross-test
  accumulation over hundreds of tests). v7-4 sets `AIRA_AITEST_WORKER_MAX_SECONDS=10` only inside its
  own short-window sub-run. This is the plan correcting the spec's coarse staging (the v0.6
  `SEQUENCING NOTE` pattern).
- **D4 — the aggregate guard is MODE-DEPENDENT (mirrors the v0.6 admission-counter D4).** In
  `advisory(ci-shim,no-cgroup,no-kill-backstop)` mode (`supervisor.py:488`) there is no outer cgroup
  and no `oom.group` — the hazard does not exist — so the guard reports `unevaluated` and SKIPS
  quietly (spawn proceeds), it does NOT refuse. In the real-cgroup delegate path a no-finite-cap
  ancestry is impossible by precondition (`confine_linux.go:2899-2901`; the bootstrap finite-max
  proof `aitest_bootstrap_linux.go:78`) — so if the real-mode replica ever finds no finite cap that
  is a BROKEN ASSUMPTION: WARN once, then skip (inside the spec §8 bounded-not-airtight envelope);
  NEVER silent.
- **D5 — RESOLVED (this plan, folding the Fable OVER-BUILD): S1 = {v7-1 guard, v7-2 reader, v7-4
  measurement}; the ladder (old v7-3) MOVES TO S2.** The reader is KEPT in S1 (the gate's deliberate
  call): it is the user-facing contract the owner explicitly asked for, its shape is stable
  (independent of measurement), and it is landed as a **documented-inert foundation primitive** (the
  map is built but has no S1 consumer — an accepted, recorded gap, not new machinery, per the
  architectural-simplicity "keep the primitive + document the gap" rule). The ladder's shape is NOT
  stable (it depends on v7-4), so it waits for S2.

---

## SLICES (ordered; each = one two-loop, mutation-verified)

### v7-1 — The aggregate outer-cap Σ-guard (fixes AIRA-229). ★ load-bearing correctness slice ★  **LANDS FIRST**

1. **Goal:** before spawning ANY worker, the supervisor applies ONE guard rule with TWO
   dispositions. Let `request` = the number the daemon writes as the worker scope `memory.max` (in
   S1 the shipped flat `self._run_estimated_bytes`); `Σ_live` = the sum of live workers' granted
   caps; `allowance` = the supervisor+relays reserve; `margin` = a safety band; `outer_cap` = the
   effective outer cap. **Rule:** spawn is permitted iff
   `Σ_live + allowance + request ≤ outer_cap − margin`. **On failure:**
   - if `Σ_live == 0` → **TERMINAL**: nothing running will ever free room, so fail the queue up
     front via `_fail_queue_terminal` (`supervisor.py:793`) with a NEW reason token (e.g.
     `outer-cap-exceeded`) — do NOT surface a retriable `WorkerAdmitDenied` (the empty-pool
     `_wait_for_admission_or_disable` path `:1893-1909` would retry forever with `Σ_live==0` and
     nothing freeing → hang). **This case subsumes Tier-1**: at `Σ_live==0` the rule is already
     stricter than "a single request exceeds the cap", so there is **NO separate Tier-1 function**.
   - if `Σ_live > 0` → **SKIP THE TICK** on the growth path (`_try_grow_one`/`_maybe_grow_pool`): a
     live worker's retirement will lower `Σ_live` and free room; retry next tick. Never busy-spin.
   - **`Σ_live` MUST be derived AT CHECK TIME** from the live workers map
     (`self.workers[*]["grant"]["memory_max"]`) — the same drift-proof read `_pool_covers_the_queue`
     makes (`supervisor.py:2465-2470`) — **NOT** a counter maintained in `_observe_worker_usage`
     (which runs only at retirement `:1697`, leaving the guard inert). Skip fallback workers
     (`grant is None`, `:1555`) and ledger-only grants (no `memory_max`).
   - **`outer_cap` = min finite `memory.max` over the outer scope's cgroup ancestry**, a Python
     replica of `effectiveConfineCap`/`effectiveCapFrom` (`internal/runner/confine_linux.go:
     2914-2947`): start at `self.outer_scope` (NOT `/proc/self/cgroup`), read each level's
     `memory.max`, keep the minimum finite `>0`; a read-error OR the literal `"max"` is "not finite,
     keep walking" (match `readConfineCap`, `:1920-1926`); STOP when the parent dir lacks
     `cgroup.controllers` (every cgroup2 dir has it; the mount root's parent does not). **Read at
     EVERY check — do NOT cache at startup** (it is 3–5 small file reads before a relay fork, and
     re-reading answers "the owner `set-property`'d `aira.slice` mid-run" for free). *(Spec §7 says
     "read once at startup" — record this as a spec amendment.)*
   - **`allowance` = `base + N_live × per_relay`** (NOT a fixed constant). The supervisor runs in
     the deliberately-uncapped `.aira-supervisor` (`worker.py:368-370`; `aitest_bootstrap_linux.go:
     74`) AND each `worker-admit` relay is a plain `Popen` (`supervisor.py:851-853`) that never
     relocates its own cgroup (no `cgroup.procs` write in `cmd/aira/main.go`) and holds the lease for
     the worker's whole life via open stdin — so N live relays ALSO charge the outer cap. `base`,
     `per_relay`, and `margin` are starting constants MEASURED in v7-4 (at FULL pool). Name the
     guard's band `outer_cap_margin` (distinct from the per-worker headroom of §4.2 and the daemon
     slice headroom — three different "headroom"s; do not conflate).
   - **Fold three stale as-built comments** (all cite guards/vocabulary S15 or AIRA-33 removed):
     `worker_scope_linux.go:~52` ("the daemon's aggregate admission guard" — replace with: the
     daemon checks only the slice ceiling `worker_admit.go:473`; the outer-cap aggregate bound is now
     THIS client-side guard); `worker.py:369-375` (cites dead `WorkerAdmitReasonOuterScopeUnbounded`
     — definition-only, nothing emits it); `supervisor.py:2457-2463` (`_pool_covers_the_queue`
     docstring still says the ledger sums children's `memory.max`, pre-S15).
2. **Files:** `internal/pylib/aitest/supervisor.py` — the `_effective_outer_cap()` walk; the
   `_would_breach_outer_cap(request)` guard (Σ-at-check-time + allowance + the two dispositions),
   consulted immediately before every worker-granting `_spawn_admit_relay` call site
   (`acquire_worker` :916; `_try_grow_one` :1948; the empty-pool `:1893-1909`; `_maybe_grow_pool`);
   the new `_fail_queue_terminal` reason token; the `base`/`per_relay`/`margin` constants; the
   docstring fix at :2457-2463. Comment-only: `internal/runner/worker_scope_linux.go:~52`,
   `internal/pylib/aitest/worker.py:369-375`.
3. **Tests (real-cgroup, under `aira confine -- `; a branch-exit gate — it CANNOT run in ci-shim CI,
   so do NOT assume it green from CI):**
   - **The load-bearing test:** a delegate suite whose outer scope has a SMALL `--memory-max` and
     workers that actually allocate near their cap; drive the pool to where `Σ_live + allowance +
     request` would cross `outer_cap − margin`; assert the guard REFUSES the over-admitting spawn and
     the outer `oom.group` **NEVER fires**.
   - `Σ_live==0` and `request` over the cap (e.g. a tiny outer `--memory-max`) → **terminal**
     (`_fail_queue_terminal`, the new reason token), no launch, no hang.
   - `Σ_live>0` and the next spawn would breach → **skip-tick**, the pool keeps its live workers, and
     a subsequent retirement lets a later tick spawn.
   - **ci-shim/advisory mode:** guard `unevaluated`, spawn PROCEEDS (does not refuse).
   - **ancestry semantics:** a scope uncapped itself but under a capped PARENT is bounded by the
     parent (mirror `effectiveConfineCap`); real-mode no-finite-cap anywhere → WARN-once then skip
     (assert the warning fires; never a refuse-all, never silent).
   - **Mutation (deterministic — no OOM race):** with `outer_cap`, `margin`, `allowance`, `request`
     fixed, assert the guard admits **at most `⌊(outer_cap − margin − allowance)/request⌋`**
     concurrent live workers; the mutant that drops the `Σ_live` term admits MORE than that and reds
     the assertion directly (independent of whether an OOM actually fires). The "outer `oom.group`
     never fires" e2e above is the complementary real-cgroup check (workers must allocate near cap,
     else the mutant passes it silently). *(The `Σ(pending)` mutation is OWED by S2 — D2.)*
4. **Deps:** none. **Lands first** — a v0.6 correctness fix on the pinned release; survives even an
   owner veto of the whole class-sized model.
5. **Parallel:** N (first).

### v7-2 — the `aira_mem` marker reader + `nodeid → bytes` map (documented-inert foundation)

1. **Goal:** register the marker and read it at collection into a map, WITHOUT driving sizing (D5 —
   no S1 consumer; recorded as an accepted foundation gap). `config.addinivalue_line("markers",
   "aira_mem(size): declares the test's incremental peak RSS …")` in `pytest_configure` —
   **placed BEFORE the early return at `__init__.py:31-33`**, else a plain `pytest` run on an
   annotated suite breaks under `--strict-markers`. At collection, for each `session.items` entry
   (beside the existing `items_by_nodeid` build at `supervisor.py:818`),
   `item.get_closest_marker("aira_mem")` → parse its single size argument → `nodeid → bytes` map;
   unannotated → `AIRA_AITEST_DEFAULT_BYTES` (**256 MiB starting point**, new env, shared parser, NOT
   load-bearing pre-measurement). Parsing: `_parse_estimated_bytes` calls `raw.strip()` so a bare-int
   argument (spec §4.1 "a bare int is bytes") must be `str()`-ed first; `aira_mem()` with NO argument
   or EXTRA arguments → warn-once + default (AIRA-223). The map is inherited by forked workers via
   COW and is UNUSED by admission in S1.
2. **Files:** `internal/pylib/aitest/__init__.py` (registration before the early return; the
   `AIRA_AITEST_DEFAULT_BYTES` resolver reusing `_parse_estimated_bytes`; `str()`/arity handling);
   `internal/pylib/aitest/supervisor.py:~818` (build the `nodeid → bytes` map beside
   `items_by_nodeid`).
3. **Tests:** `aira_mem("512M"/"1.5G"/"512MiB")` and a **bare-int** arg all parse correctly;
   `aira_mem()` no-arg and 2-arg → warn-once + default; unannotated → default; invalid string
   (`"4GB"`) → warn-once + default (assert the warning fires exactly once); `--strict-markers` suite
   green under plain `pytest` (proves registration precedes the early return); `get_closest_marker`
   closest-wins precedence (class vs function). **Mutation:** dropping the parse-and-store line (every
   nodeid silently → default) reds a map-content assertion on an annotated nodeid.
4. **Deps:** v7-1 (ordered after, so the correctness fix ships first; functionally independent).
5. **Parallel:** may run disjoint from v7-1's regions; keep serial for review clarity.

### v7-4 — measurement harness (reuse existing reads; no new sampler)

1. **Goal:** MEASURE, on AIRA's own suite (dogfood, spec §8/§11), the numbers that fix v0.7's
   tunables, **reusing the reads the code already has** (Fable OVER-BUILD: do not build a time-series
   sampler): per-worker `memory.peak` + the oom flag are already read at retirement
   (`supervisor.py:1734-1740`); `_should_recycle` already reads `memory.current` per test
   (`worker.py:285`); ADD one read of `.aira-supervisor/memory.peak` at run end (for the
   `allowance` `base` + `per_relay` terms at FULL pool). Emit a structured report and commit an
   EXECUTABLE reproduction (CLAUDE.md: a published measurement needs a committed, executable repro).
   The report fixes: the 256 MiB default (v7-2), the per-worker headroom + the size-class ladder
   shape (S2), the `outer_cap_allowance` `base`/`per_relay` and `outer_cap_margin` (v7-1), the
   watermark fraction, and the MAX_TESTS fork (OD4). For the short-window (~10 s) residue data the
   MAX_TESTS-vs-watermark fork needs, the harness sets `AIRA_AITEST_WORKER_MAX_SECONDS=10` for THAT
   sub-run ONLY — it does NOT change the default (D3); the residue-over-hundreds-of-tests run keeps
   the 600 s / 200-test window.
2. **Files:** `internal/pylib/aitest/{supervisor,worker}.py` (surface the already-read
   `memory.peak`/oom/`memory.current` values + the one new `.aira-supervisor/memory.peak` read into
   a report channel); a committed harness script under the repo's measurement convention; a short
   measurement report doc.
3. **Tests:** the harness runs under `aira confine -- ` and produces the report; a smoke assertion
   that the sampler reports a real value where the kernel exposes `memory.peak`/`memory.events` and
   `unevaluated` (never a fake 0) where it does not (the AIRA honesty rule).
4. **Deps:** v7-1 only (the reader/ladder are not measurement inputs — the harness measures the flat
   model's ground truth to SET S2's numbers).
5. **Parallel:** may follow v7-1 independently of v7-2.

---

## §11 REFINEMENT NOTE (this plan vs the spec's coarse staging)

The spec §11 bundled the ~10 s age-cap flip, "size-class worker sizing", and the ladder into S1.
This plan refines that (advisor + Fable-gate confirmed):
- **the size-class ladder, per-class spawn sizing, and per-class dispatch all move to S2** — the
  ladder's shape depends on v7-4's numbers, and sizing without dispatch is an OOM (D1/D5).
- **the ~10 s age-cap flip moves to S2** (D3 — no fairness benefit pre-S2; truncates v7-4's residue
  measurement; ~60× scope churn).
- **the `Σ(pending)` guard term + its mutation guard are OWED BY S2** (D2 — VERIFIED inert in S1's
  single-threaded ≤ 1-pending model).
- **spec-§7 amendment:** the effective-outer-cap is read at every guard check, not once at startup
  (v7-1 P2).

## Self-review (against the spec)

- §7 item 1 / OD3 aggregate Σ-guard, effective-outer-cap min-over-ancestry, mode-dependence, the
  terminal-vs-skip disposition, Σ-at-check-time, allowance = base+N×relay → v7-1 (Tier-1 collapsed
  into the one rule; client-side slice-ceiling replica dropped as daemon-covered; Σ(pending) → S2). ✓
- §4.1 `aira_mem` reader, registration (pre-early-return), grammar (+ bare-int `str()`), default,
  arity/invalid warn-once, incremental semantics → v7-2 (inert foundation, D5). ✓
- §8 accepted gaps: mid-test accumulation OOM + the supervisor-allowance-bounded-not-airtight term
  (now base+N×relay) → MEASURED in v7-4; documented in v7-1. ✓
- OD4 tunables (256 MiB, headroom, watermark, allowance, margin, MAX_TESTS fork) → starting
  constants + MEASURED in v7-4; not fixed as load-bearing. ✓
- Stale as-built comments (`worker_scope_linux.go:52`, `worker.py:369-375`, `supervisor.py:
  2457-2463`) → v7-1. ✓
- §4.2 size-class sizing + §7 item 2 per-class pool-usage sample → S2/S3 (explicitly deferred). ✓

## COVERAGE GAPS (accepted, recorded — not silent)

- The load-bearing over-admission test cannot run in ci-shim CI; it MUST be a real-cgroup
  branch-exit gate under `aira confine` (do not read CI-green as coverage).
- Mixed pool after a mid-run daemon loss (confined + fallback workers, `grant is None`): fallback
  worker RSS is unguarded by the aggregate guard — pre-existing behaviour, out of S1 scope, accepted.
- The `aira_mem` map has no S1 consumer (D5) — landed as a documented-inert foundation primitive.
- `Σ(pending)` guard term + per-class sizing/dispatch + ladder + ~10 s age-cap flip → all S2.
