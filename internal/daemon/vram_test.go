package daemon

import (
	"testing"
	"time"
)

const vramTestMiB = int64(1) << 20
const vramTestGiB = int64(1) << 30

// verifies: AIRA-268 — parse aggregate GPU memory from the REAL nvidia-smi output
// shapes (golden fixtures captured on an RTX 5080, 2026-09-19), summing multi-GPU,
// and returning unevaluated (ok=false) for no-GPU / garbage rather than a
// fabricated 0.
func TestParseNvidiaSmiMemory(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		total, free int64
		ok          bool
	}{
		{"noheader nounits (the real --format=csv,noheader,nounits)", "16303, 3960\n", 16303 * vramTestMiB, 3960 * vramTestMiB, true},
		{"with header and MiB units (--format=csv)", "memory.total [MiB], memory.free [MiB]\n16303 MiB, 3960 MiB\n", 16303 * vramTestMiB, 3960 * vramTestMiB, true},
		{"multi-GPU SUMS across lines", "16303, 3960\n8192, 8000\n", (16303 + 8192) * vramTestMiB, (3960 + 8000) * vramTestMiB, true},
		{"empty (no GPU) is unevaluated", "", 0, 0, false},
		{"no-devices error line is unevaluated", "No devices were found\n", 0, 0, false},
		{"a single malformed field is unevaluated", "16303,\n", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, free, ok := parseNvidiaSmiMemory([]byte(tc.in))
			if ok != tc.ok || total != tc.total || free != tc.free {
				t.Fatalf("parseNvidiaSmiMemory(%q) = (%d,%d,%v), want (%d,%d,%v)", tc.in, total, free, ok, tc.total, tc.free, tc.ok)
			}
		})
	}
}

// verifies: AIRA-268 — the DERIVED fit formula min(budget, free−headroom) −
// outstanding, SIGNED. Each term is exercised so a mutation dropping any one reds.
func TestVRAMAdmitAvailable(t *testing.T) {
	G := vramTestGiB
	cases := []struct {
		name                                     string
		budget, total, free, headroom, out, want int64
	}{
		// GPU mostly free: the budget binds. min(14, 16-1=15→ capped by budget 14) - 0.
		{"budget binds when the GPU is free", 14 * G, 16 * G, 16 * G, G, 0, 14 * G},
		// Desktop hogging: only 3G free, so the physical floor binds, NOT the 14G budget.
		{"physical-free floor binds when the desktop hogs VRAM", 14 * G, 16 * G, 3 * G, G, 0, 2 * G},
		// The ledger term: 4G already granted → 14 - 4 = 10 available.
		{"outstanding ledger is subtracted", 14 * G, 16 * G, 16 * G, G, 4 * G, 10 * G},
		// Auto-detect (budget 0) uses the physical total as the budget.
		{"auto-detect budget uses the total", 0, 16 * G, 16 * G, G, 0, 15 * G},
		// Over-subscribed → SIGNED negative (the next admission waits), never clamped to 0.
		{"signed negative when over-subscribed", 14 * G, 16 * G, 3 * G, G, 5 * G, -3 * G},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vramAdmitAvailable(tc.budget, tc.total, tc.free, tc.headroom, tc.out); got != tc.want {
				t.Fatalf("vramAdmitAvailable(budget=%d,total=%d,free=%d,headroom=%d,out=%d) = %d, want %d",
					tc.budget, tc.total, tc.free, tc.headroom, tc.out, got, tc.want)
			}
		})
	}
}

// verifies: AIRA-268 — the ledger sum. rederiveLedgerLocked sums vram over granted
// && accounted waiters only. A mutation dropping the vram sum term reds this.
func TestRederiveLedgerSumsVRAM(t *testing.T) {
	G := vramTestGiB
	q := &sliceQueue{waiters: []*admitWaiter{
		{state: admitGranted, accounted: true, reserve: G, cpu: 1, vram: 4 * G},
		{state: admitGranted, accounted: true, reserve: G, cpu: 1, vram: 2 * G},
		{state: admitQueued, vram: 8 * G},                // not granted → excluded
		{state: admitGranted, accounted: false, vram: G}, // not accounted → excluded
	}}
	_, _, vram, jobs := rederiveLedgerLocked(q)
	if vram != 6*G {
		t.Fatalf("vramOutstanding = %d, want 6 GiB (only the two granted+accounted waiters)", vram)
	}
	if jobs != 2 {
		t.Fatalf("jobs = %d, want 2", jobs)
	}
}

// verifies: AIRA-268 — vramSampleOnce + vramCurrent value-or-unevaluated with a
// staleness bound. A failed read is unevaluated; a stale good sample downgrades.
func TestVRAMSampleAndCurrent(t *testing.T) {
	G := vramTestGiB
	now := time.Unix(1_000_000, 0)
	s := &Server{vramReader: func() (int64, int64, bool) { return 16 * G, 4 * G, true }, vramStaleness: 10 * time.Second}
	s.admitNow = func() time.Time { return now }
	s.vramSampleOnce()
	if snap := s.vramCurrent(); !snap.evaluated || snap.total != 16*G || snap.free != 4*G {
		t.Fatalf("fresh snapshot = %+v, want evaluated {16G,4G}", snap)
	}
	// A failed read publishes an unevaluated snapshot.
	s.vramReader = func() (int64, int64, bool) { return 0, 0, false }
	s.vramSampleOnce()
	if s.vramCurrent().evaluated {
		t.Fatal("a failed nvidia-smi read must publish an UNEVALUATED snapshot, never a fabricated 0")
	}
	// A good sample older than the staleness bound downgrades to unevaluated.
	s.vramReader = func() (int64, int64, bool) { return 16 * G, 4 * G, true }
	s.vramSampleOnce()
	s.admitNow = func() time.Time { return now.Add(time.Hour) }
	if s.vramCurrent().evaluated {
		t.Fatal("a sample older than the staleness bound must read UNEVALUATED (the sampler stopped/hung)")
	}
}

// verifies: AIRA-268 — vramWaiterFitsLocked: the fit decision the grant loop uses.
// Covers fits, physical-free HOLD, post-restart-floor HOLD (empty ledger), the
// unevaluated HOLD, and the stale HOLD. vram==0 is handled by the caller's
// short-circuit, not here.
func TestVRAMWaiterFitsLocked(t *testing.T) {
	G := vramTestGiB
	now := time.Unix(1_000_000, 0)
	newServer := func(budget, total, free int64, evaluated bool, age time.Duration) *Server {
		s := &Server{vramBudgetBytes: budget, vramHeadroom: G, vramStaleness: 10 * time.Second}
		s.admitNow = func() time.Time { return now }
		s.vramSnap.Store(&vramSnapshot{total: total, free: free, evaluated: evaluated, sampledAt: now.Add(-age)})
		return s
	}
	// Budget 14G, GPU free (15G free): a 10G job fits (avail = min(14,14)-0 = 14).
	if s := newServer(14*G, 16*G, 15*G, true, 0); !s.vramWaiterFitsLocked(10*G, 0) {
		t.Fatal("a 10G job must fit when 14G is available")
	}
	// Desktop hogging (3G free): a 10G job is HELD (avail = min(14,2)-0 = 2).
	if s := newServer(14*G, 16*G, 3*G, true, 0); s.vramWaiterFitsLocked(10*G, 0) {
		t.Fatal("a 10G job must be HELD when only 2G is physically free (the desktop hogs VRAM)")
	}
	// POST-RESTART FLOOR: the ledger is empty (outstanding 0) yet a big job is still
	// HELD by the physical-free floor ALONE — the sole post-restart protection.
	if s := newServer(14*G, 16*G, 3*G, true, 0); s.vramWaiterFitsLocked(10*G, 0) {
		t.Fatal("post-restart: with an empty ledger, the physical-free floor alone must hold a 10G job when 2G is free")
	}
	// Unevaluated ceiling → HOLD (never a fabricated fit).
	if s := newServer(14*G, 16*G, 15*G, false, 0); s.vramWaiterFitsLocked(1*G, 0) {
		t.Fatal("an unevaluated ceiling must HOLD a vram job, never fabricate a fit")
	}
	// Stale snapshot → HOLD.
	if s := newServer(14*G, 16*G, 15*G, true, time.Hour); s.vramWaiterFitsLocked(1*G, 0) {
		t.Fatal("a stale snapshot must HOLD a vram job")
	}
}

// verifies: AIRA-268 — the effective budget: configured wins, else auto-detect (total).
func TestVRAMEffectiveBudget(t *testing.T) {
	G := vramTestGiB
	if got := (&Server{vramBudgetBytes: 14 * G}).vramEffectiveBudget(16 * G); got != 14*G {
		t.Fatalf("configured budget = %d, want 14G", got)
	}
	if got := (&Server{}).vramEffectiveBudget(16 * G); got != 16*G {
		t.Fatalf("auto-detect budget = %d, want the total 16G", got)
	}
}

// verifies: AIRA-268 — the FIT WIRING end to end through evaluateAdmitQueue. This
// pins the `&& vramFits` hop that the vramWaiterFitsLocked unit test cannot reach:
// a mutation dropping `&& vramFits` from the conjunctive fit would GRANT the
// over-physical-free newcomer here and red this test. RAM and CPU are wide open
// (cpuLedgerServer), so ONLY VRAM can hold or grant.
func TestVRAMFitWiredIntoTheGrantLoop(t *testing.T) {
	G := vramTestGiB
	now := time.Unix(820_000, 0)
	newQueue := func(server *Server, vram int64) *admitWaiter {
		queued := &admitWaiter{seq: 1, reserve: 1 << 20, cpu: 1, vram: vram, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
		queue := &sliceQueue{path: "/slice", server: server, kick: make(chan struct{}, 1), waiters: []*admitWaiter{queued}}
		server.evaluateAdmitQueue(queue)
		return queued
	}
	base := func() *Server {
		s := cpuLedgerServer(&now, 4) // RAM + CPU effectively unbounded
		s.vramBudgetBytes = 14 * G
		s.vramHeadroom = G
		s.vramStaleness = 10 * time.Second
		return s
	}

	// Desktop hogging: only 3G free → 2G available → a 10G newcomer HOLDS.
	held := base()
	held.vramSnap.Store(&vramSnapshot{total: 16 * G, free: 3 * G, evaluated: true, sampledAt: now})
	if w := newQueue(held, 10*G); w.state != admitQueued {
		t.Fatalf("a 10G VRAM job must HOLD when only 2G is available, even though RAM/CPU fit (state=%v) — is `&& vramFits` wired into the fit?", w.state)
	}

	// VRAM free: 15G free → 14G available → the same 10G newcomer is GRANTED.
	free := base()
	free.vramSnap.Store(&vramSnapshot{total: 16 * G, free: 15 * G, evaluated: true, sampledAt: now})
	if w := newQueue(free, 10*G); w.state != admitGranted {
		t.Fatalf("a 10G VRAM job must be GRANTED when 14G is available (state=%v)", w.state)
	}

	// A vram==0 job is ungated: granted regardless of a tight/absent VRAM snapshot.
	none := base()
	none.vramSnap.Store(&vramSnapshot{total: 16 * G, free: 0, evaluated: true, sampledAt: now})
	if w := newQueue(none, 0); w.state != admitGranted {
		t.Fatalf("a non-GPU (vram==0) job must be GRANTED regardless of VRAM pressure (state=%v)", w.state)
	}

	// An unevaluated ceiling HOLDS a vram>0 job even when the queue is otherwise idle.
	unevaluated := base()
	unevaluated.vramSnap.Store(&vramSnapshot{evaluated: false})
	if w := newQueue(unevaluated, 1*G); w.state != admitQueued {
		t.Fatalf("a vram>0 job must HOLD under an unevaluated ceiling (state=%v)", w.state)
	}
}
