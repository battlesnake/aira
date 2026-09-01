// Package runner implements the machine-local runner-lite mechanism. Faces
// (CLI and MCP) deliberately do not live here.
package runner

import (
	"context"
	"fmt"
	"io"
	"time"
)

type Status string

const (
	StatusStarting  Status = "starting"
	StatusRunning   Status = "running"
	StatusExited    Status = "exited"
	StatusKilled    Status = "killed"
	StatusCancelled Status = "cancelled"
	StatusLost      Status = "lost"
	StatusOOMKilled Status = "oom-killed"
)

func (s Status) Terminal() bool {
	return s == StatusExited || s == StatusKilled || s == StatusCancelled || s == StatusLost || s == StatusOOMKilled
}

type ScopeIntegrity string

const (
	ScopeContained ScopeIntegrity = "contained"
	// ScopeUnverified means placement was proven but the available observations
	// cannot attest descendant containment. It is not evidence of migration,
	// and it is deliberately not positive containment.
	ScopeUnverified        ScopeIntegrity = "unverified"
	ScopeHandoffUnverified ScopeIntegrity = "handoff-unverified"
	ScopeMigrated          ScopeIntegrity = "migrated"
	ScopeDescendantKilled  ScopeIntegrity = "descendant-killed"
	ScopeDescendantEscaped ScopeIntegrity = "descendant-escaped"
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
	BootID    string `json:"boot_id,omitempty"`
}

// DescendantEscapeEvidence records the exact live process identity and foreign
// cgroup that completed a witnessed escape proof. It describes an observation,
// not who moved the process or how it was moved.
type DescendantEscapeEvidence struct {
	PIDIdentity PIDIdentity `json:"pid_identity"`
	Cgroup      string      `json:"cgroup"`
}

// RunRecord is the durable protocol object. It intentionally contains no
// environment values or inferred green field. Telemetry is an opaque envelope:
// runner persists it but never assigns meaning to its values or references.
type RunRecord struct {
	SchemaVersion         int                       `json:"schema_version"`
	ID                    string                    `json:"id"`
	Owner                 string                    `json:"owner,omitempty"`
	StolenBy              string                    `json:"stolen_by,omitempty"`
	Ticket                string                    `json:"ticket,omitempty"`
	Phase                 string                    `json:"phase,omitempty"`
	Label                 string                    `json:"label,omitempty"`
	Tool                  string                    `json:"tool,omitempty"`
	Argv                  []string                  `json:"argv"`
	Cwd                   string                    `json:"cwd"`
	EnvDigest             string                    `json:"env_digest"`
	Buffering             string                    `json:"buffering"`
	Merge                 bool                      `json:"merge_streams"`
	Admission             string                    `json:"admission"`
	AdmissionReason       string                    `json:"admission_reason,omitempty"`
	AdmissionWaitedMS     int64                     `json:"admission_waited_ms"`
	ResourceSignature     string                    `json:"resource_signature,omitempty"`
	AdmissionReserve      *int64                    `json:"admission_reserve,omitempty"`
	AdmissionReserveBasis string                    `json:"admission_reserve_basis,omitempty"`
	ScopeMemoryMax        *int64                    `json:"scope_memory_max,omitempty"`
	ScopeMemoryHigh       *int64                    `json:"scope_memory_high,omitempty"`
	LaunchPrefix          []string                  `json:"launch_prefix,omitempty"`
	CgroupScope           string                    `json:"cgroup_scope,omitempty"`
	StartedAt             string                    `json:"started_at"`
	EndedAt               string                    `json:"ended_at,omitempty"`
	Status                Status                    `json:"status"`
	ScopeIntegrity        ScopeIntegrity            `json:"scope_integrity"`
	DescendantEscape      *DescendantEscapeEvidence `json:"descendant_escape,omitempty"`
	ExitCode              *int                      `json:"exit_code,omitempty"`
	Signal                string                    `json:"signal,omitempty"`
	OutputRefs            map[string]OutputRef      `json:"output_refs,omitempty"`
	CaptureComplete       bool                      `json:"capture_complete"`
	CaptureForcedClosed   bool                      `json:"capture_forced_closed"`
	StdinStored           bool                      `json:"stdin_stored"`
	ScopeKill             ScopeKill                 `json:"scope_kill"`
	KillIntent            KillIntent                `json:"kill_intent"`
	ErrorCodes            []string                  `json:"error_codes,omitempty"`
	PeakRSS               *int64                    `json:"peak_rss,omitempty"`
	CPUUser               *int64                    `json:"cpu_user,omitempty"`
	CPUSys                *int64                    `json:"cpu_sys,omitempty"`
	PIDIdentity           PIDIdentity               `json:"pid_identity,omitempty"`
	Detached              bool                      `json:"detached,omitempty"`
	StdinConnect          bool                      `json:"stdin_connect,omitempty"`
	InputSocket           string                    `json:"input_socket,omitempty"`
	SupervisorPID         PIDIdentity               `json:"supervisor_pid,omitempty"`
	LeaderExitObserved    bool                      `json:"leader_exit_observed,omitempty"`
	QuiesceForced         bool                      `json:"quiesce_forced,omitempty"`
	TerminalComplete      bool                      `json:"terminal_complete"`
	Telemetry             string                    `json:"telemetry,omitempty"`
	TelemetryRefs         []string                  `json:"telemetry_refs,omitempty"`
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
	Argv                  []string `json:"argv"`
	Ticket                string
	Phase                 string
	Label                 string
	Tool                  string
	Cwd                   string
	Env                   []string // exact KEY=VALUE overrides; inherited environment is retained
	Timeout               time.Duration
	ExplicitEnv           bool // when true, Env is the complete child environment
	Prefix                []string
	Merge                 bool
	Realtime              bool
	PTY                   bool
	StdinPath             string    // empty means null stdin; "-" means the caller's stdin
	Stdin                 io.Reader `json:"-"`
	StoreStdin            bool
	Grace                 time.Duration
	TermGrace             time.Duration
	LiveStdout            io.Writer `json:"-"` // optional best-effort foreground tee sink
	LiveStderr            io.Writer `json:"-"` // optional best-effort foreground tee sink
	NoAdmit               bool      // bypass the configured memory-admission gate
	Detach                bool      `json:"detach,omitempty"`
	StdinConnect          bool      `json:"stdin_connect,omitempty"`
	ResourceSignature     string    `json:"resource_signature,omitempty"`
	MemoryReserveOverride *int64    `json:"memory_reserve_override,omitempty"`
	MemoryReserveBasis    string    `json:"memory_reserve_basis,omitempty"`
	MemoryReservePinned   bool      `json:"memory_reserve_pinned,omitempty"`
	DaemonEstimateMemory  bool      `json:"daemon_estimate_memory,omitempty"`
	DelegateRAM           bool      `json:"delegate_ram,omitempty"`
	// DelegateRAMChargeExplicit distinguishes a user-supplied suite reserve/max
	// from the compatibility placeholder sent while the daemon resolves a
	// whole-suite estimate.
	DelegateRAMChargeExplicit bool   `json:"-"`
	ScopeMemoryMax            int64  `json:"scope_memory_max,omitempty"`
	ScopeMemoryHigh           int64  `json:"scope_memory_high,omitempty"`
	ConfineScopeID            string `json:"confine_scope_id,omitempty"`
	ConfineName               string `json:"confine_name,omitempty"`
	ConfineOwner              string `json:"confine_owner,omitempty"`
	// TelemetryPending is an opaque initial envelope supplied by Core. Runner
	// stamps it into the starting event without interpreting its value.
	TelemetryPending string `json:"telemetry_pending,omitempty"`
	detachReady      *detachSignal
	detachAck        io.ReadCloser
	detachRunID      string
}

// PeakRSSStats is the domain-free aggregate used by clients to estimate a
// command signature's admission reserve.
type PeakRSSStats struct {
	TotalCount  int
	SampleCount int
	PeakMax     int64
	OOMCount    int
	MaxOOMPeak  int64
}

type PeakRSSHistorian interface {
	PeakRSSHistory(context.Context, string) (PeakRSSStats, bool, error)
}

// SupervisorLiveness is a generic, boot-aware process observation. Consumers
// decide what that observation means for their own opaque auxiliary state.
type SupervisorLiveness string

const (
	SupervisorAlive   SupervisorLiveness = "alive"
	SupervisorDead    SupervisorLiveness = "dead"
	SupervisorUnknown SupervisorLiveness = "unknown"
)

type DetachLaunch struct {
	Record   RunRecord
	complete func(bool) error
}

func NewDetachLaunch(record RunRecord, complete func(bool) error) *DetachLaunch {
	return &DetachLaunch{Record: record, complete: complete}
}

func (d *DetachLaunch) Complete(delivered bool) error {
	if d == nil || d.complete == nil {
		return nil
	}
	return d.complete(delivered)
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
	CommonDir          string
	OutputDir          string
	Owner              string
	CgroupParent       string
	Prefix             []string
	Grace              time.Duration
	TermGrace          time.Duration
	Now                func() time.Time
	Backend            ScopeBackend // nil selects the Linux cgroup-v2 backend
	MemorySlice        string
	MemoryReserve      int64
	AdmissionMaxWait   time.Duration
	PollInterval       time.Duration
	Clock              Clock
	sliceMemoryFn      func(path string) (cur, max int64, ok bool, reason string)
	Diagnostics        io.Writer
	ReportMaxBytes     int64
	DetachReadyTimeout time.Duration
	SupervisorLeaseTTL time.Duration
	DaemonScope        map[string]any
	InputRuntimeDir    string
}

// RunInputRequest describes one serial input-plane connection. Reader is
// streamed in bounded DATA frames; Close explicitly closes the child's fd0.
type RunInputRequest struct {
	RunID  string
	Reader io.Reader
	Close  bool
	Steal  bool
}

// RunInputResult reports bytes accepted into the kernel pipe. Accepted does
// not mean that the child has processed those bytes.
type RunInputResult struct {
	RunID    string `json:"run_id"`
	Accepted int64  `json:"accepted"`
	Closed   bool   `json:"closed,omitempty"`
}

type RunInputError struct {
	Code      string
	Committed int64
	Err       error
}

func (e *RunInputError) Error() string {
	message := e.Code
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	if e.Committed > 0 || e.Code == "E_RUN_INPUT_CLOSED" || e.Code == "E_RUN_INPUT_PARTIAL" || e.Code == "E_RUN_INPUT_OUTCOME_UNKNOWN" {
		message += fmt.Sprintf(" (committed=%d)", e.Committed)
	}
	return message
}

func (e *RunInputError) Unwrap() error { return e.Err }

const (
	defaultSupervisorLeaseTTL = 120 * time.Second
	minimumSupervisorLeaseTTL = 60 * time.Second
)

// ValidSupervisorLeaseTTL enforces the approved ttl/3 cadence budget and the
// daemon's 30-second reap interval with deterministic ten-percent jitter.
func ValidSupervisorLeaseTTL(ttl time.Duration) bool {
	if ttl < minimumSupervisorLeaseTTL {
		return false
	}
	renew := ttl / 3
	jitter := renew / 10
	delayBudget := 3*5*time.Second + 1500*time.Millisecond + 10*time.Second + jitter
	return ttl-renew > delayBudget && ttl > 30*time.Second+jitter
}

const DefaultReportMaxBytes int64 = 32 << 20

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

type killPolicy struct {
	Enforce     bool
	Steal       bool
	CallerOwner string
}

type LaunchError struct {
	Code string
	Err  error
}

type ForeignOwnerError struct {
	RunID       string
	Owner       string
	CallerOwner string
}

func (e *ForeignOwnerError) Error() string {
	return fmt.Sprintf("run %s is owned by %q, not caller %q; pass --steal to override", e.RunID, e.Owner, e.CallerOwner)
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
