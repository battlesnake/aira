# AIRA

AIRA keeps a fleet of AI coding agents from trampling each other on one machine.

When several agents share a box (or just several terminals), they fight over the same CPU and RAM. One starts a 40-worker test suite, another loads a big dataset, a third kicks off a build. The machine starts swapping, the OOM killer fires, and someone's passing test run dies for no reason they can see. AIRA is the referee. It runs each heavy job in a memory-capped, deprioritised cgroup, starts a job only when there's room for it, and gives parallel test suites as many CPU workers as the box can actually spare.

It's a single static Go binary. It also runs a small background daemon for the cross-session bookkeeping, which `aira install` sets up for you. No cgo, no network, nothing in the cloud.

## The two things it does

### 1. Confine heavy work

```sh
aira confine -- make test
```

That runs `make test` inside a machine-wide slice with a hard RAM cap and group-kill on OOM, niced down so it never freezes your desktop. It won't even start until there's memory for it, and if it goes rogue it dies inside its own slice instead of taking the machine down with it. Several sessions can `aira confine` at once, and AIRA keeps their combined footprint under the cap.

For test suites, `--delegate-ram` beats reserving for the whole job up front. AIRA governs each test: it reserves RAM per test and gates pytest-xdist workers on a shared pool of CPU slots, so several sessions can all run `-n auto` and keep the box busy without OOMing each other.

```sh
aira confine --delegate-ram --memory-reserve 512M -- pytest -n auto
```

Most tests fit under a sane default. Tag the few that are genuinely RAM-hungry:

```python
@pytest.mark.aira_mem("2G")
def test_big_solve(): ...
```

### 2. Coordinate the work itself (optional)

Adopt a repo and AIRA gives it tickets, dependency links, atomic leases, a "what's unblocked" queue, review findings, and telemetry. Everything is stored as plain markdown under `.aira/` and committed alongside your code, so the record lives in git and AIRA's database is just an index it can rebuild from the files.

```sh
aira init --project demo --prefix DEMO
aira create "Fix the thing" --kind bug --severity P1
aira ready          # tickets nothing is blocking
```

## Requirements

Linux with cgroup v2 and a systemd user session, and Go to build it. AIRA is machine-local and single-user by design: no server, no network.

## Install

```sh
go build -o aira ./cmd/aira
./aira install      # creates the memory-capped slice and starts the coordination daemon
```

`aira confine` then works in any directory with no further setup. The coordination features are opt-in per repo via `aira init`.

## Ways in

One core, several front ends: the `aira` CLI, an MCP server so agents can call it directly, a terminal UI, and an installable Skill.

## What it won't do

- **Lie.** A check that can't determine its answer reports `unevaluated`, never a fake pass or a silent zero. Ambiguous requests are refused, not guessed.
- **Sprawl.** One static binary, no cgo; the release is a single file.
- **Own your data.** Tickets and findings are markdown you can read, grep, and diff. If AIRA's database vanished, it rebuilds from the files.

## Status

Working and in daily use, but young. It targets one Linux box shared by one user; multi-machine and multi-user are out of scope. Expect sharp edges.
