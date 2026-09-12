package daemon

import (
	"context"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// verifies: S13 / AIRA-220 honesty (design §4). GrantedEstablished (the confine --list
// "granted pair is trustworthy" bit) reads FALSE during the restart freeze — the granted
// total is not yet settled, because survivors may still re-declare — and reads true once
// the freeze is over. It is DERIVED from snapshot.present + the freeze (not hardcoded), so
// it survives S12 deleting the cgroup-scan adoption and S13 the dump/unanchored layer.
//
// MUTATION: revert to GrantedEstablished: snapshot.present -> the freeze arm reads true
// during the freeze and this reds.
func TestGrantedEstablishedFalseWhileFrozen(t *testing.T) {
	const maximum = int64(16 << 30)
	path := t.TempDir()
	now := time.Unix(9000, 0)
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.shimReadMemTotal = func() (int64, bool) { return 48 << 30, true }
	server.shimReadMemAvailable = func() (int64, bool, string) { return 20 << 30, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 5 << 30, maximum, 1 << 30, true, ""
	}

	// An established, anchored granted lease (a survivor re-anchored by its keeper's
	// re-declare just after restart) — the population GrantedEstablished reports on.
	seeded := &admitWaiter{
		seq: 1, reserve: 2 << 30, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: "CONFINE-g@s", basis: "redeclare", anchor: testAnchorConn(),
	}
	queue := &sliceQueue{
		path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: []*admitWaiter{seeded},
	}
	queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	registerAdmitQueue(server, queue)

	established := func() bool {
		resp := server.confineManagement(context.Background(),
			core.Request{Verb: "confine-list", Args: map[string]any{"slice": "test.slice", "owner": "session-a"}})
		result, ok := resp.Data.(runner.ConfineListResult)
		if !resp.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("confine-list response=%+v", resp)
		}
		return result.SliceReserve.GrantedEstablished
	}

	server.armRestartFreeze(now)
	if established() {
		t.Fatal("GrantedEstablished must be FALSE during the restart freeze — the granted total is still settling")
	}

	now = now.Add(3 * time.Second) // past freeze-end
	if !established() {
		t.Fatal("GrantedEstablished must be TRUE once the freeze is over")
	}
}
