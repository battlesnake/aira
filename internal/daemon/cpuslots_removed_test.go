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
