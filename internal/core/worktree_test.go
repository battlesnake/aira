package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/worktree"
)

var worktreeSelectorZero worktree.Selector

func mkdir(path string) error { return os.MkdirAll(path, 0o755) }

// TestAuditSelectorRefusesAnArgumentThatCouldBeEither. AIRA refuses ambiguous
// selectors rather than picking a meaning, and a directory whose name happens
// to be a ticket ID is exactly such a case.
func TestAuditSelectorRefusesAnArgumentThatCouldBeEither(t *testing.T) {
	dir := t.TempDir()
	ambiguous := filepath.Join(dir, "AIRA-1")
	if err := mkdir(ambiguous); err != nil {
		t.Fatal(err)
	}
	_, err := resolveAuditSelector(ambiguous, func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "E_SELECTOR_AMBIGUOUS") {
		t.Fatalf("err=%v, want E_SELECTOR_AMBIGUOUS", err)
	}
}

func TestAuditSelectorFormsResolveIndependently(t *testing.T) {
	dir := t.TempDir()
	selector, err := resolveAuditSelector("AIRA-1", func(id string) bool { return id == "AIRA-1" })
	if err != nil || selector.TicketID != "AIRA-1" || selector.Path != "" {
		t.Fatalf("selector=%+v err=%v, want the ticket form", selector, err)
	}
	selector, err = resolveAuditSelector(dir, func(string) bool { return false })
	if err != nil || selector.Path == "" || selector.TicketID != "" {
		t.Fatalf("selector=%+v err=%v, want the path form", selector, err)
	}
	selector, err = resolveAuditSelector("", nil)
	if err != nil || selector != (worktreeSelectorZero) {
		t.Fatalf("selector=%+v err=%v, want the unscoped form", selector, err)
	}
	if _, err := resolveAuditSelector("not-a-thing", func(string) bool { return false }); err == nil ||
		!strings.Contains(err.Error(), "E_SELECTOR_INVALID") {
		t.Fatalf("err=%v, want E_SELECTOR_INVALID for an argument that names neither", err)
	}
}

// TestWorktreeVerbsRefuseAStoreThatCannotAnswer: a store without the binding
// capability reports E_WORKTREE_UNAVAILABLE rather than an empty report, which
// would read as "no worktrees hold anything" — a fabricated all-clear.
func TestWorktreeVerbsRefuseAStoreThatCannotAnswer(t *testing.T) {
	c := New(metadataProbeStore{})
	if _, err := c.worktreeCapableStore(); err == nil || !strings.Contains(err.Error(), "E_WORKTREE_UNAVAILABLE") {
		t.Fatalf("err=%v, want E_WORKTREE_UNAVAILABLE", err)
	}
}

// TestWorktreeVerbsAreClientRouted is Astra's finding: nothing in Classify makes
// "touches git locally" imply RouteClient. Left unwired these two verbs would
// run inside the daemon and classify the DAEMON's checkout, not the caller's.
func TestWorktreeVerbsAreClientRouted(t *testing.T) {
	for _, verb := range []string{"worktree-register", "worktree-audit"} {
		if _, route := Classify(verb, ""); route != RouteClient {
			t.Fatalf("%s routed %v, want RouteClient", verb, route)
		}
	}
	// The rule is a prefix, so a later worktree verb inherits it rather than
	// silently falling through to the daemon default.
	if _, route := Classify("worktree-unregister", ""); route != RouteClient {
		t.Fatal("the worktree- prefix rule does not cover a future sibling verb")
	}
	// Guard the rule is narrow: an unrelated verb must still be daemon-routed.
	if _, route := Classify("touch", ""); route != RouteDaemon {
		t.Fatal("the worktree- prefix rule leaked onto an unrelated verb")
	}
}

// TestWorktreeVerbsAreNotStoreFreeCarved: both need the project store (bindings,
// leases, ticket status), so neither may take the store-free carve-out.
func TestWorktreeVerbsAreNotStoreFreeCarved(t *testing.T) {
	for _, verb := range []string{"worktree-register", "worktree-audit"} {
		if StoreFreeCarved(verb, map[string]any{}) {
			t.Fatalf("%s claims to need no store", verb)
		}
	}
}
