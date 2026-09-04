---
{"schema":1,"id":"AIRA-85","project":"aira","title":"The detached supervisor is a third production writer to state.db, bypassing the daemon single-writer relay","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["crash-recovery","daemon","detach","store"],"hold":false,"relations":[]}
---
PR #12 finding **B5** / plan candidate **53**, filed by the simplification programme's Phase 0
(plan §4.3, §5.1, §6). Source-verified against master `22cedd6`.

## The defect

The DB-owning daemon is meant to be the single writer for `state.db`. Two faces honour that:
the CLI relays through `cmd/aira/write_relay_store.go`, and the daemon writes directly.
The detached supervisor is a **third** writer and does neither:

- `cmd/aira/main.go:387 runSupervisor` calls `app.OpenWithDiagnostics(ctx, ".", ...)`,
  opening its own store handle.
- On a terminal detached run it settles telemetry via
  `core.NewWithRunner(s, project.Runner).WireAndSettleDetached(...)`, which reaches
  `c.store.AddTestReport` (`internal/core/run_wiring.go:261`, retried at `:272`) and
  `c.store.AddComputeEvent` (`:391`) — direct SQLite writes from a process the daemon does
  not know about.

## Why it is worth doing while it is small

`supervisor_leases` has **zero rows machine-wide** and `aira run --detach` is unused today, so
this is a ~50-line change against a cold path. **AIRA-22** (`confine --detach`) is a plan to
graft exactly this machinery onto `confine` — the launch path that IS used constantly. Once
AIRA-22 lands, the same change is a rewrite against the main path, under load, with real
concurrent writers. PR #12 makes the point directly: "Zero `supervisor_leases` rows suggest
detach is unused today, which is the moment to fix it."

It also closes the mutual deferral between PR #12's D5 and D7b.

## Direction

Fold the supervisor's `AddTestReport` / `AddComputeEvent` writes through the existing
store-op relay (`internal/daemon/storeops.go`, `cmd/aira/write_relay_store.go`) so the daemon
stays the only writer, with the supervisor's own store handle reserved for reads it genuinely
cannot relay — if any remain.

## Rigor: Tier A, not B

The plan raises this on review: the ~50 lines decide whether a *completed* detached run's
evidence is durably recorded. That is settlement on a crash-recovery-shaped path, which
`CLAUDE.md` names as full-two-loop work. Scheduled in the programme's Phase 5a
(it is detach/runner territory, not store territory).

## Resolution (backlog remediation plan §4, executed 2026-09-05)

`runSupervisor` no longer opens a read-write handle at all. It builds the project's local
execution dependencies with `app.OpenWithoutStore` and takes its state.db view from
`supervisorTelemetryStore` (`cmd/aira/supervisor_relay_store.go`): a `mode=ro` /
`query_only` connection wrapped in the existing `writeRelayStore`, whose
`AddTestReport`/`AddComputeEvent` overrides go to the daemon's `add-test-report` /
`add-compute-event` store-ops. The daemon-side halves already existed
(`internal/daemon/storeops.go`), so this is wiring, not new protocol — no
`ProtocolVersion` bump.

Two consequences are behaviour changes, recorded rather than hidden:

* **The supervisor now refuses when state.db is absent instead of creating it.**
  `store.Open` used to create (and migrate, and register) the machine database from a
  detached background process the daemon knows nothing about. `mode=ro` cannot.
  The reachability argument, stated precisely because a looser version of it is wrong:
  `__supervise` is a hidden subcommand spawned from exactly one site
  (`internal/runner/detach_linux.go:47,51`, `exec.Command("/proc/self/exe", "__supervise", …)`),
  reached only from `core.Do`'s `run --detach` branch (`internal/core/core.go:1608`).
  `run` is client-routed, and `daemonDispatcher.dispatchClient` runs
  `prepareClientProject` — an `ensure-scope` store-op against the daemon — *before* any
  branch, so a supervisor can only exist if the daemon answered `ensure-scope`, which it
  cannot do without having `state.db` open. Note this is **not** the same as "the CLI
  opened state.db read-only first": a `run --detach` with no `--report`/`--tool` is
  `StoreFreeCarved` (`internal/core/routing.go:99-110`) and takes the `core.StoreGuard()`
  branch, never opening the database client-side. The daemon's own open is the guarantee.
* **With no reachable daemon, the settlement records nothing and says so.** Both writes
  fail `E_DAEMON_UNAVAILABLE`, the wiring is marked incomplete, and the run settles
  `telemetry=incomplete` with per-action warnings. Previously the direct write succeeded.
  Losing the evidence *and* saying so is the honest outcome; silently being a third writer
  is not. The supervisor deliberately does not start or replace a daemon to avoid this —
  that is the shared-blast-radius action AIRA-83 removed.

## What proves it, and what does not

`internal/runner` imports `internal/store` **zero times**, so `SuperviseRequest`, the
detached launch, `RecordAuxTelemetry` and the supervisor-lease manager structurally cannot
write `state.db` — the lease already relays over the daemon socket. Combined with
`internal/core/run_wiring.go` having exactly three `c.store.` call sites across two methods
(`AddTestReport` ×2, `AddComputeEvent`), those two writes really were the whole surface.

Verified end to end, isolated from the shared machine daemon (private `XDG_STATE_HOME` +
`XDG_RUNTIME_DIR`, a private daemon, the worktree binary — the installed binary and
`aira-daemon.service` untouched): a real `aira run --detach --report go-json --tool gpt`
spawned a real `__supervise` process, which settled `telemetry: complete` with refs
`[TR-1, CE-1]`, and both rows are present in that private `state.db`
(`test_reports`=1, `test_report_results`=1, `compute_events`=1, `parser_complete=1`).
Since the supervisor's own connection is `mode=ro`, the daemon is the only thing that
could have written them.

**Coverage gaps, written down rather than left silent.** No automated test spawns a real
`__supervise` OS process (the smoke above is a recorded manual reproduction, not a
committed test); the `runSupervisor` entry point is covered only up to its store-open
refusal. Build review also raised, and this accepts without new code: a stale or
project-mismatched socket is already refused by the daemon's `scope.StateID` guard
(`internal/daemon/server.go:564`) plus `storeForScope`'s canonical-identity check, and the
large-report framing path is the same one the foreground CLI `run --report` already uses in
production, so neither is new risk from this change.

## Independent verification (2026-09-05, separate detached worktree at the PR head)

Re-run rather than trusted from the build report. Exact results:

* `go build ./...` exit 0; `go vet ./...` exit 0; `gofmt -l cmd internal` empty.
* `go test ./cmd/aira/ -count=1` exit 0 (`ok aira/cmd/aira 67.167s`); the seven new
  `TestSupervisor*` tests all PASS under `-v` (the build report and PR body say "six tests …
  four against a real in-process daemon" — it is **seven** test functions, **three** of which
  start a real in-process daemon; a miscount in the prose, not in the tests).
* `go test ./... -count=1` exit 0 on a re-run. An earlier full run failed only
  `internal/runner` — `TestScopeMembershipEventsDeliversModifyAndReleasesFD` (inotify
  `EMFILE`) and `TestRealCgroupExplicitEmptyEnvironmentIsExactAndDigested` (the known
  real-cgroup load flake). Both are environmental and confirmed unrelated: this branch
  changes no file under `internal/runner`, and `fs.inotify.max_user_instances` was **128**
  with **119** instances live machine-wide while that run executed (raised to 4096
  afterwards, at which point the same test passes on this branch and on master).
* Mutation testing re-done independently, all three mutants killed: reverting `runSupervisor`
  to `app.OpenWithDiagnostics` → killed by `TestSupervisorRefusesRatherThanCreatingStateDBItself`;
  swapping `store.OpenReadOnly` for a read-write open → killed by that test **and** by
  `…RelaysTelemetryWritesAndCannotWriteStateDBItself`; production constructor passing a nil
  relay → killed by `TestSupervisorDetachedSettlementWritesThroughTheDaemonRelay`.
* One surviving mutant, inside the coverage gap already declared above: deleting
  `telemetryStore.SetRunner(project.Runner)` **and**
  `project.Runner.SetSupervisorLeaseReader(telemetryStore.SupervisorLeaseLive)` from
  `runSupervisor` leaves the package green. Both lines restore wiring that
  `app.OpenWithDiagnostics` used to perform internally (`internal/app/project.go:253-254`), so
  their presence is correct and no regression exists; they are simply unreachable for tests
  that stop at the store-open refusal. Recorded, not fixed — closing it needs the real
  `__supervise` process test this ticket already defers.
* Source claims spot-checked and confirmed: `internal/runner` contains zero references to
  `internal/store`; `run_wiring.go` has exactly the three `c.store.` sites; the `:272` retry
  fires only on a deterministic `E_TESTREPORT_INVALID`, never on `U_DAEMON_OUTCOME_UNKNOWN`,
  so relaying cannot duplicate an ambiguous append; `daemon.ExchangeStoreOp` dials only;
  `prepareClientProject`'s `ensure-scope` really does precede the `StoreFreeCarved` branch
  (`cmd/aira/dispatcher.go:271-277`), so the reachability argument holds.

## Residual structural gap — named, not closed

**Nothing enforces single-writer beyond convention.** Any future code path may still call
`store.Open` and write directly. The remediation plan (§5 item 3, §6) considered and
**dropped** a mechanical `store.Open`-outside-`internal/daemon` lint, because the daemon
actually opens via `store.OpenDB` and because such a rule would not have caught this defect
anyway — the supervisor opened the DB *correctly*; the defect was writing afterwards. The
residual guard is code review. A runtime single-writer assertion (an flock, or a daemon-side
writer identity) stays a follow-up; it is out of this ticket's scope.

**Correction, independent verification pass:** the plan's *second* reason for dropping that
lint — "`app.OpenWithDiagnostics` → `store.Open` is the correct bootstrap for the CLI's
daemon-less fallback, so the rule would fail day one against a legitimate caller" — does not
hold against current source, and the commit message and PR body repeat it unchecked from the
plan. There is no production daemon-less CLI fallback: after this change the only non-test
caller of `app.OpenWithDiagnostics` is `inProcessDispatcher` (`cmd/aira/dispatcher.go:658`),
whose own comment declares it "injected by tests … never a production fallback", and it is
constructed only in `_test.go` files; `app.Init`'s `store.Open` (`internal/app/project.go:385`)
is likewise reached only from that test-only substrate, the daemon using `app.PrepareInit`
instead. So the lint would today fail only against test-only callers. The *decision* to drop
it stands on the reason kept above — it would not have caught this defect — not on that
stale one.
