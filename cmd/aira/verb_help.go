package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"aira/internal/core"
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
