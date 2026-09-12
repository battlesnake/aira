//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifies: S17 — the `aira worker-peak` CLI relay is fully removed from
// production Go source. The pool peak sample now flows over the retained
// `confine-report` verb (this package's own confineReport handler, unchanged),
// reached through a CLI face spelled the same as the wire verb instead of a
// separately-named relay.
//
// This is the S17 mutation pin the compiler cannot provide: a half-deletion
// that left a stray `worker-peak` dispatch arm, a dead comment, or the old
// parse/run function names would compile cleanly and leave a second, unused
// transport for the same frame -- exactly the drift `ReportPeakSample`'s own
// comment warns against. Same shape as S6's TestS6CPUSlotsGovernorFullyRemovedFromSource.
//
// It walks non-test .go source only. supervisor.py (go:embedded into the
// binary) is S17's own file to retarget and is verified by its own Python
// tests; a .go-only walk never sees it.
func TestS17WorkerPeakRelayFullyRemovedFromSource(t *testing.T) {
	tokens := []string{
		"worker-peak", "WorkerPeak", "worker_peak",
		"parseWorkerPeakArgs", "runWorkerPeakCommand",
	}
	// Walk the library (internal/) and the faces (cmd/), relative to this
	// package dir (internal/daemon), exactly as the S6 cpuslots guard does.
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
		t.Fatalf("the worker-peak relay is still referenced in production source, "+
			"but S17 must remove it entirely (the sample now flows over confine-report): %v", offenders)
	}
}
