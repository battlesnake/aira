package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"aira/internal/domain"
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
// before every query. Git files therefore remain authoritative even after
// direct edits, interrupted mutations, or removal of an indexed entity.
func (s *Store) Search(ctx context.Context, query, kind string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("E_QUERY_INVALID: grep query is empty")
	}
	if kind != "" && kind != "ticket" && kind != "finding" {
		return nil, fmt.Errorf("E_QUERY_INVALID: unsupported grep kind %q", kind)
	}
	if err := s.reconcileSearchIndex(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}

	sqlQuery := `SELECT kind, ref_id, snippet(search_fts, 3, '[', ']', '…', 32), bm25(search_fts)
		FROM search_fts WHERE search_fts MATCH ? AND worktree_id=?`
	args := []any{query, s.worktreeID}
	if kind != "" {
		sqlQuery += ` AND kind=?`
		args = append(args, kind)
	}
	sqlQuery += ` ORDER BY bm25(search_fts) ASC, kind ASC, ref_id ASC`
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		if isFTSQueryError(err) {
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
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	return result, nil
}

func isFTSQueryError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "fts5") || strings.Contains(message, "malformed match") ||
		strings.Contains(message, "unterminated") || strings.Contains(message, "syntax error")
}

func (s *Store) reconcileSearchIndex(ctx context.Context) error {
	tickets, _, _, err := scanTickets(s.root, s.worktreeID, s.projectSlug)
	if err != nil {
		return err
	}
	findings, err := s.scanFindingFiles(s.root, s.worktreeID)
	if err != nil {
		return err
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM search_fts WHERE worktree_id=?`, s.worktreeID); err != nil {
			return err
		}
		return insertSearchRows(ctx, conn, tickets, findings.valid)
	})
}

func insertSearchRows(ctx context.Context, conn *sql.Conn, tickets []scannedTicket, findings []scannedFinding) error {
	for _, ticket := range tickets {
		if _, err := conn.ExecContext(ctx, `INSERT INTO search_fts(kind,ref_id,worktree_id,content) VALUES(?,?,?,?)`,
			"ticket", ticket.Ticket.ID, ticket.WorktreeID, ticket.Ticket.Title+"\n"+ticket.Body); err != nil {
			return err
		}
	}
	for _, finding := range findings {
		if finding.Finding.Subtype != domain.FindingSubtypeReview {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO search_fts(kind,ref_id,worktree_id,content) VALUES(?,?,?,?)`,
			"finding", finding.Finding.Key, finding.WorktreeID, finding.Finding.Message); err != nil {
			return err
		}
	}
	return nil
}
