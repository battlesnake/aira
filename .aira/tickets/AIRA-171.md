---
{"schema":1,"id":"AIRA-171","project":"aira","title":"Nine ticket files on master are unreadable: unsorted relations, non-canonical relation storage, unsorted labels, and bodies with no trailing newline","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","tickets"],"hold":false,"relations":[]}
---
Found by the AIRA-170 work-review's real-files probe (Fable, PR #105), which
drove the store over this repository's own `.aira/tickets` rather than over
synthetic fixtures. Pre-existing on master; not caused by AIRA-169/170.

Nine hand-written ticket files are refused by `domain.ParseTicket`, so
`aira show` on each answers an error instead of the ticket, `aira list` silently
omits them, and `aira rant --ref ticket:<id>` cannot reference them. Each defect
is a hand-editing slip in frontmatter that no writer path would have produced.

| file | refusal |
| --- | --- |
| `AIRA-28.md` | `E_RELATION_INVALID: relations must be sorted` |
| `AIRA-62.md` | `E_RELATION_INVALID: relation is not stored on its canonical lower-ID ticket` |
| `AIRA-117.md` | `E_TICKET_INVALID: ticket labels must be unique and sorted` |
| `AIRA-141.md` | `E_CONFIG_INVALID: ticket body must end in newline` |
| `AIRA-144.md` | `E_TICKET_INVALID: ticket labels must be unique and sorted` |
| `AIRA-145.md` | `E_CONFIG_INVALID: ticket body must end in newline` |
| `AIRA-152.md` | `E_RELATION_INVALID: relation is not stored on its canonical lower-ID ticket` |
| `AIRA-153.md` | `E_RELATION_INVALID: relation is not stored on its canonical lower-ID ticket` |
| `AIRA-160.md` | `E_TICKET_INVALID: ticket labels must be unique and sorted` |

The four relation cases are not one repair. Each was read at filing:

- **`AIRA-28.md` is a pure re-order.** It holds `supersedes AIRA-29->AIRA-28`
  before `relates AIRA-62->AIRA-28`; `relationLess` sorts by KIND first and
  `relates < supersedes`, so the two entries simply swap. Both are already on
  their canonical owner. No content changes.
- **`AIRA-62.md` is a lossless deletion.** It stores `relates AIRA-62->AIRA-28`
  on itself, but the canonical owner is the lower id, `AIRA-28` — which already
  holds the byte-identical tuple. Delete the copy on AIRA-62.
- **`AIRA-153.md` is one deletion, one mirror question and one MOVE.** It stores
  `153->150`, `153->151` and `153->152` on itself.
  - `153->150`: `AIRA-150.md` already holds the identical tuple. Lossless delete.
  - `153->151`: `AIRA-151.md` holds `151->153`, the reversed tuple. Whether that
    counts as already-stored turns on `relates` being its own inverse (it is —
    `RelationKind.Inverse()` maps `relates` to `relates`, so a query on either
    endpoint surfaces the edge). Decide it explicitly and write the decision
    down; a wrong call here deletes an edge in silence.
  - `153->152`: nothing else stores it. `AIRA-152.md` holds only `152->151`.
    This one must MOVE onto `AIRA-152.md`, not be deleted.
- **`AIRA-152.md` is a MOVE.** It stores `152->151`, whose canonical owner is
  `AIRA-151`, and `AIRA-151.md` holds no copy in either direction. Insert it into
  `AIRA-151.md` in `relationLess` order and remove it from `AIRA-152.md`.

PR #105 applied exactly this treatment to the eight files in the AIRA-170
population, so `.aira/tickets/AIRA-151.md` and `AIRA-165.md` are worked examples
of both the deletion and the move.

Because the nine are excluded from the scan, `aira check` also reports cascading
`E_RELATION_TARGET_MISSING` findings for AIRA-149, AIRA-151, AIRA-153 and
AIRA-28 — those disappear when the files parse, so do not chase them separately.

## Why this is worth a ticket rather than a drive-by fix

Three of the four defect classes here are ones AIRA itself would never write; all
nine came from hand-editing frontmatter, which is what
`docs/superpowers/specs/2026-08-07-aira-design.md` intends `aira set`/`aira link`
to replace. So the repair is only half the work — the other half is asking
whether the writer paths cover what the hand-editing was reaching for (they do
not cover "add a relation to a ticket whose file is currently unreadable", which
is exactly the trap AIRA-170's last paragraph named).

## Acceptance

`internal/store/repository_tickets_test.go` already walks every ticket file and
asserts the failing set is EXACTLY `quarantinedTicketFiles`. Repairing a file
makes that test fail until its entry is deleted, so this ticket is done when the
map is empty and the literal can be removed along with the both-directions
assertion, leaving a plain "every ticket file parses" check.
