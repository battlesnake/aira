package runner

import (
	"strings"
	"testing"
)

// TestConfineLogStreamResolutionNamesWhatItRead pins the --stream contract.
//
// The default matters more than it looks: a confine job captures out and err as
// SEPARATE files, so `aira confine-log <job>` reading stdout while the failure
// went to stderr is the obvious false-pass. The defence is that every chunk
// names the stream it read, and that `err` is one flag away -- not that the verb
// refuses to answer without being told.
//
// verifies: AIRA-196
func TestConfineLogStreamResolutionNamesWhatItRead(t *testing.T) {
	record := ConfineDetachRecord{
		ScopeID:    "CONFINE-gate-1-a@session-a",
		StdoutPath: "/state/CONFINE/stdout", StderrPath: "/state/CONFINE/stderr",
	}
	for _, test := range []struct {
		stream     string
		wantPath   string
		wantStream string
	}{
		{stream: "", wantPath: "/state/CONFINE/stdout", wantStream: "out"},
		{stream: "out", wantPath: "/state/CONFINE/stdout", wantStream: "out"},
		{stream: "stdout", wantPath: "/state/CONFINE/stdout", wantStream: "out"},
		{stream: "  ERR ", wantPath: "/state/CONFINE/stderr", wantStream: "err"},
		{stream: "stderr", wantPath: "/state/CONFINE/stderr", wantStream: "err"},
	} {
		path, stream, err := confineLogStreamPath(record, test.stream)
		if err != nil {
			t.Fatalf("--stream %q: %v", test.stream, err)
		}
		if path != test.wantPath || stream != test.wantStream {
			t.Fatalf("--stream %q resolved to %s/%s, want %s/%s", test.stream, stream, path, test.wantStream, test.wantPath)
		}
	}
	// `merged` is run-log's, and a confine job HAS no merged capture. Silently
	// answering with stdout would hand back half the output under a name that
	// promises all of it.
	if _, _, err := confineLogStreamPath(record, "merged"); err == nil {
		t.Fatal("--stream merged was accepted; a detached confine job has no merged capture to return")
	} else if !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
		t.Fatalf("unknown stream reported %v, want E_CONFINE_ARGUMENT_INVALID", err)
	}
	// A record naming no capture path is unevaluated, never an empty read.
	if _, _, err := confineLogStreamPath(ConfineDetachRecord{ScopeID: "x"}, "out"); err == nil ||
		!strings.HasPrefix(err.Error(), CodeConfineLogUnavailable) {
		t.Fatalf("a record with no stdout path reported %v, want %s", err, CodeConfineLogUnavailable)
	}
}

// TestConfineLogFollowAndCompleteAreDifferentQuestions is the honesty test for
// the two predicates. Collapsing them is the tempting simplification and it is
// wrong in both directions:
//
//   - if `complete` followed `follow`, an outcome-unknown job (supervisor gone,
//     no outcome written) would report its truncated capture as the whole thing;
//   - if `follow` followed `complete`, a --follow against a dead supervisor would
//     never return, which is exactly the situation an operator reaches for it in.
//
// verifies: AIRA-196
func TestConfineLogFollowAndCompleteAreDifferentQuestions(t *testing.T) {
	for _, test := range []struct {
		state        ConfineDetachState
		wantDone     bool
		wantComplete bool
	}{
		{state: ConfineDetachStarting, wantDone: false, wantComplete: false},
		{state: ConfineDetachAdmitting, wantDone: false, wantComplete: false},
		{state: ConfineDetachRunning, wantDone: false, wantComplete: false},
		{state: ConfineDetachFinished, wantDone: true, wantComplete: true},
		{state: ConfineDetachOutcomeUnknown, wantDone: true, wantComplete: false},
	} {
		if got := confineLogFollowDone(test.state); got != test.wantDone {
			t.Fatalf("%s: follow-done=%v, want %v", test.state, got, test.wantDone)
		}
		if got := confineLogCaptureComplete(test.state, false); got != test.wantComplete {
			t.Fatalf("%s: complete=%v, want %v", test.state, got, test.wantComplete)
		}
		// A truncated window is never complete whatever the job did.
		if confineLogCaptureComplete(test.state, true) {
			t.Fatalf("%s: a truncated window was reported complete", test.state)
		}
	}
}

// TestConfineLogChunkCarriesTheJobsOwnVerdict pins that the bytes never travel
// alone. Output read from a job whose supervisor vanished is real evidence, but
// partial evidence, and a caller must not have to issue a second verb to find
// that out.
//
// verifies: AIRA-196
func TestConfineLogChunkCarriesTheJobsOwnVerdict(t *testing.T) {
	exit := 7
	status := ConfineDetachStatus{
		State:  ConfineDetachFinished,
		Reason: "",
		Record: ConfineDetachRecord{
			ScopeID: "CONFINE-gate-1-a@session-a", Name: "gate", Owner: "session-a",
			Exit: &exit, ErrorCode: "E_CONFINE_UNAVAILABLE",
		},
	}
	window := &capturedReadResult{Offset: 2, NextOffset: 9, TotalBytes: 9, Bytes: []byte("matched"), Filtered: true}
	chunk := confineLogChunk(status, "err", "/state/stderr", ConfineLogRequest{Grep: "match"}, window)
	if chunk.ScopeID != status.Record.ScopeID || chunk.Name != "gate" || chunk.Owner != "session-a" {
		t.Fatalf("identity lost: %+v", chunk)
	}
	if chunk.Stream != "err" || chunk.Path != "/state/stderr" || chunk.Encoding != "base64" {
		t.Fatalf("stream metadata lost: %+v", chunk)
	}
	if chunk.Offset != 2 || chunk.NextOffset != 9 || chunk.TotalBytes != 9 {
		t.Fatalf("cursor lost: %+v", chunk)
	}
	if !chunk.Filtered || chunk.Grep != "match" {
		t.Fatalf("the filter was applied but not reported: %+v", chunk)
	}
	if chunk.State != ConfineDetachFinished || chunk.ErrorCode != "E_CONFINE_UNAVAILABLE" {
		t.Fatalf("the job's verdict did not travel with its bytes: %+v", chunk)
	}
	if chunk.Exit == nil || *chunk.Exit != 7 {
		t.Fatalf("exit=%v, want 7", chunk.Exit)
	}
	// The exit is COPIED: a caller mutating the chunk must not reach into the
	// record it was built from.
	*chunk.Exit = 0
	if *status.Record.Exit != 7 {
		t.Fatal("the chunk aliases the record's exit code")
	}
	if !chunk.Complete {
		t.Fatal("a finished job's fully-read window was not reported complete")
	}
	// The same window from a job whose supervisor vanished must NOT be complete.
	unknown := ConfineDetachStatus{State: ConfineDetachOutcomeUnknown, Record: status.Record}
	if confineLogChunk(unknown, "out", "/state/stdout", ConfineLogRequest{}, window).Complete {
		t.Fatal("an outcome-unknown job's capture was reported complete; nothing establishes it is all of it")
	}
}
