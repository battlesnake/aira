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

func TestRegistryDiscoveryIntervalConfig(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
		code  string
	}{
		{name: "default", want: 30 * time.Second},
		{name: "empty", set: true, value: "", want: 30 * time.Second},
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
				t.Setenv("AIRA_DAEMON_DISCOVERY_INTERVAL", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_DISCOVERY_INTERVAL")
			}
			got, err := registryDiscoveryIntervalFromEnv()
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

func TestScopeReapIntervalConfig(t *testing.T) {
	// The "default" case os.Unsetenv's the var (t.Setenv cannot unset); restore the
	// TestMain-set value afterwards so later daemon tests keep the reaper inert.
	if orig, had := os.LookupEnv("AIRA_DAEMON_SCOPE_REAP_INTERVAL"); had {
		t.Cleanup(func() { _ = os.Setenv("AIRA_DAEMON_SCOPE_REAP_INTERVAL", orig) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv("AIRA_DAEMON_SCOPE_REAP_INTERVAL") })
	}
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
		code  string
	}{
		{name: "default", want: 5 * time.Minute},
		{name: "duration", set: true, value: "2m", want: 2 * time.Minute},
		{name: "disabled", set: true, value: "disabled", want: 0},
		{name: "zero", set: true, value: "0", want: 0},
		{name: "too small", set: true, value: "500ms", code: "E_CONFIG_INVALID"},
		{name: "malformed", set: true, value: "garbage", code: "E_CONFIG_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.set {
				t.Setenv("AIRA_DAEMON_SCOPE_REAP_INTERVAL", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_SCOPE_REAP_INTERVAL")
			}
			got, err := scopeReapIntervalFromEnv()
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

func TestMalformedRegistryDiscoveryIntervalFailsDaemonStartup(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("AIRA_DAEMON_DISCOVERY_INTERVAL", "eventually")
	err := NewServer(paths).Serve(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
		t.Fatalf("Serve error=%v, want E_CONFIG_INVALID", err)
	}
	if _, statErr := os.Stat(paths.RuntimeDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("malformed config touched runtime directory: %v", statErr)
	}
}

func TestWatchdogConfigFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, mode, interval string
		wantMode             watchdogMode
		wantInterval         time.Duration
		wantErr              bool
	}{
		{name: "defaults", wantMode: watchdogOff, wantInterval: 2 * time.Second},
		{name: "observe", mode: "observe", interval: "1s", wantMode: watchdogObserve, wantInterval: time.Second},
		{name: "enforce", mode: "enforce", interval: "29.999s", wantMode: watchdogEnforce, wantInterval: 29999 * time.Millisecond},
		{name: "bad mode", mode: "yes", wantErr: true},
		{name: "too short", interval: "999ms", wantErr: true},
		{name: "upper bound", interval: "30s", wantErr: true},
		{name: "malformed while off", mode: "off", interval: "bad", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AIRA_DAEMON_WATCHDOG_MODE", tc.mode)
			t.Setenv("AIRA_DAEMON_WATCHDOG_INTERVAL", tc.interval)
			mode, modeErr := watchdogModeFromEnv()
			interval, intervalErr := watchdogIntervalFromEnv()
			if (modeErr != nil || intervalErr != nil) != tc.wantErr {
				t.Fatalf("mode=%q interval=%v errors=(%v,%v) wantErr=%v", mode, interval, modeErr, intervalErr, tc.wantErr)
			}
			if !tc.wantErr && (mode != tc.wantMode || interval != tc.wantInterval) {
				t.Fatalf("mode=%q interval=%v want %q %v", mode, interval, tc.wantMode, tc.wantInterval)
			}
		})
	}
}

func TestMalformedWatchdogIntervalFailsDaemonStartupEvenWhenOff(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("AIRA_DAEMON_WATCHDOG_MODE", "off")
	t.Setenv("AIRA_DAEMON_WATCHDOG_INTERVAL", "eventually")
	err := NewServer(paths).Serve(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
		t.Fatalf("Serve error=%v, want E_CONFIG_INVALID", err)
	}
	if _, statErr := os.Stat(paths.RuntimeDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("malformed config touched runtime directory: %v", statErr)
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

func TestAdmitPollIntervalConfig(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  time.Duration
		code  string
	}{
		{name: "default", want: 250 * time.Millisecond},
		{name: "empty", set: true, value: "", want: 250 * time.Millisecond},
		{name: "minimum", set: true, value: "250ms", want: 250 * time.Millisecond},
		{name: "in range", set: true, value: "1500ms", want: 1500 * time.Millisecond},
		{name: "below floor", set: true, value: "249ms", code: "E_CONFIG_INVALID"},
		{name: "at ceiling", set: true, value: "10s", code: "E_CONFIG_INVALID"},
		{name: "malformed", set: true, value: "often", code: "E_CONFIG_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.set {
				t.Setenv("AIRA_DAEMON_ADMIT_POLL_INTERVAL", test.value)
			} else {
				_ = os.Unsetenv("AIRA_DAEMON_ADMIT_POLL_INTERVAL")
			}
			got, err := admitPollIntervalFromEnv()
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

func TestMalformedAdmitPollIntervalFailsDaemonStartupBeforeBind(t *testing.T) {
	paths := testPaths(t)
	t.Setenv("AIRA_DAEMON_ADMIT_POLL_INTERVAL", "10s")
	err := NewServer(paths).Serve(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
		t.Fatalf("Serve error=%v, want E_CONFIG_INVALID", err)
	}
	if _, statErr := os.Stat(paths.RuntimeDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("malformed config touched runtime directory: %v", statErr)
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
