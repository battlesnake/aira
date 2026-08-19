# Daemon registry discovery — proactive reaper/flusher for all registered projects, v3

**Status:** plan — Sol r1→r2 → APPROVE-PLAN → Fable code-grounded plan-gate → GATE-PASS
(1 P0 + 3 P1 + 5 P2, folded here); this is **v3** ("no re-gate needed if the amendments
land"). **Milestone:** Phase 5 · registry discovery (the deferred reaper/flusher win noted
in D1/D2). **Branch:** `codex-aira-startup-discovery`. **Depends on:** M21 (daemon), D1
(reaper), D2 (journal flusher).
**Key moves:** an **identity guard** on `ProjectID`+`WorktreeID` (hash-derived, symlink-
safe) rejects a reused `Root` before any build (Sol r1 P0); **periodic** discovery (start +
interval, skip already-cached) closes the transient-unavailable gap (Sol r1 P1); the
honesty invariant is **"no reconcile/rebuild or resurrection-capable writes"** — the D1
outbox + D2 `journal.jsonl` (and the once-per-build `Register` breadcrumb append) are the
intended effect, not violations (Sol r1 P1). **v3 (Fable):** discovery joins the daemon
**drain barrier** (own ctx + done channel, joined before DB close — M21 single-writer;
Fable P0-1); the guard is `ProjectID`+`WorktreeID` **only** (the CommonDir/GitDir compare
is unimplementable + symlink-false-fail-prone; Fable P1-1); the membership pre-check uses a
recorded **covered-worktrees set** (the `s.scopes` key isn't reconstructable from a
breadcrumb; Fable P1-2); `ListRegistryEntries` tolerates a torn tail (`readRegistry` does
NOT; Fable P1-3); coverage is **reaper + flusher** (watch builds from its own request, not
`s.scopes`; Fable P2-2).

## 1. Goal and honest scope

The daemon's D1 lease-reaper (`server.go:273` `runReaper`) and D2 journal-flusher
(`server.go:314` `runJournalFlusher`) iterate **`s.scopes`** — the per-worktree scope
cache — which is populated **only** by `storeForScope` on a client request
(`server.go:626/648`). So a project's expired leases and deferred journal entries are
reaped/flushed **only after some request touches that project this daemon lifetime**. A
project that a *previous* daemon lifetime registered, and that no request has touched
since this daemon started, is invisible to the reaper/flusher until touched — its
expired leases can block its ready-queue indefinitely.

**This milestone** enumerates the registry — on daemon start and then periodically — and
builds each registered project's scope into `s.scopes` (via the existing `storeForScope`),
so the already-running reaper/flusher cover **every** registered project proactively.

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

Add `store.ListRegistryEntries(registryPath string) ([]RegistryEntry, error)` — an
exported lister returning an **exported** `RegistryEntry` struct carrying at least
`{ProjectID, WorktreeID, CommonDir, Root, Prefixes, RequirementPrefixes}`. Missing/empty
registry → empty slice, not an error. **It must tolerate a torn tail (Fable P1-3):**
`readRegistry` (`store.go:3128`) fails the *entire* read on any decode error
(`E_CONFIG_INVALID`), and `repairJSONLTail` heals only on the *append* path (when some
`Register` runs) — which is exactly absent in the prior-lifetime-untouched scenario this
milestone targets, so a crash-torn tail would make every discovery pass a permanent no-op.
`ListRegistryEntries` instead returns the successfully-parsed **prefix** on a trailing
decode error (consistent with `repairJSONLTail`'s torn-tail definition, `store.go:3063`),
so discovery still covers the intact entries. The daemon already holds `s.Paths.RegistryPath`.

### 3.2 Reconstruct a scope from a registry entry (shared builder + identity guard)

Move `scopeFromProject` into `internal/daemon` as exported
`daemon.ScopeFromProject(project app.Project, paths Paths) (WorktreeScope, error)` (the
`WorktreeScope` type already lives in `internal/daemon`; `cmd/aira/dispatcher.go` updates
its call site to `daemon.ScopeFromProject`). Discovery then, per registry entry:
1. `project, err := app.Discover(ctx, entry.Root)` — read the on-disk config.
2. **Identity guard (Sol r1 P0; Fable P1-1):** verify `project.ProjectID == entry.ProjectID
   && project.WorktreeID == entry.WorktreeID` — **these two IDs only**. Both are
   sha256-hex of the **symlink-resolved** common/git dirs (`app` `hashID` == `store`
   `hashPath`), so they are robust to symlinked/relative roots; a raw `CommonDir`/`GitDir`
   string compare is unimplementable (the breadcrumb has no `GitDir`) and false-fail-prone
   (`entry.CommonDir` is canonicalised, `project.CommonDir` from `Discover` is not), so it
   is **not** used. If the IDs differ, `entry.Root` now hosts a **different** project —
   **skip** (do NOT build). Without this, `storeForScope` would validate the freshly-
   discovered scope against *itself* (self-consistent) and the daemon would reap/flush the
   wrong project.
3. `scope, err := daemon.ScopeFromProject(project, s.Paths)` → `s.storeForScope(scope)`.
   This **reuses** the exact cache + ready-barrier + reap-on-build, so a project
   concurrently touched by a request dedups on the cache key (no double build, no race).

### 3.3 The discovery goroutine (start + periodic, drained)

A new `s.runRegistryDiscovery(ctx, interval)` launched from `Serve` **after** `s.db` is
set and the reaper/flusher goroutines start, and **after** `Ready` is signalled —
discovery must **not** block bind/serve/`Ready` (it is I/O over N projects). Each pass:
1. `entries := store.ListRegistryEntries(s.Paths.RegistryPath)`; on read error, log and
   skip this pass (never crash the daemon).
2. For each entry **not already covered** (cheap membership pre-check — §3.3.1 — under
   `s.mu`, **released before any I/O**; `s.mu` is non-reentrant and `storeForScope` takes
   it, so holding it across the build self-deadlocks), run §3.2. **Skip-not-fail** on
   **any** per-entry error: `app.Discover` error (worktree/config gone), the identity-guard
   mismatch, `ScopeFromProject` error, **any** `storeForScope` error (`CodeProjectInvalid`,
   `E_PREFIX_OWNERSHIP_CONFLICT`, `E_CONFIG_INVALID` on config drift, …), or `ctx`
   cancelled. Each skip is logged with its reason; a bad entry never resurrects a worktree
   and never aborts the pass.
3. Successful entries are now in `s.scopes` (reaped once on build), so the reaper + flusher
   cover them.
4. Sleep `interval` (or `ctx.Done()`); re-enumerate. The covered-set skip keeps steady
   state near-free — only genuinely-new/previously-transient entries pay `app.Discover`.

The first pass runs immediately (post-`Ready`), so a prior-lifetime project's expired
leases are freed at startup, not only after a request.

**Lifecycle — join the drain barrier (Fable P0-1).** `storeForScope`'s reap-on-first-build
runs under `context.Background()`, so cancelling discovery's `ctx` cannot interrupt a
build in flight; and Serve's shutdown waits `<-reaperDone`/`<-flusherDone`, then closes the
DB and releases the daemon flock. Discovery therefore mirrors the reaper/flusher lifecycle
**fully**: its own derived ctx + `discoveryDone` channel, and Serve joins `<-discoveryDone`
in the same drain barrier before `close(drained)` / DB close (covered by the existing
`ErrDrainTimeout` retain-instance path). An undrained discovery goroutine using the DB
after close would violate the M21 single-writer invariant if a second daemon started.

**Register side effect (Fable P2-4):** each uncached build calls `storeForScope` →
`store.NewScope` → `Register`, which appends a fresh breadcrumb to `registry.jsonl` and
`mkdir`s `<common>/aira/locks` — benign, once-per-build (the covered-set skip prevents
repetition), and part of normal scope construction. The no-resurrection test must NOT
assert `registry.jsonl` unchanged.

Observable: log a one-line summary per pass when it changes anything (`discovered N new /
M registered, K skipped`). **Toggle (Fable P2-5, interval-var convention like
`reapIntervalFromEnv`):** `AIRA_DAEMON_DISCOVERY_INTERVAL` tunes the period (≥1s floor,
default modest e.g. the reap interval); `AIRA_DAEMON_DISCOVERY_INTERVAL=disabled` (or `0`)
disables discovery entirely (start + periodic) — a single knob, no separate boolean.

### 3.3.1 The covered-worktrees pre-check (Fable P1-2)

`s.scopes` is keyed by `root\0common\0gitDir\0worktreeID\0ConfigDigest`, which a registry
breadcrumb (no `GitDir`, no `ConfigDigest`) cannot reconstruct, and `store.Store` exports
no `WorktreeID` getter — so discovery cannot map-lookup `s.scopes` directly. Add a
`s.coveredWorktrees map[string]struct{}` (guarded by `s.mu`) recorded whenever a scope is
inserted (in `storeForScope` and the bootstrap-init path), keyed by the breadcrumb's
`WorktreeID`. Discovery's pre-check is `coveredWorktrees[entry.WorktreeID]` — cheap, and
`WorktreeID` uniquely identifies the worktree for coverage purposes. Without it, the
fallback (always `app.Discover`, let the cache dedup) costs ~3 `git` subprocesses × N
entries every pass, breaking "steady-state near-free".

### 3.4 Concurrency + idempotence

Discovery calls `storeForScope` exactly as request handlers do — same `s.mu`, same
`key`, same ready-barrier. A request that touches project P while discovery is mid-flight
either finds P already cached (discovery won) or builds it (request won) and discovery's
later `storeForScope(P)` is a cache hit. No new lock, no new invariant. Discovery holds
no lock across `app.Discover` I/O (it builds the `WorktreeScope` first, then calls
`storeForScope`). **Correction (Fable P2-1):** `storeForScope` holds `s.mu` across the
*full* `store.NewScope` build — including `Register`'s registry flock + `BEGIN IMMEDIATE`
— not merely the cache insert; so each uncached discovery build briefly serialises all
request handling, exactly as a request-driven build does today. The conclusion (safe seam
reuse, no new invariant) is unchanged.

## 4. Scope

**In:** §3.1 `store.ListRegistryEntries` (torn-tail tolerant) + exported `RegistryEntry`;
§3.2 move `scopeFromProject` → `daemon.ScopeFromProject` (client call site updated) + the
`ProjectID`+`WorktreeID` identity guard; §3.3 `runRegistryDiscovery` goroutine wired into
`Serve` (post-`Ready`, non-blocking, **start + periodic**, skip-cached via the
covered-worktrees set §3.3.1, skip-not-fail on any error, **joined in the drain barrier**);
`AIRA_DAEMON_DISCOVERY_INTERVAL` (single knob; `disabled`/`0` off).

**Out (stated, not silent):** registry pruning of dead entries (a separate concern;
discovery only *skips* them); reconcile/rebuild-on-discovery (the D2-deferred
git-file-intent risk — explicitly NOT revived); any change to the reaper/flusher
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
- **Toggle:** `AIRA_DAEMON_DISCOVERY_INTERVAL=disabled` (and `=0`) → no discovery (start or
  periodic); touched-only behaviour preserved.
- **Drain barrier (Fable P0-1):** a discovery pass blocked mid-build at shutdown → `Serve`
  waits on `discoveryDone` before closing the DB (the pass completes or the retain-instance
  `ErrDrainTimeout` path fires); no DB use after close. Assert `Serve` returns only after
  discovery has drained.
- **Torn-tail registry (Fable P1-3):** a `registry.jsonl` with a valid prefix + a
  crash-torn final line → `ListRegistryEntries` returns the prefix and discovery covers the
  intact entries (not a permanent no-op).
- **`ScopeFromProject` move:** the client dispatcher path is unchanged (same scope built);
  a table/golden equality against the pre-move output.
- **No resurrection, journal + Register ALLOWED (Sol r1 P1 / Fable P2-4):** discovery does
  not create/rewrite any `.aira/*.md` ticket/finding file and does not reconcile/rebuild;
  assert no ticket-file resurrection over a fixture project, while the expected D1 outbox +
  D2 `journal.jsonl` materialization **and** the once-per-build `Register` breadcrumb append
  to `registry.jsonl` for a discovered project are permitted (NOT asserted absent).

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

1. Discovery runs post-`Ready`, non-blocking; serving/bind never waits on it. **Drain
   barrier (P0-1):** its own ctx + `discoveryDone`, joined by `Serve` before DB close /
   flock release (like reaper/flusher) — no DB use after close (M21 single-writer);
   `ErrDrainTimeout` retain path covers a stuck build.
2. **Identity guard (P0):** an entry whose on-disk `{ProjectID, WorktreeID}` differs from
   the breadcrumb is skipped BEFORE `storeForScope`; a reused `Root` never causes a
   wrong-project scope/reap/flush.
3. Skip-not-fail is total: `app.Discover` error, config invalid, identity mismatch,
   registry read error → logged skip, never a daemon crash, never a resurrected worktree;
   remaining entries still discovered; one bad entry never aborts the pass.
4. Reuses `storeForScope` (same cache key, ready-barrier, reap-on-build) — no new lock, no
   double build under a concurrent request, no race; the **covered-worktrees set** (§3.3.1,
   recorded at every scope insert) is the pre-check keeping periodic passes near-free;
   `s.mu` is released before `storeForScope` (non-reentrant).
5. No reconcile/rebuild or resurrection on discovery (only `app.Discover` read-only +
   scope build); the D2 git-file-intent deferral is not revived; the D1 outbox + D2
   `journal.jsonl` **and the once-per-build `Register` breadcrumb append** for a discovered
   project ARE allowed (intended), not asserted absent.
6. `store.ListRegistryEntries` **tolerates a torn tail** (returns the parsed prefix — note
   `readRegistry` itself does NOT; the append-path `repairJSONLTail` never runs for an
   untouched project) + a missing/empty registry.
7. `ScopeFromProject` move preserves the exact client-side scope (byte/field-identical);
   client call site updated; no behaviour change on the request path.
8. `AIRA_DAEMON_DISCOVERY_INTERVAL` (single knob; `disabled`/`0` off; ≥1s floor) works;
   default-on; periodic (start + interval, covered-set skip) closes the transient-
   unavailable gap; ctx cancellation + drain stop it cleanly.
9. Honesty: closes the prior-lifetime + transiently-unavailable untouched-project gap; new
   registrations still populate via the request path; not overclaimed as a continuous
   reconciler.
