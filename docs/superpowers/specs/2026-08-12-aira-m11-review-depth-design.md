# AIRA M11 — `aira review` + review-depth recommendation (design)

Status: PLAN-REVIEWED (Sol BLOCK → 6 findings incorporated, §1b). Author: Opus
(coordinator). Milestone: Phase 3 · M11 (the last Phase-3 milestone; M9a/b/c ✓ M12 ✓
M10a/b ✓). Base: master `6d07521`.

## 1b. Sol plan-review resolutions (thread 019ff302 — BLOCK, all incorporated)

- **R1 (P0) — `default_tier` must never be 0, and absent/null must not decode to 0.**
  `default_tier` is a presence-distinguishing `*int`: **absent/null ⇒ 3** (top);
  **explicit value must be 1..3** — `0` (and `<0`, `>3`) is `E_CONFIG_INVALID`. Tier 0 is
  reachable *only* via an explicit `path_tiers` rule with `tier:0` that a concrete path
  matches — never via the unknown/unmatched fallback. (§3.1, §3.2, §3.5.)
- **R2 (P0) — unrecognised kind/severity fail-closed, not to 0.** The engine validates its
  `kind`/`severity` inputs against `domain.Kind`/`domain.Severity`. A *valid* enum with no
  floor entry contributes nothing (correct — the path drives). An **unrecognised or empty**
  kind/severity (corrupt/legacy ticket) contributes `default_tier` with basis
  `unrecognised-kind ⇒ default_tier`. (§3.2; tests T17/T18.)
- **R3 (P1) — matched-low + unmatched combo.** Add T16: `{docs/x.md (tier0),
  internal/unknown.go (unmatched)}` ⇒ `default_tier`. Defends against an impl that drops the
  unmatched leg (which would pass T2 alone yet under-recommend the mixed set).
- **R4 (P1) — presence/pointer validation for all policy fields; eager Open-time refusal.**
  `review:null`, `path_tiers:null`, `kind_floor:{"bug":null}`, omitted `default_tier` must
  each be rejected `E_CONFIG_INVALID` (never coerced to zero/nil-and-default-up-silently).
  **Validation is eager at store Open — rejecting the whole project is acceptable and
  fail-closed** (Sol-blessed); **absent `review` block remains the sole default case.**
  (§3.5.)
- **R5 (P1) — `--paths` presence tracked separately from length.** An explicitly-provided
  empty `--paths` (`paths_source: arg`, empty values ⇒ default-up) must be distinguishable
  from omission (`paths_source: area-hints`). The handler checks arg *presence*, not just
  `len>0`. (§3.4; tests T12, T19.)
- **R6 (P2) — glob-vs-glob regression.** Add T20: an area-hint glob vs a policy glob
  (`internal/store/**` vs `internal/store/**`) overlaps and inherits the tier. (§6.)

Authoritative parent: `docs/superpowers/specs/2026-08-07-aira-design.md` §16 (review
emission, routing, import), §6 (gate policy holds the path→review-tier map), §9 (gates),
§20 (Phase 3 names `review` + review-depth). This spec resolves the M11-specific
decisions and fixes the adversarial test matrix. It does **not** re-open settled
architecture.

---

## 1. Scope

Two deliverables, one read-only verb.

1. **A review-depth recommendation engine.** Given the set of paths under review, the
   ticket's kind, and its severity, recommend a review **tier 0–3** — the analogue of
   an earlier project's `review_tier.py`: **MAX over all contributing factors**, **fail-closed
   default-up** for anything unmatched or unknown. The path→tier map, kind floors, and
   severity floors live in the project gate policy (`.aira/config` `project.review`).

2. **`aira review <selector> [--paths a,b,…]`.** Assemble a context-loaded review bundle
   for the agent to act on: the recommended tier and *why*, the ticket context, its open
   findings and relations, the **hardcoded Codex-first / Fable-final routing convention**
   keyed by tier, and the exact `aira find add …` instruction the reviewer uses to report
   its verdict.

`review` is **advisory and read-only**. It recommends and assembles; it never mutates,
never runs the review, never blocks a transition. The reviewer runs its own tools and
reports findings through the existing M5 `find add` face.

### 1.1 Non-goals / deferrals (written down; do not build)

- **D1 — economics-driven tuning (§12).** Tiers are computed from *static* area/kind/
  severity policy only. Tuning thresholds on recorded review-loop economics is a Phase-4
  concern (telemetry does not exist yet). No compute/telemetry capture in M11.
- **D2 — per-project review-routing DSL (§21).** Routing is a **hardcoded tier→reviewers
  table** (Codex-first, Fable-final). A configurable routing DSL is deferred.
- **D3 — `review` does not execute or mutate.** Running Codex/Fable and recording the
  verdict is the agent's job via `aira find add … --source <reviewer>`. M11 adds no new
  finding-write path.
- **D4 — no cross-ticket recurrence insight in the bundle.** The bundle surfaces *this
  ticket's* open findings only. "Which classes recur across history" is the §17 insights
  layer (Phase 4).
- **D5 — no `check`/`ready` folding.** A tier is *advice*, not a gate verdict. It is not a
  `check` dimension and does not affect `ready`. (Gate honesty already lives in M10.)

---

## 2. Tier vocabulary

Closed enum, 0–3, ascending depth. Fixed here; a project tunes *which paths map to which
tier*, not the meanings.

| Tier | Meaning | Hardcoded routing (the bundle emits this) |
|---|---|---|
| 0 | Trivial / mechanical (docs, comments, formatting). | Self-review by the author; no external reviewer required. |
| 1 | Light. One independent lineage. | Codex build-review. |
| 2 | Standard two-loop (the default working depth). | Codex build-review **then** Fable final. |
| 3 | Maximum adversarial; correctness-critical (ID allocation, crash recovery, lease CAS, gate honesty — cf. CLAUDE.md). | Codex + Fable final + at least one additional independent lineage. |

The routing column is **data** (a `tierRouting [4]…` table), emitted verbatim in the
bundle. Codex-first / Fable-final is the hardcoded convention (§16); D2 defers making it
configurable.

---

## 3. The recommendation engine (the correctness core)

Pure, deterministic function — no clock, no I/O, unit-testable in isolation:

```
RecommendReviewTier(paths []string, kind, severity string, policy ReviewPolicy)
    → (Recommendation, error)
```

### 3.1 Policy shape (`project.review` in `.aira/config`)

```json
"review": {
  "default_tier": 3,
  "path_tiers": [
    {"glob": "docs/**",            "tier": 0},
    {"glob": "internal/store/**",  "tier": 3},
    {"glob": "internal/runner/**", "tier": 3},
    {"glob": "cmd/**",             "tier": 1}
  ],
  "kind_floor":     {"bug": 2, "requirement-work": 1},
  "severity_floor": {"P0": 3, "P1": 2}
}
```

- `default_tier` — the tier a path contributes when it matches **no** `path_tiers` rule,
  the tier used when no paths are known at all, and the tier an unrecognised kind/severity
  contributes (R2). It is a **presence-distinguishing `*int`**: **absent/null ⇒ 3** (top);
  an **explicit value must be 1..3** — `0`/negative/`>3` ⇒ `E_CONFIG_INVALID` (R1). *You can
  never express "the unknown needs no review."* **Absent `review` block ⇒ policy
  `{default_tier: 3, no rules, no floors}`** (fail-closed default-up: everything reviews at
  the deepest tier until a project deliberately configures the map). Never a silent max —
  the recommendation states `basis: no-policy-configured` / `defaulted-up`.
- `path_tiers` — **unordered** set of `{glob, tier}` rules; each `tier` is 0..3 (a
  `path_tiers` rule *may* be tier 0 — an explicit "docs need no external review" mapping;
  tier 0 is reachable only by such an explicit concrete-path match, never by the unknown
  fallback). Order is irrelevant because the result is a MAX (3.2), removing first-match
  ambiguity by construction. `null`/malformed rules ⇒ `E_CONFIG_INVALID` (R4).
- `kind_floor` / `severity_floor` — a matched, **domain-valid** kind/severity raises the
  floor to its value (0..3). Absent key ⇒ no floor from that factor (contributes nothing).
  A `null` value or an unknown key (a kind/severity not in the domain enum) ⇒
  `E_CONFIG_INVALID` (R4, typo protection).

### 3.2 Computation — MAX over factors, fail-closed default-up

Let `T = 0`. Accumulate, keeping a `basis` entry for every contributor:

1. **Paths.** For each input path `p`:
   - For each rule `r` in `path_tiers`, if `AreaGlobsOverlap(p, r.glob)` is true, contribute
     `r.tier` (basis: `path p ↦ rule r.glob ⇒ tier r.tier`).
   - If `p` overlaps **no** rule, contribute `default_tier` (basis: `path p unmatched ⇒
     default_tier`). *This is the fail-closed default-up leg — an unmatched path escalates,
     never drops to 0.*
2. **No paths at all** (neither `--paths` nor any area hint): contribute `default_tier`
   (basis: `no-paths ⇒ default_tier`).
3. **Kind.** If `kind` is a valid `domain.Kind` and `kind_floor[kind]` present, contribute
   it (basis: `kind ⇒ floor`). If `kind` is **unrecognised or empty** (corrupt/legacy
   ticket), contribute `default_tier` (basis: `unrecognised-kind ⇒ default_tier`) — R2.
4. **Severity.** Symmetric to step 3: valid+floored ⇒ contribute the floor; **unrecognised
   or empty ⇒ `default_tier`** (basis: `unrecognised-severity ⇒ default_tier`) — R2.

`T = MAX(all contributions)`. The recommendation is `{tier: T, basis: [...], paths_source}`.

**Invariants (these are the properties the adversarial matrix defends):**

- **MAX, never MIN/first-match/average.** A ticket touching both a tier-0 and a tier-3 path
  is **tier 3**. This is the single most important property — a MIN/first-match bug is the
  exact "under-reviewed correctness-critical change" failure this milestone exists to
  prevent.
- **Default-up is the only behaviour for the unknown.** Unmatched path, no paths, no policy
  → the *top* configured tier, never 0. There is no code path that lowers the tier for
  missing information.
- **Floors raise, never lower.** `kind_floor`/`severity_floor` only ever add a contribution
  to the MAX; they cannot pull a tier-3 path down.
- **Always explained.** `basis` is non-empty for every recommendation; the tier is never
  emitted as a bare numeral (the §17 "carry the reason, never a stored numeral" law).
- **Deterministic.** Same `(paths, kind, severity, policy)` → same tier and same sorted
  basis. No clock, no map-iteration-order dependence (sort the basis).

### 3.3 Glob matching

Reuse `store.AreaGlobsOverlap` verbatim. A concrete path (`internal/store/gate.go`) is a
degenerate glob; a rule (`internal/store/**`) is a glob; overlap = their languages
intersect. `AreaGlobsOverlap` is deterministic, has no intentional false negatives for the
supported syntax, and **errs toward a false positive only for abstract directory-only paths
— which for a tier engine is the safe direction** (an extra match can only raise the MAX,
i.e. deepen review). Both sides are normalised via `NormalizeAreaGlob`; a malformed rule
glob is a config error (3.5), a malformed input path is an input error (E_GLOB_INVALID).

### 3.4 Path source (the input set) — arg, then area-hint fallback

`review_tier.py` runs on the concrete changed-file set. AIRA mirrors this:

- **`--paths` provided (even if empty) ⇒ use its values** (repo-relative; the reviewer
  passes the diff's files or the areas under review). `paths_source: arg`. **An explicitly
  empty `--paths` is NOT the same as omission** (R5): it means "no paths under review" ⇒
  empty set ⇒ default-up (3.2 step 2), and the handler must distinguish arg *presence* from
  `len>0` (the arg accessor tracks whether the flag was set, not just its length).
- **`--paths` omitted ⇒ fall back to the ticket's declared area hints** (`aira touch`). A
  new best-effort read `Store.TicketAreaGlobs(ticketID) []string` returns the distinct globs
  recorded for the ticket in `area_hints` (any worktree/generation — advisory input to a
  recommendation, so the cross-generation union is honest and lease-liveness is irrelevant).
  `paths_source: area-hints`.
- **Omitted and no hints ⇒ empty set ⇒ default-up.** `paths_source: none`.

The basis records `paths_source` so the reviewer sees whether the tier rests on a real
diff, on stale hints, or on nothing.

### 3.5 Fail-closed error handling (stable codes; never a silent guess)

- **Malformed `review` policy ⇒ `E_CONFIG_INVALID` at store Open, refuse (eager,
  Sol-blessed).** Rejected: `default_tier` present-and-outside-1..3 (incl. `0`);
  `path_tiers` tier outside 0..3; a `path_tiers` glob that fails `NormalizeAreaGlob`; a
  `kind_floor`/`severity_floor` value outside 0..3; an unknown `kind_floor`/`severity_floor`
  key (not in the domain enum — typo protection); and **presence-null coercions** —
  `review:null`, `path_tiers:null`, `kind_floor:{"bug":null}`, omitted `default_tier`
  decoding to zero — which pointer/raw-presence validation must catch, never silently
  coerce to zero/nil-and-default-up (R1, R4). **Rejecting the whole project at Open is
  acceptable and fail-closed.** **Absent** `review` block is *not* malformed — it is the
  documented default policy (3.1), the sole default case; a project that doesn't use review
  simply omits the block.
- **Ambiguous or missing selector ⇒ `E_AMBIGUOUS` / `E_NOT_FOUND`, no recommendation.**
  `aira review NOPE-9` must refuse; it never emits a default-up tier for a nonexistent
  ticket. Reuse the existing selector-resolution path (same as `show`).
- **Bundle sub-section load failure ⇒ that section reads `unevaluated` with a code, tier
  still computes.** If `ListFindings`/`Relations` errors while assembling the bundle, the
  findings/relations section is `unevaluated` (stable code), never fabricated or dropped
  silently. The tier recommendation is independent and still returned.

---

## 4. `aira review` — the bundle (read-only assembly)

New **top-level verb** `review` (distinct from the M10 `gate … review` manual-gate op).
`SafetyRead`. `MCPTool: aira_review`. Single operation (not grouped).

Args: `selector` (required, positional), `paths` (optional list), `fields` (optional
projection). Example argv: `["AIRA-1", "--paths", "internal/store/gate.go,docs/x.md"]`.

Bundle (`data`) fields:

- `ticket` — id, title, kind, severity, status, milestone, labels.
- `paths` — `{source: arg|area-hints|none, values: [...]}`.
- `tier` — `{recommended: 0..3, basis: [sorted strings], default_tier}`.
- `routing` — the tier's row from the hardcoded table (§2): the ordered reviewer list.
- `findings` — this ticket's **open** findings (`ListFindings("ticket:<id>")`, disposition
  open), each with id/category/severity/verdict/source/message — so the reviewer sees what
  is already outstanding. `unevaluated` with a code on load failure (3.5).
- `relations` — `Relations(id)` (blocks/blocked-by/relates/…); `unevaluated` on failure.
- `report_instruction` — the literal template the reviewer runs to record its verdict, e.g.
  `aira find add <id> --source codex --verdict confirmed|refuted|plausible --category <cat>
  --severity P0|P1|P2 --message "<...>" [--file path:line]`.

The bundle is a *briefing*, not a mutation. Emitting it twice yields identical output
(read-only, deterministic modulo the live store contents).

---

## 5. Layering & files

Downward layers respected (store ← app ← core faces):

- `internal/store/review_tier.go` — `ReviewPolicy` type + `LoadReviewPolicy`/validation +
  pure `RecommendReviewTier(paths, kind, severity, policy)` (reuses `AreaGlobsOverlap`) +
  `TicketAreaGlobs(ticketID)`. `ReviewPolicy` carried on `store.Options` (like
  `RequirementPrefixes`); validated at Open → `E_CONFIG_INVALID`.
- `internal/app/project.go` — `ProjectConfig.Review` JSON block; thread into
  `store.Options.ReviewPolicy`. (Mirrors the `requirement_prefixes` plumbing.)
- `internal/core/core.go` — the `review` descriptor (dispatch table): `Usage`, `Args`,
  `Summary`, `Safety: SafetyRead`, `Include: true`, verb-level `Example`, `MCPTool:
  aira_review`; handler resolves selector → gathers paths (arg/hint) → loads policy →
  `RecommendReviewTier` → assembles bundle. Metadata entry for the generated agent guide.
- Register any new codes in `store.ExitCodes` / `core.ResponseContract` (`E_CONFIG_INVALID`
  already exists; add `U_REVIEW_SECTION_UNEVALUATED` for 3.5 sub-section failure).

No runner, no telemetry, no daemon. No `check` dimension.

---

## 6. Adversarial test matrix (every confirmed counterexample becomes a regression test)

Pure-engine (`RecommendReviewTier`) — these defend the §3.2 invariants:

- **T1 MAX not MIN** — paths `{docs/x.md (tier0), internal/store/g.go (tier3)}` ⇒ **3**. A
  MIN/first-match/average bug yields 0/lower; this is the load-bearing test.
- **T2 default-up on unmatched path** — path matching no rule ⇒ `default_tier`, never 0.
- **T3 default-up on no paths** — empty path set ⇒ `default_tier`, basis `no-paths`.
- **T4 kind floor raises** — tier-0 path + `kind_floor{bug:2}`, kind=bug ⇒ **2**; and tier-3
  path + `kind_floor{chore:0}` ⇒ still **3** (floor never lowers).
- **T5 severity floor raises** — tier-0 path + P0, `severity_floor{P0:3}` ⇒ **3**; tier-3
  path + P2 ⇒ still **3**.
- **T6 absent policy ⇒ default-up top** — no `review` block ⇒ tier 3, basis
  `no-policy-configured`. (Not a silent max — basis states it.)
- **T7 malformed policy ⇒ E_CONFIG_INVALID** — tier 4 / tier -1 / invalid glob / unknown
  kind_floor key each refuse at Open; never coerced to empty.
- **T8 selector refusal** — missing ⇒ `E_NOT_FOUND`, ambiguous ⇒ `E_AMBIGUOUS`; **no**
  default-up tier for a nonexistent ticket.
- **T9 deterministic + explained** — same inputs ⇒ identical tier and sorted basis; basis
  never empty.
- **T10 glob-overlap honesty** — `internal/store/gate.go` overlaps `internal/store/**`
  (match); `internal/runner/x.go` does **not** overlap `internal/store/**` (no spurious
  match → does not inherit store's tier).
- **T11 sub-section failure is unevaluated** — a forced `ListFindings`/`Relations` error ⇒
  that section `U_REVIEW_SECTION_UNEVALUATED`, tier still computed and returned.
- **T12 path-source precedence** — `--paths` present ⇒ used, `paths_source: arg`; absent ⇒
  area hints, `paths_source: area-hints`; neither ⇒ `paths_source: none` + default-up.
- **T16 matched-low + unmatched (R3)** — `{docs/x.md (tier0), internal/unknown.go
  (unmatched)}` ⇒ `default_tier`, never 0. Catches a dropped unmatched leg.
- **T17 unrecognised kind fail-closed (R2)** — matched tier-0 path + kind=`"grbg"`/`""` ⇒
  `default_tier`, never 0.
- **T18 unrecognised severity fail-closed (R2)** — matched tier-0 path + severity=`""` ⇒
  `default_tier`, never 0. (Independent of T17.)
- **T19 explicit-empty `--paths` (R5)** — `--paths ""`/empty list provided ⇒ `paths_source:
  arg` + default-up; distinct from omission ⇒ `paths_source: area-hints`.
- **T20 glob-vs-glob (R6)** — area-hint glob `internal/store/**` vs policy glob
  `internal/store/**` overlaps ⇒ inherits that tier.
- **T21 default_tier presence (R1/R4)** — `default_tier:0` ⇒ `E_CONFIG_INVALID`; omitted
  `default_tier` ⇒ decodes to 3 (not 0); `review:null`/`path_tiers:null`/`kind_floor:
  {"bug":null}` ⇒ `E_CONFIG_INVALID`. (Config-load unit tests.)

Face-generation (per the M8b lesson — verify **every** generated artifact, not a sample):

- **T13 drift** — the `review` handler's read args exactly equal its declared descriptor
  args (the instrumented arg-accessor drift test extends to `review`).
- **T14 parity** — the declared verb-level `Example` parses via real `buildRequest` to the
  same `core.Request` the MCP path produces.
- **T15 E2E** — the `Example` runs through `Run()` against a temp project and reaches core
  (not a parse error); the generated agent-guide entry for `review` is present and its
  example is *valid* (the M8b "invalid example shipped" regression).

Real-binary e2e (Opus, post-build): `aira init` a project with a real `review` policy; a
tier-3 path recommends 3; a docs-only path recommends 0; an unmatched path defaults up; a
malformed policy refuses; the bundle carries findings + routing + report_instruction.

---

## 7. Build plan (delegated, frequent commits)

TDD, one stage per commit (salvage-on-timeout):

1. `ReviewPolicy` + validation + `LoadReviewPolicy` (T7) + `store.Options` wiring.
2. Pure `RecommendReviewTier` + basis (T1–T6, T9, T10). *Correctness core — heaviest tests.*
3. `TicketAreaGlobs` + path-source resolution (T12).
4. `review` core descriptor + bundle assembly + findings/relations + `U_REVIEW_SECTION_…`
   (T8, T11) + faces/metadata/agent-guide (T13–T15).
5. app config plumbing (`ProjectConfig.Review`) + full-suite + `make ci` green.

Gate: Sol plan-review (this doc) → incorporate → build → Sol build-review (both false-fail
and false-pass directions; the false-pass direction here is *under-recommendation* — a tier
lower than the true MAX) + Opus real-cgroup/real-binary e2e → fix → re-review → merge
`--ff-only`.
