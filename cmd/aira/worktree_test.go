package main

import (
	"context"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/runner"
)

func TestCanonicalWorktreeVerbRewritesBothSubverbs(t *testing.T) {
	for _, subverb := range []string{"register", "audit"} {
		got, err := canonicalWorktreeVerb([]string{"worktree", subverb, "AIRA-1", "--base", "origin/master"})
		if err != nil {
			t.Fatalf("%s: %v", subverb, err)
		}
		want := []string{"worktree-" + subverb, "AIRA-1", "--base", "origin/master"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("rewrite=%v, want %v", got, want)
		}
	}
}

func TestCanonicalWorktreeVerbRefusesAnUnknownOrMissingSubverb(t *testing.T) {
	for _, argv := range [][]string{
		{"worktree"},
		{"worktree", "--base", "origin/master"},
		{"worktree", "gc"},
		{"worktree", "remove", "/wt/x"},
	} {
		if _, err := canonicalWorktreeVerb(argv); err == nil {
			t.Fatalf("%v was accepted; an unknown subverb must be refused by name", argv)
		} else if !strings.Contains(err.Error(), "register or audit") {
			t.Fatalf("%v: err=%v, want both valid forms named", argv, err)
		}
	}
}

func TestWorktreeRegisterBuildsItsRequest(t *testing.T) {
	positional, options, err := parseArgs("worktree-register", []string{"AIRA-1", "--base", "origin/master", "--owner", "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("worktree-register", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["selector"] != "AIRA-1" || request.Args["base"] != "origin/master" || request.Args["owner"] != "session-a" {
		t.Fatalf("args=%v", request.Args)
	}
	// Attestation is DERIVED by the face from the resolved owner. buildRequest
	// must never leave a caller-supplied truthy value in place.
	if attested, _ := request.Args["owner_attested"].(bool); attested {
		t.Fatal("owner_attested must not be settable before owner resolution")
	}
}

func TestWorktreeAuditBuildsItsRequestAndRefusesTwoSelectors(t *testing.T) {
	positional, options, err := parseArgs("worktree-audit", []string{"AIRA-1"})
	if err != nil {
		t.Fatal(err)
	}
	request, err := buildRequest("worktree-audit", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["selector"] != "AIRA-1" {
		t.Fatalf("args=%v", request.Args)
	}
	if _, err := buildRequest("worktree-audit", []string{"AIRA-1", "/wt/other"}, map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), "E_SELECTOR_AMBIGUOUS") {
		t.Fatalf("err=%v, want E_SELECTOR_AMBIGUOUS for two selectors", err)
	}
}

// TestWorktreeOptionsAreRefusedWhereTheyDoNotApply pins the per-subverb
// allow-map. --owner on `audit` would be silently accepted and discarded
// without it, and --owner-attested must be unreachable from the command line
// on either verb or the evidence grade is forgeable.
func TestWorktreeOptionsAreRefusedWhereTheyDoNotApply(t *testing.T) {
	if _, _, err := parseArgs("worktree-audit", []string{"--owner", "session-a"}); err == nil {
		t.Fatal("--owner was accepted for audit, which declares no identity")
	}
	for _, verb := range []string{"worktree-register", "worktree-audit"} {
		if _, _, err := parseArgs(verb, []string{"--owner-attested", "true"}); err == nil {
			t.Fatalf("%s accepted --owner-attested from the command line", verb)
		}
		if _, _, err := parseArgs(verb, []string{"--token", "t"}); err == nil {
			t.Fatalf("%s accepted an option that belongs to another verb", verb)
		}
	}
}

// TestStampWorktreeOwnerDerivesAttestationFromTheResolvedIdentity covers both
// legs of the honesty split, using the same predicate the confine kill-guard
// uses (runner.ConfineOwnerIsAttested).
func TestStampWorktreeOwnerDerivesAttestationFromTheResolvedIdentity(t *testing.T) {
	t.Setenv("AIRA_CONFINE_OWNER", "session-attested")
	request := core.Request{Verb: "worktree-register", Args: map[string]any{"selector": "AIRA-1"}}
	if err := stampWorktreeOwner(context.Background(), daemon.WorktreeScope{Root: t.TempDir()}, &request); err != nil {
		t.Fatal(err)
	}
	if request.Args["owner"] != "session-attested" {
		t.Fatalf("owner=%v", request.Args["owner"])
	}
	if attested, _ := request.Args["owner_attested"].(bool); !attested {
		t.Fatal("an AIRA_CONFINE_OWNER identity must be reported attested")
	}

	t.Setenv("AIRA_CONFINE_OWNER", "")
	inferredRoot := t.TempDir()
	inferred := core.Request{Verb: "worktree-register", Args: map[string]any{"selector": "AIRA-1"}}
	if err := stampWorktreeOwner(context.Background(), daemon.WorktreeScope{Root: inferredRoot}, &inferred); err != nil {
		t.Fatal(err)
	}
	owner, _ := inferred.Args["owner"].(string)
	if !strings.HasPrefix(owner, runner.ConfineInferredOwnerPrefix) && owner != runner.ConfineUnknownOwner {
		t.Fatalf("owner=%q, want an inferred or unknown identity outside a project", owner)
	}
	if attested, _ := inferred.Args["owner_attested"].(bool); attested {
		t.Fatal("a cwd-inferred identity must never be reported attested")
	}
}

// TestStampWorktreeOwnerResolvesFromTheScopeRootNotTheProcessCwd. Over MCP the
// process cwd is wherever the host launched the server, so a cwd-rooted
// resolution would attribute a binding to a directory outside the repository.
func TestStampWorktreeOwnerResolvesFromTheScopeRootNotTheProcessCwd(t *testing.T) {
	t.Setenv("AIRA_CONFINE_OWNER", "")
	scopeRoot := t.TempDir()
	request := core.Request{Verb: "worktree-register", Args: map[string]any{"selector": "AIRA-1"}}
	if err := stampWorktreeOwner(context.Background(), daemon.WorktreeScope{Root: scopeRoot}, &request); err != nil {
		t.Fatal(err)
	}
	owner, _ := request.Args["owner"].(string)
	if owner == "" {
		t.Fatal("no owner resolved")
	}
	if strings.HasPrefix(owner, runner.ConfineInferredOwnerPrefix) &&
		!strings.Contains(owner, lastPathSegment(scopeRoot)) {
		t.Fatalf("owner=%q was inferred from a directory other than the scope root %q", owner, scopeRoot)
	}
}

// TestStampWorktreeOwnerTouchesNoOtherVerb: the stamp is verb-scoped, so it can
// never inject an owner argument into a verb whose schema has no such field.
func TestStampWorktreeOwnerTouchesNoOtherVerb(t *testing.T) {
	request := core.Request{Verb: "worktree-audit", Args: map[string]any{}}
	if err := stampWorktreeOwner(context.Background(), daemon.WorktreeScope{Root: t.TempDir()}, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Args) != 0 {
		t.Fatalf("args=%v, want the audit request untouched", request.Args)
	}
}

func lastPathSegment(path string) string {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	return parts[len(parts)-1]
}

// TestRelayedBindingMustDescribeTheRequestedWrite. A relay that echoed a
// different ticket or a different checkout would make the client report a
// registration that never happened for the thing it named.
func TestRelayedBindingMustDescribeTheRequestedWrite(t *testing.T) {
	input := domain.WorktreeBindingInput{TicketID: "AIRA-1"}
	good := domain.WorktreeBinding{TicketID: "AIRA-1", WorktreeID: "w", RegisteredAt: "t"}
	if err := validateRelayedWorktreeBinding(good, input, "w"); err != nil {
		t.Fatalf("a faithful result was rejected: %v", err)
	}
	for name, bad := range map[string]domain.WorktreeBinding{
		"wrong ticket":     {TicketID: "AIRA-2", WorktreeID: "w", RegisteredAt: "t"},
		"wrong worktree":   {TicketID: "AIRA-1", WorktreeID: "other", RegisteredAt: "t"},
		"no ticket":        {TicketID: "", WorktreeID: "w", RegisteredAt: "t"},
		"no time":          {TicketID: "AIRA-1", WorktreeID: "w"},
		"attested no owne": {TicketID: "AIRA-1", WorktreeID: "w", RegisteredAt: "t", OwnerAttested: true},
	} {
		if err := validateRelayedWorktreeBinding(bad, input, "w"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
