# AIRA-36 — recursive orphan-scope reaper (reap nested scope subtrees)

**Status:** plan (pre-review)
**Ticket:** AIRA-36 (P2). Prerequisite for aitest's nested per-worker scopes (relates AIRA-30). Extends #72.
**Branch:** `aira36-recursive-reaper`
**Author:** Opus, grounded on the live finding (31 orphaned `.aitest-worker-test` trees the current reaper could not sweep) + the actual reaper code.

## 1. Problem

The #72 orphan-scope reaper removes an orphaned scope with a **single-level** `unix.Unlinkat(parentFD, ".aira-"+scopeID, AT_REMOVEDIR)` (`confine_manage_linux.go:172`). The kernel refuses `AT_REMOVEDIR` on a **non-empty** cgroup, so a scope that has leftover **child cgroups** when its supervisor dies (crash / OOM-kill / SIGKILL — paths that never self-clean) is **never reaped** — the whole tree lingers permanently. Verified live: 32 lingering dead scopes, **31 were aitest `.aitest-worker-test`-nested trees** (dead supervisor, 0 procs, up to 46 min old); 0 plain-leaf orphans (the reaper handles those). This blocks aitest's nested-scope model, whose design (§3.6) treats worker/supervisor OOM-death as normal — so the reaper **must** be the subtree-aware backstop (aitest confirmed; supervisor self-cleanup is best-effort on graceful exit only).

## 2. The one real hazard to design against

The current candidate gate (`orphanedConfineScopeCandidates`, `confine_manage.go:70`) proves `Populated == 0` — but `Populated` is the scope's **own leaf** `cgroup.procs` count (`confine_manage_linux.go:94-97`), **not** the subtree. A scope whose parent leaf is empty but which has a **live worker in a child cgroup** passes this gate today. It is safe today only because the single `AT_REMOVEDIR` then fails (child present) → `Skipped`. **A naive recursion would remove that live worker's cgroup — killing a running test.** So the recursion's load-bearing requirement is: **never remove a cgroup that (or whose descendant) holds a live process.**

## 3. Design

Extend `reapOrphanedConfineScopesWithDeps` (`confine_manage_linux.go:139`). For each gated candidate (unchanged scope-level gates: leaf-empty + supervisor-dead(ESRCH) + age≥grace + !Pending + no live admit lease), replace the single `Unlinkat` with a **fd-anchored recursive reap**:

`reapScopeSubtree(parentFD, childName)`:
1. Open the child dir fd from `parentFD` with `O_RDONLY|O_DIRECTORY|O_CLOEXEC|O_NOFOLLOW` (same anchoring as `confine_manage_linux.go:87-93`; `O_NOFOLLOW` defeats a symlink-swap TOCTOU, so no path reconstruction).
2. **Positive-proof, all-subtree-empty gate (the new safety):** read this cgroup's `cgroup.procs` via the dir fd — if non-empty, **abort the whole subtree** (return "skipped", remove nothing). A live proc anywhere ⇒ leave the entire scope intact for a later pass.
3. Recurse into every sub-directory (child cgroups) depth-first — each must also prove empty; abort the whole subtree if any descendant is non-empty.
4. **Deepest-first removal:** only after every descendant has proven empty, `Unlinkat(fd, subName, AT_REMOVEDIR)` each child (deepest-first), then the scope dir itself anchored to the original `parentFD`.
5. **Depth bound:** cap recursion depth (e.g. 32) as a loop/pathology backstop; `O_NOFOLLOW` already prevents symlink escape, and confine subtrees are shallow (aitest: outer → supervisor + N workers = 2 levels).

**Race-safety is doubly guaranteed:** (a) the all-empty pre-check refuses any subtree with a live proc; (b) `AT_REMOVEDIR` itself only ever rmdirs a genuinely-empty cgroup, so if a worker forks into a child *between* the check and the removal, that node's `Unlinkat` fails with `EBUSY`/`ENOTEMPTY` → its ancestors' removals also fail → the scope is left intact, `Skipped`, never a fabricated reap. No `SIGKILL`, no force — a scope with any live descendant is simply left for the next pass. This preserves #72's "positive proof of every facet, safe-direction on any uncertainty" contract, now applied per node.

**Partial-removal policy:** all-or-nothing per scope — if any descendant is non-empty, remove **nothing** in that scope (don't strip empty sibling branches out from under a still-orphaned tree). Cleaner state, matches #72's per-facet proof; the tree self-heals on a later pass once the live descendant exits.

**No change to:** the scope-level candidate gates, the daemon loop/cadence (`confine_reaper.go`), the `hasLiveLease` live-query, or the `ConfineReapResult` shape (Reaped/Skipped counts stay meaningful per scope).

## 4. Safety & invariants

- **Never removes a cgroup with a live process** anywhere in the subtree (all-subtree-empty positive proof + `AT_REMOVEDIR` kernel guard). This is the load-bearing correctness property — a false reap would kill a running (leaked) worker.
- **fd-anchored throughout** (`O_NOFOLLOW`, no path reconstruction) — preserves #72's symlink-swap TOCTOU defence at every level.
- **Safe-direction on uncertainty:** any read failure, non-empty node, or mid-sweep repopulation → skip the whole scope, reap nothing (`Skipped++`), leave for next pass.
- **No behavioural change for plain (non-nested) scopes:** a leaf orphan has no children → step 2 proves empty → single `AT_REMOVEDIR` → identical to today.
- **Scope-level gates unchanged:** supervisor-dead + age + !Pending + no-live-lease still required before a scope is even a candidate.

## 5. Tests

1. **Nested orphan reaped (real-cgroup):** an orphan scope (dead supervisor, gated) with an empty child cgroup → the whole tree is removed; `Reaped` includes it. The regression for the actual bug.
2. **Live worker in a child is NOT reaped (real-cgroup — the load-bearing false-reap guard):** an orphan scope whose *leaf* is empty (passes the current gate) but with a **live process in a child cgroup** → **nothing** is removed, the live worker's cgroup survives, `Skipped`. This is the test that must fail against a naive recursion that only trusts the leaf `Populated` gate.
3. **Deep nest (≥3 levels):** all-empty deep tree fully reaped deepest-first; a live proc at the deepest level leaves the entire tree intact.
4. **Mid-sweep repopulation race:** a proc forks into a child between the empty-check and the removal → `AT_REMOVEDIR` fails, scope left intact, no fabricated reap (inject via the scan deps / a real fork).
5. **Plain leaf orphan unchanged:** a childless orphan reaps exactly as today.
6. **Symlink/`O_NOFOLLOW`:** a child name swapped to a symlink is not traversed (removal fails safe).
7. Full daemon + runner suites green under `aira confine`; `make ci`; `-race` on the reaper path (mind AIRA-20).

## 6. Rollout

Daemon-side only (the reaper runs in the daemon) ⇒ **daemon restart**. #74 reconstructs on startup (safe). Deploy watched, owner-gated, rollback-ready (binary backup + old daemon). After deploy, the accumulated nested orphans (I manually cleaned the 31 dead ones with a per-node liveness gate; the 2 live aitest tests were correctly skipped) are swept automatically going forward. Ping aitest so it points its spec's "backstopped by #72's reaper" claim at the fixed version.

## 7. Deferrals

- No new cleanup verb (aitest supervisor self-cleanup stays best-effort on graceful exit; the reaper is the authoritative backstop — architectural-simplicity: extend the primitive, don't add machinery).
- The `scope-integrity=migrated` telemetry for nested scopes (AIRA-34) stays documented-not-fixed.
