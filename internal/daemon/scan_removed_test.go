package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifies: S14 — the periodic cgroup scan and its scan-derived lease-release
// path are fully removed from production source. After S14 the ONLY lease-release
// paths are socket-EOF compare-and-release (releaseAdmitWaiterLockedAnchored) and
// the operator `confine --kill`, plus the physical-reap stale-lease backstop
// (ReapScopeIfEmpty of an empty scope). Emptiness is derived from the signed
// ledger (sliceProvablyEmpty == Σleases==0), never from a ground-truth cgroup
// probe.
//
// This is the S14 mutation the compiler cannot provide: a half-deletion that left
// the scan seam, the scopeSeen/scopeVanished transition, the drain-abort setter,
// or the scan-derived `dischargeVanishedStaleLease` reclaim would COMPILE cleanly
// and re-introduce a third ground-truth-probe lease-release path — the exact
// silent-regression class S14 is the runner-up risk for. Same shape as S6's
// TestS6CPUSlotsGovernorFullyRemovedFromSource.
//
// It walks non-test .go source only.
func TestS14ScanAndVanishedLeaseReleaseRemovedFromSource(t *testing.T) {
	// Unambiguous identifiers of the deleted scan / vanished-reclaim machinery.
	// Each fails to compile if referenced from a deleted definition, but a struct
	// field, a dotted field access, a JSON tag, a const or prose could survive a
	// half-deletion — these are those survivors. None is a substring of a KEPT
	// symbol: in particular the retained const admitConfineScanIntervalDefault is
	// matched by none of them (the seam is caught by the dotted ".admitConfineScan"
	// and the "func (s *Server) confineScan(" definition, never the bare const).
	tokens := []string{
		".admitConfineScan",             // the deleted scan seam field access (field + Interval field)
		"func (s *Server) confineScan(", // the deleted scan entry-point method
		"scopeSeen",
		"scopeVanished",
		"scanFailingSince",
		"adoptedScanFailed",
		"adoptedAt",
		"liveScopes", // covers liveScopes and liveScopesKnown
		"dischargeVanishedStaleLease",
		"admitOutcomeExclusiveUnestablished",
		"admitExclusiveEstablishGrace",
		"vanishedJobs", "VanishedJobs", "vanishedBytes", "VanishedBytes",
	}
	// Walk the library (internal/) and the faces (cmd/), relative to this package
	// dir (internal/daemon), exactly as the S6 cpuslots guard does.
	roots := []string{"..", "../../cmd"}
	offenders := map[string][]string{}
	walk := func(root string, apply func(path, body string)) {
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
			apply(path, string(data))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s source: %v", root, err)
		}
	}
	for _, root := range roots {
		walk(root, func(path, body string) {
			for _, token := range tokens {
				if strings.Contains(body, token) {
					offenders[path] = append(offenders[path], token)
				}
			}
		})
	}
	if len(offenders) != 0 {
		t.Fatalf("S14 must remove the periodic scan and its scan-derived lease release entirely, "+
			"but these symbols still appear in production source: %v", offenders)
	}

	// The scan's ground-truth probe itself: runner.ListConfines legitimately stays
	// in confine_manage.go (the operator `confine --list` verb) and in the runner's
	// own list internals, so a package-wide ban would false-fail. The DELETED sites
	// are the daemon's admission scan (admit.go) and its shim seam (shim.go): those
	// two files must no longer probe the kernel for cgroup scopes at all.
	for _, name := range []string{"admit.go", "shim.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), "ListConfines") {
			t.Fatalf("%s still calls runner.ListConfines: the periodic admission scan (a ground-truth "+
				"probe that fed the emptiness gate and the vanished-lease reclaim) must be gone; "+
				"emptiness is now derived from the ledger", name)
		}
	}
}
