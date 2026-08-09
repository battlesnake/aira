# AIRA M8b — Skill face and generated agent guide over `core.Do`

Status: plan (author: Opus, owner-delegated), revised after Sol plan-review
BLOCK (all P0s absorbed below). Gemini unavailable this round (free-tier quota).
Completes Phase 2. Prerequisite M8a (typed argument model + descriptor-generated
MCP face) is landed on master at `ae2b34d`.

## 1. Scope

M8b adds the **Skill face** — the third thin adapter named in the whole-product
design (`CLI, MCP, Skill, daemon, TUI are thin faces over one core`) — and the
**generated agent guide** the repository contract calls for (`Generated help,
MCP schemas, and the agent guide come from dispatch tables`).

The single behavioural authority remains `core.Do`. M8b adds **no** new verbs,
no store behaviour, and no second command implementation. It is a *projection*
of the dispatch descriptors — refined to **operation** granularity — into two
generated artifacts, plus the descriptor metadata those artifacts honestly need.

M8b delivers:

1. **Operation-aware descriptor metadata.** The dispatch table gains, per verb:
   a one-line `Summary`, a `Safety` class, an explicit `Include` flag, and —
   for verbs whose handler branches on a discriminator (`find`, `link`) — a set
   of `Operations`, each naming the discriminator value, its own `Summary`,
   `Safety`, the subset of args it uses with per-operation requiredness, and a
   canonical example. Single-shape verbs carry no `Operations` (the verb *is*
   the operation). All of this is surfaced through `DispatchDescriptors()`.

2. **An installable Skill package** emitted by a new `aira skill` face:
   - `aira skill install <dir>` writes `<dir>/SKILL.md` (agent-facing, with
     `name`/`description` frontmatter) and `<dir>/aira.skill.json` (machine
     manifest).
   - The manifest declares Skill identity, an explicit **host/entrypoint
     contract** (entrypoint = the `aira` binary; how a host discovers and
     invokes it; per-action argv template), a **deterministic version** that is
     a hash of the *generated artifact bytes* (not just the descriptor input),
     a structured **response contract** (stable codes + verdicts + the exit-code
     map, all generated from `store`), and an `actions[]` catalog with one entry
     **per operation**, each carrying its own `{summary, safety, args[], command}`.
   - Every action's catalog entry, safety, and example command is **generated
     from `DispatchDescriptors()`**; nothing per-action is authored by hand.

3. **A generated agent guide** emitted by `aira skill guide` (stdout): the same
   operation-level content as one Markdown document, a small authored preamble
   (what AIRA is + the honesty contract) followed by fully generated sections.

Both artifacts must state, from generated `store` data, that responses carry
**stable AIRA codes**, that verdicts are `pass`/`fail`/`unevaluated`, that
**`unevaluated` is not a pass and not zero**, and the exit-code mapping.

## 2. Non-goals / explicit deferrals

- **No in-process Skill runtime distinct from the CLI.** AIRA's Skill entrypoint
  *is* the `aira` binary; the Skill instructs the host to invoke `aira <verb>`,
  the already-proven CLI face over `core.Do`. M8b does not add a fourth dispatch
  path. Per Sol's P0, "invokable/installable" is **not** claimed from request
  parity alone: it is proven by an **end-to-end invocation test** that executes
  each documented argv through the real program entry (`Run`) against a temp
  project and asserts it reaches core with a sane stable code and exit — not a
  parse error — plus the request-parity test below.
- **No change to the MCP face.** M8a's union-schema grouped MCP tools stay as-is
  (they work and are tested). The new operation metadata is *added* to the shared
  table and *consumed* by the Skill; MCP may adopt it later. Both faces still
  derive from the one table, so they cannot drift.
- **No hand-authored per-action examples/guidance** beyond the single authored
  preamble. Examples are generated from the operation arg specs.
- **No new safety enforcement.** `Safety` is descriptive metadata for warnings
  and schemas; enforcement stays in store/domain.
- **No telemetry, daemon, or traceability graph.** Phase 3+.

## 3. Invariants

1. **One authority, one inclusion predicate.** The Skill action catalog and the
   guide are derived solely from the dispatch table `core.Do` uses, via
   `DispatchDescriptors()`. A **single** `Include` predicate (a descriptor field
   set on the table, not inferred from `MCPTool`) decides membership and is used
   by both the MCP and Skill coverage tests, so Skill coverage is not silently
   coupled to MCP metadata. A verb surfaced for dispatch but missing required
   Skill metadata (`Summary`, valid `Safety`, and — if grouped — `Operations`
   covering every discriminator value) is a build-failing condition.
2. **Operation-level correctness.** For grouped verbs the catalog has one action
   per operation, each with only the args that operation uses, correct per-op
   requiredness, correct per-op safety, and a valid example. No union-derived
   action is emitted for a grouped verb.
3. **Face parity, full coverage.** For **every** generated action (every
   operation, not a representative sample), the `command` it documents, parsed by
   the CLI argv builder, produces byte-identical `core.Request` to the MCP face
   for the same operation — including special encodings (`find add` uses
   `--file path:line`, there is no `--line` CLI option; `link ls` uses the
   positional `ls` form). This extends the M8a MCP↔CLI parity guarantee to
   Skill↔CLI at operation granularity.
4. **Honest, per-operation safety.** Every mutating / lease-taking / read
   operation is marked truthfully. `find ls`/`find show`/`link ls` are `read`;
   `find add`/`find set` are `mutate`. `mutate` means *any durable state write*
   (tickets, findings, relations, **allocator/outbox/event/receipt** — hence
   `id` is `mutate`, confirmed by Sol), `lease` means lease acquire/renew/release,
   `reconcile` means rebuild/heal/check. Asserted against a golden per-operation
   table.
5. **Metadata tied to behaviour (anti-drift).** Extending the M8a instrumented
   arg-accessor drift test, each operation is exercised with its discriminator
   and probe inputs; the args the handler actually **reads** for that operation
   must equal the args that operation **declares**. Operation metadata cannot
   silently diverge from handler logic.
6. **Deterministic, artifact-scoped version.** Generation uses no clock/random.
   The manifest `version` is a hash over the **canonical generated artifact
   bytes** (manifest sans the version field + SKILL.md), so it changes on any
   generator, template, preamble, manifest-schema, response-contract, or CLI-
   grammar change that alters output — not only on descriptor edits. Re-running
   `skill install` on an unchanged binary is byte-identical (idempotent).
7. **Generated, not authored, honesty text.** The stable-code list, verdict set,
   and exit-code map in both artifacts are generated from `store` (the codes/exit
   map), so they cannot drift from the implementation. A string-only authored
   claim of exit codes is not permitted.
8. **Guide-only does not satisfy the deliverable.** `skill install` must emit the
   manifest with the host/entrypoint contract and the enumerable per-operation
   action catalog, and the end-to-end invocation test must pass; prose alone
   fails.

## 4. Design

### 4.1 Descriptor metadata

`ArgSpec` is unchanged. Add a closed `SafetyClass` string type in
`internal/core` (`read`, `mutate`, `lease`, `reconcile`) with a validated set.
Add to `verbSpec` and mirror into `DispatchDescriptor`:

- `Summary string`
- `Safety SafetyClass`
- `Include bool` (the single membership predicate for generated faces)
- `Operations []OperationSpec` (empty for single-shape verbs)

```go
type OperationSpec struct {
    Name    string      // discriminator value, e.g. "add", "ls", "list"
    Summary string
    Safety  SafetyClass
    Args    []OperationArg // arg name + per-operation requiredness
    Example []ExampleArg   // canonical args for a valid example of this op
}
```

`DispatchDescriptors()` deep-copies `Operations` (like it already deep-copies
`Enum`) so the projection stays immutable. A consistency test requires every
`Include` descriptor to have non-empty `Summary` and valid `Safety`; every
grouped verb (`find`, `link`) to enumerate `Operations` covering all its
discriminator values; and every operation to have valid `Safety` + a usable
example.

**Authoritative safety table** (golden-tested; `mutate` = any durable-state
write):

| action | safety |
|---|---|
| `init` | reconcile |
| `id` | mutate |
| `create`, `set`, `mv`, `touch`, `unlink`, `import` | mutate |
| `link` (create-relation form) | mutate |
| `link ls` | read |
| `show`, `list`, `count`, `grep`, `ready` | read |
| `find add`, `find set` | mutate |
| `find ls`, `find show` | read |
| `claim`, `release`, `heartbeat` | lease |
| `reconcile`, `check` | reconcile |

### 4.2 Generator (`internal/core`, filesystem-free)

A generator takes `[]DispatchDescriptor` and produces, deterministically:

- the ordered per-operation **action list** (single-shape verb → one action;
  grouped verb → one action per `OperationSpec`), de-aliased (`new`/`get`/`ls`
  never duplicate an action; `help` excluded);
- for each action, the arg contract and a **valid example** built as canonical
  request args, then **rendered to CLI argv via a shared renderer** so the
  documented `command` is exactly what the CLI parses (guaranteeing invariant 3);
- the response-contract section from `store` (stable codes + exit map + verdicts
  + the `unevaluated` statements);
- the manifest struct and the SKILL.md / guide Markdown.

The example renderer and the CLI parser are inverse projections of the same arg
specs; the parity test (invariant 3) proves round-trip identity for every action.

### 4.3 The `skill` face (`cmd/aira/skill.go`)

Dispatched from `Run` exactly like `mcp`:

```
if strings.ToLower(argv[0]) == "skill" { return runSkill(argv[1:], stdout, stderr) }
```

- `skill guide` → guide Markdown to stdout, exit 0.
- `skill install <dir> [--force]` → create `<dir>` if absent; write `SKILL.md` +
  `aira.skill.json`; print written paths + version; exit 0. Refuse with a stable
  error + non-zero exit if `<dir>` is a non-directory, or a target file exists
  and differs without `--force` (idempotent identical overwrite allowed).
- unknown/missing subcommand → usage to stderr, non-zero exit.

The face is a thin adapter: it calls the generator and writes bytes; no dispatch
logic lives in it.

### 4.4 Manifest (`aira.skill.json`)

```
{
  "name": "aira",
  "version": "<hash of canonical generated artifact bytes>",
  "entrypoint": { "type": "cli", "command": "aira", "invocation": "aira <verb> [args]" },
  "discovery": { "skill_file": "SKILL.md", "install_target": "<dir>" },
  "response_contract": { "stable_codes": [...], "verdicts": ["pass","fail","unevaluated"],
                         "unevaluated_is_pass": false, "exit_codes": { "OK":0, ... } },
  "actions": [ { "verb": "...", "operation": "...", "summary": "...", "safety": "...",
                 "command": "aira ...", "args": [ {name,kind,required,positional,enum} ] } ]
}
```

`response_contract` fields are generated from `store`.

## 5. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Grouped-verb example invalid (union args) | Per-operation actions with per-op arg subsets + requiredness; example rendered through the shared CLI renderer; per-operation parity test. |
| Per-operation safety lies (`find ls` marked mutate) | Per-operation `Safety`; golden per-operation safety table (invariant 4). |
| Operation metadata drifts from handler | Per-operation instrumented arg-accessor drift test: handler reads for an operation == that operation's declared args (invariant 5). |
| Generated text drifts from dispatch table | Golden action-set == `Include` set; per-action arg/summary goldens; consistency test fails on missing metadata. |
| "Includable" silently coupled to MCP | Explicit `Include` field; one shared predicate for both faces (invariant 1). |
| Version churns / misses generator changes | Version = hash of generated artifact bytes; idempotency golden (invariant 6). |
| Exit/stable-code prose drifts from `store` | Generated from `store`; not authored (invariant 7). |
| "Installable/invokable" unproven | End-to-end test runs each documented argv through `Run` against a temp project; asserts stable code + sane exit, not a parse error (invariant 8). |
| Guide-only masquerades as deliverable | Manifest presence + host/entrypoint contract + full enumeration asserted; prose alone fails. |

## 6. Tests (TDD; every confirmed counterexample becomes a regression test)

1. **Descriptor consistency (fail-closed).** Every `Include` descriptor has a
   non-empty `Summary` + valid `Safety`; grouped verbs enumerate `Operations`
   covering all discriminator values; each op has valid `Safety` + a usable
   example. Injected empty value / missing operation fails. (Proven load-bearing.)
2. **Per-operation safety golden.** The action→`Safety` map equals §4.1 exactly;
   any add/remove/reclassify fails until the table is updated.
3. **Action-set drift.** Generated Skill action set and guide section set each
   equal the operation-expanded `Include` set (de-aliased, `help` excluded).
4. **Skill↔CLI parity, full coverage.** For **every** action, the generated
   `command` parsed by the CLI argv builder yields byte-identical `core.Request`
   to the MCP path — including `find add`'s `--file path:line` and `link ls`.
5. **Per-operation drift (behaviour-tied).** Extend the M8a instrumented
   arg-accessor test: per operation, handler-read args == declared op args.
6. **Manifest artifact + host contract.** `skill install` writes an
   `aira.skill.json` that parses, declares the entrypoint + discovery contract,
   carries generated `response_contract`, and enumerates every action with
   args+safety+command; a guide-only variant (no manifest) fails.
7. **End-to-end invocation.** For representative documented actions across
   read/mutate/lease/reconcile, execute the generated argv through `Run` against
   an isolated temp project (own `XDG_STATE_HOME`); assert it reaches core with a
   stable code + sane exit (not `E_SELECTOR_INVALID`/parse error).
8. **Determinism / idempotency.** Two installs into the same dir on the same
   binary are byte-identical with equal versions; a metadata/generator change
   changes the version and the diff is the expected one.
9. **Generated honesty content.** Both artifacts contain the stable-code list,
   verdict set, exit map, and the explicit "`unevaluated` is not a pass / not
   zero" statement, generated from `store`; removing the source data or the
   statement fails.
10. **Face wiring / exits.** `skill guide` exits 0 with Markdown; `skill install
    <newdir>` exits 0 writing both files; unknown subcommand and install into a
    non-directory yield a stable error + non-zero exit; identical re-install
    without `--force` succeeds, a differing target without `--force` fails.
11. **Build constraint.** The new face compiles into the static/no-cgo binary;
    generation needs no daemon, network, or external process.

## 7. Expected yield

Agents gain a discoverable, installable Skill whose per-operation action catalog,
argument contract, and safety warnings are provably truthful to the operations
that exist, provably build the same core requests as the CLI and MCP faces, and
provably *invoke* successfully end-to-end — plus a generated agent guide that
cannot silently diverge from the implementation or lie about exit/stable codes.
This closes the third face named in the whole-product design and completes
Phase 2 (findings + surfaces + import + all three thin faces over one core).

## 8. Out-of-scope confirmation

M8b does not add runner, telemetry, gates, traceability enforcement, MCP-schema
changes, or any new store behaviour. It does not hand-author per-action guidance,
add a second dispatch path, or enforce safety at execution time. Those remain
Phase 3+ and the store/domain layers.
