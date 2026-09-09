package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadOutputUsesBinarySafeCursorsAndExplicitTruncation(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	path := filepath.Join(t.TempDir(), "RUN-1.out")
	want := []byte{0x00, 0xff, 0x01, '\n', 0x80, 0x7f}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	exit := 0
	peak, user, sys := int64(4096), int64(12), int64(3)
	appendRunEvent(t, r, "terminal", RunRecord{
		SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusExited,
		ScopeIntegrity: ScopeContained, ExitCode: &exit, CaptureComplete: true,
		TerminalComplete: true, PeakRSS: &peak, CPUUser: &user, CPUSys: &sys,
		OutputRefs: map[string]OutputRef{"out": {Path: path, Bytes: int64(len(want)), State: OutputComplete}},
	})
	first, err := r.ReadOutput(context.Background(), OutputRequest{RunID: "RUN-1", Stream: "out", MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Bytes, want[:3]) || first.Offset != 0 || first.NextOffset != 3 || first.TotalBytes != int64(len(want)) || !first.Truncated || first.Complete {
		t.Fatalf("first chunk=%+v", first)
	}
	if first.PeakRSS == nil || *first.PeakRSS != peak || first.CPUUser == nil || *first.CPUUser != user || first.CPUSys == nil || *first.CPUSys != sys {
		t.Fatalf("metrics did not ride in output chunk: %+v", first)
	}
	second, err := r.ReadOutput(context.Background(), OutputRequest{RunID: "RUN-1", Stream: "out", From: first.NextOffset, MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(append(first.Bytes, second.Bytes...), want) || second.NextOffset != int64(len(want)) || second.Truncated || !second.Complete {
		t.Fatalf("second chunk=%+v", second)
	}
	if decoded, err := base64.StdEncoding.DecodeString(base64.StdEncoding.EncodeToString(first.Bytes)); err != nil || !reflect.DeepEqual(decoded, first.Bytes) {
		t.Fatalf("base64 round trip failed: %v", err)
	}
	tail, err := r.ReadOutput(context.Background(), OutputRequest{RunID: "RUN-1", Stream: "out", Tail: 2})
	if err != nil || !reflect.DeepEqual(tail.Bytes, want[len(want)-2:]) || tail.Offset != int64(len(want)-2) {
		t.Fatalf("tail=%+v err=%v", tail, err)
	}
}

// TestReadOutputAppliesGrepAndReportsIt closes the false-pass direction on
// AIRA-196's OTHER half. The ticket adds --grep to run-log as well as
// confine-log, and cmd/aira's TestRunLogAcceptsGrep proves only the WIRING: that
// the flag parses, reaches Request.Args, and is declared on the descriptor.
// Nothing proved run-log's own read path actually applies it -- deleting the
// `Grep:` field from ReadOutput's capturedReadRequest left the whole suite green
// (verified by mutation), so a caller could have been handed the unfiltered
// window while `filtered: true` claimed otherwise, which is worse than the gap
// the flag was added to close.
//
// It also pins the two honesty properties the shared reader promises here
// specifically: the cursor keeps describing the UNDERLYING FILE so --from paging
// still works over a filtered read, and an unparseable pattern is an argument
// error rather than a filter that silently matches nothing.
//
// verifies: AIRA-196
func TestReadOutputAppliesGrepAndReportsIt(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	path := filepath.Join(t.TempDir(), "RUN-GREP.out")
	content := []byte("PASS pkg/a\nFAIL pkg/b\nPASS pkg/c\nFAIL pkg/d\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	exit := 0
	appendRunEvent(t, r, "terminal", RunRecord{
		SchemaVersion: ledgerSchema, ID: "RUN-GREP", Status: StatusExited,
		ScopeIntegrity: ScopeContained, ExitCode: &exit, CaptureComplete: true,
		TerminalComplete: true,
		OutputRefs: map[string]OutputRef{
			"out": {Path: path, Bytes: int64(len(content)), State: OutputComplete},
		},
	})

	filtered, err := r.ReadOutput(context.Background(), OutputRequest{RunID: "RUN-GREP", Stream: "out", Grep: "^FAIL "})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(filtered.Bytes); got != "FAIL pkg/b\nFAIL pkg/d\n" {
		t.Fatalf("--grep returned %q; run-log did not apply the pattern", got)
	}
	if !filtered.Filtered || filtered.Grep != "^FAIL " {
		t.Fatalf("a filtered read did not say so: filtered=%v grep=%q", filtered.Filtered, filtered.Grep)
	}
	// The cursor describes the FILE, never the filtered body, so paging survives.
	if filtered.Offset != 0 || filtered.NextOffset != int64(len(content)) || filtered.TotalBytes != int64(len(content)) {
		t.Fatalf("--grep moved the cursor: %+v", filtered)
	}
	if filtered.Truncated {
		t.Fatalf("a whole-file filtered read reported truncated: %+v", filtered)
	}

	// An unfiltered read is byte-identical and never claims to be filtered.
	whole, err := r.ReadOutput(context.Background(), OutputRequest{RunID: "RUN-GREP", Stream: "out"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(whole.Bytes, content) || whole.Filtered || whole.Grep != "" {
		t.Fatalf("unfiltered read=%+v", whole)
	}

	// A pattern that matches nothing is an empty successful read, not a fault:
	// "no matching lines" and "I could not read it" are different facts.
	empty, err := r.ReadOutput(context.Background(), OutputRequest{RunID: "RUN-GREP", Stream: "out", Grep: "nothing-matches-this"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Bytes) != 0 || !empty.Filtered || empty.NextOffset != int64(len(content)) {
		t.Fatalf("no-match read=%+v", empty)
	}

	// An unparseable pattern refuses by code rather than filtering everything out.
	if _, err := r.ReadOutput(context.Background(), OutputRequest{RunID: "RUN-GREP", Stream: "out", Grep: "("}); err == nil {
		t.Fatal("an unparseable --grep was accepted")
	} else {
		var launch *LaunchError
		if !errors.As(err, &launch) || launch.Code != "E_RUN_ARGUMENT_INVALID" {
			t.Fatalf("err=%v, want E_RUN_ARGUMENT_INVALID", err)
		}
	}
}
