---
{"schema":1,"id":"AIRA-114","project":"aira","title":"Bound the aggregate over-subscription factor (sum of scope memory.max vs the slice ceiling)","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","deferred-from-aira29","oom","shared-slice"],"hold":false,"relations":[]}
---
Deferred from AIRA-29 (dynamic reserve), reasoning in
`docs/superpowers/specs/2026-09-06-aira29-dynamic-reserve-plan.md` §3.5 and residual §4e.

AIRA-29 removed the property that made the non-delegate confine class airtight. Before it,
`reserve == memory.max` per scope and `Σreserve <= cap - headroom`, so `Σ(memory.max)` could
not exceed the slice and the kernel could not let the aggregate overrun. Charging live usage
breaks that: ten jobs each holding a 20G estimate but using 1G all fit a 64G slice, and could
then demand 200G. The owner explicitly accepted this bounded over-subscription, contained by
each scope's own `memory.max` plus the deployed 500/800 `oom_score_adj` steering.

A v4 draft proposed gating admission on `Σ(scope memory.max) <= factor * ceiling`. It was
dropped for two grounded reasons, both confirmed by the plan-gate reviewer:

- The existing scan cannot supply the cap total correctly. The adoption loop skips
  leaf-`Populated == 0` scopes (every busy aitest outer scope, whose pids are drained into a
  child cgroup) and skips non-finite caps, so a `Σcap` derived from it silently under-counts
  and the "bound" would not bind. Doing it right needs a second, subtree-live cap accounting.
- Every conservative treatment of the residual cases is worse. A scope with no finite local
  `memory.max` is reachable — the flock fallback path launches uncapped when the daemon is
  unavailable — and counting its cap honestly WEDGES the shared slice, while counting it as
  zero makes the bound porous. A wedge of this machine-wide slice is the worst failure mode
  in this subsystem; AIRA-101's whole design exists to make one unrepresentable.

The concrete failure this would bound: several scopes expand between scans until `aira.slice`
reaches its own cap, producing a memcg OOM inside the slice biased only by the class
steering. Not an uncontained host OOM.

Prerequisites for building it: a subtree-live cap accounting, and a decided policy for a
locally-uncapped scope that neither wedges nor lies.

## Resolution (2026-09-06, branch `aira114-aggregate-oversubscription-bound`)

Built per AIRA-29 §3.5 / residual §4e. Both stated prerequisites are met rather than
argued around, and the bound is a **second, independent accounting** — never derived
from the reserve ledger, whose own exclusions are correct for reserves and would have
left this bound unable to bind.

### What the bound is

`internal/daemon/admit_oversubscription.go` (new) derives, on every successful confine
scan and under `queue.mu`:

```
capAggregate = Σ(local memory.max over capped live scopes)
             + Σ(memory.current over uncapped live scopes)
```

and `evaluateAdmitQueue`'s grant loop refuses a queued waiter whose own prospective
scope cap would take that total past `factor * effectiveMaximum`. Default factor
`200` (2×), one env control that is both knob and kill switch:
`AIRA_DAEMON_OVERSUBSCRIPTION_FACTOR` accepts a multiple (`2`, `2.5`) or `disabled`,
and REFUSES anything else with `E_CONFIG_INVALID` (the AIRA-58 rule).

The ceiling is the SAME AIRA-103 pressure-throttled effective maximum the reserve
check uses, so the two gates can never disagree about the size of the slice.

### Prerequisite 1 — subtree-live cap accounting

Liveness is `ConfineRecord.SubtreePopulated` (the kernel's own `cgroup.events`
signal, produced by the scan since AIRA-101), NOT the adoption loop's leaf
`record.Populated`. That is the trap §3.5 named: `BootstrapAitestSupervisor` drains
every pid of an aitest outer scope into a child cgroup, and `podman run
--cgroups=split` does the same, so the largest-capped scopes on the machine read
leaf-empty. Unevaluated liveness counts as LIVE, so an unreadable scope can only
tighten the bound, never loosen it.

The accounting also covers the connection-held population the adoption loop
deliberately excludes, and it counts a granted waiter whose scope does not yet exist
at the cap the daemon is about to hand it — closing the grant → `backend.Create`
window that every launch has. A grant additionally increments the running total in
the same pass, so a burst of grants between two scans cannot clear the bound together.

### Prerequisite 2 — a locally-uncapped scope policy that neither wedges nor lies

An uncapped (`memory.max == "max"`) or unreadable-cap live scope contributes its
MEASURED `memory.current`. Never infinite (which would wedge this machine-wide slice
on a job the daemon never admitted — the failure AIRA-101 exists to make
unrepresentable), never zero (which would make the bound porous against exactly the
flock-fallback population AIRA does not control), and re-read every scan.

Where NEITHER a cap nor a usage reading exists, the total is UNESTABLISHED and the
bound withholds nothing. That fail-OPEN is deliberate and is the opposite of
`liveScopesKnown`: granting exclusivity asserts "you are alone" and must be provable,
whereas this bound only ever withholds capacity, and stalling every job on a shared
slice because a cgroup file could not be read would be an outage caused by a
diagnostic.

### Never-wedge, structurally

Two rules make "the head of an otherwise-idle queue is always admissible" a property,
not a tuning outcome: the parser REFUSES a factor below 1, and a waiter that would be
the only capped scope on the slice is exempt. Both are tested, and the second has a
negative arm proving it is not a blanket exemption. Sub-reservations and scope-less
`aira admit` waiters are never bounded (they create no cgroup); blocking them would
stall the very suites whose exit is the only thing that relieves the bound.

### Honest statement of what is bounded

`Σ(local memory.max)` over the capped population plus `Σ(memory.current)` over the
uncapped one. The uncapped population's growth ABOVE its measured usage is not in the
total; it stays bounded by the slice's own finite cap and by `checkedAvailable`'s
physical `current` term, exactly as before. This does not restore the pre-AIRA-29
airtight property — it makes the worst case a known multiple instead of an unbounded
one, which is what the ticket asked for.

Accepted over-counts, both bounded by one scan interval and both in the
withhold-capacity direction: a `scopeVanished` waiter keeps its cap until its lease is
reclaimed, and nothing decrements the running total on release (only the derive does).

### Incidental fix

`waiter.scopeCeiling` was assigned AFTER enqueue with no lock held, while the
evaluator could already read that waiter. AIRA-29 worked around the race by never
reading the field; this change must read it (a delegate scope's `memory.max` is its
scope ceiling, not its 512 MiB pinned reserve, and that is the largest cap population
on the machine), so the write moved into the waiter constructor under `queue.mu` via
`admitRequest.scopeCeiling`. `go test -race` is clean.

### Operator surface

`confine --list` gains one line, because this adds a NEW reason a job waits and
AIRA-71's lesson is that a silent admission wait reads as a hang:

```
slice scope caps: 20G across live scopes / 24G over-subscription bound
```

Unevaluated renders as `unevaluated / … (not applied while unevaluated)`, never as a
measured `0B`; a disabled bound prints nothing at all. The daemon logs the bound as a
TRANSITION only (the evaluator runs at up to 4/s).

Two honest limits of that line, both raised by the build review and both stated here
rather than rounded off:

- A queue exists only while it has waiters, so once the last one leaves and the queue
  is dropped (`pruneAdmitQueue`) the aggregate reports `unevaluated` even if orphan or
  flock-fallback scopes are still live on the slice. That follows the pre-existing
  absent-queue convention (`adopted` reads 0 there too), but the `(not applied while
  unevaluated)` suffix is slightly stronger than the truth in that state: nothing is
  gated because nothing is asking, and the bound IS applied on the first evaluator pass
  after a waiter arrives. Documented rather than given its own machinery.
- A waiter refused by this bound arms the AIRA-59 fairness freeze exactly as a
  reserve-refused head does, and `if frozen { continue }` precedes the sub-reservation
  exemption. So per-test sub-reservations queued behind an aggregate-blocked head are
  frozen for the hold phase even though only their own suite's EXIT relieves the bound.
  Bounded by the duty cycle's yield phase and identical to the pre-existing behaviour
  for a reserve-blocked head — not a regression from this change, and not fixed here,
  but a real interaction and named as one.

### Tests

`internal/daemon/admit_oversubscription_test.go` (13 tests, exact assertions, each
naming the wrong build a looser assertion would admit) plus
`internal/daemon/admit_oversubscription_real_cgroup_linux_test.go` — the anti-INERT
tier, which builds a real cgroup-v2 slice, gives two real `.aira-CONFINE-*` scopes
real `memory.max` values, DRAINS every pid out of each scope's own `cgroup.procs`
into a child cgroup (so both read leaf-empty while alive), runs the PRODUCTION
`runner.ListConfines` and `readSliceMemory`, and asserts the aggregate equals the
kernel's own two values and that the bound refuses a job the reserve ledger had room
for. Plus `cmd/aira/confine_test.go` for the renderer.

The headline test has three arms: bounded (refused, aggregate exactly 120 GiB),
unbounded (admitted, aggregate reaches 140 GiB = 2.19× the 64 GiB slice — the old
state, driven for real), and a smaller newcomer under the same bound (admitted, which
kills a build that simply refuses everything).

**Mutation battery: 24 deliberate breakages, 24 caught, 0 survivors.** One
(`factor < 1` → `factor <= 0`) was an equivalent mutant caught by a redundant second
guard; removing BOTH lower bounds was then caught, so the rule is genuinely pinned.

### Exit codes

- `aira confine -- go build ./...` → 0
- `aira confine -- go vet ./...` → 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` → 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test -race ./internal/daemon/ -count=1` → 1,
  from three PRE-EXISTING wall-clock-tight failures in
  `sliceceiling_real_cgroup_linux_test.go` ("helper did not acknowledge anon growth"),
  reproduced identically on a clean `origin/master` worktree. With those three skipped
  the same `-race` run is 0. This is the AIRA-20 / AIRA-117 tier, not a regression
  from this change.

### Deferrals

- The bound gates admission only; it never moves a kernel-enforced value, so an
  already-running scope is never squeezed (AIRA-103's principle).
- Making the ADOPTION loop subtree-live is still its own ticket. It is a separate
  reserve-accounting change in the tightening direction and is untouched here; the
  cap accounting has its own liveness reading precisely so the two need not move
  together.

## Build-review round 2 — BLOCK on porous operator-surface tests, fixed

The first review confirmed the admission logic itself as sound and non-porous (12/12
of its own real mutants caught, the real-cgroup anti-INERT test observed running
rather than skipping) but BLOCKed on the OPERATOR-SURFACE plumbing this change adds:
`queue → admitSnapshot → ConfineSliceReserve.CapAggregate{Bytes,Known}` had no test
that could fail. Three findings, all in the test tier; **no production code changed
in this round.**

1. **Vacuous assertion** (`admit_oversubscription_test.go`, the snapshot claim in
   `TestAggregateUnestablishedWithholdsNothing`). It snapshotted a hand-built queue
   that was never put in `s.admitQueues`, and `admitSliceSnapshotFor` returns
   `admitSnapshot{phase}` for an absent path — so `capAggregateKnown` was false BY
   CONSTRUCTION and the assertion could not fail against any implementation.
2. **`admit.go`'s snapshot copy untested.** Deleting
   `capAggregate: queue.capAggregate, capAggregateKnown: queue.capAggregateKnown`
   left the full `internal/daemon` package GREEN (verified: mutant survived, exit 0).
3. **`confine_manage.go`'s wire copy untested, one layer up.** Hardcoding
   `CapAggregateKnown: false` also left the full package green (verified, exit 0),
   because both existing arms of `TestConfineListSliceReserveSummary` pin only the
   default-FALSE fixture.

The consequence was concrete and is why a porous test was the right call to block on:
with either mutant live, `aira confine --list` prints `slice scope caps: unevaluated /
… (not applied while unevaluated)` permanently WHILE the bound is refusing launches.
An operator debugging a wait would read the exact opposite of the truth — the AIRA-71
silent-wait failure with a false statement attached instead of a missing one.

### What was done

- `registerAdmitQueue` helper puts a hand-built queue into `s.admitQueues` under
  `s.admitRegistryMu`, the lock the daemon itself uses.
- `TestAggregateUnestablishedWithholdsNothing` now registers its queue and asserts
  `snapshot.present` FIRST. That guard is the load-bearing half: it is what excludes
  the absent-queue zero, so the unestablished-bit claim beside it is a reading of a
  real queue rather than a tautology.
- New `TestSnapshotCarriesTheEstablishedAggregate` is the POSITIVE arm the negative
  one structurally cannot be (`known == false` is also what a build that never copied
  the field reports). It runs an establishing scan on a registered queue and asserts
  the EXACT total across both contributing arms of `aggregateScopeCap` —
  30 GiB scan-visible leaf-drained + 10 GiB connection-held-but-unscanned = 40 GiB —
  so a snapshot that copied only one arm, or a stale zero, fails too.
- New `TestConfineListSliceReserveSummary/aggregate-established` drives the same
  property to the WIRE struct through the real `confine-list` handler, after a real
  `evaluateAdmitQueue` pass, asserting `CapAggregateKnown: true` AND
  `CapAggregateBytes: 30 GiB` under the arm's full `reflect.DeepEqual`. It carries its
  own fixture because a real evaluator pass changes adoption and charge state the
  shared expectations do not describe. Its scan record is LEAF-DRAINED, which keeps
  every other wire field at its idle value so the aggregate is the single thing the
  arm changes — and incidentally exercises the subtree-live reading at the operator
  surface.
- Both non-blocking review observations (queue-dropped `unevaluated` wording; the
  AIRA-59 freeze over sub-reservations behind an aggregate-blocked head) are written
  into **Operator surface** above.

### Round-2 mutation battery — 5 breakages, 5 caught, 0 survivors

Every mutant applied to production code, then reverted; the two the reviewer PROVED
survive were re-run against the **full** `internal/daemon` package, not a `-run`
subset.

| # | Mutation | Scope run | Result |
|---|----------|-----------|--------|
| 1 | delete the `capAggregate/capAggregateKnown` copy in `admit.go` | full package | **caught** (exit 1) — `TestSnapshotCarriesTheEstablishedAggregate` + `…/aggregate-established` |
| 2 | `CapAggregateKnown: false` hardcoded in `confine_manage.go` | full package | **caught** (exit 1) — `…/aggregate-established` |
| 3 | `CapAggregateBytes: 0` hardcoded on the wire | targeted | **caught** (exit 1) |
| 4 | `capAggregate: 0` in the snapshot, bit left correct | targeted | **caught** (exit 1) by both new tests |
| 5 | `capAggregateKnown: true` hardcoded in the snapshot | targeted | **caught** (exit 1) — proves the unestablished arm is no longer vacuous |

Plus a test-of-the-test: removing the `registerAdmitQueue` call makes
`TestAggregateUnestablishedWithholdsNothing` fail on its own `present` guard, so the
registration is demonstrably load-bearing rather than decorative.

### Round-2 exit codes

- `aira confine -- go build ./...` → 0
- `aira confine -- go vet ./...` → 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` → 0
