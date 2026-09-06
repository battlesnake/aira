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
	// AIRA-101. WHY this job is waiting, when the reason is not ordinary
	// contention: a benchmark has asked for the slice to itself. Empty when no
	// exclusivity is active — established by the daemon in the same locked pass as
	// the position above, so it is a fact rather than an unknown.
	//
	// An operator waiting behind a drain needs to know a benchmark is running,
	// not conclude the machine is merely full and go looking for the wrong thing.
	exclusiveState string
	exclusiveName  string
	exclusiveOwner string

	// AIRA-103. Whether this wait is contention among AIRA's own jobs or a
	// ceiling reduced by memory used OUTSIDE the slice. Without it every wait
	// reads as the former, which is precisely the misattribution the dynamic
	// ceiling makes possible: the slice can look far from full while admission
	// is nonetheless closed. Carried on the SAME AIRA-24 probe -- no extra
	// round trip, no new verb -- and empty whenever the daemon did not report
	// it (older daemon, subsystem off, or ceiling unevaluated).
	ceilingThrottled bool
	// ceilingBasis (AIRA-106) is which policy term reduced the ceiling:
	// "system-pressure" or "machine-reserve". They are different facts about the
	// world and a launcher told the wrong one goes looking for the wrong cause,
	// so an unrecognised or absent basis names NEITHER.
	ceilingBasis string
	memAvailable int64
}

// confineQueueProbeTimeout bounds one probe. It is deliberately far shorter
// than the diagnostic interval that drives it: the probe is a nicety on a job
// that is already blocked, so it must never become a second thing to wait for.
const confineQueueProbeTimeout = 2 * time.Second

// Daemon cost, measured rather than asserted — twice, because the first
// measurement alone did not bound it. Build review raised the load question
// with AIRA-61's per-poll O(tree) scan (25-65% CPU) as the precedent to avoid,
// then raised that a per-call figure taken against a nearly-idle slice says
// nothing about a contended one, since the confine-list scan is O(live
// scopes). Both numbers:
//
//   - Per call, end to end: 1.5-1.7ms of daemon CPU at 3 live scopes (100 and
//     200 requests against the live daemon; committed repro
//     docs/dev/aira24-probe-cost.sh takes the utime+stime delta from
//     /proc/<daemon>/stat over the request count).
//   - Scan slope: ~16us per live scope — 0.02ms at 1 scope, 2.03ms at 128
//     (BenchmarkListConfinesByScopeCount, confine_list_scale_linux_test.go).
//
// Each waiter probes once per diagnostic tick (15s), and the queue's own
// admitMaxWaiters caps waiters at 256. The worst case therefore needs 256 jobs
// queued AND a slice carrying ~128 live scopes at the same time: 256/15s x
// (1.6ms + 2.0ms) = 61ms/s, about 6% of one core. The contended case actually
// observed (a handful of waiters, a handful of scopes) is under 0.1%. That is
// still well clear of AIRA-61's class, so the cadence stays at one probe per
// printed line and no cache is introduced.
//
// Accepted waste, named rather than hidden: the probe pays for the whole
// ConfineListResult and reads only SliceReserve. Avoiding that means a second
// daemon verb for one diagnostic — new machinery for a cost measured in tens
// of microseconds per scope, which this project's simplicity rule says not to
// build.

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
	if !ok {
		return ""
	}
	// AIRA-101. The exclusivity clause stands on its own. A job blocked behind a
	// drain often has NO queue position to report — during a hold it may be the
	// only waiter, and a position of "1 of 1" explains nothing — so tying this
	// clause to a valid position would hide the one fact that actually explains
	// the wait.
	exclusiveNote := ""
	switch position.exclusiveState {
	case "draining":
		exclusiveNote = ", slice draining for exclusive job " + describeExclusiveJob(position)
	case "held":
		exclusiveNote = ", slice held exclusively by " + describeExclusiveJob(position)
	}
	// AIRA-103. Stated BEFORE the position, and standing on its own for the same
	// reason the exclusivity clause does: it changes what the position means.
	// Under a reduced ceiling the queue may not be moving because of anything
	// AIRA is doing, and an operator reading only "position 4 of 9" would go
	// looking for the wrong cause.
	pressure := ""
	if position.ceilingThrottled {
		// AIRA-106. The cause is named from the basis, never assumed. Before it,
		// this line asserted external memory pressure for every reduced ceiling;
		// the static machine-reserve term makes that false on an idle box.
		// The basis names WHICH POLICY TERM bound the ceiling, which is what the
		// daemon actually established. It is deliberately not restated as a fact
		// about the machine: the dynamic term can bind on a perfectly idle box if
		// the configured free-memory reserve is large, so "reduced by memory used
		// outside the slice" would be an assertion the comparison does not support.
		// The MemAvailable figure beside it is what lets a launcher tell an idle
		// machine from a loaded one.
		switch position.ceilingBasis {
		case "system-pressure":
			pressure = ", slice ceiling reduced to keep the configured system free-memory reserve"
			if position.memAvailable > 0 {
				pressure += " (system MemAvailable " + FormatConfineBytes(position.memAvailable) + ")"
			}
		case "machine-reserve":
			pressure = ", slice ceiling reduced to keep the configured share of this machine outside the slice"
		default:
			pressure = ", slice ceiling reduced below the configured ceiling"
		}
	}
	if position.position <= 0 || position.queued < position.position {
		return pressure + exclusiveNote
	}
	// A known-zero ahead-figure is the head of the queue and must read as the
	// fact it is; FormatConfineBytes renders any non-positive value as
	// "unknown", which is the one thing this is not.
	ahead := "0B"
	if position.aheadBytes > 0 {
		ahead = FormatConfineBytes(position.aheadBytes)
	}
	// Two words earn their place here. "enqueue order" because the daemon's
	// fairness duty cycle can backfill a smaller waiter ahead of a stuck head,
	// so this is a place in the evaluation order and not a promise of being
	// admitted second. "queued ahead" because the figure counts ONLY the queued
	// waiters in front — the reserve already GRANTED to running jobs is much
	// larger and is not in it; a bare "reserved ahead" invites reading it as
	// "the memory standing between me and admission", which it is not.
	return fmt.Sprintf("%s, queue position %d of %d by enqueue order, %s queued ahead%s", pressure, position.position, position.queued, ahead, exclusiveNote)
}

// describeExclusiveJob names the exclusive job for the progress line. An unnamed
// or unowned holder is described as such rather than rendered as an empty pair
// of quotes, which would read as a bug in the line rather than as missing detail.
func describeExclusiveJob(position confineQueuePosition) string {
	name := strings.TrimSpace(position.exclusiveName)
	if name == "" {
		name = "an unnamed job"
	} else {
		name = fmt.Sprintf("%q", name)
	}
	owner := strings.TrimSpace(position.exclusiveOwner)
	if owner == "" {
		return name
	}
	return name + " (" + owner + ")"
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
	// A daemon that could not read the slice omits the summary entirely: nothing
	// can be established, so nothing is reported.
	if reserve == nil {
		return confineQueuePosition{}, false
	}
	// AIRA-101. Exclusivity is reported even when this caller has no queue
	// position, because a job blocked behind a HOLD is frequently the only waiter
	// and its position explains nothing while the exclusivity explains everything.
	exclusive := confineQueuePosition{}
	if reserve.Exclusive != nil {
		exclusive.exclusiveState = reserve.Exclusive.State
		exclusive.exclusiveName = reserve.Exclusive.Name
		exclusive.exclusiveOwner = reserve.Exclusive.Owner
	}
	// A scope id the daemon does not have queued reports no position. That is an
	// honest absence and must render as no position — never as "position zero" —
	// but it must not suppress the exclusivity clause above.
	if reserve.QueuePosition <= 0 {
		return exclusive, exclusive.exclusiveState != ""
	}
	queued := reserve.Queued
	if queued < reserve.QueuePosition {
		// The two are derived in one locked pass, so this is unreachable from a
		// current daemon. Refusing the PAIR keeps a nonsense "3 of 1" out of an
		// operator-facing line — but the exclusivity clause is derived
		// independently and stays reportable.
		return exclusive, exclusive.exclusiveState != ""
	}
	ahead := reserve.QueuedAheadBytes
	if ahead < 0 {
		return exclusive, exclusive.exclusiveState != ""
	}
	// AIRA-103. Only an ENFORCING, established, throttled ceiling is reported as
	// pressure: observe mode applies nothing, and an unevaluated ceiling is an
	// absence, not a state to announce to a blocked launcher. A HELD ceiling is
	// still applied, so it is still reported -- but its MemAvailable figure is up
	// to a TTL old, so the figure is dropped and only the fact is stated.
	exclusive.ceilingThrottled = reserve.CeilingMode == "enforce" && reserve.CeilingState == "throttled"
	exclusive.ceilingBasis = reserve.CeilingBasis
	// AIRA-106. The MemAvailable figure is reported only when system pressure is
	// what actually reduced the ceiling. Under the static machine-reserve term
	// MemAvailable is not the cause, and printing it beside the wait would invite
	// exactly the wrong diagnosis.
	if exclusive.ceilingThrottled && exclusive.ceilingBasis == "system-pressure" &&
		!reserve.CeilingHeld && reserve.MemAvailableBytes > 0 {
		exclusive.memAvailable = reserve.MemAvailableBytes
	}
	exclusive.position, exclusive.queued, exclusive.aheadBytes = reserve.QueuePosition, queued, ahead
	return exclusive, true
}
