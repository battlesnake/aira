# AIRA-151 — the OOM ceiling clamp applies only where the escalation determined the value

Status: **plan revision 1 — written for the plan gate. No implementation has
begun; this commit adds only this document.**

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
| `checkedAvailable` charges `max(current - reclaimable, outstanding+adopted)` against `maximum - headroom` and returns 0 when the charge reaches the ceiling | `admit.go:2622-2639` | yes — **so a reserve equal to the ceiling is grantable only while that charge is byte-exact zero** (AIRA-150) |
| `reclaimable` is the file-LRU total, not slab | `admit.go:3106-3122`, `:3151-3161` | yes — so the zero-charge condition is `current - file_lru <= 0`, not `current == 0` |
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
exactly on the ceiling, the grant gate then needs `max(current - reclaimable,
outstanding + adopted) == 0`, and with `current = 4096, reclaimable = 0` it was
refused for **107 consecutive evaluator passes (~30 s)** before timing out into
AIRA-149's saturated sentence.

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
- **with an OOM record** → the clamp cuts the value to exactly the ceiling → the
  request is admissible but, per AIRA-150, grantable only while the slice's
  charge is byte-exact zero → it almost always waits out its whole window and is
  then refused anyway, with a longer message and 30 s later.

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
with a reserve equal to the ceiling and is then grantable only while
`max(current - reclaimable, outstanding + adopted) == 0`.

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
slice". That is the right shape but too generous by construction, and a sizing
change on shared infrastructure deserves the exact figure rather than the
comfortable one. Derived from `checkedAvailable` (`admit.go:2622-2639`) and the
evaluator (`:2459-2484`):

Every request in §1.2's population is, today, resolved to a reserve **exactly
equal to the request-entry ceiling**. Such a reserve is granted if and only if,
on some evaluator pass:

1. `max(current - reclaimable, outstanding + adopted) == 0` — the slice's
   non-file-reclaimable charge is byte-exact zero and nothing else is booked; and
2. `outstanding + adopted == 0` — otherwise the evaluator's own headroom
   (`admitSliceHeadroom(outstanding + adopted + 1)`) exceeds the entry headroom
   and the evaluator's ceiling is strictly below the reserve; and
3. no AIRA-103 throttle is published — otherwise `effectiveMaximum < maximum` and
   the same inequality bites.

So the availability this change gives up is exactly: **the grant that happens
only on a byte-exactly-idle, unthrottled, otherwise-empty slice.** It is not "a
quiet slice" — one residual 4 KiB anonymous page is enough to close it, which is
what AIRA-150 measured (107 consecutive refusals, ~30 s, `current = 4096`) and
what made AIRA-139 a flake: the same fixture passed when a poll happened to read
`current = 0` and hung when it read `4096`.

Where that window is genuinely open, and where it is not:

- **The production 64 GiB `aira.slice`:** the window is closed in practice. The
  slice carries live jobs and residual charge essentially always, and reaching
  §1.2's population there needs an ordinary estimate above ~62 GiB (peak history
  above ~54 GiB) with an OOM peak below ~41 GiB. Today such a request waits its
  whole window and is refused; after this change it is refused immediately, with
  both numbers. **No availability is lost on production.**
- **A small or fixture slice** (a CI slice, a shim-configured ceiling, a test
  fixture, a small `aira.slice` install): the window is open exactly when the
  slice is idle, which for a freshly created fixture slice is common — this is
  the case AIRA-139 measured as a coin flip. Here the change is real: an unpinned
  job whose signature carries an OOM record and whose resolved reserve exceeds
  the ceiling now **cannot run at all** without `--memory-reserve` /
  `--memory-max`, where today it sometimes could.
- **The sharpest form of that**, stated plainly rather than buried: on a slice
  whose ceiling is below the 4 GiB unpinned default, AIRA-128's cold-start
  self-heal (`terminated-by=oom` → "re-run the identical command") stops working
  for that command. Today the re-run is admitted iff the slice reads byte-exactly
  idle at some pass; after this change it is refused terminally every time until
  the operator pins a reserve. That is a genuine regression in that environment,
  and it is accepted because the alternative it replaces is a 30-second wait that
  ends in a refusal anyway on any slice that is not idle, because the first run
  of a *novel* command on such a slice is **already** refused terminally by the
  identical `reserve > ceiling` boundary (AIRA-153), and because the underlying
  fault is the unconditioned default, not the clamp (§3.7).

Two costs the change **removes**, for completeness of the ledger:

- A job that wins today's byte-exact-zero window is granted a scope
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
    // exactly the ceiling, and a reserve exactly on the ceiling is grantable only
    // while the slice's own charge reads byte-exact zero (AIRA-150), so what the
    // clamp bought those rows was a wait that almost never ends, not a run.
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
squeamishness. The string is shared with every other `E_ADMIT_TOO_LARGE` — a
pinned request that asked for more than the slice holds (where "pin
`--memory-reserve`" is the wrong advice, since the operator already did), and
ci-shim mode, which answers `cap_minus_headroom=0` for a mis-sized container's
entire life (`shim.go:161`, `:186`; `install/mode.go:134-152`), where "pin at or
below 0B" is not advice at all. Making the clause correct for all three needs a
case analysis inside a message renderer, which is machinery this ticket has no
business adding on top of a sizing change. It is filed as **G3** with the exact
strings and the ci-shim complication, and T5 pins the message this ticket
actually routes traffic onto so the follow-up starts from evidence.

**What this plan does do instead** is fix the actionability where it costs one
sentence and no branching: the generated agent guide (§3.6). The guide is this
repo's authoritative live surface — AIRA-158 closed the equivalent question for
the dated AIRA-67 spec by saying exactly that — and it is where an agent
following "RE-RUN the identical command" will otherwise meet an unexplained
terminal refusal.

### 3.6 The agent guide

`internal/core/skill.go:324` currently ends its self-heal paragraph with "A
trailing `,ceiling-clamped` means the slice's admission ceiling cut the result
down." After this change that sentence is still true but incomplete in the one
place it matters: it does not say what happens when the ceiling cannot
accommodate the number at all, which is now a terminal refusal rather than a
wait. One clause is added, in the same paragraph, saying:

- `,ceiling-clamped` appears only on the escalated value — the slice ceiling caps
  how far the OOM escalation may climb;
- when the reserve the daemon resolves is above the ceiling and did **not** come
  from the escalation, the run is refused immediately with `E_ADMIT_TOO_LARGE`
  naming `required` and `cap_minus_headroom`, instead of waiting;
- the action is to pin `--memory-reserve` (or `--memory-max`) at or below the
  printed `cap_minus_headroom`, or to run where the slice is larger — and that on
  such a slice a re-run alone will not self-heal.

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
ceiling is still grantable only at byte-exact zero charge, and after this change
three routes still produce one:

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

**R1 — availability on a small idle slice.** §2.3 states the exact window this
closes and the exact environments where it is open. Accepted; it is the owner's
decision and the ticket's own named counter-argument. Mitigations that exist
already: `--memory-reserve` and `--memory-max` both pin and bypass resolution
entirely, and both are named in the guide clause added by §3.6.

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
maximum      1 << 30  ->  4 << 30        (each `const maximum` in the affected tests)
ceiling      1031798784 -> 4253023744    (= 4 GiB - 32 MiB - 8 MiB, unchanged formula)
MaxOOMPeak   56360960 -> 3758096384      (3.5 GiB; PeakMax likewise, TotalCount/SampleCount 1)
escalated    5637144576 > 4294967296     -> row (b), escalation determined
clamp guard  3758096384 < 4253023744     -> fires, reserve = ceiling
request      runner.DefaultConfineMemoryReserve, UNCHANGED (the production unpinned value)
```

`current = 4096` still yields `grantable = ceiling - 4096`; T8b's
`current = ceiling` still yields a measured 0; the seeded waiters in T12c/T12d
derive their reserves from the fixture's own ceiling and scale automatically; the
holders' 64 MiB is still enough to refuse a ceiling-sized reserve. The helper's
doc comment must be rewritten to say that the AIRA-149 §0 measured shape is no
longer reachable **because this ticket refused it**, and that the fixture now
drives the row that still clamps — which is itself the executable evidence that
AIRA-150's systematic route is gone. T4 covers this.

**R4 — the ledger and the AIRA-52 gauge.** Runs that used to be granted in the
byte-exact-zero window (and to record `estimate:…,oom-on-record,ceiling-clamped`
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

**R5 — ci-shim mode.** A shim ledger with `cap_minus_headroom = 0` already
refuses every request terminally, whatever the clamp does; §1.2's population
requires `MaxOOMPeak < ceiling`, which is unsatisfiable at ceiling 0. Unaffected.

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
| **T4** | `internal/daemon/admit_saturated_diagnosis_test.go`: `oomClampedHistory()` and the `const maximum` of its ten call sites, per R3's table; and the helper's doc comment rewritten to say the AIRA-149 §0 shape is no longer reachable **because this ticket refuses it**, and that the fixture now drives row (b) | R3. Every assertion in T8/T8b/T9/T10/T11/T12b/T12c/T12d is preserved verbatim; only the two constants and the comment move |
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
  evidence, and with the reason it is not a one-liner: the message is shared with
  pinned requests (where "pin it" is wrong) and with ci-shim's permanent
  `cap_minus_headroom=0` (where "pin at or below 0B" is not advice). Whoever
  takes it must decide the case analysis deliberately.
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
| `internal/daemon/admit_saturated_diagnosis_test.go` | T4 — `oomClampedHistory()` + ten `const maximum` values + the helper comment (R3) |
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

## 10. Questions this plan expects the gate to press on

1. **Is the tie (`escalated == reserve`) handled right?** §3.2 keeps the strict
   comparison, so an exact tie is treated as "the client's request determined the
   value" and loses the clamp — a sizing consequence promoted from what was a
   labelling rule. The alternative (`escalated >= reserve`) would keep the clamp
   there and is defensible, since the number *is* numerically the escalation's.
   The plan's answer is that one condition must govern both the basis and the
   value, and that the tie is production-reachable at exactly
   `MaxOOMPeak = 2863311531`, so the gate should rule rather than let it be
   discovered.
2. **Should the `E_ADMIT_TOO_LARGE` message move with this ticket?** §3.5 says no
   and files G3, mitigating with the guide clause; R2 states the cost honestly. A
   gate that thinks a sizing change may not ship while routing traffic onto a
   less actionable message should say so now — the fix is a conditional clause,
   and the ci-shim `cap_minus_headroom=0` case is the reason it needs a decision.
3. **Is re-basing AIRA-149's facet-2b fixtures onto row (b) the right move, or
   should they move off the OOM branch entirely?** R3 chooses row (b) to keep
   `resolved != requested` and to keep the fixtures in the branch they were
   written against. The alternative — a pinned reserve exactly on the ceiling, or
   an ordinary estimate that lands on it — is simpler but weakens the fixtures'
   realism. Either way the fixture comment must record that this ticket is why
   they moved.
4. **Is §2.3's cost quantification complete?** It claims the lost grant window is
   exactly "byte-exactly idle, unthrottled, nothing outstanding or adopted". The
   gate should check that against `checkedAvailable` and the evaluator headroom
   directly rather than against this prose, since the whole availability argument
   rests on it.
5. **Should AIRA-150 be closed by this merge?** The plan says no (§3.7, G1) and
   names the three residual routes. A gate that reads AIRA-150 as "the systematic
   case only" may disagree; the honest record either way must state what remains
   grantable-only-at-zero-charge.
