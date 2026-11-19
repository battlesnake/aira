# Agentic development loop

This repository uses a plan-first, review-gated V-model. The process keeps a
cheap design correction from becoming an expensive implementation rewrite and
makes correctness evidence durable.

## Standard loop

```text
start → plan → plan-review → plan gate → plan-fix → implement
      → work-review → work-fix → build gate → PR → merge
```

| Step | Who | Required output or gate |
|---|---|---|
| Start | owner/agent | Clean feature worktree, starting commit, checked prerequisites |
| Plan | implementer | Dated design spec with scope, invariants, seams, risks, tests, and deferrals |
| Plan review | Codex; Gemini optional (see the audit rules below) | Independent findings on shape, omissions, usability, and yield; review-only |
| Plan gate | Fable | Adversarial correctness, simplification, boundary, and abstraction review; approval before code |
| Plan fix | implementer | Findings folded into the spec; unresolved items are explicit |
| Implement | implementer | TDD implementation in the approved scope; exact command exit codes |
| Work review | Codex, then Fable | False-pass/false-fail review; two-loop red-team for correctness-critical work |
| Work fix | implementer | Every confirmed counterexample gets a regression test |
| Build gate | Fable + local evidence | Tests, coverage rationale, traceability, and review approval |
| PR / merge | owner or delegated gate | Feature branch only; the approved review record travels with the change |

The current Phase-1 plan gate is hard: Phase-1 implementation code is not
written until the SQLite result and every §21 design decision have been reviewed
and approved.

## How to work the loop

The plan must name the source of truth, the failure behaviour, the transaction or
ownership boundary, the observable evidence, and the things deliberately left
out. Design around prerequisites: the `blocked-by` ready queue is an early
feature because it prevents work behind an unlanded dependency.

Implementation starts with the smallest never-wrong cases. Tests cover both
directions of each invariant: valid input is accepted and invalid or ambiguous
input is refused. Correctness-critical changes use adversarial probes against
short-lived processes and crash/interleaving scenarios, not only in-process happy
paths.

Run ordinary development commands through `aira time -- <cmd>`, for example
`aira time -- go test ./...`, so the retained command-frequency and latency
evidence reflects the loop. Configure the project once with
`"run":{"prefix":["whale-run"]}` when the existing memory cap must remain the
inner launcher; `aira time` itself does not provide cgroup containment. The
retrofit shell form `whale-run(){ aira time -- "$@"; }` preserves the established
habit after that prefix is configured.

### Friction checkpoint

At a gate bounce or after a flaky retry, pause once: if that just cost you time,
`aira rant "<what got in the way>" --tag <recorded-tag>` it. This is the agent's
own workflow checkpoint, not an AIRA prompt; AIRA never nags during ordinary
commands. Capture the friction and continue the task rather than polishing the
wording or attempting diagnosis in the rant.

## Closing a ticket

The close path is `aira mv <id> in-progress`, then `in-review`, then `done`, in
that order. A ticket file's `status` is never hand-edited. A refused transition
(`E_TRANSITION_INVALID`) names the legal next statuses from where the ticket is.

The pre-push hook runs the full `make ci` on every push except three proven
cases. Two are path-based (AIRA-315), judged only when the pushed commit sits on
top of the `origin/master` tip and measured from that tip: when every pushed
path is Markdown under `docs/` (`docs/**/*.md`) it skips `make ci`; when every
path is Markdown under `docs/` or `.aira/tickets/` it runs only `make
test-store` (the package that parses the repository's own tickets). The third
is a recorded pass (AIRA-355): `make ci` that passes every step from a clean
worktree (no staged, unstaged, untracked or assume-unchanged/skip-worktree
file, the same HEAD tree before and after, no stop-or-ignore make flag
(`-n -t -q -i`) or variable override, `GOFLAGS` empty) writes a marker for that
tree under the git common directory, so it is shared by every worktree on this
machine. The steps run in a sub-make with make's inherited flags removed, so a
caller's `MAKE=`, `-i` or variable overrides cannot weaken what a marker proves.
The hook skips when every pushed branch's tree has a marker, and says which tree
and when. The marker proves the tree's content passed, not its ancestry: a
merge or conflict resolution that produces a new tree runs in full, but a rebase
that leaves the tree identical skips. Markers expire after 14 days or beyond the
newest 200, after which the push runs in full. Any doubt
(a tag, a deletion, a missing `origin/master`, a branch behind it, any other
path or file type, such as a `.go` or `.sh` file under `docs/`, a tree with no
marker) runs the full `make ci`. Because `make ci` tests the checked-out tree,
a push of a branch whose tree is neither checked out nor recorded is refused at
once: run `make ci` in a clean worktree at that commit, or push from there.

On a local box `make test`, `make race` and `make test-store` (and so `make ci`
and the pre-push hook) run `go test` with `TMPDIR` on a short per-run tmpfs
directory, `/dev/shm/t.XXXX` (AIRA-357, `.githooks/test-tmpdir`). The store's
`synchronous(FULL)` fsyncs dominated those suites on the WSL disk (measured:
`internal/store` 656 s on disk against 124 s on tmpfs, `internal/daemon` 343 s
against 132 s). Only where the temp files live changes: the same tests run and
`make ci` records a pass exactly as before. GitHub CI (`CI` set) is unchanged,
and so is a run where `/dev/shm` is missing or has under 1 GiB free (a full run
peaked at about 113 MB there); `AIRA_TEST_TMPFS=0 make test` opts out for one
run.
The go tool's build directory stays on disk (`GOTMPDIR`), and the daemon's
SliceCeiling real-cgroup fixture writes to the disk directory named in
`AIRA_TEST_DISK_TMPDIR`, because it measures page cache and a tmpfs file is not
page cache. A hard-killed run leaves its directory in `/dev/shm`, holding RAM;
the next run removes such a directory once it is over 10 minutes old and its
owner file proves the run that made it is gone (same pid namespace, pid dead or
reused); it never touches a live run's directory or anything without that proof.

## Lighter path for trivial changes

A typo, link correction, comment-only edit, or mechanical change with no
behavioural effect may use: inspect → edit → diff check → relevant lightweight
validation → one reviewer sanity check. It still stays in a worktree, records
the exact validation result, and must be escalated to the full loop when it
touches correctness, user-facing behaviour, persistence, IDs, leases, or the
agent workflow itself.

## Process rules from the 2026-11-19 audit

Owner-approved. Records: [synthesis](process-audit-2026-11-19-synthesis.md) and
[challenge](process-audit-2026-11-19-challenge.md); where they differ, the
challenge's corrections apply.

**Specs.**
- Before any spec of more than about 100 lines, run a premise measurement or a
  necessity check. Write its decision rule (what result means build, what means
  stop) first, before any query, and have one reviewer check the method and the
  verdict (AIRA-351 is the model).
- A spec stops as soon as one of its acceptance criteria is shown unmeetable, at
  whatever round, and becomes a short collapse memo: the criterion, the
  evidence, and what (if anything) is still worth doing.
- A spec is capped at about 450 lines. Review logs, replay output and other
  bulky records go in an appendix file. A section a round-1 reviewer flags as
  non-evidence is struck.

**Review rounds.**
- Rounds 1 and 2 run as today (round 2 reads the fix diff).
- Round 3 is kept, as the verification of fix 2: it is the only review of the
  code fix 2 added. On code it is Sol alone on the fix diff; Fable stays for
  specs and measurement gates. Round 3 has an owner (the coordinator): every
  round-3 P1/P2 ends as a fix commit, a recorded override, or a collapse,
  before merge. The non-convergence decision (see CLAUDE.md) is taken on round
  3's output.
- When a builder's embedded Sol round approved, the external review still runs
  for admission, lease, evaluator and kill changes. It is skipped only for a
  FINISH or COLLAPSE pass on a change that already had a full external loop.
  The same model has approved and then blocked the same head, so one Sol round
  is not a stable gate.
- Gemini is optional: an independent check at a non-convergence decision, not a
  routine reviewer.

**Review harness.**
- A reviewer that times out or is OOM-killed is retried once. Its failure is
  never handed to a fixer as a finding.
- No fixer is spawned when a round has zero real P1/P2 findings.
- Sol's timeout is measured from admission, not from queueing, and its relay
  gets a reserve sized from its measured peak.

**Merge.**
- Precondition: the three CI checks (`build` + vet + gofmt, `race`, `test`)
  are green on the head being merged. A failure gets one automatic re-run; a
  test that fails twice is quarantined with a ticket. GitHub does not enforce
  this (master has no branch protection); it rests on the agent's merge rule.
- The pre-push recorded-pass skip (above) replaces re-running `make ci` on a
  tree that already passed here; it is not a substitute for CI.

**Overhead.**
- Peer notices go out only when a named peer is affected (a proto, CLI or skill
  change it uses), one message per batch.
- Memory is written at session seams (a merge, a change of lane), not after
  every step.

### Minimum process per change class

| Class | Minimum |
|---|---|
| Docs-only | Author; one cold-reader review that runs every command the text prescribes; docs-only push path; green CI; merge. No plan review. |
| Small mechanical (refactor, deletion, small-fix batch) | Candidate list; necessity/behaviour vet; build with self-review and `make ci`; one review round (behaviour-equivalence plus a mutation spot-check); fix any P1/P2; green CI; merge. |
| Evaluator / admission / ID / lease (correctness-critical) | Premise measurement; spec of at most about 450 lines; plan review by Astra or Sol plus one independent Claude lineage; Opus build with embedded mutation self-review and an embedded Sol round; external build review round 1 (semantic plus test-porosity lens); fix; round 2 on the fix diff; round 3 as verification of fix 2; re-base or collapse at persistent blockers; green CI including race; staged observe-then-enforce with daemon-side triggers. |
| Measurement-only | Decision rule written first; one measuring agent; one method review in a single round; the record merged with its verdict. |

## Evidence discipline

Never infer `pass` from silence, a truncated log, an exit code without a parsed
result, or a test suite that did not actually exercise its target. Use the three
verdicts `pass`, `fail`, and `unevaluated`. A review finding is not closed by
prose alone: reproduce it, fix it, and pin the counterexample in a test or an
explicit durable design decision.
