//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// desiredCPUSlots is the worker-admit CPU gate's concurrency capacity (AIRA-64).
// It was shared with the daemon scheduler's active-set capacity until AIRA-33
// deleted that scheduler; the gate is now its sole consumer. Reserve one CPU for
// interactive work by default, but never reduce capacity below one.
func desiredCPUSlots(cpuCount int) (int, error) {
	reserve := 1
	if raw, present := os.LookupEnv("AIRA_DAEMON_CPU_RESERVE"); present && raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_CPU_RESERVE must be a non-negative integer")
		}
		reserve = value
	}
	count := cpuCount - reserve
	if count < 1 {
		count = 1
	}
	return count, nil
}

// confineScopeChildPrefix is the directory-name prefix every `aira confine`
// scope carries under its slice. It is the same membership test
// runner.listConfines uses (confine_manage_linux.go), spelled here because this
// package may not import that scan.
const confineScopeChildPrefix = ".aira-CONFINE-"

// cpuSlotsPlacementGraceDefault mirrors the aitest supervisor's own
// _PLACEMENT_ACK_TIMEOUT_SECONDS (supervisor.py). The two must not drift: the
// grace is the window in which a granted-but-not-yet-placed worker scope still
// counts as a live worker, and the client kills its own unplaced child at
// exactly this deadline. See AIRA-64 plan section 4.4.1.
const cpuSlotsPlacementGraceDefault = 60 * time.Second

// cpuSlotsPlacementGrace reads the same variable NAME the supervisor reads
// (AIRA_AITEST_PLACEMENT_ACK_TIMEOUT, seconds) — but out of the DAEMON's own
// environment, which is a different process from the pytest run.
//
// Stated plainly rather than implied: setting this for a pytest invocation does
// NOT move the daemon's grace. The two default to the same 60s and each is
// independently overridable where it runs. A mismatch is bounded in both
// directions and neither direction breaks an invariant:
//
//   - client grace LONGER than the daemon's: a scope still legitimately placing
//     can age out here and draw one extra floor grant — bounded to one per
//     outer scope per window by the lastFloorGrantAt rate limit;
//   - client grace SHORTER: this side holds an abandoned scope "live" a little
//     past the client's own kill, so floor recovery is merely delayed.
//
// A malformed or non-positive value falls back to the pinned default rather
// than disabling the grace: a zero grace would reintroduce the placement-window
// floor hole this constant exists to close.
func cpuSlotsPlacementGrace() time.Duration {
	raw := strings.TrimSpace(os.Getenv("AIRA_AITEST_PLACEMENT_ACK_TIMEOUT"))
	if raw == "" {
		return cpuSlotsPlacementGraceDefault
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return cpuSlotsPlacementGraceDefault
	}
	return time.Duration(seconds * float64(time.Second))
}

// cpuSlotsScanRoot derives the slice to scan from one worker-admit request's
// outer scope, which is a direct child of its slice by construction (every
// aitest outer scope is an `aira confine` scope).
//
// The `.aira-CONFINE-` guard is the whole safety property: without it a caller
// naming an arbitrary absolute path would have this daemon enumerate that
// path's parent. A scope that does not pass is not scanned and its CPU
// dimension is reported unevaluated (AIRA-64 plan section 4.9) — never assumed
// idle.
// Symlinks are resolved BEFORE the parent is taken (Sol build-review, P1):
// a `.aira-CONFINE-*` name that is a symlink would otherwise have the scan
// enumerate the link's directory while scope creation followed the link into
// the real slice — two different trees for one request. EvalSymlinks failing
// (including a path that does not exist) is reported as "cannot establish",
// not as a pass.
//
// The returned root is a CANDIDATE. cpuSlotsDecide additionally requires it to
// resolve through the admission slice resolver, which is what proves it is a
// real directory inside the cgroup2 mount rather than any parent a caller
// happened to name.
// It returns BOTH the candidate root and the resolved scope path, because the
// caller must key its snapshot lookup off the same resolution the scan will use
// — deriving the two separately is how a cache gets written under one key and
// read under another.
func cpuSlotsScanRoot(outerScope string) (root, scopePath string, ok bool) {
	if outerScope == "" || !filepath.IsAbs(outerScope) {
		return "", "", false
	}
	clean := filepath.Clean(outerScope)
	if !strings.HasPrefix(filepath.Base(clean), confineScopeChildPrefix) {
		return "", "", false
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		// Re-check the prefix on the RESOLVED path: a link named
		// `.aira-CONFINE-x` pointing at something else must not inherit the
		// name's authority.
		if !strings.HasPrefix(filepath.Base(resolved), confineScopeChildPrefix) {
			return "", "", false
		}
		clean = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", false
	}
	root = filepath.Dir(clean)
	if root == "" || root == "/" || root == "." {
		return "", "", false
	}
	return root, clean, true
}

// cpuSlotsSnapshot is one kernel-derived reading of a slice's aitest worker
// population. It deliberately carries TWO different counts over the same tree,
// because the two questions this gate asks must fail in opposite directions
// (AIRA-64 plan section 4.4).
type cpuSlotsSnapshot struct {
	// total is the DIRECTORY count of `.aira-worker-*` children across every
	// confine scope in the slice — the "how busy is the machine" number. It
	// never undercounts: an empty orphan still charges, which can only deny
	// above-floor growth, which is safe.
	total int
	// scopes is J: confine scopes holding at least one worker child. It is the
	// observable quantity the C + max(0, J-1) bound is stated in terms of.
	scopes int
	// liveForFloor maps an outer scope path to the number of its worker
	// children that count as LIVE for the liveness floor — populated, or
	// younger than the placement grace. It never overcounts a genuinely
	// abandoned scope, so an orphan can never withhold a job's floor worker.
	liveForFloor map[string]int
}

// parseCgroupEventsPopulated extracts the `populated` flag from a cgroup.events
// file. The second return reports whether the flag could be established at all;
// a caller must never treat "could not establish" as "not populated".
func parseCgroupEventsPopulated(data []byte) (bool, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "populated" {
			continue
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil {
			return false, false
		}
		return value != 0, true
	}
	return false, false
}

// workerScopeLiveForFloor reports whether one worker scope counts as a live
// worker for the liveness floor. It is exactly "is this cgroup POPULATED?".
//
// An unestablished reading resolves to LIVE. That is the direction that cannot
// fabricate an open floor: an over-count only denies above-floor growth, while
// an under-count admits workers the bound was supposed to refuse.
//
// WHY THERE IS NO LONGER AN AGE COMPONENT HERE. An earlier build paired this
// with "…or younger than the placement grace", reading directory mtime, to
// close the window between the daemon creating a scope and the client placing
// into it. The real-cgroup test refuted the mtime claim (a child-cgroup mkdir
// moves it), and the salvaged argument — "an abandoned scope has no process to
// create children inside it" — was then refuted by that same test, which
// creates a child inside a scope it is not a member of: cgroup membership does
// not govern who may mkdir in the directory (Sol build-review).
//
// The placement window is now closed by daemon-owned state instead of by
// filesystem inference: workerScopeState.lastGrantAt records when this daemon
// last granted under this outer scope, and cpuSlotsDecide refuses a floor grant
// inside one grace window of it. That is strictly better than the age gate on
// every axis — it needs no timestamp semantics, cannot be perturbed by anything
// outside the daemon, and it deleted more code than it added.
func workerScopeLiveForFloor(path string) bool {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return true
	}
	populated, established := parseCgroupEventsPopulated(data)
	if !established {
		return true
	}
	return populated
}

// scanSliceWorkerScopes reads one slice's aitest worker population directly
// from the cgroup tree.
//
// The tree IS the state (the same decision AIRA-39/AIRA-41 made for the RAM
// ledger), so this survives a daemon restart with no reconstruction, a killed
// relay cannot free capacity a live worker is still consuming, and a leaked
// relay cannot consume capacity no worker holds.
//
// A slice that cannot be listed is an ERROR, never an empty snapshot: a
// fabricated zero would read as "the machine is idle" and admit freely, which
// is precisely the failure this gate exists to prevent.
func scanSliceWorkerScopes(slicePath string) (cpuSlotsSnapshot, error) {
	entries, err := os.ReadDir(slicePath)
	if err != nil {
		return cpuSlotsSnapshot{}, fmt.Errorf("read slice %s: %w", slicePath, err)
	}
	snapshot := cpuSlotsSnapshot{liveForFloor: map[string]int{}}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), confineScopeChildPrefix) {
			continue
		}
		outer := filepath.Join(slicePath, entry.Name())
		children, err := os.ReadDir(outer)
		if err != nil {
			// ONLY a scope PROVEN to have vanished is skipped. That is the
			// benign teardown race between this listing and the child listing,
			// and a gone scope charges nothing, which is correct rather than an
			// undercount.
			//
			// Every other error propagates (Sol build-review, P0). Swallowing
			// them was a real fail-open: one persistently unreadable BUSY scope
			// made the whole slice look emptier than it is, so the gate admitted
			// past capacity while still reporting cpu_slots=ok — a false claim
			// of governance, and undetectable. The honest answer for a reading
			// this scan cannot establish is an error, which the caller renders
			// as `unevaluated`. Mirrors sumWorkerScopeChildren's own ENOENT
			// re-stat discipline exactly.
			if errors.Is(err, fs.ErrNotExist) {
				if _, statErr := os.Stat(outer); errors.Is(statErr, fs.ErrNotExist) {
					continue
				}
			}
			return cpuSlotsSnapshot{}, fmt.Errorf("read confine scope %s: %w", outer, err)
		}
		workers := 0
		for _, child := range children {
			if !child.IsDir() || !strings.HasPrefix(child.Name(), workerScopeChildPrefix) {
				continue
			}
			workers++
			if workerScopeLiveForFloor(filepath.Join(outer, child.Name())) {
				snapshot.liveForFloor[outer]++
			}
		}
		if workers > 0 {
			snapshot.scopes++
			snapshot.total += workers
		}
	}
	return snapshot, nil
}
