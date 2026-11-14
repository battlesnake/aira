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
# The second argument is the admission queue priority (AIRA-340): `high` only
# for a SHORT step (the pre-commit quick checks: `make fmt-check vet build`
# measured 6-10 s on the confine trailer's wall=, warm build cache; re-measure
# with `aira confine -- make -C <worktree> fmt-check vet build`), so a
# commit does not wait behind slow jobs on a busy box; `normal` for anything
# long (`make ci`, the store tests). Version skew degrades, never refuses
# commits: an installed CLIENT that predates `--priority` is detected from its
# own help and the step runs at normal priority with a one-line note; a new
# client whose admission DAEMON predates the field (installed, not yet
# restarted) gets the daemon's refusal of the field, re-asks once at normal
# priority and prints a `confine: notice:` line. Either way the commit runs;
# only its queue position is normal until both are current.
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

	local -a confine_opts=()
	if [ "$priority" = high ]; then
		# Captured, then matched: a `| grep -q` pipeline under pipefail can fail
		# when grep exits early and the help writer takes SIGPIPE.
		local help
		help="$(aira confine --help 2>/dev/null)" || help=""
		case "$help" in
		*--priority*) confine_opts=(--priority high) ;;
		*) printf '%s: the installed aira has no `confine --priority`; running at normal queue priority.\n' "$hook_name" >&2 ;;
		esac
	fi

	aira_hook_scrub_git_env
	exec aira confine "${confine_opts[@]}" -- make -C "$root" "$@"
}
