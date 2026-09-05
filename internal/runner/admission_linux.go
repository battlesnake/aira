//go:build linux

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type admissionResult struct {
	state        string
	reason       string
	waitedMS     int64
	lock         *admitLock
	release      io.Closer
	reserve      int64
	ceiling      int64
	scopeCeiling int64
	basis        string
}

var errDetachKillIntent = errors.New("detached run has a pending kill intent")

type admitLock struct {
	mu   sync.Mutex
	file *os.File
}

func (l *admitLock) release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	f := l.file
	l.file = nil
	l.mu.Unlock()
	if f != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}
}

func (l *admitLock) Close() error {
	l.release()
	return nil
}

func (result admissionResult) releaseAdmission() {
	if result.release != nil {
		_ = result.release.Close()
		return
	}
	result.lock.release()
}

// DaemonProtocolVersion is the wire protocol the runner's admission client
// speaks. It MUST equal daemon.ProtocolVersion. The runner cannot import
// internal/daemon (daemon imports runner), so the two are pinned equal by
// TestRunnerDaemonProtocolVersionMatchesTheDaemon in the external runner_test
// package rather than derived — a bump on one side alone fails that test
// instead of silently breaking admission negotiation (AIRA-83 item 3).
const DaemonProtocolVersion = 7

const (
	runnerDaemonMaxFrameBytes = 16 << 20
	admitTransportGrace       = time.Second
)

// errWorkerAdmitFrameSize is a SENTINEL, not a fresh errors.New at each call
// site, so RequestWorkerAdmit can classify "the daemon sent something that is
// not a frame" with errors.Is instead of matching the message text — the
// AIRA-42 class this channel exists to close.
var errWorkerAdmitFrameSize = errors.New("invalid daemon admission frame size")

type runnerAdmitRequestFrame struct {
	Proto   int            `json:"proto"`
	Scope   map[string]any `json:"scope"`
	Request struct {
		Verb       string         `json:"verb"`
		Args       map[string]any `json:"args,omitempty"`
		HasContent bool           `json:"has_content"`
	} `json:"request"`
}

type runnerAdmitResponseFrame struct {
	OK   bool   `json:"ok"`
	Code string `json:"code"`
	// Proto is set ONLY by the daemon's protocolMismatchFrame
	// (internal/daemon/protocol.go) — errorFrame and responseFrame both
	// leave it zero. That makes a non-zero Proto a STRUCTURAL discriminator
	// for "your client and this daemon speak different protocol versions",
	// which is how the worker-admit client tells a version skew apart from
	// an ordinary E_DAEMON_PROTOCOL argument rejection instead of matching
	// the words "daemon protocol is" out of the error sentence (AIRA-45,
	// AIRA-83(b)). Decoding it is what makes that possible; before this it
	// was simply dropped.
	Proto int             `json:"proto,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

type runnerAdmitGrant struct {
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	WaitedMS     int64  `json:"waited_ms"`
	Reserve      int64  `json:"reserve"`
	Basis        string `json:"basis"`
	ScopeCeiling int64  `json:"scope_ceiling,omitempty"`
}

type runnerAdmitRejection struct {
	Required int64  `json:"required,omitempty"`
	Ceiling  int64  `json:"cap_minus_headroom,omitempty"`
	Basis    string `json:"basis"`
}

// ErrExclusiveUnavailable prefixes every refusal of an `--exclusive` request
// that could not be granted by the daemon (AIRA-101).
//
// The rule it enforces: an exclusive request NEVER degrades. Not to the flock
// fallback, not to an `unevaluated` launch, not to `disabled` or `bypassed`.
// This is the highest-priority property of the feature, and it comes from a real
// incident — an hour of benchmark throughput numbers was invalidated by
// contention nobody noticed. A benchmark that silently runs non-exclusively
// produces numbers that LOOK clean, which is strictly worse than no feature at
// all, so the only safe answer when exclusivity cannot be established is to
// refuse to launch and say why.
const ErrExclusiveUnavailable = "E_CONFINE_UNAVAILABLE"

// exclusiveRefusal builds that refusal, carrying the daemon's OWN code and
// message when it answered one. The distinction matters operationally: "another
// benchmark holds the slice" (retry later), "the daemon could not establish an
// empty slice" (something is wrong with cgroupfs), "your daemon is too old"
// (reinstall) and "the daemon is unreachable" all demand different actions, and
// a single generic message would hide which one happened.
func exclusiveRefusal(daemonCode, daemonMessage string) error {
	detail := strings.TrimSpace(daemonMessage)
	if detail == "" {
		detail = strings.TrimSpace(daemonCode)
	}
	if detail == "" {
		return fmt.Errorf("%s: --exclusive requires a daemon admission grant and the daemon did not provide one; refusing to launch non-exclusively", ErrExclusiveUnavailable)
	}
	return fmt.Errorf("%s: --exclusive requires a daemon admission grant; refusing to launch non-exclusively (%s)", ErrExclusiveUnavailable, detail)
}

func (r *Runner) admit(ctx context.Context, req Request) (admissionResult, error) {
	if req.NoAdmit {
		if req.Exclusive {
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"},
				exclusiveRefusal("", "admission is bypassed for this launch, so exclusivity cannot be established")
		}
		return admissionResult{state: "bypassed"}, nil
	}
	effectiveReserve := r.memoryReserve
	if req.MemoryReserveOverride != nil && *req.MemoryReserveOverride > 0 {
		effectiveReserve = *req.MemoryReserveOverride
	}
	if r.memorySlice == "" || r.memoryReserve == 0 {
		if req.Exclusive {
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"},
				exclusiveRefusal("", "admission is disabled for this launch, so exclusivity cannot be established")
		}
		return admissionResult{state: "disabled"}, nil
	}
	// AIRA-58: enforce the shared ceiling HERE, before either admission path.
	// Neither the CLI parse check nor the daemon covers a programmatic caller when
	// the daemon is DOWN: admitWithFlock waits on the raw r.admissionMaxWait, so
	// an over-ceiling request would simply become an over-ceiling flock wait.
	// Refused with the terminal code, never silently clamped.
	if r.admissionMaxWait > AdmitWaitCeiling {
		return admissionResult{state: "wait_too_long", basis: "reject:wait-too-long"}, fmt.Errorf(
			"E_ADMIT_WAIT_TOO_LONG: requested admission wait %s exceeds the ceiling of %s",
			r.admissionMaxWait, AdmitWaitCeiling)
	}
	start := r.clock.Now()
	if result, granted, err := r.admitThroughDaemon(ctx, req, effectiveReserve); granted || err != nil {
		return result, err
	}
	// AIRA-101. Past here lies the flock fallback, which launches OUTSIDE the
	// daemon ledger and therefore outside any notion of exclusivity. Reaching it
	// with an exclusive request would launch a benchmark that believes it is alone
	// and is not. Refuse instead — including when the daemon was simply
	// unreachable, which is the commonest way to get here.
	if req.Exclusive {
		return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"},
			exclusiveRefusal("", "the daemon did not answer an exclusive admission request")
	}
	daemonWaited := false
	if r.admitDialFn != nil || strings.TrimSpace(r.admitSocketPath) != "" {
		daemonWaited = r.clock.Now().Sub(start) >= time.Millisecond
	}
	path, ok, reason := resolveSlicePath(r.memorySlice)
	if !ok {
		r.warnAdmission("unevaluated", reason)
		return admissionResult{state: "unevaluated", reason: reason}, nil
	}
	return r.admitWithFlock(ctx, req, path, start, effectiveReserve, daemonWaited)
}

// admitWithFlock is the retained #29 self-gating implementation. Every daemon
// failure closes its socket before entering this one fallback path.
func (r *Runner) admitWithFlock(ctx context.Context, req Request, path string, start time.Time, effectiveReserve int64, waited bool) (admissionResult, error) {
	// Time already spent waiting on a responsive-but-incomplete daemon outcome
	// belongs to this admission attempt. Ignore sub-millisecond dial failures so
	// an immediately acquired fallback lock remains "immediate".
	lastNote := start.Add(-time.Hour)
	iteration := uint64(0)
	finish := func(state, reason string, lock *admitLock) admissionResult {
		result := admissionResult{state: state, reason: reason, lock: lock, reserve: effectiveReserve, basis: "fallback:daemon-unavailable"}
		if lock != nil {
			result.release = lock
		}
		if waited {
			result.waitedMS = r.clock.Now().Sub(start).Milliseconds()
		}
		return result
	}
	for {
		if err := ctx.Err(); err != nil {
			return admissionResult{}, err
		}
		if req.Detach {
			current, currentErr := r.ledger.current(req.detachRunID)
			if currentErr != nil {
				return admissionResult{}, launchErr("U_RUN_RECONCILE_REQUIRED", currentErr)
			}
			if currentErr == nil && current.Detached && current.KillIntent.Present {
				return admissionResult{}, errDetachKillIntent
			}
		}
		cur, max, ok, reason := r.sliceMemory(path)
		if !ok {
			r.warnAdmission("unevaluated", reason)
			return finish("unevaluated", reason, nil), nil
		}
		if max-cur >= effectiveReserve {
			lockAttempt := r.lockAttemptFn
			if lockAttempt == nil {
				lockAttempt = tryAdmissionLock
			}
			lock, lockErr := lockAttempt(path)
			switch {
			case lockErr == nil:
				cur2, max2, ok2, reason2 := r.sliceMemory(path)
				if !ok2 {
					lock.release()
					r.warnAdmission("unevaluated", reason2)
					return finish("unevaluated", reason2, nil), nil
				}
				if max2-cur2 >= effectiveReserve {
					state := "immediate"
					if waited {
						state = "waited"
					}
					return finish(state, "", lock), nil
				}
				lock.release()
				cur, max = cur2, max2
			case errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN) || errors.Is(lockErr, unix.EINTR):
				// Contention and interrupted flock attempts share the bounded outer loop.
			default:
				r.warnAdmission("unevaluated", "lock-error")
				return finish("unevaluated", "lock-error", nil), nil
			}
		}
		now := r.clock.Now()
		remaining := r.admissionMaxWait - now.Sub(start)
		if remaining <= 0 {
			r.warnAdmission("timeout", "")
			return finish("timeout", "", nil), nil
		}
		waited = true
		if now.Sub(lastNote) >= 30*time.Second {
			r.noteAdmission(cur, max, effectiveReserve)
			lastNote = now
		}
		delay := jitteredPoll(r.pollInterval, iteration)
		iteration++
		if delay > remaining {
			delay = remaining
		}
		if delay <= 0 {
			delay = remaining
			if delay <= 0 {
				delay = time.Nanosecond
			}
		}
		select {
		case <-ctx.Done():
			return admissionResult{}, ctx.Err()
		case <-r.clock.After(delay):
		}
	}
}

func (r *Runner) admitThroughDaemon(ctx context.Context, req Request, effectiveReserve int64) (admissionResult, bool, error) {
	admissionStarted := time.Now()
	dial := r.admitDialFn
	if dial == nil {
		if strings.TrimSpace(r.admitSocketPath) == "" {
			return admissionResult{}, false, nil
		}
		dial = func(ctx context.Context, path string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		}
	}
	// AIRA-58: the requested wait goes on the wire AS-IS. This used to be silently
	// clamped to a private 30-minute runnerAdmitWaitCap right here, BEFORE the
	// request ever reached the daemon, so `--admit-timeout 2h` became 30m on the
	// wire and NO daemon-side test could observe it. The ceiling now lives in
	// exactly one place (runner.AdmitWaitCeiling) and is enforced by REFUSAL at
	// the edges (CLI parse time, and the daemon), never by silent substitution.
	maxWait := r.admissionMaxWait
	// The transport deadline follows the REQUESTED wait. Deriving it from a
	// shorter clamped value tore the connection down while the daemon was still
	// legitimately holding the request, and a torn connection routes into the
	// flock fallback — an UNACCOUNTED launch — instead of an honest saturated
	// rejection. A wedged daemon is still bounded, just at the caller's own
	// declared budget rather than a hidden one.
	deadlineWait := maxWait
	if deadlineWait > time.Duration(mathMaxInt64)-admitTransportGrace {
		deadlineWait = time.Duration(mathMaxInt64) - admitTransportGrace
	}
	transportDeadline := time.Now().Add(deadlineWait + admitTransportGrace)
	transportCtx, cancelTransport := context.WithDeadline(ctx, transportDeadline)
	defer cancelTransport()
	conn, err := dial(transportCtx, r.admitSocketPath)
	if err != nil {
		if err := ctx.Err(); err != nil {
			return admissionResult{}, false, err
		}
		return admissionResult{}, false, nil
	}
	monitorStop := make(chan struct{})
	monitorDone := make(chan struct{})
	monitorErr := make(chan error, 1)
	var monitorStopOnce sync.Once
	stopMonitor := func() { monitorStopOnce.Do(func() { close(monitorStop) }) }
	if req.Detach {
		if err := r.checkDetachAdmission(req); err != nil {
			_ = conn.Close()
			return admissionResult{}, false, err
		}
		interval := r.pollInterval
		if interval <= 0 {
			interval = 2 * time.Second
		}
		go func() {
			defer close(monitorDone)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-monitorStop:
					return
				case <-ticker.C:
					if err := r.checkDetachAdmission(req); err != nil {
						monitorErr <- err
						_ = conn.Close()
						return
					}
				}
			}
		}()
		defer stopMonitor()
	} else {
		close(monitorDone)
	}
	// fail closes the socket first, then routes to the SINGLE flock fallback
	// (§2.1). This is the plan-approved documented advisory degradation: the flock
	// serialises fallback clients (bounded, unlike an ungated unevaluated
	// stampede — Sol build r2), while its cross-domain over-grant against live
	// daemon reservations is bounded by the OOMPolicy=kill backstop. A detach
	// kill-intent or ctx cancellation aborts instead.
	fail := func() (admissionResult, bool, error) {
		_ = conn.Close()
		select {
		case err := <-monitorErr:
			return admissionResult{}, false, err
		default:
		}
		if err := ctx.Err(); err != nil {
			return admissionResult{}, false, err
		}
		if req.Detach {
			if err := r.checkDetachAdmission(req); err != nil {
				return admissionResult{}, false, err
			}
		}
		// AIRA-101. Every remaining route out of fail() ends in the flock fallback,
		// which launches outside the ledger and outside exclusivity. An exclusive
		// request refuses here instead, so a torn connection, an unreadable frame or
		// an unrecognised daemon code can never become a silently contended
		// benchmark.
		if req.Exclusive {
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"}, true,
				exclusiveRefusal("", "the exclusive admission exchange with the daemon did not complete")
		}
		return admissionResult{}, false, nil
	}

	_ = conn.SetDeadline(transportDeadline)
	// The connection deadline bounds the transport. Only caller cancellation
	// closes asynchronously: a full frame that completes exactly at the
	// transport deadline must win and keep this lease open through Start.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "admit"
	frame.Request.Args = map[string]any{
		"slice": r.memorySlice, "reserve": effectiveReserve,
		"max_wait_ms": maxWait.Milliseconds(),
		"signature":   req.ResourceSignature,
		"pinned":      !req.DaemonEstimateMemory || req.MemoryReservePinned,
	}
	if req.DelegateRAM {
		frame.Request.Args["delegate_ram"] = true
	}
	if req.ConfineScopeID != "" {
		frame.Request.Args["scope_id"] = req.ConfineScopeID
		frame.Request.Args["name"] = req.ConfineName
		frame.Request.Args["owner"] = req.ConfineOwner
	}
	// AIRA-101. All three are optional and absent-means-off, so an older daemon
	// is not confused by their presence — it REJECTS them with E_DAEMON_PROTOCOL,
	// which for an exclusive request is exactly right: admitExclusiveOrRefuse
	// turns that into a loud refusal rather than a silently non-exclusive launch.
	if req.Exclusive {
		frame.Request.Args["exclusive"] = true
	}
	if req.ExclusiveHolder != "" {
		frame.Request.Args["exclusive_holder"] = req.ExclusiveHolder
	}
	if req.ParentScopeID != "" {
		frame.Request.Args["parent_scope_id"] = req.ParentScopeID
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		return fail()
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		return fail()
	}
	if !response.OK || response.Code != "OK" {
		// AIRA-58: a wait-ceiling refusal is TERMINAL and must never reach fail(),
		// which routes to the flock fallback and would launch the job outside the
		// daemon ledger — turning a refusal into an unaccounted admission, strictly
		// worse than the silent clamp it replaced. Deliberately handled BEFORE the
		// structured-payload branch and WITHOUT depending on the payload parsing,
		// so a malformed or absent rejection body still refuses rather than
		// degrading. Any future admit-path refusal code needs the same treatment.
		// AIRA-101. The two exclusive refusals join this TERMINAL pre-payload
		// block for the same reason E_ADMIT_WAIT_TOO_LONG is here: anything the
		// runner does not explicitly recognise falls through to fail() and the
		// flock fallback. For an exclusive request that would be doubly wrong —
		// the job would launch both unaccounted AND non-exclusive while its
		// operator believed otherwise. Handled before the structured-payload
		// branch so a malformed body still refuses rather than degrading.
		//
		// E_DAEMON_PROTOCOL is included because that is precisely what an OLDER
		// daemon answers to the unknown `exclusive` field: version skew must be
		// loud, not a silently contended benchmark.
		if req.Exclusive {
			switch response.Code {
			case "E_ADMIT_EXCLUSIVE_ACTIVE", "U_ADMIT_EXCLUSIVE_UNESTABLISHED", "E_DAEMON_PROTOCOL", "E_DAEMON_BUSY":
				_ = conn.Close()
				return admissionResult{
					state:    "exclusive_unavailable",
					waitedMS: time.Since(admissionStarted).Milliseconds(),
					reserve:  effectiveReserve,
					basis:    "reject:exclusive-unavailable",
				}, true, exclusiveRefusal(response.Code, response.Error)
			}
		}
		if response.Code == "E_ADMIT_WAIT_TOO_LONG" {
			_ = conn.Close()
			message := strings.TrimSpace(response.Error)
			if message == "" {
				message = response.Code + ": requested admission wait exceeds the daemon ceiling"
			}
			return admissionResult{
				state:    "wait_too_long",
				waitedMS: time.Since(admissionStarted).Milliseconds(),
				reserve:  effectiveReserve,
				basis:    "reject:wait-too-long",
			}, true, errors.New(message)
		}
		if response.Code == "E_ADMIT_TOO_LARGE" || response.Code == "E_ADMIT_SATURATED" {
			var rejection runnerAdmitRejection
			if err := json.Unmarshal(response.Data, &rejection); err == nil && validRunnerAdmitRejection(response.Code, rejection) {
				_ = conn.Close()
				resolved := rejection.Required
				if resolved <= 0 {
					resolved = effectiveReserve
				}
				basis := "reject:saturated"
				message := response.Error
				if response.Code == "E_ADMIT_TOO_LARGE" {
					basis = "reject:too-large"
				} else {
					ceiling := "unknown"
					if rejection.Ceiling > 0 {
						ceiling = FormatConfineBytes(rejection.Ceiling)
					}
					// Honest wording: E_ADMIT_SATURATED means the wait window expired
					// without a grant — the slice was contended for the duration — not
					// that it is persistently "genuinely saturated" (a state the daemon
					// never establishes on this path).
					message = fmt.Sprintf("E_ADMIT_SATURATED: confine: admission rejected after %s — slice contended, no memory admission within the wait (reserve %s/%s)", time.Since(admissionStarted).Round(time.Second), FormatConfineBytes(resolved), ceiling)
				}
				if message == "" {
					message = response.Code + ": " + rejection.Basis
				}
				return admissionResult{state: strings.TrimPrefix(strings.ToLower(response.Code), "e_admit_"), waitedMS: time.Since(admissionStarted).Milliseconds(), reserve: resolved, ceiling: rejection.Ceiling, basis: basis}, true, errors.New(message)
			}
		}
		return fail()
	}
	var grant runnerAdmitGrant
	if err := json.Unmarshal(response.Data, &grant); err != nil || !validRunnerAdmitGrant(grant) {
		if req.Exclusive {
			_ = conn.Close()
			return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"}, true,
				exclusiveRefusal("", "the daemon's admission grant could not be read")
		}
		return fail()
	}
	// AIRA-101. `unevaluated` is a real grant state — the daemon answered, but
	// could not establish the slice's usage — and an ordinary job proceeds on it
	// uncapped-but-launched. An EXCLUSIVE job must not: "the daemon could not
	// evaluate the slice" is precisely the case where a claim of exclusivity would
	// be fabricated. Only a genuine immediate/waited grant is exclusivity.
	if req.Exclusive && grant.State != "immediate" && grant.State != "waited" {
		_ = conn.Close()
		return admissionResult{state: "exclusive_unavailable", basis: "reject:exclusive-unavailable"}, true,
			exclusiveRefusal("", "the daemon answered "+grant.State+" rather than granting exclusive admission")
	}
	// A full, validated frame claims the connection as the lease. Before returning
	// it, STOP the async closers so neither the ctx callback nor the detach monitor
	// can close the lease after we hand it back (Sol build r1 #3, r2 #3).
	// ctx-callback arbitration: stopClose() returns false iff the ctx callback has
	// already started (ctx is Done) — the closer won, so abort with the ctx error
	// rather than return a lease it is closing.
	if !stopClose() {
		_ = conn.Close()
		return admissionResult{}, false, ctx.Err()
	}
	// detach-monitor arbitration: stop + JOIN the monitor, then honour a kill-intent
	// that raced the grant (closer wins -> abort).
	stopMonitor()
	<-monitorDone
	select {
	case err := <-monitorErr:
		_ = conn.Close()
		return admissionResult{}, false, err
	default:
	}
	// A full, validated frame is the sole winning outcome even when its final
	// byte races the transport deadline. The flock fallback is never entered.
	return admissionResult{state: grant.State, reason: grant.Reason, waitedMS: grant.WaitedMS, release: conn, reserve: grant.Reserve, basis: grant.Basis, scopeCeiling: grant.ScopeCeiling}, true, nil
}

const mathMaxInt64 = int64(^uint64(0) >> 1)

func (r *Runner) checkDetachAdmission(req Request) error {
	if !req.Detach {
		return nil
	}
	current, err := r.ledger.current(req.detachRunID)
	if err != nil {
		return launchErr("U_RUN_RECONCILE_REQUIRED", err)
	}
	if current.Detached && current.KillIntent.Present {
		return errDetachKillIntent
	}
	return nil
}

func validRunnerAdmitGrant(grant runnerAdmitGrant) bool {
	if grant.WaitedMS < 0 || grant.Reserve <= 0 || strings.TrimSpace(grant.Basis) == "" {
		return false
	}
	switch grant.State {
	case "immediate", "waited", "unevaluated":
		return true
	default:
		return false
	}
}

func validRunnerAdmitRejection(code string, rejection runnerAdmitRejection) bool {
	switch code {
	case "E_ADMIT_TOO_LARGE":
		return rejection.Required > 0 && rejection.Ceiling >= 0 && strings.TrimSpace(rejection.Basis) != ""
	case "E_ADMIT_SATURATED":
		return rejection.Basis == "reject:saturated"
	default:
		return false
	}
}

func writeRunnerAdmitFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > runnerDaemonMaxFrameBytes {
		return errWorkerAdmitFrameSize
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeRunnerAdmitBytes(w, header[:]); err != nil {
		return err
	}
	return writeRunnerAdmitBytes(w, payload)
}

func writeRunnerAdmitBytes(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func reportConfinePeak(ctx context.Context, request ConfineRequest, signature string, peak *int64, oom bool) error {
	if strings.TrimSpace(request.AdmitSocketPath) == "" || signature == "" {
		return errors.New("daemon report unavailable")
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", request.AdmitSocketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "confine-report"
	frame.Request.Args = map[string]any{"signature": signature, "oom": oom}
	if peak != nil && *peak > 0 {
		frame.Request.Args["peak_rss"] = *peak
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		return err
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		return err
	}
	if !response.OK || response.Code != "OK" {
		if response.Error != "" {
			return errors.New(response.Error)
		}
		return errors.New("daemon rejected confine report")
	}
	return nil
}

func readRunnerAdmitFrame(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > runnerDaemonMaxFrameBytes {
		return errWorkerAdmitFrameSize
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}

func jitteredPoll(interval time.Duration, iteration uint64) time.Duration {
	// Deterministic +/-10% jitter avoids a shared random source and keeps fake
	// clock tests exact at the max-wait clamp.
	switch iteration % 3 {
	case 1:
		return interval + interval/10
	case 2:
		return interval - interval/10
	default:
		return interval
	}
}

func (r *Runner) noteAdmission(cur, max, effectiveReserve int64) {
	if r.diagnostics != nil {
		_, _ = fmt.Fprintf(r.diagnostics, "aira: memory admission waiting: current=%d max=%d reserve=%d\n", cur, max, effectiveReserve)
	}
}

func (r *Runner) warnAdmission(state, reason string) {
	if r.diagnostics == nil {
		return
	}
	if reason == "" {
		_, _ = fmt.Fprintf(r.diagnostics, "aira: warning: memory admission %s; launching without an admission lock\n", state)
		return
	}
	_, _ = fmt.Fprintf(r.diagnostics, "aira: warning: memory admission %s (%s); launching without an admission lock\n", state, reason)
}

func resolveSlicePath(slice string) (string, bool, string) {
	mount, err := unifiedMount()
	if err != nil {
		return "", false, "slice-not-found"
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	return resolveSlicePathAt(slice, mount, current)
}

// resolveSlicePathExact is the error-preserving counterpart used by confine's
// aira→whale default policy. The older resolver intentionally collapses every
// failure for admission callers; default confinement must distinguish definite
// ENOENT from permission and evaluation failures.
func resolveSlicePathExact(slice string) (string, error) {
	mount, err := unifiedMount()
	if err != nil {
		return "", err
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		return "", err
	}
	return resolveSlicePathAtExact(slice, mount, current)
}

func resolveSlicePathAtExact(slice, mount, current string) (string, error) {
	slice = strings.TrimSpace(slice)
	if slice == "" || hasParentComponent(slice) {
		return "", fs.ErrNotExist
	}
	mountAbs, err := filepath.Abs(mount)
	if err != nil {
		return "", err
	}
	mountCanonical, err := filepath.EvalSymlinks(mountAbs)
	if err != nil {
		return "", err
	}
	var candidates []string
	if filepath.IsAbs(slice) {
		candidates = []string{slice}
	} else if !strings.ContainsRune(slice, filepath.Separator) && strings.HasSuffix(slice, ".slice") {
		for cursor := filepath.Clean(current); pathWithin(mountCanonical, cursor); cursor = filepath.Dir(cursor) {
			if filepath.Base(cursor) == slice {
				candidates = append(candidates, cursor)
			}
			candidates = append(candidates, filepath.Join(cursor, slice))
			if filepath.Clean(cursor) == filepath.Clean(mountCanonical) {
				break
			}
		}
	} else {
		candidates = []string{filepath.Join(mountCanonical, slice)}
	}
	var firstFailure error
	for _, candidate := range candidates {
		candidateAbs, absErr := filepath.Abs(candidate)
		if absErr != nil {
			if firstFailure == nil {
				firstFailure = absErr
			}
			continue
		}
		if !pathWithin(mountCanonical, candidateAbs) {
			continue
		}
		canonical, evalErr := filepath.EvalSymlinks(candidateAbs)
		if evalErr != nil {
			if !errors.Is(evalErr, fs.ErrNotExist) && firstFailure == nil {
				firstFailure = evalErr
			}
			continue
		}
		if !pathWithin(mountCanonical, canonical) {
			continue
		}
		info, statErr := os.Stat(canonical)
		if statErr == nil && info.IsDir() {
			return canonical, nil
		}
		if statErr == nil {
			statErr = fmt.Errorf("%s is not a directory", canonical)
		}
		if !errors.Is(statErr, fs.ErrNotExist) && firstFailure == nil {
			firstFailure = statErr
		}
	}
	if firstFailure != nil {
		return "", firstFailure
	}
	return "", fs.ErrNotExist
}

func resolveSlicePathAt(slice, mount, current string) (string, bool, string) {
	slice = strings.TrimSpace(slice)
	if slice == "" || hasParentComponent(slice) {
		return "", false, "slice-not-found"
	}
	mountAbs, err := filepath.Abs(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	mountCanonical, err := filepath.EvalSymlinks(mountAbs)
	if err != nil {
		return "", false, "slice-not-found"
	}
	var candidates []string
	if filepath.IsAbs(slice) {
		candidates = []string{slice}
	} else if !strings.ContainsRune(slice, filepath.Separator) && strings.HasSuffix(slice, ".slice") {
		for cursor := filepath.Clean(current); pathWithin(mountCanonical, cursor); cursor = filepath.Dir(cursor) {
			if filepath.Base(cursor) == slice {
				candidates = append(candidates, cursor)
			}
			candidates = append(candidates, filepath.Join(cursor, slice))
			if filepath.Clean(cursor) == filepath.Clean(mountCanonical) {
				break
			}
		}
	} else {
		candidates = []string{filepath.Join(mountCanonical, slice)}
	}
	for _, candidate := range candidates {
		candidateAbs, absErr := filepath.Abs(candidate)
		if absErr != nil || !pathWithin(mountCanonical, candidateAbs) {
			continue
		}
		canonical, evalErr := filepath.EvalSymlinks(candidateAbs)
		if evalErr != nil || !pathWithin(mountCanonical, canonical) {
			continue
		}
		st, statErr := os.Stat(canonical)
		if statErr == nil && st.IsDir() {
			return canonical, true, ""
		}
	}
	return "", false, "slice-not-found"
}

func hasParentComponent(path string) bool {
	for _, component := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readSliceMemory(path string) (cur, max int64, ok bool, reason string) {
	currentData, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, 0, false, "read-error"
	}
	maxData, err := os.ReadFile(filepath.Join(path, "memory.max"))
	if err != nil {
		return 0, 0, false, "read-error"
	}
	current, valid := parseAdmissionMemory(currentData)
	if !valid {
		return 0, 0, false, "parse-error"
	}
	maxText := strings.TrimSpace(string(maxData))
	if maxText == "max" {
		return 0, 0, false, "unbounded"
	}
	limit, valid := parseAdmissionMemory(maxData)
	if !valid {
		return 0, 0, false, "parse-error"
	}
	return current, limit, true, ""
}

func parseAdmissionMemory(data []byte) (int64, bool) {
	text := strings.TrimSpace(string(data))
	if text == "" || len(strings.Fields(text)) != 1 {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil && value >= 0
}

func tryAdmissionLock(canonicalPath string) (*admitLock, error) {
	dir, err := admissionLockDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(canonicalPath))
	path := filepath.Join(dir, fmt.Sprintf("%x", digest[:]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &admitLock{file: f}, nil
}

func admissionLockDir() (string, error) {
	uid := os.Geteuid()
	runtimeDir := filepath.Join("/run/user", strconv.Itoa(uid))
	if st, err := os.Lstat(runtimeDir); err == nil && st.IsDir() && st.Mode().Perm()&0o022 == 0 && st.Mode().Perm()&0o300 == 0o300 && runtimeDirOwnedByUser(st, uid) {
		return filepath.Join(runtimeDir, "aira-admission"), nil
	}
	account, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || account.HomeDir == "" || !filepath.IsAbs(account.HomeDir) {
		return "", errors.New("user cache directory unavailable")
	}
	return filepath.Join(filepath.Clean(account.HomeDir), ".cache", "aira", "admission"), nil
}

func runtimeDirOwnedByUser(st os.FileInfo, uid int) bool {
	stat, ok := st.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(uid)
}
