//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

// AIRA-29 anti-INERT tier.
//
// This is the most important test in this change, and it is the only one that
// could fail while every other AIRA-29 test passed. All the others drive
// evaluateAdmitQueue through the admitConfineScan SEAM, so every one of them
// would keep passing against a build whose PRODUCTION scan could never
// establish a reading on a real cgroup tree -- which is exactly how the AIRA-59
// watchdog shipped inert: correct arithmetic that never fired.
//
// So this one builds a real cgroup-v2 slice, puts a real allocating process in
// a real .aira-CONFINE-* scope, runs the production runner.ListConfines scan and
// the production readSliceMemory, and asserts two things the ticket actually
// asked for: that the ledger charge falls from the frozen estimate to the
// kernel's OWN memory.current plus the margin, and that a queued waiter which
// did not fit before the drop is admitted after it. The second half is what
// makes it a utilisation test rather than an arithmetic one -- a charge that
// falls but frees nobody would have been operationally worthless.
//
// verifies: refreshWaiterCharge against a real cgroup tree.

// realConfineScope creates <parent>/.aira-CONFINE-<name>-<pid>-<stamp>, shaped
// so the production parseConfineScopeID accepts it and the production scan
// therefore returns a record for it. The stamp must be canonical lowercase
// base 36 or the scanner omits the scope entirely and this test would silently
// prove nothing.
func realConfineScope(t *testing.T, parent, name string) (scopePath, scopeID string) {
	t.Helper()
	stamp := strconv.FormatInt(time.Now().UnixNano()%(1<<40), 36)
	scopeID = "CONFINE-" + name + "-" + strconv.Itoa(os.Getpid()) + "-" + stamp
	scopePath = filepath.Join(parent, ".aira-"+scopeID)
	if err := os.Mkdir(scopePath, 0o755); err != nil {
		t.Fatal(err)
	}
	return scopePath, scopeID
}

// allocateInScope starts a process that touches allocateBytes of anonymous
// memory, places it in scopePath, and returns once the kernel reports the scope
// holding at least that much. It reads the scope's own memory.current, so the
// test never races its own setup and never asserts against a number it guessed.
func allocateInScope(t *testing.T, scopePath string, allocateBytes int64) {
	t.Helper()
	// perl is not assumed; sh + tr fills a shell variable with anonymous memory
	// and then blocks, which is enough and needs nothing outside coreutils.
	script := "A=$(tr '\\0' 'x' < /dev/zero | head -c " + strconv.FormatInt(allocateBytes, 10) + "); " +
		"printf '%s' \"${A#${A%?}}\" >/dev/null; sleep 300"
	allocator := exec.Command("sh", "-c", script)
	if err := allocator.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = allocator.Process.Kill()
		_, _ = allocator.Process.Wait()
	})
	if err := os.WriteFile(filepath.Join(scopePath, "cgroup.procs"),
		[]byte(strconv.Itoa(allocator.Process.Pid)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place a process into %s: %v", scopePath, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if current, ok := readChargeCgroupInt(scopePath, "memory.current"); ok && current >= allocateBytes {
			return
		}
		if time.Now().After(deadline) {
			current, _ := readChargeCgroupInt(scopePath, "memory.current")
			cgrouptest.SkipOrFailRealCgroup(t,
				"scope %s never reached %d bytes of memory.current (got %d); the allocator may not have run",
				scopePath, allocateBytes, current)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readChargeCgroupInt(dir, file string) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func TestRealCgroupDynamicChargeTracksMemoryCurrentAndFreesAWaiter(t *testing.T) {
	const (
		allocate      = 64 << 20  // what the job really uses
		reserve       = 512 << 20 // what admission estimated for it
		sliceMax      = 640 << 20
		queuedReserve = 256 << 20
	)

	slice := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(slice, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", slice, err)
	}
	// A real, finite slice cap, so the PRODUCTION readSliceMemory has something
	// to read and the admission arithmetic below runs against real numbers.
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(strconv.Itoa(sliceMax)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set memory.max on %s: %v", slice, err)
	}
	scopePath, scopeID := realConfineScope(t, slice, "dyncharge")
	allocateInScope(t, scopePath, allocate)

	now := time.Unix(1_000_000, 0)
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	// admitConfineScan and admitReadMemory are deliberately left at their
	// PRODUCTION defaults (runner.ListConfines and readSliceMemory). Replacing
	// either would put this test back in the seam tier it exists to escape.
	if current, _, _, ok, reason := readSliceMemory(slice); !ok {
		cgrouptest.SkipOrFailRealCgroup(t, "production readSliceMemory cannot read %s: %s", slice, reason)
	} else if current < allocate {
		cgrouptest.SkipOrFailRealCgroup(t,
			"the slice reads %d bytes, below the %d the scope was made to allocate; the tree is not what this test assumes", current, allocate)
	}

	held := &admitWaiter{
		seq: 1, reserve: reserve, state: admitGranted, accounted: true,
		scopeID: scopeID, grantedAt: now.Add(-time.Hour),
	}
	queued := &admitWaiter{
		seq: 2, reserve: queuedReserve, state: admitQueued,
		grantedCh: make(chan struct{}), enqueued: now,
	}
	queue := &sliceQueue{
		path: slice, server: server, waiters: []*admitWaiter{held, queued},
		outstanding: reserve, outstandingJobs: 1,
	}

	// Establish the BEFORE state honestly: with the frozen estimate charged, the
	// queued waiter genuinely does not fit. Without this the "it was admitted"
	// assertion below would prove nothing -- it might have fitted all along.
	beforeAvailable := checkedAvailable(allocate, sliceMax, 0, reserve, 0)
	if beforeAvailable >= queuedReserve {
		t.Fatalf("test arithmetic: %d bytes were available under the frozen reserve, so the %d waiter would have fitted anyway",
			beforeAvailable, int64(queuedReserve))
	}

	server.evaluateAdmitQueue(queue)

	kernelCurrent, ok := readChargeCgroupInt(scopePath, "memory.current")
	if !ok {
		t.Fatal("could not read the scope's memory.current back")
	}
	charge := held.ledgerCharge()
	if !held.chargeTracked {
		t.Fatalf("the production scan never established a charge for %s; the mechanism is INERT against a real tree", scopeID)
	}
	if charge >= reserve {
		t.Fatalf("charge = %d, still at or above the frozen reserve %d -- the charge did not track the real scope", charge, reserve)
	}
	if charge < kernelCurrent {
		t.Fatalf("charge = %d, below the kernel's own memory.current %d -- the ledger must never under-charge a live scope", charge, kernelCurrent)
	}
	// The charge must be the kernel reading plus the margin, not some smaller
	// constant that merely happens to sit under the reserve. A slack of one
	// margin absorbs the scope growing between the scan's read and this one.
	upper := addClamp(kernelCurrent, 2*server.chargeMargin(kernelCurrent, 0))
	if charge > upper {
		t.Fatalf("charge = %d, above memory.current %d plus two margins (%d) -- it is not tracking the real reading",
			charge, kernelCurrent, upper)
	}

	if queued.state != admitGranted {
		t.Fatalf("the queued %d-byte waiter was NOT admitted into the freed reserve (charge=%d, outstanding=%d, slice max=%d)",
			int64(queuedReserve), charge, queue.outstanding, int64(sliceMax))
	}
	if want := charge + queuedReserve; queue.outstanding != want {
		t.Fatalf("outstanding = %d, want %d (the tracked charge plus the new grant)", queue.outstanding, want)
	}
}
