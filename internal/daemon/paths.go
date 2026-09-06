package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"aira/internal/runner"
)

type Paths struct {
	StateHome     string
	StateID       string
	DBPath        string
	RegistryPath  string
	LeaseStateDir string
	// ConfineDetachDir holds one durable directory per detached confine job
	// (AIRA-22): its record plus its captured stdout/stderr. It is under the
	// STATE home rather than RuntimeDir on purpose — RuntimeDir resolves under
	// XDG_RUNTIME_DIR, which is tmpfs, and capturing an hour of a merge gate's
	// output into RAM inside the project whose whole purpose is bounding RAM
	// would be self-defeating. State home also survives a reboot, so a finished
	// job's result outlives more than the session that launched it.
	ConfineDetachDir string
	RuntimeDir       string
	SocketPath       string
	LockPath         string
}

const (
	defaultReapInterval         = 30 * time.Second
	defaultDiscoveryInterval    = 30 * time.Second
	defaultJournalFlushInterval = 60 * time.Second
	defaultWatchPollInterval    = 500 * time.Millisecond
	defaultAdmitPollInterval    = 250 * time.Millisecond
	defaultAdmitBackfillGrace   = time.Minute
	// defaultAdmitFreezeMaxHold bounds how long the fairness freeze may block the
	// whole queue CONTINUOUSLY before it must yield for the same duration — so
	// fairness never costs more than half of wall time (AIRA-59). Before this, a
	// freeze lasted until its head waiter's own timeout, so one ordinary
	// oversized request stalled every session on the machine for up to 30 minutes
	// on a visibly idle box. Crucially the bound is INDEPENDENT of max_wait_ms,
	// which is what makes AIRA-58's far more generous wait ceiling safe rather
	// than catastrophic. 0/"disabled" restores the old unbounded behaviour.
	defaultAdmitFreezeMaxHold = 2 * time.Minute
	defaultWatchdogInterval   = 2 * time.Second
	// AIRA-103 samples on the SAME cadence as the watchdog so the daemon has one
	// notion of "sustained memory pressure", differing only in what it measures
	// and what it does about it.
	defaultSliceCeilingInterval = 2 * time.Second
	defaultScopeReapInterval    = 5 * time.Minute
	defaultScopeReapGrace       = 2 * time.Minute

	// defaultStaleLeaseReleaseGrace is a LEASE-TTL policy, not a liveness
	// proof: any admitGranted lease whose scope is STILL found empty this
	// long after grantedAt (the daemon's own in-memory record of the exact
	// grant moment -- never enqueue time, which conflates ordinary
	// admission-queue contention with launch abandonment, and never any
	// external PID/filesystem signal) is reclaimed, unconditionally, once the
	// kernel itself confirms the scope is empty at reclaim time. This is
	// deliberately NOT framed as "the owner is dead": Sol/Codex's round-4
	// review established that no signal available to this daemon can
	// actually prove that -- a live launcher can be legitimately SIGSTOPed,
	// cgroup-frozen, or stuck in an unbounded kernel/filesystem operation for
	// longer than any fixed bound, and THIS POLICY WILL RECLAIM ITS LEASE
	// ANYWAY if that exceeds 15 minutes, which could break a launch that was
	// never actually abandoned. This is an accepted, deliberate trade-off
	// (an ordinary lease has always had this shape: bounded lifetime,
	// reclaimed on non-use, regardless of whether the holder could in
	// principle still resume) chosen because the alternative -- a
	// renewal/heartbeat protocol so an in-progress launch can explicitly
	// extend its own lease -- is real new machinery this project's
	// architectural-simplicity preference weighs against for a scenario
	// (a launcher paused for over 15 minutes before ever populating its
	// scope) with no evidence of ever occurring in practice. 15 minutes is
	// chosen because ordinary grant-to-populated latency is a fraction of a
	// second (scope creation immediately followed by child placement,
	// internal/runner/confine_linux.go) -- two to three orders of magnitude
	// of margin over anything this project has observed, not because it
	// proves anything. See AIRA-49's plan changelog (v1 through v5) for the
	// four review rounds that shaped this.
	//
	// It is a NEW, dedicated constant: defaultScopeReapGrace (2 minutes) is a
	// different mechanism, protected by the unrelated and already-safe
	// !hasLiveLease gate, and must not be reused here.
	defaultStaleLeaseReleaseGrace = 15 * time.Minute
)

type watchdogMode string

const (
	watchdogOff     watchdogMode = "off"
	watchdogObserve watchdogMode = "observe"
	watchdogEnforce watchdogMode = "enforce"
)

func watchdogModeFromEnv() (watchdogMode, error) {
	mode := watchdogMode(strings.TrimSpace(os.Getenv("AIRA_DAEMON_WATCHDOG_MODE")))
	if mode == "" {
		return watchdogOff, nil
	}
	switch mode {
	case watchdogOff, watchdogObserve, watchdogEnforce:
		return mode, nil
	default:
		return "", fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_WATCHDOG_MODE must be off, observe, or enforce")
	}
}

func watchdogIntervalFromEnv() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv("AIRA_DAEMON_WATCHDOG_INTERVAL"))
	if value == "" {
		return defaultWatchdogInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < time.Second || interval >= 30*time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_WATCHDOG_INTERVAL must be a Go duration in [1s,30s)")
	}
	return interval, nil
}

// sliceCeilingModeFromEnv / sliceCeilingIntervalFromEnv deliberately mirror the
// watchdog's pair above, including defaulting to OFF: this subsystem reduces the
// capacity admission believes in, and on a machine whose configured slice
// ceiling already exceeds what the box can afford that is a real (if safe)
// capacity cut. It ships observe-then-enforce, never on by default. Its reserve
// is not a knob for the same reason the watchdog's thresholds are not -- the
// existing knob for "how much of this machine AIRA may have" is
// `aira install --memory-max`, which sets the bound this can never exceed.
func sliceCeilingModeFromEnv() (sliceCeilingMode, error) {
	mode := sliceCeilingMode(strings.TrimSpace(os.Getenv("AIRA_DAEMON_SLICE_CEILING_MODE")))
	if mode == "" {
		return sliceCeilingOff, nil
	}
	switch mode {
	case sliceCeilingOff, sliceCeilingObserve, sliceCeilingEnforce:
		return mode, nil
	default:
		return "", fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_SLICE_CEILING_MODE must be off, observe, or enforce")
	}
}

// sliceCeilingConfigFromEnv reads EVERY slice-ceiling setting, and reads all but
// the mode only when the subsystem is actually wanted.
//
// That ordering is the whole point. This ships default-OFF, and the safety claim
// made for it is that off is EXACTLY today's behaviour. Validating the interval
// unconditionally broke that claim: a typo in AIRA_DAEMON_SLICE_CEILING_INTERVAL
// would refuse to start the daemon even with the mode off. A deliberate
// divergence from the watchdog's own pair, which parses both unconditionally.
// AIRA-106's two sizing variables follow the same rule.
//
// The returned policy carries MemTotal=0: only the caller can read MemTotal, and
// it is the caller that decides whether the resulting policy is usable on this
// machine (sliceCeilingPolicy.refusal).
func sliceCeilingConfigFromEnv() (sliceCeilingMode, time.Duration, sliceCeilingPolicy, error) {
	policy := sliceCeilingPolicy{reserveMax: sliceCeilingReserveMaxDefault, freeMin: sliceCeilingFreeMinDefault}
	mode, err := sliceCeilingModeFromEnv()
	if err != nil {
		return "", 0, policy, err
	}
	if mode == sliceCeilingOff {
		return mode, defaultSliceCeilingInterval, policy, nil
	}
	interval, err := sliceCeilingIntervalFromEnv()
	if err != nil {
		return "", 0, policy, err
	}
	if policy.reserveMax, err = sliceCeilingSizeFromEnv(
		"AIRA_DAEMON_SLICE_CEILING_RESERVE_MAX", sliceCeilingReserveMaxDefault); err != nil {
		return "", 0, policy, err
	}
	if policy.freeMin, err = sliceCeilingSizeFromEnv(
		"AIRA_DAEMON_SLICE_CEILING_FREE_MIN", sliceCeilingFreeMinDefault); err != nil {
		return "", 0, policy, err
	}
	return mode, interval, policy, nil
}

// sliceCeilingSizeFromEnv parses one of AIRA-106's two sizing parameters through
// the SHARED portable size parser (runner.ParseMemorySize), so "16G", "16GB",
// "16GiB" and a bare byte count all mean the same thing here as they do on
// `aira confine --memory-max`. A negative value is impossible through that parser
// (it accepts no sign) but is rejected anyway rather than trusted.
func sliceCeilingSizeFromEnv(name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	size, err := runner.ParseMemorySize(value)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("E_CONFIG_INVALID: %s must be a non-negative byte size such as 16G", name)
	}
	return size, nil
}

func sliceCeilingIntervalFromEnv() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv("AIRA_DAEMON_SLICE_CEILING_INTERVAL"))
	if value == "" {
		return defaultSliceCeilingInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < time.Second || interval >= 30*time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_SLICE_CEILING_INTERVAL must be a Go duration in [1s,30s)")
	}
	return interval, nil
}

func admitPollIntervalFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_ADMIT_POLL_INTERVAL")
	if !set || value == "" {
		return defaultAdmitPollInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < 250*time.Millisecond || interval >= 10*time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_ADMIT_POLL_INTERVAL must be a Go duration in [250ms,10s)")
	}
	return interval, nil
}

func admitBackfillGraceFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_ADMIT_BACKFILL_GRACE")
	if !set || value == "" {
		return defaultAdmitBackfillGrace, nil
	}
	if value == "disabled" || value == "0" {
		return 0, nil
	}
	grace, err := time.ParseDuration(value)
	if err != nil || grace <= 0 {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_ADMIT_BACKFILL_GRACE must be a positive Go duration, disabled, or 0")
	}
	return grace, nil
}

// admitFreezeMaxHoldFromEnv follows the admitBackfillGraceFromEnv convention
// exactly, including "disabled"/"0". Disabled means the fairness freeze is
// UNBOUNDED again — the pre-AIRA-59 behaviour — and the evaluator then leaves the
// phase anchor untouched rather than writing state nothing reads.
func admitFreezeMaxHoldFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD")
	if !set || value == "" {
		return defaultAdmitFreezeMaxHold, nil
	}
	if value == "disabled" || value == "0" {
		return 0, nil
	}
	hold, err := time.ParseDuration(value)
	// Floor at one second, comfortably above any permitted poll interval
	// (admitPollIntervalFromEnv caps at 10s but defaults to 250ms). A hold shorter
	// than a poll interval is refused rather than merely tolerated: the evaluator
	// would then routinely leap a whole hold/yield cycle between passes, which the
	// duty cycle handles but which makes the setting meaningless.
	if err != nil || hold < time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD must be a Go duration of at least 1s, disabled, or 0")
	}
	return hold, nil
}

func watchPollIntervalFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_WATCH_POLL_INTERVAL")
	if !set || value == "" {
		return defaultWatchPollInterval, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < 250*time.Millisecond || interval >= 10*time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_WATCH_POLL_INTERVAL must be a Go duration in [250ms,10s)")
	}
	return interval, nil
}

func reapIntervalFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_REAP_INTERVAL")
	if !set || value == "" {
		return defaultReapInterval, nil
	}
	if value == "disabled" || value == "0" {
		return 0, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval <= 0 {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_REAP_INTERVAL must be a positive Go duration, disabled, or 0")
	}
	return interval, nil
}

func journalFlushIntervalFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL")
	if !set || value == "" {
		return defaultJournalFlushInterval, nil
	}
	if value == "disabled" || value == "0" {
		return 0, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL must be a Go duration of at least 1s, disabled, or 0")
	}
	return interval, nil
}

func registryDiscoveryIntervalFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_DISCOVERY_INTERVAL")
	if !set || value == "" {
		return defaultDiscoveryInterval, nil
	}
	if value == "disabled" || value == "0" {
		return 0, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_DISCOVERY_INTERVAL must be a Go duration of at least 1s, disabled, or 0")
	}
	return interval, nil
}

func scopeReapIntervalFromEnv() (time.Duration, error) {
	value, set := os.LookupEnv("AIRA_DAEMON_SCOPE_REAP_INTERVAL")
	if !set || value == "" {
		return defaultScopeReapInterval, nil
	}
	if value == "disabled" || value == "0" {
		return 0, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < time.Second {
		return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_SCOPE_REAP_INTERVAL must be a Go duration of at least 1s, disabled, or 0")
	}
	return interval, nil
}

// PathsFromEnv pins the state identity from the daemon's own environment.
func PathsFromEnv() (Paths, error) {
	stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	home := ""
	if stateHome == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
	}
	return PathsFromEnvironment(stateHome, strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")), home)
}

// PathsFromEnvironment resolves the same daemon identity as PathsFromEnv from
// explicit environment values. It lets service-aware callers compare identities
// without mutating process-global environment variables.
func PathsFromEnvironment(stateHome, runtimeDir, home string) (Paths, error) {
	stateHome = strings.TrimSpace(stateHome)
	if stateHome == "" {
		if strings.TrimSpace(home) == "" {
			return Paths{}, errors.New("HOME is unset; cannot resolve XDG_STATE_HOME")
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	stateHome, err := canonicalPath(stateHome)
	if err != nil {
		return Paths{}, err
	}
	runtime := strings.TrimSpace(runtimeDir)
	if runtime == "" {
		runtime = filepath.Join(stateHome, "aira", "run")
	}
	runtime, err = filepath.Abs(runtime)
	if err != nil {
		return Paths{}, err
	}
	sum := sha256.Sum256([]byte(stateHome))
	stateID := hex.EncodeToString(sum[:16])
	daemonDir := filepath.Join(runtime, "aira", stateID)
	stateDir := filepath.Join(stateHome, "aira")
	return Paths{
		StateHome: stateHome, StateID: stateID,
		DBPath: filepath.Join(stateDir, "state.db"), RegistryPath: filepath.Join(stateDir, "registry.jsonl"),
		LeaseStateDir:    filepath.Join(stateDir, "leases"),
		ConfineDetachDir: filepath.Join(stateDir, "confine"), RuntimeDir: daemonDir,
		SocketPath: filepath.Join(daemonDir, "daemon.sock"), LockPath: filepath.Join(daemonDir, "daemon.lock"),
	}, nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	candidate := abs
	remainder := []string(nil)
	for {
		_, statErr := os.Lstat(candidate)
		if statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				return "", resolveErr
			}
			for index := len(remainder) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, remainder[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", statErr
		}
		remainder = append(remainder, filepath.Base(candidate))
		candidate = parent
	}
}

type LockInfo struct {
	PID    int    `json:"pid"`
	BootID string `json:"boot_id"`
}

type StatusInfo struct {
	Running bool     `json:"running"`
	Ready   bool     `json:"ready"`
	Lock    LockInfo `json:"lock"`
}

func currentBootID() string {
	data, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(data))
}

func writeLockInfo(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(LockInfo{PID: os.Getpid(), BootID: currentBootID()}); err != nil {
		return err
	}
	return file.Sync()
}

func readLockInfo(path string) LockInfo {
	data, err := os.ReadFile(path)
	if err != nil {
		return LockInfo{}
	}
	var info LockInfo
	_ = json.Unmarshal(data, &info)
	return info
}

// Status checks both the lock holder identity and socket acceptability.
func Status(paths Paths) StatusInfo {
	info := StatusInfo{Lock: readLockInfo(paths.LockPath)}
	file, err := os.OpenFile(paths.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		if lockErr := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); lockErr == nil {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		} else if errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN) {
			info.Running = info.Lock.PID > 0 && info.Lock.BootID != "" && info.Lock.BootID == currentBootID()
		}
		_ = file.Close()
	}
	conn, err := net.DialTimeout("unix", paths.SocketPath, 100*time.Millisecond)
	if err == nil {
		info.Ready = true
		_ = conn.Close()
	}
	return info
}

// Stop performs the explicit operator-authorised SIGTERM action, guarded
// against pid reuse by the recorded kernel boot identity.
func Stop(paths Paths) error {
	status := Status(paths)
	if !status.Running || status.Lock.PID <= 0 {
		return fmt.Errorf("%s: daemon is not running", CodeUnavailable)
	}
	if status.Lock.BootID != currentBootID() {
		return fmt.Errorf("%s: daemon lock belongs to another boot", CodeUnavailable)
	}
	process, err := os.FindProcess(status.Lock.PID)
	if err != nil {
		return fmt.Errorf("%s: %w", CodeUnavailable, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("%s: %w", CodeUnavailable, err)
	}
	return nil
}
