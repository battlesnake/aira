# AIRA-151 — the OOM ceiling clamp applies only where the escalation determined the value

Status: **plan revision 2 — the documentation-only fix round the plan gate
required. No implementation has begun; this commit changes only this document.**

Revision 2 changes **nothing** about the direction, §3.1's code change, the
invariants I1–I8, the population (§1.2), the fixture analysis (§1.3) or the test
plan. Both gate lineages verified the design from source and neither asked for it
to move. What they refused was the *document*: §2.3 understated the accepted
cost, R5 reached a correct conclusion by an obsolete route, §3.5/G3 and §10 Q2
inherited that stale premise, §3.6 mis-described a paragraph, and two figures
(one byte-exact constant, one count) were wrong. The five required edits, and the
one optional one, are recorded at §11 with where each landed.

This changes SIZING on the machine-wide admission gate every `aira confine` job
on this box passes through, so it is the full two-loop per `CLAUDE.md`, not the
light path.

Ticket: `.aira/tickets/AIRA-151.md` (P2, `admission`/`confine`), filed as
AIRA-149 deferral **F2** with its counter-argument. The structural defect it
addresses is `.aira/tickets/AIRA-150.md`; the rejected sibling candidate is
`.aira/tickets/AIRA-152.md`; the deliberately-out-of-scope neighbour is
`.aira/tickets/AIRA-153.md`.

**The direction is the project owner's, taken tonight, and is not re-litigated
here.** The owner was asked to choose between AIRA-151 (narrow the clamp) and
AIRA-152 (replace rather than `max` the escalation) and chose AIRA-151. §2
records why AIRA-152 was not chosen, as history rather than as an open question.

Branch: `aira151-narrow-oom-ceiling-clamp`, from `origin/master` at `16b9141`.

Prior art this plan must not re-litigate: AIRA-149 (the honesty fix that shipped
first and measured this; its plan is
`docs/superpowers/plans/2026-09-07-aira149-admit-oom-clamp-honesty-plan.md`,
sections 0, 2 and 8 in particular), AIRA-128 (the OOM self-heal and its
real-cgroup fixture), AIRA-139 (the flake removed at fixture level), AIRA-103 /
AIRA-106 (the static-vs-throttled ceiling rule), AIRA-101 (exclusivity),
AIRA-114 (the aggregate bound), AIRA-59 (the fairness freeze), AIRA-71 (a
bounded, sound, silent wait is not a correctness bug — the fix is to make it
visible).

**Verdict, up front.** One condition moves: the OOM branch's ceiling clamp is
nested inside the escalation branch, so it applies only when the escalation
strictly raised the reserve. Everything else in `resolveAdmitReserve` is
byte-for-byte unchanged. The entire behavioural consequence is a single
substitution, and §4's I1 makes it structural rather than argued: **every
request whose behaviour changes is one that is now refused terminally with
`E_ADMIT_TOO_LARGE` instead of being clamped onto the ceiling and then almost
always timing out. No request is granted a different reserve, and no job's scope
`memory.max` changes.**

---

## 0. Source read, at the commit this plan was written against

Every line below was read fresh in this worktree at `16b9141` (== `origin/master`).

| Fact | Where | Confirmed |
| --- | --- | --- |
| the OOM branch, post-AIRA-149: `escalated` with its `math.MaxInt64` overflow guard, then `oomBasis := basis + ",oom-on-record"`, then `if escalated > reserve { reserve = escalated; oomBasis = "estimate:oom-escalated" }`, then the clamp, then `return reserve, oomBasis` | `internal/daemon/admit.go:1647-1696` | yes |
| the clamp is `if stats.MaxOOMPeak < ceiling && reserve > ceiling { reserve = ceiling; oomBasis += ",ceiling-clamped" }` and sits **outside** the escalation `if` | `admit.go:1691-1694` | yes — this is the whole subject of this ticket |
| the clamp's stated justification: "An OOM observed at the present ceiling is genuinely too large. Earlier censored caps are allowed to climb to the ceiling so a runnable job is never permanently wedged" | `admit.go:1688-1690` | yes — a rationale about a value **derived from the OOM peak** |
| a pinned request returns at the first line, `pinned:client`, and never reaches the clamp | `admit.go:1609-1611` | yes — `--memory-reserve`, `--memory-max` and `--delegate-ram` each pin (`internal/runner/confine.go:103-119`) |
| an unpinned confine request's reserve is *exactly* `DefaultConfineMemoryReserve` = `4 << 30` | `confine.go:19`, `confine.go:103-119` | yes |
| the ordinary estimate needs ≥ 3 usable samples; `estimate:capped` caps at `MaxMemoryEstimateReserve = 1 << 50`; the estimate is `peak + peak*15/100` | `internal/runner/resource_estimate.go:11`, `:33-41`, `:43-65` | yes |
| `ordinary.OOMCount = 0` is set before the estimator call, so the estimator's own `estimate:oom:` arm is unreachable from this branch | `admit.go:1630-1631` vs `resource_estimate.go:56-60` | yes |
| `admitConnection` computes the request-entry ceiling as `subtractFloor(maximum, admitSliceHeadroom(admitOutstandingJobs(path)+1))`, resolves the reserve against it, and **refuses terminally at `reserve > ceiling`** with `Required`/`Ceiling`/`Basis` populated | `admit.go:1894-1906` | yes — so `ceiling` is the largest *admissible* reserve, and the too-large payload already carries both numbers and the basis |
| the too-large refusal happens **before** `enqueueResolvedConfineAdmit`, so nothing is charged, no waiter exists, and no scope is created | `admit.go:1903-1907` | yes |
| a second, race-free ceiling check under `queue.mu` re-refuses `E_ADMIT_TOO_LARGE` at enqueue time | `admit.go:2054-2056` | yes — unreachable for the affected population after this change, which is refused one call earlier |
| the grant gate is `waiter.reserve > available` with `available = checkedAvailable(current, effectiveMaximum, reclaimable, outstanding+adopted, admitSliceHeadroom(outstanding+adopted+1))` | `admit.go:2459-2461`, `:2484` | yes |
| `checkedAvailable` charges `max(current - reclaimable, outstanding+adopted)` against `effectiveMaximum - headroom` and returns 0 once the charge reaches that ceiling | `admit.go:2622-2639` | yes |
| the ENTRY ceiling and the EVALUATOR ceiling are computed from **different job counts**: entry uses `admitSliceHeadroom(admitOutstandingJobs(path)+1)`, and `admitOutstandingJobs` reads `queue.outstandingJobs` **only** — adopted jobs are excluded; the evaluator uses `admitSliceHeadroom(outstandingJobs + adoptedJobs + 1)` | `admit.go:1894-1896` + `:1276-1288` vs `:2459-2461` | yes — **this asymmetry is why "grantable only at byte-exact-zero charge" is too strong**; §2.3 derives the real inequality |
| `admitSliceHeadroom(n) = base + n × perJob`, with production defaults `base = 2 << 30` and `perJob = 64 << 20` (64 MiB); the AIRA-139/AIRA-149 fixtures override them to 32 MiB / 8 MiB | `admit.go:1079-1089`, `:49-50`; `admit_saturated_diagnosis_test.go:41-42`, `confine_oom_selfheal_real_cgroup_linux_test.go:174-175` | yes — **the production per-job term is 64 MiB, eight times the fixture's**, which is what makes §2.3's residual band material rather than a knife edge |
| `reclaimable` is the file-LRU total, not slab | `admit.go:3106-3122`, `:3151-3161` | yes — so the charge term is `current - file_lru`, not `current` |
| ci-shim mode reports **no peak-RSS and no OOM at all**, by AIRA-121 gate condition C10, so nothing from a shim run ever enters the per-signature estimator; `reportConfinePeak` is wired only on the real path and has zero references in the shim file | `internal/runner/confine_shim_linux.go:469-484`; `confine_linux.go:239` vs `admission_linux.go:768` | yes — **so `stats.OOMCount > 0` is unreachable for a shim signature and the OOM branch cannot execute there** (R5) |
| the normal shim ledger carries a REAL finite budget (own-cgroup `memory.max`, a declared `--memory-max`, or MemTotal), routinely BELOW the 4 GiB unpinned default; the `cap_minus_headroom = 0` state its comments describe is the failure AIRA-121 F1/F3 **fixed**, written in the past tense | `internal/daemon/shim.go:139-201` | yes — **so the "ceiling 0 makes the population unsatisfiable" premise revision 1 used is dead**; R5 and G3 are rewritten off it |
| an unpinned request on a sub-4 GiB shim budget with **no** OOM history is ALREADY refused terminally today: `fallback:no-history` (or `:insufficient-samples`) returns the client's own 4 GiB, which `admit.go:1903` refuses | `admit.go:1703-1718` → `:1903` | yes — so this ticket changes nothing in shim mode, in either direction |
| shim case 3 (booked-reserve-only) reports `current = 0, reclaimable = 0` deterministically | `shim.go:174-197` | yes — noted as a forward caution in R5: if shim history ever fed the estimator, §2.3's "residual charge" term would be zero by construction |
| `--delegate-ram` pins by the `reserve <= 0` arm of `ResolveConfineReserve`, and that derivation is ALREADY pinned by a shipped test row | `internal/runner/confine.go:103-119`; `confine_reserve_resolution_linux_test.go:176-178` (`MemoryReserve: -1, DelegateRAM: true` → `wantPinned: true`) | yes — the identical `reserve <= 0` path a bare `ConfineRequest{DelegateRAM: true}` takes; §7.3 records it as covered rather than adding a row |
| the resolved reserve **becomes a non-delegate job's own hard scope `memory.max`** when no `--memory-max` and no declared reserve was given | `internal/runner/confine_linux.go:966-967`, `:978-979`; the rule is stated at `internal/daemon/sliceceiling.go:27-33` | yes — so the clamp's output is kernel-enforced containment, not only an accounting figure |
| the throttled ceiling is deliberately NOT applied to the clamp or to the terminal boundary | `admit.go:2413-2417`, `sliceceiling.go:27-40` | yes — untouched here; `TestSliceCeilingDoesNotReachTheOOMEscalationClamp` pins it |
| the too-large operator message is `fmt.Sprintf("%s: required=%d cap_minus_headroom=%d basis=%s", …)` — raw byte counts, and it names **no escape hatch** | `admit.go:2799-2801` | yes — see §3.5 and R2; the AIRA-149 saturated sentence *does* name one |
| the client passes that string through unchanged for `E_ADMIT_TOO_LARGE`, sets `basis = "reject:too-large"`, and treats it as terminal (no retry, no scope, no child) | `internal/runner/admission_linux.go:541-552`, `:616` | yes |
| `validRunnerAdmitRejection` requires `Required > 0 && Ceiling >= 0 && Basis != ""` for `E_ADMIT_TOO_LARGE`; an invalid payload drops the client into `fail()` and the flock fallback, launching outside the ledger | `admission_linux.go:727-736` | yes — the newly-routed payloads satisfy it (§4 I5) |
| the AIRA-128 fixture's slice budget is `runner.DefaultConfineMemoryReserve + (2 << 30)`, chosen by AIRA-139 **specifically to keep phase 3 clear of the clamp** | `internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go:104-119` | yes |
| that fixture's phase 3 asserts the basis `fallback:insufficient-samples:n=1,oom-on-record` — row (d), unclamped — and its unit twin asserts the same plus `ceiling - reserve >= oomSelfHealTargetBytes` | same file `:273-276`, `:314-352` | yes — **verified from source, not assumed: the fixture does NOT exercise the clamp at all** (§1.3) |
| AIRA-149's facet-2b fixtures manufacture "resolved reserve == ceiling" **through this very clamp** (`oomClampedHistory()`, a 1 GiB slice, the 4 GiB unpinned default) | `internal/daemon/admit_saturated_diagnosis_test.go:57-63` and its ten call sites (`:243`, `:285`, `:317`, `:346`, `:389`, `:523`, `:553`, `:579`, `:627`, `:655`) | yes — **collateral this plan must handle**; §3.6 and R3 |
| the AIRA-52 gauge's basis matchers are `$`-anchored, so every OOM-branch basis falls to `default:` → `malformedBasis` + `excluded`; `fallback:` bases never enter the population at all | `internal/store/admission_insight.go:17-18`, `:111-122`; `internal/runner/estimate_actual.go:47` | yes — unchanged by this ticket (R4) |
| the agent guide sells `,ceiling-clamped` and the self-heal story to agents; `skill_test.go` pins the tokens | `internal/core/skill.go:324`; `internal/core/skill_test.go:639-660` | yes |

### 0.1 The measured case, re-derived against the code above

AIRA-149's §0 shape, which is AIRA-139's fixture shape (1 GiB slice, 32 MiB +
8 MiB headroom):

```
stats={TotalCount:1 SampleCount:1 PeakMax:56360960 OOMCount:1 MaxOOMPeak:56360960}
request.reserve = 4294967296 (the unpinned default)   ceiling = 1031798784
  ordinary estimate: !ok (1 sample)        -> reserve stays 4294967296
  escalated = 56360960 + 28180480 = 84541440
  84541440 > 4294967296 ?  NO              -> the escalation did NOT determine the value
  clamp today: 56360960 < 1031798784 && 4294967296 > 1031798784
                                           -> reserve = 1031798784  (= the ceiling)
  today:  returns 1031798784, basis "fallback:insufficient-samples:n=1,oom-on-record,ceiling-clamped"
  after:  returns 4294967296, basis "fallback:insufficient-samples:n=1,oom-on-record"
          -> 4294967296 > 1031798784 -> E_ADMIT_TOO_LARGE, immediately, before enqueue
```

The terminal message that case receives after this change, byte for byte from
`admit.go:2800`:

```
E_ADMIT_TOO_LARGE: required=4294967296 cap_minus_headroom=1031798784 basis=fallback:insufficient-samples:n=1,oom-on-record
```

And what it gets **today**, from AIRA-150's measurement: the reserve is placed
exactly on the ceiling, and the grant gate then needs the slice's charge to fall
inside §2.3's residual band — which in that fixture, entered with **nothing
outstanding**, collapses to `max(current - reclaimable, outstanding + adopted)
== 0`. With `current = 4096, reclaimable = 0` it was refused for **107
consecutive evaluator passes (~30 s)** before timing out into AIRA-149's
saturated sentence.

### 0.2 The two rows, named unambiguously

This ticket is easy to state backwards, and at least one framing of it in the
handover was inverted, so the mapping is fixed here and every later section
refers to this table rather than to prose.

| AIRA-149 §3.1 row | Did the escalation determine the value? | Clamp today | Clamp after AIRA-151 |
| --- | --- | --- | --- |
| (a) `escalated > reserve`, no clamp | **yes** | n/a (`MaxOOMPeak >= ceiling`, or the value fits) | **unchanged** |
| (b) `escalated > reserve`, clamped | **yes** | applied → reserve = ceiling | **unchanged — still clamped, still waits** |
| (c) ordinary estimate determined it, under the ceiling | no | n/a | unchanged |
| (c′) ordinary estimate determined it, **over** the ceiling | no | applied → reserve = ceiling | **removed → terminal `E_ADMIT_TOO_LARGE`** |
| (d) client's own unpinned default survived, under the ceiling | no | n/a | **unchanged** — this is the AIRA-128 fixture |
| (e) client's own unpinned default survived, **over** the ceiling | no | applied → reserve = ceiling | **removed → terminal `E_ADMIT_TOO_LARGE`** |

So: **the escalation-determined rows KEEP the clamp; the rows where something
else set the number LOSE it.** The reverse reading — that this ticket makes the
escalation-determined row terminal and leaves the unpinned default clamped — is
the opposite of the ticket, of the owner's decision, and of §3.

**Row (c′) is not in AIRA-149's five-row table, and this plan adds it.** That
table treats "clamped" as a property of rows (b) and (e) only, but the clamp's
condition is `reserve > ceiling` for whatever produced `reserve`, so an ordinary
estimate above the ceiling is clamped exactly as the client's default is. It is
reachable from real store data (≥ 3 samples with an OOM record whose 1.5×
escalation is below the estimate) and it is *already* named in a shipped test:
`internal/store/admission_insight_test.go:136,138` enumerate
`estimate:max=…,oom-on-record,ceiling-clamped` and
`estimate:capped,oom-on-record,ceiling-clamped` as strings the OOM branch can
produce. The under-enumeration was harmless for AIRA-149 (a labelling change
covers both) and is load-bearing here (a sizing change must know its whole
population), which is why it is corrected rather than inherited.

---

## 1. What changes, stated as one defect and one exact population

### 1.1 The defect

The clamp's own justification — *"earlier censored caps are allowed to climb to
the ceiling so a runnable job is never permanently wedged"* (`admit.go:1688`) —
is a statement about a value **derived from the OOM peak**: a job whose previous
cap was censored below what it needs should be allowed up to the largest cap the
slice can express. It says nothing about the blind unpinned 4 GiB client default,
which has no relationship to the OOM at all, and nothing about an ordinary
peak-history estimate, which the no-OOM path already refuses when it exceeds the
ceiling.

The result is an inconsistency inside one function. Two signatures with the
**identical** peak history and the identical over-ceiling resolved value are
treated differently solely because one of them also carries an OOM record:

- **without an OOM record** → `admit.go:1697-1699` returns the estimate
  unclamped → `admit.go:1903` refuses terminally with `E_ADMIT_TOO_LARGE`,
  naming `required`, `cap_minus_headroom` and the basis;
- **with an OOM record** → the clamp cuts the value to exactly the entry ceiling
  → the request is admissible but, per AIRA-150, grantable only while the slice's
  charge stays inside §2.3.1's narrow residual band → it usually waits out its
  whole window and is then refused anyway, with a longer message and 30 s later.

The OOM record makes the outcome *worse*, on the self-heal path, which is the
path the escalation exists to serve.

### 1.2 The exact population that changes

A request's behaviour changes if and only if, inside the OOM branch
(`stats.OOMCount > 0 && stats.MaxOOMPeak > 0`, unpinned):

```
escalated <= reserve            (the escalation did not strictly raise it)
  AND  reserve > ceiling        (the clamp fires today)
  AND  stats.MaxOOMPeak < ceiling   (the clamp's existing guard)
```

Nothing else. Pinned requests, delegate-ram requests, every no-OOM path, every
post-block fallback, and every row where the escalation won are untouched.

Because the clamp sets `reserve = ceiling` **exactly**, this population is
precisely AIRA-150's population: today every one of these requests is admitted
with a reserve equal to the *entry* ceiling, and is then grantable only inside the
narrow residual band §2.3 derives — which is byte-exact-zero charge whenever the
request entered a slice with no outstanding jobs, and a small multiple of the
per-job headroom term otherwise.

Two derivations that must not be inferred, and are established from source in §0
instead:

- **Pinned and `--delegate-ram` requests never reach the clamp**, because
  `ResolveConfineReserve` (`confine.go:103-119`) sets `pinned = true` for a
  declared `--memory-reserve`, for `--memory-max` (`ScopeMemoryMax > 0`) and for
  `--delegate-ram` (via the `reserve <= 0` arm, which takes the 512 MiB pinned
  overhead), and `admit.go:1609-1611` returns `pinned:client` at the first line.
  The delegate-ram half of that is not an assumption: it is pinned by a shipped
  test row, `confine_reserve_resolution_linux_test.go:176-178`, which drives the
  identical `reserve <= 0` path and asserts `wantPinned: true` (§7.3).
- **ci-shim mode cannot reach this branch at all**, because AIRA-121 gate
  condition C10 reports no peak-RSS and no OOM from a shim run, so a shim
  signature never carries `OOMCount > 0`. R5 states this and the shim-specific
  caution that goes with it.

### 1.3 The AIRA-128 real-cgroup fixture does **not** exercise this — verified, not assumed

The ticket brief asked for this to be established from source rather than
inferred, because the answer decides whether AIRA-128's shipped self-heal claim
moves. It does not.

`TestOOMSelfHealFixtureStaysOffTheCeilingClamp`
(`confine_oom_selfheal_real_cgroup_linux_test.go:314-352`) drives
`resolveAdmitReserve` with `TotalCount: 1, SampleCount: 1, PeakMax: 60 MiB,
OOMCount: 1, MaxOOMPeak: 60 MiB`, a request reserve of
`runner.DefaultConfineMemoryReserve` (4 GiB), and
`ceiling = oomSelfHealSliceMax - admitSliceHeadroom(1)` where
`oomSelfHealSliceMax = DefaultConfineMemoryReserve + (2 << 30)` = 6 GiB. So:

- the escalation is `90 MiB`, far below the 4 GiB default → **row (d)**, the
  escalation did not determine the value;
- the clamp's `reserve > ceiling` term is `4 GiB > (6 GiB - 40 MiB)` → **false**,
  so the clamp does not fire and never has since AIRA-139 sized this constant to
  keep it from firing;
- the test asserts exactly `fallback:insufficient-samples:n=1,oom-on-record`
  (no `,ceiling-clamped`) and `ceiling - reserve >= 320 MiB`.

The real-cgroup fixture
`TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` phase 3
(`:273-276`) asserts the same basis through the whole kernel path, plus
`ScopeMemoryMax >= 1.5 × oomPeak` and "the second run succeeds".

**Conclusion: phase 3 lands on the still-unpinned-default row that is UNDER the
ceiling — row (d), which this ticket does not touch.** Both tests stay green
with no edit, and AIRA-128's "the very next run succeeds" claim is unchanged.
(Contrast AIRA-152, which would have re-tuned this fixture and restated that
claim as a convergence ladder — see §2.2.)

---

## 2. Directions

### 2.1 The chosen direction (owner's decision, recorded not re-argued)

Apply the clamp only where the escalation determined the value, making the OOM
path consistent with the no-OOM path's existing terminal `E_ADMIT_TOO_LARGE`.
This is AIRA-149 §2.3's "fourth direction", filed as F2 with its
counter-argument precisely so it would get its own two-loop rather than being
merged inside an honesty fix. It now has one.

### 2.2 Why AIRA-152 was not chosen (history, for the reader's orientation)

AIRA-152 would make the escalation a **replacement** rather than a `max` with the
unpinned default when no usable ordinary estimate exists. In AIRA-149's measured
example it sizes the next attempt at 84 MiB instead of 4 GiB for a job that needs
~320 MiB, so the self-heal converges over several more OOM kills instead of one;
it requires restating AIRA-128's shipped "the very next run succeeds" claim as a
ladder and re-tuning its real-cgroup fixture; and the current `max()` encodes a
defensible rule — *never size the next attempt below what an unpinned client
asked for*. The owner chose AIRA-151. AIRA-152 stays open as a filed candidate;
this plan does not close it and does not pre-empt it. The two are compatible:
AIRA-152, if ever taken, would shrink the population §1.2 defines by making the
resolved value smaller, not by changing what the clamp does.

### 2.3 The accepted cost, quantified

The ticket states the cost as "it converts an occasionally-grantable wait into a
CERTAIN terminal refusal for a job … which today can still be granted on a quiet
slice". That is the right shape but not the exact figure, and a sizing change on
shared infrastructure deserves the exact figure rather than the comfortable one.
**Revision 1 of this plan gave a figure that was too small in the direction that
understates what is given up, and both gate lineages refused it. This is the
re-derivation.**

#### 2.3.1 The exact grant condition, derived

Every request in §1.2's population is, today, resolved to a reserve **exactly
equal to the request-ENTRY ceiling**. Write the two ceilings out, because they
are not the same expression and that is the whole correction:

```
entry      (admit.go:1894-1896, admitOutstandingJobs at :1276-1288)
  J_entry   = queue.outstandingJobs at arrival        # ADOPTED JOBS EXCLUDED
  ceiling   = maximum - ( base + (J_entry + 1) * perJob )

evaluator  (admit.go:2459-2461, :2484; checkedAvailable at :2622-2639)
  J_eval    = outstandingJobs_now + adoptedJobs_now + 1     # adopted INCLUDED
  charge    = max( current - reclaimable , outstanding + adopted )
  available = ( effectiveMaximum - ( base + J_eval * perJob ) ) - charge   # floored at 0
  granted  iff  reserve <= available
```

Substituting `reserve = ceiling` and cancelling `base`, the request is granted on
some evaluator pass iff

```
charge  <=  (effectiveMaximum - maximum)
            + perJob * ( J_entry - outstandingJobs_now - adoptedJobs_now )
```

Three consequences, each of which revision 1 got wrong or omitted:

1. **The "byte-exact zero" reading is the special case `J_entry == 0`.** It is
   correct only for a request that entered a slice with nothing outstanding —
   which is exactly the AIRA-139/AIRA-149 fixture shape, and is why the wrong
   generalisation survived revision 1. A request that entered *behind* N
   outstanding jobs which have since drained is grantable while carrying up to
   `N * perJob` of residual non-reclaimable charge.
2. **On production `perJob` is 64 MiB, not the fixture's 8 MiB**
   (`admit.go:50` `admitSliceHeadroomSupervisorDefault = 64 << 20`; base 2 GiB at
   `:49`). So on a slice a request entered behind twelve outstanding jobs, the
   residual band is up to ~768 MiB of charge — **that is not a knife edge**, and
   describing it as one was the understatement.
3. **The AIRA-103 throttle term subtracts directly.** `effectiveMaximum <
   maximum` while a throttle is published, so a throttle shrinks the band by the
   full published amount and can close it outright. Revision 1 had this right and
   it is unchanged.

The adopted term is asymmetric and worth naming on its own: adopted jobs are
excluded from the entry ceiling but charged at evaluation, so an adoption that
lands between arrival and the pass makes the band *smaller* than it was at
arrival, never larger.

#### 2.3.2 The three environments, re-derived from that inequality

- **The production 64 GiB `aira.slice`:** the conclusion of revision 1 survives,
  but for a different and stronger reason than the one it gave. The band is
  **not** vanishing there — it is up to `perJob * J_entry` = 64 MiB per job the
  request entered behind, which on a busy slice is hundreds of MiB. What closes
  production is the **population**, not the band: reaching §1.2 on a 64 GiB slice
  needs an unpinned resolved reserve above the ~62 GiB ceiling — an ordinary
  estimate over ~62 GiB (peak history above ~54 GiB) or the 4 GiB default over a
  62 GiB ceiling, the latter impossible — *and* an OOM peak below that ceiling.
  That set is essentially empty. **No availability is lost on production because
  essentially no production request is in the population, not because the grant
  window there is shut.**
- **A small or fixture slice** (a CI slice, a test fixture, a small `aira.slice`
  install): here the population is easy to reach — every unpinned request on a
  slice smaller than 4 GiB whose signature carries an OOM record — and the band
  is open whenever the slice is quiet at the entry count the request arrived
  under. For a freshly created fixture slice (`J_entry == 0`) that reduces to the
  byte-exact-zero case AIRA-139 measured as a coin flip. Here the change is real:
  such a job now **cannot run at all** without `--memory-reserve` /
  `--memory-max`, where today it sometimes could.
- **The sharpest form of that**, stated plainly rather than buried: on a slice
  whose ceiling is below the 4 GiB unpinned default, AIRA-128's cold-start
  self-heal (`terminated-by=oom` → "re-run the identical command") stops working
  for that command. Today the re-run is admitted iff the slice's charge falls
  inside the band at some pass; after this change it is refused terminally every
  time until the operator pins a reserve. That is a genuine regression in that
  environment, and it is accepted because the alternative it replaces is a
  30-second wait that ends in a refusal anyway on any slice outside the band,
  because the first run of a *novel* command on such a slice is **already**
  refused terminally by the identical `reserve > ceiling` boundary (AIRA-153),
  and because the underlying fault is the unconditioned default, not the clamp
  (§3.7).

#### 2.3.3 The transient-boundary cost, which revision 1 omitted entirely

The entry ceiling is **itself** a function of how many jobs happened to be
outstanding when the request arrived: it moves by `perJob` — 64 MiB on production
— for each one. So a request whose unclamped reserve falls inside that moving
band gets a *different answer depending on the slice's momentary occupancy at
arrival*, and after this change that answer is a **terminal**
`E_ADMIT_TOO_LARGE` whose `cap_minus_headroom` is a transient rather than a
property of the request.

This matters because of what the agent guide already says. `skill.go:323` tells
agents that `too_large` "means the request itself cannot be satisfied as written,
so change the reserve or the wait rather than retrying it unchanged" — correct
advice for a genuinely oversized request, and misleading for one that was 64 MiB
over a ceiling that will be 64 MiB higher once a neighbour finishes. Today that
request is clamped and can be granted after the drain; after this change it is
refused with advice not to retry.

This is **already true of the no-OOM path** — the identical `reserve > ceiling`
boundary at `admit.go:1903` refuses an ordinary estimate the same way, against
the same transient ceiling — so the change does not invent the defect. It is
**new for the OOM path**, and it is named here rather than discovered later. The
mitigation available in this ticket is one sentence of guide text (§3.6), which
is written to *reinforce* `skill.go:323` rather than contradict it: pin at or
below the printed `cap_minus_headroom`, or run where the slice is larger. A
message that distinguishes a transient ceiling from a permanent one is G3's
business, not this ticket's.

Two costs the change **removes**, for completeness of the ledger:

- A job that wins today's residual band is granted a scope
  `memory.max` equal to the **whole slice ceiling** (`confine_linux.go:978-979`)
  — a job that just OOM-killed at 56 MiB gets the entire slice as its own hard
  cap. That stops happening.
- A 30-second dead-end on a shared box becomes an immediate, terminal answer
  carrying `required`, `cap_minus_headroom` and the basis, which is the AIRA-71
  outcome one surface further along.

### 2.4 Rejected sub-options

- **Clamp to `checkedAvailable` instead.** Refused on the documented invariant at
  `admit.go:2413-2417`: the clamp's output sizes a job's own kernel-enforced
  `memory.max`, so deriving it from the slice's transient charge is precisely
  what AIRA-103 forbids. Unchanged from AIRA-149 §2.2.
- **Clamp to `ceiling` minus a slack.** An arbitrary constant; the simplicity
  rule refuses it, and it would leave AIRA-150 intact for rows (a)/(b) anyway.
- **Narrow the clamp *and* condition the unpinned default on the ceiling in the
  same change.** That is AIRA-153, a second sizing decision with its own blast
  radius; merging two sizing changes into one two-loop is how a sizing regression
  gets attributed to the wrong cause. §3.7 states exactly what it leaves open.
- **Keep the clamp but refuse early when the wait cannot succeed.** The daemon
  cannot establish that (a reclaimable page may go away), and AIRA-149 §2.4
  already refused it: replacing a fabricated cause with a fabricated certainty is
  worse.

---

## 3. Design

### 3.1 The change: nest the clamp inside the escalation branch

`admit.go:1684-1694` becomes:

```go
oomBasis := basis + ",oom-on-record"
if escalated > reserve {
    reserve = escalated
    oomBasis = "estimate:oom-escalated"
    // AIRA-151. The clamp lives INSIDE this branch, which is the whole change.
    // Its justification -- "earlier censored caps are allowed to climb to the
    // ceiling so a runnable job is never permanently wedged" -- is about a value
    // DERIVED FROM THE OOM PEAK. It does not apply to the blind unpinned client
    // default, nor to an ordinary peak-history estimate: for those the no-OOM
    // path already refuses an over-ceiling value terminally with
    // E_ADMIT_TOO_LARGE (admit.go:1903), naming both numbers, and an OOM record
    // must not make the same number behave differently. A clamped reserve is
    // exactly the ENTRY ceiling, and such a reserve is grantable only while the
    // slice's charge stays inside a band of one per-job headroom term per job the
    // request entered behind -- byte-exact zero when it entered an empty slice
    // (AIRA-150) -- so what the clamp bought those rows was usually a wait that
    // ends in a refusal anyway, not a run.
    if stats.MaxOOMPeak < ceiling && reserve > ceiling {
        reserve = ceiling
        oomBasis += ",ceiling-clamped"
    }
}
return reserve, oomBasis
```

Nesting rather than adding an `escalationDetermined` boolean is deliberate and is
the data-model-first form: it makes "clamped without the escalation having set
the value" **unrepresentable** rather than merely untrue, so a later edit cannot
reintroduce it by touching a flag. It also keeps one condition — `escalated >
reserve` — governing both the basis and the clamp, so the label and the value can
never disagree about which term acted.

`escalated`'s overflow guard, the `max()` semantics, the clamp's own
`MaxOOMPeak < ceiling` guard, the tie-break, `,oom-on-record`, D4's kept
estimator basis and every post-block fallback are untouched.

### 3.2 The tie-break now decides a value, not only a label

AIRA-149 stated the tie-break for the *basis*: the escalation is deemed to have
determined the value only when it **strictly raised** it (`escalated > reserve`),
and on an exact tie the source term's basis is reported. Nesting the clamp inside
that same condition promotes the tie-break from a labelling rule to a **sizing**
rule: on an exact tie the clamp no longer applies, so an over-ceiling reserve is
refused rather than clamped.

This is not hypothetical-only. `escalated = MaxOOMPeak + MaxOOMPeak/2` equals the
unpinned default exactly at `MaxOOMPeak = 2863311531`
(`2863311531 + 1431655765 = 4294967296`), so a production unpinned request can
land on it.

The plan keeps the strict comparison, for three reasons: it is the existing
condition and changing it would be a second, unrequested behaviour change; on a
tie the escalation raised *nothing* — the number was already there, so the honest
reading is that the client's own request determined it, which is exactly what
AIRA-149's basis says; and one condition governing both label and value is the
property §3.1 exists to get. It is pinned by a test row (§7 T1 row `tie`) and
raised for the gate in §10.

### 3.3 What does not change

`reserve` is computed by the identical statements in the identical order for
every row where the clamp did not fire, and — because a skipped clamp always
leaves `reserve > ceiling`, which is refused before enqueue — **no request that
is granted receives a different number than it does today** (§4 I1). The
estimator, the p90 prior, the post-block fallbacks, `checkedAvailable`, the
`reserve > ceiling` boundary itself, the enqueue-time re-check, the AIRA-59
freeze, the AIRA-114 bound, the AIRA-101 exclusivity gate, the AIRA-103 throttle
rule, the reserve ledger, the wire protocol, every CLI flag and every MCP/Skill
surface are untouched.

### 3.4 What the affected request now receives

At `admit.go:1903-1906` the daemon writes
`admitRejection{Required: reserve, Ceiling: ceiling, Basis: basis}` with
`CodeAdmitTooLarge`, which:

- satisfies `validRunnerAdmitRejection` (`Required > 0`, `Ceiling >= 0`,
  non-empty `Basis`), so the client takes the terminal branch and never falls
  through to `fail()` and the flock fallback — §4 I5, pinned by T6;
- renders as the §0.1 message, carrying both numbers and the OOM-attributed
  basis;
- creates no scope and starts no child
  (`TestConfineRejectedAdmissionCreatesNoScopeAndStartsNoChild`), records
  `reject:too-large` as the run's admission basis, and returns immediately rather
  than after the wait window.

### 3.5 The one thing the affected request loses, and what this plan does about it

The ticket calls the no-OOM path's refusal "an honest, immediate, actionable
message carrying both numbers". Read against source, two of those three are
exact and the third is thinner than the sentence AIRA-149 just built for the
saturated path:

```
today  (saturated, after AIRA-149):
  E_ADMIT_SATURATED: confine: admission rejected after 30s — nothing else was
  running in this slice or queued ahead of this request at any evaluation; the
  resolved reserve 984M did not fit the admission ceiling 984M (largest grantable
  reserve 1007612K at the last evaluation). Pin --memory-reserve or --memory-max
  to size this job yourself.

after  (too-large, admit.go:2800, unchanged by this plan):
  E_ADMIT_TOO_LARGE: required=4294967296 cap_minus_headroom=1031798784 basis=fallback:insufficient-samples:n=1,oom-on-record
```

Both numbers and the basis are there; the escape hatch is not, and the figures
are raw bytes rather than `FormatConfineBytes` renderings.

**This plan does not change that message**, and the reason is not scope
squeamishness. The string is shared with every other `E_ADMIT_TOO_LARGE`,
including a **pinned** request that asked for more than the slice holds — where
"pin `--memory-reserve`" is precisely the wrong advice, since the operator
already did, and the honest advice is the opposite one (lower it, or run
elsewhere). A second case, added in revision 2 from §2.3.3, is a request whose
`cap_minus_headroom` is a **transient**: the entry ceiling moves by 64 MiB per
outstanding job at arrival, so an escape-hatch clause must not tell an operator
the number is a property of the slice when it is a property of the moment.
Making one clause correct for all three needs a case analysis inside a message
renderer, which is machinery this ticket has no business adding on top of a
sizing change. It is filed as **G3** with the exact strings and both
complications, and T5 pins the message this ticket actually routes traffic onto
so the follow-up starts from evidence.

*(Revision 1 justified this partly on ci-shim mode "answering
`cap_minus_headroom=0` for a mis-sized container's entire life". That premise is
dead: the two comment blocks it cited — `shim.go:161`, `:186` — describe, in the
past tense, the failure AIRA-121 F1/F3 **fixed**. The clause is removed rather
than restated, so G3's successor does not start from a false constraint. The
pinned-request reason stands on its own and is sufficient.)*

**What this plan does do instead** is fix the actionability where it costs one
sentence and no branching: the generated agent guide (§3.6). The guide is this
repo's authoritative live surface — AIRA-158 closed the equivalent question for
the dated AIRA-67 spec by saying exactly that — and it is where an agent
following "RE-RUN the identical command" will otherwise meet an unexplained
terminal refusal.

### 3.6 The agent guide

`internal/core/skill.go:324`'s self-heal paragraph contains the sentence "A
trailing `,ceiling-clamped` means the slice's admission ceiling cut the result
down." That sentence is **mid-paragraph**, not its ending: it is followed by "So
an OOM token in the basis is the proof the kill was attributed to this command's
own signature…" and then by the paragraph's actual conclusion, "So the response
to `terminated-by=oom` on a first run is to RE-RUN the identical command". *(A
placement claim revision 1 got wrong, and it matters, because the new clause has
to sit between the `,ceiling-clamped` sentence and that "RE-RUN" advice — the
exact advice it qualifies.)*

After this change the `,ceiling-clamped` sentence is still true but incomplete in
the one place it matters: it does not say what happens when the ceiling cannot
accommodate the number at all, which is now a terminal refusal rather than a
wait, and the paragraph then tells the agent to re-run. One clause is added,
immediately after that sentence and before the "So an OOM token…" sentence,
saying:

- `,ceiling-clamped` appears only on the escalated value — the slice ceiling caps
  how far the OOM escalation may climb;
- when the reserve the daemon resolves is above the ceiling and did **not** come
  from the escalation, the run is refused immediately with `E_ADMIT_TOO_LARGE`
  naming `required` and `cap_minus_headroom`, instead of waiting — so on a slice
  that small, the "RE-RUN the identical command" advice below does not
  self-heal;
- the action is the one `skill.go:323` already gives for `too_large` — change the
  request rather than retrying it unchanged: pin `--memory-reserve` (or
  `--memory-max`) at or below the printed `cap_minus_headroom`, or run where the
  slice is larger.

The clause is written to **reinforce** `skill.go:323`'s existing `too_large`
guidance rather than contradict it: 323 says a `too_large` request cannot be
satisfied as written, and this clause names the one new way an agent can arrive
there and what to pass instead. It deliberately does **not** tell the agent the
ceiling might rise if it waits, even though §2.3.3 establishes that it can — that
distinction needs the message itself to carry it, which is G3.

`skill_test.go`'s existing token pins stay, plus one new pin (T7), so the claim
cannot rot silently. No other surface's wording changes.

### 3.7 What this does **not** close: AIRA-153, and AIRA-150's residue

Two neighbours stay open, and a reader must not finish this plan believing
otherwise.

**AIRA-153 (the unpinned 4 GiB default is unconditioned by the slice ceiling) is
untouched and, after this ships, strictly more visible.** `ResolveConfineReserve`
still hands the daemon exactly `DefaultConfineMemoryReserve` for every unpinned
request regardless of how large the slice is. On any slice whose ceiling is below
4 GiB that value is over the ceiling, and:

- **before** this change: a signature with **no** OOM record was refused
  terminally (returned unclamped at `admit.go:1697-1699`, refused at `:1903`),
  while a signature **with** one was clamped
  onto the ceiling and then usually wedged — two different outcomes for one
  defect;
- **after** this change: both are refused terminally, immediately, with both
  numbers. The defect is unchanged; its consequence becomes uniform, certain and
  immediate rather than sometimes-wedged.

So AIRA-151 removes the last accidental mitigation of AIRA-153 and sharpens the
question AIRA-153 already framed — *what should a small slice do with a client
that has no idea how big it is* — without answering it. The candidate answers
(condition the default on the ceiling; make the daemon publish the admissible
size; refuse at install time) all remain open and all remain sizing decisions
needing their own two-loop.

**AIRA-150 is narrowed, not closed.** A resolved reserve exactly equal to the
entry ceiling is still grantable only inside §2.3.1's residual band, and after
this change three routes still produce one:

1. rows (a)/(b) — the escalation determined the value and the clamp cut it to the
   ceiling: **deliberately kept**, since that is the case the clamp's rationale
   actually covers;
2. a pinned `--memory-reserve` / `--memory-max` exactly equal to the ceiling;
3. an ordinary estimate or client default that happens to equal it exactly.

What this ticket removes is the **systematic** route — the one that fires for
every unpinned request on a slice smaller than the default, which is what made
AIRA-150 "a certainty on a small slice". AIRA-150's own resolution note must say
this rather than being closed on the strength of this merge.

---

## 4. Invariants

**I1 — no granted reserve changes anywhere, and no job's scope `memory.max`
changes.** Structural, not argued: the clamp only ever fires when
`reserve > ceiling`; skipping it therefore leaves `reserve > ceiling`; and every
`reserve > ceiling` is refused at `admit.go:1903` **before** enqueue. So every
request whose resolution differs from today's ends in `E_ADMIT_TOO_LARGE` and is
never granted, never charged to the ledger, and never given a scope. The only
behavioural change in the whole system is *grant-or-wait → immediate terminal
refusal*, for exactly §1.2's population. Pinned by T1 (values for every row) and
T3 (the wire path).

**I2 — no request is ever granted a LARGER reserve than today.** A corollary of
I1 worth stating separately because it is the safety direction: the change can
only raise the returned number, and a raised number above the ceiling is refused
rather than admitted. Nothing gets a bigger hard cap, and the slice can never be
over-committed by this change.

**I3 — rows (a)/(b) are byte-identical.** The escalation, its overflow guard, the
clamp arithmetic and `estimate:oom-escalated[,ceiling-clamped]` are unchanged for
every input where `escalated > reserve`. Pinned by T1 rows (a)/(b), by the
unchanged `TestConfineEstimatorAndOOMEscalationClamp`, and by the unchanged
`TestSliceCeilingDoesNotReachTheOOMEscalationClamp`.

**I4 — AIRA-128's self-heal is unchanged in behaviour and in evidence.** Its
fixture is row (d) under the ceiling (§1.3), so neither the real-cgroup test nor
its unit twin is edited, and "the very next run succeeds" is not restated. This
is the invariant AIRA-152 could not have held.

**I5 — the newly-routed rejection is a VALID one.** `Required > 0`,
`Ceiling >= 0` and a non-empty `Basis` are all satisfied at `admit.go:1904`, so
`validRunnerAdmitRejection` accepts it and the client takes the terminal branch.
A regression here would drop the job into the flock fallback and launch it
**outside the ledger** — the loudest failure available in this subsystem, and the
reason it gets its own test (T6) rather than an argument.

**I6 — the basis vocabulary does not gain or lose a token.**
`,ceiling-clamped` keeps its exact spelling and meaning ("the slice's admission
ceiling cut the result down"); it simply becomes reachable only in company with
`estimate:oom-escalated`. `,oom-on-record` is unaffected, so AIRA-128's
attribution proof (AIRA-149 §3.2) is untouched.

**I7 — no new state, no new field, no new wire value, no protocol change.** The
diff is one brace level in one function, plus tests and one guide paragraph.

**I8 — the AIRA-52 gauge's evaluable population does not move.** Every OOM-branch
basis still falls to the classifier's `default:` arm, exactly as AIRA-149's T2b
pins; this change alters which rows are *produced*, never how a produced row is
classified. R4.

---

## 5. Risks

**R1 — availability on a small idle slice.** §2.3.1 states the exact grant
inequality this closes, §2.3.2 the three environments and where each stands, and
§2.3.3 the transient-boundary cost that comes with routing traffic onto a
terminal refusal. Accepted; it is the owner's decision and the ticket's own named
counter-argument. Mitigations that exist already: `--memory-reserve` and
`--memory-max` both pin and bypass resolution entirely, and both are named in the
guide clause added by §3.6.

**R2 — the terminal message is less actionable than the saturated one it
replaces.** Measured in §3.5, not asserted. Deferred as **G3** with its exact
strings and the ci-shim `cap_minus_headroom=0` complication that makes it a real
decision rather than a one-liner; partially mitigated in this change by the guide
clause (§3.6). A reviewer who thinks the message must move with this ticket
should say so at the gate — §10 Q2 — rather than after the merge.

**R3 — AIRA-149's own facet-2b fixtures are built on the shape this removes, and
will break.** `oomClampedHistory()`
(`admit_saturated_diagnosis_test.go:57-63`) manufactures "resolved reserve ==
ceiling" through **this clamp**: a 1 GiB slice, the 4 GiB unpinned default, an
OOM peak of 56360960 — row (e) exactly. After this change that request is refused
at entry and never enqueues, so all ten call sites (`:243`, `:285`, `:317`,
`:346`, `:389`, `:523`, `:553`, `:579`, `:627`, `:655`) would fail in
`startSaturatedAdmit`'s "no waiter appeared" loop rather than on their own
assertions — a failure mode that says nothing about what they test.

They must be **re-based onto row (b)**, which still clamps, so that every
assertion (`Required == ceiling`, `Grantable == ceiling - 4096`, the contention
lattice, the exclusivity and freeze arms) survives unchanged and the fixtures
stay inside the OOM branch with `resolved != requested`. The algebra the
implementer needs, so it is not re-derived under time pressure: a row-(b) clamp
requires `MaxOOMPeak < ceiling < 1.5 × MaxOOMPeak` **and**
`1.5 × MaxOOMPeak > request`, which with the 4 GiB unpinned default is
unsatisfiable on a 1 GiB slice — hence the fixture slice must grow, not the
history alone. The plan's choice:

```
maximum      1 << 30  ->  4 << 30        (the EIGHT `const maximum` declarations named in §9)
ceiling      1031798784 -> 4253024256    (= 4294967296 - 41943040; 4 GiB - 32 MiB - 8 MiB, unchanged formula)
MaxOOMPeak   56360960 -> 3758096384      (3.5 GiB; PeakMax likewise, TotalCount/SampleCount 1)
escalated    5637144576 > 4294967296     -> row (b), escalation determined
clamp guard  3758096384 < 4253024256     -> fires, reserve = ceiling
request      runner.DefaultConfineMemoryReserve, UNCHANGED (the production unpinned value)
```

*(Revision 1 printed the ceiling as `4253023744`, 512 bytes low — a
mis-evaluation of the right formula, since the 1 GiB figure `1031798784 =
1073741824 - 41943040` is correct. Corrected here because this table is presented
as a byte-exact target the implementer is told not to re-derive. Note also that
the tests **derive** `run.ceiling` from `maximum` via `subtractFloor(maximum,
server.admitSliceHeadroom(1))` (`admit_saturated_diagnosis_test.go:120`, `:286`,
`:631`, `:659`) — so this figure must NOT be pasted into an assertion; it exists
only so the implementer can check the row-(b) arithmetic holds.)*

`current = 4096` still yields `grantable = ceiling - 4096`; T8b's
`current = ceiling` still yields a measured 0; the seeded waiters in T12c/T12d
derive their reserves from the fixture's own ceiling and scale automatically; the
holders' 64 MiB is still enough to refuse a ceiling-sized reserve. The helper's
doc comment must be rewritten to say that the AIRA-149 §0 measured shape is no
longer reachable **because this ticket refused it**, and that the fixture now
drives the row that still clamps — which is itself the executable evidence that
AIRA-150's systematic route is gone. T4 covers this.

**R4 — the ledger and the AIRA-52 gauge.** Runs that used to be granted inside
§2.3.1's residual band (and to record `estimate:…,oom-on-record,ceiling-clamped`
or `fallback:…,ceiling-clamped`) no longer happen, and runs that used to end
`reject:saturated` now end `reject:too-large`. No basis string changes spelling
and no classifier changes, so no published number moves except by the
disappearance of rows: `fallback:` bases were never in the `estimate%` population
at all, and `estimate:…,ceiling-clamped` rows were counted `malformed_basis` and
excluded, so the effect is a small reduction in an exclusion count. Benign, and
the same direction AIRA-149's R2 established. I8; pinned by the unchanged
`TestOOMBranchBasesStayOutsideTheAdmissionAdequacyPopulation`, whose **comment**
must be corrected (T4b) because four of its ten enumerated strings become
unreachable while remaining correct as forward-defensive negatives.

**R5 — ci-shim mode.** Unaffected, but **not for the reason revision 1 gave**,
and the correction matters because the wrong reason would license a real
regression later.

*The dead premise.* Revision 1 argued that "a shim ledger with
`cap_minus_headroom = 0` already refuses every request terminally … §1.2's
population requires `MaxOOMPeak < ceiling`, which is unsatisfiable at ceiling 0".
That state is one AIRA-121 F1/F3 **deliberately eliminated**. `shim.go:139-201`
gives the normal shim ledger a real finite budget — the container's own cgroup
`memory.max`, a declared `--memory-max`, or MemTotal — routinely **below** the
4 GiB unpinned default, i.e. exactly §1.2's shape. The two comment blocks
revision 1 cited (`shim.go:161`, `:186`) describe the permanent-zero failure in
the **past tense**, as the thing that was fixed.

*The actual reason, which is stronger and structural.* **ci-shim mode reports no
peak-RSS and no OOM at all**, by AIRA-121 gate condition C10
(`internal/runner/confine_shim_linux.go:469-484`), and `reportConfinePeak` is
wired only on the real path (`confine_linux.go:239`; zero references in the shim
file). So `stats.OOMCount > 0` is never true for a shim-mode signature and **the
entire OOM branch — clamp included — is unreachable there**. Narrowing a branch
that cannot execute changes nothing.

*And nothing was mitigating shim mode anyway.* An unpinned request on a sub-4 GiB
shim budget is **already** refused terminally today, on the no-OOM path: with no
history the resolution falls through `admit.go:1703-1718` to
`fallback:no-history` (or `fallback:insufficient-samples`) returning the client's
own 4 GiB, which `admit.go:1903` refuses with `E_ADMIT_TOO_LARGE`. This ticket
neither creates nor removes that outcome.

*The forward caution, which is the part worth writing down.* Shim **case 3**
(`shim.go:174-197`, booked-reserve-only) reports `current = 0, reclaimable = 0`
deterministically. So if the shim seam ever opened — if shim runs began feeding
peak/OOM history back into the estimator, e.g. by adopting `ru_maxrss` with the
provenance marker C10 refused for want of a wire field — the OOM branch would
become reachable there **and** §2.3.1's charge term would be zero by
construction, so the residual band would be wide open on every such container.
The cost of this change in that world is not occasional, it is total: every
small-container OOM signature would move from clamped-and-granted to terminally
refused. Anyone reopening that seam must revisit AIRA-151 in the same change.

**R6 — mixed daemon/client builds.** `[[aira-not-live-no-compat]]` makes this a
non-goal, and nothing on the wire changes: the same codes, the same payload
fields, the same validation. An older client meets a code it already handles.

**R7 — a consumer that expects the OOM branch never to return above the
ceiling.** Enumerated: the only production caller is `admit.go:1902`, which
checks `reserve > ceiling` on the next line;
`resolveDelegateRAMScopeCeiling` is a separate function with its own clamp and is
not touched; the basis consumers (the confine trailer, `Status.ReserveBasis`,
`ContainerAdvisories`, the ledger column, the gauge) are display-or-classify
only and none of them parses `ceiling-clamped`. No consumer assumes the
post-condition.

**R8 — this looks like a cleanup and is not.** The diff is one brace level. The
mitigation is procedural and is the point of this document: full two-loop, the
cost quantified in §2.3, the population stated in §1.2, and the deferrals in §8
naming exactly what stays open.

---

## 6. Expected yield

- One inconsistency inside one function removed: a signature's OOM record no
  longer changes whether an identical over-ceiling reserve is refused or clamped.
  T2 asserts precisely that pairing.
- The systematic route into AIRA-150 — every unpinned request on a slice smaller
  than the default, which is what made it "a certainty on a small slice" —
  removed. The residue is named (§3.7) rather than implied.
- A 30-second dead-end that ends in a refusal anyway becomes an immediate
  refusal carrying `required`, `cap_minus_headroom` and an OOM-attributed basis:
  the AIRA-71 outcome on one more surface.
- A job that just OOM-killed at tens of MiB stops being eligible for a hard scope
  cap equal to the entire slice.
- One clause of agent guidance that is true on a small slice, where the shipped
  "re-run the identical command" advice silently was not.
- Net new production state: **none**. One brace level in `resolveAdmitReserve`.

---

## 7. Tests

TDD: each new test is written and observed RED against `16b9141` before the code
exists, except where it is explicitly a pin (stated as such, never presented as a
demonstration — AIRA-149's own honesty note on T13 is the precedent).

### 7.1 New — the sizing rule

(T4, T4b and T4c are the updates forced by this change and live in §7.2; the id
gap here is deliberate so each id names one thing throughout.)

| Id | Name / file | Asserts | RED against master because |
| --- | --- | --- | --- |
| **T1** | `TestCeilingClampAppliesOnlyWhenTheEscalationDeterminedTheValue` — new `internal/daemon/admit_oom_clamp_scope_test.go` | the §0.2 table, **value and basis**, one row per case: **(b)** escalation-determined + over ceiling → `ceiling` / `estimate:oom-escalated,ceiling-clamped` *(unchanged)*; **(a)** escalation-determined, `MaxOOMPeak >= ceiling` → unclamped, terminal-sized value / `estimate:oom-escalated` *(unchanged)*; **(e-default)** the §0.1 measured shape → `4294967296` / `fallback:insufficient-samples:n=1,oom-on-record`; **(e-malformed)** injected `fallback:malformed`, reserve 200 GiB, ceiling 100 GiB → `200 GiB` / `fallback:malformed,oom-on-record`; **(c′)** `SampleCount 5, PeakMax 40 GiB, MaxOOMPeak 10 GiB, ceiling 44 GiB` → `49392123904` / `estimate:max=42949672960,n=5,f=115,oom-on-record`; **(tie)** `MaxOOMPeak 2 GiB`, request exactly `3 GiB`, ceiling `2.5 GiB` → `3221225472` unclamped / `fallback:insufficient-samples:n=1,oom-on-record`. Plus the table-wide `^\S+$` basis assertion AIRA-149's T1 carries | master clamps every row except (a)/(b) to the ceiling and appends `,ceiling-clamped` |
| **T2** | `TestAnOOMRecordNoLongerChangesWhetherAnOverCeilingEstimateIsClamped` — same file | the consistency claim, as a **pairing**: the identical `PeakRSSStats` with `OOMCount/MaxOOMPeak` set and cleared, at the same ceiling, resolve to the **same value** (the c′ shape above: `49392123904` both ways). Only the basis differs (`…,oom-on-record` vs the bare estimator basis) | master returns `47244640256` (the ceiling) with the OOM record and `49392123904` without it — the inconsistency this ticket exists to remove |
| **T3** | `TestOverCeilingUnescalatedReserveIsRefusedTerminallyInsteadOfClamped` — new `internal/daemon/admit_oom_clamp_wire_test.go`, driven through the real `admitConnection` over `net.Pipe` as `TestSliceCeilingDoesNotReachTheOOMEscalationClamp` is | the §0.1 shape end to end: 1 GiB slice, 40 MiB headroom, unpinned 4 GiB request, `oomClampedHistory`-style stats → an **immediate** `E_ADMIT_TOO_LARGE` frame (before any evaluator pass) with `required == 4294967296`, `cap_minus_headroom == 1031798784`, `basis == "fallback:insufficient-samples:n=1,oom-on-record"`, **and no waiter ever enqueued** (the queue's waiter list stays empty) | master enqueues, waits the whole window and answers `E_ADMIT_SATURATED`; the "no waiter enqueued" assertion is what distinguishes a genuine early refusal from a fast timeout |
| **T5** | `TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis` — `internal/runner/admission_saturated_message_test.go` (beside AIRA-149's T13–T17) | the exact operator-facing string for the newly-routed case: contains `required=4294967296`, `cap_minus_headroom=1031798784` and the basis; the client's `basis` is `reject:too-large` and its state is terminal | **GREEN by construction — a pin, not a demonstration.** It exists so deferral G3 rests on a recorded string rather than a claim, and so a later wording change is a deliberate edit to a test rather than an unnoticed drift |
| **T6** | `TestTooLargeRejectionForAnUnescalatedOverCeilingReserveIsAcceptedByTheClient` — same file | the exact daemon payload for §0.1 satisfies `validRunnerAdmitRejection`, so `admitThroughDaemon` returns the terminal refusal and **never** reaches `fail()` / the flock fallback | **GREEN by construction — a pin on I5.** Its RED direction is a future payload or validation change that would launch this population unaccounted |
| **T7** | *update:* `internal/core/skill_test.go` `TestSkillTeachesTheOOMVerdictAndTheColdStartSelfHeal` | one new required phrase from §3.6's clause (the `E_ADMIT_TOO_LARGE` consequence and the pin-at-or-below-`cap_minus_headroom` action), beside the existing `,ceiling-clamped` / `,oom-on-record` pins | master's guide does not contain it |

### 7.2 Updates forced by the change

| Id | What | Why |
| --- | --- | --- |
| **T4** | `internal/daemon/admit_saturated_diagnosis_test.go`: `oomClampedHistory()` and the **eight** `const maximum = int64(1) << 30` declarations that serve its ten call sites (§9 names the exact lines, and the two 8 GiB declarations that must NOT be touched), per R3's table; and the helper's doc comment rewritten to say the AIRA-149 §0 shape is no longer reachable **because this ticket refuses it**, and that the fixture now drives row (b) | R3. Every assertion in T8/T8b/T9/T10/T11/T12b/T12c/T12d is preserved verbatim; only the history helper, those eight constants and the comment move |
| **T4b** | `internal/store/admission_insight_test.go` `TestOOMBranchBasesStayOutsideTheAdmissionAdequacyPopulation`: the ten strings are **kept**, the comment corrected | Four of them (`estimate:…,oom-on-record,ceiling-clamped`, `estimate:capped,oom-on-record,ceiling-clamped`, `fallback:…,oom-on-record,ceiling-clamped` ×2) become unproducible. They stay as forward-defensive negatives — a regression that re-widens the clamp must not silently move the gauge — but the comment's claim "exactly the strings rows (a)-(e) can produce" would be false and is rewritten to say which are now unreachable and why they are retained |
| **T4c** | `internal/daemon/admit_oom_basis_test.go` T1 rows `e/measured…` and `e/malformed history clamped to the ceiling` | Those two rows encode the old behaviour. They move to their post-change values/bases (i.e. they become T1's `(e-default)` / `(e-malformed)` rows) with a comment naming AIRA-151. Every other row of that table, and all of T2/T3/T3b, are untouched |

### 7.3 Must stay green, unedited — the false-fail direction

Named explicitly, because "the suite is green" is not evidence unless the tests
that *should* have objected are known:

- `TestConfineEstimatorAndOOMEscalationClamp` (`confine_admit_test.go:16-40`) —
  row (b), the clamp still applies, value and basis unchanged.
- `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`
  (`sliceceiling_test.go:1302`) — row (b) through the wire path: the request must
  still be **clamped and then WAIT** on the throttle rather than be refused. This
  is the single most important green in the set: if the narrowing were
  mis-implemented as "clamp only when the value is *below* the ceiling", or
  applied to the wrong branch, this test fails with an immediate frame.
- `TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins`
  (`confine_admit_test.go:42-61`) — row (a) with `MaxOOMPeak == ceiling`: no
  clamp today, no clamp after, still terminally too large; and the pinned arm.
- `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` and
  `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` (real cgroup,
  `AIRA_REAL_CGROUP=1`) — row (d) under the ceiling, unedited (§1.3, I4).
- `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes`,
  `TestResolveAdmitReserveKeepsTheEstimatorsOwnFallbackBasis`,
  `TestPostBlockInsufficientSamplesFallbackIsUnchanged` — AIRA-149's facet-1
  suite; the `,oom-on-record` attribution and D4 are untouched. (T2 above
  iterates both a large and a small ceiling over five shapes and stays green
  because it asserts only the presence of an OOM token.)
- `internal/runner/admission_saturated_message_test.go` T13–T17 and
  `confine_linux_test.go:934` — client-side renders driven by synthetic
  payloads; unaffected.
- `TestConfineRejectedAdmissionCreatesNoScopeAndStartsNoChild` — the terminal
  refusal still creates nothing.
- `internal/runner/confine_reserve_resolution_linux_test.go` — the whole
  `ResolveConfineReserve` table, unedited. **This is where §1.2's "delegate-ram
  requests are untouched" claim is already pinned**, and it is recorded here as
  *covered* rather than given a new test row: the row at `:176-178`
  (`ConfineRequest{MemoryReserve: -1, DelegateRAM: true}` → `wantPinned: true`)
  drives the identical `reserve <= 0` arm of `confine.go:103-119` that a bare
  `ConfineRequest{DelegateRAM: true}` takes, so the pin derivation this ticket
  depends on cannot regress silently. An orthogonal review asked for a new row
  here; it was checked against source and found already covered, so nothing is
  added — but if a later change splits that arm, this file is the one that must
  gain a literal `{DelegateRAM: true}` row.

### 7.4 Mutation evidence to produce at implement time

Each applied alone in a detached throwaway worktree at the implementation
commit, then reverted; each must go RED, with the exact failing test recorded:

1. **Move the clamp back outside the escalation `if`** (i.e. restore master's
   shape) → T1 rows (c′)/(e-default)/(e-malformed)/(tie) and T2 and T3 RED.
2. **Widen the tie-break to `escalated >= reserve`** → T1's `tie` row RED. This
   is the mutant that proves §3.2 is a decision rather than an accident.
3. **Drop the clamp entirely** (rather than narrowing it) → the two §7.3 row-(b)
   tests RED (`TestConfineEstimatorAndOOMEscalationClamp` and
   `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`), which is what proves the
   change is a *narrowing* and not a deletion.
4. **Re-base T4's fixtures onto a pinned request instead of row (b)** → T4's own
   `Required != requested` premise disappears; recorded as a rejected alternative
   with its reason rather than as a mutation, since it is a fixture choice, not
   production code.

### 7.5 Validation to record

Serialised, never concurrent, each under `aira confine`, each with its exact exit
code, `pass`/`fail`/`unevaluated` kept distinct and no truncated log read as
green:

- `aira confine -- go build ./...`
- `aira confine -- go vet ./...`
- `aira confine -- gofmt -l internal/ cmd/` (must print nothing)
- `aira confine -- go test ./internal/daemon/... ./internal/runner/... ./internal/core/... ./internal/store/... -count=1`
- `aira confine -- go test -race ./internal/daemon/... -count=1` (CI has `-race`
  off per AIRA-20, so the race evidence is produced locally)
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — required because
  §1.3's claim about the AIRA-128 fixture is load-bearing and a **skip is
  reported as `unevaluated`, never as a pass**.

If a job must be stopped, `kill <PID>` the `aira confine` process itself; never
`systemctl --user stop aira.slice`.

---

## 8. Deferrals — filed, not silent

Each gets its own ticket via `aira id` at implement time; **no ID is hand-picked
here**, per `CLAUDE.md`. AIRA-150, AIRA-152 and AIRA-153 already exist and are
updated rather than re-filed.

- **G1 — AIRA-150 stays open, narrowed.** Its ticket gains §3.7's three residual
  routes to "resolved reserve == ceiling" (escalation-determined clamp; a pinned
  reserve exactly on the ceiling; coincidence) and the statement that the
  systematic route is gone. It must **not** be closed by this merge.
- **G2 — AIRA-153 is untouched and now uniformly visible.** Its ticket gains
  §3.7's before/after pair: the unpinned default over a small ceiling used to
  produce two different outcomes depending on whether an OOM record existed and
  now produces one. The design question it names is unchanged and unanswered.
- **G3 — the `E_ADMIT_TOO_LARGE` message names no escape hatch and prints raw
  bytes.** Filed with §3.5's two strings side by side, with T5's pin as its
  evidence, and with the two reasons it is not a one-liner: the message is shared
  with **pinned** requests, where "pin `--memory-reserve`" is wrong advice
  because the operator already did; and `cap_minus_headroom` is a **transient**
  on a busy slice (§2.3.3 — the entry ceiling moves by 64 MiB per outstanding job
  at arrival), so an escape-hatch clause must not present it as a fixed property
  of the slice. Whoever takes it must decide that case analysis deliberately.
  **The ci-shim `cap_minus_headroom=0` reason revision 1 filed here is withdrawn**
  — it describes the state AIRA-121 F1/F3 fixed (R5), and a successor starting
  from it would be constrained by a condition that no longer exists.
- **G4 — no real-cgroup test drives the newly-terminal path.** T3 drives it
  deterministically through the real wire path with a stubbed slice reader.
  Reproducing it against a real cgroup needs a slice whose ceiling is below the
  4 GiB default *and* a signature carrying an OOM record, i.e. re-creating the
  knife-edge AIRA-139 removed. Accepted coverage gap, inherited from AIRA-149's
  F7/AIRA-156 rather than newly introduced.
- **G5 — the AIRA-67 design spec's basis vocabulary bullet
  (`docs/superpowers/specs/2026-08-25-confine-estimate-reserve-design.md:143`)
  stays unedited**, for the reason AIRA-158 recorded and closed: a dated
  milestone design record is history, and the authoritative live surface is the
  generated agent guide, which §3.6 does update. Named so a reviewer meets a
  decision rather than an omission.
- **G6 — AIRA-152 remains an open, compatible candidate.** Nothing here forecloses
  it; §2.2 records why it was not chosen and §2.2's last sentence records how the
  two would compose.

---

## 9. Files touched at implement time

| File | Change |
| --- | --- |
| `internal/daemon/admit.go` | the clamp moves inside the `escalated > reserve` block, with the §3.1 comment. **One brace level; no other line in the function changes.** |
| `internal/core/skill.go` | the §3.6 clause in the self-heal paragraph |
| `internal/daemon/admit_oom_clamp_scope_test.go` | new — T1, T2 |
| `internal/daemon/admit_oom_clamp_wire_test.go` | new — T3 |
| `internal/runner/admission_saturated_message_test.go` | new cases — T5, T6 |
| `internal/daemon/admit_saturated_diagnosis_test.go` | T4 — `oomClampedHistory()` (`:57-63`) + the **eight** `const maximum = int64(1) << 30` declarations at `:241`, `:283`, `:315`, `:344`, `:387`, `:521`, `:549`, `:623` + the helper comment (R3). **Eight declarations, ten call sites**: `:549` serves the subtests at `:553` and `:579`, and `:623` serves those at `:627` and `:655`. **The two 8 GiB declarations must NOT be touched** — `:209` (`TestSaturatedRejectionCarriesTheResolvedReserveAndCeiling`, which uses a non-OOM history) and `:438` (`TestSoloRefusalBesideALeafDrainedScopeReportsContention`, no OOM history); neither calls `oomClampedHistory()` and neither is affected by this ticket |
| `internal/daemon/admit_oom_basis_test.go` | T4c — the two row-(e) table entries |
| `internal/store/admission_insight_test.go` | T4b — comment only, strings retained |
| `internal/core/skill_test.go` | T7 — one new required phrase |
| `.aira/tickets/AIRA-151.md` | status, resolution, gate record |
| `.aira/tickets/AIRA-150.md`, `.aira/tickets/AIRA-153.md` | the G1/G2 notes; **neither is closed** |

No change to `checkedAvailable`, the escalation arithmetic or its overflow guard,
the ceiling computation, the `reserve > ceiling` boundary, the enqueue-time
re-check, the AIRA-59 freeze, the AIRA-114 bound, the AIRA-101 gate, the AIRA-103
throttle rule, the reserve ledger, `resolveDelegateRAMScopeCeiling`,
`internal/store/admission_insight.go`, the protocol version, any CLI flag, or any
MCP/Skill dispatch surface.

---

## 10. Questions this plan expected the gate to press on — and the rulings

All five were pressed and all five are answered. They are kept, with the ruling
recorded beneath each, because a question deleted after it was answered leaves no
evidence that it was asked.

1. **Is the tie (`escalated == reserve`) handled right?** §3.2 keeps the strict
   comparison, so an exact tie is treated as "the client's request determined the
   value" and loses the clamp — a sizing consequence promoted from what was a
   labelling rule. The alternative (`escalated >= reserve`) would keep the clamp
   there and is defensible, since the number *is* numerically the escalation's.
   The plan's answer is that one condition must govern both the basis and the
   value, and that the tie is production-reachable at exactly
   `MaxOOMPeak = 2863311531`.
   → **RULED: keep the strict `>`.** Both lineages endorsed it as the
   data-model-first form — nesting makes "clamped without the escalation having
   set the value" unrepresentable, and one condition governs both label and value.
   The tie arithmetic was independently re-run (`2863311531 + 1431655765 =
   4294967296`) and the production-reachability claim confirmed. Unchanged.
2. **Should the `E_ADMIT_TOO_LARGE` message move with this ticket?** §3.5 says no
   and files G3, mitigating with the guide clause; R2 states the cost honestly.
   → **RULED: defer, with the reasons corrected.** Deferring G3 and mitigating via
   the agent guide is the right call. But the ci-shim `cap_minus_headroom=0`
   ground revision 1 gave is **withdrawn** — it describes the state AIRA-121
   F1/F3 fixed (R5). The reasons that stand are the shared pinned-request case
   and, added in revision 2, the transient-ceiling case from §2.3.3. §3.5 and G3
   now say exactly that.
3. **Is re-basing AIRA-149's facet-2b fixtures onto row (b) the right move, or
   should they move off the OOM branch entirely?** R3 chooses row (b) to keep
   `resolved != requested` and to keep the fixtures in the branch they were
   written against. The alternative — a pinned reserve exactly on the ceiling, or
   an ordinary estimate that lands on it — is simpler but weakens the fixtures'
   realism. Either way the fixture comment must record that this ticket is why
   they moved.
   → **RULED: row (b), as proposed.** Both lineages re-derived the arithmetic
   (`3758096384 × 1.5 = 5637144576 > 4294967296` → escalation determined; guard
   `3758096384 < 4253024256` → still clamps), confirmed `run.ceiling` is derived
   from `maximum` so every assertion scales, and confirmed the file asserts no
   resolved basis. The pinned-fixture alternative was rejected for losing
   `resolved != requested`. R3's ceiling figure is corrected to `4253024256`.
4. **Is §2.3's cost quantification complete?** Revision 1 claimed the lost grant
   window was exactly "byte-exactly idle, unthrottled, nothing outstanding or
   adopted", and asked the gate to check that against `checkedAvailable` and the
   evaluator headroom directly rather than against the prose.
   → **RULED: NO — it was not, and §2.3 is re-derived.** The honest answer is the
   inequality now at §2.3.1: a reserve equal to the entry ceiling is granted iff
   `charge <= (effectiveMaximum - maximum) + perJob × (J_entry -
   outstandingJobs_now - adoptedJobs_now)`. Revision 1's condition is the special
   case `J_entry == 0`, because the entry ceiling excludes adopted jobs and uses
   `outstandingJobs + 1` while the evaluator uses `outstandingJobs + adoptedJobs
   + 1`. A request that entered behind N since-drained jobs is grantable with up
   to `N × perJob` of residual charge, and **production `perJob` is 64 MiB, eight
   times the fixtures' 8 MiB** — up to ~768 MiB behind twelve jobs, which is not a
   knife edge. The production conclusion survives, but for a different reason
   (§2.3.2: the *population* is essentially empty there, not the window shut), and
   §2.3.3 adds the transient-boundary cost revision 1 omitted altogether.
5. **Should AIRA-150 be closed by this merge?** The plan says no (§3.7, G1) and
   names the three residual routes.
   → **RULED: no, do not close it.** Endorsed by both lineages: the three residual
   routes are correct and the route this ticket removes is the systematic one.
   AIRA-150's resolution note records what remains grantable only inside
   §2.3.1's residual band.

---

## 11. Revision 2 — where each required edit landed

The gate's verdict was *documentation-only*: no change of direction, no change to
§3.1's code change, to I1–I8, to §1.2's population, to §1.3's fixture analysis or
to the test plan. Its five required edits, plus the one optional item raised by
the orthogonal review, are recorded here so the re-check is a diff and not a
re-gate.

| # | Required edit | Landed in |
| --- | --- | --- |
| 1 | Re-derive §2.3 from the real entry-vs-evaluator headroom inequality, with production `perJob = 64 MiB`, and add the transient-boundary cost; answer Q4 in the corrected form | **§2.3.1** (the derivation and its three consequences), **§2.3.2** (the three environments re-derived), **§2.3.3** (the transient boundary and its interaction with `skill.go:323`), **§10 Q4** (the ruling). Knock-ons: the §0 table gains the entry-vs-evaluator and headroom-defaults rows and drops the "byte-exact zero" gloss; §0.1, §1.2, §2.3's removed-costs bullet, §3.1's code comment and §3.7 are re-worded off the same overstatement |
| 2 | Rewrite R5's reason to AIRA-121 C10 plus the already-terminal no-OOM shim path, with the case-3 caution | **R5**, rewritten in four parts (dead premise named, C10 as the structural reason, the already-terminal no-OOM path, the forward caution about case 3's deterministic zero charge). Grounded by three new §0 table rows |
| 3 | Drop the stale `cap_minus_headroom=0` clause from §3.5/G3; fix §3.6's paragraph description | **§3.5** (clause removed and explicitly withdrawn; the transient-ceiling reason added in its place), **G3** (same), **§10 Q2** (same), **§3.6** (the `,ceiling-clamped` sentence is mid-paragraph, followed by "So an OOM token…" and the "RE-RUN the identical command" conclusion; the new clause is placed after it and written to reinforce `skill.go:323` rather than contradict it) |
| 4 | `4253023744` → `4253024256` | **R3's re-basing table**, both occurrences, with the mis-evaluation noted and a warning that the tests derive `run.ceiling` from `maximum` so the figure must not be pasted into an assertion |
| 5 | "ten `const maximum` values" → **eight** declarations, excluding `:209` and `:438` | **§9's file table** (the eight lines enumerated, the eight-declarations/ten-call-sites mapping stated, and both 8 GiB declarations named as must-not-touch) and **§7.2's T4 row** |
| opt | Record that the delegate-ram pin derivation is already covered | **§7.3** (a must-stay-green entry for `confine_reserve_resolution_linux_test.go`, naming the `:176-178` row and why no new test is added) and a **§0 table row**. No new test row; the gate downgraded this to optional after confirming the coverage exists |

Nothing else in the document changed. `git diff 26b6ff9` touches only this file.
