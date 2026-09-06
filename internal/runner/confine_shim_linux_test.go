//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shimUnitDeps is the ci-shim counterpart of confineUnitDeps, and the difference
// is the point of it: EVERY cgroup seam PANICS if it is called.
//
// That is what makes the "no cgroup work is attempted" tests non-porous.
// Requirement 2 asks for the cgroup path to be skipped ENTIRELY up front, not
// attempted and gracefully failed, and an implementation that attempted-and-
// recovered would pass a test that merely checked the exit code. These panics
// fail it.
func shimUnitDeps() confineDeps {
	panicSeam := func(name string) func() {
		return func() { panic("ci-shim launch reached the cgroup seam " + name) }
	}
	return confineDeps{
		resolveMode:      func() string { return ConfineModeShim },
		resolveSlicePath: func(string) (string, bool, string) { panicSeam("resolveSlicePath")(); return "", false, "" },
		resolveSlicePathExact: func(string) (string, error) {
			panicSeam("resolveSlicePathExact")()
			return "", nil
		},
		managedUnitPresent: func(string) (bool, error) { panicSeam("managedUnitPresent")(); return false, nil },
		ensureDelegation: func(string) (confineDelegation, error) {
			panicSeam("ensureDelegation")()
			return confineDelegation{}, nil
		},
		newBackend:          func(string) ScopeBackend { panicSeam("newBackend")(); return nil },
		writeOOMGroup:       func(Scope) error { panicSeam("writeOOMGroup")(); return nil },
		writeScopeSwapCap:   func(Scope) (string, error) { panicSeam("writeScopeSwapCap")(); return "", nil },
		writeScopeMemoryCap: func(Scope, int64, int64, bool) error { panicSeam("writeScopeMemoryCap")(); return nil },
		writeScopeCPUWeight: func(Scope, int64) bool { panicSeam("writeScopeCPUWeight")(); return true },
		readCap:             func(string) (int64, bool) { panicSeam("readCap")(); return 0, false },
		readUsage:           func(string) cgroupUsage { panicSeam("readUsage")(); return cgroupUsage{} },
		reportPeak: func(context.Context, ConfineRequest, string, *int64, bool) error {
			panicSeam("reportPeak")()
			return nil
		},
		admit: func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
			return admissionResult{state: "immediate", reserve: 1 << 20, basis: "test"}, nil
		},
		start: func(command *confineCommand) error {
			// Only the verb is rewritten. SysProcAttr is deliberately LEFT ALONE
			// (confineUnitDeps clears it) because Setpgid is what the group-signal
			// behaviour under test depends on.
			command.cmd.Args[1] = "__confine-test-setup"
			return command.Start()
		},
	}
}

// verifies: AIRA-121 requirement 2, ticket test (a)
//
// Counterexample: an implementation that resolves the slice, probes the backend,
// or reads the cap "and handles the failure" panics on one of shimUnitDeps'
// seams. Only a launch that skips every one of them up front passes.
func TestShimConfineLaunchesWithoutTouchingAnyCgroupSeam(t *testing.T) {
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &stderr, Stdout: io.Discard,
	}, shimUnitDeps())
	if err != nil || result.Exit != 0 {
		t.Fatalf("shim confine result=%+v err=%v stderr=%s", result, err, stderr.String())
	}
	if result.Status.Slice != ShimConfineSlice {
		t.Fatalf("slice=%q, want the ci-shim sentinel", result.Status.Slice)
	}
	if result.Status.Containment != ConfineContainmentAdvisory {
		t.Fatalf("containment=%q, want %q", result.Status.Containment, ConfineContainmentAdvisory)
	}
	if result.Status.Cap != ConfineCapUnevaluated || result.Status.CapBytes != 0 {
		t.Fatalf("cap=%q cap_bytes=%d; a shim launch establishes no cap and must never publish the budget as one",
			result.Status.Cap, result.Status.CapBytes)
	}
	if result.Status.PeakRSS != nil {
		t.Fatalf("peak-rss=%v, want unevaluated in shim mode (gate condition C10)", *result.Status.PeakRSS)
	}
	if result.Status.TerminatedBy != ConfineTerminatedNormal {
		t.Fatalf("terminated-by=%q, want normal", result.Status.TerminatedBy)
	}
}

// verifies: AIRA-121 requirement 6, ticket test (e)
//
// The consumer's literal merge-gate invocation shape. The second half is what
// catches a lazy "ignore every resource flag": --memory-max 32G must still
// produce a 32G LEDGER CHARGE, because in shim mode --memory-max declares the
// ledger reservation even though no cgroup write happens.
func TestShimConfineAcceptsResourceFlagsAndChargesDeclaredMemoryMax(t *testing.T) {
	deps := shimUnitDeps()
	var charged int64
	var chargedPinned bool
	deps.admit = func(_ context.Context, path string, request ConfineRequest, reserve int64) (admissionResult, error) {
		if path != ShimConfineSlice {
			t.Errorf("admit path=%q, want the ci-shim sentinel", path)
		}
		charged, chargedPinned = reserve, request.MemoryReservePinned
		return admissionResult{state: "immediate", reserve: reserve, basis: "pinned:client"}, nil
	}
	const declared = int64(32) << 30
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		ScopeMemoryMax: declared, ScopeMemoryHigh: declared / 2,
		MemoryReserve: declared, MemoryReservePinned: true,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if charged != declared || !chargedPinned {
		t.Fatalf("ledger charge=%d pinned=%v, want %d pinned; a declared --memory-max/--memory-reserve must still book the ledger in shim mode",
			charged, chargedPinned, declared)
	}
}

// verifies: AIRA-121 requirement 6
//
// --delegate-ram and --memory-reserve parse and run identically, with no
// shim-specific rejection anywhere.
func TestShimConfineAcceptsDelegateRAM(t *testing.T) {
	deps := shimUnitDeps()
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		DelegateRAM: true, MemoryReserve: 512 << 20, MemoryReservePinned: true,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// verifies: AIRA-123 (superseding AIRA-121 requirement 7 / ticket test (f))
//
// Proven by inspecting the ACTUAL CHILD ENVIRONMENT, never the flag-parsing
// path -- which is what the original ticket demanded and what still matters,
// because the whole mechanism is a variable reaching a conftest.py that reads
// nothing but its presence.
//
// THE RULE INVERTED. AIRA-121 withheld AIRA_AITEST_LIB in shim mode because
// worker-admit could only mean "nest a kernel-enforced cgroup sub-scope" and
// nothing could nest one. AIRA-123 gave worker-admit a ledger-only admission
// mode that genuinely functions with no cgroup, so the coordinates ARE
// published on a --delegate-ram launch: withholding them falls the consumer
// through to plain pytest-xdist, where per-worker RAM is invisible to everything
// and nothing prevents over-subscription at all.
//
// The two negative assertions carry the honesty half. AIRA_AITEST_OUTER_SCOPE
// must NOT be published -- there is no outer cgroup scope, and inventing one
// would be the first place this mode pretended to have a cgroup -- and
// AIRA_CONFINE_SCOPE_ID must not be either.
func TestShimConfineDelegateRAMPublishesTheAitestCoordinatesButNoScope(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "child-env")
	deps := shimUnitDeps()
	request := ConfineRequest{
		Argv:     []string{"/bin/sh", "-c", "env > " + dump},
		SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		RuntimeDir:  t.TempDir(),
		DelegateRAM: true, MemoryReserve: 512 << 20, MemoryReservePinned: true,
		// The supervisor's OWN environment carries stale coordinates, so what
		// the child ends up with must be freshly published rather than inherited.
		Env: append(os.Environ(),
			"AIRA_AITEST_LIB=/some/outer/extraction/dir",
			"AIRA_AITEST_OUTER_SCOPE=/sys/fs/cgroup/aira.slice/.aira-CONFINE-x"),
	}
	if _, err := confineWithDeps(context.Background(), request, deps); err != nil {
		t.Fatalf("shim confine err=%v", err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read child environment: %v", err)
	}
	child := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			child[key] = value
		}
	}
	lib := child["AIRA_AITEST_LIB"]
	if lib == "" {
		t.Fatal("child environment carries no AIRA_AITEST_LIB; the ledger-only backend functions in ci-shim mode, so a --delegate-ram launch must activate aitest rather than fall through to RAM-blind xdist")
	}
	if lib == "/some/outer/extraction/dir" {
		t.Fatalf("AIRA_AITEST_LIB=%q was INHERITED, not published: a stale extraction directory from an outer job is exactly the coordinate resurrection the strip exists to prevent", lib)
	}
	for _, key := range []string{"AIRA_AITEST_WORKER_ADMIT_CMD", "AIRA_AITEST_BOOTSTRAP_CMD"} {
		if child[key] == "" {
			t.Fatalf("child environment carries no %s; the coordinates are published as a set or not at all", key)
		}
	}
	if scope, present := child["AIRA_AITEST_OUTER_SCOPE"]; present {
		t.Fatalf("child environment carries AIRA_AITEST_OUTER_SCOPE=%q; there is no outer cgroup scope in ci-shim mode and publishing one -- inherited or invented -- is the first place this mode would pretend otherwise", scope)
	}
	if id := child["AIRA_CONFINE_SCOPE_ID"]; id != "" {
		t.Fatalf("child environment carries AIRA_CONFINE_SCOPE_ID=%q; there is no scope in ci-shim mode", id)
	}
}

// verifies: AIRA-123
//
// The NON-delegate arm keeps AIRA-121's unconditional STRIP, unchanged and for
// the unchanged reason: a shim confine nested inside an outer aitest-enabled
// process must not hand its child stale coordinates pointing at the outer job's
// extraction directory and relay binary.
func TestShimConfineWithoutDelegateRAMStillStripsEveryAitestCoordinate(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "child-env")
	deps := shimUnitDeps()
	request := ConfineRequest{
		Argv:     []string{"/bin/sh", "-c", "env > " + dump},
		SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
		RuntimeDir:    t.TempDir(),
		MemoryReserve: 512 << 20, MemoryReservePinned: true,
		Env: append(os.Environ(),
			"AIRA_AITEST_LIB=/some/outer/extraction/dir",
			"AIRA_AITEST_BOOTSTRAP_CMD=/usr/bin/aira",
			"AIRA_AITEST_OUTER_SCOPE=/sys/fs/cgroup/aira.slice/.aira-CONFINE-x"),
	}
	if _, err := confineWithDeps(context.Background(), request, deps); err != nil {
		t.Fatalf("shim confine err=%v", err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read child environment: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "AIRA_AITEST_") {
			t.Fatalf("child environment carries %q; a non-delegate launch must strip every aitest coordinate, not merely skip appending one", line)
		}
	}
}

// verifies: AIRA-121 gate condition C5
//
// The mode resolver fails to the STRICTER path for every unusable record, so an
// already-installed box, a corrupted record, or a shim record with no budget can
// never turn a real install into an unconfined one.
func TestResolveConfineModeFailsToRealForEveryUnusableRecord(t *testing.T) {
	dir := t.TempDir()
	for _, test := range []struct {
		name    string
		content string
		write   bool
		want    string
	}{
		{name: "absent", want: ConfineModeReal},
		{name: "malformed", write: true, content: "{not json", want: ConfineModeReal},
		{name: "wrong schema", write: true, content: `{"schema":2,"mode":"ci-shim","shim_budget_bytes":1,"shim_budget_source":"declared"}`, want: ConfineModeReal},
		{name: "unknown mode", write: true, content: `{"schema":1,"mode":"experimental"}`, want: ConfineModeReal},
		{name: "shim without budget", write: true, content: `{"schema":1,"mode":"ci-shim"}`, want: ConfineModeReal},
		{name: "shim with uncatalogued source", write: true, content: `{"schema":1,"mode":"ci-shim","shim_budget_bytes":1024,"shim_budget_source":"guessed"}`, want: ConfineModeReal},
		{name: "real", write: true, content: `{"schema":1,"mode":"real-slice"}`, want: ConfineModeReal},
		{name: "usable shim", write: true, content: `{"schema":1,"mode":"ci-shim","shim_budget_bytes":1024,"shim_budget_source":"declared"}`, want: ConfineModeShim},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, test.name+".json")
			if test.write {
				if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(InstallModeFileEnv, path)
			resetConfineModeCache()
			t.Cleanup(resetConfineModeCache)
			if got := ResolveConfineMode(); got != test.want {
				t.Fatalf("ResolveConfineMode()=%q, want %q", got, test.want)
			}
		})
	}
}

// verifies: AIRA-121 gate condition C11
//
// The record lives beside state.db in the AIRA state DIRECTORY, not at the root
// of the XDG state home, which is shared with every other application.
func TestInstallModeRecordLivesBesideTheDatabase(t *testing.T) {
	if got, want := InstallModePathFor("/home/x/.local/state"), "/home/x/.local/state/aira/install-mode.json"; got != want {
		t.Fatalf("InstallModePathFor=%q, want %q", got, want)
	}
}

// verifies: AIRA-121 requirement 3
//
// The trailer projection is the single operator-facing surface, so this is the
// test that keeps every consumer of it (foreground trailer, --list, --status,
// the detached record, the TUI) honest at once.
func TestFormatConfineStatusDistinguishesAdvisoryFromEnforcedContainment(t *testing.T) {
	shim := ConfineStatus{
		Slice: ShimConfineSlice, Containment: ConfineContainmentAdvisory,
		Cap: ConfineCapUnevaluated, Scope: ConfineScopeUnverified,
		OOMGroup: ConfineOOMGroupUnverified, Priorities: ConfinePrioritiesApplied,
		AdmissionState: "immediate", ReserveBytes: 2 << 30, TerminatedBy: ConfineTerminatedNormal,
	}
	line := FormatConfineStatus(shim)
	for _, want := range []string{
		"containment=advisory(ci-shim,no-cgroup,no-kill-backstop)",
		"slice=ci-shim", "cap=unevaluated", "scope=unverified", "peak-rss=unevaluated",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("shim trailer %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "containment=enforced") {
		t.Fatalf("shim trailer claims enforced containment: %q", line)
	}
	// The specific fabrication section 6 forbids: the budget must never be
	// rendered as this scope's enforced cap.
	if strings.Contains(line, "cap=enforced") {
		t.Fatalf("shim trailer renders an enforced cap: %q", line)
	}

	real := shim
	real.Slice, real.Containment, real.Cap, real.CapBytes = "aira.slice", ConfineContainmentEnforced, ConfineCapEnforced, 64<<30
	realLine := FormatConfineStatus(real)
	if !strings.Contains(realLine, "containment=enforced") || strings.Contains(realLine, "advisory") {
		t.Fatalf("real trailer %q does not report enforced containment", realLine)
	}

	// An unset facet reads as unevaluated, never as enforced: a launch that
	// aborted before the scope existed established no containment at all.
	if line := FormatConfineStatus(ConfineStatus{Slice: "aira.slice"}); !strings.Contains(line, "containment=unevaluated") {
		t.Fatalf("unset containment rendered as %q, want unevaluated", line)
	}
}

// verifies: AIRA-121 requirement 3, gate condition C2
//
// Shim --list renders from the daemon's granted-waiter registry alone, so an
// operator surface exists at all in a mode with no cgroup directory to read.
func TestShimConfineListRendersPendingRowsFromTheRegistry(t *testing.T) {
	scopeID := confineScopeID("gate", "session-a", false)
	result := ShimConfineList([]ConfineRegistryEntry{{ScopeID: scopeID}})
	if result.Verdict != "ok" {
		t.Fatalf("verdict=%q reason=%q, want ok", result.Verdict, result.Reason)
	}
	if len(result.Scopes) != 1 {
		t.Fatalf("scopes=%d, want 1", len(result.Scopes))
	}
	row := result.Scopes[0]
	if row.Name != "gate" || row.Owner != "session-a" || !row.Pending {
		t.Fatalf("row=%+v; name/owner must decode from the scope id and the row must be Pending", row)
	}
	if row.Populated != nil || row.RSSBytes != nil || row.Cap != nil {
		t.Fatalf("row=%+v fabricates a cgroup reading in a mode with no cgroup", row)
	}
}

// verifies: AIRA-121 gate condition C2
//
// --kill NEVER reports a kill in shim mode. It refuses, and the refusal names
// the supervisor PID to signal instead, which is the only teardown mechanism
// that exists here.
func TestShimConfineKillRefusesAndNamesTheSupervisorPID(t *testing.T) {
	scopeID := confineScopeID("gate", "session-a", false)
	_, _, _, _, ok := parseConfineScopeID(scopeID)
	if !ok {
		t.Fatalf("scope id %q does not parse", scopeID)
	}
	registry := []ConfineRegistryEntry{{ScopeID: scopeID}}
	result, err := ShimConfineKill("gate", "session-a", false, registry)
	if err == nil {
		t.Fatalf("shim kill reported %+v; it must never claim a kill it cannot perform", result)
	}
	if !strings.Contains(err.Error(), CodeConfineKillUnconfirmed) {
		t.Fatalf("error=%v, want %s", err, CodeConfineKillUnconfirmed)
	}
	if !strings.Contains(err.Error(), "kill "+itoa(os.Getpid())) {
		t.Fatalf("error=%v does not name this process's pid as the supervisor to signal", err)
	}
	if result.Status != "" {
		t.Fatalf("result=%+v; a refusal must carry no status", result)
	}

	// The ownership guard runs BEFORE the PID is disclosed: a refusal that leaked
	// a foreign supervisor's pid would let an unauthorised caller do by hand what
	// the guard just refused.
	_, foreignErr := ShimConfineKill("gate", "session-b", false, registry)
	if foreignErr == nil || !strings.Contains(foreignErr.Error(), CodeConfineOwnerUnverified) {
		t.Fatalf("cross-owner kill error=%v, want %s", foreignErr, CodeConfineOwnerUnverified)
	}
	if strings.Contains(foreignErr.Error(), "kill "+itoa(os.Getpid())) {
		t.Fatalf("ownership refusal leaked the supervisor pid: %v", foreignErr)
	}

	if _, err := ShimConfineKill("nothing-like-this", "session-a", false, registry); err == nil ||
		!strings.Contains(err.Error(), CodeConfineNotFound) {
		t.Fatalf("unmatched selector error=%v, want %s", err, CodeConfineNotFound)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// verifies: AIRA-121 requirement 2
//
// A shim job whose daemon is DOWN runs with admission=unevaluated on its
// trailer, mirroring the real path's honesty rather than fabricating a grant.
// Stated explicitly in gate condition C5 and asserted here so the behaviour
// cannot drift into a silent "admitted".
func TestShimConfineWithNoDaemonReportsUnevaluatedAdmission(t *testing.T) {
	deps := shimUnitDeps()
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "unevaluated", reason: "slice-not-found"}, nil
	}
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &stderr, Stdout: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Status.Admission != ConfineAdmissionUnevaluated {
		t.Fatalf("admission=%q, want unevaluated", result.Status.Admission)
	}
	if !strings.Contains(stderr.String(), "admission=unevaluated") {
		t.Fatalf("trailer %q does not report the unevaluated admission", stderr.String())
	}
}

// verifies: AIRA-121 requirement 2
//
// A failing admission never launches anything.
func TestShimConfineRejectedAdmissionStartsNoChild(t *testing.T) {
	deps := shimUnitDeps()
	started := false
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "rejected"}, errors.New("E_ADMIT_SATURATED: no capacity")
	}
	deps.start = func(*confineCommand) error {
		started = true
		return nil
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{"must-not-run"}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_ADMIT_SATURATED") {
		t.Fatalf("err=%v", err)
	}
	if started {
		t.Fatal("a rejected shim admission launched the job anyway")
	}
}

func waitForFile(t *testing.T, path string, budget time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
