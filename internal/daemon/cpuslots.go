//go:build linux

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// desiredCPUSlots is the daemon governor's active-set capacity. Reserve one
// CPU for interactive work by default, but never reduce capacity below one.
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
func cpuSlotsScanRoot(outerScope string) (string, bool) {
	if outerScope == "" || !filepath.IsAbs(outerScope) {
		return "", false
	}
	clean := filepath.Clean(outerScope)
	if !strings.HasPrefix(filepath.Base(clean), confineScopeChildPrefix) {
		return "", false
	}
	root := filepath.Dir(clean)
	if root == "" || root == "/" || root == "." {
		return "", false
	}
	return root, true
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

// workerScopeLiveForFloor is the single predicate the liveness floor is
// defined by: a worker scope counts as live when it is POPULATED **or** YOUNGER
// THAN THE PLACEMENT GRACE.
//
// Both halves are load-bearing and each closes a hole the other leaves open:
//
//   - populated-only would let N supervisors paused between their grant and
//     their fork each read "zero live workers" and each take a floor grant,
//     since the daemon creates the scope before the client places into it
//     (Sol plan-review round 2);
//   - young-only (i.e. a plain directory count) would let ONE empty orphan —
//     from a grant whose response write failed, or a relay the client gave up
//     on — hold the floor closed forever, so a job that today merely runs
//     slowly would instead never run at all (Sol plan-review round 1).
//
// Every unestablished reading resolves to LIVE. That is the direction that
// cannot fabricate an open floor: an over-count only denies above-floor growth,
// while an under-count admits workers the bound was supposed to refuse.
// A future mtime (clock skew) yields a negative age, which is < grace, so it
// reads as young — live — for the same reason.
//
// WHAT `mtime` ACTUALLY MEANS HERE, measured rather than assumed
// (TestWorkerScopeMtimeMovesOnlyOnDirectoryEntryChanges): it is the time of the
// last DIRECTORY-ENTRY change in the scope, not its creation time. Population
// (a cgroup.procs write) and control-file writes do not move it; a child-cgroup
// mkdir or rmdir does. The gate needs exactly one property and that is enough
// for it: an ABANDONED scope's mtime is frozen, because an abandoned scope holds
// no process that could create or remove a child inside it, so it really does
// age out and the floor really does open. A LIVE worker that nests cgroups only
// refreshes its own mtime — it looks younger, and it IS live, so counting it
// live is correct. The movement is one-directional in the safe sense; an earlier
// draft of this comment claimed mtime was creation time, and the real-cgroup
// test refuted that on its first run.
func workerScopeLiveForFloor(path string, now time.Time, grace time.Duration) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	if now.Sub(info.ModTime()) < grace {
		return true
	}
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
func scanSliceWorkerScopes(slicePath string, now time.Time, grace time.Duration) (cpuSlotsSnapshot, error) {
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
			// A confine scope that vanished between the two listings is the
			// benign teardown race and charges nothing, which is correct
			// rather than an undercount. Any other error is skipped for the
			// same reason: it concerns one scope, and failing the whole scan
			// would report the entire machine unevaluated because one job is
			// mid-teardown.
			continue
		}
		workers := 0
		for _, child := range children {
			if !child.IsDir() || !strings.HasPrefix(child.Name(), workerScopeChildPrefix) {
				continue
			}
			workers++
			if workerScopeLiveForFloor(filepath.Join(outer, child.Name()), now, grace) {
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
