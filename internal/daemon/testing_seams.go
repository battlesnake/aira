package daemon

import (
	"context"
	"sync"
	"time"

	"aira/internal/runner"
)

func (s *Server) SetAdmitReadMemoryForTest(fn func(string) (int64, int64, int64, bool, string)) {
	s.admitReadMemory = fn
}

// SetWorkerScopeTreeForTest replaces the worker-admit ledger's two cgroupfs
// seams (AIRA-39) with an in-memory tree, for external test packages that
// exercise the client/daemon boundary against a synthetic outer scope that is
// not a real cgroup. Without it the honest answer for such a scope is
// "unevaluated", which tells those tests nothing about the wire behaviour they
// exist to check.
//
// The two seams share one map, so a scope created through the daemon is seen by
// the very next scan, exactly as on a real tree. The scan throttle is disabled
// so a test observes the tree as it is.
func (s *Server) SetWorkerScopeTreeForTest() {
	var mu sync.Mutex
	caps := map[string]map[string]int64{}
	s.workerScopeScanInterval = time.Nanosecond
	s.workerScopeScan = func(outerScope string) (workerScopeChildren, error) {
		mu.Lock()
		defer mu.Unlock()
		var children workerScopeChildren
		for _, value := range caps[outerScope] {
			children.committed = addClamp(children.committed, value)
			children.count++
		}
		return children, nil
	}
	s.workerScopeCreate = func(_ context.Context, outerScope, workerID string, memoryMax int64) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		if caps[outerScope] == nil {
			caps[outerScope] = map[string]int64{}
		}
		caps[outerScope][workerScopeChildPrefix+workerID] = memoryMax
		// The fake reports `enforced`: this seam stands in for a
		// CreateWorkerScope that succeeded, and a real success writes and
		// VERIFIES memory.swap.max=0. Reporting anything else here would make
		// every test that uses this seam assert against a disposition the
		// production path never produces on success.
		return runner.WorkerScopeChildPath(outerScope, "worker-"+workerID), runner.WorkerAdmitSwapCapEnforced, nil
	}
}

// SetAdmitReadWorkerSupervisorMemoryForTest overrides the aggregate guard's
// supervisor-scope read (worker_admit.go) -- a SEPARATE seam from
// SetAdmitReadMemoryForTest above, since the supervisor's own child scope
// is deliberately never given a memory.max (AIRA-38) and so cannot use the
// same reader as the outer-scope ledger read.
func (s *Server) SetAdmitReadWorkerSupervisorMemoryForTest(fn func(string) (int64, int64, bool, string)) {
	s.admitReadWorkerSupervisorMemory = fn
}

// SetWorkerAdmitHeadroomForTest overrides the production worker-admit
// headroom default (64 MiB, worker_admit.go) so a test admitting against
// small synthetic byte values (or a small real cgroup memory.max) is not
// universally denied.
func (s *Server) SetWorkerAdmitHeadroomForTest(value int64) {
	s.workerAdmitHeadroom = value
}
