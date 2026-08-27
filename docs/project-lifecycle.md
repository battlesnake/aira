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
