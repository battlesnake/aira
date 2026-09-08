package daemon

import (
	"context"
	"fmt"
	"strings"

	"aira/internal/core"
	"aira/internal/store"
)

// AIRA-179: `aira rant [--project ID | --prefix P] ...` names the project a
// rant operation applies to, so friction about a shared tool can be filed
// where that tool's own maintainers read it rather than only in whichever
// project the session happened to be standing in.
//
// The daemon is the only place this can be resolved: it owns the machine-wide
// registration table and already reconstructs a full store scope for any
// registered project from disk. So the whole feature is one target resolution
// at the seam where the dispatcher's store is chosen — numbering, event
// sequencing, the common-directory journal and the search index then land in
// the target project because the store IS that project's store.

// rantTarget reports the explicit target selector on a rant request. Neither
// value is interpreted here; both are handed to the one machine-wide resolver,
// which owns the refusals (E_NOT_ADOPTED, E_SELECTOR_AMBIGUOUS, E_NO_PROJECT).
func rantTarget(request core.Request) (projectSelector, prefix string, targeted bool, err error) {
	if core.CanonicalVerb(request.Verb) != "rant" {
		return "", "", false, nil
	}
	if projectSelector, err = rantSelectorValue(request.Args, "project"); err != nil {
		return "", "", false, err
	}
	if prefix, err = rantSelectorValue(request.Args, "prefix"); err != nil {
		return "", "", false, err
	}
	return projectSelector, prefix, projectSelector != "" || prefix != "", nil
}

// rantSelectorValue reads one half of the selector fail-closed. The usual
// `args[name].(string)` idiom collapses an unreadable value to "", which for
// every other argument means "absent" — but here "absent" means "file it in the
// caller's own project", so a selector the daemon cannot read would silently
// write the rant to a DIFFERENT project than the one that was named. A value
// that cannot be established is refused, never treated as no selector at all.
func rantSelectorValue(args map[string]any, name string) (string, error) {
	raw, present := args[name]
	if !present || raw == nil {
		return "", nil
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("E_RANT_INVALID: rant --%s must be a string, got %T", name, raw)
	}
	return strings.TrimSpace(text), nil
}

// withoutRantTarget returns the request the resolved dispatcher runs, with the
// target selector removed. The core refuses a selector by name precisely
// because no core can resolve one; once the daemon HAS resolved it, carrying it
// further would trip that refusal. The original request object is left intact
// for observers that already saw it.
func withoutRantTarget(request core.Request) core.Request {
	args := make(map[string]any, len(request.Args))
	for name, value := range request.Args {
		if name == "project" || name == "prefix" {
			continue
		}
		args[name] = value
	}
	request.Args = args
	return request
}

// coreForRequest builds the dispatcher one request runs against, and the
// request it runs. Everything but an explicitly targeted rant runs against the
// caller's own scope, exactly as before. A targeted rant runs against the
// TARGET project's store, carrying the caller's own daemon-validated scope
// through as the rant origin so provenance is cross-checked against where the
// caller actually stood and the foreign write is attributable.
//
// The returned release, when non-nil, must be called once the response has been
// produced: it holds the target project's use reference, which is what stops an
// eject of the target from committing underneath an in-flight cross-project
// write.
func (s *Server) coreForRequest(ctx context.Context, scope WorktreeScope, request core.Request) (*core.Core, core.Request, func(), error) {
	projectSelector, prefix, targeted, err := rantTarget(request)
	if err != nil {
		return nil, request, nil, err
	}
	if !targeted {
		dispatcher, err := s.coreForScope(scope)
		return dispatcher, request, nil, err
	}
	// The caller's scope is validated first and for its own sake: its canonical
	// identity is what gets recorded as the origin, so an unusable caller scope
	// must refuse rather than produce an unattributable foreign write.
	callerView, _, err := s.storeForScope(scope)
	if err != nil {
		return nil, request, nil, err
	}
	target, err := s.db.ResolveProject(ctx, projectSelector, prefix)
	if err != nil {
		return nil, request, nil, err
	}
	resolved := withoutRantTarget(request)
	if target.ProjectID == callerView.ProjectID() {
		// Naming your own project is an ordinary local rant: same store, no
		// origin, nothing recorded as foreign. Selectors are a way to say WHERE,
		// not a second kind of rant.
		return core.New(callerView), resolved, nil, nil
	}
	release, err := s.beginProjectUse(target.ProjectID)
	if err != nil {
		return nil, request, nil, err
	}
	targetView, err := s.targetProjectView(ctx, target)
	if err != nil {
		release()
		return nil, request, nil, err
	}
	redirected, err := targetView.WithRantOrigin(callerView.RantOrigin())
	if err != nil {
		release()
		return nil, request, nil, err
	}
	return core.New(redirected), resolved, release, nil
}

// targetProjectView builds the store view for a resolved target project from
// its own registered worktrees on disk, through the same discovery and scope
// construction the registry pass uses. Every candidate must still prove its
// identity, so a stale registration cannot silently redirect a rant into a
// different project than the selector named.
func (s *Server) targetProjectView(ctx context.Context, target store.ProjectRegistration) (*store.Store, error) {
	var lastErr error
	for _, worktree := range target.Worktrees {
		project, err := s.discoverProjectAt(ctx, worktree.Root)
		if err != nil {
			lastErr = err
			continue
		}
		if project.ProjectID != target.ProjectID || project.WorktreeID != worktree.WorktreeID {
			lastErr = fmt.Errorf("registered worktree %s now resolves to project %s worktree %s", worktree.Root, project.ProjectID, project.WorktreeID)
			continue
		}
		scope, err := ScopeFromProject(project, s.Paths)
		if err != nil {
			lastErr = err
			continue
		}
		view, _, err := s.storeForScope(scope)
		if err != nil {
			lastErr = err
			continue
		}
		return view, nil
	}
	// Refusing by name, never a fabricated success or a quiet fall-back to the
	// caller's own project: the target was named and could not be served.
	if lastErr == nil {
		return nil, fmt.Errorf("E_NOT_ADOPTED: project %s has no registered worktree to serve a rant", target.ProjectID)
	}
	return nil, fmt.Errorf("E_NOT_ADOPTED: project %s has no reachable registered worktree: %v", target.ProjectID, lastErr)
}
