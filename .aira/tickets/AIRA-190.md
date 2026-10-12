---
{"schema":1,"id":"AIRA-190","project":"aira","title":"No reserved-but-not-allocatable prefix mode -- a project wanting aira to recognise its own external ids has to either own allocation or own nothing","status":"retired","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["init","ownership"],"hold":false,"relations":[]}
---

Peer proposal (devproc, 2026-09-08, refined via an adversarial review from
a second model on their side). Companion to [[AIRA-188]] (the immediate
bug: bare `aira init` silently claims the hardcoded "AIRA" prefix) and its
own follow-up (a telemetry-only `--no-tickets` project needs no prefix at
all). This ticket is the third, architecturally larger case: a project
that *does* want aira to recognise references to its own external ids
(citation, not allocation) — currently unsupported. Not designed here;
evidence and a sketch only, matching how [[AIRA-178]] was handled.

## The gap

Prefix ownership today is binary: a project either registers a prefix and
can allocate through it (`aira id`/`aira create`), or it doesn't register
it and aira knows nothing about it. There's no middle state — "recognise
and validate references to this prefix, but never hand out a number for
it" — for a project with its own external allocator (e.g. fastest-ee's
`scripts/next_id.sh`) that would still like aira to treat `BL-1129` as a
first-class, cross-checkable reference rather than an opaque string.

## What the reporter's own review caught, worth preserving precisely

Two correctness nuances that a naive implementation would miss:

1. **Refusal must scope to an UNQUALIFIED lookup, not to storing a
   project-qualified reference.** A qualified form (their proposed
   `FEE-BL-1129`, containing `BL-1129` verbatim so grep still works) and a
   prohibition on *allocating* through the prefix are complementary
   controls, not alternatives — conflating them (as the reporter initially
   did, before correction) would wrongly block legitimate citation.
2. **Enforcement has to cover every allocation and creation path** — not
   just `aira id`, but `aira create`, imports, mode changes, and
   concurrent access — with explicit transition rules for a prefix's
   existing counter state if a project moves between "allocates" and
   "reserved-only" modes. Rejecting `aira id` alone leaves the invariant
   incomplete; a caller could still create tickets some other way and
   silently violate it.

## Adjacent, orthogonal, not needed by the reporter

Compound prefixes (`FEE-BL-1129`) are the answer to genuine cross-project
prefix collisions (two projects independently wanting the *same* bare
prefix) — orthogonal to this ticket's reserved-but-not-allocatable mode,
not an alternative to it. Not needed by the reporter's own case (they
have no prefix collision, only a citation-without-allocation need) but
worth keeping in mind as the same underlying qualified-reference shape
would likely serve both.

## Status

Not designed. Evidence and a sketch only, for whoever picks this up next.

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** retired. **State on master v0.34:** still-open.

The need is already satisfied: the one named consumer (fastest-ee) cites its own BL-N ids through the id_prefix + import path, and aira never allocates for them. Building the mode would add a third project mode that `id`, `create`, import, `gate` and `lease` must all refuse on, plus counter-transition rules (the AIRA-188 hazard), for a consumer that no longer exists. That is a large, correctness-adjacent new mechanism (ID allocation) with no live need. It runs against "primitives not judgement", architectural simplicity and "keep the primitive + document the gap". Close as won't-fix/satisfied-by-AIRA-237. Re-file with a concrete consumer if one appears, and fold it together with the deferred AIRA-188 `--tickets none` mode (one named project mode, not two booleans), under the two-loop.

**Evidence:** No reserved-only/references-only prefix mode exists. A grep for references-only, reserved prefix and reserved-only finds no code. prefix_ownership (internal/store/store.go:768, 2041-2060) carries only an entity-kind column (ticket/finding/...), and registerDB always creates an id_counters row, so every registered prefix is allocatable. Newly-created prefixes are explicit-only since AIRA-188 (internal/app/project.go:475-478 refuses an unset --prefixes). The reporter's actual use case (fastest-ee owns BL-N allocation externally, wants aira to recognise its ids) was met by AIRA-237/v0.8. Project.id_prefix composes FEE-BL-N (internal/app/project.go:89-94), and `aira import --tickets` preserves externally-allocated ids and never mints them (internal/store/import_tickets.go:15-24). The ticket is status planned, "Not designed here", and carries no relations. The v0.32 spec (docs/superpowers/specs/2026-10-10-aira-v032-small-bugs-design.md:84,557) explicitly deferred the AIRA-188 `--tickets none` mode as "belongs with AIRA-190". No consumer has asked for it since.
