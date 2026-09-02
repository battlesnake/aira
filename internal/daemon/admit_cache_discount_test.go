package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvaluateAdmitQueueDiscountsOnlyReclaimableFileLRU(t *testing.T) {
	for _, test := range []struct {
		name                  string
		headroom, reclaimable int64
		wantGrant             bool
	}{
		{name: "cache grant", reclaimable: 80, wantGrant: true},
		{name: "raw gate", reclaimable: 0},
		{name: "negative reader gate", reclaimable: -1},
		{name: "cache grant with headroom", headroom: 10, reclaimable: 80, wantGrant: true},
		{name: "raw gate with headroom", headroom: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			var maximum atomic.Int64
			maximum.Store(100)
			server := admitTestServer(&maximum)
			server.admitSliceHeadroomBase = test.headroom
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
				return 90, 100, test.reclaimable, true, ""
			}
			waiter := &admitWaiter{seq: 1, reserve: 30, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
			queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

			server.evaluateAdmitQueue(queue)

			if test.wantGrant {
				waitAdmitGrant(t, waiter)
			} else {
				requireAdmitQueued(t, waiter)
			}
		})
	}
}

func TestParseSliceMemoryStatUsesOnlyFileLRU(t *testing.T) {
	data := []byte("file 1048576\nshmem 524288\ninactive_file 40\nactive_file 2\nmalformed\ninactive_anon\n")
	if reclaimable, ok := parseSliceMemoryStat(data); !ok || reclaimable != 42 {
		t.Fatalf("parseSliceMemoryStat=(%d,%v), want 42,true", reclaimable, ok)
	}
	for _, data := range [][]byte{
		[]byte("inactive_file 40\n"),
		[]byte("active_file 2\n"),
		[]byte("inactive_file nope\nactive_file 2\n"),
		[]byte("inactive_file -1\nactive_file 2\n"),
	} {
		if reclaimable, ok := parseSliceMemoryStat(data); ok || reclaimable != 0 {
			t.Fatalf("parseSliceMemoryStat(%q)=(%d,%v), want 0,false", data, reclaimable, ok)
		}
	}
}

func TestCheckedAvailableCacheDiscountGuards(t *testing.T) {
	if got := checkedAvailable(90, 100, -1, 0, 0); got != 10 {
		t.Fatalf("negative reclaimable available=%d, want raw-current 10", got)
	}
	if got := checkedAvailable(90, 100, 200, 30, 0); got != 70 {
		t.Fatalf("reclaimable above current available=%d, want 70", got)
	}
}

func TestReadSliceMemoryReclaimableIntegrationAndDegrade(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "90\n")
	write("memory.max", "100\n")
	write("memory.stat", "file 80\nshmem 40\ninactive_file 60\nactive_file 20\n")
	if current, maximum, reclaimable, ok, reason := readSliceMemory(dir); !ok || reason != "" || current != 90 || maximum != 100 || reclaimable != 80 {
		t.Fatalf("read=(%d,%d,%d,%v,%q), want 90,100,80,true,empty", current, maximum, reclaimable, ok, reason)
	}

	if err := os.Remove(filepath.Join(dir, "memory.stat")); err != nil {
		t.Fatal(err)
	}
	previousOutput := log.Writer()
	defer log.SetOutput(previousOutput)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	sliceMemoryStatDegradeOnce = sync.Once{}
	defer func() { sliceMemoryStatDegradeOnce = sync.Once{} }()
	for range 2 {
		current, maximum, reclaimable, ok, reason := readSliceMemory(dir)
		if !ok || reason != "" || current != 90 || maximum != 100 || reclaimable != 0 {
			t.Fatalf("soft-degrade read=(%d,%d,%d,%v,%q), want 90,100,0,true,empty", current, maximum, reclaimable, ok, reason)
		}
	}
	if got := strings.Count(logs.String(), "memory.stat"); got != 1 {
		t.Fatalf("soft-degrade logs=%q count=%d, want one", logs.String(), got)
	}

	if err := os.Remove(filepath.Join(dir, "memory.current")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, reason := readSliceMemory(dir); ok || reason != "read-error" {
		t.Fatalf("missing current ok=%v reason=%q, want false/read-error", ok, reason)
	}
}

func TestReadSliceMemoryReportsUnboundedReasonLiteral(t *testing.T) {
	// Pins the exact "unbounded" reason string against its real producer
	// (Fable re-gate round 3): every other test of this specific reason
	// (the Python classifier, evaluateWorkerAdmit's own unevaluated tests,
	// the CLI-stderr boundary test) exercises it via a hand-written stub
	// or an invented reason, never against readSliceMemory itself. A
	// future rename of this literal (e.g. to "uncapped") would leave every
	// one of those other suites green while silently breaking the real
	// wire message supervisor.py's classifier matches on -- reintroducing
	// the round-2 P1 indefinite-retry hang this literal exists to prevent.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("90\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("max\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, reason := readSliceMemory(dir); ok || reason != "unbounded" {
		t.Fatalf("memory.max=max: ok=%v reason=%q, want false/%q", ok, reason, "unbounded")
	}
}
