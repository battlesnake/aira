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
// fails the launch CLOSED when the job was NOT admitted, instead of running it
// UNGOVERNED and exiting 0 (the defect where "governed, slice was empty" and
// "not governed at all" shared an exit code). It is OPT-IN and default-off: a
// launch that does not set the flag is unaffected, which is what keeps ordinary
// launches unbroken.
//
// The gate keys on the raw admission STATE, refusing anything that is not
// "immediate" or "waited" (the two admitted states). It therefore refuses BOTH
// "unevaluated" (slice unreadable / ci-shim daemon-down / a daemon unevaluated
// grant) AND "timeout" -- the flock fallback's "waited the whole budget, got no
// admission, launching anyway" outcome, which is an ungoverned launch a naive
// `== unevaluated` key let through (Fable build-review P1). It ALLOWS a
// flock-fallback "immediate"/"waited": that is a real free-memory check holding
// a real lock, so a daemon-restart on a real slice still launches under the
// flag. (Note: on the REAL path a daemon-down job does NOT resolve to
// unevaluated -- Runner.admit falls to the flock path and returns
// immediate/waited -- so this does not break daemon-restart windows; only a
// ci-shim daemon-down job, which has no ledger, resolves to unevaluated and is
// refused, which is the ticket's own scenario.)

// requireAdmissionRealDeps builds the real-path confine deps and PINS the mode
// to ConfineModeReal, so these tests exercise the real path deterministically
// rather than reading the machine's install record (which would silently run
// the ci-shim path on a shim-installed CI box -- Fable build-review P3).
func requireAdmissionRealDeps() confineDeps {
	deps := confineUnitDeps(&confineFakeScope{})
	deps.resolveMode = func() string { return ConfineModeReal }
	return deps
}

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
	err := confineRunTouchingMarker(t, requireAdmissionRealDeps(), true, "unevaluated", marker)
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
// Both admitted states are pinned: "immediate", and "waited" (the flock-fallback
// grant that must NOT be refused, else the flag is unusable during a daemon
// restart on a real slice).
func TestRequireAdmissionAllowsRealPathWhenAdmitted(t *testing.T) {
	for _, state := range []string{"immediate", "waited"} {
		marker := filepath.Join(t.TempDir(), "launched")
		if err := confineRunTouchingMarker(t, requireAdmissionRealDeps(), true, state, marker); err != nil {
			t.Fatalf("an admitted (%s) job with --require-admission must launch, got: %v", state, err)
		}
		if !markerExists(marker) {
			t.Fatalf("an admitted (%s) job with --require-admission did not launch (marker absent)", state)
		}
	}
}

// Real path, flag SET, admission TIMEOUT -> refuse. The flock fallback's
// "waited the whole budget, admitted nothing, would launch anyway" is exactly an
// ungoverned launch; a naive `== unevaluated` gate let it through (Fable P1).
func TestRequireAdmissionRefusesRealPathWhenTimedOut(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	err := confineRunTouchingMarker(t, requireAdmissionRealDeps(), true, "timeout", marker)
	if err == nil {
		t.Fatal("a flock-timeout admission must be refused under --require-admission (would run ungoverned)")
	}
	if !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout refusal does not name the cause: %v", err)
	}
	if markerExists(marker) {
		t.Fatal("job LAUNCHED on a timeout despite --require-admission")
	}
}

// Default-off must STILL launch on a timeout (opt-in preserved on that state too).
func TestRequireAdmissionAbsentRealPathStillLaunchesTimeout(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	if err := confineRunTouchingMarker(t, requireAdmissionRealDeps(), false, "timeout", marker); err != nil {
		t.Fatalf("without --require-admission a timeout launch must still run, got: %v", err)
	}
	if !markerExists(marker) {
		t.Fatal("default (no flag) launch did not run under timeout admission")
	}
}

// Real path, flag ABSENT, admission unevaluated -> launches (today's default is
// preserved; the flag is opt-in). This is the assertion that pins default-off:
// a regression to always-fail-closed would break every ordinary launch during a
// daemon-restart window.
func TestRequireAdmissionAbsentRealPathStillLaunchesUnevaluated(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	if err := confineRunTouchingMarker(t, requireAdmissionRealDeps(), false, "unevaluated", marker); err != nil {
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
