//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// AIRA-101 §9.1: an exclusive request must FAIL CLOSED on every non-grant.
//
// The property under test is the feature's whole reason for existing. Past the
// daemon exchange lies admitWithFlock, which launches OUTSIDE the daemon ledger
// and therefore outside any notion of exclusivity — so reaching it with an
// exclusive request would launch a benchmark that believes it is alone and is
// not, producing contaminated numbers that look clean.
//
// Each test asserts BOTH halves: that the launch is refused, and that the flock
// fallback was never even attempted. Asserting only the error would pass against
// an implementation that took the lock, launched, and reported an error
// afterwards.

// exclusiveRunner builds a runner whose flock path is booby-trapped: reaching it
// fails the test outright rather than being detected after the fact.
func exclusiveRunner(t *testing.T) *Runner {
	t.Helper()
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 1 << 40, true, "" })
	r.lockAttemptFn = func(string) (*admitLock, error) {
		t.Error("an --exclusive request must never reach the flock fallback: it launches outside the ledger and outside exclusivity")
		return &admitLock{}, nil
	}
	return r
}

func requireExclusiveRefusal(t *testing.T, result admissionResult, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got result=%+v", what, result)
	}
	if !strings.Contains(err.Error(), ErrExclusiveUnavailable) {
		t.Fatalf("%s: expected an %s refusal, got %v", what, ErrExclusiveUnavailable, err)
	}
	if result.release != nil {
		t.Fatalf("%s: a refused exclusive request must not hold a lease", what)
	}
}

// The commonest way to reach the fallback: no daemon at all.
func TestExclusiveRefusesWhenTheDaemonIsUnreachable(t *testing.T) {
	r := exclusiveRunner(t)
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return nil, net.ErrClosed }
	result, err := r.admit(context.Background(), Request{Exclusive: true})
	requireExclusiveRefusal(t, result, err, "daemon unreachable")
}

// An OLDER daemon rejects the unknown `exclusive` field with E_DAEMON_PROTOCOL.
// Without this case the runner would treat it as an unrecognised code, fall
// through to the flock fallback, and launch a silently non-exclusive benchmark
// — which is exactly why not bumping the protocol version is only safe WITH the
// fail-closed rule.
func TestExclusiveRefusesAgainstAnOlderDaemon(t *testing.T) {
	r := exclusiveRunner(t)
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
			OK: false, Code: "E_DAEMON_PROTOCOL", Error: "E_DAEMON_PROTOCOL: unexpected admit field \"exclusive\"",
		})
	}()
	result, err := r.admit(context.Background(), Request{Exclusive: true})
	requireExclusiveRefusal(t, result, err, "older daemon")
	// The operator has to be able to tell version skew from a busy slice.
	if !strings.Contains(err.Error(), "E_DAEMON_PROTOCOL") {
		t.Fatalf("the refusal must carry the daemon's own code, got %v", err)
	}
}

// Another benchmark already holds the slice. Terminal, and the message must say
// so — "retry when it completes" is a different action from "reinstall".
func TestExclusiveRefusesWhenAnotherExclusiveIsActive(t *testing.T) {
	r := exclusiveRunner(t)
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
			OK: false, Code: "E_ADMIT_EXCLUSIVE_ACTIVE",
			Error: "E_ADMIT_EXCLUSIVE_ACTIVE: another exclusive request is already active on this slice (held); retry when it completes",
		})
	}()
	result, err := r.admit(context.Background(), Request{Exclusive: true})
	requireExclusiveRefusal(t, result, err, "another exclusive active")
	if !strings.Contains(err.Error(), "E_ADMIT_EXCLUSIVE_ACTIVE") {
		t.Fatalf("the refusal must carry the daemon's own code, got %v", err)
	}
}

// The daemon could not establish an empty slice. Distinct from "busy", and the
// distinction must survive to the operator.
func TestExclusiveRefusesWhenEmptinessCouldNotBeEstablished(t *testing.T) {
	r := exclusiveRunner(t)
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
			OK: false, Code: "U_ADMIT_EXCLUSIVE_UNESTABLISHED",
			Error: "U_ADMIT_EXCLUSIVE_UNESTABLISHED: the confine scan is failing",
		})
	}()
	result, err := r.admit(context.Background(), Request{Exclusive: true})
	requireExclusiveRefusal(t, result, err, "emptiness unestablished")
	if !strings.Contains(err.Error(), "U_ADMIT_EXCLUSIVE_UNESTABLISHED") {
		t.Fatalf("the refusal must carry the daemon's own code, got %v", err)
	}
}

// `unevaluated` is a REAL grant state that an ordinary job proceeds on. An
// exclusive job must not: "the daemon could not evaluate the slice" is precisely
// the case where a claim of exclusivity would be fabricated.
func TestExclusiveRefusesAnUnevaluatedGrant(t *testing.T) {
	r := exclusiveRunner(t)
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		data, _ := json.Marshal(runnerAdmitGrant{State: "unevaluated", Reserve: 40, Basis: "fallback:slice-unreadable"})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	result, err := r.admit(context.Background(), Request{Exclusive: true})
	requireExclusiveRefusal(t, result, err, "unevaluated grant")
}

// Admission disabled or bypassed cannot establish exclusivity either.
func TestExclusiveRefusesWhenAdmissionIsBypassedOrDisabled(t *testing.T) {
	r := exclusiveRunner(t)
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return nil, net.ErrClosed }
	result, err := r.admit(context.Background(), Request{Exclusive: true, NoAdmit: true})
	requireExclusiveRefusal(t, result, err, "admission bypassed")

	disabled := exclusiveRunner(t)
	disabled.memorySlice, disabled.memoryReserve = "", 0
	result, err = disabled.admit(context.Background(), Request{Exclusive: true})
	requireExclusiveRefusal(t, result, err, "admission disabled")
}

// The positive control. Without it every test above would also pass against an
// implementation that refused EVERY exclusive request, which would be a useless
// feature that happens to be safe.
func TestExclusiveIsGrantedOnAGenuineDaemonGrant(t *testing.T) {
	r := exclusiveRunner(t)
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	seen := make(chan map[string]any, 1)
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		seen <- request.Request.Args
		data, _ := json.Marshal(runnerAdmitGrant{State: "waited", Reserve: 40, Basis: "pinned:client", WaitedMS: 1234})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	result, err := r.admit(context.Background(), Request{Exclusive: true})
	if err != nil {
		t.Fatalf("a genuine grant must succeed: %v", err)
	}
	if args := <-seen; args["exclusive"] != true {
		t.Fatalf("the exclusive flag must reach the daemon, args=%v", args)
	}
	if result.waitedMS != 1234 {
		t.Fatalf("the drain wait must survive to the caller, got %d", result.waitedMS)
	}
	result.releaseAdmission()
}

// The nesting token and the sub-reservation marker must reach the daemon, or the
// holder's own nested work is blocked by its own hold and a drain never
// converges.
func TestNestedTokensReachTheDaemon(t *testing.T) {
	r := exclusiveRunner(t)
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	seen := make(chan map[string]any, 1)
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		seen <- request.Request.Args
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	result, err := r.admit(context.Background(), Request{
		ExclusiveHolder: "CONFINE-bench-100-1@mark",
		ParentScopeID:   "CONFINE-suite-200-1@mark",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := <-seen
	if args["exclusive_holder"] != "CONFINE-bench-100-1@mark" {
		t.Fatalf("holder token missing from the admit frame: %v", args)
	}
	if args["parent_scope_id"] != "CONFINE-suite-200-1@mark" {
		t.Fatalf("parent scope id missing from the admit frame: %v", args)
	}
	result.releaseAdmission()
}

// THE FIX SITE for the deadline leak, tested where the fix actually lives.
//
// admitThroughDaemon sets a transport deadline of now+maxWait+grace to bound the
// admission EXCHANGE, then hands that same connection back as the LEASE — which
// is held for the whole life of the job and routinely outlives any admission
// budget. Leaving the deadline set was invisible until AIRA-101 added the first
// client-side reader of the lease; then every exclusive benchmark running longer
// than its own admission budget (30 minutes by default) reported itself
// contended when nothing had actually happened.
//
// This asserts the returned lease has NO deadline, by proving a read on it is
// still blocked well past when the transport deadline would have fired. It is
// deliberately at THIS level rather than in the confine e2e test: a test that
// cleared the deadline itself would pass against a reverted fix.
func TestAGrantedLeaseCarriesNoTransportDeadline(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 1 << 40, true, "" })
	// A very short admission budget, so an un-cleared deadline fires almost at once.
	r.admissionMaxWait = 50 * time.Millisecond
	client, server := net.Pipe()
	r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &request); err != nil {
			return
		}
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate", Reserve: 40, Basis: "pinned:client"})
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		// Hold the lease open, exactly as a live daemon does for the job's lifetime.
		var one [1]byte
		_, _ = server.Read(one[:])
	}()
	result, err := r.admit(context.Background(), Request{})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	defer result.releaseAdmission()
	lease, ok := result.release.(net.Conn)
	if !ok {
		t.Fatalf("expected the lease to be the connection, got %T", result.release)
	}
	// Read on the lease well past when the transport deadline (maxWait + ~1s
	// grace) would have expired. A healthy, still-held lease must simply block.
	readErr := make(chan error, 1)
	go func() {
		var one [1]byte
		_, readErrValue := lease.Read(one[:])
		readErr <- readErrValue
	}()
	select {
	case err := <-readErr:
		t.Fatalf("the lease read returned %v: a transport deadline survived onto the lease, so every job outliving its admission budget would be reported as having lost its grant", err)
	case <-time.After(1500 * time.Millisecond):
		// Still blocked, which is the correct behaviour for a live lease.
	}
}

// A stray or malformed inherited token must be discarded, not forwarded: the
// daemon refuses a non-canonical scope id, so forwarding one would turn an
// unrelated environment variable into a launch failure.
func TestInheritedTokensAreValidatedBeforeUse(t *testing.T) {
	t.Setenv(ExclusiveHolderEnv, "not-a-scope-id")
	if holder := InheritedExclusiveHolder(); holder != "" {
		t.Fatalf("a malformed holder token must be discarded, got %q", holder)
	}
	t.Setenv(ExclusiveHolderEnv, "CONFINE-bench-100-1@mark")
	if holder := InheritedExclusiveHolder(); holder != "CONFINE-bench-100-1@mark" {
		t.Fatalf("a canonical holder token must be forwarded, got %q", holder)
	}
	t.Setenv("AIRA_CONFINE_SCOPE_ID", "garbage")
	if parent := InheritedConfineScopeID(); parent != "" {
		t.Fatalf("a malformed parent scope id must be discarded, got %q", parent)
	}
}
