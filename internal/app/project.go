package app

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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gitremote"
	"aira/internal/runner"
	"aira/internal/store"
)

type Config struct {
	Schema  int           `json:"schema"`
	Project ProjectConfig `json:"project"`
	Lease   LeaseConfig   `json:"lease"`
	Run     RunConfig     `json:"run,omitempty"`
	Git     GitConfig     `json:"git,omitempty"`
}

type GitConfig struct {
	GhFallback               *bool `json:"gh_fallback,omitempty"`
	SSHConnectTimeoutSeconds int   `json:"ssh_connect_timeout_seconds,omitempty"`
	OpTimeoutSeconds         int   `json:"op_timeout_seconds,omitempty"`
	sshTimeoutPresent        bool
	opTimeoutPresent         bool
}

// UnmarshalJSON preserves the distinction between an absent timeout (default)
// and an explicitly configured zero (invalid).
func (c *GitConfig) UnmarshalJSON(data []byte) error {
	type wire struct {
		GhFallback               *bool `json:"gh_fallback,omitempty"`
		SSHConnectTimeoutSeconds *int  `json:"ssh_connect_timeout_seconds,omitempty"`
		OpTimeoutSeconds         *int  `json:"op_timeout_seconds,omitempty"`
	}
	var value wire
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return err
	}
	c.GhFallback = value.GhFallback
	if value.SSHConnectTimeoutSeconds != nil {
		c.SSHConnectTimeoutSeconds, c.sshTimeoutPresent = *value.SSHConnectTimeoutSeconds, true
	}
	if value.OpTimeoutSeconds != nil {
		c.OpTimeoutSeconds, c.opTimeoutPresent = *value.OpTimeoutSeconds, true
	}
	return nil
}

type RunConfig struct {
	Prefix             []string `json:"prefix,omitempty"`
	CgroupParent       string   `json:"cgroup_parent,omitempty"`
	Slice              string   `json:"slice,omitempty"`
	MemoryHeadroom     string   `json:"memory_headroom,omitempty"`
	AdmissionMaxWait   string   `json:"admission_max_wait,omitempty"`
	DetachReadyTimeout string   `json:"detach_ready_timeout,omitempty"`
	ReportMaxBytes     int64    `json:"report_max_bytes,omitempty"`
}

type ProjectConfig struct {
	Slug                string            `json:"slug"`
	Prefixes            []string          `json:"prefixes"`
	RequirementPrefixes []string          `json:"requirement_prefixes,omitempty"`
	Review              json.RawMessage   `json:"review,omitempty"`
	TestReports         TestReportsConfig `json:"test_reports,omitempty"`
	Compute             ComputeConfig     `json:"compute,omitempty"`
}

type TestReportsConfig struct {
	MaxReports int `json:"max_reports,omitempty"`
	MaxAgeDays int `json:"max_age_days,omitempty"`
}

type ComputeConfig struct {
	MaxEvents         int `json:"max_events,omitempty"`
	MaxAgeDays        int `json:"max_age_days,omitempty"`
	MaxQuotaSnapshots int `json:"max_quota_snapshots,omitempty"`
}

type LeaseConfig struct {
	TTLSeconds       int `json:"ttl_seconds"`
	HeartbeatSeconds int `json:"heartbeat_seconds"`
}

type Project struct {
	Root       string
	CommonDir  string
	GitDir     string
	ProjectID  string
	WorktreeID string
	ConfigPath string
	Config     Config
	StateDir   string
	Runner     *runner.Runner    `json:"-"`
	GitOps     *gitremote.Client `json:"-"`
	GateAudit  *store.GateAudit  `json:"-"`
}

type InitResult struct {
	Root     string   `json:"root"`
	Config   string   `json:"config"`
	Project  string   `json:"project"`
	Prefixes []string `json:"prefixes"`
	Created  bool     `json:"created"`
}

func Discover(ctx context.Context, cwd string) (Project, error) {
	root, err := gitValue(ctx, cwd, "--show-toplevel")
	if err != nil {
		return Project{}, errors.New("E_NOT_PROJECT: current directory is not a git worktree")
	}
	root, err = filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return Project{}, err
	}
	common, err := gitValue(ctx, root, "--git-common-dir")
	if err != nil {
		return Project{}, errors.New("E_NOT_PROJECT: git common directory is unavailable")
	}
	common = absoluteGitPath(root, strings.TrimSpace(common))
	gitDir, err := gitValue(ctx, root, "--git-dir")
	if err != nil {
		return Project{}, errors.New("E_NOT_PROJECT: worktree git directory is unavailable")
	}
	gitDir = absoluteGitPath(root, strings.TrimSpace(gitDir))
	configPath, err := findConfig(root)
	if err != nil {
		return Project{}, err
	}
	config, err := readConfig(configPath)
	if err != nil {
		return Project{}, err
	}
	canonicalCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		canonicalCommon = common
	}
	canonicalGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		canonicalGitDir = gitDir
	}
	return Project{
		Root: root, CommonDir: common, GitDir: gitDir,
		ProjectID: hashID(canonicalCommon), WorktreeID: hashID(canonicalGitDir),
		ConfigPath: configPath, Config: config, StateDir: stateDir(),
	}, nil
}

func Open(ctx context.Context, cwd string) (*store.Store, Project, error) {
	return OpenWithDiagnostics(ctx, cwd, nil)
}

func OpenWithDiagnostics(ctx context.Context, cwd string, diagnostics io.Writer) (*store.Store, Project, error) {
	project, err := Discover(ctx, cwd)
	if err != nil {
		return nil, Project{}, err
	}
	reviewPolicy, err := store.LoadReviewPolicy(project.Config.Project.Review)
	if err != nil {
		return nil, Project{}, err
	}
	s, err := store.Open(ctx, store.Options{
		Root: project.Root, CommonDir: project.CommonDir,
		DBPath:       filepath.Join(project.StateDir, "state.db"),
		RegistryPath: filepath.Join(project.StateDir, "registry.jsonl"),
		ProjectID:    project.ProjectID, WorktreeID: project.WorktreeID,
		ProjectSlug: project.Config.Project.Slug, Prefixes: project.Config.Project.Prefixes,
		RequirementPrefixes: project.Config.Project.RequirementPrefixes,
		ReviewPolicy:        reviewPolicy,
		MaxReports:          project.Config.Project.TestReports.MaxReports,
		MaxAgeDays:          project.Config.Project.TestReports.MaxAgeDays,
		MaxComputeEvents:    project.Config.Project.Compute.MaxEvents,
		MaxComputeAgeDays:   project.Config.Project.Compute.MaxAgeDays,
		MaxQuotaSnapshots:   project.Config.Project.Compute.MaxQuotaSnapshots,
		LeaseTTLNS:          leaseTTLNS(project.Config),
	})
	if err != nil {
		return nil, Project{}, err
	}
	memoryReserve, admissionMaxWait, err := parsedRunAdmission(project.Config.Run)
	if err != nil {
		_ = s.Close()
		return nil, Project{}, err
	}
	detachReadyTimeout, err := parsedDetachReadyTimeout(project.Config.Run)
	if err != nil {
		_ = s.Close()
		return nil, Project{}, err
	}
	execution, err := runner.New(runner.Config{
		CommonDir:          project.CommonDir,
		OutputDir:          filepath.Join(project.CommonDir, "aira", "runs", "output"),
		Owner:              project.WorktreeID,
		CgroupParent:       project.Config.Run.CgroupParent,
		Prefix:             project.Config.Run.Prefix,
		MemorySlice:        project.Config.Run.Slice,
		MemoryReserve:      memoryReserve,
		AdmissionMaxWait:   admissionMaxWait,
		DetachReadyTimeout: detachReadyTimeout,
		Diagnostics:        diagnostics,
		ReportMaxBytes:     project.Config.Run.ReportMaxBytes,
	})
	if err != nil {
		_ = s.Close()
		return nil, Project{}, err
	}
	s.SetRunner(execution)
	project.Runner = execution
	project.GitOps = gitremote.New(resolvedGitConfig(project.Config.Git))
	project.GateAudit, err = store.OpenGateAudit(project.CommonDir, false)
	if err != nil {
		_ = s.Close()
		return nil, Project{}, err
	}
	return s, project, nil
}

func Init(ctx context.Context, cwd string, args map[string]any) (InitResult, error) {
	root, err := gitValue(ctx, cwd, "--show-toplevel")
	if err != nil {
		return InitResult{}, errors.New("E_NOT_PROJECT: aira init requires a git repository")
	}
	root, err = filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return InitResult{}, err
	}
	configPath := filepath.Join(root, ".aira", "config")
	if _, err := os.Stat(configPath); err == nil {
		return InitResult{}, errors.New("E_ALREADY_INITIALIZED: .aira/config already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return InitResult{}, err
	}
	slug := stringArg(args, "project")
	if slug == "" {
		slug = strings.ToLower(filepath.Base(root))
	}
	prefixes := stringSlice(args, "prefixes")
	if len(prefixes) == 0 {
		prefixes = []string{"AIRA"}
	}
	for i := range prefixes {
		prefixes[i] = strings.ToUpper(prefixes[i])
	}
	config := Config{Schema: 1, Project: ProjectConfig{Slug: slug, Prefixes: prefixes}, Lease: LeaseConfig{TTLSeconds: 900, HeartbeatSeconds: 30}}
	if err := validateConfig(config); err != nil {
		return InitResult{}, err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return InitResult{}, err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
		return InitResult{}, err
	}
	common, err := gitValue(ctx, root, "--git-common-dir")
	if err != nil {
		return InitResult{}, errors.New("E_NOT_PROJECT: git common directory is unavailable")
	}
	common = absoluteGitPath(root, strings.TrimSpace(common))
	gitDir, err := gitValue(ctx, root, "--git-dir")
	if err != nil {
		return InitResult{}, errors.New("E_NOT_PROJECT: worktree git directory is unavailable")
	}
	gitDir = absoluteGitPath(root, strings.TrimSpace(gitDir))
	canonicalCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		canonicalCommon = common
	}
	canonicalGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		canonicalGitDir = gitDir
	}
	state := stateDir()
	reviewPolicy, err := store.LoadReviewPolicy(config.Project.Review)
	if err != nil {
		return InitResult{}, err
	}
	s, err := store.Open(ctx, store.Options{
		Root: root, CommonDir: common,
		DBPath: filepath.Join(state, "state.db"), RegistryPath: filepath.Join(state, "registry.jsonl"),
		ProjectID: hashID(canonicalCommon), WorktreeID: hashID(canonicalGitDir), ProjectSlug: slug, Prefixes: prefixes,
		ReviewPolicy: reviewPolicy,
		LeaseTTLNS:   leaseTTLNS(config),
	})
	if err != nil {
		return InitResult{}, err
	}
	if err := writeConfig(configPath, data); err != nil {
		_ = s.Close()
		return InitResult{}, err
	}
	if err := s.Close(); err != nil {
		return InitResult{}, err
	}
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return InitResult{}, err
	}
	relRoot, err := filepath.Rel(cwdAbs, root)
	if err != nil {
		return InitResult{}, err
	}
	relConfig, err := filepath.Rel(cwdAbs, configPath)
	if err != nil {
		return InitResult{}, err
	}
	return InitResult{Root: filepath.ToSlash(relRoot), Config: filepath.ToSlash(relConfig), Project: slug, Prefixes: prefixes, Created: true}, nil
}

func leaseTTLNS(config Config) uint64 {
	if config.Lease.TTLSeconds <= 0 {
		return 0
	}
	return uint64(config.Lease.TTLSeconds) * 1000 * 1000 * 1000
}

func findConfig(root string) (string, error) {
	current := root
	for {
		path := filepath.Join(current, ".aira", "config")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("E_CONFIG_MISSING: no .aira/config was found")
		}
		current = parent
	}
}

func readConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("E_CONFIG_INVALID: %w", err)
	}
	var config Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("E_CONFIG_INVALID: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("E_CONFIG_INVALID: config contains multiple JSON values")
		}
		return Config{}, fmt.Errorf("E_CONFIG_INVALID: %w", err)
	}
	if err := validateConfig(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func validateConfig(config Config) error {
	if config.Schema != 1 {
		return errors.New("E_CONFIG_INVALID: unsupported config schema")
	}
	if err := domain.ValidateProjectSlug(config.Project.Slug); err != nil {
		return err
	}
	if _, err := store.LoadReviewPolicy(config.Project.Review); err != nil {
		return err
	}
	if len(config.Project.Prefixes) == 0 {
		return errors.New("E_CONFIG_INVALID: config has no prefixes")
	}
	if config.Project.TestReports.MaxReports < 0 || config.Project.TestReports.MaxAgeDays < 0 || config.Project.Compute.MaxEvents < 0 || config.Project.Compute.MaxAgeDays < 0 || config.Project.Compute.MaxQuotaSnapshots < 0 {
		return errors.New("E_CONFIG_INVALID: test report retention values must be non-negative")
	}
	if config.Run.ReportMaxBytes < 0 {
		return errors.New("E_CONFIG_INVALID: run.report_max_bytes must be non-negative")
	}
	if _, err := parsedDetachReadyTimeout(config.Run); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, prefix := range config.Project.Prefixes {
		if len(prefix) < 2 || prefix != strings.ToUpper(prefix) {
			return fmt.Errorf("E_CONFIG_INVALID: invalid prefix %q", prefix)
		}
		for _, r := range prefix {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("E_CONFIG_INVALID: invalid prefix %q", prefix)
			}
		}
		if seen[prefix] {
			return fmt.Errorf("E_CONFIG_INVALID: duplicate prefix %q", prefix)
		}
		seen[prefix] = true
	}
	ttlSeconds := config.Lease.TTLSeconds
	if ttlSeconds == 0 {
		ttlSeconds = 900
	}
	if ttlSeconds <= 0 || ttlSeconds < 60 || ttlSeconds > 86400 {
		return errors.New("E_CONFIG_INVALID: lease ttl is outside the permitted range")
	}
	heartbeatSeconds := config.Lease.HeartbeatSeconds
	if heartbeatSeconds == 0 {
		heartbeatSeconds = 30
	}
	if heartbeatSeconds <= 0 {
		return errors.New("E_CONFIG_INVALID: heartbeat must be positive")
	}
	if heartbeatSeconds >= ttlSeconds {
		return errors.New("E_CONFIG_INVALID: heartbeat must be shorter than ttl")
	}
	if _, _, err := parsedRunAdmission(config.Run); err != nil {
		return err
	}
	if (config.Git.sshTimeoutPresent && config.Git.SSHConnectTimeoutSeconds <= 0) || (!config.Git.sshTimeoutPresent && config.Git.SSHConnectTimeoutSeconds < 0) {
		return errors.New("E_CONFIG_INVALID: git.ssh_connect_timeout_seconds must be positive when configured")
	}
	if (config.Git.opTimeoutPresent && config.Git.OpTimeoutSeconds <= 0) || (!config.Git.opTimeoutPresent && config.Git.OpTimeoutSeconds < 0) {
		return errors.New("E_CONFIG_INVALID: git.op_timeout_seconds must be positive when configured")
	}
	maxDurationSeconds := int64((1<<63 - 1) / time.Second)
	if int64(config.Git.SSHConnectTimeoutSeconds) > maxDurationSeconds || int64(config.Git.OpTimeoutSeconds) > maxDurationSeconds {
		return errors.New("E_CONFIG_INVALID: git timeout exceeds time.Duration")
	}
	return nil
}

func resolvedGitConfig(config GitConfig) gitremote.Config {
	fallback := true
	if config.GhFallback != nil {
		fallback = *config.GhFallback
	}
	sshSeconds := config.SSHConnectTimeoutSeconds
	if sshSeconds == 0 && !config.sshTimeoutPresent {
		sshSeconds = 10
	}
	opSeconds := config.OpTimeoutSeconds
	if opSeconds == 0 && !config.opTimeoutPresent {
		opSeconds = 120
	}
	return gitremote.Config{
		GhFallback: fallback, SSHConnectTimeout: time.Duration(sshSeconds) * time.Second,
		OpTimeout: time.Duration(opSeconds) * time.Second,
	}
}

func parsedRunAdmission(config RunConfig) (int64, time.Duration, error) {
	slice := strings.TrimSpace(config.Slice)
	headroom := strings.TrimSpace(config.MemoryHeadroom)
	if (slice == "") != (headroom == "") {
		return 0, 0, errors.New("E_CONFIG_INVALID: run.slice and run.memory_headroom must be configured together")
	}
	var reserve int64
	var err error
	if headroom != "" {
		reserve, err = parseByteCount(headroom)
		if err != nil {
			return 0, 0, fmt.Errorf("E_CONFIG_INVALID: run.memory_headroom: %w", err)
		}
	}
	var maxWait time.Duration
	if value := strings.TrimSpace(config.AdmissionMaxWait); value != "" {
		maxWait, err = time.ParseDuration(value)
		if err != nil || maxWait <= 0 {
			return 0, 0, errors.New("E_CONFIG_INVALID: run.admission_max_wait must be a positive duration")
		}
	}
	return reserve, maxWait, nil
}

func parsedDetachReadyTimeout(config RunConfig) (time.Duration, error) {
	value := strings.TrimSpace(config.DetachReadyTimeout)
	if value == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, errors.New("E_CONFIG_INVALID: run.detach_ready_timeout must be a positive duration")
	}
	return timeout, nil
}

func parseByteCount(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty byte count")
	}
	multiplier := int64(1)
	last := value[len(value)-1]
	switch last {
	case 'k', 'K':
		multiplier = 1 << 10
		value = value[:len(value)-1]
	case 'm', 'M':
		multiplier = 1 << 20
		value = value[:len(value)-1]
	case 'g', 'G':
		multiplier = 1 << 30
		value = value[:len(value)-1]
	case 't', 'T':
		multiplier = 1 << 40
		value = value[:len(value)-1]
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 || parsed > int64(^uint64(0)>>1)/multiplier {
		return 0, errors.New("byte count must be a positive 1024-based integer without overflow")
	}
	return parsed * multiplier, nil
}

func writeConfig(path string, data []byte) error {
	tmp := path + ".aira-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func gitValue(ctx context.Context, dir string, args ...string) (string, error) {
	// Project discovery must never hang. Two failure modes are bounded here: a
	// genuinely stuck git (killed by the context deadline), and a lingering child
	// pipe — a git credential-helper / fsmonitor grandchild can inherit git's
	// stdout and keep Output() blocked reading for EOF long after git itself exits.
	// WaitDelay force-closes that pipe shortly after exit (Go 1.20+); the captured
	// ref is still valid, so a WaitDelay on an otherwise-successful git is not a
	// discovery failure. rev-parse output is a single ref, so these bounds are safe.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	commandArgs := append([]string{"-C", dir, "rev-parse"}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.WaitDelay = 5 * time.Second
	output, err := command.Output()
	if err != nil {
		if errors.Is(err, exec.ErrWaitDelay) && command.ProcessState != nil && command.ProcessState.Success() {
			return string(output), nil
		}
		return "", err
	}
	return string(output), nil
}

func absoluteGitPath(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

func hashID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func stateDir() string {
	if value := os.Getenv("XDG_STATE_HOME"); value != "" {
		return filepath.Join(value, "aira")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "aira-state")
	}
	return filepath.Join(home, ".local", "state", "aira")
}

func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func stringSlice(args map[string]any, key string) []string {
	value := args[key]
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, item := range values {
			if value, ok := item.(string); ok {
				result = append(result, value)
			}
		}
		return result
	case string:
		if values == "" {
			return nil
		}
		return strings.Split(values, ",")
	default:
		return nil
	}
}
