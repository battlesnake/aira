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
	# AIRA-378: the full `make ci` is pinned at 2G. Its peak-rss was measured at
	# 1.31-1.44 GB (the confine trailer, three runs on 2026-11-22), above the 1 GiB
	# auto reserve a command with no peak history gets, so an unpinned cold run was
	# OOM-killed (exit 137) and each retry waited in the admission queue again (waits
	# of 465-1515 s that day). Re-measure with
	# `aira confine -- make -C <worktree> ci` and read the trailer's peak-rss=.
	# Narrowed scopes (test-store, test-docs) and the quick pre-commit checks stay
	# auto-sized.
	local -a reserve=()
	if [ "${1:-}" = ci ]; then
		reserve=(--memory-reserve 2G)
	fi
	exec aira confine ${reserve[@]+"${reserve[@]}"} -- make -C "$root" "$@"
}

# ---------------------------------------------------------------------------
# AIRA-355: recorded `make ci` passes, keyed on the tree hash.
#
# `make ci` (through .githooks/ci-pass) writes one marker per tree that passed
# every step from a CLEAN worktree, under the git COMMON dir, so every worktree
# of this repository on this machine shares them: the same tree on the same
# machine gives the same result. The pre-push hook skips its full `make ci` only
# when EVERY pushed branch's tree has one. Nothing in a marker is trusted beyond
# its existence as a regular file and its first line being the version line
# below; its mtime is shown to the user and never decides anything. A marker is
# written to a temporary name and renamed into place as the last action, so a
# hard kill (a confined job has no graceful shutdown) leaves no marker.
AIRA_CI_PASS_VERSION='aira-ci-pass v1'

# aira_ci_pass_dir <root> prints the absolute marker directory.
aira_ci_pass_dir() {
	local common
	common="$(git -C "$1" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" || return 1
	case "$common" in /?*) ;; *) return 1 ;; esac
	[ -d "$common" ] || return 1
	printf '%s/aira-ci-pass\n' "$common"
}

# aira_ci_pass_is_hash <s> succeeds only for a full lowercase object id (SHA-1
# or SHA-256), so a marker name can never be a path or an option.
aira_ci_pass_is_hash() {
	case "$1" in "" | *[!0-9a-f]*) return 1 ;; esac
	[ "${#1}" -eq 40 ] || [ "${#1}" -eq 64 ]
}

# aira_ci_pass_marker <root> <tree> prints the marker's path and succeeds only
# when a valid marker for exactly that tree exists.
aira_ci_pass_marker() {
	local dir file line
	aira_ci_pass_is_hash "$2" || return 1
	dir="$(aira_ci_pass_dir "$1")" || return 1
	file="$dir/$2"
	if [ ! -f "$file" ] || [ -L "$file" ]; then return 1; fi
	IFS= read -r line <"$file" || return 1
	[ "$line" = "$AIRA_CI_PASS_VERSION" ] || return 1
	printf '%s\n' "$file"
}

# aira_ci_pass_clean_tree <root> prints HEAD's tree and succeeds only when <root>
# is the top of its worktree and that worktree is clean: no staged, unstaged or
# untracked (non-ignored) change. --untracked-files=all overrides a user's
# status.showUntrackedFiles=no, which would otherwise hide untracked files.
aira_ci_pass_clean_tree() {
	local root="$1" top status tree listing hidden rc
	top="$(git -C "$root" rev-parse --show-toplevel 2>/dev/null)" || return 1
	[ -n "$top" ] || return 1
	[ "$(cd -- "$root" && pwd -P)" = "$(cd -- "$top" && pwd -P)" ] || return 1
	status="$(git -C "$root" --no-optional-locks status --porcelain --untracked-files=all --ignore-submodules=none 2>/dev/null)" || return 1
	[ -z "$status" ] || return 1
	# assume-unchanged / skip-worktree files hide edits from `status`: any index
	# entry not in the plain cached state ('H') refuses the clean verdict.
	# Read the whole listing first: `ls-files -v | grep -q -v` makes grep exit at the
	# first hidden entry, ls-files then dies of SIGPIPE on a large index, and under
	# the hooks' pipefail that status reads as "no hidden entry" (a false clean).
	listing="$(git -C "$root" --no-optional-locks ls-files -v 2>/dev/null)" || return 1
	rc=0
	hidden="$(grep -v '^H ' <<<"$listing")" || rc=$?
	[ "$rc" -le 1 ] || return 1
	[ -z "$hidden" ] || return 1
	tree="$(git -C "$root" rev-parse --verify --quiet 'HEAD^{tree}')" || return 1
	aira_ci_pass_is_hash "$tree" || return 1
	printf '%s\n' "$tree"
}

# aira_ci_pass_record <root> <tree> writes the marker for <tree>, then prunes:
# markers (and stale temporary files) older than 14 days go, and only the
# newest 200 markers are kept. A pruning failure never fails the record.
aira_ci_pass_record() {
	local root="$1" tree="$2" dir tmp
	aira_ci_pass_is_hash "$tree" || return 1
	dir="$(aira_ci_pass_dir "$root")" || return 1
	mkdir -p -- "$dir" || return 1
	tmp="$(mktemp "$dir/.tmp.XXXXXX")" || return 1
	if ! printf '%s\n' "$AIRA_CI_PASS_VERSION" >"$tmp" || ! mv -f -- "$tmp" "$dir/$tree"; then
		rm -f -- "$tmp"
		return 1
	fi
	aira_ci_pass_prune "$dir" || true
}

aira_ci_pass_prune() {
	local dir="$1" name kept=0
	find "$dir" -maxdepth 1 -type f -mmin +20160 -delete 2>/dev/null || true
	# Marker names are bare hex ids, so `ls` output is unambiguous; only they
	# count toward the 200, and the count cap leaves anything else alone.
	ls -1t -- "$dir" 2>/dev/null | while IFS= read -r name; do
		aira_ci_pass_is_hash "$name" || continue
		kept=$((kept + 1))
		if [ "$kept" -gt 200 ]; then rm -f -- "$dir/$name"; fi
	done
}
