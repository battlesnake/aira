---
{"schema":1,"id":"AIRA-23","project":"aira","title":"Default/infer confine owner to prevent cross-session kill misattribution","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["confine","dogfood","safety"],"hold":false,"relations":[]}
---
Reported by two dogfooding sessions 2026-08-31: `aira confine --list` shows OWNER "unknown" for jobs
launched outside an adopted project without --owner/AIRA_CONFINE_OWNER. One session nearly `pgrep`-killed
TWO sibling sessions' gates, mistaking them for its own — caught only by inspecting each proc's cwd.

CURRENT BEHAVIOUR (confirmed): resolveConfineOwner (cmd/aira/main.go:890-909) precedence is
  --owner flag → AIRA_CONFINE_OWNER env → discovered project WorktreeID → "unknown" (runner.ConfineUnknownOwner,
confine.go:124). So a confine launched outside a project worktree with neither flag nor env → "unknown".
Filesystem-scan-only discovery (no live daemon registry entry) also renders "unknown"
(confine_manage_linux.go:79,204).

DIRECTION: make owner default to a stable, session-distinguishing identity so --list is actionable and
cross-kill hazard drops, WITHOUT weakening the existing kill ownership-guard (#68). Candidates: derive from
the launching session (e.g. the CC socket / session id), the login+pid-namespace, or the supervisor's cwd
repo — whatever is cheap and stable. Keep "unknown" only as a genuine last resort, and never let a fabricated
owner bypass the cross-session kill guard. Small, high-safety hygiene item. relates: #68 (confine --list/--kill
ownership guard), the detached-confine ticket (a persisted owner-keyed record helps here too).

## Resolution (2026-09-04, backlog-remediation Phase 0, plan §2) — landed with AIRA-52

Landed as one identity decision together with AIRA-52 (which persists the owner
in the scope directory name); the full reasoning is recorded there. The AIRA-23
half:

`resolveConfineOwner`'s last resort is no longer the literal `"unknown"`. It is
`runner.InferConfineOwner(cwd)` → `@cwd-<sanitised basename>`. The candidate
chosen from this ticket's own list is "the supervisor's cwd repo", because this
ticket's incident is itself the argument for it: a session nearly `pgrep`-killed
two SIBLING sessions' gates, all showing OWNER "unknown", and was saved *only by
inspecting each proc's cwd*. cwd is the discriminator that was missing from
`--list`.

**The direction's hard constraint — "WITHOUT weakening the existing kill
ownership-guard (#68)... never let a fabricated owner bypass the cross-session
kill guard" — is honoured structurally, not by care.** A bare cwd identity would
weaken it: two sessions in one directory infer the same string, so honouring it
would let either kill the other's job without `--steal`. Instead:

- the inferred form carries `ConfineInferredOwnerPrefix` (`@`), a character
  OUTSIDE the caller-supplied identity alphabet, so it can never be forged into
  an attested owner — not on the command line (`--owner @cwd-x` is refused), not
  through `AIRA_CONFINE_OWNER`, and not on the admission wire;
- `ConfineOwnerIsAttested` is false for empty, `"unknown"` and every inferred
  value, and the kill guard now requires BOTH the scope's owner and the caller to
  be attested. An inferred owner does not open the guard even against an
  identical caller identity.

So the guard is exactly as strong as before — `--steal` is still required for
every job whose session did not name itself — while `--list` reports where a job
came from instead of "unknown". "Keep `unknown` only as a genuine last resort" is
honoured too: `InferConfineOwner` returns `ConfineUnknownOwner` when the cwd
yields nothing usable, because a fabricated identity is worse than an honest
unknown.

`internal/core/skill.go`'s confine guidance now explains the `@` prefix and says
plainly that setting `AIRA_CONFINE_OWNER` is what buys you a `--steal`-free kill.

Regression test: `TestConfineKillRefusesAnInferredOwnerWithoutSteal`
(`internal/runner/confine_manage_linux_test.go`), plus the extended
`TestConfineOwnerDerivationChain` (`cmd/aira/confine_test.go`), which pins that
the inferred form is unattested AND unforgeable.

AIRA-23 -> done. `make ci`: exit 0.
