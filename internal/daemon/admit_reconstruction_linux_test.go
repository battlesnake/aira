//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"
)

func TestAdmitReconstructionRealCgroupEvaluationDoesNotDeadlock(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	if err := os.WriteFile(filepath.Join(parent, "memory.max"), []byte(strconv.FormatInt(64<<20, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "parent memory.max is not writable: %v", err)
	}

	scopeID := "CONFINE-reconstruct-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	scopePath := filepath.Join(parent, ".aira-"+scopeID)
	if err := os.Mkdir(scopePath, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create reconstruction scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scopePath, "memory.max"), []byte(strconv.FormatInt(16<<20, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "scope memory.max is not writable: %v", err)
	}
	scopeFD, err := os.OpenFile(scopePath, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("/bin/sleep", "60")
	sleeper.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(scopeFD.Fd())}
	if err := sleeper.Start(); err != nil {
		_ = scopeFD.Close()
		cgrouptest.SkipOrFailRealCgroup(t, "populate reconstruction scope: %v", err)
	}
	_ = scopeFD.Close()
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})

	server := NewServer(Paths{})
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 24 << 20, 0, true, ""
	}
	waiter := &admitWaiter{seq: 1, reserve: 9 << 20, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
	queue := &sliceQueue{path: parent, server: server, waiters: []*admitWaiter{waiter}}
	// Register the queue so a regressed impl that calls activeConfines(path) while
	// holding queue.mu would find it and re-acquire queue.mu → deadlock (caught by the
	// timeout below). Without registration activeConfines returns early on a nil queue
	// and the deadlock would go undetected.
	server.admitQueues[parent] = queue
	done := make(chan struct{})
	go func() {
		server.evaluateAdmitQueue(queue)
		close(done)
	}()
	select {
	case <-done:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("evaluateAdmitQueue deadlocked during real confine scan")
	}
	if queue.adopted != 16<<20 || queue.adoptedJobs != 1 || waiter.state != admitQueued {
		t.Fatalf("adopted=%d jobs=%d waiter=%v, want %d/1/queued", queue.adopted, queue.adoptedJobs, waiter.state, 16<<20)
	}
}
