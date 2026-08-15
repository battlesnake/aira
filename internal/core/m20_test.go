package core

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"aira/internal/runner"
)

type m20Runner struct {
	detachCalls int
	launchCalls int
	completed   []bool
	request     runner.Request
	wiringPath  string
	outputDir   string
	detachErr   error
}

func (r *m20Runner) Launch(context.Context, runner.Request) (*runner.RunRecord, error) {
	r.launchCalls++
	return nil, errors.New("foreground launch must not run")
}
func (r *m20Runner) LaunchDetached(_ context.Context, req runner.Request, wiringPath string) (*runner.DetachLaunch, error) {
	r.detachCalls++
	r.request = req
	r.wiringPath = wiringPath
	if r.detachErr != nil {
		return nil, r.detachErr
	}
	record := runner.RunRecord{ID: "RUN-20", Status: runner.StatusStarting, Detached: req.Detach}
	return runner.NewDetachLaunch(record, func(delivered bool) error {
		r.completed = append(r.completed, delivered)
		return nil
	}), nil
}
func (r *m20Runner) DetachOutputDir() string {
	return r.outputDir
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
	if !ok || record.ID != "RUN-20" || record.Status != runner.StatusStarting || !record.Detached || record.Telemetry != TelemetryNotRequested || data.Wiring.Compute.Tokens != "unevaluated" || data.Wiring.WiringComplete {
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
		"strict wiring": {"strict_wiring": true},
		"usage stdin":   {"usage": "-"},
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

func TestM20bDetachWiringFlagsProducePendingHandleAndCleanupSidecar(t *testing.T) {
	t.Run("ack cancel", func(t *testing.T) {
		execution := &m20Runner{outputDir: t.TempDir()}
		response := NewWithRunner(&m19Store{}, execution).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
			"argv": []string{"/bin/true"}, "detach": true, "report": "go-json", "suite": "unit", "config_env": []string{"A=one"},
		}})
		data, ok := response.Data.(runResponseData)
		if !response.OK || !ok || data.Telemetry != TelemetryPending || execution.request.TelemetryPending != TelemetryPending || execution.wiringPath == "" {
			t.Fatalf("response=%+v runner=%+v", response, execution)
		}
		if _, err := os.Stat(execution.wiringPath); err != nil {
			t.Fatalf("sidecar missing before simulated shim consumption: %v", err)
		}
		if err := response.AfterWrite(false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(execution.wiringPath); !os.IsNotExist(err) {
			t.Fatalf("ACK cancel did not remove sidecar: %v", err)
		}
	})
	t.Run("launch failure", func(t *testing.T) {
		execution := &m20Runner{outputDir: t.TempDir(), detachErr: errors.New("injected")}
		response := NewWithRunner(&m19Store{}, execution).Do(context.Background(), Request{Verb: "run", Args: map[string]any{
			"argv": []string{"/bin/true"}, "detach": true, "tool": "codex",
		}})
		if response.OK || execution.wiringPath == "" {
			t.Fatalf("response=%+v runner=%+v", response, execution)
		}
		if _, err := os.Stat(execution.wiringPath); !os.IsNotExist(err) {
			t.Fatalf("launch failure did not remove sidecar: %v", err)
		}
	})
}

func TestM20GetPollsDetachedRunHandle(t *testing.T) {
	response := NewWithRunner(nil, &m20Runner{}).Do(context.Background(), Request{Verb: "show", Args: map[string]any{"selector": "RUN-20"}})
	record, ok := response.Data.(*runner.RunRecord)
	if !response.OK || !ok || record.ID != "RUN-20" || record.Status != runner.StatusRunning {
		t.Fatalf("run get response=%+v", response)
	}
}
