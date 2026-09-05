---
{"schema":1,"id":"AIRA-99","project":"aira","title":"store.ErrorCode/core.Do have no structural guard against a W_ (warning) code being raised as an error and exiting 0","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty","store"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-107","to":"AIRA-99"},{"kind":"relates","from":"AIRA-109","to":"AIRA-99"}]}
---
Found by AIRA-87's independent build review (PR #41). A test now pins the invariant (TestNoWarningCodeIsRaisedAsAnError) but the code itself doesn't structurally forbid it: store.ErrorCode accepts a W_ prefix and returns it as a code, and core.Do puts whatever ErrorCode returns into Response.Code with Exit: ExitForCode(code) -- so an error of the form "W_X: ..." would exit 0 (a failure silently reported as success) if one were ever raised. No such error currently exists in the tree. Fix direction, not built: make ErrorCode return E_INTERNAL for a W_-prefixed error (warnings are never supposed to be raised as errors), or have core.Do refuse a W_-prefixed code on an error response outright.

Two smaller, lower-priority findings from the same review, bundled here rather than filed separately:
- cmd/aira/tui_execute.go:467 classifies on a U_GIT_ family that does not exist anywhere in the tree -- a permanently dead branch (trailing-underscore prefixes are excluded by design from AIRA-87's produced/catalogued scan, so it can't see this itself).
- Eleven E_ exit codes were registered at exit 1 (the default) by AIRA-87 purely to complete the vocabulary with zero behaviour change (e.g. E_COMMAND_INVALID, E_ADMIT_SATURATED, E_RUN_RECONCILE_REQUIRED) -- several look like they should plausibly be a different exit code (E_ADMIT_SATURATED looks like a 4, not a 1). Re-bucketing them is a real contract decision, deliberately left out of AIRA-87's layering-only PR. Not decided or actioned here.

## Resolution

Built and merged: PR [#54](https://github.com/battlesnake/aira/pull/54), squash-merged as
`c5c01b377946f2c09908d852f0dc16cdfca2e5df` on `origin/master`. Worktree
`/home/mark/claude/worktree-aira99`, branch `aira99-errorcode-guard`, starting commit
`f1f699a` (rebased three times onto a fast-moving `origin/master` before merge; each
rebase re-verified clean with no file overlap).

### What was built

1. **`internal/store/check.go`** (~line 649) -- `ErrorCode`'s predicate changed from
   accepting `E_`/`W_`/`U_` uniformly to accepting only `E_`/`U_`. A `W_`-prefixed error
   message now falls through to the existing `"E_INTERNAL"` return, exactly like any
   other unrecognised error text (exit 4, never 0). Doc comment states the rule
   explicitly and records why the ticket's own alternative ("have core.Do refuse a W_
   code") was rejected: dozens of call sites invoke
   `codes.ExitForCode(store.ErrorCode(err))` directly for a process exit without ever
   going through `core.Do` (verified by grep: ~55 `store.ErrorCode(` call sites outside
   package store, plus `cmd/aira/main.go`/`daemon_command.go` feeding
   `codes.ExitForCode` directly), so `ErrorCode` itself is the one choke point that
   covers every caller deriving a code from an `error` value this way. The comment also
   states, precisely, what this does *not* cover (see AIRA-109 below).
2. **`internal/store/error_code_test.go`** (new) -- `TestErrorCodeFoldsWarningPrefixToInternal`
   pins `ErrorCode(errors.New("W_STALE_INDEX: x")) == "E_INTERNAL"` and
   `codes.ExitForCode(...) != 0`, against both a real catalogued warning
   (`W_STALE_INDEX`) and a made-up, never-catalogued one (`W_MADE_UP_FOR_TEST_ONLY`) --
   the second case added after adversarial review flagged that pinning only the
   catalogued code would pass against a broken implementation that merely
   special-cased that one string.
3. **`internal/codes/produced_test.go`** -- `TestNoWarningCodeIsRaisedAsAnError` kept
   verbatim in logic; its docstring updated to state it is now *also* backed by
   `ErrorCode`'s structural guard rather than being the only thing making W_=>0 safe,
   and its failure diagnostic (previously claiming a matched literal "would exit 0")
   corrected to match the new fold-to-`E_INTERNAL` behaviour.
4. **`cmd/aira/tui_execute.go:467`** -- deleted the dead `U_GIT_` disjunct from
   `executeHasLaunchEvidence`'s `"git"` case; confirmed by grep that no `U_GIT_` code
   exists or is catalogued anywhere in the tree. Existing TUI execute tests
   (`TestExecuteHonestyReportsExecutionAndPersistenceSeparately` et al.) still pass
   unchanged.
5. Item 3 (re-bucketing the eleven `E_` codes at the default exit 1) was explicitly
   **not** actioned, per the brief -- filed as its own ticket instead (below).

### Design questions resolved

- **Which fix direction** ("ErrorCode returns E_INTERNAL for W_" vs "core.Do refuses a
  W_ code"): the brief mandated the former and explicitly forbade the latter as
  incomplete; confirmed correct by grep (11+ call sites bypass `core.Do` entirely for a
  raw process exit, and ~55 total `store.ErrorCode(` call sites exist outside package
  store) -- `ErrorCode` is the only choke point that actually covers every caller.
- **Whether any consumer depends on `ErrorCode` ever returning a `W_` code**: verified
  by grep across the whole tree -- no comparison, `switch`, or `strings.HasPrefix(code,
  "W_")` anywhere reads `ErrorCode`'s return value expecting a warning prefix. Safe to
  narrow the predicate.
- **`TestNoWarningCodeIsRaisedAsAnError`'s fate**: kept exactly as instructed (it
  catches the authoring mistake at its source, independent of `ErrorCode`'s runtime
  guard), docstring updated to describe the now-doubled guard.
- **Item 3 (re-bucketing)**: explicitly not decided or actioned, per the brief. Filed as
  [AIRA-107](/.aira/tickets/AIRA-107.md), which also pins down precisely *which* eleven
  codes AIRA-87 added at the default exit 1 (identified by diffing the pre-move
  `internal/store/check.go` catalogue against the post-move `internal/codes/codes.go`
  one): `E_ADMIT_SATURATED`, `E_ADMIT_TOO_LARGE`, `E_COMMAND_INVALID`,
  `E_FINDING_INDEX_DIVERGENCE`, `E_GATE_EXISTS`, `E_RANT_REDACTED`,
  `E_RANT_REDACTION_INCOMPLETE`, `E_RELATION_INDEX_DIVERGENCE`,
  `E_RUN_RECONCILE_REQUIRED`, `E_RUN_TELEMETRY_CONFLICT`, `E_RUN_USAGE_READ`.
- **A new question this work surfaced, not in the original brief**: the adversarial
  build review (Codex/Sol) found that `core.Do`'s `handlerData.Code` field and
  `runner.RunRecord.ErrorCodes` are plain strings assigned directly by their producers
  and reach `Response.Code` without ever calling `store.ErrorCode` or matching
  `TestNoWarningCodeIsRaisedAsAnError`'s colon-delimited scan (a bare code literal has
  no colon). Verified by grep: every literal assigned to either field today is
  `E_`/`U_`-prefixed, so this is currently inert, not a live bug. Not fixed here (it
  would mean touching `core.Do`, which the brief's own scope discipline argues against
  for a build-small ticket) -- filed as
  [AIRA-109](/.aira/tickets/AIRA-109.md) instead, with the doc comment on `ErrorCode`
  updated to state the boundary of what it guards precisely rather than overclaiming.

### Verification (all under `aira confine`)

- `go build ./...` -- exit 0 (build, and again after each of 3 rebases)
- `go vet ./...` -- exit 0 (vet, and again after each of 3 rebases)
- `go test ./internal/store/ ./internal/codes/ ./cmd/aira/ -count=1` -- exit 0, all `ok`
  (re-run after each rebase)
- Full `go test ./... -count=1` -- exit 0, 14 packages `ok`, 0 `FAIL` (run twice: once
  before the final round of review fixes, once after)
- Mutation-check: manually reverted `ErrorCode`'s predicate to re-accept `W_`, confirmed
  `TestErrorCodeFoldsWarningPrefixToInternal` fails in both its cases (`W_STALE_INDEX`
  and `W_MADE_UP_FOR_TEST_ONLY`), then restored the fix -- run twice (before and after
  the test was strengthened to two cases).
- The repo's own `pre-push` hook independently ran the full confined
  `go vet`/`go build`/`go test ./... -count=1 -timeout 20m` gate on both pushes to the
  PR branch; both exited 0 with all 14 packages `ok`.

### Review

Self-review, then one independent adversarial pass by Codex (Sol, `high` reasoning
effort, read-only sandbox against the actual worktree) per the brief's build-small
lighter path. Initial verdict `CONCERNS: 3 issue(s)`, all addressed before merge:

1. *"The 'single structural choke point' claim is overbroad: core.Do's handlerCode and
   runRecordCode branches bypass ErrorCode."* -- Confirmed true by reading `core.go`.
   Narrowed the doc-comment's claim to what `ErrorCode` actually guards (codes derived
   from an `error` value), added an explicit paragraph naming the two fields it does
   not cover, and filed AIRA-109 to record the residual rather than expand this
   ticket's scope into `core.Do`.
2. *"Testing only W_STALE_INDEX permits an implementation that special-cases that code
   while accepting every other W_."* -- Added a second, never-catalogued case
   (`W_MADE_UP_FOR_TEST_ONLY`) to `TestErrorCodeFoldsWarningPrefixToInternal`;
   re-ran the mutation-check to confirm both cases still fail against the reverted
   predicate.
3. *"The diagnostic ... still claims ErrorCode returns warning codes, giving a false
   explanation when the authoring test fails."* -- Corrected
   `TestNoWarningCodeIsRaisedAsAnError`'s `t.Errorf` message, which pre-dated this fix
   and had gone stale (it said a matched literal "would exit 0", no longer true once
   `ErrorCode` folds it to `E_INTERNAL`).

No P0s at any point. `internal/codes/codes.go` and the eleven AIRA-107 codes were
confirmed untouched by the reviewer, as instructed.
