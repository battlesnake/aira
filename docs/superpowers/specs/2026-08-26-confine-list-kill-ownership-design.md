# `aira confine --list` / `--kill`: discoverable, ownership-guarded confine job control

Status: PLAN v1 (for Sol + Fable plan-gate). Owner-approved shape (2026-08-26): approach A
(`--list` + `--kill` verbs on CLI **and** MCP + Skill rewrite) with **ownership-guarded kill
mirroring `run-kill`** (foreign-owner refusal + `--steal`). Owner explicitly accepted the heavier
ownership path over a simpler explicit-selector kill.

## 1. Motivation (a real incident)

An operator ran `aira confine -- make merge-gate`. To stop it they `kill -INT`'d PID `1620505` —
the **`bash` wrapper** — not `1620527`, the **`aira confine` supervisor**. The supervisor survived,
reparented to `/init`, and kept advancing the job. The kill-discipline rule ("`kill <PID>` the
`aira confine` process you started; it catches the signal and `cgroup.kill`s its scope") was known
and still misapplied, because **nothing surfaces which PID is the supervisor**. A confine scope's
cgroup dir is `.aira-CONFINE-<name>-<pid>-<nanotime36>` under `aira.slice` (`confineScopeID`,
`confine_linux.go:651`) — the supervisor PID is *encoded there* but never shown. The fix makes
confine jobs **discoverable** (`--list`) and gives a **robust, correctly-targeted, ownership-guarded
kill** (`--kill`) that does not depend on the operator identifying a PID by eye.

## 2. Scope

- **In:** `aira confine --list` (read-only enumeration); `aira confine --kill <selector> [--steal]`
  (robust single-scope kill with a cross-session ownership guard); both verbs on the CLI **and** the
  MCP face via the `core.Do` dispatch table; a rewrite of the Skill's confine kill guidance and the
  removal of stale `whale-run` references.
- **Out (unchanged):** `aira confine -- <argv>` launch stays CLI-only (a shell prefix has no
  meaningful MCP launch form); the admission/estimate machinery from #67; the watchdog.
- **Not built:** no confine run-history persistence; no wildcard/"kill all"; no confine `--wait`.

## 3. Ownership model — daemon-tracked, mirrors `run` (`Config.Owner` / `ForeignOwnerError`)

`aira run` stamps a caller-supplied **session-identity string** `Owner` (`runner.Config.Owner`) and
`run-kill` refuses a foreign owner (`ForeignOwnerError{Owner, CallerOwner}`) unless
`killPolicy.Steal`. Confine mirrors this exactly, keyed to the **daemon** (confine is project-less
and has no store record, but it already holds a per-job admit-lease connection to the daemon for the
job's lifetime — #67 daemon-lease-only hold).

1. **Register at admit.** Extend the confine admit frame (already carries `signature`/`pinned` from
   #67) with `scope_id`, `name`, and `owner`. The client computes `confineScopeID` up front and
   sends it. `validateAdmitArgs` accepts + records them on the granted waiter. The **held admit
   connection is the lifetime anchor**: while it is open the job is live; when it closes (teardown or
   supervisor crash — kernel closes it) the entry is gone. No new persistence.
   - `owner` derives from the same identity `aira run` uses (env/config-provided session id); empty
     owner ⇒ recorded as `unknown` (never fabricated).
2. **Active-confine registry.** The daemon exposes the set of granted confine waiters that carry a
   `scope_id` as an in-memory registry keyed by `scope_id` (name, owner, supervisor pid, slice,
   scope path, enqueued-at). This is a *view* over existing held leases, not a second lifetime.
3. **Startup rescan (honest degrade).** On daemon start, rescan `aira.slice` for `.aira-CONFINE-*`
   dirs (precedent: #47 startup registry discovery) and surface any pre-restart scopes with
   `owner=unknown` (their launching daemon is gone) so `--list` still shows them and `--kill`
   requires `--steal`. A daemon restart already drops reservations (#67 §8); dropping ownership is
   consistent.

## 4. `aira confine --list` (SafetyRead, read-only)

Enumerate live confine scopes. Columns: `name, owner, supervisor-pid, scope-id, rss (memory.current),
age, cap (memory.max)`. Two honest sources:
- **Daemon up:** the active-confine registry (authoritative owner). RSS/cap read live from each scope
  path; a per-scope read failure yields `unevaluated` for that field, never 0.
- **Daemon down / unreachable:** direct `aira.slice` cgroup-fs scan for `.aira-CONFINE-*`
  (`owner=unknown`, supervisor-pid parsed from the scope-id). If the slice cgroup itself cannot be
  read → the whole result is `unevaluated` with a reason, never a fabricated empty list.

CLI (human table + `--json`) and MCP (`aira_confine`, operation `list`). No selector.

## 5. `aira confine --kill <selector> [--steal]` (SafetyExecute, Destructive)

Kill exactly one confine scope.
- **Selector** = exact `name` | `supervisor-pid` | `scope-id`. Never a wildcard. A `name` matching
  >1 live scope → `E_SELECTOR_AMBIGUOUS` listing the candidates (kill by scope-id or pid instead). No
  match → `E_CONFINE_NOT_FOUND`.
- **Ownership guard.** Daemon resolves the scope, compares its recorded `owner` to the caller's
  `owner`; a mismatch → `E_CONFINE_FOREIGN_OWNER{scope, owner, caller}` unless `--steal`. `unknown`
  owner (post-restart / cgroup-scan) is treated as foreign → `--steal` required.
- **The kill itself = `cgroup.kill` on the scope path** (reuse `linuxScope.Kill()`,
  `cgroup_linux.go:269`): atomic, whole-subtree, **reparenting-proof** — this is what fixes the
  incident (the reparented-to-`/init` supervisor's *child scope* still dies regardless of the
  supervisor's PID or parent). Only the child tree is in the scope (the supervisor lives outside via
  `CgroupFD`), so `cgroup.kill` stops the actual workload; the supervisor observes its dead child,
  runs its normal teardown, and exits — which closes the admit lease and frees the reservation. The
  daemon also drops the registry entry / releases the lease on a successful kill so the reservation
  is freed promptly even if the supervisor lingers. **No supervisor-PID signalling** (avoids all
  recycled-PID TOCTOU); the scope path is a stable, PID-free kill target.
- **Daemon down:** the CLI writes the scope's `cgroup.kill` directly (scope found via cgroup-fs
  scan). Ownership is unverifiable without the daemon ⇒ **`--steal` is REQUIRED** (honest: never a
  silent unguarded cross-session kill). Without `--steal` → `E_CONFINE_KILL_UNVERIFIED` naming the
  scope and instructing `--steal`.
- **Honesty of outcome:** report `killed` only from a confirmed `cgroup.kill` write + scope-empty
  observation; a write error is `unevaluated`/error, never a fabricated "killed". Reuse the runner's
  kill→empty attestation pattern.

## 6. Faces + Skill

- **Dispatch table (`core.Do`).** Add the two verbs so CLI, MCP schemas, and the generated Skill/help
  all derive from one source. Launch (`confine -- …`) stays the pre-dispatch CLI interception; the
  management verbs route through the daemon (`RouteClient`) with the cgroup-fs degrade. `--list` =
  `SafetyRead`; `--kill` = `SafetyExecute` + `Destructive` (so the TUI/confirmation path treats it
  as destructive).
- **Skill rewrite.** Replace the one-line kill hint with the real procedure: `aira confine --list` to
  find the scope, `aira confine --kill <name|pid|scope-id>` to stop it; **kill the scope, not the
  `bash` wrapper and not a PID picked by eye**; never `kill -9` the supervisor (orphans the child in
  the still-capped scope); never `systemctl --user stop` the shared slice. **Delete every `whale-run`
  reference** (whale is retired) from `SKILL.md`/guide generation.

## 7. Invariants

- A `--kill` never touches the slice or any scope other than the single resolved one.
- The reservation/ledger accounting from #67 is preserved: a killed scope's lease is released exactly
  once (no double-release, no leak).
- `--list` and `--kill` never fabricate: absent/unreadable ⇒ `unevaluated`/`unknown`, never 0/empty.
- Cross-session: session B cannot kill session A's confine job without `--steal`; with the daemon
  down, no kill proceeds without `--steal`.

## 8. Code map (verified seams)

- `internal/runner/confine.go` / `confine_linux.go`: send `scope_id`+`name`+`owner` in the admit
  frame (`admitConfine`, ~`:603`); `confineScopeID` (`:651`) computed before admit. `Owner` plumbed
  from `ConfineRequest` (new field) set by the CLI.
- `internal/daemon/admit.go`: `validateAdmitArgs` accepts `scope_id`/`name`/`owner`; the granted
  waiter records them; expose the active-confine registry (a filtered view of held confine leases).
- `internal/daemon/server.go`: new `confine-list` / `confine-kill` verbs in `serveConnection`
  (precedent: `confine-report`, `server.go:466`); startup `aira.slice` rescan (precedent #47).
- `internal/runner/cgroup_linux.go`: reuse `linuxScope.Kill()` (`:269`, `cgroup.kill`) + the
  scope-empty attestation for the kill + honest outcome.
- `internal/core/core.go`: two dispatch-table entries (`confine-list`, `confine-kill`) with
  `MCPTool`/`Safety`/`Destructive`; help/summary entries.
- `cmd/aira/main.go`: parse `--list` / `--kill <selector>` / `--steal` for the `confine` verb
  (management form has no `--` launch delimiter); route to the daemon with the cgroup-fs degrade.
- `internal/core/skill.go` + `internal/install/install.go` (embedded guide): kill-guidance rewrite;
  drop `whale-run`.
- New stable codes: `E_CONFINE_FOREIGN_OWNER`, `E_CONFINE_NOT_FOUND`, `E_CONFINE_KILL_UNVERIFIED`
  (+ reuse `E_SELECTOR_AMBIGUOUS`). Exit codes assigned in the code table.

## 9. Tests (TDD; pure via seams; real-cgroup under `aira confine`; proven RED)

- **Ownership guard:** session B `--kill` of A's scope → `E_CONFINE_FOREIGN_OWNER`; with `--steal` →
  killed. Mirrors `TestForeignKillRefusalIsAtomicAndNonDestructive`.
- **Reparented-supervisor kill (the incident, real-cgroup):** a confine scope whose supervisor is
  reparented / whose launcher shell is gone is still killed by `scope-id` — `cgroup.kill` empties the
  scope; assert the workload PIDs die.
- **Daemon-down degrade:** `--list` falls back to the cgroup-fs scan (`owner=unknown`); `--kill`
  without `--steal` → `E_CONFINE_KILL_UNVERIFIED`; with `--steal` → killed.
- **Selector:** ambiguous `name` → `E_SELECTOR_AMBIGUOUS` (lists candidates); unknown → `E_CONFINE_NOT_FOUND`.
- **List honesty:** unreadable slice cgroup → `unevaluated` (not empty); a per-scope RSS read failure
  → that field `unevaluated`, others intact.
- **Accounting:** a killed scope releases its reservation exactly once (no leak, no double-release) —
  assert daemon `outstanding`/`outstandingJobs` return to baseline.
- **Kill outcome honesty:** a `cgroup.kill` write error → error/`unevaluated`, never "killed".
- **Registry lifetime:** entry appears on grant, disappears on lease close; startup rescan surfaces a
  pre-existing scope with `owner=unknown`.
- **Faces:** MCP `aira_confine` list/kill schema present; Skill contains the new guidance and **no**
  `whale-run`.
- Regression: `aira run`/`run-kill` ownership + #67 admission unchanged; `go build/vet ./... &&
  go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/ ./internal/core/ -race` green; `make
  test` under `aira confine`.

## 10. Residual risk (stated, not silent)

- Ownership is in-memory (daemon-tracked); a daemon restart makes live jobs `owner=unknown` →
  `--steal` needed to kill them (honest, consistent with #67 §8 reservation drop).
- A wedged supervisor that never reaps its killed child lingers outside the scope doing nothing; the
  daemon frees its lease on kill so no reservation leaks. Not chased further.
- `owner` is a cooperative session id (like `run`), not an OS credential; same trust model as
  `run-kill`. All callers are the same uid; the guard prevents *accidental* cross-session kills, not a
  hostile one (out of scope, as for `run`).
