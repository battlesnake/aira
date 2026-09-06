# AIRA-103 — Dynamic slice ceiling under system-wide RAM pressure

Status: plan **v3** — build-approved. v1 GATE-FAILed (Fable, 3×P0) and REJECTed
(Sol/GPT-5.6, 3×P0) → v2 restructured (§0). v2 re-gated **PASS-WITH-CHANGES**
(Fable: no P0, 5×P1) and REJECTed by Sol on signal accuracy; **§0.3 lists every
v3 change and which reviewer required it.** Fable's explicit ruling: recording the
actuator deviation in the ticket Resolution SUFFICES FOR THE BUILD, but the
deviation and §3.1's consequence must be put to the owner before any live
`enforce` flip.
Ticket: `.aira/tickets/AIRA-103.md`

> **SUPERSEDED IN PART by AIRA-106**
> (`docs/superpowers/specs/2026-09-06-aira106-two-parameter-slice-ceiling-design.md`).
> The ACTUATOR and the SIGNAL below are current and unchanged. What is superseded
> is §3's reserve derivation: `sliceCeilingSystemReserve = min(MemTotal/4, 16 GiB)`
> no longer exists, and with it the invariant that the throttle's target state
> coincides with `watchdogRecoverMemAvailable`. The published ceiling is now
> `min(MemTotal − reserveMax, affordable − freeMin)` with the owner's own
> 16 GiB / 8 GiB defaults, both environment-configurable; `freeMin` sits on the
> watchdog's KILL threshold rather than its recover threshold, which AIRA-106 §2.5
> states, bounds and puts to the owner. §3.1's ≈38 GiB figure is likewise
> superseded (≈47 GiB measured under the new formula). Everything else here —
> Findings A–C, the no-write decision, the torn-read guard, `sliceAnon`, the
> damping, the TTL hold, the `unevaluated` contract — stands as written.
Related: AIRA-91 Part B (owner decision this implements), AIRA-16 half (2)
(the *slice-internal* pressure trigger, still open and deliberately untouched),
AIRA-29 (dynamic reserve — complementary), AIRA-102 (one motivating external
pressure source), AIRA-73 (the `confine --list` reserve summary this extends).

---

## 0. What v1 got wrong, and the two decisions v2 takes as a result

v1 proposed exactly what the ticket prescribes: a daemon subsystem that writes a
reduced value into `aira.slice`'s live `memory.max` cgroup file. Two independent
adversarial reviews (Fable: GATE-FAIL, 3×P0; Sol/GPT-5.6: REJECT, 3×P0) converged
on the same conclusion from different directions. The findings are not tuning
issues; two of them are design errors in the ticket's own premises, which the
ticket explicitly asked to be re-verified against source rather than assumed.

**Finding A — the ticket's "no changes needed to `admit.go`" premise is FALSE.**
`maximum` (the value read from `memory.max`) is consumed by admission for four
structurally different purposes, and only ONE of them is a capacity question:

| consumer | line | what it decides | may it see a throttled value? |
|---|---|---|---|
| `checkedAvailable` (via `evaluateAdmitQueue`, `admitAvailable`) | `admit.go:1065`, `:495` | how much room is left **right now** | **YES — this is the throttle** |
| `E_ADMIT_TOO_LARGE` rejection | `admit.go:742`, `:863` | "this job can NEVER fit" — **terminal**, the runner does not retry (`internal/runner/admission_linux.go`) | **NO** |
| `resolveAdmitReserve`'s OOM-escalation clamp | `admit.go:548` | the reserve, which becomes a non-delegate job's **hard scope `memory.max`** | **NO** |
| `resolveDelegateRAMScopeCeiling` | `admit.go:581-613` | the delegate-ram job's **hard scope `memory.max`**, held for the job's whole life | **NO** |

A cgroupfs write cannot make that distinction: it moves one number that all four
read. Fable's worked example — slice at current 3G / granted 4G — gives a
delegate-ram pytest suite a ~1G outer scope cap instead of the 48G default, so it
OOM-groups itself; and the default 4 GiB no-history reserve is met with a
*terminal* `E_ADMIT_TOO_LARGE`, turning "wait for pressure to ease" into a hard
merge-gate failure. Both are self-inflicted failures on legitimately admitted
work — the exact class the ticket forbids.

**Finding B — raw `MemAvailable` is not a signal about memory *outside* the
slice.** The ticket asks to "monitor free SYSTEM RAM (outside the slice)".
Measured on this box: `MemTotal` 78.5 GiB, `aira.slice` `MemoryMax` 64 GiB. With
~16.5 GiB of non-slice footprint, `MemAvailable` falls below any fixed trip
threshold *purely because AIRA's own jobs are using the budget the owner
configured for them.* A trip/recover pair on raw `MemAvailable` therefore
throttles AIRA in response to AIRA, and — in v1's write design — would have
ratcheted the kernel cap down onto its own legitimately-running jobs. §3 replaces
it with a signal that is invariant to the slice's own growth.

**Finding C — writing a cap near `memory.current` manufactures the very failure
mode AIRA-91 Part B exists to stop.** With `memory.max = current + headroom`, any
I/O-heavy job fills that headroom with page cache and then sits in continuous
`memory.max`-triggered direct reclaim, generating in-slice PSI. `systemd-oomd` is
live on this box (`user@1000.service ManagedOOMMemoryPressure=kill`, limit 40%,
`aira.slice=auto`). v1 would have created AIRA-91 Part B's failure class as a
side effect of trying to prevent it.

### Decision 1 — the actuator changes from a cgroupfs write to an in-daemon capacity override

The throttled ceiling is **published in-process by the subsystem and applied only
where capacity is computed**; `aira.slice`'s kernel `memory.max` is **never
written**. This is not "a second parallel admission-blocking mechanism" — the
ticket's stated constraint — because it adds no gate, no queue and no refusal
path: it supplies one additional term to the *single existing*
`checkedAvailable` capacity check, at the same three sites that already read the
slice's memory. The admission decision logic, the queue, the fairness freeze and
every refusal code are untouched.

What this buys, structurally rather than by argument:

- **No running job can be pressured by this mechanism, by construction.** No
  kernel-enforced limit moves. Findings A and C, and every self-OOM race both
  reviewers found (TOCTOU between reading `memory.current` and writing
  `memory.max`; a grant landing in that window; a stale or bidirectionally-moving
  baseline; queue-absent `granted` reading as zero after a daemon restart) become
  unreachable rather than mitigated.
- **The static ceiling and the current capacity become separable**, which is what
  Finding A actually requires. Terminality and scope-cap sizing keep reading the
  real, unmodified `memory.max`; only capacity sees the throttle. A job too large
  for the throttled ceiling **waits**, exactly as it does under ordinary
  contention, instead of being terminally rejected.
- **The throttle can drive `available` to exactly zero.** v1 had to accept
  residual admission room equal to the slice's page cache (~8.2 GiB live), because
  the safety clamp had to use raw `memory.current` while `checkedAvailable`
  charges `current − reclaimable`. With no kernel write there is no clamp and no
  residual: the override applies directly to the same formula.
- **No baseline discovery, no `systemctl` exec, no restore-on-restart, no
  restore-on-shutdown, no systemd `daemon-reload` clobber race, and no risk of
  ever writing `max` into the slice** (which AIRA-16 records as making every
  running confine job a valid watchdog victim). Roughly a quarter of v1's code
  and none of its irreversible state.

What it gives up, stated plainly: the reduced ceiling is **not kernel-enforced**.
If AIRA's own accounting were wrong, the kernel would still allow the slice to
reach its full configured 64 GiB. That residual is covered exactly as it is
today — by the static 64 GiB cap, `systemd-oomd`, and the AIRA watchdog — none of
which this change weakens. **This is a deliberate deviation from the actuator the
ticket prescribes and is recorded as such in the ticket's Resolution, with the
kernel-write variant left available as a follow-up should the owner want
enforcement teeth; the hazards it would have to solve are Findings A–C above.**

### Decision 2 — the signal becomes "what the machine can afford the slice", not raw `MemAvailable`

See §3. One extra term makes the signal invariant to the slice's own growth,
which is what "outside the slice" actually means.

### 0.3 v2 → v3 changes, and which reviewer required each

| # | change | required by |
|---|---|---|
| 1 | Torn-read guard: the sample reads `memory.current` **twice**, around the `MemAvailable` read, and uses `min` of the two. Provably exact-or-restrictive in **both** tear directions (§3.2). | Sol r2 P0 ("`/proc/meminfo` and three cgroup files are not one snapshot … can add the same bytes twice") |
| 2 | `sliceAnon` also subtracts `slab_reclaimable`, closing the permissive double-count of slice-owned reclaimable slab (0.36–0.44 GiB live). `parseSliceMemoryStat` returns it as a **third** value; `readSliceMemory` ignores it, so admission's AIRA-21 discount is byte-identical. | Sol r2 P0; Fable r2 "verified" note |
| 3 | The system reserve becomes `min(MemTotal/4, 16 GiB)` — the owner's **existing** headroom policy (`internal/install/install.go:746`), not a value copied from the watchdog. One policy, two uses; it also stops `enforce` pinning the ceiling near zero on a ≤40 GiB box. | Sol r2 P0 ("policy leap"); Fable r2 P2(b) |
| 4 | Partial-window rule: publish **only** when the window holds 3 established samples; otherwise `unevaluated`. TTL = `max(30s, 3×interval)`. | Fable r2 P1 ("`max` over 1 sample lowers immediately at startup") |
| 5 | §3.1's arithmetic corrected to be consistent with §3's own formula. | Fable r2 P1 |
| 6 | Test 1 gains the two cases that make it non-porous against `MemAvailable + raw current`. | Fable r2 P1 |
| 7 | The blocked client's own waiting line surfaces the throttle state through the **existing** AIRA-24 confine-list probe. **No wire `Basis` string changes** — the runner validates `reject:saturated` exactly (`admission_linux.go:513`) and a mismatch would fall through to the flock fallback and launch uncounted. | Fable r2 P1 |
| 8 | Swap recorded with its live magnitude as an accepted gap, not as "small". | Fable r2 P1; Sol r2 P0 sub-item |
| 9 | `evaluateWorkerAdmit` recorded as a deliberately excluded fourth reader. | Fable r2 P2(a) |
| 10 | Rate-limited subsystem transition log. | Fable r2 P2(c); Sol r1 |
| 11 | A raise must clear the published value by ≥ 1 quantum (anti-flap). | Fable r2 P2(d) |
| 12 | `confine --list` renders `granted > ceiling` as a drain state, never as a ledger residual. | Fable r2 P2(e) |
| 13 | `admitEffectiveMaximum` keys on the **canonical** path `resolveAdmitSlicePath` returns (`EvalSymlinks`) and is a pure map lookup. | Fable r2 P2(f) |
| 14 | The real-cgroup trio folds into one end-to-end test plus the fixture's negative control. | Fable r2 P2(g) |
| 15 | The governor is signalled after a raise, **outside** the ceiling lock. | Sol r2 P1 |
| 16 | Real-**signal** cgroup tests added (anon growth and cache growth against real kernel accounting). | Sol r2 P1 ("negative control validates only the actuator, not the signal") |
| 17 | Test 12 drives the real `admitConnection` wire path. | Fable r2 E; Sol r2 P1 |

**One reviewer disagreement, resolved explicitly.** Fable verified TTL-to-
`unevaluated` as the right direction (no irreversible state; "no throttle" is
today's behaviour with machine protection unchanged). Sol argued the opposite —
the same `MemAvailable` failure also makes the watchdog unevaluated
(`watchdog.go:256`), so opening admission removes both AIRA pressure responses at
once. v3 takes the middle that both arguments actually support: the last
established ceiling is **held** for the whole TTL and only then expires. Sol gets a
hold window; Fable gets a bounded one.

---

## 1. What this builds, in one paragraph

A daemon subsystem (`sliceceiling`) samples system-wide `MemAvailable` — reusing
`readMemAvailable`/`parseMemAvailable` from `watchdog.go`, never a second reader —
together with `aira.slice`'s own memory accounting, and derives **how large a
ceiling the machine can currently afford to let the slice have**. It publishes
that number in-process. The three existing sites that compute admission capacity
take `min(live memory.max, published ceiling)`; every other consumer of
`memory.max` keeps the real value. New admissions therefore throttle when memory
*outside* the slice tightens, while already-running jobs are untouched — not as a
policy promise, but because nothing kernel-enforced changes.

## 2. Source-verified premises

Re-derived from `master@3b310c4`; the ones v1 got wrong are marked.

**P1 — `memory.max` is read live at every admission decision.**
`readSliceMemory(path)` (`admit.go:1491`) `os.ReadFile`s `memory.current`,
`memory.max`, `memory.stat` on every call. Production callers: `admitConnection`
(`:724`), `evaluateAdmitQueue` (`:1024`), `admitAvailable` (`:489`), and the
display-only `confineManagement` (`confine_manage.go:96`). No cache.

**P1a — but `maximum` has four consumers with different meanings** (Finding A
table above). **This is the correction to the ticket's premise**, and it is why
the override is applied at the capacity sites specifically rather than by moving
the underlying number.

**P2 — the shared system-memory primitive.** `readMemAvailable()` /
`parseMemAvailable()` (`watchdog.go:142`, `:150`) return
`(bytes, established, reason)`. Package-level in `package daemon`, directly
callable. No second reader is written.

**P3 — nothing in AIRA writes the slice's own `memory.max`.** The only writer is
`writeScopeMemoryValue` (`internal/runner/confine_linux.go:1215`), always against
a *scope* handle. `internal/install` only reads (`:618`, `:1163`, `:1488`). **v2
adds no writer either**, so the question the ticket asked ("confirm nothing else
concurrently writes … in a way that could race") is answered by removing the
write, not by defending it. Measured for the record, on a throwaway transient
unit (never `aira.slice`): a direct cgroupfs write to `memory.max` is **not**
reflected in `systemctl show -p MemoryMax`, and **is reverted by
`systemctl --user daemon-reload`** — so a write-based design would have been
silently undone by any unrelated unit reload on this machine.

**P4 — the kernel floors `memory.max` down to a page multiple.** Measured:
writing `600000000` reads back `599998464`. Recorded because it would have been a
live hazard for the write design (a value written *at* a computed floor reads
back *below* it); it is moot for v2 and no longer needs defending.

**P5 — a non-finite slice `memory.max` needs no special handling in v2.**
`readSliceMemory` already returns `!ok, "unbounded"` and admission already
refuses. The subsystem simply publishes nothing. AIRA-16's recorded residual (a
transiently non-finite `aira.slice` making running jobs valid watchdog victims)
is untouched because v2 never writes that file.

**P6 — the watchdog's victim classification is unaffected either way.**
`classifyWatchdogCgroup` (`watchdog.go:834`) decides only *whether a finite
`memory.max` exists in the ancestry*, never its magnitude
(`effectiveWatchdogCapFrom` has no non-test caller). Verified independently by
both reviewers. v2 changes no cgroup attribute at all, so this is doubly moot.

## 3. The signal: what the machine can afford the slice

```
current    = min(currentBefore, currentAfter)          // torn-read guard, see 3.2
sliceAnon  = max(0, current - inactive_file - active_file - slab_reclaimable)
affordable = MemAvailable + sliceAnon                  // what MemAvailable would be if the slice were emptied
desired    = affordable - sliceCeilingSystemReserve    // leave this much for everything else
ceiling    = clamp(desired, 0, live memory.max)
```

**Why `+ sliceAnon`.** `MemAvailable` already counts reclaimable file pages
wherever they live, including inside the slice; it does *not* count the slice's
anonymous pages. Adding `sliceAnon` — and only `sliceAnon` — reconstructs "how
much memory would be available if `aira.slice` released everything", without
double-counting its cache. The consequence is the property Finding B requires:
**when the slice's own jobs grow, `MemAvailable` falls and `sliceAnon` rises by
the same amount, so `affordable` is unchanged and the ceiling does not move.**
The ceiling moves only when memory is consumed *outside* the slice — the desktop,
other sessions, and (per AIRA-102) uncontained containers. That is exactly the
signal the ticket asked for.

`inactive_file` and `active_file` are already parsed by `parseSliceMemoryStat`
(`admit.go:1527`). v3 extends it to return `slab_reclaimable` as a **third**
value, because `MemAvailable` credits most of global reclaimable slab while
`memory.current` charges the slice's share of it — leaving it inside `sliceAnon`
double-counts it, permissively (0.36 GiB live in `aira.slice`, measured). The
existing two return values and therefore admission's AIRA-21 reclaimable discount
are byte-identical; `readSliceMemory` simply ignores the new one.

If `memory.stat` is unavailable the subsystem inserts **no sample** (rather than
silently treating the whole slice as anonymous, which would overstate the ceiling
by the size of the page cache).

**`sliceCeilingSystemReserve = min(MemTotal/4, 16 GiB)` — the owner's own existing
headroom policy, not a number invented here or copied from the watchdog.**
`internal/install/install.go:746` already uses exactly `min(total/4, 16GiB)` when
`aira install` sizes the slice, so this is one policy serving two purposes rather
than a second, competing definition of "how much this machine must keep free".
On this box it evaluates to 16 GiB, which is also `watchdogRecoverMemAvailable` —
and that coincidence is the independent sanity check, not the derivation: the
throttle's *target state* must be one in which the watchdog is quiescent, or the
two subsystems would disagree about whether the machine is healthy. So 16 GiB is
a **lower bound** the reserve must not fall below on a large box, and the
`MemTotal/4` term is what keeps `enforce` from pinning the ceiling near zero on a
≤40 GiB machine. `MemTotal` is read once at startup (it does not change) with the
same line-scan shape as `parseMemAvailable`; a unit test pins the relationship.

Configurability matches the watchdog exactly (`paths.go:101`, `:114`): the reserve
is derived, not a knob; **mode** (`AIRA_DAEMON_SLICE_CEILING_MODE`:
`off` | `observe` | `enforce`, default `off`) and **interval**
(`AIRA_DAEMON_SLICE_CEILING_INTERVAL`, Go duration in `[1s,30s)`, default 2s) are
environment-configurable. The knob for "AIRA may have more/less of this machine"
already exists and is `aira install --memory-max`, which sets the upper bound this
subsystem can never exceed.

### 3.1 Honest consequence, stated up front

On this machine the configured ceiling is **already unaffordable**. Stated in the
formula's own terms so the arithmetic is checkable: the footprint this mechanism
cannot give the slice is `MemTotal − affordable`, i.e. everything that is neither
free, nor reclaimable anywhere, nor the slice's own anonymous pages — non-slice
anonymous memory plus kernel/slab/page tables. Live that is roughly **25 GiB** of
78.5 GiB. With a 16 GiB reserve on top, the affordable ceiling lands near
**38 GiB**, well under the configured 64 GiB. (An earlier draft computed the
non-slice load as `MemTotal − MemAvailable − memory.current`, which wrongly
subtracts the slice's page cache that the formula counts as affordable; Fable
caught it. The ≈38 GiB conclusion is unchanged, the explanation was not.)

That is not a malfunction; it is the finding the ticket is chasing, made
measurable: **the static 64 GiB ceiling has been over-committed relative to the
machine all along, which is how admission could report "room" while the box was
starving.** But it does change this subsystem's character from "an exceptional
pressure response" to "a permanently-engaged capacity governor", which is a
capacity policy the owner set at 64 GiB. Hence: default mode `off`, rollout
`observe` → `enforce`, the coordinating session doing the live deploy, and this
consequence stated first in the ticket Resolution. The first `enforce` on this box
is a real capacity reduction — safe, because it only slows new admissions and
cannot touch a running job, but it must be a decision, not a surprise.

### 3.2 Sampling: torn-read guard, then asymmetric damping

**One sample, four reads, in this order:** `memory.current` (before) →
`memory.stat` → `memory.max` → `MemAvailable` → `memory.current` (after). The
sample uses `current = min(before, after)`.

That ordering plus the `min` makes the sample **exact or restrictive under either
tear direction**, which matters because `/proc/meminfo` and the cgroup files are
not one snapshot:

- the slice *grows* by X during the window: `MemAvailable` is the post-growth
  (lower) value while `current` is the pre-growth (lower) one, so `affordable`
  under-counts by X — restrictive;
- the slice *shrinks* by X: `MemAvailable` is the post-shrink (higher) value and
  `min` takes the post-shrink `current`, which is exactly the consistent pair.

Without the guard, a shrink between the reads adds the same bytes twice and the
`max`-over-window damping below would then *preserve* that permissive spike for
three ticks. A sample whose two `memory.current` reads differ is not discarded —
`min` is already the safe answer, and discarding would make a busy slice
unobservable.

**Damping.** The published ceiling is `max` over the last
`sliceCeilingSamples = 3` **established** samples, quantised down to 256 MiB.
That single expression *is* the hysteresis: **lowering** requires all three recent
samples to agree (≈6 s of sustained external pressure, matching the watchdog's own
debounce), while **raising** happens on the first high sample. Restricting must be
sustained; relieving must be prompt. No latch, no phase, no threshold pair.

Two rules the `max` alone does not give:

- **Partial window ⇒ no publication.** The ceiling is published only once the
  window holds three established samples; before that it is `unevaluated` (no
  throttle). Otherwise `max` over a single sample would lower the ceiling
  immediately at startup and after every expiry, silently voiding the
  three-sample rule.
- **Anti-flap on the raise.** A raise is applied only when it clears the
  published value by at least one 256 MiB quantum, so a value sitting on a
  quantum boundary does not oscillate.

**Loss of signal.** A sample is inserted only when *every* read above is
established. The last published ceiling is then **held** for
`sliceCeilingSampleTTL = max(30s, 3×interval)` and only then expires to
`unevaluated` (no throttle). The hold answers Sol's objection that the same
`/proc/meminfo` failure blinds the watchdog too, so dropping the throttle at that
instant would remove both AIRA pressure responses at once; the expiry answers
Fable's, that an advisory capacity reduction with no signal behind it must not
restrict forever. Nothing here is irreversible, and the expired state is exactly
today's behaviour.

## 4. Where the override is applied (and where it must not be)

New helper, in the subsystem's file:

```go
// admitEffectiveMaximum returns the maximum that CAPACITY questions must use:
// the live cgroup value, reduced by the published pressure ceiling. It is the
// ONLY place the throttle enters admission, and it must never be used for a
// terminality decision or to size a scope's own memory.max (see §0 Finding A).
func (s *Server) admitEffectiveMaximum(slicePath string, maximum int64) int64
```

Applied at exactly three call sites:

1. `evaluateAdmitQueue` (`admit.go:1065`) — the `maximum` passed to
   `checkedAvailable`. **This is the throttle.**
2. `admitAvailable` (`admit.go:495`) — the governor's read-only capacity
   advisory, the same question.
3. `confineManagement` (`confine_manage.go:~113`) — `CeilingBytes`, so
   `confine --list` reports the ceiling a new job actually faces.

Deliberately **not** applied at:

- `admitConnection`'s `ceiling` (`admit.go:736`) — feeds `E_ADMIT_TOO_LARGE`
  (`:742`), `resolveAdmitReserve`'s clamp (`:548`) and
  `resolveDelegateRAMScopeCeiling` (`:739`). All three keep the real static
  maximum, so a job that fits the configured ceiling is **enqueued and waits**
  rather than terminally rejected, and no scope is ever sized from a
  transient pressure reading.
- `enqueueAdmitInternal`'s ceiling check (`admit.go:863`) — same reason; it is
  the same terminal `E_ADMIT_TOO_LARGE`.
- `evaluateWorkerAdmit` (`worker_admit.go:428-484`) — a **fourth** production
  reader of the memory seam, recorded here so nobody later "fixes" the omission.
  It is keyed by an aitest job's **outer scope**, not by the slice, and that
  suite already holds its own slice reservation; throttling it would double-charge
  the same pressure and starve workers inside an already-admitted job.

`admitConnection` therefore needs **no edit at all**. Net change to `admit.go`:
two arguments, one helper call, and one extra return value on
`parseSliceMemoryStat` that its existing caller ignores.

**Keying.** The published ceiling is stored under the **canonical** slice path —
the `EvalSymlinks`-resolved path `resolveAdmitSlicePath` returns (`admit.go:1607`)
and the same string `sliceQueue.path` holds — and applied only on an exact match.
A test slice, or a caller that passed an explicit `--slice`, is unaffected.
`admitEffectiveMaximum` is a **pure map lookup under the leaf RWMutex**: it runs
inside `queue.mu` at `:1065`, so it must never resolve a path, hit the filesystem,
or take another lock.

**Prompt recovery.** After publishing a ceiling that is *higher* than the previous
one, and **after releasing the ceiling lock**, the subsystem calls
`s.governor.signal()`. The admission queue evaluator re-polls on its own ticker
(`sliceQueue.poll`, 250 ms) so it needs no signal, but the RAM-aware governor is
signal-driven (`governor.go:127`) and parked workers would otherwise stay parked
until an unrelated event.

**Lock order.** The published value lives behind a dedicated `sync.RWMutex` on
`Server` that is a strict **leaf**: `evaluateAdmitQueue` takes it while holding
`queue.mu`, so nothing may ever be acquired while holding it, and the subsystem's
own goroutine takes it only to publish, never while holding any admission lock.
The subsystem reads **no** admission state at all — it needs neither the ledger
nor the queue — so there is no lock coupling and none of v1's grant-versus-write
interleaving. (This project has already had one `activeConfines`-under-`queue.mu`
deadlock; leaf position is asserted in a test that runs the evaluator and the
subsystem concurrently under `-race`.)

## 5. Modes and uncertainty behaviour

| condition | behaviour |
|---|---|
| mode `off` (default) | goroutine parks on `ctx.Done()`; publishes nothing; `admitEffectiveMaximum` returns `maximum` unchanged |
| mode `observe` | samples, computes and **reports** the ceiling it would publish; `admitEffectiveMaximum` returns `maximum` unchanged |
| mode `enforce` | as above, and `admitEffectiveMaximum` returns `min(maximum, published)` |
| `MemAvailable` unestablished | no sample inserted; the last published ceiling is **held** until the TTL |
| slice memory read unestablished (incl. `memory.stat`) | no sample inserted (admission itself already fails closed here, `admit.go:1025-1034`) |
| fewer than 3 established samples in the window | nothing published → `unevaluated` → no throttle |
| no established sample within `max(30s, 3×interval)` | published ceiling expires → `unevaluated` → no throttle |
| `desired <= 0` | published ceiling is 0 → `checkedAvailable` returns 0 → new admissions wait. Running jobs unaffected |
| slice path unresolvable | nothing published; retried each tick |

Nothing in v2/v3 has irreversible state, so there is no restart story, no shutdown
restore, and no failure mode that outlives the daemon.

**Logging.** Transitions only (`unevaluated` ⇄ `unthrottled` ⇄ `throttled`) plus
a moved-ceiling line rate-limited to one per minute and to moves of ≥ 1 GiB —
the same discipline as `logAdmitFreezeTransition` (`admit.go:1141`), which logs
transitions rather than steady state because passes run several times a second.
This matters beyond tidiness: with `available` held at 0 by the throttle, the
AIRA-59 fairness freeze arms after the backfill grace and logs `hold`/`yield`
transitions naming the head waiter's reserve (`admit.go:1073-1099`, `:1159`) —
which reads as "one big job is blocking the queue" when the real cause is
external memory pressure. The subsystem's own line is what makes the daemon log
tell the truth.

## 6. Visibility (`aira confine --list`)

`runner.ConfineSliceReserve` gains four fields, filled from a snapshot the
subsystem publishes (measured facts only — no verdicts, no fabricated zeros):

```go
CeilingMode        string // "" when off; else "observe" | "enforce"
CeilingState       string // "unthrottled" | "throttled" | "unevaluated"
CeilingStaticBytes int64  // the live cgroup memory.max; 0 = not established
MemAvailableBytes  int64  // the newest established system reading; 0 = not established
```

`CeilingBytes` (AIRA-73's existing field) now derives from the **effective**
maximum, so it already answers "what does a new job face". Rendering, under the
existing `slice reserve:` line, whenever `CeilingMode != ""`:

```
slice ceiling: 38G effective / 64G configured; system MemAvailable 12G, reserve 16G
slice ceiling: 64G effective = configured; system MemAvailable 34G
slice ceiling: unevaluated (no established system-memory sample in 30s)
slice ceiling: 64G configured; would publish 38G (observe mode, not applied)
```

No line claims what the mechanism cannot establish. In particular it does not say
"no running job is affected" as a reassurance — that is instead true by
construction and documented in the code, not asserted at the operator.

While throttled, `GrantedBytes` can legitimately exceed `CeilingBytes`: jobs
admitted before the ceiling fell are still holding their grants. That is a
**drain state**, not the `LEDGER INCONSISTENCY` the residual line reports, so it
is rendered as one explicitly (`over the effective ceiling by N; draining — no
running job is affected and no new job is admitted until it clears`) rather than
left for the reader to mistake for a lost discharge.

**The blocked client's own line.** `internal/runner/confine_linux.go:593` prints
`waiting for memory admission on aira.slice (reserve X, waited Ns, position …)`
and `admission_linux.go:442` reports `E_ADMIT_SATURATED` as "slice contended" —
both attribute a throttled wait to ordinary contention, which is precisely the
"why am I waiting" the ticket wants surfaced. The client already probes
`confine --list` for its AIRA-24 queue position (`confine_queue_position_linux.go`),
and that reply already carries `SliceReserve`; the fix is to render
`CeilingState`/`MemAvailableBytes` from the reply it already has. **No wire
`Basis` string changes** — the runner validates `rejection.Basis ==
"reject:saturated"` exactly (`admission_linux.go:513`), and a mismatch would fall
through to `fail()` → the flock fallback → an uncounted launch, which is the
AIRA-67 aggregate-OOM class. Message text only.

## 7. Files

New:
- `internal/daemon/sliceceiling.go` — `runSliceCeiling`, `evaluateSliceCeiling`
  (pure decision over injected deps), `sliceCeilingDesired` (the arithmetic,
  separately testable), the published snapshot + `admitEffectiveMaximum`.
- `internal/daemon/sliceceiling_test.go`
- `internal/daemon/sliceceiling_real_cgroup_linux_test.go`

Modified:
- `internal/daemon/paths.go` — `sliceCeilingModeFromEnv`,
  `sliceCeilingIntervalFromEnv` (mirroring the watchdog's pair).
- `internal/daemon/server.go` — the published-snapshot field + mutex; goroutine
  launch and cancel alongside the watchdog's.
- `internal/daemon/admit.go` — **two call sites** (`:495`, `:1065`) take
  `admitEffectiveMaximum`, and `parseSliceMemoryStat` gains a third return value
  (`slab_reclaimable`) that its existing caller ignores, so admission's AIRA-21
  discount is byte-identical. Nothing else. The ticket's "admit.go untouched"
  claim is corrected in the Resolution.
- `internal/runner/confine_queue_position_linux.go` — render the throttle state
  in the blocked client's waiting line, from the `SliceReserve` its existing
  AIRA-24 probe already returns. **No wire `Basis` string changes.**
- `internal/daemon/confine_manage.go` — effective ceiling + the four new fields.
- `internal/runner/confine_manage.go` — the four new struct fields.
- `cmd/aira/main.go` — the `slice ceiling:` rendering.
- `docs/superpowers/specs/2026-08-23-aira-memory-watchdog-design.md` — a
  cross-reference recording the second consumer of `readMemAvailable` and the
  band relationship.
- `.aira/tickets/AIRA-103.md`, `AIRA-91.md`, `AIRA-16.md`, `AIRA-65.md`.

## 8. Tests

### 8.1 Unit (`sliceCeilingDesired` / `evaluateSliceCeiling`, table-driven)

1. **Invariance to the slice's own growth** — the property Finding B turns on.
   Three cases, chosen so the test fails against every wrong formula that has
   been on the table:
   a. anon growth: `MemAvailable` down X, `memory.current` up X ⇒ ceiling
      unchanged. *Fails against raw `MemAvailable`* (v1).
   b. **cache growth**: `memory.current` up X **and** `inactive_file`+
      `active_file` up X, `MemAvailable` unchanged ⇒ ceiling unchanged.
      *Fails against `MemAvailable + raw memory.current`* — case (a) alone does
      not, which is the porosity Fable found in v2.
   c. **slab growth**: `memory.current` up X and `slab_reclaimable` up X ⇒
      ceiling unchanged. *Fails against a `sliceAnon` that omits slab* (v2).
   d. `reclaimable > current` (a legal transient across the two reads) ⇒
      `sliceAnon` clamps to 0, never negative.
2. External pressure only: raising non-slice usage (MemAvailable down,
   `memory.current` unchanged) lowers the ceiling by the same amount.
2a. **Torn-read guard**: with `currentBefore != currentAfter` in each direction,
   the resulting ceiling is ≤ the ceiling computed from the consistent
   post-window pair. *Fails against a single `memory.current` read.*
3. `ceiling <= live memory.max` always; `desired > maximum` ⇒ ceiling = maximum
   ⇒ `CeilingState == "unthrottled"`.
4. `desired <= 0` ⇒ ceiling 0 ⇒ `checkedAvailable` yields 0.
5. Overflow/underflow: values near `MaxInt64` clamp via `addClamp`/`subtractFloor`
   rather than wrapping; negative `MemAvailable` is impossible per
   `parseMemAvailable` but is rejected defensively.
6. `memory.stat` unreadable ⇒ **no sample**, not a sample with `reclaimable = 0`.
7. Damping asymmetry **and the partial window**: three descending samples are
   needed before the published ceiling drops; **one** ascending sample raises it
   immediately (if it clears a quantum); with fewer than three established
   samples nothing is published at all — the case that made "lowering needs three
   samples" false at startup and after every expiry.
8. TTL: the last ceiling is held while samples fail, then expires after
   `max(30s, 3×interval)` to `unevaluated`, and `admitEffectiveMaximum` returns
   `maximum` unchanged from that point.
9. `sliceCeilingSystemReserve == min(MemTotal/4, 16 GiB)`; on a large box it
   equals `watchdogRecoverMemAvailable`, and it is always `> 0` and
   `< MemTotal`.
9a. Anti-flap: a raise smaller than one quantum above the published value is not
   applied.
10. Mode gating: `off` publishes nothing and touches no dep; `observe` computes
    and reports but `admitEffectiveMaximum` returns `maximum` unchanged.
11. **Slice keying**: a published ceiling for `/…/aira.slice` does not affect
    `admitEffectiveMaximum` for any other path.
12. **The override reaches capacity and only capacity** — the load-bearing
    non-porous test, and the direct test of Finding A. It drives the **real
    `admitConnection` wire path** (the existing `admitWriteFrame`/`admitAfter`
    seams), not the arithmetic, so a path-keying mismatch or a throttle that
    leaked into `admitConnection` is caught. With a published ceiling well below
    the static maximum: a request that fits the static ceiling but not the
    throttled one receives **no `E_ADMIT_TOO_LARGE` frame** and stays queued,
    then is granted once the ceiling lifts; the grant's `ScopeCeiling` for a
    `delegate_ram` request equals the **un-throttled** value; `resolveAdmitReserve`'s
    OOM-escalation clamp is unchanged. Fails against the v1 cgroup-write design
    and against any implementation that plumbs the throttle through
    `admitConnection`.
13. Concurrency: evaluator and subsystem run together under `-race`; the
    published-snapshot mutex is never held across another lock.

### 8.2 Real cgroup — the empirical safety proof

The brief requires a real-cgroup test proving the safety bound empirically rather
than by code-reading. In v2 the bound is stronger and differently stated: **the
kernel-enforced limit of a live cgroup holding a running process is never
modified, and the process is never pressured, no matter how hard the subsystem is
driven.** All fixtures use `cgrouptest.IsolatedScopeParent(t)` — a fresh
`os.MkdirTemp` cgroup whose name is asserted not to collide with the
`.aira-CONFINE-` prefix every production scan enumerates, torn down with
`cgroup.kill` in `t.Cleanup`. **`aira.slice` itself is never touched**, so nothing
here can disturb the ~30 live confine jobs on this box.

**`TestSliceCeilingRealCgroupThrottlesAdmissionWithoutTouchingTheJob`** — one
end-to-end test, not v2's trio (folded per Fable: for a *non-write* property the
byte-identical assertion is the only real content, so it belongs with the
behaviour it must coexist with).

1. Isolated parent, `+memory`, `memory.max = 2 GiB`, `memory.swap.max = 0` (a
   breach OOMs deterministically instead of swapping).
2. A helper (the `AIRA_WATCHDOG_PROC_HELPER` re-exec pattern,
   `watchdog_test.go:1249`) placed in it via `SysProcAttr.UseCgroupFD`, touching
   600 MiB of anonymous memory, then blocking on stdin.
3. Record `memory.max`, `memory.current`, `memory.events` `oom_kill`.
4. Drive `evaluateSliceCeiling` in `enforce` mode against that real path with
   `MemAvailable` stubbed low enough to force `desired` to zero — the hardest
   throttle the subsystem can ever ask for — with `admitSliceHeadroomBase` and
   the quantum injected small so the fixture's scale actually exercises the
   throttle instead of falling through as a no-op (the porosity Fable found in
   v2's §8.2).
5. Assert **all** of: `memory.max` byte-identical; `oom_kill` unchanged; the
   helper alive; a real `sliceQueue` waiter that fits the static ceiling but not
   the throttled one **stays queued**; and it is granted once the stubbed
   `MemAvailable` recovers. Every clause is needed — the first three make the
   safety claim, the last two stop the whole thing passing vacuously.

**`TestSliceCeilingRealCgroupHarnessDetectsALimitWrite`** — the **negative
control**. Same fixture; the test itself writes `32 MiB` straight to
`memory.max`, bypassing the subsystem, and asserts the helper **is** killed and
`oom_kill` **does** increase. It proves the *fixture*, not the subsystem: without
it, "`memory.max` unchanged and the helper survived" could be true of a fixture
that could never have observed a kill in the first place. That is the project's
standing rule — a test that cannot fail against the wrong implementation proves
nothing — applied to the harness itself.

**`TestSliceCeilingRealCgroupSignalTracksRealAccounting`** — the empirical proof
of the **signal**, which the two tests above do not touch (Sol: "the negative
control validates only the no-write actuator"). Against the same real cgroup,
with `MemAvailable` supplied by a stub that models a machine of fixed size —
`memAvailable = simulatedTotal − realSliceCurrent − simulatedOutside`, so the
slice's contribution comes from the **kernel's own `memory.current`** while
machine-level noise from the ~30 other live jobs on this box is excluded:

- the helper allocates a further 400 MiB of **anonymous** memory ⇒ the published
  ceiling must not move (real anon accounting, real invariance);
- the helper writes and re-reads a 400 MiB file inside the cgroup so
  `memory.current` rises as **page cache** ⇒ the published ceiling must not move
  (proves `inactive_file`/`active_file` really absorb it on this kernel);
- `simulatedOutside` rises by 400 MiB with the slice untouched ⇒ the ceiling must
  fall by ≈400 MiB (quantised).

This is deliberately not a raw machine-level measurement: on a box carrying 30
concurrent confine jobs, `MemAvailable` moves by gigabytes between samples and a
400 MiB signal would be pure noise. Modelling only the machine while using the
kernel's real per-cgroup accounting tests the part that is actually uncertain.

All skip on a host without cgroup-v2 delegation and hard-fail under
`AIRA_REAL_CGROUP=1`, per `cgrouptest.SkipOrFailRealCgroup`.

### 8.3 Whole suite

`aira confine -- go build ./...`, `aira confine -- go vet ./...`,
`aira confine -- go test ./...`, exit codes recorded exactly, run one at a time.

## 9. Explicit deferrals

- **Kernel enforcement of the reduced ceiling** (the ticket's original actuator).
  Deferred with reasons in §0; it would first have to solve Findings A–C.
- **`memory.high` is not touched** — lowering it generates exactly the
  sustained-reclaim PSI AIRA-91 Part B says to stop producing.
- **AIRA-16 half (2), the slice-*internal* pressure trigger**, is not built here:
  that is a kill decision under internal pressure, this is a preventive admission
  throttle under external pressure. AIRA-16 stays open on its own terms.
- **No per-threshold environment knobs** (§3), matching the watchdog.
- **`sliceAnon` is an approximation** of "what the slice would release". The
  residuals, each with its measured magnitude and its direction, recorded rather
  than modelled:
  - **Swap — the largest, and NOT small.** Live: `SwapTotal` 20 GiB /
    `SwapFree` 14.4 GiB ⇒ ~6.5 GiB swapped out, and `aira.slice`'s
    `memory.swap.current` is ~2 MB, so essentially all of it belongs to processes
    *outside* the slice. `MemAvailable` therefore overstates the rest of the box's
    health by up to that much: the signal measures memory others currently
    **occupy**, not memory they **need**, so under thrash it is **permissive**
    (over-admits). This is exactly the same limitation the existing watchdog has,
    and closing it would mean modelling working-set demand, which nothing in this
    project does. An earlier draft called this "small relative to a 256 MiB
    quantum"; that was wrong and Fable caught it.
  - Slice-owned **reclaimable slab** was double-counted permissively in v2
    (0.36 GiB live); v3 subtracts `slab_reclaimable` and closes it.
  - **Unreclaimable** slice kernel memory (0.37 GiB live) stays inside
    `sliceAnon` and cancels correctly.
  - `shmem` is swap-backed and therefore sits on the anon LRU in both
    `/proc/meminfo` and `memory.stat`, so the anon/file split is consistent
    (independently verified by Fable).
  - `MemAvailable` withholds `min(pagecache/2, wmark_low)`. Measured here:
    `wmark_low` = 152 MiB against ~38 GiB of page cache, so the term is the
    constant and slice cache growth is invariant. On a *cache-starved* box the
    withheld term becomes `pagecache/2` and slice cache growth reads
    **restrictive** by up to half — the safe direction, on the box where it
    matters.
  - A **second capped slice**, or containers outside `aira.slice` (AIRA-102),
    read correctly as external footprint. Their *unused* commitments are
    invisible, so two independently-governed slices could still overcommit each
    other; out of scope here.

## 10. Risks

| risk | mitigation |
|---|---|
| the override silently reaches a terminality or scope-sizing path | §8.1 test 12 pins all three; `admitConnection` is not edited at all |
| first `enforce` on this box is a real capacity cut (~64G → ~38G) | default `off`; `observe` first; §3.1 states the number in advance; cannot touch a running job |
| the signal is wrong and the ceiling flaps | §8.1 tests 1/2/7; damping is `max` over 3 samples, quantised |
| lock coupling / deadlock | the published snapshot is a strict leaf; the subsystem reads no admission state; `-race` concurrency test |
| loss of kernel enforcement | unchanged from today: static cap + oomd + watchdog; recorded as a deliberate deferral |
