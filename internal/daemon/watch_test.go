package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/store"
)

func watchServer(t *testing.T, interval time.Duration) (*Server, WorktreeScope, *store.Store) {
	t.Helper()
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	server.watchPollInterval = interval
	server.stopping = make(chan struct{})
	scope := testScope(t, paths, "watch")
	view, _, err := server.storeForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	return server, scope, view
}

func watchData(t *testing.T, response core.Response) WatchResponse {
	t.Helper()
	if !response.OK {
		t.Fatalf("watch failed: %+v", response)
	}
	data, ok := response.Data.(WatchResponse)
	if !ok {
		t.Fatalf("watch data type %T", response.Data)
	}
	return data
}

func addWatchAllocation(t *testing.T, view *store.Store) int64 {
	t.Helper()
	if _, err := view.AllocateID(context.Background(), "AIRA"); err != nil {
		t.Fatal(err)
	}
	seq, err := view.CurrentMaxSeq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func insertWatchEvents(t *testing.T, server *Server, scope WorktreeScope, count int) {
	t.Helper()
	db, err := sql.Open("sqlite", server.Paths.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for index := 1; index <= count; index++ {
		if _, err := db.Exec(`INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest,journaled) VALUES(?,?,?,?,?,?,?,0)`,
			scope.ProjectID, index, time.Now().UTC().Format(time.RFC3339Nano), "test", "ticket.change", fmt.Sprintf("AIRA-%d", index), "digest"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWatchFromNowSamplesMaxAndIsPaced(t *testing.T) {
	server, scope, view := watchServer(t, 25*time.Millisecond)
	want := addWatchAllocation(t, view)
	start := time.Now()
	data := watchData(t, server.watch(context.Background(), scope, map[string]any{"from_now": true, "wait_ms": 1}))
	if elapsed := time.Since(start); elapsed < server.watchPollInterval {
		t.Fatalf("from_now returned in %v, before %v", elapsed, server.watchPollInterval)
	}
	if data.Cursor != want || len(data.Events) != 0 || data.EOF {
		t.Fatalf("data=%+v want cursor=%d empty non-eof", data, want)
	}
}

func TestWatchReturnsConcurrentEventWithinPollInterval(t *testing.T) {
	server, scope, view := watchServer(t, 30*time.Millisecond)
	done := make(chan core.Response, 1)
	start := time.Now()
	go func() {
		done <- server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 500})
	}()
	time.Sleep(10 * time.Millisecond)
	want := addWatchAllocation(t, view)
	response := <-done
	data := watchData(t, response)
	if data.Cursor != want || len(data.Events) != 1 || data.Events[0].Seq != want || data.EOF {
		t.Fatalf("data=%+v", data)
	}
	if elapsed := time.Since(start); elapsed > 4*server.watchPollInterval {
		t.Fatalf("event latency=%v poll=%v", elapsed, server.watchPollInterval)
	}
}

func TestWatchFilteredCursorAdvancesPastExcludedRows(t *testing.T) {
	server, scope, view := watchServer(t, 15*time.Millisecond)
	wantCursor := addWatchAllocation(t, view)
	data := watchData(t, server.watch(context.Background(), scope, map[string]any{
		"from": int64(0), "verbs": []string{"lease.release"}, "wait_ms": 1,
	}))
	if len(data.Events) != 0 || data.Cursor != wantCursor {
		t.Fatalf("filtered data=%+v", data)
	}
	var scannedFrom []int64
	server.watchEventsSince = func(ctx context.Context, view *store.Store, from int64, limit int) ([]store.WatchEvent, int64, error) {
		scannedFrom = append(scannedFrom, from)
		return view.EventsSince(ctx, from, limit)
	}
	next := watchData(t, server.watch(context.Background(), scope, map[string]any{
		"from": data.Cursor, "verbs": []string{"lease.release"}, "wait_ms": 20,
	}))
	if next.Cursor != wantCursor || len(scannedFrom) == 0 || scannedFrom[0] != wantCursor {
		t.Fatalf("next=%+v scannedFrom=%v", next, scannedFrom)
	}
}

func TestWatchTimeoutLeavesCursorAndEOFUnchanged(t *testing.T) {
	server, scope, _ := watchServer(t, 20*time.Millisecond)
	start := time.Now()
	data := watchData(t, server.watch(context.Background(), scope, map[string]any{"from": int64(7), "wait_ms": 45}))
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("timeout elapsed=%v", elapsed)
	}
	if data.Cursor != 7 || len(data.Events) != 0 || data.EOF {
		t.Fatalf("data=%+v", data)
	}
}

func TestWatchShutdownTerminalDrainAndOverflowBoundaries(t *testing.T) {
	t.Run("concurrent event", func(t *testing.T) {
		server, scope, view := watchServer(t, 200*time.Millisecond)
		stopping := server.stopping
		done := make(chan core.Response, 1)
		go func() {
			done <- server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 1000})
		}()
		time.Sleep(20 * time.Millisecond)
		seq := addWatchAllocation(t, view)
		close(stopping)
		data := watchData(t, <-done)
		if !data.EOF || data.Cursor != seq || len(data.Events) != 1 || data.Events[0].Seq != seq {
			t.Fatalf("terminal data=%+v", data)
		}
	})
	for _, count := range []int{watchBatchCap, watchBatchCap + 1} {
		t.Run(fmt.Sprintf("pending-%d", count), func(t *testing.T) {
			server, scope, _ := watchServer(t, 2*time.Millisecond)
			insertWatchEvents(t, server, scope, count)
			close(server.stopping)
			data := watchData(t, server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 1}))
			if !data.EOF || len(data.Events) != watchBatchCap || data.Cursor != watchBatchCap {
				t.Fatalf("terminal data count=%d cursor=%d eof=%v", len(data.Events), data.Cursor, data.EOF)
			}
			if count == watchBatchCap+1 {
				// The overflow beyond batchCap is NOT lost: the client, on eof,
				// reconnects from the terminal cursor to a fresh (running) daemon.
				// A non-stopping server (open `stopping`) models that fresh daemon,
				// which serves the remaining seq from data.Cursor.
				server.stopping = make(chan struct{})
				remainder := watchData(t, server.watch(context.Background(), scope, map[string]any{"from": data.Cursor, "wait_ms": 1}))
				if len(remainder.Events) != 1 || remainder.Events[0].Seq != watchBatchCap+1 {
					t.Fatalf("remainder=%+v", remainder)
				}
			}
		})
	}
}

func TestWatchTerminalDrainFailureIsTransientWithoutCursor(t *testing.T) {
	server, scope, _ := watchServer(t, 2*time.Millisecond)
	close(server.stopping)
	server.watchEventsSince = func(ctx context.Context, _ *store.Store, from int64, _ int) ([]store.WatchEvent, int64, error) {
		<-ctx.Done()
		return nil, from, ctx.Err()
	}
	response := server.watch(context.Background(), scope, map[string]any{"from": int64(19), "wait_ms": 1})
	if response.OK || response.Code != CodeUnavailable || response.Data != nil {
		t.Fatalf("response=%+v", response)
	}
}

func TestWatchPeerCloseCancelsAndLongPollDoesNotMonopolizeDB(t *testing.T) {
	server, scope, view := watchServer(t, 100*time.Millisecond)
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), serverConn)
		close(done)
	}()
	if err := writeFrame(clientConn, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "watch", Args: map[string]any{"from": int64(0), "wait_ms": 1000}}}); err != nil {
		t.Fatal(err)
	}
	queryDone := make(chan error, 1)
	go func() { _, err := view.CurrentMaxSeq(context.Background()); queryDone <- err }()
	select {
	case err := <-queryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("long poll monopolized the database connection")
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("peer close did not promptly cancel watch")
	}
}

func TestWatchAdmissionCapAndSlotRecovery(t *testing.T) {
	server, scope, _ := watchServer(t, 3*time.Millisecond)
	for index := 0; index < watchMaxConcurrent; index++ {
		server.watchSlots <- struct{}{}
	}
	busy := server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 1})
	if busy.Code != CodeBusy {
		t.Fatalf("busy response=%+v", busy)
	}
	for index := 0; index < watchMaxConcurrent; index++ {
		<-server.watchSlots
	}
	_ = watchData(t, server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 1}))
	if len(server.watchSlots) != 0 {
		t.Fatalf("timeout leaked admission: %d", len(server.watchSlots))
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = server.watch(canceled, scope, map[string]any{"from": int64(0), "wait_ms": 100})
	if len(server.watchSlots) != 0 {
		t.Fatalf("cancel leaked admission: %d", len(server.watchSlots))
	}

	server.stopping = make(chan struct{})
	close(server.stopping)
	_ = server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 100})
	if len(server.watchSlots) != 0 {
		t.Fatalf("shutdown leaked admission: %d", len(server.watchSlots))
	}

	server.stopping = make(chan struct{})
	server.watchEventsSince = func(context.Context, *store.Store, int64, int) ([]store.WatchEvent, int64, error) { panic("test") }
	func() {
		defer func() { _ = recover() }()
		server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 1})
	}()
	if len(server.watchSlots) != 0 {
		t.Fatalf("panic leaked admission: %d", len(server.watchSlots))
	}
}

func TestWatchMinimumDurationAndCoincidentShutdownPriority(t *testing.T) {
	server, scope, _ := watchServer(t, 20*time.Millisecond)
	insertWatchEvents(t, server, scope, 1)
	start := time.Now()
	data := watchData(t, server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 1}))
	if elapsed := time.Since(start); elapsed < server.watchPollInterval || len(data.Events) != 1 {
		t.Fatalf("backlog elapsed=%v data=%+v", elapsed, data)
	}

	for iteration := 0; iteration < 20; iteration++ {
		server, scope, _ := watchServer(t, 50*time.Millisecond)
		insertWatchEvents(t, server, scope, 1)
		stopping := server.stopping
		var once sync.Once
		server.watchAfterWake = func() { once.Do(func() { close(stopping) }) }
		data := watchData(t, server.watch(context.Background(), scope, map[string]any{"from": int64(0), "wait_ms": 1}))
		if !data.EOF {
			t.Fatalf("iteration %d returned normal response: %+v", iteration, data)
		}
	}
}

func TestWatchNonReadingClientWriteIsBounded(t *testing.T) {
	server, scope, _ := watchServer(t, time.Millisecond)
	insertWatchEvents(t, server, scope, watchBatchCap)
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), serverConn)
		close(done)
	}()
	if err := writeFrame(clientConn, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "watch", Args: map[string]any{"from": int64(0), "wait_ms": 1}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(watchWriteTimeout + time.Second):
		t.Fatal("non-reading peer held response write beyond deadline")
	}
	_ = clientConn.Close()
}

func TestWatchCanceledTerminalDrainNeverClaimsEOF(t *testing.T) {
	server, scope, _ := watchServer(t, time.Millisecond)
	close(server.stopping)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := server.watch(ctx, scope, map[string]any{"from": int64(4), "wait_ms": 1})
	if response.Code != CodeUnavailable {
		t.Fatalf("response=%+v", response)
	}
	var data WatchResponse
	if response.Data != nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("canceled terminal response=%+v data=%+v", response, data)
	}
}

func TestWatchInt64CursorAbove2Pow53IsExactViaString(t *testing.T) {
	// A cursor is sent as a decimal string so it survives the request-arg decode
	// without float64 rounding. 2^53+1 is the first int not representable as a
	// float64 (float64(2^53+1) == 2^53), so the string path must preserve it and
	// the float64 path must NOT (Sol build r1 #3).
	const cursor int64 = (1 << 53) + 1
	if got := watchInt64(strconv.FormatInt(cursor, 10)); got != cursor {
		t.Fatalf("string cursor decoded to %d, want %d", got, cursor)
	}
	if lossy := watchInt64(float64(cursor)); lossy == cursor {
		t.Fatalf("float64 cursor unexpectedly exact (%d) — the string encoding would be pointless", lossy)
	}
}
