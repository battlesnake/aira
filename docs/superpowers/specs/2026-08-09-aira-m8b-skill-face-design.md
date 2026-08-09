# AIRA M8b — Skill face and generated agent guide over `core.Do`

Status: plan (author: Opus, owner-delegated). Completes Phase 2. Prerequisite
M8a (typed argument model + descriptor-generated MCP face) is landed on master
at `ae2b34d`.

## 1. Scope

M8b adds the **Skill face** — the third thin adapter named in the whole-product
design (`CLI, MCP, Skill, daemon, TUI are thin faces over one core`) — and the
**generated agent guide** that the repository contract calls for
(`Generated help, MCP schemas, and the agent guide come from dispatch tables`).

The single behavioural authority remains `core.Do`. M8b adds **no** new verbs,
no store behaviour, and no second command implementation. It is a *projection*
of the existing dispatch descriptors into two generated artifacts plus a small
amount of descriptor metadata those artifacts honestly require.

M8b delivers:

1. **A minimal descriptor metadata extension.** Each dispatchable verb gains a
   `SafetyClass` and a one-line `Summary`. These are the two facts the Skill and
   guide must state that the current descriptor cannot derive: whether an action
   mutates state / takes a lease (so the Skill can warn), and a concise
   human/agent instruction. Both are surfaced through `DispatchDescriptors()`.

2. **An installable Skill package,** emitted by a new `aira skill` face:
   - `aira skill install <dir>` writes a self-contained Skill package into
     `<dir>`: a `SKILL.md` (agent-facing instructions with `name`/`description`
     frontmatter) and a machine-readable `aira.skill.json` manifest.
   - The manifest declares Skill identity, a **deterministic content version**
     (a hash over the descriptor surface, so the version changes iff the surface
     changes), the invokable entrypoint (the `aira` binary) and, for every
     canonical action, its argument contract, safety class, and the exact
     command shape (`aira <verb> …`) that reaches `core.Do`.
   - The action catalog, argument contract, safety warnings, and examples are
     **generated from `DispatchDescriptors()`**. No action is authored by hand.

3. **A generated agent guide,** emitted by `aira skill guide` (to stdout): the
   same descriptor-derived content rendered as a single Markdown document for
   agents that consume prose rather than a package. It carries a small authored
   preamble (what AIRA is, the honesty contract) followed by fully generated
   per-action sections.

Both artifacts must state the two non-negotiable honesty facts: responses carry
**stable AIRA codes**, and **`unevaluated` is not a pass** (and not zero).

## 2. Non-goals / explicit deferrals

- **No runtime Skill execution engine.** AIRA's Skill entrypoint *is* the `aira`
  CLI; the Skill package instructs the host to invoke `aira <verb>`, which is
  already the CLI face over `core.Do`. M8b does not add an in-process action
  dispatcher distinct from the CLI — that would be a fourth surface able to
  drift. "Invokable" is satisfied by the entrypoint being the real binary whose
  every action is proven, by test, to build the same canonical `core.Request`
  as the CLI and MCP faces.
- **No per-verb hand-authored examples or long-form guidance strings** beyond
  the generated ones and the single authored preamble. Examples are generated
  from the argument specs (positional order + one representative value per
  required arg). This keeps the artifacts un-driftable.
- **No new safety enforcement.** `SafetyClass` is descriptive metadata for
  warnings and schemas; it does not gate execution. Enforcement (confirmation
  prompts, lease checks) already lives in the store/domain layers.
- **No telemetry, no daemon, no traceability graph.** Those are Phase 3+.

## 3. Invariants

1. **One authority.** The Skill action catalog and the agent guide are derived
   solely from the same dispatch table `core.Do` uses, via
   `DispatchDescriptors()`. There is no second registry of actions, arguments,
   or safety facts. A verb surfaced for dispatch but missing required Skill
   metadata is a build-failing condition (golden/consistency test), not a silent
   omission.
2. **Face parity.** For every generated Skill action, the command shape it
   documents, when parsed by the CLI argv builder, must produce byte-identical
   `core.Request` to the MCP face for the same operation. This is the M8a MCP↔CLI
   parity guarantee extended to Skill↔CLI, proven by a table test mirroring
   `TestMCPGroupedOperationsBuildTheSameCanonicalRequestsAsCLI`.
3. **Honest safety.** Every mutating or lease-taking action is marked as such in
   both artifacts. A read-only action is never marked mutating and vice-versa.
   The `SafetyClass` of each verb is asserted against a golden table so a future
   verb cannot be added without a deliberate, reviewed safety classification.
4. **Deterministic generation.** Generation uses no clock and no randomness. The
   manifest version is a stable hash of the descriptor surface. Re-running
   `aira skill install` on an unchanged binary into an unchanged directory
   produces byte-identical output (idempotent; golden-testable). A metadata
   change produces an expected, reviewable diff.
5. **Guide-only does not satisfy the deliverable.** `aira skill install` must
   emit *both* the human-facing `SKILL.md` and the machine-readable manifest
   with the entrypoint identity and enumerable action catalog. A test asserts the
   manifest exists, parses, declares the entrypoint, and enumerates every
   includable canonical action; prose alone fails.
6. **Coverage is the canonical includable set.** The Skill actions and guide
   sections cover exactly the canonical verbs that reach `core.Do` and are
   marked includable (the same set the MCP face exposes, de-aliased: `new`/`get`/
   `ls` do not create duplicate actions; `help` is not an action). A drift test
   asserts the action set equals the includable verb set.

## 4. Design

### 4.1 Descriptor metadata

`ArgSpec` is unchanged. Add to `verbSpec` and mirror into `DispatchDescriptor`:

- `Summary string` — one concise line ("Create a ticket and return its ID").
- `Safety SafetyClass` — a closed enum:
  `read` (no state change), `mutate` (writes tickets/findings/relations),
  `lease` (acquires/renews/releases a lease), `reconcile` (rebuild/heal/check).

`SafetyClass` is a new closed string type in `internal/core` with a small
validated set. Every dispatchable verb (except `help`) must set both fields;
`DispatchDescriptors()` carries them; a consistency test fails if any includable
descriptor has an empty `Summary` or an unset/invalid `Safety`.

Classification (authoritative table, asserted by golden test):

| verb | safety | note |
|---|---|---|
| `init` | reconcile | creates project scaffolding + index |
| `id` | mutate | advances a persistent allocator — not a free read |
| `create` | mutate | |
| `show`, `list`, `count`, `grep`, `ready` | read | |
| `find` | mutate | `add`/`set` mutate; `ls`/`show` read — the tool is grouped, classified by its most-privileged operation with a per-operation note in the generated text |
| `set`, `mv` | mutate | |
| `claim`, `release`, `heartbeat` | lease | |
| `touch` | mutate | area hints; requires lease ownership |
| `link`, `unlink` | mutate | |
| `import` | mutate | |
| `reconcile`, `check` | reconcile | |

`id` is reclassified `mutate` (it advances a persistent allocator — an agent must
know it is not a free read). Grouped tools (`find`, `link`, `transition`) are
classified by their most-privileged operation and the generated per-action text
states the read/write split explicitly. The plan-review gate should confirm this
classification before build.

### 4.2 The `skill` face

`cmd/aira/skill.go`, dispatched from `Run` exactly like `mcp`:

```
if argv[0] == "skill" { return runSkill(argv[1:], stdout, stderr) }
```

Subcommands:

- `skill guide` → render the agent guide Markdown to stdout, exit 0.
- `skill install <dir>` → create `<dir>` if absent, write `<dir>/SKILL.md` and
  `<dir>/aira.skill.json`, print the written paths + version, exit 0. Refuse
  (stable error, non-zero exit) if `<dir>` exists as a non-directory, or if a
  target file exists and differs and `--force` was not given (idempotent
  overwrite of identical content is allowed).
- `skill` with no/unknown subcommand → usage to stderr, non-zero exit.

The generator lives in `internal/core` (or a small `internal/core`-adjacent
generator that takes `[]DispatchDescriptor`), so it is unit-testable without the
filesystem and shared by both subcommands. `cmd/aira/skill.go` is a thin
adapter: it calls the generator and writes bytes. No dispatch logic in the face.

### 4.3 Generated content contract

Per includable canonical action, both artifacts state: canonical name, one-line
summary, safety class (with the grouped read/write note where relevant), the
argument list (name, kind, required, positional-vs-option, enum values where
closed, description), the concrete command example, and — once, in a shared
"Response contract" section — that every response carries a stable `code`, that
verdicts are `pass`/`fail`/`unevaluated`, that `unevaluated` is neither pass nor
zero, and the exit-code mapping.

The manifest (`aira.skill.json`) is the machine-readable projection: identity
(`name: "aira"`, `entrypoint: "aira"`), `version` (descriptor-surface hash),
`response_contract` (stable-codes + unevaluated note as structured fields), and
`actions[]` each with `{verb, summary, safety, command, args[]}`. It is the
enumerable catalog invariant 5/6 test against.

## 5. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Generated Skill/guide silently falls out of sync with the dispatch table | Golden tests on the action-name set, per-action arg descriptions, safety table, and manifest; a fail-closed consistency test requires every includable verb to carry `Summary`+`Safety`. |
| A future verb ships without a safety classification | `DispatchDescriptors()` consistency test fails on empty/invalid `Safety`; safety golden table must be updated deliberately. |
| Skill documents a command the CLI would parse differently → agent runs the wrong request | Skill↔CLI parity table test builds each generated command through the CLI argv path and asserts identical `core.Request`, extending the M8a MCP↔CLI parity guarantee. |
| Manifest version churns on every build (non-determinism) | Version is a deterministic hash of the descriptor surface; no clock/random; idempotency golden test. |
| "Guide-only" output masquerading as the deliverable | Install-artifact test asserts manifest presence, parseability, entrypoint declaration, and full action enumeration; prose alone fails. |
| Grouped-tool safety (`find` add vs ls) misleads | Classified by most-privileged operation; generated per-action text states the read/write split; golden-tested. |

## 6. Tests (TDD; every confirmed counterexample becomes a regression test)

1. **Descriptor consistency (fail-closed).** Every includable descriptor has a
   non-empty `Summary` and a valid `Safety`; `help` is exempt; an injected empty
   value fails. (Proven load-bearing by temporarily blanking one.)
2. **Safety golden table.** `verb → SafetyClass` matches the authoritative table
   in §4.1 exactly; adding/removing a verb or changing a class fails until the
   table is updated.
3. **Action-set drift.** The generated Skill action set and guide section set
   each equal the includable canonical verb set (de-aliased, `help` excluded) —
   mirrors the MCP `tools/list` golden.
4. **Skill↔CLI parity.** For a representative command per action (including
   grouped `find`/`link`/`set`/`mv`), the generated `command` parsed by the CLI
   argv builder yields byte-identical `core.Request` to the MCP path.
5. **Manifest artifact.** `skill install` writes a `aira.skill.json` that parses,
   declares `entrypoint: "aira"`, carries the response-contract fields, and
   enumerates every includable action with args+safety; a guide-only variant
   (SKILL.md but no manifest) fails this test.
6. **Determinism / idempotency.** Two installs into the same dir on the same
   binary produce byte-identical files and equal versions; a metadata change
   changes the version and the diff is the expected one (golden on version-hash
   input).
7. **Honesty content.** Both `SKILL.md` and the guide contain the stable-codes
   statement and the explicit "`unevaluated` is not a pass / not zero" statement;
   removing it from the generator fails the test.
8. **Face wiring / exits.** `skill guide` exits 0 and prints Markdown to stdout;
   `skill install <newdir>` exits 0 and writes both files; unknown subcommand and
   install into a non-directory path yield a stable error and non-zero exit;
   re-install of identical content without `--force` succeeds (idempotent), a
   differing target without `--force` fails.
9. **Build constraint.** The new face compiles into the existing static/no-cgo
   binary; generation requires no daemon, network, or external process.

## 7. Expected yield

Agents gain a discoverable, installable Skill whose action catalog, argument
contract, and safety warnings are provably truthful to the verbs that exist and
provably build the same core requests as the CLI and MCP faces — plus a
generated agent guide that cannot silently diverge from the implementation. This
closes the third face named in the whole-product design and completes Phase 2
(findings + surfaces + import + all three thin faces over one core).

## 8. Out-of-scope confirmation

M8b does not add runner, telemetry, gates, traceability enforcement, or any new
store behaviour. It does not hand-author per-action guidance. It does not enforce
safety at execution time. Those remain Phase 3+ and the store/domain layers.
