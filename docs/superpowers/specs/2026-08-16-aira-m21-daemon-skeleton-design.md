# M21 — Daemon skeleton (mandatory DB-owning daemon), v4 — APPROVED

**Status:** APPROVED — Sol plan-review r1–r4 → **APPROVE-PLAN** (13 defects across 3 rounds
fixed; r4 approve + 2 build-notes folded). **Milestone:** Phase 5 · M21.
**Branch:** `codex-aira-m21`. **Amends a spec principle:** §5.2 (see §1).

## 1. Purpose and the §5.2 amendment

The authoritative design (`docs/superpowers/specs/2026-08-07-aira-design.md` §5.2) states
*"core correctness never requires a running service"*. **The owner has decided to amend this
(2026-08-16):** the AIRA daemon becomes **mandatory and owns writes to the per-user
machine-wide `state.db`**, for the single-writer concurrency win (no cross-process WAL
contention / `BEGIN IMMEDIATE` waits / machine-wide flock dances) that compounds under
parallel load and produced the WSL2 flakes.

**Honest scope of the win in M21 (Sol r1/r2):** the skeleton delivers single-writer for
**routed pure-store coordination verbs only**. The carved-out verbs (§5.1) run in the client
process and therefore write `state.db` directly — not only foreground-run telemetry and the
M20 detach shim, but *every* carved verb, because obtaining its runner goes through
`app.Open`→`store.Open`, whose schema-init + `register()` do `BEGIN IMMEDIATE` writes, and
because `reconcile`/`check` call `store.Reconcile` before the runner. §7 enumerates this whole
transitional direct-writer set. So in M21 `BEGIN IMMEDIATE` contention is **reduced, not
eliminated** — full elimination arrives when the carved surface is folded behind the daemon
(D7, the next cut). Stated, not overclaimed. And a single writer never cures the raw transient
WSL2 vhdx `write()` EIO (`SQLITE_IOERR 778`) — the `store.Open` retry (`995eedc`) remains.

**M21 is the foundation cut:** the daemon process, the transport, the store-scoping refactor,
and client routing for both faces. No reaper/reconciler/watch/fairness-queue/fenced-lease/
run-input (all deferred).

## 2. The invariant (precise)

> **The per-user machine-wide `state.db` is written by at most one process — the daemon —
> for every routed (pure-store coordination) verb.** Named execution/GitOps verbs remain
> transitional client-side direct-writers (§7), enumerated and scheduled for folding.

- **The daemon is per-user**, its identity pinned to **its own** `$XDG_STATE_HOME` (§5.5),
  and always startable (same static binary, same user, no privileged resource). **No
  production escape hatch:** a client that cannot reach or start the daemon fails loudly with
  a stable `E_DAEMON_*` code — never a silent direct write.
- **In-process execution is a substrate, not a fallback** (§6): the core always runs against a
  given store in-process. Used by (a) the daemon itself, (b) the client for carved-out verbs,
  and (c) the test suite (isolated `XDG_STATE_HOME`; one machine-wide daemon cannot serve
  per-test DBs). It is reached only by dependency injection, never a production env flag (§6).

## 3. Scope

**In M21:** (1) `aira daemon` — a long-lived per-user process hosting a pure-store core over a
daemon-owned DB connection, listening on a Unix socket, caching per-**worktree** store scopes;
(2) a framed serializable request/response transport, byte-identical to in-process `core.Do`;
(3) client routing for both the **CLI and MCP** faces (§5.6); (4) lifecycle `aira daemon
[status|stop]` with graceful drain + a precise startup state machine (§5.5); (5) the
store-scoping refactor (§5.4) and daemon state-identity pinning (§5.5).

**Out (deferrals):** D1 heartbeat reaper · D2 continuous reconciler · D3 `aira watch` · D4 #29
cross-session admission fairness-queue · D5 fenced supervisor lease + shim writes via the
daemon · D6 `run-input` · D7 fold the execution/GitOps verbs' DB writes through the daemon ·
D8 TCP/auth transport · D9 non-Linux transport.

## 4. Architecture

```
  aira <verb>  (client, in the agent's cwd/worktree)
    │ 1. discover project + worktree (git rev-parse) → WorktreeScope{Root,CommonDir,GitDir,
    │                                                    WorktreeID,ProjectID,Slug,Config,…}
    │ 2. classify (verb, selector) via the shared classifier (§5.1)
    ├─ routed (pure store) ──────►  Unix socket ──►  aira daemon (per-user)
    │   3a. frame {Scope, Request}                    │  pin+verify state identity (§5.5)
    │   4a. read framed Response                      │  select/build per-worktree store scope
    │   5a. render (same FaceOutput)                  │    over the ONE daemon-owned DB conn
    │                                                 ▼  pureStoreCore.Do(ctx, Req) → Response
    └─ carved-out (runner/GitOps) ─►  in-process core (runner+GitOps) over the same DB, in the
        run/run-kill/run-log/reconcile/  CLIENT context (transitional direct-writer, §7)
        check/git/show RUN-*/gate-eval
```

Both faces (CLI, MCP) use one **client dispatcher** that owns the classify → route-or-inproc
decision (§5.6). Discovery stays client-side (the client has cwd + git); the daemon does no
git and never uses its own cwd (§5.3).

## 5. Design

### 5.1 Routing classification (operation/selector granular)

**The classifier canonicalizes aliases FIRST (Sol r3 #1).** `Core.Do` normalizes `get→show`,
`new→create`, `ls→list` (core.go:361-369). The shared classifier must apply the **identical**
canonicalization *before* classifying, or `aira get RUN-1` (alias of `show RUN-1`) would route
to the pure-store daemon and false-not-found. Alias-parity is a required test, `get RUN-*`
especially.

**Criterion (the source of truth, not a hand list):** a canonicalized `(verb, selector)`
operation is **carved-out** (runs in-process, client-side) iff its handler can reach the runner
or GitOps **by any path** — `Core.runner`/`Core.gitops` *or* `Store.runner` (the command-gate
lane goes through the store, Sol r2 #1). Everything else is **routed** to the daemon's
pure-store core (runner/GitOps nil). Carved ops must never reach the daemon core — not because
they'd error (they often silently *change behaviour* instead: `show`/`get RUN-*` falls through
to `store.Get`, `reconcile`/`check` skip their `if c.runner != nil` branch), but because that
silent divergence is exactly what routing them would cause.

Enumerated from the real dispatch table (`internal/core/core.go` `dispatchTable()`), audited
against every runner/GitOps reference in `internal/core` **and** `internal/store`:

- **Selector-granular:** `show` (and its alias `get`, canonicalized above) — a `RUN-*` selector
  reaches `c.runner.Get` (core.go:570) → carve out; `TICKET-*`/other selectors → route. The
  classifier inspects the canonicalized verb + selector.
- **Verb-level carve-out:** `run`, `run-kill`, `run-log` (`Launch`/`Kill`/`ReadOutput`);
  `reconcile`, `check` (call `store.Reconcile` **and** `runner.Reconcile`, core.go:1394/1406/
  1424); `git` (GitOps network ops); and **all** `gate run` / `gate canary-run` operations —
  whether a gate *executes* a command lane is **data-dependent** (gate content loaded from
  disk, via `Store.runner`), which the dispatcher cannot decide from `(verb, args)`, so they
  are carved out **wholesale** rather than by lane (Sol r2 #1). Non-executing `gate`
  subcommands (list/show/attest) route.
- **Routed (pure store):** all other verbs — `id`, `create`, `list`, `get`, `set`, `mv`,
  `claim`, `release`, `heartbeat`, `touch`, `link`, `unlink`, `find(ing)`, `req`, `ready`,
  `review`, `import` (see §5.3), `test-report`, `spend`, `quota`, `grep`, `insights`, `count`,
  `project`, `milestone`, `roadmap`/`backlog`/`stats` (whichever are dispatch verbs), `init`
  (bootstrap, §5.3).

**A store execution-dependency interface is required for the test (Sol r3 #2).** Today
`Store.runner`/`SetRunner` take the concrete `*runner.Runner`, so a recording sentinel cannot
be injected. Introduce a small interface for the gate-used methods (`Launch`, `ReadOutput`)
that `Store` depends on; `*runner.Runner` satisfies it in production, a recording sentinel in
the test. (Narrow, additive — not a runner redesign.)

**Completeness is enforced by a behaviour test, not a nil check (Sol r2 #1, r3 #2, r4 note):**
1. Build a core over **recording sentinel** dependencies — runner, GitOps, and the store's
   execution-dependency — that record any call. Across a fixture set covering data-dependent
   branches (a gate command lane, `show`/`get RUN-*`, `reconcile`/`check` with run records) plus
   representative pure-store ops:
   - **`routed ⇒ untouched` (hard invariant):** every routed `(verb, selector)` leaves all
     sentinels untouched — a routed op that touches one is a misclassification → fail.
   - **`touched ⇒ carved`:** any op that touches a sentinel must be classified carve-out.
   - **Explicit classification assertions** for every **wholesale** carve-out (`gate run`,
     `gate canary-run`) — since a gate with no command lane legitimately touches nothing, the
     behaviour test alone can't prove they're carved; assert the classifier labels them
     carve-out directly (Sol r4).
2. The `AfterWrite` producer (detached `run`, core.go:35) is inside the carve-out; a test
   asserts no routed verb returns a non-nil `AfterWrite`. (Confirmed true today by Sol r1.)
3. **Honesty rule:** a routed verb later found to need the runner/GitOps is reclassified to
   carve-out — never left to false-fail.

### 5.2 Wire protocol

Unix `SOCK_STREAM`; one framed request + one framed response per connection (no multiplexing
in v1). Framing: 4-byte big-endian length + JSON body, with a max-frame cap (reject oversized
→ `E_DAEMON_PROTOCOL`).

- **Request frame:** `{ "proto": <int>, "scope": <WorktreeScope>, "request": <core.Request> }`.
  `WorktreeScope` is the client-discovered serializable projection needed to build a store
  scope: `Root, CommonDir, GitDir, WorktreeID, ProjectID, Slug, Prefixes, RequirementPrefixes,
  ReviewPolicy, retention caps, LeaseTTL, config_digest`. **It carries no DB/registry/lease-
  state path** — the daemon derives those from its own env (§5.5).
- **Response frame:** the JSON projection of `core.Response` minus `AfterWrite`
  (`OK, Code, Data, Error, Warnings, Exit`); rendered through the identical `FaceOutput`, so
  text/JSON/MCP output is byte-identical to in-process.
- **Version:** on a `proto` mismatch the daemon replies with a frame carrying **its own
  supported `proto`** (evidence for the monotonic comparison, Sol r4) and `E_DAEMON_PROTOCOL`;
  the client then either performs authorized replacement (only if its proto is newer, §5.5) or
  fails loud — never a silent skew.

### 5.3 Client-context paths: git-files, `import`, `init`

- **Git-file writes route fine:** the store writes ticket/requirement `.md` by **absolute**
  path (`Root`/`CommonDir` from the scope); the daemon (same user, same filesystem) writes
  `<Root>/.aira/tickets/…` directly. The daemon never uses its own cwd for a routed verb.
- **`import` / `req import` (Sol r1 #4):** these pass caller-supplied source paths that the
  store `os.Open`s relative to the executing process — wrong in the daemon. **Fix:** the client
  resolves the source path to absolute **and reads the bytes client-side**, transmitting the
  content in the request (the importer parses bytes, not a path). A path that cannot be read
  client-side fails client-side with the existing error.
- **`init` (bootstrap):** no `.aira/config`/registration exists yet, so no full scope. The
  client sends a `bootstrap` descriptor (Root, CommonDir, GitDir, requested slug/prefixes); the
  daemon scaffolds `<Root>/.aira/config`, registers, and returns. Any caller-relative result
  presentation is rendered client-side from the absolute paths the daemon returns.

### 5.4 Store-scoping refactor (Sol r1 #2)

Today `store.Open` fuses two concerns: opening the DB **and** binding a worktree scope
(`root, commonDir, worktreeID, lease-state paths, policies, retention`). A machine-wide daemon
serving many worktrees over one DB must separate them, or sibling worktrees with equal config
alias (second worktree writes under the first `Root`, attributes claims/leases to the first
`WorktreeID`).

- **Preserve the transient-IOERR retry across the split (Sol r3 #3).** Today `store.Open`
  retries the whole `openOnce` — including `register()`'s `BEGIN IMMEDIATE` — under the WSL2
  `SQLITE_IOERR` hardening (`995eedc`). After the split, `OpenDB` cannot register (registration
  is per-worktree); **`NewScope`'s registration must keep the same bounded retry**, or the
  hardening is silently lost on first use of each scope. A test covers a transient IOERR during
  `NewScope` registration.
- **Refactor:** split into (a) `store.OpenDB(dbPath, registryPath) → *DB` — opens the shared
  connection once (WAL, `MaxOpenConns(1)`, the M21 open retry), owned by the daemon for its
  lifetime; and (b) `store.NewScope(db, ScopeOptions) → *Store` — a lightweight worktree-scoped
  view over that shared `*DB` carrying `Root/CommonDir/WorktreeID/lease paths/policies/
  retention`. In-process callers (tests, carved-out verbs) use a convenience that opens a
  private DB **and** a scope (today's `store.Open` behaviour, preserved).
- **Daemon cache:** `map[scopeKey]*Store`, `scopeKey =` canonical
  `(Root, CommonDir, GitDir, WorktreeID, config_digest)`. A config change → a new digest → a
  fresh scope. All scopes share the one daemon-owned `*DB`.
- **Concurrency:** the shared `*sql.DB` with `MaxOpenConns(1)` already serialises statements
  in-process; scopes are stateless views. Any per-scope mutable caches (e.g. prefix maps) are
  built per scope, not shared.
- **Ownership contract (Sol r2 #4):** the `*DB` is closed by exactly one owner. The daemon
  owns its shared `*DB` and closes it only on shutdown; a **scope's `Close()` is a no-op /
  release** and must never close the shared connection (today `Store.Close()` closes `s.db` —
  a scope view must not). The in-process convenience open owns its private DB and closes it as
  today. A test asserts closing a scope leaves the shared `*DB` usable.
- **Identity is recomputed, never trusted (Sol r2 #4):** `NewScope` / the daemon recompute
  the canonical `ProjectID = hash(canonical CommonDir)` and `WorktreeID = hash(canonical
  GitDir)` from the descriptor's paths, and reject a descriptor whose supplied ProjectID/
  WorktreeID disagree (`E_DAEMON_PROJECT_INVALID`) — so a malformed descriptor can never
  register or write under an arbitrary identity.

### 5.5 Daemon identity, socket, and the startup state machine (Sol r1 #5, #6)

- **State identity is the daemon's, not the client's.** The daemon derives its canonical
  `DBPath/RegistryPath/LeaseStateDir` from **its own** `$XDG_STATE_HOME` at startup and pins
  them. A request `WorktreeScope` carries **no** DB path; if a (future) field ever implies a
  different state home, the daemon rejects it with `E_DAEMON_PROJECT_INVALID`. The socket and
  lock are **namespaced by the canonical state identity**: `<runtime>/aira/<state-id>/daemon.sock`
  and `daemon.lock`, where `runtime = $XDG_RUNTIME_DIR` (fallback `$XDG_STATE_HOME/aira/run`),
  dir mode `0700`, and `state-id = hash(canonical $XDG_STATE_HOME)`. Thus clients with
  different state homes reach different daemons; none can point a daemon at a foreign DB.
- **Startup state machine (client auto-start):**
  1. `connect(sock)`. Success → send request.
  2. Refused/absent → acquire an exclusive `flock` on `daemon.lock` (bounded wait).
  3. Under the lock: re-`connect` (someone may have just won) → success → **release lock**, use
     it. Else the socket is stale/absent: `unlink` it (safe — we hold the lock), **release the
     lock**, then `fork+exec` `/proc/self/exe daemon`.
  4. Poll `connect` with a bounded deadline **regardless of our own child's fate** — under a
     race, several clients fork and all but one child exits 0 after losing the daemon flock;
     that is a **won race by another client**, not a failure (Sol r2 #3). So: our child exiting
     **0** → keep polling (the winner's socket is coming up). A **non-zero** child exit → record
     its stderr but keep polling until the deadline (a concurrent daemon may still serve us).
     Only the bounded deadline elapsing with no acceptable socket → `E_DAEMON_TIMEOUT` (or
     `E_DAEMON_UNAVAILABLE` if the last child failure is the sole cause). A `connect` that
     succeeds at any point wins immediately.
  5. The spawned daemon acquires the **same** `flock` before binding (single-instance); a loser
     exits 0 — expected under a race, per step 4.
- **Authorized replacement is MONOTONIC by protocol version (Sol r3 #4):** only a client whose
  supported `proto` is **newer** than the running daemon's may replace it — it signals the
  daemon (socket control verb or SIGTERM to the `daemon.lock` pid), waits (bounded) for the lock
  release + socket removal, then auto-starts. An **older** client that meets a newer daemon gets
  a loud `E_DAEMON_PROTOCOL` and **does not stop it** — so concurrent old/new binaries cannot
  ping-pong-replace each other. (`daemon stop` is an explicit operator action, always allowed.)
  Never `unlink` a socket whose lock is held live; never `fork` while holding the lock.
- **Liveness/`status`:** the lock holder writes `pid`+`boot_id` to `daemon.lock`; `status`
  reads them, guards pid-reuse with `boot_id`, and probes the socket.

### 5.6 Both faces route through one client dispatcher (Sol r1 #3)

The CLI (`cmd/aira/main.go`) and the **MCP** provider (`cmd/aira/mcp_project.go`, which today
calls `app.Open` + `core.Do` per request — a hidden production writer) must **both** go through
a single `Dispatcher` seam: `Dispatch(ctx, WorktreeScope, Request) → Response`. Production wires
the daemon-backed dispatcher (classify → route-or-inproc); tests wire an in-process dispatcher
(§6). No production face may `store.Open` the real DB and `core.Do` a routed verb directly. A
test drives an **MCP mutation** through the daemon to prove it.

## 6. In-process substrate via injection (Sol r1 #7)

No env flag (a process cannot distinguish a "temporary override" from a user's real
`$XDG_STATE_HOME`). Instead the client entrypoint takes an **injected `Dispatcher`**:

- **Production** wires the daemon-backed dispatcher only; the in-process store path is not
  reachable from the production entrypoint for routed verbs.
- **Tests** inject an in-process dispatcher directly (`Run`/the face is parameterised), so they
  never spawn or contact a shared per-user daemon; they run entirely over their isolated DB.
- **Daemon integration tests** isolate **both** `XDG_RUNTIME_DIR` and `XDG_STATE_HOME` (temp),
  so a real daemon under test is namespaced away from any real per-user daemon.

## 7. Transitional direct-writers (named, folded later)

Carved-out verbs (§5.1) run in-process client-side and therefore write `state.db` directly.
The honest, complete enumeration (Sol r2 #2) — larger than "run-telemetry + shim":

1. **Every carved verb's `store.Open`** — obtaining the client-side runner goes through
   `app.Open`→`store.Open`, whose schema-init + `register()` do `BEGIN IMMEDIATE` writes. So
   `git`, `run-log`, `show RUN-*`, and all carved verbs perform at least a `register` DB write
   in the client process (idempotent; may double-write a row the daemon also registers).
2. **`reconcile`/`check`** additionally call `store.Reconcile` (outbox replay → journal/DB
   writes) before the runner phase.
3. **`run`/`run-kill`/`run-log`** and their M19 telemetry wiring (`AddComputeEvent`/
   `AddTestReport`), plus the M20 detach **shim** (a separate long-lived process).

These are the *same* writers that exist today and are individually crash-safe against the WAL
DB (single-writer is a routing policy, not a SQLite lock), so leaving them direct in M21 is the
pre-daemon status quo — now explicitly scoped for folding in D7 (route the carved verbs' store
phase — incl. `register` and `store.Reconcile` — through the daemon; decompose store-phase vs
client-runner-phase) and D5 (shim / fenced lease). Until then `BEGIN IMMEDIATE` contention is
**reduced** (routed coordination serialised through the daemon), not eliminated (§1). A test
documents this set so the claim stays honest as verbs migrate.

## 8. Lifecycle and failure semantics

Graceful stop (`daemon stop`/SIGTERM): stop accepting, drain in-flight (bounded), close the DB,
release the lock, remove the socket. Crash: the kernel releases the lock; the next client
detects the stale socket (§5.5) and auto-starts; no coordination state is lost (the DB is the
authority; every mutation commits before its response). A panicking request is recovered
per-connection → `E_DAEMON_INTERNAL`; the shared daemon stays up. A restart is transparent (the
daemon holds no state beyond the DB).

## 9. Error codes (stable)

`E_DAEMON_UNAVAILABLE` (cannot reach/start; loud, never a silent bypass), `E_DAEMON_TIMEOUT`
(auto-start/request deadline), `E_DAEMON_PROJECT_INVALID` (scope/state-identity disagreement),
`E_DAEMON_PROTOCOL` (frame/version/oversize), `E_DAEMON_INTERNAL` (recovered panic). None is a
coordination verdict; transport failures are surfaced, never converted to a fake success.

## 10. Testing strategy

- **In-process parity:** the whole existing suite keeps running over injected in-process
  dispatchers on isolated DBs — the correctness baseline, must stay byte-for-byte green.
- **Routed round-trip:** a real daemon on temp `XDG_RUNTIME_DIR`+`XDG_STATE_HOME`; a routed
  coordination verb over the socket returns byte-identical to the in-process result.
- **MCP routing:** an MCP **mutation** is driven through the daemon (proves §5.6 — MCP is not a
  second writer).
- **Carve-out completeness (recording sentinels, not nil):** every routed `(verb, selector)`
  — incl. data-dependent branches (a gate-command-lane fixture, a RUN selector, a reconcile
  with run records) — exercised against a core whose runner/GitOps and `Store.runner` are
  **recording sentinels** must never touch a sentinel; no routed verb returns a non-nil
  `AfterWrite` (§5.1).
- **Sibling-worktree isolation:** two worktrees, same config, served by one daemon → each
  writes under its own `Root` and attributes claims/leases to its own `WorktreeID` (proves
  §5.4 — no aliasing).
- **Scope-close ownership:** closing a per-worktree scope leaves the shared daemon `*DB`
  usable (a subsequent routed verb still works); only `DB.Close()` closes the connection (§5.4).
- **Recomputed identity:** a descriptor whose supplied ProjectID/WorktreeID disagree with
  `hash(canonical CommonDir/GitDir)` is rejected `E_DAEMON_PROJECT_INVALID` — no write under a
  forged identity (§5.4).
- **State-identity pinning:** a client whose scope implies a different state home is rejected
  `E_DAEMON_PROJECT_INVALID`; two different `XDG_STATE_HOME` → two different sockets/daemons.
- **Single-instance race + loser keeps polling:** N concurrent auto-starts → exactly one binds;
  the N−1 losing children exit 0 and their clients **keep polling and still succeed** (never
  false-fail `E_DAEMON_UNAVAILABLE`, Sol r2 #3); N successes, one lock holder, one socket.
- **Stale-socket recovery / child early-exit / bounded timeout:** each yields the correct
  `E_DAEMON_*` (§5.5), never a silent in-process production write.
- **`import` path safety:** a routed `import` reads bytes client-side; a daemon-cwd-relative
  path never resolves against the daemon.
- **Graceful drain:** a slow in-flight request completes across `stop`.

## 11. Risks and invariants

- **R1 — partial single-writer in M21** (execution + shim direct). *Mitigation:* named,
  enumerated, scheduled (D5/D7); §1/§7 state it, never overclaim.
- **R2 — carve-out incompleteness** (a routed verb secretly needs runner/GitOps → false-fail).
  *Mitigation:* the criterion + the completeness test (§5.1).
- **R3 — sibling-worktree aliasing.** *Mitigation:* the store-scoping refactor + per-worktree
  scopeKey (§5.4) + the isolation test (§10).
- **R4 — client-controlled DB path.** *Mitigation:* the daemon pins its own state identity;
  scope carries no DB path (§5.5).
- **R5 — auto-start races / stale sockets / fork-while-locked.** *Mitigation:* the exact state
  machine (§5.5, release-before-fork, flock-before-bind, boot_id guard) + the race test.
- **R6 — MCP as a hidden writer.** *Mitigation:* one client dispatcher for both faces + the
  MCP-mutation test (§5.6).
- **R7 — test bypass of the real DB.** *Mitigation:* injected dispatchers, no env flag (§6).

## 12. Sol build-review checklist

1. **Byte-identical relay:** a routed verb's socket `Response` equals the in-process one
   (Data/Code/Warnings/Exit); nothing lost/reordered across the frame.
2. **Carve-out completeness (recording sentinels) + AfterWrite:** every runner/GitOps-touching
   path — via `Core.runner`/`gitops` OR `Store.runner` (command gates), incl. data-dependent
   `gate run`/`canary-run` (carved wholesale) — is carved out; the recording-sentinel test
   proves no routed verb touches a sentinel (not merely "no nil error"); no routed verb returns
   a non-nil `AfterWrite`.
3. **Store scoping + ownership:** sibling worktrees never alias; scopeKey includes worktree
   identity recomputed from paths (forged identity rejected); one DB connection, per-worktree
   views; `DB.Close()` owns the connection and scope `Close()` is a no-op; no per-scope state
   shared unsafely.
4. **State identity:** the daemon pins its own DB/lease paths; a foreign-state or
   identity-mismatched scope is rejected; socket/lock namespaced by state-id.
5. **Startup state machine:** flock-before-bind; release-before-fork; stale socket removed only
   under lock; a losing child's exit-0 keeps the client polling (no false-fail); only the
   bounded deadline → `E_DAEMON_TIMEOUT`; no two-daemon window.
6. **Both faces routed:** CLI and MCP share the dispatcher; no production `store.Open`+`core.Do`
   of a routed verb; the MCP-mutation test passes.
7. **Loud-failure honesty:** every unreachable path returns `E_DAEMON_*`, never a silent
   in-process production write; the injected-dispatcher test substrate cannot touch the real DB.
8. **Graceful drain:** in-flight requests survive `stop`; the DB closes only after drain.

## 13. Deferrals

D1 reaper · D2 continuous reconciler · D3 `watch` · D4 #29 fairness-queue · D5 fenced supervisor
lease + shim writes via daemon · D6 `run-input` · D7 execution + telemetry writes via daemon ·
D8 TCP/auth transport · D9 non-Linux transport.
