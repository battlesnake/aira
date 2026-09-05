---
{"schema":1,"id":"AIRA-103","project":"aira","title":"Dynamic slice ceiling: shrink aira.slice's memory.max under real system-wide RAM pressure so existing admission throttles","status":"in-review","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","memory-safety","scheduler"],"hold":false,"relations":[]}
---
Direct owner request (2026-09-05): monitor free SYSTEM RAM (outside the slice — the desktop, other system load, and per AIRA-102, currently-uncontained Docker containers) and dynamically shrink `aira.slice`'s own memory ceiling when system RAM is getting tight, so admission gets blocked by the slice's EXISTING capacity check even while the slice's own static accounting still shows "room" — rather than adding a second, parallel admission-blocking mechanism.

## This is a concrete, scoped answer to AIRA-91 Part B's owner decision

AIRA-91 Part B recorded: the oomd backstop stays as configured, never weakened — "the fix belongs entirely on AIRA's own side: admission/containment must be accurate enough that a legitimately-admitted job does not sit in sustained `memory.high` reclaim long enough to generate the PSI that trips the backstop." A dynamic ceiling that shrinks under real external memory pressure is exactly that: it makes AIRA's own admission responsive to machine-wide health, reducing how often anything gets pushed into the sustained-reclaim state that generates the PSI the backstop reacts to. Cross-link both directions.

## The plumbing already supports this with minimal new surface — verified by reading source, not assumed

`internal/daemon/admit.go`'s admission-ceiling calculation (`ceiling := subtractFloor(maximum, headroom)`, `admitCeiling`) derives `maximum` from `readMemory(slicePath)` — a **live read of the slice's own `memory.max` cgroup file at admission-check time**, not a cached or static constant. This means: if a new daemon subsystem periodically writes an adjusted value directly to `aira.slice`'s live `memory.max`, the *existing* admission-vs-capacity check picks it up automatically on its very next read — no change needed to the admission-decision logic itself. The new work is narrowly: (1) a periodic reader of system-wide memory pressure, (2) a bounded, hysteresis-controlled writer of the slice's own `memory.max`.

**Reuse, do not reimplement, the system-memory read.** `internal/daemon/watchdog.go` already has `readMemAvailable()`/`parseMemAvailable()` — a tested `(int64, bool, string)` (value, established, reason) reader of `/proc/meminfo`'s `MemAvailable`, plus the project's own established trip/recover hysteresis pattern (currently 8GiB trip / 16GiB recover for the watchdog's own kill decision). This new mechanism should almost certainly be a SIBLING consumer of the same primitive, not a second parallel memory-pressure reader — but likely needs its OWN, more conservative thresholds: it should trip EARLIER than the watchdog's last-resort kill threshold, so admission throttling naturally prevents things from ever reaching the point the watchdog exists to handle.

## The load-bearing safety bound — do not shrink below current real usage

`memory.max` is a hard kernel cap: shrinking it below the slice's CURRENT actual usage (`memory.current`) would apply immediate OOM-reclaim pressure inside the slice against already-admitted, already-running jobs — the exact self-inflicted failure this whole mechanism exists to prevent, not cause. The dynamic ceiling must be clamped: **never below current real slice `memory.current`** (with a safety margin), and probably also **never below the sum of already-GRANTED admission-ledger reservations** (so the slice's own bookkeeping stays internally consistent — a ceiling lower than what's already been promised is a lie the ledger would then have to paper over). Shrinking only ever affects room for NEW admissions; already-running jobs are never preempted or newly pressured by this mechanism — consistent with the "drain, never preempt" principle already established for AIRA-101.

## Bounds and mechanism, not decided here — the plan should resolve

- **Upper bound**: never exceed the statically-configured ceiling (`aira install --memory-max`, read via the same `parseInstalledMemoryMax` path referenced elsewhere in this codebase) — this mechanism only ever shrinks relative to that baseline, never grows beyond it.
- **Lower bound**: current `memory.current` (with margin) and/or the granted-reservation sum, per above — the plan should pick the tighter, correct bound and justify it.
- **Shape of the reduction**: a simple binary trip/recover step (matching the watchdog's own style — trip: cap near current usage plus a fixed small headroom, effectively pausing new admission until pressure eases; recover: restore to the static ceiling) is the simplest, most predictable v1 and should be the default recommendation unless the plan finds a continuous/proportional approach isn't meaningfully more complex or risky.
- **Thresholds**: what specifically trips/recovers this mechanism, and whether they should be configurable (matching how the watchdog's own thresholds are likely configured — check) — needs its own justified numbers, not copied blindly from the watchdog's kill-decision thresholds, since this is a preventive throttle, not a last-resort kill.
- **Write target**: `memory.max` directly (the value `admit.go` actually reads), via direct cgroupfs write, not through a systemd unit-file rewrite (too slow/heavy for something that may need to adjust on a short interval) — confirm this is right and that nothing else concurrently writes to the same file in a way that would race or get clobbered.
- **Visibility**: `aira confine --list`'s existing "slice reserve: X granted / Y ceiling" line (AIRA-73's own precedent) should honestly reflect the dynamic ceiling when shrunk, so an operator waiting on admission can tell WHY (external system pressure, not just "the slice happens to be full of AIRA's own jobs") — same honesty-first discipline as everything else built tonight.

## Relation to other in-flight/recorded work

- **AIRA-91 Part B**: this is a concrete implementation of that owner decision's stated direction — cross-link explicitly, and once this lands, revisit whether Part B can be considered substantially addressed or what (if anything) remains.
- **AIRA-29** (dynamic reserve / track-actual charging): complementary, not overlapping. AIRA-29 is about charging admission by REAL usage of jobs INSIDE the slice (a utilization problem). This ticket is about shrinking the slice's OWN ceiling in response to pressure OUTSIDE the slice (a machine-wide safety problem). Both make admission smarter; neither subsumes the other.
- **AIRA-65 watchdog**: this ticket's system-memory signal must reuse the watchdog's existing `readMemAvailable`/`parseMemAvailable`, not duplicate it.
- **AIRA-102** (docker escape): one of the concrete external-pressure sources this mechanism would actually help against — a runaway or merely numerous uncontained container(s) tightening system RAM would now correctly cause AIRA's own admission to throttle back, even though AIRA's ledger has no direct visibility into what's causing the pressure. Worth noting as motivation, not a dependency — this ticket doesn't need AIRA-102 to land first.

## Two other in-flight tickets touch the daemon/admission area concurrently

AIRA-101 (exclusive slice access) and AIRA-102 (podman/docker integration) are being built concurrently in separate worktrees right now. This ticket's actual code surface (a new small daemon subsystem plus a cgroupfs write) should have minimal overlap with either, but check `git log origin/master` for what's landed since starting, and rebase cleanly rather than assuming the starting point is still current.

## Resolution (2026-09-05)

Built and tested. **Read the first section below before anything else: the
actuator this ticket prescribed was NOT built, deliberately, and the reason is a
correction to two of this ticket's own premises.**

### DEVIATION FROM THIS TICKET'S PRESCRIBED ACTUATOR — needs owner acceptance before `enforce`

This ticket asked for a periodic **cgroupfs write to `aira.slice`'s own
`memory.max`**, on the reasoning that admission reads that file live, so no
admission logic would need changing. Two independent adversarial plan reviews
(Fable: GATE-FAIL, 3×P0; Sol/GPT-5.6: REJECT, 3×P0) rejected that actuator, and
re-reading the source confirmed both of the ticket's load-bearing premises are
false as written:

1. **"No changes needed to `admit.go`" is FALSE.** `maximum` has FOUR consumers
   in admission and only ONE is a capacity question. The others are the
   **terminal** `E_ADMIT_TOO_LARGE` (`admit.go:742`/`:863`, which the runner does
   not retry), `resolveAdmitReserve`'s OOM-escalation clamp (`:548`), and
   `resolveDelegateRAMScopeCeiling` (`:581`) — the last two size a job's **own
   hard scope `memory.max`**, held for its whole life. A cgroupfs write cannot
   distinguish them: it moves one number all four read. Worked consequence: a
   delegate-ram pytest suite admitted during a throttle gets a ~1 GiB outer scope
   cap instead of the 48 GiB default and OOM-groups itself; and the default 4 GiB
   no-history reserve is met with a terminal refusal, turning "wait for pressure
   to ease" into a hard merge-gate failure. Both are self-inflicted failures on
   legitimately admitted work.
2. **Raw `MemAvailable` is not a signal about memory *outside* the slice.**
   MemTotal 78.5 GiB against a 64 GiB configured slice: `MemAvailable` falls below
   any fixed threshold purely because AIRA's own jobs are using the budget the
   owner configured for them. A trip on raw `MemAvailable` throttles AIRA in
   response to AIRA.
3. Additionally, and measured on this box: a cap written near `memory.current`
   puts the slice into continuous `memory.max`-triggered reclaim as page cache
   refills the gap — manufacturing exactly the sustained-reclaim PSI that trips
   the oomd backstop, i.e. the AIRA-91 Part B failure class this ticket exists to
   reduce. And `memory_max_write` sets the counter FIRST and then runs its
   reclaim-then-OOM loop, which bails on `signal_pending`; a Go process is
   permanently signal-pending (runtime SIGURG), so the write returns with the cap
   applied, no reclaim done and no OOM raised — **632 MB of anon observed sitting
   under a 32 MiB cap**, with the kill merely deferred to the job's next charge.
   That is strictly worse than an immediate kill and is pinned by a committed
   test (`TestSliceCeilingRealCgroupHarnessDetectsALimitWrite`).

**What was built instead:** the throttled ceiling is published **in-process** and
applied only where capacity is computed. `aira.slice`'s `memory.max` is never
written. This satisfies the ticket's actual stated constraint — "no second,
parallel admission-blocking mechanism" — because it adds no gate, queue or
refusal path: it supplies one more term to the single existing `checkedAvailable`
check, at the sites that already read the slice's memory. **What it gives up is
kernel enforcement of the reduced ceiling.** That residual is covered exactly as
it is today (static cap + systemd-oomd + the AIRA watchdog), none of which this
weakens. Fable's ruling: recording the deviation here suffices for the build, but
it must be put to the owner before any live `enforce` flip. The kernel-write
variant remains available as a follow-up if enforcement teeth are wanted; it
would first have to solve (1)–(3) above.

### The signal

```
current    = min(memory.current read before, and after, the MemAvailable read)
sliceAnon  = max(0, current - inactive_file - active_file - slab_reclaimable)
affordable = MemAvailable + sliceAnon      # what MemAvailable would read if the slice were emptied
desired    = affordable - min(MemTotal/4, 16 GiB)
ceiling    = clamp(desired, 0, live memory.max)
```

The `+ sliceAnon` term is the whole point: when the slice's own jobs grow,
`MemAvailable` falls and `sliceAnon` rises by the same amount, so `affordable`
does not move. The ceiling moves only for memory consumed **outside** the slice —
the desktop, other sessions, and (per AIRA-102) uncontained containers.

**Thresholds/shape, and why.** There is no trip/recover pair and no state
machine: the ceiling is a derived quantity recomputed each tick. Damping is
`max()` over the last 3 established samples, quantised **down** to 256 MiB, which
is the hysteresis in one expression — lowering needs the whole window to agree
(~6 s, the watchdog's own debounce), raising needs one sample. Restricting must be
sustained; relieving must be prompt. A partial window publishes nothing (else
`max` over one sample would lower the ceiling at startup). A raise must clear the
published value by a whole quantum (anti-flap). The system reserve is
`min(MemTotal/4, 16 GiB)` — the owner's **existing** `aira install` headroom
policy (`internal/install/install.go:746`), not a number invented here; on a large
box it equals `watchdogRecoverMemAvailable`, which is the sanity check rather than
the derivation (the throttle's target state must be one in which the watchdog is
quiescent), and the `MemTotal/4` term stops `enforce` pinning the ceiling near
zero on a ≤40 GiB machine. Thresholds are constants and mode/interval are
env-configurable — **exactly** how the watchdog handles its own
(`AIRA_DAEMON_SLICE_CEILING_MODE` off|observe|enforce, default **off**;
`AIRA_DAEMON_SLICE_CEILING_INTERVAL` in [1s,30s), default 2s).

### Honest consequence the owner must see before `enforce`

On this machine the configured ceiling is **already unaffordable**: the footprint
this mechanism cannot give the slice (`MemTotal − affordable`: non-slice anon plus
kernel/slab/page tables) is roughly **25 GiB** of 78.5 GiB, so with a 16 GiB
reserve the affordable ceiling lands near **38 GiB**, well under the configured
64 GiB. In `enforce` mode the subsystem will therefore publish a reduced ceiling
essentially always. That is not a malfunction — it is this ticket's own finding
made measurable, and it is how admission could report "room" while the box
starved — but it changes the subsystem's character from an exceptional pressure
response to a permanently-engaged capacity governor, which is a capacity policy
the owner set at 64 GiB. Hence: default `off`, rollout `observe` → `enforce`, and
the coordinating session doing the live deploy.

### Where the throttle is applied — and the four places it must never reach

Applied: `evaluateAdmitQueue`'s `checkedAvailable` (`admit.go:1065` — the throttle
itself), `admitAvailable` (`:495` — the governor's capacity advisory), and
`confineManagement`'s `CeilingBytes` (what a new job actually faces).
**Not** applied at `admitConnection`'s own ceiling (`:736` → `E_ADMIT_TOO_LARGE`,
the OOM-escalation clamp, and the delegate-ram scope ceiling), nor at
`enqueueAdmitInternal` (`:863`, the same terminal refusal), nor at
`evaluateWorkerAdmit` (`worker_admit.go:428`, keyed by an aitest job's OUTER
SCOPE, whose suite already holds its own slice reservation). So a job too large
for the throttled ceiling **waits**, exactly as under ordinary contention.
`admitConnection` is not edited at all.

### Visibility

`aira confine --list` gains a `slice ceiling:` line (effective vs configured,
system MemAvailable, observe-vs-enforce, and `unevaluated` printing its *reason*
rather than a number), plus a **drain-state** line when granted exceeds the
effective ceiling — jobs admitted before the ceiling fell are never preempted, and
that must not read as the `LEDGER INCONSISTENCY` the residual line reports. The
blocked launcher's own waiting line also names the reduced ceiling, rendered from
the `SliceReserve` its existing AIRA-24 probe already returns — no extra round
trip, and **no wire `Basis` string changed** (`admission_linux.go:513` validates
`reject:saturated` exactly; a mismatch would fall through to the flock fallback
and launch uncounted).

### Nothing else writes the slice's `memory.max` (the ticket's question 5)

Verified: the only writer in AIRA is `writeScopeMemoryValue`
(`internal/runner/confine_linux.go:1215`), always against a *scope*;
`internal/install` only reads. The only other writer is systemd itself, and
measured on a throwaway transient unit: a direct cgroupfs write is **not**
reflected in `systemctl show -p MemoryMax`, and **is reverted by
`systemctl --user daemon-reload`** — so a write-based design would have been
silently undone by any unrelated unit reload on this machine. Moot now: this
subsystem adds no writer either.

### How the safety bound is proven

The bound is stronger than the ticket's "never shrink below current usage": **no
kernel-enforced limit is ever modified**, so this mechanism cannot pressure a
running job at all. Proven empirically, not by code reading, in
`internal/daemon/sliceceiling_real_cgroup_linux_test.go` (all fixtures on an
ISOLATED `cgrouptest.IsolatedScopeParent` cgroup — `aira.slice` is never touched):

- `TestSliceCeilingRealCgroupThrottlesAdmissionWithoutTouchingTheJob` — a real
  600 MiB-resident process in a real 2 GiB cgroup with `memory.swap.max=0`, driven
  to the hardest throttle the subsystem can ask for (published ceiling 0). Asserts
  `memory.max` **byte-identical**, `memory.events` `oom_kill` unchanged, the job
  alive, **and** that admission genuinely closed — the last clause is what stops
  the first three passing vacuously.
- `TestSliceCeilingRealCgroupHarnessDetectsALimitWrite` — the negative control.
  The test itself writes an unclamped 32 MiB cap and asserts the job **is**
  OOM-killed, establishing that the fixture can observe the failure mode the
  first test excludes. Without it, "unchanged and alive" could be true of a
  fixture that could never fail.
- `TestSliceCeilingRealCgroupSignalTracksRealAccounting` — the signal against the
  kernel's own accounting, in two explicitly separated halves: a **non-circular**
  half asserting `sliceCeilingAnon` computed from real `memory.current`/
  `memory.stat` rises with real anonymous allocation and stays flat across real
  page-cache growth; and an end-to-end half covering the wiring, damping and
  clamping around it.

### Non-porosity: every load-bearing test was mutation-verified RED

1. `affordable = raw MemAvailable` (the ticket's own proposal) → growth-invariance FAILS.
2. reader drops `slab_reclaimable` → reader test FAILS.
3. torn-read guard removed → shrink-direction case FAILS.
4. throttle plumbed into `admitConnection` → capacity-only test FAILS.
5. `sliceCeilingAnon` ignores reclaimable → real-cgroup signal test FAILS.
6. hold path fabricates zeros / mis-derives state → both hold tests FAIL.

### Two-loop record

Plan v1 → **GATE-FAIL** (Fable, 3×P0) and **REJECT** (Sol/GPT-5.6, 3×P0) → v2
restructured (actuator + signal) → v2 **PASS-WITH-CHANGES** (Fable, no P0, 5×P1)
and **REJECT** (Sol, on signal accuracy) → v3 took every finding → build →
build-review **APPROVE-WITH-FIXES** (Fable) and **BLOCK** (Sol), both taken in
full. Build-review findings fixed: resolve-failure now routed through the TTL
hold (it was dropping an enforced throttle the instant the resolver blinked, and
leaking a stale window into recovery); a stale *unthrottled* snapshot no longer
authorises a newly-RAISED live `memory.max`; expiry now wakes the kick-driven
governor (expiry is an effective raise); `off` no longer validates the interval,
so the "off is exactly today's behaviour" claim is true; observe mode reports the
counterfactual rather than the untouched static capacity; a held ceiling is marked
as such on every surface; the confine-list reply comes from ONE ceiling snapshot;
an unrecognised state renders as unknown; and a dead anti-flap branch (plus the
test that overclaimed it) was deleted. Rebased onto `ea82cb8` (AIRA-101), whose
exclusive-gate block conflicted in `admit.go` and
`confine_queue_position_linux.go`; both resolved keeping both features, and the
launcher's waiting line now carries the exclusivity clause and the pressure
clause side by side.

Thirteen mutations were run against the load-bearing tests and **all thirteen
went RED**, including two that initially did NOT (the reader-level slab fold and
the OOM-escalation clamp, whose first tests bypassed the wiring they claimed to
pin and were rewritten to drive the real paths).

### Exit codes (recorded exactly, on the rebased tree, all under `aira confine --`)

- `go build ./...` — **0**
- `go vet ./...` — **0**
- `go test ./...` (full suite, `-count=1`) — **0** (all 13 packages `ok`; the
  three real-cgroup tests RAN, they did not skip)

### Deferrals

Kernel enforcement of the reduced ceiling (above). `memory.high` untouched —
lowering it generates the very PSI AIRA-91 Part B says to stop producing.
AIRA-16 half (2), the slice-*internal* pressure trigger, stays open on its own
terms: that is a kill decision under internal pressure, this is a preventive
admission throttle under external pressure. `sliceAnon` residuals recorded with
measured magnitudes and directions in the design's §9 — notably **swap**, which
is NOT small (SwapTotal 20 GiB / SwapFree 14.4 GiB ⇒ ~6.5 GiB swapped, essentially
all non-slice): the signal measures memory others *occupy*, not memory they
*need*, so under thrash it is permissive. Same limitation the watchdog has;
recorded, not fixed.

Design: `docs/superpowers/specs/2026-09-05-aira103-dynamic-slice-ceiling-design.md`
