package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/runner/runnertest"
)

// AIRA-172: the store grades the run-ledger dimension from its own read, so the
// honest report exists for EVERY face — not only the one that happens to hold a
// Runner. These tests drive Store.Check directly, with no runner and no core
// layer, so a repair that lived only in the core check verb would fail here.
//
// SAFETY: every ledger here is a synthetic file built by runnertest inside a
// t.TempDir(). No test in this file reads or writes the machine's real
// common-directory run ledger, which is shared live by other sessions.

func runLedgerTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	common := filepath.Join(base, "common")
	return testStore(t, filepath.Join(base, "main"), common, filepath.Join(base, "state")), common
}

func runLedgerDimension(t *testing.T, s *Store) (grade string, finding CheckFinding, report CheckReport) {
	t.Helper()
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("Check refused the whole verb instead of grading a dimension: %v", err)
	}
	for _, candidate := range report.UnevaluatedFindings {
		if candidate.Subject == "run-ledger" {
			finding = candidate
		}
	}
	return report.Dimensions["run-ledger"], finding, report
}

// verifies: AIRA-172
func TestCheckGradesRunLedgerPassWhenTheLedgerIsAbsentOrHealthy(t *testing.T) {
	// A repository that has never launched a run has nothing to be inconsistent
	// about. If this graded unevaluated, every check on every fresh repository
	// would be demoted and the dimension would carry no information.
	absentStore, _ := runLedgerTestStore(t)
	if grade, _, report := runLedgerDimension(t, absentStore); grade != "pass" {
		t.Fatalf("absent ledger grades run-ledger %q, want pass: %v", grade, report.Dimensions)
	}

	healthyStore, healthyCommon := runLedgerTestStore(t)
	runnertest.WriteHealthyLedger(t, healthyCommon)
	if grade, _, report := runLedgerDimension(t, healthyStore); grade != "pass" {
		t.Fatalf("healthy ledger grades run-ledger %q, want pass: %v", grade, report.Dimensions)
	}
}

// The defect itself, at the layer that owns the dimension: one unparseable
// record must demote run-ledger alone and leave every other dimension exactly
// as a healthy repository grades it.
//
// verifies: AIRA-172
func TestCheckGradesRunLedgerUnevaluatedAndSuppressesNothingElse(t *testing.T) {
	healthyStore, healthyCommon := runLedgerTestStore(t)
	runnertest.WriteHealthyLedger(t, healthyCommon)
	_, _, healthyReport := runLedgerDimension(t, healthyStore)

	corruptStore, corruptCommon := runLedgerTestStore(t)
	ledgerPath, badOffset := runnertest.WriteCorruptLedger(t, corruptCommon)
	grade, finding, corruptReport := runLedgerDimension(t, corruptStore)

	if grade != "unevaluated" {
		t.Fatalf("corrupt ledger grades run-ledger %q, want unevaluated: %v", grade, corruptReport.Dimensions)
	}
	if !corruptReport.Unevaluated {
		t.Fatalf("report does not carry the unevaluated flag: %#v", corruptReport)
	}
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

	// The reason must name WHICH record: the AIRA-145 diagnostic is the whole
	// reason a degraded report beats a refusal here.
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

// run-ledger must be a dimension finaliseDimensions knows about, so that a
// future refactor which drops the checker reports "no checker established this
// dimension" rather than letting the dimension vanish from the report.
//
// verifies: AIRA-172
func TestRunLedgerIsOneOfTheCanonicalCheckDimensions(t *testing.T) {
	var found bool
	for _, dimension := range checkDimensions {
		if dimension == "run-ledger" {
			found = true
		}
	}
	if !found {
		t.Fatalf("run-ledger is not in checkDimensions: %v", checkDimensions)
	}
	s, _ := runLedgerTestStore(t)
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, reported := report.Dimensions["run-ledger"]; !reported {
		t.Fatalf("run-ledger is absent from a graded report: %v", report.Dimensions)
	}
}

// MarkUnevaluated is the exported primitive the core check verb uses to demote
// a dimension its own evidence contradicted. It must behave exactly like the
// internal one: write the dimension independently of the finding (which dedupes
// on Code+Subject), keep an established FAIL, and never panic on a report whose
// map has not been made.
//
// verifies: AIRA-172
func TestMarkUnevaluatedMatchesTheInternalPrimitive(t *testing.T) {
	corrupt := CheckFinding{Code: "E_JOURNAL_CORRUPT", Subject: "run-ledger", Message: "record 1"}

	// A report that already recorded the identical finding: the dedupe must not
	// leave the dimension ungraded.
	report := newCheckReport()
	report.MarkUnevaluated("run-ledger", corrupt)
	report.Dimensions["run-ledger"] = "pass" // simulate a later establish
	report.MarkUnevaluated("run-ledger", corrupt)
	if report.Dimensions["run-ledger"] != "unevaluated" {
		t.Fatalf("a deduped finding left the dimension %q", report.Dimensions["run-ledger"])
	}
	if len(report.UnevaluatedFindings) != 1 {
		t.Fatalf("finding was duplicated: %#v", report.UnevaluatedFindings)
	}
	if !report.Unevaluated {
		t.Fatal("report does not carry the unevaluated flag")
	}

	// An established failure is never laundered into an unevaluated.
	failing := newCheckReport()
	addFinding(&failing, CheckFinding{Code: "E_ID_UNRESOLVED", Subject: "AIRA-1", Kind: "fail"}, "run-ledger")
	failing.MarkUnevaluated("run-ledger", corrupt)
	if failing.Dimensions["run-ledger"] != "fail" {
		t.Fatalf("an established fail was demoted to %q", failing.Dimensions["run-ledger"])
	}

	// A zero report has no Dimensions map; the exported form must make one
	// rather than panic on a nil-map write.
	var zero CheckReport
	zero.MarkUnevaluated("run-ledger", corrupt)
	if zero.Dimensions["run-ledger"] != "unevaluated" {
		t.Fatalf("zero report = %#v", zero)
	}

	// The Kind is set by the primitive, so a caller cannot smuggle a fail or a
	// warning through it.
	forced := newCheckReport()
	forced.MarkUnevaluated("run-ledger", CheckFinding{Code: "E_JOURNAL_CORRUPT", Subject: "run-ledger", Kind: "fail"})
	if forced.Dimensions["run-ledger"] != "unevaluated" || len(forced.Findings) != 0 {
		t.Fatalf("MarkUnevaluated admitted a non-unevaluated finding: %#v", forced)
	}
}
