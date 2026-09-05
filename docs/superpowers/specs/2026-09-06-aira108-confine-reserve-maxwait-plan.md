# AIRA-108 — plan: `confine-reserve --pinned --max-wait` allegedly not honoured

Status: plan, revised after review (Sol `APPROVE-WITH-CHANGES`, DeepSeek-pro,
Gemini — all three concurring). Base: `origin/master` `ce9fa36`.

## 0. Verdict first

AIRA-108 states, as a conclusion drawn from joint live `/proc` inspection:

> **this is a genuine, confirmed-live Go-side wait-bound enforcement gap** — every
> goroutine is correctly parked in its own logic … but whatever timer/deadline is
> supposed to fire at the declared `--max-wait` is not firing.

**The available evidence does not substantiate that.** Stated precisely, and
deliberately not more strongly than the evidence carries (this wording is a
review correction — the first draft claimed the bug "does not exist", which is a
claim about history that no present-tense inspection can make):

1. **The inspected process was not waiting at inspection time.** It was holding
   a successfully granted lease. Proven below by binding the reporters' own
   `/proc` fingerprint to a reproduction of each of the two states.
2. **A granted reservation deliberately outlives `--max-wait`, without limit,**
   until its stdin reaches EOF. That is the committed contract for this verb
   (AIRA-69 design spec §4,
   `docs/superpowers/specs/2026-08-26-pytest-ram-weighted-governor-design.md:76-89`:
   *"On GRANT it prints one line to stdout … and **holds the connection open,
   blocking on stdin, until stdin closes / it is signalled**"*). So "alive past
   `--max-wait`" is not by itself evidence of anything.
3. **The waiting path is bounded, measured live at 10.01 s against a 10 s
   bound**, by two independent mechanisms (client transport deadline, daemon
   wait timer) that would both have to fail together.

What is **not** claimed: that a late grant was historically impossible for pid
1736618. Nothing observable now can rule that out. What *is* claimed is that the
one piece of evidence offered for it — the `/proc` fingerprint — in fact says the
opposite, and that the mechanism named in the ticket is not present in the code.

### The two states, measured

| | waiting for admission | granted, holding the lease |
|---|---|---|
| goroutine 1 | `[IO wait]` → `net.(*conn).Read` ← `io.ReadAtLeast` | `[select]` in `main.runConfineReserveCommand` |
| distinguishing thread | `do_epoll_wait`, syscall `281` (`epoll_pwait`) on fd 4 | `anon_pipe_read`, syscall `0` (`read`) **arg0 = `0x0` = fd 0 = stdin** |
| the other's marker | **no** `anon_pipe_read`, nothing reading fd 0 | **no** epoll thread |
| outlives `--max-wait`? | **no** — exits at the bound, `rc=4`, stdout empty | **yes, by design** — until stdin EOF |

AIRA-108 records for pid 1736618: *"six on `futex_do_wait`, one on
`anon_pipe_read`"*, and names no epoll thread. That is the **granted** column.
(The thread *count* differs from my probes — 7 there, 8–9 here; that varies with
`GOMAXPROCS` and load and carries no information. What discriminates is the
presence/absence of the two marker threads, which is invariant.)

The ticket's own note dismissing the `anon_pipe_read` thread as *"the known
stdin-EOF-watcher goroutine … benign, expected, not itself a symptom"* inverted
its meaning: in `confine-reserve` that goroutine **does not exist until after the
grant has been written** (`cmd/aira/main.go:1257-1261`), so its presence is
positive evidence the admission wait had already completed.

### Live evidence (real machine, real daemon, real cgroups, real wall clock)

Probe A — **granted, held**, real `aira.slice`, `--max-wait 10s`:

```
grant after 0.00s: b'granted reserve=67108864 basis=pinned:client\n'
t= 5.0s / 15.0s / 30.1s  alive=True  state=S (sleeping)
   threads: 7x 'futex_do_wait' + 1x 'anon_pipe_read'      (no epoll thread)
VERDICT-A: alive 30.1s after a 10s bound = STILL RUNNING (by design)
after stdin close rc=0
```

Probe B — **waiting, saturated.** A private 6 GiB cgroup was used as the slice so
the shared `aira.slice` ledger was never touched; a holder took 3.5 GiB of the
~3.9 GiB effective ceiling, then a second 3.5 GiB request could not fit:

```
waiter at t=3.0s alive=True
   threads: 7x 'futex_do_wait' + 1x 'do_epoll_wait'       (no anon_pipe_read)
waiter exited rc=4 after 10.01s (bound 10s)
   stdout=b''
   stderr=b'E_CONFINE_UNAVAILABLE: E_ADMIT_SATURATED: confine: admission rejected
            after 10s — slice contended, no memory admission within the wait'
```

Probe C — **binds the fingerprint to fd 0** (review point: `wchan` alone does not
identify the fd), and takes the SIGQUIT goroutine dump AIRA-108 asked `split` for
and never received:

```
alive 12.2s past an 8s bound: True
  tid 2061053  wchan=anon_pipe_read  syscall=[0 0x0 0xc000342000 0x2000 …]
                                              ^ read(2)  ^ fd 0 == STDIN
goroutine 1 gp=0xc000002380 m=nil [select]:
    runtime.selectgo(...)
    main.runConfineReserveCommand(...)
    main.runWithInputDispatcher(...)
    main.main()
```

Probe D — the same dump for the **waiting** state, for contrast:

```
  tid 2071865  wchan=do_epoll_wait   syscall=[281 0x4 …]      (epoll_pwait, fd 4)
goroutine 1 gp=0xc000002380 m=nil [IO wait]:
    internal/poll.runtime_pollWait(...)
    internal/poll.(*FD).Read(...)
    net.(*netFD).Read(...)
    net.(*conn).Read(...)
    io.ReadAtLeast({0xd39160, 0xc000070208}, {0xc00024d240, 0x4, 0x4}, 0x4)
```

`io.ReadAtLeast(…, 4)` is `readRunnerAdmitFrame`'s length header — the admission
response read, under the transport deadline. A process in this state has **no**
thread reading fd 0, because the stdin goroutine has not been created yet.

Probes A/B/C/D were throwaway harnesses under `~/tmp/aira108/`; A and B are
promoted to committed regression tests (§4).

## 1. The three named candidate paths, traced — all correct

**(1) `admitThroughDaemon`'s transport deadline is reached and applied for a
`--pinned` reserve.** `confine-reserve` does not route through a lookalike path:
`internal/runner/confine_reserve_linux.go:36` calls `r.admitThroughDaemon`
**directly** (the verb is daemon-only by design — spec §4 — precisely so a
per-test reservation can never take the machine-wide flock). Inside it,
`admission_linux.go:341-358` derives `transportDeadline = now + maxWait +
admitTransportGrace` from the **requested** `maxWait`, and `admission_linux.go:429`
applies it via `conn.SetDeadline(...)` unconditionally, before the request frame
is written. It is cleared only at `admission_linux.go:617`, after a full
validated grant frame has been read — i.e. only once the wait is provably over.
`admitTransportGrace` is 1 s (`admission_linux.go:88`). Probe D shows the read
this deadline governs; probe B shows it (or the daemon timer) firing.

**(2) `confine-reserve`'s own wait loop, end to end.** There is no second loop.
`DefaultConfineReserveMaxWait` (`confine_reserve.go:12`) is 300 s; the CLI parses
`--max-wait` at `cmd/aira/main.go:1225-1231` (refusing `<=0` and `>30m`);
`confine_reserve_linux.go:20-25` wires it into `Runner.admissionMaxWait`. The
poll loop in `admitWithFlock` (`admission_linux.go:~299`) — the only other wait
in the runner — is never entered by this verb. The single multiplicity is the
two-attempt too-large clamp retry (`confine_reserve_linux.go:33`), whose first
attempt is an *immediate* daemon rejection (`admit.go:1129-1132`, `:1136-1140`,
both before any wait timer is armed), so it adds no wait. Worst case is
`maxWait + 1s` plus one round trip, not `2 × maxWait`.

**(3) The daemon has a matching bound.** `admit.go:1161-1180` computes
`remaining = max_wait_ms − since(waiter.enqueued)` and selects on that timer;
expiry runs `timeoutAdmitWaiter` → `E_ADMIT_SATURATED` (`admit.go:1197-1213`).
The daemon's hold *after* a delivered grant (`admit.go:1236-1241`) is
deliberately unbounded and is the correct mirror of the client's stdin hold — the
reservation must last as long as the test it was granted for.

## 2. What the real defect is

### D1 — the reservation ledger is diagnosis-hostile (the defect that cost the hours)

`aira confine --list` reports the entire scope-less population as **one opaque
aggregate** (`cmd/aira/main.go:2624-2632`):

```
  of which: 0 confine scopes 0B, 5 scope-less reservations 5751380K, 2 adopted scopes 2706016K
```

No per-reservation signature, no age, no state. And a *granted* helper is
byte-identical in `ps` to a *waiting* one — same argv, same `--max-wait 300s`. So
an operator who sees a long-lived helper **cannot establish which of the two
states it is in from any AIRA surface**, and the only inference left is the wrong
one. 5.5 GB of a shared 62 GB machine-wide ceiling was pinned by *something* and
nothing could say what.

This is the **second** false P0 from this exact blind spot. The first is recorded
in AIRA-68's own comment at `internal/runner/confine_manage.go:100-114`:
*"Comparing Jobs against len(Scopes) is therefore invalid, and doing so is what
produced AIRA-68's P0 '23 admitted jobs, only 3 live scopes' report: 20 of those
were healthy per-test reservations."* AIRA-68's fix added the aggregate line
above; the aggregate line is demonstrably not sufficient.

By AIRA's own honesty bar this is a defect: the surface presents a number that
invites a false diagnosis while withholding the fact that settles it, when the
daemon already holds that fact in memory (`admitWaiter` carries `grantedAt`;
`admitRequest` carries `signature`).

### D2 — the wait bound has no *real-time* coverage anywhere in the tree

Existing coverage asserts the *shape* of a refusal, never that the wait ends at
the bound: `TestConfineReserveSaturatedIsTerminalNoGrant`
(`internal/runner/confine_reserve_linux_test.go:191`) drives a `net.Pipe` daemon
that answers instantly; `cmd/aira/confine_test.go:354` stubs `reserveConfined`
entirely and dwells 20 ms. So the regression AIRA-108 alleges would today be
invisible to the whole suite — which is exactly what made the allegation
unfalsifiable from inside the repository. (Precision, per review: the logic *is*
unit-tested; what is absent is any assertion that the bound holds **in real
time**, which is the only kind that could have answered this ticket.)

## 3. Scope

In scope:

1. `admitWaiter.signature` — store the signature `admitRequest` already carries
   (`admit.go:1313`, one field).
2. `admitSnapshot` gains per-reservation rows for **granted, accounted,
   scope-less** waiters — signature, reserve, held-since — gathered in the
   **same existing locked pass** over `queue.waiters`
   (`admit.go:786-880`), so rows and totals can never come from different
   instants. Rows are **copied** under the lock; sorting, capping and formatting
   happen **outside** it (review point).
3. `runner.ConfineSliceReserve.Reservations []ConfineReservationHold`
   (`internal/runner/confine_manage.go`), populated in
   `internal/daemon/confine_manage.go:126-160`. Each row carries an explicit
   `State: "holding"` — a row must *say* what it is rather than leave the reader
   to infer it from the section it appears under (review point).
4. `aira confine --list` renders the rows **longest-held first**, capped at the
   10 longest-held with an honest `… and N more` line, only when
   `ReservationJobs > 0`. The signature is **untrusted text from a client**, so
   it is truncated and non-printables escaped before rendering (review point).
5. `--max-wait` help text (`internal/core/core.go:1733`) states the bound is on
   **admission only**; `runConfineReserveCommand` carries a comment recording
   this ticket's finding, including the **inherited-stdin** property: any
   descendant that inherits the write end keeps the reservation alive after the
   original parent exits (review point).
6. Tests, §4.

Explicitly **out of scope**, with reasons:

* **Any TTL, reaper or liveness probe for a granted scope-less reservation.** Two
  reviewers named its unbounded lifetime as the residual hazard, and that is
  accepted and recorded on the ticket as a follow-up — but not built here: it
  would kill legitimately long tests, the reservation is already released
  deterministically when its holder exits or closes stdin, and
  `[[architectural-simplicity]]` is explicit that a documented gap beats new
  machinery. Expose it first (D1); decide on policy later with the data D1 gives.
* **Holder pid via `SO_PEERCRED`.** Needs new peer-credential plumbing through
  the admit connection. Signature + age already identifies the holder for the
  diagnostic purpose. Deferred and recorded.
* **Anything in fastest-ee.** Its conftests still register the plugin AIRA-33
  deleted from *this* repo; that migration is theirs and is already recorded on
  AIRA-108.
* **"Fixing" the observed helper's longevity.** It is correct behaviour; changing
  it would silently un-reserve running tests.

## 4. Tests

All new. T1/T1b/T2/T3 are real-cgroup, real-daemon, real-CLI-subprocess,
real-wall-clock — the combination that did not exist for this verb. Every timing
assertion uses monotonic timestamps taken in-process.

* **T1 — a saturated wait is bounded** (promotes probe B). A private cgroup with a
  real `memory.max`; a holder takes the ceiling; a second real `aira
  confine-reserve --max-wait D` subprocess must exit **non-zero**, print
  **nothing** on stdout, name `E_ADMIT_SATURATED` on stderr, and do so no earlier
  than `D` and within a generous, scheduling-tolerant upper bound. Fails if the
  bound is ever removed.
* **T1b — the CLIENT transport deadline alone is sufficient** (review addition,
  and the direct test of AIRA-108's own candidate (1)). A real unix listener that
  accepts the connection, reads the request frame, and **never replies**. The
  helper must still exit non-zero at about `D + admitTransportGrace`. T1 alone
  could pass on the daemon timer while the client deadline was broken; this
  isolates the half the ticket accused.
* **T2 — a granted hold outlives the bound** (promotes probe A). A fitting
  reservation is still alive at `3 × D`, then exits 0 within seconds of stdin
  close. Fails if anyone "fixes" AIRA-108 by expiring granted holders — which
  would silently un-reserve running tests.
* **T3 — `confine --list` names the held reservation.** While T2's helper holds,
  the confine-list result carries a row with `State == "holding"`, that exact
  signature, a positive reserve, and a held-duration ≥ the dwell. Mutation check
  on build-review: deleting the row emission must turn this red.
* **T4 — rendering.** Golden render of the new rows including the 10-row cap, the
  `… and N more` line, and a signature containing control characters, extending
  `cmd/aira/confine_reserve_breakdown_test.go`.

**False-fail direction.** T1's lower bound is exact (a correct build cannot exit
early) and its upper bound carries all the slack, because host load can only
delay an exit. T2's dwell is safe in the same asymmetric way as
`grantedRelayHoldDwell` (`internal/daemon/worker_admit_cli_granted_linux_test.go:41`):
a correct helper is parked in `io.Copy` on a pipe nobody has closed and cannot
exit under any load, while a helper that lost the hold returns microseconds
later. Hosts without delegated cgroups skip via `cgrouptest.SkipOrFailRealCgroup`.

**Accepted coverage gaps, stated rather than papered over.**

* T2 proves the lease is still held at `3 × D`; it does **not** prove it is held
  until stdin EOF — a regression releasing it at ten minutes survives. That is
  the identical, already-accepted bound on `grantedRelayHoldDwell`, and closing
  it needs a production seam announcing lease state, which this project does not
  add for a telemetry-grade signal.
* No test can establish what pid 1736618 did historically. T1/T1b are what would
  catch the alleged regression if it were ever real.

## 5. Risks

* **T1 saturates a queue.** It uses a private cgroup created under the delegated
  user tree and removed in cleanup, never `aira.slice`, so no other session's
  admission is affected. The same isolation was used for the live probes.
* **Snapshot cost.** The rows are gathered in a loop that already walks
  `queue.waiters` under `queue.mu`; no new lock, no new pass, no new scan. Row
  collection is bounded by `admitMaxWaiters`, which already bounds that slice.
* **Output growth.** Capped at 10 rows plus one count line.

## 6. Expected yield

The next operator who sees a long-lived `confine-reserve` runs one command and
gets a positive answer — *"holding, signature `pytest:…::test_x`, held 52m"* —
instead of the multi-hour, two-session, `/proc`-level investigation that produced
this ticket, and instead of a mis-filed P0. Plus the first real-time coverage of
the admission bound itself, in both directions and on both enforcing mechanisms.

## 7. Honest statement of what this plan claims

It **does** claim, on live evidence: the two states are distinguishable and were
distinguished; the inspected process was in the granted-and-holding state; the
bound is enforced today by two independent mechanisms, measured; the ticket's
`/proc` reading was inverted; the mechanism it names is not present in the code.

It **does not** claim historical impossibility for pid 1736618, and it does not
claim to have reproduced fastest-ee's original suite wedge. Why that caller's
test stopped progressing is outside this repository — the wedged helper was a
*symptom* faithfully holding a valid reservation, and the AIRA-side plugin that
spawned it is already deleted (AIRA-33). If a future report shows a
`confine-reserve` with the **`do_epoll_wait` / no-`anon_pipe_read`** signature
alive past its bound, that is a genuinely different fault, and T1/T1b are the
tests that would have caught it.

## 8. Review record

* **Sol (GPT-5.6, `codex exec`, reasoning=high)** — `APPROVE-WITH-CHANGES`. Four
  changes, all adopted: (1) do not claim the bug "does not exist"; the inspection
  proves the state at inspection time, not the history — reframe as "the evidence
  does not substantiate"; (2) `wchan` alone does not identify the fd — bind it
  with `/proc/<tid>/syscall` or a goroutine dump (done: probes C and D), and fix
  the thread-count discrepancy; (3) D1 is proportionate — but label rows
  `state=holding`, escape/truncate untrusted signatures, copy under lock and
  sort/render outside it, cap on the *longest-held*, and document the
  inherited-stdin risk; (4) strengthen D2 — T1 exercises the daemon timer but not
  the client transport deadline, so add a black-hole-listener test (T1b) and use
  scheduling-tolerant bounds with monotonic timestamps.
* **DeepSeek-v4-pro (effort=high)** — concurs. "Report fails substantiation;
  probes show granted holding not wait-bound gap — fix observability, not timer."
  Calls the fingerprint airtight given probes C+D; calls D1 proportionate; names
  the unbounded granted-reservation lifetime as the residual hazard to expose
  first and decide later (adopted as the recorded follow-up); warns against
  claiming absolute impossibility (adopted).
* **Gemini (flash-latest)** — concurs. "Claim is sound; AIRA-108 is an invalid bug
  report caused by an observability gap, not a broken wait timer." Raised the
  unverified-fd objection independently (answered by probe C) and flagged the
  first draft's "no coverage" phrasing as an overclaim (tightened to "no
  *real-time* coverage").
* **Fable plan gate — NOT RUN.** This session has no sub-agent dispatch tool, so
  the project's usual Fable gate could not be invoked. Three independent
  lineages were used in its place; recorded here rather than implied.

## 9. Addendum — the ticket's "ROOT CAUSE FOUND" section, tested

While this work was in progress the ticket gained a section headed **"ROOT CAUSE
FOUND (split, SIGQUIT goroutine dump on the held live instance)"**, reporting the
dump this plan had asked for and reading it as:

> The **main** goroutine … is parked in a `select` at `cmd/aira/main.go:1295` …
> That select's case set does not include a case that fires on the request's own
> `--max-wait` deadline … so it blocks on grant-or-cancel forever regardless of
> the declared bound.

**That dump is the same evidence as before, and it says the same thing: the
process had already been granted.** The select it names is the POST-GRANT HOLD —
`select { case <-done: case <-signalCtx.Done(): }` — and reaching it is proof the
admission wait completed successfully:

* Every earlier return in `runConfineReserveCommand` exits the process. The only
  way to reach that select is through a successful `reserveConfined` and the
  `granted reserve=… basis=…` line already written to stdout.
* The dump's own "companion helper goroutine created around main.go:1291-1292 in
  the same block" is the `io.Copy(io.Discard, stdin)` goroutine — which is
  *created by the lines immediately above that select*, and therefore cannot
  exist before the grant.
* Probe C reproduced this exact stack from a **demonstrably granted** helper
  (grant line read by the parent at t=0.00s), together with the fd binding that
  clinches it: `/proc/<tid>/syscall` = `[0 0x0 …]`, i.e. `read(2)` on fd 0.
* Probe D reproduced the stack the WAITING state actually has, and it is not a
  select at all: `goroutine 1 [IO wait]` → `net.(*conn).Read` ← `io.ReadAtLeast`,
  with a thread on `epoll_pwait`.

So the select does not "fail to race `--max-wait`"; racing `--max-wait` is not its
job. `--max-wait` was already enforced, upstream, by the two mechanisms §1 traces
and §4 now pins.

### The directed fix is a regression, and it is caught

The ticket goes on to direct: *"The correct fix almost certainly reuses the
pattern `internal/runner/admission_linux.go`'s own poll loop already uses …
(a timer/context case derived from the request's `MaxWait`)"*. Applied literally
— adding `case <-time.After(maxWait):` to that select — this is what happens:

```
--- FAIL: TestConfineReserveGrantOutlivesTheDeclaredBound (3.25s)
    a GRANTED reservation exited (<nil>) 2.014624618s after its grant, at or about
    its own 2s admission bound. --max-wait bounds ADMISSION ONLY; expiring a
    granted holder un-reserves a test that is still running
```

Every per-test RAM reservation would be silently released at 300 s while its test
was still running, and the ledger would then advertise capacity that running
tests are using — the aggregate over-admission class AIRA-67 exists to prevent.
That is strictly worse than the symptom being fixed, and it is why §4's T2 exists
and is stated as the more important of the two directions.

### Secondary finding: SIGQUIT — does not reproduce

The same section reports that *"SIGQUIT printed the goroutine dump but did not
terminate the process afterward"*, suspecting a missing `os.Exit` or a
`signal.Notify` swallowing it. Measured directly against this verb, with stdin
deliberately left open (probe E):

```
grant: b'granted reserve=67108864 basis=pinned:client\n'
after SIGQUIT (stdin still OPEN): rc=2 elapsed=0.10s
  -> goroutine dump captured: 15226 bytes, 22 goroutines
```

It dumps and exits in 0.10 s with status 2 — Go's ordinary uncaught-SIGQUIT
behaviour. `signal.NotifyContext` here registers only SIGINT and SIGTERM, so
SIGQUIT keeps its default disposition and nothing swallows it. No code change is
made for this. What split observed was real, but it was not this process
declining to die; the likeliest reading is that the signal and the liveness check
addressed different pids. Recorded as not-reproduced rather than fixed or
dismissed.
