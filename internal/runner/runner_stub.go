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
	ledger    *ledger
	outputDir string
	owner     string
	backend   ScopeBackend
}

func New(cfg Config) (*Runner, error) {
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
	return &Runner{ledger: l, outputDir: output, owner: cfg.Owner, backend: backend}, nil
}

func nonLinuxRunError() error {
	return &LaunchError{Code: "E_RUN_SCOPE_UNAVAILABLE", Err: errors.New("cgroup-v2 runner is supported only on Linux")}
}

func (r *Runner) Launch(context.Context, Request) (*RunRecord, error) {
	return nil, nonLinuxRunError()
}

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
