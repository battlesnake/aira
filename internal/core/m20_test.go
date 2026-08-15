package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"aira/internal/runner"
)

type m20Runner struct {
	detachCalls int
	launchCalls int
	completed   []bool
	request     runner.Request
}

func (r *m20Runner) Launch(context.Context, runner.Request) (*runner.RunRecord, error) {
	r.launchCalls++
	return nil, errors.New("foreground launch must not run")
}
func (r *m20Runner) LaunchDetached(_ context.Context, req runner.Request) (*runner.DetachLaunch, error) {
	r.detachCalls++
	r.request = req
	record := runner.RunRecord{ID: "RUN-20", Status: runner.StatusStarting, Detached: req.Detach}
	return runner.NewDetachLaunch(record, func(delivered bool) error {
		r.completed = append(r.completed, delivered)
		return nil
	}), nil
}
func (*m20Runner) Kill(context.Context, string, bool) (*runner.RunRecord, error) { return nil, nil }
func (*m20Runner) Get(id string) (*runner.RunRecord, error) {
	return &runner.RunRecord{ID: id, Status: runner.StatusRunning, Detached: true}, nil
}
func (*m20Runner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return nil, nil
}
func (*m20Runner) Reconcile(context.Context) ([]runner.RunRecord, error) { return nil, nil }

func TestM20DetachReturnsStartingHandleAndDefersACKUntilWrite(t *testing.T) {
	execution := &m20Runner{}
	response := NewWithRunner(nil, execution).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"/bin/true"}, "detach": true, "no_stdin": true, "timeout": "2s",
	}})
	if !response.OK || response.AfterWrite == nil || execution.detachCalls != 1 || execution.launchCalls != 0 {
		t.Fatalf("response=%+v runner=%+v", response, execution)
	}
	data, ok := response.Data.(runResponseData)
	record := data.RunRecord
	if !ok || record.ID != "RUN-20" || record.Status != runner.StatusStarting || !record.Detached || data.Wiring.Compute.Tokens != "unevaluated" || data.Wiring.WiringComplete {
		t.Fatalf("detach handle=%#v", response.Data)
	}
	if len(execution.completed) != 0 {
		t.Fatalf("ACK completed before handle write: %v", execution.completed)
	}
	if execution.request.Timeout != 2*time.Second || execution.request.StdinPath != "" {
		t.Fatalf("detached request=%+v", execution.request)
	}
	if err := response.AfterWrite(true); err != nil {
		t.Fatal(err)
	}
	if len(execution.completed) != 1 || !execution.completed[0] {
		t.Fatalf("ACK completion=%v", execution.completed)
	}
}

func TestM20DetachFlagRejectionMatrixPrecedesReservation(t *testing.T) {
	cases := map[string]map[string]any{
		"follow":        {"follow": true},
		"pty":           {"pty": true},
		"stdin dash":    {"stdin": "-"},
		"report":        {"report": "go-json"},
		"report stream": {"report_stream": "out"},
		"suite":         {"suite": "unit"},
		"shard":         {"shard": "1/2"},
		"retry":         {"retry": "1"},
		"usage":         {"usage": "usage.json"},
		"provider":      {"provider": "codex"},
		"tool":          {"tool": "codex"},
		"config env":    {"config_env": []string{"SECRET=value"}},
		"strict wiring": {"strict_wiring": true},
	}
	for name, conflict := range cases {
		t.Run(name, func(t *testing.T) {
			execution := &m20Runner{}
			args := map[string]any{"argv": []string{"/bin/true"}, "detach": true}
			for key, value := range conflict {
				args[key] = value
			}
			response := NewWithRunner(nil, execution).Do(context.Background(), Request{Verb: "run", Args: args})
			if response.Code != "E_RUN_ARGUMENT_INVALID" || execution.detachCalls != 0 || execution.launchCalls != 0 {
				t.Fatalf("response=%+v runner=%+v", response, execution)
			}
		})
	}
}

func TestM20GetPollsDetachedRunHandle(t *testing.T) {
	response := NewWithRunner(nil, &m20Runner{}).Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": "RUN-20"}})
	record, ok := response.Data.(*runner.RunRecord)
	if !response.OK || !ok || record.ID != "RUN-20" || record.Status != runner.StatusRunning {
		t.Fatalf("run get response=%+v", response)
	}
}
