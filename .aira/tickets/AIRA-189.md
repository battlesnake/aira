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
