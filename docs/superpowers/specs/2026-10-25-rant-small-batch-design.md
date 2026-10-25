# AIRA-315 rant-triage small batch: aitest collection, eject diagnostic, pylib nested confine, reserve-wait flake, docs-only pre-push, confine --list PEAK

Status: PLAN v1 (2026-10-25). Light path (batch of small independent fixes, one review and
test cycle; no plan gate). Ticket: AIRA-315 (milestone v0.40), from the 2026-10-25 rant
triage of RANT-41/43/44/45/46/47. Base: `origin/master` `ff03d8aa`. Every touch point below
was opened at that commit; line numbers are as of it.

**Correctness-sensitive item: item 5, the pre-push fast path.** Skipping a gate is a
false-pass hazard by construction, so it is fail-closed and gets the adversarial build review
(both directions) described in section 6. The other five items are a hook precondition, an
error string, a test-harness simplification, two test constants and one table column.

## 1. The real problem, in plain terms

| # | Rant | What goes wrong today | Still true at `ff03d8aa`? |
|---|---|---|---|
| 1 | RANT-41 | With `--aitest-workers` set, `aitest`'s `pytest_runtestloop` (`internal/pylib/aitest/__init__.py:242`) replaces pytest's own loop but copies none of its two preconditions. `pytest --collect-only` RUNS the whole suite through the pool; a module that fails to import does not stop the run (pytest prints `Interrupted: 1 error during collection`), aitest runs everything else. | Yes. Reproduced by the investigator (pytester probe); pytest 9.0.3's own `pytest_runtestloop` read here: it raises `Interrupted` when `session.testsfailed and not continue_on_collection_errors`, then returns `True` under `collectonly`. |
| 1c | (found in planning) | With `--continue-on-collection-errors`, aitest overwrites `session.testsfailed = failed + error + unevaluated` (`__init__.py:311`), discarding the collection-error count pytest had already put there (`Session.pytest_collectreport` is the same counter). A run with a broken module and otherwise green tests exits **0**. | Yes, **measured** 2026-10-25 (probe under `~/tmp/aira315-probe`, now removed): aitest `ret=0`, plain pytest on the same tree `ret=1`. A false pass. |
| 2 | RANT-45 | `aira eject` on a project where some (not all) registered worktree roots are gone says only `E_EJECT_UNVERIFIED: only some registered worktree roots are available` (`internal/daemon/eject.go:189`, in `verifyEjectDurability`). It names neither the project nor which roots are missing, so the operator cannot tell a forgotten scratch project from a live one with one removed checkout. `--force` cannot get past it (the check runs before the force test at `eject.go:57`). | Yes. |
| 3 | RANT-46 | Two pylib e2e harnesses build the aira binary with `exec.Command("aira", "confine", "--", "go", "build", ...)` (`internal/pylib/pytest_aitest_e2e_test.go:337` in `newRealDaemonAndCgroupTestHarness`; `internal/pylib/restart_merge_gate_worker_e2e_test.go:164` in `newRestartGateHarness`). Each nested confine queues for its own shared-slice admission with no time limit, inside pylib's 20-minute `go test` timeout (`Makefile` `test:`). On a busy box the local gate fails whatever the diff. | Yes. `internal/daemon/worker_admit_cli_granted_linux_test.go:56` (`buildAiraBinary`) already builds with plain `go build`. |
| 4 | RANT-43 | `TestConfineReserveGrantOutlivesTheDeclaredBound` (`internal/daemon/confine_reserve_wait_bound_linux_test.go:354`) passes `--max-wait 2s` to a real `aira confine-reserve` subprocess. That 2 s is the whole window for a real subprocess to be granted by the in-process daemon; a loaded box misses it and the test fails "no grant line". The dwell is tied to it (`dwell = 3 * bound`), which is why it was kept small. | Yes, as code; flake rate is reported, not measured. |
| 5 | RANT-44 | `.githooks/pre-push` runs `aira confine -- make ci` (fmt-check, vet, build, the whole `go test ./...`, claude-mod-check) for every push, including a push that changes only design specs or ticket files. On this shared box that is a long queued run for a change no Go test can see. | Yes. `.githooks/pre-push` is one line: `aira_hook_run_make pre-push ci`. |
| 6 | RANT-47 (2a only) | The human `aira confine --list` table shows RSS and RESERVE but not the scope's lifetime peak, so an over-pinned reservation (reserve far above anything the job ever used) is invisible there. `--json` already carries `peak_rss` (`internal/runner/confine_manage.go`, `ConfineRecord.PeakRSS`, AIRA-241; nil = unevaluated). | Yes. Header at `cmd/aira/main.go:3660`. |

## 2. Greenfield minimum, and the gap

"If we had to solve ONLY these problems from scratch":

| # | Greenfield minimum | This plan | Gap and its live justification |
|---|---|---|---|
| 1 | Return `None` from aitest's hook (hand back to pytest's own loop) under `--collect-only` or an un-continued collection error: 3 lines. | Same 3 lines. | none |
| 1c | Carry the collection-error count into the final `session.testsfailed`: 2 lines. | Same. | Not in the ticket text, but a measured false pass in the same function the ticket edits; the review rule says every confirmed counterexample is fixed and pinned. |
| 2 | Name the project and the missing / present roots in the existing error string. | Same, plus one remedy sentence (`run aira check in a present worktree`) and a test that proves the remedy works. | The remedy sentence is the only actionable part for the rant's reporter. It is included **only because a test executes it** (section 7, T2b); if that test cannot be made green the sentence is dropped, not softened. |
| 3 | Delete the three tokens `"aira", "confine", "--"` at the two call sites and fix the comments. | Same. | none (a once-per-package build cache is deferred, section 9) |
| 4 | Two constants: a wider admission window, and a dwell measured as `bound + margin` from the grant. | Same. | none |
| 5 | Either nothing (document `git push --no-verify` for docs pushes), or a docs/-only skip in the hook. | Fail-closed classifier in `.githooks/pre-push`: all paths under `docs/` → skip; all under `docs/` or `.aira/tickets/` → run only `go test ./internal/store` (new `make test-store` target); anything else, or any doubt → the existing `make ci`. | **"Nothing / --no-verify" rejected:** it makes skipping a per-push judgement by whoever pushes, and agents are told never to bypass hooks; the decision is mechanical, so the hook should make it. **The tickets tier** is required by the ticket and serves the observed case (ticket-only commits are frequent: `0c130929`, `02d31988`); it runs the one package that reads the repo's own tickets (`internal/store/repository_tickets_test.go`, `TestTheRepositorysOwnTicketFilesAreReadable`). **Narrowed from the ticket's `.aira/` to `.aira/tickets/`:** `.aira/config` is read by cwd discovery in other packages' tests (e.g. `cmd/aira/confine_summary_cli_test.go:66` runs `confine` from the package directory, and `resolveConfineOwner` → `app.Discover(".")` walks up to the repo's `.aira/config`), so a config change is not store-only. `.aira/findings/` likewise falls to full `make ci`. **The `test-store` Makefile target** lets the hook reuse `aira_hook_run_make` (root resolution, the aira-on-PATH refusal, GIT_* scrub, confinement) instead of growing a second exec path. |
| 6 | One column in the table using the existing `confineNum` (nil → `unevaluated`). | Same. | none |

What can be removed: nothing beyond the nested-confine wrappers and their justifying comments
(item 3). Item 1 deliberately hands back to pytest's loop rather than copying its checks.

## 3. Scope

In: the six items above plus 1c. Out: see section 9.

## 4. Invariants

- I1 (item 1). With `--aitest-workers` set: under `--collect-only` the pool never runs and
  pytest's own collect-only output and exit code stand; with a collection error and no
  `--continue-on-collection-errors` the pool never runs and pytest's own `Interrupted`
  (exit 2) stands; with the flag the pool runs. Behaviour without `--aitest-workers` is
  unchanged (the existing early `return None`).
- I1c. A collection error is never erased from the exit code: final
  `session.testsfailed = collection_errors + failed + error + unevaluated`, where
  `collection_errors` is `session.testsfailed` read at hook entry (nothing has run yet, so it
  is exactly the collect-report failure count).
- I2. `E_EJECT_UNVERIFIED` keeps its code and prefix (`lifecycleError` keys on it). The
  partial-roots message names the project id and slug, every missing root and every present
  root, in `target.Worktrees` order (deterministic: the query orders by `worktree_id`). A root
  whose stat fails with anything other than ENOENT keeps its existing distinct message
  (`cannot stat worktree ...`), never counted as missing.
- I3. No in-test `go build` in pylib is wrapped in a nested `aira confine`.
- I4. The reserve-wait test still fails if a granted reservation expires at `--max-wait`,
  whether that expiry is measured from the client's start or from the grant.
- I5 (item 5, the hard one). The hook skips `make ci` **only** when, for **every** ref line on
  stdin, all of the following are proven; on anything else it runs exactly what it runs today
  (`aira confine -- make -C <worktree> ci`):
  - the line has exactly four fields;
  - the remote ref is under `refs/heads/` (tags, notes, anything else → full);
  - the local object id is non-zero (a ref deletion → full) and `git cat-file -t` says
    `commit` (an annotated tag object → full);
  - `refs/remotes/origin/master` resolves to a commit, and `git merge-base <local> origin/master`
    succeeds (unrelated history → full);
  - `git diff-tree -r --no-renames --name-only <merge-base> <local>` succeeds and lists at
    least one path for this ref (empty → full);
  - every listed path begins with `docs/` (skip tier) or with `docs/` or `.aira/tickets/`
    (store tier). A quoted path (git quotes paths with control characters, `"` or `\`) begins
    with `"` and so falls to full.
  - at least one ref line was read (empty stdin → full).
  The classifier prints exactly `docs` or `tickets` and returns 0 only when proven; the caller
  treats a non-zero return, or any other output, as full.
- I6. A nil `PeakRSS` renders `unevaluated`, never `0`, never RSS standing in; a set peak
  renders unclamped (it may exceed RESERVE after a `--resize`, or for any lifetime peak).

## 5. Code touch points (opened at `ff03d8aa`)

1. `internal/pylib/aitest/__init__.py: pytest_runtestloop` (line 242). After
   `if workers_option is None: return None` (253):
   ```python
   opt = session.config.option
   if opt.collectonly or (session.testsfailed and not opt.continue_on_collection_errors):
       return None
   collection_errors = session.testsfailed
   ```
   and at line 311: `session.testsfailed = collection_errors + failed + error + unevaluated`.
   Returning `None` lets pytest's own `pytest_runtestloop` (firstresult hook) run: it raises
   `Interrupted` or returns `True` under collect-only. `pytest_sessionfinish` and
   `pytest_terminal_summary` read stash keys with defaults, so they stay silent when the pool
   never ran (already pinned by `test_terminal_summary_says_nothing_when_the_aitest_pool_never_ran`).
2. `internal/daemon/eject.go: (*Server).verifyEjectDurability` (line 167). Collect missing
   roots into a slice while looping (they are currently only counted), and replace line 189:
   ```go
   return nil, false, fmt.Errorf("E_EJECT_UNVERIFIED: project %s (%s): %d of %d registered worktree roots are missing: %s; present: %s. Run 'aira check' in a present worktree to retire the missing roots, then eject again",
       target.Slug, target.ProjectID, len(missingRoots), len(target.Worktrees), strings.Join(missingRoots, ", "), strings.Join(roots, ", "))
   ```
   Remedy verified by reading, to be pinned by T2b: `aira check` → daemon storeop `check`
   (`internal/daemon/storeops.go:268`) → `(*Store).Check` → `(*Store).Rebuild`
   (`internal/store/check.go:263`, unconditionally; also `:200` on an unmarked log) → for each
   registry/`git worktree` entry, `validGitRoot` (store.go:4268, ENOENT → `valid=false`, no
   error) → `markWorktreeActive(ctx, entry, false)` (store.go:2920/3245) →
   `ProjectRegistration` lists only `active=1` (lifecycle.go:118). Plain `aira reconcile`
   does **not** call Rebuild (core.go reconcile handler calls it only with `--rebuild`), so the
   message names `aira check`, the verb that always does.
3. `internal/pylib/pytest_aitest_e2e_test.go: newRealDaemonAndCgroupTestHarness` (line 337)
   and `internal/pylib/restart_merge_gate_worker_e2e_test.go: newRestartGateHarness`
   (line 164): `exec.Command("go", "build", "-o", binary, "aira/cmd/aira")`; replace the
   comments (330-336, 161-163) with one line saying the build runs inside the outer
   confine scope and is charged to it, as `cmd/aira/confine_nested.go`'s nested-launch
   warning recommends.
4. `internal/daemon/confine_reserve_wait_bound_linux_test.go:
   TestConfineReserveGrantOutlivesTheDeclaredBound` (354-366):
   `bound := testdeadline.Wait(10 * time.Second)` (10 s; 40 s under `-race`) and
   `dwell := bound + 2*time.Second`, with the comment rewritten: the client's ctx deadline
   (`internal/runner/confine_reserve_linux.go:94-98`, `confineReserveWithRunner`) starts
   before the grant, so an expiry tied to `--max-wait` fires at most `bound` after the grant;
   `bound + 2s` measured from the grant (the `select` already starts after `awaitGrant`) still
   catches it. They become `:=` variables (no longer constants); the `held_ms >= dwell/2`
   assertion is unchanged. The sibling `TestConfineReserveWaitEndsAtTheDeclaredBound` is not
   touched.
5. `.githooks/pre-push` (whole file) and `Makefile` (new target). The hook becomes:
   ```bash
   #!/usr/bin/env bash
   set -euo pipefail
   . "$(dirname -- "${BASH_SOURCE[0]}")/common.sh"

   # aira_prepush_scope: see the design spec, invariant I5. errexit does NOT apply
   # inside a function called from `$(...) ||`, so every fallible command carries
   # its own `|| return 1`.
   aira_prepush_scope() {
   	local root="$1" base_ref=refs/remotes/origin/master
   	local local_ref local_sha remote_ref remote_sha extra base changed path
   	local refs=0 tickets=0 paths
   	git -C "$root" rev-parse --verify --quiet "$base_ref^{commit}" >/dev/null || return 1
   	while IFS=' ' read -r local_ref local_sha remote_ref remote_sha extra; do
   		[ -n "$remote_sha" ] && [ -z "$extra" ] || return 1
   		case "$remote_ref" in refs/heads/?*) ;; *) return 1 ;; esac
   		case "$local_sha" in "" | *[!0-9a-f]*) return 1 ;; esac
   		case "$local_sha" in *[!0]*) ;; *) return 1 ;; esac
   		[ "$(git -C "$root" cat-file -t "$local_sha" 2>/dev/null)" = commit ] || return 1
   		base="$(git -C "$root" merge-base "$local_sha" "$base_ref")" || return 1
   		[ -n "$base" ] || return 1
   		changed="$(git -C "$root" diff-tree -r --no-renames --name-only "$base" "$local_sha")" || return 1
   		paths=0
   		while IFS= read -r path; do
   			case "$path" in
   			"") ;;
   			docs/?*) paths=$((paths + 1)) ;;
   			.aira/tickets/?*) paths=$((paths + 1)); tickets=$((tickets + 1)) ;;
   			*) return 1 ;;
   			esac
   		done <<<"$changed"
   		[ "$paths" -gt 0 ] || return 1
   		refs=$((refs + 1))
   	done
   	[ "$refs" -gt 0 ] || return 1
   	if [ "$tickets" -gt 0 ]; then echo tickets; else echo docs; fi
   }

   scope=full
   if root="$(aira_hook_worktree_root)"; then
   	scope="$(aira_prepush_scope "$root")" || scope=full
   fi
   case "$scope" in
   docs)
   	printf 'pre-push: every pushed path is under docs/ (against origin/master); make ci skipped (AIRA-315).\n' >&2
   	exit 0 ;;
   tickets)
   	printf 'pre-push: every pushed path is under docs/ or .aira/tickets/; running only the internal/store tests (AIRA-315).\n' >&2
   	aira_hook_run_make pre-push test-store ;;
   *)
   	aira_hook_run_make pre-push ci ;;
   esac
   ```
   (The builder may restructure; I5 and the tests in section 7 are the contract, the sketch is
   not.) Makefile: `test-store:` → `$(GO) test ./internal/store -count=1 -timeout 20m`, with a
   comment naming the hook as its caller and why `internal/store` (the repo-ticket parse
   test). `.githooks/common.sh` is not changed.
   - Why `merge-base` with `origin/master` and not the remote sha on the line: one base rule
     for every ref, no "remote branch already had unchecked code" hole (a docs commit on top
     of a code branch diffs the code too, so it runs full), no dependence on whether the
     remote sha exists locally. A stale `origin/master` only moves the merge-base further
     back, which widens the path set (safe direction); it can never narrow it.
   - Why `diff-tree` with `--no-renames`: porcelain `git diff --name-only` detects renames by
     default and lists only the destination path, so `git mv internal/x docs/x` would read as
     docs-only. `diff-tree` is plumbing (ignores `diff.renames`), and `--no-renames` is explicit
     belt and braces: a rename lists both sides.
   - Semantics note: today `make ci` tests the working tree, not the pushed commits. The skip
     judges the pushed commits; the store tier, like today's `make ci`, runs on the working
     tree. Neither tier is weaker than today on the pushed content.
6. `cmd/aira/main.go: renderConfineListResponse` (header line 3660, row `Fprintf` 3670-3674):
   header `... RSS\tPEAK\tAGE\tRESERVE\tCAP`, add `confineNum(record.PeakRSS)` after
   `confineNum(record.RSSBytes)`, and extend the AIRA-191 comment block with one sentence
   (PEAK is the scope's lifetime `memory.peak`, display-only, nil → unevaluated, never clamped
   to RESERVE). `liveCell` in `cmd/aira/confine_live_status_test.go` reads `fields[4]` (LIVE),
   which a column after RSS does not shift; no existing test pins the full header.

## 6. False-pass and false-fail analysis

| # | False pass (a broken state reported fine) | False fail (a fine state reported broken) |
|---|---|---|
| 1 | Removed: collect-only and collection errors no longer silently run the pool. Residual: none known; pytest's own loop decides both. | A consumer relying on aitest carrying on past a broken module now gets pytest's `Interrupted` (exit 2). That is pytest's documented behaviour; the escape is `--continue-on-collection-errors`, which still runs the pool (T1c pins it). |
| 1c | Removed: a collection error under `--continue-on-collection-errors` now exits 1, as pytest does. | None: `collection_errors` is read before the pool runs, so a replayed report is not double-counted (the replay's own increments happen after the read and are then overwritten by the recomputed total, as today). |
| 2 | Message only; the refusal itself is unchanged, so no new pass. The remedy sentence could become a false instruction if Rebuild stopped retiring missing roots: T2b executes it so that cannot drift silently. | None: same code, same conditions. |
| 3 | The build is still checked (`t.Fatalf` on failure). It now runs inside the outer scope, so it is still confined (the outer `go test` is confined per CLAUDE.md). | Removes a busy-box false fail (the unbounded nested admission wait). New exposure: the link step (a few hundred MB) now counts against the outer `make ci` scope's learned memory.max; history re-learns from an OOM, and the build was already serial. |
| 4 | Sensitivity kept: an expiry at `--max-wait` (from start or from grant) fires by `grant + bound`, before `dwell = bound + 2s` (T4 mutation). Residual (pre-existing, accepted in the test's own comment): a release at, say, ten minutes still passes. | Admission window 2 s → 10 s (×4 under race, plus `AIRA_TEST_DEADLINE_SCALE`): the measured failure mode ("no grant line") needs a 5× slower grant. Cost: about 9 s more runtime. |
| 5 | **The hazard.** Enumerated attacks and where each lands: mixed paths → full; rename from outside into `docs/` → both sides listed → full; delete of a non-docs file → that path listed → full; delete of a `docs/` file → skip (no test reads `docs/`, below); a ref deletion → full; a tag push (lightweight or annotated) → full; several refs in one push, any one non-docs → full (every ref must pass); a docs commit on top of an unpushed code commit → the merge-base diff includes the code → full; stale `origin/master` → wider diff → full or still-correct; `origin/master` missing → full; unrelated history (orphan branch) → merge-base fails → full; empty stdin or a ref with no changed paths → full; a path with quoting → full; look-alike prefixes (`docsx/`, `Docs/`, `internal/docs/`, a file named `docs`) → full; `.aira/config` or `.aira/findings/` → full; `REQUIREMENTS.md` and every other `*.md` outside `docs/` → full (REQUIREMENTS.md is read by `internal/store/import_requirements_test.go:390`). **Why `docs/` is safe to skip** (verified by grep at `ff03d8aa`): no test reads the repo's `docs/`; the repo-walking tests filter to `*.go` (`internal/codes/produced_test.go: forEachSourceFile`, `internal/runner/protocol_pin_linux_test.go`) or walk `internal/` and `cmd/` only (`internal/daemon/*_removed_test.go`); the `go:embed` sets are `internal/pylib/aitest/*.py`, `internal/install/assets`, `internal/install/claudemod/...`; `fmt-check`/`vet`/`build` see only Go; `claude-mod-check` reads only `internal/install/claudemod`. **Assumption, not verified:** no test outside `internal/store` reads the repo's `.aira/tickets/` (e.g. a cmd/aira test running a ticket verb from its package directory against a private daemon). Backstop: GitHub CI runs `make test` (every pure-Go test) on every pull request (`.github/workflows/ci.yml` `pull_request:`), and no real-cgroup test (the only class the hook alone runs) reads repo tickets. Bash pitfall: errexit is suspended inside a function called from `$(...) ||`, so a missing `|| return 1` would let a failed git call through; the review checks every fallible command, and the caller additionally accepts only the two exact words. | A legitimate docs-only push still runs full `make ci` when `origin/master` is absent or stale, on a ref deletion, on a tag push, on a mixed push: conservative by design, never worse than today. |
| 6 | The column could fabricate a 0 for an unread peak: `confineNum` returns `unevaluated` for nil (T6a pins it). | None: display only. |

## 7. Test plan

Each new test must be shown red against the wrong implementation (mutation named) before the
fix is restored; the builder records each mutation run in the commit message.

Item 1 (pytester, in `internal/pylib/aitest/test_init.py`, run by
`TestRealPytestAitestPackageUnitTests` in `internal/pylib/pytest_aitest_supervisor_test.go`;
directly: `aira confine -- python3 -m pytest -p pytester internal/pylib/aitest/test_init.py`
from `internal/pylib`). Use the existing `fake_run` stub pattern (`test_init.py:135-169`):
`Supervisor.run` records its calls.
- T1a `test_collect_only_hands_back_to_pytest_and_never_runs_the_pool`:
  `runpytest("--aitest-workers=1", "--collect-only", "-q")` → stub never called, `ret == 0`,
  stdout names the collected test. Mutation: drop `opt.collectonly or` → stub called → red.
- T1b `test_collection_error_interrupts_before_the_pool_runs`: `test_ok.py` plus `test_bad.py`
  importing a missing module, `runpytest("--aitest-workers=1")` → stub never called,
  `ret == 2`, stdout contains `error during collection`. Mutation: drop the `testsfailed`
  clause → stub called, ret 0 → red.
- T1c `test_continue_on_collection_errors_runs_the_pool_and_keeps_the_error_in_the_exit_code`:
  same tree, `--continue-on-collection-errors`, stub returns `passed` for `test_ok.py::test_one`
  → stub called once AND `ret == 1`. Mutations: (i) revert the `collection_errors +` carry →
  ret 0 → red (measured today: 0); (ii) return `None` on any `testsfailed` regardless of the
  flag → stub not called → red.

Item 2 (`internal/daemon/eject_test.go`, using `lifecycleFixture` and the second-worktree
pattern at `eject_test.go:370-400`):
- T2a `TestEjectPartialRootsNamesTheProjectAndEveryRoot`: two registered worktrees,
  `os.RemoveAll(secondRoot)`, `server.eject(... project ...)` → `Code == E_EJECT_UNVERIFIED`,
  and `Error` contains the project id, the slug, `secondRoot` after `missing`, and
  `scope.Root` after `present`. Mutation: restore the old string → red.
- T2b `TestEjectPartialRootsRemedyInTheMessageWorks`: after T2a's refusal, run the remedy
  through the real store path `aira check` takes (`server.storeForScope(<present scope>)`
  then `.Check(ctx)`, the method the daemon's `check` storeop calls), then eject again → no
  longer the partial-roots refusal (assert `!strings.Contains(Error, "missing")`; the eject
  is expected to succeed). Mutation: skip `markWorktreeActive` in `Rebuild` → second eject
  still refused → red. **If T2b cannot be made green, the remedy sentence is removed from the
  message and the spec is amended; it is never shipped unproven.**

Item 3: no new automated test. Reason: the defect is an unbounded admission wait that exists
only under shared-slice contention; a deterministic test would need a contended real slice,
and a source-scan guard for one call pattern is machinery for a two-line change. Verification:
`AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/pylib -count=1` passes (all ten harness
builds succeed unwrapped), and `grep -rn '"aira", "confine"' --include=*_test.go internal cmd`
lists no build helper.

Item 4: T4 is the existing `TestConfineReserveGrantOutlivesTheDeclaredBound` with new timings.
Mutation (temporary, production): in `cmd/aira/main.go`'s confine-reserve hold (after the
`granted reserve=` line, ~2091) add a `time.After(maxWait)` arm that returns → the test must
go red with the new dwell. Then `aira confine -- go test -run
'^TestConfineReserveGrantOutlivesTheDeclaredBound$' ./internal/daemon -count=3` green.

Item 5 (`internal/install/githooks_test.go`, the existing real-git harness: real hook files
copied from this checkout's `.githooks`, recording `make` and `aira` shims). New helper
`newPushFixture(t)`: from `newHookFixture`, add a bare `origin`, commit a base tree on top of
the seed (`seed.txt`, `src/thing.txt`, `docs/old.md`, `.aira/config`,
`.aira/tickets/T-1.md`) with `--no-verify`, push it as `refs/heads/master` with `--no-verify`,
and `git fetch origin` so `refs/remotes/origin/master` exists. Every case commits with
`--no-verify`, resets the log, pushes, and asserts the exact recorded make argv. **A skip case
must also assert the hook's skip notice appears in the push output**, or "no make line" would
pass vacuously if the hook never ran. Existing fixtures stay non-docs (`seed.txt`,
`pushed.txt`, `change.txt`); the existing `TestPrePushHookBuildsThePushingWorktree` has no
`origin/master` ref and so still expects `ci`.

Table test `TestPrePushHookFastPathIsFailClosed`, one subtest per case:

| Case | Push | Expect | Mutation that must turn it red |
|---|---|---|---|
| docs-add | add `docs/a.md` | skip + notice | hook always runs ci (no skip ever) |
| docs-delete | delete `docs/old.md` | skip + notice | (same) |
| docs-rename-within | `git mv docs/old.md docs/new.md` | skip + notice | (same) |
| tickets | add `.aira/tickets/T-2.md` | `-C <wt> test-store` | tickets tier sent to skip or ci |
| docs+tickets | both | `test-store` | (same) |
| aira-config | edit `.aira/config` | `ci` | match `.aira/` instead of `.aira/tickets/` |
| docs+code | `docs/a.md` + `src/b.txt` | `ci` | classify only the first path |
| root-md | edit `REQUIREMENTS.md` | `ci` | allowlist `*.md` |
| lookalikes | each of `docsx/a.md`, `Docs/a.md`, `src/docs/a.md`, a file `docs` (separate subtests) | `ci` | `docs*` prefix without the slash, or substring match |
| rename-into-docs | `git mv src/thing.txt docs/thing.txt` | `ci` | replace `diff-tree --no-renames` with porcelain `git diff --name-only` |
| code-delete | delete `src/thing.txt` | `ci` | ignore deletions |
| code-then-docs | commit `src/b.txt`, then commit `docs/a.md`, push the branch | `ci` | diff only `<local>^..<local>`, or against the remote sha |
| stale-origin-master | push a code commit C to remote master (`--no-verify`), move `refs/remotes/origin/master` back to the base, push branch C + docs commit | `ci` | (same as above; proves staleness only widens) |
| no-origin-master | delete `refs/remotes/origin/master`, push docs-only branch | `ci` | treat an unresolvable base as "nothing changed" |
| orphan | `git checkout --orphan` with only `docs/x.md`, push | `ci` | drop the merge-base guard and diff against an empty tree |
| empty-diff | new branch at `origin/master`'s commit, push | `ci` | treat an empty path set as docs-only |
| lightweight-tag | tag a docs-only commit, push the tag | `ci` | drop the `refs/heads/` check |
| annotated-tag | `git tag -a` on a docs-only commit, push the tag | `ci` | (same) |
| ref-delete | `git push origin --delete <docs-branch>` | `ci` | skip the all-zero check |
| two-refs-docs-code | `git push origin docsA codeB` | `ci` | classify only the first ref line |
| two-refs-code-docs | `git push origin codeB docsA` | `ci` | classify only the last ref line |
| two-refs-docs | two docs-only branches in one push | skip + notice | require exactly one ref |
| head-refspec | `git push origin HEAD:refs/heads/x` from a docs-only branch | skip + notice | key eligibility on the local ref name |

Item 6 (`cmd/aira/confine_live_status_test.go` style, reading the cell by the header's
`PEAK` index, never a bare `strings.Contains`):
- T6a `TestConfineListPeakColumnSaysUnevaluatedNeverZero`: two records via
  `renderConfineList`, one `PeakRSS: nil`, one `PeakRSS` above its `ReserveBytes`; cells are
  `unevaluated` and the exact unclamped value. Mutations: render `0` for nil → red; clamp to
  reserve → red; no column → red.
- T6b `TestConfineListPeakColumnWiresTheRealScopeReader` (new `_linux_test.go` in
  `cmd/aira`): write a fake slice under `t.TempDir()` with two `.aira-<scope-id>` dirs
  (`cgroup.procs`, `cgroup.events`, `memory.current` 4096, `memory.max`, `cgroup.kill`; one
  with `memory.peak` 6144, one without), call the real reader `runner.ListConfines(ctx,
  slice, nil)`, feed its result through the stub dispatch into the real renderer (which
  JSON-round-trips it in `renderConfineListResponse`), and assert PEAK = `6144` and
  `unevaluated`. Mutation: render `record.RSSBytes` in the PEAK cell → `4096` → red.

Gate: `aira confine -- make ci` from the worktree, exact exit code recorded; plus the
item-1 pytest run and the item-4 `-count=3` run above.

## 8. Risks

- Item 5 takes effect only when the **primary checkout's** `.githooks` updates (core.hooksPath
  is one absolute path into the primary checkout, AIRA-76); feature branches keep the old hook
  until the owner's checkout pulls master. The harness tests this branch's hook files.
- Item 5 bash correctness (errexit suspension, quoting): mitigated by explicit `|| return 1`,
  the two-word output check, and the table above. The adversarial review re-derives each row.
- Item 5 tickets tier rests on the unverified assumption in section 6 (no non-store test
  reads repo tickets); GitHub CI's `make test` on the PR is the backstop.
- Item 1 changes a consumer-visible behaviour (collection errors now stop the run). This is
  pytest's own behaviour and the reason the rant was filed; release notes to deploy/speed must
  say it (`aira-release-notify-peers`).
- Item 3 moves the link step's memory into the outer `make ci` scope (bounded, re-learnt).
- Item 4 adds about 9 s to one real-cgroup test.

## 9. Deferrals (not built, and why)

- RANT-45's read-only `aira projects` verb: held as AIRA-316 (a new dispatch entry, MCP schema,
  skill count and codes surface for one report; item 2 answers the reported question).
- The all-roots-gone eject message (`eject.go:57`) is left as is: it already names the remedy
  (`--force`).
- RANT-47 item 1 (live charge for pinned reserves), 2b (largest-idle-holder clause on the wait
  line) and 3 (deleting the machine-wide p90 prior): owner decision, held as AIRA-317.
- A numeric PEAK cell in `aira top`: the reserve band already draws the peak, and the table is
  width-constrained.
- Building the aira binary once per pylib package (`sync.Once`): an optimisation, not needed to
  remove the unbounded wait.
- A fast path for `.aira/config`, `.aira/findings/`, any `*.md` outside `docs/`, or the
  pre-commit hook: each has a reader in the Go suite or is out of the rant's scope.
- An opt-out environment switch for the skip: `make ci` can be run by hand; no flag needed.
- Dropping the full-`make ci` cost of a ref-deletion push: rare, and fail-closed is the
  explicit requirement.

## 10. Expected yield

- Item 5: a docs-only or spec-only branch push (every planner commit like this one) skips a
  gate that today queues for a full `go test ./...` on the shared box; a ticket-only push runs
  one package. Code pushes are unchanged.
- Item 3: removes ten unbounded admission waits from every local pylib run (the reported
  "pylib breaches 20m on a busy box" failure class).
- Item 4: removes a reported recurring flake in the local gate.
- Items 1/1c: `--collect-only` works with aitest; a broken test module can no longer be
  hidden by a green pool (1c was a measured exit-0 false pass).
- Item 2: an operator can see which project and which roots block an eject, and the one verb
  that clears it.
- Item 6: over-pinned reservations are visible in the human table, using data already read.

## 11. Build record (2026-10-25)

The plan above served as the light-path plan section (scope, invariants, risks, tests,
deferrals); no separate plan file was written.

Deviations from the sketch, each within section 5's "the builder may restructure":

- Item 5: the classifier reads all of stdin into a variable before looping. `read` drops a
  final line with no trailing newline, which would let a code ref go unjudged; an empty
  stdin becomes one empty line and is refused by the field check. The unused first field is
  `_local_ref` (shellcheck clean). Extra cases beyond the table: `lookalike-tickets-sibling`
  (`.aira/tickets-old/`), and four cases feeding the real hook crafted stdin
  (`stdin-empty`, `stdin-unterminated-last-line`, `stdin-extra-field`, and the
  `stdin-docs-control` that shows the same well-formed line alone does skip). The
  two-ref helper gives every branch a `docs/` file of its own: a code-only branch is
  already refused by the empty-path-set check, which hid a classifier judging only the
  last ref line (measured: that mutation survived until the fixture changed). git orders
  the ref lines itself, so `two-refs-code-docs` names the code branch to sort first.
  `TestMakefileTestStoreTargetRunsTheStoreTests` (`make -n test-store`) pins the target
  the tickets tier calls. `docs/dev/agentic-development-loop.md` and `docs/dev/tooling.md`
  described the old always-full hook and now describe the two tiers.
- Item 2: both ejects in T2a/T2b pass `force: true` (the partial-roots check runs before
  the force test, so force does not mask it, and it keeps the post-remedy eject free of
  unrelated live-state refusals). T2a asserts the exact
  `missing: <root>; present: <root>. ` substring.

Mutation runs (each: apply, run the named tests, restore; runner under `~/tmp`, not
committed). Every mutation turned at least one new test red, except as noted:

| Item | Mutation | Red tests |
|---|---|---|
| 1 | drop `option.collectonly or` | T1a |
| 1 | drop the `testsfailed` clause | T1b |
| 1 | revert the `collection_errors +` carry | T1c |
| 1 | return None on any `testsfailed` regardless of the flag | T1c |
| 2 | restore the old message | T2a, T2b |
| 2 | drop the present-roots list | T2a |
| 2 | `Rebuild` skips `markWorktreeActive` | T2b |
| 4 | the confine-reserve hold returns at `time.After(maxWait)` | T4 |
| 6 | nil peak renders `0` | T6a, T6b |
| 6 | peak clamped to RESERVE | T6a |
| 6 | PEAK cell shows RSS | T6a, T6b |
| 6 | no PEAK column (the pre-fix code) | T6a, T6b |
| 5 | hook always runs ci (also the pre-fix hook) | docs-add, docs-delete, docs-rename-within, tickets, docs+tickets, two-refs-docs, head-refspec, stdin-docs-control |
| 5 | tickets tier sent to skip / to ci | tickets, docs+tickets (both) |
| 5 | match `.aira/` instead of `.aira/tickets/` | aira-config, lookalike-tickets-sibling |
| 5 | classify only the first path | docs+code, rename-into-docs, code-then-docs, stale-origin-master |
| 5 | allowlist `*.md` | root-md, lookalike-docsx, -Docs, -nested-docs, -tickets-sibling |
| 5 | `docs*` / `[dD]ocs/` / `*docs/` prefix | lookalike-docsx + file-named-docs / lookalike-Docs / lookalike-nested-docs |
| 5 | porcelain `git diff --name-only` | rename-into-docs |
| 5 | ignore deletions (`--diff-filter=d`) | docs-delete, rename-into-docs |
| 5 | diff only `<local>^..<local>` | code-then-docs, stale-origin-master |
| 5 | unresolvable base treated as docs | no-origin-master |
| 5 | orphan diffed against the empty tree | orphan |
| 5 | empty path set treated as docs | empty-diff |
| 5 | drop the `refs/heads/` check | lightweight-tag |
| 5 | drop `refs/heads/` and the commit-type check | lightweight-tag, annotated-tag |
| 5 | deletion treated as nothing to check (zero check, type check and merge-base failure all bypassed) | ref-delete, orphan |
| 5 | classify only the first ref / only the last ref | two-refs-docs-code (+ stdin-unterminated-last-line) / two-refs-code-docs |
| 5 | require exactly one ref | two-refs-docs |
| 5 | key eligibility on the local ref name | head-refspec |
| 5 | read stdin directly (no full read) | stdin-unterminated-last-line |
| 5 | read stdin directly and drop the `refs > 0` check | stdin-empty, stdin-unterminated-last-line |
| 5 | accept an extra field | stdin-extra-field |

Survivor, expected: dropping only the all-zero check leaves every test green, because
`git cat-file -t 000…` fails and the type check refuses the deletion anyway (defence in
depth; the zero check is kept as I5 lists it, and the combined mutation above shows the
ref-delete case does guard the property).
