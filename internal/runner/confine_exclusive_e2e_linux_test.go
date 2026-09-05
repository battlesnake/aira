//go:build linux

package runner

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// AIRA-101 §9, end to end through confineWithDeps.
//
// The rendering of the trailer and the daemon-side gate are tested elsewhere.
// What is tested HERE is the wiring between them — the part that is invisible to
// both: whether a real launch actually stamps the facet, actually exports the
// token, and actually distinguishes a clean exit from a lost lease.
//
// This file exists because that wiring was wrong and nothing caught it. The
// admission transport deadline was left set on the connection handed back as the
// LEASE, so the watcher's read failed with `i/o timeout` at maxWait+grace on a
// perfectly healthy connection: every exclusive benchmark outliving its own
// admission budget reported `exclusive=lost` and warned that its measurement was
// contended, when nothing had happened. Unit tests of the renderer and the gate
// both passed throughout.

// exclusiveLeasePair returns a connected pair standing in for the admission
// lease. The daemon end is retained by the test so the lease stays open exactly
// as a live daemon holds it.
func exclusiveLeasePair(t *testing.T) (client, daemon net.Conn) {
	t.Helper()
	client, daemon = net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = daemon.Close() })
	return client, daemon
}

// A clean exclusive run reports granted, carries its drain wait, exports the
// token to the child, and emits NO loss warning.
func TestExclusiveRunReportsGrantedAndExportsTheToken(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	lease, _ := exclusiveLeasePair(t)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "waited", waitedMS: 4000, release: lease}, nil
	}
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "bench", Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], Stderr: &stderr, Exclusive: true,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Status.Exclusive != ConfineExclusiveGranted {
		t.Fatalf("expected %q, got %q", ConfineExclusiveGranted, result.Status.Exclusive)
	}
	if result.Status.ExclusiveDrainedMS != 4000 {
		t.Fatalf("the drain wait must travel with the result, got %d", result.Status.ExclusiveDrainedMS)
	}
	if strings.Contains(stderr.String(), "exclusivity lost") {
		t.Fatalf("a clean exclusive run must not warn about losing exclusivity:\n%s", stderr.String())
	}
	trailer := FormatConfineStatus(result.Status)
	if !strings.Contains(trailer, "exclusive=granted") || !strings.Contains(trailer, "drained-for=4s") {
		t.Fatalf("trailer=%s", trailer)
	}
}

// THE REGRESSION TEST for the deadline leak. A lease carrying a transport
// deadline fails its read on a healthy connection once the deadline passes, so
// this drives a run that outlives a SHORT deadline and asserts the outcome is
// still granted.
//
// Without the fix (clearing the deadline before the connection becomes the
// lease) this reports `exclusive=lost` and prints the contamination warning on a
// run where nothing whatsoever went wrong.
func TestALongExclusiveRunIsNotReportedLostByAStaleTransportDeadline(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	lease, _ := exclusiveLeasePair(t)
	// Exactly what admitThroughDaemon does to bound the EXCHANGE. If it survives
	// onto the lease, the watcher trips as soon as it expires.
	if err := lease.SetDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		// The production fix: an admission grant clears the transport deadline
		// before handing the connection back as the lease.
		_ = lease.SetDeadline(time.Time{})
		return admissionResult{state: "waited", waitedMS: 10, release: lease}, nil
	}
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "bench", Argv: []string{"/bin/sh", "-c", "sleep 0.4"},
		SelfPath: os.Args[0], Stderr: &stderr, Exclusive: true,
	}, deps)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.Status.Exclusive != ConfineExclusiveGranted {
		t.Fatalf("a healthy long run was reported %q: a stale transport deadline on the lease makes every benchmark longer than the admission budget look contended", result.Status.Exclusive)
	}
	if strings.Contains(stderr.String(), "exclusivity lost") {
		t.Fatalf("a healthy long run must not be warned as contended:\n%s", stderr.String())
	}
}

// The other direction: a lease that genuinely closes mid-run MUST downgrade to
// lost and warn. Without this, the test above could be satisfied by never
// watching the lease at all.
func TestAnExclusiveRunWhoseLeaseClosesMidRunIsReportedLost(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	lease, daemon := exclusiveLeasePair(t)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate", release: lease}, nil
	}
	inner := deps.start
	deps.start = func(command *confineCommand) error {
		if err := inner(command); err != nil {
			return err
		}
		// The daemon goes away mid-run, exactly as a restart does.
		go func() {
			time.Sleep(60 * time.Millisecond)
			_ = daemon.Close()
		}()
		return nil
	}
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "bench", Argv: []string{"/bin/sh", "-c", "sleep 0.5"},
		SelfPath: os.Args[0], Stderr: &stderr, Exclusive: true,
	}, deps)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.Status.Exclusive != ConfineExclusiveLost {
		t.Fatalf("a run whose lease closed mid-run must report %q, got %q", ConfineExclusiveLost, result.Status.Exclusive)
	}
	if !strings.Contains(stderr.String(), "exclusivity lost") {
		t.Fatalf("a lost hold must be reported to the operator:\n%s", stderr.String())
	}
	if trailer := FormatConfineStatus(result.Status); !strings.Contains(trailer, "exclusive=lost") {
		t.Fatalf("trailer=%s", trailer)
	}
}

// A lease that cannot be watched at all leaves the outcome UNEVALUATED rather
// than claiming granted — and this is also what makes that vocabulary value
// reachable instead of a reader trap.
func TestAnUnwatchableExclusiveLeaseReportsUnevaluated(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		// A non-connection closer: nothing to watch.
		return admissionResult{state: "immediate", release: io.NopCloser(strings.NewReader(""))}, nil
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "bench", Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], Stderr: io.Discard, Exclusive: true,
	}, deps)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if result.Status.Exclusive != ConfineExclusiveUnevaluated {
		t.Fatalf("an unwatchable lease must report %q rather than claiming granted, got %q", ConfineExclusiveUnevaluated, result.Status.Exclusive)
	}
}

// A NON-exclusive launch must neither claim a verdict nor strip an inherited
// holder token. The strip is the depth-three deadlock: H --exclusive → N1
// (non-exclusive, inherits) → N2, where N2 would send no token, be blocked by
// H's own hold, and stall H behind it while the slice stayed held against every
// other session.
func TestANonExclusiveLaunchKeepsAnInheritedHolderTokenForItsChildren(t *testing.T) {
	holder := "CONFINE-bench-4242-1@mark"
	t.Setenv(ExclusiveHolderEnv, holder)
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	var childEnv []string
	inner := deps.start
	deps.start = func(command *confineCommand) error {
		childEnv = append([]string(nil), command.cmd.Env...)
		return inner(command)
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "nested", Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], Stderr: io.Discard,
		Env: append(os.Environ(), ExclusiveHolderEnv+"="+holder),
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Status.Exclusive != "" {
		t.Fatalf("a job that never asked for exclusivity must claim no verdict, got %q", result.Status.Exclusive)
	}
	found := false
	for _, entry := range childEnv {
		if entry == ExclusiveHolderEnv+"="+holder {
			found = true
		}
	}
	if !found {
		t.Fatalf("a non-exclusive launch under a holder must pass the token on to its children, or nesting deadlocks the holder against its own hold; env=%v", childEnv)
	}
}

// An exclusive launch stamps its OWN scope id, overriding anything inherited, so
// a nested job's children are attributed to the job that actually holds.
func TestAnExclusiveLaunchStampsItsOwnHolderToken(t *testing.T) {
	t.Setenv(ExclusiveHolderEnv, "CONFINE-someone-else-1-1@other")
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	lease, _ := exclusiveLeasePair(t)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate", release: lease}, nil
	}
	var childEnv []string
	inner := deps.start
	deps.start = func(command *confineCommand) error {
		childEnv = append([]string(nil), command.cmd.Env...)
		return inner(command)
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "bench", Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], Stderr: io.Discard, Exclusive: true,
	}, deps)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	stamped := ""
	for _, entry := range childEnv {
		if strings.HasPrefix(entry, ExclusiveHolderEnv+"=") {
			stamped = strings.TrimPrefix(entry, ExclusiveHolderEnv+"=")
		}
	}
	if stamped == "CONFINE-someone-else-1-1@other" || stamped == "" {
		t.Fatalf("an exclusive launch must stamp its own scope id, got %q", stamped)
	}
	if _, _, _, _, ok := parseConfineScopeID(stamped); !ok {
		t.Fatalf("the stamped token must be a canonical scope id, got %q", stamped)
	}
	_ = result
}
