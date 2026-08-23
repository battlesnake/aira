package store

import (
	"context"
	"database/sql"
)

// AppendWatchdogEvent appends one host-watchdog decision to this project scope.
// The daemon is the single caller and broadcasts the same decision to each ready
// project so host-global safety actions remain visible in project-scoped watches.
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
