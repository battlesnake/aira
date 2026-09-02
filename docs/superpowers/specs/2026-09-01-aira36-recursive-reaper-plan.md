# AIRA-36 — recursive orphan-scope reaper (reap nested scope subtrees)

**Status:** plan v2 — folds the Sol + DeepSeek-N/A + Fable plan-review (both BLOCK/GATE-FAIL, **direction sound**). v2 corrects the threat model, replaces the recursive proof with one `cgroup.events populated` read, fixes the porous discriminator test, and states the honest race post-condition.
**Ticket:** AIRA-36 (P2). Prerequisite for aitest's nested per-worker scopes (relates AIRA-30). Extends #72.
**Branch:** `aira36-recursive-reaper`
**Author:** Opus, grounded on the live finding (**31 orphaned `.aitest-worker-test` trees, 0 plain-leaf orphans**) + the actual reaper code.

## 1. Problem

The #72 orphan-scope reaper removes an orphaned scope with a **single-level** `unix.Unlinkat(parentFD, ".aira-"+scopeID, AT_REMOVEDIR)` (`confine_manage_linux.go:172`). The kernel refuses `AT_REMOVEDIR` on a **non-empty** cgroup, so a scope that has leftover **child cgroups** when its supervisor dies (crash / OOM-kill / SIGKILL — paths that never self-clean) is **never reaped**; the whole tree lingers permanently. Verified live: 32 lingering dead scopes, **31 were aitest `.aitest-worker-test`-nested trees** (dead supervisor, 0 procs, up to 46 min old); **0 plain-leaf orphans** (the reaper handles those). This blocks aitest's nested-scope model, whose design (§3.6) treats worker/supervisor OOM-death as normal — so the reaper **must** be the subtree-aware backstop (aitest confirmed; supervisor self-cleanup is best-effort on graceful exit only). This justifies **extending the primitive**, not adding a cleanup verb (architectural-simplicity).

## 2. Threat model (corrected — Fable MF3)

`AT_REMOVEDIR` (= `rmdir`) **cannot remove or kill anything with live processes**: the kernel returns `EBUSY` on a cgroup with member procs and `ENOTEMPTY` on one with child cgroups. The reap path contains **no signal / no `cgroup.kill`** — the only mutation is the `Unlinkat` at `confine_manage_linux.go:172`. So **the kernel `AT_REMOVEDIR` guard is, by itself, the never-remove-a-live-cgroup backstop** — a naive recursion still cannot kill a running worker.

The **real, lesser hazard** a naive recursion introduces: it would strip **empty sibling branches** out from under a still-live tree (an orphaned scope whose leaf is empty — so it passes the candidate gate — but which has one child holding a live leaked worker and another empty child). Removing the empty sibling is harmless to the worker but is **state churn / a policy violation** (we don't half-dismantle a scope that still has a live descendant). The candidate gate is **leaf-only** — `orphanedConfineScopeCandidates` checks `*record.Populated != 0` (`confine_manage.go:77`), and `Populated` comes from `scope.Members()` = the scope's **own** `cgroup.procs` (`confine_manage_linux.go:94-97`, `cgroup_linux.go:238-244`), not the subtree. So a scope with a live proc in a child **is** a candidate today (safe only because the single `AT_REMOVEDIR` then fails). The subtree-empty proof (§3) is what prevents the sibling-stripping and gives all-or-nothing in the steady state.

## 3. Design

Extend `reapOrphanedConfineScopesWithDeps` (`confine_manage_linux.go:139`). Scope-level candidate gates are **unchanged** (leaf-empty + supervisor-dead(ESRCH) + age≥grace + !Pending + no live admit lease). For each candidate, replace the single `Unlinkat` with a **two-phase, fd-anchored subtree reap**:

**Phase 1 — atomic subtree-empty proof (one read; Fable MF1 / Sol P1-2).** Open the candidate scope's dir fd (`O_RDONLY|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW` from the slice `parentFD`, as `:152`) and read its **`cgroup.events` `populated` field** — the kernel's own **hierarchical** signal (`1` iff the cgroup **or any descendant** holds a live process), read in one instant. Reuse the existing primitive `scope.Empty()` (`cgroup_linux.go:258-292`); this file already documents exactly this at `confine_manage_linux.go:340-345` ("Observe population SUBTREE-aware via cgroup.events `populated` … Leaf-only cgroup.procs would miss a workload that migrated into a child cgroup"). **If populated ≠ 0 → skip the whole scope** (`Skipped++`), remove nothing. A per-node `cgroup.procs` walk would NOT be atomic (a proc migrating child→child between two node reads makes an all-empty proof falsely pass) — the single root `populated` read is the sound proof.

**Phase 2 — post-order fd-anchored removal.** Only after Phase 1 proves the subtree empty, remove it deepest-first:
- Enumerate each cgroup's children via **its own `O_NOFOLLOW` dir fd** (`unix.Openat(parentFD, name, O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC)` then `os.NewFile(fd,...).ReadDir` — take-ownership/single-close per the existing pattern at `confine_manage_linux.go:93,123`). **Never** `filepath.Join` + `os.ReadDir` — there is no fd-anchored walk helper to inherit (only path-based `os.ReadDir(slicePath)` at `:65`), so this must be pinned or a lazy impl reintroduces the reconstructed-path TOCTOU (Fable MF5).
- `Unlinkat(fd, subName, AT_REMOVEDIR)` deepest-first (children before parents), anchored to each level's fd; the scope root anchored to the slice `parentFD`.
- **Depth bound (e.g. 32):** on overflow, `Skipped++` for that scope (match the per-candidate `continue` at `:161-163`), never error the whole sweep.
- Fail closed on any enumeration/open/read error → skip the scope (`Skipped++`).

## 4. Safety & invariants (honest)

- **Never removes a cgroup with a live process** anywhere in its subtree — guaranteed by the kernel `AT_REMOVEDIR` (`EBUSY`/`ENOTEMPTY`) at every node, independent of Phase 1. Phase 1 adds all-or-nothing in the steady state and prevents empty-sibling stripping.
- **Honest race post-condition (Sol P0 / Fable MF4):** strict all-or-nothing is **not** achievable once removal starts — `rmdir` is irreversible. If a worker forks into a node **between** Phase 1 and its removal, that node's `AT_REMOVEDIR` fails `EBUSY`, its **ancestors survive**, but **already-removed descendants (each kernel-verified empty at its removal instant) stay removed**; the scope reaps on a later pass. Normative: **Phase 1 completes before the first `Unlinkat`.**
- **Honest TOCTOU (Sol P1-3):** `O_NOFOLLOW` + fd-anchoring defeat symlink escape and path-reconstruction; a directory **rename/replacement** between open and `Unlinkat` is **not** fully eliminated, but **cannot cause a false reap** — `AT_REMOVEDIR` still only removes a genuinely-empty cgroup at that fd. No over-claim of complete identity-TOCTOU elimination.
- **Safe-direction on all uncertainty:** read failure, `populated≠0`, depth overflow, or mid-sweep repopulation → skip / stop, reap nothing more, leave for next pass. No `SIGKILL`, no force, no `cgroup.kill`.
- **Accounting:** a skipped nested scope increments `Skipped` **once per scope**, not per node (`ConfineReapResult`, `confine_manage.go:58-63` unchanged).
- **No behavioural change for plain (non-nested) scopes:** a leaf orphan has an empty `populated` and no children → Phase 1 passes, one `AT_REMOVEDIR` → identical to today. Scope-level gates, daemon loop/cadence (`confine_reaper.go`), `hasLiveLease`, and `ConfineReapResult` shape unchanged.

## 5. Tests

1. **Nested orphan reaped (real-cgroup):** an orphan scope (dead supervisor, gated) with empty child (and grandchild) cgroups → whole tree removed, `Reaped` includes it. The regression for the actual bug. Build on `TestReapOrphanedConfineScopesRealCgroupSafetyGates` (`confine_manage_linux_test.go:383-451`, uses `cgrouptest.IsolatedScopeParent`).
2. **Discriminating false-reap guard (real-cgroup — Sol P0 / Fable MF2):** an orphan scope with a **live proc migrated into one child branch** AND a separate **empty sibling child (+ empty grandchild)**. Assert: **nothing is removed — the empty sibling MUST survive** (and the live proc, its cgroup, and the scope root). This is the only assertion that discriminates the Phase-1 all-empty proof from a naive post-order recursion (which would strip the empty sibling). Test 2-as-a-lone-live-child would falsely pass naive recursion (kernel EBUSY leaves everything) — so the empty sibling is mandatory.
3. **Deep nest (≥3 levels):** all-empty deep tree fully reaped deepest-first; a live proc at the deepest level → whole tree intact (Phase 1 populated=1).
4. **Mid-sweep repopulation race:** a proc forks into a node after Phase 1 passes → that node's `AT_REMOVEDIR` fails; assert live node + ancestors survive; already-removed empty descendants **may** be gone (honest post-condition, not "all intact").
5. **Plain leaf orphan unchanged:** a childless orphan reaps exactly as today.
6. **fd-anchoring (re-scoped — Fable nit):** cgroupfs forbids symlink creation, so symlink-swap inside cgroupfs is not a reachable threat; test the walker's `O_NOFOLLOW|O_DIRECTORY` open flags via a tmpdir-fixture unit test (or assert the flags), defense-in-depth only — not a real-cgroup symlink test.
7. Full daemon + runner suites green under `aira confine`; `make ci`; `-race` on the reaper path (mind AIRA-20).

## 6. Rollout

Daemon-side only (`internal/daemon/confine_reaper.go:61` is the sole non-test caller) ⇒ **daemon restart**; grace/live-lease/cadence untouched; #74 startup reconstruction is orthogonal and makes the deploying restart safe. Deploy watched, owner-gated, rollback-ready (binary backup + old daemon). After deploy, nested orphans are swept automatically (I already manually cleaned the 31 dead ones with a per-node liveness gate; 2 live aitest tests correctly skipped). Ping aitest so it points its spec's "backstopped by #72's reaper" claim at the fixed version.

## 7. Deferrals

- No new cleanup verb (supervisor self-cleanup stays best-effort; the reaper is the authoritative backstop — extend the primitive, don't add machinery).
- `scope-integrity=migrated` telemetry for nested scopes (AIRA-34) stays documented-not-fixed.
