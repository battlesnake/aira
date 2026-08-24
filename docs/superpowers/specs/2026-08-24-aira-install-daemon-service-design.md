# `aira install` — stand up the AIRA daemon service

Status: PLAN — extends the #55 `aira install` (aira.slice + delegation anchor, merged
`67eda21`; live on this box). Owner-directed (2026-08-24): "`aira install`, run from the
static binary (no extra files needed alongside), should be sufficient to set up the
service and the slice — similar to how agentmux packages the files into the binary."
Milestone #62. Successor to the whale→AIRA migration (redirect #60 + skill/MCP #61).

## 1. Why

The AIRA daemon (`aira daemon serve`, `internal/daemon`) owns `state.db` (single-writer)
and runs the periodic subsystems — the #59 **memory watchdog** (PSI killer), the lease
reaper, reconciler, journal flusher. Today it only ever runs **on demand**: a client
auto-starts it and it exits when idle. So the watchdog never runs continuously, which is
why whale-watchdog is still the live PSI killer and whale.slice/whale-watchdog cannot yet
be retired. `aira install` will additionally install + enable + start the daemon as a
**persistent systemd user service**, baked into the binary (no shipped files), so the
daemon and its watchdog run continuously — unblocking the endgame.

## 2. The embedded daemon unit

New `assets/aira-daemon.service.in`, rendered by `strings.ReplaceAll` (mirroring
`aira.slice.in` / `aira-slice-keepalive.service.in`), with `@AIRABIN@` (← `os.Executable()`
at install) and `@WATCHDOG_MODE@` / `@WATCHDOG_INTERVAL@`:

```ini
[Unit]
Description=AIRA daemon — owns state.db and runs the memory watchdog, reaper, reconciler.
# aira-managed: aira-daemon.service

[Service]
ExecStart=@AIRABIN@ daemon serve
Environment=AIRA_DAEMON_WATCHDOG_MODE=@WATCHDOG_MODE@
Environment=AIRA_DAEMON_WATCHDOG_INTERVAL=@WATCHDOG_INTERVAL@
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
```

**Deliberately NOT `Slice=aira.slice`.** The daemon *manages* aira.slice and runs its
watchdog; if it lived in aira.slice, an aira.slice group-OOM (`memory.oom.group=1`) would
kill the daemon itself — removing the very watchdog meant to prevent OOM damage. It runs
in the default user slice (small, long-lived process). (The anchor, by contrast, MUST be
in aira.slice — different role.)

**Watchdog mode is a new install flag.** `aira install --watchdog=off|observe|enforce`
(default **observe**). `observe` = the watchdog runs, selects victims, logs "WOULD
SIGKILL", but signals nothing — safe to run alongside the live whale-watchdog, and lets
the operator verify AIRA's selection before enforcing. `enforce` is honoured but, per the
#59 **interlock**, degrades to observe while whale-watchdog is active — so enforce is safe
to set early; it only starts killing once whale-watchdog is stopped (the follow-on flip).
`off` installs the daemon with no watchdog. `--watchdog-interval` optional (default 2s,
validated `[1s,30s)` as #59 requires). Invalid mode → `E_INSTALL_ARGUMENT_INVALID`.

## 3. Install mechanics (mirror the anchor, §55 pattern)

Extend `internal/install`:
- `defaultDaemonUnit = "aira-daemon.service"`; a `daemonUnit` field on `installDeps`
  (parametrised through the seam so real-systemd tests use a throwaway name, never the
  real `aira-daemon.service` — same requirement as the anchor, Fable #55).
- Render the daemon unit alongside slice+anchor (extend `renderUnits` or a sibling
  `renderDaemonUnit`); marker-guarded atomic `publishManagedUnit` (dirfd-relative,
  O_NOFOLLOW+O_NONBLOCK, temp→fsync→renameat, refuse foreign/symlink/non-regular).
- After the anchor is up + delegation verified: `systemctl --user daemon-reload` (already
  done for the slice/anchor), then `systemctl --user enable --now aira-daemon.service`,
  then **`verifyActive(d, daemonUnit)`**; on failure `E_INSTALL_UNAVAILABLE`.
- **Reachability check:** after active, confirm the daemon answers — run
  `aira daemon status` (the existing subverb) or connect to the socket
  (`daemon.PathsFromEnv().SocketPath`); a not-reachable daemon → honest
  `E_INSTALL_UNAVAILABLE`, never a fake "installed".
- Idempotence/convergence identical to #55: content-compare the unit to decide rewrite;
  a changed `--watchdog` mode re-renders + `daemon-reload` + **restart** the service (a
  service, unlike a slice, needs `restart` to pick up a changed `ExecStart`/`Environment`
  — NOT the slice's reload-applies-live path). `--dry-run` prints the rendered unit +
  planned actions, writes nothing, starts nothing. `--status` gains a daemon facet
  (unit present + marker; active; watchdog mode from live `Environment`; reachable).

## 4. Single-instance safety (verified)

`Serve` holds `unix.Flock(LOCK_EX|LOCK_NB)` on a lockfile → `ErrAlreadyRunning` if held
(`server.go:169-171`). So the systemd-managed daemon holds the lock; any client
auto-start attempt gets `ErrAlreadyRunning` and connects to the running one over the
shared socket (`PathsFromEnv`, same `XDG_RUNTIME_DIR` for user services). No double-run,
no split brain. The service inherits `XDG_RUNTIME_DIR`/`DBUS` from the user manager, so
its `PathsFromEnv` resolves the same socket + state paths clients use.

## 5. Faces

`aira install [--memory-max] [--memory-high] [--allow-overcommit] [--watchdog=MODE]
[--watchdog-interval=DUR] [--dry-run] [--status]`. Same project-less pre-dispatch
interception; `E_INSTALL_*` codes; `SafetyExecute`.

## 6. Tests (TDD; real-systemd gated, throwaway unit names)

- Pure/seam: `--watchdog` parse (off/observe/enforce/invalid); render substitution of
  `@AIRABIN@`/`@WATCHDOG_MODE@`/`@WATCHDOG_INTERVAL@`; the daemon unit is **not**
  `Slice=aira.slice` (assert absent — discriminating against a copy-paste of the anchor);
  marker present; idempotent second run "up to date"; a changed `--watchdog` re-renders +
  `daemon-reload` + **restart** (not just reload); `--dry-run` writes/starts nothing;
  reachability failure → `E_INSTALL_UNAVAILABLE`.
- Real-systemd (`AIRA_REAL_SYSTEMD=1`, throwaway `aira-daemon-test-<pid>.service`): install
  → active → `daemon status`/socket answers → then leak-safe `t.Cleanup` stop+disable+
  remove (both the daemon unit AND, as before, the slice+anchor throwaways); pre-clean
  stale `*-test-<deadpid>` units. NEVER touch the real `aira-daemon.service`.
- No-regression: the existing slice+anchor install path is unchanged when the daemon step
  is added; `--dry-run` still inert.

## 7. Deferrals (the endgame, NOT built here — owner-gated live flip)

Once the daemon runs with the watchdog in `observe` and its selection is verified: flip
`--watchdog=enforce`, **stop + disable `whale-watchdog.service`** (so #59's interlock lets
AIRA enforce), confirm AIRA is the sole PSI killer, then **retire `whale.slice` +
`whale-watchdog.service`** (aira.slice becomes the sole 64G pool — fully resolves the #55
§3 overcommit). Each step is a live machine change surfaced for owner confirmation.
