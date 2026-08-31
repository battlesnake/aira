# Per-worker admission — Slice 1: per-test forward INCREMENTS (lean on memory.current) + saturation-wait UX

**Status:** plan (pre-review)
**Branch:** `per-worker-admission-slice1`
**Relates:** AIRA-24 (saturation UX, folded here), AIRA-17 (cold-start → Slice 2)
**Author:** Opus, grounded on a 4-reader understand pass + owner design dialogue.

## 1. Problem

`--delegate-ram` test runs get **blocked despite ample RAM**. The admission charge is
`charge = max(memory.current − reclaimable, outstanding + adopted)` (`admit.go:743-760`)
— a **max**, where `memory.current` is the honest real-time RSS (cache-discounted post
AIRA-21) and `outstanding` is the summed *forecasts*: the 512MiB job overhead **plus one
per-test reserve per active test**, each sized to the worker's **cumulative RSS + 512MiB
growth** (`aira_xdist_governor/__init__.py:258-265`). Because each per-test reserve
re-includes the whole worker RSS, `outstanding` climbs **above** the honest
`memory.current`, the `max` picks the inflated forecast, and admission gates on
reserve-contention while real RAM is free (the subpipe/altium report, and AIRA-24).

## 2. Root cause (precise)

Each per-test reserve is `max(estimate, RSS + growth_headroom)`; for an unmarked test that
is `RSS + 512MiB`. Summed over the ~N active tests (one per active worker),
`outstanding ≈ Σ(RSS_i) + N·512MiB + 512MiB_job`. The `Σ(RSS_i)` term ≈ the slice's own
`memory.current` (each worker's RSS, counted once), so the `max` does **not** double-count
RSS — it resolves to `charge ≈ memory.current + N·512MiB`. **The false-block is the summed
forward-growth headroom `N·512MiB`** — a fixed 512MiB of forward reservation per active
worker, charged even when a test does not actually grow. At `-n16` that is ~8GiB of
head-of-line reservation on top of the honest `memory.current`, which is what tips the
`max` above real usage and gates a new job while RAM is free.

So the target is: **reduce the summed forward-growth over-reservation while keeping the
charge a sound forward bound**, with the per-scope `memory.max` + `oom.group` as the HARD
OOM backstop (the reserve is a pacing heuristic — a scope that outgrows its cap is
`oom.group`-killed, contained; the reserve exists to avoid gratuitously hitting that).

## 3. Design — shrink the forward-growth over-reservation (the two candidate models)

The plan-review must pick between two models; both keep AIRA-21's reclaimable discount and
the per-scope cap/oom.group backstop untouched. My lean is **Model 1** for Slice 1
(simple, sound, delivers the everyday win) with Model 2's structural change deferred to
Slice 2 where it is coupled to the per-worker base.

### Model 1 (RECOMMENDED for Slice 1) — keep the `max` model, shrink/adapt the growth headroom

Keep the reserve form `RSS + growth` (so `outstanding ≥ memory.current` and the existing
`charge = max(current, outstanding)` stays a valid forward bound — no daemon change), but
cut the per-test **growth headroom** from a flat 512MiB to a smaller default (proposed
128MiB, `AIRA_TEST_MEM_GROWTH_HEADROOM`) and let a marked test still reserve its absolute
`max(marker, RSS + growth)`. This shrinks the summed over-reservation ~4× (N·512→N·128)
so far fewer runs false-block, while `outstanding ≥ current` keeps every test's forward
growth reserved. Cost: a test that grows > headroom above its RSS may hit its per-scope
cap (contained `oom.group` kill) more often — bounded and the accepted trade. Change is a
one-constant tune + tests. **Sound by construction (outstanding never drops below current).**

### Model 2 (structural, Slice 2) — per-test forward INCREMENT + `current + Σincrement`

Size the reserve as the *increment* `max(marker − RSS, floor)` (drop the `RSS +` term) so
`outstanding` is only pending growth, AND change the daemon charge from
`max(current, outstanding)` to a **sum** `current + outstanding` for the increment class,
because with pure increments `outstanding` falls below `current` and the `max` would stop
reserving pending growth (concurrent admit-then-grow could overshoot). This is the tight,
elegant model — but it is a real daemon change that must NOT break **whole-job reserves**,
which are peaks that correctly want `max(current, reserve)` (summing a whole-job peak with
its own `current` double-books). So Model 2 needs the ledger to distinguish delta-class
(delegate-ram per-test) from peak-class (whole-job) charges — genuinely more machinery,
and it pairs naturally with the Slice-2 per-worker base (baseline held per worker,
increment per test). **Deferred to Slice 2.**

### 3.3 Accepted limitation → Slice 2 (cold start)

Neither model reserves a worker's **baseline** (imports) during a wide-`-n` **cold start**
before it shows in `memory.current`; for narrow `-n` the 512MiB job overhead absorbs it,
for wide `-n` it is a documented gap Slice 2 closes with the lazy per-worker base
(= AIRA-17). Slice 1 (Model 1) does not regress today here — today's per-test reserves are
also acquired only after a worker starts running.

## 4. Saturation-wait UX (AIRA-24, folded — focused subset)

When the slice is **genuinely** saturated a big reservation still cannot run; the friction
is the blind 30-min wait then silent `E_ADMIT_SATURATED`. Slice 1 folds the high-value,
low-risk parts:

1. **Configurable faster-fail** — a `--admit-timeout <dur>` flag on `aira confine`
   (and `--no-wait` = timeout 0 → immediate `E_ADMIT_SATURATED` if it cannot admit now),
   threading into the existing `maxWait` (`admission_linux.go:82,251-261`) which is a
   fixed 30-min cap today. Default unchanged (30 min) so existing behaviour is preserved.
2. **Waiter visibility** — extend the existing client periodic line (#71:
   "waiting for memory admission (reserve X, waited Ns)") to include the slice's granted
   reserve / ceiling (the daemon already computes this for `--list`, #73), so the operator
   sees *why* it waits (reserve-contended vs genuinely full).
3. **Clearer terminal reject** — the client already exits 1 with `reject:saturated`; make
   the final message explicit ("admission rejected after Ns: slice reserve X/Y across N
   jobs — genuinely saturated") so a reject is not mistaken for "still running".

Deferred (Slice 2 / later): a precise queue-position/ETA and an admitted-with-backpressure
mode — heavier, not needed for the immediate relief.

## 5. Scope boundaries — what stays unchanged

- The daemon ledger accounting (`outstanding`/`adopted`, `checkedAvailable`,
  `evaluateAdmitQueue`) is **untouched** — only the *values* the plugin reserves change.
- The 512MiB per-job `DefaultDelegateRAMOverhead` stays (coarse cold-start buffer +
  controller overhead) until Slice 2 reconsiders it.
- The governor (park/activate, RAM-ordering, AIRA-21 `admitAvailable` read) is untouched.
- Non-delegate whole-job reserves are untouched.
- No per-worker base, no governor charging, no marker re-spec (all Slice 2).

## 6. Tests

1. **Reduced growth-headroom sizing** (pylib, Model 1): an unmarked test reserves
   `RSS + growth` with the NEW smaller `growth` (128MiB), and a marked test reserves
   `max(marker, RSS + growth)`; a discriminating real-pytest test (like the existing
   `TestRealPytestRAMReservationUsesMeasuredRSS`, `internal/pylib`) pins the new headroom
   and proves `outstanding ≥ memory.current` still holds.
2. **Reduced false-block admission** (daemon): with several active reduced-headroom grants
   + an honest `memory.current`, a new small job admits where the OLD 512MiB-headroom
   grants would have gated it — a discriminating test through `evaluateAdmitQueue` proving
   the summed forward reservation no longer falsely saturates.
3. **Forward-bound preserved** (daemon/unit): `outstanding ≥ memory.current` still holds
   under Model 1, so every active test's growth stays reserved (no concurrent-overshoot
   regression); the per-scope cap/oom.group remains the hard backstop.
4. **Faster-fail** (`--no-wait` / `--admit-timeout`): a job that cannot admit now rejects
   immediately with `E_ADMIT_SATURATED` under `--no-wait`; default (no flag) still waits.
5. **Reject/visibility strings** — the waiting line carries reserve/ceiling; the terminal
   reject states saturation explicitly.
6. Full daemon + runner + pylib suites green under `aira confine`, `-race` clean.

## 7. Expected yield

Steady-state `--delegate-ram` runs stop false-blocking on reserve-contention when real RAM
is ample (the everyday win). Genuinely-saturated runs get a faster, legible reject. No new
host-OOM exposure. Wide-`-n` cold start unchanged (Slice 2).

## 8. Rollout

Plugin change is `go:embed`ded → a binary rebuild + swap + `aira skill install` ships it;
the CLI flags + the AIRA-24 waiter/reject strings are client-side; the `--admit-timeout`
wiring is client-side (the daemon already honours the client's `max_wait_ms`). **No daemon
restart is required for the sizing change** (it is entirely client/plugin side) — confirm
in review whether any daemon-side change (none planned) sneaks in. Deploy watched.

## 9. Slice 2 (outline, not built here)

Lazy **per-worker base** (~256MiB, `AIRA_DELEGATE_RAM_WORKER_BASE`) charged as each xdist
worker registers with the governor (keyed by `jobID == scope id`), released on worker
teardown — a NEW governor-side ledger charge (the governor is read-only today). Covers the
cold-start baseline (AIRA-17). Requires deciding: fold/shrink the 512MiB per-job overhead,
the base-vs-increment interaction (base = baseline held per worker; increment = growth per
test, already Slice 1), and marker reconciliation. Specced separately after Slice 1 soaks.
