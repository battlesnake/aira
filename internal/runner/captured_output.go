package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
)

// captured_output.go is the ONE definition of "a bounded, cursor-based window
// over a durable captured-output file", shared by `run-log` (a run record's
// OutputRefs) and `confine-log` (a detached confine job's stdout/stderr).
//
// It was factored out of Runner.ReadOutput by AIRA-196 rather than copied.
// Everything that is about a RUN -- the run id, choosing a stream from
// OutputRefs, output state, follow-until-terminal, the run's telemetry --
// stayed with ReadOutput; only the part that is genuinely about a plain file
// plus an offset lives here. Two copies of this arithmetic would have been free
// to disagree about the edges each verb inherits (tail-versus-offset
// precedence, what a cap does to the cursor, whether an offset at EOF is empty
// or an error), and a caller comparing the two verbs would have no way to know
// which was right.

// capturedReadRequest is a window over one captured file. It carries no job
// identity on purpose: whatever resolved Path -- a run record or a confine
// record -- is the caller's concern, and keeping it out is what makes this
// reader shareable at all.
type capturedReadRequest struct {
	Path string
	From int64
	// Tail applies only when From is zero. A caller that named an explicit
	// offset asked for that offset.
	Tail int64
	// MaxBytes is an observation cap imposed by a face (the MCP face uses it to
	// keep JSON responses bounded); zero means unbounded.
	MaxBytes int64
	// Grep, when non-nil, selects which LINES of the window come back. It never
	// changes the cursor -- see capturedReadResult.Filtered.
	Grep *regexp.Regexp
}

// capturedReadResult is byte-oriented and never decodes or normalises what it
// read. Offset/NextOffset/TotalBytes always describe the UNDERLYING FILE, even
// when Grep shortened Bytes.
type capturedReadResult struct {
	Offset     int64
	NextOffset int64
	TotalBytes int64
	Bytes      []byte
	Truncated  bool
	// Filtered says outright that Bytes is a subset of the window that was read,
	// so len(Bytes) != NextOffset-Offset is a stated fact rather than a
	// discrepancy a caller has to notice.
	Filtered bool
}

// capturedReadFault classifies WHY a read could not be served, so each verb maps
// it onto its own error vocabulary (E_RUN_ARGUMENT_INVALID /
// U_RUN_OUTPUT_UNAVAILABLE for run-log, E_CONFINE_ARGUMENT_INVALID /
// U_CONFINE_LOG_UNAVAILABLE for confine-log) instead of this file inventing a
// third one that neither verb's error contract knows about.
type capturedReadFault int

const (
	capturedFaultNone capturedReadFault = iota
	// capturedFaultArgument: the caller asked for a window that does not exist.
	capturedFaultArgument
	// capturedFaultUnavailable: the capture itself could not be read. It is
	// NEVER reported as an empty successful read -- "the job produced nothing"
	// and "I could not read what it produced" are different facts.
	capturedFaultUnavailable
)

func readCapturedFile(req capturedReadRequest) (*capturedReadResult, capturedReadFault, error) {
	if req.From < 0 || req.Tail < 0 || req.MaxBytes < 0 {
		return nil, capturedFaultArgument, errors.New("output offsets and limits must be non-negative")
	}
	if req.Path == "" {
		return nil, capturedFaultUnavailable, errors.New("captured output is unavailable")
	}
	file, err := os.Open(req.Path)
	if err != nil {
		return nil, capturedFaultUnavailable, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, capturedFaultUnavailable, err
	}
	total := info.Size()
	start := req.From
	if req.Tail > 0 && req.From == 0 {
		start = total - req.Tail
		if start < 0 {
			start = 0
		}
	}
	if start > total {
		return nil, capturedFaultArgument, fmt.Errorf("output offset %d exceeds total %d", start, total)
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, capturedFaultUnavailable, err
	}
	result := &capturedReadResult{Offset: start, TotalBytes: total}
	limit := total - start
	if req.MaxBytes > 0 && limit > req.MaxBytes {
		limit = req.MaxBytes
		result.Truncated = true
	}
	data := make([]byte, limit)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, capturedFaultUnavailable, err
	}
	result.NextOffset = start + int64(len(data))
	result.Truncated = result.Truncated || result.NextOffset < total
	result.Bytes = data
	if req.Grep != nil {
		result.Bytes = filterCapturedLines(data, req.Grep)
		result.Filtered = true
	}
	return result, capturedFaultNone, nil
}

// compileCapturedGrep turns a --grep option into a filter. An empty pattern is
// no filter at all (nil), never a pattern that matches everything, so the
// unfiltered path stays byte-identical and Filtered stays false.
//
// An unparseable pattern is an ERROR the caller reports, never a filter that
// silently matches nothing: a filter that quietly dropped every line would read
// exactly like "the job produced no matching output", which is the false-pass
// this whole verb family exists to avoid.
func compileCapturedGrep(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	return regexp.Compile(pattern)
}

// filterCapturedLines keeps the lines of one window that match pattern,
// terminators included, so the returned bytes remain a valid fragment of the
// captured stream rather than a re-rendered list.
//
// The match is made against the line WITHOUT its terminator (and without a
// preceding CR), so an anchored pattern such as `error$` behaves the way it
// does in grep(1) rather than never matching. A final segment with no
// terminator is still tested and, if it matches, returned as it is -- which is
// both what grep(1) does and what a still-running job's half-written last line
// actually is.
func filterCapturedLines(data []byte, pattern *regexp.Regexp) []byte {
	kept := make([]byte, 0, len(data))
	for len(data) > 0 {
		line := data
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			line, data = data[:index+1], data[index+1:]
		} else {
			data = nil
		}
		if pattern.Match(bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))) {
			kept = append(kept, line...)
		}
	}
	return kept
}
