# AIRA-284: Claude session usage via a harness mod, linked to tickets

Status: PLAN v1 (2026-10-09). Ticket: AIRA-284. Requested by the owner after reviewing
`abtop`. Evidence: a probe mod on Claude Code 2.1.294 (reported usage == transcript summed
once per `message.id`, exactly; `/clear` mints a new session id; subagent turns arrive as
separate records with `agentId`). Three read-only scouts (aira ingest path, ticket/session
linking, mod delivery and security) fed this plan.

## 1. Problem, in plain terms

The owner wants to see, per Claude session and per ticket, how many tokens (fresh input,
cache read, cache write, output) were spent while a ticket was being worked, kept alongside
the ticket's state changes. Aira cannot learn the session id or the token counts today.

## 2. Greenfield minimum

Solve only this: get exact per-turn counters and the session id from Claude into aira, and
tie each row to the ticket being worked.

- Source of numbers: a Claude Code "mod" (runs inside the harness; is handed normalised
  numbers). No transcript reading, so the design spec's "never scrapes" rule holds. A
  transcript tailer is rejected: it over-counts without `message.id` dedupe (measured 1.3x
  to 2.3x on this box) and reads prompt-bearing files.
- Sink: the EXISTING `compute_events` table through the existing `aira spend add` verb. It
  already has session, agent, model, the four disjoint buckets, ticket, worktree. No new
  table, no new verb.
- Two small additions to `spend add`, both needed for honesty:
  1. an idempotency key (`--turn-id`), so a retried or doubled event cannot double-count;
  2. write-time ticket resolution, so a row is tied to a ticket without the mod knowing aira.
- Nothing else. The ticket-linking view is a read-time query on (ticket, session).

## 3. Design

### 3.1 The mod (fixed, shipped by aira, reviewable)

One embedded mod, `aira-usage` (`plugin.json`, `hooks/hooks.json`, `hooks/register.ts`),
no imports, no `userConfig`. It hooks `turn.complete` only. Per event it builds a record of
ids and counters (never prompts, messages, tool arguments, file contents) and runs, with
`timeoutMs: 2000`:

    aira --scope-dir <cwd> spend add --provider anthropic --model <m> --source claude-mod \
         --session <sid> --agent <agentId or ""> --turn-id <turnId> --at <UTC RFC3339> \
         [--wall-ms <durationMs>]            # counters on stdin as the four Anthropic fields

Rules: always `return next(e)` unchanged; the whole body is inside try/catch; the mod never
touches `$.session.messages`, `$.fs`, `$.http`, `$.settings`, `$.session.append`, `$.prompt`,
`$.agent.spawn`. A usage field absent from the event is omitted (stored NULL, never 0).
`aira` exiting non-zero (not an aira project: `E_NOT_PROJECT`/`E_CONFIG_MISSING`; daemon
down) drops that row: v1 has NO offline buffer (see 6). A cwd that is not an aira project
is the common case and must stay silent and cheap.

### 3.2 Idempotency (`spend add --turn-id`)

- New column `turn_id TEXT NOT NULL DEFAULT ''` on `compute_events`, added by the existing
  `ensureColumnAdded` pattern. `CREATE UNIQUE INDEX IF NOT EXISTS ... (project_id, source,
  session, agent, turn_id) WHERE turn_id <> ''`.
- A duplicate insert returns the existing row's id with `duplicate:true`; it allocates no
  counter number, journals no event, touches no retention. Rows without `--turn-id`
  behave exactly as today.
- `--turn-id` is a free-text id (bounded length, charset-checked like `--session`).

### 3.3 Ticket resolution at write time

When `--ticket` is omitted, the daemon resolves it inside the insert transaction:

| State of this worktree | `ticket_id` | `ticket_status` |
|---|---|---|
| exactly one `held` lease for this worktree | that ticket | `declared` |
| no held lease, exactly one worktree binding | that ticket | `declared-binding` |
| none | empty | `none` |
| two or more candidates | empty | `unevaluated` |

An explicit `--ticket` stays `declared`. `ticket_status` follows the existing
`value|none|unevaluated` pattern (`worktree_id_status`). The lease row is overwritten in
place, so the write-time stamp is the only honest record of "who held it then". A lease
that expired between turns resolves to the binding or `none`, never to a guess.

Per-session ticket changes ("session S moved from AIRA-X to AIRA-Y") fall out of the rows:
group by (session, ticket). The `ticket.update` event itself is NOT changed (its actor stays
`aira`; no session column). The read view joins nothing on seq or time (`at_seq` comes from a
different counter than `events.seq`).

### 3.4 Retention

The compute table is count-capped (20000, oldest by `at_seq`), shared by all sources: a
per-turn feed would evict `run`-sourced and hand-entered rows that review-loop economics
depend on. v1: the cap is applied PER `source` (each source keeps its own newest N), so
`claude-mod` volume can only evict `claude-mod` rows. Age cutoff unchanged.

### 3.5 Query

`spend ls` gains `--session`, `--source`, `--agent` filters (today only ticket|phase|
provider), and `--by session` joins the `--by` list. Enough for "tokens per session per
ticket" without a new verb.

### 3.6 Delivery and consent (opt-in)

`aira install --claude-usage-mod` writes the embedded mod to
`~/.claude/skills/aira-usage/` (the folder pattern the aira skill already uses; auto-loaded
and watched). Not installed by default: a mod runs with the user's privileges, so it is an
opt-in trust grant. The install receipt records the mod's sha256; `aira doctor` (or
`install --status`) re-checks it and reports `modified` if it drifted. `aira install
--claude-usage-mod=off` removes it. `plugin validate` and `plugin test` run in CI over the
embedded source; a Go test pins the source's allowed-`$`-surface (grep-level: the forbidden
names above must not appear).

## 4. Invariants

1. Counters and ids only. The mod source contains none of the forbidden `$` surfaces.
2. A missing counter is NULL / `unevaluated`, never 0. A turn that was not delivered leaves
   no row (a gap), never a fabricated one.
3. A duplicate delivery never changes a total.
4. Ticket attribution is stamped at write time; ambiguity is `unevaluated`, never a pick.
5. `claude-mod` volume cannot evict other sources' rows.
6. The mod never changes Claude's behaviour: it returns `next(e)` and swallows its errors.

## 5. Tests (TDD; each must fail against the wrong implementation)

- Go store: duplicate (project, source, session, agent, turn_id) returns the original id,
  `duplicate:true`, no new counter number, no journal event; same turn id with a different
  agent or session inserts. Mutation: drop the unique index => RED.
- Go: ticket resolution table above, all four rows, plus expired lease, plus explicit
  `--ticket`. Mutation: pick the first of two candidates => RED.
- Go: retention per source: 20001 `claude-mod` rows leave 100 `run` rows intact.
  Mutation: global cap => RED.
- Go: `--turn-id` charset/length refusal; `spend ls --session/--source/--agent`; `--by
  session` totals.
- Go install: flag writes the files, records sha256; `off` removes; a tampered file is
  reported; `--stage=start`/`--status` rejection like AIRA-283.
- Go: mod source scan (forbidden `$` names absent; `return next(e)` present on every path).
- `claude plugin validate` / `claude plugin test` of the embedded mod in a make target that
  is skipped (reported `unevaluated`, not green) when `claude` is absent.
- Manual acceptance (recorded in the PR): a real session in an aira worktree produces rows
  that equal the deduped transcript; a non-aira cwd produces no rows and no visible delay.

## 6. Deferred (written down on purpose)

| Deferred | Why |
|---|---|
| Offline buffer in `$.store` | `$.store` is a non-atomic whole-value get/set; two sessions race. Idempotency now makes a later buffer safe to add. v1 accepts gaps while the daemon is down (`spend add` auto-starts it, so the gap should be rare). |
| `session.measure` (context, cost, rate limits) | `cost.usd` is probably cumulative (would double-count in `SUM(cost_usd)`); rate limits are account-wide percentages vs aira's integer quota counts. No consumer yet. |
| Session NAME | `$.session` has no title accessor; reading the transcript is forbidden. aira can derive the tmux window name from the process (AIRA-277) at read time later. |
| Stamping the session id on `ticket.update` / leases | Not needed for the stated view; adds a schema change to a correctness-critical table. |
| `session.end`, `sid_change`, compaction events | The probe logged zero `session.end` rows; a new session id is visible from the rows themselves. |
| Codex / OpenCode | Different counter semantics (cumulative; cached-inclusive). Their own source later. |
| `claude -p`, SDK, desktop remote | Mod loading and `$.process` availability unverified there. Verify before claiming coverage; v1 documents interactive CLI only. |

## 7. Risks

- Mod API is early access and moves between Claude Code releases; a skipped hook is silent.
  Mitigation: `aira` reports `unevaluated` (no rows) rather than zero; a doctor line shows
  the last row time per session.
- Hook await semantics for `turn.complete` are undocumented: a slow `aira` may delay the end
  of a turn. Mitigation: `timeoutMs: 2000`; measure end-of-turn latency with the real mod
  before release and record the number.
- Per-call cost: process spawn + git context + daemon round trip per turn. Acceptable per
  turn (seconds apart); measure.
- Per-source retention changes a shared table's behaviour: a regression test pins it.
- Trust: opt-in, fixed source, hash-checked, removable.
