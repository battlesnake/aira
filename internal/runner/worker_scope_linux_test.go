//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/cgrouptest"
)

func TestCreateWorkerScopeWritesVerifiedMemoryCap(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfineDelegation(outer); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate outer scope: %v", err)
	}

	// 134217728 (128 MiB) is an exact multiple of the page size —
	// writeScopeMemoryCap's own verification page-floors the value before
	// comparing (confirmed against the real verifyScopeMemoryValue/
	// floorMemoryPage code), so an unaligned value like 107374182 would be
	// floored by the kernel to 107372544 and this verbatim-string comparison
	// would fail even on a correct implementation.
	scopePath, swapCap, err := CreateWorkerScope(context.Background(), outer, "1", 134217728)
	if err != nil {
		t.Fatalf("CreateWorkerScope: %v", err)
	}
	if want := WorkerScopeChildPath(outer, "worker-1"); scopePath != want {
		t.Fatalf("scopePath=%q want %q", scopePath, want)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.max")); err != nil || strings.TrimSpace(string(data)) != "134217728" {
		t.Fatalf("memory.max=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.oom.group")); err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("memory.oom.group=%q err=%v", data, err)
	}
	// verifies: AIRA-35 — memory.high must be UNSET on a worker scope. This is
	// the assertion that fails if the retired soft throttle is ever restored:
	// at the old 80% split a deliberate leaker did not converge to its
	// oom.group kill in 420 seconds, and at 95% it took 16–18 s at the 512 MiB
	// cap this product ships. An unset memory.high reads back as "max".
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.high")); err != nil ||
		strings.TrimSpace(string(data)) != "max" {
		t.Fatalf("memory.high=%q err=%v, want \"max\" (unset) — a worker scope must carry no "+
			"kernel reclaim throttle; see CreateWorkerScope for the measured convergence cost", data, err)
	}
	// verifies: AIRA-35 — memory.swap.max must be 0, and the returned
	// disposition must MATCH what the kernel actually holds. Without this cap,
	// memory.max bounds memory but not memory+swap, and a worker that exceeds
	// it is reclaimed into swap and never killed at all (measured: 512 MiB
	// allocated inside a 32 MiB cap, exit status 0, ~520 MiB paged out).
	//
	// The expectation is DERIVED from whether this host exposes the control at
	// all, never hardcoded: on a CONFIG_SWAP=n or swapaccount=0 kernel the file
	// is absent and a hardcoded "enforced" would fail for the wrong reason.
	if !IsWorkerAdmitSwapCap(swapCap) {
		t.Fatalf("swapCap=%q is not a catalogued WorkerAdmitSwapCap value", swapCap)
	}
	switch _, statErr := os.Stat(filepath.Join(scopePath, "memory.swap.max")); {
	case statErr == nil:
		if swapCap != WorkerAdmitSwapCapEnforced {
			t.Fatalf("swapCap=%q but memory.swap.max exists on this host — a disposition that "+
				"does not match the kernel is exactly the fabricated claim this field exists to prevent", swapCap)
		}
		data, err := os.ReadFile(filepath.Join(scopePath, "memory.swap.max"))
		if err != nil || strings.TrimSpace(string(data)) != "0" {
			t.Fatalf("memory.swap.max=%q err=%v, want \"0\" — without it memory.max does not "+
				"contain a runaway on any host with swap", data, err)
		}
	default:
		if swapCap == WorkerAdmitSwapCapEnforced {
			t.Fatalf("swapCap=enforced but memory.swap.max does not exist (%v) — enforced is a "+
				"claim that the cap was written AND verified", statErr)
		}
		t.Logf("memory.swap.max unavailable on this host (%v); swap disposition reported as %q, "+
			"which is the honest unevaluated-style answer rather than a fake pass", statErr, swapCap)
	}
}

// verifies: AIRA-35 — the swap-cap ENOENT disambiguation is decided by POSITIVE
// evidence, never by a failure to look.
//
// /proc/swaps and the memory.swap.* cgroup files are registered by the same
// CONFIG_SWAP build, so "no memory.swap.max AND no /proc/swaps" really does
// prove this kernel cannot swap. But a missing or unmounted /proc makes EVERY
// path under it return ENOENT, and concluding "this kernel cannot swap" from
// that is the fake pass AIRA forbids — so the not-applicable verdict also
// requires a control path under /proc to be readable.
func TestClassifyAbsentSwapControlNeedsPositiveEvidence(t *testing.T) {
	dir := t.TempDir()
	control := filepath.Join(dir, "control")
	if err := os.WriteFile(control, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	swaps := filepath.Join(dir, "swaps")

	for _, test := range []struct {
		name         string
		swapsPath    string
		controlPath  string
		createSwaps  bool
		want         string
		whyItMatters string
	}{
		{
			name:      "no /proc/swaps but /proc is readable proves the kernel cannot swap",
			swapsPath: swaps, controlPath: control, want: WorkerAdmitSwapCapNotApplicable,
			whyItMatters: "the only case where memory.max really is the whole footprint bound",
		},
		{
			name:      "/proc/swaps present means this kernel CAN swap, so the cap is missing",
			swapsPath: swaps, controlPath: control, createSwaps: true, want: WorkerAdmitSwapCapUnavailable,
			whyItMatters: "a swap-capable host whose swap we could not bound must be reported, not excused",
		},
		{
			name:      "an unreadable /proc establishes nothing",
			swapsPath: swaps, controlPath: filepath.Join(dir, "missing-control"),
			want:         WorkerAdmitSwapCapUnavailable,
			whyItMatters: "every path under a missing /proc returns ENOENT; that is a failure to look, not a proof",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.createSwaps {
				if err := os.WriteFile(swaps, []byte("Filename\tType\tSize\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(swaps) })
			}
			originalSwaps, originalControl := procSwapsPath, procSelfStatPath
			procSwapsPath, procSelfStatPath = test.swapsPath, test.controlPath
			t.Cleanup(func() { procSwapsPath, procSelfStatPath = originalSwaps, originalControl })
			if got := classifyAbsentSwapControl(); got != test.want {
				t.Fatalf("classifyAbsentSwapControl()=%q want %q — %s", got, test.want, test.whyItMatters)
			}
		})
	}
}

func TestCreateWorkerScopeRemovesScopeOnMemoryCapFailure(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately do NOT call ensureConfineDelegation(outer). Without
	// +memory in outer's subtree_control, the worker child will not expose
	// memory.max and writeScopeMemoryCap must fail for this real cgroup
	// delegation error rather than a fabricated failure.
	data, err := os.ReadFile(filepath.Join(outer, "cgroup.subtree_control"))
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "read outer cgroup.subtree_control: %v", err)
	}
	for _, controller := range strings.Fields(string(data)) {
		if controller == "memory" {
			t.Fatalf("test precondition failed: outer unexpectedly delegates memory; cannot reproduce missing worker memory.max")
		}
	}

	_, _, err = CreateWorkerScope(context.Background(), outer, "1", 134217728)
	if err == nil {
		t.Fatal("CreateWorkerScope unexpectedly succeeded: worker memory.max was available despite missing outer memory delegation")
	}
	// verifies: AIRA-35 — the failure must be attributed to the MEMORY cap, not
	// to the swap cap. An undelegated memory controller exposes no memory.*
	// files at all, so memory.swap.max is ENOENT too; if the swap write ran
	// FIRST it would read that ENOENT as "this kernel has no swap support" and
	// hand back a scope with no memory.max at all. The ordering inside
	// CreateWorkerScope is what prevents that, and this pins it.
	if !strings.Contains(err.Error(), "memory cap") {
		t.Fatalf("error=%v, want it attributed to the memory cap — a swap-cap attribution here "+
			"means the swap write ran before memory.max and misread an undelegated controller", err)
	}
	scopePath := filepath.Join(outer, ".aira-worker-1")
	if _, statErr := os.Stat(scopePath); !os.IsNotExist(statErr) {
		t.Fatalf("capless worker scope remains after memory cap failure: stat %q: %v", scopePath, statErr)
	}
}
