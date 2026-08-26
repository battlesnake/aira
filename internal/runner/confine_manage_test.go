package runner

import (
	"reflect"
	"testing"
	"time"
)

func TestOrphanedConfineScopeCandidatesRequireEveryPositiveProof(t *testing.T) {
	intPtr := func(value int) *int { return &value }
	int64Ptr := func(value int64) *int64 { return &value }
	record := func(scopeID string) ConfineRecord {
		return ConfineRecord{
			ScopeID:       scopeID,
			Populated:     intPtr(0),
			SupervisorPID: intPtr(101),
			AgeSeconds:    int64Ptr(120),
		}
	}

	reap := record("CONFINE-reap-101-1")
	populated := record("CONFINE-populated-102-1")
	populated.Populated = intPtr(1)
	populated.SupervisorPID = intPtr(102)
	unknownPopulation := record("CONFINE-population-unknown-103-1")
	unknownPopulation.Populated = nil
	unknownPopulation.SupervisorPID = intPtr(103)
	unknownSupervisor := record("CONFINE-supervisor-unknown-104-1")
	unknownSupervisor.SupervisorPID = nil
	aliveSupervisor := record("CONFINE-alive-105-1")
	aliveSupervisor.SupervisorPID = intPtr(105)
	young := record("CONFINE-young-106-1")
	young.SupervisorPID = intPtr(106)
	young.AgeSeconds = int64Ptr(119)
	unknownAge := record("CONFINE-age-unknown-107-1")
	unknownAge.SupervisorPID = intPtr(107)
	unknownAge.AgeSeconds = nil
	pending := record("CONFINE-pending-108-1")
	pending.SupervisorPID = intPtr(108)
	pending.Pending = true
	leased := record("CONFINE-leased-109-1")
	leased.SupervisorPID = intPtr(109)

	deadCalls := make(map[int]int)
	got := orphanedConfineScopeCandidates([]ConfineRecord{
		reap,
		populated,
		unknownPopulation,
		unknownSupervisor,
		aliveSupervisor,
		young,
		unknownAge,
		pending,
		leased,
	}, 2*time.Minute, func(pid int) bool {
		deadCalls[pid]++
		return pid != 105
	}, func(scopeID string) bool {
		return scopeID == leased.ScopeID
	})

	if !reflect.DeepEqual(got, []ConfineRecord{reap}) {
		t.Fatalf("candidates=%+v, want only %+v", got, reap)
	}
	// A live daemon lease keeps the scope even though it is empty+dead+old, and its
	// PID gate is still evaluated (the lease check is last in the disjunction).
	if want := map[int]int{101: 1, 105: 1, 106: 1, 107: 1, 108: 1, 109: 1}; !reflect.DeepEqual(deadCalls, want) {
		t.Fatalf("supervisorDead calls=%v, want %v", deadCalls, want)
	}
	if got := orphanedConfineScopeCandidates([]ConfineRecord{reap}, 2*time.Minute, nil, nil); len(got) != 0 {
		t.Fatalf("nil supervisor-death check selected candidates=%+v", got)
	}
}
