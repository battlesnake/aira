# `aira install` — stand up the AIRA daemon service (service-authoritative)

Status: PLAN v3 — service-authoritative (clients defer), owner-chosen. v2 re-gate: **Sol
GATE-FAIL (narrow residuals) + Fable GATE-PASS-WITH-NITS** — Fable code-confirmed v2 closes
both v1 P0s (`StartLimitIntervalSec=0` + MainPID==Lock.PID tie; a mistaken fork loses the
flock harmlessly) and said fold P1-1/P1-2 verbatim + **build without a third gate**. v3
folds: identity-aware deferral (P1-1), `replaceOlderDaemon`→restart (P1-2), a daemon-side
self-defer guard (closes Sol's install-race + Fable P2-3), is-enabled-only detection,
wait-for-incumbent-stopped, false-success `daemon stop` fix, full-SocketPath divergence,
explicit systemctl seam. Extends #55 `aira install` (merged `67eda21`; live). Milestone
#62. Successor to the whale→AIRA migration (#60 redirect + #61 skill/MCP). Ready to build.

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

## 2. Client-defer + daemon self-defer (`dispatcher.go` + `daemon serve`)

`spawnDaemon()` (`dispatcher.go:74-88`) is the sole fork point (already injectable via
`d.spawn` for tests). Change it to **defer to the service** when ALL hold:
1. `systemctl --user is-enabled aira-daemon.service` == `enabled` (via a NEW explicit
   injectable `systemctlRun` seam on `daemonDispatcher`, so tests drive real detection —
   `d.spawn` replaces the whole function and can't; the nit Fable flagged). **`is-enabled`
   only — NOT "or unit file present"** (Sol P2): a manual `systemctl --user disable` must
   actually disable auto-start.
2. **Identity match (Fable P1-1, load-bearing):** the client's own resolved canonical
   `SocketPath` (from `PathsFromEnv`, keyed by `XDG_STATE_HOME`+`XDG_RUNTIME_DIR` via the
   sha256 `stateID`, `paths.go:143-172`) equals the SocketPath the *service* will bind —
   i.e. the client's resolved `XDG_STATE_HOME`/`XDG_RUNTIME_DIR` equal the unit's baked
   `Environment=XDG_STATE_HOME=…` (read from the managed unit file). If they DIVERGE (a
   test harness / repro script with a temp `XDG_STATE_HOME`, an alt profile), do **NOT**
   defer — **fork** as today: a divergent-identity daemon uses a different lock/socket/
   state.db and cannot compete with the service, and the client must reach *its own* daemon
   at *its own* socket, which `systemctl start`ing the service would never bind (that would
   be a v2-introduced regression → E_TIMEOUT).

When deferring: `systemctl --user start aira-daemon.service` (idempotent; blocks until
active) and hand back a `childResult` chan reporting only a `systemctl start` failure; the
existing socket-poll in `exchangeOrStartUsing` connects once the service binds. When
`systemctl --user` is unavailable (no user bus — headless/cron), `is-enabled` exits
nonzero → treated as not-enabled → **fork fallback** (honest degradation; a dead user
manager means no service competes anyway). When the unit is not installed: fork verbatim.

**Daemon self-defer (belt-and-suspenders; closes Sol's install-transition race + the
restart-gap double-fault Fable rated P2-3).** A stray `daemon serve` can still be forked in
the narrow window where a client saw "not enabled" microseconds before install enabled it,
or during a restart gap after a transient detection failure — and it never idle-exits, so
it would strand the service. Guard it at daemon startup, BEFORE acquiring the flock: if
`AIRA_DAEMON_MANAGED` is **unset** (this instance was NOT started by the service — the unit
sets `Environment=AIRA_DAEMON_MANAGED=1`) AND `is-enabled`==enabled AND the identity
matches (as above), the instance **self-defers**: best-effort `systemctl --user start
aira-daemon.service` then exit 0, never running as a stray daemon. So any race-forked
client daemon self-corrects and the service is always the sole daemon. **Observability
(Fable P2-3):** when `serve` loses the flock (`ErrAlreadyRunning`) while under a unit
(`$INVOCATION_ID` set), log the current lock-holder PID so `journalctl` exposes any
interloper. `--status`'s MainPID facet also surfaces a mismatch.

**`replaceOlderDaemon` (Fable P1-2).** On a protocol mismatch (`exchangeWithReplacement`,
`dispatcher.go:296-302`) `replaceOlderDaemon` (`:474-491`) SIGTERMs the daemon; under a
managed service systemd restarts the *same on-disk binary* in 2s. When the unit is
installed, replace the `daemon.Stop`+wait with **`systemctl --user restart
aira-daemon.service`**; if the protocol STILL mismatches after the restart, return a clear
`"installed aira-daemon.service binary is older than this client — re-run 'aira install'"`
error instead of bouncing the machine daemon on every command (the dev-worktree-client-
newer-than-installed-binary case).

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
Environment=AIRA_DAEMON_MANAGED=1
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
  `XDG_STATE_HOME` than the user manager. Install **verifies the full resolved
  `SocketPath`** (which depends on `XDG_STATE_HOME` AND `XDG_RUNTIME_DIR`, `paths.go:143-172`
  — Fable P2-4) matches what the baked unit will produce, and refuses/warns honestly on
  divergence rather than baking a value clients can't reach.

## 4. Install mechanics (mirror the #55 anchor; §7 hardening)

Extend `internal/install` — `defaultDaemonUnit = "aira-daemon.service"` + a `daemonUnit`
field on `installDeps` (parametrised like `sliceUnit`/`anchorUnit`, so real-systemd tests
use a throwaway name). After the anchor is up + delegation verified:
1. **Stop any incumbent daemon and WAIT for it gone** (Fable P2-1): `daemon.Stop(paths)`
   (`paths.go:268-284`; swallow its not-running error — "best-effort"), then poll
   `daemon.Status` until `!Running` (like `dispatcher.go:478-489`) with a bounded deadline,
   so the service's `serve` acquires the lock cleanly and step 5's MainPID check can't
   sample the draining incumbent's Lock.PID (up to 10s drain, `server.go:310-318`).
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

- Under `Restart=always`, `aira daemon stop` would **FALSE-SUCCEED** (Fable P2-2 — its
  25ms poll `daemon_command.go:59-66` observes the 0→2s stopped gap before `RestartSec=2`
  resurrects the daemon, then reports exit-0 "stopped" while it comes back). Make `daemon
  stop` **unit-aware**: when `aira-daemon.service` is is-enabled, refuse with a message
  redirecting to `systemctl --user stop aira-daemon.service` (or `disable`) rather than
  reporting a stop that systemd immediately undoes. `--status`/docs state the same.
- **Endgame (NOT built here):** once the daemon runs with the watchdog in `observe` and its
  selection is verified, `aira install --watchdog=enforce`, **stop+disable
  `whale-watchdog.service`** (so #59's interlock lets AIRA enforce), confirm AIRA is the
  sole PSI killer, then **retire `whale.slice` + `whale-watchdog.service`** (aira.slice the
  sole 64G pool — resolves #55 §3 overcommit). Each is a live machine change surfaced for
  owner confirmation.
