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

// AIRA-149 facet 2b, client side. The terminal sentence used to assert
// "slice contended, no memory admission within the wait" for EVERY non-exclusive
// saturated rejection, print the client's own unresolved request under the word
// "reserve", and print "unknown" for the ceiling.
//
// Every case below drives the real admitThroughDaemon over net.Pipe, so what is
// asserted is the message an operator actually receives.
//
// verifies: AIRA-149 §3.6, I4, R6

// saturatedMessage runs one admission against a daemon that answers with the
// given rejection, and returns the error text the caller is handed.
func saturatedMessage(t *testing.T, clientReserve int64, rejection runnerAdmitRejection) string {
	t.Helper()
	runner := &Runner{
		memorySlice:      "/fake/finite.slice",
		memoryReserve:    clientReserve,
		admissionMaxWait: time.Second,
		pollInterval:     time.Millisecond,
		clock:            newInstantClock(),
		sliceMemory:      func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" },
	}
	client, server := net.Pipe()
	runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var frame runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &frame); err != nil {
			return
		}
		data, _ := json.Marshal(rejection)
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
			Code: "E_ADMIT_SATURATED", Error: "", Data: data,
		})
	}()
	result, _, err := runner.admitThroughDaemon(context.Background(), Request{DaemonEstimateMemory: true}, clientReserve)
	if err == nil {
		t.Fatalf("a saturated rejection produced no error (result=%+v)", result)
	}
	if result.basis != "reject:saturated" {
		t.Fatalf("basis=%q, want %q", result.basis, "reject:saturated")
	}
	return err.Error()
}

// The §0 measured shape, so the numbers below are the ticket's own.
const (
	saturatedCeiling   = int64(1031798784) // 984M as FormatConfineBytes renders it
	saturatedGrantable = int64(1031794688) // ceiling - 4096 -> 1007612K
)

func TestSaturatedMessagePrintsTheDaemonsResolvedReserveNotTheClientsRequest(t *testing.T) {
	message := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
		Contention: "observed",
	})
	if !strings.Contains(message, FormatConfineBytes(saturatedCeiling)) {
		t.Fatalf("message %q omits the daemon-resolved reserve %s", message, FormatConfineBytes(saturatedCeiling))
	}
	if strings.Contains(message, FormatConfineBytes(DefaultConfineMemoryReserve)) {
		t.Fatalf("message %q prints the CLIENT's own unresolved request (%s) under the word reserve",
			message, FormatConfineBytes(DefaultConfineMemoryReserve))
	}
	if strings.Contains(message, "unknown") {
		t.Fatalf("message %q still reports an unknown ceiling although the daemon sent one", message)
	}
}

func TestSaturatedMessageNamesTheUnfittableReserveInsteadOfContention(t *testing.T) {
	message := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
		Contention: "none-observed", Grantable: int64Ptr(saturatedGrantable),
	})
	if strings.Contains(message, "slice contended") {
		t.Fatalf("message %q still manufactures a contended slice", message)
	}
	// The fact that WAS established, with the word `ahead` intact: a waiter queued
	// BEHIND the head was never in its way, and claiming the broader fact would be
	// an over-claim in a message whose whole purpose is not to over-claim.
	if !strings.Contains(message, "queued ahead of this request") {
		t.Fatalf("message %q does not state the fact that was actually established", message)
	}
	if strings.Contains(message, "the slice was empty") {
		t.Fatalf("message %q claims an empty slice; the slice's own residual charge is why the request failed", message)
	}
	for _, want := range []string{
		FormatConfineBytes(saturatedCeiling),
		FormatConfineBytes(saturatedGrantable),
		"--memory-reserve",
		"--memory-max",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q omits %q", message, want)
		}
	}

	// The sibling case: FormatConfineBytes(0) is this codebase's word for NOT
	// ESTABLISHED, so a measured zero must not be rendered through it.
	zero := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
		Contention: "none-observed", Grantable: int64Ptr(0),
	})
	if !strings.Contains(zero, "largest grantable reserve 0B") {
		t.Fatalf("message %q does not render a MEASURED zero grantable as 0B", zero)
	}
	if strings.Contains(zero, "largest grantable reserve unknown") {
		t.Fatalf("message %q turned a measured zero into %q, the conflation this change removes", zero, "unknown")
	}

	// And an ABSENT grantable omits the parenthetical entirely rather than
	// inventing a figure for it.
	absent := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
		Contention: "none-observed",
	})
	if strings.Contains(absent, "largest grantable reserve") {
		t.Fatalf("message %q reports a grantable figure the daemon never sent", absent)
	}
}

func TestSaturatedMessageKeepsTheContendedWordingWhenContentionWasObserved(t *testing.T) {
	message := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
		Contention: "observed", Grantable: int64Ptr(saturatedGrantable),
	})
	if !strings.Contains(message, "slice contended, no memory admission within the wait") {
		t.Fatalf("message %q lost the established contended wording", message)
	}
}

func TestSaturatedMessageReportsAnUnestablishedContentionAsSuch(t *testing.T) {
	message := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
		Contention: "unevaluated",
	})
	if !strings.Contains(message, "could not establish this request's contention") {
		t.Fatalf("message %q does not say the gate could not establish the contention", message)
	}
	if strings.Contains(message, "slice contended") {
		t.Fatalf("message %q asserts contention the daemon never established", message)
	}
	if strings.Contains(message, "queued ahead of this request") {
		t.Fatalf("message %q claims solitude on a reading nobody has", message)
	}
}

func TestSaturatedMessageFallsBackToTheGenericWordingWhenContentionIsUnreported(t *testing.T) {
	// An empty contention field is "not reported by this build", never
	// "none-observed": an older daemon must degrade to today's message rather
	// than to a wrong one.
	message := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated",
	})
	if !strings.Contains(message, "slice contended, no memory admission within the wait") {
		t.Fatalf("message %q changed the unreported-contention wording", message)
	}
	if strings.Contains(message, "queued ahead of this request") {
		t.Fatalf("message %q read an ABSENT contention field as an established solitude", message)
	}
}

func TestSaturatedExclusiveWordingStillWinsOverTheContentionClause(t *testing.T) {
	for _, test := range []struct {
		exclusive string
		want      string
	}{
		{"held", "held exclusively by another job for benchmarking"},
		{"draining", "draining for an exclusive job"},
	} {
		t.Run(test.exclusive, func(t *testing.T) {
			message := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
				Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
				Exclusive: test.exclusive, Contention: "none-observed", Grantable: int64Ptr(saturatedGrantable),
			})
			if !strings.Contains(message, test.want) {
				t.Fatalf("message %q lost the AIRA-101 exclusivity wording", message)
			}
			if strings.Contains(message, "queued ahead of this request") {
				t.Fatalf("message %q sends a caller blocked by a benchmark to look at RAM", message)
			}
		})
	}
}

// AIRA-151. The population this ticket routes onto E_ADMIT_TOO_LARGE, seen from
// the client side.
//
// tooLargeAdmission runs one admission against a daemon that answers with the
// given E_ADMIT_TOO_LARGE frame, and returns everything the caller is handed:
// the result, whether the exchange was HANDLED (a false there means
// admitThroughDaemon fell through to fail() and then to the flock fallback,
// launching the job outside the ledger), and the error text.
func tooLargeAdmission(t *testing.T, clientReserve int64, message string, rejection runnerAdmitRejection) (admissionResult, bool, error) {
	t.Helper()
	runner := &Runner{
		memorySlice:      "/fake/finite.slice",
		memoryReserve:    clientReserve,
		admissionMaxWait: time.Second,
		pollInterval:     time.Millisecond,
		clock:            newInstantClock(),
		sliceMemory:      func(string) (int64, int64, bool, string) { return 0, 1 << 30, true, "" },
	}
	client, server := net.Pipe()
	runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var frame runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &frame); err != nil {
			return
		}
		data, _ := json.Marshal(rejection)
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
			Code: "E_ADMIT_TOO_LARGE", Error: message, Data: data,
		})
	}()
	return runner.admitThroughDaemon(context.Background(), Request{DaemonEstimateMemory: true}, clientReserve)
}

// The §0.1 measured shape and the message internal/daemon/admit.go:2800 builds
// for it, byte for byte:
//
//	fmt.Sprintf("%s: required=%d cap_minus_headroom=%d basis=%s", …)
const (
	tooLargeRequired = int64(4294967296) // the unpinned client default, UNCLAMPED
	tooLargeCeiling  = int64(1031798784) // 1 GiB slice - 32 MiB - 8 MiB headroom
	tooLargeBasis    = "fallback:insufficient-samples:n=1,oom-on-record"
	tooLargeMessage  = "E_ADMIT_TOO_LARGE: required=4294967296 cap_minus_headroom=1031798784 basis=fallback:insufficient-samples:n=1,oom-on-record"
)

// TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis records the exact
// operator-facing string this ticket now routes traffic onto.
//
// It is GREEN by construction and is NOT a red-first demonstration: the client
// already passes an E_ADMIT_TOO_LARGE message through unchanged. It exists so
// that deferral G3 — "the too-large message names no escape hatch and prints
// raw bytes, unlike the AIRA-149 saturated sentence beside it" — rests on a
// recorded string rather than on a claim, and so that a later wording change is
// a deliberate edit to a test rather than unnoticed drift.
//
// verifies: AIRA-151 §3.5, G3
func TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis(t *testing.T) {
	result, _, err := tooLargeAdmission(t, DefaultConfineMemoryReserve, tooLargeMessage, runnerAdmitRejection{
		Required: tooLargeRequired, Ceiling: tooLargeCeiling, Basis: tooLargeBasis,
	})
	if err == nil {
		t.Fatalf("a too-large rejection produced no error (result=%+v)", result)
	}
	message := err.Error()
	for _, want := range []string{
		"required=4294967296",
		"cap_minus_headroom=1031798784",
		"basis=" + tooLargeBasis,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q omits %q; both numbers and the basis are what make this refusal actionable at all", message, want)
		}
	}
	if result.basis != "reject:too-large" {
		t.Fatalf("basis=%q, want %q — the run's recorded admission basis", result.basis, "reject:too-large")
	}
	if result.state != "too_large" {
		t.Fatalf("state=%q, want %q — the terminal state the agent guide tells agents not to retry", result.state, "too_large")
	}
	if result.reserve != tooLargeRequired {
		t.Fatalf("reserve=%d, want the DAEMON-resolved %d", result.reserve, tooLargeRequired)
	}
	if result.ceiling != tooLargeCeiling {
		t.Fatalf("ceiling=%d, want %d", result.ceiling, tooLargeCeiling)
	}
	// G3's evidence, recorded rather than asserted as acceptable: unlike the
	// AIRA-149 saturated sentence this message names no escape hatch and renders
	// raw byte counts. The agent guide carries the action instead (AIRA-151 §3.6).
	if strings.Contains(message, "--memory-reserve") {
		t.Fatalf("message %q now names an escape hatch; G3 was filed against a message that did not, and its successor must start from a current string", message)
	}
}

// TestTooLargeRejectionForAnUnescalatedOverCeilingReserveIsAcceptedByTheClient
// pins I5.
//
// The rejection AIRA-151 newly routes this population onto must satisfy
// validRunnerAdmitRejection, or admitThroughDaemon drops through fail() into
// the flock fallback and launches the job OUTSIDE the ledger — the loudest
// failure available in this subsystem, and the reason this is a test rather
// than an argument.
//
// GREEN by construction: Required > 0, Ceiling >= 0 and a non-empty Basis are
// all satisfied at admit.go:1904 today. Its RED direction is a future payload
// or validation change.
//
// verifies: AIRA-151 I5, §3.4
func TestTooLargeRejectionForAnUnescalatedOverCeilingReserveIsAcceptedByTheClient(t *testing.T) {
	payload := runnerAdmitRejection{
		Required: tooLargeRequired, Ceiling: tooLargeCeiling, Basis: tooLargeBasis,
	}
	if !validRunnerAdmitRejection("E_ADMIT_TOO_LARGE", payload) {
		t.Fatalf("the daemon payload %+v is rejected by validRunnerAdmitRejection; this population would launch outside the ledger", payload)
	}
	result, handled, err := tooLargeAdmission(t, DefaultConfineMemoryReserve, tooLargeMessage, payload)
	if !handled {
		t.Fatalf("the exchange was not handled (result=%+v, err=%v); admitThroughDaemon fell through to fail() and the flock fallback", result, err)
	}
	if err == nil {
		t.Fatal("a too-large rejection must be terminal, and a terminal refusal carries an error")
	}
	if result.basis != "reject:too-large" {
		t.Fatalf("basis=%q, want %q", result.basis, "reject:too-large")
	}
}
