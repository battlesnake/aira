# AIRA-68 — "admission ledger reserve leak": investigation and plan (v4)

Ticket: `.aira/tickets/AIRA-68.md` (P0, `bug`, labels `admission daemon dogfood`).
Programme: `docs/superpowers/plans/2026-09-04-simplification-programme-plan.md` §7 Phase 1,
order 4. Rigor: **Tier A** (§6). Worktree: `aira68-ledger-reserve-leak`, base `d878d9a`.
Primary file per §7.1: `internal/daemon/admit.go`.

**v4 is the built plan, after four review rounds across two independent lineages (Codex/Sol ×3,
DeepSeek-pro ×1).** Full log in §8. The substantive changes across versions: v1's
`ConfineScopeAbsent` syscall primitive is **deleted** in favour of a signal derived from the
scan the evaluator already runs (v2); v2's over-strong "the job's processes are gone" proof
claim is **withdrawn** and replaced by an exactly-calibrated one (v3); and v3's reclaim is made
safe against the **destructive** half of the scope-id ABA, which carrying a waiter pointer did
not cover (v4).

---

## 1. What the ticket claims, and what the evidence actually shows

The ticket claims a **reserve leak**: "reserves that were legitimately granted are not being
released when their owning job actually exits, accumulating over the daemon's uptime". Its
evidence is one `aira confine --list` reading — `63138336K granted / 61952M ceiling across 23
admitted jobs` — set against "only 3 scopes actually exist and are live", plus an earlier
reading of 48 GB / 14 jobs, read as growth.

### 1.1 The `Jobs` count and the scope table count different populations

`confineManagement` builds the summary as (`internal/daemon/confine_manage.go:131-144`):

```go
snapshot := s.admitSliceSnapshot(path)
totalJobs := addJobCountClamp(snapshot.outstandingJobs, snapshot.adoptedJobs)
```

`outstandingJobs` is incremented once per **granted admission waiter** (`admit.go:918`),
whatever that waiter is; `Jobs` then adds a third population on top:

| Population | Counter | Has a cgroup scope? | Appears in the `--list` table? |
|---|---|---|---|
| `aira confine` job | `outstandingJobs` | yes | **yes** |
| `aira confine-reserve` per-test reservation (AIRA-69) | `outstandingJobs` | **no** | **no** |
| scan-adopted scope (AIRA-74) | `adoptedJobs` | yes | yes |

`aira confine-reserve` sends **no** `scope_id`/`name`/`owner`
(`internal/runner/confine_reserve_linux.go:37-42`; the tuple is optional at
`admit.go:1194-1217`), so its waiter has `scopeID == ""`. It creates no cgroup scope at all —
it is a pure ledger reservation for one pytest test running *inside* somebody else's scope.
It is nonetheless counted as an "admitted job", while the table above the summary lists
**only scopes**.

So "N admitted jobs" and "M rows in the table above" are not comparable quantities, and
`N ≫ M` is the **expected** reading whenever a `--delegate-ram` pytest suite is running.
Nothing in the output says so.

### 1.2 Live measurement — what it does and does not establish

Ten samples, 20 s apart, against the live shared daemon, comparing the reported `Jobs` against
a `ps`-derived count of live admission client processes. Committed reproduction:
`docs/dev/aira68-ledger-sample.sh`.

```
granted        ceiling   jobs   live confine-reserve + confine = clients
64094336K      62720M     11     13 +  8 = 21
45787048K      62656M     12     13 +  3 = 16
47480424K      62208M     19     20 +  3 = 23
46774384K      62272M     18     18 +  3 = 21
47630160K      62208M     19     20 +  3 = 23
48166864K      62080M     21     20 +  5 = 25
53043844K      62080M     21     21 +  8 = 29
43560764K      62592M     13     11 + 10 = 21
48261924K      61760M     26     22 + 10 = 32
50354784K      61696M     27     20 + 10 = 30
```

**What this establishes:** `Jobs` and `granted` **fall as well as rise** (21 → 13 jobs;
64 GB → 43.5 GB, repeatedly, at high churn), and the ledger tracks live client churn in both
directions. The reading that generated the ticket — an aggregate climbing toward the ceiling —
**is not reproduced**, and the aggregate is not monotone.

**What this does NOT establish** *(both reviewers, P0; Sol pressed it again on v2)*:
`Jobs ≤ clients` is **not** proof of zero ghosts. `pgrep` counts queued-but-ungranted clients
too, and a leaked subset can accumulate underneath legitimate reservations that fall faster
than it grows. A cardinality comparison cannot establish a per-lease property, and "the
aggregate is not monotone" does not *refute* an accumulating subset.

**Therefore, stated as AIRA itself requires:** the ticket's reading is **not reproduced**; the
existence of a residual leaked subset is **`unevaluated`** by this measurement — neither
established nor refuted. A quiescence observation (zero live clients ⟹ `Jobs` must be 0)
would settle it directly and was watched for opportunistically on the shared box; the outcome,
including a miss, is recorded in the PR as `unevaluated` rather than as a pass. The per-lease
property is established for the *code* by T1a/T1b (§5) and made *observable in production* by
D1's vanished-scope counter (§2).

### 1.3 Code audit of every path that can change `outstanding`

`queue.outstanding` / `outstandingJobs` are written in exactly two places:

- **charge**: `evaluateAdmitQueue` at grant (`admit.go:914-918`), under `queue.mu`, setting
  `waiter.accounted = true` in the same critical section;
- **discharge**: `releaseAdmitWaiter` (`admit.go:1009-1037`), under `queue.mu`, guarded by
  `state == admitGranted && accounted`, idempotent via the `admitReleased` terminal state.

Every exit from `admitConnection` runs `defer release()` (`admit.go:589-597`) — including the
peer-close, timeout, shutdown, write-failure and rejection paths. `releaseActiveConfine`
(kill-drop and the stale sweep) reaches the same state-guarded function. `queue.adopted` is
recomputed from scratch on every scan pass and therefore cannot accumulate at all.

**No charge-without-discharge path was found, and both plan reviewers audited the same code and
found none either.** That is a negative result from source reading, and it is reported as
exactly that — not as proof.

### 1.4 What *does* explain "granted ≈ ceiling with a small live footprint"

Not a leak — two known, separately-owned over-charges:

- **AIRA-62** (Phase 1 order 3, *not this ticket*): `cmd/aira/main.go:905-907` does
  `if maximum > 0 { reserve, reservePinned = maximum, true }`, collapsing `--memory-max` into
  the reserve unconditionally. The live invocation
  `aira confine --delegate-ram --memory-max 32G --memory-reserve 512M -- make merge-gate`
  is therefore charged **32 GB**, silently discarding the 512 MB the caller declared, for a job
  whose design contract (`confine.go:18-23`) says a delegate reserve is "a small PINNED
  framework overhead". One such job held half the slice ceiling during the ticket's own
  observation window. **Out of scope here** and untouched.
- The per-test reservation population is *designed* to be large in count (AIRA-69 Model 3).
  Only the reporting makes it look like wreckage.

### 1.5 Root-cause honesty

**The reading that produced the P0 — 23 admitted jobs against 3 live scopes — is fully
explained without any leak, and is not reproduced by measurement; no code path producing an
accumulating leak was found by three independent readings of the source.** The observation
that generated the ticket is real; the diagnosis attached to it is a misreading, caused by a
diagnostic that prints one fused number where three populations exist. Whether a *residual*
leaked subset exists remains `unevaluated` (§1.2) — which is precisely why D1 ships a
production instrument for it rather than a verdict.

The ticket is not closed as "not a bug". Two real defects are fixed here: the **diagnostic that
manufactured the misdiagnosis** (D1) and the one **genuine hole in the existing ledger-reclaim
backstop** found while auditing for the alleged leak (D2). A third gap (D3) is documented and
deliberately not built.

---

## 2. Defects fixed

### D1 — `confine --list` fuses three ledger populations into one uncomparable number

**Where:** `admitSliceSnapshot` (`admit.go:267-300`), `confineManagement`
(`confine_manage.go:131-144`), `ConfineSliceReserve` (`runner/confine_manage.go:45-57`),
renderer (`cmd/aira/main.go:2224-2232`).

**Why it is a defect, not a nicety.** `CLAUDE.md`: *"AIRA is primitives, not judgement…
Ambiguous selectors are refused."* The summary invites exactly one comparison — its job count
against the table printed immediately above it — and that comparison is invalid. It cost a P0
ticket, an evening's investigation, and a recommendation to restart a shared daemon as a
"mitigation".

**Fix.** In the *same single locked pass* the snapshot already makes, report counts **and**
bytes per population, plus a ghost count:

- `ScopeJobs` / `ScopeBytes` — connection-held grants **with** a scope id.
- `ReservationJobs` / `ReservationBytes` — connection-held grants **without** a scope id
  (`confine-reserve`); these have no row in the table by construction.
- `AdoptedJobs` / `AdoptedBytes` — scan-adopted scopes with no live connection.
- `VanishedJobs` / `VanishedBytes` — a **subset of** `ScopeJobs`: grants whose scope was
  observed by the ≤1 s confine scan and is now absent (§2 D2's `scopeVanished` bit). This is
  the closest available instrument to the ticket's actual question, for the population where
  any answer is physically possible, available immediately rather than after the 15-minute
  reclaim TTL.

  **Named for what it observes, not for a verdict** *(Sol round 2, P1)*. It is **not** called
  `Ghost*`: a scope can be empty and removed while the job's leader is still alive, having
  migrated itself into a sibling cgroup — a real, repository-witnessed behaviour
  (`internal/runner/descendant_escape_linux_test.go:440
  TestRealCgroupConfineWitnessesSiblingEscape`; the descendant-escape attestation work
  witnesses and attests escape but deliberately does not prevent it, and keeps `ScopeContained`
  leader-only). "Its scope was seen and is now gone" is a fact; "the job is dead" is a
  verdict this daemon cannot issue, and `CLAUDE.md` forbids issuing it anyway.

**Two instruments, two different bug classes, stated precisely** *(reviewers: v1 overclaimed
here)*:

| Instrument | Detects | Does **not** detect |
|---|---|---|
| `VanishedJobs`/`VanishedBytes` | a scope-backed lease whose scope no longer exists | scope-less reservations (no cgroup artifact — §2 D3); a lease whose scope came and went entirely between two scans; a migrated leader still alive (it is *counted*, correctly, as scope-gone) |
| Job **and** byte residuals vs the incremental counters | counter/waiter-list skew (a lost decrement, a double decrement) | a stuck waiter that is consistently present in *both* accountings |

The residual is reported as **two signed deltas, independently** (jobs and bytes), because the
single most likely mutation — dropping the `outstanding -=` while keeping `outstandingJobs--`
— is byte-only. Negative byte deltas are rendered with an explicit sign and never passed
through `formatReserveBytes`, which floors at `0B`.

**Rendered form** (existing two lines unchanged; the third is new, the fourth conditional):

```
slice reserve: 48G granted / 61G ceiling across 23 admitted jobs
slice queue: 0 queued waiters, freeze idle
  of which: 3 confine scope(s) 24G, 20 scope-less reservation(s) 14G, 0 adopted scope(s) 0B
  LEDGER INCONSISTENCY: jobs +2, bytes -1073741824 unattributable
```

and, when `VanishedJobs > 0`:

```
  2 lease(s) 4G whose scope was observed and is now gone — reclaimed at the 15m TTL
```

That scope-less reservations never appear in the table above is stated in the
`ConfineSliceReserve` doc comment (where a JSON consumer reads it), not printed every run.

### D2 — the stale-lease sweep can never reclaim a lease whose scope is already gone

**Where:** `releaseStaleGrantedLeasesPass` (`internal/daemon/confine_reaper.go:181-202`).

```go
reaped, reapErr := runner.ReapScopeIfEmpty(candidate.path, candidate.scopeID, nil)
if reapErr != nil || !reaped {
    continue
}
```

`ReapScopeIfEmpty` opens the scope directory via `openConfineReapDirectory` →
`confineReapOpenat` (`runner/confine_manage_linux.go:203-259`). If the directory **does not
exist** that returns raw `ENOENT`, so the pass takes `reapErr != nil` and skips — *every pass,
forever*. AIRA-49's one backstop for "a granted lease that never transitions out of granted" is
blind to the case where the scope is already gone: it can reclaim only leases it can
*physically reap*, and there is nothing left to reap. This is exactly the ticket's stated
failure shape, and the only instance of it provable from source.

**Fix — driven entirely off the scan the evaluator already runs**, with no new syscall
primitive *(v1 proposed `runner.ConfineScopeAbsent`; both reviewers showed it unsafe — see
§8)*:

`evaluateAdmitQueue` already performs a ≤1 s `ListConfines` scan and already walks
`queue.waiters` in the same locked block to build its `held` set. `listConfines` enumerates
**every** `.aira-CONFINE-*` directory under the slice (`confine_manage_linux.go:73-83`),
irrespective of population or cap, so scan membership is an authoritative presence test. Two
bits are added to `admitWaiter`, written **only** in that block and **only** when the scan
succeeded:

```go
// scan succeeded; present = set of every scope id the scan enumerated
for _, waiter := range queue.waiters {
    if waiter == nil || waiter.state != admitGranted || waiter.scopeID == "" { continue }
    if _, ok := present[waiter.scopeID]; ok {
        waiter.scopeSeen, waiter.scopeVanished = true, false
    } else if waiter.scopeSeen {
        waiter.scopeVanished = true          // existed → gone: a state TRANSITION
    }
}
```

`scopeVanished` can only ever be set for a waiter whose scope was **positively observed to
exist** and is now **positively observed to be absent** by the same authority the `adopted`
ledger already trusts. On scan failure nothing is written (fail-closed).

The sweep then reclaims on **either** proof, with all existing gates unchanged (`admitGranted`,
`scopeID != ""`, non-zero `grantedAt`, `age >= staleLeaseReleaseGrace` = 15 min). The ordering
below is load-bearing, not stylistic *(Sol round 3, P0)*:

```go
for _, c := range candidates {
    // One locked re-read of the CURRENT facts for this exact waiter pointer.
    // The candidate list is only a pre-filter: by the time we act, the waiter may
    // have been released by its own connection and a replacement admitted.
    live, vanished := s.staleCandidateState(c)      // queue.mu; exact pointer identity
    if !live { continue }
    switch {
    case vanished:
        // NO filesystem call. There is nothing to reap, and issuing a
        // scope-id-keyed destructive rmdir here could destroy a *replacement*
        // job's newly created, still-empty scope.
        proof = "scope observed by the confine scan and then gone"
    default:
        reaped, err := runner.ReapScopeIfEmpty(c.path, c.scopeID, nil)
        if err != nil || !reaped { continue }
        proof = "scope physically reaped as empty"
    }
    s.releaseAdmitWaiter(c.queue, c.waiter)   // pointer identity — see the ABA fix below
}
```

**Why `vanished` must be re-read, and must not reach `ReapScopeIfEmpty`** *(Sol round 3, P0)*.
`ReapScopeIfEmpty` is keyed on the scope-id **string**, so it is subject to the same ABA the
ledger release was: if the stale waiter is released between candidate collection and the sweep
acting, and a replacement with the same scope id is admitted and creates its (briefly empty)
scope, the destructive rmdir would remove *the replacement's* scope. That hazard is
**pre-existing** — it is in today's pass — and it is not fixed by carrying a waiter pointer,
because the pointer guards only the ledger discharge, not the filesystem call. The two changes
above close it: a `vanished` candidate makes no filesystem call at all, and every candidate is
re-validated against its exact waiter pointer under `queue.mu` immediately before acting, with
`vanished` re-read fresh at the same instant rather than trusted from the snapshot.

A residual window remains between the re-validation and the `openat` in the non-vanished
branch. It is bounded by that window and further by construction: a same-id replacement
requires an identical `<name>-<pid>-<base36 unix-nanosecond>` triple
(`confineScopeIDPattern`, `admit.go:192`), and `enqueueAdmitInternal:691-697` refuses a
duplicate `scope_id` while the first is unreleased — so the replacement must be released,
re-enqueued, granted, and have created its scope, all inside that window. Recorded as
negligible-by-construction, and strictly narrower than today's.

**Why the "existed → gone" transition is admissible where mere absence was not.** The existing
empty-scope reclaim is safe against a *stalled launcher* because it is **destructive**: it
removes the directory, so the launcher's next cgroupfs write fails `ENOENT`/`ENODEV` and the
launch aborts cleanly (`confine_reaper.go:105-110`). v1's plain absence check had no such
fence: a launcher stalled past the TTL **before** creating its scope would be observed absent,
have its lease released, then create the scope and run **uncharged** — a genuine over-commit
and a return of the #67 aggregate-OOM class. The transition test closes exactly that: such a
launcher never had `scopeSeen` set, so it is never a candidate, and behaviour for it is
**identical to today's** (T5). A waiter that *did* create its scope created it once and never
re-creates it (`confine_linux.go:586` — `backend.Create` is called exactly once per launch).

**Exactly how strong this proof is — and no stronger** *(Sol round 2, P0; v2's claim here was
wrong)*. A cgroup directory can only be removed when it holds no processes, so "seen then
gone" establishes **that the scope held no processes at removal time**. It does **not**
establish that the job is dead: the leader can migrate itself into a sibling cgroup and keep
running, which this repository witnesses in
`TestRealCgroupConfineWitnessesSiblingEscape`. That is precisely the strength of the
**pre-existing** empty-reap branch's proof, which reclaims exactly the same lease in exactly
the same situation today: the migrated-leader gap is inherited, not introduced.

It is nevertheless the right ledger behaviour, and this is the argument for keeping it rather
than an apology for it:

- A migrated leader is **uncontained by construction**: the descendant-escape attestation
  work witnesses and attests escape, never prevents it, and keeps `ScopeContained`
  telemetry-only (`internal/runner/descendant_escape_linux_test.go`). Its reserve is no longer
  buying containment for anything, because its `memory.max` no longer applies to it.
- The release is **ledger-only**. The sibling cgroup is still inside the slice, so the escapee's
  memory is still charged through `max(current − reclaimable, Σ reserve)`
  (`checkedAvailable`, `admit.go:976`) — the same treatment `evaluateAdmitQueue:780-796`
  already documents for adopted scopes it deliberately under-counts.
- Compared with **today**, releasing is not worse for this case: if the escapee's supervisor is
  alive it releases its own lease momentarily anyway; if the supervisor is wedged, today's
  behaviour is to hold the reserve **forever**, which is the defect this ticket is about.

This is written down as an accepted, pinned gap (T14), not left silent.

**Accepted false negatives (a lease that stays stuck), all in the safe direction:**

- **A scope created and removed entirely between two scans** never sets `scopeSeen`, so if its
  connection then wedges, neither the reclaim nor `VanishedJobs` sees it *(Sol round 2, P1)*.
  Bounded to jobs shorter than the scan interval (≤1 s) that *also* wedge their connection. The
  existing empty-reap branch has the identical blind spot for the identical reason — it needs a
  directory to reap. Not closed here; closing it needs the in-flight-set redesign that is
  candidate 14 / AIRA-29 (Phase 5b).
- **A scope id accepted by the daemon's `confineScopeIDPattern` but rejected by
  `parseConfineScopeID`** (e.g. degenerate pid/stamp fields) is omitted from every scan, so
  `scopeSeen` never becomes true *(Sol round 2, P1)*. Safe direction: never a false reclaim.

**Accepted false positive, unchanged from today:** a long job in the sub-millisecond window
between its own `cleanup` (which removes the scope) and its own `releaseAdmission`
(`confine_linux.go:576` registered before `:600`, so cleanup runs first). Release is idempotent
and the client's own release is then a no-op; documented at `confine_reaper.go:128-141`.

**ABA fix (both reviewers, P1).** `staleLeaseCandidate` carries only `path`/`scopeID`, and
`releaseActiveConfine` then re-**searches** for a granted waiter with that scope id — so a
different waiter could be released. Scope ids embed a PID and a wall-clock stamp; uniqueness is
not enforced anywhere. Fix: carry `*sliceQueue` and `*admitWaiter` on the candidate and call
`releaseAdmitWaiter(queue, waiter)` directly. Pointer identity defeats ABA completely, the
function is already state-guarded and idempotent, and this also applies to the pre-existing
empty-scope branch. `releaseActiveConfine` remains for `confine-kill`, its only other caller.

### D3 — scope-less reservations have no backstop at all: documented, not built

`staleGrantedLeases` skips `waiter.scopeID == ""` (`confine_reaper.go:154`). A
`confine-reserve` lease has **no cgroup artifact whatsoever**, so no reap-based or scan-based
proof of any kind exists for it, and its only release path is its connection closing. This
population is now the *majority* of the ledger (20 of 21 admitted jobs in sample 6), and
`VanishedJobs` is structurally blind to it.

I am **not** building a reclaim path for it. The candidate mechanism — `SO_PEERCRED` plus a
start-tick liveness probe — is real new machinery for a failure mode with **no observed
instance** (§1.2 shows this population being released normally at high churn, and the
the ticket's reading is not reproduced and a residual subset is `unevaluated` — §1.2),
and `architectural-simplicity` is explicit that "keep the
primitive + document the gap" beats stacking machinery for a hypothetical. D1 is the
proportionate response: it makes this population *countable and named*, so a genuine wedge
shows as `N scope-less reservation(s)` standing still with no pytest running.

Recorded as an accepted coverage gap in the `ConfineSliceReserve` doc comment, in
`staleGrantedLeases`' existing gaps list, and pinned by T7 so any future change is deliberate.

---

## 3. Scope and explicit deferrals

**In scope:** `internal/daemon/admit.go` (snapshot split, `scopeSeen`/`scopeVanished` in the
existing scan block), `internal/daemon/confine_manage.go` (populate the report),
`internal/daemon/confine_reaper.go` (vanished-proof reclaim + ABA fix),
`internal/runner/confine_manage.go` (the `ConfineSliceReserve` type — additive fields only),
`cmd/aira/main.go` (renderer, ~15 lines in the confine-list block only),
`docs/dev/aira68-ledger-sample.sh` (committed reproduction).

**No new file in `internal/runner`, no new syscall primitive, no `!linux` stub** — v1's
`ConfineScopeAbsent` is deleted.

**Deferred, deliberately:**

- **AIRA-62** — the `--memory-max` → reserve collapse (§1.4, `main.go:905-907`). Phase 1
  order 3. Untouched.
- **Candidates 14–17** (cgroup tree as the ledger, AIRA-29). Phase 5b, owner-held. This plan
  deliberately does **not** restructure `outstanding`/`adopted`; programme-plan §4.1
  candidate 14 says restructuring before AIRA-68 is understood destroys the evidence, and §1
  is that evidence.
- **A liveness probe for scope-less reservations** — D3.
- **`queue.adopted` staleness on a persistently failing scan.** Frozen, bounded, non-growing,
  already logged once; candidate 14's business.
- **The empty-queue evaluator goroutine left behind when `enqueueAdmitInternal` returns an
  error** (`admit.go:667-706` creates the queue before the ceiling/waiter-cap checks).
  `pruneAdmitRegistry` (`server.go:378`) already collects it. Not a reserve leak. Noted only.
- **`cmd/aira/main.go` collision risk.** Programme §7.1 warns against two items in `main.go` at
  once. The edit here is confined to the confine-list rendering block (~line 2224), ~1300 lines
  from AIRA-62's (`:905`) and disjoint from Phase 4's dispatch work. Recorded rather than
  avoided: leaving the split invisible would defeat D1.

---

## 4. Invariants that must survive

1. `Σ granted reserve ≤ cap − headroom` — untouched. No admission decision reads any new
   field; every addition is diagnostic or reclaim-gating.
2. `outstanding`/`outstandingJobs` remain the authoritative incremental counters on the hot
   path. The derived split is computed **only** in `admitSliceSnapshot` (a `--list`-only path);
   walking per waiter inside `evaluateAdmitQueue`'s grant loop would make it O(n²).
3. One locked snapshot: totals, split, ghosts, queued count and freeze phase all describe the
   same instant. *(Reviewer P1: a two-pass read would itself cry wolf.)*
4. Fail-closed: `scopeSeen`/`scopeVanished` are written **only** on a successful scan;
   `scopeVanished` is never set without `scopeSeen`; nothing fabricates a zero or an absence.
5. Ledger release stays idempotent and terminal (`admitReleased`), so the new reclaim path
   cannot double-discharge against the connection's own later close.
6. Lock order `admitRegistryMu → sliceQueue.mu` unchanged. The new bits are written under
   `queue.mu` in the evaluator and read under `queue.mu` in `staleGrantedLeases` and
   `admitSliceSnapshot`.
7. A stalled launcher that never created its scope is treated **exactly as today**: no
   candidate, no reclaim.

---

## 5. Tests (TDD — each written to fail first)

Real-cgroup tests reuse `cgrouptest.IsolatedScopeParent` / `SkipOrFailRealCgroup` and the
`staleLeaseTest*` fixtures in `confine_reaper_linux_test.go`.

| # | Test | Direction | Mutation that must break it |
|---|---|---|---|
| T1a | End-to-end over the real admit socket: grant *k* scope-less and *m* scope-backed leases, close every client connection, assert `outstanding == 0`, `outstandingJobs == 0`. | false-pass | delete the `outstanding -=` / `outstandingJobs--` pair |
| T1b | Same, but the client is a **subprocess killed with SIGKILL** — the filed failure's actual shape (no graceful close, kernel-delivered EOF). | false-pass | make the peer-close goroutine ignore read errors |
| T2 | Snapshot split: 2 scope-backed + 3 scope-less grants + 1 adopted scope report the right per-population counts **and** bytes; `Jobs`/`GrantedBytes` still equal the totals. | false-pass | **classify every connection-held grant as scope-backed** (drop the `scopeID == ""` branch) — *not* "classify by name", which co-occurs with scope_id and would still pass |
| T3a | Job residual: a fixture whose `outstandingJobs` alone is desynchronised renders `jobs +N`. | false-pass | drop the job residual |
| T3b | **Byte** residual: a fixture whose `outstanding` alone is desynchronised renders a signed byte delta (and does not floor it to `0B`). | false-pass | report only the job residual (v1's design) |
| T4 | **D2:** past-TTL granted lease, scope **seen by a scan and then removed** → reclaimed, `outstanding` back to 0, log says "observed by the confine scan and then gone". | false-pass | restore `if reapErr != nil \|\| !reaped { continue }` |
| T5 | **Sol P0-2 regression:** past-TTL granted lease whose scope was **never seen** and does not exist → **not** reclaimed. | false-fail | set `scopeVanished` without requiring `scopeSeen` |
| T6 | Past-TTL granted lease whose real scope exists **and is populated** by a live process → **not** reclaimed, and `scopeVanished` stays false. | false-fail | treat a scan-present scope as vanished |
| T7 | Younger than the grace, scope seen-then-gone → **not** reclaimed. | false-fail | drop the `age >= grace` gate |
| T8 | D3's gap, pinned at `staleGrantedLeases`: a past-TTL `scopeID == ""` lease is **not** in the candidate list. Asserted on the candidate list, not on the sweep — the sweep would also reject it downstream on scope-id validation, which would make the test porous. | false-fail | drop the `scopeID == ""` skip |
| T9 | **ABA, driven through the real sequence** *(Sol round 2, P2: v2's version was porous — `enqueueAdmitInternal:691` rejects a duplicate `scope_id` while the first is unreleased, and stale-first ordering would let a scope-id search find the right waiter anyway)*. Take the candidate list; **then** release the stale waiter (as its own connection close would) and enqueue+grant a replacement with the same `scopeID` **which creates a real, still-empty scope directory**; **then** run the release step. Assert (a) the replacement is still charged **and** (b) *(Sol round 3, P0)* **the replacement's scope directory still exists** — the destructive half of the ABA. | false-pass | (a) release via `releaseActiveConfine`'s scope-id search; (b) drop the locked pointer re-validation so `ReapScopeIfEmpty` still runs and rmdirs the replacement's empty scope |
| T9b | **Stale `vanished` must be re-read, not trusted** *(Sol round 3, P0)*: a candidate collected as vanished whose scope has since **reappeared and is populated** is not reclaimed. | false-fail | act on the snapshotted `c.vanished` instead of the locked re-read |
| T10 | Scan failure writes **neither** bit: a lease seen, then a scan error, then absence-by-error → not reclaimed. | false-fail | mark vanished when `scanErr != nil` |
| T11 | `VanishedJobs`/`VanishedBytes` report the seen-then-gone lease and it **remains inside** `ScopeJobs`/`ScopeBytes` (a subset, not a fourth population, so the split still sums to `Jobs`). | false-pass | make vanished leases a separate population, breaking the sum |
| T12 | `admitSliceSnapshot` on an absent queue reports the genuine idle zero: `present == false`, all-zero split, **no** inconsistency line, **no** vanished count. | false-fail | return `present: true` for an absent queue *(the v2 mutation — computing a residual before the nil-queue return — was porous: zero counters minus zero populations is still zero)* |
| T13 | CLI: breakdown renders with correct pluralisation/byte formatting; absent `SliceReserve` prints none of the new lines; a zero residual prints no inconsistency line. | false-pass | drop the conditional and print the residual unconditionally |
| T14 | **Migrated-leader gap, pinned** *(Sol round 2 P0; tightened per Sol round 3 P1)*: a past-TTL lease whose leader escaped into a sibling cgroup, leaving its own scope empty. The escaped PID is **proved alive** — read from `/proc/<pid>/stat`'s state field, NOT `kill -0`, which succeeds for a zombie the test never waits on — immediately before **and** after the sweep, so the test cannot pass for a dead escapee. Assert `ScopeJobs` 1 → 0 across the discharge. *(Built against the **reap** branch, not the vanished one: in the realistic fixture the scope is still present at the last scan and this sweep is what removes it. The v3 text asked for `VanishedJobs == 1` here, which would need a fixture contradicting its own scenario. The observation-not-verdict wording is pinned in the renderer test instead.)* | documentation-as-test | drop the liveness proof (the test then pins nothing about the disputed condition); or rename the reported field/line back to `Ghost*` (the renderer test's wording assertion fails) |

**Mutation testing is mandatory** (Tier A): each row's mutation is applied to a throwaway copy
of the tree, the named test confirmed to **fail** against it and to **pass** on the real fix.
Results recorded in the PR.

## 6. Expected yield

- The `--list` summary can no longer be misread as evidence of a leak.
- `VanishedJobs` reports, in one command, how much of the ledger is charged to leases whose
  scope no longer exists — for the population where that is physically observable.
- One provable, permanently-stuck class of ledger lease becomes reclaimable, on a transition
  proof rather than a policy guess.
- An ABA hazard in the existing reclaim path is closed.
- A committed, executable reproduction of §1.2.
- Net new production code ≈ 110 lines; no change to any admission decision.

## 7. Risks

| Risk | Mitigation |
|---|---|
| The vanished-proof reclaim releases a live job's lease | Requires seen→gone **and** ≥15 min past grant; a never-created scope is excluded by construction (T5); ledger-only release; real RSS still charged via `max(current − reclaimable, Σ reserve)`. The one live-job case (migrated leader) is inherited from the existing empty-reap branch, argued in §2 D2 and pinned by T14 |
| The split cries wolf | Equal by construction (`granted && accounted` ⟺ counted), one locked pass, T3a/T3b/T12 pin both directions |
| Overclaiming the leak verdict | §1.2/§1.5 say exactly what is *not reproduced* and what is `unevaluated`; no "refuted" and no "no leak" claim survives in v3 |
| A stuck lease this design still cannot see | Two false negatives named and accepted in §2 D2 (sub-scan-interval scope; unparseable scope id); both are the safe direction and both are pre-existing |
| `main.go` rebase conflict with AIRA-62 / Phase 4 | Edit confined to the confine-list block; recorded in §3 |

## 8. Build review and mutation testing (post-implementation)

Two independent lineages reviewed the **built code**: Codex/Sol (with repo access) and
DeepSeek-pro (inline). Both returned **GATE-FAIL**, and — as on v1 — they found the **same
three P0s independently**, which is the strongest available signal that they are real.

### 8.1 Mutation testing found four porous tests before either reviewer ran

Tier A mandates mutation testing, and it earned its keep. The first run (13 mutations) left
**four tests green against mutations they existed to catch**, including the two headline ones:

| Mutation | Was caught? | Root cause |
|---|---|---|
| Delete the ledger discharge outright (`outstanding -= reserve; outstandingJobs--`) | **NO** | `releaseAdmitWaiter` removes the waiter from `queue.waiters` **outside** the `accounted` guard; `pruneAdmitQueue` then deletes the empty queue, and `admitSliceSnapshot` honestly returns the absent-queue zero. The tests waited for `(0 jobs, 0 bytes)` — **an absent ledger satisfies that without the discharge ever running.** They were reading absence as proof of correctness, for exactly the leak the ticket alleges. |
| Byte-only discharge loss | **NO** | Same, plus the mutation was aimed at a unit test that never calls the mutated code at all. |
| Reclaim on plain absence (drop the `scopeSeen` requirement) | **NO** | Every reaper test sets `scopeSeen`/`scopeVanished` **directly on the waiter**, so **nothing drove `evaluateAdmitQueue`'s scan block** — the producer of the bits, and where the entire safety argument lives. |
| Mark a currently-observed scope vanished | **NO** | Same. |

Fixes: the end-to-end tests now hold one **pinned** client open so the queue can never be
pruned, assert `present == true`, and assert the ledger falls to **the pin's exact
contribution** rather than to zero; and a new `admit_scope_transition_test.go` drives the
evaluator's scan block directly. Final state: **21 mutations, all caught, none porous, none
invalid** (`docs/dev/aira68-mutation-check.sh`).

### 8.2 The three P0s both reviewers found, and the fixes

| Finding | Resolution |
|---|---|
| **P0 (both)** `staleLeaseState` validated under `queue.mu`, **released the lock**, and only then discharged. Between the two, the evaluator can re-observe the scope and clear `scopeVanished` — so the sweep discharges a lease whose reclaim proof has just evaporated. | `releaseAdmitWaiter` split into `releaseAdmitWaiterLocked` (discharge, lock held, reports whether **this** call transitioned) + `afterAdmitRelease` (post-unlock signalling, preserving the `admitRegistryMu → queue.mu` order). The vanished branch is now **one critical section**: validate and discharge together. The reap branch validates, reaps outside the lock (a syscall cannot hold `queue.mu`), then **re-validates and discharges atomically**. Tests: `TestDischargeReapedStaleLeaseRevalidatesBeforeTouchingTheLedger`, `TestReleaseAdmitWaiterLockedReportsOnlyTheCallThatTransitioned`. Mutations m15, m16 |
| **P0 (both)** A `scopeVanished` bit set by a genuine transition **survives a persistently failing scan**, where it can no longer be refreshed or cleared — so the sweep reclaims on a sighting the daemon can no longer confirm. Directly contrary to "a check that cannot establish its result reports `unevaluated`, never a fake pass". | `dischargeVanishedStaleLease` now additionally requires `!queue.adoptedScanFailed`. Fail-closed: while the scan is broken the reserve is **held**, and reclaim resumes the moment the scan recovers. Tests: `...WillNotReclaimAVanishedLeaseWhileTheScanIsFailing`, `...ReclaimsOnceTheScanRecovers`. Mutations m14, m14b |
| **P0 (both)** The scan observes a **pathname**, and cgroup v2 permits renaming a cgroup within its parent — so "scope id absent" is not unconditionally "that cgroup was removed". A renamed, still-populated scope would be read as vanished and reclaimed while the job runs on, still contained. | **Accepted and documented, not built** (`admit.go`, the `scopeSeen`/`scopeVanished` doc comment). Nothing in AIRA renames a scope; closing it needs per-scope inode identity threaded through the scan — real machinery for an externally-injected scenario, which `architectural-simplicity` says to document rather than build. The consequence is bounded exactly as the migrated-leader gap is: the release is **ledger-only**, and a renamed cgroup is still inside the slice, so its memory is still charged through `max(current − reclaimable, Σ reserve)`. The scan-currency requirement above narrows the window without closing it. |

### 8.3 Lower-severity findings

| Finding | Resolution |
|---|---|
| **P1 (Sol)** The rendered line said the scope "is now gone" — a **present-tense** claim about state read up to one scan ago, which the daemon cannot establish at the moment it prints it | Reworded to "whose scope the confine scan **observed and then observed absent**". Renderer test asserts the wording; mutation m12 unaffected |
| **P2 (Sol)** `releaseStaleLeaseCandidate` reported `reclaimed=true` even when `releaseAdmitWaiter` was a no-op because a concurrent ordinary release got there first — a receipt for an act this pass did not perform | The discharge now reports whether **it** transitioned the waiter, and the proof/log follows that. Mutation m15 |
| **P1 (Sol)** The remaining reap-branch destructive-ABA window (between re-validation and `openat`) is untested | Left as the **accepted gap §2 D2 already records**, now also bounded by the post-reap re-validation. Steering that interleaving needs a synchronisation hook in production code; the two discharge paths are asserted directly instead |
| **P2 (DeepSeek)** Pointer ABA if waiters were pooled/recycled | Not reachable: `admitWaiter`s are freshly allocated per enqueue and never pooled, and Go cannot reuse an address while a live pointer is held. Recorded in `staleLeaseActionableLocked`'s doc comment |

### 8.4 Two `internal/runner` flakes, recorded honestly

Six full-suite runs: **four green (exit 0), two red**, each red a *different* wall-clock-tight
test in `internal/runner`, never the same one twice, and never a test this change can reach.

| Test | Evidence |
|---|---|
| `governor_slot_test.go:383 TestGovernorSlotReconnectsWithSameUUID` | Failed at **2.01 s** against a hard `time.After(2 * time.Second)` deadline (one of four in that test, none synchronised to real work). Passes **10/10** in isolation. **Already named in AIRA-20** as a known instance of this class, with the same signature recorded there: "Passed 6/6 in isolation… same wall-clock-tight reconnect-deadline class, not a data race and not caused by the change." |
| `runner_test.go:833 TestRealCgroupTimeoutExitRaceHasOneTerminalWithArbitration` | Real-cgroup timeout/exit arbitration. Passes **10/10** in isolation. A **new instance** of the same class; appended to AIRA-20. |

**Why these are not this change's doing** — stated as what is established, not as a wave-through:

- The complete `internal/runner` diff is **45 added lines: struct fields on
  `ConfineSliceReserve` plus their doc comments.** No function, no control flow, no behaviour in
  that package is touched, and neither test reads that type.
- Both pass 10/10 in isolation; the daemon package containing every behavioural change is green
  in all six runs.
- The failure mode is a fixed wall-clock deadline expiring under full-suite CPU contention on a
  shared, heavily loaded box — precisely what AIRA-20 exists to fix.

**What is *not* established:** three baseline passes of `internal/runner` at the base commit
`d878d9a` (my files reverted, new tests removed) did **not** reproduce either failure, so
"reproduced on an unmodified baseline" is **`unevaluated`** — the flakes are simply too
infrequent to have been caught in three passes. The non-involvement argument above rests on the
diff and on isolation runs, not on a baseline reproduction, and is reported as exactly that.

## 9. Plan review log

Three rounds, two independent lineages: Codex/Sol ×3 (GATE-FAIL, GATE-FAIL, GATE-FAIL →
resolved below) and DeepSeek-pro ×1 (APPROVE-WITH-CHANGES). The two lineages independently
found the *same* two P0s on v1, which is why v2 restructured rather than patched.

*(No Fable review: the Agent tool with `model: "fable"` is not exposed in this execution
context. DeepSeek-pro was substituted as the second independent lineage. Recorded, not
glossed.)*

### v3 → v4 (Codex/Sol round 3 — GATE-FAIL)

| Finding | Resolution |
|---|---|
| **P0** The ABA fix protects only the *ledger* identity, not the **destructive reap**: `ReapScopeIfEmpty` is keyed on the scope-id **string**, so a same-id replacement's newly created, briefly empty scope can be rmdir'd. The switch also acted on a possibly-stale snapshotted `c.vanished` | Two changes in §2 D2: (1) a `vanished` candidate makes **no filesystem call at all** — the destructive reap is bypassed entirely for that class; (2) every candidate is re-validated against its **exact waiter pointer under `queue.mu`** immediately before acting, with `vanished` **re-read fresh** at that instant. The residual window is bounded and argued negligible-by-construction of the scope-id triple. New tests T9(b) and T9b |
| **P1** D3 still said "accumulation claim refuted", contradicting §1.2/§1.5 | Corrected |
| **P1** T14 does not pin the disputed *live-leader* condition | T14 now proves the escaped PID alive (via `/proc/<pid>/stat`, immune to the zombie that `kill -0` reports as live) immediately before **and** after the sweep, and asserts `ScopeJobs` 1 → 0 across the discharge |
| **P2** AIRA-20 mis-cited (it is now the `-race` CI ticket, not escape containment) | Citations changed to the descendant-escape attestation work and `internal/runner/descendant_escape_linux_test.go` directly |
| Confirmed OK | The migrated-leader policy is "honestly scoped and does not require new liveness machinery"; extracting a single-candidate release helper for T9 is reasonable |

### v2 → v3 (Codex/Sol round 2 — GATE-FAIL)

| Finding | Resolution |
|---|---|
| **P0** "directory gone ⇒ job processes gone" is **false**: a leader can migrate to a sibling cgroup and keep running (`TestRealCgroupConfineWitnessesSiblingEscape`), leaving its scope empty and reapable | v2's proof claim **withdrawn**. §2 D2 now calibrates the proof exactly ("no processes were in the scope at removal"), shows the gap is **inherited from the pre-existing empty-reap branch, not introduced**, argues why releasing is still the right ledger behaviour (an escapee is uncontained by construction and is still charged via `current`), and **pins it with T14** instead of leaving it silent |
| **P0** "accumulation refuted" is still unsupported: legitimate reservations can fall faster than ghosts accumulate | "Refuted" removed everywhere. §1.2/§1.5 now say: the ticket's *reading* is **not reproduced**; a residual leaked subset is **`unevaluated`** |
| **P1** `GhostJobs` mislabels a live migrated job | Renamed `VanishedJobs`/`VanishedBytes`; rendered as an observation ("scope was observed and is now gone"), never a death verdict. T14 asserts the wording |
| **P1** scan throttling ⇒ permanent false negative for a sub-scan-interval scope; and ids accepted by `confineScopeIDPattern` but dropped by `parseConfineScopeID` never get `scopeSeen` | Both written down as **accepted false negatives** in §2 D2, both shown to be the safe direction and pre-existing; closing them is candidate 14 / AIRA-29 work |
| **P2** T9's ABA mutation would still pass (duplicate `scope_id` is rejected at enqueue; stale-first ordering) | T9 rewritten to drive the real interleaving: capture candidates → release the stale waiter → grant a same-id replacement → run the release step |
| **P2** T12's mutation is porous (zero minus zero is still zero) | T12's mutation changed to "return `present: true` for an absent queue" |
| Confirmed OK | `unevaluated` → `scanErr` propagation; no new lock-order, race, or O(n²) issue |

### v1 → v2 (Codex/Sol round 1 — GATE-FAIL; DeepSeek-pro — APPROVE-WITH-CHANGES)

| Finding | Lineage | Resolution |
|---|---|---|
| **P0** "no leak" not proven; `pgrep` includes queued clients, and falling totals do not exclude a leaked subset | Sol, DeepSeek | §1.2 rewritten (further corrected in v3): ticket reading **not reproduced**, residual subset **`unevaluated`**; per-lease proof moved to T1a/T1b and to D1's vanished-scope counter |
| **P0** v1's plain absence check is unsafe — the existing reclaim's safety comes from *destroying* the scope; with nothing to destroy, a launcher stalled past the TTL can create its scope **after** the check and run uncharged | Sol, DeepSeek | `ConfineScopeAbsent` **deleted**. Replaced by the scan-derived seen→gone **transition**, which excludes a never-created scope by construction. T5 is its regression |
| **P0** D1's residual cannot detect the alleged leak — a leaked grant consistent in both accountings shows residual 0 | DeepSeek | Claim withdrawn and replaced by the two-instrument table in D1; `VanishedJobs` added as the instrument that *can* detect it |
| **P1** ABA: release by scope-id search can hit a newer waiter | Sol, DeepSeek | Candidate now carries `*sliceQueue` + `*admitWaiter`; pointer-identity release. T9 |
| **P1** residual must be signed and reported for **jobs and bytes independently**; don't floor negatives through `formatReserveBytes` | Sol | Adopted. T3a/T3b |
| **P1** cry-wolf if the snapshot is not atomic | DeepSeek | Already invariant 3; restated explicitly, T12 |
| **P1** porous mutations in T2/T3/T5/T7/T10; T1 should model SIGKILL | Sol | Test table rewritten; every mutation re-chosen (see the table's notes) |
| **P1** simpler: ship D1 + D3 and drop D2 until a leak is reproduced | DeepSeek | Partly adopted: D2's *new machinery* is dropped; what remains is a bug fix in an existing pass using an existing scan, plus the ABA fix. Kept because the skip-forever behaviour is provable from source, not hypothetical |
| **P2** prose: `outstandingJobs` is two populations, `Jobs` is three; AIRA-62 is at `main.go:906` not `:857` | Sol | Corrected in §1.1 / §1.4 |
| **P2** "zero observed instances" unsupported until the measurement is strengthened | Sol | D3 reworded to "no observed instance", scoped to what §1.2 actually shows |
