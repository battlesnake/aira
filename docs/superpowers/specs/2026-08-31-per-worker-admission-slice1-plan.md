# Per-worker admission — Slice 1: shrink the per-test forward-growth over-reservation

**Status:** plan v2 (post plan-review: Sol BLOCK + Fable FAIL folded, DeepSeek nits)
**Branch:** `per-worker-admission-slice1`
**Relates:** AIRA-24 (saturation UX — safe subset folded), AIRA-17 (cold-start → Slice 2)
**Author:** Opus, grounded on a 4-reader understand pass + the Sol/DeepSeek/Fable two-loop.

## 1. Problem

`--delegate-ram` test runs get **blocked despite ample RAM**. Admission charges
`charge = max(memory.current − reclaimable, outstanding + adopted)` (`admit.go:743-760`),
where `memory.current − reclaimable` (reclaimable = `active_file + inactive_file`,
`admit.go:1032-1056`) is the honest cache-discounted live RSS (call it `effectiveCurrent`),
and `outstanding` is the summed *forecasts*: the 512MiB job overhead **plus one per-test
reserve per active test**, each `max(estimate, RSS + growth_headroom)` with
`growth_headroom = 512MiB` and an injected `AIRA_TEST_MEM_DEFAULT = 512MiB`
(`__init__.py:18,258-265`, `env.go:30,92-97`).

## 2. Root cause (precise, code-grounded)

For an unmarked test `RSS + 512MiB ≥ 512MiB` always, so the reserve is effectively
`RSS + 512MiB`; one is held per worker-in-test across the test (`__init__.py:395,409-411`).
Summed over ~N active workers:
`outstanding ≈ Σ(RSS_i) + N·512MiB + 512MiB_job`. Because `charge` is a **max** (not a sum),
the `Σ(RSS_i)` term does not double-count against `effectiveCurrent` — it resolves to
`charge ≈ effectiveCurrent + N·512MiB`. **The dominant false-block term is the summed
forward-growth headroom `N·512MiB`** (~8GiB at `-n16`): a fixed 512MiB of forward
reservation per active worker, charged even when a test does not grow.

**Two secondary terms (named for honesty; the fix removes only the dominant one):**
(a) `Σ(RSS_i)` **exceeds** `effectiveCurrent` by roughly `Σ(file-resident_i)` (~0.5–2GiB at
`-n16`): each worker's mapped file pages (shared libs, mmapped fixtures) are in its `statm`
RSS once per process, but are discounted out of `effectiveCurrent` by AIRA-21's
`active_file+inactive_file` subtraction. This over-charge **survives Model 1** and
strengthens the Slice-2 case. (b) Reserves freeze acquisition-time RSS while `current`
grows, and between-test workers hold nothing — both push the other way.

## 3. Design — Model 1: reduce the per-test forward padding (keep the `max` model)

The reserve headroom is **best-effort forward padding above the honest current-floor**, not
a hard forward bound. Shrink the padding so `outstanding` stops gratuitously exceeding
`effectiveCurrent`, and let the `max`'s current-floor carry OOM-safety.

### 3.1 Sizing change (`aira_xdist_governor/__init__.py`)

```
rss    = worker current RSS (/proc/self/statm, as today)
pad    = GROWTH_PAD           # AIRA_TEST_MEM_GROWTH_HEADROOM, default 512MiB → 128MiB
marker = aira_mem(item)       # absolute peak, UNCHANGED semantics
reserve = marker is not None ? max(marker, rss + pad) : rss + pad
```

- **Drop the flat 512MiB `AIRA_TEST_MEM_DEFAULT` floor for unmarked tests** — replace it
  with `rss + pad`, so a low-RSS worker is no longer charged a flat 512MiB (the miss Sol/
  Fable caught). Marked tests still reserve `max(marker, rss + pad)` so a known peak is
  protected. Markers stay ABSOLUTE (no re-spec; that is Slice 2).
- Summed forward padding drops from `N·512MiB` to `N·128MiB`; the low-RSS default floor is
  gone. This is the everyday relief.

### 3.2 Why it is OOM-safe (the CORRECT rationale — current-floor, not a forward invariant)

`charge = max(effectiveCurrent, outstanding + adopted)` (`admit.go:750-755`) **always
charges at least `effectiveCurrent`** — the honest live cache-discounted RSS. So admission
never grants while the slice's real usage is already within headroom of the cap, regardless
of how small `outstanding` is. `outstanding ≥ current` is **NOT** an invariant (it fails on
between-test workers, mid-test growth after acquisition, the xdist controller and
test-spawned subprocesses that are in slice `current` but in no per-test reserve, and the
AIRA-21 discount) — and the plan does **not** rely on it. The reserve headroom is only
forward *padding* above that floor: cutting 512→128MiB raises a test's **unreserved
concurrent-growth exposure** from `>512MiB` to `>128MiB` per test — a bounded pacing trade,
not an OOM hole.

**Honest containment note.** New exposure is **in-slice only**. Under-reserved concurrent
growth can breach the slice `memory.max`; the kernel then OOM-kills inside the slice, and
`oom.group` contains the kill to one scope — but for `@dr` jobs whose per-scope ceiling
(48GiB, `confine.go:26`) exceeds the slice, the **binding** limit is the slice cap and the
group-killed victim can be a **sibling** scope, not the overgrower. This is a small,
bounded re-widening of #67's random-victim strictness — **already possible today** whenever
a test grows >512MiB above its reserve; Model 1 widens the window from >512MiB to >128MiB.
The per-scope `memory.max` + `oom.group` machinery is otherwise untouched (no scope-creation
change), so there is **no host-OOM path**. Post-deploy we **monitor the per-scope
oom.group kill rate** (DeepSeek); if it rises materially, raise `pad` or advance Slice 2.

### 3.3 Accepted limitation → Slice 2 (cold start)

No per-test reserve covers a worker's **baseline** (imports) during a wide-`-n` cold start
before it shows in `current`; the 512MiB job overhead absorbs narrow `-n`, wide `-n` is a
documented gap Slice 2 closes with the lazy per-worker base (= AIRA-17). Slice 1 does not
regress here (today's per-test reserves are also acquired only after a worker runs).

## 4. Saturation-wait UX (AIRA-24 — SAFE subset folded; racy parts deferred)

Fable confirmed the daemon already honours a per-request `max_wait_ms` (`admit.go:896-904,
468-489`) with no daemon change. Slice 1 folds only the parts that are sound client-side:

1. **Configurable shorter faster-fail** — `--admit-timeout <dur>` on `aira confine`
   threads into the client's `admissionMaxWait` (`admission_linux.go:341`,
   `runnerAdmitWaitCap` cap unchanged). It must be a **positive** duration; the default is
   unchanged (30 min). **NOT** a zero/`--no-wait`: `confine_linux.go:803-805` coerces
   `maxWait ≤ 0` to 30 min (the inverse of "reject now"), and a literal 0 races the
   enqueue-kick evaluator against an immediately-firing deadline (`admit.go:468-489` vs
   `queue.signal()` :573) → a job that could admit now would flakily reject. **Zero-wait
   `--no-wait` is DEFERRED** to AIRA-24 proper, where it needs an explicit NoWait flag +
   an evaluate-before-reject protocol change, not a client timeout.
2. **Clearer terminal reject** — on `reject:saturated`, print an explicit line ("admission
   rejected after Ns — slice genuinely saturated") so a reject is not mistaken for "still
   running".
3. **Waiter visibility** — extend the existing client periodic waiting line
   (`confine_linux.go:489-505`, printed by a client goroutine while the admit socket
   blocks) to include the slice granted-reserve/ceiling via a **second** daemon connection
   per tick (the #73 `--list` reserve query) — do NOT multiplex the blocked admit socket.
   Note: it explains reserve-contention, not the `effectiveCurrent` half of the `max`.

Deferred: precise queue-position/ETA, admitted-with-backpressure, and daemon-side
`--no-wait` (AIRA-24 proper). On the **flock fallback** path (daemon down) a short/zero wait
yields state `timeout` and the job **launches without a lock** (`admission_linux.go:211-214`
→ `confine_linux.go:526-527`): document that `--admit-timeout`'s hard behaviour is
daemon-path only; the fallback keeps today's advisory launch.

## 5. Scope boundaries — unchanged

Daemon ledger accounting (`outstanding`/`adopted`, `checkedAvailable`, `evaluateAdmitQueue`)
is untouched — only the plugin's reserve *values* and the client `--admit-timeout`/reject
strings change. The 512MiB job overhead, the governor, AIRA-21's discount, non-delegate
whole-job reserves, and all per-scope cap/oom.group machinery are untouched. No per-worker
base, no daemon charge change, no marker re-spec, no class-split ledger (all Slice 2).

## 6. Tests (true properties — NOT the false invariant)

1. **Reduced sizing** (pylib real-pytest, like `TestRealPytestRAMReservationUsesMeasuredRSS`):
   an unmarked test reserves `rss + 128MiB` (the 512MiB default no longer wins for low RSS);
   a marked test reserves `max(marker, rss + 128MiB)`. Pin the new pad and the dropped
   default; compare against the OLD `rss + 512MiB` to prove the reduction.
2. **Reduced false-block** (daemon): with several active reduced-pad grants and an honest
   `effectiveCurrent`, a new small job admits where the OLD 512MiB-pad grants would gate it
   — a discriminating `evaluateAdmitQueue` test proving the summed padding no longer falsely
   saturates. (This does not assert `outstanding ≥ current`.)
3. **Current-floor preserved** (daemon/unit — the TRUE safety property, replacing the porous
   invariant test): `checkedAvailable` never returns headroom that would admit while
   `effectiveCurrent` is already within `headroom` of `maximum`; i.e. `charge ≥
   effectiveCurrent` for any `outstanding`, including `outstanding < effectiveCurrent`.
4. **`--admit-timeout`** (positive): a job that cannot admit within a short positive timeout
   rejects with `E_ADMIT_SATURATED` on the daemon path; the default (no flag) still waits
   30 min; a `≤ 0` value is rejected at the CLI (not coerced to 30 min). Fallback-path note
   asserted separately (advisory launch, no hard reject).
5. **Reject / visibility strings** — terminal reject states saturation; the waiting line
   carries reserve/ceiling from the second connection.
6. Full daemon + runner + pylib suites green under `aira confine`, `-race` clean.

## 7. Expected yield

Steady-state `--delegate-ram` runs stop false-blocking on the summed `N·512MiB` forward
padding when real RAM is ample (the everyday win), and low-RSS workers stop paying the flat
512MiB default. Genuinely-saturated runs get a configurable faster-fail + a legible reject.
No host-OOM exposure; a bounded, monitored increase in contained per-scope kills. The
`Σ(file-resident)` secondary over-charge and cold-start remain for Slice 2.

## 8. Rollout

The plugin is `go:embed`ded → a binary rebuild + swap + `aira skill install` ships the
sizing change; `--admit-timeout` + reject/visibility strings are client-side. **No daemon
restart is required** (confirmed: no daemon-side change). Deploy watched; monitor the
oom.group kill rate.

## 9. Slice 2 (outline, not built here)

Model 2 (per-test **increment** `max(marker−RSS, floor)` + a **class-split** ledger charging
delegate-ram delta-class as `current + Σincrement` while whole-job peak-class keeps
`max(current, reserve)` — Fable verified summing the same sliceQueue would double-book
peaks) **coupled** with the lazy **per-worker base** (~256MiB, charged as each worker
registers with the governor, keyed by scope id → covers cold-start = AIRA-17) and the
`Σ(file-resident)` shared-page handling. DeepSeek's **shared/capped growth pool**
(`current + overhead + min(N,k)·pad`) is a candidate here to cap the N-scaling entirely.
Plus daemon-side `--no-wait` (AIRA-24 proper). Specced separately after Slice 1 soaks.
