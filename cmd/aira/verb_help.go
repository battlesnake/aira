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

// refusal is what a verb with no help-table entry reports instead.
func (e *helpRequestError) refusal(verb string) error {
	if e.flag == "-h" {
		return fmt.Errorf("E_SELECTOR_INVALID: unexpected argument -h for %s", verb)
	}
	return fmt.Errorf("E_SELECTOR_INVALID: option --help is not valid for %s", verb)
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
	if len(args) < 2 || (args[1] != "--help" && args[1] != "-h") {
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
	if len(argv) == 0 || argv[0] != "confine" {
		return false
	}
	for _, arg := range argv[1:] {
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
