# AIRA-49: Release stuck confine ledger leases on confirmed-dead supervisors — Implementation Plan (v2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a confine job's owning process dies without releasing its admission lease (external SIGKILL of the wrapping `aira confine` launcher, or any other non-graceful death), the daemon must eventually release the stuck ledger reservation on its own — never leaving a permanently unrecoverable reservation, and never requiring a daemon restart to clear it — and it must do so with a safety property AT LEAST as strong as the one the existing physical-directory reaper already has, not a weaker one.

**Architecture (v2 — revised after a v1 plan-review BLOCK from Codex/Sol; see "What changed from v1" below):** A daemon-side sweep pass finds ledger-granted leases whose scope LOOKS dead by a cheap, coarse, list-scan-based pre-filter (empty by leaf-only `cgroup.procs`, recorded supervisor PID not found via `kill(pid,0)`, past a grace window, not mid-launch). For each such candidate, it ATTEMPTS THE ACTUAL PHYSICAL REAP of that scope's directory tree, reusing the exact same fd-anchored, kernel-enforced removal (`reapEmptyConfineScopeTree`) the existing orphaned-scope reaper (AIRA-36/#72) already uses and already trusts. `Unlinkat(AT_REMOVEDIR)` on a cgroup directory that still has ANY live process anywhere in its subtree fails at the kernel level (`ENOTEMPTY`) — unconditionally, atomically, and with no window for a stale userspace signal to matter. **Only if that physical reap actually succeeds** does the pass release the ledger lease. The coarse pre-filter is now explicitly just a cheap "is this candidate worth attempting" gate, not a safety proof; the kernel's own atomic removal is the sole safety gate, exactly mirroring how the existing orphan reaper is already safe today.

**What changed from v1, and why:** v1 proposed releasing the lease based on the list-scan's `Populated` field directly, reasoning it reused "the same proof" the existing reaper already trusts. Codex/Sol's plan-review (BLOCK) found this was actually a materially WEAKER signal than v1 claimed: `ConfineRecord.Populated` (`internal/runner/confine_manage_linux.go`, `listConfinesWithDeps`) is populated from `scope.Members()` — a read of `cgroup.procs`, which is **leaf-only** (a cgroup-v2 directory with a live nested child cgroup, e.g. aitest's own `.aira-supervisor` holding worker sub-scopes, correctly shows ZERO entries in ITS OWN `cgroup.procs` even while its subtree is very much alive). The existing reaper's actual safety comes from a SEPARATE mechanism: `reapEmptyConfineScopeTree` re-opens the scope fresh and calls `Empty()` (subtree-aware, via `cgroup.events`'s `populated` field) immediately before removal, AND the kernel itself refuses to remove a non-empty directory regardless — so even a wrong candidate selection there is harmless (the `Unlinkat` just fails, `Skipped++`). v1's release path had no equivalent: releasing a ledger entry is not a kernel-guarded operation, so a wrong "empty" verdict there is not self-correcting. Sol also found this made the plan's own PID-namespace safety concern (which v1 flagged but did not resolve) moot in the WRONG direction — a `kill(pid,0)` false-negative on a genuinely-live supervisor would have gone straight through to an unsafe release. v2's reap-first-release-second design fixes this at the root: it makes the SAME kernel-enforced check the existing reaper already relies on the sole gate for release too, so a wrong pre-filter guess (whatever its cause — leaf-vs-subtree, PID-namespace weirdness, a race) can only ever result in a harmless failed reap attempt, never an unsafe release. Sol also caught a literal no-op bug in v1's pseudocode (the candidate function's nil-`supervisorDead` fail-closed guard was never given a real default, so the daemon's own call site — passing `nil` expecting an internal default — would have made the whole feature inert) and porous/unrunnable test scaffolding (hardcoded real-slice resolution with no path-injection seam, and a misunderstanding of `scopeReapGrace`'s zero-value fallback). v2 fixes all of these directly in the design below, not as afterthoughts.

**Tech Stack:** Go (`internal/runner`, `internal/daemon`), existing `pytest`/`go test` toolchains unaffected.

**Spec:** Ticket AIRA-49 (`.aira/tickets/AIRA-49.md`) carries the original root-cause analysis. This plan document's "What changed from v1" section above carries the plan-review findings that shaped the actual design; read both.

## Global Constraints

- No cgo; one static Go binary.
- Every heavy command (`go build`, `go test`, `go vet`) MUST be run via `aira confine --`.
- Correctness-critical work: full two-loop (plan-review, Fable gate, implement, build-review, Fable/build gate) before merge.
- Reuse existing primitives; do not duplicate the reap machinery. The physical removal in this plan MUST go through `reapEmptyConfineScopeTree` (the exact function the existing orphan reaper calls) — do not write a second removal implementation, and do not release a lease based on any signal OTHER than that function's own success return.
- `defaultScopeReapInterval` is 5 minutes and `defaultScopeReapGrace` is 2 minutes (`internal/daemon/paths.go`). Worst-case time to release a genuinely stuck lease is therefore up to grace+interval (~7 minutes), not "within 2 minutes" — state this accurately in code comments and commit messages; do not imply a tighter bound than the code actually delivers.

---

## File Structure

- `internal/runner/confine_manage_linux.go` — refactor `reapOrphanedConfineScopesWithDeps`'s per-candidate removal into a small reusable helper (`reapConfineScopeByID`, taking an already-open slice parent fd), and add `ReleaseStaleGrantedLeases`, the new exported entry point the daemon calls.
- `internal/runner/confine_manage.go` — add `orphanHeuristicMatch` (renamed and reframed from v1's `isOrphanProven` — same fields, but the new name and doc comment make clear it is a coarse pre-filter, not a safety proof) and its two callers: the existing `orphanedConfineScopeCandidates` (unchanged behavior, just now built on the shared helper) and the new `staleGrantedLeaseCandidates` (unexported now — v1 exported it as `StaleGrantedLeaseCandidates` for the daemon to call directly, but v2 moves the daemon-facing entry point to `ReleaseStaleGrantedLeases` in the linux file instead, since release now requires the linux-only physical-reap step; keep the pure candidate-selection function unexported and package-internal).
- `internal/daemon/confine_reaper.go` — add `releaseStaleGrantedLeasesPass`, calling `runner.ReleaseStaleGrantedLeases` with `s.releaseActiveConfine` as the release callback, wired into `runScopeReaper`'s loop immediately before the existing `reapOrphanedScopesPass` call (a lease released in this pass becomes eligible for the immediately-following `reapOrphanedScopesPass` in the SAME tick as a bonus, though it is no longer load-bearing for correctness the way v1's ordering was, since this pass now does its own physical reap directly).
- Tests: alongside `orphanedConfineScopeCandidates`'s existing tests (find that file first) for the pre-filter; a new or existing real-cgroup test file (gated the same way the rest of this package's real-cgroup tests are, using `cgrouptest.IsolatedScopeParent`) for `ReleaseStaleGrantedLeases`, since its core safety property can only be meaningfully tested against a REAL cgroup tree (a fake/mocked `Empty()`/`Unlinkat` would just test the mock, not the kernel behavior the whole design leans on); a daemon-level test for `releaseStaleGrantedLeasesPass`'s wiring/callback plumbing (this one CAN use a fake, since it's testing plumbing, not kernel semantics).

## Task 1: Extract a shared coarse pre-filter (not a safety proof) and its inverse

**Files:**
- Modify: `internal/runner/confine_manage.go` (the existing `orphanedConfineScopeCandidates` function)
- Test: wherever `orphanedConfineScopeCandidates`'s existing tests currently live (find them first)

**Interfaces:**
- Produces: `orphanHeuristicMatch(record ConfineRecord, graceSeconds int64, supervisorDead func(pid int) bool) bool` (unexported) and `staleGrantedLeaseCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord` (unexported — package-internal only, called from `ReleaseStaleGrantedLeases` in Task 2, NOT called directly by the daemon).
- Consumes: `ConfineRecord` (existing), `pidIsDead` (existing, `confine_manage_linux.go`).

- [ ] **Step 1: Write the failing tests first**

Read the existing tests for `orphanedConfineScopeCandidates` before writing new ones — match their existing fixture-building style exactly.

```go
func TestStaleGrantedLeaseCandidatesMatchesTheSameHeuristicAsOrphanReapingInverted(t *testing.T) {
	grace := 2 * time.Minute
	deadPID := func(pid int) bool { return pid == 111 }
	hasLease := func(scopeID string) bool { return scopeID == "granted-and-looks-dead" || scopeID == "granted-but-alive" }

	populatedZero, populatedOne := 0, 1
	oldEnough, tooYoung := int64(200), int64(10)
	pid, alivePID := 111, 222

	records := []ConfineRecord{
		{ScopeID: "granted-and-looks-dead", SupervisorPID: &pid, Populated: &populatedZero, AgeSeconds: &oldEnough},
		{ScopeID: "granted-but-alive", SupervisorPID: &alivePID, Populated: &populatedZero, AgeSeconds: &oldEnough},
		{ScopeID: "granted-but-too-young", SupervisorPID: &pid, Populated: &populatedZero, AgeSeconds: &tooYoung},
		{ScopeID: "granted-but-leaf-nonempty", SupervisorPID: &pid, Populated: &populatedOne, AgeSeconds: &oldEnough},
		{ScopeID: "looks-dead-but-no-lease", SupervisorPID: &pid, Populated: &populatedZero, AgeSeconds: &oldEnough},
	}

	got := staleGrantedLeaseCandidates(records, grace, deadPID, hasLease)

	if len(got) != 1 || got[0].ScopeID != "granted-and-looks-dead" {
		t.Fatalf("candidates=%+v, want exactly [granted-and-looks-dead] -- this is a CANDIDATE list, not proof of anything; Task 2's physical-reap step is what actually decides", got)
	}
}
```

Plus the fail-closed-on-unknown-facets and nil-func-returns-none cases from v1 (unchanged reasoning — keep them, adjust names to `staleGrantedLeaseCandidates`).

Also confirm (re-run or write, if missing) a test that `orphanedConfineScopeCandidates`'s own existing behavior is byte-for-byte unchanged by this extraction.

- [ ] **Step 2: Run to verify failure** — `aira confine -- go test ./internal/runner/... -run 'StaleGrantedLeaseCandidates' -v` — expect FAIL (undefined function).

- [ ] **Step 3: Implement**

```go
// orphanHeuristicMatch is a CHEAP, COARSE pre-filter shared by orphan
// directory reaping and stale-lease release candidate selection. It is
// NOT proof of anything by itself -- Populated here is leaf-only
// (cgroup.procs via scope.Members(), NOT the subtree-aware cgroup.events
// populated field), so a cgroup with a live NESTED child (e.g. aitest's
// own .aira-supervisor holding worker sub-scopes) can match this heuristic
// while genuinely still being alive. Both real callers require a SEPARATE,
// authoritative confirmation before acting: orphan reaping gets it from the
// kernel itself refusing Unlinkat(AT_REMOVEDIR) on a non-empty directory;
// stale-lease release (Task 2) gets it by requiring that SAME kernel-enforced
// removal to actually succeed before releasing anything. Never wire this
// heuristic's output directly to an irreversible action.
func orphanHeuristicMatch(record ConfineRecord, graceSeconds int64, supervisorDead func(pid int) bool) bool {
	return record.Populated != nil && *record.Populated == 0 &&
		record.SupervisorPID != nil && supervisorDead(*record.SupervisorPID) &&
		record.AgeSeconds != nil && *record.AgeSeconds >= graceSeconds &&
		!record.Pending
}

func orphanedConfineScopeCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord {
	graceSeconds := int64(grace / time.Second)
	candidates := make([]ConfineRecord, 0)
	if supervisorDead == nil {
		return candidates
	}
	for _, record := range records {
		if !orphanHeuristicMatch(record, graceSeconds, supervisorDead) ||
			(hasLiveLease != nil && hasLiveLease(record.ScopeID)) {
			continue
		}
		candidates = append(candidates, record)
	}
	return candidates
}

// staleGrantedLeaseCandidates is orphanedConfineScopeCandidates's mirror
// image (AIRA-49): scopes that STILL HOLD a granted ledger lease
// (hasLiveLease true) despite matching the same coarse heuristic. This is
// package-internal on purpose -- ReleaseStaleGrantedLeases (confine_manage_linux.go)
// is the only sanctioned caller, since a candidate from this list must NEVER
// be released without first passing the physical-reap confirmation it wraps.
func staleGrantedLeaseCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord {
	graceSeconds := int64(grace / time.Second)
	candidates := make([]ConfineRecord, 0)
	if supervisorDead == nil || hasLiveLease == nil {
		return candidates
	}
	for _, record := range records {
		if !orphanHeuristicMatch(record, graceSeconds, supervisorDead) || !hasLiveLease(record.ScopeID) {
			continue
		}
		candidates = append(candidates, record)
	}
	return candidates
}
```

- [ ] **Step 4: Run to verify pass** — `aira confine -- go test ./internal/runner/... -run 'OrphanedConfineScopeCandidates|StaleGrantedLeaseCandidates' -v`

- [ ] **Step 5: Commit** — `git commit -m "refactor(runner): extract shared orphan pre-filter heuristic (AIRA-49)"`

## Task 2: Reap-first, release-second — the actual safety-bearing change

**Files:**
- Modify: `internal/runner/confine_manage_linux.go` (extract `reapConfineScopeByID`; add `ReleaseStaleGrantedLeases`)
- Test: a NEW real-cgroup test file (or an existing one covering `ReapOrphanedConfineScopes` at this level — check `internal/runner/confine_manage_linux_test.go` first), following the SAME `cgrouptest.IsolatedScopeParent(t)` + `AIRA_REAL_CGROUP` convention every other real-cgroup test in this package already uses. **Do not test this task's core safety property against a mock** — the entire point of the redesign is that the kernel's own `Unlinkat` behavior is the safety gate, so the test must exercise a REAL cgroup tree.

**Interfaces:**
- Consumes: `reapEmptyConfineScopeTree` (existing, this file), `staleGrantedLeaseCandidates` (Task 1), `runner.ListConfines` (existing).
- Produces: `func ReleaseStaleGrantedLeases(ctx context.Context, slicePath string, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool, release func(scopeID string)) (ConfineReapResult, error)` — reuses `ConfineReapResult` (already has `Reaped []string`/`Skipped int`, which map naturally onto "released" / "attempted but still populated (left alone, correctly)").

- [ ] **Step 1: Write the failing test first**

This MUST be a real-cgroup test (skips under `SkipOrFailRealCgroup` when unavailable, hard-fails under `AIRA_REAL_CGROUP=1` per this package's established convention — follow whatever existing real-cgroup test in this file already does verbatim for the skip/fail-mode boilerplate). Cases:

1. **The property this whole plan exists to deliver:** a scope directory with NO live processes anywhere in its subtree (use `cgrouptest.IsolatedScopeParent` to get a real delegated cgroup, do NOT put any live process in it), a `hasLiveLease` fake reporting it as leased, a `supervisorDead` fake reporting its recorded PID as dead, age past grace → `ReleaseStaleGrantedLeases` returns it in `Reaped`, the `release` callback is invoked exactly once with its scope ID, and the directory is actually gone from disk afterward.
2. **The property v1 was missing and Sol caught:** a scope directory with a LIVE NESTED CHILD cgroup holding a real live process (fork a real long-lived child process — e.g. `sleep 30` — placed into a child cgroup one level under the candidate scope, so the candidate's OWN leaf `cgroup.procs` is empty but its subtree is not) with the SAME `hasLiveLease`/`supervisorDead`/age fakes as case 1 (i.e., it WOULD have matched v1's flawed release logic) → `ReleaseStaleGrantedLeases` must NOT release it: the `release` callback must NOT be called, the directory must still exist, and it should appear in `Skipped` (or an equivalent "attempted but not empty" outcome) — kill the child process and clean up the fixture at test end regardless of outcome.
3. A scope where `hasLiveLease` reports false (no lease held) → not a candidate at all, `release` never called (this is `staleGrantedLeaseCandidates`'s job from Task 1, but confirm the wiring here too).
4. A scope too young (age < grace) → not released even though otherwise heuristic-matching.

- [ ] **Step 2: Run to verify failure** — expect FAIL (undefined function / or case 2 failing against a naive implementation if you write the naive version first to confirm the test actually catches the v1 bug class before writing the real fix — recommended, since this is the single most important test in this entire plan).

- [ ] **Step 3: Implement**

```go
// reapConfineScopeByID attempts to physically remove ONE named scope's
// directory tree via the exact fd-anchored, kernel-enforced removal every
// other reap path in this file uses. Extracted from
// reapOrphanedConfineScopesWithDeps so ReleaseStaleGrantedLeases (below)
// gets the identical safety property with zero duplicated logic: success
// is authoritative, fresh, subtree-aware proof of emptiness (the kernel
// itself refuses AT_REMOVEDIR on anything non-empty); failure just means
// "not empty (yet)", never an error to escalate.
func reapConfineScopeByID(parentFD int, scopeID string, afterEmptyProof func()) (bool, error) {
	if !validConfineScopeID(scopeID) {
		return false, fmt.Errorf("invalid scope id")
	}
	return reapEmptyConfineScopeTree(parentFD, ".aira-"+scopeID, afterEmptyProof)
}
```

Update `reapOrphanedConfineScopesWithDeps`'s existing loop body to call `reapConfineScopeByID(parentFD, record.ScopeID, deps.afterReapEmptyProof)` instead of its current inlined `childName := ".aira-" + record.ScopeID; reapEmptyConfineScopeTree(parentFD, childName, ...)` — pure deduplication, no behavior change (add/keep a test confirming this).

```go
// ReleaseStaleGrantedLeases finds ledger-granted leases whose scope matches
// the coarse "might be dead" heuristic (staleGrantedLeaseCandidates, Task 1),
// then for each one ATTEMPTS THE ACTUAL PHYSICAL REAP of its scope directory
// via reapConfineScopeByID -- the same kernel-enforced removal the ordinary
// orphan-scope reaper already relies on. release(scopeID) is invoked ONLY
// when that physical reap actually succeeds: a successful Unlinkat is
// authoritative, TOCTOU-immune, subtree-aware proof the scope was genuinely,
// fully empty at that instant, regardless of whether the coarse heuristic's
// leaf-only Populated field or kill(pid,0)'s PID-namespace assumptions were
// individually trustworthy -- a wrong heuristic match just costs one failed,
// harmless reap attempt. release is called synchronously, in scope-id order,
// before this function returns.
func ReleaseStaleGrantedLeases(ctx context.Context, slicePath string, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool, release func(scopeID string)) (ConfineReapResult, error) {
	listed, err := listConfinesWithDeps(ctx, slicePath, nil, defaultConfineScanDeps())
	if err != nil {
		return ConfineReapResult{}, err
	}
	if listed.Verdict == "unevaluated" {
		return ConfineReapResult{Verdict: "unevaluated", Reason: listed.Reason, Reaped: []string{}}, nil
	}
	if supervisorDead == nil {
		supervisorDead = pidIsDead
	}
	candidates := staleGrantedLeaseCandidates(listed.Scopes, grace, supervisorDead, hasLiveLease)
	result := ConfineReapResult{Verdict: "pass", Reaped: []string{}}
	if len(candidates) == 0 {
		return result, nil
	}
	parentFD, err := unix.Open(slicePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return result, fmt.Errorf("open confine slice for stale-lease release: %w", err)
	}
	defer unix.Close(parentFD)
	for _, record := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		reaped, reapErr := reapConfineScopeByID(parentFD, record.ScopeID, nil)
		if reapErr != nil || !reaped {
			result.Skipped++
			continue
		}
		if release != nil {
			release(record.ScopeID)
		}
		result.Reaped = append(result.Reaped, record.ScopeID)
	}
	return result, nil
}
```

Note the important difference in call ORDER from the reasoning above: physical reap happens BEFORE the release callback, and release only fires on a confirmed-successful reap — this is the crux of the whole redesign, do not reorder it.

- [ ] **Step 4: Run to verify pass** — `AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner/... -run 'ReleaseStaleGrantedLeases|ReapOrphanedConfineScopes' -v`. Case 2 above (live nested child) passing is the single most important assertion in this plan — if it doesn't exist or doesn't fail against a naive Populated-trusting implementation, this task is not actually done regardless of what else passes.

- [ ] **Step 5: Commit** — `git commit -m "fix(runner): reap-first-release-second stale confine lease recovery (AIRA-49)"`

## Task 3: Wire the daemon-side pass into the existing reaper loop

**Files:**
- Modify: `internal/daemon/confine_reaper.go`
- Test: `internal/daemon/confine_reaper_test.go` (new, or extend an existing one if `reapOrphanedScopesPass` already has `*Server`-level tests — check first, and reuse whatever fixture constructs a `*Server` with a fake/injectable confine slice path; if `runner.ResolveConfineManagementSlice("")` truly cannot be overridden for a test double, that is itself a real gap this task must either close with a minimal seam or explicitly document as an accepted, deliberate real-cgroup-only test boundary matching how `reapOrphanedScopesPass` itself is presumably already tested — find out which before writing new tests, do not assume).

**Interfaces:**
- Consumes: `runner.ReleaseStaleGrantedLeases` (Task 2), `s.activeConfines(path)` (existing), `s.releaseActiveConfine` (existing, passed as the `release` callback), `s.scopeReapGrace`/`defaultScopeReapGrace` (existing).
- Produces: `func (s *Server) releaseStaleGrantedLeasesPass(ctx context.Context)`.

- [ ] **Step 1: Write the failing test first**

Whatever test scaffolding this needs, get the EXACT existing fixture-construction pattern for `*Server` + fake confine slice + a real `admitGranted` waiter from the daemon package's own existing admit/reaper tests before writing anything new (do not hand-roll a second way to do this). Cover:
1. A granted lease whose scope was genuinely, physically reaped by the underlying call → `s.releaseActiveConfine` was invoked with that scope ID (assert via `s.activeConfines(path)` no longer listing it, matching v1's original assertion style, which is still valid here).
2. `s.scopeReapGrace` left at its zero value must use `defaultScopeReapGrace` (2 minutes), NOT zero — do not repeat v1's test-comment error claiming `scopeReapGrace=0` means "no grace wait"; if a test needs a short grace for speed, set `s.scopeReapGrace` to an explicit small nonzero duration and account for it, or use a fixture whose `AgeSeconds` is already comfortably past whatever grace is actually in effect.
3. `releaseStaleGrantedLeasesPass` then `reapOrphanedScopesPass` called back to back (mirroring `runScopeReaper`'s real ordering) is no longer required to prove the directory disappears (Task 2's own real-cgroup test already proves that at the `runner` layer) — this daemon-level test only needs to prove the WIRING: that the daemon supplies the right path/grace/registry-derived `hasLiveLease` to `runner.ReleaseStaleGrantedLeases` and that its `release` callback is really `s.releaseActiveConfine`. Keep this test at the plumbing level; do not re-derive Task 2's kernel-behavior coverage here.

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

```go
func (s *Server) releaseStaleGrantedLeasesPass(ctx context.Context) {
	_, path, err := runner.ResolveConfineManagementSlice("")
	if err != nil {
		return
	}
	grace := s.scopeReapGrace
	if grace <= 0 {
		grace = defaultScopeReapGrace
	}
	registry := s.activeConfines(path)
	if len(registry) == 0 {
		return
	}
	hasLiveLease := func(scopeID string) bool {
		for _, entry := range registry {
			if entry.ScopeID == scopeID {
				return true
			}
		}
		return false
	}
	result, err := runner.ReleaseStaleGrantedLeases(ctx, path, grace, nil, hasLiveLease, func(scopeID string) {
		s.releaseActiveConfine(path, scopeID)
	})
	if err != nil {
		log.Printf("aira daemon: scope-reaper: stale-lease release sweep error: %v", err)
		return
	}
	if len(result.Reaped) > 0 {
		log.Printf("aira daemon: scope-reaper: released %d stale confine lease(s) (confirmably-dead supervisor, physically reaped): %s", len(result.Reaped), strings.Join(result.Reaped, ", "))
	}
}
```

Wire into `runScopeReaper`, immediately before the existing reap call:

```go
		s.releaseStaleGrantedLeasesPass(ctx)
		s.reapOrphanedScopesPass(ctx)
```

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Full confined verification** — `aira confine -- make ci`, plus `AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner/... ./internal/daemon/...`

- [ ] **Step 6: Commit** — `git commit -m "fix(daemon): wire stale-lease release into the scope-reaper loop (AIRA-49)"`

## Deferred / explicitly out of scope

- Improving `ConfineKill`'s `U_CONFINE_NOT_LAUNCHED` message for the transient case: still deferred, same reasoning as v1 — this fix means a manual retry loop is no longer the only recovery path (worst case ~7 minutes, not forever).
- Hardening the admit-connection-close release path for delegate-ram: still deferred, same reasoning as v1 — this sweep is now a correctness backstop regardless of that path's behavior, not a workaround for it.
