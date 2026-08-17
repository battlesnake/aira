//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestSupervisorLeaseReaderMatrix(t *testing.T) {
	processes := []processLiveness{processAlive, processDead, processUnknown}
	leases := []struct {
		name string
		live bool
	}{
		{"held-live", true}, {"held-expired", false}, {"lapsed", false}, {"absent", false},
		{"wrong-boot", false}, {"malformed", false}, {"read-error", false},
	}
	scopes := []struct {
		name  string
		state detachedScopeState
	}{
		{"empty", detachedScopeEmpty}, {"nonempty", detachedScopeNonempty},
		{"absent", detachedScopeEmpty}, {"uninspectable", detachedScopeUninspectable},
	}
	for _, proc := range processes {
		for _, lease := range leases {
			for _, scope := range scopes {
				for _, leaderExit := range []bool{false, true} {
					decision := decideDetachedReader(proc, lease.live, scope.state, leaderExit, true)
					want := detachedPreserve
					switch {
					case scope.state == detachedScopeUninspectable:
						want = detachedPreserve
					case leaderExit && scope.state == detachedScopeEmpty:
						want = detachedFinalizeEvidence
					case scope.state == detachedScopeEmpty && proc == processDead:
						want = detachedMarkLost
					}
					if decision.action != want {
						t.Fatalf("proc=%d lease=%s scope=%s exit=%v action=%d want=%d", proc, lease.name, scope.name, leaderExit, decision.action, want)
					}
					if decision.action == detachedMarkLost && (proc != processDead || scope.state != detachedScopeEmpty || leaderExit) {
						t.Fatalf("porous lost cell: proc=%d lease=%s scope=%s exit=%v", proc, lease.name, scope.name, leaderExit)
					}
					if decision.recentHeartbeat != (scope.state == detachedScopeEmpty && !leaderExit && proc == processUnknown && lease.live) {
						t.Fatalf("lease consulted outside R4/unknown: proc=%d lease=%s scope=%s exit=%v decision=%+v", proc, lease.name, scope.name, leaderExit, decision)
					}
				}
			}
		}
	}
}

func TestSupervisorLeaseReaderNonPorousDiscriminators(t *testing.T) {
	tests := []struct {
		name       string
		proc       processLiveness
		leaseLive  bool
		scope      detachedScopeState
		leaderExit bool
		want       detachedReaderAction
	}{
		{"unknown plus live lease preserves", processUnknown, true, detachedScopeEmpty, false, detachedPreserve},
		{"alive plus lapsed preserves", processAlive, false, detachedScopeEmpty, false, detachedPreserve},
		{"dead plus live lease is lost", processDead, true, detachedScopeEmpty, false, detachedMarkLost},
		{"exit plus lapsed finalises evidence", processDead, false, detachedScopeEmpty, true, detachedFinalizeEvidence},
		{"uninspectable dead pre-scope preserves", processDead, false, detachedScopeUninspectable, false, detachedPreserve},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideDetachedReader(test.proc, test.leaseLive, test.scope, test.leaderExit, false)
			if got.action != test.want {
				t.Fatalf("decision=%+v want action=%d", got, test.want)
			}
		})
	}
}

func TestSupervisorLeaseReconcileUninspectableDeadPreScopePreserves(t *testing.T) {
	oldBoot, oldStat := readBootIDFn, readProcStatFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn = oldBoot, oldStat })
	readBootIDFn = func() (string, error) { return "boot-a", nil }
	readProcStatFn = func(int) ([]byte, error) { return nil, os.ErrNotExist }
	r, scope := newMemoryRunner(t, nil)
	run := detachedRunForTest(scope, processDead)
	appendRunEvent(t, r, "starting", run)
	r.backend = &openErrorBackend{err: errors.New("scope unreadable")}
	got := reconcileOne(t, r)
	if got.Status.Terminal() || !containsString(got.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("R1 did not preserve/surface dead pre-scope run: %+v", got)
	}
}

func TestSupervisorLeaseReadErrorIsUnknownNotDeath(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.supervisorLeaseReadFn = func(context.Context, string) (bool, error) { return false, errors.New("db read failed") }
	live, err := r.readSupervisorLeaseLive(context.Background(), "RUN-1")
	if live || err == nil {
		t.Fatalf("lease read live=%v err=%v", live, err)
	}
}

// TestSupervisorLeaseReconcileUnknownFailingLeaseReadPreserves drives a REAL
// Reconcile (not the seam): empty scope + processUnknown + a FAILING lease read
// must preserve the run with U_RUN_RECONCILE_REQUIRED — a read fault is treated as
// "no positive signal", never death (Sol build r1 P2). It also proves the lease
// reader was actually consulted at R4/unknown.
func TestSupervisorLeaseReconcileUnknownFailingLeaseReadPreserves(t *testing.T) {
	oldBoot := readBootIDFn
	t.Cleanup(func() { readBootIDFn = oldBoot })
	readBootIDFn = func() (string, error) { return "boot-a", nil }
	r, scope := newMemoryRunner(t, nil) // empty scope
	run := detachedRunForTest(scope, processUnknown)
	appendRunEvent(t, r, "starting", run)
	var consulted bool
	r.supervisorLeaseReadFn = func(context.Context, string) (bool, error) {
		consulted = true
		return false, errors.New("db read failed")
	}
	got := reconcileOne(t, r)
	if !consulted {
		t.Fatal("R4/unknown did not consult the supervisor lease reader")
	}
	if got.Status.Terminal() || !containsString(got.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("failing lease read did not preserve the unknown run: %+v", got)
	}
}
