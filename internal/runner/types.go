// Package runner implements the machine-local runner-lite mechanism. Faces
// (CLI and MCP) deliberately do not live here.
package runner

import (
	"context"
	"io"
	"time"
)

type Status string

const (
	StatusStarting  Status = "starting"
	StatusRunning   Status = "running"
	StatusExited    Status = "exited"
	StatusKilled    Status = "killed"
	StatusLost      Status = "lost"
	StatusOOMKilled Status = "oom-killed"
)

func (s Status) Terminal() bool {
	return s == StatusExited || s == StatusKilled || s == StatusLost || s == StatusOOMKilled
}

type ScopeIntegrity string

const (
	ScopeContained ScopeIntegrity = "contained"
	// ScopeUnverified means clone placement succeeded, but the leader was
	// never positively observed in cgroup.procs before it exited. It is not
	// evidence of migration, and it is deliberately not positive containment.
	ScopeUnverified        ScopeIntegrity = "unverified"
	ScopeHandoffUnverified ScopeIntegrity = "handoff-unverified"
	ScopeMigrated          ScopeIntegrity = "migrated"
	ScopeDescendantKilled  ScopeIntegrity = "descendant-killed"
)

type OutputState string

const (
	OutputComplete OutputState = "complete"
	OutputPartial  OutputState = "partial"
	OutputEvicted  OutputState = "evicted"
	OutputUnavail  OutputState = "unavailable"
)

type OutputRef struct {
	Path   string      `json:"path"`
	Bytes  int64       `json:"bytes"`
	Digest string      `json:"digest,omitempty"`
	State  OutputState `json:"state"`
}

type ScopeKill struct {
	Requested bool   `json:"requested"`
	Started   bool   `json:"started"`
	Completed bool   `json:"completed"`
	GraceMS   int64  `json:"grace_ms,omitempty"`
	Actor     string `json:"actor,omitempty"`
	At        string `json:"at,omitempty"`
}

type KillIntent struct {
	Present   bool   `json:"present"`
	Sequence  uint64 `json:"sequence,omitempty"`
	Completed bool   `json:"completed"`
	Empty     bool   `json:"empty_scope"`
}

type PIDIdentity struct {
	PID       int    `json:"pid,omitempty"`
	StartTick uint64 `json:"start_tick,omitempty"`
}

// RunRecord is the durable protocol object. It intentionally contains no
// environment values, telemetry, or inferred green field.
type RunRecord struct {
	SchemaVersion       int                  `json:"schema_version"`
	ID                  string               `json:"id"`
	Argv                []string             `json:"argv"`
	Cwd                 string               `json:"cwd"`
	EnvDigest           string               `json:"env_digest"`
	Buffering           string               `json:"buffering"`
	Admission           string               `json:"admission"`
	AdmissionReason     string               `json:"admission_reason,omitempty"`
	AdmissionWaitedMS   int64                `json:"admission_waited_ms"`
	LaunchPrefix        []string             `json:"launch_prefix,omitempty"`
	CgroupScope         string               `json:"cgroup_scope,omitempty"`
	StartedAt           string               `json:"started_at"`
	EndedAt             string               `json:"ended_at,omitempty"`
	Status              Status               `json:"status"`
	ScopeIntegrity      ScopeIntegrity       `json:"scope_integrity"`
	ExitCode            *int                 `json:"exit_code,omitempty"`
	Signal              string               `json:"signal,omitempty"`
	OutputRefs          map[string]OutputRef `json:"output_refs,omitempty"`
	CaptureComplete     bool                 `json:"capture_complete"`
	CaptureForcedClosed bool                 `json:"capture_forced_closed"`
	StdinStored         bool                 `json:"stdin_stored"`
	ScopeKill           ScopeKill            `json:"scope_kill"`
	KillIntent          KillIntent           `json:"kill_intent"`
	ErrorCodes          []string             `json:"error_codes,omitempty"`
	PeakRSS             *int64               `json:"peak_rss,omitempty"`
	CPUUser             *int64               `json:"cpu_user,omitempty"`
	CPUSys              *int64               `json:"cpu_sys,omitempty"`
	PIDIdentity         PIDIdentity          `json:"pid_identity,omitempty"`
	TerminalComplete    bool                 `json:"terminal_complete"`
}

func (r RunRecord) CleanSuccess() bool {
	return r.Status == StatusExited && r.ExitCode != nil && *r.ExitCode == 0 &&
		r.ScopeIntegrity == ScopeContained && r.CaptureComplete && r.TerminalComplete && len(r.ErrorCodes) == 0
}

type EnvEntry struct {
	Key   []byte
	Value []byte
}

type Request struct {
	Argv        []string
	Cwd         string
	Env         []string // exact KEY=VALUE overrides; inherited environment is retained
	Timeout     time.Duration
	ExplicitEnv bool // when true, Env is the complete child environment
	Prefix      []string
	Merge       bool
	Realtime    bool
	StdinPath   string // empty means null stdin; "-" means the caller's stdin
	Stdin       io.Reader
	StoreStdin  bool
	Grace       time.Duration
	TermGrace   time.Duration
	LiveStdout  io.Writer // optional best-effort foreground tee sink
	LiveStderr  io.Writer // optional best-effort foreground tee sink
	NoAdmit     bool      // bypass the configured memory-admission gate
}

// Clock is the admission loop's time seam. After must deliver after d in the
// clock's own timeline; Launch always selects it together with ctx.Done().
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// OutputRequest describes one bounded, cursor-based read from a captured
// stream. MaxBytes is an observation cap imposed by a caller (the MCP face
// uses it to keep JSON responses bounded); zero means unbounded.
type OutputRequest struct {
	RunID    string
	Stream   string
	From     int64
	Tail     int64
	Full     bool
	Follow   bool
	MaxBytes int64
}

// OutputChunk is deliberately byte-oriented. encoding/json base64-encodes
// Bytes, so the same object is safe for arbitrary output over MCP.
type OutputChunk struct {
	RunID       string      `json:"run_id"`
	Stream      string      `json:"stream"`
	Encoding    string      `json:"encoding"`
	Offset      int64       `json:"offset"`
	NextOffset  int64       `json:"next_offset"`
	TotalBytes  int64       `json:"total_bytes"`
	Bytes       []byte      `json:"bytes"`
	Complete    bool        `json:"complete"`
	Truncated   bool        `json:"truncated"`
	OutputState OutputState `json:"output_state"`
	RunStatus   Status      `json:"run_status"`
	PeakRSS     *int64      `json:"peak_rss,omitempty"`
	CPUUser     *int64      `json:"cpu_user,omitempty"`
	CPUSys      *int64      `json:"cpu_sys,omitempty"`
	ErrorCodes  []string    `json:"error_codes,omitempty"`
}

type Config struct {
	CommonDir        string
	OutputDir        string
	CgroupParent     string
	Prefix           []string
	Grace            time.Duration
	TermGrace        time.Duration
	Now              func() time.Time
	Backend          ScopeBackend // nil selects the Linux cgroup-v2 backend
	MemorySlice      string
	MemoryReserve    int64
	AdmissionMaxWait time.Duration
	PollInterval     time.Duration
	Clock            Clock
	sliceMemoryFn    func(path string) (cur, max int64, ok bool, reason string)
	Diagnostics      io.Writer
}

type ScopeBackend interface {
	Probe(context.Context) error
	Create(context.Context, string) (Scope, error)
	Open(context.Context, string) (Scope, error)
}

type Scope interface {
	Reference() string
	FD() int
	Members() ([]int, error)
	Empty() (bool, error)
	Terminate([]int) error
	Kill() error
	Remove() error
}

type killResult struct {
	Started   bool
	Completed bool
	Empty     bool
}

type LaunchError struct {
	Code string
	Err  error
}

func (e *LaunchError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *LaunchError) Unwrap() error { return e.Err }

// EffectiveArgv is the no-shell argv topology used by Launch. It is also a
// small adapter/test seam for proving option tokens survive unchanged.
func EffectiveArgv(prefix, target []string) ([]string, error) {
	if len(target) == 0 || target[0] == "" {
		return nil, &LaunchError{"E_RUN_ARGUMENT_INVALID", context.Canceled}
	}
	cleanPrefix, err := validatePrefix(prefix)
	if err != nil {
		return nil, &LaunchError{"E_RUN_PREFIX_INVALID", err}
	}
	return append(append([]string(nil), cleanPrefix...), target...), nil
}
