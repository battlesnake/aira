# aitest — a fork+admission pytest worker pool replacing xdist

## 1. Problem

Multiple concurrent agent sessions/worktrees on this machine run large
`pytest -n auto` suites. The current mitigation is a bolt-on governor
(`internal/pylib/aira_xdist_governor`, a bespoke CPU park/active-set daemon
scheduler in `internal/daemon/governor.go`, and a per-test RAM reservation
against the `#67` admission ledger via `aira confine-reserve`) layered on top
of pytest-xdist's own worker model. It does not reliably avoid OOM and does
not use the machine well, for structural reasons the existing design docs
already name but haven't been able to fix by patching the governor further:

- **Cumulative worker RSS is undercounted by construction (AIRA-12).** An
  xdist worker's real footprint is its cumulative high-water mark across every
  test it has ever run in its lifetime (caches, fragmentation, module state),
  not any single test's estimate. Per-test reservation charges one test's
  estimate at a time while actual worker RSS keeps climbing underneath it — no
  per-test estimate, however accurate, fixes this. AIRA-12's own design doc
  explicitly weighed a hard per-worker cgroup cap against this and rejected it
  — but only because xdist owns worker spawn, so AIRA had no cheap way to wrap
  each execnet-spawned worker in its own scope.
- **Bootstrap is entirely ungoverned (AIRA-17/AIRA-26, open, unsolved).**
  xdist's execnet gateway spawns N interpreters and they import their full
  dependency tree *before* any pytest hook can fire — the exact moment of
  highest simultaneous memory/CPU pressure is invisible to every governance
  layer AIRA has. Both tickets are still open; both note the fix needs a
  pre-import gate, which the current hook-based architecture cannot provide.
- **Governance only fires for `--delegate-ram` launches.** Plain
  `aira run`/`aira confine -- pytest -n auto` gets `AIRA_PY_LIB` importable
  but no CPU or RAM governance at all — a real, silent coverage gap
  (`internal/pylib/env.go:57-115` vs `internal/core/command.go:170`).
- **Static reserve-for-lifetime wastes the machine (AIRA-27/28/29).** The
  general confine admission ledger holds a job's *estimated peak* for its
  entire lifetime; peaks are brief and rarely coincide, so the ledger
  saturates long before physical RAM does (measured: 33.6G reserved, 2.6G
  used, 62 minutes, while ~48G sat idle). AIRA-29 (in flight, branch
  `aira29-dynamic-reserve`, not yet merged) fixes this at the *whole-scope*
  granularity for general confine jobs. It doesn't and can't fix the
  worker-granularity version of the same problem, because xdist workers are
  invisible to the confine ledger — they're not separate cgroup scopes at all
  today.

Retrofitting xdist with more plugins keeps hitting the same wall: xdist's
worker lifecycle isn't ours to govern. This spec proposes owning it instead.

## 2. Goals / non-goals

**Goals:**

- Kernel-enforced, per-worker memory containment, so a leaking or
  mis-annotated test cannot threaten a sibling worker or the run as a whole.
- Admission driven by *live* measured occupancy, not held static estimates,
  so idle headroom actually gets used.
- Time/count/RSS-bounded worker recycling as a cooperative-multitasking
  mechanism across concurrent sessions, and as a hard backstop against
  Python's own memory fragmentation/cache growth — recycling returns
  everything to the OS unconditionally, unlike `gc.collect()`/`malloc_trim`.
- A general AIRA capability: opt-in per project, like the current governor,
  not AIRA's-own-suite-only.
- A daemon-side protocol phrased generically enough that a future non-Python
  test runner could speak it, even though pytest is the only client now.
- A simpler mental model: one supervisor that owns collection, admission,
  worker lifecycle and reporting, instead of a third-party scheduler with
  governance hooks bolted onto its edges.

**Non-goals for v1 (explicit deferrals):**

- `loadscope`/`loadgroup`-equivalent fixture-affinity grouping. v1 is a flat
  dynamic queue; add affinity only if a real suite needs it.
- A persistent worker pool that outlives a single `pytest` invocation.
  Amortising import cost across separate runs needs correctness-critical
  cache invalidation on code change — not worth it until the per-invocation
  model is proven.
- Any non-pytest client of the new daemon verb. The wire shape is generic;
  building a second client is future work.

## 3. Design

### 3.1 Components

- **Supervisor** — a new pytest plugin (package `internal/pylib/aitest`,
  replacing `internal/pylib/aira_xdist_governor`). On activation it performs
  normal pytest collection to get the full nodeid list, lets the suite import
  whatever it needs, calls `gc.freeze()`, then drives a dynamic nodeid queue.
  It never runs a test itself.
- **Worker** — forked from the supervisor (COW-shares the warm import state;
  no re-import cost). Loops: pull the next nodeid from the supervisor over its
  pipe → run it against one long-lived `Session`/collection tree (not a fresh
  `pytest.main()` per test) → stream the resulting `TestReport`(s) plus
  captured output back → after each test, and only then, check its own
  recycle conditions.
- **Daemon** — a new verb, `worker-admit`, on the existing verb-multiplexed
  Unix socket (`internal/daemon/server.go`, `serveConnection`,
  alongside `admit`/`governor`/`confine-report`/`confine-list`/`confine-kill`/
  `watch`). Grants placement for one worker; does not fork anything itself.

Activation is a new explicit flag (`--aitest-workers=N|auto`), not a
reinterpretation of xdist's own `-n` — a project with xdist installed for
unrelated reasons must not have its flag silently hijacked.

### 3.2 Why "full replacement" is smaller than it sounds

xdist's controller already keeps exactly the reporting shape we want: each
worker keeps one long-lived `Session`, runs items directly through the item
protocol, and streams `TestReport` objects for each phase back to the
controller, which replays them into its *own* real pytest hooks. That's how
`junitxml`/`terminalreporter` get full fidelity today without xdist
reimplementing pytest's reporting internals — it's pytest's own multi-process
support, merely orchestrated by xdist. We keep that shape unchanged. Coverage
combination is coverage.py's own parallel-mode (`COVERAGE_PROCESS_START`,
per-worker `.coverage.<suffix>` data files, `coverage combine` at the end) —
also not xdist-specific. What we actually replace is transport (fork+pipe
instead of execnet), worker spawn/recycle policy (aira-admitted,
time/count/RSS-bounded instead of "spawn N once, live forever"), and the
scheduler (RAM-aware dynamic pull instead of xdist's own).

**Accepted Slice 1 limitation: no look-ahead `nextitem`.** Real xdist (and
plain pytest) supplies each item's actual *next* item to the item protocol,
so a session/module/class-scoped fixture shared by consecutive tests is set
up once and persists across them. Slice 1's worker instead runs every item
with `nextitem=None` (pytest's own "this is the last item" signal), which
tears down and rebuilds the entire fixture stack after every single test —
a real behavioral deviation for suites with expensive or stateful
session-scoped fixtures, not just an internal-API uncertainty. Implementing
real look-ahead dispatch is deferred past Slice 1 (closely related to, and
no more urgent than, the loadscope/loadgroup fixture-affinity grouping §2
already defers); not needed to validate this slice's core admission/
lifecycle loop.

**Rejected alternative:** patching xdist's own scheduler via
`pytest_xdist_make_scheduler` and leaving execnet as transport. xdist's
gateway/spawn model hard-assumes all N workers are spawned once at session
start and live for the whole run — there's no seam for "spawn over time as
admission allows" or "recycle and replace a worker mid-session" without
patching xdist internals it wasn't designed to expose, on a dependency whose
release cadence we don't control. Rejected as the same retrofitting problem
this spec exists to get away from.

### 3.3 Admission model — nested scopes, live occupancy plus a worst-case aggregate guard

Two cgroup levels:

- **Outer scope** — the `aitest` run launches exactly like any other job
  under `aira confine --delegate-ram` today: unchanged slice-level
  `Σgranted ≤ cap−headroom`, unchanged `oom_score_adj=800` delegate class
  (`confine_linux.go:34-35`, AIRA-27, deployed). This scope automatically
  inherits AIRA-29's dynamic-reserve fix once it ships — no special
  integration needed, `aitest` is just a normal confine job at this level.
- **Per-worker sub-scope** — each `worker-admit` grant creates a child cgroup
  under the outer scope. This is the genuinely new piece, and it should adopt
  AIRA-29's chosen mechanisms rather than reinvent them, at worker instead of
  whole-job granularity:
  - Admission for a *new* worker is decided primarily against the OUTER
    scope's own **live `memory.current`** — a single read, not a sum of
    individually-read worker scopes: cgroup memory accounting is
    hierarchical, so that one number already includes the supervisor's own
    RSS plus every already-placed worker's, and re-summing the children
    separately would be both redundant with that accounting and unsafe
    (it can under-count relative to what the kernel's own
    `memory.oom.group` actually acts on). Re-evaluated each daemon tick —
    not a static peak held for the run's lifetime. This is what actually
    fixes the utilisation problem (AIRA-29's own numbers: 33.6G reserved
    vs 2.6G used for one job).
  - **Amended past the original design (build-review, Slice 1):** a
    live-usage-only admission is silent on the *sum of already-granted
    caps* — it can pack workers up to the ceiling while every one of them
    still has headroom left to grow toward its own `memory.max`, so if
    they all grew simultaneously the outer scope could still be pushed
    over its cap and trip its own `memory.oom.group`, killing the whole
    run (supervisor plus every sibling worker) — exactly the incident
    class this design exists to prevent (Goal 2), and the same
    aggregate-not-bound failure class AIRA-27/28/29 already fixed at
    whole-job granularity, found here at worker granularity instead. A
    second, worst-case guard therefore runs on top of — not instead of —
    the live-usage check: Σ(already-granted worker `memory.max`) plus the
    supervisor scope's own live `memory.current` must still fit under the
    ceiling before a new grant is issued. This trades a little
    utilisation (a live-usage-only check would admit a worker whose
    siblings simply haven't grown to their peaks yet) for that hard
    guarantee. Both checks are re-evaluated every poll tick, so a sibling
    worker retiring (freeing its committed cap) or shrinking (freeing live
    usage) can unblock a grant that either check alone was blocking.
  - **AMENDED by AIRA-35 (measured, 2026-09-06); supersedes the original
    `memory.high`/`memory.max` split.** Each worker scope gets `memory.max`
    (hard containment, with `memory.oom.group=1`) and **`memory.swap.max=0`**,
    and **no `memory.high` at all**. Two measurements forced this; both were
    taken with the probe now committed as
    `TestWorkerScopeOOMGroupKillConvergesPromptly` and its negative control,
    and the full tables are in
    `docs/superpowers/specs/2026-09-06-aira35-worker-oom-convergence-plan.md`.
      - *Without `memory.swap.max`, a worker scope did not contain a runaway at
        all.* cgroup-v2's `memory.max` bounds memory, not memory+swap, and swap
        defaults to unbounded. A 512 MiB allocation inside a 32 MiB
        `memory.max` was **never OOM-killed**: it was reclaimed into swap and
        exited 0, with ~520 MiB written to the swap device — reproduced at
        every cap size tested and for a slow leaker as well as a fast one. The
        original design's per-worker containment simply did not happen on any
        host with swap. It is also what makes the aggregate guard above sound:
        a Σ over `memory.max` cannot bound a footprint that escapes into swap.
      - *`memory.high` was a convergence livelock, and narrowing the gap only
        moved it.* At the original 80% split a deliberate leaker did **not**
        converge to its `oom.group` kill in **420 seconds** (5475 `memory.high`
        events, **zero** `memory.max` events — the kernel held the cgroup
        pinned below its own hard cap, which is precisely what `memory.high`
        promises to do). At a 95% split the delay tracks the *absolute* width
        of the throttle window and so grows with the cap: ~1 s at 32 MiB but
        **16–18 s at the 512 MiB cap this actually ships**. With no
        `memory.high` it is 0.03–0.48 s across that whole range. `memory.high`
        is designed for a cgroup whose *userspace* supervisor acts on the
        resulting pressure signal; nothing above a worker scope does, so it was
        a livelock by construction — and a process stuck in that reclaim path
        can enter an unkillable D-state (AIRA-35's original report).
      - *What the throttle was claimed to buy is provided elsewhere.* The outer
        scope's Σ is protected by the aggregate admission guard above, not by
        the throttle: a worker growing to its full `memory.max` is already
        reserved against the ceiling. The proactive-recycle watermark (§3.4) is
        a userspace comparison that needs a *number*, not a kernel throttle.
      - *What it did cost, stated plainly.* Workers may now occupy their full
        committed `memory.max` rather than being pinned ~20% below it, and
        their anon pages are unreclaimable, so post-grant slack against an
        outer-scope OOM is `workerAdmitHeadroomDefault` (64 MiB) plus the
        supervisor's live usage being re-read at every grant. The residual
        exposure is supervisor growth after the run's last grant — unchanged in
        size, changed in failure mode (previously it degraded via swap, now it
        reaches `oom.group`).
      - *Where the swap cap cannot be set*, the grant still proceeds — refusing
        would stall every aitest run on such a host — but carries `swap_cap`
        (`enforced` / `not-applicable` / `unavailable`) and the supervisor says
        so once on the suite's own output. `not-applicable` requires positive
        proof the kernel cannot swap; a failure to look is `unavailable`.
  - Sizing of a fresh grant's `memory.max` backstop is intended to come from a
    coarse per-suite history estimate (signature `pytest-worker:<suite-hash>`,
    reuse of `internal/store/confine_peak_history.go`'s existing table and
    `resolveAdmitReserve`'s p90-prior/no-history fallback, `admit.go:216-294`)
    — a safety backstop sizing the cap, **not** the admission signal; the
    admission signal is live occupancy as above. **Still deferred as of
    AIRA-35**: what ships is a flat `AIRA_AITEST_ESTIMATED_BYTES`, defaulting
    to 512 MiB (`internal/pylib/aitest/__init__.py`'s
    `_resolve_estimated_bytes`). Said explicitly because this paragraph read as
    though history-based sizing were live, and the difference is load-bearing
    now that a worker over its cap is killed rather than quietly swapped: the
    remedy for an under-sized worker is that one environment variable, which is
    what the OOM diagnostic names.

`worker-admit` request/response sketch (connection-held lease, same idiom as
`admitConnection`/`governorConnection`, `admit.go:384-522`,
`governor.go:648-748` — dies with the socket, background peer-read detects
early disconnect):

```
→ {"verb":"worker-admit","job_id":"...","outer_scope":"<cgroup path>",
   "signature":"pytest-worker:<suite-hash>","estimated_bytes":N,
   "max_wait_ms":N}
← {"state":"granted"|"denied"|"timeout"|"unevaluated",
   "class":"granted"|"contended"|"request-invalid"|"admission-unusable"
          |"placement-failed"|"contract-violation",
   "reason":"<stable token>","detail":"<free text, never parsed>","waited_ms":N,
   "scope_path":"<child cgroup path>","memory_max":N,
   "swap_cap":"enforced"|"not-applicable"|"unavailable"}
```

A future client speaking this wire shape (§3.8's generalisation goal) must
treat `unevaluated` as retriable, the same as `denied`/`timeout` — it means
the daemon is reachable and answered, only that a live memory read could not
be established this instant (AIRA's own "report unevaluated, never a fake
pass" rule applied to this check), never that there is no daemon to talk to
at all. Routing it into permanent unconfined fallback (the treatment correct
only for genuine unreachability, §3.7) would strip containment for the rest
of the run over what is usually a transient glitch — the one named exception
being the structural `unevaluated: unbounded` case §3.7 itself calls out.

**Outcome-class convention (AMENDED by AIRA-42; supersedes the prior
reason-string prefix convention).** The disposition is carried by `class`, a
field of its own, and clients match it by EXACT value. The earlier design put
it in a `reject:`/`fallback:` prefix on the `reason` string, which made a
prose prefix the load-bearing control-flow signal on both sides of a
process boundary; six recorded recurrences of "a new Go error shape reached
the Python classifier unrecognised" — each fixed by adding one more substring
probe, with the fallthrough default being "abandon RAM containment for the
rest of the run" — are why that convention is retired rather than extended.

The six classes and what each requires of a client:

| `class` | meaning | required client behaviour |
|---|---|---|
| `granted` | admitted | use the grant; `state` is `granted` and only then |
| `contended` | reachable, no room or no answer *right now* (`insufficient-headroom`, `aggregate-cap-exceeded`, `saturated`, `admit-slots-saturated`, `worker-scope-id-collision`, `outer-scope-unreadable`, `supervisor-scope-unreadable`, `worker-scopes-unreadable`, a late or severed reply) | retry; keep containment |
| `request-invalid` | waiting cannot change this answer, and the daemon is healthy — the daemon's own poll loop breaks on it immediately rather than waiting out `max_wait_ms`. Mostly static facts about THIS request (`exceeds-ceiling`, an argument rejection), but **also two daemon-side infrastructure facts** added by AIRA-39 (`worker-scope-create-failed`, `worker-id-space-exhausted`): the class names the DISPOSITION, not a diagnosis. Retrying a broken cgroupfs forever would stall every aitest run on the machine; `admission-unusable` would strip containment over a daemon that is answering fine | terminal for the affected work: mark unevaluated, never retry, never disable the daemon |
| `admission-unusable` | daemon-backed admission is not usable for this run (`dial-failed`, `protocol-version-mismatch`, `outer-scope-unbounded`) | fall back to unconfined for the rest of the run |
| `placement-failed` | the local cgroup placement mechanism failed: a forked child that never confirmed its `__placed__` ack. **No relay producer since AIRA-39** — the CLI no longer creates worker scopes, so this is raised by the supervisor's own fork/ack path; the class is retained so that disposition has one name across both sides | fall back, and do not blame the daemon in the diagnostic |
| `contract-violation` | the two sides disagree about the channel itself: an uncatalogued `state`/`class`, a self-contradicting pair, an unintelligible frame, or an error code this client does not know | terminal and loud — **never** resolved into "there is no daemon", which would strip containment on an unrecognised shape |

`reason` is a stable exact-match token whose spelling nothing branches on, and
`detail` is free text that nothing parses. A client that finds itself matching
substrings of either has reintroduced the defect this convention exists to
prevent. The two containment-stripping classes are exactly `admission-unusable`
and `placement-failed`; widening that set is a design change, not an
implementation detail.

The `aira worker-admit` CLI relay projects the same vocabulary onto one
machine-readable stdout line in every outcome, grant or not
(`aira-worker-admit state=… class=… reason=… [placement fields] [detail=…]`,
`detail` query-escaped), and adds the three states only a client can
establish: `unavailable`, `argument-invalid`, `placement-failed`. Its stderr
carries a human diagnostic that is never a classification input.

On `granted`, the supervisor places the forked worker into `scope_path`. This
reuses `RunConfineSetup`'s verified-placement handshake
(`confine_linux.go:1272+`, `clone3(CLONE_INTO_CGROUP)` + parent/child
release-pipe verification before any target code runs) only on the **Go
side** — the bootstrap step that relocates the supervisor itself out of the
outer scope (§3.4) is a Go CLI verb and factors real, shared cgroup-creation
code with `confine`. The **worker fork itself cannot reuse the atomic
handshake**: it is a Python-level `os.fork()` of the already-running,
warm-imported supervisor (the entire point being COW-shared interpreter
state), and Python's stdlib has no `clone3`/`CLONE_INTO_CGROUP` binding —
that mechanism is Go `os/exec`-only. The forked child instead calls
`place_self()` (a plain `cgroup.procs` write) immediately after `fork()`
returns, before any test code runs. This leaves a narrow, bounded window
where the child is transiently a member of the supervisor's scope rather
than its own.

**Corrected 2026-09-05 (AIRA-37): that window is *not* "pure interpreter
overhead", as this section previously claimed.** CPython's `os.fork()` runs
`PyOS_AfterFork_Child()` — and so every handler registered via
`os.register_at_fork(after_in_child=...)` — in the child *before* `fork()`
returns to Python, so such handlers execute inside the window, unplaced.
They demonstrably exist on real runs: `logging`, `threading` and `random`
register handlers at import and pytest imports all three, and AIRA's own
`aira_xdist_governor` registers one at module scope (which is exactly how
forked aitest workers disable that governor, AIRA-92). aitest itself
registers none. The window's real bounds are the two that survive: it still
ends before any *test* code runs, so no test-driven allocation happens in
it, and the **outer** scope's hierarchical cap still charges everything that
does allocate — what is briefly coarser is the granularity of containment,
not containment itself. *Outer* is precise, not loose: the unplaced child
sits in `.aira-supervisor`, which is deliberately given no `memory.max` of
its own, so the bound comes from the outer confine scope one level up, whose
finite `memory.max` is a hard precondition of the grant (`worker-admit`
refuses with `outer-scope-unbounded` otherwise) rather than an assumption.
The sharper hazard is fork-across-threads locking (a handler blocking on a
lock another thread held at fork time), bounded but not eliminated by the
supervisor being single-threaded.

Still accepted for Slice 1 (architectural-simplicity: a raw ctypes `clone3`
syscall would place atomically but would skip `PyOS_AfterFork_Child`, and
therefore the stdlib's own child-side lock reinitialisation, unless it
re-invoked it by hand — more machinery and a larger hazard, not a smaller
one); documented here rather than left implicit. `worker.py`'s `fork_worker`
docstring carries the full audit, and
`test_atfork_after_in_child_handlers_run_before_fork_returns` pins the
ordering.

### 3.4 Worker lifecycle and recycling

Recycle conditions are checked **only between tests, never mid-test**, per
the original ask:

- elapsed time budget,
- test count,
- and — new versus the original sketch — the worker's own `memory.current`
  crossing a configured fraction of its **`memory.max`** (AIRA-35; it was a
  fraction of `memory.high` until that file stopped being written, and the
  default was re-expressed from 80%-of-80% to 64%-of-`memory.max` so the
  trigger point is unchanged), so a worker heading toward its cap retires
  itself cleanly and reports back, rather than either silently accumulating
  until the kernel intervenes or (in the degenerate case) never recycling
  because it happened to stay just under a purely time-based trigger.

**How strong this is, stated honestly (AIRA-35).** It is a between-tests best
effort, not a guarantee that a worker retires before its cap. A worker that
grows within a *single* test reaches `memory.max` mid-test and is group-killed
instead. That was always true — the check only runs between tests — but the
`memory.high` throttle used to make such a worker's growth glacial, so it
usually survived to the next check point. Removing the throttle removes that
concealment: what it was providing was not containment but a slow leak dressed
up as a graceful retirement, at the price of minutes in an unkillable reclaim
path. A mid-test grower being killed and requeued is the normal, expected
outcome below, and the supervisor's unevaluated report names the cap that
killed it and the knob that raises it.

A worker that *is* OOM-killed by its own `memory.max` (annotation was wrong,
or a genuine leak) is a normal, expected event, not an incident — the
supervisor detects the exit and requests a fresh grant if queue work remains.

### 3.5 Test dispatch

Pull-based dynamic queue: an idle worker asks the supervisor for the next
nodeid over its pipe (mirrors xdist's own `load` distribution mechanism,
which already dispatches this way). No affinity grouping in v1 (§2).

### 3.6 Crash and retry semantics

The supervisor tracks an attempt count per nodeid. If a worker dies (kernel
OOM, host watchdog, any non-reporting exit) while a nodeid is in flight, that
nodeid is requeued once onto a fresh worker. A second failure marks it
`unevaluated` — a distinct outcome from a normal `failed` `TestReport`, never
silently folded into either pass or fail, per AIRA's existing rule that a
check which cannot establish its result reports `unevaluated`.

**"A worker dies" is observed, not inferred (AIRA-40).** Each worker's `pidfd`
sits in the supervisor's own `select()` set alongside its result pipe, so the
death of the tracked process is what triggers the requeue. The original
implementation instead inferred death from the result pipe reaching EOF, which
is unsound in one direction: a test that calls `os.fork()` itself (or uses
`multiprocessing`'s default `fork` start method on Linux) leaves the grandchild
holding a duplicate of the worker's result-pipe write end — `CLOEXEC` fires on
`exec()`, not on `fork()`, so nothing prevents the inheritance — and one live
write end keeps the pipe open no matter that the worker is gone. A grandchild
outliving an OOM-killed worker therefore withheld EOF indefinitely and the
whole run hung, alive and silent, with the promise above unkept. EOF is
retained as a second, corroborating trigger: it is sufficient in the common
case, arrives at the same instant there, and is all that is available on a host
too old for `pidfd_open` (Python 3.9+, Linux 5.3+), where the supervisor says
once on stderr that it is running without the independent check rather than
implying a guarantee it cannot make.

### 3.7 Daemon-unavailable fallback

`worker-admit` itself fails **closed** at the daemon layer — no
flock-equivalent substitute inside the daemon, matching the precedent already
set by `confine-reserve` (`confineReserveWithRunner`,
`confine_reserve_linux.go:31-70`: daemon-only, returns
`E_CONFINE_UNAVAILABLE` rather than falling back). But the supervisor, like
today's CPU/RAM governor plugin on its own timeout/error path, degrades
**open**: on `worker-admit` being genuinely unreachable, emit a visible
warning on the suite's own output (not just a log line — a suite running
this way has silently lost containment for the rest of the run) and cap the
fallback pool at **`n_workers ≤ min(requested worker count, NumCPU)`** (owner
decision — proportional to the host, not a guessed constant, and never
grown beyond what `--aitest-workers` actually asked for) with **no cgroup
placement at all** — identical in spirit to "prints `continue`, worker runs
ungoverned" in the current plugin. This is deliberately not a new
half-daemon local-cgroup mechanism; it reuses the exact fail-open shape the
existing governor already has, just applied to worker spawn instead of
per-test checkpoints.

**Genuinely unreachable, not merely declined.** The permanent, whole-run
fallback above triggers when there is no daemon to talk to at all — a
dial/connect failure, the CLI itself failing to launch, or a malformed
response — **or** when the LOCAL cgroup mechanism itself is what failed:
the daemon granted admission (reachable, healthy) but the child never
confirmed its own placement into the granted scope (a forked child dying
before its `__placed__` ack). **AIRA-39 removed the second member of that
second category**: the CLI no longer calls `CreateWorkerScope` at all — the
daemon creates the scope inside the same critical section that decides to
grant — so a creation failure is now a daemon verdict
(`worker-scope-create-failed`, class `request-invalid`: terminal for the
queued work, daemon still available, containment NOT stripped) rather than a
local placement failure discovered after a grant. The forked-child ack path
is what `placement-failed` now covers. Both are deliberate fallback
triggers: a daemon that answers is not the thing that's broken in either
case, but the cgroup mechanism itself demonstrably is, and there is no
"try again" that fixes a locally-broken placement path the way waiting
out contention fixes a `denied`. It must NOT trigger on an ordinary
`denied` (budget exhausted right now), `timeout` (the daemon is up, just
busy/contended, and the request waited out its own poll window), or
`unevaluated` (a live memory read momentarily failed on an otherwise-live
daemon) response from a daemon that is plainly still there: those mean
"don't add a worker at this moment," not "abandon containment for the
rest of the run." **One `unevaluated` sub-case IS a fallback trigger,
not a transient one, and is genuinely structural**: an outer scope whose
own `memory.max` reads back `"unbounded"` (reason `unbounded`) can never
become capped by waiting, since a real confine-launched outer scope is
always given a finite `memory.max` by the daemon as part of the same
atomic grant that launches it — before it is ever queryable. This
specific shape signals the client asked about a scope that isn't a real,
daemon-admitted outer scope at all (found live: a second aitest-enabled
pytest invocation nested inside one confine job can discover a prior
run's own uncapped supervisor scope as its "outer"), so it is classified
alongside the two triggers above rather than retried forever. **That
discovery step no longer exists as of AIRA-44**: the confine launcher
publishes its own scope as `AIRA_AITEST_OUTER_SCOPE` and the bootstrap
verb uses it instead of self-discovering from its current cgroup, so a
second invocation in one job now bootstraps against the real outer scope
and gets genuine per-worker admission. The `unbounded` classification
stays exactly as specified — it is now a backstop for a hand invocation
or a launcher that publishes no coordinate, not the expected path for
this scenario. Conflating
fallback-worthy conditions with merely-declined ones (exactly what an
earlier revision of this design did)
defeats the point of admission — a single contended moment would
otherwise permanently strip containment from everything after it. The
client-side exception types this distinction requires
(`WorkerAdmitUnavailable`, `WorkerPlacementFailed` — both fallback-
triggering; `WorkerAdmitDenied`, and the `WorkerAdmitTerminal` pair
`WorkerAdmitRequestInvalid`/`WorkerAdmitContractViolation` — all
retriable/terminal-but-scoped, never fallback-triggering) are an
implementation detail of the supervisor, not a wire-protocol change. **Since
AIRA-42 the supervisor no longer DERIVES that distinction at all**: it reads
the `class` field of the §3.3 outcome channel and maps it to one of those
types by exact value, so the mapping table is the whole classifier.

### 3.8 Deletion, retention, generalisation

**Deleted outright** (no back-compat burden — AIRA has no users/data):
`internal/pylib/aira_xdist_governor`, `aira governor-slot`
(`internal/runner/governor_slot.go`), the pytest call sites of
`aira confine-reserve`, `internal/daemon/governor.go`'s park/active-set
scheduler, and the three `2026-08-30` scheduler-slice specs. This entire
stack exists only to govern xdist workers `aitest` replaces outright.

**Kept, unchanged, as foundation:** the daemon socket/frame/verb-dispatch
machinery, the `#67` cgroup-scope-creation and placement-handshake code
(factored into a shared function per §3.3), the peak-RSS history table
(repurposed to key on worker-suite signature), `oom_score_adj` classing, and
the outer-scope admission ledger (`admit`, `checkedAvailable`, headroom math)
— entirely untouched; `aitest`'s outer scope is just another confine job to
all of this.

**Generalised:** `worker-admit`'s wire shape (job/signature/bytes in,
scope/caps out) carries nothing pytest-specific, so a future non-Python
runner could speak it without a protocol change.

## 4. Hard invariants

- A worker never runs test code before its cgroup placement is verified —
  no test ever executes outside a real, checked scope (fallback mode in
  §3.7 is the sole, explicitly-labelled exception, and it places zero
  cgroup expectation on itself). Verification is NOT the same two-way
  handshake `confine` itself uses for a top-level launch (that requires
  `clone3(CLONE_INTO_CGROUP)` at process-creation time, a Go/`os/exec`-only
  mechanism — see §3.3's own correction) — a worker instead confirms
  placement by writing its own pid into the granted scope's
  `cgroup.procs` and sending one `__placed__` acknowledgement down its
  result pipe before running any test code; the supervisor treats a
  worker that dies before that ack as `WorkerPlacementFailed`, distinct
  from a mid-test crash.
- A killed-mid-test outcome is never reported as `passed` or silently
  dropped; it is `unevaluated` after one failed retry (§3.6).
- Recycle checks fire only at test boundaries; a running test is never
  interrupted.
- Per-worker `memory.max` is always ≤ the outer scope's own cap, and new
  worker grants are evaluated against BOTH the outer scope's live occupancy
  AND a worst-case guard on Σ(already-granted worker caps) plus the
  supervisor's own live usage (§3.3, amended past the original
  once-computed-static-split framing by build-review) — never a
  once-computed static split. A job that isn't using its granted headroom
  yields it to sibling worker grants within the same tick cadence AIRA-29
  establishes for the outer ledger, but a grant that would push the WORST
  case over the ceiling is refused even while live usage still has room.

## 5. Staging

1. **Slice 1 — core loop.** Supervisor, fork+admission+recycle, flat pull
   dispatch, pass/fail/unevaluated aggregation only (no JUnit XML, no
   coverage combine yet). Enough to validate the admission/lifecycle
   machinery against a real cgroup tier before investing in output fidelity.
2. **Slice 2 — output fidelity.** JUnit XML via the replayed-session
   mechanism (§3.2), coverage.py parallel-mode combine, retry/crash semantics
   wired end to end (§3.6). This is what closes the "full xdist-equivalent
   output" bar.
3. **Slice 3 — fallback + generalisation polish.** Daemon-unavailable
   fallback (§3.7), `worker-admit` wire-shape review for the generality goal,
   watermark tuning from Slice 1/2 field data. (AIRA-35 retired `memory.high`
   for worker scopes entirely, so the only tuning surface left here is the
   `memory.max` watermark fraction.)
4. **Slice 4 — retirement.** Delete the governor stack (§3.8) once Slice 2 is deployed
   and AIRA's own suite has run on `aitest` clean — AIRA's own dogfood suite
   is the first real migration, not a separate project.

## 6. Open questions

- Exact worker-suite "signature" definition for peak-history keying (repo
  path hash? conftest content hash? explicit marker?) — needs a build-time
  decision, not a design-time one; low risk either way since it only sizes
  the backstop cap, not the admission signal.
- Exact `memory.max`-crossing fraction for the proactive-recycle watermark —
  needs field data from Slice 1, not a guess now. (Re-pointed from
  `memory.high` by AIRA-35, which deliberately preserved the effective trigger
  point rather than folding an unmeasured retune into a convergence fix. Owned
  by AIRA-32.)
- ~~Exact size of the daemon-unavailable fallback pool~~ — resolved
  (owner, 2026-09-01): `n_workers ≤ NumCPU`, visible on-output warning (§3.7).
- Whether any other AIRA workload still has a legitimate use for
  `internal/daemon/governor.go`'s CPU park/active-set machinery once `aitest`
  ships. Everything found during investigation for this spec points to it
  being pytest-worker-specific (built as `#49`'s replacement, consumed only
  by the plugin this spec deletes) — but confirm with a grep sweep for other
  callers before deleting it in Slice 4, rather than assuming from this spec
  alone.
- `aitest`'s relationship to AIRA-29 is additive, not competing (§3.3) — but
  AIRA-29 is still mid-review (v3 on branch `aira29-dynamic-reserve`, not
  merged) as of this writing; if its ledger model changes materially before
  `aitest`'s build starts, re-check §3.3 against whatever ships.

## 7. Test plan

- **Supervisor logic**, in isolation from a live daemon: queue/dispatch/
  retry/recycle-trigger logic against a fake `worker-admit` responder —
  fast, no cgroup dependency, covers the state machine exhaustively.
- **Placement and containment**, in AIRA's existing confined-integration
  tier: a worker that genuinely exceeds its `memory.max` is actually
  OOM-killed and the run recovers (requeue-once → `unevaluated` on second
  failure); a worker crossing its `memory.max` watermark self-recycles before
  that *where it reaches a between-tests check point at all* (§3.4);
  Σ(worker sub-caps) never exceeds the outer scope's cap under concurrent
  admission.
  **The first of those depends on `memory.swap.max=0` (§3.3), and the test
  asserting it must NOT pre-cap swap on an ancestor** — AIRA-35 found the
  original e2e doing exactly that, which made it prove the harness rather than
  the product: it passed identically whether or not production capped worker
  swap, and production capped none.
- **Adversarial cases:** daemon down for the whole run (fallback path, §3.7);
  daemon dies mid-run (in-flight worker grants must not orphan); a worker
  exits for recycling at the exact moment it's mid-request for a new nodeid
  (no dropped/duplicated test); two nodeids racing the same
  requeue-after-crash path don't double-count.
- **Reporting parity:** JUnit XML and coverage output from an `aitest` run
  diffed against the same suite's current xdist output for outcome/duration
  fields, on a fixed seed suite, to catch replay-fidelity regressions.

## 8. Deploy

Flag-gated new entry point (`--aitest-workers`), not a coexistence mode with
the old governor — the old stack is deleted per §3.8 once Slice 2 lands.
AIRA's own test suite is the first migration (dogfood), proving the
mechanism under AIRA's own real, large, RAM-annotated suite before any other
project is told to adopt it.
