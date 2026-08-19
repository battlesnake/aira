package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"aira/internal/app"
	"aira/internal/store"
)

func (s *Server) registryEntries() ([]store.RegistryEntry, error) {
	if s.listRegistryEntries != nil {
		return s.listRegistryEntries(s.Paths.RegistryPath)
	}
	return store.ListRegistryEntries(s.Paths.RegistryPath)
}

func (s *Server) discoverProjectAt(ctx context.Context, root string) (app.Project, error) {
	if s.discoverProject != nil {
		return s.discoverProject(ctx, root)
	}
	return app.Discover(ctx, root)
}

// discoverRegistryPass performs one best-effort registry enumeration. Its
// counters are used only for observability and deterministic tests; individual
// entry failures never escape the pass.
func (s *Server) discoverRegistryPass(ctx context.Context) (discovered, registered, skipped int) {
	entries, err := s.registryEntries()
	if err != nil {
		log.Printf("aira daemon: registry discovery read: %v", err)
		return 0, 0, 0
	}
	registered = len(entries)
	for _, entry := range entries {
		s.mu.Lock()
		_, covered := s.coveredWorktrees[entry.WorktreeID]
		s.mu.Unlock()
		if covered {
			continue
		}

		project, err := s.discoverProjectAt(ctx, entry.Root)
		if err != nil {
			skipped++
			logRegistryDiscoverySkip(entry, err)
			continue
		}
		if project.ProjectID != entry.ProjectID || project.WorktreeID != entry.WorktreeID {
			skipped++
			logRegistryDiscoverySkip(entry, fmt.Errorf("identity mismatch: found project %s worktree %s", project.ProjectID, project.WorktreeID))
			continue
		}
		scope, err := ScopeFromProject(project, s.Paths)
		if err != nil {
			skipped++
			logRegistryDiscoverySkip(entry, err)
			continue
		}
		_, cached, err := s.storeForScope(scope)
		if err != nil {
			skipped++
			logRegistryDiscoverySkip(entry, err)
			continue
		}
		if !cached {
			discovered++
		}
	}
	if discovered > 0 || skipped > 0 {
		log.Printf("aira daemon: registry discovery: discovered %d new / %d registered, %d skipped", discovered, registered, skipped)
	}
	return discovered, registered, skipped
}

func logRegistryDiscoverySkip(entry store.RegistryEntry, err error) {
	log.Printf("aira daemon: registry discovery skip project %s worktree %s root %q: %v", entry.ProjectID, entry.WorktreeID, entry.Root, err)
}

func (s *Server) runRegistryDiscovery(ctx context.Context, interval time.Duration) {
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
		s.discoverRegistryPass(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
