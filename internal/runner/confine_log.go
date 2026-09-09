package runner

import (
	"fmt"
	"strings"
	"time"
)

// CodeConfineLogUnavailable is `confine-log`'s "I cannot tell you". It is never
// used for a job that produced no output: an empty capture file is an EMPTY
// SUCCESSFUL READ, and conflating the two would let "the log could not be
// opened" read as "the job printed nothing".
const CodeConfineLogUnavailable = "U_CONFINE_LOG_UNAVAILABLE"

// confineLogFollowInterval is the poll period of `--follow`. A var so a test can
// shorten it; it is not a tunable.
var confineLogFollowInterval = 200 * time.Millisecond

// ConfineLogRequest is one bounded read of a detached confine job's captured
// output, addressed by the job's own handle (AIRA-196). It carries no path: the
// whole point of the verb is that a caller never has to know or construct one.
type ConfineLogRequest struct {
	// Selector is `confine --status`'s selector, unchanged: a name, a supervisor
	// pid, or a scope id.
	Selector string
	Owner    string
	// Stream is "out" or "err". Confine captures the two SEPARATELY (there is no
	// merged file to offer), and an empty value means "out".
	Stream string
	From   int64
	Tail   int64
	// Full waives the caller's own MaxBytes observation cap for this read. See
	// ReadConfineLog for why it means that here and why run-log's identically
	// named field is left alone.
	Full   bool
	Follow bool
	Grep   string
	// MaxBytes is the face's observation cap; zero means unbounded.
	MaxBytes int64
}

// ConfineLogChunk is byte-oriented for the same reason OutputChunk is:
// encoding/json base64-encodes Bytes, so arbitrary output is safe over MCP.
//
// It always carries the JOB's resolved state alongside the bytes. Output read
// from a job whose supervisor vanished is real evidence, but it is partial
// evidence, and a reader must not have to ask a second verb to find that out.
type ConfineLogChunk struct {
	ScopeID    string `json:"scope_id"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Stream     string `json:"stream"`
	Path       string `json:"path"`
	Encoding   string `json:"encoding"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	TotalBytes int64  `json:"total_bytes"`
	Bytes      []byte `json:"bytes"`
	// Complete means the JOB IS FINISHED and this window reached the end of its
	// capture. An outcome-unknown job is deliberately never complete: its
	// supervisor died without recording an outcome, so nothing establishes that
	// what was captured is all there was.
	Complete  bool `json:"complete"`
	Truncated bool `json:"truncated"`
	// Filtered says Bytes is a SUBSET of the window [Offset,NextOffset) selected
	// by Grep. The cursor keeps describing the underlying file so --from paging
	// still works.
	Filtered  bool               `json:"filtered,omitempty"`
	Grep      string             `json:"grep,omitempty"`
	State     ConfineDetachState `json:"state"`
	Reason    string             `json:"reason,omitempty"`
	Exit      *int               `json:"exit,omitempty"`
	ErrorCode string             `json:"error_code,omitempty"`
}

// confineLogStreamPath resolves --stream against a durable record.
//
// An empty --stream is "out" rather than a refusal. run-log refuses an ambiguous
// stream because a run MAY have a merged capture and the choice is genuinely
// undetermined; a confine job always has exactly these two, so refusing every
// bare `aira confine-log <job>` would be obstruction. The chunk names the stream
// it read in every response, so the default is stated, never assumed.
func confineLogStreamPath(record ConfineDetachRecord, stream string) (string, string, error) {
	switch strings.ToLower(strings.TrimSpace(stream)) {
	case "", "out", "stdout":
		if record.StdoutPath == "" {
			return "", "out", fmt.Errorf("%s: the durable record for %s names no captured stdout path", CodeConfineLogUnavailable, record.ScopeID)
		}
		return record.StdoutPath, "out", nil
	case "err", "stderr":
		if record.StderrPath == "" {
			return "", "err", fmt.Errorf("%s: the durable record for %s names no captured stderr path", CodeConfineLogUnavailable, record.ScopeID)
		}
		return record.StderrPath, "err", nil
	default:
		return "", "", fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: unknown stream %q; a detached confine job captures `out` and `err` as separate files and has no merged stream", stream)
	}
}

// confineLogFollowDone and confineLogCaptureComplete answer two DIFFERENT
// questions and must not be collapsed into one predicate.
//
// Follow asks "can more bytes still arrive?". A supervisor that is gone will
// never write again, so outcome-unknown ends a --follow -- otherwise the verb
// would spin forever against a dead job, which is precisely the situation an
// operator reaches for it in.
//
// Complete asks "do I know this is all of it?". Outcome-unknown means the
// supervisor died without recording an outcome, so nothing establishes that the
// capture is whole. Answering `complete` there would be the fabrication AIRA-22
// built the whole state vocabulary to prevent.
func confineLogFollowDone(state ConfineDetachState) bool {
	return state == ConfineDetachFinished || state == ConfineDetachOutcomeUnknown
}

func confineLogCaptureComplete(state ConfineDetachState, truncated bool) bool {
	return state == ConfineDetachFinished && !truncated
}

// confineLogChunk assembles the response from a resolved status and a window
// that has already been read.
func confineLogChunk(status ConfineDetachStatus, stream, path string, request ConfineLogRequest, window *capturedReadResult) *ConfineLogChunk {
	record := status.Record
	chunk := &ConfineLogChunk{
		ScopeID: record.ScopeID, Name: record.Name, Owner: record.Owner,
		Stream: stream, Path: path, Encoding: "base64",
		Offset: window.Offset, NextOffset: window.NextOffset, TotalBytes: window.TotalBytes,
		Bytes: window.Bytes, Truncated: window.Truncated,
		Filtered: window.Filtered, Grep: request.Grep,
		State: status.State, Reason: status.Reason,
		ErrorCode: record.ErrorCode,
	}
	chunk.Complete = confineLogCaptureComplete(status.State, window.Truncated)
	if record.Exit != nil {
		exit := *record.Exit
		chunk.Exit = &exit
	}
	return chunk
}
