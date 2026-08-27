package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/domain"
	"aira/internal/store"
)

func lifecycleFixture(t *testing.T) (*Server, WorktreeScope, *store.Store) {
	t.Helper()
	paths := testPaths(t)
	root := filepath.Join(filepath.Dir(paths.StateHome), "project")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"life","prefixes":["LIFE"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config), 0o644); err != nil {
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
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(paths)
	server.db = db
	view, _, err := server.storeForScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	return server, scope, view
}

func lifecycleSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEjectTombstonePreventsScopeResurrectionAndInitAloneMayClearIt(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	if !response.OK {
		t.Fatalf("eject: %+v", response)
	}
	if _, _, err := server.storeForScope(scope); store.ErrorCode(err) != "E_NOT_ADOPTED" {
		t.Fatalf("post-eject scope error=%v, want E_NOT_ADOPTED", err)
	}
	restarted := NewServer(server.Paths)
	restarted.db = server.db
	if _, _, err := restarted.storeForScope(scope); store.ErrorCode(err) != "E_NOT_ADOPTED" {
		t.Fatalf("post-restart scope error=%v, want E_NOT_ADOPTED", err)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	for _, table := range []string{"projects", "prefix_ownership"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE project_id=?`, scope.ProjectID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s resurrected %d rows", table, count)
		}
	}
	var tombstones int
	if err := db.QueryRow(`SELECT count(*) FROM ejections WHERE project_id=?`, scope.ProjectID).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Fatalf("tombstones=%d err=%v", tombstones, err)
	}
	assertNoProjectRowsAfterEject(t, db, scope.ProjectID)
}

func assertNoProjectRowsAfterEject(t *testing.T, db *sql.DB, projectID string) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		columns, err := db.Query(`PRAGMA table_info("` + strings.ReplaceAll(table, `"`, `""`) + `")`)
		if err != nil {
			t.Fatal(err)
		}
		hasProject := false
		for columns.Next() {
			var cid, notNull, primaryKey int
			var name, typ string
			var defaultValue sql.NullString
			if err := columns.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
				t.Fatal(err)
			}
			hasProject = hasProject || name == "project_id"
		}
		if err := columns.Close(); err != nil {
			t.Fatal(err)
		}
		if !hasProject || table == "ejections" {
			continue
		}
		var count int
		query := `SELECT count(*) FROM "` + strings.ReplaceAll(table, `"`, `""`) + `" WHERE project_id=?`
		if err := db.QueryRow(query, projectID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("eject left %d rows in introspected table %s", count, table)
		}
	}
}

func TestRefusedEjectLeavesProjectAndNoTombstone(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	db := lifecycleSQL(t, server.Paths.DBPath)
	_, err := db.Exec(`INSERT INTO leases(project_id,ticket_id,state,generation,holder_token_hash,boot_id,last_heartbeat_mono_ns,ttl_ns,actor,worktree_id)
		VALUES (?, 'LIFE-1', 'held', 1, ?, (SELECT trim(readfile('/proc/sys/kernel/random/boot_id'))), 0, 9223372036854775807, 'agent-one', ?)`,
		scope.ProjectID, strings.Repeat("a", 43), scope.WorktreeID)
	if err != nil {
		// modernc SQLite does not expose readfile(); seed the boot id from Go.
		boot, readErr := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if readErr != nil {
			t.Fatal(readErr)
		}
		_, err = db.Exec(`INSERT INTO leases(project_id,ticket_id,state,generation,holder_token_hash,boot_id,last_heartbeat_mono_ns,ttl_ns,actor,worktree_id)
			VALUES (?, 'LIFE-1', 'held', 1, ?, ?, 0, 9223372036854775807, 'agent-one', ?)`, scope.ProjectID, strings.Repeat("a", 43), strings.TrimSpace(string(boot)), scope.WorktreeID)
	}
	if err != nil {
		t.Fatal(err)
	}
	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID})
	if response.Code != "E_EJECT_LIVE_STATE" || !strings.Contains(response.Error, "agent-one") {
		t.Fatalf("response=%+v", response)
	}
	var projects, tombstones int
	if err := db.QueryRow(`SELECT count(*) FROM projects WHERE project_id=?`, scope.ProjectID).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM ejections WHERE project_id=?`, scope.ProjectID).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if projects != 1 || tombstones != 0 {
		t.Fatalf("refusal projects=%d tombstones=%d", projects, tombstones)
	}
	if _, _, err := server.storeForScope(scope); err != nil {
		t.Fatalf("refused target was bricked: %v", err)
	}
}

func TestLiveSupervisorLeaseRefusesUnlessForced(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	db := lifecycleSQL(t, server.Paths.DBPath)
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO supervisor_leases(project_id,run_id,state,generation,holder_token_hash,holder_pid,holder_start_tick,holder_boot_id,last_heartbeat_mono_ns,ttl_ns,actor,worktree_id)
		VALUES (?, 'RUN-7', 'held', 1, ?, 1234, 1, ?, 0, 9223372036854775807, 'aira-supervisor', ?)`, scope.ProjectID, strings.Repeat("a", 43), strings.TrimSpace(string(boot)), scope.WorktreeID); err != nil {
		t.Fatal(err)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID}); got.Code != "E_EJECT_LIVE_STATE" || !strings.Contains(got.Error, "RUN-7") {
		t.Fatalf("supervisor refusal=%+v", got)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("forced supervisor eject=%+v", got)
	}
}

func TestReviewedRantCascadesDuringEjectAndLeavesNoProjectRows(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	db := lifecycleSQL(t, server.Paths.DBPath)
	if _, err := db.Exec(`INSERT INTO rants(project_id,id,body,actor,received_at,seq) VALUES (?, 'RANT-1', 'body', 'agent', 'now', 1)`, scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO rant_reviews(project_id,rant_id,reviewer,at,note,outcome) VALUES (?, 'RANT-1', 'reviewer', 'now', 'note', 'actioned')`, scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	if !response.OK {
		t.Fatalf("eject reviewed rant: %+v", response)
	}
	for _, table := range []string{"rants", "rant_reviews"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE project_id=?`, scope.ProjectID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d rows", table, count)
		}
	}
}

func TestEjectDrainsOutboxButRefusesFileIndexDivergence(t *testing.T) {
	server, scope, view := lifecycleFixture(t)
	if _, err := view.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "kept", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	if err := os.WriteFile(filepath.Join(scope.Root, ".aira", "tickets", "LIFE-1.md"), []byte("diverged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	if response.Code != "E_EJECT_UNVERIFIED" {
		t.Fatalf("response=%+v", response)
	}
	var projectRows, tombstones int
	if err := db.QueryRow(`SELECT count(*) FROM projects WHERE project_id=?`, scope.ProjectID).Scan(&projectRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM ejections WHERE project_id=?`, scope.ProjectID).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if projectRows != 1 || tombstones != 0 {
		t.Fatalf("divergence refusal project=%d tombstone=%d", projectRows, tombstones)
	}
}

func staleMaterializedTicket(t *testing.T, view *store.Store, scope WorktreeScope) {
	t.Helper()
	if _, err := view.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "stale", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scope.Root, ".aira", "tickets", "LIFE-1.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	report, err := view.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["stale-index"] != "warning" {
		t.Fatalf("stale-index=%q, want warning", report.Dimensions["stale-index"])
	}
}

func TestEjectPurgeSkipsDurabilityForStaleMaterializedIndex(t *testing.T) {
	server, scope, view := lifecycleFixture(t)
	staleMaterializedTicket(t, view, scope)

	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "purge": true, "force": true})
	if !response.OK {
		t.Fatalf("purge stale index=%+v", response)
	}
	if _, err := os.Stat(filepath.Join(scope.Root, ".aira")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purge left .aira: %v", err)
	}
}

func TestEjectKeepsDurabilityCheckForStaleMaterializedIndex(t *testing.T) {
	server, scope, view := lifecycleFixture(t)
	staleMaterializedTicket(t, view, scope)

	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	if response.Code != "E_EJECT_UNVERIFIED" || !strings.Contains(response.Error, "stale-index") {
		t.Fatalf("deregister stale index=%+v", response)
	}
}

func TestEjectPurgePreservesWorktreeIdentityGuard(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	replacementRoot := filepath.Join(t.TempDir(), "replacement")
	if err := os.MkdirAll(filepath.Join(replacementRoot, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", replacementRoot, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"replacement","prefixes":["REPLACED"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}` + "\n"
	if err := os.WriteFile(filepath.Join(replacementRoot, ".aira", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	replacement, err := app.Discover(context.Background(), replacementRoot)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ProjectID == scope.ProjectID {
		t.Fatalf("replacement project ID=%s, want different from %s", replacement.ProjectID, scope.ProjectID)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	if _, err := db.Exec(`UPDATE worktrees SET root=? WHERE project_id=? AND worktree_id=?`, replacementRoot, scope.ProjectID, scope.WorktreeID); err != nil {
		t.Fatal(err)
	}

	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "purge": true, "force": true})
	if response.Code != "E_EJECT_UNVERIFIED" || !strings.Contains(response.Error, replacement.ProjectID) {
		t.Fatalf("purge replacement root=%+v", response)
	}
	if _, err := os.Stat(filepath.Join(replacementRoot, ".aira")); err != nil {
		t.Fatalf("identity refusal purged replacement .aira: %v", err)
	}
}

func TestEjectDrainsPendingMaterialisationBeforeDrop(t *testing.T) {
	server, scope, view := lifecycleFixture(t)
	if _, err := view.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "drain", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	path := filepath.Join(scope.Root, ".aira", "tickets", "LIFE-1.md")
	intended, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE outbox SET materialised=0,resolution=NULL,intended_bytes=?,precondition_digest='' WHERE project_id=? AND path<>''`, intended, scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("eject=%+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("drained canonical file missing after deregister: %v", err)
	}
}

func TestEjectTransactionReassertsOutboxAfterDurabilityCheck(t *testing.T) {
	server, scope, view := lifecycleFixture(t)
	if _, err := view.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "race", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	server.beforeEjectTransaction = func() {
		if _, err := db.Exec(`UPDATE outbox SET materialised=0,resolution=NULL WHERE project_id=? AND path<>''`, scope.ProjectID); err != nil {
			t.Errorf("inject pending outbox: %v", err)
		}
	}
	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	if response.Code != "E_EJECT_UNVERIFIED" || !strings.Contains(response.Error, "unresolved materialisations") {
		t.Fatalf("response=%+v", response)
	}
	var projects, tombstones int
	if err := db.QueryRow(`SELECT count(*) FROM projects WHERE project_id=?`, scope.ProjectID).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM ejections WHERE project_id=?`, scope.ProjectID).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if projects != 1 || tombstones != 0 {
		t.Fatalf("race refusal projects=%d tombstones=%d", projects, tombstones)
	}
}

func TestEjectChecksEveryRegisteredWorktree(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	commitAIRA(t, scope.Root)
	secondRoot := filepath.Join(filepath.Dir(scope.Root), "second")
	if output, err := exec.Command("git", "-C", scope.Root, "worktree", "add", "-q", "-b", "second", secondRoot).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, output)
	}
	secondProject, err := app.Discover(context.Background(), secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	secondScope, err := ScopeFromProject(secondProject, server.Paths)
	if err != nil {
		t.Fatal(err)
	}
	secondView, _, err := server.storeForScope(secondScope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondView.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "second", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, ".aira", "tickets", "LIFE-1.md"), []byte("diverged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); got.Code != "E_EJECT_UNVERIFIED" {
		t.Fatalf("multi-worktree eject=%+v", got)
	}
}

func TestEjectNonENOENTStatFailureIsNotTreatedAsGone(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	component := filepath.Join(scope.Root, "not-a-directory")
	if err := os.WriteFile(component, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	brokenRoot := filepath.Join(component, "child")
	if _, err := os.Stat(brokenRoot); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("broken registered root stat err=%v, want non-ENOENT", err)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	if _, err := db.Exec(`UPDATE worktrees SET root=? WHERE project_id=? AND worktree_id=?`, brokenRoot, scope.ProjectID, scope.WorktreeID); err != nil {
		t.Fatal(err)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); got.Code != "E_EJECT_UNVERIFIED" {
		t.Fatalf("non-ENOENT stat eject=%+v", got)
	}
}

func TestEjectGoneRootRequiresForceAndPermissionErrorIsNeverGone(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	if err := os.RemoveAll(scope.Root); err != nil {
		t.Fatal(err)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID}); got.Code != "E_EJECT_UNVERIFIED" {
		t.Fatalf("gone root without force=%+v", got)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("gone root with force=%+v", got)
	}
}

func TestPurgeRefusesDirtyAIRAAndDoesNotFollowSymlink(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	if err := os.WriteFile(filepath.Join(scope.Root, ".gitignore"), []byte(".aira/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope.Root, ".aira", "untracked"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "purge": true}); got.Code != "E_PURGE_DIRTY" {
		t.Fatalf("dirty purge=%+v", got)
	}
	if _, err := os.Stat(filepath.Join(scope.Root, ".aira", "untracked")); err != nil {
		t.Fatalf("dirty refusal removed file: %v", err)
	}
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "purge": true, "force": true}); !got.OK {
		t.Fatalf("forced dirty purge=%+v", got)
	}
	if _, err := os.Stat(filepath.Join(scope.Root, ".aira")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forced purge left .aira: %v", err)
	}

	escapeRoot := t.TempDir()
	outside := filepath.Join(escapeRoot, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(escapeRoot, "root")
	if err := os.Mkdir(linkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkRoot, ".aira")); err != nil {
		t.Fatal(err)
	}
	if err := purgeAIRA(linkRoot); err == nil {
		t.Fatal("symlink purge unexpectedly succeeded")
	}
	if data, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(data) != "safe" {
		t.Fatalf("symlink escape touched sentinel data=%q err=%v", data, err)
	}
}

func TestEjectRejectsMissingIndexedRequirement(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	if err := os.MkdirAll(filepath.Join(scope.Root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	missingPath := filepath.Join(scope.Root, ".aira", "requirements", "AR-1.md")
	if _, err := db.Exec(`INSERT INTO requirements(project_id,worktree_id,id,path,digest,status,text) VALUES(?,?,?,?,?,?,?)`,
		scope.ProjectID, scope.WorktreeID, "AR-1", missingPath, "digest", "planned", "must survive"); err != nil {
		t.Fatal(err)
	}
	got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	if got.Code != "E_EJECT_UNVERIFIED" || !strings.Contains(got.Error, "stale-index") {
		t.Fatalf("missing indexed requirement eject=%+v", got)
	}
}

func TestEjectSelectorErrorsAreStable(t *testing.T) {
	server, _, _ := lifecycleFixture(t)
	if got := server.eject(context.Background(), map[string]any{"prefix": "NONE"}); got.Code != "E_NOT_ADOPTED" {
		t.Fatalf("missing prefix=%+v", got)
	}
	if got := server.eject(context.Background(), map[string]any{}); got.Code != "E_NO_PROJECT" {
		t.Fatalf("missing selector=%+v", got)
	}
}

func TestPurgeMissingRootIsNoop(t *testing.T) {
	if err := purgeAIRA(filepath.Join(t.TempDir(), "missing")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestEjectExcludesCachedProjectUseBeforeCommit(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	release, err := server.beginProjectUse(scope.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan core.Response, 1)
	go func() {
		done <- server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	}()
	var refused atomic.Bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := server.storeForScope(scope); store.ErrorCode(err) == "E_NOT_ADOPTED" {
			refused.Store(true)
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !refused.Load() {
		release()
		t.Fatal("eject guard never excluded cached scope")
	}
	select {
	case response := <-done:
		release()
		t.Fatalf("eject committed while cached project use remained active: %+v", response)
	default:
	}
	release()
	if response := <-done; !response.OK {
		t.Fatalf("eject after active use drained: %+v", response)
	}
}

func TestEjectWaitsForPeriodicProjectUseBeforeCommit(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	started := make(chan struct{})
	releaseReap := make(chan struct{})
	server.reapScope = func(context.Context, *store.Store) (int, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-releaseReap
		return 0, nil
	}
	reaperCtx, cancelReaper := context.WithCancel(context.Background())
	defer cancelReaper()
	go server.runReaper(reaperCtx, time.Millisecond)
	<-started

	done := make(chan core.Response, 1)
	go func() {
		done <- server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true})
	}()
	select {
	case response := <-done:
		close(releaseReap)
		t.Fatalf("eject committed while periodic project use remained active: %+v", response)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseReap)
	if response := <-done; !response.OK {
		t.Fatalf("eject after periodic use drained: %+v", response)
	}
}

func TestEjectDurabilityRejectsFailedOrUnevaluatedIndexDimension(t *testing.T) {
	for name, report := range map[string]store.CheckReport{
		"failed without index code": {
			Dimensions: map[string]string{"duplicate-id": "fail"},
			Findings:   []store.CheckFinding{{Code: "E_DUPLICATE_ID", Message: "duplicate durable IDs", Kind: "fail"}},
		},
		"unevaluated without index code": {
			Dimensions:          map[string]string{"relation-integrity": "unevaluated"},
			UnevaluatedFindings: []store.CheckFinding{{Code: "U_RELATION_GRAPH_UNESTABLISHED", Message: "cannot establish graph", Kind: "unevaluated"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ejectDurabilityFinding(report); got == "" {
				t.Fatal("unsafe index dimension was accepted")
			}
		})
	}
}

func TestPurgeRechecksDirtyStateBeforeEjectTransaction(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	commitAIRA(t, scope.Root)
	server.beforeEjectTransaction = func() {
		if err := os.WriteFile(filepath.Join(scope.Root, ".aira", "late-untracked"), []byte("keep"), 0o644); err != nil {
			t.Errorf("inject late dirty file: %v", err)
		}
	}
	response := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "purge": true})
	if response.Code != "E_PURGE_DIRTY" {
		t.Fatalf("response=%+v", response)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	var projects, tombstones int
	if err := db.QueryRow(`SELECT count(*) FROM projects WHERE project_id=?`, scope.ProjectID).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM ejections WHERE project_id=?`, scope.ProjectID).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if projects != 1 || tombstones != 0 {
		t.Fatalf("late dirty refusal projects=%d tombstones=%d", projects, tombstones)
	}
	if _, err := os.Stat(filepath.Join(scope.Root, ".aira", "late-untracked")); err != nil {
		t.Fatalf("late dirty file was removed: %v", err)
	}
}
