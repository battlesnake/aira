package app

import (
	"bytes"
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
	MemoryEstimate     bool     `json:"memory_estimate,omitempty"`
	AdmissionMaxWait   string   `json:"admission_max_wait,omitempty"`
	DetachReadyTimeout string   `json:"detach_ready_timeout,omitempty"`
	SupervisorLeaseTTL string   `json:"supervisor_lease_ttl,omitempty"`
	ReportMaxBytes     int64    `json:"report_max_bytes,omitempty"`
}

type ProjectConfig struct {
	Slug                string            `json:"slug"`
	Prefixes            []string          `json:"prefixes"`
	RequirementPrefixes []string          `json:"requirement_prefixes,omitempty"`
	Review              json.RawMessage   `json:"review,omitempty"`
	TestReports         TestReportsConfig `json:"test_reports,omitempty"`
	Compute             ComputeConfig     `json:"compute,omitempty"`
	Commands            CommandsConfig    `json:"commands,omitempty"`
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

type CommandsConfig struct {
	MaxEvents  int `json:"max_events,omitempty"`
	MaxAgeDays int `json:"max_age_days,omitempty"`
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

// InitPlan is a validated bootstrap descriptor. Preparing it performs git
// discovery but does not open the machine DB or write the config file, allowing
// the daemon to register it through its already-open shared DB first.
type InitPlan struct {
	Project Project
	Data    []byte
	Adopt   bool
}

// discoverGitRoot resolves the absolute worktree root, reporting the caller's
// own E_NOT_PROJECT wording so `init` and ordinary discovery keep their
// distinct messages.
func discoverGitRoot(ctx context.Context, cwd, notProject string) (string, error) {
	root, err := gitValue(ctx, cwd, "--show-toplevel")
	if err != nil {
		return "", errors.New("E_NOT_PROJECT: " + notProject)
	}
	return filepath.Abs(strings.TrimSpace(root))
}

// gitIdentity resolves the common directory, the worktree git directory and the
// two derived identities for an already-resolved root. Discover,
// DiscoverBootstrap and PrepareInit all need exactly this and nothing else;
// keeping one copy is what stops the three drifting apart.
func gitIdentity(ctx context.Context, root string) (Project, error) {
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
	canonicalCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		canonicalCommon = common
	}
	canonicalGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		canonicalGitDir = gitDir
	}
	state, err := store.DefaultStateDir()
	if err != nil {
		return Project{}, err
	}
	return Project{
		Root: root, CommonDir: common, GitDir: gitDir,
		ProjectID: hashID(canonicalCommon), WorktreeID: hashID(canonicalGitDir),
		StateDir: state,
	}, nil
}

func Discover(ctx context.Context, cwd string) (Project, error) {
	root, err := discoverGitRoot(ctx, cwd, "current directory is not a git worktree")
	if err != nil {
		return Project{}, err
	}
	project, err := gitIdentity(ctx, root)
	if err != nil {
		return Project{}, err
	}
	configPath, err := findConfig(root)
	if err != nil {
		return Project{}, err
	}
	config, err := readConfig(configPath)
	if err != nil {
		return Project{}, err
	}
	project.ConfigPath, project.Config = configPath, config
	return project, nil
}

// DiscoverBootstrap returns git identity before .aira/config exists. It is
// read-only and is used by the client to describe a routed init request.
func DiscoverBootstrap(ctx context.Context, cwd string) (Project, error) {
	root, err := discoverGitRoot(ctx, cwd, "aira init requires a git repository")
	if err != nil {
		return Project{}, err
	}
	return gitIdentity(ctx, root)
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
		Root: project.Root, CommonDir: project.CommonDir, GitDir: project.GitDir,
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
		MaxCommandEvents:    project.Config.Project.Commands.MaxEvents,
		MaxCommandAgeDays:   project.Config.Project.Commands.MaxAgeDays,
		MaxQuotaSnapshots:   project.Config.Project.Compute.MaxQuotaSnapshots,
		LeaseTTLNS:          leaseTTLNS(project.Config),
	})
	if err != nil {
		return nil, Project{}, err
	}
	project, err = BuildWithoutStore(project, diagnostics)
	if err != nil {
		_ = s.Close()
		return nil, Project{}, err
	}
	s.SetRunner(project.Runner)
	project.Runner.SetSupervisorLeaseReader(s.SupervisorLeaseLive)
	return s, project, nil
}

// OpenWithoutStore builds the local execution dependencies for a carved verb
// without opening state.db or registering the client worktree.
func OpenWithoutStore(ctx context.Context, cwd string, diagnostics io.Writer) (Project, error) {
	project, err := Discover(ctx, cwd)
	if err != nil {
		return Project{}, err
	}
	return BuildWithoutStore(project, diagnostics)
}

// BuildWithoutStore constructs local execution dependencies from one already
// discovered project snapshot. It does not rediscover or open state.db.
func BuildWithoutStore(project Project, diagnostics io.Writer) (Project, error) {
	memoryReserve, admissionMaxWait, memoryEstimate, err := parsedRunAdmission(project.Config.Run)
	if err != nil {
		return Project{}, err
	}
	project.Config.Run.MemoryEstimate = memoryEstimate
	detachReadyTimeout, err := parsedDetachReadyTimeout(project.Config.Run)
	if err != nil {
		return Project{}, err
	}
	supervisorLeaseTTL, err := parsedSupervisorLeaseTTL(project.Config.Run)
	if err != nil {
		return Project{}, err
	}
	daemonScope, err := runnerDaemonScope(project)
	if err != nil {
		return Project{}, err
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
		SupervisorLeaseTTL: supervisorLeaseTTL,
		DaemonScope:        daemonScope,
		Diagnostics:        diagnostics,
		ReportMaxBytes:     project.Config.Run.ReportMaxBytes,
	})
	if err != nil {
		return Project{}, err
	}
	project.Runner = execution
	project.GitOps = gitremote.New(resolvedGitConfig(project.Config.Git))
	project.GateAudit, err = store.OpenGateAudit(project.CommonDir, false)
	if err != nil {
		return Project{}, err
	}
	return project, nil
}

func Init(ctx context.Context, cwd string, args map[string]any) (InitResult, error) {
	plan, err := PrepareInit(ctx, cwd, args)
	if err != nil {
		return InitResult{}, err
	}
	project := plan.Project
	reviewPolicy, err := store.LoadReviewPolicy(project.Config.Project.Review)
	if err != nil {
		return InitResult{}, err
	}
	dbPath := filepath.Join(project.StateDir, "state.db")
	registryPath := filepath.Join(project.StateDir, "registry.jsonl")
	db, err := store.OpenDB(dbPath, registryPath)
	if err != nil {
		return InitResult{}, err
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	tombstoned, err := db.ProjectEjected(ctx, project.ProjectID)
	if err != nil {
		return InitResult{}, err
	}
	if plan.Adopt || tombstoned {
		configBytes, err := json.Marshal(project.Config)
		if err != nil {
			return InitResult{}, err
		}
		digest := sha256.Sum256(configBytes)
		view, err := store.NewUnregisteredScope(db, store.ScopeOptions{
			Root: project.Root, CommonDir: project.CommonDir, GitDir: project.GitDir,
			ProjectID: project.ProjectID, WorktreeID: project.WorktreeID,
			ProjectSlug: project.Config.Project.Slug, Prefixes: project.Config.Project.Prefixes,
			RequirementPrefixes: project.Config.Project.RequirementPrefixes, ReviewPolicy: reviewPolicy,
			LeaseTTLNS: leaseTTLNS(project.Config), ConfigDigest: hex.EncodeToString(digest[:]), Bootstrap: true,
		})
		if err != nil {
			return InitResult{}, err
		}
		if err := view.PreflightAdoption(ctx); err != nil {
			return InitResult{}, err
		}
		if err := view.StageAdoption(ctx); err != nil {
			return InitResult{}, err
		}
		staged := true
		defer func() {
			if staged {
				_ = view.RollbackStagedAdoption(context.Background())
			}
		}()
		if plan.Adopt {
			if err := view.Rebuild(ctx); err != nil {
				return InitResult{}, err
			}
		} else if err := CommitInit(plan); err != nil {
			return InitResult{}, err
		}
		if err := view.Register(ctx); err != nil {
			return InitResult{}, err
		}
		staged = false
		return plan.Result(cwd)
	}
	if err := db.Close(); err != nil {
		return InitResult{}, err
	}
	db = nil
	s, err := store.Open(ctx, store.Options{
		Root: project.Root, CommonDir: project.CommonDir, GitDir: project.GitDir,
		DBPath: dbPath, RegistryPath: registryPath,
		ProjectID: project.ProjectID, WorktreeID: project.WorktreeID,
		ProjectSlug: project.Config.Project.Slug, Prefixes: project.Config.Project.Prefixes,
		RequirementPrefixes: project.Config.Project.RequirementPrefixes,
		ReviewPolicy:        reviewPolicy, LeaseTTLNS: leaseTTLNS(project.Config),
	})
	if err != nil {
		return InitResult{}, err
	}
	if err := CommitInit(plan); err != nil {
		_ = s.Close()
		return InitResult{}, err
	}
	if err := s.Close(); err != nil {
		return InitResult{}, err
	}
	return plan.Result(cwd)
}

// PrepareInit validates and discovers an init bootstrap without opening state.db
// or writing .aira/config.
func PrepareInit(ctx context.Context, cwd string, args map[string]any) (InitPlan, error) {
	root, err := discoverGitRoot(ctx, cwd, "aira init requires a git repository")
	if err != nil {
		return InitPlan{}, err
	}
	configPath := filepath.Join(root, ".aira", "config")
	adopt := false
	var config Config
	var data []byte
	if existing, statErr := os.ReadFile(configPath); statErr == nil {
		committed, err := exec.CommandContext(ctx, "git", "-C", root, "show", "HEAD:.aira/config").Output()
		if err != nil {
			return InitPlan{}, errors.New("E_ALREADY_INITIALIZED: .aira/config already exists")
		}
		if !bytes.Equal(existing, committed) {
			return InitPlan{}, errors.New("E_CONFIG_INVALID: working .aira/config diverges from committed config")
		}
		config, err = readConfig(configPath)
		if err != nil {
			return InitPlan{}, err
		}
		if requested := stringArg(args, "project"); requested != "" && requested != config.Project.Slug {
			return InitPlan{}, fmt.Errorf("E_CONFIG_INVALID: committed project slug mismatch: config=%s requested=%s", config.Project.Slug, requested)
		}
		if requested := stringSlice(args, "prefixes"); len(requested) > 0 {
			for i := range requested {
				requested[i] = strings.ToUpper(requested[i])
			}
			if strings.Join(requested, "\x00") != strings.Join(config.Project.Prefixes, "\x00") {
				return InitPlan{}, fmt.Errorf("E_CONFIG_INVALID: committed project prefixes mismatch: config=%v requested=%v", config.Project.Prefixes, requested)
			}
		}
		data = committed
		adopt = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return InitPlan{}, statErr
	}
	if !adopt {
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
		config = Config{Schema: 1, Project: ProjectConfig{Slug: slug, Prefixes: prefixes}, Lease: LeaseConfig{TTLSeconds: 900, HeartbeatSeconds: 30}}
		if err := validateConfig(config); err != nil {
			return InitPlan{}, err
		}
		var err error
		data, err = json.MarshalIndent(config, "", "  ")
		if err != nil {
			return InitPlan{}, err
		}
		data = append(data, '\n')
	}
	project, err := gitIdentity(ctx, root)
	if err != nil {
		return InitPlan{}, err
	}
	project.ConfigPath, project.Config = configPath, config
	return InitPlan{Project: project, Data: data, Adopt: adopt}, nil
}

// CommitInit writes the config prepared by PrepareInit.
func CommitInit(plan InitPlan) error {
	if plan.Adopt {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(plan.Project.Root, ".aira", "tickets"), 0o755); err != nil {
		return err
	}
	return writeConfig(plan.Project.ConfigPath, plan.Data)
}

// Result presents a prepared init relative to the caller, preserving the
// existing CLI contract.
func (plan InitPlan) Result(cwd string) (InitResult, error) {
	project := plan.Project
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return InitResult{}, err
	}
	relRoot, err := filepath.Rel(cwdAbs, project.Root)
	if err != nil {
		return InitResult{}, err
	}
	relConfig, err := filepath.Rel(cwdAbs, project.ConfigPath)
	if err != nil {
		return InitResult{}, err
	}
	return InitResult{Root: filepath.ToSlash(relRoot), Config: filepath.ToSlash(relConfig), Project: project.Config.Project.Slug, Prefixes: project.Config.Project.Prefixes, Created: true}, nil
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
	if config.Project.TestReports.MaxReports < 0 || config.Project.TestReports.MaxAgeDays < 0 || config.Project.Compute.MaxEvents < 0 || config.Project.Compute.MaxAgeDays < 0 || config.Project.Compute.MaxQuotaSnapshots < 0 || config.Project.Commands.MaxEvents < 0 || config.Project.Commands.MaxAgeDays < 0 {
		return errors.New("E_CONFIG_INVALID: telemetry retention values must be non-negative")
	}
	if config.Run.ReportMaxBytes < 0 {
		return errors.New("E_CONFIG_INVALID: run.report_max_bytes must be non-negative")
	}
	if config.Run.ReportMaxBytes > store.StoreOpBodyMax {
		return fmt.Errorf("E_CONFIG_INVALID: run.report_max_bytes must be at most %d", store.StoreOpBodyMax)
	}
	if _, err := parsedDetachReadyTimeout(config.Run); err != nil {
		return err
	}
	if _, err := parsedSupervisorLeaseTTL(config.Run); err != nil {
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
	if _, _, _, err := parsedRunAdmission(config.Run); err != nil {
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

const maxMemoryEstimateReserve int64 = 1 << 50 // mirrors core.maxEstimateReserve and daemon.admitMaxReserve

func parsedRunAdmission(config RunConfig) (int64, time.Duration, bool, error) {
	slice := strings.TrimSpace(config.Slice)
	headroom := strings.TrimSpace(config.MemoryHeadroom)
	if (slice == "") != (headroom == "") {
		return 0, 0, false, errors.New("E_CONFIG_INVALID: run.slice and run.memory_headroom must be configured together")
	}
	if config.MemoryEstimate && slice == "" {
		return 0, 0, false, errors.New("E_CONFIG_INVALID: run.memory_estimate requires run.slice and run.memory_headroom")
	}
	var reserve int64
	var err error
	if headroom != "" {
		reserve, err = runner.ParseMemorySize(headroom)
		if err != nil {
			return 0, 0, false, fmt.Errorf("E_CONFIG_INVALID: run.memory_headroom: %w", err)
		}
		if reserve <= 0 {
			return 0, 0, false, errors.New("E_CONFIG_INVALID: run.memory_headroom must be a positive byte count")
		}
		if config.MemoryEstimate && reserve > maxMemoryEstimateReserve {
			return 0, 0, false, errors.New("E_CONFIG_INVALID: run.memory_headroom exceeds the memory estimate reserve cap")
		}
	}
	var maxWait time.Duration
	if value := strings.TrimSpace(config.AdmissionMaxWait); value != "" {
		maxWait, err = time.ParseDuration(value)
		if err != nil || maxWait <= 0 {
			return 0, 0, false, errors.New("E_CONFIG_INVALID: run.admission_max_wait must be a positive duration")
		}
	}
	return reserve, maxWait, config.MemoryEstimate, nil
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

func parsedSupervisorLeaseTTL(config RunConfig) (time.Duration, error) {
	value := strings.TrimSpace(config.SupervisorLeaseTTL)
	if value == "" {
		return 120 * time.Second, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil || ttl < 60*time.Second || !runner.ValidSupervisorLeaseTTL(ttl) {
		return 0, errors.New("E_CONFIG_INVALID: run.supervisor_lease_ttl violates the supervisor lease timing invariant")
	}
	return ttl, nil
}

func runnerDaemonScope(project Project) (map[string]any, error) {
	reviewPolicy, err := store.LoadReviewPolicy(project.Config.Project.Review)
	if err != nil {
		return nil, err
	}
	configBytes, err := json.Marshal(project.Config)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(configBytes)
	return map[string]any{
		"root": project.Root, "common_dir": project.CommonDir, "git_dir": project.GitDir,
		"project_id": project.ProjectID, "worktree_id": project.WorktreeID,
		"slug": project.Config.Project.Slug, "prefixes": project.Config.Project.Prefixes,
		"requirement_prefixes": project.Config.Project.RequirementPrefixes,
		"review_policy":        reviewPolicy, "review_configured": reviewPolicy.Configured,
		"max_reports": project.Config.Project.TestReports.MaxReports, "max_age_days": project.Config.Project.TestReports.MaxAgeDays,
		"max_compute_events": project.Config.Project.Compute.MaxEvents, "max_compute_age_days": project.Config.Project.Compute.MaxAgeDays,
		"max_command_events": project.Config.Project.Commands.MaxEvents, "max_command_age_days": project.Config.Project.Commands.MaxAgeDays,
		"max_quota_snapshots": project.Config.Project.Compute.MaxQuotaSnapshots,
		"lease_ttl_ns":        leaseTTLNS(project.Config), "config_digest": hex.EncodeToString(digest[:]),
	}, nil
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
	commandArgs := append([]string{"-C", dir, "rev-parse"}, args...)
	// Project discovery runs on every command and must be robust to a flaky
	// git-exec: under load a git subprocess can transiently fail, and a
	// credential-helper/fsmonitor grandchild can inherit git's stdout and keep
	// Output() blocked for EOF after git itself exits. Each attempt is bounded (a
	// context deadline kills a stuck git; WaitDelay force-closes a lingering pipe
	// and, on an otherwise-successful git, still yields the captured ref); a
	// transient failure is retried a bounded number of times with a short backoff.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		out, err := runGitRevParse(ctx, commandArgs)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return "", lastErr
}

func runGitRevParse(ctx context.Context, commandArgs []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", commandArgs...)
	// Scrub GIT_* (AIRA-93). `git -C <dir> rev-parse` names the directory
	// explicitly, but an inherited GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE OVERRIDES
	// it, so discovery silently resolves a different repository than the one it
	// was asked about — and everything downstream (project id, worktree id, the
	// common-dir receipts and journal) is then keyed to the wrong project. That
	// is not hypothetical: two receipts in this repository's own shared
	// .git/aira/receipts.jsonl were written from /tmp test working directories
	// under this project's id, colliding with its real seq 1 and 3 and making
	// `aira reconcile --rebuild` fail E_JOURNAL_CORRUPT.
	//
	// AIRA-46 scrubbed the same variables in .githooks/common.sh, but only for
	// the shell hooks; this is the one path it did not reach — the binary's own
	// git-invoking helper, which inherits whatever environment its caller had.
	command.Env = scrubGitEnvironment(os.Environ())
	command.WaitDelay = 3 * time.Second
	output, err := command.Output()
	if err != nil {
		// git itself exited successfully but a grandchild held the stdout pipe past
		// WaitDelay: the captured ref is valid, so this is not a discovery failure.
		if errors.Is(err, exec.ErrWaitDelay) && command.ProcessState != nil && command.ProcessState.Success() && len(output) > 0 {
			return string(output), nil
		}
		return "", err
	}
	return string(output), nil
}

// scrubGitEnvironment drops every GIT_* variable. It removes ALL of them rather
// than an allowlist of the three that bite hardest: git keeps adding
// environment-driven overrides (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE,
// GIT_COMMON_DIR, GIT_OBJECT_DIRECTORY, GIT_CEILING_DIRECTORIES, …), and
// discovery needs none of them — it passes `-C <dir>` and asks rev-parse a
// question about that directory alone. An allowlist would silently regrow the
// hole the next time git ships another one.
func scrubGitEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_") {
			continue
		}
		result = append(result, entry)
	}
	return result
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
