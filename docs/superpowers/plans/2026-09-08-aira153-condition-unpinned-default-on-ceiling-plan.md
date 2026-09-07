# AIRA-153 — one slice-fitted quantity bounds the unpinned reserve PRIOR and the OOM escalation clamp

Status: **plan revision 2 — rewritten against the plan gate's FAIL. No
implementation has begun; this commit changes only this document.**

This changes SIZING on the machine-wide admission gate every `aira confine` job
on this box passes through, at **two** call sites (revision 1 had one), so it is
the full two-loop per `CLAUDE.md`, not the light path.

Ticket: `.aira/tickets/AIRA-153.md` (P2, `admission`/`confine`), filed as
AIRA-149 deferral **F4** and sharpened by AIRA-151's own resolution notes. The
structural defect it interacts with is `.aira/tickets/AIRA-150.md`; the shipped
sibling whose clamp this revision RETARGETS is `.aira/tickets/AIRA-151.md`; the
superseded candidate is `.aira/tickets/AIRA-152.md`.

Branch: `aira153-condition-default-on-ceiling`, rebased onto `origin/master` at
`a836a90` (revision 1 was written against `0165918`; the only commit between
them is a ticket file, `.aira/tickets/AIRA-160.md`, and every line cited below
was re-read at `a836a90`).

## What revision 2 changes, and why

The gate FAILed revision 1 on four required changes and two factual fixes. It
also RULED that a fix is warranted at all — from source, not from this plan's
narrative — and that the direction (candidate C, daemon-side) is correct. The
four changes are not cosmetic; two of them change what ships:

1. **The fit is GATED on `reserve >= ceiling`** (F3). Revision 1 fitted whenever
   `FIT(ceiling) < reserve`, which lowered a job's kernel-enforced `memory.max`
   by up to 13% on slices where the 4 GiB default is admissible AND grantable
   today — a silent under-provision on a band where nothing is broken, and a
   violation of revision 1's own stated criterion for rejecting
   `min(hint, ceiling/2)`. The fit now fires only where the prior is at or over
   the ceiling, i.e. exactly where it is refused or wedged today.
2. **AIRA-151's clamp is RETARGETED to the same quantity, and ships WITH this
   change** (F1, and the gate's explicit ruling on revision 1's §10 Q5).
   Revision 1 filed the consequence as deferral G1 and called it "one rung".
   The gate's derivation — which I re-derived and confirm below — is that it is
   a DETERMINISTIC, PERMANENT 30-minute wedge for every job whose footprint
   exceeds the fitted cap, on exactly the population this ticket exists to
   serve. It cannot be deferred, and it can only be closed at the clamp.
3. **The agent guide carves out the over-fit case** (F2). "RE-RUN becomes true
   again on a small slice" is not true for a job OOM-killed AT the fitted cap;
   shipping it unqualified would be an honesty regression.
4. **The new dominant shape is pinned executably** (F4), with a mutation (M8)
   that reintroduces the wedge and must go RED.

Both minor factual fixes are applied (F6: `admitMaxWaiters` bounds waiters, not
outstanding jobs; F7: T10's fixture margin). The gate's §10 rulings are recorded
in §10 as rulings, not as open questions.

One place where this revision does NOT do what the gate's text said, with the
derivation: the gate directed the AIRA-149 facet-2b fixture onto **AIRA-150
route 2** (a pinned reserve exactly equal to the ceiling). Route 2 does survive,
but it collapses the fixture's own stated contract — `oomClampedHistory`'s
comment requires the resolved reserve to be "different from what the client
asked for, so `Required`, `Ceiling` and `Grantable` are three distinct numbers
an implementation that echoes the request cannot fake", and a pinned reserve IS
what the client asked for. §7.2/U9 uses **AIRA-150 route 3** instead — an
ORDINARY ESTIMATE that lands exactly on the ceiling, the other route AIRA-150
names and this change leaves untouched — which satisfies the gate's requirement
(re-base onto a route that legitimately survives) while keeping the property the
fixture was built to have. The exact fixture is derived, not searched:
`PeakMax = 3698281962` gives `estimate == 4253024256 == ceiling` byte-exactly.

---

## 0. Source read, at the commit this plan was written against

Every line below was read fresh in this worktree at `a836a90` (== `origin/master`).

| Fact | Where | Confirmed |
| --- | --- | --- |
| `ResolveConfineReserve` leaves an unpinned request at exactly `DefaultConfineMemoryReserve`, and the ceiling appears nowhere in it | `internal/runner/confine.go:103-119`, `:19` (`4 << 30`) | yes — this is the ticket's subject, and the function is PURE and portable, with no access to any slice fact |
| `--memory-reserve`, `--memory-max` (non-delegate) and `--delegate-ram` each set `pinned` | `confine.go:104-117` | yes — three independent arms, all pinned |
| the client sends `pinned = !DaemonEstimateMemory \|\| MemoryReservePinned`, and only the confine path sets `DaemonEstimateMemory: true` | `internal/runner/admission_linux.go:467`; `internal/runner/confine_linux.go:1631` | yes — **so the affected population is exactly "unpinned confine requests"; every `aira run` admission is pinned and cannot reach the fit** |
| a pinned request returns at the first line of `resolveAdmitReserve` with `pinned:client` | `internal/daemon/admit.go:1609-1611` | yes |
| the ordinary estimate needs ≥ 3 usable samples; the estimator's margin constant is `memoryEstimateSafetyPct = 15`; the estimate is `peak + peak*15/100`; `MaxMemoryEstimateReserve = 1 << 50` | `internal/runner/resource_estimate.go:11-13`, `:33-41`, `:43-65`, `:8` | yes |
| the estimator's four `!ok` bases are `fallback:no-history`, `fallback:capture-unavailable`, `fallback:insufficient-samples:n=%d`, `fallback:malformed` | `resource_estimate.go:33-46`, `:69-71` | yes — non-empty on every `!ok` path |
| `ordinary.OOMCount = 0` before the estimator call, so the estimator's own `estimate:oom:` arm is unreachable from this branch | `admit.go:1630-1631` vs `resource_estimate.go:56-60` | yes |
| the OOM branch, as shipped by AIRA-151: `escalated` + overflow guard, `oomBasis := basis + ",oom-on-record"`, then `if escalated > reserve { reserve = escalated; oomBasis = "estimate:oom-escalated"; if MaxOOMPeak < ceiling && reserve > ceiling { reserve = ceiling; oomBasis += ",ceiling-clamped" } }`, then `return reserve, oomBasis` | `admit.go:1647-1722`, the nested clamp at `:1716-1719` | yes — **this revision changes the clamp's TARGET and its GUARD, and nothing else about it (§3.3)** |
| the `SampleCount >= 3 && reserve > 0` return, which returns the CLIENT HINT when the estimator was `!ok` at three or more samples | `admit.go:1723-1725` | yes — a fifth site the hint escapes through |
| the machine-wide p90 prior return, reached whenever the per-signature block did not return | `admit.go:1729-1734` | yes — `estimate:p90-prior`, built from a synthetic `PeakRSSStats{TotalCount:3, SampleCount:3, PeakMax: peak}` |
| the four post-block fallbacks each `return request.reserve` verbatim: `fallback:no-signature`, `fallback:history-unavailable`, `fallback:insufficient-samples`, `fallback:no-history` | `admit.go:1735-1744` | yes — **these are where a NOVEL command on a small slice dies today** |
| `resolveAdmitReserve` takes `request admitRequest` BY VALUE | `admit.go:1608` | yes — so assigning `request.reserve` inside it cannot escape to any caller |
| `admitConnection` computes the request-entry ceiling as `subtractFloor(maximum, admitSliceHeadroom(admitOutstandingJobs(path)+1))`, resolves against it, and refuses terminally at `reserve > ceiling` | `admit.go:1920-1932`, `admitOutstandingJobs` at `:1276-1288`, `subtractFloor` at `:972-977` | yes — `subtractFloor` floors at 0, so `ceiling >= 0` always |
| the too-large refusal happens BEFORE `enqueueResolvedConfineAdmit`: nothing charged, no waiter, no scope | `admit.go:1929-1933` | yes |
| the enqueue-time re-check refuses `E_ADMIT_TOO_LARGE` again under `queue.mu`, and the waiter is built with the RESOLVED reserve, never `request.reserve` | `admit.go:2080-2082`, `:2115` | yes |
| `admitSliceHeadroom(n) = base + n*perJob`, production defaults `base = 2 << 30`, `perJob = 64 << 20`, and **ci-shim mode does not override either** (the only assignments are the defaults in `NewServer`) | `admit.go:1079-1089`, `:49-50`; `internal/daemon/server.go:234-235`; `internal/daemon/shim.go:132-201` | yes — F6's correction; the shim swaps the slice resolver and the memory reader only |
| the grant gate is `waiter.reserve > available`, `available = checkedAvailable(current, effectiveMaximum, reclaimable, outstanding+adopted, admitSliceHeadroom(outstanding+adopted+1))`, charging `max(current-reclaimable, outstanding+adopted)` | `admit.go:2459-2461`, `:2484`, `:2648-2665` | yes — **so a reserve equal to the ceiling is grantable only inside AIRA-150's residual band; a reserve below it by M tolerates M of charge** |
| **the resolved reserve BECOMES a non-delegate unpinned job's own hard scope `memory.max`** on an admitted daemon grant, recorded as `cap-source=auto:daemon-reserve` | `internal/runner/confine_linux.go:978-981` | yes — this change sizes kernel-enforced containment |
| the unpinned CLIENT-side fallback (flock, daemon unavailable) is deliberately left UNCAPPED | `confine_linux.go:971-977` (comment) | yes — so a client-side fit would have no effect there (§2.3) |
| `MinPinnedScopeCap = 1 << 20` is "the smallest reserve a caller may DECLARE", refused at the runner boundary | `internal/runner/confine.go:48-62`; `confine_linux.go:471-473` | yes |
| `workerAdmitEstimatedBytesMin = 1 << 20` exists "so a sub-page estimate can never floor memory.max to zero pages and instant-OOM the worker on placement" | `internal/daemon/worker_admit.go:27-30` | yes — the precedent for this plan's degenerate floor, in this codebase's own words |
| worker-admit is a SEPARATE path with a client-supplied `estimated_bytes`; it never calls `resolveAdmitReserve` | `worker_admit.go:915-916`, `:941` | yes — untouched |
| **the default install sizes the slice at `MemTotal - min(MemTotal/4, 16 GiB)`, floored to whole GiB, with a 4 GiB minimum** | `internal/install/install.go:1292-1304`; `minimumCeilingGiB = 4` at `:52` | yes — **this is the ruling fact (§1.2); the `2/3-MemTotal` comment elsewhere is stale** |
| a confined job's default admission wait is **30 minutes** | `confine_linux.go:1616-1618` | yes — the cost of a wedge, and why a wedge is worse than a refusal |
| `E_ADMIT_SATURATED`'s documented meaning to an agent is "the job is simply owed a RETRY when the box frees up (nothing about the request is wrong)"; `too_large` means "the request itself cannot be satisfied as written" | `internal/core/skill.go:323` | yes — so a wedge that ends in `saturated` actively misdirects the agent |
| a job OOM-killed in its scope records `memory.peak` as its peak with `oom=true`, and the store's `MaxOOMPeak` is `MAX(peak_rss) WHERE oom=1` | `confine_linux.go:1363-1386` (`RecordConfinePeak`); `internal/store/confine_peak_history.go:58-64` | yes — **so a job killed at a cap of C records `MaxOOMPeak ≈ C`, which is the whole of F1** |
| ci-shim reports no peak-RSS and no OOM (AIRA-121 C10), so the OOM branch is unreachable there — the four post-block fallbacks are not | `internal/runner/confine_shim_linux.go:469-484`; `admit.go:1735-1744` | yes — the shim's route into this defect is `fallback:no-history` |
| the AIRA-52 gauge selects `estimate%` then `$`-anchors its matchers, so every other basis lands on `default:` → `malformedBasis` + `excluded`; `fallback:` bases never enter the population | `internal/store/admission_insight.go:64-66`, `:111-122`; `internal/runner/estimate_actual.go:47` | yes — unchanged by this ticket (R5) |
| the AIRA-128 fixture's slice budget is `runner.DefaultConfineMemoryReserve + (2 << 30)` = 6 GiB; fixture headroom is 32 MiB + 8 MiB; its escalation is ~84 MiB | `internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go:93-133`, `:174-175` | yes — §1.5 shows it is a structural no-op under BOTH halves of this change |
| AIRA-149's facet-2b fixtures: eight `maximum = 4 GiB` declarations serving ten `oomClampedHistory()` call sites, plus two `8 GiB` declarations that use an ordinary-estimate history instead | `internal/daemon/admit_saturated_diagnosis_test.go:41-42`, `:57-83`, `:225-248` | yes — **and §1.5 corrects revision 1: the ten sites DO change under the retargeted clamp and must be re-based (U9)** |
| `oomClampedHistory`'s own contract is "a resolved reserve EXACTLY equal to the request-entry ceiling, and different from what the client asked for" | `admit_saturated_diagnosis_test.go:57-60` | yes — the reason U9 uses AIRA-150 route 3 rather than route 2 |
| the agent guide's self-heal paragraph carries AIRA-151's clause, and `skill_test.go` pins its exact sentences | `internal/core/skill.go:324`; `internal/core/skill_test.go:639-660` | yes — one of those sentences becomes false and must move (§3.7) |
| `E_ADMIT_TOO_LARGE`'s operator message is `required=%d cap_minus_headroom=%d basis=%s`, raw bytes | `admit.go:2799-2801` | yes — unchanged here; AIRA-151's filed G3 |

### 0.1 The quantity, and every figure this plan turns on

All arithmetic below was computed with the exact integer form specified in §3.2,
not estimated, and re-derived independently of revision 1.

```
FIT(c) = floor(c * 100 / 115)   "the largest reserve that, grown by the
                                 estimator's own 15% margin, still fits inside c"

c                FIT(c)           c - FIT(c)      what it is
--------------   --------------   -------------   -----------------------------
      1205862          1048575          157287    one byte under MinPinnedScopeCap
      1205863          1048576          157287    exactly MinPinnedScopeCap
    629145600        547083130        82062470    T13's sibling fixture (640 MiB slice)
   1031798784        897216333       134582451    the AIRA-139/149/151 fixture (1 GiB)
   2684354560       2334221356       350133204    U2's tie-row ceiling
   3179282432       2764593419       414689013    T9's fixture (3 GiB slice)
   4227858432       3676398636       551459796    **a DEFAULT install on an 8 GiB box**
   4253024256       3698281961       554742295    AIRA-149 facet-2b (4 GiB slice)
   4294967296       3734754170       560213126    a ceiling exactly at the default
   6400507904       5565659046       834848858    AIRA-128's fixture (6 GiB slice)
   8589934592       7469508340      1120426252    scope-test row (a)'s ceiling
  10737418240       9336885426      1400532814    confine_admit_test's 10 GiB
  47244640256      41082295874      6162344382    scope-test row (c') ceiling
  59055800320      51352869843      7702930477    **scope-test row (b) ceiling (55 GiB)**
  66504884224      57830334107      8674550117    **production aira.slice, j = 0**
  68719476736      59756066726      8963410010    the static 64 GiB (headroom 0)
 107374182400      93368854260     14005328140    row (e-malformed)'s 100 GiB
 9223372036854775807  8020323510308500701          math.MaxInt64 — the split form is
                                                  positive; the naive c*100 overflows to 0
```

`FIT(c) < c` for every `c >= 1`, and the identity `c = 115q + r ⇒ ⌊100c/115⌋ =
100q + ⌊100r/115⌋` is exact, so the two forms of the division agree on every row
above.

**Where the fit fires, under revision 2's gate.** The fit is applied only when
the prior is at or over the ceiling, so for the 4 GiB unpinned default:

- **ceiling ≥ 4,294,967,297 B: strict no-op.** Not one byte moves, by
  construction rather than by measurement.
- ceiling ≤ 4,294,967,296 B: the prior is fitted to `FIT(ceiling)`, strictly
  below the ceiling.

With production headroom (`base = 2 GiB`, `perJob = 64 MiB`) and `j` outstanding
jobs at arrival, the trigger is a slice `maximum` at or below
`4294967296 + 2147483648 + 67108864*(j+1)`, i.e. **≤ 6.0625 GiB at `j = 0`**,
rising by 64 MiB per outstanding job. On the 64 GiB production slice the
condition is unreachable at any plausible occupancy: it would need ~918
outstanding jobs, and long before that the headroom term self-limits (at
`j = 918` the headroom alone is ~59.5 GiB against a 64 GiB cap, and
`subtractFloor` collapses the ceiling), with the AIRA-114 aggregate bound
tighter still. *(F6: `admitMaxWaiters = 256` bounds `queue.waiters`
(`admit.go:2077`), NOT `outstandingJobs`, which is the term in the entry
ceiling. Revision 1 cited it wrongly; the conclusion holds for the reason just
given.)*

**Where the retargeted clamp fires.** The clamp is entered only when the OOM
escalation determined the value AND that value exceeds the ceiling
(`escalated > ceiling`, i.e. `MaxOOMPeak >= ⌈2c/3⌉`), and it now applies only
while `MaxOOMPeak < FIT(ceiling)`:

```
production ceiling 66504884224:
  MaxOOMPeak <  44,336,589,484 (41.29 GiB) : escalation under the ceiling, no clamp (unchanged)
  MaxOOMPeak in [44336589484, 57830334107) : clamp fires -> 57830334107 (53.86 GiB)
                                             instead of 66504884224 (61.94 GiB)
  MaxOOMPeak in [57830334107, 66504884224) : NO clamp -> terminal E_ADMIT_TOO_LARGE,
                                             where master clamped to the ceiling
  MaxOOMPeak >= 66504884224                : no clamp (unchanged, master's guard too)
```

### 0.2 The population, named unambiguously

| # | What determined the number | Bounded by the slice? | Rationale |
| --- | --- | --- | --- |
| P1 | the client's UNPINNED reserve hint (`fallback:no-signature` / `:history-unavailable` / `:insufficient-samples` / `:no-history`, the `SampleCount>=3` `!ok` return, and the OOM branch's rows (d)/(e)) | **YES — fitted, when it is at or over the ceiling** | a PRIOR: a compiled-in constant with no relationship to this command or this slice |
| P2 | the machine-wide `estimate:p90-prior` | **YES — same rule, same gate** | a PRIOR: a measurement of OTHER commands, used because this one has none |
| E1 | the ordinary per-signature estimate (`estimate:max=…`, `estimate:capped`) | no | THIS command's own measured evidence; over the ceiling it is refused terminally, naming both numbers |
| E2 | the OOM escalation (`estimate:oom-escalated`) | **only where AIRA-151 already bounded it** | THIS command's own evidence. AIRA-151 decided that an escalation over the ceiling may be cut down; revision 2 changes only WHAT it is cut down TO, and to what the guard compares (§3.3) |
| E3 | a PINNED reserve (`pinned:client`) | no | the operator's own number; returns before any of this |

The rule AIRA's honesty conventions already draw everywhere else: **a guess may
be bounded by an established fact; a measurement may not be silently reduced to
fit one** — with the standing exception AIRA-151 owns and reasoned about, where
an over-ceiling escalation is cut down rather than refused so a runnable job is
never permanently wedged. Revision 2 makes that exception actually deliver what
it promises, because cutting to the ceiling produces a value the slice cannot
grant.

---

## 1. What is wrong, and the exact population that changes

### 1.1 The defect

`ResolveConfineReserve` is a pure function of the request. It cannot see the
slice, and by design it must not: AIRA-62 is the bug that arises when the reserve
decision is duplicated outside it. So an unpinned confine request arrives at the
daemon carrying a compiled-in 4 GiB constant as its reserve hint, and the daemon
— which does know the ceiling, has it in scope, and passes it to
`resolveAdmitReserve` as an argument — uses it only to REFUSE the hint, never to
bound it.

On a large slice this is invisible. On any slice whose ceiling is at or below the
constant, the constant alone meets the boundary at `admit.go:1929`, and every
path that returns the hint verbatim (six sites, §0's table) yields a value that
is refused. The consequence is not "some requests are sized badly": it is that
**no unpinned confine job can run on such a slice at all**, because a novel
command has no history, `fallback:no-history` returns the constant, and the
constant is refused.

### 1.2 Is a fix warranted at all? — yes, from the install default

The gate ruled on this and I record the derivation here because it is the reason
"working as intended, use `--memory-reserve`" is refused:

- `aira install` with no `--memory-max` sizes the slice at
  `MemTotal - min(MemTotal/4, 16 GiB)`, floored to whole GiB, minimum 4 GiB
  (`install.go:1292-1304`, `:52`).
- On an **8 GiB box** that is a **6 GiB** slice. Its entry ceiling at zero
  occupancy is `6442450944 - (2147483648 + 67108864) = 4227858432`, which is
  **below** `DefaultConfineMemoryReserve = 4294967296`.
- Therefore **every unpinned `aira confine -- <cmd>` on a default install of an
  ordinary 6–9 GiB box is refused `E_ADMIT_TOO_LARGE` at request entry** — and
  `SKILL.md` mandates that exact unpinned form for every heavy command.
- The same holds for any ci-shim container budget below ~6.06 GiB, whose
  documented promise is that "existing recipes run verbatim with no `if CI`
  branching" (`skill.go:320`).
- AIRA-128's cold-start self-heal is broken on that whole class, and the 4 GiB
  constant is documented in source as "a conservative #50 no-history fallback" —
  a guess. Bounding a guess by a known fact is warranted.

A product whose own install default plus its own mandated usage refuses every
job on an ordinary machine has a defect, not an escape-hatch UX gap.

### 1.3 What AIRA-151 changed, and why this is its completion rather than its reversal

- **before AIRA-151:** a signature with an OOM record had its over-ceiling hint
  clamped onto the ceiling, so it was admissible and *sometimes* granted — inside
  the residual band AIRA-151 §2.3.1 derives, which is byte-exact zero charge on a
  slice the request entered empty. Broken, but occasionally worked.
- **after AIRA-151:** that clamp applies only where the escalation determined the
  value, so on the small slices this ticket is about the hint is returned
  unclamped and refused terminally, every time.

AIRA-151 was right that a wait which almost always ends in a refusal is worse
than an immediate honest refusal, and it removed the last accidental mitigation
of THIS defect. **AIRA-153 does not undo it.** It removes the reason a PRIOR was
over the ceiling in the first place, and then — because the fit creates a new
population of jobs that can reach the clamp — fixes the clamp's target so the
value it produces is one the slice can actually grant. Both halves express one
quantity; see §2.5 for why they cannot ship apart.

### 1.4 The exact population that changes

Two disjoint populations, one per site.

**A — the fitted prior.** A request's resolution changes iff **all** of:

```
NOT request.pinned                       (so: not --memory-reserve, not --memory-max,
                                          not --delegate-ram, not any `aira run`)
AND the returned number is P1 or P2      (a prior, per §0.2 — never E1/E2/E3)
AND that number >= ceiling               (the GATE: only where it is refused or
                                          wedged today — F3)
AND FIT(ceiling) >= runner.MinPinnedScopeCap    (the degenerate floor, §3.4)
```

**B — the retargeted clamp.** A request's resolution changes iff **all** of:

```
the OOM escalation determined the value  (escalated > reserve, AIRA-151's own condition)
AND escalated > ceiling                  (AIRA-151's own condition, unchanged)
AND MaxOOMPeak < ceiling                 (master would have clamped)
```

with two sub-cases:

- `MaxOOMPeak < FIT(ceiling)`: still clamps, to `FIT(ceiling)` instead of
  `ceiling` — a value ~13% lower and, unlike the ceiling, **grantable**;
- `FIT(ceiling) <= MaxOOMPeak < ceiling`: no longer clamps at all — refused
  terminally with `required` and `cap_minus_headroom`, immediately, where master
  admitted it at the ceiling and left it to wait out its window.

Two derivations that must not be inferred, established from source in §0:

- **Pinned requests never reach either site**, because `resolveAdmitReserve`
  returns `pinned:client` at its first line. The delegate-ram arm is already
  pinned by a shipped test row
  (`confine_reserve_resolution_linux_test.go:176-178`).
- **`aira run` never reaches them either**, because the client sets
  `pinned = !DaemonEstimateMemory || MemoryReservePinned` and only
  `confine_linux.go:1631` sets `DaemonEstimateMemory: true`.

### 1.5 What does NOT change — verified against the fixtures, not assumed

Revision 1 claimed three no-ops. Two survive; **one was wrong and is corrected
here**, which is why §7.2 grew.

- **AIRA-128's real-cgroup fixture and its unit twin are untouched, under both
  halves.** Ceiling 6400507904: the 4 GiB prior is *below* it, so the gated fit
  does not fire; and its escalation (~84 MiB) never exceeds the ceiling, so the
  clamp is never entered.
  `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` and
  `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` keep every assertion.
- **The production slice is a no-op for the fit at any reachable occupancy**
  (§0.1). The *clamp* is not a no-op there; its production effect is stated
  exactly in §0.1 and accepted in R3.
- **CORRECTION — AIRA-149's ten facet-2b call sites DO change.** Revision 1 said
  they were untouched. Under the retargeted guard they are not:
  `MaxOOMPeak = 3758096384` is **above** `FIT(4253024256) = 3698281961`, so the
  guard fails, the clamp does not fire, the escalation 5637144576 stands, and the
  request is refused terminally at entry instead of enqueuing — every one of
  those ten tests would fail in `startSaturatedAdmit`'s "the admission request
  never reached the queue" loop. U9 re-bases the fixture; §7.2 says onto what and
  why.

---

## 2. Directions

The ticket names three candidates and mandates none. All three are evaluated from
source; one is taken. The gate confirmed this section's placement argument
independently from source.

### 2.1 Candidate A — refuse at install time (REFUSED, and on a derivation)

Make `aira install` refuse to bake a slice whose cap is below
`DefaultConfineMemoryReserve` plus headroom, so the defect cannot exist.

Refused on three grounds, the third decisive:

1. It does not cover the population. A fixture slice, a shim-configured
   container budget (`shim.go:139-201`), a systemd drop-in that lowers
   `MemoryMax` after install, and a CI runner that never runs `aira install` at
   all are outside its reach, and three of those four are named in the ticket.
2. It converts a sizing defect into a product prohibition: "AIRA does not support
   slices below ~6 GiB" — removing a legitimate use case (a small CI box) to
   avoid sizing a number correctly. Note that `install.go`'s own default would
   then have to refuse to install on an 8 GiB box.
3. **It cannot establish the property it claims, because the ceiling is a
   function of concurrent occupancy, not of the install.** The entry ceiling is
   `maximum - (base + (jobs+1)*perJob)`; on an 8 GiB slice with production
   defaults, `jobs = 31` gives a ceiling of exactly 4294967296 and `jobs = 32`
   puts it below, so the identical unpinned request that was admissible at low
   occupancy is refused at high occupancy on a machine that passed every
   install-time check.

### 2.2 Candidate B — make the daemon publish the admissible size (REFUSED)

Have the daemon tell the client the ceiling and let the client re-resolve its own
reserve against it.

Refused:

- It relocates one sizing decision into a place with strictly less information
  and adds a round trip to do it.
- The number it would obtain is a **transient**: the entry ceiling moves by
  `perJob` for every job outstanding at arrival, so a client that fetched,
  decided and re-sent could still be refused.
- It is the AIRA-62 shape verbatim — a second copy of the reserve decision
  outside `ResolveConfineReserve`, whose doc comment (`confine.go:65-102`)
  records a 64× silent over-reservation caused by exactly this duplication.
- The cheap variant — have the CLIENT retry after an `E_ADMIT_TOO_LARGE` that
  already carries `cap_minus_headroom` — contradicts the shipped `too_large`
  contract the agent guide states (`skill.go:323`) and makes an automatic retry
  loop out of a terminal code.

### 2.3 Candidate C — bound the prior by the slice (TAKEN), daemon-side

Sited in `resolveAdmitReserve`, where the ceiling is already an argument. Why
daemon-side rather than in `ResolveConfineReserve`:

- The client cannot compute the ceiling. It can read a cgroup cap
  (`ConfineLaunchInfo.CapBytes`) but not `admitSliceHeadroom`'s configured base
  and per-job terms, not `admitOutstandingJobs`, and not the shim budget — and it
  reads that cap *after* admission is requested.
- A client-side fit would be a second, drifting definition of the ceiling:
  AIRA-62 again.
- It would have no effect where it appeared to: on the flock fallback path the
  unpinned reserve is deliberately **not** enforced as a cap at all
  (`confine_linux.go:971-977`).

Why it refines rather than reverses the rule AIRA-152's rejection rested on
("never size the next attempt below what an unpinned client asked for"): after
this change that rule reads "…**bounded by what the slice can hold**", which on
every slice where the old rule was satisfiable is the identical rule, and on the
slices where it was unsatisfiable replaces "refuse the job" with "give it the
largest prior the slice can grant". AIRA-152 stays superseded.

### 2.4 Rejected variants of the fit

Each checked against source and rejected for a stated reason.

- **Fit whenever `FIT(ceiling) < reserve` (revision 1's UNGATED form).**
  REFUSED — this is the gate's F3, and it is the most important rejection in
  revision 2. For `ceiling` in `(4294967296, 4939212390]` (production headroom,
  `j = 0`: a `MemoryMax` of 6.06–6.66 GiB, an ordinary small dev or CI box) the
  4 GiB default is admissible AND grantable today, with up to 614 MiB of slack,
  and the ungated fit would lower the job's kernel-enforced `memory.max` by up to
  560,213,125 B (13%) — so a job that peaks in `(FIT(c), 4 GiB]` succeeds today
  and is OOM-killed after. That is silent under-provisioning, and it violates
  this plan's own criterion for rejecting `min(hint, ceiling/2)` below. Revision
  1's R1 ("on those slices it got no cap and no run at all") is simply false over
  that band.
  **Taken instead: fit only when `reserve >= ceiling`** — exactly the requests
  that are refused (`>`) or land on the ungrantable ceiling (`==`, AIRA-150
  route 3) today. `>=` rather than `>` so an auto-sized prior can never sit
  exactly on the ceiling.
- **Fit to the ceiling itself (`min(hint, ceiling)`).** REFUSED: a reserve
  exactly equal to the entry ceiling is AIRA-150, granted only while
  `charge <= (effectiveMaximum - maximum) + perJob*(J_entry - J_now)`, which on a
  slice the request entered empty is byte-exact zero. It would reintroduce
  AIRA-150 systematically at a new site, one ticket after it was removed.
- **Fit to `ceiling - admitSliceHeadroom(1)`, or to `ceiling - perJob`.** Both
  express the margin in the codebase's own headroom vocabulary and both
  degenerate: `admitSliceHeadroomBase`/`Supervisor` are configurable and are set
  to **0** at 21 and 22 call sites respectively in `internal/daemon`'s own tests,
  and `subtractFloor` would then make the margin zero — the rejected variant
  above, reachable by configuration.
- **A fixed constant margin (`ceiling - 256 MiB`).** An arbitrary number; the
  simplicity rule refuses it, AIRA-151 §2.4 already refused "ceiling minus an
  arbitrary slack" for the clamp, and it scales wrongly in both directions.
- **Fit to `checkedAvailable` (the currently grantable figure).** REFUSED on the
  documented invariant at `admit.go:2413-2417` and `sliceceiling.go:27-33`: this
  value becomes a job's own kernel-enforced `memory.max`, and sizing containment
  from the slice's transient charge is what AIRA-103 forbids.
  `TestSliceCeilingDoesNotReachTheOOMEscalationClamp` exists to keep it
  forbidden.
- **Fit a FRACTION of the ceiling unconditionally (`min(hint, ceiling/2)`).**
  Refused for changing behaviour on slices where nothing is wrong.
- **Fit AFTER the resolution instead of before it.** Looks simpler and is WRONG,
  with a counterexample. Hint 4 GiB, ceiling 1031798784, `MaxOOMPeak = 671088640`
  (escalation 1006632960):
  - fitting BEFORE: the fitted prior is 897216333, the escalation 1006632960
    raises it, the escalation determined the value, and 1006632960 is under the
    ceiling so nothing clamps — the job is sized at what its own OOM evidence
    justifies, basis `estimate:oom-escalated`;
  - fitting AFTER: the escalation 1006632960 does *not* exceed the unfitted 4 GiB
    hint, so the hint wins, is fitted to 897216333, and the job is sized **below
    what its own OOM evidence says it needs**, under a fallback basis.
  Fitting before the comparison keeps `max(prior, escalation)` a genuine max of
  two candidates. Mutation M1 is exactly this row.

### 2.5 Why AIRA-151's clamp must be retargeted in the SAME change (F1 / Q5)

Revision 1 filed this as deferral G1 and described it as "bounded (one rung)".
That was wrong on the facts. The derivation, from source:

1. This change admits a small-slice job at a fitted `memory.max` of `FIT(c)`,
   which is `0.8696 c`.
2. A job OOM-killed at that cap records `MaxOOMPeak ≈ FIT(c)` — the peak comes
   from the scope's own `memory.peak` (`confine_linux.go:1363-1386`) and the
   store keeps `MAX(peak_rss) WHERE oom=1` (`confine_peak_history.go:58-64`).
3. `FIT(c)/c = 0.8696` lies inside AIRA-151's clamp band `(2/3, 1)` **by
   construction**. So the next admission computes
   `escalated = 1.5 × 0.8696c = 1.304c > c`, master's guard `MaxOOMPeak < c`
   holds, the clamp fires, and the reserve becomes **exactly the ceiling**.
4. A reserve exactly equal to the entry ceiling is AIRA-150: grantable only while
   `charge <= perJob*(J_entry - J_now)`, byte-exact zero on a slice entered
   empty. Verified for `c = 1031798784`, `2642411520` and `4253024256`: the
   resolved reserve is the ceiling in all three.
5. So the request waits out the **default 30-minute** admission window
   (`confine_linux.go:1616-1618`) and is refused `E_ADMIT_SATURATED`, whose
   documented meaning to an agent is "owed a RETRY, nothing about the request is
   wrong" (`skill.go:323`) — so the guide sends the agent straight back into the
   same wedge.
6. Because the job never runs, **no new peak is recorded**. The state is
   PERMANENT, not a rung.

Master's behaviour for that same class today is an immediate, honest
`too_large`. So shipping the fit alone would convert an honest refusal into
precisely the wedge-then-refuse outcome AIRA-151 shipped to remove, for the
population this ticket exists to serve. Any fit above `2c/3` wedges on the first
escalation; a fit below `2c/3` only adds one grantable rung before the same
wedge. **The wedge is structural in the clamp's target**, so it can only be
closed there.

The retarget is therefore not a second, separable idea: it is the same quantity,
applied at the site that consumes what the first site produces. `FIT(ceiling)` is
the largest value the slice can grant one job; the clamp's job is to cut an
over-ceiling escalation down to something the slice can give; cutting to the
ceiling never was that. AIRA-151's own rationale — "a wait that ends in a refusal
is worse than an honest refusal" — is what selects both halves of the retarget:
clamp to a grantable value, and refuse honestly when even that is below the
evidence.

**The cost, stated before the gate rather than discovered after it** (§0.1's
production band, R3): on the 64 GiB production slice, an OOM peak in
[41.29 GiB, 53.86 GiB) is now capped at 53.86 GiB instead of 61.94 GiB, and a
peak in [53.86 GiB, 61.94 GiB) becomes a terminal refusal instead of one
practically-ungrantable shot at the ceiling.

---

## 3. Design

### 3.1 One quantity, computed once, used at three sites

```go
func (s *Server) resolveAdmitReserve(request admitRequest, ceiling int64) (int64, string) {
	if request.pinned {
		return request.reserve, "pinned:client"
	}
	// AIRA-153. ONE quantity governs every auto-sized value this function can
	// place against `ceiling`: the largest reserve the slice can actually GRANT
	// one job. `ceiling` is only the largest ADMISSIBLE one -- the grant gate is
	// strictly tighter (AIRA-150) -- so anything AIRA sizes for itself is bounded
	// by `fit`, never by `ceiling`.
	//
	// Three sites, below: the client's unpinned prior, the machine-wide p90
	// prior, and AIRA-151's OOM-escalation clamp. `fit` is 0 when the slice is
	// too small for any viable reserve, and every site then leaves the value
	// alone so the existing terminal E_ADMIT_TOO_LARGE answers, naming both
	// numbers.
	fit := runner.SliceFittedReserve(ceiling)

	// SITE 1 -- the client's unpinned prior. runner.ResolveConfineReserve hands
	// the daemon a compiled-in constant with no relationship to this command or
	// to this slice, and that function is pure and portable precisely so the
	// reserve decision has ONE home (AIRA-62); the daemon is the only party that
	// knows the ceiling, so the bounding happens here.
	//
	// GATED on `>= ceiling`: only a prior that is REFUSED today (`>`), or that
	// lands exactly on the ungrantable ceiling (`==`, AIRA-150 route 3), is
	// touched. Where the default already fits and is already granted, this
	// function returns byte-for-byte what it returns today -- lowering a working
	// job's kernel-enforced memory.max by 13% would be silent under-provisioning
	// (plan §2.4).
	//
	// Applied BEFORE anything reads `request.reserve`, which is load-bearing in
	// two directions: all six sites that return the hint verbatim inherit it, and
	// the OOM escalation's `escalated > reserve` comparison sees the fitted value
	// as its FLOOR, so the escalation can still raise the reserve to whatever this
	// command's own OOM evidence justifies (§2.4's counterexample).
	// `request` is a value copy (see the signature), so this cannot escape.
	fitted := ""
	if fit > 0 && request.reserve >= ceiling {
		request.reserve, fitted = fit, ",ceiling-fitted"
	}
	...
```

Inside the history block the suffix is carried by PROVENANCE, never by comparing
numbers — AIRA-149's lesson is that a label must name the term that acted, and
two terms can coincide on a value:

```go
	estimated, estimateUsable, basis := runner.EstimateMemoryReserve(ordinary, 0)
	reserve := request.reserve
	suffix := fitted                       // the fitted PRIOR is what `reserve` holds
	if estimateUsable {
		reserve, suffix = estimated, ""    // this command's own evidence; never fitted
	}
	...
	oomBasis := basis + ",oom-on-record"
	if escalated > reserve {
		reserve = escalated
		oomBasis = "estimate:oom-escalated"      // evidence again; suffix is moot
		// SITE 3 -- AIRA-151's clamp, retargeted. See §3.3.
		if fit > 0 && stats.MaxOOMPeak < fit && reserve > ceiling {
			reserve = fit
			oomBasis += ",ceiling-clamped"
		}
		return reserve, oomBasis
	}
	return reserve, oomBasis + suffix
	...
	if stats.SampleCount >= 3 && reserve > 0 {
		return reserve, basis + suffix
	}
```

Site 2 is the same rule applied to the other prior, with the same gate:

```go
	if peak, ok := s.cachedAdmitPeakP90(readCtx); ok {
		stats := runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: peak}
		if reserve, usable, _ := runner.EstimateMemoryReserve(stats, 0); usable {
			// AIRA-153 SITE 2. The p90 is a PRIOR about commands other than this
			// one -- it is consulted precisely because this signature has no
			// history -- so it is bounded exactly as the client's default is, and
			// only where it would otherwise be refused or land on the ceiling.
			// Without this, AIRA-128's cold start (which the guide teaches as
			// `estimate:p90-prior`) is still terminally refused on any slice whose
			// ceiling is below the box's p90 -- the ordinary shape of a small
			// ci-shim budget beside a large aira.slice, since one machine-wide
			// state.db serves both.
			if fit > 0 && reserve >= ceiling {
				return fit, "estimate:p90-prior,ceiling-fitted"
			}
			return reserve, "estimate:p90-prior"
		}
	}
```

and the four post-block fallbacks each gain the suffix on their existing return:

```go
	return request.reserve, "fallback:no-signature" + fitted
	return request.reserve, "fallback:history-unavailable" + fitted
	return request.reserve, "fallback:insufficient-samples" + fitted
	return request.reserve, "fallback:no-history" + fitted
```

Nothing else in the function moves: the estimator call, `historyUnavailable`,
`insufficientSamples`, the escalation and its `math.MaxInt64` guard, AIRA-149's
`,oom-on-record` and D4's kept estimator basis, the clamp's NESTING inside the
escalation branch, its `reserve > ceiling` condition, and the pinned return are
byte-for-byte identical.

### 3.2 `SliceFittedReserve` — the quantity, and why it is that one

It lives in `internal/runner/resource_estimate.go`, beside the constant it
inverts and beside `MinPinnedScopeCap`, because that file already owns reserve
SIZING and `internal/daemon` already imports it. The daemon owns the ceiling and
supplies it; the runner owns the rule. That is the existing downward layering,
not a new seam.

```go
// SliceFittedReserve is the largest reserve a slice whose admission ceiling is
// `ceiling` can actually GRANT one job, and it is the ONE quantity AIRA-153 uses
// wherever the daemon sizes a value for itself against that ceiling.
//
// The quantity is the INVERSE of the estimator's own growth margin: the largest
// figure that, grown by memoryEstimateSafetyPct the way every history-derived
// estimate here is, still fits inside `ceiling`. So no new constant enters the
// codebase, and the number means something already meant elsewhere.
//
// It must be strictly BELOW the ceiling and it must never degenerate to zero,
// for one reason each:
//   - the ceiling is the largest ADMISSIBLE reserve, not the largest GRANTABLE
//     one. A reserve equal to it is granted only while the slice's own charge
//     sits inside a narrow residual band (AIRA-150), which on a slice the
//     request entered empty is byte-exact zero. Sizing TO the ceiling -- which
//     is what the OOM clamp used to do -- is the wedge, not the fix.
//   - a margin expressed in the configurable headroom terms is zero whenever
//     those are configured to zero, which is a margin only by luck.
// A proportional margin has neither failure: it is ~13.04% of the ceiling on a
// 1 GiB fixture slice and on a 64 GiB production slice alike.
//
// Returns 0 -- "this slice cannot grant a viable reserve at all" -- when:
//   - `ceiling <= 0`, which is what subtractFloor yields for a slice smaller
//     than its own headroom;
//   - the fitted value would be below MinPinnedScopeCap. That is the DEGENERATE
//     case, and the honest answer there is the existing terminal
//     E_ADMIT_TOO_LARGE naming `required` and `cap_minus_headroom`, not a
//     sub-megabyte memory.max that would instant-OOM the job on placement --
//     the exact hazard workerAdmitEstimatedBytesMin was minted for.
// Callers apply their own trigger (plan §3.1): a PRIOR is fitted only when it is
// at or over the ceiling; the OOM clamp fires only where AIRA-151 already said
// it should.
func SliceFittedReserve(ceiling int64) int64 {
	if ceiling <= 0 {
		return 0
	}
	// Overflow-free exact floor of ceiling*100/(100+pct): writing it as one
	// multiply would overflow int64 above ~92 PiB of ceiling, and a wrapped
	// negative here would silently size every value to nonsense.
	const scale = 100 + memoryEstimateSafetyPct
	fitted := ceiling/scale*100 + ceiling%scale*100/scale
	if fitted < MinPinnedScopeCap {
		return 0
	}
	return fitted
}
```

The identity `c = 115q + r ⇒ ⌊100c/115⌋ = 100q + ⌊100r/115⌋` is exact, and the
two forms were checked to agree on every figure in §0.1, including
`math.MaxInt64` where the naive form yields 0.

### 3.3 The clamp, retargeted — the exact delta to AIRA-151

Master (`admit.go:1716-1719`):

```go
	if stats.MaxOOMPeak < ceiling && reserve > ceiling {
		reserve = ceiling
		oomBasis += ",ceiling-clamped"
	}
```

After:

```go
	if fit > 0 && stats.MaxOOMPeak < fit && reserve > ceiling {
		reserve = fit
		oomBasis += ",ceiling-clamped"
	}
```

Three properties, each stated so it can be checked:

- **The condition to ENTER is unchanged.** `reserve > ceiling` still selects
  "the escalation's own value exceeds what this slice may admit". An escalation
  that lands in `(fit, ceiling]` is left exactly as it is: it is this command's
  own evidence, it is admissible today, and lowering it would be the same class
  of silent under-provisioning §2.4 refuses for the prior.
- **The GUARD moves from `ceiling` to `fit`, and that is the wedge fix.** Its
  meaning is unchanged in words — "an OOM already observed at or above what this
  slice can give is genuinely too large, so do not pretend otherwise" — but the
  quantity it compares against is now the one the slice can actually grant. A job
  OOM-killed at a fitted cap records `MaxOOMPeak ≈ fit`, fails the guard, and is
  refused immediately with both numbers instead of being parked on an
  ungrantable ceiling for 30 minutes.
- **The TARGET moves from `ceiling` to `fit`, and that is what makes the clamp
  keep its own promise.** AIRA-151 kept the clamp so "earlier censored caps are
  allowed to climb … so a runnable job is never permanently wedged"; a value
  equal to the entry ceiling is not one such a job can be granted. Every clamped
  value is now simultaneously **strictly above the OOM peak that produced it**
  (the guard) and **strictly below the ceiling by ~13%** (the quantity), so the
  rung is a real one.

The basis token stays `,ceiling-clamped`: it means "the slice's admission ceiling
cut this value down", which is still exactly what happened. The guide sentence
that describes the resulting number is updated in §3.7.

### 3.4 The degenerate case, and what still refuses

`SliceFittedReserve` returns 0 below `runner.MinPinnedScopeCap` (1 MiB), the
codebase's own boundary for "a reserve that can be a real cap", already enforced
at the runner boundary for a declared `--memory-reserve` and already mirrored by
`workerAdmitEstimatedBytesMin` with the reason spelled out in source. There, no
site fires and `admit.go:1929` refuses terminally exactly as today.

After this change a request is terminally refused when, and only when:

1. the value is a per-signature ESTIMATE above the ceiling (E1) — this command's
   own measured evidence says it needs more than the slice has;
2. the value is an OOM ESCALATION above the ceiling whose own recorded peak is at
   or above `FIT(ceiling)` (E2) — the job has already been killed at, or above,
   the largest cap this slice can grant;
3. the request is PINNED above the ceiling — the operator's own number;
4. the slice is so small that `FIT(ceiling) < 1 MiB`, i.e. `ceiling <= 1205862`
   bytes (`FIT(1205862) = 1048575`, one byte under `MinPinnedScopeCap`, against
   `FIT(1205863) = 1048576` which fires), or `ceiling == 0`.

Case 2 is new and is the honest replacement for the wedge derived in §2.5.

Named rather than buried: there is a band above the degenerate floor —
`FIT(ceiling)` between 1 MiB and whatever the job actually needs — in which a job
is now admitted and will certainly be OOM-killed. That is not a silent
under-provision: the kill is attributed to the signature, `terminated-by=oom` is
reported, `cap-source=auto:daemon-reserve` tells the agent AIRA chose the number,
and the next admission either escalates within the slice or refuses terminally by
case 2. The ladder terminates; it does not wedge. G1 records the one bounded
residual (§8).

### 3.5 The basis token

The value returned changes for the affected population, so the basis must say so
— AIRA-149's own rule, *reserve-basis names the provenance of the number actually
returned*.

`,ceiling-fitted` is a new token in the existing grammar (`family:name[:params]`
with comma-separated params, no spaces — the trailer's `reserve-basis=` field is
space-delimited, `confine.go:897-900`). Two structural properties, asserted
rather than argued:

- **It appears only on a PRIOR.** It is carried by `suffix`, which is cleared the
  moment `estimateUsable` is true and is unreachable from the escalation branch,
  which rebuilds `oomBasis` from scratch. So it can never attach to
  `estimate:max=…`, `estimate:capped`, or `estimate:oom-escalated`.
- **It and `,ceiling-clamped` are mutually exclusive by construction.** The two
  partition cleanly: *clamped* = the escalation's own value was cut down to what
  the slice can grant; *fitted* = a prior was sized to the slice before anything
  else looked at it.

The full set of newly-producible basis strings, which §7's gauge pin enumerates:

```
fallback:no-signature,ceiling-fitted
fallback:history-unavailable,ceiling-fitted
fallback:insufficient-samples,ceiling-fitted
fallback:no-history,ceiling-fitted
fallback:insufficient-samples:n=<N>,oom-on-record,ceiling-fitted
fallback:capture-unavailable,oom-on-record,ceiling-fitted
fallback:malformed,oom-on-record,ceiling-fitted
fallback:malformed,ceiling-fitted          (the SampleCount>=3 !ok return)
estimate:p90-prior,ceiling-fitted
```

`estimate:oom-escalated,ceiling-clamped` is unchanged in spelling; only the
number under it moves.

### 3.6 What the affected request now receives

- an admission, at a reserve the slice can grant, with the grant gate untouched;
- a scope `memory.max` equal to that reserve, `cap-source=auto:daemon-reserve`
  (`confine_linux.go:978-981`) — so the booking and the kernel-enforced bound are
  the same number, which is why lowering it cannot over-commit the slice (I3);
- a trailer whose `reserve-basis=` names both the term and the fit;
- if the job genuinely needs more, an OOM attributed to its signature and a
  higher next admission — AIRA-128's ladder, working on a slice where it was
  unreachable — terminating in an immediate, honest `E_ADMIT_TOO_LARGE` once its
  recorded peak reaches what the slice can grant.

### 3.7 The agent guide

`internal/core/skill.go:324` currently ends AIRA-151's clause with:

> … On a slice that small the RE-RUN advice below does not self-heal, and
> retrying the command unchanged cannot help.

That sentence becomes false, in the direction that tells an agent to give up on a
case that now works. But its predecessor sentence — "refused immediately with
`E_ADMIT_TOO_LARGE` naming `required` and `cap_minus_headroom` instead of
waiting: pin `--memory-reserve` (or `--memory-max`) at or below the printed
`cap_minus_headroom`" — stays TRUE and stays pinned; only its population
narrows.

The replacement, in the same position, says four things, and the fourth is the
carve-out the gate required (F2) — the unqualified "RE-RUN becomes true again on
a small slice" must not ship:

- an unpinned reserve is a PRIOR and is fitted to the slice it is admitted into
  whenever it would otherwise not fit; `,ceiling-fitted` in the basis means
  exactly that and is not an error — the job ran, at the largest prior this slice
  can grant;
- `,ceiling-fitted` and `,ceiling-clamped` are different facts: *fitted* sizes a
  guess to the slice before the job runs, *clamped* cuts the OOM escalation's own
  value down to the same figure;
- on a small slice, RE-RUN self-heals **when the job fits under that cap**: the
  first run is capped at the largest prior the slice can grant, the OOM is
  attributed, and the next identical invocation is admitted at the escalated
  value;
- **a job OOM-killed AT that cap is refused on re-run** with
  `E_ADMIT_TOO_LARGE` naming both numbers, because its own measured peak is
  already at what the slice can give. That is not a retry case: pin at or below
  the printed `cap_minus_headroom`, or run where the slice is larger.

`skill_test.go`'s pin gains `,ceiling-fitted` and a phrase from the fourth bullet,
and keeps every phrase it pins today (§7.2/U4).

### 3.8 What this does NOT close

- **AIRA-150 stays open, narrowed further.** Its three routes onto a reserve
  exactly equal to the entry ceiling: route 1 (the OOM clamp) becomes
  **unreachable** — the clamp now targets `FIT(ceiling)`; route 3's *client
  default* half becomes unreachable — a prior at or over the ceiling is fitted
  strictly below it. What remains is route 2 (a PINNED `--memory-reserve` /
  `--memory-max` exactly equal to the ceiling) and route 3's *estimate* half (an
  ordinary per-signature estimate that happens to equal the ceiling exactly).
  Both are values AIRA did not size for itself, or sized from this command's own
  measurement; neither is systematic. **The claim is "no auto-sized PRIOR and no
  CLAMPED value can land on the ceiling", not "nothing can"** — and U9's re-based
  fixture drives route 3's estimate half deliberately, which is the executable
  evidence that it remains.
- **AIRA-151's rule stands; its target and guard change.** The clamp is still
  nested inside the escalation branch, still governed by one condition for both
  the value and the label, still strict on the tie-break, and still uses the
  STATIC ceiling (AIRA-103). What changes is the quantity, and this plan owns
  that change — its production effect (§0.1), its invariants (§4), its tests
  (§7.2), and its ticket note (U6).
- **The `E_ADMIT_TOO_LARGE` message** still prints raw bytes and names no escape
  hatch. That is AIRA-151's filed G3; this ticket changes its population without
  touching its wording, and §7.3 keeps its shipped pins green.

---

## 4. Invariants

**I1 — pinned requests are byte-identical.** `--memory-reserve`, `--memory-max`
and `--delegate-ram` all return at `admit.go:1609-1611` before either site
exists. Structural, pinned by T7 and the unedited
`confine_reserve_resolution_linux_test.go` table.

**I2 — an ORDINARY estimate is never fitted, at either site.** It reaches the
ceiling boundary unchanged, so an over-ceiling estimate is still refused
terminally with both numbers. Enforced by `suffix` being cleared on
`estimateUsable` and by the clamp living inside the escalation branch; pinned by
T3's negative direction and by the re-based U1 wire test.

**I3 — no request is ever granted a LARGER reserve than today, and the slice
cannot be over-committed.** Three halves: *the fit only lowers* (`FIT(c) < c <=
reserve` whenever it fires); *the clamp only lowers* (`fit < ceiling < reserve`
whenever it fires, and where it no longer fires the value is refused rather than
granted); *containment* — the resolved reserve is simultaneously the ledger
booking and the job's own kernel-enforced `memory.max`
(`confine_linux.go:978-981`), so a smaller booking is matched by a smaller hard
cap and cannot under-book what the job can take. Pinned by T4.

**I4 — the fit is a strict no-op wherever the prior already fits under the
ceiling.** The gate is `reserve >= ceiling`, so for the 4 GiB default every
slice with `ceiling >= 4294967297` is untouched — production, the AIRA-128
fixture, and the two 8 GiB facet-2b declarations — by construction, not by
measurement. Pinned by T1's and T2's boundary rows and §7.3's list.

**I5 — every auto-sized value is STRICTLY below the ceiling.** `FIT(c) < c` for
all `c >= 1`, so neither a fitted prior nor a clamped escalation can produce
AIRA-150's ungrantable equality. Pinned by T1, T5's grant assertion and U7.

**I6 — every CLAMPED value is strictly ABOVE the OOM peak that produced it.**
The guard is `MaxOOMPeak < fit` and the target is `fit`, so a clamped rung is
always a genuine increase over the last kill. Master guaranteed this too
(`MaxOOMPeak < ceiling`, target `ceiling`); revision 2 keeps it while making the
rung grantable. Pinned by T11's rows.

**I7 — a degenerately small slice still refuses terminally.** Below
`MinPinnedScopeCap` the helper returns 0 and no site fires, so `admit.go:1929`
answers as today with `required` and `cap_minus_headroom` populated. Pinned by
T1's floor rows and T8.

**I8 — the basis names the fit whenever the fit determined the value, and never
otherwise**, carried by provenance rather than a value comparison;
`,ceiling-fitted` and `,ceiling-clamped` are mutually exclusive by construction.
Pinned by T2 and T3.

**I9 — `,oom-on-record` is unaffected**, so AIRA-128's attribution proof
(AIRA-149 §3.2) is untouched: the token is appended in the same place and the
suffix follows it rather than replacing it.

**I10 — the AIRA-52 gauge's evaluable population does not move.** Every
newly-producible basis is either a `fallback:` string (never selected by the
`estimate%` predicate) or `estimate:p90-prior,ceiling-fitted`, which fails the
`$`-anchored matchers exactly as `estimate:p90-prior` already does and is counted
`malformed_basis` + excluded. `estimate:oom-escalated,ceiling-clamped` is
unchanged in spelling. No published adequacy number changes. Pinned by U5.

**I11 — no auto-sized value can wedge on the ceiling.** A prior at or over the
ceiling is fitted strictly below it; a clamped escalation targets `fit`, strictly
below it. The two systematic routes onto AIRA-150's ungrantable equality are
gone. Pinned by T5, T11 and T12; the surviving non-systematic routes are named in
§3.8 and driven by U9.

**I12 — no new state, no new wire field, no protocol change, no CLI flag, no
MCP/Skill dispatch change.** One pure exported helper, one local, one suffix, one
retargeted condition, one guide paragraph.

---

## 5. Risks

**R1 — this is a sizing change on the machine-wide gate, and it lowers numbers.**
Two populations (§1.4), both downward only (I3), and in both the lowered number
is simultaneously the job's hard cap, so the slice cannot be over-committed.
- *For the fit*, the residual is nil by construction: it fires only where the
  prior is refused (`>`) or lands on the ungrantable ceiling (`==`) today, so no
  request that runs today is sized differently. This is the correction of
  revision 1's R1, which claimed that of a wider band than was true (F3).
- *For the clamp*, the residual is real and is R3.

**R2 — a job admitted at a fitted prior may OOM where today it was refused.**
True, and it is the intended trade: a refusal is not a safer outcome than an
attributed OOM, because the OOM feeds the ladder and the refusal does not. The
honesty machinery exists and is unchanged — `terminated-by=oom`, exit 137, the
OOM advisory, and `cap-source=auto:daemon-reserve` telling the agent a re-run CAN
change the number (`skill.go:325`).

**R3 — the clamp retarget changes production behaviour on the 64 GiB slice.**
Precisely (§0.1): an OOM peak in [41.29 GiB, 53.86 GiB) is clamped to 53.86 GiB
instead of 61.94 GiB — 13% lower, and grantable where the old value was
grantable only inside AIRA-150's residual band; and a peak in
[53.86 GiB, 61.94 GiB) is now refused terminally instead of being admitted at the
ceiling. **Accepted, on AIRA-151's own rationale**: a wait that ends in a refusal
is worse than an honest refusal, and a job whose last kill was at 54–62 GiB on a
62 GiB ceiling is genuinely too large for this slice. The refusal names
`required` and `cap_minus_headroom`, so the operator gets a number to pin at. The
band is narrow, it requires a real multi-tens-of-GiB OOM to reach, and nothing in
the shipped test suite drives it (which is itself why T11 is added).

**R4 — the ladder can still spend one extra attempt at the top rung.** If a real
OOM kill records a peak a page or two BELOW `FIT(ceiling)` rather than at it, the
next admission clamps to exactly `FIT(ceiling)` — the largest cap the slice can
grant, strictly above that peak — and the job RUNS again rather than waiting: it
either fits or is killed again, and a kill at that cap records a peak at or above
`FIT(ceiling)`, which fails the guard and refuses terminally. So the worst case
is one extra real run, never a wait on an ungrantable value. Bounded, honest, and
recorded as G1 rather than hidden.

**R5 — ci-shim mode: a fitted booking with no kernel enforcement.** In shim mode
there is no cgroup and no `memory.max`
(`containment=advisory(ci-shim,no-cgroup,no-kill-backstop)`), so the fitted
reserve is a ledger booking nothing enforces. That is already true of every shim
booking and is the mode's documented limit; what changes is that a small
container's jobs are admitted at all rather than answering `E_ADMIT_TOO_LARGE`
for the container's whole life (AIRA-151 R5 recorded that outcome and explicitly
did not remove it). The booking can never exceed the container's own budget minus
headroom. Accepted; recorded as G3.

**R6 — the ledger and the AIRA-52 gauge.** Runs that used to end
`reject:too-large` now run and record a `fallback:…,ceiling-fitted` or
`estimate:p90-prior,ceiling-fitted` basis. No existing basis changes spelling and
no classifier changes, so no published number moves except by rows appearing.
I10; pinned by U5.

**R7 — AIRA-151's own tests are built on shapes this makes unreachable or
re-values.** Four families move, each with its reason and each re-based rather
than deleted (§7.2): the over-ceiling CLIENT DEFAULT wire test (U1), the two
row-(e) table entries plus the tie row (U2/U3), the clamp's own value in
`confine_admit_test.go` and `sliceceiling_test.go` (U7/U8), and AIRA-149's
facet-2b fixture (U9). AIRA-151's *claim* is not weakened: §7.2 shows each
re-based test still goes RED under a mutation that restores master's clamp
placement, and the re-basing is itself the executable evidence that the
systematic routes are gone.

**R8 — the tie-break row loses its sizing dimension unless it is re-based.**
AIRA-151 §3.2 promoted `escalated > reserve` from a labelling rule to a sizing
rule and pinned it with a row where the escalation exactly equals the CLIENT
DEFAULT over the ceiling — a shape the fit removes. U2 re-bases it onto an exact
tie between the escalation and an ORDINARY ESTIMATE (never fitted):
`PeakMax = 2801065628 → estimate 3221225472`; `MaxOOMPeak = 2147483648 →
escalation 3221225472`; ceiling 2684354560. Both figures were recomputed. The
pin survives intact and still goes RED under the `>=` mutation (M7).

**R9 — a consumer that assumes an unpinned resolution equals the client's
request.** Enumerated from source: the only production consumer of the resolved
value is `admitConnection` (which compares it to the ceiling on the next line)
and the waiter it builds; the basis consumers are the confine trailer,
`Status.ReserveBasis`, `ContainerAdvisories`, the `admission_reserve_basis`
ledger column and the AIRA-52 gauge — all display-or-classify, none parsing
`ceiling-clamped` today or `ceiling-fitted` after. `admit.go:2115` already builds
the waiter from the RESOLVED reserve.

**R10 — this looks small and is not.** The production diff is one helper, one
local, six suffixed returns, one retargeted condition and one guide paragraph,
and it moves two sizing decisions on the machine-wide gate. The mitigation is
procedural and is this document: full two-loop, the populations in §1.4, the
production effect in §0.1, the costs in R3/R4, and the deferrals in §8.

---

## 6. Expected yield

- **A slice at or below ~6 GiB becomes usable for unpinned confine jobs again** —
  including a **default install on an ordinary 8 GiB box**, where today every
  unpinned `aira confine` is refused (§1.2).
- **AIRA-128's cold-start self-heal is restored for that whole class**, the
  property AIRA-151 recorded as broken and deferred here. Proven at the kernel by
  a real-cgroup fixture (T9), not only in unit arithmetic.
- **`aira confine` in a small ci-shim container starts working**, closing the
  outcome AIRA-151 R5 named and left alone.
- **The OOM clamp stops producing a value the slice cannot grant.** AIRA-150's
  route 1 is removed outright, and the 30-minute
  admitted-then-saturated dead end for a job at the top of the ladder is replaced
  by an immediate, correctly-labelled `too_large`.
- One coherent rule replaces an unconditioned constant and an unusable clamp
  target: *the largest reserve this slice can grant* — one quantity, three sites,
  visible in the basis.
- Net new production state: **none**. One pure exported function and one string
  suffix.

---

## 7. Tests

TDD: each new test is written and observed RED against `a836a90` before the code
exists, except where it is explicitly a pin — stated as such and never presented
as a demonstration (AIRA-149's honesty note on its T13 is the precedent).

### 7.1 New

| Id | Name / file | Asserts | RED against master because |
| --- | --- | --- | --- |
| **T1** | `TestSliceFittedReserve` — new `internal/runner/resource_estimate_fit_test.go` | the pure quantity, as a table: the §0.1 figures (`1031798784→897216333`, `4227858432→3676398636`, `4253024256→3698281961`, `3179282432→2764593419`, `629145600→547083130`, `6400507904→5565659046`, `59055800320→51352869843`, `66504884224→57830334107`, `107374182400→93368854260`); `0` for `ceiling <= 0` and for a negative ceiling; the **degenerate floor** — `1205863 → 1048576` (== `MinPinnedScopeCap`) vs `1205862 → 0`; **strictly below** — `fit < ceiling` on every row; and **overflow** — `math.MaxInt64` returns `8020323510308500701`, which the naive `ceiling*100/115` cannot | the function does not exist |
| **T2** | `TestEveryUnpinnedPriorAtOrOverTheCeilingIsFittedAndSaysSo` — new `internal/daemon/admit_ceiling_fitted_test.go` | all seven prior routes at ceiling `1031798784` with the 4 GiB hint: `fallback:no-signature`, `fallback:history-unavailable`, `fallback:insufficient-samples`, `fallback:no-history`, the `SampleCount>=3` `!ok` return (`fallback:malformed,ceiling-fitted`), the OOM branch row (d)/(e) (`fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted`), and `estimate:p90-prior,ceiling-fitted` (p90 PEAK = 4 GiB, whose prior is that peak's own ESTIMATE, `4939212390`) — each asserting the VALUE `897216333` **and** the exact basis. Plus the **GATE** rows, which are the F3 fix, each taken at that route's OWN prior because the two priors are different numbers: for the six client-hint routes, ceiling `4294967297` returns `4294967296` with an UNSUFFIXED basis and ceiling `4294967296` (the `>=` boundary) fits to `3734754170`; for the p90 route, ceiling `4939212391` returns `4939212390` / `estimate:p90-prior` unsuffixed, and ceiling `4939212390` fits to `4294967295` / `estimate:p90-prior,ceiling-fitted` | master returns each unfitted prior unsuffixed at the small ceiling |
| **T3** | `TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped` — same file | the provenance property in both directions: (i) a usable ORDINARY estimate over a small ceiling resolves to the estimate with **no** `,ceiling-fitted`; (ii) an escalation-determined value carries `estimate:oom-escalated[,ceiling-clamped]` and **no** `,ceiling-fitted`; (iii) across a grid of stats × ceilings, no basis contains both tokens; (iv) the table-wide `^\S+$` basis assertion AIRA-149 carries | master emits neither token, and (i)/(ii) are the mutation surface for "fit the estimate too" |
| **T4** | `TestResolutionIsMonotoneInTheClientHintAndNeverRaisesIt` — same file | I3, without a test seam: over a grid of stats shapes × ceilings × ascending hints, `resolveAdmitReserve` never returns a LARGER value for a SMALLER hint, and never returns more than master would for the same inputs (computed inline from the shipped formula). **Stated honestly as a property pin, not a demonstration** | GREEN by construction — a pin on I3 |
| **T5** | `TestSmallSliceUnpinnedRequestIsAdmittedAndGrantedInsteadOfRefused` — new `internal/daemon/admit_ceiling_fitted_wire_test.go`, driven through the real `admitConnection` over `net.Pipe` | §1.2's shape end to end at the 1 GiB fixture ceiling: `current = 4096`, the measured one-sample OOM history, unpinned 4 GiB → **a waiter IS enqueued** (the negative of AIRA-151's own "no waiter" assertion), one evaluator pass **grants** it, and the grant carries `Reserve == 897216333` and `Basis == "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"` | master answers an immediate `E_ADMIT_TOO_LARGE` and never enqueues |
| **T6** | `TestDefaultInstallCeilingAdmitsANovelUnpinnedCommand` — same file | §1.2 exactly: `maximum = 6 GiB`, PRODUCTION headroom (`2 GiB + 64 MiB`, not the fixture's), no history and no p90 at all → ceiling `4227858432`, granted at `3676398636` with `fallback:no-history,ceiling-fitted`. **This is the ordinary-machine case the ticket exists for**, and it never touches the OOM branch | master answers `E_ADMIT_TOO_LARGE` |
| **T7** | `TestFittingNeverTouchesAPinnedRequest` — `admit_ceiling_fitted_test.go` | a pinned request at 4 GiB against a 1 GiB ceiling returns `4294967296` / `pinned:client` verbatim and is then refused by the ceiling boundary; driven for a declared reserve, a `--memory-max`-derived reserve and a `--delegate-ram` overhead | **GREEN by construction — a pin on I1.** Its RED direction is a fit applied before the pinned return, which would silently resize an operator's own number |
| **T8** | `TestASliceTooSmallForAnyViableReserveStillRefusesTerminally` — the wire file | I7: `maximum` chosen so the ceiling is `1205862` (fit would be `1048575`, one byte below `MinPinnedScopeCap`) → an immediate `E_ADMIT_TOO_LARGE` carrying `required == 4294967296` and `cap_minus_headroom == 1205862`, **no waiter enqueued**; plus the `ceiling == 0` arm | **GREEN by construction — a pin on I7.** Its RED direction is a fit with no floor, which would hand a job a sub-megabyte `memory.max` and instant-OOM it on placement |
| **T9** | `TestSmallSliceOOMSelfHealConvergesOnTheFittedBaseline` — new `internal/daemon/confine_selfheal_small_slice_real_cgroup_linux_test.go`, reusing `TestConfineOOMSelfHealWorkload` and the AIRA-128 phase structure | **the ticket's headline property, at the kernel.** Fixture slice `runner.DefaultConfineMemoryReserve - (1 << 30)` = 3 GiB (derived, not picked — see T10), ceiling `3179282432`, fit `2764593419`: phase 1 seeds three real runs to establish a p90; phase 2's cold start is capped at that prior and is OOM-killed (`terminated-by=oom`, exit 137, no completion marker); phase 3 re-runs the IDENTICAL argv with no operator action and **succeeds**, at `ReserveBasis == "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"`, `ScopeMemoryMax == 2764593419`, and the completion marker present | master refuses phase 3 with `E_ADMIT_TOO_LARGE` and `confine: ran=no`, so the marker never appears — the exact self-heal AIRA-151 recorded as broken |
| **T10** | `TestSmallSliceSelfHealFixtureActuallyExercisesTheFitAndStaysOffTheCeiling` — same file, unit, no cgroup | T9's fixture invariants, the way `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` guards AIRA-128's, with **both margins named in the assertion message** (F7): (i) `ceiling <= runner.DefaultConfineMemoryReserve` so the fit actually fires, with `4294967296 - 3179282432 = 1115684864` (1.04 GiB) of drift margin; (ii) `ceiling - fit = 414689013` (395.5 MiB) `>= oomSelfHealTargetBytes` (335544320, 320 MiB), leaving **75.5 MiB** of slack, so admission can never come to depend on the slice's residual charge — the AIRA-139 flake in this fixture's terms; (iii) the phase-3 basis carries `,ceiling-fitted` | the basis does not exist on master |
| **T11** | `TestTheOOMClampTargetsWhatTheSliceCanGrantNotTheCeiling` — `admit_ceiling_fitted_test.go` | **the retarget's boundary, the dominant new shape (F4).** At ceilings `1031798784` and `4227858432`, three rows each: `MaxOOMPeak == FIT(ceiling)` and `== FIT(ceiling)+1` resolve to the UNCLAMPED escalation (`1345824499` / `1345824501`, and `5514597954` / `5514597955`) with basis `estimate:oom-escalated` — i.e. over the ceiling, terminal; `MaxOOMPeak == FIT(ceiling)-1` clamps to exactly `FIT(ceiling)` with `estimate:oom-escalated,ceiling-clamped`. Plus I6 across the grid: a clamped value is always `> MaxOOMPeak` and always `< ceiling`. Plus the production band's two edges from §0.1 | master clamps every one of those rows to the ceiling |
| **T12** | `TestAnOOMAtTheFittedCapIsRefusedImmediatelyAndNeverEnqueues` — the wire file | the same shape through the real `admitConnection`: 1 GiB fixture slice, `MaxOOMPeak == 897216333` → `E_ADMIT_TOO_LARGE` with `required == 1345824499` and `cap_minus_headroom == 1031798784`, **no waiter enqueued and no 30-minute wait**. This is the executable form of §2.5's derivation | master enqueues it at the ceiling, where it is never granted at `current = 4096` |
| **T13** | `TestSmallSliceOOMAboveTheFittedCapIsTerminalAtTheKernel` — same real-cgroup file as T9 | **the terminal refusal proven at the kernel, not only in unit arithmetic (F4).** A 640 MiB fixture slice (ceiling `629145600`, fit `547083130`): phase A runs the workload with a PINNED `--memory-max` of `608174080` (580 MiB — deliberately above the fit by 58.3 MiB and below the ceiling by 20 MiB) against a 700 MiB workload, so the kernel really kills it and `RecordConfinePeak` really records `MaxOOMPeak ≈ 608174080 > fit`; phase B re-runs the IDENTICAL argv UNPINNED and must be refused `E_ADMIT_TOO_LARGE` (`confine: ran=no`, `admission=too_large`) **within a second**, never `saturated`, never a wait | master clamps phase B to the ceiling, enqueues it, and answers `E_ADMIT_SATURATED` after the whole wait |

### 7.2 Updates forced by the change

| Id | What | Why |
| --- | --- | --- |
| **U1** | `internal/daemon/admit_oom_clamp_wire_test.go` — `TestOverCeilingUnescalatedReserveIsRefusedTerminallyInsteadOfClamped` re-based from the client default onto an ORDINARY ESTIMATE. New fixture: the same 1 GiB slice and ceiling `1031798784`, stats `{TotalCount:5, SampleCount:5, PeakMax:2147483648, OOMCount:1, MaxOOMPeak:56360960}` → estimate `2469606195`, escalation `84541440` (loses) → `required == 2469606195`, `basis == "estimate:max=2147483648,n=5,f=115,oom-on-record"`, immediate `E_ADMIT_TOO_LARGE`, **no waiter enqueued** | R7. AIRA-151's claim is unchanged and still tested end to end; only its route to an over-ceiling value moves, because a prior can no longer be one. The comment must say that, and that the move is itself the evidence this ticket removed the route |
| **U2** | `internal/daemon/admit_oom_clamp_scope_test.go` — four rows. `b` → `51352869843` (the clamp now targets the fit; basis unchanged); `e-default` → `897216333` / `fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted`; `e-malformed` → `93368854260` / `fallback:malformed,oom-on-record,ceiling-fitted`; `tie` re-based per R8 to `{TotalCount:5, SampleCount:5, PeakMax:2801065628, OOMCount:1, MaxOOMPeak:2147483648}` at ceiling `2684354560` → `3221225472` / `estimate:max=2801065628,n=5,f=115,oom-on-record`. Rows `a` (`MaxOOMPeak 10 GiB` ≥ `FIT(8 GiB) = 7469508340`, so still unguarded and still terminal) and `c-prime` (ceiling 44 GiB, prior 4 GiB below it, so no fit) are **untouched** | R7, R8. Every figure recomputed: `2801065628 + 2801065628*15/100 = 3221225472` and `2147483648 + 2147483648/2 = 3221225472` — an exact tie, verified |
| **U3** | `internal/daemon/admit_oom_basis_test.go` — the two row-(e) entries of `TestOOMEscalationBasisNamesTheTermThatDeterminedTheReserve` take U2's new values. Every other row of that table, and all of `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes`, `TestResolveAdmitReserveKeepsTheEstimatorsOwnFallbackBasis` and `TestPostBlockInsufficientSamplesFallbackIsUnchanged`, is unchanged (their ceilings are 60 GiB and 100 GiB with priors below them, and no clamp is entered) | the same two rows appear in two tables; that duplication is pre-existing |
| **U4** | `internal/core/skill.go:324` (§3.7) and `internal/core/skill_test.go`'s pin. The AIRA-151 sentence about `E_ADMIT_TOO_LARGE` and `cap_minus_headroom` is KEPT and stays pinned (its `why` is updated); the "On a slice that small the RE-RUN advice below does not self-heal" sentence is REPLACED; new pins for `,ceiling-fitted` and for the over-fit carve-out phrase | F2. Leaving the shipped sentence would tell agents a working case cannot work; replacing it with an unqualified optimistic one would be the opposite error |
| **U5** | `internal/store/admission_insight_test.go` — `TestOOMBranchBasesStayOutsideTheAdmissionAdequacyPopulation` gains §3.5's newly-producible strings, including `estimate:p90-prior,ceiling-fitted`, with a comment stating which are producible and which are retained as forward-defensive negatives | I10. A regression that made a fitted basis evaluable would move a published adequacy number silently |
| **U6** | `.aira/tickets/AIRA-153.md` (status, resolution, gate record); `.aira/tickets/AIRA-150.md` gains §3.8's narrowing (route 1 removed, route 3's default half removed, routes 2 and 3-estimate remain — **not closed**); `.aira/tickets/AIRA-151.md` records that its clamp's TARGET and GUARD moved to `FIT(ceiling)`, with the production band and the rationale | deferrals are filed, not silent; and AIRA-151's ticket must not read as untouched when its shipped behaviour changed |
| **U7** | `internal/daemon/confine_admit_test.go` — `TestConfineEstimatorAndOOMEscalationClamp`'s OOM arm: expected `55<<30` → `51352869843` (`= FIT(55 GiB)`, since `MaxOOMPeak 40 GiB < 51352869843`), basis unchanged. `TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins` keeps every assertion (`MaxOOMPeak 10 GiB >= FIT(10 GiB) = 9336885426`, so still unguarded, still terminal, pin still wins) and gains ONE row at `MaxOOMPeak = 9336885425` (one byte under the fit) asserting `9336885426` / `estimate:oom-escalated,ceiling-clamped`, so the new boundary is pinned where the old one was | the clamp's target moved; the second test's comment would otherwise still explain the value by the old guard |
| **U8** | `internal/daemon/sliceceiling_test.go` — `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`: expected `Reserve` `maximum` (68719476736) → `59756066726` (`= FIT(64 GiB)`, headroom 0 in that fixture), basis unchanged. **The AIRA-103 rule it exists for is preserved and strengthened**: against the throttled 1 GiB ceiling the guard would still fail (`MaxOOMPeak 50 GiB >= FIT(1 GiB)`), leaving 75 GiB and a terminal refusal, so the test still discriminates static-vs-throttled — and its grant no longer depends on the fixture's `current = 0`, which removed an AIRA-150 knife-edge from a shipped test | the clamp's target moved; the test asserts that target exactly |
| **U9** | `internal/daemon/admit_saturated_diagnosis_test.go` — `oomClampedHistory()` re-based onto **AIRA-150 route 3's estimate half**, which this change leaves untouched: `runner.PeakRSSStats{TotalCount: 5, SampleCount: 5, PeakMax: 3698281962}` → `estimate == 4253024256`, byte-exactly the 4 GiB fixtures' entry ceiling, basis `estimate:max=3698281962,n=5,f=115`. Renamed `ceilingExactEstimateHistory()`; its comment records the third re-basing and why. **All ten call sites, the eight 4 GiB declarations, the two 8 GiB ones and every `Required`/`Ceiling`/`Grantable`/contention assertion are otherwise unchanged** | §1.5's correction. Under the retargeted guard the OOM shape no longer clamps (`MaxOOMPeak 3758096384 >= FIT = 3698281961`) and would be refused at entry, so every one of those tests would die in `startSaturatedAdmit`'s "never reached the queue" loop. Route 3-estimate is chosen over the gate-suggested route 2 (a pinned reserve equal to the ceiling) because route 2 makes the resolved reserve EQUAL the client's request, which the helper's own contract forbids: "different from what the client asked for, so `Required`, `Ceiling` and `Grantable` are three distinct numbers an implementation that echoes the request cannot fake" |

### 7.3 Must stay green, unedited — the false-fail direction

Named explicitly, because "the suite is green" is not evidence unless the tests
that *should* have objected are known. Each was checked against §0.1.

- `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` and
  `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` (real cgroup,
  `AIRA_REAL_CGROUP=1`) — ceiling `6400507904`, prior below it (no fit) and
  escalation ~84 MiB (clamp never entered). **The single most important green in
  the set:** AIRA-128's shipped self-heal claim and its basis string must not
  move, and a mis-implementation that fitted unconditionally, or that clamped
  outside the escalation branch, fails here first.
- `TestAnOOMRecordNoLongerChangesWhetherAnOverCeilingEstimateIsClamped` — ceiling
  44 GiB, prior 4 GiB, the estimate determines the value and the clamp is never
  entered; AIRA-151's consistency claim is unaffected.
- `internal/runner/confine_reserve_resolution_linux_test.go` — the whole
  `ResolveConfineReserve` table, unedited. **This is where "the client still
  sends the unconditioned default, and all three pinning arms still pin" is
  pinned**, and it is the evidence that this change is daemon-side only.
- `internal/runner/admission_saturated_message_test.go` T13–T17 and AIRA-151's
  `TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis` /
  `TestTooLargeRejectionForAnUnescalatedOverCeilingReserveIsAcceptedByTheClient`
  — client-side renders driven by synthetic payloads; the message is unchanged.
- `TestConfineRejectedAdmissionCreatesNoScopeAndStartsNoChild` — the terminal
  refusal still creates nothing.
- `internal/daemon/worker_admit_test.go` — worker-admit never calls
  `resolveAdmitReserve`.
- `TestDelegateRAMScopeCeilingIsIndependentOfPinnedReserve` and the delegate-ram
  scope-ceiling clamp in `confine_admit_test.go` — `resolveDelegateRAMScopeCeiling`
  is a different function with its own clamp and is not touched.

### 7.4 Mutation evidence to produce at implement time

Each applied alone in a detached throwaway worktree at the implementation commit,
then reverted; each must go RED, with the exact failing test recorded.

1. **M1 — fit AFTER the resolution instead of before it** (§2.4's
   counterexample). → a dedicated T3 row (`MaxOOMPeak = 671088640`, ceiling
   `1031798784`) RED: the answer becomes `897216333` / `fallback:…,ceiling-fitted`
   instead of `1006632960` / `estimate:oom-escalated`. Value AND basis both move,
   so the row cannot pass by coincidence.
2. **M2 — fit the ordinary estimate as well as the priors** → U1 RED (the
   over-ceiling estimate is admitted instead of refused) and T3(i) RED.
3. **M3 — drop the `,ceiling-fitted` suffix** → T2 and T3 RED. The mutant that
   would have shipped a changed number under an unchanged label.
4. **M4 — remove the `MinPinnedScopeCap` floor** → T1's floor rows and T8 RED.
5. **M5 — fit to the ceiling itself** → T1's strictly-below rows RED and T5's
   grant assertion RED (`reserve == ceiling` is refused at `current = 4096`). The
   mutant that reintroduces AIRA-150.
6. **M6 — apply the fit before the pinned return** → T7 RED.
7. **M7 — widen AIRA-151's tie-break to `escalated >= reserve`** → U2's re-based
   `tie` row RED. Carried forward from AIRA-151 so its ruling stays pinned on a
   reachable shape (R8).
8. **M8 — leave the clamp's GUARD at master's `MaxOOMPeak < ceiling`** (target
   still the fit) → T11's `MaxOOMPeak == FIT(ceiling)` and `== FIT(ceiling)+1`
   rows RED, T12 RED (a waiter is enqueued), T13 RED at the kernel. **This is the
   mutant that reintroduces the 30-minute wedge**, and the reason F1 could not be
   deferred.
9. **M9 — leave the clamp's TARGET at `ceiling`** (guard already the fit) → U2's
   row `b`, U7 and U8 RED, and T11's clamped row RED. The mutant that keeps
   producing a value the slice cannot grant.
10. **M10 — drop the `>= ceiling` gate on the fit** (revision 1's ungated form)
    → T2's gate rows RED at ceiling `4294967297`. The mutant that silently
    under-provisions a job that works today (F3).

### 7.5 Validation to record

Serialised, never concurrent, each under `aira confine`, each with its exact exit
code, `pass`/`fail`/`unevaluated` kept distinct, and no truncated log read as
green:

- `aira confine -- go build ./...`
- `aira confine -- go vet ./...`
- `aira confine -- gofmt -l internal/ cmd/` (must print nothing)
- `aira confine -- go test ./internal/daemon/... ./internal/runner/... ./internal/core/... ./internal/store/... -count=1`
- `aira confine -- go test -race ./internal/daemon/... -count=1` (CI has `-race`
  off per AIRA-20, so the race evidence is produced locally)
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — **required**,
  because §1.5's AIRA-128 no-op claim and T9/T13's whole existence are
  load-bearing, and a skip is reported as `unevaluated`, never as a pass.

If a job must be stopped, `kill <PID>` the `aira confine` process itself; never
`systemctl --user stop aira.slice`.

---

## 8. Deferrals — filed, not silent

Each gets its own ticket via `aira id` at implement time; **no ID is hand-picked
here**, per `CLAUDE.md`. AIRA-150, AIRA-151 and AIRA-152 already exist and are
updated rather than re-filed. (Revision 1's G1 — "the ladder's top rung wedges" —
is NOT here: it ships as part of this change, per §2.5.)

- **G1 — the top rung can cost one extra real run (R4).** If a kernel OOM records
  a peak strictly below `FIT(ceiling)` rather than at it, the next admission
  clamps to exactly `FIT(ceiling)` and the job runs there before the terminal
  refusal. File with the derivation, the observation that each attempt RUNS and
  is attributed (so it never wedges), and the note that `cap-source=auto:daemon-reserve`
  already tells an agent that an identical re-run killed at the same cap is a
  genuine bug (`skill.go:325`).
- **G2 — the admitted-but-certainly-OOM band (§3.4).** A slice whose
  `FIT(ceiling)` is above 1 MiB but below any real job's footprint now admits jobs
  that will be killed. Accepted deliberately: refusing there would require a
  judgement about "how big a real job is", which *AIRA is primitives, not
  judgement* forbids, and the ladder terminates honestly.
- **G3 — ci-shim: a fitted booking with no kernel enforcement (R5).** Record that
  the fit reaches shim mode through the four post-block fallbacks (never the OOM
  branch, which AIRA-121 C10 makes unreachable there), that it is what makes a
  small container usable at all, and that the booking is advisory in that mode by
  design.
- **G4 — AIRA-150 stays open, narrowed twice.** Its ticket gains: route 1
  (the clamp) removed, route 3's client-default half removed, routes 2 (pinned ==
  ceiling) and 3-estimate remaining and now driven deliberately by the re-based
  facet-2b fixture. **It must not be closed by this merge.**
- **G5 — AIRA-151's clamp changed and its ticket must say so.** Not a deferral of
  work but of RECORD: its rule, nesting, tie-break strictness and AIRA-103
  ceiling choice are untouched; its target and guard are now `FIT(ceiling)`; its
  production band is §0.1's.
- **G6 — the `E_ADMIT_TOO_LARGE` message still names no escape hatch and prints
  raw bytes.** AIRA-151's filed G3, unchanged here — but its population is now
  narrower and more uniform (an over-ceiling ESTIMATE, an over-fit OOM
  escalation, or a pinned request), which makes "pin at or below
  `cap_minus_headroom`" correct advice for the first two and wrong for the third,
  so the case analysis it was deferred for is still required.
- **G7 — the agent guide's "A FIRST run … is capped at a machine-wide prior
  (`estimate:p90-prior`)" is imprecise when no p90 exists**, where the true basis
  is `fallback:no-history`. Pre-existing, unrelated to the fit, and named so the
  §3.7 edit is not mistaken for having addressed it.
- **G8 — install-time refusal is NOT taken (§2.1).** Filed with the derivation
  that it cannot establish its property, plus the observation from §1.2 that
  `install.go`'s own default would fail such a check on an 8 GiB box.
- **G9 — no real-cgroup coverage of the degenerate refusal (T8's shape).**
  Reproducing it against a real cgroup needs a slice with a sub-1.2 MiB ceiling,
  which cannot hold the test harness itself. Accepted coverage gap, of the same
  class as AIRA-149's F7; T8 drives it deterministically through the real wire
  path with a stubbed reader.
- **G10 — AIRA-152 remains superseded.** Nothing here reopens it; §2.3 records
  that this change refines rather than removes the `max()` rule its rejection
  rested on.

---

## 9. Files touched at implement time

| File | Change |
| --- | --- |
| `internal/runner/resource_estimate.go` | new `SliceFittedReserve` (§3.2). No existing line changes; `memoryEstimateSafetyPct` and `MinPinnedScopeCap` are read, not moved |
| `internal/daemon/admit.go` | `resolveAdmitReserve` only: the `fit` local after the pinned return, the gated prior fit and its `fitted` local, the `suffix` provenance local in the history block, the suffix on the OOM branch's non-escalated return and on the `SampleCount>=3` return, the retargeted clamp condition and target, the gated p90 fit, and the suffix on the four post-block fallbacks. **No other function in the file changes** |
| `internal/core/skill.go` | the §3.7 clause replaces AIRA-151's now-false sentence and keeps the true one |
| `internal/runner/resource_estimate_fit_test.go` | new — T1 |
| `internal/daemon/admit_ceiling_fitted_test.go` | new — T2, T3, T4, T7, T11 |
| `internal/daemon/admit_ceiling_fitted_wire_test.go` | new — T5, T6, T8, T12 |
| `internal/daemon/confine_selfheal_small_slice_real_cgroup_linux_test.go` | new — T9, T10, T13 |
| `internal/daemon/admit_oom_clamp_wire_test.go` | U1 |
| `internal/daemon/admit_oom_clamp_scope_test.go` | U2 — four rows |
| `internal/daemon/admit_oom_basis_test.go` | U3 — two rows |
| `internal/core/skill_test.go` | U4 |
| `internal/store/admission_insight_test.go` | U5 |
| `internal/daemon/confine_admit_test.go` | U7 |
| `internal/daemon/sliceceiling_test.go` | U8 |
| `internal/daemon/admit_saturated_diagnosis_test.go` | U9 — the fixture helper only |
| `.aira/tickets/AIRA-153.md`, `.aira/tickets/AIRA-150.md`, `.aira/tickets/AIRA-151.md` | U6 |

No change to `ResolveConfineReserve`, `checkedAvailable`, the escalation
arithmetic or its overflow guard, the clamp's NESTING or its `reserve > ceiling`
entry condition, the ceiling computation, the `reserve > ceiling` boundary, the
enqueue-time re-check, the AIRA-59 freeze, the AIRA-114 bound, the AIRA-101 gate,
the AIRA-103 throttle rule, the reserve ledger, `resolveDelegateRAMScopeCeiling`,
`worker_admit.go`, `internal/store/admission_insight.go`, the protocol version,
any CLI flag, or any MCP/Skill dispatch surface.

---

## 10. Rulings carried from the plan gate, and what is left open

Revision 1 put six questions to the gate. All six were ruled on; they are
recorded here as decisions, not questions, so a later reader does not reopen a
settled point.

1. **`FIT(c) = ⌊100c/115⌋` is the quantity.** ACCEPTED. The "inverse of the
   estimator's own margin" rationale is partly post-hoc, but it introduces no new
   constant, is strictly below the ceiling, cannot degenerate under headroom
   configuration, and its ~13% margin is generous against the measured 4 KiB
   residual that produced the AIRA-139 flake. With the clamp retargeted the
   ladder terminates honestly, so the "should the fit be below `2c/3` to buy one
   more grantable rung" question is moot.
2. **Both priors are fitted, under the same gate.** ACCEPTED as revision 1
   proposed, now with the `>= ceiling` trigger applied to both.
3. **`,ceiling-fitted` is required** by AIRA-149's basis rule; the spelling is
   accepted. "No token" was refused explicitly.
4. **`MinPinnedScopeCap` is the degenerate floor.** ACCEPTED, with G2 remaining
   filed.
5. **The clamp retarget ships WITH this change**, not after it. This reverses
   revision 1's answer; §2.5 carries the derivation and §0.1 the production
   effect.
6. **U1 is re-based onto an ordinary estimate (row c′)**, rather than deleted or
   kept on a shape no real client can produce. ACCEPTED.

Open for the build gate rather than for this plan:

- Whether U9's fixture should instead take AIRA-150 route 2 (a pinned reserve
  equal to the ceiling), accepting the loss of `oomClampedHistory`'s
  request-≠-resolved property, or keep route 3's estimate half as chosen here.
  The plan takes route 3 with the derivation in U9; a reviewer who prefers route
  2 should say so before implementation, because it also requires a second
  fixture for `TestSaturatedRejectionCarriesTheResolvedReserveAndCeiling`.
- Whether T13's phase A should pin `--memory-max` at 580 MiB as specified, or
  drive the peak from an unpinned first run and accept that the recorded peak may
  land a page below the fit (R4/G1), which would make the test non-deterministic.
  The plan pins it deliberately so the kernel-level assertion is exact.
