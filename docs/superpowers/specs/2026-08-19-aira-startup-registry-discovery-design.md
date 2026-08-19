# Daemon registry discovery — proactive reaper/flusher/watch for all registered projects, v2

**Status:** plan — Sol plan-review r1 → REQUEST-CHANGES (1 P0 + 2 P1, all folded); this
is v2. **Milestone:** Phase 5 · registry discovery (the deferred reaper/flusher/watch win
noted in D1/D2). **Branch:** `codex-aira-startup-discovery`. **Depends on:** M21 (daemon),
D1 (reaper), D2 (journal flusher), D3 (watch).
**v2 moves:** an explicit **identity guard** — discovery rejects an entry whose on-disk
`app.Discover` identity no longer matches the registry breadcrumb `{ProjectID,
WorktreeID}`, so a reused `Root` can never make the daemon reap/flush the wrong project
(Sol r1 P0); discovery is **periodic** (start + interval, skip already-cached), closing
the transient-unavailable-at-startup gap (Sol r1 P1); the honesty invariant is **"no
reconcile/rebuild or resurrection-capable writes"**, not "no file writes" — the enabled
D1 outbox + D2 `journal.jsonl` materialization are the intended effect (Sol r1 P1).

## 1. Goal and honest scope

The daemon's D1 lease-reaper (`server.go:273` `runReaper`) and D2 journal-flusher
(`server.go:314` `runJournalFlusher`) iterate **`s.scopes`** — the per-worktree scope
cache — which is populated **only** by `storeForScope` on a client request
(`server.go:626/648`). So a project's expired leases and deferred journal entries are
reaped/flushed **only after some request touches that project this daemon lifetime**. A
project that a *previous* daemon lifetime registered, and that no request has touched
since this daemon started, is invisible to the reaper/flusher/watch until touched — its
expired leases can block its ready-queue indefinitely.

**This milestone** enumerates the registry — on daemon start and then periodically — and
builds each registered project's scope into `s.scopes` (via the existing `storeForScope`),
so the already-running reaper/flusher/watch cover **every** registered project proactively.

**Coverage + why periodic (Sol r1 P1).** Post-D7b every registration flows through the
daemon — `ensure-scope`/`register` calls `storeForScope`, which caches the scope — so any
project registering through *this* running daemon is already scope-cached; discovery is
mostly a no-op in steady state (skip-cached is cheap). It exists to cover projects
registered by a *prior* daemon lifetime and untouched since this daemon started, **and**
entries that were transiently unavailable (worktree on a not-yet-mounted path, config
mid-write) at an earlier discovery pass and later become valid. A single discover-at-start
would leave that transient class invisible until the next daemon restart, so discovery
**re-enumerates on an interval** (`AIRA_DAEMON_DISCOVERY_INTERVAL`, default modest, e.g.
the reap interval), skipping entries already in `s.scopes`. This is not a continuous
reconciler (§ honesty below) — it only *builds scopes*, cheaply, for the existing
background work.

**Honest boundary — no reconcile/rebuild, no resurrection (NOT "no file writes", Sol r1
P1).** Discovery itself **only** reads each project's config (`app.Discover` — read-only
config discovery, no store open, no git mutation) and builds the scope; it **never**
reconciles or rebuilds. Building a scope does trigger the intended D1/D2 background
effects — `storeForScope` reaps expired leases (writing `lease.lapse` to the outbox) and
the flusher materializes `journal.jsonl` from the already-committed outbox — and those
writes are the **point** of the milestone, not a violation. What discovery must never do
is create/rewrite a `.aira/*.md` ticket/finding file or otherwise **resurrect** a scope's
git-file intent, or run the full reconcile D2 deliberately deferred (git-file-intent risks
worktree-resurrection + finding-lock starvation). A registry entry whose worktree/config
is gone, or whose on-disk identity no longer matches the breadcrumb, is **skipped**, never
built, never resurrected.

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

### 3.2 Reconstruct a scope from a registry entry (shared builder + identity guard)

Move `scopeFromProject` into `internal/daemon` as exported
`daemon.ScopeFromProject(project app.Project, paths Paths) (WorktreeScope, error)` (the
`WorktreeScope` type already lives in `internal/daemon`; `cmd/aira/dispatcher.go` updates
its call site to `daemon.ScopeFromProject`). Discovery then, per registry entry:
1. `project, err := app.Discover(ctx, entry.Root)` — read the on-disk config.
2. **Identity guard (Sol r1 P0):** verify `project.ProjectID == entry.ProjectID &&
   project.WorktreeID == entry.WorktreeID` (and the canonicalised `CommonDir`/`GitDir`
   match the breadcrumb). If they differ, `entry.Root` now hosts a **different** project
   than the one registered — **skip** (do NOT build). Without this, `storeForScope` would
   validate the freshly-discovered scope against *itself* (self-consistent) and the daemon
   would reap/flush the wrong project.
3. `scope, err := daemon.ScopeFromProject(project, s.Paths)` → `s.storeForScope(scope)`.
   This **reuses** the exact cache + ready-barrier + reap-on-build, so a project
   concurrently touched by a request dedups on the cache key (no double build, no race).

### 3.3 The discovery goroutine (start + periodic)

A new `s.runRegistryDiscovery(ctx, interval)` launched from `Serve` **after** `s.db` is
set and the reaper/flusher goroutines start, and **after** `Ready` is signalled —
discovery must **not** block bind/serve/`Ready` (it is I/O over N projects). Each pass:
1. `entries := store.ListRegistryEntries(s.Paths.RegistryPath)`; on read error, log and
   skip this pass (never crash the daemon).
2. For each entry **not already in `s.scopes`** (cheap membership pre-check under `s.mu`,
   released before I/O), run §3.2. **Skip-not-fail** on any of: `app.Discover` error
   (worktree/config gone or invalid), the identity-guard mismatch, `ScopeFromProject`
   error, a `storeForScope` `CodeProjectInvalid`, or `ctx` cancelled. Each skip is logged
   with its reason; a bad entry never resurrects a worktree and never aborts the pass.
3. Successful entries are now in `s.scopes` (reaped once on build), so the reaper/flusher/
   watch cover them.
4. Sleep `interval` (or `ctx.Done()`); re-enumerate. The pre-cached skip keeps steady
   state near-free — only genuinely-new/previously-transient entries pay `app.Discover`.

The first pass runs immediately (post-`Ready`), so a prior-lifetime project's expired
leases are freed at startup, not only after a request. Observable: log a one-line summary
per pass when it changes anything (`discovered N new / M registered, K skipped`).
`AIRA_DAEMON_STARTUP_DISCOVERY=0` disables discovery entirely (tests + escape hatch);
`AIRA_DAEMON_DISCOVERY_INTERVAL` tunes the period (default modest, ≥1s floor). Default on.

### 3.4 Concurrency + idempotence

Discovery calls `storeForScope` exactly as request handlers do — same `s.mu`, same
`key`, same ready-barrier. A request that touches project P while discovery is mid-flight
either finds P already cached (discovery won) or builds it (request won) and discovery's
later `storeForScope(P)` is a cache hit. No new lock, no new invariant. Discovery holds
no lock across `app.Discover` I/O (it builds the `WorktreeScope` first, then calls
`storeForScope`, which takes `s.mu` only for the cache insert).

## 4. Scope

**In:** §3.1 `store.ListRegistryEntries` + exported `RegistryEntry`; §3.2 move
`scopeFromProject` → `daemon.ScopeFromProject` (client call site updated) + the identity
guard; §3.3 `runRegistryDiscovery` goroutine wired into `Serve` (post-`Ready`,
non-blocking, skip-cached, skip-not-fail, **start + periodic**); `AIRA_DAEMON_STARTUP_
DISCOVERY` toggle + `AIRA_DAEMON_DISCOVERY_INTERVAL`.

**Out (stated, not silent):** registry pruning of dead entries (a separate concern;
discovery only *skips* them); reconcile/rebuild-on-discovery (the D2-deferred
git-file-intent risk — explicitly NOT revived); any change to the reaper/flusher/watch
bodies themselves (they already iterate `s.scopes`).

## 5. Testing

- **Discovery populates scopes:** a registry with project P (built by a prior "daemon",
  i.e. registered then the scope cache cleared) — after `Serve` + discovery, `s.scopes`
  contains P and P's expired lease is reaped without any client request to P. (real
  socket / real store, `AIRA_REAL_SOCKET`-gated.)
- **Skip-not-fail — missing worktree:** a registry entry whose `Root` was deleted →
  discovery skips it, logs, and still discovers the sibling valid entries; the daemon
  serves normally.
- **Identity guard (Sol r1 P0):** an entry whose recorded `{ProjectID, WorktreeID}` no
  longer matches `app.Discover(entry.Root)` — i.e. `Root` now hosts a **different**
  project — is **skipped**, and crucially the daemon does NOT build or reap/flush the
  other project. Assert no scope for the wrong identity is created and the other project's
  leases are untouched.
- **Skip-not-fail — invalid/absent config:** an entry whose config is gone/corrupt →
  skipped, daemon healthy, other entries covered.
- **Periodic catches a transient (Sol r1 P1):** an entry unavailable on pass 1 (worktree
  absent) that becomes valid before pass 2 is discovered on pass 2 without a client
  request or restart. (Drive with a short `AIRA_DAEMON_DISCOVERY_INTERVAL`.)
- **Non-blocking:** `Ready` fires before the first discovery pass completes (inject a slow
  `app.Discover` or many entries; assert `Ready` is not gated on discovery).
- **Idempotence/concurrency + skip-cached:** a request touching P during discovery yields
  exactly one cached scope for P (no double build); a subsequent discovery pass skips the
  already-cached P (no re-`app.Discover`); a project registered *after* start via the
  daemon is scope-cached by the request path.
- **Toggle:** `AIRA_DAEMON_STARTUP_DISCOVERY=0` → no discovery (start or periodic);
  touched-only behaviour preserved.
- **`ScopeFromProject` move:** the client dispatcher path is unchanged (same scope built);
  a table/golden equality against the pre-move output.
- **No resurrection, journal ALLOWED (Sol r1 P1):** discovery does not create/rewrite any
  `.aira/*.md` ticket/finding file and does not reconcile/rebuild; assert that over a
  fixture project (no ticket-file resurrection), while the expected D1 outbox +
  D2 `journal.jsonl` materialization for a discovered project is permitted (not asserted
  absent).

## 6. Risks

- **R1 — a huge/one-slow registry stalls startup work.** *Mitigation:* discovery is a
  post-`Ready` background goroutine; serving never waits on it; per-entry failures are
  isolated (skip-not-fail); machine-local registries are small.
- **R2 — worktree resurrection / reconcile-rebuild.** *Mitigation:* discovery only
  `app.Discover`s (read-only config) + builds scopes; it never reconciles/rebuilds; a gone
  worktree / identity-mismatched entry is skipped. Test asserts no `.aira/*.md` resurrection
  (the D1 outbox + D2 `journal.jsonl` for a discovered project are the intended effect, not
  a violation).
- **R5 — a reused `Root` reaps the wrong project (Sol r1 P0).** *Mitigation:* the §3.2
  identity guard rejects any entry whose on-disk `{ProjectID, WorktreeID}` differs from the
  breadcrumb before `storeForScope` is ever called.
- **R3 — races with request-driven `storeForScope`.** *Mitigation:* discovery uses the
  same seam + cache key + ready-barrier; dedup is automatic; no new lock.
- **R4 — layering.** *Mitigation:* `internal/daemon` already imports `internal/app`
  (`server.go:19`); moving `ScopeFromProject` into `daemon` is import-legal and removes
  the duplicate.

## 7. Sol build-review checklist

1. Discovery runs post-`Ready`, non-blocking; serving/bind never waits on it; `ctx`
   cancellation stops it cleanly.
2. **Identity guard (P0):** an entry whose on-disk `{ProjectID, WorktreeID}` differs from
   the breadcrumb is skipped BEFORE `storeForScope`; a reused `Root` never causes a
   wrong-project scope/reap/flush.
3. Skip-not-fail is total: `app.Discover` error, config invalid, identity mismatch,
   registry read error → logged skip, never a daemon crash, never a resurrected worktree;
   remaining entries still discovered; one bad entry never aborts the pass.
4. Reuses `storeForScope` (same cache key, ready-barrier, reap-on-build) — no new lock, no
   double build under a concurrent request, no race; the skip-cached pre-check keeps
   periodic passes near-free.
5. No reconcile/rebuild or resurrection on discovery (only `app.Discover` read-only +
   scope build); the D2 git-file-intent deferral is not revived; the D1 outbox + D2
   `journal.jsonl` for a discovered project ARE allowed (intended), not asserted absent.
6. `store.ListRegistryEntries` handles a missing/empty/partially-corrupt registry
   (torn-tail tolerance consistent with `readRegistry`).
7. `ScopeFromProject` move preserves the exact client-side scope (byte/field-identical);
   client call site updated; no behaviour change on the request path.
8. `AIRA_DAEMON_STARTUP_DISCOVERY` toggle + `AIRA_DAEMON_DISCOVERY_INTERVAL` work;
   default-on; periodic (start + interval, skip-cached) closes the transient-unavailable
   gap; `ctx` cancellation stops it cleanly.
9. Honesty: closes the prior-lifetime + transiently-unavailable untouched-project gap; new
   registrations still populate via the request path; not overclaimed as a continuous
   reconciler.
