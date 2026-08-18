# RANT — agent friction capture + git-context provenance (design / plan)

**Status:** v5 — APPROVED + operator forks resolved (2026-08-18): hook→skill-only; digest→owner-triggered/on-demand. Ready to build.
**Milestone:** Phase 5 · rant (task #43). Follows D1–D6 (all merged; master `59596b5`).
**Loop:** multi-model design → Fable final gate → committed spec → Sol plan-review → Fable plan-gate
→ Terra build → Opus real-HW verify → Sol build-review → merge.

---

## 1. Goal + intent
`aira rant "<text>"` — **zero-ceremony, agent-initiated, free-form complaint** logged into AIRA, so
agent friction (which evaporates at session end today) becomes a **durable, reviewable corpus** a
human or analysis agent periodically reviews to find process inefficiencies. The *qualitative*
complement to command telemetry; turns the one-off log-mining retro into a **continuous, evidence-
based** one. **Non-goals:** real-time alerting; AIRA auto-diagnosing (primitives, not judgement — the
synthesis is the reviewer's job); a finding-of-record (a rant is a subjective gripe until triaged).

## 2. The two graveyard-makers — both MUST be instituted, not just built (round-2)
A friction corpus dies at either end; the entity in the middle can't save it.
- **Adoption (input).** A passive tool → an empty corpus (agents have no persistent self). The owner's
  hard constraint is **AIRA itself NEVER nags — context-block advertisement only.** The one driver that
  honours it is the **dev-loop SKILL checkpoint: the agent's OWN workflow prompting, not AIRA.** So v1's
  **definition-of-done includes wiring ≥1 concrete checkpoint** into the dev-loop skill/guidance
  (`docs/dev/agentic-development-loop.md` + the aira Skill): *at a gate bounce / a flaky-retry, "if that
  just cost you time, `aira rant` it."* Ship only the tool description → empty corpus; ship the
  checkpoint → it fills. (An AIRA-side friction-moment hook that fires ONE invitation at a detected
  anomaly is **deferred** — off-by-default *unimplemented* machinery adds no adoption value, Sol; its
  boundary is specified in §9 for later. **Owner decision (2026-08-18): leave adoption to the skill
  checkpoint for now — the AIRA-side hook stays deferred.**)
- **Review ritual (output).** A corpus with no owner + no cadence is a graveyard. v1 ships a
  COMMITTED, build-review-checkable **`docs/dev/friction-digest.md`** describing the review flow.
  **Owner decision (2026-08-18): the ritual is OWNER-TRIGGERED / on-demand — no fixed cadence; the
  owner tells an agent "review this repo's rants and report back," which runs the review surfaces
  (`rant ls --by tag|actor`, `--unreviewed`, `grep`, typed outcomes) and records dispositions.** The
  doc names the owner as trigger + the concrete surface commands + the output (→ tickets/lints); the
  **minimal `resolved_by` link** (§4) makes "did my rant change anything" answerable.

## 3. Scope — v1 = the smallest genuinely-reviewable loop
**IN:** the `rant` entity (capture); `rant ls`/`count --by tag`/`--by actor`/`get`; **append-only
review observations** with a **typed outcome**; `--unreviewed` + an explicit **`--since <seq>`** cursor;
**caller-observed git-context**; FTS; the **same-tag prior-outcome** loop-closer; a "recorded-tags / N
distinct actors" aggregate; **`aira rant redact`** (tombstone a secret-bearing body, keep ID+provenance);
the §2 skill-checkpoint + first digest ritual (both as committed artifacts — §2/§8).
**DEFER:** a persisted **review-PASS entity** (v1 is single-reviewer — `--unreviewed`+`--since` suffice;
append-only rows make it migration-free to add for multi-reviewer/auditable-interrupted passes — the
one Sol↔Opus-5 divergence, resolved toward YAGNI + honesty); a trend/"themes"/similarity gauge (free
tags can't honestly cluster); cross-verb git-context rollout + the M19 snapshot fold (fast-follow);
the friction-moment hook impl; the full promote-to-ticket/`derived-from` lifecycle; cross-project;
live command-telemetry capture; the `dirty?` bit; a payload-bearing journal.

## 4. Data model (grounded in the real codebase — Terra)
Rants are a **lightweight entity beside findings**, stored **DB-only** (the `journal.jsonl` carries
`eventRecord` *metadata* not payloads, so it can't recover the corpus — a `rant.create`/`rant.reviewed`
event header IS journaled for ordering + `watch`).
- **ID = a project-scoped `rant_counter`** (follow `test_report_counter`), NOT `Store.AllocateID` (which
  only knows ticket/requirement prefixes, writes git-file receipts, and would collide in machine-global
  `prefix_ownership`). `RANT-<n>`.
- Tables: `rants` · `rant_tags` (join) · `rant_git_context` (per-field value+status) · `rant_context_refs`
  (typed) · `rant_reviews` (**append-only**).
- `rants`: `body` (free-form, **bounded by BYTES** ≤ 8 KiB, UTF-8/NUL-validated, empty rejected,
  over→`E_RANT_TOO_LARGE`, whitespace preserved + byte-identity tested); `severity` (OPTIONAL enum
  `papercut·annoyance·blocker` — reporter's subjective weight, enables impact-sort; enum-only, a free
  cost-string re-fragments what tag-normalization fixes); `idempotency_key`; `actor`/`session`/`model`;
  `observed_at`/`received_at`/`resolver_version`; `seq`.
  - **Idempotency = `UNIQUE(project_id, idempotency_key)`** (Sol r2): an identical normalized retry
    returns the ORIGINAL rant; the same key with a DIFFERENT body/tags/severity/refs → explicit
    `E_RANT_IDEMPOTENCY_CONFLICT`; key bounded; never semantic dedup.
- `rant_tags`: normalized (lowercase, hyphenate — `slow_tests`≡`slow-tests`), bounded count+length,
  free-form + a suggested seed set in help.
- `rant_context_refs`: bounded, **typed** (`run·ticket·finding·gate`), validated in-project (replaces
  the bespoke `about_run`).
- `rant_reviews` (**append-only**; `reviewed` is DERIVED — unreviewed ≡ no row): `reviewer`, `at`,
  `note`, and a **typed optional `outcome` ∈ {actioned·planned·duplicate·wont-fix·needs-evidence}**
  (explicitly NON-final — Sol r2: never infer disposition from prose) + an optional **`resolved_by`**
  typed ref (reuse the context-ref kinds) so an actioned rant points at the ticket/lint it produced.
  "reviewed" means strictly **examined**, never accepted/resolved.

## 5. Git-context = CALLER-OBSERVED provenance (owner-requested; Sol reframing)
A typed `GitContext` **stamped client-side at call time onto `core.Request`** (a first-class field, NOT
in `Args` — Terra), stored **verbatim + labelled caller-observed, never daemon-verified**:
`{ repo_root, worktree_path, worktree_id, head_hash, head_ref, remote_url }` + per-field
`status ∈ {value|none|unevaluated}(+reason)` + `observed_at` + daemon `received_at` + `resolver_version`.
Keep BOTH `repo_root` + `worktree_path` — they describe different facts for a *linked* worktree (the
owner's "if appropriate" case); `worktree_id` replaces neither.
- **Read git FILES directly (Terra's recipe), the hard cases `unevaluated` not faked** — the honesty
  stance that makes file-reading SAFE where a subprocess is a hang-risk/over-claim. Consume already-
  discovered `Root/CommonDir/GitDir/WorktreeID`; resolve bounded regular files `GitDir/HEAD` → validated
  symbolic-ref chain → loose ref (`GitDir` then `CommonDir`) → exact `CommonDir/packed-refs` match;
  distinguish **detached·unborn·absent·unreadable**; **retry until two consecutive snapshots agree**
  (ref-update race); config parse handles quoting/continuations/case/dupes; **reftable · config
  `include`s · unusual ref storage → explicit `unevaluated`**. (Sol's bounded-cancellable-`git`-subprocess
  is the documented alternative if reftable adoption grows; `app.Project` discovery already shells to
  git independently — orthogonal.)
- **Secrecy (Sol):** `remote_url` → strip HTTP(S) userinfo+query+fragment + SCP-style creds (**reuse the
  unexported `gitremote.redactURL`** — Terra); abs worktree paths are machine-local (stored by default,
  but path storage **configurable to relativize/hash** for the future remote phase). **Rant bodies +
  review notes are UNTRUSTED input** (a prompt-injection surface) — documented; a reviewing agent must
  treat them as data, never raw instructions; the loop-closer (§6) surfaces only
  `{rant_id, tag, typed outcome}` (never body/note prose — prose stays behind an explicit `rant get`);
  and a v1 **`aira rant redact RANT-n`** tombstones a body an agent pasted a secret into, **keeping the
  ID + provenance + journal history** (Fable 2 — agents WILL paste token-bearing error output).
- **Scope: artifact-CREATING verbs only** (rant now; run/finding-create later) via a new `verbSpec`/
  `DispatchDescriptor` provenance flag — NOT review actions / generic telemetry / hot read paths.

## 6. Routing + faces (Terra)
- **`rant` is a ROUTED verb** (daemon-owned durable write; default `Classify` routes it). The client
  resolves `GitContext` + stamps it on `core.Request`; the daemon **cross-checks the
  scope-stable fields** (`repo_root`, `worktree_id`, git-dir-derived) against its `WorktreeScope` and,
  on a mismatch, **stores the caller value with an explicit `mismatch` status label** (never silently
  rejects — the caller's honest observation is still recorded, flagged); the **caller-only evidence**
  (`head_hash`, `head_ref`, `remote_url`) is stored **verbatim, unvalidated** (the daemon cannot know the
  caller's commit).
- **CLI:** `aira rant "<text>" [--tag T]… [--severity S] [--ref run:RUN-n|ticket:…]… [--idem KEY]`;
  `aira rant ls [--by tag|actor] [--unreviewed] [--since <seq>]`; `aira rant get RANT-n`;
  `aira rant review RANT-n [--outcome O] [--note "…"] [--resolved-by ticket:X]`.
- **MCP:** `aira_rant({ text, tags?, severity?, refs?, idempotency_key? })`. Tool description = the only
  permitted prompt: *"Ranting welcome: slow tests, linter noise, flaky infra, confusing setup — dump it
  unfiltered; logged for later review, you won't be asked to format it."* Never nagged per-turn.
- **Loop-closer (Q5 = v1; Sol/Opus-5 converge):** capture returns, **post-commit**, a small deterministic
  set of **recent rants sharing a recorded tag**, each as **`{rant_id, tag, typed outcome}` ONLY — never
  the note/body prose** (Fable: prose can be fenced but not sanitized, so it stays behind an explicit
  `rant get`), labelled "sharing recorded tags", NEVER "similar" — so the ranter sees "RANT-4/RANT-9 share
  tag `slow-tests`; RANT-4 outcome=`wont-fix`". Dedupes *future* re-files + closes the only agent-facing
  feedback loop; honest because tag-match is a fact + the outcome is typed (not prose-inferred).
- **Reviewable surfaces:** `ls`/`count --by tag`/**`--by actor`** (distinct-reporter is the systemic
  signal — "5 agents hit this" ≠ "1 vented 5×"); FTS via `aira grep`; the generic `watch` already streams
  `rant.create` (no dedicated watch UX); `--unreviewed` + `--since <seq>` give honest "what's new"
  without a persisted pass. Aggregates read "**recorded tags / N distinct actors**", never "themes".

## 7. Invariants
1. Subjective-opinion type; aggregates read "N rants / N distinct actors about tag X", never a claim.
2. `reviewed` DERIVED from append-only observations; "reviewed" = examined; disposition only via the
   typed `outcome`, never inferred from prose; `outcome` is explicitly NON-final.
3. git-context is caller-OBSERVED: scope-stable fields (repo_root/worktree_id/git-dir) are daemon
   cross-checked → mismatch stored with a `mismatch` label (never silently rejected); the caller-only
   evidence (head_hash/head_ref/remote_url) is stored verbatim, unvalidated. Unresolvable/reftable/
   includes → `unevaluated`; creds redacted; bodies+notes untrusted (never raw-injected into a
   reviewer's prompt; the loop-closer returns no prose).
4. Idempotent under retry via `UNIQUE(project_id, idempotency_key)`; same-key-different-input →
   `E_RANT_IDEMPOTENCY_CONFLICT`; no semantic dedup.
5. Cheap to file, NEVER nagged (context block only; adoption via the dev-loop skill checkpoint);
   aggressive to aggregate; a sidecar stream that never pollutes primary work views.
6. Over-size body → `E_RANT_TOO_LARGE`; empty rejected; tags/refs bounded + validated in-project.

## 8. Effort + build order (Terra: minimal slice ≈ 4–6 days; full ≈ 10–15)
git-resolver tests → domain/schema/`rant_counter`/tags-join/reviews → atomic capture+event+idempotency
→ routed CLI/MCP → list/`--by actor`/review(+outcome) → FTS reinsert-on-rebuild + `CountRants` → the
same-tag loop-closer → the skill-checkpoint + digest-ritual doc. Traps: reinsert DB rants
transactionally into every FTS delete/rebuild; CHECK-constrained review/outcome fields; project-vs-
worktree visibility; routed + in-process byte-identity tests for the stored body + git-context.

## 9. Deferrals (with boundaries specified where they'll extend)
Friction-moment hook (opt-in, off; fires ≤1 invitation at a detected anomaly AIRA already has telemetry
for; owner fork); persisted review-PASS entity (multi-reviewer; add via append-only rows, migration-
free); promote-to-ticket/`derived-from` lifecycle (the `resolved_by` ref is the v1 seed); cross-verb
git-context + M19 fold; trend/similarity gauges; cross-project; live command-telemetry capture;
`dirty?` bit; payload-bearing journal (only if corpus-recovery-across-DB-loss is ever wanted).

## 10. Resolved open questions
Q1 adoption: **owner chose skill-only** — the friction-moment hook is deferred (§9); the dev-loop skill
checkpoint is the v1 DoD (§2). Q2 severity: **enum only**. Q3 pass entity: **defer** — `--unreviewed`+`--since` for v1
(single-reviewer), add the entity migration-free later. Q4: **keep both paths**. Q5 same-tag loop-closer:
**v1**, post-commit, typed outcomes, honest label.
