package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
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
		_, failed := s.discoveryFailed[entry.WorktreeID]
		s.mu.Unlock()
		if covered {
			// A covered entry's skip reason, if any, is stale: forget it so a
			// later failure logs again.
			s.forgetDiscoverySkip(entry)
			continue
		}
		if failed {
			continue
		}

		// AIRA-209: a registry root that no longer exists cannot be a project;
		// stat it instead of forking git three times (with retries) per pass.
		// No latch: the next pass stats again, so a root that reappears is found.
		if _, statErr := os.Stat(entry.Root); errors.Is(statErr, fs.ErrNotExist) {
			skipped++
			s.logRegistryDiscoverySkip(entry, statErr)
			continue
		}

		project, err := s.discoverProjectAt(ctx, entry.Root)
		if err != nil {
			skipped++
			s.logRegistryDiscoverySkip(entry, err)
			continue
		}
		if project.ProjectID != entry.ProjectID || project.WorktreeID != entry.WorktreeID {
			skipped++
			s.logRegistryDiscoverySkip(entry, fmt.Errorf("identity mismatch: found project %s worktree %s", project.ProjectID, project.WorktreeID))
			continue
		}
		scope, err := ScopeFromProject(project, s.Paths)
		if err != nil {
			skipped++
			s.logRegistryDiscoverySkip(entry, err)
			continue
		}
		_, cached, err := s.storeForScope(scope)
		if err != nil {
			s.mu.Lock()
			if s.discoveryFailed == nil {
				s.discoveryFailed = make(map[string]struct{})
			}
			s.discoveryFailed[entry.WorktreeID] = struct{}{}
			s.mu.Unlock()
			skipped++
			s.logRegistryDiscoverySkip(entry, err)
			continue
		}
		s.forgetDiscoverySkip(entry)
		if !cached {
			discovered++
		}
	}
	if discovered > 0 || skipped > 0 {
		// Print only when the counts change (AIRA-209): with dead roots registered
		// every pass would otherwise repeat an identical line.
		triple := [3]int{discovered, registered, skipped}
		s.mu.Lock()
		changed := s.discoveryLastSummary != triple
		s.discoveryLastSummary = triple
		s.mu.Unlock()
		if changed {
			log.Printf("aira daemon: registry discovery: discovered %d new / %d registered, %d skipped", discovered, registered, skipped)
		}
	}
	return discovered, registered, skipped
}

func discoverySkipKey(entry store.RegistryEntry) string {
	return entry.WorktreeID + "\x00" + entry.Root
}

// logRegistryDiscoverySkip prints a skip line only when this (worktree id, root)
// key has not logged yet or its reason text changed since the last line, so a
// persistent skip costs one journal line instead of one per pass.
func (s *Server) logRegistryDiscoverySkip(entry store.RegistryEntry, err error) {
	key, reason := discoverySkipKey(entry), err.Error()
	s.mu.Lock()
	previous, seen := s.discoverySkipLogged[key]
	if seen && previous == reason {
		s.mu.Unlock()
		return
	}
	if s.discoverySkipLogged == nil {
		s.discoverySkipLogged = make(map[string]string)
	}
	s.discoverySkipLogged[key] = reason
	s.mu.Unlock()
	log.Printf("aira daemon: registry discovery skip project %s worktree %s root %q: %v", entry.ProjectID, entry.WorktreeID, entry.Root, err)
}

func (s *Server) forgetDiscoverySkip(entry store.RegistryEntry) {
	s.mu.Lock()
	delete(s.discoverySkipLogged, discoverySkipKey(entry))
	s.mu.Unlock()
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
