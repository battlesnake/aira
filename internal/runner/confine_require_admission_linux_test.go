package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifies: AIRA-222 -- --require-admission (ConfineRequest.RequireAdmission)
// fails the launch CLOSED when memory admission could not be evaluated, instead
// of running the job UNGOVERNED and exiting 0 (the defect where "governed, slice
// was empty" and "not governed at all" shared an exit code). It is OPT-IN and
// default-off: a launch that does not set the flag is unaffected, which is what
// keeps a transient daemon-restart window from breaking every confine on the box.
//
// The gate is keyed on the resolved admission=unevaluated facet, matching the
// flag name and the ticket. A NOTE for a reviewer weighing the semantic: this
// refuses a real-path job that has a finite cap (so it IS contained) but whose
// admission was not evaluated -- e.g. a capped job on a box with the daemon
// down. That is deliberate for an opt-in "require admission" flag: a caller that
// asked for a confirmed admission and got none is told so, and the CI use that
// motivated this (aitest worker-admit) needs a reachable daemon anyway, so a
// daemon-down container is not giving that caller what it asked for regardless.

// confineRunTouchingMarker runs a child that creates `marker`, through the
// supplied deps, with admit stubbed to `admitState`. It returns the error from
// confineWithDeps; the caller decides whether the marker should exist.
func confineRunTouchingMarker(t *testing.T, deps confineDeps, requireAdmission bool, admitState, marker string) error {
	t.Helper()
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: admitState, reason: "slice-not-found"}, nil
	}
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	var stderr bytes.Buffer
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "touch " + marker},
		SelfPath: os.Args[0], RequireAdmission: requireAdmission,
		Stderr: &stderr, Stdout: io.Discard,
	}, deps)
	return err
}

func markerExists(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// Real path, flag SET, admission unevaluated -> terminal refusal, child never runs.
func TestRequireAdmissionRefusesRealPathWhenUnevaluated(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	err := confineRunTouchingMarker(t, confineUnitDeps(&confineFakeScope{}), true, "unevaluated", marker)
	if err == nil {
		t.Fatal("expected a terminal refusal, got nil (job would have launched ungoverned)")
	}
	if !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") || !strings.Contains(err.Error(), "require-admission") {
		t.Fatalf("refusal does not name the cause and the flag: %v", err)
	}
	if !strings.Contains(err.Error(), "slice-not-found") {
		t.Fatalf("refusal does not carry the admission reason: %v", err)
	}
	if markerExists(marker) {
		t.Fatal("job LAUNCHED despite the --require-admission refusal (marker was created)")
	}
}

// Real path, flag SET, admission granted -> launches normally (no over-refusal).
func TestRequireAdmissionAllowsRealPathWhenAdmitted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	if err := confineRunTouchingMarker(t, confineUnitDeps(&confineFakeScope{}), true, "immediate", marker); err != nil {
		t.Fatalf("an admitted job with --require-admission must launch, got: %v", err)
	}
	if !markerExists(marker) {
		t.Fatal("an admitted job with --require-admission did not launch (marker absent)")
	}
}

// Real path, flag ABSENT, admission unevaluated -> launches (today's default is
// preserved; the flag is opt-in). This is the assertion that pins default-off:
// a regression to always-fail-closed would break every ordinary launch during a
// daemon-restart window.
func TestRequireAdmissionAbsentRealPathStillLaunchesUnevaluated(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	if err := confineRunTouchingMarker(t, confineUnitDeps(&confineFakeScope{}), false, "unevaluated", marker); err != nil {
		t.Fatalf("without --require-admission an unevaluated launch must still run, got: %v", err)
	}
	if !markerExists(marker) {
		t.Fatal("default (no --require-admission) launch did not run under unevaluated admission")
	}
}

// ci-shim path, flag SET, admission unevaluated -> refusal (the scenario the
// ticket was filed for: a build-time `aira install --ci=shim` leaves no slice,
// so a fresh container runs ungoverned at exit 0).
func TestRequireAdmissionRefusesShimPathWhenUnevaluated(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	err := confineRunTouchingMarker(t, shimUnitDeps(), true, "unevaluated", marker)
	if err == nil {
		t.Fatal("expected a ci-shim refusal, got nil (job would have run ungoverned at exit 0)")
	}
	if !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") || !strings.Contains(err.Error(), "require-admission") {
		t.Fatalf("shim refusal does not name the cause and the flag: %v", err)
	}
	if markerExists(marker) {
		t.Fatal("ci-shim job LAUNCHED despite the --require-admission refusal")
	}
}

// ci-shim path, flag ABSENT -> launches (today's advisory behaviour preserved).
func TestRequireAdmissionAbsentShimPathStillLaunches(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	if err := confineRunTouchingMarker(t, shimUnitDeps(), false, "unevaluated", marker); err != nil {
		t.Fatalf("without --require-admission a ci-shim job must still run, got: %v", err)
	}
	if !markerExists(marker) {
		t.Fatal("default ci-shim launch did not run")
	}
}
