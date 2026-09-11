package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"time"

	"aira/internal/runner"

	"golang.org/x/sys/unix"
)

// Daemon restart recovery (design §4, gate P1-A — "the biggest risk"). After a
// GRACEFUL restart the previous daemon wrote a lease dump (S10); a fresh daemon, after
// DB-open and BEFORE net.Listen, reloads a < leaseDumpFreshness dump, kill -0-probes
// each recorded pid (an EARLY-DROP optimisation, NOT the safety), seeds the survivors
// UNANCHORED, then at listen-ready freezes NEW admissions for restartFreeze so a slow
// survivor's re-declare is not beaten to its space by a new admission. A reloaded lease
// is re-anchored by the client's re-declare (S9); any lease STILL unanchored at
// end-of-freeze + unanchoredGrace is dropped REGARDLESS of pid — that drop timer is the
// REAL safety, because the dump records the SUPERVISOR pid, which outlives its workers,
// so kill -0 alone would leak a retired worker's lease forever.
//
// A CRASH restart (SIGKILL/OOM/panic) skips the graceful dump, so there is nothing to
// reload; every live client's re-declare is then an absent-lease establish-granted (S9).
const (
	// defaultRestartFreeze is how long NEW admissions wait from listen-ready (design
	// §4: 2000 ms). Localhost, so survivors reconnect well inside it.
	defaultRestartFreeze = 2 * time.Second
	// defaultUnanchoredGrace is the extra time after the freeze before a lease still
	// unanchored is dropped (design §4: ~10 s).
	defaultUnanchoredGrace = 10 * time.Second
)

// reloadLeaseDump reloads the restart lease dump, kill-probes each record, and seeds
// the survivors UNANCHORED. Called in Serve after DB-open, before net.Listen, so the
// ledger is pre-seeded before any connection is accepted. CONSUME-ONCE: the dump file
// is UNLINKED after the read attempt and before seeding, so a second restart cannot
// re-seed the same dump (which would double-count the ledger). Best-effort throughout:
// any read/decode/resolve failure logs and falls back to the re-declare path, which
// recovers a missing or partial dump (the dump is an optimisation, not a correctness
// requirement — design §4/S10).
func (s *Server) reloadLeaseDump() {
	path := leaseDumpPath(s.Paths)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// No dump: a real reboot (RuntimeDir is tmpfs, wiped on reboot) or the first
		// v0.5→new upgrade → start at full quota, which is correct (§4).
		return
	}
	if err != nil {
		log.Printf("aira daemon: restart: lease dump unreadable (%v); starting at full quota", err)
		return
	}
	// CONSUME-ONCE (§15 P2-B): unlink after ANY read attempt, BEFORE seeding. Remove
	// is best-effort — a failed remove is logged; the freshness check still bounds a
	// re-read, and the drop timer + re-declare make a stale re-seed self-correcting.
	if rmErr := os.Remove(path); rmErr != nil {
		log.Printf("aira daemon: restart: could not unlink lease dump %q after reading (consume-once best-effort): %v", path, rmErr)
	}
	dump, decErr := decodeLeaseDump(bytes.NewReader(data))
	if decErr != nil {
		// A malformed record returns the decoded PREFIX plus the error (§15 P2-B,
		// "logged and skipped"): seed the prefix, log the rest.
		log.Printf("aira daemon: restart: lease dump decode stopped after %d record(s): %v", len(dump.Records), decErr)
	}
	// Nothing to seed — a valid empty dump OR a header-decode failure (zero records,
	// zero Stamp). Return BEFORE the freshness check so a corrupt header does not also
	// log a misleading "56y old" from the zero-value Stamp.
	if len(dump.Records) == 0 {
		return
	}
	// FRESHNESS (§15 P2-A): a dump older than the threshold means a real reboot or a
	// slow drain → full quota. Uses s.admitNowTime() (not time.Since) so the fake clock
	// drives the boundary; the stamp is wall-clock UnixNano so it survives the restart.
	age := s.admitNowTime().Sub(dump.Stamp)
	if age > leaseDumpFreshness {
		log.Printf("aira daemon: restart: lease dump is %s old (> %s); starting at full quota", age.Round(time.Second), leaseDumpFreshness)
		return
	}
	// Resolve the ONE slice ONCE (D1: the frozen dump/frame carry no slice, so every
	// reloaded lease folds into the DefaultConfineSlice queue S9's re-declare also
	// targets — or a reloaded lease would land in a different queue than its
	// re-declare). A daemon that cannot resolve its own slice cannot locate the ledger:
	// skip the whole reload (the dump is already unlinked; re-declare recovers).
	slice, ok, reason := s.sliceResolver()(runner.DefaultConfineSlice)
	if !ok {
		log.Printf("aira daemon: restart: slice unresolved (%s); skipping reload of %d lease(s)", reason, len(dump.Records))
		return
	}
	kept, dropped := 0, 0
	for _, rec := range dump.Records {
		if !s.probeReloadedLeaseAlive(rec.ClientPID, rec.ProcessStartTick) {
			dropped++
			continue
		}
		charge := redeclareChargeOf(rec.Frame)
		request := admitRequest{
			scopeID:          charge.ScopeID,
			cpu:              charge.CPU,
			parentScopeID:    charge.ParentScopeID,
			clientPID:        rec.ClientPID,
			processStartTick: rec.ProcessStartTick,
		}
		if _, _, code, seedErr := s.enqueueReload(slice, charge.RAM, request); seedErr != nil {
			// A reload establish never refuses (absent lease, gates bypassed), so an
			// error is a bug or an impossible duplicate scope id in the dump; log and
			// skip that one record, keep the rest (best-effort).
			log.Printf("aira daemon: restart: reload seed for scope %q failed: %s: %v", charge.ScopeID, code, seedErr)
			continue
		}
		kept++
	}
	log.Printf("aira daemon: restart: reloaded %d held lease(s) from a %s-old dump (%d dropped by kill-probe); new admissions frozen for %s pending re-declare",
		kept, age.Round(time.Second), dropped, s.restartFreeze)
}

// probeReloadedLeaseAlive is the reload kill-probe (design §4): an EARLY-DROP
// optimisation, NOT the safety (the unanchored-drop timer is). It DROPS (returns false)
// ONLY on POSITIVE death proof and KEEPS (returns true) on EVERY ambiguity, because the
// drop timer collects whatever is never re-declared — dropping on uncertainty would
// discard a live holder's lease.
//
// Decision table:
//
//	pid <= 0                               keep (no syscall; NEVER kill -0 0, which
//	                                       signals the whole process group)
//	kill(pid,0) == ESRCH                   DROP (no such process)
//	kill(pid,0) == EPERM / other error     keep (alive, another uid / unknown)
//	alive, record tick == 0                keep (unreadable at dump time — honest)
//	alive, live tick unreadable/0          keep (cannot compare)
//	alive, both ticks known and DIFFER     DROP (pid was recycled by another process)
//	alive, both ticks known and EQUAL      keep (same process still running)
func (s *Server) probeReloadedLeaseAlive(pid int, recordTick uint64) bool {
	if pid <= 0 {
		return true
	}
	kill := s.killForProbe
	if kill == nil {
		kill = func(pid int) error { return unix.Kill(pid, 0) }
	}
	if err := kill(pid); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return false
		}
		return true
	}
	if recordTick == 0 {
		return true
	}
	tick, ok, _ := readProcStartTime(pid)
	if !ok || tick == 0 {
		return true
	}
	return tick == recordTick
}

// armRestartFreeze sets the freeze-end instant to now + restartFreeze, freezing NEW
// admissions until then (design §4). Called ONCE at listen-ready. A non-positive
// restartFreeze arms nothing (0 = unarmed), so a build that disables the freeze is
// representable.
func (s *Server) armRestartFreeze(now time.Time) {
	if s.restartFreeze <= 0 {
		return
	}
	s.restartFreezeUntilNanos.Store(now.Add(s.restartFreeze).UnixNano())
}

// restartFrozenAt reports whether the restart new-admission freeze is active at now.
// Unarmed (0) is never frozen. Read by the evaluator grant pass and the
// GrantedEstablished honesty bit.
func (s *Server) restartFrozenAt(now time.Time) bool {
	end := s.restartFreezeUntilNanos.Load()
	return end != 0 && now.UnixNano() < end
}

// runRestartFreeze is the restart-recovery timer, spawned at listen-ready and cancelled
// on shutdown. Two phases, driven by the restartAfter seam (nil → time.After) so tests
// advance the schedule without a real sleep:
//
//  1. at freeze-end: signal every admit queue so a waiter blocked PURELY by the freeze
//     re-evaluates immediately rather than at the next poll tick.
//  2. at freeze-end + unanchoredGrace: drop every lease STILL unanchored — the REAL
//     safety (kill -0 alone leaks a supervisor pid that outlived its worker).
//
// It runs UNCONDITIONALLY — NOT gated on the freeze having been armed. The drop safety
// must NOT be coupled to the freeze: a build with restartFreeze<=0 (freeze disabled) that
// still reloaded a dump seeds unanchored leases which must still be dropped. Both phases
// no-op harmlessly when there is nothing to do (an immediate signal, a drop over an empty
// unanchored set), so the cost of always running is a single cancellable goroutine.
func (s *Server) runRestartFreeze(ctx context.Context) {
	after := s.restartAfter
	if after == nil {
		after = time.After
	}
	select {
	case <-ctx.Done():
		return
	case <-after(s.restartFreeze):
	}
	s.signalAllAdmitQueues()
	select {
	case <-ctx.Done():
		return
	case <-after(s.unanchoredGrace):
	}
	s.dropUnanchoredLeases()
}

// signalAllAdmitQueues kicks every live admit queue. Used at freeze-end so a waiter that
// was blocked purely by the restart freeze re-evaluates at once. The queue set is
// snapshotted under admitRegistryMu, which is released before signalling (queue.signal
// is a non-blocking coalescing send).
func (s *Server) signalAllAdmitQueues() {
	s.admitRegistryMu.Lock()
	queues := make([]*sliceQueue, 0, len(s.admitQueues))
	for _, queue := range s.admitQueues {
		queues = append(queues, queue)
	}
	s.admitRegistryMu.Unlock()
	for _, queue := range queues {
		queue.signal()
	}
}

// dropUnanchoredLeases is the restart-freeze SAFETY (design §4 gate P1-A): at
// end-of-freeze + grace it drops every lease STILL unanchored — a reloaded lease no
// live client ever re-declared. kill -0 at reload is only an early-drop optimisation; a
// supervisor pid that outlived its worker is alive, so kill -0 alone would keep a
// retired worker's lease forever.
//
// Reading waiter.unanchored AND dropping it MUST be one queue.mu critical section: a
// concurrent re-declare's anchorLeaseLocked clears unanchored under the SAME lock, so a
// re-declare that lands mid-scan re-anchors its lease and this scan must then NOT drop
// it. The release is the UNCONDITIONAL releaseAdmitWaiterLocked (no conn): this is NOT
// an EOF release, and releaseAdmitWaiterLockedAnchored's conn == nil guard would REFUSE
// a nil-anchor reloaded lease. afterAdmitRelease runs only after queue.mu is dropped.
func (s *Server) dropUnanchoredLeases() {
	s.admitRegistryMu.Lock()
	queues := make([]*sliceQueue, 0, len(s.admitQueues))
	for _, queue := range s.admitQueues {
		queues = append(queues, queue)
	}
	s.admitRegistryMu.Unlock()
	for _, queue := range queues {
		dropped := false
		queue.mu.Lock()
		// Iterate a COPY: releaseAdmitWaiterLocked mutates queue.waiters in place.
		for _, waiter := range append([]*admitWaiter(nil), queue.waiters...) {
			if waiter == nil || !waiter.unanchored {
				continue
			}
			if releaseAdmitWaiterLocked(queue, waiter) {
				dropped = true
				log.Printf("aira daemon: restart: dropping lease %q still unanchored at end-of-freeze+grace (no re-declare; reserve=%d cpu=%d)",
					waiter.scopeID, waiter.reserve, waiter.cpu)
			}
		}
		queue.mu.Unlock()
		if dropped {
			s.afterAdmitRelease(queue)
		}
	}
}
