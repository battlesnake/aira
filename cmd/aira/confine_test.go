package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

func TestParseConfineArgsKeepsTargetByteTransparent(t *testing.T) {
	positional, options, err := parseArgs("confine", []string{"--slice", "safe.slice", "--name", "job", "--", "pytest", "--json", "--name", "child"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positional, []string{"pytest", "--json", "--name", "child"}) {
		t.Fatalf("target=%q", positional)
	}
	if options["slice"] != "safe.slice" || options["name"] != "job" {
		t.Fatalf("options=%v", options)
	}

	args, jsonOutput := removeJSON([]string{"confine", "--", "tool", "--json"})
	if jsonOutput || !reflect.DeepEqual(args, []string{"confine", "--", "tool", "--json"}) {
		t.Fatalf("removeJSON args=%q json=%v", args, jsonOutput)
	}
}

func TestParseConfineManagementAndJSON(t *testing.T) {
	args, jsonOutput := removeJSON([]string{"confine", "--list", "--owner", "session-a", "--json"})
	if !jsonOutput {
		t.Fatal("management --json was not recognized")
	}
	positional, options, err := parseArgs("confine", args[1:])
	if err != nil || len(positional) != 0 || options["list"] != "true" || options["owner"] != "session-a" {
		t.Fatalf("positional=%v options=%v err=%v", positional, options, err)
	}
	_, options, err = parseArgs("confine", []string{"--kill", "CONFINE-job-1-a", "--steal", "--slice", "aira.slice"})
	if err != nil || options["kill"] != "CONFINE-job-1-a" || options["steal"] != "true" {
		t.Fatalf("options=%v err=%v", options, err)
	}
	for _, argv := range [][]string{{"--list", "--kill", "x"}, {"--list", "--steal"}, {"--kill"}, {"--list", "--owner", "../bad"}} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID:") {
			t.Fatalf("argv=%v err=%v", argv, err)
		}
	}
}

func TestConfineOwnerDerivationChain(t *testing.T) {
	t.Setenv("AIRA_CONFINE_OWNER", "environment-owner")
	if owner, err := resolveConfineOwner(context.Background(), "flag-owner"); err != nil || owner != "flag-owner" {
		t.Fatalf("flag owner=%q err=%v", owner, err)
	}
	if owner, err := resolveConfineOwner(context.Background(), ""); err != nil || owner != "environment-owner" {
		t.Fatalf("environment owner=%q err=%v", owner, err)
	}
	t.Setenv("AIRA_CONFINE_OWNER", "")
	projectRoot := t.TempDir()
	if err := exec.Command("git", "-C", projectRoot, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(projectRoot, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(projectRoot)
	project, err := app.Discover(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if owner, err := resolveConfineOwner(context.Background(), ""); err != nil || owner != project.WorktreeID {
		t.Fatalf("project owner=%q want=%q err=%v", owner, project.WorktreeID, err)
	}
	// AIRA-23: the tail of the chain is no longer the literal "unknown". With no
	// flag, no environment and no discoverable project, the owner is INFERRED
	// from the launch directory — marked, so the kill guard still treats it as
	// unattested, but printed by --list instead of the useless "unknown" that let
	// one session nearly kill two siblings' jobs.
	last := t.TempDir()
	t.Chdir(last)
	inferred, err := resolveConfineOwner(context.Background(), "")
	if err != nil {
		t.Fatalf("inferred owner err=%v", err)
	}
	if inferred != runner.InferConfineOwner(last) || !strings.HasPrefix(inferred, runner.ConfineInferredOwnerPrefix) {
		t.Fatalf("owner=%q want the marked inference for %q", inferred, last)
	}
	if runner.ConfineOwnerIsAttested(inferred) {
		t.Fatalf("inferred owner %q must never be attested", inferred)
	}
	// A caller can never SUPPLY the inferred form: '@' is outside the identity
	// alphabet, so an inferred owner cannot be forged into an attested one, on
	// the command line or on the wire.
	if _, err := resolveConfineOwner(context.Background(), inferred); err == nil {
		t.Fatalf("explicit owner %q was accepted; the inferred marker is forgeable", inferred)
	}
	if _, err := resolveConfineOwner(context.Background(), "bad/owner"); err == nil {
		t.Fatal("invalid explicit owner accepted")
	}
}

func TestConfineLaunchOwnerThreadsIntoAdmissionRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	t.Setenv("AIRA_CONFINE_OWNER", "session-launch")
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if request.Owner != "session-launch" {
			t.Fatalf("owner=%q", request.Owner)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--owner", "session-launch", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineManagementDispatchesOutsideProject(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	called := false
	dispatch := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		called = true
		if scope.Root != "" || request.Verb != "confine-kill" || request.Args["owner"] != "session-a" || request.Args["selector"] != "job" || request.Args["steal"] != true {
			t.Fatalf("scope=%+v request=%+v", scope, request)
		}
		return core.Response{OK: true, Code: "OK", Data: runner.ConfineKillResult{Status: "killed", ScopeID: "CONFINE-job-1-a", Name: "job", Owner: "session-a"}}
	})
	var stdout, stderr bytes.Buffer
	if exit := runWithInputDispatcher([]string{"confine", "--kill", "job", "--steal"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !called {
		t.Fatalf("exit=%d called=%v stdout=%q stderr=%q", exit, called, stdout.String(), stderr.String())
	}
}

func TestConfineListRendersHumanTableAndAllowsJSON(t *testing.T) {
	zero, pid, rss, age, cap := 0, 123, int64(4096), int64(2), "8192"
	result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{{
		Name: "husk", Owner: runner.ConfineUnknownOwner, SupervisorPID: &pid,
		ScopeID: "CONFINE-husk-123-abc", Populated: &zero, RSSBytes: &rss, AgeSeconds: &age, Cap: &cap,
	}}}
	dispatch := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
		if request.Verb != "confine-list" {
			t.Fatalf("request=%+v", request)
		}
		return core.Response{OK: true, Code: "OK", Data: result}
	})
	var stdout, stderr bytes.Buffer
	if exit := runWithInputDispatcher([]string{"confine", "--list"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !strings.Contains(stdout.String(), "SUPERVISOR-PID") || !strings.Contains(stdout.String(), "husk") || !strings.Contains(stdout.String(), "  0") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runWithInputDispatcher([]string{"confine", "--list", "--json"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !strings.Contains(stdout.String(), `"populated":0`) {
		t.Fatalf("json exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

// AIRA-102: the LIVE column is what an operator reads to decide whether a job is
// running, and it must come from the SUBTREE-aware signal. A job that relocates
// its processes into a child cgroup -- an aitest suite draining into
// `.aira-supervisor`, or `podman run --cgroups=split` moving everything into
// `<scope>/runtime` plus the container payload -- has an empty LEAF count while
// very much alive, and the old single POPULATED column rendered exactly that as
// a bare 0.
//
// verifies: AIRA-102
func TestRenderConfineListLiveColumnUsesSubtreePopulation(t *testing.T) {
	pid, zero, rss, age := 4242, 0, int64(1<<20), int64(7)
	scopeCap := "2147483648"
	live, dead := true, false

	render := func(t *testing.T, subtree *bool) string {
		t.Helper()
		result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{{
			Name: "split-job", Owner: runner.ConfineUnknownOwner, SupervisorPID: &pid,
			ScopeID: "CONFINE-split-job-4242-abc", Populated: &zero, SubtreePopulated: subtree,
			RSSBytes: &rss, AgeSeconds: &age, Cap: &scopeCap,
		}}}
		dispatch := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, _ core.Request) core.Response {
			return core.Response{OK: true, Code: "OK", Data: result}
		})
		var stdout, stderr bytes.Buffer
		if exit := runWithInputDispatcher([]string{"confine", "--list"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 {
			t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
		}
		return stdout.String()
	}

	output := render(t, &live)
	for _, want := range []string{"LIVE", "LEAF-PROCS", "yes"} {
		if !strings.Contains(output, want) {
			t.Fatalf("a RUNNING split job's row lacks %q: %q", want, output)
		}
	}
	// The whole point: a leaf count of 0 must no longer be the thing an operator
	// reads as "not running".
	if strings.Contains(output, "POPULATED") {
		t.Fatalf("the ambiguous POPULATED column survived: %q", output)
	}
	if output = render(t, &dead); !strings.Contains(output, "no") {
		t.Fatalf("a genuinely empty scope does not render LIVE=no: %q", output)
	}
	// An unreadable population is unevaluated, never a fabricated "no".
	if output = render(t, nil); !strings.Contains(output, "unevaluated") {
		t.Fatalf("an unreadable population must render unevaluated, not a guess: %q", output)
	}
}

func TestRenderConfineListReserveSummary(t *testing.T) {
	tests := []struct {
		name    string
		reserve *runner.ConfineSliceReserve
		want    string
	}{
		{
			name:    "present",
			reserve: &runner.ConfineSliceReserve{GrantedBytes: 3 << 30, CeilingBytes: 12 << 30, Jobs: 1},
			want:    "slice reserve: 3G granted / 12G ceiling across 1 admitted job\n",
		},
		{
			// An idle slice (0 granted) renders a genuine zero as "0B", never
			// "unknown" — reserve/ceiling here are always established values.
			name:    "idle-zero",
			reserve: &runner.ConfineSliceReserve{GrantedBytes: 0, CeilingBytes: 12 << 30, Jobs: 0},
			want:    "slice reserve: 0B granted / 12G ceiling across 0 admitted jobs\n",
		},
		{name: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}, SliceReserve: test.reserve}
			var stdout, stderr bytes.Buffer
			exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr)
			if exit != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
			}
			if gotSummary := strings.Contains(stdout.String(), "slice reserve:"); gotSummary != (test.reserve != nil) {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if test.want != "" && !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("stdout=%q, want summary %q", stdout.String(), test.want)
			}
		})
	}
}

func TestConfineDaemonDownRequiresSteal(t *testing.T) {
	d := &daemonDispatcher{}
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
	}
	d.resolveConfineSlice = func(string) (string, string, error) { return "aira.slice", filepath.Join("/unused", "slice"), nil }
	d.killConfine = func(_ context.Context, _ string, selector, caller string, steal bool, _ []runner.ConfineRegistryEntry) (runner.ConfineKillResult, error) {
		if selector != "job" || caller != "session-a" {
			t.Fatalf("selector=%q caller=%q", selector, caller)
		}
		if !steal {
			return runner.ConfineKillResult{}, errors.New(runner.CodeConfineOwnerUnverified + ": daemon down")
		}
		return runner.ConfineKillResult{Status: "killed", ScopeID: "CONFINE-job-1-a"}, nil
	}
	base := core.Request{Verb: "confine-kill", Args: map[string]any{"selector": "job", "owner": "session-a", "steal": false}}
	if response := d.Dispatch(context.Background(), daemon.WorktreeScope{}, base); response.Code != runner.CodeConfineOwnerUnverified || response.OK {
		t.Fatalf("without steal response=%+v", response)
	}
	base.Args["steal"] = true
	if response := d.Dispatch(context.Background(), daemon.WorktreeScope{}, base); !response.OK || response.Code != "OK" {
		t.Fatalf("with steal response=%+v", response)
	}
}

func TestConfineRunsDirectlyWithoutProjectOrDispatcher(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	called := false
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		called = true
		if request.Slice != "test.slice" || request.Name != "job" || !reflect.DeepEqual(request.Argv, []string{"tool", "--flag"}) {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{Exit: 27}, nil
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--slice", "test.slice", "--name", "job", "--", "tool", "--flag"}, &stdout, &stderr, strings.NewReader("stdin"))
	if !called || exit != 27 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("called=%v exit=%d stdout=%q stderr=%q", called, exit, stdout.String(), stderr.String())
	}
}

// covers: task-57 confine CLI parsing and request threading.
func TestConfineMemoryFlagsThreadIntoRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if request.ScopeMemoryMax != 32<<20 || request.ScopeMemoryHigh != 16<<20 {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--memory-max", "32M", "--memory-high", "16m", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineAdmitTimeoutValidationAndThreading(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	// Sub-1ms values (500us, 999us) must ALSO be rejected: the wire value is
	// max_wait_ms and Milliseconds() truncates them to 0, which would reintroduce
	// the deferred zero-wait evaluator race (a false "saturated" reject).
	for _, raw := range []string{"0", "-1s", "500us", "999us"} {
		if _, _, err := parseArgs("confine", []string{"--admit-timeout", raw, "--", "true"}); err == nil || !strings.Contains(err.Error(), "--admit-timeout") {
			t.Fatalf("--admit-timeout %q err=%v, want CLI rejection", raw, err)
		}
	}
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if request.AdmissionMaxWait != 25*time.Millisecond {
			t.Fatalf("AdmissionMaxWait=%s want 25ms", request.AdmissionMaxWait)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--admit-timeout", "25ms", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineDelegateRAMFlagThreadsIntoRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if !request.DelegateRAM {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--delegate-ram", "--memory-reserve", "512M", "--", "pytest", "-q"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineReserveCLIParsesPinnedRequestAndHoldsUntilStdinClose(t *testing.T) {
	original := reserveConfined
	t.Cleanup(func() { reserveConfined = original })
	granted := make(chan runner.ConfineReserveRequest, 1)
	reserveConfined = func(_ context.Context, request runner.ConfineReserveRequest) (*runner.ConfineReservation, error) {
		granted <- request
		return &runner.ConfineReservation{State: "immediate", Reserve: request.Bytes, Basis: "pinned:client"}, nil
	}
	reader, writer := io.Pipe()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runWithInput([]string{"confine-reserve", "--bytes", "512M", "--pinned", "--signature", "pytest:test_x.py::test_y", "--max-wait", "5s"}, &stdout, &stderr, reader)
	}()
	request := <-granted
	if request.Bytes != 512<<20 || !request.Pinned || request.Signature != "pytest:test_x.py::test_y" || request.MaxWait != 5*time.Second {
		t.Fatalf("request=%+v", request)
	}
	select {
	case exit := <-done:
		t.Fatalf("helper exited before stdin close: %d", exit)
	case <-time.After(20 * time.Millisecond):
	}
	_ = writer.Close()
	if exit := <-done; exit != 0 || stdout.String() != "granted reserve=536870912 basis=pinned:client\n" || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

// AIRA-62. This replaces TestConfinePinnedReserveFlagEnvironmentAndMemoryMaxPrecedence,
// whose "memory-max" case asserted `--memory-reserve 12M --memory-max 16M` reached the
// runner as MemoryReserve=16M. That assertion WAS the bug, pinned: the CLI pre-resolved
// the ledger charge, which made the runner's delegate-ram carve-out dead code.
//
// The invariant that test really protected — a non-delegate --memory-max UP-CHARGES the
// charge to the cap — is not dropped; it moves to `wantCharge` below, where it is now
// decided, and is independently pinned at the runner in
// TestConfineReserveResolutionAcrossDelegateAndMemoryMax.
//
// Both halves are asserted deliberately, so neither layer can drift silently:
//   - TRANSCRIPTION: what the operator typed reaches ConfineRequest verbatim. A
//     reintroduced CLI substitution fails here.
//   - RESOLVED CHARGE: that request run through the production resolver. A broken
//     resolution rule fails here.
//
// verifies: AIRA-62 the CLI transcribes and never resolves the admission charge.
func TestConfineReserveTranscriptionAndResolvedChargeAcrossFlagsAndEnvironment(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	const overhead = runner.DefaultDelegateRAMOverhead
	for _, test := range []struct {
		name             string
		env              string
		argv             []string
		wantReserve      int64
		wantPinned       bool
		wantMax          int64
		wantDelegate     bool
		wantCharge       int64
		wantChargePinned bool
	}{
		// Rows 1-3 of the plan's truth table: non-delegate, unchanged by AIRA-62.
		{
			name: "flag beats environment", env: "8M",
			argv:        []string{"confine", "--memory-reserve", "12M", "--", "true"},
			wantReserve: 12 << 20, wantPinned: true,
			wantCharge: 12 << 20, wantChargePinned: true,
		},
		{
			name: "environment supplies a pinned reserve", env: "8M",
			argv:        []string{"confine", "--", "true"},
			wantReserve: 8 << 20, wantPinned: true,
			wantCharge: 8 << 20, wantChargePinned: true,
		},
		{
			// The documented up-charge (internal/core/skill.go:318): "--memory-max N on a
			// non-delegate job UP-CHARGES the admission reserve to N". Preserved exactly,
			// but it is now the RESOLVER that does it, not the CLI.
			name: "non-delegate memory-max up-charges the declared reserve", env: "8M",
			argv:        []string{"confine", "--memory-reserve", "12M", "--memory-max", "16M", "--", "true"},
			wantReserve: 12 << 20, wantPinned: true, wantMax: 16 << 20,
			wantCharge: 16 << 20, wantChargePinned: true,
		},
		{
			name: "non-delegate memory-max alone up-charges from unpinned", env: "",
			argv:        []string{"confine", "--memory-max", "16M", "--", "true"},
			wantReserve: 0, wantPinned: false, wantMax: 16 << 20,
			wantCharge: 16 << 20, wantChargePinned: true,
		},
		// Rows 4-6: delegate-ram. The last three are the AIRA-62 bug.
		{
			name: "delegate honours an explicit reserve", env: "",
			argv:        []string{"confine", "--delegate-ram", "--memory-reserve", "512M", "--", "pytest"},
			wantReserve: 512 << 20, wantPinned: true, wantDelegate: true,
			wantCharge: 512 << 20, wantChargePinned: true,
		},
		{
			// The ticket's reproduction: a 64x over-reservation before AIRA-62.
			name: "delegate memory-max never overrides an explicit reserve", env: "",
			argv:        []string{"confine", "--delegate-ram", "--memory-max", "32G", "--memory-reserve", "512M", "--", "pytest"},
			wantReserve: 512 << 20, wantPinned: true, wantMax: 32 << 30, wantDelegate: true,
			wantCharge: 512 << 20, wantChargePinned: true,
		},
		{
			// A delegate cap is a containment CEILING, not a reserve, so with no
			// declared reserve the charge is the framework overhead -- identical to a
			// delegate job that passes no --memory-max at all.
			name: "delegate memory-max alone charges the overhead not the cap", env: "",
			argv:        []string{"confine", "--delegate-ram", "--memory-max", "32G", "--", "pytest"},
			wantReserve: 0, wantPinned: false, wantMax: 32 << 30, wantDelegate: true,
			wantCharge: overhead, wantChargePinned: true,
		},
		{
			// Sol P2: the environment contract must survive under delegate-ram too.
			name: "delegate memory-max never overrides an environment reserve", env: "8M",
			argv:        []string{"confine", "--delegate-ram", "--memory-max", "32G", "--", "pytest"},
			wantReserve: 8 << 20, wantPinned: true, wantMax: 32 << 30, wantDelegate: true,
			wantCharge: 8 << 20, wantChargePinned: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AIRA_CONFINE_RESERVE", test.env)
			var captured runner.ConfineRequest
			runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
				captured = request
				return runner.ConfineResult{}, nil
			}
			if exit := runWithInput(test.argv, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
				t.Fatalf("exit=%d", exit)
			}
			if captured.MemoryReserve != test.wantReserve || captured.MemoryReservePinned != test.wantPinned ||
				captured.ScopeMemoryMax != test.wantMax || captured.DelegateRAM != test.wantDelegate {
				t.Fatalf("transcription: reserve=%d pinned=%v max=%d delegate=%v, want %d/%v/%d/%v",
					captured.MemoryReserve, captured.MemoryReservePinned, captured.ScopeMemoryMax, captured.DelegateRAM,
					test.wantReserve, test.wantPinned, test.wantMax, test.wantDelegate)
			}
			charge, chargePinned := runner.ResolveConfineReserve(captured)
			if charge != test.wantCharge || chargePinned != test.wantChargePinned {
				t.Fatalf("resolved charge=%d pinned=%v, want %d/%v", charge, chargePinned, test.wantCharge, test.wantChargePinned)
			}
		})
	}
}

// The exact reproduction recorded in the AIRA-62 ticket body.
//
// Named for what it actually proves (build-review P2): this covers PARSE + RESOLVE --
// the argv the ticket reports, transcribed into a ConfineRequest and run through the
// production resolver. It does NOT prove the number reaches the daemon; runConfined is
// mocked here. That last hop is covered in internal/runner by
// TestConfineAdmitWireFrameCarriesTheResolvedChargeNotTheCap, which drives the real
// admitConfine over a real socket and decodes the frame.
//
// verifies: AIRA-62 the reported argv resolves to the declared reserve, not the cap.
func TestConfineDelegateRAMWithMemoryMaxResolvesTheReserveNotTheCap(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	t.Setenv("AIRA_CONFINE_RESERVE", "")
	var captured runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		captured = request
		return runner.ConfineResult{}, nil
	}
	// `aira confine --delegate-ram --memory-max 32G --memory-reserve 512M` charged the
	// admission ledger 32G, not the 512M the caller explicitly asked for (AIRA-62).
	argv := []string{"confine", "--delegate-ram", "--memory-max", "32G", "--memory-reserve", "512M", "--", "make", "merge-gate"}
	if exit := runWithInput(argv, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	charge, pinned := runner.ResolveConfineReserve(captured)
	if charge != 512<<20 || !pinned {
		t.Fatalf("ledger charge=%d pinned=%v, want %d pinned (the cap %d must not be charged)",
			charge, pinned, int64(512<<20), int64(32<<30))
	}
	// The cap itself is untouched: AIRA-62 changes the CHARGE, never the containment.
	if captured.ScopeMemoryMax != 32<<30 {
		t.Fatalf("ScopeMemoryMax=%d, want %d", captured.ScopeMemoryMax, int64(32<<30))
	}
}

// verifies: task-57 confine rejects invalid cap relationships before launch.
func TestConfineMemoryFlagValidation(t *testing.T) {
	for _, argv := range [][]string{
		{"--memory-high", "1M", "--", "true"},
		{"--memory-high", "0", "--", "true"},
		{"--memory-max", "0", "--", "true"},
		{"--memory-max", "1023K", "--", "true"},
		{"--memory-max", "2M", "--memory-high", "3M", "--", "true"},
		{"--memory-max", "garbage", "--", "true"},
		{"--memory-reserve", "garbage", "--", "true"},
		{"--memory-reserve", "1023K", "--", "true"},
	} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID:") {
			t.Fatalf("parseArgs(%q) err=%v", argv, err)
		}
	}
}

func TestConfineInfraErrorUsesDedicatedExitAndDoesNotDispatch(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		return runner.ConfineResult{}, errors.New("E_CONFINE_UNAVAILABLE: slice missing.slice: not found")
	}
	var stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--slice", "missing.slice", "--", "must-not-run"}, io.Discard, &stderr, strings.NewReader("")); exit != 4 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestParseConfineArgsRejectsUnsafeShapes(t *testing.T) {
	for _, argv := range [][]string{
		{"true"},
		{"--", ""},
		{"--unsafe-unconfined", "--", "true"},
		{"--slice", "one", "--slice", "two", "--", "true"},
		{"positional", "--", "true"},
	} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
			t.Fatalf("parseArgs(%q) err=%v", argv, err)
		}
	}
}

func TestConfineDescriptorIsClientExecuteWithoutMCP(t *testing.T) {
	canonical, route := core.Classify("confine", "")
	if canonical != "confine" || route != core.RouteClient {
		t.Fatalf("classify=%q/%v", canonical, route)
	}
	var found bool
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name != "confine" {
			continue
		}
		found = true
		if descriptor.Safety != core.SafetyExecute || descriptor.MCPTool != "" || descriptor.Include || descriptor.Usage != "confine [--slice S] [--name N] [--owner ID] [--memory-reserve S] [--memory-max S] [--memory-high S] [--admit-timeout D] [--delegate-ram] [--exclusive] [--detach] -- <argv...>" {
			t.Fatalf("descriptor=%+v", descriptor)
		}
	}
	if !found {
		t.Fatal("confine descriptor missing")
	}
}

// confine-status is CLI-only for the same reason confine is, and for one of its
// own: it reads a durable filesystem record, so routing it through the daemon
// would make AIRA-22's survivability verb depend on the component most likely to
// have been restarted during the pause it exists to survive.
//
// verifies: AIRA-22
func TestConfineStatusDescriptorIsCLIOnlyWithoutMCP(t *testing.T) {
	canonical, route := core.Classify("confine-status", "")
	if canonical != "confine-status" || route != core.RouteClient {
		t.Fatalf("classify=%q/%v", canonical, route)
	}
	var found bool
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name != "confine-status" {
			continue
		}
		found = true
		if descriptor.MCPTool != "" || descriptor.Include {
			t.Fatalf("confine-status must not be a generated action or MCP tool: %+v", descriptor)
		}
		if descriptor.Safety != core.SafetyRead {
			t.Fatalf("confine-status is a read: %+v", descriptor)
		}
		if descriptor.Usage != "confine --status [<name|supervisor-pid|scope-id>] [--owner ID] [--json]" {
			t.Fatalf("usage=%q", descriptor.Usage)
		}
	}
	if !found {
		t.Fatal("confine-status descriptor missing")
	}
	response := core.New(nil).Do(context.Background(), core.Request{Verb: "confine-status", Args: map[string]any{"selector": "gate"}})
	if response.OK || response.Code != "E_CONFINE_UNAVAILABLE" {
		t.Fatalf("confine-status must refuse dispatcher routing: %+v", response)
	}
}

func TestConfineReserveDescriptorIsCLIOnly(t *testing.T) {
	canonical, route := core.Classify("confine-reserve", "")
	if canonical != "confine-reserve" || route != core.RouteClient {
		t.Fatalf("classify=%q/%v", canonical, route)
	}
	response := core.New(nil).Do(context.Background(), core.Request{Verb: "confine-reserve", Args: map[string]any{
		"bytes": "1", "pinned": true, "signature": "pytest:x",
	}})
	if response.Code != "E_CONFINE_UNAVAILABLE" || response.OK {
		t.Fatalf("response=%+v", response)
	}
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name == "confine-reserve" && (descriptor.Include || descriptor.MCPTool != "") {
			t.Fatalf("descriptor=%+v", descriptor)
		}
	}
}

// verifies: AIRA-103's `slice ceiling:` line. It exists so an operator waiting on
// admission can tell external system memory pressure from "the slice happens to
// be full of AIRA's own jobs", which the reserve line above it cannot
// distinguish. Every case pins the honesty rule: nothing is printed when the
// subsystem is off, an unevaluated ceiling prints its REASON and never a number,
// observe mode says plainly that nothing was applied, and an unestablished
// MemAvailable prints no figure rather than "0B".
func TestRenderConfineListCeilingLine(t *testing.T) {
	for _, test := range []struct {
		name    string
		reserve runner.ConfineSliceReserve
		want    []string
		absent  []string
	}{
		{
			name:    "subsystem-off",
			reserve: runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 12 << 30, Jobs: 1},
			absent:  []string{"slice ceiling:"},
		},
		{
			name: "throttled",
			reserve: runner.ConfineSliceReserve{
				GrantedBytes: 1 << 30, CeilingBytes: 30 << 30, Jobs: 1,
				CeilingMode: "enforce", CeilingState: "throttled",
				CeilingStaticBytes: 64 << 30, MemAvailableBytes: 6 << 30,
			},
			want: []string{
				"slice ceiling: reduced below the 64G configured ceiling by memory used OUTSIDE the slice",
				"(system MemAvailable 6G)",
				"running jobs are untouched",
			},
		},
		{
			name: "throttled-draining",
			reserve: runner.ConfineSliceReserve{
				GrantedBytes: 40 << 30, CeilingBytes: 30 << 30, Jobs: 4,
				CeilingMode: "enforce", CeilingState: "throttled", CeilingStaticBytes: 64 << 30,
			},
			// granted > ceiling is a DRAIN state, not the ledger inconsistency
			// the residual line reports; it must say so in its own words.
			want:   []string{"granted exceeds the effective ceiling by 10G: draining"},
			absent: []string{"LEDGER INCONSISTENCY", "MemAvailable"},
		},
		{
			name: "unthrottled",
			reserve: runner.ConfineSliceReserve{
				CeilingBytes: 62 << 30, CeilingMode: "enforce", CeilingState: "unthrottled",
				CeilingStaticBytes: 64 << 30, MemAvailableBytes: 34 << 30,
			},
			want:   []string{"slice ceiling: at its 64G configured ceiling; not reduced by system memory pressure (system MemAvailable 34G)"},
			absent: []string{"draining"},
		},
		{
			// In observe mode CeilingBytes is the UNTOUCHED static capacity --
			// observe applies nothing -- so the counterfactual must be read from
			// CeilingWouldBeBytes. Rendering CeilingBytes here reported the static
			// figure as though it were the observed decision, leaving the
			// observe-then-enforce rollout blind on its own operator surface.
			name: "observe",
			reserve: runner.ConfineSliceReserve{
				CeilingBytes: 62 << 30, CeilingWouldBeBytes: 30 << 30,
				CeilingMode: "observe", CeilingState: "throttled",
				CeilingStaticBytes: 64 << 30, MemAvailableBytes: 6 << 30,
			},
			want:   []string{"30G would be effective under system memory pressure (observe mode, not applied)"},
			absent: []string{"62G would be effective"},
		},
		{
			// A HELD ceiling's numbers are up to a TTL old. Rendering them
			// unmarked states a current fact the daemon cannot establish.
			name: "held",
			reserve: runner.ConfineSliceReserve{
				CeilingBytes: 30 << 30, CeilingMode: "enforce", CeilingState: "throttled",
				CeilingHeld: true, CeilingReason: "memavailable:read-error",
				CeilingStaticBytes: 64 << 30, MemAvailableBytes: 6 << 30,
			},
			want: []string{"(system MemAvailable 6G, last established)", "holding: memavailable:read-error"},
		},
		{
			// A newer daemon's vocabulary must read as unknown, never be silently
			// rendered as one of the states this binary happens to know.
			name: "unrecognised-state",
			reserve: runner.ConfineSliceReserve{
				CeilingBytes: 62 << 30, CeilingMode: "enforce", CeilingState: "draining-forever",
				CeilingStaticBytes: 64 << 30,
			},
			want:   []string{`slice ceiling: unevaluated (unrecognised state "draining-forever")`},
			absent: []string{"configured ceiling"},
		},
		{
			name: "unevaluated",
			reserve: runner.ConfineSliceReserve{
				CeilingBytes: 62 << 30, CeilingMode: "enforce", CeilingState: "unevaluated",
				CeilingReason: "memavailable:read-error",
			},
			want:   []string{"slice ceiling: unevaluated (memavailable:read-error)"},
			absent: []string{"0B", "configured ceiling"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reserve := test.reserve
			result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}, SliceReserve: &reserve}
			var stdout, stderr bytes.Buffer
			if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout=%q, want %q", stdout.String(), want)
				}
			}
			// Scoped to the ceiling block, so an absence assertion cannot be
			// satisfied or defeated by the reserve lines above it.
			ceilingBlock := ""
			if index := strings.Index(stdout.String(), "slice ceiling:"); index >= 0 {
				ceilingBlock = stdout.String()[index:]
			}
			for _, absent := range test.absent {
				haystack := ceilingBlock
				if absent == "slice ceiling:" {
					haystack = stdout.String()
				}
				if strings.Contains(haystack, absent) {
					t.Fatalf("ceiling block=%q, must not contain %q", haystack, absent)
				}
			}
		})
	}
}
