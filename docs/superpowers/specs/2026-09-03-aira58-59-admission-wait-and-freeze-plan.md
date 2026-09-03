# AIRA-58 + AIRA-59 — honest admission wait ceiling, and a duty-bounded fairness freeze

Status: plan (v5 — after four adversarial gate rounds across two independent
lineages (Codex/Sol, Fable), plus two HIGH findings relayed from the PR #7
architectural review. Changelog in §11.)
Tickets: AIRA-58 (P1), AIRA-59 (P1) — `relates`, same subsystem, compounding.
Touches: `internal/daemon/admit.go`, `internal/daemon/worker_admit.go`,
`internal/daemon/paths.go`, `internal/daemon/server.go`,
`internal/daemon/protocol.go`, `internal/daemon/confine_manage.go`,
`internal/runner/admission_linux.go`, `internal/runner/confine_linux.go`,
`cmd/aira/main.go`, and a correction note on
`docs/superpowers/specs/2026-08-27-admission-backfill-gc-design.md`.

## 1. Why these are one piece of work

Not merely "same file". The original backfill design states the coupling itself
(`2026-08-27-admission-backfill-gc-design.md:61`):

> head delay — bounded by the pre-freeze jobs' completion and ultimately by the
> 30-min cap → honest rejection — for utilisation.

So `admitWaitCapMs` (the AIRA-58 bug) is **load-bearing for the freeze's blast
radius** in the freeze's own correctness argument. Both review lineages verified
this independently. Consequences:

1. Fixing AIRA-58 naively — honouring `--admit-timeout 2h` — **makes AIRA-59
   strictly worse**, moving the worst-case slice-wide stall from 30 minutes to
   whatever any one caller asks for. AIRA-59's suggested direction (3) is
   therefore only half right: its *fast-fail* reading bounds the damage, its
   *honour-the-timeout* reading amplifies it.
2. AIRA-59 cannot be fixed by tuning numbers: the defect is that **the freeze has
   no bound of its own** — it borrows one from a caller-controlled knob.

The fix must **decouple** them: give the freeze its own bound so the wait ceiling
can become honest without amplifying the freeze. Neither half may ship alone
(§9.3).

A third strand joins them (§4.2): the wait ceiling and the timeout path both feed
the **flock fallback**, which today launches jobs with no per-scope cap. Getting
AIRA-58 wrong does not merely delay a job — it can route it into an unaccounted
launch. That is why the fallback is in scope rather than filed away.

## 2. Confirmed mechanisms

### 2.1 AIRA-58 — THREE silent clamps, not one

`internal/daemon/admit.go:25` declares `admitWaitCapMs = 30*60*1000`, applied by
silent substitution at `admit.go:905-913` and `worker_admit.go:349-350`.

**The ticket is wrong that "the client-side flag threading is correct".** It
traced `confine_linux.go:831-833` but missed `internal/runner/admission_linux.go:82`:

```go
runnerAdmitWaitCap = 30 * time.Minute
```

clamped at `:252-255` **before the request is sent**, with the transport deadline
derived from the capped value at `:256-262` and the wire `max_wait_ms` written
from it at `:342`. A daemon-only fix therefore does not fix AIRA-58 at all:
`--admit-timeout 2h` still reaches the daemon as 30m while every daemon-side test
passes. Found independently by the Codex/Sol gate, by the Fable gate, and by the
PR #7 architectural review (as "B1") — and verified in source here. It was v1's
worst defect.

Nothing in the code justifies the 30-minute figure; the backfill design uses it
as a *consequence*, not a policy.

### 2.2 AIRA-59 — the freeze, and where the big head waiters really come from

`evaluateAdmitQueue` (`admit.go:711-729`) walks waiters in FIFO `seq` order. For
the first queued waiter with `reserve > available` it sets `waiter.waited`, and
once `now - enqueued >= s.admitBackfillGrace` sets `frozen = true`, after which
**every** later waiter is skipped for the rest of the pass regardless of whether
it would fit. `paths.go:36`: `defaultAdmitBackfillGrace = time.Minute`.

The freeze is per-pass but re-arms every pass while the head still does not fit,
so it is *effectively continuous* until the head leaves the queue — i.e. until
its own `max_wait_ms` expires. That is the bug: **the freeze's duration is the
head waiter's timeout.**

**CORRECTION to v1/v2 — the source of the 32G heads (verified in source).**
v2 claimed delegate-ram jobs keep a small pinned reserve and "are not the big
heads". That is **wrong**. `cmd/aira/main.go:857-859`:

```go
if maximum > 0 {
    reserve, reservePinned = maximum, true
}
```

is unconditional, has no delegate-ram guard, and **overwrites an explicitly
parsed `--memory-reserve`**. It runs in the CLI, so the runner's
`!request.DelegateRAM` guard at `confine_linux.go:459-461` is **dead for every
CLI caller**. So `aira confine --delegate-ram --memory-max 32G --memory-reserve
512M` really does reserve **32G**, pinned.

This matches the field evidence exactly:
- AIRA-58's repro was rejected with "(reserve **32G**/unknown)" despite
  `--memory-reserve 512M`.
- AIRA-59's observed ledger "**33280M** granted" = 32768M + 512M precisely.

The live giant heads are delegate-ram merge-gates reserving their whole scope cap
*in addition to* their per-test `confine-reserve` children — the double-booking
the delegate-ram overhead reserve exists to avoid (`confine_linux.go:449-454`).
It is also an AIRA-58-class silent override: *asked for 512M, silently got 32G.*

**Its own ticket, not folded in** (§8): it is a memory-accounting change in the
**over-commit direction** (32G → 512M of ledger charge) whose safety rests on a
separate argument about whether the per-test children fully cover suite usage —
including when the governor is off or the payload is not pytest. AIRA-59's fix
helps regardless of how the big heads arise.

### 2.3 Live evidence (from the coordinator)

An unrelated session's `aira confine --delegate-ram --memory-max 32G
--memory-reserve 512M -- make merge-gate`, with many
`aira confine-reserve --bytes ~900M-1G --pinned --signature pytest:<test>
--max-wait 300s` children — one per pytest test, from the RAM governor — each at
0% CPU (correctly blocked) but **3-4+ minutes elapsed each**, on a slice with
multi-GB headroom.

The blast radius is worse than filed: per-test reservations enter the **same
machine-wide per-slice queue**, so hundreds of small waiters freeze behind one
large head. And uniform-size queues are *not* self-freezing — if every waiter
wants ~1G and the head does not fit, none fit, so the freeze costs nothing. The
damage needs **size diversity**: a large head plus small waiters. That is the
observed shape and what §7.3 must reproduce.

### 2.4 Path verification (done, not assumed)

- **`confine-reserve` shares the freeze loop.** `cmd/aira/main.go:675` →
  `runner.ConfineReserve` → `confine_reserve_linux.go:38` → `admitThroughDaemon`
  → verb **`admit`** (`admission_linux.go:337`) → `server.go:569-578` →
  `admitConnection` → `enqueueResolvedConfineAdmit` → the **same** `sliceQueue`
  and freeze loop. No parallel implementation.
- **Caller** is `internal/pylib/aira_xdist_governor/__init__.py:353`, per test,
  reserving `max(aira_mem marker, measured RSS + 128M)` (`__init__.py:261`).
- **`worker-admit` is a separate mechanism, NOT affected by §5.**
  `evaluateWorkerAdmit` (`worker_admit.go:157`) never touches a `sliceQueue` and
  has **no freeze logic**. It is the `aitest` supervisor's path, not the
  governor's. Its lack of arrival ordering is a different exposure class (§8).
- **AIRA-58 affects both** — the clamp is in `validateWorkerAdmitArgs` too.
- **A third, already-honest ceiling**: `cmd/aira/main.go:709-716` *refuses*
  `confine-reserve --max-wait` outside `(0,30m]`. Not a dishonesty bug.

## 3. Invariant that must not move

`Σ(granted reserve) ≤ cap − headroom`, via `checkedAvailable` +
`queue.outstanding`/`outstandingJobs` recomputed **after each grant inside the
same locked pass** (`admit.go:715-717`, `740-741`), the fail-closed slice-read
return (`admit.go:699-709`), and the ceiling re-check in `enqueueAdmitInternal`
(`admit.go:558`).

The freeze change is designed so this is **structurally untouched**: it adds no
grant path and does not change `checkedAvailable`, the ledger arithmetic, or the
`waiter.reserve > available` test. It changes only *which waiters are considered*
and *how long a caller may wait*. Both lineages searched for a Σreserve hole here
and found none.

The real holes found were both *out-of-ledger* admissions on the fallback path —
v1's own rejection design would have caused one (§4.0), and one already exists
(§4.2). That is where the OOM risk actually lives.

## 4. AIRA-58 — honest ceiling, refused at the edge, and a safe fallback

### 4.0 The dangerous failure mode v1 would have introduced

In `admitThroughDaemon`, only `E_ADMIT_TOO_LARGE` and `E_ADMIT_SATURATED` are
terminal (`admission_linux.go:361-391`); **every other** non-OK code falls to
`fail()` (`:313-329`), which routes to the **flock fallback** — whose own comment
concedes "cross-domain over-grant against live daemon reservations", basis
`fallback:daemon-unavailable`.

So v1's "reject with `E_PROTOCOL`" would not have refused anything: the job would
have launched **outside the daemon ledger** — strictly worse than today's clamp,
and precisely the over-commit direction this subsystem exists to prevent. Any
refusal on this path must use a code the client treats as terminal.

### 4.1 Behaviour

- **One shared exported constant**, `runner.AdmitWaitCeiling = 24h`, used by CLI,
  runner and daemon. No env knob: this is a typo guard, not a policy, and a
  single constant removes exactly the three-way drift that produced three
  independent 30-minute clamps. (Both lineages independently argued against a
  knob; a configurable daemon ceiling would also let an operator set a value no
  CLI caller could reach.)
- **Client clamp removed.** `runnerAdmitWaitCap` and `admission_linux.go:252-255`
  go; the requested wait goes on the wire, and the transport deadline derives
  from the **requested** wait (`:256-262`). This also removes the *premature*
  fallback trigger (§4.2).
- **CLI validates synchronously.** `--admit-timeout` is range-checked at parse
  time against the shared ceiling, refusing with `E_CONFINE_ARGUMENT_INVALID` —
  mirroring the already-honest `confine-reserve --max-wait` precedent at
  `main.go:709-716`. The usage string states the bound.
- **Daemon refuses over-ceiling `admit` requests** with a new stable terminal code
  `E_ADMIT_WAIT_TOO_LONG` (`protocol.go`), naming requested value and ceiling,
  before enqueueing. The runner handles it **as terminal with no flock fallback**,
  added to the structured branch *and* to `validRunnerAdmitRejection`
  (`admission_linux.go:449-458`) so a malformed payload cannot silently fall
  through to `fail()`.

**`worker-admit` deliberately differs, twice.**
1. Its ceiling **stays 30 minutes**. `admitConnection` is gated by `admitSlots`
   (`admit.go:386-399`, `admitGlobalMax = 1024`); **`workerAdmitConnection` has
   no such gate** — verified, `admitSlots` appears nowhere in `worker_admit.go` —
   and it holds a connection and goroutine for the whole wait
   (`worker_admit.go:356-430`). A 24h ceiling there would permit unbounded
   retained connections. Its only caller uses 300s.
2. It refuses with the **existing** `CodeProtocol` (`= "E_DAEMON_PROTOCOL"`,
   `protocol.go:30`), **not** the new code. `worker_admit_client_linux.go:97-103`
   wraps *any* non-OK response as `E_CONFINE_UNAVAILABLE`, and the aitest
   supervisor treats "unavailable" by **disabling daemon admission and running
   unconfined**, while it already classifies `E_DAEMON_PROTOCOL` as permanent.
   A code the supervisor does not know would have created a *second*
   unconfined-fallback hazard of the §4.0 class. Only the dishonesty is fixed
   there: refuse and name the ceiling instead of clamping.

### 4.2 The flock fallback launches uncapped (PR #7 finding "B2") — verified

`confine_linux.go:584-593`:

```go
admitted := admission.state == "immediate" || admission.state == "waited"
if !request.DelegateRAM && scopeMemoryMax <= 0 && admitted &&
   admission.lock == nil && admission.release != nil && admission.reserve > 0 {
    scopeMemoryMax = admission.reserve
}
```

`admitWithFlock`'s `finish()` (`admission_linux.go:152-161`) always sets
`lock`/`release` on a successful fallback, so `admission.lock == nil` is **false**
there. `scopeMemoryMax` stays 0, the `scopeMemoryMax > 0` guard at `:609` fails,
`writeScopeMemoryCap` is never called, and the scope launches with **no
`memory.max`** and no ledger accounting.

Triggered by: daemon down, daemon **restart** (drops every waiting connection at
once — the coordinator did exactly this tonight as an AIRA-59 mitigation), any
transport failure, and — per §2.1 — a client hitting its own premature 30-minute
deadline. B1 and B2 genuinely compound.

**Two precisions, because severity depends on them.**

1. **It is not unbounded.** The scope still sits inside `aira.slice` (64G cap,
   `memory.oom.group=1`), so the desktop is protected. What is lost is *per-scope*
   containment and ledger accounting, so one fallback job can consume the whole
   slice and OOM its **neighbours** — the AIRA-27 collateral class. HIGH, not
   CRITICAL. `admission_linux.go:307-312` shows this was known and accepted,
   "bounded by the OOMPolicy=kill backstop".
2. **The sharpest framing is inconsistency, not policy.** For a non-delegate job
   with explicit `--memory-reserve 512M`, the *daemon* path already enforces
   512M as the scope `memory.max` (a pinned reserve makes `admission.reserve` the
   user's own number, so `:591-592` fires). The fallback path does not. **Same
   command, same user request, different containment depending on whether the
   daemon happened to answer.** That is a divergence, not a trade-off.

**Chosen fix — split by whether the number is the user's or a guess.**

- **Pinned reserve → enforce, independently of admission state (IN SCOPE).** For a
  non-delegate request, whenever `request.MemoryReservePinned && ScopeMemoryMax <= 0`,
  enforce `request.MemoryReserve` as the scope cap — **regardless of admission
  state or whether a lock is held**. The existing admitted-daemon-grant branch at
  `:591` is retained only for *unpinned* estimates.

  v4 scoped this to successful flock acquisition by keeping `admitted` in the
  condition. That was too narrow: the fallback can return launchable `timeout` or
  `unevaluated` states with **no lock** (`admission_linux.go:175-214`), a
  responsive daemon can itself return `unevaluated` (`admit.go:419-427`), and
  `confine_linux.go:526-535` proceeds to create the scope in all of those cases.
  Those pinned launches would have stayed uncapped even though "the user supplied
  a trustworthy number" applies identically. Keying on *pinned-ness* rather than
  on admission state is both broader and simpler.
- **Unpinned reserve → NOT capped (settled, §8).** There the client holds only its
  own local default; the daemon's estimate is precisely what is unavailable. This
  was put to the review lineages as a genuine open question, and the answer was
  that **no defensible source exists**: `DefaultConfineMemoryReserve` is
  explicitly a guess (`confine_linux.go:446-457`), so enforcing it as a hard cap
  would false-OOM jobs that succeed today — which is exactly the documented
  reason `:585-589` restricts the cap to accounted grants. That decision stands,
  and stands on a reason rather than on inertia.

This split closes every case where the user told us the number, without inventing
a cap where nobody did.

## 5. AIRA-59 — a duty-bounded freeze, honestly described

### 5.1 What is actually wrong

Not the 60-second grace, and not that the freeze exists. The freeze is
**unbounded in duration**, borrowing its bound from a caller-controlled knob.

### 5.2 Rejected: size-scoped freezing (ticket suggestion 2)

**Rejected as backwards**; both lineages upheld this.

Large-head starvation comes *specifically from small waiters*: they are the only
ones that fit in the crumbs the head is accumulating. The head needs
`available ≥ reserve`; `available` grows only as jobs release; exempt small
waiters consume each release as it appears. Exempting them makes the freeze a
no-op in exactly the case it exists for — a 32G merge-gate would never run on a
churning 64G slice.

The HPC refinement collapses too: "hold out the head's reserve, backfill from the
surplus" gives `surplus = available − head.reserve`, negative **by construction**
(the head is frozen precisely because `available < head.reserve`), so it admits
nothing and is byte-for-byte the current freeze. True EASY/conservative
backfilling needs per-job **runtime estimates**, which AIRA lacks and which the
original design already listed as a deferral.

**One alternative recorded** (raised by the Fable gate): a *proportional-share
earmark* — credit the head a fixed share of each *release*, backfill the
remainder. It gives monotone head progress independent of job duration, which the
chosen design does not. Deferred, not dismissed: it is blind to the
`current`/`adopted` charge that §5.3 shows dominates in practice, needs new
per-release accounting state, and is comparable in complexity to what it
replaces.

### 5.3 The guarantee being "preserved" was never true

v1 proposed *growing* freeze windows to preserve the original starvation-freedom
claim. Both gates showed that claim holds in neither design, removing the
justification for that machinery.

**(a) `available` is not controlled by the queue.** `checkedAvailable`
(`admit.go:752-768`) charges `max(effectiveCurrent, outstanding + adopted)`.
`effectiveCurrent` is the slice's **real RSS** — including memory the queue never
granted: delegate-ram/aitest workers growing toward a 32G scope cap on a small
reserve, **uncapped flock-fallback launches (§4.2)**, and long-lived confined
residents (this project's CLAUDE.md mandates confining dev servers). `adopted` is
a scan of scopes the ledger cannot reconstruct (`admit.go:647-692`). Freezing
cannot force either to drain, so "available rises to the full ceiling" is false
**today**, before any change. Worse, the enqueue ceiling check (`admit.go:558`)
tests headroom only, so a head can be accepted and then be permanently
unadmittable.

**(b) The proof needs a uniform duration bound.** Any duty-cycled scheme admits
fresh jobs during yields, so the in-flight set is not fixed. "Every job
terminates" is weaker than "there is a uniform bound `D` on job duration": with
only the former, each yield can admit a job outliving the next hold, forever.
v1's growing-window argument quietly assumed the latter. **Bounded duty and
unconditional admission-starvation-freedom are incompatible under the honest
assumption** — window growth buys nothing.

The growing-window machinery is therefore **dropped**, which is also what the
project's architectural-simplicity rule demands.

This argues *for* a duty bound: an unadmittable head costs ≤50% of throughput
instead of 100%, and still gets an honest saturated rejection at its own timeout.

### 5.4 Chosen design: one knob, a queue-level bounded hold

The freeze becomes a **queue-level phase machine**, independent of which waiter
is protected:

- The queue is **idle**, in a **hold** (frozen), or in a **yield** (backfill runs
  normally).
- A hold lasts at most `admitFreezeMaxHold`; on expiry the queue enters a yield of
  the same duration; then it may hold again.
- Duty is exactly 50%, stated as a flat fact rather than parameterised. A
  percentage knob would be arithmetic theatre: hold-then-yield-of-equal-length is
  50% for any value, as the gate correctly noted about v1.

One new setting: `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD` (duration, default **2m**),
with `0`/`disabled` meaning *unbounded hold* — exactly today's behaviour —
following `admitBackfillGraceFromEnv`'s convention (`paths.go:121-134`). `grace`
is unchanged at 60s (§5.6).

**Holder identity must not affect timing.** Both lineages independently found
that v2's "re-anchor the window when the protected waiter changes" defeats the
bound entirely: a stream of unfittable heads with short timeouts — exactly the
retry-loop workaround AIRA-58 forced on its reporter, or 15 sessions' staggered
merge-gates — chains fresh full holds and keeps the queue ~100% frozen. So
hold/yield deadlines are **queue-level and never reset by holder departure**; a
successor inherits the running phase. `freezeHolder` is retained **for
diagnostics only** (§5.7) and never controls timing. This is also simpler than
v2: one phase deadline, no per-waiter bookkeeping.

**What is guaranteed (and what is not)** — stated honestly, because the previous
statement was wrong:

1. **Σreserve ≤ cap − headroom — unchanged.** Yielding only *widens* the set of
   waiters reaching the unchanged fit test (§3; tested §7.4).
2. **Bounded collateral (the AIRA-59 fix).** The queue is never blocked by
   fairness for more than half of wall time, and — because the phase is
   queue-level — that holds under arbitrary holder churn. The bound is
   **independent of `max_wait_ms`**, which is what makes §4's generous ceiling
   safe rather than catastrophic.
3. **The head keeps meaningful priority**: FIFO position, admitted the moment it
   fits, and repeated exclusive 2-minute accumulation windows.
4. **Every waiter still gets a bounded, honest terminal response** at its own —
   now genuinely honoured — timeout.

**Accepted, documented costs** (not silent):
- Neither the old design nor this one guarantees a large head is *eventually
  admitted* (§5.3a). The freeze never could; it now stops punishing everyone else
  for it. The 2026-08-27 design doc gets a correction note rather than being left
  to contradict the code.
- Head latency genuinely degrades. With grace 60s, 2m holds at 50% duty and a 30m
  timeout, a head gets roughly half the remaining window as exclusive
  accumulation. **Some heads admitted today will instead receive an honest
  saturated rejection.** v1/v2's "strictly weaker in latency" understated this.

### 5.5 State and placement

Per-`sliceQueue`, all under the existing `queue.mu` (no new lock, no change to
the documented `governorSet.mu → admitRegistryMu → sliceQueue.mu` order):
`freezePhase` (idle/hold/yield), `freezePhaseUntil time.Time`, and
`freezeHolder *admitWaiter` **for logging only**.

Transitions are **mutually exclusive**, evaluated once per pass inside the
single-writer `evaluateAdmitQueue` section (`runEvaluator` is the only production
caller, `admit.go:601-603`). Spelled out because v2 was loose enough to permit
per-tick deadline renewal — flagged by both gates:

- **An active hold never changes its deadline.** "Arm" means the idle→hold
  *transition* only; a pass that merely observes an ongoing freeze must not touch
  `freezePhaseUntil`.
- **hold, expired** → yield, `freezePhaseUntil = now + maxHold`.
- **yield, expired** → idle.
- **idle** + a non-fitting waiter past `grace` → hold,
  `freezePhaseUntil = now + maxHold`. During a yield, no arming.
- **Nothing froze this pass** (head fitted, granted, or left) → the phase is *not*
  cleared; it runs to its deadline, so churn cannot reset accounting. A holder
  that becomes fitting updates only the diagnostics-only `freezeHolder`; it must
  **not** clear `freezePhase` or its deadline, or repeated holder-fit churn would
  restart fresh holds and recreate near-100% freezing — the same attack as
  holder-change re-anchoring, by another route.
- **Placement:** all of this sits **after** the fail-closed slice-read return
  (`admit.go:699-709`), so a transient unreadable-slice blip cannot advance or
  restart a phase (§7.4).

**Accepted consequence of holder-independence.** Because the phase survives holder
departure, a successor that is *younger than `grace`* can inherit protection it
has not yet earned, for the remainder of an active hold. This is the deliberate
flip side of closing the churn attack, it is bounded by `maxHold` and the 50%
duty, and it is recorded rather than left as a surprise.

`admitBackfillGrace = 0`/`disabled` (strict FIFO) keeps its exact meaning: the arm
condition short-circuits before any phase logic.

**`maxHold = 0`/`disabled` must bypass the phase machine entirely**, not merely
set an infinite deadline. Today's freeze is recomputed statelessly from each
blocked waiter's own age on every pass (`admit.go:710-728`); a *persistent*
unbounded phase would behave differently — surviving holder departure to protect a
young successor before its grace, and surviving a temporarily all-fitting queue.
So `disabled` routes through the existing stateless loop with no phase state at
all. This is the only way "disabled reproduces today exactly" is a true statement
rather than an approximate one.

### 5.6 Why the 60s grace is deliberately left alone

The duty bound subsumes it (arming early is now cheap because bounded); raising it
would *weaken* head-of-line protection, the one property this subsystem currently
gets right; and moving two levers at once makes behaviour unattributable. It
remains env-tunable.

### 5.7 Diagnosability (ticket suggestion 4) — bounded version

- **Transition-only daemon logs** on hold/yield/idle transitions, naming slice,
  protected waiter `seq`, reserve, `available`, queued age, and how many waiters
  are blocked. Strictly transitions — passes run at up to 4/s.
- **`confine --list` reserve summary** gains queued-waiter count and freeze phase.
  `admitOutstandingReserve` (`admit.go:167`) already feeds
  `confine_manage.go:129` → `cmd/aira/main.go:2121`. A queue that genuinely has
  no waiters reports a **known zero**, not `unevaluated`; `unevaluated` is
  reserved for a queue whose state could not be established.

A full queued-waiter dump (new verb across CLI + MCP + Skill) is **deferred**.

## 6. Detecting whether B2 already fired tonight

The coordinator restarted the daemon while jobs were live. `aira confine --list`
showing 5 admitted jobs with finite caps is consistent with no uncapped launch but
is **not conclusive** — an uncapped job that already exited would not appear.

A positive check, if wanted (diagnostic, not part of the fix): a fallback launch
records `ReserveBasis = "fallback:daemon-unavailable"` (`admission_linux.go:153`)
in its confine status. Any journaled confine record carrying that basis in
tonight's window is direct evidence; a live one is `memory.max = max` on a
`.aira-CONFINE-*` scope. Offered rather than assumed — this plan does not depend
on the answer.

## 7. Tests (TDD — written first, each must fail before the fix)

Conventions from `admit_test.go`: the `admitNow` fake clock, the
`admitReadMemory` seam, directly constructed `sliceQueue`s, and
`waitAdmitGrant`/`requireAdmitQueued`.

**On sequential vs concurrent**: the freeze state machine is exercised by
deterministic fake-clock passes *by design* — `evaluateAdmitQueue` has exactly one
production caller (`admit.go:601-603`), so a single-writer test is faithful, not
thin. Stated rather than assumed. Genuinely concurrent coverage is still required
at the connection layer (tests 19, 5), where real goroutines and the peer-close
lifecycle are the thing under test.

### 7.1 AIRA-58
1. **Runner wire-boundary test** (catches v1's non-fix): drive
   `admitThroughDaemon` via the `admitDialFn` seam with `AdmissionMaxWait = 2h`
   and assert the **`max_wait_ms` on the wire is 7200000**, not 1800000.
2. The transport deadline derives from the requested wait, so a 2h request is not
   torn down at 30m — and therefore does not fall into the fallback (§4.2).
3. Daemon-side: 2h honoured — installed deadline equals 2h (`admitAfter` seam).
4. An over-ceiling `admit` request is refused with `E_ADMIT_WAIT_TOO_LONG`, naming
   requested value **and** ceiling, with **no waiter enqueued** (asserted on
   `len(queue.waiters)`).
5. **Terminal-code test (the §4.0 guard):** the runner treats
   `E_ADMIT_WAIT_TOO_LONG` as terminal and **never enters the flock path** —
   including a malformed-payload variant. Also: `confine-reserve` does not retry
   it as `too_large` (`confine_reserve_linux.go:48-52`).
6. `worker-admit` refuses over its 30m ceiling with `E_DAEMON_PROTOCOL`, and its
   ceiling is asserted **distinct** from the `admit` ceiling so a later
   "consistency" refactor cannot silently raise the unbounded path.
7. **Supervisor classification test**: the worker-admit refusal is permanent and
   does **not** disable daemon admission / trigger unconfined fallback.
8. Inversion of the clamp-encoding tests at `admit_test.go:256`, `:689` and the
   `worker_admit_test.go` sites.
9. Shared-constant test: CLI, runner and daemon refuse at the same value —
   covering the **programmatic runner boundary**, not only CLI parsing, since a
   non-CLI caller reaches the runner directly.

### 7.2 Fallback capping (§4.2)
10. A **pinned** reserve admitted via the flock fallback **does** get
    `writeScopeMemoryCap` called with the user's value — the divergence closed.
11. **Every launchable non-admitted state** is covered, not just successful flock
    acquisition: fallback `timeout`, fallback `unevaluated` and lock-error (no
    lock held, `admission_linux.go:175-214`), and a responsive daemon returning
    `unevaluated` (`admit.go:419-427`). Each with a pinned reserve must be capped.
    This is the v4-too-narrow case; without it a pinned launch still escapes.
12. Daemon-path and fallback-path parity: the same `--memory-reserve N`
    non-delegate command yields the same scope cap either way.
13. An **unpinned** fallback admission is unchanged (still uncapped) — pinning the
    settled boundary so a later change is a conscious act.
14. Delegate-ram fallback still gets a finite cap via `delegateRAMScopeFallback()`
    and never regresses to uncapped (`confine_linux.go:600-608`).

### 7.3 AIRA-59 core
15. Freeze arms as today, then **yields** once the hold is spent: small waiters
    behind an unfittable head are granted; the head stays queued.
16. Freeze **re-arms** after the yield — bounded, not deleted.
17. **Exact boundary timing**: hold exactly `maxHold`, yield exactly `maxHold`,
    checked one tick before / at / after, **with repeated evaluator passes inside
    a single hold** asserting the deadline does not slide forward.
18. **Holder churn does not extend the freeze** (the P1 both gates found): head A
    times out mid-hold, successor B takes over → B inherits the running phase and
    gets **no fresh full hold**; a sustained stream cannot exceed the duty bound.
19. No dangling `freezeHolder` after `releaseAdmitWaiter` splices `queue.waiters`
    (`admit.go:795-802`).
20. Governor-shaped contention, **genuinely concurrent** through `admitConnection`:
    one 32G-class head plus many ~1G pinned per-test waiters; small waiters
    progress within one yield, and the frozen fraction stays ≈50% **including
    under holder churn** (a "≈50%" assertion with one stable holder is porous).
21. The same at the **`confine-reserve` seam** with governor-shaped args
    (`pinned: true`, `signature: "pytest:<nodeid>"`, `max_wait_ms: 300000`,
    reserve ~1G) — the confirmed real entry point (§2.4).
22. A holder that becomes fitting is granted immediately, and the **phase
    persists** — only the diagnostics-only `freezeHolder` changes. (v4 asserted
    the opposite and contradicted §5.5; clearing on fit is a second route to the
    churn attack, so this test is deliberately inverted.)
23. `maxHold = 0`/`disabled` reproduces today's unbounded freeze **exactly** —
    including the discriminating case: an old holder departs and a successor
    **younger than `grace`** becomes head, which today's stateless loop must
    leave unprotected. A persistent-phase implementation fails this.
    `grace = 0`/`disabled` remains strict FIFO with no yielding.
24. `admitFreezeMaxHoldFromEnv` parser + startup-failure tests.

### 7.4 Invariant / false-pass direction
25. After any yielded backfill pass, `queue.outstanding` equals the exact sum of
    granted reserves and `outstanding ≤ cap − headroom`.
26. **`adopted`-dominant and `effectiveCurrent`-dominant cases.** Test 25 alone can
    pass an implementation that ignores those charges, so it is porous by itself.
27. Fail-closed unchanged: an unreadable slice grants nobody, and a mid-hold read
    failure must **not** advance or restart the phase.
28. `enqueueAdmitInternal`'s ceiling check (`admit.go:558`) still rejects an
    over-ceiling reserve regardless of freeze state.
29. Diagnostics: transition logs fire on transitions **only**; `confine --list`
    renders queued/freeze fields, with a genuine empty queue a known zero.
30. **Mutation testing.** Reintroduce each defect; confirm the named test fails,
    then passes on the real fix:
    (a) daemon clamp → 3; (b) **runner** clamp → 1 (proves v1's blind spot closed);
    (c) `E_DAEMON_PROTOCOL` on the `admit` path → 5 (flock-bypass guard);
    (d) new code on the worker path → 7; (e) freeze unbounded → 15;
    (f) yield permanent → 16; (g) renew hold deadline each pass → 17;
    (h) re-anchor phase on holder change → 18; (i) yielded waiter skips fit test →
    25, 26; (j) clear phase on unreadable-slice pass → 27;
    (k) drop the pinned-fallback cap → 10;
    (l) re-narrow the pinned cap to `admitted` only → 11;
    (m) clear the phase when the holder fits → 22;
    (n) implement `disabled` as a persistent infinite phase → 23.

## 8. Scope and deliberate deferrals

**In.** The shared wait ceiling across CLI, runner and daemon (removal of
`runnerAdmitWaitCap`, new terminal `E_ADMIT_WAIT_TOO_LONG` with no-fallback
handling); **pinned-reserve cap enforcement on the flock fallback** (§4.2); the
queue-level bounded-hold freeze; freeze transition logging; the `confine --list`
queued/freeze summary; the 2026-08-27 correction note; all tests above.

**Out, deliberately.**
- **Capping *unpinned* flock-fallback launches** (§4.2). Put to both gates with
  this plan; lands only if they agree, else filed with the evidence. Enforcing a
  client-side guess as a hard cap can false-OOM jobs that succeed today, and the
  contrary decision at `confine_linux.go:585-589` survived a prior adversarial
  review — it will not be overturned without one.
- **`cmd/aira/main.go:857-859` forcing `reserve = --memory-max` even for
  delegate-ram**, silently overriding `--memory-reserve` (§2.2). Verified, and the
  confirmed source of the live 32G heads — but a memory-accounting change in the
  over-commit direction needing its own safety argument. **Filed as its own P1.**
- Size-scoped freezing (§5.2); proportional-share earmark (§5.2); raising
  `defaultAdmitBackfillGrace` (§5.6); runtime-estimate backfill reservations.
- **A concurrency bound for `worker-admit`** (it has none, §4.1) — a behavioural
  change to a path neither ticket concerns; its ceiling stays 30m instead.
- The `worker-admit` evaluator's lack of arrival ordering.
- `main.go:709-716`'s `(0,30m]` bound on `confine-reserve --max-wait` — refuses
  honestly, so not an AIRA-58 defect.
- No new CLI/MCP verb for full queue introspection.

## 9. Risks

1. **Weakened head latency — the main accepted cost.** Some heads admitted today
   will instead get an honest saturated rejection (§5.4). Written down rather than
   discovered later. Tests 15 and 22 keep protection real and opt-outable.
2. **`maxHold` mis-set.** `0`/`disabled` restores today's behaviour exactly (test
   22). Parsed, range-checked, hard-failing startup like its siblings.
3. **The halves must land together.** Shipping §4 without §5 extends the
   worst-case slice-wide stall from 30 minutes to the caller's whole timeout.
4. **Pinned-fallback capping could kill a job that previously ran uncapped.** This
   is the intended correction (it matches the daemon path), but it *is* a
   behaviour change: a user whose `--memory-reserve` was too small and who
   silently relied on the fallback being uncapped will now be OOM-killed inside
   their own declared bound. Accepted — the daemon path already does this, and
   the inconsistency is the bug. Tests 10-11 pin the parity.
5. **Clock seam.** All phase arithmetic uses `s.admitNowTime()`, never
   `time.Now()`.
6. **Refusal is a behaviour change.** 24h is far outside observed usage.
7. **Log volume.** Mitigated by transition-only logging.

## 10. Expected yield

Removes a machine-wide throughput failure in which one ordinary request stalls
every other session's admission for up to 30 minutes (soon: for as long as it
liked) on a visibly idle box; makes `--admit-timeout` mean what it says across all
three clamp sites; and closes the *pinned* half of a live containment gap in which
a daemon restart silently launches jobs with no per-scope memory cap. It also
avoids opening two further unconfined/out-of-ledger fallback hazards that a naive
fix would have created.

## 11. Changelog

Every finding was **independently re-verified in source** before acceptance; none
was taken on a reviewer's word.

**v2 — after the Codex/Sol gate returned BLOCK.**
- **P0 (§2.1):** a *second* silent 30-minute clamp in the runner. v1 was
  daemon-only and would have shipped a **non-fix its own tests passed**.
- **P0 (§4.0):** `E_PROTOCOL` would have fallen through `fail()` to the **flock
  fallback**, letting a refused job launch outside the ledger.
- **P1 (§4.1):** `worker-admit` has no `admitSlots` bound → its ceiling stays 30m.
- **P1 (§5.3-5.4):** `dutyPct` was arithmetic theatre; the growing-window proof
  smuggled in a uniform duration bound. Both dropped for a single `maxHold` knob —
  **less machinery than v1**.
- **P1 (§5.3a):** unconditional starvation-freedom retracted for an explicit
  guarantee list plus a documented gap.
- **P2 (§7):** exact-transition, holder-churn, parser, `adopted`/`effectiveCurrent`
  and concurrent tests added; `admit_test.go:689` noted as *asserting the bug*.

**v3 — after the Fable gate (on v1) and the Sol re-gate (on v2).** Both lineages
independently reproduced the two P0s above.
- **Root cause CORRECTED (§2.2):** `main.go:857-859` forces
  `reserve = --memory-max` for **all** jobs including delegate-ram, silently
  overriding `--memory-reserve`. Matches "(reserve 32G)" and "33280M granted"
  exactly. v2's claim that delegate-ram jobs are not the big heads was wrong.
- **P0 (§4.1):** the new terminal code on the *worker* path would have been
  wrapped to `E_CONFINE_UNAVAILABLE` and made the aitest supervisor **disable
  daemon admission and run unconfined** — a second §4.0-class hazard.
- **P1 (§5.4-5.5):** holder-change re-anchoring defeats the duty bound under head
  churn. The phase machine is now **queue-level and holder-independent**.
- **P1 (§5.5):** transitions made mutually exclusive; phase logic placed after the
  fail-closed read.
- **P1 (§4.1):** env knob dropped for **one shared constant**.

**v4 — after the PR #7 architectural review (relayed by the coordinator).**
- **B1 (§2.1):** independently confirms the runner clamp already fixed in v2/v3 —
  a third lineage reaching the same finding. Count corrected to **three** clamp
  sites. Already in scope; no change needed beyond naming it.
- **B2 (§4.2) — NEW, verified:** the flock fallback launches non-delegate scopes
  with **no `memory.max`**, because `:591`'s `admission.lock == nil` is false for
  every fallback admission. Triggered by daemon restart — which happened on this
  box tonight. Two precisions added: it is bounded by the slice cap (HIGH, not
  CRITICAL), and the decisive framing is that the **daemon path already caps a
  pinned reserve while the fallback does not** — a divergence, not a trade-off.
  The pinned case is folded in; the unpinned case is gated rather than silently
  overturning a previously reviewed decision (§8).
- §6 added: how to establish whether B2 actually fired tonight.

**v5 — after the Sol gate on v4 returned BLOCK.** It also *confirmed* three things
it had been asked to attack: the pinned/unpinned split is correct (a
`pinned:client` daemon grant has `lock == nil` and so is already enforced as
`memory.max`, while a flock grant carries both `lock` and `release` — that is
exactly the divergence); pinned cap enforcement has no lifecycle conflict with the
flock lease, `--memory-high`, delegate-ram or detach; and the HIGH-not-CRITICAL
severity read is right because the finite parent slice remains the backstop.
- **P1 (§4.2):** the pinned fix was **too narrow**. Keeping `admitted` in the
  condition covered only successful flock acquisition, leaving pinned launches
  uncapped on fallback `timeout`/`unevaluated`/lock-error and on a daemon
  `unevaluated` — all of which still create the scope. Now keyed on
  *pinned-ness alone*, independent of admission state: broader **and** simpler.
- **P1 (§5.5, test 22):** v4's test 21 required a fitting holder to clear the
  phase, contradicting §5.5 and reopening the churn attack by another route.
  Inverted: the phase persists; only diagnostics change.
- **P1 (§5.4-5.5, test 23):** `maxHold=0/disabled` is not "today exactly" if
  implemented as a persistent infinite phase — today's loop is *stateless* per
  pass. `disabled` now bypasses the phase machine entirely, and the test includes
  the discriminating younger-than-grace-successor case.
- **P2:** the unpinned question is **settled with a reason**: no defensible cap
  source exists, `DefaultConfineMemoryReserve` being explicitly a guess.
- **P2 (test 9):** shared-constant coverage extended to the programmatic runner
  boundary.
- Accepted consequence now recorded (§5.5): holder-independence means a
  younger-than-grace successor can inherit protection for the rest of a hold.

Net: scope grew by two files and one error code; the freeze mechanism is
**simpler than v1**; a root-cause misattribution was corrected; **three** defects
that would each have shipped a plausible-looking wrong fix — two opening new
out-of-ledger admission paths — were caught before any code was written; and a
fourth, pre-existing containment gap was verified and half-closed.
