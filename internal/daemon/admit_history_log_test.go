package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// verifies: AIRA-332 -- a failed per-signature history read is logged with its
// cause, the elapsed time, the error and a short signature hash, never the argv
// (which may carry secrets), so a fallback:history-unavailable admission after a
// reboot can be diagnosed. A successful read logs nothing.
func TestAdmitHistoryReadFailureIsLoggedWithoutTheArgv(t *testing.T) {
	const signature = "deploy\x00--token\x00s3cret-value"
	for _, test := range []struct {
		name      string
		err       error
		wantCause string
	}{
		{"database error", errors.New("database is locked"), "db-error after "},
		{"deadline", context.DeadlineExceeded, "timeout after "},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(Paths{})
			server.stopping = make(chan struct{})
			server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
				time.Sleep(2 * time.Millisecond)
				return runner.PeakRSSStats{}, test.err
			}
			var basis string
			logged := captureDaemonLog(t, func() {
				_, basis = server.resolveAdmitReserve(admitRequest{reserve: 1 << 30, signature: signature}, 64<<30)
			})
			if !strings.HasPrefix(basis, "fallback:history-unavailable") {
				t.Fatalf("harness shape not reached: basis=%q", basis)
			}
			for _, want := range []string{"confine history read failed", test.wantCause, "ms", test.err.Error(), "signature sha256:"} {
				if !strings.Contains(logged, want) {
					t.Fatalf("log %q lacks %q", logged, want)
				}
			}
			if strings.Contains(logged, "s3cret-value") || strings.Contains(logged, "deploy") {
				t.Fatalf("log leaked the argv: %q", logged)
			}
		})
	}
	t.Run("a successful read logs nothing", func(t *testing.T) {
		server := NewServer(Paths{})
		server.stopping = make(chan struct{})
		server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
			return runner.PeakRSSStats{}, nil
		}
		logged := captureDaemonLog(t, func() {
			server.resolveAdmitReserve(admitRequest{reserve: 1 << 30, signature: signature}, 64<<30)
		})
		if strings.Contains(logged, "history read failed") {
			t.Fatalf("a successful read was logged as a failure: %q", logged)
		}
	})
}
