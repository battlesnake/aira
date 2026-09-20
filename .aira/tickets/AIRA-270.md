---
{"schema":1,"id":"AIRA-270","project":"aira","title":"aira create: allocates a ticket id but does not persist the ticket (phantom id), and prints the id on failure as if it succeeded — data-integrity + honesty bug (reported by flavour-kernel-c2, 2026-09-20)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["bug"],"hold":false,"relations":[]}
---

Reported by peer session flavour-kernel-c2 (repo /home/mark/claude/flavour-kernel), 2026-09-20. NOT blocking (peer re-created under new ids); phantom ids are harmless sequence gaps, no cleanup needed on their side.

## Symptom
Two `aira create` calls CONSUMED ticket ids but did NOT persist the ticket: `aira get FK-45` / `FK-46` → `E_NOT_FOUND`, yet the counter advanced past them (next create got FK-47). AND — the load-bearing honesty bug — the failing creates still printed `"id":"FK-45"` in their output, so a caller that greps the id reads a FAILURE as success. That is exactly the fake-success AIRA's "primitives, not judgement / honest" discipline forbids: a create that cannot persist must report `ok:false` and emit NO id (or roll back the counter).

## Candidate triggers (from the peer; NOT yet measured by me)
- Both failing creates: `--hold --milestone engine-adaptive-tran` + a long multi-line `--body`.
- FK-45 additionally passed TWO `--label` flags (`--label solver --label checkpoint`); FK-46 one (`--label solver`).
- A later create with a single `--label` + long body (FK-47) persisted fine; FK-43/44 (simpler) fine. So: repeated `--label`, and/or the long/multi-line `--body`, and/or an allocate-before-write ordering.
- Note: THIS ticket (AIRA-270) was created with a single `--label bug` and NO `--body` and persisted fine — consistent with the trigger being `--body` and/or repeated `--label`.

## The two asks (fix scope)
1. **create atomicity + honest failure output** — a create that fails after allocating an id must either roll back the id counter OR report `ok:false` and NOT emit an `id`. The current fake-`id`-on-failure is the priority (honesty).
2. **repro** the trigger (repeated `--label` / long multi-line `--body`) on a SCRATCH project (NOT flavour-kernel), then fix.

## Sequencing
Captured while v0.24 (AIRA-269, aira top VRAM bar) is mid-build. Fix this AFTER v0.24, via the two-loop (id-allocation + persistence atomicity is correctness-critical). Surfaced to owner for prioritization.
