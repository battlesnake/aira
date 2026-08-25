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
	state    string
	reason   string
	waitedMS int64
	lock     *admitLock
	release  io.Closer
	reserve  int64
	basis    string
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

const (
	runnerDaemonProtocolVersion = 5
	runnerDaemonMaxFrameBytes   = 16 << 20
	admitTransportGrace         = time.Second
	runnerAdmitWaitCap          = 30 * time.Minute
)

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
	OK    bool            `json:"ok"`
	Code  string          `json:"code"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

type runnerAdmitGrant struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	WaitedMS int64  `json:"waited_ms"`
	Reserve  int64  `json:"reserve"`
	Basis    string `json:"basis"`
}

type runnerAdmitRejection struct {
	Required int64  `json:"required,omitempty"`
	Ceiling  int64  `json:"cap_minus_headroom,omitempty"`
	Basis    string `json:"basis"`
}

func (r *Runner) admit(ctx context.Context, req Request) (admissionResult, error) {
	if req.NoAdmit {
		return admissionResult{state: "bypassed"}, nil
	}
	effectiveReserve := r.memoryReserve
	if req.MemoryReserveOverride != nil && *req.MemoryReserveOverride > 0 {
		effectiveReserve = *req.MemoryReserveOverride
	}
	if r.memorySlice == "" || r.memoryReserve == 0 {
		return admissionResult{state: "disabled"}, nil
	}
	start := r.clock.Now()
	if result, granted, err := r.admitThroughDaemon(ctx, req, effectiveReserve); granted || err != nil {
		return result, err
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
	maxWait := r.admissionMaxWait
	if maxWait > runnerAdmitWaitCap {
		maxWait = runnerAdmitWaitCap
	}
	// The client transport deadline is derived from the CAPPED maxWait (Sol build
	// r1 #4): a wedged daemon must not strand the client past the advertised cap.
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
		return admissionResult{}, false, nil
	}

	_ = conn.SetDeadline(transportDeadline)
	// The connection deadline bounds the transport. Only caller cancellation
	// closes asynchronously: a full frame that completes exactly at the
	// transport deadline must win and keep this lease open through Start.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	frame := runnerAdmitRequestFrame{Proto: runnerDaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "admit"
	frame.Request.Args = map[string]any{
		"slice": r.memorySlice, "reserve": effectiveReserve,
		"max_wait_ms": maxWait.Milliseconds(),
		"signature":   req.ResourceSignature,
		"pinned":      !req.DaemonEstimateMemory || req.MemoryReservePinned,
	}
	if req.ConfineScopeID != "" {
		frame.Request.Args["scope_id"] = req.ConfineScopeID
		frame.Request.Args["name"] = req.ConfineName
		frame.Request.Args["owner"] = req.ConfineOwner
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		return fail()
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		return fail()
	}
	if !response.OK || response.Code != "OK" {
		if response.Code == "E_ADMIT_TOO_LARGE" || response.Code == "E_ADMIT_SATURATED" {
			var rejection runnerAdmitRejection
			if err := json.Unmarshal(response.Data, &rejection); err == nil && validRunnerAdmitRejection(response.Code, rejection) {
				_ = conn.Close()
				message := response.Error
				if message == "" {
					message = response.Code + ": " + rejection.Basis
				}
				resolved := rejection.Required
				if resolved <= 0 {
					resolved = effectiveReserve
				}
				basis := "reject:saturated"
				if response.Code == "E_ADMIT_TOO_LARGE" {
					basis = "reject:too-large"
				}
				return admissionResult{state: strings.TrimPrefix(strings.ToLower(response.Code), "e_admit_"), reserve: resolved, basis: basis}, true, errors.New(message)
			}
		}
		return fail()
	}
	var grant runnerAdmitGrant
	if err := json.Unmarshal(response.Data, &grant); err != nil || !validRunnerAdmitGrant(grant) {
		return fail()
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
	return admissionResult{state: grant.State, reason: grant.Reason, waitedMS: grant.WaitedMS, release: conn, reserve: grant.Reserve, basis: grant.Basis}, true, nil
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
		return errors.New("invalid daemon admission frame size")
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
	frame := runnerAdmitRequestFrame{Proto: runnerDaemonProtocolVersion, Scope: map[string]any{}}
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
		return errors.New("invalid daemon admission frame size")
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
