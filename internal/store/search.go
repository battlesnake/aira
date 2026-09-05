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

// searchIndexDDL is the per-query index. It carries no project_id/worktree_id
// columns because the index is private to one Search call and only ever holds
// this scope's rows, so scoping is structural rather than a predicate that has
// to be trusted. Column 2 is `content`, which is what snippet() must address.
const searchIndexDDL = `CREATE VIRTUAL TABLE search USING fts5(kind UNINDEXED, ref_id UNINDEXED, content)`

// Search answers a grep from a private, per-query FTS index built from the
// current canonical files plus the DB-authoritative rants. Git files therefore
// remain authoritative even after direct edits, interrupted mutations, or
// removal of an indexed entity.
//
// The index lives in its own in-memory SQLite database, not in a persistent
// table and not in a TEMP table on the Store's own connection. That is what
// lets a grep be served from a read-only Store: OpenReadOnly opens with
// `query_only(ON)`, under which BOTH `CREATE TEMP TABLE` and
// `CREATE VIRTUAL TABLE temp.x USING fts5(...)` fail with "attempt to write a
// readonly database", and cmd/aira's client-routed path hands exactly that
// Store to a relay which exposes the whole read surface, Search included.
// grep is daemon-routed today (internal/core/routing.go's default arm), so a
// TEMP-table design would have been latent rather than live — but latent and
// unguarded is still a real gap, and a private database simply does not have
// it.
//
// The canonical scan and the rant read share a brief per-project search lock
// with AIRA mutations and other greps, so one grep observes one canonical
// snapshot across the many files it reads. The lock is released before the
// index is built and queried: past that point the result is a pure function of
// data already in memory. A mutation that lands after the release is
// intentionally reflected by the next grep — freshness across separate greps is
// advisory/eventual.
func (s *Store) Search(ctx context.Context, query, kind string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("E_QUERY_INVALID: grep query is empty")
	}
	if kind != "" && kind != "ticket" && kind != "finding" && kind != "rant" {
		return nil, fmt.Errorf("E_QUERY_INVALID: unsupported grep kind %q", kind)
	}
	docs, err := s.searchSnapshot(ctx)
	if err != nil {
		if ErrorCode(err) == "U_INDEX_UNESTABLISHED" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	return searchPrivateIndex(ctx, docs, query, kind)
}

// searchRow is one document of the per-query index.
type searchRow struct {
	kind    string
	refID   string
	content string
}

// searchSnapshot takes the one canonical snapshot a grep is entitled to: the
// ticket and finding files on disk plus this project's rants, all read under a
// single hold of the per-project search lock.
func (s *Store) searchSnapshot(ctx context.Context) ([]searchRow, error) {
	lock, err := s.acquireSearchLock()
	if err != nil {
		return nil, err
	}
	defer unlockFile(lock)

	tickets, _, _, inconclusive, err := scanTickets(s.root, s.worktreeID, s.projectSlug)
	if err != nil {
		return nil, err
	}
	if inconclusive {
		return nil, indexUnestablishedError()
	}
	findings, inconclusive, err := s.scanFindingFiles(s.root, s.worktreeID)
	if err != nil {
		return nil, err
	}
	if inconclusive {
		return nil, indexUnestablishedError()
	}
	docs := make([]searchRow, 0, len(tickets)+len(findings.valid))
	for _, ticket := range tickets {
		docs = append(docs, searchRow{kind: "ticket", refID: ticket.Ticket.ID, content: ticket.Ticket.Title + "\n" + ticket.Body})
	}
	for _, finding := range findings.valid {
		if finding.Finding.Subtype != domain.FindingSubtypeReview {
			continue
		}
		docs = append(docs, searchRow{kind: "finding", refID: finding.Finding.Key, content: finding.Finding.Message})
	}
	if s.beforeSearchIndexBuild != nil {
		s.beforeSearchIndexBuild()
	}
	rants, err := s.scanRantRows(ctx)
	if err != nil {
		return nil, err
	}
	return append(docs, rants...), nil
}

// scanRantRows reads this project's rants. It is the only part of a grep that
// touches the Store's own connection, and it is a plain SELECT so it is legal
// under query_only. Rants are project-scoped, not worktree-scoped: every
// worktree of a project greps the same rants, which is exactly what the
// persistent index did by inserting all project rants tagged with the current
// worktree.
func (s *Store) scanRantRows(ctx context.Context) ([]searchRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, body FROM rants WHERE project_id=?`, s.projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []searchRow
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		result = append(result, searchRow{kind: "rant", refID: id, content: body})
	}
	return result, rows.Err()
}

// searchPrivateIndex builds the throwaway index and runs the MATCH against it.
// A malformed user query is still reported as E_QUERY_INVALID; every other
// failure is E_INDEX_UNEVALUATED, never a silently empty result.
func searchPrivateIndex(ctx context.Context, docs []searchRow, query, kind string) ([]SearchResult, error) {
	// Each connection to ":memory:" would get its own empty database, so the
	// pool is pinned to one connection and that connection is held for the
	// whole build-and-query.
	//
	// temp_store(2) = MEMORY is load-bearing, not decoration. An in-memory
	// DATABASE does not make SQLite's TEMPORARY storage in-memory: the pinned
	// driver builds with SQLITE_TEMP_STORE=1, so transient indices and the
	// ORDER BY sorter may spill to files on disk — and the rows being sorted
	// here include rant bodies and their snippets. A rant redacted immediately
	// after a concurrent grep took its snapshot would then have its body sitting
	// in a spill file that nothing scrubs. Forcing MEMORY removes that path
	// entirely. (Process memory itself — swap, a core dump — is explicitly NOT
	// covered by the erasure guarantee; closing SQLite does not scrub RAM.)
	db, err := sql.Open("sqlite", "file::memory:?_pragma=temp_store(2)")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, searchIndexDDL); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
	}
	for _, doc := range docs {
		if kind != "" && doc.kind != kind {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO search(kind,ref_id,content) VALUES(?,?,?)`, doc.kind, doc.refID, doc.content); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSearchUnevaluated, err)
		}
	}
	// Column 2 is `content`. snippet()'s column argument is 0-based, and the
	// long-standing `3` against the old five-column table addressed WORKTREE_ID:
	// every grep result's snippet was the literal worktree name ("main"), never
	// the matched text. It survived because the only assertion was
	// `Snippet != ""`, which a non-empty worktree id satisfies. The guard is the
	// assertion that a snippet must CONTAIN the matched term.
	rows, err := conn.QueryContext(ctx, `SELECT kind, ref_id, snippet(search, 2, '[', ']', '…', 32), bm25(search)
		FROM search WHERE search MATCH ? ORDER BY bm25(search) ASC, kind ASC, ref_id ASC`, query)
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

// acquireSearchLock takes the grep-vs-mutation snapshot lock. It is keyed by
// project: all three holders (Search, materialiseIntent, Rebuild) operate on
// one project's files and rows, so a machine-wide lock made a grep in one
// project serialise against a ticket write in an unrelated one, on the single
// shared state directory. Project ids are hex digests, so they are safe in a
// filename.
func (s *Store) acquireSearchLock() (*os.File, error) {
	return acquireLock(filepath.Join(filepath.Dir(s.dbPath), "search-rebuild."+s.projectID+".lock"))
}
