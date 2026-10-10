package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/store"
)

// seedOversizedHistory records `subjects` distinct confine subjects whose
// signatures are `signatureBytes` long, straight into the daemon's database, so
// that the unpaged dump/budget reply is larger than the 16 MiB wire frame.
func seedOversizedHistory(t *testing.T, subjects, signatureBytes int) []string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.DBPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenDB(paths.DBPath, paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	var signatures []string
	for i := 0; i < subjects; i++ {
		signature := string(rune('a'+i)) + strings.Repeat("x", signatureBytes)
		peak := int64(1<<20 + i)
		if err := db.RecordConfinePeak(context.Background(), store.ResourcePeakObservation{
			Kind: store.ResourcePeakKindConfine, Signature: signature, Peak: &peak, At: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		signatures = append(signatures, signature)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	startCommandDaemon(t, daemon.NewServer(paths))
	return signatures
}

// verifies: AIRA-280 — over a real daemon socket, a history whose unpaged reply
// exceeds the 16 MiB frame is dumped and budgeted COMPLETE through the real argv
// entrypoint. Before this ticket both failed with a bare "E_DAEMON_UNAVAILABLE:
// EOF" (the daemon's oversize write was refused and the connection dropped).
func TestConfineDumpAndBudgetCompleteOnAHistoryLargerThanOneFrame(t *testing.T) {
	const subjects, signatureBytes = 20, 1 << 20 // ~20 MiB of signatures
	signatures := seedOversizedHistory(t, subjects, signatureBytes)

	dumpPath := filepath.Join(t.TempDir(), "dump.jsonl")
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--dump", dumpPath, "--json"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("dump exit=%d stdout=%.300q stderr=%.300q", exit, stdout.String(), stderr.String())
	}
	seen := map[string]bool{}
	for _, row := range readDump(t, dumpPath) {
		if row["record_type"] == runner.ConfineDumpRecordAdmission {
			seen[row["signature"].(string)] = true
		}
	}
	for _, signature := range signatures {
		if !seen[signature] {
			t.Fatalf("the dump is missing a subject (%d of %d present)", len(seen), subjects)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if exit := runWithInput([]string{"confine", "--budget", "--json"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("budget exit=%d stdout=%.300q stderr=%.300q", exit, stdout.String(), stderr.String())
	}
	var envelope struct {
		Data runner.ConfineBudgetResult `json:"data"`
	}
	raw := stdout.Bytes()
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data.Verdict == "" {
		var direct runner.ConfineBudgetResult
		if err2 := json.Unmarshal(raw, &direct); err2 != nil {
			t.Fatalf("budget output is not JSON: %v / %v", err, err2)
		}
		envelope.Data = direct
	}
	if len(envelope.Data.Subjects) != subjects || envelope.Data.Next != nil {
		t.Fatalf("budget subjects = %d (next=%v), want %d", len(envelope.Data.Subjects), envelope.Data.Next, subjects)
	}
}
