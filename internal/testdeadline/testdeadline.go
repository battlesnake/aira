// Package testdeadline centralises the wall-clock deadlines AIRA's tests wait on.
//
// AIRA's tests run on a loaded, shared machine and under `-race`, where every
// wall-clock interval stretches unpredictably. A test that asserts "this arrives
// within 200ms" is then asserting the scheduler's mood, not the code's behaviour:
// it fails on contention and passes on a quiet box, which is a false fail, and the
// project's honesty discipline treats a check that cannot establish its result as
// worthless rather than as a pass. AIRA-20 records at least five distinct tests in
// internal/runner alone that flaked this way, several without `-race` at all.
//
// The distinction this package draws is the whole point:
//
//   - A LIVENESS BACKSTOP answers "did this ever happen?". Its only job is to turn
//     a hang into a reported failure instead of a stuck suite. Its exact value is
//     not a property under test, so it must be generous: on a passing run the timer
//     never fires and a large value costs nothing. Use Wait, After, or Eventually.
//
//   - A LATENCY ASSERTION answers "was this prompt?", and the bound is a genuine
//     property under test — a long poll that returns an event promptly rather than
//     sitting out its full timeout, say. It still cannot be a bare wall-clock
//     constant on a contended box, so it is scaled. Use Exceeded.
//
//   - A NEGATIVE WAIT answers "did this correctly NOT happen within X?". Contention
//     delays the thing under test and the timer alike, so those do not produce false
//     failures and this package deliberately does not touch them: scaling one only
//     makes the suite slower. They stay as plain time.After.
//
// Scale is AIRA_TEST_DEADLINE_SCALE (default 1) multiplied by a built-in ×4 under
// the race detector, whose instrumentation inflates every interval.
//
// The package is imported only from _test.go files; nothing in production depends on
// it, so the testing package never links into the release binary.
package testdeadline

import (
	"math"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// MinBackstop is the floor applied to every liveness backstop before scaling.
//
// A backstop below this is indistinguishable from a latency assertion on a loaded
// box: the sub-second intervals AIRA's tests were originally written with (30ms,
// 100ms, 200ms) are shorter than a single scheduling hiccup under full-suite CPU
// contention. Raising the floor costs nothing on a passing run — the timer does not
// fire — and only delays the report of a genuine hang.
const MinBackstop = 5 * time.Second

// PollInterval is how often Eventually re-checks its condition. Short enough that a
// condition that becomes true immediately does not add measurable suite time; long
// enough that a tight poll loop does not itself starve the goroutine it is waiting on.
const PollInterval = 2 * time.Millisecond

// ScaleEnv is the environment variable that multiplies every deadline this package
// hands out. Set it above 1 on a slow or heavily loaded runner.
const ScaleEnv = "AIRA_TEST_DEADLINE_SCALE"

var (
	scaleOnce  sync.Once
	scaleValue float64
)

// Scale reports the multiplier applied to every deadline this package hands out.
//
// It is the AIRA_TEST_DEADLINE_SCALE environment value (default 1) times raceScale,
// which is 4 under the race detector and 1 otherwise. A value that does not parse, or
// that is not greater than zero, is ignored rather than silently tightening deadlines
// — a scale below 1 would manufacture exactly the false failures this package exists
// to remove, so it is clamped to 1.
func Scale() float64 {
	scaleOnce.Do(func() {
		raw, ok := os.LookupEnv(ScaleEnv)
		scaleValue = scaleFor(raw, ok)
	})
	return scaleValue
}

// scaleFor is Scale's decision, separated from the environment read so the clamp is
// directly testable.
//
// NaN and +Inf are both refused explicitly rather than left to the comparison or to
// the clamp downstream.
//
// ParseFloat accepts "nan", and NaN < 1 is false, so NaN would pass the clamp; every
// subsequent multiplication would then be NaN, scaleWith's overflow guard would not
// catch it (NaN > anything is also false), and time.Duration(NaN) is a large NEGATIVE
// duration — every backstop in the suite would fire instantly.
//
// +Inf is caught by scaleWith's clamp for a positive duration, but not for a zero one:
// 0 * +Inf is NaN, and Exceeded applies no floor, so Exceeded(elapsed, 0) at an
// infinite scale would report every observation as exceeded. Refusing the input is
// simpler than reasoning about which call sites can reach a zero duration.
func scaleFor(raw string, present bool) float64 {
	if !present || raw == "" {
		return raceScale
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 1 {
		return raceScale
	}
	return raceScale * parsed
}

// Wait returns the liveness backstop to use for an event the test expects to happen:
// the larger of d and MinBackstop, scaled.
//
// Never use it for a negative wait ("must NOT arrive within d") — there, a longer
// deadline only makes the suite slower and the plain time.After is correct.
func Wait(d time.Duration) time.Duration {
	if d < MinBackstop {
		d = MinBackstop
	}
	return scaleWith(d, Scale())
}

// After is time.After(Wait(d)): the drop-in replacement for a bare time.After used as
// the timeout arm of a select that waits for something the test expects to arrive.
func After(d time.Duration) <-chan time.Time {
	return time.After(Wait(d))
}

// Exceeded reports whether an observed elapsed time is over a scaled budget. Use it
// for the rarer latency assertion, where the bound is a property under test and so
// must stay an assertion rather than become a backstop.
//
// Unlike Wait it applies no floor: the caller's budget is meaningful, and raising it
// to MinBackstop would delete the assertion.
func Exceeded(elapsed, budget time.Duration) bool {
	return elapsed > scaleWith(budget, Scale())
}

// Eventually polls cond until it reports true or the scaled backstop for budget
// expires, then fails the test with the formatted message.
//
// This is the preferred replacement for a fixed sleep followed by an assertion: it
// returns as soon as the condition holds, so it is faster than the sleep it replaces
// on a quiet box and correct on a loaded one. cond must be safe to call repeatedly
// from the calling goroutine.
func Eventually(tb testing.TB, budget time.Duration, cond func() bool, format string, args ...any) {
	tb.Helper()
	eventuallyWithin(tb, Wait(budget), PollInterval, cond, format, args...)
}

// eventuallyWithin is Eventually against an already-resolved backstop and poll
// interval, so this package's own tests can exercise the failure path without waiting
// out MinBackstop, and can prove the pre-poll fast path without timing anything.
//
// Both seams exist for testability and neither is worth exporting: a caller outside
// this package has no reason to choose either value.
func eventuallyWithin(tb testing.TB, backstop, pollInterval time.Duration, cond func() bool, format string, args ...any) {
	tb.Helper()
	if cond() {
		return
	}
	// The backstop is a separate timer arm rather than a check after each tick.
	// Checking only after a tick makes the effective deadline max(backstop,
	// pollInterval), so a poll interval longer than the backstop hangs instead of
	// reporting — which is the one thing a liveness backstop exists to prevent, and
	// which this package's own mutation check hit.
	expired := time.NewTimer(backstop)
	defer expired.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if cond() {
				return
			}
		case <-expired.C:
			// One last look before failing: the condition may have become true
			// between the final tick and the deadline.
			if cond() {
				return
			}
			tb.Helper()
			tb.Fatalf(format, args...)
		}
	}
}

// scaleWith multiplies d by factor, guarding the float conversion against overflow of
// the int64 nanosecond representation.
//
// factor is a parameter rather than a call to Scale so this arithmetic is testable at
// factors the default build never uses: in a plain `go test` with no environment
// override the factor is exactly 1, and a scale function that ignored its factor
// entirely — or that inverted the overflow clamp — would pass every test that only
// exercised the live value.
func scaleWith(d time.Duration, factor float64) time.Duration {
	// NaN needs its own guard: `factor <= 1` is false for NaN, and so is the overflow
	// comparison below, so an unguarded NaN would reach time.Duration(NaN) and become
	// a large negative deadline. Scale can no longer produce one, but the seam exists
	// so a caller can pass an arbitrary factor, and refusing nonsense here costs one
	// comparison.
	if math.IsNaN(factor) || factor <= 1 {
		return d
	}
	scaled := float64(d) * factor
	if scaled > float64(maxScaled) {
		return maxScaled
	}
	return time.Duration(scaled)
}

// maxScaled is the ceiling scaleWith clamps to: large enough that no real deadline
// reaches it, small enough to stay clear of the int64 nanosecond overflow that would
// turn a huge deadline into a negative one.
const maxScaled = time.Duration(1 << 62)
