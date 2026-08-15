package core

import (
	"bytes"
	"context"
	"testing"

	"aira/internal/runner"
)

type faceRunner struct {
	request runner.Request
}

func (r *faceRunner) Launch(_ context.Context, request runner.Request) (*runner.RunRecord, error) {
	r.request = request
	return &runner.RunRecord{ID: "RUN-1", Status: runner.StatusExited}, nil
}
func (*faceRunner) Kill(context.Context, string, bool) (*runner.RunRecord, error) { return nil, nil }
func (*faceRunner) Get(string) (*runner.RunRecord, error)                         { return nil, nil }
func (*faceRunner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return nil, nil
}
func (*faceRunner) Reconcile(context.Context) ([]runner.RunRecord, error) { return nil, nil }

func TestRunnerFaceWiresSeparateLiveSinksWithoutMutatingCore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &faceRunner{}
	core := NewWithRunnerFace(nil, fake, nil, FaceOutput{Stdout: &stdout, Stderr: &stderr, Live: true})
	response := core.Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "merge": false,
	}})
	if !response.OK || fake.request.LiveStdout != &stdout || fake.request.LiveStderr != &stderr {
		t.Fatalf("separate face response=%+v request=%+v", response, fake.request)
	}
	// A face is immutable across requests; changing a caller's request cannot
	// toggle live output on the Core itself.
	fake.request = runner.Request{}
	response = core.Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "merge": true,
	}})
	if !response.OK || fake.request.LiveStdout != &stdout || fake.request.LiveStderr != nil {
		t.Fatalf("merged face response=%+v request=%+v", response, fake.request)
	}
}

func TestRunnerFaceDefaultsAndJSONStyleFaceSuppressLiveSinks(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &faceRunner{}
	core := NewWithRunnerFace(nil, fake, nil, FaceOutput{Stdout: &stdout, Stderr: &stderr})
	response := core.Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"},
	}})
	if !response.OK || fake.request.LiveStdout != nil || fake.request.LiveStderr != nil {
		t.Fatalf("suppressed face response=%+v request=%+v", response, fake.request)
	}
}

type killFaceRunner struct {
	faceRunner
	steal  bool
	record *runner.RunRecord
	err    error
}

func (r *killFaceRunner) Kill(_ context.Context, _ string, steal bool) (*runner.RunRecord, error) {
	r.steal = steal
	return r.record, r.err
}

func TestRunKillForeignOwnerReturnsStructuredRefusalAndThreadsSteal(t *testing.T) {
	refusedRecord := &runner.RunRecord{ID: "RUN-1", Owner: "A", ErrorCodes: []string{"existing"}}
	fake := &killFaceRunner{record: refusedRecord, err: &runner.ForeignOwnerError{RunID: "RUN-1", Owner: "A", CallerOwner: "B"}}
	dispatcher := NewWithRunner(nil, fake)
	response := dispatcher.Do(context.Background(), Request{Verb: "run-kill", Args: map[string]any{"run_id": "RUN-1"}})
	data, ok := response.Data.(map[string]any)
	if response.OK || response.Code != "E_RUN_FOREIGN_OWNER" || response.Exit != 1 || !ok {
		t.Fatalf("foreign response=%+v", response)
	}
	if data["run_id"] != "RUN-1" || data["owner"] != "A" || data["caller_owner"] != "B" || data["hint"] != "pass --steal to override" {
		t.Fatalf("foreign payload=%#v", data)
	}
	if len(refusedRecord.ErrorCodes) != 1 || refusedRecord.ErrorCodes[0] != "existing" {
		t.Fatalf("refusal mutated error codes: %+v", refusedRecord.ErrorCodes)
	}

	fake.err = nil
	fake.record = &runner.RunRecord{ID: "RUN-1", Status: runner.StatusKilled, ErrorCodes: []string{"E_RUN_KILLED"}}
	response = dispatcher.Do(context.Background(), Request{Verb: "run-kill", Args: map[string]any{"run_id": "RUN-1", "steal": true}})
	if !fake.steal || response.Code != "E_RUN_KILLED" {
		t.Fatalf("steal=%v response=%+v", fake.steal, response)
	}
}
