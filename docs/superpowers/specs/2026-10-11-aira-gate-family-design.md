# AIRA gate family: bookkeeping-free subject, named stale bindings, canary-run report, run output

Status: PLAN v1 (2026-10-11), awaiting plan review. Batch of four ci-build reports in one
change (owner rule: batch small independent fixes, one review and test cycle). Tickets, in
priority order: AIRA-285, AIRA-286, AIRA-288, AIRA-287. Base: master `44ab844`.

Requested by the peer session ci-build (repository `~/claude/tooling`, 116 tracked tickets,
one command gate `clops-gate` with a mutation canary and `require_current_canary`). Their
flow: `aira gate run clops-gate`, then `aira mv <ticket> <status>` (edits only
`.aira/tickets/*.md`), then `aira ready`, which reports every ticket unevaluated.

**Honesty-critical items: AIRA-285 (changes what a gate proof is bound to) and AIRA-286
(rewrites the stored-pass binding check).** Both get the two-loop adversarial build review
(Opus builds, Fable reviews). The governing rule for this whole plan: a stored pass must
never be served for a tree a checker could observe as different. AIRA-287 and AIRA-288 are
reporting changes that mint nothing.

## 1. Problem, in plain terms, and evidence

| Ticket | Problem | Evidence (2026-10-11, `44ab844`) | Verdict |
|---|---|---|---|
| AIRA-285 | Moving a ticket throws away every gate result. | **Measured** with a throwaway probe test (deleted, not committed): a traceability gate proven by `RunGate` with a tracked `.aira/tickets/AIRA-1.md`, then an unstaged edit to that ticket (the shape `aira mv` leaves), then `GateCheck` -> `Verdict:unevaluated Code:U_GATE_NO_RESULT`. Cause: the subject digest covers every tracked file (`subjectTreeDigest`, `internal/store/gate_subject.go:73`), and results are looked up by gate id plus subject digest (`gateProjectionRows`, `gate_index.go:169`), so any byte change to a tracked ticket orphans every result. The ticket title says `U_GATE_PROOF_STALE`; by the code a subject change can only produce `U_GATE_NO_RESULT` (a moved subject is a lookup miss, never a binding mismatch). The peer's `PROOF_STALE` is the AIRA-286 environment case below. Scale: tooling tracks 116 ticket files (76,777 bytes) of 764 files; AIRA tracks 296 tickets plus 2 findings (2,168,363 bytes) of 1,160. | Build |
| AIRA-286 | A stale proof does not say which binding went stale. | `GateCheck` (`gate_eval.go:702-809`) sets `U_GATE_PROOF_STALE` at four sites (768, 784, 790, 797), each behind a long `\|\|` of comparisons, and records no reason. **Root cause of the peer's stale proof, measured:** the env binding is checked against `os.LookupEnv` of the process that evaluates the check (`currentCommandEnvDigest`, `gate_command.go:183`, called at `gate_eval.go:788`). `gate run` runs in the caller (`routing.go:77`), but `ready`, `gate check` and `gate prove` run inside the daemon (the default route), while `aira check` runs in the caller (`routing.go:62`). The daemon's environment (`/proc/<MainPID>/environ`) has `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games:/usr/local/games:/snap/bin:/snap/bin`; the interactive shell's PATH is a different 1,486-character string; `HOME`, `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` are identical in both. So a gate that allow-lists `PATH` records a proof that `ready` reads stale forever, while `aira check` reads it fresh. The peer hit exactly this: tooling commit `5f636dc` ("clops-gate sets PATH inside the command so its env digest is stable") says "PATH differed, so every proof was U_GATE_PROOF_STALE" and removed `PATH` from `env_allow` (it was present in `2b6acce`). | Build (naming + PATH warning); root cause to the owner (section 7, D1) |
| AIRA-288 | A failing command gate shows no output. | `runCommandChecker` reads the whole stdout and stderr (`gate_command.go:114-123`) and drops them; it does set `DimensionEvaluation.RunID` (line 86), but `RunGate` neither records nor returns it (`gateResultFields`, `gate_eval.go:34`; `GateCheckResult`, `gate_index.go:18`). The output itself is already persisted per run: the runner writes every stream under `<common>/aira/runs/output` (`internal/app/project.go:307`), nothing evicts it (`runner.OutputEvicted` is declared at `types.go:60` and never assigned), and `aira run-log <RUN-id> --stream out\|err --tail N` (`core.go:2184`) already reads it. Only the run id is missing. | Build |
| AIRA-287 | `canary-run` says `verdict: fail` when the canary fired as intended. | `GateAction` "canary-run" returns `map{"verdict": evaluation.Predicate, ...}` (`gate_eval.go:468`): that is the gate's result on the canary tree, which is `fail` exactly when the canary fired. It is returned unwrapped, so the envelope says `OK` and exit 0 whatever happened, including a canary that did not fire. | Build |

## 2. Greenfield minimum, and rejected alternatives

If each problem had to be solved alone, from nothing:

- **285:** leave AIRA's own workflow bookkeeping out of the gate subject, and leave it out
  of the bytes the checker runs on by the same rule, so the digest and what a checker can
  see stay one set. One path predicate at the one capture function both share.
- **286:** when a pass is refused as stale, say which comparison failed. One name per
  comparison, collected where the comparison already happens. Plus the warning the peer
  asked for, as one pure function.
- **288:** the output is already stored; record the run id with the result and return it,
  and point at the existing `aira run-log`.
- **287:** report the canary's own outcome (fired, did not fire, could not tell) next to
  the gate result it observed, from the mapping `RunGate` already uses, and let the exit
  code follow it.

The plan below is that minimum. It adds no new verb, no new code, no new storage, and no
protocol change.

| Item | Alternative | Why rejected |
|---|---|---|
| 285 | Exclude the whole `.aira/` tree except `.aira/gates/` (the peer's suggestion) | `.aira/requirements/*.md` is read by AIRA's own traceability checker (`traceSnapshotFromSubject`, `traceability.go:111-128`). Excluded from the snapshot too, every traceability gate would read an empty registry (`U_TRACE_EMPTY`, unevaluated forever); excluded from the digest only, a requirement status change would leave a pass standing that the checker would now fail: a false pass. `.aira/config` and any future `.aira/` file default to "in", the safe direction. The peer's flow only edits tickets, so the narrower rule fully solves their report. |
| 285 | Exclude bookkeeping from the digest only, keep it in the materialised snapshot | The false-pass shape this plan exists to avoid: a command that reads a ticket file could change its verdict after a ticket edit while the digest, and so the stored pass, stays the same. The digest and the snapshot must come from one filtered capture (invariant I1). |
| 285 | A per-gate subject glob (peer: "optional, not needed") | Already rejected for AIRA-72 (`2026-09-04-aira72-gate-subject-digest-scope-plan.md` 3.4): an author-supplied narrowing of an honesty digest is an author-supplied licence to go stale. A fixed, AIRA-owned exclusion of files AIRA itself writes and no AIRA checker reads is a different thing. |
| 285 | Key results by a git object id (`HEAD` commit or `git write-tree`) instead of the working tree | `write-tree` was rejected for AIRA-72 (3.1): the index honours assume-unchanged and skip-worktree, so a real edit can be invisible to it. A commit id also misses every uncommitted code edit. Both would include ticket commits anyway. |
| 285 | Exclude `.aira/gates/` too (their bindings are already in `definition_digest` / `declaration_digest`) | Rejected for AIRA-72 (3.5); the peer also asked to keep it. |
| 286 | Only the warning, no names | The warning covers PATH only; the next differing variable (the daemon also lacks the shell's `GOFLAGS`, `GOPATH`, etc.) would be just as silent. |
| 286 | Name only the first mismatching binding | Same code size as naming all of them at the site that tripped (each site is already one expression over all of its comparisons), and all names is strictly more useful. Precedence between sites is kept exactly as today. |
| 286 | Also name "subject" on `U_GATE_NO_RESULT` when an older result exists | Cut in the challenge pass: after 285 a `NO_RESULT` means the tracked tree really changed (or the gate never ran), which the code already says; the run id and `aira run-log` show the last run. No live need. |
| 286 | Fix the daemon-versus-caller environment now | It is a design reversal of M10b section 3.4 either way (section 7, D1); it belongs to the owner, not to a peer-report batch. The warning text below states the mechanism accurately in the meantime. |
| 288 | A new `aira gate log <gate-id>` operation (the peer said this "is enough") | Cut in the challenge pass: `aira run-log` already returns the last N bytes of a persisted run stream, from the caller, with offsets and `--grep`. A second reader of the same files is per-feature surface (routing case, CLI parse case, dispatch metadata, store method, tests) for a one-call saving. Recorded as an open question for the peer and owner; it can be added later as a thin wrapper if dogfooding shows the two-step path is friction. |
| 288 | Return the output tail inline in every `gate run` reply | Puts up to tens of KiB into every agent's context on every run, passing or not, to save one call on a failing one. |
| 288 | Return a file path | The peer said no path is needed; a path is also not readable over MCP from another host. |
| 287 | Keep exit 0 and only rename fields | The ambiguity the peer reported is partly that every outcome exits 0. Exit codes now carry the same three-state meaning as `gate run`. |

## 3. Design

### 3.1 AIRA-285: AIRA's ticket and finding files are not part of a gate subject

**Decision.** Exactly two root-relative directories are excluded from every gate subject:
`.aira/tickets/` and `.aira/findings/`. Everything else that is tracked stays in, including
`.aira/config`, `.aira/requirements/`, `.aira/gates/` (definitions and canaries) and any
`.aira/` path added later. The exclusion applies to the digest **and** to the materialised
snapshot a command runs in, because both are built from one capture.

Why these two and only these: they are written by routine workflow verbs (`aira create`,
`set`, `mv`, `link`, `claim`, `find add`, `find set`), so they change many times between
code changes; and no AIRA checker reads them (the traceability lane selects `.go` files and
`.aira/requirements/` only, `isTracePath`, `traceability.go:97`; the manual lane digests
only). Requirements are deliberately kept: the traceability verdict depends on their
status, so a requirement edit must still invalidate (an over-invalidation for command
gates, accepted, section 7 D3).

**Code.** `internal/store/gate_subject.go`:

- New, next to `subjectTreeDigest`:

  ```go
  // gateSubjectExcludedDirs are AIRA's own workflow bookkeeping (AIRA-285). They are
  // removed from the CAPTURE, so they are absent from the digest and from every tree a
  // checker evaluates alike: no checker can observe what the digest does not cover.
  var gateSubjectExcludedDirs = []string{".aira/tickets/", ".aira/findings/"}

  func excludedFromGateSubject(path string) bool // strings.HasPrefix against each entry
  ```

  The match is a root-relative prefix that includes the trailing slash, on the slash form
  `trackedSnapshotPaths` already returns. So `.aira/tickets-archive/x.md`, a tracked file
  named `.aira/tickets`, and `docs/.aira/tickets/x.md` all stay in.
- `captureSubjectEntries` (line 106): skip a path when `excludedFromGateSubject(path)`,
  before the `Lstat`. This is the single function behind `subjectTreeDigest` (GateCheck,
  manual lane, review challenge, attestation) and `captureSubject` (RunGate, mutation
  canary, fixture canary, `canary-run`), and `materializeSubject` writes only
  `subject.entries`, so one line covers every lane.
- Rewrite the doc comments of `subjectTreeDigest` ("every tracked file, full stop") and
  `captureSubjectEntries` to state the two exclusions, why they cannot reintroduce AIRA-72
  (the digest can be narrower than a checker's view only if the two are filtered
  separately, and here they are filtered once, before both), and the boundaries in 3.1.1.
- `trackedSnapshotPaths` (`gate_command.go:364`) is unchanged; it stays a lister.

#### 3.1.1 Boundaries, written down

- **Mutation and fixture canaries.** An `inject-file` mutation targeting
  `.aira/tickets/...` or `.aira/findings/...` is dropped by the re-capture of the mutated
  tree, so the canary does not fire: `E_GATE_CANARY_DID_NOT_FIRE`, a loud hard fail, never
  a pass. A fixture seed that writes there is dropped the same way. Same shape as the
  documented ignored-path case (`TestInjectFileCanaryIntoIgnoredPathDoesNotFire`).
- **A gate cannot check ticket or finding files.** A command gate whose purpose was to
  lint `.aira/tickets/` now sees none (they are not in its snapshot). `aira check`
  validates those files; a gate is not the tool for it.
- **Not hermetic, unchanged.** A command can still read the live worktree by absolute
  path (`$HOME/...`), exactly as it can already read untracked files there. That
  pre-existing boundary is not widened in kind: the bound bytes and the snapshot bytes
  stay one set.
- **A gitlink or symlink under an excluded directory** is no longer refused
  (`TestSubjectDigestGitlinkFailsClosed`'s rule applies to subject entries); it is never
  read, so nothing is claimed about it.

#### 3.1.2 Effect on results already recorded

No compatibility obligation; stated for the release notes. The digest framing is
unchanged, so the digest changes exactly when the entry set changes:

- A repository with any tracked file under `.aira/tickets/` or `.aira/findings/` (AIRA
  itself, tooling) gets a new digest for the same tree: every stored result is orphaned,
  `ready` and `gate check` report `U_GATE_NO_RESULT` until `aira gate run <id>` is run once
  per gate. Fail-closed.
- A repository with neither keeps byte-identical digests; its results stay valid.
- Version skew between a new client (`gate run`) and an old daemon (`ready`), or the
  reverse, computes different digests for the same tree, so every lookup misses
  (`U_GATE_NO_RESULT`), never a false pass. `GateCheck` runs in the daemon for `ready`, so
  the daemon must run the new binary (`aira install` restarts it since AIRA-203).

### 3.2 AIRA-286: name the stale binding; warn on `PATH`

**Vocabulary (closed, documented in the `GateCheckResult` doc comment), in this fixed
output order:** `definition`, `declaration`, `subject`, `lane`, `env`, `age`, `proof`.

`internal/store/gate_index.go`, `GateCheckResult` gains:

```go
Mismatch    []string `json:",omitempty"` // bindings that refused a stored pass; set iff Code is U_GATE_PROOF_STALE
RunID       string   `json:",omitempty"` // AIRA-288
CanaryRunID string   `json:",omitempty"` // AIRA-288
Warnings    []string `json:",omitempty"` // set by RunGate only
```

(The struct has no JSON tags today, so field names stay `GateID`-style; `omitempty` keeps
existing output unchanged when a field is empty.)

`internal/store/gate_eval.go`, `GateCheck`: **control flow, precedence and codes stay
exactly as today.** Only the two long boolean expressions are replaced by functions that
return names, and the two single-condition sites append a name:

- Site 1 (lines 760-766) becomes `mismatch := resultBindingMismatches(...)` and
  `bindingMismatch := len(mismatch) > 0`. Term to name:
  `canaryErr != nil` -> declaration; `digestErr != nil` -> declaration;
  `definitionErr != nil` -> definition; current definition digest differs -> definition;
  current declaration digest differs -> declaration; `subject_scope` differs -> subject;
  `lane` differs -> lane; `evaluator_version` differs -> lane;
  `canary_tree_digest == ""` -> proof.
- Site 2 (line 783) becomes `linkedProofMismatches(...)`, same rule. Term to name (linked
  proof-of-fire field against the result or the current state): `gate_id` -> proof;
  `canary_id` -> declaration; `definition_digest` -> definition; `declaration_digest` ->
  declaration; `canary_tree_digest` -> proof; `subject_scope` -> subject; `lane` -> lane;
  `evaluator_version` -> lane; `env_digest` (command gates) -> env.
- Site 3 (lines 787-792): `envErr != nil` or current env digest differs -> env.
- Site 4 (lines 794-798): `at` unparseable or older than `max_age_secs` -> age.
- Every site that sets `U_GATE_PROOF_STALE` sets `result.Mismatch` from its names, ordered
  by the vocabulary order and de-duplicated. `U_GATE_UNPROVEN`, `U_GATE_NO_RESULT` and a
  pass carry no `Mismatch`.
- `result.RunID`, `result.CanaryRunID` are copied from the record's `run_id` /
  `canary_run_id` fields (3.3) for every found record. They are references, not bindings,
  and play no part in the verdict.

One message helper, `gateFindingMessage(r GateCheckResult) string` in `gate_index.go`:
`"gate <id> is <verdict>"`, plus `" (does not match: <names joined by ", ">)"` when
`Mismatch` is non-empty. Used by `ready` (`relation_ready.go:634`) and by
`checkGatesReadOnly` (`gate_eval.go:868`, which today uses the bare verdict). Codes and
kinds of both findings are unchanged; the message is the only change, and the finding
de-duplication key (`appendUniqueFinding`) already includes the message.

**PATH warning.** `internal/gate/command.go`: `func (c Command) EnvAllowWarnings() []string`,
exact (case-sensitive) name match on `PATH`, returning one fixed sentence:

> env_allow contains PATH: a command gate's proof is bound to the values of its env_allow
> variables, and aira ready, aira gate check and aira gate prove compare them against the
> environment of the aira daemon process, whose PATH comes from the service manager and
> normally differs from your shell's, so the proof reads U_GATE_PROOF_STALE (does not
> match: env); give argv[0] as an absolute path and set PATH inside the command (for
> example argv /usr/bin/bash -c 'export PATH=...; exec ...') instead

Call sites: `writeGateDefinition` (`gate_write.go`, appended to the existing
`GateWriteResult.Warnings` for `add` and `set` when the written definition has a command),
and `RunGate` (`GateCheckResult.Warnings`, so a hand-authored definition, which never
passes through `add`, is warned at its first run). Prose, not a new `W_` code: the
existing gate warnings are prose and no consumer needs to branch on it.

### 3.3 AIRA-288: record and return the run ids; read output with `aira run-log`

`internal/store/gate_eval.go`:

- `RunGate`, main path: add `run_id` (from `subjectEval.RunID`) and `canary_run_id` (from
  `canaryEval.RunID`) to the result record's fields when non-empty, and set
  `GateCheckResult.RunID` / `CanaryRunID` on the returned result. The audit schema has an
  open field set (`validateGateAuditFields`, `gate_audit.go:241` checks required fields
  only), so no schema change. Neither field is added to the proof-of-fire record or to any
  binding comparison.
- `RunGate`, canary-error path (lines 202-221): `appendCanaryUnevaluated` gains the two
  run ids and records them; the returned error is wrapped as
  `fmt.Errorf("%w (subject run %s, canary run %s)", err, ...)`, naming only the ids that
  exist. The `CODE:` prefix is untouched, so `store.ErrorCode` still classifies it. This
  path returns an error, so the reply carries no data; the error text and `gate check`
  (which reads the recorded ids) are how the user finds the runs.
- `GateCheck`: see 3.2 (copies the ids from the found record).
- `canary-run`: see 3.4 (`RunID`).

Reading the output is the existing verb: `aira run-log <RUN-id> --stream err --tail 16384`
(and `--stream out`). No new operation (section 2). The `gate` verb's `run` and
`canary-run` operation summaries in `applyDispatchMetadata` (`core.go`) gain "; read its
output with aira run-log <run id>", so the generated help, MCP schema and skill text say
so.

Dimension and manual gates launch no process and record no run id; that is shown by the
absent fields, never by an empty log.

### 3.4 AIRA-287: `canary-run` reports whether the canary fired

`internal/gate/verdict.go`: `func CanaryHealthFor(p PredicateState) CanaryHealth`:
`fail` -> `CanaryPass` (fired), `pass` -> `CanaryFail` (did not fire), anything else ->
`CanaryUnevaluated`. `RunGate` lines 222-227 are replaced by it, so `RunGate` and
`canary-run` cannot disagree. (Every proof-eligible canary expects `fail`,
`ValidateCanary`, `canary.go:126`; the synthetic ratchet's `fail` is its "regressed".)

`internal/store/gate_index.go`, new type:

```go
type CanaryRunResult struct {
	CanaryID   string              `json:"canary_id"`
	GateID     string              `json:"gate_id"`
	Verdict    string              `json:"verdict"`     // the canary's own: pass = fired as intended, fail = did not fire, unevaluated
	Fired      *bool               `json:"fired"`       // true / false when established; null when not
	GateResult gate.PredicateState `json:"gate_result"` // the gate's result observed on the canary tree
	Code       string              `json:"code,omitempty"`
	RunID      string              `json:"run_id,omitempty"`
	TreeDigest string              `json:"tree_digest"`
	Findings   []CheckFinding      `json:"findings"`
}
```

`GateAction` "canary-run" (`gate_eval.go:438-471`) returns it; `Verdict` is
`CanaryHealthFor(evaluation.Predicate)` as a string, `Fired` is `&true` for pass, `&false`
for fail, `nil` otherwise. A `runCanary` error is still returned as an error (unchanged).
`canary-run` still writes nothing to the audit ledger and mints no proof.

`internal/core/core.go`, gate handler (line 2424 area): next to the existing
`store.GateCheckResult` unwrap, `if result, ok := value.(store.CanaryRunResult); ok {
return handlerData{Data: result, Verdict: result.Verdict}, nil }`. Envelope code and exit:
fired `PASS`/0, did not fire `FAIL`/1, unevaluated `UNEVALUATED`/3 (`verdictExit`).

## 4. Invariants

- **I1 (285).** The set of tracked paths a gate checker can evaluate, and the set its
  subject digest covers, are the same set: one filter, applied in `captureSubjectEntries`
  before both. `subjectTreeDigest(root)` equals `captureSubject(root).digest` for any
  stable tree.
- **I2 (285).** A tracked path is excluded if and only if it starts with `.aira/tickets/`
  or `.aira/findings/` relative to the project root. Every other tracked path, including
  all other `.aira/` content, is in the subject.
- **I3 (286).** For every ledger and tree state, `GateCheck` returns the same `Verdict`,
  `Code`, `Trusted` and `Suspect` as at `44ab844`. `Mismatch` is descriptive only.
- **I4 (286).** `Mismatch` is non-empty exactly when `Code` is `U_GATE_PROOF_STALE`, and
  holds only vocabulary names, in vocabulary order, without duplicates.
- **I5 (288).** Run ids are references: they are never compared as a binding, never
  required for a pass, and their absence never reads as "no output".
- **I6 (287).** `canary-run` writes nothing to the gate ledger. `Fired` is `null` whenever
  the canary-tree result is not established. `RunGate` and `canary-run` use the one
  `CanaryHealthFor` mapping.
- **I7.** Daemon protocol unchanged (14). New fields are additive JSON.

## 5. Tests (TDD; each must fail against the wrong implementation)

Write each test first and record it RED on `44ab844`, or RED against the named mutation
where the current code cannot fail it. Real-cgroup tests use `realCommandStore` and follow
`cgrouptest.SkipOrFailRealCgroup` (mandatory in `make ci`'s real mode). Every forged ledger
record goes through `OpenGateAudit(...).Append`, as `TestPassWithoutProofCannotResurfaceThroughReadOnlyCheck`
does.

**AIRA-285** (`internal/store/gate_subject_scope_test.go` unless noted):

- `TestSubjectDigestIgnoresAiraBookkeeping`: fixture with code, a tracked
  `.aira/tickets/AIRA-1.md` and `.aira/findings/f-1.md`. Unstaged edit of the ticket, a new
  tracked ticket, an edit of the finding: the digest is unchanged each time (RED today).
- `TestSubjectDigestCoversEveryTrackedFile` gains rows that must still move the digest:
  `.aira/config`, `.aira/requirements/AR-1.md`, `.aira/gates/g.json`,
  `.aira/gates/canaries/g-canary.json`, and the near misses `.aira/tickets-archive/x.md`,
  `.aira/ticketsx/y.md`, `docs/.aira/tickets/x.md`, `nested/.aira/findings/f.md`.
- `TestExcludedBookkeepingIsAbsentFromEveryLane`: `captureSubject(root).entries` holds no
  excluded path and does hold the requirement and gate files; the directory returned by
  `materializeSubject(subject)` has no `.aira/tickets` and no `.aira/findings` (filesystem
  assertion) and has the others; `subjectTreeDigest(root) == captureSubject(root).digest`.
- `TestTicketMoveKeepsProvenGate` (`gate_honesty_test.go`, runner-free): from
  `newProvenGateStore` plus a tracked ticket, `RunGate` pass; an unstaged ticket edit
  through the store's own status-transition path (the one `aira mv` uses; a direct file
  edit if that path needs a daemon) leaves `GateCheck` at a trusted pass (RED today, the
  measured `U_GATE_NO_RESULT`). Twin, same test: then edit `.aira/requirements/AR-1.md`
  -> `U_GATE_NO_RESULT` (requirements still bind).
- `TestCommandGateNeverSeesAiraBookkeeping` (`gate_command_integration_test.go`, real
  cgroup): non-Go subject whose tracked ticket contains marker `M`; command gate
  `detect-marker . M`; `inject-file` canary writing `M` to a normal path. `RunGate` ->
  trusted pass (the command did not see `M`); then edit the ticket -> `GateCheck` still
  pass.
- `TestInjectFileCanaryIntoBookkeepingDoesNotFire` (real cgroup): the canary targets
  `.aira/tickets/zz.md` -> `E_GATE_CANARY_DID_NOT_FIRE`, verdict fail, no proof-of-fire
  record appended.
- Mutations, each must turn a test RED:
  - **F1 (false pass)** filter in `digestSubjectEntries` instead of the capture (digest
    excludes, snapshot includes): `...AbsentFromEveryLane` (entries and directory) and
    `...NeverSeesAiraBookkeeping` (the command sees `M`, fails).
  - **F2 (false pass / skew)** filter only in `subjectTreeDigest`: the equality assertion,
    and `TestTicketMoveKeepsProvenGate` (RunGate and GateCheck digests never agree).
  - **F3 (false pass)** exclude all of `.aira/`: the config, requirements and gate rows,
    and the requirement twin (an edited requirement would keep the pass).
  - **F4 (false pass)** exclude `.aira/` except `.aira/gates/` (the peer's literal
    suggestion): the config and requirements rows and the twin.
  - **F5** prefix without the trailing slash: the `tickets-archive` and `ticketsx` rows.
  - **F6** `strings.Contains`: the `docs/` and `nested/` rows.
  - **F7 (false fail)** no exclusion at all (today): the first and fourth tests.

**AIRA-286** (`internal/store/gate_eval_test.go`, runner-free; the env rows use a
command-gate definition and canary written as files, with records forged for it, because
`GateCheck` never launches a process):

- `TestStaleProofNamesEveryMismatchedBinding`, table-driven. Fixture: `newProvenGateStore`
  gives a real result `R` and proof `P`. Result rows append a copy of `R` with one field
  changed (its `proof_seq` still links to `P`). Proof rows append a copy of `P` with one
  field changed, then a copy of `R` whose `proof_seq` names it. Each row asserts `Verdict
  unevaluated`, `Code U_GATE_PROOF_STALE`, `Trusted false`, and exactly the expected
  `Mismatch`. Rows: site 1 through forged result fields (`definition_digest`,
  `declaration_digest`, `subject_scope`, `lane`, `evaluator_version`, empty
  `canary_tree_digest`: six), and through the current state (canary file deleted ->
  `[declaration]`); site 2 through forged proof fields (all nine, `env_digest` on the
  command-gate fixture); site 3 (`t.Setenv` on an allow-listed variable -> `[env]`); site 4
  (`at` older than `max_age_secs` -> `[age]`; `at` = `garbage` -> `[age]`); and definition
  plus lane both changed -> `[definition lane]` (asserted twice in one run). **Control rows:**
  an unchanged copy of `R` -> trusted pass with empty `Mismatch`; an unchanged copy of `P`
  plus a linked copy of `R` -> trusted pass. These prove the forging harness alone does
  not trip staleness, so a stale row cannot pass for the wrong reason.
- `definitionErr` (a discovered definition that `gate.DigestGate` refuses) and `digestErr`
  (a resolved canary that `DeclarationDigest` refuses) are not constructible through
  `discoverGates` / `canaryFor`; they are code-reading items for the build review
  (accepted coverage gap).
- `TestReadyAndCheckFindingsNameTheBinding`: env-mismatch fixture -> the `ready` finding
  and the `aira check` gates finding both carry `U_GATE_PROOF_STALE` and a message ending
  `(does not match: env)`.
- `TestEnvAllowWarnings` (`internal/gate`): `PATH` -> the one sentence; `PATHX`, `path`,
  `MY_PATH`, empty list -> none. Store: `gate add` and `gate set` adding `PATH` carry it in
  `GateWriteResult.Warnings`; without `PATH`, none. `RunGate` on a `PATH` gate carries it
  in `Warnings` (real cgroup).
- Mutations: drop any one term from either helper (its row passes, RED); compute names
  but decide staleness from a narrower expression (rows RED); emit names on a pass
  (control rows RED); unordered names (the double assertion flakes); `strings.Contains`
  for the warning (`PATHX` RED); also check I3 by keeping every pre-existing gate test
  green unmodified.

**AIRA-288** (`gate_command_integration_test.go`, real cgroup; add a helper action
`list-tree` that prints every file name under the cwd, so a canary run's output names the
injected file and a subject run's does not):

- `TestGateRunRecordsAndReturnsRunIDs`: a failing command gate with an `inject-file`
  canary. The result record has `run_id` and `canary_run_id`; the returned result carries
  the same ids; `GateCheck` returns them; `runner.ReadOutput` of `run_id` lacks the
  injected name and of `canary_run_id` has it (mutation: swap the two -> RED; omit either
  -> RED).
- `TestCanaryErrorRecordsCanaryRunID` (runner-free, a sequenced variant of
  `stubExecution` from `gate_cpu_timeout_test.go`): the first launch (subject) is a clean
  exit-0 record with complete outputs; the second (canary) returns `RUN-2` whose
  `ReadOutput` reports incomplete output, so `runCommandChecker` returns an error carrying
  `RunID`. The unevaluated result record has `run_id RUN-1` and `canary_run_id RUN-2`; the
  returned error names both and still classifies as `U_GATE_COMMAND_RUN_UNEVALUATED` via
  `store.ErrorCode` (mutation: put the ids before the code -> RED). A timeout is not this
  path (it returns no error and goes through the main path), so it is not used here.
- `TestRunIDIsNotABinding` (runner-free): a forged otherwise-valid result whose `run_id`
  names no run still reads as a trusted pass.
- `TestTraceabilityGateRecordsNoRunID`: dimension `RunGate` result record has neither
  field, and the returned result's fields are empty.

**AIRA-287** (`internal/store`, runner-free with fixture canaries on the traceability
gate; `internal/gate`; `internal/core`):

- `TestCanaryHealthFor`: `fail` -> pass, `pass` -> fail, `unevaluated`, `""` and `bogus`
  -> unevaluated (mutation: default arm to fail -> RED).
- `TestCanaryRunReportsFired`: the `testTraceGate` fixture canary (fires) -> `verdict
  pass`, `fired true`, `gate_result fail`; a seed whose links resolve (does not fire) ->
  `fail`, `false`, `pass`; a seed that yields an unevaluated traceability predicate
  (`U_TRACE_EMPTY`) -> `unevaluated`, JSON `"fired":null`, `gate_result unevaluated`
  (mutation: non-pointer `Fired` -> `false` -> RED). The ledger is byte-identical before
  and after each call (I6).
- `TestCanaryRunEnvelope` (`internal/core`, fake gate action store): the three results ->
  `PASS`/0, `FAIL`/1, `UNEVALUATED`/3 (mutation: return the value unwrapped -> all exit 0,
  RED).
- Mutation: `RunGate` keeps its own copy of the mapping with one arm changed -> the
  existing proof-of-fire tests go RED; kept as a code-reading item that the inline copy is
  deleted.

Gate: `aira confine -- make ci` (the gate's exact command), plus `go test -race` on
`internal/store`, `internal/gate` and `internal/core`.

## 6. Expected yield

- The peer's flow (`gate run`, `aira mv`, `ready`) keeps the proof: the measured
  `U_GATE_NO_RESULT` after a ticket edit becomes a trusted pass. For a 15-minute gate on a
  116-ticket project, that removes a gate re-run after every bookkeeping edit. Same for
  AIRA itself once it adopts gates.
- A refused pass says why: the peer's earlier `U_GATE_PROOF_STALE` would have read
  `(does not match: env)`, and `gate add`/`set`/`run` would have warned about `PATH`
  before the first stale proof, instead of a debugging round and a workaround commit.
- A failing gate's output is one `aira run-log` call away, from the id in the reply, in
  `gate check`, or in the error text.
- `canary-run` answers the question it is asked (did the canary fire?) and its exit code
  says so.

## 7. Deferrals and "not in this batch"

| # | Item | Why not here |
|---|---|---|
| D1 | **Read-time env binding compares against the wrong process.** `ready`, `gate check`, `gate prove` (daemon) and `aira check` (caller) can disagree on the same proof, and neither compares against the environment that will actually run the next `gate run`. Two honest fixes: (a) the client sends its own values of the allow-listed names with daemon-routed reads, so the read compares against the caller (keeps M10b 3.4; `ready` answers then depend on which shell or MCP host asks); (b) drop the read-time comparison and keep env as a proof-time binding only (the result and its proof must share one env digest, already checked), so env drift is caught by the next `gate run`. Today's daemon comparison delivers neither: the daemon's environment never changes, so a caller's env change is invisible to `ready` anyway. | A design reversal either way; owner decision. Recommendation: (b), simpler and it removes the PATH trap at the root, after which the PATH warning can be deleted. Suggest a held ticket. |
| D2 | Per-gate subject glob | Peer: optional, not needed. Rejected in principle for AIRA-72. |
| D3 | Requirement edits still invalidate command gates | Correct for traceability gates; for command gates an over-invalidation (safe direction). Revisit only with a measured cost. |
| D4 | `aira gate log <gate-id>` | Cut (section 2). A thin wrapper over `run-log` if dogfooding shows the two-step path is friction. |
| D5 | `exit-zero` reads every non-zero exit as `fail` | The peer's `check.sh` exits 3 for "tool missing, unevaluated". AIRA reads that as `E_GATE_COMMAND_FAILED` on the subject, and on the canary tree as a fire, which mints proof-of-fire. No false pass in a steady environment (the subject fails too), but a fire can be caused by something other than the mutation (already documented for `inject-file`). A declared "unevaluated exit codes" list for command gates would close it. Suggest a ticket; tell the peer. |
| D6 | `gate add` dispatch example uses `--env-allow PATH` | It will now print the warning, which is accurate. Changing the example is part of D1. |
| D7 | Output retention | Runner outputs are never evicted (pre-existing). This plan adds references to runs that already exist and stores nothing new. |

## 8. Risks

- **Every stored result is invalidated once** in repositories that track tickets or
  findings (3.1.2). Fail-closed; release notes say to re-run each gate once after the
  daemon runs the new binary.
- **Refactor of the binding check (286).** Replacing two `||` expressions with
  name-returning helpers is the one place this batch could drop a comparison and serve a
  false pass. Mitigations: control flow kept identical (I3), one table row per term with
  control rows, and the build review reads the old and new expressions side by side.
- **Exit code of `canary-run` changes** from always 0 to 0/1/3, and the meaning of its
  `verdict` key changes from "the gate's result on the canary tree" to "the canary's own
  verdict" (the old meaning moves to `gate_result`). The peer runs it by hand; release notes
  and the peer notice say so.
- **A command gate that read ticket files** loses them (3.1.1). None known: AIRA has no
  gates, and tooling's `check.sh snapshot` reads no `.aira/` path (checked: `step_trace`
  reads `evidence/live.json` and `clankerops/docs/*.md`).
- **Skill and help text** change (two operation summaries): separate `aira skill install`
  at release.

## 9. Release notes (for the build)

Proto unchanged (14). Daemon restart required (the subject digest and the stale-binding
names are computed by `GateCheck`, which `ready` runs in the daemon); `aira install`
performs it. Skill reinstall required. After upgrading, run `aira gate run <id>` once per
gate in any repository that tracks `.aira/tickets/` or `.aira/findings/`. Tell ci-build:
ticket moves no longer invalidate proofs; stale proofs name the binding; `canary-run`'s
`verdict` is now the canary's own (pass = fired) with `fired` and `gate_result` beside it
and a 0/1/3 exit; failing-gate output via `aira run-log <run id>` from `RunID` /
`CanaryRunID`; and D5 (their exit-3 convention).

## 10. Review log

Plan review pending (Codex Sol `high` and Gemini as orthogonal reviewers when available;
Fable as the plan gate). Standing questions for reviewers: what can be removed while still
solving the four reports; what is the from-scratch minimum; is any tracked path that a
checker can observe now outside the digest (the false-pass direction); does any binding
term disappear in the 286 refactor.
