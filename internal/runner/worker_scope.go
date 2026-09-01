package runner

import (
	"path/filepath"
	"strings"
)

// WorkerScopeChildPath returns the exact child cgroup directory path that
// linuxScopeBackend.Create(ctx, id) would create under parent, WITHOUT
// touching the filesystem. It exists so the daemon can know a granted
// worker's scope path (to read memory.current from it) without creating
// anything itself, and so every client (aitest-bootstrap, worker-admit)
// derives the identical path from the same (parent, id) pair. Returns "" for
// an id that Create would itself reject (matches cgroup_linux.go's own
// validation: no "/" in id).
func WorkerScopeChildPath(parent, id string) string {
	if strings.Contains(id, "/") || id == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(parent), ".aira-"+id)
}
