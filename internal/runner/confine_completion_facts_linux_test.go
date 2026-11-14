package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// completionFakeClock is the two-clock seam AIRA-323 adds to confineDeps: a wall
// clock that a test can step independently of the monotonic one.
type completionFakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (clock *completionFakeClock) advance(mono, wall time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.mono += mono
	clock.wall = clock.wall.Add(wall)
}

func (clock *completionFakeClock) wallNow() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.wall
}

func (clock *completionFakeClock) monoNow() time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.mono
}

// completionHarness runs one clean confine launch through the unit harness with
// the given admit, repo identity and clock, and returns the captured reports and
// how many times repoIdentity had been called when admit was entered.
type completionHarness struct {
	clock        *completionFakeClock
	admit        func() admissionResult
	repoIdentity func() (string, bool)
	// Per-phase clock moves; each runs inside the named seam.
	onRepo, onAdmit, onStart, onHandshake, onUsage func()
}

func (harness completionHarness) run(t *testing.T) ([]ConfinePeakReport, int) {
	t.Helper()
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	repoCalls := 0
	repoCallsAtAdmit := -1
	deps.wallNow = harness.clock.wallNow
	deps.monoNow = harness.clock.monoNow
	deps.repoIdentity = func() (string, bool) {
		repoCalls++
		if harness.onRepo != nil {
			harness.onRepo()
		}
		return harness.repoIdentity()
	}
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		repoCallsAtAdmit = repoCalls
		if harness.onAdmit != nil {
			harness.onAdmit()
		}
		return harness.admit(), nil
	}
	start := deps.start
	deps.start = func(command *confineCommand) error {
		if err := start(command); err != nil {
			return err
		}
		if harness.onStart != nil {
			harness.onStart()
		}
		return nil
	}
	deps.readHandshake = func(file *os.File, timeout time.Duration) ([]byte, error) {
		// Runs after deps.start has returned (amendment 1).
		if harness.onHandshake != nil {
			harness.onHandshake()
		}
		return readConfineHandshake(file, timeout)
	}
	deps.readUsage = func(string) cgroupUsage {
		// The teardown usage read, before the report is built (amendment 1).
		if harness.onUsage != nil {
			harness.onUsage()
		}
		return cgroupUsage{PeakRSS: int64ptr(4 << 20), OOMKill: int64ptr(0)}
	}
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	var reports []ConfinePeakReport
	deps.reportPeak = func(_ context.Context, _ ConfineRequest, report ConfinePeakReport) error {
		reports = append(reports, report)
		return nil
	}
	var diagnostics bytes.Buffer
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &diagnostics,
	}, deps); err != nil {
		t.Fatalf("confine: %v (diagnostics=%q)", err, diagnostics.String())
	}
	if repoCalls != 1 {
		t.Fatalf("repoIdentity called %d times, want exactly once", repoCalls)
	}
	return reports, repoCallsAtAdmit
}

func completionDaemonGrant() admissionResult {
	return admissionResult{state: "waited", waitedMS: 17_500, reserve: 32 << 20, basis: "pinned:client", release: &confineCountingCloser{}}
}

// TestConfineCompletionTimingComposite is T4: one scenario in which every phase
// moves BOTH clocks by a distinct amount, so each interval's start and end are
// pinned together on the one captured report.
//
// Phase A (repoIdentity, before admission): mono +7, wall +11.
// Phase B (admit: 12 000 queued, a dropped connection, one 500 ms reconnect gap,
// 17 500 queued again): mono +30 000, wall +29 000; the grant says waited 17 500.
// Phase C (deps.start): mono +2 000, wall steps back one hour.
// Phase D (after deps.start returned: the handshake read): mono +400, wall +13.
// Phase E (the teardown usage read): mono +50, wall +17.
//
// Expected: WaitMS = 30 000 (the whole admission call on the client's clock, not
// the grant's 17 500), WallMS = 2 000 + 400 + 50 = 2 450, StartedAtMS = T0 +
// 29 011 (the wall reading at the admission return, before the step).
//
// verifies: AIRA-323
func TestConfineCompletionTimingComposite(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	clock := &completionFakeClock{wall: t0}
	reports, _ := completionHarness{
		clock:        clock,
		admit:        completionDaemonGrant,
		repoIdentity: func() (string, bool) { return "0123456789abcdef", true },
		onRepo:       func() { clock.advance(7*time.Millisecond, 11*time.Millisecond) },
		onAdmit:      func() { clock.advance(30_000*time.Millisecond, 29_000*time.Millisecond) },
		onStart:      func() { clock.advance(2_000*time.Millisecond, -time.Hour) },
		onHandshake:  func() { clock.advance(400*time.Millisecond, 13*time.Millisecond) },
		onUsage:      func() { clock.advance(50*time.Millisecond, 17*time.Millisecond) },
	}.run(t)
	if len(reports) != 1 {
		t.Fatalf("reports=%d, want 1", len(reports))
	}
	report := reports[0]
	wantStarted := t0.Add(29_011 * time.Millisecond).UnixMilli()
	if report.WaitMS == nil || *report.WaitMS != 30_000 ||
		report.WallMS == nil || *report.WallMS != 2_450 ||
		report.StartedAtMS == nil || *report.StartedAtMS != wantStarted {
		t.Fatalf("WaitMS=%v WallMS=%v StartedAtMS=%v, want 30000 / 2450 / %d",
			derefInt64(report.WaitMS), derefInt64(report.WallMS), derefInt64(report.StartedAtMS), wantStarted)
	}
}

func derefInt64(value *int64) any {
	if value == nil {
		return "<nil>"
	}
	return *value
}

// TestConfineCompletionWaitOnlyForADaemonGrant: WaitMS is set iff deps.admit
// returned a lease (release), whatever the state name.
//
// verifies: AIRA-323
func TestConfineCompletionWaitOnlyForADaemonGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   admissionResult
		wantWait bool
	}{
		{"immediate grant", admissionResult{state: "immediate", release: &confineCountingCloser{}}, true},
		{"daemon unevaluated grant", admissionResult{state: "unevaluated", reason: "slice-unreadable", release: &confineCountingCloser{}}, true},
		{"local no-daemon unevaluated", admissionResult{state: "unevaluated", reason: "no-daemon", waitedMS: 9}, false},
		{"disabled", admissionResult{state: "disabled", waitedMS: 9}, false},
		{"bypassed", admissionResult{state: "bypassed", waitedMS: 9}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &completionFakeClock{wall: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
			result := tc.result
			reports, _ := completionHarness{
				clock:        clock,
				admit:        func() admissionResult { return result },
				repoIdentity: func() (string, bool) { return "none", true },
				onAdmit:      func() { clock.advance(250*time.Millisecond, 250*time.Millisecond) },
			}.run(t)
			if len(reports) != 1 {
				t.Fatalf("reports=%d", len(reports))
			}
			report := reports[0]
			if tc.wantWait {
				if report.WaitMS == nil || *report.WaitMS != 250 {
					t.Fatalf("WaitMS=%v, want 250 for a daemon grant", derefInt64(report.WaitMS))
				}
			} else if report.WaitMS != nil {
				t.Fatalf("WaitMS=%d for a lease-less admission; must be absent", *report.WaitMS)
			}
			if report.WallMS == nil || report.StartedAtMS == nil {
				t.Fatalf("wall/start must be present on every confine report: %+v", report)
			}
		})
	}
}

// TestConfineCompletionRepoIdentity is T6: the identity is taken once, before
// admission, and copied into the report only when established.
//
// verifies: AIRA-323
func TestConfineCompletionRepoIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       string
		ok       bool
		wantRepo string
	}{
		{"established hash", "0123456789abcdef", true, "0123456789abcdef"},
		{"established none", "none", true, "none"},
		// The fake returns a NON-empty id with ok=false: only ok decides.
		{"unevaluated", "0123456789abcdef", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &completionFakeClock{wall: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
			reports, callsAtAdmit := completionHarness{
				clock:        clock,
				admit:        completionDaemonGrant,
				repoIdentity: func() (string, bool) { return tc.id, tc.ok },
			}.run(t)
			if len(reports) != 1 || reports[0].Repo != tc.wantRepo {
				t.Fatalf("reports=%+v, want Repo %q", reports, tc.wantRepo)
			}
			if callsAtAdmit != 1 {
				t.Fatalf("repoIdentity calls seen at admit entry = %d, want 1 (computed before admission)", callsAtAdmit)
			}
		})
	}
}

// TestConfineCompletionProductionSeams is T4b: the three production seams,
// un-faked, return real values in the right units.
//
// verifies: AIRA-323
func TestConfineCompletionProductionSeams(t *testing.T) {
	deps := defaultConfineDeps()
	if deps.wallNow == nil || deps.monoNow == nil || deps.repoIdentity == nil {
		t.Fatal("defaultConfineDeps lacks an AIRA-323 seam")
	}
	t.Run("monoNow advances by a slept interval", func(t *testing.T) {
		before := deps.monoNow()
		time.Sleep(30 * time.Millisecond)
		elapsed := deps.monoNow() - before
		if elapsed < 30*time.Millisecond || elapsed > 10*time.Second {
			t.Fatalf("monoNow advanced %v across a 30ms sleep", elapsed)
		}
	})
	t.Run("wallNow is the wall clock in full millisecond precision with no monotonic reading", func(t *testing.T) {
		// Bracket the reading between two system wall readings. A wall clock
		// truncated to seconds would fall below `before` whenever `before` has a
		// non-zero millisecond part, so such a bracket is waited for.
		var before, got, after int64
		for {
			before = time.Now().UnixMilli()
			if before%1000 == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			got = deps.wallNow().UnixMilli()
			after = time.Now().UnixMilli()
			break
		}
		if got < before || got > after {
			t.Fatalf("wallNow=%d outside [%d, %d]", got, before, after)
		}
		if strings.Contains(deps.wallNow().String(), "m=") {
			t.Fatalf("wallNow carries a monotonic reading: %s", deps.wallNow())
		}
	})
	t.Run("repoIdentity is RepoIdentity of the working directory", func(t *testing.T) {
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root := newRepoIdentityRepo(t, base, "repo")
		dir := filepath.Join(root, "sub")
		t.Chdir(dir)
		id, ok := deps.repoIdentity()
		want := mustRepoIdentity(t, dir)
		if !ok || id != want {
			t.Fatalf("repoIdentity()=(%q, %v), want (%q, true)", id, ok, want)
		}
	})
}
