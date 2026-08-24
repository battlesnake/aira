# `aira install` — stand up the AIRA daemon service (service-authoritative)

Status: PLAN v2 — re-cut after Sol + Fable GATE-FAIL on v1. Owner chose the
**service-authoritative (clients defer)** convergence (2026-08-24). Extends the #55
`aira install` (aira.slice + anchor, merged `67eda21`; live). Milestone #62. Successor
to the whale→AIRA migration (#60 redirect + #61 skill/MCP).

## 1. Why + the convergence problem (v1's fatal flaw, corrected)

The AIRA daemon (`aira daemon serve` → `daemon.NewServer(paths).Serve(ctx)`,
`daemon_command.go:30-33`) owns `state.db` and runs the periodic subsystems incl. the
#59 memory watchdog. It must run **continuously** for the watchdog to be live — today it
only runs **on-demand**: any `aira` command auto-starts one by forking `/proc/self/exe
daemon` (`dispatcher.go:78`) with the client's env (watchdog **off**), and it **never
idle-exits** (`Serve` runs until signal — v1's "exits when idle" claim was wrong, Fable).

A single-instance `flock(LOCK_EX|LOCK_NB)` (`server.go:169-174`) permits only ONE daemon.
So a naive systemd service **competes** with the client fork for that lock. Both gates
found this breaks two ways: at install an incumbent client-daemon holds the lock → the
service `ErrAlreadyRunning`→exits→`Restart=always` start-limits to **`failed`** while a
socket probe false-passes against the wrong (watchdog-off) daemon; and at runtime a
client can win the lock during a restart gap and spawn a watchdog-off daemon, silently
reverting the deliverable.

**Resolution (owner-chosen): the systemd service is the single source of truth; clients
defer to it.** Once `aira-daemon.service` is installed, the client auto-start seam starts
the *service* instead of forking its own daemon — so no new incumbents are ever created,
and the only incumbent to displace is the one that predates the install.

## 2. Client-defer — the one code seam (`dispatcher.go`)

`spawnDaemon()` (`dispatcher.go:74-88`) is the sole fork point (already injectable via
`d.spawn` for tests). Change it: if `aira-daemon.service` is **installed and enabled**
(detected via `systemctl --user is-enabled aira-daemon.service` == `enabled`, or the
managed unit file present), run **`systemctl --user start aira-daemon.service`**
(idempotent — no-op if already running; blocks until active) and hand back a `childResult`
channel that reports only a `systemctl start` failure; the existing socket-poll in
`exchangeOrStartUsing` then connects once the service binds the socket. If the unit is
NOT installed, keep today's `/proc/self/exe daemon` fork verbatim (fallback for
un-installed machines / tests). This is the whole convergence change: no takeover-serve,
no lock-fighting — deferring clients never create a competing daemon, so after install
the service daemon is the only one, and every client connects to it.

Detection must be cheap + not itself auto-start (a plain `systemctl` query, no socket
dial). `d.spawn` injection stays so tests drive both branches offline.

## 3. The embedded daemon unit

New `assets/aira-daemon.service.in`, rendered via `strings.ReplaceAll` (mirroring the
anchor), placeholders `@AIRABIN@` (← `os.Executable()`), `@WATCHDOG_MODE@`,
`@WATCHDOG_INTERVAL@`, `@STATEHOME@`:

```ini
[Unit]
Description=AIRA daemon — owns state.db and runs the memory watchdog, reaper, reconciler.
# aira-managed: aira-daemon.service
StartLimitIntervalSec=0

[Service]
ExecStart=@AIRABIN@ daemon serve
Environment=AIRA_DAEMON_WATCHDOG_MODE=@WATCHDOG_MODE@
Environment=AIRA_DAEMON_WATCHDOG_INTERVAL=@WATCHDOG_INTERVAL@
Environment=XDG_STATE_HOME=@STATEHOME@
MemoryAccounting=yes
MemoryMax=1G
MemoryHigh=768M
TasksMax=512
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
```

- **NOT `Slice=aira.slice`** (Fable/Sol): the daemon manages aira.slice + is the watchdog's
  own home; an aira.slice group-OOM must not kill it (`watchdog.go:296` already excludes
  the daemon from its own victims, and a systemd-managed daemon is no longer a
  claude-descendant so whale-watchdog won't pick it either — so it is otherwise the one
  unbounded process; hence its **own** `MemoryMax=1G`/`TasksMax`, single process, no
  oom.group needed).
- **`StartLimitIntervalSec=0`** — no start-limit trap.
- **`Environment=XDG_STATE_HOME=@STATEHOME@`** baked from the install-time resolved value
  (`stateID = sha256(stateHome)` keys the socket, `paths.go:163-164`) — so the service and
  clients resolve the SAME `state.db` + socket even if a shell exports a different
  `XDG_STATE_HOME` than the user manager. Install **verifies** the current resolved state
  home matches the baked value and warns on divergence.

## 4. Install mechanics (mirror the #55 anchor; §7 hardening)

Extend `internal/install` — `defaultDaemonUnit = "aira-daemon.service"` + a `daemonUnit`
field on `installDeps` (parametrised like `sliceUnit`/`anchorUnit`, so real-systemd tests
use a throwaway name). After the anchor is up + delegation verified:
1. **Stop any incumbent daemon** best-effort (`daemon.Stop(paths)`, `paths.go:268-284`) so
   the service's `serve` acquires the lock cleanly (no `ErrAlreadyRunning` flap).
2. Render + marker-guarded atomic `publishManagedUnit` the daemon unit; `daemon-reload`.
3. `systemctl --user enable --now aira-daemon.service`.
4. **`loginctl enable-linger <user>`** (best-effort; parity with whale-watchdog) so the
   daemon survives logout/reboot. If it fails (no privilege), warn honestly — the daemon
   then only runs while the user session is up.
5. **Honest reachability (NOT socket-answer-alone):** require the unit `ActiveState=active`
   AND `SubState=running` (not `activating`/`failed`), AND `systemctl --user show -p
   MainPID aira-daemon.service` **equals** the running daemon's lock owner PID from `aira
   daemon status` (`Lock.PID`); mismatch/unreadable → `E_INSTALL_UNAVAILABLE`, never a fake
   "installed". A crash-looping daemon (Restart=always masking a bad ExecStart) thus fails
   the install honestly.
- **Watchdog flag:** `aira install --watchdog=off|observe|enforce` (default **observe** —
  runs the watchdog, logs WOULD-SIGKILL, signals nothing; safe beside the live
  whale-watchdog, verified signal-free at `watchdog.go:317-321`) + `--watchdog-interval`
  (default 2s, `[1s,30s)`). `enforce` is honoured but the #59 interlock degrades it to
  observe while whale-watchdog is active — safe to set early. Invalid → `E_INSTALL_ARGUMENT_INVALID`.
- **Idempotence/convergence:** content-compare the unit to decide rewrite; a changed
  `--watchdog` re-renders + `daemon-reload` + **`systemctl restart aira-daemon.service`**
  (a service needs restart to pick up changed Environment; the SIGTERM→drain≤10s path
  `server.go:294-320` + crash-safe WAL make restart clean). `--dry-run` prints the unit +
  planned actions, writes/starts nothing.
- `--status` gains a daemon facet: unit present (+marker); active+running; watchdog mode
  (live `Environment`); reachable (MainPID-tied); linger on/off.

## 5. Single-instance + restart safety (verified, Fable)

Flock (`server.go:169-174`) gives single-instance. With clients deferring (§2), the service
is the only starter after install, so no lock contention. SIGTERM→`NotifyContext`
(`daemon_command.go:31`)→drain ≤10s (`server.go:294-320`); drain-timeout `os.Exit(1)`
leaves a stale socket that the next instance clears (`server.go:197-199`); SQLite WAL is
crash-safe → `systemctl restart` won't corrupt `state.db`.

## 6. Tests (TDD; real-systemd gated, env-isolated throwaway unit)

- Pure/seam: `--watchdog` parse; render substitution incl. `@STATEHOME@`; the daemon unit
  is **not** `Slice=` anything (discriminating vs an anchor copy-paste) and **has**
  `MemoryMax`/`StartLimitIntervalSec=0`; marker present; idempotent second run "up to
  date"; a changed `--watchdog` re-renders + reload + **restart**; `--dry-run` inert;
  reachability MainPID-mismatch → `E_INSTALL_UNAVAILABLE`.
- **Client-defer (dispatcher):** with a fake `systemctl` dep reporting the unit enabled,
  `spawnDaemon` runs `systemctl --user start aira-daemon.service` and does NOT fork
  `/proc/self/exe`; with the unit not-enabled it forks (fallback). A `systemctl start`
  failure surfaces honestly (no silent uncontained fork).
- **Real-systemd (`AIRA_REAL_SYSTEMD=1`, throwaway `aira-daemon-test-<pid>.service`):** the
  test unit MUST carry `Environment=XDG_STATE_HOME=<test-tmp>` + `XDG_RUNTIME_DIR=<test-tmp>`
  so it binds a TEST socket/state, never the real machine daemon (Fable P1). Install →
  active+running → MainPID-tied reachable → leak-safe `t.Cleanup` stop+disable+remove of
  ALL throwaway units (slice+anchor+daemon); pre-clean stale `*-test-<deadpid>`. NEVER
  touch the real `aira-daemon.service`.
- No-regression: slice+anchor install unchanged; `--dry-run` inert; the un-installed
  fork fallback still works.

## 7. Operational notes + the endgame (deferred, owner-gated live flip)

- Under `Restart=always`, `aira daemon stop` will `E_TIMEOUT` (systemd restarts in 2s) —
  `--status`/docs say: stop the managed daemon with `systemctl --user stop
  aira-daemon.service` (or `disable`).
- **Endgame (NOT built here):** once the daemon runs with the watchdog in `observe` and its
  selection is verified, `aira install --watchdog=enforce`, **stop+disable
  `whale-watchdog.service`** (so #59's interlock lets AIRA enforce), confirm AIRA is the
  sole PSI killer, then **retire `whale.slice` + `whale-watchdog.service`** (aira.slice the
  sole 64G pool — resolves #55 §3 overcommit). Each is a live machine change surfaced for
  owner confirmation.
