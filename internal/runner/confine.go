package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultConfineSlice = "aira.slice"
	// A project-less invocation has no per-project peak-RSS history. Four GiB is
	// a conservative #50 no-history fallback; injected callers may override it.
	DefaultConfineMemoryReserve = int64(4 << 30)
	// Under --delegate-ram the suite delegates RAM accounting to its per-test
	// reservations, so its OWN reserve must be a small PINNED framework overhead —
	// never the unpinned whole-command estimate, which would double-book the
	// per-test reservations in queue.outstanding and could inflate via history to
	// reject the whole suite E_ADMIT_TOO_LARGE.
	DefaultDelegateRAMOverhead = int64(512 << 20)
	// DefaultDelegateRAMScopeCeiling is the compiled-in containment cap used
	// whenever a daemon ceiling is unavailable. It is not an admission charge.
	DefaultDelegateRAMScopeCeiling = int64(48 << 30)
)

type ConfineAdmission string

const (
	ConfineAdmissionAdmitted    ConfineAdmission = "admitted"
	ConfineAdmissionTimeout     ConfineAdmission = "timeout"
	ConfineAdmissionUnevaluated ConfineAdmission = "unevaluated"
)

type ConfineCap string

const (
	ConfineCapEnforced    ConfineCap = "enforced"
	ConfineCapUnevaluated ConfineCap = "unevaluated"
)

type ConfineScope string

const (
	ConfineScopePlaced     ConfineScope = "placed"
	ConfineScopeUnverified ConfineScope = "unverified"
)

type ConfineOOMGroup string

const (
	ConfineOOMGroupSet        ConfineOOMGroup = "set"
	ConfineOOMGroupUnverified ConfineOOMGroup = "unverified"
)

type ConfinePriorities string

const (
	ConfinePrioritiesApplied    ConfinePriorities = "applied"
	ConfinePrioritiesUnverified ConfinePriorities = "unverified"
)

// ConfineCPUWeight reports whether the foreground supervisor established the
// optional CPU-weight aging control. It is deliberately independent of the
// fail-closed memory confinement facets.
type ConfineCPUWeight string

const (
	ConfineCPUWeightAging       ConfineCPUWeight = "aging"
	ConfineCPUWeightUnavailable ConfineCPUWeight = "unavailable"
)

// ConfineStatus keeps the independently verified cap, admission, placement,
// OOM-group, and priority facets separate. In particular, a successful
// admission never fabricates a cap snapshot or implies priority success.
type ConfineStatus struct {
	Slice                string
	Cap                  ConfineCap
	Admission            ConfineAdmission
	AdmissionState       string
	AdmissionWaitedMS    int64
	CapBytes             int64
	ReserveBytes         int64
	Scope                ConfineScope
	ScopeIntegrity       ScopeIntegrity
	DescendantEscape     *DescendantEscapeEvidence
	OOMGroup             ConfineOOMGroup
	Priorities           ConfinePriorities
	CPUWeight            ConfineCPUWeight
	ScopeMemoryMax       int64
	ScopeMemoryHigh      int64
	ScopeMemoryBinding   string
	ScopeMemoryEffective int64
	ReserveBasis         string
	PeakRSS              *int64
}

type ConfineRequest struct {
	Slice               string
	Name                string
	Owner               string
	ScopeID             string
	Argv                []string
	Env                 []string
	RuntimeDir          string
	AdmitSocketPath     string
	MemoryReserve       int64
	MemoryReservePinned bool
	DelegateRAM         bool
	ScopeMemoryMax      int64
	ScopeMemoryHigh     int64
	AdmissionMaxWait    time.Duration
	PollInterval        time.Duration
	HandshakeTimeout    time.Duration
	Stdin               io.Reader
	Stdout              io.Writer
	Stderr              io.Writer
	SelfPath            string
	ResourceSignature   string
}

const ConfineUnknownOwner = "unknown"

// ValidateConfineIdentity applies the deliberately small scope-id component
// alphabet to cooperative owner identities. It prevents control characters or
// path-shaped values from crossing the CLI/admission boundary; it is not an
// authentication mechanism.
func ValidateConfineIdentity(value string) error {
	if value == "" {
		return errors.New("identity is empty")
	}
	if len(value) > 100 {
		return errors.New("identity is too long")
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r) {
			continue
		}
		return errors.New("identity requires letters, digits, '.', '_', or '-'")
	}
	return nil
}

type ConfineResult struct {
	Exit   int
	Status ConfineStatus
}

// ResolveConfineSlice applies only explicit flag > environment precedence.
// Linux resolves the machine default from live, injected state; portable code
// must not probe cgroups or silently select a compatibility slice.
func ResolveConfineSlice(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if value := os.Getenv("AIRA_CONFINE_SLICE"); value != "" {
		return value
	}
	return ""
}

// Confine runs a foreground command in a newly-created cgroup scope. Platform
// implementations must fail closed when the scope cannot be established.
func Confine(ctx context.Context, request ConfineRequest) (ConfineResult, error) {
	return confine(ctx, request)
}

func FormatConfineBytes(value int64) string {
	if value <= 0 {
		return "unknown"
	}
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if value%unit.bytes == 0 {
			return strconv.FormatInt(value/unit.bytes, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(value, 10)
}

// FormatConfineStatus is the single operator-facing honesty projection.
func FormatConfineStatus(status ConfineStatus) string {
	capFacet := status.Cap
	if capFacet == "" {
		capFacet = ConfineCapUnevaluated
	}
	slice := status.Slice
	if slice == "" {
		slice = "unevaluated"
	}
	line := "confine: slice=" + slice + " cap=" + string(capFacet)
	if status.CapBytes > 0 {
		line += "(" + FormatConfineBytes(status.CapBytes) + ")"
	}
	if status.ReserveBytes > 0 {
		line += " reserve=" + FormatConfineBytes(status.ReserveBytes)
		if status.ReserveBasis != "" {
			line += " reserve-basis=" + status.ReserveBasis
		}
	}
	admissionFacet := status.AdmissionState
	if admissionFacet == "" {
		admissionFacet = string(status.Admission)
	}
	if admissionFacet != "" {
		line += " admission=" + admissionFacet
	}
	line += " scope=" + string(status.Scope)
	if status.ScopeIntegrity != "" {
		line += " scope-integrity=" + string(status.ScopeIntegrity)
	}
	if status.DescendantEscape != nil {
		line += " escaped-pid=" + strconv.Itoa(status.DescendantEscape.PIDIdentity.PID)
		line += " escaped-cgroup=" + status.DescendantEscape.Cgroup
	}
	line += " oom.group=" + string(status.OOMGroup)
	line += " priorities=" + string(status.Priorities)
	cpuWeight := status.CPUWeight
	if cpuWeight == "" {
		cpuWeight = ConfineCPUWeightUnavailable
	}
	line += " cpu-weight=" + string(cpuWeight)
	if status.ScopeMemoryMax <= 0 {
		line += " scope-memory.max=not-requested"
	} else {
		line += " scope-memory.max=enforced=" + strconv.FormatInt(status.ScopeMemoryMax, 10)
		if status.ScopeMemoryBinding != "" {
			line += " binding=" + status.ScopeMemoryBinding
		}
		if status.ScopeMemoryEffective > 0 {
			line += " effective=" + strconv.FormatInt(status.ScopeMemoryEffective, 10)
		}
		if status.ScopeMemoryHigh > 0 {
			line += " memory.high=reclaim-pressure=" + strconv.FormatInt(status.ScopeMemoryHigh, 10)
		}
	}
	return line
}
