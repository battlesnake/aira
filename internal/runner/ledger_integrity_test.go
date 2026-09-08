package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/runner/runnertest"
)

// AIRA-172: LedgerIntegrity is the read-only entry point `aira check` grades
// its run-ledger dimension from. Before it existed, the only way any face could
// learn the ledger was corrupt was to fail a verb closed at exit 4.
//
// SAFETY: every ledger here is a synthetic file built inside t.TempDir(). No
// test in this file reads or writes the machine's real common-directory run
// ledger, which is shared live by other sessions.

// verifies: AIRA-172
func TestLedgerIntegrityAcceptsAbsentAndHealthyLedgers(t *testing.T) {
	// A project that has never launched a run has nothing to be inconsistent
	// about. Reporting the absent file as corruption would fail the check
	// dimension on every fresh repository.
	empty := t.TempDir()
	if err := LedgerIntegrity(empty); err != nil {
		t.Fatalf("absent ledger reported as a defect: %v", err)
	}
	// And it must not have CREATED the run directories to find that out: check
	// is a read-only pass over this evidence.
	if _, err := os.Stat(filepath.Join(empty, "aira", "runs")); !os.IsNotExist(err) {
		t.Fatalf("LedgerIntegrity created runner state as a side effect (stat err = %v)", err)
	}

	healthy := t.TempDir()
	runnertest.WriteHealthyLedger(t, healthy)
	if err := LedgerIntegrity(healthy); err != nil {
		t.Fatalf("healthy ledger reported as a defect: %v", err)
	}
}

// verifies: AIRA-172
func TestLedgerIntegrityReportsTheDecodeDefectVerbatim(t *testing.T) {
	common := t.TempDir()
	runnertest.WriteCorruptLedger(t, common)
	err := LedgerIntegrity(common)
	if err == nil {
		t.Fatal("LedgerIntegrity accepted an undecodable record")
	}
	got := err.Error()
	// The check report's dimension grading switches on this code, and the
	// operator's only way to find the bad record is the AIRA-145 detail, so
	// both must survive being returned unwrapped.
	if code, _, _ := strings.Cut(got, ":"); code != "E_JOURNAL_CORRUPT" {
		t.Fatalf("code token = %q: %q", code, got)
	}
	for _, want := range []string{
		"invalid run ledger record",
		fmt.Sprintf("unknown field %q", runnertest.FutureField),
		"record 1 at byte offset",
		filepath.Join(common, "aira", "runs", "ledger.bin"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic lost %q: %q", want, got)
		}
	}
}

// read() enforces per-record framing and decoding; replay() enforces the
// lifecycle. A LedgerIntegrity that stopped at read() would report a green it
// had not established for a ledger whose records each decode but whose run
// lifecycle is impossible.
//
// verifies: AIRA-172
func TestLedgerIntegrityAlsoEnforcesTheReplayInvariants(t *testing.T) {
	common := t.TempDir()
	runnertest.WriteLedger(t, common,
		synthLedgerEvent(ledgerSchema, 1, "terminal", "RUN-1"),
		synthLedgerEvent(ledgerSchema, 2, "running", "RUN-1"),
	)
	// Each record decodes on its own, so read() is happy.
	l := &ledger{root: common, ledger: filepath.Join(common, "aira", "runs", "ledger.bin")}
	if _, err := l.read(); err != nil {
		t.Fatalf("this fixture must be read-clean for the test to mean anything: %v", err)
	}
	err := LedgerIntegrity(common)
	if err == nil {
		t.Fatal("LedgerIntegrity accepted a ledger with a record after a terminal run")
	}
	if code, _, _ := strings.Cut(err.Error(), ":"); code != "E_JOURNAL_CORRUPT" {
		t.Fatalf("code token = %q: %q", code, err.Error())
	}
}

// TestLedgerFramingRecipeIsStable pins runnertest.Frame — the copy of the
// on-disk framing that packages ABOVE the runner build corrupt-ledger fixtures
// with, because frame() is unexported and store/core cannot call it.
//
// Without this pin a change to the real framing would leave those fixtures
// producing a TORN frame rather than a decodable record with a bad field, so
// their tests would keep passing for the wrong reason. Here the drift fails
// once, locally, with a message that names what to fix.
//
// verifies: AIRA-172
func TestLedgerFramingRecipeIsStable(t *testing.T) {
	for _, payload := range append(runnertest.HealthyRecords(), runnertest.CorruptRecords()...) {
		if got, want := frame(payload), runnertest.Frame(payload); string(got) != string(want) {
			t.Fatalf("ledger framing changed: internal/runner/runnertest.Frame must be updated to match\n"+
				"got  %x\nwant %x", got, want)
		}
	}
	// And the fixtures must land where LedgerIntegrity looks.
	common := t.TempDir()
	path, _ := runnertest.WriteLedger(t, common, runnertest.HealthyRecords()...)
	l := &ledger{root: common, ledger: filepath.Join(common, "aira", "runs", "ledger.bin")}
	if path != l.ledger {
		t.Fatalf("runnertest writes %s but the runner reads %s", path, l.ledger)
	}
	if _, err := l.read(); err != nil {
		t.Fatalf("the runner cannot read a runnertest ledger: %v", err)
	}
}
