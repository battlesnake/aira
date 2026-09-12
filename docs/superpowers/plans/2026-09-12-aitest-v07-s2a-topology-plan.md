# aitest v0.7 S2a — sibling worker scopes + `--delegate-ram` collapse + v7-1 excision — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Place aitest worker cgroup scopes as siblings directly under `aira.slice` (not nested under the outer confine scope), collapse `--delegate-ram` to an ordinary confine job, and excise the now-harmful v7-1 client-side outer-cap guard — closing AIRA-229 and AIRA-232 by construction.

**Architecture:** The daemon ledger is already slice-authoritative (each worker is a separate signed lease against the one slice, released on socket-EOF). The only change is the kernel cgroup topology: create each worker via the *ordinary* confine scope-creation path under the slice, move the parent↔worker linkage from cgroup-path to an explicit identifier, and stop giving `--delegate-ram` a bespoke whole-subtree envelope. Because there is no longer a shared smaller-than-slice parent cap, the outer `oom.group` can no longer whole-suite-kill (AIRA-229), N supervisors cannot jointly breach (AIRA-232), and the client-side guard that existed only to prevent that breach must be removed (it would otherwise read the now-small parent cap and silently throttle the pool).

**Tech Stack:** Go (no cgo; static binary), cgroup v2, Python (pytest plugin: `internal/pylib/aitest/`). Real-cgroup e2e tests run under `aira confine`.

**Spec:** [`docs/superpowers/specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md`](../specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md) — §4 (topology / delegate collapse), §10 (guard excision), §11 (229/232 dissolve), §14 (S1 asterisk). The S1 spec's `aira_mem` grammar is unchanged.

## Global Constraints

- **No cgo; one static Go binary.** (spec §2)
- **`ci-shim` mode (no cgroup) must degrade honestly** — no scope, no `memory.max`, no kill backstop; the topology change is a real-cgroup concern and must be a no-op in shim mode. (Q1/Q2 caveats: shim publishes coordinates but creates no sub-scope.)
- **A check that cannot establish its result reports `unevaluated`, never a fake pass/zero.** (project rule)
- **No backwards-compat obligation** — schema/protocol/config may change freely; **no daemon protocol bump in S2a** (the ledger and worker-admit wire are unchanged here; the 11→12 try-acquire bump is S2b). (spec §2, §6)
- **Two-loop mandatory** (topology + ledger are correctness-critical): Opus builds, Fable gates + build-reviews, every behavioural claim mutation-verified, real-cgroup e2e for anything the shim can't exercise.
- **Heavy commands under `aira confine -- `**; never `systemctl --user stop aira.slice`; kill your own `aira confine` PID. `~/tmp` for scratch. Push feature branches with `--no-verify` only after confirming the change can't break `make test` locally (RANT-44 flaky hook); GitHub CI is the gate.
- **Line numbers below are as read at master `61745b9` (fact-find `wf_9a9f12da-c13`). They shift as edits land — re-grep the named symbol before each edit; trust the symbol, not the line.**

## File Structure

Created:
- `internal/pylib/pytest_aitest_topology_e2e_test.go` — real-cgroup e2e gates proving sibling placement + no-whole-suite-kill + pool-scales-past-3 + multi-supervisor safety.

Modified (Go):
- `internal/runner/worker_scope_linux.go` — `CreateWorkerScope` roots the worker under the slice, not `outerScope`. Fix the stale doc-comment (:52-60) that cites the deleted daemon scan + the client guard.
- `internal/runner/worker_scope.go` — `WorkerScopeChildPath` (if it must change to slice-relative).
- `internal/daemon/worker_admit.go` — `create(...)` call (:599) passes the slice as parent; `workerParentScopeID` (:230) derives the parent from the explicit id, not the outer dir basename; the exclusivity exemption (:85-91) reads that id.
- `internal/runner/confine.go` — `ResolveConfineReserve` (:121): delete the `DelegateRAM` branches so a delegate job takes the ordinary reserve; remove `DefaultDelegateRAMOverhead` / `DefaultDelegateRAMScopeCeiling` if fully unused.
- `internal/runner/confine_linux.go` — delegate `memory.max = scopeCeiling` branch (:985-1002) → ordinary reserve; keep the aitest-coordinate publication (:1116-1140).
- `internal/daemon/admit.go` — delete `resolveDelegateRAMScopeCeiling` (:1478-1511) and its call; a delegate job resolves the ordinary reserve/ceiling.
- `internal/runner/oomsteer.go` — delete the `@dr` oom-class-800 selection (:26-31, :67-70); delegate scopes get the ordinary `oom_score_adj = 500`.
- `internal/runner/admission_linux.go` — delegate declares 1 core like an ordinary confine job (:389-392), not 0. (Re-check: does any downstream assume delegate==0 cores? `redeclare.go:32` legalises 0 across restart — verify removal is safe.)
- `internal/runner/aitest_bootstrap_linux.go` — remove the `.aira-supervisor` drain + controller delegation (:37-113); the supervisor runs directly in the parent scope.
- `internal/pylib/env.go` — `AIRA_AITEST_OUTER_SCOPE` becomes the *parent* scope id for linkage, not a cgroup to nest under (or add `AIRA_CONFINE_SCOPE_ID`); wire the explicit `parent_scope_id` to worker-admit.

Modified (Python):
- `internal/pylib/aitest/worker.py` — `place_self` (:453-459) enters the daemon-returned *sibling* scope (no behavioural change if the daemon returns the new path; verify).
- `internal/pylib/aitest/supervisor.py` — **excise the v7-1 guard** per the Q7 inventory (constants :144-166, `WorkerAdmitOuterCapExceeded` :454-468, `__init__` attrs :810-818, methods :1459-1620, chokepoint call :1652); pass `parent_scope_id` to worker-admit; drop the 3 `_emit_measurement_report` guard keys (:2178-2180).

Deleted:
- `internal/pylib/aitest/test_outer_cap_guard.py` (whole file) — **but first relocate** `test_env_bytes_accepts_bare_zero_and_warns_on_garbage` (:176-189) to a surviving test file (`_env_bytes` stays, used at supervisor.py:919).
- `internal/pylib/pytest_aitest_e2e_test.go` — delete `TestRealPytestAitestOuterCapGuard{Terminal,SkipTick}` (:562-696) and the orphaned `readOuterMemoryEventCounter` (:535-559).

Doc touch (no runtime dep): stale AIRA-229/guard references in `worker_scope_linux.go:52-60`, `supervisor.py:2838-2839`, the measurement test comments, and the ticket/plan prose.

---

## Task 1: Pin current behaviour with a failing sibling-placement e2e

**Files:**
- Create: `internal/pylib/pytest_aitest_topology_e2e_test.go`
- Reference: `internal/pylib/pytest_aitest_e2e_test.go` (`newRealDaemonAndCgroupTestHarness` :262, the `@dr` scope-id shape, `readOuterMemoryEventCounter` pattern)

**Interfaces:**
- Consumes: `newRealDaemonAndCgroupTestHarness` (shared harness).
- Produces: `TestRealPytestAitestWorkerScopesAreSiblings` — asserts, after a `--delegate-ram` run, that each live worker scope directory is a child of `aira.slice`, **not** a child of the outer `.aira-CONFINE-@dr-*` scope.

- [ ] **Step 1: Write the failing test.** Launch a short `--delegate-ram` pytest run under a real cgroup via the harness; while ≥1 worker is live, walk the cgroup tree and assert the worker scope's parent dir is the slice, not the outer scope. Expected at HEAD: FAIL (workers are currently `<outer>/.aira-worker-N`).
- [ ] **Step 2: Run it, confirm it fails** for the right reason (parent is the outer scope). Record exact output.
- [ ] **Step 3:** (no implementation yet — this test stays red until Task 3.) Commit the test alone, marked `t.Skip("red until S2a Task 3 — documents target topology")` flipped off, or keep it failing in a dedicated commit noted as the red anchor.

> Rationale: this is the inverse-mutation anchor — it must be RED against the current nested topology and GREEN only once Task 3 lands, proving the topology change is real and not porous.

## Task 2: Thread an explicit `parent_scope_id` (decouple linkage from cgroup path)

**Files:**
- Modify: `internal/daemon/worker_admit.go` (`workerParentScopeID` :230-231; exclusivity exemption :85-91), `internal/pylib/env.go`, `internal/pylib/aitest/supervisor.py` (worker-admit arg assembly ~:953-954), `cmd/aira/main.go` (aitest-bootstrap env read ~:1891)
- Test: `internal/daemon/worker_admit_test.go` (parent-id derivation)

**Interfaces:**
- Produces: worker-admit receives the parent scope-id as an explicit field/arg (`parent_scope_id`), not derived by stripping `.aira-` off the outer directory basename.

- [ ] **Step 1: Write the failing unit test** asserting `workerParentScopeID` returns the supervisor's confine scope-id from the explicit field even when the worker scope path is slice-relative (a sibling path no longer encodes the parent). Expected: FAIL (current code derives from the outer dir basename).
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — add `parent_scope_id` to the worker-admit request; the supervisor passes its own confine scope-id (already known: `self.outer_scope`'s id); `workerParentScopeID` reads the field; the exclusivity exemption uses it. Keep back-compat path deletable (no users).
- [ ] **Step 4: Run, confirm pass.** Re-run the existing worker-admit + exclusivity tests — no regression.
- [ ] **Step 5: Commit.**

## Task 3: Create worker scopes as siblings under the slice

**Files:**
- Modify: `internal/runner/worker_scope_linux.go` (`CreateWorkerScope` :63-108), `internal/runner/worker_scope.go` (`WorkerScopeChildPath` :16-20 if needed), `internal/daemon/worker_admit.go` (`create(...)` call :599 — pass the slice as parent), `internal/pylib/aitest/worker.py` (`place_self` :453-459 — verify it enters the returned path unchanged)
- Test: Task 1's e2e flips GREEN; add a unit test on the computed scope path.

**Interfaces:**
- Consumes: the resolved slice cgroup path (the daemon already resolves `DefaultConfineSlice` in worker-admit :429-433 — reuse it).
- Produces: `CreateWorkerScope` roots the worker at `<slice>/.aira-CONFINE-<worker-id>` via the **ordinary confine scope-creation path** (so the orphan reaper globbing `.aira-CONFINE-*` and `confine --list` cover it by construction — spec §4; do not invent a new name the reaper can't see).

- [ ] **Step 1:** Write a unit test asserting the worker scope path is slice-rooted and uses the `.aira-CONFINE-*` naming the reaper recognises. Expected: FAIL.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — pass the slice (not `req.outerScope`) as the parent to scope creation; reuse the ordinary confine scope path so controller delegation + common-ancestor `cgroup.procs` migration (the permission model `place_self` needs) come for free. The daemon returns the new path to the supervisor, which hands it to the worker's `place_self` unchanged.
- [ ] **Step 4: Run, confirm pass** — the new unit test AND Task 1's e2e (`...WorkerScopesAreSiblings`) both GREEN. Re-run the full `internal/pylib/aitest` suite + the existing real-cgroup e2e gates.
- [ ] **Step 5: Commit.**

> After this task, worker RAM no longer charges up to the outer scope. The next tasks remove the now-vestigial delegate envelope and the guard.

## Task 4: Excise the v7-1 client-side outer-cap guard

**Files:**
- Modify: `internal/pylib/aitest/supervisor.py` (Q7 inventory: constants :144-166, `WorkerAdmitOuterCapExceeded` :454-468, `__init__` attrs :810-818, methods `_read_cgroup_memory_max`/`_effective_outer_cap`/`_sum_live_worker_caps`/`_would_breach_outer_cap` :1459-1620, chokepoint call :1652, docstring :1625-1637; measurement keys :2178-2180; stale docstring :2838-2839)
- Relocate: `test_env_bytes_accepts_bare_zero_and_warns_on_garbage` (test_outer_cap_guard.py:176-189) → `test_aira_mem_marker.py` or a new `test_env_bytes.py` (**`_env_bytes` survives**, used at supervisor.py:919)
- Delete: `internal/pylib/aitest/test_outer_cap_guard.py` (after relocation), `TestRealPytestAitestOuterCapGuard{Terminal,SkipTick}` + orphaned `readOuterMemoryEventCounter` in `pytest_aitest_e2e_test.go`
- Modify doc comment: `internal/runner/worker_scope_linux.go:52-60`

**Interfaces:**
- Removes one source of `WorkerAdmitTerminal`/`WorkerAdmitDenied`; the **shared** catch sites (`_fail_queue_terminal`, `_try_grow_one`, `run()`, `_replace_worker`, `_wait_for_admission_or_disable`) are unchanged because other terminal subclasses remain.

- [ ] **Step 1: Write/adjust the regression test that would red if the guard still throttled the pool** — a test (unit or e2e) asserting the pool scales to N>3 workers under a small parent cap with per-worker 256 MiB requests (the advisor's skip-tick-to-3 scenario). Expected at HEAD (guard present + small parent cap after Task 3): FAIL (pool caps at ~3).
- [ ] **Step 2: Run, confirm fail** for the right reason (guard skip-ticks).
- [ ] **Step 3: Relocate** the `_env_bytes` test; run it green in its new home.
- [ ] **Step 4: Excise** the guard per the inventory (top-down to avoid line drift); drop the 3 measurement keys; fix the stale doc comments.
- [ ] **Step 5: Run** — the pool-scales test GREEN; the full `internal/pylib/aitest` suite GREEN; a `AIRA_AITEST_MEASURE_DIR` run does **not** `NameError` (verify the measurement report still emits). Delete the guard test file + the two e2e gates; confirm `go build`/`go vet` clean (no orphaned symbols).
- [ ] **Step 6: Commit.**

> Task 3 + Task 4 are the coupled correctness pair (spec §10): the guard must not survive the topology change. If splitting commits, land Task 4 in the same PR as Task 3 — never merge a state where workers are siblings but the guard is still live.

## Task 5: Collapse `--delegate-ram` to an ordinary confine job

**Files:**
- Modify: `internal/runner/confine.go` (`ResolveConfineReserve` :121-137 — delete the `DelegateRAM` branches; remove `DefaultDelegateRAMOverhead`/`DefaultDelegateRAMScopeCeiling` constants :36-39 if unused), `internal/runner/confine_linux.go` (delegate `memory.max = scopeCeiling` :985-1002 → ordinary reserve), `internal/daemon/admit.go` (delete `resolveDelegateRAMScopeCeiling` :1478-1511 + its call + the `delegateRAMScope*` consts :41-42), `internal/runner/oomsteer.go` (delete `@dr` class-800 :26-31,:67-70), `internal/runner/admission_linux.go` (delegate declares 1 core :389-392), `internal/runner/aitest_bootstrap_linux.go` (remove `.aira-supervisor` drain + controller delegation :37-113)
- Test: `internal/runner/confine_admit_test.go` (delegate reserve/cap expectations) + a real-cgroup e2e.

**Interfaces:**
- A `--delegate-ram` job now resolves the **ordinary** reserve (history-estimated p90, `--memory-reserve`/`--memory-max` override, modest default), `memory.max = reserve`, `oom_score_adj = 500`, 1 declared core — differing from an ordinary confine job ONLY by (a) publishing aitest coordinates and (b) its workers being placed as siblings.

- [ ] **Step 1: Write the failing test** — a `--delegate-ram` admit expects ordinary-reserve sizing (not the 512 MiB pin / 48 GiB envelope) and `oom_score_adj = 500`. Expected: FAIL.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — delete the delegate-specific reserve/ceiling/oom-class/core branches; keep the aitest-coordinate publication (`AppendAitestChildEnvironment`, confine_linux.go:1116-1140); remove the supervisor drain (the supervisor now runs directly in the parent scope — verify it can still spawn workers, which are siblings, so the cgroup-v2 "no processes + controller-children" constraint no longer applies).
- [ ] **Step 4: Run, confirm pass** + full suite + e2e. Verify the `redeclare.go:32` 0-core legalisation removal is safe (delegate now declares 1 core).
- [ ] **Step 5: Commit.**

> Accepted first-run cost (spec §4): a signature's old whole-subtree peak history over-reserves the parent once post-cutover, then self-corrects. No signature marker; documented.

## Task 6: AIRA-229 / AIRA-232 real-cgroup closure gates

**Files:**
- Modify: `internal/pylib/pytest_aitest_topology_e2e_test.go`
- Reference: the deleted `readOuterMemoryEventCounter` pattern (re-add locally if needed to read slice/worker `memory.events`).

**Interfaces:**
- Produces two gates, each a real-cgroup e2e under `aira confine`:
  - `TestRealPytestAitestNoWholeSuiteKillOnAggregate` (AIRA-229): a workload whose aggregate worker RAM would have tripped the old outer cap now runs to completion — no `oom_group_kill` on a parent scope, every test yields a real result (not `unevaluated`).
  - `TestRealPytestAitestMultiSupervisorSafe` (AIRA-232): two supervisors under one parent, combined demand > any single parent cap, all workers admitted/serialised by the daemon against the slice with no whole-suite kill.

- [ ] **Step 1: Write both gates.** (They can only pass post-Task-3/5.)
- [ ] **Step 2: Run** under `aira confine -- `; confirm GREEN. Add a false-pass trap (e.g. assert the test count actually ran, per the S1 porous-test lesson — a gate that passes when zero workers spawn is porous).
- [ ] **Step 3: Mutation-check** — temporarily restore nesting (Task 3 mutation) and confirm `...NoWholeSuiteKill...` reds; restore the guard (Task 4 mutation) and confirm the pool-scales test reds.
- [ ] **Step 4: Commit.**

## Task 7: Tidy + ticket closure notes

- [ ] **Step 1:** Update stale prose (AIRA-229 refs in `worker_scope_linux.go`, `supervisor.py:2838-2839`, measurement test comments).
- [ ] **Step 2:** `aira transition` AIRA-229 and AIRA-232 to done with a note pointing to the S2 spec §11 + this slice (dissolved by topology, not a guard). Use `aira link` for any relation, never hand-write tuples.
- [ ] **Step 3:** Full `internal/pylib/...` + `internal/runner/...` + `internal/daemon/...` build/vet/test under `aira confine`; record exact exit codes. PR.

---

## Self-Review

- **Spec coverage:** §4 (sibling scopes, delegate collapse, linkage-via-env, drain removal) → Tasks 2,3,5. §10 (guard excision, measurement keys, test relocation) → Task 4. §11 (229/232 dissolve) → Task 6. §14 (S1 asterisk) → Task 4. Not in S2a (correctly deferred): batch/prune/age-cap (S2b), try-acquire protocol bump (S2b), warning (S2c), OOM phantom + release verb (S2d), tunable measurement (S2e).
- **Ordering risk:** Task 4 (guard) MUST ship with Task 3 (topology) — never a merged state with siblings + live guard. Stated in Task 4 note.
- **Porous-test risk:** Task 1 and Task 6 carry explicit false-pass traps + mutation checks (the S1 lesson: a gate that can't fail against the wrong impl proves nothing).
- **ci-shim:** every topology assertion is real-cgroup-gated; shim mode must be a no-op (no sub-scope) — verify in Task 3/5 that shim still publishes coordinates and admits ledger-only.
- **Line-number drift:** flagged globally; re-grep symbols before editing.

## Execution Handoff

Plan complete. Recommended: **Subagent-Driven** (fresh Opus builder per task-group + Fable build-review), per the two-loop for correctness-critical work. Build order: Task 1 → 2 → 3+4 (coupled) → 5 → 6 → 7.
