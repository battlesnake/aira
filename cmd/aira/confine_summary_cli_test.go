package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// verifies: AIRA-281 -- the CLI refuses the unsound shapes synchronously, before
// any request exists: a tree hash with no file to carry it, an empty path, an
// invalid hash, and either option on the management form.
func TestParseConfineArgsSummaryFileRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"tree hash without file", []string{"--summary-tree-hash", "abc123", "--", "true"}, "--summary-tree-hash requires --summary-file"},
		{"empty summary file", []string{"--summary-file", "", "--", "true"}, "--summary-file requires a non-empty path"},
		{"empty tree hash", []string{"--summary-file", "out.jsonl", "--summary-tree-hash", "", "--", "true"}, "--summary-tree-hash"},
		{"tree hash with a space", []string{"--summary-file", "out.jsonl", "--summary-tree-hash", "a b", "--", "true"}, "--summary-tree-hash"},
		{"summary file on the management form", []string{"--summary-file", "out.jsonl", "--list"}, "E_CONFINE_ARGUMENT_INVALID"},
		{"tree hash on the management form", []string{"--summary-tree-hash", "abc", "--list"}, "E_CONFINE_ARGUMENT_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseConfineArgs(tc.argv)
			if err == nil {
				t.Fatalf("parseConfineArgs(%v) accepted an unsound shape", tc.argv)
			}
			if !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q lacks the stable code and %q", err, tc.want)
			}
		})
	}
	_, options, err := parseConfineArgs([]string{"--summary-file", "out.jsonl", "--summary-tree-hash", "sha256:ab.c_d-e+f/g=", "--", "true"})
	if err != nil {
		t.Fatalf("a valid pair was refused: %v", err)
	}
	if options["summary-file"] != "out.jsonl" || options["summary-tree-hash"] != "sha256:ab.c_d-e+f/g=" {
		t.Fatalf("options not recorded: %v", options)
	}
}

// verifies: AIRA-281 -- the CLI makes the path ABSOLUTE and transcribes both
// options onto the request, so a detached supervisor (another cwd) and the
// foreground path name the same file. Passing the raw relative path through is
// the mutation this reds on.
func TestConfineSummaryFileIsMadeAbsoluteAndReachesTheRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{}, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if exit := runWithInput([]string{"confine", "--summary-file", "rel.jsonl", "--summary-tree-hash", "deadbeef", "--", "true"},
		io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if want := filepath.Join(cwd, "rel.jsonl"); seen.SummaryFile != want {
		t.Fatalf("SummaryFile = %q, want the absolute %q", seen.SummaryFile, want)
	}
	if seen.SummaryTreeHash != "deadbeef" {
		t.Fatalf("SummaryTreeHash = %q, want deadbeef", seen.SummaryTreeHash)
	}

	// An already-absolute path is left alone, and the default is off.
	seen = runner.ConfineRequest{}
	abs := filepath.Join(t.TempDir(), "abs.jsonl")
	if exit := runWithInput([]string{"confine", "--summary-file", abs, "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if seen.SummaryFile != abs || seen.SummaryTreeHash != "" {
		t.Fatalf("request = %+v, want only the absolute SummaryFile", seen)
	}
	seen = runner.ConfineRequest{SummaryFile: "stale"}
	if exit := runWithInput([]string{"confine", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if seen.SummaryFile != "" {
		t.Fatalf("SummaryFile = %q without the option", seen.SummaryFile)
	}
}

// verifies: AIRA-281 -- the generated help names the three options this change
// touches. fail_fast is the pre-existing gap: the parser accepted --fail-fast
// while the dispatch table never named it.
func TestConfineHelpNamesSummaryAndFailFastOptions(t *testing.T) {
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name != "confine" {
			continue
		}
		names := map[string]bool{}
		for _, arg := range descriptor.Args {
			names[arg.Name] = true
		}
		for _, want := range []string{"summary_file", "summary_tree_hash", "fail_fast"} {
			if !names[want] {
				t.Errorf("confine dispatch Args lack %q", want)
			}
		}
		for _, want := range []string{"--summary-file", "--summary-tree-hash", "--fail-fast"} {
			if !strings.Contains(descriptor.Usage, want) {
				t.Errorf("confine Usage lacks %q: %s", want, descriptor.Usage)
			}
		}
		return
	}
	t.Fatal("confine descriptor missing")
}

// verifies: AIRA-281 -- a relative --summary-file resolves the way the shell's
// `>>` would: the kernel resolves `..` against the PHYSICAL cwd, so the CLI
// must join the physical cwd and must not clean `..` as text against the
// logical $PWD (filepath.Abs does, and sends the line to a different file).
// The same holds for an already-absolute path containing `..`.
func TestConfineSummaryFileResolvesDotDotPhysicallyNotTextually(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "real", "deep")
	other := filepath.Join(root, "real", "x")
	for _, dir := range []string{deep, other, filepath.Join(root, "logical")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "logical", "link")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", link) // what the shell does after `cd logical/link`

	original := runConfined
	t.Cleanup(func() { runConfined = original })
	var seen runner.ConfineRequest
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		seen = request
		return runner.ConfineResult{}, nil
	}
	resolve := func(path string) string {
		t.Helper()
		// Not filepath.Dir: it cleans `..` as text, which is the very bug under test.
		resolved, err := filepath.EvalSymlinks(path[:strings.LastIndex(path, "/")])
		if err != nil {
			t.Fatalf("%q does not resolve: %v", path, err)
		}
		return resolved
	}
	for _, option := range []string{"../x/out.jsonl", link + "/../x/out.jsonl"} {
		seen = runner.ConfineRequest{}
		if exit := runWithInput([]string{"confine", "--summary-file", option, "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
			t.Fatalf("%q: exit=%d (a path the shell can write to was refused)", option, exit)
		}
		if !filepath.IsAbs(seen.SummaryFile) {
			t.Fatalf("%q: SummaryFile %q is not absolute", option, seen.SummaryFile)
		}
		if got := resolve(seen.SummaryFile); got != other {
			t.Fatalf("%q: SummaryFile %q resolves to %s, want %s (where `>>` writes)", option, seen.SummaryFile, got, other)
		}
	}
}
