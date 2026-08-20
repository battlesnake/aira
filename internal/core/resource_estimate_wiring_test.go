package core

import (
	"context"
	"errors"
	"testing"

	"aira/internal/runner"
)

type estimateWiringRunner struct {
	configuredPrefix []string
	headroom         int64
	stats            runner.PeakRSSStats
	historyErr       error
	historyCalls     int
	launchCalls      int
	detachCalls      int
	request          runner.Request
}

func (r *estimateWiringRunner) PeakRSSHistory(context.Context, string) (runner.PeakRSSStats, bool, error) {
	r.historyCalls++
	return r.stats, true, r.historyErr
}
func (r *estimateWiringRunner) MemoryReserve() int64 { return r.headroom }
func (r *estimateWiringRunner) validate(request runner.Request) error {
	selected, err := runner.EffectivePrefix(r.configuredPrefix, request.Prefix)
	if err != nil {
		return err
	}
	_, err = runner.EffectiveArgv(selected, request.Argv)
	return err
}
func (r *estimateWiringRunner) Launch(_ context.Context, request runner.Request) (*runner.RunRecord, error) {
	r.launchCalls++
	r.request = request
	if err := r.validate(request); err != nil {
		return nil, err
	}
	exit := 0
	return &runner.RunRecord{ID: "RUN-1", Status: runner.StatusExited, ExitCode: &exit}, nil
}
func (r *estimateWiringRunner) LaunchDetached(_ context.Context, request runner.Request, _ string) (*runner.DetachLaunch, error) {
	r.detachCalls++
	r.request = request
	if err := r.validate(request); err != nil {
		return nil, err
	}
	return runner.NewDetachLaunch(runner.RunRecord{ID: "RUN-2", Status: runner.StatusStarting}, nil), nil
}
func (*estimateWiringRunner) DetachOutputDir() string { return "" }
func (*estimateWiringRunner) Kill(context.Context, string, bool) (*runner.RunRecord, error) {
	return nil, nil
}
func (*estimateWiringRunner) Get(string) (*runner.RunRecord, error) { return nil, nil }
func (*estimateWiringRunner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return nil, nil
}
func (*estimateWiringRunner) Reconcile(context.Context) ([]runner.RunRecord, error) { return nil, nil }

func TestMemoryEstimateWiresSignatureOverrideAndFallbacks(t *testing.T) {
	for _, test := range []struct {
		name    string
		stats   runner.PeakRSSStats
		readErr error
		reserve *int64
		basis   string
		detach  bool
	}{
		{name: "foreground estimate", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 100}, reserve: int64PointerCore(115), basis: "estimate:max=100,n=3,f=115"},
		{name: "detached estimate", stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 100}, reserve: int64PointerCore(115), basis: "estimate:max=100,n=3,f=115", detach: true},
		{name: "no history", basis: "fallback:no-history"},
		{name: "read error", readErr: errors.New("broken db"), basis: "fallback:read-error"},
		{name: "read timeout", readErr: context.DeadlineExceeded, basis: "fallback:read-timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &estimateWiringRunner{headroom: 500, stats: test.stats, historyErr: test.readErr}
			args := map[string]any{"argv": []string{"go", "test", "./..."}}
			if test.detach {
				args["detach"] = true
			}
			response := NewWithRunner(nil, r).WithMemoryEstimate(true).Do(context.Background(), Request{Verb: "run", Args: args})
			if !response.OK {
				t.Fatalf("response=%+v", response)
			}
			wantSignature, err := resourceSignature(nil, nil, []string{"go", "test", "./..."})
			if err != nil {
				t.Fatal(err)
			}
			if r.historyCalls != 1 || r.request.ResourceSignature != wantSignature || r.request.MemoryReserveBasis != test.basis || !equalInt64PointerCore(r.request.MemoryReserveOverride, test.reserve) {
				t.Fatalf("historyCalls=%d request=%+v", r.historyCalls, r.request)
			}
		})
	}
}

func TestMemoryEstimateOffDoesNoSignatureOrHistoryWork(t *testing.T) {
	r := &estimateWiringRunner{headroom: 500, stats: runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 100}}
	response := NewWithRunner(nil, r).WithMemoryEstimate(false).Do(context.Background(), Request{Verb: "run", Args: map[string]any{"argv": []string{"go", "test", "./..."}}})
	if !response.OK || r.historyCalls != 0 || r.request.ResourceSignature != "" || r.request.MemoryReserveOverride != nil || r.request.MemoryReserveBasis != "" {
		t.Fatalf("response=%+v historyCalls=%d request=%+v", response, r.historyCalls, r.request)
	}
}

func TestMemoryEstimateWithoutHistorianFallsBackToStatic(t *testing.T) {
	r := &faceRunner{}
	response := NewWithRunner(nil, r).WithMemoryEstimate(true).Do(context.Background(), Request{Verb: "run", Args: map[string]any{"argv": []string{"go", "test", "./..."}}})
	if !response.OK || r.request.ResourceSignature == "" || r.request.MemoryReserveOverride != nil || r.request.MemoryReserveBasis != "fallback:read-error" {
		t.Fatalf("response=%+v request=%+v", response, r.request)
	}
}

func TestSignatureErrorRemainsRunnerCanonicalInForegroundAndDetached(t *testing.T) {
	for _, detached := range []bool{false, true} {
		for _, test := range []struct {
			name             string
			argv             []string
			configuredPrefix []string
			code             string
		}{
			{name: "empty argv", argv: []string{}, code: "E_RUN_ARGUMENT_INVALID"},
			{name: "malformed prefix", argv: []string{"true"}, configuredPrefix: []string{"timeout", "--", "600"}, code: "E_RUN_PREFIX_INVALID"},
		} {
			t.Run(test.name+map[bool]string{false: "/foreground", true: "/detached"}[detached], func(t *testing.T) {
				r := &estimateWiringRunner{configuredPrefix: test.configuredPrefix, headroom: 500}
				args := map[string]any{"argv": test.argv, "detach": detached}
				response := NewWithRunner(nil, r).WithCommandPrefix(test.configuredPrefix).WithMemoryEstimate(true).Do(context.Background(), Request{Verb: "run", Args: args})
				if response.Code != test.code || r.historyCalls != 0 {
					t.Fatalf("response=%+v historyCalls=%d", response, r.historyCalls)
				}
				if r.request.ResourceSignature != "" || r.request.MemoryReserveOverride != nil || r.request.MemoryReserveBasis != "" {
					t.Fatalf("signature error leaked estimate fields: %+v", r.request)
				}
			})
		}
	}
}

func int64PointerCore(value int64) *int64 { return &value }

func equalInt64PointerCore(left, right *int64) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
