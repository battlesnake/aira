---
{"schema":1,"id":"AIRA-185","project":"aira","title":"aira drain wait -- hold the slice empty for a deploy, reusing exclusive-mode admission mechanics","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","deploy"],"hold":false,"relations":[]}
---

Owner observation (2026-09-08): "Perhaps it would be useful if there was a
way for us to 'drain' the slice so we can wait for it to drain and not
admit any new jobs (just leave them queued). Would be useful for when we
need to deploy updates. We already have the exclusive mode, so can
probably re-use logic written for that."

Directly relevant to this same session's own standing task: a full night
spent watching `aira confine --list` by hand, waiting for the box to quiet
enough to flip the slice-ceiling default to `enforce` (AIRA-177, merged).

## Plan

Full design, three revisions: `docs/superpowers/plans/2026-09-08-slice-drain-mode-plan.md`.

Plan-reviewed by Astra (Codex, orthogonal review) across three passes:
- **Pass 1: NEEDS_REVISION**, four P1 findings against the original design
  (a detached `start`/`status`/`release` lifecycle with its own
  gate-generalisation and release-ownership mechanism) — a daemon restart
  would release the operator's own drain unexpectedly, queue-overflow
  falls back to flock bypassing the gate entirely, a detached helper
  survives its invoking script's death (the orphan-drain failure mode the
  design was meant to prevent), and `--kill --steal`'s ownership guard
  doesn't fit a scopeless drain (keyed on worktree identity, not session).
- **Pass 2 (revision 2, scope substantially narrowed): NEEDS_REVISION**,
  two small P2 findings — the plan still allowed "the CLI process blocks
  directly" as an alternative to launching through the real
  `runner.Confine` scope-creation path, and `--reason` couldn't be
  smuggled through the existing `Name` field (rejects spaces/colons, must
  match the scope ID) without a new wire field.
- **Pass 3 (revision 3): PASS.** Both fixes confirmed sufficient; no new
  blocking issue found across three independent passes.

## Summary of the approved design

- **v1 is `aira drain wait [--timeout DURATION] [--reason TEXT]`** — thin
  CLI sugar issuing the *same* `Exclusive: true` admission request `aira
  confine --exclusive -- <argv>` already makes today, where `<argv>` is a
  real placeholder binary launched through the genuine `runner.Confine`
  path (real scope, real admission, real charging) that blocks on
  SIGINT/SIGTERM/`--timeout` instead of running a user command. No detach,
  no background helper, no new gate state, no new release verb.
- **A detached/scriptable lifecycle (`start`/`status`/`release` surviving
  across processes) is explicitly deferred to a future v2** — Astra's pass
  1 findings against that shape are real, unsolved design problems, not
  implementation details; v1 sidesteps them entirely by staying
  foreground-only and connection-bound, inheriting exclusive mode's
  already-tested safety properties for free.
- **`--timeout` governs only the post-admission held duration**, wrapping
  the existing `ConfineRequest.Timeout` (which already excludes
  admission/setup time) — admission itself remains the separate,
  already-existing ~30-minute-default budget. Must be documented plainly
  in the tool's own `--help` text so it isn't misread as an overall
  deadline.
- **`--reason` is a genuinely new optional wire field** (not a reuse of
  `Name`), requiring a small additive change to `admit.go`'s argument
  validator (field allowlist, argument-count allowance, string parsing) —
  explicitly NOT a gate/behaviour change, threaded through to
  `confine --list`'s rendering (minimum v1 target) and optionally `aira
  top`'s footer.
- **What a drain actually protects against is narrower than it might
  seem, and this must not be oversold**: a daemon restart/binary swap does
  not kill already-admitted confine jobs today (separate cgroup scopes,
  never daemon children) — drain buys a clean, uncontended window and
  avoids a config change landing mid-contention, not job-safety. If the
  deploy itself restarts the daemon, the drain ends at that point (same
  fail-open-on-restart property everything else already has) —
  operational guidance is to sequence a daemon restart as the *last* step
  inside a drain window, documented as such rather than solved in code.
- **Queue overflow (256-entry cap) falling back to flock, bypassing the
  gate, is an accepted, documented limitation** of the reused mechanism,
  not something v1 fixes — drain is best-effort contention reduction, not
  an absolute guarantee.

Five open questions remain for whoever picks up the build (verb
name/home, whether `aira top`'s footer needs the `Reason` append in v1,
timing/trigger for a future v2 detached lifecycle, whether `install.sh`
should wrap itself with `drain wait`) — see the plan's §5.

## Status

Plan approved (Astra pass 3: PASS). Owner authorised proceeding straight
to build once approved. Not yet built.
