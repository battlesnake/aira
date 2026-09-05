package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
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
			result = append(result, runner.ConfineRegistryEntry{ScopeID: waiter.scopeID})
		}
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	return result
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
	if err := runner.ValidateConfineOwner(callerOwner); err != nil {
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
		_, maximum, _, ok, _ := readMemory(path)
		if ok {
			// ONE locked snapshot: granted totals and queued/freeze state must
			// describe the same instant, or the summary contradicts itself.
			//
			// AIRA-24: a caller that is ITSELF queued names its own scope id and
			// gets its position out of that same pass. `aira confine --list`
			// never passes one (buildRequest does not accept the option), so this
			// is the blocked launcher's own progress-line probe and nothing else.
			snapshot := s.admitSliceSnapshotFor(path, stringArg(request.Args, "scope_id"))
			outstanding, adopted := snapshot.outstanding, snapshot.adopted
			totalJobs := addJobCountClamp(snapshot.outstandingJobs, snapshot.adoptedJobs)
			queued, freezePhase := snapshot.queued, snapshot.phase
			result.SliceReserve = &runner.ConfineSliceReserve{
				GrantedBytes: addClamp(outstanding, adopted),
				// Ceiling is what one MORE job would face; scale headroom by the
				// TOTAL admitted jobs (outstanding + adopted) so it stays consistent
				// with the Jobs shown, not just the connection-held ones.
				CeilingBytes: subtractFloor(maximum, s.admitSliceHeadroom(addJobCountClamp(totalJobs, 1))),
				Jobs:         totalJobs,
				Queued:       queued,
				FreezePhase:  freezePhase,
				// Zero unless the caller named a scope id that is queued right
				// now: absence of a position, never "position zero".
				QueuePosition:    snapshot.queuePosition,
				QueuedAheadBytes: snapshot.queuedAheadBytes,
				// AIRA-68: the same snapshot's population split, so the summary can
				// never again be read against the Scopes table above it as though
				// they counted the same thing.
				ScopeJobs:        snapshot.scopeJobs,
				ScopeBytes:       snapshot.scopeBytes,
				ReservationJobs:  snapshot.reservationJobs,
				ReservationBytes: snapshot.reservationBytes,
				AdoptedJobs:      snapshot.adoptedJobs,
				AdoptedBytes:     snapshot.adopted,
				VanishedJobs:     snapshot.vanishedJobs,
				VanishedBytes:    snapshot.vanishedBytes,
				ResidualJobs:     snapshot.residualJobs(),
				ResidualBytes:    snapshot.residualBytes(),
			}
		}
		return core.Response{OK: true, Code: "OK", Data: result}
	case "confine-kill":
		selector := stringArg(request.Args, "selector")
		steal, _ := request.Args["steal"].(bool)
		result, killErr := runner.KillConfine(ctx, path, selector, callerOwner, steal, registry)
		if killErr != nil {
			return confineManagementError(killErr)
		}
		// AIRA-70 finding #2: a cross-session `aira confine --kill` used to kill
		// a job with no record anywhere of who did it -- this file had no log
		// call at all. Reached only past KillConfine's error return, so it is
		// written only for a kill the daemon confirmed: a refused (owner
		// unverified), not-launched, or unconfirmed kill kills nothing and must
		// not leave a line claiming otherwise.
		log.Printf("aira daemon: confine-kill: killer=%s steal=%v target-scope=%s target-name=%s target-owner=%s slice=%s",
			callerOwner, steal, result.ScopeID, result.Name, result.Owner, path)
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
