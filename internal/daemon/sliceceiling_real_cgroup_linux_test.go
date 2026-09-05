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

func realCeilingDeps(dir string, memAvailable *int64, now *time.Time, publish func(sliceCeilingSnapshot)) sliceCeilingDeps {
	return sliceCeilingDeps{
		resolveSlice:     func() (string, bool, string) { return dir, true, "" },
		readSliceParts:   readSliceCeilingParts,
		readSliceCurrent: readSliceCeilingCurrent,
		readMemAvailable: func() (int64, bool, string) { return *memAvailable, true, "" },
		reserve:          16 << 30,
		samples:          sliceCeilingSamples,
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
	const simulatedTotal = int64(48 << 30)
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
