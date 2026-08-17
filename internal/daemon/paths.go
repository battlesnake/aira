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
)

type Paths struct {
	StateHome     string
	StateID       string
	DBPath        string
	RegistryPath  string
	LeaseStateDir string
	RuntimeDir    string
	SocketPath    string
	LockPath      string
}

const (
	defaultReapInterval         = 30 * time.Second
	defaultJournalFlushInterval = 60 * time.Second
	defaultWatchPollInterval    = 500 * time.Millisecond
)

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

// PathsFromEnv pins the state identity from the daemon's own environment.
func PathsFromEnv() (Paths, error) {
	stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	stateHome, err := canonicalPath(stateHome)
	if err != nil {
		return Paths{}, err
	}
	runtime := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
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
		LeaseStateDir: filepath.Join(stateDir, "leases"), RuntimeDir: daemonDir,
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
