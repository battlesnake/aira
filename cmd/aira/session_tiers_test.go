package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-381. The setter verb, `confine --list` and `aira top` faces of session
// tiers. T-numbers are the spec's §5.

const topSessionColumn = 9

// tierDaemon serves a real daemon on a temp state dir whose slice resolves to
// sliceDir, from outside any project, and returns its paths.
func tierDaemon(t *testing.T, sliceDir string) daemon.Paths {
	t.Helper()
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("AIRA_CONFINE_SLICE", "")
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.DBPath), 0o700); err != nil {
		t.Fatal(err)
	}
	server := daemon.NewServer(paths)
	server.SetAdmitResolveSliceForTest(func(string) (string, bool, string) { return sliceDir, true, "" })
	server.SetRestartFreezeForTest(0)
	startCommandDaemon(t, server)
	return paths
}

func storedSessionTiers(t *testing.T, paths daemon.Paths) map[string]string {
	t.Helper()
	conn, err := sql.Open("sqlite", paths.DBPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.Query(`SELECT owner, tier FROM confine_session_tiers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	marks := map[string]string{}
	for rows.Next() {
		var owner, tier string
		if err := rows.Scan(&owner, &tier); err != nil {
			t.Fatal(err)
		}
		marks[owner] = tier
	}
	return marks
}

func runCLI(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := runWithInput(argv, &stdout, &stderr, strings.NewReader(""))
	return exit, stdout.String(), stderr.String()
}

// listedSessionTiers is `confine --list --json`'s session_tiers on the test
// slice, whose directory does not exist: the listing's UNEVALUATED path.
func listedSessionTiers(t *testing.T) string {
	t.Helper()
	exit, stdout, stderr := runCLI(t, "confine", "--list", "--slice", "test.slice", "--json")
	if exit != 3 { // an UNEVALUATED reply exits 3
		t.Fatalf("confine --list --json exit=%d stdout=%q stderr=%q, want the unevaluated exit", exit, stdout, stderr)
	}
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("list output %q: %v", stdout, err)
	}
	if verdict := string(envelope.Data["verdict"]); verdict != `"unevaluated"` {
		t.Fatalf("test premise: the test slice lists unevaluated, got verdict %s", verdict)
	}
	return string(envelope.Data["session_tiers"])
}

// verifies: AIRA-381 T19a, T17c (CLI half) — `aira confine-session-tier
// field low --owner me` from outside any project, against a real daemon on a
// temp socket: OK, and both the published table (via `confine --list --json`
// on the UNEVALUATED path, a slice the listing cannot read) and the store hold
// `field: low`; a `cc-<uuid>` mark is listed with its FULL owner; medium
// deletes the row. Mutations: the verb missing from main.go's verb form, the
// option allowlist, the route or Dispatch (a refusal); the slice insertion kept
// (the daemon refuses the key). The text line is
// TestSessionTierListLineRendersEveryState's (stdout here is not a terminal).
func TestSessionTierCLISetsAgainstARealDaemon(t *testing.T) {
	sliceDir := filepath.Join(t.TempDir(), "missing-slice")
	paths := tierDaemon(t, sliceDir)
	const claude = "cc-1a2b3c4d-0000-4000-8000-000000000001"
	if exit, stdout, stderr := runCLI(t, "confine-session-tier", "field", "low", "--owner", "me"); exit != 0 {
		t.Fatalf("set exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if exit, stdout, stderr := runCLI(t, "confine-session-tier", claude, "high", "--owner", "me"); exit != 0 {
		t.Fatalf("set cc exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if got := storedSessionTiers(t, paths); len(got) != 2 || got["field"] != "low" || got[claude] != "high" {
		t.Fatalf("store %v", got)
	}
	if got, want := listedSessionTiers(t), `[{"owner":"`+claude+`","tier":"high"},{"owner":"field","tier":"low"}]`; got != want {
		t.Fatalf("listed %s, want %s", got, want)
	}
	// Back to medium deletes the row.
	if exit, _, stderr := runCLI(t, "confine-session-tier", "field", "medium", "--owner", "me"); exit != 0 {
		t.Fatalf("remove exit=%d stderr=%q", exit, stderr)
	}
	if got := storedSessionTiers(t, paths); len(got) != 1 || got[claude] != "high" {
		t.Fatalf("store after remove %v", got)
	}
}

// verifies: AIRA-381 T17b (--list half), T17 render — a reply without
// session_tiers prints `session tiers: unevaluated`, never "none set"; an empty
// list prints `none set`; marks print with FULL owners, high first. On both
// paths of renderConfineListResponse. Mutations: nil rendered "none set"; the
// line dropped on the unevaluated early return; owners shortened.
func TestSessionTierListLineRendersEveryState(t *testing.T) {
	const claude = "cc-1a2b3c4d-0000-4000-8000-000000000001"
	marks := []runner.ConfineSessionTier{{Owner: claude, Tier: "low"}, {Owner: "field", Tier: "high"}, {Owner: "alpha", Tier: "low"}}
	empty := []runner.ConfineSessionTier{}
	for _, verdict := range []string{"pass", "unevaluated"} {
		for _, test := range []struct {
			tiers *[]runner.ConfineSessionTier
			want  string
		}{
			{nil, "session tiers: unevaluated (old daemon, ci-shim, or stored marks did not load: see the daemon journal)"},
			{&empty, "session tiers: none set"},
			{&marks, "session tiers: high field; low alpha, " + claude},
		} {
			listing := runner.ConfineListResult{Verdict: verdict, Scopes: []runner.ConfineRecord{}, SessionTiers: test.tiers}
			if verdict == "unevaluated" {
				listing.Reason = "slice unreadable"
			}
			data, _ := json.Marshal(listing)
			var stdout, stderr bytes.Buffer
			renderConfineListResponse(core.Response{OK: true, Code: "OK", RawData: data}, &stdout, &stderr)
			if !strings.Contains(stdout.String(), test.want+"\n") {
				t.Fatalf("verdict %s tiers %v: output %q does not carry %q", verdict, test.tiers, stdout.String(), test.want)
			}
		}
	}
}

// verifies: AIRA-381 T18 (client half), T19c — the CLI refuses bad values
// E_CONFINE_ARGUMENT_INVALID exit 2 before sending anything, refuses `--slice`
// and `--scope-dir`, and the request it sends has no slice. Mutations: the
// client validator skipped; `--slice` allowed; the slice insertion kept.
func TestSessionTierCLIRefusesBadValuesAndSendsNoSlice(t *testing.T) {
	var sent []daemon.RequestFrame
	d := &daemonDispatcher{}
	d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent = append(sent, frame)
		data, _ := json.Marshal(runner.ConfineSessionTierResult{Session: "field", Tier: "low", Previous: "medium"})
		return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
	}
	run := func(argv ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		exit := runWithInputDispatcher(argv, &stdout, &stderr, strings.NewReader(""), d)
		return exit, stdout.String() + stderr.String()
	}
	for _, argv := range [][]string{
		{"confine-session-tier", "unknown", "high"},
		{"confine-session-tier", "", "high"},
		{"confine-session-tier", strings.Repeat("a", 65), "high"},
		{"confine-session-tier", "bad owner", "high"},
		{"confine-session-tier", "field", "urgent"},
		{"confine-session-tier", "field"},
		{"confine-session-tier", "field", "low", "extra"},
	} {
		if exit, output := run(argv...); exit != 2 || !strings.Contains(output, "E_CONFINE_ARGUMENT_INVALID") {
			t.Fatalf("argv %q: exit %d output %q, want E_CONFINE_ARGUMENT_INVALID exit 2", argv, exit, output)
		}
	}
	for _, argv := range [][]string{
		{"confine-session-tier", "field", "low", "--slice", "aira.slice"},
		{"confine-session-tier", "field", "low", "--scope-dir", t.TempDir()},
	} {
		if exit, output := run(argv...); exit == 0 {
			t.Fatalf("argv %q accepted: %q", argv, output)
		}
	}
	if len(sent) != 0 {
		t.Fatalf("a refused command sent %d frames", len(sent))
	}
	if exit, output := run("confine-session-tier", "field", "low", "--owner", "me"); exit != 0 {
		t.Fatalf("valid set exit %d: %q", exit, output)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d frames, want 1", len(sent))
	}
	args := sent[0].Request.Args
	if _, has := args["slice"]; has || sent[0].Request.Verb != "confine-session-tier" || args["session"] != "field" || args["tier"] != "low" || args["owner"] != "me" || sent[0].Scope.Root != "" {
		t.Fatalf("sent %+v, want a project-less confine-session-tier with no slice", sent[0])
	}
}

// verifies: AIRA-381 T19d, T19e, T21 — `aira confine --help` lists the verb;
// it is client-routed, store-free and refuses --scope-dir; the descriptor has
// Include false and no MCPTool, so it is neither an MCP tool nor a Skill
// action, while the generated agent guide and help name it with "agents do not
// set tiers". Mutations: drop it from confineHelpVerbs, routing.go, scope_dir.go
// or the Include exclusion.
func TestSessionTierVerbSurfaces(t *testing.T) {
	exit, stdout, stderr := runCLI(t, "confine", "--help")
	if exit != 0 || !strings.Contains(stdout+stderr, "confine-session-tier") {
		t.Fatalf("confine --help exit=%d does not list the verb: %q %q", exit, stdout, stderr)
	}
	canonical, route := core.Classify("confine-session-tier", "")
	if canonical != "confine-session-tier" || route != core.RouteClient {
		t.Fatalf("classify=%q/%v, want client-routed", canonical, route)
	}
	if !core.StoreFreeCarved("confine-session-tier", nil) {
		t.Fatal("confine-session-tier must be store-free")
	}
	if verbAcceptsScopeDir("confine-session-tier") {
		t.Fatal("confine-session-tier must refuse a scope-dir override")
	}
	descriptors := core.New(nil).DispatchDescriptors()
	descriptor, found := descriptorNamed(descriptors, "confine-session-tier")
	if !found || descriptor.Include || descriptor.MCPTool != "" || descriptor.Safety != core.SafetyExecute {
		t.Fatalf("descriptor found=%v %+v, want Include false, no MCPTool, SafetyExecute", found, descriptor)
	}
	if !strings.Contains(descriptor.Summary, "agents do not set tiers") {
		t.Fatalf("summary %q does not say agents do not set tiers", descriptor.Summary)
	}
	for name := range newMCPServer(nil).byName {
		if strings.Contains(name, "session_tier") {
			t.Fatalf("MCP tool %s exposes the setter", name)
		}
	}
	artifacts, err := core.GenerateSkillArtifacts(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range artifacts.Actions {
		if action.Verb == "confine-session-tier" {
			t.Fatal("the setter leaked into the Skill actions")
		}
	}
	if guide := string(artifacts.Guide); !strings.Contains(guide, "confine-session-tier") || !strings.Contains(guide, "agents do not set tiers") {
		t.Fatal("the agent guide does not name confine-session-tier with \"agents do not set tiers\"")
	}
	if response, ok := verbHelpResponse("confine-session-tier"); !ok || !strings.Contains(string(tierJSON(t, response)), "agents do not set tiers") {
		t.Fatalf("generated help for the verb ok=%v does not carry the summary", ok)
	}
}

func tierJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// verifies: AIRA-381 T25 — the protocol version is unchanged, so a daemon
// predating the verb answers it with an unrelated code: the reply keeps the
// daemon's code and words and gains a hint naming `aira confine-session-tier`;
// the verb's own codes pass untouched. Mutations: the verb missing from the
// hint condition; the hint naming another verb.
func TestSessionTierOldDaemonReplyNamesTheLikelyCause(t *testing.T) {
	for _, test := range []struct {
		code string
		hint bool
	}{
		{"E_CONFIG_INVALID", true}, // measured: the live pre-381 daemon's answer (protocol.go)
		{"E_NOT_PROJECT", true},
		{"E_UNKNOWN_VERB", true},
		{"E_CONFINE_SESSION_TIERS_FULL", false},
		{"E_CONFINE_UNAVAILABLE", false},
		{"E_CONFINE_ARGUMENT_INVALID", false},
	} {
		t.Run(test.code, func(t *testing.T) {
			d := &daemonDispatcher{}
			d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
				return daemon.ResponseFrame{Code: test.code, Error: test.code + ": daemon words", Exit: codes.ExitForCode(test.code)}, nil
			}
			response := d.dispatchConfineManagementOnce(context.Background(),
				core.Request{Verb: "confine-session-tier", Args: map[string]any{"session": "field", "tier": "low", "owner": "me"}})
			if response.OK || response.Code != test.code || !strings.HasPrefix(response.Error, test.code+": daemon words") || response.Exit == 0 {
				t.Fatalf("response=%+v, want the daemon's own code, words and a non-zero exit", response)
			}
			if got := strings.Contains(response.Error, "may predate `aira confine-session-tier`"); got != test.hint {
				t.Fatalf("error=%q hint=%v, want %v", response.Error, got, test.hint)
			}
		})
	}
}

// tierTopState is an `aira top` model over a listing: two live rows (alpha
// owned by field, bravo with no owner in its id) and queued rows.
func tierTopState(t *testing.T, tiers *[]runner.ConfineSessionTier, queued ...runner.ConfineRecord) tuiState {
	t.Helper()
	listing := topQueuedListing(queued...)
	listing.Scopes[0].ScopeID = "CONFINE-alpha-101-aa@field"
	listing.Scopes[0].Owner = "field"
	listing.SessionTiers = tiers
	model, _ := topViewModel(topTick{}, listing)
	state := newTUIState(8)
	state.Active = viewTop
	state.ConfineOwner = "operator"
	panel := state.Panels[viewTop]
	panel.Status = panelReady
	panel.Model = model
	state.Panels[viewTop] = panel
	return state
}

// pickTier presses `t` on table row, picks tier and returns the confirmation.
func pickTier(t *testing.T, state tuiState, row int, tier string) (tuiState, *paletteConfirm) {
	t.Helper()
	descriptors := core.New(nil).DispatchDescriptors()
	state = onTopHighlight(state, row)
	state, commands := onTUIKey(state, 't', descriptors)
	if len(commands) != 0 {
		t.Fatalf("t dispatched before any confirmation: %v", commands)
	}
	if state.InlineAction == nil || state.InlineAction.Stage != inlineStagePicker {
		return state, nil
	}
	if got := strings.Join(state.InlineAction.Options, ","); got != "high,medium,low" {
		t.Fatalf("picker offers %s, want high,medium,low", got)
	}
	state, commands = onInlineActionPick(state, tier, descriptors)
	if len(commands) != 0 {
		t.Fatalf("the pick dispatched before the confirmation: %v", commands)
	}
	return state, state.PaletteConfirm
}

// verifies: AIRA-381 T19b — through the `aira top` model and DispatchPalette to
// a REAL daemon: `t` then a pick on a running row and on a queued row each
// sets the row's FULL owner parsed from its scope id (a queued row whose
// SESSION cell shows a tmux name included), the sent request has no slice and
// the caller as owner; an ownerless row sends nothing; `f` on a running row and
// `e` on a queued row are still refused E_TUI_INLINE_ROW_KIND. Mutations: send
// the SESSION cell text; drop AnyRow (one row kind refused); widen another
// action's gate; DispatchPalette not admitting the verb.
func TestTopTierKeySetsTheRowOwnerThroughARealDaemon(t *testing.T) {
	paths := tierDaemon(t, t.TempDir())
	const claude = "cc-1a2b3c4d-0000-4000-8000-000000000001"
	queued := topQueuedTestRecord("CONFINE-zulu-201-cc@"+claude, "zulu", 4*gib)
	queued.Owner = claude
	tmux := "editor-window"
	queued.Pane = &tmux
	state := tierTopState(t, &[]runner.ConfineSessionTier{}, queued)
	if cell := state.Panels[viewTop].Model.Rows[2].Cells[topSessionColumn]; strings.Contains(cell, claude) {
		t.Fatalf("test premise: the queued SESSION cell %q shows the full owner", cell)
	}
	dispatcher, err := newDaemonDispatcher(nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		row           int
		tier, session string
	}{
		{1, "high", "field"},
		{3, "low", claude},
	} {
		_, confirm := pickTier(t, state, test.row, test.tier)
		if confirm == nil || confirm.Verb != "confine-session-tier" || !paletteConfirmEnabled(confirm) {
			t.Fatalf("row %d: confirm %+v, want an enabled confine-session-tier confirmation", test.row, confirm)
		}
		args := confirm.Request.Args
		if _, has := args["slice"]; has || args["session"] != test.session || args["tier"] != test.tier || args["owner"] != "operator" {
			t.Fatalf("row %d: request %+v, want session %s tier %s owner operator and no slice", test.row, confirm.Request, test.session, test.tier)
		}
		if want := "Set session " + test.session + " to " + strings.ToUpper(test.tier) + "."; !strings.Contains(confirm.Summary, want) || !strings.Contains(confirm.Summary, "running jobs are not affected") {
			t.Fatalf("row %d: page %q", test.row, confirm.Summary)
		}
		result := executePaletteRequest(context.Background(), dispatcher, daemon.WorktreeScope{}, confirm.Request)
		if result.Outcome != paletteApplied || !strings.Contains(result.Text, "session "+test.session+": medium -> "+test.tier) {
			t.Fatalf("row %d: outcome %v text %q", test.row, result.Outcome, result.Text)
		}
	}
	if got := storedSessionTiers(t, paths); len(got) != 2 || got["field"] != "high" || got[claude] != "low" {
		t.Fatalf("store %v", got)
	}
	// An ownerless row (bravo) sends nothing and says why.
	refused, confirm := pickTier(t, state, 2, "low")
	if confirm != nil || refused.InlineAction != nil || !strings.Contains(refused.InlineError, "no session owner to mark") {
		t.Fatalf("ownerless row: confirm %+v error %q", confirm, refused.InlineError)
	}
	// The other actions' gates are unchanged.
	descriptors := core.New(nil).DispatchDescriptors()
	for _, test := range []struct {
		row int
		key rune
	}{{1, 'f'}, {3, 'e'}} {
		after, commands := onTUIKey(onTopHighlight(state, test.row), test.key, descriptors)
		if after.PaletteConfirm != nil || len(commands) != 0 || !strings.HasPrefix(after.InlineError, "E_TUI_INLINE_ROW_KIND:") {
			t.Fatalf("%c on row %d: confirm %+v error %q, want the row-kind refusal", test.key, test.row, after.PaletteConfirm, after.InlineError)
		}
	}
}

// verifies: AIRA-381 T22, T24 (row half), §3.6 — the SESSION cell is marked by
// the SESSION's tier from the owner in the scope id: a marked owner's live and
// queued rows show " (high)"/" (low)"; a queued `--priority high` row of a
// medium session is unmarked in SESSION and `high` in LIVE; a row whose id has
// no owner tail is unmarked even when its record claims a marked owner.
// Mutations: the suffix from job priority; keyed on record.Owner.
func TestTopSessionCellShowsTheSessionTier(t *testing.T) {
	high := topQueuedTestRecord("CONFINE-yank-202-dd@session-q", "yank", gib)
	high.Priority = runner.ConfinePriorityHigh
	low := topQueuedTestRecord("CONFINE-xray-203-ee@lowsession", "xray", gib)
	low.Owner = "lowsession"
	claimed := topQueuedTestRecord("CONFINE-whis-204-ff", "whis", gib)
	claimed.Owner = "field"
	state := tierTopState(t, &[]runner.ConfineSessionTier{{Owner: "field", Tier: "high"}, {Owner: "lowsession", Tier: "low"}}, high, low, claimed)
	rows := state.Panels[viewTop].Model.Rows
	for _, test := range []struct {
		row        int
		suffix     string
		liveHasLow bool
	}{{0, " (high)", false}, {1, "", false}, {2, "", false}, {3, " (low)", false}, {4, "", false}} {
		cell := rows[test.row].Cells[topSessionColumn]
		if test.suffix != "" && !strings.HasSuffix(cell, test.suffix) {
			t.Fatalf("row %d (%s) SESSION %q, want suffix %q", test.row, rows[test.row].ID, cell, test.suffix)
		}
		if test.suffix == "" && (strings.Contains(cell, "(high)") || strings.Contains(cell, "(low)")) {
			t.Fatalf("row %d (%s) SESSION %q, want unmarked", test.row, rows[test.row].ID, cell)
		}
	}
	if live := rows[2].Cells[3]; !strings.HasPrefix(live, "high") {
		t.Fatalf("--priority high row LIVE %q, want high", live)
	}
	// No marks: nothing is suffixed anywhere.
	plain := tierTopState(t, &[]runner.ConfineSessionTier{}, high, low, claimed)
	for index, row := range plain.Panels[viewTop].Model.Rows {
		if cell := row.Cells[topSessionColumn]; strings.Contains(cell, "(high)") || strings.Contains(cell, "(low)") {
			t.Fatalf("no marks: row %d SESSION %q", index, cell)
		}
	}
}

// verifies: AIRA-381 T17b (`aira top` half, nit 2) — a reply without
// session_tiers shows `tiers unevaluated` ON SCREEN at 80 and 120 columns,
// first in the one-line footer, on both footer paths (with a reserve frame and
// on the no-reserve early return); a loaded table adds nothing. Mutations: the
// clause appended after the granted clause (clipped at 80); dropped on the
// early return; shown for an empty table.
func TestTopFooterShowsTiersUnevaluatedOnScreen(t *testing.T) {
	frame := &runner.ConfineSliceReserve{
		GrantedEstablished: true, GrantedBytes: 45500 << 20, CeilingBytes: 45600 << 20,
		Jobs: 27, ScopeJobs: 27, Queued: 4, FreezePhase: "idle",
	}
	for _, test := range []struct {
		name    string
		reserve *runner.ConfineSliceReserve
		tiers   *[]runner.ConfineSessionTier
		want    bool
	}{
		{"reserve, unevaluated", frame, nil, true},
		{"no reserve, unevaluated", nil, nil, true},
		{"reserve, loaded", frame, &[]runner.ConfineSessionTier{}, false},
		{"no reserve, loaded", nil, &[]runner.ConfineSessionTier{{Owner: "field", Tier: "high"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			listing := topTestListing(test.reserve, topTestRecord("CONFINE-alpha-101-aa", "alpha", 4*gib, 3*gib))
			listing.SessionTiers = test.tiers
			raw, err := json.Marshal(listing)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			screen := tcell.NewSimulationScreen("UTF-8")
			runtime := newTopRuntime(ctx, topListingDispatcher{raw: raw}, nil, nil, nil, screen)
			done := make(chan error, 1)
			go func() { done <- runtime.run() }()
			ready := "granted 45500M"
			if test.reserve == nil {
				ready = "slice reserve unevaluated"
			}
			var seen string
			eventuallyOnUI(t, runtime, testdeadline.Wait(10*time.Second), func() bool {
				seen = simulationText(screen)
				return strings.Contains(seen, ready)
			}, "the footer never rendered %q", ready)
			for _, width := range []int{80, 120} {
				onUIDraw(t, runtime, func() bool { screen.SetSize(width, 30); return true })
				drawn := onUI(t, runtime, func() string { return simulationText(screen) })
				if got := strings.Contains(drawn, "tiers unevaluated"); got != test.want {
					t.Fatalf("width %d: tiers unevaluated shown=%v, want %v:\n%s", width, got, test.want, drawn)
				}
			}
			screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
			select {
			case <-done:
			case <-testdeadline.After(5 * time.Second):
				t.Fatal("quit timed out")
			}
		})
	}
}
