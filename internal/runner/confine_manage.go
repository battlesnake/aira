package runner

import (
	"context"
	"time"
)

const (
	CodeConfineOwnerUnverified = "E_CONFINE_OWNER_UNVERIFIED"
	CodeConfineNotFound        = "E_CONFINE_NOT_FOUND"
	CodeConfineNotLaunched     = "U_CONFINE_NOT_LAUNCHED"
	CodeConfineKillUnconfirmed = "U_CONFINE_KILL_UNCONFIRMED"
)

// ConfineRegistryEntry names a scope with a held daemon admission lease. It
// never establishes filesystem existence — its only job is to surface a scope
// that is admitted but not yet on disk, as a Pending row.
//
// It used to carry Name and Owner too. Both are now decoded from the scope
// directory name instead (AIRA-52), which is authoritative and, unlike the
// daemon's in-memory waiter list, survives a daemon restart.
type ConfineRegistryEntry struct {
	ScopeID string `json:"scope_id"`
}

// ConfineRecord preserves per-field uncertainty with nil values. A nil facet
// is rendered as "unevaluated" by human faces and as JSON null.
type ConfineRecord struct {
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	SupervisorPID *int   `json:"supervisor_pid"`
	ScopeID       string `json:"scope_id"`
	Populated     *int   `json:"populated"`
	RSSBytes      *int64 `json:"rss_bytes"`
	// SubtreePopulated is liveness read from cgroup.events `populated`, which is
	// SUBTREE-aware, unlike Populated above (leaf cgroup.procs only). AIRA-101
	// needs the distinction and it is not cosmetic: BootstrapAitestSupervisor
	// drains EVERY pid out of an aitest outer scope into <outer>/.aira-supervisor,
	// so a fully busy suite reads Populated == 0 while SubtreePopulated is true.
	// Reading a running job as empty is how an exclusive benchmark would be handed
	// a fabricated "you are alone".
	//
	// nil means the reading could not be established (the scope vanished mid-scan,
	// or cgroup.events could not be opened) and must never be rendered as empty.
	// killConfine already used this same source for the same reason; this only
	// makes it available to the scan.
	SubtreePopulated  *bool    `json:"subtree_populated"`
	AgeSeconds        *int64   `json:"age_seconds"`
	Cap               *string  `json:"cap"`
	Pending           bool     `json:"pending,omitempty"`
	UnevaluatedFields []string `json:"unevaluated_fields,omitempty"`
}

type ConfineListResult struct {
	Verdict      string               `json:"verdict"`
	Reason       string               `json:"reason,omitempty"`
	Scopes       []ConfineRecord      `json:"scopes"`
	SliceReserve *ConfineSliceReserve `json:"slice_reserve,omitempty"`
}

type ConfineSliceReserve struct {
	GrantedBytes int64 `json:"granted_bytes"`
	CeilingBytes int64 `json:"ceiling_bytes"`
	Jobs         int   `json:"jobs"`
	// Queued and FreezePhase answer "what is stuck, and why" for the admission
	// queue. Root-causing AIRA-59 required source reading precisely because
	// `confine --list` reported only ADMITTED jobs: nothing surfaced waiters that
	// were queued but ungranted, nor that a fairness freeze was holding them.
	// A queue with no waiters reports a known zero and phase "idle" — never
	// "unevaluated", which is reserved for state that could not be established.
	Queued      int    `json:"queued"`
	FreezePhase string `json:"freeze_phase,omitempty"`

	// AIRA-24. QueuePosition and QueuedAheadBytes answer ONE waiter's own
	// question — "where am I in that queue?" — which Queued above cannot: a
	// job blocked in admission for half an hour could see how many were
	// waiting but never whether it was next or last. They are populated ONLY
	// when the caller named its own scope id (the `scope_id` argument), and
	// only while that scope is still a QUEUED waiter; a granted, released, or
	// unknown scope id leaves both zero. `aira confine --list` never passes a
	// scope id, so its output is unchanged.
	//
	// Both are derived in the SAME locked pass as Queued, so position, total,
	// and bytes-ahead can never describe different instants.
	//
	// Position is an index in the daemon's EVALUATION order (by enqueue
	// sequence), not a promise of grant order: the AIRA-59 fairness duty
	// cycle's yield phase can admit a later, smaller waiter that fits while
	// the head is still too large. QueuedAheadBytes is the sum of the reserves
	// of the queued waiters ahead — a fact about the queue, never an ETA,
	// which the daemon cannot establish.
	//
	// Zero means "no position established", never "position zero": a reader
	// must render its absence as an absence and print nothing.
	QueuePosition    int   `json:"queue_position,omitempty"`
	QueuedAheadBytes int64 `json:"queued_ahead_bytes,omitempty"`

	// AIRA-68. Jobs and GrantedBytes above are TOTALS over three structurally
	// different populations, and only two of them can ever appear as a row in the
	// Scopes table:
	//
	//   ScopeJobs        connection-held `aira confine` jobs        -> a row
	//   ReservationJobs  connection-held `aira confine-reserve`
	//                    per-test reservations, which create NO
	//                    cgroup scope at all                        -> NO row
	//   AdoptedJobs      scopes adopted by the daemon's scan        -> a row
	//
	// Comparing Jobs against len(Scopes) is therefore invalid, and doing so is
	// what produced AIRA-68's P0 "23 admitted jobs, only 3 live scopes" report:
	// 20 of those were healthy per-test reservations from a running
	// --delegate-ram pytest suite. Jobs > len(Scopes) is the EXPECTED shape while
	// such a suite runs. Consumers wanting "how much is charged to something with
	// a scope" want ScopeBytes + AdoptedBytes, never GrantedBytes.
	ScopeJobs        int   `json:"scope_jobs"`
	ScopeBytes       int64 `json:"scope_bytes"`
	ReservationJobs  int   `json:"reservation_jobs"`
	ReservationBytes int64 `json:"reservation_bytes"`
	AdoptedJobs      int   `json:"adopted_jobs"`
	AdoptedBytes     int64 `json:"adopted_bytes"`

	// VanishedJobs/VanishedBytes are a SUBSET of ScopeJobs/ScopeBytes: leases
	// whose scope the daemon's own scan observed and then observed gone. They are
	// reclaimed at the stale-lease TTL.
	//
	// Named for what was observed, never for a verdict. A scope can be empty and
	// removed while the job's leader is still alive, having migrated into a
	// sibling cgroup — real, witnessed behaviour — so "its scope is gone" is a
	// fact and "the job is dead" is not one this daemon can establish.
	//
	// Structurally blind to the scope-less population: a reservation has no cgroup
	// artifact, so nothing can be observed about it either way.
	VanishedJobs  int   `json:"vanished_jobs"`
	VanishedBytes int64 `json:"vanished_bytes"`

	// AIRA-103. WHY the ceiling is what it is. CeilingBytes above already derives
	// from the EFFECTIVE maximum, so it is honest without these; they exist so an
	// operator waiting on admission can tell external system memory pressure from
	// "the slice is simply full of AIRA's own jobs", which the numbers above
	// cannot distinguish.
	//
	// CeilingMode is "" when the subsystem is off, and then every other field
	// here is meaningless and must not be rendered. CeilingState is
	// "unthrottled" | "throttled" | "unevaluated" -- the last reserved for a
	// ceiling that could NOT be established and which must never render as a
	// number. A zero CeilingStaticBytes or MemAvailableBytes is likewise an
	// absence, never a measured zero.
	// CeilingHeld marks a ceiling whose numbers are the LAST ESTABLISHED ones,
	// not current readings: the newest sample failed and the hold TTL has not yet
	// expired. Every surface must say so rather than presenting a stale
	// MemAvailable as the current one.
	//
	// CeilingWouldBeBytes is what the ceiling WOULD be if applied. In enforce
	// mode it equals CeilingBytes; in observe mode it is the counterfactual,
	// because observe applies nothing and CeilingBytes there is the untouched
	// static capacity.
	// CeilingBasis (AIRA-106) names WHICH policy term reduced a THROTTLED ceiling:
	// "machine-reserve" (the static "leave this much of the machine outside the
	// slice" bound) or "system-pressure" (memory used outside the slice right
	// now). Empty in every other state. It exists because the two causes call for
	// opposite operator responses, and because the pre-AIRA-106 wording asserted
	// the pressure cause unconditionally — which the static term makes false on an
	// otherwise idle machine.
	CeilingMode         string `json:"ceiling_mode,omitempty"`
	CeilingState        string `json:"ceiling_state,omitempty"`
	CeilingBasis        string `json:"ceiling_basis,omitempty"`
	CeilingReason       string `json:"ceiling_reason,omitempty"`
	CeilingHeld         bool   `json:"ceiling_held,omitempty"`
	CeilingStaticBytes  int64  `json:"ceiling_static_bytes,omitempty"`
	CeilingWouldBeBytes int64  `json:"ceiling_would_be_bytes,omitempty"`
	MemAvailableBytes   int64  `json:"mem_available_bytes,omitempty"`

	// ResidualJobs/ResidualBytes cross-check the derived split against the
	// daemon's incremental counters. They are equal by construction, so a
	// non-zero value here is a real lost or double ledger discharge. Signed:
	// negative means more was discharged than was ever charged, which is just as
	// real a defect as the positive direction.
	ResidualJobs  int   `json:"residual_jobs"`
	ResidualBytes int64 `json:"residual_bytes"`

	// Exclusive is AIRA-101's slice-exclusivity state, derived in the SAME locked
	// pass as every count above so an operator can never be shown an exclusive
	// holder alongside figures from a different instant.
	//
	// nil means NO exclusivity is active. That is a POSITIVE fact established by
	// the same walk, never an unevaluated reading, and consumers must render it as
	// "none" rather than as unknown — an operator whose job is blocked needs to be
	// able to rule a benchmark out, not merely fail to see one.
	Exclusive *ConfineExclusiveState `json:"exclusive,omitempty"`

	// AIRA-108. The scope-less population, NAMED. ReservationJobs/ReservationBytes
	// above are an aggregate, and an aggregate is exactly what could not settle
	// AIRA-108: 5.5 GB of a shared 62 GB machine-wide ceiling was pinned by
	// "5 scope-less reservations" and no AIRA surface could say by WHAT.
	//
	// That mattered because a GRANTED `aira confine-reserve` helper and one still
	// WAITING for admission are byte-identical in `ps` — same argv, same
	// `--max-wait 300s` — so an operator who saw a long-lived helper could not
	// establish which of the two states it was in, and the only inference left was
	// the wrong one: that it had blown its declared bound. (It had not: a granted
	// reservation is held until its stdin closes, by design, for the whole life of
	// the test it was granted for.) Two sessions spent hours at /proc level and
	// filed a P0 that was not there. AIRA-68's comment above records the FIRST
	// false P0 from this same blind spot; the aggregate line it added is
	// demonstrably not sufficient on its own.
	//
	// Every row is a GRANTED, accounted, scope-less waiter — EXACTLY the
	// population ReservationJobs/ReservationBytes already count, no wider and no
	// narrower — derived in the same locked pass as every count above, so rows and
	// totals always describe one instant. Truncated to the longest-held few (the
	// renderer says how many were elided); an empty slice is a real "none", never
	// an unevaluated read, for the same reason Exclusive's nil is.
	//
	// "Scope-less" is a structural fact, not a verb: `aira confine-reserve` is
	// overwhelmingly what lands here, but any admission that creates no cgroup
	// scope does — `aira run` among them. The rows therefore say what the daemon
	// KNOWS (granted, this big, held this long, under this signature) and never
	// name a verb it did not observe. This is the pre-existing shape of the
	// aggregate above; the rows make it visible rather than change it.
	Reservations []ConfineReservationHold `json:"reservations,omitempty"`
}

// ConfineReservationHold is one scope-less admission the daemon is holding right
// now — typically an `aira confine-reserve --pinned` per-test RAM lease.
type ConfineReservationHold struct {
	// State is always "holding" today, and is nevertheless carried on the wire
	// rather than left implicit. A row that does not SAY what it is leaves the
	// reader to infer it from the section it was printed under, and inferring
	// state from context is the exact failure this whole field exists to end.
	State string `json:"state"`
	// Signature is client-supplied text (`pytest:<nodeid>` for the per-test
	// governor). It is UNTRUSTED and must be escaped and length-limited by every
	// renderer — it reaches a terminal.
	Signature string `json:"signature,omitempty"`
	Reserve   int64  `json:"reserve"`
	// HeldMS is milliseconds since the grant was delivered — the number that
	// answers "is this stuck?". A duration, never a wall-clock instant, so a
	// reader never has to reconcile the daemon's clock against its own.
	HeldMS int64 `json:"held_ms"`
}

// ConfineReservationStateHolding is the only state a reservation row carries
// today: the daemon has granted this reserve and is holding it until the client
// drops its connection. It is spelled out on the wire rather than implied so a
// later state cannot silently inherit this one's meaning.
const ConfineReservationStateHolding = "holding"

// ConfineReservationSignatureLimit bounds a rendered signature. A pytest nodeid
// is already long and is entirely client-supplied, so the terminal boundary
// truncates rather than trusting it to be reasonable.
const ConfineReservationSignatureLimit = 96

// ConfineReservationRowLimit is how many reservation rows a renderer prints. The
// rest are reported as a count, never dropped silently: a suite with 40 workers
// must not turn `confine --list` into a wall of text, and must not be able to
// hide the oldest hold either — which is why the rows kept are the LONGEST-HELD.
const ConfineReservationRowLimit = 10

// ConfineExclusiveState names which job holds (or is draining toward) exclusive
// use of a slice, so a blocked operator learns a benchmark is running rather
// than concluding the slice is merely full.
type ConfineExclusiveState struct {
	// State is "draining" (stopping new admissions, waiting for running jobs to
	// finish) or "held" (the exclusive job is running alone).
	State   string `json:"state"`
	Name    string `json:"name,omitempty"`
	Owner   string `json:"owner,omitempty"`
	ScopeID string `json:"scope_id,omitempty"`
	// WaitingJobs counts the queued waiters actually held up behind it. It never
	// counts the exclusive job as waiting for itself.
	WaitingJobs int `json:"waiting_jobs"`
}

type ConfineKillResult struct {
	Status  string `json:"status"`
	ScopeID string `json:"scope_id"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
}

type ConfineReapResult struct {
	Verdict string   `json:"verdict"`
	Reason  string   `json:"reason,omitempty"`
	Reaped  []string `json:"reaped"`
	Skipped int      `json:"skipped"`
}

// orphanedConfineScopeCandidates requires positive proof for every orphan
// facet. Unknown population, supervisor, or age state is never a candidate. A
// scope with a live daemon admit lease (hasLiveLease) is NEVER a candidate: that
// is the authoritative, PID-namespace-independent liveness signal — kill(pid,0)
// alone can misjudge a supervisor whose scope-id PID is namespace-local.
func orphanedConfineScopeCandidates(records []ConfineRecord, grace time.Duration, supervisorDead func(pid int) bool, hasLiveLease func(scopeID string) bool) []ConfineRecord {
	graceSeconds := int64(grace / time.Second)
	candidates := make([]ConfineRecord, 0)
	if supervisorDead == nil {
		return candidates
	}
	for _, record := range records {
		if record.Populated == nil || *record.Populated != 0 ||
			record.SupervisorPID == nil || !supervisorDead(*record.SupervisorPID) ||
			record.AgeSeconds == nil || *record.AgeSeconds < graceSeconds ||
			record.Pending ||
			(hasLiveLease != nil && hasLiveLease(record.ScopeID)) {
			continue
		}
		candidates = append(candidates, record)
	}
	return candidates
}

func ListConfines(ctx context.Context, slicePath string, registry []ConfineRegistryEntry) (ConfineListResult, error) {
	return listConfines(ctx, slicePath, registry)
}

func KillConfine(ctx context.Context, slicePath, selector, callerOwner string, steal bool, registry []ConfineRegistryEntry) (ConfineKillResult, error) {
	return killConfine(ctx, slicePath, selector, callerOwner, steal, registry, 2*time.Second)
}
