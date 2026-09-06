package runner

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// countingMembersScope wraps confineFakeScope and counts Members() calls so a
// test can observe how often the membership sampler polls the scope.
type countingMembersScope struct {
	confineFakeScope
	countMu sync.Mutex
	count   int
}

func (s *countingMembersScope) Members() ([]int, error) {
	s.countMu.Lock()
	s.count++
	s.countMu.Unlock()
	return s.confineFakeScope.Members()
}

func (s *countingMembersScope) calls() int {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return s.count
}

func (s *countingMembersScope) setMembers(members []int) {
	s.mu.Lock()
	s.members = append([]int(nil), members...)
	s.mu.Unlock()
}

// verifies: the scope-membership sampler polls cgroup.procs at
// scopeMembershipSampleInterval and no faster, and still takes its final sample
// on stop. The sampler runs for the whole lifetime of every `aira confine` /
// `aira run` supervisor and its per-tick cost is O(scope tree size) /proc reads;
// a 2ms tick made an idle single-process supervisor burn ~13% of a core and a
// 31-process tree ~112% (2026-09-03 investigation). The ceiling is
// window/interval plus one tick of jitter (the seed sample uses the initial
// members and does not poll).
func TestScopeMembershipSamplerIsRateLimited(t *testing.T) {
	// The contract floor: below ~20ms the sampler's O(tree) /proc reads cost
	// whole percents of a core per supervisor (13% idle, 112% for a 31-process
	// tree at 2ms), and several supervisors run concurrently on a shared box.
	if scopeMembershipSampleInterval < 20*time.Millisecond {
		t.Fatalf("scopeMembershipSampleInterval=%v is below the 20ms contract floor", scopeMembershipSampleInterval)
	}
	leader, err := currentPIDIdentity()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	scope := &countingMembersScope{confineFakeScope: confineFakeScope{members: []int{leader.PID}}}
	stop := make(chan struct{})
	result := make(chan scopeMonitorResult, 1)
	go monitorScopeMembership(scope, leader, []int{leader.PID}, stop, result)
	const window = 300 * time.Millisecond
	time.Sleep(window)
	calls := scope.calls()
	ceiling := int(window/scopeMembershipSampleInterval) + 2
	if calls > ceiling {
		t.Fatalf("sampler polled Members() %d times in %v; ceiling %d at interval %v", calls, window, ceiling, scopeMembershipSampleInterval)
	}
	if calls < 1 {
		t.Fatalf("sampler never polled Members() in %v", window)
	}
	// A descendant that joins right before stop must be seen by the on-stop
	// sample (it confirms new members with a second Members() read).
	scope.setMembers([]int{leader.PID, child.Process.Pid})
	close(stop)
	summary := <-result
	if scope.calls() < calls+1 {
		t.Fatalf("no on-stop sample: %d calls before stop, %d after", calls, scope.calls())
	}
	if !summary.HadDescendants {
		t.Fatalf("on-stop sample missed the descendant %d: %+v", child.Process.Pid, summary)
	}
	if summary.LeaderMigrated || summary.Gap || summary.Escape != nil {
		t.Fatalf("in-scope leader and descendant produced a non-clean summary: %+v", summary)
	}
}

// referencePathScope wraps confineFakeScope with an explicit Reference(),
// rather than confineFakeScope's fixed "/fake/scope", so a test can point the
// scope at a path under the real cgroup-v2 mount and check a mocked
// /proc/<pid>/cgroup observation against it with pathEqualOrUnder.
type referencePathScope struct {
	confineFakeScope
	ref string
}

func (s *referencePathScope) Reference() string { return s.ref }

// AIRA-34 regression: a leader that relocates ITSELF into a descendant cgroup
// it created (aitest's supervisor moving into `outer/.aira-supervisor` before
// forking per-worker sub-scopes, a podman --cgroups=split nested container, or
// any other legitimate nesting) is absent from the scope's own cgroup.procs
// while remaining genuinely within the scope subtree. The leaf-only presence
// test used to call this a migration; monitorScopeMembership must instead
// apply the same subtree-aware witness the descendant loop already uses and
// report it as contained.
func TestScopeMembershipLeaderRelocatesIntoNestedSubScopeIsNotMigrated(t *testing.T) {
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })
	mount, err := unifiedMount()
	if err != nil {
		t.Skip(err)
	}
	scopePath := filepath.Join(mount, "aira34-leader-nested-scope")
	leader := PIDIdentity{PID: 9101, StartTick: 4242, BootID: "boot"}
	readBootIDFn = func() (string, error) { return "boot", nil }
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', leader.StartTick), nil }
	readProcCgroupFn = func(int) ([]byte, error) {
		return []byte("0::/aira34-leader-nested-scope/nested\n"), nil
	}
	scope := &referencePathScope{ref: scopePath}
	stop := make(chan struct{})
	result := make(chan scopeMonitorResult, 1)
	// initialMembers is empty: the leader has already relocated itself into the
	// nested sub-scope by the time this sample runs, mirroring the aitest
	// supervisor's self-relocation ahead of the runner's membership poll.
	go monitorScopeMembership(scope, leader, []int{}, stop, result)
	close(stop)
	summary := <-result
	if summary.LeaderMigrated {
		t.Fatalf("leader relocated into its own nested sub-scope reported migrated: %+v", summary)
	}
	if summary.Gap {
		t.Fatalf("leader relocated into a readable nested sub-scope produced a spurious gap: %+v", summary)
	}
	if summary.Escape != nil {
		t.Fatalf("leader's own relocation was recorded as a descendant escape: %+v", summary)
	}
}

// AIRA-34 regression, the other direction: a leader that genuinely leaves the
// scope subtree entirely (not into a descendant of its own scope) must still
// be witnessed as a migration. This mutation-verifies the escape-detection
// guard (pathEqualOrUnder via witnessedEscape) still fires once the leaf-only
// test is replaced with the subtree-aware one.
func TestScopeMembershipLeaderRelocatesOutOfScopeIsStillMigrated(t *testing.T) {
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })
	mount, err := unifiedMount()
	if err != nil {
		t.Skip(err)
	}
	scopePath := filepath.Join(mount, "aira34-leader-nested-scope")
	leader := PIDIdentity{PID: 9102, StartTick: 4343, BootID: "boot"}
	readBootIDFn = func() (string, error) { return "boot", nil }
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', leader.StartTick), nil }
	readProcCgroupFn = func(int) ([]byte, error) {
		// A sibling that shares the scope's directory name as a STRING PREFIX
		// (not a path component) mutation-tests that containment is decided by
		// pathEqualOrUnder's component-wise filepath.Rel, not a naive
		// strings.HasPrefix that a regression could substitute.
		return []byte("0::/aira34-leader-nested-scope-sibling\n"), nil
	}
	scope := &referencePathScope{ref: scopePath}
	stop := make(chan struct{})
	result := make(chan scopeMonitorResult, 1)
	go monitorScopeMembership(scope, leader, []int{}, stop, result)
	close(stop)
	summary := <-result
	if !summary.LeaderMigrated {
		t.Fatalf("leader relocated entirely outside the scope tree was not reported migrated: %+v", summary)
	}
}

// A leader absent from the scope whose /proc/<pid>/cgroup cannot be read is
// neither provably contained nor provably escaped: it must be a residual gap,
// never a false migration claim.
func TestScopeMembershipLeaderAbsentWithUnreadableCgroupIsGapNotMigrated(t *testing.T) {
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })
	mount, err := unifiedMount()
	if err != nil {
		t.Skip(err)
	}
	scopePath := filepath.Join(mount, "aira34-leader-nested-scope")
	leader := PIDIdentity{PID: 9103, StartTick: 4444, BootID: "boot"}
	readBootIDFn = func() (string, error) { return "boot", nil }
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', leader.StartTick), nil }
	readProcCgroupFn = func(int) ([]byte, error) { return nil, errors.New("hidepid") }
	scope := &referencePathScope{ref: scopePath}
	stop := make(chan struct{})
	result := make(chan scopeMonitorResult, 1)
	go monitorScopeMembership(scope, leader, []int{}, stop, result)
	close(stop)
	summary := <-result
	if summary.LeaderMigrated {
		t.Fatalf("unreadable leader cgroup was falsely reported migrated: %+v", summary)
	}
	if !summary.Gap {
		t.Fatalf("unreadable leader cgroup did not record a gap: %+v", summary)
	}
}

// eventsPathScope is a fake scope whose cgroup.events lives at an arbitrary
// path, so the inotify watcher can be exercised against a real file.
type eventsPathScope struct {
	confineFakeScope
	events string
}

func (s *eventsPathScope) EventsPath() string { return s.events }

// inotifyFDs returns the set of this process's inotify descriptor numbers.
//
// The measurement used to be len(os.ReadDir("/proc/self/fd")) — a process-global
// COUNT in a test binary where earlier tests leave goroutines holding sockets, pipes
// and capture files. One of those closing between the baseline and the check cancels
// the watcher's own +1 exactly, and the assertion fails with "9 open fds, baseline 9",
// observed during AIRA-20's own whole-suite verification with no code change behind it
// and no reproduction in isolation.
//
// Narrowing it to a count of inotify descriptors was not enough: monitorScopeMembership
// creates one per real-cgroup run in this same binary, so an inotify already present in
// the baseline that closes mid-check reproduces the identical cancellation on a smaller
// population. Returning the SET closes it. The caller asks whether a NEW descriptor
// appeared and whether that SPECIFIC descriptor went away, which no concurrent open or
// close elsewhere in the process can affect.
//
// If the kernel ever stopped naming these descriptors this way the set would be empty
// and the establishment check would FAIL rather than silently pass, which is the honest
// direction for a measurement that can no longer establish its result.
func inotifyFDs(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, 4)
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			// Closed under us — including the descriptor ReadDir itself held. A
			// descriptor that no longer exists is not an inotify watch.
			continue
		}
		if strings.Contains(target, "inotify") {
			found[entry.Name()] = true
		}
	}
	return found
}

// newInotifyFD returns the one descriptor present in after and absent from before, or
// fails. Exactly one is the property: the watcher opens a single inotify fd.
func newInotifyFD(t *testing.T, before, after map[string]bool) string {
	t.Helper()
	var added []string
	for fd := range after {
		if !before[fd] {
			added = append(added, fd)
		}
	}
	if len(added) != 1 {
		t.Fatalf("inotify watch not established: %d new inotify fds (before=%v after=%v)", len(added), before, after)
	}
	return added[0]
}

// readSyscalls returns this process's cumulative read-syscall count (syscr from
// /proc/self/io); the accounting counts the read of /proc/self/io itself.
func readSyscalls(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "syscr: "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("/proc/self/io has no syscr line")
	return 0
}

// verifies: scopeMembershipEvents wakes the sampler when cgroup.events changes,
// costs no syscalls while idle, coalesces a burst without blocking, and releases
// its inotify fd once stop closes — across repeated start/stop cycles. The idle
// assertion is the regression guard: the previous reader busy-polled the fd
// (read → EAGAIN → sleep 1ms), ~200 reads per 200ms idle; the pollable reader
// parks in epoll and makes none.
func TestScopeMembershipEventsDeliversModifyAndReleasesFD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cgroup.events")
	if err := os.WriteFile(path, []byte("populated 1\nfrozen 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 3; cycle++ {
		before := inotifyFDs(t)
		stop := make(chan struct{})
		events := scopeMembershipEvents(&eventsPathScope{events: path}, stop)
		watchFD := newInotifyFD(t, before, inotifyFDs(t))
		const idle = 200 * time.Millisecond
		readsBefore := readSyscalls(t)
		select {
		case <-events:
			t.Fatalf("cycle %d: event delivered before any modification", cycle)
		case <-time.After(idle):
		}
		if idleReads := readSyscalls(t) - readsBefore; idleReads > 20 {
			t.Fatalf("cycle %d: watcher made %d read syscalls over %v idle; a parked reader makes none", cycle, idleReads, idle)
		}
		if err := os.WriteFile(path, []byte("populated 0\nfrozen 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		select {
		case <-events:
		case <-testdeadline.After(2 * time.Second):
			t.Fatalf("cycle %d: cgroup.events modification did not wake the sampler", cycle)
		}
		// A burst must coalesce into the buffered slot and never block the reader.
		for i := 0; i < 20; i++ {
			if err := os.WriteFile(path, []byte("populated 1\nfrozen 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-events:
		case <-testdeadline.After(2 * time.Second):
			t.Fatalf("cycle %d: burst of modifications delivered nothing", cycle)
		}
		close(stop)
		deadline := time.Now().Add(testdeadline.Wait(2 * time.Second))
		for inotifyFDs(t)[watchFD] {
			if time.Now().After(deadline) {
				t.Fatalf("cycle %d: inotify fd %s not released after stop", cycle, watchFD)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// reapOnPollScope is a fake scope whose FIRST Members() poll is the moment the
// leader is reaped: the poll returns an empty scope and simultaneously flips the
// mocked /proc/<pid>/stat reads to the errno the kernel actually returns for a
// task that disappeared between open() and read(). The initial sample is seeded
// (monitorScopeMembership does not poll for it), so the flip is deterministic:
// sample 1 sees a live member, every later sample sees the reaped one.
type reapOnPollScope struct {
	confineFakeScope
	ref    string
	reaped *atomic.Bool
}

func (s *reapOnPollScope) Reference() string { return s.ref }

func (s *reapOnPollScope) Members() ([]int, error) {
	s.reaped.Store(true)
	return nil, nil
}

// AIRA-112 regression. `read(/proc/<pid>/stat)` returns ESRCH — not ENOENT —
// when a task is reaped between the open and the read, which is exactly what
// happens to a sub-100ms run's leader between two membership samples. Only
// ENOENT satisfies errors.Is(err, os.ErrNotExist), so processLive used to call
// that definitive kernel answer `processUnknown`, the sampler recorded a
// residual Gap for a process it had positive proof was gone, and a run whose
// leader WAS positively observed in cgroup.procs was downgraded from contained
// to unverified. That downgrade is what failed
// TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration intermittently
// (~1-2% of `/bin/sh -c printf ok` launches on this kernel, reproduced with
// -count=200 and instrumented to this exact errno).
func TestAIRA112ReapedLeaderIsNotAResidualGap(t *testing.T) {
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })
	leader := PIDIdentity{PID: 9110, StartTick: 5150, BootID: "boot"}
	readBootIDFn = func() (string, error) { return "boot", nil }
	reaped := &atomic.Bool{}
	readProcStatFn = func(int) ([]byte, error) {
		if reaped.Load() {
			return nil, &fs.PathError{Op: "read", Path: "/proc/9110/stat", Err: syscall.ESRCH}
		}
		return procStatForTest('S', leader.StartTick), nil
	}
	readProcCgroupFn = func(int) ([]byte, error) {
		t.Errorf("a reaped leader must never be probed for a cgroup migration")
		return nil, errors.New("unreachable")
	}
	scope := &reapOnPollScope{ref: "/fake/aira112-scope", reaped: reaped}
	stop := make(chan struct{})
	result := make(chan scopeMonitorResult, 1)
	go monitorScopeMembership(scope, leader, []int{leader.PID}, stop, result)
	close(stop)
	summary := <-result
	if summary.Gap {
		t.Fatalf("a leader the kernel proved gone (ESRCH) was recorded as an unreadable gap: %+v", summary)
	}
	if summary.LeaderMigrated {
		t.Fatalf("a reaped leader was reported migrated: %+v", summary)
	}
	if summary.Escape != nil {
		t.Fatalf("a reaped leader was reported as an escape: %+v", summary)
	}
}

// The other direction, so the ESRCH fix cannot be widened into "any read error
// means dead": a leader whose /proc/<pid>/stat is genuinely unreadable (EACCES
// under hidepid, say) carries NO proof of absence and must still be a gap.
func TestAIRA112UnreadableLeaderStatIsStillAGap(t *testing.T) {
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })
	leader := PIDIdentity{PID: 9111, StartTick: 5151, BootID: "boot"}
	readBootIDFn = func() (string, error) { return "boot", nil }
	blocked := &atomic.Bool{}
	readProcStatFn = func(int) ([]byte, error) {
		if blocked.Load() {
			return nil, &fs.PathError{Op: "read", Path: "/proc/9111/stat", Err: syscall.EACCES}
		}
		return procStatForTest('S', leader.StartTick), nil
	}
	readProcCgroupFn = func(int) ([]byte, error) { return nil, errors.New("hidepid") }
	scope := &reapOnPollScope{ref: "/fake/aira112-scope", reaped: blocked}
	stop := make(chan struct{})
	result := make(chan scopeMonitorResult, 1)
	go monitorScopeMembership(scope, leader, []int{leader.PID}, stop, result)
	close(stop)
	summary := <-result
	if !summary.Gap {
		t.Fatalf("an unreadable leader stat carries no proof of absence and must record a gap: %+v", summary)
	}
	if summary.LeaderMigrated {
		t.Fatalf("an unreadable leader stat was falsely reported migrated: %+v", summary)
	}
}

// processLive's own contract, pinned per errno so the mapping cannot regress
// silently underneath the sampler.
func TestAIRA112ProcessLiveMapsAbsenceErrnosToDead(t *testing.T) {
	oldBoot, oldStat := readBootIDFn, readProcStatFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn = oldBoot, oldStat })
	readBootIDFn = func() (string, error) { return "boot", nil }
	identity := PIDIdentity{PID: 4242, StartTick: 77, BootID: "boot"}
	for _, testCase := range []struct {
		name string
		err  error
		want processLiveness
	}{
		// The kernel's two distinct ways of saying "this pid no longer names a
		// task": ENOENT when the /proc entry is already gone at open(), ESRCH
		// when the task is reaped between the open and the read.
		{name: "enoent-at-open", err: &fs.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.ENOENT}, want: processDead},
		{name: "esrch-at-read", err: &fs.PathError{Op: "read", Path: "/proc/4242/stat", Err: syscall.ESRCH}, want: processDead},
		// Anything else is a read failure, not an absence proof.
		{name: "eacces", err: &fs.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.EACCES}, want: processUnknown},
		{name: "eio", err: &fs.PathError{Op: "read", Path: "/proc/4242/stat", Err: syscall.EIO}, want: processUnknown},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			readProcStatFn = func(int) ([]byte, error) { return nil, testCase.err }
			if got := processLive(identity); got != testCase.want {
				t.Fatalf("processLive(%v) = %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}
