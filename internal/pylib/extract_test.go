package pylib

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

// notEmbedded declares, per embedded root, the git-tracked paths that are
// deliberately NOT shipped. Everything else git tracks under the root must be
// embedded, and nothing else may be. Entries ending in "/" exclude a subtree.
//
// This is the whole hand-maintained surface of the embed contract: two source
// hygiene files and one fixture subtree, each with a stated reason. It replaces
// the two "does the tree contain these three names" tests, which asserted a
// subset (so `all:`'s untracked __pycache__/.pytest_cache scratch passed them
// happily) and asserted the presence of .gitignore and README.md, which no Go
// consumer reads and which the *.py globs correctly no longer ship.
var notEmbedded = map[string][]string{
	"aira_xdist_governor": {
		".gitignore", // source hygiene for the checkout, not a runtime input
		"README.md",  // documentation; read in the repo, not from the extraction
	},
	"aitest": {
		".gitignore",
		"README.md",
		"testdata/", // fixtures for aitest's own source-tree pytest tier
	},
}

// TestEmbeddedTreesMatchTrackedSources is the AIRA-66 prevention mechanism: the
// embedded set must equal exactly the git-tracked files under each root, minus
// the declared exclusions above. `git ls-files` is the single oracle — there is
// no second hand-maintained manifest to drift.
//
// It fails in both directions, which is the point:
//   - anything embedded that git does not track (a reverted `all:`, a stray
//     __pycache__/*.pyc or .pytest_cache entry, an untracked scratch .py the
//     glob still matches, a pattern reaching outside the root) is a surplus, and
//   - any tracked file the pattern misses (a new runtime .json, a new runtime
//     subpackage the *.py glob cannot reach) is a shortfall that forces an
//     explicit decision: extend the embed pattern, or declare the exclusion here.
//
// NAMED RESIDUAL: this test, not the embed pattern, is what makes the build
// hermetic. `*.py` still matches an UNTRACKED scratch .py sitting in the package
// directory, so `go build` alone can still bake one in; only `go test` (which
// `make ci` runs) catches it. Closing that at build time would need a generated
// tracked-file manifest — a second source of truth that drifts, which AIRA-66
// deliberately rejected. The residual is narrower than the defect it replaces:
// __pycache__/.pytest_cache appear from merely running pytest, an untracked
// scratch .py takes a deliberate act.
//
// verifies: AIRA-66
func TestEmbeddedTreesMatchTrackedSources(t *testing.T) {
	for _, tc := range []struct {
		root string
		tree fs.FS
	}{
		{embeddedRoot, embeddedPyLib},
		{embeddedAitestRoot, embeddedAitest},
	} {
		t.Run(tc.root, func(t *testing.T) {
			want := trackedFiles(t, tc.root)
			// Walk from ".", not from tc.root: a second //go:embed directive on
			// the same variable, or a pattern reaching outside the expected
			// root, embeds bytes that a root-anchored walk would never see —
			// the test would pass while the binary grew.
			got := embeddedFiles(t, tc.tree)
			for _, name := range want {
				if !slices.Contains(got, name) {
					t.Errorf("tracked but NOT embedded: %s (extend the //go:embed pattern, or declare it in notEmbedded)", name)
				}
			}
			for _, name := range got {
				if !slices.Contains(want, name) {
					t.Errorf("embedded but not a tracked, non-excluded source: %s (the embed pattern is picking up scratch)", name)
				}
			}
		})
	}
}

// trackedFiles is the oracle: git's own view of the root, minus notEmbedded.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	// A skip is `unevaluated`, not a pass — but `go test` still exits 0 overall,
	// so this test protects a git checkout (every CI and developer run here) and
	// NOT a source-archive build with no VCS. Stated rather than implied.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("unevaluated: git is unavailable, so the tracked-source oracle cannot be established: %v", err)
	}
	if err := exec.Command("git", "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skipf("unevaluated: not inside a git work tree, so the tracked-source oracle cannot be established: %v", err)
	}
	// Failures BELOW this point are real failures, never skips: git exists and
	// the package is in a work tree, so an error means the oracle is broken.
	out, err := exec.Command("git", "ls-files", "-z", "--", root).Output()
	if err != nil {
		t.Fatalf("git ls-files -- %s: %v", root, err)
	}
	var files []string
	for _, name := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		relative := strings.TrimPrefix(name, root+"/")
		if excluded(root, relative) {
			continue
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		t.Fatalf("git ls-files -- %s returned no non-excluded file; the oracle would be vacuous", root)
	}
	slices.Sort(files)
	return files
}

func excluded(root, relative string) bool {
	for _, pattern := range notEmbedded[root] {
		if strings.HasSuffix(pattern, "/") {
			if strings.HasPrefix(relative, pattern) {
				return true
			}
			continue
		}
		if relative == pattern {
			return true
		}
	}
	return false
}

// embeddedFiles walks the WHOLE embed.FS, not a chosen subtree of it, so that a
// pattern reaching outside the expected root shows up as a surplus.
func embeddedFiles(t *testing.T, tree fs.FS) []string {
	t.Helper()
	var files []string
	if err := fs.WalkDir(tree, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			files = append(files, name)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk embedded tree: %v", err)
	}
	slices.Sort(files)
	return files
}

func TestExtractAitestIsIdempotent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	first, err := ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	ready, err := os.ReadFile(filepath.Join(first, readyName))
	if err != nil || strings.TrimSpace(string(ready)) != filepath.Base(first) {
		t.Fatalf("ready=%q err=%v target=%s", ready, err, first)
	}

	marker := filepath.Join(first, "caller-marker")
	if err := os.WriteFile(marker, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second extraction path=%q want %q", second, first)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserved" {
		t.Fatalf("fast path rewrote published tree: got=%q err=%v", got, err)
	}
}

func TestExtractPyLibIsIdempotentAndReadyLast(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	first, err := ExtractPyLib()
	if err != nil {
		t.Fatal(err)
	}
	ready, err := os.ReadFile(filepath.Join(first, readyName))
	if err != nil || strings.TrimSpace(string(ready)) != filepath.Base(first) {
		t.Fatalf("ready=%q err=%v target=%s", ready, err, first)
	}

	marker := filepath.Join(first, "caller-marker")
	if err := os.WriteFile(marker, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := ExtractPyLib()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second extraction path=%q want %q", second, first)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserved" {
		t.Fatalf("fast path rewrote published tree: got=%q err=%v", got, err)
	}
}

func TestExtractPyLibContentHashChangesWithTree(t *testing.T) {
	dataHome := t.TempDir()
	firstFS := fstest.MapFS{"pkg/__init__.py": {Data: []byte("VALUE = 1\n")}}
	secondFS := fstest.MapFS{"pkg/__init__.py": {Data: []byte("VALUE = 2\n")}}
	first, err := extractPyLibFS(firstFS, "pkg", dataHome)
	if err != nil {
		t.Fatal(err)
	}
	second, err := extractPyLibFS(secondFS, "pkg", dataHome)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("different embedded content reused %s", first)
	}
	for _, dir := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(dir, readyName)); err != nil {
			t.Fatalf("published hash dir %s is incomplete: %v", dir, err)
		}
	}
}

func TestExtractPyLibConcurrentPublishHasOneCompleteTreeAndNoLoserTemp(t *testing.T) {
	dataHome := t.TempDir()
	source := fstest.MapFS{
		"pkg/__init__.py": {Data: []byte("VALUE = 1\n")},
		"pkg/nested.py":   {Data: []byte("NESTED = True\n")},
	}
	type result struct {
		dir string
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var ready sync.WaitGroup
	for range 2 {
		ready.Add(1)
		go func() {
			ready.Done()
			<-start
			dir, err := extractPyLibFS(source, "pkg", dataHome)
			results <- result{dir: dir, err: err}
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.dir != second.dir {
		t.Fatalf("concurrent extraction results: %#v %#v", first, second)
	}
	if _, err := os.Stat(filepath.Join(first.dir, readyName)); err != nil {
		t.Fatalf("published tree is incomplete: %v", err)
	}
	root := filepath.Dir(first.dir)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tempPrefix) {
			t.Fatalf("loser temp was not cleaned: %s", entry.Name())
		}
	}
}

func TestExtractPyLibFailureNeverPublishesTarget(t *testing.T) {
	dataHome := t.TempDir()
	source := failingFS{
		FS: fstest.MapFS{
			"pkg/__init__.py": {Data: []byte("VALUE = 1\n")},
			"pkg/bad.py":      {Data: []byte("unreadable")},
		},
		failName: "pkg/bad.py",
	}
	if _, err := extractPyLibFS(source, "pkg", dataHome); err == nil {
		t.Fatal("extraction unexpectedly succeeded")
	}
	root := filepath.Join(dataHome, "aira", "pylib")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), tempPrefix) {
			t.Fatalf("failed extraction published target %s", entry.Name())
		}
	}
}

type failingFS struct {
	fs.FS
	failName string
}

func (f failingFS) Open(name string) (fs.File, error) {
	if name == f.failName {
		return nil, errors.New("injected read failure")
	}
	return f.FS.Open(name)
}
