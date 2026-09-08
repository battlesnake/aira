package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/runner"
	"aira/internal/runner/runnertest"
	"aira/internal/store"
)

// AIRA-172, item 2. A single unparseable run-ledger record used to fail the
// WHOLE check/reconcile verb closed at exit 4, so an operator could reach no
// finding at all: not the ticket-file integrity failures AIRA-171 records, not
// relation integrity, nothing. CLAUDE.md: "A check that cannot establish its
// result reports `unevaluated`, never a fake pass or zero" -- a corrupt ledger
// record establishes nothing about the other dimensions, so it must demote its
// own dimension and leave every other one graded.
//
// SAFETY: every ledger here is a synthetic file built by runnertest inside a
// t.TempDir(). No test in this file reads or writes the machine's real
// common-directory run ledger, which is shared live by other sessions.

func realRunnerAt(t *testing.T, commonDir string) *runner.Runner {
	t.Helper()
	r, err := runner.New(runner.Config{CommonDir: commonDir})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func checkReportFor(t *testing.T, response Response) store.CheckReport {
	t.Helper()
	var report store.CheckReport
	marshalRoundTrip(t, response.Data, &report)
	return report
}

func unevaluatedFinding(report store.CheckReport, subject string) (store.CheckFinding, bool) {
	for _, finding := range report.UnevaluatedFindings {
		if finding.Subject == subject {
			return finding, true
		}
	}
	return store.CheckFinding{}, false
}

// verifies: AIRA-172
func TestCheckGradesEveryOtherDimensionWhenTheRunLedgerIsCorrupt(t *testing.T) {
	// The two stores are built the same way and differ ONLY in their ledger, so
	// comparing their dimension maps proves that nothing but run-ledger was
	// suppressed. Asserting a hand-written list of dimensions instead would pass
	// just as happily if a future dimension were added and silenced.
	healthyStore, healthyBase := coreTestStoreWithRoot(t)
	healthyCommon := filepath.Join(healthyBase, "common")
	runnertest.WriteHealthyLedger(t, healthyCommon)
	healthy := NewWithRunner(healthyStore, realRunnerAt(t, healthyCommon)).
		Do(context.Background(), Request{Verb: "check"})
	if !healthy.OK {
		t.Fatalf("check with a healthy ledger = %#v", healthy)
	}
	healthyReport := checkReportFor(t, healthy)
	if healthyReport.Dimensions["run-ledger"] != "pass" {
		t.Fatalf("a healthy ledger must grade run-ledger pass, not %q: %v",
			healthyReport.Dimensions["run-ledger"], healthyReport.Dimensions)
	}

	corruptStore, corruptBase := coreTestStoreWithRoot(t)
	corruptCommon := filepath.Join(corruptBase, "common")
	ledgerPath, badOffset := runnertest.WriteCorruptLedger(t, corruptCommon)
	corrupt := NewWithRunner(corruptStore, realRunnerAt(t, corruptCommon)).
		Do(context.Background(), Request{Verb: "check"})

	// The headline: the verb answers, rather than refusing at exit 4.
	if !corrupt.OK {
		t.Fatalf("check on a corrupt ledger still fails the whole verb closed: %#v", corrupt)
	}
	if corrupt.Exit == 4 || corrupt.Code == "E_JOURNAL_CORRUPT" {
		t.Fatalf("check on a corrupt ledger = exit %d code %q; want a graded report", corrupt.Exit, corrupt.Code)
	}
	corruptReport := checkReportFor(t, corrupt)
	if corruptReport.Dimensions["run-ledger"] != "unevaluated" {
		t.Fatalf("run-ledger = %q, want unevaluated: %v",
			corruptReport.Dimensions["run-ledger"], corruptReport.Dimensions)
	}

	// Nothing else moved. This is the whole point of the ticket.
	if len(corruptReport.Dimensions) != len(healthyReport.Dimensions) {
		t.Fatalf("dimension SET changed: corrupt=%v healthy=%v", corruptReport.Dimensions, healthyReport.Dimensions)
	}
	for dimension, want := range healthyReport.Dimensions {
		if dimension == "run-ledger" {
			continue
		}
		if got := corruptReport.Dimensions[dimension]; got != want {
			t.Errorf("%s = %q with a corrupt ledger but %q with a healthy one; a corrupt run-ledger record establishes nothing about it",
				dimension, got, want)
		}
	}

	// The reason must name WHICH record, which is the detail AIRA-145 added and
	// the reason a report is worth returning at all here.
	finding, ok := unevaluatedFinding(corruptReport, "run-ledger")
	if !ok {
		t.Fatalf("no unevaluated finding names the run ledger: %#v", corruptReport.UnevaluatedFindings)
	}
	if finding.Code != "E_JOURNAL_CORRUPT" || finding.Kind != "unevaluated" {
		t.Fatalf("run-ledger finding = %#v, want code E_JOURNAL_CORRUPT kind unevaluated", finding)
	}
	for _, want := range []string{
		"invalid run ledger record",
		fmt.Sprintf("unknown field %q", runnertest.FutureField),
		fmt.Sprintf("record 1 at byte offset %d", badOffset),
		ledgerPath,
	} {
		if !strings.Contains(finding.Message, want) {
			t.Errorf("run-ledger finding does not carry %q: %q", want, finding.Message)
		}
	}
}

// A corrupt ledger must not launder an established FAILURE into a merely
// unevaluated report either: the degraded verb has to keep reporting the
// findings it did establish, with a fail verdict and exit 1.
//
// verifies: AIRA-172
func TestCheckStillReportsEstablishedFailuresWhenTheRunLedgerIsCorrupt(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	common := filepath.Join(base, "common")
	runnertest.WriteCorruptLedger(t, common)
	// An allocation with no materialised ticket file is an established
	// allocated-id-file failure.
	if _, err := s.AllocateID(context.Background(), "AIRA"); err != nil {
		t.Fatal(err)
	}

	response := NewWithRunner(s, realRunnerAt(t, common)).Do(context.Background(), Request{Verb: "check"})
	if !response.OK || response.Exit != 1 {
		t.Fatalf("check = OK %v exit %d, want a fail verdict at exit 1: %#v", response.OK, response.Exit, response)
	}
	report := checkReportFor(t, response)
	if report.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", report.Verdict)
	}
	if report.Dimensions["allocated-id-file"] != "fail" {
		t.Fatalf("allocated-id-file = %q, want fail: %v", report.Dimensions["allocated-id-file"], report.Dimensions)
	}
	if report.Dimensions["run-ledger"] != "unevaluated" {
		t.Fatalf("run-ledger = %q, want unevaluated: %v", report.Dimensions["run-ledger"], report.Dimensions)
	}
	var found bool
	for _, finding := range report.Findings {
		if finding.Code == "E_ID_UNRESOLVED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the established failure was dropped: %#v", report.Findings)
	}
}

// reconcile has the same blast radius and the same repair: the STORE half has
// already reconciled by the time the run half refuses, so the verb reports the
// run half unevaluated instead of discarding durable work at exit 4. The
// runner's own refusal to APPEND to a corrupt ledger is unchanged.
//
// verifies: AIRA-172
func TestReconcileReportsTheRunHalfUnevaluatedWhenTheRunLedgerIsCorrupt(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	common := filepath.Join(base, "common")
	ledgerPath, badOffset := runnertest.WriteCorruptLedger(t, common)

	response := NewWithRunner(s, realRunnerAt(t, common)).Do(context.Background(), Request{Verb: "reconcile"})
	if !response.OK || response.Exit != 3 {
		t.Fatalf("reconcile = OK %v exit %d, want an unevaluated result at exit 3: %#v", response.OK, response.Exit, response)
	}
	data, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("reconcile data type = %T", response.Data)
	}
	if data["reconciled"] != true {
		t.Fatalf("reconcile dropped the store half it had already done: %#v", data)
	}
	unevaluated, ok := data["runs_unevaluated"].(map[string]any)
	if !ok {
		t.Fatalf("reconcile does not report the run half as unevaluated: %#v", data)
	}
	if unevaluated["code"] != "E_JOURNAL_CORRUPT" {
		t.Fatalf("runs_unevaluated code = %v, want E_JOURNAL_CORRUPT", unevaluated["code"])
	}
	message, _ := unevaluated["message"].(string)
	for _, want := range []string{
		fmt.Sprintf("unknown field %q", runnertest.FutureField),
		fmt.Sprintf("record 1 at byte offset %d", badOffset),
		ledgerPath,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("runs_unevaluated does not carry %q: %q", want, message)
		}
	}
	// The ledger is a durable audit file. Degrading the REPORT must not have
	// degraded the refusal to write: the corrupt record is still there, byte
	// for byte.
	after, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), runnertest.FutureField) {
		t.Fatalf("reconcile rewrote or truncated the corrupt ledger")
	}
}

// The guard against the one way this repair could fabricate a green: if the
// store's own read finds the ledger healthy but the runner's reconcile then
// refuses it — a ledger corrupted between the two reads, or a face whose
// runner and store disagree about the common directory — the dimension must
// still be demoted rather than left claiming the pass store.Check recorded.
//
// verifies: AIRA-172
func TestCheckDemotesRunLedgerWhenOnlyTheReconcileSeesCorruption(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	common := filepath.Join(base, "common")
	runnertest.WriteHealthyLedger(t, common) // store.Check will grade run-ledger pass

	response := NewWithRunner(s, &journalCorruptRunner{}).Do(context.Background(), Request{Verb: "check"})
	if !response.OK {
		t.Fatalf("check = %#v, want a graded report", response)
	}
	report := checkReportFor(t, response)
	if report.Dimensions["run-ledger"] != "unevaluated" {
		t.Fatalf("run-ledger = %q; a reconcile that saw corruption must demote the pass store.Check recorded: %v",
			report.Dimensions["run-ledger"], report.Dimensions)
	}
	finding, ok := unevaluatedFinding(report, "run-ledger")
	if !ok || finding.Code != "E_JOURNAL_CORRUPT" {
		t.Fatalf("run-ledger finding = %#v, ok=%v", finding, ok)
	}
	if !strings.Contains(finding.Message, "seeded reconcile corruption") {
		t.Fatalf("the reconcile's own reason was lost: %q", finding.Message)
	}
}

// Only E_JOURNAL_CORRUPT is degraded. Every other reconcile failure keeps
// today's fail-closed behaviour rather than being quietly widened, so a
// reconcile error that says nothing about the ledger still refuses.
//
// verifies: AIRA-172
func TestCheckStillFailsClosedOnANonJournalReconcileError(t *testing.T) {
	s, base := coreTestStoreWithRoot(t)
	runnertest.WriteHealthyLedger(t, filepath.Join(base, "common"))
	response := NewWithRunner(s, &failingRunner{err: fmt.Errorf("E_RUN_SCOPE_UNAVAILABLE: cgroup is gone")}).
		Do(context.Background(), Request{Verb: "check"})
	if response.OK {
		t.Fatalf("a non-ledger reconcile failure must still refuse: %#v", response)
	}
	if response.Code != "E_RUN_SCOPE_UNAVAILABLE" {
		t.Fatalf("code = %q, want E_RUN_SCOPE_UNAVAILABLE: %#v", response.Code, response)
	}
}

type failingRunner struct{ err error }

func (*failingRunner) Launch(context.Context, runner.Request) (*runner.RunRecord, error) {
	return nil, nil
}
func (*failingRunner) Kill(context.Context, string, bool) (*runner.RunRecord, error) {
	return nil, nil
}
func (*failingRunner) Get(string) (*runner.RunRecord, error) { return nil, nil }
func (*failingRunner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return nil, nil
}
func (r *failingRunner) Reconcile(context.Context) ([]runner.RunRecord, error) { return nil, r.err }

type journalCorruptRunner struct{ failingRunner }

func (*journalCorruptRunner) Reconcile(context.Context) ([]runner.RunRecord, error) {
	return nil, fmt.Errorf("E_JOURNAL_CORRUPT: invalid run ledger record: seeded reconcile corruption")
}
