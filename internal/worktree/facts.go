package worktree

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// integrationRefRemedy is stated on every unevaluated integration ref, because
// an unevaluated fact whose remedy is unnamed is a dead end for the reader.
const integrationRefRemedy = "set `git.integration_ref` in .aira/config, pass --base <ref>, or run `git remote set-head origin -a`"

// resolveIntegrationRef implements steps 1, 3, 4 and 5 of the plan's chain.
// Step 2 — the base_ref recorded on this worktree's own bindings — is
// per-worktree and lives in integrationRefFor.
//
// The two banned answers are load-bearing, both measured on this repository:
//
//   - `@{upstream}` / `branch.<name>.merge` is configured for most local
//     branches here and, for a feature branch, points at ITSELF
//     (`branch.aira29-dynamic-reserve.merge = refs/heads/aira29-dynamic-reserve`).
//     Comparing a branch to its own pushed copy yields zero unique commits and
//     `is-ancestor` true for every pushed branch — a blanket false "merged —
//     safe to remove" across the whole population. It is never consulted.
//   - `master`/`main`/`init.defaultBranch` are guesses. A guessed integration
//     ref produces a confident wrong answer in exactly the direction that loses
//     work, so the chain ends in `unevaluated` instead.
func (a *Auditor) resolveRepoIntegrationRef(ctx context.Context, in Inputs) (gitcontext.Field, error) {
	if override := strings.TrimSpace(in.BaseOverride); override != "" {
		if a.refResolves(ctx, in.Root, override) {
			return fieldValue(override), nil
		}
		return gitcontext.Field{}, fmt.Errorf("E_SELECTOR_INVALID: --base %s does not resolve to a commit in %s", override, in.Root)
	}
	if configured := strings.TrimSpace(in.ConfigRef); configured != "" {
		if a.refResolves(ctx, in.Root, configured) {
			return fieldValue(configured), nil
		}
		// A configured-but-broken value is a positively established
		// CONFIGURATION. Falling through to origin/HEAD would silently paper
		// over the misconfiguration and answer from a source the operator did
		// not choose, so the chain stops here.
		return fieldUnevaluated(fmt.Sprintf("git.integration_ref %q does not resolve; point it at a ref that exists", configured)), nil
	}
	out, _, err := a.git()(ctx, in.Root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			return fieldValue(ref), nil
		}
	}
	return fieldUnevaluated("no integration ref established: " + integrationRefRemedy), nil
}

// integrationRefFor applies chain step 2 — the base_ref this worktree's own
// bindings recorded — between the explicit override and the configured value.
func (a *Auditor) integrationRefFor(ctx context.Context, entry Entry, explicit []domain.WorktreeBinding,
	in Inputs, repoRef gitcontext.Field) gitcontext.Field {

	if strings.TrimSpace(in.BaseOverride) != "" {
		return repoRef
	}
	recorded := map[string][]string{}
	for _, binding := range explicit {
		if ref := strings.TrimSpace(binding.BaseRef); ref != "" {
			recorded[ref] = append(recorded[ref], binding.TicketID)
		}
	}
	switch len(recorded) {
	case 0:
		return repoRef
	case 1:
		for ref := range recorded {
			if a.refResolves(ctx, entry.Path, ref) {
				return fieldValue(ref)
			}
			return fieldUnevaluated(fmt.Sprintf("recorded base_ref %q no longer resolves; %s", ref, integrationRefRemedy))
		}
	}
	// Several bindings on one checkout recorded DIFFERENT integration refs.
	// There is no basis for preferring one, and picking either would answer a
	// question with an arbitrary premise — so the chain refuses rather than
	// falling through to a value none of them named. Ambiguity is refused here
	// exactly as it is for selectors.
	pairs := make([]string, 0, len(recorded))
	for ref, tickets := range recorded {
		sort.Strings(tickets)
		pairs = append(pairs, strings.Join(tickets, "/")+" records "+ref)
	}
	sort.Strings(pairs)
	return fieldUnevaluated("binding base_ref conflict: " + strings.Join(pairs, ", ") + "; pass --base <ref> to settle it")
}

func (a *Auditor) refResolves(ctx context.Context, dir, ref string) bool {
	_, _, err := a.git()(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

func (a *Auditor) uncommitted(ctx context.Context, entry Entry) Flag {
	if entry.Bare {
		return Flag{Status: gitcontext.StatusNone, Reason: "bare checkout has no working tree"}
	}
	out, stderr, err := a.git()(ctx, entry.Path, "status", "--porcelain")
	if err != nil {
		return flagUnevaluated(gitFailure("status --porcelain", stderr, err))
	}
	return flagValue(strings.TrimSpace(out) != "")
}

// pushed asks whether HEAD is contained in ANY remote-tracking ref. It needs no
// integration ref, which is the whole reason the recovery recommendation stays
// evaluable when the integration ref does not resolve — and the recovery
// recommendation is the one whose being wrong actually loses work.
func (a *Auditor) pushed(ctx context.Context, entry Entry) Flag {
	if entry.Head.Status != gitcontext.StatusValue {
		return flagUnevaluated("HEAD unresolved, so remote containment cannot be established")
	}
	out, stderr, err := a.git()(ctx, entry.Path, "for-each-ref", "--format=%(refname)", "--contains="+entry.Head.Value, "refs/remotes/")
	if err != nil {
		return flagUnevaluated(gitFailure("for-each-ref --contains", stderr, err))
	}
	return flagValue(strings.TrimSpace(out) != "")
}

func (a *Auditor) uniqueCommits(ctx context.Context, entry Entry) Count {
	ref, head, reason := measurable(entry)
	if reason != "" {
		return countUnevaluated(reason)
	}
	out, stderr, err := a.git()(ctx, entry.Path, "rev-list", "--count", ref+".."+head)
	if err != nil {
		return countUnevaluated(gitFailure("rev-list --count", stderr, err))
	}
	count, parseErr := strconv.Atoi(strings.TrimSpace(out))
	if parseErr != nil {
		return countUnevaluated("rev-list --count: git did not report a number")
	}
	return countValue(count)
}

func (a *Auditor) merged(ctx context.Context, entry Entry) Flag {
	ref, head, reason := measurable(entry)
	if reason != "" {
		return flagUnevaluated(reason)
	}
	_, stderr, err := a.git()(ctx, entry.Path, "merge-base", "--is-ancestor", head, ref)
	if err == nil {
		return flagValue(true)
	}
	// merge-base --is-ancestor ANSWERS with its exit status: 1 means "no".
	// Any other failure is a failure, and reading it as "no" would be a fake
	// negative dressed as a fact.
	if code, ok := GitExitCode(err); ok && code == 1 {
		return flagValue(false)
	}
	return flagUnevaluated(gitFailure("merge-base --is-ancestor", stderr, err))
}

func measurable(entry Entry) (ref, head, reason string) {
	if entry.Facts.IntegrationRef.Status != gitcontext.StatusValue {
		detail := entry.Facts.IntegrationRef.Reason
		if detail == "" {
			detail = integrationRefRemedy
		}
		return "", "", "no integration ref: " + detail
	}
	if entry.Head.Status != gitcontext.StatusValue {
		return "", "", "HEAD unresolved"
	}
	return entry.Facts.IntegrationRef.Value, entry.Head.Value, ""
}

// infer is the fallback for a checkout with no registered binding: this
// project's own naming conventions, read off the branch name and off the
// `AIRA-<n>:` prefixes on the branch's unique commits.
//
// An inferred association is NEVER promoted to explicit, and an inferred ID
// that names no ticket in this project is REJECTED rather than reported — a
// worktree with no ticket must report none, not a forced guess.
func (a *Auditor) infer(ctx context.Context, entry Entry, in Inputs) ([]TicketBinding, []string) {
	type candidate struct {
		id     string
		source string
	}
	var candidates []candidate
	for _, id := range branchNameCandidates(entry.Branch.Value, in.Prefixes) {
		candidates = append(candidates, candidate{id: id, source: SourceBranchName})
	}
	if ref, head, reason := measurable(entry); reason == "" {
		out, _, err := a.git()(ctx, entry.Path, "log", "--no-merges", "--format=%s",
			"--max-count="+strconv.Itoa(maxInferredSubjects), ref+".."+head)
		if err == nil {
			for _, id := range commitPrefixCandidates(out, in.Prefixes) {
				candidates = append(candidates, candidate{id: id, source: SourceCommitPrefix})
			}
		}
	}

	seen := map[string]bool{}
	var bindings []TicketBinding
	var rejected []string
	rejectedSeen := map[string]bool{}
	for _, found := range candidates {
		if seen[found.id] {
			continue
		}
		seen[found.id] = true
		if in.TicketStatus == nil {
			continue
		}
		if _, ok := in.TicketStatus(found.id); !ok {
			if !rejectedSeen[found.id] {
				rejectedSeen[found.id] = true
				rejected = append(rejected, found.id+" ("+found.source+"): no such ticket in this project")
			}
			continue
		}
		bindings = append(bindings, TicketBinding{
			TicketID: found.id, Confidence: ConfidenceInferred, Source: found.source,
		})
	}
	return bindings, rejected
}

// branchNameCandidates matches only this project's own documented branch
// convention, anchored: `<prefix><number>` optionally followed by a separator
// and a slug. It is deliberately narrow. A looser pattern would start matching
// names like `investigate-aira91-92-…` and `review-whole-project`, and a wrong
// association is worse than none: it attaches another ticket's status to this
// checkout and can push it into the "superseded" bucket.
func branchNameCandidates(branch string, prefixes []string) []string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil
	}
	var out []string
	for _, prefix := range normalisedPrefixes(prefixes) {
		pattern := regexp.MustCompile(`^(?i:` + regexp.QuoteMeta(prefix) + `)[-_]?(\d+)(?:[-_].*)?$`)
		if match := pattern.FindStringSubmatch(branch); match != nil {
			out = append(out, prefix+"-"+strings.TrimLeft(match[1], "0")+"")
		}
	}
	return normaliseIDs(out)
}

// commitPrefixCandidates reads `AIRA-176:` and the multi-ticket `AIRA-188/189/190:`
// form this repository uses on 17 of its last 300 master commits.
func commitPrefixCandidates(subjects string, prefixes []string) []string {
	var out []string
	for _, prefix := range normalisedPrefixes(prefixes) {
		pattern := regexp.MustCompile(`^(?i:` + regexp.QuoteMeta(prefix) + `)-(\d+(?:/\d+)*)\s*:`)
		for _, subject := range strings.Split(subjects, "\n") {
			match := pattern.FindStringSubmatch(strings.TrimSpace(subject))
			if match == nil {
				continue
			}
			for _, number := range strings.Split(match[1], "/") {
				out = append(out, prefix+"-"+strings.TrimLeft(number, "0"))
			}
		}
	}
	return normaliseIDs(out)
}

func normalisedPrefixes(prefixes []string) []string {
	out := make([]string, 0, len(prefixes))
	seen := map[string]bool{}
	for _, prefix := range prefixes {
		prefix = strings.ToUpper(strings.TrimSpace(prefix))
		if prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out
}

func normaliseIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || strings.HasSuffix(id, "-") || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
