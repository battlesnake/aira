package daemon

import (
	"context"
	"net"
	"time"
)

// deadlinePolicy is the daemon transport's ONE deadline convention, applied
// symmetrically to both ends of the socket (AIRA-84).
//
// It exists because this is the third connection-deadline defect of the same
// class in this transport (AIRA-18, AIRA-92, AIRA-84), and all three had the
// same shape: one deadline stamped at connect time was silently re-used to
// bound something it was never sized for. The convention, stated once:
//
//  1. CONNECT bounds the HANDSHAKE ONLY — the client writing its request frame
//     and the daemon reading it. Framing a request is never slow work, so one
//     fixed budget fits every verb. A connect deadline must never survive
//     frame parse; on the daemon side that is enforced by clearing the read
//     deadline exactly once, immediately after the inbound frame is parsed and
//     validated (serveConnection).
//
//  2. The RESPONSE WAIT is never the connect deadline. On the client it is the
//     caller's own context deadline when the caller declares one, else
//     ResponseWait. The daemon has no response wait — it PRODUCES the response;
//     its budget for doing so is verb-declared and lives in the handler
//     (serveStoreOp picking storeOpAppendTimeout vs storeOpHeavyTimeout per op
//     is the existing example, and is a different kind of timeout from
//     anything here: it bounds an operation's own work, not a socket).
//
//  3. A WRITE deadline is stamped IMMEDIATELY BEFORE each response write, so
//     it bounds the write itself and never how long the handler happened to
//     take. Violating (3) is precisely the AIRA-84 defect: a routed verb that
//     commits durably and then fails its response-frame write on a stale
//     connect-time deadline is reported to the caller as OUTCOME_UNKNOWN —
//     this project's worst failure direction, a lie about work that happened.
//
// Deliberately NOT covered, so "symmetric" has a defined boundary: the
// long-lived streaming paths (watch, admit, governor, worker-admit) own their
// own frames and already stamp their own justified write budgets, and
// operation execution budgets (storeops.go) answer "how long may this run",
// not "how long may this connection sit idle".
type deadlinePolicy struct {
	// Connect bounds the handshake: the client's request-frame write and the
	// daemon's request-frame read.
	Connect time.Duration
	// ResponseWait bounds the CLIENT's wait for a response when, and only
	// when, the caller declares no deadline of its own. It is unused by the
	// daemon.
	ResponseWait time.Duration
	// Write bounds one response-frame write, stamped immediately before it.
	Write time.Duration
}

// defaultDeadlines is the production policy.
//
// Connect and Write are 30s, UNCHANGED in value from the two constants they
// replace (server.go's connect stamp and storeOpWriteTimeout). The AIRA-84
// defect was never that the connect deadline was too long — it was that it
// outlived the handshake — so shortening it here would be an unforced
// behaviour change on a path where a legal request frame may be MaxFrameBytes.
//
// ResponseWait is DERIVED, not picked: it must exceed the daemon's largest
// sanctioned unit of work (storeOpHeavyTimeout, 5 minutes) plus its
// response-write allowance (Write, 30s), or the client re-creates AIRA-84's
// defect from the other end — abandoning work the daemon is still legitimately
// doing and reporting OUTCOME_UNKNOWN for a mutation that lands seconds later.
// 6 minutes is that sum plus a 30s margin, and deliberately no more: both
// plan-review lineages flagged that the wait is also the worst-case hang below,
// so it takes the SMALLEST value the derivation permits, not a round one.
// TestDefaultResponseWaitExceedsTheDaemonsLargestWorkBudget pins the
// relationship so neither constant can drift into re-opening the defect.
//
// THE DERIVATION IS INCOMPLETE, AND SAYING SO IS THE POINT (build-review
// finding, accepted). storeOpHeavyTimeout bounds STORE OPS. Generic routed
// verbs — the very path AIRA-84 is about — carry NO daemon-side work budget at
// all; they inherit the serve context and may run arbitrarily long. So
// ResponseWait is a floor derived from the only budget that exists, not a
// bound on the work it waits for: a routed verb that genuinely runs past 6
// minutes will still commit and still be reported OUTCOME_UNKNOWN. This fix
// narrows that window from 30 seconds to 6 minutes; it does not close it. The
// real closure is a per-verb execution budget for routed verbs, which is new
// machinery this change deliberately does not build (the remediation plan's
// deferral list carries it). Anyone tempted to read ResponseWait as a
// guarantee should read this paragraph instead.
//
// The accepted cost, stated rather than hidden: against a WEDGED daemon (one
// holding the connection without answering — not a crashed one, which closes
// the socket and yields an immediate EOF) a CLI call now blocks for up to
// ResponseWait instead of 30s. SIGINT still terminates it. That trade is the
// point: a rare pathological hang is a better failure than routinely
// fabricating OUTCOME_UNKNOWN for work that actually committed. It is bought
// with a bounded budget rather than a config knob, because a knob would be new
// machinery for a failure mode that is a daemon bug in the first place.
var defaultDeadlines = deadlinePolicy{
	Connect:      30 * time.Second,
	ResponseWait: 6 * time.Minute,
	Write:        30 * time.Second,
}

// requestPhaseDeadline bounds writing the request frame: the connect budget, or
// the caller's own deadline when that is sooner. A caller asking for less than
// the handshake budget still gets what it asked for.
func requestPhaseDeadline(ctx context.Context, now time.Time, policy deadlinePolicy) time.Time {
	deadline := now.Add(policy.Connect)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		return callerDeadline
	}
	return deadline
}

// responsePhaseDeadline bounds waiting for the response: the caller's own
// deadline whenever it declares one — longer OR shorter than ResponseWait,
// because a caller that declares a budget owns it — else ResponseWait.
func responsePhaseDeadline(ctx context.Context, now time.Time, policy deadlinePolicy) time.Time {
	if callerDeadline, ok := ctx.Deadline(); ok {
		return callerDeadline
	}
	return now.Add(policy.ResponseWait)
}

// resolvedDeadlines fills a zero-valued policy from the production defaults.
//
// A Server built as a bare struct literal rather than through NewServer — which
// several tests in this package do — would otherwise carry a zero policy, and
// every deadline derived from it would be time.Now(): ALREADY EXPIRED. That
// turns rule (3) inside out, failing precisely the writes it exists to protect,
// and it would fail as a baffling transport error rather than as a visible
// misconfiguration. The former storeOpWriteTimeout field had the same trap on
// one write site; this fix routes many more writes through the policy, so the
// trap is closed rather than inherited.
func (s *Server) resolvedDeadlines() deadlinePolicy {
	policy := s.deadlines
	if policy.Connect <= 0 {
		policy.Connect = defaultDeadlines.Connect
	}
	if policy.ResponseWait <= 0 {
		policy.ResponseWait = defaultDeadlines.ResponseWait
	}
	if policy.Write <= 0 {
		policy.Write = defaultDeadlines.Write
	}
	return policy
}

// reply writes one POST-HANDLER response frame under rule (3): a write deadline
// stamped immediately before the write, sized by the daemon, never inherited
// from connect time. It reports whether the frame was written, which
// serveConnection tracks so its panic recovery never double-writes.
//
// Handshake rejections (a malformed frame, a protocol mismatch, a foreign state
// identity) deliberately do NOT use reply: they are part of the handshake, so
// rule (1)'s connect deadline is the correct budget for them, and giving them a
// fresh 30s window instead would let a slow peer hold a serve goroutine longer
// than the handshake it failed (plan-review finding, accepted).
func (s *Server) reply(conn net.Conn, frame any) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(s.resolvedDeadlines().Write))
	return writeFrame(conn, frame) == nil
}

// replyStoreOp is reply for the store-op path, whose response carries a
// separately framed body.
func (s *Server) replyStoreOp(conn net.Conn, frame ResponseFrame) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(s.resolvedDeadlines().Write))
	return writeResponse(conn, frame) == nil
}
