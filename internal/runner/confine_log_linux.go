//go:build linux

package runner

import (
	"context"
	"fmt"
	"time"
)

// ReadConfineLog answers `aira confine-log <selector>` against the real durable
// record store.
//
// It performs NO daemon round trip, deliberately and for AIRA-22's original
// reason: this is the read half of a survivability verb, and routing it through
// the component most likely to have been restarted during exactly the long pause
// the verb exists to survive would defeat it. `confine --status` reads the same
// store the same way; this reads one more file beside it.
//
// covers: AIRA-196
func ReadConfineLog(ctx context.Context, root string, request ConfineLogRequest) (*ConfineLogChunk, error) {
	grep, grepErr := compileCapturedGrep(request.Grep)
	if grepErr != nil {
		return nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --grep: %w", grepErr)
	}
	status, err := ConfineDetachStatusFor(root, request.Selector, request.Owner)
	if err != nil {
		return nil, err
	}
	// --follow waits for the job, then reads once, exactly as run-log does. The
	// MaxBytes gate is run-log's too: a capped (MCP) reader is answered
	// immediately rather than parked for the life of the job.
	if request.Follow && request.MaxBytes == 0 {
		status, err = followConfineLog(ctx, root, request, status)
		if err != nil {
			return nil, err
		}
	}
	path, stream, err := confineLogStreamPath(status.Record, request.Stream)
	if err != nil {
		return nil, err
	}
	// --full waives the FACE's observation cap for this one read, which is the
	// only reading under which the flag is true rather than decorative: on the
	// CLI there is no cap, so an unbounded read is already the default, and over
	// MCP the cap is exactly what --full asks to set aside. The caller opted in
	// explicitly and the response is one chunk, so nothing here is unbounded by
	// accident.
	//
	// Deliberately AFTER the follow gate above, which still reads
	// request.MaxBytes: `--follow --full` over a capped face must be answered
	// immediately rather than parking the request for the life of the job.
	//
	// run-log's identical --full is currently inert and AIRA-196 does NOT change
	// it: internal callers (core/run_wiring.go, store/gate_command.go) pass
	// Full: true ALONGSIDE a real cap they depend on, so giving Full this meaning
	// there would silently remove the run-report size bound. That gap is recorded,
	// not fixed here.
	maxBytes := request.MaxBytes
	if request.Full {
		maxBytes = 0
	}
	window, fault, readErr := readCapturedFile(capturedReadRequest{
		Path: path, From: request.From, Tail: request.Tail, MaxBytes: maxBytes, Grep: grep,
	})
	switch fault {
	case capturedFaultArgument:
		return nil, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %w", readErr)
	case capturedFaultUnavailable:
		// Named, never silently empty: "the capture could not be read" and "the
		// job printed nothing" are different facts and only one of them is good
		// news.
		return nil, fmt.Errorf("%s: %s (%s): %w", CodeConfineLogUnavailable, status.Record.ScopeID, path, readErr)
	}
	return confineLogChunk(status, stream, path, request, window), nil
}

// followConfineLog re-resolves the job until no more bytes can arrive. It
// re-reads the DURABLE RECORD each tick rather than trusting the first
// resolution, so a supervisor that dies mid-follow is noticed and the wait ends
// with outcome-unknown instead of never.
func followConfineLog(ctx context.Context, root string, request ConfineLogRequest, status ConfineDetachStatus) (ConfineDetachStatus, error) {
	for !confineLogFollowDone(status.State) {
		timer := time.NewTimer(confineLogFollowInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, ctx.Err()
		case <-timer.C:
		}
		next, err := ConfineDetachStatusFor(root, request.Selector, request.Owner)
		if err != nil {
			return status, err
		}
		status = next
	}
	return status, nil
}
