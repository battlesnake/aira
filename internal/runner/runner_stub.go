//go:build !linux

package runner

import (
	"context"
	"errors"
	"path/filepath"
)

// Runner keeps the transport-facing API available on non-Linux platforms.
// Execution remains deliberately unavailable because runner scopes require
// Linux cgroup v2.
type Runner struct {
	ledger          *ledger
	outputDir       string
	owner           string
	backend         ScopeBackend
	reportMaxBytes  int64
	inputRuntimeDir string
}

func New(cfg Config) (*Runner, error) {
	if cfg.SupervisorLeaseTTL == 0 {
		cfg.SupervisorLeaseTTL = defaultSupervisorLeaseTTL
	}
	if !ValidSupervisorLeaseTTL(cfg.SupervisorLeaseTTL) {
		return nil, &LaunchError{Code: "E_CONFIG_INVALID", Err: errors.New("supervisor lease TTL violates the renewal timing invariant")}
	}
	if cfg.ReportMaxBytes == 0 {
		cfg.ReportMaxBytes = DefaultReportMaxBytes
	}
	if cfg.ReportMaxBytes < 0 {
		return nil, &LaunchError{Code: "E_CONFIG_INVALID", Err: errors.New("report max bytes must be non-negative")}
	}
	l, err := newLedger(cfg.CommonDir, cfg.OutputDir)
	if err != nil {
		return nil, err
	}
	output := cfg.OutputDir
	if output == "" {
		output = filepath.Join(l.root, "aira", "runs", "output")
	}
	backend := cfg.Backend
	if backend == nil {
		backend = newDefaultBackend(cfg.CgroupParent)
	}
	return &Runner{ledger: l, outputDir: output, owner: cfg.Owner, backend: backend, reportMaxBytes: cfg.ReportMaxBytes, inputRuntimeDir: cfg.InputRuntimeDir}, nil
}

func (r *Runner) ReportMaxBytes() int64 { return r.reportMaxBytes }

func (r *Runner) SetInputRuntimeDir(path string) { r.inputRuntimeDir = path }

func (r *Runner) SetSupervisorLeaseReader(func(context.Context, string) (bool, error)) {}

func nonLinuxRunError() error {
	return &LaunchError{Code: "E_RUN_SCOPE_UNAVAILABLE", Err: errors.New("cgroup-v2 runner is supported only on Linux")}
}

func (r *Runner) Launch(context.Context, Request) (*RunRecord, error) {
	return nil, nonLinuxRunError()
}

func (r *Runner) LaunchDetached(context.Context, Request, string) (*DetachLaunch, error) {
	return nil, nonLinuxRunError()
}

func (r *Runner) DetachOutputDir() string { return r.outputDir }

func (r *Runner) Supervise(context.Context, string, int, int) error {
	return nonLinuxRunError()
}

func (r *Runner) SuperviseRequest(context.Context, Request, int, int) (*RunRecord, error) {
	return nil, nonLinuxRunError()
}

func (r *Runner) RecordAuxTelemetry(context.Context, string, string, []string) (*RunRecord, error) {
	return nil, nonLinuxRunError()
}

func (r *Runner) SupervisorLiveness(RunRecord) SupervisorLiveness { return SupervisorUnknown }

func (r *Runner) Kill(context.Context, string, bool) (*RunRecord, error) {
	return nil, nonLinuxRunError()
}

func (r *Runner) Get(id string) (*RunRecord, error) {
	record, err := r.ledger.current(id)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *Runner) ReadOutput(context.Context, OutputRequest) (*OutputChunk, error) {
	return nil, nonLinuxRunError()
}

func (r *Runner) Reconcile(context.Context) ([]RunRecord, error) {
	return nil, nonLinuxRunError()
}

func (r *Runner) Probe(ctx context.Context) error { return r.backend.Probe(ctx) }
