---
{"schema":1,"id":"AIRA-237","project":"aira","title":"aira: id-accepting ticket-import verb + configurable project-prefix (fastest.ee coordination substrate)","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["import"],"hold":false,"relations":[]}
---
PLANNED — captured from a live owner+peer design reconciliation (2026-09-13). DEFERRED behind the owner's shape decision; do NOT build until greenlit AND the shape is chosen. Peers: subpipe (fastest.ee-side extractor/adoption), speed (routing/mgmt). This ticket is the aira-SIDE substrate (aira development = my lane); the fastest.ee-side extractor is subpipe's.

## Problem
fastest.ee wants aira as a cross-session coordination layer (leases / blocked-by ready-queue / worktree-bindings / relations) over its EXISTING backlog (BL/NF/VR/UX/TR/KB/... ~21 prefixes across docs/backlog.md + REQUIREMENTS.md, allocated by `make id`). aira today has NO path to ingest externally-allocated ids as tickets (create rejects --id and auto-mints; `aira import` is findings-only; `req import` is requirements-kind + format-brittle; `aira id` burns numbers without tickets). NOTE: my earlier 'external-allocator posture' claim citing compute.go:62 was WRONG — that comment is about the free-form ticket_id FIELD on spend/run telemetry, not ticket allocation.

## Owner shape decision (spectrum; speed leaning (2))
- (b) index-only: make id + backlog.md + traceability gate stay fastest.ee's; aira only coordinates.
- (2) allocator-migration (speed's lean): aira becomes the ALLOCATOR (make id -> aira id, retires) + coordinates, but backlog.md/REQUIREMENTS.md stay AUTHOR-WRITTEN and the traceability gate runs unchanged (references aira-issued ids). minting != owning.
- (3) full migration (LATER, additive): aira owns the items; backlog/REQUIREMENTS become aira-generated; the traceability dogfood re-sources against aira.
The import verb is needed in ALL three (nothing throwaway); (3) is a pure additive follow-on over (2) with NO verb rework. Only real fork = traceability-gate source-of-truth (deferrable).

## Component 1 — id-accepting ticket-import verb
- Input: clean JSONL, one obj/line {id, title, status, body, labels, links:[{kind,to}]} — decoupled from fastest.ee markdown (their thin extractor emits it; aira never parses their files).
- PRESERVE id verbatim (never mint); VALIDATE prefix-owned + no number collision.
- FIELD-SCOPED IDEMPOTENT UPSERT: re-import REFRESHES backlog-owned fields (title/status/body/labels = their truth) but NEVER clobbers aira-owned coordination state (live lease, worktree-binding, in-aira relations).
- STATUS = force-set-on-import, bypassing ValidateTransition (backlog.md is truth) — same as ImportRequirements. A row DISAPPEARING -> retire + report, NEVER hard-delete (a live lease may exist).
- Links to not-yet-imported ids: two-pass (create all, then link) for order-independence.
- Model on ImportRequirements (import_requirements.go) — id-preserving upsert pattern exists there; generalise to ticket-kind + all prefixes + tolerant of a clean structured input (do NOT re-couple to a brittle markdown-table parser).

## Component 2 — configurable project-prefix (FEE) + repo-visibility switch
- Owner design: the project's .aira/config carries the prefix (e.g. FEE) AND a switch: does the prefix appear in the repo's own ids, or is it aira-only? fastest.ee = aira-only (files stay BL-123, never type FEE); a project wanting FEE in its ids = in-repo.
- Internally aira always stores the full unique id; the switch sets only the repo-facing face. NECESSARY (not cosmetic): prefixes are owned MACHINE-WIDE (prefix_ownership PK = prefix; E_PREFIX_OWNERSHIP_CONFLICT), so bare BL/NF collide across projects on the box — FEE- namespaces the set.
- Parser needs NO change: splitTicketID splits on the LAST '-' (store.go:3458), so FEE-BL-123 -> prefix 'FEE-BL', number 123 for free.
- Relax the prefix CHARSET validator(s) to allow the separator: validPrefix (A-Z only, store.go:3464, E_ID_INVALID at store-init) is CONFIRMED one site; subpipe's spike hit E_CONFIG_INVALID at config-parse implying a SECOND site — PIN THE EXACT VALIDATOR SET when speccing (1 confirmed, probably 2).
- CLI ergonomics: auto-prepend the prefix on input, strip on repo-facing display. Cost risk = whether selector-input is ONE CLI chokepoint or many (untraced; pin when speccing).

## Component 3 — allocator seed (load-bearing under (2)/(3))
- GROUNDED: aira mints from a per-prefix cursor id_counters(project_id,prefix,next_number) — allocator reads it at store.go:3353 and bumps next+1 at :3364. Import seeds it via a high-water-mark upsert (import_requirements.go:420-422; a second HWM upsert on the reconcile/receipt path store.go:3142-3144). So seeding BL-1..BL-100 raises the cursor to 101 and the next `aira id BL` mints BL-101 — no stomp. The new ticket verb MUST replicate that id_counters HWM upsert (proven pattern; a REQUIREMENT of the verb, not free).

## BUILD CHECKLIST (carry into the spec)
- [ ] E2E ROUND-TRIP TEST (subpipe's flag — the one link confirmed by source-read but NOT run end-to-end, and exactly what (2) rests on): import/seed existing ids -> `aira id <prefix>` mints the next free number -> author a row with that aira-issued id -> NO stomp of imported ids. This is load-bearing under (2); must be an explicit test.
- [ ] idempotent re-import preserves a live lease + worktree-binding + in-aira relations (field-scoped upsert).
- [ ] force-set status bypasses ValidateTransition; disappearing row retires (not deletes).
- [ ] FEE aira-only: files/display show BL-123, store holds full unique id; in-repo mode shows FEE-BL-123.
- [ ] pin + relax the exact prefix-charset validator set (>=1, likely 2).

## Deferred / owner-decision items
- The shape (b/2/3) — owner's call via speed. Spec to the chosen shape.
- Status-vocabulary mapping (fastest.ee statuses -> aira 7-state enum): extractor emits aira-canonical, or aira config carries a map.
- Per-topic ownership: aira has per-TICKET leases only; hard topic->session enforcement is net-new machinery — recommend CONVENTION (prefix/label = topic + lease) over building it, unless a concrete need arises.
