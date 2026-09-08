package worktree

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

// scriptedGit records every invocation and answers from a scenario table. The
// recording half is load-bearing, not convenience: several tests below assert
// what the audit NEVER asks git, which no amount of output inspection can prove.
type scriptedGit struct {
	calls   []string
	respond func(dir string, args []string) (string, string, error)
}

func (s *scriptedGit) call(_ context.Context, dir string, args ...string) (string, string, error) {
	s.calls = append(s.calls, dir+" :: "+strings.Join(args, " "))
	return s.respond(dir, args)
}

func (s *scriptedGit) asked(fragment string) bool {
	for _, call := range s.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

// checkoutScript is one worktree's answers.
type checkoutScript struct {
	head       string
	branch     string
	detached   bool
	bare       bool
	gitDir     string
	commonDir  string
	status     string
	remoteRefs string
	// uniqueCount is returned by rev-list --count.
	uniqueCount string
	// mergedExit is the exit status merge-base --is-ancestor answers with.
	mergedExit int
	subjects   string
	// failures forces a hard error for any call whose joined args contain the key.
	failures map[string]error
}

type repoScript struct {
	root       string
	commonDir  string
	order      []string
	checkouts  map[string]*checkoutScript
	originHead string
	// resolvableRefs are the refs rev-parse --verify accepts.
	resolvableRefs map[string]bool
	listErr        error
}

func (r *repoScript) git() *scriptedGit {
	fake := &scriptedGit{}
	fake.respond = func(dir string, args []string) (string, string, error) {
		joined := strings.Join(args, " ")
		if script, ok := r.checkouts[dir]; ok && script.failures != nil {
			for fragment, err := range script.failures {
				if strings.Contains(joined, fragment) {
					return "", "boom", err
				}
			}
		}
		switch {
		case joined == "worktree list --porcelain":
			if r.listErr != nil {
				return "", "not a git repository", r.listErr
			}
			return r.porcelain(), "", nil
		case joined == "rev-parse --git-dir --git-common-dir":
			script := r.checkouts[dir]
			if script == nil {
				return "", "no such worktree", errors.New("exit 128")
			}
			return script.gitDir + "\n" + script.commonDir + "\n", "", nil
		case strings.HasPrefix(joined, "rev-parse --verify --quiet "):
			ref := strings.TrimSuffix(strings.TrimPrefix(joined, "rev-parse --verify --quiet "), "^{commit}")
			if r.resolvableRefs[ref] {
				return "deadbeef\n", "", nil
			}
			return "", "", &ExitError{Code: 1}
		case joined == "symbolic-ref --quiet refs/remotes/origin/HEAD":
			if r.originHead == "" {
				return "", "", &ExitError{Code: 1}
			}
			return r.originHead + "\n", "", nil
		case joined == "status --porcelain":
			return r.checkouts[dir].status, "", nil
		case strings.HasPrefix(joined, "for-each-ref"):
			return r.checkouts[dir].remoteRefs, "", nil
		case strings.HasPrefix(joined, "rev-list --count"):
			return r.checkouts[dir].uniqueCount + "\n", "", nil
		case strings.HasPrefix(joined, "merge-base --is-ancestor"):
			if code := r.checkouts[dir].mergedExit; code != 0 {
				return "", "", &ExitError{Code: code}
			}
			return "", "", nil
		case strings.HasPrefix(joined, "log "):
			return r.checkouts[dir].subjects, "", nil
		case joined == "branch --show-current":
			return r.checkouts[dir].branch + "\n", "", nil
		case strings.HasPrefix(joined, "merge-base "):
			return "basecommit\n", "", nil
		}
		return "", "unexpected call " + joined, errors.New("unexpected git call: " + joined)
	}
	return fake
}

func (r *repoScript) porcelain() string {
	var out strings.Builder
	for _, path := range r.order {
		script := r.checkouts[path]
		out.WriteString("worktree " + path + "\n")
		if script.bare {
			out.WriteString("bare\n\n")
			continue
		}
		out.WriteString("HEAD " + script.head + "\n")
		if script.detached {
			out.WriteString("detached\n\n")
			continue
		}
		out.WriteString("branch refs/heads/" + script.branch + "\n\n")
	}
	return out.String()
}

func fakeIdentity(commonDir, gitDir string) (string, string, error) {
	return "project:" + commonDir, "worktree:" + gitDir, nil
}

// oneFeatureRepo is the shape almost every case below starts from: a main
// checkout on master plus one linked feature worktree.
func oneFeatureRepo() *repoScript {
	return &repoScript{
		root:      "/repo",
		commonDir: "/repo/.git",
		order:     []string{"/repo", "/wt/feature"},
		checkouts: map[string]*checkoutScript{
			"/repo": {
				head: "aaaa", branch: "master", gitDir: "/repo/.git", commonDir: "/repo/.git",
				uniqueCount: "0", mergedExit: 0,
			},
			"/wt/feature": {
				head: "bbbb", branch: "aira176-worktree-ticket-association",
				gitDir: "/repo/.git/worktrees/feature", commonDir: "/repo/.git",
				uniqueCount: "0", mergedExit: 0,
			},
		},
		originHead:     "refs/remotes/origin/master",
		resolvableRefs: map[string]bool{"refs/remotes/origin/master": true, "origin/master": true, "origin/main": true},
	}
}

func runAudit(t *testing.T, repo *repoScript, in Inputs) (Report, *scriptedGit) {
	t.Helper()
	fake := repo.git()
	auditor := &Auditor{Git: fake.call, Identity: fakeIdentity}
	if in.Root == "" {
		in.Root = repo.root
	}
	if in.TicketStatus == nil {
		in.TicketStatus = func(string) (string, bool) { return "", false }
	}
	report, err := auditor.Audit(context.Background(), in)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	return report, fake
}

func entryFor(t *testing.T, report Report, path string) Entry {
	t.Helper()
	for _, entry := range report.Worktrees {
		if entry.Path == path {
			return entry
		}
	}
	t.Fatalf("no entry for %s in %+v", path, report.Worktrees)
	return Entry{}
}

// --- recommendation buckets, both directions ------------------------------

// TestMergedCleanWorktreeIsSafeToRemove is the false-FAIL direction of the
// merged bucket: the one shape that should get the recommendation must get it,
// or the whole feature is inert.
func TestMergedCleanWorktreeIsSafeToRemove(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code != RecommendMerged {
		t.Fatalf("recommendation=%+v, want merged", entry.Recommendation)
	}
	if len(entry.Recommendation.Because) == 0 {
		t.Fatal("a recommendation with no stated basis is an opaque verdict")
	}
}

// TestUncommittedChangesBeatMerged is the sharpest false-PASS guard in the
// suite. A merged branch with a dirty tree is EXACTLY the case where "merged —
// safe to remove" destroys work that git has no copy of.
func TestUncommittedChangesBeatMerged(t *testing.T) {
	repo := oneFeatureRepo()
	repo.checkouts["/wt/feature"].status = " M internal/store/store.go\n?? scratch-notes.md\n"
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code != RecommendRecover {
		t.Fatalf("recommendation=%+v, want recover", entry.Recommendation)
	}
}

// TestUnpushedUniqueCommitsRecommendRecovery covers the second recovery shape:
// committed work no remote has a copy of.
func TestUnpushedUniqueCommitsRecommendRecovery(t *testing.T) {
	repo := oneFeatureRepo()
	feature := repo.checkouts["/wt/feature"]
	feature.uniqueCount, feature.mergedExit, feature.remoteRefs = "4", 1, ""
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code != RecommendRecover {
		t.Fatalf("recommendation=%+v, want recover", entry.Recommendation)
	}
	if !strings.Contains(strings.Join(entry.Recommendation.Because, " "), "4 commit(s)") {
		t.Fatalf("recovery reason does not name the commit count: %v", entry.Recommendation.Because)
	}
}

// TestPushedUniqueCommitsOnDoneTicketLookSuperseded is the false-fail direction
// of the superseded bucket.
func TestPushedUniqueCommitsOnDoneTicketLookSuperseded(t *testing.T) {
	repo := oneFeatureRepo()
	feature := repo.checkouts["/wt/feature"]
	feature.uniqueCount, feature.mergedExit = "3", 1
	feature.remoteRefs = "refs/remotes/origin/aira176-worktree-ticket-association\n"
	report, _ := runAudit(t, repo, Inputs{
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-176", RegisteredAt: "t",
		}},
		TicketStatus: func(string) (string, bool) { return "done", true },
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code != RecommendSuperseded {
		t.Fatalf("recommendation=%+v, want superseded", entry.Recommendation)
	}
}

// TestOpenTicketIsNotSuperseded is its false-PASS twin: identical git facts, an
// OPEN ticket, and the bucket must not fire. Without this, "superseded" would
// collapse into "any pushed branch with commits", which is most of them.
func TestOpenTicketIsNotSuperseded(t *testing.T) {
	repo := oneFeatureRepo()
	feature := repo.checkouts["/wt/feature"]
	feature.uniqueCount, feature.mergedExit = "3", 1
	feature.remoteRefs = "refs/remotes/origin/aira176-worktree-ticket-association\n"
	report, _ := runAudit(t, repo, Inputs{
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-176", RegisteredAt: "t",
		}},
		TicketStatus: func(string) (string, bool) { return "in-progress", true },
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code != RecommendNone {
		t.Fatalf("recommendation=%+v, want none for an open ticket", entry.Recommendation)
	}
}

// TestLiveLeaseOverridesEveryRemovalBucket: a merged, spotless checkout someone
// is holding a lease on must still be left alone.
func TestLiveLeaseOverridesEveryRemovalBucket(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-176", RegisteredAt: "t",
		}},
		Leases:       []Lease{{TicketID: "AIRA-176", WorktreeID: "worktree:/repo/.git/worktrees/feature", Actor: "opus"}},
		TicketStatus: func(string) (string, bool) { return "in-progress", true },
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code != RecommendActive {
		t.Fatalf("recommendation=%+v, want active", entry.Recommendation)
	}
	if !strings.Contains(strings.Join(entry.Recommendation.Because, " "), "opus") {
		t.Fatalf("active recommendation does not name the holder: %v", entry.Recommendation.Because)
	}
}

// TestUnevaluatedStatusSuppressesEveryRemovalRecommendation is the fail-closed
// rule stated as a test: a check that cannot establish its result must not
// produce a pass. A broken `git status` previously would have left
// has_uncommitted_changes falsy and let "merged — safe to remove" through.
func TestUnevaluatedStatusSuppressesEveryRemovalRecommendation(t *testing.T) {
	repo := oneFeatureRepo()
	repo.checkouts["/wt/feature"].failures = map[string]error{"status --porcelain": errors.New("exit 128")}
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.HasUncommittedChanges.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("has_uncommitted_changes=%+v, want unevaluated", entry.Facts.HasUncommittedChanges)
	}
	if entry.Recommendation.Code != RecommendNone {
		t.Fatalf("recommendation=%+v, want none when uncommitted state is unevaluated", entry.Recommendation)
	}
	if !strings.Contains(strings.Join(entry.Recommendation.Because, " "), "has_uncommitted_changes was not established false") {
		t.Fatalf("suppression reason not stated: %v", entry.Recommendation.Because)
	}
}

// TestMergeBaseFailureIsUnevaluatedNotNotMerged pins the exit-code split.
// `merge-base --is-ancestor` answers with its status, so collapsing "exit 2,
// something broke" into "exit non-zero, so not merged" would report a fact that
// was never established.
func TestMergeBaseFailureIsUnevaluatedNotNotMerged(t *testing.T) {
	repo := oneFeatureRepo()
	repo.checkouts["/wt/feature"].mergedExit = 128
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.MergedIntoIntegration.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("merged=%+v, want unevaluated", entry.Facts.MergedIntoIntegration)
	}
	if entry.Facts.MergedIntoIntegration.False() {
		t.Fatal("a broken merge-base must not read as a proven negative")
	}
}

// TestMainWorktreeIsNeverARemovalCandidate. The main checkout on master is
// clean and trivially merged, so every fact-based bucket would call it "nothing
// to lose — safe to remove" — advice that destroys the repository.
func TestMainWorktreeIsNeverARemovalCandidate(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/repo")
	if !entry.Main {
		t.Fatal("the checkout whose git dir is the common dir must be flagged main")
	}
	if entry.Recommendation.Code != RecommendMainWorktree {
		t.Fatalf("recommendation=%+v, want main-worktree", entry.Recommendation)
	}
}

// --- the integration-ref chain --------------------------------------------

// TestUpstreamIsNeverConsulted is the 77-self-pointing-branches trap, asserted
// on what the audit ASKS git rather than on what it concludes. With
// branch.<name>.merge pointing at the branch itself — the configuration on most
// local branches in this repository — any use of @{upstream} yields zero unique
// commits and is-ancestor true, i.e. a blanket false "merged — safe to remove".
func TestUpstreamIsNeverConsulted(t *testing.T) {
	repo := oneFeatureRepo()
	repo.originHead = ""
	repo.resolvableRefs = map[string]bool{}
	report, fake := runAudit(t, repo, Inputs{})
	for _, banned := range []string{"@{upstream}", "branch.", "--symbolic-full-name", "rev-parse --abbrev-ref"} {
		if fake.asked(banned) {
			t.Fatalf("the audit consulted %q; git calls were %v", banned, fake.calls)
		}
	}
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.IntegrationRef.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("integration_ref=%+v, want unevaluated with no origin/HEAD and no config", entry.Facts.IntegrationRef)
	}
	if entry.Recommendation.Code == RecommendMerged || entry.Recommendation.Code == RecommendNothingToLose {
		t.Fatalf("recommendation=%+v: an unresolvable integration ref must not produce a removal verdict", entry.Recommendation)
	}
}

// TestUnresolvedIntegrationRefStillRecoversUnpushedWork is §5.3's deliberate
// asymmetry: the removal buckets go silent, but the bucket whose being wrong
// loses work stays evaluable, because remote containment needs no integration
// ref at all.
func TestUnresolvedIntegrationRefStillRecoversUnpushedWork(t *testing.T) {
	repo := oneFeatureRepo()
	repo.originHead = ""
	repo.resolvableRefs = map[string]bool{}
	repo.checkouts["/wt/feature"].status = " M main.go\n"
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code != RecommendRecover {
		t.Fatalf("recommendation=%+v, want recover even with no integration ref", entry.Recommendation)
	}
}

func TestIntegrationRefChainPrefersExplicitBaseOverEverything(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		BaseOverride: "origin/main", ConfigRef: "origin/master",
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-1", BaseRef: "origin/master", RegisteredAt: "t",
		}},
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.IntegrationRef.Value != "origin/main" {
		t.Fatalf("integration_ref=%+v, want the --base override", entry.Facts.IntegrationRef)
	}
}

func TestIntegrationRefChainPrefersBindingOverConfig(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		ConfigRef: "origin/master",
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-1", BaseRef: "origin/main", RegisteredAt: "t",
		}},
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.IntegrationRef.Value != "origin/main" {
		t.Fatalf("integration_ref=%+v, want the recorded binding base_ref", entry.Facts.IntegrationRef)
	}
}

// TestConflictingBindingBaseRefsRefuseRatherThanFallThrough is F5 of the plan
// review. With several tickets bound to one checkout the "the base_ref recorded
// on that worktree's binding row" phrasing has no single referent, and there is
// no basis for preferring either — so the chain REFUSES rather than silently
// answering from a source none of the bindings named.
func TestConflictingBindingBaseRefsRefuseRatherThanFallThrough(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		ConfigRef: "origin/master",
		Bindings: []domain.WorktreeBinding{
			{WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-1", BaseRef: "origin/master", RegisteredAt: "t"},
			{WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-2", BaseRef: "origin/main", RegisteredAt: "t"},
		},
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.IntegrationRef.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("integration_ref=%+v, want unevaluated on a conflict", entry.Facts.IntegrationRef)
	}
	if !strings.Contains(entry.Facts.IntegrationRef.Reason, "binding base_ref conflict") {
		t.Fatalf("reason=%q, want it to name the conflict", entry.Facts.IntegrationRef.Reason)
	}
	if entry.Facts.IntegrationRef.Value == "origin/master" {
		t.Fatal("a conflict must not fall through to the configured ref")
	}
}

// TestAgreeingBindingBaseRefsAreUsed is the conflict rule's false-fail twin.
func TestAgreeingBindingBaseRefsAreUsed(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		Bindings: []domain.WorktreeBinding{
			{WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-1", BaseRef: "origin/main", RegisteredAt: "t"},
			{WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-2", BaseRef: "origin/main", RegisteredAt: "t"},
		},
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.IntegrationRef.Value != "origin/main" {
		t.Fatalf("integration_ref=%+v, want origin/main", entry.Facts.IntegrationRef)
	}
}

// TestBrokenConfiguredRefDoesNotFallThroughToOriginHead: a configured value is
// a positively established CONFIGURATION. Papering over it with origin/HEAD
// would answer from a source the operator did not choose and hide the typo.
func TestBrokenConfiguredRefDoesNotFallThroughToOriginHead(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{ConfigRef: "origin/nonexistent"})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.IntegrationRef.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("integration_ref=%+v, want unevaluated", entry.Facts.IntegrationRef)
	}
	if entry.Facts.IntegrationRef.Value == "refs/remotes/origin/master" {
		t.Fatal("a broken git.integration_ref silently fell through to origin/HEAD")
	}
}

func TestUnresolvableExplicitBaseIsRefused(t *testing.T) {
	repo := oneFeatureRepo()
	fake := repo.git()
	auditor := &Auditor{Git: fake.call, Identity: fakeIdentity}
	_, err := auditor.Audit(context.Background(), Inputs{Root: repo.root, BaseOverride: "origin/typo"})
	if err == nil || !strings.Contains(err.Error(), "E_SELECTOR_INVALID") {
		t.Fatalf("err=%v, want E_SELECTOR_INVALID for an unresolvable --base", err)
	}
}

// --- inference -------------------------------------------------------------

func TestBranchNameInferenceIsLabelledInferred(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		Prefixes:     []string{"AIRA"},
		TicketStatus: func(id string) (string, bool) { return "planned", id == "AIRA-176" },
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Confidence != ConfidenceInferred {
		t.Fatalf("confidence=%q, want inferred", entry.Confidence)
	}
	if len(entry.Bindings) != 1 || entry.Bindings[0].TicketID != "AIRA-176" {
		t.Fatalf("bindings=%+v, want AIRA-176", entry.Bindings)
	}
	if entry.Bindings[0].Confidence != ConfidenceInferred || entry.Bindings[0].Source != SourceBranchName {
		t.Fatalf("binding=%+v, want inferred/branch-name", entry.Bindings[0])
	}
}

// TestExplicitBindingIsNeverDowngradedAndSuppressesInference: an explicit
// registration must win, and must not be reported next to a guess as if the two
// were the same grade of evidence.
func TestExplicitBindingIsNeverDowngradedAndSuppressesInference(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		Prefixes: []string{"AIRA"},
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-999",
			Owner: "session-a", OwnerAttested: true, RegisteredAt: "t",
		}},
		TicketStatus: func(string) (string, bool) { return "planned", true },
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Confidence != ConfidenceExplicit {
		t.Fatalf("confidence=%q, want explicit", entry.Confidence)
	}
	if len(entry.Bindings) != 1 || entry.Bindings[0].TicketID != "AIRA-999" {
		t.Fatalf("bindings=%+v, want only the registered binding", entry.Bindings)
	}
	if !entry.Bindings[0].OwnerAttested || entry.Bindings[0].Owner != "session-a" {
		t.Fatalf("binding=%+v lost its owner attestation", entry.Bindings[0])
	}
}

// TestMultiTicketCommitPrefixesAreAllInferred covers the `AIRA-188/189/190:`
// form this repository uses on 17 of its last 300 master commits.
func TestMultiTicketCommitPrefixesAreAllInferred(t *testing.T) {
	repo := oneFeatureRepo()
	feature := repo.checkouts["/wt/feature"]
	feature.branch = "scratch"
	feature.uniqueCount, feature.mergedExit = "2", 1
	feature.subjects = "AIRA-188/189/190: file — three at once\nAIRA-171/172: file — two\n"
	known := map[string]bool{"AIRA-188": true, "AIRA-189": true, "AIRA-190": true, "AIRA-171": true, "AIRA-172": true}
	report, _ := runAudit(t, repo, Inputs{
		Prefixes:     []string{"AIRA"},
		TicketStatus: func(id string) (string, bool) { return "done", known[id] },
	})
	entry := entryFor(t, report, "/wt/feature")
	got := map[string]bool{}
	for _, binding := range entry.Bindings {
		got[binding.TicketID] = true
		if binding.Confidence != ConfidenceInferred || binding.Source != SourceCommitPrefix {
			t.Fatalf("binding=%+v, want inferred/commit-prefix", binding)
		}
	}
	for id := range known {
		if !got[id] {
			t.Fatalf("bindings=%+v, missing %s", entry.Bindings, id)
		}
	}
}

// TestInferenceNeverInventsATicket: a branch matching the convention but naming
// a ticket this project does not have reports NO binding, and says so.
func TestInferenceNeverInventsATicket(t *testing.T) {
	repo := oneFeatureRepo()
	repo.checkouts["/wt/feature"].branch = "aira99999-ghost"
	report, _ := runAudit(t, repo, Inputs{
		Prefixes:     []string{"AIRA"},
		TicketStatus: func(string) (string, bool) { return "", false },
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Confidence != ConfidenceNone || len(entry.Bindings) != 0 {
		t.Fatalf("entry=%+v, want no binding at all", entry)
	}
	if len(entry.InferenceRejected) != 1 || !strings.Contains(entry.InferenceRejected[0], "AIRA-99999") {
		t.Fatalf("inference_rejected=%v, want the rejected guess named", entry.InferenceRejected)
	}
}

// TestUnconventionalBranchNamesInferNothing pins the deliberately narrow
// pattern. These four are real branch names in this repository, and matching
// any of them would attach another ticket's status to the wrong checkout.
func TestUnconventionalBranchNamesInferNothing(t *testing.T) {
	for _, branch := range []string{"master", "review-whole-project", "investigate-aira91-92-aitest-contention", "worktree-wf_abc123"} {
		if got := branchNameCandidates(branch, []string{"AIRA"}); len(got) != 0 {
			t.Errorf("branch %q inferred %v, want nothing", branch, got)
		}
	}
	if got := branchNameCandidates("aira176-worktree-ticket-association", []string{"AIRA"}); len(got) != 1 || got[0] != "AIRA-176" {
		t.Errorf("conventional branch inferred %v, want [AIRA-176]", got)
	}
}

// --- bindings whose checkout is gone ---------------------------------------

// TestOrphanBindingIsReportedNotSwept. AIRA never deletes a binding, so a
// checkout that has been removed leaves a row. It is reported, with the last
// path AIRA saw it at, rather than quietly disappearing.
func TestOrphanBindingIsReportedNotSwept(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/gone/.git", TicketID: "AIRA-140", Branch: "aira140-x", RegisteredAt: "t",
		}},
		KnownRoots: map[string]string{"worktree:/gone/.git": "/wt/gone"},
	})
	if len(report.OrphanBindings) != 1 {
		t.Fatalf("orphans=%+v, want one", report.OrphanBindings)
	}
	if report.OrphanBindings[0].LastKnownRoot != "/wt/gone" {
		t.Fatalf("orphan=%+v, want the last known root named", report.OrphanBindings[0])
	}
}

// --- identity, selectors, budget -------------------------------------------

// TestUnresolvableWorktreeIdentitySuppressesRemoval: without an identity the
// audit cannot know whether a lease names this checkout, so it must not
// recommend removing it. This is the F3 join failing SAFE.
func TestUnresolvableWorktreeIdentitySuppressesRemoval(t *testing.T) {
	repo := oneFeatureRepo()
	repo.checkouts["/wt/feature"].failures = map[string]error{"rev-parse --git-dir": errors.New("exit 128")}
	report, _ := runAudit(t, repo, Inputs{})
	entry := entryFor(t, report, "/wt/feature")
	if entry.WorktreeID.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("worktree_id=%+v, want unevaluated", entry.WorktreeID)
	}
	if entry.Facts.LiveLease.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("live_lease=%+v, want unevaluated when identity is unknown", entry.Facts.LiveLease)
	}
	if entry.Recommendation.Code != RecommendNone {
		t.Fatalf("recommendation=%+v, want none", entry.Recommendation)
	}
}

func TestTicketSelectorScopesToThatTicketsCheckouts(t *testing.T) {
	repo := oneFeatureRepo()
	report, _ := runAudit(t, repo, Inputs{
		Selector: Selector{TicketID: "AIRA-176"},
		Bindings: []domain.WorktreeBinding{{
			WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-176", RegisteredAt: "t",
		}},
		TicketStatus: func(string) (string, bool) { return "planned", true },
	})
	if len(report.Worktrees) != 1 || report.Worktrees[0].Path != "/wt/feature" {
		t.Fatalf("worktrees=%+v, want only the bound checkout", report.Worktrees)
	}
}

func TestPathSelectorNamingNoCheckoutIsRefused(t *testing.T) {
	repo := oneFeatureRepo()
	fake := repo.git()
	auditor := &Auditor{Git: fake.call, Identity: fakeIdentity}
	_, err := auditor.Audit(context.Background(), Inputs{Root: repo.root, Selector: Selector{Path: "/not/a/worktree"}})
	if err == nil || !strings.Contains(err.Error(), "E_SELECTOR_INVALID") {
		t.Fatalf("err=%v, want E_SELECTOR_INVALID", err)
	}
}

// TestElapsedBudgetReportsUnevaluatedRatherThanAPartialTruth. With ~85
// checkouts and a stuck git grandchild the sweep must end; what it must NOT do
// is present the checkouts it never looked at as if it had.
func TestElapsedBudgetReportsUnevaluatedRatherThanAPartialTruth(t *testing.T) {
	repo := oneFeatureRepo()
	fake := repo.git()
	auditor := &Auditor{Git: fake.call, Identity: fakeIdentity, Budget: time.Nanosecond,
		Now: func() time.Time { return time.Now() }}
	report, err := auditor.Audit(context.Background(), Inputs{Root: repo.root,
		TicketStatus: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	entry := entryFor(t, report, "/wt/feature")
	if entry.Facts.HasUncommittedChanges.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("facts=%+v, want unevaluated after the budget elapsed", entry.Facts)
	}
	if entry.Recommendation.Code != RecommendNone {
		t.Fatalf("recommendation=%+v, want none", entry.Recommendation)
	}
	if !strings.Contains(strings.Join(report.Notes, " "), "time budget") {
		t.Fatalf("notes=%v, want the elapsed budget stated", report.Notes)
	}
}

func TestParseWorktreeListReadsBranchAndHeadWithoutExtraSubprocesses(t *testing.T) {
	got := parseWorktreeList("worktree /repo\nHEAD aaaa\nbranch refs/heads/master\n\n" +
		"worktree /wt/detached\nHEAD bbbb\ndetached\n\n" +
		"worktree /wt/bare\nbare\n\n")
	if len(got) != 3 {
		t.Fatalf("checkouts=%d, want 3", len(got))
	}
	if got[0].branch != "refs/heads/master" || got[0].head != "aaaa" {
		t.Fatalf("main=%+v", got[0])
	}
	if !got[1].detached || got[1].branch != "" {
		t.Fatalf("detached=%+v", got[1])
	}
	if !got[2].bare {
		t.Fatalf("bare=%+v", got[2])
	}
}

func TestGitExitCodeSeparatesRanAndFailedFromNeverRan(t *testing.T) {
	if code, ok := GitExitCode(&ExitError{Code: 1}); !ok || code != 1 {
		t.Fatalf("code=%d ok=%v, want 1/true", code, ok)
	}
	if _, ok := GitExitCode(errors.New("context deadline exceeded")); ok {
		t.Fatal("a call that never produced a status must not report one")
	}
}

// TestSupersededNeedsEveryBoundTicketEstablishedClosed. With the per-ticket key
// a checkout can carry several bindings. If one names a done ticket and another
// names a ticket AIRA cannot read at all, "looks superseded" would be carried by
// the half that was established — so the bucket demands all of them.
func TestSupersededNeedsEveryBoundTicketEstablishedClosed(t *testing.T) {
	repo := oneFeatureRepo()
	feature := repo.checkouts["/wt/feature"]
	feature.uniqueCount, feature.mergedExit = "3", 1
	feature.remoteRefs = "refs/remotes/origin/aira176-worktree-ticket-association\n"
	report, _ := runAudit(t, repo, Inputs{
		Bindings: []domain.WorktreeBinding{
			{WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-1", RegisteredAt: "t"},
			{WorktreeID: "worktree:/repo/.git/worktrees/feature", TicketID: "AIRA-2", RegisteredAt: "t"},
		},
		TicketStatus: func(id string) (string, bool) { return "done", id == "AIRA-1" },
	})
	entry := entryFor(t, report, "/wt/feature")
	if entry.Recommendation.Code == RecommendSuperseded {
		t.Fatalf("recommendation=%+v: one unreadable bound ticket must sink the bucket", entry.Recommendation)
	}
}
