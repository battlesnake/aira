// Package gitcontext resolves caller-observed Git provenance without starting
// a Git subprocess. Unsupported storage forms are reported as unevaluated.
package gitcontext

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"aira/internal/gitremote"
)

const ResolverVersion = "git-files-v1"

type Status string

const (
	StatusValue       Status = "value"
	StatusNone        Status = "none"
	StatusUnevaluated Status = "unevaluated"
	// StatusMismatch is assigned by the receiving store after scope
	// cross-checking. The caller-side resolver never emits it.
	StatusMismatch Status = "mismatch"
)

type Field struct {
	Value  string `json:"value,omitempty"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type GitContext struct {
	RepoRoot        Field  `json:"repo_root"`
	WorktreePath    Field  `json:"worktree_path"`
	WorktreeID      Field  `json:"worktree_id"`
	HeadHash        Field  `json:"head_hash"`
	HeadRef         Field  `json:"head_ref"`
	RemoteURL       Field  `json:"remote_url"`
	ObservedAt      string `json:"observed_at"`
	ResolverVersion string `json:"resolver_version"`
}

type Options struct {
	RepoRoot, WorktreePath, CommonDir, GitDir, WorktreeID string
	Now                                                   func() time.Time
}

type Resolver struct {
	ReadFile    func(string) ([]byte, error)
	MaxAttempts int
}

func NewResolver() Resolver {
	return Resolver{ReadFile: readBoundedRegularFile, MaxAttempts: 6}
}

type snapshot struct {
	HeadHash, HeadRef, RemoteURL Field
}

func (r Resolver) Resolve(opts Options) GitContext {
	if r.ReadFile == nil {
		r.ReadFile = readBoundedRegularFile
	}
	if r.MaxAttempts < 2 {
		r.MaxAttempts = 2
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	result := GitContext{
		RepoRoot: fieldForValue(opts.RepoRoot, "absent"), WorktreePath: fieldForValue(opts.WorktreePath, "absent"),
		WorktreeID: fieldForValue(opts.WorktreeID, "absent"), ObservedAt: now().UTC().Format(time.RFC3339Nano),
		ResolverVersion: ResolverVersion,
	}
	var previous snapshot
	havePrevious := false
	for attempt := 0; attempt < r.MaxAttempts; attempt++ {
		current := r.resolveSnapshot(opts)
		if havePrevious && current == previous {
			result.HeadHash, result.HeadRef, result.RemoteURL = current.HeadHash, current.HeadRef, current.RemoteURL
			return result
		}
		previous, havePrevious = current, true
	}
	result.HeadHash = Field{Status: StatusUnevaluated, Reason: "ref-update-race"}
	result.HeadRef = Field{Status: StatusUnevaluated, Reason: "ref-update-race"}
	result.RemoteURL = previous.RemoteURL
	return result
}

func (r Resolver) resolveSnapshot(opts Options) snapshot {
	config, configErr := r.ReadFile(filepath.Join(opts.CommonDir, "config"))
	settings := parseConfig(config, configErr)
	var headHash, headRef Field
	if settings.reftable {
		headHash, headRef = unevaluated("reftable"), unevaluated("reftable")
	} else {
		headHash, headRef = r.resolveHead(opts)
	}
	return snapshot{HeadHash: headHash, HeadRef: headRef, RemoteURL: settings.remote}
}

func (r Resolver) resolveHead(opts Options) (Field, Field) {
	data, err := r.ReadFile(filepath.Join(opts.GitDir, "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		return none("absent"), none("absent")
	}
	if err != nil {
		return unevaluated("unreadable"), unevaluated("unreadable")
	}
	value := strings.TrimSpace(string(data))
	if hashValid(value) {
		return valueOf(strings.ToLower(value)), none("detached")
	}
	if !strings.HasPrefix(value, "ref: ") {
		return unevaluated("unusual-head"), unevaluated("unusual-head")
	}
	ref := strings.TrimSpace(strings.TrimPrefix(value, "ref: "))
	seen := map[string]bool{}
	for depth := 0; depth < 8; depth++ {
		if !validRef(ref) || seen[ref] {
			return unevaluated("unusual-ref-storage"), unevaluated("unusual-ref-storage")
		}
		seen[ref] = true
		data, found, readErr := r.readLooseRef(opts, ref)
		if readErr != nil {
			return unevaluated("unreadable"), unevaluated("unreadable")
		}
		if !found {
			hash, packed, packedErr := r.readPackedRef(opts.CommonDir, ref)
			if packedErr != nil {
				return unevaluated("unreadable"), unevaluated("unreadable")
			}
			if packed {
				return valueOf(hash), valueOf(ref)
			}
			return none("unborn"), valueOf(ref)
		}
		text := strings.TrimSpace(string(data))
		if hashValid(text) {
			return valueOf(strings.ToLower(text)), valueOf(ref)
		}
		if strings.HasPrefix(text, "ref: ") {
			ref = strings.TrimSpace(strings.TrimPrefix(text, "ref: "))
			continue
		}
		return unevaluated("unusual-ref-storage"), unevaluated("unusual-ref-storage")
	}
	return unevaluated("symbolic-ref-depth"), unevaluated("symbolic-ref-depth")
}

func (r Resolver) readLooseRef(opts Options, ref string) ([]byte, bool, error) {
	paths := []string{filepath.Join(opts.GitDir, filepath.FromSlash(ref))}
	common := filepath.Join(opts.CommonDir, filepath.FromSlash(ref))
	if common != paths[0] {
		paths = append(paths, common)
	}
	for _, path := range paths {
		data, err := r.ReadFile(path)
		if err == nil {
			return data, true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func (r Resolver) readPackedRef(commonDir, ref string) (string, bool, error) {
	data, err := r.ReadFile(filepath.Join(commonDir, "packed-refs"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", false, fmt.Errorf("unusual packed-refs line")
		}
		if fields[1] == ref {
			if !hashValid(fields[0]) {
				return "", false, fmt.Errorf("invalid packed hash")
			}
			return strings.ToLower(fields[0]), true, nil
		}
	}
	return "", false, nil
}

type configSettings struct {
	remote   Field
	reftable bool
}

var configSectionPattern = regexp.MustCompile(`^\[\s*([^\s\]"]+)(?:\s+"((?:[^"\\]|\\.)*)")?\s*\]$`)

func parseConfig(data []byte, readErr error) configSettings {
	if errors.Is(readErr, os.ErrNotExist) {
		return configSettings{remote: none("absent")}
	}
	if readErr != nil {
		return configSettings{remote: unevaluated("unreadable")}
	}
	logical := configLogicalLines(string(data))
	section, subsection := "", ""
	settings := configSettings{remote: none("absent")}
	for _, raw := range logical {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if match := configSectionPattern.FindStringSubmatch(line); match != nil {
			section, subsection = strings.ToLower(match[1]), strings.ToLower(unescapeConfig(match[2]))
			if section == "include" || section == "includeif" {
				settings.remote = unevaluated("config-include")
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			key = fields[0]
			value = strings.TrimSpace(strings.TrimPrefix(line, key))
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = parseConfigValue(value)
		if section == "extensions" && key == "refstorage" && strings.EqualFold(value, "reftable") {
			settings.reftable = true
		}
		if ((section == "include" || section == "includeif") && key == "path") || key == "include.path" {
			settings.remote = unevaluated("config-include")
		}
		if section == "remote" && subsection == "origin" && key == "url" && settings.remote.Reason != "config-include" {
			settings.remote = valueOf(gitremote.RedactURL(value))
		}
	}
	return settings
}

func configLogicalLines(value string) []string {
	physical := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	result := make([]string, 0, len(physical))
	current := ""
	for _, line := range physical {
		current += line
		trimmed := strings.TrimRight(current, " \t")
		if trailingBackslashes(trimmed)%2 == 1 {
			current = strings.TrimSuffix(trimmed, "\\")
			continue
		}
		result = append(result, current)
		current = ""
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

func trailingBackslashes(value string) int {
	count := 0
	for i := len(value) - 1; i >= 0 && value[i] == '\\'; i-- {
		count++
	}
	return count
}

func parseConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return unescapeConfig(value[1 : len(value)-1])
	}
	if index := strings.IndexAny(value, "#;"); index >= 0 && (index == 0 || value[index-1] == ' ' || value[index-1] == '\t') {
		value = value[:index]
	}
	return strings.TrimSpace(value)
}

func unescapeConfig(value string) string {
	replacer := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t", `\b`, "\b")
	return replacer.Replace(value)
}

func validRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	return !strings.ContainsAny(ref, " ~^:?*[\\") && !strings.HasSuffix(ref, ".") && !strings.HasSuffix(ref, ".lock")
}

func hashValid(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func readBoundedRegularFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	const max = 16 << 20
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, errors.New("file exceeds resolver limit")
	}
	return bytes.Clone(data), nil
}

func valueOf(value string) Field      { return Field{Value: value, Status: StatusValue} }
func none(reason string) Field        { return Field{Status: StatusNone, Reason: reason} }
func unevaluated(reason string) Field { return Field{Status: StatusUnevaluated, Reason: reason} }
func fieldForValue(value, absentReason string) Field {
	if value == "" {
		return none(absentReason)
	}
	return valueOf(value)
}
