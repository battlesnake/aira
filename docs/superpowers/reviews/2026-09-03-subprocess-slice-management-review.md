# Subprocess / slice management — architectural review

- **Status:** review for the owner's decision. Nothing in this document has been
  implemented; every proposal is a proposal.
- **Date:** 2026-09-03
- **Reviewer:** Fable (fresh read; no prior involvement in this subsystem's builds)
- **Base commit:** `994abee` (`master`), worktree `review-subprocess-slice-mgmt`
- **Prompt:** the owner, after watching AIRA-51/52/58/59 land in one evening: *"The
  subprocess/slice management in aira is perhaps overcomplicated and hard to
  reason about, hence the bugs."* The primary question is which of the accumulated
  machinery is necessary and which should be cut or merged; bug-finding is
  secondary.
- **Scope read in full:** `internal/runner/{confine,confine_linux,confine_manage,
  confine_manage_linux,confine_reserve,confine_reserve_linux,admission_linux,
  governor_slot,aitest_bootstrap_linux,worker_admit_client_linux,worker_scope_linux,
  cgroup_linux,types,peak_rss_history,resource_estimate}.go`,
  `internal/daemon/{admit,confine_manage,confine_reaper,confine_report,cpuslots,
  governor,watchdog,worker_admit,server,paths}.go`, `internal/pylib/env.go`,
  `internal/pylib/aira_xdist_governor/__init__.py`, the aitest README and the
  supervisor's daemon call sites, `cmd/aira/main.go` (confine dispatch),
  the top-level design spec, the thirty confine/admission/governor/watchdog
  specs and plans since 2026-08-15 (via a digest, spot-checked against the
  originals for every claim used here), the tickets AIRA-4/12/15/16/17/21/22/23/
  24/25/26/27/28/29/49/51/52/58/59, the live `aira.slice` and `aira-daemon`
  units, and one read-only `aira confine --list` snapshot.
- **Not done:** no tests were run (this is a reading review); no code was changed.

---

## 0. Verdict

The subsystem is not one over-engineered design. It is **four admission
mechanisms, two launch paths, and three memory-pressure responders** that were each
reasonable when added and were never reconciled with each other. The per-job
core — cgroup scope, `memory.max = reserve`, `oom.group=1`, a slice ledger
that keeps `Σ reserve ≤ cap − headroom` — is sound and earns its complexity. Most
of the rest either (a) governs the xdist substrate that the project has already
decided to delete, (b) exists to reconstruct in-memory state that the kernel
already holds in the cgroup tree, or (c) is a fallback for a daemon-optional world
the project left on 2026-08-16.

Tonight's bugs are not independent. AIRA-58 (silent 30-minute clamp), AIRA-59
(freeze stalls the queue), AIRA-51 (client prints a reserve the daemon will not
honour), AIRA-52 (owner lost across restart) and AIRA-49 (stuck lease) all come
from the same two structural facts: **the ledger's truth lives in daemon memory
rather than in the cgroup tree**, and **per-test leases were made first-class
citizens of the whole-job admission queue**. Fix those two facts and the class
goes away; keep tuning constants and it will keep coming back.

If the owner acts on three things (§6): finish the xdist retirement that
`2026-09-01-aitest-design.md` §3.8 already commits to, remove the flock fallback
and the "timed out → launch anyway" path, and make the cgroup tree the ledger's
source of truth (which is AIRA-29's banked direction taken one step further).

Along the way I found one bug that materially changes tonight's in-flight fix
(§5 B1: there is a **third** 30-minute clamp, on the client) and one hazard that
I believe explains the AIRA-15 "uncapped 20.6 G unknown-owner job" class (§5 B2).

---

## 1. Method and confidence grading

I read the code first and the specs second, so that the map in §2 describes what
runs, not what was intended. Each proposal in §4 names the incident or spec that
motivated the mechanism it would remove and says how that incident stays covered.
Each finding in §5 carries a grade:

- **HIGH** — traced line by line; I would bet on it without a repro.
- **MEDIUM** — mechanism traced, occurrence not observed; or the trace has one
  step I could not fully verify from source.
- **LOW** — pattern-matched or cosmetic; flagged so it is not lost, not asserted.

File references are `path:line` against `994abee`.

---

## 2. The map — what exists today

### 2.1 What the design asked for, and the two wrappers that grew

`docs/superpowers/specs/2026-08-07-aira-design.md` §14 is explicit: *"AIRA is
the **outer** wrapper (lifecycle/capture/handle/kill); the slice stays the
**inner** wrapper (memory containment)."* The original runner had no admission,
no reserve estimation, no ledger, no reaper, no governor and no watchdog. All of
those arrived after 2026-08-15 in 62 commits touching the subsystem, across ~30
specs, and the
top-level spec's own status line now concedes a *"long tail of later hardening
milestones (confinement, install, watchdog, daemon, project lifecycle, memory
accounting)"*.

There are now **two launch paths** that both create a cgroup scope, admit against
the slice, place a child with `clone3(CLONE_INTO_CGROUP)`, and tear down:

| Path | Entry | Size | Containment semantics |
|---|---|---|---|
| `aira run` | `internal/runner/runner_linux.go` `Launch` (`:230`) | 2,598 lines | admission released **at start** (`runner_linux.go:358`); scope **uncapped** unless `--memory-max`; ledger-recorded, detach, tee, telemetry |
| `aira confine` | `internal/runner/confine_linux.go` `confineWithDeps` (`:373`) | 1,468 lines | admission **held for the job's lifetime** (`admit.go:517-522`); scope `memory.max = reserve` (`confine_linux.go:590-593`); project-less, foreground only |

`confine` was born as the `whale-run` replacement (#54, 2026-08-21) and was
deliberately "project-less, daemon-optional". It then acquired its own admission
(#67), its own list/kill (#68), its own reaper (#72, AIRA-36), its own restart
reconstruction (#74) and its own lease sweep (AIRA-49). The `run` path kept the
older, weaker semantics: #67 §8 records *"Σ bound holds only for confine tenants
(`aira run` keeps release-at-start + uncapped scopes)"* as an accepted residual.
So the spec's "one runner" is today one recorder and one container, with the
container being the one everybody actually uses (`CLAUDE.md`: prefix heavy work
with `aira confine`).

### 2.2 Mechanism inventory

Every distinct mechanism in scope, what it is for, the incident that motivated it,
and what it depends on. LOC is non-test Go unless marked.

| # | Mechanism | Where | LOC | Purpose | Motivating incident / spec | Interacts with |
|---|---|---|---|---|---|---|
| M1 | **Capped slice** `aira.slice` (`MemoryMax=64G`, `MemoryHigh=62G`, `MemorySwapMax=2G`, `CPUWeight/IOWeight=50`) + keepalive anchor + `+memory` delegation | `internal/install`, live unit | — | The only hard bound on aggregate agent memory; kernel-enforced | 2026-05-29 53 GB run killed the desktop (design §14); #55 (`daemon-reload` resets `subtree_control`) | Everything below assumes M1 |
| M2 | **Per-job scope**: `.aira-CONFINE-…` cgroup, `oom.group=1`, `oom_score_adj` 500/800, `nice 19`, `ionice BE7`, two-pipe placement handshake, membership monitor, teardown attestation | `confine_linux.go:535-796`, `cgroup_linux.go` | ~900 | Contain one job; prove placement; never exec unconfined | #54; #20 (descendant escape) | M3 (cap value), M11 (scan) |
| M3 | **Slice ledger** — daemon per-slice FIFO queue; `charge = max(current − reclaimable, outstanding + adopted)`; `available = (max − headroom(jobs)) − charge`; connection-held lease; backfill with grace-freeze; reserve resolution (pinned / per-signature history ×1.15 / OOM ×1.5 / machine p90 prior / fallbacks) | `daemon/admit.go` | 1,185 | Keep `Σ reserve ≤ cap − headroom` so the slice-level OOM never fires and every kill is per-scope contained | D4 #40 (flock unfair); #67 (reserve released at start → aggregate OOM, innocent victims, 2026-08-25); AIRA-4 (37 G head blocks three 512 M jobs, 2026-08-27); AIRA-21 (page cache counted as used, 2026-08-31) | M4, M5, M6, M8, M11, M12 |
| M4 | **Restart adoption** — ≤1/s `ListConfines` scan; every populated scope not held by a live connection contributes `memory.max` (`@dr`: `min(cap, current+64 MiB)`) to `adopted` | `admit.go:604-694` | ~90 | A restart must not forget running jobs' reserves (over-admission window) | #74 (2026-08-27) | M3, M11 |
| M5 | **Stale-lease sweep** — a granted lease older than 15 min whose scope the kernel lets us `rmdir` is released | `daemon/confine_reaper.go:142-202`, `paths.go:73` | ~70 | A lease whose connection somehow never closed must not hold ledger forever | AIRA-49 (2026-09-02; original trigger *still unconfirmed*) | M3, M11 |
| M6 | **Orphan-scope reaper** — 5-min sweep; empty + supervisor PID dead + age ≥ 2 min + not pending + no live lease → recursive `AT_REMOVEDIR` | `confine_reaper.go:35-70`, `confine_manage_linux.go:143-310` | ~230 | Dead supervisors leave empty cgroup dirs | #72; AIRA-36 (31 orphaned nested aitest trees) | M11 |
| M7 | **Flock fallback** — if the daemon cannot be dialled *or any transport error occurs*, poll raw `max − current ≥ reserve`, take a machine-wide `flock`, release it at `Start` | `admission_linux.go:136-238`, `confine_linux.go:709-711` | ~120 | Pre-daemon self-gating; retained as "advisory degradation" | #29 (2026-08-15, an agent's wait loop died, peer idle 6 h); D4 kept it; #67: "only transport failure falls back to flock" | Bypasses M3 entirely; scope left **uncapped** (`confine_linux.go:591` requires `lock == nil`) |
| M8 | **Per-test RAM lease** — the xdist plugin spawns `aira confine-reserve --pinned` per test; each is an ordinary waiter in M3's queue with a 300 s wait | `aira_xdist_governor/__init__.py:338-371`, `confine_reserve*.go`, `main.go:899-951` | 140 Go + ~120 py | Bound Σ RSS of xdist workers under `--delegate-ram` | #69 (two suites' 15 corpus tests exceeded 64 G); AIRA-12 (cumulative worker RSS) | M3 (shares queue, freeze, 256-waiter cap) |
| M9 | **CPU active-set governor** — daemon-side park/activate scheduler with per-job floor, youngest-first, RAM-ordered activation with hysteresis and a skip-bound force valve; a per-worker `aira governor-slot` relay process; plugin checkpoints every 10 s | `daemon/governor.go`, `runner/governor_slot.go`, `cpuslots.go` | 755 + 390 + 27 | Stop N sessions × `-n auto` from thrashing 16 cores | #49 (flock slot dir); cooperative-scheduler Slices 2–3 (2026-08-30); live `AIRA_SCHED_MODE=enforce` | M3 (`admitAvailable` read; `governor.signal()` on every ledger release) |
| M10 | **cpu.weight aging** — scope weight 100→10 over 30 min, parent-side timer, fail-open | `confine_linux.go:69-76, 1012-1137` | ~130 | Old contending scopes yield CPU to fresh bootstraps | AIRA-14 (execnet bootstrap blank); Slice 1 | kernel only |
| M11 | **Scope scan** `ListConfines` — reads `cgroup.procs`, `memory.current`, `memory.max` per scope | `confine_manage_linux.go:65-141` | ~80 | Existence + occupancy for `--list`, `--kill`, M4, M5, M6 | #68 | used by M3/M4/M5/M6 |
| M12 | **Peak-RSS history** — `confine-report` at exit → `confine_peak_history` (last 20/signature); per-signature stats + machine p90 | `confine_report.go`, `store/confine_peak_history.go`, `resource_estimate.go` | ~150 | Size the reserve from evidence, not a flat 4 G | #50, #67 | M3 (reserve resolution) |
| M13 | **Worker admission** — per-outer-scope ledger keyed `(job_id, outer_scope)`; live `memory.current` + worst-case `Σ worker caps + supervisor RSS ≤ outer cap − 64 MiB`; grants a child scope `memory.max = estimate`, `memory.high = 0.8×` | `daemon/worker_admit.go`, `worker_admit_client_linux.go`, `worker_scope_linux.go`, `aitest_bootstrap_linux.go` | 470 + 120 + 50 + 144 | aitest's per-worker kernel containment inside one confine job | aitest design (2026-09-01); AIRA-38 review wave | M2 (outer scope), M6 (nested reap) |
| M14 | **Memory watchdog** — every 2 s read `MemAvailable`; below 8 GiB for 3 samples, SIGTERM→SIGKILL the heaviest *uncapped*, claude-descendant, ≥2 GiB, unprotected process; flock authority + `systemctl is-active whale-watchdog` interlock; audit events to every project journal | `daemon/watchdog.go` | 967 | Kill an agent's *unconfined* runaway before the host OOMs | #59/#64/#65 (2026-08-24 17:32 event, Σ 69 GiB fleet); first real enforce fire 2026-08-26 | independent of M1–M13; **exempts every `.aira-` cgroup** |
| M15 | **`--delegate-ram`** flag — reserve = 512 MiB pinned (unless `--memory-max`), scope cap = daemon ceiling (history ×1.15, min 4 G, default 48 G, ≤ slice − headroom) or 48 G fallback, `oom_score_adj=800`, `@dr` dirname marker, governor + aitest env injection | `confine_linux.go:446-462, 600-608, 661-693`; `admit.go:296-357`; `main.go:857-859` | ~150 | Mark a job that sub-allocates (a test suite) so its own reserve is overhead, not peak | #69; AIRA-15 (uncapped `@dr` scope drove slice OOM); AIRA-27 (800 vs 500) | M3, M8, M9, M13 |

### 2.3 Life of one `aira confine -- cmd` (what actually happens, in order)

1. CLI (`main.go:835-897`): resolve `--memory-reserve`/`AIRA_CONFINE_RESERVE`;
   **if `--memory-max` is given the reserve becomes `--memory-max`, pinned, for
   both classes** (`:857-859`); owner from `--owner` → env → worktree id →
   `unknown`; daemon socket path (if unresolvable: one stderr line and M7).
2. `confineWithDeps` (`confine_linux.go:373`): validate; resolve slice (default
   `aira.slice`, refuse if the unit is missing or the effective cap is not finite
   — `:434-438`, the #54 precondition); ensure `+memory` (`+cpu` best-effort).
3. Reserve default: delegate → 512 MiB pinned; else 4 GiB unpinned
   (`:446-462`). Signature = argv joined (`:463-468`). Scope id
   `CONFINE-[@dr-]name-pid-stamp` (`:886-896`).
4. Start the 15 s "waiting for memory admission (reserve X)" printer — X is the
   **client's** number, not the daemon's (`:494-508`; AIRA-51).
5. `admitConfine` → `Runner.admit` → `admitThroughDaemon`
   (`admission_linux.go:240`): **clamp `maxWait` to 30 min** (`:252-255`),
   transport deadline = clamped wait + 1 s (`:258-262`), send `admit`.
6. Daemon `admitConnection` (`admit.go:385`): global slot (1,024); validate
   (**clamp `max_wait_ms` to 30 min** again, `:912-914`); read slice `memory.max`;
   resolve reserve (pinned / history / p90 / fallback, `:217-290`); resolve the
   delegate scope ceiling (`:296-329`, a *second* history read); reject
   `E_ADMIT_TOO_LARGE` if `reserve > max − headroom(outstandingJobs+1)`; enqueue;
   wait for grant, deadline, peer close, or shutdown.
7. Evaluator goroutine (`admit.go:600-750`), once per kick and every 250 ms:
   ≤1/s `ListConfines` scan → `adopted`; read `memory.current/max/stat`; walk
   waiters in arrival order; each waiter that fits is granted (`grantedAt` set,
   `outstanding += reserve`); the first that does not fit and has been queued
   longer than the backfill grace **freezes every later waiter for this pass**
   (`:718-728`).
8. Grant frame → client. The connection is now the lease
   (`admit.go:517-522`). On any transport error at the client instead: M7.
9. Back in `confineWithDeps`: create scope; `cpu.weight` aging; `oom.group=1`;
   **non-delegate + daemon-admitted → `memory.max = reserve`** (`:590-593`);
   delegate → `memory.max = scope ceiling or 48 G` (`:600-608`); write env
   (governor and/or aitest coordinates, `:671-693`); spawn `aira __confine-setup`
   with `CgroupFD` (`:696`); child verifies its own cgroup and cap ancestry,
   applies adj/nice/ionice, hands back a handshake, waits for release, execs.
10. Parent verifies membership, identity; starts the membership monitor; releases
    the child; waits; reads `memory.peak`/`oom_kill`; `confine-report`s the peak
    (M12); attests teardown; prints the status line; removes the scope; closes
    the lease connection (which releases the ledger).

Ten steps, seven of which are daemon-side and three of which (5, 6, 8) contain
a 30-minute constant.

### 2.4 The four admission paths

```
                   whole job                  per test (xdist)          per worker (aitest)
                   ─────────                  ────────────────          ───────────────────
 client            aira confine               aira confine-reserve      aira worker-admit
                     │                           │                        │
                     │ admit (scope_id,name,owner)│ admit (pinned, no scope)│ worker-admit (job,outer,bytes)
                     ▼                           ▼                        ▼
 daemon      ┌──────────────────────────────────────────┐        ┌──────────────────────┐
             │ M3 slice ledger  (admit.go)               │        │ M13 worker ledger     │
             │  one FIFO queue per slice                  │        │  one map per outer    │
             │  outstanding + adopted, backfill, freeze   │        │  scope; own headroom, │
             │  256 waiters, 30 min cap                   │        │  own poll, own 30 min │
             └──────────────────────────────────────────┘        └──────────────────────┘
                     │ any transport error                             (no fallback: refuse)
                     ▼
 client      ┌──────────────────────────┐        ┌──────────────────────────┐
             │ M7 flock fallback         │        │ M9 CPU governor           │
             │  raw max−current ≥ reserve│        │  governor-slot relay ↔    │
             │  lock released at Start   │        │  daemon active set        │
             │  scope left UNCAPPED      │        │  (reads M3, never charges)│
             └──────────────────────────┘        └──────────────────────────┘
```

Three ledgers (M3, M13, M9's active set) and one bypass (M7). M3 and M13 share
no code and use different headroom constants (2 GiB + 64 MiB/job vs 64 MiB),
different poll intervals (250 ms evaluator vs 200 ms loop), and the same
copied 30-minute clamp. M9 reads M3 but M3 signals M9 on every release
(`admit.go:809-811`).

### 2.5 The constants, and where the 30 minutes came from

| Value | Where | Meaning |
|---|---|---|
| 30 min | `admission_linux.go:82` `runnerAdmitWaitCap` | **client** clamp on `max_wait_ms` before it is sent; transport deadline derives from it |
| 30 min | `admit.go:25` `admitWaitCapMs` | daemon clamp for `admit` |
| 30 min | `worker_admit.go:349-351` | daemon clamp for `worker-admit` |
| 30 min | `confine_linux.go:833` | default when `--admit-timeout` is unset |
| 30 min | `main.go:711`, `:911` | `confine-reserve --max-wait` upper bound (**rejected loudly**, unlike the three clamps) |
| 30 min | #29 spec §1 | the original `aira run` `admission_max_wait` **default** |
| 300 s | `confine_reserve.go:12`, plugin `:16`, `governor_slot.go:21` | per-test lease wait; governor relay wait |
| 30 s | aitest `supervisor.py:179` | worker-admit wait |
| 60 s → **2 h live** | `paths.go:36`; drop-in `admit-backfill-grace-mitigation.conf` | backfill grace before freeze |
| 15 min | `paths.go:73` | stale-lease TTL |
| 5 min / 2 min | `paths.go:38-39` | orphan reaper interval / grace |
| 2 GiB + 64 MiB×jobs | `admit.go:33-34` | slice headroom |
| 64 MiB | `worker_admit.go:23` | worker ledger headroom |
| 4 GiB | `confine.go:17` | no-history whole-job reserve |
| 512 MiB | `confine.go:23` | delegate "framework overhead" reserve |
| 48 GiB / 4 GiB / 15 % / 64 MiB | `confine.go:26`, `admit.go:35-37` | delegate scope ceiling default / min / safety / adoption margin |
| 256 / 1,024 | `admit.go:26-27` | per-slice waiters / global admit connections |
| 8 GiB / 16 GiB / 3 / 2 GiB / 5 s | `watchdog.go:23-28` | watchdog trip / recover / debounce / min victim / grace |
| 500 / 800 | `confine_linux.go:34-35` | `oom_score_adj` non-delegate / delegate |
| 1 s | `admission_linux.go:81` | transport grace beyond the clamped wait |

The genealogy of the 30 minutes: it was the *default* wait in the #29 spec. D4
copied it into the daemon as a *sanity cap* on the wire value; a Sol build-review
of #67 ("a wedged daemon must not strand the client past the advertised cap",
`admission_linux.go:256-257`) copied it into the client as a *transport deadline*;
worker-admit copied the daemon line verbatim. A default became a ceiling in three
places without anyone deciding it should be a ceiling. AIRA-58 found two of them.

### 2.6 Size and churn

- Non-test Go in scope: **~8,200 lines**; tests: **~11,900**; Python: ~2,300 (of
  which the xdist plugin is 419 and its Go-side integration test 928).
- `confine_linux.go` has been rewritten 22 times, `admission_linux.go` 13,
  `admit.go` 12 — all since 2026-08-15.
- 62 subsystem commits out of 379 repository commits in the same window.

---

## 3. Where the complexity is doing real work

These are hard to reason about **and** earn it. I would not simplify them without
a specific reason.

**3.1 The per-scope containment core (M2).** `memory.max = reserve`, `oom.group=1`,
`clone3(CLONE_INTO_CGROUP)`, the two-pipe handshake so the target never execs
before placement is verified, and the child's own re-verification that it is in an
`oom.group=1` cgroup with a finite cap in its ancestry (`confine_linux.go:1354-1379`).
Each piece closes a real hole: without the child-side check a forged fd could run
uncapped (#54 Sol P0-a); without `oom.group` a runaway's *sibling* dies
(#57 §0); without the handshake a fast-exiting child could escape verification.
The membership monitor and teardown attestation (#20) are telemetry-only and
honest about it (`ScopeContained` stays leader-only). Keep all of it.

**3.2 The invariant `Σ reserve ≤ cap − headroom` and `charge = max(current −
reclaimable, Σ reserve)` (M3 core, `admit.go:752-769`).** The `max` is what makes
per-test leases, adopted scopes and live usage compose without double-counting
(#69 §2a Model 3); the reclaimable discount is what stops a heavy-I/O job from
falsely filling the slice (AIRA-21, with the correct LRU-only subtraction that
keeps shmem charged). The lifetime-held lease (#67) is what turned the aggregate
OOM into a per-scope OOM. A simpler version — release at start, raw
`memory.current` — is exactly the #67 incident. Keep the arithmetic; §4 P4 is
about where its *inputs* come from, not the formula.

**3.3 The finite-cap precondition and fail-closed reads.** Refusing to launch
into an uncapped slice (`confine_linux.go:434-438`), refusing to grant when the
slice memory read fails (`admit.go:700-708`), never fabricating a discount
(`admit.go:1032-1037`). These are one-liners that prevent whole incident classes.

**3.4 Worker admission's worst-case guard (M13, `worker_admit.go:275-281`).**
Live `memory.current` alone would admit workers whose caps sum past the outer
cap; the guard `Σ worker caps + supervisor RSS ≤ outer − headroom` is the
difference between "a runaway worker self-OOMs" and "the whole suite is
group-killed". The comment block is long but the logic is ~40 lines and correct.

**3.5 The kernel-as-witness reap (`reapEmptyConfineScopeTree`,
`confine_manage_linux.go:224-310`).** Using `Unlinkat(AT_REMOVEDIR)` failure as
the subtree-aware emptiness proof, fd-anchored with `O_NOFOLLOW`, is the right
primitive: it cannot false-positive, and AIRA-36 showed why per-node
`cgroup.procs` walks cannot be atomic. Keep.

**3.6 `grantedAt` (AIRA-49 v3).** Measuring lease age from the grant, not the
enqueue, is correct and cheap. Even if the sweep that uses it is simplified
(P4), the timestamp should survive.

**3.7 The watchdog's four predicates and pidfd discipline (M14).** "Uncapped ∧
claude-descendant ∧ heavy ∧ not-protected", revalidated per target before each
signal, is the minimum that makes an unconfined-runaway killer safe on a shared
desktop. It fired for real (2026-08-26). What around it is *not* earning its
keep is in P6.

---

## 4. Where it isn't — proposals

Each proposal: what to cut or merge; why it was not pulling its weight; the
incident that motivated it and how that incident stays covered; what else has to
change. Ordered by leverage.

### P1 — Finish the xdist retirement now: delete M8, M9 and the `confine-reserve` verb

**What:** delete `internal/pylib/aira_xdist_governor/`, `aira governor-slot`
(`runner/governor_slot*.go`), `daemon/governor.go`, `daemon/cpuslots.go`, the
`confine-reserve` verb (`runner/confine_reserve*.go`, `main.go:108-113, 458-460,
675-719, 899-951`), the `AIRA_GOVERNOR_*` / `AIRA_TEST_MEM_*` /
`AIRA_CONFINE_RESERVE_CMD` env plumbing in `pylib/env.go`, the `governor`
verb in `server.go:581-591`, `admitAvailable` and the `governor.signal()` hook
in `admit.go:187-211, 809-811`, and the three `2026-08-30` scheduler-slice specs'
implementations. Roughly 2,000 non-test lines (Go + Python) and ~2,300 test lines
(`governor.go` 755, `governor_slot*.go` ~410, `cpuslots.go` 27,
`confine_reserve*.go` ~150, the plugin 435, plus the env/CLI/dispatch glue).

**Why it is not pulling its weight:** `2026-09-01-aitest-design.md` §3.8 already
lists exactly this set under *"Deleted outright"*: *"This entire stack exists only
to govern xdist workers `aitest` replaces outright."* aitest (merged as AIRA-30,
AIRA-31) uses `aitest-bootstrap` + `worker-admit` only — I grepped
`supervisor.py`/`worker.py`/`__init__.py` for `governor`, `confine-reserve`,
`GOVERNOR`: zero hits. Meanwhile the retained xdist stack is the direct cause of
the worst thing observed tonight: per-test leases are first-class waiters in the
whole-job queue, so **they participate in the freeze** (`admit.go:711-728` treats
every `admitQueued` waiter identically; per-test waiters simply have no
`scopeID`), **they consume the 256-waiter cap** (B5), and **under a freeze every
test re-enters a 300 s wait** because the plugin has no RAM-disabled latch (B4) —
which is the "suite self-stalls with the machine idle" symptom. The governor adds
a 755-line userspace scheduler with park/unpark epochs, RAM-ordering hysteresis
and a skip-bound force valve; its live log tonight shows workers doing "fresh
acquire" every few seconds against one job, i.e. it is running but not doing
anything the kernel's CFS plus M10's `cpu.weight` aging would not do.

**Motivating incidents and coverage:**
- #69 / AIRA-12 (aggregate OOM from xdist workers; cumulative worker RSS): covered
  by M13 — per-worker kernel `memory.max` inside the outer cap, plus the worst-case
  sum guard. This is strictly stronger than a fail-open per-test lease.
- #49 / cooperative scheduler (N × `-n auto` thrash): aitest's worker count is
  bounded per job (`--aitest-workers`, `AIRA_AITEST_MAX_WORKERS_FALLBACK =
  NumCPU`), and cross-job CPU sharing falls to the kernel via M10 (kept). What is
  lost is *cross-job* active-set fairness. The scheduler spec §2 already says it
  is *"not hard preemption"*; the honest bound it delivers is "reclaim within ≤
  max(current test durations)" (Slice 2 plan, corrected). `cpu.weight` aging gives
  a comparable proportional-share outcome with zero protocol.
- AIRA-17 / AIRA-26 (simultaneous cold-start bootstrap blank): **not covered by
  M9 either** — Slice 2 "governs test EXECUTION not pre-plugin execnet BOOTSTRAP"
  (its own plan). Nothing is lost.

**What else changes:** `pylib/pytest_integration_test.go` (928 lines, xdist
governor e2e) goes; `confine_linux.go:661-671` keeps only the aitest env branch;
`--delegate-ram` shrinks to P7's meaning. AIRA-25 and AIRA-26 (class-split
ledger, pre-import floor) are closed as superseded. If any fastest-ee `conftest.py`
still registers `aira_xdist_governor`, it must be removed first or it will spawn a
verb that no longer exists (the plugin fails open, so the failure mode is a log
line, not a break).

### P2 — Remove the flock fallback (M7) and the "timed out → launch anyway" path

**What:** delete `admitWithFlock`, `tryAdmissionLock`, `admissionLockDir`
(`admission_linux.go:146-238, 761-798`), the `lock` field of `admissionResult`,
the `admission.lock != nil` release at `confine_linux.go:709-711`, and the
`"timeout"` → continue mapping at `confine_linux.go:529-530`. A daemon transport
failure becomes `E_CONFINE_UNAVAILABLE` (the client already has the right message
shape at `admission_linux.go:379-383`). Keep one explicit, loud escape hatch —
`--no-admit` / `AIRA_CONFINE_NO_ADMIT=1` — that **still caps the scope** at the
requested or default reserve and prints that it bypassed the ledger.

**Why it is not pulling its weight:** the daemon has been mandatory since M21
(2026-08-16: *"the AIRA daemon becomes mandatory"*), is a systemd service with
`Restart=always RestartSec=2` (#62), and reconstructs the ledger on restart in ~1 s
(#74). The fallback's premise — "the daemon might not be there" — is a
two-second window. Its cost is that **every transport error, not just
daemon-absent, routes through it** (`fail()`, `admission_linux.go:307-329`), and
a flock-path launch is scope-uncapped and ledger-uncharged (B2). AIRA-21 §3.3
already recorded the fallback as *"left RAW — a daemon-less non-delegate scope is
uncapped and slice containment there is a per-process collateral OOM"* and
AIRA-16 lists it as an accepted uncapped class. The "advisory degradation" is
more dangerous than the refusal it avoids: it is the AIRA-15 P1 class
(innocent capped neighbours group-killed by a slice-level OOM that an uncapped
scope drove).

**Motivating incidents and coverage:**
- #29 §0 (an agent's home-made wait loop died; a peer sat idle 6 h): that
  incident was about *having* admission at all, in a daemonless world. With a
  mandatory service, a refusal with a stable code is the honest outcome; the
  agent retries. Nothing silently strands.
- D4 §8 "any fallback path is an advisory window": D4 *documented* the window; it
  did not argue for it.
- #67 "flock release stays at start (else one client stalls everyone)": moot once
  there is no flock.

**What else changes:** `runner_linux.go` `Launch` shares `Runner.admit`; the
`run` face should get the same refusal (it already tolerates `disabled`). The
`Diagnostics` "launching without an admission lock" strings go. AIRA-16's
"residual uncapped AIRA classes" shrinks to `unevaluated` (slice unreadable),
which P4 can also close by capping at the requested reserve.

### P3 — One timeout, honoured end to end

**What:** delete `runnerAdmitWaitCap` and `admitWaitCapMs` clamps
(`admission_linux.go:252-255`, `admit.go:912-914`, `worker_admit.go:349-351`).
The client sends what the user asked for; the transport deadline is *derived from
the request* (`requested + grace`); if an operational ceiling is wanted, it is one
constant, checked once, **rejected loudly** (`E_CONFINE_ARGUMENT_INVALID: --admit-
timeout exceeds Xh`) the way `confine-reserve --max-wait` already is
(`main.go:711`). Make the client's "waiting" line print `requested` and, once the
daemon has resolved it, the daemon's reserve (AIRA-51) — a one-frame "queued
{reserve, basis, position}" ack before the grant would close AIRA-51 and give
AIRA-24 its queue position for free.

**Why:** §2.5. A default became a ceiling by copy. The fix in flight for AIRA-58
addresses two of three sites (B1).

**Coverage:** the only argument for a hard cap was "a wedged daemon must not
strand the client" — that is what the transport deadline is for, and it should
be `requested + grace`, not `min(requested, 30m) + 1s`.

### P4 — Make the cgroup tree the ledger's source of truth (AIRA-29, one step further)

**What:** replace the in-memory `outstanding`/`adopted` pair with a scan-derived
ledger: every ≤1 s (the cadence M4 already runs), the charge for each live scope
is computed from its cgroup files — for an airtight scope `memory.max`; for a
delegate scope AIRA-29's `min(cap, max(coldFloor, peakSoFar + margin))` — plus a
small **in-flight set** of grants whose scope has not yet appeared (TTL ≈ 60 s,
after which the grant is dropped and the client, if still alive, fails its next
cgroup write with `ENOENT` exactly as the AIRA-49 plan already reasons for the
15-minute case). `outstanding` stops being authoritative; it becomes a cache
between scans.

**Why it is not pulling its weight today:** M4, M5, AIRA-52 and half of AIRA-49
exist to keep daemon memory consistent with the cgroup tree. The tree is the
truth: a scope exists iff a job is running; its `memory.max` is its airtight
charge; its `memory.current` is its usage. #74 already computes exactly this for
adopted scopes and AIRA-29 already plans to compute the dynamic charge from the
same scan. What remains in daemon memory after this change is the in-flight set
and per-scope `peakSoFar`, both of which are rebuilt harmlessly on restart
(AIRA-29 §3.7 accepts a cold-floor re-warm).

**Owner attribution (AIRA-52) without new state:** the `@dr` marker already
proves the dirname is a restart-surviving carrier (#74, AIRA-15). Owner is a
validated identity of ≤100 chars over `[A-Za-z0-9._-]` (`confine.go:130-144`);
encode it in the scope id (e.g. `CONFINE-[@dr-]name@owner-pid-stamp`; `@` cannot
occur in a name, and 255-byte cgroup names have room). `--list` then reads owner
from the tree, `freshConfineOwner` and the registry merge go away, and the
`--kill` ownership guard survives restarts.

**Motivating incidents and coverage:**
- #67 (release-at-start): the lease is still held — the scope's existence is the
  lease. Nothing releases until the scope is removed.
- #74 (restart forgets reserves): the scan *is* the ledger; there is nothing to
  forget. The grant→create window is the in-flight set.
- AIRA-49 (stuck lease): a lease is now "a scope dir exists" or "an in-flight
  grant younger than 60 s". A dead supervisor's scope is reaped by M6 (kept);
  its charge disappears with the directory. The 15-minute sweep and its physical-
  reap coupling are no longer needed.
- AIRA-52: dirname-encoded owner.
- AIRA-29's growth race: unchanged from AIRA-29 — bounded by per-scope
  `memory.high = charge` (its §3.5), swap, and `oom_score_adj` steering.
- MF3 from the AIRA-29 review ("dynamic replacement only with a usable scan
  record, else the frozen reserve stands"): applies verbatim; an unreadable scope
  is charged its in-flight/last-known reserve.

**Honest cost:** this is the largest proposal — a rewrite of `admit.go`'s ledger
core (~400 lines) and the deletion of `admit_reconstruction*`, the stale-lease
sweep and most of `confine_manage.go`. It pairs naturally with building AIRA-29
(banked at `aira29-dynamic-reserve`), which is on hold pending aitest; if
AIRA-29 is built, do it this way rather than adding `effectiveCharge` on top of
`outstanding`.

### P5 — Fairness: keep the freeze, remove its pathological population, add introspection

**What:** do **not** redesign the backfill freeze. Keep AIRA-4's mechanism. Do
three cheap things: (1) P1 removes per-test waiters from the queue, which is what
made a 60 s grace fire constantly; (2) make the grace proportional to the head's
own wait (`max(5 min, requested/10)`) rather than a flat number, so a job that
asked for 2 h is protected from starvation on a 12-minute horizon and a job that
asked for 5 min never freezes anyone; (3) show the queue: `--list` prints queued
waiters (reserve, waited, basis) and, when a freeze is active, which head is
being protected. AIRA-59 could not be confirmed live because nothing exposes the
queue (`--list` shows only granted leases).

**Why not remove the freeze:** I checked the alternatives. Size-aware backfill
(freeze only comparable-or-larger requests) weakens the anti-starvation guarantee
— a steady stream of small jobs can hold `Σ small` at exactly the level that
keeps a 32 G head from ever seeing 32 G free. Reservation accumulation is the
freeze under another name. D4 chose strict FIFO for a reason; AIRA-4 relaxed it
for a measured reason (a 37 G head stalled three 512 M jobs that fit 16 G idle).
The freeze is the minimal correct mechanism; it was tuned for a queue of tens of
whole jobs and then fed thousands of per-test leases.

**Live note:** the drop-in `AIRA_DAEMON_ADMIT_BACKFILL_GRACE=2h` is already
applied. That trades AIRA-59 for its opposite (a big head can starve for two
hours). Proportional grace is the middle.

### P6 — Watchdog: keep the killer, drop the vestiges

**What:** delete the `whale-watchdog` interlock (`watchdog.go:877-899, 921-932`)
and the PSI read paths that exist only to decorate events (`readHostPressureFull`,
`eventWithPSI`, the `psiSampled` closure); keep the authority flock. Consider
routing audit events to one daemon-level journal instead of every ready project
(`emitWatchdogEvent`, `:947-967`).

**Why:** whale is gone from code and machine (#55 teardown complete). The
interlock `exec`s `systemctl --user is-active whale-watchdog` with a 2 s timeout
**inside the kill path**, on a box that is by definition thrashing; a bus timeout
degrades the watchdog to observe at the exact moment it is needed (B8). PSI was
demoted to observation-only in #64 because it was the reason the watchdog was
inert; it now costs ~150 lines to decorate a log line.

**Coverage:** #59's interlock existed so two killers would not race; there is one
killer now. #64's decision (MemAvailable-authoritative) is unchanged.

### P7 — `--delegate-ram`: one meaning, and reject contradictory flags

**What:** after P1, the flag means exactly: "this job sub-allocates (aitest);
charge it by usage, not by peak; class it as the preferred slice-OOM victim (800);
inject aitest coordinates". Make `main.go:857-859` class-aware: for a delegate
job, `--memory-max` is the *ceiling*, not the reserve, and giving both
`--memory-max` and `--memory-reserve` is either honoured as (cap, charge) or
rejected — never silently collapsed (B3). Drop the `@dr` adoption special case
once P4 lands (charge comes from the scan for every class).

**Why:** `confine.go:18-23` says a delegate reserve is "a small PINNED framework
overhead"; the live ledger shows a delegate job charged 32 G because
`--memory-max 32G` was on the command line. Two peers' recommended invocation
(`--delegate-ram --memory-max 32G --memory-reserve 512M`, AIRA-58, AIRA-24)
silently discards the 512 M. That is the AIRA-29 under-utilisation class
self-inflicted by a flag interaction.

### P8 — Direction only: one launch path

`aira run` and `aira confine` are two implementations of "launch in a capped
scope with admission" with different containment semantics (§2.1). The design
spec asked for one runner. Long term, `run` should be `confine` + recording
(ledger, capture, detach), not a parallel implementation with weaker guarantees.
This is a multi-milestone refactor and not a top-three item; I list it so the
next `run`-side containment fix is weighed against convergence rather than
patched in place. AIRA-22 (`confine --detach`) is the natural forcing function —
it wants `run`'s detach shim under `confine`'s containment.

---

## 5. Further bugs and hazards

### B1 — There is a third 30-minute clamp, on the client (HIGH)

`internal/runner/admission_linux.go:82` declares `runnerAdmitWaitCap = 30 *
time.Minute`; `:252-255` clamps `maxWait` to it **before** the `admit` frame is
built (`:342`), and `:258-262` derives the socket deadline from the clamped value.
AIRA-58 names only `admit.go:912-914` and `worker_admit.go:349-351`. A daemon-only
fix leaves `--admit-timeout 2h` clamped to 30 min at the client. Worse: if the
daemon cap is raised and the client's is not, the client's transport deadline
(30 min + 1 s) fires first, `readRunnerAdmitFrame` errors, `fail()` closes the
socket and **the request enters the flock fallback** (B2) instead of receiving
the daemon's `E_ADMIT_SATURATED`. Today the two caps are equal and the daemon's
timer (armed at enqueue) beats the client's (armed at dial) by about one second,
which is the only reason the observed symptom is a clean rejection.

### B2 — A transport failure during admission becomes a ledger-bypassing, scope-uncapped launch (mechanism HIGH; live attribution MEDIUM-LOW)

Trace: `admitThroughDaemon` → any error from `writeRunnerAdmitFrame`,
`readRunnerAdmitFrame`, or an invalid frame → `fail()` (`admission_linux.go:307-329`)
→ returns `(_, false, nil)` → `admit()` (`:129-141`) → `admitWithFlock`. Then either:

- (a) `max − current ≥ reserve` on the **raw** slice reading (no reclaimable
  discount, no ledger) → flock taken → `finish("immediate"|"waited", lock)` →
  `confineWithDeps:591` skips the per-scope cap because `admission.lock != nil`
  → the scope launches **uncapped**, the ledger sees it only through
  `memory.current`, and the flock is released at `Start` (`:709-711`); or
- (b) `remaining ≤ 0` → `finish("timeout", "", nil)` (`:211-215`) →
  `confineWithDeps:529-530` maps it to `ConfineAdmissionTimeout` and **continues
  to launch** — while the same condition reported by the daemon is a terminal
  `E_ADMIT_SATURATED`.

A daemon restart during a queue (two happened on 2026-09-02 per AIRA-52; every
deploy is one) closes every waiting `admit` connection at once (`server.go:361`,
`admit.go:486-487`) → every waiting client simultaneously enters (a). This is a
stampede of uncapped, unaccounted launches into the shared slice — precisely the
input the AIRA-15 P1 analysis was looking for (*"INVESTIGATE FIRST: which confine
invocation left the 20.6G job CAP max"*). I have not matched it to that specific
job; I am confident in the path. P2 removes it.

### B3 — `--delegate-ram --memory-max N --memory-reserve R` silently charges N and discards R (HIGH)

`main.go:857-859`: `if maximum > 0 { reserve, reservePinned = maximum, true }`
runs for both classes; `confine_linux.go:459-462` only restates it for
non-delegate. Live at review time: the sole admitted job is `@dr`, `CAP
34359738368`, ledger `32G granted / 1 job`. `confine.go:18-23` documents the
delegate reserve as 512 MiB overhead. AIRA-24's correction records the
up-charge; AIRA-27 suggested rejecting the contradictory pair for non-delegate;
nobody closed the delegate case. Effect: the recommended peer invocation
reserves the whole suite's cap for its lifetime — the measured AIRA-29
under-utilisation (33.6 G reserved / 2.6 G used) by another route.

### B4 — The per-test RAM lease has no disabled latch; under a freeze every test waits 300 s (mechanism HIGH; tonight's attribution MEDIUM)

`aira_xdist_governor/__init__.py:338-371`: on any failure `_acquire_reservation`
logs once (`_log_once` dedups the *log*, not the *attempt*) and returns `None`.
Unlike the CPU side (`_disable_governor`, `:133-137`), nothing marks RAM
governance off. So during an AIRA-59 freeze, each test on each worker spawns
`aira confine-reserve`, queues behind the frozen head, times out at
`_DEFAULT_MAX_WAIT = 300` s (`:16`, `:356`) with `E_ADMIT_SATURATED`, runs
ungoverned, and the next test repeats. Throughput ≈ one test per 300 s per
worker while the machine idles — the owner's observation. P1 deletes the path;
otherwise a `_ram_disabled` latch plus a per-test wait of seconds, not minutes.

### B5 — `admitMaxWaiters = 256` is a cliff shared by whole jobs and per-test leases (MEDIUM)

`admit.go:26, 555-557`: sixteen sessions × 16 workers exceeds 256 and returns
`E_DAEMON_BUSY: too many admission waiters`; the plugin fails open (ungoverned)
with no daemon-side log line. Not observed; certain to fire at the load level the
box saw tonight if enough suites run `-n auto` under the xdist plugin. Gone with
P1.

### B6 — Bookkeeping cost under per-test churn (MEDIUM-LOW, unmeasured)

Each per-test lease is: one `fork`/`exec` of the ~20 MB static `aira` binary,
one socket, one daemon goroutine, one evaluator kick on enqueue and one on
release, each kick re-reading `memory.current/max/stat` and (≤1/s) walking every
scope's four cgroup files, plus `governor.signal()` → a sort of all workers
(`admit.go:809-811`, `governor.go:266-608`). At hundreds of test completions per
second across sessions this is plausibly "aira's own admission bookkeeping is the
dominant process in `top`" (AIRA-59). The owner flagged this design cost in
AIRA-12 (*"even the per-test `confine-reserve` daemon round-trip is suspect at
that scale; prefer AMORTISING"*). I did not measure it. Gone with P1.

### B7 — `E_ADMIT_TOO_LARGE` pre-check ignores adopted jobs' headroom (LOW, known)

`admit.go:429-431` and `:558` size the ceiling with `outstandingJobs + 1`; the
grant path uses `outstandingJobs + adoptedJobs + 1` (`:715-716`). #74 documents
this asymmetry (a too-large request waits out to `SATURATED` instead of failing
fast). Listed for completeness; P4 removes the distinction.

### B8 — The watchdog's kill path execs `systemctl` for a unit that no longer exists (MEDIUM)

`watchdog.go:382, 877-899, 921-932`: every enforce decision runs
`systemctl --user is-active whale-watchdog` with a 2 s timeout; any error other
than an exact `inactive`/`failed` degrades the decision to observe. On a host
under memory pressure a D-Bus round trip is exactly the call that can stall or
fail — so the killer is most likely to stand down when it is most needed. Whale
was removed in #55. P6.

### B9 — `--list` reports `0 granted` for adopted jobs whenever the queue is empty (LOW, cosmetic)

`adopted` lives on the per-slice queue; `pruneAdmitQueue` (`admit.go:815-825`)
deletes the queue when no waiters remain. After a restart, once the first
post-restart job finishes and no waiter is queued, `admitOutstandingReserve`
returns `ok=false` and `--list` prints `slice reserve: 0 granted across 0 jobs`
above a table of populated scopes. Admission itself is safe (the next enqueue
creates a queue and rescans before granting, `:609-612`), and `admitAvailable`
reads "plenty" in the same window (only M9 consumes it). P4 dissolves it.

### B10 — Per-test clamp-to-ceiling can hand one test the whole slice budget (MEDIUM, mechanism only)

`confine_reserve_linux.go:49-53`: a pinned reserve above the ceiling is retried
once **at the ceiling**. A mis-marked `aira_mem` (say `200G`) therefore reserves
`cap − headroom` for one test's duration, freezing every other waiter — a
self-inflicted AIRA-59 head. The clamp message goes to the worker's stderr only.
Gone with P1.

### B11 — Restart changes a delegate job's charge by up to its whole `--memory-max` (LOW)

Combining B3 with M4: pre-restart the live `@dr` job is charged 32 G; after a
restart adoption charges `min(cap, current + 64 MiB)` ≈ 15.5 G
(`admit.go:674-686`). Documented as the safe direction relative to a forgotten
ledger, but it means the same job's charge is a function of whether the daemon
has restarted. P4/P7 make the charge class-consistent.

---

## 6. If the owner can only act on three things

1. **P1 — delete the xdist governance stack** (`aira_xdist_governor`,
   `governor-slot`, `governor.go`, `confine-reserve`). It is already the plan of
   record (aitest §3.8); it removes ~2,000 non-test lines and four of tonight's
   hazards (B4, B5, B6, B10) at once; and it takes per-test leases out of the
   whole-job queue, which is what made the fairness freeze pathological. Do it
   before re-tuning any admission constant, because the tuning problem changes
   shape once the queue holds tens of jobs instead of thousands of tests.

2. **P2 + P3 together — no flock fallback, no silent timeout clamp.** These are
   small, mechanical, and close the two highest-confidence hazards (B1, B2).
   They also make the in-flight AIRA-58 fix correct: without the client-side
   change it is incomplete, and with a daemon-only cap raise it becomes *worse*
   (B1's second paragraph). If the other agent's admit.go change has not merged,
   fold `admission_linux.go:82, 252-262` into it.

3. **P4 — cgroup tree as ledger, built as the AIRA-29 vehicle.** This is the
   structural fix for the class (AIRA-49, AIRA-52, #74, and the next bug of that
   shape). It is also the only proposal that costs real design work, so it should
   go through the two-loop with AIRA-29's banked plan as the base and this review
   as the "why the ledger moves out of memory" argument. Encode owner in the
   scope id as the first slice — it is independent, tiny, and retires AIRA-52.

P5, P6 and P7 are worth doing and each is under a day; P8 is a direction.

---

## 7. Appendix

### 7.1 What I did and did not do

- Read every file in scope in full (listed at the top). Line references were
  taken from the files at `994abee`, not from ticket text.
- Ran exactly one live command against the shared daemon, `aira confine --list`
  (read-only), and read the live unit files and six hours of the daemon journal.
- Did not run the test suites, did not restart anything, did not touch the
  shared slice.
- Used a sub-agent to digest the thirty specs; every spec-derived claim used
  above was spot-checked against the original text (§2.5's genealogy, #67 §8,
  AIRA-21 §3.3, aitest §3.8, D4 §8, AIRA-49 v5).

### 7.2 Size of the proposed deletions (approximate, non-test / test)

| Proposal | Non-test | Test |
|---|---|---|
| P1 xdist stack | ~2,000 (Go + py) | ~2,300 |
| P2 flock fallback | ~150 | ~400 (the flock cases in `admission_linux_test.go`) |
| P3 clamps | ~15 | ~30 |
| P4 ledger rewrite | −~350 net (rewrite ~400, delete ~750) | −~600 net |
| P6 watchdog vestiges | ~150 | ~250 |

### 7.3 Tickets this review would close or re-scope

- Closes: AIRA-25, AIRA-26 (superseded by P1/M13), AIRA-52 (P4 dirname owner),
  AIRA-16's flock-class residual (P2).
- Re-scopes: AIRA-58 (add the client clamp, B1), AIRA-59 (P1 + proportional
  grace + queue introspection, P5), AIRA-24 (queue position from the P3 ack
  frame), AIRA-29 (build as P4), AIRA-28 (remains shelved; P4 keeps the owner's
  dynamic-reserve direction).
- Unaffected: AIRA-17 (bootstrap-admission — still open under every proposal),
  AIRA-22 (`confine --detach` — becomes the P8 forcing function), AIRA-23.
