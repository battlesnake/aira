# AIRA-284: Claude session usage via a harness mod, linked to tickets

Status: PLAN v2 (2026-10-09; Sol review BLOCK applied: live-lease only, conflict on mismatched duplicate, two retention pools, install ownership contract, observed-subtotal wording; Gemini noted, read-time resolution rejected because the lease row is overwritten in place). Was PLAN v1. Ticket: AIRA-284. Requested by the owner after reviewing
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

- New column `turn_id TEXT NOT NULL DEFAULT ''` on `compute_events` (existing
  `ensureColumnAdded` pattern) and `CREATE UNIQUE INDEX IF NOT EXISTS ... (project_id,
  source, session, agent, turn_id) WHERE turn_id <> ''`.
- `--turn-id` requires a non-empty `--session` (refused otherwise); free text, bounded
  length, charset-checked like `--session`.
- A duplicate with IDENTICAL counters returns the existing id with `duplicate:true`: no
  counter number, no journal event, no retention pass. A duplicate key with DIFFERENT
  counters or model is refused with a stable code (`E_COMPUTE_TURN_CONFLICT`), never
  silently accepted; the first payload stands.
- The guarantee lasts while the row is retained: eviction deletes the key. v1 has no replay
  source (no offline buffer), so a replay can only be an immediate retry; documented, and a
  future buffer must bound its replay age below the retention horizon.
- Rows without `--turn-id` behave exactly as today. Legacy rows keep `turn_id=''`.
- Protocol: the daemon strictly decodes spend-add payloads, so the new fields bump the wire
  protocol version (aira has no compat obligation; client and daemon install together).

### 3.3 Ticket association at write time (opt-in `--resolve-ticket`)

The mod passes `--resolve-ticket`; existing producers (`run` auto-ingest, hand entry) are
unchanged. With the flag and no `--ticket`, the daemon resolves inside the insert
transaction, using the SAME liveness test the lease machinery uses (boot id + monotonic
clock sampled inside the transaction; an expired-but-unreleased `held` row does not count):

| Live `held` leases for this worktree | `ticket_id` | `ticket_status` |
|---|---|---|
| exactly one | that ticket | `lease-held` |
| none | empty | `none` |
| two or more | empty | `unevaluated` |

Worktree bindings are NOT used (they never expire or unregister, so they would attribute
spend to a ticket nobody is working). An explicit `--ticket` stays `declared`.

What this claims, and no more: "at ingest time this worktree held a live lease on ticket T".
It does NOT claim that this session did the work (two Claude sessions can share a worktree),
nor that the turn happened under that lease; a turn that ran just before a claim or just
after a release gets the neighbouring state. The read view and the skill say so. `ticket_status`
is a new nullable column; legacy rows read as `unknown` (not `none`).

The `ticket.update` event is NOT changed. The read view joins nothing on seq or time.

### 3.4 Retention

The count cap (20000, oldest by `at_seq`) and the age cutoff are global today; a per-turn
feed would evict `run`-sourced and hand-entered rows. v1: two bounded pools, `claude-mod`
and everything else, each with its own count cap and age cutoff, so mod volume can only
evict mod rows and arbitrary `--source` text cannot create unbounded pools. The mod's source
value is fixed (`claude-mod`); a caller passing it by hand lands in the mod pool.

### 3.5 Query

`spend ls --session <id>` filter, and `--by session`: sum each of the four buckets per
(session, ticket), NULL-aware (a bucket with no non-NULL contributor stays NULL, not 0; the
row carries a count of contributing turns). All totals are labelled OBSERVED subtotals:
dropped deliveries, missing counters and retention mean they are lower bounds, and "no
rows" cannot distinguish "never ran" from "mod not loaded". No completeness claim, and no
"last row time" doctor line in v1. Other filters (`--agent`, `--source`) are deferred.

### 3.6 Delivery and consent (opt-in)

`aira install --claude-usage-mod` installs the embedded mod to `~/.claude/skills/aira-usage/`
(manifest at `.claude-plugin/plugin.json`, per the mods reference). Opt-in only: a mod runs
with the user's privileges. Executable-file ownership contract:

- Files are written atomically (temp file in the same directory, then rename), refusing a
  symlinked or foreign (not aira-written) target; the directory carries an aira marker file
  naming the sha256 of each file.
- `off` and reinstall touch only files listed in the marker; unknown files are left and
  reported. An ordinary reinstall without the flag preserves an installed mod.
- The mod invokes aira by the absolute path of the installed binary, recorded at install
  time (argv, no shell, no PATH lookup), so a PATH-planted `aira` is not run per turn.
- `install --status` re-hashes against the marker and reports `ok | modified | absent`.
  A hash check detects drift after the fact; it is not a sandbox, and the forbidden-name
  grep (below) is a review aid, not proof.

## 4. Invariants

1. Counters and ids only. The mod source contains none of the forbidden `$` surfaces.
2. A missing counter is NULL / `unevaluated`, never 0. A turn that was not delivered leaves
   no row (a gap), never a fabricated one.
3. A duplicate delivery never changes a total.
4. Ticket association is stamped at write time from a LIVE lease only; ambiguity is `unevaluated`, never a pick; it is never described as session causality.
5. `claude-mod` volume cannot evict other rows, and the total row count stays bounded.
6. The mod never changes Claude's behaviour: it returns `next(e)` and swallows its errors.

## 5. Tests (TDD; each must fail against the wrong implementation)

- Store: identical duplicate returns the original id, `duplicate:true`, no counter number, no
  journal event; same key with different counters => `E_COMPUTE_TURN_CONFLICT`; different
  agent or session inserts; `--turn-id` without `--session` refused. Mutation: drop the
  unique index => RED; accept-first-silently => RED.
- Ticket association: one live lease, none, two live, EXPIRED-held lease (must be `none`),
  released lease, binding-only (must be `none`), explicit `--ticket`, and no flag (existing
  producers unchanged). Mutation: count expired leases / fall back to binding / pick first
  => RED.
- Retention: 20001 `claude-mod` rows leave 100 other rows intact; unlimited distinct
  `--source` values stay in one bounded pool; age cutoff per pool. Mutation: global cap => RED.
- Query: `--by session` sums per (session, ticket), NULL bucket stays NULL, turn count shown;
  `--session` filter.
- Wire: protocol version bump; old-shape payload refused with the stable code.
- Install: atomic write, symlink/foreign-file refusal, marker + sha256, `off` removes only
  marked files, reinstall preserves, tamper => `modified`, absolute binary path embedded,
  `--stage=start`/`--status` flag rejection like AIRA-283.
- Mod runtime (`claude plugin test`, reported `unevaluated` not green when `claude` is
  absent): payload contains only the allowed fields; non-zero exit and timeout are swallowed;
  `next(e)` is called exactly once on every path (success, error, timeout); a missing counter
  is omitted, not 0. Source scan for forbidden `$` names as a review aid.
- Manual acceptance (recorded in the PR): a real session in an aira worktree yields rows equal
  to the deduped transcript; a non-aira cwd yields no rows and no visible delay; measured
  end-of-turn latency added.

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
