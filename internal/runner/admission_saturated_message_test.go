//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"fmt"
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

	// S4 (P2-2): the signed ledger can drive grantable NEGATIVE during the restart
	// re-declare window. That deficit is rendered honestly as "over-subscribed by
	// N", NOT flattened to "0B" (which would read as "nothing grantable now" and
	// hide that a release must first recover the ledger).
	deficit := int64(-(16 << 30))
	negative := saturatedMessage(t, DefaultConfineMemoryReserve, runnerAdmitRejection{
		Basis: "reject:saturated", Required: saturatedCeiling, Ceiling: saturatedCeiling,
		Contention: "none-observed", Grantable: &deficit,
	})
	if !strings.Contains(negative, "slice over-subscribed by "+FormatConfineBytes(-deficit)) {
		t.Fatalf("message %q does not render a negative grantable as the over-subscription deficit", negative)
	}
	if strings.Contains(negative, "largest grantable reserve 0B") {
		t.Fatalf("message %q flattened a negative deficit to 0B, hiding the over-subscription", negative)
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
	// The CONFINE shape: DaemonEstimateMemory is set by confine's launch path and
	// by nothing else, so this is the only shape that leaves `pinned` on the wire
	// under the operator's control. tooLargeAdmissionForRequest exists because
	// that made every pinned-arm test here a confine test.
	result, handled, err, _ := tooLargeAdmissionForRequest(
		t, Request{DaemonEstimateMemory: true}, clientReserve, message, rejection)
	return result, handled, err
}

// tooLargeAdmissionForRequest is tooLargeAdmission over an arbitrary Request,
// and additionally returns the `pinned` flag the runner actually PUT ON THE
// WIRE for it -- the one fact the daemon's advice is allowed to rest on.
func tooLargeAdmissionForRequest(t *testing.T, req Request, clientReserve int64, message string, rejection runnerAdmitRejection) (admissionResult, bool, error, bool) {
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
	wire := make(chan bool, 1)
	go func() {
		defer server.Close()
		defer close(wire)
		var frame runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &frame); err != nil {
			return
		}
		pinned, _ := frame.Request.Args["pinned"].(bool)
		wire <- pinned
		data, _ := json.Marshal(rejection)
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
			Code: "E_ADMIT_TOO_LARGE", Error: message, Data: data,
		})
	}()
	result, handled, err := runner.admitThroughDaemon(context.Background(), req, clientReserve)
	return result, handled, err, <-wire
}

// The §0.1 measured shape and the message internal/daemon/admit.go builds for
// it, byte for byte:
//
//	fmt.Sprintf("%s: required=%d cap_minus_headroom=%d basis=%s", …)
//
// followed, since AIRA-165, by " -- " and the advice arm that basis selects.
const (
	tooLargeRequired = int64(4294967296) // the unpinned client default, UNCLAMPED
	tooLargeCeiling  = int64(1031798784) // 1 GiB slice - 32 MiB - 8 MiB headroom
	tooLargeBasis    = "fallback:insufficient-samples:n=1,oom-on-record"
	tooLargeMessage  = "E_ADMIT_TOO_LARGE: required=4294967296 cap_minus_headroom=1031798784 basis=fallback:insufficient-samples:n=1,oom-on-record -- this number is AIRA's blind default for a command it has not measured, not a measurement of this one, and this slice cannot grant even that; pin a --memory-reserve you know this command fits in, or run where the slice is larger"

	// The two other populations AIRA-153 leaves able to reach this refusal, in
	// the daemon's own spelling. The daemon owns the split and pins it
	// end-to-end (internal/daemon/admit_too_large_advice_test.go); what these
	// establish is the half only the CLIENT can establish -- that each arm
	// reaches the operator intact rather than being rewritten, truncated, or
	// swapped for the runner's own sentence the way E_ADMIT_SATURATED's is.
	tooLargePinnedBasis   = "pinned:client"
	tooLargePinnedMessage = "E_ADMIT_TOO_LARGE: required=4294967296 cap_minus_headroom=1031798784 basis=pinned:client -- this reserve was PINNED on the client side, so AIRA neither sized it nor fitted it to this slice, and it is larger than this slice can grant; which pin is not established here -- it may be a --memory-reserve, --memory-max or --delegate-ram passed to confine, a `docker run --memory` limit AIRA charged for an otherwise unpinned job, or an `aira run` reserve (run.memory_reserve, or AIRA's own estimate, both of which aira run sends pinned); give it a reserve of at most cap_minus_headroom -- lower the one you passed, or set one -- or run where the slice is larger"

	tooLargeEstimateRequired = int64(2469606195)
	tooLargeEstimateBasis    = "estimate:max=2147483648,n=5,f=115"
	tooLargeEstimateMessage  = "E_ADMIT_TOO_LARGE: required=2469606195 cap_minus_headroom=1031798784 basis=estimate:max=2147483648,n=5,f=115 -- this is AIRA's estimate from this command's OWN measured peak history, and it exceeds what this slice can grant; pin a smaller --memory-reserve only if you know the real need is smaller, otherwise this command cannot run on this slice"

	tooLargeOOMRequired = int64(1345824499)
	tooLargeOOMBasis    = "estimate:oom-escalated"
	tooLargeOOMMessage  = "E_ADMIT_TOO_LARGE: required=1345824499 cap_minus_headroom=1031798784 basis=estimate:oom-escalated -- this command was OOM-killed here, and the reserve its own recorded peak justifies is larger than this slice can grant; it does not fit on this slice -- run where the slice is larger rather than retrying it unchanged"
)

// TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis records the exact
// operator-facing strings this refusal routes traffic onto, now that AIRA-165
// has case-split the advice half.
//
// It is GREEN by construction and is NOT a red-first demonstration: the client
// already passes an E_ADMIT_TOO_LARGE message through unchanged. Its subject is
// what only this side can establish — that whichever arm the daemon selected
// arrives at the operator with both numbers, the basis, AND that arm's advice
// intact. AIRA-151 G3 ("the message names no escape hatch") is what this test
// used to record as an accepted gap; AIRA-165 closed it, so the assertion is
// now the other way round: an escape hatch appropriate to THIS population must
// be present, and the one instruction an operator who already pinned cannot act
// on must be absent from the pinned arm.
//
// verifies: AIRA-151 §3.5, AIRA-165
func TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis(t *testing.T) {
	for _, test := range []struct {
		name       string
		message    string
		required   int64
		basis      string
		wantAdvice string
	}{
		{
			name: "an unpinned prior the slice cannot fit", message: tooLargeMessage,
			required: tooLargeRequired, basis: tooLargeBasis,
			wantAdvice: "blind default for a command it has not measured",
		},
		{
			name: "a reserve pinned on the client side", message: tooLargePinnedMessage,
			required: tooLargeRequired, basis: tooLargePinnedBasis,
			wantAdvice: "was PINNED on the client side",
		},
		{
			name: "this command's own measured estimate", message: tooLargeEstimateMessage,
			required: tooLargeEstimateRequired, basis: tooLargeEstimateBasis,
			wantAdvice: "this command's OWN measured peak history",
		},
		{
			name: "an OOM escalation the slice cannot grant", message: tooLargeOOMMessage,
			required: tooLargeOOMRequired, basis: tooLargeOOMBasis,
			wantAdvice: "this command was OOM-killed here",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, _, err := tooLargeAdmission(t, DefaultConfineMemoryReserve, test.message, runnerAdmitRejection{
				Required: test.required, Ceiling: tooLargeCeiling, Basis: test.basis,
			})
			if err == nil {
				t.Fatalf("a too-large rejection produced no error (result=%+v)", result)
			}
			message := err.Error()
			for _, want := range []string{
				fmt.Sprintf("required=%d", test.required),
				"cap_minus_headroom=1031798784",
				"basis=" + test.basis,
				test.wantAdvice,
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("message %q omits %q; both numbers, the basis and the advice for THIS population are what make this refusal actionable at all", message, want)
				}
			}
			if result.basis != "reject:too-large" {
				t.Fatalf("basis=%q, want %q — the run's recorded admission basis", result.basis, "reject:too-large")
			}
			if result.state != "too_large" {
				t.Fatalf("state=%q, want %q — the terminal state the agent guide tells agents not to retry", result.state, "too_large")
			}
			if result.reserve != test.required {
				t.Fatalf("reserve=%d, want the DAEMON-resolved %d", result.reserve, test.required)
			}
			if result.ceiling != tooLargeCeiling {
				t.Fatalf("ceiling=%d, want %d", result.ceiling, tooLargeCeiling)
			}
			// Every arm now names an action. G3's recorded gap was that none did.
			if !strings.Contains(message, "--memory-reserve") && !strings.Contains(message, "run where the slice is larger") {
				t.Fatalf("message %q names no escape hatch at all; that is the G3 gap AIRA-165 closed", message)
			}
		})
	}

	t.Run("the pinned arm is not told to pin", func(t *testing.T) {
		// The defect AIRA-165 exists for: "pin --memory-reserve at or below
		// cap_minus_headroom" was given to every population, and it is the one
		// instruction a request that arrived ALREADY pinned cannot act on.
		result, _, err := tooLargeAdmission(t, DefaultConfineMemoryReserve, tooLargePinnedMessage, runnerAdmitRejection{
			Required: tooLargeRequired, Ceiling: tooLargeCeiling, Basis: tooLargePinnedBasis,
		})
		if err == nil {
			t.Fatalf("a too-large rejection produced no error (result=%+v)", result)
		}
		message := err.Error()
		if strings.Contains(message, "pin a ") || strings.Contains(message, "pin --memory-reserve") {
			t.Fatalf("message %q tells an operator whose reserve is already pinned to pin", message)
		}
		if !strings.Contains(message, "at most cap_minus_headroom") || !strings.Contains(message, "lower the one you passed") {
			t.Fatalf("message %q does not name the action (a reserve at most cap_minus_headroom, by lowering the one passed)", message)
		}
	})
}

// TestTooLargePinnedAdviceDoesNotBlameConfineFlagsOnAnAiraRunRefusal is the
// regression for the arm's honesty (build review, Fable BLOCK).
//
// `pinned:client` is a WIRE FLAG, not an operator action. admitThroughDaemon
// sends `pinned: !req.DaemonEstimateMemory || req.MemoryReservePinned`, and the
// SOLE setter of DaemonEstimateMemory is confine's own launch path -- so every
// `aira run` admission is `pinned:client` even though `aira run` has no
// --memory-reserve, --memory-max or --delegate-ram flag to pass at all. Its
// reserve comes from `run.memory_reserve` in .aira/config or from core's own
// peak-RSS estimate. (The same is true of a `docker run --memory` limit charged
// onto an otherwise unpinned confine job, and of `aira confine-reserve`.)
//
// Every other pinned-arm test here and in internal/daemon uses the CONFINE
// shape -- Request{DaemonEstimateMemory: true}, or args["pinned"] set directly
// -- so the flag's non-flag origins were never exercised, which is exactly how
// the advice came to state a confine flag as the cause. This test drives the
// `aira run` shape instead.
//
// verifies: AIRA-165
func TestTooLargePinnedAdviceDoesNotBlameConfineFlagsOnAnAiraRunRefusal(t *testing.T) {
	// The `aira run` shape: no DaemonEstimateMemory (confine sets it and nothing
	// else does), and no MemoryReservePinned either -- nobody passed a flag.
	request := Request{ResourceSignature: "sig"}

	result, handled, err, pinnedOnWire := tooLargeAdmissionForRequest(
		t, request, DefaultConfineMemoryReserve, tooLargePinnedMessage, runnerAdmitRejection{
			Required: tooLargeRequired, Ceiling: tooLargeCeiling, Basis: tooLargePinnedBasis,
		})

	// The premise, established rather than assumed: this flagless request really
	// does arrive at the daemon as pinned, which is why it lands on this arm.
	if !pinnedOnWire {
		t.Fatalf("the `aira run` shape sent pinned=%v; if that is now false this test no longer drives the population it exists for", pinnedOnWire)
	}
	if !handled {
		t.Fatalf("the exchange was not handled (result=%+v, err=%v)", result, err)
	}
	if err == nil {
		t.Fatalf("a too-large rejection produced no error (result=%+v)", result)
	}
	message := err.Error()

	// An operator here passed NO flag, so any sentence asserting they did is a
	// fabricated cause -- the same fabrication the unrecognised-basis arm returns
	// "" to avoid.
	for _, forbidden := range []string{
		"you pinned this reserve yourself",
		"you pinned",
		"the reserve you pinned",
		"do not re-pin the same number",
	} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("message %q tells an `aira run` caller %q; aira run has no --memory-reserve/--memory-max/--delegate-ram flag, and its reserve comes from run.memory_reserve or AIRA's own estimate", message, forbidden)
		}
	}

	// What it must say instead: the wire fact, the origins as POSSIBILITIES
	// (including this one), and the action.
	for _, want := range []string{
		"was PINNED on the client side",
		"which pin is not established here",
		"run.memory_reserve",
		"at most cap_minus_headroom",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q omits %q, so an `aira run` caller cannot find the reserve AIRA will not name", message, want)
		}
	}
	if strings.Contains(message, "pin a ") || strings.Contains(message, "pin --memory-reserve") {
		t.Fatalf("message %q tells a caller whose reserve is already pinned to pin", message)
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
