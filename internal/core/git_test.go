package core

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"aira/internal/gitremote"
)

type fakeGitOps struct {
	request gitremote.Request
	result  *gitremote.Result
	err     error
}

func (f *fakeGitOps) Run(_ context.Context, request gitremote.Request) (*gitremote.Result, error) {
	f.request = request
	return f.result, f.err
}

func TestGitDispatchBuildsClosedRequestAndSuccessPayload(t *testing.T) {
	fake := &fakeGitOps{result: &gitremote.Result{Op: "push", Auth: "ssh", Remote: "upstream", URL: "git@github.com:o/r.git", Host: "github.com", ExitCode: 0}}
	dispatcher := New(nil).WithGitOps(fake)
	response := dispatcher.Do(context.Background(), Request{Verb: "git", Args: map[string]any{
		"subverb": "push", "remote": "upstream", "refspecs": []string{"HEAD:main"},
	}})
	if !response.OK || response.Code != "OK" || response.Data != fake.result {
		t.Fatalf("response=%+v", response)
	}
	if fake.request.Verb != "push" || fake.request.Remote != "upstream" || len(fake.request.Refspecs) != 1 || fake.request.Refspecs[0] != "HEAD:main" {
		t.Fatalf("request=%+v", fake.request)
	}
}

func TestGitDispatchUnknownAndNilSeamHaveStableCodes(t *testing.T) {
	response := New(nil).Do(context.Background(), Request{Verb: "git", Args: map[string]any{"subverb": "pull"}})
	if response.OK || response.Code != gitremote.CodeArgInvalid || response.Exit != 2 {
		t.Fatalf("unknown response=%+v", response)
	}
	response = New(nil).Do(context.Background(), Request{Verb: "git", Args: map[string]any{"subverb": "fetch"}})
	if response.OK || response.Code != gitremote.CodeSSHUnavailable || response.Exit != 1 {
		t.Fatalf("nil response=%+v", response)
	}
}

func TestGitDispatchMapsStructuredSentinelData(t *testing.T) {
	fake := &fakeGitOps{err: &gitremote.Error{StableCode: gitremote.CodeFallbackBlocked, Message: "blocked", Details: map[string]any{"reason": "insteadof-rewrite"}}}
	response := New(nil).WithGitOps(fake).Do(context.Background(), Request{Verb: "git", Args: map[string]any{"subverb": "fetch"}})
	data, ok := response.Data.(map[string]any)
	if response.OK || response.Code != gitremote.CodeFallbackBlocked || response.Exit != 1 || !ok || data["reason"] != "insteadof-rewrite" {
		t.Fatalf("response=%+v", response)
	}
}

func TestGitFaceWiresLiveSinksAndJSONStyleSuppressesThem(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &fakeGitOps{result: &gitremote.Result{Op: "fetch", Auth: "ssh", ExitCode: 0}}
	dispatcher := NewWithRunnerFace(nil, nil, nil, FaceOutput{Stdout: &stdout, Stderr: &stderr, Live: true}).WithGitOps(fake)
	if response := dispatcher.Do(context.Background(), Request{Verb: "git", Args: map[string]any{"subverb": "fetch"}}); !response.OK {
		t.Fatal(response)
	}
	if fake.request.LiveStdout != &stdout || fake.request.LiveStderr != &stderr {
		t.Fatalf("live request=%+v", fake.request)
	}

	fake.request = gitremote.Request{}
	dispatcher = NewWithRunnerFace(nil, nil, nil, FaceOutput{Stdout: &stdout, Stderr: &stderr}).WithGitOps(fake)
	if response := dispatcher.Do(context.Background(), Request{Verb: "git", Args: map[string]any{"subverb": "fetch"}}); !response.OK {
		t.Fatal(response)
	}
	if fake.request.LiveStdout != nil || fake.request.LiveStderr != nil {
		t.Fatalf("suppressed request=%+v", fake.request)
	}
}

func TestGitDispatchDoesNotFlattenUnknownErrors(t *testing.T) {
	fake := &fakeGitOps{err: errors.New("E_INTERNAL: boom")}
	response := New(nil).WithGitOps(fake).Do(context.Background(), Request{Verb: "git", Args: map[string]any{"subverb": "fetch"}})
	if response.OK || response.Code != "E_INTERNAL" {
		t.Fatalf("response=%+v", response)
	}
}
