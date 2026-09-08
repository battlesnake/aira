---
{"schema":1,"id":"AIRA-165","project":"aira","title":"E_ADMIT_TOO_LARGE prints raw bytes and names no escape hatch, on a now-narrower population","status":"done","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-165","to":"AIRA-153"},{"kind":"relates","from":"AIRA-165","to":"AIRA-151"}]}
---

AIRA-151 deferral **G3**, carried forward by AIRA-153 deferral **G6** and now
given its own ticket rather than living inside another ticket's resolution.

The operator-facing message is
`E_ADMIT_TOO_LARGE: required=%d cap_minus_headroom=%d basis=%s`
(`internal/daemon/admit.go`), in raw bytes, naming no escape hatch. Two
complications are why neither AIRA-151 nor AIRA-153 touched it:

1. the string is shared with PINNED requests, where "pin `--memory-reserve`" is
   exactly the wrong advice;
2. `cap_minus_headroom` is the ENTRY ceiling, a transient that moves by one
   per-job headroom term for every job outstanding at arrival, so a request
   marginally over it is refused with advice not to retry even though the
   ceiling may rise once a neighbour finishes.

AIRA-153 made the population NARROWER and more uniform, which is what makes the
case analysis tractable now. After it, a terminal refusal can only be:

- an over-ceiling per-signature ESTIMATE (this command's own measurement);
- an OOM ESCALATION whose recorded peak is at or above `FIT(ceiling)`;
- a PINNED request above the ceiling.

"Pin at or below `cap_minus_headroom`" is correct advice for the first two and
wrong for the third, so the case analysis the deferral was taken for is still
required — but it is now a three-way split with no blind-prior case in it.

`TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis` pins the current string,
so a successor starts from recorded evidence rather than a paraphrase.

## Resolution (2026-09-08)

The ESCAPE-HATCH half is fixed: the advice is now case-split by the population
that produced the refusal. The RAW-BYTES half is considered and DECLINED, with
the reason recorded below rather than left as an unstated omission.

**No admission decision changed.** `resolveAdmitReserve`, `checkedAvailable` and
`admitConnection`'s `reserve > ceiling` boundary are byte-for-byte untouched;
every resolved reserve, basis, grant and refusal is exactly what master
produces. The whole change is the TEXT appended to one operator-facing message,
plus one generated-guide clause (AIRA-166) and their tests.

### The split, and what decides it

`internal/daemon/admit.go` gains one pure function, `tooLargeRefusalAdvice`,
called only from `writeAdmitRejection` and only for `CodeAdmitTooLarge`. The
numbers half is unchanged and unconditional — `required`, `cap_minus_headroom`
and `basis` keep their exact spelling and position, and the advice is APPENDED
after ` -- `, never substituted for them, so every existing reader of that line
is unaffected.

The case is decided by the basis TERM, on AIRA-149's own rule that a label names
the term that ACTED. The basis is `family:name[:params][,token...]`; trailing
tokens (`,oom-on-record`, `,ceiling-fitted`, `,ceiling-clamped`) qualify a term,
they do not replace it, so they are trimmed before the match rather than pattern-
matched around. That is the information the call site already had, carried by
provenance rather than re-derived by comparing numbers.

| basis term | population | advice |
| --- | --- | --- |
| `pinned:client` | the operator's own `--memory-reserve` / `--memory-max` / `--delegate-ram` | lower it to at most `cap_minus_headroom`, or use a larger slice — explicitly **not** "pin", which is what they already did |
| `estimate:oom-escalated` | this command's own OOM at or above `FIT(ceiling)` | it does not fit on this slice; run where the slice is larger rather than retrying it unchanged |
| any other `estimate:` | this command's OWN measured peak history | pin a SMALLER reserve only if you know the real need is smaller, otherwise it cannot run here |
| `estimate:p90-prior` | a machine-wide prior about OTHER commands | named as a prior, not a measurement; pin a reserve you know fits, or use a larger slice |
| `fallback:*` | AIRA's blind unpinned default | same, named as a blind default |
| anything else | — | **no advice at all**, numbers and basis only |

### Why FOUR arms where the ticket enumerates three

The three populations above are AIRA-153's, and they are the whole population on
an ordinary slice. The fourth arm exists because AIRA-153 §3.2 deliberately
routes one more here: when `FIT(ceiling)` falls below `runner.MinPinnedScopeCap`
the fit does not fire at all, and an UNFITTED prior — the 4 GiB default, or the
machine-wide p90 — reaches this refusal (`TestASliceTooSmallForAnyViableReserve
StillRefusesTerminally` is that population, already shipped). The same shape is
reachable on an ordinary slice through the enqueue-time re-check, where the
ceiling can have tightened behind a request between resolution and enqueue.

Telling that operator that "this command's own measured peak history" is too
large would be a FABRICATED cause: a prior is a guess about other commands, and
this function must never claim it measured one. So the fourth arm is required
for honesty, not for completeness — and `estimate:p90-prior` gets it despite its
`estimate:` family, which is the one case where classifying on the family rather
than the term would say the wrong thing.

An UNRECOGNISED basis gets no advice rather than a plausible-looking guess, on
the same rule that makes an unestablished check `unevaluated` rather than a
pass. The refusal still names both numbers and the basis.

### Tests

- `TestTooLargeRefusalAdviceIsCaseSplitByPopulation`
  (`internal/daemon/admit_too_large_advice_test.go`) — all five arms driven END
  TO END through the real `admitConnection` on real fixtures (the AIRA-139/149/
  151 1 GiB slice and AIRA-153 T8's degenerate 1205862 ceiling), each asserting
  the exact `required`/`cap_minus_headroom`/`basis` prefix, its own advice
  phrase, and the ABSENCE of every other arm's phrase — so a one-size-fits-all
  regression fails on four of five rows rather than passing on the one it
  happens to describe. Plus the direct negative: the pinned arm never tells the
  operator to pin.
- `TestTooLargeAdviceIsDecidedByTheTermNotTheTrailingTokens` — the classification
  rule over 19 basis spellings, including every token combination and three
  unrecognised bases that must produce NO advice.
- `TestEveryRefusableResolutionGetsAdvice` — the anti-porousness half: over
  AIRA-153's own grid (8 history shapes x 11 ceilings x 2 p90 states x both
  pinning arms), every resolution that WOULD be refused (`reserve > ceiling`)
  must land on a real arm, never on the silent default. 112+ refusals are
  asserted to exist so the grid cannot pass vacuously.
- `TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis`
  (`internal/runner/admission_saturated_message_test.go`) — rewritten from the
  one-size pin to the four-population pass-through, which is the half only the
  CLIENT can establish: each arm reaches the operator intact rather than being
  rewritten the way `E_ADMIT_SATURATED`'s message is. Its G3 assertion is
  INVERTED — it used to assert the message named no escape hatch (the recorded
  gap); it now asserts each arm names one, and that the pinned arm does not say
  "pin".

### Mutation evidence

Each applied alone in the worktree, run, then reverted:

| # | mutation | RED |
| --- | --- | --- |
| M1 | do not append the advice (master's exact behaviour) | all five rows of the wire test |
| M2 | one-size advice for every basis (the defect, restated) | four of five wire rows, the pinned negative, and 18 of 19 term rows |
| M3 | match the `estimate:` FAMILY before the `estimate:p90-prior` term | exactly the two p90 rows and nothing else |
| M4 | drop the trailing-token trim | exactly the `,ceiling-clamped` and `,ceiling-fitted` rows |

### The raw-bytes half: declined, with the reason

`runner.FormatConfineBytes` renders a suffix only for EXACT multiples of a unit
and otherwise returns the number unchanged, so it would print
`required=897216333 (897216333)` for precisely the fitted and estimated figures
this refusal carries — the numbers that most need rendering are the ones it
cannot render. Making them readable needs a SECOND, rounding size formatter
beside the existing one, which is new machinery and a second inconsistent
rendering of the same quantity across the product, for a display nicety. Against
the architectural-simplicity rule that is not worth it, so the numbers stay raw
and this is a decision rather than an omission. A successor that wants it should
change `FormatConfineBytes` itself, once, for every surface.

### Not done, deliberately

- **Complication 2 (the transient entry ceiling) is untouched.** `cap_minus_
  headroom` is still the ceiling at request entry, and a request marginally over
  it is still refused terminally even though a neighbour finishing would raise
  it. Fixing that is an admission-DECISION change (re-evaluation, or a
  retry-after hint), not a wording change, and this ticket was scoped to say
  accurate things about the decision that was actually made.
- **The generated guide's own `E_ADMIT_TOO_LARGE` sentence is unchanged.** It
  already names both populations it describes and its advice ("pin at or below
  the printed `cap_minus_headroom`, or run where the slice is larger") does not
  contradict any arm above — for a pinned request "pin at or below" and "lower
  it to at most" are the same instruction. Rewriting it would have been drift,
  not a fix.
