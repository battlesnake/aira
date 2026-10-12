---
{"schema":1,"id":"AIRA-282","project":"aira","title":"aira.mk: shared confine-step Makefile macro wrapping confine --summary-file","status":"retired","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.29","labels":[],"hold":true,"relations":[]}
---


Held until the summary-file ticket ships. A small `aira.mk` include with a `confine-step` macro: name + reserve + `--require-admission` + `--summary-file`, so repos do not hand-roll it. Requested by field/Stoner and spice/flavour.

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** retired. **State on master v0.34:** still-open.

Greenfield minimum: the macro is about four lines of make, `aira confine --name $(1) --memory-reserve $(2) --require-admission --summary-file $(SUM) -- $(3)`, and every flag already exists. All the hard parts (append-safe JSON line, ran:false for refused jobs, unevaluated-never-0, wall/cpu) shipped in AIRA-281. The only new thing a shared include adds is a distribution mechanism (embed in the binary and have `aira install` or a new verb place it, or have repos `include` a path from the installed tree). That is new machinery and a new versioned contract for ~4 lines, and the right reserve, name scheme, summary path and fresh-file-per-run policy differ per repo (Stoner vs flavour). AIRA is primitives, not judgement, and a Makefile macro is repo CI policy. Cheapest way to satisfy the requesters: add a 5-line copy-paste snippet (with the unlink-and-recreate and omit-empty-tree-hash notes from the AIRA-281 spec) to the Skill/docs next to the --summary-file paragraph, or let the requesting repos own the macro themselves. If the owner still wants a shipped include, that is a distribution-policy decision, but the default recommendation is close as wontfix and replace with the doc snippet (a trivial docs-only change, no core.Do or daemon touch).

**Evidence:** .aira/tickets/AIRA-282.md: status planned, hold:true, milestone v0.29 (stale; master is v0.34), body is two sentences, no relations (the blocks link lives on AIRA-281). Prerequisite AIRA-281 is status done: merged as e2ac078 (PR #194), `--summary-file` / `--summary-tree-hash` in internal/core/core.go:1927 and internal/runner/confine_summary.go, documented in the Skill text at internal/core/skill.go. `git ls-files` shows no aira.mk or any .mk file; grep for `confine-step` / `aira.mk` finds only the ticket and the AIRA-281 spec (spec lines 5-6, 61, 394, 414, 491 which only note "AIRA-282 macro: omit the tree-hash flag rather than pass an empty one; unlink-and-recreate the summary file, never truncate"). `git log --all --grep AIRA-282` gives only ticket-bookkeeping commits (82a217c, ee4a4c1). So not built and not obsolete, merely unbuilt and held.
