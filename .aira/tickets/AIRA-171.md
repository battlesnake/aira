---
{"schema":1,"id":"AIRA-171","project":"aira","title":"Nine ticket files on master are unreadable: unsorted relations, non-canonical relation storage, unsorted labels, and bodies with no trailing newline","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","tickets"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-171","to":"AIRA-172"}]}
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
- **`AIRA-152.md` is a lossless deletion, plus the destination of the
  `153->152` move above.** It stores `152->151`, whose canonical owner is
  `AIRA-151` — and `AIRA-151.md` already holds `151->152`, the reversed tuple,
  the same already-stored shape as the `153->151` case. Delete `152->151` from
  `AIRA-152.md`; do NOT insert it into `AIRA-151.md`, which would store the one
  `relates` edge twice on one file, once in each direction (the parser would
  accept it — the two tuples are distinct). `AIRA-152.md`'s relations then become
  exactly the moved `153->152`. *(Corrected at the PR #105 close-out: the filed
  text said `AIRA-151.md` "holds no copy in either direction", which was false —
  `AIRA-151.md` at `bd9482d` holds `151->152`, `151->153`, `165->151`,
  `169->151`.)*

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

## Resolution

Done. The filed list was re-derived against master at `c30c074` before anything
was edited — PR #105 had landed in between and had already fixed this class of
defect on other files — and the nine files and their nine refusals came back
byte-identical to the table above. Nothing was stale; every prescription below
is the ticket's own, applied as written except for the one correction `c30c074`
had already made.

The prescription was applied verbatim. Each edit is one frontmatter line, except
the two trailing-newline files, which gain one byte:

| file | before | after |
| --- | --- | --- |
| `AIRA-28.md` | `[supersedes 29->28, relates 62->28]` | `[relates 62->28, supersedes 29->28]` — pure re-order, `relates < supersedes` |
| `AIRA-62.md` | `[relates 62->28]` | `[]` — the identical tuple is on `AIRA-28.md` |
| `AIRA-117.md` | `["aira-106","cgroup","test","race"]` | `["aira-106","cgroup","race","test"]` |
| `AIRA-141.md` | body ends `…as AIRA-148.` | trailing `\n` added; no other byte changed |
| `AIRA-144.md` | `["runner","cgroup"]` | `["cgroup","runner"]` |
| `AIRA-145.md` | body ends `…at the time.` | trailing `\n` added; no other byte changed |
| `AIRA-152.md` | `[relates 152->151]` | `[relates 153->152]` — one delete, one move in |
| `AIRA-153.md` | `[153->150, 153->151, 153->152, 153->162…168]` | `[153->162…168]` — two deletes, one move out |
| `AIRA-160.md` | `["admission","confine","aitest"]` | `["admission","aitest","confine"]` |

**The `153->151` mirror question, decided.** `relates` IS its own inverse:
`RelationKind.Inverse()` maps `relates` to `relates`, and
`derivedRelationViewsWithWarnings` projects a stored `A relates B` from B's end
as `B relates A`. `AIRA-151.md` holds `151->153`, so a query on AIRA-153 already
surfaces the edge and deleting the copy on `AIRA-153.md` loses nothing. The same
reasoning covers `152->151` against `AIRA-151.md`'s `151->152`, which is the
correction `c30c074` had already made to this ticket.

**Losslessness, measured rather than asserted.** The semantic edge set was
extracted from every ticket file before and after — `relates` as an UNORDERED
pair (it is its own inverse), the five directional kinds as ordered pairs. Both
sides are 81 edges and the two sets are identical: nothing lost, nothing
invented, only storage location and order changed. Label multisets are likewise
identical per file and the rest of each frontmatter is byte-equal.

`quarantinedTicketFiles` and the both-directions assertion are gone, replaced by
a plain "every ticket file is readable" walk. The map was deleted rather than
emptied: an empty map is a live exemption seam the next hand-edited defect could
be added to. Two guards were added in its place, because a plain parse walk
cannot tell a lossless delete from a lost edge — one asserting the tuple that
must still be STORED on each canonical owner, one asserting the edge is still
SURFACED from the endpoint whose file lost it. Both were mutation-checked:
dropping the moved `153->152` fails both halves, and reintroducing AIRA-160's
unsorted labels fails the walk.

The cascading `E_RELATION_TARGET_MISSING` findings the body predicted would
disappear are not separately verified here; they were a consequence of the nine
files being unscannable, and all nine now scan.

The second half the body asks for — whether the writer paths cover what the
hand-editing was reaching for — is NOT done and is not claimed. "Add a relation
to a ticket whose file is currently unreadable" still has no verb. That is left
open as the follow-up this ticket names.

## Review (Fable build-review gate) — MERGED

PR #106 merged as `41bdb73` (2026-09-08). Everything below is the reviewer's own
reproduction, not the builder's transcript.

- Real-file walk reproduced independently, the way the AIRA-170 review's probe did: a
  throwaway `domain.ParseTicket` walk over this repository's `.aira/tickets` (172
  files). At the PR base `c30c074` it refuses exactly the nine files in the table
  above with exactly the nine codes; at the PR head `ae92c64` it refuses none
  (172/172 parse), both from a `git archive` of the commit and from the live
  worktree.
- Losslessness re-measured rather than trusted: an independent script extracted every
  stored relation from all 172 files before and after (`relates` as an unordered pair,
  the directional kinds ordered) — 81 edges each side, `lost=[]`, `invented=[]`. The
  only stored tuples that changed location are the five the ticket prescribes:
  `152->151` gone (`AIRA-151.md` holds `151->152`), `153->150` and `62->28`
  deduplicated onto their canonical owners, `153->151` gone (`AIRA-151.md` holds
  `151->153`), `153->152` moved to `AIRA-152.md`. Label multisets identical per file,
  every other frontmatter field byte-equal, every body ends in `\n`, every label list
  sorted and unique, every stored relation on its canonical lower-ID owner.
- `quarantinedTicketFiles` and its both-directions assertion are gone from
  `repository_tickets_test.go`, not emptied (confirmed on the diff). Non-porosity
  re-run by the reviewer: re-introducing AIRA-160's unsorted labels in the worktree
  fails `TestTheRepositorysOwnTicketFilesAreReadable`.
- One pre-existing shape outside this ticket's nine, recorded as a follow-up rather than
  chased: `AIRA-30.md` stores both `AIRA-30 relates AIRA-36` and `AIRA-36 relates
  AIRA-30` — one `relates` edge stored twice on one file, once in each direction. The
  parser accepts it (the two tuples are distinct, exactly as the body above warned),
  so it is not a refusal, but it is the double-storage no writer path would produce.
- Merge gate, reviewer's own run: `AIRA_REAL_CGROUP=1 aira confine -- go test ./...
  -count=1` exit 0; gofmt clean on every changed Go file.
- The owner's separate root commit adding `relates AIRA-171->AIRA-172` ("AIRA-171:
  record its relation to AIRA-172") was rebased onto the merge commit ahead of this
  record; AIRA-171 is the lower id, so this file is that edge's canonical owner.

The second half the body asks for — a writer path for "add a relation to a ticket
whose file is currently unreadable" — remains open, as the Resolution says.
