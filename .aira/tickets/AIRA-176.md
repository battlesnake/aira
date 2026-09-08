---
{"schema":1,"id":"AIRA-176","project":"aira","title":"Associate worktrees + agent/session identity with tickets, and classify staleness honestly (merged / no-changes / superseded / lost-work)","status":"in-progress","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","worktree"],"hold":false,"relations":[]}
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
