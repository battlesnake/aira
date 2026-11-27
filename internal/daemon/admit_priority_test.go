package daemon

import (
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-340. `aira confine --priority high`: a launch-time queue tier between
// --front pushes and normal jobs, FIFO among itself, queue order ONLY (a high
// head is an ordinary unpushed head: AIRA-307 budget and backstop apply).

func highWaiter(seq int64, name, owner string) *admitWaiter {
	waiter := frontWaiter(seq, name, owner)
	waiter.high = true
	return waiter
}

func orderNames(queue *sliceQueue) string {
	names := []string{}
	for _, waiter := range admitOrderLocked(queue, nil) {
		names = append(names, waiter.name)
	}
	return strings.Join(names, ",")
}

// verifies: AIRA-340 — the three-tier order: --front pushes (latest first),
// then high jobs in ENQUEUE order, then normal jobs in enqueue order. Mutations
// this reds against: order ignores high (high treated as normal); high tier
// latest-first; high tier ahead of the pushed tier; a pushed high job listed
// twice or in the high tier.
func TestAdmitOrderPushedThenHighFIFOThenNormal(t *testing.T) {
	now := time.Unix(70000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	n1 := frontWaiter(1, "n1", "s-1")
	h1 := highWaiter(2, "h1", "s-1")
	n2 := frontWaiter(3, "n2", "s-1")
	h2 := highWaiter(4, "h2", "s-1")
	h3 := highWaiter(5, "h3", "s-1")
	n3 := frontWaiter(6, "n3", "s-1")
	queue := queueOf(server, n1, h1, n2, h2, h3, n3)

	if got, want := orderNames(queue), "h1,h2,h3,n1,n2,n3"; got != want {
		t.Fatalf("order=%s, want %s (high FIFO before normal FIFO)", got, want)
	}
	mustPush(t, queue, n2.scopeID)
	if got, want := orderNames(queue), "n2,h1,h2,h3,n1,n3"; got != want {
		t.Fatalf("order after pushing n2=%s, want %s (a push beats every high job)", got, want)
	}
	mustPush(t, queue, h3.scopeID)
	if got, want := orderNames(queue), "h3,n2,h1,h2,n1,n3"; got != want {
		t.Fatalf("order after pushing h3=%s, want %s (a pushed high job is in the pushed tier, latest push first)", got, want)
	}
	// A granted high job leaves the order entirely.
	h1.state = admitGranted
	if got, want := orderNames(queue), "h3,n2,h2,n1,n3"; got != want {
		t.Fatalf("order after h1 granted=%s, want %s", got, want)
	}
}

// verifies: AIRA-340 — the dispatch consequences of the order. Room for one
// 4 GiB job: a high job launched AFTER two normal jobs is granted first; of two
// high jobs the earlier one is; a --front push beats a high job. Mutations: the
// order ignores high (n1 is granted); high latest-first (h2 is granted); high
// ahead of pushed (h is granted).
func TestAdmitHighDispatchesBeforeEarlierNormalsFIFOAndBehindPushes(t *testing.T) {
	t.Run("high before earlier normals", func(t *testing.T) {
		now := time.Unix(71000, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		n1, n2 := frontWaiter(2, "n1", "s-1"), frontWaiter(3, "n2", "s-1")
		h := highWaiter(4, "h", "s-1")
		n1.reserve, n2.reserve, h.reserve = 4*qGiB, 4*qGiB, 4*qGiB
		queue := queueOf(server, heldLease(1, 60*qGiB), n1, n2, h) // room 4 GiB

		server.evaluateAdmitQueue(queue)

		waitAdmitGrant(t, h)
		requireAdmitQueued(t, n1)
		requireAdmitQueued(t, n2)
		if queue.outstanding != 64*qGiB {
			t.Fatalf("outstanding=%d, want exactly the 64 GiB ceiling", queue.outstanding)
		}
	})
	t.Run("two highs are FIFO", func(t *testing.T) {
		now := time.Unix(71100, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		h1, h2 := highWaiter(2, "h1", "s-1"), highWaiter(3, "h2", "s-1")
		h1.reserve, h2.reserve = 4*qGiB, 4*qGiB
		queue := queueOf(server, heldLease(1, 60*qGiB), h1, h2)

		server.evaluateAdmitQueue(queue)

		waitAdmitGrant(t, h1)
		requireAdmitQueued(t, h2)
	})
	t.Run("a push beats a high", func(t *testing.T) {
		now := time.Unix(71200, 0)
		server := queueOrderServer(t, 64*qGiB, &now)
		h := highWaiter(2, "h", "s-1")
		p := frontWaiter(3, "p", "s-1")
		h.reserve, p.reserve = 4*qGiB, 4*qGiB
		queue := queueOf(server, heldLease(1, 60*qGiB), h, p)
		mustPush(t, queue, p.scopeID)

		server.evaluateAdmitQueue(queue)

		waitAdmitGrant(t, p)
		requireAdmitQueued(t, h)
	})
}

// verifies: AIRA-340 decision 3 — a high head is an ORDINARY head, not a pushed
// one: a normal job overtakes it only through its AIRA-307 bypass budget
// (reserve/2), a jumper over the budget is held and arms the duty cycle, and the
// reported phase is never "pushed". Mutations: give a high head the pushed rule
// (`case head.pushSeq > 0 || head.high`: the in-budget jumper is held); exempt a
// high head from the budget (the over-budget jumper is granted).
func TestAdmitHighHeadUsesTheBypassBudgetNotThePushedRule(t *testing.T) {
	now := time.Unix(72000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	head := highWaiter(2, "head", "s-1")
	head.reserve = 24 * qGiB // budget 12 GiB
	inBudget := queuedWaiter(3, 8*qGiB, now)
	overBudget := queuedWaiter(4, 5*qGiB, now)                                  // 8+5 > 12
	queue := queueOf(server, heldLease(1, 44*qGiB), head, inBudget, overBudget) // room 20 GiB
	registerAdmitQueue(server, queue)

	server.evaluateAdmitQueue(queue)

	requireAdmitQueued(t, head)
	waitAdmitGrant(t, inBudget)
	requireAdmitQueued(t, overBudget)
	if head.bypassed != 8*qGiB {
		t.Fatalf("high head bypassed=%d, want the 8 GiB jumper counted against its budget", head.bypassed)
	}
	if queue.freezeHolderSeq != head.seq {
		t.Fatalf("freezeHolderSeq=%d, want the high head %d", queue.freezeHolderSeq, head.seq)
	}
	if queue.freezeArmedAt.IsZero() {
		t.Fatal("the budget withheld a fitting grant behind a high head but did not arm the hold")
	}
	if phase := server.admitSliceSnapshot("/slice").phase; phase == admitFreezePhasePushed {
		t.Fatalf("a high (unpushed) head is reported %q", phase)
	}
}

// verifies: AIRA-340 — the starvation consequence of the ordering decision,
// pinned so it is a visible choice: a FITTING high job is evaluated before an
// earlier normal head is identified, so it is granted without being charged to
// that head's budget. A stream of high jobs can therefore keep a large normal
// head waiting; that is why the guidance keeps high for short hooked steps.
func TestAdmitFittingHighIsGrantedAheadOfANormalHeadWithoutBudgetCharge(t *testing.T) {
	now := time.Unix(73000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	head := queuedWaiter(2, 24*qGiB, now) // budget 12 GiB
	high := highWaiter(3, "big-high", "s-1")
	high.reserve = 13 * qGiB // over the head's budget, fits the 20 GiB room
	queue := queueOf(server, heldLease(1, 44*qGiB), head, high)

	server.evaluateAdmitQueue(queue)

	waitAdmitGrant(t, high)
	requireAdmitQueued(t, head)
	if head.bypassed != 0 {
		t.Fatalf("head.bypassed=%d, want 0: the high job was ahead of it, not a bypass", head.bypassed)
	}
}

// verifies: AIRA-340 — ci-shim mode orders high exactly as the real slice does
// (queue state only, like --front there). Mutation: order ignores high -> n is
// granted.
func TestAdmitHighOrderInCIShimMode(t *testing.T) {
	now := time.Unix(74000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	server.confineMode = runner.ConfineModeShim
	if !server.shimMode() {
		t.Fatal("precondition: shim mode")
	}
	n := frontWaiter(2, "n", "s-1")
	h := highWaiter(3, "h", "s-1")
	n.reserve, h.reserve = 4*qGiB, 4*qGiB
	queue := queueOf(server, heldLease(1, 60*qGiB), n, h)

	server.evaluateAdmitQueue(queue)

	waitAdmitGrant(t, h)
	requireAdmitQueued(t, n)
}

// verifies: AIRA-340 — the client's queue position, the queued-ahead total and
// the queued rows read the same order the evaluator runs, and each row says its
// priority. Mutations: drop High from the queued entry (rows lose the flag);
// order ignores high (positions shift).
func TestAdmitSnapshotFollowsTheHighTier(t *testing.T) {
	now := time.Unix(75000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	a, b := frontWaiter(1, "alpha", "s-1"), frontWaiter(2, "bravo", "s-1")
	h := highWaiter(3, "hotel", "s-1")
	a.reserve, b.reserve, h.reserve = 1*qGiB, 2*qGiB, 3*qGiB
	queue := queueOf(server, a, b, h)
	registerAdmitQueue(server, queue)

	if s := server.admitSliceSnapshotFor("/slice", h.scopeID, nil); s.queuePosition != 1 || s.queuedAheadBytes != 0 {
		t.Fatalf("high position=%d ahead=%d, want 1/0", s.queuePosition, s.queuedAheadBytes)
	}
	snapshot := server.admitSliceSnapshotFor("/slice", a.scopeID, nil)
	if snapshot.queuePosition != 2 || snapshot.queuedAheadBytes != 3*qGiB {
		t.Fatalf("alpha position=%d ahead=%d, want 2 behind the high job's 3 GiB", snapshot.queuePosition, snapshot.queuedAheadBytes)
	}
	var ids []string
	for _, entry := range snapshot.queuedEntries {
		ids = append(ids, entry.ScopeID)
		if entry.High != (entry.ScopeID == h.scopeID) {
			t.Fatalf("entry %s High=%v", entry.ScopeID, entry.High)
		}
	}
	if want := []string{h.scopeID, a.scopeID, b.scopeID}; strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("queuedEntries order=%v, want %v", ids, want)
	}
}

// verifies: AIRA-340 — a queued high HEAD that ends without a grant leaves no
// ghost, whether it is cancelled (AIRA-328 `--kill`) or times out: it leaves
// the evaluation order, the next pass grants the jumper its budget was holding,
// it is no longer the recorded head, and the ledger holds only real grants.
// Mutation: keep high waiters in the order regardless of state
// (`waiter.state != admitQueued && !waiter.high`) -> it stays the head.
func TestAdmitHighHeadEndingWithoutAGrantLeavesNoGhost(t *testing.T) {
	for _, end := range []string{"cancelled", "timed out"} {
		t.Run(end, func(t *testing.T) {
			now := time.Unix(76000, 0)
			server := queueOrderServer(t, 64*qGiB, &now)
			held := heldLease(1, 44*qGiB)
			head := highWaiter(2, "head", "s-1")
			head.reserve = 24 * qGiB
			jumper := queuedWaiter(3, 13*qGiB, now) // over the 12 GiB budget, fits the 20 GiB room
			queue := queueOf(server, held, head, jumper)
			server.evaluateAdmitQueue(queue)
			requireAdmitQueued(t, jumper)
			if queue.freezeHolderSeq != head.seq {
				t.Fatalf("precondition: the high head is protected (holder=%d)", queue.freezeHolderSeq)
			}
			outstanding := queue.outstanding

			if end == "cancelled" {
				if _, handled, err := cancelQueuedConfineLocked(queue, "head", "s-1", false, now); !handled || err != nil {
					t.Fatalf("cancel high head: handled=%v err=%v", handled, err)
				}
				requireCancelled(t, head)
			} else {
				server.timeoutAdmitWaiter(queue, head)
				if head.state != admitRejected {
					t.Fatalf("timed-out head state=%v", head.state)
				}
			}
			for _, waiter := range admitOrderLocked(queue, nil) {
				if waiter == head {
					t.Fatal("the ended high waiter is still in the evaluation order")
				}
			}
			server.evaluateAdmitQueue(queue)
			waitAdmitGrant(t, jumper)
			if queue.freezeHolderSeq == head.seq {
				t.Fatal("the ended high head is still recorded as the protected head")
			}
			if queue.outstanding != outstanding+13*qGiB {
				t.Fatalf("outstanding=%d, want %d (held + the granted jumper only)", queue.outstanding, outstanding+13*qGiB)
			}
		})
	}
}

// verifies: AIRA-340 — the `priority` admit field: "high" sets it on both the
// scoped and the scope-less return, "normal" and absence leave it off, any
// other word or type is refused (never read as normal or high), and a
// sub-reservation cannot claim it. Mutations: drop `high: high` from either
// return literal; accept an unknown word; drop the parent_scope_id refusal.
func TestValidateAdmitArgsPriority(t *testing.T) {
	scoped := func(extra map[string]any) map[string]any {
		args := map[string]any{"slice": "aira.slice", "reserve": int64(1 << 20), "scope_id": "CONFINE-job-123-abc@s-1", "name": "job", "owner": "s-1"}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	scopeless := func(extra map[string]any) map[string]any {
		args := map[string]any{"slice": "aira.slice", "reserve": int64(1 << 20), "pinned": true}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	for _, test := range []struct {
		name string
		args map[string]any
		high bool
	}{
		{"scoped high", scoped(map[string]any{"priority": "high"}), true},
		{"scoped normal", scoped(map[string]any{"priority": "normal"}), false},
		{"scoped absent", scoped(nil), false},
		{"scope-less high", scopeless(map[string]any{"priority": "high"}), true},
		{"scope-less absent", scopeless(nil), false},
		{"every optional field at once", scoped(map[string]any{"priority": "high", "cpu": int64(1), "vram": int64(1 << 20), "max_wait_ms": int64(1000), "signature": "sig", "pinned": true, "exclusive_holder": "CONFINE-holder-9-zz@s-1"}), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := validateAdmitArgs(test.args, int64(time.Hour/time.Millisecond))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if request.high != test.high {
				t.Fatalf("high=%v, want %v", request.high, test.high)
			}
		})
	}
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"unknown word", scoped(map[string]any{"priority": "urgent"})},
		{"upper case", scoped(map[string]any{"priority": "HIGH"})},
		{"empty", scoped(map[string]any{"priority": ""})},
		{"bool", scoped(map[string]any{"priority": true})},
		{"sub-reservation", scopeless(map[string]any{"priority": "high", "parent_scope_id": "CONFINE-suite-77-aa@s-1"})},
	} {
		t.Run("refused "+test.name, func(t *testing.T) {
			if _, err := validateAdmitArgs(test.args, int64(time.Hour/time.Millisecond)); err == nil || admitErrorCode(err) != CodeProtocol {
				t.Fatalf("err=%v code=%q, want a %s refusal", err, admitErrorCode(err), CodeProtocol)
			}
		})
	}
}
