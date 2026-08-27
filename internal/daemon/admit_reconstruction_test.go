package daemon

import (
	"errors"
	"math"
	"testing"
	"time"

	"aira/internal/runner"
)

func confineScanRecord(scopeID string, populated int, cap string) runner.ConfineRecord {
	return runner.ConfineRecord{ScopeID: scopeID, Populated: &populated, Cap: &cap}
}

func noConfinesScan(string) (runner.ConfineListResult, error) {
	return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}}, nil
}

func reconstructionTestServer(now *time.Time, scan func(string) (runner.ConfineListResult, error)) *Server {
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return *now }
	server.admitConfineScanInterval = time.Second
	server.admitConfineScan = scan
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, bool, string) {
		return 0, 100, true, ""
	}
	return server
}

func TestAdmitReconstructsReserveAndGatesWaiter(t *testing.T) {
	now := time.Unix(100, 0)
	server := reconstructionTestServer(&now, func(path string) (runner.ConfineListResult, error) {
		if path != "/slice" {
			t.Fatalf("scan path=%q, want /slice", path)
		}
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-a-1-a", 1, "40"),
			confineScanRecord("CONFINE-b-2-b", 2, "20"),
		}}, nil
	})
	server.admitSliceHeadroomSupervisor = 5
	waiter := &admitWaiter{seq: 1, reserve: 26, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

	server.evaluateAdmitQueue(queue)

	if queue.adopted != 60 || queue.adoptedJobs != 2 || !queue.adoptedAt.Equal(now) {
		t.Fatalf("adopted=%d jobs=%d at=%v, want 60/2/%v", queue.adopted, queue.adoptedJobs, queue.adoptedAt, now)
	}
	if waiter.state != admitQueued || queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("waiter state=%v outstanding=%d jobs=%d, want queued/0/0", waiter.state, queue.outstanding, queue.outstandingJobs)
	}
}

func TestAdmitReconstructionExcludesConnectionHeldScope(t *testing.T) {
	now := time.Unix(200, 0)
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-held-1-a", 1, "40"),
			confineScanRecord("CONFINE-adopted-2-b", 1, "20"),
		}}, nil
	})
	held := &admitWaiter{seq: 1, reserve: 40, state: admitGranted, accounted: true, scopeID: "CONFINE-held-1-a"}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{held}, outstanding: 40, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)

	if queue.adopted != 20 || queue.adoptedJobs != 1 || addClamp(queue.outstanding, queue.adopted) != 60 {
		t.Fatalf("outstanding=%d adopted=%d adopted_jobs=%d total=%d, want 40/20/1/60", queue.outstanding, queue.adopted, queue.adoptedJobs, addClamp(queue.outstanding, queue.adopted))
	}
}

func TestAdmitReconstructionSelfHealsAfterScopeRelease(t *testing.T) {
	now := time.Unix(300, 0)
	scopes := []runner.ConfineRecord{
		confineScanRecord("CONFINE-a-1-a", 1, "10"),
		confineScanRecord("CONFINE-b-2-b", 1, "20"),
	}
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: scopes}, nil
	})
	queue := &sliceQueue{path: "/slice", server: server}

	server.evaluateAdmitQueue(queue)
	if queue.adopted != 30 || queue.adoptedJobs != 2 {
		t.Fatalf("first adopted=%d jobs=%d, want 30/2", queue.adopted, queue.adoptedJobs)
	}
	scopes = scopes[1:]
	now = now.Add(time.Second)
	server.evaluateAdmitQueue(queue)
	if queue.adopted != 20 || queue.adoptedJobs != 1 {
		t.Fatalf("healed adopted=%d jobs=%d, want 20/1", queue.adopted, queue.adoptedJobs)
	}
}

func TestAdmitReconstructionRefreshIsThrottled(t *testing.T) {
	now := time.Unix(400, 0)
	scans := 0
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		scans++
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-a-1-a", 1, "10"),
		}}, nil
	})
	queue := &sliceQueue{path: "/slice", server: server}

	server.evaluateAdmitQueue(queue)
	now = now.Add(999 * time.Millisecond)
	server.evaluateAdmitQueue(queue)
	if scans != 1 || queue.adopted != 10 || queue.adoptedJobs != 1 {
		t.Fatalf("scans=%d adopted=%d jobs=%d, want 1/10/1", scans, queue.adopted, queue.adoptedJobs)
	}
	now = now.Add(time.Millisecond)
	server.evaluateAdmitQueue(queue)
	if scans != 2 {
		t.Fatalf("scans=%d after interval, want 2", scans)
	}
}

func TestAdmitReconstructionScanFailureKeepsPriorLedger(t *testing.T) {
	now := time.Unix(500, 0)
	scanResult := runner.ConfineListResult{}
	scanErr := errors.New("read failed")
	scans := 0
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		scans++
		return scanResult, scanErr
	})
	queue := &sliceQueue{path: "/slice", server: server, adopted: 33, adoptedJobs: 2}

	server.evaluateAdmitQueue(queue)
	if queue.adopted != 33 || queue.adoptedJobs != 2 || !queue.adoptedAt.Equal(now) || !queue.adoptedScanFailed {
		t.Fatalf("error changed ledger: adopted=%d jobs=%d at=%v failed=%v, want 33/2/%v/true", queue.adopted, queue.adoptedJobs, queue.adoptedAt, queue.adoptedScanFailed, now)
	}
	now = now.Add(999 * time.Millisecond)
	server.evaluateAdmitQueue(queue)
	if scans != 1 {
		t.Fatalf("failed scan retried inside throttle interval: scans=%d, want 1", scans)
	}

	now = now.Add(time.Millisecond)
	scanErr = nil
	scanResult = runner.ConfineListResult{Verdict: "unevaluated", Reason: "slice unreadable", Scopes: []runner.ConfineRecord{}}
	server.evaluateAdmitQueue(queue)
	if queue.adopted != 33 || queue.adoptedJobs != 2 || !queue.adoptedScanFailed {
		t.Fatalf("unevaluated changed ledger: adopted=%d jobs=%d failed=%v, want 33/2/true", queue.adopted, queue.adoptedJobs, queue.adoptedScanFailed)
	}

	now = now.Add(time.Second)
	scanResult = runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
		confineScanRecord("CONFINE-recovered-1-a", 1, "11"),
	}}
	server.evaluateAdmitQueue(queue)
	if queue.adopted != 11 || queue.adoptedJobs != 1 || queue.adoptedScanFailed {
		t.Fatalf("recovered adopted=%d jobs=%d failed=%v, want 11/1/false", queue.adopted, queue.adoptedJobs, queue.adoptedScanFailed)
	}
}

func TestAdmitReconstructionNonFiniteCapsContributeNeitherBytesNorHeadroom(t *testing.T) {
	now := time.Unix(600, 0)
	populated := 1
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-max-1-a", 1, "max"),
			{ScopeID: "CONFINE-nil-2-b", Populated: &populated, Cap: nil},
			confineScanRecord("CONFINE-bad-3-c", 1, "not-an-integer"),
			confineScanRecord("CONFINE-negative-4-d", 1, "-9"),
			confineScanRecord("CONFINE-finite-5-e", 1, "12"),
		}}, nil
	})
	// Non-zero per-job headroom so a spurious job count would shrink the ceiling.
	server.admitSliceHeadroomSupervisor = 10
	server.admitReadMemory = func(string) (int64, int64, bool, string) { return 0, 100, true, "" }
	// A queued waiter whose reserve fits ONLY if the four non-finite scopes added
	// neither bytes nor headroom-jobs: ceiling = 100 − headroom(adoptedJobs 1 + 1)*10
	// = 80; available = 80 − max(0+12, 0) = 68. If the non-finite scopes had each been
	// counted as a job (jobs=5), ceiling would be 100 − 60 = 40 → available 28 → blocked.
	waiter := &admitWaiter{seq: 1, reserve: 68, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

	server.evaluateAdmitQueue(queue)

	if queue.adopted != 12 || queue.adoptedJobs != 1 {
		t.Fatalf("adopted=%d jobs=%d, want 12/1 (only the finite cap counts)", queue.adopted, queue.adoptedJobs)
	}
	if waiter.state != admitGranted {
		t.Fatalf("waiter state=%v, want granted (non-finite scopes must not consume headroom)", waiter.state)
	}
}

func TestAdmitReconstructionSkipsEmptyAndUnknownScopes(t *testing.T) {
	now := time.Unix(700, 0)
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-empty-1-a", 0, "100"),
			confineScanRecord("CONFINE-live-2-b", 2, "25"),
			{ScopeID: "CONFINE-unknown-3-c", Populated: nil, Cap: func() *string { value := "50"; return &value }()},
		}}, nil
	})
	queue := &sliceQueue{path: "/slice", server: server}

	server.evaluateAdmitQueue(queue)

	if queue.adopted != 25 || queue.adoptedJobs != 1 {
		t.Fatalf("adopted=%d jobs=%d, want 25/1", queue.adopted, queue.adoptedJobs)
	}
}

func TestAdmitReconstructionGrantWindowCountsEachHeldJobOnce(t *testing.T) {
	now := time.Unix(800, 0)
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-visible-1-a", 1, "40"),
		}}, nil
	})
	visible := &admitWaiter{seq: 1, reserve: 40, state: admitGranted, accounted: true, scopeID: "CONFINE-visible-1-a"}
	notCreated := &admitWaiter{seq: 2, reserve: 30, state: admitGranted, accounted: true, scopeID: "CONFINE-not-created-2-b"}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{visible, notCreated}, outstanding: 70, outstandingJobs: 2}

	server.evaluateAdmitQueue(queue)

	if queue.adopted != 0 || queue.adoptedJobs != 0 || addClamp(queue.outstanding, queue.adopted) != 70 {
		t.Fatalf("outstanding=%d adopted=%d adopted_jobs=%d total=%d, want 70/0/0/70", queue.outstanding, queue.adopted, queue.adoptedJobs, addClamp(queue.outstanding, queue.adopted))
	}
}

func TestAdmitChargeInvariantIncludesAdopted(t *testing.T) {
	charge := addClamp(20, 50)
	if got := checkedAvailable(40, 100, charge, 10); got != 20 {
		t.Fatalf("available=%d, want ceiling(90)-max(70,40)=20", got)
	}
	if got := checkedAvailable(80, 100, charge, 10); got != 10 {
		t.Fatalf("available=%d, want ceiling(90)-max(70,80)=10", got)
	}
}

func TestAddClampSaturatesWithoutFabricatingHeadroom(t *testing.T) {
	for _, test := range []struct {
		a, b int64
		want int64
	}{
		{40, 2, 42},
		{math.MaxInt64 - 5, 5, math.MaxInt64},
		{math.MaxInt64 - 5, 6, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{-1, 1, math.MaxInt64},
		{1, -1, math.MaxInt64},
	} {
		if got := addClamp(test.a, test.b); got != test.want {
			t.Fatalf("addClamp(%d,%d)=%d, want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestAdmitReconstructionCapSumSaturates(t *testing.T) {
	now := time.Unix(900, 0)
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-large-1-a", 1, "9223372036854775802"),
			confineScanRecord("CONFINE-overflow-2-b", 1, "10"),
		}}, nil
	})
	waiter := &admitWaiter{seq: 1, reserve: 1, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

	server.evaluateAdmitQueue(queue)

	if queue.adopted != math.MaxInt64 || queue.adoptedJobs != 2 || waiter.state != admitQueued {
		t.Fatalf("adopted=%d jobs=%d waiter=%v, want MaxInt64/2/queued", queue.adopted, queue.adoptedJobs, waiter.state)
	}
}
