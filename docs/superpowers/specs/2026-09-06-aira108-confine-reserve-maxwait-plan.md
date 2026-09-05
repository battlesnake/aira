# AIRA-108 — plan: `confine-reserve --pinned --max-wait` allegedly not honoured

Status: plan for review. Base: `origin/master` `ce9fa36`.

## 0. Verdict first — the alleged mechanism does not exist

AIRA-108 states, as a conclusion drawn from joint live `/proc` inspection:

> **this is a genuine, confirmed-live Go-side wait-bound enforcement gap** — every
> goroutine is correctly parked in its own logic … but whatever timer/deadline is
> supposed to fire at the declared `--max-wait` is not firing.

That conclusion is **refuted by live reproduction on this machine**, against the
real running `aira-daemon`, real cgroups and real wall-clock time. Both halves
were reproduced:

* **The waiting path IS bounded.** A genuinely-contended `aira confine-reserve
  --pinned --max-wait 10s` exited at **10.01 s**, `rc=4`, with
  `E_ADMIT_SATURATED`, printing no grant.
* **A GRANTED reservation deliberately outlives `--max-wait`, without limit,**
  until its stdin reaches EOF. That is the contract, stated verbatim in the
  AIRA-69 design spec §4 (`docs/superpowers/specs/2026-08-26-pytest-ram-weighted-governor-design.md:76-89`):
  *"On GRANT it prints one line to stdout (`granted reserve=<n> basis=pinned:client`)
  and **holds the connection open, blocking on stdin, until stdin closes / it is
  signalled**"*.

The `/proc` fingerprint the ticket recorded as proof of a wait-bound gap is, in
fact, **the fingerprint of the granted-and-holding state**, reproduced exactly.
The two states are distinguishable and were distinguished:

| state | thread `wchan` set | `fd/1` | outlives `--max-wait`? |
|---|---|---|---|
| waiting for admission | *N−1* × `futex_do_wait` + **1 × `do_epoll_wait`**, **no** `anon_pipe_read` | pipe | **no** — exits at the bound |
| granted, holding the lease | *N−1* × `futex_do_wait` + **1 × `anon_pipe_read`**, **no** epoll | pipe | **yes — by design**, until stdin EOF |

AIRA-108 records for pid 1736618: *"six on `futex_do_wait`, one on
`anon_pipe_read`"*, and no epoll thread. That is the **granted** row, exactly.
The ticket's own note dismissing the `anon_pipe_read` thread as "the known
stdin-EOF-watcher goroutine … benign, expected, not itself a symptom" inverted
its meaning: in `confine-reserve` that goroutine **does not exist until after the
grant has been written** (`cmd/aira/main.go:1257-1261`), so its presence is
positive proof the admission wait had already completed successfully.

Therefore: **`--max-wait` was honoured.** The helper was holding a legitimately
granted per-test RAM reservation for a caller whose test had stopped making
progress. The 52-minute and 506-second lifetimes are the *duration of the
caller's test*, not an admission wait.

### Live evidence

Probe A — granted, held (real daemon, real `aira.slice`, `--max-wait 10s`):

```
grant after 0.00s: b'granted reserve=67108864 basis=pinned:client\n'
t=  5.0s alive=True state=S (sleeping)
   threads: {…7× 'futex_do_wait', '1914700': 'anon_pipe_read'}
   fd/1 -> pipe:[342402774]
t= 15.0s alive=True   (same signature)
t= 30.1s alive=True   (same signature)
VERDICT-A: alive 30.1s after a 10s bound = STILL RUNNING (by design)
after stdin close rc=0
```

Probe B — waiting, saturated (private 6 GiB cgroup as the slice, so the shared
`aira.slice` ledger was never touched; a holder takes 3.5 GiB of a ~3.9 GiB
ceiling, then a second 3.5 GiB request cannot fit):

```
holder grant: b'granted reserve=3758096384 basis=pinned:client\n'
waiter at t=3.0s alive=True
   threads: {…7× 'futex_do_wait', '1945327': 'do_epoll_wait'}
   fd/1 -> pipe:[342539224]
waiter exited rc=4 after 10.01s (bound 10s)
   stdout=b''
   stderr=b'E_CONFINE_UNAVAILABLE: E_ADMIT_SATURATED: confine: admission rejected
            after 10s — slice contended, no memory admission within the wait
            (reserve 3584M/unknown)'
```

Both probes are promoted to committed regression tests (§4); the throwaway
harnesses lived under `~/tmp/aira108/`.

## 1. The three named candidate paths, traced — all correct

**(1) `admitThroughDaemon`'s transport deadline is reached and applied for a
`--pinned` reserve.** `confine-reserve` does *not* route through a lookalike
path: `internal/runner/confine_reserve_linux.go:36` calls `r.admitThroughDaemon`
**directly** (that is the whole point of the verb — daemon-only, never the flock
fallback, spec §4). Inside it, `admission_linux.go:341-358` derives
`transportDeadline = now + maxWait + admitTransportGrace` from the **requested**
`maxWait`, and `admission_linux.go:429` applies it with `conn.SetDeadline(...)`
unconditionally, before the request frame is written. It is cleared only at
`admission_linux.go:617`, after a full validated grant frame has been read — i.e.
only once the wait is provably over. `admitTransportGrace` is 1 s
(`admission_linux.go:88`). A daemon that never answers therefore fails the read
at `maxWait + 1s` → `fail()` → `E_CONFINE_UNAVAILABLE`. Verified live by probe B.

**(2) `confine-reserve`'s own wait loop, end to end.** There is no second loop.
`DefaultConfineReserveMaxWait` (`confine_reserve.go:12`) is 300 s; the CLI parses
`--max-wait` at `cmd/aira/main.go:1225-1231` (rejecting `<=0` and `>30m`);
`confine_reserve_linux.go:20-25` wires it into `Runner.admissionMaxWait`. The
polling loop in `admitWithFlock` (`admission_linux.go:~299`) — the only other
wait in the runner — is **never entered** by this verb. The single multiplicity
is the two-attempt too-large clamp retry (`confine_reserve_linux.go:33`), and its
first attempt is an *immediate* daemon rejection (`admit.go:1129-1132` /
`:1136-1140`, both before the wait timer is armed), so it adds no wait. Worst
case is therefore `maxWait + 1s` plus one round trip, not `2 × maxWait`.

**(3) The daemon has a matching bound.** `admit.go:1161-1180` computes
`remaining = max_wait_ms − since(waiter.enqueued)` and `select`s on that timer;
expiry runs `timeoutAdmitWaiter` and answers `E_ADMIT_SATURATED`
(`admit.go:1197-1213`). The daemon's own hold *after* a delivered grant
(`admit.go:1236-1241`: `select { <-peerCtx.Done(); <-s.stopping }`) is
deliberately unbounded and is the correct mirror of the client's stdin hold —
the reservation must last as long as the test it was granted for.

Nothing in these three paths fails to honour the bound.

## 2. What the real defect is

Not a wait-bound bug. Two real, connected defects, for which this incident is
direct evidence.

### D1 — the reservation ledger is diagnosis-hostile (the defect that cost the hours)

`aira confine --list` reports the entire scope-less population as **one opaque
aggregate**:

```
  of which: 0 confine scopes 0B, 5 scope-less reservations 5751380K, 2 adopted scopes 2706016K
```

(`cmd/aira/main.go:2624-2632`). There is no per-reservation signature, no age, no
holder. And a *granted* `confine-reserve` process is byte-identical in `ps` to a
*waiting* one — same argv, same `--max-wait 300s`. So an operator who sees a
long-lived helper **cannot establish which of the two states it is in from any
AIRA surface**, and the only inference left is the wrong one: "it blew its bound".

This is the **second** false P0 produced by this exact blind spot. The first is
recorded in AIRA-68's own comment at
`internal/runner/confine_manage.go:100-114`: *"Comparing Jobs against
len(Scopes) is therefore invalid, and doing so is what produced AIRA-68's P0
'23 admitted jobs, only 3 live scopes' report: 20 of those were healthy per-test
reservations."* AIRA-68's fix added the aggregate line above. The aggregate line
is demonstrably not sufficient — 5.5 GB of a shared 62 GB machine-wide ceiling
was pinned by *something* and nothing could say what.

By AIRA's own honesty bar this is a defect, not a nice-to-have: the surface
presents a number that invites a false diagnosis while withholding the fact that
settles it, when the daemon already holds that fact in memory (`admitWaiter`
already carries `grantedAt`; `admitRequest` already carries `signature`).

### D2 — the wait bound has no real-time coverage anywhere in the tree

Every existing `confine-reserve` admission test mocks the daemon with an
instantly-answering `net.Pipe` — `TestConfineReserveSaturatedIsTerminalNoGrant`
(`internal/runner/confine_reserve_linux_test.go:191`) asserts the *shape* of the
refusal, never that the wait ends at the bound. `cmd/aira/confine_test.go:354`
stubs `reserveConfined` entirely and dwells 20 ms. So **the regression AIRA-108
alleges would today be invisible to the whole suite**, which is precisely what
made the allegation unfalsifiable from inside the repository. That is an open
coverage gap regardless of this incident's outcome.

## 3. Scope

In scope:

1. `admitWaiter.signature` — store the signature `admitRequest` already carries
   (`admit.go:1313`, one field).
2. `admitSnapshot` gains per-reservation rows for **granted, accounted,
   scope-less** waiters — signature, reserve, held-since — collected in the
   **same locked pass** as every other count in `admitSliceSnapshotFor`
   (`admit.go:786-880`), so an operator can never be shown rows and totals from
   different instants.
3. `runner.ConfineSliceReserve.Reservations []ConfineReservationHold`
   (`internal/runner/confine_manage.go`), populated in
   `internal/daemon/confine_manage.go:126-160`.
4. `aira confine --list` renders the rows, **longest-held first**, capped at 10
   with an honest `… and N more` line, only when `ReservationJobs > 0`.
5. `--max-wait` help text (`internal/core/core.go:1733`) states the bound is on
   **admission only**, and `cmd/aira/main.go`'s `confine-reserve` block carries a
   comment recording this ticket's finding so the next reader does not re-derive it.
6. Tests, §4.

Explicitly **out of scope**, with reasons:

* **Any TTL, reaper or liveness probe for a granted scope-less reservation.** It
  would kill legitimately long tests, the reservation is already released
  deterministically when its holder exits or closes stdin, and
  `[[architectural-simplicity]]` is explicit: prefer "keep the primitive +
  document the gap" over new machinery. The property is documented instead.
* **Holder pid via `SO_PEERCRED`.** It would name the process directly, but needs
  new peer-credential plumbing through the admit connection; signature + age
  already identifies the holder unambiguously. Deferred, recorded on the ticket.
* **Anything in fastest-ee.** Its conftests still register the plugin AIRA-33
  deleted from *this* repo; that migration is theirs and is already recorded on
  AIRA-108.
* **"Fixing" the observed helper's longevity.** It is correct behaviour; changing
  it would silently un-reserve running tests.

## 4. Tests

All four are new. T1/T2/T3 are real-cgroup, real-daemon, real-CLI-subprocess,
real-wall-clock — the combination that did not exist for this verb.

* **T1 — a saturated wait is bounded** (promotes probe B). Private cgroup with a
  real `memory.max`; a holder takes the ceiling; a second real `aira
  confine-reserve --max-wait D` subprocess must exit **non-zero within
  `[D, D+grace]`**, print **nothing** on stdout, and name `E_ADMIT_SATURATED` on
  stderr. Fails if the bound is ever removed — the regression AIRA-108 alleges.
* **T2 — a granted hold outlives the bound** (promotes probe A). A fitting
  reservation is still alive at `3 × maxWait`, then exits 0 within seconds of
  stdin close. Fails if anyone "fixes" AIRA-108 by expiring granted holders.
* **T3 — `confine --list` names the held reservation.** While T2's helper holds,
  the daemon's confine-list result carries a row with that exact signature, a
  positive reserve, and a held-duration ≥ the dwell. Mutation check on
  build-review: deleting the row emission must turn this red.
* **T4 — rendering.** Golden render of the new rows including the 10-row cap and
  the `… and N more` line, extending
  `cmd/aira/confine_reserve_breakdown_test.go`.

**False-fail direction.** T1's window is `[D, D+5s]` with `D = 6s`: a correct
build exits within milliseconds of `D`, and host load can only delay the exit,
so the upper bound carries the whole margin. T2's dwell is safe in the same
asymmetric way as `grantedRelayHoldDwell`
(`internal/daemon/worker_admit_cli_granted_linux_test.go:41`): a correct helper
is parked in `io.Copy` on a pipe nobody has closed and cannot exit under any
load, while a helper that lost the hold returns microseconds later. Hosts
without delegated cgroups skip via `cgrouptest.SkipOrFailRealCgroup`.

**Accepted coverage gap, stated rather than papered over.** T2 proves the lease
is still held at `3 × maxWait`; it does **not** prove it is held until stdin EOF
— a regression releasing it at, say, ten minutes survives. That is the identical,
already-accepted bound on `grantedRelayHoldDwell`, and closing it needs a
production seam announcing lease state, which this project does not add for a
telemetry-grade signal.

## 5. Risks

* **T1 saturates a queue.** It uses a private cgroup created under the delegated
  user tree and removed in cleanup, never `aira.slice`, so no other session's
  admission is affected. The same isolation was used for the live probe.
* **Snapshot cost.** The new rows are gathered in a loop that already walks
  `queue.waiters` under `queue.mu`; no new lock, no new pass, no new scan.
* **Output growth.** Capped at 10 rows; a `--delegate-ram` suite with 40 workers
  prints 10 rows and one honest count line.

## 6. Expected yield

The next operator who sees a long-lived `confine-reserve` runs one command and
gets a positive answer — *"this is a granted reservation, signature
`pytest:…::test_x`, held 52m"* — instead of the multi-hour, two-session,
`/proc`-level investigation that produced this ticket, and instead of a
mis-filed P0. Plus the first real-time coverage of the admission bound itself.

## 7. Honest statement of what this plan does and does not claim

It **does** claim, on live evidence: the bound is honoured; the reported
lifetimes are the granted hold; the ticket's `/proc` reading was inverted.

It **does not** claim to have reproduced fastest-ee's original suite wedge. Why
that caller's test stopped progressing is outside this repository — the wedged
helper was a *symptom* faithfully holding a valid reservation, and the AIRA-side
plugin that spawned it is already deleted (AIRA-33). If a future report shows a
`confine-reserve` with the **`do_epoll_wait`** signature alive past its bound,
that is a genuinely different fault and T1 is the test that would have caught it.
