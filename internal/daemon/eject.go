package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/gitcontext"
	"aira/internal/store"

	"golang.org/x/sys/unix"
)

func (s *Server) eject(ctx context.Context, args map[string]any) core.Response {
	projectSelector, _ := args["project"].(string)
	prefix, _ := args["prefix"].(string)
	force, _ := args["force"].(bool)
	purge, _ := args["purge"].(bool)
	target, err := s.db.ResolveProject(ctx, strings.TrimSpace(projectSelector), strings.TrimSpace(prefix))
	if err != nil {
		return lifecycleError(err)
	}
	if err := s.beginEject(target); err != nil {
		return lifecycleError(err)
	}
	defer s.endEject(target.ProjectID)
	// Resolve again after the exclusion has drained every in-flight project use.
	// A worktree may have registered between the selector lookup and beginEject;
	// durability must cover that final, stable registration set.
	target, err = s.db.ProjectRegistration(ctx, target.ProjectID)
	if err != nil {
		return lifecycleError(err)
	}

	holders, err := s.db.EjectLiveHolders(ctx, target.ProjectID)
	if err != nil {
		return lifecycleError(err)
	}
	if len(holders) > 0 && !force {
		return lifecycleError(fmt.Errorf("E_EJECT_LIVE_STATE: %s", strings.Join(holders, "; ")))
	}

	roots, gone, err := s.verifyEjectDurability(ctx, target, purge)
	if err != nil {
		return lifecycleError(err)
	}
	if gone && !force {
		return lifecycleError(errors.New("E_EJECT_UNVERIFIED: every registered worktree root is gone; --force is required"))
	}
	if purge {
		for _, root := range roots {
			if err := purgeDirty(root); err != nil {
				if !force || store.ErrorCode(err) != "E_PURGE_DIRTY" {
					return lifecycleError(err)
				}
			}
		}
	}

	if s.beforeEjectTransaction != nil {
		s.beforeEjectTransaction()
	}
	if purge && !force {
		for _, root := range roots {
			if err := purgeDirty(root); err != nil {
				return lifecycleError(err)
			}
		}
	}
	result, err := s.db.Eject(ctx, target, force)
	if err != nil {
		return lifecycleError(err)
	}
	if err := store.TrimRegistryProject(s.Paths.RegistryPath, target.ProjectID); err != nil {
		return lifecycleError(fmt.Errorf("E_EJECT_UNVERIFIED: project ejected but registry trim was not verified: %w", err))
	}
	if purge {
		for _, root := range roots {
			if err := purgeAIRA(root); err != nil {
				return lifecycleError(fmt.Errorf("E_EJECT_UNVERIFIED: project ejected but purge was not verified: %w", err))
			}
		}
		result.Files = "purged"
	}
	return core.Response{OK: true, Code: "OK", Data: result}
}

func lifecycleError(err error) core.Response {
	code := store.ErrorCode(err)
	return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
}

func (s *Server) beginEject(target store.ProjectRegistration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ejecting == nil {
		s.ejecting = make(map[string]struct{})
	}
	if _, exists := s.ejecting[target.ProjectID]; exists {
		return fmt.Errorf("E_DAEMON_BUSY: project %s already has a lifecycle operation", target.ProjectID)
	}
	s.ejecting[target.ProjectID] = struct{}{}
	for key, entry := range s.scopes {
		if entry.view.ProjectID() == target.ProjectID {
			delete(s.scopes, key)
			delete(s.coveredWorktrees, entry.view.WorktreeID())
			delete(s.discoveryFailed, entry.view.WorktreeID())
		}
	}
	for _, worktree := range target.Worktrees {
		delete(s.coveredWorktrees, worktree.WorktreeID)
		delete(s.discoveryFailed, worktree.WorktreeID)
	}
	if s.projectCond == nil {
		s.projectCond = sync.NewCond(&s.mu)
	}
	for s.projectUses[target.ProjectID] != 0 {
		s.projectCond.Wait()
	}
	return nil
}

func (s *Server) endEject(projectID string) {
	s.mu.Lock()
	delete(s.ejecting, projectID)
	s.mu.Unlock()
}

func (s *Server) beginProjectUse(projectID string) (func(), error) {
	s.mu.Lock()
	if _, blocked := s.ejecting[projectID]; blocked {
		s.mu.Unlock()
		return nil, fmt.Errorf("E_NOT_ADOPTED: project %s is being ejected", projectID)
	}
	if s.projectUses == nil {
		s.projectUses = make(map[string]int)
	}
	s.projectUses[projectID]++
	s.mu.Unlock()
	return func() {
		s.endProjectUse(projectID)
	}, nil
}

func (s *Server) endProjectUse(projectID string) {
	s.mu.Lock()
	s.projectUses[projectID]--
	if s.projectUses[projectID] == 0 {
		delete(s.projectUses, projectID)
		if s.projectCond != nil {
			s.projectCond.Broadcast()
		}
	}
	s.mu.Unlock()
}

func (s *Server) verifyEjectDurability(ctx context.Context, target store.ProjectRegistration, skipDurabilityCheck bool) ([]string, bool, error) {
	roots := make([]string, 0, len(target.Worktrees))
	missing := 0
	for _, worktree := range target.Worktrees {
		root := filepath.Clean(worktree.Root)
		info, err := os.Stat(root)
		if errors.Is(err, os.ErrNotExist) {
			missing++
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: cannot stat worktree %s: %w", root, err)
		}
		if !info.IsDir() {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: worktree root is not a directory: %s", root)
		}
		roots = append(roots, root)
	}
	if len(target.Worktrees) == 0 || missing == len(target.Worktrees) {
		return roots, true, nil
	}
	if missing != 0 {
		return nil, false, errors.New("E_EJECT_UNVERIFIED: only some registered worktree roots are available")
	}
	for _, root := range roots {
		project, err := app.Discover(ctx, root)
		if err != nil {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: discover %s: %w", root, err)
		}
		if project.ProjectID != target.ProjectID {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: worktree %s resolves to project %s", root, project.ProjectID)
		}
		if skipDurabilityCheck {
			continue
		}
		scope, err := ScopeFromProject(project, s.Paths)
		if err != nil {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: scope %s: %w", root, err)
		}
		view, err := s.unregisteredScope(scope)
		if err != nil {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: open %s: %w", root, err)
		}
		if err := view.Reconcile(ctx); err != nil {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: drain %s: %w", root, err)
		}
		report, err := view.Check(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: check %s: %w", root, err)
		}
		if finding := ejectDurabilityFinding(report); finding != "" {
			return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: check %s: %s", root, finding)
		}
	}
	sort.Strings(roots)
	return roots, false, nil
}

func (s *Server) unregisteredScope(scope WorktreeScope) (*store.Store, error) {
	scope.ReviewPolicy.Configured = scope.ReviewConfigured
	return store.NewUnregisteredScope(s.db, store.ScopeOptions{
		Root: scope.Root, CommonDir: scope.CommonDir, GitDir: scope.GitDir,
		ProjectID: scope.ProjectID, WorktreeID: scope.WorktreeID, ProjectSlug: scope.Slug,
		Prefixes: scope.Prefixes, RequirementPrefixes: scope.RequirementPrefixes,
		ReviewPolicy: scope.ReviewPolicy, LeaseStateDir: filepath.Join(s.Paths.LeaseStateDir, scope.WorktreeID), LeaseTTLNS: scope.LeaseTTLNS,
		MaxReports: scope.MaxReports, MaxAgeDays: scope.MaxAgeDays,
		MaxComputeEvents: scope.MaxComputeEvents, MaxComputeAgeDays: scope.MaxComputeAgeDays,
		MaxCommandEvents: scope.MaxCommandEvents, MaxCommandAgeDays: scope.MaxCommandAgeDays,
		MaxQuotaSnapshots: scope.MaxQuotaSnapshots, ConfigDigest: scope.ConfigDigest,
	})
}

func ejectDurabilityFinding(report store.CheckReport) string {
	for _, dimension := range []string{
		"allocated-id-file", "duplicate-id", "stale-index", "ticket-file-integrity",
		"reconcile-integrity", "rebuild-integrity", "relation-integrity", "finding-integrity",
	} {
		if status := report.Dimensions[dimension]; status != "" && status != "pass" {
			return dimension + ": " + status
		}
	}
	for _, finding := range append(append([]store.CheckFinding(nil), report.Findings...), report.Warnings...) {
		code := strings.ToUpper(finding.Code)
		if strings.Contains(code, "INDEX") || strings.Contains(code, "DIVERGENCE") || strings.Contains(code, "WRITE_CONFLICT") || strings.Contains(code, "ID_UNRESOLVED") {
			return finding.Code + ": " + finding.Message
		}
	}
	for _, finding := range report.UnevaluatedFindings {
		if strings.Contains(strings.ToUpper(finding.Code), "INDEX") {
			return finding.Code + ": " + finding.Message
		}
	}
	return ""
}

func purgeDirty(root string) error {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("E_EJECT_UNVERIFIED: stat purge root: %w", err)
	}
	command := exec.Command("git", "-C", root, "status", "--porcelain", "--ignored", "--", ".aira")
	command.Env = gitcontext.ScrubbedEnvironment()
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("E_EJECT_UNVERIFIED: git status .aira: %w", err)
	}
	if len(output) != 0 {
		return errors.New("E_PURGE_DIRTY: .aira has untracked, staged, or unstaged changes; commit them or use --force")
	}
	return nil
}

func purgeAIRA(root string) error {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	return unlinkTreeAt(rootFD, ".aira")
}

func unlinkTreeAt(parentFD int, name string) error {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	names, readErr := dir.Readdirnames(-1)
	if readErr != nil {
		_ = dir.Close()
		return readErr
	}
	for _, child := range names {
		if child == "." || child == ".." || strings.Contains(child, "/") {
			_ = dir.Close()
			return fmt.Errorf("E_PURGE_DIRTY: invalid directory entry %q", child)
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, child, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = dir.Close()
			return err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := unlinkTreeAt(fd, child); err != nil {
				_ = dir.Close()
				return err
			}
		} else if err := unix.Unlinkat(fd, child, 0); err != nil {
			_ = dir.Close()
			return err
		}
	}
	if err := dir.Close(); err != nil {
		return err
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}
