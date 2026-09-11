//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifies: S6 — the AIRA-64 cpuslots flock CPU slot-governor is fully removed
// from production source. CPU is now governed by the admission ledger (S5's
// per-slice cpuOutstanding vs the 2×NumCPU ceiling), so the old flock slot-dir
// governor and every symbol, wire field and comment that named it must be gone.
//
// This is the S6 mutation pin the compiler cannot provide: a half-deletion that
// left the CPUSlots wire field on a struct, the cpu_slots render block, or a
// dead comment referencing the governor would COMPILE cleanly and ship a
// governance surface the code no longer backs — exactly how AIRA-59 once
// shipped an inert subsystem. The same shape as S5's TestS5NoCPUMaxReferenceInSource.
//
// It walks non-test .go source only. supervisor.py (go:embedded into the binary)
// keeps its own cpu_slots reader and is S16's to retire — a .py file, so this
// .go-only walk never sees it, and the embed directive names a path, not the token.
func TestS6CPUSlotsGovernorFullyRemovedFromSource(t *testing.T) {
	// The residual tokens a compiling half-deletion could leave behind. Symbol
	// references to the deleted cpuslots.go internals (cpuSlotsDecide,
	// scanSliceWorkerScopes, …) fail to compile and need no guard; these are the
	// ones that survive compilation — a struct field, a JSON tag, an env-var
	// name, or prose.
	tokens := []string{
		"cpuSlots", "CPUSlots", "cpu_slots", "cpu-slots", "cpuslots",
		"lastGrantAt", "CPU_RESERVE",
	}
	// Walk the library (internal/) and the faces (cmd/), relative to this
	// package dir (internal/daemon), exactly as the S5 cpu.max guard does.
	roots := []string{"..", "../../cmd"}
	offenders := map[string][]string{}
	for _, root := range roots {
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
			body := string(data)
			for _, token := range tokens {
				if strings.Contains(body, token) {
					offenders[path] = append(offenders[path], token)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s source: %v", root, err)
		}
	}
	if len(offenders) != 0 {
		t.Fatalf("the AIRA-64 cpuslots governor is still referenced in production source, "+
			"but S6 must remove it entirely (CPU is governed by the admission ledger, S5): %v", offenders)
	}
}

// verifies: S6 — the INTERIM CPU window. S6 deleted the flock governor that bounded
// aitest pytest workers' CPU concurrency, but S5's ledger CPU charge is on the
// `aira confine` / `confine-reserve` admission path, NOT on worker-admit:
// evaluateWorkerAdmit charges no CPU term, and aitest workers reach the daemon via
// worker-admit (not confine-reserve — the embedded per-test governor that issued it was
// retired in AIRA-33). So between S6 and S15 an aitest worker carries NO CPU charge at
// all (RAM stays bounded by the aggregate guard). This is plan-sanctioned and documented
// in the S6 code comment + the BUILT (S6) plan record.
//
// This test makes that window a CHECKED fact rather than a silent hole: worker_admit.go
// references none of the ledger's CPU symbols. S15 (which rebuilds worker-admit onto the
// signed ledger and charges CPU there) will introduce one of them and RED this guard —
// which is the signal to DELETE this test, the §S15.3 inversion pattern. It is NOT a
// claim that no CPU bound is desirable; it is the honest record that there is none yet.
func TestS6InterimWorkerAdmitCPUUnbounded(t *testing.T) {
	data, err := os.ReadFile("worker_admit.go")
	if err != nil {
		t.Fatalf("read worker_admit.go: %v", err)
	}
	body := string(data)
	// The ledger's CPU-governance symbols (S5). Their ABSENCE from worker_admit.go is the
	// interim window. The S6 explanatory comment in that file deliberately avoids these
	// literal tokens, so the guard keys on a real call/field, not prose about its own
	// absence.
	for _, token := range []string{"cpuOutstanding", "cpuCeiling(", "cpuFits"} {
		if strings.Contains(body, token) {
			t.Fatalf("worker_admit.go now references %q: worker-admit appears to charge CPU against the ledger. "+
				"If this is S15 closing the S6->S15 interim window, DELETE this witness test (it has done its job). "+
				"If not, the interim-window documentation in this file and worker_admit.go is now stale and must be corrected.", token)
		}
	}
}
