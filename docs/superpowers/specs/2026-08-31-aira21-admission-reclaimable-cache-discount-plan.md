# AIRA-21 — Discount reclaimable page cache from the confine-admission charge

**Status:** plan (pre-review)
**Ticket:** AIRA-21 (P2 bug)
**Branch:** `aira21-cache-discount`
**Author:** Opus (coordinator), grounded on a 4-reader understand pass

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

Charge the **non-reclaimable working set** instead of raw `memory.current`:

```
reclaimable      = inactive_file + active_file      (from memory.stat)
effectiveCurrent = max(0, memory.current − reclaimable)
charge           = max(outstanding + adopted, effectiveCurrent)
```

Under `memory.max`, the kernel reclaims the file LRU (clean pages dropped, dirty pages
written back and dropped — all without swap, since file pages have a real backing store)
**before** it OOM-kills. So bytes on the file LRU are not a hard OOM floor, and removing
them from the actual-pressure term is sound. The reserve sum stays an operand of the
`max`, so `charge ≥ outstanding + adopted` unconditionally — the discount only relaxes
the actual-pressure floor, never the reserve invariant.

### 3.1 The load-bearing safety constraint: LRU counters only, never `file`

Subtract **`inactive_file + active_file`** (the kernel's file-LRU counters), **never**
the memory.stat `file` total. `file` = file LRU **+ shmem**. Shmem/tmpfs/`MAP_SHARED`
folios are `PageSwapBacked` → they sit on the *anon* LRU and are reclaimable only to swap
(here a shared 2 GiB pool), i.e. anon-like. Subtracting `file` would discount shmem and
open a genuine OOM hole. Using the two LRU counters keeps shmem, `unevictable`, kernel
slab, pagetables, and sock as non-reclaimable pressure — all correct.

Dirty/writeback file pages *are* inside `inactive_file+active_file` and are
reclaimable-but-not-instant. This is still OOM-safe (`memory.max` forces writeback and
throttles dirtiers before OOM). Subtracting a clean-only figure
(`(inactive_file+active_file) − file_dirty − file_writeback`) is an **optional**
belt-and-braces refinement — **deferred**; not required for correctness.

### 3.2 Mechanics

1. **New parser** `parseSliceMemoryStat(data []byte) (inactiveFile, activeFile int64, ok bool)`
   in `admit.go`, modelled on `parseMemAvailable` (`watchdog.go:150`): split on `\n`,
   `strings.Fields` per line, match `field[0]` against `inactive_file` / `active_file`,
   `strconv.ParseInt(field[1])`. `ok` only if **both** fields were found and parsed
   ≥ 0.

2. **Extend `readSliceMemory`** (`admit.go:990`) to also read `memory.stat` from the same
   dir and return a fifth value `reclaimable int64`. New signature:
   `readSliceMemory(path) (current, maximum, reclaimable int64, ok bool, reason string)`.
   - `memory.current` / `memory.max` unreadable/unbounded/parse-error → `ok=false`
     (HARD fail-closed, exactly as today — waiters stay queued).
   - `memory.stat` missing / unparseable / either field absent → `reclaimable = 0`,
     `ok=true` (SOFT — degrade to today's raw-current behaviour; **never fabricate a
     discount**).

3. **Widen the seam.** `admitReadMemory` (`server.go:95`) becomes
   `func(string) (current, maximum, reclaimable int64, ok bool, reason string)`. All four
   consumers (`admitAvailable` :197, adopt path :412, `evaluateAdmitQueue` :692,
   confine-list reserve summary `confine_manage.go:123`) select it via the existing
   `readMemory := s.admitReadMemory; if nil { readSliceMemory }` idiom — no call-site
   restructuring, only the destructure arity.

4. **Discount in `checkedAvailable`.** New signature
   `checkedAvailable(current, maximum, reclaimable, outstanding, headroom int64) int64`:
   ```go
   effectiveCurrent := subtractFloor(current, reclaimable) // clamp ≥ 0 (non-atomic reads)
   charge := outstanding
   if effectiveCurrent > charge { charge = effectiveCurrent }
   ```
   Reuse `subtractFloor` (`admit.go:116`). The two callers (`admit.go:207`, `:714`) pass
   the new `reclaimable`. `admitCeiling` and `resolveDelegateRAMScopeCeiling` are
   current-independent and are **not** touched.

5. **Mirror into the runner flock-fallback.** `internal/runner/admission_linux.go` has a
   second, independent `readSliceMemory` (`:717`) feeding the daemon-less admission gate
   `max − cur >= reserve` (`:180`, `:194`), where `cur` is the **sole** gate (no ledger).
   Apply the identical discount there for parity, so the daemon-down path is consistent.
   This is a *larger* relaxation (no reserve backstop) but stays bounded by slice
   `memory.max` + per-scope `oom.group`. (Alternative considered and rejected: leave it as
   an accepted asymmetry — rejected because a silent behavioural fork between the daemon and
   fallback paths is exactly the kind of honesty gap AIRA avoids.)

### 3.3 Explicit scope boundaries — what deliberately stays RAW

- **Per-scope `RSSBytes` in #74 adopted reconstruction** (`admit.go:679`,
  `confine_manage_linux.go:101`) — this is the **reserve** side (a post-restart forecast of
  a scope's held reserve). Over-counting reserves is the safe direction; discounting here
  would weaken the reserve floor. **Stays raw.**
- **Per-signature peak-RSS reserve history** (`resolveDelegateRAMScopeCeiling`) — reserve
  side. **Stays raw.**
- **`admitCeiling` / `resolveDelegateRAMScopeCeiling`** — current-independent
  representability caps. **Untouched.**

The discount applies to exactly one quantity: the slice-level actual-pressure `current`
that feeds `checkedAvailable`.

## 4. Invariants preserved

- **Reserve invariant** `Σ(reserve) ≤ cap − headroom`: the reserve sum is always an operand
  of `charge = max(...)`, so `charge ≥ outstanding + adopted` regardless of the discount.
  Grant admits iff `reserve ≤ ceiling − charge`, so after grant
  `outstanding_new + adopted ≤ ceiling`. Untouched.
- **Fail-closed admission**: an unestablished `memory.current`/`memory.max` read still
  returns `ok=false` and leaves waiters queued. Only the *new* `memory.stat` read degrades
  soft (to no-discount).
- **OOM safety**: correctness (no host OOM) rests on the untouched slice `memory.max` +
  `MemoryHigh` soft-brake + per-scope `memory.max` sub-cap + `oom.group=1`. Even if the
  discount over-admits, the worst case is a governed, per-scope-contained slice-internal
  OOM (kernel reclaims file LRU, then `oom.group`-kills the offending scope) — never a
  machine-wide OOM. The MemAvailable watchdog protects the desktop from *uncapped* runaways
  and **exempts** `.aira-` scopes, so it is not — and need not be — the backstop for
  in-slice over-admission; the kernel cap machinery is.

## 5. Interaction with Slice-3 (governor RAM ordering)

`governor.go:388` consumes `admitAvailable` to order park/reactivate near a slice's RAM
limit. Post-fix it sees *more* available headroom (cache no longer counted as occupied), so
its ordering actuation shifts to reflect real reclaimable-aware availability — an intended,
benign improvement (the governor becomes more accurate, never less safe: it still never
RAM-preempts a running worker, still fails open). Covered by re-running the existing
governor RAM-ordering tests plus one that asserts a cache-dominated slice reports the
larger availability.

## 6. Tests (every confirmed counterexample becomes a regression test)

1. **Discriminating admit/gate** (unit, synchronous `evaluateAdmitQueue` scaffold):
   two rows with **identical** `current = 90`, `ceiling = 100`, `headroom = 0`,
   `reserve = 30`; `reclaimable = 80` → `effective = 10` → `available = 90 ≥ 30` → GRANT;
   `reclaimable = 0` → `effective = 90` → `available = 10 < 30` → GATE. The reclaimable
   field is the sole discriminator — proves the discount reaches the authoritative grant.
2. **Parser** (unit): `parseSliceMemoryStat` extracts `inactive_file + active_file`;
   a `memory.stat` containing a large `file` and a large `shmem` yields
   `reclaimable = inactive_file + active_file` and **excludes** `shmem` and the `file`
   total (the shmem OOM-hole regression guard); malformed / missing field → `ok=false`.
3. **Fail semantics** (unit/real-fs, temp-dir like `admit_reconstruction_linux_test.go`):
   `memory.stat` absent while `current`/`max` present → `reclaimable = 0`, `ok = true`
   (raw current, today's behaviour); `memory.current` absent → `ok = false` (hard
   fail-closed unchanged).
4. **Clamp**: `reclaimable > current` (non-atomic skew) → `effectiveCurrent = 0`, no
   negative charge.
5. **Reader integration** (real-fs): a temp cgroup dir with fake
   `memory.current`/`memory.max`/`memory.stat` drives `readSliceMemory` end-to-end.
6. **Runner-fallback mirror**: the same discount is exercised on the
   `internal/runner/admission_linux.go` copy.
7. Re-green the full daemon + runner suites under `aira confine`, `-race` clean.

## 7. Expected yield

Eliminates spurious admission stalls when a slice's `memory.current` is inflated by
reclaimable file cache and real RAM is abundant, while leaving anon-dominated pressure to
gate exactly as before. No new host-OOM exposure (backstop unchanged). Behaviour is
byte-identical to today whenever `memory.stat` is unreadable or the slice carries no file
cache.

## 8. Deferrals (written down, not silent)

- Clean-only refinement `(inactive_file+active_file) − file_dirty − file_writeback` —
  optional stall-avoidance polish; not needed for OOM-safety. Deferred.
- Per-scope reclaimable discount for adopted reconstruction — intentionally NOT done
  (reserve side must stay conservative). Not a deferral so much as an explicit non-goal.

## 9. Rollout

Daemon-side change → requires a daemon restart to deploy (the #74 reconstruction makes the
restart itself safe). Client/runner-side mirror is a binary swap. Deploy is **owner-gated**;
this plan stops at the build gate.
