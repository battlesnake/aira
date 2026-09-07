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
