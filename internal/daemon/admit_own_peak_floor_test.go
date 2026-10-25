package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/store"
	"aira/internal/testdeadline"
)

// AIRA-314. A command's own largest CLEAN recorded peak floors its unpinned
// admission reserve: never below GrowByEstimatorMargin(MaxCleanPeak), at every
// sample count, inside the OOM branch and its clamp, and never fitted to the
// slice (a measured floor over the ceiling is refused E_ADMIT_TOO_LARGE).
//
// Every test here uses a REAL store: rows written through RecordConfinePeak (or
// the daemon's own confine-report handler), `server.db = db`, and NO
// admitPeakHistory / admitPeakP90 seam, so the real readers run. A hand-built
// seam fixture that omitted MaxCleanPeak would switch the floor off silently.
//
// Expected values are literals computed by hand (spec §7), never recomputed by a
// copy of the algorithm.
const (
	ownFloorHint      = int64(1073741824) // the unpinned client default, H
	ownFloorPriorPeak = int64(1610612736) // the seeded machine-wide p90
	ownFloorPrior     = int64(1852204646) // p = p90 x 1.15
	ownFloorCeiling   = int64(8589934592) // 8 GiB
	ownFloorFit       = int64(7469508340) // FIT(8 GiB)

	ownFloorBelow = int64(524288000)  // F 602931200
	ownFloorAbove = int64(2947493888) // RANT-52's clean peak, F 3389617971
	ownFloorOver  = int64(8053063680) // F 9261023232 > ceiling
)

// ownFloorRow is one recorded run of the target signature. A zero peak is a run
// whose capture failed (recorded with no peak).
type ownFloorRow struct {
	peak int64
	oom  bool
}

// ownFloorServer builds a FRESH store and a FRESH server (fresh p90 cache) per
// call, so one cell's rows can never move another cell's machine-wide p90.
// withPrior seeds ten other signatures with three 1.5 GiB clean rows each, which
// the real ConfinePeakP90 turns into the prior p. Rows are written in order, one
// second apart, under the signature "target".
func ownFloorServer(t *testing.T, withPrior bool, rows ...ownFloorRow) (*Server, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.OpenDB(filepath.Join(dir, "state.db"), filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	base := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	if withPrior {
		for signature := 0; signature < 10; signature++ {
			for sample := 0; sample < 3; sample++ {
				peak := ownFloorPriorPeak
				if err := db.RecordConfinePeak(ctx, store.ResourcePeakObservation{
					Kind: store.ResourcePeakKindConfine, Signature: fmt.Sprintf("prior-%d", signature),
					Peak: &peak, At: base.Add(time.Duration(signature*10+sample) * time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for index, row := range rows {
		observation := store.ResourcePeakObservation{
			Kind: store.ResourcePeakKindConfine, Signature: "target", OOM: row.oom,
			At: base.Add(time.Hour + time.Duration(index)*time.Second),
		}
		if row.peak > 0 {
			peak := row.peak
			observation.Peak = &peak
		}
		if err := db.RecordConfinePeak(ctx, observation); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.db = db
	return server, db
}

func ownFloorResolve(t *testing.T, server *Server, ceiling int64) (int64, string) {
	t.Helper()
	return server.resolveAdmitReserve(admitRequest{reserve: ownFloorHint, signature: "target"}, ceiling)
}

// ownFloorHistory builds the spec §7 history kinds at n samples of one class peak.
func ownFloorHistory(kind string, n int, peak int64) []ownFloorRow {
	var rows []ownFloorRow
	switch kind {
	case "clean":
		for i := 0; i < n; i++ {
			rows = append(rows, ownFloorRow{peak: peak})
		}
	case "oom-only":
		for i := 0; i < n; i++ {
			rows = append(rows, ownFloorRow{peak: peak, oom: true})
		}
	case "mixed":
		if n == 1 {
			// An OOM row whose capture failed, plus one clean row.
			return []ownFloorRow{{oom: true}, {peak: peak}}
		}
		rows = append(rows, ownFloorRow{peak: peak / 2, oom: true})
		for i := 1; i < n; i++ {
			rows = append(rows, ownFloorRow{peak: peak})
		}
	}
	return rows
}

type ownFloorWant struct {
	reserve int64
	basis   string
}

// TestOwnPeakFloorTable is AIRA-314 T1: the grid over (samples) x (history kind)
// x (peak class), plus the explicit cells X1-X11. Each cell asserts the exact
// value AND the exact basis.
//
// verifies: AIRA-314 I1, I2, I3, I4, I5, I6; T1
func TestOwnPeakFloorTable(t *testing.T) {
	p, h := ownFloorPrior, ownFloorHint
	classes := []struct {
		name string
		peak int64
	}{{"below", ownFloorBelow}, {"above", ownFloorAbove}, {"over-ceiling", ownFloorOver}}

	t.Run("n=0", func(t *testing.T) {
		for _, test := range []struct {
			name string
			rows []ownFloorRow
		}{
			{"no rows", nil},
			{"one row with no peak", []ownFloorRow{{}}},
			{"one OOM row with no peak", []ownFloorRow{{oom: true}}},
		} {
			t.Run(test.name, func(t *testing.T) {
				server, _ := ownFloorServer(t, true, test.rows...)
				reserve, basis := ownFloorResolve(t, server, ownFloorCeiling)
				if reserve != p || basis != "estimate:p90-prior" {
					t.Fatalf("reserve=%d basis=%q, want %d/%q — zero samples must be byte-identical to today (I4)",
						reserve, basis, p, "estimate:p90-prior")
				}
			})
		}
	})

	// want[kind][n][class]
	ownMax := func(peak int64, n int, oom bool) string {
		basis := fmt.Sprintf("estimate:own-max=%d,n=%d,f=115", peak, n)
		if oom {
			basis += ",oom-on-record"
		}
		return basis
	}
	estMax := func(peak int64, n int, oom bool) string {
		basis := fmt.Sprintf("estimate:max=%d,n=%d,f=115", peak, n)
		if oom {
			basis += ",oom-on-record"
		}
		return basis
	}
	grid := map[string]map[int][3]ownFloorWant{
		"clean": {
			1:  {{p, "estimate:p90-prior"}, {3389617971, ownMax(ownFloorAbove, 1, false)}, {9261023232, ownMax(ownFloorOver, 1, false)}},
			2:  {{p, "estimate:p90-prior"}, {3389617971, ownMax(ownFloorAbove, 2, false)}, {9261023232, ownMax(ownFloorOver, 2, false)}},
			3:  {{602931200, estMax(ownFloorBelow, 3, false)}, {3389617971, estMax(ownFloorAbove, 3, false)}, {9261023232, estMax(ownFloorOver, 3, false)}},
			20: {{602931200, estMax(ownFloorBelow, 20, false)}, {3389617971, estMax(ownFloorAbove, 20, false)}, {9261023232, estMax(ownFloorOver, 20, false)}},
		},
		"oom-only": {
			1:  {{h, "fallback:insufficient-samples:n=1,oom-on-record"}, {4421240832, "estimate:oom-escalated"}, {12079595520, "estimate:oom-escalated"}},
			2:  {{h, "fallback:insufficient-samples:n=2,oom-on-record"}, {4421240832, "estimate:oom-escalated"}, {12079595520, "estimate:oom-escalated"}},
			3:  {{786432000, "estimate:oom-escalated"}, {4421240832, "estimate:oom-escalated"}, {12079595520, "estimate:oom-escalated"}},
			20: {{786432000, "estimate:oom-escalated"}, {4421240832, "estimate:oom-escalated"}, {12079595520, "estimate:oom-escalated"}},
		},
		"mixed": {
			// n=1 mixed: the OOM row has no peak, so the OOM branch is not entered
			// and the basis carries NO ,oom-on-record.
			1:  {{p, "estimate:p90-prior"}, {3389617971, ownMax(ownFloorAbove, 1, false)}, {9261023232, ownMax(ownFloorOver, 1, false)}},
			2:  {{h, "fallback:insufficient-samples:n=2,oom-on-record"}, {3389617971, ownMax(ownFloorAbove, 2, true)}, {9261023232, ownMax(ownFloorOver, 2, true)}},
			3:  {{602931200, estMax(ownFloorBelow, 3, true)}, {3389617971, estMax(ownFloorAbove, 3, true)}, {9261023232, estMax(ownFloorOver, 3, true)}},
			20: {{602931200, estMax(ownFloorBelow, 20, true)}, {3389617971, estMax(ownFloorAbove, 20, true)}, {9261023232, estMax(ownFloorOver, 20, true)}},
		},
	}
	for _, kind := range []string{"clean", "oom-only", "mixed"} {
		for _, n := range []int{1, 2, 3, 20} {
			for index, class := range classes {
				want := grid[kind][n][index]
				t.Run(fmt.Sprintf("n=%d/%s/%s", n, kind, class.name), func(t *testing.T) {
					server, _ := ownFloorServer(t, true, ownFloorHistory(kind, n, class.peak)...)
					reserve, basis := ownFloorResolve(t, server, ownFloorCeiling)
					if reserve != want.reserve || basis != want.basis {
						t.Fatalf("reserve=%d basis=%q, want %d/%q", reserve, basis, want.reserve, want.basis)
					}
				})
			}
		}
	}

	for _, test := range []struct {
		name    string
		rows    []ownFloorRow
		pinned  int64
		ceiling int64 // 0 means ownFloorCeiling
		want    ownFloorWant
	}{
		{
			// The outcome-table row "OOM branch, E > ceiling, clamp guard fails":
			// E = 2668376064 > ceiling 1610612736, and MaxOOMPeak 1778917376 is not
			// below FIT(1610612736) = 1400532813, so the clamp does not fire and E is
			// returned unclamped. F = 3389617971 > E, so the floor is the value.
			name:    "X9 E over the ceiling, clamp guard fails, F above E",
			rows:    []ownFloorRow{{peak: 1778917376, oom: true}, {peak: 2947493888}},
			ceiling: 1610612736,
			want:    ownFloorWant{3389617971, "estimate:own-max=2947493888,n=2,f=115,oom-on-record"},
		},
		{
			// The outcome-table row "floor below the clamp value": E = 9663676416 >
			// ceiling, the guard MaxOOMPeak 6442450944 < FIT holds, so the clamp
			// fires at FIT. F = 1234803097 < FIT, so the clamp value AND basis stand
			// (I2: never below today). A floor that replaced a clamped value would
			// shrink a just-OOM-killed 6 GiB job to 1.15 GiB.
			name: "X10 n=2 floor below the clamp value",
			rows: []ownFloorRow{{peak: 1073741824}, {peak: 6442450944, oom: true}},
			want: ownFloorWant{ownFloorFit, "estimate:oom-escalated,ceiling-clamped"},
		},
		{
			name: "X11 n=3 floor below the clamp value",
			rows: []ownFloorRow{{peak: 1073741824}, {peak: 1073741824}, {peak: 6442450944, oom: true}},
			want: ownFloorWant{ownFloorFit, "estimate:oom-escalated,ceiling-clamped"},
		},
		{
			// Case C: OOM-only, censored evidence; AIRA-153's clamp stands.
			name: "X1 case C, OOM-only n=1 in the clamp band",
			rows: []ownFloorRow{{peak: 7247757312, oom: true}},
			want: ownFloorWant{ownFloorFit, "estimate:oom-escalated,ceiling-clamped"},
		},
		{
			name: "X2 n=2 clean beside a smaller OOM, clamp fired",
			rows: []ownFloorRow{{peak: 6979321856}, {peak: 5905580032, oom: true}},
			want: ownFloorWant{8026220134, "estimate:own-max=6979321856,n=2,f=115,oom-on-record"},
		},
		{
			// The clean peak is BELOW the OOM peak: a PeakMax > MaxOOMPeak test
			// would miss it.
			name: "X3 n=2 clean below a larger OOM, clamp fired",
			rows: []ownFloorRow{{peak: 6979321856}, {peak: 7247757312, oom: true}},
			want: ownFloorWant{8026220134, "estimate:own-max=6979321856,n=2,f=115,oom-on-record"},
		},
		{
			name: "X4 n=3 clamp fired below the clean peak",
			rows: []ownFloorRow{{peak: 7247757312}, {peak: 7247757312}, {peak: 6442450944, oom: true}},
			want: ownFloorWant{8334920908, "estimate:own-max=7247757312,n=3,f=115,oom-on-record"},
		},
		{
			name: "X5 n=20 clamp fired below the clean peak",
			rows: append(ownFloorHistory("clean", 19, 7247757312), ownFloorRow{peak: 6442450944, oom: true}),
			want: ownFloorWant{8334920908, "estimate:own-max=7247757312,n=20,f=115,oom-on-record"},
		},
		{
			// Over the ceiling: returned as is, never fitted (I5); refused by
			// admitConnection (T8).
			name: "X6 n=3 floor over the ceiling",
			rows: []ownFloorRow{{peak: 7516192768}, {peak: 7516192768}, {peak: 6442450944, oom: true}},
			want: ownFloorWant{8643621683, "estimate:own-max=7516192768,n=3,f=115,oom-on-record"},
		},
		{
			// F == p exactly: the strict tie-break keeps today's basis.
			name: "X7 tie keeps the prior's basis",
			rows: []ownFloorRow{{peak: ownFloorPriorPeak}},
			want: ownFloorWant{ownFloorPrior, "estimate:p90-prior"},
		},
		{
			name:   "X8 pinned is untouched",
			rows:   []ownFloorRow{{peak: ownFloorOver}},
			pinned: 2147483648,
			want:   ownFloorWant{2147483648, "pinned:client"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := ownFloorServer(t, true, test.rows...)
			request := admitRequest{reserve: ownFloorHint, signature: "target"}
			if test.pinned > 0 {
				request = admitRequest{reserve: test.pinned, signature: "target", pinned: true}
			}
			ceiling := test.ceiling
			if ceiling == 0 {
				ceiling = ownFloorCeiling
			}
			reserve, basis := server.resolveAdmitReserve(request, ceiling)
			if reserve != test.want.reserve || basis != test.want.basis {
				t.Fatalf("reserve=%d basis=%q, want %d/%q", reserve, basis, test.want.reserve, test.want.basis)
			}
		})
	}
}

// TestOwnPeakFloorBeatsAnEscalationBelowTheCleanPeak is AIRA-314 T2, the RANT-52
// replay: an OOM, then a clean (pinned) run at a larger peak. Today the next
// unpinned run is sized at 1.5x the OOM peak, below its own clean peak.
//
// verifies: AIRA-314 I1, I6; T2
func TestOwnPeakFloorBeatsAnEscalationBelowTheCleanPeak(t *testing.T) {
	for _, test := range []struct {
		name string
		rows []ownFloorRow
		want ownFloorWant
	}{
		{
			// Today: E = 2668376064, estimate:oom-escalated.
			name: "escalation raised, below the clean peak",
			rows: []ownFloorRow{{peak: 1778917376, oom: true}, {peak: 2947493888}},
			want: ownFloorWant{3389617971, "estimate:own-max=2947493888,n=2,f=115,oom-on-record"},
		},
		{
			// Today: the escalation (786432000) does not raise H, so H with
			// fallback:insufficient-samples:n=2,oom-on-record.
			name: "escalation did not raise the hint",
			rows: []ownFloorRow{{peak: 524288000, oom: true}, {peak: 1288490188}},
			want: ownFloorWant{1481763716, "estimate:own-max=1288490188,n=2,f=115,oom-on-record"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := ownFloorServer(t, true, test.rows...)
			reserve, basis := ownFloorResolve(t, server, ownFloorCeiling)
			if reserve != test.want.reserve || basis != test.want.basis {
				t.Fatalf("reserve=%d basis=%q, want %d/%q", reserve, basis, test.want.reserve, test.want.basis)
			}
		})
	}
}

// TestOwnPeakFloorReplacesAFittedPrior is AIRA-314 T4: a floor win drops the
// ,ceiling-fitted suffix, because the returned value is no longer the fitted
// prior (AIRA-149's rule: the basis names the term that acted).
//
// verifies: AIRA-314 I6; T4
func TestOwnPeakFloorReplacesAFittedPrior(t *testing.T) {
	for _, test := range []struct {
		name      string
		withPrior bool
		ceiling   int64
		rows      []ownFloorRow
		want      ownFloorWant
	}{
		{
			// Control: no own clean peak, so today's fitted p90 prior stands.
			name: "SITE 2 control, no clean row", withPrior: true, ceiling: 1610612736,
			want: ownFloorWant{1400532813, "estimate:p90-prior,ceiling-fitted"},
		},
		{
			name: "SITE 2 fitted p90 prior", withPrior: true, ceiling: 1610612736,
			rows: []ownFloorRow{{peak: 1288490188}},
			want: ownFloorWant{1481763716, "estimate:own-max=1288490188,n=1,f=115"},
		},
		{
			// Control: a clean peak whose F loses to the fitted default keeps the
			// fitted basis byte-for-byte.
			name: "SITE 1 control, floor loses", ceiling: 939524096,
			rows: []ownFloorRow{{peak: 524288000}},
			want: ownFloorWant{816977474, "fallback:insufficient-samples,ceiling-fitted"},
		},
		{
			name: "SITE 1 fitted default", ceiling: 939524096,
			rows: []ownFloorRow{{peak: 734003200}},
			want: ownFloorWant{844103680, "estimate:own-max=734003200,n=1,f=115"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := ownFloorServer(t, test.withPrior, test.rows...)
			reserve, basis := ownFloorResolve(t, server, test.ceiling)
			if reserve != test.want.reserve || basis != test.want.basis {
				t.Fatalf("reserve=%d basis=%q, want %d/%q", reserve, basis, test.want.reserve, test.want.basis)
			}
		})
	}
}

// TestOwnPeakFloorWithNoReadablePrior is AIRA-314 T5, the RANT-55 replay: one
// clean (pinned) run at 1.02 GiB, then the same argv unpinned. With no readable
// prior today returns the 1 GiB hint and the job is OOM-killed.
//
// verifies: AIRA-314 I1, I6; T5
func TestOwnPeakFloorWithNoReadablePrior(t *testing.T) {
	t.Run("one clean run", func(t *testing.T) {
		server, _ := ownFloorServer(t, false, ownFloorRow{peak: 1099382784})
		reserve, basis := ownFloorResolve(t, server, ownFloorCeiling)
		if reserve != 1264290201 || basis != "estimate:own-max=1099382784,n=1,f=115" {
			t.Fatalf("reserve=%d basis=%q, want 1264290201/%q — today's 1 GiB fallback is what OOM-killed RANT-55",
				reserve, basis, "estimate:own-max=1099382784,n=1,f=115")
		}
	})
	t.Run("control: the escalation beats the floor and keeps its basis", func(t *testing.T) {
		server, _ := ownFloorServer(t, false, ownFloorRow{peak: 1099382784}, ownFloorRow{peak: 1073741824, oom: true})
		reserve, basis := ownFloorResolve(t, server, ownFloorCeiling)
		if reserve != 1610612736 || basis != "estimate:oom-escalated" {
			t.Fatalf("reserve=%d basis=%q, want 1610612736/%q — a floor that loses must not relabel",
				reserve, basis, "estimate:oom-escalated")
		}
	})
}

// TestOwnPeakFloorOverTheCeilingIsRefusedWithTheOwnHistoryAdvice is AIRA-314 T8:
// through the real admitConnection over net.Pipe, a floor over the ceiling is
// refused at request entry, naming both numbers, with the "OWN measured peak
// history" advice, and nothing is enqueued (I5, I8). X9 pins the unclamped
// escalation over the ceiling (clamp guard failing) that the floor must replace.
//
// verifies: AIRA-314 I5, I8; T8
func TestOwnPeakFloorOverTheCeilingIsRefusedWithTheOwnHistoryAdvice(t *testing.T) {
	// One job's headroom on this server is 32 MiB + 8 MiB, so cap - headroom is
	// exactly the cell's ceiling (8 GiB unless the cell names another).
	for _, test := range []struct {
		name     string
		rows     []ownFloorRow
		ceiling  int64 // 0 means ownFloorCeiling
		required int64
		basis    string
	}{
		{
			// X9: E = 2668376064 over the 1.5 GiB ceiling with the clamp guard
			// failing; F = 3389617971 > E, so the refusal must name F and the
			// own-history advice, not E and the OOM-escalated arm.
			name:     "X9: unclamped escalation over the ceiling, below the clean peak",
			rows:     []ownFloorRow{{peak: 1778917376, oom: true}, {peak: 2947493888}},
			ceiling:  1610612736,
			required: 3389617971, basis: "estimate:own-max=2947493888,n=2,f=115,oom-on-record",
		},
		{
			name: "one clean run over the ceiling", rows: []ownFloorRow{{peak: ownFloorOver}},
			required: 9261023232, basis: "estimate:own-max=8053063680,n=1,f=115",
		},
		{
			name:     "X6: the clamp would have granted fit below the clean peak",
			rows:     []ownFloorRow{{peak: 7516192768}, {peak: 7516192768}, {peak: 6442450944, oom: true}},
			required: 8643621683, basis: "estimate:own-max=7516192768,n=3,f=115,oom-on-record",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ceiling := test.ceiling
			if ceiling == 0 {
				ceiling = ownFloorCeiling
			}
			maximum := ceiling + (32 << 20) + (8 << 20)
			server, _ := ownFloorServer(t, false, test.rows...)
			server.admitPollInterval = time.Hour
			server.admitSliceHeadroomBase = 32 << 20
			server.admitSliceHeadroomSupervisor = 8 << 20
			server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
				return 4096, maximum, 0, true, ""
			}
			queue := &sliceQueue{
				path: "/slice", kick: make(chan struct{}, 1), stop: make(chan struct{}),
				stopped: make(chan struct{}), poll: time.Hour, server: server,
			}
			server.admitRegistryMu.Lock()
			server.admitQueues = map[string]*sliceQueue{"/slice": queue}
			server.admitRegistryMu.Unlock()
			server.admitAfter = func(time.Duration) <-chan time.Time { return make(chan time.Time) }

			serverConn, clientConn := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer serverConn.Close()
				server.admitConnection(serverConn, map[string]any{
					"slice": "slice", "reserve": ownFloorHint,
					"max_wait_ms": int64(30_000), "signature": "target",
				})
			}()
			t.Cleanup(func() {
				_ = clientConn.Close()
				<-done
			})
			frames := make(chan ResponseFrame, 1)
			errs := make(chan error, 1)
			go func() {
				var frame ResponseFrame
				if err := readFrame(clientConn, &frame); err != nil {
					errs <- err
					return
				}
				frames <- frame
			}()
			var frame ResponseFrame
			select {
			case frame = <-frames:
			case err := <-errs:
				t.Fatalf("read: %v", err)
			case <-testdeadline.After(5 * time.Second):
				t.Fatal("no frame: the request was enqueued instead of refused at entry")
			}
			if frame.Code != CodeAdmitTooLarge {
				t.Fatalf("code=%q, want %q", frame.Code, CodeAdmitTooLarge)
			}
			var rejection admitRejection
			if err := json.Unmarshal(frame.Data, &rejection); err != nil {
				t.Fatalf("rejection payload: %v", err)
			}
			if rejection.Required != test.required || rejection.Ceiling != ceiling || rejection.Basis != test.basis {
				t.Fatalf("rejection=%+v, want required=%d cap_minus_headroom=%d basis=%q",
					rejection, test.required, ceiling, test.basis)
			}
			if !strings.Contains(frame.Error, measuredAdvicePhrase) {
				t.Fatalf("error=%q, want the %q advice arm", frame.Error, measuredAdvicePhrase)
			}
			queue.mu.Lock()
			waiters := len(queue.waiters)
			queue.mu.Unlock()
			if waiters != 0 {
				t.Fatalf("%d waiter(s) enqueued; the refusal must precede the enqueue so the ledger is untouched", waiters)
			}
		})
	}
}

// TestHintEligibleRunFloorsTheNextUnpinnedReserve is AIRA-314 T9: the property
// that makes the over-reserve hint's "drop --memory-reserve and let aira
// estimate it from this command's history" true at every sample count. The
// hint-eligible run is recorded through the daemon's REAL confine-report
// handler, exactly as the runner reports a pinned clean run.
//
// verifies: AIRA-314 §5.2; T9
func TestHintEligibleRunFloorsTheNextUnpinnedReserve(t *testing.T) {
	const (
		hintPeak  = int64(1181116006)
		hintFloor = int64(1358283406) // 1.15 x hintPeak
	)
	report := func(t *testing.T, server *Server, peak int64, pinned bool) {
		t.Helper()
		args := map[string]any{"signature": "target", "oom": false, "peak_rss": peak}
		if pinned {
			args["budget"] = int64(3 << 30)
			args["budget_basis"] = "cap:operator:--memory-reserve"
		}
		if response := server.confineReport(args); !response.OK {
			t.Fatalf("confine-report: %+v", response)
		}
	}
	for _, withPrior := range []bool{false, true} {
		t.Run(fmt.Sprintf("prior=%v", withPrior), func(t *testing.T) {
			server, _ := ownFloorServer(t, withPrior)
			for runs := 1; runs <= 3; runs++ {
				report(t, server, hintPeak, true)
				reserve, basis := ownFloorResolve(t, server, ownFloorCeiling)
				if reserve < hintFloor {
					t.Fatalf("after %d hint-eligible run(s): reserve=%d (%s), want >= %d — the hint told the agent to drop the pin",
						runs, reserve, basis, hintFloor)
				}
			}
		})
	}
	t.Run("an older larger clean row sets the floor", func(t *testing.T) {
		server, _ := ownFloorServer(t, true)
		report(t, server, ownFloorAbove, false)
		report(t, server, hintPeak, true)
		reserve, basis := ownFloorResolve(t, server, ownFloorCeiling)
		if reserve != 3389617971 || basis != "estimate:own-max=2947493888,n=2,f=115" {
			t.Fatalf("reserve=%d basis=%q, want 3389617971/%q — the floor is the MAXIMUM retained clean peak, not the newest",
				reserve, basis, "estimate:own-max=2947493888,n=2,f=115")
		}
		if reserve < hintFloor {
			t.Fatalf("reserve=%d below the hint-eligible run's floor %d", reserve, hintFloor)
		}
	})
}
