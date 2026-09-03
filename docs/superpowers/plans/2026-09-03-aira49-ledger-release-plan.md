# AIRA-49: Reclaim stuck confine ledger leases past a lease-TTL — Implementation Plan (v5)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a confine job's owning process dies without releasing its admission lease, the daemon must eventually reclaim the stuck ledger reservation on its own — never leaving it permanently unrecoverable, never requiring a daemon restart — via a policy whose consequences are honestly documented, not a claim of having *proven* the owner dead (a claim this project's own review process has now shown, twice, cannot actually be made from any signal available here).

**Architecture (v5 — four plan-review rounds, three BLOCKs and one PASS-WITH-FIXES; see "What changed" below):** Every granted admission lease is, from this point on, understood to carry an implicit reclaim policy — precisely: **minimum grant age plus empty-at-reclaim-time** (Sol round-5's more accurate phrasing; "maximum unused lifetime" wrongly suggests continuous non-use is tracked, which it is not — only the two point-in-time facts below are). If `staleLeaseReleaseGrace` (default 15 minutes) has elapsed since `grantedAt` AND the scope is found empty at the moment of the sweep, the daemon reclaims it — a lease-TTL policy, not a liveness proof. A background sweep, run across **every currently registered admission-queue slice** (not just the default one — see Task 3), finds `admitGranted` waiters whose lease was GRANTED (not merely enqueued — see below) at least `staleLeaseReleaseGrace` ago. For each, it attempts to physically remove that scope's cgroup directory tree via the exact fd-anchored, kernel-enforced removal (`reapEmptyConfineScopeTree`) the existing orphaned-scope reaper (AIRA-36/#72) already uses. **Only on a confirmed-successful physical removal** does the pass reclaim the ledger lease. Two facts gate the action: how long the ledger itself has held the GRANT (an in-process timestamp set at the exact moment of grant, immune to admission-queue contention), and whether the kernel itself just proved the scope is empty right now — but the plan explicitly does NOT claim these together prove the owner is dead (see v5's changelog entry for why that claim doesn't hold, and why the fix is honest framing plus a conservative default rather than a fifth attempt at a stronger proof that does not exist).

**What changed, across three review rounds:**

- **v1** released a lease based on a list-scan's `Populated` field (leaf-only `cgroup.procs`, not subtree-aware — misses a live NESTED child cgroup). Sol/Codex: BLOCK. Also found a TOCTOU race, an unresolved PID-namespace concern, a literal no-op bug, and unrunnable tests.
- **v2** fixed subtree-safety/TOCTOU by requiring the kernel's own `Unlinkat` to succeed before releasing (reap-first, release-on-success). Sol/Codex, round 2: still BLOCK — a kernel-confirmed-empty scope only proves emptiness *at that instant*, not that nothing will populate it later; the age signal v2 used (a scope directory's filesystem mtime) starts ticking from `confineScopeID`'s mint time, which happens *before* admission is even granted (`internal/runner/confine_linux.go`), so a job that merely queued for a while under contention could already look "stuck" the moment it launches. Fable independently found the identical class of gap reviewing the same v2, with exact line citations (`confine_linux.go:475` scope-ID mint before `:509` admit, scope created at `:535`, child placed at `:703`) and rendered PASS WITH FIXES with a long, precise list (reuse this file's *existing* real-cgroup reaper test fixtures by name rather than hand-rolling new ones; thread an `afterEmptyProof` seam through so a repopulation-after-proof race is actually testable; log releases made before an early ctx-cancel return; document the no-directory-lease and PID-reuse-liveness gaps explicitly; and — importantly — traced that a SIGKILLed launcher's connection-close *should already* release the lease via the existing `admitConnection`/`cancelPeer` path, `admit.go:452-521`, meaning the ORIGINAL reported stuck state's root cause is not actually fully explained by "nothing releases it" and needs a post-deploy repro check, not just a plausible-sounding narrative).
- **v3 (drafted, then superseded before a fourth review round)** dropped the list-scan/`kill(pid,0)` heuristic entirely in favor of the daemon's OWN `admitWaiter.enqueued` timestamp as the age signal. This turned out to have the SAME underlying flaw Fable had just found for v2, one field over: `enqueued` (`internal/daemon/admit.go:571`) is set once, at waiter CREATION — i.e. at the moment a request first arrives and is queued (`state: admitQueued`) — and is NEVER updated when the waiter later actually transitions to `admitGranted` (`admit.go:729`). A job that waited a long time in the admission queue under contention would therefore look old the instant it's granted, exactly the same "queueing delay masquerading as launch-abandonment" confusion Fable flagged for v2's scope-ID-mint-based age, just measured from a different starting point with the identical defect.
- **v4 added a genuinely new, dedicated `admitWaiter.grantedAt` timestamp**, set only at the exact moment `waiter.state = admitGranted` fires (`admit.go:729`) — the one moment in this whole system that authoritatively marks "the daemon just decided this job may proceed," closing the queueing-contention defect both Sol and Fable found (independently) at its true root. Sol's round-4 review confirmed this specific fix works ("no admission contention remains afterward") — but still BLOCKed on two further, narrower points: (1) `grantedAt` closes the *contention* defect but the plan's framing still implicitly claimed the kernel-empty-reap + age combination *proves* the owner is gone, which is not actually true — a live launcher can be legitimately `SIGSTOP`ed, cgroup-frozen, or stuck in an unbounded kernel/filesystem operation for longer than any fixed grace, and reclaiming its lease mid-pause would break a launch that was never actually abandoned; Sol's own suggested resolution: "either define this explicitly as a lease-expiry policy... or add a protocol/state transition" — v5 takes the first option, since it needs no new code and matches this system's own existing "lease" vocabulary exactly (a lease with a bounded lifetime that reclaims on non-use is the ordinary, well-understood semantics of a lease, not a novel concept requiring new machinery); (2) the sweep only ever resolved ONE default slice path (`ResolveConfineManagementSlice("")`), silently missing any lease admitted against an explicitly-specified `--slice` — v5 fixes this concretely (Task 3).
- **v5 (this plan)** reframes the grace period as an explicit, honestly-documented lease-TTL/reclaim policy rather than a liveness-detection claim (Global Constraints and Task 3's doc comment both state the SIGSTOP/paused-launcher tradeoff plainly — this is a deliberate, accepted policy consequence, not an oversight), and fixes the sweep to iterate every registered admission-queue slice, not just the default.

**Tech Stack:** Go (`internal/daemon`, `internal/runner`).

**Spec:** Ticket AIRA-49 (`.aira/tickets/AIRA-49.md`, original root-cause analysis) plus this plan's "What changed" section (three real review rounds' findings) — read both.

## Global Constraints

- No cgo; one static Go binary.
- Every heavy command (`go build`, `go test`, `go vet`) MUST be run via `aira confine --`.
- Correctness-critical work: full two-loop before merge.
- The physical removal MUST go through `reapEmptyConfineScopeTree` — do not write a second removal implementation.
- Do NOT use `kill(pid,0)` / any OS-level PID-liveness check anywhere in this feature. Three review rounds converged on this: the release decision needs no such check, only (a) `grantedAt` age and (b) kernel-confirmed physical removal.
- Do NOT use `admitWaiter.enqueued` for this feature's age gate — it measures time since a request was first submitted, not time since it was granted, and conflates ordinary admission-queue contention with launch abandonment. Use the new `grantedAt` field (Task 1) exclusively.
- `staleLeaseReleaseGrace`'s default (Task 2) is a NEW, dedicated constant — do not reuse `defaultScopeReapGrace` (2 minutes; a different mechanism, protected by the unrelated and already-safe `!hasLiveLease` gate).
- Before considering this plan "done," re-run (or arrange for fastest-ee-dc to re-run) the ORIGINAL reported repro (`aira confine --delegate-ram -- <long job>`, external-kill the wrapper, wait) against the deployed fix and confirm release actually happens within the expected bound. Fable's review traced that the ordinary connection-close release path (`admit.go:452-521`) *should* already have handled a plain SIGKILL of the launcher, which means the original stuck-lease report's true trigger is not fully explained by "nothing releases it" — this sweep is a correct and valuable backstop regardless of the original trigger's exact mechanism, but do not close this out as "root-caused and fixed" without that honest caveat and, where practical, a live confirmation.

---

## File Structure

- `internal/daemon/admit.go` — add `grantedAt time.Time` to `admitWaiter`; set it at the existing `waiter.state = admitGranted` transition (line ~729).
- `internal/runner/confine_manage_linux.go` — add `ReapScopeIfEmpty(slicePath, scopeID string, afterEmptyProof func()) (bool, error)`, wrapping the existing `reapEmptyConfineScopeTree` (open the slice fd, delegate, close). The `afterEmptyProof` parameter exists specifically so a test can inject a repopulation race between the empty-proof and the actual removal (mirroring whatever seam the existing orphan-reap tests already use for the identical purpose — reuse it, do not invent a second one).
- `internal/daemon/confine_reaper.go` — add `releaseStaleGrantedLeasesPass` and `staleGrantedLeases`, wired into `runScopeReaper`'s loop immediately before the existing `reapOrphanedScopesPass` call.
- `internal/daemon/paths.go` — add `defaultStaleLeaseReleaseGrace = 15 * time.Minute`.
- `internal/daemon/server.go` — add a `staleLeaseReleaseGrace time.Duration` field to `*Server`, mirroring `scopeReapGrace`'s exact existing field/init pattern.
- Tests:
  - `internal/runner/confine_manage_linux_test.go` — for `ReapScopeIfEmpty`. **Before writing anything, read this file's existing real-cgroup reaper test helpers and reuse them by name**: `reaperTestParentAndDeadPID`, `createReaperTestScope`, `mkdirReaperTestCgroup`, `startReaperTestSleeper` (waits for `populated 1` before returning — belt-and-braces against a vacuous pass), `stopReaperTestSleeper`, `assertReaperTestMissing`, `reaperTestDead`, `confineTestScopeID`, and use `TestReapOrphanedConfineScopesKeepsEmptySiblingOfLiveNestedBranch` (around line 472) as the template for the live-nested-child case, and `TestReapOrphanedConfineScopesRepopulationAfterProof` (around line 540) as the template for the `afterEmptyProof`-seam race case. Do not hand-roll new fixture helpers that duplicate these.
  - `internal/daemon/confine_reaper_test.go` (new, or extend an existing daemon-level reaper test file — check first) — for the daemon-side pass. Use `t.Setenv("AIRA_CONFINE_SLICE", cgrouptest.IsolatedScopeParent(t))` (this daemon package already imports `cgrouptest`, e.g. `admit_reconstruction_linux_test.go`) for a REAL confine slice path a real-cgroup test can target, and the hand-built `sliceQueue`/`admitWaiter{state: admitGranted, ...}` fixture pattern already used at `internal/daemon/confine_manage_test.go:39-50` for constructing a granted waiter — reuse both rather than reinventing them. Note `t.Setenv` forbids `t.Parallel`. Note also that `NewServer` sets `scopeReapGrace` to 2 minutes by default (`server.go:148`), not zero — irrelevant to THIS feature's own `staleLeaseReleaseGrace` field (which this plan introduces with its own explicit default), but be precise about which field a test is setting.

## Task 1: `grantedAt` — the authoritative, contention-immune age signal

**Files:**
- Modify: `internal/daemon/admit.go`
- Test: wherever this package's existing admit-grant tests live (find them first — likely covering the `admitQueued`→`admitGranted` transition already, e.g. near `TestEvaluateAdmitQueueDiscountsOnlyReclaimableFileLRU` or similar).

**Interfaces:**
- Produces: `admitWaiter.grantedAt time.Time` (new field), set exactly once, at the same point `waiter.state = admitGranted` is set.
- Consumes: `s.admitNowTime()` (existing, already used for `enqueued`/`waitedMS` — use it for consistency and test-clock-overridability, not `time.Now()` directly).

- [ ] **Step 1: Write the failing test first**

```go
func TestGrantSetsGrantedAtDistinctFromEnqueuedUnderQueueingDelay(t *testing.T) {
	// Construct whatever this package's existing admit-evaluation test
	// fixture is (reuse it, do not hand-roll a new one), enqueue a waiter,
	// advance the fake clock (if this package's admitNowTime is
	// test-overridable -- check first) or otherwise simulate a queueing
	// delay before the waiter is actually granted, then grant it. Assert
	// waiter.grantedAt is at (or very close to) the grant moment, NOT
	// waiter.enqueued, and that grantedAt.Sub(enqueued) reflects the
	// simulated queueing delay (i.e. they are genuinely different values
	// under contention, not just two names for the same timestamp).
}
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Add the field to the `admitWaiter` struct (`admit.go`, near `enqueued`):

```go
	grantedAt    time.Time
```

At the existing grant transition:

```go
		waiter.state = admitGranted
		waiter.grantedAt = s.admitNowTime()
		waiter.accounted = true
```

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(daemon): track admitWaiter.grantedAt distinct from enqueue time (AIRA-49)"`

## Task 2: `ReapScopeIfEmpty` — single-scope kernel-enforced removal, with a testable repopulation seam

**Files:**
- Modify: `internal/runner/confine_manage_linux.go`
- Modify: `internal/runner/confine_manage_stub.go` — **required, not optional** (Sol round-5: caught as a build-break, not a style nit). `internal/daemon/confine_reaper.go` (Task 3) is NOT Linux-gated and calls `runner.ReapScopeIfEmpty` unconditionally — every other cross-platform daemon file that calls into this package's Linux-only real implementations (`ReapOrphanedConfineScopes`, `ResolveConfineManagementSlice`, etc.) has a matching stub here for `!linux` builds; add `ReapScopeIfEmpty`'s stub in the same style (`return false, errors.New("E_CONFINE_UNAVAILABLE: confine management requires Linux")`), or every non-Linux build of this repo fails with an undefined symbol. Verify with a non-Linux cross-compile (`GOOS=darwin aira confine -- go build ./...` or equivalent) as part of this task's own verification, not just `go build ./...` on this machine.
- Test: `internal/runner/confine_manage_linux_test.go`, reusing the exact fixture helpers named in "File Structure" above.

**Interfaces:**
- Produces: `func ReapScopeIfEmpty(slicePath, scopeID string, afterEmptyProof func()) (bool, error)`.
- Consumes: `reapEmptyConfineScopeTree` (existing, unchanged — it already accepts an `afterEmptyProof func()` parameter; thread this function's own parameter straight through, do not swallow it as `nil`), `validConfineScopeID` (existing).

- [ ] **Step 1: Write the failing tests first**

```go
func TestReapScopeIfEmptyRemovesAGenuinelyEmptyScope(t *testing.T) {
	// Using createReaperTestScope / this file's existing real-cgroup
	// convention: a real delegated cgroup-v2 directory, no live process
	// anywhere in it. ReapScopeIfEmpty(slicePath, scopeID, nil) -> (true,
	// nil), directory actually gone from disk afterward.
}

func TestReapScopeIfEmptyLeavesALiveNestedChildAlone(t *testing.T) {
	// Mirror TestReapOrphanedConfineScopesKeepsEmptySiblingOfLiveNestedBranch's
	// fixture exactly: startReaperTestSleeper in a CHILD cgroup one level
	// under the candidate scope (so the candidate's own leaf cgroup.procs
	// is empty but its subtree is not). ReapScopeIfEmpty -> (false, ...),
	// directory still exists. stopReaperTestSleeper + cleanup regardless
	// of outcome.
}

func TestReapScopeIfEmptyDoesNotRemoveAScopeRepopulatedAfterTheEmptyProof(t *testing.T) {
	// Mirror TestReapOrphanedConfineScopesRepopulationAfterProof exactly,
	// via the new afterEmptyProof parameter: inject a live process into
	// the scope from inside the callback, AFTER the empty check has
	// already passed but BEFORE the actual Unlinkat. Assert the kernel
	// itself still correctly refuses removal -- (false, ...), directory
	// (and the injected process) still present. This is the direct proof
	// that "reap succeeded" really is TOCTOU-immune, not merely
	// TOCTOU-unlikely.
}

func TestReapScopeIfEmptyRejectsAnInvalidScopeID(t *testing.T) {
	ok, err := ReapScopeIfEmpty(t.TempDir(), "../not-a-real-scope-id", nil)
	if ok || err == nil {
		t.Fatalf("got (%v, %v), want a rejection", ok, err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner/... -run 'TestReapScopeIfEmpty' -v`

- [ ] **Step 3: Implement**

```go
// ReapScopeIfEmpty attempts to physically remove ONE named scope's
// directory tree via the exact fd-anchored, kernel-enforced removal every
// other reap path in this file uses (reapEmptyConfineScopeTree). Success
// is authoritative, fresh, subtree-aware, TOCTOU-immune proof the scope
// was genuinely, fully empty at removal time -- the kernel itself refuses
// Unlinkat(AT_REMOVEDIR) on anything non-empty, anywhere in the subtree,
// even if it was proven empty a moment earlier (see
// TestReapScopeIfEmptyDoesNotRemoveAScopeRepopulatedAfterTheEmptyProof).
// Failure means "not empty (yet)"; callers must never treat it as an
// error to escalate, only as "try again later, if at all."
//
// This proves ONLY that the scope was empty at removal time -- it does
// NOT prove the scope's owner will never populate it later (a scope can
// be genuinely, temporarily empty mid-launch, before its process is
// placed into it). Callers making an irreversible decision (releasing an
// admission lease, e.g.) from a successful reap MUST additionally gate on
// an age signal immune to queueing/launch delay -- see AIRA-49's
// admitWaiter.grantedAt for why enqueue time and directory-mtime-derived
// age were both found unsafe for this across three review rounds.
func ReapScopeIfEmpty(slicePath, scopeID string, afterEmptyProof func()) (bool, error) {
	if !validConfineScopeID(scopeID) {
		return false, fmt.Errorf("invalid scope id")
	}
	parentFD, err := unix.Open(slicePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("open confine slice: %w", err)
	}
	defer unix.Close(parentFD)
	return reapEmptyConfineScopeTree(parentFD, ".aira-"+scopeID, afterEmptyProof)
}
```

- [ ] **Step 4: Run to verify pass** — all four cases, including the repopulation-after-proof race.

- [ ] **Step 5: Commit** — `git commit -m "feat(runner): ReapScopeIfEmpty, a single-scope kernel-enforced removal primitive with a testable repopulation seam (AIRA-49)"`

## Task 3: Daemon-side sweep, gated on `grantedAt`

**Files:**
- Modify: `internal/daemon/confine_reaper.go`, `internal/daemon/paths.go`, `internal/daemon/server.go`
- Test: `internal/daemon/confine_reaper_test.go` (new, or extend existing), using the `AIRA_CONFINE_SLICE` + `cgrouptest.IsolatedScopeParent(t)` seam and the `confine_manage_test.go:39-50` granted-waiter fixture (both named in "File Structure" above).

**Interfaces:**
- Consumes: `runner.ReapScopeIfEmpty` (Task 2), `admitWaiter.grantedAt` (Task 1), the existing `s.admitQueues`/`s.admitRegistryMu` locking pattern (match `activeConfines`'s exact lock order — registry mutex then queue mutex, never the reverse), `s.releaseActiveConfine` (existing), `s.admitNowTime()` (existing, for consistency with how grantedAt was set).
- Produces: `func (s *Server) releaseStaleGrantedLeasesPass(ctx context.Context)`, `func (s *Server) staleGrantedLeases(grace time.Duration) []staleLeaseCandidate` where `staleLeaseCandidate` is `{path, scopeID string; grantedFor time.Duration}` — note `staleGrantedLeases` takes NO path parameter; it iterates every registered `s.admitQueues` slice itself (Sol round-4: the single-slice version silently missed leases on a non-default `--slice`).

- [ ] **Step 1: Write the failing tests first**

```go
func TestReleaseStaleGrantedLeasesPassReleasesAWaiterGrantedLongAgoOnceItsScopeReapsEmpty(t *testing.T) {
	// t.Setenv("AIRA_CONFINE_SLICE", cgrouptest.IsolatedScopeParent(t)) for
	// a real confine slice; build a *Server; hand-construct a sliceQueue
	// with an admitWaiter{state: admitGranted, scopeID: ..., grantedAt:
	// s.admitNowTime().Add(-time.Hour), ...} per confine_manage_test.go's
	// existing pattern; create a REAL, empty ".aira-<scopeID>" cgroup
	// directory under the slice path (createReaperTestScope-equivalent).
	// s.staleLeaseReleaseGrace = time.Second (fast test, well under the
	// simulated hour-old grant).
	//
	// s.releaseStaleGrantedLeasesPass(ctx)
	//
	// Assert: s.activeConfines(path) no longer lists the scope, the scope
	// directory is gone from disk, AND (Sol round-4: don't stop at the
	// registry-listing check alone) the SAME sliceQueue's own
	// queue.outstanding and queue.outstandingJobs have been decremented
	// back to whatever they were before this waiter was granted (reach
	// into the test's own hand-constructed sliceQueue to check this
	// directly) -- proving the ledger's accounting, not just its
	// listing, is actually correct after release.
}

func TestReleaseStaleGrantedLeasesPassLeavesARecentlyGrantedWaiterAlone(t *testing.T) {
	// Same shape, grantedAt = s.admitNowTime() (fresh). Assert the lease
	// is still present in activeConfines afterward, and (if the scope
	// directory was created empty for the test) it is untouched.
}

func TestReleaseStaleGrantedLeasesPassLeavesALiveNestedChildScopeAlone(t *testing.T) {
	// grantedAt old enough to pass the age gate, but the scope has a live
	// nested child (startReaperTestSleeper) -- assert NOT released, same
	// as Task 2's equivalent case, proving the daemon-level wiring
	// preserves Task 2's own safety property end to end.
}

func TestStaleGrantedLeasesNeverReadsEnqueuedForItsAgeDecision(t *testing.T) {
	// A waiter with enqueued set very old (simulating a long queueing
	// wait) but grantedAt set recently (simulating "just granted after a
	// long queue wait") must NOT be selected as a candidate even though
	// enqueued alone would suggest staleness -- this is the direct
	// regression test for the v3 defect this plan's Task 1 exists to fix.
}

func TestStaleGrantedLeasesCoversEveryRegisteredSliceNotJustTheDefault(t *testing.T) {
	// Sol round-4/5: the direct regression test for the multi-slice
	// defect. Build a *Server with TWO distinct sliceQueue entries under
	// s.admitQueues, keyed by two different, real, distinct slice paths
	// (e.g. two separate cgrouptest.IsolatedScopeParent(t) directories --
	// neither the daemon's ordinary default-resolved path), each holding
	// its own old-enough admitGranted waiter with a matching empty real
	// scope directory. Call s.staleGrantedLeases(grace) directly (not the
	// full pass) and assert candidates from BOTH paths are returned --
	// this is what actually locks in "iterates every registered queue",
	// not just "works for the one path a test happens to construct".
}
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

`internal/daemon/paths.go`, alongside the other `default*` constants:

```go
// defaultStaleLeaseReleaseGrace is a LEASE-TTL policy, not a liveness
// proof: any admitGranted lease whose scope is STILL found empty this
// long after grantedAt (the daemon's own in-memory record of the exact
// grant moment -- never enqueue time, which conflates ordinary
// admission-queue contention with launch abandonment, and never any
// external PID/filesystem signal) is reclaimed, unconditionally, once the
// kernel itself confirms the scope is empty at reclaim time. This is
// deliberately NOT framed as "the owner is dead": Sol/Codex's round-4
// review established that no signal available to this daemon can
// actually prove that -- a live launcher can be legitimately SIGSTOPed,
// cgroup-frozen, or stuck in an unbounded kernel/filesystem operation for
// longer than any fixed bound, and THIS POLICY WILL RECLAIM ITS LEASE
// ANYWAY if that exceeds 15 minutes, which could break a launch that was
// never actually abandoned. This is an accepted, deliberate trade-off
// (an ordinary lease has always had this shape: bounded lifetime,
// reclaimed on non-use, regardless of whether the holder could in
// principle still resume) chosen because the alternative -- a
// renewal/heartbeat protocol so an in-progress launch can explicitly
// extend its own lease -- is real new machinery this project's
// architectural-simplicity preference weighs against for a scenario
// (a launcher paused for over 15 minutes before ever populating its
// scope) with no evidence of ever occurring in practice. 15 minutes is
// chosen because ordinary grant-to-populated latency is a fraction of a
// second (scope creation immediately followed by child placement,
// internal/runner/confine_linux.go) -- two to three orders of magnitude
// of margin over anything this project has observed, not because it
// proves anything. See AIRA-49's plan changelog (v1 through v5) for the
// four review rounds that shaped this.
defaultStaleLeaseReleaseGrace = 15 * time.Minute
```

`internal/daemon/server.go`: add `staleLeaseReleaseGrace time.Duration` to `*Server`, mirroring `scopeReapGrace`'s exact existing field/init pattern.

`internal/daemon/confine_reaper.go`:

```go
type staleLeaseCandidate struct {
	path       string
	scopeID    string
	grantedFor time.Duration
}

// staleGrantedLeases returns admitGranted waiters across EVERY currently
// registered admission-queue slice (Sol round-4: the prior draft only
// ever resolved the one default slice via ResolveConfineManagementSlice(""),
// silently missing any lease admitted against an explicitly-specified
// --slice) whose lease has been held, per grantedAt (Task 1 -- the
// daemon's own authoritative grant-moment record, never enqueue time and
// never any OS-derived signal), for at least `grace`.
func (s *Server) staleGrantedLeases(grace time.Duration) []staleLeaseCandidate {
	s.admitRegistryMu.Lock()
	queues := make(map[string]*sliceQueue, len(s.admitQueues))
	for path, queue := range s.admitQueues {
		queues[path] = queue
	}
	s.admitRegistryMu.Unlock()
	now := s.admitNowTime()
	var stale []staleLeaseCandidate
	for path, queue := range queues {
		queue.mu.Lock()
		for _, waiter := range queue.waiters {
			if waiter == nil || waiter.state != admitGranted || waiter.scopeID == "" || waiter.grantedAt.IsZero() {
				continue
			}
			if age := now.Sub(waiter.grantedAt); age >= grace {
				stale = append(stale, staleLeaseCandidate{path: path, scopeID: waiter.scopeID, grantedFor: age})
			}
		}
		queue.mu.Unlock()
	}
	return stale
}

// releaseStaleGrantedLeasesPass is AIRA-49's backstop: a granted lease
// that never transitions out of "granted" -- for whatever reason, known
// or not (see this plan's "root-cause honesty" note; the ordinary
// connection-close release path is expected to handle a plain SIGKILL of
// the launcher, so a lease reaching this backstop may indicate a rarer
// path than originally assumed) -- would otherwise be permanently stuck:
// ConfineKill's own empty-scope path returns a "retry" error that can
// never resolve, and the ordinary orphan reaper explicitly treats any
// granted lease as proof of life and skips it. For each lease past its
// TTL (per staleGrantedLeases -- grantedAt-based, immune to queueing
// delay; see defaultStaleLeaseReleaseGrace's doc comment for why this is
// a reclaim POLICY, not a death proof), this attempts the same
// kernel-enforced physical reap the ordinary orphan reaper trusts
// (runner.ReapScopeIfEmpty) and reclaims the ledger lease ONLY on a
// confirmed-successful reap -- never on the age signal alone.
func (s *Server) releaseStaleGrantedLeasesPass(ctx context.Context) {
	grace := s.staleLeaseReleaseGrace
	if grace <= 0 {
		grace = defaultStaleLeaseReleaseGrace
	}
	var released []string
	for _, candidate := range s.staleGrantedLeases(grace) {
		if err := ctx.Err(); err != nil {
			if len(released) > 0 {
				log.Printf("aira daemon: scope-reaper: released %d stale confine lease(s) before cancellation: %s", len(released), strings.Join(released, ", "))
			}
			return
		}
		reaped, reapErr := runner.ReapScopeIfEmpty(candidate.path, candidate.scopeID, nil)
		if reapErr != nil || !reaped {
			continue
		}
		s.releaseActiveConfine(candidate.path, candidate.scopeID)
		released = append(released, candidate.scopeID)
		log.Printf("aira daemon: scope-reaper: reclaimed stale confine lease (granted %s ago, past its %s TTL, scope physically reaped as empty): scope=%s slice=%s", candidate.grantedFor.Round(time.Second), grace, candidate.scopeID, candidate.path)
	}
}
```

Note this pass no longer calls `runner.ResolveConfineManagementSlice("")` at all — it derives every slice path it needs directly from `s.admitQueues`'s own keys, which is exactly the set of slices that could possibly hold a granted lease. (The separate, existing `reapOrphanedScopesPass` still only resolves the default slice — a pre-existing limitation of that unrelated mechanism, out of scope for this plan; do not fix it here, but do not accidentally copy its limitation into this new code either.)

Wire into `runScopeReaper`, immediately before the existing reap call:

```go
		s.releaseStaleGrantedLeasesPass(ctx)
		s.reapOrphanedScopesPass(ctx)
```

Document explicitly (code comment on `staleGrantedLeases` or `releaseStaleGrantedLeasesPass`, implementer's choice of exact placement) two accepted, deliberate residual gaps, per Fable's review — do not silently omit either:
- A granted waiter whose scope directory does not exist yet at all (genuinely still mid-launch, before scope creation) is correctly never a candidate for THIS pass (`ReapScopeIfEmpty` fails to open it, `reaped=false`) — its only current release path if abandoned before ever creating a scope is the connection-close path. This is acceptable: such a window is bounded by however long scope creation itself can plausibly take, which this project has no evidence is ever close to `staleLeaseReleaseGrace`.
- PID reuse on top of a dead supervisor is not this pass's concern at all (it uses no PID signal); a stuck lease whose SCOPE remains genuinely populated (e.g. by an unrelated process that happened to land in the same, still-existing cgroup after the real supervisor died — an unlikely but not impossible scenario) would simply never satisfy the physical-reap gate and would stay stuck, same safe-direction liveness gap the existing orphan reaper already accepts for the identical reason.

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Full confined verification** — `aira confine -- make ci`, plus `AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/runner/... ./internal/daemon/...`

- [ ] **Step 6: Commit** — `git commit -m "fix(daemon): release stale confine leases gated on grantedAt + kernel-confirmed empty reap (AIRA-49)"`

## Task 4: Post-deploy verification against the original repro

**Not a code task.** After Task 3 is merged and deployed:

- [ ] Re-run (or ask fastest-ee-dc to re-run, since they hold the original repro details — scope_id `CONFINE-@dr-job-4017698-dl50y3eb6xkj` pattern, `aira confine --delegate-ram -- <long job>` then external-kill the wrapper) the original AIRA-49 repro against the deployed fix.
- [ ] Confirm the lease actually releases within the expected bound (grace + reaper interval).
- [ ] If it releases via the ORDINARY connection-close path before this sweep ever gets a chance to (plausible per Fable's trace of `admit.go:452-521`) — that is fine and expected for a plain SIGKILL; this sweep exists as the backstop for whatever OTHER, still-unexplained mechanism produced the original stuck state. Update AIRA-49's ticket with whatever is actually observed, honestly, rather than declaring the original root cause confirmed without having watched it happen.

## Deferred / explicitly out of scope

- Improving `ConfineKill`'s `U_CONFINE_NOT_LAUNCHED` message for the transient case.
- Hardening the admit-connection-close release path itself — this sweep is a backstop regardless of that path's behavior, and Task 4 may reveal that path already works correctly for the common case.
- Shortening `staleLeaseReleaseGrace` below 15 minutes — deliberately conservative given three review rounds on getting the safety direction right; tighten later as a lower-risk follow-up if the ~20-minute worst-case recovery time proves to matter in practice.
