package core

import (
	"context"
	"strings"
	"testing"

	"aira/internal/gitcontext"
)

func TestRantCorePreservesBodyAndCallerOnlyGitEvidence(t *testing.T) {
	s, root := coreTestStoreWithRoot(t)
	c := New(s)
	body := "  routed-looking bytes\nwith trailing space  "
	observed := gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: "main", Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: "caller-not-daemon-validated", Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "caller/ref", Status: gitcontext.StatusValue},
		RemoteURL:    gitcontext.Field{Value: "caller-remote-verbatim", Status: gitcontext.StatusValue},
		ObservedAt:   "2026-08-18T12:00:00Z", ResolverVersion: "test-v1",
	}
	response := c.Do(context.Background(), Request{Verb: "rant", Args: map[string]any{"subverb": "capture", "text": body, "tags": []string{"plumbing"}}, GitContext: &observed, Actor: "terra", Session: "session-1", Model: "codex-terra"})
	if !response.OK {
		t.Fatalf("capture: %#v", response)
	}
	got, err := s.GetRant("RANT-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != body || got.GitContext != observed || got.Actor != "terra" || got.Session != "session-1" || got.Model != "codex-terra" {
		t.Fatalf("stored rant changed: %#v", got)
	}
}

func TestRantCoreStoresScopeMismatchButDoesNotValidateCallerOnlyEvidence(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	observed := gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: "/wrong/repo", Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: "/wrong/worktree", Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: "wrong", Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: strings.Repeat("not-a-hash", 3), Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "not a validated ref", Status: gitcontext.StatusValue},
		RemoteURL:    gitcontext.Field{Value: "verbatim remote evidence", Status: gitcontext.StatusValue},
	}
	response := c.Do(context.Background(), Request{Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "mismatch"}, GitContext: &observed})
	if !response.OK {
		t.Fatalf("capture: %#v", response)
	}
	got, err := s.GetRant("RANT-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.GitContext.RepoRoot.Status != gitcontext.StatusMismatch || got.GitContext.WorktreePath.Status != gitcontext.StatusMismatch || got.GitContext.WorktreeID.Status != gitcontext.StatusMismatch {
		t.Fatalf("stable fields were not marked mismatch: %#v", got.GitContext)
	}
	if got.GitContext.HeadHash != observed.HeadHash || got.GitContext.HeadRef != observed.HeadRef || got.GitContext.RemoteURL != observed.RemoteURL {
		t.Fatalf("caller-only evidence was changed: %#v", got.GitContext)
	}
}

func TestOnlyRantCaptureRequiresGitContext(t *testing.T) {
	for operation, want := range map[string]bool{"capture": true, "": true, "ls": false, "get": false, "review": false, "redact": false} {
		if got := RequiresGitContext(Request{Verb: "rant", Args: map[string]any{"subverb": operation}}); got != want {
			t.Fatalf("operation %q requires context=%v, want %v", operation, got, want)
		}
	}
}
