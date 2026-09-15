---
{"schema":1,"id":"AIRA-235","project":"aira","title":"aitest v0.7 S2b: largest-first per-test worker sizing (requested=given + bin-packing dispatch)","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","v0.7"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-238","to":"AIRA-235"},{"kind":"relates","from":"AIRA-239","to":"AIRA-235"}]}
---

Stage S2b of the aitest v0.7 design. Replaces the flat 512 MiB per-worker
reservation with **per-test sizing scheduled largest-first**: each worker is
sized to the largest ready test that fits current quota, and idle workers pull
the largest queued test that fits their reservation (retiring when none fits so
the master can repack). Big tests launch early (quota maximal ⇒ no tail-latency
starvation); small tests fill the gaps and keep cores busy.

Plan: `docs/superpowers/plans/2026-09-13-aitest-v07-s2b-largest-first-plan.md`.

**Pure supervisor change** — the worker-admit path already grants the requested
`estimated_bytes` verbatim (the history-sizing `resolveAdmitReserve` is
confine-only). Overhead = the warm-import baseline (`aira_mem` is incremental on
top of it, spec 4.1); default 512 MiB = today's flat reserve, provably safe.

DEFERRED (not this release): daemon `requested=given` for the ordinary `aira
confine` path (a separate fleet-wide density change); CPU-time weighting;
config-TOML; single-test mode; usage-history file; under-size warning.
