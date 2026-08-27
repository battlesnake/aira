package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/store"
)

// shortRuntimeDir returns a short unique directory for XDG_RUNTIME_DIR. A daemon
// socket path must fit the 108-byte AF_UNIX sun_path limit; t.TempDir() embeds the
// (long) test name, which combined with the state-id namespace overflows it. Real
// runtime dirs (/run/user/<uid>) are short, so this models production faithfully.
func shortRuntimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "art")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func testScope(t *testing.T, paths Paths, name string) WorktreeScope {
	t.Helper()
	base := filepath.Dir(paths.StateHome)
	root := filepath.Join(base, "roots", name)
	common := filepath.Join(base, "common")
	gitDir := filepath.Join(common, "worktrees", name)
	for _, path := range []string{root, common, gitDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projectID, worktreeID, err := store.CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	return WorktreeScope{
		Root: root, CommonDir: common, GitDir: gitDir,
		ProjectID: projectID, WorktreeID: worktreeID, Slug: "aira",
		Prefixes: []string{"AIRA"}, ConfigDigest: "fixture", StateID: paths.StateID,
	}
}

func startServer(t *testing.T, server *Server) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	return cancel, done
}

func TestServerRoutedRoundTripAndProtocolEvidence(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "one")
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "id", Args: map[string]any{"prefix": "AIRA"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Proto != 0 || !response.OK || response.Code != "OK" {
		t.Fatalf("response = %+v", response)
	}

	mismatch, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion + 1, Scope: scope, Request: core.Request{Verb: "list"}})
	if err != nil {
		t.Fatal(err)
	}
	if mismatch.Proto != ProtocolVersion || mismatch.Code != CodeProtocol {
		t.Fatalf("protocol mismatch response = %+v", mismatch)
	}
}

func TestEnsureScopeRegistersFreshAndRefreshesCachedScopeExactlyOnce(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	scope := testScope(t, paths, "ensure")

	for invocation := 1; invocation <= 2; invocation++ {
		response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{Proto: ProtocolVersion, Scope: scope, Op: "ensure-scope"})
		if !response.OK || response.Code != "OK" {
			t.Fatalf("invocation %d response = %+v", invocation, response)
		}
		data, err := os.ReadFile(paths.RegistryPath)
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; lines != invocation {
			t.Fatalf("invocation %d registry lines = %d, want exactly %d", invocation, lines, invocation)
		}
	}

	conn, err := sql.Open("sqlite", paths.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for table, want := range map[string]int{"projects": 1, "worktrees": 1, "prefix_ownership": 1} {
		var got int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
}

func TestEnsureScopeSurfacesPrefixOwnershipConflict(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	first := independentScope(t, paths, "first", "SHARED")
	second := independentScope(t, paths, "second", "SHARED")
	if response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{Proto: ProtocolVersion, Scope: first, Op: "ensure-scope"}); !response.OK {
		t.Fatalf("first registration = %+v", response)
	}
	response := exchangeStoreOpOverPipe(t, server, StoreOpFrame{Proto: ProtocolVersion, Scope: second, Op: "ensure-scope"})
	if response.OK || response.Code != "E_PREFIX_OWNERSHIP_CONFLICT" || !strings.HasPrefix(response.Error, "E_PREFIX_OWNERSHIP_CONFLICT:") {
		t.Fatalf("conflict response = %+v", response)
	}
}

func exchangeStoreOpOverPipe(t *testing.T, server *Server, frame StoreOpFrame) ResponseFrame {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), serverConn)
		close(done)
	}()
	if err := writeStoreOp(clientConn, frame); err != nil {
		t.Fatal(err)
	}
	var response ResponseFrame
	if err := readResponse(clientConn, &response); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.Close()
	<-done
	return response
}

func independentScope(t *testing.T, paths Paths, name, prefix string) WorktreeScope {
	t.Helper()
	base := filepath.Join(filepath.Dir(paths.StateHome), "independent", name)
	root := filepath.Join(base, "root")
	common := filepath.Join(base, "common")
	gitDir := filepath.Join(common, "worktrees", name)
	for _, path := range []string{root, common, gitDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projectID, worktreeID, err := store.CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	return WorktreeScope{
		Root: root, CommonDir: common, GitDir: gitDir, ProjectID: projectID, WorktreeID: worktreeID,
		Slug: name, Prefixes: []string{prefix}, ConfigDigest: name, StateID: paths.StateID,
	}
}

func TestServerRoutedResponseIsByteIdenticalToInProcess(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "one")
	request := core.Request{Verb: "list"}
	wire, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	private, err := store.OpenDB(filepath.Join(filepath.Dir(paths.StateHome), "private", "state.db"), filepath.Join(filepath.Dir(paths.StateHome), "private", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = private.Close() })
	view, err := store.NewScope(private, store.ScopeOptions{
		Root: scope.Root, CommonDir: scope.CommonDir, GitDir: scope.GitDir,
		ProjectID: scope.ProjectID, WorktreeID: scope.WorktreeID, ProjectSlug: scope.Slug,
		Prefixes: scope.Prefixes, ReviewPolicy: scope.ReviewPolicy,
		LeaseStateDir: filepath.Join(filepath.Dir(paths.StateHome), "private", "leases"), ConfigDigest: scope.ConfigDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(core.New(view).Do(context.Background(), request))
	got, _ := json.Marshal(wire.CoreResponse())
	if !bytes.Equal(got, want) {
		t.Fatalf("reconstructed response differs\n got: %s\nwant: %s", got, want)
	}
}

func TestServerRoutedRantPreservesBodyAndGitContext(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "rant")
	body := "  routed body\ntrailing bytes  "
	observed := gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: scope.Root, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: scope.Root, Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: scope.WorktreeID, Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: "caller-head-verbatim", Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "caller-ref-verbatim", Status: gitcontext.StatusValue},
		RemoteURL:    gitcontext.Field{Value: "caller-remote-verbatim", Status: gitcontext.StatusValue},
		ObservedAt:   "2026-08-18T12:00:00Z", ResolverVersion: "route-test-v1",
	}
	captured, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "rant", Args: map[string]any{"subverb": "capture", "text": body, "tags": []string{"route"}}, GitContext: &observed, Actor: "terra"}})
	if err != nil || !captured.OK {
		t.Fatalf("capture response=%+v err=%v", captured, err)
	}
	gotFrame, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "rant", Args: map[string]any{"subverb": "get", "selector": "RANT-1"}}})
	if err != nil || !gotFrame.OK {
		t.Fatalf("get response=%+v err=%v", gotFrame, err)
	}
	var got domain.Rant
	if err := json.Unmarshal(gotFrame.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Body != body || got.GitContext != observed || got.Actor != "terra" {
		t.Fatalf("routed rant changed: %#v", got)
	}
}

func TestServerSiblingWorktreesKeepRootsAndIdentitiesIsolated(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	first := testScope(t, paths, "one")
	second := testScope(t, paths, "two")
	create := func(scope WorktreeScope, title string) ResponseFrame {
		t.Helper()
		response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "create", Args: map[string]any{
			"title": title, "kind": "feature", "severity": "P2", "body": "", "labels": []string{},
		}}})
		if err != nil || !response.OK {
			t.Fatalf("create %s response=%+v err=%v", title, response, err)
		}
		return response
	}
	create(first, "first")
	create(second, "second")
	firstPath := filepath.Join(first.Root, ".aira", "tickets", "AIRA-1.md")
	secondPath := filepath.Join(second.Root, ".aira", "tickets", "AIRA-2.md")
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing isolated ticket %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(first.Root, ".aira", "tickets", "AIRA-2.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second ticket leaked into first root: %v", err)
	}
	if first.WorktreeID == second.WorktreeID {
		t.Fatal("sibling worktree identities alias")
	}
}

func TestServerRejectsForeignStateAndForgedIdentity(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "one")
	scope.StateID = "foreign"
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "list"}})
	if err != nil || response.Code != CodeProjectInvalid {
		t.Fatalf("foreign state response=%+v err=%v", response, err)
	}
	scope.StateID = paths.StateID
	scope.WorktreeID = "forged"
	response, err = Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "list"}})
	if err != nil || response.Code != CodeProjectInvalid {
		t.Fatalf("forged identity response=%+v err=%v", response, err)
	}
}

func TestServerSingleInstanceFlockBeforeBind(t *testing.T) {
	paths := testPaths(t)
	first := NewServer(paths)
	_, _ = startServer(t, first)
	second := NewServer(paths)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := second.Serve(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Serve error = %v, want ErrAlreadyRunning", err)
	}
}

func TestSymlinkAliasedMissingStateHasSingleInstance(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("XDG_STATE_HOME", filepath.Join(aliasParent, "missing", "state"))
	aliasPaths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(realParent, "missing", "state"))
	realPaths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if aliasPaths.StateID != realPaths.StateID || aliasPaths.SocketPath != realPaths.SocketPath {
		t.Fatalf("alias paths=%+v, real paths=%+v", aliasPaths, realPaths)
	}
	_, _ = startServer(t, NewServer(aliasPaths))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := NewServer(realPaths).Serve(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("aliased second Serve error = %v, want ErrAlreadyRunning", err)
	}
}

func TestServerRoundTripPreservesPresentEmptyImports(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	scope := testScope(t, paths, "empty-import")
	scope.RequirementPrefixes = []string{"AR"}
	requests := []core.Request{
		{Verb: "import", Args: map[string]any{"file": "missing-findings.jsonl", "strict": true}, Content: []byte{}, HasContent: true},
		{Verb: "req", Args: map[string]any{"subverb": "import", "file": "missing-requirements.jsonl"}, Content: []byte{}, HasContent: true},
	}
	for _, request := range requests {
		response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: request})
		if err != nil {
			t.Fatalf("%s exchange: %v", request.Verb, err)
		}
		if !response.OK {
			t.Fatalf("%s empty import reopened daemon-relative path: %+v", request.Verb, response)
		}
	}
}

func TestServerRecoversPerConnectionPanic(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	var calls int
	server.Handle = func(context.Context, WorktreeScope, core.Request) core.Response {
		calls++
		if calls == 1 {
			panic("fixture")
		}
		return core.Response{OK: true, Code: "OK"}
	}
	_, _ = startServer(t, server)
	request := RequestFrame{Proto: ProtocolVersion, Scope: testScope(t, paths, "one"), Request: core.Request{Verb: "list"}}
	response, err := Exchange(context.Background(), paths.SocketPath, request)
	if err != nil || response.Code != CodeInternal {
		t.Fatalf("panic response=%+v err=%v", response, err)
	}
	response, err = Exchange(context.Background(), paths.SocketPath, request)
	if err != nil || !response.OK {
		t.Fatalf("post-panic response=%+v err=%v", response, err)
	}
}

func TestServerGracefulDrainCompletesInflightRequest(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	started := make(chan struct{})
	release := make(chan struct{})
	dbClosed := make(chan struct{})
	server.closeDB = func(db *store.DB) error {
		close(dbClosed)
		return db.Close()
	}
	server.Handle = func(context.Context, WorktreeScope, core.Request) core.Response {
		close(started)
		<-release
		return core.Response{OK: true, Code: "OK", Data: map[string]any{"drained": true}}
	}
	cancel, done := startServer(t, server)
	responses := make(chan ResponseFrame, 1)
	errorsCh := make(chan error, 1)
	go func() {
		response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: testScope(t, paths, "one"), Request: core.Request{Verb: "list"}})
		responses <- response
		errorsCh <- err
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("daemon stopped before in-flight request completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-errorsCh; err != nil {
		t.Fatal(err)
	}
	if response := <-responses; !response.OK {
		t.Fatalf("drained response = %+v", response)
	}
	select {
	case <-dbClosed:
	case <-time.After(time.Second):
		t.Fatal("DB was not closed after all users drained")
	}
}

type daemonTestClock struct {
	boot string
	mono uint64
}

func (c daemonTestClock) Now() (string, uint64, error) { return c.boot, c.mono, nil }

func prepareExpiredDaemonLease(t *testing.T, paths Paths, scope WorktreeScope) string {
	t.Helper()
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.NewScope(db, store.ScopeOptions{
		Root: scope.Root, CommonDir: scope.CommonDir, GitDir: scope.GitDir,
		ProjectID: scope.ProjectID, WorktreeID: scope.WorktreeID, ProjectSlug: scope.Slug,
		Prefixes: scope.Prefixes, LeaseStateDir: paths.LeaseStateDir, LeaseTTLNS: 1,
		ConfigDigest: scope.ConfigDigest, Clock: daemonTestClock{boot: currentBootID(), mono: 0},
	})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	ticket, err := view.CreateTicket(context.Background(), domain.CreateTicketInput{
		Title: "restart gap", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := view.Claim(context.Background(), ticket.ID, false, "old-holder"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return ticket.ID
}

func TestReapOnScopeBuildClosesRestartGap(t *testing.T) {
	paths := testPaths(t)
	scope := testScope(t, paths, "restart-gap")
	scope.LeaseTTLNS = 1
	ticketID := prepareExpiredDaemonLease(t, paths, scope)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), serverConn)
		close(done)
	}()
	if err := writeFrame(clientConn, RequestFrame{
		Proto: ProtocolVersion, Scope: scope,
		Request: core.Request{Verb: "claim", Args: map[string]any{"selector": ticketID, "actor": "new-holder"}},
	}); err != nil {
		t.Fatal(err)
	}
	var response ResponseFrame
	if err := readFrame(clientConn, &response); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.Close()
	<-done
	if !response.OK {
		t.Fatalf("plain post-restart claim response=%+v", response)
	}
	conn, err := sql.Open("sqlite", paths.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var lapses int
	if err := conn.QueryRow(`SELECT count(*) FROM events WHERE project_id=? AND verb='lease.lapse'`, scope.ProjectID).Scan(&lapses); err != nil {
		t.Fatal(err)
	}
	if lapses != 1 {
		t.Fatalf("lease.lapse events=%d, want 1", lapses)
	}
	server.mu.Lock()
	var view *store.Store
	for _, entry := range server.scopes {
		view = entry.view
		break
	}
	server.mu.Unlock()
	if view == nil {
		t.Fatal("fresh daemon did not cache the built scope")
	}
	if err := view.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(scope.CommonDir, "aira", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(journal, []byte(`"verb":"lease.lapse"`)) {
		t.Fatalf("reconcile did not materialise lease.lapse: %s", journal)
	}
}

func TestScopeBuildBarrierSingleflightAndOtherProjectConcurrency(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	blockedScope := independentScope(t, paths, "blocked", "BLOCK")
	otherScope := independentScope(t, paths, "other", "OTHER")
	if _, _, err := server.storeForScope(otherScope); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var blockedCalls atomic.Int32
	server.reapScope = func(_ context.Context, view *store.Store) (int, error) {
		if view.ProjectID() == blockedScope.ProjectID {
			if blockedCalls.Add(1) == 1 {
				close(started)
			}
			<-release
		}
		return 0, nil
	}
	creatorDone := make(chan error, 1)
	go func() {
		_, _, err := server.storeForScope(blockedScope)
		creatorDone <- err
	}()
	<-started

	waiterDone := make(chan error, 1)
	go func() {
		_, _, err := server.storeForScope(blockedScope)
		waiterDone <- err
	}()
	select {
	case err := <-waiterDone:
		t.Fatalf("same-scope request overtook initial reap: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	otherDone := make(chan error, 1)
	go func() {
		_, _, err := server.storeForScope(otherScope)
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow on-build reap held s.mu and blocked another project's cached scope")
	}
	close(release)
	if err := <-creatorDone; err != nil {
		t.Fatal(err)
	}
	if err := <-waiterDone; err != nil {
		t.Fatal(err)
	}
	if got := blockedCalls.Load(); got != 1 {
		t.Fatalf("initial reap calls=%d, want singleflight 1", got)
	}
}

func TestScopeBuildReapFailureIsBestEffortAndClosesBarrier(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		return 0, errors.New("fixture reap failure")
	}
	scope := independentScope(t, paths, "best-effort", "BEST")
	if _, cached, err := server.storeForScope(scope); err != nil || cached {
		t.Fatalf("creator cached=%v err=%v", cached, err)
	}
	if _, cached, err := server.storeForScope(scope); err != nil || !cached {
		t.Fatalf("waiter cached=%v err=%v", cached, err)
	}
}

func TestScopeBuildPanicStillClosesBarrier(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	started := make(chan struct{})
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		close(started)
		panic("fixture reap panic")
	}
	scope := independentScope(t, paths, "panic-barrier", "PANIC")
	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		_, _, _ = server.storeForScope(scope)
	}()
	<-started
	if panicValue := <-recovered; panicValue == nil {
		t.Fatal("initial reap did not panic")
	}
	server.reapScope = nil
	waiter := make(chan error, 1)
	go func() {
		_, _, err := server.storeForScope(scope)
		waiter <- err
	}()
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("same-scope waiter remained blocked after initial reap panic")
	}
}

func TestPeriodicReaperDedupesProjectsAndSkipsUnreadyScopes(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	first := testScope(t, paths, "periodic-one")
	second := testScope(t, paths, "periodic-two")
	firstView, _, err := server.storeForScope(first)
	if err != nil {
		t.Fatal(err)
	}
	secondView, _, err := server.storeForScope(second)
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.scopes["unready"] = &scopeEntry{view: firstView, ready: make(chan struct{})}
	server.mu.Unlock()
	var calls atomic.Int32
	called := make(chan struct{}, 1)
	server.reapScope = func(ctx context.Context, _ *store.Store) (int, error) {
		calls.Add(1)
		select {
		case called <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runReaper(ctx, time.Millisecond)
		close(done)
	}()
	<-called
	cancel()
	<-done
	if got := calls.Load(); got != 1 {
		t.Fatalf("periodic calls=%d, want one deduped project sweep", got)
	}
	if firstView.ProjectID() != secondView.ProjectID() {
		t.Fatal("fixture worktrees did not share a project")
	}
}

func TestPeriodicReaperContinuesAfterSweepError(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	if _, _, err := server.storeForScope(independentScope(t, paths, "periodic-error", "PERR")); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	second := make(chan struct{})
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		// The 1ms reaper can fire a third sweep before cancel() lands, so only the
		// second call closes; later calls are harmless no-ops (a bare close(second)
		// double-closes and panics under load).
		switch calls.Add(1) {
		case 1:
			return 0, errors.New("fixture periodic failure")
		case 2:
			close(second)
		}
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runReaper(ctx, time.Millisecond)
		close(done)
	}()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("ticker stopped after a sweep error")
	}
	cancel()
	<-done
}

func TestIdleReapThenTimerFlushesDeferredLapseExactlyOnce(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	scope := independentScope(t, paths, "d2-idle-reap", "IDLE")
	scope.LeaseTTLNS = 1
	view, _, err := server.storeForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := view.CreateTicket(context.Background(), domain.CreateTicketInput{
		Title: "expire while idle", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Claim(context.Background(), ticket.ID, false, "alice"); err != nil {
		t.Fatal(err)
	}
	var reaped int
	for attempt := 0; attempt < 20 && reaped == 0; attempt++ {
		time.Sleep(time.Millisecond)
		reaped, err = view.ReapExpiredLeases(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	if reaped != 1 {
		t.Fatalf("reaped=%d, want one expired lease", reaped)
	}
	journalPath := filepath.Join(scope.CommonDir, "aira", "journal.jsonl")
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte(`"verb":"lease.lapse"`)) {
		t.Fatal("reaper journaled lease.lapse inline")
	}
	flushed := make(chan struct{}, 1)
	server.flushScopeFn = func(ctx context.Context, view *store.Store) (int, error) {
		count, err := view.FlushDeferredJournal(ctx)
		select {
		case flushed <- struct{}{}:
		default:
		}
		return count, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runJournalFlusher(ctx, time.Millisecond)
		close(done)
	}()
	select {
	case <-flushed:
	case <-time.After(3 * time.Second):
		t.Fatal("journal flusher did not tick")
	}
	cancel()
	<-done
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(after), `"verb":"lease.lapse"`); got != 1 {
		t.Fatalf("lease.lapse journal occurrences=%d, want exactly one", got)
	}
	if count, err := view.FlushDeferredJournal(context.Background()); err != nil || count != 0 {
		t.Fatalf("post-timer idempotent flush count=%d err=%v", count, err)
	}
}

func TestPeriodicJournalFlusherDedupesProjectsAndSkipsUnreadyScopes(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db

	// Project P: two ready worktrees of the SAME project → must be flushed once.
	firstView, _, err := server.storeForScope(testScope(t, paths, "flush-one"))
	if err != nil {
		t.Fatal(err)
	}
	secondView, _, err := server.storeForScope(testScope(t, paths, "flush-two"))
	if err != nil {
		t.Fatal(err)
	}
	if firstView.ProjectID() != secondView.ProjectID() {
		t.Fatal("fixture worktrees did not share a project")
	}
	// Project Q: an INDEPENDENT ready project → must be flushed once.
	qView, _, err := server.storeForScope(independentScope(t, paths, "flush-indep-ready", "FQREADY"))
	if err != nil {
		t.Fatal(err)
	}
	// Project R: an INDEPENDENT project present ONLY via an UNREADY entry → must
	// be skipped. Build its view, then replace its ready cache entries with a
	// single never-ready entry (an independent project makes a wrongful include
	// observable — dedup cannot mask it, unlike a same-project unready entry).
	rView, _, err := server.storeForScope(independentScope(t, paths, "flush-indep-unready", "FRUNRDY"))
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	for key, entry := range server.scopes {
		if entry.view.ProjectID() == rView.ProjectID() {
			delete(server.scopes, key)
		}
	}
	server.scopes["unready-R"] = &scopeEntry{view: rView, ready: make(chan struct{})}
	server.mu.Unlock()

	perProject := map[string]int{}
	server.flushScopeFn = func(_ context.Context, v *store.Store) (int, error) {
		perProject[v.ProjectID()]++ // called sequentially by flushReadyProjects; no lock needed
		return 0, nil
	}
	server.flushReadyProjects(context.Background())

	if perProject[firstView.ProjectID()] != 1 {
		t.Fatalf("shared project flushed %d times, want exactly one (dedup)", perProject[firstView.ProjectID()])
	}
	if perProject[qView.ProjectID()] != 1 {
		t.Fatalf("independent ready project flushed %d times, want one", perProject[qView.ProjectID()])
	}
	if perProject[rView.ProjectID()] != 0 {
		t.Fatalf("unready independent project was flushed %d times, want zero (skip not-ready)", perProject[rView.ProjectID()])
	}
}

func TestPeriodicJournalFlusherIsolatesProjectErrors(t *testing.T) {
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	if _, _, err := server.storeForScope(independentScope(t, paths, "flush-error-one", "FERRONE")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.storeForScope(independentScope(t, paths, "flush-error-two", "FERRTWO")); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	both := make(chan struct{}, 1)
	server.flushScopeFn = func(context.Context, *store.Store) (int, error) {
		if calls.Add(1) == 2 {
			both <- struct{}{}
		}
		return 0, errors.New("fixture flush failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runJournalFlusher(ctx, time.Millisecond)
		close(done)
	}()
	select {
	case <-both:
	case <-time.After(time.Second):
		t.Fatal("one project flush error blocked another project")
	}
	cancel()
	<-done
}

func TestDisabledJournalFlusherParksUntilCancellation(t *testing.T) {
	server := NewServer(Paths{})
	var calls atomic.Int32
	server.flushScopeFn = func(context.Context, *store.Store) (int, error) {
		calls.Add(1)
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runJournalFlusher(ctx, 0)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("disabled journal flusher performed work")
	}
	select {
	case <-done:
		t.Fatal("disabled journal flusher did not park")
	default:
	}
	cancel()
	<-done
}

func TestDisabledJournalFlusherDaemonServesWithoutFlushOnBuild(t *testing.T) {
	t.Setenv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL", "disabled")
	paths := testPaths(t)
	server := NewServer(paths)
	var calls atomic.Int32
	server.flushScopeFn = func(context.Context, *store.Store) (int, error) {
		calls.Add(1)
		return 0, nil
	}
	_, _ = startServer(t, server)
	scope := independentScope(t, paths, "disabled-flusher", "DFLUSH")
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "list"},
	})
	if err != nil || !response.OK {
		t.Fatalf("disabled-flusher daemon response=%+v err=%v", response, err)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("disabled flusher calls=%d, want zero including scope build", calls.Load())
	}
}

func TestShutdownWaitsForReaperAsWellAsConnections(t *testing.T) {
	t.Setenv("AIRA_DAEMON_REAP_INTERVAL", "1ms")
	paths := testPaths(t)
	server := NewServer(paths)
	server.DrainTimeout = 20 * time.Millisecond
	var calls atomic.Int32
	reaperStarted := make(chan struct{})
	releaseReaper := make(chan struct{})
	reaperFinished := make(chan struct{})
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		if calls.Add(1) == 1 { // on-build sweep
			return 0, nil
		}
		close(reaperStarted)
		<-releaseReaper
		close(reaperFinished)
		return 0, nil
	}
	var closes atomic.Int32
	server.closeDB = func(db *store.DB) error {
		closes.Add(1)
		return db.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	}
	scope := independentScope(t, paths, "reaper-drain", "RDRAIN")
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "list"},
	})
	if err != nil || !response.OK {
		t.Fatalf("scope build response=%+v err=%v", response, err)
	}
	<-reaperStarted
	cancel()
	serveErr := <-done
	var drainTimeout *ErrDrainTimeout
	if !errors.As(serveErr, &drainTimeout) {
		t.Fatalf("Serve error=%v, want ErrDrainTimeout", serveErr)
	}
	if closes.Load() != 0 {
		t.Fatal("DB closed while periodic reaper was still active")
	}
	close(releaseReaper)
	<-reaperFinished
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := drainTimeout.lock.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(paths.SocketPath)
}

func TestShutdownWaitsForJournalFlusher(t *testing.T) {
	t.Setenv("AIRA_DAEMON_REAP_INTERVAL", "disabled")
	t.Setenv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL", "1s")
	paths := testPaths(t)
	server := NewServer(paths)
	server.DrainTimeout = 2 * time.Second
	started := make(chan struct{})
	release := make(chan struct{})
	server.flushScopeFn = func(context.Context, *store.Store) (int, error) {
		close(started)
		<-release
		return 0, nil
	}
	var closes atomic.Int32
	server.closeDB = func(db *store.DB) error {
		closes.Add(1)
		return db.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	}
	scope := independentScope(t, paths, "flusher-drain", "FDRAIN")
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "list"},
	})
	if err != nil || !response.OK {
		t.Fatalf("scope build response=%+v err=%v", response, err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("periodic journal flush did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Serve returned before flusher drained: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if closes.Load() != 0 {
		t.Fatal("DB closed while journal flusher was active")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if closes.Load() != 1 {
		t.Fatalf("DB close calls=%d, want one after flusher drain", closes.Load())
	}
}

func TestDrainTimeoutWithStuckJournalFlusherRetainsLockAndDB(t *testing.T) {
	t.Setenv("AIRA_DAEMON_REAP_INTERVAL", "disabled")
	t.Setenv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL", "1s")
	paths := testPaths(t)
	server := NewServer(paths)
	server.DrainTimeout = 20 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	server.flushScopeFn = func(context.Context, *store.Store) (int, error) {
		close(started)
		<-release
		return 0, nil
	}
	var closes atomic.Int32
	server.closeDB = func(db *store.DB) error {
		closes.Add(1)
		return db.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	}
	scope := independentScope(t, paths, "flusher-timeout", "FTIMEOUT")
	response, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto: ProtocolVersion, Scope: scope, Request: core.Request{Verb: "list"},
	})
	if err != nil || !response.OK {
		t.Fatalf("scope build response=%+v err=%v", response, err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("periodic journal flush did not start")
	}
	cancel()
	serveErr := <-done
	var drainTimeout *ErrDrainTimeout
	if !errors.As(serveErr, &drainTimeout) || drainTimeout.lock == nil {
		t.Fatalf("Serve error=%T %v, want lock-owning ErrDrainTimeout", serveErr, serveErr)
	}
	if closes.Load() != 0 {
		t.Fatal("DB closed while timed-out journal flusher was active")
	}
	replacementCtx, replacementCancel := context.WithTimeout(context.Background(), time.Second)
	defer replacementCancel()
	if replacementErr := NewServer(paths).Serve(replacementCtx); !errors.Is(replacementErr, ErrAlreadyRunning) {
		t.Fatalf("replacement Serve=%v, want ErrAlreadyRunning", replacementErr)
	}
	close(release)
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := drainTimeout.lock.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(paths.SocketPath)
}

func TestDrainTimeoutRetainsLockSkipsDBCloseAndSurvivesGC(t *testing.T) {
	t.Setenv("AIRA_DAEMON_REAP_INTERVAL", "disabled")
	paths := testPaths(t)
	server := NewServer(paths)
	server.DrainTimeout = 20 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	server.Handle = func(context.Context, WorktreeScope, core.Request) core.Response {
		close(started)
		<-release
		return core.Response{OK: true, Code: "OK"}
	}
	var closes atomic.Int32
	server.closeDB = func(db *store.DB) error {
		closes.Add(1)
		return db.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	exchangeDone := make(chan error, 1)
	go func() {
		_, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
			Proto: ProtocolVersion, Scope: testScope(t, paths, "wedged"), Request: core.Request{Verb: "list"},
		})
		exchangeDone <- err
	}()
	<-started
	cancel()
	serveErr := <-done
	var drainTimeout *ErrDrainTimeout
	if !errors.As(serveErr, &drainTimeout) || drainTimeout.lock == nil {
		t.Fatalf("Serve error=%T %v, want lock-owning ErrDrainTimeout", serveErr, serveErr)
	}
	if closes.Load() != 0 {
		t.Fatal("DB was closed on drain timeout")
	}
	runtime.GC()
	replacementCtx, replacementCancel := context.WithTimeout(context.Background(), time.Second)
	defer replacementCancel()
	if replacementErr := NewServer(paths).Serve(replacementCtx); !errors.Is(replacementErr, ErrAlreadyRunning) {
		t.Fatalf("replacement Serve after GC=%v, want ErrAlreadyRunning", replacementErr)
	}
	close(release)
	if exchangeErr := <-exchangeDone; exchangeErr != nil {
		t.Fatal(exchangeErr)
	}
	if closes.Load() != 0 {
		t.Fatal("timed-out Serve later closed DB")
	}
	// A real daemon exits immediately. The test has now drained the worker and
	// may explicitly release retained resources belonging to its unique fixture.
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := drainTimeout.lock.Close(); err != nil {
		t.Fatal(err)
	}
	// The socket pathname intentionally persisted. A replacement acquires the
	// released lock and removes that stale pathname before binding.
	recovered := NewServer(paths)
	_, _ = startServer(t, recovered)
}

func TestPathsNamespaceStateIdentity(t *testing.T) {
	first := testPaths(t)
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "other-state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "run"))
	second, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if first.StateID == second.StateID || first.SocketPath == second.SocketPath {
		t.Fatalf("state identities alias: first=%+v second=%+v", first, second)
	}
}
