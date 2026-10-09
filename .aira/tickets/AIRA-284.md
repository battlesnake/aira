---
{"schema":1,"id":"AIRA-284","project":"aira","title":"Agent session usage log: capture session name/id + token/cache usage alongside ticket state changes (abtop-inspired)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---


## Request (owner, 2026-10-09)

Look at `abtop` (graykode/abtop, Rust, read-only source reviewed from a shallow clone; not run) and add aira support so that, when possible, aira acquires and logs per-agent-session identity and usage (session name + id, tokens in/out, cache read/write, ...) alongside ticket state changes.

## How abtop gets the numbers (findings, 2026-10-09)

abtop is a pure 2 s poller. It pushes nothing and needs no hook for per-session data.

- **pid -> session id:** Claude Code itself writes `<config>/sessions/<PID>.json` = `{pid, sessionId, cwd, startedAt, kind, entrypoint}` (verified present on this box under `~/.claude/sessions/`). abtop finds `claude` pids from the process table, reads `/proc/<pid>/environ` for `CLAUDE_CONFIG_DIR` (multi-profile), and `/proc/<pid>/cwd` + `/proc/<pid>/fd`.
- **Usage:** it tail-reads the transcript `<config>/projects/<cwd with / \ : _ . -> '-'>/<sessionId>.jsonl`. Every line with top-level `type == "assistant"` carries `message.model` and `message.usage.{input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens}`; abtop sums them. Also `version`, `gitBranch`, `timestamp` on lines. Incremental: per-session byte offset + (inode, mtime) identity; partial last line retried; lines >10 MB skipped.
- **Context window:** heuristic only (200K, or 1M if the model string has `[1m]` or observed context >200K); context = input + cache_read (or input + cache_creation on a fresh session). Compaction is inferred (context drop >30% and cache_read drop >80%), not read from a marker.
- **Subagents:** `<session>/subagents/agent-<hash>.{meta.json,jsonl}`; whether the parent transcript also contains sidechain usage is UNVERIFIED.
- **Rate limits:** the only hook-based item. `abtop --setup` registers a Claude `statusLine` command that writes account-level 5 h / 7 d usage; not per-session. Claude has ONE statusLine slot.
- **Codex:** `rollout-*.jsonl` found via the pid's open fds; `token_count.total_token_usage` is a cumulative counter (overwritten, not summed); Codex `input_tokens` INCLUDES cached tokens, Claude's does not. OpenCode: SQLite sums.
- **Not available from those sources:** cost, a native session name (abtop shows the first prompt or its own `claude --print` summary), cache TTL breakdown, stop reason, API errors, exact context window, per-session rate limits.
- **Hazards in abtop itself (do not copy):** NO dedupe by `message.id`/`requestId` (a message written on several lines would be over-counted; unverified against real transcripts); /clear mints a new sessionId + jsonl without rewriting the pid file (abtop guesses by newest mtime, ambiguous when two pids share a cwd); counters default to 0 when absent (aira must report `unevaluated`); token/usage history is not persisted; its JSON snapshot includes redacted prompt text (aira should log counters and ids only).

## What aira has today

- Ticket state changes: `Store.MoveTicket` -> `events` row (`ticket.update`, actor literal `aira`, no from/to status, no session) + `outbox`, in one transaction (internal/store/store.go ~2240-2294, 3406). Durable.
- `compute_events` (`aira spend add`): ticket, phase, model, provider, `session`, `agent`, source, and disjoint buckets fresh_input / cache_read / cache_write / output / reasoning; DB-only and retention-capped (internal/store/compute.go, store.go ~1009). So the TABLE for tokens/cache largely exists; what is missing is a way to LEARN the Claude session id and to link a sample to a ticket transition.
- No Claude session id or transcript path is captured anywhere today (grep CLAUDE_SESSION / clientInfo / transcript: nothing). Confine attribution uses `AIRA_CONFINE_OWNER` / worktree id / `@cwd-*`, and the tmux window name (AIRA-277) from `/proc/<pid>/environ`.

## Design fork the plan must settle first

1. **Spec conflict.** The design spec (compute section, ~line 134) says capture is out-of-core, via Claude hooks (Stop/SubagentStop usage) or OTEL, "never the transcript", and "AIRA records what an ingester hands it, never scrapes". An abtop-style transcript tailer contradicts it. Options: (a) hook/OTEL-fed ingest only, `aira spend add --session ... --source claude-hook` (cheapest, no spec change; needs a Claude hook config the user installs; hook payload fields unverified here); (b) an optional out-of-core collector process (not the core) that tails transcripts and calls the ingest verb, which keeps the core rule intact; (c) amend the spec. Challenge pass: greenfield minimum may be (a) plus resolving session id from `~/.claude/sessions/<pid>.json` for the calling process's ancestors.
2. **Linking to ticket transitions.** The `ticket.update` event has no session/model; compute rows can be evicted while the transition survives, so a join by seq/time is approximate. Decide: stamp `session`/agent id on the event (small), or log a periodic usage sample row keyed by session and join at read time.
3. **Honesty.** Absent field / unreadable file / no transcript yet / pid dead are all `unevaluated`, never 0. Dedupe by `message.id` (verify against real transcripts before relying on it). Log counters and ids only: no prompts, chat or tool arguments (secrets risk; best-effort redaction is not airtight).
4. **Identity details.** Handle /clear (sid change on the same pid is an event), PID reuse (record pid start time), shared cwd ambiguity, multi-profile `CLAUDE_CONFIG_DIR`, Codex/OpenCode as later kinds with their own counter semantics (cumulative vs summed; cached-inclusive vs exclusive).
5. **Cost.** Incremental offset + inode reads, bounded line length; a first read of a multi-hundred-MB transcript must not stall the daemon (use a separate collector, not the admission hot path).

Minimal field set per sample: ts, agent kind, pid + start time, session_id (+ source: pidfile | transcript-scan | rollout-meta), cwd, git branch, model, cli version, input / output / cache_read / cache_write tokens (nullable), context tokens (nullable), turn count, per-field status (ok | absent | unevaluated). Per event: session_start / end, sid_change, model_change, compaction, counter_reset, plus the aira ticket id and from->to transition that coincided.

Held (backlog capture; owner asked for a ticket, not a build). Needs a plan + challenge pass and the spec fork resolved before any build. Source of this analysis: ~/tmp/abtop-src/repo (graykode/abtop @ 4b96568); do not run it.

## Update 2026-10-09: the mod route is the preferred design (verified by a probe)

Claude Code (2.1.294) has a second plugin system, "mods" / function hooks (skill `plugin-authoring`; `claude plugin validate|test`; hot-reloaded from a mods folder or `--plugin-dir`; API marked early access). A mod runs INSIDE the harness and gets pushed, normalised data, so no transcript scraping:

- `$.session.id()` = the transcript file name = the session id (matches `~/.claude/sessions/<pid>.json`); `$.session.model()/cwd()/root()/repo()`.
- `turn.complete` carries `usage` (`input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `model`), `reason`, `durationMs`, `turnId`, `agentId` (subagents). Per-call usage on `turn.step` results.
- `session.measure` fires after each turn and when a rate-limit window moves: context tokens/window/percent, `rateLimits` (five_hour, seven_day, with reset times; ACCOUNT-wide), `cost.usd`.
- `session.start` / `session.end` (reason incl. clear/resume). `classic.Stop` etc. carry `transcript_path`.
- `$.process.run(argv)` (no shell) / `$.process.spawn` / `$.http.fetch` can call `aira`. No session NAME accessor was found.

Probe (`~/.claude/dev-mods/<session>/aira-usage-probe`, logs ids + counters to ~/tmp/aira-usage-probe.jsonl; tiny, no prompt text) confirmed it works. EVIDENCE vs abtop: for one real turn the mod reported (in 6, out 932, cache_read 1,089,759, cache_write 13,129) which EXACTLY equals the transcript summed once per `message.id` (last usage per id). abtop's per-line sum (no dedupe) gave (10, 1782, 1,824,029, 14,049): 1.9x too many output tokens, 1.7x too many cache reads; over six recent prompts the naive sum overshot 1.3x-2.3x. Transcript scraping the abtop way over-counts on this box; the mod does not.

Resolves the spec fork: a mod is out-of-core and hands aira already-normalised numbers, so "never the transcript / never scrapes" is honoured with no spec amendment. Remaining design: mod -> `aira spend add` (or a small ingest verb) in an aira project; link to ticket transitions by stamping the session id when a ticket is claimed/moved (aira side) rather than from the mod; rate limits are account-wide so log once, not per session; the mod needs the user to install it (`/plugin install ... --marketplace`) and, under `claude -p`, mods may not load; Codex/OpenCode still need their own sources. Unverified: `/clear` and subagent behaviour, `session.end` completing within its short bound, whether `-p` runs load the mod. Still held; needs a plan + challenge pass.
