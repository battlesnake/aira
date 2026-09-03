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
//
// A waiter with no grantedAt record at all is skipped: "no grant record means
// never released by this pass" is the fail-closed direction, and falling back
// to enqueued would reintroduce the exact queueing-delay-as-abandonment defect
// grantedAt exists to remove.
//
// Lock order matches activeConfines exactly: registry mutex, then queue mutex,
// never the reverse. The registry snapshot is taken and released before any
// queue is locked.
//
// Accepted, deliberate residual gaps -- none of these is an oversight:
//   - A granted waiter whose scope directory does not exist yet at all
//     (genuinely still mid-launch, before scope creation) is correctly never a
//     candidate for release by this pass: ReapScopeIfEmpty fails to open it and
//     reports reaped=false. Its only current release path if abandoned before
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
