# aitest v0.7 S2a — sibling worker scopes + `--delegate-ram` collapse + v7-1 excision — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

> **GATE-1 REDRAFT (2026-09-12).** The Fable plan-gate affirmed the architecture but found nesting was silently doing four jobs (unique naming, kill propagation, escape attestation, sub-reservation marking) the first draft replaced none of, two gates that would pass against a broken impl, an incomplete excision inventory, and a strong simplification. This redraft folds all of it. See spec §16.

> **GATE-2 FOLD (2026-09-12).** GATE-2 = narrow FAIL: bulk closed, architecture affirmed, P0-1/P1-1/P1-4 closed on the fresh-admit path. Two restart-path gaps + a sharper escape mechanism folded (spec §16.1): (1) **restart parity** — the relay re-declares keyed by path + `serveReDeclare` has no kill hook → post-restart parent-kill re-orphans workers (Task 4); (2) **local escape exemption** — mint the worker-name pid slot with the **parent supervisor pid** so the exemption is a local `pid==os.Getpid()` check, no daemon coupling (Task 1 + Task 5); (3) `confine --kill` selector handling is now **mandatory** (Task 10); (4) the `(seq,parent-pid,stamp)` id is unique by construction → drop reseed/`@dr` (Task 1); (5) a new bounded daemon-down accepted gap (Accepted Gaps).

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

## Task 1: First-class worker confine scope names; pid slot = parent supervisor pid (P0-1, P1-B, P2-C)

**Files:** `internal/daemon/worker_admit.go` (`workerScopeFor`/`allocateWorkerScopeID` :117-214, EEXIST reseed :602-611 — **delete the reseed/EEXIST machinery**), `internal/runner/worker_scope_linux.go`/`worker_scope.go`, `internal/runner/confine.go` (mint via the `confineScopeID`/`MintConfineScopeID` grammar :1138-1255); drop the `@dr` marker (`IsDelegateRAMScopeID`, `bindConfineScopeID:1288`) — its only consumers go in Task 7; Test: `internal/daemon/worker_admit_test.go`.

**Interfaces:** Produces `CONFINE-aitest-w<seq>-<parentSupervisorPID>-<stamp36>` worker ids → name `aitest-w<seq>` (valid per `ValidateConfineIdentity`). **The pid slot is the PARENT SUPERVISOR pid** — parsed from `parent_scope_id`, which equals `os.Getpid()` of the monitor process (foreground and `--detach` alike, per `MintConfineScopeID`). This choice is load-bearing downstream: Task 5's escape exemption is a local `parseConfineScopeID(basename).pid == os.Getpid()` check, and a *populated* worker scope whose embedded pid is dead is positive orphan proof. `(seq, parentPid, timeStamp)` is **unique by construction** — no cross-scope counter, no reseed, no `EEXIST` path. The granted lease's `scopeID` == scope dir-name-minus-`.aira-`.

- [ ] **Step 1: Failing unit test** — two distinct supervisor contexts (distinct parent pids) admitting workers under the one slice produce **distinct, `parseConfineScopeID`-parseable** ids with the parent pid in the pid slot; no `EEXIST`/`WorkerScopeIDCollision` code path is reachable. Expected: FAIL (current per-outer `worker-N` collides + is unparseable).
- [ ] **Step 2: Run, confirm fail** (collision or parse-reject).
- [ ] **Step 3: Implement** — mint via the confine grammar with the parent-supervisor pid slot; **delete** `allocateWorkerScopeID`'s per-outer scan + the EEXIST reseed (unique-by-construction); set the lease `scopeID` = dirname-minus-`.aira-` so the reaper `hasLiveLease` veto (confine_reaper.go:54-61), `ReapScopeIfEmpty`/`--kill` (confine_manage_linux.go:369-380,560-631) and `oomsteer` `confineScopeDirName` (oomsteer.go:232) all align.
- [ ] **Step 4: Run, confirm pass**; re-run worker-admit + reaper tests (no regression). Confirm `parseConfineScopeID` accepts the name (gate verified the grammar does).
- [ ] **Step 5: Commit.**

## Task 2: Explicit `parent_scope_id`; preserve sub-reservation marking (P1-3, §16d)

**Files:** `worker_admit.go` (`workerParentScopeID` :225-232, `isSubReservation` admit.go:328-330), `internal/pylib/env.go` (`AIRA_CONFINE_SCOPE_ID` :196), `supervisor.py` (~:953-954), `confine_shim_linux.go` (sentinel :300-304); Test: `worker_admit_test.go`.

**Interfaces:** Worker-admit carries `parent_scope_id` as an explicit field (the supervisor's confine scope **id**, from `AIRA_CONFINE_SCOPE_ID` — not `self.outer_scope`, which is a path). Empty is refused; a non-empty value is validated **parseable** by `parseConfineScopeID` (mirror admit.go:3017; the shim sentinel is the one exempt value), so Task 5 can extract its pid.

- [ ] **Step 1: Failing tests** — (a) `workerParentScopeID` returns the explicit id under sibling placement; (b) an **empty** `parent_scope_id` is refused `E_DAEMON_PROTOCOL` (so a worker can never silently become a job); (c) a non-empty-but-unparseable `parent_scope_id` is refused (except the sentinel); (d) `isSubReservation` still holds for a worker. Expected: FAIL.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — add the field; supervisor sends the id (`AIRA_CONFINE_SCOPE_ID`, always published on the real path, confine_linux.go:1110); ci-shim publishes the sentinel; daemon refuses empty + validates parseability; the exclusivity exemption + `isSubReservation` read the field.
- [ ] **Step 4: Run, confirm pass**; re-run the AIRA-68 "workers aren't jobs" invariants (`outstandingJobs`, `sliceProvablyEmpty`, drain/`--exclusive`, `confine --list` N-jobs). **The real-cgroup e2e harness must publish a canonical `AIRA_CONFINE_SCOPE_ID` as of THIS task** (not Task 9) — otherwise every existing real-cgroup aitest e2e reds on refuse-empty (P3c).
- [ ] **Step 5: Commit.**

## Task 3: Create worker scopes as siblings under the slice (§4)

**Files:** `worker_admit.go` `create(...)` :599 (parent = slice, reuse the resolved `DefaultConfineSlice` :429-433), `worker_scope_linux.go` :63-108, `worker.py` `place_self` :453-459; Test: the Task 9 `...WorkerScopesAreSiblings` gate anchor + a unit test on the path.

- [ ] **Step 1: Failing unit test** — the computed worker scope path is slice-rooted with the Task-1 name. Expected: FAIL.
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** — pass the slice (not `req.outerScope`) as parent; reuse the ordinary confine scope-creation path so controller delegation + common-ancestor `cgroup.procs` migration (the permission model `place_self` needs — the slice already carries `+memory/+cpu` via `ensureConfineDelegation`) come for free; daemon returns the new path to the supervisor → worker `place_self` unchanged. **Note (P3f):** "the ordinary confine scope-creation path" is currently client launch code (confine_linux.go:793); extract it into a shared helper callable from the daemon's worker-admit, rather than duplicating it.
- [ ] **Step 4: Run, confirm pass** + full `internal/pylib/aitest` suite + existing real-cgroup e2e. Worker RAM no longer charges the outer scope.
- [ ] **Step 5: Commit.**

## Task 4: Daemon kills + rmdirs worker scopes on relay peer-EOF — fresh AND restart paths (P1-1, P1-A, §16b/§16.1)

**Files:** `worker_admit.go` relay lifecycle :538-548/:630-636; `server.go` `serveReDeclare` :913-1030; `worker_admit_client_linux.go` re-declare key :229-237; `confine_manage_linux.go` (`cgroup.kill`+rmdir helper :609-624, ENOENT-tolerant); Test: daemon unit tests for both paths + the `stopping` guard; real-cgroup gates D (Task 9) + the S18 restart merge-gate (`restart_merge_gate_worker_e2e_test.go`).

**Interfaces:** On a worker relay's peer-EOF (`peerCtx.Done()`), the daemon `cgroup.kill`s then rmdirs that worker scope (rmdir tolerates ENOENT — the supervisor's own `_forget_worker_scope` may have won the race, :2216). Skipped when `s.stopping` (a daemon restart EOFs every relay, but workers must survive and re-declare, :85-91). **This is the one path where relay death now kills its worker** (was: lease-only release) — documented as an intended behaviour change.

**Restart parity (P1-A — the GATE-2 gap):** both halves must hold across a daemon restart, not just on fresh admit:
- (i) the relay re-declares its lease keyed by `strings.TrimPrefix(filepath.Base(ScopePath), ".aira-")` (currently `ScopeID: grant.ScopePath`, a path — which would undo Task 1's dirname alignment);
- (ii) `serveReDeclare` installs the **same** peer-EOF kill+rmdir when `scopeID != "" && parentScopeID != ""` (that shape is uniquely a worker lease — a scoped *ordinary* admit carries no `parent_scope_id`, admit.go:3027-3030), so a post-restart parent-kill does not re-orphan mid-test workers.

- [ ] **Step 1: Failing tests** — (unit) on peer-EOF the worker scope is killed+removed; on `s.stopping` it is NOT. (unit/merge-gate) after a simulated restart + re-declare, the lease key is the dirname (not a path) AND a subsequent peer-EOF kills+rmdirs the re-declared worker. (e2e, Task 9 Gate D) kill the parent → no worker survives, no dir remains — both fresh and post-restart. Expected: FAIL (today relay-EOF frees the lease but leaves process+dir; re-declare keys by path and installs no kill).
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** the peer-EOF kill+rmdir on the fresh relay path; re-key the re-declare; install the same hook in `serveReDeclare`; ENOENT-tolerant rmdir; guard both on `peerCtx.Done()` vs `s.stopping`.
- [ ] **Step 4: Run, confirm pass**; verify `_forget_worker_scope` still works and the double-rmdir race is benign (ENOENT); extend the S18 restart merge-gate with the re-anchored key-set assertion + a "parent kill after restart leaves no orphan" case.
- [ ] **Step 5: Commit.**

## Task 5: Exempt the worker fork→place_self migration from escape attestation — LOCAL pid check (P1-2, P1-B, §16c/§16.1)

**Files:** `internal/runner/runner_linux.go` `monitorScopeMembership`/`witnessedEscape` :1627-1832 **and** `classifyLaunchScopeIntegrity`'s `Teardown.Escape` check :1593, `confine_linux.go` :1237,1400-1408; Test: Task 9 `...ScopeIntegrityContained` gate + mutation.

**Interfaces:** A process seen alive in a sibling cgroup whose basename parses (`parseConfineScopeID`) to **name prefix `aitest-w` AND embedded pid == `os.Getpid()`** (this monitor's own pid — which is the pid Task 1 minted into the worker name) is NOT a witnessed escape. This is a **purely local positive check** — no lease table, no daemon round-trip per membership sample — and, crucially, it holds **through teardown**: on a `--timeout`/`--kill` of the parent the relays die and leases release, but the pid-in-the-name is still `os.Getpid()`, so the killed-run case no longer false-flags `descendant-escaped` (the GATE-2 P1-B defect). A genuine escape to any *other* cgroup (no `aitest-w` name / wrong pid) stays witnessed.

- [ ] **Step 1: Failing gate** (Task 9 Gate E) — a delegate run attests `scope-integrity=contained`, not `descendant-escaped`, **including on the `--kill`/`--timeout` teardown path**. Expected: FAIL (every worker migration is currently witnessed).
- [ ] **Step 2: Run, confirm fail.**
- [ ] **Step 3: Implement** the local exemption in BOTH the live sampler (`witnessedEscape`) and the teardown classifier (`Teardown.Escape`). Requires Task 1's parent-pid slot and Task 2's parseability guarantee.
- [ ] **Step 4: Run, confirm pass**; confirm a genuine escape (a process in an unrelated cgroup, or an `aitest-w` name whose pid ≠ this monitor) is still witnessed (don't over-exempt); mutation: drop the pid check → Gate E reds.
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
  - **Shim sentinel source moves (P3a):** the `parent_scope_id` sentinel was published by the now-deleted bootstrap verb (main.go:1878); the **ci-shim launcher** must publish it directly (confine_shim_linux.go:290 currently withholds it deliberately), or shim delegate runs fail refuse-empty (Task 2).
  - **Fallback e2e trigger (P3b):** the two fallback e2e tests (pytest_aitest_e2e_test.go:127,219) trigger on the deleted `AIRA_AITEST_BOOTSTRAP_CMD` binary (env.go:21,83) — give them a new daemon-down trigger and drop the dead env key.
- [ ] **Step 4: Run, confirm pass** + full suite + shim tests (shim delegate still ordinary-reserve + coordinate+sentinel publish) + a real-cgroup delegate run (supervisor runs directly in the parent scope; workers sibling).
- [ ] **Step 5: Commit.**

## Task 8: Measurement channel → parent memory.peak + per-worker `scope_path` (P2-5, P2-A)

**Files:** `supervisor.py` `_emit_measurement_report` :2145-2156, `_pool_peak_records` :2040, `_cleanup_supervisor_scope`; Test: `test_measurement_report.py` + the measurement e2e.

- [ ] **Step 1: Failing tests** — (a) the report's supervisor-peak term reads the parent `memory.peak` (real value), not the removed `supervisor_scope`; (b) `_pool_peak_records` now includes each worker's granted `scope_path` (Gate A's deterministic anchor — today it holds only peak/memory_max/oom). Expected: FAIL.
- [ ] **Step 2-4:** implement the redirect; add `scope_path` to the per-worker record; drop the `supervisor_scope=` token; run green.
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

## Task 10: `confine --list`/`--kill` selector (MANDATORY), tidy, ticket closure (P2-4, P3)

- [ ] **Step 1 (MANDATORY, not "decide"):** every sibling worker of a delegate job embeds the **same** supervisor pid (Task 1), so `confine --kill <supervisor-pid>` would hit `E_SELECTOR_AMBIGUOUS` (confine_manage_linux.go:560-566) — a regression for a core gesture. Label worker rows with a recognisable component and **filter them from the default `--list`/`--kill` pid/name selector** (a worker is killed via its parent, or an explicit worker-scope selector), so `--kill <supervisor-pid>` resolves to the parent job unambiguously. Test both the ambiguity-is-gone and the worker-still-reachable cases.
- [ ] **Step 2:** Keep/rename `readOuterMemoryEventCounter` (don't delete-then-re-add); re-home/drop `testdata/test_slow_passing.py` (orphaned by the SkipTick gate deletion). **Correct the oomsteer touch:** sub-reservations `continue` before `budgets[]` (oomsteer.go:445-447) so worker leases are **never steered** (the earlier "harmless once names parse" note was wrong) — the real change is to **drop the `children` aggregation** of the parent's `memory.current`, since sibling workers no longer charge the parent.
- [ ] **Step 3:** `aira transition` AIRA-229 + AIRA-232 → done (note → spec §11/§16/§16.1 + this slice; `aira link` for relations, never hand-write tuples).
- [ ] **Step 4:** Full `internal/{pylib,runner,daemon}/...` build+vet+test under `aira confine`; exact exit codes recorded. PR (one atomic PR for Tasks 1-10).

---

## Accepted Gaps (S2a)

- **Daemon-down parent-kill (spec §16.1, P2-B).** Nesting's `cgroup.kill` was daemon-independent (workers were in the parent's subtree); sibling kill depends on the daemon being alive at parent-death. If the daemon is down **and** the parent is killed **and** a worker is mid-test, that worker survives until it finishes its current test and hits nodeid-pipe EOF (supervisor gone), then exits — a **bounded "one more test" leak**, not permanent. Accepted for S2a. Task 1's parent-pid slot makes a future reaper fix cheap (a *populated* worker scope whose embedded pid is dead = positive orphan proof) if the bounded leak ever matters — not built now ("keep the primitive + document the gap").

## Self-Review

- **Spec coverage:** §4 → T1,T2,T3,T7. §16a naming → T1. §16b kill → T4. §16c escape → T5. §16d sub-reservation → T2. §16 decision-log (memory-reserve/namespacing/fallback) → T7. §16 simplification (bootstrap delete) → T7. §10 guard → T6. §11 (229/232) → T9. §14 asterisk → T6. Measurement → T8. Deferred to S2b+ (correctly absent): batch/prune/age-cap, try-acquire protocol 11→12, warning, OOM phantom, tunable measurement.
- **Atomicity/ordering:** T1,T2 precede T3; T4,T5 depend on T3; T6 ships with T3; T7 depends on T3; all land as ONE PR (no partial-topology merge). Stated up top.
- **Porous-test risk:** T9 uses the isolated-slice seam (not production), a real alloc-and-hold fixture (existing fixtures can't exercise aggregate OOM), deterministic pool-report anchors (not timing races), explicit false-pass traps (test-count-ran), and per-mechanism mutation checks.
- **ci-shim:** T2 sentinel, T7 shim-ordinary-reserve, and "topology no-op in shim" are asserted; shim has no scope so T3/T4/T5 are real-cgroup-only.
- **Frozen codec:** `redeclare.go` explicitly NOT touched (gate P1-5).
- **Line drift:** re-grep symbols before editing (flagged globally).

## Execution Handoff

**Subagent-Driven** (fresh Opus builder per task-group + Fable build-review), per the two-loop. Build order: T1 → T2 → T3+T6 (coupled) → T4 → T5 → T7 → T8 → T9 → T10 (T1's pid slot feeds T5; T8's `scope_path` feeds T9 Gate A). **Re-gate before building** — this plan failed GATE-1 (four nesting-jobs) and GATE-2 (two restart-path gaps + the exemption mechanism), both narrow and converging; it must clear GATE-3 first.
