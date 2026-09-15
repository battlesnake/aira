# `aira board` — read-only TUI kanban board (design spec)

- **Ticket:** AIRA-252
- **Date:** 2026-09-15
- **Status:** plan-review complete (Fable plan-gate + DeepSeek-pro orthogonal, both APPROVE-WITH-FIXES; all fixes folded in). Ready to build.
- **Author:** Opus (dogfooding aira)
- **Baseline:** v0.9 @ `origin/master` 207b032; verified against source at that commit.

## 1. Problem & goal

The owner needs better at-a-glance visibility over aira-tracked work: what is
done / in progress / backlog, **what sessions are doing what**, and the
**relations** between tickets (depends/blocks). Today that is only reachable by
reading `aira list`/`ready`/`lease ls`/`confine --list` by hand, or via the
`aira tui` tabbed dashboard — neither presents a kanban overview or a
cross-project picture.

**Goal:** a new `aira board` command — a **read-only** terminal UI presenting a
kanban board (columns = ticket status) with per-card status/relations/hold/ready
signals, a sessions/activity strip, ticket **search**, drill-in detail, an
**all-projects overview**, and a **per-project** board that opens by default when
run inside an aira project repo. Purely informational; it performs no mutations.

## 2. Non-goals & explicit deferrals

- **No mutations.** The board never claims, transitions, holds, links, or edits.
  (`aira tui` already covers actionable flows.)
- **No new core/store/domain verb.** The board is a thin face over existing read
  verbs; multi-project fan-out lives in the UI layer, preserving the invariant
  that `core.Do` serves exactly one project's store. (See §19; the zero-core-change
  claim was verified by the plan gate for both increments.)
- **No first-class "session" identity.** aira does not model sessions; the board
  shows the honest best-effort join of what agents *declare* (leases, worktree
  bindings, confine-job owners). Adding a real session identity is a separate,
  larger project, out of scope. (§9.)
- **No dependency-cycle detection** in v1. Core does no transitive/cycle analysis
  (`relation_ready.go` single-pass); the board mirrors that. The blocked-by count
  stays accurate per-edge in a cycle; a board-only cycle *hint* is a deferred
  enhancement.
- **No pagination of large columns.** A single read verb caps at 50 rows
  (`ListLimit`, `query.go:19`) with no cursor/offset. A large column shows a count
  header + the first 50 (the **lowest 50 ids** — `sortRecords`, `query.go:557-568`
  sorts by prefix/number/suffix), disclosed as e.g. `done · 50/193 (lowest ids)`.
  True pagination is a deferred surface gap.
- **All-projects ticket search** (grep fan-out across projects) is deferred;
  Increment-1 search is per focused project, overview search filters project cards
  (§10).
- **Out-of-repo behaviour is Increment 2.** In Increment 1, `aira board` outside an
  aira project returns the normal Discover error (as `aira tui` does today,
  `main.go:480-484`); the all-projects overview that replaces that error lands in
  Increment 2.

## 3. Success criteria

- `aira board` inside an aira project opens that project's kanban.
- `aira board` outside any aira project opens the all-projects overview
  (Increment 2; Increment 1 returns the Discover error).
- Seven status columns, each rendered even when empty.
- A card shows id, title, severity, kind, and honest badges for hold / ready /
  blocked-by (with explicit **unevaluated** states where truncation makes a
  definite answer impossible — never a fabricated negative).
- `Enter` shows full ticket detail incl. its relation neighbourhood.
- A sessions strip shows held leases + running jobs, correlated to tickets where
  the owner id resolves, honestly labelling the rest.
- `/` search matches ticket ids, title words, and body content; unevaluated/invalid
  search states are shown as such, never as "no results".
- Index staleness is shown as a board banner.
- No-TTY exits cleanly (honest error, no panic); `--json` refused.

## 4. Architecture overview

The board is a **third entry point** over the existing tview/tcell TUI runtime,
alongside `runTUI` (single-project, watch on) and `runTop` (projectless,
machine-wide, no watch). It reuses the Elm-style pure reducer + imperative shell
+ off-UI executor in `cmd/aira/tui*.go`.

- **Framework:** rivo/tview v0.42.0 + gdamore/tcell/v2 (already vendored). No new
  dependency.
- **Reads go through the daemon** via the `Dispatcher`, like `aira tui`. The board
  composes existing read verbs; it does not aggregate in core. (Nuance: most reads
  are `RouteDaemon` over the socket; `worktree-audit` and `confine-list` are
  `RouteClient` and run in the caller — `worktree-audit` opens the store read-only
  in-process, `routing.go:75`. So "purely daemon-backed" is true for the grid
  reads but not literally every read; this only matters for cost, §13.)
- **No-TTY safety (AIRA-134):** every runtime MUST route through `runTUIRuntime` /
  the `appRunDone`+`coordinateShutdown` coordinator (`tui.go:114,461-472`) and never
  call `app.Run()` directly, or a failed screen init panics.

## 5. Data sources (the read surface)

All existing verbs; field names/shapes verified against v0.9 source.

| Need | Verb (dispatched) | Scope | Notes |
|---|---|---|---|
| Column cards **and** column count | `list status:<s>` (one per column) | project | rows: id/status/kind/severity/assignee/milestone/hold/labels/title/relations[]; response carries `total` + `truncated`. Capped 50/call. The per-column `total` is the authoritative count — **no separate `count` call needed for the drilled board**. |
| Ready / blocked overlay | `ready` (no selector) | project | per-row `ready` bool + `blockers[]` + `verdict` + `findings[]`; **also capped at 50 and `truncated`** (`core.go:1571-1575`); omits blocked-but-clean and satisfied tickets; emits id-less finding rows (`{path, ready:false}`, `core.go:3025-3026`) that the join MUST skip. |
| Authoritative per-ticket readiness/blockers | `ready selector:<id>` | ticket | full scan for one ticket (drill-in). |
| Relations (badges) | (from `list` rows) | project | rows carry **stored** forward edges only (`core.go:3005`; `query.go:276-283` does not populate derived views). |
| Relations (drill-in) | `link list` `{list:true, selector:<id>}` | ticket | full bidirectional neighbourhood (inverses applied). |
| Ticket detail | `fetchTicketDetail` (`show`+`ready`+`link`+`find`, tui_data.go:125-140) | ticket | reuse verbatim. |
| Held leases | `lease ls` | project | `HeldLeaseRow{TicketID, Actor(free-text, default "aira"), WorktreeID, Generation, TTLNanos, LastHeartbeatMonoNanos, Expired, AgeNote}`. Render age as **`AgeNote` verbatim** — never arithmetic on the mono clock. |
| Running jobs | `confine-list` (empty scope, `owner=ConfineUnknownOwner`) | machine-wide | `ConfineRecord{Name, Owner, SupervisorPID, ScopeID, Command(*string — nil = unevaluated), RSSBytes, AgeSeconds, SupervisorLive(*bool), ...}`. **No ticket, no cwd.** |
| Search | `grep <query>` `kind:ticket` | project | FTS over ticket title+body; rows `{id, kind, rank, snippet}` (snippet contains literal `[term]` markers — must be escaped, §10/§14). Returns `{unevaluated:true}` on `E_INDEX_UNEVALUATED` and errors `E_QUERY_INVALID` on FTS-syntax queries (`core.go:995-997`, `search.go:199-215`). Also indexes findings/rants → pass `kind:ticket`. |
| Project enumeration | `store.ListRegistryEntries(paths.RegistryPath)` | machine-wide, local file read | `RegistryEntry{ProjectID, CommonDir, WorktreeID, Root, Prefixes, RequirementPrefixes}`. Pure local read of `registry.jsonl`; append-only (dead roots persist; dedup needed). |
| Per-project overview counts | `count --by status` | project | `{distribution:{status:n}, total}`. Overview only. |
| Worktree git state | `worktree-audit` | repo | **expensive** (RouteClient, ~6 git subprocs × N checkouts); drill-in only, never in refresh. |

**Warnings honesty — plumbing required (P1, verified):** `W_STALE_INDEX` is NOT on
rows — `TicketRecord.Warnings` is `json:"-"` (`query.go:30`); the warning lives on
the response envelope (`Response.Warnings`, `core.go:44,1602`), and the current TUI
decoder `decodeTUIResponse` **discards it** (`tui_data.go:21-44`). So staleness is a
**board-level banner**, not a per-card marker, and Increment 1 includes a small
face-level change to carry `Response.Warnings` through `decodeTUIResponse` to the
board state. This is still zero core change. Reads scan canonical git files, so
counts are correct despite a stale derived index; the banner means a reconcile is
pending. Per-row relation findings (e.g. `E_RELATION_INDEX_DIVERGENCE`) may also
appear and are surfaced as a small marker.

## 6. Column model

Columns are the seven `domain.Status` values in canonical order, from
`domain.AllowedStatusStrings()` (never the live distribution keys):

```
draft · planned · in-progress · in-review · done · retired · superseded
```

All seven render even when empty. Because seven columns overflow a normal
terminal, **navigation is horizontal**: the column strip is a horizontal
`tview.Flex`; only the columns that fit are shown, the focused column kept in
view. Header shows status name + count (from the column's `list` `total`).
**When `total > 50` the header carries a persistent truncation marker** (e.g.
`done · 50/193 (lowest ids)`) so the omission is never silent (read-only, so no
actions to disable; the incompleteness is always visible).

## 7. Cards & badges

Each card (a row in its column's `tview.Table`) shows id, title (escaped &
truncated), a colour-ranked severity tag (`P0` hottest), kind, and honest badges:

- **hold:** `⏸` iff the row's `hold` bool is set.
- **ready:** a **positive-or-unevaluated** badge, never a fabricated negative.
  - `● ready` iff the card appears in the `ready` set with `ready==true`.
  - `? ready` (**unevaluated**) iff the card is **workable** (`planned`/`in-progress`)
    AND absent from the `ready` set AND `ready.truncated==true` — because the ready
    list was cut and we genuinely cannot know (P1: absence past the 50-cap must not
    read as "not ready").
  - **no badge** iff the card is workable, absent, and `ready` was **not** truncated
    (a full ready scan omits only blocked-but-clean workable tickets = genuinely not
    ready; blockage is shown by the blocked-by badge), or the card is satisfied
    (`done`/`retired`/`superseded`, never a ready candidate).
  - The join **skips id-less finding rows** (`{path, ready:false}`).
  - Do NOT recompute readiness client-side. The drill-in shows the authoritative
    per-ticket verdict via `ready selector:<id>`.
- **blocked-by:** count stored `blocks` edges whose `To == card.id` and whose `From`
  prerequisite is not `satisfied()` (`{done,retired,superseded}`), from the union of
  **loaded** `list` rows' `relations[]`. Because a `blocks` edge is stored only on
  its canonical (lower-id) endpoint and a truncated column omits rows:
  - `⛔N` is **exact** iff every **non-terminal** column (draft/planned/in-progress/
    in-review) is untruncated — then any unloaded `From` is necessarily terminal
    (satisfied) and would not count anyway (matches `relation_ready.go:513-525`).
  - `⛔?` (**unevaluated**) iff any non-terminal column is truncated (a blocking
    `From` might be in its unloaded tail).
  - The drill-in shows authoritative blockers via `ready selector:<id>`.

## 8. Drill-in detail + relations

**Increment 3 (AIRA-254) supersedes the Increment-1 JSON overlay.** The owner's
verdict on the raw drill-in — "just gives me a JSON dump, nothing useful" — drove
two changes:

- **A persistent info pane** in the top ~quarter of the screen renders the SELECTED
  ticket's readable detail: an instant header (`id · status · severity · kind`) and
  the FULL raw title (never truncated — the column cell truncates, the pane does
  not), then the fetched fields (assignee / labels / milestone / relations /
  findings) and the body, each honest (`loading…` until landed, `unevaluated
  (CODE)` for a section that could not be read; a failed section never blanks or
  fabricates another). On a wide terminal (≥ 90 cols) the pane splits title+fields
  left | body right (body the larger share); narrow stacks them into one wrapping
  column. The pane's internal split is ALL PROPORTIONAL — no hand-computed row
  counts — so tview never gets a negative size, and the title (rendered first) is
  the last thing to clip; a silent clip is disclosed in the border as `· +N ↵`.
  The pane is hidden on a terminal too short to fit it without eating the columns.
- **`Enter` opens the same readable detail as a centered, size-clamped overlay**
  (the full-screen scrollable view, the escape hatch for a long title/body or a
  clipped pane). It is self-contained from the fetched model so it also opens an
  UNLOADED grep-only search hit. The overlay box is shrunk to fit small screens so
  its border/header/title are never placed off-screen.

The detail is fetched by `fetchBoardDetail` (`tui_board_detail.go`: `show` +
`link {list,selector}` + `find ls ticket:<id>`) into a structured
`boardDetailModel` of plain values, each with its own section code. Relations read
directly from the store's `RelationView` (From = the subject, To = the other end,
Kind pre-inverted for incoming edges). `Esc` closes the overlay (and re-arms the
pane for the current selection); the pane's raw full title needs no escaping
(dynamic colours off), while the width-bounded column cell still escapes+truncates.

## 9. Sessions / activity strip

**Honesty framing (load-bearing):** aira has no session entity. The strip is a
best-effort join of three declared signals, each graded by attestation; unknowns
are shown as unknown, never fabricated.

- **Held leases** (`lease ls`, per-project): "ticket X held by actor Y, <AgeNote>"
  + expired flag. Age is `AgeNote` verbatim.
- **Running jobs** (`confine-list`, machine-wide): owner, command, RSS, age, alive?.
  `Command`/`SupervisorLive` are pointers where nil means **unevaluated** — rendered
  as such, never 0/dead/blank.
- **Correlation:** a confine `Owner` equals a `worktree_id` only when launched in a
  repo with no `--owner`/`AIRA_CONFINE_OWNER` override (chain: explicit → env →
  WorktreeID → `@cwd-` inferred → unknown, `main.go:2061-2080`). When it is a
  64-hex attested id, resolve `worktree_id → root → project_id` via
  `ListRegistryEntries` (a pure local read) and thence to any binding/lease naming a
  ticket. When it is a label (`stoner-task5`), inferred (`@cwd-…`), or `unknown`,
  show owner+command and ticket "unknown". Never parse the command to guess a ticket.

**Cost control:** leases + confine-list refresh live; the git-heavy
`worktree-audit` is fetched **only** on explicit session drill-in, never in the
refresh loop. Empty states first-class ("no active claims").

## 10. Search

`/` opens a search input (modelled on the `:` palette). The query matches three
ways, merged and de-duplicated by ticket id:

1. **Ticket id** — if the query is id-shaped (project prefix ± number, e.g.
   `247`, `AIRA-247`), resolve directly against loaded rows / `get`. **Do NOT send
   an id-shaped query to `grep`** — the hyphen is an FTS-syntax hazard
   (`E_QUERY_INVALID`, `search.go:199-215`).
2. **Title / id-prefix** — client-side substring/prefix filter over loaded rows.
3. **Content** — dispatch `grep <query> kind:ticket`, sending the query **quoted as
   a phrase** to avoid FTS-syntax errors; results carry `{id, snippet}`.

**Honest result states:** a `grep` `{unevaluated:true}` (`E_INDEX_UNEVALUATED`) or
an `E_QUERY_INVALID` renders as **"search unevaluated: <code>"**, never "no
results". A genuine empty match renders an explicit "no matches" state. Snippets
contain literal `[term]` markers → `tview.Escape` before rendering.

**Presentation (implemented, Increment 1):** matching cards highlighted, non-matches
dimmed; a navigable results overlay lists `id — snippet` (`↑/↓` move, `Enter` opens,
`Esc` clears). `Enter` on a LOADED match jumps to its card; on an UNLOADED id (a
grep-only content hit in a truncated column, or a `show`-probe-resolved id) it opens
that ticket's detail drill-in. An id-shaped query with no loaded match falls back to
a `show` probe: a resolvable ticket becomes an openable result, a genuine
`E_NOT_FOUND` is an honest "not found" (never conflated with "no matches"). A grep
that hit the 50-cap discloses the count as a floor ("N+ matches (truncated)").

Increment-1 search is scoped to the focused project. In the Increment-2 overview,
`/` filters the **project cards** by slug/prefix over loaded overview data (no
dispatch); full ticket-content search there needs a focused project (grep is
per-project) — the overview shows a hint to drill in.

## 11. All-projects overview (Increment 2)

Assembled in the UI (the pattern `aira top` and the daemon's `discoverRegistryPass`
use), never in core:

1. `entries := store.ListRegistryEntries(paths.RegistryPath)` — pure local read.
2. **Dedup by `WorktreeID`** (breadcrumbs append per cold scope), then **group by
   `ProjectID`**.
3. Drop entries whose `Root` no longer exists (`os.Stat`).
4. **Canonical worktree per project:** pick the **main checkout** — the entry whose
   git common-dir belongs to its own root (a linked worktree's common-dir points
   elsewhere); else the first live root, **labelled**. The overview discloses which
   checkout it shows (per-worktree truth: the same ticket can differ across
   branches).
5. `app.Discover(root)` → slug + scope; **render every project with its state
   code** rather than silently skipping. **Correction (Increment-2 build):
   `app.Discover` never returns `E_NOT_ADOPTED` — an ejected project's
   `.aira/config` persists on disk, so Discover SUCCEEDS. The state is therefore
   TWO-STAGE: Discover → `available` (has a dispatchable scope) or
   `unavailable(<code>)` (E_CONFIG_MISSING / E_CONFIG_INVALID / E_NOT_PROJECT);
   then the lazy DISPATCH (step 6) → `ejected` when `count`/`lease` return
   `E_NOT_ADOPTED` (the daemon's `storeForScope` refuses an ejected scope,
   `server.go`), else `unevaluated(<code>)` on any other read failure. An
   unfocused ejected project reads "…" until its lazy read arrives.** This
   resolves the §14 contradiction — nothing is dropped.
6. For each **available** project dispatch `count --by status` + `lease ls` → a card:
   slug, prefixes, status distribution, activity count (open leases + owned jobs).
   One machine-wide `confine-list` renders the jobs strip. A card's lazy result is
   ALWAYS keyed by the card's registry `ProjectID` (resolved from the requested
   root), never by the fetch's fresh Discover id — so a card-time Discover failure
   or a fresh-id≠registry-id mismatch lands a definite `unevaluated(<code>)` rather
   than a card that loops "…" and re-dispatches on every focus.

**Bounded side-effect (disclosed):** dispatching a read to a project the daemon has
not cached since boot appends **one `registry.jsonl` breadcrumb per cold project**
(NewScope registers once on cache miss, `store.go:558`; the hit path re-registers
only on `ensure-scope`, `storeops.go:183`) — so a first-open fan-out across N cold
projects is up to N lines, not one. Bounded and harmless, but not strictly
zero-write. **Mitigation, adopted:** the project *list* comes from the pure-read
registry file; `count`/`lease ls` are dispatched **lazily** — only for the project
card focused/scrolled into view — not eagerly to all N on open.

## 12. Repo-aware default & mode switching

- **Default:** `scopeForCWD(cwd)`. Success → per-project board.
  **`E_NOT_PROJECT` OR `E_CONFIG_MISSING`** (a plain git repo with no `.aira/config`
  fails with `E_CONFIG_MISSING`, `project.go:546`, NOT `E_NOT_PROJECT`) →
  all-projects overview (Increment 2). `E_CONFIG_INVALID` stays a hard error.
  (Increment 1: any of these is just the returned Discover error.)
- **Switching (Increment 2) — independent runtimes, an outer loop, ZERO executor
  change.** Only the watch loop is scope-bound (captured by value,
  `tui_executor.go:127`); `Dispatch` takes the scope per call (`top` already passes a
  scope per call). So the overview is its own **watch-less** runtime, and the
  per-project board its own **watch-on** runtime. `runBoard` is an outer loop:
  ```
  scope,mode := initial(cwd)
  for {
    if mode==board   { next,act := runProjectBoard(scope); if act==quit {return}; mode=overview }
    if mode==overview{ chosen,act := runOverview();       if act==quit {return}; scope=chosen; mode=board }
  }
  ```
  Each `runX` builds a fresh runtime via `newTUIRuntimeForViews`, runs to a
  transition key (`o` = to overview, `Enter` on an overview row = into that project,
  `q` = quit), stops its app, and returns the next state. There is **no scope-swap
  inside a runtime and no watch-rebind** — the previously "delicate part" is
  designed out. (Minor transition repaint is acceptable for a read-only viewer.)

## 13. Refresh model & cost

- **Per-project board:** event-driven, reusing `runTUIWatchLoop` + the 250 ms
  debounced `cmdScheduleRefresh`. The board view MUST be registered in `dataViews`
  / participate in `invalidatedViews` (`tui_controller.go:592-608`) or watch events
  will not refresh it.
- **Overview:** a modest seconds-timer (like `viewTop`'s 1 s tick), lazy per-card
  dispatch. Per refresh the overview is ~`2N+1` reads (`count` + `lease ls` per
  focused project + one machine-wide `confine-list`) — kept lean by laziness.
  **Deviation (Increment-2 build): the seconds-tick refreshes the machine-wide
  jobs strip AND re-fetches ONLY the currently-focused card's `count`/`lease` on
  the same cadence, so the focused card's distribution is not arbitrarily stale;
  unfocused cards stay lazy (fetched once on first focus). A card's `count`
  envelope carrying `W_STALE_INDEX` is surfaced as a per-card `· stale` marker
  (§14), so a reconcile-pending distribution never reads authoritative.**
- Each `list`/`ready`/`count` re-scans ticket files on disk (not a cheap SQLite
  read); `worktree-audit` is never in a refresh loop.

## 14. Honesty rules (AIRA is primitives, not judgement)

- Staleness (`W_STALE_INDEX`) → a **board banner** (rows can't carry it, §5).
- Never merge `list` total with `ready` total (different sets); never let a
  truncated `ready`/`list` produce a fabricated "not ready"/`⛔0` (§7).
- Render nil `Command`/`SupervisorLive` as "unevaluated".
- Show every registry project with its state code (available / ejected / unavailable),
  never silently dropped.
- Grade session correlation (attested hash vs `@cwd-` vs label vs unknown).
- Search unevaluated/invalid ≠ "no results".
- **Escape all user-supplied text** (`tview.Escape` on titles, snippets, actors,
  commands) — tview `TableCell` interprets `[…]` colour tags and grep snippets
  carry literal `[term]` markers (unverified that TableCell parses tags in all
  paths → escape unconditionally).
- **Daemon-unreachable mid-session** → one board-level banner + last-good columns
  marked stale, not seven per-column errors.

## 15. Keybindings (read-only)

`q` quit — **in EVERY mode `q` quits the whole viewer** (Increment-2 build
correction: the earlier "in board mode reached from overview: back to overview"
was wrong and contradicted §12; returning to the overview is `o`, and `q` ends
the outer loop). `←/→` or `h/l` move column · `↑/↓` move card · `Enter` expand the
selected ticket's detail overlay / (overview) open project · `f` (Increment 3)
toggle the focused column full-width (hides the other columns for readable titles)
· `Esc` back/close · `/` search · `r` refresh · `o` (Increment 2) board → overview.
The selected ticket's readable detail always shows in the top info pane (§8). No
mutation keys.

## 16. Increments

**Increment 1 — single-project board (ONE runtime; zero core/store/domain change):**
7-column kanban, cards + honest badges (incl. the truncation-unevaluated states),
horizontal navigation with a width seam (`screen.Size()` at render), drill-in
(reusing `fetchTicketDetail`), sessions strip (leases + confine live; audit lazy),
search (focused project, id/title/content with honest unevaluated states),
staleness banner (the small `decodeTUIResponse` Warnings plumbing), repo-aware
default (in-repo → board; out-of-repo → the Discover error). Fable build-review, PR,
merge.

**Increment 2 — all-projects overview + mode switching:** the overview runtime
(registry walk, per-project state codes, canonical-worktree rule, lazy per-card
`count`/`lease ls`, machine-wide jobs strip, project-card search) and the outer
`runBoard` switching loop (§12). **No executor change, no core change.** Fable
build-review, PR, merge.

Each increment is its own two-loop (Opus builds, Fable reviews) and its own PR.

## 17. Testing strategy (mirrors existing TUI tiers)

1. **Pure reducer unit tests** (`tui_board_controller_test.go`): status→column
   bucketing (incl. empty draft/in-review); horizontal nav (scroll/focus at narrow
   width); search filter/merge/dedup + id-shape routing.
2. **View-model tests** (`tui_board_viewmodel_test.go`): card fields, badges,
   sessions rows (attested vs unknown correlation), empty states. **False-pass
   tests (Fable-mandated):**
   - truncated `ready` missing card X → `? ready` unevaluated (not unbadged).
   - a blocking `From` in a truncated non-terminal column → `⛔?` (not `⛔0`).
   - `Response.Warnings=["W_STALE_INDEX"]` → banner shown.
   - `grep {unevaluated:true}` / `E_QUERY_INVALID` → "search unevaluated", not "no
     results".
   - a `[x]`-bearing title/snippet renders literally (escape verified).
   - id-less finding row skipped in the ready join.
   - nil `Command` → "unevaluated".
3. **Simulation-screen smoke** (`tui_board_smoke_test.go`): `tcell.NewSimulationScreen`
   + stub `Dispatcher`; assert columns/badges/drill-in/search; a **40-column** smoke
   for the horizontal-scroll seam.
4. **No-TTY test** (mirror `tui_screen_init_test.go`): screen-init failure is an
   honest `E_INTERNAL`, never a panic.
5. Increment 2 adds: registry dedup(by worktree_id)/group(by project_id)/dead-root-skip
   + per-project state-code tests; canonical-worktree-rule tests; an **outer-loop
   transition** test (overview→project→overview yields the right scopes/modes) —
   replacing the (now removed) scope-swap test.

Every confirmed review counterexample becomes a regression test (repo policy).

## 18. Risks & mitigations

- **7-column width** → horizontal scroll + focused-column-in-view + a render-time
  `screen.Size()` seam (pure fn of width+focus); 40-col smoke.
- **Column/ready truncation at 50** → authoritative `total` in header; honest
  `? ready` / `⛔?` unevaluated states; disclose "lowest 50 ids".
- **Staleness plumbing** → `decodeTUIResponse` carries `Response.Warnings`; banner;
  tested.
- **Read-not-pure breadcrumb (Inc 2)** → disclosed; up to N; lazy-dispatch
  mitigation.
- **Sessions sparse/opaque** → honest empty + attestation grading (owner accepted).
- **`worktree-audit` cost** → never in refresh; lazy drill-in only.
- **tview tag injection** → escape all user text unconditionally.
- **Daemon-unreachable** → single banner + last-good marked stale.
- **AIRA-134 no-TTY panic** → route through `runTUIRuntime`; no-TTY test.

## 19. Considered & rejected alternatives

- **A cross-project "board snapshot" core verb.** Breaks the one-project invariant.
  Rejected: face-level registry walk is precedented (`aira top`,
  `discoverRegistryPass`, `eject`, `rant --project`).
- **An 8th tab on `aira tui`.** The `1`..`7` number-key selection is hardcoded
  (`tui_controller.go:480`) and the one-table-per-view assumption pervades render /
  focus-restore. A kanban is a new layout shape. Cleaner as its own face.
- **Scope-swap + watch-rebind within one runtime.** Rejected in favour of
  independent per-mode runtimes in an outer loop (§12) — zero executor change,
  removes the one delicate concurrency seam.
- **Reading `store.List()` in-process to dodge the 50-cap.** The grid reads are
  daemon-backed; per-status `list` (with its `total`) is correct.

## 20. File plan

**Add (`cmd/aira/`):**
- `tui_board.go` — `runBoard` (Increment 1: one per-project runtime; Increment 2:
  the outer switching loop + overview runtime) + `newBoardRuntime` over
  `newTUIRuntimeForViews`.
- `tui_board_controller.go` — board reducer, `boardState` (columns, selection,
  search, banner), horizontal nav, search merge, badge derivation (incl.
  unevaluated states).
- `tui_board_viewmodel.go` — envelope→columns/cards/sessions builders.
- Tests: `tui_board_controller_test.go`, `tui_board_viewmodel_test.go`,
  `tui_board_smoke_test.go`, board no-TTY assertion.

**Modify (`cmd/aira/`):**
- `main.go` — register `board` verb; refuse `--json`; option table (~830);
  `requestForVerb` (~2610); scope handling (Increment 1 in-repo, Increment 2 hybrid).
- `scope_dir.go` — add `board` to the projectless-capable verb list (Increment 2).
- `tui_data.go` — board fetch cases (per-status `list`, `ready`, `lease`,
  `confine-list`, `grep kind:ticket`); **carry `Response.Warnings` through
  `decodeTUIResponse`**.
- `tui_controller.go` / `tui.go` — board view wiring; board view registered in
  `dataViews`.

**No changes** to `internal/core`, `internal/store`, `internal/domain`,
`internal/daemon` in either increment (plan-gate-verified). Increment 2 adds only
face-level registry-walk + scope construction.
