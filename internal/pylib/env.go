package pylib

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	extractForChild     = ExtractPyLib
	childEnvFailureOnce = new(sync.Once)
)

var governorEnvironmentKeys = map[string]struct{}{
	"AIRA_PY_LIB":            {},
	"AIRA_CPU_SLOTS_DIR":     {},
	"AIRA_CPU_POLL_INTERVAL": {},
	"AIRA_CPU_MAX_WAIT":      {},
}

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

// AppendChildEnvironment exposes the sidecar and machine-local slot set to a
// child. Extraction is advisory: on any failure it returns an environment with
// every governor variable stripped, disabling gating instead of using stale
// inherited coordinates.
func AppendChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer) []string {
	result := StripGovernorEnvironment(env)
	if strings.TrimSpace(runtimeDir) == "" {
		return result
	}
	pythonDir, err := extractForChild()
	if err != nil {
		childEnvFailureOnce.Do(func() {
			if diagnostics != nil {
				_, _ = fmt.Fprintf(diagnostics, "aira CPU governor disabled: %v\n", err)
				return
			}
			log.Printf("aira CPU governor disabled: %v", err)
		})
		return result
	}
	result = upsertChildEnv(result, "AIRA_PY_LIB", pythonDir)
	result = upsertChildEnv(result, "AIRA_CPU_SLOTS_DIR", filepath.Join(runtimeDir, "cpuslots"))
	for _, key := range []string{"AIRA_CPU_POLL_INTERVAL", "AIRA_CPU_MAX_WAIT"} {
		if value, configured := os.LookupEnv(key); configured {
			result = append(result, key+"="+value)
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
