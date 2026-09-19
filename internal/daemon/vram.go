package daemon

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// AIRA-268. VRAM admission-gating. Agents declare GPU VRAM via `aira confine
// --vram <size>`; the daemon gates admission on real free VRAM as a THIRD
// conjunctive resource dimension (RAM, CPU, VRAM). No VRAM cgroup is ever written
// (there is none on NVIDIA) — this is admission accounting, exactly as `cpu` is.
//
// NVIDIA-only in this build: the reader forks nvidia-smi. s.vramReader is the
// single seam where a future rocm-smi (AMD) reader would be added.

const (
	// defaultVRAMHeadroom leaves room for the display/compositor and other non-AIRA
	// GPU consumers, mirroring the RAM headroom rationale.
	defaultVRAMHeadroom = int64(1) << 30 // 1 GiB
	// defaultVRAMStaleness bounds how old the last good sample may be before the
	// ceiling reads unevaluated (the sampler stopped or hung).
	defaultVRAMStaleness = 10 * time.Second
	// vramSampleCadence is how often the background sampler refreshes the snapshot
	// once VRAM work has been requested.
	vramSampleCadence = 2 * time.Second
)

// vramSnapshot is a value-or-unevaluated aggregate reading of GPU memory (bytes).
type vramSnapshot struct {
	total     int64
	free      int64
	evaluated bool
	sampledAt time.Time
}

// vramAdmitAvailable is the SIGNED VRAM availability. DERIVED, NOT a
// checkedAvailable lookalike: checkedAvailable's `current` is the slice's own
// memory.current (aira-scoped), whereas nvidia-smi `free` is MACHINE-WIDE (the
// desktop + every process). Because per-process VRAM attribution is unavailable,
// the safe worst case subtracts the full declared ledger (outstanding = Σgranted)
// from the effective ceiling min(budget, free−headroom): a slow-ramping job whose
// declared VRAM is not yet reflected in `free` is still charged. effBudget is the
// configured budget, or the physical total when auto-detecting (budget == 0).
// Result is SIGNED — a charge past the ceiling makes the next admission wait,
// never a clamp-at-zero that hides the deficit (mirrors checkedAvailable).
func vramAdmitAvailable(budget, total, free, headroom, outstanding int64) int64 {
	effBudget := budget
	if effBudget <= 0 {
		effBudget = total
	}
	ceiling := effBudget
	if physical := free - headroom; physical < ceiling {
		ceiling = physical
	}
	return ceiling - outstanding
}

// parseNvidiaSmiMemory parses `nvidia-smi --query-gpu=memory.total,memory.free
// --format=csv` output into aggregate BYTES, SUMMING across GPUs. It tolerates a
// header line and a trailing " MiB" unit suffix (nvidia reports MiB), so both
// `--format=csv` and `--format=csv,noheader,nounits` parse. ok is false when no
// GPU data row is present — the caller then reads the ceiling as unevaluated and
// NEVER fabricates a 0 or infinite total.
func parseNvidiaSmiMemory(out []byte) (total, free int64, ok bool) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	rows := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		t, okT := parseMiBField(fields[0])
		f, okF := parseMiBField(fields[1])
		if !okT || !okF {
			// A header row ("memory.total [MiB], memory.free [MiB]") or a malformed
			// line: skip it rather than fail the whole parse.
			continue
		}
		total += t
		free += f
		rows++
	}
	if rows == 0 {
		return 0, 0, false
	}
	return total, free, true
}

// parseMiBField parses one nvidia-smi memory cell ("16303" or "16303 MiB") into
// bytes. ok=false for a header cell or anything non-numeric.
func parseMiBField(field string) (int64, bool) {
	f := strings.TrimSpace(field)
	f = strings.TrimSpace(strings.TrimSuffix(f, "MiB"))
	mib, err := strconv.ParseInt(f, 10, 64)
	if err != nil || mib < 0 {
		return 0, false
	}
	return mib << 20, true // MiB → bytes
}

// readNvidiaSmiVRAM is the default GPU-memory seam: it forks nvidia-smi (~50 ms)
// and parses aggregate total/free. ok=false when nvidia-smi is absent, errors, or
// reports no GPU. NEVER called under queue.mu — only by the sampler goroutine and
// the per-job enqueue bootstrap.
func readNvidiaSmiVRAM() (total, free int64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=memory.total,memory.free", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, false
	}
	return parseNvidiaSmiMemory(out)
}

func (s *Server) readVRAM() (total, free int64, ok bool) {
	if s.vramReader != nil {
		return s.vramReader()
	}
	return readNvidiaSmiVRAM()
}

func (s *Server) vramHeadroomBytes() int64 {
	if s.vramHeadroom > 0 {
		return s.vramHeadroom
	}
	return defaultVRAMHeadroom
}

func (s *Server) vramStalenessDur() time.Duration {
	if s.vramStaleness > 0 {
		return s.vramStaleness
	}
	return defaultVRAMStaleness
}

// vramSampleOnce reads the GPU once (off-lock; forks nvidia-smi via the seam) and
// publishes the snapshot atomically. Called by the sampler goroutine on a ticker
// and once synchronously at each GPU-job enqueue (so the first job has a fresh
// reading before the ticker fires).
func (s *Server) vramSampleOnce() {
	total, free, ok := s.readVRAM()
	s.vramSnap.Store(&vramSnapshot{total: total, free: free, evaluated: ok, sampledAt: s.admitNowTime()})
}

// vramCurrent loads the published snapshot, downgrading it to unevaluated when
// absent or older than the staleness bound (the sampler stopped/hung). No lock,
// no fork — safe from the fit loop under queue.mu.
func (s *Server) vramCurrent() vramSnapshot {
	snap := s.vramSnap.Load()
	if snap == nil || !snap.evaluated {
		return vramSnapshot{}
	}
	if s.admitNowTime().Sub(snap.sampledAt) > s.vramStalenessDur() {
		return vramSnapshot{total: snap.total, free: snap.free, evaluated: false, sampledAt: snap.sampledAt}
	}
	return *snap
}

// vramEffectiveBudget is the configured budget, or the physical total when
// auto-detecting (budget == 0). CLAMPED to the physical total: a misconfigured
// budget larger than the card must not let a job bigger than the card pass the
// enqueue too-large gate and then HOLD forever (physical free can never reach it).
func (s *Server) vramEffectiveBudget(total int64) int64 {
	budget := s.vramBudgetBytes
	if budget <= 0 || (total > 0 && budget > total) {
		return total
	}
	return budget
}

// vramWaiterFitsLocked reports whether a vram>0 waiter fits, reading the atomic
// snapshot (no fork). An unevaluated/stale snapshot returns false → the waiter
// HOLDS (stays queued), never a fabricated fit. queue.mu is held by the caller.
func (s *Server) vramWaiterFitsLocked(vram, outstanding int64) bool {
	snap := s.vramCurrent()
	if !snap.evaluated {
		return false
	}
	return vram <= vramAdmitAvailable(s.vramBudgetBytes, snap.total, snap.free, s.vramHeadroomBytes(), outstanding)
}

// runVRAMSampler refreshes the snapshot on a ticker until ctx is done, but forks
// nvidia-smi ONLY after VRAM has actually been requested (vramEverRequested) — a
// box that never runs GPU work never forks. Started by the daemon's Run loop; a
// bare test Server never starts it (tests call vramSampleOnce / set vramSnap).
func (s *Server) runVRAMSampler(ctx context.Context) {
	ticker := time.NewTicker(vramSampleCadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.vramEverRequested.Load() {
				s.vramSampleOnce()
			}
		}
	}
}
