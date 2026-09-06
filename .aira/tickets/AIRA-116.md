---
{"schema":1,"id":"AIRA-116","project":"aira","title":"No test proves Serve applies any of the daemon's parsed env settings","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["config","daemon","test-coverage"],"hold":false,"relations":[]}
---
Found during the AIRA-29 adversarial build review (Sol), then ground-checked. PRE-EXISTING and
shared by every env setting the daemon parses, not specific to AIRA-29.

`Serve` reads several env-derived settings — `admitBackfillGraceFromEnv`,
`admitFreezeMaxHoldFromEnv`, `watchPollIntervalFromEnv`, and AIRA-29's
`dynamicReserveFromEnv` — and assigns each to the Server. Every one of them is tested AT THE
PARSER (e.g. `admit_freeze_test.go` calls `admitFreezeMaxHoldFromEnv` directly). NOTHING tests
that `Serve` actually applies the parsed value to the field the subsystem reads.

So a dropped or mistyped assignment in `Serve` — the parsed value discarded, or written to the
wrong field — would leave every existing test green while the setting silently did nothing.
That matters most for AIRA-29's `AIRA_DAEMON_DYNAMIC_RESERVE`, which is a KILL SWITCH: the one
occasion it is used is an operator reverting an admission change on a shared machine under
load, and it failing silently is the worst available outcome.

Not fixed inside AIRA-29 because a bespoke test for one of the four would be inconsistent with
the file's own convention and would need a socket, a DB and a live daemon for a one-line
assignment. The honest fix is ONE test that starts a Server with each variable set and asserts
all four fields, covering the whole class at once.

## Resolution (in-review)

Branch `aira116-serve-env-settings-test`, off `cd1dabf`. Test-only: no production
file is touched.

New file `internal/daemon/serve_env_settings_test.go`, two tests.

### 1. `TestServeAppliesParsedEnvSettings` — the regression test

One `Server`, started through the existing `startServer` harness (real socket,
real DB, real `Serve`), with **every** env-derived daemon setting set to a
distinctive non-default value; each assertion then reads the Server field the
subsystem actually reads.

Eight rows, a superset of the four the ticket names:

| field | variable | set to | default |
| --- | --- | --- | --- |
| `watchPollInterval` | `AIRA_DAEMON_WATCH_POLL_INTERVAL` | `1300ms` | 500ms |
| `admitPollInterval` | `AIRA_DAEMON_ADMIT_POLL_INTERVAL` | `1700ms` | 250ms |
| `admitBackfillGrace` | `AIRA_DAEMON_ADMIT_BACKFILL_GRACE` | `37s` | 1m |
| `admitFreezeMaxHold` | `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD` | `91s` | 2m |
| `dynamicReserve` | `AIRA_DAEMON_DYNAMIC_RESERVE` | `disabled` | true |
| `oversubscriptionFactorPct` | `AIRA_DAEMON_OVERSUBSCRIPTION_FACTOR` | `3.5` → 350 | 200 |
| `cpuSlotsGrace` | `AIRA_AITEST_PLACEMENT_ACK_TIMEOUT` | `7.5` → 7.5s | 60s |
| `cpuSlotsCapacity` | `AIRA_DAEMON_CPU_RESERVE` | `0` → NumCPU | NumCPU−1 |

Two deliberate design points, both about not being porous:

- **Every variable is set AFTER `NewServer` returns.** `NewServer` itself reads
  `AIRA_DAEMON_CPU_RESERVE` and `AIRA_AITEST_PLACEMENT_ACK_TIMEOUT`, so setting
  them before construction would have let the constructor satisfy those two
  assertions with `Serve`'s assignment deleted. Setting them after construction
  means the only path to the asserted value is `Serve` parsing and assigning it.
- **All eight values are mutually distinct**, including the five durations, so a
  *cross-wired* assignment (a parsed value written to a neighbouring field) fails
  an assertion rather than coincidentally agreeing.

The `cpuSlotsCapacity` row cannot falsify on a single-CPU host (reserve 0 and the
default reserve 1 both clamp to capacity 1). That row reports `unevaluated` with
its reason rather than passing silently; the other seven are unaffected.

### 2. `TestServeEnvSettingCoverageIsComplete` — the class stays closed

The ticket's complaint is a **class** ("shared by every env setting the daemon
parses"), so a fixed table of eight would rot the moment a ninth setting lands.
This guard parses `server.go` with `go/ast`, walks `Serve`'s body, and collects
every `s.<field> = …` whose value is env-derived — directly
(`s.cpuSlotsGrace = cpuSlotsPlacementGrace()`) or through a local
(`v, err := xFromEnv(); … s.f = v`). It then fails in **both** directions:

- a setting `Serve` applies that the table does not assert — i.e. the *next*
  silently-inert setting, caught at the moment it is added; and
- a table row naming a field `Serve` no longer assigns — a stale assertion that
  could no longer catch anything.

It is fail-closed: if `(*Server).Serve` cannot be located in `server.go` it
fails rather than vacuously passing.

Accepted, documented gap: an env reader named by neither the `*FromEnv`
convention nor the two grandfathered names (`desiredCPUSlots`,
`cpuSlotsPlacementGrace`) would be invisible to the guard. That is recorded in
the source comment, not left implicit.

### Mutation evidence

The tests were run against 11 mutations of `server.go`; **all 11 were killed by
an assertion** (each mutation applied on its own, then reverted):

- drop each of the eight `s.<field> = <parsed>` assignments (8 mutations) —
  killed by the matching row, e.g. `s.dynamicReserve = true, want false`;
- `s.admitPollInterval = watchPollInterval` (cross-wire) — killed
  (`s.admitPollInterval = 1.3s, want 1.7s`);
- `s.admitFreezeMaxHold = admitBackfillGrace` (cross-wire) — killed
  (`s.admitFreezeMaxHold = 37s, want 1m31s`);
- add a ninth env setting to `Serve` without a table row — killed by the
  coverage guard (`does not assert: [storeOpAppendTimeout]`).

Each mutation is a one-line edit to `server.go`, reproducible by hand from the
list above.

### Gate

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0
  (all 15 packages `ok`)

## Done

Merged via PR #66 as merge commit `f775e73`. Fable build-review independently re-ran the gate
(build/vet/test all exit 0) and 9 one-line `server.go` mutations (8 drops/cross-wires + an
uncovered ninth setting + a stale row), all killed.
