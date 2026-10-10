package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

// AIRA-211. One `--help` for every verb that has a help-table entry.
//
// The request is recognised in exactly two places, both token-position based so
// a value (`--body -h`, `--argv -h`) or a launched child's argv (`confine -- cmd
// --help`) is never mistaken for it:
//
//   - preParseHelpVerb: the token right after the verb is `--help` or `-h`;
//   - the generic parser (parseArgs): `--help` where an option NAME is expected,
//     or `-h` where a POSITIONAL is expected, returns a helpRequestError.
//
// Both are rendered by renderVerbHelp from the same dispatch-table entries
// `aira help` prints.

// helpRequestError is parseArgs' sentinel for "the user asked for help".
type helpRequestError struct{ flag string }

func (e *helpRequestError) Error() string { return "help requested (" + e.flag + ")" }

func asHelpRequest(err error) (*helpRequestError, bool) {
	var request *helpRequestError
	if errors.As(err, &request) {
		return request, true
	}
	return nil, false
}

func isHelpRequest(err error) bool {
	_, ok := asHelpRequest(err)
	return ok
}

// helpRefusalCode is the stable code of a refused help token: run-* verbs keep
// their own family code (spec 3.3), every other verb E_SELECTOR_INVALID.
func helpRefusalCode(verb string) string {
	if strings.HasPrefix(verb, "run-") {
		return "E_RUN_ARGUMENT_INVALID"
	}
	return "E_SELECTOR_INVALID"
}

// refusal is what a verb with no help-table entry reports instead.
func (e *helpRequestError) refusal(verb string) error {
	code := helpRefusalCode(verb)
	if e.flag == "-h" {
		return fmt.Errorf("%s: unexpected argument -h for %s", code, verb)
	}
	return fmt.Errorf("%s: option --help is not valid for %s", code, verb)
}

// otherArgumentsRefusal is the stderr line printed when the help token arrived
// with other arguments: the help is shown, nothing ran, and the exit is 2. The
// "pass it as an option value" hint is offered only where it is true: gate's
// --argv / --env-allow take a following `--`-prefixed token as their value.
func (e *helpRequestError) otherArgumentsRefusal(verb string) string {
	text := fmt.Sprintf("%s: %s was given with other arguments, so %s was refused and nothing ran (`aira help %s` shows its usage", helpRefusalCode(verb), e.flag, verb, verb)
	if verb == "gate" {
		text += fmt.Sprintf("; to pass %s as an option VALUE put it directly after its option, e.g. --argv %s", e.flag, e.flag)
	}
	return text + ")"
}

// verbsWithoutHelpEntry are the dispatched verbs that have no help-table entry
// (the spec's list). `aira help <one of these>` must not say the verb is unknown.
var verbsWithoutHelpEntry = map[string]bool{
	"skill": true, "top": true, "board": true, "tui": true, "mcp": true, "daemon": true,
	"version": true, "watch": true, "worker-admit": true, "confine-report": true, "drain-hold": true,
}

func verbExistsWithoutHelpEntry(verb string) bool {
	return verbsWithoutHelpEntry[strings.ToLower(verb)]
}

// preParseHelpVerb reports whether argv is `<verb> --help|-h` and returns the
// lower-cased verb and whether --json was given. The global --scope-dir and
// --json are stripped first (with the existing strippers) to find the verb; a
// malformed --scope-dir is not an error here, the request just falls through so
// the existing refusal still fires and help is never printed for a request that
// also carries a malformed option.
func preParseHelpVerb(argv []string) (verb string, jsonOutput bool, ok bool) {
	args, _, err := removeScopeDir(argv)
	if err != nil {
		return "", false, false
	}
	args, jsonOutput = removeJSON(args)
	// The help token must be the ONLY thing after the verb. With anything else
	// alongside it the request is a command carrying a stray help token, which
	// master refused (exit 2): it falls through to the parsers, never to exit-0
	// help (`confine --help -- true` would otherwise exit 0 without running).
	if len(args) != 2 || (args[1] != "--help" && args[1] != "-h") {
		return "", false, false
	}
	return strings.ToLower(args[0]), jsonOutput, true
}

// verbHelpResponse returns the help response restricted to one verb's entries:
// the confine family for `confine`, the two subverbs for `worktree`, the
// canonical verb for an alias (new, ls, get), otherwise the verb's own entry.
// found is false when the verb has no help-table entry.
func verbHelpResponse(verb string) (core.Response, bool) {
	response := core.New(nil).Do(context.Background(), core.Request{Verb: "help"})
	all, ok := response.Data.([]map[string]string)
	if !ok {
		return response, false
	}
	verb = strings.ToLower(verb)
	var wanted []string
	switch verb {
	case "confine":
		wanted = confineHelpVerbs
	case "worktree":
		wanted = []string{"worktree-register", "worktree-audit"}
	default:
		wanted = []string{core.CanonicalVerb(verb)}
	}
	filtered := make([]map[string]string, 0, len(wanted))
	for _, entry := range all {
		for _, name := range wanted {
			if entry["verb"] == name {
				filtered = append(filtered, entry)
				break
			}
		}
	}
	if len(filtered) == 0 {
		return response, false
	}
	response.Data = filtered
	return response, true
}

// renderVerbHelp is the one help renderer: the JSON envelope when piped (like
// `aira help`), the git-help-style listing on a terminal. Always exit 0.
func renderVerbHelp(response core.Response, renderJSON bool, stdout, stderr io.Writer) int {
	if !renderJSON && response.OK {
		return renderHelp(response, stdout, stderr)
	}
	return render(response, renderJSON, stdout, stderr)
}

// sortedKeys returns a map's keys in sorted order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// confineManagementFlags are the `confine` options that make a request a
// management request (list/kill/status/budget/dump) rather than a launch.
var confineManagementFlags = map[string]bool{"--list": true, "--kill": true, "--status": true, "--budget": true, "--dump": true}

// isConfineLaunch reports whether argv (verb first) is a request to LAUNCH a
// confine job: the verb is exactly `confine` (the hyphenated confine-* verbs are
// management, never launches) and no management flag, with or without =value,
// appears before the first `--`. It reads tokens only, so it works before, and
// when, option parsing fails. AIRA-207 keys the stderr `ran=no` line on it, so a
// management request never gets one (nothing was being launched) and a `--list`
// after `--` (the child's argument) does not hide a launch.
func isConfineLaunch(argv []string) bool {
	// Skip the GLOBAL options that may precede the verb (`aira --scope-dir X
	// confine ...`). A refused --scope-dir returns argv unchanged (removeScopeDir),
	// so the verb is not at argv[0] on exactly the path that refuses it; the skip
	// tolerates a malformed one (empty value, repeated, missing value).
	start := 0
	for start < len(argv) {
		name, _, hasInline := strings.Cut(argv[start], "=")
		if argv[start] == "--json" {
			start++
		} else if name == scopeDirFlag {
			start++
			// Any next token that is not option-like is the (possibly empty or
			// blank) value: an empty token can never be the verb, so
			// `--scope-dir "$UNSET" confine -- ...` is still a confine launch.
			if !hasInline && start < len(argv) && !strings.HasPrefix(argv[start], "--") {
				start++
			}
		} else {
			break
		}
	}
	if start >= len(argv) || argv[start] != "confine" {
		return false
	}
	for _, arg := range argv[start+1:] {
		if arg == "--" {
			break
		}
		name, _, _ := strings.Cut(arg, "=")
		if confineManagementFlags[name] {
			return false
		}
	}
	return true
}

// refuseConfineLaunch reports a confine launch refused before it reached the
// launcher: the same two stderr lines, in the same order, an in-flight refusal
// prints (the fixed `ran=no` line, then the error), so a script has ONE string
// to key on for "this confine launch never ran", whatever layer refused it.
func refuseConfineLaunch(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintln(stderr, runner.FormatConfineNeverRan(runner.ConfineStatus{}, err))
	_, _ = fmt.Fprintln(stderr, err)
	code := store.ErrorCode(err)
	if code == "E_INTERNAL" {
		code = "E_CONFINE_ARGUMENT_INVALID"
	}
	return codes.ExitForCode(code)
}

// renderConfineRefusal renders a refusal exactly as render does and, when the
// request is a confine launch (isConfineLaunch), also puts the `ran=no` line on
// stderr -- and the error text itself when render would not have (JSON mode
// writes only stdout). stdout is unchanged.
func renderConfineRefusal(args []string, response core.Response, renderJSON bool, stdout, stderr io.Writer) int {
	if isConfineLaunch(args) {
		_, _ = fmt.Fprintln(stderr, runner.FormatConfineNeverRan(runner.ConfineStatus{}, errors.New(response.Error)))
		if renderJSON {
			_, _ = fmt.Fprintln(stderr, response.Error)
		}
	}
	return render(response, renderJSON, stdout, stderr)
}

// renderConfineManagement renders a confine MANAGEMENT result (--list, --kill,
// --budget, --dump and their hyphenated spellings). The rendering follows the
// pipe default (JSON on stdout, AIRA-214), but a FAILURE also puts its text on
// stderr -- where these forms printed it before the pipe default. A peer script
// that captures stderr to a file and tests only the exit status
// (fastest-ee in-container-gate.sh: `aira confine --dump F 2>>err || log ...`)
// would otherwise see an empty error file and the failure only in its stdout log.
func renderConfineManagement(response core.Response, renderJSON bool, stdout, stderr io.Writer) int {
	if renderJSON && !response.OK && response.Error != "" {
		_, _ = fmt.Fprintln(stderr, response.Error)
	}
	return render(response, renderJSON, stdout, stderr)
}
