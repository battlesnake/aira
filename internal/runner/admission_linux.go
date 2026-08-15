//go:build linux

package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

func (r *Runner) admit(ctx context.Context, req Request) (admissionResult, error) {
	if req.NoAdmit {
		return admissionResult{state: "bypassed"}, nil
	}
	if r.memorySlice == "" || r.memoryReserve == 0 {
		return admissionResult{state: "disabled"}, nil
	}
	path, ok, reason := resolveSlicePath(r.memorySlice)
	if !ok {
		r.warnAdmission("unevaluated", reason)
		return admissionResult{state: "unevaluated", reason: reason}, nil
	}
	start := r.clock.Now()
	waited := false
	lastNote := start.Add(-time.Hour)
	iteration := uint64(0)
	finish := func(state, reason string, lock *admitLock) admissionResult {
		result := admissionResult{state: state, reason: reason, lock: lock}
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
		if max-cur >= r.memoryReserve {
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
				if max2-cur2 >= r.memoryReserve {
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
			r.noteAdmission(cur, max)
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

func (r *Runner) noteAdmission(cur, max int64) {
	if r.diagnostics != nil {
		_, _ = fmt.Fprintf(r.diagnostics, "aira: memory admission waiting: current=%d max=%d reserve=%d\n", cur, max, r.memoryReserve)
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
