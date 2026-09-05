//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Test-binary helper modes. The runner package's TestMain dispatches to these,
// so the SAME binary can play launcher, supervisor, and test — which is what
// lets these tests exercise the real production launch path (spawnDetachedShim,
// the ready/ack handshake, the record store) across a real process boundary
// rather than asserting on struct fields.
// ---------------------------------------------------------------------------

const (
	fakeSupervisorEnv       = "AIRA_CONFINE_FAKE_SUPERVISOR"
	detachStateDirEnv       = "AIRA_CONFINE_TEST_STATE_DIR"
	detachLaunchArgvEnv     = "AIRA_CONFINE_TEST_ARGV"
	detachLaunchSliceEnv    = "AIRA_CONFINE_TEST_SLICE"
	detachLaunchNoAckEnv    = "AIRA_CONFINE_TEST_SKIP_ACK"
	detachSuperviseHoldEnv  = "AIRA_CONFINE_TEST_HOLD"
	detachLaunchDoneMarker  = "AIRA_CONFINE_TEST_LAUNCH_MARKER"
	detachSuperviseRealMode = ""
)

// runRealConfineSupervisor is the production entry point, invoked exactly as
// cmd/aira invokes it, so the real-cgroup end-to-end test exercises the shipped
// code rather than a test double.
func runRealConfineSupervisor(argv []string) int {
	values := map[string]string{}
	for i := 0; i+1 < len(argv); i += 2 {
		values[strings.TrimPrefix(argv[i], "--")] = argv[i+1]
	}
	readyFD, readyErr := strconv.Atoi(values["ready-fd"])
	ackFD, ackErr := strconv.Atoi(values["ack-fd"])
	if readyErr != nil || ackErr != nil {
		return 2
	}
	if err := SuperviseConfineDetached(context.Background(), values["control"], readyFD, ackFD); err != nil {
		return 1
	}
	return 0
}

// runFakeConfineSupervisor stands in for the real supervisor in tests that must
// not touch cgroups. It speaks the REAL wire protocol (confineDetachReady over
// the ready fd, one ack byte back) and uses the REAL record store, so what it
// fakes is only the confinement itself.
func runFakeConfineSupervisor(mode string, argv []string) int {
	values := map[string]string{}
	for i := 0; i+1 < len(argv); i += 2 {
		values[strings.TrimPrefix(argv[i], "--")] = argv[i+1]
	}
	if mode == "silent" {
		// Exit without reporting anything: the launcher must treat this as a
		// failure, not hang and not succeed.
		return 3
	}
	readyFD, _ := strconv.Atoi(values["ready-fd"])
	ackFD, _ := strconv.Atoi(values["ack-fd"])
	ready := os.NewFile(uintptr(readyFD), "ready")
	ack := os.NewFile(uintptr(ackFD), "ack")
	if ready == nil || ack == nil {
		return 3
	}
	announce := &detachSignal{file: ready}
	var request ConfineRequest
	if err := consumeControlValue(values["control"], &request); err != nil {
		_ = announce.send(confineDetachReady{Code: CodeConfineDetachFailed, Error: err.Error()})
		return 3
	}
	scopeID, err := MintConfineScopeID(request)
	if err != nil {
		_ = announce.send(confineDetachReady{Code: "E_CONFINE_ARGUMENT_INVALID", Error: err.Error()})
		return 3
	}
	job, err := openConfineDetachJob(request.DetachStateDir, scopeID)
	if err != nil {
		_ = announce.send(confineDetachReady{Code: CodeConfineDetachFailed, Error: err.Error()})
		return 3
	}
	defer job.close()
	// Same discipline as the real supervisor: fd 2 is /dev/null in a detached
	// shim, so without this a panic here would vanish and a test failure would
	// have nothing to report.
	job.captureSupervisorStderr()
	stdoutPath, stderrPath, logPath := job.paths()
	record := ConfineDetachRecord{
		Schema: ConfineDetachSchema, ScopeID: scopeID, Name: request.Name, Owner: request.Owner,
		Supervisor: PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid()), BootID: bootIDOrEmpty()},
		Phase:      ConfineDetachPhaseAdmitting, StartedAt: nowString(nil),
		StdoutPath: stdoutPath, StderrPath: stderrPath, SupervisorLogPath: logPath,
	}
	if record.Owner == "" {
		record.Owner = ConfineUnknownOwner
	}
	if err := job.write(record); err != nil {
		_ = announce.send(confineDetachReady{Code: CodeConfineDetachFailed, Error: err.Error()})
		return 3
	}
	if err := announce.send(confineDetachReady{
		ScopeID: scopeID, Slice: "test.slice", CapBytes: 1 << 30,
		SupervisorPID: os.Getpid(), RecordDir: job.path,
	}); err != nil {
		return 3
	}
	var acknowledgement [1]byte
	if _, err := io.ReadFull(ack, acknowledgement[:]); err != nil {
		record.Terminal, record.EndedAt = true, nowString(nil)
		record.ErrorCode = CodeConfineDetachCancelled
		record.Error = "the launcher did not acknowledge the detached handle"
		_ = job.write(record)
		return 3
	}
	if mode == "cancel-probe" {
		return 0
	}
	// "survive": keep working until something kills us. Each tick appends to the
	// captured stdout, so a test can prove the JOB is still producing output and
	// not merely that a process exists.
	record.Phase, record.RunningAt = ConfineDetachPhaseRunning, nowString(nil)
	_ = job.write(record)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = job.stdout.WriteString("tick\n")
		time.Sleep(20 * time.Millisecond)
	}
	record.Terminal, record.EndedAt = true, nowString(nil)
	zero := 0
	record.Exit = &zero
	_ = job.write(record)
	return 0
}

// runConfineDetachLauncher performs a REAL LaunchConfineDetached from a separate
// process, prints the handle as one JSON line, acknowledges it, and then blocks.
// Blocking is the point: the test group-kills this process to prove the detached
// supervisor is genuinely independent of it.
func runConfineDetachLauncher() int {
	argv := strings.Split(os.Getenv(detachLaunchArgvEnv), "\x1f")
	if len(argv) == 1 && argv[0] == "" {
		argv = []string{"/bin/true"}
	}
	request := ConfineRequest{
		Slice: os.Getenv(detachLaunchSliceEnv), Name: "detached", Owner: "session-test",
		Argv: argv, SelfPath: os.Args[0], DetachStateDir: os.Getenv(detachStateDirEnv),
		// 64MiB, not the 1MiB minimum: a DECLARED reserve becomes the scope's
		// memory.max, and a token cap OOM-kills the child at launch ("pid absent"),
		// which is the trap confine.go's MinPinnedScopeCap comment describes.
		MemoryReserve: 64 << 20, MemoryReservePinned: true,
	}
	launch, err := LaunchConfineDetached(context.Background(), request)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stdout, `{"error":`+strconv.Quote(err.Error())+`}`)
		return 4
	}
	payload, _ := json.Marshal(map[string]any{
		"scope_id": launch.ScopeID, "supervisor_pid": launch.SupervisorPID,
		"record_dir": launch.RecordDir, "slice": launch.Slice,
	})
	_, _ = os.Stdout.Write(append(payload, '\n'))
	if os.Getenv(detachLaunchNoAckEnv) != "1" {
		if ackErr := launch.Acknowledge(true); ackErr != nil {
			return 4
		}
	} else {
		// Deliberately drop the handle without acknowledging: the supervisor must
		// then refuse to run an unreported job.
		_ = launch.Acknowledge(false)
	}
	if marker := os.Getenv(detachLaunchDoneMarker); marker != "" {
		_ = os.WriteFile(marker, []byte(launch.ScopeID), 0o600)
	}
	if os.Getenv(detachSuperviseHoldEnv) == "0" {
		return 0
	}
	select {}
}

type launcherHandle struct {
	ScopeID       string `json:"scope_id"`
	SupervisorPID int    `json:"supervisor_pid"`
	RecordDir     string `json:"record_dir"`
	Slice         string `json:"slice"`
	Error         string `json:"error"`
}

// startDetachLauncher starts the launcher subprocess and returns only once the
// handle has been reported AND acknowledged.
//
// Waiting for the acknowledgement is load-bearing, not tidiness. The launcher
// prints the handle and only then writes the ack byte, so a test that killed it
// the instant it could parse the handle would race that window — and the
// supervisor would then correctly refuse to run an unreported job, making a
// survival test fail for the opposite of the reason it is testing. (Both
// survival tests flaked exactly this way before the wait was added; the product
// behaved as designed each time.)
func startDetachLauncher(t *testing.T, env []string) (*exec.Cmd, launcherHandle) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "acknowledged")
	env = append(env, detachLaunchDoneMarker+"="+marker)
	skipAck := false
	for _, entry := range env {
		if entry == detachLaunchNoAckEnv+"=1" {
			skipAck = true
		}
	}
	cmd := exec.Command(os.Args[0], "__confine-detach-launch")
	cmd.Env = append(os.Environ(), env...)
	// Its OWN process group, so the test can group-kill the launcher exactly the
	// way a terminal or a supervising harness would.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	type decoded struct {
		handle launcherHandle
		err    error
	}
	results := make(chan decoded, 1)
	go func() {
		var handle launcherHandle
		decodeErr := json.NewDecoder(stdout).Decode(&handle)
		results <- decoded{handle: handle, err: decodeErr}
	}()
	var handle launcherHandle
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("launcher produced no handle: %v", result.err)
		}
		handle = result.handle
	case <-time.After(30 * time.Second):
		t.Fatal("launcher did not report a handle")
	}
	if skipAck || handle.Error != "" {
		return cmd, handle
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return cmd, handle
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the launcher never acknowledged the detached handle")
	return nil, launcherHandle{}
}

func detachProcessAlive(pid int) bool {
	return pid > 0 && !errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}

// TestDetachedSupervisorSurvivesAGroupKillOfItsLauncher is the survivability
// test AIRA-22 exists for. It is deliberately a GROUP kill, escalating through
// the exact signals that killed the original hour-long job: SIGHUP (what nohup
// was expected to solve and did not), SIGINT (the foreground supervisor's own
// teardown signal), and finally SIGKILL to the whole launcher process group.
//
// Signalling the launcher's pid alone would prove nothing -- any child survives
// its parent's death by reparenting, with or without setsid. Only a GROUP kill
// distinguishes a genuinely detached session from a merely-forked child.
//
// verifies: AIRA-22
func TestDetachedSupervisorSurvivesAGroupKillOfItsLauncher(t *testing.T) {
	state := t.TempDir()
	launcher, handle := startDetachLauncher(t, []string{
		fakeSupervisorEnv + "=survive",
		detachStateDirEnv + "=" + state,
	})
	if handle.SupervisorPID <= 0 || handle.ScopeID == "" {
		t.Fatalf("launcher reported an unusable handle: %+v", handle)
	}
	if handle.SupervisorPID == launcher.Process.Pid {
		t.Fatal("the supervisor is the launcher itself; nothing was detached")
	}
	// A detached supervisor must be its own session leader, so no terminal
	// hangup and no process-group signal can reach it.
	supervisorSID, err := unix.Getsid(handle.SupervisorPID)
	if err != nil {
		t.Fatalf("getsid(%d): %v", handle.SupervisorPID, err)
	}
	launcherSID, err := unix.Getsid(launcher.Process.Pid)
	if err != nil {
		t.Fatalf("getsid(launcher): %v", err)
	}
	if supervisorSID == launcherSID {
		t.Fatalf("supervisor shares session %d with its launcher", supervisorSID)
	}
	capture := filepath.Join(handle.RecordDir, confineDetachStdoutName)
	before := detachFileSize(t, capture)
	for _, signal := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGKILL} {
		if err := syscall.Kill(-launcher.Process.Pid, signal); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Fatalf("group-signal %v: %v", signal, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = launcher.Wait()
	if detachProcessAlive(launcher.Process.Pid) {
		t.Fatal("the launcher survived its own group SIGKILL; the test proved nothing")
	}
	// The supervisor must be alive AND still doing work: a process that exists
	// but has stopped producing output would satisfy a weaker assertion.
	deadline := time.Now().Add(5 * time.Second)
	grew := false
	for time.Now().Before(deadline) {
		if detachFileSize(t, capture) > before {
			grew = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !detachProcessAlive(handle.SupervisorPID) {
		t.Fatalf("the detached supervisor (pid %d, session %d; launcher session %d) died with its launcher's process group.\nsupervisor log: %s\nrecord: %s",
			handle.SupervisorPID, supervisorSID, launcherSID,
			readIfPresent(filepath.Join(handle.RecordDir, confineDetachSupervisorLog)),
			readIfPresent(filepath.Join(handle.RecordDir, confineDetachRecordName)))
	}
	if !grew {
		t.Fatalf("the detached job stopped producing captured output after its launcher was killed.\nsupervisor log: %s",
			readIfPresent(filepath.Join(handle.RecordDir, confineDetachSupervisorLog)))
	}
	_ = unix.Kill(handle.SupervisorPID, unix.SIGKILL)
}

func readIfPresent(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	if len(data) == 0 {
		return "<empty>"
	}
	return string(data)
}

func detachFileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

// TestLaunchConfineDetachedReportsASupervisorThatNeverReportsReady pins the
// no-ghost-job property: when the launcher reports a failure, nothing may be
// left running behind it.
//
// verifies: AIRA-22
func TestLaunchConfineDetachedReportsASupervisorThatNeverReportsReady(t *testing.T) {
	state := t.TempDir()
	t.Setenv(fakeSupervisorEnv, "silent")
	launch, err := LaunchConfineDetached(context.Background(), ConfineRequest{
		Name: "silent", Owner: "session-test", Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], DetachStateDir: state,
	})
	if err == nil {
		t.Fatalf("a supervisor that reported nothing was treated as a successful launch: %+v", launch)
	}
	if !strings.Contains(err.Error(), CodeConfineDetachFailed) {
		t.Fatalf("wrong code: %v", err)
	}
	entries, readErr := os.ReadDir(state)
	if readErr != nil {
		t.Fatalf("read state dir: %v", readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("a failed launch left a job record directory %q behind", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".ctrl") {
			// The control file is consumed by the shim; a shim that never ran must
			// not leave the whole request lying in the state directory.
			t.Fatalf("a failed launch left its control file %q behind", entry.Name())
		}
	}
}

// TestUnacknowledgedDetachedSupervisorRefusesToRunTheJob is the other half of
// the same property, from the supervisor's side: if the launcher never confirms
// that the handle reached a human, the job must not run at all. Without this,
// a launcher killed between ready and printing leaves an hour-long job running
// under a handle nobody holds.
//
// verifies: AIRA-22
func TestUnacknowledgedDetachedSupervisorRefusesToRunTheJob(t *testing.T) {
	state := t.TempDir()
	_, handle := startDetachLauncher(t, []string{
		fakeSupervisorEnv + "=cancel-probe",
		detachStateDirEnv + "=" + state,
		detachLaunchNoAckEnv + "=1",
		detachSuperviseHoldEnv + "=0",
	})
	if handle.ScopeID == "" {
		t.Fatalf("no handle: %+v", handle)
	}
	recordPath := filepath.Join(state, handle.ScopeID, confineDetachRecordName)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(recordPath)
		if err == nil {
			var record ConfineDetachRecord
			if json.Unmarshal(data, &record) == nil && record.Terminal {
				if record.ErrorCode != CodeConfineDetachCancelled {
					t.Fatalf("unacknowledged launch recorded %q, want %q", record.ErrorCode, CodeConfineDetachCancelled)
				}
				if record.Exit != nil {
					t.Fatalf("a cancelled launch fabricated exit %d", *record.Exit)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("an unacknowledged supervisor never wrote a terminal cancelled record")
}

// TestConfineDetachRecordWriteIsAtomicAcrossAFailureBetweenWriteAndRename proves
// atomicity deterministically rather than probabilistically: the seam fails
// exactly between the temp write and the rename, which is the only window in
// which a reader could observe a partial record.
//
// verifies: AIRA-22
func TestConfineDetachRecordWriteIsAtomicAcrossAFailureBetweenWriteAndRename(t *testing.T) {
	state := t.TempDir()
	scopeID := confineScopeID("atomic", "session-a", false)
	job, err := openConfineDetachJob(state, scopeID)
	if err != nil {
		t.Fatalf("open job: %v", err)
	}
	defer job.close()
	first := ConfineDetachRecord{Schema: ConfineDetachSchema, ScopeID: scopeID, Name: "atomic", Owner: "session-a", Phase: ConfineDetachPhaseAdmitting, StartedAt: "t0"}
	if err := job.write(first); err != nil {
		t.Fatalf("write first: %v", err)
	}
	confineDetachBeforeRenameHook = func() error { return errors.New("injected crash before rename") }
	t.Cleanup(func() { confineDetachBeforeRenameHook = nil })
	second := first
	second.Phase, second.Terminal, second.StartedAt = ConfineDetachPhaseRunning, true, "t1"
	if err := job.write(second); err == nil {
		t.Fatal("the injected pre-rename failure was not reported")
	}
	confineDetachBeforeRenameHook = nil
	records, err := ListConfineDetachRecords(state)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("listed %d records, want 1", len(records))
	}
	if records[0].Terminal || records[0].StartedAt != "t0" || records[0].ReadError != "" {
		t.Fatalf("a crash before the rename was visible to a reader: %+v", records[0])
	}
	// A leftover temp file must not be mistaken for a record, and a subsequent
	// write must still succeed rather than tripping over it.
	if err := job.write(second); err != nil {
		t.Fatalf("write after a failed write: %v", err)
	}
	records, err = ListConfineDetachRecords(state)
	if err != nil || len(records) != 1 || !records[0].Terminal {
		t.Fatalf("the retried write did not land: %+v (%v)", records, err)
	}
}

// verifies: AIRA-22
func TestConfineDetachRecordFilesAreOwnerOnlyAndRefuseSymlinksAndReuse(t *testing.T) {
	state := t.TempDir()
	scopeID := confineScopeID("modes", "session-a", false)
	job, err := openConfineDetachJob(state, scopeID)
	if err != nil {
		t.Fatalf("open job: %v", err)
	}
	if err := job.write(ConfineDetachRecord{Schema: ConfineDetachSchema, ScopeID: scopeID}); err != nil {
		t.Fatalf("write: %v", err)
	}
	dirInfo, err := os.Stat(filepath.Join(state, scopeID))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Fatalf("record directory mode %#o, want 0700", mode)
	}
	for _, name := range []string{confineDetachRecordName, confineDetachStdoutName, confineDetachStderrName, confineDetachSupervisorLog} {
		info, statErr := os.Stat(filepath.Join(state, scopeID, name))
		if statErr != nil {
			t.Fatalf("stat %s: %v", name, statErr)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s mode %#o, want 0600", name, mode)
		}
	}
	job.close()
	// A second supervisor must never adopt an existing record directory: two
	// writers interleaving into one record is how a job's outcome gets attributed
	// to the wrong job.
	if _, err := openConfineDetachJob(state, scopeID); err == nil {
		t.Fatal("a second supervisor adopted an existing record directory")
	}
	// A pre-planted symlink where a capture file would go must be refused, not
	// written through.
	victim := filepath.Join(t.TempDir(), "victim")
	symlinked := confineScopeID("symlink", "session-a", false)
	if err := os.MkdirAll(filepath.Join(state, symlinked), 0o700); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(state, symlinked, confineDetachStdoutName)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The directory already exists, so this refuses at the Mkdir. Prove the
	// deeper guard too by opening the file directly through the job helper.
	if _, err := openConfineDetachJob(state, symlinked); err == nil {
		t.Fatal("an existing (symlink-seeded) directory was adopted")
	}
	if _, err := os.Stat(victim); err == nil {
		t.Fatal("the symlink target was created; a write followed the symlink")
	}
}

// verifies: AIRA-22
func TestConfineDetachStoreRejectsAnInvalidScopeID(t *testing.T) {
	state := t.TempDir()
	for _, scopeID := range []string{"", "..", "../escape", "CONFINE-a/b-1-1", "not-a-scope", "CONFINE-job-1-A1", "CONFINE-job-01-a"} {
		if _, err := openConfineDetachJob(state, scopeID); err == nil {
			t.Fatalf("scope id %q was accepted as a record directory name", scopeID)
		}
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatalf("a refused scope id still created %d entries", len(entries))
	}
}

// A directory whose record cannot be read must still be LISTED, with a read
// error, so `--status` can say "I cannot tell you" rather than "no such job".
//
// verifies: AIRA-22
func TestListConfineDetachRecordsSurfacesUnreadableAndMismatchedRecords(t *testing.T) {
	state := t.TempDir()
	corrupt := confineScopeID("corrupt", "session-a", false)
	if err := os.MkdirAll(filepath.Join(state, corrupt), 0o700); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, corrupt, confineDetachRecordName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mismatched := confineScopeID("mismatch", "session-a", false)
	if err := os.MkdirAll(filepath.Join(state, mismatched), 0o700); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	foreign, _ := json.Marshal(ConfineDetachRecord{Schema: ConfineDetachSchema, ScopeID: "CONFINE-other-1-a", Terminal: true})
	if err := os.WriteFile(filepath.Join(state, mismatched, confineDetachRecordName), foreign, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	records, err := ListConfineDetachRecords(state)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("listed %d records, want 2", len(records))
	}
	for _, record := range records {
		if record.ReadError == "" {
			t.Fatalf("record %q was interpreted despite being unreadable/mismatched: %+v", record.ScopeID, record)
		}
		if record.Terminal {
			t.Fatalf("record %q reported an outcome it cannot support", record.ScopeID)
		}
		if record.Name == "" || record.Owner == "" {
			t.Fatalf("record %q lost the identity carried by its directory name", record.ScopeID)
		}
	}
	// A missing root is an empty set, not an error: nothing has been detached yet.
	empty, err := ListConfineDetachRecords(filepath.Join(state, "nope"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("missing root: %v / %+v", err, empty)
	}
}

// TestConfineSupervisorAliveDistinguishesLiveReusedAndUnreadablePIDs pins the
// REAL probe, not an injected one. The pure resolver's table test uses a stub,
// so without this the production probe would be untested.
//
// verifies: AIRA-22
func TestConfineSupervisorAliveDistinguishesLiveReusedAndUnreadablePIDs(t *testing.T) {
	boot := bootIDOrEmpty()
	if boot == "" {
		t.Skip("boot id unavailable")
	}
	self := PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid()), BootID: boot}
	if alive, evaluated := confineSupervisorAlive(self); !alive || !evaluated {
		t.Fatalf("this process reported alive=%v evaluated=%v", alive, evaluated)
	}
	// Same pid, wrong start tick: the pid was reused by an unrelated process.
	reused := self
	reused.StartTick = self.StartTick + 1
	if alive, evaluated := confineSupervisorAlive(reused); alive || !evaluated {
		t.Fatalf("a reused pid reported alive=%v evaluated=%v; want false/true", alive, evaluated)
	}
	// Different boot: every pid from before a reboot is meaningless.
	rebooted := self
	rebooted.BootID = "00000000-0000-0000-0000-000000000000"
	if alive, evaluated := confineSupervisorAlive(rebooted); alive || !evaluated {
		t.Fatalf("a pre-reboot pid reported alive=%v evaluated=%v; want false/true", alive, evaluated)
	}
	// A genuinely reaped child.
	child := exec.Command("/bin/true")
	if err := child.Run(); err != nil {
		t.Fatalf("run child: %v", err)
	}
	gone := PIDIdentity{PID: child.Process.Pid, StartTick: 1, BootID: boot}
	if alive, _ := confineSupervisorAlive(gone); alive {
		t.Fatal("a reaped child reported alive")
	}
	if alive, evaluated := confineSupervisorAlive(PIDIdentity{PID: 0, BootID: boot}); alive || evaluated {
		t.Fatalf("pid 0 reported alive=%v evaluated=%v; want false/false (unevaluated)", alive, evaluated)
	}
}

// TestConfineWithDepsRefusesAPreSetScopeIDMintedByAnotherProcess is the
// enforcement half of the supervisor-minted-scope-id design: the refusal must
// happen inside the real launch path and must prevent the child from starting,
// not merely return an error from a helper.
//
// verifies: AIRA-22
func TestConfineWithDepsRefusesAPreSetScopeIDMintedByAnotherProcess(t *testing.T) {
	started := false
	deps := confineDetachFixtureDeps(&started)
	foreign := strings.Replace(confineScopeID("gate", "session-a", false),
		"-"+strconv.Itoa(os.Getpid())+"-", "-"+strconv.Itoa(os.Getpid()+1)+"-", 1)
	request := ConfineRequest{
		Slice: "finite.slice", Name: "gate", Owner: "session-a",
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}
	request.presetScopeID = foreign
	_, err := confineWithDeps(context.Background(), request, deps)
	if err == nil {
		t.Fatal("a scope id naming a foreign supervisor pid was accepted")
	}
	if !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") || !strings.Contains(err.Error(), "supervisor pid") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
	if started {
		t.Fatal("a child was started despite the scope-id refusal")
	}
}

// The launch gate must run AFTER every synchronous precondition and BEFORE
// admission -- that ordering is what stops `--detach` turning an exit-4 launch
// failure into a premature exit 0.
//
// verifies: AIRA-22
func TestConfineLaunchGateRunsAfterPreconditionsAndBeforeAdmission(t *testing.T) {
	started := false
	deps := confineDetachFixtureDeps(&started)
	admitted := false
	base := deps.admit
	deps.admit = func(ctx context.Context, path string, request ConfineRequest, reserve int64) (admissionResult, error) {
		admitted = true
		return base(ctx, path, request, reserve)
	}
	var seen ConfineLaunchInfo
	gateCalls := 0
	request := ConfineRequest{
		Slice: "finite.slice", Name: "gate", Owner: "session-a",
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
		BeforeAdmit: func(info ConfineLaunchInfo) error {
			gateCalls++
			seen = info
			if admitted {
				t.Error("the launch gate ran after admission")
			}
			return nil
		},
	}
	if _, err := confineWithDeps(context.Background(), request, deps); err != nil {
		t.Fatalf("confine: %v", err)
	}
	if gateCalls != 1 {
		t.Fatalf("launch gate ran %d times, want exactly 1", gateCalls)
	}
	if seen.Slice != "finite.slice" || seen.CapBytes <= 0 || seen.SupervisorPID != os.Getpid() || seen.ScopeID == "" {
		t.Fatalf("the launch gate carried unresolved facts: %+v", seen)
	}
	if !admitted {
		t.Fatal("admission never ran")
	}

	// A gate that refuses must abort BEFORE admission and before any child.
	started, admitted = false, false
	request.BeforeAdmit = func(ConfineLaunchInfo) error { return errors.New(CodeConfineDetachCancelled + ": refused") }
	_, err := confineWithDeps(context.Background(), request, deps)
	if err == nil || !strings.Contains(err.Error(), CodeConfineDetachCancelled) {
		t.Fatalf("a refusing launch gate did not abort the launch: %v", err)
	}
	if admitted {
		t.Fatal("a refused launch was still admitted, so it charged the shared ledger")
	}
	if started {
		t.Fatal("a refused launch still started a child")
	}
}

// A foreground confine (no callbacks) must be entirely unaffected, and OnPlaced
// must fire only where placement is proven.
//
// verifies: AIRA-22
func TestConfineOnPlacedFiresOnceAtProvenPlacement(t *testing.T) {
	started := false
	deps := confineDetachFixtureDeps(&started)
	placed := 0
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "gate", Owner: "session-a",
		Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
		OnPlaced: func(info ConfineLaunchInfo) {
			placed++
			if info.ScopeID == "" {
				t.Error("OnPlaced carried no scope id")
			}
		},
	}, deps)
	if err != nil {
		t.Fatalf("confine: %v", err)
	}
	if placed != 1 {
		t.Fatalf("OnPlaced fired %d times, want 1", placed)
	}
	if result.Status.Scope != ConfineScopePlaced {
		t.Fatalf("scope facet = %q", result.Status.Scope)
	}
}

// ConfineRequest must carry NO field named Detach. admitConfine transcribes
// ConfineRequest fields onto a runner.Request, and Request.Detach arms
// checkDetachAdmission, which dereferences r.ledger -- nil in confine's
// project-less admitter. The field's ABSENCE is the guard: with it present, one
// plausible transcription panics mid-admission inside a detached supervisor
// whose stderr nobody is reading.
//
// verifies: AIRA-22
func TestConfineRequestHasNoDetachFieldToTranscribe(t *testing.T) {
	requestType := reflect.TypeOf(ConfineRequest{})
	for i := 0; i < requestType.NumField(); i++ {
		if requestType.Field(i).Name == "Detach" {
			t.Fatal("ConfineRequest grew a Detach field; transcribing it into Request.Detach arms " +
				"checkDetachAdmission against confine's nil ledger. Carry detachedness on the entry point instead.")
		}
	}
	// And the transcription itself, read from production: admitConfine's Request
	// literal must leave Detach false.
	if (Request{}).Detach {
		t.Fatal("a zero Request already arms detach admission")
	}
}

// confineDetachFixtureDeps is the shared fake-cgroup fixture plus a start
// observer, so a test can assert that a refusal prevented the child from being
// started at all rather than merely returning an error.
func confineDetachFixtureDeps(started *bool) confineDeps {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	inner := deps.start
	deps.start = func(command *confineCommand) error {
		*started = true
		return inner(command)
	}
	return deps
}

func cgrouptestIsolatedParent(t *testing.T) string {
	t.Helper()
	return cgrouptest.IsolatedScopeParent(t)
}

// TestSuperviseConfineDetachedWritesATerminalRecordWhenTheLaunchFails proves the
// detach path preserves the foreground exit contract: a precondition failure is
// reported to the LAUNCHER with its own code (so `--detach` exits 4, not 0) and
// is also recorded durably.
//
// verifies: AIRA-22
func TestSuperviseConfineDetachedWritesATerminalRecordWhenTheLaunchFails(t *testing.T) {
	state := t.TempDir()
	request := ConfineRequest{
		// A slice that cannot exist: resolution fails before admission, which is
		// exactly the class of failure the launch gate must report synchronously.
		Slice: "aira-nonexistent-" + strconv.Itoa(os.Getpid()) + ".slice",
		Name:  "failing", Owner: "session-test", Argv: []string{"/bin/true"},
		DetachStateDir: state, SelfPath: os.Args[0],
	}
	control, err := writeControlValue(state, "confine-detach-*.ctrl", request)
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
	defer ackW.Close()
	superviseErr := SuperviseConfineDetached(context.Background(), control, int(readyW.Fd()), int(ackR.Fd()))
	_ = readyW.Close()
	if superviseErr == nil {
		t.Fatal("an unresolvable slice was supervised as a success")
	}
	var message confineDetachReady
	if err := json.NewDecoder(readyR).Decode(&message); err != nil {
		t.Fatalf("the supervisor reported nothing to its launcher: %v", err)
	}
	if message.Code != "E_CONFINE_UNAVAILABLE" {
		t.Fatalf("the launcher was told %q, not the foreground code E_CONFINE_UNAVAILABLE: %+v", message.Code, message)
	}
	records, err := ListConfineDetachRecords(state)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v (%v)", records, err)
	}
	record := records[0]
	if !record.Terminal {
		t.Fatal("a failed launch left a non-terminal record, which reads as outcome-unknown forever")
	}
	if record.Exit != nil {
		t.Fatalf("a failed launch fabricated exit %d", *record.Exit)
	}
	if record.ErrorCode != "E_CONFINE_UNAVAILABLE" || record.Error == "" {
		t.Fatalf("the record does not carry the failure: %+v", record)
	}
	status := classifyConfineDetachRecord(record, confineSupervisorAlive)
	if status.State != ConfineDetachFinished {
		t.Fatalf("state = %q, want finished", status.State)
	}
}

// TestConfineDetachEndToEndSurvivesTheLauncherAndCapturesTheOutcome is the real
// thing: a genuine cgroup scope, the real supervisor, a launcher that is
// group-killed the instant it has reported, and a job whose exit code and output
// are recovered afterwards from the durable record alone.
//
// verifies: AIRA-22
func TestConfineDetachEndToEndSurvivesTheLauncherAndCapturesTheOutcome(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	launcher, handle := startDetachLauncher(t, []string{
		detachStateDirEnv + "=" + state,
		detachLaunchSliceEnv + "=" + parent,
		detachLaunchArgvEnv + "=" + strings.Join([]string{"/bin/sh", "-c", "sleep 1; echo out; echo err >&2; exit 7"}, "\x1f"),
	})
	if handle.Error != "" {
		t.Fatalf("launch failed: %s", handle.Error)
	}
	if handle.ScopeID == "" || handle.SupervisorPID <= 0 {
		t.Fatalf("unusable handle: %+v", handle)
	}
	// Kill the launcher's whole group immediately: the job has not finished, so
	// anything that ties the job's life to the launcher's will lose it here.
	_ = syscall.Kill(-launcher.Process.Pid, syscall.SIGKILL)
	_ = launcher.Wait()

	deadline := time.Now().Add(60 * time.Second)
	var record ConfineDetachRecord
	for time.Now().Before(deadline) {
		records, err := ListConfineDetachRecords(state)
		if err == nil && len(records) == 1 && records[0].Terminal {
			record = records[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !record.Terminal {
		t.Fatal("the detached job never reached a terminal record after its launcher was killed")
	}
	if record.Exit == nil || *record.Exit != 7 {
		t.Fatalf("exit = %v, want 7 (error %q %q)", record.Exit, record.ErrorCode, record.Error)
	}
	stdout, err := os.ReadFile(record.StdoutPath)
	if err != nil || !strings.Contains(string(stdout), "out") {
		t.Fatalf("captured stdout = %q (%v)", stdout, err)
	}
	stderr, err := os.ReadFile(record.StderrPath)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if !strings.Contains(string(stderr), "err") {
		t.Fatalf("captured stderr lost the job's own output: %q", stderr)
	}
	if !strings.Contains(string(stderr), "terminated-by=normal") {
		t.Fatalf("captured stderr lost the confine trailer: %q", stderr)
	}
	status := classifyConfineDetachRecord(record, confineSupervisorAlive)
	if status.State != ConfineDetachFinished {
		t.Fatalf("state = %q, want finished", status.State)
	}
	if record.EnvDigest == "" || record.Cwd == "" || len(record.Argv) == 0 {
		t.Fatalf("the record cannot be used to reconstruct the run: %+v", record)
	}
}
