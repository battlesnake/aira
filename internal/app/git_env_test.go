package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain refuses to run this package's tests with a GIT_* variable inherited
// from the caller (AIRA-93). A test binary launched from a git hook — or from a
// shell that exported GIT_DIR — would otherwise resolve discovery against the
// HOOK's repository rather than each test's own temporary one, and every id
// derived from that (project, worktree, and the common-dir receipts and journal
// keyed by them) would be written under the wrong project. Two such receipts are
// already in this repository's shared journal, written from /tmp test working
// directories, and they are why `aira reconcile --rebuild` fails
// E_JOURNAL_CORRUPT here.
//
// It unsets rather than fails: the variables are noise for these tests, and a
// hard failure would make a hook-launched run unusable rather than correct. The
// scrub in runGitRevParse is the production guarantee; this is the guarantee
// that the tests themselves cannot be fooled.
//
// verifies: AIRA-93
func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, "GIT_") {
			_ = os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}

// TestGitDiscoveryIgnoresAnInheritedGitDir is AIRA-93's regression test: an
// inherited GIT_DIR must not redirect discovery away from the directory it was
// explicitly asked about.
//
// The two repositories are real and distinct, and the assertion is on the
// resolved COMMON DIR, because that is what keys the receipts file the bug
// corrupted — asserting only "no error" would pass against the bug.
//
// verifies: AIRA-93
func TestGitDiscoveryIgnoresAnInheritedGitDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("unevaluated: git is unavailable: %v", err)
	}
	subject := t.TempDir()
	decoy := t.TempDir()
	for _, dir := range []string{subject, decoy} {
		if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
			t.Skipf("unevaluated: cannot create a git repository: %v", err)
		}
	}

	want, err := gitValue(context.Background(), subject, "--git-common-dir")
	if err != nil {
		t.Fatalf("baseline discovery: %v", err)
	}

	// The decoy is what a hook (or a stray export) would leave in the
	// environment. Set it AFTER the baseline so the two calls differ in exactly
	// one thing.
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	got, err := gitValue(context.Background(), subject, "--git-common-dir")
	if err != nil {
		t.Fatalf("discovery with an inherited GIT_DIR: %v", err)
	}
	if got != want {
		t.Fatalf("inherited GIT_DIR redirected discovery: got %q want %q (decoy=%q)",
			strings.TrimSpace(got), strings.TrimSpace(want), decoy)
	}
}

// The scrub helper's own unit tests live with it, in internal/gitcontext, since
// every git-invoking site in the repository now shares it.
