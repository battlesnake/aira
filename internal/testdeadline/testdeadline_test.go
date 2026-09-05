package testdeadline

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// TestScaleIsTheRaceMultiplierTimesTheEnvironment pins the two inputs and the one
// clamp. A scale below 1 would shorten every deadline in the suite and manufacture
// exactly the false failures this package removes, so it is refused rather than
// honoured, and so is anything that does not parse.
//
// verifies: AIRA-20
func TestScaleIsTheRaceMultiplierTimesTheEnvironment(t *testing.T) {
	for _, test := range []struct {
		name  string
		raw   string
		unset bool
		want  float64
	}{
		{name: "unset", unset: true, want: raceScale},
		{name: "empty", raw: "", want: raceScale},
		{name: "one", raw: "1", want: raceScale},
		{name: "three", raw: "3", want: 3 * raceScale},
		{name: "fractional above one", raw: "2.5", want: 2.5 * raceScale},
		{name: "below one is refused", raw: "0.5", want: raceScale},
		{name: "zero is refused", raw: "0", want: raceScale},
		{name: "negative is refused", raw: "-4", want: raceScale},
		{name: "unparseable is refused", raw: "soon", want: raceScale},
		// ParseFloat accepts "nan" and NaN < 1 is false, so a bare comparison would
		// let it through; every deadline would then be NaN and time.Duration(NaN) is
		// a large NEGATIVE duration, firing every backstop in the suite instantly.
		{name: "NaN is refused", raw: "nan", want: raceScale},
		{name: "NaN with sign is refused", raw: "-NaN", want: raceScale},
		// +Inf is refused here rather than left to scaleWith's clamp: the clamp catches
		// it for a positive duration, but 0 * +Inf is NaN, and Exceeded applies no floor,
		// so Exceeded(elapsed, 0) at an infinite scale would report every observation as
		// exceeded.
		{name: "positive infinity is refused", raw: "inf", want: raceScale},
		{name: "negative infinity is refused", raw: "-inf", want: raceScale},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := scaleFor(test.raw, !test.unset); got != test.want {
				t.Fatalf("scale(%q, set=%v)=%v, want %v", test.raw, !test.unset, got, test.want)
			}
		})
	}
}

// TestScaleIsReadOnceAndMatchesTheEnvironment checks the exported entry point agrees
// with the pure function above for the environment this process actually has, and
// that repeated calls are stable (the value is cached behind a sync.Once).
//
// verifies: AIRA-20
func TestScaleIsReadOnceAndMatchesTheEnvironment(t *testing.T) {
	first := Scale()
	if first < 1 {
		t.Fatalf("Scale()=%v, want at least 1", first)
	}
	if second := Scale(); second != first {
		t.Fatalf("Scale() is not stable: %v then %v", first, second)
	}
}

// TestWaitFloorsShortBackstopsAndKeepsLongOnes is the property that fixes the
// non-race flakes: a backstop shorter than a scheduling hiccup is raised to
// MinBackstop, while a caller that already asked for longer keeps its own value.
//
// verifies: AIRA-20
func TestWaitFloorsShortBackstopsAndKeepsLongOnes(t *testing.T) {
	// The expectation is computed in float, not by multiplying two Durations: a
	// Duration conversion truncates, so AIRA_TEST_DEADLINE_SCALE=2.5 — the documented
	// way to widen deadlines on a slow runner, and a value this package's own table
	// accepts — would compare against 2× and fail every subtest.
	scaled := func(d time.Duration) time.Duration { return time.Duration(float64(d) * Scale()) }
	for _, test := range []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "zero", in: 0, want: MinBackstop},
		{name: "far below the floor", in: 30 * time.Millisecond, want: MinBackstop},
		{name: "just below the floor", in: MinBackstop - time.Nanosecond, want: MinBackstop},
		{name: "at the floor", in: MinBackstop, want: MinBackstop},
		{name: "above the floor", in: 30 * time.Second, want: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Wait(test.in)
			if got < test.want {
				t.Fatalf("Wait(%v)=%v, want at least %v", test.in, got, test.want)
			}
			if want := scaled(test.want); got < want || got > want+time.Second {
				t.Fatalf("Wait(%v)=%v, want ~%v at scale %v", test.in, got, want, Scale())
			}
		})
	}
}

// TestWaitIsMonotonicAndNeverShortensNegatives guards the arithmetic: a scale can
// only ever lengthen a deadline, and a nonsensical negative input still yields a
// usable backstop rather than a timer that has already fired.
//
// verifies: AIRA-20
func TestWaitIsMonotonicAndNeverShortensNegatives(t *testing.T) {
	if got := Wait(-time.Hour); got < MinBackstop {
		t.Fatalf("Wait(-1h)=%v, want at least the floor %v", got, MinBackstop)
	}
	previous := time.Duration(0)
	for _, d := range []time.Duration{0, time.Second, MinBackstop, time.Minute, time.Hour} {
		got := Wait(d)
		if got < d {
			t.Fatalf("Wait(%v)=%v shortened the caller's deadline", d, got)
		}
		if got < previous {
			t.Fatalf("Wait is not monotonic: Wait(%v)=%v after %v", d, got, previous)
		}
		previous = got
	}
}

// TestExceededScalesTheBudgetWithoutFlooringIt separates Exceeded from Wait. A
// latency assertion's budget is a property under test, so raising it to MinBackstop
// would silently delete the assertion; it scales and nothing more.
//
// verifies: AIRA-20
func TestExceededScalesTheBudgetWithoutFlooringIt(t *testing.T) {
	const budget = 100 * time.Millisecond
	scaledBudget := time.Duration(float64(budget) * Scale())
	if Exceeded(scaledBudget-time.Millisecond, budget) {
		t.Fatal("an observation inside the scaled budget was reported as exceeding it")
	}
	if !Exceeded(scaledBudget+time.Second, budget) {
		t.Fatal("an observation well past the scaled budget was not reported")
	}
	// The floor is deliberately absent: a 100ms budget must not become a 5s one.
	if !Exceeded(MinBackstop, budget) {
		t.Fatalf("Exceeded floored a %v budget up to MinBackstop, deleting the assertion", budget)
	}
}

// TestScaleWithMultipliesAndClampsAtEveryFactor covers the arithmetic the default
// build cannot reach. `make test` runs without -race and without the environment
// override, so the live factor is exactly 1 and scaleWith returns its input
// untouched: a version that dropped the multiplication, or that inverted the overflow
// clamp, would pass every other test in this file. Driving the factor directly is the
// only thing that can fail against those.
//
// verifies: AIRA-20
func TestScaleWithMultipliesAndClampsAtEveryFactor(t *testing.T) {
	for _, test := range []struct {
		name   string
		in     time.Duration
		factor float64
		want   time.Duration
	}{
		{name: "factor one is identity", in: 7 * time.Second, factor: 1, want: 7 * time.Second},
		{name: "factor below one never shortens", in: 7 * time.Second, factor: 0.25, want: 7 * time.Second},
		{name: "integer factor", in: 3 * time.Second, factor: 4, want: 12 * time.Second},
		{name: "fractional factor is not truncated", in: 2 * time.Second, factor: 2.5, want: 5 * time.Second},
		{name: "fractional factor on a fractional result", in: time.Second, factor: 1.5, want: 1500 * time.Millisecond},
		{name: "overflow is clamped, not wrapped", in: time.Duration(1 << 61), factor: 1000, want: maxScaled},
		{name: "infinity is clamped, not wrapped", in: time.Second, factor: math.Inf(1), want: maxScaled},
		// The seam takes an arbitrary factor, so it guards NaN itself rather than relying
		// on scaleFor having filtered it: NaN passes both `<= 1` and the overflow
		// comparison, and time.Duration(NaN) is a large NEGATIVE duration.
		{name: "NaN factor is refused, not turned negative", in: 7 * time.Second, factor: math.NaN(), want: 7 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := scaleWith(test.in, test.factor)
			if got != test.want {
				t.Fatalf("scaleWith(%v, %v)=%v, want %v", test.in, test.factor, got, test.want)
			}
			if got < 0 {
				t.Fatalf("scaleWith(%v, %v)=%v: a negative deadline fires immediately", test.in, test.factor, got)
			}
		})
	}
}

// TestEventuallyReturnsOnTheFirstTrueWithoutPolling proves the fast path: a
// condition that already holds costs no wall-clock time at all, so replacing a fixed
// sleep with Eventually makes the suite faster rather than slower.
//
// verifies: AIRA-20
func TestEventuallyReturnsOnTheFirstTrueWithoutPolling(t *testing.T) {
	// Counting evaluations cannot prove this on its own: the first TICK also
	// evaluates exactly once, and so does the backstop's final look, so `calls == 1`
	// is equally what a version WITHOUT the fast path produces — confirmed by
	// mutation, twice. The separation has to be structural. A one-hour poll interval
	// puts the first tick out of reach, so the only two ways to reach cond are the
	// pre-poll check (immediately) and the backstop arm (30s later); a budget of 5s
	// sits an order of magnitude from both.
	//
	// This is a LATENCY ASSERTION in the package's own sense — the bound is the
	// property — which is why it uses Exceeded and a budget chosen against the
	// alternative, rather than the 2ms PollInterval bound it replaces. That one could
	// not fail against the mutant at all AND flaked on a single preemption: the worst
	// of both directions.
	calls := 0
	started := time.Now()
	eventuallyWithin(t, 30*time.Second, time.Hour, func() bool { calls++; return true }, "unreachable: the condition is always true")
	if calls != 1 {
		t.Fatalf("condition evaluated %d times, want exactly one", calls)
	}
	if elapsed := time.Since(started); Exceeded(elapsed, 5*time.Second) {
		t.Fatalf("an already-true condition waited %v: the pre-poll fast path is gone", elapsed)
	}
}

// TestEventuallyReturnsAsSoonAsTheConditionHolds checks the polling path stops at the
// transition rather than at the budget.
//
// verifies: AIRA-20
func TestEventuallyReturnsAsSoonAsTheConditionHolds(t *testing.T) {
	calls := 0
	Eventually(t, time.Hour, func() bool { calls++; return calls >= 3 }, "unreachable")
	if calls != 3 {
		t.Fatalf("condition evaluated %d times, want three", calls)
	}
}

// TestEventuallyFailsWithTheCallersMessage is the negative direction: a condition
// that never holds must fail, and must fail with the caller's own diagnostic rather
// than a generic timeout, or the report cannot be acted on.
//
// verifies: AIRA-20
func TestEventuallyFailsWithTheCallersMessage(t *testing.T) {
	fake := &recordingTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		eventuallyWithin(fake, 5*time.Millisecond, time.Millisecond, func() bool { return false }, "marker %d never appeared", 7)
	}()
	if !fake.failed {
		t.Fatal("a condition that never holds did not fail the test")
	}
	if fake.message != "marker 7 never appeared" {
		t.Fatalf("failure message=%q, want the caller's formatted message", fake.message)
	}
}

// recordingTB captures one Fatalf instead of ending the real test. Fatalf must not
// return, so it panics; the caller recovers.
type recordingTB struct {
	testing.TB
	failed  bool
	message string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.message = fmt.Sprintf(format, args...)
	panic("testdeadline: fatal")
}
