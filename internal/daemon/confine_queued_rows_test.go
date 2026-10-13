package daemon

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-308. The admission queue's waiters as `confine --list` rows.
//
// verifies: AIRA-308

func queuedTestID(name string, pid int) string {
	return "CONFINE-" + name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@session-a"
}

func queuedTestServer(t *testing.T, waiters []*admitWaiter) (*Server, string) {
	t.Helper()
	path := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, int64(64) << 30, 0, true, "" }
	queue := &sliceQueue{path: path, server: server, waiters: waiters}
	server.admitQueues[path] = queue
	return server, path
}

func queuedRowWaiter(seq int64, state admitWaiterState, scopeID string, reserve, vram int64) *admitWaiter {
	return &admitWaiter{
		seq: seq, reserve: reserve, vram: vram, state: state, accounted: state == admitGranted,
		grantedCh: make(chan struct{}), scopeID: scopeID, name: "n" + strconv.FormatInt(seq, 10), owner: "session-a",
	}
}

// (a) The headline, with a population built so each wrong implementation fails a
// DIFFERENT assertion: a granted scoped waiter, two queued scoped waiters in
// non-sorted id order with distinct reserves, a queued SCOPE-LESS reservation
// between them, a released waiter and a rejected waiter.
//
//   - listing every waiter                       -> granted/released/rejected rows appear
//   - listing scope-less queued reservations     -> a row with no decodable identity
//   - sorting by scope id (as Scopes is)         -> the order flips
//   - reading the granted charge, not the request-> reserves differ
//   - dropping the count                         -> Queued != 4
func TestConfineListPublishesQueuedWaitersInEnqueueOrder(t *testing.T) {
	grantedID := queuedTestID("granted", os.Getpid())
	lateID := queuedTestID("zulu", 7101)   // sorts AFTER first by id, enqueued BEFORE it
	firstID := queuedTestID("alpha", 7102) // sorts first, enqueued second
	server, path := queuedTestServer(t, []*admitWaiter{
		queuedRowWaiter(1, admitGranted, grantedID, 4<<30, 0),
		queuedRowWaiter(2, admitQueued, lateID, 3<<30, 2<<30),
		queuedRowWaiter(3, admitQueued, "", 1<<30, 0), // a per-test confine-reserve hold
		queuedRowWaiter(4, admitReleased, queuedTestID("gone", 7103), 9<<30, 0),
		queuedRowWaiter(5, admitQueued, firstID, 5<<30, 0),
		queuedRowWaiter(6, admitRejected, queuedTestID("rejected", 7104), 8<<30, 0),
	})
	server.admitQueues[path].outstanding, server.admitQueues[path].outstandingJobs = 4<<30, 1

	result := reserveListing(t, server)
	if len(result.Queued) != 2 {
		t.Fatalf("queued rows=%+v, want exactly the two queued SCOPED waiters", result.Queued)
	}
	if result.Queued[0].ScopeID != lateID || result.Queued[1].ScopeID != firstID {
		t.Fatalf("order=%s,%s, want enqueue order %s,%s", result.Queued[0].ScopeID, result.Queued[1].ScopeID, lateID, firstID)
	}
	first, second := result.Queued[0], result.Queued[1]
	if first.Name != "zulu" || first.Owner != "session-a" || first.SupervisorPID == nil || *first.SupervisorPID != 7101 {
		t.Fatalf("identity: %+v", first)
	}
	if first.ReserveBytes == nil || *first.ReserveBytes != 3<<30 || second.ReserveBytes == nil || *second.ReserveBytes != 5<<30 {
		t.Fatalf("reserves=%v,%v, want the requested 3G,5G", first.ReserveBytes, second.ReserveBytes)
	}
	if first.VRAMBytes == nil || *first.VRAMBytes != 2<<30 {
		t.Fatalf("vram=%v, want the requested 2G", first.VRAMBytes)
	}
	for _, record := range result.Scopes {
		if record.ScopeID == lateID || record.ScopeID == firstID {
			t.Fatalf("queued job leaked into Scopes: %+v", record)
		}
	}
	if result.SliceReserve == nil || result.SliceReserve.Queued != 3 {
		t.Fatalf("SliceReserve.Queued=%+v, want the true total 3 (the scope-less hold counts, and has no row)", result.SliceReserve)
	}
}

// Bounded, and honest about it: the cap is the named constant, the rows are the
// FIRST cap in enqueue order, and SliceReserve.Queued still says how many wait.
func TestConfineListQueuedRowsAreBoundedAndTheTotalStaysTrue(t *testing.T) {
	total := runner.ConfineQueuedRowLimit + 6
	waiters := make([]*admitWaiter, 0, total)
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		id := queuedTestID("job"+strconv.Itoa(i), 8000+i)
		ids = append(ids, id)
		waiters = append(waiters, queuedRowWaiter(int64(i+1), admitQueued, id, int64(i+1)<<20, 0))
	}
	server, _ := queuedTestServer(t, waiters)

	result := reserveListing(t, server)
	if len(result.Queued) != runner.ConfineQueuedRowLimit {
		t.Fatalf("rows=%d, want the cap %d", len(result.Queued), runner.ConfineQueuedRowLimit)
	}
	for i, record := range result.Queued {
		if record.ScopeID != ids[i] {
			t.Fatalf("row %d is %s, want the %dth enqueued (%s): truncation must drop the TAIL", i, record.ScopeID, i, ids[i])
		}
	}
	if result.SliceReserve == nil || result.SliceReserve.Queued != total {
		t.Fatalf("SliceReserve.Queued=%+v, want the true total %d", result.SliceReserve, total)
	}
}

// The blocked launcher's own position probe names its scope id and reads only
// SliceReserve; it must keep getting the position and must not be made to pay for
// the rows.
func TestConfineListPositionProbeKeepsItsPositionAndSkipsTheRows(t *testing.T) {
	selfID := queuedTestID("self", os.Getpid())
	server, _ := queuedTestServer(t, []*admitWaiter{
		queuedRowWaiter(1, admitQueued, queuedTestID("head", 7201), 1<<30, 0),
		queuedRowWaiter(2, admitQueued, selfID, 2<<30, 0),
	})
	request := reserveListRequest()
	request.Args["scope_id"] = selfID
	response := server.confineManagement(context.Background(), request)
	result, ok := response.Data.(runner.ConfineListResult)
	if !response.OK || !ok || result.SliceReserve == nil {
		t.Fatalf("response=%+v", response)
	}
	if result.SliceReserve.QueuePosition != 2 || result.SliceReserve.Queued != 2 {
		t.Fatalf("probe lost its position: %+v", result.SliceReserve)
	}
	if len(result.Queued) != 0 {
		t.Fatalf("the position probe was handed %d queued rows it never reads", len(result.Queued))
	}
	// And the same queue, listed by a caller that named no scope id, has them.
	if got := reserveListing(t, server).Queued; len(got) != 2 {
		t.Fatalf("an ordinary listing lost the rows: %+v", got)
	}
}

// A job that is on disk AND still queued in the snapshot (granted between the two
// reads) must never be listed as running and waiting at once.
func TestConfineListNeverListsAScopeAsBothRunningAndQueued(t *testing.T) {
	slice := t.TempDir()
	server := NewServer(Paths{})
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, int64(64) << 30, 0, true, "" }
	onDisk := reserveScopeID(t, "ondisk", os.Getpid())
	reserveScopeDir(t, slice, onDisk, 1<<30, 1<<20)
	waitingID := queuedTestID("waiting", 7301)
	server.admitQueues[slice] = &sliceQueue{path: slice, server: server, waiters: []*admitWaiter{
		queuedRowWaiter(1, admitQueued, onDisk, 1<<30, 0),
		queuedRowWaiter(2, admitQueued, waitingID, 2<<30, 0),
	}}
	result := reserveListing(t, server)
	if len(result.Queued) != 1 || result.Queued[0].ScopeID != waitingID {
		t.Fatalf("queued=%+v, want only the job with no scope on disk", result.Queued)
	}
	if reserveScopeByID(t, result, onDisk).ScopeID != onDisk {
		t.Fatal("the on-disk scope vanished from Scopes")
	}
}

// ci-shim daemon: rows come from the same waiter list with NO /proc read. The
// supervisor pid is this very (readable) test process, so a path that did read
// /proc would produce a Command and fail here.
func TestShimConfineListPublishesQueuedWaitersWithoutLiveReads(t *testing.T) {
	server := shimTestServer(t, 4<<30)
	_, holder := enqueueOn(t, server, runner.ShimConfineSlice, 3<<30)
	waitAdmitGrant(t, holder)
	queue, waiting := enqueueOn(t, server, runner.ShimConfineSlice, 3<<30)
	queuedID := shimTestScopeID()
	queue.mu.Lock()
	state := waiting.state
	waiting.scopeID = queuedID
	queue.mu.Unlock()
	if state != admitQueued {
		t.Fatalf("test premise: second job state=%v, want queued behind the first", state)
	}

	result := reserveListing(t, server)
	if len(result.Queued) != 1 || result.Queued[0].ScopeID != queuedID {
		t.Fatalf("queued=%+v", result.Queued)
	}
	row := result.Queued[0]
	if row.Command != nil || row.Cwd != nil || row.Pane != nil {
		t.Fatalf("shim queued row read /proc: %+v", row)
	}
	if row.ReserveBytes == nil || *row.ReserveBytes != 3<<30 {
		t.Fatalf("reserve=%v", row.ReserveBytes)
	}
}

// The rows are copied out in the SAME locked pass that counts the queue. Hammer a
// queue whose waiters flip between queued and released under queue.mu: with every
// waiter scoped and the population below the cap, the rows and the count must
// agree in EVERY snapshot. A second lock acquisition (or a read of queue.waiters
// outside the pass) shows up as a row/count disagreement between the two.
func TestQueuedRowsAndCountComeFromOneLockedPass(t *testing.T) {
	const population = 12
	waiters := make([]*admitWaiter, 0, population)
	for i := 0; i < population; i++ {
		waiters = append(waiters, queuedRowWaiter(int64(i+1), admitQueued, queuedTestID("w"+strconv.Itoa(i), 9000+i), 1<<20, 0))
	}
	server, path := queuedTestServer(t, waiters)
	queue := server.admitQueues[path]

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		flip := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			queue.mu.Lock()
			for i, waiter := range queue.waiters {
				if (i+flip)%3 == 0 {
					waiter.state = admitReleased
				} else {
					waiter.state = admitQueued
				}
			}
			queue.mu.Unlock()
			flip++
		}
	}()
	for i := 0; i < 2000; i++ {
		snapshot := server.admitSliceSnapshotFor(path, "")
		if len(snapshot.queuedEntries) != snapshot.queued {
			close(stop)
			wg.Wait()
			t.Fatalf("snapshot %d: %d rows vs queued count %d — two different instants", i, len(snapshot.queuedEntries), snapshot.queued)
		}
	}
	close(stop)
	wg.Wait()
}
