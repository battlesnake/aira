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

1. `pylib.AppendConfineChildEnvironment(env, scopeID, slice)` publishes `AIRA_CONFINE_SLICE`
   beside `AIRA_CONFINE_SCOPE_ID`, and the new key joins the strip set for the same reason
   the scope id is in it: a nested launch must publish ITS slice, not carry its
   grandparent's through.
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
(`TestConfineChildReceivesTheResolvedSlice`: `AIRA_CONFINE_SLICE=""`;
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
| `TestConfineChildReceivesTheResolvedSlice` | behaviour half, through a real `confineWithDeps` launch: the child gets `/fake/finite.slice` (the resolved PATH, from `Slice: "finite.slice"`), upserted over a stale inherited value |
| `TestAppendConfineChildEnvironmentPublishesTheResolvedSlice` | unit half: upsert over stale, no empty `AIRA_CONFINE_SLICE=` published, key is in the strip set |

The existing strip-set guards in `internal/core` and `internal/runner` gained the new key so
it cannot silently leave the strip set.

Note for a future reader: the reserve tests emptied `AIRA_CONFINE_SCOPE_ID`/`AIRA_CONFINE_SLICE`
explicitly rather than assuming absence, because the suite itself runs under `aira confine`,
which now exports both.

### Gates

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0

### Not changed

`worker-admit` (aitest's nested sub-scopes) resolves its own placement from the handed-down
outer scope and is out of scope here.
