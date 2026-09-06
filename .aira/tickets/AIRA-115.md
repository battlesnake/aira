---
{"schema":1,"id":"AIRA-115","project":"aira","title":"confine-reserve defaults its slice instead of inheriting the parent job's, mis-attributing sub-reservations","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["accounting","admission","confine","confine-reserve"],"hold":false,"relations":[]}
---
Found during the AIRA-29 adversarial build review (Sol), then ground-checked in the source.
PRE-EXISTING; AIRA-29 neither introduced nor worsened it, but it is now written down.

`confineReserve` (`internal/runner/confine_reserve_linux.go`) inherits the parent job's SCOPE
ID from the environment (`InheritedConfineScopeID()`) but defaults its SLICE independently to
`DefaultConfineSlice` when the caller passes none. So a parent job confined to a non-default
slice can have its per-test `aira confine-reserve` sub-reservations register against
`aira.slice` instead — charging a slice whose cgroup does not hold that memory, while the
slice that does hold it never sees the reservation.

Consequences:

- The reserving slice is over-charged for memory it is not hosting, so healthy jobs there can
  wait behind a phantom reservation.
- The hosting slice under-counts, though its own physical `memory.current` term still charges
  the real usage.

Why AIRA-29 does not make it worse: AIRA-29 excludes a parent scope from dynamic charging
when it has live sub-reservations IN THE SAME QUEUE, which is exactly the queue where the
double-book would occur. In the split-slice case no single queue double-books — the queue
with no child charge is precisely the one where charging the parent its live usage is
correct. Before AIRA-29 the same split existed with the parent's frozen reserve.

Likely fix: have `confine-reserve` inherit the parent's resolved slice from the environment
alongside its scope id, and refuse rather than silently default when a parent scope id is
present but its slice cannot be established (the AIRA-58 rule).

## Resolution (in-review)

Built as diagnosed. Branch `aira115-confine-reserve-slice-inheritance`, off `bd807e6`.

### The fix

The scope id and the slice are two halves of one fact — WHICH job a sub-reservation belongs
to, and WHERE that job lives — and only the first half was ever published to the confined
child. So they now travel together.

1. `pylib.AppendConfineChildEnvironment(env, scopeID, slice)` publishes
   `AIRA_CONFINE_PARENT_SLICE` (`pylib.ConfineParentSliceEnv`) beside
   `AIRA_CONFINE_SCOPE_ID`, and the new key joins the strip set for the same reason the
   scope id is in it: a nested launch must publish ITS slice, not carry its grandparent's
   through. The name is a **new** variable, not `AIRA_CONFINE_SLICE` — see "Build-review
   fixes" below; that one is the operator's input and is neither written nor stripped.
2. The confine launch site passes `path` — the **resolved slice cgroup path**, not the name
   the caller typed. That is deliberate and load-bearing: it is exactly the value
   `admitConfine` uses as its `memorySlice`, so the sub-reservation lands in its parent's own
   daemon admission queue. A bare slice NAME would instead be re-resolved by the daemon from
   the *daemon's* cgroup ancestry, which for a slice that is not on the daemon's own path
   resolves elsewhere or not at all. (For the default `aira.slice` both resolve to the same
   canonical path today, so this is exactness where the old behaviour was coincidence.)
3. `runner.InheritedConfineSlice()` reads it back, discarding a value with a `..` component
   (both slice resolvers refuse one; forwarding it would only produce an unevaluated
   admission).
4. `resolveConfineReserveSlice` replaces the bare `if Slice == "" { Slice =
   DefaultConfineSlice }`, with this precedence:
   - an **explicit** `--slice` still wins (AIRA-58 forbids silently substituting for a
     caller's declared value; inheritance fills a gap, it does not overrule a statement);
   - otherwise the inherited parent slice;
   - otherwise `DefaultConfineSlice`, which is what that default was always actually for —
     the unconfined caller.
5. The AIRA-58 refusal for the remaining gap: a parent scope id present with **no usable
   slice** means the environment asserts the reservation belongs to a running job while
   withholding where that job lives. That is precisely the silent mis-attribution being
   removed, so it is refused **before any dial**, as `E_CONFINE_UNAVAILABLE` — callers treat
   that as fail-open, so the test then runs unreserved, which under-counts one slice rather
   than over-charging an unrelated one.

### Tests

The three that pin the bug were confirmed RED by reverting the fix in place and re-running
(`TestConfineChildReceivesTheResolvedSlice`: the parent-slice coordinate was `""`;
`TestConfineReserveChargesTheInheritedParentSlice`: `admit slice=aira.slice`;
`TestConfineReserveRefusesRatherThanDefaultUnderAParentScope`: granted instead of refusing).
The two false-fail guards pass in both directions BY DESIGN — that is what they are for —
and are recorded as such rather than claimed as regression coverage.

| Test | Direction |
| --- | --- |
| `TestConfineReserveChargesTheInheritedParentSlice` | the bug: wire `slice` was `aira.slice`, now the parent's. Also asserts `parent_scope_id` still rides along — a fix that inherited the slice but dropped the AIRA-101 sub-reservation marker would break drain convergence, and a slice-only assertion could not see it (it is also the proof-of-life for the frame) |
| `TestConfineReserveRefusesRatherThanDefaultUnderAParentScope` | the AIRA-58 refusal, in both the absent and `..`-unusable shapes, with a **zero-dial** assertion — a refusal issued after the daemon had already been asked would have charged the wrong slice anyway |
| `TestConfineReserveOutsideAConfineJobKeepsTheDefaultSlice` | false-fail guard: "inherit or refuse" must not be over-applied into refusing every standalone `aira confine-reserve` |
| `TestConfineReserveExplicitSliceOverridesTheInheritedOne` | false-fail guard on the precedence |
| `TestConfineChildReceivesTheResolvedSlice` | behaviour half, through a real `confineWithDeps` launch: the child gets `/fake/finite.slice` (the resolved PATH, from `Slice: "finite.slice"`), upserted over a stale inherited value, AND the operator's `AIRA_CONFINE_SLICE` arrives unchanged |
| `TestInheritedParentSliceIsNotAnExplicitSliceInput` | the build-review F1 regression: with only the emitted coordinate set, `ResolveConfineSlice("")` is still `""` and a nested `confineWithDeps` with no `--slice` still runs DEFAULT resolution and names `aira.slice` |
| `TestAppendConfineChildEnvironmentPublishesTheResolvedSlice` | unit half: upsert over stale, no empty coordinate published, key is in the strip set, and `AIRA_CONFINE_SLICE` is neither rewritten nor in the strip set |

The existing strip-set guards in `internal/core` and `internal/runner` gained the new key so
it cannot silently leave the strip set.

Note for a future reader: the reserve tests emptied
`AIRA_CONFINE_SCOPE_ID`/`AIRA_CONFINE_PARENT_SLICE` explicitly rather than assuming absence,
because the suite itself runs under `aira confine`, which now exports both.

## Build-review fixes (round 2)

The first cut published the resolved slice under **`AIRA_CONFINE_SLICE`**, which was a real
BLOCK: that variable is already the OPERATOR's explicit-slice INPUT, read by
`ResolveConfineSlice` (`internal/runner/confine.go:491-499`) under the install-owned-slice
design §4 precedence `--slice` > `$AIRA_CONFINE_SLICE` > `aira.slice`, where an explicit
value **never** falls back. So `confineWithDeps`'s `explicitSlice := ResolveConfineSlice(...)`
treated the parent's published absolute cgroup path as an operator-declared `--slice` for
every NESTED `aira confine` — the norm on this box. `resolveDefaultConfineSlice`'s
managed-unit guard and whale fallback were skipped, the status line and the detach record
named the raw path, and the same value reached `ResolveConfineManagementSlice("")` in the
daemon reaper/manage paths, the client fallback, and any daemon auto-spawned from there.
It also turned the mandated gate red once installed: under the PR binary as confine parent,
`internal/runner`'s `TestDefaultConfinePresentButUncappedFailsOnAIRA` failed because the
inherited path became the explicit slice. The PR's own green gate had run under the
pre-PR installed binary, which cannot publish the coordinate, so it could not see this.

- **F1 (BLOCK) — fixed by renaming the published coordinate.** The emitted value now goes to
  `pylib.ConfineParentSliceEnv` = `AIRA_CONFINE_PARENT_SLICE`; `runner.InheritedConfineSlice`
  reads that; the strip set carries that. `AIRA_CONFINE_SLICE`, `ResolveConfineSlice`, and
  spec §4 are untouched, and `AIRA_CONFINE_SLICE` is deliberately NOT in the strip set: it is
  the operator's own setting, not a coordinate AIRA emits, so a descendant is entitled to see
  it. The rule is stated at the constant: an emitted OUTPUT coordinate and an operator INPUT
  must never share a variable name.
- **F2 — fixed.** `internal/daemon/oomsteer.go`'s `admitScopeBudgets` comment said
  `confine-reserve` "defaults its slice independently of its parent", which this change
  falsifies. It now records that a sub-reservation taken inside a confine job lands in its
  parent's queue, and names the narrower residue that remains (an explicit `--slice` to a
  different slice, a parent whose waiter has already left the queue, a non-confine caller).
- **F3 — recorded.** Upgrade transition: an IN-FLIGHT job launched by the pre-fix binary
  exports a scope id and no parent-slice coordinate. If its per-test helper resolves to the
  new binary, every `confine-reserve` is REFUSED by the AIRA-58 rule — fail-open, so those
  tests run unreserved until the job ends. Acceptable under the no-compat rule, but it is not
  true that nothing needs redeploying beyond the client binary: jobs already running through
  the old binary lose per-test reservations for the rest of their life.
- **F4 — decided, not dropped.** With the rename, a standalone (unconfined) `confine-reserve`
  no longer honours an operator-set `AIRA_CONFINE_SLICE`; it charges `aira.slice`, exactly as
  before this ticket. That is deliberate: this fix teaches the reserve to inherit what its
  PARENT JOB emitted, not to start reading the operator's launch setting, which is a separate
  behaviour change to a separate input. It is pinned by
  `TestConfineReserveOutsideAConfineJobKeepsTheDefaultSlice`, which now sets
  `AIRA_CONFINE_SLICE` and asserts it is ignored, and stated in
  `resolveConfineReserveSlice`'s doc comment.

Mutation evidence for the F1 regression test: pointing `pylib.ConfineParentSliceEnv` back at
`"AIRA_CONFINE_SLICE"` turns `TestInheritedParentSliceIsNotAnExplicitSliceInput`,
`TestConfineChildReceivesTheResolvedSlice`,
`TestAppendConfineChildEnvironmentPublishesTheResolvedSlice` and
`TestCoordinationEnvironmentIsStrippedAndKeptOutOfTheDigest` RED. Each half of the new test
was checked in isolation, so neither masks the other: the launch half alone reports
`nested confine resolved slice="/sys/fs/cgroup/.../aira.slice", want "aira.slice"` — the
reviewer's exact post-deploy symptom.

### Gates

Run with the BRANCH binary as the confine parent, not the installed one, because that is the
only configuration in which the published coordinate exists at all:

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0
- `AIRA_REAL_CGROUP=1 <branch-binary> confine -- go test ./... -count=1` — exit 0

### Not changed

`worker-admit` (aitest's nested sub-scopes) resolves its own placement from the handed-down
outer scope and is out of scope here.
