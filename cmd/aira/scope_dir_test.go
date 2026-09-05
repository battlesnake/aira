package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
)

// scopeDirProject makes a git worktree with an .aira/config and one commit, so
// discovery, scope construction and git-context resolution all succeed inside it.
func scopeDirProject(t *testing.T, parent, name, slug string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"schema":1,"project":{"slug":%q,"prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`, slug)
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"git", "-C", root, "init", "-q"},
		{"git", "-C", root, "add", ".aira/config"},
		{"git", "-C", root, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", name},
	} {
		if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", argv, err, out)
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// scopeDirFaceEnv chdirs into serverCwd and points the daemon paths at a
// throwaway state/runtime dir, reproducing the shape of the AIRA-82 defect: a
// face process whose own working directory is not the caller's worktree.
func scopeDirFaceEnv(t *testing.T) (serverCwd, callerWorktree string) {
	t.Helper()
	parent := t.TempDir()
	serverCwd = scopeDirProject(t, parent, "face-process-cwd", "faceproc")
	callerWorktree = scopeDirProject(t, parent, "caller-worktree", "callerwt")
	t.Chdir(serverCwd)
	t.Setenv("XDG_STATE_HOME", filepath.Join(parent, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	return serverCwd, callerWorktree
}

// captureMCPRantFrame drives the real MCP face over a daemonDispatcher whose
// transport is intercepted, so the captured frame is exactly what production
// would put on the wire: the discovered scope plus the git context stamped from
// it. `aira_rant` is the verb RANT-18 was filed with and it opts into git
// context, which is the provenance this ticket is about.
func captureMCPRantFrame(t *testing.T, scopeDir string) daemon.RequestFrame {
	t.Helper()
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var captured []daemon.RequestFrame
	dispatcher := &daemonDispatcher{paths: paths, jsonOutput: true}
	dispatcher.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		captured = append(captured, frame)
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
	}
	arguments := map[string]any{"text": "worktree provenance"}
	if scopeDir != "" {
		arguments[scopeDirArgument] = scopeDir
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_rant","arguments":%s}}`, encoded) + "\n"
	var out, diagnostics bytes.Buffer
	if code := runMCPWithDispatcher(context.Background(), strings.NewReader(message), &out, &diagnostics, dispatcher); code != 0 {
		t.Fatalf("mcp exit=%d out=%q diagnostics=%q", code, out.String(), diagnostics.String())
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d frames, want 1: out=%q", len(captured), out.String())
	}
	return captured[0]
}

// verifies: AIRA-82 — an MCP call carries no cwd of its own, so without an
// explicit override every request is scoped (and git-stamped) against the MCP
// server process's directory rather than the worktree it was filed from.
func TestMCPScopeDirOverridesTheFaceProcessWorkingDirectory(t *testing.T) {
	serverCwd, callerWorktree := scopeDirFaceEnv(t)

	defaulted := captureMCPRantFrame(t, "")
	if defaulted.Scope.Root != serverCwd {
		t.Fatalf("default scope root=%q, want the face process cwd %q", defaulted.Scope.Root, serverCwd)
	}

	overridden := captureMCPRantFrame(t, callerWorktree)
	if overridden.Scope.Root != callerWorktree {
		t.Fatalf("overridden scope root=%q, want %q", overridden.Scope.Root, callerWorktree)
	}
	if overridden.Scope.ProjectID == defaulted.Scope.ProjectID || overridden.Scope.WorktreeID == defaulted.Scope.WorktreeID {
		t.Fatalf("override did not change project/worktree identity: default=%+v overridden=%+v", defaulted.Scope, overridden.Scope)
	}
	// The RANT-18 symptom itself: the recorded provenance must name the
	// worktree the call was filed from, not the face's own directory.
	if overridden.Request.GitContext == nil || defaulted.Request.GitContext == nil {
		t.Fatalf("git context missing: default=%+v overridden=%+v", defaulted.Request.GitContext, overridden.Request.GitContext)
	}
	if overridden.Request.GitContext.WorktreePath.Value != callerWorktree {
		t.Fatalf("stamped worktree_path=%+v, want %q", overridden.Request.GitContext.WorktreePath, callerWorktree)
	}
	if defaulted.Request.GitContext.WorktreePath.Value != serverCwd {
		t.Fatalf("default stamped worktree_path=%+v, want %q", defaulted.Request.GitContext.WorktreePath, serverCwd)
	}
	if overridden.Request.GitContext.HeadHash.Value == "" ||
		overridden.Request.GitContext.HeadHash.Value == defaulted.Request.GitContext.HeadHash.Value {
		t.Fatalf("stamped head_hash did not follow the override: %+v", overridden.Request.GitContext.HeadHash)
	}
}

// verifies: AIRA-82 — the override names a directory for discovery to run in;
// it never becomes a core argument, so MCP and CLI requests stay byte-identical.
func TestMCPScopeDirNeverReachesTheCoreRequest(t *testing.T) {
	_, callerWorktree := scopeDirFaceEnv(t)
	defaulted := captureMCPRantFrame(t, "")
	overridden := captureMCPRantFrame(t, callerWorktree)
	if !reflect.DeepEqual(defaulted.Request.Args, overridden.Request.Args) {
		t.Fatalf("scope_dir leaked into core args: default=%#v overridden=%#v", defaulted.Request.Args, overridden.Request.Args)
	}
	if _, present := overridden.Request.Args[scopeDirArgument]; present {
		t.Fatalf("scope_dir present in core args: %#v", overridden.Request.Args)
	}
}

// verifies: AIRA-82 — the override is advertised on every project-scoped tool
// and refused on the project-less ones, so it is discoverable and never
// silently ignored.
func TestMCPScopeDirIsDeclaredOnProjectScopedToolsOnly(t *testing.T) {
	server := newMCPServer(nil)
	projectLess := map[string]bool{"aira_eject": true, "aira_confine_list": true, "aira_confine_kill": true}
	for _, tool := range server.tools {
		schema, ok := tool.InputSchema.(mcpInputSchema)
		if !ok {
			t.Fatalf("%s schema type=%T", tool.Name, tool.InputSchema)
		}
		property, declared := schema.Properties[scopeDirArgument]
		if projectLess[tool.Name] {
			if declared {
				t.Fatalf("%s is project-less but declares %s", tool.Name, scopeDirArgument)
			}
			continue
		}
		if !declared || property.Type != "string" || property.Description == "" {
			t.Fatalf("%s scope_dir property=%+v declared=%v", tool.Name, property, declared)
		}
		for _, required := range schema.Required {
			if required == scopeDirArgument {
				t.Fatalf("%s makes %s required", tool.Name, scopeDirArgument)
			}
		}
	}
}

// verifies: AIRA-82 — the MCP face resolves a directory at two further sites
// besides the general scope: init's bootstrap discovery and the relativisation
// of init's reported paths. Both follow the override, so an init issued for
// another worktree neither bootstraps the wrong one nor reports paths relative
// to the server's own directory.
func TestMCPScopeDirCoversTheInitBootstrapAndRelativisationSites(t *testing.T) {
	_, callerWorktree := scopeDirFaceEnv(t)
	var got daemon.WorktreeScope
	dispatcher := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, _ core.Request) core.Response {
		got = scope
		return core.Response{OK: true, Code: "OK", Data: app.InitResult{
			Root:    filepath.Join(callerWorktree, "sub"),
			Config:  filepath.Join(callerWorktree, "sub", ".aira", "config"),
			Created: true,
		}}
	})
	message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_init","arguments":{"scope_dir":%q}}}`, callerWorktree) + "\n"
	var out, diagnostics bytes.Buffer
	if code := runMCPWithDispatcher(context.Background(), strings.NewReader(message), &out, &diagnostics, dispatcher); code != 0 {
		t.Fatalf("mcp exit=%d out=%q diagnostics=%q", code, out.String(), diagnostics.String())
	}
	if got.Root != callerWorktree || !got.Bootstrap {
		t.Fatalf("init bootstrap scope=%+v, want root %q", got, callerWorktree)
	}
	if !strings.Contains(out.String(), `"root":"sub"`) {
		t.Fatalf("init paths were not relativised against the override: %q", out.String())
	}
}

// verifies: AIRA-82 (build-review finding) — a RELATIVE scope_dir would be
// resolved against the MCP server's own directory, the one thing the caller
// cannot see and is trying to escape. Accepting one would hand back the same
// wrong scope under a new name, so MCP takes absolute paths only. The CLI, whose
// process cwd IS the caller's own directory, still takes a relative one.
func TestMCPScopeDirRefusesARelativePathThatTheCLIAccepts(t *testing.T) {
	serverCwd, callerWorktree := scopeDirFaceEnv(t)
	relative := filepath.Join("..", filepath.Base(callerWorktree))

	server := newMCPServer(nil)
	message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_rant","arguments":{"text":"x","scope_dir":%q}}}`, relative) + "\n"
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(message), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "must be an absolute path") {
		t.Fatalf("MCP accepted a relative scope_dir: %q", out.String())
	}

	// The same relative path is meaningful on the CLI and must keep working.
	if _, err := os.Stat(filepath.Join(serverCwd, relative)); err != nil {
		t.Fatal(err)
	}
	scope, exit, stdout, stderr := scopeForCLIRun(t, []string{"--scope-dir", relative, "ls", "--json"})
	if exit != 0 || scope.Root != callerWorktree {
		t.Fatalf("CLI relative --scope-dir exit=%d root=%q stdout=%q stderr=%q", exit, scope.Root, stdout, stderr)
	}
}

// verifies: AIRA-82 (build-review finding) — the override sends imported CONTENT
// to another project while `file` is still read relative to the face's own
// directory. On MCP those two bases differ invisibly, so the combination is
// refused rather than importing a same-named file from the wrong directory.
func TestMCPScopeDirRefusesARelativeImportFile(t *testing.T) {
	serverCwd, callerWorktree := scopeDirFaceEnv(t)
	if err := os.WriteFile(filepath.Join(serverCwd, "notes.md"), []byte("# wrong project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	dispatcher := &daemonDispatcher{paths: paths, jsonOutput: true}
	dispatcher.exchange = func(_ context.Context, _ string, _ daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent++
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
	}
	message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_import","arguments":{"file":"notes.md","scope_dir":%q}}}`, callerWorktree) + "\n"
	var out, diagnostics bytes.Buffer
	if code := runMCPWithDispatcher(context.Background(), strings.NewReader(message), &out, &diagnostics, dispatcher); code != 0 {
		t.Fatalf("mcp exit=%d out=%q diagnostics=%q", code, out.String(), diagnostics.String())
	}
	if sent != 0 {
		t.Fatalf("a relative import file was dispatched against an overridden scope: %q", out.String())
	}
	if !strings.Contains(out.String(), "E_IMPORT_INVALID") || !strings.Contains(out.String(), "notes.md") {
		t.Fatalf("refusal did not name the ambiguity: %q", out.String())
	}
}

// verifies: AIRA-82 — a tool that has no project scope refuses the override
// rather than accepting and discarding it.
func TestMCPScopeDirIsRefusedByProjectlessTools(t *testing.T) {
	server := newMCPServer(nil)
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_confine_list","arguments":{"scope_dir":"/tmp"}}}` + "\n"
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(message), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unknown argument") || !strings.Contains(out.String(), scopeDirArgument) {
		t.Fatalf("project-less tool accepted scope_dir: %q", out.String())
	}
}

// verifies: AIRA-82 — the face injects scope_dir into every project-scoped tool
// schema, which would silently SHADOW a same-named dispatch-table argument if
// one were ever added. No core descriptor may claim that name.
func TestNoDispatchDescriptorClaimsTheScopeDirArgumentName(t *testing.T) {
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		for _, arg := range descriptor.Args {
			if arg.Name == scopeDirArgument {
				t.Fatalf("descriptor %q declares %q, which the MCP face injects and would overwrite", descriptor.Name, scopeDirArgument)
			}
		}
		for _, operation := range descriptor.Operations {
			for _, arg := range operation.Args {
				if arg.Name == scopeDirArgument {
					t.Fatalf("descriptor %q operation %q declares %q, which the MCP face injects and would overwrite", descriptor.Name, operation.Name, scopeDirArgument)
				}
			}
		}
	}
}

// verifies: AIRA-82 — an unusable override fails with the path it was given
// and never falls back to the face's own directory.
func TestMCPScopeDirRefusesAnUnusableDirectory(t *testing.T) {
	serverCwd, _ := scopeDirFaceEnv(t)
	missing := filepath.Join(serverCwd, "no-such-worktree")
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	dispatcher := &daemonDispatcher{paths: paths, jsonOutput: true}
	dispatcher.exchange = func(_ context.Context, _ string, _ daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent++
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
	}
	message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_rant","arguments":{"text":"x","scope_dir":%q}}}`, missing) + "\n"
	var out, diagnostics bytes.Buffer
	if code := runMCPWithDispatcher(context.Background(), strings.NewReader(message), &out, &diagnostics, dispatcher); code != 0 {
		t.Fatalf("mcp exit=%d", code)
	}
	if sent != 0 {
		t.Fatalf("an unusable scope_dir still dispatched %d frames: %q", sent, out.String())
	}
	if !strings.Contains(out.String(), "E_NOT_PROJECT") || !strings.Contains(out.String(), "no-such-worktree") {
		t.Fatalf("refusal did not name the offending path: %q", out.String())
	}
}

// scopeForCLIRun runs the CLI face against a recording dispatcher and returns
// the scope it resolved.
func scopeForCLIRun(t *testing.T, argv []string) (daemon.WorktreeScope, int, string, string) {
	t.Helper()
	var got daemon.WorktreeScope
	calls := 0
	dispatcher := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, _ core.Request) core.Response {
		got, calls = scope, calls+1
		return core.Response{OK: true, Code: "OK"}
	})
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher(argv, &stdout, &stderr, dispatcher)
	if calls > 1 {
		t.Fatalf("dispatched %d times", calls)
	}
	return got, exit, stdout.String(), stderr.String()
}

// verifies: AIRA-82 — the CLI carries the same explicit override, so a hook,
// script or agent that cannot chdir can still name the worktree a call belongs
// to instead of silently inheriting the process cwd.
func TestCLIScopeDirOverridesTheProcessWorkingDirectory(t *testing.T) {
	serverCwd, callerWorktree := scopeDirFaceEnv(t)

	defaulted, exit, stdout, stderr := scopeForCLIRun(t, []string{"ls", "--json"})
	if exit != 0 {
		t.Fatalf("ls exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if defaulted.Root != serverCwd {
		t.Fatalf("default scope root=%q, want %q", defaulted.Root, serverCwd)
	}

	overridden, exit, stdout, stderr := scopeForCLIRun(t, []string{"--scope-dir", callerWorktree, "ls", "--json"})
	if exit != 0 {
		t.Fatalf("overridden ls exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if overridden.Root != callerWorktree {
		t.Fatalf("overridden scope root=%q, want %q", overridden.Root, callerWorktree)
	}
	if overridden.ProjectID == defaulted.ProjectID || overridden.WorktreeID == defaulted.WorktreeID {
		t.Fatalf("override did not change identity: default=%+v overridden=%+v", defaulted, overridden)
	}

	inline, exit, stdout, stderr := scopeForCLIRun(t, []string{"--scope-dir=" + callerWorktree, "ls", "--json"})
	if exit != 0 || inline.Root != callerWorktree {
		t.Fatalf("--scope-dir=DIR exit=%d root=%q stdout=%q stderr=%q", exit, inline.Root, stdout, stderr)
	}

	trailing, exit, stdout, stderr := scopeForCLIRun(t, []string{"ls", "--scope-dir", callerWorktree, "--json"})
	if exit != 0 || trailing.Root != callerWorktree {
		t.Fatalf("trailing --scope-dir exit=%d root=%q stdout=%q stderr=%q", exit, trailing.Root, stdout, stderr)
	}
}

// verifies: AIRA-82 — the CLI resolves a directory at three separate sites
// (bootstrap discovery for init, default-project discovery for eject, and the
// general scope), and all three honour the override. Missing one leaves a verb
// silently scoped to the process cwd.
func TestCLIScopeDirCoversEveryDiscoverySite(t *testing.T) {
	_, callerWorktree := scopeDirFaceEnv(t)
	wanted, err := app.Discover(context.Background(), callerWorktree)
	if err != nil {
		t.Fatal(err)
	}

	var initScope daemon.WorktreeScope
	var stdout, stderr bytes.Buffer
	initArgv := []string{"--scope-dir", callerWorktree, "init"}
	if exit := RunWithDispatcher(initArgv, &stdout, &stderr, dispatcherFunc(
		func(_ context.Context, scope daemon.WorktreeScope, _ core.Request) core.Response {
			initScope = scope
			return core.Response{OK: true, Code: "OK"}
		})); exit != 0 {
		t.Fatalf("init exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if initScope.Root != callerWorktree || !initScope.Bootstrap {
		t.Fatalf("init bootstrap scope=%+v, want root %q", initScope, callerWorktree)
	}

	var ejectRequest core.Request
	stdout.Reset()
	stderr.Reset()
	ejectArgv := []string{"--scope-dir", callerWorktree, "eject"}
	if exit := RunWithDispatcher(ejectArgv, &stdout, &stderr, dispatcherFunc(
		func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
			ejectRequest = request
			return core.Response{OK: true, Code: "OK"}
		})); exit != 0 {
		t.Fatalf("eject exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if project, _ := ejectRequest.Args["project"].(string); project != wanted.ProjectID {
		t.Fatalf("eject default project=%q, want %q (the override's project)", project, wanted.ProjectID)
	}
}

// verifies: AIRA-82 — a verb with no project scope refuses the override rather
// than accepting and discarding it.
func TestCLIScopeDirIsRefusedForProjectlessVerbs(t *testing.T) {
	_, callerWorktree := scopeDirFaceEnv(t)
	for _, verb := range []string{"confine", "confine-list", "worker-admit", "governor-slot"} {
		var stdout, stderr bytes.Buffer
		exit := RunWithDispatcher([]string{"--scope-dir", callerWorktree, verb, "--", "true"}, &stdout, &stderr, dispatcherFunc(
			func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
				t.Fatalf("%s dispatched with a scope override", verb)
				return core.Response{}
			}))
		combined := stdout.String() + stderr.String()
		if exit == 0 || !strings.Contains(combined, "E_SELECTOR_INVALID") || !strings.Contains(combined, scopeDirFlag) {
			t.Fatalf("%s exit=%d output=%q", verb, exit, combined)
		}
	}
}

// verifies: AIRA-82 — an unusable override fails with the path it was given
// rather than resolving against the process cwd.
func TestCLIScopeDirRefusesAnUnusableDirectory(t *testing.T) {
	serverCwd, _ := scopeDirFaceEnv(t)
	notADirectory := filepath.Join(serverCwd, ".aira", "config")
	for _, override := range []string{filepath.Join(serverCwd, "no-such-worktree"), notADirectory} {
		_, exit, stdout, stderr := scopeForCLIRun(t, []string{"--scope-dir", override, "ls", "--json"})
		combined := stdout + stderr
		if exit == 0 || !strings.Contains(combined, "E_NOT_PROJECT") || !strings.Contains(combined, filepath.Base(override)) {
			t.Fatalf("override %q exit=%d output=%q", override, exit, combined)
		}
	}
}

// verifies: AIRA-82 — a face-level global that no dispatch table declares is
// still discoverable: the human help listing names it explicitly.
func TestHumanHelpNamesTheScopeDirGlobal(t *testing.T) {
	var stdout, stderr bytes.Buffer
	entries := []map[string]string{{"verb": "ls", "usage": "ls [query]"}}
	if exit := renderHelp(core.Response{OK: true, Code: "OK", Data: entries}, &stdout, &stderr); exit != 0 {
		t.Fatalf("renderHelp exit=%d stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), scopeDirFlag) {
		t.Fatalf("help output does not name %s: %q", scopeDirFlag, stdout.String())
	}
}

// verifies: AIRA-82 — the scope override is stripped BEFORE --json is, so
// removeJSON still sees the verb at argv[0] and keeps its post-delimiter
// carve-out. Stripping in the other order leaves argv[0] == "--scope-dir",
// removeJSON stops recognising `run`, and a target's own --json is silently
// eaten (the AIRA-57 byte-transparency hazard).
func TestScopeDirIsStrippedBeforeJSONSoTargetFlagsSurvive(t *testing.T) {
	_, callerWorktree := scopeDirFaceEnv(t)
	var got core.Request
	dispatcher := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
		got = request
		return core.Response{OK: true, Code: "OK"}
	})
	var stdout, stderr bytes.Buffer
	argv := []string{"--scope-dir", callerWorktree, "run", "--", "prog", "--json"}
	if exit := RunWithDispatcher(argv, &stdout, &stderr, dispatcher); exit != 0 {
		t.Fatalf("run exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	target, _ := got.Args["argv"].([]string)
	if !reflect.DeepEqual(target, []string{"prog", "--json"}) {
		t.Fatalf("target argv=%#v, want the child's own --json preserved", got.Args["argv"])
	}
}

// verifies: AIRA-82 — the option is a face-level global, stripped before verb
// parsing, and a --scope-dir belonging to a launched target is left alone.
func TestRemoveScopeDirStripsTheGlobalAndNeverTheTarget(t *testing.T) {
	cases := []struct {
		name  string
		argv  []string
		rest  []string
		value string
		fail  bool
	}{
		{name: "absent", argv: []string{"ls", "--json"}, rest: []string{"ls", "--json"}},
		{name: "leading", argv: []string{"--scope-dir", "/w", "ls"}, rest: []string{"ls"}, value: "/w"},
		{name: "inline", argv: []string{"--scope-dir=/w", "ls"}, rest: []string{"ls"}, value: "/w"},
		{name: "trailing", argv: []string{"ls", "--scope-dir", "/w"}, rest: []string{"ls"}, value: "/w"},
		{name: "target argv untouched", argv: []string{"run", "--scope-dir", "/w", "--", "prog", "--scope-dir", "/other"},
			rest: []string{"run", "--", "prog", "--scope-dir", "/other"}, value: "/w"},
		{name: "git refspec delimiter untouched", argv: []string{"git", "push", "origin", "--", "HEAD:main"},
			rest: []string{"git", "push", "origin", "--", "HEAD:main"}},
		{name: "missing value", argv: []string{"ls", "--scope-dir"}, fail: true},
		// Build-review finding: without this, --scope-dir swallows the next
		// option instead of reporting a missing value.
		{name: "value is another option", argv: []string{"--scope-dir", "--json", "ls"}, fail: true},
		{name: "value is the delimiter", argv: []string{"run", "--scope-dir", "--", "prog"}, fail: true},
		{name: "repeated", argv: []string{"--scope-dir", "/a", "--scope-dir", "/b", "ls"}, fail: true},
		{name: "empty inline", argv: []string{"--scope-dir=", "ls"}, fail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, value, err := removeScopeDir(tc.argv)
			if tc.fail {
				if err == nil {
					t.Fatalf("rest=%v value=%q, want an error", rest, value)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rest, tc.rest) || value != tc.value {
				t.Fatalf("rest=%v value=%q, want rest=%v value=%q", rest, value, tc.rest, tc.value)
			}
		})
	}
}
