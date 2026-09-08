package worktree

import (
	"fmt"
	"sort"
	"strings"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// closedStatuses are the ticket states that make "this checkout's work has been
// overtaken" plausible enough to say out loud — and only ever as "verify the
// ticket's actual PR before removing", never as a removal verdict.
var closedStatuses = map[string]bool{
	string(domain.StatusDone):       true,
	string(domain.StatusRetired):    true,
	string(domain.StatusSuperseded): true,
}

// recommend composes the buckets from the established facts.
//
// Two rules govern the whole function, and they are what make it safe:
//
//  1. Every removal-adjacent bucket demands POSITIVELY ESTABLISHED facts. An
//     unevaluated fact is neither true nor false, so it cannot satisfy a
//     precondition — a check that cannot establish its result never produces a
//     fake pass. `Flag.False()` exists precisely so a bucket must ask for a
//     PROVEN negative rather than accept `!True()`.
//
//  2. The recovery bucket is checked FIRST among the git buckets, and it needs
//     no integration ref. So the case where being wrong actually loses work —
//     a checkout holding work nothing else has a copy of — stays evaluable even
//     when the integration ref does not resolve, while every "safe to remove"
//     bucket goes silent. The asymmetry is deliberate and is in the safe
//     direction.
func recommend(entry Entry) Recommendation {
	facts := entry.Facts

	if entry.Main {
		return Recommendation{
			Code: RecommendMainWorktree,
			Text: "the repository's main checkout — never a removal candidate",
			Because: []string{
				"its git directory IS the repository common directory",
				"`git worktree remove` cannot remove it, and deleting it destroys the repository",
			},
		}
	}
	if facts.LiveLease.True() {
		return Recommendation{
			Code:    RecommendActive,
			Text:    "active — leave it",
			Because: append([]string{"a currently-live lease names this checkout or a ticket bound to it"}, leaseHolders(entry)...),
		}
	}

	if facts.HasUncommittedChanges.True() {
		return Recommendation{
			Code: RecommendRecover,
			Text: "holds work nothing else has a copy of — recover before removing",
			Because: []string{
				"`git status --porcelain` is non-empty (tracked modifications or untracked files)",
				"recover by committing to a rescue branch and pushing it, or by taking a named-ref stash (`git stash create` + `git update-ref refs/agent-stash/<name>`), before the worktree goes",
			},
		}
	}
	if facts.UniqueCommitCount.Known() && facts.UniqueCommitCount.Value > 0 && facts.PushedToAnyRemote.False() {
		return Recommendation{
			Code: RecommendRecover,
			Text: "holds work nothing else has a copy of — recover before removing",
			Because: []string{
				fmt.Sprintf("%d commit(s) not in %s", facts.UniqueCommitCount.Value, facts.IntegrationRef.Value),
				"HEAD is contained in no refs/remotes/* ref, so no remote has a copy",
				"push the branch somewhere before removing the worktree",
			},
		}
	}

	// Everything below recommends, or edges toward, removal. Each demands a
	// proven absence of a live lease and a proven absence of local changes.
	blocked := removalPreconditions(facts)
	if len(blocked) == 0 {
		if facts.MergedIntoIntegration.True() {
			return Recommendation{
				Code: RecommendMerged,
				Text: "merged — safe to remove",
				Because: []string{
					fmt.Sprintf("HEAD is an ancestor of %s (`merge-base --is-ancestor`)", facts.IntegrationRef.Value),
					"no live lease names this checkout",
					"no uncommitted changes and no untracked files",
				},
			}
		}
		if facts.UniqueCommitCount.Known() && facts.UniqueCommitCount.Value == 0 {
			return Recommendation{
				Code: RecommendNothingToLose,
				Text: "nothing to lose — safe to remove",
				Because: []string{
					fmt.Sprintf("no commits of its own relative to %s", facts.IntegrationRef.Value),
					"no uncommitted changes and no untracked files",
					"no live lease names this checkout",
				},
			}
		}
		if facts.UniqueCommitCount.Known() && facts.UniqueCommitCount.Value > 0 && facts.PushedToAnyRemote.True() {
			if closed := closedBindings(entry); len(closed) > 0 {
				return Recommendation{
					Code: RecommendSuperseded,
					Text: "looks superseded — verify the ticket's actual PR before removing",
					Because: append([]string{
						fmt.Sprintf("%d commit(s) not in %s, but HEAD is contained in a refs/remotes/* ref, so a remote has a copy",
							facts.UniqueCommitCount.Value, facts.IntegrationRef.Value),
						"no live lease names this checkout",
					}, closed...),
				}
			}
		}
	}

	return Recommendation{
		Code:    RecommendNone,
		Text:    "no recommendation — the facts needed for one were not established",
		Because: append(blocked, unevaluatedReasons(facts)...),
	}
}

// removalPreconditions returns the reasons no removal-adjacent recommendation
// may be made. An empty result means both preconditions were PROVEN, not merely
// not-disproven.
func removalPreconditions(facts Facts) []string {
	var blocked []string
	if !facts.LiveLease.False() {
		blocked = append(blocked, "live_lease was not established false: "+describe(facts.LiveLease))
	}
	if !facts.HasUncommittedChanges.False() {
		blocked = append(blocked, "has_uncommitted_changes was not established false: "+describe(facts.HasUncommittedChanges))
	}
	return blocked
}

func describe(flag Flag) string {
	switch flag.Status {
	case gitcontext.StatusValue:
		if flag.Value {
			return "true"
		}
		return "false"
	case gitcontext.StatusNone:
		if flag.Reason != "" {
			return "none (" + flag.Reason + ")"
		}
		return "none"
	default:
		if flag.Reason != "" {
			return "unevaluated (" + flag.Reason + ")"
		}
		return "unevaluated"
	}
}

func unevaluatedReasons(facts Facts) []string {
	var reasons []string
	if facts.IntegrationRef.Status != gitcontext.StatusValue && facts.IntegrationRef.Reason != "" {
		reasons = append(reasons, "integration_ref: "+facts.IntegrationRef.Reason)
	}
	if !facts.UniqueCommitCount.Known() && facts.UniqueCommitCount.Reason != "" {
		reasons = append(reasons, "unique_commit_count: "+facts.UniqueCommitCount.Reason)
	}
	if facts.MergedIntoIntegration.Status == gitcontext.StatusUnevaluated && facts.MergedIntoIntegration.Reason != "" {
		reasons = append(reasons, "merged_into_integration: "+facts.MergedIntoIntegration.Reason)
	}
	if facts.PushedToAnyRemote.Status == gitcontext.StatusUnevaluated && facts.PushedToAnyRemote.Reason != "" {
		reasons = append(reasons, "pushed_to_any_remote: "+facts.PushedToAnyRemote.Reason)
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "no bucket's preconditions were all established")
	}
	return reasons
}

func closedBindings(entry Entry) []string {
	var closed []string
	for _, binding := range entry.Bindings {
		// A status that was not established cannot support "this work has been
		// overtaken". Skipping it instead would let one done ticket carry the
		// bucket for a checkout whose other ticket AIRA could not read at all.
		if binding.TicketStatus.Status != gitcontext.StatusValue {
			return nil
		}
		if !closedStatuses[strings.ToLower(binding.TicketStatus.Value)] {
			return nil
		}
		closed = append(closed, fmt.Sprintf("bound ticket %s is %s (%s binding)",
			binding.TicketID, binding.TicketStatus.Value, binding.Confidence))
	}
	sort.Strings(closed)
	return closed
}

func leaseHolders(entry Entry) []string {
	var held []string
	for _, binding := range entry.Bindings {
		if !binding.LiveLease.True() {
			continue
		}
		where := "another checkout"
		if binding.LeaseHeldHere {
			where = "this checkout"
		}
		actor := binding.LeaseActor
		if actor == "" {
			actor = "an unnamed actor"
		}
		held = append(held, fmt.Sprintf("%s is leased by %s from %s", binding.TicketID, actor, where))
	}
	sort.Strings(held)
	return held
}
