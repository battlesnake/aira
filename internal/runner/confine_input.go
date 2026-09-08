package runner

import (
	"errors"
	"fmt"
	"io"
	"strconv"
)

// confine_input.go carries `confine-input`'s local classification. The wire
// protocol, the server, and the client streaming loop are `run-input`'s,
// reused unchanged (AIRA-196).
//
// The E_RUN_INPUT_* code vocabulary is reused too, and that is a decision rather
// than an oversight: the server a confine job runs IS runInputPlane, and it
// emits those codes on the wire. A parallel E_CONFINE_INPUT_* vocabulary would
// have to be translated at the socket boundary, which is two names for one fact
// plus a real mistranslation risk in the one place -- a partial, ambiguous
// delivery -- where being wrong is worst. The exit-code contract already maps
// every one of them.

// ConfineInputRequest injects bytes into a detached confine job's stdin, or
// closes it, addressed by the job's own handle.
type ConfineInputRequest struct {
	// Selector is `confine --status`'s selector: a name, a supervisor pid, or a
	// scope id.
	Selector string
	Owner    string
	Close    bool
	Steal    bool
	Reader   io.Reader
}

// ConfineInputResult reports what was ACCEPTED FOR DELIVERY -- bytes the
// supervisor acknowledged writing into the child's stdin pipe. It is never a
// claim that the child read them.
type ConfineInputResult struct {
	ScopeID  string `json:"scope_id"`
	Name     string `json:"name"`
	Accepted int64  `json:"accepted"`
	Closed   bool   `json:"closed"`
}

// confineInputPlaneID names a detached job's input socket file.
//
// Deliberately SHORT rather than the scope id: a Unix socket path is capped at
// 107 bytes (prepareRunInputPlane refuses beyond it) and a confine scope id
// carries a name, a pid, a nonce AND an owner, which would routinely blow that
// budget once a runtime directory is prefixed. The supervisor pid plus
// prepareRunInputPlane's own 96-bit nonce is unique per job, and nothing ever
// parses this name back: the durable record is what maps a job to its socket.
func confineInputPlaneID(supervisorPID int) string {
	return "confine-" + strconv.Itoa(supervisorPID)
}

// classifyConfineInputStatus decides whether a resolved job can be written to,
// mirroring classifyRunInputRecord clause for clause.
//
// The FIRST clause is the load-bearing one. A job launched without
// --stdin-connect has no conduit at all -- its stdin is /dev/null -- and saying
// so by name is what keeps the default-off design usable: an operator who
// forgot the flag gets a sentence naming it, not a dial against a socket that
// was never created, and never a wait.
func classifyConfineInputStatus(status ConfineDetachStatus) (string, error) {
	record := status.Record
	if !record.StdinConnect {
		return "", &RunInputError{Code: "E_RUN_INPUT_UNAVAILABLE", Err: fmt.Errorf(
			"the detached confine job %s was not launched with --stdin-connect, so its stdin is /dev/null and there is no conduit to write to",
			record.ScopeID)}
	}
	switch status.State {
	case ConfineDetachFinished:
		return "", &RunInputError{Code: "E_RUN_INPUT_CLOSED", Err: errors.New("the job has finished")}
	case ConfineDetachRunning:
		// The only state in which the child exists and the plane is serving.
	default:
		// Starting, admitting, and outcome-unknown are all "not yet, or not
		// evaluable" -- never "closed", which would tell an operator the job is
		// over when it may be hours deep in the admission queue.
		reason := status.Reason
		if reason == "" {
			reason = "the job's last recorded state is " + string(status.State)
		}
		return "", &RunInputError{Code: "E_RUN_INPUT_NOT_READY", Err: errors.New(reason)}
	}
	if record.InputSocket == "" {
		return "", &RunInputError{Code: "E_RUN_INPUT_NOT_READY", Err: errors.New(
			"the job is running but its durable record names no input socket yet")}
	}
	return record.InputSocket, nil
}
