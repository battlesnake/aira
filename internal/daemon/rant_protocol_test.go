package daemon

import (
	"bytes"
	"reflect"
	"testing"

	"aira/internal/core"
	"aira/internal/gitcontext"
)

func TestRantRequestFramePreservesBodyAndTypedGitContext(t *testing.T) {
	context := gitcontext.GitContext{HeadHash: gitcontext.Field{Value: "caller bytes", Status: gitcontext.StatusValue}, ObservedAt: "2026-08-18T12:00:00Z", ResolverVersion: "v1"}
	want := RequestFrame{Proto: ProtocolVersion, Scope: WorktreeScope{ProjectID: "p", WorktreeID: "w"}, Request: core.Request{Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "  byte identity\n  "}, GitContext: &context}}
	var wire bytes.Buffer
	if err := writeFrame(&wire, want); err != nil {
		t.Fatal(err)
	}
	var got RequestFrame
	if err := readFrame(&wire, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}
