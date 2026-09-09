package runner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// verifies: AIRA-206 -- the confine status trailer begins on its OWN line, so
// an anchored (?m)^confine: parse still finds terminated-by= even when the
// job's last output block carried no trailing newline. That is the NORMAL shape
// for the kills the trailer exists to explain: a block-buffered child SIGKILLed
// by memory.oom.group leaves a partial last block. A glued trailer
// ("partial-lineconfine: slice=... terminated-by=oom") makes the anchored parse
// miss terminated-by= entirely and read an OOM-killed job as one that produced
// trustworthy results -- a spurious gate RED / false pass. The trailer is all a
// foreground caller has (--json is refused for non-management confine; a
// record.json is written only by the detach supervisor).

var confineTrailerLineRE = regexp.MustCompile(`(?m)^confine: slice=`)

// Case 1: the child writes a partial (newline-free) line to its STDERR, which
// shares the confineLockedWriter with the trailer. Reproducible through the
// buffer harness because stderr IS wrapped by that writer.
func TestConfineTrailerBeginsOnOwnLineAfterPartialStderr(t *testing.T) {
	diag := confineTrailer(t, []string{"/bin/sh", "-c", "printf partial-no-newline >&2"},
		cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}, 0)
	if !strings.Contains(diag, "partial-no-newline") {
		t.Fatalf("child's partial stderr line was not emitted: %q", diag)
	}
	if strings.Contains(diag, "partial-no-newlineconfine:") {
		t.Fatalf("trailer glued onto the child's partial last line: %q", diag)
	}
	if !confineTrailerLineRE.MatchString(diag) {
		t.Fatalf("trailer does not begin its own line (anchored ^confine: slice= failed): %q", diag)
	}
}

// Case 2 (discriminating): the child writes a partial line to its STDOUT, which
// is wired RAW -- NOT through the locked writer. With Stdout and Stderr pointing
// at the SAME *os.File, the raw stdout partial and the locked-writer trailer
// land in one fd and glue at the OS level, which a lastByteWasNewline bool on
// the locked writer CANNOT see (it never observed the stdout write). Only an
// unconditional leading \n on the trailer fixes this. A *bytes.Buffer would take
// os/exec's pipe+goroutine path and not model the shared-fd shape, so a real
// *os.File is load-bearing here (matches `aira confine -- pytest > log 2>&1`).
func TestConfineTrailerBeginsOnOwnLineWhenStdoutSharesStderrFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "confine-shared-out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create shared out file: %v", err)
	}
	defer f.Close()

	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.readUsage = func(string) cgroupUsage {
		return cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}
	}
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }

	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "printf partial-to-stdout"},
		SelfPath: os.Args[0], Stdout: f, Stderr: f,
	}, deps); err != nil {
		t.Fatalf("confine: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared out file: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "partial-to-stdout") {
		t.Fatalf("child's partial stdout was not emitted: %q", out)
	}
	if strings.Contains(out, "partial-to-stdoutconfine:") {
		t.Fatalf("trailer glued onto the child's partial stdout line: %q", out)
	}
	if !confineTrailerLineRE.MatchString(out) {
		t.Fatalf("trailer does not begin its own line: %q", out)
	}
}

// Case 3 (ci-shim twin): the shim trailer (confine_shim_linux.go) must begin its
// own line too. The ci-shim path is the owner-elevated scenario (an aitest worker
// under GCP Batch, RANT-39) where a partial last block from an oomd kill is most
// likely, and in that deployment the child's stderr can be wired to a raw fd, so
// this is not a lesser case than the real path.
func TestConfineShimTrailerBeginsOnOwnLineAfterPartialStderr(t *testing.T) {
	deps := shimUnitDeps()
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "unevaluated", reason: "slice-not-found"}, nil
	}
	var stderr bytes.Buffer
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv:     []string{"/bin/sh", "-c", "printf shim-partial-no-newline >&2"},
		SelfPath: os.Args[0], Stderr: &stderr, Stdout: io.Discard,
	}, deps); err != nil {
		t.Fatalf("shim confine: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "shim-partial-no-newline") {
		t.Fatalf("child's partial stderr was not emitted (shim): %q", out)
	}
	if strings.Contains(out, "shim-partial-no-newlineconfine:") {
		t.Fatalf("shim trailer glued onto the child's partial last line: %q", out)
	}
	if !confineTrailerLineRE.MatchString(out) {
		t.Fatalf("shim trailer does not begin its own line: %q", out)
	}
}
