// Package worktree classifies the git state of a repository's checkouts and
// joins each to the ticket an agent declared it is for.
//
// The whole package is a READ. It never removes a worktree, never writes a
// classification back, and never caches a git-derived fact: `aira worktree
// audit` recomputes every fact from git on every run. That is deliberate —
// AIRA-175 was a derived index that quietly disagreed with the files it was
// meant to reflect, and a stale "safe to remove" is the one failure here that
// actually destroys work.
//
// Every fact is a tri-state (`value` / `none` / `unevaluated`) borrowed from
// internal/gitcontext, which is already this codebase's vocabulary for "I could
// not establish this". A check that cannot establish its result reports
// `unevaluated`, never a fake pass — so an unevaluated fact SUPPRESSES the
// removal recommendations it would otherwise support, while leaving the
// recovery recommendation (the one whose being wrong loses work) evaluable.
package worktree

import "aira/internal/gitcontext"

// Flag is a tri-state boolean fact.
//
// Value is deliberately NOT omitempty. An established false is a RESULT — "this
// checkout has no uncommitted changes" is the whole basis of a removal
// recommendation — and eliding it would render identically to a fact that was
// never established, which is precisely the distinction this type exists to
// keep.
type Flag struct {
	Value  bool              `json:"value"`
	Status gitcontext.Status `json:"status"`
	Reason string            `json:"reason,omitempty"`
}

// Count is a tri-state integer fact. Value is not omitempty for the same reason
// Flag's is not: zero unique commits is an answer, not an absence.
type Count struct {
	Value  int               `json:"value"`
	Status gitcontext.Status `json:"status"`
	Reason string            `json:"reason,omitempty"`
}

// True reports a POSITIVELY ESTABLISHED true. An unevaluated flag is never
// true, which is what makes the removal buckets fail closed.
func (f Flag) True() bool { return f.Status == gitcontext.StatusValue && f.Value }

// False reports a POSITIVELY ESTABLISHED false. Deliberately not !True(): an
// unevaluated flag is neither true nor false, and a bucket that needs a proven
// negative must ask for one.
func (f Flag) False() bool { return f.Status == gitcontext.StatusValue && !f.Value }

// Known reports whether the count was established.
func (c Count) Known() bool { return c.Status == gitcontext.StatusValue }

func flagValue(value bool) Flag { return Flag{Value: value, Status: gitcontext.StatusValue} }

func flagUnevaluated(reason string) Flag {
	return Flag{Status: gitcontext.StatusUnevaluated, Reason: reason}
}

func countValue(value int) Count { return Count{Value: value, Status: gitcontext.StatusValue} }

func countUnevaluated(reason string) Count {
	return Count{Status: gitcontext.StatusUnevaluated, Reason: reason}
}

func fieldValue(value string) gitcontext.Field {
	return gitcontext.Field{Value: value, Status: gitcontext.StatusValue}
}

func fieldNone(reason string) gitcontext.Field {
	return gitcontext.Field{Status: gitcontext.StatusNone, Reason: reason}
}

func fieldUnevaluated(reason string) gitcontext.Field {
	return gitcontext.Field{Status: gitcontext.StatusUnevaluated, Reason: reason}
}

// Confidence grades HOW a ticket came to be associated with a checkout. It is
// never promoted: an inferred association stays labelled inferred no matter how
// convincing the evidence looks.
type Confidence string

const (
	// ConfidenceExplicit is a `worktree_bindings` row an agent wrote.
	ConfidenceExplicit Confidence = "explicit"
	// ConfidenceInferred is derived from this project's own naming conventions.
	ConfidenceInferred Confidence = "inferred"
	// ConfidenceNone means no association was established. A worktree with no
	// ticket reports none — never a forced guess.
	ConfidenceNone Confidence = "none"
)

// Inference sources, reported verbatim so a reader can re-derive the guess.
const (
	SourceRegistered   = "registered"
	SourceBranchName   = "branch-name"
	SourceCommitPrefix = "commit-prefix"
)

// TicketBinding is one ticket associated with one checkout.
type TicketBinding struct {
	TicketID   string     `json:"ticket_id"`
	Confidence Confidence `json:"confidence"`
	Source     string     `json:"source"`
	// The remaining fields are populated only for an explicit binding: they are
	// what the agent recorded at registration, not live facts.
	Branch        string `json:"branch,omitempty"`
	BaseRef       string `json:"base_ref,omitempty"`
	BaseCommit    string `json:"base_commit,omitempty"`
	Owner         string `json:"owner,omitempty"`
	OwnerAttested bool   `json:"owner_attested"`
	RegisteredAt  string `json:"registered_at,omitempty"`
	// TicketStatus is the bound ticket's own live status, or none when the
	// ticket is not in this project.
	TicketStatus gitcontext.Field `json:"ticket_status"`
	// LiveLease says whether a currently-live lease is held on this ticket, and
	// from which worktree.
	LiveLease        Flag   `json:"live_lease"`
	LeaseWorktreeID  string `json:"lease_worktree_id,omitempty"`
	LeaseActor       string `json:"lease_actor,omitempty"`
	LeaseHeldHere    bool   `json:"lease_held_here,omitempty"`
	LeaseHeldEndOnly bool   `json:"-"`
}

// Facts is the independently-named evidence an audit establishes per checkout.
// Nothing here is stored; all of it is recomputed per run.
type Facts struct {
	// IntegrationRef is the ref "merged" and "unique commits" are measured
	// against, resolved by the fail-closed chain in resolveIntegrationRef.
	IntegrationRef gitcontext.Field `json:"integration_ref"`
	// HasUncommittedChanges is `git status --porcelain` non-empty: tracked
	// modifications OR untracked files. Ignored files are invisible to it, which
	// is an accepted gap, not a claim of emptiness.
	HasUncommittedChanges Flag `json:"has_uncommitted_changes"`
	// UniqueCommitCount is `git rev-list --count <integration>..<head>`.
	UniqueCommitCount Count `json:"unique_commit_count"`
	// MergedIntoIntegration is `git merge-base --is-ancestor <head> <integration>`.
	MergedIntoIntegration Flag `json:"merged_into_integration"`
	// PushedToAnyRemote is containment of HEAD in any refs/remotes/* ref. It
	// needs no integration ref, which is why the recovery bucket stays evaluable
	// when the integration ref does not resolve.
	PushedToAnyRemote Flag `json:"pushed_to_any_remote"`
	// LiveLease is true when ANY currently-live lease names this checkout, or
	// names a ticket bound to it.
	LiveLease Flag `json:"live_lease"`
}

// Recommendation codes. A recommendation is a CONCLUSION the reader can
// re-derive from Because, never an opaque verdict.
const (
	RecommendActive        = "active"
	RecommendRecover       = "recover"
	RecommendNothingToLose = "nothing-to-lose"
	RecommendMerged        = "merged"
	RecommendSuperseded    = "superseded"
	RecommendMainWorktree  = "main-worktree"
	RecommendNone          = "none"
)

// Recommendation names what was proven and what follows from it, in that order.
type Recommendation struct {
	Code string `json:"code"`
	Text string `json:"text"`
	// Because lists the established facts this rests on, phrased so the reader
	// can check each one against the Facts block above it.
	Because []string `json:"because"`
}

// Entry is one audited checkout.
type Entry struct {
	Path       string           `json:"path"`
	WorktreeID gitcontext.Field `json:"worktree_id"`
	Branch     gitcontext.Field `json:"branch"`
	Head       gitcontext.Field `json:"head"`
	// Main is the repository's primary checkout — its git dir IS the common dir.
	// It is never a removal candidate: `git worktree remove` cannot remove it and
	// deleting it destroys the repository.
	Main     bool `json:"main"`
	Current  bool `json:"current"`
	Bare     bool `json:"bare"`
	Detached bool `json:"detached"`
	Locked   bool `json:"locked"`
	Prunable bool `json:"prunable"`

	Confidence Confidence      `json:"binding_confidence"`
	Bindings   []TicketBinding `json:"bindings"`
	// InferenceRejected records a convention match that named a ticket this
	// project does not have. Reported rather than silently dropped, and never
	// promoted to a binding.
	InferenceRejected []string `json:"inference_rejected,omitempty"`

	Facts          Facts          `json:"facts"`
	Recommendation Recommendation `json:"recommendation"`
}

// OrphanBinding is a declared binding whose checkout `git worktree list` no
// longer reports.
//
// AIRA never deletes a binding: there is no unregister verb and no gc, so the
// only removal is the projects FK cascade on `aira eject`. That is a deliberate
// choice, not an omission — a deletion path is a second way to lose the record
// of where work was done, for a feature whose entire purpose is not to lose
// work. The cost is exactly this: an orphan row, which is REPORTED rather than
// quietly swept.
type OrphanBinding struct {
	WorktreeID    string `json:"worktree_id"`
	TicketID      string `json:"ticket_id"`
	Branch        string `json:"branch,omitempty"`
	Owner         string `json:"owner,omitempty"`
	OwnerAttested bool   `json:"owner_attested"`
	RegisteredAt  string `json:"registered_at"`
	// LastKnownRoot is the path the `worktrees` registry last saw that identity
	// at, or empty when AIRA never recorded one. It is a last-known location,
	// explicitly not a claim the path still exists.
	LastKnownRoot string `json:"last_known_root,omitempty"`
}

// Report is the whole audit.
type Report struct {
	Root string `json:"root"`
	// IntegrationRef is the repository-level resolution (chain steps 1, 3, 4).
	// A per-worktree binding can still override it via step 2, so each entry
	// carries its own.
	IntegrationRef gitcontext.Field `json:"integration_ref"`
	Worktrees      []Entry          `json:"worktrees"`
	OrphanBindings []OrphanBinding  `json:"orphan_bindings,omitempty"`
	// Notes carry audit-wide honesty statements — the elapsed time budget, an
	// unreadable ref store — that belong to no single entry.
	Notes []string `json:"notes,omitempty"`
}

// Lease is the live-lease projection the audit needs. It is passed in rather
// than read here so classification is testable without a database.
type Lease struct {
	TicketID   string
	WorktreeID string
	Actor      string
}
