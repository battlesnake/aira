package pylib

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var (
	extractForChild     = ExtractPyLib
	childEnvFailureOnce = new(sync.Once)

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

var governorEnvironmentKeys = map[string]struct{}{
	"AIRA_PY_LIB":                   {},
	"AIRA_GOVERNOR":                 {},
	"AIRA_GOVERNOR_CMD":             {},
	"AIRA_GOVERNOR_MAX_WAIT":        {},
	"AIRA_CONFINE_SCOPE_ID":         {},
	"AIRA_GOVERNOR_SLICE":           {},
	"AIRA_TEST_MEM_GOVERNOR":        {},
	"AIRA_TEST_MEM_DEFAULT":         {},
	"AIRA_TEST_MEM_GROWTH_HEADROOM": {},
	"AIRA_CONFINE_RESERVE_CMD":      {},
}

const DefaultTestMemoryReserve = "512M"

// IsGovernorEnvironmentKey reports whether key is launch coordination rather
// than part of the tested child environment identity.
func IsGovernorEnvironmentKey(key string) bool {
	_, ok := governorEnvironmentKeys[key]
	return ok
}

// StripGovernorEnvironment removes inherited or explicitly supplied governor
// coordinates. Failed setup must disable gating rather than retain stale state.
func StripGovernorEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && IsGovernorEnvironmentKey(key) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

// AppendChildEnvironment exposes the sidecar to a child. Extraction is
// advisory: on any failure it returns an environment with
// every governor variable stripped, disabling gating instead of using stale
// inherited coordinates.
func AppendChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer) []string {
	return appendChildEnvironment(env, runtimeDir, diagnostics, false, "", "", "", "")
}

// AppendConfineChildEnvironment couples per-test RAM governance to an explicit
// delegate-RAM confine launch. Every other launch strips these coordinates.
func AppendConfineChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer, delegateRAM bool, reserveCommand, memoryDefault, scopeID, slice string) []string {
	return appendChildEnvironment(env, runtimeDir, diagnostics, delegateRAM, reserveCommand, memoryDefault, scopeID, slice)
}

func appendChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer, delegateRAM bool, reserveCommand, memoryDefault, scopeID, slice string) []string {
	result := StripGovernorEnvironment(env)
	if strings.TrimSpace(runtimeDir) == "" {
		return result
	}
	pythonDir, err := extractForChild()
	if err != nil {
		childEnvFailureOnce.Do(func() {
			if diagnostics != nil {
				_, _ = fmt.Fprintf(diagnostics, "aira scheduler governor disabled: %v\n", err)
				return
			}
			log.Printf("aira scheduler governor disabled: %v", err)
		})
		return result
	}
	result = upsertChildEnv(result, "AIRA_PY_LIB", pythonDir)
	if scopeID != "" {
		result = upsertChildEnv(result, "AIRA_CONFINE_SCOPE_ID", scopeID)
	}
	for _, key := range []string{"AIRA_GOVERNOR_MAX_WAIT"} {
		if value, configured := os.LookupEnv(key); configured {
			result = append(result, key+"="+value)
		}
	}
	if delegateRAM {
		if strings.TrimSpace(memoryDefault) == "" {
			memoryDefault = DefaultTestMemoryReserve
		}
		result = upsertChildEnv(result, "AIRA_TEST_MEM_GOVERNOR", "1")
		// Kept for the plugin's malformed-marker fallback. Unmarked tests reserve
		// measured RSS plus growth headroom; this value is no longer their floor.
		result = upsertChildEnv(result, "AIRA_TEST_MEM_DEFAULT", memoryDefault)
		result = upsertChildEnv(result, "AIRA_CONFINE_RESERVE_CMD", reserveCommand)
		// The relay uses the same resolved self binary as confine-reserve and
		// identifies this confined pytest session by its scope id.
		result = upsertChildEnv(result, "AIRA_GOVERNOR_CMD", reserveCommand)
		result = upsertChildEnv(result, "AIRA_GOVERNOR_SLICE", slice)
		governor := strings.TrimSpace(os.Getenv("AIRA_GOVERNOR"))
		if governor != "off" {
			governor = "daemon"
		}
		result = upsertChildEnv(result, "AIRA_GOVERNOR", governor)
		if value, configured := os.LookupEnv("AIRA_TEST_MEM_GROWTH_HEADROOM"); configured {
			result = upsertChildEnv(result, "AIRA_TEST_MEM_GROWTH_HEADROOM", value)
		}
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
