---
{"schema":1,"id":"AIRA-109","project":"aira","title":"core.Do's handlerData.Code / RunRecord.ErrorCodes bypass both AIRA-99 guards against a W_ code","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
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
