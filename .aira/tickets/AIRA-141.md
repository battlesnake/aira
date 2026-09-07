---
{"schema":1,"id":"AIRA-141","project":"aira","title":"aira run's ci-shim launch path releases its daemon admission lease too early, unlike aira confine's shim path","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Accepted gap recorded during AIRA-129's Fable review (PR #87, merged `71d90d5`),
filed here as its own ticket per the review's own recommendation rather than
left buried in AIRA-129's resolution notes.

## The gap

`launchShim` (`internal/runner/runner_shim_linux.go`, AIRA-129) releases its
daemon admission lease at the end of `launchPrep` (child start) — matching
`aira run`'s own real-path model (the daemon's `.aira-CONFINE-*` adoption scan
never re-books an `.aira-RUN-*` scope either; the real path is covered
instead by the slice's live `memory.current` charge).

`confineShim` (AIRA-121), by contrast, holds its DAEMON lease for the job's
WHOLE life and releases only the flock after start.

In shim mode — no cgroup, so no live `memory.current` to fall back on — with
a declared or cgroup-memory-max budget and no readable own-cgroup usage (the
daemon's booked-reserve-only case), a running `aira run` job becomes
INVISIBLE to the admission ledger the instant it starts. A second `aira run`
can then be admitted against RAM the first job is already using, silently
over-committing the ci-shim RAM budget the whole mechanism exists to enforce.

## Why this wasn't caught in AIRA-129

No test drives `launchShim` through a real daemon grant far enough to observe
lease lifetime after launch — a coverage gap, written down rather than
silently left. AIRA-129's own dogfood exercised `admission=disabled` (a fresh
project has no `run.memory_slice`), so the daemon-admission leg of the shim
launch path was covered by the shared `r.admit` reading, not by a live grant.

## Why this is not a regression

Consistent with `aira run`'s EXISTING (non-shim) admission model — this is a
genuine divergence from `aira confine`'s shim behaviour specifically, not a
new hole `aira run` didn't already have in some form.

## Shape of the fix

Hold the daemon lease until the job's wait returns, the way `confineShim`
does, rather than releasing it at child start. Needs a test that actually
drives `launchShim` through a real daemon grant and observes the lease still
held (or the ledger still charged) while the child is running, to close the
coverage gap that let this ship unnoticed.