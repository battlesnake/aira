---
{"schema":1,"id":"AIRA-141","project":"aira","title":"aira run's ci-shim launch path releases its daemon admission lease too early, unlike aira confine's shim path","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
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

## Resolution

Built, exactly the shape the ticket asks for and nothing wider.
`internal/runner/runner_shim_linux.go` only; no new mechanism, no new
tunable, no wire or schema change.

### The change

`launchPrep`'s `defer releaseAdmit()` — described there as a leak backstop,
but in fact the whole admission lifetime, since a successful prep released
the lease at child start — is gone. The backstop now sits at `launchShim`'s
own scope, so it still fires on every failure path while no longer ending the
lease on the success path. The lease is therefore held until `launchShim`
returns, which is after the wait has returned and the terminal has been
committed.

That is `confineShim`'s rule, reused rather than reinvented: the SAME
`admissionResult.releaseAdmission` (the daemon lease is the admit socket
itself, closed to release), the SAME `sync.Once` wrapper, the SAME
`admission.lock != nil` discriminator.

Every failure inside `launchPrep` still releases EXPLICITLY, and that is not
redundant with the outer defer: it must happen BEFORE `failLaunchPrep`,
because that call evaluates terminal arbitration and a sibling must be able
to recheck admission before it does. Only the ordering guarantee changed
hands; the release itself is unchanged.

### Why the two admissions are treated differently

The FLOCK fallback is still released at child start, and that divergence is
deliberate rather than an oversight in the mirror:

- A daemon grant is a BOOKED RESERVE against the shim RAM budget. It must
  last as long as the RAM does, because in shim mode there is no cgroup and
  therefore no `memory.current` charge naming the job — the booked reserve is
  the only representation of it the ledger has. This is precisely why the
  real `aira run` path can release at start and this one cannot: there the
  kernel keeps counting after the lease goes.
- The flock fallback (daemon down) carries no reserve at all. It is
  whole-slice mutual exclusion — one holder, admitting the next client only
  when this one lets go. Holding it for a job's life would turn a degraded
  fallback into a global serialiser of every shim launch on the box.

`confineShim` draws the line in the same place and reads the same
`admission.lock != nil` flag, which only `admitWithFlock` ever sets.

### Tests

`internal/runner/runner_shim_admission_lease_linux_test.go`, closing the
coverage gap the ticket names: before this, nothing drove `launchShim`
through a real daemon grant at all.

- `TestShimRunHoldsTheDaemonAdmissionLeaseUntilTheJobEnds` — a fake daemon
  over `net.Pipe` answers one `admit` grant; the lease IS that connection, so
  a read on the daemon side returns at the release and at no other time (no
  seam, no timing guess). The child blocks on a marker this test alone
  writes; with the run provably live, the lease must still be open, and after
  the run ends it must be closed (held is not the same as leaked). The record
  is asserted to carry `admission=immediate`, so the test cannot pass
  vacuously against an `admission=disabled` launch whose nil lease is never
  released — the exact reading AIRA-129's dogfood was limited to.
- `TestShimRunReleasesTheFlockFallbackWhenTheChildStarts` — the false-fail
  direction. A real `flock` is taken by the injected `lockAttemptFn`, and
  while the child runs a second open file description on the same file must
  be able to acquire it (`flock(2)` treats separate descriptions
  independently even within one process, so this contends exactly as another
  client would).

Both verified NON-POROUS by mutation, each against the other:

- Restoring the pre-fix release (`defer releaseAdmit()` back inside
  `launchPrep`, outer defer and flock branch removed) FAILS the daemon-lease
  test — exit 1 — while the flock test still passes.
- Dropping the `admission.lock != nil` discriminator (releasing nothing at
  start) FAILS the flock test — exit 1 — while the daemon-lease test still
  passes.

The daemon test's non-porosity does not rest on its 250ms dwell: `running` is
appended AFTER the child starts and the pre-fix release happened at the end
of `launchPrep`, strictly BEFORE that append, so the lease is already closed
when the assertion is reached. The dwell only removes the goroutine-scheduling
window.

### Not changed, deliberately

- The real (non-shim) `aira run` path. Its early release is correct for the
  reason above (the slice's live `memory.current` charge), and the ticket
  scopes this to the shim divergence.
- The other accepted gaps AIRA-129's review recorded — the interrupt window
  between `interrupted.Load()` and `startWith`, and the cosmetic late-signal
  wording. Neither is this ticket, and neither was silently swept in.

### Verification

Foreground, exact exit codes, on `origin/master` `b390650`:

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0

An earlier full-suite attempt on this same tree exited 1 with two
`internal/store` traceability failures reporting SQLite `database or disk is
full (13)`. That was the machine, not the diff: the box's root filesystem was
at 100% (5.1G free of 1007G) during that run, this change touches
`internal/runner` only and no `internal/store` test can reach `launchShim`,
and `./internal/store -run TestTraceability` passed in isolation (exit 0)
immediately afterwards. Recorded rather than quietly re-run away.