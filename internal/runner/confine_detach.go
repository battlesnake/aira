package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// CodeConfineDetachFailed is returned when a detached launch could not be
	// established. It NEVER covers "the job failed": a detached job's own outcome
	// lives in its durable record.
	CodeConfineDetachFailed = "E_CONFINE_DETACH_FAILED"
	// CodeConfineDetachCancelled is the supervisor's verdict when the launcher
	// never acknowledged the handle. The job is deliberately not run: a job
	// nobody was told about would burn a shared reserve invisibly.
	CodeConfineDetachCancelled = "U_CONFINE_DETACH_CANCELLED"
	// CodeConfineOutcomeUnknown is `--status`'s verdict when the record is not
	// terminal and its supervisor is gone or unevaluable.
	CodeConfineOutcomeUnknown = "U_CONFINE_OUTCOME_UNKNOWN"

	confineDetachRecordName    = "record.json"
	confineDetachTempName      = "record.json.tmp"
	confineDetachStdoutName    = "stdout"
	confineDetachStderrName    = "stderr"
	confineDetachSupervisorLog = "supervisor.log"
)

// ConfineDetachLaunch is what a successful detached launch hands back. It
// deliberately carries no exit code: at this point the job has not run.
type ConfineDetachLaunch struct {
	ScopeID           string
	Slice             string
	CapBytes          int64
	SupervisorPID     int
	RecordDir         string
	RecordPath        string
	StdoutPath        string
	StderrPath        string
	SupervisorLogPath string

	// Acknowledge tells the supervisor whether the handle reached the user, and
	// is the load-bearing half of the handshake: a supervisor that is NOT
	// acknowledged abandons the launch before admission, so a launcher that
	// reported a detach failure can never leave an unreported job consuming the
	// shared slice.
	//
	// A successful LaunchConfineDetached ALWAYS sets it. Callers must treat a nil
	// Acknowledge as a launch failure rather than as "nothing to do": leaving it
	// unwritten makes the supervisor cancel, so reporting success would be a
	// fabricated one.
	Acknowledge func(delivered bool) error
}

// ConfineDetachSchema versions the durable record written by a detached confine
// supervisor. A reader that meets a schema it does not know reports
// outcome-unknown rather than guessing at the fields it recognises.
const ConfineDetachSchema = 1

// The observed lifecycle phases a detached supervisor writes into its record.
// Each is written from a point where the fact is already established, never
// inferred: `admitting` from the launch gate (every synchronous precondition has
// passed), `running` from the proven-placement point. Nothing writes `running`
// on the strength of the supervisor merely being alive.
const (
	ConfineDetachPhaseStarting  = "starting"
	ConfineDetachPhaseAdmitting = "admitting"
	ConfineDetachPhaseRunning   = "running"
)

// ConfineDetachRecord is the durable, session-independent answer to "what
// happened to my confined job" (AIRA-22). It lives beside the job's captured
// output under the XDG state home, is rewritten atomically, and is the only
// artifact that survives both the launching session and the supervisor.
type ConfineDetachRecord struct {
	Schema      int      `json:"schema"`
	ScopeID     string   `json:"scope_id"`
	Name        string   `json:"name"`
	Owner       string   `json:"owner"`
	Slice       string   `json:"slice,omitempty"`
	CapBytes    int64    `json:"cap_bytes,omitempty"`
	Argv        []string `json:"argv,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	DelegateRAM bool     `json:"delegate_ram,omitempty"`
	// EnvDigest is a digest, never the environment itself. A confine job's
	// environment routinely carries credentials and this record is a durable
	// file whose whole purpose is to be long-lived.
	EnvDigest   string      `json:"env_digest,omitempty"`
	Supervisor  PIDIdentity `json:"supervisor"`
	Phase       string      `json:"phase"`
	StartedAt   string      `json:"started_at"`
	AdmittingAt string      `json:"admitting_at,omitempty"`
	RunningAt   string      `json:"running_at,omitempty"`
	EndedAt     string      `json:"ended_at,omitempty"`
	// Terminal is the single bit meaning "the supervisor wrote an outcome". Its
	// absence is never read as success: a supervisor that was SIGKILLed and a
	// supervisor that finished are distinguished by exactly this field plus the
	// supervisor's own liveness, and nothing else.
	Terminal bool `json:"terminal"`
	// Exit is absent whenever Confine returned an error, including errors raised
	// after the child had already started. A zero would be a fabricated success
	// and a one would be a fabricated failure.
	Exit      *int           `json:"exit,omitempty"`
	ErrorCode string         `json:"error_code,omitempty"`
	Error     string         `json:"error,omitempty"`
	Status    *ConfineStatus `json:"status,omitempty"`

	StdoutPath        string `json:"stdout_path,omitempty"`
	StderrPath        string `json:"stderr_path,omitempty"`
	SupervisorLogPath string `json:"supervisor_log_path,omitempty"`

	// ReadError is set by the STORE, never by a supervisor, when a record
	// directory exists but its record.json could not be read or decoded. It is
	// the difference between "I cannot tell you" and "there is no such job", and
	// conflating those is exactly the fabrication this record exists to prevent.
	ReadError string `json:"read_error,omitempty"`
}

// ConfineDetachState is what `aira confine --status` reports. Four of the five
// values are observations; the fifth is an admission of ignorance.
type ConfineDetachState string

const (
	// ConfineDetachStarting: the supervisor is alive but has not yet reached the
	// launch gate, so not even the slice has been resolved.
	ConfineDetachStarting ConfineDetachState = "starting"
	// ConfineDetachAdmitting: every synchronous precondition passed and the job
	// is in (or waiting on) admission. Deliberately NOT "running": admission on a
	// contended slice legitimately queues for a long time, and a queued job is
	// not running by any definition an operator would accept.
	ConfineDetachAdmitting ConfineDetachState = "admitting"
	// ConfineDetachRunning: the child was started and proven to be a member of
	// the scope.
	ConfineDetachRunning ConfineDetachState = "running"
	// ConfineDetachFinished: the supervisor wrote a terminal record. The record's
	// own Exit/ErrorCode say how it ended; `finished` never implies success.
	ConfineDetachFinished ConfineDetachState = "finished"
	// ConfineDetachOutcomeUnknown: a non-terminal record whose supervisor is gone
	// or cannot be evaluated, or a record that cannot be read. The one thing this
	// verb must never do is imply an outcome it does not have.
	ConfineDetachOutcomeUnknown ConfineDetachState = "outcome-unknown"
)

// ConfineDetachStatus pairs the resolved state with the record it was resolved
// from, so a caller always sees the evidence alongside the verdict.
type ConfineDetachStatus struct {
	State  ConfineDetachState  `json:"state"`
	Reason string              `json:"reason,omitempty"`
	Record ConfineDetachRecord `json:"record"`
}

// ConfineSupervisorProbe answers "is this exact process still alive". The second
// result is whether the question could be answered at all: an unreadable /proc
// entry is unevaluated, and unevaluated resolves to outcome-unknown, never to
// running.
type ConfineSupervisorProbe func(PIDIdentity) (alive bool, evaluated bool)

// classifyConfineDetachRecord is the whole honesty model in one function.
//
// covers: AIRA-22
func classifyConfineDetachRecord(record ConfineDetachRecord, probe ConfineSupervisorProbe) ConfineDetachStatus {
	status := ConfineDetachStatus{Record: record}
	if record.ReadError != "" {
		status.State = ConfineDetachOutcomeUnknown
		status.Reason = "the durable record could not be read: " + record.ReadError
		return status
	}
	if record.Schema != ConfineDetachSchema {
		status.State = ConfineDetachOutcomeUnknown
		status.Reason = "the durable record uses schema " + strconv.Itoa(record.Schema) +
			", which this binary cannot interpret (expected " + strconv.Itoa(ConfineDetachSchema) + ")"
		return status
	}
	// Terminal first, and unconditionally: a supervisor that wrote an outcome and
	// then exited is finished, and asking whether it is still alive would turn
	// every completed job into outcome-unknown.
	if record.Terminal {
		status.State = ConfineDetachFinished
		return status
	}
	if probe == nil {
		status.State = ConfineDetachOutcomeUnknown
		status.Reason = "supervisor liveness was not evaluated"
		return status
	}
	alive, evaluated := probe(record.Supervisor)
	if !evaluated {
		status.State = ConfineDetachOutcomeUnknown
		status.Reason = fmt.Sprintf("the record is not terminal and supervisor pid %d could not be evaluated, so the job's outcome is unevaluated", record.Supervisor.PID)
		return status
	}
	if !alive {
		status.State = ConfineDetachOutcomeUnknown
		status.Reason = fmt.Sprintf("the detached supervisor (pid %d) is gone and wrote no terminal record, so the job's outcome is unevaluated; see the captured output and supervisor log", record.Supervisor.PID)
		return status
	}
	switch record.Phase {
	case ConfineDetachPhaseRunning:
		status.State = ConfineDetachRunning
	case ConfineDetachPhaseAdmitting:
		status.State = ConfineDetachAdmitting
		status.Reason = "the job has passed every launch precondition and is in admission; it is not running yet"
	default:
		status.State = ConfineDetachStarting
		status.Reason = "the supervisor is alive but has not reached the launch gate"
	}
	return status
}

// confineDetachOwnerMatches scopes name/pid selectors to the caller's own jobs.
// A SCOPE ID is exempt: it is globally unique and was named explicitly, so
// refusing to look it up because it belongs to another owner would be obstruction
// rather than safety. This verb is read-only; nothing here is an ownership guard
// (that is `--kill`'s job), it exists to keep `--status <name>` unambiguous when
// several sessions use the same names.
func confineDetachOwnerMatches(record ConfineDetachRecord, callerOwner string) bool {
	return strings.TrimSpace(record.Owner) == strings.TrimSpace(callerOwner)
}

func confineDetachSelectorMatches(record ConfineDetachRecord, selector string) (byScopeID bool, matched bool) {
	if selector == record.ScopeID {
		return true, true
	}
	if selector == record.Name {
		return false, true
	}
	if record.Supervisor.PID > 0 && selector == strconv.Itoa(record.Supervisor.PID) {
		return false, true
	}
	return false, false
}

// ListConfineDetachStatuses reports the caller's own records, newest first.
//
// covers: AIRA-22
func ListConfineDetachStatuses(records []ConfineDetachRecord, callerOwner string, probe ConfineSupervisorProbe) []ConfineDetachStatus {
	out := make([]ConfineDetachStatus, 0, len(records))
	for _, record := range records {
		if !confineDetachOwnerMatches(record, callerOwner) {
			continue
		}
		out = append(out, classifyConfineDetachRecord(record, probe))
	}
	sortConfineDetachStatuses(out)
	return out
}

func sortConfineDetachStatuses(statuses []ConfineDetachStatus) {
	sort.SliceStable(statuses, func(i, j int) bool {
		if statuses[i].Record.StartedAt != statuses[j].Record.StartedAt {
			return statuses[i].Record.StartedAt > statuses[j].Record.StartedAt
		}
		return statuses[i].Record.ScopeID > statuses[j].Record.ScopeID
	})
}

// ResolveConfineDetachStatus resolves ONE selector against the store.
//
// Ambiguity is refused rather than guessed at (CLAUDE.md), but the refusal is
// made actionable: it lists every candidate newest-first with its start time and
// resolved state, so the operator is one copy-paste from the answer. A selector
// that matches nothing under the caller's own owner, but does match under
// another, says so instead of pretending the job never existed.
//
// covers: AIRA-22
func ResolveConfineDetachStatus(records []ConfineDetachRecord, selector, callerOwner string, probe ConfineSupervisorProbe) (ConfineDetachStatus, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return ConfineDetachStatus{}, fmt.Errorf("%s: a confine status selector is required", CodeConfineNotFound)
	}
	var matches []ConfineDetachRecord
	foreign := 0
	for _, record := range records {
		byScopeID, matched := confineDetachSelectorMatches(record, selector)
		if !matched {
			continue
		}
		if !byScopeID && !confineDetachOwnerMatches(record, callerOwner) {
			foreign++
			continue
		}
		matches = append(matches, record)
	}
	if len(matches) == 0 {
		if foreign > 0 {
			return ConfineDetachStatus{}, fmt.Errorf(
				"%s: selector %q matched no detached confine record for owner %q (%d matched other owners; name the scope id, or pass --owner)",
				CodeConfineNotFound, selector, callerOwner, foreign)
		}
		return ConfineDetachStatus{}, fmt.Errorf("%s: selector %q matched no detached confine record", CodeConfineNotFound, selector)
	}
	statuses := make([]ConfineDetachStatus, 0, len(matches))
	for _, record := range matches {
		statuses = append(statuses, classifyConfineDetachRecord(record, probe))
	}
	if len(statuses) > 1 {
		sortConfineDetachStatuses(statuses)
		described := make([]string, 0, len(statuses))
		for _, status := range statuses {
			described = append(described, fmt.Sprintf("%s (started %s, %s)", status.Record.ScopeID, status.Record.StartedAt, status.State))
		}
		return ConfineDetachStatus{}, fmt.Errorf("E_SELECTOR_AMBIGUOUS: selector %q matched %d detached confine records, newest first: %s",
			selector, len(statuses), strings.Join(described, "; "))
	}
	return statuses[0], nil
}

// FormatConfineDetachStatus is the single human projection. The captured-output
// paths are printed in EVERY state, including outcome-unknown: partial output is
// real evidence even when the outcome is not.
func FormatConfineDetachStatus(status ConfineDetachStatus) string {
	record := status.Record
	lines := []string{}
	head := "confine: scope=" + record.ScopeID + " name=" + record.Name + " owner=" + record.Owner + " state=" + string(status.State)
	if record.Slice != "" {
		head += " slice=" + record.Slice
	}
	if record.Supervisor.PID > 0 {
		head += " supervisor-pid=" + strconv.Itoa(record.Supervisor.PID)
	}
	if record.StartedAt != "" {
		head += " started=" + record.StartedAt
	}
	if record.EndedAt != "" {
		head += " ended=" + record.EndedAt
	}
	if status.State == ConfineDetachFinished {
		if record.Exit != nil {
			head += " exit=" + strconv.Itoa(*record.Exit)
		} else {
			head += " exit=unevaluated"
		}
		if record.ErrorCode != "" {
			head += " error-code=" + record.ErrorCode
		}
	}
	lines = append(lines, head)
	if status.Reason != "" {
		lines = append(lines, "confine: "+status.Reason)
	}
	if record.Error != "" {
		lines = append(lines, "confine: error: "+record.Error)
	}
	if record.Status != nil {
		lines = append(lines, FormatConfineStatus(*record.Status))
	}
	for _, ref := range []struct{ label, path string }{
		{"stdout", record.StdoutPath}, {"stderr", record.StderrPath}, {"supervisor-log", record.SupervisorLogPath},
	} {
		if ref.path != "" {
			lines = append(lines, "confine:   "+ref.label+"="+ref.path)
		}
	}
	return strings.Join(lines, "\n")
}
