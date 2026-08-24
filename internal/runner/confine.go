package runner

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultConfineSlice = "whale.slice"
	// A project-less invocation has no per-project peak-RSS history. Four GiB is
	// a conservative #50 no-history fallback; injected callers may override it.
	DefaultConfineMemoryReserve = int64(4 << 30)
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
	ScopeMemoryMax       int64
	ScopeMemoryHigh      int64
	ScopeMemoryBinding   string
	ScopeMemoryEffective int64
}

type ConfineRequest struct {
	Slice            string
	Name             string
	Argv             []string
	Env              []string
	RuntimeDir       string
	AdmitSocketPath  string
	MemoryReserve    int64
	ScopeMemoryMax   int64
	ScopeMemoryHigh  int64
	AdmissionMaxWait time.Duration
	PollInterval     time.Duration
	HandshakeTimeout time.Duration
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	SelfPath         string
}

type ConfineResult struct {
	Exit   int
	Status ConfineStatus
}

// ResolveConfineSlice applies flag > environment > machine default precedence.
func ResolveConfineSlice(flagValue string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("AIRA_CONFINE_SLICE")); value != "" {
		return value
	}
	return DefaultConfineSlice
}

// Confine runs a foreground command in a newly-created cgroup scope. Platform
// implementations must fail closed when the scope cannot be established.
func Confine(ctx context.Context, request ConfineRequest) (ConfineResult, error) {
	return confine(ctx, request)
}

func formatConfineBytes(value int64) string {
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
	line := "confine: slice=" + status.Slice + " cap=" + string(capFacet)
	if status.CapBytes > 0 {
		line += "(" + formatConfineBytes(status.CapBytes) + ")"
	}
	if status.ReserveBytes > 0 {
		line += " reserve=" + formatConfineBytes(status.ReserveBytes)
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
