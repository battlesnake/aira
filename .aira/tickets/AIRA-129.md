---
{"schema":1,"id":"AIRA-129","project":"aira","title":"aira run: ci-shim support (AIRA-121 deferred it; only aira confine has a shim path)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["ci","confine","run"],"hold":false,"relations":[]}
---
Follow-up recorded by AIRA-121 (ci-shim mode for systemd/cgroup-unavailable
containers), taken as an explicit, written decision rather than a silent trim —
AIRA-121's plan section 5.7 and residual 5 name this fork.

AIRA-121 gave `aira confine` an advisory ci-shim launch path: no cgroup scope,
admission gated by an in-daemon RAM-budget ledger, honest `containment=advisory`
reporting, and process-GROUP signal forwarding. `aira run` did NOT get the same
treatment and REFUSES in shim mode with `E_RUN_SCOPE_UNAVAILABLE`, naming
`aira confine` as the shim-capable verb.

Why it was deferred rather than built:

- `Runner.Launch` carries a far larger surface than `confineWithDeps` — the
  project run ledger, telemetry, PTY, `--detach`, per-run scope caps, output
  capture, and the AIRA-20 descendant-escape attestation — and every one of the
  cgroup-derived facets is keyed on a scope that does not exist in shim mode.
- The deployment shape AIRA-121 exists for (a GCP Batch container running
  `aira confine -- make <gate>`) does not use `aira run` at all.
- Attempting the launch and failing at `backend.Create` deep inside
  `Runner.Launch` would leave a half-written run record behind, which is worse
  than refusing at the door.

What this ticket should build:

1. A shim launch path for `Runner.Launch` mirroring `confineShim`: no scope
   creation attempted up front, admission through the same ledger, `Setpgid` +
   process-group signal forwarding with the same deliver-then-grace-then-SIGKILL
   ordering, and the same owned-stdio treatment so a setsid'd descendant cannot
   block the supervisor's wait.
2. Every cgroup-derived facet on `RunRecord` reporting its ESTABLISHED
   unevaluated value, plus the `containment=advisory(...)` projection, so a run
   record from a shim box is distinguishable from a real-slice one at a glance
   rather than looking like a failed run.
3. A decision, written down, on `--detach` in shim mode: the detached supervisor
   currently mints its own scope id and binds it, which still works, but its
   `--status` reporting and the AIRA-72 reaper's assumptions must be re-checked
   against a mode with no scopes.
4. Tests in the same shape AIRA-121 used: a `newBackend`-panics harness proving
   no cgroup seam is reached, and a real end-to-end run in shim mode.

