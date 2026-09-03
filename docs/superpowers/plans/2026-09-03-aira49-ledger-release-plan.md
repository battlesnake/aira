# AIRA-49: Release stuck confine ledger leases on confirmed-dead supervisors — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a confine job's owning process dies without releasing its admission lease (external SIGKILL of the wrapping `aira confine` launcher, or any other non-graceful death), the daemon must eventually release the stuck ledger reservation on its own, using the same positive-proof evidence the existing orphaned-scope reaper already trusts — never leaving a permanently unrecoverable reservation, and never requiring a daemon restart to clear it.

**Architecture:** Extract the existing orphan-proof predicate (populated==0 AND supervisor confirmably dead AND age past grace AND not mid-launch) out of `orphanedConfineScopeCandidates` into a small shared helper, then add its logical inverse: `StaleGrantedLeaseCandidates`, which finds ledger-granted leases that meet that SAME proof but whose supervisor is dead (rather than finding directories with NO lease, which is what the existing reaper already does). A new daemon-side sweep pass calls this before the existing scope-reaper pass, in the same periodic loop, releasing any proven-stale lease via the already-existing `releaseActiveConfine` — so the very next reaper pass in the same tick can then remove the now-lease-free directory through the existing, already-hardened path. No new proof machinery, no new removal machinery — only a new consumer of proof that already exists and was already two-loop-hardened for AIRA-36/#72.

**Tech Stack:** Go (`internal/runner`, `internal/daemon`), existing `pytest`/`go test` toolchains unaffected.

**Spec:** Ticket AIRA-49 (`.aira/tickets/AIRA-49.md`) carries the full root-cause analysis this plan implements: `internal/runner/confine_manage_linux.go`'s `ConfineKill` cannot distinguish "still launching" from "owner confirmably dead, permanently empty"; nothing periodically re-validates a granted waiter's owning PID; the orphan reaper explicitly (and correctly, for its own purpose) treats any granted lease as proof of life and skips it. This plan closes the gap at its root: nothing was ever releasing the stale lease itself.

## Global Constraints

- No cgo; one static Go binary (existing project-wide rule, unaffected by this change).
- Every heavy command (`go build`, `go test`, `go vet`) MUST be run via `aira confine --`.
- Correctness-critical work: this plan's two tasks below are both TDD, and the whole change goes through this project's mandatory two-loop (plan-review, Fable gate, implement, build-review, Fable/build gate) before merge — do not skip review steps to save time.
- Reuse existing primitives; do not duplicate the orphan-proof logic. If a task's tests would require re-deriving the proof conditions from scratch instead of calling the shared helper, that is a sign the extraction in Task 1 was not done correctly — stop and fix Task 1 first.
- Grace period: reuse the daemon's existing `scopeReapGrace` (`internal/daemon/paths.go`'s `defaultScopeReapGrace`, 2 minutes) for lease release too. Do not invent a second magic number — the proof evidence backing a release decision here is identical in kind to the proof already trusted, at the same age threshold, for physically removing a directory (see the "Known risk to flag in review" note in Task 2).

---

## File Structure

- `internal/runner/confine_manage.go` — add the shared `isOrphanProven` predicate (extracted from the existing `orphanedConfineScopeCandidates`) and the new exported `StaleGrantedLeaseCandidates`. This file already owns `orphanedConfineScopeCandidates` and `ConfineOwnerLookup`, so the new sibling function belongs here, not in a new file.
- `internal/runner/confine_manage_test.go` — new table-driven tests for `isOrphanProven` (via its two callers' behavior) and `StaleGrantedLeaseCandidates` directly. (Check whether this test file already exists; if the existing tests for `orphanedConfineScopeCandidates` live in a different file such as `confine_manage_linux_test.go`, add the new tests alongside them there instead, to keep proof-predicate tests together.)
- `internal/daemon/confine_reaper.go` — add `releaseStaleGrantedLeasesPass`, wired into `runScopeReaper`'s loop immediately before the existing `reapOrphanedScopesPass` call.
- `internal/daemon/confine_reaper_test.go` (new, or an existing daemon reaper test file if one already covers `reapOrphanedScopesPass` at the `*Server` level — check first) — integration-style test(s) proving: a granted waiter whose backing scope is orphan-proven gets released, and in the SAME pass its directory is then reaped; a granted waiter that is NOT yet orphan-proven (too young, still populated, or supervisor still alive) is left untouched.

## Task 1: Extract the shared orphan-proof predicate and add its inverse

**Files:**
- Modify: `internal/runner/confine_manage.go` (the `orphanedConfineScopeCandidates` function, lines ~65-87 as of this plan's writing)
- Test: wherever `orphanedConfineScopeCandidates`'s existing tests currently live (find them first — likely `internal/runner/confine_manage_linux_test.go` or `confine_manage_test.go`)

**Interfaces:**
- Produces: `isOrphanProven(record ConfineRecord, graceSeconds int64, supervisorDead func(pid int) bool) bool` (unexported — same-package helper) and `StaleGrantedLeaseCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord` (exported — called cross-package from `internal/daemon`, mirroring how `ReapOrphanedConfineScopes` is already called cross-package).
- Consumes: nothing new — `ConfineRecord` (existing, `confine_manage.go`) and `pidIsDead` (existing, `confine_manage_linux.go`, used as the default `supervisorDead` exactly like `ReapOrphanedConfineScopes` already defaults it).

- [ ] **Step 1: Write the failing tests first**

Read the existing tests for `orphanedConfineScopeCandidates` before writing new ones — match their existing style/fixture-building helpers exactly (do not invent a second way to build a `ConfineRecord` fixture if one already exists in that test file).

Add these cases (adapt exact helper names to whatever the existing file already uses for constructing `ConfineRecord`/`grace`/fake `supervisorDead`/`hasLiveLease` functions):

```go
func TestStaleGrantedLeaseCandidatesRequiresTheSameOrphanProofAsTheDirectoryReaper(t *testing.T) {
	grace := 2 * time.Minute
	deadPID := func(pid int) bool { return pid == 111 }
	hasLease := func(scopeID string) bool { return scopeID == "granted-and-dead" || scopeID == "granted-but-alive" }

	populatedZero := 0
	populatedOne := 1
	oldEnough := int64(200) // seconds, > 120s grace
	tooYoung := int64(10)
	pid := 111
	alivePID := 222

	records := []ConfineRecord{
		{ScopeID: "granted-and-dead", SupervisorPID: &pid, Populated: &populatedZero, AgeSeconds: &oldEnough},
		{ScopeID: "granted-but-alive", SupervisorPID: &alivePID, Populated: &populatedZero, AgeSeconds: &oldEnough},
		{ScopeID: "granted-but-too-young", SupervisorPID: &pid, Populated: &populatedZero, AgeSeconds: &tooYoung},
		{ScopeID: "granted-but-still-populated", SupervisorPID: &pid, Populated: &populatedOne, AgeSeconds: &oldEnough},
		{ScopeID: "dead-and-old-but-no-lease", SupervisorPID: &pid, Populated: &populatedZero, AgeSeconds: &oldEnough},
	}

	got := StaleGrantedLeaseCandidates(records, grace, deadPID, hasLease)

	if len(got) != 1 || got[0].ScopeID != "granted-and-dead" {
		t.Fatalf("candidates=%+v, want exactly [granted-and-dead]", got)
	}
}

func TestStaleGrantedLeaseCandidatesFailsClosedOnUnknownFacets(t *testing.T) {
	grace := 2 * time.Minute
	deadPID := func(int) bool { return true }
	hasLease := func(string) bool { return true }
	oldEnough := int64(200)
	pid := 111

	for name, record := range map[string]ConfineRecord{
		"nil populated":   {ScopeID: "x", SupervisorPID: &pid, Populated: nil, AgeSeconds: &oldEnough},
		"nil supervisor":  {ScopeID: "x", SupervisorPID: nil, Populated: intPtr(0), AgeSeconds: &oldEnough},
		"nil age":         {ScopeID: "x", SupervisorPID: &pid, Populated: intPtr(0), AgeSeconds: nil},
		"pending":         {ScopeID: "x", SupervisorPID: &pid, Populated: intPtr(0), AgeSeconds: &oldEnough, Pending: true},
	} {
		t.Run(name, func(t *testing.T) {
			got := StaleGrantedLeaseCandidates([]ConfineRecord{record}, grace, deadPID, hasLease)
			if len(got) != 0 {
				t.Fatalf("%s: candidates=%+v, want none (fail-closed on unknown/mid-launch facets)", name, got)
			}
		})
	}
}

func TestStaleGrantedLeaseCandidatesReturnsNoneWithoutASupervisorDeadOrHasLiveLeaseFunc(t *testing.T) {
	oldEnough := int64(200)
	pid := 111
	record := ConfineRecord{ScopeID: "x", SupervisorPID: &pid, Populated: intPtr(0), AgeSeconds: &oldEnough}

	if got := StaleGrantedLeaseCandidates([]ConfineRecord{record}, 2*time.Minute, nil, func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("nil supervisorDead: candidates=%+v, want none", got)
	}
	if got := StaleGrantedLeaseCandidates([]ConfineRecord{record}, 2*time.Minute, func(int) bool { return true }, nil); len(got) != 0 {
		t.Fatalf("nil hasLiveLease: candidates=%+v, want none", got)
	}
}
```

Add a small `intPtr` test helper if this test file does not already have an equivalent (check first — `orphanedConfineScopeCandidates`'s existing tests very likely already have one under a different name; reuse it rather than adding a duplicate).

Also add a regression test proving `orphanedConfineScopeCandidates`'s own existing behavior is UNCHANGED by the extraction — re-run (or confirm still present and passing) its existing test(s) verbatim; do not weaken or remove any existing case.

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `aira confine -- go test ./internal/runner/... -run 'TestStaleGrantedLeaseCandidates' -v`
Expected: FAIL with "undefined: StaleGrantedLeaseCandidates" (the function does not exist yet).

- [ ] **Step 3: Implement the extraction + new function**

In `internal/runner/confine_manage.go`, replace the existing `orphanedConfineScopeCandidates` with:

```go
// isOrphanProven is the single positive-proof gate both orphan reaping
// (directory removal) and stale lease release (ledger cleanup) require:
// the scope is empty, its recorded supervisor PID is confirmably dead, it
// is past the grace window (never mid-launch), and it was never a
// registry-only Pending placeholder. Any unknown facet fails closed.
func isOrphanProven(record ConfineRecord, graceSeconds int64, supervisorDead func(pid int) bool) bool {
	return record.Populated != nil && *record.Populated == 0 &&
		record.SupervisorPID != nil && supervisorDead(*record.SupervisorPID) &&
		record.AgeSeconds != nil && *record.AgeSeconds >= graceSeconds &&
		!record.Pending
}

// orphanedConfineScopeCandidates requires positive proof for every orphan
// facet. Unknown population, supervisor, or age state is never a candidate. A
// scope with a live daemon admit lease (hasLiveLease) is NEVER a candidate: that
// is the authoritative, PID-namespace-independent liveness signal — kill(pid,0)
// alone can misjudge a supervisor whose scope-id PID is namespace-local.
func orphanedConfineScopeCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord {
	graceSeconds := int64(grace / time.Second)
	candidates := make([]ConfineRecord, 0)
	if supervisorDead == nil {
		return candidates
	}
	for _, record := range records {
		if !isOrphanProven(record, graceSeconds, supervisorDead) ||
			(hasLiveLease != nil && hasLiveLease(record.ScopeID)) {
			continue
		}
		candidates = append(candidates, record)
	}
	return candidates
}

// StaleGrantedLeaseCandidates is orphanedConfineScopeCandidates's mirror
// image (AIRA-49): instead of finding scopes with NO ledger lease that are
// safe to physically reap, it finds scopes that STILL HOLD a granted ledger
// lease (hasLiveLease is true) despite meeting the exact same positive-proof
// death evidence. A lease that never transitions out of "granted" — because
// its owning process died via anything other than a clean graceful exit or a
// successful ConfineKill (external SIGKILL of the wrapping launcher, e.g.) —
// is otherwise permanently unrecoverable: orphanedConfineScopeCandidates
// will never select it (a live lease is proof of life BY DESIGN there), and
// ConfineKill's own empty-scope path returns a "retry" error that can never
// resolve, since nothing will ever populate the scope again. The caller is
// expected to release each returned candidate's lease (e.g. via
// releaseActiveConfine) and let the very next orphan-reap pass remove its
// now-lease-free directory through the existing, already-hardened path —
// this function only identifies candidates, it does not release anything
// itself and has no side effects.
func StaleGrantedLeaseCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord {
	graceSeconds := int64(grace / time.Second)
	candidates := make([]ConfineRecord, 0)
	if supervisorDead == nil || hasLiveLease == nil {
		return candidates
	}
	for _, record := range records {
		if !isOrphanProven(record, graceSeconds, supervisorDead) || !hasLiveLease(record.ScopeID) {
			continue
		}
		candidates = append(candidates, record)
	}
	return candidates
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `aira confine -- go test ./internal/runner/... -run 'OrphanedConfineScopeCandidates|StaleGrantedLeaseCandidates' -v`
Expected: PASS, all cases including the pre-existing `orphanedConfineScopeCandidates` tests (unchanged behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/runner/confine_manage.go internal/runner/*_test.go
git commit -m "refactor(runner): extract shared orphan-proof predicate, add StaleGrantedLeaseCandidates (AIRA-49)"
```

## Task 2: Wire a daemon-side release pass into the existing reaper loop

**Files:**
- Modify: `internal/daemon/confine_reaper.go`
- Test: `internal/daemon/confine_reaper_test.go` if it exists at the `*Server`-level already (check first — search for existing tests calling `reapOrphanedScopesPass` or constructing a `*Server` with a live `admitQueues` fixture and a real/fake confine slice directory); otherwise create it, matching whatever fixture-building pattern the existing daemon admit tests already use (e.g. `admitTestServer`, seen in `internal/daemon/admit_cache_discount_test.go`).

**Interfaces:**
- Consumes: `runner.StaleGrantedLeaseCandidates` (Task 1), `s.activeConfines(path)` (existing, `internal/daemon/confine_manage.go`), `s.releaseActiveConfine(path, scopeID)` (existing, same file), `runner.ListConfines(ctx, slicePath, registry) (runner.ConfineListResult, error)` (existing, `internal/runner/confine_manage.go`), `runner.ResolveConfineManagementSlice("")` (existing, used identically by `reapOrphanedScopesPass`), `s.scopeReapGrace`/`defaultScopeReapGrace` (existing).
- Produces: `func (s *Server) releaseStaleGrantedLeasesPass(ctx context.Context)` — no return value, logs what it released, matching `reapOrphanedScopesPass`'s own shape exactly.

**Known risk to flag explicitly at plan-review and build-review (do not silently resolve it — a reviewer must weigh in):** `orphanedConfineScopeCandidates`'s own doc comment warns that `kill(pid,0)` ("kill(pid,0) alone") "can misjudge a supervisor whose scope-id PID is namespace-local" — which is exactly why that function ALSO requires `!hasLiveLease` as an additional, "authoritative, PID-namespace-independent" signal before trusting a `kill(pid,0)`-based dead verdict for the *reap* direction. `StaleGrantedLeaseCandidates` selects records where `hasLiveLease` is TRUE (that is its entry criterion, by definition) — so for the *release* direction, `kill(pid,0)` is the ONLY liveness signal available; there is no second, independent signal analogous to what protects the reap direction. Confirm before merge (a plan-review question, not something to resolve unilaterally in this task) whether AIRA's actual deployment model ever runs a confine supervisor in an isolated PID namespace (containers, `unshare`, etc.) — if it does not (this codebase's confine launch path forks/execs directly, no PID-namespace isolation found anywhere in `internal/runner` as of this plan's writing), `kill(pid,0)` returning ESRCH is authoritative on its own (no process with that PID exists anywhere the daemon's own namespace can see, and the daemon and its confine children always share one PID namespace) and this is safe; if such isolation is ever introduced, this function would need its own independent confirmation signal before it could safely be trusted, the same way the reap direction already has one.

- [ ] **Step 1: Write the failing test first**

```go
func TestReleaseStaleGrantedLeasesPassReleasesAConfirmablyDeadSupervisorsLease(t *testing.T) {
	dir := t.TempDir()
	scopeID := "TESTDEAD-000000-abc123"
	scopeDir := filepath.Join(dir, ".aira-"+scopeID)
	if err := os.MkdirAll(scopeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// populate the interface files a real listConfinesWithDeps scan needs;
	// mirror whatever the existing reaper tests already use to fake a
	// filesystem-backed confine scope (find that helper and reuse it here
	// rather than hand-rolling a second one).

	server := newTestServerWithAdmitQueue(t, dir) // or whatever the existing
	// admit-test fixture constructor is named -- see admit_cache_discount_test.go
	grantAdmitWaiterForTest(t, server, dir, scopeID, "owner-x") // sets state
	// admitGranted for scopeID on the queue for `dir`; find/reuse the
	// existing granting helper other admit tests already use rather than
	// reaching into unexported state directly from a new helper.

	server.scopeReapGrace = 0 // no need to wait out a real grace window in the test

	server.releaseStaleGrantedLeasesPass(context.Background())

	if len(server.activeConfines(dir)) != 0 {
		t.Fatalf("lease still active after release pass, want released")
	}
}

func TestReleaseStaleGrantedLeasesPassLeavesALiveSupervisorsLeaseAlone(t *testing.T) {
	// same shape as above, but the fixture's recorded supervisor PID is
	// os.Getpid() (genuinely alive) instead of a PID confirmed via ESRCH --
	// assert activeConfines still reports it after the pass.
}

func TestReleaseStaleGrantedLeasesPassThenReapPassRemovesTheDirectoryInTheSameTick(t *testing.T) {
	// same dead-supervisor fixture as the first test; call
	// releaseStaleGrantedLeasesPass then reapOrphanedScopesPass back to
	// back (mirroring runScopeReaper's own ordering) and assert the scope
	// directory no longer exists on disk afterward -- the end-to-end
	// property this whole plan exists to deliver.
}
```

Before writing these, actually read `internal/daemon/confine_reaper_test.go` (if it exists) and the admit test fixtures in `internal/daemon/admit_cache_discount_test.go`/`admit_test.go` to find the real helper names for: building a `*Server` with a fake confine slice path, granting an `admitWaiter` into `admitGranted` state for a specific scope, and faking a real-cgroup-shaped directory scan (`listConfinesWithDeps` likely takes injectable deps for testing without real cgroupfs — find and reuse that seam rather than requiring `AIRA_REAL_CGROUP=1`). The exact code above is illustrative of intent and coverage, not literal — match this codebase's existing test-fixture conventions precisely.

- [ ] **Step 2: Run to verify failure**

Run: `aira confine -- go test ./internal/daemon/... -run 'TestReleaseStaleGrantedLeasesPass' -v`
Expected: FAIL — `releaseStaleGrantedLeasesPass` does not exist yet.

- [ ] **Step 3: Implement**

In `internal/daemon/confine_reaper.go`:

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
	listed, err := runner.ListConfines(ctx, path, registry)
	if err != nil || listed.Verdict == "unevaluated" {
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
	for _, record := range runner.StaleGrantedLeaseCandidates(listed.Scopes, grace, nil, hasLiveLease) {
		s.releaseActiveConfine(path, record.ScopeID)
		pid := 0
		if record.SupervisorPID != nil {
			pid = *record.SupervisorPID
		}
		log.Printf("aira daemon: scope-reaper: released stale granted lease (confirmably-dead supervisor pid=%d): scope=%s", pid, record.ScopeID)
	}
}
```

Wire it into `runScopeReaper` immediately before the existing reap call:

```go
		s.releaseStaleGrantedLeasesPass(ctx)
		s.reapOrphanedScopesPass(ctx)
```

(This is the only change to `runScopeReaper` itself — one new line, same position both passes already share: inside the existing `for { ... }` loop, before the existing `timer` wait.)

- [ ] **Step 4: Run to verify pass**

Run: `aira confine -- go test ./internal/daemon/... -run 'TestReleaseStaleGrantedLeasesPass' -v`
Expected: PASS, all three cases.

- [ ] **Step 5: Full confined verification**

Run: `aira confine -- make ci`
Expected: exit 0, every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/confine_reaper.go internal/daemon/*_test.go
git commit -m "fix(daemon): release stuck confine ledger leases on confirmably-dead supervisors (AIRA-49)"
```

## Deferred / explicitly out of scope for this plan

- Improving `ConfineKill`'s `U_CONFINE_NOT_LAUNCHED` message for the transient (still-launching) case is NOT in this plan. Once this fix ships, a scope that WOULD have hit that message forever now gets released automatically within one `scopeReapGrace` window (2 minutes) by the new background pass, so a manual `--kill --steal` retry loop is no longer the only path to recovery — the message itself staying generic is an acceptable, much lower-severity residual UX nit, not the resource-leak correctness bug this plan closes. Ticket separately if it still bothers someone after this lands.
- Hardening the admit-connection-close release path for delegate-ram specifically (candidate fix (c) from AIRA-49's ticket) is NOT in this plan — this plan's backstop sweep makes that hardening a pure optimization (faster recovery than waiting for the sweep) rather than a correctness requirement, so it is deferred rather than blocking this fix.
