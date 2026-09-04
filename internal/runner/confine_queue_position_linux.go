//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// confineQueuePosition is one blocked launcher's own place in the daemon's
// admission queue (AIRA-24). It exists only as a rendering input for the
// periodic "waiting for memory admission" progress line: a wait that a job
// cannot see into is indistinguishable from a hang, and the aggregate
// `--list` summary (AIRA-73) answers "how full is the slice", never "am I
// next".
//
// position is 1-based within the queued waiters, in the daemon's evaluation
// order — NOT a promise of grant order, because the AIRA-59 fairness duty
// cycle's yield phase can admit a later, smaller waiter that fits while the
// head is still too large. aheadBytes is the summed reserve of the waiters
// ahead: a fact, never an ETA, which nothing in this system can establish.
type confineQueuePosition struct {
	position   int
	queued     int
	aheadBytes int64
}

// confineQueueProbeTimeout bounds one probe. It is deliberately far shorter
// than the diagnostic interval that drives it: the probe is a nicety on a job
// that is already blocked, so it must never become a second thing to wait for.
const confineQueueProbeTimeout = 2 * time.Second

// confineQueueNote renders the queue clause of one admission-wait progress
// line, or "" when no position could be established.
//
// The probe is bounded twice over: by its own short timeout, and by the end of
// the admission wait itself. The launch path joins this goroutine before it
// continues, so a daemon that accepts a query and never answers would
// otherwise hold an already-granted job at the starting line for the whole
// timeout. waitDone is the same channel that stops the ticker.
//
// An absent position prints NOTHING — not "position unknown", which would put
// a line no operator can act on onto every tick of a daemon-less wait, and
// certainly not a zero, which would state that nothing is queued while this
// very job waits in the queue.
func confineQueueNote(ctx context.Context, deps confineDeps, request ConfineRequest, slicePath string, waitDone <-chan struct{}) string {
	if deps.queuePosition == nil {
		return ""
	}
	timeout := deps.admitQueueProbeTimeout
	if timeout <= 0 {
		timeout = confineQueueProbeTimeout
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, timeout)
	watcherStopped := make(chan struct{})
	go func() {
		defer close(watcherStopped)
		select {
		case <-waitDone:
			cancelProbe()
		case <-probeCtx.Done():
		}
	}()
	position, ok := deps.queuePosition(probeCtx, request, slicePath)
	cancelProbe()
	<-watcherStopped
	if !ok || position.position <= 0 || position.queued < position.position {
		return ""
	}
	// A known-zero ahead-figure is the head of the queue and must read as the
	// fact it is; FormatConfineBytes renders any non-positive value as
	// "unknown", which is the one thing this is not.
	ahead := "0B"
	if position.aheadBytes > 0 {
		ahead = FormatConfineBytes(position.aheadBytes)
	}
	return fmt.Sprintf(", queue position %d of %d, %s reserved ahead", position.position, position.queued, ahead)
}

// confineQueuePositionFromDaemon asks the daemon, over its OWN short-lived
// connection, where this job sits in the queue.
//
// A separate connection is load-bearing, not incidental: the blocked admit
// socket is the lease, and the daemon's admit handler reads one byte from it
// solely to detect the client going away (internal/daemon/admit.go). Writing a
// progress query onto that socket would be read as exactly that and would drop
// the job's own place in the queue. Never multiplex the admission socket.
//
// Every failure is silent and returns false. This is a diagnostic: a daemon
// that is down, wedged, older, or simply does not know this scope id must
// leave the existing progress line exactly as it was, never replace a real
// wait with an error the operator cannot act on. A false return is "no
// position established", never "position zero".
func confineQueuePositionFromDaemon(ctx context.Context, request ConfineRequest, slicePath string) (confineQueuePosition, bool) {
	socket := strings.TrimSpace(request.AdmitSocketPath)
	scopeID := strings.TrimSpace(request.ScopeID)
	if socket == "" || scopeID == "" || strings.TrimSpace(slicePath) == "" {
		return confineQueuePosition{}, false
	}
	// The daemon validates the owner on every confine management request and
	// refuses an empty one. confine-list itself never uses it (only
	// confine-kill's ownership check does), so a launcher whose owner is unset
	// or malformed asks under the explicit "nobody claimed this" identity
	// rather than being unable to ask at all.
	owner := strings.TrimSpace(request.Owner)
	if ValidateConfineOwner(owner) != nil {
		owner = ConfineUnknownOwner
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return confineQueuePosition{}, false
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(confineQueueProbeTimeout)
	}
	_ = conn.SetDeadline(deadline)
	// Cancellation must reach a probe already blocked in a read: the launch
	// path joins this goroutine, so a wedged daemon would otherwise hold a
	// granted job at the starting line for the whole timeout.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "confine-list"
	// slicePath is the RESOLVED cgroup path, the same value the admit request
	// carries, so the daemon reads the queue this job is actually queued on
	// rather than re-resolving a slice name to a possibly different one.
	frame.Request.Args = map[string]any{"slice": slicePath, "owner": owner, "scope_id": scopeID}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		return confineQueuePosition{}, false
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		return confineQueuePosition{}, false
	}
	if !response.OK || response.Code != "OK" || len(response.Data) == 0 {
		return confineQueuePosition{}, false
	}
	var result ConfineListResult
	if err := json.Unmarshal(response.Data, &result); err != nil {
		return confineQueuePosition{}, false
	}
	reserve := result.SliceReserve
	// A daemon that could not read the slice omits the summary entirely, and a
	// scope id it does not have queued reports no position. Both are honest
	// absences, and both must render as no position at all.
	if reserve == nil || reserve.QueuePosition <= 0 {
		return confineQueuePosition{}, false
	}
	queued := reserve.Queued
	if queued < reserve.QueuePosition {
		// The two are derived in one locked pass, so this is unreachable from a
		// current daemon. Refusing it keeps a nonsense pair ("3 of 1") out of an
		// operator-facing line rather than printing whatever arrives.
		return confineQueuePosition{}, false
	}
	ahead := reserve.QueuedAheadBytes
	if ahead < 0 {
		return confineQueuePosition{}, false
	}
	return confineQueuePosition{position: reserve.QueuePosition, queued: queued, aheadBytes: ahead}, true
}
