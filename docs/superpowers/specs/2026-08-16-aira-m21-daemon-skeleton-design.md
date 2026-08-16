# M21 — Daemon skeleton (mandatory DB-owning daemon), v1

**Status:** plan (pre Sol plan-review). **Milestone:** Phase 5 · M21.
**Branch:** `codex-aira-m21`. **Supersedes a spec principle:** amends §5.2 (see §1).

## 1. Purpose and the §5.2 amendment

The authoritative design (`docs/superpowers/specs/2026-08-07-aira-design.md` §5.2)
states *"core correctness never requires a running service"* — the daemonless floor the
whole Phase 1–5 build rests on. **The owner has decided to amend this (2026-08-16):** the
AIRA daemon becomes **mandatory and owns all writes to the machine-wide `state.db`**.
The motivation is the single-writer concurrency win — no cross-process WAL contention,
no `BEGIN IMMEDIATE`/busy-timeout waits, no machine-wide flock dances — which is the
class of failure that compounds under parallel load (and which produced the WSL2 flakes).

**Honest caveat, accepted by the owner:** a single writer removes the *contention* class
but does **not** cure the raw transient WSL2 vhdx `write()` EIO (`SQLITE_IOERR 778`) — the
daemon still writes the same vhdx — so the `store.Open` retry (master `995eedc`) remains.

**This milestone (M21) is the skeleton only.** Later cuts add the heartbeat reaper,
continuous reconciler, `aira watch`, the #29 cross-session admission fairness-queue, the
fenced supervisor lease, and `run-input`. M21 establishes the process, the transport, and
the invariant; it adds no new coordination behaviour.

## 2. The invariant

> **The per-user machine-wide `state.db` is written by at most one process: the daemon.**

Precisely, in M21:

- **The daemon is per-user**, scoped to `$XDG_STATE_HOME` (the same key as `state.db`), and
  is **always startable** (it is the same static binary, run as the same user; no privileged
  resource is required). There is therefore **no production escape hatch** — if a client
  cannot reach or start the daemon it fails loudly with a stable error, never silently
  writing the DB directly.
- **Coordination verbs route through the daemon** (§5). This is the single-writer path.
- **In-process execution is a substrate, not a fallback** (§6): the core can always run
  against a *given* store in-process. This is used by the test suite (each test has an
  isolated `XDG_STATE_HOME`/temp DB — one machine-wide daemon cannot serve per-test DBs)
  and by the daemon itself (the daemon *is* an in-process core over the real store).
- **Transitional direct-writers, documented, folded in later cuts (§7):** the execution
  verbs (`run`, `run-kill`, `run-log`) and the M20 detach shim execute in the client's own
  context and today write DB telemetry directly. M21 does **not** relay these; it carves
  them out as a named, enumerated set of transitional direct-writers. The single-writer
  invariant is therefore delivered for coordination in M21 and completed for execution in a
  later cut. This partial delivery is stated, not hidden.

## 3. Scope

**In M21:**

1. `aira daemon` — a long-lived per-user process hosting an in-process core over the real
   store, listening on a Unix stream socket, with a per-project `Core` cache.
2. A framed serializable request/response transport over that socket, byte-identical in
   result to today's in-process `core.Do`.
3. Client routing for **coordination verbs**: discover the project client-side, connect to
   the daemon (auto-starting it under a single-instance guard if absent), send the request,
   render the response.
4. Lifecycle: `aira daemon` (run in foreground), `aira daemon status`, `aira daemon stop`;
   graceful shutdown that drains in-flight requests; stale-socket detection and cleanup.
5. The in-process substrate retained for tests and for the daemon's own core.

**Out (explicit deferrals, later cuts):** heartbeat reaper (D1); continuous reconciler (D2);
`aira watch` (D3); #29 cross-session admission fairness-queue (D4); fenced supervisor lease +
routing the detach shim's writes through the daemon (D5); `run-input` (D6); relaying the
execution verbs' DB writes through the daemon (D7); a TCP/auth transport (D8, Unix-socket only
in v1); a Windows/non-Linux transport (D9).

## 4. Architecture

```
  aira <verb>  (client, in the agent's cwd/worktree)
    │  1. discover project (git rev-parse) → Project{ProjectID, CommonDir, Root, Config, …}
    │  2. classify verb: coordination → route; execution/detach → in-process (transitional)
    ▼
  ── coordination ──────────────►  Unix socket  ──►  aira daemon (per-user)
    3a. frame {Project, Request} JSON                 │  select/build per-project Core
        over the stream                               │  (keyed by ProjectID) over the
    4a. read framed Response JSON                      │  ONE open real store
    5a. render (same FaceOutput as today)              ▼  core.Do(ctx, Request) → Response
                                                       │  (in the daemon process)
  ── execution/detach ──────────►  in-process core over the real store (transitional
        run/run-kill/run-log             direct-writer; runs in the CLIENT context)
```

- **One daemon per user.** It holds the real store open once and caches a `Core` per
  `ProjectID`. A project's `Core` carries that project's prefixes/review-policy; its runner
  and GitOps are **not** used for coordination verbs (those never launch subprocesses), so
  the daemon may build a coordination-only `Core` (runner/GitOps nil) — see §5.4.
- **Discovery stays client-side.** The client runs project discovery (it has the cwd and git
  access — the hardened `gitValue`), and sends the resolved `Project` to the daemon. The
  daemon does no git and no cwd-sensitive work for coordination verbs.
- **The daemon reuses `core.Do`.** No new dispatch logic; the daemon is a thin socket front
  over the existing `Core.Do` (`internal/core/core.go:359`), which is already documented as
  the transport-neutral seam and speaks serializable `Request`/`Response`.

## 5. Client routing (coordination verbs)

### 5.1 Verb classification

The dispatch table (`Core.dispatchTable()`) is the source of truth. Each `verbSpec` is
tagged (a new boolean/field) as **coordination** (relayable — pure store work) or
**execution** (client-side in M21). The classification is enumerated in code, not inferred:

- **Execution / carve-out (in-process, transitional):** `run`, `run-kill`, `run-log`, and the
  hidden `__supervise` shim path. Rationale: `run` launches a subprocess that must execute in
  the client's cwd/worktree/cgroup, not the daemon's; `run-log` streams from client-visible
  capture files; the shim is already a separate long-lived process.
- **Coordination / relayable (everything else):** `init`, `id`, `create`, `list`, `get`,
  `set`, `mv`, `claim`, `release`, `heartbeat`, `touch`, `link`, `find`, `req`, `ready`,
  `review`, `import`, `test-report`, `ratchet`, `spend`, `quota`, `grep`, `backlog`,
  `roadmap`, `stats`, `check`, `reconcile`, `exec`.
- **`AfterWrite` carve-out:** any response carrying a non-nil `AfterWrite` callback
  (`core.go:35`, `json:"-"`) cannot cross the socket. In M21 the only `AfterWrite` producer is
  the detached-run delivery handshake, which is already inside the execution carve-out, so no
  coordination verb crosses the socket with an `AfterWrite`. A build-time assertion (a test)
  enforces this: no coordination-classified verb may return a non-nil `AfterWrite`.

**Honesty rule:** if a verb's coordination handler is later found to write outside the
daemon's process, it is reclassified to the carve-out with a stable warning — never left as a
silent second writer.

### 5.2 Wire protocol

A Unix `SOCK_STREAM` socket. Each interaction is one framed request and one framed response,
length-prefixed (4-byte big-endian length + JSON body); connection is one-shot per request in
v1 (no multiplexing — simplest correct thing; keep-alive/streaming is a `watch`-era concern).

- **Request frame:** `{ "project": <Project descriptor>, "request": <core.Request> }`.
  The `Project` descriptor is the client-discovered, serializable projection needed to build
  the per-project store Options (ProjectID, CommonDir, Root, StateDir, ProjectSlug, prefixes,
  review policy, retention caps, lease TTL). It is validated daemon-side against the same
  `store.Options` rules; a mismatch → `E_DAEMON_PROJECT_INVALID`.
- **Response frame:** the JSON projection of `core.Response` **minus** `AfterWrite`
  (`OK, Code, Data, Error, Warnings, Exit`). The client renders it through the identical
  `FaceOutput` path it uses today, so text/JSON/MCP output is byte-identical.
- **Protocol version:** the first frame carries a `proto` integer; a client/daemon version
  mismatch → the client refuses and (per §5.5) restarts the daemon.

### 5.3 Git-file writes and `init` (the daemon is not cwd-bound)

Coordination verbs are not "DB-only": `create`/`set`/`mv`/`req`/`import` write ticket and
requirement **git-files** (`.md`) into the worktree, and `init` scaffolds `.aira/config`.
This does **not** block relaying, because the store writes git-files by **absolute path** —
its `Options.Root`/`CommonDir` are absolute paths carried in the client's `Project`
descriptor, and the daemon (same user, same filesystem) can write `<root>/.aira/tickets/…`
and `<common>/aira/…` directly. The daemon never uses its own cwd for coordination; every
path is client-provided and absolute. (Runs are different — a subprocess *inherits* cwd and
namespace, which is why the execution verbs stay client-side, §5.1.)

- **`init` is the bootstrap special case:** no `.aira/config`/registration exists yet, so the
  client cannot send a fully-formed `Project` descriptor. The client sends the repo facts it
  *can* discover (root, common-dir, git-dir, requested slug/prefixes); the daemon scaffolds
  `<root>/.aira/config`, registers the project, and returns. `init` is classified coordination
  but takes a distinct request shape (a `bootstrap` descriptor, not a `Project`).
- **A worktree the daemon cannot see** (e.g. a bind-mount visible only to the client) would
  make git-file writes fail — surfaced as the store's existing IO error, never a fake success.
  This is out of scope for the "same user, same machine" model (a documented assumption).

### 5.4 Per-project Core cache

The daemon keeps `map[ProjectID]*coordinationCore` under a mutex. On a request for an unseen
project it builds the store Options from the request's `Project` descriptor, `store.Open`s
(the real machine-wide DB, already open once at the connection layer — see §5.6), and
constructs a coordination `Core` (runner/GitOps nil, since coordination verbs never launch).
Config drift (a project whose `.aira/config` changed since it was cached) is handled by keying
the cache on `(ProjectID, config_digest)`; a new digest builds a fresh entry. The store handle
itself is shared (one DB, one `*sql.DB`, `MaxOpenConns(1)` as today).

### 5.5 Auto-start and single-instance

- **Socket + lock live under `$XDG_RUNTIME_DIR/aira/`** (fallback `$XDG_STATE_HOME/aira/run/`
  if `XDG_RUNTIME_DIR` is unset), dir mode `0700`. Socket `daemon.sock`; lock `daemon.lock`.
- **Auto-start:** a client that finds no live daemon `fork+exec`s `aira daemon` (via
  `/proc/self/exe`), then waits (bounded, e.g. 2s) for the socket to accept, then proceeds.
- **Single-instance:** the daemon acquires an exclusive `flock` on `daemon.lock` (the existing
  `runner/ledger.go` flock helpers) *before* binding the socket; a second daemon losing the
  race exits 0 silently (someone else won). A client racing to auto-start likewise: only one
  `fork+exec` wins the lock; the losers connect to the winner's socket.
- **Stale-socket detection:** if `connect` fails with `ECONNREFUSED` on an existing socket
  file (daemon died), the client removes the stale socket **only while holding the lock** and
  auto-starts. Never remove a socket whose lock is held live.
- **Liveness:** the lock holder writes its pid to `daemon.lock`; `aira daemon status` reads it
  and probes the socket. A `boot_id` guard (as in the runner) distinguishes a live pid from a
  post-reboot reused pid.

### 5.6 The store handle

The daemon `store.Open`s the machine-wide DB **once** at startup and keeps it open for its
lifetime (the load-once session). Coordination cores share this one handle. On graceful stop
the handle is closed after in-flight requests drain.

## 6. In-process substrate (tests + the daemon itself)

`Core.Do` already runs in-process over any `Store`. M21 keeps a single well-named entry point
—`app`/`core` construct an in-process core over a given store—used by:

- **the daemon** (its hosted core), and
- **the test suite:** `cmd/aira` tests call `Run([]string{…})` against an isolated
  `XDG_STATE_HOME`. Under M21 the *default* client path would auto-start a daemon; tests must
  not spawn a per-user daemon against a shared socket. So the client exposes an **in-process
  execution mode** selected when the target DB is test-isolated. Mechanism: an internal
  `AIRA_INPROCESS=1` (test-only, set by the test harness / `Run` wrapper) forces the in-process
  substrate. This is **not** a production escape hatch (production has none, §2); it is the
  test substrate. A guard rejects `AIRA_INPROCESS=1` pointing at the real per-user
  `$XDG_STATE_HOME` unless a temp override is in effect, so it cannot become a silent
  production bypass.

*(Design note for the reviewer: the cleanest form of this may be that `Run`/the client takes
an injected "dispatcher" — daemon-backed or in-process — and tests inject the in-process one
directly, avoiding an env flag. The implementer chooses; the invariant is that tests never
touch a shared daemon and production never runs in-process.)*

## 7. Transitional direct-writers (named, folded later)

M21 leaves these writing `state.db` outside the daemon, each documented and enumerated:

1. **Execution verbs** `run`/`run-kill`/`run-log` — execute client-side; their M19 telemetry
   wiring (`AddComputeEvent`/`AddTestReport`) writes the DB directly. Folded in D7 (client
   executes; DB writes relay to the daemon).
2. **The M20 detach shim** — a separate long-lived process writing run/telemetry records.
   Folded in D5 (fenced supervisor lease / route shim writes through the daemon).

Because these are the *same* writers that already exist and are individually crash-safe
against the DB (WAL permits concurrent openers; single-writer is a routing policy, not a
SQLite lock), leaving them direct in M21 is correct, not a regression — it is the pre-daemon
status quo for those verbs, now explicitly scoped for migration.

## 8. Lifecycle and failure semantics

- **Graceful stop** (`aira daemon stop`, SIGTERM): stop accepting, drain in-flight requests
  (bounded), close the store, release the lock, remove the socket.
- **Crash:** the lock is released by the kernel on process death; the next client detects the
  stale socket (§5.5) and auto-starts a fresh daemon. No coordination state is lost — the DB
  is the authority and every mutation is already durably committed before the response.
- **A request that panics** in the daemon is recovered per-connection → `E_DAEMON_INTERNAL`;
  the daemon stays up (one bad request never takes down the shared process).
- **Idempotency across restart:** because the daemon adds no state beyond the DB, a restart is
  transparent; there is no in-memory mutation to lose.

## 9. Error codes (stable)

`E_DAEMON_UNAVAILABLE` (cannot reach or start the daemon; loud failure, never a silent
bypass), `E_DAEMON_PROJECT_INVALID` (bad project descriptor), `E_DAEMON_PROTOCOL`
(frame/version error), `E_DAEMON_INTERNAL` (recovered panic / unexpected), `E_DAEMON_TIMEOUT`
(auto-start or request deadline). None of these is a coordination verdict; they are transport
failures and are surfaced, never converted to a fake success.

## 10. Testing strategy

- **In-process substrate parity:** the existing suite keeps running in-process (isolated DBs)
  — this is the correctness baseline and must stay green byte-for-byte.
- **Daemon round-trip:** a real daemon started on a temp `XDG_RUNTIME_DIR` + temp
  `XDG_STATE_HOME`; a client routes a coordination verb through the socket; assert the
  response is byte-identical to the in-process result for the same request.
- **Single-instance race:** N concurrent auto-starts → exactly one daemon binds; the losers
  connect to it; assert one lock holder, one socket, N successful responses.
- **Stale-socket recovery:** kill the daemon leaving the socket file; next client detects
  `ECONNREFUSED`, cleans up under lock, auto-starts, succeeds.
- **AfterWrite assertion:** a test enumerates the dispatch table and asserts no
  coordination-classified verb returns a non-nil `AfterWrite`.
- **No-silent-bypass guard:** `AIRA_INPROCESS=1` against the real `$XDG_STATE_HOME` (no temp
  override) is rejected.
- **Graceful drain:** a slow in-flight request completes across a `stop`.
- **Loud failure:** a client that cannot start the daemon (socket dir made unwritable) returns
  `E_DAEMON_UNAVAILABLE`, never an in-process write.

## 11. Risks and invariants

- **R1 — partial single-writer in M21.** Execution verbs + shim still write direct. *Mitigation:*
  named, enumerated, scheduled (D5/D7); the invariant is stated as "coordination single-writer
  in M21", not overclaimed.
- **R2 — auto-start races / stale sockets.** *Mitigation:* flock-before-bind; stale cleanup
  only under lock; boot_id pid guard (§5.5). Two-loop-tested (§10).
- **R3 — a daemon that hosts a stale per-project Core after a config change.** *Mitigation:*
  cache keyed on `(ProjectID, config_digest)` (§5.4).
- **R4 — the daemon becomes a single point of failure.** *Mitigation:* crash → transparent
  auto-restart; the DB is the authority; no in-memory mutation to lose (§8).
- **R5 — a coordination verb that secretly writes outside the daemon.** *Mitigation:* the
  classification is enumerated + the AfterWrite assertion + the honesty rule (§5.1).

## 12. Sol build-review checklist

1. **Byte-identical relay:** does a routed coordination verb produce exactly the in-process
   `Response` (Data/Code/Warnings/Exit)? Any field lost or reordered across the frame?
2. **AfterWrite carve-out completeness:** is every `AfterWrite`-producing or DB-writing-outside
   verb actually in the execution carve-out? Prove no coordination verb writes direct.
3. **Single-instance correctness:** flock acquired before bind; loser exits cleanly; stale
   socket removed only under lock; boot_id guard against pid reuse. Any window where two
   daemons bind, or a live socket is removed?
4. **Loud-failure honesty:** every unreachable-daemon path returns `E_DAEMON_*` and NEVER a
   silent in-process production write. The `AIRA_INPROCESS` guard cannot bypass the real DB.
5. **Graceful drain:** an in-flight request is not truncated by `stop`; the store closes only
   after drain.
6. **Test substrate isolation:** tests never spawn/contact a shared per-user daemon; the
   in-process path is what runs under `go test`.
7. **No new dispatch logic:** the daemon calls `Core.Do` unchanged; no verb behaviour forks
   between in-process and daemon paths.

## 13. Deferrals

D1 heartbeat reaper · D2 continuous reconciler · D3 `aira watch` · D4 #29 cross-session
admission fairness-queue · D5 fenced supervisor lease + shim writes through the daemon ·
D6 `run-input` · D7 execution-verb DB writes relayed through the daemon · D8 TCP/auth
transport · D9 non-Linux transport.
