package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aira/internal/core"
)

// The explicit per-call scope override (AIRA-82).
//
// Every AIRA face resolves the project/worktree scope of a call from a
// DIRECTORY — app.Discover(ctx, dir) — and both faces defaulted that directory
// to the face process's own working directory ("."). For the CLI that default
// is right: the process cwd IS the caller's worktree. For a long-lived MCP
// server it is wrong. An MCP request carries no directory of its own, so every
// call is scoped against wherever the server happened to be launched, and the
// git provenance stamped from that scope (stampGitContext) names that directory
// too. RANT-18 is the observed instance: a rant filed from a feature worktree
// recorded worktree_path=<the shared root>, head_ref=refs/heads/master.
//
// Nothing was fabricated — the daemon recorded faithfully what the face handed
// it. What was missing is a way for the caller to say which directory the call
// belongs to. That is this override, with identical semantics on both faces:
//
//	CLI: aira --scope-dir <dir> <verb> ...
//	MCP: {"scope_dir": "<dir>", ...}
//
// It overrides the INPUT to discovery, never its result: project identity,
// worktree identity and git context are still read from git and .aira/config
// under that directory, so the override cannot name a project that is not
// really there. An unusable directory is refused, never quietly replaced by the
// process cwd — a silent fallback would reintroduce exactly the misattribution
// this fixes.
//
// Scope of the override: it selects the project/worktree, not the resolution of
// relative FILE arguments. `aira import --file x.md` still reads x.md relative
// to the calling process's own cwd (prepareImportContent). On the CLI that is
// unambiguous and stays allowed; on MCP the two bases would differ silently, so
// refuseAmbiguousImportPath refuses that combination outright.
//
// Relative values are accepted on the CLI and refused on MCP, deliberately. The
// CLI process's cwd IS the caller's own directory, so "../sibling" means what
// the caller thinks it means. An MCP caller has no idea what the server's cwd
// is — that is the whole defect — so a relative override there would resolve
// against the very directory the caller is trying to escape, handing back the
// same wrong scope under a new name.
const (
	scopeDirFlag        = "--scope-dir"
	scopeDirArgument    = "scope_dir"
	scopeDirDescription = "Absolute path of the git worktree this call is scoped to (default: this MCP server process's own directory, which is usually NOT the caller's worktree). Set it whenever the call belongs to a specific worktree."
)

// resolveScopeDir turns a face-supplied override into the directory discovery
// runs in. Empty keeps the process working directory. A non-empty value is
// checked here rather than left to app.Discover, whose fixed message ("current
// directory is not a git worktree") would name the wrong directory — the same
// class of confidently-wrong reporting this ticket is about.
func resolveScopeDir(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ".", nil
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return "", fmt.Errorf("E_NOT_PROJECT: scope directory %q is unavailable: %v", trimmed, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("E_NOT_PROJECT: scope directory %q is not a directory", trimmed)
	}
	return trimmed, nil
}

// removeScopeDir strips the global --scope-dir option out of argv before verb
// parsing, the way removeJSON strips --json: the per-verb option tables are
// closed sets and would otherwise refuse it on every verb.
//
// The scan stops at the first bare "--". Everything after that delimiter
// belongs to a launched target (run/time/confine) or to git's own refspec list,
// so a target's own --scope-dir must reach the target untouched. Stopping there
// unconditionally costs nothing for the verbs that have no delimiter: their
// parser already refuses a bare "--" as an unknown option.
//
// On error the original argv is returned unchanged so the caller can still
// discover --json and render the refusal in the right shape.
func removeScopeDir(argv []string) ([]string, string, error) {
	result := make([]string, 0, len(argv))
	value := ""
	seen := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			result = append(result, argv[i:]...)
			break
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		if name != scopeDirFlag {
			result = append(result, arg)
			continue
		}
		if seen {
			return argv, "", fmt.Errorf("E_SELECTOR_INVALID: option %s may occur once", scopeDirFlag)
		}
		seen = true
		if hasInline {
			if strings.TrimSpace(inline) == "" {
				return argv, "", fmt.Errorf("E_SELECTOR_INVALID: option %s requires a value", scopeDirFlag)
			}
			value = inline
			continue
		}
		// An option-like next token is a missing value, not a directory named
		// "--json" — the same rule parseArgs already applies to every other
		// option. Without it, `aira --scope-dir --json ls` would eat the --json.
		// A directory that genuinely starts with "--" is still reachable through
		// the --scope-dir=<value> form.
		if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") || strings.TrimSpace(argv[i+1]) == "" {
			return argv, "", fmt.Errorf("E_SELECTOR_INVALID: option %s requires a value", scopeDirFlag)
		}
		i++
		value = argv[i]
	}
	return result, value, nil
}

// refuseAmbiguousImportPath guards the one ambiguity this override newly creates
// on the MCP face. `import`'s `file` argument is read by prepareImportContent
// relative to the FACE process's directory, while the override sends the
// imported CONTENT to a different project. With both in play, an MCP `import`
// naming a relative `notes.md` reads the server's own notes.md and files it
// against another worktree — a silently wrong file, which is this ticket's exact
// failure class. Refuse it and say why.
//
// Deliberately MCP-only: on the CLI the process cwd is the caller's own, so
// "this file here, into that project" is a legitimate and unambiguous request.
func refuseAmbiguousImportPath(request core.Request, scopeDirOverride string) error {
	if strings.TrimSpace(scopeDirOverride) == "" || !isImportRequest(request) {
		return nil
	}
	path, _ := request.Args["file"].(string)
	if strings.TrimSpace(path) == "" || filepath.IsAbs(path) {
		return nil
	}
	return fmt.Errorf("E_IMPORT_INVALID: import file %q is relative to this face's own directory, not to %s %q; pass an absolute path", path, scopeDirArgument, scopeDirOverride)
}

// verbAcceptsScopeDir reports whether a CLI verb resolves a project/worktree
// scope at all. The machine-local, project-less verbs do not, so they refuse the
// override instead of accepting and discarding it.
func verbAcceptsScopeDir(verb string) bool {
	switch verb {
	case "confine", "confine-reserve", "confine-list", "confine-kill",
		"aitest-bootstrap", "worker-admit",
		"help", "--help":
		return false
	}
	return true
}

// toolAcceptsScopeDir is the MCP-side twin of verbAcceptsScopeDir. These tools
// are dispatched with an empty WorktreeScope by design (mcp_project.go), so they
// neither declare nor accept the override.
//
// eject is deliberately asymmetric with the CLI, which DOES accept the override
// for it: the CLI defaults eject's project selector from a discovered project
// (main.go), so a directory is meaningful there, whereas the MCP face performs
// no discovery for eject at all and requires an explicit selector.
func toolAcceptsScopeDir(tool string) bool {
	switch tool {
	case "aira_eject", "aira_confine_list", "aira_confine_kill":
		return false
	}
	return true
}
