# AIRA-149 — the OOM-escalation basis names a provenance its number does not have, and the wait it wedges reports a fabricated cause

Status: **plan revision 1 — awaiting plan review and the Fable plan gate**. This
touches the machine-wide admission gate every `aira confine` job on this box goes
through, so it is the full two-loop per `CLAUDE.md`, not the light path. **No
implementation has begun**; this commit adds only this document.

Ticket: `.aira/tickets/AIRA-149.md` (P2, `admission`/`confine`/`honesty`).
Filed out of AIRA-139 (`19c6bf2`), which removed the flake at fixture level and
deliberately left this. Prior art this plan must not re-litigate: AIRA-128 (the
OOM self-heal and its real-cgroup fixture), AIRA-133 (auto-estimated vs
operator-supplied cap provenance), AIRA-101 (the exclusivity rejection wording
and its precedence), AIRA-103/AIRA-106 (the static-vs-throttled ceiling rule),
AIRA-71 (a bounded, sound, silent wait is not a correctness bug — the fix is to
make it visible), AIRA-114 (the aggregate over-subscription bound), AIRA-59 (the
fairness freeze).

Branch: `aira149-admit-oom-clamp-honesty`, from `origin/master` at `19c6bf2`.

**Verdict, up front.** The ticket offers three directions. This plan adopts
**direction 3 (diagnosis) plus the facet-1 labelling fix**, and refuses
directions 1 and 2 with reasons (§2). It departs from the naive form of the
facet-1 fix — "otherwise report the true basis of the value returned" — because
that, on its own, **destroys a load-bearing attribution proof** and makes a
mutation-verified real-cgroup test porous (§3.2). The adopted form states the
value's true provenance *and* keeps the OOM attribution nameable. **No resolved
reserve value changes, and no admission or grant decision changes.**

---

## 0. Source read, at the commit this plan was written against

Every line below was read fresh in this worktree at `19c6bf2`. The ticket's own
quotations were checked and are current.

| Fact | Where | Confirmed |
| --- | --- | --- |
| the OOM branch, verbatim as the ticket quotes it | `internal/daemon/admit.go:1486-1503` | yes |
| an unpinned confine request's reserve is *exactly* `DefaultConfineMemoryReserve` | `internal/runner/confine.go:103-119`, `confine.go:19` (`4 << 30`) | yes — `--memory-reserve`, `--memory-max` and `--delegate-ram` each set `pinned` |
| a pinned request returns at the first line of `resolveAdmitReserve` | `admit.go:1454-1456` | yes |
| the ordinary estimate needs ≥ 3 samples | `internal/runner/resource_estimate.go:33-41` (`memoryEstimateMinSamples`) | yes |
| `ordinary.OOMCount = 0` is set before the estimator call, so the estimator's own `estimate:oom:` basis is unreachable from here | `admit.go:1476-1480` vs `resource_estimate.go:56-60` | yes |
| the estimator's basis is **discarded** when `ok == false`, and a hardcoded `"fallback:insufficient-samples"` is used instead | `admit.go:1475-1480` | yes — a third, smaller false label (§3.4) |
| the clamp is `stats.MaxOOMPeak < ceiling && reserve > ceiling` | `admit.go:1499-1501` | yes |
| `admitConnection` refuses terminally only at `reserve > ceiling` | `admit.go:1710-1713` | yes — so `ceiling` is the largest *admissible* reserve |
| the grant gate is `reserve <= checkedAvailable(...)` | `admit.go:2246`, `2263` | yes |
| `checkedAvailable` charges `max(current - reclaimable, outstanding+adopted)` against `maximum - headroom` | `admit.go:2393-2410` | yes — so a reserve equal to the ceiling is grantable only at byte-exact zero charge |
| the saturated rejection payload carries **only** `Basis` and `Exclusive` | `admit.go:1785`, `1794` | yes — `Required` and `Ceiling` are left zero |
| the client therefore prints **its own** unpinned request, not the daemon's resolved reserve | `internal/runner/admission_linux.go:527-530` — `resolved := rejection.Required; if resolved <= 0 { resolved = effectiveReserve }` | yes — this is where "reserve 4G" in the ticket's message comes from |
| and prints `unknown` for the ceiling | `admission_linux.go:536-539` | yes |
| the fabricated sentence itself | `admission_linux.go:557-559` (`default:` arm) | yes |
| exclusivity already takes precedence over that arm | `admission_linux.go:549-556` | yes — AIRA-101; must not regress |
| `validRunnerAdmitRejection` pins only `Basis == "reject:saturated"` for this code | `admission_linux.go:660-661` | yes — extra fields are free |
| the in-wait progress line "queue position 1 of 1 … 0B queued ahead" | `internal/runner/confine_queue_position_linux.go:168-185` | yes — and it was **honest**; only the terminal message lied (§3.6) |
| `reserve-basis=` is space-delimited in the trailer, so a basis may contain commas but never spaces | `internal/runner/confine.go:897-900` | yes — existing bases already carry comma params (`estimate:max=%d,n=%d,f=115`) |
| the evaluator computes `available` *before* the freeze check, and skips it only on the exclusivity `continue` | `admit.go:2240-2246`, `2247-2250` | yes |
| the evaluator returns early, evaluating nobody, when the slice memory read fails | `admit.go:2157-2168` | yes — a waiter can time out having never been evaluated |
| `oversubscriptionBlocks` requires `queue.capAggregate > 0`, which requires live scanned scopes | `internal/daemon/admit_oversubscription.go:299-307` | yes — load-bearing for the §4.3 invariant |
| the AIRA-128 fixture's phase 3 is *exactly* the "escalation did not determine the value" case, and its own comment says so | `internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go:73-85` | yes — "what phase 3 pins is the escalation's ATTRIBUTION … not the escalated VALUE" |
| that fixture's phase-3 assertion is `ReserveBasis != "estimate:oom-escalated"` → fail | same file, `:256-259`, and the unit twin at `:320-323` | yes |
| the agent guide sells the label to agents | `internal/core/skill.go:324`; pinned by `internal/core/skill_test.go:644` | yes |
| `estimate%` is the SQL predicate for the AIRA-52 gauge population | `internal/runner/estimate_actual.go:47` | yes — §5.2 |

Measured evidence carried over from the ticket and re-derived against the code
above (arithmetic checked by hand, not re-run):

```
stats={TotalCount:1 SampleCount:1 PeakMax:56360960 OOMCount:1 MaxOOMPeak:56360960}
req.reserve=4294967296  ceiling=1031798784
  ordinary estimate: !ok (1 sample)            -> reserve stays 4294967296
  escalated = 84541440, not > 4294967296       -> the max() changes nothing
  clamp: 56360960 < 1031798784 && 4294967296 > 1031798784 -> reserve = 1031798784
  returned 1031798784, basis "estimate:oom-escalated"
```

Neither the escalation nor any estimate appears in that answer.

---

## 1. What is actually wrong, stated as defects

**D1 (facet 1).** `estimate:oom-escalated` is returned unconditionally by the OOM
branch, including when the escalation did not determine the returned value. Two
of the branch's four reachable outcomes therefore carry a false label, and one of
those two — no usable ordinary estimate, escalation below the client's reserve —
is the *normal* state immediately after a first OOM.

**D2 (facet 2b, part one).** The saturated rejection omits `Required` and
`Ceiling`, so the operator-facing sentence prints the **client's own unresolved
request** ("reserve 4G") under the word "reserve", and "unknown" for the ceiling.
The one pair of numbers that explains the refusal is absent, and one of the two
numbers that *is* printed is not the number the decision was made on.

**D3 (facet 2b, part two).** "slice contended, no memory admission within the
wait" is asserted for every non-exclusive saturated rejection, including one
where the daemon never observed another job holding or queued. That is a
manufactured cause.

**D4 (small, same function, same class).** `resolveAdmitReserve` discards the
estimator's own `!ok` basis and substitutes a hardcoded
`"fallback:insufficient-samples"`, which is wrong when the real reason was
`fallback:malformed` or `fallback:capture-unavailable`.

**Not a defect: the in-wait progress line.** "queue position 1 of 1 by enqueue
order, 0B queued ahead" was literally true. It is the terminal message that
lies. No change there (§8, deferral F6).

**Not fixed here: the structural wedge (facet 2 proper).** A resolved reserve
equal to the ceiling remains grantable only on a byte-exactly-empty slice.
§2 says why, and §8 files the candidate.

---

## 2. The three directions, evaluated independently

### 2.1 Direction 1 (replacement escalation) — refused here

Making the escalation a replacement rather than a `max` when no usable ordinary
estimate exists would, in the ticket's own example, size the next attempt at
84 MiB instead of 4 GiB for a job that needs ~320 MiB. The self-heal would then
take **several more OOM kills** to converge instead of one. The current `max()`
encodes a defensible rule — *never size the next attempt below what an unpinned
client asked for* — and replacing it trades operator-visible OOM kills for slice
efficiency. That is a real design question with a real cost either way; it is not
a by-product of an honesty fix, and it must not be merged as one. It would also
require restating AIRA-128's shipped claim ("the very next run succeeds") as a
convergence ladder and retuning its real-cgroup fixture.

### 2.2 Direction 2 (retarget the clamp) — refused, and on a stronger ground than "kludge"

`ceiling` minus an arbitrary slack is a kludge and the simplicity rule refuses it.
But the more principled-looking variant — clamp to the *currently grantable*
quantity, `checkedAvailable(...)` — is refused on an **already-documented
invariant**: `admit.go:2216-2219` says the throttled/live figure is
"DELIBERATELY not applied to the ceiling `admitConnection` computes from the same
file: that one decides the TERMINAL `E_ADMIT_TOO_LARGE` and **sizes a job's own
hard scope cap**". That clamp's output *becomes* a non-delegate job's
`memory.max`. Sizing kernel-enforced containment from the slice's transient
charge is exactly what AIRA-103 forbade, and `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`
exists to keep it forbidden. Direction 2 has no principled target.

### 2.3 A fourth direction the ticket does not list, found in this read — filed, not adopted

The clamp's stated justification is "*earlier censored caps are allowed to climb
to the ceiling so a runnable job is never permanently wedged*". That rationale is
about a value **derived from the OOM peak**. It does not apply to the blind
unpinned client default, which has nothing to do with the OOM. So a minimal,
genuinely principled sizing fix exists: **apply the clamp only when the
escalation determined the value.**

It is not adopted here, for a reason worth writing down: on the no-OOM path the
identical over-ceiling value is *already* refused terminally with
`E_ADMIT_TOO_LARGE` (`admit.go:1710`), which is an honest, immediate, actionable
message carrying both numbers. So narrowing the clamp would make the OOM path
consistent with the ordinary path — but it converts an occasionally-grantable
wait into a **certain terminal refusal** for a job whose ordinary estimate
exceeds the ceiling but which today can still be granted on a quiet slice. That
is a sizing/availability decision with its own blast radius on shared
infrastructure, and it belongs to its own ticket and its own two-loop. §8, F2.

### 2.4 Direction 3 (diagnosis) — adopted, and sufficient

Adopted, with facet 1. The completeness argument, because "cheapest" is not a
justification:

- AIRA-71 settled this exact class on this exact subsystem: a **bounded, sound,
  fail-closed, fair** admission wait that is *silent* is not a correctness bug;
  the fix is to make it visible. The wait here is bounded (`max_wait`, ceiling
  `admitWaitCeilingMs`), it releases its ledger charge on every return path, and
  it refuses fail-closed. What was wrong was the sentence at the end of it.
- After this change the operator gets, within the wait's own bound, the three
  numbers that fully explain the refusal plus the established statement that
  nothing else was in the way, and the documented escape hatch
  (`--memory-reserve` / `--memory-max`, both of which pin and bypass resolution
  entirely). That is actionable, which is the whole of the AIRA-71 lesson.
- Nothing else in this plan can *end* the wedge without changing sizing, and
  changing sizing on the machine-wide gate is precisely what the ticket says to
  do deliberately rather than as a cleanup.

**Rejected sub-option: refuse early when the wait "cannot succeed".** The daemon
cannot establish that. A slice whose charge is one residual reclaimable page may
reach byte-exact zero at the next pass; `checkedAvailable` discounts
`reclaimable` precisely because that memory can go away. Refusing early would
replace a fabricated cause with a fabricated **certainty**, which is worse.

---

## 3. Design

### 3.1 Facet 1 — the basis names the term that determined the value

The rule, stated once and implemented once: **`reserve-basis` names the
provenance of the number actually returned.**

The OOM branch (`admit.go:1486-1503`) keeps its arithmetic byte-for-byte and
changes only what it returns as `basis`:

| # | Condition | Value returned | Basis returned |
| --- | --- | --- | --- |
| a | `escalated > reserve`, no clamp | `1.5 × MaxOOMPeak` | `estimate:oom-escalated` *(unchanged)* |
| b | `escalated > reserve`, clamp applied | `ceiling` | `estimate:oom-escalated,ceiling-clamped` |
| c | `escalated <= reserve`, ordinary estimate usable | the ordinary estimate | `estimate:max=<P>,n=<N>,f=115,oom-on-record` |
| d | `escalated <= reserve`, no usable estimate | the client's reserve | `fallback:insufficient-samples:n=<N>,oom-on-record` |
| e | as (d), then clamp applied | `ceiling` | `fallback:insufficient-samples:n=<N>,oom-on-record,ceiling-clamped` |

Row (e) is the ticket's measured case. Rows (a) and (b) keep the token every
existing document, test and agent-facing string already uses.

Grammar, and why it is not new machinery: the basis vocabulary is already
`family:name[:params]` with **comma-separated params** (`estimate:max=%d,n=%d,f=115`,
`fallback:insufficient-samples:n=%d`). `oom-on-record` and `ceiling-clamped` are
two more comma params on an existing basis. No new separator, no new family, and
no space (which the trailer's `key=value` format forbids).

Tie-break, stated so it is not an accident: the escalation is deemed to have
determined the value only when it **strictly raised** it (`escalated > reserve`,
the existing condition, unchanged). On an exact tie both terms produce the same
number and the source basis is reported.

### 3.2 Why the naive form of the facet-1 fix is insufficient — the finding that changed this plan

`estimate:oom-escalated` currently carries **two** meanings welded together:

1. *attribution* — "an OOM record for **this signature** was found and consulted";
2. *provenance* — "the returned number is 1.5× the OOM peak".

Meaning (2) is false in rows (c)–(e). Meaning (1) is true in all four, and it is
**load-bearing**: AIRA-128's real-cgroup end-to-end test uses it as the proof
that a real kernel OOM travelled `memory.events → confine teardown → reportPeak →
RecordConfinePeak → ConfinePeakHistory → resolveAdmitReserve`, and its comment
says so explicitly ("This basis is reachable ONLY through `stats.OOMCount > 0`
for THIS exact signature… A signature-attribution gap of any kind leaves this at
a fallback basis"). That test's mutation record lists "escalation branch disabled
→ RED at phase 3" as one of its kills.

Reporting a bare `fallback:insufficient-samples` in row (d) would delete meaning
(1) from the system. The phase-3 assertion would then be comparing a string
reachable from many paths, and its stated contract would become false — it would
survive only by accidentally differing from the `estimate:p90-prior` the mutant
happens to produce. That is precisely the porous-load-bearing-test class
`[[two-loop-porous-tests]]` exists to catch, and it would be introduced *by* an
honesty fix.

The `oom-on-record` token is therefore not decoration: it is what preserves an
existing verified property while the provenance half is corrected. It is
reachable only through `stats.OOMCount > 0 && stats.MaxOOMPeak > 0` for the
signature being resolved, exactly as `oom-escalated` is today.

### 3.3 What facet 1 does **not** change

`reserve` is computed by exactly the same statements, in the same order, with the
same constants. Rows (a)–(e) assert **values as well as labels** (§7 T1) so this
is enforced, not merely intended. AIRA-128's convergence behaviour is
byte-identical: its phase-3 run is still admitted at the 4 GiB default and still
succeeds on the second attempt.

### 3.4 D4 — stop discarding the estimator's own fallback basis

`basis := "fallback:insufficient-samples"` before the estimator call becomes the
estimator's returned basis in **both** arms, i.e. the `!ok` basis is kept rather
than overwritten. Two call sites read that local: the `stats.SampleCount >= 3 &&
reserve > 0` return (reachable with `ok == false` only via `fallback:malformed`,
which today is reported as `insufficient-samples` — a false label) and the new
row (c)/(d) arm. Two lines, same honesty rule, same function; leaving it would be
indefensible in a change whose subject is false bases.

### 3.5 Facet 2b — an established diagnosis instead of a manufactured one

Three additions to the saturated rejection, all diagnosis, none consulted by any
decision.

**(i) Populate the two numbers that already have fields.** `admitConnection`
still holds `reserve` (resolved) and `ceiling` (request-entry `maximum -
headroom`) in scope at the rejection site (`admit.go:1709`, `1703`, read at
`1785`/`1794`). Both go into the existing `admitRejection.Required` and
`.Ceiling`. No new plumbing. This alone deletes D2: the message stops printing
the client's own 4 GiB under the word "reserve".

**(ii) Latch, over the whole wait, whether anything else was ever in the way.**
Three new fields on `admitWaiter`, written **only** in `evaluateAdmitQueue`'s
existing refusal branches, under `queue.mu`, which the loop already holds:

```go
// AIRA-149. Diagnosis only: never read by any admission decision.
// LATCHED across the wait, never sampled at the instant of rejection.
contention   string // "" | admitContentionObserved | admitContentionNoneObserved
lastGrantable *int64 // the checkedAvailable the capacity gate last computed for THIS waiter
```

Transition rule, at each refusal of this waiter:

```
othersPresent := queue.outstandingJobs > 0 || queue.adoptedJobs > 0 || queuedAhead > 0
othersPresent -> contention = "observed"      (STICKY; once observed, never cleared)
!othersPresent && contention != "observed" -> contention = "none-observed"
whenever `available` was computed for this waiter -> lastGrantable = &available
```

`queuedAhead` is a local counter of still-queued waiters already examined in this
pass; a waiter granted earlier in the same pass has already incremented
`outstandingJobs`, so it counts as present. The exclusivity `continue` branch
sets `"observed"` explicitly rather than by inference.

Three decisions in that rule are load-bearing and each is tested:

- **Sticky.** "Contended" wins for the whole wait. A waiter that was blocked
  behind a real job for 29 of 30 seconds and happens to be alone at the instant
  the timer fires must not be told nothing was in the way (T8).
- **Job counts, not bytes.** A residual 4 KiB page in the slice is **not**
  another job. Reading a nonzero `current` as contention is exactly the
  misdiagnosis this ticket is about; the AIRA-139 case had `current=4096` with
  zero jobs (T6).
- **Absent means unevaluated.** A waiter the gate never evaluated (its wait
  expired before any pass, or the slice memory read failed for the whole wait —
  `admit.go:2157-2168`) latches nothing, and is reported `unevaluated`, never
  "nothing was in the way" (T9).

**(iii) Carry them on the wire.** `admitRejection` gains
`Contention string json:"contention,omitempty"` and
`Grantable *int64 json:"grantable_bytes,omitempty"`. The pointer is deliberate:
`0` grantable is the *interesting* value in the ticket's own case, and an
`omitempty` scalar would erase it. `Basis` keeps its exact `reject:saturated`
spelling, which `validRunnerAdmitRejection` pins.

Daemon-side the enum is `"observed"` / `"none-observed"` / `"unevaluated"`, and
the daemon always sets one of the three on this path, so an **empty** value at
the client strictly means "not reported by this build".

### 3.6 The sentence the client renders

Rendering stays where it already is (`admission_linux.go:549-559`), and the
AIRA-101 exclusivity `switch` keeps its precedence — the new clause only ever
replaces the `default:` arm.

```
none-observed:
  E_ADMIT_SATURATED: confine: admission rejected after 30s — nothing else held or
  was queued for this slice at any evaluation; the resolved reserve 984M did not
  fit the admission ceiling 984M (largest grantable reserve 0B at the last
  evaluation). Pin --memory-reserve or --memory-max to size this job yourself.

observed (wording unchanged, numbers now real):
  E_ADMIT_SATURATED: confine: admission rejected after 30s — slice contended, no
  memory admission within the wait (reserve 984M/984M)

unevaluated:
  E_ADMIT_SATURATED: confine: admission rejected after 30s — the admission gate
  did not evaluate this request before the wait expired (reserve 984M/984M)

empty (not reported by this daemon build):
  the existing sentence, unchanged
```

Two wording rules the implementation must hold to:

- The ceiling is the **request-entry** figure and the grantable is the **gate's
  last pass**; they are different instants (a throttle or a change in job count
  can move the second). Each is labelled by provenance and the sentence never
  invites the reader to subtract one from the other.
- "nothing else held or was queued **at any evaluation**" is the fact that was
  established. Not "the slice was empty" — the slice's own residual charge is why
  the request failed, and claiming emptiness would be a new fabrication.

---

## 4. Invariants

**I1. No resolved reserve value changes anywhere.** Every row of T1 asserts the
value as well as the basis; §3.3.

**I2. No admission or grant decision changes.** `checkedAvailable`, the
`reserve > ceiling` terminal boundary, the OOM clamp, the AIRA-59 freeze, the
AIRA-114 aggregate bound, `enqueueAdmitInternal`'s ceiling check and the ledger
are untouched. The new fields are write-only from the evaluator and read-only at
the rejection site.

**I3. AIRA-128's self-heal is unchanged in behaviour and its attribution proof
survives**, carried by `oom-on-record` (§3.2), pinned by T2.

**I4. AIRA-101 exclusivity keeps precedence** over the contention clause (T15).

**I5. `reject:saturated` stays the rejection `Basis`** — `validRunnerAdmitRejection`
pins it and a mismatch drops the client into the flock fallback, launching
outside the ledger.

**I6. No new work on the hot path.** The latch writes happen only inside refusal
branches that already execute; the grant path is untouched. No new daemon verb,
no extra socket round trip, no extra cgroupfs read.

**I7. Every new field is written and read under `queue.mu` only.** Same
discipline as the AIRA-29 charge fields documented at `admit.go:220-224`.

---

## 5. Risks

**R1 — the basis string is operator- and agent-facing.** `internal/core/skill.go:324`
sells `estimate:oom-escalated` to agents as the proof that the next run
self-healed. Rows (c)–(e) will now report a different string. *Mitigation:* the
agent guide prose is updated in the same change to describe what the token
family actually means, and `skill_test.go:644` with it. The `oom-escalated`
spelling itself is unchanged where it is true.

**R2 — the AIRA-52 estimate-vs-actual gauge population moves.**
`estimate_actual.go:47` selects `admission_reserve_basis LIKE 'estimate%'`.
Row (d)/(e) rows leave that population (they were never estimates); rows (c),
(a), (b) stay in it. This is an intended honesty improvement — the gauge is
measuring estimate adequacy and was being fed non-estimates — and it is named
here rather than discovered later.

**R3 — new mutable per-waiter state.** Mitigated by I7 and by a mandatory local
`go test -race ./internal/daemon/...` run (CI has `-race` off per AIRA-20, so the
race evidence must be produced locally and recorded with its exit code).

**R4 — over-reporting contention.** The sticky rule can report "observed" for a
wait whose contention ended early. That is the *safe* direction: it yields the
existing, already-shipped sentence.

**R5 — a consumer matching the old sentence.**
`internal/runner/confine_linux_test.go:934` requires `"slice contended"` for
`E_ADMIT_SATURATED`. It sends a rejection with no `contention` field, so it lands
on the unchanged empty-value arm and stays green — but it must not be left as the
only saturated-wording coverage, so T12–T14 are added beside it.

**R6 — mixed daemon/client builds.** `[[aira-not-live-no-compat]]` makes this a
non-goal, but the empty-value arm means an older daemon degrades to today's
message rather than to a wrong one. Stated, not engineered for.

---

## 6. Expected yield

- One false operator-facing provenance label deleted in the *commonest* state of
  the OOM branch (row (d)/(e)), and in row (c).
- One false number deleted from a terminal message (the client's own 4 GiB
  printed as "reserve") and one `unknown` replaced by the real ceiling.
- One manufactured cause ("slice contended") replaced by an established one, in
  the exact case the ticket measured — including the daemon's own last grantable
  figure, which is the number that explains it.
- A silent 30-second dead-end on a shared box becomes a message naming both the
  cause and the escape hatch — the AIRA-71 outcome, on a second surface.
- Two follow-up tickets filed with the evidence to judge them (§8 F1, F2), rather
  than a sizing change merged inside an honesty fix.
- Net new production state: three fields, all diagnosis-only.

---

## 7. Tests

Every test is named, placed, and given its RED direction. TDD: each is written
and observed RED against `19c6bf2` before the corresponding code exists.

### 7.1 Facet 1 — the basis

| Id | Name / file | Asserts | RED against master because |
| --- | --- | --- | --- |
| **T1** | `TestOOMEscalationBasisNamesTheTermThatDeterminedTheReserve` — new `internal/daemon/admit_oom_basis_test.go` | the full §3.1 table, **value and basis** for rows (a)–(e), including row (e) driven with the ticket's measured stats and ceiling | master returns `estimate:oom-escalated` for (b)–(e) |
| **T2** | `TestEveryOOMBranchBasisNamesTheOOMRecordAndOnlyTheOOMBranchDoes` — same file | every row of T1 contains `oom-escalated` or `oom-on-record`; the identical stats with `OOMCount = 0` (and with `MaxOOMPeak = 0`) contain **neither** | new property; it is the test that keeps AIRA-128's attribution proof non-porous (§3.2). Mutation: dropping the `,oom-on-record` append must turn it RED |
| **T3** | `TestResolveAdmitReserveKeepsTheEstimatorsOwnFallbackBasis` — same file | `SampleCount >= 3` with `PeakMax <= 0` → `fallback:malformed`; `SampleCount == 1` → `fallback:insufficient-samples:n=1` | master reports `fallback:insufficient-samples` for both (D4) |
| **T4** | *updates, not new:* `TestConfineEstimatorAndOOMEscalationClamp` (→ `estimate:oom-escalated,ceiling-clamped`), `TestSliceCeilingDoesNotReachTheOOMEscalationClamp` (→ same; this **strengthens** its own stated purpose, since the clamp it exists to exercise is now named in the basis), `TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins` (label unchanged — verified row (a) with no clamp) | — | — |
| **T5** | *updates:* `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` and `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` phase 3 → exact `fallback:insufficient-samples:n=1,oom-on-record`, with the fixture comment rewritten to say what the token proves and what it does not | the value assertions (`ScopeMemoryMax >= 1.5 × oomPeak`, "the second run succeeds") are untouched | — |
| **T6** | *update:* `internal/core/skill_test.go:644` + the `skill.go:324` prose | the agent guide describes the token family truthfully | — |

### 7.2 Facet 2b — the diagnosis (daemon side, `internal/daemon`)

Driven through the real `admitConnection` wire path, as `TestSliceCeilingDoesNotReachTheOOMEscalationClamp`
is, because the defect is in what reaches the client.

| Id | Name (new `internal/daemon/admit_saturated_diagnosis_test.go`) | Asserts | RED because |
| --- | --- | --- | --- |
| **T7** | `TestSaturatedRejectionCarriesTheResolvedReserveAndCeiling` | a timed-out waiter's rejection has `required` == the **daemon-resolved** reserve and `cap_minus_headroom` == the request-entry ceiling | master leaves both zero |
| **T8** | `TestSaturatedRejectionReportsNoContentionWhenNothingWasEverQueuedOrHeld` | **the facet-2b shape, exactly**: one waiter, alone, resolved reserve == ceiling, slice `current = 4096`, nothing outstanding or adopted → `contention == "none-observed"`, `required == ceiling`, `grantable_bytes == 0` (present, not omitted) | master reports no contention field at all |
| **T9** | `TestSaturatedRejectionReportsContentionWhenAnotherJobHeldTheSlice` | a granted job holds the ledger; the second waiter times out → `"observed"` | false-positive direction; guards the new clause from claiming solitude wrongly |
| **T10** | `TestSaturatedContentionIsLatchedAcrossTheWholeWaitNotSampledAtRejection` | a holder occupies the slice for the early passes and is **released before the deadline**, leaving the waiter alone at the instant of rejection → still `"observed"` | RED against any implementation that reads the queue at rejection time instead of latching (§3.5) |
| **T11** | `TestSaturatedRejectionSaysUnevaluatedWhenTheGateNeverEvaluatedIt` | `admitReadMemory` returns `ok == false` for the whole wait, so the evaluator returns early every pass → `"unevaluated"`, and **no** `grantable_bytes` | master fabricates "contended" for a slice it could not even read |
| **T12** | `TestSaturatedSoloRefusalCanOnlyComeFromTheCapacityGate` | drive an AIRA-114 aggregate-cap refusal and an AIRA-59 freeze refusal; both report `"observed"` | pins the §4.3 invariant that makes the solo sentence safe — `oversubscriptionBlocks` needs `capAggregate > 0`, which needs live scopes, and a freeze needs a waiter ahead. If a future change breaks that, this fails instead of shipping a wrong sentence |

### 7.3 Facet 2b — the sentence (client side, `internal/runner`)

New `internal/runner/admission_saturated_message_test.go`, driving
`admitThroughDaemon` over `net.Pipe` exactly as
`TestConfineRejectedAdmissionCreatesNoScopeAndStartsNoChild` does.

| Id | Name | Asserts | RED because |
| --- | --- | --- | --- |
| **T13** | `TestSaturatedMessagePrintsTheDaemonsResolvedReserveNotTheClientsRequest` | `Required = 984M` with a client `effectiveReserve` of 4 GiB → the message contains `984M` and **not** `4G` | master prints the client's request (`admission_linux.go:527-530`) |
| **T14** | `TestSaturatedMessageNamesTheUnfittableReserveInsteadOfContention` | `contention="none-observed"`, `Required == Ceiling`, `Grantable = 0` → message does **not** contain `slice contended`, and does contain the reserve, the ceiling, the grantable figure and the "nothing else held or was queued" fact | master always says "slice contended" |
| **T15** | `TestSaturatedMessageKeepsTheContendedWordingWhenContentionWasObserved` | `contention="observed"` → the existing sentence, with real numbers | false-fail direction |
| **T16** | `TestSaturatedMessageFallsBackToTheGenericWordingWhenContentionIsUnreported` | `contention=""` → today's sentence, unchanged | pins that empty is **not** read as "none-observed" (R6), and keeps `confine_linux_test.go:934` meaningful |
| **T17** | `TestSaturatedExclusiveWordingStillWinsOverTheContentionClause` | `Exclusive="held"` **and** `contention="none-observed"` → the AIRA-101 exclusivity sentence | I4; a regression here would resurrect the exact misdirection AIRA-101's build review caught |

### 7.4 Deliberately not written

**No real-cgroup end-to-end for the wedge.** Reproducing it needs a slice whose
ceiling equals the resolved reserve with a nonzero residual charge — which is
*precisely* the knife-edge AIRA-139 removed from the fixture, and re-adding it
would re-add the flake. T8 drives the identical shape deterministically through
the real wire path with a stubbed slice reader. Recorded as an accepted coverage
gap, not an oversight.

### 7.5 Validation to record

`whale-run`-prefixed, with exact exit codes, per `CLAUDE.md`:

- `aira confine -- go build ./...`
- `aira confine -- go test ./internal/daemon/... ./internal/runner/... ./internal/core/...`
- `aira confine -- go test -race ./internal/daemon/...` (R3)
- `aira confine -- go test ./...`
- the real-cgroup suite for `internal/daemon` on this box, since T5 touches the
  AIRA-128 fixture; a skip is reported as `unevaluated`, never as a pass.

---

## 8. Deferrals — filed, not silent

Each gets its own ticket via `aira id` at implement time. **No ID is hand-picked
here**, per `CLAUDE.md`.

- **F1 — the structural wedge stands.** A resolved reserve equal to the ceiling
  is admissible but grantable only at byte-exact zero charge. After this change
  it fails with an accurate message inside the wait's bound instead of a
  fabricated one; it still fails. Carry §0's measured evidence and §2's analysis
  onto the ticket.
- **F2 — the candidate fix for F1 found in this read (§2.3):** apply the ceiling
  clamp only when the escalation determined the value, making the OOM path
  consistent with the no-OOM path's existing terminal `E_ADMIT_TOO_LARGE`. File
  **with its counter-argument**: it converts an occasionally-grantable wait into
  a certain refusal for a large ordinary estimate.
- **F3 — direction 1 (replacement escalation)** with §2.1's cost analysis, so a
  future session does not re-derive it.
- **F4 — the unpinned 4 GiB default is unconditioned by the slice ceiling.**
  This is the actual reason facet 2 is a certainty on any slice below
  ~4 GiB + headroom and near-unreachable on the 62 GiB production ceiling. A
  design question in its own right; note that the no-OOM path already refuses
  such a request terminally, so the question is what a small slice *should* do.
- **F5 — `contention="unevaluated"` does not say why** (deadline before the first
  pass vs a slice memory read failing for the whole wait). Both are honest as
  `unevaluated`; naming which would need a queue-level record of the early
  return.
- **F6 — the in-wait progress line is unchanged.** Adding "your resolved reserve
  is the whole ceiling" to each 15 s tick would surface the fact sooner than the
  terminal message; deferred as a separate diagnostic decision, and the line as
  it stands is true.
- **F7 — accepted coverage gap:** no real-cgroup test drives the wedge (§7.4).

---

## 9. Files touched at implement time

| File | Change |
| --- | --- |
| `internal/daemon/admit.go` | `resolveAdmitReserve` basis (§3.1, §3.4); `admitRejection` gains `Contention` + `Grantable`; `admitWaiter` gains the latch fields; `evaluateAdmitQueue` refusal branches write them; the two `writeAdmitRejection` saturated call sites populate `Required`/`Ceiling`/`Contention`/`Grantable` |
| `internal/runner/admission_linux.go` | `runnerAdmitRejection` mirrors the two fields; the `default:` arm of the exclusivity switch becomes the three-way contention render |
| `internal/core/skill.go` | the agent-guide sentence about `estimate:oom-escalated` |
| `internal/daemon/admit_oom_basis_test.go` | new — T1, T2, T3 |
| `internal/daemon/admit_saturated_diagnosis_test.go` | new — T7–T12 |
| `internal/runner/admission_saturated_message_test.go` | new — T13–T17 |
| `internal/daemon/confine_admit_test.go`, `sliceceiling_test.go`, `confine_oom_selfheal_real_cgroup_linux_test.go`, `internal/core/skill_test.go` | T4, T5, T6 updates |
| `.aira/tickets/AIRA-149.md` | status, resolution, and the §8 links |

No changes to `checkedAvailable`, the clamp arithmetic, the ceiling computation,
the freeze, the aggregate bound, the ledger, the protocol version, any CLI flag,
or any MCP/Skill surface.

---

## 10. Questions this plan expects the gate to press on

1. **Is `oom-on-record` new machinery the simplicity rule should refuse?** The
   answer this plan gives: it is one comma param in an existing basis grammar,
   and it exists to *preserve* a verified property (§3.2) rather than to add one.
   The alternative — dropping it — silently degrades AIRA-128's attribution test.
   If the gate disagrees, the fallback is to keep `estimate:oom-escalated`
   verbatim in every row and fix only the diagnosis; that leaves D1 unfixed and
   should be said out loud, not settled by omission.
2. **Is a latched three-state contention field too much state for a diagnosis?**
   The cheaper alternative (read the queue at rejection time) is refuted by T10:
   it would report "nothing was in the way" for a wait that spent almost all of
   itself behind a real job.
3. **Should `Ceiling` on a saturated rejection be the request-entry ceiling or
   the gate's last-pass ceiling?** This plan sends the request-entry one (the
   same number the terminal `E_ADMIT_TOO_LARGE` boundary used, and a stable
   property of the request) and labels the grantable figure separately by its own
   instant. Sending a third number was judged not to earn its place; the gate may
   disagree.
4. **Is direction 3 enough, given the wait still cannot end?** §2.4 answers on
   the AIRA-71 precedent and files the structural candidate with its evidence.
