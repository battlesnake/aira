package daemon

import (
	"context"
	"log"
	"strings"
	"time"

	"aira/internal/runner"
)

func (s *Server) runScopeReaper(ctx context.Context, interval time.Duration) {
	if interval == 0 {
		<-ctx.Done()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.releaseStaleGrantedLeasesPass(ctx)
		s.reapOrphanedScopesPass(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Server) reapOrphanedScopesPass(ctx context.Context) {
	_, path, err := runner.ResolveConfineManagementSlice("")
	if err != nil {
		return
	}
	grace := s.scopeReapGrace
	if grace <= 0 {
		grace = defaultScopeReapGrace
	}
	// Authoritative, PID-namespace-independent liveness: a scope with a granted
	// admit lease is held by a live supervisor and is never reaped. Query the live
	// lease set on EACH check rather than snapshotting up front: a confine registers
	// its granted admit lease BEFORE the client returns from admission and creates
	// the scope directory, so any directory the scan observes whose supervisor is
	// still live is visible here. A pre-scan snapshot could miss a lease granted in
	// the window between the snapshot and the scan (a request that queued past the
	// grace, then got granted, then created its still-empty scope); a live query
	// cannot. Scope IDs are unique per launch, so a released scope's ID is never
	// re-leased, and a directory with no live lease is genuinely orphaned.
	hasLiveLease := func(scopeID string) bool {
		for _, entry := range s.activeConfines(path) {
			if entry.ScopeID == scopeID {
				return true
			}
		}
		return false
	}
	result, err := runner.ReapOrphanedConfineScopes(ctx, path, grace, nil, hasLiveLease)
	if err != nil {
		log.Printf("aira daemon: scope-reaper: sweep error: %v", err)
		return
	}
	if len(result.Reaped) > 0 {
		log.Printf("aira daemon: scope-reaper: removed %d orphaned confine scope(s): %s", len(result.Reaped), strings.Join(result.Reaped, ", "))
	}
}

// staleLeaseCandidate identifies one lease by the EXACT waiter pointer, never by
// its scope id (AIRA-68). The earlier shape carried only path+scopeID and the
// release step re-SEARCHED the queue for a granted waiter with that id, which is
// an ABA hazard: between candidate collection and the release, the stale lease
// can close its own connection and a replacement can be admitted under the same
// id — and the search would then release the replacement's lease.
//
// A pointer alone is not the whole fix. ReapScopeIfEmpty is also keyed on the
// scope-id STRING, so it would rmdir the REPLACEMENT's newly created,
// still-empty scope. That destructive half is closed by releaseStaleLeaseCandidate,
// which re-validates under queue.mu immediately before acting and makes no
// filesystem call at all for a vanished lease.
type staleLeaseCandidate struct {
	queue      *sliceQueue
	waiter     *admitWaiter
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
//
// A waiter with no grantedAt record at all is skipped: "no grant record means
// never released by this pass" is the fail-closed direction, and falling back
// to enqueued would reintroduce the exact queueing-delay-as-abandonment defect
// grantedAt exists to remove.
//
// AIRA-68: a SCOPE-LESS lease (scopeID == "", i.e. an `aira confine-reserve`
// per-test reservation) is skipped too, and this is an accepted, deliberate
// coverage gap rather than an oversight — see the last bullet below. It is the
// selector here, and not any downstream validation, that must keep excluding it:
// a test asserting only on this pass's EFFECT would also pass against an
// implementation that dropped the skip, because ReapScopeIfEmpty rejects an empty
// scope id on its own.
//
// Lock order matches activeConfines exactly: registry mutex, then queue mutex,
// never the reverse. The registry snapshot is taken and released before any
// queue is locked.
//
// Accepted, deliberate residual gaps -- none of these is an oversight:
//   - A granted waiter whose scope directory does not exist yet at all
//     (genuinely still mid-launch, before scope creation) is correctly never a
//     candidate for release by this pass: ReapScopeIfEmpty fails to open it and
//     reports reaped=false, and AIRA-68's vanished branch requires a scope that
//     was OBSERVED to exist first, which such a waiter never had. Its only
//     current release path if abandoned before
//     ever creating a scope is the connection-close path. That window is
//     bounded by how long scope creation itself can plausibly take -- normally
//     a fraction of a second (one socket frame, an Mkdir, a handful of
//     cgroupfs writes, then clone3(CLONE_INTO_CGROUP): no locks, no queueing,
//     no I/O beyond local cgroupfs) -- nowhere close to staleLeaseReleaseGrace.
//   - The one residual false-positive direction (a launcher legitimately
//     SIGSTOPed or frozen inside that sub-second window for longer than the
//     grace) is fail-closed, confirmed by trace: when it resumes, its cgroup fd
//     is already gone, so its own next cgroupfs write returns ENOENT/ENODEV,
//     the launch fails cleanly, and the job's own deferred cleanup runs. It
//     never ends up running uncontained.
//   - AIRA-68, the scope-LESS population: an `aira confine-reserve` reservation
//     creates no cgroup artifact of any kind, so neither the physical reap nor
//     the confine scan can establish anything about it, and its ONLY release
//     path is its connection closing. It is now the majority of the ledger by
//     count on a busy box. A liveness probe for it (SO_PEERCRED plus a
//     start-tick check) is deliberately NOT built: there is no observed instance
//     of such a lease wedging, and the project's architectural-simplicity rule
//     prefers keeping the primitive and documenting the gap over new machinery
//     for a hypothetical. What ships instead is visibility — `confine --list`
//     now names and counts this population separately, so a genuine wedge shows
//     as scope-less reservations standing still with no suite running.
//   - AIRA-68, a leader that MIGRATED OUT of its scope: escape is witnessed and
//     attested but never prevented (internal/runner/descendant_escape_linux_test.go),
//     so an escapee's original scope becomes empty and reclaimable while the
//     process is still alive. Both branches of this pass reclaim that lease, and
//     that is the intended behaviour, not an accident: the escapee is uncontained
//     by construction, so its reserve is buying nothing; the release is
//     ledger-only; and its memory is still charged through
//     max(current - reclaimable, sum of reserves). The alternative for a wedged
//     supervisor is holding the reserve forever, which is the defect AIRA-68 is
//     about.
//   - PID reuse on top of a dead supervisor is not this pass's concern at all
//     (it uses no PID signal). A stuck lease whose SCOPE remains genuinely
//     populated -- e.g. by an unrelated process that happened to land in the
//     same, still-existing cgroup after the real supervisor died -- would
//     simply never satisfy the physical-reap gate and would stay stuck: the
//     same safe-direction liveness gap the existing orphan reaper already
//     accepts for the identical reason.
//   - This sweep only ever frees LEDGER accounting. If a peer connection
//     genuinely IS still open on the daemon side (the original,
//     still-not-fully-explained AIRA-49 case), that connection's parked
//     goroutine, its admit slot and its socket fd all persist regardless of
//     what this sweep does; they are released only by that connection's own
//     eventual close. Nobody should expect this sweep to close a live
//     connection -- it only ever un-sticks the ledger's own bookkeeping.
//   - Like reapOrphanedScopesPass, this only ever consults the ledger's own
//     admitQueues. It makes no claim about any slice this daemon has never been
//     asked to admit against; that is the sweep's defined scope, not a gap.
//   - A job legitimately older than the grace can have a genuinely empty scope
//     for a brief window on its OWN successful exit path too: after cmd.Wait()
//     returns but before the launcher's deferred cleanup actually removes the
//     scope and reports usage/attests teardown (confine_linux.go -- readUsage,
//     reportPeak, attestScopeTeardown, cleanupConfineScope). A sweep tick
//     landing in that sub-second window reaps the directory and reclaims the
//     lease a little early. Build-review traced this and confirmed it is
//     harmless: release is idempotent (the launcher's own later release is a
//     no-op), the exit code is unaffected, and the launcher's own usage/
//     attestation calls correctly observe the gone scope as an honest
//     unevaluated/unverified outcome rather than fabricating a result -- never
//     a false pass. Not worth a code-level fix (architectural-simplicity): it
//     costs nothing but occasionally losing one legitimate job's peak-RSS
//     sample from the estimate history a little early.
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
				stale = append(stale, staleLeaseCandidate{queue: queue, waiter: waiter, path: path, scopeID: waiter.scopeID, grantedFor: age})
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
// a reclaim POLICY, not a death proof), this establishes ONE of two positive
// proofs and reclaims the ledger lease only on that -- never on the age signal
// alone:
//
//  1. the same kernel-enforced physical reap the ordinary orphan reaper trusts
//     (runner.ReapScopeIfEmpty), or
//  2. AIRA-68's seen -> gone TRANSITION, observed by the evaluator's own confine
//     scan (admitWaiter.scopeSeen/scopeVanished).
//
// The second exists because ReapScopeIfEmpty returns ENOENT for a scope
// directory that is ALREADY GONE, which the old code treated as "skip" -- so a
// lease whose scope had disappeared could never be reclaimed by this pass at
// all, on any pass, forever. That is exactly AIRA-68's stated failure shape.
// See releaseStaleLeaseCandidate for why the vanished branch must make no
// filesystem call, and admitWaiter for why the transition is admissible where
// plain absence was not.
func (s *Server) releaseStaleGrantedLeasesPass(ctx context.Context) {
	grace := s.staleLeaseGrace()
	var released []string
	for _, candidate := range s.staleGrantedLeases(grace) {
		if err := ctx.Err(); err != nil {
			if len(released) > 0 {
				log.Printf("aira daemon: scope-reaper: reclaimed %d stale confine lease(s) before cancellation: %s", len(released), strings.Join(released, ", "))
			}
			return
		}
		proof, reclaimed := s.releaseStaleLeaseCandidate(candidate)
		if !reclaimed {
			continue
		}
		released = append(released, candidate.scopeID)
		// The log names WHICH proof was used. A "vanished" reclaim removed
		// nothing, and reporting it as a reap would describe an act that did not
		// happen.
		log.Printf("aira daemon: scope-reaper: reclaimed stale confine lease (granted %s ago, past its %s TTL, %s): scope=%s slice=%s",
			candidate.grantedFor.Round(time.Second), grace, proof, candidate.scopeID, candidate.path)
	}
}

func (s *Server) staleLeaseGrace() time.Duration {
	if s.staleLeaseReleaseGrace > 0 {
		return s.staleLeaseReleaseGrace
	}
	return defaultStaleLeaseReleaseGrace
}

// staleLeaseActionableLocked re-reads the CURRENT facts for one candidate's
// EXACT waiter pointer. queue.mu must be held.
//
// The candidate list is a pre-filter taken in an earlier locked pass, not a
// decision: by the time the sweep acts, that lease may already have been
// released by its own connection close and a replacement admitted under the same
// scope id. Pointer identity is what defeats that ABA — a scope id embeds a pid
// and a wall-clock stamp but is not enforced unique anywhere, and waiters are
// freshly allocated and never pooled, so a live pointer cannot alias a different
// waiter.
func staleLeaseActionableLocked(now time.Time, queue *sliceQueue, waiter *admitWaiter, scopeID string, grace time.Duration) bool {
	found := false
	for _, existing := range queue.waiters {
		if existing == waiter {
			found = true
			break
		}
	}
	if !found || waiter.state != admitGranted || !waiter.accounted || waiter.scopeID != scopeID {
		return false
	}
	return !waiter.grantedAt.IsZero() && now.Sub(waiter.grantedAt) >= grace
}

// dischargeVanishedStaleLease validates and discharges in ONE critical section,
// and is the whole vanished branch: it makes no filesystem call at all, because
// there is nothing to reap and a scope-id-keyed destructive rmdir here could
// remove a same-id replacement's newly created, still-empty scope.
//
// TWO conditions beyond the shared validation, both fail-closed:
//
//   - waiter.scopeVanished, re-read HERE rather than trusted from the candidate
//     snapshot. A scope observed absent one pass ago may have been observed
//     present since, and the evaluator clears the bit when it is.
//   - !queue.adoptedScanFailed. A vanished bit is an observation, and an
//     observation that can no longer be refreshed is not a current fact. While
//     the scan is failing the daemon cannot establish whether the scope is still
//     absent, so it must not reclaim on a stale sighting — "a check that cannot
//     establish its result reports unevaluated, never a fake pass". Both plan
//     reviewers found this independently; without it one persistent cgroupfs
//     failure freezes a stale absence into a permanent licence to reclaim.
//
// Doing this in one critical section (rather than validate, unlock, discharge)
// is what closes the race both reviewers found: between an unlocked validation
// and the discharge, the evaluator can re-observe the scope and clear the bit,
// after which the sweep would discharge a lease whose proof had just evaporated.
func (s *Server) dischargeVanishedStaleLease(candidate staleLeaseCandidate, grace time.Duration) bool {
	queue, waiter := candidate.queue, candidate.waiter
	if queue == nil || waiter == nil {
		return false
	}
	queue.mu.Lock()
	if !staleLeaseActionableLocked(s.admitNowTime(), queue, waiter, candidate.scopeID, grace) ||
		!waiter.scopeVanished || queue.adoptedScanFailed {
		queue.mu.Unlock()
		return false
	}
	released := releaseAdmitWaiterLocked(queue, waiter)
	queue.mu.Unlock()
	if released {
		s.afterAdmitRelease(queue)
	}
	return released
}

// dischargeReapedStaleLease re-validates and discharges atomically AFTER a
// successful physical reap. The reap syscall itself cannot be made under
// queue.mu, so the validation is done twice: once to decide whether to touch the
// filesystem at all, and once — here — to decide whether to touch the ledger.
func (s *Server) dischargeReapedStaleLease(candidate staleLeaseCandidate, grace time.Duration) bool {
	queue, waiter := candidate.queue, candidate.waiter
	if queue == nil || waiter == nil {
		return false
	}
	queue.mu.Lock()
	if !staleLeaseActionableLocked(s.admitNowTime(), queue, waiter, candidate.scopeID, grace) {
		queue.mu.Unlock()
		return false
	}
	released := releaseAdmitWaiterLocked(queue, waiter)
	queue.mu.Unlock()
	if released {
		s.afterAdmitRelease(queue)
	}
	return released
}

// staleLeaseStillActionable is the pre-filter for the DESTRUCTIVE branch: it is
// what stops a stale candidate whose waiter has since been released and replaced
// from reaching ReapScopeIfEmpty and rmdir'ing the replacement's scope.
func (s *Server) staleLeaseStillActionable(candidate staleLeaseCandidate, grace time.Duration) bool {
	queue, waiter := candidate.queue, candidate.waiter
	if queue == nil || waiter == nil {
		return false
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return staleLeaseActionableLocked(s.admitNowTime(), queue, waiter, candidate.scopeID, grace)
}

// releaseStaleLeaseCandidate reclaims ONE stale lease, on either of two proofs,
// and reports which. It returns reclaimed=false whenever it could not establish
// one — never a reclaim on the age signal alone, and never a reported reclaim
// that a concurrent ordinary release actually performed.
//
// A candidate that is vanished but whose scan has since failed falls through to
// the reap branch, where ReapScopeIfEmpty returns ENOENT for the absent
// directory and nothing is reclaimed. That is the intended fail-closed outcome,
// not an accident of ordering.
func (s *Server) releaseStaleLeaseCandidate(candidate staleLeaseCandidate) (proof string, reclaimed bool) {
	grace := s.staleLeaseGrace()
	if s.dischargeVanishedStaleLease(candidate, grace) {
		// Past tense, and only about the SCAN's observations. "is now gone" would
		// be a present-tense claim about state last read up to one scan ago, and
		// the daemon cannot establish it at the moment it prints it.
		return "scope observed by the confine scan and then observed absent", true
	}
	if !s.staleLeaseStillActionable(candidate, grace) {
		return "", false
	}
	reaped, reapErr := runner.ReapScopeIfEmpty(candidate.path, candidate.scopeID, nil)
	if reapErr != nil || !reaped {
		return "", false
	}
	if !s.dischargeReapedStaleLease(candidate, grace) {
		return "", false
	}
	return "scope physically reaped as empty", true
}
