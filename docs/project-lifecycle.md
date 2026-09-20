# Project lifecycle

## Adopt a project

Run `aira init` in a Git worktree. For a new project, optionally choose its
slug and ticket prefixes:

```sh
aira init --project demo --prefix DEMO
```

If the repository already contains committed `.aira/` files, `aira init`
adopts that record instead of replacing it. It validates the committed config,
rebuilds the machine-local index from the files, and claims the configured
prefix only after the rebuild succeeds. A prefix conflict names its current
owner; eject that owner explicitly rather than editing `state.db`.

## Eject a project

From an adopted worktree, `aira eject` uses the current `.aira/config`. A dead
or remote worktree can be selected explicitly:

```sh
aira eject --prefix DEMO
aira eject --project <project-id>
```

Eject drains pending file writes, verifies the committed record, refuses live
leases, releases the prefix, and removes only the machine-local project index
and telemetry. The `.aira/` files and common Git journal remain, so a later
`aira init` can re-adopt them. A gone root requires `--force`.

`--purge` also removes `.aira/`. It refuses untracked, staged, or unstaged
changes under `.aira` unless `--force` is supplied. Commit anything that must
survive before purging.

## Tickets live in the worktree that created them

A ticket is a `.aira/tickets/<id>.md` file in the working tree where `aira
create` ran, tracked like any other git file. The id and its receipt are
recorded durably the moment `create` succeeds, but the ticket *body* is a plain
working-tree file — so a ticket created in a throwaway or linked worktree and
never committed is lost when that worktree is removed, exactly like any other
uncommitted file. Commit tickets (or create them from a durable checkout) so
they survive and reach other worktrees through git.

Because the file is git-tracked, its presence is per-checkout: a ticket
committed on one branch is simply absent from a worktree whose branch predates
it. `aira get <id>` tells this apart from an id that never existed — when the
machine-local ledger holds a ticket allocation for the id but no file is present
in the current worktree, it reports `E_TICKET_NOT_IN_WORKTREE` (naming the path
the ticket was originally recorded at) instead of a bare `E_NOT_FOUND`. It does
*not* claim the ticket is lost: the file may be committed on another branch or
worktree, or its uncommitted copy may have gone with a removed worktree — `git
log`, the other checkouts, and `aira worktree audit` tell those apart.
