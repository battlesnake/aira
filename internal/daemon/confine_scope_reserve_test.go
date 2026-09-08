package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-191 / AIRA-192. The per-scope reserve the daemon now publishes beside
// each scope's cap.
//
// The defect these pin: `confine --list` exposed a scope's memory.max and
// nothing else, and `aira top` summed those caps as the slice's claim. For a
// --delegate-ram scope the cap is an AIRA-15 containment CEILING, many times the
// pinned framework reserve the ledger actually charges, so the dashboard drew
// ~93 GiB of claims on an 80 GiB machine while the ledger's own line read 40 GiB
// granted / 53 GiB ceiling — and `Claimed + Outside > Total` fired OVER-SUBSCRIBED
// on a healthy slice.
//
// verifies: AIRA-191
// verifies: AIRA-192

// reserveScopeDir writes a scope directory the real cgroupfs scan can read, with
// a caller-chosen memory.max so a test can make the cap and the ledger charge
// differ the way a delegate scope's really do.
func reserveScopeDir(t *testing.T, slice, scopeID string, capBytes, current int64) {
	t.Helper()
	path := filepath.Join(slice, ".aira-"+scopeID)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"cgroup.events":  "populated 1\n",
		"cgroup.procs":   "4242\n",
		"memory.current": strconv.FormatInt(current, 10) + "\n",
		"memory.max":     strconv.FormatInt(capBytes, 10) + "\n",
		"cgroup.kill":    "",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func reserveScopeID(t *testing.T, name string, pid int, delegate bool) string {
	t.Helper()
	id := "CONFINE-"
	if delegate {
		id += "@dr-"
	}
	id += name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@session-a"
	if runner.IsDelegateRAMScopeID(id) != delegate {
		t.Fatalf("test premise: scope id %q does not read as delegate=%v", id, delegate)
	}
	return id
}

func reserveListRequest() core.Request {
	return core.Request{Verb: "confine-list", Args: map[string]any{"slice": "test.slice", "owner": "session-a"}}
}

func reserveScopeByID(t *testing.T, result runner.ConfineListResult, scopeID string) runner.ConfineRecord {
	t.Helper()
	for _, record := range result.Scopes {
		if record.ScopeID == scopeID {
			return record
		}
	}
	t.Fatalf("scope %s missing from the listing %+v", scopeID, result.Scopes)
	return runner.ConfineRecord{}
}

func reserveListing(t *testing.T, server *Server) runner.ConfineListResult {
	t.Helper()
	response := server.confineManagement(context.Background(), reserveListRequest())
	result, ok := response.Data.(runner.ConfineListResult)
	if !response.OK || !ok {
		t.Fatalf("confine-list response=%+v", response)
	}
	return result
}

// The headline. A delegate-ram scope's 45 GiB ceiling sits on disk while the
// daemon charges it 512 MiB; the listing must publish the 512 MiB and must not
// launder the ceiling into the reserve field.
//
// Non-porous in the false-pass direction: the two numbers are 90x apart and both
// are asserted, so a build that simply re-published Cap under a new name fails
// on the first assertion, and one that published a fabricated zero fails too.
func TestConfineListPublishesTheLedgerChargeNotTheDelegateScopeCeiling(t *testing.T) {
	const (
		ceiling = int64(45) << 30
		charge  = int64(512) << 20
		current = int64(9) << 30
	)
	slice := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return current, int64(64) << 30, 0, true, ""
	}
	scopeID := reserveScopeID(t, "suite", 5101, true)
	reserveScopeDir(t, slice, scopeID, ceiling, current)
	queue := &sliceQueue{path: slice, server: server, outstanding: charge, outstandingJobs: 1}
	queue.waiters = []*admitWaiter{{
		seq: 1, reserve: charge, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		scopeID: scopeID, name: "suite", owner: "session-a", scopeCeiling: ceiling,
	}}
	server.admitQueues[slice] = queue

	record := reserveScopeByID(t, reserveListing(t, server), scopeID)
	if record.ReserveBytes == nil {
		t.Fatal("a connection-held, accounted, scope-backed waiter published NO reserve; the daemon holds this number and the listing is the one place it can be read back")
	}
	if *record.ReserveBytes != charge {
		t.Fatalf("reserve=%d, want the ledger charge %d (the scope ceiling beside it is %d)",
			*record.ReserveBytes, charge, ceiling)
	}
	if record.Cap == nil || *record.Cap != strconv.FormatInt(ceiling, 10) {
		t.Fatalf("cap=%v, want the untouched ceiling %d: the reserve is an ADDITION, and a face that still wants the ceiling must still get it",
			record.Cap, ceiling)
	}
}

// The honesty direction. A scope on disk that the daemon holds no admission
// record for must publish NO reserve — never its cap, never a zero. This is the
// post-restart and daemon-lost population, and it is precisely where a
// cap-shaped fallback would look most plausible and be most wrong.
func TestConfineListLeavesAnUnknownScopeReserveUnevaluated(t *testing.T) {
	const capBytes = int64(45) << 30
	slice := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 1 << 30, int64(64) << 30, 0, true, ""
	}
	scopeID := reserveScopeID(t, "orphan", 5102, true)
	reserveScopeDir(t, slice, scopeID, capBytes, 1<<30)
	// A queue exists (so the snapshot is `present`) but knows nothing about this
	// scope: the strictly harder case than no queue at all, because the daemon has
	// state to answer from and must still decline.
	server.admitQueues[slice] = &sliceQueue{path: slice, server: server}

	record := reserveScopeByID(t, reserveListing(t, server), scopeID)
	if record.ReserveBytes != nil {
		t.Fatalf("an untracked scope published reserve=%d; a cap is not a reserve and the honest answer is unevaluated",
			*record.ReserveBytes)
	}
	named := false
	for _, facet := range record.UnevaluatedFields {
		if facet == runner.ConfineReserveFacet {
			named = true
		}
	}
	if !named {
		t.Fatalf("unevaluated fields=%v, want `reserve` named so a JSON consumer is told the field was not established",
			record.UnevaluatedFields)
	}
}

// A granted waiter that is NOT accounted contributes nothing to the ledger's
// ScopeBytes, so it must contribute no per-scope reserve either. Publishing one
// would break the reconciliation below in the direction that matters — the bar
// would draw a claim the slice is not holding.
func TestConfineListPublishesNoReserveForAnUnaccountedWaiter(t *testing.T) {
	slice := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 1 << 30, int64(64) << 30, 0, true, ""
	}
	scopeID := reserveScopeID(t, "unaccounted", 5103, false)
	reserveScopeDir(t, slice, scopeID, 4<<30, 1<<30)
	queue := &sliceQueue{path: slice, server: server}
	queue.waiters = []*admitWaiter{{
		seq: 1, reserve: 4 << 30, state: admitGranted, accounted: false, grantedCh: make(chan struct{}),
		scopeID: scopeID, name: "unaccounted", owner: "session-a",
	}}
	server.admitQueues[slice] = queue

	result := reserveListing(t, server)
	record := reserveScopeByID(t, result, scopeID)
	if record.ReserveBytes != nil {
		t.Fatalf("an unaccounted waiter published reserve=%d while contributing %d to ScopeBytes",
			*record.ReserveBytes, result.SliceReserve.ScopeBytes)
	}
}

// The invariant that makes the whole change checkable rather than merely
// plausible: the per-scope reserves published by ONE listing sum to that same
// listing's own scope-backed ledger total. A build whose per-scope number came
// from anywhere but the ledger — the cap, the frozen grant under a dynamic
// charge, a re-read of memory.current — fails here.
//
// The fixture carries all three populations at once, with three DIFFERENT
// numbers per scope (cap, frozen reserve, live charge), so no two of them can be
// confused and still pass.
func TestConfineListPerScopeReservesReconcileWithTheSliceLedger(t *testing.T) {
	const (
		delegateCeiling = int64(45) << 30
		delegateCharge  = int64(512) << 20
		plainCap        = int64(8) << 30
		plainFrozen     = int64(8) << 30
		plainCharge     = int64(3) << 30 // AIRA-29 has re-derived this DOWN from the grant
	)
	slice := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 12 << 30, int64(64) << 30, 0, true, ""
	}
	delegateID := reserveScopeID(t, "suite", 5104, true)
	plainID := reserveScopeID(t, "build", 5105, false)
	reserveScopeDir(t, slice, delegateID, delegateCeiling, 9<<30)
	reserveScopeDir(t, slice, plainID, plainCap, 2<<30)
	queue := &sliceQueue{
		path: slice, server: server,
		outstanding: delegateCharge + plainCharge, outstandingJobs: 2,
	}
	queue.waiters = []*admitWaiter{
		{seq: 1, reserve: delegateCharge, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
			scopeID: delegateID, name: "suite", owner: "session-a", scopeCeiling: delegateCeiling},
		// The dynamic-charge case: the frozen grant is 8 GiB, the ledger charges
		// 3 GiB, and 3 GiB is what the slice is actually holding for it.
		{seq: 2, reserve: plainFrozen, effectiveCharge: plainCharge, chargeTracked: true,
			state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
			scopeID: plainID, name: "build", owner: "session-a"},
	}
	server.admitQueues[slice] = queue

	result := reserveListing(t, server)
	if result.SliceReserve == nil {
		t.Fatal("no slice reserve on the reply")
	}
	total := int64(0)
	for _, record := range result.Scopes {
		if record.ReserveBytes != nil {
			total += *record.ReserveBytes
		}
	}
	if want := result.SliceReserve.ScopeBytes + result.SliceReserve.AdoptedBytes; total != want {
		t.Fatalf("per-scope reserves sum to %d, want the listing's own %d (ScopeBytes %d + AdoptedBytes %d)",
			total, want, result.SliceReserve.ScopeBytes, result.SliceReserve.AdoptedBytes)
	}
	if got := reserveScopeByID(t, result, plainID).ReserveBytes; got == nil || *got != plainCharge {
		t.Fatalf("dynamically charged scope reserve=%v, want the live charge %d rather than the %d frozen grant or the %d cap",
			got, plainCharge, plainFrozen, plainCap)
	}
}

// The post-restart population. A scope the daemon ADOPTED by scan (its
// connection-held lease died with the previous daemon) is charged a
// reconstructed reserve, and that reconstruction is per-scope inside the
// evaluator — so the listing can name it rather than reporting the whole
// adopted population as one anonymous scalar.
//
// Without this the bar goes blank across every daemon restart, which is a new
// honesty problem in place of the old wrong-number one.
func TestConfineListNamesAdoptedScopeReserves(t *testing.T) {
	const (
		sliceMax = 64 * gib
		capBytes = 30 * gib
		rss      = 2 * gib
	)
	now := time.Unix(400_000, 0)
	slice := t.TempDir()
	scopeID := reserveScopeID(t, "adopted", 5106, false)
	server := oversubServer(&now, sliceMax, 3*gib, 200, staticScan(oversubRecord(scopeID, rss, capBytes)))
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	reserveScopeDir(t, slice, scopeID, capBytes, rss)
	queue := &sliceQueue{path: slice, server: server}
	registerAdmitQueue(server, queue)

	server.evaluateAdmitQueue(queue)
	if queue.adoptedJobs != 1 || queue.adopted <= 0 {
		t.Fatalf("test premise: the scan adopted %d jobs / %d bytes", queue.adoptedJobs, queue.adopted)
	}

	result := reserveListing(t, server)
	record := reserveScopeByID(t, result, scopeID)
	if record.ReserveBytes == nil {
		t.Fatal("an adopted scope published no reserve; the evaluator reconstructed one for it and the listing must be able to say whose it is")
	}
	if *record.ReserveBytes != queue.adopted {
		t.Fatalf("adopted reserve=%d, want the reconstruction the ledger charges: %d", *record.ReserveBytes, queue.adopted)
	}
	// It is a reconstruction from LIVE USAGE, never the cap: adopting a delegate
	// or warm scope's whole cap is the over-reservation AIRA-74/AIRA-29 removed.
	if *record.ReserveBytes >= capBytes {
		t.Fatalf("adopted reserve=%d reached the %d cap; the reconstruction is usage+margin, not the ceiling", *record.ReserveBytes, int64(capBytes))
	}
	if result.SliceReserve == nil || result.SliceReserve.AdoptedBytes != *record.ReserveBytes {
		t.Fatalf("adopted row=%d does not reconcile with AdoptedBytes=%+v", *record.ReserveBytes, result.SliceReserve)
	}
}

// The per-scope adopted reserves must move EXACTLY as the adopted scalar beside
// them does, in both directions, or the rows and the total they reconcile
// against start describing different instants.
//
// A failed scan deliberately RETAINS the adopted ledger (a stale reserve is the
// conservative direction for admission, and admit_reconstruction_test pins it),
// so the rows must be retained too — dropping them would report an adopted total
// no row accounts for. A later SUCCESSFUL scan is authoritative and replaces the
// whole set, so a scope that has gone must leave with it rather than lingering
// as a phantom claim.
func TestAdoptedScopeReservesTrackTheAdoptedScalarAcrossScans(t *testing.T) {
	const sliceMax = 64 * gib
	now := time.Unix(410_000, 0)
	scopeID := reserveScopeID(t, "adopted", 5107, false)
	mode := "ok"
	server := oversubServer(&now, sliceMax, 3*gib, 200, func(string) (runner.ConfineListResult, error) {
		switch mode {
		case "fail":
			return runner.ConfineListResult{Verdict: "unevaluated", Reason: "stubbed scan failure"}, os.ErrPermission
		case "gone":
			return runner.ConfineListResult{Verdict: "pass"}, nil
		}
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{oversubRecord(scopeID, 2*gib, 30*gib)}}, nil
	})
	queue := &sliceQueue{path: "/slice", server: server}
	registerAdmitQueue(server, queue)

	server.evaluateAdmitQueue(queue)
	granted := server.admitSliceSnapshot("/slice").scopeReserves[scopeID]
	if granted <= 0 || granted != queue.adopted {
		t.Fatalf("test premise: per-scope reserve %d does not carry the adopted scalar %d", granted, queue.adopted)
	}

	mode, now = "fail", now.Add(time.Hour)
	server.evaluateAdmitQueue(queue)
	if !queue.adoptedScanFailed || queue.adopted != granted {
		t.Fatalf("test premise: a failed scan changed the adopted scalar (%d, failed=%v)", queue.adopted, queue.adoptedScanFailed)
	}
	if got := server.admitSliceSnapshot("/slice").scopeReserves[scopeID]; got != granted {
		t.Fatalf("failed scan dropped the per-scope reserve to %d while the adopted scalar it reconciles with stayed at %d", got, granted)
	}

	mode, now = "gone", now.Add(time.Hour)
	server.evaluateAdmitQueue(queue)
	if queue.adopted != 0 {
		t.Fatalf("test premise: the successful scan left adopted=%d", queue.adopted)
	}
	if _, present := server.admitSliceSnapshot("/slice").scopeReserves[scopeID]; present {
		t.Fatal("a successful scan that no longer sees the scope left its reserve standing as a phantom claim")
	}
}
