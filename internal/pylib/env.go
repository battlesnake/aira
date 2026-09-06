package pylib

import (
	"fmt"
	"io"
	"log"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var (
	extractAitestForChild = ExtractAitest
	aitestEnvFailureOnce  = new(sync.Once)
)

var aitestEnvironmentKeys = map[string]struct{}{
	"AIRA_AITEST_LIB":                  {},
	"AIRA_AITEST_WORKER_ADMIT_CMD":     {},
	"AIRA_AITEST_BOOTSTRAP_CMD":        {},
	"AIRA_AITEST_MAX_WORKERS_FALLBACK": {},
	"AIRA_AITEST_OUTER_SCOPE":          {},
}

// IsAitestEnvironmentKey reports whether key is aitest launch coordination
// rather than part of the tested child environment identity.
func IsAitestEnvironmentKey(key string) bool {
	_, ok := aitestEnvironmentKeys[key]
	return ok
}

// StripAitestEnvironment removes inherited or explicitly supplied aitest
// coordinates. Failed setup must disable aitest rather than retain stale
// state.
func StripAitestEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && IsAitestEnvironmentKey(key) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

// AppendAitestChildEnvironment publishes the aitest launch coordinates to a
// confined child.
//
// outerScope is the launching job's own confine scope path, and it is
// load-bearing rather than informational (AIRA-44): the aitest bootstrap verb
// used to self-discover the outer scope from whatever cgroup the CALLING process
// happened to be in, which is wrong for the second aitest-enabled pytest run in
// one confine job — by then the first run's bootstrap has relocated the whole
// process tree, `make` and its shell included, into <outer>/.aira-supervisor, so
// run 2 discovers THAT as its outer scope and every worker-admit call against
// the nested, deliberately-uncapped supervisor scope comes back
// "unevaluated: unbounded". Passing the real scope down from the launcher, which
// already holds it, removes the discovery step and with it the failure.
//
// An empty outerScope is honest, not fatal: the launcher passes what it has, and
// the bootstrap verb falls back to self-discovery with the same behaviour as
// before, which is still correct for a single-run job.
func AppendAitestChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer, workerAdmitCommand, outerScope string) []string {
	result := StripAitestEnvironment(env)
	if strings.TrimSpace(runtimeDir) == "" || strings.TrimSpace(workerAdmitCommand) == "" {
		return result
	}
	aitestDir, err := extractAitestForChild()
	if err != nil {
		aitestEnvFailureOnce.Do(func() {
			if diagnostics != nil {
				_, _ = fmt.Fprintf(diagnostics, "aitest disabled: %v\n", err)
				return
			}
			log.Printf("aitest disabled: %v", err)
		})
		return result
	}
	result = upsertChildEnv(result, "AIRA_AITEST_LIB", aitestDir)
	result = upsertChildEnv(result, "AIRA_AITEST_WORKER_ADMIT_CMD", workerAdmitCommand)
	result = upsertChildEnv(result, "AIRA_AITEST_BOOTSTRAP_CMD", workerAdmitCommand)
	result = upsertChildEnv(result, "AIRA_AITEST_MAX_WORKERS_FALLBACK", strconv.Itoa(runtime.NumCPU()))
	if scope := strings.TrimSpace(outerScope); scope != "" {
		result = upsertChildEnv(result, "AIRA_AITEST_OUTER_SCOPE", scope)
	}
	return result
}

// ConfineParentSliceEnv carries the RESOLVED slice cgroup path of the confine
// job a process is running inside, published by AppendConfineChildEnvironment
// and read back by runner.InheritedConfineSlice.
//
// AIRA-115. The name is deliberately NOT AIRA_CONFINE_SLICE, and that is the
// whole point of the separate constant. AIRA_CONFINE_SLICE is the OPERATOR's
// explicit-slice INPUT — `--slice` > `$AIRA_CONFINE_SLICE` > `aira.slice`, with
// an explicit value that never falls back (the install-owned-slice design, §4,
// via runner.ResolveConfineSlice). Publishing this job's own resolved coordinate
// under that name would make every NESTED `aira confine` read its parent's
// absolute cgroup path as an operator-declared explicit slice, bypassing default
// resolution's managed-unit guard and whale fallback, and feeding the same path
// to the daemon's management-slice resolution and to an auto-spawned daemon.
// An OUTPUT coordinate and an INPUT setting must not share a name.
const ConfineParentSliceEnv = "AIRA_CONFINE_PARENT_SLICE"

// coordinationEnvironmentKeys is the STRIP set: launch coordination rather than
// part of the tested child environment identity.
//
// AIRA-33 deleted the aira_xdist_governor plugin and every mechanism that read
// the nine legacy keys below, so AIRA never SETS one again. They stay in the
// strip set deliberately, and the asymmetry is the point: a child launched from
// inside a still-running pre-deletion job can still inherit a live AIRA_PY_LIB
// pointing at an extant extraction directory, and carrying that into a conftest
// that guards on it is a stale-plugin import path. Stripping costs nine map
// entries; not stripping costs a silent resurrection. AIRA_CONFINE_SCOPE_ID and
// AIRA_CONFINE_PARENT_SLICE are the two keys here that are both stripped and
// re-exported (see AppendConfineChildEnvironment), because
// runner.InheritedConfineScopeID and runner.InheritedConfineSlice read them to
// attach a confine-reserve sub-reservation to its parent job AND to the slice
// that job actually runs in. Both are stripped first for the same reason: a
// nested launch that inherited its grandparent's coordinates unchanged would
// charge the wrong scope and the wrong slice.
//
// AIRA_CONFINE_SLICE is deliberately ABSENT from this set: it is the operator's
// own input, not a coordinate AIRA emits, and a descendant is entitled to see
// the setting its operator exported.
var coordinationEnvironmentKeys = map[string]struct{}{
	"AIRA_CONFINE_SCOPE_ID": {},
	ConfineParentSliceEnv:   {},

	// Retired by AIRA-33: stripped, never set.
	"AIRA_PY_LIB":                   {},
	"AIRA_GOVERNOR":                 {},
	"AIRA_GOVERNOR_CMD":             {},
	"AIRA_GOVERNOR_MAX_WAIT":        {},
	"AIRA_GOVERNOR_SLICE":           {},
	"AIRA_TEST_MEM_GOVERNOR":        {},
	"AIRA_TEST_MEM_DEFAULT":         {},
	"AIRA_TEST_MEM_GROWTH_HEADROOM": {},
	"AIRA_CONFINE_RESERVE_CMD":      {},
}

// IsCoordinationEnvironmentKey reports whether key is launch coordination rather
// than part of the tested child environment identity.
func IsCoordinationEnvironmentKey(key string) bool {
	_, ok := coordinationEnvironmentKeys[key]
	return ok
}

// StripCoordinationEnvironment removes inherited or explicitly supplied launch
// coordinates. Failed setup must disable coordination rather than retain stale
// state.
func StripCoordinationEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && IsCoordinationEnvironmentKey(key) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

// AppendConfineChildEnvironment publishes the confine scope id and the RESOLVED
// slice of the job to a confined child, having first stripped every inherited
// launch coordinate.
//
// AIRA-115. The slice travels with the scope id because the two are one fact
// split in half: `aira confine-reserve` running inside the job identifies itself
// as a sub-reservation of that scope id, but used to let its slice DEFAULT to
// aira.slice. A job confined to any other slice therefore had its per-test
// sub-reservations charged to a slice whose cgroup does not hold that memory,
// while the slice that does hold it never saw them. Exporting the resolved slice
// (the same value the job's own admission was keyed on, so the sub-reservation
// lands in exactly its parent's daemon queue) is what closes that split.
//
// It is published under ConfineParentSliceEnv, never AIRA_CONFINE_SLICE — see
// that constant for why an emitted coordinate must not collide with the
// operator's explicit-slice input.
//
// Until AIRA-33 this also armed the aira_xdist_governor plugin on a
// --delegate-ram launch (extracting the sidecar, exporting AIRA_PY_LIB and the
// per-test RAM keys). All of that is gone; what a delegate-RAM launch means now
// is a pinned framework-overhead reserve and a generous scope ceiling, decided
// entirely in the runner, with no child-side participation.
//
// Behaviour improvement, stated so it is not mistaken for an accident: the scope
// id used to be exported only if the (now deleted) sidecar extraction succeeded
// AND a RuntimeDir was supplied. Neither gate has anything to do with the scope
// id, so both are gone and it now exports whenever there is one.
func AppendConfineChildEnvironment(env []string, scopeID, slice string) []string {
	result := StripCoordinationEnvironment(env)
	if scopeID != "" {
		result = upsertChildEnv(result, "AIRA_CONFINE_SCOPE_ID", scopeID)
	}
	// Exported independently of the scope id rather than only alongside it: the
	// slice is the correct charge for any reservation taken from inside this job,
	// whether or not a scope id was minted. The reverse pairing is the one that
	// must never happen — a scope id with no slice is what confineReserve refuses
	// rather than silently defaulting (AIRA-58's rule, AIRA-115's bug).
	if slice != "" {
		result = upsertChildEnv(result, ConfineParentSliceEnv, slice)
	}
	return result
}

func upsertChildEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	written := false
	for _, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			if !written {
				result = append(result, key+"="+value)
				written = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !written {
		result = append(result, key+"="+value)
	}
	return result
}
