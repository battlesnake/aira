package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aira/internal/domain"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// SearchResult is the small, transport-neutral result returned by grep.
type SearchResult struct {
	Kind    string  `json:"kind"`
	ID      string  `json:"id"`
	Snippet string  `json:"snippet"`
	Rank    float64 `json:"rank"`
}

var ErrSearchUnevaluated = errors.New("E_INDEX_UNEVALUATED: search index could not be built")

// Search reconciles the disposable FTS cache from the current canonical files
// plus DB-authoritative rants before every query. Git files therefore remain
// authoritative even after
// direct edits, interrupted mutations, or removal of an indexed entity.
// The scan, replacement, and MATCH query share a brief search writer lock with
// AIRA mutations and other greps, so one grep observes one canonical snapshot.
// A mutation that lands after this lock is released is intentionally reflected
// by the next grep: freshness across separate greps is advisory/eventual.
func (s *Store) Search(ctx context.Context, query, kind string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("E_QUERY_INVALID: grep query is empty")
	}
	if kind != "" && kind != "ticket" && kind != "finding" && kind != "rant" {
		return nil, fmt.Errorf("E_QUERY_INVALID: unsupported grep kind %q", kind)
	}
	lock, err := s.acquireSearchLock()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	defer unlockFile(lock)
	if err := s.reconcileSearchIndex(ctx); err != nil {
		if ErrorCode(err) == "U_INDEX_UNESTABLISHED" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	if s.beforeSearchQuery != nil {
		if err := s.beforeSearchQuery(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
		}
	}

	// Column 4 is `content`. The table is
	// (project_id, kind, ref_id, worktree_id, content), and snippet()'s column
	// argument is 0-based, so the long-standing `3` here addressed WORKTREE_ID:
	// every grep result's snippet was the literal worktree name ("main"), never
	// the matched text. Found while scoping AIRA-74; it survived because the only
	// assertion was `Snippet != ""`, which a non-empty worktree id satisfies.
	sqlQuery := `SELECT kind, ref_id, snippet(search_fts, 4, '[', ']', '…', 32), bm25(search_fts)
		FROM search_fts WHERE search_fts MATCH ? AND project_id=? AND worktree_id=?`
	args := []any{query, s.projectID, s.worktreeID}
	if kind != "" {
		sqlQuery += ` AND kind=?`
		args = append(args, kind)
	}
	sqlQuery += ` ORDER BY bm25(search_fts) ASC, kind ASC, ref_id ASC`
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		if isFTSUserQueryError(err) {
			return nil, fmt.Errorf("E_QUERY_INVALID: %w", err)
		}
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	defer rows.Close()
	result := []SearchResult{}
	for rows.Next() {
		var row SearchResult
		if err := rows.Scan(&row.Kind, &row.ID, &row.Snippet, &row.Rank); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		if isFTSUserQueryError(err) {
			return nil, fmt.Errorf("E_QUERY_INVALID: %w", err)
		}
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	return result, nil
}

func isFTSUserQueryError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code()&0xff == sqlite3.SQLITE_ERROR
}

func (s *Store) reconcileSearchIndex(ctx context.Context) error {
	tickets, _, _, inconclusive, err := scanTickets(s.root, s.worktreeID, s.projectSlug)
	if err != nil {
		return err
	}
	if inconclusive {
		return indexUnestablishedError()
	}
	findings, inconclusive, err := s.scanFindingFiles(s.root, s.worktreeID)
	if err != nil {
		return err
	}
	if inconclusive {
		return indexUnestablishedError()
	}
	if s.beforeSearchReconcileCommit != nil {
		s.beforeSearchReconcileCommit()
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM search_fts WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID); err != nil {
			return err
		}
		if err := insertSearchRows(ctx, conn, s.projectID, tickets, findings.valid); err != nil {
			return err
		}
		return insertRantSearchRows(ctx, conn, s.projectID, s.worktreeID)
	})
}

func insertRantSearchRows(ctx context.Context, conn *sql.Conn, projectID, worktreeID string) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content)
		SELECT project_id,'rant',id,?,body FROM rants WHERE project_id=?`, worktreeID, projectID)
	return err
}

func (s *Store) acquireSearchLock() (*os.File, error) {
	return acquireLock(filepath.Join(filepath.Dir(s.dbPath), "search-rebuild.lock"))
}

func insertSearchRows(ctx context.Context, conn *sql.Conn, projectID string, tickets []scannedTicket, findings []scannedFinding) error {
	for _, ticket := range tickets {
		if _, err := conn.ExecContext(ctx, `INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content) VALUES(?,?,?,?,?)`,
			projectID, "ticket", ticket.Ticket.ID, ticket.WorktreeID, ticket.Ticket.Title+"\n"+ticket.Body); err != nil {
			return err
		}
	}
	for _, finding := range findings {
		if finding.Finding.Subtype != domain.FindingSubtypeReview {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content) VALUES(?,?,?,?,?)`,
			projectID, "finding", finding.Finding.Key, finding.WorktreeID, finding.Finding.Message); err != nil {
			return err
		}
	}
	return nil
}
