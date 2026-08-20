package pylib

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func TestEmbeddedPyLibIncludesImportPackageAndDocumentation(t *testing.T) {
	for _, name := range []string{
		"aira_xdist_governor/__init__.py",
		"aira_xdist_governor/.gitignore",
		"aira_xdist_governor/README.md",
	} {
		if _, err := fs.Stat(embeddedPyLib, name); err != nil {
			t.Fatalf("embedded tree is missing %s: %v", name, err)
		}
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
