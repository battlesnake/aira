package gitcontext

import (
	"os"
	"strings"
)

// ScrubbedEnvironment returns the process environment with every GIT_* variable
// removed. Every place in AIRA that shells out to git must use it (AIRA-93).
//
// WHY. AIRA always names the repository explicitly — `git -C <dir> …` — but an
// inherited GIT_DIR, GIT_WORK_TREE or GIT_INDEX_FILE OVERRIDES `-C`. A process
// that inherited one (from a git hook, or a shell that exported it) therefore
// operates on a DIFFERENT repository than the one it asked about, silently:
//
//   - project discovery resolves the wrong project and worktree id, so the
//     common-dir receipts and journal keyed by them are written under the wrong
//     project. Two such receipts are in this repository's own shared journal,
//     written from /tmp test working directories, and they are why
//     `aira reconcile --rebuild` fails E_JOURNAL_CORRUPT here;
//   - the gate canary's `git init` / `git add -A` in a scratch directory would
//     stage into the inherited index instead of the scratch repository's;
//   - test-report provenance would name another repository's HEAD.
//
// It removes ALL GIT_* rather than an allowlist of the three that bite hardest.
// git keeps adding environment-driven overrides, and an allowlist silently
// regrows the hole the next time it ships one.
//
// ACCEPTED TRADEOFF, stated rather than implied: a few GIT_* variables are
// legitimate configuration rather than an override — GIT_CONFIG_GLOBAL (which
// can carry a `safe.directory` entry), GIT_CEILING_DIRECTORIES and
// GIT_DISCOVERY_ACROSS_FILESYSTEM among them. Dropping them makes discovery more
// permissive, not wrong, in every case except a caller who put `safe.directory`
// ONLY in a GIT_CONFIG_GLOBAL file — that caller gets git's dubious-ownership
// refusal, which is a loud, diagnosable failure rather than a silently wrong
// answer. That is the direction AIRA prefers, and it is why the blanket form is
// chosen over an allowlist that would have to be curated forever.
//
// verifies: AIRA-93
func ScrubbedEnvironment() []string {
	return scrubGitVariables(os.Environ())
}

// ScrubbedEnvironmentFrom is ScrubbedEnvironment for a caller that is already
// building a custom environment (e.g. one that pins LC_ALL/LANG).
func ScrubbedEnvironmentFrom(env []string) []string {
	return scrubGitVariables(env)
}

func scrubGitVariables(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_") {
			continue
		}
		result = append(result, entry)
	}
	return result
}
