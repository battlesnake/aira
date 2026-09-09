//go:build linux

package runner

import (
	"context"
	"net"
)

// confineInputDialFn is the dial seam, nil in production. It exists so a test
// can drive the real handshake and streaming loop over a socket it controls.
var confineInputDialFn func(context.Context, string) (net.Conn, error)

// ConfineInput answers `aira confine-input <selector>` against the real durable
// record store.
//
// Like the read half it needs NO daemon. Unlike the read half it does need the
// job's SUPERVISOR to be alive: injecting stdin means writing into a pipe that
// process owns, so there is no durable-record-only form of it. That asymmetry is
// architectural, not a gap -- a durable record can preserve what a job PRINTED,
// but nothing can preserve a conduit into a process that has exited.
//
// covers: AIRA-196
func ConfineInput(ctx context.Context, root string, request ConfineInputRequest) (*ConfineInputResult, error) {
	status, err := ConfineDetachStatusFor(root, request.Selector, request.Owner)
	if err != nil {
		return nil, err
	}
	path, err := classifyConfineInputStatus(status)
	if err != nil {
		return nil, err
	}
	conn, err := dialRunInput(ctx, path, request.Owner, request.Steal, confineInputDialFn)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	accepted, closed, streamErr := streamRunInput(conn, request.Reader, request.Close)
	// The result is returned WITH the error: a partial delivery is a fact the
	// caller must report, and dropping it would leave an operator unable to tell
	// how much of their input landed.
	return &ConfineInputResult{
		ScopeID: status.Record.ScopeID, Name: status.Record.Name,
		Accepted: accepted, Closed: closed,
	}, streamErr
}
