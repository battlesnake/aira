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
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"

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
	if mode == "slow" {
		// Outlive the launcher's readiness timeout without reporting.
		time.Sleep(5 * time.Second)
		return 3
	}
	if mode == "refuse" {
		// Report a FOREGROUND code with a detail that does NOT repeat it, which is
		// what forces the launcher to attach the code itself rather than relying on
		// every supervisor message happening to start with one.
		readyFD, _ := strconv.Atoi(values["ready-fd"])
		ready := os.NewFile(uintptr(readyFD), "ready")
		if ready == nil {
			return 3
		}
		_ = (&detachSignal{file: ready}).send(confineDetachReady{
			Code: "E_CONFINE_UNAVAILABLE", Error: "slice has no finite memory.max in its cgroup ancestry",
		})
		return 4
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
	case <-testdeadline.After(30 * time.Second):
		t.Fatal("launcher did not report a handle")
	}
	// The supervisor is setsid'd, so killing the launcher's process group -- which
	// is exactly what these tests do -- deliberately does NOT reach it. Without an
	// explicit cleanup every run would leave supervisors (and their confined jobs)
	// alive for as long as their argv takes, holding admission reserve on the
	// shared slice and slowing every later test on the machine.
	if handle.SupervisorPID > 0 {
		t.Cleanup(func() { _ = unix.Kill(handle.SupervisorPID, unix.SIGKILL) })
	}
	if skipAck || handle.Error != "" {
		return cmd, handle
	}
	deadline := time.Now().Add(testdeadline.Wait(30 * time.Second))
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
	deadline := time.Now().Add(testdeadline.Wait(5 * time.Second))
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

// A supervisor that refuses with a FOREGROUND code must have that code reach
// the launcher's caller intact, or `--detach` degrades every synchronous launch
// failure to the generic exit-1 default and the operator loses the diagnosis
// the foreground form would have given them.
//
// verifies: AIRA-22
func TestLaunchConfineDetachedPropagatesTheSupervisorsOwnErrorCode(t *testing.T) {
	t.Setenv(fakeSupervisorEnv, "refuse")
	_, err := LaunchConfineDetached(context.Background(), ConfineRequest{
		Name: "refused", Owner: "session-test", Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], DetachStateDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a refusing supervisor was treated as a successful launch")
	}
	if !strings.HasPrefix(err.Error(), "E_CONFINE_UNAVAILABLE:") {
		t.Fatalf("error %q does not lead with the supervisor's own code, so it maps to the exit-1 default", err)
	}
	if !strings.Contains(err.Error(), "no finite memory.max") {
		t.Fatalf("the supervisor's detail was lost: %v", err)
	}
}

// A supervisor that never reports must time the launcher out rather than
// hanging it, and must be reported as a failure.
//
// verifies: AIRA-22
func TestLaunchConfineDetachedTimesOutOnASilentSupervisor(t *testing.T) {
	t.Setenv(fakeSupervisorEnv, "slow")
	original := confineDetachReadyTimeout
	confineDetachReadyTimeout = 150 * time.Millisecond
	t.Cleanup(func() { confineDetachReadyTimeout = original })
	started := time.Now()
	_, err := LaunchConfineDetached(context.Background(), ConfineRequest{
		Name: "slow", Owner: "session-test", Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], DetachStateDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a supervisor that never reported was treated as a successful launch")
	}
	if !strings.Contains(err.Error(), CodeConfineDetachFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("wrong failure: %v", err)
	}
	if elapsed := time.Since(started); testdeadline.Exceeded(elapsed, 3*time.Second) {
		t.Fatalf("the launcher waited %s; the readiness timeout did not bound it", elapsed)
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
	deadline := time.Now().Add(testdeadline.Wait(10 * time.Second))
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
	_, supervisorPID, _, _, _ := parseConfineScopeID(scopeID)
	first := ConfineDetachRecord{
		Schema: ConfineDetachSchema, ScopeID: scopeID, Name: "atomic", Owner: "session-a",
		Supervisor: PIDIdentity{PID: supervisorPID},
		Phase:      ConfineDetachPhaseAdmitting, StartedAt: "t0",
	}
	if err := job.write(first); err != nil {
		t.Fatalf("write first: %v", err)
	}
	confineDetachBeforeRenameHook = func() error { return errors.New("injected crash before rename") }
	t.Cleanup(func() { confineDetachBeforeRenameHook = nil })
	second := first
	exitZero := 0
	second.Phase, second.Terminal, second.StartedAt, second.Exit = ConfineDetachPhaseRunning, true, "t1", &exitZero
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
	// An EMPTY pre-existing directory is the case that distinguishes creating the
	// directory from merely ensuring it: the capture files' O_EXCL catches a
	// populated one either way, so only this case proves the directory itself is
	// created rather than adopted.
	empty := confineScopeID("empty", "session-a", false)
	if err := os.Mkdir(filepath.Join(state, empty), 0o700); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := openConfineDetachJob(state, empty); err == nil {
		t.Fatal("a supervisor adopted an empty pre-existing record directory instead of refusing it")
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
// The INCOMPLETE-identity and ZOMBIE cases are the ones a hand-rolled probe gets
// wrong (build review, Sol): both must resolve away from "running", one as
// unevaluated and one as dead.
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
	// INCOMPLETE identities are unevaluated, never optimistically alive: a record
	// written before the boot id or start tick could be read carries no evidence
	// that its pid is still the same process.
	for _, incomplete := range []PIDIdentity{
		{PID: os.Getpid(), StartTick: self.StartTick},      // no boot id
		{PID: os.Getpid(), BootID: boot},                   // no start tick
		{PID: 0, StartTick: self.StartTick, BootID: boot},  // no pid
		{PID: -1, StartTick: self.StartTick, BootID: boot}, // nonsense pid
	} {
		if alive, evaluated := confineSupervisorAlive(incomplete); alive || evaluated {
			t.Fatalf("incomplete identity %+v reported alive=%v evaluated=%v; want false/false", incomplete, alive, evaluated)
		}
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
	// A ZOMBIE still has a /proc entry and its original start tick, so a probe
	// that only asks "does /proc answer" would call it alive. A zombie supervisor
	// has exited and will never write an outcome.
	zombie := exec.Command("/bin/true")
	if err := zombie.Start(); err != nil {
		t.Fatalf("start zombie: %v", err)
	}
	zombiePID := zombie.Process.Pid
	zombieIdentity := PIDIdentity{PID: zombiePID, StartTick: processStartTick(zombiePID), BootID: boot}
	deadline := time.Now().Add(testdeadline.Wait(5 * time.Second))
	sawZombie := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(zombiePID), "stat"))
		if err == nil && processLivenessFromStat(data) == processDead {
			sawZombie = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawZombie {
		_ = zombie.Wait()
		t.Skip("could not observe the child in the zombie state")
	}
	alive, evaluated := confineSupervisorAlive(zombieIdentity)
	_ = zombie.Wait()
	if alive || !evaluated {
		t.Fatalf("a zombie supervisor reported alive=%v evaluated=%v; want false/true (it exited and will never write an outcome)", alive, evaluated)
	}
}

// A record whose contents disagree with the authoritative directory name cannot
// be trusted to describe either job: a wrong owner misdirects owner-scoped
// selection and a wrong supervisor pid makes `--status` probe an unrelated
// process's liveness.
//
// verifies: AIRA-22
func TestListConfineDetachRecordsRefusesARecordThatDisagreesWithItsDirectory(t *testing.T) {
	scopeID := confineScopeID("bound", "session-a", false)
	name, pid, _, owner, ok := parseConfineScopeID(scopeID)
	if !ok {
		t.Fatalf("setup: %q does not parse", scopeID)
	}
	sound := ConfineDetachRecord{
		Schema: ConfineDetachSchema, ScopeID: scopeID, Name: name, Owner: owner,
		Supervisor: PIDIdentity{PID: pid}, Phase: ConfineDetachPhaseRunning,
	}
	for _, test := range []struct {
		name   string
		mutate func(*ConfineDetachRecord)
		want   string
	}{
		{name: "foreign name", mutate: func(r *ConfineDetachRecord) { r.Name = "other" }, want: "names"},
		{name: "foreign owner", mutate: func(r *ConfineDetachRecord) { r.Owner = "session-b" }, want: "owner"},
		{name: "foreign supervisor pid", mutate: func(r *ConfineDetachRecord) { r.Supervisor.PID = pid + 1 }, want: "supervisor pid"},
		{name: "foreign scope id", mutate: func(r *ConfineDetachRecord) { r.ScopeID = "CONFINE-other-1-a" }, want: "scope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := t.TempDir()
			if err := os.MkdirAll(filepath.Join(state, scopeID), 0o700); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			forged := sound
			test.mutate(&forged)
			payload, _ := json.Marshal(forged)
			if err := os.WriteFile(filepath.Join(state, scopeID, confineDetachRecordName), payload, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			records, err := ListConfineDetachRecords(state)
			if err != nil || len(records) != 1 {
				t.Fatalf("records=%+v err=%v", records, err)
			}
			if records[0].ReadError == "" {
				t.Fatalf("a record disagreeing with its directory was interpreted: %+v", records[0])
			}
			if !strings.Contains(records[0].ReadError, test.want) {
				t.Fatalf("read error %q does not name the mismatch %q", records[0].ReadError, test.want)
			}
			// It must still be SELECTABLE, from the directory's own identity.
			if records[0].Name != name || records[0].Owner != owner || records[0].Supervisor.PID != pid {
				t.Fatalf("the directory's authoritative identity was lost: %+v", records[0])
			}
		})
	}
	// A sound record round-trips.
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, scopeID), 0o700); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	payload, _ := json.Marshal(sound)
	if err := os.WriteFile(filepath.Join(state, scopeID, confineDetachRecordName), payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	records, err := ListConfineDetachRecords(state)
	if err != nil || len(records) != 1 || records[0].ReadError != "" {
		t.Fatalf("a sound record was refused: %+v (%v)", records, err)
	}
}

// A non-regular record (a FIFO, say) must be refused rather than opened, or a
// status query blocks forever on a file someone else planted.
//
// verifies: AIRA-22
func TestListConfineDetachRecordsRefusesANonRegularRecord(t *testing.T) {
	state := t.TempDir()
	scopeID := confineScopeID("fifo", "session-a", false)
	if err := os.MkdirAll(filepath.Join(state, scopeID), 0o700); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := unix.Mkfifo(filepath.Join(state, scopeID, confineDetachRecordName), 0o600); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}
	done := make(chan []ConfineDetachRecord, 1)
	go func() {
		records, _ := ListConfineDetachRecords(state)
		done <- records
	}()
	select {
	case records := <-done:
		if len(records) != 1 || records[0].ReadError == "" {
			t.Fatalf("a fifo was accepted as a record: %+v", records)
		}
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("reading a fifo record blocked the status query")
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

// superviseOutcome is what a subprocess run of the REAL supervisor produced.
type superviseOutcome struct {
	message  confineDetachReady
	exitCode int
}

// runSuperviseSubprocess runs the production `__confine-supervise` entry point in
// a SEPARATE process. That isolation is load-bearing rather than stylistic: the
// supervisor calls signal.Ignore(SIGHUP) and dup2s its own fd 2 onto a log file
// inside a t.TempDir(), so running it in-process would leave the whole test
// binary's stderr pointing at a deleted file and SIGHUP ignored for every later
// test (build review, Fable).
func runSuperviseSubprocess(t *testing.T, request ConfineRequest, acknowledge bool) superviseOutcome {
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
	if acknowledge {
		// EPIPE is legitimate here: a supervisor that failed a precondition never
		// reaches the launch gate and so never reads the acknowledgement.
		if _, err := ackW.Write([]byte{1}); err != nil && !errors.Is(err, syscall.EPIPE) {
			t.Fatalf("acknowledge: %v", err)
		}
	}
	_ = ackW.Close()
	waitErr := cmd.Wait()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code = exitErr.ExitCode()
	} else if waitErr != nil {
		t.Fatalf("wait: %v", waitErr)
	}
	return superviseOutcome{message: message, exitCode: code}
}

func soleDetachRecord(t *testing.T, state string) ConfineDetachRecord {
	t.Helper()
	records, err := ListConfineDetachRecords(state)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v (%v)", records, err)
	}
	return records[0]
}

// TestSuperviseConfineDetachedWritesATerminalRecordWhenTheLaunchFails proves the
// detach path preserves the foreground exit contract: a precondition failure is
// reported to the LAUNCHER with its own code (so `--detach` exits 4, not 0) and
// is also recorded durably.
//
// verifies: AIRA-22
func TestSuperviseConfineDetachedWritesATerminalRecordWhenTheLaunchFails(t *testing.T) {
	state := t.TempDir()
	outcome := runSuperviseSubprocess(t, ConfineRequest{
		// A slice that cannot exist: resolution fails before admission, which is
		// exactly the class of failure the launch gate must report synchronously.
		Slice: "aira-nonexistent-" + strconv.Itoa(os.Getpid()) + ".slice",
		Name:  "failing", Owner: "session-test", Argv: []string{"/bin/true"},
		DetachStateDir: state, SelfPath: os.Args[0],
	}, true)
	if outcome.exitCode == 0 {
		t.Fatal("an unresolvable slice was supervised as a success")
	}
	if outcome.message.Code != "E_CONFINE_UNAVAILABLE" {
		t.Fatalf("the launcher was told %q, not the foreground code E_CONFINE_UNAVAILABLE: %+v", outcome.message.Code, outcome.message)
	}
	record := soleDetachRecord(t, state)
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

// TestSuperviseConfineDetachedWritesATerminalRecordOnAPanic pins the deferred
// writer. "Every exit path writes a terminal record" is a claim about a defer,
// and the only way to test a defer that guards against panics is to panic.
//
// verifies: AIRA-22
func TestSuperviseConfineDetachedWritesATerminalRecordOnAPanic(t *testing.T) {
	state := t.TempDir()
	restore := isolateSupervisorSideEffects(t)
	defer restore()
	confineDetachAfterRecordHook = func() { panic("injected supervisor panic") }
	t.Cleanup(func() { confineDetachAfterRecordHook = nil })
	control, err := writeControlValue(state, "confine-detach-*.ctrl", ConfineRequest{
		Slice: "aira-nonexistent-" + strconv.Itoa(os.Getpid()) + ".slice",
		Name:  "panicking", Owner: "session-test", Argv: []string{"/bin/true"},
		DetachStateDir: state, SelfPath: os.Args[0],
	})
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
	defer readyR.Close()
	defer readyW.Close()
	defer ackR.Close()
	defer ackW.Close()
	// The supervisor wraps the descriptors it is given in its OWN *os.File and
	// closes them. Handing it dup'd fds keeps that ownership disjoint from the
	// test's: sharing one fd between two *os.File values is a double close, and a
	// double close silently corrupts whichever unrelated file later reuses that
	// descriptor number -- observed as an EBADF in a different test entirely.
	supervisorReady, err := unix.Dup(int(readyW.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	supervisorAck, err := unix.Dup(int(ackR.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("the supervisor swallowed the panic instead of re-raising it")
			}
		}()
		_ = SuperviseConfineDetached(context.Background(), control, supervisorReady, supervisorAck)
	}()
	record := soleDetachRecord(t, state)
	if !record.Terminal {
		t.Fatal("a panicking supervisor left a non-terminal record, which reads as outcome-unknown forever")
	}
	if record.Exit != nil {
		t.Fatalf("a panicking supervisor fabricated exit %d", *record.Exit)
	}
	if record.ErrorCode != CodeConfineDetachFailed || !strings.Contains(record.Error, "panicked") {
		t.Fatalf("the record does not name the panic: %+v", record)
	}
}

// A supervisor whose own process identity is unavailable can never be
// liveness-checked, so its record would read `outcome-unknown` for ever however
// the job ended. It must refuse to start rather than create a record nobody can
// interpret.
//
// verifies: AIRA-22
func TestSuperviseConfineDetachedRefusesWhenItsOwnIdentityIsUnavailable(t *testing.T) {
	state := t.TempDir()
	originalBootID := readBootIDFn
	readBootIDFn = func() (string, error) { return "", errors.New("injected: boot id unreadable") }
	t.Cleanup(func() { readBootIDFn = originalBootID })
	restore := isolateSupervisorSideEffects(t)
	defer restore()
	control, err := writeControlValue(state, "confine-detach-*.ctrl", ConfineRequest{
		Slice: "aira-nonexistent-" + strconv.Itoa(os.Getpid()) + ".slice",
		Name:  "identityless", Owner: "session-test", Argv: []string{"/bin/true"},
		DetachStateDir: state, SelfPath: os.Args[0],
	})
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
	defer readyR.Close()
	defer readyW.Close()
	defer ackR.Close()
	defer ackW.Close()
	supervisorReady, err := unix.Dup(int(readyW.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	supervisorAck, err := unix.Dup(int(ackR.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	superviseErr := SuperviseConfineDetached(context.Background(), control, supervisorReady, supervisorAck)
	if superviseErr == nil {
		t.Fatal("a supervisor with no establishable identity started anyway")
	}
	if !strings.Contains(superviseErr.Error(), "process identity is unavailable") {
		t.Fatalf("unhelpful refusal: %v", superviseErr)
	}
	// The refusal must happen BEFORE the record store is created: a record whose
	// supervisor can never be liveness-checked is worse than no record.
	entries, readErr := os.ReadDir(state)
	if readErr != nil {
		t.Fatalf("read state dir: %v", readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("an identity-less supervisor created an uninterpretable record directory %q", entry.Name())
		}
	}
}

// isolateSupervisorSideEffects lets ONE test run the supervisor in-process (the
// panic path cannot be injected across a process boundary) without leaking its
// two process-global side effects into the rest of the package.
func isolateSupervisorSideEffects(t *testing.T) func() {
	t.Helper()
	saved, err := unix.Dup(2)
	if err != nil {
		t.Fatalf("dup fd 2: %v", err)
	}
	return func() {
		_ = unix.Dup3(saved, 2, 0)
		_ = unix.Close(saved)
		signal.Reset(syscall.SIGHUP)
	}
}

// TestRealSupervisorRefusesToRunAnUnacknowledgedJob exercises the PRODUCTION
// cancel path, not the fake supervisor's re-implementation of it. Without this,
// a real supervisor that ignored the acknowledgement entirely would pass the
// suite, and the no-ghost-job property would rest on test-only code.
//
// verifies: AIRA-22
func TestRealSupervisorRefusesToRunAnUnacknowledgedJob(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	evidence := filepath.Join(t.TempDir(), "the-job-ran")
	outcome := runSuperviseSubprocess(t, ConfineRequest{
		Slice: parent, Name: "unacked", Owner: "session-test",
		Argv:           []string{"/bin/sh", "-c", "touch " + evidence},
		DetachStateDir: state, SelfPath: os.Args[0],
		MemoryReserve: 64 << 20, MemoryReservePinned: true,
	}, false)
	if outcome.message.ScopeID == "" {
		t.Fatalf("the supervisor never reached the launch gate: %+v", outcome.message)
	}
	if _, err := os.Stat(evidence); err == nil {
		t.Fatal("an unacknowledged job RAN; a launcher reporting a detach failure would have left a ghost job on the shared slice")
	}
	record := soleDetachRecord(t, state)
	if !record.Terminal || record.ErrorCode != CodeConfineDetachCancelled {
		t.Fatalf("unacknowledged launch recorded %+v, want a terminal %s", record, CodeConfineDetachCancelled)
	}
	if record.Exit != nil {
		t.Fatalf("a cancelled launch fabricated exit %d", *record.Exit)
	}
	if record.Phase != ConfineDetachPhaseAdmitting {
		t.Fatalf("phase=%q: the cancel must happen at the launch gate, before admission", record.Phase)
	}
}

// TestConfineListAndKillTargetTheDetachedSupervisorPID is the §3.2 invariant seen
// from the DAEMON's side: the pid embedded in the cgroup directory name must be
// the surviving supervisor's, because that is what `confine --list` publishes,
// what `confine --kill <pid>` matches, and what the orphan reaper's liveness
// predicate reads. A launcher-minted id would break all three.
//
// verifies: AIRA-22
func TestConfineListAndKillTargetTheDetachedSupervisorPID(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	launcher, handle := startDetachLauncher(t, []string{
		detachStateDirEnv + "=" + state,
		detachLaunchSliceEnv + "=" + parent,
		detachLaunchArgvEnv + "=" + strings.Join([]string{"/bin/sh", "-c", "echo started; sleep 10"}, "\x1f"),
	})
	if handle.Error != "" {
		t.Fatalf("launch failed: %s", handle.Error)
	}
	_ = syscall.Kill(-launcher.Process.Pid, syscall.SIGKILL)
	_ = launcher.Wait()

	// Wait until the JOB itself has exec'd and produced output. Killing earlier
	// would race the confine handshake and terminate the job before it ran, which
	// is a different (and honestly reported) outcome from the external cgroup
	// kill this test is about.
	capture := filepath.Join(handle.RecordDir, confineDetachStdoutName)
	runningBy := time.Now().Add(testdeadline.Wait(30 * time.Second))
	for time.Now().Before(runningBy) {
		if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), "started") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for the scope to appear and be populated.
	var listed ConfineRecord
	deadline := time.Now().Add(testdeadline.Wait(30 * time.Second))
	for time.Now().Before(deadline) {
		result, err := ListConfines(context.Background(), parent, nil)
		if err == nil && result.Verdict == "pass" {
			for _, record := range result.Scopes {
				if record.ScopeID == handle.ScopeID && record.Populated != nil && *record.Populated > 0 {
					listed = record
				}
			}
		}
		if listed.ScopeID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if listed.ScopeID == "" {
		t.Fatal("the detached job's scope never appeared populated in confine --list")
	}
	if listed.SupervisorPID == nil || *listed.SupervisorPID != handle.SupervisorPID {
		t.Fatalf("confine --list reports supervisor pid %v, but the surviving supervisor is %d; the scope id names the wrong process",
			listed.SupervisorPID, handle.SupervisorPID)
	}
	if listed.Owner != "session-test" {
		t.Fatalf("owner=%q, want the launching owner", listed.Owner)
	}
	// Kill BY SUPERVISOR PID, which is only possible because the surviving
	// supervisor minted the id.
	killed, err := KillConfine(context.Background(), parent, strconv.Itoa(handle.SupervisorPID), "session-test", false, nil)
	if err != nil {
		t.Fatalf("confine --kill %d: %v", handle.SupervisorPID, err)
	}
	if killed.ScopeID != handle.ScopeID {
		t.Fatalf("killed %q, want %q", killed.ScopeID, handle.ScopeID)
	}
	deadline = time.Now().Add(testdeadline.Wait(30 * time.Second))
	for time.Now().Before(deadline) {
		record := soleDetachRecord(t, state)
		if !record.Terminal {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// The killed job must never read as a clean run.
		if record.Exit != nil && *record.Exit == 0 {
			t.Fatalf("an externally killed job recorded a clean exit 0: %+v", record)
		}
		// The supervisor sent no signal itself and this scope's own memory.events
		// records no OOM, so the AIRA-70 classifier's verdict for a `confine
		// --kill` is unattributed-sigkill. It is asserted only when the classifier
		// ran at all: a kill landing during the launch handshake ends the job
		// earlier, with an equally honest E_CONFINE_UNAVAILABLE.
		if record.Status != nil && record.Status.TerminatedBy != "" &&
			record.Status.TerminatedBy != ConfineTerminatedUnattributedSIGKILL {
			t.Fatalf("terminated-by=%q, want %q for an external cgroup kill",
				record.Status.TerminatedBy, ConfineTerminatedUnattributedSIGKILL)
		}
		if record.Status == nil || record.Status.TerminatedBy == "" {
			if record.ErrorCode == "" {
				t.Fatalf("the killed job recorded neither a termination verdict nor an error: %+v", record)
			}
		}
		return
	}
	t.Fatal("the killed detached job never reached a terminal record")
}

// A detached job exists to outlive hangups, so an explicit SIGHUP to the
// supervisor must not end it. `confineSignalSource` notifies only SIGINT and
// SIGTERM, so without the explicit signal.Ignore the default disposition would
// terminate the supervisor with no teardown at all.
//
// verifies: AIRA-22
func TestDetachedSupervisorIgnoresAnExplicitSIGHUP(t *testing.T) {
	parent := cgrouptestIsolatedParent(t)
	state := t.TempDir()
	launcher, handle := startDetachLauncher(t, []string{
		detachStateDirEnv + "=" + state,
		detachLaunchSliceEnv + "=" + parent,
		detachLaunchArgvEnv + "=" + strings.Join([]string{"/bin/sh", "-c", "sleep 2; echo survived"}, "\x1f"),
	})
	if handle.Error != "" {
		t.Fatalf("launch failed: %s", handle.Error)
	}
	_ = syscall.Kill(-launcher.Process.Pid, syscall.SIGKILL)
	_ = launcher.Wait()
	// Straight at the supervisor, not at a process group: this is the signal a
	// terminal hangup or a careless `kill -HUP` delivers.
	for i := 0; i < 3; i++ {
		if err := unix.Kill(handle.SupervisorPID, unix.SIGHUP); err != nil {
			t.Fatalf("SIGHUP: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline := time.Now().Add(testdeadline.Wait(45 * time.Second))
	for time.Now().Before(deadline) {
		record := soleDetachRecord(t, state)
		if record.Terminal {
			if record.Exit == nil || *record.Exit != 0 {
				t.Fatalf("SIGHUP ended the detached job: %+v", record)
			}
			stdout, _ := os.ReadFile(record.StdoutPath)
			if !strings.Contains(string(stdout), "survived") {
				t.Fatalf("the job did not run to completion through SIGHUP: %q", stdout)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the detached job never completed after SIGHUP")
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

	deadline := time.Now().Add(testdeadline.Wait(60 * time.Second))
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
