//go:build linux

package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// isolatedScopeParent creates a unique cgroup-v2 parent per test so a run's
// `.aira-RUN-1` scope cannot collide (EEXIST) with another runner test that reuses
// the same per-CommonDir run-counter name under the shared default parent. Tests
// that intentionally leave a scope un-removed — forced-close (a lingering `sleep`
// descendant keeps the cgroup non-empty) and append-failure (an aborted launch
// returns before scope removal) — must use this, or their leftover `.aira-RUN-1`
// EEXISTs the next same-named mkdir within the same `go test` process. No resource
// controller is enabled (bare scoped kill needs none); killed+removed at test end.
func isolatedScopeParent(t *testing.T) string {
	t.Helper()
	mount, err := unifiedMount()
	if err != nil {
		skipOrFailRealCgroup(t, "cgroup-v2 unavailable: %v", err)
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		skipOrFailRealCgroup(t, "current cgroup unavailable: %v", err)
	}
	// current holds the test process (no-internal-process rule), so place the parent
	// under current's parent, which delegates to children and holds no direct procs.
	host := filepath.Dir(current)
	name := ".aira-m17-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	parent := filepath.Join(host, name)
	if err := os.Mkdir(parent, 0o755); err != nil {
		skipOrFailRealCgroup(t, "cannot create scope parent under %s: %v", host, err)
	}
	t.Cleanup(func() {
		// Kill the whole subtree, then remove any lingering child run scope dirs (a
		// forced-close descendant or an aborted launch can leave a `.aira-RUN-n` under
		// the parent) before removing the parent itself. Retry to absorb the
		// cgroup.kill→task-exit window before cgroup.procs drains; without removing the
		// children the parent rmdir fails and a later invocation would EEXIST its name.
		_ = os.WriteFile(filepath.Join(parent, "cgroup.kill"), []byte("1"), 0o644)
		for i := 0; i < 100; i++ {
			if entries, err := os.ReadDir(parent); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						_ = os.Remove(filepath.Join(parent, e.Name()))
					}
				}
			}
			if os.Remove(parent) == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return parent
}

type lockStepSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *lockStepSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *lockStepSink) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

type blockingSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	writes  atomic.Int64
	mu      sync.Mutex
	buf     bytes.Buffer
}

func newBlockingSink() *blockingSink {
	return &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingSink) Write(p []byte) (int, error) {
	s.writes.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *blockingSink) unblock() { close(s.release) }

func (s *blockingSink) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

type shortWriteSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *shortWriteSink) Write(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *shortWriteSink) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

type errorSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *errorSink) Write(p []byte) (int, error) {
	n := len(p) / 2
	if n == 0 {
		n = len(p)
	}
	s.mu.Lock()
	_, _ = s.buf.Write(p[:n])
	s.mu.Unlock()
	return n, errors.New("EPIPE")
}

var liveMarkerRE = regexp.MustCompile(`\[aira: ([0-9]+) bytes elided from live view — see run-log\]`)

func drainLive(t *testing.T, data []byte, sink io.Writer) (captureResult, []byte) {
	t.Helper()
	base := t.TempDir()
	dst, err := os.OpenFile(filepath.Join(base, "capture"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stream := newLiveStream(sink)
	resultCh := make(chan captureResult, 1)
	go drain("out", rd, dst, resultCh, stream)
	go func() {
		_, _ = wr.Write(data)
		_ = wr.Close()
	}()
	result := <-resultCh
	<-stream.done
	var live []byte
	switch sink := sink.(type) {
	case *lockStepSink:
		live = sink.Bytes()
	case *blockingSink:
		live = sink.Bytes()
	case *shortWriteSink:
		live = sink.Bytes()
	case *errorSink:
		sink.mu.Lock()
		live = append([]byte(nil), sink.buf.Bytes()...)
		sink.mu.Unlock()
	}
	return result, live
}

func drainCapture(t *testing.T, data []byte) (captureResult, []byte) {
	t.Helper()
	base := t.TempDir()
	path := filepath.Join(base, "capture")
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan captureResult, 1)
	go drain("out", rd, dst, resultCh)
	go func() {
		_, _ = wr.Write(data)
		_ = wr.Close()
	}()
	result := <-resultCh
	captured, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return result, captured
}

func TestLiveTeeLockStepPreservesBinaryBytes(t *testing.T) {
	oldDepth := liveQueueDepth
	liveQueueDepth = 1024
	t.Cleanup(func() { liveQueueDepth = oldDepth })
	want := append([]byte{0, 1, 255}, bytes.Repeat([]byte("live\x00"), 2000)...)
	result, live := drainLive(t, want, &lockStepSink{})
	if result.Err != nil || result.State != OutputComplete || result.Bytes != int64(len(want)) {
		t.Fatalf("capture=%+v", result)
	}
	if !bytes.Equal(live, want) || bytes.Contains(live, []byte("aira:")) {
		t.Fatalf("live=%q want exact binary bytes", live[:min(len(live), 80)])
	}
}

func TestLiveTeeDoesNotChangeCaptureDigestOrBytes(t *testing.T) {
	want := bytes.Repeat([]byte{0, 1, 2, 255, '\n'}, 4096)
	withoutLive, captured := drainCapture(t, want)
	withLive, live := drainLive(t, want, &lockStepSink{})
	if withoutLive.Digest != withLive.Digest || withoutLive.Bytes != withLive.Bytes || !bytes.Equal(captured, live) {
		t.Fatalf("capture changed without=%+v with=%+v captured=%d live=%d", withoutLive, withLive, len(captured), len(live))
	}
}

func TestLiveTeeUnitCaptureCompletesWhileSinkBlocked(t *testing.T) {
	oldDepth := liveQueueDepth
	liveQueueDepth = 2
	t.Cleanup(func() { liveQueueDepth = oldDepth })
	sink := newBlockingSink()
	const chunk = 32 * 1024
	want := bytes.Repeat([]byte("capture-unit-"), (liveQueueDepth*chunk*4)/len("capture-unit-"))
	base := t.TempDir()
	path := filepath.Join(base, "capture")
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	live := newLiveStream(sink)
	resultCh := make(chan captureResult, 1)
	drainDone := make(chan struct{})
	go func() {
		drain("out", rd, dst, resultCh, live)
		close(drainDone)
	}()
	go func() {
		_, _ = wr.Write(want)
		_ = wr.Close()
	}()
	select {
	case <-sink.entered:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("blocking sink was not entered")
	}
	deadline := testdeadline.After(2 * time.Second)
	for {
		stat, statErr := os.Stat(path)
		if statErr == nil && stat.Size() == int64(len(want)) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("capture file did not reach full size while sink was blocked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	var result captureResult
	select {
	case result = <-resultCh:
	case <-testdeadline.After(time.Second):
		t.Fatal("capture result was not published while sink was blocked")
	}
	select {
	case <-drainDone:
	case <-testdeadline.After(time.Second):
		t.Fatal("drain did not exit while sink was blocked")
	}
	sink.unblock()
	select {
	case <-live.done:
	case <-testdeadline.After(time.Second):
		t.Fatal("live writer did not exit after release")
	}
	wantDigest := sha256.Sum256(want)
	if result.Err != nil || result.Bytes != int64(len(want)) || result.Digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("capture=%+v want bytes=%d digest=%x", result, len(want), wantDigest)
	}
}

func TestLiveTeeElisionMarkersAccountForDropsAndTrailingDrop(t *testing.T) {
	oldDepth := liveQueueDepth
	liveQueueDepth = 4
	t.Cleanup(func() { liveQueueDepth = oldDepth })
	sink := newBlockingSink()
	want := bytes.Repeat([]byte("0123456789abcdef"), 32*1024)
	base := t.TempDir()
	dst, err := os.OpenFile(filepath.Join(base, "capture"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stream := newLiveStream(sink)
	resultCh := make(chan captureResult, 1)
	drainDone := make(chan struct{})
	go func() {
		drain("out", rd, dst, resultCh, stream)
		close(drainDone)
	}()
	go func() {
		_, _ = wr.Write(want)
		_ = wr.Close()
	}()
	select {
	case <-sink.entered:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("live sink did not block")
	}
	var result captureResult
	select {
	case result = <-resultCh:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("capture result was not published while live sink was blocked")
	}
	select {
	case <-drainDone:
	case <-testdeadline.After(time.Second):
		t.Fatal("drain did not exit while live sink was blocked")
	}
	sink.unblock()
	select {
	case <-stream.done:
	case <-testdeadline.After(time.Second):
		t.Fatal("live writer did not exit after release")
	}
	live := sink.Bytes()
	if result.Err != nil || result.Bytes != int64(len(want)) {
		t.Fatalf("capture=%+v", result)
	}
	markers, elided := markerAccounting(live)
	if markers == 0 || elided == 0 || int64(len(live))-int64(markerBytes(live))+elided != int64(len(want)) {
		t.Fatalf("live accounting markers=%d elided=%d live=%d want=%d", markers, elided, len(live), len(want))
	}
}

func TestLiveTeeShortWritesAndSinkErrorsDoNotAffectCapture(t *testing.T) {
	data := bytes.Repeat([]byte{0, 1, 2, 255}, 4096)
	result, live := drainLive(t, data, &shortWriteSink{})
	if result.Err != nil || result.Bytes != int64(len(data)) || !bytes.Equal(live, data) {
		t.Fatalf("short sink result=%+v live=%d want=%d", result, len(live), len(data))
	}
	result, _ = drainLive(t, data, &errorSink{})
	if result.Err != nil || result.Bytes != int64(len(data)) {
		t.Fatalf("error sink changed capture=%+v", result)
	}
}

func markerAccounting(data []byte) (int, int64) {
	var total int64
	markers := 0
	for _, match := range liveMarkerRE.FindAllSubmatch(data, -1) {
		count, _ := strconv.ParseInt(string(match[1]), 10, 64)
		total += count
		markers++
	}
	return markers, total
}

func markerBytes(data []byte) int {
	total := 0
	for _, match := range liveMarkerRE.FindAllIndex(data, -1) {
		total += match[1] - match[0]
	}
	return total
}

func TestLiveTeeLaunchBlockedSinkDoesNotBlockCapture(t *testing.T) {
	r := realRunner(t)
	sink := newBlockingSink()
	done := filepath.Join(t.TempDir(), "DONE")
	const size = 2 * 1024 * 1024
	outcome := make(chan struct {
		record *RunRecord
		err    error
	}, 1)
	go func() {
		record, err := r.Launch(context.Background(), Request{
			Argv:       []string{"/bin/sh", "-c", fmt.Sprintf("head -c %d /dev/zero; touch %q", size, done)},
			LiveStdout: sink,
		})
		outcome <- struct {
			record *RunRecord
			err    error
		}{record, err}
	}()
	select {
	case <-sink.entered:
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("live sink did not become blocked")
	}
	deadline := testdeadline.After(3 * time.Second)
	for {
		record, err := r.Get("RUN-1")
		if err == nil && record.OutputRefs["out"].Path != "" {
			stat, statErr := os.Stat(record.OutputRefs["out"].Path)
			if statErr == nil && stat.Size() == size {
				if _, doneErr := os.Stat(done); doneErr == nil {
					break
				}
			}
		}
		select {
		case <-deadline:
			t.Fatal("capture did not complete while live sink was blocked")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	sink.unblock()
	select {
	case item := <-outcome:
		if item.err != nil || item.record == nil || item.record.Status != StatusExited {
			t.Fatalf("launch=%+v", item)
		}
		want := sha256.Sum256(bytes.Repeat([]byte{0}, size))
		if item.record.OutputRefs["out"].Digest != hex.EncodeToString(want[:]) {
			t.Fatalf("digest=%s want=%x", item.record.OutputRefs["out"].Digest, want)
		}
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("launch did not return after live sink release")
	}
}

func TestLiveTeeNoWriterCreatedBeforeRunningAppendFailure(t *testing.T) {
	// Unique parent: the injected append failure aborts Launch after the scope is
	// created but before it is removed, so the leftover `.aira-RUN-1` would EEXIST a
	// later same-named run under the shared parent; the parent cleanup removes it.
	r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: isolatedScopeParent(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		skipOrFailRealCgroup(t, "real cgroup-v2 delegation unavailable: %v", err)
	}
	var created atomic.Int64
	previousHook := liveStreamCreateHook
	liveStreamCreateHook = func() { created.Add(1) }
	t.Cleanup(func() { liveStreamCreateHook = previousHook })
	r.appendFault = func(event ledgerEvent) error {
		if event.Kind == "running" || event.Kind == "scope-integrity" {
			return errors.New("injected running append failure")
		}
		return nil
	}
	_, err = r.Launch(context.Background(), Request{
		Argv:       []string{"/bin/true"},
		LiveStdout: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "E_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("append fault launch error=%v", err)
	}
	if got := created.Load(); got != 0 {
		t.Fatalf("live stream created before append failure: %d", got)
	}
}

func TestLiveTeeForcedCloseDisablesBlockedSink(t *testing.T) {
	// Integration property (timing-independent): forced-close — a descendant holds
	// the pipe open past grace — with a PERMANENTLY blocked live sink must NOT hang
	// Launch. The capture path completes on the grace timer and finishLive disables
	// the gate WITHOUT joining the writer, so Launch returns forced-closed regardless
	// of writer scheduling. We deliberately do NOT synchronise on the writer reaching
	// the sink (that races the grace timer and is not this test's property); the
	// gate-disable BOUND (≤1 in-flight write after full drain) is asserted
	// deterministically by the cgroup-free unit tests, which can observe live.done.
	// Unique parent: the forced-close child leaves a `sleep` descendant holding the
	// pipe, so the runner cannot remove the (non-empty) `.aira-RUN-1` scope; under a
	// unique parent that leftover cannot EEXIST-collide with another runner test, and
	// the parent's cgroup.kill cleanup terminates the descendant at test end.
	r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: isolatedScopeParent(t), Grace: 200 * time.Millisecond, TermGrace: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		skipOrFailRealCgroup(t, "real cgroup-v2 delegation unavailable: %v", err)
	}
	sink := newBlockingSink()
	outcome := make(chan struct {
		record *RunRecord
		err    error
	}, 1)
	go func() {
		record, err := r.Launch(context.Background(), Request{
			Argv:       []string{"/bin/sh", "-c", "head -c 131072 /dev/zero; (sleep 30) & exit 0"},
			LiveStdout: sink,
		})
		outcome <- struct {
			record *RunRecord
			err    error
		}{record, err}
	}()
	// The key I4 guarantee: Launch returns despite the permanently blocked sink.
	select {
	case item := <-outcome:
		if item.err != nil || item.record == nil || !item.record.CaptureForcedClosed {
			t.Fatalf("forced-close launch err=%v record=%+v", item.err, item.record)
		}
	case <-testdeadline.After(8 * time.Second):
		t.Fatal("forced-close launch hung on the blocked live sink")
	}
	// The gate was disabled by the time Launch returned, so at most one write was in
	// flight (0 if the writer had not yet reached the sink); no new writes can start.
	if got := sink.writes.Load(); got > 1 {
		t.Fatalf("forced-close crossed the disabled gate %d times", got)
	}
	sink.unblock()
}

func TestLiveTeeCancelDuringNormalJoinKeepsCaptureComplete(t *testing.T) {
	r := realRunner(t)
	sink := newBlockingSink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcome := make(chan struct {
		record *RunRecord
		err    error
	}, 1)
	go func() {
		record, err := r.Launch(ctx, Request{Argv: []string{"/bin/sh", "-c", "printf cancel-join"}, LiveStdout: sink})
		outcome <- struct {
			record *RunRecord
			err    error
		}{record, err}
	}()
	select {
	case <-sink.entered:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("live sink did not become blocked")
	}
	var path string
	deadline := testdeadline.After(2 * time.Second)
	for path == "" {
		if record, err := r.Get("RUN-1"); err == nil {
			path = record.OutputRefs["out"].Path
			if path != "" {
				if data, readErr := os.ReadFile(path); readErr == nil && string(data) == "cancel-join" {
					break
				}
				path = ""
			}
		}
		select {
		case <-deadline:
			t.Fatal("capture did not reach EOF-complete bytes before cancel")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	// Let Launch pass the completed capture result into collectCapture and
	// reach the normal live-writer join before exercising cancellation.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case item := <-outcome:
		if item.err != nil || item.record == nil || !item.record.CaptureComplete || item.record.CaptureForcedClosed {
			t.Fatalf("cancel join launch=%+v", item)
		}
		if item.record.OutputRefs["out"].Digest == "" {
			t.Fatal("cancel join lost capture digest")
		}
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("cancel during normal live join did not return")
	}
	if got := sink.writes.Load(); got > 1 {
		t.Fatalf("cancel join crossed blocked gate %d times", got)
	}
	sink.unblock()
}

func TestLiveTeeGateDisableIsNonBlocking(t *testing.T) {
	sink := newBlockingSink()
	stream := newLiveStream(sink)
	stream.ch <- liveChunk{data: []byte("x")}
	select {
	case <-sink.entered:
	case <-testdeadline.After(time.Second):
		t.Fatal("writer did not enter sink")
	}
	stream.gate.disable()
	stream.finalDropped = 7
	close(stream.ch)
	done := make(chan struct{})
	go func() { <-stream.done; close(done) }()
	select {
	case <-done:
		t.Fatal("blocked sink unexpectedly returned")
	case <-time.After(20 * time.Millisecond):
	}
	sink.unblock()
	select {
	case <-done:
	case <-testdeadline.After(time.Second):
		t.Fatal("writer did not drain after release")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
