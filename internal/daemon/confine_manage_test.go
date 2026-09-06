package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

func writeConfineDaemonScope(t *testing.T, slice, scopeID, events string) string {
	t.Helper()
	path := filepath.Join(slice, ".aira-"+scopeID)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"cgroup.events": events, "cgroup.procs": "", "memory.current": "4096\n", "memory.max": "8192\n", "cgroup.kill": "",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// verifies: a confine-kill that returns a RETAINED-lease outcome
// (not-launched / kill-unconfirmed) does NOT release the reservation — only a
// confirmed "killed" reaches releaseActiveConfine. A handler that released
// unconditionally would free a still-running job's reservation and re-open the
// #67 over-admission this guards. The prior mid-launch test asserted this against
// the runner's own closer (which the killer never touches — architecturally
// vacuous); this drives the real daemon kill→release decision.
func TestConfineKillRetainedOutcomesDoNotReleaseReservation(t *testing.T) {
	setup := func(t *testing.T) (*Server, string, *sliceQueue, *admitWaiter) {
		slice := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
		queue := &sliceQueue{path: slice, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
		// AIRA-52: ownership is carried by the scope id itself, so the fixture
		// must mint the owner-bearing form the launcher now produces.
		id := "CONFINE-held-5101-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@session-a"
		waiter := &admitWaiter{seq: 1, reserve: 64, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), enqueued: time.Now(), scopeID: id, name: "held", owner: "session-a"}
		queue.waiters = []*admitWaiter{waiter}
		queue.outstanding, queue.outstandingJobs = 64, 1
		server.admitQueues[slice] = queue
		return server, id, queue, waiter
	}
	killReq := func(id string) core.Request {
		return core.Request{Verb: "confine-kill", Args: map[string]any{"slice": "test.slice", "selector": id, "owner": "session-a"}}
	}
	t.Run("not-launched-retains", func(t *testing.T) {
		server, id, queue, waiter := setup(t)
		writeConfineDaemonScope(t, queue.path, id, "populated 0\n") // empty → not-launched
		resp := server.confineManagement(context.Background(), killReq(id))
		if resp.Code != runner.CodeConfineNotLaunched {
			t.Fatalf("resp=%+v, want not-launched", resp)
		}
		if queue.outstanding != 64 || queue.outstandingJobs != 1 || waiter.state != admitGranted {
			t.Fatalf("retained outcome released reservation: outstanding=%d jobs=%d state=%v", queue.outstanding, queue.outstandingJobs, waiter.state)
		}
	})
	t.Run("unconfirmed-retains", func(t *testing.T) {
		server, id, queue, waiter := setup(t)
		path := writeConfineDaemonScope(t, queue.path, id, "populated 1\n")
		_ = os.Remove(filepath.Join(path, "cgroup.kill"))
		if err := os.Mkdir(filepath.Join(path, "cgroup.kill"), 0o755); err != nil { // write fails → unconfirmed
			t.Fatal(err)
		}
		resp := server.confineManagement(context.Background(), killReq(id))
		if resp.Code != runner.CodeConfineKillUnconfirmed {
			t.Fatalf("resp=%+v, want kill-unconfirmed", resp)
		}
		if queue.outstanding != 64 || waiter.state != admitGranted {
			t.Fatalf("unconfirmed released reservation: outstanding=%d state=%v", queue.outstanding, waiter.state)
		}
	})
}

func TestConfineRegistryLifetimeFreshLookupAndExactlyOnceRelease(t *testing.T) {
	server := NewServer(Paths{})
	path := "/slice"
	queue := &sliceQueue{path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	waiter := &admitWaiter{
		seq: 1, reserve: 64, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), enqueued: time.Now(),
		scopeID: "CONFINE-job-123-abc", name: "job", owner: "session-a",
	}
	queue.waiters = []*admitWaiter{waiter}
	queue.outstanding, queue.outstandingJobs = 64, 1
	server.admitQueues[path] = queue

	entries := server.activeConfines(path)
	// AIRA-52: the registry now carries the scope id ALONE. Name and owner are
	// decoded from the id itself, which is on disk and survives a restart, so
	// there is no freshConfineOwner lookup into daemon memory to make any more.
	if len(entries) != 1 || entries[0] != (runner.ConfineRegistryEntry{ScopeID: waiter.scopeID}) {
		t.Fatalf("entries=%+v", entries)
	}

	server.releaseActiveConfine(path, waiter.scopeID) // daemon confirmed-kill path
	server.releaseAdmitWaiter(queue, waiter)          // later supervisor conn-close path
	if queue.outstanding != 0 || queue.outstandingJobs != 0 || waiter.state != admitReleased {
		t.Fatalf("outstanding=%d jobs=%d state=%v", queue.outstanding, queue.outstandingJobs, waiter.state)
	}
	if entries := server.activeConfines(path); len(entries) != 0 {
		t.Fatalf("registry retained released entry: %+v", entries)
	}
}

func TestConfineRegistryRejectsDuplicateScopeID(t *testing.T) {
	server := NewServer(Paths{})
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	request := admitRequest{scopeID: "CONFINE-job-123-abc", name: "job", owner: "session-a"}
	queue, _, code, err := server.enqueueResolvedConfineAdmit("/slice", 1, "pinned:client", 100, request)
	if err != nil || code != "" {
		t.Fatalf("first enqueue code=%q err=%v", code, err)
	}
	defer func() { queue.stopOnce.Do(func() { close(queue.stop) }) }()
	if _, _, code, err := server.enqueueResolvedConfineAdmit("/slice", 1, "pinned:client", 100, request); err == nil || code != CodeProtocol {
		t.Fatalf("duplicate code=%q err=%v", code, err)
	}
}

func TestConfineListSliceReserveSummary(t *testing.T) {
	const (
		maximum     = int64(16 << 30)
		granted     = int64(3 << 30)
		adopted     = int64(4 << 30)
		jobs        = 2
		adoptedJobs = 3
		base        = int64(2 << 30)
		supervisor  = int64(64 << 20)
	)
	// AIRA-68: the two connection-held jobs are REAL waiters of the two real
	// populations — one scope-backed `aira confine` job and one scope-less
	// `aira confine-reserve` reservation — rather than hand-set counters with an
	// empty waiter list. That earlier fixture was itself internally inconsistent
	// (counters claiming two jobs the ledger's own waiter list did not contain),
	// which is precisely the defect ResidualJobs/ResidualBytes now detect; keeping
	// it would have baked a fabricated inconsistency into the expectation.
	const scopeBackedBytes, reservationBytes = int64(2 << 30), int64(1 << 30)
	// AIRA-127's system frame, and the slice reading it is drawn against. The
	// memory reader below returns a non-zero memory.current so that a build which
	// dropped the copy-out (publishing a fabricated zero) is caught.
	const (
		systemMemTotal     = int64(48 << 30)
		systemMemAvailable = int64(20 << 30)
		sliceCurrent       = int64(5 << 30)
		sliceReclaimable   = int64(1 << 30)
		sliceHigh          = int64(14 << 30)
	)
	// withSystemFrame stamps the AIRA-127 system/slice frame onto an arm's
	// expectation. One definition, so a new wire field cannot be added and then
	// silently left unasserted in two arms out of three.
	//
	// The ceiling subsystem is OFF in these fixtures, so CeilingEffectiveBytes is
	// an ABSENCE (zero) rather than the raw maximum: publishing the maximum there
	// would draw a throttle marker on an unthrottled slice.
	withSystemFrame := func(want runner.ConfineSliceReserve) runner.ConfineSliceReserve {
		want.SystemMemTotalBytes, want.SystemMemAvailableBytes = systemMemTotal, systemMemAvailable
		want.SliceCurrentBytes, want.SliceReclaimableBytes = sliceCurrent, sliceReclaimable
		want.SliceMaxBytes = maximum
		want.SliceHighBytes, want.SliceHighState = sliceHigh, runner.ConfineSliceHighSet
		return want
	}
	setup := func(t *testing.T) (*Server, string) {
		t.Helper()
		path := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
		server.admitSliceHeadroomBase = base
		server.admitSliceHeadroomSupervisor = supervisor
		// AIRA-127: PIN the system frame. Without these seams the expectations below
		// would carry whatever this host's /proc/meminfo happens to say, so a correct
		// build would go red on a different machine — the same class of defect the
		// frozen clock below fixes for the reservation row's age.
		server.shimReadMemTotal = func() (int64, bool) { return systemMemTotal, true }
		server.shimReadMemAvailable = func() (int64, bool, string) { return systemMemAvailable, true, "" }
		// A REAL memory.high in the fixture slice, so the default reader's parse is
		// what produces the "set" state below rather than an injected constant. The
		// aggregate arm deliberately leaves the file absent and pins "unevaluated",
		// which is the state a build that fabricated a zero soft limit would break.
		if err := os.WriteFile(filepath.Join(path, "memory.high"), []byte(strconv.FormatInt(sliceHigh, 10)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// AIRA-108: FREEZE the clock for this unit fixture. The reservation row's
		// age is `now - grantedAt`, so a real clock plus a fixed grantedAt makes
		// the expected HeldMS depend on how long the test took to reach the
		// snapshot — a correct build goes red on a 1ms scheduling delay (found by
		// Sol build-review). The integration tests keep real time, where the age
		// is asserted as a lower bound rather than an equality.
		frozen := time.Now()
		server.admitNow = func() time.Time { return frozen }
		queue := &sliceQueue{path: path, server: server, outstanding: granted, outstandingJobs: jobs, adopted: adopted, adoptedJobs: adoptedJobs}
		queue.waiters = []*admitWaiter{
			{seq: 1, reserve: scopeBackedBytes, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: "CONFINE-job-5101-abc", name: "job", owner: "session-a"},
			// AIRA-108: the scope-less waiter carries the signature and grant
			// instant `confine --list` now names it by, so this fixture exercises
			// the reservation ROW as well as the aggregate it used to be.
			{seq: 2, reserve: reservationBytes, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
				signature: "pytest:tools/test_x.py::test_y", grantedAt: frozen.Add(-90 * time.Second)},
		}
		server.admitQueues[path] = queue
		return server, path
	}
	request := core.Request{Verb: "confine-list", Args: map[string]any{"slice": "test.slice", "owner": "session-a"}}

	t.Run("established", func(t *testing.T) {
		server, _ := setup(t)
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return sliceCurrent, maximum, sliceReclaimable, true, ""
		}
		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("response=%+v result=%+v", response, result)
		}
		wantJobs := jobs + adoptedJobs
		// Ceiling scales headroom by TOTAL admitted jobs (outstanding+adopted)+1.
		wantCeiling := maximum - base - int64(jobs+adoptedJobs+1)*supervisor
		// Queued/FreezePhase are the AIRA-59 diagnostics. This fixture has no
		// queued waiters, so a KNOWN zero and "idle" are the correct report —
		// never "unevaluated", which is reserved for state that cannot be read.
		want := withSystemFrame(runner.ConfineSliceReserve{
			GrantedBytes: granted + adopted, CeilingBytes: wantCeiling, Jobs: wantJobs, Queued: 0, FreezePhase: "idle",
			// The split names WHICH population each job belongs to, so the job
			// count can never again be read against the scope table above it.
			ScopeJobs: 1, ScopeBytes: scopeBackedBytes,
			ReservationJobs: 1, ReservationBytes: reservationBytes,
			// AIRA-108: the aggregate above is no longer the only report of this
			// population. The row NAMES it, and says `holding` rather than leaving
			// the reader to infer the state from the heading it sits under.
			Reservations: []runner.ConfineReservationHold{{
				State: runner.ConfineReservationStateHolding, Signature: "pytest:tools/test_x.py::test_y",
				Reserve: reservationBytes, HeldMS: 90000,
			}},
			AdoptedJobs: adoptedJobs, AdoptedBytes: adopted,
			// AIRA-114. The bound is ON by default, so its limit is reported.
			// CapAggregateKnown stays FALSE here because no evaluator pass has
			// scanned this fixture's slice, and an unevaluated aggregate must
			// present as unevaluated rather than as a measured zero.
			CapBoundBytes: maximum * oversubscriptionFactorPctDefault / 100,
		})
		if got := *result.SliceReserve; !reflect.DeepEqual(got, want) {
			t.Fatalf("slice reserve=%+v, want %+v", got, want)
		}
	})

	t.Run("adopted-only", func(t *testing.T) {
		server, path := setup(t)
		server.admitQueues[path].outstanding = 0
		server.admitQueues[path].outstandingJobs = 0
		server.admitQueues[path].waiters = nil
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return sliceCurrent, maximum, sliceReclaimable, true, ""
		}
		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("response=%+v result=%+v", response, result)
		}
		wantCeiling := maximum - base - int64(adoptedJobs+1)*supervisor
		want := withSystemFrame(runner.ConfineSliceReserve{
			GrantedBytes: adopted, CeilingBytes: wantCeiling, Jobs: adoptedJobs, Queued: 0, FreezePhase: "idle",
			AdoptedJobs: adoptedJobs, AdoptedBytes: adopted,
			CapBoundBytes: maximum * oversubscriptionFactorPctDefault / 100,
		})
		// Reservations is nil here, and that is a POSITIVE fact from the same
		// walk (this fixture cleared the waiter list), not an unevaluated read.
		if got := *result.SliceReserve; !reflect.DeepEqual(got, want) {
			t.Fatalf("slice reserve=%+v, want adopted-only %+v", got, want)
		}
	})

	// AIRA-114. The ESTABLISHED arm of the aggregate, and the only one that can
	// fail against a build which hardcodes `CapAggregateKnown: false` on the wire
	// or drops the queue -> snapshot copy. Both arms above pin the DEFAULT-false
	// fixture, which such a build reproduces exactly.
	//
	// It carries its own fixture rather than reusing setup(): this arm needs a
	// real evaluateAdmitQueue pass, whose adoption and charge effects the shared
	// expectations do not describe, and reusing them would have meant loosening
	// the DeepEqual that makes every other field here load-bearing.
	//
	// What a false report costs, and why this is worth its own arm: with the bit
	// stuck false `aira confine --list` prints "slice scope caps: unevaluated ...
	// (not applied while unevaluated)" permanently, while the bound is in fact
	// refusing launches — an operator debugging a wait would be told the exact
	// opposite of the truth, the AIRA-71 silent-wait failure with a wrong
	// statement attached instead of a missing one.
	t.Run("aggregate-established", func(t *testing.T) {
		const suiteCap = int64(30) << 30
		path := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
		server.admitSliceHeadroomBase = base
		server.admitSliceHeadroomSupervisor = supervisor
		frozen := time.Now()
		server.admitNow = func() time.Time { return frozen }
		server.shimReadMemTotal = func() (int64, bool) { return systemMemTotal, true }
		server.shimReadMemAvailable = func() (int64, bool, string) { return systemMemAvailable, true, "" }
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return sliceCurrent, maximum, sliceReclaimable, true, ""
		}
		// One LEAF-DRAINED scope: subtree-live, so the aggregate counts it, but
		// leaf-empty, so the adoption loop skips it. That keeps every other field
		// on the wire at its idle value and leaves the aggregate as the single
		// thing this arm changes.
		server.admitConfineScanInterval = time.Nanosecond
		server.admitConfineScan = staticScan(leafDrainedRecord("CONFINE-suite-5101-abc", 2<<30, suiteCap))
		queue := &sliceQueue{path: path, server: server}
		server.admitRegistryMu.Lock()
		server.admitQueues[path] = queue
		server.admitRegistryMu.Unlock()

		server.evaluateAdmitQueue(queue)

		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("response=%+v result=%+v", response, result)
		}
		want := runner.ConfineSliceReserve{
			// AIRA-127. This arm writes NO memory.high into its slice, so the soft
			// limit is "unevaluated" rather than the "set" the other two pin. That
			// is the distinction the wire keeps and a renderer acts on: an absent
			// file is not a slice with no soft limit, and neither is a zero.
			SystemMemTotalBytes: systemMemTotal, SystemMemAvailableBytes: systemMemAvailable,
			SliceCurrentBytes: sliceCurrent, SliceReclaimableBytes: sliceReclaimable,
			SliceMaxBytes: maximum, SliceHighState: runner.ConfineSliceHighUnevaluated,
			CeilingBytes: maximum - base - supervisor, Queued: 0, FreezePhase: "idle",
			// The value AND the bit. Asserting only the bit would survive a build
			// that reported a fabricated total beside a true bit.
			CapAggregateBytes: suiteCap,
			CapAggregateKnown: true,
			CapBoundBytes:     maximum * oversubscriptionFactorPctDefault / 100,
		}
		if got := *result.SliceReserve; !reflect.DeepEqual(got, want) {
			t.Fatalf("slice reserve=%+v, want %+v", got, want)
		}
	})

	t.Run("memory-unavailable", func(t *testing.T) {
		server, _ := setup(t)
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, 0, 0, false, "read-error"
		}
		response := server.confineManagement(context.Background(), request)
		result, ok := response.Data.(runner.ConfineListResult)
		if !response.OK || !ok {
			t.Fatalf("response=%+v", response)
		}
		if result.SliceReserve != nil {
			t.Fatalf("slice reserve=%+v, want nil", result.SliceReserve)
		}
	})
}

// AIRA-127. The real memory.high read, against real files. It keeps three
// outcomes apart, and the distinction is load-bearing on the operator surface:
// "none" says a slice has no soft limit configured, "unevaluated" says nobody
// could find out, and a renderer that collapsed them would draw a limit tick at
// the bar's origin — a slice throttled to nothing — for an unreadable file.
//
// verifies: AIRA-127
func TestReadSliceMemoryHigh(t *testing.T) {
	cases := []struct {
		name      string
		content   *string
		wantBytes int64
		wantState string
	}{
		{"set", strPtr("15032385536\n"), 14 << 30, runner.ConfineSliceHighSet},
		{"set-without-newline", strPtr("1048576"), 1 << 20, runner.ConfineSliceHighSet},
		{"none", strPtr("max\n"), 0, runner.ConfineSliceHighNone},
		{"unparsable", strPtr("fourteen gigs\n"), 0, runner.ConfineSliceHighUnevaluated},
		{"absent", nil, 0, runner.ConfineSliceHighUnevaluated},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := t.TempDir()
			if testCase.content != nil {
				if err := os.WriteFile(filepath.Join(path, "memory.high"), []byte(*testCase.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			bytes, state := readSliceMemoryHigh(path)
			if bytes != testCase.wantBytes || state != testCase.wantState {
				t.Fatalf("readSliceMemoryHigh=%d/%q, want %d/%q", bytes, state, testCase.wantBytes, testCase.wantState)
			}
		})
	}
}

func strPtr(value string) *string { return &value }
