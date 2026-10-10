package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// failDispatcher fails the test if any verb handler is reached: --help must be
// answered before dispatch.
type failDispatcher struct{ t *testing.T }

func (d failDispatcher) Dispatch(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	d.t.Helper()
	d.t.Fatalf("help reached the dispatcher: %#v", request)
	return core.Response{}
}

// helpVerbs runs argv with stdout a buffer (so JSON), requires exit 0, and
// returns the sorted verb names in the help envelope.
func helpVerbs(t *testing.T, argv []string) []string {
	t.Helper()
	original := runInstaller
	runInstaller = func([]string, io.Writer) error {
		t.Fatalf("install handler reached for %q", argv)
		return nil
	}
	defer func() { runInstaller = original }()
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher(argv, &stdout, &stderr, failDispatcher{t})
	if exit != 0 {
		t.Fatalf("%q exit=%d stdout=%q stderr=%q", argv, exit, stdout.String(), stderr.String())
	}
	var envelope struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
		Data []struct {
			Verb  string `json:"verb"`
			Usage string `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK {
		t.Fatalf("%q: not a help envelope: err=%v stdout=%q", argv, err, stdout.String())
	}
	var verbs []string
	for _, entry := range envelope.Data {
		if entry.Usage == "" {
			t.Fatalf("%q: entry %q has no usage", argv, entry.Verb)
		}
		verbs = append(verbs, entry.Verb)
	}
	sort.Strings(verbs)
	return verbs
}

// verifies: AIRA-211. `<verb> --help` and `-h` are answered with that verb's
// own help-table entries, never a parser refusal and never another verb's text.
func TestVerbHelpIsAnsweredForEveryVerbBeforeDispatch(t *testing.T) {
	cases := map[string][]string{
		"rant":         {"rant"},
		"run-log":      {"run-log"},
		"run":          {"run"},
		"time":         {"time"},
		"git":          {"git"},
		"install":      {"install"},
		"drain":        {"drain"},
		"list":         {"list"},
		"create":       {"create"},
		"ls":           {"list"},
		"new":          {"create"},
		"get":          {"show"},
		"confine":      {"confine", "confine-budget", "confine-dump", "confine-kill", "confine-list", "confine-status"},
		"worktree":     {"worktree-audit", "worktree-register"},
		"confine-list": {"confine-list"},
	}
	for verb, want := range cases {
		for _, flag := range []string{"--help", "-h"} {
			got := helpVerbs(t, []string{verb, flag})
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s: help verbs = %v, want %v", verb, flag, got, want)
			}
		}
	}
	// Global options before/after the verb do not hide the request.
	if got := helpVerbs(t, []string{"--json", "list", "--help"}); !reflect.DeepEqual(got, []string{"list"}) {
		t.Fatalf("--json list --help = %v", got)
	}
	if got := helpVerbs(t, []string{"list", "--scope-dir", t.TempDir(), "--help"}); got != nil && !reflect.DeepEqual(got, []string{"list"}) {
		t.Fatalf("list --scope-dir X --help = %v", got)
	}
}

// verifies: AIRA-211. Only the token right after the verb is read, so a child's
// own --help after `--` is never intercepted.
func TestPreParseHelpReadsOnlyTheTokenAfterTheVerb(t *testing.T) {
	for _, argv := range [][]string{
		{"confine", "--", "printf", "%s", "--help"},
		{"confine", "--", "-h"},
		{"run", "--", "tool", "--help"},
		{"time", "--", "tool", "-h"},
		{"create", "T", "--body", "-h"},
		{"gate", "add", "--argv", "-h"},
		{"list"},
		{},
	} {
		if verb, _, ok := preParseHelpVerb(argv); ok {
			t.Fatalf("%q was read as a help request for %q", argv, verb)
		}
	}
	if verb, _, ok := preParseHelpVerb([]string{"--json", "LS", "-h"}); !ok || verb != "ls" {
		t.Fatalf("--json LS -h => %q,%v", verb, ok)
	}
	// parseArgs keeps --help in the child's argv.
	positional, options, err := parseArgs("confine", []string{"--", "printf", "%s", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positional, []string{"printf", "%s", "--help"}) || options["help"] != "" {
		t.Fatalf("positional=%q options=%v", positional, options)
	}
}

// verifies: AIRA-211. The generic parser reads --help where an option NAME is
// expected and -h where a POSITIONAL is expected, and never in a value position.
func TestGenericParserHelpSentinelNeverReadsValues(t *testing.T) {
	for _, tc := range []struct {
		verb string
		argv []string
		want string
	}{
		{"list", []string{"--by", "status", "--help"}, "list"},
		{"create", []string{"Fix", "it", "-h"}, "create"},
		{"count", []string{"-h"}, "count"},
	} {
		if _, _, err := parseArgs(tc.verb, tc.argv); err == nil || !isHelpRequest(err) {
			t.Fatalf("parseArgs(%s, %q) err=%v, want the help sentinel", tc.verb, tc.argv, err)
		}
	}
	_, options, err := parseArgs("create", []string{"T", "--body", "-h"})
	if err != nil || options["body"] != "-h" {
		t.Fatalf("create --body -h: options=%v err=%v", options, err)
	}
	_, options, err = parseArgs("gate", []string{"add", "--argv", "df", "--argv", "-h"})
	if err != nil || !strings.HasSuffix(options["argv"], "-h") || !strings.Contains(options["argv"], "df") {
		t.Fatalf("gate --argv -h: options=%v err=%v", options, err)
	}
	// Through Run: no dispatch, the verb's own entry is shown, but the exit is 2
	// (the request carried other arguments, so it is a refusal, not a clean help).
	if got := helpShownButRefused(t, []string{"list", "--by", "status", "--help"}); !reflect.DeepEqual(got, []string{"list"}) {
		t.Fatalf("list --by status --help = %v", got)
	}
	if got := helpShownButRefused(t, []string{"create", "Fix", "it", "-h"}); !reflect.DeepEqual(got, []string{"create"}) {
		t.Fatalf("create Fix it -h = %v", got)
	}
}

// helpShownButRefused runs argv, requires exit 2 with the verb's help envelope on
// stdout and an E_SELECTOR_INVALID line on stderr, and returns the help verbs.
func helpShownButRefused(t *testing.T, argv []string) []string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher(argv, &stdout, &stderr, failDispatcher{t})
	if exit != 2 {
		t.Fatalf("%q exit=%d, want 2 (stdout=%q stderr=%q)", argv, exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "E_SELECTOR_INVALID") {
		t.Fatalf("%q: stderr has no refusal: %q", argv, stderr.String())
	}
	var envelope struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
		Data []struct {
			Verb string `json:"verb"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("%q: stdout is not the help envelope: %q (%v)", argv, stdout.String(), err)
	}
	// The machine contract on a pipe: a refused command is never ok:true.
	if envelope.OK || envelope.Code != "E_SELECTOR_INVALID" {
		t.Fatalf("%q: stdout envelope says ok=%v code=%q for a refused command (exit 2): %q", argv, envelope.OK, envelope.Code, stdout.String())
	}
	var verbs []string
	for _, entry := range envelope.Data {
		verbs = append(verbs, entry.Verb)
	}
	sort.Strings(verbs)
	return verbs
}

// verifies: AIRA-211. A `-h` / `--help` that is NOT the bare request must never
// turn a malformed command into an exit-0 no-op: master refused these (exit 2),
// and a script's `aira gate add G --argv df -h || die` relies on that. Help is
// still shown, nothing is dispatched, the exit is 2. A bare `<verb> -h` stays 0.
func TestHelpAmongOtherArgumentsIsARefusalNotASuccess(t *testing.T) {
	for _, argv := range [][]string{
		{"gate", "add", "G", "--argv", "df", "-h"},
		{"gate", "add", "G", "--argv", "df", "--help"},
		{"mv", "AIRA-1", "-h"},
		{"claim", "AIRA-1", "-h"},
		{"link", "A", "B", "-h"},
		{"list", "--by", "status", "--help"},
	} {
		if got := helpShownButRefused(t, argv); len(got) == 0 {
			t.Fatalf("%q: refusal printed no help", argv)
		}
	}
	// Bare requests, and the worktree subverb spelling, are clean help (exit 0).
	for _, argv := range [][]string{
		{"gate", "-h"}, {"mv", "--help"}, {"worktree", "register", "--help"}, {"worktree", "audit", "-h"},
	} {
		if got := helpVerbs(t, argv); len(got) == 0 {
			t.Fatalf("%q: no help", argv)
		}
	}
	// The value-position carve-out is untouched: -h is data after its option.
	_, options, err := parseArgs("gate", []string{"add", "G", "--argv", "df", "--argv", "-h"})
	if err != nil || !strings.Contains(options["argv"], "-h") {
		t.Fatalf("gate --argv -h: options=%v err=%v", options, err)
	}
}

// verifies: AIRA-211. The pre-parse answers exit-0 help ONLY when the help token
// is the sole token after the verb. A help token in position 2 followed by other
// arguments is a command master refused (exit 2), and must never become an exit-0
// no-op: `aira confine --help -- true` would otherwise exit 0 having run nothing
// and printed no ran=no line. Mutation: `len(args) < 2` in preParseHelpVerb -> RED.
func TestHelpTokenFollowedByOtherArgumentsIsNotCleanHelp(t *testing.T) {
	for _, argv := range [][]string{
		{"confine", "--help", "--", "true"},
		{"confine", "-h", "--", "true"},
		{"gate", "-h", "add", "G", "--argv", "df"},
		{"mv", "-h", "AIRA-1", "done"},
		{"create", "--help", "Fix", "it"},
		{"--json", "list", "--help", "x"},
	} {
		if verb, _, ok := preParseHelpVerb(argv); ok {
			t.Fatalf("%q was read as a bare help request for %q", argv, verb)
		}
		var stdout, stderr bytes.Buffer
		exit := RunWithDispatcher(argv, &stdout, &stderr, failDispatcher{t})
		if exit == 0 {
			t.Fatalf("%q exited 0 (stdout=%q stderr=%q): a command with a stray help token is a refusal", argv, stdout.String(), stderr.String())
		}
	}
}

// verifies: AIRA-211. `confine --help -- true` is a launch that never ran, so its
// refusal carries the ran=no line like every other confine launch refusal.
func TestConfineHelpAmongLaunchArgumentsSaysNeverRan(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher([]string{"confine", "--help", "--", "true"}, &stdout, &stderr, failDispatcher{t})
	if exit == 0 || !strings.Contains(stderr.String(), runner.ConfineNeverRanFacet) {
		t.Fatalf("exit=%d stderr=%q, want a refusal with the ran=no line", exit, stderr.String())
	}
}

// verifies: AIRA-211. `aira help <verb>` for a verb that EXISTS but has no help
// entry must not claim the verb is unknown: E_UNKNOWN_VERB is reserved for a
// verb that does not exist (a script may use it to test existence).
func TestHelpForAVerbWithoutAnEntryIsNotUnknown(t *testing.T) {
	for _, verb := range []string{"top", "board", "tui", "skill", "mcp", "daemon", "version", "watch", "worker-admit", "confine-report", "drain-hold"} {
		var stdout, stderr bytes.Buffer
		exit := RunWithDispatcher([]string{"help", verb}, &stdout, &stderr, failDispatcher{t})
		if exit != 2 || strings.Contains(stdout.String(), "E_UNKNOWN_VERB") || !strings.Contains(stdout.String(), "no help entry for") {
			t.Fatalf("help %s exit=%d stdout=%q", verb, exit, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if exit := RunWithDispatcher([]string{"help", "nosuchverb"}, &stdout, &stderr, failDispatcher{t}); exit != 2 || !strings.Contains(stdout.String(), "E_UNKNOWN_VERB") {
		t.Fatalf("help nosuchverb exit=%d stdout=%q", exit, stdout.String())
	}
}

// verifies: AIRA-211. `aira help <verb>` prints exactly that verb's entries.
func TestHelpVerbSubcommand(t *testing.T) {
	if got := helpVerbs(t, []string{"help", "list"}); !reflect.DeepEqual(got, []string{"list"}) {
		t.Fatalf("help list = %v", got)
	}
	if got := helpVerbs(t, []string{"-h"}); len(got) < 40 {
		t.Fatalf("aira -h should list every verb, got %d", len(got))
	}
	var stdout, stderr bytes.Buffer
	if exit := RunWithDispatcher([]string{"help", "nosuch"}, &stdout, &stderr, failDispatcher{t}); exit != 2 || !strings.Contains(stdout.String(), `E_UNKNOWN_VERB: no verb named \"nosuch\"`) {
		t.Fatalf("help nosuch exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exit := RunWithDispatcher([]string{"help", "list", "ready"}, &stdout, &stderr, failDispatcher{t}); exit != 2 || !strings.Contains(stdout.String(), "E_SELECTOR_INVALID") {
		t.Fatalf("help list ready exit=%d stdout=%q", exit, stdout.String())
	}
}

// verifies: AIRA-211. A refused option gets a did-you-mean from the verb's own
// allowed set, and nothing when nothing is close; the text is sorted/stable.
func TestGenericRefusalDidYouMean(t *testing.T) {
	_, _, err := parseArgs("rant", []string{"--tags", "x"})
	if err == nil || err.Error() != "E_SELECTOR_INVALID: option --tags is not valid for rant (did you mean --tag?)" {
		t.Fatalf("rant --tags: %v", err)
	}
	_, _, err = parseArgs("rant", []string{"--zzzzzz", "x"})
	if err == nil || err.Error() != "E_SELECTOR_INVALID: option --zzzzzz is not valid for rant" {
		t.Fatalf("rant --zzzzzz: %v", err)
	}
	_, _, err = parseArgs("list", []string{"--by"})
	if err == nil || err.Error() != "E_SELECTOR_INVALID: option --by requires a value" {
		t.Fatalf("list --by: %v", err)
	}
	// A multi-match suggestion is exact, in sorted vocabulary order, and only
	// names options the verb accepts.
	const wantGate = "E_SELECTOR_INVALID: option --mutation is not valid for gate (did you mean one of --mutation-content, --mutation-expected-result, --mutation-file, --mutation-kind, --mutation-occurrence, --mutation-pkgdir, --mutation-seed, --mutation-test, --mutation-testname?)"
	for run := 0; run < 30; run++ {
		_, _, err = parseArgs("gate", []string{"--mutation", "x"})
		if err == nil || err.Error() != wantGate {
			t.Fatalf("run %d: gate --mutation: %v", run, err)
		}
	}
	_, _, err = parseArgs("spend", []string{"--cost", "1"})
	if err == nil || !strings.Contains(err.Error(), "(did you mean --cost-usd?)") {
		t.Fatalf("spend --cost: %v", err)
	}
	for _, name := range suggestedNames(err.Error()) {
		if !spendAllowed[name] {
			t.Fatalf("suggested --%s which spend refuses: %v", name, err)
		}
	}
	// run-* verbs keep their own code.
	if _, _, err := parseArgs("run-log", []string{"--bogus", "x"}); err == nil || !strings.HasPrefix(err.Error(), "E_RUN_ARGUMENT_INVALID:") {
		t.Fatalf("run-log: %v", err)
	}
}

var spendAllowed = map[string]bool{"provider": true, "model": true, "source": true, "ticket": true, "phase": true, "at": true, "session": true, "agent": true, "turn-id": true, "total": true, "cost-usd": true, "usage-file": true, "bucket": true, "reasoning-subset": true, "resolve-ticket": true, "by": true}

// suggestedNames extracts the --names from a "(did you mean ...)" clause.
func suggestedNames(message string) []string {
	start := strings.Index(message, "(did you mean")
	if start < 0 {
		return nil
	}
	var names []string
	for _, field := range strings.FieldsFunc(message[start:], func(r rune) bool { return r == ' ' || r == ',' || r == '?' || r == ')' }) {
		if strings.HasPrefix(field, "--") {
			names = append(names, strings.TrimPrefix(field, "--"))
		}
	}
	return names
}

// verifies: AIRA-211. The TERMINAL branch of the help-among-other-arguments
// refusal: help listed on stdout, the refusal line on stderr, exit 2. Mutation:
// replace that branch's tail with `return renderVerbHelp(response, false, stdout,
// stderr)` (the help-and-succeed shape) -> exit 0 -> RED.
func TestHelpAmongOtherArgumentsOnATerminalIsARefusal(t *testing.T) {
	for _, argv := range [][]string{
		{"gate", "add", "G", "--argv", "df", "-h"},
		{"mv", "AIRA-1", "--help"},
	} {
		var stdout terminalBuffer
		var stderr bytes.Buffer
		exit := RunWithDispatcher(argv, &stdout, &stderr, failDispatcher{t})
		if exit != 2 {
			t.Fatalf("%q terminal exit=%d, want 2 (stdout=%q stderr=%q)", argv, exit, stdout.String(), stderr.String())
		}
		if strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") || !strings.Contains(stdout.String(), argv[0]) {
			t.Fatalf("%q: terminal stdout is not the human help listing: %q", argv, stdout.String())
		}
		if !strings.Contains(stderr.String(), "E_SELECTOR_INVALID") {
			t.Fatalf("%q: terminal stderr has no refusal: %q", argv, stderr.String())
		}
	}
}

// verifies: AIRA-211 / spec 3.3. run-* verbs keep E_RUN_ARGUMENT_INVALID for the
// help-among-arguments refusal, on a pipe (envelope + stderr) and on a terminal.
// Mutation: hard-code E_SELECTOR_INVALID in the refusal -> RED.
func TestHelpAmongOtherArgumentsKeepsRunCodeForRunVerbs(t *testing.T) {
	argv := []string{"run-log", "RUN-1", "--help"}
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher(argv, &stdout, &stderr, failDispatcher{t})
	if exit != 2 || !strings.Contains(stdout.String(), `"code":"E_RUN_ARGUMENT_INVALID"`) ||
		!strings.Contains(stderr.String(), "E_RUN_ARGUMENT_INVALID") || strings.Contains(stdout.String()+stderr.String(), "E_SELECTOR_INVALID") {
		t.Fatalf("%q piped: exit=%d stdout=%q stderr=%q", argv, exit, stdout.String(), stderr.String())
	}
	var terminal terminalBuffer
	stderr.Reset()
	exit = RunWithDispatcher(argv, &terminal, &stderr, failDispatcher{t})
	if exit != 2 || !strings.Contains(stderr.String(), "E_RUN_ARGUMENT_INVALID") || strings.Contains(stderr.String(), "E_SELECTOR_INVALID") {
		t.Fatalf("%q terminal: exit=%d stderr=%q", argv, exit, stderr.String())
	}
}

// verifies: AIRA-211. The "put the flag directly after its option" hint is only
// true where a flag is accepted as an option value (gate --argv / --env-allow);
// everywhere else it is omitted rather than offered falsely.
// Mutation: print the hint for every verb -> RED.
func TestHelpAmongOtherArgumentsHintOnlyWhereItHolds(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := RunWithDispatcher([]string{"gate", "add", "G", "--argv", "df", "--help"}, &stdout, &stderr, failDispatcher{t}); exit != 2 ||
		!strings.Contains(stderr.String(), "--argv --help") {
		t.Fatalf("gate: exit=%d stderr=%q, want the --argv hint", exit, stderr.String())
	}
	for _, argv := range [][]string{{"mv", "AIRA-1", "-h"}, {"list", "--by", "status", "--help"}, {"claim", "AIRA-1", "-h"}} {
		stdout.Reset()
		stderr.Reset()
		if exit := RunWithDispatcher(argv, &stdout, &stderr, failDispatcher{t}); exit != 2 {
			t.Fatalf("%q: exit=%d", argv, exit)
		}
		if strings.Contains(stderr.String(), "--argv") || strings.Contains(stderr.String(), "option VALUE") {
			t.Fatalf("%q: false hint: %q", argv, stderr.String())
		}
		if !strings.Contains(stderr.String(), "nothing ran") {
			t.Fatalf("%q: refusal text lost: %q", argv, stderr.String())
		}
	}
}

// verifies: AIRA-82/211. The help/version verbs resolve no scope, so --scope-dir
// is refused for them; `-h` is spelled the same as `--help`.
// Mutation: drop "-h" from verbAcceptsScopeDir's refused list -> RED.
func TestScopeDirIsRefusedForDashH(t *testing.T) {
	for _, verb := range []string{"help", "--help", "-h"} {
		if verbAcceptsScopeDir(verb) {
			t.Fatalf("verbAcceptsScopeDir(%q) = true", verb)
		}
	}
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher([]string{"--scope-dir", t.TempDir(), "-h"}, &stdout, &stderr, failDispatcher{t})
	if exit != 2 || !strings.Contains(stdout.String(), "E_SELECTOR_INVALID") {
		t.Fatalf("--scope-dir D -h: exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

// verifies: AIRA-207. A malformed --scope-dir on a confine launch gets the
// ran=no line even when --json follows the option: the launch check must read the
// ORIGINAL argv (removeScopeDir returns it unchanged), not the --json-stripped
// copy in which `--scope-dir confine` looks like an option and its value.
// Mutation: pass the stripped args to renderConfineRefusal on that path -> RED.
func TestMalformedScopeDirConfineLaunchWithJSONSaysNeverRan(t *testing.T) {
	defer mustNotLaunch(t)()
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher([]string{"--scope-dir", "--json", "confine", "--", "true"}, &stdout, &stderr, failDispatcher{t})
	if exit == 0 || !strings.Contains(stderr.String(), runner.ConfineNeverRanFacet) {
		t.Fatalf("exit=%d stdout=%q stderr=%q, want a refusal with the ran=no line", exit, stdout.String(), stderr.String())
	}
}
