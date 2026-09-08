---
{"schema":1,"id":"AIRA-189","project":"aira","title":"aira spend records a model field on write but computeDistribution's --by never supports grouping by it","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["spend"],"hold":false,"relations":[]}
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
