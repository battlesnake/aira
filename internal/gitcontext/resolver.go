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

	"golang.org/x/sys/unix"
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
	// ReadFile reads the fixed metadata files (HEAD, config, packed-refs). Loose
	// refs, whose sub-path is attacker-influenced, are read separately through
	// an openat2 no-symlink walk so a malicious or unusual repository cannot
	// steer a read outside the ref store and have its contents reported as a
	// real hash.
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
	switch settings.storage {
	case storageReftable:
		headHash, headRef = unevaluated("reftable"), unevaluated("reftable")
	case storageFiles:
		if settings.hashLen == 0 {
			// Object format is set to something we cannot validate against, so
			// a files-backend read could accept a wrong-width hash.
			headHash, headRef = unevaluated("unknown-object-format"), unevaluated("unknown-object-format")
		} else {
			headHash, headRef = r.resolveHead(opts, settings.hashLen)
		}
	default: // storageUnknown: unreadable or include-dependent config.
		headHash, headRef = unevaluated("ref-storage-unknown"), unevaluated("ref-storage-unknown")
	}
	return snapshot{HeadHash: headHash, HeadRef: headRef, RemoteURL: settings.remote}
}

func (r Resolver) resolveHead(opts Options, hashLen int) (Field, Field) {
	data, err := r.ReadFile(filepath.Join(opts.GitDir, "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		return none("absent"), none("absent")
	}
	if err != nil {
		return unevaluated("unreadable"), unevaluated("unreadable")
	}
	value := strings.TrimSpace(string(data))
	if hashValid(value, hashLen) {
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
			hash, packed, packedErr := r.readPackedRef(opts.CommonDir, ref, hashLen)
			if packedErr != nil {
				return unevaluated("unreadable"), unevaluated("unreadable")
			}
			if packed {
				return valueOf(hash), valueOf(ref)
			}
			return none("unborn"), valueOf(ref)
		}
		text := strings.TrimSpace(string(data))
		if hashValid(text, hashLen) {
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
	roots := []string{opts.GitDir}
	if opts.CommonDir != "" && opts.CommonDir != opts.GitDir {
		roots = append(roots, opts.CommonDir)
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		data, found, err := readLooseRefBeneath(root, ref)
		if err != nil {
			return nil, false, err
		}
		if found {
			return data, true, nil
		}
	}
	return nil, false, nil
}

// readLooseRefBeneath (per-platform) opens ref strictly beneath root without
// following any symlink. On Linux it uses openat2(RESOLVE_BENEATH|
// RESOLVE_NO_SYMLINKS); elsewhere it falls back to the Lstat-walk guard. It
// lives in resolver_linux.go / resolver_other.go.

// readLooseRefBeneathFallback is the pre-openat2 path: reject any symlink or
// out-of-root component with an Lstat walk, then open O_NOFOLLOW. It leaves a
// narrow check-then-open window an active local attacker could race, which is
// outside AIRA's machine-local single-user threat model.
func readLooseRefBeneathFallback(root, ref string) ([]byte, bool, error) {
	if err := guardRefBeneathRoot(root, ref); err != nil {
		return nil, false, err
	}
	data, err := readBoundedRegularFile(filepath.Join(root, filepath.FromSlash(ref)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func (r Resolver) readPackedRef(commonDir, ref string, hashLen int) (string, bool, error) {
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
			if !hashValid(fields[0], hashLen) {
				return "", false, fmt.Errorf("invalid packed hash")
			}
			return strings.ToLower(fields[0]), true, nil
		}
	}
	return "", false, nil
}

// refStorageKind records what we could establish about the ref backend. Only a
// known files backend permits interpreting HEAD/refs as files.
type refStorageKind int

const (
	storageFiles refStorageKind = iota
	storageReftable
	storageUnknown
)

type configSettings struct {
	remote  Field
	storage refStorageKind
	// hashLen is the expected hex hash width (40 for sha1, 64 for sha256);
	// 0 when the object format could not be established.
	hashLen int
}

var configSectionPattern = regexp.MustCompile(`^\[\s*([^\s\]"]+)(?:\s+"((?:[^"\\]|\\.)*)")?\s*\]$`)

func parseConfig(data []byte, readErr error) configSettings {
	if errors.Is(readErr, os.ErrNotExist) {
		// No config file at all: git resolves refs with its built-in defaults,
		// which are the files backend and the SHA-1 object format.
		return configSettings{remote: none("absent"), storage: storageFiles, hashLen: 40}
	}
	if readErr != nil {
		// Unreadable config: neither the ref backend nor the object format can
		// be established, so HEAD must not be interpreted as files.
		return configSettings{remote: unevaluated("unreadable"), storage: storageUnknown}
	}
	logical := configLogicalLines(string(data))
	section, subsection := "", ""
	remote := none("absent")
	storage := storageFiles
	hashLen := 40
	hasInclude := false
	for _, raw := range logical {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if match := configSectionPattern.FindStringSubmatch(line); match != nil {
			section, subsection = strings.ToLower(match[1]), strings.ToLower(unescapeConfig(match[2]))
			if section == "include" || section == "includeif" {
				hasInclude = true
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
		switch {
		case ((section == "include" || section == "includeif") && key == "path") || key == "include.path":
			hasInclude = true
		case section == "extensions" && subsection == "" && key == "refstorage":
			switch strings.ToLower(value) {
			case "reftable":
				storage = storageReftable
			case "files":
				storage = storageFiles
			default:
				storage = storageUnknown
			}
		case section == "extensions" && subsection == "" && key == "objectformat":
			switch strings.ToLower(value) {
			case "sha1":
				hashLen = 40
			case "sha256":
				hashLen = 64
			default:
				hashLen = 0
			}
		case section == "remote" && subsection == "origin" && key == "url":
			remote = valueOf(gitremote.RedactURL(value))
		}
	}
	if hasInclude {
		// An include(If) can silently set the URL, ref backend, or object
		// format from another file we do not read; none can be trusted.
		return configSettings{remote: unevaluated("config-include"), storage: storageUnknown}
	}
	return configSettings{remote: remote, storage: storage, hashLen: hashLen}
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

// validRef enforces the subset of git check-ref-format we rely on: a refs/…
// path whose every component is a legal refname component. A purely lexical
// check is not enough — an accepted but malformed name (a leading dot, a .lock
// component, a control byte) could otherwise steer a file read.
func validRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") {
		return false
	}
	if strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return false
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return false
	}
	for _, component := range strings.Split(ref, "/") {
		if !validRefComponent(component) {
			return false
		}
	}
	return true
}

func validRefComponent(component string) bool {
	if component == "" { // empty ⇒ leading, trailing, or doubled slash
		return false
	}
	if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
		return false
	}
	for _, r := range component {
		if r < 0x20 || r == 0x7f { // ASCII control characters and DEL
			return false
		}
		if strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	return true
}

func hashValid(value string, hashLen int) bool {
	if hashLen == 0 || len(value) != hashLen {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

var (
	errRefEscapesRoot = errors.New("ref path escapes repository root")
	errRefSymlink     = errors.New("ref path traverses a symlink")
)

// guardRefBeneathRoot verifies that ref, resolved under root, stays within root
// and that no existing path component is a symlink. A component that does not
// yet exist ends the walk (there is nothing further to follow); the ReadFile
// then simply reports it absent. Injected in-memory tests supply their own
// content without touching disk, so a non-existent walk is transparent to them.
func guardRefBeneathRoot(root, ref string) error {
	full := filepath.Join(root, filepath.FromSlash(ref))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errRefEscapesRoot
	}
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errRefSymlink
		}
	}
	return nil
}

func readBoundedRegularFile(path string) ([]byte, error) {
	// O_NOFOLLOW closes the last-component symlink race the path guard cannot
	// (a symlink swapped in after the Lstat); a symlinked final component then
	// fails to open and is treated as unreadable rather than followed.
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
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
