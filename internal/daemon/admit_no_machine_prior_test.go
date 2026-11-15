package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-341 deleted the machine-wide p90 prior (the old cold-start rung of the
// unpinned-reserve estimator). The tests below were written FIRST against the
// resolver that still had the rung (commit "AIRA-341: characterise the resolver
// before deleting the machine-wide prior"), then reduced to the post-deletion
// expectations, so the deletion is pinned against the behaviour it replaced.

// TestColdSignatureSizesAtTheUnpinnedDefault is the live box's commonest cold
// start (docs/dev/aira-329-cold-start-diagnosis.md): no history of its own.
// Before AIRA-341 this answer held where the machine-wide read ran out of its
// 250 ms deadline (the characterisation commit pinned it with a prior seam that
// blocked until the deadline expired). It is the unpinned 1 GiB default
// (AIRA-304), named `fallback:no-history`, unchanged.
//
// verifies: AIRA-341
func TestColdSignatureSizesAtTheUnpinnedDefault(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakHistory = staticHistory(runner.PeakRSSStats{})
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: runner.DefaultConfineMemoryReserve, signature: "novel"}, 64<<30)
	if reserve != runner.DefaultConfineMemoryReserve || basis != "fallback:no-history" {
		t.Fatalf("cold start = %d/%q, want %d/%q",
			reserve, basis, runner.DefaultConfineMemoryReserve, "fallback:no-history")
	}
}

// TestColdSignatureBesideOtherSignaturesHistoryOnARealStore is the case the
// deletion CHANGES (owner decision 2026-11-14): a real store holding ten other
// signatures with three 1.5 GiB clean peaks each, so the old machine-wide read
// succeeded. Before AIRA-341 the cold start was sized from their p90 (1852204646
// bytes); now it is the 1 GiB default under `fallback:no-history`, exactly as
// where that read missed its deadline.
//
// verifies: AIRA-341
func TestColdSignatureBesideOtherSignaturesHistoryOnARealStore(t *testing.T) {
	server, _ := ownFloorServer(t, true)
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: runner.DefaultConfineMemoryReserve, signature: "novel"}, 64<<30)
	if reserve != runner.DefaultConfineMemoryReserve || basis != "fallback:no-history" {
		t.Fatalf("cold start beside other signatures' history = %d/%q, want %d/%q",
			reserve, basis, runner.DefaultConfineMemoryReserve, "fallback:no-history")
	}
}

type noPriorCell struct {
	name      string
	signature string
	read      func(context.Context, string) (runner.PeakRSSStats, error)
	pinned    bool
	hint      int64
	ceiling   int64
}

// noPriorGrid is every input class resolveAdmitReserve distinguishes: no
// signature, a failed read, no reader, no rows, capture-unavailable,
// one/two/three clean samples either side of the default, a malformed history,
// OOM records below, near and far above the default, and a pinned request; each
// at a ceiling far above the default, above it, equal to it, below it (the fit
// fires) and too small for any fit; each with the 1 GiB client hint and a small
// one.
func noPriorGrid() []noPriorCell {
	type history struct {
		name      string
		signature string
		read      func(context.Context, string) (runner.PeakRSSStats, error)
		pinned    bool
	}
	readErr := func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{}, errors.New("E_DAEMON_INTERNAL: injected read failure")
	}
	const mib, gib = int64(1 << 20), int64(1 << 30)
	histories := []history{
		{name: "no-signature", signature: ""},
		{name: "read-error", signature: "sig", read: readErr},
		{name: "no-reader", signature: "sig"},
		{name: "no-rows", signature: "sig", read: staticHistory(runner.PeakRSSStats{})},
		{name: "capture-unavailable", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 2})},
		{name: "one-clean-small", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 100 * mib, MaxCleanPeak: 100 * mib})},
		{name: "one-clean-large", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 1536 * mib, MaxCleanPeak: 1536 * mib})},
		{name: "two-clean-mid", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 2, SampleCount: 2, PeakMax: 800 * mib, MaxCleanPeak: 800 * mib})},
		{name: "three-clean-small", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 200 * mib, MaxCleanPeak: 200 * mib})},
		{name: "three-clean-large", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 3 * gib, MaxCleanPeak: 3 * gib})},
		{name: "three-clean-huge", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 50 * gib, MaxCleanPeak: 50 * gib})},
		{name: "malformed", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 3, SampleCount: 3})},
		{name: "oom-one-small", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 40 * mib, OOMCount: 1, MaxOOMPeak: 40 * mib})},
		{name: "oom-one-large", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 800 * mib, OOMCount: 1, MaxOOMPeak: 800 * mib})},
		{name: "oom-beside-clean", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 2 * gib, OOMCount: 1, MaxOOMPeak: gib, MaxCleanPeak: 2 * gib})},
		{name: "oom-huge", signature: "sig", read: staticHistory(runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 10 * gib, OOMCount: 1, MaxOOMPeak: 10 * gib})},
		{name: "pinned", signature: "sig", read: staticHistory(runner.PeakRSSStats{}), pinned: true},
	}
	ceilings := []int64{64 * gib, 2 * gib, runner.DefaultConfineMemoryReserve, 897581056, 1 * mib}
	hints := []int64{runner.DefaultConfineMemoryReserve, 256 * mib}
	var cells []noPriorCell
	for _, h := range histories {
		for _, ceiling := range ceilings {
			for _, hint := range hints {
				cells = append(cells, noPriorCell{
					name:      fmt.Sprintf("%s/ceiling=%d/hint=%d", h.name, ceiling, hint),
					signature: h.signature, read: h.read, pinned: h.pinned, hint: hint, ceiling: ceiling,
				})
			}
		}
	}
	return cells
}

// TestResolveAdmitReserveIsTheOldResolverWithThePriorMissing is the differential
// pin. noPriorWant is, cell for cell, what the OLD resolver returned when its
// machine-wide read failed or ran out of its deadline -- generated from the
// pre-deletion code and committed with it, where the same test also pinned the
// two "read succeeded" columns. The deletion makes every input behave exactly
// as the old resolver did on a miss, so every cell must match byte for byte.
//
// On the old resolver a successful read changed 72 of these 170 cells (a prior
// above the default) and 63 (a prior below it). Every changed cell is one that
// fell through the per-signature block without returning: no signature, a
// failed read, no reader, no rows, capture-unavailable, or one/two clean
// samples with no OOM (where the own-peak floor did not exceed the prior).
// Every cell with three usable samples, an OOM on record, a malformed history or
// a pinned request was identical in all three modes, and is here too.
//
// verifies: AIRA-341
func TestResolveAdmitReserveIsTheOldResolverWithThePriorMissing(t *testing.T) {
	cells := noPriorGrid()
	if len(cells) != len(noPriorWant) {
		t.Fatalf("grid has %d cells, expectations %d", len(cells), len(noPriorWant))
	}
	for index, cell := range cells {
		want := noPriorWant[index]
		if want.name != cell.name {
			t.Fatalf("cell %d is %q, expectation is for %q", index, cell.name, want.name)
		}
		server := NewServer(Paths{})
		server.stopping = make(chan struct{})
		server.admitPeakHistory = cell.read
		reserve, basis := server.resolveAdmitReserve(
			admitRequest{reserve: cell.hint, signature: cell.signature, pinned: cell.pinned}, cell.ceiling)
		if reserve != want.reserve || basis != want.basis {
			t.Errorf("%s = %d/%q, want %d/%q (the old resolver on a missed prior read)",
				cell.name, reserve, basis, want.reserve, want.basis)
		}
	}
}

// TestNoLiveCodeOrDocNamesTheDeletedPrior is the grep test: no Go source and no
// live document may name the deleted rung, its basis or its reader. Historical
// records -- dated specs, plans and reviews, the ticket store, and the dated
// measurement write-ups and their repro scripts under docs/dev -- are left as
// written. The needles are assembled at run time so this file does not match
// itself.
//
// verifies: AIRA-341
func TestNoLiveCodeOrDocNamesTheDeletedPrior(t *testing.T) {
	needles := []string{
		"p90" + "-prior",
		"ConfinePeak" + "P90",
		"admitPeak" + "P90",
		"admitPrior" + "Refresh",
	}
	root := moduleRootForDaemonTests(t)
	historical := func(rel string) bool {
		return strings.HasPrefix(rel, ".aira/") ||
			strings.HasPrefix(rel, "docs/superpowers/") ||
			strings.HasPrefix(rel, "docs/plans/") ||
			strings.HasPrefix(rel, "docs/dev/aira-")
	}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == ".git" || rel == "dist" || historical(rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if historical(rel) {
			return nil
		}
		switch filepath.Ext(rel) {
		case ".go", ".md", ".sh", ".py", ".txt", ".json":
		default:
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, needle := range needles {
			if strings.Contains(string(body), needle) {
				t.Errorf("%s names the deleted machine-wide prior (%q); AIRA-341 removed it", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files under %s; the walk is not reaching the source tree", scanned, root)
	}
}

func moduleRootForDaemonTests(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}

var noPriorWant = []struct {
	name    string
	reserve int64
	basis   string
}{
	{"no-signature/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:no-signature"},
	{"no-signature/ceiling=68719476736/hint=268435456", 268435456, "fallback:no-signature"},
	{"no-signature/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:no-signature"},
	{"no-signature/ceiling=2147483648/hint=268435456", 268435456, "fallback:no-signature"},
	{"no-signature/ceiling=1073741824/hint=1073741824", 933688542, "fallback:no-signature,ceiling-fitted"},
	{"no-signature/ceiling=1073741824/hint=268435456", 268435456, "fallback:no-signature"},
	{"no-signature/ceiling=897581056/hint=1073741824", 780505266, "fallback:no-signature,ceiling-fitted"},
	{"no-signature/ceiling=897581056/hint=268435456", 268435456, "fallback:no-signature"},
	{"no-signature/ceiling=1048576/hint=1073741824", 1073741824, "fallback:no-signature"},
	{"no-signature/ceiling=1048576/hint=268435456", 268435456, "fallback:no-signature"},
	{"read-error/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:history-unavailable"},
	{"read-error/ceiling=68719476736/hint=268435456", 268435456, "fallback:history-unavailable"},
	{"read-error/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:history-unavailable"},
	{"read-error/ceiling=2147483648/hint=268435456", 268435456, "fallback:history-unavailable"},
	{"read-error/ceiling=1073741824/hint=1073741824", 933688542, "fallback:history-unavailable,ceiling-fitted"},
	{"read-error/ceiling=1073741824/hint=268435456", 268435456, "fallback:history-unavailable"},
	{"read-error/ceiling=897581056/hint=1073741824", 780505266, "fallback:history-unavailable,ceiling-fitted"},
	{"read-error/ceiling=897581056/hint=268435456", 268435456, "fallback:history-unavailable"},
	{"read-error/ceiling=1048576/hint=1073741824", 1073741824, "fallback:history-unavailable"},
	{"read-error/ceiling=1048576/hint=268435456", 268435456, "fallback:history-unavailable"},
	{"no-reader/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:no-history"},
	{"no-reader/ceiling=68719476736/hint=268435456", 268435456, "fallback:no-history"},
	{"no-reader/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:no-history"},
	{"no-reader/ceiling=2147483648/hint=268435456", 268435456, "fallback:no-history"},
	{"no-reader/ceiling=1073741824/hint=1073741824", 933688542, "fallback:no-history,ceiling-fitted"},
	{"no-reader/ceiling=1073741824/hint=268435456", 268435456, "fallback:no-history"},
	{"no-reader/ceiling=897581056/hint=1073741824", 780505266, "fallback:no-history,ceiling-fitted"},
	{"no-reader/ceiling=897581056/hint=268435456", 268435456, "fallback:no-history"},
	{"no-reader/ceiling=1048576/hint=1073741824", 1073741824, "fallback:no-history"},
	{"no-reader/ceiling=1048576/hint=268435456", 268435456, "fallback:no-history"},
	{"no-rows/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:no-history"},
	{"no-rows/ceiling=68719476736/hint=268435456", 268435456, "fallback:no-history"},
	{"no-rows/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:no-history"},
	{"no-rows/ceiling=2147483648/hint=268435456", 268435456, "fallback:no-history"},
	{"no-rows/ceiling=1073741824/hint=1073741824", 933688542, "fallback:no-history,ceiling-fitted"},
	{"no-rows/ceiling=1073741824/hint=268435456", 268435456, "fallback:no-history"},
	{"no-rows/ceiling=897581056/hint=1073741824", 780505266, "fallback:no-history,ceiling-fitted"},
	{"no-rows/ceiling=897581056/hint=268435456", 268435456, "fallback:no-history"},
	{"no-rows/ceiling=1048576/hint=1073741824", 1073741824, "fallback:no-history"},
	{"no-rows/ceiling=1048576/hint=268435456", 268435456, "fallback:no-history"},
	{"capture-unavailable/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"capture-unavailable/ceiling=68719476736/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"capture-unavailable/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"capture-unavailable/ceiling=2147483648/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"capture-unavailable/ceiling=1073741824/hint=1073741824", 933688542, "fallback:insufficient-samples,ceiling-fitted"},
	{"capture-unavailable/ceiling=1073741824/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"capture-unavailable/ceiling=897581056/hint=1073741824", 780505266, "fallback:insufficient-samples,ceiling-fitted"},
	{"capture-unavailable/ceiling=897581056/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"capture-unavailable/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"capture-unavailable/ceiling=1048576/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=68719476736/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=2147483648/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=1073741824/hint=1073741824", 933688542, "fallback:insufficient-samples,ceiling-fitted"},
	{"one-clean-small/ceiling=1073741824/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=897581056/hint=1073741824", 780505266, "fallback:insufficient-samples,ceiling-fitted"},
	{"one-clean-small/ceiling=897581056/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"one-clean-small/ceiling=1048576/hint=268435456", 268435456, "fallback:insufficient-samples"},
	{"one-clean-large/ceiling=68719476736/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=68719476736/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=2147483648/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=2147483648/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1073741824/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1073741824/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=897581056/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=897581056/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1048576/hint=1073741824", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"one-clean-large/ceiling=1048576/hint=268435456", 1852204646, "estimate:own-max=1610612736,n=1,f=115"},
	{"two-clean-mid/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"two-clean-mid/ceiling=68719476736/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"two-clean-mid/ceiling=2147483648/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=1073741824/hint=1073741824", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=1073741824/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=897581056/hint=1073741824", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=897581056/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"two-clean-mid/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples"},
	{"two-clean-mid/ceiling=1048576/hint=268435456", 964689920, "estimate:own-max=838860800,n=2,f=115"},
	{"three-clean-small/ceiling=68719476736/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=68719476736/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=2147483648/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=2147483648/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1073741824/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1073741824/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=897581056/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=897581056/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1048576/hint=1073741824", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-small/ceiling=1048576/hint=268435456", 241172480, "estimate:max=209715200,n=3,f=115"},
	{"three-clean-large/ceiling=68719476736/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=68719476736/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=2147483648/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=2147483648/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1073741824/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1073741824/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=897581056/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=897581056/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1048576/hint=1073741824", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-large/ceiling=1048576/hint=268435456", 3704409292, "estimate:max=3221225472,n=3,f=115"},
	{"three-clean-huge/ceiling=68719476736/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=68719476736/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=2147483648/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=2147483648/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1073741824/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1073741824/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=897581056/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=897581056/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1048576/hint=1073741824", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"three-clean-huge/ceiling=1048576/hint=268435456", 61740154880, "estimate:max=53687091200,n=3,f=115"},
	{"malformed/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:malformed"},
	{"malformed/ceiling=68719476736/hint=268435456", 268435456, "fallback:malformed"},
	{"malformed/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:malformed"},
	{"malformed/ceiling=2147483648/hint=268435456", 268435456, "fallback:malformed"},
	{"malformed/ceiling=1073741824/hint=1073741824", 933688542, "fallback:malformed,ceiling-fitted"},
	{"malformed/ceiling=1073741824/hint=268435456", 268435456, "fallback:malformed"},
	{"malformed/ceiling=897581056/hint=1073741824", 780505266, "fallback:malformed,ceiling-fitted"},
	{"malformed/ceiling=897581056/hint=268435456", 268435456, "fallback:malformed"},
	{"malformed/ceiling=1048576/hint=1073741824", 1073741824, "fallback:malformed"},
	{"malformed/ceiling=1048576/hint=268435456", 268435456, "fallback:malformed"},
	{"oom-one-small/ceiling=68719476736/hint=1073741824", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=68719476736/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=2147483648/hint=1073741824", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=2147483648/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=1073741824/hint=1073741824", 933688542, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"},
	{"oom-one-small/ceiling=1073741824/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=897581056/hint=1073741824", 780505266, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"},
	{"oom-one-small/ceiling=897581056/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=1048576/hint=1073741824", 1073741824, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-small/ceiling=1048576/hint=268435456", 268435456, "fallback:insufficient-samples:n=1,oom-on-record"},
	{"oom-one-large/ceiling=68719476736/hint=1073741824", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=68719476736/hint=268435456", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=2147483648/hint=1073741824", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=2147483648/hint=268435456", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=1073741824/hint=1073741824", 933688542, "estimate:oom-escalated,ceiling-clamped"},
	{"oom-one-large/ceiling=1073741824/hint=268435456", 933688542, "estimate:oom-escalated,ceiling-clamped"},
	{"oom-one-large/ceiling=897581056/hint=1073741824", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=897581056/hint=268435456", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=1048576/hint=1073741824", 1258291200, "estimate:oom-escalated"},
	{"oom-one-large/ceiling=1048576/hint=268435456", 1258291200, "estimate:oom-escalated"},
	{"oom-beside-clean/ceiling=68719476736/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=68719476736/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=2147483648/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=2147483648/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1073741824/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1073741824/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=897581056/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=897581056/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1048576/hint=1073741824", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-beside-clean/ceiling=1048576/hint=268435456", 2469606195, "estimate:max=2147483648,n=4,f=115,oom-on-record"},
	{"oom-huge/ceiling=68719476736/hint=1073741824", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=68719476736/hint=268435456", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=2147483648/hint=1073741824", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=2147483648/hint=268435456", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1073741824/hint=1073741824", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1073741824/hint=268435456", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=897581056/hint=1073741824", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=897581056/hint=268435456", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1048576/hint=1073741824", 16106127360, "estimate:oom-escalated"},
	{"oom-huge/ceiling=1048576/hint=268435456", 16106127360, "estimate:oom-escalated"},
	{"pinned/ceiling=68719476736/hint=1073741824", 1073741824, "pinned:client"},
	{"pinned/ceiling=68719476736/hint=268435456", 268435456, "pinned:client"},
	{"pinned/ceiling=2147483648/hint=1073741824", 1073741824, "pinned:client"},
	{"pinned/ceiling=2147483648/hint=268435456", 268435456, "pinned:client"},
	{"pinned/ceiling=1073741824/hint=1073741824", 1073741824, "pinned:client"},
	{"pinned/ceiling=1073741824/hint=268435456", 268435456, "pinned:client"},
	{"pinned/ceiling=897581056/hint=1073741824", 1073741824, "pinned:client"},
	{"pinned/ceiling=897581056/hint=268435456", 268435456, "pinned:client"},
	{"pinned/ceiling=1048576/hint=1073741824", 1073741824, "pinned:client"},
	{"pinned/ceiling=1048576/hint=268435456", 268435456, "pinned:client"},
}
