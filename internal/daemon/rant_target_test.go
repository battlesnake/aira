package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/store"
)

// rantProjectScope stands up a real adopted project on disk — the redirect
// rebuilds the target's scope from its own registered worktree, so a synthetic
// scope would not exercise it.
func rantProjectScope(t *testing.T, paths Paths, name, slug, prefix string) WorktreeScope {
	t.Helper()
	root := filepath.Join(filepath.Dir(paths.StateHome), name)
	if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"schema":1,"project":{"slug":%q,"prefixes":[%q]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`+"\n", slug, prefix)
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := app.Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := ScopeFromProject(project, paths)
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func rantExchange(t *testing.T, paths Paths, scope WorktreeScope, request core.Request) core.Response {
	t.Helper()
	frame, err := Exchange(context.Background(), paths.SocketPath, RequestFrame{Proto: ProtocolVersion, Scope: scope, Request: request})
	if err != nil {
		t.Fatalf("exchange %s: %v", request.Verb, err)
	}
	return frame.CoreResponse()
}

func decodeRant(t *testing.T, response core.Response) domain.Rant {
	t.Helper()
	raw, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Rant domain.Rant `json:"rant"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode rant envelope from %s: %v", raw, err)
	}
	if envelope.Rant.ID == "" {
		var bare domain.Rant
		if err := json.Unmarshal(raw, &bare); err != nil {
			t.Fatalf("decode bare rant from %s: %v", raw, err)
		}
		return bare
	}
	return envelope.Rant
}

// decodeRantListing reads `rant ls`'s bare array, returning the raw JSON too so
// a failure names what was actually listed rather than only a count.
func decodeRantListing(t *testing.T, response core.Response) ([]domain.Rant, []byte) {
	t.Helper()
	if !response.OK {
		t.Fatalf("rant ls: %+v", response)
	}
	raw, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	var rants []domain.Rant
	if err := json.Unmarshal(raw, &rants); err != nil {
		t.Fatalf("decode rant listing %s: %v", raw, err)
	}
	return rants, raw
}

func callerGitContext(scope WorktreeScope) gitcontext.GitContext {
	return gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: scope.Root, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: scope.Root, Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: scope.WorktreeID, Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: "caller-head-verbatim", Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "caller-ref-verbatim", Status: gitcontext.StatusValue},
		RemoteURL:    gitcontext.Field{Value: "caller-remote-verbatim", Status: gitcontext.StatusValue},
		ObservedAt:   "2026-09-09T12:00:00Z", ResolverVersion: "rant-target-test",
	}
}

// The whole point of AIRA-179: a rant filed FROM a downstream project with an
// explicit target lands in the TARGET project's store — its numbering, its
// listing — while the caller's own project records nothing. Its provenance is
// the caller's own worktree, recorded honestly rather than downgraded, and its
// origin is recorded explicitly so it is never indistinguishable from a local
// rant of the target's own.
func TestRantTargetSelectorFilesInTheTargetProject(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	caller := rantProjectScope(t, paths, "downstream", "downstream", "DOWN")
	target := rantProjectScope(t, paths, "shared", "shared", "SHARED")
	// Both projects are adopted on this machine, which is the only precondition
	// the selector has. The caller registers nothing about the target.
	rantExchange(t, paths, caller, core.Request{Verb: "list"})
	rantExchange(t, paths, target, core.Request{Verb: "list"})

	observed := callerGitContext(caller)
	response := rantExchange(t, paths, caller, core.Request{
		Verb:       "rant",
		Args:       map[string]any{"subverb": "capture", "text": "aira confine hid why my job waited", "prefix": "shared", "tags": []string{"cross-project"}},
		Actor:      "opus",
		GitContext: &observed,
	})
	if !response.OK {
		t.Fatalf("targeted capture: %+v", response)
	}
	filed := decodeRant(t, response)
	if filed.OriginProjectID != caller.ProjectID {
		t.Fatalf("origin_project_id = %q, want the caller project %q", filed.OriginProjectID, caller.ProjectID)
	}
	if filed.GitContext != observed {
		t.Fatalf("caller provenance was rewritten: %#v", filed.GitContext)
	}
	// The event this capture allocated belongs to the TARGET's sequence, which
	// is what makes the redirect a real store swap rather than a relabelled
	// local write: the journal and search index follow the same store.
	rawResponse, _ := json.Marshal(response.Data)
	var envelope struct {
		Event struct {
			ProjectID string `json:"project_id"`
			Seq       int64  `json:"seq"`
		} `json:"event"`
	}
	if err := json.Unmarshal(rawResponse, &envelope); err != nil {
		t.Fatalf("decode capture event from %s: %v", rawResponse, err)
	}
	if envelope.Event.ProjectID != target.ProjectID || envelope.Event.Seq == 0 {
		t.Fatalf("capture event = %+v, want a sequenced event of target project %q", envelope.Event, target.ProjectID)
	}

	// It is readable through the same selector, and absent locally.
	got := rantExchange(t, paths, caller, core.Request{Verb: "rant", Args: map[string]any{"subverb": "get", "selector": filed.ID, "prefix": "SHARED"}})
	if !got.OK || decodeRant(t, got).OriginProjectID != caller.ProjectID {
		t.Fatalf("targeted get: %+v", got)
	}
	local := rantExchange(t, paths, caller, core.Request{Verb: "rant", Args: map[string]any{"subverb": "ls"}})
	if !local.OK {
		t.Fatalf("local ls: %+v", local)
	}
	if localRants, raw := decodeRantListing(t, local); len(localRants) != 0 {
		t.Fatalf("the redirected rant also landed locally: %s", raw)
	}
	// And it is a first-class rant of the target's, numbered by the target.
	targetRants, rawTarget := decodeRantListing(t, rantExchange(t, paths, target, core.Request{Verb: "rant", Args: map[string]any{"subverb": "ls"}}))
	if len(targetRants) != 1 || targetRants[0].ID != filed.ID || targetRants[0].OriginProjectID != caller.ProjectID {
		t.Fatalf("target listing = %s", rawTarget)
	}
}

// Naming your own project is an ordinary local rant, not a foreign one: the
// selector says WHERE, it does not create a second kind of rant. It must also
// not reach the core's "requires the daemon transport" refusal.
func TestRantTargetSelectorNamingTheCallersOwnProjectStaysLocal(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	caller := rantProjectScope(t, paths, "self", "self", "SELF")
	rantExchange(t, paths, caller, core.Request{Verb: "list"})

	observed := callerGitContext(caller)
	response := rantExchange(t, paths, caller, core.Request{
		Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "my own project's friction", "prefix": "SELF"},
		Actor: "opus", GitContext: &observed,
	})
	if !response.OK {
		t.Fatalf("self-targeted capture: %+v", response)
	}
	filed := decodeRant(t, response)
	if filed.OriginProjectID != "" {
		t.Fatalf("a self-targeted rant recorded origin %q, want empty", filed.OriginProjectID)
	}
	if filed.GitContext != observed {
		t.Fatalf("caller provenance was rewritten: %#v", filed.GitContext)
	}
	local := rantExchange(t, paths, caller, core.Request{Verb: "rant", Args: map[string]any{"subverb": "get", "selector": filed.ID}})
	if !local.OK {
		t.Fatalf("the self-targeted rant is not readable locally: %+v", local)
	}
}

// Nothing guesses a target. Every unresolvable selector refuses by name, with
// the codes the one machine-wide resolver already owns.
func TestRantTargetSelectorRefusalsAreInherited(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	caller := rantProjectScope(t, paths, "refusals", "refusals", "REF")
	rantExchange(t, paths, caller, core.Request{Verb: "list"})
	observed := callerGitContext(caller)

	for name, probe := range map[string]struct {
		args map[string]any
		want string
	}{
		"unowned prefix":  {map[string]any{"subverb": "capture", "text": "x", "prefix": "NOBODY"}, "E_NOT_ADOPTED"},
		"unknown project": {map[string]any{"subverb": "capture", "text": "x", "project": "project-that-is-not-adopted"}, "E_NOT_ADOPTED"},
		"both selectors":  {map[string]any{"subverb": "capture", "text": "x", "prefix": "REF", "project": caller.ProjectID}, "E_SELECTOR_AMBIGUOUS"},
		"read refusal":    {map[string]any{"subverb": "ls", "prefix": "NOBODY"}, "E_NOT_ADOPTED"},
	} {
		response := rantExchange(t, paths, caller, core.Request{Verb: "rant", Args: probe.args, Actor: "opus", GitContext: &observed})
		if response.OK || response.Code != probe.want {
			t.Fatalf("%s: code=%q ok=%v error=%q, want %s", name, response.Code, response.OK, response.Error, probe.want)
		}
	}
	// A refused target files nothing, anywhere.
	local := rantExchange(t, paths, caller, core.Request{Verb: "rant", Args: map[string]any{"subverb": "ls"}})
	if listing, raw := decodeRantListing(t, local); len(listing) != 0 {
		t.Fatalf("a refused target still filed something locally: %s", raw)
	}
}

// Typed refs validate against the TARGET, which is the natural consequence of
// the store swap rather than a special case — and the sharpest proof that the
// redirect really changed store rather than only relabelling the write.
func TestRantTargetSelectorValidatesTypedRefsAgainstTheTarget(t *testing.T) {
	paths := testPaths(t)
	server := NewServer(paths)
	_, _ = startServer(t, server)
	caller := rantProjectScope(t, paths, "refs-caller", "refscaller", "RCALL")
	target := rantProjectScope(t, paths, "refs-target", "refstarget", "RTGT")
	callerTicket := rantExchange(t, paths, caller, core.Request{Verb: "create", Args: map[string]any{"title": "caller ticket"}})
	targetTicket := rantExchange(t, paths, target, core.Request{Verb: "create", Args: map[string]any{"title": "target ticket"}})
	if !callerTicket.OK || !targetTicket.OK {
		t.Fatalf("fixture tickets: caller=%+v target=%+v", callerTicket, targetTicket)
	}
	ticketID := func(response core.Response) string {
		raw, _ := json.Marshal(response.Data)
		var envelope struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("decode ticket id from %s: %v", raw, err)
		}
		return envelope.ID
	}
	observed := callerGitContext(caller)

	accepted := rantExchange(t, paths, caller, core.Request{
		Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "cites the target's own ticket", "prefix": "RTGT", "refs": []string{"ticket:" + ticketID(targetTicket)}},
		Actor: "opus", GitContext: &observed,
	})
	if !accepted.OK {
		t.Fatalf("a ref to the target's own ticket was refused: %+v", accepted)
	}
	refused := rantExchange(t, paths, caller, core.Request{
		Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "cites a ticket only the caller has", "prefix": "RTGT", "refs": []string{"ticket:" + ticketID(callerTicket)}},
		Actor: "opus", GitContext: &observed,
	})
	if refused.OK {
		t.Fatalf("a caller-only ticket ref was accepted in the target: %+v", refused)
	}
}

// withoutRantTarget must strip BOTH selectors and nothing else: leaving either
// in would trip the core's refusal, and dropping anything else would silently
// lose a caller argument.
func TestWithoutRantTargetStripsOnlyTheSelector(t *testing.T) {
	original := core.Request{Verb: "rant", Args: map[string]any{
		"subverb": "capture", "text": "body", "prefix": "SHARED", "project": "project-shared", "tags": []string{"a"},
	}}
	resolved := withoutRantTarget(original)
	if _, present := resolved.Args["prefix"]; present {
		t.Fatal("prefix survived the strip")
	}
	if _, present := resolved.Args["project"]; present {
		t.Fatal("project survived the strip")
	}
	if resolved.Args["subverb"] != "capture" || resolved.Args["text"] != "body" {
		t.Fatalf("stripped request lost caller arguments: %#v", resolved.Args)
	}
	if original.Args["prefix"] != "SHARED" {
		t.Fatal("the strip mutated the original request an observer already saw")
	}
}

func TestRantTargetIgnoresOtherVerbsAndEmptySelectors(t *testing.T) {
	for name, request := range map[string]core.Request{
		"other verb":      {Verb: "eject", Args: map[string]any{"prefix": "AIRA"}},
		"no selector":     {Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "x"}},
		"blank selectors": {Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "x", "prefix": "  ", "project": ""}},
		"nil selector":    {Verb: "rant", Args: map[string]any{"subverb": "capture", "text": "x", "prefix": nil}},
	} {
		_, _, targeted, err := rantTarget(request)
		if targeted || err != nil {
			t.Fatalf("%s: targeted=%v err=%v", name, targeted, err)
		}
	}
	project, prefix, targeted, err := rantTarget(core.Request{Verb: "rant", Args: map[string]any{"prefix": " SHARED "}})
	if !targeted || err != nil || prefix != "SHARED" || project != "" {
		t.Fatalf("rantTarget = (%q,%q,%v,%v)", project, prefix, targeted, err)
	}
}

// A selector the daemon cannot READ must refuse, never collapse to "no
// selector". Everywhere else in the dispatch table an unreadable argument
// degrades to absent, which is harmless — but absent here means "file it in the
// caller's own project", so the usual idiom would silently write the rant to a
// DIFFERENT project than the one that was named. Fail closed instead.
func TestRantTargetRefusesAnUnreadableSelectorRatherThanFilingLocally(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"numeric project": {"subverb": "capture", "text": "x", "project": 7},
		"list prefix":     {"subverb": "capture", "text": "x", "prefix": []string{"AIRA"}},
		"bool prefix":     {"subverb": "capture", "text": "x", "prefix": true},
	} {
		project, prefix, targeted, err := rantTarget(core.Request{Verb: "rant", Args: args})
		if err == nil {
			t.Fatalf("%s: rantTarget = (%q,%q,%v) with no error — an unreadable selector became a local rant", name, project, prefix, targeted)
		}
		if store.ErrorCode(err) != "E_RANT_INVALID" {
			t.Fatalf("%s: error %v, want E_RANT_INVALID", name, err)
		}
	}
}
