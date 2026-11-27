package daemon

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-381. Session priority tiers: the order (§3.2), the lane (§3.4) and the
// order-only contract (§3.3), on the AIRA-307/309 evaluator seam
// (queueOrderServer, queueOf: one evaluateAdmitQueue call per pass, fake clock
// and memory, zero headroom). T-numbers are the spec's §5.

// tierJob is a queued, scoped job whose scope id carries owner ("" for none).
func tierJob(seq int64, name, owner string, reserve int64, high bool) *admitWaiter {
	waiter := frontWaiter(seq, name, owner)
	waiter.reserve, waiter.high = reserve, high
	return waiter
}

// publishTiers installs a session table on the server, as a load or a set does.
func publishTiers(server *Server, marks map[string]sessionTier) {
	table := make(map[string]sessionTier, len(marks))
	for owner, tier := range marks {
		table[owner] = tier
	}
	server.tiers.Store(&table)
}

func namesOf(waiters []*admitWaiter) string {
	names := make([]string, 0, len(waiters))
	for _, waiter := range waiters {
		names = append(names, waiter.name)
	}
	return strings.Join(names, ",")
}

// preTierOrder is an ORACLE copy of the pre-381 admitOrderLocked: pushed
// (latest first), then high, then the rest, both in enqueue order.
func preTierOrder(queue *sliceQueue) []*admitWaiter {
	var pushed, high, rest []*admitWaiter
	for _, waiter := range queue.waiters {
		if waiter == nil || waiter.state != admitQueued {
			continue
		}
		switch {
		case waiter.pushSeq > 0:
			pushed = append(pushed, waiter)
		case waiter.high:
			high = append(high, waiter)
		default:
			rest = append(rest, waiter)
		}
	}
	for i := 1; i < len(pushed); i++ {
		for j := i; j > 0 && pushed[j].pushSeq > pushed[j-1].pushSeq; j-- {
			pushed[j], pushed[j-1] = pushed[j-1], pushed[j]
		}
	}
	return append(append(pushed, high...), rest...)
}

// randomTierQueue is a generated queue holding every waiter kind the order
// sees: pushed, job-high and normal owned waiters, sub-reservations (their
// worker scope id carries the parent's owner), scope-less and ownerless ones,
// and some that are not queued. owners are the owner strings used.
func randomTierQueue(server *Server, rng *rand.Rand, owners []string, size int) *sliceQueue {
	queue := queueOf(server)
	for i := 0; i < size; i++ {
		seq := int64(i + 1)
		var waiter *admitWaiter
		switch kind := rng.Intn(10); {
		case kind < 5: // owned
			waiter = tierJob(seq, "j"+strconv.Itoa(i), owners[rng.Intn(len(owners))], qGiB, rng.Intn(3) == 0)
		case kind == 5: // sub-reservation of an owned parent
			waiter = tierJob(seq, "w"+strconv.Itoa(i), owners[rng.Intn(len(owners))], qGiB, false)
			waiter.parentScopeID = "CONFINE-parent-1-a"
		case kind == 6: // scope-less (`aira run`, a confine-reserve hold)
			waiter = queuedWaiter(seq, qGiB, time.Unix(0, 0))
			waiter.name, waiter.owner, waiter.high = "s"+strconv.Itoa(i), owners[0], rng.Intn(3) == 0
		case kind == 7: // scoped, no owner tail, though the request claimed one (I6)
			waiter = tierJob(seq, "u"+strconv.Itoa(i), "", qGiB, rng.Intn(3) == 0)
			waiter.owner = owners[rng.Intn(len(owners))]
		default: // owned, then not queued
			waiter = tierJob(seq, "g"+strconv.Itoa(i), owners[rng.Intn(len(owners))], qGiB, false)
			waiter.state = []admitWaiterState{admitGranted, admitReleased, admitRejected}[rng.Intn(3)]
		}
		queue.waiters = append(queue.waiters, waiter)
		queue.seq = seq
	}
	// Push a few queued waiters, in a random order.
	for _, index := range rng.Perm(len(queue.waiters)) {
		waiter := queue.waiters[index]
		if waiter.state == admitQueued && rng.Intn(5) == 0 {
			queue.seq++
			waiter.pushSeq = queue.seq
		}
	}
	return queue
}

// verifies: AIRA-381 T1 — with no marks the order is today's, element by
// element, against an oracle copy of the pre-381 function, over generated
// queues; a table naming only owners that are not queued changes nothing
// either. Mutations: drop the job-high sub-level (buckets ignore waiter.high);
// put unpushed waiters anywhere but today's place.
func TestSessionTiersT1EmptyTableIsTodaysOrder(t *testing.T) {
	now := time.Unix(381000, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	rng := rand.New(rand.NewSource(381))
	owners := []string{"field", "cc-1a2b3c4d-0000-4000-8000-000000000001", "@cwd-repo", "b"}
	for trial := 0; trial < 400; trial++ {
		queue := randomTierQueue(server, rng, owners, 1+rng.Intn(14))
		want := namesOf(preTierOrder(queue))
		for _, tiers := range []map[string]sessionTier{nil, {}, {"absent-a": tierHigh, "absent-b": tierLow}} {
			if got := namesOf(admitOrderLocked(queue, tiers)); got != want {
				t.Fatalf("trial %d tiers %v: order %s, want today's %s", trial, tiers, got, want)
			}
		}
	}
}

// verifies: AIRA-381 T2 (I1) — untiered waiters stay MEDIUM. Marking every
// owner HIGH gives pushed, owned job-high, owned normal, untiered job-high,
// untiered normal; every owner LOW gives pushed, untiered job-high, untiered
// normal, owned job-high, owned normal. Untiered: a sub-reservation (whose
// worker id carries a marked owner), a scope-less waiter and a scoped waiter
// with no owner tail (whose request claimed a marked owner). Mutations: tier a
// sub-reservation by its scope id; tier by waiter.owner.
func TestSessionTiersT2UntieredWaitersAreMedium(t *testing.T) {
	now := time.Unix(381100, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	sub := tierJob(2, "sub", "a", qGiB, false)
	sub.parentScopeID = "CONFINE-parent-1-a"
	scopeless := queuedWaiter(3, qGiB, now)
	scopeless.name, scopeless.owner, scopeless.high = "scopeless", "a", true
	ownerless := tierJob(4, "ownerless", "", qGiB, false)
	ownerless.owner = "a"
	queue := queueOf(server,
		tierJob(1, "an", "a", qGiB, false), sub, scopeless, ownerless,
		tierJob(5, "bh", "b", qGiB, true), tierJob(6, "pushed", "b", qGiB, false), tierJob(7, "bn", "b", qGiB, false))
	mustPush(t, queue, queue.waiters[5].scopeID)
	for _, test := range []struct {
		tier sessionTier
		want string
	}{
		{tierHigh, "pushed,bh,an,bn,scopeless,sub,ownerless"},
		{tierLow, "pushed,scopeless,sub,ownerless,bh,an,bn"},
	} {
		if got := namesOf(admitOrderLocked(queue, map[string]sessionTier{"a": test.tier, "b": test.tier})); got != test.want {
			t.Fatalf("every owner %s: order %s, want %s", test.tier, got, test.want)
		}
	}
}

// verifies: AIRA-381 T3 (D2) — the full order over {HIGH, MEDIUM, LOW} x
// {job-high, normal} x {pushed, not}: pushes latest first whatever their tier,
// then the tier is the primary key and job priority the sub-level, enqueue
// order inside each bucket; a LOW job-high waiter is after a MEDIUM normal one.
// Mutations: swap the tier and job-priority keys; rank an unmarked owner LOW.
func TestSessionTiersT3TierIsPrimaryJobPriorityTheSubLevel(t *testing.T) {
	now := time.Unix(381200, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	var waiters []*admitWaiter
	seq := int64(0)
	for _, round := range []string{"1", "2"} {
		for _, owner := range []string{"lo", "me", "hi"} {
			for _, high := range []bool{false, true} {
				seq++
				name := owner + map[bool]string{true: "H", false: "n"}[high] + round
				waiters = append(waiters, tierJob(seq, name, owner, qGiB, high))
			}
		}
	}
	queue := queueOf(server, waiters...)
	// Push a LOW normal, then a HIGH job-high, then a MEDIUM normal: latest first.
	mustPush(t, queue, waiters[0].scopeID) // lon1
	mustPush(t, queue, waiters[5].scopeID) // hiH1
	mustPush(t, queue, waiters[8].scopeID) // men2
	tiers := map[string]sessionTier{"hi": tierHigh, "lo": tierLow}
	want := "men2,hiH1,lon1," + "hiH2," + "hin1,hin2," + "meH1,meH2," + "men1," + "loH1,loH2," + "lon2"
	if got := namesOf(admitOrderLocked(queue, tiers)); got != want {
		t.Fatalf("order %s\nwant  %s", got, want)
	}
}

// verifies: AIRA-381 T4 — the listing's queued rows, position and queued-ahead
// figures follow the order the evaluator runs. Lane off, equal 4 GiB jobs, room
// for one at a time, mixed tiers: after each pass the job granted is the one
// `confine-list` listed first, through the real confine-list case (which loads
// the table itself). Mutations: the snapshot ordered without the table (nil
// passed from the confine-list case, or admitSliceSnapshotFor ignoring it).
func TestSessionTiersT4ListingFollowsTheEvaluator(t *testing.T) {
	ids := map[string]string{}
	var waiters []*admitWaiter
	for index, owner := range []string{"lo", "me", "hi", "me", "lo", "hi"} {
		name := owner + strconv.Itoa(index)
		id := queuedTestID(name, 7200+index)
		ids[id] = name
		waiter := queuedRowWaiter(int64(index+2), admitQueued, strings.Replace(id, "@session-a", "@"+owner, 1), 4<<30, 0)
		waiter.name = name
		waiters = append(waiters, waiter)
	}
	held := queuedRowWaiter(1, admitGranted, "", 60<<30, 0)
	server, path := queuedTestServer(t, append([]*admitWaiter{held}, waiters...))
	server.admitSliceHeadroomBase, server.admitSliceHeadroomSupervisor = 0, 0
	queue := admitQueueAtLocked(server, path)
	queue.kick = make(chan struct{}, 1)
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh, "lo": tierLow})
	var granted []string
	for pass := 0; pass < len(waiters); pass++ {
		listing := reserveListing(t, server)
		if len(listing.Queued) == 0 {
			t.Fatalf("pass %d: nothing listed", pass)
		}
		first := listing.Queued[0].Name
		server.evaluateAdmitQueue(queue)
		var now []string
		for _, waiter := range waiters {
			if waiter.state == admitGranted {
				now = append(now, waiter.name)
				waiter.state = admitReleased // release it for the next pass
			}
		}
		queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
		if len(now) != 1 || now[0] != first {
			t.Fatalf("pass %d: granted %v, but the listing put %s first", pass, now, first)
		}
		granted = append(granted, first)
	}
	if got, want := strings.Join(granted, ","), "hi2,hi5,me1,me3,lo0,lo4"; got != want {
		t.Fatalf("grant order %s, want %s", got, want)
	}
}

// verifies: AIRA-381 T5 — a push beats a tier, latest push first: pushed LOW A
// then B behind waiting HIGH jobs, room for one at a time: B, A, then the HIGH
// jobs. Mutations: rank the tier above the push; pushes not latest-first.
func TestSessionTiersT5PushBeatsTier(t *testing.T) {
	now := time.Unix(381500, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	h1, h2 := tierJob(2, "h1", "hi", 4*qGiB, false), tierJob(3, "h2", "hi", 4*qGiB, true)
	a, b := tierJob(4, "a", "lo", 4*qGiB, false), tierJob(5, "b", "lo", 4*qGiB, false)
	held := heldLease(1, 60*qGiB)
	queue := queueOf(server, held, h1, h2, a, b)
	mustPush(t, queue, a.scopeID)
	mustPush(t, queue, b.scopeID)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh, "lo": tierLow})
	var order []string
	for pass := 0; pass < 4; pass++ {
		server.evaluateAdmitQueue(queue)
		for _, waiter := range []*admitWaiter{h1, h2, a, b} {
			if waiter.state == admitGranted {
				order = append(order, waiter.name)
				endLease(queue, waiter)
			}
		}
	}
	if got, want := strings.Join(order, ","), "b,a,h2,h1"; got != want {
		t.Fatalf("grant order %s, want %s", got, want)
	}
}

// verifies: AIRA-381 T8 (I3) — no per-waiter state: marking an already-queued
// MEDIUM session LOW reorders it on the next pass, and setting it back to
// medium restores the order. Through the real setter (store write seamed).
// Mutation: the tier copied onto the waiter at enqueue.
func TestSessionTiersT8MarkReordersQueuedWaiters(t *testing.T) {
	now := time.Unix(381800, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	publishTiers(server, nil)
	server.replaceSessionTiersFn = func(context.Context, map[string]string) error { return nil }
	x, y := tierJob(2, "x", "sx", 4*qGiB, false), tierJob(3, "y", "sy", 4*qGiB, false)
	queue := queueOf(server, heldLease(1, 60*qGiB), x, y)
	order := func() string { return namesOf(admitOrderLocked(queue, tierTable(server.tiers.Load()))) }
	if got := order(); got != "x,y" {
		t.Fatalf("unmarked order %s", got)
	}
	setTier(t, server, "sx", "low")
	if got := order(); got != "y,x" {
		t.Fatalf("after marking sx LOW: order %s, want y,x", got)
	}
	setTier(t, server, "sx", "medium")
	if got := order(); got != "x,y" {
		t.Fatalf("after setting sx back to medium: order %s, want x,y", got)
	}
	setTier(t, server, "sx", "low")
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, y)
	requireAdmitQueued(t, x)
}

func setTier(t *testing.T, server *Server, session, tier string) runner.ConfineSessionTierResult {
	t.Helper()
	result, err := server.setSessionTier(context.Background(), "operator", map[string]any{"session": session, "tier": tier})
	if err != nil {
		t.Fatalf("set %s %s: %v", session, tier, err)
	}
	return result
}

// verifies: AIRA-381 T9 — a sub-reservation is never tiered by its parent: the
// parent's owner LOW, its (older) sub-reservation then a MEDIUM job-level
// waiter, room for one: the sub-reservation is granted, as with no marks.
// Mutation: tier a sub-reservation by the owner in its worker scope id.
func TestSessionTiersT9SubReservationIsUntiered(t *testing.T) {
	now := time.Unix(381900, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	sub := tierJob(2, "worker", "lo", 4*qGiB, false)
	sub.parentScopeID = "CONFINE-parent-1-a@lo"
	medium := tierJob(3, "medium", "me", 4*qGiB, false)
	queue := queueOf(server, heldLease(1, 60*qGiB), sub, medium)
	publishTiers(server, map[string]sessionTier{"lo": tierLow})
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, sub)
	requireAdmitQueued(t, medium)
}

// verifies: AIRA-381 T24 (I6) — the tier key is the owner in the scope id,
// never the request's claim: a scope id with no owner tail whose request
// claimed `field`, `field` HIGH, behind an older MEDIUM waiter, room for one:
// the older waiter is granted. Mutation: key read from waiter.owner.
func TestSessionTiersT24KeyIsTheScopeIDOwner(t *testing.T) {
	now := time.Unix(382400, 0)
	server := queueOrderServer(t, 64*qGiB, &now)
	older := tierJob(2, "older", "other", 4*qGiB, false)
	claimed := tierJob(3, "claimed", "", 4*qGiB, false)
	claimed.owner = "field"
	queue := queueOf(server, heldLease(1, 60*qGiB), older, claimed)
	publishTiers(server, map[string]sessionTier{"field": tierHigh})
	if got := namesOf(admitOrderLocked(queue, tierTable(server.tiers.Load()))); got != "older,claimed" {
		t.Fatalf("order %s, want older,claimed (the claim is not the key)", got)
	}
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, older)
	requireAdmitQueued(t, claimed)
}

// verifies: AIRA-381 T10 (D6, both directions) — order only: lane off, 14 of 32
// GiB running, no pin discount. A HIGH head H of 20 GiB is refused (room 18);
// behind it, enqueued L1 LOW 4, L2 LOW 4, M MEDIUM 4 GiB, all fitting. H's
// budget is 10: M then L1 are granted (8 <= 10), L2 is withheld (12 > 10), the
// anchor arms and the phase is hold; the next backstop pass grants L2.
// Mutations: no tier order (FIFO: L1, L2 granted, M withheld); a zero-budget
// arm for a lower tier (L1 withheld); the budget skipped for lower tiers (L2
// granted in the first pass).
func TestSessionTiersT10OrderOnlyUnderTheBudget(t *testing.T) {
	now := time.Unix(383000, 0)
	server := queueOrderServer(t, 32*qGiB, &now)
	head := tierJob(2, "H", "hi", 20*qGiB, false)
	l1, l2 := tierJob(3, "L1", "lo", 4*qGiB, false), tierJob(4, "L2", "lo", 4*qGiB, false)
	m := tierJob(5, "M", "me", 4*qGiB, false)
	queue := queueOf(server, heldLease(1, 14*qGiB), head, l1, l2, m)
	registerAdmitQueue(server, queue)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh, "lo": tierLow})

	server.evaluateAdmitQueue(queue)

	requireAdmitQueued(t, head)
	waitAdmitGrant(t, m)
	waitAdmitGrant(t, l1)
	requireAdmitQueued(t, l2)
	if head.bypassed != 8*qGiB {
		t.Fatalf("head bypassed=%d GiB, want 8", head.bypassed/qGiB)
	}
	if queue.freezeArmedAt.IsZero() {
		t.Fatal("the withheld L2 did not arm the hold")
	}
	if phase := server.admitSliceSnapshot("/slice").phase; phase != admitFreezeHold.String() {
		t.Fatalf("phase %q, want hold", phase)
	}
	now = now.Add(server.admitFreezeMaxHold + time.Second)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, l2)
	requireAdmitQueued(t, head)
}

// cloneTierWaiter copies the fields the evaluator reads, with a fresh channel.
func cloneTierWaiter(waiter *admitWaiter) *admitWaiter {
	return &admitWaiter{
		seq: waiter.seq, reserve: waiter.reserve, cpu: waiter.cpu, state: waiter.state, accounted: waiter.accounted,
		grantedCh: make(chan struct{}), enqueued: waiter.enqueued, scopeID: waiter.scopeID, name: waiter.name,
		owner: waiter.owner, parentScopeID: waiter.parentScopeID, high: waiter.high, pushSeq: waiter.pushSeq,
	}
}

// verifies: AIRA-381 T29 (I2) — production is its own oracle. Generated queues,
// tables and passes, lane off: run A orders by the table; run B holds the same
// waiters ENQUEUED in A's evaluation order, with no table and `high` cleared
// (pushes kept), so today's evaluator walks B in exactly A's order. Every pass
// makes identical grants, bypassed, anchor and phase in both. Mutation: any
// tier-aware condition in the head switch (e.g. a LOW waiter refused while a
// head is protected).
func TestSessionTiersT29OrderOnlyAgainstProduction(t *testing.T) {
	rng := rand.New(rand.NewSource(3810029))
	owners := []string{"hi", "me", "lo", "x"}
	for trial := 0; trial < 150; trial++ {
		now := time.Unix(384000+int64(trial)*1000, 0)
		serverA := queueOrderServer(t, 32*qGiB, &now)
		serverB := queueOrderServer(t, 32*qGiB, &now)
		marks := map[string]sessionTier{}
		for _, owner := range owners {
			if tier := sessionTier(rng.Intn(3)); tier != tierMedium {
				marks[owner] = tier
			}
		}
		publishTiers(serverA, marks)
		var population []*admitWaiter
		held := heldLease(1, int64(rng.Intn(20))*qGiB)
		population = append(population, held)
		for i := 0; i < 3+rng.Intn(10); i++ {
			seq := int64(i + 2)
			waiter := tierJob(seq, "j"+strconv.Itoa(i), owners[rng.Intn(len(owners))], int64(1+rng.Intn(16))*qGiB, rng.Intn(3) == 0)
			switch rng.Intn(6) {
			case 0:
				waiter.parentScopeID = "CONFINE-parent-1-a"
			case 1:
				waiter.scopeID = ""
			}
			population = append(population, waiter)
		}
		queueA := queueOf(serverA, population...)
		for _, waiter := range population {
			if waiter.state == admitQueued && rng.Intn(6) == 0 {
				queueA.seq++
				waiter.pushSeq = queueA.seq
			}
		}
		// B: held first, then A's evaluation order with high cleared and pushes kept.
		bWaiters := []*admitWaiter{cloneTierWaiter(held)}
		pairs := map[*admitWaiter]*admitWaiter{held: bWaiters[0]}
		for _, waiter := range admitOrderLocked(queueA, marks) {
			clone := cloneTierWaiter(waiter)
			clone.high = false
			bWaiters = append(bWaiters, clone)
			pairs[waiter] = clone
		}
		queueB := queueOf(serverB, bWaiters...)
		queueB.seq = queueA.seq
		registerAdmitQueue(serverA, queueA)
		registerAdmitQueue(serverB, queueB)
		for pass := 0; pass < 6; pass++ {
			serverA.evaluateAdmitQueue(queueA)
			serverB.evaluateAdmitQueue(queueB)
			for a, b := range pairs {
				if a.state != b.state || a.bypassed != b.bypassed {
					t.Fatalf("trial %d pass %d waiter %s: A state %v bypassed %d, B state %v bypassed %d",
						trial, pass, a.name, a.state, a.bypassed, b.state, b.bypassed)
				}
			}
			if !queueA.freezeArmedAt.Equal(queueB.freezeArmedAt) || queueA.freezeHolderSeq != queueB.freezeHolderSeq {
				t.Fatalf("trial %d pass %d: anchor A %v/%d, B %v/%d", trial, pass,
					queueA.freezeArmedAt, queueA.freezeHolderSeq, queueB.freezeArmedAt, queueB.freezeHolderSeq)
			}
			if pa, pb := serverA.admitSliceSnapshot("/slice").phase, serverB.admitSliceSnapshot("/slice").phase; pa != pb {
				t.Fatalf("trial %d pass %d: phase A %s, B %s", trial, pass, pa, pb)
			}
			// Release a random granted job in both, then move the clock.
			for a, b := range pairs {
				if a != held && a.state == admitGranted && rng.Intn(3) == 0 {
					endLease(queueA, a)
					endLease(queueB, b)
				}
			}
			now = now.Add(time.Duration(rng.Intn(90)) * time.Second)
		}
	}
}

// verifies: AIRA-381 T13 — a tier is never a gate: a LOW job alone on an empty
// slice is granted in the first pass.
func TestSessionTiersT13LowAloneIsGranted(t *testing.T) {
	now := time.Unix(385000, 0)
	server := queueOrderServer(t, 32*qGiB, &now)
	low := tierJob(1, "low", "lo", 4*qGiB, false)
	queue := queueOf(server, low)
	publishTiers(server, map[string]sessionTier{"lo": tierLow})
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, low)
}

// verifies: AIRA-381 T30 — no wedge: running MEDIUM R (14 of 32 GiB), HIGH head
// H of 20 GiB, R's nested 2 GiB child C (same owner, an ordinary waiter), lane
// off: C is granted within H's budget; R released; H granted. Mutation: a tier
// hold of any kind (C withheld behind the HIGH head).
func TestSessionTiersT30NestedChildBehindAHighHead(t *testing.T) {
	now := time.Unix(386000, 0)
	server := queueOrderServer(t, 32*qGiB, &now)
	running := heldLease(1, 14*qGiB)
	running.scopeID = "CONFINE-r-6001-a@me"
	head := tierJob(2, "H", "hi", 20*qGiB, false)
	child := tierJob(3, "C", "me", 2*qGiB, false)
	queue := queueOf(server, running, head, child)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh})
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, child)
	requireAdmitQueued(t, head)
	endLease(queue, running)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, head)
}

// tierLaneServer is the seam with the lane on, ZERO headroom (unlike
// laneServer), a 32 GiB ceiling and 64 CPU slots; observed collects the lane
// grants in order.
func tierLaneServer(t *testing.T, laneCap int64, now *time.Time) (*Server, *[]string) {
	t.Helper()
	server := queueOrderServer(t, 32*qGiB, now)
	server.smallLaneCap, server.smallLaneJobMax = laneCap, qGiB
	server.readCPUCores = func() int { return 32 }
	observed := &[]string{}
	server.smallLaneGrantObserver = func(_ *sliceQueue, granted, _ *admitWaiter, _ laneOccupancy, _ int64) {
		*observed = append(*observed, granted.name)
	}
	return server, observed
}

// tierLaneJob is a queued, scoped, lane-kind job of one core with an owner.
func tierLaneJob(seq int64, name, owner string, reserve int64, high bool) *admitWaiter {
	waiter := tierJob(seq, name, owner, reserve, high)
	waiter.laneKind, waiter.cpu = true, 1
	return waiter
}

const halfGiB = qGiB / 2

// verifies: AIRA-381 T6 (D4) — lane candidates ignore the tier: lane room for
// one, an older LOW small job is lane-granted and a newer HIGH small job's
// laneWait is full. Mutation: lane candidates in evaluation order.
func TestSessionTiersT6LaneIgnoresTier(t *testing.T) {
	now := time.Unix(387000, 0)
	server, observed := tierLaneServer(t, halfGiB, &now)
	low, high := tierLaneJob(1, "low", "lo", halfGiB, false), tierLaneJob(2, "high", "hi", halfGiB, false)
	queue := queueOf(server, low, high)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh, "lo": tierLow})
	server.evaluateAdmitQueue(queue)
	if got := strings.Join(*observed, ","); got != "low" {
		t.Fatalf("lane grants %q, want low", got)
	}
	requireLaneWait(t, high, laneWaitFull)
}

// verifies: AIRA-381 T6b (D4) — lane candidates ignore job priority: lane room
// for one, same session, an older normal small job is lane-granted and a newer
// `--priority high` one's laneWait is full. Mutation: lane candidates keep
// job-high first (the pre-381 order).
func TestSessionTiersT6bLaneIgnoresJobPriority(t *testing.T) {
	now := time.Unix(387100, 0)
	server, observed := tierLaneServer(t, halfGiB, &now)
	normal, high := tierLaneJob(1, "normal", "me", halfGiB, false), tierLaneJob(2, "high", "me", halfGiB, true)
	queue := queueOf(server, normal, high)
	server.evaluateAdmitQueue(queue)
	if got := strings.Join(*observed, ","); got != "normal" {
		t.Fatalf("lane grants %q, want normal", got)
	}
	requireLaneWait(t, high, laneWaitFull)
}

// verifies: AIRA-381 T6c — lane candidates keep the push first: zero headroom,
// 32 GiB, 14 running; enqueue MEDIUM small A 0.5, HIGH B 20 (refused), then
// push small P 0.5: P and A are lane-granted in one pass and B's bypassed
// stays 0. Mutation: plain FIFO candidates (A meets the pushed reference P and
// is laneWaitPushed; the walk then grants it charged to B: bypassed 0.5).
func TestSessionTiersT6cLaneKeepsPushFirst(t *testing.T) {
	now := time.Unix(387200, 0)
	server, observed := tierLaneServer(t, 8*qGiB, &now)
	a := tierLaneJob(2, "A", "me", halfGiB, false)
	b := tierLaneJob(3, "B", "hi", 20*qGiB, false)
	p := tierLaneJob(4, "P", "me", halfGiB, false)
	queue := queueOf(server, heldLease(1, 14*qGiB), a, b, p)
	mustPush(t, queue, p.scopeID)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh})
	server.evaluateAdmitQueue(queue)
	if got := strings.Join(*observed, ","); got != "P,A" {
		t.Fatalf("lane grants %q, want P,A", got)
	}
	requireAdmitQueued(t, b)
	if b.bypassed != 0 {
		t.Fatalf("B bypassed=%d, want 0 (A went through the lane, not past B)", b.bypassed)
	}
}

// verifies: AIRA-381 T7 — the lane reference stays in EVALUATION order: 32 GiB,
// 26 granted, no small leases; enqueue MEDIUM 8, HIGH 31.5, LOW 1 GiB small.
// The reference is the HIGH waiter (31.5 + 1 > 32), so the small job is kept
// and never lane-granted. Mutation: the reference taken from the candidate
// (arrival) order (MEDIUM 8: 8 + 1 <= 32, the small job lane-granted).
func TestSessionTiersT7LaneReferenceInEvaluationOrder(t *testing.T) {
	now := time.Unix(387300, 0)
	server, observed := tierLaneServer(t, 8*qGiB, &now)
	m := tierLaneJob(2, "M", "me", 8*qGiB, false)
	h := tierLaneJob(3, "H", "hi", 31*qGiB+halfGiB, false)
	s := tierLaneJob(4, "S", "lo", qGiB, false)
	queue := queueOf(server, heldLease(1, 26*qGiB), m, h, s)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh, "lo": tierLow})
	server.evaluateAdmitQueue(queue)
	if len(*observed) != 0 {
		t.Fatalf("lane grants %v, want none", *observed)
	}
	requireLaneWait(t, s, laneWaitKept)
}

// verifies: AIRA-381 T7b — the reference advances in evaluation coordinates:
// zero headroom, 32 GiB, lane cap 1.5 GiB; enqueue LOW L 0.5, MEDIUM M 31,
// HIGH H 0.5, LOW S 0.5 (evaluation H, M, L, S; candidates L, M, H, S). One
// pass: L and H lane-granted; M is the reference; S kept (31 + 1.5 > 32); M
// granted by the walk (31 + 1 = 32). Mutation: the reference advanced from the
// candidate position (`nextRef(index + 1)`): from H's candidate index 2 it
// lands on S, S is lane-granted and M refused.
func TestSessionTiersT7bReferenceAdvancesInEvaluationOrder(t *testing.T) {
	now := time.Unix(387400, 0)
	server, observed := tierLaneServer(t, qGiB+halfGiB, &now)
	l := tierLaneJob(1, "L", "lo", halfGiB, false)
	m := tierLaneJob(2, "M", "me", 31*qGiB, false)
	h := tierLaneJob(3, "H", "hi", halfGiB, false)
	s := tierLaneJob(4, "S", "lo", halfGiB, false)
	queue := queueOf(server, l, m, h, s)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh, "lo": tierLow})
	server.evaluateAdmitQueue(queue)
	if got := strings.Join(*observed, ","); got != "L,H" {
		t.Fatalf("lane grants %q, want L,H", got)
	}
	requireLaneWait(t, s, laneWaitKept)
	waitAdmitGrant(t, m)
	requireAdmitQueued(t, s)
}

// verifies: AIRA-381 T7c — the reference moves only when it is the one
// granted: same box; enqueue LOW A 0.5, HIGH B 31.5, MEDIUM C 0.5 (evaluation
// B, C, A; candidates A, B, C). One pass: A lane-granted; B stays the
// reference; C kept (31.5 + 1 > 32); B granted by the walk (32 <= 32).
// Mutation: today's `if index == reference` lines kept (A's candidate index 0
// equals the reference index 0, the reference jumps to C: C lane-granted, B
// refused).
func TestSessionTiersT7cReferenceMovesOnlyWhenGranted(t *testing.T) {
	now := time.Unix(387500, 0)
	server, observed := tierLaneServer(t, qGiB+halfGiB, &now)
	a := tierLaneJob(1, "A", "lo", halfGiB, false)
	b := tierLaneJob(2, "B", "hi", 31*qGiB+halfGiB, false)
	c := tierLaneJob(3, "C", "me", halfGiB, false)
	queue := queueOf(server, a, b, c)
	publishTiers(server, map[string]sessionTier{"hi": tierHigh, "lo": tierLow})
	server.evaluateAdmitQueue(queue)
	if got := strings.Join(*observed, ","); got != "A" {
		t.Fatalf("lane grants %q, want A", got)
	}
	requireLaneWait(t, c, laneWaitKept)
	waitAdmitGrant(t, b)
	requireAdmitQueued(t, c)
}

// verifies: AIRA-381 T18 (daemon half) — bad values are refused
// E_CONFINE_ARGUMENT_INVALID before anything is touched: unknown, empty,
// over-long and malformed sessions, a bad tier, and a slice key (empty or not,
// T19c's daemon half). Mutations: accept `unknown`; accept a fourth tier;
// ignore a slice key.
func TestSessionTiersT18SetterRefusesBadValues(t *testing.T) {
	server := NewServer(Paths{})
	publishTiers(server, nil)
	writes := 0
	server.replaceSessionTiersFn = func(context.Context, map[string]string) error { writes++; return nil }
	for _, args := range []map[string]any{
		{"session": "unknown", "tier": "high"},
		{"session": "", "tier": "high"},
		{"session": strings.Repeat("a", 65), "tier": "high"},
		{"session": "bad owner", "tier": "high"},
		{"session": "a/b", "tier": "low"},
		{"session": "field", "tier": "urgent"},
		{"session": "field", "tier": ""},
		{"session": "field", "tier": "High"},
		{"session": "field", "tier": "high", "slice": ""},
		{"session": "field", "tier": "high", "slice": "aira.slice"},
	} {
		response := server.confineManagement(context.Background(), coreRequest("confine-session-tier", withOwner(args)))
		if response.OK || response.Code != "E_CONFINE_ARGUMENT_INVALID" || response.Exit != 2 {
			t.Fatalf("args %v: response %+v, want E_CONFINE_ARGUMENT_INVALID exit 2", args, response)
		}
	}
	if writes != 0 || len(*server.tiers.Load()) != 0 {
		t.Fatalf("a refused set touched the table (writes %d, table %v)", writes, *server.tiers.Load())
	}
	// The accepted forms, including an inferred "@cwd-" owner (L4).
	for _, session := range []string{"field", "@cwd-repo", "cc-1a2b3c4d-0000-4000-8000-000000000001", strings.Repeat("a", 64)} {
		response := server.confineManagement(context.Background(), coreRequest("confine-session-tier", withOwner(map[string]any{"session": session, "tier": "low"})))
		if !response.OK {
			t.Fatalf("session %q refused: %+v", session, response)
		}
	}
}

// verifies: AIRA-381 T20 — the 64-mark cap refuses a NEW mark only, with
// E_CONFINE_SESSION_TIERS_FULL exit 1 and nothing changed; re-setting a marked
// session and removing one both pass at the cap. Mutations: no cap; the cap
// applied to a re-set or a removal.
func TestSessionTiersT20Cap(t *testing.T) {
	server := NewServer(Paths{})
	marks := map[string]sessionTier{}
	for i := 0; i < runner.ConfineSessionTierCap; i++ {
		marks[fmt.Sprintf("s%02d", i)] = tierLow
	}
	publishTiers(server, marks)
	writes := 0
	server.replaceSessionTiersFn = func(context.Context, map[string]string) error { writes++; return nil }
	response := server.confineManagement(context.Background(), coreRequest("confine-session-tier", withOwner(map[string]any{"session": "new", "tier": "high"})))
	if response.Code != "E_CONFINE_SESSION_TIERS_FULL" || response.Exit != 1 {
		t.Fatalf("65th mark: %+v, want E_CONFINE_SESSION_TIERS_FULL exit 1", response)
	}
	if writes != 0 || len(*server.tiers.Load()) != runner.ConfineSessionTierCap {
		t.Fatalf("a refused set changed something: writes %d, %d marks", writes, len(*server.tiers.Load()))
	}
	setTier(t, server, "s00", "high")
	if (*server.tiers.Load())["s00"] != tierHigh {
		t.Fatal("re-set at the cap did not apply")
	}
	setTier(t, server, "s01", "medium")
	if _, still := (*server.tiers.Load())["s01"]; still {
		t.Fatal("remove at the cap did not apply")
	}
	setTier(t, server, "new", "high")
}

// verifies: AIRA-381 (nit 3) — an UNEVALUATED table (a Server that never loaded
// one: ci-shim, a failed load) refuses every set with the one refusal,
// E_CONFINE_UNAVAILABLE exit 4, naming both causes, and writes nothing.
// Mutation: a nil table treated as empty (the set accepted, a store write that
// would delete rows the daemon never read).
func TestSessionTiersNilTableRefusesSets(t *testing.T) {
	for _, shim := range []bool{false, true} {
		server := NewServer(Paths{})
		if shim {
			server.confineMode = runner.ConfineModeShim
		}
		writes := 0
		server.replaceSessionTiersFn = func(context.Context, map[string]string) error { writes++; return nil }
		response := server.confineManagement(context.Background(), coreRequest("confine-session-tier", withOwner(map[string]any{"session": "field", "tier": "high"})))
		if response.Code != "E_CONFINE_UNAVAILABLE" || response.Exit != 4 ||
			!strings.Contains(response.Error, "ci-shim") || !strings.Contains(response.Error, "did not load") {
			t.Fatalf("shim=%v: %+v, want E_CONFINE_UNAVAILABLE naming ci-shim and a failed load", shim, response)
		}
		if writes != 0 || server.tiers.Load() != nil {
			t.Fatalf("shim=%v: the refused set wrote (%d) or published", shim, writes)
		}
	}
}

func coreRequest(verb string, args map[string]any) core.Request {
	return core.Request{Verb: verb, Args: args}
}

func withOwner(args map[string]any) map[string]any {
	out := map[string]any{"owner": "operator"}
	for key, value := range args {
		out[key] = value
	}
	return out
}
