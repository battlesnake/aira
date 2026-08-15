package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInitCreatesRegisteredProjectAndRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	result, err := Init(context.Background(), root, map[string]any{"project": "demo", "prefixes": "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.Project != "demo" {
		t.Fatalf("init result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "tickets")); err != nil {
		t.Fatal(err)
	}
	opened, _, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if _, err := Init(context.Background(), root, map[string]any{"project": "demo", "prefixes": "DEMO"}); !strings.Contains(err.Error(), "E_ALREADY_INITIALIZED") {
		t.Fatalf("second init error = %v", err)
	}
	if filepath.IsAbs(result.Root) || filepath.IsAbs(result.Config) || result.Root != "." || result.Config != ".aira/config" {
		t.Fatalf("init leaked absolute paths: %#v", result)
	}
}

func TestOpenWiresNonemptyWorktreeOwnerIntoRunner(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := Init(context.Background(), root, map[string]any{"project": "demo", "prefixes": "DEMO"}); err != nil {
		t.Fatal(err)
	}
	opened, project, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if project.Runner == nil || project.WorktreeID == "" {
		t.Fatalf("open project=%+v", project)
	}
	owner := reflect.ValueOf(project.Runner).Elem().FieldByName("owner").String()
	if owner == "" || owner != project.WorktreeID {
		t.Fatalf("runner owner=%q worktree=%q", owner, project.WorktreeID)
	}
}

func TestRunAdmissionConfigParsesBytesAndDuration(t *testing.T) {
	reserve, maxWait, err := parsedRunAdmission(RunConfig{Slice: "whale.slice", MemoryHeadroom: "4G", AdmissionMaxWait: "30m"})
	if err != nil {
		t.Fatal(err)
	}
	if reserve != 4*1024*1024*1024 || maxWait != 30*time.Minute {
		t.Fatalf("reserve=%d maxWait=%s", reserve, maxWait)
	}
	reserve, _, err = parsedRunAdmission(RunConfig{Slice: "whale.slice", MemoryHeadroom: "1073741824"})
	if err != nil || reserve != 1024*1024*1024 {
		t.Fatalf("plain bytes reserve=%d err=%v", reserve, err)
	}
	reserve, _, err = parsedRunAdmission(RunConfig{Slice: "whale.slice", MemoryHeadroom: "512M"})
	if err != nil || reserve != 512*1024*1024 {
		t.Fatalf("megabytes reserve=%d err=%v", reserve, err)
	}
}

func TestRunAdmissionConfigRejectsMalformedAndHalfConfig(t *testing.T) {
	for name, run := range map[string]RunConfig{
		"slice only":        {Slice: "whale.slice"},
		"headroom only":     {MemoryHeadroom: "4G"},
		"zero":              {Slice: "whale.slice", MemoryHeadroom: "0"},
		"negative":          {Slice: "whale.slice", MemoryHeadroom: "-1"},
		"malformed":         {Slice: "whale.slice", MemoryHeadroom: "4GB"},
		"overflow":          {Slice: "whale.slice", MemoryHeadroom: "9223372036854775807G"},
		"zero duration":     {Slice: "whale.slice", MemoryHeadroom: "4G", AdmissionMaxWait: "0s"},
		"negative duration": {Slice: "whale.slice", MemoryHeadroom: "4G", AdmissionMaxWait: "-1s"},
		"bad duration":      {Slice: "whale.slice", MemoryHeadroom: "4G", AdmissionMaxWait: "soon"},
	} {
		t.Run(name, func(t *testing.T) {
			base := Config{Schema: 1, Project: ProjectConfig{Slug: "demo", Prefixes: []string{"DEMO"}}, Lease: LeaseConfig{TTLSeconds: 900, HeartbeatSeconds: 30}, Run: run}
			if err := validateConfig(base); err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
				t.Fatalf("validateConfig()=%v", err)
			}
		})
	}
}

func TestInitPrefixConflictDoesNotLeaveConfigScaffold(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	first := t.TempDir()
	second := t.TempDir()
	for _, root := range []string{first, second} {
		if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Init(context.Background(), first, map[string]any{"project": "first", "prefixes": "SHARED"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(context.Background(), second, map[string]any{"project": "second", "prefixes": "SHARED"}); !strings.Contains(err.Error(), "E_PREFIX_OWNERSHIP_CONFLICT") {
		t.Fatalf("prefix conflict = %v", err)
	}
	if _, err := os.Stat(filepath.Join(second, ".aira", "config")); !os.IsNotExist(err) {
		t.Fatalf("conflicting init left config: %v", err)
	}
}

func TestInitFromSubdirectoryReportsPathsRelativeToCWD(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "subdir")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	result, err := Init(context.Background(), cwd, map[string]any{"project": "demo", "prefixes": "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.Rel(cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	wantConfig, err := filepath.Rel(cwd, filepath.Join(root, ".aira", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Root != filepath.ToSlash(wantRoot) || result.Config != filepath.ToSlash(wantConfig) {
		t.Fatalf("subdir init paths = %#v, want root=%q config=%q", result, wantRoot, wantConfig)
	}
}

func TestValidateConfigRejectsInvalidLeaseTiming(t *testing.T) {
	base := Config{
		Schema:  1,
		Project: ProjectConfig{Slug: "demo", Prefixes: []string{"DEMO"}},
		Lease:   LeaseConfig{TTLSeconds: 900, HeartbeatSeconds: 30},
	}
	for name, config := range map[string]Config{
		"negative heartbeat": func() Config {
			config := base
			config.Lease.HeartbeatSeconds = -1
			return config
		}(),
		"negative ttl": func() Config {
			config := base
			config.Lease.TTLSeconds = -1
			return config
		}(),
		"heartbeat exceeds effective default ttl": func() Config {
			config := base
			config.Lease.TTLSeconds = 0
			config.Lease.HeartbeatSeconds = 3600
			return config
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateConfig(config); err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
				t.Fatalf("validateConfig(%s) = %v", name, err)
			}
		})
	}
}

func TestGitConfigPresenceDefaultsAndExplicitFalse(t *testing.T) {
	base := `{"schema":1,"project":{"slug":"demo","prefixes":["DEMO"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}`
	for _, tc := range []struct {
		name, suffix string
		fallback     bool
		ssh, op      time.Duration
	}{
		{name: "block absent", suffix: `}`, fallback: true, ssh: 10 * time.Second, op: 120 * time.Second},
		{name: "fields absent", suffix: `,"git":{}}`, fallback: true, ssh: 10 * time.Second, op: 120 * time.Second},
		{name: "explicit false", suffix: `,"git":{"gh_fallback":false,"ssh_connect_timeout_seconds":3,"op_timeout_seconds":9}}`, fallback: false, ssh: 3 * time.Second, op: 9 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte(base+tc.suffix), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			got := resolvedGitConfig(config.Git)
			if got.GhFallback != tc.fallback || got.SSHConnectTimeout != tc.ssh || got.OpTimeout != tc.op {
				t.Fatalf("resolved=%+v", got)
			}
		})
	}
}

func TestGitConfigExplicitNonpositiveTimeoutFailsEagerly(t *testing.T) {
	base := `{"schema":1,"project":{"slug":"demo","prefixes":["DEMO"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30},"git":`
	for _, block := range []string{
		`{"ssh_connect_timeout_seconds":0}}`, `{"ssh_connect_timeout_seconds":-1}}`,
		`{"op_timeout_seconds":0}}`, `{"op_timeout_seconds":-1}}`,
	} {
		path := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(path, []byte(base+block), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readConfig(path); err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID:") {
			t.Fatalf("block=%s error=%v", block, err)
		}
	}
}

func TestOpenBuildsGitOpsFromProjectConfig(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := Init(context.Background(), root, map[string]any{"project": "demo", "prefixes": "DEMO"}); err != nil {
		t.Fatal(err)
	}
	opened, project, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if project.GitOps == nil {
		t.Fatal("Open left GitOps nil")
	}
}
