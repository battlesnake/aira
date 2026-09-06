//go:build linux

package daemon

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"
)

// AIRA-103 real-cgroup proof.
//
// EVERY fixture here uses cgrouptest.IsolatedScopeParent, a fresh os.MkdirTemp
// cgroup whose name is asserted not to collide with the .aira-CONFINE- prefix
// every production scan enumerates, torn down with cgroup.kill in t.Cleanup.
// aira.slice itself is NEVER touched: these tests run alongside dozens of live
// confine jobs on a shared machine and must not be able to disturb one.

const (
	ceilingFixtureCap   = int64(2 << 30)
	ceilingFixtureTouch = int64(600 << 20)
)

// TestSliceCeilingAllocHelper is the re-exec'd fixture process (the
// AIRA_WATCHDOG_PROC_HELPER pattern). It touches every page it allocates, so the
// memory is genuinely resident and genuinely non-reclaimable, then reports
// "ready" and waits on stdin for further instructions:
//
//	"anon\n"  allocate and touch another chunk of anonymous memory
//	"file\n"  write and re-read a file inside the cgroup, so memory.current
//	          rises as PAGE CACHE rather than anon
func TestSliceCeilingAllocHelper(t *testing.T) {
	if os.Getenv("AIRA_SLICE_CEILING_HELPER") != "1" {
		return
	}
	size, err := strconv.ParseInt(os.Getenv("AIRA_SLICE_CEILING_HELPER_BYTES"), 10, 64)
	if err != nil || size <= 0 {
		os.Exit(2)
	}
	hold := make([][]byte, 0, 4)
	touch := func(n int64) {
		block := make([]byte, n)
		for i := int64(0); i < n; i += 4096 {
			block[i] = 1
		}
		hold = append(hold, block)
	}
	touch(size)
	dir := os.Getenv("AIRA_SLICE_CEILING_HELPER_DIR")
	_, _ = os.Stdout.WriteString("ready\n")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "anon":
			touch(size)
		case "file":
			// CHUNKED on purpose. A whole-file buffer would allocate `size`
			// bytes of ANONYMOUS memory, so the cgroup would grow by anon plus
			// cache and the case would prove nothing about the file LRU. The
			// 1 MiB buffer is reused, so essentially all of the growth is cache.
			path := filepath.Join(dir, "cache-"+strconv.Itoa(len(hold)))
			chunk := make([]byte, 1<<20)
			if file, err := os.Create(path); err == nil {
				for written := int64(0); written < size; written += int64(len(chunk)) {
					if _, err := file.Write(chunk); err != nil {
						break
					}
				}
				_ = file.Close()
			}
			if file, err := os.Open(path); err == nil {
				for {
					if _, err := file.Read(chunk); err != nil {
						break
					}
				}
				_ = file.Close()
			}
		default:
			os.Exit(0)
		}
		_, _ = os.Stdout.WriteString("done\n")
	}
	os.Exit(0)
}

type ceilingCgroupFixture struct {
	parent  string
	dir     string
	cmd     *exec.Cmd
	stdin   *os.File
	replies *bufio.Scanner
	// exited closes when the helper has been REAPED. A bare
	// Process.Signal(0) is not a liveness test here: an OOM-killed child that
	// nothing has waited on is a zombie, and signalling a zombie succeeds -- so
	// the negative control would have reported the job alive after the kernel
	// had killed it, and would have failed for the wrong reason.
	exited chan struct{}
}

func newCeilingCgroupFixture(t *testing.T) *ceilingCgroupFixture {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	scope := filepath.Join(parent, "fixture")
	if err := os.Mkdir(scope, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create fixture cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scope, "memory.max"), []byte(strconv.FormatInt(ceilingFixtureCap, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture memory.max is not writable: %v", err)
	}
	// Without this a breach would swap instead of OOM-killing, and the negative
	// control below could not establish that the fixture observes a kill at all.
	if err := os.WriteFile(filepath.Join(scope, "memory.swap.max"), []byte("0"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture memory.swap.max is not writable: %v", err)
	}
	scopeFD, err := os.OpenFile(scope, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer scopeFD.Close()

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSliceCeilingAllocHelper$")
	cmd.Env = append(os.Environ(),
		"AIRA_SLICE_CEILING_HELPER=1",
		"AIRA_SLICE_CEILING_HELPER_BYTES="+strconv.FormatInt(ceilingFixtureTouch, 10),
		"AIRA_SLICE_CEILING_HELPER_DIR="+dir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(scopeFD.Fd())}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = stdinRead
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdinWrite.Close()
		cgrouptest.SkipOrFailRealCgroup(t, "start fixture helper in %s: %v", scope, err)
	}
	_ = stdinRead.Close()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = stdinWrite.Close()
		_ = cmd.Process.Kill()
		<-exited
	})
	replies := bufio.NewScanner(stdout)
	if !replies.Scan() || strings.TrimSpace(replies.Text()) != "ready" {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture helper never reported ready: %v", replies.Err())
	}
	return &ceilingCgroupFixture{parent: parent, dir: scope, cmd: cmd, stdin: stdinWrite, replies: replies, exited: exited}
}

func (f *ceilingCgroupFixture) grow(t *testing.T, kind string) {
	t.Helper()
	if _, err := f.stdin.WriteString(kind + "\n"); err != nil {
		t.Fatalf("instruct helper to grow (%s): %v", kind, err)
	}
	if !f.replies.Scan() {
		t.Fatalf("helper did not acknowledge %s growth: %v", kind, f.replies.Err())
	}
}

// poke instructs the helper WITHOUT waiting for its acknowledgement, for the
// case where the instruction is expected to kill it.
func (f *ceilingCgroupFixture) poke(t *testing.T, kind string) {
	t.Helper()
	if _, err := f.stdin.WriteString(kind + "\n"); err != nil {
		t.Fatalf("instruct helper (%s): %v", kind, err)
	}
}

func (f *ceilingCgroupFixture) alive(t *testing.T) bool {
	t.Helper()
	select {
	case <-f.exited:
		return false
	default:
		return true
	}
}

func readCgroupInt(t *testing.T, dir, name string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	value, ok := parseAdmitMemory(data)
	if !ok {
		t.Fatalf("parse %s: %q", name, data)
	}
	return value
}

func readCgroupRaw(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func readCgroupOOMKills(t *testing.T, dir string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		t.Fatalf("read memory.events: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "oom_kill" {
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatalf("parse oom_kill: %q", fields[1])
			}
			return value
		}
	}
	t.Fatal("memory.events has no oom_kill line")
	return 0
}

// realCeilingSimulatedTotal is the MemTotal these fixtures model. MemAvailable is
// modelled rather than measured because this box carries dozens of concurrent
// confine jobs: the real figure moves by gigabytes between samples and a few
// hundred MiB of signal would be pure noise. The model is
// max(0, simulatedTotal - outside - sliceAnon) -- sliceAnon, NOT memory.current,
// because si_mem_available counts reclaimable file pages as available wherever
// they live, so only the slice's non-reclaimable footprint reduces it.
const realCeilingSimulatedTotal = int64(48 << 30)

func realCeilingDeps(dir string, memAvailable *int64, now *time.Time, publish func(sliceCeilingSnapshot)) sliceCeilingDeps {
	return sliceCeilingDeps{
		resolveSlice:     func() (string, bool, string) { return dir, true, "" },
		readSliceParts:   readSliceCeilingParts,
		readSliceCurrent: readSliceCeilingCurrent,
		readMemAvailable: func() (int64, bool, string) { return *memAvailable, true, "" },
		// AIRA-106: reserveMax 0 makes the static term non-binding here, so these
		// fixtures keep testing the PRESSURE term against real kernel accounting,
		// exactly as they did before. The static term has its own real-cgroup case
		// in TestSliceCeilingRealCgroupNeverShrinksBelowRealUsage.
		policy:  sliceCeilingPolicy{memTotal: realCeilingSimulatedTotal, reserveMax: 0, freeMin: 16 << 30},
		samples: sliceCeilingSamples,
		// A small quantum so the fixture's few hundred MiB actually move the
		// published ceiling. With the production 256 MiB quantum a 400 MiB step
		// would be indistinguishable from rounding, and the test would pass
		// against an implementation that did nothing.
		quantum: 16 << 20,
		ttl:     defaultSliceCeilingTTL,
		now:     func() time.Time { return *now },
		publish: publish,
	}
}

// verifies: THE SAFETY PROPERTY, empirically, against a real cgroup holding a
// real running process. Driven to the hardest throttle the subsystem can ever
// ask for, it must leave the kernel-enforced limit BYTE-IDENTICAL, must not
// OOM-kill the job, and must nonetheless close admission — the last clause is
// what stops the first three passing vacuously.
//
// This is the whole reason the actuator is an in-process ceiling rather than the
// cgroupfs write the ticket proposed: with no kernel-enforced limit moving,
// "this mechanism can never pressure a running job" is structural. The negative
// control below establishes that this fixture would have SEEN the failure.
func TestSliceCeilingRealCgroupThrottlesAdmissionWithoutTouchingTheJob(t *testing.T) {
	fixture := newCeilingCgroupFixture(t)
	maxBefore := readCgroupRaw(t, fixture.dir, "memory.max")
	killsBefore := readCgroupOOMKills(t, fixture.dir)
	currentBefore := readCgroupInt(t, fixture.dir, "memory.current")
	if currentBefore < ceilingFixtureTouch/2 {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture charged only %d bytes; the helper's allocation is not accounted here", currentBefore)
	}

	memAvailable := int64(0) // hardest possible throttle: desired underflows to 0
	now := time.Unix(1_700_000_000, 0)
	server := NewServer(Paths{})
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	deps := realCeilingDeps(fixture.dir, &memAvailable, &now, server.publishSliceCeilingSnapshot)
	state := &sliceCeilingState{}
	var snapshot sliceCeilingSnapshot
	for i := 0; i < 4*sliceCeilingSamples; i++ {
		snapshot = evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
		now = now.Add(2 * time.Second)
	}

	if snapshot.State != sliceCeilingThrottled || snapshot.Ceiling != 0 {
		t.Fatalf("snapshot=%+v, want a fully throttled zero ceiling — otherwise the assertions below are vacuous", snapshot)
	}
	if effective := server.admitEffectiveMaximum(fixture.dir, ceilingFixtureCap); effective != 0 {
		t.Fatalf("effective maximum=%d under a zero ceiling, want 0", effective)
	}
	if available := checkedAvailable(currentBefore, server.admitEffectiveMaximum(fixture.dir, ceilingFixtureCap), 0, 0, 0); available != 0 {
		t.Fatalf("available=%d, want admission fully closed", available)
	}
	if maxAfter := readCgroupRaw(t, fixture.dir, "memory.max"); maxAfter != maxBefore {
		t.Fatalf("memory.max moved from %q to %q — this mechanism must never write a kernel-enforced limit", maxBefore, maxAfter)
	}
	if kills := readCgroupOOMKills(t, fixture.dir); kills != killsBefore {
		t.Fatalf("oom_kill went from %d to %d: a running job was pressured", killsBefore, kills)
	}
	if !fixture.alive(t) {
		t.Fatal("the fixture job died while the ceiling was throttled")
	}
}

// verifies: THE NEGATIVE CONTROL. It proves the FIXTURE, not the subsystem --
// without it, "memory.max unchanged and the job survived" could be true of a
// fixture that could never have observed a kill in the first place. Here the
// test itself writes an unclamped cap below the helper's resident anon, exactly
// as an un-clamped cgroupfs writer would, and the job must die.
//
// MEASURED KERNEL FACT, and the reason this test provokes an allocation rather
// than asserting on the write alone: memory_max_write sets page_counter max
// FIRST and only then runs its reclaim-then-OOM loop, and that loop bails with
// -EINTR on signal_pending. A Go process writing the file is permanently
// signal-pending (the runtime's own SIGURG async-preemption), so the write
// returns with the cap APPLIED, no reclaim performed and no OOM raised -- 632 MB
// of anon sitting under a 32 MiB cap, observed here. The kill is merely
// DEFERRED to the job's next charge. That is a strictly worse failure mode than
// an immediate kill (the cgroup is silently over-limit and the death lands on
// whatever allocates next, arbitrarily later), and it is one more reason this
// subsystem does not write the file at all.
func TestSliceCeilingRealCgroupHarnessDetectsALimitWrite(t *testing.T) {
	fixture := newCeilingCgroupFixture(t)
	killsBefore := readCgroupOOMKills(t, fixture.dir)
	if err := os.WriteFile(filepath.Join(fixture.dir, "memory.max"), []byte(strconv.FormatInt(32<<20, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "write fixture memory.max: %v", err)
	}
	if got := readCgroupInt(t, fixture.dir, "memory.max"); got != 32<<20 {
		cgrouptest.SkipOrFailRealCgroup(t, "memory.max read back as %d, want the 32MiB the test wrote", got)
	}
	fixture.poke(t, "anon")
	deadline := testdeadline.After(30 * time.Second)
	for {
		if readCgroupOOMKills(t, fixture.dir) > killsBefore && !fixture.alive(t) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("a cap of 32MiB neither OOM-killed nor stopped a job holding %d bytes of touched anon and allocating more; this fixture cannot observe the failure mode the safety test claims to exclude (current=%d events=%q)",
				ceilingFixtureTouch, readCgroupInt(t, fixture.dir, "memory.current"), readCgroupRaw(t, fixture.dir, "memory.events"))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// verifies: THE SIGNAL, against the kernel's own per-cgroup accounting rather
// than the arithmetic every unit test injects.
//
// Deliberately in two halves, because only the first is non-circular and saying
// so is the difference between a proof and a tautology:
//
//  1. KERNEL TRUTH. sliceCeilingAnon, computed from the REAL memory.current and
//     memory.stat of a real cgroup, must rise by an anonymous allocation and stay
//     FLAT across page-cache growth of the same size. That is the uncertain fact
//     the whole signal rests on -- that inactive_file+active_file really do
//     absorb the slice's cache on this kernel -- and nothing in this test
//     computes it for the kernel.
//  2. END TO END. With MemAvailable modelled the way si_mem_available actually
//     behaves (only NON-reclaimable usage reduces it), the published ceiling must
//     be invariant to both growth forms and must track external footprint one for
//     one. This half shares the sliceAnon term with the implementation, so it is
//     not independent evidence for (1); it covers the wiring, damping,
//     quantisation and clamping around it.
//
// MemAvailable is modelled rather than measured because this box carries dozens
// of concurrent confine jobs: the real figure moves by gigabytes between samples
// and a few hundred MiB of signal would be pure noise.
func TestSliceCeilingRealCgroupSignalTracksRealAccounting(t *testing.T) {
	fixture := newCeilingCgroupFixture(t)
	realAnon := func() (anon, current int64) {
		t.Helper()
		current, reclaimable, _, ok, reason := readSliceCeilingParts(fixture.dir)
		if !ok {
			cgrouptest.SkipOrFailRealCgroup(t, "fixture slice read unevaluated: %s", reason)
		}
		return sliceCeilingAnon(current, reclaimable), current
	}

	// (1) KERNEL TRUTH.
	baseAnon, _ := realAnon()
	const tolerance = int64(96 << 20) // the helper's own runtime churns a little
	fixture.grow(t, "anon")
	grownAnon, grownCurrent := realAnon()
	if grownAnon-baseAnon < ceilingFixtureTouch-tolerance {
		t.Fatalf("sliceCeilingAnon rose only %d for %d bytes of real anonymous allocation", grownAnon-baseAnon, ceilingFixtureTouch)
	}
	fixture.grow(t, "file")
	cachedAnon, cachedCurrent := realAnon()
	if cachedCurrent-grownCurrent < ceilingFixtureTouch/2 {
		t.Skipf("memory.current rose only %d for a %d-byte file write; the page cache is not being charged here, so this kernel fact cannot be established",
			cachedCurrent-grownCurrent, ceilingFixtureTouch)
	}
	if delta := cachedAnon - grownAnon; delta > tolerance || delta < -tolerance {
		t.Fatalf("sliceCeilingAnon moved %d across %d bytes of PAGE CACHE growth (memory.current rose %d); the file LRU must absorb it or the signal is not invariant to the slice's own caching",
			delta, ceilingFixtureTouch, cachedCurrent-grownCurrent)
	}

	// (2) END TO END.
	const simulatedTotal = realCeilingSimulatedTotal
	outside := int64(8 << 30)
	memAvailable := int64(0)
	now := time.Unix(1_700_000_000, 0)
	var published sliceCeilingSnapshot
	deps := realCeilingDeps(fixture.dir, &memAvailable, &now, func(s sliceCeilingSnapshot) { published = s })
	// The fixture's own 2 GiB cap is far below any plausible desired ceiling, so
	// the modelled maximum is raised out of the way: this half is about the
	// SIGNAL, and the min-with-memory.max clamp is pinned by the unit tests.
	deps.readSliceParts = func(path string) (int64, int64, int64, bool, string) {
		current, reclaimable, _, ok, reason := readSliceCeilingParts(path)
		return current, reclaimable, simulatedTotal, ok, reason
	}
	state := &sliceCeilingState{}
	settleReal := func() int64 {
		t.Helper()
		for i := 0; i < 2*sliceCeilingSamples; i++ {
			anon, _ := realAnon()
			// si_mem_available counts reclaimable file pages as AVAILABLE
			// wherever they live, so only the slice's non-reclaimable footprint
			// reduces it. Modelling it as total-outside-memory.current instead
			// would make the slice's own page cache look like consumed memory,
			// which is exactly the mistake the signal exists to avoid.
			memAvailable = subtractFloor(simulatedTotal-outside, anon)
			evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
			now = now.Add(2 * time.Second)
		}
		if published.State == sliceCeilingUnevaluated {
			cgrouptest.SkipOrFailRealCgroup(t, "ceiling unevaluated against the real fixture: %s", published.Reason)
		}
		return published.Ceiling
	}

	base := settleReal()
	fixture.grow(t, "anon")
	if got := settleReal(); got > base+tolerance || got < base-tolerance {
		t.Fatalf("ceiling moved from %d to %d for the SLICE'S OWN anon growth, want it invariant", base, got)
	}
	fixture.grow(t, "file")
	if got := settleReal(); got > base+tolerance || got < base-tolerance {
		t.Fatalf("ceiling moved from %d to %d for the slice's own PAGE CACHE growth, want it invariant", base, got)
	}

	// Identical growth OUTSIDE the slice must move it, one for one.
	const external = int64(2 << 30)
	outside += external
	got := settleReal()
	if base-got < external-tolerance || base-got > external+tolerance {
		t.Fatalf("ceiling moved %d for %d bytes of EXTERNAL pressure, want it to track one for one", base-got, external)
	}
	if !fixture.alive(t) {
		t.Fatal("the fixture job died during the signal test")
	}
}

// AIRA-106 real-cgroup proof of the SAFETY BOUND.
//
// The brief asks for "a real-cgroup test proving the new formula never shrinks
// below current real usage". The naive form of that claim is FALSE, and stating
// it precisely is half the work:
//
//	published = min(memory.max, quantise_down(min(machineTerm, sliceAnon + Δ)))
//	                                              where Δ = MemAvailable - freeMin
//
// Quantise-down loses strictly less than one quantum, so there are THREE ways for
// the published ceiling to land under sliceAnon and the claim needs all three
// closed. The provable statements are:
//
//	(a) Δ >= quantum  AND  machineTerm >= sliceAnon + quantum  AND  memory.max >= sliceAnon
//	      => published >= sliceAnon
//	(b) machineTerm >= sliceAnon  AND  memory.max >= sliceAnon
//	      => published >= sliceAnon - quantum
//
// A draft stated (a) with only `machineTerm >= sliceAnon` and no memory.max term.
// That is FALSE: sliceAnon = 1.6 GiB against machineTerm = 1.7 GiB quantises to
// 1.5 GiB however much pressure margin there is. Every precondition is therefore
// asserted in the test body rather than left to the fixture to satisfy by luck.
//
// The admission corollary is ONE-DIRECTIONAL -- sufficient, not necessary:
// available > 0 follows from Δ > slab_reclaimable + quantum + headroom (plus any
// excess of outstanding over the slice's charge), because checkedAvailable
// charges current - inactive_file - active_file, which is sliceAnon PLUS slab
// (readSliceMemory discards slab; sliceCeilingAnon does not).
//
// The claim is proved against the KERNEL'S OWN per-cgroup accounting, with
// MemAvailable modelled per realCeilingSimulatedTotal.
const ceilingBoundMargin = int64(1 << 30)

// ceilingBoundCharge reads the inputs ADMISSION itself uses, through admission's
// OWN reader, plus the slab figure that only the ceiling signal subtracts.
//
// The distinction is load-bearing and easy to get wrong: readSliceCeilingParts
// returns reclaimable = file_LRU + slab (addClamp'd together), while
// readSliceMemory returns file_LRU ONLY, because AIRA-21's admission discount
// deliberately excludes slab. Deriving slab as
// `ceilingCurrent - ceilingReclaimable - sliceAnon` therefore yields IDENTICALLY
// ZERO -- an earlier draft of this file did exactly that, so its slab guard could
// never fire and its checkedAvailable call discounted slab that production does
// not, making the corollary strictly more permissive than the code it modelled.
func ceilingBoundCharge(t *testing.T, dir string) (current, admitReclaimable, sliceAnon, slab int64) {
	t.Helper()
	current, _, admitReclaimable, ok, reason := readSliceMemory(dir)
	if !ok {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture admission read unevaluated: %s", reason)
	}
	ceilingCurrent, ceilingReclaimable, _, ok, reason := readSliceCeilingParts(dir)
	if !ok {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture slice read unevaluated: %s", reason)
	}
	return current, admitReclaimable, sliceCeilingAnon(ceilingCurrent, ceilingReclaimable),
		subtractFloor(ceilingReclaimable, admitReclaimable)
}

// ceilingBoundProbe settles the subsystem against the real cgroup and reports
// the published snapshot beside the real sliceAnon it must be judged against.
// Shared by the bound test and its negative control so the control is testing
// the SAME assertion path, not a parallel one -- which is what makes it a
// control rather than a second test.
func ceilingBoundProbe(t *testing.T, fixture *ceilingCgroupFixture, policy sliceCeilingPolicy, outside int64) (published sliceCeilingSnapshot, sliceAnon, slab int64) {
	t.Helper()
	memAvailable := int64(0)
	now := time.Unix(1_700_000_000, 0)
	deps := realCeilingDeps(fixture.dir, &memAvailable, &now, func(s sliceCeilingSnapshot) { published = s })
	deps.policy = policy
	// The fixture's own 2 GiB memory.max would clamp every candidate, hiding the
	// policy under the clamp; the modelled maximum is raised out of the way so the
	// FORMULA is what is under test. The clamp itself is pinned by the unit tests.
	deps.readSliceParts = func(path string) (int64, int64, int64, bool, string) {
		current, reclaimable, _, ok, reason := readSliceCeilingParts(path)
		return current, reclaimable, policy.memTotal, ok, reason
	}
	state := &sliceCeilingState{}
	// Deliberately more than one full window: the published figure is the MAX over
	// sliceCeilingSamples, so a control that drives pressure for fewer ticks would
	// still be reading the previous (higher) ceiling and would "fail" for a reason
	// that has nothing to do with the bound.
	for i := 0; i < 2*sliceCeilingSamples; i++ {
		_, _, sliceAnon, slab = ceilingBoundCharge(t, fixture.dir)
		// si_mem_available counts reclaimable file pages as available wherever they
		// live, so only the slice's NON-reclaimable footprint reduces it. Modelling
		// it against memory.current instead would count the slice's own page cache
		// as consumed memory, the exact mistake this signal exists to avoid.
		memAvailable = subtractFloor(policy.memTotal-outside, sliceAnon)
		evaluateSliceCeiling(sliceCeilingEnforce, state, deps)
		now = now.Add(2 * time.Second)
	}
	if published.State == sliceCeilingUnevaluated {
		// A FAILURE, not a skip. The fixture and every read above are already
		// established by this point (ceilingBoundCharge would have skipped
		// otherwise), so an unevaluated publication after two full windows of good
		// samples is the subsystem being wrong -- and routing it through
		// SkipOrFailRealCgroup would let an implementation that ALWAYS publishes
		// unevaluated skip both this test and its negative control on any run
		// without AIRA_REAL_CGROUP=1.
		t.Fatalf("ceiling still unevaluated after %d established samples: %s", 2*sliceCeilingSamples, published.Reason)
	}
	return published, sliceAnon, slab
}

// verifies (AIRA-106): THE SAFETY BOUND, against a real cgroup holding a real
// running process. With the machine term out of the way and MemAvailable a full
// margin above freeMin, the published ceiling never falls below what the slice
// is ALREADY holding -- so admission can never close merely because the slice
// holds what it holds -- and the AIRA-103 actuator clauses still hold: the
// kernel-enforced limit is byte-identical, nothing is OOM-killed, the job lives.
//
// Both real growth forms are exercised, because the bound has to survive the
// page-cache case that broke every naive formulation of this signal.
func TestSliceCeilingRealCgroupNeverShrinksBelowRealUsage(t *testing.T) {
	fixture := newCeilingCgroupFixture(t)
	maxBefore := readCgroupRaw(t, fixture.dir, "memory.max")
	killsBefore := readCgroupOOMKills(t, fixture.dir)

	// freeMin is held a full margin below the modelled MemAvailable, and the
	// machine term is left non-binding, so this measures exactly claim (a).
	policy := sliceCeilingPolicy{memTotal: realCeilingSimulatedTotal, reserveMax: 0, freeMin: 8 << 30}
	outside := realCeilingSimulatedTotal - policy.freeMin - ceilingBoundMargin - (4 << 30)

	for _, step := range []string{"", "anon", "file"} {
		if step != "" {
			fixture.grow(t, step)
		}
		published, sliceAnon, slab := ceilingBoundProbe(t, fixture, policy, outside)
		if sliceAnon <= 0 {
			cgrouptest.SkipOrFailRealCgroup(t, "fixture holds no non-reclaimable memory after %q; the bound would be vacuous", step)
		}
		if slab >= ceilingBoundMargin {
			cgrouptest.SkipOrFailRealCgroup(t, "fixture slab_reclaimable %d is not below the %d margin; the corollary below is unprovable here", slab, ceilingBoundMargin)
		}
		// EVERY precondition of the claim is asserted, not assumed. The bound is
		// `published >= sliceAnon`, and quantise-down alone can lose almost a whole
		// quantum, so it needs BOTH a pressure margin of at least one quantum AND a
		// machine term at least one quantum clear of sliceAnon -- and the ceiling is
		// also clamped to the (modelled) maximum, so that must clear sliceAnon too.
		// A draft that omitted the machine and maximum preconditions asserted
		// something false; it passed only because this fixture happens to leave
		// gigabytes of slack around a ~1 GiB footprint.
		if published.MemAvailable-policy.freeMin < ceilingBoundMargin {
			cgrouptest.SkipOrFailRealCgroup(t, "modelled MemAvailable %d is not a full %d above freeMin %d after %q",
				published.MemAvailable, ceilingBoundMargin, policy.freeMin, step)
		}
		if policy.machineTerm() < sliceAnon+ceilingBoundMargin || policy.memTotal < sliceAnon+ceilingBoundMargin {
			cgrouptest.SkipOrFailRealCgroup(t, "machine term %d / modelled maximum %d is not a full %d clear of the footprint %d after %q; the bound does not apply",
				policy.machineTerm(), policy.memTotal, ceilingBoundMargin, sliceAnon, step)
		}
		if published.Ceiling < sliceAnon {
			t.Fatalf("after %q growth the published ceiling %d fell BELOW the slice's own real footprint %d while MemAvailable %d was %d above freeMin %d",
				step, published.Ceiling, sliceAnon, published.MemAvailable, published.MemAvailable-policy.freeMin, policy.freeMin)
		}
		// The admission corollary, through ADMISSION'S OWN reader so the charge is
		// production's (current - file LRU = sliceAnon + slab) and not a more
		// forgiving one. headroom and outstanding are zeroed so the margin asserted
		// above is the whole story; the corollary is stated as SUFFICIENT, so the
		// non-zero cases are strictly harder and are not claimed here.
		current, admitReclaimable, _, _ := ceilingBoundCharge(t, fixture.dir)
		if available := checkedAvailable(current, published.Ceiling, admitReclaimable, 0, 0); available <= 0 {
			t.Fatalf("after %q growth admission closed (available=%d) though MemAvailable was %d above freeMin and slab was %d: the ceiling is not leaving room for the slice's existing footprint",
				step, available, published.MemAvailable-policy.freeMin, slab)
		}
		if maxAfter := readCgroupRaw(t, fixture.dir, "memory.max"); maxAfter != maxBefore {
			t.Fatalf("memory.max moved from %q to %q after %q: this mechanism must never write a kernel-enforced limit", maxBefore, maxAfter, step)
		}
		if kills := readCgroupOOMKills(t, fixture.dir); kills != killsBefore {
			t.Fatalf("oom_kill went from %d to %d after %q: a running job was pressured", killsBefore, kills, step)
		}
		if !fixture.alive(t) {
			t.Fatalf("the fixture job died after %q growth", step)
		}
	}

	// The MACHINE half, where "never below real usage" is deliberately NOT an
	// invariant. With the static term set below the slice's own footprint the
	// ceiling DOES fall under it and admission closes -- correctly, because the
	// slice is already over its configured share of the machine -- and, critically,
	// STILL without touching anything kernel-enforced. Saying so here is the
	// difference between a proof and a slogan.
	_, sliceAnon, _ := ceilingBoundProbe(t, fixture, policy, outside)
	overCommitted := sliceCeilingPolicy{
		memTotal:   realCeilingSimulatedTotal,
		reserveMax: realCeilingSimulatedTotal - sliceAnon/2,
		freeMin:    policy.freeMin,
	}
	published, sliceAnon, _ := ceilingBoundProbe(t, fixture, overCommitted, outside)
	if published.Basis != sliceCeilingBasisMachine {
		t.Fatalf("basis=%q with the machine term below the slice's footprint, want machine-reserve", published.Basis)
	}
	if published.Ceiling >= sliceAnon {
		t.Fatalf("ceiling=%d did not fall below the slice's footprint %d under a machine term of %d; the machine half of the policy is not being applied",
			published.Ceiling, sliceAnon, overCommitted.machineTerm())
	}
	if maxAfter := readCgroupRaw(t, fixture.dir, "memory.max"); maxAfter != maxBefore {
		t.Fatalf("memory.max moved from %q to %q under the machine term", maxBefore, maxAfter)
	}
	if kills := readCgroupOOMKills(t, fixture.dir); kills != killsBefore || !fixture.alive(t) {
		t.Fatalf("the machine term pressured a running job: oom_kill %d -> %d, alive=%v", killsBefore, kills, fixture.alive(t))
	}
}

// verifies (AIRA-106): THE NEGATIVE CONTROL for the bound above.
//
// Distinct from TestSliceCeilingRealCgroupHarnessDetectsALimitWrite, which
// validates the no-write ACTUATOR. This one validates the BOUND'S OWN HARNESS:
// it drives the same fixture through the same ceilingBoundProbe with MemAvailable
// pushed BELOW freeMin, and the probe must then report a ceiling under the real
// sliceAnon and a closed admission. Without it, "the ceiling stayed above real
// usage" could be true of a harness that could never have observed the opposite.
//
// The pressure is SUSTAINED for more than a full damping window on purpose: the
// published figure is the max over sliceCeilingSamples, so a single low sample
// would leave the previous ceiling standing and the control would report a pass
// it had not earned.
func TestSliceCeilingRealCgroupUsageBoundHarnessDetectsAViolation(t *testing.T) {
	fixture := newCeilingCgroupFixture(t)
	fixture.grow(t, "anon")
	policy := sliceCeilingPolicy{memTotal: realCeilingSimulatedTotal, reserveMax: 0, freeMin: 8 << 30}

	// The model makes the pressure term independent of the slice's own footprint
	// -- memAvailable = simulatedTotal - outside - sliceAnon, so
	// pressure = simulatedTotal - outside - freeMin -- which is exactly the
	// invariance the signal is built for and which lets the violation be aimed.
	_, measuredAnon, _ := ceilingBoundProbe(t, fixture, policy, realCeilingSimulatedTotal-policy.freeMin-(4<<30))
	if measuredAnon <= sliceCeilingQuantum {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture footprint %d is not above one quantum; a partial violation cannot be aimed", measuredAnon)
	}

	for _, test := range []struct {
		name        string
		outside     int64
		wantCeiling string
	}{
		{
			// The HARDEST violation the model allows: everything outside the slice
			// is in use, MemAvailable floors at zero, the ceiling floors at zero.
			name: "ceiling-floored-at-zero", outside: realCeilingSimulatedTotal, wantCeiling: "zero",
		},
		{
			// The case the zero-ceiling row CANNOT reach: a NON-ZERO ceiling that is
			// nonetheless below the slice's own footprint. It matters because
			// checkedAvailable short-circuits on `maximum <= headroom` before it ever
			// compares a charge -- so with a zero ceiling the "admission closed"
			// assertion is satisfied by the ceiling alone and never exercises the
			// comparison the bound is actually about.
			name: "ceiling-below-footprint-but-positive", wantCeiling: "in (0, sliceAnon)",
			outside: realCeilingSimulatedTotal - policy.freeMin - measuredAnon/2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			published, sliceAnon, _ := ceilingBoundProbe(t, fixture, policy, test.outside)
			if sliceAnon <= 0 {
				cgrouptest.SkipOrFailRealCgroup(t, "fixture holds no non-reclaimable memory; the control would be vacuous")
			}
			if published.Ceiling >= sliceAnon {
				t.Fatalf("the harness reported a ceiling %d (want %s) at or above the real footprint %d, with MemAvailable %d against freeMin %d: it cannot observe the violation the bound test claims to exclude",
					published.Ceiling, test.wantCeiling, sliceAnon, published.MemAvailable, policy.freeMin)
			}
			// Through admission's own reader, for the same reason the positive case
			// uses it: a more generous reclaimable discount here would make the
			// control easier to satisfy than production.
			current, admitReclaimable, _, _ := ceilingBoundCharge(t, fixture.dir)
			if available := checkedAvailable(current, published.Ceiling, admitReclaimable, 0, 0); available != 0 {
				t.Fatalf("available=%d under a ceiling %d below the slice's own footprint %d, want admission fully closed", available, published.Ceiling, sliceAnon)
			}
		})
	}
}
