//go:build linux

package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"aira/internal/redeclare"
)

const (
	// leaseKeeperDialTimeout + leaseKeeperReconnectGap give design §4's "reconnect
	// 2/sec (500 ms timeout)": a 500 ms dial timeout then a 500 ms gap before the
	// next attempt. Localhost, so a returning daemon is reached well inside the 2 s
	// freeze window.
	leaseKeeperDialTimeout  = 500 * time.Millisecond
	leaseKeeperReconnectGap = 500 * time.Millisecond
	// leaseKeeperExchangeGrace bounds the re-declare frame-write + 1-byte-ack read on
	// a freshly reconnected conn, so a wedged daemon cannot park the keeper forever.
	// Cleared the instant the ack arrives — the held read that follows has no deadline
	// (a lease is held for the whole job life).
	leaseKeeperExchangeGrace = 5 * time.Second
	// leaseKeeperMaxNoAck bounds CONSECUTIVE connected-but-no-ack re-declare attempts
	// within one restart episode. A refuse and a crash-mid-re-declare are
	// indistinguishable to the client (both are EOF with no ack, S9 residual), and
	// every legitimate refuse cause is unreachable for a real post-crash client, so we
	// retry — but not forever: after N we give up, log loudly, and leave the job
	// running under its cgroup cap. We NEVER kill it and NEVER fall open.
	leaseKeeperMaxNoAck = 10
)

// leaseKeeper owns the daemon admission lease connection for a granted job. For a
// scope-bearing, non-exclusive grant it RE-DECLARES the lease across a daemon
// restart (design §4): when the held connection EOFs it reconnects (2/sec) and
// sends the frozen ARDR frame, re-anchoring the survivor's lease in the new daemon
// so the end-of-freeze+grace drop does not collect it. It NEVER falls open to an
// ungoverned launch — the job is already running and cgroup-capped, so indefinite
// reconnect is harmless (failure class 2).
//
// Three modes, decided once at construction:
//   - scope-bearing, non-exclusive → reconnect + re-declare (frame set, goroutine
//     started here).
//   - scope-LESS (a confine-reserve sub-reservation has no ARDR key) → HOLD only:
//     no goroutine, the conn is just held and closed at teardown, as before S13.
//   - exclusive → the lease is LOST on restart (exclusivity cannot be re-established,
//     §15 P2-D), so it never reconnects; the confine caller starts the exclusive
//     watcher via watchExclusive.
//
// The keeper wraps admissionResult.release, so teardown's Close() is the single
// point that ends the hold and stops reconnecting. THE INVARIANT that prevents a
// leaked lease across a teardown/reconnect race: k.conn is ALWAYS the one
// connection Close() will close. adopt() installs a freshly dialled conn as k.conn
// UNDER THE LOCK (refusing if already stopped) BEFORE the re-declare exchange, so a
// Close() racing the exchange closes exactly that conn and the exchange errors out;
// the post-ack stopped recheck is belt-and-braces on top of the invariant.
type leaseKeeper struct {
	mu      sync.Mutex
	conn    net.Conn
	stopped bool
	done    chan struct{} // closed by the first Close(); wakes a reconnect back-off

	// Reconnect inputs. frame == nil means "do not reconnect" (scope-less or
	// exclusive); dial is the same dialer the admit exchange used.
	dial       func(context.Context, string) (net.Conn, error)
	socketPath string
	frame      []byte
	scopeID    string // for log lines only

	// Timings, defaulted from the leaseKeeper* consts by newLeaseKeeper. Instance
	// fields so the keeper's state machine can be driven synchronously in tests
	// (a directly-constructed keeper sets small values) without mutating globals.
	dialTimeout  time.Duration
	reconnectGap time.Duration
	maxNoAck     int

	// Exclusive branch, wired by the confine caller via watchExclusive.
	exclusiveLost atomic.Bool
	warnOnce      sync.Once
}

// newLeaseKeeper wraps the granted lease conn and, for a scope-bearing
// non-exclusive grant, starts the reconnect goroutine. grant carries the daemon's
// echoed figures; the re-declared RAM is grant.Reserve (the exact reserve the
// ledger holds — re-SETting it is idempotent, no double-count), the CPU is the
// declared core count (1, matching the original admit — S2a made a delegate job ordinary).
func newLeaseKeeper(conn net.Conn, req Request, grant runnerAdmitGrant, dial func(context.Context, string) (net.Conn, error), socketPath string) *leaseKeeper {
	if req.Exclusive || req.ConfineScopeID == "" || dial == nil {
		// Exclusive: never reconnect (watchExclusive handles EOF). Scope-less: no
		// ARDR key, hold only. Either way, no reconnect goroutine.
		return newLeaseKeeperFrame(conn, nil, req.ConfineScopeID, dial, socketPath)
	}
	cpuCores := uint32(DefaultConfineCPUCores)
	frame, err := redeclare.EncodeFrame(redeclare.Record{
		ScopeID:       req.ConfineScopeID, // VERBATIM: the daemon keys the lease on this string
		RAMBytes:      uint64(grant.Reserve),
		CPUCores:      cpuCores,
		ParentScopeID: req.ParentScopeID,
	})
	if err != nil {
		// A minted scope id is always valid utf8 and the reserve is > 0, so this is
		// unreachable in practice; if it ever fires, hold without reconnecting (the
		// job is cgroup-capped) rather than spin on an un-encodable frame.
		log.Printf("aira: lease keeper: cannot encode re-declare frame for scope %q: %v; holding without reconnect", req.ConfineScopeID, err)
		return newLeaseKeeperFrame(conn, nil, req.ConfineScopeID, dial, socketPath)
	}
	return newLeaseKeeperFrame(conn, frame, req.ConfineScopeID, dial, socketPath)
}

// newLeaseKeeperFrame is the shared keeper core (S15): it wraps conn and, when frame
// is non-nil and a dialer is available, starts the reconnect + re-declare loop that
// re-anchors the lease across a daemon restart. A nil frame (or nil dial) is a
// HOLD-ONLY keeper — a scope-less confine-reserve, an exclusive lease that cannot be
// re-established, or a shim advisory worker lease with no ARDR key. Both the confine
// keeper (newLeaseKeeper) and the aitest worker relay (RequestWorkerAdmit) build
// their own frozen ARDR frame and hand it here, so there is ONE reconnect state
// machine and two frame-builders, not two loops that could drift.
func newLeaseKeeperFrame(conn net.Conn, frame []byte, scopeID string, dial func(context.Context, string) (net.Conn, error), socketPath string) *leaseKeeper {
	k := &leaseKeeper{
		conn:         conn,
		done:         make(chan struct{}),
		dial:         dial,
		socketPath:   socketPath,
		scopeID:      scopeID,
		dialTimeout:  leaseKeeperDialTimeout,
		reconnectGap: leaseKeeperReconnectGap,
		maxNoAck:     leaseKeeperMaxNoAck,
	}
	if frame == nil || dial == nil {
		return k
	}
	k.frame = frame
	go k.reconnectLoop()
	return k
}

// Close stops the keeper and closes the current lease connection, which the daemon
// sees as the lease-releasing EOF. Idempotent.
func (k *leaseKeeper) Close() error {
	k.mu.Lock()
	if k.stopped {
		k.mu.Unlock()
		return nil
	}
	k.stopped = true
	conn := k.conn
	k.mu.Unlock()
	close(k.done)
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

func (k *leaseKeeper) isStopped() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.stopped
}

func (k *leaseKeeper) currentConn() net.Conn {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.conn
}

// adopt installs newConn as the current lease conn under the lock, closing the
// previous conn (the dead held conn, or a failed-exchange attempt). It returns
// false if Close() has already fired — the caller then closes newConn so it
// anchors no lease nobody will ever release.
func (k *leaseKeeper) adopt(newConn net.Conn) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return false
	}
	if k.conn != nil && k.conn != newConn {
		_ = k.conn.Close()
	}
	k.conn = newConn
	return true
}

// sleepOrStopped waits d, returning true if Close() fired during the wait so the
// caller exits the reconnect loop promptly instead of after a full back-off.
func (k *leaseKeeper) sleepOrStopped(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-k.done:
		return true
	case <-t.C:
		return false
	}
}

// reconnectLoop holds the lease conn and re-declares across daemon restarts until
// Close(). It is started only for a scope-bearing, non-exclusive grant.
func (k *leaseKeeper) reconnectLoop() {
	for {
		if k.blockOnCurrentRead() {
			return // Close() ended the read
		}
		// The held conn returned on its own (daemon EOF/restart). Re-anchor.
		if !k.reconnectAndReDeclare() {
			return // stopped, or bounded-give-up
		}
	}
}

// blockOnCurrentRead blocks on a 1-byte read of the current lease conn. The client
// writes NOTHING after the frame, so any return means the daemon side ended: a
// return via Close() (stopped) exits the keeper; any other return (EOF/restart)
// triggers a reconnect. Returns true iff stopped.
func (k *leaseKeeper) blockOnCurrentRead() bool {
	conn := k.currentConn()
	if conn == nil {
		return true
	}
	var one [1]byte
	_, _ = conn.Read(one[:])
	return k.isStopped()
}

type reDeclareOutcome int

const (
	reDeclareAcked reDeclareOutcome = iota
	reDeclareRetry
	reDeclareStopped
)

// reconnectAndReDeclare re-dials (2/sec, indefinitely on a down daemon) and
// re-declares the lease. It returns true once re-anchored (loop back to the held
// read), false when Close() fired or the bounded no-ack budget is exhausted.
func (k *leaseKeeper) reconnectAndReDeclare() bool {
	noAck := 0
	for {
		if k.isStopped() {
			return false
		}
		dctx, cancel := context.WithTimeout(context.Background(), k.dialTimeout)
		newConn, err := k.dial(dctx, k.socketPath)
		cancel()
		if err != nil {
			// Daemon not back yet (dial refused/ENOENT): retry forever at 2/sec. A
			// down daemon is not a no-ack, so it never spends the give-up budget.
			if k.sleepOrStopped(k.reconnectGap) {
				return false
			}
			continue
		}
		if !k.adopt(newConn) {
			// Close() raced between dial-success and adopt: close the conn we opened so
			// it anchors nothing, and exit.
			_ = newConn.Close()
			return false
		}
		switch k.reDeclareOnCurrent() {
		case reDeclareAcked:
			return true
		case reDeclareStopped:
			return false
		default: // reDeclareRetry
			noAck++
			if noAck >= k.maxNoAck {
				// Give-up honesty (concurrency review): the job's SAFETY is intact — its
				// connection stays closed and it runs under its own cgroup cap, so it never
				// falls open to an ungoverned launch. But the daemon LEDGER CHARGE is not
				// restored: nothing re-declares this lease after give-up, so the daemon
				// UNDER-COUNTS this slice until the job ends (the memory watchdog / OOM
				// killer is the backstop for the resulting over-admit). Accepted gap.
				log.Printf("aira: lease keeper: re-declare for scope %q got no ack after %d connected attempts; giving up reconnect. The job keeps running under its cgroup cap (never falls open), but its reserve is no longer counted in the daemon ledger until it ends — the slice is under-counted, with the memory watchdog as the backstop.", k.scopeID, noAck)
				return false
			}
			if k.sleepOrStopped(k.reconnectGap) {
				return false
			}
		}
	}
}

// reDeclareOnCurrent sends the frozen ARDR frame on the current conn and reads the
// single frozen ack byte. It bounds the exchange with a deadline (cleared on
// success). A transport error under Close() is reDeclareStopped; any other
// non-ack outcome is reDeclareRetry (indistinguishable refuse vs crash — S9).
func (k *leaseKeeper) reDeclareOnCurrent() reDeclareOutcome {
	conn := k.currentConn()
	if conn == nil {
		return reDeclareStopped
	}
	_ = conn.SetDeadline(time.Now().Add(leaseKeeperExchangeGrace))
	if err := writeRunnerAdmitBytes(conn, k.frame); err != nil {
		return k.retryUnlessStopped()
	}
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		return k.retryUnlessStopped()
	}
	if ack[0] != redeclare.AckByte {
		return reDeclareRetry
	}
	_ = conn.SetDeadline(time.Time{}) // a held lease has no deadline
	return reDeclareAcked
}

func (k *leaseKeeper) retryUnlessStopped() reDeclareOutcome {
	if k.isStopped() {
		return reDeclareStopped
	}
	return reDeclareRetry
}

// watchExclusive starts the exclusive-hold watcher (design §15 P2-D). An exclusive
// lease CANNOT be re-established across a daemon restart, so on the held conn's EOF
// the keeper records exclusive=lost and warns to w, but does NOT reconnect. Called
// at most once, by the confine caller, only for an exclusive grant; w is confine's
// locked diagnostics writer. The keeper's stopped flag stands in for the old
// teardownStarted guard: a close driven by teardown (Close()) is not a loss.
func (k *leaseKeeper) watchExclusive(w io.Writer) {
	go func() {
		conn := k.currentConn()
		if conn == nil {
			return
		}
		var one [1]byte
		_, err := conn.Read(one[:])
		if err == nil || k.isStopped() {
			return
		}
		k.exclusiveLost.Store(true)
		k.warnOnce.Do(func() {
			// AIRA-206: leading \n — a mid-run warning to the shared locked writer that
			// would otherwise glue onto the child's partial line.
			fmt.Fprint(w, "\naira: warning: exclusivity lost (admission lease closed) — this run was no longer scheduled alone; treat any measurement from it as contended\n")
		})
	}()
}

func (k *leaseKeeper) exclusiveWasLost() bool {
	return k.exclusiveLost.Load()
}
