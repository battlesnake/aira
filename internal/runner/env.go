package runner

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"aira/internal/pylib"
)

var locateLibstdbufFn = locateLibstdbuf

func locateLibstdbuf() string {
	candidates := make([]string, 0)
	if matches, err := filepath.Glob("/usr/lib/*/coreutils/libstdbuf.so"); err == nil {
		candidates = append(candidates, matches...)
	}
	candidates = append(candidates,
		"/usr/libexec/coreutils/libstdbuf.so",
		"/usr/lib/coreutils/libstdbuf.so",
	)
	if stdbuf, err := exec.LookPath("stdbuf"); err == nil {
		dir := filepath.Dir(stdbuf)
		if matches, globErr := filepath.Glob(filepath.Join(dir, "..", "lib*", "coreutils", "libstdbuf.so")); globErr == nil {
			candidates = append(candidates, matches...)
		}
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		file, err := os.Open(candidate)
		if err != nil {
			continue
		}
		_ = file.Close()
		return candidate
	}
	return ""
}

func stdbufInjection(env []string) (out []string, applied bool) {
	path := locateLibstdbufFn()
	if path == "" {
		return env, false
	}

	preload := ""
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok && key == "LD_PRELOAD" {
			preload = value
			break
		}
	}
	if preload != "" {
		preload = path + ":" + preload
	} else {
		preload = path
	}
	values := map[string]string{
		"_STDBUF_O":        "L",
		"_STDBUF_E":        "L",
		"PYTHONUNBUFFERED": "1",
		"LD_PRELOAD":       preload,
	}
	keys := []string{"_STDBUF_O", "_STDBUF_E", "PYTHONUNBUFFERED", "LD_PRELOAD"}
	seen := make(map[string]bool, len(keys))
	out = make([]string, 0, len(env)+len(keys))
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if value, replace := values[key]; ok && replace {
			if !seen[key] {
				out = append(out, key+"="+value)
				seen[key] = true
			}
			continue
		}
		out = append(out, item)
	}
	for _, key := range keys {
		if !seen[key] {
			out = append(out, key+"="+values[key])
		}
	}
	return out, true
}

func EnvDigest(entries []EnvEntry) (string, error) {
	seen := make(map[string]struct{}, len(entries))
	ordered := append([]EnvEntry(nil), entries...)
	for _, e := range ordered {
		key := string(e.Key)
		if key == "" || strings.ContainsRune(key, '=') {
			return "", fmt.Errorf("E_RUN_ENV_INVALID: invalid environment key %q", key)
		}
		if _, ok := seen[key]; ok {
			return "", fmt.Errorf("E_RUN_ENV_INVALID: duplicate environment key %q", key)
		}
		seen[key] = struct{}{}
	}
	sort.Slice(ordered, func(i, j int) bool { return string(ordered[i].Key) < string(ordered[j].Key) })
	h := sha256.New()
	var buf [binary.MaxVarintLen64]byte
	for _, e := range ordered {
		n := binary.PutUvarint(buf[:], uint64(len(e.Key)))
		_, _ = h.Write(buf[:n])
		_, _ = h.Write(e.Key)
		n = binary.PutUvarint(buf[:], uint64(len(e.Value)))
		_, _ = h.Write(buf[:n])
		_, _ = h.Write(e.Value)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StripCoordinationEnv removes launch-coordination entries from the environment
// identity shared by runners and command gates.
func StripCoordinationEnv(entries []EnvEntry) []EnvEntry {
	result := make([]EnvEntry, 0, len(entries))
	for _, entry := range entries {
		if !pylib.IsCoordinationEnvironmentKey(string(entry.Key)) {
			result = append(result, entry)
		}
	}
	return result
}

func effectiveEnvironment(overrides []string) ([]string, []EnvEntry, error) {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.IndexByte(key, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, nil, fmt.Errorf("E_RUN_ENV_INVALID: invalid inherited environment entry")
		}
		if _, exists := values[key]; exists {
			return nil, nil, fmt.Errorf("E_RUN_ENV_INVALID: duplicate inherited environment key %q", key)
		}
		values[key] = value
	}
	overrideKeys := make(map[string]struct{}, len(overrides))
	for _, item := range overrides {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return nil, nil, fmt.Errorf("E_RUN_ENV_INVALID: invalid override")
		}
		if _, exists := overrideKeys[key]; exists {
			return nil, nil, fmt.Errorf("E_RUN_ENV_INVALID: duplicate override key %q", key)
		}
		overrideKeys[key] = struct{}{}
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	entries := make([]EnvEntry, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
		entries = append(entries, EnvEntry{Key: []byte(key), Value: []byte(values[key])})
	}
	return result, entries, nil
}

func explicitEnvironment(values []string) ([]string, []EnvEntry, error) {
	seen := make(map[string]struct{}, len(values))
	keys := make([]string, 0, len(values))
	parsed := make(map[string]string, len(values))
	for _, item := range values {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return nil, nil, fmt.Errorf("E_RUN_ENV_INVALID: invalid explicit environment entry")
		}
		if _, exists := seen[key]; exists {
			return nil, nil, fmt.Errorf("E_RUN_ENV_INVALID: duplicate explicit environment key %q", key)
		}
		seen[key] = struct{}{}
		parsed[key] = value
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	entries := make([]EnvEntry, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+parsed[key])
		entries = append(entries, EnvEntry{Key: []byte(key), Value: []byte(parsed[key])})
	}
	return env, entries, nil
}

func validatePrefix(prefix []string) ([]string, error) {
	prefix = append([]string(nil), prefix...)
	if len(prefix) == 0 {
		return prefix, nil
	}
	delimiters := 0
	for _, token := range prefix {
		if token == "--" {
			delimiters++
		}
	}
	if delimiters > 1 || (delimiters == 1 && prefix[len(prefix)-1] != "--") {
		return nil, fmt.Errorf("E_RUN_PREFIX_INVALID: prefix delimiter must occur once at the end")
	}
	if delimiters == 1 {
		prefix = prefix[:len(prefix)-1]
	}
	if len(prefix) == 0 {
		return nil, fmt.Errorf("E_RUN_PREFIX_INVALID: empty prefix")
	}
	return prefix, nil
}

// EffectivePrefix selects the requested launch prefix when it is non-nil;
// otherwise it selects the configured prefix. A non-nil empty request
// deliberately suppresses a configured prefix.
func EffectivePrefix(configured, requested []string) ([]string, error) {
	if requested == nil {
		requested = configured
	}
	return validatePrefix(requested)
}
