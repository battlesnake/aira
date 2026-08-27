package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/store"
)

func commitAIRA(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{
		{"config", "user.email", "aira-test@example.invalid"},
		{"config", "user.name", "AIRA Test"},
		{"add", ".aira"},
		{"commit", "-qm", "track aira"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
}

func TestInitAdoptsCommittedFilesRebuildsAndClearsTombstone(t *testing.T) {
	server, scope, view := lifecycleFixture(t)
	if _, err := view.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "survives", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	commitAIRA(t, scope.Root)
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("eject=%+v", got)
	}
	bootstrap := scope
	bootstrap.Bootstrap = true
	response := server.bootstrap(context.Background(), bootstrap, map[string]any{})
	if !response.OK {
		t.Fatalf("adopt=%+v", response)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	var tickets, tombstones, prefixes int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM tickets WHERE project_id='` + scope.ProjectID + `' AND id='LIFE-1'`: &tickets,
		`SELECT count(*) FROM ejections WHERE project_id='` + scope.ProjectID + `'`:               &tombstones,
		`SELECT count(*) FROM prefix_ownership WHERE project_id='` + scope.ProjectID + `'`:        &prefixes,
	} {
		if err := db.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if tickets != 1 || tombstones != 0 || prefixes != 1 {
		t.Fatalf("tickets=%d tombstones=%d prefixes=%d", tickets, tombstones, prefixes)
	}
}

func TestAdoptionRebuildFailureRollsBackClaimAndBreadcrumb(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	commitAIRA(t, scope.Root)
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("eject=%+v", got)
	}
	before, err := os.ReadFile(server.Paths.RegistryPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	server.adoptRebuild = func(context.Context, *store.Store) error { return errors.New("injected rebuild failure") }
	bootstrap := scope
	bootstrap.Bootstrap = true
	response := server.bootstrap(context.Background(), bootstrap, map[string]any{})
	if response.OK || !strings.Contains(response.Error, "injected rebuild failure") {
		t.Fatalf("adopt=%+v", response)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	for _, table := range []string{"projects", "prefix_ownership"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE project_id=?`, scope.ProjectID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("failed adopt left %s=%d", table, count)
		}
	}
	after, err := os.ReadFile(server.Paths.RegistryPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed adopt changed registry before=%q after=%q", before, after)
	}
}

func TestCommittedConfigArgumentMismatchFailsBeforeClaim(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	commitAIRA(t, scope.Root)
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("eject=%+v", got)
	}
	bootstrap := scope
	bootstrap.Bootstrap = true
	response := server.bootstrap(context.Background(), bootstrap, map[string]any{"project": "different"})
	if response.OK || !strings.Contains(response.Error, "mismatch") {
		t.Fatalf("response=%+v", response)
	}
}

func TestModifiedCommittedConfigFailsBeforeClaim(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	commitAIRA(t, scope.Root)
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("eject=%+v", got)
	}
	modified := `{"schema":1,"project":{"slug":"other","prefixes":["OTHER"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}` + "\n"
	if err := os.WriteFile(filepath.Join(scope.Root, ".aira", "config"), []byte(modified), 0o644); err != nil {
		t.Fatal(err)
	}
	bootstrap := scope
	bootstrap.Bootstrap = true
	response := server.bootstrap(context.Background(), bootstrap, map[string]any{})
	if response.OK || response.Code != "E_CONFIG_INVALID" || !strings.Contains(response.Error, "committed config") {
		t.Fatalf("response=%+v", response)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	var projects, prefixes, tombstones int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM projects WHERE project_id=?`:         &projects,
		`SELECT count(*) FROM prefix_ownership WHERE project_id=?`: &prefixes,
		`SELECT count(*) FROM ejections WHERE project_id=?`:        &tombstones,
	} {
		if err := db.QueryRow(query, scope.ProjectID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if projects != 0 || prefixes != 0 || tombstones != 1 {
		t.Fatalf("modified config claim projects=%d prefixes=%d tombstones=%d", projects, prefixes, tombstones)
	}
}

func TestInitWithCommittedConfigRefusesAlreadyAdoptedProjectCleanly(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	commitAIRA(t, scope.Root)
	bootstrap := scope
	bootstrap.Bootstrap = true
	response := server.bootstrap(context.Background(), bootstrap, map[string]any{})
	if response.OK || response.Code != "E_ALREADY_INITIALIZED" {
		t.Fatalf("response=%+v", response)
	}
	db := lifecycleSQL(t, server.Paths.DBPath)
	var projects, prefixes, tombstones int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM projects WHERE project_id=?`:         &projects,
		`SELECT count(*) FROM prefix_ownership WHERE project_id=?`: &prefixes,
		`SELECT count(*) FROM ejections WHERE project_id=?`:        &tombstones,
	} {
		if err := db.QueryRow(query, scope.ProjectID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if projects != 1 || prefixes != 1 || tombstones != 0 {
		t.Fatalf("repeat init changed registration projects=%d prefixes=%d tombstones=%d", projects, prefixes, tombstones)
	}
}

func TestAdoptionPrefixConflictNamesOwnerAndEject(t *testing.T) {
	server, scope, _ := lifecycleFixture(t)
	commitAIRA(t, scope.Root)
	if got := server.eject(context.Background(), map[string]any{"project": scope.ProjectID, "force": true}); !got.OK {
		t.Fatalf("eject=%+v", got)
	}
	owner := independentScope(t, server.Paths, "owner", "LIFE")
	if _, _, err := server.storeForScope(owner); err != nil {
		t.Fatal(err)
	}
	bootstrap := scope
	bootstrap.Bootstrap = true
	response := server.bootstrap(context.Background(), bootstrap, map[string]any{})
	if response.OK || response.Code != "E_PREFIX_OWNERSHIP_CONFLICT" || !strings.Contains(response.Error, owner.ProjectID) || !strings.Contains(response.Error, "aira eject") {
		t.Fatalf("response=%+v", response)
	}
}
