# AIRA

AIRA is a shared workspace for the AI coding agents on one machine. It hands them tickets, keeps them from picking up each other's work, remembers what code review found, records what their runs cost, and stops their test suites from OOM-killing each other. Think "JIRA for agents", plus a referee for the box they share.

It exists because a fleet of agents across many git worktrees keeps hitting the same three walls:

- **They collide on IDs.** Two sessions allocate ticket `FOO-42` at the same moment; one of them is silently dropped in a later rebase.
- **Review findings evaporate.** A bug caught in review lives only in some chat scrollback, so the same class of bug comes back next week.
- **They step on each other.** With no shared record of who owns what, two agents half-build the same thing, or one builds on a prerequisite that never landed — the most expensive kind of wasted work.

And underneath all of that they fight over the machine: one starts a 40-worker test suite, another loads a big dataset, the OOM killer fires, and someone's passing test run dies for no reason they can see.

AIRA is a single static Go binary. It keeps the durable record as plain markdown in your repo under `.aira/`, backs it with a rebuildable SQLite index, and runs a small background daemon for the cross-session bookkeeping. No cgo, no network, nothing in the cloud.

## What it does

### Track the work

Adopt a repo and AIRA becomes the authority for tickets and their IDs. IDs are allocated atomically, so two sessions can never collide on one. Each ticket is a single markdown file, committed alongside your code — so agents on different tickets never touch the same file, and the record lives in git.

```sh
aira init                                   # adopt this repo
aira create "Fix the parser" --kind bug --severity P1
aira list --by status                       # what's open, grouped
aira grep "parser"                          # full-text search across tickets
```

### Map the dependencies, find what's ready

Link tickets by how they actually relate, then ask what's unblocked — so nobody half-builds behind a prerequisite that hasn't landed yet.

```sh
aira link FOO-9 blocks FOO-12
aira ready                                  # tickets nothing is blocking — safe to start
```

### Claim work without collisions

An atomic lease says "this one is mine." Other sessions can see it, and a crashed session's lease expires so the work frees itself. This kills the two-agents-one-ticket churn.

```sh
aira claim FOO-12                           # take it
aira heartbeat FOO-12                       # still on it
aira release FOO-12                         # hand it back
aira lease ls                               # who holds what
```

### Remember what review found

Findings are typed, queryable records — not prose that scrolls off the top of a chat. Requirements and a covers/verifies graph tie code and tests back to what they are meant to satisfy, and `aira review` recommends how deeply a given change needs to be looked at.

```sh
aira review FOO-12                          # review-depth recommendation
aira find ls                                # findings, queryable
aira req ls                                 # requirements
```

### Run checks that can't quietly lie

A gate is a deterministic check with three verdicts, never two: `pass`, `fail`, or `unevaluated`. A gate that couldn't run reads `unevaluated` — it never reports a fake green. `aira check` sweeps the whole workspace the same honest way, catching duplicate IDs, a stale index, or a relation pointing at a ticket that no longer exists.

```sh
aira check                                  # integrity and health sweep
```

### Run commands and keep the receipts

AIRA can launch subprocesses for you — foreground or detached — capture their output, kill them cleanly by scope, and record what they cost. `aira time` wraps any command and records how long it ran and how it exited; test reports (JUnit or Go-JSON) are ingested, and flaky tests surfaced.

```sh
aira run -- pytest -q                       # run, capture, record
aira time -- go build ./...                 # record timing and exit status
aira test-report flaky                      # which tests flip
aira insights                               # live gauges over all of the above
```

### Referee the machine

Heavy jobs run inside a memory-capped, deprioritised cgroup that only starts when there is room for it. If a job goes rogue it dies inside its own slice instead of taking the desktop down with it. Several sessions can run at once, and AIRA keeps their combined footprint under the cap.

```sh
aira confine -- make test
```

For test suites, `--delegate-ram` governs memory per test rather than reserving for the whole job up front, and gates pytest-xdist workers on a shared pool of CPU slots. Each test's reservation is sized to what its worker is actually holding, so several sessions can all run `-n auto` and keep the box busy without OOMing each other.

```sh
aira confine --delegate-ram --memory-reserve 512M -- pytest -n auto
```

Most tests fit under a sane default; tag the few that are genuinely RAM-hungry:

```python
@pytest.mark.aira_mem("2G")
def test_big_solve(): ...
```

### Capture the friction

When something about the workflow is annoying or wrong, `aira rant` records it on the spot, with the git context attached, so papercuts get written down instead of forgotten.

```sh
aira rant "the --foo flag should default to bar"
```

## One tool, several front ends

Everything above is one core behind thin adapters: the `aira` CLI, an MCP server so agents can call it directly, a terminal UI, and an installable Skill. They all compile to the same verbs, so the faces can't drift apart, and the help, MCP schemas, and agent guide are generated from the same tables.

## Honesty by design

- **It won't lie.** A check that can't determine its answer reports `unevaluated`, never a fake pass or a silent zero. An ambiguous request is refused, not guessed.
- **It won't sprawl.** One static binary, no cgo; the release is a single file. AIRA supplies primitives and records verdicts — it never calls a model or plays judge.
- **It won't own your data.** Tickets, findings, and requirements are markdown you can read, grep, and diff. If AIRA's database vanished, it rebuilds from the files.

## Requirements

Linux with cgroup v2 and a systemd user session, and Go to build it. AIRA is machine-local and single-user by design: no server, no network.

## Install

```sh
go build -o aira ./cmd/aira
./aira install      # creates the memory-capped slice and starts the coordination daemon
```

`aira confine` then works in any directory with no further setup. The coordination features are opt-in per repo via `aira init`.

## Status

Working, in daily use, and young. It targets one Linux box shared by one user; multi-machine and multi-user are out of scope. Lifecycle gates and the traceability graph are advisory for now — they warn, they don't block. Expect sharp edges.
