# RANT — agent friction capture + git-context provenance (design / plan)

**Status:** v1 — DRAFT for multi-model design review (Sol · Terra · Opus-5 · Fable final gate).
**Milestone:** Phase 5 · rant (task #43). Follows D1–D6 (all merged; master `59596b5`).
**Loop:** multi-model design rounds → committed spec → Sol plan-review (rounds → APPROVE-PLAN)
→ Fable plan-gate → Terra build → Opus real-HW verify → Sol build-review → merge.

---

## 1. Goal + intent
`aira rant "<text>"` — a **zero-ceremony, agent-initiated, free-form complaint** logged into AIRA.
Purpose (owner): a **durable, reviewable corpus** of agent friction that humans or a dedicated
analysis agent periodically review to find **process inefficiencies and areas for improvement**.
It is the *qualitative* complement to command telemetry: an agent notices "these 200 integration
tests fire on every trivial change and could be skipped," and today that insight **evaporates at
session end** (like review findings did before AIRA made them survive by construction, §10). A
rant makes it a standing, git-situated datum — turning the one-off retrospective (what a human/
agent does by hand mining logs) into a **continuous, evidence-based retro**.

**Non-goals:** real-time alerting; AIRA auto-diagnosing the slowdown ("primitives, not judgement"
— the synthesis stays the reviewer's job); a formal finding-of-record (a rant is a subjective
gripe until a reviewer triages it into one).

## 2. Scope — v1 = capture + review, NOT the promotion lifecycle
**IN:** the `rant` entity; `aira rant` capture; a shared client-side **git-context provenance**
primitive (§4); reviewable surfaces (`rant ls --by tag`, `grep` FTS, `watch`, one insight gauge);
a **minimal reviewed-marker** so review passes are cumulative.
**OUT (deferred, §8):** promote-a-recurring-rant-to-ticket/lint with `derived-from` edges; the full
disposition workflow; cross-project aggregation; live command-telemetry *capture* (a separate
milestone — the metrics substrate already exists, §17; only coverage is missing); ML clustering.

## 3. The rant entity (data model)
A **lightweight, event-like** record that sits **beside** findings — NOT inside them. A finding is
a claim-of-record with verdicts + disposition + a `resolves` edge; a rant is a logged *subjective
opinion*. Different semantics ⇒ different entity, so rants never pollute finding aggregates.

- `id` — `RANT-<n>`, allocated via the existing `aira id` allocator (never hand-picked).
- `body` — free-form text, bounded (e.g. ≤ 8 KiB; over → `E_RANT_TOO_LARGE`, never silent trunc).
- `tags[]` — optional; free-form strings, with a *suggested* light vocabulary surfaced in help
  (`slow-tests`, `linter-noise`, `flaky`, `toil`, `tooling`, `docs`, `env`). Not an enum — an agent
  can coin a tag; the suggested set just seeds consistent clustering.
- `about_run` — optional `RUN-*` reference (fuses the qualitative rant to the quantitative
  run/command telemetry).
- **git-context** — the §4 provenance struct, stamped at capture time.
- **provenance** — actor (agent identity, the runner's `owner`), model + session if available.
- `seq` + `at` — auto (the §11 event stamp).
- **review state (minimal):** `reviewed bool`, `reviewed_at`, `reviewed_by`, optional `outcome`
  note. Enough that a review pass is cumulative and the "we heard this, here's what we did / why we
  won't" decision is durable (§10 `waived`-style visible-debt discipline, kept to its lightest form).

**Storage + journaling.** Rants live in `state.db` (a `rants` table) AND are **journaled** (§11):
they are low-frequency (agent-initiated, not per-run) so they won't swamp the journal the way
per-run/`spend` telemetry would, and they are exactly the kind of *significant, provenance-bearing
mutation* the journal is for — durable across DB loss, git-context preserved. (Per-run command
telemetry stays DB-only; a rant is not per-run.)

**Honesty.** `kind = rant`, explicitly subjective. Every aggregate reads "**N rants about X**",
never "X is true/bad" — the same stance §10 takes for reviewer verdict ratios (honest aggregates of
recorded labels, not proven claims).

## 4. Git-context provenance (the shared primitive — owner-requested, cross-cutting)
A reusable `GitContext { repo_root, worktree_path, worktree_id, head_hash, head_ref, remote_url }`
resolved **client-side at call time**, stamped onto the verbs that opt in. Rant is its **first
consumer and forcing function**; `run`/`finding`/telemetry/`review` reuse it, and **M19's ad-hoc
pre-launch VCS snapshot (commit/branch/worktree) folds into this one honest resolver** — one
primitive, not two.

- **Read git FILES directly — do NOT shell to `git`.** `head_hash`/`head_ref` come from
  `<gitdir>/HEAD` (+ the ref file, with a `packed-refs` fallback); `remote_url` from `<gitdir>/config`
  `[remote "origin"] url`. This is cheap, subprocess-free, and **avoids the `git rev-parse` hang**
  M20b already got bitten by (`gitValue` + `WaitDelay`), and it matches the store's "git files"
  layer. Detached HEAD → the hash, no `head_ref`.
- **Redact credentials** from `remote_url` (`https://user:token@host/…` → `https://host/…`) before
  storing — AIRA's secrecy posture. Multiple remotes → prefer `origin`; record explicit "none" if
  absent.
- **`worktree_path` "if appropriate":** the working-tree root; for a *linked* worktree
  (`gitdir != commondir`) record the linked path; `worktree_id` reuses AIRA's existing identity.
- **HEAD is call-time, per-worktree provenance — NOT daemon-cached scope identity.** The daemon
  caches the scope (root/gitdir/worktree) by a stable config digest, but HEAD moves every commit and
  each worktree has its own HEAD, so the **client** resolves `GitContext` and passes it in the
  request; the daemon stores it verbatim (it cannot derive the caller's commit).
- **Scope — diagnostic/state-changing verbs only.** A per-verb dispatch-table flag opts a verb in
  (rant, run, finding, telemetry, review). Hot read paths (`ls`/`get`/`count`/`grep`) do NOT pay the
  resolution latency for provenance they don't durably keep.
- **Honesty.** Not-a-git-repo / unreadable HEAD / no remote / detached → explicit `unevaluated` /
  `none`, never a faked value. Optional cheap `dirty?` bit is a later opt-in (`git status
  --porcelain` is pricier).

## 5. Faces
- **CLI:** `aira rant "<text>" [--tag T]… [--about RUN-n]`; `aira rant ls [--by tag] [--since <seq>]
  [--unreviewed]`; `aira rant review RANT-n [--note "…"]`.
- **MCP:** `aira_rant({ text, tags?, about_run? })`. **The MCP tool description advertises it — the
  only "prompt" the owner permits:** e.g. *"Ranting welcome. If something in this repo wastes your
  time — slow tests, linter noise, flaky infra, confusing setup — dump it here, unfiltered. It's
  logged for later review; you won't be asked to format it."* Never nagged per-turn.
- **Grep (FTS)** over rant bodies; **`watch` (D3)** streams new rants live to an analysis agent;
  one **insight gauge** — "rant themes by tag + trend + unreviewed-since-last-pass" (a live query,
  never a stored numeral, §17).

## 6. Reviewability (the actual deliverable)
The value is realised in the **review pass**, not the individual rant. The reviewer (human or agent)
needs to **cluster by tag** (+ FTS similarity), **sort by frequency/recency**, see provenance +
git-context, and — critically — know **"what's new since I last looked"** (the `seq` cursor / D3
`watch` give this for free, so a pass is incremental). The **minimal reviewed-marker** keeps the
pass cumulative: the next reviewer isn't re-reading the same 200 gripes, and the disposition is
durable.

## 7. Honesty + anti-noise invariants
1. A rant is a subjective opinion, typed as such; aggregates read "N rants about X".
2. **Cheap to file** (text-only, everything else auto/optional), **never prompted** (context block
   only), **aggressive to aggregate** — the system must never become the thing agents rant about.
3. Rants are a **sidecar stream** — they never pollute primary work views (`ls`/`backlog`/`ready`).
4. git-context: `unevaluated` when unresolvable; credentials redacted.
5. Over-size body → stable `E_RANT_TOO_LARGE`, never silent truncation.

## 8. Deferrals
Promote-to-ticket/lint lifecycle (`derived-from` edges + full disposition); cross-project rant
aggregation (AIRA is machine-local/per-project today); live command-telemetry capture (retrospective
ingestion + a hook — a separate milestone); ML/embedding clustering; the `dirty?` bit.

## 9. Open questions for the model rounds
- **Entity vs pure event.** A first-class allocated `RANT-n` entity (carries a reviewed-marker + is
  referenceable) vs a pure append-only event. (Lean: lightweight entity.)
- **Journal vs DB-only** for rants. (Lean: journal — low-frequency, provenance-bearing.)
- **git-context inside rant vs its own cross-cutting milestone first.** (Lean: build the primitive
  within rant as the first consumer, designed for reuse.)
- **Tag model** — free vs suggested-enum vs both. (Lean: free + a suggested seed set.)
- **Reviewed-marker granularity** — per-rant vs per-cluster.
- **De-dup at capture** — none (log everything, dedupe at review) vs a cheap near-dup hint.
- Is `about_run` enough, or should a rant also optionally reference a ticket/finding/gate?
