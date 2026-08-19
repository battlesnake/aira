# Daemon startup registry discovery — proactive reaper/flusher/watch for all registered projects, v1

**Status:** plan (pre-review). **Milestone:** Phase 5 · startup registry discovery (the
deferred reaper/flusher/watch win noted in D1/D2). **Branch:** `codex-aira-startup-discovery`.
**Depends on:** M21 (daemon), D1 (reaper), D2 (journal flusher), D3 (watch).

## 1. Goal and honest scope

The daemon's D1 lease-reaper (`server.go:273` `runReaper`) and D2 journal-flusher
(`server.go:314` `runJournalFlusher`) iterate **`s.scopes`** — the per-worktree scope
cache — which is populated **only** by `storeForScope` on a client request
(`server.go:626/648`). So a project's expired leases and deferred journal entries are
reaped/flushed **only after some request touches that project this daemon lifetime**. A
project that a *previous* daemon lifetime registered, and that no request has touched
since this daemon started, is invisible to the reaper/flusher/watch until touched — its
expired leases can block its ready-queue indefinitely.

**This milestone** enumerates the registry once on daemon start and builds each
registered project's scope into `s.scopes` (via the existing `storeForScope`), so the
already-running reaper/flusher/watch cover **every** registered project proactively.

**Why discover-at-start is sufficient (scoping argument).** Post-D7b every registration
flows through the daemon — `ensure-scope`/`register` calls `storeForScope`, which caches
the scope. So any project that registers through *this* running daemon is already
scope-cached. The **only** gap is projects registered by a *prior* daemon lifetime and
untouched since this daemon started; discover-at-start closes exactly that gap. No
periodic re-enumeration is needed (and it is an explicit non-goal — it would re-read the
registry every tick for no coverage gain).

**Honest boundary — no git-file-intent, no resurrection.** Discovery **only** reads each
project's config (`app.Discover`, a read-only config discovery — no store open, no git
mutation) and builds the scope; it does **not** reconcile/rebuild. The reaper it feeds is
DB-only (D1), and the flusher writes `journal.jsonl` from the already-committed outbox
(D2's deliberately-narrowed safe path). D2 explicitly deferred full-reconcile-on-a-timer
because git-file-intent risks worktree-resurrection + finding-lock starvation; this
milestone does **not** revive that — it adds *scope discovery for the existing DB-only
background work only*. A registry entry whose worktree/config is gone is **skipped**,
never resurrected.

## 2. Current shape (verified)

- `Serve` (`server.go:98`) opens the DB (`:158`), listens (`:175`), starts the reaper
  (`:191`) + flusher (`:199`) goroutines, signals `Ready` (`:201`), then serves.
- `runReaper`/`runJournalFlusher` iterate `s.scopes` under `s.mu`.
- `storeForScope(scope WorktreeScope)` (`server.go:589`) canonicalises + identity-checks
  paths, builds a `store.NewScope`, caches it under a ready-barrier, and reaps once on
  first build. It is the single scope-construction seam.
- `scopeFromProject(app.Project, Paths) (WorktreeScope, error)` lives in
  `cmd/aira/dispatcher.go:485` (client-side).
- `readRegistry(path) ([]registryEntry, error)` (`store.go:3128`) is **unexported**;
  `registryEntry` = `{ProjectID, CommonDir, WorktreeID, Root, Prefixes,
  RequirementPrefixes, At}` — a breadcrumb, **not** the full config.
- `app.Discover(ctx, cwd) (Project, error)` (`project.go:136`) reads `.aira/config` and
  computes identity, no store open (used already by D7a's store-free branch).

## 3. Design

### 3.1 Enumerate the registry (new exported lister)

Add `store.ListRegistryEntries(registryPath string) ([]RegistryEntry, error)` (a thin
exported wrapper over `readRegistry`, returning an **exported** `RegistryEntry` struct
carrying at least `{ProjectID, WorktreeID, CommonDir, Root, Prefixes,
RequirementPrefixes}`). Missing/empty registry → empty slice, not an error. The daemon
already holds `s.Paths.RegistryPath`.

### 3.2 Reconstruct a scope from a registry entry (shared builder)

Move `scopeFromProject` into `internal/daemon` as exported
`daemon.ScopeFromProject(project app.Project, paths Paths) (WorktreeScope, error)` (the
`WorktreeScope` type already lives in `internal/daemon`; `cmd/aira/dispatcher.go` updates
its call site to `daemon.ScopeFromProject`). Discovery then, per registry entry:
`app.Discover(ctx, entry.Root)` → `daemon.ScopeFromProject(project, s.Paths)` →
`s.storeForScope(scope)`. This **reuses** the exact cache + ready-barrier + reap-on-build,
so a project concurrently touched by a request dedups on the cache key (no double build,
no race).

### 3.3 The discovery goroutine

A new `s.runStartupDiscovery(ctx)` launched from `Serve` **after** `s.db` is set and the
reaper/flusher goroutines start, and **after** `Ready` is signalled — discovery must
**not** block bind/serve/`Ready` (it is I/O over N projects). It:
1. `entries := store.ListRegistryEntries(s.Paths.RegistryPath)`; on read error, log and
   return (never crash the daemon).
2. For each entry, **skip-not-fail** on any of: `app.Discover` error (worktree/config
   gone or invalid), `ScopeFromProject` error, a `storeForScope` identity mismatch
   (`CodeProjectInvalid` — the registered paths no longer canonicalise to the recorded
   identity), or `ctx` cancelled. Each skip is logged at debug/info with the reason; a
   bad entry never resurrects a worktree and never aborts discovery of the rest.
3. Successful entries are now in `s.scopes`, so the next reaper/flusher tick covers them;
   `storeForScope` already reaps once on first build, so an idle project's expired leases
   are freed promptly on discovery, not only on the first timer tick.

Bounded + observable: log a one-line summary (`discovered N/M registered projects, K
skipped`). An `AIRA_DAEMON_STARTUP_DISCOVERY=0` env disables it (tests + an escape hatch);
default on.

### 3.4 Concurrency + idempotence

Discovery calls `storeForScope` exactly as request handlers do — same `s.mu`, same
`key`, same ready-barrier. A request that touches project P while discovery is mid-flight
either finds P already cached (discovery won) or builds it (request won) and discovery's
later `storeForScope(P)` is a cache hit. No new lock, no new invariant. Discovery holds
no lock across `app.Discover` I/O (it builds the `WorktreeScope` first, then calls
`storeForScope`, which takes `s.mu` only for the cache insert).

## 4. Scope

**In:** §3.1 `store.ListRegistryEntries` + exported `RegistryEntry`; §3.2 move
`scopeFromProject` → `daemon.ScopeFromProject` (client call site updated); §3.3
`runStartupDiscovery` goroutine wired into `Serve` (post-`Ready`, non-blocking, skip-not-
fail); `AIRA_DAEMON_STARTUP_DISCOVERY` toggle.

**Out (stated, not silent):** periodic re-enumeration (discover-at-start suffices — §1);
registry pruning of dead entries (a separate concern; discovery only *skips* them);
reconcile/rebuild-on-discovery (the D2-deferred git-file-intent risk — explicitly NOT
revived); any change to the reaper/flusher/watch bodies themselves (they already iterate
`s.scopes`).

## 5. Testing

- **Discovery populates scopes:** a registry with project P (built by a prior "daemon",
  i.e. registered then the scope cache cleared) — after `Serve` + discovery, `s.scopes`
  contains P and P's expired lease is reaped without any client request to P. (real
  socket / real store, `AIRA_REAL_SOCKET`-gated.)
- **Skip-not-fail — missing worktree:** a registry entry whose `Root` was deleted →
  discovery skips it, logs, and still discovers the sibling valid entries; the daemon
  serves normally.
- **Skip-not-fail — invalid/absent config; identity mismatch:** an entry whose config is
  gone/corrupt, and one whose paths now canonicalise to a different identity → both
  skipped, daemon healthy, other entries covered.
- **Non-blocking:** `Ready` fires before discovery completes (inject a slow
  `app.Discover` or many entries; assert `Ready` is not gated on discovery).
- **Idempotence/concurrency:** a request touching P during discovery yields exactly one
  cached scope for P (no double build); a project registered *after* start via the daemon
  is scope-cached by the request path (not dependent on discovery).
- **Toggle:** `AIRA_DAEMON_STARTUP_DISCOVERY=0` → no discovery; touched-only behaviour
  preserved.
- **`ScopeFromProject` move:** the client dispatcher path is unchanged (same scope built);
  a table/golden equality against the pre-move output.
- **No resurrection:** discovery does not create/rewrite any `.aira/*.md` or reconcile;
  assert no git-file writes occur during discovery over a fixture project.

## 6. Risks

- **R1 — a huge/one-slow registry stalls startup work.** *Mitigation:* discovery is a
  post-`Ready` background goroutine; serving never waits on it; per-entry failures are
  isolated (skip-not-fail); machine-local registries are small.
- **R2 — worktree resurrection / git-file side effects.** *Mitigation:* discovery only
  `app.Discover`s (read-only config) + builds scopes; it never reconciles/rebuilds; a
  gone worktree is skipped. Test asserts no git-file writes.
- **R3 — races with request-driven `storeForScope`.** *Mitigation:* discovery uses the
  same seam + cache key + ready-barrier; dedup is automatic; no new lock.
- **R4 — layering.** *Mitigation:* `internal/daemon` already imports `internal/app`
  (`server.go:19`); moving `ScopeFromProject` into `daemon` is import-legal and removes
  the duplicate.

## 7. Sol build-review checklist

1. Discovery runs post-`Ready`, non-blocking; serving/bind never waits on it; `ctx`
   cancellation stops it cleanly.
2. Skip-not-fail is total: `app.Discover` error, config invalid, identity mismatch,
   registry read error → logged skip, never a daemon crash, never a resurrected worktree;
   remaining entries still discovered.
3. Reuses `storeForScope` (same cache key, ready-barrier, reap-on-build) — no new lock, no
   double build under a concurrent request, no race.
4. No git-file-intent: only `app.Discover` (read-only) + scope build; no reconcile/rebuild
   on discovery; the D2 deferral is not revived.
5. `store.ListRegistryEntries` handles a missing/empty/partially-corrupt registry
   (torn-tail tolerance consistent with `readRegistry`).
6. `ScopeFromProject` move preserves the exact client-side scope (byte/field-identical);
   client call site updated; no behaviour change on the request path.
7. `AIRA_DAEMON_STARTUP_DISCOVERY` toggle works; default-on; discover-at-start (not
   periodic) is the intended, sufficient scope.
8. Honesty: this closes the prior-lifetime-untouched-project gap only; new registrations
   still populate via the request path; not overclaimed as a full continuous reconciler.
