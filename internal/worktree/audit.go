package worktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// CodeScan is the stable error code for an audit that could not even enumerate
// the repository's checkouts. Anything narrower than that is reported as an
// unevaluated fact inside a successful report, never as a command failure.
const CodeScan = "E_WORKTREE_AUDIT"

// IdentityCall resolves AIRA's worktree identity for one checkout's git
// directories. It is INJECTED rather than reimplemented here: the audit joins
// discovered checkouts to `worktree_bindings` rows written under the store's own
// identity, and a second copy of that canonicalisation is exactly how such a
// join silently stops matching. (The plan review's F3 caught this: the store's
// own worktree SCANNER assigns a path digest placeholder, `worktree-<digest>`,
// which is not the binding key at all — so the placeholder must never be used
// for this join.)
type IdentityCall func(commonDir, gitDir string) (projectID, worktreeID string, err error)

// Selector scopes an audit. Exactly one form may be set; the caller resolves
// which, and an input that could be either is refused before it reaches here.
type Selector struct {
	TicketID string
	Path     string
}

// Inputs are everything the audit reads that is not git. Passing them in rather
// than reading them here keeps the whole classifier testable without a database.
type Inputs struct {
	// Root is the repository root to enumerate from.
	Root string
	// CurrentWorktreeID marks which checkout the caller is standing in.
	CurrentWorktreeID string
	Bindings          []domain.WorktreeBinding
	Leases            []Lease
	// TicketStatus reports a ticket's live status. ok=false means this project
	// has no such ticket, which is what stops an inferred guess being promoted.
	TicketStatus func(ticketID string) (status string, ok bool)
	// KnownRoots maps worktree identity to the last path AIRA saw it at, for
	// naming orphaned bindings.
	KnownRoots map[string]string
	// Prefixes are the project's ticket ID prefixes, e.g. ["AIRA"].
	Prefixes []string
	// ConfigRef is `.aira/config` -> git.integration_ref, empty when unset.
	ConfigRef string
	// BaseOverride is an explicit --base on this invocation.
	BaseOverride string
	Selector     Selector
}

// Auditor runs one audit.
type Auditor struct {
	Git      GitCall
	Identity IdentityCall
	Budget   time.Duration
	Now      func() time.Time
}

func (a *Auditor) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Auditor) git() GitCall {
	if a.Git != nil {
		return a.Git
	}
	return RunGit
}

// Audit enumerates the repository's checkouts, joins each to its declared or
// inferred tickets, recomputes every git fact live, and composes a
// recommendation that names the facts it rests on.
func (a *Auditor) Audit(ctx context.Context, in Inputs) (Report, error) {
	if strings.TrimSpace(in.Root) == "" {
		return Report{}, errors.New(CodeScan + ": audit needs a repository root")
	}
	if a.Identity == nil {
		return Report{}, errors.New(CodeScan + ": audit needs a worktree identity resolver")
	}
	budget := a.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	deadline := a.now().Add(budget)

	// An empty list, never null: "no checkout matched" is an answer, and a null
	// here would read to a consumer as "the audit produced nothing".
	report := Report{Root: in.Root, Worktrees: []Entry{}}
	out, stderr, err := a.git()(ctx, in.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return Report{}, fmt.Errorf("%s: %s", CodeScan, gitFailure("worktree list", stderr, err))
	}
	checkouts := parseWorktreeList(out)
	if len(checkouts) == 0 {
		return Report{}, errors.New(CodeScan + ": git reported no worktrees for " + in.Root)
	}

	// The repository-level integration ref (chain steps 1, 3, 4). Step 2 is
	// per-worktree and applied below.
	repoRef, refErr := a.resolveRepoIntegrationRef(ctx, in)
	if refErr != nil {
		return Report{}, refErr
	}
	report.IntegrationRef = repoRef
	report.Notes = append(report.Notes,
		"every git fact below is recomputed on each run; nothing about a worktree's state is stored",
		"`pushed_to_any_remote` is containment in a refs/remotes/* ref, which is only as fresh as the last fetch",
		"`has_uncommitted_changes` sees tracked modifications and untracked files; ignored files are invisible to it",
		"AIRA never removes a worktree and never deletes a binding: act on this report yourself")

	bindingsByWorktree := map[string][]domain.WorktreeBinding{}
	for _, binding := range in.Bindings {
		bindingsByWorktree[binding.WorktreeID] = append(bindingsByWorktree[binding.WorktreeID], binding)
	}
	leasesByTicket := map[string]Lease{}
	leasesByWorktree := map[string][]Lease{}
	for _, lease := range in.Leases {
		leasesByTicket[lease.TicketID] = lease
		leasesByWorktree[lease.WorktreeID] = append(leasesByWorktree[lease.WorktreeID], lease)
	}

	seen := map[string]bool{}
	budgetSpent := false
	for _, raw := range checkouts {
		entry := a.identify(ctx, raw, in)
		if entry.WorktreeID.Status == gitcontext.StatusValue {
			seen[entry.WorktreeID.Value] = true
		}
		explicit := bindingsByWorktree[entry.WorktreeID.Value]
		if !a.selects(entry, explicit, in) {
			continue
		}
		if !budgetSpent && !a.now().Before(deadline) {
			budgetSpent = true
			report.Notes = append(report.Notes,
				fmt.Sprintf("time budget of %s elapsed; remaining checkouts are reported unevaluated", budget))
		}
		a.populate(ctx, &entry, explicit, in, leasesByTicket, leasesByWorktree, report.IntegrationRef, budgetSpent)
		report.Worktrees = append(report.Worktrees, entry)
	}

	for _, binding := range in.Bindings {
		if seen[binding.WorktreeID] {
			continue
		}
		if in.Selector.TicketID != "" && !strings.EqualFold(in.Selector.TicketID, binding.TicketID) {
			continue
		}
		if in.Selector.Path != "" {
			continue
		}
		report.OrphanBindings = append(report.OrphanBindings, OrphanBinding{
			WorktreeID: binding.WorktreeID, TicketID: binding.TicketID, Branch: binding.Branch,
			Owner: binding.Owner, OwnerAttested: binding.OwnerAttested, RegisteredAt: binding.RegisteredAt,
			LastKnownRoot: in.KnownRoots[binding.WorktreeID],
		})
	}
	sort.SliceStable(report.OrphanBindings, func(i, j int) bool {
		if report.OrphanBindings[i].WorktreeID != report.OrphanBindings[j].WorktreeID {
			return report.OrphanBindings[i].WorktreeID < report.OrphanBindings[j].WorktreeID
		}
		return report.OrphanBindings[i].TicketID < report.OrphanBindings[j].TicketID
	})
	if in.Selector.Path != "" && len(report.Worktrees) == 0 {
		return Report{}, fmt.Errorf("E_SELECTOR_INVALID: %s names no checkout of %s", in.Selector.Path, in.Root)
	}
	return report, nil
}

// identify resolves the one fact the join depends on — this checkout's AIRA
// worktree identity — plus the free porcelain fields.
func (a *Auditor) identify(ctx context.Context, raw checkout, in Inputs) Entry {
	entry := Entry{
		Path: raw.path, Bare: raw.bare, Detached: raw.detached,
		Locked: raw.locked, Prunable: raw.prunable, Confidence: ConfidenceNone,
	}
	if raw.branch != "" {
		entry.Branch = fieldValue(shortBranch(raw.branch))
	} else if raw.detached {
		entry.Branch = fieldNone("detached HEAD")
	} else if raw.bare {
		entry.Branch = fieldNone("bare checkout")
	} else {
		entry.Branch = fieldNone("git reported no branch")
	}
	if raw.head != "" {
		entry.Head = fieldValue(raw.head)
	} else {
		entry.Head = fieldNone("git reported no HEAD")
	}

	out, stderr, err := a.git()(ctx, raw.path, "rev-parse", "--git-dir", "--git-common-dir")
	if err != nil {
		entry.WorktreeID = fieldUnevaluated(gitFailure("rev-parse git dirs", stderr, err))
		return entry
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		entry.WorktreeID = fieldUnevaluated("rev-parse git dirs: git named fewer than two directories")
		return entry
	}
	gitDir := absoluteFrom(raw.path, strings.TrimSpace(lines[0]))
	commonDir := absoluteFrom(raw.path, strings.TrimSpace(lines[1]))
	_, worktreeID, identityErr := a.Identity(commonDir, gitDir)
	if identityErr != nil {
		entry.WorktreeID = fieldUnevaluated("worktree identity: " + identityErr.Error())
		return entry
	}
	entry.WorktreeID = fieldValue(worktreeID)
	if _, mainID, mainErr := a.Identity(commonDir, commonDir); mainErr == nil {
		entry.Main = mainID == worktreeID
	}
	entry.Current = in.CurrentWorktreeID != "" && in.CurrentWorktreeID == worktreeID
	return entry
}

func absoluteFrom(root, path string) string {
	if path == "" {
		return root
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

// selects applies the selector using only facts that cost no per-worktree git
// work, so scoping an audit to one ticket does not sweep every checkout's log.
func (a *Auditor) selects(entry Entry, explicit []domain.WorktreeBinding, in Inputs) bool {
	switch {
	case in.Selector.Path != "":
		want, err := filepath.Abs(in.Selector.Path)
		if err != nil {
			return false
		}
		return filepath.Clean(entry.Path) == filepath.Clean(want) || sameFilePath(entry.Path, want)
	case in.Selector.TicketID != "":
		for _, binding := range explicit {
			if strings.EqualFold(binding.TicketID, in.Selector.TicketID) {
				return true
			}
		}
		for _, candidate := range branchNameCandidates(entry.Branch.Value, in.Prefixes) {
			if strings.EqualFold(candidate, in.Selector.TicketID) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// populate computes every live fact and the recommendation for one checkout.
func (a *Auditor) populate(ctx context.Context, entry *Entry, explicit []domain.WorktreeBinding, in Inputs,
	leasesByTicket map[string]Lease, leasesByWorktree map[string][]Lease, repoRef gitcontext.Field, budgetSpent bool) {

	if budgetSpent {
		reason := ErrBudgetElapsed.Error()
		entry.Facts = Facts{
			IntegrationRef:        fieldUnevaluated(reason),
			HasUncommittedChanges: flagUnevaluated(reason),
			UniqueCommitCount:     countUnevaluated(reason),
			MergedIntoIntegration: flagUnevaluated(reason),
			PushedToAnyRemote:     flagUnevaluated(reason),
			LiveLease:             flagUnevaluated(reason),
		}
		entry.Bindings, entry.Confidence = explicitBindings(explicit, in, leasesByTicket, entry), confidenceFor(explicit)
		entry.Recommendation = recommend(*entry)
		return
	}

	entry.Facts.IntegrationRef = a.integrationRefFor(ctx, *entry, explicit, in, repoRef)
	entry.Facts.HasUncommittedChanges = a.uncommitted(ctx, *entry)
	entry.Facts.PushedToAnyRemote = a.pushed(ctx, *entry)
	entry.Facts.UniqueCommitCount = a.uniqueCommits(ctx, *entry)
	entry.Facts.MergedIntoIntegration = a.merged(ctx, *entry)

	entry.Bindings = explicitBindings(explicit, in, leasesByTicket, entry)
	entry.Confidence = confidenceFor(explicit)
	if len(entry.Bindings) == 0 {
		inferred, rejected := a.infer(ctx, *entry, in)
		entry.Bindings = decorate(inferred, in, leasesByTicket, entry)
		entry.InferenceRejected = rejected
		if len(entry.Bindings) > 0 {
			entry.Confidence = ConfidenceInferred
		}
	}
	entry.Facts.LiveLease = liveLeaseFact(*entry, leasesByWorktree)
	if entry.Bindings == nil {
		// An empty list, never null: "this checkout is bound to no ticket" is an
		// answer the reader must be able to see.
		entry.Bindings = []TicketBinding{}
	}
	entry.Recommendation = recommend(*entry)
}

func confidenceFor(explicit []domain.WorktreeBinding) Confidence {
	if len(explicit) > 0 {
		return ConfidenceExplicit
	}
	return ConfidenceNone
}

func explicitBindings(explicit []domain.WorktreeBinding, in Inputs, leases map[string]Lease, entry *Entry) []TicketBinding {
	bindings := make([]TicketBinding, 0, len(explicit))
	for _, binding := range explicit {
		bindings = append(bindings, TicketBinding{
			TicketID: binding.TicketID, Confidence: ConfidenceExplicit, Source: SourceRegistered,
			Branch: binding.Branch, BaseRef: binding.BaseRef, BaseCommit: binding.BaseCommit,
			Owner: binding.Owner, OwnerAttested: binding.OwnerAttested, RegisteredAt: binding.RegisteredAt,
		})
	}
	return decorate(bindings, in, leases, entry)
}

// decorate stamps the live ticket status and lease facts onto each binding.
func decorate(bindings []TicketBinding, in Inputs, leases map[string]Lease, entry *Entry) []TicketBinding {
	for index := range bindings {
		status, ok := "", false
		if in.TicketStatus != nil {
			status, ok = in.TicketStatus(bindings[index].TicketID)
		}
		if in.TicketStatus == nil {
			bindings[index].TicketStatus = fieldUnevaluated("ticket status unavailable")
		} else if ok {
			bindings[index].TicketStatus = fieldValue(status)
		} else {
			bindings[index].TicketStatus = fieldNone("no such ticket in this project")
		}
		if lease, held := leases[bindings[index].TicketID]; held {
			bindings[index].LiveLease = flagValue(true)
			bindings[index].LeaseWorktreeID = lease.WorktreeID
			bindings[index].LeaseActor = lease.Actor
			bindings[index].LeaseHeldHere = entry.WorktreeID.Status == gitcontext.StatusValue &&
				lease.WorktreeID == entry.WorktreeID.Value
		} else {
			bindings[index].LiveLease = flagValue(false)
		}
	}
	sort.SliceStable(bindings, func(i, j int) bool { return bindings[i].TicketID < bindings[j].TicketID })
	return bindings
}

// liveLeaseFact is true when a live lease names this checkout, or names a
// ticket bound to it. A lease held elsewhere on a bound ticket still counts:
// someone is working that ticket right now and this checkout is claimed by it.
func liveLeaseFact(entry Entry, leasesByWorktree map[string][]Lease) Flag {
	if entry.WorktreeID.Status == gitcontext.StatusValue {
		if len(leasesByWorktree[entry.WorktreeID.Value]) > 0 {
			return flagValue(true)
		}
	}
	for _, binding := range entry.Bindings {
		if binding.LiveLease.True() {
			return flagValue(true)
		}
	}
	if entry.WorktreeID.Status != gitcontext.StatusValue {
		return flagUnevaluated("worktree identity unresolved, so a lease on this checkout cannot be found")
	}
	return flagValue(false)
}

func sameFilePath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return filepath.Clean(leftResolved) == filepath.Clean(rightResolved)
}
