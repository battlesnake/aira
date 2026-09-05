package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestControlValueRoundTripsAConfineRequestAndPreservesTheRunControlContract is
// the AIRA-22 convergence test. `confine --detach` reuses the very control-file
// primitive `run --detach` uses rather than growing a second one, so this pins
// BOTH halves at once: the new confine payload round-trips, and every property
// the run path already depended on -- 0600, removal on read, unknown-field
// rejection, trailing-data rejection -- survives the generalisation.
//
// verifies: AIRA-22
func TestControlValueRoundTripsAConfineRequestAndPreservesTheRunControlContract(t *testing.T) {
	dir := t.TempDir()
	request := ConfineRequest{
		Slice: "aira.slice", Name: "gate", Owner: "session-a",
		Argv: []string{"/bin/sh", "-c", "true"}, MemoryReserve: 4 << 30,
		MemoryReservePinned: true, DelegateRAM: true, DetachStateDir: dir,
		// The io fields must NOT be serialised: they are process-local and
		// json.Marshal would fail outright on some of them.
		Stdin: strings.NewReader("x"), Stdout: os.Stdout, Stderr: os.Stderr,
	}
	path, err := writeControlValue(dir, "confine-*.ctrl", request)
	if err != nil {
		t.Fatalf("writeControlValue: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat control file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("control file mode = %#o, want 0600: a control file carries the whole request", mode)
	}
	var decoded ConfineRequest
	if err := consumeControlValue(path, &decoded); err != nil {
		t.Fatalf("consumeControlValue: %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("control file still present after consume: %v", statErr)
	}
	if decoded.Slice != request.Slice || decoded.Name != request.Name || decoded.Owner != request.Owner ||
		decoded.MemoryReserve != request.MemoryReserve || !decoded.MemoryReservePinned || !decoded.DelegateRAM ||
		decoded.DetachStateDir != dir || len(decoded.Argv) != 3 || decoded.Argv[2] != "true" {
		t.Fatalf("round trip lost fields: %+v", decoded)
	}
	if decoded.Stdin != nil || decoded.Stdout != nil || decoded.Stderr != nil {
		t.Fatal("io fields must not cross the control file")
	}
}

// verifies: AIRA-22
func TestConsumeControlValueRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "unknown field", payload: `{"slice":"aira.slice","not_a_field":1}` + "\n"},
		{name: "trailing data", payload: `{"slice":"aira.slice"}` + "\n" + `{"slice":"b"}` + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "control")
			if err := os.WriteFile(path, []byte(test.payload), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			var decoded ConfineRequest
			if err := consumeControlValue(path, &decoded); err == nil {
				t.Fatal("malformed control file was accepted")
			}
		})
	}
}
