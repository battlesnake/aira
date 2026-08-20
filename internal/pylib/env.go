package pylib

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	extractForChild     = ExtractPyLib
	childEnvFailureOnce = new(sync.Once)
)

// AppendChildEnvironment exposes the sidecar and machine-local slot set to a
// child. Extraction is advisory: on any failure it returns the original
// environment, including no partial slot or tuning variables.
func AppendChildEnvironment(env []string, runtimeDir string, diagnostics io.Writer) []string {
	if strings.TrimSpace(runtimeDir) == "" {
		return env
	}
	pythonDir, err := extractForChild()
	if err != nil {
		childEnvFailureOnce.Do(func() {
			if diagnostics != nil {
				_, _ = fmt.Fprintf(diagnostics, "aira CPU governor disabled: %v\n", err)
			}
		})
		return env
	}
	result := upsertChildEnv(env, "AIRA_PY_LIB", pythonDir)
	result = upsertChildEnv(result, "AIRA_CPU_SLOTS_DIR", filepath.Join(runtimeDir, "cpuslots"))
	for _, key := range []string{"AIRA_CPU_POLL_INTERVAL", "AIRA_CPU_MAX_WAIT"} {
		if value, configured := os.LookupEnv(key); configured && !hasChildEnv(result, key) {
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

func hasChildEnv(env []string, key string) bool {
	for _, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			return true
		}
	}
	return false
}
