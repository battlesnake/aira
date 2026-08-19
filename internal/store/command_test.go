package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

func commandPtr(value int64) *int64 { return &value }

func commandInput(key string, source domain.CommandKeySource, status domain.CommandOutcome, exit, wall *int64) domain.CommandEventInput {
	return domain.CommandEventInput{Key: key, KeySource: source, Program: strings.Fields(key)[0], ArgvDigest: strings.Repeat("a", 64), Status: status, ExitCode: exit, WallMS: wall}
}

func TestCommandEventDBRejectsIllegalOutcomeAndGitPairings(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	base := []any{s.projectID, "CMD-900", "2026-08-18T00:00:00Z", 900, "go test", "program-subcommand", "go", "", "digest", "", "exited", nil, "", 1, "", "", "", "", "", "", "unevaluated", "", "unevaluated", "", "unevaluated"}
	insert := `INSERT INTO command_events(project_id,id,at,at_seq,key,key_source,program,argv_preview,argv_digest,prefix_preview,status,exit_code,signal,wall_ms,ticket_id,phase,actor,session,cwd,head_hash,head_hash_status,head_ref,head_ref_status,worktree_id,worktree_id_status) VALUES(` + strings.TrimSuffix(strings.Repeat("?,", len(base)), ",") + `)`
	if _, err := s.db.Exec(insert, base...); err == nil {
		t.Fatal("DB accepted exited without exit_code")
	}
	illegalOutcomes := []struct {
		name     string
		status   string
		exitCode any
		signal   string
		wallMS   any
	}{
		{name: "signalled-with-exit-code", status: "signalled", exitCode: int64(0), signal: "TERM", wallMS: int64(1)},
		{name: "exited-with-signal", status: "exited", exitCode: int64(0), signal: "KILL", wallMS: int64(1)},
		{name: "launch-failed-with-wall", status: "launch-failed", exitCode: nil, signal: "", wallMS: int64(1)},
	}
	for _, test := range illegalOutcomes {
		t.Run(test.name, func(t *testing.T) {
			values := append([]any(nil), base...)
			values[10], values[11], values[12], values[13] = test.status, test.exitCode, test.signal, test.wallMS
			if _, err := s.db.Exec(insert, values...); err == nil {
				t.Fatalf("DB accepted illegal outcome pairing: status=%q exit_code=%v signal=%q wall_ms=%v", test.status, test.exitCode, test.signal, test.wallMS)
			}
		})
	}
	badGit := append([]any(nil), base...)
	badGit[11] = int64(0)
	badGit[19], badGit[20] = "", "value"
	if _, err := s.db.Exec(insert, badGit...); err == nil {
		t.Fatal("DB accepted value git status with empty value")
	}
}

func TestCommandEventsRoundTripRetentionFiltersAndGitStates(t *testing.T) {
	base := t.TempDir()
	s, err := Open(context.Background(), Options{Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira", WorktreeID: "main", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, MaxCommandEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := gitcontext.GitContext{HeadHash: gitcontext.Field{Status: gitcontext.StatusUnevaluated}, HeadRef: gitcontext.Field{Value: "refs/heads/main", Status: gitcontext.StatusValue}, WorktreeID: gitcontext.Field{Value: "wrong", Status: gitcontext.StatusValue}}
	for i, key := range []string{"go build", "go test", "pytest"} {
		input := commandInput(key, domain.CommandKeyProgramSubcommand, domain.CommandExited, commandPtr(0), commandPtr(int64(i+1)))
		input.GitContext, input.TicketID, input.Phase = ctx, "AIRA-1", "implement"
		result, err := s.AddCommandEvent(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if result.ID != "CMD-"+string(rune('1'+i)) {
			t.Fatalf("id=%q", result.ID)
		}
		if i == 2 && (result.EvictedCount != 1 || result.Remaining != 2) {
			t.Fatalf("retention=%#v", result)
		}
	}
	rows, err := s.ListCommandEvents("ticket:AIRA-1 phase:implement")
	if err != nil || len(rows) != 2 || rows[0].ID != "CMD-3" || rows[1].ID != "CMD-2" {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	if rows[0].GitContext.HeadRef.Status != gitcontext.StatusMismatch || rows[0].GitContext.HeadRef.Value != "refs/heads/main" || rows[0].GitContext.HeadHash.Status != gitcontext.StatusUnevaluated || rows[0].GitContext.WorktreeID.Status != gitcontext.StatusMismatch {
		t.Fatalf("four-state git context not preserved: %#v", rows[0].GitContext)
	}
	if rows[0].GitContext.HeadRef.Reason != "" || rows[0].GitContext.WorktreeID.Reason != "" {
		t.Fatalf("high-volume command row retained git reasons: %#v", rows[0].GitContext)
	}
	if miss, err := s.ListCommandEvents("branch:main"); err != nil || len(miss) != 0 {
		t.Fatalf("mismatched branch unexpectedly matched filter: rows=%#v err=%v", miss, err)
	}
	var journalRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=?`, s.projectID).Scan(&journalRows); err != nil || journalRows != 0 {
		t.Fatalf("journal rows=%d err=%v", journalRows, err)
	}
	if _, err := os.Stat(filepath.Join(base, ".aira")); !os.IsNotExist(err) {
		t.Fatalf("command telemetry materialised files: %v", err)
	}
}

func TestCommandDistributionUsesPairAndExitedOnlyFailureDenominator(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	add := func(source domain.CommandKeySource, status domain.CommandOutcome, code *int64, signal string) {
		input := commandInput("go test", source, status, code, commandPtr(10))
		input.Signal = signal
		if _, err := s.AddCommandEvent(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		add(domain.CommandKeyProgramSubcommand, domain.CommandExited, commandPtr(0), "")
	}
	add(domain.CommandKeyProgramSubcommand, domain.CommandExited, commandPtr(2), "")
	add(domain.CommandKeyProgramSubcommand, domain.CommandSignalled, nil, "TERM")
	add(domain.CommandKeyProgramSubcommand, domain.CommandTimeout, nil, "KILL")
	launchFailed := commandInput("go test", domain.CommandKeyProgramSubcommand, domain.CommandLaunchFailed, nil, nil)
	if _, err := s.AddCommandEvent(context.Background(), launchFailed); err != nil {
		t.Fatal(err)
	}
	add(domain.CommandKeyProgramSubcommand, domain.CommandUnknown, nil, "")
	add(domain.CommandKeyLabel, domain.CommandExited, commandPtr(0), "")
	result, err := s.CommandDistribution("", "key")
	if err != nil || result.Total != 9 || len(result.Groups) != 2 {
		t.Fatalf("distribution=%#v err=%v", result, err)
	}
	var heuristic CommandDistributionGroup
	for _, group := range result.Groups {
		if group.KeySource == domain.CommandKeyProgramSubcommand {
			heuristic = group
		}
	}
	if heuristic.Count != 8 || heuristic.Exited != 4 || heuristic.ExitedNonzero != 1 || heuristic.Signalled != 1 || heuristic.Timeout != 1 || heuristic.LaunchFailed != 1 || heuristic.Unknown != 1 {
		t.Fatalf("heuristic=%#v", heuristic)
	}
	if failureRate := float64(heuristic.ExitedNonzero) / float64(heuristic.Exited); failureRate != float64(1)/4 {
		t.Fatalf("failure rate=%v from exited_nonzero=%d exited=%d", failureRate, heuristic.ExitedNonzero, heuristic.Exited)
	}
	paired, err := s.ListCommandEvents("key-source:program-subcommand key:go test")
	if err != nil || len(paired) != 8 {
		t.Fatalf("paired drill rows=%d err=%v", len(paired), err)
	}
	weird := commandInput("go test status:exited", domain.CommandKeyLabel, domain.CommandExited, commandPtr(0), commandPtr(1))
	if _, err := s.AddCommandEvent(context.Background(), weird); err != nil {
		t.Fatal(err)
	}
	paired, err = s.ListCommandEvents("key-source:label key:go test status:exited")
	if err != nil || len(paired) != 1 || paired[0].Key != weird.Key {
		t.Fatalf("filter-looking key drill=%#v err=%v", paired, err)
	}
}

func TestCommandLatencyNearestRankAndPerStatisticFloors(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	addKey := func(key string, n int) {
		for i := 1; i <= n; i++ {
			input := commandInput(key, domain.CommandKeyProgram, domain.CommandExited, commandPtr(0), commandPtr(int64(i)))
			if _, err := s.AddCommandEvent(context.Background(), input); err != nil {
				t.Fatal(err)
			}
		}
	}
	addKey("three", 3)
	addKey("six", 6)
	addKey("twenty", 20)
	rows, err := s.CommandLatencyByKeyPair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]CommandLatencySummary{}
	for _, row := range rows {
		byKey[row.Key] = row
	}
	if byKey["three"].P50MS != nil || byKey["three"].P95MS != nil {
		t.Fatalf("three=%#v", byKey["three"])
	}
	if byKey["six"].P50MS == nil || *byKey["six"].P50MS != 3 || byKey["six"].P95MS != nil {
		t.Fatalf("six=%#v", byKey["six"])
	}
	if byKey["twenty"].P50MS == nil || *byKey["twenty"].P50MS != 10 || byKey["twenty"].P95MS == nil || *byKey["twenty"].P95MS != 19 {
		t.Fatalf("twenty=%#v", byKey["twenty"])
	}
}

func TestCommandBranchDistributionPreservesNonValueStates(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	statuses := []gitcontext.Field{{Value: "refs/heads/x", Status: gitcontext.StatusValue}, {Status: gitcontext.StatusNone}, {Status: gitcontext.StatusUnevaluated}, {Value: "refs/heads/y", Status: gitcontext.StatusMismatch}}
	for _, field := range statuses {
		input := commandInput("true", domain.CommandKeyProgram, domain.CommandExited, commandPtr(0), commandPtr(1))
		input.GitContext.HeadRef = field
		if _, err := s.AddCommandEvent(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	dist, err := s.CommandDistribution("", "branch")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"(mismatch)", "(none)", "(unevaluated)", "x"}
	got := make([]string, len(dist.Groups))
	for i := range dist.Groups {
		got[i] = dist.Groups[i].Value
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("branches=%v want=%v", got, want)
	}
}

func TestCommandUnknownMayPersistNilOrMeasuredWall(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	for _, wall := range []*int64{nil, commandPtr(3)} {
		input := commandInput("odd", domain.CommandKeyProgram, domain.CommandUnknown, nil, wall)
		if _, err := s.AddCommandEvent(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListCommandEvents("")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
}

func TestCommandTelemetryThousandRowsStayDBOnlyAndRingBySequence(t *testing.T) {
	base := t.TempDir()
	s, err := Open(context.Background(), Options{Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira", WorktreeID: "main", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, MaxCommandEvents: 1000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for i := 0; i < 1001; i++ {
		input := commandInput("true", domain.CommandKeyProgram, domain.CommandExited, commandPtr(0), commandPtr(1))
		if _, err := s.AddCommandEvent(context.Background(), input); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	rows, err := s.ListCommandEvents("")
	if err != nil || len(rows) != 1000 || rows[0].ID != "CMD-1001" || rows[len(rows)-1].ID != "CMD-2" {
		t.Fatalf("ring first/last=%v/%v len=%d err=%v", rows[0].ID, rows[len(rows)-1].ID, len(rows), err)
	}
	var journal int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=?`, s.projectID).Scan(&journal); err != nil || journal != 0 {
		t.Fatalf("journal=%d err=%v", journal, err)
	}
	if _, err := os.Stat(filepath.Join(base, ".aira")); !os.IsNotExist(err) {
		t.Fatalf("DB-only telemetry wrote project files: %v", err)
	}
}

func TestCommandRetentionDefaultsAndValidation(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	if s.maxCommandEvents != 50000 || s.maxCommandAgeDays != 0 {
		t.Fatalf("defaults=%d/%d", s.maxCommandEvents, s.maxCommandAgeDays)
	}
	base := t.TempDir()
	_, err := Open(context.Background(), Options{Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state.db"), RegistryPath: filepath.Join(base, "registry.jsonl"), ProjectID: "p", WorktreeID: "w", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, MaxCommandAgeDays: -1})
	if err == nil {
		t.Fatal("negative command retention was accepted")
	}
}

var _ = sql.ErrNoRows
