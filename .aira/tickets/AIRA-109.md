---
{"schema":1,"id":"AIRA-109","project":"aira","title":"core.Do's handlerData.Code / RunRecord.ErrorCodes bypass both AIRA-99 guards against a W_ code","status":"in-review","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
---
Surfaced by the adversarial build review for AIRA-99 (Sol/Codex), which was
scoped narrowly to store.ErrorCode -- this is a genuinely separate, currently
inert finding, filed rather than fixed inline to keep that change mechanical.

AIRA-99 made store.ErrorCode the structural guard against a W_ (warning) code
ever being raised as an `error` and reaching Response.Code as a reported
failure that exits 0. That guard, and the existing static-scan test
(TestNoWarningCodeIsRaisedAsAnError), both operate on errors constructed as
"CODE: message" strings -- the repo's one error-construction convention.

Two paths into core.Do's Response.Code do NOT go through either guard, because
they never construct an `error` at all -- they are plain string codes assigned
directly by their producers:

- internal/core/core.go's `handlerData.Code` field (set directly by handler
  functions, e.g. `handlerData{Code: "E_RUN_FOREIGN_OWNER", ...}`), consumed at
  core.go's `handlerCode != ""` branch (~core.go:548-553) with
  `Exit: codes.ExitForCode(handlerCode)`.
- `runner.RunRecord.ErrorCodes` (a `[]string`, populated across
  internal/runner/runner_linux.go, detach_linux.go, evidence.go), consumed by
  `runRecordCode` (core.go:619) as `record.ErrorCodes[0]` and then
  `Exit: codes.ExitForCode(code)` (core.go:562-564).

Verified at AIRA-99 time (grep): every literal currently assigned to either
field is E_/U_-prefixed. No W_ code is produced on either path today, so this
is not a live bug. But if one ever were added:

1. store.ErrorCode's new guard would never see it (it is never parsed out of
   an error message).
2. TestNoWarningCodeIsRaisedAsAnError's scan would not catch it either: a bare
   string literal like `"W_SOMETHING"` has no colon, so it fails the scan's
   `idx <= 0` / "CODE: message" shape check by design (that check is what lets
   the scan tell a CheckFinding.Code/TicketRecord.Warnings bare literal, which
   is legitimate, apart from an error-message-form literal, which is not).

So a future author adding a W_-prefixed literal to either field would produce
exactly the AIRA-99 hazard (an exit that reports a failure as success) with
neither of AIRA-99's two safeguards able to see it.

Not decided or actioned here. Plausible directions for whoever picks this up:
- Extend the static scan to also flag a bare `"W_..."` literal specifically
  where it is the argument to `handlerData{Code: ...}` or
  `append(...ErrorCodes, ...)` / `appendUnique(...ErrorCodes, ...)` -- narrower
  and more surgical than treating every bare W_ literal as suspect (bare W_
  literals are legitimately common as CheckFinding.Code/TicketRecord.Warnings
  values).
- Add a cheap runtime assertion in core.Do itself at the two consumption sites
  (handlerCode, runRecordCode) that a W_-prefixed code never reaches
  Response.Code with a nonzero-implying OK:false -- this is exactly the
  "core.Do refuses a W_ code" direction AIRA-99 rejected as incomplete for the
  ErrorCode-boundary hazard, but it is NOT incomplete for this specific pair of
  in-process fields, since both are internal to core.Do's own dispatch and
  nothing bypasses core.Do to set them.
- Decide it is not worth guarding given the fields are producer-controlled by
  a small, reviewed set of call sites in internal/runner and internal/core
  rather than free-form error text -- i.e. accept the residual risk and
  document why here instead of building more machinery
  ([[architectural-simplicity]]-style judgement call for the owner).

## Resolution (in-review)

Branch `aira109-w-code-bypass-scan`, off `cd1dabf`.

### Still not a live bug — reconfirmed before building

Re-grepped the whole tree at `cd1dabf`. Every bare `"W_..."` literal outside
`internal/codes/codes.go` is one of: a `CheckFinding{Code: ...}` value
(`store/traceability.go`, `store/check.go`, `store/area.go`,
`store/relation_ready.go`), a `TicketRecord`/row `Warnings` entry
(`store/query.go`, `store/finding.go`, `store/relation_ready.go`), a local
`warning` variable, or a `strings.HasPrefix` classifier
(`runner/confine_detach_linux.go`). Not one is a `handlerData{Code: ...}` value
or an `ErrorCodes` append argument. The three `handlerData{Code:}` literal sites
are `U_TESTREPORT_INCOMPARABLE`, `E_RUN_WIRING_INCOMPLETE` and
`E_RUN_FOREIGN_OWNER`; every `ErrorCodes` append literal is `E_`/`U_`. So the
hazard is still latent, and this change is a guard against a future author, not
a fix for a present defect.

### What was built — option 1 of the three listed above

The coordinator chose the static-scan extension over the runtime assertion and
over accepting the risk. It mirrors the AIRA-99 pattern (extend the existing
guard rather than add machinery), which is what
[[architectural-simplicity]] asks for: no production code changed at all, only
a test-time scan plus a corrected doc comment.

`internal/codes/produced_test.go` gains
`TestNoWarningCodeIsAssignedAsADirectResponseCode`, which walks the same
non-test Go files the AIRA-87 catalogue scan already walks and flags a whole
`W_` code literal in exactly two AST shapes:

1. the `Code:` value of a `handlerData` composite literal, and
2. any non-first argument of an `append`/`appendUnique` call whose first
   argument is an `.ErrorCodes` selector.

Everything else is left alone on purpose. A bare `W_` literal is the normal,
correct way to write a `CheckFinding.Code` or a `Warnings` entry, so flagging
every bare `W_` literal would be noise that teaches authors to route around the
check — and the fix they would reach for (stop reporting the warning) is worse
than the thing being guarded.

The scan matches by AST shape and by identifier name, not by type-checking:
`internal/codes` must not import `internal/core` or `internal/runner`, and a
type-checked scan would pull the module into this package's test binary for no
extra safety. The name dependence is made safe by a non-vacuity guard — the
test also counts the sites it matched and **fails** if either shape matched
nothing anywhere in the tree, so a rename of `handlerData` or `appendUnique`
surfaces as "the shape matcher is stale" rather than as a silently vacuous pass.
That is the same "unevaluated is never a pass" discipline `scanProducedCodes`
already applies when its walk returns an empty code set.

### Tests, and the evidence they are not porous

Four tests, and each was made to fail on purpose before being trusted:

- `TestNoWarningCodeIsAssignedAsADirectResponseCode` — the tree-wide guard.
  **Proof it can fail:** planting `handlerData{Code: "W_RUN_FOREIGN_OWNER"}` at
  `core.go:1806` and `appendUnique(record.ErrorCodes, "W_RUN_FAILED")` at
  `runner_linux.go:811` made it report both, at the right file:line, with the
  right shape. In the same run the pre-existing
  `TestNoWarningCodeIsRaisedAsAnError` still **passed** against both planted
  violations — which is the ticket's whole claim (that these two paths bypass
  the existing guards) demonstrated rather than asserted. Both plants reverted.
  **Proof the non-vacuity guard works:** renaming `handlerData` throughout
  `internal/core` made the test fail with "the scan matched no
  handlerData{Code: ...} site anywhere in the tree". Reverted.
- `TestDirectResponseCodeScanCatchesBothBypassShapes` — plants a `W_` literal in
  `handlerData{Code:}`, in an `appendUnique(...ErrorCodes, ...)` and in a
  builtin `append(...ErrorCodes, ...)` in synthetic source, and requires all
  three flagged with the correct shape and no others.
- `TestDirectResponseCodeScanLeavesLegitimateWarningLiteralsAlone` — the
  false-fail direction: `CheckFinding{Code: "W_STALE_INDEX"}`, a
  `Warnings = []string{...}` assignment, `appendUniqueStrings(record.Warnings,
  ...)`, `append(row.Warnings, ...)`, a `warning := "W_..."` local and a
  `strings.HasPrefix(..., "W_")` classifier must all stay unflagged.
- `TestDirectResponseCodeScanSeesNonWarningCodesToo` — pins the non-vacuity
  guard's premise: the two shapes are matched by shape, so an `E_`/`U_` literal
  in either is counted as a site (keeping the tree-wide guard honest) while
  never being reported as a violation.

### Recorded limitations, not claimed away

- Only a whole literal is visible. A code reaching either field through a
  variable — `handlerData{Code: gitErr.Code()}`, `appendUnique(...,
  launch.Code)`, `appendUnique(..., decision.diagnostic)` — is invisible, the
  same blind spot `TestNoWarningCodeIsRaisedAsAnError` already records for
  `fmt.Errorf("%s: ...", code)`.
- Only `append`/`appendUnique` are matched, not a whole-slice assignment
  (`record.ErrorCodes = []string{"W_X"}`, or `ErrorCodes: []string{...}` in a
  `RunRecord` literal). No site in the tree writes `ErrorCodes` that way today —
  every one of the ~40 writes goes through `appendUnique` — so covering it would
  be machinery for a shape nobody uses.
- A direct field assignment on a zero-valued struct, `hd.Code = "W_X"`, is not
  matched, and **cannot be** without type information: `store.CheckFinding` also
  has a `Code` field, and `finding.Code = "W_STALE_INDEX"` is correct, common
  code. A name-only scan cannot tell the two apart, so catching the evasion
  would break the legitimate line. That trade is pinned by a line in
  `TestDirectResponseCodeScanLeavesLegitimateWarningLiteralsAlone` rather than
  left as prose.
- Only a keyed composite literal is matched; a positional `handlerData{...}`
  would slip past for the same want-of-types reason. Every `handlerData` in the
  tree is keyed.
- Two consequences of name-over-type matching are deliberate and both err toward
  flagging: any type whose field is called `ErrorCodes` counts (the output-chunk
  type in `runner/types.go` has one, and a `W_` there would be just as wrong),
  and an `append` whose result is discarded is still reported (a discarded
  append of a warning code is a bug either way).

The last three came from an external adversarial read of the scan (DeepSeek V4
pro, asked for evasion shapes). Its other findings — aliasing and spread
arguments — are the already-recorded variable/whole-slice gaps in another
costume and are folded into those bullets. Every limit is written into the
test's doc comment too, where the next author will find them.

A `"W_CODE: message"` literal in either field is not a gap: it has a colon, so
it fails `codePattern` here and is caught by `TestNoWarningCodeIsRaisedAsAnError`
instead. Between the two tests, the bare and message forms are both covered.

`internal/store/check.go`'s `ErrorCode` doc comment said this hazard was
"recorded as a follow-up rather than fixed here"; it now points at the scan that
covers it and keeps only the residual variable-valued gap.

### Deliberately not done

No runtime assertion in `core.Do` (option 2). It would duplicate a guard the
static scan already gives at authoring time, and it can only report a problem
after a wrong code has already been produced — whereas the scan refuses to let
the code be written. Nothing changed in `internal/core` or `internal/runner`.
