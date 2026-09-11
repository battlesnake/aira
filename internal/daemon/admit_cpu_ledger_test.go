package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// S5 — CPU as the second ledger resource, PER SLICE (parallel to RAM). The lease
// reserve is the vector {ram, cpu}; the CPU ceiling is 2×NumCPU (integer, NO cgroup
// read, NO cpu.max); admit is conjunctive (RAM AND CPU); a CPU release wakes THIS
// slice's CPU-blocked waiters via the same per-queue signal RAM uses.
//
// CPU is per-slice (D1, resolved to per-slice) on the one-slice (aira.slice)
// assertion — cores are machine-wide, so concurrent slices would need a machine-wide
// sum; see the cpuCeiling / cpuOutstanding comments. These tests pin the core count
// with the cpuCoreCounter seam so the ceiling is deterministic, and set RAM
// effectively unbounded so only CPU gates unless a test says otherwise.

// s5Lease is one granted lease's declared resource vector for the fixtures below.
type s5Lease struct{ reserve, cpu int64 }

// cpuLedgerServer is a server whose ceiling is a fixed 2×numCPU (via readCPUCores)
// and whose RAM is effectively unbounded, so CPU is the only gate a test exercises
// unless it overrides admitReadMemory. Dev (real-cgroup) mode: with current=0 the
// dev RAM floor never bites.
func cpuLedgerServer(now *time.Time, numCPU int) *Server {
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return *now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = noConfinesScan
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.readCPUCores = func() int { return numCPU }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 50, 0, true, ""
	}
	return server
}

// buildS5Queue is the CPU sibling of buildS4Queue: it seeds a queue with granted
// (accounted) leases, each carrying a {reserve, cpu} vector, plus one queued
// newcomer, runs a single evaluation pass, and returns the newcomer. It sets the
// queue's per-slice cpuOutstanding to the held cores (just as buildS4Queue sets
// outstanding for RAM), so a directly-built queue behaves as one grown through real
// grants.
func buildS5Queue(server *Server, path string, now time.Time, held []s5Lease, newcomer s5Lease) *admitWaiter {
	var waiters []*admitWaiter
	var outstanding, cpuOutstanding int64
	seq := int64(0)
	for _, lease := range held {
		seq++
		waiters = append(waiters, &admitWaiter{
			seq: seq, reserve: lease.reserve, cpu: lease.cpu, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour),
		})
		outstanding += lease.reserve
		cpuOutstanding += lease.cpu
	}
	seq++
	queued := &admitWaiter{
		seq: seq, reserve: newcomer.reserve, cpu: newcomer.cpu, state: admitQueued,
		grantedCh: make(chan struct{}), enqueued: now,
	}
	waiters = append(waiters, queued)
	queue := &sliceQueue{
		path: path, server: server, kick: make(chan struct{}, 1),
		waiters: waiters, outstanding: outstanding, cpuOutstanding: cpuOutstanding, outstandingJobs: len(held),
	}
	server.evaluateAdmitQueue(queue)
	return queued
}

// verifies: S5 — the CPU ceiling is 2×NumCPU, not 1×. Granted leases sum to exactly
// NumCPU cores; a one-core newcomer then fits under the 2× ceiling (avail =
// 2N − N = N ≥ 1) and is GRANTED. Under a mutated 1× ceiling (avail = N − N = 0) it
// would be refused, so this test reds if cpuCeiling returns 1×NumCPU.
func TestS5CPUCeilingIsTwiceNumCPU(t *testing.T) {
	const numCPU = 2 // ceiling = 4 cores
	now := time.Unix(800_000, 0)
	server := cpuLedgerServer(&now, numCPU)

	// Two granted one-core leases => Σcores = 2 = NumCPU. Under the 2× ceiling of 4,
	// a one-core newcomer has 2 cores of room; under a 1× ceiling of 2 it has none.
	newcomer := buildS5Queue(server, "/slice", now,
		[]s5Lease{{reserve: 1 << 20, cpu: 1}, {reserve: 1 << 20, cpu: 1}},
		s5Lease{reserve: 1 << 20, cpu: 1})
	if newcomer.state != admitGranted {
		t.Fatalf("a one-core newcomer must fit under the 2×NumCPU ceiling with %d cores held (avail = 2N−N = N); a 1×NumCPU ceiling would refuse it (state=%v)", numCPU, newcomer.state)
	}
}

// verifies: S5 — the fit is CONJUNCTIVE: admission needs EVERY resource to fit.
// Both directions are pinned: (a) RAM fits but CPU is over → block; (b) CPU fits but
// RAM is over → block. A non-conjunctive fit consulting only one resource would
// grant one of these.
func TestS5ConjunctiveFitBlocksOnEitherResource(t *testing.T) {
	const numCPU = 2 // ceiling = 4 cores
	now := time.Unix(810_000, 0)

	// (a) RAM fits (huge ceiling), CPU over: 4 cores held fill the ceiling, so a
	// one-core newcomer cannot fit even though RAM is wide open.
	t.Run("ram_fits_cpu_over", func(t *testing.T) {
		server := cpuLedgerServer(&now, numCPU)
		newcomer := buildS5Queue(server, "/slice", now,
			[]s5Lease{{reserve: 1 << 20, cpu: 4}},
			s5Lease{reserve: 1 << 20, cpu: 1})
		if newcomer.state != admitQueued {
			t.Fatalf("CPU is full (4/4) so the newcomer must BLOCK even though RAM fits (state=%v)", newcomer.state)
		}
	})

	// (b) CPU fits (idle), RAM over: a modest slice ceiling is filled by a held RAM
	// lease that declares 0 cores, so CPU is wide open but RAM is not.
	t.Run("cpu_fits_ram_over", func(t *testing.T) {
		server := cpuLedgerServer(&now, numCPU)
		const maximum = 64 * gib
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, maximum, 0, true, ""
		}
		// A 60 GiB held lease with 0 cores => RAM avail = 4 GiB, CPU avail = 4 cores.
		newcomer := buildS5Queue(server, "/slice", now,
			[]s5Lease{{reserve: 60 * gib, cpu: 0}},
			s5Lease{reserve: 16 * gib, cpu: 1})
		if newcomer.state != admitQueued {
			t.Fatalf("RAM is over (needs 16 GiB, 4 GiB free) so the newcomer must BLOCK even though CPU is idle (state=%v)", newcomer.state)
		}
	})
}

// verifies: S5 — N = 2×NumCPU+1 one-core workers: the first 2×NumCPU fit and the
// LAST one blocks (CPU avail = 0), then a per-slice CPU release admits it. Pins the
// ceiling from ABOVE (the ledger is full at exactly 2×NumCPU) and the per-slice
// CPU-release wake, and asserts the release kicks THIS queue (the existing signal).
func TestS5NPlusOneOneCoreWorkersLastBlocksUntilRelease(t *testing.T) {
	const numCPU = 2 // ceiling = 4 cores; 2N+1 = 5 one-core workers
	now := time.Unix(820_000, 0)
	server := cpuLedgerServer(&now, numCPU)
	server.admitPollInterval = time.Hour // only an explicit signal can wake the queue

	// 2N = 4 granted one-core leases fill the ceiling; the 5th (the newcomer) blocks.
	var waiters []*admitWaiter
	seq := int64(0)
	for i := 0; i < 2*numCPU; i++ {
		seq++
		waiters = append(waiters, &admitWaiter{
			seq: seq, reserve: 1 << 20, cpu: 1, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour),
		})
	}
	seq++
	newcomer := &admitWaiter{
		seq: seq, reserve: 1 << 20, cpu: 1, state: admitQueued,
		grantedCh: make(chan struct{}), enqueued: now,
	}
	waiters = append(waiters, newcomer)
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: waiters, outstanding: int64(len(waiters)-1) << 20, cpuOutstanding: int64(2 * numCPU), outstandingJobs: len(waiters) - 1,
	}
	registerAdmitQueue(server, queue)

	server.evaluateAdmitQueue(queue)
	if newcomer.state != admitQueued {
		t.Fatalf("the (2N+1)th one-core worker must BLOCK while 2N=%d cores are held (state=%v)", 2*numCPU, newcomer.state)
	}

	// Drain the kick so the post-release assertion cannot pass on a stale signal.
	select {
	case <-queue.kick:
	default:
	}

	// Release one held lease through the real discharge path (re-derive the
	// per-slice CPU sum); afterAdmitRelease kicks THIS queue.
	server.releaseAdmitWaiter(queue, waiters[0])
	select {
	case <-queue.kick:
	default:
		t.Fatal("a CPU release did not kick its own queue; a CPU-blocked waiter would sleep until the poll tick")
	}

	// One core is now free, so the last worker is admitted.
	server.evaluateAdmitQueue(queue)
	if newcomer.state != admitGranted {
		t.Fatalf("after a one-core release the blocked worker must be admitted (state=%v)", newcomer.state)
	}
	// AIRA-149 honesty: a CPU refusal latches `observed`, never `none-observed` —
	// this slice's own cores were held, so something was in the way. (The waiter was
	// granted above; its latch from the blocked pass must still be observed.)
	if got := admitContentionToken(newcomer.contention); got != "observed" {
		t.Fatalf("a CPU refusal must latch `observed` (own cores held), got %q; never a fabricated RAM-solitude `none-observed`", got)
	}
}

// verifies: S5 / P1 (Fable) — CPU is PER SLICE, so a pruned queue's stale evaluator
// pass cannot erase a same-path successor's CPU charge (the over-admit the
// machine-wide ledger bred). Q1 on /P is released and pruned; Q2, a NEW queue on the
// SAME path, holds the whole ceiling; Q1's stale pass runs; Q2 must still gate its
// own slice on its own intact cpuOutstanding. (Green by construction now — each
// queue's CPU sum is self-contained; this guards against ever regressing to a shared
// path-keyed CPU ledger.)
func TestS5PrunedQueueStalePassDoesNotEraseSuccessorCPU(t *testing.T) {
	const numCPU = 2 // ceiling = 4 cores
	now := time.Unix(840_000, 0)
	server := cpuLedgerServer(&now, numCPU)
	server.admitPollInterval = time.Hour

	// Q1 on /P: one granted one-core lease; release it -> afterAdmitRelease prunes Q1.
	lease1 := &admitWaiter{seq: 1, reserve: 1 << 20, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour)}
	q1 := &sliceQueue{path: "/P", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: []*admitWaiter{lease1}, outstanding: 1 << 20, cpuOutstanding: 1, outstandingJobs: 1}
	registerAdmitQueue(server, q1)
	server.evaluateAdmitQueue(q1)
	server.releaseAdmitWaiter(q1, lease1)
	server.admitRegistryMu.Lock()
	_, still := server.admitQueues["/P"]
	server.admitRegistryMu.Unlock()
	if still {
		t.Fatal("precondition: q1 should have been pruned")
	}

	// Q2, the successor on the SAME path, holds the whole ceiling (4 cores) and has a
	// queued one-core newcomer that must block.
	lease2 := &admitWaiter{seq: 1, reserve: 1 << 20, cpu: 4, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour)}
	newcomer := &admitWaiter{seq: 2, reserve: 1 << 20, cpu: 1, state: admitQueued,
		grantedCh: make(chan struct{}), enqueued: now}
	q2 := &sliceQueue{path: "/P", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: []*admitWaiter{lease2, newcomer}, outstanding: 2 << 20, cpuOutstanding: 4, outstandingJobs: 1}
	registerAdmitQueue(server, q2)
	server.evaluateAdmitQueue(q2)
	if newcomer.state != admitQueued {
		t.Fatalf("precondition: Q2's newcomer must block while Q2 holds 4/4 cores (state=%v)", newcomer.state)
	}

	// The pruned Q1's evaluator runs ONE more pass (its pending kick raced the closed
	// stop channel). With a per-slice CPU sum it touches only Q1 (empty), never Q2.
	server.evaluateAdmitQueue(q1)

	// Q2's charge is intact and still gating: re-evaluate Q2; the newcomer must STILL
	// block. A shared path-keyed CPU ledger (the deleted machine-wide design) would
	// have had Q1's stale pass zero /P's charge, admitting this newcomer past 4 cores.
	server.evaluateAdmitQueue(q2)
	if newcomer.state != admitQueued {
		t.Fatalf("Q2's one-core newcomer must STILL block after Q1's stale pass — its per-slice 4-core charge must survive a same-path prune+recreate (state=%v)", newcomer.state)
	}
	if q2.cpuOutstanding != 4 {
		t.Fatalf("Q2's per-slice cpuOutstanding = %d after Q1's stale pass, want 4 (unclobbered)", q2.cpuOutstanding)
	}
}

// verifies: S5 / P2 — a request for more cores than the box can EVER provide
// (cpu > 2×NumCPU) is refused fail-fast, NEVER enqueues, and with a CLIENT-TERMINAL
// code. Driven through real admitConnection. Asserts the code is E_ADMIT_TOO_LARGE
// (in the runner's terminal pre-payload set — NOT CodeProtocol, which would fall
// through to the ungoverned flock fallback), that the structured rejection payload
// satisfies the client's validRunnerAdmitRejection contract (Required>0, Ceiling>=0,
// Basis!="") so the client refuses rather than degrading, and that no queue exists.
func TestS5CPUOverCeilingRefusedNeverEnqueues(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(1 << 40)
	server := admitTestServer(&maximum)
	server.readCPUCores = func() int { return 2 } // ceiling = 4 cores

	serverConn, clientConn := net.Pipe()
	// cpu = 5 > ceiling 4: impossible on this box.
	args := map[string]any{
		"slice": "slice", "reserve": int64(1 << 20), "cpu": int64(5),
		"max_wait_ms": int64(1000), "signature": "", "pinned": true,
	}
	go func() {
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()

	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if frame.OK {
		t.Fatalf("cpu=5 on a 2-core box (ceiling 4) must be REFUSED, got an OK grant")
	}
	if frame.Code != CodeAdmitTooLarge {
		t.Fatalf("cpu-over-ceiling refusal code = %q, want %q (client-terminal; CodeProtocol would flock-fallback ungoverned)", frame.Code, CodeAdmitTooLarge)
	}
	// The client treats E_ADMIT_TOO_LARGE as terminal ONLY when the structured
	// payload is valid; an invalid/absent body falls through to fail() -> flock. Pin
	// the contract validRunnerAdmitRejection enforces so this stays wire-safe.
	var rej admitRejection
	if err := json.Unmarshal(frame.Data, &rej); err != nil {
		t.Fatalf("rejection payload did not unmarshal: %v", err)
	}
	if !(rej.Required > 0 && rej.Ceiling >= 0 && strings.TrimSpace(rej.Basis) != "") {
		t.Fatalf("rejection payload {Required:%d Ceiling:%d Basis:%q} does not satisfy the client's terminal-refusal contract (Required>0 && Ceiling>=0 && Basis!=\"\"); the client would flock-fallback", rej.Required, rej.Ceiling, rej.Basis)
	}
	server.admitRegistryMu.Lock()
	queues := len(server.admitQueues)
	server.admitRegistryMu.Unlock()
	if queues != 0 {
		t.Fatalf("an impossible-cpu request must NEVER enqueue; %d queue(s) were created", queues)
	}
	_ = clientConn.Close()
}

// verifies: S5 / P3 — a negative cpu is rejected structurally by validateAdmitArgs
// (CodeProtocol, never enqueued). Dropping the `cpu < 0` check would let a negative
// core count through; this reds it.
func TestS5NegativeCPURejected(t *testing.T) {
	_, err := validateAdmitArgs(map[string]any{
		"slice": "slice", "reserve": int64(1 << 20), "cpu": int64(-1),
		"max_wait_ms": int64(1000),
	}, admitWaitCeilingMs)
	if err == nil {
		t.Fatal("a negative cpu must be rejected by validateAdmitArgs, got nil error")
	}
	if code := admitErrorCode(err); code != CodeProtocol {
		t.Fatalf("negative-cpu rejection code = %q, want %q", code, CodeProtocol)
	}
}

// verifies: S5 — NO cpu.max is written (or even referenced by cgroup path name)
// anywhere in production. CPU is admission-accounting only; sharing stays
// cpu.weight-based and the kernel time-shares. A regression reaching for a hard CPU
// bandwidth cap would name the "cpu.max" cgroup file, so this fails if that quoted
// literal appears in any non-test source file under internal/. (Prose mentions of
// cpu.max in comments are unquoted and do not match. This guards the quoted literal;
// it is not an exhaustive proof — an assembled path would evade it.)
func TestS5NoCPUMaxReferenceInSource(t *testing.T) {
	root := ".." // internal/
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), `"cpu.max"`) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/ source: %v", err)
	}
	if len(offenders) != 0 {
		t.Fatalf("cpu.max is referenced by name in production source, but S5 CPU accounting must write no cpu.max: %v", offenders)
	}
}
