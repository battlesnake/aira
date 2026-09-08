package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realRepo builds a repository with a main checkout on `base` and one linked
// worktree on `feature` carrying a single unique commit.
func realRepo(t *testing.T) (root, featurePath string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "--quiet")
	git(t, root, "symbolic-ref", "HEAD", "refs/heads/base")
	write(t, filepath.Join(root, "README.md"), "one\n")
	git(t, root, "add", ".")
	git(t, root, "commit", "--quiet", "-m", "initial")

	featurePath = filepath.Join(base, "feature")
	git(t, root, "worktree", "add", "--quiet", "-b", "aira176-feature", featurePath)
	write(t, filepath.Join(featurePath, "feature.md"), "work\n")
	git(t, featurePath, "add", ".")
	git(t, featurePath, "commit", "--quiet", "-m", "AIRA-176: file — real work")
	return root, featurePath
}

// TestAuditAgainstARealRepositoryIgnoresAnInheritedGitDir is AIRA-93's hazard
// applied to this feature, and it is the reason every git call here goes through
// RunGit's scrubbed environment.
//
// `git -C <dir>` names the repository explicitly, but an inherited GIT_DIR
// OVERRIDES it. Without the scrub, an audit run from a shell that exported one
// would enumerate a DIFFERENT repository's worktrees and hand back removal
// recommendations for checkouts it never looked at. The decoy below is a real
// second repository, so an unscrubbed implementation fails this loudly.
func TestAuditAgainstARealRepositoryIgnoresAnInheritedGitDir(t *testing.T) {
	root, featurePath := realRepo(t)
	decoy := t.TempDir()
	git(t, decoy, "init", "--quiet")
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)

	auditor := &Auditor{Git: RunGit, Identity: fakeIdentity}
	report, err := auditor.Audit(context.Background(), Inputs{
		Root: root, BaseOverride: "base", Prefixes: []string{"AIRA"},
		TicketStatus: func(id string) (string, bool) { return "in-progress", id == "AIRA-176" },
	})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	paths := map[string]bool{}
	for _, entry := range report.Worktrees {
		paths[entry.Path] = true
	}
	if !paths[root] || !paths[featurePath] {
		t.Fatalf("worktrees=%v, want the real repository's two checkouts, not the decoy's", paths)
	}
	if paths[decoy] {
		t.Fatal("the inherited GIT_DIR steered the audit at another repository")
	}

	feature := entryFor(t, report, featurePath)
	if feature.Branch.Value != "aira176-feature" {
		t.Fatalf("branch=%+v", feature.Branch)
	}
	if !feature.Facts.UniqueCommitCount.Known() || feature.Facts.UniqueCommitCount.Value != 1 {
		t.Fatalf("unique_commit_count=%+v, want 1", feature.Facts.UniqueCommitCount)
	}
	if !feature.Facts.MergedIntoIntegration.False() {
		t.Fatalf("merged=%+v, want a proven false", feature.Facts.MergedIntoIntegration)
	}
	if !feature.Facts.PushedToAnyRemote.False() {
		t.Fatalf("pushed=%+v, want a proven false in a repository with no remotes", feature.Facts.PushedToAnyRemote)
	}
	if feature.Recommendation.Code != RecommendRecover {
		t.Fatalf("recommendation=%+v, want recover for an unpushed unique commit", feature.Recommendation)
	}
	if feature.Confidence != ConfidenceInferred || len(feature.Bindings) != 1 || feature.Bindings[0].TicketID != "AIRA-176" {
		t.Fatalf("bindings=%+v confidence=%q, want AIRA-176 inferred", feature.Bindings, feature.Confidence)
	}
	main := entryFor(t, report, root)
	if !main.Main || main.Recommendation.Code != RecommendMainWorktree {
		t.Fatalf("main entry=%+v, want the main checkout excluded from removal", main)
	}
}

// TestAuditSeesUntrackedFilesAsWorkToRecover. `git status --porcelain` lists
// untracked files (only IGNORED ones are invisible to it), and an untracked
// scratch script is exactly the "something a human cares about that git can't
// see" the design refuses to delete.
func TestAuditSeesUntrackedFilesAsWorkToRecover(t *testing.T) {
	root, featurePath := realRepo(t)
	git(t, featurePath, "reset", "--hard", "--quiet", "base")
	write(t, filepath.Join(featurePath, "repro.sh"), "#!/bin/sh\n")

	auditor := &Auditor{Git: RunGit, Identity: fakeIdentity}
	report, err := auditor.Audit(context.Background(), Inputs{Root: root, BaseOverride: "base"})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	feature := entryFor(t, report, featurePath)
	if !feature.Facts.HasUncommittedChanges.True() {
		t.Fatalf("has_uncommitted_changes=%+v, want true for an untracked file", feature.Facts.HasUncommittedChanges)
	}
	if feature.Recommendation.Code != RecommendRecover {
		t.Fatalf("recommendation=%+v, want recover", feature.Recommendation)
	}
}

// TestCaptureRegistrationRecordsNothingWhenNoIntegrationRefResolves is the
// register-side half of the "never guess" rule: with no --base, no configured
// ref and no origin/HEAD, base_ref is recorded as NONE rather than as a
// plausible-looking `master`.
func TestCaptureRegistrationRecordsNothingWhenNoIntegrationRefResolves(t *testing.T) {
	root, _ := realRepo(t)
	capture, err := CaptureRegistration(context.Background(), RunGit, root, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if capture.Branch.Value != "base" {
		t.Fatalf("branch=%+v, want the live branch", capture.Branch)
	}
	if capture.BaseRef.Value != "" {
		t.Fatalf("base_ref=%+v, want nothing recorded when nothing resolved", capture.BaseRef)
	}
	if capture.BaseCommit.Value != "" {
		t.Fatalf("base_commit=%+v, want nothing recorded without a base_ref", capture.BaseCommit)
	}
}

func TestCaptureRegistrationRecordsTheMergeBaseWhenARefResolves(t *testing.T) {
	root, featurePath := realRepo(t)
	capture, err := CaptureRegistration(context.Background(), RunGit, featurePath, "base", "")
	if err != nil {
		t.Fatal(err)
	}
	if capture.BaseRef.Value != "base" {
		t.Fatalf("base_ref=%+v", capture.BaseRef)
	}
	want := strings.TrimSpace(git(t, root, "rev-parse", "base"))
	if capture.BaseCommit.Value != want {
		t.Fatalf("base_commit=%q, want %q", capture.BaseCommit.Value, want)
	}
}

func TestCaptureRegistrationRefusesAnUnresolvableExplicitBase(t *testing.T) {
	root, _ := realRepo(t)
	if _, err := CaptureRegistration(context.Background(), RunGit, root, "", "origin/typo"); err == nil ||
		!strings.Contains(err.Error(), "E_SELECTOR_INVALID") {
		t.Fatalf("err=%v, want a refusal rather than a silently empty base_ref", err)
	}
}
