package daemon

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"aira/internal/runner"
)

// AIRA-381. Session priority tiers (spec
// docs/superpowers/specs/2026-11-27-session-priority-tiers-design.md).
//
// The owner marks a session (an owner string, the one a job's scope id carries)
// HIGH or LOW; every other session is MEDIUM. A mark changes ONE thing, the
// admission evaluation order (admitOrderLocked), exactly as `--priority high`
// does one level down: no hold, no head-rule, budget or backstop change, no
// per-waiter state. The small-job lane walks the same order (small_lane.go).
//
// The table is Server.tiers: an immutable map published by pointer. A nil
// pointer is UNEVALUATED (ci-shim mode, a failed load at start, a Server built
// without Serve): it orders as an empty table, the list reply omits
// session_tiers, and every set is refused.
//
// s.tiers.Load() has exactly three call sites, one per reader, each loading
// once: evaluateAdmitQueue's start, the confine-list case's start, and
// setSessionTier. Everything else takes the map as an argument.

// sessionTier is a session's admission tier. The zero value is medium, which is
// never stored: a session with no row is medium.
type sessionTier uint8

const (
	tierMedium sessionTier = iota
	tierHigh
	tierLow
)

func (t sessionTier) String() string {
	switch t {
	case tierHigh:
		return runner.ConfineSessionTierHigh
	case tierLow:
		return runner.ConfineSessionTierLow
	default:
		return runner.ConfineSessionTierMedium
	}
}

// rank is the tier's place in the evaluation order: high 0, medium 1, low 2.
func (t sessionTier) rank() int {
	switch t {
	case tierHigh:
		return 0
	case tierLow:
		return 2
	default:
		return 1
	}
}

func parseSessionTier(text string) (sessionTier, bool) {
	switch text {
	case runner.ConfineSessionTierHigh:
		return tierHigh, true
	case runner.ConfineSessionTierMedium:
		return tierMedium, true
	case runner.ConfineSessionTierLow:
		return tierLow, true
	}
	return tierMedium, false
}

// sessionTierWriteTimeout bounds the store write of one set. A commit that
// times out AFTER committing leaves the refused set stored; the next set
// rewrites the table from memory, and a restart loads what is stored (accepted,
// spec §3.1).
const sessionTierWriteTimeout = 10 * time.Second

// tierTable is the published table behind the pointer: nil for unevaluated.
func tierTable(published *map[string]sessionTier) map[string]sessionTier {
	if published == nil {
		return nil
	}
	return *published
}

// waiterTier is a waiter's tier: the mark of the owner embedded in its scope id
// (runner.ParseConfineScopeID, the parse every row and the --front/kill guards
// use), never waiter.owner (the request's claim). Untiered, always medium: a
// sub-reservation (a running job's internal progress, D5), a scope-less waiter
// (`aira run`) and a scope id with no owner tail.
func waiterTier(waiter *admitWaiter, tiers map[string]sessionTier) sessionTier {
	if len(tiers) == 0 || waiter.isSubReservation() || waiter.scopeID == "" {
		return tierMedium
	}
	_, _, _, owner, ok := runner.ParseConfineScopeID(waiter.scopeID)
	if !ok || owner == "" {
		return tierMedium
	}
	return tiers[owner]
}

// sessionTiersFromRows validates the stored rows into a table. Any unusable row
// (an owner the setter would refuse, a tier that is not high or low) or more
// than the cap fails the WHOLE load: a silently dropped mark would order that
// session medium while the store still says otherwise.
func sessionTiersFromRows(rows map[string]string) (map[string]sessionTier, error) {
	if len(rows) > runner.ConfineSessionTierCap {
		return nil, fmt.Errorf("%d stored marks, over the cap of %d", len(rows), runner.ConfineSessionTierCap)
	}
	table := make(map[string]sessionTier, len(rows))
	for owner, text := range rows {
		tier, ok := parseSessionTier(text)
		if err := runner.ValidateConfineSessionTierArgs(owner, text); err != nil || !ok || tier == tierMedium {
			return nil, fmt.Errorf("stored mark owner=%q tier=%q is not a valid high or low mark", owner, text)
		}
		table[owner] = tier
	}
	return table, nil
}

// loadSessionTiers is the one load, in Serve after OpenDB and before Listen, so
// the first pass and every re-declared survivor see the stored marks. A failure
// leaves the table nil (unevaluated) and the daemon serving; it never refuses
// start. Nothing deletes rows the daemon did not read: the way out is to fix the
// rows by hand and restart.
func (s *Server) loadSessionTiers(ctx context.Context) {
	s.tiers.Store(nil)
	if s.shimMode() {
		log.Print("aira daemon: session tiers: ci-shim mode, unevaluated (a container is one session)")
		return
	}
	rows, err := s.db.ConfineSessionTiers(ctx)
	if err != nil {
		log.Printf("aira daemon: session tiers: did not load (%v); unevaluated: ordered as no marks, every set refused, until a daemon restart loads them", err)
		return
	}
	table, err := sessionTiersFromRows(rows)
	if err != nil {
		log.Printf("aira daemon: session tiers: did not load (%v); unevaluated: ordered as no marks, every set refused. "+
			"Remedy by hand, then restart the daemon: sqlite3 %s \"SELECT owner, tier FROM confine_session_tiers\" to see the rows, "+
			"and \"DELETE FROM confine_session_tiers WHERE owner='<owner>'\" to remove a bad one (at most %d rows)",
			err, s.Paths.DBPath, runner.ConfineSessionTierCap)
		return
	}
	s.tiers.Store(&table)
	log.Printf("aira daemon: session tiers: %d loaded", len(table))
}

// replaceSessionTiers writes the whole table to the store: s.db's method, or the
// test seam.
func (s *Server) replaceSessionTiers(ctx context.Context, marks map[string]string) error {
	if s.replaceSessionTiersFn != nil {
		return s.replaceSessionTiersFn(ctx, marks)
	}
	return s.db.ReplaceConfineSessionTiers(ctx, marks)
}

// setSessionTier is the confine-session-tier handler. Under tiersMu for the
// whole operation (two concurrent sets must not lose one): refuse on an
// unevaluated table, copy, apply, cap-check, write the copy to the store, and
// only then publish it and write the audit line. No queue.mu: the new order
// applies on the next evaluator pass.
func (s *Server) setSessionTier(ctx context.Context, callerOwner string, args map[string]any) (runner.ConfineSessionTierResult, error) {
	// No --slice: tiers are machine-wide, and an accepted-and-ignored slice is
	// what AIRA-82/196 refuse. Refused whatever its value, empty included.
	if _, present := args["slice"]; present {
		return runner.ConfineSessionTierResult{}, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: confine-session-tier takes no slice: session tiers are machine-wide")
	}
	session, tierText := stringArg(args, "session"), stringArg(args, "tier")
	if err := runner.ValidateConfineSessionTierArgs(session, tierText); err != nil {
		return runner.ConfineSessionTierResult{}, err
	}
	tier, _ := parseSessionTier(tierText)
	if s.sessionTierSetEnteredFn != nil {
		s.sessionTierSetEnteredFn(session)
	}
	s.tiersMu.Lock()
	defer s.tiersMu.Unlock()
	current := s.tiers.Load()
	if current == nil {
		return runner.ConfineSessionTierResult{}, fmt.Errorf("E_CONFINE_UNAVAILABLE: session tiers are unevaluated on this daemon: " +
			"it runs in ci-shim mode (a container is one session), or the stored marks did not load at daemon start (see the daemon journal, fix the rows it names and restart the daemon)")
	}
	previous := (*current)[session]
	next := make(map[string]sessionTier, len(*current)+1)
	for owner, mark := range *current {
		next[owner] = mark
	}
	if tier == tierMedium {
		delete(next, session)
	} else {
		next[session] = tier
	}
	if len(next) > runner.ConfineSessionTierCap {
		return runner.ConfineSessionTierResult{}, fmt.Errorf("E_CONFINE_SESSION_TIERS_FULL: %d sessions are already marked, the most the daemon keeps; set one back to medium first (`aira confine --list` lists them)", runner.ConfineSessionTierCap)
	}
	stored := make(map[string]string, len(next))
	for owner, mark := range next {
		stored[owner] = mark.String()
	}
	writeCtx, cancel := context.WithTimeout(ctx, sessionTierWriteTimeout)
	defer cancel()
	if err := s.replaceSessionTiers(writeCtx, stored); err != nil {
		return runner.ConfineSessionTierResult{}, fmt.Errorf("E_CONFINE_UNAVAILABLE: store session tiers: %v", err)
	}
	s.tiers.Store(&next)
	log.Printf("aira daemon: confine-session-tier: setter=%s session=%s tier=%s->%s", callerOwner, session, previous, tier)
	return runner.ConfineSessionTierResult{Session: session, Tier: tier.String(), Previous: previous.String()}, nil
}

// sessionTierRows is the list reply's field: nil (absent, unevaluated) for an
// unevaluated table, else every mark sorted by owner, `[]` when there are none.
func sessionTierRows(published *map[string]sessionTier) *[]runner.ConfineSessionTier {
	if published == nil {
		return nil
	}
	rows := make([]runner.ConfineSessionTier, 0, len(*published))
	for owner, tier := range *published {
		rows = append(rows, runner.ConfineSessionTier{Owner: owner, Tier: tier.String()})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Owner < rows[j].Owner })
	return &rows
}
