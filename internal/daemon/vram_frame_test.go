package daemon

import (
	"testing"
	"time"

	"aira/internal/runner"
)

// verifies: AIRA-269 — fillVRAMFrame stamps the confine-list VRAM display frame
// from the LOCKED ledger figures (outstanding/jobs) + a LOCK-FREE read of the
// sampler's atomic snapshot, with the four-way honesty discriminator the operator
// must tell apart. The byte total/free are published ONLY under set/stale, never
// fabricated for a no-GPU / no-GPU-work reading.
func TestFillVRAMFrame(t *testing.T) {
	G := vramTestGiB
	now := time.Unix(2_000_000, 0)
	newServer := func() *Server {
		s := &Server{vramBudgetBytes: 14 * G, vramHeadroom: G, vramStaleness: 10 * time.Second}
		s.admitNow = func() time.Time { return now }
		return s
	}

	// set: a fresh evaluated snapshot → every field meaningful.
	set := newServer()
	set.vramSnap.Store(&vramSnapshot{total: 16 * G, free: 3 * G, evaluated: true, sampledAt: now})
	var r runner.ConfineSliceReserve
	set.fillVRAMFrame(&r, 8*G, 2)
	if r.VRAMState != runner.VRAMStateSet {
		t.Fatalf("state=%q, want set", r.VRAMState)
	}
	if r.VRAMTotalBytes != 16*G || r.VRAMFreeBytes != 3*G || r.VRAMBudgetBytes != 14*G ||
		r.VRAMOutstandingBytes != 8*G || r.VRAMJobs != 2 || r.VRAMHeadroomBytes != G {
		t.Fatalf("set frame = %+v", r)
	}

	// no-gpu-work: the sampler was never armed (nil snapshot) → NOT a failure.
	work := newServer()
	var rw runner.ConfineSliceReserve
	work.fillVRAMFrame(&rw, 0, 0)
	if rw.VRAMState != runner.VRAMStateNoGPUWork {
		t.Fatalf("state=%q, want no-gpu-work", rw.VRAMState)
	}
	if rw.VRAMTotalBytes != 0 || rw.VRAMFreeBytes != 0 {
		t.Fatalf("no-gpu-work must publish no card figures, got total=%d free=%d", rw.VRAMTotalBytes, rw.VRAMFreeBytes)
	}

	// no-gpu: the sampler ran and could not read a device (evaluated=false).
	nogpu := newServer()
	nogpu.vramSnap.Store(&vramSnapshot{evaluated: false, sampledAt: now})
	var rn runner.ConfineSliceReserve
	nogpu.fillVRAMFrame(&rn, 0, 0)
	if rn.VRAMState != runner.VRAMStateNoGPU {
		t.Fatalf("state=%q, want no-gpu", rn.VRAMState)
	}
	if rn.VRAMTotalBytes != 0 {
		t.Fatalf("no-gpu must publish no fabricated total, got %d", rn.VRAMTotalBytes)
	}

	// stale: a good sample gone old → still carry the last-good card figures, marked stale.
	stale := newServer()
	stale.vramSnap.Store(&vramSnapshot{total: 16 * G, free: 5 * G, evaluated: true, sampledAt: now.Add(-time.Hour)})
	var rs runner.ConfineSliceReserve
	stale.fillVRAMFrame(&rs, G, 1)
	if rs.VRAMState != runner.VRAMStateStale {
		t.Fatalf("state=%q, want stale", rs.VRAMState)
	}
	if rs.VRAMTotalBytes != 16*G || rs.VRAMFreeBytes != 5*G {
		t.Fatalf("stale must carry the last-good card figures, got total=%d free=%d", rs.VRAMTotalBytes, rs.VRAMFreeBytes)
	}
}
