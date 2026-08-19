package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/domain"
	"aira/internal/store"
)

func newDiscoveryTestServer(t *testing.T) (*Server, Paths) {
	t.Helper()
	paths := testPaths(t)
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	return server, paths
}

func projectForDiscoveryScope(scope WorktreeScope) app.Project {
	return app.Project{
		Root: scope.Root, CommonDir: scope.CommonDir, GitDir: scope.GitDir,
		ProjectID: scope.ProjectID, WorktreeID: scope.WorktreeID,
		Config: app.Config{
			Schema: 1,
			Project: app.ProjectConfig{
				Slug: scope.Slug, Prefixes: append([]string(nil), scope.Prefixes...),
				RequirementPrefixes: append([]string(nil), scope.RequirementPrefixes...),
			},
			Lease: app.LeaseConfig{TTLSeconds: 900, HeartbeatSeconds: 30},
		},
	}
}

func registryEntryForProject(project app.Project) store.RegistryEntry {
	return store.RegistryEntry{
		ProjectID: project.ProjectID, WorktreeID: project.WorktreeID,
		CommonDir: project.CommonDir, Root: project.Root,
		Prefixes: project.Config.Project.Prefixes, RequirementPrefixes: project.Config.Project.RequirementPrefixes,
	}
}

func createConfiguredGitProject(t *testing.T, base, name, prefix string) app.Project {
	t.Helper()
	root := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"` + name + `","prefixes":["` + prefix + `"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := app.Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func waitForCondition(t *testing.T, timeout time.Duration, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func TestStoreForScopeRecordsCoveredWorktree(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	scope := independentScope(t, paths, "covered", "COVERED")
	if _, _, err := server.storeForScope(scope); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	_, covered := server.coveredWorktrees[scope.WorktreeID]
	server.mu.Unlock()
	if !covered {
		t.Fatalf("worktree %s was cached without coverage membership", scope.WorktreeID)
	}
}

func TestBootstrapRecordsCoveredWorktree(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	root := filepath.Join(filepath.Dir(paths.StateHome), "bootstrap-covered")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	project, err := app.DiscoverBootstrap(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	scope := WorktreeScope{
		Root: project.Root, CommonDir: project.CommonDir, GitDir: project.GitDir,
		ProjectID: project.ProjectID, WorktreeID: project.WorktreeID,
		StateID: paths.StateID, Bootstrap: true,
	}
	response := server.bootstrap(context.Background(), scope, map[string]any{"project": "bootstrap", "prefixes": "BOOT"})
	if !response.OK {
		t.Fatalf("bootstrap response=%+v", response)
	}
	server.mu.Lock()
	_, covered := server.coveredWorktrees[project.WorktreeID]
	server.mu.Unlock()
	if !covered {
		t.Fatalf("bootstrap worktree %s was inserted without coverage membership", project.WorktreeID)
	}
}

func TestDiscoverRegistryPassSkipsBadEntriesAndGuardsIdentity(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	valid := projectForDiscoveryScope(independentScope(t, paths, "valid", "VALID"))
	wrong := projectForDiscoveryScope(independentScope(t, paths, "wrong", "WRONG"))
	invalid := projectForDiscoveryScope(independentScope(t, paths, "invalid", "INVALID"))
	invalid.Config.Project.Review = []byte(`null`)
	entries := []store.RegistryEntry{
		{ProjectID: "missing-project", WorktreeID: "missing-worktree", Root: "/missing"},
		{ProjectID: "stale-project", WorktreeID: "stale-worktree", Root: "/reused"},
		registryEntryForProject(invalid),
		registryEntryForProject(valid),
	}
	server.listRegistryEntries = func(string) ([]store.RegistryEntry, error) { return entries, nil }
	server.discoverProject = func(_ context.Context, root string) (app.Project, error) {
		switch root {
		case "/missing":
			return app.Project{}, errors.New("E_NOT_PROJECT: gone")
		case "/reused":
			return wrong, nil
		case invalid.Root:
			return invalid, nil
		case valid.Root:
			return valid, nil
		default:
			return app.Project{}, errors.New("unexpected root")
		}
	}
	var reaped []string
	server.reapScope = func(_ context.Context, view *store.Store) (int, error) {
		reaped = append(reaped, view.ProjectID())
		return 0, nil
	}
	discovered, registered, skipped := server.discoverRegistryPass(context.Background())
	if discovered != 1 || registered != 4 || skipped != 3 {
		t.Fatalf("summary discovered=%d registered=%d skipped=%d", discovered, registered, skipped)
	}
	server.mu.Lock()
	_, validCovered := server.coveredWorktrees[valid.WorktreeID]
	_, wrongCovered := server.coveredWorktrees[wrong.WorktreeID]
	scopeCount := len(server.scopes)
	server.mu.Unlock()
	if !validCovered || wrongCovered || scopeCount != 1 {
		t.Fatalf("validCovered=%v wrongCovered=%v scopes=%d", validCovered, wrongCovered, scopeCount)
	}
	if len(reaped) != 1 || reaped[0] != valid.ProjectID {
		t.Fatalf("reaped projects=%v; reused-root project leases must remain untouched", reaped)
	}
}

func TestDiscoverRegistryPassReadErrorIsBestEffort(t *testing.T) {
	server, _ := newDiscoveryTestServer(t)
	server.listRegistryEntries = func(string) ([]store.RegistryEntry, error) {
		return nil, errors.New("fixture read failure")
	}
	if discovered, registered, skipped := server.discoverRegistryPass(context.Background()); discovered != 0 || registered != 0 || skipped != 0 {
		t.Fatalf("summary=%d/%d/%d, want zero pass", discovered, registered, skipped)
	}
}

func TestDiscoverRegistryPassUsesRealDiscoveryAndContinuesPastUnavailableEntries(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	base := filepath.Join(filepath.Dir(paths.StateHome), "real-discovery")
	valid := createConfiguredGitProject(t, base, "valid-real", "REAL")
	reusedOriginal := createConfiguredGitProject(t, base, "original-real", "ORIGINAL")
	reusedReplacement := createConfiguredGitProject(t, base, "replacement-real", "REPLACE")

	invalidRoot := filepath.Join(base, "invalid-real")
	if err := os.MkdirAll(filepath.Join(invalidRoot, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", invalidRoot, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	invalidIdentity, err := app.DiscoverBootstrap(context.Background(), invalidRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidRoot, ".aira", "config"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingIdentity := projectForDiscoveryScope(independentScope(t, paths, "gone-real", "GONE"))
	if err := os.RemoveAll(missingIdentity.Root); err != nil {
		t.Fatal(err)
	}
	reusedEntry := registryEntryForProject(reusedOriginal)
	reusedEntry.Root = reusedReplacement.Root
	writeDiscoveryRegistry(t, paths.RegistryPath,
		registryEntryForProject(missingIdentity),
		reusedEntry,
		store.RegistryEntry{ProjectID: invalidIdentity.ProjectID, WorktreeID: invalidIdentity.WorktreeID, CommonDir: invalidIdentity.CommonDir, Root: invalidIdentity.Root, Prefixes: []string{"INVALID"}},
		registryEntryForProject(valid),
	)
	var reaped []string
	server.reapScope = func(_ context.Context, view *store.Store) (int, error) {
		reaped = append(reaped, view.ProjectID())
		return 0, nil
	}
	discovered, registered, skipped := server.discoverRegistryPass(context.Background())
	if discovered != 1 || registered != 4 || skipped != 3 {
		t.Fatalf("summary discovered=%d registered=%d skipped=%d", discovered, registered, skipped)
	}
	if len(reaped) != 1 || reaped[0] != valid.ProjectID {
		t.Fatalf("reaped projects=%v, want only valid %s", reaped, valid.ProjectID)
	}
	server.mu.Lock()
	_, replacementCovered := server.coveredWorktrees[reusedReplacement.WorktreeID]
	_, validCovered := server.coveredWorktrees[valid.WorktreeID]
	server.mu.Unlock()
	if replacementCovered || !validCovered {
		t.Fatalf("replacementCovered=%v validCovered=%v", replacementCovered, validCovered)
	}
}

func TestRunRegistryDiscoveryRetriesTransientEntry(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	project := projectForDiscoveryScope(independentScope(t, paths, "transient", "TRANSIENT"))
	server.listRegistryEntries = func(string) ([]store.RegistryEntry, error) {
		return []store.RegistryEntry{registryEntryForProject(project)}, nil
	}
	var calls atomic.Int32
	server.discoverProject = func(context.Context, string) (app.Project, error) {
		if calls.Add(1) == 1 {
			return app.Project{}, errors.New("E_NOT_PROJECT: temporarily unavailable")
		}
		return project, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.runRegistryDiscovery(ctx, 5*time.Millisecond)
		close(done)
	}()
	waitForCondition(t, time.Second, "periodic discovery did not cover transient entry", func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		_, ok := server.coveredWorktrees[project.WorktreeID]
		return ok
	})
	cancel()
	<-done
	if calls.Load() != 2 {
		t.Fatalf("Discover calls=%d, want failed first pass plus successful second pass", calls.Load())
	}
}

func TestDiscoverRegistryPassQuarantinesPersistentStoreFailure(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	ownerScope := independentScope(t, paths, "prefix-owner", "SHARED")
	if _, _, err := server.storeForScope(ownerScope); err != nil {
		t.Fatal(err)
	}

	conflict := projectForDiscoveryScope(independentScope(t, paths, "prefix-conflict", "SHARED"))
	writeDiscoveryRegistry(t, paths.RegistryPath, registryEntryForProject(conflict))
	var discoverCalls atomic.Int32
	server.discoverProject = func(context.Context, string) (app.Project, error) {
		discoverCalls.Add(1)
		return conflict, nil
	}

	for pass := 1; pass <= 3; pass++ {
		server.discoverRegistryPass(context.Background())
		data, err := os.ReadFile(paths.RegistryPath)
		if err != nil {
			t.Fatal(err)
		}
		if lines := bytes.Count(data, []byte{'\n'}); lines != 2 {
			t.Fatalf("pass %d registry lines=%d, want one source breadcrumb plus one failed registration", pass, lines)
		}
	}
	if calls := discoverCalls.Load(); calls != 1 {
		t.Fatalf("persistent store failure discovered %d times, want one attempt per daemon lifetime", calls)
	}
}

func TestRunRegistryDiscoveryDisabledDoesNotEnumerate(t *testing.T) {
	for _, value := range []string{"disabled", "0"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AIRA_DAEMON_DISCOVERY_INTERVAL", value)
			interval, err := registryDiscoveryIntervalFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			server := NewServer(Paths{})
			var calls atomic.Int32
			server.listRegistryEntries = func(string) ([]store.RegistryEntry, error) {
				calls.Add(1)
				return nil, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				server.runRegistryDiscovery(ctx, interval)
				close(done)
			}()
			time.Sleep(10 * time.Millisecond)
			cancel()
			<-done
			if calls.Load() != 0 {
				t.Fatalf("disabled discovery enumerated registry %d times", calls.Load())
			}
		})
	}
}

func TestRegistryDiscoveryConcurrentRequestSingleflightsAndThenSkipsCovered(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	project := projectForDiscoveryScope(independentScope(t, paths, "concurrent", "CONCUR"))
	scope, err := ScopeFromProject(project, paths)
	if err != nil {
		t.Fatal(err)
	}
	server.listRegistryEntries = func(string) ([]store.RegistryEntry, error) {
		return []store.RegistryEntry{registryEntryForProject(project)}, nil
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var discoverCalls atomic.Int32
	server.discoverProject = func(context.Context, string) (app.Project, error) {
		if discoverCalls.Add(1) == 1 {
			close(started)
			<-release
		}
		return project, nil
	}
	var reapCalls atomic.Int32
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		reapCalls.Add(1)
		return 0, nil
	}
	passDone := make(chan struct{})
	go func() {
		server.discoverRegistryPass(context.Background())
		close(passDone)
	}()
	<-started
	if _, cached, err := server.storeForScope(scope); err != nil || cached {
		t.Fatalf("concurrent request cached=%v err=%v", cached, err)
	}
	close(release)
	<-passDone
	server.discoverRegistryPass(context.Background())
	server.mu.Lock()
	scopeCount := len(server.scopes)
	server.mu.Unlock()
	if scopeCount != 1 || reapCalls.Load() != 1 || discoverCalls.Load() != 1 {
		t.Fatalf("scopes=%d reapCalls=%d discoverCalls=%d", scopeCount, reapCalls.Load(), discoverCalls.Load())
	}
}

func TestDiscoveredScopeFeedsReaperAndFlusher(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	project := projectForDiscoveryScope(independentScope(t, paths, "background", "BACK"))
	server.listRegistryEntries = func(string) ([]store.RegistryEntry, error) {
		return []store.RegistryEntry{registryEntryForProject(project)}, nil
	}
	server.discoverProject = func(context.Context, string) (app.Project, error) { return project, nil }
	server.reapScope = func(context.Context, *store.Store) (int, error) { return 0, nil }
	server.discoverRegistryPass(context.Background())

	reaped := make(chan string, 1)
	server.reapScope = func(ctx context.Context, view *store.Store) (int, error) {
		reaped <- view.ProjectID()
		return 0, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	reaperDone := make(chan struct{})
	go func() {
		server.runReaper(ctx, time.Millisecond)
		close(reaperDone)
	}()
	select {
	case got := <-reaped:
		if got != project.ProjectID {
			t.Fatalf("reaper project=%s want=%s", got, project.ProjectID)
		}
	case <-time.After(time.Second):
		t.Fatal("reaper did not cover discovered scope")
	}
	cancel()
	<-reaperDone

	var flushed string
	server.flushScopeFn = func(_ context.Context, view *store.Store) (int, error) {
		flushed = view.ProjectID()
		return 0, nil
	}
	server.flushReadyProjects(context.Background())
	if flushed != project.ProjectID {
		t.Fatalf("flusher project=%s want=%s", flushed, project.ProjectID)
	}
}

func TestDiscoveryUsesIntactRegistryPrefixDespiteTornTail(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	project := projectForDiscoveryScope(independentScope(t, paths, "torn", "TORN"))
	entry := registryEntryForProject(project)
	data := `{"project_id":"` + entry.ProjectID + `","common_dir":"` + entry.CommonDir + `","worktree_id":"` + entry.WorktreeID + `","root":"` + entry.Root + `","prefixes":["TORN"]}` + "\n" + `{"project_id":"crash`
	if err := os.WriteFile(paths.RegistryPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	server.discoverProject = func(context.Context, string) (app.Project, error) { return project, nil }
	server.discoverRegistryPass(context.Background())
	server.mu.Lock()
	_, covered := server.coveredWorktrees[project.WorktreeID]
	server.mu.Unlock()
	if !covered {
		t.Fatal("intact registry prefix was not discovered")
	}
}

func TestDiscoveryDoesNotResurrectMarkdownIntent(t *testing.T) {
	server, paths := newDiscoveryTestServer(t)
	project := createConfiguredGitProject(t, filepath.Join(filepath.Dir(paths.StateHome), "honesty-projects"), "honesty", "HONEST")
	ticket := domain.Ticket{
		Schema: 1, ID: "HONEST-7", Project: project.Config.Project.Slug, Title: "unreconciled intent",
		Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2,
		Labels: []string{}, Relations: []domain.Relation{},
	}
	data, err := domain.RenderTicket(ticket, "recoverable from the git file")
	if err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(project.Root, ".aira", "tickets", ticket.ID+".md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryRegistry(t, paths.RegistryPath, registryEntryForProject(project))
	server.discoverRegistryPass(context.Background())
	projection, err := sql.Open("sqlite", paths.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Close()
	var projected int
	if err := projection.QueryRow(`SELECT count(*) FROM tickets WHERE project_id=? AND worktree_id=? AND id=?`, project.ProjectID, project.WorktreeID, ticket.ID).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if projected != 0 {
		t.Fatalf("discovery reconciled unreconciled git-file intent %s into the projection", ticket.ID)
	}
	if _, err := os.Stat(ticketPath); err != nil {
		t.Fatalf("unreconciled git-file intent disappeared: %v", err)
	}
	entries, err := store.ListRegistryEntries(paths.RegistryPath)
	if err != nil || len(entries) == 0 {
		t.Fatalf("normal Register breadcrumb side effect missing: entries=%d err=%v", len(entries), err)
	}
}

func requireRealSocket(t *testing.T) {
	t.Helper()
	if os.Getenv("AIRA_REAL_SOCKET") != "1" {
		t.Skip("real Unix-socket daemon lifecycle requires AIRA_REAL_SOCKET=1")
	}
}

func writeDiscoveryRegistry(t *testing.T, path string, entries ...store.RegistryEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := json.NewEncoder(file).Encode(entry); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func serveForDiscoveryTest(t *testing.T, server *Server) (context.CancelFunc, <-chan error, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	return cancel, done, ready
}

func TestServeSignalsReadyBeforeRegistryDiscoveryCompletes(t *testing.T) {
	requireRealSocket(t)
	paths := testPaths(t)
	scope := independentScope(t, paths, "slow-ready", "SLOWREADY")
	project := projectForDiscoveryScope(scope)
	writeDiscoveryRegistry(t, paths.RegistryPath, registryEntryForProject(project))
	server := NewServer(paths)
	started := make(chan struct{})
	release := make(chan struct{})
	server.discoverProject = func(context.Context, string) (app.Project, error) {
		close(started)
		<-release
		return project, nil
	}
	cancel, done, ready := serveForDiscoveryTest(t, server)
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Serve exited before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not signal Ready")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("post-Ready discovery did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("Serve exited while discovery was blocked: %v", err)
	default:
	}
	close(release)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestServeDrainsRegistryDiscoveryBeforeClosingDB(t *testing.T) {
	requireRealSocket(t)
	paths := testPaths(t)
	scope := independentScope(t, paths, "drain", "DRAIN")
	project := projectForDiscoveryScope(scope)
	writeDiscoveryRegistry(t, paths.RegistryPath, registryEntryForProject(project))
	server := NewServer(paths)
	server.discoverProject = func(context.Context, string) (app.Project, error) { return project, nil }
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		close(buildStarted)
		<-releaseBuild
		return 0, nil
	}
	dbClosed := make(chan struct{})
	server.closeDB = func(db *store.DB) error {
		close(dbClosed)
		return db.Close()
	}
	cancel, done, ready := serveForDiscoveryTest(t, server)
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Serve exited before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not signal Ready")
	}
	select {
	case <-buildStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not enter scope build")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Serve returned before discovery drained: %v", err)
	case <-dbClosed:
		t.Fatal("DB closed while discovery still used it")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseBuild)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after discovery drained")
	}
	select {
	case <-dbClosed:
	default:
		t.Fatal("DB was not closed after discovery drained")
	}
}

func TestServeDiscoveryDrainTimeoutRetainsInstance(t *testing.T) {
	requireRealSocket(t)
	t.Setenv("AIRA_DAEMON_REAP_INTERVAL", "disabled")
	t.Setenv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL", "disabled")
	paths := testPaths(t)
	project := projectForDiscoveryScope(independentScope(t, paths, "drain-timeout", "DRAINTIMEOUT"))
	writeDiscoveryRegistry(t, paths.RegistryPath, registryEntryForProject(project))
	server := NewServer(paths)
	server.DrainTimeout = 20 * time.Millisecond
	server.discoverProject = func(context.Context, string) (app.Project, error) { return project, nil }
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	buildFinished := make(chan struct{})
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		close(buildStarted)
		<-releaseBuild
		close(buildFinished)
		return 0, nil
	}
	var closes atomic.Int32
	server.closeDB = func(db *store.DB) error {
		closes.Add(1)
		return db.Close()
	}
	cancel, done, ready := serveForDiscoveryTest(t, server)
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Serve exited before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not signal Ready")
	}
	select {
	case <-buildStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not enter scope build")
	}
	cancel()
	serveErr := <-done
	var drainTimeout *ErrDrainTimeout
	if !errors.As(serveErr, &drainTimeout) || drainTimeout.lock == nil {
		t.Fatalf("Serve error=%T %v, want lock-owning ErrDrainTimeout", serveErr, serveErr)
	}
	if closes.Load() != 0 {
		t.Fatal("DB closed while timed-out discovery still used it")
	}
	replacementCtx, replacementCancel := context.WithTimeout(context.Background(), time.Second)
	defer replacementCancel()
	if replacementErr := NewServer(paths).Serve(replacementCtx); !errors.Is(replacementErr, ErrAlreadyRunning) {
		t.Fatalf("replacement Serve=%v, want ErrAlreadyRunning", replacementErr)
	}
	close(releaseBuild)
	<-buildFinished
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := drainTimeout.lock.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(paths.SocketPath)
}

func TestServeDiscoveryReapsPriorLifetimeProjectWithoutRequest(t *testing.T) {
	requireRealSocket(t)
	paths := testPaths(t)
	root := filepath.Join(filepath.Dir(paths.StateHome), "prior-project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"prior","prefixes":["PRIOR"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := app.Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := ScopeFromProject(project, paths)
	if err != nil {
		t.Fatal(err)
	}
	scope.LeaseTTLNS = 1
	prepareExpiredDaemonLease(t, paths, scope)

	server := NewServer(paths)
	cancel, done, ready := serveForDiscoveryTest(t, server)
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Serve exited before Ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not signal Ready")
	}
	waitForCondition(t, 5*time.Second, "prior-lifetime expired lease was not reaped", func() bool {
		conn, openErr := sql.Open("sqlite", paths.DBPath)
		if openErr != nil {
			return false
		}
		defer conn.Close()
		var lapses int
		return conn.QueryRow(`SELECT count(*) FROM events WHERE project_id=? AND verb='lease.lapse'`, project.ProjectID).Scan(&lapses) == nil && lapses == 1
	})
	server.mu.Lock()
	_, covered := server.coveredWorktrees[project.WorktreeID]
	server.mu.Unlock()
	if !covered {
		t.Fatal("prior-lifetime worktree was reaped but not cached")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
