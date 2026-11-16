# Shared helpers for AIRA's git hooks. Sourced, never executed.
#
# Two properties every AIRA hook needs, both learned the hard way:
#
#   AIRA-76 — `core.hooksPath` is a single absolute path shared by the primary
#   checkout and every linked worktree, so a hook must never infer the tree it
#   is checking from its own location: `dirname "${BASH_SOURCE[0]}"/..` always
#   names the primary checkout, whichever worktree is committing. A commit in
#   one worktree then failed on an unrelated session's unformatted file, which
#   is why every commit in this repository needed `--no-verify`.
#
#   AIRA-46 — git exports `GIT_DIR`/`GIT_INDEX_FILE` into a hook's environment.
#   Anything the hook runs that shells out to git inherits them and operates on
#   the real repository even when it passes `git -C <its own temp dir>`. That
#   corrupted the shared repo once already. Hooks therefore hand the build a
#   scrubbed environment.
#
# And per CLAUDE.md the heavy targets run under `aira confine --`, so a hook's
# build cannot OOM the desktop.

# aira_hook_worktree_root prints the absolute root of the worktree that is
# actually running this hook. Git runs hooks from the top of that worktree and
# exports GIT_DIR pointing at its admin directory; both agree, and both are
# resolved before any scrubbing so the answer never depends on the cwd alone.
aira_hook_worktree_root() {
	local root
	root="$(git rev-parse --show-toplevel 2>/dev/null)" || return 1
	[ -n "$root" ] || return 1
	printf '%s\n' "$root"
}

# aira_hook_scrub_git_env removes every GIT_* variable git exported into this
# hook, so the build below sees the same environment an ordinary shell run
# would (AIRA-46).
aira_hook_scrub_git_env() {
	local var
	for var in ${!GIT_@}; do
		unset "$var" || true
	done
}

# aira_hook_run_make runs the named make targets against the committing
# worktree, confined, with a clean environment. It never returns.
#
# The second argument is the admission queue priority the hook asks for
# (AIRA-340): `high` only for a SHORT step (the pre-commit quick checks, `make
# fmt-check vet build`: the confine trailer's wall= measured about 9 s with a
# warm build cache, and about 40 s on the first run in a new worktree on a
# loaded box, load average about 65; re-measure with `aira confine -- make -C
# <worktree> fmt-check vet build`). Both are under a minute, so `high` stays
# defensible: a commit does not wait behind slow jobs on a busy box. `normal`
# for anything long (`make ci`, the store tests).
#
# AIRA-348: `high` is asked for through AIRA_CONFINE_PRIORITY, and only when the
# caller has not set it, so the caller has the last word either way:
# `AIRA_CONFINE_PRIORITY=normal git commit` runs the quick checks at normal
# priority, and `AIRA_CONFINE_PRIORITY=high git push` runs `make ci` at high (an
# explicit choice; keep it rare). `aira confine` validates the value (anything
# but high/normal refuses the step, so a typo is loud) and removes the variable
# from the job's environment, so nothing inside the step inherits it. Version
# skew degrades, never refuses commits: an installed CLIENT that predates the
# variable ignores it and the step queues at normal priority WITHOUT a note (the
# old `confine --help` probe that printed one is gone); a new client whose
# admission DAEMON predates priority re-asks once at normal priority and prints
# a `confine: notice:` line. Either way the commit runs.
aira_hook_run_make() {
	local hook_name="$1"
	local priority="$2"
	shift 2

	local root
	if ! root="$(aira_hook_worktree_root)"; then
		printf '%s: cannot resolve the worktree root (git rev-parse --show-toplevel failed).\n' "$hook_name" >&2
		printf '%s: refusing to guess which checkout to build. Re-run from a git worktree, or use --no-verify.\n' "$hook_name" >&2
		return 1
	fi

	if ! command -v aira >/dev/null 2>&1; then
		printf '%s: `aira` is not on PATH, and CLAUDE.md requires heavy commands to run under `aira confine --`.\n' "$hook_name" >&2
		printf '%s: install aira, or bypass this hook with --no-verify (and confine the build yourself).\n' "$hook_name" >&2
		return 1
	fi

	aira_hook_scrub_git_env
	if [ "$priority" = high ]; then
		export AIRA_CONFINE_PRIORITY="${AIRA_CONFINE_PRIORITY:-high}"
	fi
	exec aira confine -- make -C "$root" "$@"
}
