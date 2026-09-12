//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
	// S2a: CreateWorkerScope now takes the worker's minted confine scope NAME (the
	// daemon passes CONFINE-aitest-w<seq>-<parentPid>-<stamp>), creating
	// .aira-<scopeName>, not a .aira-worker-N child.
	scopeName := "CONFINE-aitest-w1-111111-1"
	scopePath, swapCap, err := CreateWorkerScope(context.Background(), outer, scopeName, 134217728)
	if err != nil {
		t.Fatalf("CreateWorkerScope: %v", err)
	}
	if want := WorkerScopeChildPath(outer, scopeName); scopePath != want {
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

// verifies: S2a P2-1 — CreateWorkerScope SELF-HEALS a recoverable missing +memory
// delegation. This is the exact window §16b/P2-1 targets: the create parent (the
// slice) has memory in its cgroup.controllers but LOST it from its own
// subtree_control (a systemd reset, or T7's outer-scope reconfigure). Pre-P2-1 the
// worker child then exposed no memory.max and CreateWorkerScope failed, taking the
// whole suite unevaluated. P2-1's idempotent ensureConfineDelegation re-enables
// +memory before creating the child, so the memory.max write succeeds.
//
// This replaces the former TestCreateWorkerScopeRemovesScopeOnMemoryCapFailure, whose
// failure scenario (this same missing-delegation condition) P2-1 now HEALS rather than
// fails on. The swap-after-memory ordering that test also pinned is now unreachable via
// delegation — ensureConfineDelegation fails closed BEFORE any scope file is written
// when the controller is truly unavailable (see the fail-closed test below), so the
// swap write can no longer misread an undelegated controller. MUTATION: drop the
// ensureConfineDelegation call from CreateWorkerScope → the worker memory.max ENOENTs
// and this REDS.
func TestCreateWorkerScopeSelfHealsMissingMemoryDelegation(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	// outer has memory in its cgroup.controllers (parent delegates it) but NOT in its
	// own subtree_control — the recoverable window. We deliberately do NOT delegate it
	// here; CreateWorkerScope's ensureConfineDelegation must.
	data, err := os.ReadFile(filepath.Join(outer, "cgroup.subtree_control"))
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "read outer cgroup.subtree_control: %v", err)
	}
	for _, controller := range strings.Fields(string(data)) {
		if controller == "memory" {
			t.Fatalf("test precondition failed: outer already delegates memory; cannot exercise the self-heal")
		}
	}

	scopeName := "CONFINE-aitest-w1-111111-1"
	scopePath, _, err := CreateWorkerScope(context.Background(), outer, scopeName, 134217728)
	if err != nil {
		t.Fatalf("CreateWorkerScope failed on a RECOVERABLE missing delegation: %v — P2-1's ensureConfineDelegation must re-enable +memory so the worker memory.max write succeeds instead of failing the whole suite unevaluated", err)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.max")); err != nil || strings.TrimSpace(string(data)) != "134217728" {
		t.Fatalf("worker memory.max=%q err=%v, want it written after the self-heal (134217728)", data, err)
	}
}

// verifies: S2a P2-1 — a genuinely undelegatable parent fails CLOSED at the delegation
// step, BEFORE any scope directory is created (rather than mkdir a scope and ENOENT on
// its memory.max downstream). An unreadable parent cgroup.controllers is the robust
// trigger and needs no privileged cgroup fixture. This preserves the fail-closed
// coverage the repurposed test above used to carry.
func TestCreateWorkerScopeFailsClosedWhenParentUndelegatable(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "no-such-cgroup")
	_, _, err := CreateWorkerScope(context.Background(), parent, "CONFINE-aitest-w1-111111-1", 134217728)
	if err == nil {
		t.Fatal("CreateWorkerScope succeeded on an undelegatable parent; P2-1 must fail closed at the delegation step")
	}
	if !strings.Contains(err.Error(), "delegate memory controller") {
		t.Fatalf("error=%v, want the delegation-step attribution (fail closed before scope creation)", err)
	}
	if _, statErr := os.Stat(filepath.Join(parent, ".aira-CONFINE-aitest-w1-111111-1")); !os.IsNotExist(statErr) {
		t.Fatalf("a scope directory was created despite the delegation failure: stat err=%v", statErr)
	}
}

// verifies: S2a P2-2 — a worker sibling scope gets a static cpu.weight = the aged
// FLOOR, so N workers competing directly under the slice weigh ~ one confine job
// rather than N. Best-effort: skipped where the host exposes no cpu controller (the
// write is fail-open, matching the ordinary confine path). MUTATION: drop the
// cpu.weight write from CreateWorkerScope → the file keeps the kernel default (100)
// and this REDS.
func TestCreateWorkerScopeWritesFloorCPUWeight(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	controllers, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "read cgroup.controllers: %v", err)
	}
	if !strings.Contains(" "+strings.Join(strings.Fields(string(controllers)), " ")+" ", " memory ") {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not available on %s", parent)
	}
	if !strings.Contains(" "+strings.Join(strings.Fields(string(controllers)), " ")+" ", " cpu ") {
		t.Skip("host cgroup has no cpu controller; the worker cpu.weight write is fail-open by design")
	}
	scopeName := "CONFINE-aitest-w7-111111-2"
	scopePath, _, err := CreateWorkerScope(context.Background(), parent, scopeName, 134217728)
	if err != nil {
		t.Fatalf("CreateWorkerScope (P2-1 delegates +memory/+cpu on the parent itself): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(scopePath, "cpu.weight"))
	if err != nil {
		t.Fatalf("read worker cpu.weight: %v — P2-2 must write it when the cpu controller is delegated", err)
	}
	want := strconv.FormatInt(confineCPUWeightConfig().Floor, 10)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("worker cpu.weight=%q, want the aged floor %s (N siblings must not over-share CPU vs other confine jobs, P2-2)", got, want)
	}
}
