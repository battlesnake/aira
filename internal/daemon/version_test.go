package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"aira/internal/buildid"
	"aira/internal/core"
)

// TestVersionIsAnsweredBeforeAnyScopeWork closes the coverage gap the AIRA-202
// build review found: the daemon's version arm — the load-bearing half of the
// ticket — had no test at all, while its immediate neighbour confine-report is
// driven exactly this way.
//
// The EMPTY scope is the whole point of the test. `aira version` resolves no
// project, so the client sends WorktreeScope{}. If the arm were deleted, or
// merely moved below the scope work, the request would fall through to
// coreForRequest -> coreForScope -> storeForScope, where an empty ProjectSlug
// refuses with `E_CONFIG_INVALID: scope options are incomplete`
// (internal/store/store.go:580).
//
// That is not a harmless regression, which is why this is worth a test rather
// than a comment: cmd/aira/version.go reads E_CONFIG_INVALID on THIS request as
// evidence that the daemon predates the verb, and prints "the running daemon
// does not implement the version verb, which itself indicates it is OLDER than
// this client". So moving this arm turns a current daemon into a confident,
// fabricated staleness accusation — precisely the class of output CLAUDE.md
// forbids, and the build review confirmed no test in the tree detected it.
//
// verifies: AIRA-202
func TestVersionIsAnsweredBeforeAnyScopeWork(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)

	frame, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{
		Proto:   ProtocolVersion,
		Scope:   WorktreeScope{},
		Request: core.Request{Verb: "version"},
	})
	if err != nil {
		t.Fatalf("version exchange: %v", err)
	}
	if !frame.OK || frame.Code != "OK" {
		t.Fatalf("version refused with an EMPTY scope: %+v — the arm no longer precedes scope resolution, "+
			"and the CLI reads that refusal as proof the daemon is stale", frame)
	}

	var identity buildid.Identity
	if err := json.Unmarshal(frame.Data, &identity); err != nil {
		t.Fatalf("decode identity from %s: %v", string(frame.Data), err)
	}
	// The daemon under test is this test binary, built in a linked worktree, so
	// the identity is genuinely unestablished. Assert the CONTRACT rather than a
	// literal revision: established or not, it must never be a silent zero value.
	if identity.Established {
		if identity.Revision == "" {
			t.Fatal("established identity carries no revision")
		}
	} else if identity.Reason == "" {
		t.Fatal("an unestablished daemon identity must carry its reason, not an empty struct")
	}
}
