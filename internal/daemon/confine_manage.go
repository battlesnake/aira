package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// confineReservationRows turns the snapshot's copied-out reservation values into
// the wire rows, LONGEST-HELD FIRST and capped.
//
// The ordering is the whole point (AIRA-108): the row an operator needs is the
// oldest hold, so a cap that dropped it would defeat the field. Ties break on
// the larger reserve, then on signature, so the order is total and the output is
// deterministic — a list that reshuffles between two runs of `confine --list`
// reads as churn that is not happening.
//
// The signature is NOT escaped here. It travels the wire as the client sent it,
// because a JSON consumer wants the real value; escaping is the RENDERER's job,
// at the one boundary that actually reaches a terminal.
func confineReservationRows(rows []admitReservationRow) []runner.ConfineReservationHold {
	if len(rows) == 0 {
		return nil
	}
	sorted := make([]admitReservationRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].heldMS != sorted[j].heldMS {
			return sorted[i].heldMS > sorted[j].heldMS
		}
		if sorted[i].reserve != sorted[j].reserve {
			return sorted[i].reserve > sorted[j].reserve
		}
		return sorted[i].signature < sorted[j].signature
	})
	if len(sorted) > runner.ConfineReservationRowLimit {
		sorted = sorted[:runner.ConfineReservationRowLimit]
	}
	out := make([]runner.ConfineReservationHold, 0, len(sorted))
	for _, row := range sorted {
		out = append(out, runner.ConfineReservationHold{
			// Every row emitted here is a granted, accounted, scope-less waiter —
			// admitSliceSnapshotFor's own guard — so the state is a fact about the
			// waiter it was copied from, not a default.
			State: runner.ConfineReservationStateHolding,
			// A reservation may legitimately carry no signature (`signature` is
			// optional on the admit wire). Left empty and rendered as an explicit
			// "(unnamed)", never invented.
			Signature: row.signature, Reserve: row.reserve, HeldMS: row.heldMS,
		})
	}
	return out
}

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
	// AIRA-121 gate condition C2. In shim mode BOTH branches below are wrong:
	// ResolveConfineManagementSlice resolves the REAL aira.slice cgroup (it does
	// not go through the admitResolveSlice seam at all), so `--list` would key its
	// registry lookup and its reserve summary on a queue nothing writes to. The
	// sentinel is the one queue there is.
	if s.shimMode() {
		return runner.ShimConfineSlice, nil
	}
	if slice == "" {
		_, path, err := runner.ResolveConfineManagementSlice("")
		return path, err
	}
	resolve := s.sliceResolver()
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
		// AIRA-121 gate condition C2. Shim mode renders its rows from the daemon's
		// own granted-waiter registry and NEVER calls ListConfines: that function
		// os.ReadDir's the slice cgroup directory, so against the sentinel every
		// `--list` would answer UNEVALUATED / E_CONFINE_UNAVAILABLE -- an operator
		// surface that never works at all.
		var result runner.ConfineListResult
		if s.shimMode() {
			result = runner.ShimConfineList(registry)
		} else {
			listed, listErr := runner.ListConfines(ctx, path, registry)
			if listErr != nil {
				return confineManagementError(listErr)
			}
			result = listed
		}
		if result.Verdict == "unevaluated" {
			return core.Response{OK: true, Code: "UNEVALUATED", Data: result, Exit: 3}
		}
		readMemory := s.memoryReader()
		sliceCurrent, maximum, sliceReclaimable, ok, _ := readMemory(path)
		if ok {
			// AIRA-103. A CAPACITY question -- "what would one more job face" --
			// so it takes the pressure-throttled maximum, exactly like the
			// evaluator's own check. Reporting the static value here would tell an
			// operator a job fits when the daemon will not admit it.
			//
			// ONE ceiling snapshot for the whole reply. Taking the effective
			// maximum and the ceiling state in two separate locked reads let a
			// publication land between them, so CeilingBytes could describe the
			// old ceiling while the mode/state/MemAvailable beside it described
			// the new one -- the same self-inconsistency admitSliceSnapshot exists
			// to prevent.
			ceiling := s.sliceCeilingSnapshotFor(path)
			ceilingMaximum := sliceCeilingEffectiveMaximum(ceiling, maximum)
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
				CeilingBytes: subtractFloor(ceilingMaximum, s.admitSliceHeadroom(addJobCountClamp(totalJobs, 1))),
				// AIRA-103. What the ceiling WOULD be if applied. In enforce mode
				// it equals CeilingBytes; in OBSERVE mode CeilingBytes is the
				// untouched static capacity (observe applies nothing), so
				// rendering that as the observed decision would leave the
				// prescribed observe-then-enforce rollout blind on its own
				// operator surface. ZERO when the subsystem is off -- an ABSENCE,
				// because "off adds nothing to the wire" is the claim this ships
				// on, pinned by TestConfineListSliceReserveSummary.
				CeilingWouldBeBytes: sliceCeilingWouldBeBytes(ceiling, ceilingMaximum,
					s.admitSliceHeadroom(addJobCountClamp(totalJobs, 1))),
				Jobs:        totalJobs,
				Queued:      queued,
				FreezePhase: freezePhase,
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
				// AIRA-108. The same population, NAMED rather than only counted.
				// Sorted and capped OUTSIDE the queue lock — the snapshot already
				// copied the values out under it.
				Reservations:  confineReservationRows(snapshot.reservations),
				AdoptedJobs:   snapshot.adoptedJobs,
				AdoptedBytes:  snapshot.adopted,
				VanishedJobs:  snapshot.vanishedJobs,
				VanishedBytes: snapshot.vanishedBytes,
				ResidualJobs:  snapshot.residualJobs(),
				ResidualBytes: snapshot.residualBytes(),
				// AIRA-103. Absent (all zero/empty) when the subsystem is off, so
				// the renderer prints nothing rather than a fabricated state.
				CeilingMode:        string(ceiling.Mode),
				CeilingState:       ceiling.State,
				CeilingBasis:       ceiling.Basis,
				CeilingReason:      ceiling.Reason,
				CeilingHeld:        ceiling.Held,
				CeilingStaticBytes: ceiling.StaticMax,
				MemAvailableBytes:  ceiling.MemAvailable,
				// AIRA-114. From the SAME snapshot and the SAME ceiling reading the
				// evaluator's own gate uses, so this line can never describe a
				// different instant or a different slice size than the decision it
				// explains.
				CapAggregateBytes: snapshot.capAggregate,
				CapAggregateKnown: snapshot.capAggregateKnown,
				CapBoundBytes:     s.oversubscriptionLimit(ceilingMaximum),
			}
			// AIRA-127. The system-and-slice frame `aira top` draws its RAM bar
			// in, from the SAME reading whose `ok` gates this whole struct plus
			// one memory.high read and the same /proc/meminfo pair the ceiling
			// subsystem samples. Nothing here feeds an admission decision.
			//
			// Withheld ENTIRELY in shim mode: the host's /proc/meminfo is not
			// namespaced to the container (readShimMemory documents the same
			// trap), so publishing host MemTotal beside an advisory container
			// budget would present a frame nobody measured. Its absence is what
			// makes the renderer say "unevaluated" instead of drawing a bar.
			if !s.shimMode() {
				sliceHigh, sliceHighState := s.memoryHighReader()(path)
				memTotal, memTotalOK := s.memTotalReader()()
				memAvailable, memAvailableOK, _ := s.memAvailableReader()()
				result.SliceReserve.SliceCurrentBytes = sliceCurrent
				result.SliceReserve.SliceReclaimableBytes = sliceReclaimable
				result.SliceReserve.SliceMaxBytes = maximum
				result.SliceReserve.SliceHighBytes = sliceHigh
				result.SliceReserve.SliceHighState = sliceHighState
				result.SliceReserve.CeilingEffectiveBytes = sliceCeilingReportedEffective(ceiling, ceilingMaximum)
				// Each meminfo figure is published only if IT was established:
				// a failed MemAvailable read must not also erase a MemTotal the
				// same pass did establish, and a zero here is an absence the
				// renderer refuses to draw rather than a machine with no RAM.
				if memTotalOK {
					result.SliceReserve.SystemMemTotalBytes = memTotal
				}
				if memAvailableOK {
					result.SliceReserve.SystemMemAvailableBytes = memAvailable
				}
			}
			// AIRA-121. The advisory wording travels on the SAME line as the
			// numbers it qualifies. Without it a shim reserve summary is
			// byte-identical to a real slice's and presents an advisory budget as
			// an enforced ceiling.
			if s.shimMode() {
				result.SliceReserve.Containment = string(runner.ConfineContainmentAdvisory)
				result.SliceReserve.BudgetSource = s.shimBudget.Source
			}
			// AIRA-101, from the SAME snapshot, so the exclusive holder and the
			// counts above can never describe different instants. Left nil when
			// nothing is exclusive: a positive "none", never an unevaluated reading.
			if snapshot.exclusiveState != "" {
				result.SliceReserve.Exclusive = &runner.ConfineExclusiveState{
					State:       snapshot.exclusiveState,
					Name:        snapshot.exclusiveName,
					Owner:       snapshot.exclusiveOwner,
					ScopeID:     snapshot.exclusiveScopeID,
					WaitingJobs: snapshot.exclusiveWaiting,
					// AIRA-119, from that same pass: the age belongs to the identity
					// beside it, never to a later reading of a state that may have
					// changed hands in between.
					SinceMS: snapshot.exclusiveSinceMS,
				}
			}
		}
		return core.Response{OK: true, Code: "OK", Data: result}
	case "confine-kill":
		selector := stringArg(request.Args, "selector")
		steal, _ := request.Args["steal"].(bool)
		if s.shimMode() {
			// Never a fabricated "killed". cgroup.kill is the only mechanism that
			// reaches a job's whole subtree atomically, and shim mode has no cgroup
			// at all, so the honest answer is a refusal that names the supervisor PID
			// to signal -- that supervisor forwards to the job's process GROUP.
			_, killErr := runner.ShimConfineKill(selector, callerOwner, steal, registry)
			return confineManagementError(killErr)
		}
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
	return core.Response{Code: code, Error: err.Error(), Exit: codes.ExitForCode(code)}
}
