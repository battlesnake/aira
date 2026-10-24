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
// order (pushed jobs first, then enqueue order — AIRA-309) — NOT a promise of
// grant order, because a later, smaller waiter that fits can be admitted past a
// head that is still too large, within the head's AIRA-307 budget or in the
// backstop's backfilling pass. aheadBytes is the summed reserve of the waiters
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

	// AIRA-181. The OTHER population — the jobs that are already ADMITTED, and
	// the reserve they hold. aheadBytes above counts only the queued waiters in
	// front, and at position 1 it is 0B by definition; four independent sessions
	// read that as "nothing is blocking me", killed correctly-waiting jobs, and
	// guessed their reserve down. The reserve standing between this job and
	// admission is almost entirely the granted total, which the probe already had
	// in its hands and threw away.
	//
	// heldEstablished is the honesty bit and is required: heldBytes 0 across
	// heldJobs 0 is a real, informative reading on an empty-but-closed slice
	// (a drain, a freeze, a collapsed ceiling), so a renderer must be able to
	// tell it from the zero value of a struct nobody filled in.
	//
	// ceilingBytes is what one MORE job would face — the same figure, from the
	// same reply, that `confine --list`'s own summary line prints. Zero is "not
	// established" and must render as nothing, never as a ceiling of zero.
	heldBytes       int64
	heldJobs        int
	heldEstablished bool
	ceilingBytes    int64

	// AIRA-186. This job's OWN resolved reserve, as the daemon has it. Zero is an
	// absence: an older daemon, or no queued waiter to speak for. See
	// ConfineSliceReserve.ResolvedReserveBytes for why the client cannot know
	// this figure by itself while it waits.
	resolvedReserve int64
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
// AIRA-308 added queued ROWS to that reply (up to ConfineQueuedRowLimit, each a
// live /proc read of the supervisor's argv, cwd and environ). The probe is
// exempted: the daemon builds no queued rows for a request that names a
// scope_id, so none of the cost above grew.
//
// Accepted waste, named rather than hidden: the probe pays for the whole
// ConfineListResult and reads only SliceReserve. Avoiding that means a second
// daemon verb for one diagnostic — new machinery for a cost measured in tens
// of microseconds per scope, which this project's simplicity rule says not to
// build.

// confineQueueNote renders the queue clause of one admission-wait progress
// line, or "" when nothing could be established. The bool reports whether the
// daemon placed this job in its queue (a real position), which is the precondition
// for offering the `--front` command: a push of a job the daemon does not hold
// queued is refused.
//
// The line is deliberately short (owner 2026-10-24: the long form, with queued-ahead
// bytes and a job count, was unreadable). It states only what lets a reader decide
// what to do: where the job stands, how full the slice is, and any NON-ordinary
// reason for the wait. The full picture is `aira confine --list` / `aira top`.
//
// The probe is bounded twice over: by its own short timeout, and by the end of
// the admission wait itself. The launch path joins this goroutine before it
// continues, so a daemon that accepts a query and never answers would
// otherwise hold an already-granted job at the starting line for the whole
// timeout. waitDone is the same channel that stops the ticker.
//
// An absent position prints NOTHING -- not "position unknown", and certainly not
// a zero, which would state that nothing is queued while this very job waits in
// the queue.
//
// clientReserve is the figure the progress line ALREADY prints. It is passed in
// for one purpose (AIRA-186): to keep the resolved reserve off the line when it is
// the number already there, while an unpinned job -- whose printed figure is a
// hint the daemon has already replaced -- is told the real one.
func confineQueueNote(ctx context.Context, deps confineDeps, request ConfineRequest, slicePath string, clientReserve int64, waitDone <-chan struct{}) (string, bool) {
	if deps.queuePosition == nil {
		return "", false
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
		return "", false
	}
	// AIRA-101. The exclusivity clause stands on its own: a job blocked behind a
	// drain often has NO queue position to report, and a position of "1 of 1"
	// explains nothing, so tying this clause to a valid position would hide the one
	// fact that actually explains the wait.
	exclusiveNote := ""
	switch position.exclusiveState {
	case "draining":
		exclusiveNote = ", slice draining for exclusive job " + describeExclusiveJob(position)
	case "held":
		exclusiveNote = ", slice held exclusively by " + describeExclusiveJob(position)
	}
	// AIRA-103/106. Stated before the position, standing on its own: it changes what
	// the position means. The cause is named from the basis, never assumed -- the
	// static machine-reserve term can bind on an idle box, so the MemAvailable
	// figure is printed only beside the system-pressure basis, where it is the
	// cause, and withheld from the others, where it would send a reader looking for
	// memory nothing is using.
	pressure := ""
	if position.ceilingThrottled {
		switch position.ceilingBasis {
		case "system-pressure":
			pressure = ", ceiling cut to keep system free memory"
			if position.memAvailable > 0 {
				pressure += " (MemAvailable " + FormatConfineMegabytes(position.memAvailable) + ")"
			}
		case "machine-reserve":
			pressure = ", ceiling cut to keep the machine-share reserve"
		default:
			pressure = ", ceiling cut below the configured ceiling"
		}
	}
	if position.position <= 0 || position.queued < position.position {
		return pressure + exclusiveNote, false
	}
	// "of N queued" is the daemon's evaluation order, not a promise of grant order:
	// a pushed job (AIRA-309) goes first and a smaller waiter can pass a stuck head
	// (AIRA-307). The figure the old line led with -- the reserve queued ahead -- is
	// gone; "granted/ceiling" is the number that says how full the slice is.
	return fmt.Sprintf(", %d of %d queued", position.position, position.queued) +
		confineHeldNote(position) + pressure + confineOwnReserveNote(position, clientReserve) + exclusiveNote, true
}

// confineHeldNote states what the ADMITTED jobs hold, against the ceiling one
// more job faces (AIRA-181). Both figures come from the same probe reply that
// carried the position, so this costs no round trip.
//
// It prints nothing at all when the daemon did not establish the figure. A zero it
// DID establish is printed as "0M granted", because on a slice that is closed for
// another reason -- a drain, a fairness freeze, a ceiling collapsed by outside
// pressure -- an empty running set is the single most useful thing the line can
// say. A ceiling that could not be established is left out rather than printed as
// "0M", which would state that the slice can admit nothing.
//
// Every byte figure on the line is whole MiB (FormatConfineMegabytes, the unit
// `aira top` uses), so the granted total and the ceiling compare at a glance.
func confineHeldNote(position confineQueuePosition) string {
	if !position.heldEstablished {
		return ""
	}
	held := FormatConfineMegabytes(position.heldBytes)
	if position.ceilingBytes > 0 {
		return fmt.Sprintf(", %s of %s granted", held, FormatConfineMegabytes(position.ceilingBytes))
	}
	return ", " + held + " granted"
}

// confineOwnReserveNote states this job's OWN resolved reserve (AIRA-186).
//
// Two different silences, on purpose: a reserve the daemon did not report is an
// absence and prints nothing (the client's own figure is NOT substituted -- while
// unpinned it is a hint the daemon has already replaced); a resolved reserve
// identical to the figure already on the line adds nothing.
//
// The one case that always speaks is a reserve larger than the ceiling itself: the
// job is blocked by its own size and not by the jobs ahead of it, so waiting is
// futile in a way ordinary contention is not.
func confineOwnReserveNote(position confineQueuePosition, clientReserve int64) string {
	if position.resolvedReserve <= 0 {
		return ""
	}
	if position.ceilingBytes > 0 && position.resolvedReserve > position.ceilingBytes {
		return fmt.Sprintf(", reserve %s exceeds the whole %s ceiling -- blocked by its own size; pin a smaller --memory-reserve",
			FormatConfineMegabytes(position.resolvedReserve), FormatConfineMegabytes(position.ceilingBytes))
	}
	if position.resolvedReserve == clientReserve {
		return ""
	}
	return ", reserve resolves to " + FormatConfineMegabytes(position.resolvedReserve)
}

// formatConfineWaitLine is the one place the periodic admission-wait line is
// composed, for the real and ci-shim paths alike (they differ only in verb and
// the advisory wording). It names the job and, when the daemon holds it queued,
// the minimal command an operator runs to push it to the front.
//
// The selector is the supervisor PID: a name need not be unique ("job" is the
// default for every unnamed launch, and a reused name is refused as ambiguous),
// whereas a PID is unique among live processes and short. `--steal` is part of the
// command because the person typing it is not the job's attested owner, and without
// the flag the push is refused (CheckConfineOwner).
func formatConfineWaitLine(verb, sliceName string, request ConfineRequest, reserve int64, pinned bool, waited int64, vramNote, queueNote string, queued bool) string {
	name, pid := request.Name, 0
	if parsedName, parsedPID, _, _, ok := parseConfineScopeID(request.ScopeID); ok {
		name, pid = parsedName, parsedPID
	}
	if name == "" {
		name = "job"
	}
	reserveText := "reserve " + FormatConfineMegabytes(reserve)
	if !pinned {
		// The daemon resolves the actual grant, so the client's figure is only an
		// estimate and says so.
		reserveText = "requested ~" + FormatConfineMegabytes(reserve) + " (unpinned; the daemon sizes the real grant)"
	}
	line := fmt.Sprintf("confine: %s on %s: %q %s%s, waited %ds%s", verb, sliceName, name, reserveText, vramNote, waited, queueNote)
	if queued && pid > 0 {
		line += fmt.Sprintf(". Push to front: aira confine --front %d --steal", pid)
	}
	return line + "\n"
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
	// AIRA-181. The running set's held reserve, from the SAME reply. Both figures
	// are refused as a PAIR when either is negative: a negative granted total or
	// job count is a ledger defect, and half of a self-contradictory pair on an
	// operator-facing line is worse than no clause at all.
	// AIRA-220. Establishment is the daemon's own GrantedEstablished bit, not
	// merely non-negative numbers: an absent ledger publishes a fabricated 0/0
	// that passes a `>= 0` gate, which is exactly how this note came to print
	// "0B already granted across 0 admitted jobs" on a slice with live jobs. The
	// `>= 0` sanity is kept as a defensive conjunct against a negative ledger
	// defect.
	if reserve.GrantedEstablished && reserve.GrantedBytes >= 0 && reserve.Jobs >= 0 {
		exclusive.heldBytes, exclusive.heldJobs, exclusive.heldEstablished = reserve.GrantedBytes, reserve.Jobs, true
	}
	// The ceiling is carried independently of that pair: it is a separate reading
	// and it is what AIRA-186's own-size comparison is made against, so a
	// ledger-side absence must not take it away too. Zero or negative is "not
	// established" and renders as nothing.
	if reserve.CeilingBytes > 0 {
		exclusive.ceilingBytes = reserve.CeilingBytes
	}
	// AIRA-186. Non-positive is an absence — an older daemon, or a waiter the
	// daemon did not match — never a reserve of zero.
	if reserve.ResolvedReserveBytes > 0 {
		exclusive.resolvedReserve = reserve.ResolvedReserveBytes
	}
	exclusive.position, exclusive.queued, exclusive.aheadBytes = reserve.QueuePosition, queued, ahead
	return exclusive, true
}
