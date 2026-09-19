package daemon

import (
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// verifies: AIRA-268 (Fable S1/S2) — queue.vramOutstanding actually REACHES the fit,
// and the grant-path rederive SETS it. Two 8G GPU jobs against a 14G budget with
// ample physical free (15G): A is granted, and in the SAME pass its 8G charge holds
// B (avail = min(14,15−1) − 8 = 6 < 8). RAM+CPU are wide open (cpuLedgerServer), so
// ONLY the VRAM ledger can hold B. A grant-path rederive that dropped the vram sum
// (leaving vramOutstanding 0), or a fit passing 0 instead of queue.vramOutstanding,
// would over-admit B — the exact double-grant Fable's S1/S2 mutations survived.
func TestVRAMLedgerHoldsSecondJobInSamePass(t *testing.T) {
	G := vramTestGiB
	now := time.Unix(820_000, 0)
	s := cpuLedgerServer(&now, 4)
	s.vramBudgetBytes = 14 * G
	s.vramHeadroom = G
	s.vramSnap.Store(&vramSnapshot{total: 16 * G, free: 15 * G, evaluated: true, sampledAt: now})

	first := &admitWaiter{seq: 1, reserve: 1 << 20, cpu: 1, vram: 8 * G, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	second := &admitWaiter{seq: 2, reserve: 1 << 20, cpu: 1, vram: 8 * G, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{path: "/slice", server: s, kick: make(chan struct{}, 1), waiters: []*admitWaiter{first, second}}
	s.evaluateAdmitQueue(queue)

	if first.state != admitGranted {
		t.Fatalf("job A (8G) must be granted with 14G available (state=%v)", first.state)
	}
	if second.state != admitQueued {
		t.Fatalf("job B (8G) must be HELD in the same pass by A's 8G VRAM charge (only 6G left) — is queue.vramOutstanding fed by the grant-path rederive AND read at the fit? (state=%v)", second.state)
	}
	if queue.vramOutstanding != 8*G {
		t.Fatalf("vramOutstanding after the pass = %d, want 8G (A granted)", queue.vramOutstanding)
	}
}

// verifies: AIRA-268 (Fable S3) — the release-path rederive drops the vram sum, so a
// freed GPU lease returns its VRAM to the ledger. A release-path rederive that
// assigned `_` for vram would leave vramOutstanding stuck forever.
func TestVRAMOutstandingDropsOnRelease(t *testing.T) {
	G := vramTestGiB
	now := time.Unix(830_000, 0)
	s := cpuLedgerServer(&now, 4)
	s.vramBudgetBytes = 14 * G
	s.vramHeadroom = G
	lease := &admitWaiter{seq: 1, reserve: 1 << 20, cpu: 1, vram: 8 * G, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour)}
	queue := &sliceQueue{path: "/slice", server: s, kick: make(chan struct{}, 1), stop: make(chan struct{}), waiters: []*admitWaiter{lease}, outstanding: 1 << 20, vramOutstanding: 8 * G, outstandingJobs: 1}

	queue.mu.Lock()
	releaseAdmitWaiterLocked(queue, lease)
	got := queue.vramOutstanding
	queue.mu.Unlock()
	if got != 0 {
		t.Fatalf("vramOutstanding = %d after releasing the only GPU lease, want 0 (the release-path rederive must drop the vram sum)", got)
	}
}

// vramAdmitServer builds an admitTestServer whose RAM ceiling is effectively
// unbounded and whose GPU reader is deterministic, so ONLY VRAM decides.
func vramAdmitServer(t *testing.T, budget, total, free int64, gpuOK bool) *Server {
	t.Helper()
	var maximum atomic.Int64
	maximum.Store(1 << 40) // RAM never gates
	server := admitTestServer(&maximum)
	server.vramBudgetBytes = budget
	server.vramHeadroom = vramTestGiB
	server.vramReader = func() (int64, int64, bool) { return total, free, gpuOK }
	return server
}

// verifies: AIRA-268 (Fable S4) — the two enqueue refusals, driven through the REAL
// admitConnection: over-budget → E_ADMIT_VRAM_TOO_LARGE, GPU-unreadable →
// E_ADMIT_VRAM_UNAVAILABLE. Both are client-terminal codes with a valid rejection
// payload, and NEITHER enqueues. Deleting the 18-line refusal block survived a green
// suite before this test existed.
func TestVRAMEnqueueRefusalsAreClientTerminalAndNeverEnqueue(t *testing.T) {
	G := vramTestGiB
	for _, tc := range []struct {
		name     string
		budget   int64
		total    int64
		free     int64
		gpuOK    bool
		vram     int64
		wantCode string
		wantReq  bool // the payload carries Required>0 (too-large) vs unavailable (basis only)
	}{
		{"over budget", 14 * G, 16 * G, 15 * G, true, 20 * G, CodeAdmitVRAMTooLarge, true},
		{"GPU unreadable", 14 * G, 0, 0, false, 10 * G, CodeAdmitVRAMUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := vramAdmitServer(t, tc.budget, tc.total, tc.free, tc.gpuOK)
			serverConn, clientConn := net.Pipe()
			args := validAdmitArgs(1<<20, 1000)
			args["vram"] = tc.vram
			go func() {
				defer serverConn.Close()
				server.admitConnection(serverConn, args)
			}()
			var frame ResponseFrame
			if err := readFrame(clientConn, &frame); err != nil {
				t.Fatalf("read response: %v", err)
			}
			if frame.OK || frame.Code != tc.wantCode {
				t.Fatalf("a %s vram request must be REFUSED with %s, got OK=%v code=%q", tc.name, tc.wantCode, frame.OK, frame.Code)
			}
			// The runner treats these as terminal ONLY when the payload is valid; pin the
			// contract validRunnerAdmitRejection enforces so the client refuses rather
			// than falling through to an ungoverned launch.
			var rej admitRejection
			if err := json.Unmarshal(frame.Data, &rej); err != nil {
				t.Fatalf("rejection payload did not unmarshal: %v", err)
			}
			if strings.TrimSpace(rej.Basis) == "" {
				t.Fatalf("rejection payload has empty Basis: %+v", rej)
			}
			if tc.wantReq && !(rej.Required > 0 && rej.Ceiling >= 0) {
				t.Fatalf("too-large payload {Required:%d Ceiling:%d} must satisfy Required>0 && Ceiling>=0", rej.Required, rej.Ceiling)
			}
			server.admitRegistryMu.Lock()
			queues := len(server.admitQueues)
			server.admitRegistryMu.Unlock()
			if queues != 0 {
				t.Fatalf("a refused vram request must NEVER enqueue; %d queue(s) created", queues)
			}
			_ = clientConn.Close()
		})
	}
}

// verifies: AIRA-268 (Fable S4 arm d) — a VRAM job that fits the budget and reads an
// evaluated ceiling but exceeds physical-free is NOT refused: it takes the normal
// contended path (E_ADMIT_SATURATED in non-blocking mode) and DOES enqueue. This is
// the held-not-refused distinction: a VRAM refusal here would be wrong.
func TestVRAMContendedJobIsHeldNotRefused(t *testing.T) {
	G := vramTestGiB
	server := vramAdmitServer(t, 14*G, 16*G, 3*G, true) // only 3G free → 2G available
	serverConn, clientConn := net.Pipe()
	args := validAdmitArgs(1<<20, 0) // max_wait_ms=0 → non-blocking
	args["vram"] = 10 * G
	go func() {
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if frame.Code == CodeAdmitVRAMTooLarge || frame.Code == CodeAdmitVRAMUnavailable {
		t.Fatalf("a merely physical-free-contended VRAM job must NOT get a VRAM refusal code; got %q — it should be held/saturated", frame.Code)
	}
	if frame.Code != CodeAdmitSaturated {
		t.Fatalf("a non-blocking VRAM-contended job = %q, want %s (held, then saturated)", frame.Code, CodeAdmitSaturated)
	}
	server.admitRegistryMu.Lock()
	queues := len(server.admitQueues)
	server.admitRegistryMu.Unlock()
	if queues != 1 {
		t.Fatalf("a contended (not refused) VRAM job must ENQUEUE; %d queue(s)", queues)
	}
	_ = clientConn.Close()
}

// verifies: AIRA-268 (Fable S5) — a vram>0 request ARMS the sampler (vramEverRequested),
// so the background ticker begins refreshing the ceiling; without it a queued GPU job
// reads a stale bootstrap sample after the staleness bound and holds indefinitely.
// Deleting the Store(true) survived a green suite before this test.
func TestVRAMRequestArmsTheSampler(t *testing.T) {
	G := vramTestGiB
	server := vramAdmitServer(t, 14*G, 16*G, 15*G, true)
	if server.vramEverRequested.Load() {
		t.Fatal("precondition: the sampler must not be armed before any vram request")
	}
	serverConn, clientConn := net.Pipe()
	args := validAdmitArgs(1<<20, 0)
	args["vram"] = 10 * G
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !server.vramEverRequested.Load() {
		t.Fatal("a vram>0 request must ARM the sampler (vramEverRequested); the ticker forks nvidia-smi only after this")
	}
	_ = clientConn.Close()
	<-done
}

// verifies: AIRA-268 (nit 7) — vramEffectiveBudget is CLAMPED to the physical total, so
// a misconfigured budget larger than the card cannot let a job bigger than the card
// pass the too-large gate and then hold forever (physical free can never reach it).
func TestVRAMEffectiveBudgetClampedToCard(t *testing.T) {
	G := vramTestGiB
	// Budget 24G on a 16G card → the effective budget is the 16G total.
	if got := (&Server{vramBudgetBytes: 24 * G}).vramEffectiveBudget(16 * G); got != 16*G {
		t.Fatalf("a misconfigured 24G budget on a 16G card = %d, want the card total 16G (clamped)", got)
	}
	// A configured budget below the card still wins.
	if got := (&Server{vramBudgetBytes: 14 * G}).vramEffectiveBudget(16 * G); got != 14*G {
		t.Fatalf("a 14G budget below the card = %d, want 14G", got)
	}
}

// verifies: AIRA-268 (Fable S6) — validateAdmitArgs accepts the OPTIONAL `vram` field
// (structural: non-negative) and threads it onto the request. Removing `vram` from the
// whitelist (so an unknown-field error fires) reds the accept case; dropping the
// `vram < 0` check reds the negative case.
func TestValidateAdmitArgsParsesVRAM(t *testing.T) {
	G := vramTestGiB
	req, err := validateAdmitArgs(map[string]any{
		"slice": "slice", "reserve": int64(1 << 20), "vram": int64(4 * G),
		"max_wait_ms": int64(1000), "signature": "", "pinned": true,
	}, 60_000)
	if err != nil {
		t.Fatalf("a valid vram field must be accepted, got %v", err)
	}
	if req.vram != 4*G {
		t.Fatalf("parsed req.vram = %d, want 4G", req.vram)
	}
	if _, err := validateAdmitArgs(map[string]any{
		"slice": "slice", "reserve": int64(1 << 20), "vram": int64(-1),
		"max_wait_ms": int64(1000), "signature": "", "pinned": true,
	}, 60_000); err == nil {
		t.Fatal("a negative vram must be rejected structurally")
	}
}
