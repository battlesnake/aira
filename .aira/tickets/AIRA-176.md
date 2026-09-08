---
{"schema":1,"id":"AIRA-176","project":"aira","title":"Associate worktrees + agent/session identity with tickets, and classify staleness honestly (merged / no-changes / superseded / lost-work)","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","worktree"],"hold":false,"relations":[]}
---

Owner request (2026-09-08): a recent session spent real effort manually
auditing a population of git worktrees to sort them into four buckets —
merged (safe to delete), no unique changes (safe to delete), superseded by
other work, and genuinely holding lost work that needed recovering before
deletion. No tool support exists for this today. The owner asked for a plan
first, not a build.

Design spec `docs/superpowers/specs/2026-08-07-aira-design.md:16,77` already
names ~194 live worktrees as an observed pain point and states worktree
bindings are meant to be "ephemeral and self-healing", but nothing anywhere
classifies a worktree's git state or says when one is finished. Verified:
there is no merge-base/`IsAncestor`/`--merged` logic anywhere in this
repository today.

## Plan

Full design: `docs/superpowers/plans/2026-09-08-worktree-ticket-association-plan.md`.

Summary of the proposal:

- A new, DB-only `worktree_bindings` table (ticket_id, worktree_id, branch,
  base_commit, owner, registered_at, last_seen_at, released_at) — durable
  local-machine state, same category as leases/area_hints, not a git file.
  Keeps history rather than a single current-row, so a repurposed worktree's
  prior ticket isn't silently lost.
- Classification is **computed live at query time, never stored** — the
  AIRA-175 lesson applied deliberately: a cached verdict that can drift from
  the git truth is a liability, not a convenience. `aira worktree audit`
  composes a small set of independently-named facts (uncommitted changes,
  unique commit count, merged-into-master, pushed-to-any-remote, live lease,
  bound ticket status) into a recommendation that names what was proven,
  never an opaque verdict — mirroring the AIRA-72 orphan-reaper's own
  discipline ("a TTL policy, not a death proof").
- A worktree with no registered binding falls back to inference from the
  branch name and commit-message `AIRA-<N>:` prefixes (this project's own
  consistent convention, observed all night) — always visibly labelled
  `inferred`, never silently promoted to `explicit` confidence.
- Two new verbs: `aira worktree register <ticket>` (SafetyMutate, the
  machine-checkable form of the "Start here" ritual CLAUDE.md/
  agentic-development-loop.md already mandate in prose) and `aira worktree
  audit [selector]` (SafetyReadOnly, reports only — never deletes, never
  writes). Deliberately **no auto-delete/gc in v1** — AIRA should never
  autonomously decide a worktree is disposable.
- MCP + Skill integration is the standard dispatch-table path (verified:
  one registration gets a CLI subcommand skeleton, a fully-generated MCP
  tool, and a golden-tested Skill/guide mention automatically; the only
  hand-written pieces are the CLI parseArgs/buildRequest arm and the
  contextual "when/why" prose block in `renderMarkdownBody`).
- Session/agent identity reuses the existing, validated
  `AIRA_CONFINE_OWNER`/`resolveConfineOwner` chain rather than inventing a
  third identity concept (a separate `AIRA_SESSION` inconsistency was found
  during research and is named as its own, smaller, future ticket).

The plan's §4 recorded four open questions; all four are **resolved** in §5
(new table keyed `(project_id, worktree_id, ticket_id)`; `claim` writes no
binding, `register` is the only writer; a fail-closed integration-ref chain
ending in `unevaluated` with `@{upstream}` banned; full two-loop). Two review
passes on §5 — an Astra plan review and a Fable gate — are answered in the
plan's §6, and §7 is the test plan. Where §6 names a change it supersedes §5
and §2.

## Status

Built on branch `aira176-worktree-ticket-association`. Shipped:

- `worktree_bindings` (project_id, worktree_id, ticket_id, branch, base_ref,
  base_commit, owner, owner_attested, registered_at), projects-FK cascade, no
  FK on ticket_id, no stored classification.
- `aira worktree register <id> [--base <ref>] [--owner ID]` (SafetyMutate) and
  `aira worktree audit [<id>|<path>] [--base <ref>]` (SafetyRead), both
  `RouteClient` via an explicit `Classify` case, both generated into MCP
  (`aira_worktree_register`, `aira_worktree_audit`) and the Skill.
- Live classification in `internal/worktree`: tri-state facts, a recommendation
  that names its basis, inference from branch name and `AIRA-<n>:` commit
  prefixes always labelled `inferred`, and no deletion path of any kind.
- Identity from the `AIRA_CONFINE_OWNER` chain, resolved from the RESOLVED SCOPE
  ROOT (not the process cwd) so MCP attributes a binding correctly, with
  `owner_attested` derived — never assertable from the command line.

## Review (Fable build-review gate) — MERGED

PR #120 merged as `07f9064` (2026-09-09; branch tip `d3da6bf`, PR base
`777d75b`, merge base at landing `5a39425`). Everything below is the reviewer's
own verification against source and by running the suite, not the builder's
transcript.

- **AIRA-175 lesson honoured.** No classification verdict is stored anywhere:
  every fact in `internal/worktree/facts.go` is a fresh subprocess per run, and
  the schema (`store.go` `worktree_bindings`) has no column for one. The three
  git-derived columns a binding does carry (`branch`, `base_ref`, `base_commit`)
  are labelled at-registration hints; the audit reads the live branch/HEAD off
  `git worktree list --porcelain`, and `base_ref` is only chain step 2 and is
  re-verified with `rev-parse --verify` before use (a deleted ref becomes
  `unevaluated`, not a stale answer). The orphan report's path comes from the
  `worktrees` registry, not from the binding row.
- **F3 join verified end to end, not just in tests.** Smoke against a real
  isolated daemon (`XDG_STATE_HOME` keyed): `register` stamped the store's own
  64-hex `hashPath(gitDir)` identity and a subsequent `audit` joined it as
  `explicit` — so `store.CanonicalScopeIdentity` injected as `IdentityCall`
  matches the binding key in practice. Same smoke: unpushed unique commit →
  `recover`; main → `main-worktree`; clean detached checkout with no resolvable
  integration ref → `none` with the remedy named, never a removal verdict; that
  checkout dirtied → `recover` with no integration ref (§5.3 asymmetry live);
  `--base origin/typo` → `E_SELECTOR_INVALID` exit 2.
- **Porosity: 11 mutations, 11 genuine kills**, each by the named test, none by
  a compile error (one first attempt was a vacuous kill from an unused import
  and was redone with the import kept in use): `Flag.False()`→`!True()` (3
  tests), env unscrubbed (`GIT_DIR` decoy test), merge-base exit collapsed,
  main-worktree exclusion removed, superseded skipping an unread ticket,
  unknown identity read as no-lease, budget ignored, `worktree-` routing case
  removed, unpushed-recover bucket removed, store stamping a forged identity,
  `ON CONFLICT` dropping `owner_attested` (attestation inheritance).
- **Gates, reviewer's own runs, confined, exact exit codes.** On
  merge(`0c6e0b8` + PR): `go build ./...` 0, `go vet ./...` 0, `gofmt -l` 0
  (empty), `go test ./...` 0 — 15 packages `ok`, none cached, `internal/worktree`
  included (the first attempt was OOM-killed at the auto-estimated 1.2G cap and
  re-run with `--memory-reserve 10G`). Four unrelated PRs (#114/#117/#118/#119)
  landed on master between that base and the merge, so the landed tree
  `07f9064` is re-gated by this commit's own pre-push `make ci` (fmt-check, vet,
  build, `go test ./... -count=1`); the merged `store.go` was read to confirm the
  `worktree_bindings` table composed cleanly beside the rant changes. CI on the
  PR: build+vet+gofmt pass, test pass. The first pre-push `make ci` on the
  landed tree was REFUSED: `gitremote/TestRealRunTimeoutKillsProcessGroup`
  failed with `pid output=""` — its 50ms deadline expired before `/bin/sh`
  printed, while the hook's auto-estimated 1.24G cap had the suite at 95% of
  its reserve beside a cgroup-heavy sibling test. That package is untouched by
  this PR, the same test passed in the full run above and in CI, [[AIRA-144]]
  already names it as the 50ms-deadline flake, and a confined `-count=5`
  re-run on `07f9064` with a 3G reserve passed 5/5 (exit 0). Not bypassed: the
  push was retried with `AIRA_CONFINE_RESERVE=10G` so the hook's own `make ci`
  gates this commit unstarved.
- **Accepted gaps, written down** (none block; all fail safe): (1) a checkout
  whose `rev-parse --git-dir` fails is not marked `seen`, so its binding is
  listed under `orphan_bindings` — an informational misreport, no removal
  recommendation is possible for it; (2) a `locked` worktree is reported but not
  weighed by `recommend` — `git worktree remove` itself refuses a locked
  checkout without `--force`; (3) squash merges and stale remote-tracking refs
  as recorded in the plan §7.1. (4) **Deploy note:** the table is created by
  the daemon's `initDB`; a new client against a not-yet-restarted daemon fails
  `WorktreeBindings()` loudly (`no such table`), never an empty all-clear —
  restart `aira-daemon.service` after `install.sh`.
- Both builder-flagged decisions stand: the main checkout is never a removal
  candidate (caught by the real-git test as well as the smoke), and the three
  new codes are catalogued with exits 2/3/4 (`internal/codes/codes.go`). The
  four-line `CLAUDE.md` addition is kept: it records the register/audit ritual
  the generated Skill already teaches, and the two documents now agree.
