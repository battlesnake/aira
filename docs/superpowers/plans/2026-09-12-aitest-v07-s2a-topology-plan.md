# aitest v0.7 S2a — sibling worker scopes + `--delegate-ram` collapse + v7-1 excision — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

> **GATE-1 REDRAFT (2026-09-12).** The Fable plan-gate affirmed the architecture but found nesting was silently doing four jobs (unique naming, kill propagation, escape attestation, sub-reservation marking) the first draft replaced none of, two gates that would pass against a broken impl, an incomplete excision inventory, and a strong simplification. This redraft folds all of it. See spec §16.

**Goal:** Make each aitest worker a **first-class sibling confine scope** directly under `aira.slice`; collapse `--delegate-ram` to an ordinary confine job (incl. deleting the `aitest-bootstrap` subprocess); and excise the now-harmful v7-1 guard — closing AIRA-229 and AIRA-232 by construction, with no regression to kill propagation, scope-integrity attestation, or job accounting.

**Architecture:** The daemon ledger is already slice-authoritative (each worker is a separate signed lease against the one slice, released on socket-EOF). The change is the kernel cgroup topology: each worker scope is created under the slice via the ordinary confine scope-creation path, with a unique parseable pid-bearing confine name; linkage moves from cgroup-path to an explicit `parent_scope_id`; the daemon owns worker-scope kill+rmdir on relay peer-EOF; escape-attestation exempts the worker's fork→`place_self` migration into its own sibling scope. `--delegate-ram` becomes an ordinary confine job that only additionally publishes aitest coordinates. Because there is no shared smaller-than-slice parent cap, the outer `oom.group` cannot whole-suite-kill (AIRA-229) and N supervisors cannot jointly breach (AIRA-232); the v7-1 guard (which read the parent cap) must be removed in the same change.

**Tech Stack:** Go (no cgo; static binary), cgroup v2, Python (`internal/pylib/aitest/`). Real-cgroup e2e under `aira confine`, pointed at an isolated harness slice (never the production slice).

**Spec:** [`../specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md`](../specs/2026-09-12-aitest-v07-s2-daemon-authoritative-design.md) — §3, §4, §10, §11, and **§16 (GATE-1 refinements — the load-bearing detail for this plan)**.

## Global Constraints

- **No cgo; one static Go binary.**
- **`ci-shim` mode (no cgroup) is a topology no-op** — it publishes aitest coordinates + a `parent_scope_id` *sentinel* and admits ledger-only; no scope, no `memory.max`, no kill, no attestation. Verify shim is unbroken in every task.
- **`unevaluated`, never fake pass/zero** for any read that can't establish its result.
- **No backwards-compat obligation.** **No daemon protocol bump in S2a** (ledger + worker-admit wire unchanged except the added `parent_scope_id` field, which is not a version-gated frame — the 11→12 try-acquire bump is S2b). `redeclare.go` is the FROZEN codec — do NOT edit it; `cpu_cores==0` stays legal by contract there.
- **Two-loop mandatory** (topology + ledger + kill-path are correctness-critical): Opus builds, Fable build-reviews, every behavioural claim mutation-verified, real-cgroup e2e for anything shim can't exercise.
- **Heavy commands under `aira confine -- `**; kill your own `aira confine` PID, never `systemctl --user stop aira.slice`; `~/tmp` scratch; push `--no-verify` only after confirming no `make test` breakage (RANT-44), GitHub CI is the gate.
- **This is a large but ATOMIC slice** — a half-done topology change is broken (siblings without kill-propagation orphan workers; siblings with a live guard throttle the pool). Checkpoint-commit per task, but land as ONE PR; never merge a partial state.
- **Line numbers are as read at `61745b9` (gate re-grep). They drift — re-grep the named symbol before each edit.**

## File Structure

Created:
- `internal/pylib/pytest_aitest_topology_e2e_test.go` — real-cgroup gates (sibling placement via pool-report anchor; no-whole-suite-kill with a real alloc-and-hold fixture; pool-scales-past-3; multi-supervisor; parent-kill leaves no orphan; scope-integrity=contained).
- `internal/pylib/aitest/testdata/test_alloc_hold.py` — a fixture that allocates and HOLDS a marked amount (for the 229 aggregate mutation; the existing fixtures allocate nothing / self-OOM only).
- (maybe) `internal/pylib/aitest/test_env_bytes.py` — relocated `_env_bytes` unit test.

Modified (Go) — grouped by the job each serves (spec §16):
- **Naming/allocator (§16a):** `internal/daemon/worker_admit.go` (`workerScopeFor`/`allocateWorkerScopeID` :117-214, EEXIST reseed :602-611, lease `scopeID` set); `internal/runner/worker_scope_linux.go` / `worker_scope.go` (name + slice-rooted path); confirm `parseConfineScopeID` accepts the new grammar.
- **parent_scope_id / sub-reservation (§16d):** `worker_admit.go` (`workerParentScopeID` :225-232, `isSubReservation` admit.go:328-330, refuse-empty), `internal/pylib/env.go` (`AIRA_CONFINE_SCOPE_ID` :196; new `AIRA_AITEST_ADMISSION`), `internal/pylib/aitest/supervisor.py` (send the id), `internal/runner/confine_shim_linux.go` (publish a sentinel :300-304).
- **Placement (§4):** `worker_admit.go` `create(...)` :599 (parent = slice), `worker_scope_linux.go` :63-108, `worker.py` `place_self` :453-459 (verify unchanged behaviour on the returned path).
- **Kill propagation (§16b):** `worker_admit.go` relay lifecycle :538-548/:630-636 — on peer-EOF `cgroup.kill`+rmdir the worker scope; guard on `peerCtx.Done()`, NOT `s.stopping`. `confine_manage_linux.go` helpers for kill+rmdir.
- **Escape attestation (§16c):** `internal/runner/runner_linux.go` `monitorScopeMembership`/`witnessedEscape` :1627-1832 — exempt migration into a live-leased sibling worker scope whose `parent_scope_id`==this scope.
- **Delegate collapse + full inventory (§16 decision-log, P1-5):** `confine.go` (`ResolveConfineReserve` :121-137; `DefaultDelegateRAMOverhead`/`DefaultDelegateRAMScopeCeiling` :36-39; `delegateRAMScopeFallback`+`AIRA_DELEGATE_RAM_SCOPE_DEFAULT` confine_linux.go:1970-1976; `ConfineCapSourceDelegateRAM` :465-471 + render :1691 + `store/resource_budget.go:248-357`; `ContainerReserveSkipDelegateRAM`+`ResolveReserve` delegateRAM param container.go:75-78,481 + confine_shim_linux.go:84,220; `lease_keeper_linux.go:100`), `confine_linux.go` (delegate `memory.max` :985-1002), `admit.go` (`resolveDelegateRAMScopeCeiling` :1478-1511 + `delegateRAMScope{Min,Safety,OOMEscalation}` :41-42 + `sliceceiling.go:29,732`), `oomsteer.go` (`ConfineDelegateOOMScoreAdj` :26-31,:67-70,:78-84 + env; decide the `@dr` marker `IsDelegateRAMScopeID`/`bindConfineScopeID:1288`), `admission_linux.go` (1 core :389-392; `delegate_ram` wire arg :404 → daemon `request.delegateRAM`/`scopeCeiling`/`AdmitResponse.ScopeCeiling`), `internal/core/skill.go:318` (prose), and the parent-signature namespacing (P2-2).
- **Bootstrap deletion (§16 simplification, P2-6):** delete `aitest-bootstrap` verb (`cmd/aira/main.go` ~:1862-1925), `BootstrapAitestSupervisor`/`drainIntoScope`/`moveIntoScope`/`scopeHasFiniteMemoryMax` (`aitest_bootstrap_linux.go`), supervisor.py `bootstrap()` subprocess; launcher publishes env directly.
- **Fallback cap (P2-3):** `worker.py`/`supervisor.py` `_spawn_fallback_worker` + `AIRA_AITEST_MAX_WORKERS_FALLBACK` env.go:84 — cap at 1 under a finite parent cap.
- **Measurement (P2-5):** `supervisor.py` `_emit_measurement_report` :2145-2156 → parent `memory.peak`; drop `supervisor_scope=` + `_cleanup_supervisor_scope`.
- **Guard excision (§10):** `supervisor.py` per the Q7 inventory (below).

Deleted: `internal/pylib/aitest/test_outer_cap_guard.py` (after relocating the `_env_bytes` test :176-189); `TestRealPytestAitestOuterCapGuard{Terminal,SkipTick}` in `pytest_aitest_e2e_test.go` :562-696 (keep/rename `readOuterMemoryEventCounter` :535-559 for the new gates rather than delete-then-re-add; `testdata/test_slow_passing.py` is orphaned by the SkipTick deletion — re-home or drop).

---

## Task 1: First-class worker confine scope names + slice-keyed allocator (P0-1)

**Files:** `internal/daemon/worker_admit.go` (`workerScopeFor`/`allocateWorkerScopeID` :117-214, EEXIST reseed :602-611), `internal/runner/worker_scope_linux.go`/`worker_scope.go`; Test: `internal/daemon/worker_admit_test.go`.

**Interfaces:** Produces `CONFINE-aitest-w<seq>-<supervisorPID>-<stamp>` worker ids, minted through the confine grammar, counter keyed on the **slice** (or globally unique by construction); the granted lease's `scopeID` == scope dir-name-minus-`.aira-`.

- [ ] **Step 1: Failing unit test** — two distinct outer/supervisor contexts admitting workers under the one slice produce **distinct, `parseConfineScopeID`-parseable** ids with no `EEXIST`/`WorkerScopeIDCollision`. Expected: FAIL (current per-outer `worker-N` collides + is unparseable).
- [ ] **Step 2: Run, confirm fail** (collision or parse-reject).
- [ ] **Step 3: Implement** — new name grammar + slice-keyed (or uniqueness-by-construction) counter; reseed scans the slice for the new prefix; set lease `scopeID` so the reaper `hasLiveLease` veto (confine_reaper.go:55-61) and `oomsteer` `confineScopeDirName` (oomsteer.go:232) align.
- [ ] **Step 4: Run, confirm pass**; re-run worker-admit + reaper tests (no regression).
- [ ] **Step 5: Commit.**

## Task 2: Explicit `parent_scope_id`; preserve sub-reservation marking (P1-3, §16d)

**Files:** `worker_admit.go` (`workerParentScopeID` :225-232, `isSubReservation` admit.go:328-330), `internal/pylib/env.go` (`AIRA_CONFINE_SCOPE_ID` :196), `supervisor.py` (~:953-954), `confine_shim_linux.go` (sentinel :300-304); Test: `worker_admit_test.go`.

**Interfaces:** Worker-admit carries `parent_scope_id` as an explicit field (the supervisor's confine scope **id**, from `AIRA_CONFINE_SCOPE_ID` — not `self.outer_scope`, which is a path). Empty is refused.

- [ ] **Step 1: Failing tests** — (a) `workerParentScopeID` returns the explicit id under sibling placement; (b) an **empty** `parent_scope_id` is refused `E_DAEMON_PROTOCOL` (so a worker can never silently become a job); (c) `isSubReservation` still holds for a worker. Expected: FAIL.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — add the field; supervisor sends the id; ci-shim publishes a sentinel; daemon refuses empty; the exclusivity exemption + `isSubReservation` read the field.
- [ ] **Step 4: Run, confirm pass**; re-run the AIRA-68 "workers aren't jobs" invariants (`outstandingJobs`, `sliceProvablyEmpty`, drain/`--exclusive`, `confine --list` N-jobs).
- [ ] **Step 5: Commit.**

## Task 3: Create worker scopes as siblings under the slice (§4)

**Files:** `worker_admit.go` `create(...)` :599 (parent = slice, reuse the resolved `DefaultConfineSlice` :429-433), `worker_scope_linux.go` :63-108, `worker.py` `place_self` :453-459; Test: the Task 9 `...WorkerScopesAreSiblings` gate anchor + a unit test on the path.

- [ ] **Step 1: Failing unit test** — the computed worker scope path is slice-rooted with the Task-1 name. Expected: FAIL.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — pass the slice (not `req.outerScope`) as parent; reuse the ordinary confine scope-creation path so controller delegation + common-ancestor `cgroup.procs` migration (the permission model `place_self` needs — the slice already carries `+memory/+cpu` via `ensureConfineDelegation`) come for free; daemon returns the new path to the supervisor → worker `place_self` unchanged.
- [ ] **Step 4: Run, confirm pass** + full `internal/pylib/aitest` suite + existing real-cgroup e2e. Worker RAM no longer charges the outer scope.
- [ ] **Step 5: Commit.**

## Task 4: Daemon kills + rmdirs worker scopes on relay peer-EOF (P1-1, §16b)

**Files:** `worker_admit.go` relay lifecycle :538-548/:630-636, `confine_manage_linux.go` (`cgroup.kill`+rmdir helper :609-624); Test: a real-cgroup gate in Task 9 + a daemon unit test for the `stopping` guard.

**Interfaces:** On a worker relay's peer-EOF (`peerCtx.Done()`), the daemon `cgroup.kill`s then rmdirs that worker scope. Skipped when `s.stopping` (restart must not kill live workers — they re-declare, :85-91).

- [ ] **Step 1: Failing test** — (unit) on peer-EOF the worker scope is killed+removed; on `s.stopping` it is NOT. (e2e, in Task 9) kill the parent → no worker process survives, no worker dir remains. Expected: FAIL (today relay-EOF frees the lease but leaves the process + dir).
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** the peer-EOF kill+rmdir; guard on `peerCtx.Done()` vs `s.stopping`.
- [ ] **Step 4: Run, confirm pass**; verify the normal supervisor-driven retirement path (`_forget_worker_scope`) still works and doesn't double-rmdir.
- [ ] **Step 5: Commit.**

## Task 5: Exempt the worker fork→place_self migration from escape attestation (P1-2, §16c)

**Files:** `internal/runner/runner_linux.go` `monitorScopeMembership`/`witnessedEscape` :1627-1832, `confine_linux.go` :1237,1400-1408; Test: Task 9 `...ScopeIntegrityContained` gate + mutation.

**Interfaces:** A process migrating from the parent scope **into a live-leased sibling worker scope whose `parent_scope_id` == this scope** is NOT a witnessed escape (positive id, not a name-prefix guess).

- [ ] **Step 1: Failing gate** (Task 9) — a delegate run attests `scope-integrity=contained`, not `descendant-escaped`. Expected: FAIL (every worker migration is currently witnessed).
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** the exemption keyed on the live lease's `parent_scope_id`.
- [ ] **Step 4: Run, confirm pass**; confirm a genuine escape (a process leaving to an UNleased/ unrelated cgroup) is still witnessed (don't over-exempt).
- [ ] **Step 5: Commit.**

## Task 6: Excise the v7-1 guard (§10) — ships with Task 3

**Files:** `supervisor.py` (Q7 inventory: constants :144-166, `WorkerAdmitOuterCapExceeded` :454-468, `__init__` attrs :810-818, methods :1459-1620, chokepoint :1652, docstrings :1625-1637/:2838-2839, measurement keys :2178-2180); relocate `test_env_bytes_accepts_bare_zero_and_warns_on_garbage` (test_outer_cap_guard.py:176-189, `_env_bytes` survives at :919); delete test_outer_cap_guard.py + the two e2e gates; fix `worker_scope_linux.go:52-60` doc.

- [ ] **Step 1: Failing test** — pool scales to N>3 workers under a small (history-sized, ~300 MiB) parent cap with 256 MiB requests. Expected at HEAD (guard live + small parent after Task 3): FAIL — in fact the *first* spawn hits `Σ_live==0` → `WorkerAdmitOuterCapExceeded` terminal → whole queue `unevaluated` (worse than skip-tick; the gate confirmed this).
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Relocate** the `_env_bytes` test; run green in its new home.
- [ ] **Step 4: Excise** the guard top-down; drop the 3 measurement keys; fix doc comments; delete the guard test file + e2e gates; `go build`/`go vet` clean (no orphaned symbols).
- [ ] **Step 5: Run** — pool-scales test GREEN; full suite GREEN; a `AIRA_AITEST_MEASURE_DIR` run does NOT `NameError`.
- [ ] **Step 6: Commit** (same PR as Task 3 — never a merged siblings+live-guard state).

## Task 7: Collapse `--delegate-ram` to an ordinary confine job + delete the bootstrap subprocess (§16 decision-log + simplification; P1-5, P2-1, P2-2, P2-3, P2-6)

**Files:** the full §16 / File-Structure inventory above.

- [ ] **Step 1: Failing tests** — (a) a `--delegate-ram` admit resolves the **ordinary** reserve (not the 512 MiB pin / 48 GiB envelope) + `oom_score_adj=500` + 1 core; (b) `--delegate-ram --memory-reserve 512M` sets the parent `memory.max=512M` like any confine job (document the retired idiom); (c) a delegate signature uses a **namespaced** history key so a fresh parent-only scope is not refused by stale whole-subtree history (P2-2); (d) daemon-down fallback caps at 1 worker under a finite parent cap (P2-3). Expected: FAIL.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — delete the delegate-specific reserve/ceiling/oom-class/core branches per the inventory (do NOT touch `redeclare.go`); keep `AppendAitestChildEnvironment` coordinate publication; **delete the `aitest-bootstrap` verb + `BootstrapAitestSupervisor`/`drainIntoScope`/`moveIntoScope`/`scopeHasFiniteMemoryMax` + supervisor `bootstrap()`**, publishing `AIRA_AITEST_OUTER_SCOPE`(→`parent_scope_id`) + `AIRA_AITEST_ADMISSION` at launch; namespace the parent signature; cap fallback; update `skill.go:318` + confine help prose.
- [ ] **Step 4: Run, confirm pass** + full suite + shim tests (shim delegate still ordinary-reserve + coordinate+sentinel publish) + a real-cgroup delegate run (supervisor runs directly in the parent scope; workers sibling).
- [ ] **Step 5: Commit.**

## Task 8: Measurement channel → parent memory.peak (P2-5)

**Files:** `supervisor.py` `_emit_measurement_report` :2145-2156, `_cleanup_supervisor_scope`; Test: `test_measurement_report.py` + the measurement e2e.

- [ ] **Step 1: Failing test** — the report's supervisor-peak term reads the parent `memory.peak` (real value), not the removed `supervisor_scope`. Expected: FAIL (reads gone path → `unevaluated`).
- [ ] **Step 2-4:** implement the redirect; drop the `supervisor_scope=` token; run green.
- [ ] **Step 5: Commit.**

## Task 9: Real-cgroup gates — isolated slice, alloc-and-hold, deterministic anchors (P1-4; AIRA-229/232; §16c/b)

**Files:** `internal/pylib/pytest_aitest_topology_e2e_test.go`, `testdata/test_alloc_hold.py`; use the `admitResolveSlice` testing seam (`internal/daemon/testing_seams.go`) to point the harness daemon at the harness parent (`IsolatedScopeParent`, finite `memory.max`) — **never the production slice** (cgrouptest forbids it).

- [ ] **Gate A — `...WorkerScopesAreSiblings`:** assert each granted `scope_path` from `pool-report.json` (emitted per-worker, supervisor.py:2158-2176) is a direct child of the harness slice. Deterministic (no "walk while live" race).
- [ ] **Gate B — `...NoWholeSuiteKillOnAggregate` (AIRA-229):** `test_alloc_hold.py` allocates+holds; parent cap 256 MiB, per-worker 200 MiB, `--aitest-workers=3`; assert `scoped_workers ≥ 2`, **every test `passed`**, parent `oom_group_kill == 0`. False-pass trap: assert the real test count ran (not zero).
- [ ] **Gate C — `...MultiSupervisorSafe` (AIRA-232):** two supervisors under one parent, combined demand > any single cap; all workers serialised by the daemon; no whole-suite kill; distinct parseable names (Task 1).
- [ ] **Gate D — `...ParentKillLeavesNoOrphan` (P1-1):** kill the parent; assert no worker process survives, no worker dir remains.
- [ ] **Gate E — `...ScopeIntegrityContained` (P1-2):** a delegate run attests `scope-integrity=contained`.
- [ ] **Mutation checks:** restore nesting → Gate A/B red; restore the guard → Task-6 pool-scales red; drop the escape exemption → Gate E red; drop the peer-EOF kill → Gate D red. Each must red for the right reason.
- [ ] **Commit.**

## Task 10: `confine --list`/`--kill` naming, tidy, ticket closure (P2-4, P3)

- [ ] **Step 1:** Decide `confine --list`/`--kill` handling of worker rows (pid-bearing names make `--kill <pid>` ambiguous) — label workers with a recognisable component or filter them from the default list; test.
- [ ] **Step 2:** Re-home/keep `readOuterMemoryEventCounter`; re-home/drop `testdata/test_slow_passing.py`; note oomsteer will begin steering worker leases once names parse (harmless — confirm).
- [ ] **Step 3:** `aira transition` AIRA-229 + AIRA-232 → done (note → spec §11/§16 + this slice; `aira link` for relations, never hand-write tuples).
- [ ] **Step 4:** Full `internal/{pylib,runner,daemon}/...` build+vet+test under `aira confine`; exact exit codes recorded. PR (one atomic PR for Tasks 1-10).

---

## Self-Review

- **Spec coverage:** §4 → T1,T2,T3,T7. §16a naming → T1. §16b kill → T4. §16c escape → T5. §16d sub-reservation → T2. §16 decision-log (memory-reserve/namespacing/fallback) → T7. §16 simplification (bootstrap delete) → T7. §10 guard → T6. §11 (229/232) → T9. §14 asterisk → T6. Measurement → T8. Deferred to S2b+ (correctly absent): batch/prune/age-cap, try-acquire protocol 11→12, warning, OOM phantom, tunable measurement.
- **Atomicity/ordering:** T1,T2 precede T3; T4,T5 depend on T3; T6 ships with T3; T7 depends on T3; all land as ONE PR (no partial-topology merge). Stated up top.
- **Porous-test risk:** T9 uses the isolated-slice seam (not production), a real alloc-and-hold fixture (existing fixtures can't exercise aggregate OOM), deterministic pool-report anchors (not timing races), explicit false-pass traps (test-count-ran), and per-mechanism mutation checks.
- **ci-shim:** T2 sentinel, T7 shim-ordinary-reserve, and "topology no-op in shim" are asserted; shim has no scope so T3/T4/T5 are real-cgroup-only.
- **Frozen codec:** `redeclare.go` explicitly NOT touched (gate P1-5).
- **Line drift:** re-grep symbols before editing (flagged globally).

## Execution Handoff

**Subagent-Driven** (fresh Opus builder per task-group + Fable build-review), per the two-loop. Build order: T1 → T2 → T3+T6 (coupled) → T4 → T5 → T7 → T8 → T9 → T10. **Re-gate the redraft before building** (this plan failed GATE-1; it must pass GATE-2 first).
