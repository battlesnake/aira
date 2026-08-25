# `aira confine --list` / `--kill`: discoverable, ownership-guarded confine job control

Status: PLAN v3 (BUILDER-READY). v2 re-gate: **Fable GATE-PASS-WITH-NITS ("the build can start" —
all five v1 P1s verified closed against the code, exactly-once release re-confirmed, no new load-
bearing problem) + Sol substantively-closed**. v3 folds the 4 P2 nits: **(1)** explicitly hoist
`confineScopeID` to *before* `deps.admit` (a required build step — it is computed after admit today);
**(2)** the `registry ∪ scan` union dedups by canonical scope-id (one record; registry enriches
owner; conflicts fail closed) and `--kill` resolves owner from a *fresh* registry lookup, never a
`--list` snapshot; **(3)** two dispatch verbs derive two MCP tools; **(4)** `--owner` on both the
launch-form allowlist and the management parser, the kill holds one scope **dirfd** across
observe→kill→confirm, and `owner` values are charset-sanitised (accidental same-uid isolation, not
authentication). v1 was GATE-FAIL by Sol + DeepSeek + Fable (three lineages, strongly
convergent). Fable's **code-grounded** gate verified the architecture is sound and feasible (held-
admit-lease anchor, scope-path `cgroup.kill`, registry-as-view, exactly-once release already
structurally guaranteed) and that only five load-bearing P1s + P2 polish need folding — bounded
amendments, no redesign. v2 folds: **(a)** kill requires the scope observed *populated* (no empty-
scope "killed" fabrication in the mid-launch window); **(b)** a real project-less owner-identity
derivation (run's `project.WorktreeID` does not exist project-less); **(c)** an MCP project-
requirement carve-out for the two verbs; **(d)** daemon-up list/kill unions the registry with the
`aira.slice` cgroup scan (existence from the scan, owner from the registry); **(e)** `scope_id` path
validation. Plus the P2s. Owner-approved shape (2026-08-26): approach A + ownership-guarded kill.

## 1. Motivation (a real incident)

An operator ran `aira confine -- make merge-gate`. To stop it they `kill -INT`'d the **`bash`
wrapper** PID, not the **`aira confine` supervisor** PID; the supervisor survived, reparented to
`/init`, and kept advancing the job. The kill rule ("`kill` the `aira confine` process; it
`cgroup.kill`s its scope") was known and still misapplied, because **nothing surfaces which PID is
the supervisor**. A confine scope's cgroup dir is `.aira-CONFINE-<name>-<pid>-<nanotime36>` under
`aira.slice` (`confineScopeID`, `confine_linux.go:651`) — the supervisor PID is *encoded there* but
never shown. This makes confine jobs **discoverable** (`--list`) and gives a **robust, correctly-
targeted, ownership-guarded, honesty-checked kill** (`--kill`) that does not depend on eyeballing a PID.

## 2. Scope

- **In:** `aira confine --list` (read-only enumeration); `aira confine --kill <selector> [--steal]`;
  both on CLI **and** MCP via the dispatch table (with an MCP project carve-out, §6); a rewrite of
  the Skill's confine kill guidance + removal of stale `whale-run` references.
- **Out (unchanged):** `aira confine -- <argv>` launch stays CLI-only; #67 admission/estimate; the watchdog.
- **Not built:** no confine run-history persistence; no wildcard/"kill all"; no confine `--wait`;
  **no pre-launch cancellation token** (a kill in the millisecond mid-launch window honestly reports
  "not launched yet, retry" rather than cancelling a not-yet-placed child — §10).

## 3. Existence vs ownership — two distinct sources of truth (folds P1-d)

The v1 error was anchoring *existence* on the daemon admit connection. Corrected model:

- **Existence = the cgroup filesystem.** A confine job exists iff its `.aira-CONFINE-*` scope dir
  exists under `aira.slice`. This is authoritative and survives supervisor crash, flock-fallback
  launches (which never contact the daemon), and daemon restarts.
- **Ownership = the daemon's in-memory active-confine registry.** At admit the confine client sends
  `scope_id`, `name`, `owner` on the existing admit frame (which already carries `signature`/`pinned`
  from #67); `validateAdmitArgs` records them on the granted waiter (`admit.go:599`). The registry is
  a view over held confine leases, keyed by `scope_id`. It supplies **owner** (and the RAM
  reservation link), never existence.
- **`--list` and `--kill` operate on `registry ∪ cgroup-scan`, deduped by canonical scope-id.** The
  scan (existence) is the spine: one record per `.aira-CONFINE-*` dir. The registry *enriches* the
  matching record with `owner` (+ the reservation link). A scan-only scope (flock-fallback, orphaned
  supervisor, pre-restart) has `owner=unknown` and is treated as foreign (→ `--steal`). A registry
  entry with no scope dir yet (grant→`backend.Create` window) is `pending`/mid-launch (not a second
  record). Any registry/scan skew resolves conservatively (`unknown` → `--steal`, never an enabled
  kill). `--kill` resolves the scope's owner from a **fresh registry lookup at kill time**, never
  from a stale `--list` snapshot, so a caller's own just-registered job is not spuriously foreign.

### Owner identity (folds P1-b — run's `project.WorktreeID` does not exist project-less)

`aira run`'s `Owner` is `project.WorktreeID` (`internal/app/project.go:290`), which requires an
initialised aira project. Confine is project-less, so a naive reuse records `owner=unknown` almost
always and the guard collapses to always-`--steal` (no protection — the exact heavier path the owner
chose). Corrected derivation chain (first that resolves):
1. explicit `--owner <id>` flag on the launching `aira confine`;
2. `$AIRA_CONFINE_OWNER` environment variable;
3. `project.WorktreeID` when the launch cwd is inside an aira project;
4. honest `unknown` (guard degrades to `--steal`, never a fabricated identity).

The Skill/guide instruct long-lived agent sessions to `export AIRA_CONFINE_OWNER=<stable-session-id>`
so cross-session protection is real for the common (project-less) case. `--kill`'s caller owner
resolves by the same chain.

## 4. `aira confine --list` (SafetyRead, read-only)

Enumerate live confine scopes from `registry ∪ aira.slice cgroup-scan`. Columns: `name, owner
(or unknown), supervisor-pid, scope-id, populated (member count), rss (memory.current), age, cap
(memory.max)`. Honesty:
- `owner` is `unknown` for scan-only scopes (never fabricated to the caller's identity).
- A `populated=0` husk scope (supervisor died before `scope.Remove`) is shown with `populated=0`, not
  presented as a live job (folds P2-d).
- Per-field read failure ⇒ that field `unevaluated`, never 0. If the slice cgroup itself is
  unreadable ⇒ the whole result is `unevaluated` with a reason, never a fabricated empty list.

CLI (human table + `--json`) and its own derived MCP tool (the dispatch verb `confine-list` yields
one MCP tool; `confine-kill` yields a second — two verbs, two tools, per the dispatch-table
derivation, §6/§8). No selector. `--json` is permitted on the management form (folds P2-a — today
`main.go` rejects `--json` for all confine).

## 5. `aira confine --kill <selector> [--steal]` (SafetyExecute, Destructive)

Kill exactly one confine scope, honestly.

- **Selector** = exact `name` | `supervisor-pid` | `scope-id`, resolved against `registry ∪ scan`.
  Every selector must resolve to exactly one live scope; >1 → `E_SELECTOR_AMBIGUOUS` (list
  candidates; use scope-id). A reused/stale supervisor-pid matching >1 scope fails closed the same
  way. No match → `E_CONFINE_NOT_FOUND`. Names may contain `-`/`.`, so parse `<pid>-<t36>` from the
  **right** of the dir name (folds P2-b).
- **Ownership guard.** Compare the resolved scope's `owner` to the caller's owner; mismatch or
  `unknown` → `E_CONFINE_OWNER_UNVERIFIED{scope, owner, caller}` unless `--steal` (renamed from the
  v1 `E_CONFINE_KILL_UNVERIFIED` — it names a refusal, folds P2-f).
- **`scope_id` path validation (folds P1-e).** `validateAdmitArgs` accepts a `scope_id` only if it
  matches `^CONFINE-[A-Za-z0-9._-]+-[0-9]+-[0-9a-z]+$`; the kill opens only a direct child of the
  resolved slice dir named `.aira-<scope_id>` (dirfd-relative `openat` under the slice, `O_NOFOLLOW`,
  no `..`). A traversal-shaped id is rejected at admit, never reaching a kill. The kill holds **one
  opened scope dirfd across observe→kill→confirm** (populated-read, `cgroup.kill`, `waitEmpty` all
  relative to that fd) so no path re-resolution can retarget a recycled dir mid-operation.
- **The kill = `cgroup.kill` on the validated scope path** (reuse `linuxScope.Kill()`,
  `cgroup_linux.go:269`): atomic, whole-subtree, **reparenting-proof** — fixes the incident (the
  reparented supervisor's *child scope* still dies regardless of the supervisor's PID/parent). The
  supervisor lives outside the scope (`CgroupFD`, `confine_linux.go:499`), so `cgroup.kill` stops the
  workload; the supervisor's `cmd.Wait` unblocks, it runs normal teardown, closes the admit lease,
  and the reservation frees. No supervisor-PID signalling (no recycled-PID TOCTOU).
- **Populated-gate (folds P1-a — no empty-scope "killed" fabrication).** Before killing, observe the
  scope's population (`cgroup.events` `populated`, or non-empty `cgroup.procs`). Three honest outcomes:
  - **populated ≥ 1** → write `cgroup.kill`, then bounded `waitEmpty` (`cgroup_linux.go:292`) confirms
    `populated=0` → report **`killed`**; the daemon then releases the lease/registry entry (idempotent,
    §8). One child per scope ⇒ populated→kill→empty is race-free (the supervisor never places a second).
  - **populated = 0, scope dir exists** (grant→Create done but child not yet placed, i.e. mid-launch)
    → **do NOT kill, do NOT drop the lease**; report `not-launched` (`U_CONFINE_NOT_LAUNCHED`:
    "mid-launch or already exited; nothing to kill — retry"). Never `killed`.
  - **scope dir absent** (grant→`backend.Create` window, or already gone) → `E_CONFINE_NOT_FOUND`
    ("mid-launch or already gone; retry"), lease retained (folds P2-c).
- **Kill-outcome honesty under wedge/D-state.** `waitEmpty` is bounded; if the scope is not confirmed
  empty within the bound (e.g. a task stuck in uninterruptible sleep) → report
  `U_CONFINE_KILL_UNCONFIRMED` (unevaluated), never `killed`, and do not drop the lease.
- **Daemon down:** the CLI does the same populated-gated `cgroup.kill` directly on the scan-resolved
  scope. Ownership is unverifiable without the registry ⇒ **`--steal` REQUIRED**; without it →
  `E_CONFINE_OWNER_UNVERIFIED`. Read-only `--list` still works via the scan.

## 6. Faces + Skill (folds P1-c — MCP project carve-out)

- **Dispatch table (`core.Do`).** Add `confine-list` (`SafetyRead`) and `confine-kill`
  (`SafetyExecute` + `Destructive`) so CLI, MCP schemas, and the generated Skill/help derive from one
  source. Launch stays the pre-dispatch CLI interception; the management verbs get an explicit
  **project-less** path.
- **Routing.** Add both verbs to `Classify`'s `RouteClient` branch and to `StoreFreeCarved`
  (`routing.go`), and to the daemon's `serveConnection` special-case slot **before** the RouteClient
  project rejection (`server.go:466-498`, exactly where `confine-report` is handled), using the
  `reportConfinePeak`-style project-less raw-frame socket transport (`admission_linux.go:469`) with
  the cgroup-fs degrade when the daemon is unreachable.
- **MCP carve-out.** `runMCP` requires a project for every non-`init` verb (`mcp_project.go:45`) and
  the StoreFreeCarved client path still does `app.Discover`+validate (`dispatcher.go:193-218`). Carve
  `confine-list`/`confine-kill` out alongside the `init` carve-out (`mcp_project.go:37`) to the direct
  project-less handler. (`--steal` is a normal Destructive-confirmed argument; no ownership bypass by
  default — folds P2 MCP-steal note.)
- **Skill rewrite.** Replace the one-line hint with the real procedure: `aira confine --list` to find
  the scope, `aira confine --kill <name|pid|scope-id>` to stop it; **kill the scope, not the `bash`
  wrapper and not a PID picked by eye**; never `kill -9` the supervisor (orphans the child in the
  still-capped scope); never `systemctl --user stop` the shared slice; `export AIRA_CONFINE_OWNER` for
  cross-session ownership. **Delete every `whale-run` reference** (whale is retired).

## 7. Invariants

- A `--kill` touches only the single resolved scope (validated path), never the slice or another scope.
- `killed` is reported only after populated→`cgroup.kill`→bounded-empty confirmation; every other path
  is an honest non-`killed` outcome (`not-launched` / `not-found` / `unconfirmed` / `owner-unverified`).
- The #67 reservation release is exactly-once (already structurally guaranteed: `releaseAdmitWaiter`
  decrements only from `admitGranted`+`accounted` under `queue.mu`, `admit.go:504-521`); a daemon
  kill-drop plus the supervisor's later conn-close cannot double-fire.
- `--list`/`--kill` never fabricate: absent/unreadable ⇒ `unevaluated`/`unknown`, never 0/empty.
- Cross-session: with the daemon up and a resolved `owner`, session B cannot kill session A's job
  without `--steal`; with the daemon down (or `owner=unknown`), no kill proceeds without `--steal`.

## 8. Code map (verified by the Fable gate against the code)

- `internal/runner/confine.go`/`confine_linux.go`: add `Owner` to `ConfineRequest` (derivation chain
  §3); send `scope_id`+`name`+`owner` in the admit frame (`admitConfine` ~`:603`). **Required build
  step: hoist `confineScopeID` (`:651`) to BEFORE `deps.admit` (`:376`)** — today it is computed
  inside `backend.Create` (`:400`), *after* admit; it is a pure function of name+pid+nanotime, so the
  hoist is trivial but load-bearing (the admit frame must carry the scope-id). Child placed via
  `CgroupFD` (`:499`).
- `internal/daemon/admit.go`: `validateAdmitArgs` (`:599`) accepts + **validates** `scope_id`
  (canonical regex) and records `name`/`owner` on the granted waiter; the active-confine registry is a
  filtered view of held confine leases; `releaseAdmitWaiter` (`:504-521`) is the idempotent release.
- `internal/daemon/server.go`: new `confine-list`/`confine-kill` verbs in the `serveConnection`
  special-case slot before the RouteClient rejection (`:466-498`, `confine-report` precedent); the
  daemon-up list/kill unions the registry with an `aira.slice` scan.
- `internal/runner/cgroup_linux.go`: reuse `linuxScope.Kill()` (`:269`, `cgroup.kill`) + `waitEmpty`
  (`:292`) for the populated-gate + bounded empty attestation; a new project-less scan of
  `.aira-CONFINE-*` under the slice for existence + population.
- `internal/core/core.go`: two dispatch entries with `MCPTool`/`Safety`/`Destructive`; help/summary.
- `internal/core/routing.go`, `mcp_project.go`, `dispatcher.go`: `Classify` RouteClient +
  `StoreFreeCarved` membership; MCP project carve-out.
- `cmd/aira/main.go`: parse `--list` / `--kill <selector>` / `--steal` / `--owner` for `confine`
  (management form has no `--` delimiter; allow `--json`; the management branch precedes the
  `--`-mandatory check `:519-529`). Add `--owner` to the **launch-form** option allowlist (`:537`)
  too, not only the management parser. `owner` values are charset-sanitised (same allowlist as the
  scope-id charset) on both forms.
- `internal/core/skill.go` + `internal/install/install.go` (embedded guide): kill-guidance rewrite;
  drop `whale-run`.
- New stable codes: `E_CONFINE_OWNER_UNVERIFIED`, `E_CONFINE_NOT_FOUND`, `U_CONFINE_NOT_LAUNCHED`,
  `U_CONFINE_KILL_UNCONFIRMED` (+ reuse `E_SELECTOR_AMBIGUOUS`). Exit codes assigned in the code table.

## 9. Tests (TDD; pure via seams; real-cgroup under `aira confine`; proven RED)

- **Ownership guard:** session B `--kill` of A's scope → `E_CONFINE_OWNER_UNVERIFIED`; `--steal` → killed.
  Owner-derivation unit tests (flag > env > WorktreeID > unknown).
- **Mid-launch no-fabrication (folds P1-a, the load-bearing test):** hold the launch at a seam between
  `backend.Create` and `deps.start`; a kill there observes `populated=0` and reports `not-launched`
  (NOT `killed`), does not drop the lease, and the job still launches and completes. A kill after
  placement observes `populated≥1` and truly kills.
- **Reparented-supervisor kill (the incident, real-cgroup):** a scope whose supervisor is reparented /
  launcher gone is killed by scope-id; assert workload PIDs die.
- **Scan-union + dedup (folds P1-d):** a flock-fallback / scan-only scope (no registry entry) appears
  in daemon-up `--list` with `owner=unknown` and is killable only with `--steal`; a scope present in
  BOTH registry and scan appears **once** with the registry's owner (no double-count); `--kill`
  resolves owner from a fresh registry lookup (a just-registered own job is killable without `--steal`).
- **scope_id validation (folds P1-e):** an admit frame with a traversal-shaped `scope_id`
  (`../`, `/`, non-canonical) is rejected at `validateAdmitArgs`; no kill path is ever composed from it.
- **Daemon-down degrade:** `--list` via scan (`owner=unknown`); `--kill` without `--steal` →
  `E_CONFINE_OWNER_UNVERIFIED`; with `--steal` → killed.
- **Selector:** ambiguous `name` and ambiguous reused-pid → `E_SELECTOR_AMBIGUOUS`; unknown → `E_CONFINE_NOT_FOUND`.
- **List honesty:** unreadable slice → whole result `unevaluated`; per-scope RSS read failure → that
  field `unevaluated`; a `populated=0` husk shows `populated=0`, not "live".
- **Kill honesty:** `cgroup.kill` write error or `waitEmpty` timeout → `U_CONFINE_KILL_UNCONFIRMED`,
  never `killed`.
- **Accounting (folds P2-e):** a killed scope releases its #67 reservation exactly once — fire BOTH
  release paths (daemon kill-drop, then the supervisor's conn close) and assert `outstanding`/
  `outstandingJobs` return to baseline once.
- **Registry lifetime:** entry on grant, gone on lease close; startup rescan surfaces a pre-existing
  scope with `owner=unknown`.
- **Faces:** the two MCP tools (from `confine-list`/`confine-kill`) dispatch **outside a project**
  (carve-out), and an e2e test proves the carve-out still enforces ownership, the Destructive
  marking, and `--steal` — i.e. it bypasses only project discovery, not any safety check. Skill
  contains the new guidance and **no** `whale-run`.
- Regression: `aira run`/`run-kill` ownership + #67 admission unchanged; `go build/vet ./... &&
  go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/ ./internal/core/ -race` green; `make test`
  under `aira confine`.

## 10. Residual risk (stated, not silent)

- **No pre-launch cancellation:** a kill in the millisecond mid-launch window (scope created, child
  not yet placed) honestly reports `not-launched`/retry rather than cancelling the not-yet-placed
  child; the operator retries once the workload is running. A cancellation token the supervisor checks
  before `CgroupFD` placement is a deferred enhancement; the incident (a running job) is unaffected.
- Ownership is in-memory (daemon-tracked); a daemon restart makes live jobs `owner=unknown` (scan-
  only) → `--steal` needed (honest, consistent with #67 §8 reservation drop).
- A wedged supervisor that never reaps its killed child lingers outside the scope doing nothing; the
  daemon frees its lease on the confirmed kill so no reservation leaks.
- `owner` is a cooperative, caller-supplied, **spoofable** session id (like `run`), not an OS
  credential: the guard is **accidental same-uid isolation, not authentication**. It prevents an agent
  from *accidentally* killing another session's job, not a hostile same-uid actor (out of scope, as
  for `run`). `owner` values are charset-sanitised (no injection into the scope/kill path). An
  optional `SO_PEERCRED` cross-check of the admit-conn PID is noted for a future hardening.
