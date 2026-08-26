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
