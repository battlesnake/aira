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
func (*faceRunner) Kill(context.Context, string) (*runner.RunRecord, error) { return nil, nil }
func (*faceRunner) Get(string) (*runner.RunRecord, error)                   { return nil, nil }
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
