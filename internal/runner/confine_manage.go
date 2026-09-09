package runner

import (
	"context"
	"strings"
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
	SubtreePopulated *bool `json:"subtree_populated"`
	// SupervisorLive is whether the process that LAUNCHED this scope still
	// exists — a different question from SubtreePopulated above, and the one that
	// separates the two states an empty subtree used to conflate (AIRA-183):
	//
	//   - true with an empty subtree: the supervisor is alive and the scope is
	//     merely between processes (mid fork/exec, or a legitimately idle moment).
	//     Nothing is wrong and nothing is going to be reaped.
	//   - false with an empty subtree: the supervisor is GONE. The scope is
	//     orphaned and the daemon's reaper removes it once it is past the reap
	//     grace. This is the state an operator was reading as "the kill did not
	//     work" or "something leaked".
	//
	// It comes from the same kill(pid, 0) signal the orphan reaper's own
	// supervisorDead predicate uses (orphanedConfineScopeCandidates), so the
	// listing and the reaper cannot disagree about who is dead.
	//
	// nil is "could not be established", never "dead": an unreadable or
	// ambiguous supervisor reading must not be rendered as an orphan, because
	// that is precisely the reading an operator acts on by killing something.
	// Two things produce nil deliberately: a signal error that is neither ESRCH
	// nor EPERM, and — the load-bearing one — a scope whose supervisor probes
	// dead while the daemon still holds a live admit lease for it. That
	// disagreement is exactly what a PID-namespace-local supervisor PID looks
	// like from outside its namespace, and the reaper already refuses to reap on
	// it; the listing correspondingly refuses to call it orphaned.
	SupervisorLive *bool   `json:"supervisor_live"`
	AgeSeconds     *int64  `json:"age_seconds"`
	Cap            *string `json:"cap"`
	// Command is the WRAPPED invocation this scope was created for — the argv
	// after `aira confine`'s own `--` separator — read live from
	// /proc/<SupervisorPID>/cmdline at listing time, exactly as RSSBytes and Cap
	// above are read live from the scope's cgroup files (AIRA-135).
	//
	// nil is "could not be established", never "no command": the supervisor may
	// have exited between the directory scan and this read, /proc may be
	// unreadable, or the platform may have no /proc at all. It is never filled in
	// with a placeholder.
	//
	// KNOWN GAP, documented rather than papered over: SupervisorPID is decoded
	// from the scope DIRECTORY NAME, so between a supervisor's death and the
	// orphan reaper's sweep a reused PID could carry an unrelated process's argv.
	// That is exactly the trust level the PID field beside it already has (and
	// that killConfine's pid selector already accepts); this field inherits it and
	// widens nothing, and it participates in no decision — it is a display facet.
	Command *string `json:"command"`
	// CPUUsageUsec is the scope's CUMULATIVE cpu.stat `usage_usec` — total CPU
	// time charged to this cgroup and its descendants since the cgroup was
	// created, in microseconds — read live at listing time exactly as RSSBytes
	// (memory.current) and Cap (memory.max) beside it are (AIRA-137).
	//
	// It is a COUNTER, not a rate. There is no live "% CPU" anywhere in a cgroup;
	// a rate needs two of these and the wall-clock interval between them, which is
	// why ConfineSliceReserve.CPUSampleUnixNano travels with it. A consumer that
	// renders this number directly is rendering "CPU-seconds since boot", not
	// load, and no face in this repo does.
	//
	// nil is "could not be established" (the scope vanished mid-scan, cpu.stat was
	// unreadable, or the kernel published no usage_usec key), never a measured
	// zero — a scope that has genuinely used no CPU publishes usage_usec 0, and
	// collapsing the two would draw an unreadable scope as an idle one.
	CPUUsageUsec *int64 `json:"cpu_usage_usec"`
	// ReserveBytes is what the daemon's admission ledger CHARGES this scope right
	// now — its share of the `slice reserve: <granted>` total the same reply
	// carries (AIRA-191, AIRA-192). It is established only by
	// ApplyConfineScopeReserves, from the daemon's own locked admission snapshot,
	// and never by the cgroupfs scan that fills every field above it.
	//
	// It is NOT Cap, and that distinction is the whole reason this field exists. A
	// `--delegate-ram` scope's memory.max is an AIRA-15 containment CEILING sized
	// to give a whole framework's workers room, deliberately far wider than the
	// pinned framework reserve the ledger charges it (admit.go's own words: "a
	// delegate scope's memory.max is its scope ceiling, not its pinned framework
	// reserve, and that is the largest cap population on the machine"). Summing
	// caps to answer "how much of the slice is reserved" therefore over-states the
	// claim by tens of gigabytes on a machine running a delegate suite, which is
	// exactly what `aira top` did until AIRA-192: it drew ~93 GiB of claims on an
	// 80 GiB machine while the ledger's own line read 40 GiB granted of a 53 GiB
	// ceiling. Nor is it the frozen grant payload: since AIRA-29 a scope's charge
	// is re-derived from live usage, and it is the CHARGE, not the grant, that
	// occupies the slice and gates the next admission.
	//
	// nil is "the daemon holds no admission record for this scope" — a job it lost
	// track of across a restart, one whose reserve could not be reconstructed, or
	// any listing built without the daemon at all — and every renderer must say
	// unevaluated. Falling back to Cap is forbidden: it IS the defect. Zero, by
	// contrast, is a REAL established charge and must not collapse into absence,
	// which is why this is a pointer rather than an int64 with a sentinel.
	//
	// Summed over one listing's rows it reconciles with that listing's own
	// ScopeBytes + AdoptedBytes, up to the one-scan-interval skew the adopted
	// ledger already documents.
	ReserveBytes      *int64   `json:"reserve_bytes"`
	Pending           bool     `json:"pending,omitempty"`
	UnevaluatedFields []string `json:"unevaluated_fields,omitempty"`
}

// ConfineReserveFacet is the UnevaluatedFields name for an unestablished
// per-scope reserve, spelled once so the producer and every consumer cannot
// drift.
const ConfineReserveFacet = "reserve"

// ApplyConfineScopeReserves stamps the daemon's per-scope ledger charges onto a
// listing's records, matched by scope id.
//
// It is the ONE place ConfineRecord.ReserveBytes is ever set, and it is called on
// EVERY path that produces a listing — including the daemon-down client fallback,
// which passes a nil map. That uniformity is the point: a record that has been
// through this function either carries a reserve the daemon established or names
// `reserve` as unevaluated, so no consumer is left to infer which of the two it
// is holding, and none of them has any reason to invent a fallback.
//
// reserves is a snapshot the caller has already copied out from under the
// daemon's queue lock; this function takes no lock and makes no decision.
//
// covers: AIRA-191
// covers: AIRA-192
func ApplyConfineScopeReserves(scopes []ConfineRecord, reserves map[string]int64) {
	for index := range scopes {
		record := &scopes[index]
		if value, ok := reserves[record.ScopeID]; ok && record.ScopeID != "" {
			// An established reading also clears any earlier "unevaluated" mark, so a
			// second pass over the same records cannot leave a record contradicting
			// itself.
			charge := value
			record.ReserveBytes = &charge
			record.UnevaluatedFields = withoutConfineFacet(record.UnevaluatedFields, ConfineReserveFacet)
			continue
		}
		record.ReserveBytes = nil
		record.UnevaluatedFields = withConfineFacet(record.UnevaluatedFields, ConfineReserveFacet)
	}
}

func withConfineFacet(facets []string, facet string) []string {
	for _, existing := range facets {
		if existing == facet {
			return facets
		}
	}
	return append(facets, facet)
}

// withoutConfineFacet returns a NEW slice when it has to drop something, and the
// original untouched when it does not. Compacting in place would be shorter and
// would quietly rewrite whatever else shares that backing array — a hazard with
// no upside on a list of at most a handful of short strings.
func withoutConfineFacet(facets []string, facet string) []string {
	kept := make([]string, 0, len(facets))
	for _, existing := range facets {
		if existing != facet {
			kept = append(kept, existing)
		}
	}
	if len(kept) == len(facets) {
		return facets
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// ConfineCPUFrame is the SYSTEM-and-SLICE CPU counter pair, read as one sample
// with one timestamp (AIRA-137). It is the exact CPU analogue of the
// SystemMemTotal/SliceCurrent pair the RAM bar's "rest of system" region is
// derived from: rest-of-system CPU is a ROOT-cgroup aggregate minus the SLICE's
// own aggregate, never a sum over the individually listed scopes.
//
// Every counter is cumulative microseconds. Each has its own `Known` bit because
// the two reads fail independently — an unreadable slice cpu.stat must not also
// erase a root reading the same pass did establish — and because zero is a legal
// counter value that must never stand in for "unreadable".
//
// SampleUnixNano is the instant of the sample, taken by the SERVER between the
// two reads. It is on the wire because the client cannot substitute its own tick
// time: fetch, decode and render latency skew a client-side clock by tens of
// milliseconds against a one-second tick, which is a percent-level error on
// every rate derived from it. Zero means no sample instant was established, and
// then no rate may be computed from these counters at all.
type ConfineCPUFrame struct {
	SystemUsageUsec int64
	SystemKnown     bool
	SliceUsageUsec  int64
	SliceKnown      bool
	SampleUnixNano  int64
}

// ConfineCommandWireLimit bounds the bytes of /proc/<pid>/cmdline the scan reads
// and retains. It exists for AVAILABILITY, not neatness, on the same reasoning as
// ConfineReservationSignatureWireLimit below: a process's argv area can run to
// megabytes, and an unbounded copy of one per scope could push a `confine --list`
// reply past MaxFrameBytes — at which point `--list` fails for every job on the
// slice. Elision is MARKED (see confineCommandFromCmdline), never silent.
const ConfineCommandWireLimit = 4096

// confineCommandFromCmdline extracts the wrapped command from an `aira confine`
// supervisor's NUL-separated /proc/<pid>/cmdline.
//
// `aira confine`'s CLI syntax is always `confine [flags] -- <argv...>`, so the
// displayed command is everything AFTER the first bare `--`: aira's own flags are
// not what an operator scanning the table is looking for. When there is no `--`
// at all — which a real supervisor should never produce — the WHOLE argv is
// returned rather than the field being silently dropped.
//
// ok=false means nothing could be established: an empty cmdline (the process
// exited under the read, or is a kernel thread), or a `--` with nothing after it.
// Neither is ever rendered as an empty command.
//
// The argv is joined with single spaces. That is a RENDERING, not a shell-quoted
// round trip: an argument containing whitespace is not re-quoted, so the result
// is a faithful list of the real arguments and not something to paste back into a
// shell. Terminal-safety escaping belongs to the renderer, at the one boundary
// that actually reaches a terminal, exactly as it does for a reservation
// signature.
func confineCommandFromCmdline(data []byte) (string, bool) {
	elided := false
	if len(data) > ConfineCommandWireLimit {
		data = data[:ConfineCommandWireLimit]
		elided = true
	}
	argv := strings.Split(string(data), "\x00")
	// A cmdline is NUL-TERMINATED, so the split yields exactly ONE artefactual
	// trailing empty element. Exactly one is dropped: an empty ARGUMENT is legal
	// (`sh -c ""`), and trimming every trailing empty would delete a real one.
	if len(argv) > 0 && argv[len(argv)-1] == "" {
		argv = argv[:len(argv)-1]
	}
	if len(argv) == 0 {
		return "", false
	}
	for index, arg := range argv {
		if arg != "--" {
			continue
		}
		argv = argv[index+1:]
		if len(argv) == 0 {
			// `aira confine ... --` with nothing after it launches nothing. An
			// empty string here would render as a job running no command.
			return "", false
		}
		break
	}
	text := strings.Join(argv, " ")
	if strings.TrimSpace(text) == "" {
		// Nothing but empty arguments establishes no command to show.
		return "", false
	}
	if elided {
		// The bound above can cut mid-rune. Dropping the invalid tail keeps a
		// terminal-bound string well-formed; the ellipsis states that it happened.
		text = strings.ToValidUTF8(text, "") + " …"
	}
	return text, true
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
	// GrantedEstablished is the honesty bit for the ledger-derived numbers on
	// this struct (AIRA-220): GrantedBytes, Jobs, and the population split
	// (ScopeJobs/ScopeBytes, ReservationJobs/ReservationBytes,
	// AdoptedJobs/AdoptedBytes) are meaningful ONLY when it is true. Its precise
	// meaning: a queue (ledger) object exists for the slice — its adopted half is
	// as fresh as the last successful cgroup scan. It is false whenever the daemon
	// holds no queue object at all, which is NOT only a fresh/restarted daemon
	// before its first admission: a queue exists only while something is
	// connection-held, so pruneAdmitQueue deletes it (adopted ledger included) the
	// moment the last waiter releases. So a slice with live ADOPTED jobs but
	// nothing connection-held right now also reads false. A false bit must render
	// as `unevaluated`, never as a `0B granted / 0 admitted jobs` that reads "the
	// slice is empty, launch freely" while jobs are in fact live. Known accepted
	// gaps a true bit does NOT rule out (documented, not machined away per the
	// simplicity rule): a failed adopted scan leaves a stale/zero adopted figure
	// with present still true (the `slice scope caps: unevaluated` line beside it
	// signals the scan failure), and a sub-millisecond window between queue
	// registration and the first adoption scan. CeilingBytes is an independent
	// memory read and stays valid regardless of this bit.
	GrantedEstablished bool `json:"granted_established"`
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

	// AIRA-186. The RESOLVED reserve of that SAME named waiter — the figure the
	// daemon is actually gating this job's admission on — taken from the same
	// locked pass and the same waiter as the two fields above.
	//
	// It exists because a blocked launcher cannot otherwise know it. An UNPINNED
	// request sends the daemon a compiled-in prior as a hint; resolveAdmitReserve
	// replaces it with a history-derived estimate before the job is ever queued,
	// and the client learns that number only on the single blocking admit
	// response. So while it waits, the figure its own progress line prints
	// ("requested reserve 2G, unpinned") is NOT the number the slice is
	// contending over, and a 35.7G estimate against a 61.6G ceiling is
	// indistinguishable at the wait site from ordinary contention — the exact
	// misreading AIRA-186 was filed for. It rides the AIRA-24 probe that already
	// asks for the position, so it costs no extra round trip and no new verb.
	//
	// Zero is an ABSENCE — the scope id is not a queued waiter here, or the
	// daemon does not report it — never a reserve of zero; a renderer must print
	// nothing rather than a fabricated figure. `aira confine --list` passes no
	// scope id, so its output is unchanged.
	ResolvedReserveBytes int64 `json:"resolved_reserve_bytes,omitempty"`

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

	// AIRA-114. The aggregate over-subscription bound: how much memory every
	// LIVE scope on this slice is permitted to hold at once (the sum of their own
	// memory.max values), against the multiple of the ceiling admission allows.
	// It answers a question none of the numbers above can. The ledger reports
	// what is CHARGED, and since AIRA-29 that is live usage, so a slice can look
	// half empty while the caps already handed out total far more than it holds.
	//
	// CapBoundBytes is zero when the bound is switched off — an ABSENCE, so the
	// renderer prints nothing rather than a limit of zero.
	//
	// CapAggregateKnown is the honesty bit and is required: a zero
	// CapAggregateBytes means "no live capped scope" only while it is true. When
	// false the daemon could not establish the total (a failing confine scan, or
	// a live scope whose memory.max and memory.current were both unreadable), the
	// bound is withholding nothing, and every surface must say unevaluated rather
	// than render the zero as an idle slice.
	CapAggregateBytes int64 `json:"cap_aggregate_bytes,omitempty"`
	CapAggregateKnown bool  `json:"cap_aggregate_known,omitempty"`
	CapBoundBytes     int64 `json:"cap_bound_bytes,omitempty"`

	// ResidualJobs/ResidualBytes cross-check the derived split against the
	// daemon's incremental counters. They are equal by construction, so a
	// non-zero value here is a real lost or double ledger discharge. Signed:
	// negative means more was discharged than was ever charged, which is just as
	// real a defect as the positive direction.
	ResidualJobs  int   `json:"residual_jobs"`
	ResidualBytes int64 `json:"residual_bytes"`

	// AIRA-127. The system-and-slice frame `aira top` draws its RAM bar in. Every
	// one of these is a READING the confine-list pass already had in its hands or
	// takes from the same /proc/meminfo the ceiling subsystem samples; none of
	// them is a new admission signal, and nothing here participates in an
	// admission decision.
	//
	// ZERO IS AN ABSENCE in every field below, never a measured zero, and a
	// renderer must say "unevaluated" rather than draw a bar from it. Two
	// structural cases produce that absence deliberately:
	//
	//   - ci-shim mode, where the host's /proc/meminfo is NOT namespaced to the
	//     container (the same trap readShimMemory documents). Presenting host
	//     MemTotal as "total system RAM" beside an advisory container budget
	//     would be a fabricated frame, so shim mode publishes none of it.
	//   - a daemon-down client fallback, which builds no SliceReserve at all.
	//
	// SliceCurrentBytes/SliceReclaimableBytes come from the SAME memory reader
	// call whose `ok` gates this whole struct, so they describe the same instant
	// as GrantedBytes beside them. Reclaimable is carried separately rather than
	// pre-subtracted because the two are different facts: the slice's
	// non-reclaimable footprint is what MemAvailable does NOT already credit, and
	// that subtraction is a rendering judgement, not a reading.
	SystemMemTotalBytes     int64 `json:"system_mem_total_bytes,omitempty"`
	SystemMemAvailableBytes int64 `json:"system_mem_available_bytes,omitempty"`
	SliceCurrentBytes       int64 `json:"slice_current_bytes,omitempty"`
	SliceReclaimableBytes   int64 `json:"slice_reclaimable_bytes,omitempty"`
	// SliceMaxBytes is the slice's own live memory.max: the HARD limit, and the
	// figure every ceiling term below is a reduction of. Distinct from
	// CeilingStaticBytes, which carries the same reading only while the AIRA-103
	// ceiling subsystem is running and is absent when it is off.
	SliceMaxBytes int64 `json:"slice_max_bytes,omitempty"`
	// SliceHighBytes is the slice's memory.high SOFT limit -- reclaim pressure,
	// not a bound. SliceHighState is required to read it, and is the honesty bit:
	// "set" (a number), "none" (memory.high is `max` -- a POSITIVE fact that no
	// soft limit is configured), or "unevaluated" (the file could not be read or
	// parsed). A zero SliceHighBytes under state "set" cannot occur; under the
	// other two states the number is meaningless and must not be drawn.
	SliceHighBytes int64  `json:"slice_high_bytes,omitempty"`
	SliceHighState string `json:"slice_high_state,omitempty"`
	// CeilingEffectiveBytes is the AIRA-103/106 effective ceiling BEFORE the
	// per-job admission headroom CeilingBytes above subtracts. It is the figure
	// an operator sees a throttle in: when it sits below SliceMaxBytes, capacity
	// has been reduced by system pressure or the machine reserve, and the gap
	// between the two is the reduction. Zero when the subsystem is off.
	CeilingEffectiveBytes int64 `json:"ceiling_effective_bytes,omitempty"`

	// AIRA-137. The CPU frame `aira top` draws its CPU bar in — the exact
	// structural mirror of the RAM frame above, and published under the same
	// rules: withheld ENTIRELY in shim mode (a container's host-visible root
	// cgroup cpu.stat is not namespaced to it, so it is not a frame anybody
	// measured), and feeding no admission decision anywhere.
	//
	// System/Slice are CUMULATIVE cpu.stat `usage_usec` counters for the ROOT
	// cgroup and for this slice. CPUSampleUnixNano is the server-side instant
	// they were read at. A rate is (Δusec / Δwall-clock) across two ticks and is
	// computed by the CONSUMER, never here: the daemon holds no per-client tick
	// history and inventing one would make two clients on different refresh
	// intervals disagree about the same machine.
	//
	// SystemCPUKnown/SliceCPUKnown are required to read the counters. Unlike the
	// byte fields above, ZERO IS A LEGAL VALUE here — an idle cgroup really does
	// publish usage_usec 0 — so the absence bit cannot be folded into the number.
	//
	// CPUCores is runtime.NumCPU() on the daemon's host: the bar's TOTAL CAPACITY
	// is core-count × one core of CPU time per unit wall-clock. Zero is an
	// absence, and a bar with no established core count is not drawn.
	SystemCPUUsageUsec int64 `json:"system_cpu_usage_usec,omitempty"`
	SystemCPUKnown     bool  `json:"system_cpu_known,omitempty"`
	SliceCPUUsageUsec  int64 `json:"slice_cpu_usage_usec,omitempty"`
	SliceCPUKnown      bool  `json:"slice_cpu_known,omitempty"`
	CPUSampleUnixNano  int64 `json:"cpu_sample_unix_nano,omitempty"`
	CPUCores           int   `json:"cpu_cores,omitempty"`

	// AIRA-121. Containment/BudgetSource carry the ci-shim disposition on the
	// SAME summary line the granted/ceiling numbers are printed on. Without them
	// the reserve line in shim mode is byte-identical to a real slice's, which
	// would present an ADVISORY budget as an enforced ceiling -- the one place an
	// operator is most likely to read the number as a guarantee.
	//
	// Both empty in real mode, so every existing summary line is unchanged.
	Containment  string `json:"containment,omitempty"`
	BudgetSource string `json:"budget_source,omitempty"`

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

// The three states ConfineSliceReserve.SliceHighState may carry. "none" is a
// POSITIVE fact (memory.high reads `max`, so no soft limit is configured) and is
// deliberately distinct from "unevaluated" (the file could not be read); a
// renderer that collapsed the two would present an unreadable cgroup as an
// unlimited one.
const (
	ConfineSliceHighSet         = "set"
	ConfineSliceHighNone        = "none"
	ConfineSliceHighUnevaluated = "unevaluated"
)

// ConfineReservationStateHolding is the only state a reservation row carries
// today: the daemon has granted this reserve and is holding it until the client
// drops its connection. It is spelled out on the wire rather than implied so a
// later state cannot silently inherit this one's meaning.
const ConfineReservationStateHolding = "holding"

// ConfineReservationSignatureLimit bounds a rendered signature. A pytest nodeid
// is already long and is entirely client-supplied, so the terminal boundary
// truncates rather than trusting it to be reasonable.
const ConfineReservationSignatureLimit = 96

// ConfineReservationSignatureWireLimit bounds the signature the DAEMON retains
// and puts on the wire, and it exists for availability, not neatness: the admit
// protocol accepts a signature of any length up to the 16 MiB frame, so an
// unbounded diagnostic copy would let a few hostile (or merely absurd)
// reservations push the whole confine-list response past MaxFrameBytes — at
// which point `confine --list` fails for every job on that slice. It is wider
// than the render limit on purpose: a JSON consumer should get the full nodeid
// of any realistic caller, while the terminal gets a line it can display.
const ConfineReservationSignatureWireLimit = 512

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

	// AIRA-119. SinceMS is how long THIS state has been in effect — measured from
	// the exclusive waiter's own grant for "held", and from its enqueue for
	// "draining" — derived in the SAME locked pass as the identity above, so the
	// age and the job it describes can never come from different instants.
	//
	// It exists because the identity alone is not enough to act on, which is the
	// defect AIRA-119 records. Name defaults to "job" for every unnamed confine,
	// Owner may come from AIRA_CONFINE_OWNER and so appears in no argv, and in the
	// "draining" state the named job HAS NOT LAUNCHED YET: it owns no process and
	// no cgroup scope, by construction (`aira confine` creates its scope only
	// after admission is granted), so it has no row in the Scopes table either. An
	// operator who greps for the named job therefore finds nothing and reasonably
	// concludes the daemon is naming a job that already released. The age is what
	// separates a healthy drain from a stuck one, and ScopeID above is the handle
	// that makes the named job findable at all — the daemon has always sent it and
	// no face has ever rendered it.
	//
	// Zero is an UNESTABLISHED age (an anchor the daemon never set, or a clock
	// that went backwards), never "zero seconds": a renderer must omit the clause
	// rather than print a fabricated 0s.
	SinceMS int64 `json:"since_ms,omitempty"`

	// AIRA-185. Reason is the holder's own free-text label for WHY the slice is
	// exclusive ("deploy: slice-ceiling flip"), supplied by `aira drain wait
	// --reason`. It exists because Name cannot carry it: Name must be a valid
	// confine identity (no spaces, no colons) and must match the scope id, so a
	// human-readable purpose has nowhere to live in the identity fields.
	//
	// It is DIAGNOSTIC ONLY and takes part in no admission, gate or reaping
	// decision anywhere: an absent, malformed or hostile value can only change
	// what an operator is shown.
	//
	// Empty means the holder supplied none — every `aira confine --exclusive` does
	// — and a renderer must omit the clause rather than invent one. It is
	// UNTRUSTED terminal-bound text: bounded on the wire by
	// ConfineExclusiveReasonWireLimit and escaped plus re-bounded by every
	// renderer.
	Reason string `json:"reason,omitempty"`
}

// ConfineExclusiveReasonWireLimit bounds the reason the DAEMON retains and puts
// on the wire, for the same availability reason
// ConfineReservationSignatureWireLimit exists: the admit protocol would
// otherwise accept a reason of any size up to the 16 MiB frame, and an
// unbounded diagnostic copy on a long-lived waiter would ride into every
// `confine --list` reply until the response exceeded MaxFrameBytes and the verb
// stopped working for every job on the slice.
const ConfineExclusiveReasonWireLimit = 512

// ConfineExclusiveReasonLimit bounds what a RENDERER prints. Narrower than the
// wire limit on purpose: a JSON consumer should get the whole label, while the
// terminal gets a line it can display beside the identity it qualifies.
const ConfineExclusiveReasonLimit = 96

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

// ReadConfineCPUFrame samples the root-cgroup and slice CPU counters for
// `aira top`'s CPU bar (AIRA-137). It reads two cgroup files and a clock; it
// takes no lock, makes no decision, and returns an all-unknown frame rather than
// an error on any platform or host where the counters are not available.
func ReadConfineCPUFrame(slicePath string) ConfineCPUFrame {
	return readConfineCPUFrame(slicePath)
}

func KillConfine(ctx context.Context, slicePath, selector, callerOwner string, steal bool, registry []ConfineRegistryEntry) (ConfineKillResult, error) {
	return killConfine(ctx, slicePath, selector, callerOwner, steal, registry, 2*time.Second)
}
