package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AIRA-121. The install MODE is the one durable fact that decides whether this
// machine has a real, kernel-enforced cgroup slice or the container shim.
//
// It is recorded ONCE, by `aira install`, in a single file beside state.db, and
// every reader — the confine client, `aira run`, the daemon itself, and the
// aitest bootstrap verb — resolves it from that same record. There is
// deliberately no second source: a client that believed it was in shim mode
// while the daemon believed otherwise would launch an UNCONTAINED job while the
// ledger booked a real cgroup scope for it, which is the exact class of silent
// divergence this record exists to prevent.
const (
	// ConfineModeReal is the ordinary, kernel-enforced path: a per-job cgroup
	// scope under aira.slice, with cgroup.kill as the backstop.
	ConfineModeReal = "real-slice"
	// ConfineModeShim is the container shim (AIRA-121): no systemd, no
	// delegated cgroup subtree, no per-job scope, and therefore NO kill
	// backstop. Admission is an advisory in-daemon RAM ledger and nothing more.
	ConfineModeShim = "ci-shim"
)

// ShimConfineSlice is the sentinel slice NAME and PATH used end to end in shim
// mode. It is not a filesystem path and must never be treated as one: the
// daemon's shim slice resolver returns it for any requested slice, and every
// cgroup read keyed on it is re-sourced (internal/daemon/shim.go). Naming it
// something that cannot be a cgroup path is deliberate — a stray real cgroup
// read against it fails loudly rather than resolving somewhere plausible.
const ShimConfineSlice = "ci-shim"

// InstallModeFileEnv overrides where the install-mode record is read from. It
// is a TEST/override seam for the location only, never for the mode: what the
// record SAYS is still the single source of truth. Production leaves it unset.
const InstallModeFileEnv = "AIRA_INSTALL_MODE_FILE"

// InstallModeRecord is the durable install-mode record. It is written by
// `aira install --stage=build` and read (never written) by everything else.
type InstallModeRecord struct {
	Schema      int               `json:"schema"`
	Mode        string            `json:"mode"`
	AiraVersion string            `json:"aira_version,omitempty"`
	RecordedAt  string            `json:"recorded_at,omitempty"`
	Home        string            `json:"home,omitempty"`
	UID         int               `json:"uid"`
	ResolvedBy  string            `json:"resolved_by,omitempty"`
	Capability  map[string]string `json:"capability,omitempty"`
	// ShimBudgetBytes is the ledger ceiling the shim daemon admits against.
	// Zero in real mode, and never zero in a valid shim record: a shim install
	// that cannot establish a budget FAILS rather than installing an ungated
	// shim (internal/install).
	ShimBudgetBytes int64 `json:"shim_budget_bytes,omitempty"`
	// ShimBudgetSource is one of ShimBudgetSource*. It is printed wherever the
	// budget is, because "declared" and "the whole machine" are very different
	// claims and a bare byte count cannot tell them apart.
	ShimBudgetSource string `json:"shim_budget_source,omitempty"`
	ShimCgroupPath   string `json:"shim_cgroup_path,omitempty"`
}

// The closed set of shim budget provenances.
const (
	// ShimBudgetSourceDeclared: an operator typed `--memory-max` at install.
	// AIRA-121 gate condition C8: on the real path the slice MemoryMax IS the
	// admission ceiling the daemon reads, so --memory-max meaning "the ledger
	// ceiling" in shim mode is the SAME meaning, not a second one. A declared
	// budget is required in practice because the meminfo fallback over-books
	// whenever a node runs more than one container without a per-container
	// memory.max (GCP Batch taskCountPerNode > 1 is exactly this).
	ShimBudgetSourceDeclared = "declared"
	// ShimBudgetSourceCgroupMemoryMax: the container's OWN cgroup memory.max,
	// i.e. the number the container runtime will OOM-kill against.
	ShimBudgetSourceCgroupMemoryMax = "cgroup-memory-max"
	// ShimBudgetSourceMemTotal: /proc/meminfo MemTotal. The distinctly WEAKER
	// source, and recorded as such: /proc/meminfo is not namespaced on the
	// runtimes in question, so inside a container it reports the HOST's memory.
	ShimBudgetSourceMemTotal = "meminfo-memtotal"
)

// ValidShimBudgetSource reports whether value is a catalogued provenance.
func ValidShimBudgetSource(value string) bool {
	switch value {
	case ShimBudgetSourceDeclared, ShimBudgetSourceCgroupMemoryMax, ShimBudgetSourceMemTotal:
		return true
	}
	return false
}

// DescribeShimBudgetSource renders a provenance for an operator. An
// uncatalogued value renders as itself rather than being silently normalised.
func DescribeShimBudgetSource(source string) string {
	switch source {
	case ShimBudgetSourceDeclared:
		return "declared with --memory-max at install"
	case ShimBudgetSourceCgroupMemoryMax:
		return "the container's own cgroup memory.max"
	case ShimBudgetSourceMemTotal:
		return "/proc/meminfo MemTotal (host-wide; weaker than a container memory.max)"
	case "":
		return "unevaluated"
	}
	return source
}

// InstallModePathFor names the record inside an XDG state home. AIRA-121 gate
// condition C11: it lives in the AIRA state DIRECTORY, beside state.db, not at
// the root of the state home — Paths.StateHome is the XDG state home and is
// shared with every other application on the box.
func InstallModePathFor(stateHome string) string {
	return filepath.Join(stateHome, "aira", "install-mode.json")
}

// InstallModeRecordPath resolves the record's location the same way the daemon
// resolves its state home, without importing internal/daemon (which imports
// this package).
func InstallModeRecordPath() string {
	if override := strings.TrimSpace(os.Getenv(InstallModeFileEnv)); override != "" {
		return override
	}
	stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateHome == "" {
		home := strings.TrimSpace(os.Getenv("HOME"))
		if home == "" {
			var err error
			if home, err = os.UserHomeDir(); err != nil {
				return ""
			}
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return InstallModePathFor(stateHome)
}

// ReadInstallModeRecord reads and validates one record. It reports ok=false for
// absence, an unreadable or malformed file, and an unrecognised mode — every one
// of which degrades to the STRICTER (real) path at the one call site that
// decides, so an unreadable record can never turn a real install into an
// unconfined one.
func ReadInstallModeRecord(path string) (InstallModeRecord, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return InstallModeRecord{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return InstallModeRecord{}, false
	}
	var record InstallModeRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return InstallModeRecord{}, false
	}
	if record.Schema != 1 {
		return InstallModeRecord{}, false
	}
	switch record.Mode {
	case ConfineModeReal, ConfineModeShim:
	default:
		return InstallModeRecord{}, false
	}
	if record.Mode == ConfineModeShim {
		if record.ShimBudgetBytes <= 0 || !ValidShimBudgetSource(record.ShimBudgetSource) {
			// A shim record without a usable budget describes an ungated shim.
			// Refusing to recognise it is the fail-closed direction: the client
			// falls back to the real path, which refuses to launch without a
			// finite cap rather than launching unconfined.
			return InstallModeRecord{}, false
		}
	}
	return record, true
}

// WriteInstallModeRecord writes a record atomically (temp + rename in the same
// directory), 0600. Exported because internal/install owns the write and this
// package owns the schema.
func WriteInstallModeRecord(path string, record InstallModeRecord) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("install-mode record path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".install-mode-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

var (
	confineModeOnce   sync.Once
	confineModeCached string
)

// ResolveConfineMode returns this process's confinement mode, read once and
// cached. Absent, unreadable, malformed, or unrecognised all resolve to
// ConfineModeReal, so every already-installed box is untouched and an
// unreadable record degrades to the stricter path.
func ResolveConfineMode() string {
	confineModeOnce.Do(func() {
		confineModeCached = resolveConfineModeUncached()
	})
	return confineModeCached
}

func resolveConfineModeUncached() string {
	record, ok := ReadInstallModeRecord(InstallModeRecordPath())
	if !ok {
		return ConfineModeReal
	}
	if record.Mode == ConfineModeShim {
		return ConfineModeShim
	}
	return ConfineModeReal
}

// resetConfineModeCache re-reads the record. Test-only: the production process
// resolves its mode once, because the record is written at image-build time and
// cannot change under a running job.
func resetConfineModeCache() {
	confineModeOnce = sync.Once{}
	confineModeCached = ""
}
