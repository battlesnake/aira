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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type readStep struct {
	data []byte
	err  error
}

type scriptedReadCloser struct {
	steps  []readStep
	closed bool
}

func (r *scriptedReadCloser) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := &r.steps[0]
	n := copy(p, step.data)
	if n < len(step.data) {
		step.data = step.data[n:]
		return n, nil
	}
	err := step.err
	r.steps = r.steps[1:]
	return n, err
}

func (r *scriptedReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestPTYReaderMapsOnlyEIOToEOFWithoutDroppingBytes(t *testing.T) {
	other := errors.New("other read failure")
	for _, tc := range []struct {
		name     string
		step     readStep
		wantData string
		wantErr  error
	}{
		{name: "zero EIO", step: readStep{err: unix.EIO}, wantErr: io.EOF},
		{name: "bytes EIO", step: readStep{data: []byte("tail"), err: unix.EIO}, wantData: "tail", wantErr: io.EOF},
		{name: "wrapped EIO", step: readStep{data: []byte("wrapped"), err: &os.PathError{Op: "read", Path: "ptmx", Err: unix.EIO}}, wantData: "wrapped", wantErr: io.EOF},
		{name: "bytes other", step: readStep{data: []byte("partial"), err: other}, wantData: "partial", wantErr: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &scriptedReadCloser{steps: []readStep{tc.step}}
			reader := &ptyReader{ReadCloser: source}
			buf := make([]byte, 32)
			n, err := reader.Read(buf)
			if string(buf[:n]) != tc.wantData || !errors.Is(err, tc.wantErr) {
				t.Fatalf("Read=(%q,%v), want (%q,%v)", buf[:n], err, tc.wantData, tc.wantErr)
			}
		})
	}
}

func TestPTYDrainCommitsLargeTailBeforeEIOAndRejectsOtherErrors(t *testing.T) {
	payload := append(bytes.Repeat([]byte("0123456789abcdef"), 4096), []byte("TAIL-MARKER\n")...)
	for _, tc := range []struct {
		name      string
		finalErr  error
		wantState OutputState
	}{
		{name: "EIO is complete", finalErr: unix.EIO, wantState: OutputComplete},
		{name: "non-EIO is partial", finalErr: unix.EAGAIN, wantState: OutputPartial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture")
			dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			source := &scriptedReadCloser{steps: []readStep{{data: payload[:32*1024]}, {data: payload[32*1024:], err: tc.finalErr}}}
			results := make(chan captureResult, 1)
			go drain("log", &ptyReader{ReadCloser: source}, dst, results)
			result := <-results
			captured, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(payload)
			if !bytes.Equal(captured, payload) || result.Bytes != int64(len(payload)) || result.Digest != hex.EncodeToString(digest[:]) || result.State != tc.wantState || source.closed != true {
				t.Fatalf("result=%+v bytes=%d closed=%v", result, len(captured), source.closed)
			}
			if tc.wantState == OutputComplete && result.Err != nil {
				t.Fatalf("clean EIO drain error=%v", result.Err)
			}
			if tc.wantState == OutputPartial && !errors.Is(result.Err, tc.finalErr) {
				t.Fatalf("partial drain error=%v want %v", result.Err, tc.finalErr)
			}
		})
	}
}

type closeCompletesCapture struct {
	results chan<- captureResult
}

func (c closeCompletesCapture) Close() error {
	c.results <- captureResult{Name: "log", State: OutputComplete}
	return nil
}

func TestPTYCaptureIncompleteIsMonotonicAcrossDeadlineEIORace(t *testing.T) {
	results := make(chan captureResult, 1)
	tracker := &captureCompleteness{}
	captures, forced := collectPTYCapture(context.Background(), results, 1, time.Millisecond, []io.Closer{closeCompletesCapture{results: results}}, tracker)
	if len(captures) != 1 || captures[0].State != OutputPartial || !forced || tracker.complete() {
		t.Fatalf("captures=%+v forced=%v complete=%v", captures, forced, tracker.complete())
	}
}

type neverEmptyPTYScope struct {
	killed bool
}

func (*neverEmptyPTYScope) Reference() string       { return "/never-empty" }
func (*neverEmptyPTYScope) FD() int                 { return -1 }
func (*neverEmptyPTYScope) Members() ([]int, error) { return []int{123}, nil }
func (*neverEmptyPTYScope) Empty() (bool, error)    { return false, nil }
func (*neverEmptyPTYScope) Terminate([]int) error   { return nil }
func (s *neverEmptyPTYScope) Kill() error           { s.killed = true; return nil }
func (*neverEmptyPTYScope) Remove() error           { return nil }

func TestPTYScopeQuiescenceUsesBoundedContext(t *testing.T) {
	scope := &neverEmptyPTYScope{}
	r := &Runner{grace: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hadDescendants, err := r.quiescePTYScope(ctx, scope)
	if !hadDescendants || !scope.killed || !errors.Is(err, context.Canceled) {
		t.Fatalf("quiesce=(descendants=%v killed=%v err=%v)", hadDescendants, scope.killed, err)
	}
}

func TestMergeEvidenceCarriesPTYBufferingAndMergedCapture(t *testing.T) {
	base := RunRecord{ID: "RUN-1", Buffering: "none"}
	candidate := RunRecord{ID: "RUN-1", Buffering: "pty", Merge: true, Status: StatusExited}
	merged := mergeEvidence(base, candidate)
	if merged.Buffering != "pty" || !merged.Merge {
		t.Fatalf("PTY evidence was lost: %+v", merged)
	}
}

func TestRealCgroupPTYControllingTTYAndPipeDiscriminator(t *testing.T) {
	script := `test -t 1 && test -t 2 && tty <&1 && test "$(ps -o tpgid= -p $$ | tr -d ' ')" = "$(ps -o pgid= -p $$ | tr -d ' ')" && : </dev/tty`
	ptyRecord, err := realRunner(t).Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script}, PTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if ptyRecord.ExitCode == nil || *ptyRecord.ExitCode != 0 || ptyRecord.Buffering != "pty" || !ptyRecord.Merge || !ptyRecord.CaptureComplete || len(ptyRecord.OutputRefs) != 1 {
		t.Fatalf("PTY record=%+v", ptyRecord)
	}
	data, err := os.ReadFile(ptyRecord.OutputRefs["log"].Path)
	if err != nil || !bytes.Contains(data, []byte("/dev/pts/")) {
		t.Fatalf("PTY output=%q err=%v", data, err)
	}

	pipeRecord, err := realRunner(t).Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	if pipeRecord.ExitCode == nil || *pipeRecord.ExitCode == 0 {
		t.Fatalf("pipe capture falsely passed TTY probe: %+v", pipeRecord)
	}
	if ptyRecord.EnvDigest != pipeRecord.EnvDigest {
		t.Fatalf("PTY tactic changed base env digest: pty=%q pipe=%q", ptyRecord.EnvDigest, pipeRecord.EnvDigest)
	}
}

func TestRealCgroupPTYByteFaithfulCompleteCapture(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "printf 'a\\nb\\n'"}, PTY: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("a\nb\n")
	data, err := os.ReadFile(record.OutputRefs["log"].Path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(want)
	ref := record.OutputRefs["log"]
	if !bytes.Equal(data, want) || bytes.Contains(data, []byte{'\r'}) || ref.Digest != hex.EncodeToString(digest[:]) || ref.State != OutputComplete || !record.CaptureComplete {
		t.Fatalf("record=%+v data=%q", record, data)
	}
	chunk, err := r.ReadOutput(context.Background(), OutputRequest{RunID: record.ID, Full: true})
	if err != nil || chunk.Stream != "log" || !bytes.Equal(chunk.Bytes, want) {
		t.Fatalf("default merged run-log chunk=%+v err=%v", chunk, err)
	}
}

func TestRealCgroupPTYDefaultNullStdinDoesNotDeadlock(t *testing.T) {
	started := time.Now()
	deadline := 2 * time.Second
	record, err := realRunner(t).Launch(context.Background(), Request{Argv: []string{"/bin/cat"}, PTY: true, Timeout: deadline})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) >= deadline || record.ExitCode == nil || *record.ExitCode != 0 || !record.CaptureComplete {
		t.Fatalf("null-stdin PTY run did not terminate cleanly: elapsed=%s record=%+v", time.Since(started), record)
	}
}

func TestRealCgroupPTYRunIsScopeKillable(t *testing.T) {
	r := realRunner(t)
	outcome := launchAsync(t, r, Request{Argv: []string{"/bin/sh", "-c", "sleep 30"}, PTY: true})
	killed, err := r.Kill(context.Background(), "RUN-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if killed.Status != StatusKilled || !killed.ScopeKill.Completed {
		t.Fatalf("run-kill result=%+v", killed)
	}
	result := <-outcome
	if result.err != nil && !strings.Contains(result.err.Error(), "U_RUN_RECONCILE_REQUIRED") {
		t.Fatal(result.err)
	}
	current, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusKilled || current.Buffering != "pty" || !current.Merge || current.CaptureComplete || current.OutputRefs["log"].State != OutputPartial {
		t.Fatalf("killed PTY capture was not partial: %+v", current)
	}
}

func TestAllocatePTYReturnsOutputRawCloseOnExecPair(t *testing.T) {
	master, slave, err := allocatePTY()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = master.Close()
		_ = slave.Close()
	})

	termios, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if termios.Oflag&unix.OPOST != 0 {
		t.Fatalf("slave output processing remains enabled: oflag=%#x", termios.Oflag)
	}
	for name, fd := range map[string]int{"master": int(master.Fd()), "slave": int(slave.Fd())} {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("%s F_GETFD: %v", name, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("%s is not O_CLOEXEC: flags=%#x", name, flags)
		}
	}
}

func TestAllocatePTYHasNoLingeringSlaveReference(t *testing.T) {
	master, slave, err := allocatePTY()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := (&ptyReader{ReadCloser: master}).Read(make([]byte, 1))
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("master read after last slave close=%v, want mapped EIO/EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("master read blocked: a slave descriptor is still open")
	}
}

func TestPTYControllingFDUsesChildStdioTopology(t *testing.T) {
	if got := controllingTTYFD(false); got != 1 {
		t.Fatalf("stdout/stderr-only PTY Ctty=%d, want child fd 1", got)
	}
	if got := controllingTTYFD(true); got != 0 {
		t.Fatalf("documented PTY-stdin topology Ctty=%d, want child fd 0", got)
	}
}

func TestSetupPTYCaptureUsesOneMergedStream(t *testing.T) {
	masterRead, masterWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	slaveRead, slaveWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = masterWrite.Close()
	_ = slaveRead.Close()
	cmd := exec.Command("/bin/true")
	readers, writers, err := setupPTYCapture(cmd, func() (*os.File, *os.File, error) {
		return masterRead, slaveWrite, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closePipes(readers, writers) })
	if len(readers) != 1 || len(writers) != 1 || readers["log"] != masterRead || writers["log"] != slaveWrite {
		t.Fatalf("PTY streams readers=%v writers=%v", readers, writers)
	}
	if cmd.Stdout != slaveWrite || cmd.Stderr != slaveWrite || cmd.Stdin != nil {
		t.Fatalf("PTY stdio topology stdin=%v stdout=%v stderr=%v", cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
}

func TestPTYSetupFailureFailsClosedAndForcesMergedRecord(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.setupPipesFn = func(*exec.Cmd, bool) (map[string]*os.File, map[string]*os.File, error) {
		t.Fatal("PTY setup failure silently downgraded to pipes")
		return nil, nil, nil
	}
	r.allocatePTYFn = func() (*os.File, *os.File, error) {
		return nil, nil, fmt.Errorf("TCSETS: %w", unix.EIO)
	}
	var openedMerge bool
	r.openOutputsFn = func(dir, id string, merge bool) (map[string]string, map[string]*os.File, error) {
		openedMerge = merge
		path := filepath.Join(dir, id+".log")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		return map[string]string{"log": path}, map[string]*os.File{"log": file}, err
	}
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, PTY: true, Merge: false})
	if record != nil || err == nil || !strings.Contains(err.Error(), "E_RUN_PTY_UNAVAILABLE") {
		t.Fatalf("Launch=(%+v,%v), want fail-closed PTY error", record, err)
	}
	if !openedMerge {
		t.Fatal("PTY request did not force merged output before setup")
	}
	current, getErr := r.Get("RUN-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if !current.Status.Terminal() || current.Merge != true || current.Buffering == "pty" || !containsString(current.ErrorCodes, "E_RUN_PTY_UNAVAILABLE") {
		t.Fatalf("failed PTY left dishonest or nonterminal ledger evidence: %+v", current)
	}
	rawLedger, readErr := os.ReadFile(r.ledger.ledger)
	if readErr != nil || !bytes.Contains(rawLedger, []byte(`"buffering":""`)) || bytes.Contains(rawLedger, []byte(`"buffering":"pty"`)) {
		t.Fatalf("pre-Start ledger buffering was dishonest: contains_blank=%v contains_pty=%v err=%v", bytes.Contains(rawLedger, []byte(`"buffering":""`)), bytes.Contains(rawLedger, []byte(`"buffering":"pty"`)), readErr)
	}
}

func TestPTYAndRealtimeAreRejectedBeforeLedgerAllocation(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, PTY: true, Realtime: true})
	if record != nil || err == nil || !strings.Contains(err.Error(), "E_RUN_ARGUMENT_INVALID") {
		t.Fatalf("Launch=(%+v,%v)", record, err)
	}
	if events, readErr := r.ledger.read(); readErr != nil || len(events) != 0 {
		t.Fatalf("mutually-exclusive tactic refusal wrote ledger events=%+v err=%v", events, readErr)
	}
}

func TestPTYStartFailureKeepsLaunchClassificationAndNeverRecordsPTY(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	r.startFn = func(cmd *exec.Cmd) error {
		attr := cmd.SysProcAttr
		if attr == nil || !attr.UseCgroupFD || attr.CgroupFD != scope.FD() || !attr.Setsid || !attr.Setctty || attr.Ctty != 1 {
			t.Fatalf("SysProcAttr=%+v", attr)
		}
		return syscall.ENOENT
	}
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/missing"}, PTY: true})
	if record != nil || err == nil || !strings.Contains(err.Error(), "E_RUN_LAUNCH_FAILED") || strings.Contains(err.Error(), "E_RUN_PTY_UNAVAILABLE") {
		t.Fatalf("Launch=(%+v,%v), want existing launch classification", record, err)
	}
	current, getErr := r.Get("RUN-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if current.Buffering == "pty" || current.Merge != true {
		t.Fatalf("failed Start recorded dishonest PTY evidence: %+v", current)
	}
}

func TestAllocatePTYFailsClosedWhenTIOCGPTPEERUnavailable(t *testing.T) {
	previous := ioctlGetPTPeerFn
	t.Cleanup(func() { ioctlGetPTPeerFn = previous })

	var masterFD int
	ioctlGetPTPeerFn = func(fd int, flags int) (int, error) {
		masterFD = fd
		if want := unix.O_RDWR | unix.O_NOCTTY | unix.O_CLOEXEC; flags != want {
			t.Errorf("TIOCGPTPEER flags=%#x want %#x", flags, want)
		}
		return 0, unix.ENOTTY
	}
	master, slave, err := allocatePTY()
	if master != nil || slave != nil || !errors.Is(err, unix.ENOTTY) {
		t.Fatalf("allocatePTY=(%v,%v,%v), want nil pair and ENOTTY", master, slave, err)
	}
	if masterFD <= 0 {
		t.Fatal("TIOCGPTPEER seam was not called")
	}
	if _, err := unix.FcntlInt(uintptr(masterFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("master remained open after peer failure: %v", err)
	}
}

func TestAllocatePTYTermiosFailureClosesBothDescriptors(t *testing.T) {
	previousPeer, previousGet := ioctlGetPTPeerFn, ioctlGetTermiosFn
	t.Cleanup(func() {
		ioctlGetPTPeerFn = previousPeer
		ioctlGetTermiosFn = previousGet
	})
	var masterFD, slaveFD int
	ioctlGetPTPeerFn = func(fd int, flags int) (int, error) {
		masterFD = fd
		peer, err := previousPeer(fd, flags)
		slaveFD = peer
		return peer, err
	}
	ioctlGetTermiosFn = func(int, uint) (*unix.Termios, error) {
		return nil, unix.EIO
	}
	master, slave, err := allocatePTY()
	if master != nil || slave != nil || !errors.Is(err, unix.EIO) {
		t.Fatalf("allocatePTY=(%v,%v,%v), want nil pair and EIO", master, slave, err)
	}
	for name, fd := range map[string]int{"master": masterFD, "slave": slaveFD} {
		if fd <= 0 {
			t.Fatalf("%s descriptor was not allocated", name)
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("%s remained open after termios failure: %v", name, err)
		}
	}
}
