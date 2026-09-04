package daemon

import (
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// writeWorkerChild builds one cgroupfs-shaped child directory. memoryMax is
// written verbatim, so "max" (uncapped) and malformed values are expressible.
func writeWorkerChild(t *testing.T, outerScope, name, memoryMax string) {
	t.Helper()
	child := filepath.Join(outerScope, name)
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if memoryMax == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(child, "memory.max"), []byte(memoryMax+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// verifies: AIRA-39 — the scan sums exactly the `.aira-worker-*` children's
// memory.max, charges non-numeric suffixes too, and excludes the deliberately
// uncapped supervisor scope and anything that is not a worker child.
func TestScanWorkerScopeChildrenSumsCappedWorkerChildrenOnly(t *testing.T) {
	outer := t.TempDir()
	writeWorkerChild(t, outer, ".aira-worker-1", "400")
	writeWorkerChild(t, outer, ".aira-worker-2", "600")
	writeWorkerChild(t, outer, ".aira-worker-foo", "50")
	// The supervisor scope is deliberately uncapped and is accounted for
	// separately by the aggregate guard's supervisor-RSS term. Charging it here
	// (or erroring on its "max") would break every real run.
	writeWorkerChild(t, outer, ".aira-supervisor", "max")
	writeWorkerChild(t, outer, ".aira-CONFINE-something", "999999")
	if err := os.WriteFile(filepath.Join(outer, "memory.max"), []byte("8000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	children, err := scanWorkerScopeChildren(outer)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if children.committed != 1050 || children.count != 3 {
		t.Fatalf("children=%+v, want committed=1050 count=3 (400+600+50; supervisor, confine scope and interface files excluded)", children)
	}
	if children.maxIndex != 2 {
		t.Fatalf("maxIndex=%d, want 2: a non-numeric suffix is charged but must not perturb id allocation", children.maxIndex)
	}
}

// verifies: AIRA-39 — a reading the scan cannot establish is an ERROR, which the
// evaluator turns into "unevaluated". A fabricated zero here is exactly the
// over-admit AIRA-39 reports.
func TestScanWorkerScopeChildrenErrorsOnUnestablishableCap(t *testing.T) {
	for _, test := range []struct {
		name      string
		memoryMax string
	}{
		{name: "uncapped", memoryMax: "max"},
		{name: "malformed", memoryMax: "not-a-number"},
		{name: "negative", memoryMax: "-1"},
		// An existing directory with NO memory.max means the memory controller
		// is not delegated. ENOENT alone must NOT be read as "the child
		// vanished" (found by Sol plan-review): skipping it would under-charge.
		{name: "capless (controller not delegated)", memoryMax: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			outer := t.TempDir()
			writeWorkerChild(t, outer, ".aira-worker-1", "400")
			writeWorkerChild(t, outer, ".aira-worker-2", test.memoryMax)
			children, err := scanWorkerScopeChildren(outer)
			if err == nil {
				t.Fatalf("scan returned %+v with no error; an unestablishable child cap must never be silently dropped", children)
			}
			if !strings.Contains(err.Error(), ".aira-worker-2") {
				t.Fatalf("err=%v, want the offending child named", err)
			}
		})
	}
}

// vanishedDirEntry names a child that no longer exists — the state
// supervisor.py's _retire_worker leaves behind between os.ReadDir and the
// per-child read. It cannot be produced deterministically through ReadDir,
// which is why sumWorkerScopeChildren takes the entry list.
type vanishedDirEntry struct{ name string }

func (e vanishedDirEntry) Name() string               { return e.name }
func (e vanishedDirEntry) IsDir() bool                { return true }
func (e vanishedDirEntry) Type() fs.FileMode          { return fs.ModeDir }
func (e vanishedDirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

// verifies: AIRA-39 — a child that has genuinely gone away between the listing
// and the read is SKIPPED (it charges nothing because it no longer exists),
// while a child that is still there but has no memory.max is an ERROR. The two
// look identical at the memory.max read (both ENOENT) and are told apart by a
// re-stat; conflating them either under-charges or wedges every run.
func TestSumWorkerScopeChildrenSkipsVanishedChildButNotACaplessOne(t *testing.T) {
	outer := t.TempDir()
	writeWorkerChild(t, outer, ".aira-worker-1", "400")
	entries, err := os.ReadDir(outer)
	if err != nil {
		t.Fatal(err)
	}

	withVanished := append(append([]os.DirEntry{}, entries...), vanishedDirEntry{name: ".aira-worker-9"})
	children, err := sumWorkerScopeChildren(outer, withVanished)
	if err != nil {
		t.Fatalf("a vanished child must be skipped, not reported as an error: %v", err)
	}
	if children.committed != 400 || children.count != 1 {
		t.Fatalf("children=%+v, want only the surviving 400-byte child charged", children)
	}

	writeWorkerChild(t, outer, ".aira-worker-2", "")
	if entries, err = os.ReadDir(outer); err != nil {
		t.Fatal(err)
	}
	if _, err = sumWorkerScopeChildren(outer, entries); err == nil {
		t.Fatal("a child that still exists but has no memory.max was skipped; only a genuinely vanished child may be skipped")
	}
}

// verifies: AIRA-39, and the backlog-remediation plan's tooling item 3 ("ledger
// drift from the kernel object it tracks"). The property, over randomised
// interleavings of grant / external create / retire / daemon restart:
//
//	a GRANT never leaves Σ(caps of the worker scopes that actually exist) plus
//	the supervisor's footprint above the outer scope's ceiling, whenever it was
//	within the ceiling before that grant.
//
// That is Goal 2 — exceeding the ceiling is what lets the outer scope's
// memory.oom.group kill a whole run. The "whenever it was within before"
// qualifier is load-bearing and not a weakening: an EXTERNAL create (a stale
// client, a leftover, another tool) can put the tree over the ceiling on its
// own, and no admission decision can undo that. What the daemon must guarantee
// is that IT never adds to an over-committed tree, and never creates the
// over-commitment itself — both of which this asserts.
func TestWorkerAdmitCommittedNeverBelowSumOfExistingCappedChildren(t *testing.T) {
	const (
		ceiling       = 10000
		supervisorRSS = 500
	)
	seed := time.Now().UnixNano()
	random := rand.New(rand.NewSource(seed))
	t.Logf("seed=%d", seed)

	tree := newWorkerScopeTree()
	server := NewServer(Paths{})
	tree.install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, ceiling)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(
		map[string]int64{runner.WorkerScopeChildPath("/outer", "supervisor"): supervisorRSS})

	externalNames := []string{".aira-worker-ext-a", ".aira-worker-ext-b", ".aira-worker-777"}
	live := map[string]int64{}
	total := func() int64 {
		children, err := tree.scan("/outer")
		if err != nil {
			t.Fatal(err)
		}
		return children.committed + supervisorRSS
	}
	grants, denials := 0, 0
	for step := 0; step < 400; step++ {
		before := total()
		granted := false
		switch random.Intn(10) {
		case 0, 1:
			// A worker retires: supervisor.py reaps it, then rmdirs its scope.
			for name := range live {
				tree.remove("/outer", name)
				delete(live, name)
				break
			}
		case 2:
			// The daemon restarts: every byte of in-memory ledger state is
			// gone, the tree is not. This is AIRA-39 itself.
			server = NewServer(Paths{})
			tree.install(server)
			server.workerAdmitHeadroom = 0
			server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, ceiling)
			server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(
				map[string]int64{runner.WorkerScopeChildPath("/outer", "supervisor"): supervisorRSS})
		case 3:
			// A worker scope appears that this daemon never granted (a stale
			// client, another job, a leftover). Non-numeric names included, so
			// an EEXIST collision cannot be what catches it.
			name := externalNames[random.Intn(len(externalNames))]
			if _, exists := live[name]; !exists {
				size := int64(random.Intn(2000) + 100)
				tree.put("/outer", name, size)
				live[name] = size
			}
		default:
			size := int64(random.Intn(3000) + 100)
			response, proceed := server.evaluateWorkerAdmit(t.Context(), workerAdmitRequest{
				jobID: "job-" + strconv.Itoa(random.Intn(3)), outerScope: "/outer", estimatedBytes: size,
			})
			if !proceed {
				t.Fatalf("step %d: evaluation abandoned unexpectedly", step)
			}
			switch response.State {
			case "granted":
				live[workerScopeChildPrefix+response.WorkerID] = response.MemoryMax
				granted, grants = true, grants+1
			case "denied", "unevaluated":
				denials++
			default:
				t.Fatalf("step %d: unexpected state %q", step, response.State)
			}
		}

		// The invariant, recomputed from the tree itself rather than from
		// anything the daemon believes about it.
		if after := total(); granted && before <= ceiling && after > ceiling {
			t.Fatalf("step %d: a GRANT took Σ(existing worker caps)+supervisor from %d to %d, past the outer ceiling %d (seed=%d). The outer scope's memory.oom.group can now kill the whole run",
				step, before, after, ceiling, seed)
		}
		if granted && before > ceiling {
			t.Fatalf("step %d: a grant was issued while the tree was ALREADY over the ceiling (%d > %d, seed=%d): the daemon must never add to an over-committed scope",
				step, before, ceiling, seed)
		}
	}
	// Guard against a vacuous pass: a run that never granted, or never denied,
	// would satisfy the property without exercising it.
	if grants == 0 || denials == 0 {
		t.Fatalf("degenerate run: grants=%d denials=%d (seed=%d)", grants, denials, seed)
	}
}
