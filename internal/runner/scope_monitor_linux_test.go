package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
