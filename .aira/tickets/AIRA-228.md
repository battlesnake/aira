---
{"schema":1,"id":"AIRA-228","project":"aira","title":"Admission core: delete inherited over-engineering / scope-creep (single-queue collapse + generality audit)","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["admission","simplify","tech-debt"],"hold":false,"relations":[]}
---
Backlog cleanup (owner-directed 2026-09-11): after the admission-counter rebuild lands, remove generality the daemon carries for a multi-slice world that was never planned, and audit the admission/confine code for other scope-creep to delete.

PRIMARY ITEM — collapse per-slice-PATH queues to a single queue:
- The admission code is structured as `map[path]->sliceQueue` with per-queue locks and prune/recreate-per-path. This is pre-existing v0.5 generality for multiple concurrent slices. We have never planned multiple slices: `aira install` bakes exactly one capped `aira.slice`, every `aira confine` targets it, and nothing in the design/tickets/plan introduces a second.
- Surfaced during the counter rebuild: extending this multi-queue shape to CPU (a machine-wide `cpuByQueue` map + `cpuLedgerMu` + `signalAllQueues` cross-queue wake) was over-engineered and BRED a measured cross-slice CPU over-admit (a pruned queue's stale pass deleted the successor's entry). Fixed in the rebuild by asserting one-slice and making CPU a per-slice scalar (D1 re-resolved to per-slice). The deeper per-path STRUCTURE remains.
- Cleanup: collapse to a single queue for `aira.slice` — delete the per-path map, the prune/recreate-per-path machinery, and any remaining cross-queue plumbing. Simplifies RAM too and removes a whole class of prune/recreate complexity. Keep a clear assertion/guard: one slice; if multi-slice is ever genuinely needed, this is where it comes back.

BROADER SWEEP — apply the "simplify and challenge before building" ethos RETROACTIVELY: audit the admission/confine code for other inherited generality or scope-creep that serves no real need (abstractions with one caller, config knobs nothing sets, multi-X support for a single X) and delete it.

Scope note: this is a SEPARATE refactor from the counter-model rebuild (which deliberately keeps the existing structure to keep its diff about the admission model, not the plumbing). Do it after the rebuild merges. Owner chose (a) finish-then-cleanup over folding it into the rebuild.
