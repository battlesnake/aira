package daemon

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-370. The evaluator's wait diagnosis (admitWaiter.blocked, rendered by
// admitBlockedTokens): every gate a pass can hold a queued waiter at is named,
// in the documented precedence, written in the same locked pass that refused
// the grant, cleared every pass, and absent for a waiter the pass did not
// evaluate. The seam is AIRA-350's (laneServer) with the lane OFF, so nothing
// here leans on the lane: 64 CPU slots, production headroom
// H(n) = 2048 + 64n MiB, sizes in MiB.

// diagServer is laneServer with the lane off.
func diagServer(t *testing.T, current *int64, now *time.Time) *Server {
	t.Helper()
	server := laneServer(t, laneE, current, now)
	server.smallLaneCap, server.smallLaneJobMax = 0, 0
	return server
}

func blockedTokens(waiter *admitWaiter) string {
	return strings.Join(admitBlockedTokens(waiter.blocked, waiter.heldByHead), ",")
}

// verifies: AIRA-370 — each resource the fit refused is named, and only those:
// RAM, CPU slots, VRAM and their combinations; ceiling-cut qualifies RAM only
// while the enforced ceiling is below memory.max, and never appears without
// RAM. Mutations: drop a resource bit (its row goes red); attach ceiling-cut
// regardless of ramFits (the "cpu with the ceiling cut" row goes red); set it
// on a throttled-but-not-lower snapshot (the "ram, ceiling at max" row).
func TestAdmitDiagnosisNamesTheResourcesThatDidNotFit(t *testing.T) {
	for _, test := range []struct {
		name        string
		leaseRAM    int64 // MiB
		leaseCPU    int64
		waiterRAM   int64
		waiterVRAM  int64
		ceilingDrop int64 // MiB the enforced ceiling sits below memory.max; 0 = none published
		want        string
	}{
		// room 45056 − H(2) − 40000 = 2880.
		{"ram", 40000, 1, 8000, 0, 0, "ram"},
		{"ram, ceiling cut", 40000, 1, 8000, 0, 3000, "ram,ceiling-cut"},
		{"ram, ceiling at max", 40000, 1, 8000, 0, -1, "ram"},
		{"cpu slots", 1000, 64, 1000, 0, 0, "cpu"},
		{"cpu with the ceiling cut", 1000, 64, 1000, 0, 3000, "cpu"},
		{"ram and cpu", 40000, 64, 8000, 0, 0, "ram,cpu"},
		{"vram unevaluated", 1000, 1, 1000, 1024, 0, "vram"},
		{"everything", 40000, 64, 8000, 1024, 3000, "ram,ceiling-cut,cpu,vram"},
		{"fits", 1000, 1, 1000, 0, 0, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(370000, 0)
			current := int64(0)
			server := diagServer(t, &current, &now)
			lease := laneLease(1, test.leaseRAM*mib)
			lease.cpu = test.leaseCPU
			waiter := laneJob(2, test.waiterRAM*mib, now)
			waiter.vram = test.waiterVRAM * mib
			queue := queueOf(server, lease, waiter)
			switch {
			case test.ceilingDrop > 0:
				server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
					Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingThrottled,
					Basis: "system-pressure", Ceiling: laneE - test.ceilingDrop*mib, StaticMax: laneE,
				})
			case test.ceilingDrop < 0:
				// Enforced and "throttled", but not below memory.max: nothing is cut
				// in this pass, so ceiling-cut must not be claimed.
				server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
					Mode: sliceCeilingEnforce, SlicePath: "/slice", State: sliceCeilingThrottled,
					Basis: "system-pressure", Ceiling: laneE, StaticMax: laneE,
				})
			}

			server.evaluateAdmitQueue(queue)

			if got := blockedTokens(waiter); got != test.want {
				t.Fatalf("blocked_by=%q, want %q", got, test.want)
			}
			if granted := waiter.state == admitGranted; granted != (test.want == "") {
				t.Fatalf("granted=%v with blocked_by %q", granted, test.want)
			}
		})
	}
}

// verifies: AIRA-370 — the gates that hold a waiter before or after the fit
// are named, alone, in precedence: restart-freeze, exclusive, head (a fit the
// head rule refused; the head itself names its own resource). Mutations: drop
// the freeze or exclusive write (their row reads "" ); order the head token
// after the resources in admitBlockedTokens (the precedence unit test).
func TestAdmitDiagnosisNamesTheGate(t *testing.T) {
	t.Run("restart freeze", func(t *testing.T) {
		now := time.Unix(370100, 0)
		current := int64(0)
		server := diagServer(t, &current, &now)
		server.restartFreeze = 2 * time.Second
		waiter := laneJob(1, 8000*mib, now) // would fit an empty slice
		queue := queueOf(server, waiter)
		server.armRestartFreeze(now)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, waiter, false)
		if got := blockedTokens(waiter); got != "restart-freeze" {
			t.Fatalf("blocked_by=%q, want restart-freeze", got)
		}
		now = now.Add(3 * time.Second)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, waiter, true)
	})
	t.Run("exclusive holder", func(t *testing.T) {
		now := time.Unix(370200, 0)
		current := int64(0)
		server := diagServer(t, &current, &now)
		holder := laneLease(1, 1000*mib)
		holder.exclusive = true
		waiter := laneJob(2, 1000*mib, now) // fits the ledger
		queue := queueOf(server, holder, waiter)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, waiter, false)
		if got := blockedTokens(waiter); got != "exclusive" {
			t.Fatalf("blocked_by=%q, want exclusive", got)
		}
	})
	t.Run("behind the head", func(t *testing.T) {
		now := time.Unix(370300, 0)
		current := int64(0)
		server := diagServer(t, &current, &now)
		lease := laneLease(1, 38000*mib)
		head := laneJob(2, 8000*mib, now)   // room 45056 − H(3) − 38000 = 4864: refused
		behind := laneJob(3, 2000*mib, now) // fits, but the head's budget is spent
		queue := queueOf(server, lease, head, behind)
		spendBudget(queue, head, now)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, behind, false)
		if got := blockedTokens(head); got != "ram" {
			t.Fatalf("head blocked_by=%q, want ram", got)
		}
		if got := blockedTokens(behind); got != "head" {
			t.Fatalf("behind blocked_by=%q, want head", got)
		}
	})
}

// verifies: AIRA-370 — precedence is a property of the renderer too, so a
// combination one pass never writes still names only the strongest gate.
func TestAdmitBlockedTokensPrecedence(t *testing.T) {
	all := admitBlockedRestartFreeze | admitBlockedExclusive | admitBlockedRAM | admitBlockedCeilingCut | admitBlockedCPU | admitBlockedVRAM
	for _, test := range []struct {
		blocked admitBlocked
		head    bool
		want    []string
	}{
		{all, true, []string{"restart-freeze"}},
		{all &^ admitBlockedRestartFreeze, true, []string{"exclusive"}},
		{admitBlockedRAM | admitBlockedCPU, true, []string{"head"}},
		{admitBlockedVRAM | admitBlockedCPU | admitBlockedCeilingCut | admitBlockedRAM, false, []string{"ram", "ceiling-cut", "cpu", "vram"}},
		{0, false, nil},
	} {
		if got := admitBlockedTokens(test.blocked, test.head); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("tokens(%b, head=%v)=%v, want %v", test.blocked, test.head, got, test.want)
		}
	}
}

// verifies: AIRA-370 — a waiter the pass did not evaluate reports nothing: one
// enqueued since the last pass, and every waiter after a pass that could not
// read the slice (the previous pass's reason is not carried over as current).
// A reason is rewritten, never accumulated, across passes. Mutation: move the
// clear back after the slice read (the read-failure arm goes red); drop the
// clear (the "ram then head" arm reads "ram").
func TestAdmitDiagnosisIsClearedEveryPassAndAbsentWhenUnevaluated(t *testing.T) {
	now := time.Unix(370400, 0)
	current := int64(0)
	server := diagServer(t, &current, &now)
	lease := laneLease(1, 40000*mib)
	waiter := laneJob(2, 8000*mib, now) // room 2880: ram
	queue := queueOf(server, lease, waiter)
	server.evaluateAdmitQueue(queue)
	if got := blockedTokens(waiter); got != "ram" {
		t.Fatalf("blocked_by=%q, want ram", got)
	}
	late := laneJob(3, 1000*mib, now)
	addWaiters(queue, late)
	if got := blockedTokens(late); got != "" {
		t.Fatalf("an unevaluated waiter reports %q", got)
	}

	readOK := server.admitReadMemory
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 0, 0, false, "unreadable" }
	server.evaluateAdmitQueue(queue)
	if got := blockedTokens(waiter); got != "" {
		t.Fatalf("after an unreadable pass blocked_by=%q, want unevaluated", got)
	}
	server.admitReadMemory = readOK

	// The next pass: the late 1000 fits and is granted (it is behind no head
	// with a spent budget: the default budget is half the head's reserve, 4000).
	// The 8000 waiter is still ram; then spend its budget and add a fitting
	// waiter behind it, which reads head, not a stale reason.
	server.evaluateAdmitQueue(queue)
	requireLaneGranted(t, late, true)
	if got := blockedTokens(waiter); got != "ram" {
		t.Fatalf("blocked_by=%q, want ram", got)
	}
	behind := laneJob(4, 500*mib, now)
	addWaiters(queue, behind)
	spendBudget(queue, waiter, now)
	server.evaluateAdmitQueue(queue)
	if got := blockedTokens(behind); got != "head" {
		t.Fatalf("behind blocked_by=%q, want head", got)
	}
	// Room comes back: the 8000 is granted; the behind waiter, now unprotected,
	// is granted in the same pass.
	endLease(queue, lease)
	server.evaluateAdmitQueue(queue)
	requireLaneGranted(t, waiter, true)
	requireLaneGranted(t, behind, true)
}

// verifies: AIRA-370 — the snapshot publishes the diagnosis lane on or off: on
// each queued entry and on the AIRA-24 probe's own waiter, from the same locked
// walk, beside the CPU slot ledger's booked total. Two waiters with different
// diagnoses, each probed by its own scope id, so a copy taken from the wrong
// waiter is seen. Mutations: gate the copy on laneOn (the lane-off run reads
// nil); take the probe's or an entry's tokens from the first queued waiter.
func TestAdmitDiagnosisSnapshot(t *testing.T) {
	for _, lane := range []bool{false, true} {
		now := time.Unix(370500, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		if !lane {
			server.smallLaneCap, server.smallLaneJobMax = 0, 0
		}
		lease := laneLease(1, 1000*mib)
		lease.cpu = 64
		cpuWaiter := laneJob(2, 2000*mib, now)
		// room 45056 − H(3) − 1000 = 41864: too small for 44000, and no slot.
		bothWaiter := laneJob(3, 44000*mib, now)
		queue := queueOf(server, lease, cpuWaiter, bothWaiter)
		server.admitQueue = queue
		server.evaluateAdmitQueue(queue)

		want := map[string][]string{cpuWaiter.scopeID: {"cpu"}, bothWaiter.scopeID: {"ram", "cpu"}}
		var snapshot admitSnapshot
		for scopeID, tokens := range want {
			snapshot = server.admitSliceSnapshotFor("/slice", scopeID)
			if !reflect.DeepEqual(snapshot.queuedBlockedBy, tokens) {
				t.Fatalf("lane=%v probe %s blocked_by=%v, want %v", lane, scopeID, snapshot.queuedBlockedBy, tokens)
			}
		}
		if len(snapshot.queuedEntries) != 2 {
			t.Fatalf("lane=%v entries=%+v", lane, snapshot.queuedEntries)
		}
		for _, entry := range snapshot.queuedEntries {
			if !reflect.DeepEqual(entry.BlockedBy, want[entry.ScopeID]) {
				t.Fatalf("lane=%v entry %s blocked_by=%v, want %v", lane, entry.ScopeID, entry.BlockedBy, want[entry.ScopeID])
			}
		}
		if snapshot.cpuOutstanding != 64 {
			t.Fatalf("cpuOutstanding=%d, want 64", snapshot.cpuOutstanding)
		}
		if other := server.admitSliceSnapshotFor("/slice", "CONFINE-nobody-1-a"); other.queuedBlockedBy != nil {
			t.Fatalf("an unmatched probe got %v", other.queuedBlockedBy)
		}
	}
}

// verifies: AIRA-370 — the confine-list reply carries the diagnosis end to end:
// on each queued row for `--list` (lane off), on the probe's own SliceReserve
// for a caller that names its scope id, beside the CPU slot ledger the `cpu`
// token is about (the evaluator's own R x cores ceiling and booked cores).
// Mutations: drop BlockedBy (or the slot pair) from the reply literal in
// confine_manage.go; publish the ceiling from cpuCoreCounter alone (no R).
func TestConfineListCarriesTheWaitDiagnosis(t *testing.T) {
	leaseID, waiterID := queuedTestID("running", os.Getpid()), queuedTestID("waiting", 7201)
	lease := queuedRowWaiter(1, admitGranted, leaseID, 1<<30, 0)
	lease.cpu = 8
	waiter := queuedRowWaiter(2, admitQueued, waiterID, 1<<30, 0)
	waiter.cpu = 1
	server, path := queuedTestServer(t, []*admitWaiter{lease, waiter})
	server.readCPUCores = func() int { return 4 }
	server.cpuSlotsPerCore = 2 // ceiling 8 slots, all booked by the lease
	queue := admitQueueAtLocked(server, path)
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	server.evaluateAdmitQueue(queue)
	if waiter.state != admitQueued {
		t.Fatalf("fixture: the waiter was granted")
	}

	result := reserveListing(t, server)
	if len(result.Queued) != 1 || !reflect.DeepEqual(result.Queued[0].BlockedBy, []string{"cpu"}) {
		t.Fatalf("queued rows=%+v, want one row blocked_by [cpu]", result.Queued)
	}
	response := server.confineManagement(context.Background(), core.Request{Verb: "confine-list",
		Args: map[string]any{"slice": "aira.slice", "owner": "session-a", "scope_id": waiterID}})
	probe, ok := response.Data.(runner.ConfineListResult)
	if !response.OK || !ok || probe.SliceReserve == nil {
		t.Fatalf("probe response=%+v", response)
	}
	reserve := probe.SliceReserve
	if !reflect.DeepEqual(reserve.BlockedBy, []string{"cpu"}) || reserve.CPUOutstandingCores != 8 || reserve.CPUCeilingCores != 8 {
		t.Fatalf("probe blocked_by=%v cpu=%d/%d, want [cpu] 8/8", reserve.BlockedBy, reserve.CPUOutstandingCores, reserve.CPUCeilingCores)
	}
}
