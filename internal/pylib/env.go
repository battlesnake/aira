package pylib

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

var (
	extractForChild     = ExtractPyLib
	childEnvFailureOnce = new(sync.Once)
)

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
