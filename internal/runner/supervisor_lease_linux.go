//go:build linux

package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	supervisorLeaseRouteDeadline = 5 * time.Second
	supervisorLeaseAttempts      = 3
)

type supervisorLeaseClaim struct {
	RunID     string
	Identity  PIDIdentity
	TokenHash string
	TTL       time.Duration
}

type supervisorLeaseRouteKind uint8

const (
	supervisorLeaseDialFailure supervisorLeaseRouteKind = iota
	supervisorLeaseAmbiguous
	supervisorLeaseFault
)

type supervisorLeaseRouteError struct {
	kind supervisorLeaseRouteKind
	code string
	err  error
}

func (e *supervisorLeaseRouteError) Error() string {
	if e.code != "" {
		return e.code + ": " + e.err.Error()
	}
	return e.err.Error()
}

func (e *supervisorLeaseRouteError) Unwrap() error { return e.err }

type supervisorLeaseManager struct {
	runner     *Runner
	runID      string
	identity   PIDIdentity
	ttl        time.Duration
	stop       chan struct{}
	done       chan struct{}
	generation int64
	token      string
	pending    string
	iteration  uint64
	unhealthy  bool
}

func newSupervisorCapability() (token, tokenHash string, err error) {
	clear := make([]byte, 32)
	if _, err := rand.Read(clear); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(clear)
	hash := sha256.Sum256(clear)
	return token, base64.RawURLEncoding.EncodeToString(hash[:]), nil
}

func (r *Runner) startSupervisorLease(ctx context.Context, record RunRecord) (*supervisorLeaseManager, error) {
	manager := &supervisorLeaseManager{runner: r, runID: record.ID, identity: record.SupervisorPID, ttl: r.supervisorLeaseTTL, stop: make(chan struct{}), done: make(chan struct{})}
	token, tokenHash, err := newSupervisorCapability()
	if err != nil {
		return nil, launchErr("E_RUN_SUPERVISOR_LEASE_INVALID", err)
	}
	manager.pending = token
	generation, _, err := r.claimSupervisorLeaseWithRetry(ctx, supervisorLeaseClaim{RunID: record.ID, Identity: record.SupervisorPID, TokenHash: tokenHash, TTL: r.supervisorLeaseTTL})
	if err == nil {
		manager.generation, manager.token, manager.pending = generation, token, ""
	} else if !supervisorLeaseUnreachable(err) {
		return nil, launchErr(supervisorLeaseErrorCode(err), err)
	} else {
		r.warnSupervisorLease("daemon unavailable; starting without a supervisor lease")
	}
	go manager.run()
	return manager, nil
}

func supervisorLeaseUnreachable(err error) bool {
	var route *supervisorLeaseRouteError
	return errors.As(err, &route) && (route.kind == supervisorLeaseDialFailure || route.kind == supervisorLeaseAmbiguous)
}

// supervisorLeaseTransportLost reports a genuinely broken transport where the
// reply was lost (EOF/timeout/reset) — the only frame-read failure that is
// ambiguous. A malformed-but-fully-read frame is a protocol fault, not this.
func supervisorLeaseTransportLost(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func supervisorLeaseErrorCode(err error) string {
	var route *supervisorLeaseRouteError
	if errors.As(err, &route) && route.code != "" {
		return route.code
	}
	message := err.Error()
	if index := strings.IndexByte(message, ':'); index > 0 && (strings.HasPrefix(message, "E_") || strings.HasPrefix(message, "U_")) {
		return message[:index]
	}
	return "E_RUN_SUPERVISOR_LEASE_FAILED"
}

func (r *Runner) warnSupervisorLease(message string) {
	if r.diagnostics != nil {
		_, _ = fmt.Fprintf(r.diagnostics, "aira: warning: %s\n", message)
	}
}

func (r *Runner) claimSupervisorLeaseWithRetry(ctx context.Context, claim supervisorLeaseClaim) (int64, string, error) {
	var last error
	for attempt := 0; attempt < supervisorLeaseAttempts; attempt++ {
		generation, outcome, err := r.claimSupervisorLease(ctx, claim)
		if err == nil {
			return generation, outcome, nil
		}
		last = err
		var route *supervisorLeaseRouteError
		if !errors.As(err, &route) || route.kind == supervisorLeaseFault || route.kind == supervisorLeaseDialFailure {
			return 0, "", err
		}
		if attempt+1 < supervisorLeaseAttempts {
			delay := 500 * time.Millisecond
			if attempt == 1 {
				delay = time.Second
			}
			select {
			case <-ctx.Done():
				return 0, "", ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return 0, "", last
}

func (r *Runner) claimSupervisorLease(ctx context.Context, claim supervisorLeaseClaim) (int64, string, error) {
	if r.supervisorLeaseClaimFn != nil {
		return r.supervisorLeaseClaimFn(ctx, claim)
	}
	// Identity integers travel as decimal STRINGS so a >2^53 value keeps byte
	// identity across the daemon's map decode (float64 would lose precision — the
	// M21 lesson); leaseIntArg parses strings exactly (Sol build r1 P1).
	args := map[string]any{
		"run_id": claim.RunID, "pid": strconv.FormatInt(int64(claim.Identity.PID), 10),
		"start_tick": strconv.FormatUint(claim.Identity.StartTick, 10),
		"boot_id":    claim.Identity.BootID, "ttl_ms": strconv.FormatInt(claim.TTL.Milliseconds(), 10),
		"token_hash": claim.TokenHash,
	}
	var response struct {
		Generation int64  `json:"generation"`
		Outcome    string `json:"outcome"`
	}
	if err := r.supervisorLeaseDaemonCall(ctx, "supervise-lease-claim", args, &response); err != nil {
		return 0, "", err
	}
	if response.Generation < 1 || (response.Outcome != "claimed" && response.Outcome != "existing") {
		return 0, "", &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: "E_DAEMON_PROTOCOL", err: errors.New("invalid supervisor lease claim response")}
	}
	return response.Generation, response.Outcome, nil
}

func (r *Runner) renewSupervisorLease(ctx context.Context, runID string, generation int64, token string) (string, error) {
	if r.supervisorLeaseRenewFn != nil {
		return r.supervisorLeaseRenewFn(ctx, runID, generation, token)
	}
	var response struct {
		Outcome string `json:"outcome"`
	}
	if err := r.supervisorLeaseDaemonCall(ctx, "supervise-lease-renew", map[string]any{"run_id": runID, "generation": strconv.FormatInt(generation, 10), "token": token}, &response); err != nil {
		return "", err
	}
	switch response.Outcome {
	case "ok", "expired", "fenced", "token", "absent":
		return response.Outcome, nil
	default:
		return "", &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: "E_DAEMON_PROTOCOL", err: errors.New("invalid supervisor lease renew response")}
	}
}

func (r *Runner) renewSupervisorLeaseWithRetry(ctx context.Context, runID string, generation int64, token string) (string, error) {
	var last error
	for attempt := 0; attempt < supervisorLeaseAttempts; attempt++ {
		outcome, err := r.renewSupervisorLease(ctx, runID, generation, token)
		if err == nil {
			return outcome, nil
		}
		last = err
		var route *supervisorLeaseRouteError
		if !errors.As(err, &route) || route.kind == supervisorLeaseFault {
			return "", err
		}
		if attempt+1 < supervisorLeaseAttempts {
			delay := 500 * time.Millisecond
			if attempt == 1 {
				delay = time.Second
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return "", last
}

func (r *Runner) releaseSupervisorLease(ctx context.Context, runID string, generation int64, token string) (string, error) {
	if r.supervisorLeaseReleaseFn != nil {
		return r.supervisorLeaseReleaseFn(ctx, runID, generation, token)
	}
	var response struct {
		Outcome string `json:"outcome"`
	}
	if err := r.supervisorLeaseDaemonCall(ctx, "supervise-lease-release", map[string]any{"run_id": runID, "generation": strconv.FormatInt(generation, 10), "token": token}, &response); err != nil {
		return "", err
	}
	return response.Outcome, nil
}

func (r *Runner) supervisorLeaseDaemonCall(ctx context.Context, verb string, args map[string]any, data any) error {
	callCtx, cancel := context.WithTimeout(ctx, supervisorLeaseRouteDeadline)
	defer cancel()
	dial := r.admitDialFn
	if dial == nil {
		if strings.TrimSpace(r.admitSocketPath) == "" {
			return &supervisorLeaseRouteError{kind: supervisorLeaseDialFailure, err: errors.New("daemon socket is not configured")}
		}
		dial = func(ctx context.Context, path string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
	}
	conn, err := dial(callCtx, r.admitSocketPath)
	if err != nil {
		return &supervisorLeaseRouteError{kind: supervisorLeaseDialFailure, err: err}
	}
	defer conn.Close()
	if deadline, ok := callCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: cloneAnyMap(r.daemonScope)}
	frame.Request.Verb, frame.Request.Args = verb, args
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		return &supervisorLeaseRouteError{kind: supervisorLeaseAmbiguous, err: err}
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		// Only a genuinely LOST transport (EOF/timeout/reset) is ambiguous — the
		// reply may or may not reflect a committed op. A fully-read but malformed
		// frame is an UP daemon speaking garbage: a fault, not unreachable, so it
		// must not degrade to advisory-leaseless (Sol build r1 P0).
		if supervisorLeaseTransportLost(err) {
			return &supervisorLeaseRouteError{kind: supervisorLeaseAmbiguous, err: err}
		}
		return &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: "E_DAEMON_PROTOCOL", err: err}
	}
	if !response.OK || response.Code != "OK" {
		code := response.Code
		if code == "" {
			code = "E_DAEMON_PROTOCOL"
		}
		return &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: code, err: errors.New(response.Error)}
	}
	if err := json.Unmarshal(response.Data, data); err != nil {
		return &supervisorLeaseRouteError{kind: supervisorLeaseFault, code: "E_DAEMON_PROTOCOL", err: err}
	}
	return nil
}

func (m *supervisorLeaseManager) run() {
	defer close(m.done)
	for {
		m.flushUnhealthy()
		interval := jitteredPoll(m.ttl/3, m.iteration)
		m.iteration++
		after := m.runner.supervisorLeaseAfter
		var tick <-chan time.Time
		if after != nil {
			tick = after(interval)
		} else {
			tick = time.After(interval)
		}
		select {
		case <-m.stop:
			m.flushUnhealthy()
			if m.generation > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), supervisorLeaseRouteDeadline)
				_, _ = m.runner.releaseSupervisorLease(ctx, m.runID, m.generation, m.token)
				cancel()
			}
			return
		case <-tick:
			m.cadence()
		}
	}
}

// flagUnhealthy records that a real (non-unreachable) lease fault occurred and
// immediately attempts to surface it. The flag is retried each cadence until the
// U_RUN_SUPERVISOR_LEASE_UNHEALTHY diagnostic is durably recorded (Sol build r1
// P0): a single append failure must never permanently drop the diagnostic.
func (m *supervisorLeaseManager) flagUnhealthy() {
	m.unhealthy = true
	m.flushUnhealthy()
}

func (m *supervisorLeaseManager) flushUnhealthy() {
	if m.unhealthy && m.runner.markSupervisorLeaseUnhealthy(m.runID) {
		m.unhealthy = false
	}
}

func (m *supervisorLeaseManager) cadence() {
	m.flushUnhealthy()
	if m.generation > 0 {
		outcome, err := m.runner.renewSupervisorLeaseWithRetry(context.Background(), m.runID, m.generation, m.token)
		if err != nil {
			if !supervisorLeaseUnreachable(err) {
				m.flagUnhealthy()
			}
			return
		}
		if outcome == "ok" {
			return
		}
		m.generation, m.token = 0, ""
		pending, _, err := newSupervisorCapability()
		if err != nil {
			m.flagUnhealthy()
			return
		}
		m.pending = pending
	}
	if m.pending == "" {
		pending, _, err := newSupervisorCapability()
		if err != nil {
			m.flagUnhealthy()
			return
		}
		m.pending = pending
	}
	decoded, err := base64.RawURLEncoding.DecodeString(m.pending)
	if err != nil {
		m.flagUnhealthy()
		return
	}
	hash := sha256.Sum256(decoded)
	claim := supervisorLeaseClaim{RunID: m.runID, Identity: m.identity, TokenHash: base64.RawURLEncoding.EncodeToString(hash[:]), TTL: m.ttl}
	generation, _, err := m.runner.claimSupervisorLeaseWithRetry(context.Background(), claim)
	if err != nil {
		if !supervisorLeaseUnreachable(err) {
			m.flagUnhealthy()
		}
		return
	}
	m.generation, m.token, m.pending = generation, m.pending, ""
}

func (m *supervisorLeaseManager) stopAndRelease() {
	if m == nil {
		return
	}
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

// markSupervisorLeaseUnhealthy reports true when the diagnostic is durably
// recorded OR no longer needed (the run is terminal, or the code is already
// present). It reports FALSE on any transient failure (lock/read/append) so the
// caller retries until the diagnostic is durable (Sol build r1 P0).
func (r *Runner) markSupervisorLeaseUnhealthy(runID string) bool {
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), runID+".lock"))
	if err != nil {
		return false
	}
	defer unlockFile(lock)
	current, err := r.ledger.current(runID)
	if err != nil {
		return false
	}
	if current.Status.Terminal() {
		return true // terminal runs no longer need the advisory diagnostic.
	}
	updated := appendUnique(current.ErrorCodes, "U_RUN_SUPERVISOR_LEASE_UNHEALTHY")
	if len(updated) == len(current.ErrorCodes) {
		return true // already recorded.
	}
	current.ErrorCodes = updated
	if _, err := r.append(ledgerEvent{Kind: "supervisor-lease-unhealthy", Run: current}); err != nil {
		return false
	}
	return true
}
