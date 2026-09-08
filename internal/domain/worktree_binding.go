package domain

import (
	"errors"
	"strings"
)

// WorktreeBindingCodeInvalid is the stable code for a refused binding write.
const WorktreeBindingCodeInvalid = "E_WORKTREE_BINDING_INVALID"

// WorktreeBinding is one AGENT-DECLARED association between a checkout and a
// ticket: "this worktree is where AIRA-176 is being worked".
//
// It is deliberately NOT a classification and NOT a liveness record. Everything
// an audit says about a worktree's git state (uncommitted work, unique commits,
// merged, pushed) is recomputed from git on every read and never stored here —
// the AIRA-175 lesson: a cached derivation that can drift away from the thing it
// derives from is a liability, not a convenience. What a binding stores is only
// what git cannot answer: which ticket an agent SAID this checkout is for, and
// how strong the evidence for that claim is (Owner + OwnerAttested).
//
// Branch/BaseRef/BaseCommit are recorded as observed AT REGISTRATION TIME and
// are therefore hints, never authority: the audit re-reads the live branch and
// HEAD from `git worktree list --porcelain` and uses the recorded BaseRef only
// as one step of its integration-ref resolution chain.
type WorktreeBinding struct {
	WorktreeID string `json:"worktree_id"`
	TicketID   string `json:"ticket_id"`
	// Branch as observed when the binding was written. Empty means the checkout
	// had a detached HEAD, or the branch could not be established.
	Branch string `json:"branch,omitempty"`
	// BaseRef is the integration ref this work branched from, when one was
	// positively established at registration. Empty means NONE was established —
	// never a default. `master`/`main`/`@{upstream}` are never guessed.
	BaseRef string `json:"base_ref,omitempty"`
	// BaseCommit is the merge-base against BaseRef at registration, when BaseRef
	// resolved. Empty otherwise.
	BaseCommit string `json:"base_commit,omitempty"`
	// Owner reuses AIRA_CONFINE_OWNER's identity namespace exactly (no third
	// identity concept). OwnerAttested mirrors runner.ConfineOwnerIsAttested: a
	// binding declared by an unattested caller is visibly weaker evidence rather
	// than silently equal to an attested one.
	Owner         string `json:"owner,omitempty"`
	OwnerAttested bool   `json:"owner_attested"`
	RegisteredAt  string `json:"registered_at"`
}

// WorktreeBindingInput is the client-supplied half of a binding write. The
// project and worktree identities are NOT carried here: they come from the
// receiving store's own scope, so a caller cannot declare a binding for a
// checkout it is not standing in.
type WorktreeBindingInput struct {
	TicketID      string `json:"ticket_id"`
	Branch        string `json:"branch,omitempty"`
	BaseRef       string `json:"base_ref,omitempty"`
	BaseCommit    string `json:"base_commit,omitempty"`
	Owner         string `json:"owner,omitempty"`
	OwnerAttested bool   `json:"owner_attested"`
}

// Validate refuses a binding that names no ticket, and refuses an attested
// claim with no owner to attest. Field-shape validation of Owner itself belongs
// to the identity owner (runner.ValidateConfineIdentity), applied by the face
// that resolves it.
func (input WorktreeBindingInput) Validate() error {
	if strings.TrimSpace(input.TicketID) == "" {
		return errors.New(WorktreeBindingCodeInvalid + ": a binding must name a ticket")
	}
	if input.OwnerAttested && strings.TrimSpace(input.Owner) == "" {
		return errors.New(WorktreeBindingCodeInvalid + ": owner_attested with no owner")
	}
	return nil
}
