//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type chunkReader struct{ reader io.Reader }

func (r chunkReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.reader.Read(p)
}

type deadlineReadError struct{}

func (deadlineReadError) Error() string   { return "deadline" }
func (deadlineReadError) Timeout() bool   { return true }
func (deadlineReadError) Temporary() bool { return true }

type deadlineFrameConn struct {
	mu     sync.Mutex
	reader *bytes.Reader
	closed atomic.Bool
}

func (c *deadlineFrameConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.reader.Read(p)
	if n > 0 && c.reader.Len() == 0 {
		return n, deadlineReadError{}
	}
	return n, err
}
func (c *deadlineFrameConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *deadlineFrameConn) Close() error                { c.closed.Store(true); return nil }
func (c *deadlineFrameConn) LocalAddr() net.Addr         { return testNetAddr("local") }
func (c *deadlineFrameConn) RemoteAddr() net.Addr        { return testNetAddr("remote") }
func (c *deadlineFrameConn) SetDeadline(time.Time) error { return nil }
func (c *deadlineFrameConn) SetReadDeadline(time.Time) error {
	return nil
}
func (c *deadlineFrameConn) SetWriteDeadline(time.Time) error {
	return nil
}

type testNetAddr string

func (a testNetAddr) Network() string { return "test" }
func (a testNetAddr) String() string  { return string(a) }

type instantClock struct {
	mu  sync.Mutex
	now time.Time
}

func newInstantClock() *instantClock { return &instantClock{now: time.Unix(100, 0)} }
func (c *instantClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *instantClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

type pacedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newPacedClock() *pacedClock { return &pacedClock{now: time.Unix(200, 0)} }
func (c *pacedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *pacedClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	time.AfterFunc(time.Millisecond, func() {
		c.mu.Lock()
		c.now = c.now.Add(d)
		now := c.now
		c.mu.Unlock()
		ch <- now
	})
	return ch
}

func currentSliceForTest(t *testing.T) string {
	t.Helper()
	mount, err := unifiedMount()
	if err != nil {
		t.Fatal(err)
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		t.Fatal(err)
	}
	path, ok, reason := resolveSlicePathAt(current, mount, current)
	if !ok {
		t.Fatalf("resolve current slice: %s", reason)
	}
	return path
}

func secureRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func gateOnlyRunner(t *testing.T, clock Clock, fn func(string) (int64, int64, bool, string)) (*Runner, string) {
	t.Helper()
	path := currentSliceForTest(t)
	return &Runner{memorySlice: path, memoryReserve: 40, admissionMaxWait: 100 * time.Millisecond, pollInterval: 10 * time.Millisecond, clock: clock, sliceMemory: fn}, path
}

func TestReadSliceMemoryTable(t *testing.T) {
	for _, tc := range []struct {
		name, current, max, reason string
		ok                         bool
		wantCurrent, wantMax       int64
		missing                    string
	}{
		{name: "values", current: "25\n", max: "100\n", ok: true, wantCurrent: 25, wantMax: 100},
		{name: "unbounded", current: "25", max: "max", reason: "unbounded"},
		{name: "empty current", current: "", max: "100", reason: "parse-error"},
		{name: "negative current", current: "-1", max: "100", reason: "parse-error"},
		{name: "nonnumeric max", current: "1", max: "many", reason: "parse-error"},
		{name: "overflow", current: "9223372036854775808", max: "100", reason: "parse-error"},
		{name: "missing current", max: "100", missing: "memory.current", reason: "read-error"},
		{name: "missing max", current: "1", missing: "memory.max", reason: "read-error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.missing != "memory.current" {
				if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte(tc.current), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missing != "memory.max" {
				if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(tc.max), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cur, max, ok, reason := readSliceMemory(dir)
			if ok != tc.ok || reason != tc.reason || (ok && (cur != tc.wantCurrent || max != tc.wantMax)) {
				t.Fatalf("read=(%d,%d,%v,%q)", cur, max, ok, reason)
			}
		})
	}

	t.Run("permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permission bits")
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("1"), 0); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("2"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok, reason := readSliceMemory(dir); ok || reason != "read-error" {
			t.Fatalf("ok=%v reason=%q", ok, reason)
		}
	})
}

func TestResolveSlicePathTable(t *testing.T) {
	mount := t.TempDir()
	ancestor := filepath.Join(mount, "user.slice", "whale.slice")
	current := filepath.Join(ancestor, "session.scope")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join(mount, "machine.slice")
	if err := os.Mkdir(relative, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(mount, "escape.slice")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value, want string
		ok                bool
	}{
		{name: "absolute", value: relative, want: relative, ok: true},
		{name: "mount relative", value: "machine.slice", want: relative, ok: true},
		{name: "bare ancestor", value: "whale.slice", want: ancestor, ok: true},
		{name: "nonexistent", value: "missing.slice"},
		{name: "dotdot escape", value: "../outside"},
		{name: "symlink escape", value: "escape.slice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, reason := resolveSlicePathAt(tc.value, mount, current)
			if ok != tc.ok || (!ok && reason != "slice-not-found") {
				t.Fatalf("resolve=(%q,%v,%q)", got, ok, reason)
			}
			if ok && filepath.Clean(got) != filepath.Clean(tc.want) {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAdmissionT1FailOpenReasonsAndDisabled(t *testing.T) {
	if result, err := (&Runner{}).admit(context.Background(), Request{}); err != nil || result.state != "disabled" {
		t.Fatalf("disabled result=%+v err=%v", result, err)
	}
	for _, reason := range []string{"slice-not-found", "read-error", "unbounded", "parse-error"} {
		t.Run(reason, func(t *testing.T) {
			var diagnostics bytes.Buffer
			r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 0, false, reason })
			r.diagnostics = &diagnostics
			result, err := r.admit(context.Background(), Request{})
			if err != nil || result.state != "unevaluated" || result.reason != reason || !strings.Contains(diagnostics.String(), "warning") {
				t.Fatalf("result=%+v err=%v diagnostics=%q", result, err, diagnostics.String())
			}
		})
	}
}

func TestRunnerNewDefaultsAndValidatesAdmissionTiming(t *testing.T) {
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.admissionMaxWait != 30*time.Minute || r.pollInterval != 2*time.Second {
		t.Fatalf("maxWait=%s poll=%s", r.admissionMaxWait, r.pollInterval)
	}
	for _, cfg := range []Config{
		{CommonDir: t.TempDir(), AdmissionMaxWait: -time.Second},
		{CommonDir: t.TempDir(), PollInterval: -time.Second},
	} {
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
			t.Fatalf("New(%+v) err=%v", cfg, err)
		}
	}
}

type countingBackend struct {
	scope   *memoryScope
	creates atomic.Int64
}

func (b *countingBackend) Probe(context.Context) error { return nil }
func (b *countingBackend) Create(context.Context, string) (Scope, error) {
	b.creates.Add(1)
	return b.scope, nil
}
func (b *countingBackend) Open(context.Context, string) (Scope, error) { return b.scope, nil }

func TestAdmissionT2NoLaunchSideEffectsDuringWait(t *testing.T) {
	path := currentSliceForTest(t)
	clock := newPacedClock()
	var relieved atomic.Bool
	firstRead := make(chan struct{})
	var once sync.Once
	backend := &countingBackend{scope: &memoryScope{}}
	base := t.TempDir()
	r, err := New(Config{CommonDir: base, Backend: backend, MemorySlice: path, MemoryReserve: 40, AdmissionMaxWait: time.Second, PollInterval: 10 * time.Millisecond, Clock: clock,
		sliceMemoryFn: func(string) (int64, int64, bool, string) {
			once.Do(func() { close(firstRead) })
			if relieved.Load() {
				return 0, 100, true, ""
			}
			return 90, 100, true, ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	r.startFn = func(*exec.Cmd) error { return errors.New("injected start failure") }
	done := make(chan error, 1)
	go func() {
		_, launchErr := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
		done <- launchErr
	}()
	<-firstRead
	if _, err := os.Stat(r.ledger.counter); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("counter exists during wait: %v", err)
	}
	if events, err := r.ledger.read(); err != nil || len(events) != 0 {
		t.Fatalf("ledger during wait=%d err=%v", len(events), err)
	}
	entries, err := os.ReadDir(r.outputDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outputs during wait=%d err=%v", len(entries), err)
	}
	if backend.creates.Load() != 0 {
		t.Fatalf("scope creates during wait=%d", backend.creates.Load())
	}
	relieved.Store(true)
	if err := <-done; err == nil {
		t.Fatal("injected post-admission failure was not reached")
	}
	events, err := r.ledger.read()
	if err != nil || len(events) == 0 {
		t.Fatalf("post-wait ledger=%d err=%v", len(events), err)
	}
	last := events[len(events)-1].Run
	if last.Admission != "waited" || last.AdmissionWaitedMS <= 0 || backend.creates.Load() != 1 {
		t.Fatalf("record=%+v creates=%d", last, backend.creates.Load())
	}
}

func TestAdmissionT3CancelBeforeFirstReadHasNoSideEffects(t *testing.T) {
	path := currentSliceForTest(t)
	var reads atomic.Int64
	backend := &countingBackend{scope: &memoryScope{}}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: backend, MemorySlice: path, MemoryReserve: 40, Clock: newInstantClock(), sliceMemoryFn: func(string) (int64, int64, bool, string) {
		reads.Add(1)
		return 90, 100, true, ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Launch(ctx, Request{Argv: []string{"/bin/true"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if reads.Load() != 0 || backend.creates.Load() != 0 {
		t.Fatalf("reads=%d creates=%d", reads.Load(), backend.creates.Load())
	}
	if events, err := r.ledger.read(); err != nil || len(events) != 0 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
}

func TestAdmissionT4ExactFakeClockTimeout(t *testing.T) {
	clock := newInstantClock()
	start := clock.Now()
	r, _ := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 90, 100, true, "" })
	r.admissionMaxWait = 25 * time.Millisecond
	result, err := r.admit(context.Background(), Request{})
	if err != nil || result.state != "timeout" || clock.Now().Sub(start) != 25*time.Millisecond || result.waitedMS != 25 || result.lock != nil {
		t.Fatalf("result=%+v elapsed=%s err=%v", result, clock.Now().Sub(start), err)
	}
}

func TestAdmissionPersistentEINTRUsesBoundedOuterLoop(t *testing.T) {
	clock := newInstantClock()
	start := clock.Now()
	var attempts atomic.Int64
	r, _ := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	r.admissionMaxWait = 25 * time.Millisecond
	r.lockAttemptFn = func(string) (*admitLock, error) {
		attempts.Add(1)
		return nil, unix.EINTR
	}
	type outcome struct {
		result admissionResult
		err    error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		result, err := r.admit(ctx, Request{})
		done <- outcome{result: result, err: err}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(time.Second):
		cancel()
		got = <-done
		t.Fatalf("persistent EINTR did not reach the admission deadline: err=%v attempts=%d", got.err, attempts.Load())
	}
	result, err := got.result, got.err
	if err != nil || result.state != "timeout" || result.reason != "" || result.lock != nil || result.waitedMS != 25 || clock.Now().Sub(start) != 25*time.Millisecond {
		t.Fatalf("result=%+v elapsed=%s attempts=%d err=%v", result, clock.Now().Sub(start), attempts.Load(), err)
	}
	if attempts.Load() < 2 || attempts.Load() > 10 {
		t.Fatalf("persistent EINTR attempts=%d, want bounded polling", attempts.Load())
	}
}

func TestAdmissionT5RealFlockSerializesWaiters(t *testing.T) {
	clock := newPacedClock()
	r, _ := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	r.admissionMaxWait = time.Second
	first, err := r.admit(context.Background(), Request{})
	if err != nil || first.lock == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	done := make(chan admissionResult, 1)
	go func() { result, _ := r.admit(context.Background(), Request{}); done <- result }()
	select {
	case result := <-done:
		first.lock.release()
		t.Fatalf("second bypassed held flock: %+v", result)
	case <-time.After(10 * time.Millisecond):
	}
	first.lock.release()
	select {
	case second := <-done:
		if second.lock == nil || second.state != "waited" {
			t.Fatalf("second=%+v", second)
		}
		second.lock.release()
	case <-time.After(time.Second):
		t.Fatal("second did not acquire after release")
	}
}

func TestAdmissionT5LockHeldThroughStartAndSerializedRecheck(t *testing.T) {
	parent := isolatedScopeParent(t)
	path := currentSliceForTest(t)
	clock := newPacedClock()
	var room atomic.Bool
	room.Store(true)
	pressureObserved := make(chan struct{})
	var pressureOnce sync.Once
	memory := func(string) (int64, int64, bool, string) {
		if room.Load() {
			return 0, 100, true, ""
		}
		pressureOnce.Do(func() { close(pressureObserved) })
		return 90, 100, true, ""
	}
	r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: parent, MemorySlice: path, MemoryReserve: 40, AdmissionMaxWait: 2 * time.Second, PollInterval: 10 * time.Millisecond, Clock: clock, sliceMemoryFn: memory, Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		skipOrFailRealCgroup(t, "real cgroup unavailable for lock-through-Start test: %v", err)
	}
	startEntered := make(chan struct{})
	allowFirstStart := make(chan struct{})
	var allowOnce sync.Once
	allowFirst := func() { allowOnce.Do(func() { close(allowFirstStart) }) }
	defer allowFirst()
	firstStarted := make(chan error, 1)
	secondStarted := make(chan struct{})
	var starts atomic.Int64
	r.startFn = func(cmd *exec.Cmd) error {
		switch starts.Add(1) {
		case 1:
			close(startEntered)
			<-allowFirstStart
			err := cmd.Start()
			if err == nil {
				room.Store(false)
			}
			firstStarted <- err
			return err
		case 2:
			close(secondStarted)
			return errors.New("second start reached")
		default:
			return errors.New("unexpected extra start")
		}
	}
	firstDone := make(chan launchOutcome, 1)
	go func() {
		record, launchErr := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "sleep 0.05"}})
		firstDone <- launchOutcome{record: record, err: launchErr}
	}()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("first Launch did not reach blocked Start")
	}
	secondDone := make(chan launchOutcome, 1)
	go func() {
		record, launchErr := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
		secondDone <- launchOutcome{record: record, err: launchErr}
	}()
	select {
	case <-secondStarted:
		allowFirst()
		t.Fatal("second Launch reached Start while first Start still held the admission window")
	case <-time.After(20 * time.Millisecond):
	}
	allowFirst()
	if err := <-firstStarted; err != nil {
		t.Fatalf("first real Start failed: %v", err)
	}
	select {
	case <-pressureObserved:
	case <-time.After(time.Second):
		t.Fatal("second Launch did not re-read first launch's memory effect")
	}
	select {
	case <-secondStarted:
		t.Fatal("second Launch started while serialized recheck still reported pressure")
	default:
	}
	room.Store(true)
	select {
	case second := <-secondDone:
		if second.err == nil {
			t.Fatal("injected second Start failure was not reached after relief")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Launch did not proceed after relief")
	}
	select {
	case first := <-firstDone:
		if first.err != nil || first.record == nil || first.record.Status != StatusExited {
			t.Fatalf("first launch=%+v", first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Launch did not finish")
	}
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	var secondRecord RunRecord
	for _, event := range events {
		if event.Run.ID == "RUN-2" {
			secondRecord = event.Run
		}
	}
	if secondRecord.Admission != "waited" || secondRecord.AdmissionWaitedMS <= 0 {
		t.Fatalf("second admission=%+v", secondRecord)
	}
}

func TestAdmissionLockRootIsSharedAcrossXDGEnvironments(t *testing.T) {
	firstEnv, secondEnv := secureRuntimeDir(t), secureRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", firstEnv)
	firstDir, err := admissionLockDir()
	if err != nil {
		t.Fatal(err)
	}
	r1, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	first, err := r1.admit(context.Background(), Request{})
	if err != nil || first.lock == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	defer first.lock.release()
	if err := os.Setenv("XDG_RUNTIME_DIR", secondEnv); err != nil {
		t.Fatal(err)
	}
	secondDir, err := admissionLockDir()
	if err != nil {
		t.Fatal(err)
	}
	if firstDir != secondDir {
		t.Fatalf("lock dirs differ across environments: %q != %q", firstDir, secondDir)
	}
	clock := newInstantClock()
	r2, _ := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	r2.admissionMaxWait = 20 * time.Millisecond
	var contended atomic.Bool
	r2.lockAttemptFn = func(path string) (*admitLock, error) {
		lock, lockErr := tryAdmissionLock(path)
		if errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN) {
			contended.Store(true)
		}
		return lock, lockErr
	}
	second, err := r2.admit(context.Background(), Request{})
	if err != nil || second.state != "timeout" || second.lock != nil || !contended.Load() {
		t.Fatalf("second=%+v contended=%v err=%v", second, contended.Load(), err)
	}
}

func TestAdmissionT6NoAdmitBypassesPressure(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { panic("must not read") })
	result, err := r.admit(context.Background(), Request{NoAdmit: true})
	if err != nil || result.state != "bypassed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAdmissionT7LiveLockHolderTimesOutUnlocked(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", secureRuntimeDir(t))
	path := currentSliceForTest(t)
	memory := func(string) (int64, int64, bool, string) { return 0, 100, true, "" }
	first, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, MemorySlice: path, MemoryReserve: 40, AdmissionMaxWait: time.Second, PollInterval: 10 * time.Millisecond, Clock: newInstantClock(), sliceMemoryFn: memory})
	if err != nil {
		t.Fatal(err)
	}
	prepEntered := make(chan struct{})
	releasePrep := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(releasePrep) }) }
	defer releaseFirst()
	first.reserveIDFn = func() (string, error) {
		close(prepEntered)
		<-releasePrep
		return "", errors.New("release first prep")
	}
	firstDone := make(chan error, 1)
	go func() {
		_, launchErr := first.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
		firstDone <- launchErr
	}()
	<-prepEntered

	clock := newInstantClock()
	var diagnostics bytes.Buffer
	second, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, MemorySlice: path, MemoryReserve: 40, AdmissionMaxWait: 25 * time.Millisecond, PollInterval: 10 * time.Millisecond, Clock: clock, sliceMemoryFn: memory, Diagnostics: &diagnostics})
	if err != nil {
		t.Fatal(err)
	}
	second.startFn = func(*exec.Cmd) error { return errors.New("second reached start") }
	secondDone := make(chan error, 1)
	go func() {
		_, launchErr := second.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
		secondDone <- launchErr
	}()
	select {
	case err := <-secondDone:
		if err == nil {
			t.Fatal("injected second start failure was not reached")
		}
	case <-time.After(time.Second):
		t.Fatal("second Launch blocked behind live flock past its deadline")
	}
	events, err := second.ledger.read()
	if err != nil || len(events) == 0 {
		t.Fatalf("second events=%d err=%v", len(events), err)
	}
	last := events[len(events)-1].Run
	if last.Admission != "timeout" || last.AdmissionWaitedMS != 25 || !strings.Contains(diagnostics.String(), "timeout") {
		t.Fatalf("second record=%+v diagnostics=%q", last, diagnostics.String())
	}
	releaseFirst()
	if err := <-firstDone; err == nil {
		t.Fatal("first injected prep failure was not reached")
	}
}

func TestAdmissionT8CancelDuringFlockContention(t *testing.T) {
	clock := newPacedClock()
	r, path := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	holder, err := tryAdmissionLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, admitErr := r.admit(ctx, Request{}); done <- admitErr }()
	time.Sleep(3 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("contention wait ignored cancellation")
	}
}

func TestAdmissionT9LockDirectoryFailureFailsOpenLoudly(t *testing.T) {
	var diagnostics bytes.Buffer
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	r.diagnostics = &diagnostics
	r.lockAttemptFn = func(string) (*admitLock, error) { return nil, os.ErrPermission }
	result, err := r.admit(context.Background(), Request{})
	if err != nil || result.state != "unevaluated" || result.reason != "lock-error" || !strings.Contains(diagnostics.String(), "warning") {
		t.Fatalf("result=%+v err=%v diagnostics=%q", result, err, diagnostics.String())
	}
}

type createFaultBackend struct{ scope *memoryScope }

func (b *createFaultBackend) Probe(context.Context) error { return nil }
func (b *createFaultBackend) Create(context.Context, string) (Scope, error) {
	return nil, errors.New("create fault")
}
func (b *createFaultBackend) Open(context.Context, string) (Scope, error) { return b.scope, nil }

func TestAdmissionT10EveryPostAdmissionFailureReleasesLock(t *testing.T) {
	path := currentSliceForTest(t)
	for _, name := range []string{"reserve-id", "starting-append", "output", "scope-create", "scope-append", "stdin", "pipes", "start"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", secureRuntimeDir(t))
			scope := &memoryScope{}
			var backend ScopeBackend = &memoryBackend{scope: scope}
			if name == "scope-create" {
				backend = &createFaultBackend{scope: scope}
			}
			r, err := New(Config{CommonDir: t.TempDir(), Backend: backend, MemorySlice: path, MemoryReserve: 40, Clock: newInstantClock(), sliceMemoryFn: func(string) (int64, int64, bool, string) { return 0, 100, true, "" }})
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "reserve-id":
				r.reserveIDFn = func() (string, error) { return "", errors.New("reserve fault") }
			case "starting-append":
				r.appendFault = func(event ledgerEvent) error {
					if event.Kind == "starting" {
						return errors.New("starting fault")
					}
					return nil
				}
			case "output":
				r.openOutputsFn = func(string, string, bool) (map[string]string, map[string]*os.File, error) {
					return nil, nil, errors.New("output fault")
				}
			case "scope-append":
				r.appendFault = func(event ledgerEvent) error {
					if event.Kind == "scope-created" {
						return errors.New("scope append fault")
					}
					return nil
				}
			case "stdin":
				r.setupStdinFn = func(*exec.Cmd, Request, string) (func(), bool, error) { return nil, false, errors.New("stdin fault") }
			case "pipes":
				r.setupPipesFn = func(*exec.Cmd, bool) (map[string]*os.File, map[string]*os.File, error) {
					return nil, nil, errors.New("pipe fault")
				}
			case "start":
				r.startFn = func(*exec.Cmd) error { return errors.New("start fault") }
			}
			if _, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}}); err == nil {
				t.Fatal("injected failure returned nil")
			}
			lock, err := tryAdmissionLock(path)
			if err != nil {
				t.Fatalf("admission lock leaked after %s: %v", name, err)
			}
			lock.release()
		})
	}
}

func TestAdmissionT10ReleasePrecedesFailBeforeLaunchArbitration(t *testing.T) {
	path := currentSliceForTest(t)
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, MemorySlice: path, MemoryReserve: 40, Clock: newInstantClock(), sliceMemoryFn: func(string) (int64, int64, bool, string) { return 0, 100, true, "" }})
	if err != nil {
		t.Fatal(err)
	}
	r.openOutputsFn = func(string, string, bool) (map[string]string, map[string]*os.File, error) {
		return nil, nil, errors.New("output fault")
	}
	arbitrationEntered := make(chan struct{})
	unblockArbitration := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(unblockArbitration) }) }
	defer unblock()
	r.failBeforeLaunchFn = func(ctx context.Context, record RunRecord, code string, cause error) (*RunRecord, error) {
		close(arbitrationEntered)
		<-unblockArbitration
		return r.failBeforeLaunch(ctx, record, code, cause)
	}
	launchDone := make(chan error, 1)
	go func() {
		_, launchErr := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
		launchDone <- launchErr
	}()
	select {
	case <-arbitrationEntered:
	case <-time.After(time.Second):
		t.Fatal("failure did not enter blocked terminal arbitration")
	}
	competitor, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	result, err := competitor.admit(context.Background(), Request{})
	if err != nil || result.state != "immediate" || result.lock == nil {
		t.Fatalf("competitor could not acquire before arbitration completed: result=%+v err=%v", result, err)
	}
	result.lock.release()
	unblock()
	if err := <-launchDone; err == nil {
		t.Fatal("injected launch failure returned nil")
	}
}

func TestAdmissionT11KillAndReconcileDoNotTakeAdmissionLock(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", secureRuntimeDir(t))
	path := currentSliceForTest(t)
	r, scope := newMemoryRunner(t, nil)
	r.memorySlice, r.memoryReserve = path, 40
	r.clock, r.sliceMemory = newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 100, true, "" }
	r.admissionMaxWait, r.pollInterval = time.Second, 10*time.Millisecond
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, ScopeIntegrity: ScopeHandoffUnverified, CgroupScope: scope.Reference(), Admission: "immediate"}
	appendRunEvent(t, r, "starting", run)
	appendRunEvent(t, r, "scope-created", run)
	holder, err := tryAdmissionLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.release()
	done := make(chan struct{}, 2)
	go func() { _, _ = r.Kill(context.Background(), run.ID, false); done <- struct{}{} }()
	go func() { _, _ = r.Reconcile(context.Background()); done <- struct{}{} }()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Kill/Reconcile blocked on the admission lock")
		}
	}
}

func TestAdmissionT12LockedRecheckFailureUsesExactReason(t *testing.T) {
	for _, reason := range []string{"read-error", "unbounded", "parse-error"} {
		t.Run(reason, func(t *testing.T) {
			var calls atomic.Int64
			var diagnostics bytes.Buffer
			r, path := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) {
				if calls.Add(1) == 1 {
					return 0, 100, true, ""
				}
				return 0, 0, false, reason
			})
			r.diagnostics = &diagnostics
			result, err := r.admit(context.Background(), Request{})
			if err != nil || result.state != "unevaluated" || result.reason != reason || result.waitedMS != 0 || !strings.Contains(diagnostics.String(), "warning") {
				t.Fatalf("result=%+v err=%v diagnostics=%q", result, err, diagnostics.String())
			}
			lock, err := tryAdmissionLock(path)
			if err != nil {
				t.Fatalf("locked recheck leaked flock: %v", err)
			}
			lock.release()
		})
	}
}

func TestAdmissionT13WaitedMSOnLateErrorAndEAGAINContention(t *testing.T) {
	clock := newInstantClock()
	var calls atomic.Int64
	r, _ := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) {
		if calls.Add(1) <= 2 {
			return 90, 100, true, ""
		}
		return 0, 0, false, "read-error"
	})
	result, err := r.admit(context.Background(), Request{})
	if err != nil || result.state != "unevaluated" || result.reason != "read-error" || result.waitedMS <= 0 {
		t.Fatalf("late error result=%+v err=%v", result, err)
	}

	clock = newInstantClock()
	r, path := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
	r.admissionMaxWait = 20 * time.Millisecond
	holder, err := tryAdmissionLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.release()
	result, err = r.admit(context.Background(), Request{})
	if err != nil || result.state != "timeout" || result.reason == "lock-error" || result.waitedMS <= 0 {
		t.Fatalf("contention result=%+v err=%v", result, err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestAdmissionDiagnosticsErrorsAreIgnored(t *testing.T) {
	r, _ := gateOnlyRunner(t, newInstantClock(), func(string) (int64, int64, bool, string) { return 0, 0, false, "read-error" })
	r.diagnostics = failingWriter{}
	if result, err := r.admit(context.Background(), Request{}); err != nil || result.state != "unevaluated" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRealMemoryAdmissionWaitsForReliefThenIsImmediate(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		skipOrFailRealCgroup(t, "python3 is unavailable for the admission fixture")
	}
	const limit = int64(128 * 1024 * 1024)
	const reserve = int64(64 * 1024 * 1024)
	parent := writableMemoryParent(t, "134217728")
	filler, err := New(Config{CommonDir: t.TempDir(), CgroupParent: parent, Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := New(Config{CommonDir: t.TempDir(), CgroupParent: parent, MemorySlice: parent, MemoryReserve: reserve, AdmissionMaxWait: 2 * time.Second, PollInterval: 10 * time.Millisecond, Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := filler.Probe(context.Background()); err != nil {
		skipOrFailRealCgroup(t, "real admission filler cgroup unavailable: %v", err)
	}
	if err := admitted.Probe(context.Background()); err != nil {
		skipOrFailRealCgroup(t, "real admission run cgroup unavailable: %v", err)
	}
	// The filler and admitted runners have independent ledgers whose first IDs
	// would otherwise collide under the shared parent.
	if _, err := admitted.ledger.reserveID(); err != nil {
		t.Fatal(err)
	}
	fillerDone := make(chan launchOutcome, 1)
	go func() {
		record, launchErr := filler.Launch(context.Background(), Request{Argv: []string{"python3", "-c", "import time; x=bytearray(80*1024*1024); [x.__setitem__(i, 1) for i in range(0, len(x), 4096)]; time.sleep(0.5)"}})
		fillerDone <- launchOutcome{record: record, err: launchErr}
	}()
	pressureDeadline := time.Now().Add(2 * time.Second)
	for {
		cur, max, ok, reason := readSliceMemory(parent)
		if !ok {
			t.Fatalf("read real admission parent: %s", reason)
		}
		if max != limit {
			t.Fatalf("memory.max=%d want %d", max, limit)
		}
		if max-cur < reserve {
			break
		}
		if time.Now().After(pressureDeadline) {
			t.Fatal("filler did not create admission pressure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waited, err := admitted.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if waited.Admission != "waited" || waited.AdmissionWaitedMS <= 0 {
		t.Fatalf("waited record=%+v", waited)
	}
	fillerResult := <-fillerDone
	if fillerResult.err != nil || fillerResult.record == nil || fillerResult.record.Status != StatusExited {
		t.Fatalf("filler result=%+v", fillerResult)
	}
	immediate, err := admitted.Launch(context.Background(), Request{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	if immediate.Admission != "immediate" || immediate.AdmissionWaitedMS != 0 {
		t.Fatalf("immediate record=%+v", immediate)
	}
}

func TestDaemonAdmitReadFullFraming(t *testing.T) {
	want := runnerAdmitResponseFrame{OK: true, Code: "OK", Data: json.RawMessage(`{"state":"waited","waited_ms":17}`)}
	var encoded bytes.Buffer
	if err := writeRunnerAdmitFrame(&encoded, want); err != nil {
		t.Fatal(err)
	}
	var got runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(chunkReader{reader: bytes.NewReader(encoded.Bytes())}, &got); err != nil {
		t.Fatalf("one-byte framing failed: %v", err)
	}
	if !got.OK || got.Code != "OK" || !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("frame=%+v", got)
	}
	for name, cut := range map[string]int{"partial header": 2, "partial payload": len(encoded.Bytes()) - 1} {
		t.Run(name, func(t *testing.T) {
			var frame runnerAdmitResponseFrame
			if err := readRunnerAdmitFrame(bytes.NewReader(encoded.Bytes()[:cut]), &frame); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error=%v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestDaemonAdmitGrantStatesRemainByteIdentical(t *testing.T) {
	for _, state := range []string{"immediate", "waited", "timeout", "unevaluated"} {
		t.Run(state, func(t *testing.T) {
			clock := newInstantClock()
			runner, _ := gateOnlyRunner(t, clock, func(string) (int64, int64, bool, string) { return 0, 100, true, "" })
			client, server := net.Pipe()
			runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
			go func() {
				defer server.Close()
				var request runnerAdmitRequestFrame
				if readRunnerAdmitFrame(server, &request) != nil {
					return
				}
				data, _ := json.Marshal(runnerAdmitGrant{State: state, Reason: "reason", WaitedMS: 7})
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				var one [1]byte
				_, _ = server.Read(one[:])
			}()
			result, err := runner.admit(context.Background(), Request{})
			if err != nil || result.state != state || result.reason != "reason" || result.waitedMS != 7 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			result.releaseAdmission()
		})
	}
	for _, state := range []string{"disabled", "bypassed"} {
		runner := &Runner{clock: newInstantClock()}
		request := Request{NoAdmit: state == "bypassed"}
		if state == "bypassed" {
			runner.memorySlice, runner.memoryReserve = "/unused", 1
		}
		result, err := runner.admit(context.Background(), request)
		if err != nil || result.state != state {
			t.Fatalf("%s result=%+v err=%v", state, result, err)
		}
	}
}

func TestDaemonAdmitStatesAreByteIdenticalOnRunRecord(t *testing.T) {
	for _, state := range []string{"immediate", "waited", "timeout", "unevaluated"} {
		t.Run(state, func(t *testing.T) {
			path := currentSliceForTest(t)
			runner, err := New(Config{
				CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}},
				MemorySlice: path, MemoryReserve: 40, AdmissionMaxWait: time.Second,
				PollInterval: time.Millisecond, Clock: newInstantClock(),
				sliceMemoryFn: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
			})
			if err != nil {
				t.Fatal(err)
			}
			client, server := net.Pipe()
			runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
			go func() {
				defer server.Close()
				var request runnerAdmitRequestFrame
				if readRunnerAdmitFrame(server, &request) != nil {
					return
				}
				data, _ := json.Marshal(runnerAdmitGrant{State: state, Reason: "wire-reason", WaitedMS: 11})
				_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
				var one [1]byte
				_, _ = server.Read(one[:])
			}()
			runner.startFn = func(*exec.Cmd) error { return errors.New("injected after admission") }
			if _, err := runner.Launch(context.Background(), Request{Argv: []string{"/bin/true"}}); err == nil {
				t.Fatal("injected Start error not returned")
			}
			events, err := runner.ledger.read()
			if err != nil || len(events) == 0 {
				t.Fatalf("events=%d err=%v", len(events), err)
			}
			record := events[len(events)-1].Run
			if record.Admission != state || record.AdmissionReason != "wire-reason" || record.AdmissionWaitedMS != 11 {
				t.Fatalf("record admission=(%q,%q,%d)", record.Admission, record.AdmissionReason, record.AdmissionWaitedMS)
			}
		})
	}
}

func TestDaemonAdmitWedgedDaemonDeadlineFallsToFlock(t *testing.T) {
	// A wedged daemon (dial succeeds, no response) trips the client-side transport
	// deadline (Sol build r1 #1/#4). The client closes the socket, then routes to the
	// SINGLE flock fallback (§2.1, Sol build r2 #2): the plan-approved advisory
	// degradation — the flock serialises fallback clients (bounded), never an ungated
	// unevaluated stampede. The bounded deadline ensures the client is never stranded.
	path := currentSliceForTest(t)
	runner := &Runner{
		memorySlice: path, memoryReserve: 40, admissionMaxWait: 5 * time.Millisecond,
		pollInterval: time.Millisecond, clock: systemClock{},
		sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
	}
	client, server := net.Pipe()
	runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var request runnerAdmitRequestFrame
		if readRunnerAdmitFrame(server, &request) != nil {
			return
		}
		var one [1]byte
		_, _ = server.Read(one[:]) // wedged: never reply until the client closes
	}()
	var attempts atomic.Int64
	runner.lockAttemptFn = func(string) (*admitLock, error) { attempts.Add(1); return &admitLock{}, nil }
	started := time.Now()
	result, err := runner.admit(context.Background(), Request{})
	if err != nil || result.lock == nil || attempts.Load() != 1 {
		t.Fatalf("result=%+v attempts=%d err=%v (want flock fallback)", result, attempts.Load(), err)
	}
	if elapsed := time.Since(started); elapsed < admitTransportGrace || elapsed > 2*time.Second {
		t.Fatalf("wedged-daemon deadline elapsed=%v", elapsed)
	}
	result.releaseAdmission()
}

func TestDaemonAdmitPartialFrameFallsToFlock(t *testing.T) {
	// A partial frame is a post-dial failure (a committed-but-unaccepted grant, §2.1):
	// the client closes the socket, then routes to the SINGLE flock fallback (Sol build
	// r2 #2). The truncated frame produces an immediate io.ErrUnexpectedEOF once the
	// server closes, so no deadline wait is needed here.
	path := currentSliceForTest(t)
	runner := &Runner{
		memorySlice: path, memoryReserve: 40, admissionMaxWait: 50 * time.Millisecond,
		pollInterval: time.Millisecond, clock: newInstantClock(),
		sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
	}
	client, server := net.Pipe()
	runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		var request runnerAdmitRequestFrame
		if readRunnerAdmitFrame(server, &request) != nil {
			_ = server.Close()
			return
		}
		var complete bytes.Buffer
		data, _ := json.Marshal(runnerAdmitGrant{State: "immediate"})
		_ = writeRunnerAdmitFrame(&complete, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data})
		partial := complete.Bytes()[:len(complete.Bytes())-1]
		_, _ = server.Write(partial)
		_ = server.Close() // truncated frame -> client read errors immediately
	}()
	var attempts atomic.Int64
	runner.lockAttemptFn = func(string) (*admitLock, error) { attempts.Add(1); return &admitLock{}, nil }
	result, err := runner.admit(context.Background(), Request{})
	if err != nil || result.lock == nil || attempts.Load() != 1 {
		t.Fatalf("result=%+v attempts=%d err=%v (want flock fallback)", result, attempts.Load(), err)
	}
	result.releaseAdmission()
}

func TestDaemonAdmitFullFrameAtDeadlineWinsWithoutFlock(t *testing.T) {
	data, err := json.Marshal(runnerAdmitGrant{State: "waited", WaitedMS: 9})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := writeRunnerAdmitFrame(&encoded, runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data}); err != nil {
		t.Fatal(err)
	}
	conn := &deadlineFrameConn{reader: bytes.NewReader(encoded.Bytes())}
	path := currentSliceForTest(t)
	runner := &Runner{
		memorySlice: path, memoryReserve: 40, admissionMaxWait: time.Millisecond,
		pollInterval: time.Millisecond, clock: newInstantClock(),
		sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
		admitDialFn: func(context.Context, string) (net.Conn, error) { return conn, nil },
	}
	var fallback atomic.Int64
	runner.lockAttemptFn = func(string) (*admitLock, error) {
		fallback.Add(1)
		return &admitLock{}, nil
	}
	result, err := runner.admit(context.Background(), Request{})
	if err != nil || result.state != "waited" || result.waitedMS != 9 || fallback.Load() != 0 {
		t.Fatalf("result=%+v fallback=%d err=%v", result, fallback.Load(), err)
	}
	if conn.closed.Load() {
		t.Fatal("winning grant connection closed before launch release")
	}
	result.releaseAdmission()
	if !conn.closed.Load() {
		t.Fatal("winning grant connection was not released")
	}
}

func TestDaemonAdmitDialFailureAndBusyFallToFlock(t *testing.T) {
	// §2.1 (Sol build r2 #2): EVERY daemon non-grant routes to the SINGLE flock
	// fallback — an unreachable daemon (dial failure -> no live reservations) AND a
	// live daemon that declines at a cap (E_DAEMON_BUSY). Labelling a busy decline as
	// "unevaluated" would launch un-gated and could stampede; the documented advisory
	// degradation is the flock, which serialises fallback clients (bounded).
	for _, test := range []struct {
		name string
		dial func(context.Context, string) (net.Conn, error)
	}{
		{name: "dial failure",
			dial: func(context.Context, string) (net.Conn, error) { return nil, os.ErrNotExist }},
		{name: "busy",
			dial: func(context.Context, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					defer server.Close()
					var request runnerAdmitRequestFrame
					_ = readRunnerAdmitFrame(server, &request)
					_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: "E_DAEMON_BUSY", Error: "busy"})
					var one [1]byte
					_, _ = server.Read(one[:])
				}()
				return client, nil
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := currentSliceForTest(t)
			runner := &Runner{
				memorySlice: path, memoryReserve: 40, admissionMaxWait: time.Second,
				pollInterval: time.Millisecond, clock: newInstantClock(), admitDialFn: test.dial,
				sliceMemory: func(string) (int64, int64, bool, string) { return 0, 100, true, "" },
			}
			var attempts atomic.Int64
			runner.lockAttemptFn = func(string) (*admitLock, error) { attempts.Add(1); return &admitLock{}, nil }
			result, err := runner.admit(context.Background(), Request{})
			if err != nil || result.lock == nil || attempts.Load() != 1 {
				t.Fatalf("%s result=%+v attempts=%d err=%v (want flock fallback)", test.name, result, attempts.Load(), err)
			}
			result.releaseAdmission()
		})
	}
}
