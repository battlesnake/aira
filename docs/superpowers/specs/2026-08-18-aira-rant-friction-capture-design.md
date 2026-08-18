# RANT — agent friction capture + git-context provenance (design / plan)

**Status:** v2 — folds round-1 multi-model review (Sol · Opus-5 · Terra, all convergent). For round-2.
**Milestone:** Phase 5 · rant (task #43). Follows D1–D6 (all merged; master `59596b5`).
**Loop:** multi-model design rounds → committed spec → Sol plan-review → Fable plan-gate → Terra
build → Opus real-HW verify → Sol build-review → merge.

---

## 1. Goal + intent
`aira rant "<text>"` — **zero-ceremony, agent-initiated, free-form complaint** logged into AIRA, so
agent friction (which evaporates at session end today) becomes a **durable, reviewable corpus** that
humans or an analysis agent periodically review to find process inefficiencies. The *qualitative*
complement to command telemetry; turns the one-off log-mining retro into a **continuous, evidence-
based** one. **Non-goals:** real-time alerting; AIRA auto-diagnosing (primitives, not judgement — the
synthesis is the reviewer's job); a finding-of-record (a rant is a subjective gripe until triaged).

## 2. The two existential risks the design must answer (round-1, Opus-5)
A friction corpus dies two ways, and the *entity in the middle* can't save it — both ends must be designed:
- **Adoption (input).** A purely passive tool → an empty corpus: agents have no persistent self and
  feel no accumulated friction. **Reconciled with the owner's "never prompted (context block only)":
  AIRA itself never nags.** Adoption comes from the *edges*, not AIRA interrupting: (a) an **opt-in,
  OFF-by-default friction-moment hook** — when enabled, AIRA may emit ONE contextual invitation at a
  *detected anomaly* it already has telemetry for (a gate bounce, a test suite over a wall-clock
  threshold, a retry loop); (b) the **dev-loop skills** invite a rant at natural junctures (post-gate-
  bounce, post-flaky-retry) — agent-side, not AIRA. **This is a genuine owner fork (§10-Q1); v1 ships
  the context-block advertisement + designs the hook but leaves it OFF/deferred pending the owner.**
- **Review ritual (output).** A corpus with no owner + no cadence is a graveyard. v1 names the ritual:
  a periodic **friction digest** pass (an owner + a cadence + an output = tickets/lints), served by
  the review surfaces below — the machinery alone can't create the ritual, so the spec states it.

## 3. Scope — v1 = the smallest genuinely-reviewable loop (Terra/Sol YAGNI)
**IN:** the `rant` entity (capture); `rant ls`/`count --by tag`/`get`; **append-only review
observations** + a **review-pass cursor**; **caller-observed git-context** provenance; FTS search;
`unreviewed` aging/counts; a "**recorded-tags**" aggregate.
**DEFER:** a trend gauge + any "themes/similarity" claim (free tags can't honestly cluster — Sol);
**cross-verb git-context rollout** (rant is the sole v1 consumer; run/finding fold in later) + the
M19 snapshot migration (fast-follow); the friction-moment hook *implementation*; promote-a-recurring-
rant-to-ticket/lint lifecycle (`derived-from` edges); cross-project aggregation; live command-
telemetry capture; ML clustering; the `dirty?` bit.

## 4. Data model (grounded in the real codebase — Terra)
Rants are a **lightweight entity beside findings** (different semantics: a finding is a claim-of-record
with verdicts; a rant is a logged subjective opinion), stored **DB-only**.

- **ID = a project-scoped `rant_counter`** (follow `test_report_counter`), NOT `Store.AllocateID` —
  the ticket allocator only knows configured ticket/requirement prefixes, writes git-file allocation
  receipts, and `RANT` would collide in machine-global `prefix_ownership`. `RANT-<n>`.
- **DB-only, with a journal EVENT HEADER only.** `journal.jsonl` carries `eventRecord` *metadata*, not
  payloads, so it can NOT recover the corpus — the earlier "journal it for durability" lean was wrong
  (Terra). Bodies/provenance/reviews live in DB tables; `rant.create` / `rant.reviewed` events are
  journaled (ordering + `watch` visibility) exactly like other significant mutations.
- Tables: `rants(project_id, rant_id, body, severity, idempotency_key, actor, session, observed_at,
  received_at, resolver_version, seq, ...)`, `rant_tags(project_id, rant_id, tag)` (join table),
  `rant_git_context(...)` (per-field value + status), `rant_context_refs(project_id, rant_id, kind,
  target)`, `rant_reviews(project_id, rant_id, pass_id, reviewer, at, note)` (**append-only**),
  `rant_passes(project_id, pass_id, reviewer, started_at, completed_at, through_seq, universe)`.
- **`reviewed` is DERIVED, never a mutable bool** (Sol): unreviewed ≡ no `rant_reviews` row (`reviewed_
  at IS NULL`). "reviewed" means strictly **examined**, not accepted/resolved. Append-only reviews keep
  *who examined what, when, in which pass, with what note* — a bool erases that + can falsely imply
  disposition.
- Fields:
  - `body` — free-form, **bounded by BYTES** (≤ 8 KiB), UTF-8/NUL-validated, empty rejected; over →
    `E_RANT_TOO_LARGE` (never silent trunc); whitespace preserved (explicit; byte-identity tested).
  - `tags[]` — optional; **normalized** (lowercase, hyphenate) so clustering doesn't fragment
    (`slow_tests`≠`slow-tests`); bounded count + length; free-form with a *suggested seed set* in help.
  - `severity` — OPTIONAL small enum (`papercut · annoyance · blocker`) — the ranter's subjective
    weight; telemetry gives frequency, only the ranter gives cost → enables impact-sort (Opus-5).
  - `context_refs[]` — bounded, **typed** (`run · ticket · finding · gate`), validated in-project
    (replaces the bespoke `about_run` — Sol/Opus-5).
  - **provenance** — `actor` (the runner's `owner`), `session`/`model` when available.
  - **idempotency_key** — client-supplied; a retried MCP/CLI call does NOT double-file (Sol).

## 5. Git-context = CALLER-OBSERVED provenance (owner-requested; Sol reframing)
A typed `GitContext` **stamped client-side at call time onto `core.Request`** (a first-class field,
NOT inside `Args` — Terra), stored **verbatim** and labelled **caller-observed, never daemon-verified**
(the client can be stale-before-commit or fabricate). Fields the owner asked for + honesty metadata:
`{ repo_root, worktree_path, worktree_id, head_hash, head_ref, remote_url }` + per-field
`status ∈ {value|none|unevaluated}(+reason)` + `observed_at` + daemon `received_at` + `resolver_version`.
(repo_root and worktree_path coincide for the main worktree but DIFFER for a linked worktree — the
owner's "if appropriate" case — so both are kept.)

- **Read git FILES directly (Terra's recipe), marking the hard cases `unevaluated` rather than
  faking them (the honesty stance that makes file-reading SAFE where a subprocess is a hang-risk /
  over-claim).** Consume the already-discovered `Root/CommonDir/GitDir/WorktreeID` (linked worktrees
  already modelled by `app.Project`). Resolve bounded regular files: `GitDir/HEAD` → validated
  symbolic-ref chain → loose ref in `GitDir` then `CommonDir` → exact match in `CommonDir/packed-refs`;
  distinguish **detached · unborn · absent · unreadable**; **retry until two consecutive snapshots
  agree** (ref-update race). Config parsing (for `remote_url`) handles quoting/continuations/case/
  duplicate URLs; **reftable, config `include`s, and unusual ref storage → explicit `unevaluated`**,
  never mis-parsed. (Sol's bounded-cancellable-`git`-subprocess is the documented alternative if
  reftable adoption grows; note: `app.Project` discovery already shells to git independently, so this
  is orthogonal to that pre-existing path.)
- **Secrecy (Sol — bigger than cred-strip):** `remote_url` → strip HTTP(S) userinfo + query + fragment
  and SCP-style creds (**reuse the existing unexported `gitremote.redactURL`** — Terra); host/repo
  normalization is a documented option. Abs worktree paths are the user's own machine (machine-local
  single-user) so stored by default, but path storage is **configurable to relativize/hash** for the
  future remote/multi-user phase. **Rant bodies are UNTRUSTED input** (a prompt-injection surface) —
  documented as such; a reviewing agent must treat them as data, never inject them raw as
  instructions; audited redaction/tombstoning keeps the ID + journal history.
- **Scope: artifact-CREATING verbs only** (rant, later run/finding-create) via a new `verbSpec`/
  `DispatchDescriptor` provenance flag — NOT review actions or generic telemetry (they describe
  *existing* records + should reference the original context). NOT hot read paths.

## 6. Routing + faces (Terra)
- **`rant` is a ROUTED verb** — a daemon-owned durable write; default `Classify` already routes it
  daemon-side. The client resolves `GitContext` + stamps it on `core.Request` before routing; the
  daemon validates the scope-stable fields against `WorktreeScope` and accepts the caller-only HEAD
  evidence verbatim.
- **CLI:** `aira rant "<text>" [--tag T]… [--severity S] [--ref run:RUN-n|ticket:… ]…`;
  `aira rant ls [--by tag] [--unreviewed] [--since <seq>] [--by actor]`; `aira rant get RANT-n`;
  `aira rant review RANT-n [--pass P] [--note "…"]`; `aira rant pass start|complete`.
- **MCP:** `aira_rant({ text, tags?, severity?, refs?, idempotency_key? })`. **The tool description
  is the only permitted "prompt"** — e.g. *"Ranting welcome: if something wastes your time — slow
  tests, linter noise, flaky infra, confusing setup — dump it here unfiltered; it's logged for later
  review, you won't be asked to format it."* Never nagged per-turn.
- **Capture returns same-TAG recent rants + their review outcomes** (honest: a tag match is a *fact*,
  not ML similarity — reconciles Opus-5's loop-closing with Sol's no-semantic-dedup) so the ranter
  sees "3 similar; one reviewed: won't-fix because X" → dedupes + suppresses re-filing + closes the
  loop back to agents (the only feedback path, else `outcome` notes rot).
- **Reviewable surfaces:** `rant ls`/`count --by tag`/**`--by actor`** (distinct-reporter is the
  primary systemic signal — "5 agents hit this" ≠ "1 vented 5×", Opus-5); FTS via `aira grep`; the
  generic `watch` already streams `rant.create` (no dedicated watch UX needed — Terra); the
  **pass cursor** makes "since I last looked" well-defined across reviewers + interrupted passes (Sol).
  Aggregates read "**recorded tags / N distinct actors**", never "themes" (§17 honesty).

## 7. Invariants
1. A rant is a subjective opinion, typed as such; every aggregate reads "N rants / N distinct actors
   about tag X", never a proven claim.
2. `reviewed` is DERIVED from append-only observations; "reviewed" = examined, never accepted/resolved.
3. git-context is caller-OBSERVED (per-field status, `observed_at`/`received_at`), never daemon-verified;
   unresolvable/reftable/includes → `unevaluated`, never faked; creds redacted; bodies untrusted.
4. Cheap to file (text-only), NEVER nagged (context block only), aggressive to aggregate; a sidecar
   stream that never pollutes primary work views; idempotent under retry.
5. Over-size body → `E_RANT_TOO_LARGE`; empty → rejected; tags/refs bounded + validated in-project.

## 8. Effort + build order (Terra: minimal slice ≈ 4–6 days; full ≈ 10–15)
resolver tests → domain/schema/`rant_counter`/tags-join → atomic capture+event → routed CLI/MCP →
list/review/count → FTS reinsert-on-rebuild → (fast-follow) M19 fold + cross-verb stamping + gauge.
Data-model traps: reinsert DB rants transactionally into every FTS delete/rebuild; `CountRants` (count
is entity-specific); CHECK-constrained review fields; define project-vs-worktree visibility; routed +
in-process byte-identity tests for the stored body + git-context.

## 9. Deferrals
Friction-moment hook impl (opt-in, off); promote-to-ticket/lint lifecycle; cross-verb git-context +
M19 fold; trend/similarity gauges; cross-project; live command-telemetry capture; `dirty?` bit;
payload-bearing journal (if corpus-recovery-across-DB-loss is ever wanted).

## 10. Open questions for round-2
- **Q1 (owner fork):** the adoption forcing-function — ship the opt-in friction-moment hook in v1
  (off by default) or defer it entirely and rely on skills + context-block? (Leaning: design it, defer
  impl, surface to owner.)
- **Q2:** severity enum (`papercut·annoyance·blocker`) vs a free rough-cost string vs both.
- **Q3:** is a review-PASS entity worth v1, or is `--unreviewed` + a per-reviewer "last seen seq" note
  enough? (Sol wants the pass; Terra's minimal slice omits it — resolve.)
- **Q4:** keep both repo_root + worktree_path, or is worktree_id + one path enough?
- **Q5:** capture-time same-tag-outcome lookup in v1, or defer (it's the loop-closer but adds a read
  on the hot capture path)?
