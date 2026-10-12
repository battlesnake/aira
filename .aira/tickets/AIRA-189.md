---
{"schema":1,"id":"AIRA-189","project":"aira","title":"aira spend records a model field on write but computeDistribution's --by never supports grouping by it","status":"retired","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["spend"],"hold":false,"relations":[]}
---

Peer report (devproc, 2026-09-08), verified from source.

## Verified from source

`aira spend add` requires and persists a `model` field —
`domain.ComputeEvent.Model` (`internal/domain/compute.go:62,97`) is
validated as required on every write (`compute.go:212`: empty `Model` is
rejected). But `computeDistribution` (`internal/core/core.go:2440-2463`),
the function backing `aira spend ls --by <field>`, has a hardcoded
allowlist of exactly `provider`/`ticket`/`phase`/`conservation` — `model`
is not one of them, so `aira spend ls --by model` fails
`E_SELECTOR_INVALID: unsupported compute distribution field "model"`,
even though every event already has the field on disk.

Reporter's use case (cost comparison between models) is blocked by this
specifically, though not urgently — they have a workaround (join via
`--ticket`, since `ticket` is already a supported `--by` value, combined
with their own separate ticket-to-model mapping).

## Not designed here

Whether `model` should simply be added to `computeDistribution`'s
allowlist (the obvious, minimal fix — the field already exists on every
row, `computeDistribution`'s switch just needs one more case) or whether
this points at a broader pattern worth checking (are there other
write-side fields on `ComputeEvent` with no corresponding `--by` support?)
is left for whoever picks this up. Looks like a small, contained fix
either way.

## Follow-up (devproc, 2026-09-08) — the ticket-join workaround does not
actually substitute for this; sharpens the justification

The reporter initially described their own `--ticket`-join workaround
(ticket → their own build/review/gate-to-model mapping, joined against
`aira spend --by ticket`) as sufficient. On reflection they withdrew
that: a single ticket's compute events legitimately span multiple models
(a Sonnet build, an Astra review, a Fable gate all against one ticket),
so a ticket-level total cannot be correctly split across models by any
join — the join only has ticket-level granularity, not event-level. Since
`Model` is required and recorded on every individual event, true
per-model cost is only correctly computable as an event-level
aggregation, which is exactly what `--by model` would give directly.
This makes the fix a genuine correctness gap for that use case, not
merely a convenience the join already covers.

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** retired. **State on master v0.34:** still-open.

The minimal fix (add a `model` case) is a 3-line change but yields only event counts, which is not the per-model cost the ticket claims it needs; the real need is met today by grouping the event-level `rows` that `spend ls` already returns (model + cost_usd + buckets per row), and a proper SpendByModel cost aggregation would be a new feature with a missing-cost honesty design for a P3 reporter convenience. Under 'primitives not judgement' and 'keep the primitive, document the gap', close as wontfix and add one line to the `spend ls` help noting that rows carry model/cost_usd for client-side grouping. If the owner wants allowlist parity anyway, it is a trivial `case "model": value = row.Model` plus the allowlist entry and a test, but it should not be sold as a correctness fix.

**Evidence:** Still reproduces on master. internal/core/core.go:2781-2784 computeDistribution rejects any `by` not in provider/ticket/phase/conservation with E_SELECTOR_INVALID; `model` is only a write-side field (domain/compute.go:92, required at :257). Query filters (internal/store/compute.go ~line 497) also accept only ticket/phase/provider/session, so there is no `model:` filter either. git log --all --grep AIRA-189 shows only the two filing commits (24e39b6, 6d61c5e); no fix commit. Facts that undercut the ticket's justification: (1) the `spend ls` response always returns the full event-level `rows` (core.go:1322-1333 returns total+rows+distribution; ListComputeEvents has no limit), and every row carries model, cost_usd and the four buckets, so event-level per-model aggregation is already possible client-side with one group-by; the follow-up's 'only computable event-level' is satisfied by `rows`. (2) The distribution is an event COUNT map[string]int (core.go:2785-2803), so adding a `model` case would give events-per-model, not per-model cost; the reporter's stated need (cost comparison) would still need a sum. (3) cost_usd is optional/caller-supplied, so a true per-model cost sum would need an honesty design (missing cost = unevaluated, not 0) like SpendBySession's lower-bound note (core.go ~2775). (4) `--by phase` is intercepted at core.go:1311 before computeDistribution, so that case is already dead code; the list is already uneven.
