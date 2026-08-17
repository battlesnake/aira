package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReapIntervalConfig(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
		code  string
	}{
		{name: "default", want: 30 * time.Second},
		{name: "duration", set: true, value: "2m", want: 2 * time.Minute},
		{name: "disabled", set: true, value: "disabled", want: 0},
		{name: "zero", set: true, value: "0", want: 0},
		{name: "malformed", set: true, value: "soon", code: "E_CONFIG_INVALID"},
		{name: "negative", set: true, value: "-1s", code: "E_CONFIG_INVALID"},
		{name: "noncanonical zero", set: true, value: "0s", code: "E_CONFIG_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.set {
				t.Setenv("AIRA_DAEMON_REAP_INTERVAL", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_REAP_INTERVAL")
			}
			got, err := reapIntervalFromEnv()
			if test.code != "" {
				if err == nil || !strings.HasPrefix(err.Error(), test.code+":") {
					t.Fatalf("interval=%v err=%v, want %s", got, err, test.code)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("interval=%v want=%v err=%v", got, test.want, err)
			}
		})
	}
}

func TestMalformedReapIntervalFailsDaemonStartup(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("AIRA_DAEMON_REAP_INTERVAL", "eventually")
	err := NewServer(paths).Serve(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
		t.Fatalf("Serve error=%v, want E_CONFIG_INVALID", err)
	}
	if _, statErr := os.Stat(paths.RuntimeDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("malformed config touched runtime directory: %v", statErr)
	}
}

func TestJournalFlushIntervalConfig(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
		code  string
	}{
		{name: "default", want: 60 * time.Second},
		{name: "empty", set: true, value: "", want: 60 * time.Second},
		{name: "duration", set: true, value: "2m", want: 2 * time.Minute},
		{name: "minimum", set: true, value: "1s", want: time.Second},
		{name: "disabled", set: true, value: "disabled", want: 0},
		{name: "zero", set: true, value: "0", want: 0},
		{name: "too small", set: true, value: "999ms", code: "E_CONFIG_INVALID"},
		{name: "malformed", set: true, value: "soon", code: "E_CONFIG_INVALID"},
		{name: "negative", set: true, value: "-1s", code: "E_CONFIG_INVALID"},
		{name: "noncanonical zero", set: true, value: "0s", code: "E_CONFIG_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.set {
				t.Setenv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL")
			}
			got, err := journalFlushIntervalFromEnv()
			if test.code != "" {
				if err == nil || !strings.HasPrefix(err.Error(), test.code+":") {
					t.Fatalf("interval=%v err=%v, want %s", got, err, test.code)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("interval=%v want=%v err=%v", got, test.want, err)
			}
		})
	}
}

func TestWatchPollIntervalConfig(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
		code  string
	}{
		{name: "default", want: 500 * time.Millisecond},
		{name: "empty", set: true, value: "", want: 500 * time.Millisecond},
		{name: "minimum", set: true, value: "250ms", want: 250 * time.Millisecond},
		{name: "in range", set: true, value: "1500ms", want: 1500 * time.Millisecond},
		{name: "below floor", set: true, value: "249ms", code: "E_CONFIG_INVALID"},
		{name: "at ceiling", set: true, value: "10s", code: "E_CONFIG_INVALID"},
		{name: "above ceiling", set: true, value: "11s", code: "E_CONFIG_INVALID"},
		{name: "malformed", set: true, value: "often", code: "E_CONFIG_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.set {
				t.Setenv("AIRA_DAEMON_WATCH_POLL_INTERVAL", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_WATCH_POLL_INTERVAL")
			}
			got, err := watchPollIntervalFromEnv()
			if test.code != "" {
				if err == nil || !strings.HasPrefix(err.Error(), test.code+":") {
					t.Fatalf("interval=%v err=%v, want %s", got, err, test.code)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("interval=%v want=%v err=%v", got, test.want, err)
			}
		})
	}
}

func TestMalformedWatchPollIntervalFailsDaemonStartupBeforeBind(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("AIRA_DAEMON_WATCH_POLL_INTERVAL", "10s")
	err := NewServer(paths).Serve(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
		t.Fatalf("Serve error=%v, want E_CONFIG_INVALID", err)
	}
	if _, statErr := os.Stat(paths.RuntimeDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("malformed config touched runtime directory: %v", statErr)
	}
}

func TestMalformedJournalFlushIntervalFailsDaemonStartupBeforeBind(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("AIRA_DAEMON_JOURNAL_FLUSH_INTERVAL", "eventually")
	err := NewServer(paths).Serve(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
		t.Fatalf("Serve error=%v, want E_CONFIG_INVALID", err)
	}
	if _, statErr := os.Stat(paths.RuntimeDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("malformed config touched runtime directory: %v", statErr)
	}
}

func TestStateIdentityResolvesSymlinkBeforeMissingSuffix(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("XDG_STATE_HOME", filepath.Join(realParent, "missing", "state"))
	realPaths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(aliasParent, "missing", "state"))
	aliasPaths, err := PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if aliasPaths.StateHome != realPaths.StateHome || aliasPaths.StateID != realPaths.StateID {
		t.Fatalf("alias state=(%q, %q), real=(%q, %q)", aliasPaths.StateHome, aliasPaths.StateID, realPaths.StateHome, realPaths.StateID)
	}
	if aliasPaths.DBPath != realPaths.DBPath || aliasPaths.SocketPath != realPaths.SocketPath || aliasPaths.LockPath != realPaths.LockPath {
		t.Fatalf("aliased paths diverged:\n alias=%+v\n real=%+v", aliasPaths, realPaths)
	}
}
