# AIRA-153 — the unpinned reserve PRIOR is fitted to the slice it is being admitted into

Status: **plan revision 1 — written for the plan gate. No implementation has
begun; this commit changes only this document.**

This changes SIZING on the machine-wide admission gate every `aira confine` job
on this box passes through, so it is the full two-loop per `CLAUDE.md`, not the
light path.

Ticket: `.aira/tickets/AIRA-153.md` (P2, `admission`/`confine`), filed as
AIRA-149 deferral **F4** and sharpened by AIRA-151's own resolution notes. The
structural defect it interacts with is `.aira/tickets/AIRA-150.md`; the shipped
sibling that removed this defect's last accidental mitigation is
`.aira/tickets/AIRA-151.md`; the superseded candidate is
`.aira/tickets/AIRA-152.md`.

**Unlike AIRA-151, this ticket has NO owner-mandated direction.** The ticket
names three candidates and settles none. §2 evaluates all three from source and
records why one is taken and two are refused; the plan's own rigor is what
carries that decision, and §10 puts the load-bearing choices in front of the
gate explicitly rather than presenting them as settled.

Branch: `aira153-condition-default-on-ceiling`, from `origin/master` at
`0165918`.

Prior art this plan must not re-litigate: AIRA-151 (the clamp narrowing that
shipped first — its plan is
`docs/superpowers/plans/2026-09-07-aira151-narrow-oom-ceiling-clamp-plan.md`),
AIRA-149 (the honesty fix and the basis-naming rule — its plan is
`docs/superpowers/plans/2026-09-07-aira149-admit-oom-clamp-honesty-plan.md`,
sections 0, 2 and 8), AIRA-128 (the OOM self-heal and its real-cgroup fixture),
AIRA-139 (the flake removed at fixture level), AIRA-103/AIRA-106 (the
static-vs-throttled ceiling rule), AIRA-121 (ci-shim containment and its ledger),
AIRA-114 (the aggregate bound), AIRA-101 (exclusivity), AIRA-62 (one place
decides the reserve), AIRA-71 (a bounded, sound, silent wait is not a
correctness bug — the fix is to make it visible).

**Verdict, up front.** `resolveAdmitReserve` gains ONE rule, applied at two
sites, expressed as one pure helper:

> A reserve figure that is a **PRIOR** — a guess about a command AIRA has never
> measured — is bounded by what the destination slice can actually grant a
> single job. A reserve figure derived from **THIS SIGNATURE'S OWN EVIDENCE** —
> the ordinary peak-history estimate and the OOM escalation — is not.

There are exactly two priors: the client's unpinned reserve hint
(`runner.DefaultConfineMemoryReserve`, 4 GiB, unconditioned today) and the
machine-wide `estimate:p90-prior`. Both are AIRA's own guesses; neither has any
relationship to the slice they are about to be admitted into. Everything else in
the function is byte-for-byte unchanged, including AIRA-151's nested clamp,
AIRA-149's basis table, the `math.MaxInt64` overflow guard, the terminal
`reserve > ceiling` boundary and `checkedAvailable`.

The behavioural consequence, stated so it can be checked rather than believed:
**on a slice whose admission ceiling is at or above 4,939,212,391 bytes the
change is a no-op by construction, and on a smaller one an unpinned request that
is refused terminally today is instead admitted at a value the slice can grant,
restoring AIRA-128's cold-start self-heal for the whole class of slices AIRA-151
recorded it as broken on.**

---

## 0. Source read, at the commit this plan was written against

Every line below was read fresh in this worktree at `0165918` (== `origin/master`).

| Fact | Where | Confirmed |
| --- | --- | --- |
| `ResolveConfineReserve` leaves an unpinned request at exactly `DefaultConfineMemoryReserve`, and the ceiling appears nowhere in it | `internal/runner/confine.go:103-119`, `:19` (`4 << 30`) | yes — this is the ticket's subject, and the function is PURE and portable, with no access to any slice fact |
| `--memory-reserve`, `--memory-max` (non-delegate) and `--delegate-ram` each set `pinned` | `confine.go:104-117` | yes — three independent arms, all pinned |
| the client sends `pinned = !DaemonEstimateMemory \|\| MemoryReservePinned`, and only the confine path sets `DaemonEstimateMemory: true` | `internal/runner/admission_linux.go:467`; `internal/runner/confine_linux.go:1631` | yes — **so the affected population is exactly "unpinned confine requests"; every `aira run` admission is pinned and cannot reach this change** |
| a pinned request returns at the first line of `resolveAdmitReserve` with `pinned:client` | `internal/daemon/admit.go:1609-1611` | yes |
| the ordinary estimate needs ≥ 3 usable samples; the estimator's margin constant is `memoryEstimateSafetyPct = 15`; the estimate is `peak + peak*15/100`; `MaxMemoryEstimateReserve = 1 << 50` | `internal/runner/resource_estimate.go:11-13`, `:33-41`, `:43-65`, `:8` | yes |
| the estimator's four `!ok` bases are `fallback:no-history`, `fallback:capture-unavailable`, `fallback:insufficient-samples:n=%d`, `fallback:malformed` | `resource_estimate.go:33-46`, `:69-71` | yes — non-empty on every `!ok` path, which AIRA-149 relied on and this plan still does |
| `ordinary.OOMCount = 0` before the estimator call, so the estimator's own `estimate:oom:` arm is unreachable from this branch | `admit.go:1630-1631` vs `resource_estimate.go:56-60` | yes |
| the OOM branch, post-AIRA-151: `escalated` + overflow guard, `oomBasis := basis + ",oom-on-record"`, then `if escalated > reserve { reserve = escalated; oomBasis = "estimate:oom-escalated"; if MaxOOMPeak < ceiling && reserve > ceiling { reserve = ceiling; oomBasis += ",ceiling-clamped" } }`, then `return reserve, oomBasis` | `admit.go:1647-1722`, the nested clamp at `:1716-1719` | yes — **the clamp is INSIDE the escalation branch and this plan does not move it** |
| the `SampleCount >= 3 && reserve > 0` return, which returns the CLIENT HINT when the estimator was `!ok` at three or more samples | `admit.go:1723-1725` | yes — a fifth site the hint escapes through, easy to miss |
| the machine-wide p90 prior return, reached whenever the per-signature block did not return | `admit.go:1729-1734` | yes — `estimate:p90-prior`, built from a synthetic `PeakRSSStats{TotalCount:3, SampleCount:3, PeakMax: peak}` |
| the four post-block fallbacks each `return request.reserve` verbatim: `fallback:no-signature`, `fallback:history-unavailable`, `fallback:insufficient-samples`, `fallback:no-history` | `admit.go:1735-1744` | yes — **these are where a NOVEL command on a small slice dies today** |
| `resolveAdmitReserve` takes `request admitRequest` BY VALUE | `admit.go:1608` | yes — so assigning `request.reserve` inside it cannot escape to any caller |
| `admitConnection` computes the request-entry ceiling as `subtractFloor(maximum, admitSliceHeadroom(admitOutstandingJobs(path)+1))`, resolves against it, and refuses terminally at `reserve > ceiling` | `admit.go:1920-1932`, `admitOutstandingJobs` at `:1276-1288`, `subtractFloor` at `:972-977` | yes — `subtractFloor` floors at 0, so `ceiling >= 0` always |
| the too-large refusal happens BEFORE `enqueueResolvedConfineAdmit`: nothing charged, no waiter, no scope | `admit.go:1929-1933` | yes |
| the enqueue-time re-check refuses `E_ADMIT_TOO_LARGE` again under `queue.mu`, and the waiter is built with the RESOLVED reserve, never `request.reserve` | `admit.go:2080-2082`, `:2115` | yes — so nothing downstream reads the client's own hint |
| `admitSliceHeadroom(n) = base + n*perJob`, production defaults `base = 2 << 30`, `perJob = 64 << 20` | `admit.go:1079-1089`, `:49-50` | yes |
| the grant gate is `waiter.reserve > available`, `available = checkedAvailable(current, effectiveMaximum, reclaimable, outstanding+adopted, admitSliceHeadroom(outstanding+adopted+1))`, charging `max(current-reclaimable, outstanding+adopted)` | `admit.go:2459-2461`, `:2484`, `:2648` | yes — **so a reserve equal to the ceiling is grantable only inside AIRA-150's residual band; a reserve strictly below it by M tolerates M of charge** |
| **the resolved reserve BECOMES a non-delegate unpinned job's own hard scope `memory.max`** on an admitted daemon grant, recorded as `cap-source=auto:daemon-reserve` | `internal/runner/confine_linux.go:978-981` | yes — so this change sizes kernel-enforced containment, which is why §2.4 refuses any target derived from transient charge |
| the unpinned CLIENT-side fallback (flock, daemon unavailable) is deliberately left UNCAPPED | `confine_linux.go:971-977` (comment) | yes — so a client-side fit would have no effect on the fallback path, only on what is sent to the daemon (§2.3) |
| `MinPinnedScopeCap = 1 << 20` is "the smallest reserve a caller may DECLARE", refused at the runner boundary | `internal/runner/confine.go:48-62`; `confine_linux.go:471-473` | yes |
| `workerAdmitEstimatedBytesMin = 1 << 20` exists "so a sub-page estimate can never floor memory.max to zero pages and instant-OOM the worker on placement" | `internal/daemon/worker_admit.go:27-30` | yes — **the precedent for this plan's degenerate floor, in this codebase's own words** |
| worker-admit is a SEPARATE path with a client-supplied `estimated_bytes` and its own validation; it never calls `resolveAdmitReserve` | `worker_admit.go:915-916`, `:941` | yes — untouched |
| ci-shim mode uses the SAME `admitConnection` and the same `resolveAdmitReserve`, swapping only the slice resolver and the memory reader, and its budget is a real finite one routinely below 4 GiB | `internal/daemon/shim.go:132-201` | yes — so this change reaches shim mode, where AIRA-151 R5 recorded an unpinned request as already terminally refused |
| ci-shim reports no peak-RSS and no OOM (AIRA-121 C10), so the OOM branch is unreachable there — but the four post-block fallbacks are not | `internal/runner/confine_shim_linux.go:469-484`; `admit.go:1735-1744` | yes — **the shim's route into this defect is `fallback:no-history`, not the OOM branch** |
| the AIRA-52 gauge selects `estimate%` and then `$`-anchors its matchers, so every other basis lands on `default:` → `malformedBasis` + `excluded`; `fallback:` bases never enter the population at all | `internal/store/admission_insight.go:64-66`, `:111-122`; `internal/runner/estimate_actual.go:47` | yes — unchanged by this ticket (R5) |
| the AIRA-128 fixture's slice budget is `runner.DefaultConfineMemoryReserve + (2 << 30)` = 6 GiB, chosen by AIRA-139 to keep phase 3 clear of the clamp; fixture headroom is 32 MiB + 8 MiB | `internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go:93-133`, `:174-175` | yes — §1.4 shows this makes it a structural no-op here too |
| AIRA-149's facet-2b fixtures: **eight** `maximum = 4 GiB` declarations (`:259,301,333,362,405,539,567,641`) serving **ten** `oomClampedHistory()` call sites (`:261,303,335,364,407,541,571,597,645,673`), plus **two** `8 GiB` declarations (`:227` inside a `const (…)` block, and `:456`) that call it not at all | `internal/daemon/admit_saturated_diagnosis_test.go:41-42`, `:57-83` | yes — the same eight-declarations/ten-call-sites mapping AIRA-151 §9 recorded, at shifted lines; §1.4 shows every one of them resolves IDENTICALLY after this change, and the two 8 GiB sites are outside the OOM branch entirely |
| the agent guide's self-heal paragraph carries AIRA-151's clause, and `skill_test.go` pins its exact sentence | `internal/core/skill.go:324`; `internal/core/skill_test.go:639-660` | yes — that sentence becomes FALSE after this change and must move (§3.6) |
| `E_ADMIT_TOO_LARGE`'s operator message is `required=%d cap_minus_headroom=%d basis=%s`, raw bytes, no escape hatch | `admit.go:2799-2801` | yes — unchanged here; it is AIRA-151's filed G3 |

### 0.1 The four measured shapes, re-derived against the code above

All arithmetic below was computed, not estimated, and every figure in this plan
uses the exact integer form specified in §3.2.

```
FIT(c) = floor(c * 100 / 115)     "the largest prior that leaves the estimator's
                                   own 15% growth margin inside the ceiling c"

(1) the AIRA-139 / AIRA-149 / AIRA-151 measured shape — 1 GiB slice
    maximum 1073741824, headroom 32 MiB + 8 MiB, ceiling 1031798784
    stats {TotalCount:1 SampleCount:1 PeakMax:56360960 OOMCount:1 MaxOOMPeak:56360960}
    client hint 4294967296 (the unpinned default)
      today : hint survives (escalated 84541440 does not raise it), 4294967296 > ceiling
              -> E_ADMIT_TOO_LARGE, immediately, at request entry
      after : FIT(1031798784) = 897216333 < 4294967296 -> the hint is fitted
              escalated 84541440 still does not raise it -> reserve 897216333
              basis "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"
              897216333 <= ceiling -> ADMITTED; grant gate at current=4096 gives
              available = ceiling - 4096 = 1031794688 >= 897216333 -> GRANTED
              scope memory.max = 897216333, cap-source=auto:daemon-reserve

(2) a NOVEL command on the same slice (no history at all, no p90)
      today : fallback:no-history returns 4294967296 -> E_ADMIT_TOO_LARGE
      after : 897216333, basis "fallback:no-history,ceiling-fitted" -> GRANTED
    This is the larger half of the defect and the ticket's text names it:
    on such a slice NOTHING unpinned can run today, OOM record or not.

(3) the AIRA-128 real-cgroup fixture — 6 GiB slice
    ceiling = 6442450944 - 41943040 = 6400507904
    FIT(6400507904) = 5565659046 >= 4294967296 -> NO-OP. Not one byte moves.

(4) AIRA-149's facet-2b fixtures — 4 GiB slice, AIRA-151 row (b)
    ceiling = 4294967296 - 41943040 = 4253024256
    FIT(4253024256) = 3698281961 -> the hint IS fitted, from 4294967296
    but stats.MaxOOMPeak 3758096384 escalates to 5637144576, which raises the
    reserve either way, so the escalation still determines the value, the clamp
    still fires (3758096384 < 4253024256 and 5637144576 > ceiling), and the
    answer is STILL exactly the ceiling 4253024256 with basis
    "estimate:oom-escalated,ceiling-clamped". Every assertion in that file is
    unchanged. -> NO-OP where it matters.
```

The exact no-op boundary, computed rather than asserted: `FIT(c) >= 4294967296`
iff `c >= 4939212391`. So

- **ceiling ≥ 4,939,212,391 B (≈ 4.6 GiB): the change is a strict no-op.**
- ceiling ≤ 4,939,212,390 B: the unpinned default is fitted.

With production headroom (`base = 2 GiB`, `perJob = 64 MiB`) and `j` outstanding
jobs at arrival, that boundary is a slice `maximum` of
`4939212391 + 2147483648 + 67108864*(j+1)`, i.e. **≈ 7.15 GiB at `j = 0`**,
rising by 64 MiB per outstanding job. On the 64 GiB production `aira.slice` the
condition is unreachable at any plausible occupancy: it would need **918**
outstanding jobs at arrival, and `admitMaxWaiters` bounds a slice's queue at
**256** (`admit.go:42`).

### 0.2 The population, named unambiguously

| # | What determined the number | Fitted? | Rationale |
| --- | --- | --- | --- |
| P1 | the client's UNPINNED reserve hint (`fallback:no-signature` / `:history-unavailable` / `:insufficient-samples` / `:no-history`, the `SampleCount>=3` `!ok` return, and the OOM branch's rows (d)/(e)) | **YES** | a PRIOR: a compiled-in constant with no relationship to this command or this slice |
| P2 | the machine-wide `estimate:p90-prior` | **YES** | a PRIOR: a measurement of OTHER commands, used because this one has none |
| E1 | the ordinary per-signature estimate (`estimate:max=…`, `estimate:capped`) | no | THIS command's own measured evidence |
| E2 | the OOM escalation (`estimate:oom-escalated`, with or without `,ceiling-clamped`) | no | THIS command's own measured evidence, and AIRA-151 owns its ceiling interaction |
| E3 | a PINNED reserve (`pinned:client`) | no | the operator's own number; returns before any of this |

The line is the same one AIRA's honesty rules already draw everywhere else:
**a guess may be bounded by an established fact; a measurement may not be
silently reduced to fit one.** When E1/E2 exceed the ceiling the honest answer is
the existing terminal `E_ADMIT_TOO_LARGE` naming both numbers — this command
genuinely needs more than this slice can give — and this plan preserves it
exactly.

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

On a large slice this is invisible. On any slice whose ceiling is below the
constant, the constant alone is over the ceiling, and every path that returns the
hint verbatim (six sites, §0's table) yields a value the boundary at
`admit.go:1929` refuses terminally. The consequence is not "some requests are
sized badly": it is that **no unpinned confine job can run on such a slice at
all**, because a novel command has no history, `fallback:no-history` returns the
constant, and the constant is refused.

The same is true of the machine-wide prior on a box whose p90 exceeds a small
slice's ceiling — the shim case is the concrete one, since one machine-wide
`state.db` serves both a large `aira.slice` and a small container budget.

### 1.2 What AIRA-151 changed, and why that makes this worse rather than better

Recorded because AIRA-151's own resolution note is candid about it and the gate
must not read this ticket as a reversal:

- **before AIRA-151:** a signature with an OOM record had its over-ceiling hint
  clamped onto the ceiling, so it was admissible and *sometimes* granted — inside
  the residual band AIRA-151 §2.3.1 derives, which is byte-exact-zero charge on a
  slice the request entered empty. Broken, but occasionally worked.
- **after AIRA-151:** that clamp applies only where the escalation determined the
  value. Stated exactly rather than as "never": the escalation determines it only
  when `1.5 × MaxOOMPeak > 4294967296`, i.e. `MaxOOMPeak > 2863311530`, which on
  a slice whose ceiling is under 4 GiB requires the job to have already been
  OOM-killed above ~2.67 GiB — reachable on a 3–4 GiB slice, and impossible on
  anything smaller than 2.67 GiB. So for the small slices this ticket is about
  the hint is returned unclamped and refused terminally, every time; on the
  narrow 3–4 GiB band the clamp still fires and the request waits on AIRA-150's
  residual band instead. Both outcomes are the same defect.

AIRA-151 was right — a wait that almost always ends in a refusal is worse than an
immediate honest refusal — and it removed the last accidental mitigation of THIS
defect. Its plan says so (§3.7 / G2) and files it here. **AIRA-153 does not undo
it.** It removes the reason the value was over the ceiling in the first place, so
the terminal refusal AIRA-151 routes traffic onto stops being reachable *from a
blind prior*, and stays exactly as it is *from real evidence*.

### 1.3 The exact population that changes

A request's resolution changes if and only if **all** of:

```
NOT request.pinned                       (so: not --memory-reserve, not --memory-max,
                                          not --delegate-ram, not any `aira run`)
AND the returned number is P1 or P2      (a prior, per §0.2 — never E1/E2)
AND FIT(ceiling) < that number           (equivalently, for the 4 GiB default:
                                          ceiling <= 4939212390)
AND FIT(ceiling) >= runner.MinPinnedScopeCap   (the degenerate floor, §3.3)
```

Nothing else. Two derivations that must not be inferred and are established from
source in §0 instead:

- **Pinned requests never reach the fit**, because `resolveAdmitReserve` returns
  `pinned:client` at its first line and all three pinning arms are in
  `ResolveConfineReserve`. The delegate-ram half is already pinned by a shipped
  test row (`confine_reserve_resolution_linux_test.go:176-178`).
- **`aira run` never reaches it either**, because the client sets
  `pinned = !DaemonEstimateMemory || MemoryReservePinned` and only
  `confine_linux.go:1631` sets `DaemonEstimateMemory: true`.

### 1.4 What does NOT change — verified against the fixtures, not assumed

Three claims a reviewer would otherwise have to take on trust. Each was
re-derived in §0.1 from the fixtures' own constants:

- **AIRA-128's real-cgroup fixture and its unit twin are untouched.** Their
  ceiling is 6400507904, `FIT` of which is 5565659046, above the 4 GiB hint. The
  fit is a structural no-op there, so `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission`
  and `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` keep every assertion,
  including the basis string and the `ceiling - reserve >= oomSelfHealTargetBytes`
  slack. This is AIRA-151's I4 held again, for a different reason.
- **AIRA-149's ten facet-2b call sites are untouched.** Their hint IS fitted
  (4294967296 → 3698281961) but their escalation raises the reserve past both
  values and the clamp then cuts it to the ceiling, so `Required`, `Ceiling` and
  `Grantable` are the same three numbers. `oomClampedHistory()`, the nine
  `const maximum` declarations and every assertion stay as AIRA-151 left them.
- **The production slice is a no-op at any reachable occupancy** (§0.1's
  boundary arithmetic).

---

## 2. Directions

The ticket names three candidates and mandates none. All three are evaluated
here from source; one is taken.

### 2.1 Candidate A — refuse at install time (REFUSED, and on a derivation)

Make `aira install` refuse to bake a slice whose cap is below
`DefaultConfineMemoryReserve` plus headroom, so the defect cannot exist.

Refused on three grounds, the third decisive:

1. It does not cover the population. A fixture slice, a shim-configured
   container budget (`shim.go:139-201`), a systemd drop-in that lowers
   `MemoryMax` after install, and a CI runner that never runs `aira install` at
   all are all outside its reach, and three of those four are named in the
   ticket.
2. It converts a sizing defect into a product prohibition: "AIRA does not support
   slices below ~7 GiB". That is a decision to remove a legitimate use case (a
   small CI box), taken to avoid sizing a number correctly.
3. **It cannot establish the property it claims, because the ceiling is a
   function of concurrent occupancy, not of the install.** From
   `admit.go:1920-1922` the entry ceiling is
   `maximum - (base + (jobs+1)*perJob)`. On an 8 GiB slice with production
   defaults, `jobs = 31` gives a ceiling of exactly 4294967296 — the unpinned
   default, to the byte — and `jobs = 32` puts it below, so the identical
   unpinned request that was admissible at low occupancy is terminally refused at
   high occupancy on a slice that passed every install-time check. An install
   gate would print a green tick over a machine that still refuses jobs under
   load.

### 2.2 Candidate B — make the daemon publish the admissible size (REFUSED)

Have the daemon tell the client the ceiling, and let the client re-resolve its
own reserve against it.

Refused:

- It relocates one sizing decision into a place with strictly less information
  and adds a round trip to do it. The client would need the same formula this
  plan writes, plus a protocol verb or a two-phase admit to obtain the ceiling.
- The number it would obtain is a **transient**: the entry ceiling moves by
  `perJob` for every job outstanding at arrival (AIRA-151 §2.3.3), so a client
  that fetched, decided and re-sent would be sizing against a stale figure and
  could still be refused.
- It is the AIRA-62 shape, verbatim: a second copy of the reserve decision
  outside `ResolveConfineReserve`, which that function's own doc comment
  (`confine.go:65-102`) exists to prevent. The comment records a 64× silent
  over-reservation caused by exactly this duplication.
- A cheaper variant — have the CLIENT retry after an `E_ADMIT_TOO_LARGE` that
  already carries `cap_minus_headroom` — contradicts the shipped `too_large`
  contract the agent guide states (`skill.go:323`: "the request itself cannot be
  satisfied as written … rather than retrying it unchanged") and makes an
  automatic retry loop out of a terminal code.

A residual of this candidate is worth keeping and is filed rather than adopted:
the daemon *already* publishes `cap_minus_headroom` on the refusal, and the one
population that still meets it after this change (a genuine over-ceiling
estimate) is the population for which "pin at or below this number" is correct
advice. That is AIRA-151's filed **G3** about the message, not this ticket.

### 2.3 Candidate C — condition the unpinned prior on the ceiling (TAKEN)

The ticket's first candidate, refined by §0.2's line between priors and evidence,
and sited **daemon-side**, in `resolveAdmitReserve`, where the ceiling is already
an argument.

Why daemon-side rather than in `ResolveConfineReserve` (which the brief asks be
established rather than assumed):

- The client cannot compute the ceiling. It can read a cgroup cap
  (`ConfineLaunchInfo.CapBytes`) but not `admitSliceHeadroom`'s configured base
  and per-job terms, not `admitOutstandingJobs`, and not the shim budget — and it
  reads that cap *after* admission is requested, not before.
- A client-side fit would be a second, drifting definition of the ceiling: AIRA-62
  again.
- It would have no effect where it appeared to: on the flock fallback path the
  unpinned reserve is deliberately **not** enforced as a cap at all
  (`confine_linux.go:971-977`), so fitting it client-side would change only what
  is sent to a daemon that is going to resolve its own number anyway.

Why it is a refinement of, not a reversal of, the rule AIRA-152's rejection
rested on. That rule is "never size the next attempt below what an unpinned
client asked for" (`.aira/tickets/AIRA-152.md`). After this change it reads
"never size the next attempt below what an unpinned client asked for, **bounded
by what the slice can hold**" — which on every slice where the old rule was
satisfiable is the identical rule, and on the slices where it was unsatisfiable
replaces "refuse the job" with "give it the largest prior the slice can grant".
AIRA-152 stays superseded; nothing here reopens it.

### 2.4 Rejected variants of candidate C

Each was checked against source and rejected for a stated reason, not on taste.

- **Fit to the ceiling itself (`min(hint, ceiling)`).** REFUSED, and this is the
  most important rejection in the plan: a reserve exactly equal to the entry
  ceiling is AIRA-150, which AIRA-151 has just finished removing the systematic
  route into. From §0's grant gate, such a reserve is granted only while
  `charge <= (effectiveMaximum - maximum) + perJob*(J_entry - J_now)`, which on a
  quiet slice the request entered empty is byte-exact zero. Adopting it would
  reintroduce AIRA-150 systematically at a new site, one ticket after it was
  removed. The margin is not decoration; it is the whole difference between
  admissible and grantable.
- **Fit to `ceiling - admitSliceHeadroom(1)`, or to `ceiling - perJob`.** Both
  express the margin in the codebase's own headroom vocabulary, and both
  degenerate: `admitSliceHeadroomBase`/`Supervisor` are configurable and are set
  to **0** at 21 and 22 call sites respectively in `internal/daemon`'s own tests,
  and `subtractFloor` would then make the margin zero — the rejected variant
  above, reachable by configuration. A margin that can be configured to nothing
  is not a margin.
- **A fixed constant margin (`ceiling - 256 MiB`).** An arbitrary number; the
  simplicity rule refuses it, and AIRA-151 §2.4 already refused "ceiling minus an
  arbitrary slack" for the clamp. It also scales wrongly in both directions: on a
  1 GiB slice it is a quarter of the ceiling, on a 64 GiB slice it is noise.
- **Fit to `checkedAvailable` (the currently grantable figure).** REFUSED on the
  documented invariant at `admit.go:2413-2417` and `sliceceiling.go:27-33`: this
  value becomes a job's own kernel-enforced `memory.max`, and sizing containment
  from the slice's transient charge is exactly what AIRA-103 forbids.
  `TestSliceCeilingDoesNotReachTheOOMEscalationClamp` exists to keep it
  forbidden.
- **Fit a FRACTION of the ceiling unconditionally (`min(hint, ceiling/2)`).**
  Refused for changing behaviour on slices where nothing is wrong: it would halve
  the resolved reserve on every slice below 8 GiB, including the whole band where
  the 4 GiB default is admissible and grantable today. The fit must be a no-op
  wherever the current value already works — which is what makes §1.4's three
  no-op claims structural rather than empirical.
- **Fit AFTER the resolution instead of before it.** This looks simpler (one
  site, at the end of the function) and is WRONG, with a counterexample rather
  than an argument. Take hint 4 GiB, ceiling 1031798784, `MaxOOMPeak = 800 MiB`
  (escalation 1258291200):
  - fitting BEFORE: the fitted hint is 897216333, the escalation 1258291200
    raises it, the escalation determined the value, AIRA-151's clamp cuts it to
    the ceiling 1031798784 with basis `estimate:oom-escalated,ceiling-clamped`;
  - fitting AFTER: the escalation 1258291200 does *not* exceed the unfitted 4 GiB
    hint, so the hint wins, is fitted to 897216333, and the job is sized **below
    the 1.25 GiB its own OOM evidence says it needs**, with a basis naming a
    fallback.
  That is precisely the failure the brief names: conditioning the floor must not
  silently cap the ceiling the escalation can reach. Fitting before the
  comparison keeps `max(prior, escalation)` a genuine max of the two candidates.
  Mutation M1 in §7.4 is exactly this.

---

## 3. Design

### 3.1 The rule, in `resolveAdmitReserve`

The client hint is fitted ONCE, immediately after the pinned return, so every
one of the six sites that returns it inherits the fitted value and the
escalation's `max()` sees it as its floor. A provenance-carried suffix — never a
value comparison — names the fit in the basis at the sites where the fitted hint
is what was actually returned.

```go
func (s *Server) resolveAdmitReserve(request admitRequest, ceiling int64) (int64, string) {
	if request.pinned {
		return request.reserve, "pinned:client"
	}
	// AIRA-153. An UNPINNED reserve is a PRIOR -- runner.ResolveConfineReserve
	// hands the daemon a compiled-in constant with no relationship to this
	// command or to the slice it is about to be admitted into, and that function
	// is pure and portable precisely so the reserve decision has ONE home
	// (AIRA-62). The daemon is the only party that knows the ceiling, so the
	// bounding happens here.
	//
	// Fitted BEFORE anything reads `request.reserve`, which is load-bearing in
	// two directions:
	//   - all six sites that return the hint verbatim (the four post-block
	//     fallbacks, the SampleCount>=3 !ok return, and the OOM branch's rows
	//     (d)/(e)) inherit it, so a novel command on a small slice stops being
	//     refused for asking for more than the machine has;
	//   - the OOM escalation's `escalated > reserve` comparison sees the fitted
	//     value as its FLOOR, so the escalation can still raise the reserve to
	//     whatever this command's own OOM evidence justifies. Fitting afterwards
	//     would cap the escalation below its own evidence -- see the plan's §2.4
	//     counterexample.
	// `request` is a value copy (see the signature), so this cannot escape.
	fitted := ""
	if bounded, changed := runner.FitUnpinnedReserveToCeiling(request.reserve, ceiling); changed {
		request.reserve, fitted = bounded, ",ceiling-fitted"
	}
	...
```

Inside the history block the suffix is carried by PROVENANCE, not by comparing
numbers — AIRA-149's own lesson is that a label must name the term that acted,
and two terms can coincide on a value:

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
		if stats.MaxOOMPeak < ceiling && reserve > ceiling {   // AIRA-151, untouched
			reserve = ceiling
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

The machine-wide prior is the second fit site, and it is the same rule applied to
the other prior:

```go
	if peak, ok := s.cachedAdmitPeakP90(readCtx); ok {
		stats := runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: peak}
		if reserve, usable, _ := runner.EstimateMemoryReserve(stats, 0); usable {
			// AIRA-153. The p90 is a PRIOR about commands other than this one --
			// it is consulted precisely because this signature has no history --
			// so it is bounded by the slice exactly as the client's default is.
			// Without this, AIRA-128's cold start (which the guide teaches as
			// `estimate:p90-prior`) is still terminally refused on any slice
			// whose ceiling is below the box's p90, which is the ordinary shape
			// of a small ci-shim budget beside a large aira.slice: one
			// machine-wide state.db serves both.
			if bounded, changed := runner.FitUnpinnedReserveToCeiling(reserve, ceiling); changed {
				return bounded, "estimate:p90-prior,ceiling-fitted"
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
`insufficientSamples`, the escalation and its `math.MaxInt64` guard, AIRA-151's
nested clamp and its `MaxOOMPeak < ceiling` guard, AIRA-149's `,oom-on-record`
and D4's kept estimator basis, and the pinned return are byte-for-byte identical.

### 3.2 `FitUnpinnedReserveToCeiling` — the one quantity, and why it is that one

It lives in `internal/runner/resource_estimate.go`, beside the constant it
inverts and beside `MinPinnedScopeCap`, because that file already owns reserve
SIZING and `internal/daemon` already imports it. The daemon owns the ceiling and
supplies it; the runner owns the rule. That is the existing downward layering,
not a new seam.

```go
// FitUnpinnedReserveToCeiling bounds an UNPINNED reserve PRIOR by what the
// destination slice can actually grant one job, and reports whether it moved.
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
//     request entered empty is byte-exact zero. Fitting TO the ceiling would
//     reintroduce that wedge systematically.
//   - a margin expressed in the configurable headroom terms is zero whenever
//     those are configured to zero, which is a margin only by luck.
// A proportional margin has neither failure: it is ~13.04% of the ceiling on a
// 1 GiB fixture slice and on a 64 GiB production slice alike.
//
// Reports changed=false, leaving the caller's value alone, when:
//   - there is nothing to fit (reserve <= 0, or ceiling <= 0 -- a ceiling of 0
//     is what subtractFloor yields for a slice smaller than its own headroom,
//     and every positive reserve is then correctly refused terminally);
//   - the prior already fits, which is the production case and makes this a
//     structural no-op there rather than an empirical one;
//   - the fitted value would be below MinPinnedScopeCap. That is the DEGENERATE
//     case, and the honest answer there is the existing terminal
//     E_ADMIT_TOO_LARGE naming `required` and `cap_minus_headroom`, not a
//     sub-megabyte memory.max that would instant-OOM the job on placement --
//     the exact hazard workerAdmitEstimatedBytesMin was minted for.
func FitUnpinnedReserveToCeiling(reserve, ceiling int64) (int64, bool) {
	if reserve <= 0 || ceiling <= 0 {
		return reserve, false
	}
	// Overflow-free exact floor of ceiling*100/(100+pct): writing it as one
	// multiply would overflow int64 above ~92 PiB of ceiling, and a wrapped
	// negative here would silently fit every prior to nonsense.
	const scale = 100 + memoryEstimateSafetyPct
	fitted := ceiling/scale*100 + ceiling%scale*100/scale
	if fitted >= reserve || fitted < MinPinnedScopeCap {
		return reserve, false
	}
	return fitted, true
}
```

The two forms of the division were checked to agree on every figure in this plan
(`1031798784 → 897216333`, `4253024256 → 3698281961`, `6400507904 → 5565659046`,
`107374182400 → 93368854260`, `2684354560 → 2334221356`, `8547991552 →
7433036132`) and the identity is exact for all inputs:
`c = 115q + r ⇒ ⌊100c/115⌋ = 100q + ⌊100r/115⌋`.

### 3.3 The degenerate case, and what still refuses

The floor is `runner.MinPinnedScopeCap` (1 MiB), the codebase's own boundary for
"a reserve that can be a real cap", already enforced at the runner boundary for a
declared `--memory-reserve` and already mirrored by
`workerAdmitEstimatedBytesMin` with the reason spelled out in source. Below it
the fit does not fire, the prior stays over the ceiling, and
`admit.go:1929` refuses terminally exactly as today.

So after this change a request is still terminally refused when, and only when:

1. the value is a per-signature ESTIMATE or an OOM ESCALATION above the ceiling
   (E1/E2) — this command's own evidence says it needs more than the slice has;
2. the request is PINNED above the ceiling — the operator's own number;
3. the slice is so small that `FIT(ceiling) < 1 MiB`, i.e. `ceiling <= 1205862`
   bytes (`FIT(1205862) = 1048575`, one byte under `MinPinnedScopeCap`, against
   `FIT(1205863) = 1048576` which fires), or `ceiling == 0` (a slice smaller than
   its own configured headroom).

Named honestly rather than buried: there is a band above that floor —
`FIT(ceiling)` between 1 MiB and whatever the job actually needs — in which the
job is now admitted and will certainly be OOM-killed. That is not a silent
under-provision. It is the self-heal ladder's own domain: the kill is attributed
to the signature, `terminated-by=oom` is reported, the next admission escalates,
and once the recorded peak reaches the ceiling the escalation exceeds it and the
request is refused terminally with both numbers. The ladder terminates; it does
not loop. §5's R3 states where it terminates badly.

### 3.4 The basis token, and why silence was not an option

The value returned changes for the affected population, so the basis must say
so. This is AIRA-149's own rule — *reserve-basis names the provenance of the
number actually returned* — and a change that quietly returned a different number
under an unchanged label would be the defect AIRA-149 shipped to remove,
committed by its own successor.

`,ceiling-fitted` is a new token in the existing grammar
(`family:name[:params]` with comma-separated params, no spaces — the trailer's
`reserve-basis=` field is space-delimited, `confine.go:897-900`). Two structural
properties, both asserted rather than argued:

- **It appears only on a PRIOR.** It is carried by `suffix`, which is cleared the
  moment `estimateUsable` is true and is unreachable from the escalation branch,
  which rebuilds `oomBasis` from scratch. So it can never attach to
  `estimate:max=…`, `estimate:capped`, or `estimate:oom-escalated`.
- **It and `,ceiling-clamped` are mutually exclusive by construction**, because
  `,ceiling-clamped` is appended only inside the escalation branch, which
  discards the suffix. The two tokens therefore partition cleanly: *clamped* =
  the escalation's own value was cut down by the ceiling; *fitted* = a prior was
  sized to the slice before anything else looked at it.

The full set of newly-producible basis strings, which is what §7's gauge pin
enumerates:

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

### 3.5 What the affected request now receives

- an admission, at a reserve the slice can grant, with the grant gate untouched;
- a scope `memory.max` equal to that reserve, `cap-source=auto:daemon-reserve`
  (`confine_linux.go:978-981`) — so the booking and the kernel-enforced bound are
  still the same number, which is why lowering it cannot over-commit the slice
  (I3);
- a trailer whose `reserve-basis=` names both the term and the fit;
- and, if the job genuinely needs more, an OOM attributed to its signature and a
  higher next admission — AIRA-128's ladder, working on a slice where it was
  previously unreachable.

### 3.6 The agent guide

`internal/core/skill.go:324` currently carries AIRA-151's clause, which says:

> When the reserve the daemon resolves is above the ceiling and did NOT come from
> that escalation — the usual shape on a slice smaller than the unpinned default
> — the run is refused immediately with `E_ADMIT_TOO_LARGE` … On a slice that
> small the RE-RUN advice below does not self-heal, and retrying the command
> unchanged cannot help.

After this change **that sentence is false**, and it is false in the direction
that tells an agent to give up on a case that now works. It must move with the
code; leaving it would be a shipped honesty regression, not a stale comment.

The replacement, placed in the same position (after the `,ceiling-clamped`
sentence, before "So an OOM token in the basis is the proof…"), says three
things:

- an unpinned reserve is a PRIOR and is fitted to the slice it is admitted into;
  `,ceiling-fitted` in the basis means exactly that and is not an error — the job
  ran, at the largest prior this slice can grant;
- `,ceiling-fitted` and `,ceiling-clamped` are different facts: *fitted* sizes a
  guess to the slice before the job runs, *clamped* cuts the OOM escalation's own
  value down to the ceiling;
- the terminal `E_ADMIT_TOO_LARGE` still happens, and now means something
  narrower and more actionable: this command's OWN measured history (or a pinned
  flag) asks for more than the slice can hold. The action is unchanged and is the
  one `skill.go:323` already gives — change the request rather than retrying it:
  pin at or below the printed `cap_minus_headroom`, or run where the slice is
  larger.

The "RE-RUN the identical command" conclusion below it becomes true again on a
small slice, which is the property this ticket exists to restore.

### 3.7 What this does NOT close

- **AIRA-150 stays open.** A resolved reserve exactly equal to the entry ceiling
  is still grantable only inside its residual band. This change removes one more
  route onto it (a prior can no longer land there — it lands strictly below by
  construction) and, per R3, makes the escalation-determined route *more*
  reachable on small slices, because those slices now run jobs that record OOM
  peaks at all.
- **AIRA-151 stays exactly as it shipped.** The clamp's condition, guard,
  arithmetic and basis are untouched. What changes is which rows are producible:
  its rows (e)/(e-malformed) — "the client's own unpinned default survived, over
  the ceiling" — become unreachable for a real client, since a prior can no
  longer exceed the ceiling. Its rule is unchanged and still governs every row;
  §7.2 re-bases its two tests onto the routes that remain, and says why.
- **The `E_ADMIT_TOO_LARGE` message** still prints raw bytes and names no escape
  hatch. That is AIRA-151's filed G3; this ticket narrows its population without
  touching its wording, and §7.3 keeps its two shipped pins green.

---

## 4. Invariants

**I1 — pinned requests are byte-identical.** `--memory-reserve`, `--memory-max`
and `--delegate-ram` all return at `admit.go:1609-1611` before the fit exists.
Structural, and pinned by T7 plus the unedited
`confine_reserve_resolution_linux_test.go` table.

**I2 — this command's own evidence is never fitted.** The ordinary estimate and
the OOM escalation reach the ceiling boundary unchanged, so an over-ceiling
estimate is still refused terminally with both numbers. Enforced by the `suffix`
being cleared on `estimateUsable` and by the escalation branch rebuilding
`oomBasis`; pinned by T3's negative direction and by the re-based U1 wire test.

**I3 — no request is ever granted a LARGER reserve than today, and the slice
cannot be over-committed.** Two halves:
*monotonicity* — the fit only ever lowers the hint, and the remaining resolution
is monotone non-decreasing in the hint (`max(hint, escalated)` is monotone; the
clamp only lowers; the estimate ignores the hint), so the resolved value after
the change is ≤ the resolved value before it for every input;
*containment* — the resolved reserve is simultaneously the ledger booking and the
job's own kernel-enforced `memory.max` (`confine_linux.go:978-981`), so a smaller
booking is matched by a smaller hard cap and cannot under-book what the job can
actually take. The monotonicity half is pinned by T4; the containment half is
stated here because it is what makes the direction safe at all.

**I4 — the fit is a strict no-op wherever the prior already fits.**
`FitUnpinnedReserveToCeiling` returns `changed=false` unless
`FIT(ceiling) < reserve`, so the production slice, the AIRA-128 fixture and the
AIRA-149 facet-2b fixtures are untouched by construction, not by measurement
(§0.1, §1.4). Pinned by T1's boundary rows and by §7.3's must-stay-green list.

**I5 — the fitted value is STRICTLY below the ceiling, for every ceiling it
fires on.** `FIT(c) = ⌊100c/115⌋ < c` for all `c >= 1`, so the fit can never
produce AIRA-150's ungrantable equality. This is the property a headroom-derived
or fixed-constant margin could not guarantee (§2.4). Pinned by T1 and by T5's
grant assertion.

**I6 — a degenerately small slice still refuses terminally.** Below
`MinPinnedScopeCap` the fit does not fire, so the prior stays over the ceiling
and `admit.go:1929` answers as it does today, with `required` and
`cap_minus_headroom` populated. Pinned by T1's floor rows and T8.

**I7 — the basis names the fit whenever the fit determined the value, and never
otherwise.** Carried by provenance, never by a value comparison.
`,ceiling-fitted` and `,ceiling-clamped` are mutually exclusive by construction.
Pinned by T2 and T3.

**I8 — `,oom-on-record` is unaffected**, so AIRA-128's attribution proof
(AIRA-149 §3.2) is untouched: the token is appended in the same place, and the
suffix follows it rather than replacing it.

**I9 — the AIRA-52 gauge's evaluable population does not move.** Every
newly-producible basis is either a `fallback:` string (never selected by the
`estimate%` predicate at all) or `estimate:p90-prior,ceiling-fitted`, which fails
the `$`-anchored matchers exactly as `estimate:p90-prior` already does and is
counted `malformed_basis` + excluded. No published adequacy number changes.
Pinned by U5.

**I10 — no new state, no new wire field, no protocol change, no CLI flag, no
MCP/Skill dispatch change.** One pure exported helper, one local and one suffix
in `resolveAdmitReserve`, one basis token, one guide paragraph.

---

## 5. Risks

**R1 — this is a sizing change on the machine-wide gate, and it lowers numbers.**
The population is §1.3's, the direction is downward only (I3), and the lowered
number is simultaneously the job's hard cap, so the slice cannot be
over-committed. The residual is that a job on a small slice now gets a smaller
cap than the 4 GiB it nominally asked for — but on those slices it got no cap and
no run at all, because it was refused. Accepted, and quantified in §0.1.

**R2 — a job admitted at a fitted prior may OOM where today it was refused.**
True, and it is the intended trade: a refusal is not a safer outcome than an
attributed OOM, because the OOM feeds the ladder and the refusal does not. The
honesty machinery for it already exists and is unchanged —
`terminated-by=oom`, exit 137, the OOM advisory, `cap-source=auto:daemon-reserve`
telling the agent that a re-run CAN change the number (`skill.go:325`).

**R3 — the ladder's top rung wedges instead of refusing, and this change makes
that rung reachable on small slices.** The precise condition, derived from
AIRA-151's untouched clamp: once the recorded `MaxOOMPeak` for a signature
satisfies `ceiling/1.5 < MaxOOMPeak < ceiling`, the escalation exceeds the
ceiling, the clamp's guard holds, and the reserve is set to exactly the ceiling —
AIRA-150's kept route 1 — where it is grantable only inside the residual band and
otherwise waits out its window and is refused with AIRA-149's honest saturated
sentence. Because the job does not run, no new peak is recorded, so the state
persists rather than resolving. Today that rung is unreachable on such a slice
only because nothing gets far enough to record an OOM at all.
**Named, not hidden:** it is a genuinely worse UX than a terminal refusal for a
job that cannot fit, it is bounded (one rung, between 2/3 and 1× of the ceiling),
and fixing it means changing the clamp — AIRA-151's deliberately-kept row (b) and
a second sizing decision, which is exactly what "one sizing change per two-loop"
forbids. Filed as **G1** with both candidate answers.

**R4 — ci-shim mode: a fitted booking with no kernel enforcement.** In shim mode
there is no cgroup and no `memory.max`
(`containment=advisory(ci-shim,no-cgroup,no-kill-backstop)`), so the fitted
reserve is a ledger booking that nothing enforces, and a job that exceeds it is
not killed. That is already true of every shim booking and is the mode's
documented limit, not something this change introduces; what changes is that a
small container's jobs are admitted at all rather than answering
`E_ADMIT_TOO_LARGE` for the container's whole life (AIRA-151 R5 recorded that
outcome and explicitly did not remove it). The booking can never exceed the
container's own budget minus headroom, because it is bounded by the ceiling
derived from it. Accepted; recorded as **G3**.

**R5 — the ledger and the AIRA-52 gauge.** Runs that used to end
`reject:too-large` now run and record a `fallback:…,ceiling-fitted` or
`estimate:p90-prior,ceiling-fitted` basis. No existing basis changes spelling and
no classifier changes, so no published number moves except by rows appearing:
`fallback:` bases were never in the `estimate%` population, and
`estimate:p90-prior*` was always excluded as `malformed_basis`. I9; pinned by U5.

**R6 — AIRA-151's own tests are built on a shape this makes unreachable.**
`TestOverCeilingUnescalatedReserveIsRefusedTerminallyInsteadOfClamped` and the
two row-(e) table entries in `admit_oom_clamp_scope_test.go` /
`admit_oom_basis_test.go` all drive an over-ceiling CLIENT DEFAULT. After this
change a client default can no longer be over the ceiling, so those rows must
move onto the routes that remain — an ordinary estimate (row c′) for the wire
test, and their new fitted values for the two tables. AIRA-151's *claim* is not
weakened by this: §7.2 shows each re-based test still fails under a mutation that
restores master's clamp placement. The re-basing is itself the executable
evidence that this ticket removed the systematic route.

**R7 — the tie-break row loses its sizing dimension unless it is re-based.**
AIRA-151 §3.2 promoted `escalated > reserve` from a labelling rule to a sizing
rule and pinned it with a row where the escalation exactly equals the CLIENT
DEFAULT over the ceiling. That shape is now unreachable. §7.2 re-bases it onto an
exact tie between the escalation and an ORDINARY ESTIMATE
(`PeakMax = 2801065628 → estimate 3221225472`; `MaxOOMPeak = 2147483648 →
escalation 3221225472`; ceiling 2684354560), which is reachable, still over the
ceiling, and still goes RED under the `>=` mutation. The tie-break pin survives
intact.

**R8 — a consumer that assumes an unpinned resolution equals the client's
request.** Enumerated from source: the only production consumer of the resolved
value is `admitConnection` (which compares it to the ceiling on the next line)
and the waiter it builds; the basis consumers are the confine trailer,
`Status.ReserveBasis`, `ContainerAdvisories`, the `admission_reserve_basis`
ledger column and the AIRA-52 gauge, all display-or-classify and none of which
parses `ceiling-clamped` today or would parse `ceiling-fitted`. `admit.go:2115`
already builds the waiter from the RESOLVED reserve, never `request.reserve`.

**R9 — this looks small and is not.** The production diff is one helper, one
local, six suffixed returns and one guide paragraph. The mitigation is procedural
and is this document: full two-loop, the population in §1.3, the cost in §5, the
no-op boundary computed in §0.1, and the deferrals in §8 naming what stays open.

---

## 6. Expected yield

- **A slice below ~7 GiB becomes usable for unpinned confine jobs again.** Today
  every such job — novel or OOM-recorded — is refused terminally, so a small CI
  slice, a shim-configured container budget and a fixture slice can run nothing
  without a manually pinned reserve.
- **AIRA-128's cold-start self-heal is restored for that whole class**, which is
  the property AIRA-151 recorded as broken and deferred to this ticket. Proven at
  the kernel by a real-cgroup fixture (T9), not only in unit arithmetic.
- **`aira confine` in a small ci-shim container starts working**, closing by
  construction the outcome AIRA-151 R5 named and left alone.
- One coherent rule replaces an unconditioned constant: *a prior is bounded by
  the slice, a measurement is not* — stated once, applied at two sites, visible
  in the basis.
- Net new production state: **none**. One pure exported function and one string
  suffix.

---

## 7. Tests

TDD: each new test is written and observed RED against `0165918` before the code
exists, except where it is explicitly a pin — stated as such and never presented
as a demonstration (AIRA-149's own honesty note on its T13 is the precedent).

### 7.1 New

| Id | Name / file | Asserts | RED against master because |
| --- | --- | --- | --- |
| **T1** | `TestFitUnpinnedReserveToCeiling` — new `internal/runner/resource_estimate_fit_test.go` | the pure rule, as a table: **no-op** at `ceiling = 4939212391` with the 4 GiB prior and **fits** at `4939212390` (the exact boundary, both sides); the four §0.1 figures (`1031798784→897216333`, `4253024256→3698281961`, `6400507904→` no-op, `2684354560→2334221356`); `changed=false` for `reserve <= 0`, `ceiling == 0`, and a negative ceiling; the **degenerate floor** — `ceiling = 1205863` (fit `1048576` == `MinPinnedScopeCap`, fires) vs `ceiling = 1205862` (fit `1048575`, one byte under, does not fire); **strictly below** — `fitted < ceiling` for every row; and **overflow** — `ceiling = math.MaxInt64` returns a positive value below it, which the naive `ceiling*100/115` cannot | the function does not exist |
| **T2** | `TestEveryUnpinnedPriorPathIsFittedAndSaysSo` — new `internal/daemon/admit_ceiling_fitted_test.go` | all six prior-returning routes at ceiling `1031798784` with the 4 GiB hint: `fallback:no-signature` (empty signature), `fallback:history-unavailable` (read error), `fallback:insufficient-samples` (post-block, one sample, no OOM, no p90), `fallback:no-history` (no history, no p90), the `SampleCount>=3` `!ok` return (`PeakMax: 0`, no OOM → `fallback:malformed,ceiling-fitted`), the OOM branch row (d)/(e) (`fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted`), and `estimate:p90-prior,ceiling-fitted` (p90 = 4 GiB). Each asserts the VALUE `897216333` **and** the exact basis | master returns `4294967296` with an unsuffixed basis on all seven |
| **T3** | `TestCeilingFittedNamesOnlyAPriorAndNeverAppearsBesideCeilingClamped` — same file | the provenance property in both directions: (i) a usable ORDINARY estimate over a small ceiling resolves to the estimate with **no** `,ceiling-fitted` (the c′ shape at ceiling `1031798784`); (ii) an escalation-determined value carries `estimate:oom-escalated[,ceiling-clamped]` and **no** `,ceiling-fitted`; (iii) across a grid of stats × ceilings, no basis ever contains both tokens; (iv) the table-wide `^\S+$` basis assertion AIRA-149 carries | master emits neither token, and (i)/(ii) are the mutation surface for "fit the estimate too" |
| **T4** | `TestResolutionIsMonotoneInTheClientHint` — same file | I3's monotonicity half, without a test seam: over a grid of stats shapes × ceilings × ascending hints, `resolveAdmitReserve` never returns a LARGER value for a SMALLER hint. Combined with T1's `fitted <= reserve` this is the "never larger than today" invariant. **Stated honestly as a property pin, not a demonstration:** it is green on master too, and its RED direction is a future fit that raises a value | GREEN by construction — a pin on I3 |
| **T5** | `TestSmallSliceUnpinnedRequestIsAdmittedAndGrantedInsteadOfRefused` — new `internal/daemon/admit_ceiling_fitted_wire_test.go`, driven through the real `admitConnection` over `net.Pipe` | §0.1 shape (1) end to end: 1 GiB slice, 32 MiB + 8 MiB headroom, `current = 4096`, the measured one-sample OOM history, unpinned 4 GiB → **a waiter IS enqueued** (the negative of AIRA-151's own "no waiter" assertion), one evaluator pass **grants** it, and the grant carries `Reserve == 897216333` and `Basis == "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"` | master answers an immediate `E_ADMIT_TOO_LARGE` and never enqueues |
| **T6** | `TestSmallSliceNovelCommandIsAdmittedInsteadOfRefused` — same file | §0.1 shape (2): the same slice, **no history and no p90 at all** → granted at `897216333` with `fallback:no-history,ceiling-fitted`. This is the class for which nothing unpinned can run today, and it never touches the OOM branch | master answers `E_ADMIT_TOO_LARGE` |
| **T7** | `TestFittingNeverTouchesAPinnedRequest` — `internal/daemon/admit_ceiling_fitted_test.go` | a pinned request at 4 GiB against a 1 GiB ceiling returns `4294967296` / `pinned:client` verbatim and is then refused by the ceiling boundary; driven for a declared reserve, a `--memory-max`-derived reserve and a `--delegate-ram` overhead | **GREEN by construction — a pin on I1.** Its RED direction is a future fit applied before the pinned return, which would silently resize an operator's own number |
| **T8** | `TestASliceTooSmallForAnyViableReserveStillRefusesTerminally` — same wire file | I6: `maximum` chosen so the ceiling is `1205862` (fit `1048575`, one byte below `MinPinnedScopeCap`) → an immediate `E_ADMIT_TOO_LARGE` carrying `required == 4294967296` and `cap_minus_headroom == 1205862`, with **no waiter enqueued**; plus the `ceiling == 0` arm (a slice smaller than its own headroom) | **GREEN by construction — a pin on I6.** Its RED direction is a fit with no floor, which would hand a job a sub-megabyte `memory.max` and instant-OOM it on placement |
| **T9** | `TestSmallSliceOOMSelfHealConvergesOnTheFittedBaseline` — new `internal/daemon/confine_selfheal_small_slice_real_cgroup_linux_test.go`, reusing `TestConfineOOMSelfHealWorkload` and the AIRA-128 phase structure | **the ticket's headline property, at the kernel.** A 2560 MiB fixture slice (ceiling `2642411520`, fit `2297749147`): phase 1 seeds three real runs to establish a p90; phase 2's cold start is capped at the prior and is OOM-killed (`terminated-by=oom`, exit 137, no completion marker); phase 3 re-runs the IDENTICAL argv with no operator action and **succeeds**, at `ReserveBasis == "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"`, `ScopeMemoryMax == 2297749147 >= 1.5 × oomPeak`, and the completion marker present | master refuses phase 3 with `E_ADMIT_TOO_LARGE` and `confine: ran=no`, so the marker never appears — the exact self-heal AIRA-151 recorded as broken |
| **T10** | `TestSmallSliceSelfHealFixtureActuallyExercisesTheFitAndStaysOffTheCeiling` — same file, unit, no cgroup | T9's fixture invariant, the way `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` guards AIRA-128's: the phase-3 resolution must (i) carry `,ceiling-fitted`, so the fixture cannot silently drift onto a slice large enough for the fit to be a no-op, and (ii) leave `ceiling - reserve = 344662373 >= oomSelfHealTargetBytes` (320 MiB), so admission cannot come to depend on the slice's residual charge — the AIRA-139 flake, in this fixture's own terms | the basis does not exist on master |

### 7.2 Updates forced by the change

| Id | What | Why |
| --- | --- | --- |
| **U1** | `internal/daemon/admit_oom_clamp_wire_test.go` — `TestOverCeilingUnescalatedReserveIsRefusedTerminallyInsteadOfClamped` re-based from the client default onto an ORDINARY ESTIMATE. New fixture: same 1 GiB slice and ceiling `1031798784`, stats `{TotalCount:5, SampleCount:5, PeakMax:2147483648, OOMCount:1, MaxOOMPeak:56360960}` → estimate `2469606195`, escalation `84541440` (loses) → `required == 2469606195`, `basis == "estimate:max=2147483648,n=5,f=115,oom-on-record"`, immediate `E_ADMIT_TOO_LARGE`, **no waiter enqueued** | R6. AIRA-151's claim is unchanged and still tested end to end; only its route to an over-ceiling value moves, because a prior can no longer be one. The comment must say that, and that the move is itself the evidence this ticket removed the route |
| **U2** | `internal/daemon/admit_oom_clamp_scope_test.go` — three rows. `e-default` → `897216333` / `fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted`; `e-malformed` → `93368854260` / `fallback:malformed,oom-on-record,ceiling-fitted`; `tie` re-based per R7 to `{TotalCount:5, SampleCount:5, PeakMax:2801065628, OOMCount:1, MaxOOMPeak:2147483648}` at ceiling `2684354560` → `3221225472` / `estimate:max=2801065628,n=5,f=115,oom-on-record`. Rows `a`, `b` and `c-prime` are **untouched** | R6, R7. The AIRA-151 arithmetic was re-derived for each: `2801065628 + 2801065628*15/100 = 3221225472` and `2147483648 + 2147483648/2 = 3221225472` — an exact tie, verified, not assumed |
| **U3** | `internal/daemon/admit_oom_basis_test.go` — the two row-(e) entries of `TestOOMEscalationBasisNamesTheTermThatDeterminedTheReserve` take the same new values as U2's. Every other row of that table, and all of `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes`, `TestResolveAdmitReserveKeepsTheEstimatorsOwnFallbackBasis` and `TestPostBlockInsufficientSamplesFallbackIsUnchanged`, is unchanged (their ceilings are 60 GiB and 100 GiB → no-op) | the same two rows appear in two tables; that duplication is pre-existing |
| **U4** | `internal/core/skill.go:324` (§3.6) and `internal/core/skill_test.go`'s pin, whose current required phrase is AIRA-151's now-false sentence. The replacement phrase pins the `,ceiling-fitted` token, the fitted-vs-clamped distinction, and the narrowed meaning of `E_ADMIT_TOO_LARGE` | leaving the shipped sentence would tell agents a working case cannot work |
| **U5** | `internal/store/admission_insight_test.go` — `TestOOMBranchBasesStayOutsideTheAdmissionAdequacyPopulation` gains §3.4's newly-producible strings, including `estimate:p90-prior,ceiling-fitted`, with the comment stating which are producible and which are retained as forward-defensive negatives | I9. A regression that made a fitted basis evaluable would move a published adequacy number silently |
| **U6** | `.aira/tickets/AIRA-153.md` (status, resolution, gate record); `.aira/tickets/AIRA-150.md` and `.aira/tickets/AIRA-151.md` gain the §3.7 notes. **Neither AIRA-150 nor AIRA-151 is closed or reopened** | deferrals are filed, not silent |

### 7.3 Must stay green, unedited — the false-fail direction

Named explicitly, because "the suite is green" is not evidence unless the tests
that *should* have objected are known. Each was checked against §0.1's boundary.

- `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` and
  `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` (real cgroup,
  `AIRA_REAL_CGROUP=1`) — ceiling `6400507904`, fit `5565659046` → no-op. **The
  single most important green in the set:** AIRA-128's shipped self-heal claim
  and its basis string must not move, and a mis-implementation that fitted
  unconditionally (or fitted the escalation) fails here first.
- The whole of `internal/daemon/admit_saturated_diagnosis_test.go` — the eight
  4 GiB `maximum` declarations and their ten `oomClampedHistory()` call sites,
  the two 8 GiB declarations that use a non-OOM history, and every
  `Required`/`Ceiling`/`Grantable`/contention assertion. Verified in §0.1 (4):
  the hint IS fitted there and the answer is unchanged anyway, because the
  escalation wins either way and the clamp then cuts it to the same ceiling.
- `TestSliceCeilingDoesNotReachTheOOMEscalationClamp` (`sliceceiling_test.go`) —
  64 GiB, headroom 0 → no-op; the AIRA-103 static-ceiling rule is untouched.
- `TestConfineEstimatorAndOOMEscalationClamp` and
  `TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins`
  (`confine_admit_test.go`) — ceilings 60/55/10 GiB → no-op, including the pinned
  arm.
- `TestAnOOMRecordNoLongerChangesWhetherAnOverCeilingEstimateIsClamped` — ceiling
  44 GiB → no-op; AIRA-151's consistency claim is unaffected.
- `internal/runner/confine_reserve_resolution_linux_test.go` — the whole
  `ResolveConfineReserve` table, unedited. **This is where "the client still
  sends the unconditioned default, and all three pinning arms still pin" is
  pinned**, and it is the evidence that this change is daemon-side only.
- `internal/runner/admission_saturated_message_test.go` T13–T17 and AIRA-151's
  `TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis` /
  `TestTooLargeRejectionForAnUnescalatedOverCeilingReserveIsAcceptedByTheClient`
  — client-side renders driven by synthetic payloads; the message is unchanged
  and G3's evidence must stay current.
- `TestConfineRejectedAdmissionCreatesNoScopeAndStartsNoChild` — the terminal
  refusal still creates nothing.
- `internal/daemon/worker_admit_test.go` — worker-admit never calls
  `resolveAdmitReserve`.

### 7.4 Mutation evidence to produce at implement time

Each applied alone in a detached throwaway worktree at the implementation commit,
then reverted; each must go RED, with the exact failing test recorded.

1. **M1 — fit AFTER the resolution instead of before it** (§2.4's counterexample).
   → a dedicated T3 row (`MaxOOMPeak = 800 MiB`, ceiling `1031798784`) RED: the
   answer becomes `897216333`/`fallback:…,ceiling-fitted` instead of
   `1031798784`/`estimate:oom-escalated,ceiling-clamped`. This is the mutant that
   proves the escalation's ceiling is not capped by the fit.
2. **M2 — fit the ordinary estimate as well as the priors** → U1 RED (the
   over-ceiling estimate is admitted instead of refused) and T3(i) RED.
3. **M3 — drop the `,ceiling-fitted` suffix** → T2 and T3 RED. The mutant that
   would have shipped a changed number under an unchanged label.
4. **M4 — remove the `MinPinnedScopeCap` floor** → T1's floor rows and T8 RED.
5. **M5 — fit to the ceiling itself (`min(hint, ceiling)`)** → T1's
   strictly-below rows RED and T5's grant assertion RED (`reserve == ceiling` is
   refused at `current = 4096`). The mutant that reintroduces AIRA-150.
6. **M6 — apply the fit before the pinned return** → T7 RED.
7. **M7 — widen AIRA-151's tie-break to `escalated >= reserve`** → U2's re-based
   `tie` row RED. Carried forward from AIRA-151 so its ruling stays pinned on a
   reachable shape (R7).

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
  because §1.4's AIRA-128 no-op claim and T9's whole existence are load-bearing,
  and a skip is reported as `unevaluated`, never as a pass.

If a job must be stopped, `kill <PID>` the `aira confine` process itself; never
`systemctl --user stop aira.slice`.

---

## 8. Deferrals — filed, not silent

Each gets its own ticket via `aira id` at implement time; **no ID is hand-picked
here**, per `CLAUDE.md`. AIRA-150, AIRA-151 and AIRA-152 already exist and are
updated rather than re-filed.

- **G1 — the self-heal ladder's top rung wedges instead of refusing (R3).** File
  with the exact condition `ceiling/1.5 < MaxOOMPeak < ceiling`, the observation
  that the state persists because the job never runs to record a new peak, and
  the two candidate answers: retarget AIRA-151's clamp to the same fitted
  quantity this ticket introduces (one concept, two call sites), or refuse
  terminally at that rung on the grounds that the escalation's own evidence
  already exceeds the slice. Both are sizing changes on the clamp and need their
  own two-loop. This is AIRA-150's kept route 1, made more reachable.
- **G2 — the admitted-but-certainly-OOM band (§3.3).** A slice whose
  `FIT(ceiling)` is above 1 MiB but below any real job's footprint now admits jobs
  that will be killed. Accepted deliberately: refusing there would require a
  judgement about "how big a real job is", which `AIRA is primitives, not
  judgement` forbids, and the ladder terminates honestly. Filed so a future
  session meets a decision rather than an omission.
- **G3 — ci-shim: a fitted booking with no kernel enforcement (R4).** Record that
  the fit reaches shim mode through the four post-block fallbacks (never the OOM
  branch, which AIRA-121 C10 makes unreachable there), that it is what makes a
  small container usable at all, and that the booking is advisory in that mode by
  design.
- **G4 — AIRA-150 stays open, narrowed differently.** Its ticket gains: priors can
  no longer land on the ceiling (removed route), and the escalation-determined
  route becomes more reachable on small slices (G1). **It must not be closed by
  this merge.**
- **G5 — AIRA-151 stays exactly as shipped, with a narrowed population.** Its
  ticket gains §3.7's note: its rows (e)/(e-malformed) become unreachable for a
  real client, its rule is untouched, and two of its tests were re-based onto the
  routes that remain — with the re-basing itself as the evidence.
- **G6 — the `E_ADMIT_TOO_LARGE` message still names no escape hatch and prints
  raw bytes.** AIRA-151's filed G3, unchanged here. Worth noting on that ticket
  that its population is now narrower and more uniform — a genuine over-ceiling
  ESTIMATE or a pinned request — which makes "pin at or below
  `cap_minus_headroom`" correct advice for the first and wrong for the second, so
  the case analysis it was deferred for is still required.
- **G7 — the agent guide's "A FIRST run … is capped at a machine-wide prior
  (`estimate:p90-prior`)" is imprecise when no p90 exists**, where the true basis
  is `fallback:no-history`. Pre-existing, unrelated to the fit, and named so the
  §3.6 edit is not mistaken for having addressed it.
- **G8 — install-time refusal is NOT taken (§2.1).** Filed with the derivation
  that it cannot establish its property, because the entry ceiling is a function
  of concurrent occupancy — the 8 GiB / 31-job arithmetic — so a future session
  does not re-derive it.
- **G9 — no real-cgroup coverage of the degenerate refusal (T8's shape).**
  Reproducing it against a real cgroup needs a slice with a sub-1.2 MiB ceiling,
  which cannot hold the test harness itself. Accepted coverage gap, of the same
  class as AIRA-149's F7/AIRA-156; T8 drives it deterministically through the real
  wire path with a stubbed reader.
- **G10 — AIRA-152 remains superseded.** Nothing here reopens it; §2.3 records
  that this change refines rather than removes the `max()` rule its rejection
  rested on.

---

## 9. Files touched at implement time

| File | Change |
| --- | --- |
| `internal/runner/resource_estimate.go` | new `FitUnpinnedReserveToCeiling` (§3.2). No existing line changes; `memoryEstimateSafetyPct` and `MinPinnedScopeCap` are read, not moved |
| `internal/daemon/admit.go` | `resolveAdmitReserve` only: the fit + `fitted` local after the pinned return, the `suffix` provenance local in the history block, the suffix on the OOM branch's non-escalated return and on the `SampleCount>=3` return, the p90 fit, and the suffix on the four post-block fallbacks. **No other function in the file changes** |
| `internal/core/skill.go` | the §3.6 clause replaces AIRA-151's now-false sentence |
| `internal/runner/resource_estimate_fit_test.go` | new — T1 |
| `internal/daemon/admit_ceiling_fitted_test.go` | new — T2, T3, T4, T7 |
| `internal/daemon/admit_ceiling_fitted_wire_test.go` | new — T5, T6, T8 |
| `internal/daemon/confine_selfheal_small_slice_real_cgroup_linux_test.go` | new — T9, T10 |
| `internal/daemon/admit_oom_clamp_wire_test.go` | U1 — the fixture re-based onto row (c′) |
| `internal/daemon/admit_oom_clamp_scope_test.go` | U2 — three rows |
| `internal/daemon/admit_oom_basis_test.go` | U3 — two rows |
| `internal/core/skill_test.go` | U4 — the required phrase |
| `internal/store/admission_insight_test.go` | U5 — the enumeration and its comment |
| `.aira/tickets/AIRA-153.md`, `.aira/tickets/AIRA-150.md`, `.aira/tickets/AIRA-151.md` | U6 |

No change to `ResolveConfineReserve`, `checkedAvailable`, the escalation
arithmetic or its overflow guard, AIRA-151's clamp, the ceiling computation, the
`reserve > ceiling` boundary, the enqueue-time re-check, the AIRA-59 freeze, the
AIRA-114 bound, the AIRA-101 gate, the AIRA-103 throttle rule, the reserve
ledger, `resolveDelegateRAMScopeCeiling`, `worker_admit.go`,
`internal/store/admission_insight.go`, the protocol version, any CLI flag, or any
MCP/Skill dispatch surface.

---

## 10. Questions this plan puts to the gate

This ticket has no owner-mandated direction, so these are live, not recorded
rulings.

1. **Is `FIT(c) = ⌊100c/115⌋` the right quantity, or should the margin be
   something else?** The plan's case: it must be strictly below the ceiling
   (AIRA-150, I5), it must not degenerate under configuration (which rules out
   the headroom terms), it must not be a new arbitrary constant (which rules out
   a fixed slack), and it must be a no-op wherever the prior already fits (which
   rules out an unconditional fraction). The 15% inverse satisfies all four and
   introduces no new number. The gate should press on whether "the largest prior
   that leaves the estimator's growth margin inside the ceiling" is a real
   rationale or a post-hoc one, and whether ~13% of the ceiling is enough margin
   against a real idle slice's non-reclaimable residual (the AIRA-139 measurement
   was one 4 KiB page; T10 asserts 328 MiB of slack for the real fixture).
2. **Should the machine-wide `estimate:p90-prior` be fitted too, or only the
   client default?** The ticket names only the default. The plan fits both, on
   the ground that the p90 is a prior about OTHER commands and that AIRA-128's
   cold start is documented as going through it — so leaving it unfitted would
   leave the ticket's headline property broken whenever a box's p90 exceeds a
   small slice's ceiling, which is the ordinary shim shape. If the gate prefers
   the narrower change, T2's p90 row and the §3.1 p90 hunk come out and the gap
   must be filed loudly rather than left implicit.
3. **Is `,ceiling-fitted` warranted, or is the fit better left silent?** The plan
   says the basis must name it (AIRA-149's rule) and that the token costs one
   comma param in an existing grammar. The gate may prefer a different spelling —
   `,slice-fitted` and `,prior-fitted` were both considered — but "no token" must
   be refused explicitly if it is refused, not settled by omission.
4. **Is `MinPinnedScopeCap` the right degenerate floor?** It is this codebase's
   own boundary for a viable declared cap and has a stated precedent
   (`workerAdmitEstimatedBytesMin`). A larger floor would refuse more slices
   earlier and would be a judgement about job size; a smaller one would hand a job
   a `memory.max` that kills it on placement. G2 records the band this choice
   accepts.
5. **Should R3/G1 (the ladder's top rung wedging on a small slice) be fixed
   here?** The plan says no — it is a second sizing change, on AIRA-151's
   deliberately-kept clamp — and files it with both candidate answers. A reviewer
   who thinks the two must ship together should say so at the gate rather than
   after the merge, because the cost is a persistent 30-second dead-end for jobs
   whose peak sits between 2/3 and 1× of a small slice's ceiling.
6. **Is re-basing AIRA-151's wire test onto row (c′), rather than deleting it or
   pinning it as a forward-defensive negative, the right move?** R6/U1 chooses
   re-basing so the claim stays tested end to end on a reachable shape. The
   alternative — keeping the unreachable client-default shape by calling
   `resolveAdmitReserve` directly with a synthetic over-ceiling hint — would keep
   the old numbers but stop testing anything a real client can produce.
