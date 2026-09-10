# AIRA — agent guide

AIRA is a machine-local coordination layer for AI coding agents. The authoritative
whole-product design is [`docs/superpowers/specs/2026-08-07-aira-design.md`](docs/superpowers/specs/2026-08-07-aira-design.md).
This file is the operating contract for agents working in the repository.

## Start here

Before changing anything:

1. Work in a dedicated git worktree on a feature branch. The repository root stays
   on `master` for the owner; never develop there.
2. Confirm the worktree is clean and record the starting commit. Run
   `aira worktree register <id>` alongside `aira claim <id>`: the claim says you
   are working it now, the binding says which ticket this checkout is for and
   outlives the lease. Before removing any worktree, read
   `aira worktree audit` rather than eyeballing `git status` and `git log`.
3. Read the applicable design spec in full. Do not infer a missing decision from
   an implementation convenience.
4. Check the backlog and prerequisites. A ticket blocked by an unlanded
   prerequisite is not ready to build.

Temporary stashes must use an explicit ref, never the shared repo-global stash
stack. If a stash is unavoidable:

```bash
stash_ref=refs/agent-stash/aira/<ticket>/<unique-purpose>
sha=$(git stash create "aira/<ticket>/<purpose>")
test -n "$sha" && git update-ref "$stash_ref" "$sha"
```

Restore with `git stash apply "$sha"`, then remove only the named ref with
`git update-ref -d "$stash_ref"`. Prefer a second worktree when parallel work
is possible.

## Persistent scratch

Never do scratch work in `/tmp`: it is reboot-volatile. Disposable probes,
measurement harnesses, captured logs, and intermediate generated files belong
under `~/tmp/`, outside every worktree. A published measurement must still have
a committed, executable reproduction. The repository root and feature
worktrees contain only intentional project files.

## Development loop

Substantive work follows:

`plan → plan-review → plan gate → plan-fix → implement → work-review → work-fix → build gate → PR → merge`

The plan is a committed design spec. Codex and Gemini provide the orthogonal
plan review when available; Fable is the plan gate. Implementation is TDD and
must preserve the scope and explicit deferrals in the approved plan. Codex then
reviews the build and Fable performs the final review. The two-loop adversarial
process is mandatory for ID allocation, crash recovery, lease CAS, and other
correctness-critical work. Purely trivial documentation or mechanical changes
may use the lighter path described in
[`docs/dev/agentic-development-loop.md`](docs/dev/agentic-development-loop.md).

## Hard engineering rules

- Phase 1 is coordination MVP: git-file content, rebuildable index,
  reconciliation, common-directory receipts and journal, IDs, tickets,
  relations, leases, area hints, and the blocked-by ready queue. The runner,
  telemetry, gates, and review emission are later phases.
- AIRA is primitives, not judgement. A check that cannot establish its result
  reports `unevaluated`, never a fake pass or zero. Ambiguous selectors are
  refused. Errors use stable codes.
- The core is one downward-layered implementation behind `core.Do`; CLI, MCP,
  Skill, daemon, and TUI are thin faces. Generated help, MCP schemas, and the
  agent guide come from dispatch tables.
- No cgo. The release target is one static Go binary.
- Prefix heavy commands, including Go builds/tests and real-data loads, with
  `whale-run` (which now execs `aira confine --slice aira.slice`, confining the job
  to a cgroup scope under `aira.slice`). If a job must be stopped, `kill <PID>` the
  `whale-run`/`aira confine` process you started (it cgroup-kills its own scope);
  never `systemctl --user stop aira.slice` (or `whale.slice`) — that hits every
  session. A whale-run job now has no graceful shutdown (Ctrl-C hard-kills the tree).
- Do not claim a command is green from truncated output. Record its exact exit
  code and distinguish `pass`, `fail`, and `unevaluated`.
- Phase 0 uses `covers:` in Go doc comments and `verifies:` in tests as a
  traceability convention. The enforcing fail-closed graph check arrives in
  Phase 3; it must not be invented as a vacuous Phase 0 gate.
- Until AIRA's allocator exists, the seed registries are maintained manually.
  The migration from an external `make id` allocator to `aira id` is an
  explicit Phase 1 concern. Once `aira id` exists, never hand-pick IDs.

## Review and merge

Reviews are durable engineering evidence. A plan records its scope, invariants,
risks, tests, expected yield, and deferrals. A build review must examine both
false-fail and false-pass directions, and every confirmed counterexample becomes
a regression test. Coverage gaps are written down and accepted by reviewers;
they are never silent. See [`docs/review-and-merge-policy.md`](docs/review-and-merge-policy.md)
and [`docs/adversarial-verification.md`](docs/adversarial-verification.md).

## Repository layout

The intended downward layers are:

```text
internal/store/   git files, common-dir audit data, SQLite, reconciler
internal/domain/  tickets, relations, leases, findings, projects, events
internal/query/   selectors, cursors, distributions, FTS
internal/interp/  command language
internal/runner/  later subprocess execution
cmd/aira/         thin CLI, MCP, TUI, daemon, and install faces
```

Phase 1 does not create runner, telemetry, or gate implementation merely because
the top-level architecture names them.

## Simplify and challenge before building — by default, not on request

Before building anything non-trivial — especially when a design, ticket, or plan
arrives as a given — run a short pass separate from "is it correct?": restate, in
plain terms, the actual problem the requester needs solved, then ask **does this
need building at all? what is the simplest thing that solves it? what EXISTING
complexity can be removed?** A green correctness review proves a design is built
right, never that it should be built — necessity is a separate question, and it is
yours to raise unprompted.

**Forcing function — the greenfield minimum.** Ask: *"if we had to solve ONLY this
problem, from scratch — no existing code, tools or solutions to reuse, and no future
problems to anticipate — what is the bare-minimum viable build?"* That answer is the
yardstick. Compare it to what you have or are about to build: the gap is candidate
complexity, and every part of it needs a live justification — reuse genuinely cheaper
than the minimum, or a future need that is actually arriving — never "it might be
handy" or "it's already there." Drift from the minimum should be a conscious choice,
not an accident.

Bias toward removing or collapsing over adding; prefer "keep the primitive + document
the gap" over new machinery. AIRA has no users and no backwards-compat obligation, so
*deleting* existing complexity is on the table, not just declining new complexity.
Before reversing a trade, check whether a prior decision already settled it (grep the
ticket store) — don't silently re-litigate it.

Scale it to the stakes: a one-line sanity check for a small change; an explicit
challenge-review for anything substantial. When you brief a reviewer, make **"what
can be simplified or removed while still solving the original problem?"** and **"what's
the from-scratch minimum for *just* this problem?"** standing questions alongside
correctness — the human shouldn't have to ask.

In this repo this is the operating form of "AIRA is primitives, not judgement" and the
architectural-simplicity rule: a real design (AIRA-224) passed two correctness gates
before a challenge pass found the requirement was already met by the existing admission
ledger and the right move was to build nothing.

A second worked example (2026-09-10, reservation-model admission): a challenge pass *during
the build* found that the counter the daemon already has, plus socket-liveness (the holder's
fd closing = release, SIGCHLD downward), plus a rigid version-frozen reconnect+re-declare for
the daemon-restart-without-reboot case, would meet the admission requirement more simply —
retiring the kernel-scan/adoption, the live-`memory.current` charge (`refreshWaiterCharge`),
the `aira worker-peak` relay, and the dual-role `dynamicReserve` flag the design was
accreting. Its one trade (a bounded cold-start over-admit window) sits inside the
already-accepted "bounded, not airtight" envelope. Every over-design smell in the global
CLAUDE.md fired here, and the correctness gates (green, mutation-tested, race-clean) caught
none — they test built-right, not build-right-thing. Recorded in the review-against-actual-problem
memory.
