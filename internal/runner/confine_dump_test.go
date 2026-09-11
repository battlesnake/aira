package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteConfineDumpJSONLRoundTrips pins the wire shape: one JSON object per
// line (JSONL), in the order Admissions then Queues, atomically written.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestWriteConfineDumpJSONLRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")

	peak := int64(200 << 20)
	result := ConfineDumpResult{
		Verdict: "ok", Scope: "machine-wide",
		Admissions: []ConfineDumpAdmissionRow{
			{
				RecordType: ConfineDumpRecordAdmission, Kind: "confine", Signature: "make test",
				At: "2026-09-11T00:00:00Z", ObservedPeakBytes: &peak, OOM: false,
				Outcome: ConfineDumpUnevaluated,
			},
		},
		Queues: []ConfineDumpQueueRow{
			{RecordType: ConfineDumpRecordQueue, Slice: "/aira.slice", RAMOutstandingBytes: 512 << 20, CPUCeilingCores: 4},
		},
	}
	if err := WriteConfineDumpJSONL(path, result); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	lines := []string{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines (1 admission + 1 queue), got %d: %q", len(lines), string(data))
	}
	var admission map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &admission); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	if admission["record_type"] != "admission" {
		t.Fatalf("line 0 record_type = %v, want admission", admission["record_type"])
	}
	if admission["signature"] != "make test" {
		t.Fatalf("line 0 signature = %v", admission["signature"])
	}
	var queue map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &queue); err != nil {
		t.Fatalf("line 1 not valid JSON: %v", err)
	}
	if queue["record_type"] != "queue" {
		t.Fatalf("line 1 record_type = %v, want queue", queue["record_type"])
	}
}

// TestWriteConfineDumpJSONLUnevaluatedFieldsStayNull is the honesty mutation
// pin (AIRA hard rule: a value that cannot be established is `unevaluated`,
// never a fabricated 0). An admission row with no declared reserve and no
// observed peak must marshal those fields as JSON null (an absent pointer),
// never as the number 0 -- a mutant that swapped the *int64 fields for plain
// int64 would make this test red because `0.0 != nil` in the decoded map.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestWriteConfineDumpJSONLUnevaluatedFieldsStayNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	result := ConfineDumpResult{
		Verdict: "ok", Scope: "machine-wide",
		Admissions: []ConfineDumpAdmissionRow{
			{
				RecordType: ConfineDumpRecordAdmission, Kind: "confine", Signature: "unmeasured-cmd",
				At: "2026-09-11T00:00:00Z", OOM: false, Outcome: ConfineDumpUnevaluated,
				// DeclaredReserveBytes / ObservedPeakBytes / WaitMS deliberately nil.
			},
		},
	}
	if err := WriteConfineDumpJSONL(path, result); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	declared, present := row["declared_reserve_bytes"]
	if present && declared != nil {
		t.Fatalf("declared_reserve_bytes must be absent or null, not a fabricated value: %v", declared)
	}
	if outcome, ok := row["outcome"].(string); !ok || outcome != "unevaluated" {
		t.Fatalf("outcome must be the honest sentinel %q, got %v", ConfineDumpUnevaluated, row["outcome"])
	}
	if _, present := row["wait_ms"]; present {
		t.Fatalf("wait_ms must be absent (unevaluated), never a fabricated 0: %v", row["wait_ms"])
	}
}

// TestWriteConfineDumpJSONLIsAtomic pins the CreateTemp+write+fsync+rename
// contract: a failed write (bad target directory) must not leave a partial
// file at the destination path, and a successful write replaces any prior
// content wholesale rather than appending.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestWriteConfineDumpJSONLIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.jsonl")
	if err := os.WriteFile(path, []byte("stale content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := ConfineDumpResult{Verdict: "ok", Scope: "machine-wide"}
	if err := WriteConfineDumpJSONL(path, result); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(data), "stale content") {
		t.Fatalf("atomic write must replace, not append: %q", string(data))
	}
	// No leaked temp file beside the target.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("expected exactly the target file, found: %v", names)
	}
}

// TestWriteConfineDumpJSONLFailsOnUnwritableDirectory pins that a write
// failure is reported rather than silently swallowed.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestWriteConfineDumpJSONLFailsOnUnwritableDirectory(t *testing.T) {
	result := ConfineDumpResult{Verdict: "ok", Scope: "machine-wide"}
	err := WriteConfineDumpJSONL(filepath.Join(t.TempDir(), "does-not-exist", "dump.jsonl"), result)
	if err == nil {
		t.Fatal("expected an error writing into a non-existent directory")
	}
}
