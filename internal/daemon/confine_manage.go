package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

func (s *Server) activeConfines(path string) []runner.ConfineRegistryEntry {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return []runner.ConfineRegistryEntry{}
	}
	queue.mu.Lock()
	result := make([]runner.ConfineRegistryEntry, 0)
	for _, waiter := range queue.waiters {
		if waiter != nil && waiter.state == admitGranted && waiter.scopeID != "" {
			result = append(result, runner.ConfineRegistryEntry{ScopeID: waiter.scopeID, Name: waiter.name, Owner: waiter.owner})
		}
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	return result
}

func (s *Server) freshConfineOwner(path, scopeID string) (string, bool) {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return runner.ConfineUnknownOwner, false
	}
	queue.mu.Lock()
	owner := ""
	known := false
	for _, waiter := range queue.waiters {
		if waiter == nil || waiter.state != admitGranted || waiter.scopeID != scopeID {
			continue
		}
		if !known {
			owner, known = waiter.owner, true
		} else if owner != waiter.owner {
			owner, known = runner.ConfineUnknownOwner, false
			break
		}
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	if !known || owner == "" || owner == runner.ConfineUnknownOwner {
		return runner.ConfineUnknownOwner, false
	}
	return owner, true
}

func (s *Server) releaseActiveConfine(path, scopeID string) {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return
	}
	queue.mu.Lock()
	var waiter *admitWaiter
	for _, candidate := range queue.waiters {
		if candidate != nil && candidate.state == admitGranted && candidate.scopeID == scopeID {
			waiter = candidate
			break
		}
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	if waiter != nil {
		s.releaseAdmitWaiter(queue, waiter)
	}
}

func (s *Server) resolveConfineManagementPath(slice string) (string, error) {
	slice = strings.TrimSpace(slice)
	if slice == "" {
		_, path, err := runner.ResolveConfineManagementSlice("")
		return path, err
	}
	resolve := s.admitResolveSlice
	if resolve == nil {
		resolve = resolveAdmitSlicePath
	}
	path, ok, reason := resolve(slice)
	if !ok {
		if reason == "" {
			reason = "slice-not-found"
		}
		return "", fmt.Errorf("E_CONFINE_UNAVAILABLE: slice %s: %s", slice, reason)
	}
	return path, nil
}

func (s *Server) confineManagement(ctx context.Context, request core.Request) core.Response {
	callerOwner := stringArg(request.Args, "owner")
	if err := runner.ValidateConfineIdentity(callerOwner); err != nil {
		return confineManagementError(fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: owner: %w", err))
	}
	path, err := s.resolveConfineManagementPath(stringArg(request.Args, "slice"))
	if err != nil {
		return confineManagementError(err)
	}
	registry := s.activeConfines(path)
	switch core.CanonicalVerb(request.Verb) {
	case "confine-list":
		result, listErr := runner.ListConfines(ctx, path, registry)
		if listErr != nil {
			return confineManagementError(listErr)
		}
		if result.Verdict == "unevaluated" {
			return core.Response{OK: true, Code: "UNEVALUATED", Data: result, Exit: 3}
		}
		readMemory := s.admitReadMemory
		if readMemory == nil {
			readMemory = readSliceMemory
		}
		_, maximum, ok, _ := readMemory(path)
		if ok {
			outstanding, outstandingJobs, adopted, adoptedJobs, _ := s.admitOutstandingReserve(path)
			totalJobs := addJobCountClamp(outstandingJobs, adoptedJobs)
			result.SliceReserve = &runner.ConfineSliceReserve{
				GrantedBytes: addClamp(outstanding, adopted),
				// Ceiling is what one MORE job would face; scale headroom by the
				// TOTAL admitted jobs (outstanding + adopted) so it stays consistent
				// with the Jobs shown, not just the connection-held ones.
				CeilingBytes: subtractFloor(maximum, s.admitSliceHeadroom(addJobCountClamp(totalJobs, 1))),
				Jobs:         totalJobs,
			}
		}
		return core.Response{OK: true, Code: "OK", Data: result}
	case "confine-kill":
		selector := stringArg(request.Args, "selector")
		steal, _ := request.Args["steal"].(bool)
		result, killErr := runner.KillConfine(ctx, path, selector, callerOwner, steal, registry, func(scopeID string) (string, bool) {
			return s.freshConfineOwner(path, scopeID)
		})
		if killErr != nil {
			return confineManagementError(killErr)
		}
		// This is the daemon kill-drop release. The admit connection's later
		// close reaches the same state-guarded releaseAdmitWaiter path.
		s.releaseActiveConfine(path, result.ScopeID)
		return core.Response{OK: true, Code: "OK", Data: result}
	default:
		return confineManagementError(errors.New("E_UNKNOWN_VERB: unknown confine management verb"))
	}
}

func stringArg(args map[string]any, name string) string {
	value, _ := args[name].(string)
	return strings.TrimSpace(value)
}

func confineManagementError(err error) core.Response {
	code := store.ErrorCode(err)
	return core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
}
