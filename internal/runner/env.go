package runner

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

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

func effectivePrefix(configured, requested []string) ([]string, error) {
	if requested == nil {
		requested = configured
	}
	return validatePrefix(requested)
}
