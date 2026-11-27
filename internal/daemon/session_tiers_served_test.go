package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
	"aira/internal/testdeadline"
)

// AIRA-381. The session table on a SERVED daemon (Serve's one load, the store,
// the setter's publication order), over the real socket where the spec asks.

// serveTiers starts server on paths and returns a stop that cancels Serve and
// waits for it to return (so a second Server can take the same state dir).
func serveTiers(t *testing.T, server *Server, slice string) func() {
	t.Helper()
	server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 32 * qGiB, 0, true, "" }
	server.restartFreeze = 0
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-testdeadline.After(5 * time.Second):
				t.Error("daemon did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func socketSet(t *testing.T, paths Paths, session, tier string) core.Response {
	t.Helper()
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: WorktreeScope{},
		Request: core.Request{Verb: "confine-session-tier", Args: map[string]any{"session": session, "tier": tier, "owner": "operator"}}})
	if err != nil {
		t.Fatal(err)
	}
	return response.CoreResponse()
}

func socketList(t *testing.T, paths Paths) (core.Response, map[string]json.RawMessage) {
	t.Helper()
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: WorktreeScope{},
		Request: core.Request{Verb: "confine-list", Args: map[string]any{"slice": "aira.slice", "owner": "operator"}}})
	if err != nil {
		t.Fatal(err)
	}
	reply := response.CoreResponse()
	raw, _ := json.Marshal(reply.Data)
	if len(reply.RawData) > 0 {
		raw = reply.RawData
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("confine-list data %s: %v", raw, err)
	}
	return reply, fields
}

func storedTiers(t *testing.T, paths Paths) map[string]string {
	t.Helper()
	conn, err := sql.Open("sqlite", paths.DBPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.Query(`SELECT owner, tier FROM confine_session_tiers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	marks := map[string]string{}
	for rows.Next() {
		var owner, tier string
		if err := rows.Scan(&owner, &tier); err != nil {
			t.Fatal(err)
		}
		marks[owner] = tier
	}
	return marks
}

// seedTiers writes rows straight into a fresh state database (schema from OpenDB).
func seedTiers(t *testing.T, paths Paths, rows map[string]string) {
	t.Helper()
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("sqlite", paths.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for owner, tier := range rows {
		if _, err := conn.Exec(`INSERT INTO confine_session_tiers(owner, tier) VALUES(?, ?)`, owner, tier); err != nil {
			t.Fatal(err)
		}
	}
}

// captureLog redirects the standard logger to a lockedBuffer for the test. The
// daemon's goroutines keep logging while the test reads, so every read goes
// through the buffer's locked String.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return buffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// verifies: AIRA-381 T12 (D7, I8) — marks persist across a daemon restart: set
// `field` HIGH over the socket, stop the daemon, serve a NEW Server on the same
// state dir: its table is {field: high}; a survivor of `field` re-declares and
// keeps its reserve; a new `field` waiter queued behind MEDIUM ones is granted
// first (room for one); `confine-list` lists `field high`. Mutation: Serve does
// not load the marks (the restarted table is empty).
func TestSessionTiersT12MarksSurviveARestart(t *testing.T) {
	paths := testPaths(t)
	slice := t.TempDir()
	stop := serveTiers(t, NewServer(paths), slice)
	if response := socketSet(t, paths, "field", "high"); !response.OK {
		t.Fatalf("set: %+v", response)
	}
	stop()

	restarted := NewServer(paths)
	serveTiers(t, restarted, slice)
	published := restarted.tiers.Load()
	if published == nil || len(*published) != 1 || (*published)["field"] != tierHigh {
		t.Fatalf("restarted table %v, want {field: high}", published)
	}
	// A survivor re-declares: granted at once, its reserve unchanged.
	_, survivor, code, err := restarted.enqueueReDeclare("/survivors", 2*qGiB, "pinned:client", admitRequest{
		reserve: 2 * qGiB, cpu: 1, scopeID: "CONFINE-survivor-6100-a@field", name: "survivor", owner: "field", peerSameUID: true, signature: "plain",
	})
	if err != nil || survivor == nil || survivor.state != admitGranted || survivor.reserve != 2*qGiB {
		t.Fatalf("survivor re-declare: code %s err %v waiter %+v", code, err, survivor)
	}
	// The first pass orders the new field waiter HIGH.
	m1, m2 := tierJob(2, "m1", "me", 4*qGiB, false), tierJob(3, "m2", "me", 4*qGiB, false)
	fieldJob := tierJob(4, "fieldjob", "field", 4*qGiB, false)
	queue := queueOf(restarted, heldLease(1, 28*qGiB), m1, m2, fieldJob)
	restarted.admitSliceHeadroomBase, restarted.admitSliceHeadroomSupervisor = 0, 0
	restarted.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, fieldJob)
	requireAdmitQueued(t, m1)
	_, fields := socketList(t, paths)
	if got := string(fields["session_tiers"]); got != `[{"owner":"field","tier":"high"}]` {
		t.Fatalf("listed session_tiers %s", got)
	}
}

// verifies: AIRA-381 T12b (I8) — a failed load (a stored owner the setter would
// refuse; more than 64 rows) leaves the daemon SERVING with an unevaluated
// table: ordered as no marks, no session_tiers in the list reply, the journal
// naming the cause and the manual sqlite remedy, every set refused
// E_CONFINE_UNAVAILABLE exit 4, and the stored rows untouched. Mutations: the
// load failure refuses start; a failed load reported as an empty ("none set")
// table; a set accepted on an unloaded table.
func TestSessionTiersT12bFailedLoadIsUnevaluated(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i <= runner.ConfineSessionTierCap; i++ {
		tooMany[fmt.Sprintf("s%02d", i)] = "low"
	}
	for name, rows := range map[string]map[string]string{
		"bad owner": {"field": "high", "bad owner!": "low"},
		"over cap":  tooMany,
	} {
		t.Run(name, func(t *testing.T) {
			paths := testPaths(t)
			seedTiers(t, paths, rows)
			journal := captureLog(t)
			server := NewServer(paths)
			serveTiers(t, server, t.TempDir())
			if server.tiers.Load() != nil {
				t.Fatalf("table published after a failed load: %v", *server.tiers.Load())
			}
			text := journal.String()
			if !strings.Contains(text, "session tiers: did not load") || !strings.Contains(text, "sqlite3 "+paths.DBPath) ||
				!strings.Contains(text, "DELETE FROM confine_session_tiers") {
				t.Fatalf("journal does not name the cause and the remedy:\n%s", text)
			}
			if _, fields := socketList(t, paths); fields["session_tiers"] != nil {
				t.Fatalf("an unevaluated table was listed: %s", fields["session_tiers"])
			}
			response := socketSet(t, paths, "other", "high")
			if response.Code != "E_CONFINE_UNAVAILABLE" || response.Exit != 4 {
				t.Fatalf("set on an unloaded table: %+v", response)
			}
			if got := storedTiers(t, paths); len(got) != len(rows) {
				t.Fatalf("stored rows changed: %d, want %d", len(got), len(rows))
			}
			// Ordered as no marks: field's job is not ahead of an older one.
			older, fieldJob := tierJob(1, "older", "me", qGiB, false), tierJob(2, "fieldjob", "field", qGiB, true)
			queue := queueOf(server, older, fieldJob)
			if got := namesOf(admitOrderLocked(queue, tierTable(server.tiers.Load()))); got != "fieldjob,older" {
				t.Fatalf("order %s, want today's (job-high first)", got)
			}
		})
	}
}

// verifies: AIRA-381 T12c (I7) — nothing is published before the store write
// commits. (a) A failing write refuses E_CONFINE_UNAVAILABLE and leaves both
// the published table and the store unchanged. (b) With the writer PAUSED
// inside the store write (after it committed), a concurrent confine-list sees
// the OLD table; after it resumes, the new one. Mutation: publish the table
// before the store write.
func TestSessionTiersT12cPublishOnlyAfterTheStoreWrite(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	var mode string
	var modeMu sync.Mutex
	paused, resume := make(chan struct{}), make(chan struct{})
	server.replaceSessionTiersFn = func(ctx context.Context, marks map[string]string) error {
		modeMu.Lock()
		current := mode
		modeMu.Unlock()
		switch current {
		case "fail":
			return errors.New("injected write failure")
		case "pause":
			if err := server.db.ReplaceConfineSessionTiers(ctx, marks); err != nil {
				return err
			}
			close(paused)
			<-resume
			return nil
		}
		return server.db.ReplaceConfineSessionTiers(ctx, marks)
	}
	serveTiers(t, server, t.TempDir())
	if response := socketSet(t, paths, "kept", "low"); !response.OK {
		t.Fatalf("set: %+v", response)
	}

	modeMu.Lock()
	mode = "fail"
	modeMu.Unlock()
	response := socketSet(t, paths, "field", "high")
	if response.Code != "E_CONFINE_UNAVAILABLE" || response.Exit != 4 || !strings.Contains(response.Error, "store session tiers") {
		t.Fatalf("failing write: %+v", response)
	}
	if got := *server.tiers.Load(); len(got) != 1 || got["kept"] != tierLow {
		t.Fatalf("published table after a failed write: %v", got)
	}
	if got := storedTiers(t, paths); len(got) != 1 || got["kept"] != "low" {
		t.Fatalf("store after a failed write: %v", got)
	}

	modeMu.Lock()
	mode = "pause"
	modeMu.Unlock()
	done := make(chan core.Response, 1)
	go func() { done <- socketSet(t, paths, "field", "high") }()
	select {
	case <-paused:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("the writer never reached the store write")
	}
	if _, fields := socketList(t, paths); string(fields["session_tiers"]) != `[{"owner":"kept","tier":"low"}]` {
		t.Fatalf("while the writer is paused the list shows %s, want the old table", fields["session_tiers"])
	}
	close(resume)
	if response := <-done; !response.OK {
		t.Fatalf("paused set: %+v", response)
	}
	if _, fields := socketList(t, paths); string(fields["session_tiers"]) != `[{"owner":"field","tier":"high"},{"owner":"kept","tier":"low"}]` {
		t.Fatalf("after the write the list shows %s", fields["session_tiers"])
	}
}

// verifies: AIRA-381 T15 (I7) — tiersMu serialises the setter end to end, by
// the paused-writer technique: (a) while one set is paused inside its store
// write, a second set that has PROVABLY reached the setter (the
// sessionTierSetEnteredFn handshake, waited for before the first is resumed)
// does not complete; resumed, both are in the final table and the store. (b) 63
// marks plus two concurrent NEW owners: exactly one is refused
// E_CONFINE_SESSION_TIERS_FULL. The handshake makes the mutation deterministic:
// without tiersMu the second set reads the table the paused first has not yet
// published, so one update is lost (or both new owners pass the cap check)
// however late the second request arrives. Mutation: drop tiersMu, with the
// second goroutine delayed two seconds -> red.
func TestSessionTiersT15SetsAreSerialised(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			paths := testPaths(t)
			server := NewServer(paths)
			var calls int
			var callsMu sync.Mutex
			paused, resume := make(chan struct{}), make(chan struct{})
			secondEntered := make(chan struct{})
			server.sessionTierSetEnteredFn = func(session string) {
				if session == "second" {
					close(secondEntered)
				}
			}
			server.replaceSessionTiersFn = func(ctx context.Context, marks map[string]string) error {
				if err := server.db.ReplaceConfineSessionTiers(ctx, marks); err != nil {
					return err
				}
				callsMu.Lock()
				calls++
				first := calls == 1 && len(marks) >= 1 && (marks["first"] != "")
				callsMu.Unlock()
				if first {
					close(paused)
					<-resume
				}
				return nil
			}
			if full {
				rows := map[string]string{}
				for i := 0; i < runner.ConfineSessionTierCap-1; i++ {
					rows[fmt.Sprintf("s%02d", i)] = "low"
				}
				seedTiers(t, paths, rows)
			}
			serveTiers(t, server, t.TempDir())
			firstDone, secondDone := make(chan core.Response, 1), make(chan core.Response, 1)
			go func() { firstDone <- socketSet(t, paths, "first", "high") }()
			select {
			case <-paused:
			case <-testdeadline.After(5 * time.Second):
				t.Fatal("the first set never reached the store write")
			}
			go func() { secondDone <- socketSet(t, paths, "second", "low") }()
			// The contention handshake: the second set is inside the setter (past
			// validation, at the lock) before the first is released.
			select {
			case <-secondEntered:
			case response := <-secondDone:
				t.Fatalf("the second set completed while the first was paused: %+v", response)
			case <-testdeadline.After(10 * time.Second):
				t.Fatal("the second set never reached the setter")
			}
			select {
			case response := <-secondDone:
				t.Fatalf("the second set completed while the first was paused: %+v", response)
			case <-time.After(300 * time.Millisecond):
			}
			close(resume)
			first, second := <-firstDone, <-secondDone
			if !first.OK {
				t.Fatalf("first set: %+v", first)
			}
			table, stored := *server.tiers.Load(), storedTiers(t, paths)
			if full {
				if second.Code != "E_CONFINE_SESSION_TIERS_FULL" {
					t.Fatalf("second new owner at 64: %+v, want E_CONFINE_SESSION_TIERS_FULL", second)
				}
				if len(table) != runner.ConfineSessionTierCap || len(stored) != runner.ConfineSessionTierCap {
					t.Fatalf("table %d marks, store %d, want 64", len(table), len(stored))
				}
				return
			}
			if !second.OK || table["first"] != tierHigh || table["second"] != tierLow || stored["first"] != "high" || stored["second"] != "low" {
				t.Fatalf("lost update: second %+v, table %v, store %v", second, table, stored)
			}
		})
	}
}

// verifies: AIRA-381 T17 — a loaded EMPTY table is sent as `[]`, never dropped
// or null, through the real socket. Mutation: omit an empty list (a nil
// slice behind the pointer, or no pointer).
func TestSessionTiersT17EmptyTableIsAnEmptyList(t *testing.T) {
	paths := testPaths(t)
	serveTiers(t, NewServer(paths), t.TempDir())
	_, fields := socketList(t, paths)
	if got := string(fields["session_tiers"]); got != "[]" {
		t.Fatalf("session_tiers %q, want []", got)
	}
}

// verifies: AIRA-381 T17c (daemon half, I4) — session_tiers comes from the
// Server's table, so a mark with no waiters and no live queue is listed, and
// it is on the UNEVALUATED return too (a slice the listing cannot read).
// Mutations: fill it from the queue snapshot; set it only on the OK path.
func TestSessionTiersT17cListedOnEveryReturn(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	stop := serveTiers(t, server, t.TempDir())
	if response := socketSet(t, paths, "field", "high"); !response.OK {
		t.Fatalf("set: %+v", response)
	}
	reply, fields := socketList(t, paths)
	if !reply.OK || string(fields["session_tiers"]) != `[{"owner":"field","tier":"high"}]` {
		t.Fatalf("OK path: %+v session_tiers %s", reply, fields["session_tiers"])
	}
	stop()
	unreadable := NewServer(paths)
	serveTiers(t, unreadable, t.TempDir()+"/missing")
	reply, fields = socketList(t, paths)
	if reply.Code != "UNEVALUATED" || string(fields["session_tiers"]) != `[{"owner":"field","tier":"high"}]` {
		t.Fatalf("UNEVALUATED path: code %s session_tiers %s", reply.Code, fields["session_tiers"])
	}
}
