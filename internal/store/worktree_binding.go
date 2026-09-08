package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"aira/internal/domain"
)

// Root returns the worktree root this store is scoped to. `aira worktree audit`
// enumerates the repository's checkouts from here, exactly as the reconciler's
// own scan does, rather than from a caller-supplied path.
func (s *Store) Root() string { return s.root }

// TicketPrefixes returns this project's registered ticket ID prefixes. The
// audit's inference fallback reads convention off them (`aira176-…` branches,
// `AIRA-176:` commit subjects) rather than hard-coding this repository's own.
func (s *Store) TicketPrefixes() []string { return s.prefixesByKind(kindTicket) }

// RegisterWorktreeBinding writes (or refreshes) ONE binding, keyed
// (project_id, worktree_id, ticket_id).
//
// The key is per-TICKET, not per-worktree: one checkout here routinely serves
// several live tickets at once (17 of the last 300 commits on master carry
// multi-ticket prefixes such as `AIRA-188/189/190:`). A per-worktree key would
// have made registering the second ticket close the first — reporting a
// finished association for work in progress — so the multi-ticket case is made
// unrepresentable rather than merely handled. This is the exact shape
// area_hints already uses, `(project_id, ticket_id, worktree_id, glob)`.
//
// ticket_id deliberately carries NO foreign key to `tickets`: that table is
// keyed (project_id, worktree_id, id) — one row per worktree holding the file —
// so a ticket ID has no single parent row to reference. area_hints has the same
// `ticket_id TEXT NOT NULL` with a projects-only FK; this follows it exactly.
//
// The worktree identity is the STORE's own, never a caller argument, so a
// caller cannot declare a binding for a checkout it is not standing in.
func (s *Store) RegisterWorktreeBinding(ctx context.Context, input domain.WorktreeBindingInput) (domain.WorktreeBinding, error) {
	if err := input.Validate(); err != nil {
		return domain.WorktreeBinding{}, err
	}
	binding := domain.WorktreeBinding{
		WorktreeID:    s.worktreeID,
		TicketID:      strings.TrimSpace(input.TicketID),
		Branch:        strings.TrimSpace(input.Branch),
		BaseRef:       strings.TrimSpace(input.BaseRef),
		BaseCommit:    strings.TrimSpace(input.BaseCommit),
		Owner:         strings.TrimSpace(input.Owner),
		OwnerAttested: input.OwnerAttested,
		RegisteredAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	attested := 0
	if binding.OwnerAttested {
		attested = 1
	}
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `INSERT INTO worktree_bindings(
			project_id, worktree_id, ticket_id, branch, base_ref, base_commit, owner, owner_attested, registered_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, worktree_id, ticket_id) DO UPDATE SET
				branch=excluded.branch, base_ref=excluded.base_ref, base_commit=excluded.base_commit,
				owner=excluded.owner, owner_attested=excluded.owner_attested, registered_at=excluded.registered_at`,
			s.projectID, binding.WorktreeID, binding.TicketID, binding.Branch, binding.BaseRef,
			binding.BaseCommit, binding.Owner, attested, binding.RegisteredAt)
		return execErr
	})
	if err != nil {
		return domain.WorktreeBinding{}, err
	}
	return binding, nil
}

// WorktreeBindings returns every declared binding in this project, ordered so a
// report is stable. Bindings are never deleted by AIRA itself — there is no
// unregister verb and no gc — so this is the complete declared set until the
// project is ejected, at which point the projects FK cascade removes it.
func (s *Store) WorktreeBindings() ([]domain.WorktreeBinding, error) {
	rows, err := s.db.Query(`SELECT worktree_id, ticket_id, branch, base_ref, base_commit, owner, owner_attested, registered_at
		FROM worktree_bindings WHERE project_id=? ORDER BY worktree_id, ticket_id`, s.projectID)
	if err != nil {
		return nil, translateDBError(err)
	}
	defer rows.Close()
	bindings := make([]domain.WorktreeBinding, 0)
	for rows.Next() {
		var binding domain.WorktreeBinding
		var attested int
		if err := rows.Scan(&binding.WorktreeID, &binding.TicketID, &binding.Branch, &binding.BaseRef,
			&binding.BaseCommit, &binding.Owner, &attested, &binding.RegisteredAt); err != nil {
			return nil, translateDBError(err)
		}
		binding.OwnerAttested = attested != 0
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, translateDBError(err)
	}
	return bindings, nil
}

// WorktreeRoots maps worktree identity to the last root AIRA saw that checkout
// at. It exists so a binding whose checkout git no longer reports can still be
// NAMED in a report rather than reduced to an opaque hash. The path is the
// registry's, not the binding's: nothing about a worktree's location is cached
// on the binding row, so there is no second copy to drift.
func (s *Store) WorktreeRoots() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT worktree_id, root FROM worktrees WHERE project_id=?`, s.projectID)
	if err != nil {
		return nil, translateDBError(err)
	}
	defer rows.Close()
	roots := map[string]string{}
	for rows.Next() {
		var id, root string
		if err := rows.Scan(&id, &root); err != nil {
			return nil, translateDBError(err)
		}
		roots[id] = root
	}
	if err := rows.Err(); err != nil {
		return nil, translateDBError(err)
	}
	return roots, nil
}
