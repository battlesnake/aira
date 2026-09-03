# AIRA M10b — command-backed gates and mutation canaries

Status: **DRAFT** (design; drafted by Luna, Opus-reviewed; NOT yet Sol-plan-reviewed
or plan-gated). To be reconciled against M10a's actual audit/verdict/`EvaluationRoot`
API once M10a lands, then plan-reviewed before build.
Date: 2026-08-11
Prerequisites: M10a gate honesty engine (must land first); merged M12 runner-lite
Scope: command-backed checkable gates, `tests-green`, and mutation canaries

> Coordinator note: this bank-ahead draft assumes M10a's engine as an opaque, stable
> dependency. Two additive M12 runner capabilities it depends on (an explicit-empty
> environment mode, and an `OnScopeReady` launch callback for gate-owned timeouts)
> are flagged in §3.4/§4.2 as M10b build concerns — neither alters M12's cgroup,
> capture, ledger, kill, or terminal-state semantics.

## 1. Scope

M10b adds command execution to the M10a gate engine.

It adds:

1. A `command` checker whose declared argv is executed only through the M12 runner.
2. A `tests-green` predicate over one command run.
3. A `mutation` canary mode that proves the command gate can detect a known mutation.
4. The command-lane data model, runner seam, stable codes, and descriptor parity coverage required by those features.

M10a remains the authority for:

- gate content parsing and validation;
- `EvaluationRoot` and escape guards;
- the common-directory authenticated audit ledger;
- verdict folding;
- proof-of-fire records and freshness;
- canary cadence and proof scope;
- `check` and `ready` folding;
- existing `aira gate` operations and response contracts.

M10b does not modify those mechanisms.

All gate verdicts remain exactly one of `pass`, `fail`, or `unevaluated`. No command result may infer a pass from missing output, zero tests, a missing run, or an unavailable parser.

## 2. Non-goals and deferrals

M10b does not add:

- ratchet gates or baseline storage;
- `TestReport` archives or report comparison;
- flaky-test classification;
- coverage predicates;
- non-Go test-output parsers;
- command composition or gate dependencies;
- detached or daemon-owned execution;
- shell interpretation, pipes, redirects, globbing, or implicit command wrappers;
- a new grouped CLI/MCP verb;
- automatic execution from `aira check` or `aira ready`;
- write/merge enforcement.

Ratchet, archive comparison, flaky classification, and additional test formats remain Phase 4 work.

`aira check` remains strictly read-only. Only `gate run` evaluates a command.

## 3. Data model additions

### 3.1 Gate schema

M10b increments the gate schema version from the M10a version to version 2.

The versioned decoder must accept command-backed fields only in version 2. An unknown or unsupported schema version is rejected with the existing M10a definition-invalid code. M10a gate definitions remain valid.

A checkable gate gains:

- `checker: command`;
- a required command predicate;
- a required command lane;
- one or more existing canary references.

The command checker has exactly these predicates:

- `exit-zero`;
- `tests-green`.

There is no implicit default. A command gate must declare its predicate explicitly.

### 3.2 Command lane

The committed lane configuration contains:

| Field | Contract |
|---|---|
| `argv` | Non-empty argv array; tokens are passed verbatim to the runner. |
| `cwd` | Either `root` or a relative declared subdirectory. |
| `env_allow` | Sorted, unique environment-variable names allowed into the child. |
| `timeout` | Positive duration owned by the gate evaluator. |
| `output_cap_bytes` | Positive total byte cap for captured stdout plus stderr. |
| `parser` | Required for `tests-green`; absent for `exit-zero`. |
| `merge` | Always false for M10b command lanes; stdout and stderr remain separate. |
| `stdin` | Closed/null input. Command gates do not accept interactive input. |

The lane configuration is committed gate policy, not a caller-supplied runtime override.

The argv is never passed through a shell. A token such as `&&`, `|`, `>`, or `*` has no shell meaning.

A relative executable name is permitted only when `PATH` is included in `env_allow`. Otherwise the first argv token must be an absolute executable path. This prevents command lookup from depending on an unrecorded environment value.

### 3.3 Working-directory policy

`cwd: root` means the root of the command gate's materialized `EvaluationRoot`.

`cwd: subdir` is resolved relative to that root. The subdirectory must:

- be non-empty;
- be relative;
- contain no `..` component;
- contain no absolute-path syntax;
- resolve inside the evaluation root;
- not escape through a symlink.

The caller's worktree is never used as the command's mutable working directory. The subject tree is materialized into an isolated evaluation boundary. The tree digest remains the identity of the evaluated subject; the temporary materialization may be writable by the command.

### 3.4 Environment policy

The command process receives only variables named by `env_allow`.

For each allowed name, M10b reads the caller's current value at evaluation time. An unset allowed variable is omitted. Every other inherited variable is stripped.

Names must match the platform environment-name grammar, be unique, and be stored canonically sorted. Environment values are never written to gate records.

The effective environment digest is the M12 length-prefixed SHA-256 digest of the exact `KEY=VALUE` set passed to the child. It is recorded in:

- the M12 `RunRecord.EnvDigest`;
- the M10b command result;
- the lane identity used by proof-of-fire.

Changing an allowed environment value changes the lane identity and invalidates an older proof.

M12 currently preserves the inherited environment by default. M10b therefore requires one additive runner request capability: an explicit-environment mode that starts from an empty environment and applies the supplied `KEY=VALUE` entries. Existing M12 callers retain the current inherited-environment behavior. This is an environment-selection addition only; it does not alter cgroup, capture, ledger, kill, or terminal-state semantics.

### 3.5 Lane identity

The canonical lane identity includes:

- exact argv;
- cwd policy;
- resolved cwd relative to the evaluation root;
- sorted environment allow-list;
- effective environment digest;
- timeout;
- output cap;
- parser and predicate identifiers;
- runner API/version identifier.

The lane identity is included in the M10a proof scope through the existing lane/config digests. The subject tree digest and mutation declaration digest remain separate proof-scope components.

### 3.6 Output cap

The default output cap is 8 MiB. A committed lane may configure another value from 1 KiB through 64 MiB.

The cap is the combined byte count of stdout and stderr. M10b uses separate runner streams so the parser can consume stdout without mixing diagnostics from stderr.

M12 remains responsible for faithfully capturing raw bytes to files. M10b reads the completed capture through the runner's output API and checks the recorded byte counts. If the combined count exceeds the cap:

- the output is not parsed;
- the command result is `unevaluated`;
- the result includes `U_GATE_OUTPUT_OVERFLOW`;
- a prior pass or prior proof cannot rescue the current evaluation.

M10b does not silently truncate output and then parse the prefix as successful evidence. The runner may have captured more than the cap because M12 has no built-in per-run capture limit; the cap is an evaluation-evidence limit. Disk-full and incomplete-capture conditions remain visible through M12 error codes and also produce an unevaluated gate result.

### 3.7 Parser identifier

M10b initially supports exactly one parser:

`go-test-json-v1`

This is the output produced by Go's `go test -json` command. The parser identifier is part of the lane identity and proof scope.

No parser is implied by `exit-zero`. A `tests-green` lane must explicitly use `go-test-json-v1`.

## 4. Evaluation protocol

### 4.1 General command evaluation

`gate run` performs these steps:

1. Load and validate the M10a gate and canary declarations.
2. Resolve the subject tree and create the subject `EvaluationRoot`.
3. Resolve the committed command lane.
4. Resolve the allow-listed environment and compute its digest.
5. Validate the cwd, argv, timeout, cap, and parser configuration.
6. Launch the command through the M12 runner.
7. Apply the gate-owned timeout.
8. Await the terminal M12 `RunRecord`.
9. Inspect captured output and evaluate the selected predicate.
10. Run the declared canary using the same command lane.
11. Reuse the M10a verdict fold.
12. Append the existing authenticated result, proof, and projection records as required by M10a.

The evaluator must not call `os/exec`, `syscall.StartProcess`, a shell, a process-group kill, or a direct PID kill.

### 4.2 Runner seam and timeout

M12 is foreground and blocking: `runner.Launch` returns after the run reaches a terminal state. M10b owns the timeout outside the runner.

The runner needs one narrow additive callback seam. The command evaluator supplies an `OnScopeReady` callback to `runner.Launch`. The runner invokes it after the durable scope-created record exists and before the child is launched. The callback receives the run ID and actual scope reference. No command output or environment value is exposed through the callback.

M10b then:

1. Starts a timer for the committed timeout.
2. Calls `runner.Launch` in the foreground evaluation goroutine.
3. If the timer fires first, calls `runner.Kill` with the run ID.
4. Waits for `Launch` to return and uses the final durable `RunRecord`.

The timeout path always uses `runner.Kill`, which terminates the entire M12 cgroup scope. It never calls `Process.Kill` or kills only the launch PID.

M12's kill-versus-exit arbitration remains authoritative:

- if a durable wait result was published before kill intent, the established exit result wins;
- otherwise the kill must complete and prove an empty scope;
- an unproven kill is unevaluated.

If the timeout wins, the gate result is `unevaluated` with `U_GATE_COMMAND_TIMEOUT`, even if the child later returns zero. A nonzero child exit before the timeout is an established command failure and may produce a gate `fail`.

A command timeout is not a predicate failure. It means the evaluator did not establish the predicate.

### 4.3 Runner result admissibility

For `exit-zero`, a command run is admissible only when:

- the M12 status is `exited`;
- `exit_code` is present and equals zero;
- `ScopeIntegrity` is `contained`;
- `CaptureComplete` is true;
- `TerminalComplete` is true;
- there are no M12 capture, scope, handoff, migration, or reconciliation errors.

Any missing, partial, killed, lost, unavailable, or integrity-compromised run is unevaluated.

For an admissible run with a nonzero exit code, the command predicate is `fail` with `E_GATE_COMMAND_FAILED`. The exact child exit code remains in the run evidence.

Exit zero is necessary for `tests-green` but is not sufficient.

### 4.4 `tests-green` predicate

`tests-green` is evaluated over exactly one command run. It does not compare against a baseline or an archive.

The predicate is:

`exit-zero AND parser-complete AND discovered-test-count > 0`

Evaluation order is:

1. Any timeout, incomplete capture, output overflow, unavailable output, lost run, or runner integrity error produces `unevaluated`.
2. An incomplete parser produces `unevaluated`.
3. A complete parser with a nonzero exit produces `fail`.
4. A complete parser with exit zero and zero discovered tests produces `unevaluated`.
5. A complete parser with exit zero and at least one discovered test produces `pass`, subject to the existing M10a proof requirement.

Thus:

| Run state | `tests-green` verdict |
|---|---|
| Exit zero, valid output, one or more discovered tests | Predicate pass |
| Exit zero, valid output, zero discovered tests | Unevaluated |
| Nonzero exit, parser complete | Fail |
| Timeout | Unevaluated |
| Output overflow | Unevaluated |
| Capture unavailable or incomplete | Unevaluated |
| Malformed or incomplete JSON | Unevaluated |
| No terminal run result | Unevaluated |

A predicate result of pass is still not a gate pass until the M10a proof-of-fire fold succeeds.

### 4.5 `go-test-json-v1` parser contract

The parser consumes stdout as raw bytes from the runner. It requires:

- valid UTF-8;
- one JSON object per line;
- no blank or non-JSON lines;
- no trailing partial line;
- only the supported Go test event fields and event actions.

Supported event actions are `start`, `run`, `pause`, `cont`, `output`, `bench`, `pass`, `fail`, and `skip`. Unknown actions or malformed objects make the parser incomplete.

A discovered test is a unique `(package, test)` pair from an event with `Action == "run"` and a non-empty `Test` field. Subtests count as distinct discovered tests when their complete Go test name differs.

Parser completeness requires:

- every byte belongs to a complete line;
- every line parses as a supported event object;
- required fields have valid types;
- at least one package terminal event (`pass`, `fail`, or `skip`) is observed for each package represented in the stream;
- the runner reports complete stdout capture and EOF.

A package-level `fail` event is parser-complete even if no test was discovered; the nonzero process exit then makes `tests-green` fail. A package that completes successfully without any `run` event produces count zero and therefore `unevaluated`.

The parser does not inspect stderr for test events. Stderr remains evidence and contributes to the total output cap.

### 4.6 Mutation canary evaluation

M10b adds `mutation` to the M10a canary mode enum.

A mutation canary:

1. Resolves the same subject tree used by the command gate.
2. Materializes a separate isolated evaluation root.
3. Applies the declared typed mutation to that copy.
4. Runs the exact same command lane, including argv, cwd policy, environment policy, timeout, cap, parser, and predicate.
5. Requires the resulting predicate to be an established `fail`.
6. Records proof-of-fire using the existing M10a proof protocol.

The mutation command is a separate run from the subject command. `tests-green` remains a predicate over one run; M10b does not combine output from the subject and canary runs.

A mutation that does not make the predicate fail is `E_GATE_CANARY_DID_NOT_FIRE`. This is an established canary failure and folds to gate `fail`, never to a warning or pass.

A mutation that cannot be applied, times out, overflows output, or produces incomplete parser evidence is `unevaluated` and cannot create proof-of-fire.

### 4.7 Typed mutation seeds

Mutation declarations use a closed, versioned tagged union. They cannot contain arbitrary patches, source blobs, shell commands, or opaque JSON.

M10b initially supports:

| Mutation kind | Required typed fields | Generated effect |
|---|---|---|
| `go-negate-assertion` | Relative file path, exact test function name, assertion occurrence number | Negates the selected boolean assertion using a fixed AST transformation. |
| `go-inject-failing-test` | Relative package directory, reviewed test name | Adds a fixed-template test whose assertion is always false. |
| `inject-file` | Relative target path, literal file body (bounded, UTF-8) | Creates the named file with the declared body. Create-only: an existing target is refused, never overwritten. |

Both `go-` kinds drive `go/parser` over `.go` files. Until `inject-file` existed they were the whole union, so a command gate in any other toolchain could never reach a canary-proven pass: the required canary could not fire, and `E_GATE_CANARY_DID_NOT_FIRE` is a hard fail rather than `unevaluated`. `inject-file` is the language-agnostic kind that closes that gap (AIRA-55). Its body is literal file bytes written verbatim into the isolated snapshot — never a patch, script, or command the evaluator interprets — so the union stays closed and this section's no-executable-content rule holds.

`inject-file` yields a weaker proof than `go-inject-failing-test`, and a declaration must be authored knowing it. `go-inject-failing-test` injects a compiling, failing test, so its fire proves the whole test-failure-to-nonzero-exit pathway; `inject-file` proves only that the declared perturbation produces a nonzero exit. A declaration should therefore inject a compiling, failing test in the subject's own language: a body that merely breaks the build proves only that the build breaks. The honest-mistake false pass this admits is a `make test` recipe that aborts on a compile error but swallows real test failures — it fires on a syntax-broken injection and would never fire on a failing test, so the lane earns a trusted pass it cannot back. This is a documented, accepted limitation of the kind, not a defect in the fold.

A target path matched by the subject's git excludes (its own `.gitignore`, or the user's `core.excludesFile`) never reaches the checker's tracked-snapshot view and so produces `E_GATE_CANARY_DID_NOT_FIRE` rather than a pass.

The seed also contains:

- `schema_version`;
- a deterministic numeric seed;
- the target path and selector;
- the operation-specific fields;
- an expected mutation description.

The numeric seed is an identity and reproducibility input. It is not executable content. Mutation application is performed by a fixed M10b transformer, not by interpreting seed text.

A mutation declaration is invalid when its kind, target, selector, path, or operation is not supported. Structural invalidity uses the existing `E_GATE_CANARY_INVALID` catalog entry. A runtime application failure uses `U_GATE_MUTATION_APPLY_FAILED`.

The canonical mutation declaration digest includes the complete typed declaration and the resulting mutated-tree digest. It is included in the existing M10a proof scope.

### 4.8 Isolation and unchanged caller tree

The caller worktree is never mutated.

The mutation copy must be created outside the caller worktree and outside the source tree's `.git` directory. M10a's `EvaluationRoot` escape guards apply unchanged:

- reject symlink escapes;
- reject `.git` traversal;
- reject absolute paths;
- reject parent-directory traversal;
- verify every materialized path remains below the evaluation root.

The evaluator records the source worktree digest before evaluation and verifies it after evaluation. Tests must additionally compare a byte-for-byte snapshot, including file contents, modes, symlink targets, and directory entries.

The mutation canary is valid only when the command actually runs in the mutated root. A distinct-root sentinel test is mandatory.

## 5. Verdicts and stable codes

### 5.1 Existing M10a fold

M10b passes predicate results, canary health, proof state, and evidence availability into the unchanged M10a verdict fold.

M10b must not set `trusted`, `proofState`, or `canaryHealth` directly. Those values remain derived by the M10a engine.

A command predicate pass without current proof remains a non-pass result according to the existing M10a codes and fold.

### 5.2 M10b-specific codes

The following additions are registered in the same stable catalog and response contract:

| Code | Meaning | Exit class |
|---|---|---:|
| `E_GATE_COMMAND_FAILED` | An admissible command run established a nonzero exit and the command predicate failed. | 1 |
| `U_GATE_COMMAND_TIMEOUT` | The gate-owned timer expired before an admissible result was established. | 3 |
| `U_GATE_OUTPUT_OVERFLOW` | Captured stdout plus stderr exceeded the committed evidence cap. | 3 |
| `U_GATE_PARSER_INCOMPLETE` | The selected parser could not establish a complete supported output stream. | 3 |
| `U_GATE_COMMAND_RUN_UNEVALUATED` | The runner did not establish admissible terminal evidence. | 3 |
| `U_GATE_MUTATION_APPLY_FAILED` | A valid mutation declaration could not be applied to the isolated copy. | 3 |

Existing M10a codes retain their meanings, including:

- `E_GATE_CANARY_DID_NOT_FIRE`;
- `U_GATE_CANARY_UNEVALUATED`;
- `U_GATE_PROOF_STALE`;
- `U_GATE_NO_RESULT`;
- `U_GATE_EVIDENCE_UNAVAILABLE`;
- definition, checker, lane, and canary-invalid codes;
- journal and authentication integrity codes.

Runner-specific codes such as `E_RUN_OUTPUT_DISK_FULL`, `E_RUN_SCOPE_HANDOFF`, and `U_RUN_RECONCILE_REQUIRED` are retained as structured detail on the gate evidence. They must not be discarded or converted to a pass.

All `U_GATE_*` outcomes use the existing unevaluated exit class. None may be mapped to exit zero, warning-only status, or a successful gate verdict.

## 6. Faces

M10b adds no grouped verb.

The existing `aira gate` descriptors are extended in place.

### 6.1 `gate add` and `gate set`

The descriptors must expose:

- `checker=command`;
- `predicate=exit-zero|tests-green`;
- argv tokens;
- cwd policy;
- environment allow-list;
- timeout;
- output cap;
- parser selection;
- mutation-canary fields.

All fields are validated identically through CLI, MCP, and core dispatch.

A command gate cannot be created without at least one canary reference. A `tests-green` gate cannot be created without `parser=go-test-json-v1`.

### 6.2 Existing evaluation operations

`gate run` evaluates the command and its canary through the M10a protocol.

`gate canary-run` runs the declared mutation canary using the same lane and returns its established predicate result, proof status, and runner evidence.

`gate canary-show` renders the typed mutation declaration and canonical identity. It does not execute it.

`gate check`, `aira check`, and `aira ready` remain read-only and never launch the command.

### 6.3 Examples and parity

Descriptors and generated guides must include examples for:

- a command gate using `exit-zero`;
- a `tests-green` gate using `go test -json`;
- a mutation canary that injects a known-failing test;
- timeout and output-cap fields;
- an explicit environment allow-list.

Parity tests must verify every M10b field through:

- CLI parsing;
- core dispatch;
- MCP operation arguments;
- response rendering;
- generated Skill actions;
- generated guide examples.

No new `aira tests-green` verb or alias is introduced.

## 7. Invariants

1. Every command executes through the M12 runner.
2. No command gate calls bare `os/exec`, a shell, a process-group kill, or a direct PID kill.
3. Every command run has a declared argv, cwd policy, environment policy, timeout, and output cap.
4. Cwd is the evaluation root or a validated descendant; arbitrary cwd values are rejected.
5. The child environment contains only explicitly allow-listed variables.
6. The effective environment digest is recorded in the run and lane identity.
7. Timeout is gate-owned and uses `Launch`, a timer, and whole-scope `Kill`.
8. A timeout never becomes a command failure or pass; it is unevaluated.
9. Output overflow never becomes a truncated successful parse; it is unevaluated.
10. Parser-incomplete output never becomes a pass.
11. `tests-green` requires exit zero, parser completeness, and discovered-test-count greater than zero.
12. A successful process with zero discovered tests is unevaluated.
13. `tests-green` evaluates exactly one command run.
14. A command predicate pass cannot produce a gate pass without a current M10a proof-of-fire.
15. Mutation canaries run the same lane against an isolated mutated root.
16. A mutation that does not make the predicate fail produces `E_GATE_CANARY_DID_NOT_FIRE`.
17. A mutation application failure cannot create proof.
18. The caller worktree remains byte-for-byte unchanged.
19. Proof scope includes command lane, environment digest, parser/predicate configuration, mutation declaration, and tree digest.
20. M10a audit, proof, fold, and canary semantics remain unchanged.
21. `check` and `ready` never execute commands or repair missing evidence.
22. Missing, stale, partial, killed, lost, or unavailable runner evidence never becomes pass.
23. Runner errors remain visible as structured evidence.
24. No M10b path creates a fake zero, empty output, or implicit success.

## 8. Tests

Tests are TDD additions to the M10a matrix. Every confirmed counterexample becomes a regression test.

### 8.1 Content and descriptor validation

1. A version-2 command gate round-trips with canonical field ordering.
2. A version-1 M10a gate remains readable.
3. Unknown command predicates are rejected.
4. Empty argv, empty timeout, zero output cap, invalid parser, and invalid environment names are rejected.
5. Absolute cwd, `..`, symlink escape, and missing subdirectory are rejected.
6. A relative executable without `PATH` in the allow-list is rejected.
7. Duplicate and unsorted environment names are normalized or rejected according to the M10a canonicalization rule.
8. `tests-green` without `go-test-json-v1` is rejected.
9. A mutation with an unknown kind, arbitrary patch payload, invalid target, or unsupported selector is rejected.
10. A mutation canary with no command gate or no canary reference is rejected.

### 8.2 Environment and lane identity

11. An allowed variable reaches the child with its exact value.
12. An unlisted inherited variable is absent.
13. An unset allowed variable is absent rather than fabricated as an empty value.
14. Environment values containing newline, equals, or binary test bytes produce distinct digests.
15. Changing an allowed environment value changes the lane identity.
16. Changing only the allow-list changes the lane/config digest.
17. The run record contains the environment digest but no environment values.
18. Equivalent CLI and MCP lane declarations produce identical canonical lane identities.

### 8.3 Runner integration

19. The command evaluator invokes the runner and never the process package directly.
20. Exact argv tokens, including shell metacharacters and `--`-prefixed arguments, reach the child unchanged.
21. The child runs in the materialized evaluation root or declared subdirectory.
22. The `OnScopeReady` callback receives the durable run ID before the child can outlive the timeout setup.
23. A timeout calls `runner.Kill` with the run ID.
24. A timeout kills a setsid grandchild in the same cgroup.
25. A timeout result is `U_GATE_COMMAND_TIMEOUT`, not fail or pass.
26. A kill-versus-exit race follows M12's durable arbitration.
27. An unproven kill produces unevaluated evidence.
28. A missing cgroup capability prevents execution and never creates a gate pass.
29. A nonzero child exit produces `E_GATE_COMMAND_FAILED` with the exact child exit code retained.
30. A killed or lost runner record never satisfies `exit-zero`.

### 8.4 Capture and cap

31. Binary stdout and stderr are captured without decoding or normalization.
32. The combined stdout/stderr byte count exactly matches the runner records.
33. Output exactly at the cap is accepted.
34. Output one byte over the cap produces `U_GATE_OUTPUT_OVERFLOW`.
35. Overflow in stderr is detected even when stdout is valid JSON.
36. Disk-full output produces an unevaluated gate result with the runner error preserved.
37. Evicted or unavailable output produces unevaluated, never an empty successful parse.
38. The parser is never run on truncated output.

### 8.5 `go-test-json-v1`

39. Valid `go test -json` output with one test and exit zero satisfies the predicate portion of `tests-green`.
40. Valid output with multiple packages counts unique package/test pairs.
41. Subtests are counted distinctly by complete test name.
42. Exit zero with package pass events but no `run` events is unevaluated.
43. Nonzero exit with complete JSON is fail.
44. Malformed JSON is `U_GATE_PARSER_INCOMPLETE`.
45. A blank line, non-JSON banner, unknown action, invalid field type, or partial final line is parser-incomplete.
46. Missing package terminal events are parser-incomplete.
47. Valid stdout plus arbitrary stderr diagnostics remains parseable, subject to the total cap.
48. Parser output from two runs cannot be combined to manufacture a count greater than zero.

### 8.6 Mutation canaries

49. `go-negate-assertion` changes only the selected assertion in the isolated copy.
50. `go-inject-failing-test` uses the fixed failing-test template.
50a. `inject-file` creates only its declared target in the isolated copy, creates any missing parent directory, and refuses an existing target rather than overwriting it, so the mutation is provably additive.
50b. An `inject-file` target the subject's git excludes match never reaches the checker and produces `E_GATE_CANARY_DID_NOT_FIRE`.
51. The same command argv, cwd, environment, timeout, cap, parser, and predicate are used for the mutation run.
52. A mutation causing nonzero exit fires an `exit-zero` canary.
53. A mutation causing `tests-green` to fail fires a `tests-green` canary.
54. A mutation that leaves the predicate passing produces `E_GATE_CANARY_DID_NOT_FIRE`.
55. A mutation timeout, overflow, parser failure, or runner loss produces `U_GATE_CANARY_UNEVALUATED`.
56. Mutation application failure produces `U_GATE_MUTATION_APPLY_FAILED`.
57. Mutation declaration and mutated-tree digests enter proof scope.
58. Editing a mutation declaration invalidates prior proof.
59. A proof from one mutation cannot satisfy another gate or subject tree.
60. Distinct caller and mutation-root sentinels prove the command ran in the mutation root.
61. The caller tree is byte-for-byte unchanged after successful, failed, timed-out, and malformed mutations.
62. Symlink, `.git`, and parent-directory escapes are rejected.

### 8.7 Proof, fold, and integration

63. A command predicate pass without current proof does not yield gate pass.
64. A current mutation proof allows the normal command result to pass through the unchanged M10a fold.
65. A canary non-fire causes gate fail even when the subject command passed.
66. A canary unevaluated result prevents a pass.
67. A changed lane, environment digest, parser, cap, timeout, command, or tree digest invalidates older proof.
68. `aira check` performs no command launch, timer, kill, output read, projection write, or key creation.
69. `ready` treats command fail and unevaluated according to M10a `advisory_in_ready`.
70. Stable codes and exit classes match across CLI, MCP, Skill, guide, and core responses.
71. Every descriptor example reaches the intended core operation without argument drift.
72. Static and Linux builds remain valid with the runner seam enabled.

## 9. Risks and mitigations

| Risk | Mitigation |
|---|---|
| A command mutates the source tree | Run only in an isolated `EvaluationRoot`; verify caller bytes before and after. |
| A symlink or path traversal escapes the root | Reuse M10a escape guards and reject unsafe materialization. |
| Blocking M12 launch prevents timeout control | Add the narrow `OnScopeReady` seam and use the existing whole-scope `Kill`. |
| The runner captures more than the evidence cap | Inspect recorded byte counts and return `U_GATE_OUTPUT_OVERFLOW`; never parse a truncated prefix. |
| Go output format changes | Keep `go-test-json-v1` strict and versioned; unsupported output is unevaluated. |
| A command exits zero without testing anything | Require parser completeness and discovered-test-count greater than zero for `tests-green`. |
| Environment drift invalidates evidence | Record the effective environment digest in the lane identity and proof scope. |
| Mutation appears to run but the real tree was used | Use distinct-root sentinels and bind proof to the mutation-tree digest. |
| Runner ledger/output retention removes evidence | Preserve M12 evidence states and map unavailable output to unevaluated. |
| A canary is weakened without changing its ID | Include declaration, seed, and resolved tree digests in existing M10a proof scope. |

## 10. Accepted deviations and open decisions

### 10.1 Accepted deviations

1. M10b initially supports only Go `test2json`/`go test -json` output.
2. The output cap is an evidence cap checked after M12 capture, not a streaming child-output limit. A future runner milestone may add an enforced capture limit.
3. Allowed environment variables use their current caller values. Fixed environment values and secret-provider integration are deferred.
4. `tests-green` does not import or compare a `TestReport`; it is strictly a single-run predicate.
5. Flaky classification and contradictory observations are deferred to Phase 4.

### 10.2 Open decisions

1. Whether a future runner API should replace `OnScopeReady` with a first-class asynchronous run handle. M10b uses the callback because it preserves the existing foreground `Launch` contract.
2. Whether Phase 4 should add additional versioned parsers beside `go-test-json-v1`.
3. Whether a future runner should enforce the output cap during capture to reduce disk pressure, while preserving the same unevaluated overflow semantics.
4. Whether environment allow-lists should later support reviewed fixed values in addition to inherited allow-listed values.
