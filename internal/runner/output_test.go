package runner

import (
	"context"
	"encoding/base64"
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
