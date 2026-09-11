package daemon

import (
	"context"
	"fmt"
	"sort"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// AIRA admission-counter rebuild, S18: the daemon-side half of
// `aira confine --dump <file>` (design §12, "Data collection & CI dump").
//
// This is a SEPARATE, new, CI/archival feature. S13 deleted the OLD binary
// restart-lease dump (dump.go / reload.go's now-gone reload layer, the
// ARDR-era design §4 mechanism) entirely; confine-dump shares no file, no
// wire format, and no code with it. Confirmed absent: there is no
// internal/daemon/dump.go in this tree.
//
// READ-ONLY over the store and the live admission registry. It touches no
// admission decision path: admit.go is not modified by this file, and every
// helper reused below (checkedAvailable, ledgerAvailable, cpuAvailable,
// elapsedMilliseconds, admitFreezePhaseAt) is a pure read/derive function,
// never the grant/release/evaluate loop. No network: the daemon returns
// structured data over its existing local transport; the CLI face
// (cmd/aira/confine_dump.go) performs the actual file write.

// confineDump answers the `confine-dump` wire verb: design §12's retained
// populations, reshaped for archival.
//
//   - Admissions: every retained confine_peak_history sample (the SAME
//     table/reader AIRA-180's confine-budget already classifies) --
//     "declared reserve vs observed peak". No new capture machinery.
//   - Waiters: every admission still live in this daemon process's
//     in-memory queue at dump time (queued or granted), carrying the real
//     wait/outcome terms §12 asks for and the persisted Admissions rows
//     structurally cannot -- see ConfineDumpAdmissionRow's doc comment.
//   - Queues: one row per slice this daemon currently holds an admission
//     queue for, giving oldest-blocked wait, a negative-available excursion
//     (the signed ledger going negative during the §4 restart window), and
//     per-resource (ram/cpu) utilisation.
//
// Waiters and Queues are read together, in ONE locked walk per queue
// (buildConfineDumpQueuesAndWaiters), that consults exactly the fields
// admitSliceSnapshotFor and confine_manage.go's own listing already read --
// outstanding/cpuOutstanding/outstandingJobs, queue.waiters, the freeze
// state, and the slice's live memory.max.
func (s *Server) confineDump(args map[string]any) core.Response {
	callerOwner := stringArg(args, "owner")
	if err := runner.ValidateConfineOwner(callerOwner); err != nil {
		return confineManagementError(fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: owner: %w", err))
	}
	if s.db == nil {
		return core.Response{Code: CodeUnavailable, Error: CodeUnavailable + ": state database is unavailable"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
	defer cancel()
	subjects, err := s.db.ResourceBudgetSubjects(ctx)
	if err != nil {
		return core.Response{Code: CodeInternal, Error: CodeInternal + ": read usage history: " + err.Error()}
	}
	queues, waiters := s.buildConfineDumpQueuesAndWaiters()
	result := runner.ConfineDumpResult{
		Verdict:    "ok",
		Scope:      store.ResourceBudgetUniverseScope(),
		Admissions: buildConfineDumpAdmissions(subjects),
		Waiters:    waiters,
		Queues:     queues,
	}
	return core.Response{OK: true, Code: "OK", Data: result}
}

// buildConfineDumpAdmissions is PURE: it reads no daemon state and writes
// nothing. One record per persisted sample (not just the newest per
// subject), since an archival dump's whole point is the retained history,
// not a live snapshot of it.
//
// Outcome/WaitMS are always the honest unevaluated sentinel/nil here -- see
// the doc comment on runner.ConfineDumpAdmissionRow for why confine_peak_history
// structurally cannot carry either.
func buildConfineDumpAdmissions(subjects []store.ResourceBudgetSubjectRows) []runner.ConfineDumpAdmissionRow {
	rows := make([]runner.ConfineDumpAdmissionRow, 0, len(subjects))
	for _, subject := range subjects {
		for _, sample := range subject.Samples {
			row := runner.ConfineDumpAdmissionRow{
				RecordType: runner.ConfineDumpRecordAdmission,
				Kind:       string(subject.Kind),
				Signature:  subject.Signature,
				At:         sample.At,
				OOM:        sample.OOM,
				Outcome:    runner.ConfineDumpUnevaluated,
			}
			if sample.Peak != nil {
				peak := *sample.Peak
				row.ObservedPeakBytes = &peak
			}
			if sample.Budget != nil {
				budget := *sample.Budget
				row.DeclaredReserveBytes = &budget
				row.DeclaredReserveBasis = sample.BudgetBasis
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// waiterOutcomeName maps an admitWaiterState to the confine-dump wire
// vocabulary. admitReleased never appears here: a released waiter is
// removed from queue.waiters (releaseAdmitWaiterLocked), so this walk never
// observes one.
func waiterOutcomeName(waiter *admitWaiter) string {
	switch waiter.state {
	case admitQueued:
		// Genuinely undecided -- asserting a grant/deny now, before the
		// evaluator has decided either way, would be exactly the fabrication
		// the honesty rule forbids in the OTHER direction.
		return runner.ConfineDumpUnevaluated
	case admitGranted, admitRejected:
		// waiter.outcome is the daemon's own decided term ("immediate",
		// "waited", "saturated", ...) -- reused verbatim rather than
		// re-derived, so this can never drift from what admission itself
		// decided.
		if waiter.outcome != "" {
			return waiter.outcome
		}
		return runner.ConfineDumpUnevaluated
	default:
		return runner.ConfineDumpUnevaluated
	}
}

func waiterStateName(waiter *admitWaiter) string {
	switch waiter.state {
	case admitQueued:
		return "queued"
	case admitGranted:
		return "granted"
	case admitRejected:
		return "rejected"
	default:
		return runner.ConfineDumpUnevaluated
	}
}

// buildConfineDumpQueuesAndWaiters is a READ-ONLY, SEPARATE locked walk over
// every slice this daemon currently holds an admission queue for. It does
// not call or modify admitSliceSnapshotFor or any admission-decision
// function in admit.go -- admit.go is untouched by this slice, per the S18
// brief's hard constraint. It reads the same queue fields
// admitSliceSnapshotFor and confine_manage.go's listing already read
// (queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs,
// queue.waiters, queue.freezeArmedAt) under the same queue.mu discipline,
// and the same memory-reader seam admitConnection uses, so a negative
// `available` is computed on the SAME signed arithmetic admission itself
// uses (checkedAvailable/ledgerAvailable), never re-derived differently.
//
// Ordering is by slice path (sorted), so the dump is deterministic across
// runs rather than depending on Go's randomised map iteration.
func (s *Server) buildConfineDumpQueuesAndWaiters() ([]runner.ConfineDumpQueueRow, []runner.ConfineDumpWaiterRow) {
	s.admitRegistryMu.Lock()
	paths := make([]string, 0, len(s.admitQueues))
	byPath := make(map[string]*sliceQueue, len(s.admitQueues))
	for path, queue := range s.admitQueues {
		paths = append(paths, path)
		byPath[path] = queue
	}
	s.admitRegistryMu.Unlock()
	sort.Strings(paths)

	now := s.admitNowTime()
	readMemory := s.memoryReader()
	shim := s.shimMode()
	rows := make([]runner.ConfineDumpQueueRow, 0, len(paths))
	var waiterRows []runner.ConfineDumpWaiterRow
	for _, path := range paths {
		queue := byPath[path]
		row := runner.ConfineDumpQueueRow{RecordType: runner.ConfineDumpRecordQueue, Slice: path}

		queue.mu.Lock()
		row.RAMOutstandingBytes = queue.outstanding
		row.CPUOutstandingCores = queue.cpuOutstanding
		// Captured under the lock, used for the headroom computation OUTSIDE
		// it: this is the queue's OWN CURRENT job count, matching what the
		// evaluator's fit-check charges for capacity already held (NOT
		// jobs+1, which is admitConnection's own prospective-new-admission
		// headroom for a job that has not arrived -- reporting that basis
		// here would UNDER-headroom relative to what admission actually
		// reserves, making `available` read more optimistic than reality).
		outstandingJobsSnapshot := queue.outstandingJobs
		var oldestWaitMS int64
		for _, waiter := range queue.waiters {
			if waiter == nil {
				continue
			}
			if waiter.state == admitQueued {
				row.QueuedCount++
				if waited := elapsedMilliseconds(waiter.enqueued, now); waited > oldestWaitMS {
					oldestWaitMS = waited
				}
			}
			waiterRow := runner.ConfineDumpWaiterRow{
				RecordType: runner.ConfineDumpRecordWaiter, Slice: path,
				ScopeID: waiter.scopeID, Signature: waiter.signature,
				ReserveBytes: waiter.reserve, CPUCores: waiter.cpu,
				State: waiterStateName(waiter), Outcome: waiterOutcomeName(waiter),
			}
			if waiter.state == admitQueued {
				waiterRow.WaitMS = elapsedMilliseconds(waiter.enqueued, now)
			} else {
				waiterRow.WaitMS = waiter.waitedMS
			}
			waiterRows = append(waiterRows, waiterRow)
		}
		if s.admitFreezeMaxHold > 0 {
			row.Phase = admitFreezePhaseAt(queue.freezeArmedAt, now, s.admitFreezeMaxHold).String()
		} else {
			row.Phase = "disabled"
		}
		queue.mu.Unlock()

		if row.QueuedCount > 0 {
			value := oldestWaitMS
			row.OldestBlockedWaitMS = &value
		}
		row.RestartFrozen = s.restartFrozenAt(now)
		row.CPUCeilingCores = s.cpuCeiling()

		// Memory read OUTSIDE queue.mu, matching confine_manage.go's own ordering
		// (it takes the ledger snapshot first, then reads memory unlocked). The
		// raw memory.max is reported here, NOT the AIRA-103 pressure-throttled
		// effective ceiling (sliceCeilingEffectiveMaximum) -- this dump is a
		// signed-ledger observation, not a live admission decision, and pulling
		// in that subsystem would be new surface this slice does not need.
		current, maximum, reclaimable, ok, _ := readMemory(path)
		jobsHeadroom := s.admitSliceHeadroom(outstandingJobsSnapshot)
		if ok && maximum >= 0 {
			value := maximum
			row.RAMCeilingBytes = &value
		}
		// checkedAvailable/ledgerAvailable return exactly 0 for BOTH a
		// genuinely-zero available AND an unusable/degenerate reading
		// (maximum<=headroom, or a negative current/maximum/outstanding) --
		// the two are indistinguishable from the return value alone (see its
		// own doc comment). Replicate its guard here so this dump reports
		// unevaluated rather than a possibly-fabricated 0/false in the
		// degenerate case, instead of trusting the collapsed result. Kept
		// SEPARATE from the ceiling guard above: a small ceiling relative to
		// headroom makes "available" unusable, but the ceiling figure itself
		// is still a real, established reading.
		if ok && maximum >= 0 && current >= 0 && row.RAMOutstandingBytes >= 0 && jobsHeadroom >= 0 && maximum > jobsHeadroom {
			var available int64
			if shim {
				available = ledgerAvailable(maximum, row.RAMOutstandingBytes, jobsHeadroom)
			} else {
				available = checkedAvailable(current, maximum, reclaimable, row.RAMOutstandingBytes, jobsHeadroom)
			}
			availableValue := available
			row.AvailableBytes = &availableValue
			row.NegativeAvailable = available < 0
		}

		rows = append(rows, row)
	}
	return rows, waiterRows
}
