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

## MEASURED ROOT CAUSE (2026-09-20) — REFUTES the premise above
Investigated under systematic-debugging. The peer's reproduction (`--hold --milestone` + long multi-line `--body` + repeated `--label`) does NOT reproduce a phantom on a scratch project — it succeeds. The real mechanism, from the machine-wide store (`state.db` `allocations`):

| Ticket | Materialised path | Fate |
|---|---|---|
| FK-43, FK-44, **FK-47** | `/home/mark/claude/flavour-kernel/.aira/tickets/` (main checkout) | persist ✓ |
| **FK-45, FK-46** | `/home/mark/tmp/fk-adaptive/.aira/tickets/` (throwaway worktree, since **removed**) | lost ✗ |

Both FK-45/46 are `state='materialised'`, outbox `materialised=1` — the creates **genuinely succeeded** and honestly returned `ok:true` + the id. They were NOT phantoms and there was NO fake-success-on-failure (the create handler at `core.go:776` returns `nil, err` with no id on real failure). The `.aira/tickets/*.md` are per-worktree **git working-tree files**, indexed per `worktree_id` (`query.go:322/365`), read back via `os.Lstat(ticketPath)` (`query.go:345`). FK-45/46 were created from the `~/tmp/fk-adaptive` worktree; they were never committed, so removing that worktree deleted the files. `aira get FK-45` now reads the recorded (gone) path → bare `E_NOT_FOUND: selector matched no tickets`.

**The variable the peer attributed to flags (repeated `--label` / long body) was actually the working directory** — FK-45/46 from a throwaway worktree, FK-47 from the main checkout. Flags were a red herring.

**So: not a data-integrity bug, not an honesty bug.** The one genuine (small) gap: `aira get`/`list` collapse "created-then-file-lost (known-materialised at a gone path)" into the same `E_NOT_FOUND` as "never existed", even though the machine-wide `allocations` table still knows it was materialised. Disposition (close-as-designed+document vs add an honesty surface vs durability change) put to the owner 2026-09-20.
