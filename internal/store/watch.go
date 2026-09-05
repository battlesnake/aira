package store

import (
	"context"
	"database/sql"
)

// AppendWatchdogEvent appends one host-watchdog decision to this project scope.
// The daemon is the single caller and broadcasts the same decision to each ready
// project so host-global safety actions remain visible in project-scoped watches.
//
// UNJOURNALED BY DESIGN (AIRA-75). These rows keep `journaled=0` forever, and
// that is a decision rather than an omission:
//
//   - There is nothing to journal them AGAINST. The journal is the durable
//     record of this project's own allocations and mutations, replayed and
//     cross-checked against git-file content. A host-global watchdog kill is not
//     a fact about this project at all — the same decision is broadcast verbatim
//     into every ready project's stream — so a journal entry would be a
//     fabricated per-project provenance record for a machine-level event.
//   - They are still WATCH-VISIBLE, which is the whole point of writing them
//     here: `aira watch` reads `events` (EventsSince, ordered by seq), so the
//     rows must exist and must carry a seq. Demoting them to a daemon log line —
//     the other reading of "stop minting a sequence number" — would silently
//     remove the only surface on which a session sees the watchdog kill
//     something. That is a different, worse change.
//
// AIRA-75 filed this as "voiding the journal's gap-detection". Re-verified: no
// gap-detection over `events.seq` exists to void — nothing in this package reads
// seq contiguity, and the journal is keyed by (project, seq) lookups
// (journalEventFor), never by "every seq must be present".
//
// KNOWN OPEN CONSEQUENCE, and NOT the one the ticket named (build-review, Sol):
// the seq is still not free. Rebuild reconstructs `event_counters.next_seq` from
// the receipts and journal alone (Store.Rebuild's maxSeq walk), and these rows
// appear in neither — so after a database loss their sequence numbers are
// forgotten and REISSUED to new events, and an `aira watch` consumer resuming
// from its old cursor (cmd/aira/watch.go, `seq > from`) silently skips them. An
// earlier version of this comment claimed the seq "costs nothing beyond a
// number" — that is retracted.
//
// RETRACTED REMEDY (AIRA-75, CLOSED): the build-review that found the above
// proposed "have Rebuild also take MAX(seq) over the `events` table" as the
// fix. That is WRONG — it is a no-op in every case where reissue can actually
// happen, so no counter change closes this gap:
//
//   - A rebuild against an INTACT database never reissues to begin with: both
//     `events` and `event_counters` survive, and `event_counters.next_seq` is
//     already correct there — seq-allocation and event-insert commit as one
//     `BEGIN IMMEDIATE` transaction (§2.1 of the d3-watch design, enforced by
//     test), so the two can never desync on a live DB. Rebuild's own upsert
//     (`ON CONFLICT ... next_seq = CASE WHEN event_counters.next_seq <
//     excluded.next_seq THEN excluded.next_seq ELSE event_counters.next_seq
//     END`) only ever raises the stored value, never lowers it. Adding a
//     second source here changes nothing that was not already correct.
//   - Reissue only happens after a full DATABASE LOSS (fresh schema, every
//     table starts empty) or an `aira eject` (`events` and `event_counters`
//     both carry `FOREIGN KEY(project_id) REFERENCES projects(project_id) ON
//     DELETE CASCADE`, so ejecting the project row deletes both together — no
//     other code path deletes either table). In both routes `events` is EMPTY
//     at exactly the moment Rebuild runs, so `SELECT MAX(seq) FROM events`
//     reads 0 — the same nothing the receipts/journal walk already contributes
//     for these rows. There is no surviving copy of a watchdog row's seq
//     anywhere once its project's rows are gone: no fix that reconstructs the
//     counter from data still inside the SAME on-disk database can recover it,
//     because that data is erased by the identical event that caused the loss.
//     The only place a watchdog seq could survive DB loss is a durable store
//     OUTSIDE the database — i.e. the journal — and that is exactly the
//     "journal them" option this file already rejects above, for a reason
//     unrelated to counters (no per-project provenance to journal against).
//
// ACCEPTED, DOCUMENTED BOUND: an `aira watch --from N` consumer that resumes
// after a database loss or eject may silently skip up to N_skip new events,
// where N_skip = the count of trailing unjournaled (watchdog) seqs issued
// between the loss and the resume — those seq numbers are reissued to whatever
// real events land first after rebuild, and the resuming cursor is already past
// them. This sits outside the d3-watch design's "durable in the DB, recoverable
// via --from" guarantee (docs/superpowers/specs/2026-08-17-aira-d3-watch-design.md)
// for exactly the one class of row (watchdog) deliberately excluded from
// durability by design, above. Journaling the seq (contradicts "nothing to
// journal them against", above) and an epoch/generation token in the watch
// cursor (new protocol machinery) were both considered and rejected as
// disproportionate to this narrow, accepted gap.
//
// Measured on this machine, 2026-09-04: 245 of this project's 541 events (45.3%)
// are journaled=0, and every one of them is an `aira-watchdog` actor row
// (watchdog.trip / .intent / .outcome / .recovered / .defer). No other producer
// leaves an unjournaled event.
func (s *Store) AppendWatchdogEvent(ctx context.Context, verb, target string) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		seq, err := nextSequence(ctx, conn, s.projectID)
		if err != nil {
			return err
		}
		return insertEventActor(ctx, conn, s.projectID, seq, "aira-watchdog", verb, target)
	})
}

// WatchEvent is the durable event header exposed by the daemon watch API.
// Seq is the ordering authority; At is advisory wall-clock evidence.
type WatchEvent struct {
	Seq    int64  `json:"seq"`
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Verb   string `json:"verb"`
	Target string `json:"target"`
}

// EventsSince returns the next unfiltered event window after from and the
// greatest sequence scanned. The rows are fully consumed before return, so no
// database connection is held while a caller waits between scans.
func (s *Store) EventsSince(ctx context.Context, from int64, limit int) ([]WatchEvent, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,at_wall,actor,verb,target FROM events
		WHERE project_id=? AND seq>? ORDER BY seq ASC LIMIT ?`, s.projectID, from, limit)
	if err != nil {
		return nil, from, translateDBError(err)
	}
	defer rows.Close()
	events := make([]WatchEvent, 0)
	next := from
	for rows.Next() {
		var event WatchEvent
		if err := rows.Scan(&event.Seq, &event.At, &event.Actor, &event.Verb, &event.Target); err != nil {
			return nil, from, translateDBError(err)
		}
		events = append(events, event)
		next = event.Seq
	}
	if err := rows.Err(); err != nil {
		return nil, from, translateDBError(err)
	}
	return events, next, nil
}

// CurrentMaxSeq returns the project's committed event high-water mark.
func (s *Store) CurrentMaxSeq(ctx context.Context) (int64, error) {
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM events WHERE project_id=?`, s.projectID).Scan(&seq); err != nil {
		return 0, translateDBError(err)
	}
	return seq, nil
}
