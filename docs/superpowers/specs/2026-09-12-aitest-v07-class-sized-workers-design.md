# aitest v0.7 — class-sized short-lived workers, per-test RAM annotation

> **⚠ SUPERSEDED FOR S2 (2026-09-12).** The owner reinstated the batch **and** moved all quota
> authority into the daemon. The S2 design is now
> [`2026-09-12-aitest-v07-s2-daemon-authoritative-design.md`](2026-09-12-aitest-v07-s2-daemon-authoritative-design.md):
> slice is the one cap; workers are sibling scopes individually reserved against it; `--delegate-ram`
> becomes an ordinary confine job; batch/prune + ~10 s age cap + peak-vs-reservation warning +
> OOM-penalty phantom reservation. The class-sized-workers + client-side outer-cap model below is a
> review-era reversal that the owner has since overturned for S2 — read it for S1 history and the
> `aira_mem` marker grammar only. **S1 landed and stands; the v7-1 guard below is removed by S2.**
>
> **Status:** DESIGN — landed to master for build 2026-09-12; three-lens adversarially reviewed.
> **The owner-described batch/prune/kick-back flow is REVERSED here** in favour of class-sized
> short-lived workers + lazy same-class pull (§2 for the reasoning, OD1 for the one-place reinstate
> path) — a conscious simplification, surfaced to the owner, not silently re-litigated. The OPEN
> DECISIONS (OD1–OD5) carry the reviewers' **leading recommendations, not owner ratification**; OD1
> (grow-mechanism / batch-reinstate fork) is the owner's to settle and does not bind until stage S2.
> EXTENDS [`2026-09-01-aitest-design.md`](2026-09-01-aitest-design.md) and reconciles against the
> shipped v0.6 admission-counter model ([`2026-09-10-simple-admission-counter-design.md`](2026-09-10-simple-admission-counter-design.md),
> its AS-BUILT amendment, and the code that landed under it, #130). It does not restate the parts of
> the v0.1 aitest design that are unchanged (fork+COW warm-import, the worker-admit outcome
> vocabulary, `pidfd`-observed death, JUnit/coverage replay, the daemon-unavailable fallback).
> Where this document and the v0.1 spec's §3.3/§4 invariants disagree, the reconciliation is
> called out explicitly in §7.

> **REVISION NOTE (2026-09-12) — the batch is retired; the model is now class-sized workers +
> lazy same-class pull.** The original draft built a *batch* (a reserved subset of nodeids handed
> to a worker) with a prune/kick-back negotiation. Two of three reviewers, grounded against the
> shipped code, showed the batch's stated justification is **false as-built**: the interim worker is
> admitted **once at spawn** and then pulls **many** nodeids one-at-a-time
> (`internal/pylib/aitest/worker.py:508+`, verified: `for line in pipe_in: … run_one(item)` until
> `__stop__`/recycle). Admission is therefore **already** amortised over the worker's whole life; a
> batch adds **zero** admission or fork saving over the shipped one-at-a-time loop. Its only real
> benefit was tight sizing, and that comes from **worker-sizing by size-class** (OD2), not from
> pre-committing a nodeid set. This revision deletes the batch object, the prune negotiation, the
> kick-back, the max-over-batch-vs-crash accounting, the try-acquire, and the protocol 11→12 bump.
> **The owner-described batch/prune flow is preserved as the rejected alternative in OD1, with a
> one-place reinstate path**, so the fork stays visible.

## 1. Problem

The v0.6 interim ([shipped, protocol 11]) deliberately runs a **flat** worker pool: a persistent
set of workers, each pulling **one FIFO nodeid at a time** (`worker.py:508-564`), each with a
**uniform** `memory.max` = a single flat `AIRA_AITEST_ESTIMATED_BYTES` (512 MiB default,
`__init__.py:295`), recycled at **600 s / 200 tests / 64 %-watermark** (`worker.py:7-8,23`). There
is **no per-test memory annotation**. This was the right first cut — it validated the
admission/lifecycle machinery against a real cgroup tier — but it leaves two problems the original
aitest spec §1 already named:

- **A uniform flat reserve is wrong in both directions.** 512 MiB over-reserves every light unit
  test (so fewer workers fit than RAM allows) and under-reserves a genuinely heavy one (so it OOMs,
  requeues, and — if it OOMs again — is marked `unevaluated`). A suite is not uniform; a single
  flat number cannot size it.
- **Long-lived workers make the machine-wide governor slow to redistribute.** A worker that lives
  600 s / 200 tests holds its slot against the unified ledger for that whole time. On a shared box
  an incumbent suite that recycles **re-requests its own slot immediately from its own retirement
  path**, so a newly-started sibling suite can sit at its floor — the exact fairness hole
  `_maybe_grow_pool` documents (`supervisor.py:2015-2021`) and papers over with a 1 Hz growth probe.
  Slot *turnover*, not just a slot *cap*, is what makes redistribution prompt.

v0.7 addresses both: a per-test RAM **annotation** sizes admission; each worker is **sized to a RAM
size-class** at spawn and pulls only same-class tests one at a time; and every worker is
**short-lived (~10 s)** so slots turn over fast enough for cross-suite fairness to bind.

## 2. Simplify-first — what is the minimum, and why not a batch

The from-scratch minimum for *only* "size a worker to its work and turn slots over" is a small,
subtractive change to the shipped interim:

1. **A per-test RAM annotation** (`aira_mem`) so admission can size a worker to actual work instead
   of a flat suite constant. A flat reserve is wrong in both directions (§1). **Necessary.**
2. **Lower `AIRA_AITEST_WORKER_MAX_SECONDS` from 600 to ~10** so slots turn over and redistribution
   is prompt. **Necessary for the fairness goal.**
3. **Size the worker to the annotation of the tests it will run**, and hand it only tests that fit
   its cap (worker-sizing by size-class, OD2). This is the sizing win the batch was reaching for,
   without reserving a nodeid set.

**Why not a batch.** The draft's batch justification — "a 10 s one-at-a-time worker would re-admit
(one daemon round trip per test)" — is **false as-built.** The interim worker is admitted **once**
at spawn (its `scope_path`/`memory.max` are fixed by the grant) and then pulls **many** nodeids
one-at-a-time via `for line in pipe_in` (`worker.py:508-564`, verified). Admission is already
amortised over the worker's whole life. A ~10 s worker pulling same-class tests one-at-a-time and a
~10 s batch worker each do **exactly one admission and one fork per slot** — the batch delivers no
churn or fork saving. Its only real function is tight sizing, supplied by size-class worker sizing
without reserving specific nodeids. So the batch machinery — reserved nodeid set, prune negotiation
(§4.3 in the draft), kick-back (§5), the kick-back-vs-crash accounting (§8), the try-acquire and the
protocol bump — fails the necessity test and is **deleted** (OD1). This is a conscious reversal of
the owner-described flow; the reasoning is above and the reinstate path is OD1.

**RSS honesty — what the ~10 s cap does and does not do.** The draft claimed the 10 s cap keeps
"cumulative worker RSS near its reservation" and "retires the AIRA-12 cumulative-RSS objection."
**That claim is retracted.** `memory.max` charges *resident* memory; `gc.collect()` reclaims cyclic
garbage into Python's free lists but does **not** return pages to the OS, and `gc.freeze()` returns
nothing. Within one 10 s window a worker's RSS is monotonically non-decreasing — roughly baseline +
Σ(un-returned residue of prior tests) + the current test's peak. **The 10 s age cap bounds
accumulation *time*, not accumulation *RSS*.** What actually keeps RSS under `memory.max` is (a)
the per-worker **headroom** added to the cap (§4.2), (b) the **64 % between-tests watermark** that
retires a worker before it climbs into the cap, and (c) the kernel `oom.group` as the last resort.
The 10 s cap's real, genuine job is **turnover / cross-suite fairness** (§6), and it is reframed as
such throughout. See the accepted-gap on mid-test accumulation OOM in §8.

## 3. Goals / non-goals

**Goals (new or sharpened vs v0.1):**

- Size each worker's RAM reservation from **its RAM size-class** (the coarse bucket its tests fall
  in, OD2), not a flat suite constant.
- **Short-lived workers (~10 s)** as the **turnover / fairness** mechanism; the 64 % watermark and
  the per-worker headroom (not the age cap) are the RSS-containment mechanisms.
- **aira is the machine-wide worker-concurrency governor** across concurrent suites — N suites
  share ~2·nCPU total workers, not N·nCPU (§6).
- A **prompt, fair** redistribution of slots as suites start and finish, via slot *turnover* **and**
  a shared-slice growth claim that sits in the fair daemon FIFO (§6, OD1).

**Non-goals for v0.7 (explicit deferrals):**

- **Per-test CPU annotation.** A worker reserves exactly one core (`DefaultConfineCPUCores`,
  `worker_admit.go:514`), unchanged; design §8's "cpu: 1 unless annotated" stays deferred.
- **Fixture-affinity / `loadgroup` batching.** Workers are grouped by RAM size-class only, never by
  fixture scope (v0.1 §2 still holds). A worker runs independent nodeids.
- **look-ahead `nextitem`.** Unchanged from v0.1 §3.2 — every item still runs with `nextitem=None`.
- **Sub-slice-ceiling adaptive annotation defaults** (learning a per-suite default from history).
  The peak-RSS estimator's role degrades under per-test sizing (§7); reviving it is future work.

## 4. The size-class worker + annotation model

### 4.1 The `aira_mem` marker

A test declares its RAM need with a pytest marker:

```python
@pytest.mark.aira_mem("512M")      # 1024-based size string; K/M/G/T = powers of 1024
def test_loads_a_big_fixture(): ...
```

- **Semantics (load-bearing — fixed here).** `aira_mem` declares the test's **incremental peak
  RSS**: the additional resident memory the test's own work needs **on top of the worker's
  warm-import baseline**, not the worker's total RSS while running it. This is the natural reading
  and the portable one — a test's own footprint does not depend on the shared baseline of whatever
  worker runs it. **Direct consequence:** a worker's `memory.max` is **not** `max(annotation)`; it
  is `class-cap + per-worker headroom`, where the headroom covers the COW-fault baseline and
  cross-test residue (§4.2). Sizing `memory.max` to the bare annotation would systematically
  undersize it and make the mid-test-OOM the *expected* case, not the tail.
- **Name:** `aira_mem`, the name AIRA's own skill documentation already calls "its per-test RAM
  marker" (`internal/core/skill_test.go:613,638`); the live reader went away with the #69 RAM
  governor / `aira_xdist_governor` (AIRA-33), so **there is no live registration today** — v0.7
  re-introduces the reader under the documented name rather than coining a new one.
- **Argument syntax:** a single size string parsed by the **same** grammar as
  `AIRA_AITEST_ESTIMATED_BYTES` / `--memory-reserve` — `_parse_estimated_bytes` (`__init__.py:246`)
  and Go `runner.parseMemorySize` — so `512M`, `1.5G`, `512MiB`, a bare byte count all mean the same
  everywhere in AIRA. A bare int is bytes. Invalid → warn once, treat as unannotated (the AIRA-223
  no-silent-swallow discipline, `__init__.py:281-295`).
- **Default when absent:** `AIRA_AITEST_DEFAULT_BYTES`, **256 MiB** starting point (OD4). This is
  the *incremental* default; it is field-tunable and **must be validated against a measured COW
  baseline before it is fixed** (OD4) — do not treat 256 MiB as load-bearing.
- **Registered** via `config.addinivalue_line("markers", "aira_mem(size): …")` in
  `pytest_configure` so `--strict-markers` suites do not fail and it appears in `pytest --markers`.
- **Read at collection**, once, in the supervisor: for each `session.items` entry,
  `item.get_closest_marker("aira_mem")` → parse → attach to a `nodeid → bytes` map alongside
  `items_by_nodeid` (`supervisor.py:813-819`). Unannotated nodeids map to the default. This map is
  the sole input to size-class bucketing; it is computed before any worker is admitted and inherited
  by forked workers via COW.

### 4.2 Size-class worker sizing, and the per-worker cap

A worker is **sized to one RAM size-class at spawn** (OD2=c: a coarse power-of-2 ladder over the
annotation map). It then pulls **only same-class nodeids** one at a time (§5) until the age cap or
its class sub-queue drains. Because every test it runs is ≈ the class size, there is **no
heterogeneous inflation** (the draft's max-over-batch problem) — the residual is only the bounded
round-up of an annotation to its bucket cap (OD2, accepted).

**The cap is `class-cap + per-worker headroom`, and the ledger charge equals the cap.** This is the
correction to the draft, and it must be stated exactly to avoid re-opening the slice invariant:

- The daemon writes the requested `memory.max` to the worker scope **verbatim** — the request *is*
  the cap (`internal/runner/worker_scope_linux.go:57,87`: `CreateWorkerScope(…, memoryMax)` →
  `writeScopeMemoryCap(scope, memoryMax, …)`; reviewers confirm the granted `memory.max` == request
  and no per-worker headroom is added daemon-side, `worker_admit_cli_granted_linux_test.go:251,311`).
- Therefore the **request** the supervisor makes = `class-cap + per-worker headroom`, and the
  **ledger charge, the containment cap, and the request are one and the same number.** We do **not**
  reserve `class-cap` against the ledger while sizing `memory.max` larger — that would make
  Σ(`memory.max`) > Σ(ledger) and let real RSS exceed the slice ceiling. The reservation and the
  containment cap stay equal.
- **The trade this makes explicit:** reserving `class-cap + headroom` **lowers concurrency** by the
  headroom fraction versus reserving the bare class cap. This is the price of not silently marking
  passing tests `unevaluated` via mid-test OOM. The design **owns** this cost rather than the
  rejected alternative (reserve bare class-cap and eat a measured mid-test OOM-kill/requeue rate —
  see the accepted-gap in §8, which is why that alternative is *not* the default).
- **The headroom value is field-tunable and must be measured, not reasoned** (OD4): it covers the
  private COW-fault baseline at fork+placement plus cross-test residue, and glibc's dynamic
  `M_MMAP_THRESHOLD` means even large-buffer workloads shift from mmap (returned on free) to brk
  (pinned) after the first allocation cycle, so residue accumulates. S1 instruments worker RSS-vs-cap
  and the OOM/requeue rate before the headroom and the 256 MiB default are fixed.

### 4.3 Growing the pool (no batch, no prune)

Under size-class workers there is **no prune loop** (the draft's negotiation degenerates for
homogeneous workers — dropping a same-size test does not lower the max). Growth is a **whole-worker
gate**, not a per-test negotiation:

1. Pick a size-class to grow. To avoid heavy-class starvation under heterogeneity, pick the class of
   the **oldest-waiting ready test** (OD3 Tier 2), not "whatever small frees appear."
2. Compute the request = that class cap + headroom.
3. Run the **client-side aggregate guard** (§7 / OD3): admit only if
   `Σ(this suite's live worker memory.max) + Σ(issued-but-ungranted requests) + supervisor allowance
   + request ≤ effective_outer_cap − headroom`, where `effective_outer_cap` is the **min over the
   scope's cgroup ancestry** (`effectiveConfineCap`-style, `internal/runner/confine_linux.go`), not a
   bare read of `<outer_scope>/memory.max`. **The `Σ(pending requests)` term is load-bearing:** with
   one pending claim per class, two classes that each pass the guard independently against the same
   Σ-live can both be granted and overshoot the outer cap — so a pending request counts against Σ
   from the moment it is issued until it is granted (folded into Σ-live) or cancelled. Also refuse
   any **request (class cap + headroom)** that exceeds the hard ceilings (OD3 Tier 1) — the check is
   on the request, i.e. what the daemon writes as `memory.max`, not the bare class cap.
4. Issue the growth claim via the **select-integrated relay fd** (OD1): the growth relay's stdout is
   placed in the supervisor's existing `select()` set alongside worker pipes and pidfds, so a
   **blocking** daemon claim can sit in the fair daemon FIFO **without freezing** the single-threaded
   dispatch loop. If the class does not fit current available, the claim waits (fairly) and the loop
   keeps dispatching to live workers; when the class drains from the queue, the claim is cancelled by
   closing the relay stdin.

### 4.4 gc discipline

- The **supervisor** still calls `gc.freeze()` **once, pre-fork** (`supervisor.py:2494-2495`) — the
  COW-friendly placement: freezing before the fork keeps the warm-imported generation off the
  young-collection scan and shared read-only across all workers. **Kept unchanged.**
- The **worker** additionally calls `gc.freeze()` **before its first test** and `gc.collect()`
  **after each test**. Honest scope: these help collection **speed** (the young scan stays small)
  and slightly slow arena growth by reclaiming the cyclic fraction (real for pytest traceback /
  exception chains). **They do not return memory to the OS and are not part of the RSS bound** (§2).
  Keep them for speed; do not describe them as "what keeps RSS flat."

## 5. Worker lifecycle & recycle

The worker loop (`run_worker_loop`, `worker.py:508-564`) is **unchanged in shape**: pull one nodeid
→ run it → check recycle → report result (with `__recycle_next__` riding the same line,
`worker.py:35,558-561`) → retire if recycling. A worker holds **exactly one nodeid at a time** (the
shipped lazy-pull loop); there is **no reserved set**. v0.7 changes recycle *conditions* and adds a
per-class dispatch rule:

- **Age cap (turnover mechanism): `AIRA_AITEST_WORKER_MAX_SECONDS`, default ~10 s** (was 600).
  Checked **between tests only** (`_should_recycle`, `worker.py:258-295`; v0.1 §3.4). Its job is slot
  turnover / fairness, **not** the RSS bound (§2).
- **64 % memory watermark: co-primary RSS backstop, checked between tests** in the same
  `_should_recycle`. Since the per-worker cap now carries only measured headroom (no implicit 512 MiB
  slack), the watermark is the load-bearing between-tests defence against cross-test accumulation:
  it retires a worker that has climbed toward its cap **before** the next test can push resident
  memory over `memory.max`. Consider lowering the fraction once the baseline is measured (OD4).
- **`AIRA_AITEST_WORKER_MAX_TESTS` (200): disputed — see OD4.** The reviewers split on whether to
  delete it or keep it active as a direct residue-event bound. Do not silently pick; OD4 carries the
  fork.

**Per-class dispatch (new rule, replaces kick-back).** A worker is class-scoped: `_dispatch_to_idle_workers`
and `_pool_covers_the_queue` become **per-class** — a class-C worker is handed only class-C nodeids
(strict same-class, **not** ≤-class; a big worker draining small tests over-reserves). **A class-C
worker whose class sub-queue drains before its age cap is `__stop__`ped promptly** rather than
idle-holding a class-sized lease while another class waits for room. Because a worker holds one
nodeid at a time and `attempts`/`in_flight` are updated only at **real dispatch**, a worker that
retires or crashes leaves its class's un-pulled nodeids **untouched in the queue** — there is nothing
to kick back and nothing to double-queue (§8).

**A single test that runs > 10 s.** Recycle is between-tests only, so the worker **finishes the
in-flight test, then** the age check fires and it retires. The long test's result is a **real**
result (pass/fail), **never** `unevaluated`, and is **never interrupted**. A test that alone always
exceeds 10 s causes its worker to retire after that one test each time — acceptable churn for a
genuinely slow test.

## 6. Cross-suite concurrency — aira as the machine-wide worker governor

**Goal:** M concurrently-running pytest suites on one box yield **~2·nCPU total workers
machine-wide**, not M·nCPU.

**What already holds in v0.6.** Every worker lease reserves **one core** (`DefaultConfineCPUCores`,
`worker_admit.go:514`) against the **CPU ceiling of 2·cores** (design §7) on the **one unified signed
ledger** every suite's workers charge. So the ≤ 2·nCPU concurrent-worker bound **already holds** — it
is not new.

**What v0.7 adds — and an honest correction to the draft.** The draft claimed ~10 s turnover fixes
cross-suite fairness **"structurally."** **That is retracted.** The fairness hole `_maybe_grow_pool`
documents (`supervisor.py:2015-2021`) is caused by **probe-gating**, not slot lifetime: a retiring
suite self-reclaims its freed slot via its own immediate event-driven `_replace_worker`→`_try_grow_one`
probe (fires at the instant its own worker frees room), while a competitor only catches that window
via its 1 Hz probe. On a saturated slice a growth claim is enqueued **only after** a probe shows room,
so competing suites mostly never enter the fair daemon FIFO — it stays a **probe race the incumbent
wins**. Shortening workers to ~10 s makes slots turn over faster (good for **latency**) but preserves
the self-reclaim advantage and **multiplies the race count ~60×** (every worker re-admits every ~10 s
instead of every ~600 s), also multiplying relay forks and lost-race scope churn on the AIRA-36 reaper.

So: **turnover helps latency; it does not by itself arbitrate cross-suite fairness.** Real fairness
needs the **shared-slice mechanism** the v0.6 design §5 already provides — a **blocking** growth claim
that sits in the fair daemon FIFO, so release-drain serves competing suites in order. v0.7 gets there
via the **select-integrated growth claim** (OD1/§4.3): the claim is blocking (fair) but never freezes
the dispatch loop. This repairs the self-reclaim asymmetry that pure probe-gating leaves open.

**Honest bound:** 2·nCPU is the **CPU-bound** ceiling. On a small-RAM box the **RAM** ledger binds
first — total workers = min(2·nCPU, Σ-class-caps-that-fit-the-slice). "~2·nCPU" is a ceiling, not a
guarantee; a heavy-class suite gets fewer, larger workers.

`--aitest-workers=N|auto` is a **per-suite soft cap** *under* aira's governor: the suite never runs
more than N workers, but aira may hold it below N when the shared ledger is contended. `auto` = nCPU
(`__init__.py:207-209`), i.e. "as many as the governor will give me, up to nCPU."

## 7. Reconciliation with the v0.6 interim — what changes

> **AS-BUILT AMENDMENT (2026-09-12, S1 slice v7-1 + build-review).** Two corrections to §7 item 1
> below, from building and adversarially reviewing the guard:
> 1. **`effective_outer_cap` is read at EVERY guard check, NOT once at startup.** It is a few small
>    file reads before a relay fork, and re-reading handles an owner `set-property` on `aira.slice`
>    mid-run for free. (The text below says "read once at startup" — superseded.)
> 2. **The premise "the client already sizes and counts every worker" holds for ONE supervisor per
>    outer scope, and is FALSE for N.** Two+ aitest supervisors under one outer scope is daemon-
>    supported and is exactly the design's `make -j` use case (several pytest targets in one
>    `--delegate-ram` confine job). v7-1's client guard sums only its OWN `self.workers`, so N
>    supervisors each guard only their own Σ and can jointly breach the shared outer cap. v7-1
>    therefore closes the **single-supervisor-per-outer** case only. Closing N-supervisors is a
>    **pre-S2 fork (AIRA-232), alongside OD1 batch-reinstate:** client sibling-sum
>    (`os.listdir(outer_scope)`) + a live `.aira-supervisor/memory.current` read (the allowance's
>    `base + N×per_relay` model breaks when N supervisors share one `.aira-supervisor`) + a
>    race-sized margin (a cross-supervisor TOCTOU with no shared client map — the `Σ(pending)`
>    problem across processes), **vs** a daemon-side per-outer-scope term (a cleaner restore of the
>    S15-deleted scan; the daemon serialises grants, so no client TOCTOU). Latent at v0.6 defaults
>    (2·nCPU·512 MiB ≪ the 48 GiB default outer); live under S2's per-class sizing.

A clean migration. AIRA has no users/data (no compat obligation), so this is mostly **subtractive**
plus a targeted addition, each through the two-loop.

**Add:**
- The `aira_mem` marker reader + `nodeid → bytes` map (§4.1).
- Size-class worker sizing + per-worker headroom + per-class dispatch (§4.2, §5, OD2).
- The **client-side aggregate outer-cap guard** and the dual-ceiling annotation check (§7 items 1–2
  below, OD3) — the load-bearing new correctness code.
- The **select-integrated growth claim** (relay stdout fd into the existing `select()` set, OD1).
- The worker-side `gc.freeze()`/`gc.collect()` for collection speed (§4.4; not an RSS bound).

**Change / delete:**
- `_DEFAULT_MAX_SECONDS` 600 → ~10 (turnover mechanism, §5/§6).
- Flat 512 MiB default → 256 MiB *unannotated incremental* default; flat value stops being the only
  sizing input (OD4; measure the baseline before fixing the number).
- **Delete the batch model entirely** (draft §4.2 reserved-batch, §4.3 prune negotiation, §5
  kick-back, §8 kick-back-vs-crash accounting, the try-acquire, the protocol 11→12 bump). Nothing
  replaces it but the per-class lazy-pull rule (§5) that the shipped one-at-a-time loop already
  supports.

**Retire / reconcile two as-built assumptions that per-test sizing breaks:**

1. **The v0.1 §3.3/§4 "Σ(granted caps under an outer scope) ≤ outer cap" aggregate guard is gone
   as-built, and per-test sizing turns the gap into a whole-suite-kill hazard.** S15 **deleted the
   outer-cap aggregate scan** (`worker_admit.go:461-466`, verified verbatim: *"There is no outer-cap
   aggregate scan any more (S15 deleted it); the outer scope's own memory.oom.group is the
   kernel-side bound on Σ(worker caps) ≤ outer-cap"*); the daemon now checks a request only against
   the **slice ceiling** (`worker_admit.go:473`), and `CreateWorkerScope` writes the requested
   `memory.max` verbatim (`internal/runner/worker_scope_linux.go:57,87`) — its own doc comment at
   `:52` still cites "the daemon's aggregate admission guard," **stale as-built**. With the flat
   512 MiB interim this was invisible (512 MiB × nCPU ≪ any outer cap). **Per-test sizing makes it
   live:** a delegate suite's outer scope always has a finite `memory.max` with `oom.group=1`
   (`confine_linux.go` refuses to launch without one). Σ(live worker caps) is bounded only by the
   *slice* ceiling — so under any explicit `--delegate-ram`/`--memory-max`, or the 48 GiB default vs
   a 64 GiB slice for a large run, workers can be admitted past the **outer** cap, the outer scope's
   `oom.group=1` fires, and it **group-kills the whole delegate subtree** (supervisor + every sibling
   worker = the whole suite) — the exact catastrophe the original §4 invariant existed to prevent. A
   per-annotation check alone (draft OD3 Tier 1) does **not** stop this: it bounds one worker, not
   the running sum.

   **Fix (client-side, in the supervisor):** before spawning any worker, require
   `Σ(this suite's live worker memory.max) + Σ(issued-but-ungranted requests) + supervisor allowance
   + request ≤ effective_outer_cap − headroom`, reading the outer cap as the **min over the scope's
   cgroup ancestry** (`effectiveConfineCap`-style, `internal/runner/confine_linux.go`), read once at
   startup, not a bare `<outer_scope>/memory.max`. The `Σ(pending requests)` term is required so two
   concurrently-pending class claims cannot both pass the guard against the same Σ-live and overshoot
   (§4.3 step 3). Because the kernel bounds each worker at its own cap, Σ(caps) ≥ Σ(actual RSS), so
   this keeps the outer `oom.group` from firing on Σ. **The daemon's own aggregate guard is gone and
   is not being rebuilt here** (a daemon-side per-outer-scope ledger is a larger change; the client
   already sizes and counts every worker, so the guard is nearly free there). The **supervisor
   allowance** term is the subject of a second accepted-gap in §8 (it is bounded, not airtight —
   the supervisor's own `memory.current` charges the outer cap and can grow after the check).

2. **`_observe_worker_usage`'s "every worker in a run is granted the same figure" assumption
   (`supervisor.py:1725-1733`) is false under v0.7** — divergent per-class caps are the norm. The
   AIRA-180 single pool-usage sample fed to `confine-report` (design §9, the peak-RSS estimator's
   feedback) becomes meaningless as one figure. v0.7 reports **per-size-class** samples (the subject
   key gains the class) — a map-key change, not new machinery. (Low-cost: the estimator was already
   deferred as a sizing input, §3.)

**Keep unchanged:** the worker-admit outcome vocabulary + `class`-field classifier
(`supervisor.py:440-564`), one-connection-per-worker-lease + EOF release (design §3/§8),
`pidfd`-observed death + requeue-once (v0.1 §3.6), the version-frozen ARDR re-declare + 2 s restart
freeze (v0.6 §4 as-built), the daemon-unavailable fallback (v0.1 §3.7), JUnit/coverage replay.

## 8. Hard invariants (new or sharpened)

1. **A worker holds one nodeid at a time; `attempts` increments only at real dispatch.** A worker
   that retires (age cap / watermark / class-drained) or crashes leaves its class's **un-pulled**
   nodeids untouched in the queue — there is nothing to "kick back" and no just-finished nodeid to
   double-queue. (This **retires** the draft's kick-back-vs-crash accounting: with no reserved set,
   the shipped requeue-once for a *crashed* in-flight test is the only requeue path.)
2. **No request exceeding a hard ceiling is ever dispatched.** A **request** (`class cap + headroom`
   — the number the daemon writes as `memory.max`, not the bare class cap) > (slice ceiling −
   headroom) **or** > `effective_outer_cap` (min over ancestry) is **terminal-`unevaluated` up front**
   (OD3 Tier 1). And **no worker is spawned that would breach the aggregate guard** of §7 item 1
   (Σ live caps + Σ pending requests + supervisor allowance + request ≤ effective outer cap −
   headroom).
3. **Reservation = containment cap = ledger charge, per worker.** A worker's request, its cgroup
   `memory.max`, and its ledger charge are one number: `class-cap + per-worker headroom`. There is
   **no** decoupling that sizes `memory.max` above the ledger charge (that would let Σ RSS exceed the
   slice ceiling, §4.2). Sequential execution (one test at a time per worker) makes the class cap the
   binding *per-worker* figure; the *aggregate* over one outer scope is bounded by §7 item 1.
4. **The age cap is a between-tests check.** A running test is never interrupted; a > 10 s test
   completes and yields a real result, then its worker retires (§5).
5. **The daemon never learns nodeids.** It knows leases (RAM/CPU) only. Queue/in-flight state lives
   entirely in the supervisor (Python), so a daemon restart never loses or duplicates a test (OD5).
   This is *cleaner* without the batch — a worker holds one nodeid, not a reserved set.

**ACCEPTED GAP — mid-test accumulation OOM can consume the crash budget (must be MEASURED, not
fixed in-design).** With only measured headroom on the cap (no implicit slack), a worker that has
accumulated cross-test residue can, running a heavy test late in its ~10 s window, push resident
memory over `memory.max` **mid-test** — before the between-tests watermark can retire it. That is a
kernel `oom.group=1` kill, which **is a crash** and therefore consumes the test's requeue-once
budget (invariant 1 protects only clean retirements). A genuinely-passing test that is twice unlucky
(lands accumulation-OOM on two successive workers) is marked `unevaluated` while actually passing.
**Distinguishing an accumulation-OOM from a real crash would need the supervisor to read the scope's
`memory.events` `oom_kill` counter — new machinery — so this is NOT fixed in-design.** It is judged
**rare** (headroom + watermark keep workers off the cap, and a requeued test lands on a **fresh,
zero-residue** worker, so "twice unlucky" needs two independent accumulation-OOMs), and **accepted as
a measured gap:** S1 instruments the OOM-kill / requeue rate directly; if it is non-trivial, revisit
(read `memory.events`, or raise headroom, or lower the watermark). This is the residual of retracting
the draft's "RSS is bounded near reservation" claim (§2); the design must own it rather than assert
it cannot happen.

**ACCEPTED GAP — the supervisor-allowance term in the aggregate guard is bounded, not airtight.** The
supervisor runs in `.aira-supervisor` with **no `memory.max` of its own**, so its live
`memory.current` charges the **outer** cap alongside every worker scope. The §7-item-1 aggregate
guard therefore carries a **supervisor allowance** term. A guard that read the supervisor's live
`memory.current` at check time would still be racing growth that happens *after* the check (event
staging, JUnit/coverage replay), so v0.7 reserves a **fixed allowance with margin** — sized from the
supervisor's warm-import `memory.current` plus a replay/staging margin, measured in S1 — rather than a
moving read. This is **bounded, not airtight**: if the supervisor grows past its allowance the outer
`oom.group` could still fire. It is accepted because the supervisor's footprint is dominated by the
COW-shared warm-import set (already resident and shared) and its per-run growth is small and bounded;
S1 measures supervisor `memory.current` under load to set the allowance and confirm the margin. If it
proves large or unbounded, revisit (a supervisor `memory.max`, or a `memory.current`-tracking term).

## 9. Daemon protocol — no change

The wire fields already exist and **v0.7 needs no new fields and no protocol bump.**
`WorkerAdmitResponse` carries `available_bytes`/`available_cpu` (`worker_admit.go:96-97`), a grant
carries `scope_path`/`memory_max`/`containment` (`:69-91`), and the request carries `estimated_bytes`
+ the `nonBlocking` bit (`:100-115,282-291`). The recommended growth mechanism (OD1) is a
**blocking claim** whose relay stdout is moved into the supervisor's `select()` set so it does not
freeze the dispatch loop — a **supervisor-side refactor, no wire change; stays protocol 11.**
**Critically, the non-blocking probe no longer *gates* issuing the claim** (that gating is the exact
cause of the §6 cross-suite fairness race): a blocking claim is issued whenever a class has ready
tests, the aggregate guard (§7 item 1) passes, and no claim for that class is already pending — the
claim then sits in the **fair daemon FIFO** whether or not a snapshot currently shows room. A builder
must not reinstate a "probe-shows-room-first" gate around the claim, or the fairness fix evaporates. The only variant that would need a bump is the **rejected** atomic
try-acquire (OD1 fallback A), where `max_wait_ms==0` would change from "pure snapshot, never grants"
to "grant if fits, else report available" — a contract a protocol-11 client correctly rejects
(`_probe_available` raises on a probe that granted, `supervisor.py:1108-1115`), hence 11→12. Since the
batch/prune the try-acquire served is deleted, that variant is not the recommendation.

---

## OPEN DECISIONS (owner)

### OD1 — the pool-growth mechanism (and whether to retire the owner-described batch/prune flow)

**Context — the fork.** The owner described a batch negotiation: "the supervisor forms a candidate
batch, computes its max, issues a non-blocking admit; if aira refuses it **also** returns available
quota; the supervisor **prunes** oversize tests and retries." Two reviewers, grounded against the
shipped code, showed this negotiation's premise is false (§2): the interim worker already amortises
admission over its whole life (`worker.py:508-564`), so the batch adds no churn/fork saving, and its
sizing benefit is delivered by size-class worker sizing (OD2) without reserving a nodeid set. Under
size-class workers the prune loop also **degenerates** (a homogeneous worker's max does not drop when
you remove a same-size test), so the described negotiation has no work to do.

- **RECOMMENDATION (both structural reviewers converge): retire the batch/prune flow; grow the pool
  with a select-integrated blocking claim, no protocol bump.** Put the growth relay's stdout fd in
  the supervisor's existing `select()` set (§4.3, §9). A growth claim is then a **blocking** daemon
  claim that sits in the **fair daemon FIFO** (release-drain serves competing suites in order — this
  is the real §6 cross-suite fairness fix) **without freezing** the single-threaded dispatch loop and
  **without** the `_ADMIT_READ_GRACE_SECONDS = 5 s` stall (which on a 10 s pool is half a worker's
  life). Stay on **protocol 11**. One pending claim per class per suite; cancel by closing the relay
  stdin when the class drains. **The probe must NOT gate issuing the claim** — a claim goes into the
  FIFO whenever a class has ready tests and the aggregate guard passes (§9); keeping a
  probe-shows-room-first gate leaves the §6 fairness race intact. Verify at build time it does not
  disturb `_pool_covers_the_queue` accounting or placement-ack sequencing.
- **FALLBACK (A): atomic try-acquire (`max_wait_ms==0` becomes grant-if-fits-else-report-available;
  protocol 11 → 12).** Acceptable — AIRA has no compat obligation — but **unnecessary** once the
  batch/prune it served is deleted (the try-acquire existed only to make each prune round atomic).
  S15 removed it precisely because it "COULD grant a worker speculatively"; with no batch that reason
  stands again.
- **REJECTED: shipping (B) with the 5 s grace unchanged on a 10 s pool** — it freezes the dispatch
  loop for up to half a worker's life on a lost race.
- **REINSTATE PATH (fork stays visible):** to restore the owner's original design, re-introduce the
  batch object + the prune negotiation here and take fallback (A) for atomicity; §2 records why this
  was reversed so the reversal is not silently re-litigated.

### OD2 — worker sizing by size-class

The old max-over-batch inflation problem is gone because workers are homogeneous; what remains is how
to bucket.

- **RECOMMENDATION: (c) coarse power-of-2 size-class ladder, applied as WORKER SIZING at spawn (not
  batch formation).** Partition the annotation map into a small fixed ladder (≤256 MiB, ≤512 MiB,
  ≤1 GiB, ≤2 GiB, …, each rounded **up** to the bucket cap). A class-K worker is sized to K's cap +
  headroom and pulls **strict same-class** nodeids one at a time (§5). Strict same-class, **not**
  ≤-class: a big worker draining small tests over-reserves worse than the buckets themselves.
- **Residual (accepted):** rounding a 300 MiB test up to a 512 MiB bucket over-reserves ~70 %, but
  it is bounded to **one bucket step** (unlike the draft's heterogeneous inflation, which was
  unbounded within a batch). Keep the ladder **coarse** — do not add exact-value buckets (they
  fragment: 256/300/512/600 → four tiny buckets that never fill).
- **REJECTED: (a) flat heterogeneous batches.** A single heavy/mis-annotated test at the FIFO front
  poisons the first batch, heavy tests spread one-per-batch, and the slice fills with over-reserved
  workers so concurrency collapses for the **whole run**, not for 10 s (the age cap bounds one
  worker's hold time, not the aggregate).

### OD3 — starvation / livelock + the outer-cap hazard

Three layers; the aggregate guard (new) is the one that closes the whole-suite-kill.

- **Tier 1 — hard-ceiling requests are terminal.** A **request** (`class cap + headroom`, the number
  written as `memory.max` — not the bare class cap) > (slice ceiling − headroom) **or**
  > `effective_outer_cap` is marked **`unevaluated` up front, fail-fast** — the shipped
  `WorkerAdmitReasonExceedsCeiling`/`request-invalid` disposition (`worker_admit.go:473-480`) plus a
  **client-side outer-cap check that reads the effective (min-over-ancestry) cap**
  (`effectiveConfineCap`-style, `internal/runner/confine_linux.go`), **not** a bare
  `<outer_scope>/memory.max`. Mandatory (§7 item 1): without it a heavy annotation kills the whole
  suite.
- **NEW — the aggregate Σ-guard (critical, closes the whole-suite `oom.group` kill).** Before
  spawning any worker: `Σ(this suite's live worker memory.max) + Σ(issued-but-ungranted requests) +
  supervisor allowance + request ≤ effective_outer_cap − headroom` (§7 item 1). Because the kernel
  bounds each worker at its own cap, Σ(caps) ≥ Σ(RSS), so this keeps the outer `oom.group` from
  firing on the running sum. Tier 1 alone does **not** do this — it bounds a single request, not the
  sum. The **`Σ(pending requests)` term is required**: without it, two concurrently-pending class
  claims each pass the guard against the same Σ-live and both get granted, re-opening the kill (§4.3
  step 3). The **supervisor-allowance term is bounded, not airtight** (its `memory.current` can grow
  after the check): reserve a fixed allowance with margin (see the second §8 accepted-gap), or raise
  it if S1 shows the supervisor grows.
- **Tier 2 — eligible-but-too-big-now waits, off the dispatch loop.** A class whose cap ≤ ceilings
  but > current available must **wait**, never busy-spin. It grows via the **select-integrated
  blocking claim** (OD1) — the claim sits in the fair FIFO while the dispatch loop keeps serving live
  workers. **Anti-starvation under heterogeneity:** when growing, pick the class of the
  **oldest-waiting ready test**, so a heavy 2 GiB class cannot starve behind a perpetually-refilling
  256 MiB class grabbing every small free.
  - **Accepted cross-slice trade (state honestly):** a heavy-class claim at the **head** of the fair
    daemon FIFO arms the AIRA-59 head-of-line backfill-freeze, which **stalls lighter backfill for
    every suite on the shared slice** until the heavy class accumulates enough room. This is the
    price of fixing heavy-class starvation within a suite; it is bounded (the heavy class does
    eventually fit as the slice drains) and is the honest cost of using the shared fair mechanism
    rather than a per-suite probe race.

### OD4 — the knobs: ~10 s age cap, 256 MiB default, the watermark, and MAX_TESTS

- **Age cap `AIRA_AITEST_WORKER_MAX_SECONDS ≈ 10` (from 600):** field-tunable starting point. It is
  the **turnover / fairness** mechanism (§6), **not** an RSS bound (§2). No correctness objection.
- **`AIRA_AITEST_DEFAULT_BYTES = 256 MiB` (unannotated *incremental* default):** starting point only.
  **Do NOT fix it before measuring the COW-fault baseline** on a real suite (S1). Physics gate:
  glibc raises `M_MMAP_THRESHOLD` dynamically after freeing a large mmap'd chunk, so even numpy /
  large-buffer workloads shift from mmap (returned) to brk (pinned) after the first cycle and
  accumulate from test 2 — "large allocations are returned" is not safe. If the baseline is large
  (~100 MiB), the watermark fires after a handful of tests and constant recycling both defeats
  admission-amortisation and still leaves mid-test accumulation uncapped. This is a **measure,
  don't reason** item.
- **64 % watermark: co-primary RSS backstop — ALL reviewers agree, keep it active.** It is the
  load-bearing between-tests defence now that the cap carries only measured headroom (§5). Consider
  **lowering** the fraction once the baseline is measured.
- **`AIRA_AITEST_WORKER_MAX_TESTS` (200): GENUINE REVIEWER CONFLICT — surfaced, not resolved.**
  - **Lead recommendation: DELETE it.** The between-tests `_should_recycle` already runs the 64 %
    watermark every test, which is the **direct measurement** of resident memory that MAX_TESTS only
    **proxies** by counting; the ~10 s age cap subsumes its turnover role; and AIRA has no compat
    obligation, so a wired-but-off knob is pure carried complexity. (Reviewer 3.)
  - **Dissent (must be visible): KEEP it active.** The count is the **direct accumulation bound** — it
    bounds the number of residue-leaving *events*, whereas the ~10 s **time** cap is a proxy that
    fails precisely for a fast unit suite running **hundreds** of tests per 10 s window (many residue
    events, no time-based retirement). The watermark catches climb, but a MAX_TESTS default is the
    cheap event-count belt. (Reviewer 2.)
  - **How to break the tie in the field:** S1 measures per-test residue and the OOM/requeue rate on a
    fast unit suite. If the watermark alone holds RSS flat across hundreds of fast tests, delete
    MAX_TESTS; if fast suites show accumulation the watermark misses between checks, keep a
    MAX_TESTS default active. Do not carry two overlapping *primary* triggers regardless — the age
    cap is the primary turnover trigger either way.

**The > 10 s single test (owner's explicit sub-question):** recycle is between-tests only (§5), so a
worker **finishes the in-flight test, then** retires on age. The long test yields a **real** result
and is **never interrupted**. A test that alone always exceeds 10 s retires its worker after each such
test — acceptable churn for a genuinely slow test. Unchanged by dropping the batch.

### OD5 — interaction with socket-liveness + reconnect/re-declare across a daemon restart

A ~10 s worker holds a **lease** (one connection per worker, supervisor-held via its `aira
worker-admit` relay, design §8). A daemon restart (usually an upgrade) lands: the new daemon starts
with an **empty ledger + a 2 s new-admission freeze**, and each surviving relay reconnects (2/sec) and
re-declares its version-frozen **ARDR** frame to re-anchor its lease (v0.6 §4 as-built).

**RECOMMENDATION: no new machinery — verified sound against the shipped `leaseKeeper`, and *simpler*
without the batch.** Spec these consequences explicitly:

1. **Nodeid state is never at risk (cleaner than the batch model).** The daemon knows only leases,
   never nodeids (§8 inv 5). A worker now holds **one** nodeid at a time (not a reserved batch), so a
   restart can cost at most a lease re-anchor, never a lost or duplicated test.
2. **A worker RETIRING during the reconnect window releases cleanly.** Short-lived workers frequently
   hit the age cap during the 2–4 s restart/reconnect window. The supervisor closing that worker's
   relay stdin = connection EOF = lease release; a re-declare that races the close is resolved by the
   shipped **compare-and-release** (reviewers verify: `k.conn`-always-closable + adopt-under-lock +
   Close, `lease_keeper_linux.go:100-161`), and a late re-declare after the lease is already gone is
   absorbed by the **signed ledger** going momentarily negative and self-correcting (v0.6 §2). The
   `maxNoAck` give-up (`lease_keeper_linux.go:275-283`) is **benign** for a ~10 s worker: the job
   keeps running under its cgroup cap (never falls open), with only a brief ledger under-count until
   it retires ~10 s later; the watchdog is the backstop.
3. **New-worker admission stalls ~2 s during the freeze; honest, not a bug.** A ~10 s pool churns
   fast, so the freeze briefly stalls *replenishment*. The growth path treats admission-unavailable
   as **transient** (skip the tick, never disable — S16 / `_try_grow_one`, `supervisor.py:1937-1938`),
   and the empty-pool blocking claim rides the freeze out with a periodic on-output notice
   (`_read_line_blocking_with_progress`, `supervisor.py:207`). v0.7 inherits both unchanged.

**Build-time measurement (report-as, not verified here):** a suite churning ~2·nCPU short-lived relays
reconnecting inside the 2 s freeze on localhost (v0.6 §13's own open "REMAINING" item). Keep it as a
measurement, not a design risk.

## 10. Test plan (delta over v0.1 §7)

- **Annotation reader:** `aira_mem` parsed for the size grammar; unannotated → default; invalid →
  warn-once-and-default; `--strict-markers` clean; semantics = incremental peak (assert the cap is
  `class-cap + headroom`, not bare `max(annotation)`).
- **Sizing + aggregate guard (OD3, critical):** a class-K worker's `memory.max` == class cap +
  headroom == its ledger charge (no decoupling); an annotation > effective outer cap (min over
  ancestry) is terminal-`unevaluated`; **and the aggregate guard holds — spawning workers whose Σ
  caps would exceed the effective outer cap is refused, so the outer scope's `oom.group` NEVER fires
  from over-admission** (the §7 hazard — the load-bearing new test).
- **Per-class dispatch (OD2):** a class-C worker is handed only class-C nodeids (strict same-class);
  a worker whose class drains before the age cap is `__stop__`ped promptly; a worker that crashes or
  retires leaves un-pulled class nodeids in the queue with **no double-queue and no lost test**;
  `attempts` increments only at real dispatch.
- **Age cap + accumulation (OD4):** a worker retires ~10 s in; a > 10 s single test completes with a
  real result then retires; a fast unit suite of hundreds of tests either stays flat under the
  watermark (→ MAX_TESTS delete) or shows accumulation the watermark misses (→ MAX_TESTS keep) —
  **measured, feeds the OD4 fork**; the accumulation-OOM/requeue rate is instrumented (§8 accepted
  gap).
- **Growth claim (OD1):** a blocking growth claim on a contended slice does **not** freeze the
  dispatch loop (live workers keep getting nodeids while a claim is pending); the heavy class grows on
  the oldest-waiting-test rule and does not starve behind a refilling light class.
- **Restart (OD5):** a restart mid-run loses no nodeid and double-counts none; a worker retiring
  inside the reconnect window releases cleanly; the signed ledger absorbs a late re-declare. Reuses
  the v0.6 restart-under-load merge-gate harness.
- **Cross-suite (§6):** two concurrent suites converge to ≤ 2·nCPU total workers; a newly-started
  suite acquires slots via the fair FIFO growth claim (not only after an incumbent's probe race), and
  latency to first slot improves with ~10 s turnover.

## 11. Staging

1. **S1 — annotation + reader + sizing + the critical guards + MEASUREMENT.** The `aira_mem` reader,
   size-class worker sizing (OD2) with per-worker headroom, OD3 Tier 1 (dual-ceiling incl. the
   effective-outer-cap check) **and the aggregate Σ-guard** (the load-bearing correctness code), the
   ~10 s age cap, the watermark kept active. One-annotated-class-per-worker on the shipped
   one-at-a-time loop (no growth-claim refactor yet). **Instrument** worker RSS-vs-cap, the
   OOM-kill/requeue rate, fast-suite residue, and **supervisor `memory.current` under load** — these
   decide 256 MiB, the per-worker headroom, the watermark fraction, the supervisor allowance (§8
   second accepted-gap), and the MAX_TESTS fork (OD4).
2. **S2 — the growth mechanism + per-class scheduling.** The select-integrated growth claim (OD1, no
   protocol bump), per-class `_dispatch_to_idle_workers`/`_pool_covers_the_queue`, the
   oldest-waiting-class anti-starvation rule (OD3 Tier 2), prompt `__stop__` on class drain. This is
   the fairness/latency layer; it adds **no** batch, prune, kick-back, try-acquire, or wire change.
3. **S3 — feedback + polish.** Per-size-class pool-usage sample (§7 item 2), watermark/age/default
   tuning from S1/S2 field data, cross-suite fairness soak. Decide the MAX_TESTS fork from S1 data.

Dogfood on AIRA's own suite first, exactly as v0.1 §8.
