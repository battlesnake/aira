//go:build linux

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// confineDetachReadyTimeout bounds how long a launcher waits for its detached
// supervisor to report. It covers only the supervisor's synchronous
// preconditions (slice resolution, the cgroup probe, delegation) -- never
// admission, which is unbounded by design and happens after the handshake.
var confineDetachReadyTimeout = 10 * time.Second

// confineDetachReady is the one message a detached supervisor sends its
// launcher. It carries RESOLVED facts, so the launcher prints what is true
// rather than what was asked for -- notably the slice actually resolved, which
// may differ from the requested one.
type confineDetachReady struct {
	ScopeID       string `json:"scope_id,omitempty"`
	Slice         string `json:"slice,omitempty"`
	CapBytes      int64  `json:"cap_bytes,omitempty"`
	SupervisorPID int    `json:"supervisor_pid,omitempty"`
	RecordDir     string `json:"record_dir,omitempty"`
	Code          string `json:"code,omitempty"`
	Error         string `json:"error,omitempty"`
}

// spawnDetachedShim is the ONE definition of "detached" in this codebase, shared
// by `run --detach` and `confine --detach`: a new session (so no controlling
// terminal and no membership of the launcher's process group), no inherited
// stdio, and a reaper goroutine so the launcher does not accumulate a zombie.
func spawnDetachedShim(selfPath string, argv []string, extra []*os.File) (*exec.Cmd, error) {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	cmd := exec.Command(selfPath, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd, nil
}

// LaunchConfineDetached starts a session-independent confine supervisor and
// returns once that supervisor has passed every precondition the foreground form
// checks synchronously. It does NOT wait for admission, which can legitimately
// queue for hours, so a successful return means "the job is about to be
// admitted", never "the job succeeded".
//
// covers: AIRA-22
func LaunchConfineDetached(ctx context.Context, request ConfineRequest) (*ConfineDetachLaunch, error) {
	self := strings.TrimSpace(request.SelfPath)
	if self == "" {
		self = "/proc/self/exe"
	}
	root := strings.TrimSpace(request.DetachStateDir)
	if root == "" {
		// Fail closed rather than silently running in the foreground: a caller
		// that asked to detach and got a foreground run would lose the job to the
		// next session pause, which is the entire defect this verb exists to fix.
		return nil, errors.New(CodeConfineDetachFailed + ": no durable record directory is configured for detached confine jobs")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("%s: create record directory: %w", CodeConfineDetachFailed, err)
	}
	control, err := writeControlValue(root, "confine-detach-*.ctrl", request)
	if err != nil {
		return nil, fmt.Errorf("%s: write control file: %w", CodeConfineDetachFailed, err)
	}
	// The removal is UNCONDITIONAL, unlike the run path's. On success it is a
	// no-op, because the supervisor consumes the control file before it reports
	// ready. On every failure it matters twice over: the control file carries the
	// whole request and would otherwise accumulate forever in a DURABLE state
	// directory, and removing it stops a shim that has not yet read it from
	// launching a job the launcher has already declared failed.
	defer func() { _ = os.Remove(control) }()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", CodeConfineDetachFailed, err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		return nil, fmt.Errorf("%s: %w", CodeConfineDetachFailed, err)
	}
	cmd, err := spawnDetachedShim(self,
		[]string{"__confine-supervise", "--control", control, "--ready-fd", "3", "--ack-fd", "4"},
		[]*os.File{readyW, ackR})
	if err != nil {
		closeOutputFiles(map[string]*os.File{"ready-r": readyR, "ready-w": readyW, "ack-r": ackR, "ack-w": ackW})
		return nil, fmt.Errorf("%s: start supervisor: %w", CodeConfineDetachFailed, err)
	}
	_ = readyW.Close()
	_ = ackR.Close()
	type readyOutcome struct {
		message confineDetachReady
		err     error
	}
	messages := make(chan readyOutcome, 1)
	go func() {
		var message confineDetachReady
		decodeErr := json.NewDecoder(readyR).Decode(&message)
		_ = readyR.Close()
		messages <- readyOutcome{message: message, err: decodeErr}
	}()
	timer := time.NewTimer(confineDetachReadyTimeout)
	defer timer.Stop()
	var message confineDetachReady
	select {
	case outcome := <-messages:
		if outcome.err != nil {
			// The supervisor died or closed the pipe without reporting. Closing
			// the ack unacknowledged is what stops it launching if it somehow
			// survives, so the launcher's "failed" verdict stays true.
			_ = ackW.Close()
			return nil, fmt.Errorf("%s: supervisor reported nothing: %w", CodeConfineDetachFailed, outcome.err)
		}
		message = outcome.message
	case <-timer.C:
		_ = readyR.Close()
		_ = ackW.Close()
		return nil, fmt.Errorf("%s: supervisor readiness timed out after %s", CodeConfineDetachFailed, confineDetachReadyTimeout)
	case <-ctx.Done():
		_ = readyR.Close()
		_ = ackW.Close()
		return nil, fmt.Errorf("%s: %w", CodeConfineDetachFailed, ctx.Err())
	}
	if message.Code != "" || message.ScopeID == "" {
		_ = ackW.Close()
		code := message.Code
		if code == "" {
			code = CodeConfineDetachFailed
		}
		detail := message.Error
		if detail == "" {
			detail = "the supervisor reported no scope id"
		}
		// The supervisor's OWN code is preserved, so `--detach` against (say) an
		// uncapped slice exits 4 with E_CONFINE_UNAVAILABLE exactly as the
		// foreground form does, rather than degrading every launch failure into a
		// generic detach failure the operator has to go and look up.
		return nil, errors.New(detail)
	}
	dir := filepath.Join(root, message.ScopeID)
	launch := &ConfineDetachLaunch{
		ScopeID: message.ScopeID, Slice: message.Slice, CapBytes: message.CapBytes,
		SupervisorPID: message.SupervisorPID, RecordDir: dir,
		RecordPath:        filepath.Join(dir, confineDetachRecordName),
		StdoutPath:        filepath.Join(dir, confineDetachStdoutName),
		StderrPath:        filepath.Join(dir, confineDetachStderrName),
		SupervisorLogPath: filepath.Join(dir, confineDetachSupervisorLog),
	}
	acknowledged := false
	launch.Acknowledge = func(delivered bool) error {
		if acknowledged {
			return nil
		}
		acknowledged = true
		var writeErr error
		if delivered {
			n, err := ackW.Write([]byte{1})
			writeErr = err
			if writeErr == nil && n != 1 {
				writeErr = io.ErrShortWrite
			}
		}
		if closeErr := ackW.Close(); writeErr == nil {
			writeErr = closeErr
		}
		return writeErr
	}
	_ = cmd
	return launch, nil
}

// confineDetachJob owns one job's record directory. Every file inside is opened
// relative to the directory's own fd with O_NOFOLLOW, so a pre-planted symlink
// cannot redirect a write out of the store -- the same discipline the confine
// kill and reap paths already use for cgroup directories.
type confineDetachJob struct {
	scopeID string
	path    string
	dir     *os.File
	stdout  *os.File
	stderr  *os.File
	logFile *os.File
}

func openConfineDetachJob(root, scopeID string) (*confineDetachJob, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("no durable record directory is configured")
	}
	if !validConfineScopeID(scopeID) {
		return nil, fmt.Errorf("refusing to open a record directory for malformed scope id %q", scopeID)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(root, scopeID)
	// Mkdir, never MkdirAll: an already-existing directory means either a scope
	// id collision or another supervisor's store, and adopting it would let two
	// supervisors interleave writes into one record. Fail closed.
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), path)
	if dir == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open record directory")
	}
	job := &confineDetachJob{scopeID: scopeID, path: path, dir: dir}
	for _, spec := range []struct {
		name   string
		target **os.File
	}{
		{confineDetachStdoutName, &job.stdout},
		{confineDetachStderrName, &job.stderr},
		{confineDetachSupervisorLog, &job.logFile},
	} {
		file, openErr := job.create(spec.name)
		if openErr != nil {
			job.close()
			return nil, openErr
		}
		*spec.target = file
	}
	return job, nil
}

func (job *confineDetachJob) create(name string) (*os.File, error) {
	fd, err := unix.Openat(int(job.dir.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(job.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create %s", name)
	}
	return file, nil
}

func (job *confineDetachJob) close() {
	for _, file := range []*os.File{job.stdout, job.stderr, job.logFile, job.dir} {
		if file != nil {
			_ = file.Close()
		}
	}
}

// captureSupervisorStderr points the supervisor's OWN fd 2 at supervisor.log.
// Without it the shim's stderr is /dev/null, and a Go runtime fatal error -- a
// panic on a goroutine the supervisor does not own, which skips every deferred
// writer -- would leave a permanently non-terminal record with no recorded
// cause anywhere.
func (job *confineDetachJob) captureSupervisorStderr() {
	if job.logFile == nil {
		return
	}
	_ = unix.Dup3(int(job.logFile.Fd()), 2, 0)
}

func (job *confineDetachJob) paths() (stdout, stderr, log string) {
	return filepath.Join(job.path, confineDetachStdoutName),
		filepath.Join(job.path, confineDetachStderrName),
		filepath.Join(job.path, confineDetachSupervisorLog)
}

// write replaces record.json atomically. A reader therefore sees either the
// previous complete record or the new one, never a half-written mixture, and a
// crash between the write and the rename leaves the previous record intact.
func (job *confineDetachJob) write(record ConfineDetachRecord) error {
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	dirFD := int(job.dir.Fd())
	if err := unix.Unlinkat(dirFD, confineDetachTempName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	fd, err := unix.Openat(dirFD, confineDetachTempName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), confineDetachTempName)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("open temporary record")
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if hook := confineDetachBeforeRenameHook; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	if err := unix.Renameat(dirFD, confineDetachTempName, dirFD, confineDetachRecordName); err != nil {
		return err
	}
	return job.dir.Sync()
}

// confineDetachBeforeRenameHook is a test seam for the crash-between-write-and-
// rename case, which is the only way to prove atomicity deterministically.
var confineDetachBeforeRenameHook func() error

// ListConfineDetachRecords reads every durable record under root. A directory
// whose record cannot be read or decoded is returned WITH a ReadError rather
// than dropped: silently omitting it would turn "I cannot tell you" into "there
// is no such job".
//
// covers: AIRA-22
func ListConfineDetachRecords(root string) ([]ConfineDetachRecord, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("no durable record directory is configured")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing has ever been detached on this machine. An empty set is a
			// known fact, not an unevaluated one.
			return []ConfineDetachRecord{}, nil
		}
		return nil, err
	}
	records := make([]ConfineDetachRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		scopeID := entry.Name()
		name, pid, _, owner, ok := parseConfineScopeID(scopeID)
		if !ok {
			continue
		}
		if owner == "" {
			owner = ConfineUnknownOwner
		}
		records = append(records, readConfineDetachRecord(filepath.Join(root, scopeID), scopeID, name, owner, pid))
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ScopeID < records[j].ScopeID })
	return records, nil
}

func readConfineDetachRecord(dir, scopeID, name, owner string, pid int) ConfineDetachRecord {
	// Identity comes from the DIRECTORY NAME, which is authoritative, so a
	// record's selector fields stay usable even when its contents are not.
	fallback := ConfineDetachRecord{
		ScopeID: scopeID, Name: name, Owner: owner,
		Supervisor: PIDIdentity{PID: pid},
	}
	fd, err := unix.Open(filepath.Join(dir, confineDetachRecordName), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		fallback.ReadError = err.Error()
		return fallback
	}
	file := os.NewFile(uintptr(fd), confineDetachRecordName)
	if file == nil {
		_ = unix.Close(fd)
		fallback.ReadError = "open record"
		return fallback
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		fallback.ReadError = err.Error()
		return fallback
	}
	var record ConfineDetachRecord
	if err := json.Unmarshal(data, &record); err != nil {
		fallback.ReadError = err.Error()
		return fallback
	}
	if record.ScopeID != scopeID {
		// A record naming a different scope than the directory it lives in cannot
		// be trusted to describe either. Refuse to interpret it rather than
		// reporting one job's outcome under another job's name.
		fallback.ReadError = fmt.Sprintf("record names scope %q but lives in %q", record.ScopeID, scopeID)
		return fallback
	}
	return record
}

// confineSupervisorAlive is the real liveness probe. It compares the FULL
// identity: a differing boot id means the machine rebooted and every pid from
// before it is meaningless, and a differing start tick means the pid was reused
// by an unrelated process. Anything it cannot read is reported unevaluated, and
// unevaluated resolves to outcome-unknown -- never to "running".
//
// covers: AIRA-22
func confineSupervisorAlive(identity PIDIdentity) (bool, bool) {
	if identity.PID <= 0 {
		return false, false
	}
	boot, bootErr := currentBootID()
	if bootErr != nil || boot == "" {
		return false, false
	}
	if identity.BootID != "" && identity.BootID != boot {
		// A pid recorded before a reboot cannot be alive now. This is an
		// established fact, so it is evaluated.
		return false, true
	}
	tick := processStartTick(identity.PID)
	if tick == 0 {
		// No such process (or /proc says nothing about it). Absence of the
		// process is itself the answer when the pid is simply gone.
		if errors.Is(unix.Kill(identity.PID, 0), unix.ESRCH) {
			return false, true
		}
		return false, false
	}
	if identity.StartTick != 0 && tick != identity.StartTick {
		return false, true
	}
	return true, true
}

// ConfineDetachStatusFor answers `aira confine --status <selector>` against the
// real store, with the real liveness probe.
//
// covers: AIRA-22
func ConfineDetachStatusFor(root, selector, callerOwner string) (ConfineDetachStatus, error) {
	records, err := ListConfineDetachRecords(root)
	if err != nil {
		return ConfineDetachStatus{}, fmt.Errorf("%s: %w", CodeConfineOutcomeUnknown, err)
	}
	return ResolveConfineDetachStatus(records, selector, callerOwner, confineSupervisorAlive)
}

// ConfineDetachStatusList answers `aira confine --status` with no selector.
//
// covers: AIRA-22
func ConfineDetachStatusList(root, callerOwner string) ([]ConfineDetachStatus, error) {
	records, err := ListConfineDetachRecords(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", CodeConfineOutcomeUnknown, err)
	}
	return ListConfineDetachStatuses(records, callerOwner, confineSupervisorAlive), nil
}

// SuperviseConfineDetached is the body of the hidden `__confine-supervise` verb:
// the setsid'd process that actually owns a detached job for its whole life.
//
// covers: AIRA-22
func SuperviseConfineDetached(ctx context.Context, controlPath string, readyFD, ackFD int) error {
	ready := os.NewFile(uintptr(readyFD), "confine-detach-ready")
	ack := os.NewFile(uintptr(ackFD), "confine-detach-ack")
	if ready == nil || ack == nil {
		return errors.New(CodeConfineDetachFailed + ": invalid supervisor control descriptors")
	}
	// The confined child must never inherit the handshake descriptors: it would
	// hold the ready pipe open past the supervisor's own report.
	unix.CloseOnExec(readyFD)
	unix.CloseOnExec(ackFD)
	defer ack.Close()
	announce := &detachSignal{file: ready}
	report := func(code string, cause error) error {
		if !announce.sentAlready() {
			_ = announce.send(confineDetachReady{Code: code, Error: cause.Error()})
		}
		return cause
	}
	var request ConfineRequest
	if err := consumeControlValue(controlPath, &request); err != nil {
		return report(CodeConfineDetachFailed, fmt.Errorf("%s: read control file: %w", CodeConfineDetachFailed, err))
	}
	// A detached job exists precisely to outlive hangups, so an explicit
	// `kill -HUP` must not default-terminate the supervisor: that would skip the
	// teardown handler entirely, orphaning a running job inside its scope and
	// freezing its record non-terminal forever. SIGTERM/SIGINT still tear down
	// through the ordinary handler, and `aira confine --kill` still works.
	signal.Ignore(syscall.SIGHUP)

	scopeID, err := MintConfineScopeID(request)
	if err != nil {
		return report("E_CONFINE_ARGUMENT_INVALID", err)
	}
	job, err := openConfineDetachJob(request.DetachStateDir, scopeID)
	if err != nil {
		return report(CodeConfineDetachFailed, fmt.Errorf("%s: open record store: %w", CodeConfineDetachFailed, err))
	}
	defer job.close()
	job.captureSupervisorStderr()
	stdoutPath, stderrPath, logPath := job.paths()

	cwd, _ := os.Getwd()
	record := ConfineDetachRecord{
		Schema: ConfineDetachSchema, ScopeID: scopeID,
		Name: request.Name, Owner: request.Owner, Argv: append([]string(nil), request.Argv...),
		Cwd: cwd, DelegateRAM: request.DelegateRAM, EnvDigest: confineEnvironmentDigest(request.Env),
		Supervisor: PIDIdentity{PID: os.Getpid(), StartTick: processStartTick(os.Getpid()), BootID: bootIDOrEmpty()},
		Phase:      ConfineDetachPhaseStarting, StartedAt: nowString(nil),
		StdoutPath: stdoutPath, StderrPath: stderrPath, SupervisorLogPath: logPath,
	}
	// Name and owner are read back from the id that was actually minted, so the
	// record can never disagree with the cgroup directory about whose job it is.
	if name, _, _, owner, ok := parseConfineScopeID(scopeID); ok {
		record.Name = name
		record.Owner = owner
		if record.Owner == "" {
			record.Owner = ConfineUnknownOwner
		}
	}
	if err := job.write(record); err != nil {
		return report(CodeConfineDetachFailed, fmt.Errorf("%s: write record: %w", CodeConfineDetachFailed, err))
	}
	// A panic on THIS goroutine still lands a terminal record; a runtime-fatal
	// crash cannot, which is why supervisor.log exists and why the absence of a
	// terminal record is reported as outcome-unknown rather than as an outcome.
	terminalWritten := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if !terminalWritten {
				record.Terminal, record.EndedAt = true, nowString(nil)
				record.ErrorCode = CodeConfineDetachFailed
				record.Error = fmt.Sprintf("the detached supervisor panicked: %v", recovered)
				_ = job.write(record)
			}
			panic(recovered)
		}
	}()

	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return report(CodeConfineDetachFailed, fmt.Errorf("%s: open /dev/null: %w", CodeConfineDetachFailed, err))
	}
	defer devnull.Close()

	request.presetScopeID = scopeID
	request.Stdin, request.Stdout, request.Stderr = devnull, job.stdout, job.stderr
	request.BeforeAdmit = func(info ConfineLaunchInfo) error {
		record.Phase, record.AdmittingAt = ConfineDetachPhaseAdmitting, nowString(nil)
		record.Slice, record.CapBytes = info.Slice, info.CapBytes
		if writeErr := job.write(record); writeErr != nil {
			return fmt.Errorf("%s: write record: %w", CodeConfineDetachFailed, writeErr)
		}
		if sendErr := announce.send(confineDetachReady{
			ScopeID: scopeID, Slice: info.Slice, CapBytes: info.CapBytes,
			SupervisorPID: info.SupervisorPID, RecordDir: job.path,
		}); sendErr != nil {
			return fmt.Errorf("%s: the detached handle could not be reported: %w", CodeConfineDetachCancelled, sendErr)
		}
		var acknowledgement [1]byte
		if _, readErr := io.ReadFull(ack, acknowledgement[:]); readErr != nil {
			return errors.New(CodeConfineDetachCancelled +
				": the launcher did not acknowledge the detached handle, so nobody holds it; refusing to run an unreported job")
		}
		return nil
	}
	request.OnPlaced = func(ConfineLaunchInfo) {
		record.Phase, record.RunningAt = ConfineDetachPhaseRunning, nowString(nil)
		_ = job.write(record)
	}

	result, confineErr := Confine(ctx, request)
	record.Terminal, record.EndedAt = true, nowString(nil)
	status := result.Status
	record.Status = &status
	if status.Slice != "" {
		record.Slice = status.Slice
	}
	if confineErr != nil {
		// Exit stays ABSENT whenever Confine returned an error, including errors
		// raised after the child had already started: a zero would be a fabricated
		// success and a one a fabricated failure.
		record.ErrorCode = confineErrorCode(confineErr)
		record.Error = confineErr.Error()
	} else {
		exit := result.Exit
		record.Exit = &exit
	}
	writeErr := job.write(record)
	terminalWritten = writeErr == nil
	if !announce.sentAlready() {
		// Confine failed before the launch gate, so the launcher is still waiting.
		// Give it the real code rather than letting it time out into a generic
		// detach failure.
		code := record.ErrorCode
		if code == "" {
			code = CodeConfineDetachFailed
		}
		cause := confineErr
		if cause == nil {
			cause = errors.New("the confined job ended before the launch gate was reached")
		}
		_ = announce.send(confineDetachReady{Code: code, Error: cause.Error()})
	}
	if confineErr != nil {
		return confineErr
	}
	return writeErr
}

func bootIDOrEmpty() string {
	boot, err := currentBootID()
	if err != nil {
		return ""
	}
	return boot
}

// confineEnvironmentDigest records WHICH environment a detached job ran with,
// without recording the environment itself: a confine job's environment
// routinely carries credentials, and this record is a durable file whose whole
// purpose is to outlive the session.
func confineEnvironmentDigest(env []string) string {
	entries := env
	if entries == nil {
		entries = os.Environ()
	}
	ordered := append([]string(nil), entries...)
	sort.Strings(ordered)
	sum := sha256.New()
	for _, entry := range ordered {
		_, _ = sum.Write([]byte(entry))
		_, _ = sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

// confineErrorCode extracts the stable leading error code from a confine error.
// internal/runner must not import internal/store, so the extraction is local;
// the grammar is the project's own "CODE: detail" convention.
func confineErrorCode(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	colon := strings.IndexByte(text, ':')
	if colon <= 0 {
		return ""
	}
	candidate := text[:colon]
	if !strings.HasPrefix(candidate, "E_") && !strings.HasPrefix(candidate, "U_") && !strings.HasPrefix(candidate, "W_") {
		return ""
	}
	for _, r := range candidate {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		return ""
	}
	return candidate
}
