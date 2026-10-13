package runner

import (
	"context"
	"time"
)

// AIRA-308. QUEUED confine jobs as listing rows.
//
// `confine --list` and `aira top` used to know only a COUNT of the waiters in the
// admission queue (ConfineSliceReserve.Queued): the jobs themselves, the ones an
// operator is waiting on and most wants to name, were not rows. The daemon now
// copies them out of its queue in the same locked pass that counts them and
// publishes them as ConfineListResult.Queued.
//
// They are a SEPARATE list from Scopes on purpose. A queued job holds no cgroup
// scope, no memory and no lease, so every consumer that reads Scopes as "what is
// running" — the kill selectors, the orphan reaper, the RAM/CPU/VRAM bar maths,
// `confine --list`'s table — is untouched by construction rather than by each
// having to remember to skip a flag.

// ConfineQueuedRowLimit bounds how many queued rows one reply carries. It exists
// for AVAILABILITY, on the same reasoning as ConfineCommandWireLimit: each row
// carries a command of up to ConfineCommandWireLimit bytes, and an unbounded list
// could push a reply past MaxFrameBytes, at which point `--list` stops working for
// every job on the slice. Truncation is never silent: ConfineSliceReserve.Queued
// stays the true total, so a renderer can say "showing N of M".
const ConfineQueuedRowLimit = 64

// ConfineQueuedEntry is one queued waiter as the daemon's locked admission
// snapshot copied it out: the scope id the client minted (which carries name, pid,
// launch stamp and owner — AIRA-52) and the two figures only the daemon knows.
type ConfineQueuedEntry struct {
	ScopeID string
	// ReserveBytes is the waiter's RESOLVED requested reserve — the figure
	// admission is gating on (AIRA-186), not a prior and not a charge: nothing is
	// charged to a queued job yet.
	ReserveBytes int64
	// VRAMBytes is its declared --vram request (0 = "declared no VRAM").
	VRAMBytes int64
	// Pushed is whether an operator pushed this waiter to the front (AIRA-309).
	Pushed bool
}

// queuedConfineRecord builds the facets of a queued row that need no live read,
// and the supervisor pid its live facets are read through. ok=false when the scope
// id does not decode, in which case no row can be established and none is made.
//
// AgeSeconds runs from the scope id's launch stamp — the same anchor a LIVE row's
// AGE uses — rather than from the daemon's enqueue time. For a job that is
// waiting the two differ by well under a second, but the launch stamp is
// daemon-independent (a restarted daemon re-enqueues waiters and would restart an
// enqueue clock) and it makes a row's AGE continuous across the moment the job is
// admitted and moves from this list to Scopes.
//
// Every cgroup-derived facet is named unevaluated: there is no scope to read, and
// printing zeros would state that the job is idle and uncapped. supervisor_live is
// named too, on mergeConfineRegistry's grounds: nothing has read it.
func queuedConfineRecord(entry ConfineQueuedEntry, now time.Time) (ConfineRecord, int, bool) {
	name, pid, stamp, owner, ok := parseConfineScopeID(entry.ScopeID)
	if !ok {
		return ConfineRecord{}, 0, false
	}
	if owner == "" {
		owner = ConfineUnknownOwner
	}
	reserve, vram := entry.ReserveBytes, entry.VRAMBytes
	record := ConfineRecord{
		Name: name, Owner: owner, ScopeID: entry.ScopeID, SupervisorPID: &pid,
		Worker:       IsAitestWorkerScopeName(name), // classified exactly as a live row is
		ReserveBytes: &reserve, VRAMBytes: &vram, Pushed: entry.Pushed,
		UnevaluatedFields: []string{"populated", "rss", "cap", "cpu", "supervisor_live"},
	}
	if age := now.Sub(time.Unix(0, stamp)); age >= 0 {
		seconds := int64(age / time.Second)
		record.AgeSeconds = &seconds
	} else {
		record.UnevaluatedFields = append(record.UnevaluatedFields, "age")
	}
	return record, pid, true
}

// ShimQueuedConfines builds queued rows with NO live read of any kind: command,
// cwd and pane are named unevaluated beside the cgroup facets. It is the ci-shim
// daemon's path (there is no /proc to trust there, exactly as for ShimConfineList)
// and the non-Linux build's.
func ShimQueuedConfines(entries []ConfineQueuedEntry) []ConfineRecord {
	return shimQueuedConfines(entries, time.Now())
}

func shimQueuedConfines(entries []ConfineQueuedEntry, now time.Time) []ConfineRecord {
	var rows []ConfineRecord
	for _, entry := range entries {
		record, _, ok := queuedConfineRecord(entry, now)
		if !ok {
			continue
		}
		record.UnevaluatedFields = append(record.UnevaluatedFields, "command", "cwd", "pane")
		rows = append(rows, record)
	}
	return rows
}

// ListQueuedConfines builds queued rows, reading each supervisor's command, cwd
// and tmux window live exactly as ListConfines does for a scope. entries are taken
// in the order given (the daemon's evaluation order) and the result keeps it; a
// nil result means no row could be established, never a fabricated one.
func ListQueuedConfines(ctx context.Context, entries []ConfineQueuedEntry) []ConfineRecord {
	return listQueuedConfines(ctx, entries)
}
