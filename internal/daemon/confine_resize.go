package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aira/internal/runner"
)

// AIRA-312. `aira confine --resize <name|supervisor-pid|scope-id> --memory-reserve
// SZ`: lower the admission-ledger reserve of one RUNNING (granted) confine job, so
// an operator can hand back the unused part of a job that is clearly massively
// over-reserved without killing it.
//
// The whole effect is one field on the granted waiter (reserve) and the ledger
// re-derived from it. Ledger only, by design (spec decision): the job's
// memory.max is NOT touched, so after a resize the ledger can under-count what
// the job may legally use by up to old-new; the slice MemoryMax and the memory
// watchdog stay the backstops. The OOM-steering budget reads the same reserve
// (admitScopeBudgets), so it drops with it.
//
// A reduction is dropped by ANY re-declare of the lease: the client's lease
// keeper re-declares the reserve it was GRANTED, and the re-anchoring SET in
// enqueueAdmitInternal overwrites reserve with it (a reconnect without a daemon
// restart), and a restarted daemon has no memory of the resize at all. That is
// the fail-safe direction (back to larger), and no persistence machinery exists.
//
// Resolution runs against the daemon's own GRANTED waiters, under queue.mu, with
// the selector grammar --front uses (confineFrontSelects): an exact scope id
// always matches; a name or supervisor pid only a job-level, non-worker waiter.

// resizeRunningConfine parses rawReserve, resolves selector to exactly one
// running job on path, applies the shared ownership guard, checks the new
// reserve is a reduction at or above the job's live memory use, and lowers it.
// It returns the result and the live usage it checked against, or a coded
// error, and signals the queue (outside queue.mu) on success so a waiter that
// now fits is evaluated at once.
func (s *Server) resizeRunningConfine(path, selector, rawReserve, callerOwner string, steal bool) (runner.ConfineResizeResult, int64, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return runner.ConfineResizeResult{}, 0, errors.New("E_CONFINE_ARGUMENT_INVALID: confine-resize requires one selector")
	}
	newReserve, err := runner.ParseConfineResizeReserve(rawReserve)
	if err != nil {
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --memory-reserve: %w", err)
	}
	readUsage := s.scopeUsageReader()
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("%s: selector %q matched no running confine job (the daemon holds no admission for this slice)", runner.CodeConfineNotFound, selector)
	}
	queue.mu.Lock()
	result, usage, err := resizeRunningConfineLocked(queue, selector, newReserve, callerOwner, steal, readUsage)
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	if err != nil {
		return runner.ConfineResizeResult{}, 0, err
	}
	queue.signal()
	return result, usage, nil
}

// resizeRunningConfineLocked is the resolution, the validation and the effect.
// queue.mu must be held. Every refusal returns before the one write, so a
// refused resize leaves the ledger exactly as it found it. The usage read is a
// cgroup file read under queue.mu, the same shape as the evaluator's own
// slice-memory read.
func resizeRunningConfineLocked(queue *sliceQueue, selector string, newReserve int64, callerOwner string, steal bool, readUsage func(string) (int64, bool, string)) (runner.ConfineResizeResult, int64, error) {
	var matches []*admitWaiter
	for _, waiter := range queue.waiters {
		if waiter != nil && waiter.state == admitGranted && waiter.accounted && confineFrontSelects(waiter, selector) {
			matches = append(matches, waiter)
		}
	}
	if len(matches) == 0 {
		// Distinguish "still waiting" from "no such job": an operator who named a
		// job that has not been admitted yet must be told so, and pointed at the
		// verbs that act on a queued job.
		for _, waiter := range queue.waiters {
			if waiter != nil && waiter.state == admitQueued && confineFrontSelects(waiter, selector) {
				return runner.ConfineResizeResult{}, 0, fmt.Errorf("%s: selector %q names %s, which is still waiting for admission and holds no reserve yet; use `aira confine --front` or `--kill` on a queued job", runner.CodeConfineNotRunning, selector, waiter.scopeID)
			}
		}
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("%s: selector %q matched no running confine job", runner.CodeConfineNotFound, selector)
	}
	if len(matches) > 1 {
		ids := make([]string, len(matches))
		for i := range matches {
			ids[i] = matches[i].scopeID
		}
		sort.Strings(ids)
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("E_SELECTOR_AMBIGUOUS: selector %q matched %s", selector, strings.Join(ids, ", "))
	}
	target := matches[0]
	name, _, _, owner, parsed := runner.ParseConfineScopeID(target.scopeID)
	if target.isSubReservation() || !parsed || runner.IsAitestWorkerScopeName(name) || strings.Contains(target.scopeID, "/") {
		// Reached only by an explicit scope id. A sub-reservation or an aitest
		// worker is an admitted job's internal progress; its charge follows its
		// parent's, so resizing one alone would mean nothing.
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("%s: %s is a sub-reservation or worker whose reserve follows its parent job; resize the parent job instead", runner.CodeConfineNotRunning, target.scopeID)
	}
	if strings.TrimSpace(owner) == "" {
		owner = runner.ConfineUnknownOwner
	}
	// Ownership comes from the scope id the client minted (AIRA-52), through the
	// one shared guard, exactly as --kill and --front take it.
	if err := runner.CheckConfineOwner(target.scopeID, owner, callerOwner, steal); err != nil {
		return runner.ConfineResizeResult{}, 0, err
	}
	old := target.reserve
	if newReserve >= old {
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("%s: %s reserves %d bytes; a resize must name a smaller reserve (asked for %d)", runner.CodeConfineResizeNotReduction, target.scopeID, old, newReserve)
	}
	usage, ok, reason := readUsage(filepath.Join(queue.path, confineScopeDirName(target.scopeID)))
	if !ok {
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("%s: the live memory use of %s could not be established, so no floor exists to check the new reserve against: %s", runner.CodeConfineResizeUsageUnevaluated, target.scopeID, reason)
	}
	if newReserve < usage {
		return runner.ConfineResizeResult{}, 0, fmt.Errorf("%s: %s is using %d bytes right now; the new reserve (%d) may not be below it", runner.CodeConfineResizeBelowUsage, target.scopeID, usage, newReserve)
	}
	target.reserve = newReserve
	// The one ledger writer, all four derived totals together. CPU and VRAM are
	// unchanged by a resize, but they are re-derived beside RAM so the cache can
	// never be left half-refreshed.
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	return runner.ConfineResizeResult{
		Status: "resized", ScopeID: target.scopeID, Name: name, Owner: owner,
		OldReserveBytes: old, NewReserveBytes: newReserve,
	}, usage, nil
}

// scopeUsageReader returns the job-scope live-usage reader for this daemon's
// mode, on the same single-decision-point rule as sliceResolver and
// memoryReader. ci-shim mode has no per-job cgroup, so its reader always
// answers unevaluated; a test override wins over both.
func (s *Server) scopeUsageReader() func(string) (int64, bool, string) {
	if s.confineScopeUsage != nil {
		return s.confineScopeUsage
	}
	if s.shimMode() {
		return func(string) (int64, bool, string) {
			return 0, false, "ci-shim mode has no per-job cgroup to read live memory use from"
		}
	}
	return readConfineScopeUsage
}

// readConfineScopeUsage reads one job scope's memory.current with the existing
// reader (readSliceCeilingCurrent, which the OOM steerer also uses for scopes).
// A measured 0 is a reading. A failure says why: a scope directory that is not
// there (still launching, or already gone) is told apart from one whose
// memory.current is unreadable or malformed. Never a guessed floor.
func readConfineScopeUsage(dir string) (int64, bool, string) {
	if value, ok := readSliceCeilingCurrent(dir); ok {
		return value, true, ""
	}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, false, "the job's scope cgroup is not present (still launching, or already gone)"
	}
	return 0, false, "the job's scope memory.current could not be read or parsed"
}

// logConfineResize writes the audit line for a resize the daemon applied.
// Called only after success, like confine-kill's and confine-front's lines: a
// refused resize changed nothing and must not leave a line claiming otherwise.
func logConfineResize(callerOwner string, steal bool, result runner.ConfineResizeResult, usage int64, path string) {
	log.Printf("aira daemon: confine-resize: actor=%s steal=%v target-scope=%s target-name=%s target-owner=%s old=%d new=%d usage=%d slice=%s",
		callerOwner, steal, result.ScopeID, result.Name, result.Owner, result.OldReserveBytes, result.NewReserveBytes, usage, path)
}
