# AIRA-21 — Discount reclaimable page cache from the confine-admission charge

**Status:** plan v2 (post plan-review; gate-clean)
**Ticket:** AIRA-21 (P2 bug)
**Branch:** `aira21-cache-discount`
**Author:** Opus (coordinator), grounded on a 4-reader understand pass + the
Sol / DeepSeek / Fable plan-review two-loop.

## 1. Problem

A light `aira confine` (4 GiB reserve) waited 135 s+ for memory admission and
never got it, despite ~41 GiB `MemAvailable` and ~30 GiB of reservation headroom,
with a single large 32 GiB-reserve job present. Reported live by a dogfooding
session ("speed"), 2026-08-31.

## 2. Root cause (evidence-grounded)

Admission's live-room gate is `checkedAvailable` (`internal/daemon/admit.go:741`):

```go
ceiling := maximum - headroom
charge  := outstanding            // Σ granted reserves + adopted
if current > charge { charge = current }
available := ceiling - charge     // reject waiter if reserve > available
```

`current`/`maximum` come from `readSliceMemory` (`admit.go:990`), which reads the
cgroup's `memory.current` and `memory.max`. **`memory.current` includes reclaimable
page cache** (the cgroup-v2 file LRU). Under a heavy-I/O job the file cache balloons
and pushes `memory.current` toward the ceiling, so `charge = max(current, reserves)`
sees the slice as nearly full and rejects a new small job — even though that cache is
instantly reclaimable and the machine has abundant real RAM.

At speed's stall a 4 GiB reject forces `charge > ~57.9 GiB`, hence
`memory.current > ~57.9 GiB`, while only ~37 GiB was truly used machine-wide
(`MemAvailable` 41 GiB) — so the bulk of that ~58 GiB was reclaimable cache the
kernel would evict on demand. A live read of `aira.slice` during triage confirmed the
composition: `memory.current` 29.1 GiB = `anon` 18.5 GiB + `file` 10.4 GiB.

This is the **safe direction** (it over-rejects, never OOMs) but it spuriously blocks
real work — and heavy-I/O is exactly when confine is most needed.

**Not a Slice-3 regression.** The Slice-3 RAM-ordering read (`admitAvailable`,
`admit.go:184`) is strictly read-only; the authoritative charge path
(`evaluateAdmitQueue`) is the untouched #67/#74 logic. No rollback is implicated.

## 3. Design

Charge the **non-reclaimable working set** instead of raw `memory.current`, at the
slice level only:

```
reclaimable      = clamp≥0( inactive_file + active_file )     (from memory.stat)
effectiveCurrent = subtractFloor(memory.current, reclaimable) (clamp ≥ 0)
charge           = max(outstanding + adopted, effectiveCurrent)
```

Under `memory.max`, the kernel reclaims the file LRU (clean pages dropped, dirty pages
written back and dropped — without swap, since file pages have a real backing store)
**before** it OOM-kills. So bytes on the file LRU are not a hard OOM floor, and removing
them from the actual-pressure term is sound. The reserve sum stays an operand of the
`max`, so `charge ≥ outstanding + adopted` unconditionally — the discount only relaxes
the actual-pressure floor, never the reserve invariant.

### 3.1 The load-bearing safety constraint: LRU counters only, never `file`

Subtract **`inactive_file + active_file`** (the kernel's file-LRU counters), **never**
the memory.stat `file` total. `file` = file LRU **+ shmem**. Shmem/tmpfs/`MAP_SHARED`
folios are `PageSwapBacked` → they sit on the *anon* LRU and are reclaimable only to swap
(here a shared 2 GiB pool), i.e. anon-like. Subtracting `file` would discount shmem and
open a genuine OOM hole. Using the two LRU counters keeps shmem, `unevictable`
(mlocked), kernel slab, pagetables, and sock as non-reclaimable pressure — all correct.

**Dirty/writeback caveat (honest).** `file_dirty`/`file_writeback` pages *are* inside
`inactive_file+active_file` and are reclaimable-but-not-instant. This is still OOM-safe
in the ordinary case: `memory.max` forces writeback and throttles dirtiers before OOM. But
it is not free — under sustained heavy dirty write load the discount can admit a job that
then meets *throttling* (slowdown), and in a pathological edge (writeback wedged on a dead
filesystem) a slice-internal OOM. We therefore do **not** claim clean per-scope
containment for the discounted bytes (see §4). Subtracting only clean cache
(`(inactive_file+active_file) − file_dirty − file_writeback`) is a known refinement,
**deferred** (§8) to keep the change minimal per the architectural-simplicity rule; revisit
if field data shows dirty-driven throttling.

### 3.2 Mechanics

1. **New parser** `parseSliceMemoryStat(data []byte) (inactiveFile, activeFile int64, ok bool)`
   in `admit.go`, modelled on `parseMemAvailable` (`watchdog.go:150`): split on `\n`,
   `strings.Fields` per line, **guard `len(fields) < 2` (malformed line → skip)**, match
   `fields[0]` against `inactive_file` / `active_file`, `strconv.ParseInt(fields[1],10,64)`.
   memory.stat values are raw bytes (2-field `key value` lines, no `kB` unit → no
   conversion). `ok` only if **both** fields were found and parsed **≥ 0**. The returned
   reclaimable is `addClamp(inactiveFile, activeFile)` (saturating; a corrupt file can
   never overflow to a negative sum).

2. **Extend `readSliceMemory`** (`admit.go:990`) to also read `memory.stat` from the same
   dir and return a fifth value `reclaimable int64`. New signature:
   `readSliceMemory(path) (current, maximum, reclaimable int64, ok bool, reason string)`.
   - `memory.current` / `memory.max` unreadable / unbounded (`"max"`) / parse-error →
     `ok=false` (HARD fail-closed, exactly as today — waiters stay queued). The existing
     raw `current < 0` / `maximum < 0` guards are preserved **ahead of** any discount.
   - `memory.stat` missing / unparseable / either LRU field absent → `reclaimable = 0`,
     `ok=true` (SOFT — degrade to today's raw-current behaviour; **never fabricate a
     discount**). Emit a **throttled one-time log** on first soft-degrade per queue
     (modelled on the `adoptedScanFailed` one-shot log at `admit.go:632-636`) so the field
     behaviour is not silently different — addresses the observability nit.

3. **Widen the seam.** `admitReadMemory` (`server.go:95`) becomes
   `func(string) (current, maximum, reclaimable int64, ok bool, reason string)`. All four
   consumers select it via the existing `readMemory := s.admitReadMemory; if nil {
   readSliceMemory }` idiom — no call-site restructuring, only the destructure arity:
   `admitAvailable` (:197), `admitConnection`'s pre-enqueue read (:412; a plain
   `current,max` reader — **not** the adopt path, which is the confine scan inside
   `evaluateAdmitQueue`), `evaluateAdmitQueue` (:692), and the confine-list reserve summary
   (`confine_manage.go:123`). The two that discard `current` with `_` keep discarding it.

4. **Discount in `checkedAvailable`.** New signature
   `checkedAvailable(current, maximum, reclaimable, outstanding, headroom int64) int64`.
   Guards, in order (fail-closed / never-fabricate-a-discount):
   ```go
   if current < 0 || maximum < 0 || outstanding < 0 || headroom < 0 || maximum <= headroom {
       return 0
   }
   if reclaimable < 0 { reclaimable = 0 }          // negative ⇒ NO discount, never a full one
   effectiveCurrent := subtractFloor(current, reclaimable) // clamp ≥ 0 (non-atomic skew)
   charge := outstanding
   if effectiveCurrent > charge { charge = effectiveCurrent }
   ...
   ```
   The `reclaimable < 0 → 0` guard is essential: `subtractFloor(current, negative)` would
   otherwise be treated as a *full* discount by the underlying floor helper — the unsafe
   direction. The two callers (`admit.go:207`, `:714`) pass the new `reclaimable`.
   `admitCeiling` and `resolveDelegateRAMScopeCeiling` are current-independent and are
   **not** touched.

### 3.3 Explicit scope boundaries — what deliberately stays RAW

- **The runner flock-fallback (`internal/runner/admission_linux.go`) is LEFT RAW — the
  discount is NOT mirrored there.** (Reverses the v1 plan after the Sol BLOCK, code-
  confirmed.) In the daemon-less fallback a **non-delegate** job runs with **no per-scope
  `memory.max` cap** — `confine_linux.go:588` grants the reserve-sized cap only when
  `admission.lock == nil` (the daemon path); the flock path sets `admission.lock != nil`,
  so its non-delegate scope is uncapped. Its sole over-admission protection is the raw
  `max − cur ≥ reserve` gate (`admission_linux.go:180,194`); discounting it would admit
  more uncapped work, and slice-level containment is a per-process collateral OOM (slice
  `memory.oom.group=0`), **not** the clean per-scope `oom.group` kill the daemon path gets.
  The fallback is a degraded mode (daemon mandatory + usually up), so we keep it
  conservative rather than trade a rare daemon-down stall for higher collateral-OOM risk.
  (Delegate-ram fallback jobs are unaffected — they always get a cap via `scopeCeiling` /
  `delegateRAMScopeFallback`, `confine_linux.go:597-600 — but they are not touched either.)
- **Per-scope `RSSBytes` in #74 adopted reconstruction** (`admit.go:676-683`,
  `confine_manage_linux.go:101` via `readField("memory.current")` — a *different* code path
  from `readSliceMemory`) — the **reserve** side. Over-counting reserves is safe; stays
  raw. The boundary holds **by construction** (separate reader), not just by discipline.
- **Per-signature peak-RSS reserve history** (`resolveDelegateRAMScopeCeiling`, DB-side
  `admitPeakHistory`) — reserve side. **Stays raw.**
- **`admitCeiling` / `resolveDelegateRAMScopeCeiling`** — current-independent
  representability caps. **Untouched.**

The discount applies to exactly one quantity: the slice-level actual-pressure `current`
that feeds the daemon `checkedAvailable`.

## 4. Invariants preserved

- **Reserve invariant** `Σ(reserve) ≤ cap − headroom`: the reserve sum is always an operand
  of `charge = max(...)`, so `charge ≥ outstanding + adopted` regardless of the discount.
  Grant admits iff `reserve ≤ ceiling − charge`, so after grant
  `outstanding_new + adopted ≤ ceiling`. Untouched. (Confirmed by Fable against the actual
  `addClamp(outstanding, adopted)` operands at `admit.go:207,714`.)
- **Fail-closed admission**: an unestablished `memory.current`/`memory.max` read still
  returns `ok=false` and leaves waiters queued. Only the *new* `memory.stat` read degrades
  soft (to no-discount), with a one-time log.
- **OOM safety**: correctness (no *host* OOM) rests on the untouched slice `memory.max` +
  `MemoryHigh` soft-brake + `MemorySwapMax=2G` + the daemon path's per-scope `memory.max`
  sub-cap + `oom.group=1`. The MemAvailable watchdog (`watchdog.go:264`) independently
  protects the desktop and already nets out reclaimable cache; it exempts `.aira-` scopes
  (`watchdog.go:600`), so it is not — and need not be — the backstop for in-slice
  over-admission. **Honesty on containment:** for daemon-path jobs an over-admission is
  contained per-scope by `oom.group`; but the discount includes dirty/writeback pages
  (§3.1), so we do **not** claim that every discounted byte is instantly reclaimable — the
  realistic worst case under heavy dirty load is throttling, and the extreme (wedged
  writeback) is a contained slice-internal OOM, never a host OOM.

## 5. Interaction with Slice-3 (governor RAM ordering)

`governor.go:388` consumes `admitAvailable` to order park/reactivate near a slice's RAM
limit. Post-fix it sees *more* available headroom (cache no longer counted as occupied), so
its ordering actuation reflects reclaimable-aware availability — an intended improvement.
It still never RAM-preempts a *running* worker and still fails open. Verified by a new test
(§6.6) that a cache-dominated slice does **not** cause the governor to preempt a running
worker (proves "never less safe", not just asserts it) plus re-running the existing
governor RAM-ordering suite.

## 6. Tests (every confirmed counterexample becomes a regression test)

1. **Discriminating admit/gate** (unit, synchronous `evaluateAdmitQueue` scaffold):
   two rows, **identical** `current = 90`, `ceiling = 100`, `headroom = 0`, `reserve = 30`;
   `reclaimable = 80` → `effective = 10` → `available = 90 ≥ 30` → GRANT;
   `reclaimable = 0` → `effective = 90` → `available = 10 < 30` → GATE. The reclaimable
   field is the sole discriminator — proves the discount reaches the authoritative grant.
2. **Nonzero-headroom composition**: repeat with `headroom > 0` to prove the arithmetic
   composes with the headroom term (DeepSeek nit).
3. **Parser**: `parseSliceMemoryStat` extracts `inactive_file + active_file`; a
   `memory.stat` with a large `file` **and** `shmem` yields `reclaimable = inactive_file +
   active_file`, **excluding** `shmem` and the `file` total (the shmem OOM-hole regression
   guard); malformed / short line skipped; missing either field → `ok=false`.
4. **Negative / fail semantics**: `reclaimable < 0` (corrupt) → treated as `0` (no
   discount, NOT a full one); `memory.stat` absent while `current`/`max` present →
   `reclaimable = 0`, `ok = true` (raw current) + one-time log fires once; `memory.current`
   absent → `ok = false` (hard fail-closed unchanged).
5. **Clamp**: `reclaimable > current` (non-atomic skew) → `effectiveCurrent = 0`, no
   negative charge.
6. **Governor no-preempt**: a cache-dominated slice (high `current`, high `reclaimable`)
   reports the larger `admitAvailable` and does not trigger RAM-preemption of a running
   worker.
7. **Runner fallback unchanged**: assert the `internal/runner/admission_linux.go` gate
   still charges raw `cur` (the discount was deliberately **not** mirrored) — pins the §3.3
   decision so a later edit can't silently discount the uncapped path.
8. **Reader integration** (real-fs, temp dir like `admit_reconstruction_linux_test.go`):
   fake `memory.current`/`memory.max`/`memory.stat` drive `readSliceMemory` end-to-end.
9. Re-green the full daemon + runner suites under `aira confine`, `-race` clean.

## 7. Expected yield

Eliminates spurious admission stalls when a slice's `memory.current` is inflated by
reclaimable file cache and real RAM is abundant, while leaving anon-dominated pressure to
gate exactly as before. No new host-OOM exposure (backstop unchanged). Behaviour is
byte-identical to today whenever `memory.stat` is unreadable or the slice carries no file
cache, and in the daemon-less fallback path (deliberately unchanged).

## 8. Deferrals (written down, not silent)

- **Clean-only refinement** `(inactive_file+active_file) − file_dirty − file_writeback` —
  removes the dirty-throttling edge (§3.1); not needed for host-OOM safety; deferred to keep
  the change minimal. Revisit if field data shows dirty-driven throttling.
- **Runner flock-fallback discount** — deliberately NOT done (§3.3); a safety-motivated
  non-goal, not a deferral, because the fallback lacks per-scope caps for non-delegate jobs.

## 9. Rollout

Daemon-side change → requires a daemon restart to deploy (the #74 reconstruction makes the
restart itself safe). No client/runner binary change (fallback untouched). Deploy is
**owner-gated**; this plan stops at the build gate.

## Appendix — plan-review folds (v1 → v2)

- **Sol BLOCK** (runner mirror unsafe: fallback non-delegate jobs uncapped) → **folded**:
  the mirror is removed; the fallback stays raw (§3.3), with the `admission.lock`/uncapped
  rationale, code-confirmed at `confine_linux.go:588`.
- **Fable NIT 1** (negative `reclaimable` → `subtractFloor` fabricates a full discount) →
  **folded**: `reclaimable < 0 → 0` guard + `addClamp` sum + preserved raw `current < 0`
  guard (§3.2.1, §3.2.4).
- **Fable NIT 3 / label** (admit.go:412 is `admitConnection`'s read, not the adopt path) →
  **folded** (§3.2.3).
- **Fable churn count** (19 daemon seam closures + `governor_test` scaffold type + 3 direct
  `checkedAvailable` test calls; runner-side churn now avoided) → recorded.
- **DeepSeek nits** (nonzero-headroom test; governor no-preempt test; soft-degrade log;
  state the fallback is not parity-of-safety) → **folded** (§3.2.2 log, §6.2, §6.6, §3.3).
- **Sol / DeepSeek containment overstatement** (dirty/writeback not instantly reclaimable)
  → **folded**: containment language tempered (§3.1, §4); clean-only deferred (§8).
