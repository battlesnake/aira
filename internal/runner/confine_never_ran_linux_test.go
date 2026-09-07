//go:build linux

package runner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// verifies: AIRA-147 — Confine EMITS the never-ran envelope on the caller's own
// stderr whenever it returns an error. Exercised through the exported funnel
// (not confineWithDeps), because the funnel is where the emit lives and where
// every face — the CLI, the detached supervisor, the ci-shim path — passes.
//
// Empty argv is the one refusal that needs no cgroup, no daemon and no slice, so
// this states a fact about the funnel rather than about the host it runs on.
func TestConfineEmitsNeverRanEnvelopeOnError(t *testing.T) {
	var stderr bytes.Buffer
	_, err := Confine(context.Background(), ConfineRequest{Stderr: &stderr})
	if err == nil {
		t.Fatal("Confine with empty argv returned no error")
	}
	line := strings.TrimSpace(stderr.String())
	if !strings.HasPrefix(line, "confine: "+ConfineNeverRanFacet+" ") {
		t.Fatalf("Confine emitted no never-ran envelope; stderr = %q (err %v)", stderr.String(), err)
	}
	// The envelope must carry the REAL code, not a placeholder: a caller that
	// cannot tell "the box was full" from "the request was wrong" is back to
	// string-matching the prose this line exists to replace.
	if !strings.Contains(line, " code=E_CONFINE_ARGUMENT_INVALID ") {
		t.Fatalf("never-ran envelope lost the error code: %q", line)
	}
	if !strings.Contains(line, " slice=") || !strings.Contains(line, " admission=") {
		t.Fatalf("never-ran envelope is missing a facet: %q", line)
	}
}

// verifies: AIRA-147 — a confine that DID run its target emits no never-ran
// envelope. Without this, an emit accidentally moved outside the `err != nil`
// guard would stamp `ran=no` on every successful job, which is the same lie in
// the opposite direction.
func TestRealCgroupConfineSuccessEmitsNoNeverRanEnvelope(t *testing.T) {
	parent := confineMemoryParent(t, "67108864")
	var stderr bytes.Buffer
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1, Argv: []string{"/bin/true"},
		SelfPath: os.Args[0], Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if result.Exit != 0 {
		t.Fatalf("exit = %d, want 0", result.Exit)
	}
	// The ordinary trailer must still be the only thing this job's stderr
	// carries about its outcome.
	if !strings.Contains(stderr.String(), "terminated-by=") {
		t.Fatalf("successful confine emitted no ran trailer: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), ConfineNeverRanFacet) {
		t.Fatalf("successful confine emitted the never-ran facet: %q", stderr.String())
	}
}
