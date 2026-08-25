//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type confineScanDeps struct {
	now       func() time.Time
	readField func(*linuxScope, string, int64) ([]byte, error)
	waitEmpty func(context.Context, Scope, time.Duration) error
}

func defaultConfineScanDeps() confineScanDeps {
	return confineScanDeps{now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty}
}

func ResolveConfineManagementSlice(slice string) (string, string, error) {
	deps := fillConfineDeps(confineDeps{})
	explicit := ResolveConfineSlice(slice)
	if explicit == "" {
		return resolveDefaultConfineSlice(deps)
	}
	path, ok, reason := deps.resolveSlicePath(explicit)
	if !ok {
		if reason == "" {
			reason = "slice-not-found"
		}
		return explicit, "", confineUnavailable(explicit, errors.New(reason))
	}
	return explicit, path, nil
}

func readConfineScopeField(scope *linuxScope, name string, limit int64) ([]byte, error) {
	file, err := scope.openFile(name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

func listConfines(ctx context.Context, slicePath string, registry []ConfineRegistryEntry) (ConfineListResult, error) {
	return listConfinesWithDeps(ctx, slicePath, registry, defaultConfineScanDeps())
}

func listConfinesWithDeps(ctx context.Context, slicePath string, registry []ConfineRegistryEntry, deps confineScanDeps) (ConfineListResult, error) {
	if err := ctx.Err(); err != nil {
		return ConfineListResult{}, err
	}
	entries, err := os.ReadDir(slicePath)
	if err != nil {
		return ConfineListResult{Verdict: "unevaluated", Reason: err.Error(), Scopes: []ConfineRecord{}}, nil
	}
	byID := make(map[string]ConfineRecord)
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".aira-CONFINE-") {
			continue
		}
		scopeID := strings.TrimPrefix(entry.Name(), ".aira-")
		name, pid, stamp, ok := parseConfineScopeID(scopeID)
		if !ok {
			continue
		}
		record := ConfineRecord{Name: name, Owner: ConfineUnknownOwner, ScopeID: scopeID, SupervisorPID: &pid}
		if age := deps.now().Sub(time.Unix(0, stamp)); age >= 0 {
			seconds := int64(age / time.Second)
			record.AgeSeconds = &seconds
		} else {
			record.UnevaluatedFields = append(record.UnevaluatedFields, "age")
		}
		path := filepath.Join(slicePath, entry.Name())
		fd, openErr := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			record.UnevaluatedFields = append(record.UnevaluatedFields, "populated", "rss", "cap")
			byID[scopeID] = record
			continue
		}
		scope := &linuxScope{path: path, fd: os.NewFile(uintptr(fd), entry.Name())}
		members, memberErr := scope.Members()
		if memberErr == nil {
			count := len(members)
			record.Populated = &count
		} else {
			record.UnevaluatedFields = append(record.UnevaluatedFields, "populated")
		}
		if data, readErr := deps.readField(scope, "memory.current", 64); readErr == nil {
			if value, parseErr := parseConfineInt(data); parseErr == nil {
				record.RSSBytes = &value
			} else {
				record.UnevaluatedFields = append(record.UnevaluatedFields, "rss")
			}
		} else {
			record.UnevaluatedFields = append(record.UnevaluatedFields, "rss")
		}
		if data, readErr := deps.readField(scope, "memory.max", 64); readErr == nil {
			value := strings.TrimSpace(string(data))
			if value == "max" {
				record.Cap = &value
			} else if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && parsed >= 0 {
				value = strconv.FormatInt(parsed, 10)
				record.Cap = &value
			} else {
				record.UnevaluatedFields = append(record.UnevaluatedFields, "cap")
			}
		} else {
			record.UnevaluatedFields = append(record.UnevaluatedFields, "cap")
		}
		_ = scope.fd.Close()
		byID[scopeID] = record
	}
	mergeConfineRegistry(byID, registry)
	result := ConfineListResult{Verdict: "pass", Scopes: make([]ConfineRecord, 0, len(byID))}
	for _, record := range byID {
		result.Scopes = append(result.Scopes, record)
	}
	sort.Slice(result.Scopes, func(i, j int) bool { return result.Scopes[i].ScopeID < result.Scopes[j].ScopeID })
	return result, nil
}

func mergeConfineRegistry(byID map[string]ConfineRecord, registry []ConfineRegistryEntry) {
	seen := make(map[string]ConfineRegistryEntry)
	conflict := make(map[string]bool)
	for _, entry := range registry {
		if !validConfineScopeID(entry.ScopeID) {
			continue
		}
		if prior, exists := seen[entry.ScopeID]; exists && (prior.Owner != entry.Owner || prior.Name != entry.Name) {
			conflict[entry.ScopeID] = true
		}
		seen[entry.ScopeID] = entry
	}
	for scopeID, entry := range seen {
		record, exists := byID[scopeID]
		if !exists {
			name, pid, stamp, ok := parseConfineScopeID(scopeID)
			if !ok {
				continue
			}
			record = ConfineRecord{Name: name, Owner: ConfineUnknownOwner, ScopeID: scopeID, SupervisorPID: &pid, Pending: true, UnevaluatedFields: []string{"populated", "rss", "cap"}}
			age := time.Since(time.Unix(0, stamp))
			if age >= 0 {
				seconds := int64(age / time.Second)
				record.AgeSeconds = &seconds
			}
		}
		if !conflict[scopeID] && entry.Name == record.Name && ValidateConfineIdentity(entry.Owner) == nil && entry.Owner != ConfineUnknownOwner {
			record.Owner = entry.Owner
		} else {
			record.Owner = ConfineUnknownOwner
		}
		byID[scopeID] = record
	}
}

func parseConfineInt(data []byte) (int64, error) {
	text := strings.TrimSpace(string(data))
	if text == "" || len(strings.Fields(text)) != 1 {
		return 0, errors.New("not one integer")
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("invalid non-negative integer")
	}
	return value, nil
}

func validConfineScopeID(scopeID string) bool {
	_, _, _, ok := parseConfineScopeID(scopeID)
	return ok
}

func parseConfineScopeID(scopeID string) (string, int, int64, bool) {
	if !strings.HasPrefix(scopeID, "CONFINE-") || strings.Contains(scopeID, "/") {
		return "", 0, 0, false
	}
	rest := strings.TrimPrefix(scopeID, "CONFINE-")
	last := strings.LastIndexByte(rest, '-')
	if last <= 0 || last == len(rest)-1 {
		return "", 0, 0, false
	}
	stamp, err := strconv.ParseInt(rest[last+1:], 36, 64)
	if err != nil || stamp <= 0 {
		return "", 0, 0, false
	}
	rest = rest[:last]
	last = strings.LastIndexByte(rest, '-')
	if last <= 0 || last == len(rest)-1 {
		return "", 0, 0, false
	}
	name := rest[:last]
	pid64, err := strconv.ParseInt(rest[last+1:], 10, 32)
	if err != nil || pid64 <= 0 || ValidateConfineIdentity(name) != nil {
		return "", 0, 0, false
	}
	return name, int(pid64), stamp, true
}

func killConfine(ctx context.Context, slicePath, selector, callerOwner string, steal bool, registry []ConfineRegistryEntry, freshOwner ConfineOwnerLookup, timeout time.Duration) (ConfineKillResult, error) {
	return killConfineWithDeps(ctx, slicePath, selector, callerOwner, steal, registry, freshOwner, timeout, defaultConfineScanDeps())
}

func killConfineWithDeps(ctx context.Context, slicePath, selector, callerOwner string, steal bool, registry []ConfineRegistryEntry, freshOwner ConfineOwnerLookup, timeout time.Duration, deps confineScanDeps) (ConfineKillResult, error) {
	listed, err := listConfinesWithDeps(ctx, slicePath, registry, deps)
	if err != nil {
		return ConfineKillResult{}, err
	}
	if listed.Verdict == "unevaluated" {
		return ConfineKillResult{}, fmt.Errorf("%s: confine slice discovery is unevaluated: %s", CodeConfineKillUnconfirmed, listed.Reason)
	}
	selector = strings.TrimSpace(selector)
	var matches []ConfineRecord
	for _, record := range listed.Scopes {
		pid := ""
		if record.SupervisorPID != nil {
			pid = strconv.Itoa(*record.SupervisorPID)
		}
		if selector == record.ScopeID || selector == record.Name || selector == pid {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return ConfineKillResult{}, fmt.Errorf("%s: selector %q matched no confine scope", CodeConfineNotFound, selector)
	}
	if len(matches) > 1 {
		ids := make([]string, len(matches))
		for i := range matches {
			ids[i] = matches[i].ScopeID
		}
		sort.Strings(ids)
		return ConfineKillResult{}, fmt.Errorf("E_SELECTOR_AMBIGUOUS: selector %q matched %s", selector, strings.Join(ids, ", "))
	}
	record := matches[0]
	owner, known := ConfineUnknownOwner, false
	if freshOwner != nil {
		owner, known = freshOwner(record.ScopeID)
	}
	if !known || owner == "" || owner == ConfineUnknownOwner {
		owner = ConfineUnknownOwner
		known = false
	}
	if !steal && (!known || callerOwner == "" || callerOwner == ConfineUnknownOwner || owner != callerOwner) {
		return ConfineKillResult{}, fmt.Errorf("%s: scope=%s owner=%s caller=%s; pass --steal to override", CodeConfineOwnerUnverified, record.ScopeID, owner, callerOwner)
	}
	if !validConfineScopeID(record.ScopeID) {
		return ConfineKillResult{}, fmt.Errorf("%s: invalid resolved scope id", CodeConfineNotFound)
	}
	parentFD, err := unix.Open(slicePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			return ConfineKillResult{}, fmt.Errorf("%s: confine slice disappeared: %w", CodeConfineNotFound, err)
		}
		return ConfineKillResult{}, fmt.Errorf("%s: open slice: %w", CodeConfineKillUnconfirmed, err)
	}
	defer unix.Close(parentFD)
	childName := ".aira-" + record.ScopeID
	fd, err := unix.Openat(parentFD, childName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			return ConfineKillResult{}, fmt.Errorf("%s: scope %s is mid-launch or already gone; retry", CodeConfineNotFound, record.ScopeID)
		}
		return ConfineKillResult{}, fmt.Errorf("%s: open scope %s: %w", CodeConfineKillUnconfirmed, record.ScopeID, err)
	}
	scope := &linuxScope{path: filepath.Join(slicePath, childName), fd: os.NewFile(uintptr(fd), childName)}
	defer scope.fd.Close()
	members, err := scope.Members()
	if err != nil {
		return ConfineKillResult{}, fmt.Errorf("%s: scope %s population could not be established: %w", CodeConfineKillUnconfirmed, record.ScopeID, err)
	}
	if len(members) == 0 {
		return ConfineKillResult{}, fmt.Errorf("%s: scope %s has nothing to kill yet; retry", CodeConfineNotLaunched, record.ScopeID)
	}
	scope.events, err = scope.openFile("cgroup.events", unix.O_RDONLY)
	if err != nil {
		return ConfineKillResult{}, fmt.Errorf("%s: scope %s confirmation channel unavailable: %w", CodeConfineKillUnconfirmed, record.ScopeID, err)
	}
	defer scope.events.Close()
	if err := scope.Kill(); err != nil {
		return ConfineKillResult{}, fmt.Errorf("%s: cgroup.kill for %s: %w", CodeConfineKillUnconfirmed, record.ScopeID, err)
	}
	scope.removedMeansEmpty = true
	if err := deps.waitEmpty(ctx, scope, timeout); err != nil {
		return ConfineKillResult{}, fmt.Errorf("%s: scope %s empty state unconfirmed: %w", CodeConfineKillUnconfirmed, record.ScopeID, err)
	}
	return ConfineKillResult{Status: "killed", ScopeID: record.ScopeID, Name: record.Name, Owner: owner}, nil
}
