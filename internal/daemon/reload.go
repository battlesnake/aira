package daemon

import (
	"context"
	"time"
)

// Daemon restart recovery (design §4, gate P1-A — "the biggest risk"). On ANY restart —
// crash (SIGKILL/OOM/panic) or graceful — the fresh daemon opens an EMPTY ledger and, at
// listen-ready, FREEZES new admissions for restartFreeze so a slow survivor's re-declare
// is not beaten to its space by a new admission. Every live client's keeper reconnects
// within that window and re-declares its lease; an absent-lease re-declare ESTABLISHES it
// granted (S9), re-anchoring the survivor's RAM/CPU in the fresh ledger. The 2s freeze +
// the signed ledger + the establish-granted re-declare carry the whole recovery.
//
// S13 deleted the graceful lease dump / reload / kill-probe / unanchored-drop layer: it
// was a best-effort pre-seed of a slow-re-declarer tail (not correctness — the crash path
// never had a dump and relied on re-declare alone), and it births the supervisor-pid
// indirection §4 flags. There is no dump to reload and no unanchored lease to drop; a
// granted lease is always anchored to a live connection.
const (
	// defaultRestartFreeze is how long NEW admissions wait from listen-ready (design
	// §4: 2000 ms). Localhost, so survivors reconnect well inside it.
	defaultRestartFreeze = 2 * time.Second
)

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
// on shutdown. At freeze-end it signals every admit queue so a waiter blocked PURELY by
// the freeze re-evaluates immediately rather than at the next poll tick. Driven by the
// restartAfter seam (nil → time.After) so tests advance the schedule without a real
// sleep. (S13 removed the second phase — the unanchored-drop — with the dump layer: with
// no dump there are no unanchored leases to collect.)
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
