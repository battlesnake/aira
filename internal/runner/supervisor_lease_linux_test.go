//go:build linux

package runner

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseConfigTimingInvariant(t *testing.T) {
	if !ValidSupervisorLeaseTTL(60*time.Second) || !ValidSupervisorLeaseTTL(120*time.Second) {
		t.Fatal("approved supervisor lease TTL boundary/default rejected")
	}
	for _, ttl := range []time.Duration{-time.Second, 0, 41 * time.Second, 59999 * time.Millisecond} {
		if ValidSupervisorLeaseTTL(ttl) {
			t.Fatalf("invalid supervisor lease TTL %s accepted", ttl)
		}
		if _, err := New(Config{CommonDir: t.TempDir(), SupervisorLeaseTTL: ttl}); ttl != 0 && err == nil {
			t.Fatalf("New accepted invalid explicit TTL %s", ttl)
		}
	}
}

func TestSupervisorLeaseReacquireAllNonOKOutcomes(t *testing.T) {
	for _, outcome := range []string{"expired", "fenced", "token", "absent"} {
		t.Run(outcome, func(t *testing.T) {
			r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			var claims atomic.Int32
			r.supervisorLeaseRenewFn = func(context.Context, string, int64, string) (string, error) { return outcome, nil }
			r.supervisorLeaseClaimFn = func(_ context.Context, claim supervisorLeaseClaim) (int64, string, error) {
				claims.Add(1)
				if claim.TokenHash == "" {
					t.Fatal("reacquire rotated to an empty capability")
				}
				return 2, "claimed", nil
			}
			manager := &supervisorLeaseManager{runner: r, runID: "RUN-1", identity: PIDIdentity{PID: 1, StartTick: 2, BootID: "boot"}, ttl: 60 * time.Second, generation: 1, token: "old"}
			manager.cadence()
			if manager.generation != 2 || manager.token == "" || claims.Load() != 1 {
				t.Fatalf("reacquire outcome=%q manager=%+v claims=%d", outcome, manager, claims.Load())
			}
		})
	}
}

func TestSupervisorLeaseAmbiguousClaimRetainsToken(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	var firstHash string
	r.supervisorLeaseClaimFn = func(_ context.Context, claim supervisorLeaseClaim) (int64, string, error) {
		calls++
		if calls == 1 {
			firstHash = claim.TokenHash
			return 0, "", &supervisorLeaseRouteError{kind: supervisorLeaseAmbiguous, err: errors.New("reply lost")}
		}
		if claim.TokenHash != firstHash {
			t.Fatalf("ambiguous retry changed token hash: %q -> %q", firstHash, claim.TokenHash)
		}
		return 7, "existing", nil
	}
	claim := supervisorLeaseClaim{RunID: "RUN-1", Identity: PIDIdentity{PID: 1, StartTick: 2, BootID: "boot"}, TokenHash: "retained", TTL: 60 * time.Second}
	generation, outcome, err := r.claimSupervisorLeaseWithRetry(context.Background(), claim)
	if err != nil || generation != 7 || outcome != "existing" || calls != 2 {
		t.Fatalf("retry generation=%d outcome=%q calls=%d err=%v", generation, outcome, calls, err)
	}
}

// TestSupervisorLeaseMalformedReplyIsFaultNotLeaseless drives the REAL
// supervisorLeaseDaemonCall socket path (not the claimFn seam): an UP daemon that
// returns a fully-framed but non-JSON body is a protocol FAULT that must fail
// readiness, never degrade to advisory-leaseless (Sol build r1 P0).
func TestSupervisorLeaseMalformedReplyIsFaultNotLeaseless(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.admitSocketPath = "/unused"
	r.admitDialFn = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var req runnerAdmitRequestFrame
			if readRunnerAdmitFrame(server, &req) != nil {
				return
			}
			payload := []byte("not-json-garbage")
			var header [4]byte
			binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
			_, _ = server.Write(header[:])
			_, _ = server.Write(payload)
			var one [1]byte
			_, _ = server.Read(one[:])
		}()
		return client, nil
	}
	record := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, Detached: true, SupervisorPID: PIDIdentity{PID: 1, StartTick: 2, BootID: "boot"}}
	manager, startErr := r.startSupervisorLease(context.Background(), record)
	var launch *LaunchError
	if manager != nil || !errors.As(startErr, &launch) || launch.Code != "E_DAEMON_PROTOCOL" {
		t.Fatalf("malformed reply from an up daemon must fail readiness, not go leaseless: manager=%+v err=%v", manager, startErr)
	}
}

// TestSupervisorLeaseUnhealthyRetriesUntilDurable proves a single append failure
// does not permanently drop the U_RUN_SUPERVISOR_LEASE_UNHEALTHY diagnostic: the
// pending-health flag is retried until the append lands (Sol build r1 P0).
func TestSupervisorLeaseUnhealthyRetriesUntilDurable(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	record := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Detached: true, SupervisorPID: PIDIdentity{PID: 1, StartTick: 2, BootID: "boot"}}
	if _, err := r.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
		t.Fatal(err)
	}
	var failFirst atomic.Bool
	failFirst.Store(true)
	r.appendFault = func(event ledgerEvent) error {
		if event.Kind == "supervisor-lease-unhealthy" && failFirst.CompareAndSwap(true, false) {
			return errors.New("injected append failure")
		}
		return nil
	}
	r.supervisorLeaseRenewFn = func(context.Context, string, int64, string) (string, error) {
		return "", &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: "E_DAEMON_INTERNAL", err: errors.New("db failed")}
	}
	manager := &supervisorLeaseManager{runner: r, runID: record.ID, identity: record.SupervisorPID, ttl: 60 * time.Second, generation: 1, token: "token"}
	manager.cadence()
	if current, _ := r.ledger.current(record.ID); containsString(current.ErrorCodes, "U_RUN_SUPERVISOR_LEASE_UNHEALTHY") {
		t.Fatal("diagnostic recorded despite the injected first-append failure")
	}
	if !manager.unhealthy {
		t.Fatal("pending-health flag was cleared despite the append failure")
	}
	// The NEXT cadence must AUTOMATICALLY flush the pending diagnostic (this test
	// never calls flushUnhealthy directly, so it fails if the cadence auto-retry
	// is removed). Make the renew merely unreachable so no NEW fault is raised.
	r.supervisorLeaseRenewFn = func(context.Context, string, int64, string) (string, error) {
		return "", &supervisorLeaseRouteError{kind: supervisorLeaseDialFailure, err: errors.New("daemon down")}
	}
	manager.cadence()
	current, err := r.ledger.current(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(current.ErrorCodes, "U_RUN_SUPERVISOR_LEASE_UNHEALTHY") || manager.unhealthy {
		t.Fatalf("cadence auto-retry did not durably record the diagnostic: codes=%v pending=%v", current.ErrorCodes, manager.unhealthy)
	}
}

// TestSupervisorLeaseWireIntegersAreExactStrings captures the real request frames
// and asserts identity integers travel as EXACT decimal strings, so a value above
// 2^53 keeps byte identity across the daemon's map decode (float64 would lose it —
// Sol build r1 P1). It fails against numeric encoding.
func TestSupervisorLeaseWireIntegersAreExactStrings(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.admitSocketPath = "/unused"
	var mu sync.Mutex
	var lastArgs map[string]any
	respond := func(data map[string]any) func(context.Context, string) (net.Conn, error) {
		return func(context.Context, string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				var req runnerAdmitRequestFrame
				if readRunnerAdmitFrame(server, &req) != nil {
					return
				}
				mu.Lock()
				lastArgs = req.Request.Args
				mu.Unlock()
				payload, _ := json.Marshal(data)
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: payload})
				var one [1]byte
				_, _ = server.Read(one[:])
			}()
			return client, nil
		}
	}
	const big = uint64(9007199254740993) // 2^53 + 1
	r.admitDialFn = respond(map[string]any{"generation": 1, "outcome": "claimed"})
	if _, _, err := r.claimSupervisorLease(context.Background(), supervisorLeaseClaim{RunID: "RUN-1", Identity: PIDIdentity{PID: 123, StartTick: big, BootID: "boot"}, TokenHash: "hash", TTL: 60 * time.Second}); err != nil {
		t.Fatalf("claim err=%v", err)
	}
	mu.Lock()
	claimArgs := lastArgs
	mu.Unlock()
	if claimArgs["start_tick"] != "9007199254740993" {
		t.Fatalf("start_tick wire value=%#v, want exact decimal string", claimArgs["start_tick"])
	}
	r.admitDialFn = respond(map[string]any{"outcome": "ok"})
	if _, err := r.renewSupervisorLease(context.Background(), "RUN-1", int64(big), "capability"); err != nil {
		t.Fatalf("renew err=%v", err)
	}
	mu.Lock()
	renewArgs := lastArgs
	mu.Unlock()
	if renewArgs["generation"] != "9007199254740993" {
		t.Fatalf("generation wire value=%#v, want exact decimal string", renewArgs["generation"])
	}
}

func TestSupervisorLeaseClaimFailureClassification(t *testing.T) {
	newRunner := func(t *testing.T) *Runner {
		r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	record := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, Detached: true, SupervisorPID: PIDIdentity{PID: 1, StartTick: 2, BootID: "boot"}}
	t.Run("definite dial failure is advisory", func(t *testing.T) {
		r := newRunner(t)
		r.supervisorLeaseClaimFn = func(context.Context, supervisorLeaseClaim) (int64, string, error) {
			return 0, "", &supervisorLeaseRouteError{kind: supervisorLeaseDialFailure, err: errors.New("no daemon")}
		}
		manager, err := r.startSupervisorLease(context.Background(), record)
		if err != nil || manager == nil || manager.generation != 0 {
			t.Fatalf("manager=%+v err=%v", manager, err)
		}
		manager.stopAndRelease()
	})
	t.Run("up but broken daemon fails readiness", func(t *testing.T) {
		r := newRunner(t)
		r.supervisorLeaseClaimFn = func(context.Context, supervisorLeaseClaim) (int64, string, error) {
			return 0, "", &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: "E_DAEMON_INTERNAL", err: errors.New("database failed")}
		}
		manager, err := r.startSupervisorLease(context.Background(), record)
		var launch *LaunchError
		if manager != nil || !errors.As(err, &launch) || launch.Code != "E_DAEMON_INTERNAL" {
			t.Fatalf("manager=%+v err=%v", manager, err)
		}
	})
}

func TestSupervisorLeasePostReadinessFaultPersistsDiagnostic(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	record := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Detached: true, SupervisorPID: PIDIdentity{PID: 1, StartTick: 2, BootID: "boot"}}
	if _, err := r.append(ledgerEvent{Kind: "starting", Run: record}); err != nil {
		t.Fatal(err)
	}
	r.supervisorLeaseRenewFn = func(context.Context, string, int64, string) (string, error) {
		return "", &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: "E_DAEMON_INTERNAL", err: errors.New("database failed")}
	}
	manager := &supervisorLeaseManager{runner: r, runID: record.ID, identity: record.SupervisorPID, ttl: 60 * time.Second, generation: 1, token: "token"}
	manager.cadence()
	current, err := r.ledger.current(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, code := range current.ErrorCodes {
		found = found || code == "U_RUN_SUPERVISOR_LEASE_UNHEALTHY"
	}
	if current.Status.Terminal() || !found {
		t.Fatalf("post-readiness lease fault was not persistently surfaced: %+v", current)
	}
}

func TestSupervisorLeaseOperationsAreSerialised(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var active, maximum atomic.Int32
	enter := func() {
		current := active.Add(1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
	}
	r.supervisorLeaseRenewFn = func(context.Context, string, int64, string) (string, error) { enter(); return "expired", nil }
	r.supervisorLeaseClaimFn = func(context.Context, supervisorLeaseClaim) (int64, string, error) { enter(); return 2, "claimed", nil }
	manager := &supervisorLeaseManager{runner: r, runID: "RUN-1", identity: PIDIdentity{PID: 1, StartTick: 2, BootID: "boot"}, ttl: 60 * time.Second, generation: 1, token: "old"}
	manager.cadence()
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent lease operations=%d", maximum.Load())
	}
}

func TestSupervisorLeaseStopReleasesCurrentGeneration(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, SupervisorLeaseTTL: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	never := make(chan time.Time)
	r.supervisorLeaseAfter = func(time.Duration) <-chan time.Time { return never }
	released := make(chan struct{}, 1)
	r.supervisorLeaseReleaseFn = func(_ context.Context, runID string, generation int64, token string) (string, error) {
		if runID != "RUN-1" || generation != 9 || token != "capability" {
			t.Fatalf("release run=%q generation=%d token=%q", runID, generation, token)
		}
		released <- struct{}{}
		return "ok", nil
	}
	manager := &supervisorLeaseManager{runner: r, runID: "RUN-1", ttl: 60 * time.Second, generation: 9, token: "capability", stop: make(chan struct{}), done: make(chan struct{})}
	go manager.run()
	manager.stopAndRelease()
	select {
	case <-released:
	default:
		t.Fatal("terminal stop did not release the current supervisor lease")
	}
}
