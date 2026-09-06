package runner

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// verifies: AIRA-136 — the deadline multiplexer. These tests are hermetic: they
// drive startDeadlineSource directly with an injected CPU reader and touch no
// cgroup, no process and no ledger.
//
// The pervasive wrong implementation they exist to defeat is "a CPU timeout that
// is really a second wall-clock timer". Each test that cannot pass against it
// says so.

// deadlineTestInterval is short so these tests are quick, and is passed
// explicitly rather than by retuning the production constant.
const deadlineTestInterval = 2 * time.Millisecond

// scriptedCPUReader returns a queued sequence of samples, repeating the last one
// forever. It is safe for concurrent use because the source goroutine is the
// only caller but the test goroutine reads the call count.
type scriptedCPUReader struct {
	mu      sync.Mutex
	samples []struct {
		used time.Duration
		ok   bool
	}
	calls int
}

func (s *scriptedCPUReader) push(used time.Duration, ok bool) *scriptedCPUReader {
	s.samples = append(s.samples, struct {
		used time.Duration
		ok   bool
	}{used, ok})
	return s
}

func (s *scriptedCPUReader) read(string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return 0, false
	}
	index := s.calls
	if index >= len(s.samples) {
		index = len(s.samples) - 1
	}
	s.calls++
	return s.samples[index].used, s.samples[index].ok
}

func (s *scriptedCPUReader) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// receiveFire waits for one fire against a generous liveness backstop.
func receiveFire(t *testing.T, src *deadlineSource) deadlineFire {
	t.Helper()
	select {
	case fire := <-src.C:
		return fire
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("the deadline source never fired")
		return deadlineFire{}
	}
}

// refuseFire asserts no fire arrives within a NEGATIVE wait. Per testdeadline's
// contract a negative wait is deliberately unscaled: contention delays the thing
// under test and the timer alike.
func refuseFire(t *testing.T, src *deadlineSource, window time.Duration, format string, args ...any) {
	t.Helper()
	select {
	case fire := <-src.C:
		t.Helper()
		t.Fatalf(format+" (fired %+v)", append(append([]any(nil), args...), fire)...)
	case <-time.After(window):
	}
}

func TestAIRA136DeadlineSourceIsNilWithoutABound(t *testing.T) {
	t.Parallel()
	if src := startDeadlineSource(deadlineConfig{ReadCPU: func(string) (time.Duration, bool) { return 0, true }}); src != nil {
		t.Fatal("a source was started for a run with no deadline of either kind")
	}
}

// TestAIRA136DeadlineSourceEmitsAtMostOneFire kills the two-triggers design
// directly: both bounds are set and both are immediately breachable, and the
// source must still emit exactly one value and then be dead.
func TestAIRA136DeadlineSourceEmitsAtMostOneFire(t *testing.T) {
	t.Parallel()
	reader := (&scriptedCPUReader{}).push(time.Hour, true)
	src := startDeadlineSource(deadlineConfig{
		Wall: time.Nanosecond, CPU: time.Millisecond, CPUBaseOK: true,
		Interval: deadlineTestInterval, ReadCPU: reader.read,
	})
	if src == nil {
		t.Fatal("no source was started for a run with both bounds")
	}
	first := receiveFire(t, src)
	if first.Code != "E_RUN_TIMEOUT" && first.Code != "E_RUN_CPU_TIMEOUT" {
		t.Fatalf("fire carried no deadline code: %+v", first)
	}
	refuseFire(t, src, 100*time.Millisecond, "the source emitted a SECOND fire after %+v", first)
	src.halt()
}

func TestAIRA136DeadlineSourceCPUFireCarriesCPUActorAndCode(t *testing.T) {
	t.Parallel()
	reader := (&scriptedCPUReader{}).push(0, true).push(5*time.Second, true)
	src := startDeadlineSource(deadlineConfig{
		CPU: time.Second, CPUBaseOK: true,
		Interval: deadlineTestInterval, ReadCPU: reader.read,
	})
	fire := receiveFire(t, src)
	if fire.Actor != "run-cpu-timeout" || fire.Code != "E_RUN_CPU_TIMEOUT" {
		t.Fatalf("CPU fire mis-attributed: %+v", fire)
	}
	if fire.Budget != time.Second || fire.Observed < fire.Budget {
		t.Fatalf("CPU fire did not carry its own evidence: %+v", fire)
	}
	src.halt()
}

// TestAIRA136DeadlineSourceWallFireCarriesWallActorAndCode is the mirror, so a
// swapped actor/code pair cannot pass both tests.
func TestAIRA136DeadlineSourceWallFireCarriesWallActorAndCode(t *testing.T) {
	t.Parallel()
	src := startDeadlineSource(deadlineConfig{Wall: time.Nanosecond})
	fire := receiveFire(t, src)
	if fire.Actor != "run-timeout" || fire.Code != "E_RUN_TIMEOUT" {
		t.Fatalf("wall fire mis-attributed: %+v", fire)
	}
	if fire.Budget != time.Nanosecond || fire.Observed != 0 {
		t.Fatalf("the wall fire asserted a CPU observation it does not have: %+v", fire)
	}
	src.halt()
}

// TestAIRA136UnreadableCPUStatNeverFires is the non-porous unevaluated test: the
// reader returns a value FAR over the budget, marked unevaluated. An
// implementation that ignores ok fires immediately; the correct one never fires,
// because an unreadable counter is not evidence of anything.
func TestAIRA136UnreadableCPUStatNeverFires(t *testing.T) {
	t.Parallel()
	reader := (&scriptedCPUReader{}).push(time.Hour, false)
	src := startDeadlineSource(deadlineConfig{
		CPU: time.Millisecond, CPUBaseOK: true,
		Interval: deadlineTestInterval, ReadCPU: reader.read,
	})
	refuseFire(t, src, 200*time.Millisecond, "an unreadable cpu.stat was treated as evidence")
	if reader.count() == 0 {
		t.Fatal("vacuous: the sampler never read the counter at all")
	}
	src.halt()
}

// TestAIRA136CPUBudgetIsMeasuredFromItsBaseline fails against an implementation
// that compares the scope's ABSOLUTE counter against the budget: that one fires
// on the very first sample.
func TestAIRA136CPUBudgetIsMeasuredFromItsBaseline(t *testing.T) {
	t.Parallel()
	reader := (&scriptedCPUReader{}).
		push(10*time.Second, true).
		push(10500*time.Millisecond, true).
		push(11*time.Second, true)
	src := startDeadlineSource(deadlineConfig{
		CPU: time.Second, CPUBase: 10 * time.Second, CPUBaseOK: true,
		Interval: deadlineTestInterval, ReadCPU: reader.read,
	})
	fire := receiveFire(t, src)
	if fire.Observed != time.Second {
		t.Fatalf("the fire was decided on the absolute counter, not consumption: %+v", fire)
	}
	if reader.count() < 3 {
		t.Fatalf("fired after %d samples; the two under-budget samples were not honoured", reader.count())
	}
	src.halt()
}

// TestAIRA136LostBaselineAdoptsFirstSampleAndFiresLate pins the safe direction:
// with no pre-start baseline the FIRST successful sample becomes the baseline,
// which undercounts by whatever the child already burned, so the bound fires
// late and never early.
func TestAIRA136LostBaselineAdoptsFirstSampleAndFiresLate(t *testing.T) {
	t.Parallel()
	reader := (&scriptedCPUReader{}).
		push(40*time.Second, true). // adopted as the baseline
		push(40500*time.Millisecond, true).
		push(41*time.Second, true)
	src := startDeadlineSource(deadlineConfig{
		CPU: time.Second, CPUBaseOK: false,
		Interval: deadlineTestInterval, ReadCPU: reader.read,
	})
	fire := receiveFire(t, src)
	if fire.Observed != time.Second {
		t.Fatalf("the adopted baseline was not used: %+v", fire)
	}
	src.halt()
}

// TestAIRA136LostBaselineSkipsUnevaluatedSamplesWhenAdopting proves the adoption
// is of the first ESTABLISHED sample, not of an unevaluated zero — adopting a
// fabricated zero baseline against a scope whose counter is already high would
// fire the bound far too early, which is the one direction this design forbids.
func TestAIRA136LostBaselineSkipsUnevaluatedSamplesWhenAdopting(t *testing.T) {
	t.Parallel()
	reader := (&scriptedCPUReader{}).
		push(0, false).
		push(40*time.Second, true). // the first ESTABLISHED sample, adopted
		push(40*time.Second, true)
	src := startDeadlineSource(deadlineConfig{
		CPU: time.Second, CPUBaseOK: false,
		Interval: deadlineTestInterval, ReadCPU: reader.read,
	})
	refuseFire(t, src, 200*time.Millisecond, "an unevaluated sample was adopted as the baseline")
	src.halt()
}

// TestAIRA136DeadlineSourceHaltJoinsItsGoroutine covers both halt paths: after a
// discarded fire (the goroutine has already returned) and with no fire at all.
// Under -race a sampler still running after halt would be observable.
func TestAIRA136DeadlineSourceHaltJoinsItsGoroutine(t *testing.T) {
	t.Parallel()
	fired := startDeadlineSource(deadlineConfig{Wall: time.Nanosecond})
	_ = receiveFire(t, fired)
	fired.halt()
	select {
	case <-fired.done:
	default:
		t.Fatal("halt returned while the source goroutine was still running")
	}

	reader := (&scriptedCPUReader{}).push(0, true)
	quiet := startDeadlineSource(deadlineConfig{
		Wall: time.Hour, CPU: time.Hour, CPUBaseOK: true,
		Interval: deadlineTestInterval, ReadCPU: reader.read,
	})
	quiet.halt()
	select {
	case <-quiet.done:
	default:
		t.Fatal("halt returned while an unfired source was still sampling")
	}
}

// TestAIRA136ReadCgroupCPUUsedIsUnevaluatedNotZero pins the read primitive's
// honesty in every direction a missing counter can present.
func TestAIRA136ReadCgroupCPUUsedIsUnevaluatedNotZero(t *testing.T) {
	t.Parallel()
	if _, ok := readCgroupCPUUsed(""); ok {
		t.Fatal("an empty scope path was reported as an established measurement")
	}
	if _, ok := readCgroupCPUUsed(t.TempDir()); ok {
		t.Fatal("a missing cpu.stat was reported as an established measurement")
	}
	dir := t.TempDir()
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("usage_usec 500\n")
	if _, ok := readCgroupCPUUsed(dir); ok {
		t.Fatal("a cpu.stat without user_usec/system_usec was reported as established")
	}
	write("usage_usec 500\nuser_usec 300\nsystem_usec 200\n")
	used, ok := readCgroupCPUUsed(dir)
	if !ok || used != 500*time.Microsecond {
		t.Fatalf("readCgroupCPUUsed=%v,%v want 500us,true (user_usec+system_usec)", used, ok)
	}
}
