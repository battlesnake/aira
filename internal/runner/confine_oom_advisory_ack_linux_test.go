package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// verifies: AIRA-217 -- the own-cap OOM advisory is built from what the client
// can ESTABLISH about the peak report: only an acknowledged (nil-error) report
// that carried a known peak lets it say the kill was recorded. An error from
// reportPeak means "unconfirmed", not "not recorded": the daemon commits the
// sample before it replies, so a lost reply follows a persisted sample.

// daemonReserveOOMDiagnostics runs a confine job whose own AIRA-estimated cap is
// OOM-killed, with the given peak report seam, and returns the diagnostics text.
func daemonReserveOOMDiagnostics(t *testing.T, peak *int64, reportPeak func(context.Context, ConfineRequest, ConfinePeakReport) error) string {
	t.Helper()
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.readUsage = func(string) cgroupUsage {
		return cgroupUsage{
			OOMKill: int64ptr(1), OOMKillLocal: int64ptr(1), OOMGroupKillLocal: int64ptr(0),
			OOMLocal: int64ptr(1), PeakRSS: peak,
		}
	}
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate", reserve: 32 << 20, basis: "estimate:p90-prior", release: &confineCountingCloser{}}, nil
	}
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	deps.reportPeak = reportPeak
	var diagnostics bytes.Buffer
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "kill -s KILL $$"}, SelfPath: os.Args[0], Stderr: &diagnostics,
	}, deps); err != nil {
		t.Fatalf("confine: %v (diagnostics=%q)", err, diagnostics.String())
	}
	out := diagnostics.String()
	if !strings.Contains(out, "cap-source=auto:daemon-reserve") || !strings.Contains(out, "OOM-killed at its memory cap") {
		t.Fatalf("fixture did not reach the daemon-reserve own-cap OOM advisory: %q", out)
	}
	return out
}

func assertHedgedOOMAdvisory(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "AIRA could not confirm that this kill was recorded") || !strings.Contains(out, "aira confine --budget") {
		t.Fatalf("advisory is not the hedged text: %q", out)
	}
	for _, forbidden := range []string{"The kill and its peak are recorded", "now recorded", "not recorded", "NOT recorded", "sized higher", "genuine bug worth reporting"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("hedged advisory contains %q: %q", forbidden, out)
		}
	}
}

func TestOOMAdvisoryPlumbingFollowsTheReportAcknowledgement(t *testing.T) {
	peak := int64(31 << 20)
	t.Run("a report error hedges", func(t *testing.T) {
		out := daemonReserveOOMDiagnostics(t, &peak, func(context.Context, ConfineRequest, ConfinePeakReport) error {
			return errors.New("daemon unreachable")
		})
		assertHedgedOOMAdvisory(t, out)
	})
	t.Run("an acknowledgement lost after persistence hedges and never claims not-recorded", func(t *testing.T) {
		var history []ConfinePeakReport
		out := daemonReserveOOMDiagnostics(t, &peak, func(_ context.Context, _ ConfineRequest, report ConfinePeakReport) error {
			history = append(history, report) // the daemon committed the sample...
			return context.DeadlineExceeded   // ...and the reply was lost
		})
		if len(history) != 1 || !history[0].OOM || history[0].Peak == nil || *history[0].Peak != peak {
			t.Fatalf("fixture did not persist the sample before losing the reply: %+v", history)
		}
		assertHedgedOOMAdvisory(t, out)
	})
	t.Run("an acknowledged report with a known peak says recorded", func(t *testing.T) {
		out := daemonReserveOOMDiagnostics(t, &peak, func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil })
		if !strings.Contains(out, "The kill and its peak are recorded against this command's signature") {
			t.Fatalf("an acknowledged report with a peak did not get the recorded text: %q", out)
		}
		if strings.Contains(out, "sized higher") || strings.Contains(out, "could not confirm") {
			t.Fatalf("recorded text carries a forbidden phrase: %q", out)
		}
	})
	t.Run("an acknowledged report with an unreadable peak hedges", func(t *testing.T) {
		out := daemonReserveOOMDiagnostics(t, nil, func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil })
		assertHedgedOOMAdvisory(t, out)
	})
}
