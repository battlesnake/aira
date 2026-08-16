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
	"strings"
	"testing"
	"time"

	"aira/internal/core"
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
	if err := writeFrame(clientConn, frame); err != nil {
		t.Fatal(err)
	}
	var response ResponseFrame
	if err := readFrame(clientConn, &response); err != nil {
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
