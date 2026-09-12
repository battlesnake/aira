package daemon

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"aira/internal/runner"
)

func (s *Server) SetAdmitReadMemoryForTest(fn func(string) (int64, int64, int64, bool, string)) {
	s.admitReadMemory = fn
}

// GrantedLeasesForTest snapshots the daemon's signed ledger as scope_id ->
// reserve over every admit queue, counting exactly the granted && accounted
// waiters (the same "Σleases" definition sliceProvablyEmpty and
// rederiveLedgerLocked use). It is the EXPORTED twin of the S13 confine merge
// gate's in-package gateLeases helper, provided so the S18 worker-path merge
// gate — which lives in the EXTERNAL pylib_test package to break the
// daemon<->pylib import cycle — can assert the exact scope-id key-set and Σ of
// re-anchored worker leases across a daemon restart without reaching into these
// unexported fields itself.
func (s *Server) GrantedLeasesForTest() map[string]int64 {
	s.admitRegistryMu.Lock()
	queues := make([]*sliceQueue, 0, len(s.admitQueues))
	for _, q := range s.admitQueues {
		queues = append(queues, q)
	}
	s.admitRegistryMu.Unlock()
	out := map[string]int64{}
	for _, q := range queues {
		q.mu.Lock()
		for _, w := range q.waiters {
			if w != nil && w.state == admitGranted && w.accounted {
				out[w.scopeID] = w.reserve
			}
		}
		q.mu.Unlock()
	}
	return out
}

// SetAdmitResolveSliceForTest overrides slice resolution so an external test can
// pin a deterministic slice path without depending on the host's real aira.slice
// cgroup. S15's worker-admit resolves the slice (unlike the pre-S15 path that read
// the outer scope directly), so worker-admit tests need this to stay hermetic.
func (s *Server) SetAdmitResolveSliceForTest(fn func(string) (string, bool, string)) {
	s.admitResolveSlice = fn
}

// SetRestartFreezeForTest sets the restart new-admission freeze duration; zero
// disables it, so a grant test does not wait out the 2 s freeze armed at Serve.
func (s *Server) SetRestartFreezeForTest(d time.Duration) {
	s.restartFreeze = d
}

// SetWorkerScopeTreeForTest replaces worker-admit's two cgroupfs seams (S15) with
// an in-memory tree, for external test packages that exercise the client/daemon
// boundary against a synthetic outer scope that is not a real cgroup. The seams
// share one map, so a scope created through the daemon is seen by the next id
// re-seed, exactly as on a real tree.
func (s *Server) SetWorkerScopeTreeForTest() {
	var mu sync.Mutex
	children := map[string]map[string]int64{}
	s.workerScopeMaxIndex = func(outerScope string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		maxIndex := 0
		for name := range children[outerScope] {
			if index, err := strconv.Atoi(strings.TrimPrefix(name, workerScopeChildPrefix)); err == nil && index > maxIndex {
				maxIndex = index
			}
		}
		return maxIndex, nil
	}
	s.workerScopeCreate = func(_ context.Context, outerScope, workerID string, memoryMax int64) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		if children[outerScope] == nil {
			children[outerScope] = map[string]int64{}
		}
		children[outerScope][workerScopeChildPrefix+workerID] = memoryMax
		// AIRA-35: deliberately NOT "enforced". This seam stands in for a
		// successful CreateWorkerScope, and "not-applicable" is an equally real
		// success disposition (a kernel with no swap support) -- but it differs
		// from the value a fabricating hop would invent, which is the entire
		// point: any hop that manufactures the value instead of carrying it fails
		// the assertions downstream.
		return runner.WorkerScopeChildPath(outerScope, "worker-"+workerID), runner.WorkerAdmitSwapCapNotApplicable, nil
	}
}

// SetConfineShimModeForTest puts this server into AIRA-121's ci-shim mode
// without an install-mode record on disk, for unit tests that construct a Server
// with NewServer and never call Serve (which is where production resolves the
// mode from the durable record).
//
// It sets the mode ONLY. The three re-sourced seams are resolved lazily at each
// point of use (sliceResolver, memoryReader, confineScan), so a test may still
// override any of them afterwards and its override wins — which is what lets a
// ledger test inject a small synthetic budget without a container cgroup.
func (s *Server) SetConfineShimModeForTest(budgetBytes int64, source, cgroupPath string) {
	s.confineMode = runner.ConfineModeShim
	s.shimBudget = shimBudget{Bytes: budgetBytes, Source: source, CgroupPath: cgroupPath}
}

// SetShimMeminfoForTest injects readShimMemory's host-wide /proc/meminfo
// fallback seams (AIRA-121 F3), so a test can drive the
// ShimBudgetSourceMemTotal branch with a synthetic MemTotal/MemAvailable pair
// instead of depending on this host's actual, unpredictable memory state.
func (s *Server) SetShimMeminfoForTest(readTotal func() (int64, bool), readAvailable func() (int64, bool, string)) {
	s.shimReadMemTotal = readTotal
	s.shimReadMemAvailable = readAvailable
}

// SetCPUFrameForTest injects AIRA-137's CPU-frame seams, so a test can assert
// the `confine --list` reply's CPU fields against a fixed frame instead of
// against this host's real root-cgroup counters, which move between every two
// statements and can never be asserted on.
func (s *Server) SetCPUFrameForTest(frame func(string) runner.ConfineCPUFrame, cores func() int) {
	s.readCPUFrame = frame
	s.readCPUCores = cores
}
