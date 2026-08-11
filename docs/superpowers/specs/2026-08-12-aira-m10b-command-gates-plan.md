# AIRA M10b — command-backed gates (implementation plan)

Status: plan (reconciles the M10b design DRAFT against the real M10a + M12 APIs)
Date: 2026-08-12
Base: master `2b9229e` (M10a merged)
Design: [`2026-08-11-aira-m10b-command-gates-design.md`](2026-08-11-aira-m10b-command-gates-design.md)
Prerequisites: M10a gate honesty engine (merged); M12 runner-lite (merged)

The design draft was written before M10a's final API existed and proposed two
runner mechanisms (`OnScopeReady` callback, explicit-env request field). This
plan pins the reconciliation against the **actual** merged code, resolves the
mechanism choices, and gives the build map. The draft remains the authority on
scope, non-goals, the verdict/honesty rules, and the test matrix; this plan
overrides it only where it names a real seam.

## 1. Reconciliation with the merged code (decisions)

1. **Gate-owned timeout → runner-internal `Request.Timeout` (NOT `OnScopeReady`).**
   `runner.Launch` (internal/runner/runner_linux.go:74) reserves the run ID
   internally (line 118) and blocks on `cmd.Wait()` (220-222) with **no
   `ctx.Done()` case** — the comment at :164 deliberately avoids ctx-driven
   cancellation because it would `Process.Kill` the main PID (unsafe). So a
   caller `context.WithTimeout` cannot drive a scoped kill, and the draft's
   `OnScopeReady`-callback-then-`Kill` is unnecessary complexity that leaks scope
   timing to the gate layer. DECISION: add `Timeout time.Duration` to
   `runner.Request`; in the wait loop `select` on `waitCh`, `time.After(Timeout)`
   (and keep the existing membership monitor); on expiry perform the **existing
   whole-scope kill** (`r.killScope`) and record a terminal `RunRecord` with
   `Status=killed` + a new `E_RUN_TIMEOUT` error code. Zero timeout = no timeout.
   The gate maps `E_RUN_TIMEOUT` → `U_GATE_COMMAND_TIMEOUT` (unevaluated, never
   fail-as-pass). This keeps all process control cgroup-scoped and inside the
   runner, where the scope + kill already live.
2. **Explicit allow-listed env → `Request.ExplicitEnv bool`.** Today
   `Request.Env` are overrides layered on the inherited environment
   (types.go:109; `cmd.Env = env` at runner_linux.go:167). M10b needs a stripped
   environment. DECISION: add `ExplicitEnv bool`; when true the child env is
   EXACTLY `req.Env` (no inheritance). The gate computes the allow-listed
   `KEY=VALUE` set (reading each allowed name's current caller value) and sets
   `ExplicitEnv=true`. `RunRecord.EnvDigest` already records the digest.
3. **Checker dispatch seam.** M10a's `EvaluateDimension(root, dimension)`
   (gate_eval.go) is the `check-dimension` checker; RunGate calls it for both the
   subject and (via `runFixtureCanary`) the canary. DECISION: introduce
   `evaluateChecker(def, root) (predicate, evidence, error)` dispatching on
   `def.Lane.Checker`: `"check-dimension"` → `EvaluateDimension`; `"command"` →
   new `runCommandChecker`. BOTH subject and canary evaluation go through it, so a
   command gate's canary runs the same command lane against its mutated root.
4. **Admissibility from the real `RunRecord`.** The draft's admissibility maps to
   real fields: `Status==StatusExited`, `ExitCode!=nil && *ExitCode==0`,
   `ScopeIntegrity==ScopeContained`, `CaptureComplete`, `TerminalComplete`,
   `len(ErrorCodes)==0` — i.e. `RunRecord.CleanSuccess()` (types.go:96) for the
   exit-zero predicate. A non-zero clean exit → command predicate `fail`
   (`E_GATE_COMMAND_FAILED`, exit code retained). Any killed/lost/partial/
   integrity-compromised/timeout run → unevaluated (`U_GATE_COMMAND_*`), never a
   pass. Output is read via `runner.ReadOutput` (bounded); overflow past the
   committed cap → `U_GATE_OUTPUT_OVERFLOW` (never parse-a-prefix-as-pass).
5. **Mutation canary seam.** `runFixtureCanary` currently rejects any non-fixture
   mode. DECISION: rename/refactor to `runCanary` dispatching on `c.Mode`:
   `fixture` (existing), `mutation` (new). Mutation applies a typed, closed-union
   seed (`go-negate-assertion`, `go-inject-failing-test`) to an isolated copy of
   the subject tree (reuse M10a's `safeFixturePath`/`copyFixtureSeed` escape
   guards + byte-for-byte caller-tree-unchanged), then runs the same command lane
   via `evaluateChecker` and asserts predicate `fail` (fires) → proof, else
   `E_GATE_CANARY_DID_NOT_FIRE`.
6. **Proof scope + audit.** The command lane config (argv, cwd policy,
   env-allow-list, timeout, output-cap, parser, predicate) is part of the
   committed gate definition, so `gate.DigestGate` already binds it into
   `definition_digest` — the M10a proof/`GateCheck` re-validation covers command
   lanes with no new binding needed beyond the mutation-declaration digest (which
   M10a already folds via the canary declaration digest). `validateGateAuditFields`
   (gate_audit.go) is unchanged: command results are ordinary `result` records
   (gate_id/subject/verdict). Schema bump: gate `schema_version` → 2 for
   command-bearing fields (M10a stays valid at v1).

## 1b. Sol plan-review resolutions (verdict BLOCK — all incorporated)

Thread `019ff2a2`. These OVERRIDE the §1 decisions where they conflict; each gets
a mandatory regression test.

- **R1 (P0, overrides §1.1 timeout):** the timeout must NOT call `killScope`
  directly — that bypasses durable `KillIntent` and the arbitration, and drops
  `monitorScopeMembership` migration evidence. Factor ONE shared kill path used by
  both `Kill()` and the timeout: publish durable kill intent FIRST → kill scope →
  record `E_RUN_TIMEOUT` → merge monitor + capture evidence → commit via the
  existing `appendTerminalLocked` CAS. A run that exited cleanly just before the
  deadline keeps its clean-exit terminal (not rewritten to killed); a genuinely
  timed-out run can never be read later as a clean success. Test: timeout-vs-exit
  race yields exactly one terminal record with correct arbitration; a timed-out run
  is `killed`+`E_RUN_TIMEOUT`, never `CleanSuccess()`.
- **R2 (P0, overrides §1.2 env):** in Go, `cmd.Env == nil` means INHERIT. So
  `ExplicitEnv` must set `cmd.Env` to a **non-nil** slice (empty slice for empty
  env), built from the canonicalised/validated allow-listed `KEY=VALUE` set, and
  the recorded `EnvDigest` must be over THAT exact child set (not
  `effectiveEnvironment`, which includes inherited vars). The command evaluator
  ALWAYS sets `ExplicitEnv=true`. Tests: empty allow-list → truly empty child env
  (a probe var set in the parent is ABSENT in the child); digest = exact set;
  an unlisted inherited var never reaches the child.
- **R3 (P0, overrides §1.6 proof):** `DigestGate` binds only serialized
  `GateDefinition` fields, and the effective `EnvDigest` is evaluation-time data
  absent from proof validation → changing an allowed env VALUE would reuse an old
  proof. Fix: (a) embed the **canonical command lane** (argv, cwd policy,
  env-allow-list, timeout, cap, parser, predicate) as real serialized fields of the
  definition so `DigestGate` binds them (not a free-form `Lane.ConfigDigest`); and
  (b) record the run's effective `env_digest` in the result + proof-of-fire records,
  and have `GateCheck` recompute the current allow-listed env digest **read-only**
  (read the allow-list from the definition + the current values) and downgrade to
  `U_GATE_PROOF_STALE` on mismatch. Test: editing an allowed env value or any lane
  field invalidates a prior command-gate pass at `gate check`.
- **R4 (P1, overrides §1.4 admissibility):** do NOT rely on `CleanSuccess()` alone —
  it ignores `CaptureForcedClosed` and `OutputRefs` states (an evicted/partial
  output could slip through) and it cannot distinguish a clean NON-zero exit (→
  `fail`) from an unevaluable run. Add a dedicated admissibility predicate that
  requires `CaptureForcedClosed==false` and every consumed `OutputRef.State ==
  complete`, classifies exit==0 (candidate pass) vs clean exit!=0 (`fail`) vs
  non-admissible (unevaluated), and makes `ReadOutput` reject `Truncated`/incomplete/
  byte-count-mismatch BEFORE the parser runs. Tests: evicted/partial/forced-closed →
  unevaluated; clean nonzero → `E_GATE_COMMAND_FAILED`.
- **R5 (P1, overrides §1.5 mutation isolation):** `copyFixtureSeed` was written for
  small fixture file-maps and rejects `.git`; it is unsuitable for a full subject
  repo and must not tempt a caller-tree fallback. Specify a dedicated isolated
  **subject snapshot**: materialise the tracked files (coherent `git ls-files`
  snapshot, like M9c) into a temp dir, `git init` it, apply the typed mutation,
  validate symlink/path escapes, and **hard-fail** (`U_GATE_MUTATION_APPLY_FAILED`)
  if materialisation is unavailable — never run in the caller tree. Test: mutation
  runs only in the snapshot; caller tree byte-for-byte unchanged; unavailable
  materialisation → unevaluated, not pass.
- **R6 (P1, parser completeness):** requiring package terminal events still permits
  `run(TestX)` → package `pass` with no terminal event for `TestX` (count>0 but
  `TestX` never terminated). Require EVERY discovered test/subtest to reach a valid
  terminal action (`pass`/`fail`/`skip`) with balanced start→terminal transitions;
  an unterminated discovered test → `U_GATE_PARSER_INCOMPLETE`. Test: that exact
  case.

## 2. Build map

- **`internal/runner/` (small M12 extension, its own adversarial care):**
  `Request.Timeout` + wait-loop `time.After` → whole-scope kill + `E_RUN_TIMEOUT`
  (register in the runner code catalog); `Request.ExplicitEnv` → exact child env.
  Tests: timeout kills a setsid grandchild + terminal `killed`+`E_RUN_TIMEOUT`;
  a kill-vs-exit race near the deadline follows existing arbitration; explicit env
  strips inherited vars and the digest matches. **These are REAL-CGROUP tests →
  they SKIP in the Codex sandbox (read-only cgroup mount); Opus gates them on this
  box (the M12 lesson).**
- **`internal/gate/` (domain):** command lane types (`Command{Argv, Cwd,
  EnvAllow, TimeoutMS, OutputCapBytes, Parser, Predicate}`), `Checker=command`,
  predicates `exit-zero|tests-green`, mutation seed closed union + validation
  (reject arbitrary patches / unknown kinds / non-`fail` expected). Schema v2
  decoder. Pure `go-test-json-v1` parser (exit0 ∧ parser-complete ∧ count>0;
  strict event grammar; package terminal events required). Table tests.
- **`internal/store/gate_eval.go`:** `evaluateChecker` dispatch; `runCommandChecker`
  (build allow-listed env, materialise the subject `EvaluationRoot`, `runner.Launch`
  with `Timeout`+`ExplicitEnv`, admissibility from `CleanSuccess`, read+parse output,
  predicate fold); `runCanary` mutation mode + typed transformers (AST negate /
  inject-failing-test). New codes `U_GATE_COMMAND_TIMEOUT`, `U_GATE_OUTPUT_OVERFLOW`,
  `U_GATE_PARSER_INCOMPLETE`, `U_GATE_COMMAND_RUN_UNEVALUATED`,
  `E_GATE_COMMAND_FAILED`, `U_GATE_MUTATION_APPLY_FAILED` in the store catalog.
- **faces:** extend the existing `aira gate` descriptors (command-lane fields on
  add/set; mutation canary on canary-run/show) — no new verb. Descriptor drift +
  CLI↔core↔MCP parity + example-reaches-core for the new fields.
- **Store wiring:** `runCommandChecker` needs the runner handle. The store already
  has no runner seam; thread the `*runner.Runner` (from `app.Open`, project.go:123)
  into the gate evaluator (a store field or an eval-time parameter), mirroring how
  core holds the Runner.

## 3. Tests (design §8 subset that lands in M10b)

All of design §8.1–8.7 EXCEPT what needs a full archive. Every confirmed
counterexample → regression test. Correctness-critical set gated by the two-loop:
timeout→unevaluated (not pass), output-overflow→unevaluated (no prefix-parse),
tests-green zero-count→unevaluated, command runs ONLY via the runner (never bare
`os/exec`), mutation non-fire→`E_GATE_CANARY_DID_NOT_FIRE`, caller tree unchanged,
command pass impossible without current proof-of-fire, env allow-list + digest.

## 4. Process

plan (this) → **Sol plan-review** (runner timeout race, explicit-env correctness,
admissibility completeness, mutation isolation, no-shell argv, proof binding) → fix
→ **delegated build** (Codex, self-contained multi-stage TDD brief, FREQUENT COMMITS
per the timeout-salvage lesson; note real-cgroup tests skip in sandbox) → **Sol
build-review + Opus real-cgroup e2e** (a real `go test -json` gate + a mutation
canary + a timeout, on this box's real cgroups — LOAD-BEARING like M12) → gate →
`git -C ~/aira merge --ff-only`. Then M11 (review-depth).

## 5. Accepted deviations (extend the draft)

1. Timeout is runner-internal (`Request.Timeout`), not the draft's `OnScopeReady`
   callback (§1.1). 2. Explicit env is `Request.ExplicitEnv` (§1.2). 3. First
   parser is `go-test-json-v1` only (draft §10.1). 4. `tests-green` is a single-run
   predicate, no `TestReport` archive (→ Phase 4). Any further departure adds a
   recorded subsection naming the invariant + test.
