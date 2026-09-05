package testdeadline

import (
	"fmt"
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
	scale := time.Duration(Scale())
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
			if got < test.want*scale || got > test.want*scale+time.Second {
				t.Fatalf("Wait(%v)=%v, want ~%v at scale %v", test.in, got, test.want*scale, Scale())
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
	scale := time.Duration(Scale())
	const budget = 100 * time.Millisecond
	if Exceeded(budget*scale-time.Millisecond, budget) {
		t.Fatal("an observation inside the scaled budget was reported as exceeding it")
	}
	if !Exceeded(budget*scale+time.Second, budget) {
		t.Fatal("an observation well past the scaled budget was not reported")
	}
	// The floor is deliberately absent: a 100ms budget must not become a 5s one.
	if !Exceeded(MinBackstop, budget) {
		t.Fatalf("Exceeded floored a %v budget up to MinBackstop, deleting the assertion", budget)
	}
}

// TestEventuallyReturnsOnTheFirstTrueWithoutPolling proves the fast path: a
// condition that already holds costs no wall-clock time at all, so replacing a fixed
// sleep with Eventually makes the suite faster rather than slower.
//
// verifies: AIRA-20
func TestEventuallyReturnsOnTheFirstTrueWithoutPolling(t *testing.T) {
	calls := 0
	started := time.Now()
	Eventually(t, time.Hour, func() bool { calls++; return true }, "unreachable")
	if calls != 1 {
		t.Fatalf("condition evaluated %d times, want exactly one", calls)
	}
	if elapsed := time.Since(started); elapsed > PollInterval {
		t.Fatalf("an already-true condition waited %v", elapsed)
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
		eventuallyWithin(fake, 5*time.Millisecond, func() bool { return false }, "marker %d never appeared", 7)
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
