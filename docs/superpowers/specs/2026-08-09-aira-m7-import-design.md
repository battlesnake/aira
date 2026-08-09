# AIRA M7 — `aira import` (Phase 2)

- **Status:** design for build. **Branch:** `codex-aira-m7` off master `9054bf7` (M6 complete).
- **AIRA never calls a model.** `import` is a DETERMINISTIC ingester of a structured interchange
  format; converting arbitrary markdown (`docs/reviews/`, backlogs) INTO that format is the agent's job
  (its own tooling / an LLM like DeepSeek), never AIRA's.
- **Deferred:** ticket import (needs ID-allocation reconciliation — a distinct concern) and
  requirement import (Phase 3); a markdown/heuristic importer (agents convert markdown→JSONL instead).

## Scope
`aira import <file>` seeds the FINDINGS corpus from a structured **JSONL** file (one JSON review-finding
record per line), so AIRA can answer "which classes recur" over history on day one. Findings are
content-keyed (§M5) → import is idempotent by construction (re-import updates, never duplicates).

## Format + semantics
- JSONL: each line is a JSON object with the review-finding fields — `ticket` (well-formed ID;
  **existence NOT required** — a finding may reference a historical/retired ticket), `category`,
  `severity`, `verdict`, `source`, `message` (required); `file`,`line`,`requirement`,`disposition`,
  `waiver_reason`,`waiver_actor` (optional). `subtype` defaults to `review` (reconciliation findings are
  internal — reject `subtype:reconciliation` in import). Each record is validated via the M5
  `NewReviewFinding` constructor and created via the existing `AddFinding` write path (git file + index
  + journal event) — reuse it, don't reinvent.
- **Honest, never-silent error handling (the thing to get right):** parse line-by-line; a malformed line
  (bad JSON) or an invalid finding (constructor rejects) is RECORDED with its line number + reason and
  SKIPPED — never silently dropped. Default = best-effort: import every valid record, report the skips.
  Exit: `0` iff ALL records imported; `1` if any were skipped (partial — the summary lists every skip);
  `--strict` → abort the whole import on the FIRST bad record (import nothing, `E_IMPORT_INVALID`/exit 2).
- **Output:** a JSON summary `{imported, updated, skipped:[{line, error}], total}`. `--json` default for
  the machine surface; the skip list is always present when non-empty.
- Idempotency: re-importing the same file changes nothing (content-key); a record with edited
  message/severity/verdict updates the existing finding (counts as `updated`, not `imported`).

## Verb (core.Do + thin CLI)
`aira import <file> [--strict]` — read the file (a real path; missing file → `E_NOT_FOUND`/exit 2),
ingest per above, print the summary. Add stable codes `E_IMPORT_INVALID` (exit 2). Streaming line read
(do not load a huge file wholly into memory if avoidable).

## Invariants
Deterministic (no model call); idempotent (content-key); never silently drops a record (skips surfaced
in the summary AND the exit code); reuses the M5 finding write path (git-authoritative, journaled);
stable codes; no-cgo/static. Project/worktree-scoped like every other write.

## Tests (TDD)
- A JSONL of N valid findings → all imported (git files + index rows + `find ls` shows them); re-import
  → 0 imported / N updated-or-noop, no duplicates.
- A mix of valid + malformed (bad JSON line, invalid enum, empty ticket, subtype:reconciliation) →
  valid imported, each bad one in `skipped` with its line number, exit 1; `--strict` → nothing imported,
  exit 2, first error reported.
- A record editing an existing finding's message → `updated`, same key/file.
- Missing file → E_NOT_FOUND exit 2. Empty file → 0/0, exit 0.
- Regression: all Phase-1/M5/M6 tests + M3 concurrency -count=20 green; no-cgo static build.

## Yield
`aira import` seeds the findings store from a structured export in one deterministic, idempotent,
honestly-reported pass — the corpus starts populated so recurrence/verdict-ratio queries work on day one,
while keeping AIRA a recorder that never calls a model.
