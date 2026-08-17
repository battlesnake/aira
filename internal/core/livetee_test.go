package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"testing"

	"aira/internal/runner"
)

type faceRunner struct {
	request      runner.Request
	inputRequest runner.RunInputRequest
	inputData    []byte
	inputCalls   int
	launchCalls  int
}

func (r *faceRunner) Launch(_ context.Context, request runner.Request) (*runner.RunRecord, error) {
	r.launchCalls++
	r.request = request
	return &runner.RunRecord{ID: "RUN-1", Status: runner.StatusExited}, nil
}

func TestRunStdinConnectCoreValidationRejectsEveryConflictingFaceOption(t *testing.T) {
	cases := map[string]map[string]any{
		"requires detach": {"stdin_connect": true},
		"stdin":           {"stdin_connect": true, "detach": true, "stdin": "input"},
		"no stdin":        {"stdin_connect": true, "detach": true, "no_stdin": true},
		"pty":             {"stdin_connect": true, "detach": true, "pty": true},
		"store stdin":     {"stdin_connect": true, "detach": true, "store_stdin": true},
	}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &faceRunner{}
			options["argv"] = []string{"child"}
			response := NewWithRunner(nil, fake).Do(context.Background(), Request{Verb: "run", Args: options})
			if response.Code != "E_RUN_ARGUMENT_INVALID" || fake.launchCalls != 0 {
				t.Fatalf("response=%+v launchCalls=%d", response, fake.launchCalls)
			}
		})
	}
}
func (*faceRunner) Kill(context.Context, string, bool) (*runner.RunRecord, error) { return nil, nil }
func (*faceRunner) Get(string) (*runner.RunRecord, error)                         { return nil, nil }
func (*faceRunner) ReadOutput(context.Context, runner.OutputRequest) (*runner.OutputChunk, error) {
	return nil, nil
}
func (*faceRunner) Reconcile(context.Context) ([]runner.RunRecord, error) { return nil, nil }
func (r *faceRunner) Input(_ context.Context, request runner.RunInputRequest) (*runner.RunInputResult, error) {
	r.inputCalls++
	r.inputRequest = request
	if request.Reader != nil {
		r.inputData, _ = io.ReadAll(request.Reader)
	}
	return &runner.RunInputResult{RunID: request.RunID, Accepted: int64(len(r.inputData)), Closed: request.Close}, nil
}

func TestRunInputFaceStreamsCLIBytesAndBoundsMCPData(t *testing.T) {
	fake := &faceRunner{}
	dispatcher := NewWithRunnerFace(nil, fake, bytes.NewReader([]byte{0, 1, 0xff}), FaceOutput{})
	response := dispatcher.Do(context.Background(), Request{Verb: "run-input", Args: map[string]any{"run_id": "RUN-1", "close": true, "steal": true}})
	if !response.OK || !bytes.Equal(fake.inputData, []byte{0, 1, 0xff}) || !fake.inputRequest.Close || !fake.inputRequest.Steal {
		t.Fatalf("response=%+v request=%+v data=%x", response, fake.inputRequest, fake.inputData)
	}

	overCap := base64.StdEncoding.EncodeToString(make([]byte, runner.MaxRunInputFrameBytes+1))
	fake.inputCalls = 0
	response = NewWithRunnerOutputCap(nil, fake, 1024).Do(context.Background(), Request{Verb: "run-input", Args: map[string]any{"run_id": "RUN-1", "data": overCap}})
	if response.Code != "E_RUN_ARGUMENT_INVALID" || fake.inputCalls != 0 {
		t.Fatalf("over-cap response=%+v calls=%d", response, fake.inputCalls)
	}
}

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

func TestRunnerPTYForcesMergedRequestAndSingleLiveSink(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &faceRunner{}
	core := NewWithRunnerFace(nil, fake, nil, FaceOutput{Stdout: &stdout, Stderr: &stderr, Live: true})
	response := core.Do(context.Background(), Request{Verb: "run", Args: map[string]any{
		"argv": []string{"child"}, "merge": false, "pty": true,
	}})
	if !response.OK || !fake.request.PTY || !fake.request.Merge || fake.request.LiveStdout != &stdout || fake.request.LiveStderr != nil {
		t.Fatalf("PTY face response=%+v request=%+v", response, fake.request)
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
