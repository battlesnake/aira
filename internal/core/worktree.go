package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/store"
	"aira/internal/worktree"
)

// worktreeStore is the capability AIRA-176's two verbs need. It is an OPTIONAL
// interface rather than a widening of Store: every face and every test double
// that does not care about worktree bindings keeps working unchanged, and a
// store that cannot answer says so out loud instead of returning a fabricated
// empty report.
type worktreeStore interface {
	Root() string
	WorktreeID() string
	TicketPrefixes() []string
	RegisterWorktreeBinding(context.Context, domain.WorktreeBindingInput) (domain.WorktreeBinding, error)
	WorktreeBindings() ([]domain.WorktreeBinding, error)
	WorktreeRoots() (map[string]string, error)
	ListLeases(context.Context) ([]store.HeldLeaseRow, error)
}

// WorktreeRegisterResult is what `aira worktree register` reports back.
type WorktreeRegisterResult struct {
	Binding domain.WorktreeBinding `json:"binding"`
	Root    string                 `json:"root"`
	// Notes name every field the capture could NOT establish, and what to do
	// about it. A registration that recorded no integration ref still succeeds —
	// the binding's job is to say which ticket this checkout is for — but the
	// reader is told, rather than discovering it later as an unevaluated audit.
	Notes []string `json:"notes,omitempty"`
}

func (c *Core) worktreeCapableStore() (worktreeStore, error) {
	ws, ok := c.store.(worktreeStore)
	if !ok {
		return nil, errors.New("E_WORKTREE_UNAVAILABLE: this store cannot answer worktree binding queries")
	}
	return ws, nil
}

func (c *Core) worktreeRegister(ctx context.Context, selector, base, owner string, ownerAttested bool) (any, error) {
	ws, err := c.worktreeCapableStore()
	if err != nil {
		return nil, err
	}
	record, err := c.store.Get(selector)
	if err != nil {
		return nil, err
	}
	root := ws.Root()
	capture, err := worktree.CaptureRegistration(ctx, worktree.RunGit, root, c.integrationRef, base)
	if err != nil {
		return nil, err
	}

	input := domain.WorktreeBindingInput{
		TicketID:      record.Ticket.ID,
		Branch:        valueOrEmpty(capture.Branch),
		BaseRef:       valueOrEmpty(capture.BaseRef),
		BaseCommit:    valueOrEmpty(capture.BaseCommit),
		Owner:         strings.TrimSpace(owner),
		OwnerAttested: ownerAttested,
	}
	binding, err := ws.RegisterWorktreeBinding(ctx, input)
	if err != nil {
		return nil, err
	}
	result := WorktreeRegisterResult{Binding: binding, Root: root}
	if capture.Branch.Status != gitcontext.StatusValue {
		result.Notes = append(result.Notes, "branch: "+describeField(capture.Branch))
	}
	if capture.BaseRef.Status != gitcontext.StatusValue {
		result.Notes = append(result.Notes,
			"base_ref: "+describeField(capture.BaseRef)+"; `aira worktree audit` will report merged/unique-commit facts unevaluated for this checkout until one resolves")
	}
	if capture.BaseCommit.Status != gitcontext.StatusValue && capture.BaseRef.Status == gitcontext.StatusValue {
		result.Notes = append(result.Notes, "base_commit: "+describeField(capture.BaseCommit))
	}
	switch {
	case input.Owner == "":
		result.Notes = append(result.Notes, "owner: none recorded; set AIRA_CONFINE_OWNER or pass --owner so a later audit can say who declared this")
	case !ownerAttested:
		result.Notes = append(result.Notes, "owner "+input.Owner+" is INFERRED, not attested: weaker evidence than a session that set AIRA_CONFINE_OWNER")
	}
	return result, nil
}

func (c *Core) worktreeAudit(ctx context.Context, selector, base string) (any, error) {
	ws, err := c.worktreeCapableStore()
	if err != nil {
		return nil, err
	}
	scoped, err := resolveAuditSelector(selector, func(id string) bool {
		_, getErr := c.store.Get(id)
		return getErr == nil
	})
	if err != nil {
		return nil, err
	}
	bindings, err := ws.WorktreeBindings()
	if err != nil {
		return nil, err
	}
	roots, err := ws.WorktreeRoots()
	if err != nil {
		return nil, err
	}
	held, err := ws.ListLeases(ctx)
	if err != nil {
		return nil, err
	}
	leases := make([]worktree.Lease, 0, len(held))
	for _, row := range held {
		if row.Expired {
			continue
		}
		leases = append(leases, worktree.Lease{TicketID: row.TicketID, WorktreeID: row.WorktreeID, Actor: row.Actor})
	}
	auditor := &worktree.Auditor{Git: worktree.RunGit, Identity: store.CanonicalScopeIdentity}
	report, err := auditor.Audit(ctx, worktree.Inputs{
		Root:              ws.Root(),
		CurrentWorktreeID: ws.WorktreeID(),
		Bindings:          bindings,
		Leases:            leases,
		KnownRoots:        roots,
		Prefixes:          ws.TicketPrefixes(),
		ConfigRef:         c.integrationRef,
		BaseOverride:      base,
		Selector:          scoped,
		TicketStatus: func(id string) (string, bool) {
			record, getErr := c.store.Get(id)
			if getErr != nil {
				return "", false
			}
			return string(record.Ticket.Status), true
		},
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// resolveAuditSelector decides whether one selector names a ticket or a path,
// and REFUSES one that could be either. AIRA does not guess between two
// meanings of the same argument.
func resolveAuditSelector(selector string, ticketExists func(string) bool) (worktree.Selector, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return worktree.Selector{}, nil
	}
	isTicket := ticketExists != nil && ticketExists(selector)
	isPath := false
	if info, statErr := os.Stat(selector); statErr == nil && info.IsDir() {
		isPath = true
	}
	switch {
	case isTicket && isPath:
		return worktree.Selector{}, fmt.Errorf("E_SELECTOR_AMBIGUOUS: %q names both a ticket and a directory", selector)
	case isTicket:
		return worktree.Selector{TicketID: selector}, nil
	case isPath:
		absolute, absErr := filepath.Abs(selector)
		if absErr != nil {
			return worktree.Selector{}, fmt.Errorf("E_SELECTOR_INVALID: %v", absErr)
		}
		return worktree.Selector{Path: absolute}, nil
	default:
		return worktree.Selector{}, fmt.Errorf("E_SELECTOR_INVALID: %q is neither a ticket in this project nor an existing directory", selector)
	}
}

func valueOrEmpty(field gitcontext.Field) string {
	if field.Status == gitcontext.StatusValue {
		return field.Value
	}
	return ""
}

func describeField(field gitcontext.Field) string {
	text := string(field.Status)
	if field.Reason != "" {
		text += " (" + field.Reason + ")"
	}
	return text
}
