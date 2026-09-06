package runner

import (
	"os"
	"path/filepath"
	"time"
)

// AIRA-136. A run can be ended early by a wall-clock deadline or by a
// cumulative CPU-time budget. This file is the single source that multiplexes
// BOTH into one channel that emits at most one value, so Launch keeps exactly
// one deadline branch, exactly one killWithIntent call site, and AIRA-126's
// kill/terminal arbitration remains the sole decision point for both bounds.
//
// The alternative — a second, independent goroutine calling killWithIntent for
// the same run — is precisely the intent-arbitration hazard AIRA-126 exists to
// get right, in a new shape.

// cpuBudgetSampleInterval is the period of the CPU-time budget sampler. It is
// deliberately its OWN constant and not scopeMembershipSampleInterval: the two
// have unrelated costs and unrelated coverage consequences, and coupling them
// would let a future change to either silently retune the other.
//
// Cost is not the binding constraint. The membership sampler walks cgroup.procs
// plus O(scope tree) /proc entries every tick, which is why its own comment
// records ~112% of a core at 2ms for a 31-process tree; this sampler reads one
// fixed-size pseudo-file whatever the tree looks like, on the order of a few
// microseconds, so at 100ms it is ~0.005% of a core.
//
// Overshoot is the binding constraint. Between the last sample under budget and
// the sample that fires, a job can consume up to interval x (cores it is
// actually getting) of CPU-time: 0.1 cpu-s on one core, ~1.6 cpu-s on a 16-way
// box, ~6.4 cpu-s on a 64-way one. 250ms and above start producing double-digit
// cpu-second surprise on a large machine; 50ms halves a term already dominated
// by parallelism while doubling the read rate for no useful resolution.
//
// The overshoot is ALWAYS in the late direction. So is a coarse interval, a lost
// baseline, and an unevaluated sample. Nothing in this design can fire a budget
// a job had not actually reached.
const cpuBudgetSampleInterval = 100 * time.Millisecond

// deadlineFire is the single value a deadline source ever emits. It names the
// bound that fired so the ONE kill site can attribute the kill honestly without
// making a second decision.
type deadlineFire struct {
	Actor    string        // "run-timeout" | "run-cpu-timeout"
	Code     string        // "E_RUN_TIMEOUT" | "E_RUN_CPU_TIMEOUT"
	Budget   time.Duration // the bound that was breached
	Observed time.Duration // CPU-time consumed at the deciding sample; zero for the wall bound
}

// deadlineSource multiplexes the wall-clock deadline and the cumulative
// CPU-time budget into ONE channel. At most one value is ever sent: the
// goroutine returns immediately after a send.
//
// C is buffered 1, so a fire that loses the race to the child's own wait is
// discarded without blocking the goroutine — the same discard today's unread
// timer.C drain performs.
//
// The source holds NO sampled state Launch reads back. The authoritative final
// CPU total is the teardown read Launch already takes; a mid-run sample could
// only ever be a lower bound, which proves nothing a budget check needs — a
// lower bound under the budget does not establish that the bound held, and a
// sample at or over the budget cannot exist without the source having fired.
type deadlineSource struct {
	C    chan deadlineFire
	stop chan struct{}
	done chan struct{}
}

type deadlineConfig struct {
	Wall      time.Duration
	CPU       time.Duration
	CPUBase   time.Duration // cumulative CPU charged to the scope before the child started
	CPUBaseOK bool
	ScopePath string
	Interval  time.Duration
	ReadCPU   func(string) (time.Duration, bool)
}

// startDeadlineSource returns nil when no bound was requested, so Launch's
// no-deadline path stays a bare receive.
func startDeadlineSource(cfg deadlineConfig) *deadlineSource {
	if cfg.Wall <= 0 && cfg.CPU <= 0 {
		return nil
	}
	src := &deadlineSource{
		C:    make(chan deadlineFire, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go func() {
		defer close(src.done)
		var wall <-chan time.Time
		if cfg.Wall > 0 {
			timer := time.NewTimer(cfg.Wall)
			defer timer.Stop()
			wall = timer.C
		}
		var tick <-chan time.Time
		baseline, haveBaseline := cfg.CPUBase, cfg.CPUBaseOK
		if cfg.CPU > 0 {
			interval := cfg.Interval
			if interval <= 0 {
				interval = cpuBudgetSampleInterval
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			tick = ticker.C
		}
		for {
			select {
			case <-src.stop:
				return
			case <-wall:
				src.C <- deadlineFire{Actor: "run-timeout", Code: "E_RUN_TIMEOUT", Budget: cfg.Wall}
				return
			case <-tick:
				used, ok := cfg.ReadCPU(cfg.ScopePath)
				if !ok {
					// UNEVALUATED. An unreadable counter is never treated as
					// zero: fabricating "no CPU used" would silently disable the
					// bound, and fabricating a large value would kill a job on
					// no evidence. Launch records the absence honestly at
					// teardown instead.
					continue
				}
				if !haveBaseline {
					// The pre-start baseline read failed; adopt the first
					// successful sample. This UNDERCOUNTS by whatever the child
					// burned before it, so the bound fires LATE, never early.
					baseline, haveBaseline = used, true
					continue
				}
				consumed := used - baseline
				if !decideCPUBudgetExceeded(consumed, cfg.CPU) {
					continue
				}
				src.C <- deadlineFire{
					Actor: "run-cpu-timeout", Code: "E_RUN_CPU_TIMEOUT",
					Budget: cfg.CPU, Observed: consumed,
				}
				return
			}
		}
	}()
	return src
}

// halt closes the source and JOINS its goroutine, the same shape as
// monitorStop/monitorResult, so no sampler outlives the launch that owns it.
// It is safe after a fire: the goroutine has already returned and closed done.
func (s *deadlineSource) halt() {
	close(s.stop)
	<-s.done
}

// readCgroupCPUUsed returns the cgroup's CUMULATIVE user+system CPU time —
// hierarchical over this cgroup and every descendant, which is exactly the
// quantity a job-wide budget means — and whether that was ESTABLISHED.
//
// A missing file, an unparseable file, or a missing user_usec or system_usec key
// is unevaluated (false), never zero.
//
// It reads ONE file. readCgroupUsage opens four (memory.peak, cpu.stat,
// memory.events, memory.events.local); three are irrelevant to a CPU budget and
// would quadruple the sampler's syscall rate for no evidence. The primitive
// being reused is parseCPUStat.
//
// user_usec + system_usec rather than usage_usec, deliberately: that pair is
// what the confine trailer already prints and what the run record already stores
// as cpu_user/cpu_sys, so the number a kill was decided on and the number in the
// record are the same quantity by construction and an operator can audit one
// against the other. usage_usec can differ from their sum by rounding, which
// would read as an inconsistency.
func readCgroupCPUUsed(scopePath string) (time.Duration, bool) {
	if scopePath == "" {
		return 0, false
	}
	data, err := os.ReadFile(filepath.Join(scopePath, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	user, system := parseCPUStat(data)
	if user == nil || system == nil {
		return 0, false
	}
	return time.Duration(*user+*system) * time.Microsecond, true
}

// readCgroupCPUFn is the injection point, matching the existing
// readProcStatFn / readProcCgroupFn / readBootIDFn convention.
var readCgroupCPUFn = readCgroupCPUUsed
