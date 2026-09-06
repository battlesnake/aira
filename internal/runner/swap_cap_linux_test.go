//go:build linux

package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

// swapUncappedMemoryParent builds a real cgroup-v2 parent with +memory delegated
// and a finite memory.max, and DELIBERATELY leaves its swap unbounded.
//
// That last part is the whole point and is why this cannot reuse
// confineMemoryParent or writableMemoryParent: both write memory.swap.max=0 on
// the PARENT, which bounds the child's swap by inheritance. A containment test
// run under such a parent proves the harness, not the product — precisely the
// false-pass AIRA-35 found in the aitest e2e test and AIRA-110 found again for
// confine scopes. Every assertion here must be about the bound AIRA writes on
// the scope it creates.
func swapUncappedMemoryParent(t *testing.T, maximum string) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	// Written explicitly rather than relied on as the kernel default, so this
	// stays true if the ambient fixture ever changes underneath it. Skipped when
	// the control is absent: there is then nothing to leave unbounded, and the
	// caller's own control-presence gate decides what that means.
	if _, err := os.Stat(filepath.Join(parent, "memory.swap.max")); err == nil {
		if err := os.WriteFile(filepath.Join(parent, "memory.swap.max"), []byte("max"), 0o644); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot leave parent swap unbounded: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "memory.max"), []byte(maximum), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory.max is not writable: %v", err)
	}
	return parent
}

// effectiveSwapCeiling is the memory.swap.max analogue of effectiveConfineCap:
// the smallest finite memory.swap.max across path and its ancestors up to the
// cgroup2 mount root, and whether any finite bound exists. memory.swap.max is
// hierarchical, so a scope under a swap-bounded ancestor is bounded too.
func effectiveSwapCeiling(path string) (int64, bool) {
	mount, err := unifiedMount()
	if err != nil {
		return 0, false
	}
	current, root := filepath.Clean(path), filepath.Clean(mount)
	var best int64
	found := false
	for {
		data, readErr := os.ReadFile(filepath.Join(current, "memory.swap.max"))
		if readErr == nil {
			if value, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); parseErr == nil {
				if !found || value < best {
					best, found = value, true
				}
			}
		}
		if current == root {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || len(parent) < len(root) {
			break
		}
		current = parent
	}
	return best, found
}

// activeSwapDevices counts the swap areas the kernel currently has online.
// /proc/swaps carries one header line plus one line per area.
func activeSwapDevices(t *testing.T) int {
	t.Helper()
	file, err := os.Open(procSwapsPath)
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Filename") {
			continue
		}
		count++
	}
	return count
}

// verifies: AIRA-110 — EVERY confine scope carries memory.swap.max=0, capped or
// not, and the disposition it reports matches what the kernel actually holds.
//
// RED against master: no production path wrote memory.swap.max on a confine
// scope at all, so a freshly created scope read back "max" (inherited-unbounded)
// in both sub-cases below.
//
// The uncapped sub-case is not decoration. A confine scope is deliberately left
// without a memory.max when an UNPINNED reserve is not daemon-admitted, and such
// a job is exactly the one whose swap must still be bounded: it is accounted for
// in the slice ledger and it feeds peak-RSS history, both of which a footprint
// that escapes into swap silently falsifies.
func TestConfineRealScopeBoundsSwapToZero(t *testing.T) {
	parent := swapUncappedMemoryParent(t, strconv.FormatInt(512<<20, 10))
	if _, err := os.Stat(filepath.Join(parent, "memory.swap.max")); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t,
			"this host exposes no memory.swap.max (%v); a swap bound cannot be established or asserted here", err)
	}
	for _, test := range []struct {
		name      string
		request   ConfineRequest
		uncapped  bool
		stubAdmit bool
	}{
		{
			name: "capped scope",
			request: ConfineRequest{
				ScopeMemoryMax: 64 << 20, MemoryReserve: 64 << 20, MemoryReservePinned: true,
			},
		},
		{
			// Admission is stubbed to "unevaluated" so the scope is deterministically
			// left uncapped: a live daemon would otherwise grant a history-derived
			// reserve and the scope WOULD get a memory.max, which is the case the
			// sub-test above already covers.
			name:      "uncapped scope",
			request:   ConfineRequest{},
			uncapped:  true,
			stubAdmit: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := &confineScopeObservation{}
			deps := defaultConfineDeps()
			deps.newBackend = func(path string) ScopeBackend {
				return confineObservingBackend{ScopeBackend: newDefaultBackend(path), observation: observation}
			}
			if test.stubAdmit {
				deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
					return admissionResult{state: "unevaluated", reason: "stubbed for an uncapped scope"}, nil
				}
			}
			request := test.request
			request.Slice = parent
			request.Argv = []string{"/bin/true"}
			request.SelfPath = os.Args[0]
			request.Stderr = io.Discard
			request.AdmissionMaxWait = 2 * time.Second
			request.PollInterval = 10 * time.Millisecond
			result, err := confineWithDeps(context.Background(), request, deps)
			if err != nil {
				cgrouptest.SkipOrFailRealCgroup(t, "confine swap-bound fixture unavailable: %v", err)
			}
			if result.Exit != 0 {
				t.Fatalf("exit=%d status=%+v", result.Exit, result.Status)
			}
			if test.uncapped && result.Status.ScopeMemoryMax != 0 {
				t.Fatalf("scope-memory.max=%d — this sub-test must exercise an UNCAPPED scope, "+
					"or it proves nothing about a scope with no memory.max", result.Status.ScopeMemoryMax)
			}
			if !test.uncapped && result.Status.ScopeMemoryMax <= 0 {
				t.Fatalf("scope-memory.max=%d, want the requested cap", result.Status.ScopeMemoryMax)
			}
			if observation.swapMax != "0" {
				t.Fatalf("scope memory.swap.max=%q, want \"0\" — without it memory.max bounds memory "+
					"but not memory+swap, and the scope contains nothing on a host with swap", observation.swapMax)
			}
			if result.Status.ScopeSwapCap != WorkerAdmitSwapCapEnforced {
				t.Fatalf("status swap disposition=%q, want %q — the kernel holds the bound, so any other "+
					"value is a claim that contradicts the cgroup", result.Status.ScopeSwapCap, WorkerAdmitSwapCapEnforced)
			}
			if trailer := FormatConfineStatus(result.Status); !strings.Contains(trailer, "scope-swap.max=enforced") {
				t.Fatalf("trailer=%q carries no established swap facet", trailer)
			}
		})
	}
}

// verifies: AIRA-110 — the swap bound is what makes a confine scope's memory.max
// an actual containment bound. This is the measured behaviour from the ticket,
// reproduced through the production launch path.
//
// RED against master on any host with swap: the child allocated 256 MiB inside a
// 32 MiB scope cap, was reclaimed into swap rather than OOM-killed, and exited 0.
//
// Two skips keep it from being porous rather than merely green:
//   - no online swap area, or no swap control: the old behaviour would OOM-kill
//     too, so a pass here would prove nothing.
//   - an ancestor already bounding swap below the allocation: that ancestor, not
//     the scope AIRA created, would be doing the containment — the exact
//     harness-proving false pass this file exists to avoid.
func TestConfineRealSwapBoundMakesScopeCapContainARunaway(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "python3 is unavailable: %v", err)
	}
	const allocation = 256 << 20
	parent := swapUncappedMemoryParent(t, strconv.FormatInt(512<<20, 10))
	if _, err := os.Stat(filepath.Join(parent, "memory.swap.max")); err != nil {
		t.Skipf("no memory.swap.max on this host (%v); an unbounded-swap escape is impossible here, "+
			"so this test could not distinguish the fix from the environment", err)
	}
	if devices := activeSwapDevices(t); devices == 0 {
		t.Skip("no online swap area; a scope with no swap cap would be OOM-killed anyway, " +
			"so this test could not distinguish the fix from the environment")
	}
	if ceiling, bounded := effectiveSwapCeiling(parent); bounded && ceiling < allocation {
		t.Skipf("an ancestor of %s already bounds swap to %d bytes, below the %d-byte allocation; "+
			"that ancestor rather than the scope's own cap would contain the runaway", parent, ceiling, allocation)
	}
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 32 << 20, MemoryReservePinned: true, ScopeMemoryMax: 32 << 20,
		AdmissionMaxWait: 30 * time.Second, PollInterval: 10 * time.Millisecond,
		Argv:     []string{"python3", "-c", "x=bytearray(" + strconv.Itoa(allocation) + "); x[-1]=1"},
		SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "confine swap-containment fixture unavailable: %v", err)
	}
	if result.Exit != 137 {
		t.Fatalf("exit=%d (want 137, SIGKILL by the scope's own OOM) status=%+v — a %d-byte allocation "+
			"inside a 32 MiB memory.max escaped its cap, which is what an unbounded memory.swap.max allows",
			result.Exit, result.Status, allocation)
	}
	if result.Status.ScopeSwapCap != WorkerAdmitSwapCapEnforced {
		t.Fatalf("swap disposition=%q, want %q", result.Status.ScopeSwapCap, WorkerAdmitSwapCapEnforced)
	}
}

// verifies: AIRA-110 — a confine launch whose swap bound cannot be written is
// REFUSED, and the target never starts. Fail-closed, exactly as the memory.max
// and memory.oom.group writes on the same scope already are.
//
// The request carries no cap at all, so this also pins that the swap write is
// UNCONDITIONAL: gate it behind `scopeMemoryMax > 0` and this test goes green
// with a launched target.
func TestConfineSwapCapFailureDoesNotLaunch(t *testing.T) {
	scope := &confineFakeScope{}
	started := false
	deps := confineUnitDeps(scope)
	deps.writeScopeSwapCap = func(Scope) (string, error) {
		return "", errors.New("swap control unavailable")
	}
	deps.start = func(*confineCommand) error {
		started = true
		return errors.New("stop after detecting an unbounded-swap launch attempt")
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "noswap.slice", Argv: []string{"must-not-run"}, Stderr: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE: slice noswap.slice") ||
		!strings.Contains(err.Error(), "memory.swap.max") {
		t.Fatalf("error=%v", err)
	}
	if started {
		t.Fatal("target launch reached after memory.swap.max failed")
	}
}

// verifies: AIRA-110 — the `aira run` launch sites pair the swap bound with the
// memory.max write, and a failed swap bound is terminal there too.
//
// RED against master, which never called a swap writer from this path at all:
// the seam would not fire, the launch would succeed, and both assertions fail.
func TestRunScopeMemoryCapPairsSwapCapAndFailsClosed(t *testing.T) {
	scope := &confineFakeScope{}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: confineFakeBackend{scope: scope}})
	if err != nil {
		t.Fatal(err)
	}
	started := false
	r.startFn = func(*exec.Cmd) error {
		started = true
		return nil
	}
	r.writeScopeMemoryCapFn = func(Scope, int64, int64, bool) error { return nil }
	called := false
	r.writeScopeSwapCapFn = func(Scope) (string, error) {
		called = true
		return "", errors.New("swap control unavailable")
	}
	record, err := r.Launch(context.Background(), Request{
		Argv: []string{"/bin/true"}, NoAdmit: true, ScopeMemoryMax: 32 << 20,
	})
	if !called {
		t.Fatal("a capped run scope was created without any memory.swap.max write; its cap bounds " +
			"memory but not memory+swap")
	}
	if err == nil || !strings.Contains(err.Error(), "E_RUN_CAP_UNAVAILABLE") {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	if started {
		t.Fatal("target started after the swap bound failed")
	}
}

// verifies: AIRA-110 — writeScopeSwapCap fails CLOSED on any error that is not a
// proved-absent control. Only ENOENT is a disposition; everything else is an
// error, because a scope whose controls are unreliable must never be handed out
// claiming containment.
//
// EISDIR (memory.swap.max present but not openable for writing) stands in for
// the permission/IO family: it is reproducible without root and without a real
// cgroup, and it is NOT ENOENT, so a writer that collapsed every open failure
// into classifyAbsentSwapControl would return ("unavailable", nil) here.
func TestWriteScopeSwapCapFailsClosedOnUnwritableControl(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "memory.swap.max"), 0o755); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	scope := &memoryCapFDScope{fd: int(opened.Fd()), reference: filepath.Join(t.TempDir(), "wrong-reference")}
	disposition, err := writeScopeSwapCap(scope)
	if err == nil {
		t.Fatalf("disposition=%q err=nil — an unwritable swap control was reported as an outcome "+
			"instead of failing the launch closed", disposition)
	}
	if disposition != "" {
		t.Fatalf("disposition=%q alongside an error; a failed write must claim nothing", disposition)
	}
	if !strings.Contains(err.Error(), "memory.swap.max") {
		t.Fatalf("error=%v does not name the control that failed", err)
	}
}

// verifies: AIRA-110 — a proved-absent control is still the honest non-error
// disposition, so a kernel without swap accounting does not lose the ability to
// run confined jobs. This is the companion to the fail-closed test above: with
// only that one, collapsing ENOENT into an error would look correct.
func TestWriteScopeSwapCapAbsentControlIsADispositionNotAFailure(t *testing.T) {
	dir := t.TempDir()
	opened, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	scope := &memoryCapFDScope{fd: int(opened.Fd()), reference: filepath.Join(t.TempDir(), "wrong-reference")}
	disposition, err := writeScopeSwapCap(scope)
	if err != nil {
		t.Fatalf("absent memory.swap.max returned an error (%v); a kernel with no swap accounting "+
			"would then be unable to run any confined job at all", err)
	}
	if !IsWorkerAdmitSwapCap(disposition) || disposition == WorkerAdmitSwapCapEnforced {
		t.Fatalf("disposition=%q — an absent control must report a catalogued, non-enforced value; "+
			"\"enforced\" would be a containment claim with nothing behind it", disposition)
	}
}
