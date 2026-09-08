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
//
// clientReserve is the figure the progress line ALREADY prints ahead of this
// clause. It is passed in for one purpose (AIRA-186): to keep the resolved
// reserve off the line when it is byte-for-byte the number already there, so a
// pinned job is not told twice, while an unpinned job — whose printed figure is
// a hint the daemon has already replaced — is told the real one.
func confineQueueNote(ctx context.Context, deps confineDeps, request ConfineRequest, slicePath string, clientReserve int64, waitDone <-chan struct{}) string {
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
	//
	// AIRA-181 closes the gap that wording could only warn about. The warning was
	// correct and insufficient: naming what the figure is NOT left the reader to
	// supply the figure it is, and four sessions supplied "nothing". The two
	// clauses now sit together, each naming its own population in its own words —
	// "queued ahead" for the waiters, "already granted across N admitted jobs"
	// for the running set — so neither number can be mistaken for the other and
	// the larger one is no longer missing. The vocabulary is deliberately
	// `confine --list`'s own ("granted", "admitted jobs", "ceiling"), so an
	// operator who cross-checks with that command reads the same nouns.
	return pressure +
		fmt.Sprintf(", queue position %d of %d by enqueue order, %s queued ahead", position.position, position.queued, ahead) +
		confineHeldNote(position) + confineOwnReserveNote(position, clientReserve) + exclusiveNote
}

// confineHeldNote states what the ADMITTED jobs hold, against the ceiling one
// more job faces (AIRA-181). Both figures come from the same probe reply that
// carried the position, so this costs no round trip.
//
// It prints nothing at all when the daemon did not establish the pair. A zero
// it DID establish is printed as "0B ... across 0 admitted jobs", because on a
// slice that is closed for some other reason — a drain, a fairness freeze, a
// ceiling collapsed by outside pressure — an empty running set is the single
// most useful thing the line can say, and suppressing it would leave the
// operator with the same "nothing is blocking me" reading this clause exists to
// end.
//
// ACCEPTED GAP, stated rather than left for a reader to discover. Both figures
// go through FormatConfineBytes, which picks whichever of T/G/M/K divides the
// value EXACTLY and otherwise prints raw bytes. A granted total is a sum of live
// ledger charges and is essentially never round in any unit, so this clause
// routinely renders as an eleven-digit integer beside a rounded ceiling —
// verified live: "54116871208 already granted across 5 admitted jobs / 52608M
// slice ceiling". That still fixes what AIRA-181 is about (the blocker is now
// NAMED, where before the line said only "0B queued ahead"), but it makes the
// magnitude comparison harder than it should be on a line whose whole purpose is
// being read at a glance.
//
// It is deliberately not fixed here. The remedy is the one the owner already
// chose for the same problem on `aira top` — one fixed unit, rounded
// (topFormatMegabytes, cmd/aira/tui_top.go) — and applying it to this line means
// also moving AIRA-24's existing "queued ahead" figure, since two unit systems
// in one sentence would be worse than either. That is a decision about what unit
// the admission-wait line speaks, not part of the reporting these two tickets
// asked for, so it is filed rather than smuggled in. See AIRA-193.
func confineHeldNote(position confineQueuePosition) string {
	if !position.heldEstablished {
		return ""
	}
	held := "0B"
	if position.heldBytes > 0 {
		held = FormatConfineBytes(position.heldBytes)
	}
	jobs := "jobs"
	if position.heldJobs == 1 {
		jobs = "job"
	}
	note := fmt.Sprintf(", %s already granted across %d admitted %s", held, position.heldJobs, jobs)
	// A ceiling that could not be established is left out rather than printed as
	// "0B", which would state that the slice can admit nothing.
	if position.ceilingBytes > 0 {
		note += " / " + FormatConfineBytes(position.ceilingBytes) + " slice ceiling"
	}
	return note
}

// confineOwnReserveNote states this job's OWN resolved reserve against that same
// ceiling (AIRA-186).
//
// Two different silences, on purpose:
//
//   - A reserve the daemon did not report is an absence and prints nothing. The
//     client's own figure is NOT substituted: while unpinned it is a hint the
//     daemon has already replaced, and printing it as "this job's own reserve"
//     would name a number nothing is gating on — the fabrication this ticket is
//     about, restated in the fix.
//   - A resolved reserve identical to the figure already on the line (every
//     pinned request) adds nothing, so it is not repeated. The comparison it
//     would support is already available: the pinned figure is at the head of
//     the line and the ceiling is in the clause above.
//
// The one case that always speaks is a reserve larger than the ceiling itself.
// That is reachable while queued even though admission refuses `reserve >
// ceiling` outright at enqueue: the ceiling is not fixed, and falls under
// outside memory pressure or as more jobs are admitted and each takes its own
// headroom. It is also the one state where waiting is futile in a way ordinary
// contention is not, and it is exactly the distinction AIRA-186 asks the wait
// site to make — so it is stated in full, with its own numbers, even when the
// figure duplicates one already on the line.
func confineOwnReserveNote(position confineQueuePosition, clientReserve int64) string {
	if position.resolvedReserve <= 0 {
		return ""
	}
	// "resolves to" is the news that the daemon chose a different number from the
	// one printed at the head of the line; "is" states a figure the caller pinned
	// itself. Saying "resolves to" for a pinned request would imply the daemon
	// moved a number it honours verbatim.
	verb := "is"
	if position.resolvedReserve != clientReserve {
		verb = "resolves to"
	}
	if position.ceilingBytes > 0 && position.resolvedReserve > position.ceilingBytes {
		return fmt.Sprintf(", this job's own reserve %s %s — larger than the whole %s slice ceiling, so it is blocked by its own size and not by the jobs ahead of it; pin a smaller --memory-reserve",
			verb, FormatConfineBytes(position.resolvedReserve), FormatConfineBytes(position.ceilingBytes))
	}
	if position.resolvedReserve == clientReserve {
		return ""
	}
	return fmt.Sprintf(", this job's own reserve %s %s", verb, FormatConfineBytes(position.resolvedReserve))
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
	if reserve.GrantedBytes >= 0 && reserve.Jobs >= 0 {
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
