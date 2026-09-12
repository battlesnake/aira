//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// ---------------------------------------------------------------------------
// Fixtures: a REAL record store, written through the production writer
// (openConfineDetachJob + job.write) so what these tests read back is what a
// supervisor actually produces, identity cross-checks included.
// ---------------------------------------------------------------------------

// confineLogFixture writes one durable record plus its two capture files. The
// scope id is minted for THIS process, because readConfineDetachRecord refuses a
// record whose supervisor pid disagrees with its directory name -- so a fixture
// that faked the pid would be reported unreadable rather than exercised.
func confineLogFixture(t *testing.T, root, name, owner, stdout, stderr string, mutate func(*ConfineDetachRecord)) ConfineDetachRecord {
	t.Helper()
	scopeID, err := MintConfineScopeID(ConfineRequest{Name: name, Owner: owner})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	job, err := openConfineDetachJob(root, scopeID)
	if err != nil {
		t.Fatalf("open record store: %v", err)
	}
	defer job.close()
	if _, err := job.stdout.WriteString(stdout); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := job.stderr.WriteString(stderr); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	stdoutPath, stderrPath, logPath := job.paths()
	record := ConfineDetachRecord{
		Schema: ConfineDetachSchema, ScopeID: scopeID, Name: name, Owner: owner,
		Supervisor: PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid()), BootID: bootIDOrEmpty()},
		Phase:      ConfineDetachPhaseRunning, StartedAt: nowString(nil),
		StdoutPath: stdoutPath, StderrPath: stderrPath, SupervisorLogPath: logPath,
	}
	if mutate != nil {
		mutate(&record)
	}
	if err := job.write(record); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return record
}

func finished(exit int) func(*ConfineDetachRecord) {
	return func(record *ConfineDetachRecord) {
		record.Terminal, record.EndedAt = true, nowString(nil)
		code := exit
		record.Exit = &code
	}
}

// TestReadConfineLogAnswersIdenticallyRunningAndFinished is the property the
// ticket asks for outright: reading a detached job's log must not depend on
// whether the job is still going. The BYTES and the CURSOR are identical; only
// the job's own state, and therefore `complete`, differ -- which is the one
// difference that is a fact rather than an artefact.
//
// verifies: AIRA-196
func TestReadConfineLogAnswersIdenticallyRunningAndFinished(t *testing.T) {
	const captured = "line one\nline two\n"
	running := t.TempDir()
	confineLogFixture(t, running, "gaterun", "session-a", captured, "", nil)
	done := t.TempDir()
	confineLogFixture(t, done, "gatedone", "session-a", captured, "", finished(0))

	live, err := ReadConfineLog(context.Background(), running, ConfineLogRequest{Selector: "gaterun", Owner: "session-a"})
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	over, err := ReadConfineLog(context.Background(), done, ConfineLogRequest{Selector: "gatedone", Owner: "session-a"})
	if err != nil {
		t.Fatalf("finished: %v", err)
	}
	if string(live.Bytes) != captured || string(over.Bytes) != captured {
		t.Fatalf("bytes differ: running=%q finished=%q, want %q", live.Bytes, over.Bytes, captured)
	}
	if live.Offset != over.Offset || live.NextOffset != over.NextOffset || live.TotalBytes != over.TotalBytes {
		t.Fatalf("cursor differs: running=%+v finished=%+v", live, over)
	}
	if live.State != ConfineDetachRunning || over.State != ConfineDetachFinished {
		t.Fatalf("state: running=%s finished=%s", live.State, over.State)
	}
	if live.Complete {
		t.Fatal("a RUNNING job's capture was reported complete; more bytes can still arrive")
	}
	if !over.Complete {
		t.Fatal("a finished job's fully-read capture was not reported complete")
	}
	if over.Exit == nil || *over.Exit != 0 {
		t.Fatalf("exit=%v, want 0", over.Exit)
	}
}

// TestReadConfineLogWindowsStreamsAndGrep exercises the verb's whole read
// surface against a real store: the two separate captures, the byte window, and
// --grep. The --grep case is the one the owner asked for, and it is checked
// against a MULTI-LINE capture with both matching and non-matching lines --
// a single-line fixture would pass against a filter that returned everything.
//
// verifies: AIRA-196
func TestReadConfineLogWindowsStreamsAndGrep(t *testing.T) {
	root := t.TempDir()
	const out = "starting\nPASS pkg/a\nFAIL pkg/b\nPASS pkg/c\nFAIL pkg/d\n"
	const errText = "warning: slow\nerror: boom\n"
	confineLogFixture(t, root, "gate", "session-a", out, errText, finished(1))

	read := func(request ConfineLogRequest) *ConfineLogChunk {
		t.Helper()
		request.Selector, request.Owner = "gate", "session-a"
		chunk, err := ReadConfineLog(context.Background(), root, request)
		if err != nil {
			t.Fatalf("%+v: %v", request, err)
		}
		return chunk
	}

	if chunk := read(ConfineLogRequest{}); string(chunk.Bytes) != out || chunk.Stream != "out" {
		t.Fatalf("default read: stream=%s bytes=%q", chunk.Stream, chunk.Bytes)
	}
	if chunk := read(ConfineLogRequest{Stream: "err"}); string(chunk.Bytes) != errText || chunk.Stream != "err" {
		t.Fatalf("--stream err: stream=%s bytes=%q", chunk.Stream, chunk.Bytes)
	}
	if chunk := read(ConfineLogRequest{Tail: 12}); string(chunk.Bytes) != "FAIL pkg/d\n" && !strings.HasSuffix(out, string(chunk.Bytes)) {
		t.Fatalf("--tail returned %q, which is not a suffix of the capture", chunk.Bytes)
	}
	// Paging: a --from window continues exactly where the previous one stopped.
	first := read(ConfineLogRequest{})
	if chunk := read(ConfineLogRequest{From: first.NextOffset}); len(chunk.Bytes) != 0 || chunk.Offset != first.NextOffset {
		t.Fatalf("paging past the end returned %q at %d, want an empty window at %d", chunk.Bytes, chunk.Offset, first.NextOffset)
	}

	// --grep. It must FILTER (not merely pass through), report that it did, and
	// leave the cursor describing the underlying file so paging still works.
	grepped := read(ConfineLogRequest{Grep: "^FAIL "})
	if got, want := string(grepped.Bytes), "FAIL pkg/b\nFAIL pkg/d\n"; got != want {
		t.Fatalf("--grep '^FAIL ' returned %q, want %q", got, want)
	}
	if !grepped.Filtered || grepped.Grep != "^FAIL " {
		t.Fatalf("--grep did not report itself: %+v", grepped)
	}
	if grepped.TotalBytes != int64(len(out)) || grepped.NextOffset != int64(len(out)) {
		t.Fatalf("--grep moved the cursor: %+v", grepped)
	}
	// A pattern matching nothing is an empty read, never an error and never the
	// unfiltered window.
	if empty := read(ConfineLogRequest{Grep: "no-such-line"}); len(empty.Bytes) != 0 || !empty.Filtered {
		t.Fatalf("a non-matching --grep returned %q (filtered=%v)", empty.Bytes, empty.Filtered)
	}
	// And --grep composes with --stream rather than being silently stdout-only.
	if chunk := read(ConfineLogRequest{Stream: "err", Grep: "^error"}); string(chunk.Bytes) != "error: boom\n" {
		t.Fatalf("--stream err --grep returned %q", chunk.Bytes)
	}

	// --full is not decorative: it waives the face's observation cap for this
	// read. A capped read of the same capture must be truncated, and the same
	// read with --full must not be. A flag that merely asserted the default
	// would pass the first half of this and fail the second.
	capped := read(ConfineLogRequest{MaxBytes: 8})
	if !capped.Truncated || len(capped.Bytes) != 8 {
		t.Fatalf("a capped read was not truncated: %+v", capped)
	}
	whole := read(ConfineLogRequest{MaxBytes: 8, Full: true})
	if whole.Truncated || string(whole.Bytes) != out {
		t.Fatalf("--full did not waive the observation cap: truncated=%v bytes=%q", whole.Truncated, whole.Bytes)
	}
}

// TestReadConfineLogRefusalsAreNamedNeverEmpty pins the direction every refusal
// must fall in. `confine-log` exists so an agent can decide from a job's output;
// an unreadable capture reported as an empty successful read is the exact
// false-pass that would let it decide from nothing.
//
// verifies: AIRA-196
func TestReadConfineLogRefusalsAreNamedNeverEmpty(t *testing.T) {
	root := t.TempDir()
	record := confineLogFixture(t, root, "gate", "session-a", "hello\n", "", finished(0))

	for _, test := range []struct {
		name     string
		request  ConfineLogRequest
		wantCode string
	}{
		{name: "unknown selector", request: ConfineLogRequest{Selector: "absent", Owner: "session-a"}, wantCode: CodeConfineNotFound},
		{name: "empty selector", request: ConfineLogRequest{Owner: "session-a"}, wantCode: CodeConfineNotFound},
		// A NAME belongs to its owner. Another session's `gate` must not be
		// readable by name, or two sessions using the same names would silently
		// read each other's logs.
		{name: "foreign owner by name", request: ConfineLogRequest{Selector: "gate", Owner: "session-b"}, wantCode: CodeConfineNotFound},
		{name: "unknown stream", request: ConfineLogRequest{Selector: "gate", Owner: "session-a", Stream: "merged"}, wantCode: "E_CONFINE_ARGUMENT_INVALID"},
		{name: "unparseable grep", request: ConfineLogRequest{Selector: "gate", Owner: "session-a", Grep: "("}, wantCode: "E_CONFINE_ARGUMENT_INVALID"},
		{name: "offset past the end", request: ConfineLogRequest{Selector: "gate", Owner: "session-a", From: 9999}, wantCode: "E_CONFINE_ARGUMENT_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			chunk, err := ReadConfineLog(context.Background(), root, test.request)
			if err == nil {
				t.Fatalf("accepted, returning %+v", chunk)
			}
			if chunk != nil {
				t.Fatalf("a refusal also returned a chunk: %+v", chunk)
			}
			if code := errorCodeOf(err); code != test.wantCode {
				t.Fatalf("code=%s (%v), want %s", code, err, test.wantCode)
			}
		})
	}

	// A SCOPE ID is globally addressable, exactly as `confine --status`'s is:
	// it was named explicitly and is unique, so refusing it on ownership would be
	// obstruction rather than safety.
	if _, err := ReadConfineLog(context.Background(), root, ConfineLogRequest{Selector: record.ScopeID, Owner: "session-b"}); err != nil {
		t.Fatalf("a scope id was refused to another owner: %v", err)
	}

	// A capture the record names but that cannot be opened is UNEVALUATED. This
	// is the case that must never degrade into an empty successful read.
	if err := os.Remove(record.StdoutPath); err != nil {
		t.Fatalf("remove capture: %v", err)
	}
	chunk, err := ReadConfineLog(context.Background(), root, ConfineLogRequest{Selector: "gate", Owner: "session-a"})
	if err == nil {
		t.Fatalf("a missing capture read as %q with no error", chunk.Bytes)
	}
	if code := errorCodeOf(err); code != CodeConfineLogUnavailable {
		t.Fatalf("code=%s (%v), want %s", code, err, CodeConfineLogUnavailable)
	}
}

// TestReadConfineLogFollowEndsOnADeadSupervisor pins that --follow terminates.
// A follow that only stopped on `finished` would hang forever against the very
// job an operator reaches for it on: one whose supervisor was killed.
//
// verifies: AIRA-196
func TestReadConfineLogFollowEndsOnADeadSupervisor(t *testing.T) {
	root := t.TempDir()
	// A record naming a supervisor pid that cannot be alive: the scope id is
	// minted for this process, so the record is readable, and the PID identity is
	// then overwritten with a start tick that can never match. processLive reads
	// that as DEAD (a different process now holds the pid), not unevaluated.
	confineLogFixture(t, root, "ghost", "session-a", "partial output\n", "", func(record *ConfineDetachRecord) {
		record.Supervisor.StartTick = ^uint64(0) >> 1
	})
	previous := confineLogFollowInterval
	confineLogFollowInterval = time.Millisecond
	t.Cleanup(func() { confineLogFollowInterval = previous })

	done := make(chan *ConfineLogChunk, 1)
	go func() {
		chunk, err := ReadConfineLog(context.Background(), root, ConfineLogRequest{Selector: "ghost", Owner: "session-a", Follow: true})
		if err != nil {
			done <- nil
			return
		}
		done <- chunk
	}()
	select {
	case chunk := <-done:
		if chunk == nil {
			t.Fatal("--follow against a dead supervisor errored instead of returning the partial capture")
		}
		if chunk.State != ConfineDetachOutcomeUnknown {
			t.Fatalf("state=%s, want outcome-unknown", chunk.State)
		}
		if string(chunk.Bytes) != "partial output\n" {
			t.Fatalf("bytes=%q", chunk.Bytes)
		}
		if chunk.Complete {
			t.Fatal("an outcome-unknown capture was reported complete")
		}
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("--follow never returned against a supervisor that is gone")
	}
}

// TestConfineInputPlaneIDFitsTheUnixSocketBudget pins the reason the input
// socket is NOT named after the scope id. A Unix socket path is capped at 107
// bytes and a confine scope id carries a name, a pid, a nonce AND an owner;
// naming the socket after it would make --stdin-connect fail on ordinary jobs,
// on the path where the failure is a refused launch.
//
// verifies: AIRA-196
func TestConfineInputPlaneIDFitsTheUnixSocketBudget(t *testing.T) {
	id := confineInputPlaneID(4194304)
	if id != "confine-4194304" {
		t.Fatalf("plane id=%q", id)
	}
	const runtimeDir = "/run/user/4294967295/aira/inputs/"
	full := len(runtimeDir) + len(id) + 1 + 24 + len(".sock")
	if full > unixSocketPathMax {
		t.Fatalf("a plane path is %d bytes, over the %d-byte Unix socket limit", full, unixSocketPathMax)
	}
	// And the id it replaces would NOT have fitted, which is why it exists.
	scopeID := confineScopeID("merge-gate-worktree-aira196", "session-abcdef012345")
	if oversized := len(runtimeDir) + len(scopeID) + 1 + 24 + len(".sock"); oversized <= unixSocketPathMax {
		t.Fatalf("a scope-id-named socket path is only %d bytes; if that now fits, the short id has lost its reason to exist", oversized)
	}
}

// confineInputRuntimeDir returns a runtime directory short enough for a Unix
// socket path. t.TempDir() is NOT usable here and that is a real constraint
// rather than a test detail: its name embeds the test's own name, which pushed
// the socket path past the 107-byte kernel limit and made the supervisor refuse
// the launch (observed, first run). Production's runtime directory is
// $XDG_RUNTIME_DIR/aira/<id>, which is short; the fixture has to be too.
//
// It lives under ~/tmp rather than /tmp because this box wipes /tmp on restart.
func confineInputRuntimeDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "tmp", "a196")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("runtime root: %v", err)
	}
	base, err := os.MkdirTemp(root, "r")
	if err != nil {
		t.Fatalf("runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

// superviseHandle is runSuperviseSubprocess's non-blocking twin: it returns once
// the supervisor has reported and been acknowledged, so a test can INTERACT with
// the running job (which is the whole point of confine-input) instead of only
// inspecting the wreckage afterwards.
type superviseHandle struct {
	cmd   *exec.Cmd
	ready confineDetachReady
}

func startSuperviseSubprocess(t *testing.T, request ConfineRequest) *superviseHandle {
	t.Helper()
	control, err := writeControlValue(request.DetachStateDir, "confine-detach-*.ctrl", request)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	cmd := exec.Command(os.Args[0], "__confine-supervise", "--control", control, "--ready-fd", "3", "--ack-fd", "4")
	cmd.Env = append(os.Environ(), fakeSupervisorEnv+"=")
	cmd.ExtraFiles = []*os.File{readyW, ackR}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	// The supervisor is genuinely detached, so a test that fails midway must not
	// leave it running against the shared slice.
	t.Cleanup(func() {
		if cmd.ProcessState == nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	_ = readyW.Close()
	_ = ackR.Close()
	decoded := make(chan confineDetachReady, 1)
	go func() {
		var message confineDetachReady
		_ = json.NewDecoder(readyR).Decode(&message)
		_ = readyR.Close()
		decoded <- message
	}()
	var message confineDetachReady
	select {
	case message = <-decoded:
	case <-testdeadline.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the supervisor reported nothing")
	}
	if _, err := ackW.Write([]byte{1}); err != nil && !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("acknowledge: %v", err)
	}
	_ = ackW.Close()
	return &superviseHandle{cmd: cmd, ready: message}
}

func (h *superviseHandle) wait(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("wait: %v", err)
		}
	case <-testdeadline.After(30 * time.Second):
		_ = h.cmd.Process.Kill()
		t.Fatal("the supervisor never exited")
	}
}

// errorCodeOf extracts the leading stable code from a runner error the same way
// the faces do, so these tests assert on the code an operator would actually
// see rather than on message text.
func errorCodeOf(err error) string {
	if err == nil {
		return ""
	}
	var inputErr *RunInputError
	if errors.As(err, &inputErr) {
		return inputErr.Code
	}
	message := err.Error()
	if index := strings.IndexByte(message, ':'); index >= 0 {
		message = message[:index]
	}
	return message
}

// ---------------------------------------------------------------------------
// The end-to-end stdin-conduit tests. These run the PRODUCTION supervisor
// against a REAL cgroup scope, because the whole property under test -- what a
// detached job's fd 0 actually is -- exists only once a child has been execve'd.
// ---------------------------------------------------------------------------

// TestDetachedConfineStdinIsDevnullWithoutStdinConnect is the regression test
// the ticket demands, and it is a HANG test as much as a field test.
//
// The job is `cat`. With stdin on /dev/null it reads EOF at once, prints its
// marker and exits, so the supervisor returns and the assertions run. If a
// future change ever wires the AIRA-196 pipe unconditionally, `cat` blocks on a
// pipe nobody is writing to, the supervisor never returns, and this test fails
// on its deadline -- which is exactly the production failure it is standing in
// for. That is why the job reads stdin rather than merely being inspected.
//
// verifies: AIRA-196
func TestDetachedConfineStdinIsDevnullWithoutStdinConnect(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	runtimeDir := confineInputRuntimeDir(t)
	// Started non-blocking and waited on with a DEADLINE on purpose: if the pipe
	// is ever wired unconditionally, `cat` blocks forever and the failure must
	// surface as a bounded "the supervisor never exited" rather than as the whole
	// package timing out ten minutes later.
	supervisor := startSuperviseSubprocess(t, ConfineRequest{
		Slice: parent, Name: "noinput", Owner: "session-test",
		Argv:           []string{"/bin/sh", "-c", "cat; echo '[stdin reached EOF]'"},
		DetachStateDir: state, RuntimeDir: runtimeDir, SelfPath: os.Args[0],
		MemoryReserve: 64 << 20, MemoryReservePinned: true,
	})
	if supervisor.ready.ScopeID == "" {
		t.Fatalf("the supervisor never reached the launch gate: %+v", supervisor.ready)
	}
	supervisor.wait(t)
	record := soleDetachRecord(t, state)
	if record.StdinConnect {
		t.Fatal("a job launched WITHOUT --stdin-connect recorded stdin_connect=true")
	}
	if record.InputSocket != "" {
		t.Fatalf("a job launched WITHOUT --stdin-connect recorded an input socket %q", record.InputSocket)
	}
	// Nothing was created on disk either: the absence must be structural, not
	// merely unrecorded.
	if entries, err := os.ReadDir(filepath.Join(runtimeDir, "inputs")); err == nil && len(entries) > 0 {
		t.Fatalf("a job that did not opt in left %d socket(s) behind: %v", len(entries), entries)
	}
	if record.Exit == nil || *record.Exit != 0 {
		t.Fatalf("exit=%v, want 0; the job did not run to completion", record.Exit)
	}
	chunk, err := ReadConfineLog(context.Background(), state, ConfineLogRequest{Selector: "noinput", Owner: "session-test"})
	if err != nil {
		t.Fatalf("confine-log: %v", err)
	}
	if !strings.Contains(string(chunk.Bytes), "[stdin reached EOF]") {
		t.Fatalf("the job's stdin did not reach EOF; captured %q", chunk.Bytes)
	}

	// And confine-input refuses it BY NAME rather than dialling anything.
	_, inputErr := ConfineInput(context.Background(), state, ConfineInputRequest{
		Selector: "noinput", Owner: "session-test", Reader: strings.NewReader("late\n"),
	})
	if inputErr == nil {
		t.Fatal("confine-input accepted a job that has no stdin conduit")
	}
	if code := errorCodeOf(inputErr); code != "E_RUN_INPUT_UNAVAILABLE" {
		t.Fatalf("code=%s (%v), want E_RUN_INPUT_UNAVAILABLE", code, inputErr)
	}
	if !strings.Contains(inputErr.Error(), "--stdin-connect") {
		t.Fatalf("the refusal does not name the flag that would have enabled it: %v", inputErr)
	}
}

// TestDetachedConfineStdinConnectDeliversInjectedBytes is the opt-in half: with
// --stdin-connect the job gets a real stdin, `confine-input` writes to it by
// HANDLE (never by path), --close EOFs it, and the bytes come back out of
// `confine-log`. It exercises the production supervisor, the production socket,
// and both new verbs against one another.
//
// verifies: AIRA-196
func TestDetachedConfineStdinConnectDeliversInjectedBytes(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	runtimeDir := confineInputRuntimeDir(t)
	supervisor := startSuperviseSubprocess(t, ConfineRequest{
		Slice: parent, Name: "withinput", Owner: "session-test",
		// `cat` echoes whatever is injected; it exits only when stdin is CLOSED,
		// so the supervisor's own exit proves --close reached the child.
		Argv:           []string{"/bin/sh", "-c", "cat"},
		DetachStateDir: state, RuntimeDir: runtimeDir, SelfPath: os.Args[0],
		StdinConnect:  true,
		MemoryReserve: 64 << 20, MemoryReservePinned: true,
	})
	if supervisor.ready.ScopeID == "" {
		t.Fatalf("the supervisor never reached the launch gate: %+v", supervisor.ready)
	}

	// Wait for the durable record to say `running` AND name a socket: that pair
	// is precisely what confine-input resolves against, so waiting on anything
	// else would test a different discovery path than production uses.
	deadline := time.Now().Add(30 * time.Second)
	var record ConfineDetachRecord
	for {
		record = soleDetachRecord(t, state)
		if record.Phase == ConfineDetachPhaseRunning && record.InputSocket != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never became running with a socket: %+v", record)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !record.StdinConnect {
		t.Fatal("--stdin-connect did not record stdin_connect=true")
	}
	if _, err := os.Stat(record.InputSocket); err != nil {
		t.Fatalf("the recorded socket does not exist: %v", err)
	}

	// Addressed by the job's own NAME. Nothing here knows a path.
	result, err := ConfineInput(context.Background(), state, ConfineInputRequest{
		Selector: "withinput", Owner: "session-test",
		Reader: strings.NewReader("injected line\n"), Close: true,
	})
	if err != nil {
		t.Fatalf("confine-input: %v", err)
	}
	if result.Accepted != int64(len("injected line\n")) || !result.Closed {
		t.Fatalf("accepted=%d closed=%v, want %d/true", result.Accepted, result.Closed, len("injected line\n"))
	}
	if result.ScopeID != record.ScopeID {
		t.Fatalf("result names scope %q, want %q", result.ScopeID, record.ScopeID)
	}

	supervisor.wait(t)
	final := soleDetachRecord(t, state)
	if final.Exit == nil || *final.Exit != 0 {
		t.Fatalf("exit=%v, want 0: --close did not EOF the child's stdin", final.Exit)
	}
	chunk, err := ReadConfineLog(context.Background(), state, ConfineLogRequest{Selector: "withinput", Owner: "session-test"})
	if err != nil {
		t.Fatalf("confine-log: %v", err)
	}
	if !strings.Contains(string(chunk.Bytes), "injected line") {
		t.Fatalf("the injected bytes never reached the job's stdin; captured %q", chunk.Bytes)
	}
	if !chunk.Complete {
		t.Fatalf("a finished job's capture was not complete: %+v", chunk)
	}
	// The socket is not left behind on a durable path once the supervisor is gone.
	if _, statErr := os.Stat(record.InputSocket); statErr == nil {
		t.Fatalf("the input socket %s outlived its supervisor", record.InputSocket)
	}
}

// TestDetachedConfineStdinConnectRefusesWithNoRuntimeDirectory pins the
// fail-closed direction. A launcher that asked for a writable stdin and was
// silently given /dev/null would sit forever waiting to feed a conduit that does
// not exist, so an unusable runtime directory REFUSES THE LAUNCH instead.
//
// verifies: AIRA-196
func TestDetachedConfineStdinConnectRefusesWithNoRuntimeDirectory(t *testing.T) {
	state := t.TempDir()
	outcome := runSuperviseSubprocess(t, ConfineRequest{
		Slice: "aira-nonexistent-" + strconv.Itoa(os.Getpid()) + ".slice",
		Name:  "noruntime", Owner: "session-test", Argv: []string{"/bin/true"},
		DetachStateDir: state, SelfPath: os.Args[0],
		StdinConnect: true, RuntimeDir: "",
	}, true)
	if outcome.exitCode == 0 {
		t.Fatal("--stdin-connect with no runtime directory was supervised as a success")
	}
	if !strings.Contains(outcome.message.Error, "--stdin-connect") {
		t.Fatalf("the launcher was not told which option failed: %+v", outcome.message)
	}
	record := soleDetachRecord(t, state)
	if !record.Terminal {
		t.Fatal("a refused --stdin-connect launch left a non-terminal record")
	}
	if record.StdinConnect || record.InputSocket != "" {
		t.Fatalf("a refused conduit was recorded as established: %+v", record)
	}
	if record.Exit != nil {
		t.Fatalf("a refused launch fabricated exit %d", *record.Exit)
	}
}
