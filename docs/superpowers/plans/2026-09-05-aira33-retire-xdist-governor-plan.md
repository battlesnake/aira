# AIRA-33 — retire the legacy xdist-governor stack

- **Status:** PLAN v2 — **Fable plan-gate: GATE-PASS-WITH-CONDITIONS**
  (`docs/superpowers/reviews/2026-09-05-fable-aira33-plan-gate.md`). No blocking
  findings; the wake-path proof was independently re-derived and **HOLDS**. Four
  conditions and five non-blocking findings, every one accepted and folded in —
  see §12, which amends the sections above and wins where it differs.
- **Ticket:** AIRA-33. Branch `aira33-retire-xdist-governor`, worktree
  `/home/mark/claude/worktree-aira33`, forked from `origin/master` at `6f1d35a`.
- **Spec:** `docs/superpowers/specs/2026-09-01-aitest-design.md` §3.8 (Deletion,
  retention, generalisation), §5 Slice 4, §6.
- **Owner decision carried into this plan:** delete now, regardless of
  fastest-ee's own migration timing, accepting a narrow accounting regression on
  the still-pinned fastest-ee legs (they fall back to flat 512 MB whole-job
  charging until fastest-ee migrates them onto aitest on its own schedule). This
  is explicitly accepted and is **not** a blocker.

This is a **deletion**, not a behaviour change. Its correctness risk is entirely
in what the deletion *silently takes with it*: a wake path, an env export, a
startup validation, a shared test helper. Every one of those is enumerated below
with the evidence that removing it is safe.

---

## 1. Scope: what the deleted stack actually is

Three cooperating layers, all and only for governing **pytest-xdist** workers:

| Layer | File(s) | What it does |
|---|---|---|
| Python plugin | `internal/pylib/aira_xdist_governor/` | pytest plugin: per-test CPU checkpoint (park/resume via a relay) + per-test RAM reservation (spawns `aira confine-reserve`) |
| Client relay | `internal/runner/governor_slot.go` (+ `_linux.go`, `_stub.go`), `aira governor-slot` verb | one long-lived Go process per xdist worker, relaying `acquire`/`checkpoint` line-protocol to the daemon |
| Daemon scheduler | `internal/daemon/governor.go` | `governorSet`: the park/active-set scheduler, fairness floor, observe/enforce modes, RAM-ordered activation |

aitest (AIRA-30…64, landed) replaces all three: it forks its own workers, admits
each through `worker-admit` into a nested cgroup sub-scope, and bounds CPU
concurrency through `cpuslots_gate.go`. Nothing in aitest touches any of the
three layers above.

**Pre-deletion grep-sweep for a non-pytest caller of the CPU park/active-set
machinery** (spec §6 flags this as unconfirmed, not assumed) — **RUN, result:
none.** The daemon-side entry point is the `governor` verb
(`server.go:688` → `governorConnection`). Its only in-tree client is
`runner.GovernorSlot` (`governor_slot.go:200`), whose only caller is the
`governor-slot` CLI verb (`main.go:198`), whose only caller is
`aira_xdist_governor/__init__.py:172`. `governorSet`'s only other entry points
are `signal()` (two call sites, both analysed in §3) and the `AIRA_SCHED_MODE`
startup wiring. No other package imports it; `governorSet` and every helper are
unexported and confined to `internal/daemon`.

---

## 2. DELETE (whole files)

```
internal/daemon/governor.go                          (755)
internal/daemon/governor_test.go                     (626)
internal/runner/governor_slot.go                     (394)
internal/runner/governor_slot_linux.go               ( 15)   [1]
internal/runner/governor_slot_stub.go                (  6)   [1]
internal/runner/governor_slot_test.go                (466)
internal/pylib/aira_xdist_governor/__init__.py       (~420)
internal/pylib/aira_xdist_governor/shim.py
internal/pylib/aira_xdist_governor/README.md
internal/pylib/aira_xdist_governor/.gitignore
internal/pylib/pytest_integration_test.go            (959)   [2]
internal/pylib/reservation_sizing_test.go            ( 78)
docs/superpowers/specs/2026-08-30-scheduler-slice2-daemon-active-set-plan.md
docs/superpowers/specs/2026-08-30-scheduler-slice3-ram-aware-admission-plan.md
```

**[1] Re-verification finding, not in the brief's list.** `governor_slot_linux.go`
and `governor_slot_stub.go` exist solely to define
`setGovernorParentDeathSignal()`, whose only caller is `governor_slot.go:326`.
They must go with it or the package fails to compile with two unused build-tagged
functions defining the same symbol for no consumer.

**[2] `pytest_integration_test.go` is governor-only, verified test by test.** All
21 `TestRealPytest*` functions exercise the plugin (relay, checkpoint, per-test
reservation sizing, fail-open, marker precedence). This includes AIRA-65's
`TestRealPytestRAMForkDoesNotPinHelperStdin:633` (already closed separately as
close-not-needed; see §7). **But it also defines a helper three surviving files
use** — see §5.1.

### 2.1 Spec deviation: keep the Slice 1 spec — DECISION FOR FABLE

Spec §3.8 and the brief both say "the three `2026-08-30` scheduler-slice specs".
Re-verification says two, not three:

- `scheduler-slice2-daemon-active-set-plan.md` — describes `governor.go`'s
  park/active-set scheduler and the `governor-slot` relay. **Entirely deleted
  code. Delete the spec.**
- `scheduler-slice3-ram-aware-admission-plan.md` — describes the RAM-ordered
  activation and `admitAvailable`. **Entirely deleted code. Delete the spec.**
- `scheduler-slice1-cpuweight-aging-plan.md` — describes cgroup **`cpu.weight`
  aging in the confine supervisor**. That code is **NOT deleted and NOT part of
  this stack**: `ConfineCPUWeightAging`, the `cpu-weight=aging` trailer facet and
  the `+cpu` subtree-control delegation all live in
  `internal/runner/confine_linux.go` and stay. Deleting this spec would remove
  the only design record for live, retained code.

**Proposal: delete slice2 + slice3, KEEP slice1**, and add a two-line status
banner to slice1 and to the umbrella
`2026-08-30-aira-cooperative-scheduler-design.md` recording that slices 2–3 were
retired by AIRA-33 while slice 1 remains live. Fable to confirm or overrule.

---

## 3. UNWIRE (surgical edits) — with the safety evidence for each

### 3.1 `internal/daemon/sliceceiling.go` — the wake-path question, ANSWERED NOT ASSUMED

Two `deps.signalGovernor()` call sites go away, plus the `signalGovernor` field
and its construction in `realSliceCeilingDeps`:

- `:421` — after a **published raise** (`!hadPub || published > previous`).
- `:447` — after a **TTL expiry that was throttled** (an effective raise, since
  expiry hands admission back the raw `memory.max`).

The question the brief demands be *proved*: **once the governor signal is gone,
does the ceiling raise / unthrottle path still wake everything that needs
waking?**

**Answer: nothing else needs waking. Proof, per consumer of the ceiling:**

1. **The slice admission queue** (`sliceQueue`). `admitEffectiveMaximum` is
   consumed by `evaluateAdmitQueue` (`sliceceiling.go:553`). `evaluateAdmitQueue`
   is driven by `sliceQueue.runEvaluator` (`admit.go:1360-1372`), which is
   `select` over `q.kick`, **`ticker.C`**, and `q.stop`, where
   `ticker := time.NewTicker(q.poll)` and `q.poll` defaults to
   `defaultAdmitPollInterval = 250ms` (`paths.go:43`). The `ticker.C` case calls
   `q.signal()`, i.e. the queue re-evaluates itself at ≥4 Hz whether or not
   anyone kicks it. A ceiling raise is therefore observed within one poll
   interval with **no** external signal. This is not merely the existing
   comment's claim — it is the code. It is also why `sliceceiling.go` never
   called `queue.signal()` in the first place.
2. **`worker-admit`** (aitest's per-worker gate). Its waiter loop is an explicit
   `time.After(sleep)` poll at `workerAdmitPollInterval`, defaulting to **200 ms**
   (`worker_admit.go:919-921, 985-990`). It has no kick channel at all. Also,
   `admitEffectiveMaximum` is deliberately **not applied** to
   `evaluateWorkerAdmit` (`sliceceiling.go:562-565`: keyed by outer scope, not by
   slice, so throttling it would double-charge), so the ceiling does not gate it
   in the first place.
3. **The CPU-concurrency gate** (`cpuslots_gate.go`, AIRA-64). Consulted only
   *inside* `evaluateWorkerAdmit`, i.e. on the 200 ms poll above, and its live
   count is derived from the **cgroup tree** (`scanSliceWorkerScopes`), not from
   connections or from any signal. Confirms the brief's expectation; now
   evidenced rather than assumed.
4. **`confineManagement`** (`--list` display) — a request-driven read, no waiters.

**Conclusion: this resolves to "nothing else needs waking", and the resolution is
demonstrated, not assumed.** The deletion removes the *only* signal-driven
consumer of a ceiling raise. Every remaining consumer is poll-driven at
200–250 ms.

**Test consequence:** `sliceceiling_test.go`'s fake `governorWakes` counter (`:34`,
`:65`) and the two tests that assert on it —
`TestSliceCeilingRaiseSignalsGovernor:426` and
`TestSliceCeilingExpiryWakesTheGovernor:848` — are deleted with the field. Their
*other* assertions (that a sub-quantum move publishes nothing; that expiry clears
the window and republishes `unevaluated`) are already covered by
`TestSliceCeilingSubQuantumMovementIsIgnored:406` and the surrounding expiry
tests; I will confirm by inspection before deleting and, if either test carries a
unique non-governor assertion, keep that assertion in the surviving neighbour
rather than dropping coverage silently.

### 3.2 `internal/daemon/admit.go`

- `:1813-1815` — remove the `s.governor.signal()` from `afterAdmitRelease`.
  **Safe:** the line immediately above it is already `queue.signal()`, so the
  release path still kicks the admission queue; only the governor kick goes.
- `:885-889` — the lock-order comment on `admitAvailable`. See §3.3.

### 3.3 `admitAvailable` becomes dead — DECISION FOR FABLE

`Server.admitAvailable` (`admit.go:890`) has exactly **one** production caller:
`governor.go:388`. After the deletion it is unreachable production code with only
test callers (`admit_test.go:918-928`).

**Proposal: delete `admitAvailable` and its tests**, and reduce
`admitEffectiveMaximum`'s doc comment (`sliceceiling.go:549-556`) from three
application sites to two (`evaluateAdmitQueue`, `confineManagement`). It is a
pure read-only helper; removing it cannot change runtime behaviour. Keeping it
would leave a documented "the governor's read-only advisory" for a governor that
no longer exists — exactly the kind of stale claim this project treats as a
correctness defect in documentation.

Fable to confirm. If Fable prefers a smaller diff, the fallback is to keep it and
re-document it honestly as currently-unused; I do not recommend that.

### 3.4 `internal/daemon/server.go`

- `:94` — remove the `governor *governorSet` field.
- `:199` — remove `server.governor = newGovernorSet(capacity, governorObserve, server)`.
  `capacity` is still needed: it feeds `cpuSlotsCapacity` in the same struct
  literal (`:190`). `desiredCPUSlots` is **KEPT** (see §4).
- `:296-304` — remove `governorModeFromEnv(os.Getenv("AIRA_SCHED_MODE"))` and the
  governor capacity/mode update. **Behaviour change, called out explicitly:** a
  malformed `AIRA_SCHED_MODE` currently makes `Serve` return an error and the
  daemon refuse to start. After this change the variable is simply ignored. That
  is correct — the setting controls nothing once the scheduler is gone — but it
  is a real removal of a startup validation and must be stated, not slipped in.
  The AIRA-64 `cpuSlotsCapacity` assignment at `:305-313` is **kept unchanged**.
- `:319` — reword the `slotErr != nil` log line. It currently says
  `"aira scheduler governor: using safe capacity-1 fallback"`; after the deletion
  the fallback protects the **worker-admit CPU gate**, so the message must name
  that instead. Leaving it would be a false statement in the daemon journal.
- `:461-463` — remove the `governor.stopOnce`/`close(governor.stop)` from the
  drain path.
- `:688-696` — remove the `verb == "governor"` dispatch branch.
- `:642-648`, `deadlines.go:40` — the two comments enumerating long-lived framed
  handlers ("store-op, admit, governor, worker-admit, watch") drop `governor`.
  These comments carry a live **INVARIANT** for future handlers, so they are
  edited, never deleted.

### 3.5 `cmd/aira/main.go`

Remove the `governor-slot` verb end to end: `:193-198` (dispatch), `:599-600`
(arg parsing dispatch), `:863-880` (`parseGovernorSlotArgs`), `:1496-1516`
(`runGovernorSlotCommand`). Fix `:1078`'s stderr text, which currently names the
"CPU governor" as the thing that gets disabled without daemon paths.

`AIRA_GOVERNOR_SLICE` is read only by `runGovernorSlotCommand` and goes with it.

### 3.6 `cmd/aira/scope_dir.go`

Remove `"governor-slot"` from `verbAcceptsScopeDir`'s project-less list (`:155`),
and from `scope_dir_test.go:427`'s table. `confine-reserve`, `aitest-bootstrap`
and `worker-admit` stay.

### 3.7 `internal/core/core.go`, `internal/core/routing.go` — **NO CHANGE**

Re-verification finding: `grep -n governor internal/core/core.go
internal/core/routing.go` returns **nothing**. `governor-slot` is one of the
"eleven out-of-table verbs" and was never in the verb table or the routing
switch. The brief listed these files conditionally on `confine-reserve` also
being deleted; per §3.9 it is not, so both files are untouched.

### 3.8 `internal/pylib/env.go` — the env keys and `AIRA_PY_LIB`

`governorEnvironmentKeys` currently holds ten keys. Disposition of each:

| Key | Sole consumer | Action |
|---|---|---|
| `AIRA_PY_LIB` | the plugin (extraction path) | **remove** |
| `AIRA_GOVERNOR` | plugin CPU checkpoint | **remove** |
| `AIRA_GOVERNOR_CMD` | plugin (spawns the relay) | **remove** |
| `AIRA_GOVERNOR_MAX_WAIT` | `governor_slot.go` | **remove** |
| `AIRA_GOVERNOR_SLICE` | `runGovernorSlotCommand` | **remove** |
| `AIRA_TEST_MEM_GOVERNOR` | plugin RAM half | **remove** |
| `AIRA_TEST_MEM_DEFAULT` | plugin RAM half | **remove** |
| `AIRA_TEST_MEM_GROWTH_HEADROOM` | plugin RAM half | **remove** |
| `AIRA_CONFINE_RESERVE_CMD` | plugin (spawns `confine-reserve`) | **remove** |
| `AIRA_CONFINE_SCOPE_ID` | **`runner.InheritedConfineScopeID()`** | **KEEP** |

**`AIRA_CONFINE_SCOPE_ID` is load-bearing beyond the plugin and must survive.**
`InheritedConfineScopeID()` (`confine_linux.go:1390`) reads it, and
`confine_reserve_linux.go:53` uses it to set `ParentScopeID`, which is what lets
a `confine-reserve` sub-reservation declare itself part of an already-running job
rather than new work entering the slice (`admit.go:224`,
`confine_reaper.go:105/135`). Dropping it would silently turn every sub-reservation
into a fresh slice charge.

So `appendChildEnvironment` keeps its strip-then-export shape but exports only
`AIRA_CONFINE_SCOPE_ID`. `StripGovernorEnvironment` / `runner.StripGovernorEnv`
(used for gate-command environment identity at `gate_command.go:169` and
`runner_linux.go:287`) keep exactly that one key. `AppendChildEnvironment`'s
`runtimeDir`/`diagnostics` parameters become dead once `ExtractPyLib` is gone —
`AppendChildEnvironment` reduces to `StripGovernorEnvironment` at its two callers
(`runner_linux.go:307`, `command.go:171`), and `AppendConfineChildEnvironment`
loses `delegateRAM`, `reserveCommand` and `memoryDefault`. I will simplify the
signatures accordingly rather than leave ignored parameters, and delete
`extractForChild`/`childEnvFailureOnce` and `DefaultTestMemoryReserve`.

**Consequence at `confine_linux.go:967-977`:** the `memoryDefault` block (which
reads `AIRA_TEST_MEM_DEFAULT` and falls back to `pylib.DefaultTestMemoryReserve`)
goes with it. Note this is **not** the delegate-ram admission reserve — that is
`runner.DefaultDelegateRAMOverhead` (`confine.go:25`, 512 MiB), a separate
constant used by `ResolveConfineReserve`, and it is **untouched**.

**A behaviour improvement, stated so it is not mistaken for an accident:**
`AIRA_CONFINE_SCOPE_ID` is currently exported *only if* `ExtractPyLib()`
succeeds (`env.go:150-164` returns early on extraction failure, before the scope
id is written). Removing the extraction removes that unrelated gate, so the scope
id is now exported whenever there is one. That is strictly more correct.

**`AIRA_PY_LIB` and fastest-ee — verified, intended, safe.** fastest-ee's
conftests guard on it, e.g. `backend/conftest.py:13-25`:

```python
try:
    aira_py_lib = os.environ.get("AIRA_PY_LIB")
    if not aira_py_lib or config.pluginmanager.has_plugin("aira_xdist_governor"):
        return
    ...
except Exception:
    pass
```

With `AIRA_PY_LIB` unset, `not aira_py_lib` is true and `pytest_configure`
returns immediately — no import, no exception, suite unchanged. The whole guard
is additionally inside `try/except Exception: pass`. Eleven fastest-ee conftests
share this shape (AIRA-77's resolution enumerates them). So the short-circuit is
**intended** (it is the documented off-switch) and **safe** (fail-open by
construction, and the accounting regression is the owner-accepted one).

### 3.9 `confine-reserve` — **KEEP**, per the spec's own wording

The brief made this conditional: "*if* the confine-reserve verb widening is
adopted". Spec §3.8 settles it — it lists "the pytest **call sites** of
`aira confine-reserve`", not the verb. Those call sites die with the plugin.

Reasons to keep the verb and `internal/runner/confine_reserve*.go`:

1. The spec deletes call sites, not the verb.
2. The daemon-side machinery it drives is **retained and referenced by other
   landed work**: `parentScopeID` sub-reservations (`admit.go:224`), AIRA-68's
   scope-less-lease reaper carve-out (`confine_reaper.go:105/135`), and AIRA-101's
   exclusive-drain interaction with sub-reservations. Deleting the verb would
   orphan tested daemon paths without deleting them.
3. AIRA-102 (done) names `confine-reserve` as the mechanism a future Docker
   accounting shim would reuse.

Deleting it is a separable change with its own blast radius; folding it into a
deletion ticket would be scope creep. **Recorded as an explicit non-deletion, not
an oversight.** Fable to confirm.

### 3.10 `internal/pylib/extract.go`

Remove `embeddedRoot`, the `//go:embed aira_xdist_governor/*.py` directive,
`embeddedPyLib` and `ExtractPyLib`. `embeddedAitestRoot`, `embeddedAitest`,
`ExtractAitest` and the whole shared `extractPyLibFS` implementation stay.

### 3.11 `internal/core/skill.go` — the generated SKILL prose

Two paragraphs make claims that become **false** the moment the plugin is gone.
This is generated agent-facing guidance; a false claim here is a correctness
defect, not a doc nit.

- `:318` (the `confine` blurb) says `--delegate-ram` means "the governor then
  admits and RAM-caps each test as it runs (blocking and gc-collecting workers to
  stay under the cap)", recommends
  `aira confine --delegate-ram --memory-reserve <framework> -- pytest ...` as the
  per-test-governed shape, describes `aira_mem("4G")` markers as the way to
  declare per-test RAM, and describes the per-test gate "failing OPEN under
  contention". **All of that is the deleted plugin.** After this change
  `--delegate-ram` means exactly: a small pinned framework-overhead reserve
  instead of a whole-job reserve, a generous (not airtight) scope ceiling, the
  `@dr` scope-id class, and the aitest coordinates. The prose must be rewritten
  to say that and only that. The `NOT airtight` warning **stays** — it is still
  true, and for the sharper aitest reason (`:322` already states it) — but its
  stated mechanism changes from "the per-test gate fails open" to "a delegate
  scope's `memory.max` is a generous ceiling, not its reserve".
- `:322` (the aitest blurb) ends with a migration-hazard paragraph about
  `--delegate-ram` arming the legacy plugin and the `-p no:aira_xdist_governor`
  workaround. **Delete that paragraph** — with the plugin gone there is no hazard
  and no workaround. Its neighbouring accounting sentence "(an explicit
  `--memory-reserve` replaces that default, and a registered legacy governor adds
  its own per-test reservations on top)" loses its trailing clause.
- `internal/core/skill_test.go:521-526` asserts the presence of
  `-p no:aira_xdist_governor` (AIRA-77's guard). It must be **inverted**: assert
  the SKILL text no longer mentions `aira_xdist_governor` at all, so a future
  re-introduction of stale prose fails the build.

### 3.12 `README.md:97`

"The pytest plugin cooperates with the daemon scheduler between tests, so several
sessions can all run `-n auto` while the daemon controls the active worker set.
Each test's reservation is sized to what its worker is actually holding" — every
clause describes the deleted plugin. Rewrite to describe `--delegate-ram` +
aitest as they now are.

### 3.13 `.github/workflows/ci.yml:47-66`

The `-race` NOTE names AIRA-33 as the unblock and
`TestRealPytestRAMForkDoesNotPinHelperStdin` as the one remaining flake, "which
is a hardcoded `process.wait(timeout=1.0)` in the xdist governor — production
code inside AIRA-33's deletion scope". That test and that timeout are deleted
here.

**I will update the note, not re-add the `-race` job.** Re-adding `-race` is
**AIRA-20's** ticket, explicitly (`AIRA-20`: "`-race` CI re-add after wall-clock-tight
test hardening"), and AIRA-20 also records **four** distinct wall-clock-tight
flakes in `internal/runner` (including two in `governor_slot_test.go`, which this
deletion removes, and two — `TestM20LauncherDefersACKAndBoundsReadiness`,
`TestGovernorSlotReconnectDoesNotBlockFailOpenOutput` — of which only the second
is deleted here). Turning `-race` on in this PR would be claiming green for a
suite whose flake inventory I have not re-measured, which is exactly what the
note forbids. The note is rewritten to say: AIRA-33 has landed and removed the
named blocker; what remains is AIRA-20's own measurement.

### 3.14 `internal/daemon/cpuslots.go` — KEEP, doc fix only

`desiredCPUSlots` is shared with AIRA-64's `cpuslots_gate.go` and is called from
`server.go:165` and `:295`. **Do not delete.** Its doc comment
(`cpuslots.go:16`) says "the daemon governor's active-set capacity" and must be
reworded to name the worker-admit CPU gate. `cpuslots_gate.go:22`'s
"the same capacity concept the governor uses (desiredCPUSlots)" needs the same
treatment.

### 3.15 `internal/daemon/admit_governor_contention_test.go` — KEEP

Despite its name it does not touch `governorSet`: it is AIRA-59's fairness-freeze
regression (`TestGovernorPerTestReservationsAreNotStalledByALargeNeighbourHead`),
using small pinned reservations merely as a *scenario*. It compiles and passes
unchanged. **I will rename it** to
`admit_small_reservation_contention_test.go` with the test renamed to
`TestSmallPinnedReservationsAreNotStalledByALargeNeighbourHead`, and rewrite the
two comments that describe the scenario as "the pytest RAM governor's per-test
reservations", because after this PR that phrase names nothing. Renaming is
optional per the brief; I judge it warranted precisely because the stale name is
what would make a future reader delete a live AIRA-59 regression by mistake.

---

## 4. KEEP — explicit non-deletions (so a reviewer can check I did not overreach)

- `internal/daemon/cpuslots.go` (`desiredCPUSlots`) — shared with AIRA-64.
- `internal/daemon/cpuslots_gate.go` — AIRA-64, untouched but for one comment.
- `internal/daemon/admit_governor_contention_test.go` — AIRA-59, renamed only.
- `aira confine-reserve` + `internal/runner/confine_reserve*.go` — §3.9.
- `AIRA_CONFINE_SCOPE_ID` — §3.8.
- `runner.DefaultDelegateRAMOverhead` / `DefaultDelegateRAMScopeCeiling` and all
  `--delegate-ram` semantics other than the plugin env exports — §3.8.
- `internal/pylib/aitest/` and every aitest test file.
- `scheduler-slice1-cpuweight-aging-plan.md` — §2.1.
- The `sliceceiling.go` throttle itself (AIRA-103) — only the wake call goes.

---

## 5. Test-side work

### 5.1 `requireRealPytest` must be relocated BEFORE the deletion

`pytest_integration_test.go:814` defines `requireRealPytest`, which
**three other files use**: `pytest_aitest_e2e_test.go`,
`pytest_aitest_supervisor_test.go` (both survive) and `reservation_sizing_test.go`
(deleted). Deleting the file without moving this helper breaks the whole `pylib`
test package. It moves to a new `internal/pylib/realpytest_test.go`.

Every other helper in that file (`realPytestProject`, `runPytest`,
`realPytestEnv`, `writeGovernorHelper`, `writeReserveHelper`, `indexOf`,
`waitForRealPytestFile`, `assertRealPytestItemRuns`, `waitForRealPytestPath`,
`writeTestFile`, `activatingConftest`, `pytestResult`) is used only within it and
goes with it. Verified by grep, per-symbol.

### 5.2 `internal/pylib/extract_test.go`

- `notEmbedded`: drop the `"aira_xdist_governor"` entry.
- `TestEmbeddedTreesMatchTrackedSources`: drop the `{embeddedRoot, embeddedPyLib}`
  case; the `aitest` case stays and keeps the AIRA-66 contract intact.
- `TestExtractPyLibIsIdempotentAndReadyLast` calls `ExtractPyLib()`. **Retarget to
  `ExtractAitest()`** rather than delete: it is the only test of the real
  end-to-end extraction/idempotence contract, and that contract is retained.
- The other three (`ContentHashChangesWithTree`, `ConcurrentPublish...`,
  `FailureNeverPublishesTarget`) drive `extractPyLibFS` with `fstest.MapFS` and
  need no change.

### 5.3 `internal/pylib/env_test.go`

Heavily governor-oriented. Every assertion about a removed key goes; the
`AIRA_CONFINE_SCOPE_ID` export/strip assertions **stay**, and I will add one that
did not exist: that the scope id is exported even though there is no longer any
extraction step that could fail first (§3.8's behaviour improvement — a test that
would have failed before the change, so it is not vacuous).

### 5.4 `internal/daemon/admit_test.go`, `sliceceiling_test.go`

- Delete `TestReleaseAdmitWaiterOnlySignalsGovernor:933` (it asserts exactly the
  removed signal) and, with §3.3 adopted, the `admitAvailable` assertions at
  `:918-928`.
- Delete the `governorWakes` fake field and its two tests (§3.1), after checking
  neither carries a unique non-governor assertion.

### 5.5 Other test files touching removed symbols

`internal/runner/runner_test.go`, `internal/runner/confine_linux_test.go`,
`internal/runner/detach_linux_test.go`,
`internal/runner/admission_exclusive_linux_test.go`,
`internal/core/command_test.go`, `internal/store/gate_command_integration_test.go`,
`cmd/aira/scope_dir_test.go`, `internal/core/skill_test.go`. Each asserts on
governor env keys or the `governor-slot` verb. Edits are mechanical: drop
removed-key assertions, keep `AIRA_CONFINE_SCOPE_ID` ones.

**Discipline for every such edit: an assertion is either removed because the
behaviour it pinned is deleted, or kept. No assertion is *weakened* to keep a
test compiling.** Where a surviving test would be left asserting nothing, it is
deleted outright rather than left as a vacuous pass.

### 5.6 New assertions added by this change

Deletion PRs are the classic place for a silent regression, so three tests are
**added**, each of which fails against `master`:

1. `internal/core/skill_test.go` — the generated SKILL text contains no
   `aira_xdist_governor`, no `AIRA_TEST_MEM_`, no `AIRA_GOVERNOR`, and no
   `aira_mem(` marker guidance. (Guards §3.11 against stale prose creeping back.)
2. `internal/pylib/env_test.go` — a `--delegate-ram` confine child environment
   contains **none** of the nine removed keys and **does** contain
   `AIRA_CONFINE_SCOPE_ID` and the `AIRA_AITEST_*` coordinates. (Guards §3.8: the
   aitest coordinates and the scope id must survive the same edit that removes
   the governor ones.)
3. `internal/daemon/sliceceiling_test.go` — a published raise still updates the
   published ceiling and calls `deps.publish`, with no wake side-channel. (Guards
   §3.1: proves the raise path is intact after the signal is removed.)

---

## 6. Invariants this change must not break

1. **Nothing that waits is left unwoken.** §3.1's proof: every remaining consumer
   of the slice ceiling is poll-driven at 200–250 ms.
2. **`--delegate-ram` still charges a pinned framework overhead** and still gets a
   finite scope `memory.max`. `ResolveConfineReserve` and
   `resolveDelegateRAMScopeCeiling` are untouched.
3. **`confine-reserve` sub-reservations still attach to their parent job** —
   `AIRA_CONFINE_SCOPE_ID` survives (§3.8).
4. **aitest is untouched.** Worker admission, nested sub-scopes, the CPU gate and
   the outcome channel are not modified.
5. **The AIRA-66 embed-hermeticity contract still holds** for `aitest`.
6. **No generated agent-facing text makes a claim about code that no longer
   exists** (§3.11, §3.12), and a test enforces it (§5.6.1).
7. **AIRA-59's fairness-freeze regression survives** the deletion of its
   misleadingly-named file (§3.15).

## 7. Knock-ons — consistency check, no action

AIRA-65 and AIRA-77 were both independently closed on 2026-09-05. Their
resolution texts were read in full and this deletion **contradicts neither**:

- **AIRA-65** (close-not-needed) rests on "the code is slated for deletion by
  AIRA-33" and states a residual: "if AIRA-33 is ABANDONED rather than landed,
  this should be reopened". Landing AIRA-33 discharges the residual. Its
  resolution also asks for a "tiny doc follow-up on closure": `ci.yml:62-66`
  should stop citing AIRA-65 by number. §3.13 does exactly that.
- **AIRA-77** (close-superseded-by-AIRA-33) states the double-arming "remains
  live in source until AIRA-33 deletes the plugin, with the SKILL.md guard as the
  interim". This PR deletes the plugin and therefore retires the interim guard —
  consistent, and §3.11 removes the guard prose in the same change.

Neither ticket file is modified.

## 8. Deliberately out of scope

- Re-adding the `-race` CI job — AIRA-20's ticket (§3.13).
- Deleting `confine-reserve` (§3.9).
- Touching AIRA-29's dynamic-reserve work or the accounting gap the SKILL
  documents. The owner-accepted fastest-ee regression is *recorded*, not fixed.
- fastest-ee's own conftests. Removing the now-dead `AIRA_PY_LIB` blocks there is
  a fastest-ee ticket; they are already inert and fail-open (§3.8).

## 9. Execution order (TDD-shaped, each step compiling)

1. Relocate `requireRealPytest` (§5.1) — the package must never be uncompilable.
2. Add the three new assertions (§5.6) against `master` behaviour where they can
   be written to fail now; run them and **record the failure** before proceeding.
3. Unwire the daemon (`sliceceiling` → `admit` → `server`), with test edits, so
   `internal/daemon` compiles governor-free before `governor.go` is removed.
4. Delete `governor.go` + `governor_test.go`.
5. Unwire and delete the runner relay and the CLI verb.
6. Unwire `pylib` env/extract, then delete the plugin directory and its tests.
7. Prose: `skill.go` + `skill_test.go`, `README.md`, `ci.yml`, `cpuslots*.go`
   comments, spec banners; delete slice2/slice3.
8. `aira confine -- make ci` (build + vet + gofmt + full `go test ./...`), exact
   exit codes recorded. Then a real-cgroup run where the environment allows it.
9. Self-review, then adversarial build-review (different lens), then rebase on a
   re-fetched `origin/master`, re-verify the branch tip, PR, merge.

## 10. Expected yield

~4,400 lines of Go/Python deleted, one CLI verb, one daemon verb, one daemon
subsystem and nine environment variables retired; three false statements removed
from agent-facing generated guidance; AIRA-20's stated blocker cleared.
Behaviourally the only intended change is that `--delegate-ram` no longer
per-test-governs an xdist suite — which nothing in this repo does any more, and
which for fastest-ee is the owner-accepted flat-512 MB fallback.

## 11. Open questions for the plan gate

1. **§3.3** — delete the now-dead `admitAvailable`, or keep it re-documented?
   (I recommend delete.)
2. **§3.9** — confirm `confine-reserve` is KEPT, against the brief's conditional
   and in favour of spec §3.8's "call sites" wording.
3. **§2.1** — confirm keeping `scheduler-slice1-cpuweight-aging-plan.md`, against
   spec §3.8's literal "three specs", because it documents retained live code.
4. **§3.15** — is renaming `admit_governor_contention_test.go` warranted?
5. **§3.4** — is silently dropping `AIRA_SCHED_MODE`'s startup validation
   acceptable, or should the daemon warn once if the variable is set?

---

## 12. Fable plan-gate v1 → plan v2 amendments

Verdict: **GATE-PASS-WITH-CONDITIONS**, no blocking findings. Full review at
`docs/superpowers/reviews/2026-09-05-fable-aira33-plan-gate.md`. The gate
independently re-derived the §3.1 wake-path proof from source across eight
consumers (including two the plan had not enumerated: the `grantedCh` grant
delivery path and the daemon's only condition variable, `projectCond`) and
returned **HOLDS**. Every finding below was **re-verified against source by me
before acceptance**, not taken on the reviewer's word. All nine are accepted; the
sections above are amended as follows.

### C1 (condition) — §5.6.3 overclaimed "fails against master"

**Verified true.** `evaluateSliceCeiling` guards `deps.signalGovernor != nil`
(`sliceceiling.go:421`), so a fake `deps` with a nil signal already compiles and
publishes a raise on master. New test 3 is therefore **forward-only**: a
regression guard that the raise path still publishes after the wake call is gone,
not a red-then-green test.

**Amendment:** §5.6 new tests 1 and 2 are red-against-master and their failure is
recorded in §9 step 2. **New test 3 is explicitly excluded from that step** and
labelled a forward-only guard. Manufacturing a failure record for it would be
exactly the fabricated evidence this project forbids.

### C2 (condition) — §5.2's retarget would produce a duplicate

**Verified true.** `TestExtractAitestIsIdempotent` (`extract_test.go:161-186`) is
line-for-line identical to `TestExtractPyLibIsIdempotentAndReadyLast`
(`:188-213`) apart from `ExtractAitest()` vs `ExtractPyLib()`. §5.2's claim that
the latter is "the only test of the real end-to-end extraction/idempotence
contract" is **false**.

**Amendment:** **delete** `TestExtractPyLibIsIdempotentAndReadyLast`; do not
retarget. Coverage is unchanged because the identical aitest test already exists.

### C3 (condition) — deleting the governor-signal test drops the ONLY guard on the release→queue kick

**Verified true, and it is the sharpest finding in the review.**
`afterAdmitRelease` (`admit.go:1809`) calls `queue.signal()` *and*
`s.governor.signal()`. `TestReleaseAdmitWaiterOnlySignalsGovernor`
(`admit_test.go:933`) is the only test in the package that inspects a kick
channel at all (`grep -rn '\.kick' --include=*_test.go internal/daemon/` returns
exactly one hit, `:946`, and it is the governor's). The release e2e tests set
`server.admitPollInterval = 5 * time.Millisecond`
(`admit_release_e2e_test.go:99,197`), so they pass on the ticker alone and would
stay green if `queue.signal()` were deleted — release-to-grant latency would
silently degrade from immediate to a 250 ms poll in production.

**Amendment:** do **not** merely delete the test and do **not** write the gap
down as accepted. §5.6 gains a **fourth new test**,
`TestAfterAdmitReleaseKicksTheQueue`, asserting `afterAdmitRelease` leaves
`queue.kick` armed — the surviving half of the assertion the deleted test was
carrying. It is red against a build with `queue.signal()` removed, which is the
regression it exists to catch.

### C4 (condition) — §9's step order does not compile

**Verified true.** `governor.go` itself dereferences `s.governor`
(`:649, :671, :695, :716`), so removing the `Server.governor` field before
deleting `governor.go` breaks `internal/daemon`.

**Amendment: §9 steps 3–4 are restated as:**

- **3.** Remove the governor *call sites* only — `sliceceiling.go:421/447` plus
  the `signalGovernor` dep field and its construction, `admit.go:1813`,
  `server.go:296-304`, `server.go:461-463`, `server.go:688-696` — and the tests
  that pin them. The `Server.governor` field stays; the package still compiles.
- **4.** In **one commit**: delete `governor.go` + `governor_test.go` **and**
  remove the `Server.governor` field (`server.go:94`) and its construction
  (`:199`). These cannot be separated.

### F5 — keep the full strip set; rename the identifiers

**Verified.** Shrinking `StripGovernorEnvironment` to one key would stop scrubbing
stale inherited legacy coordinates. The concrete corner: a child launched from
inside a *still-running pre-deletion* delegate job would carry a live
`AIRA_PY_LIB` pointing at an extant extraction directory into a fastest-ee
conftest. Fail-open guarded, so not a breakage — but a real stale-plugin import
path, and keeping nine strings in a map costs nothing.

**Amendment to §3.8:** all ten keys **stay in the strip set**; only
`AIRA_CONFINE_SCOPE_ID` is still **exported**. Because a set named
`governorEnvironmentKeys` containing one non-governor key would be a lie (F6f),
the identifiers are renamed: `governorEnvironmentKeys` →
`coordinationEnvironmentKeys`, `IsGovernorEnvironmentKey` →
`IsCoordinationEnvironmentKey`, `StripGovernorEnvironment` →
`StripCoordinationEnvironment`, `runner.StripGovernorEnv` →
`runner.StripCoordinationEnv`. A comment records that the nine legacy keys are
**stripped but never set**, and why.

The gate-command identity concern in §3.8 is **withdrawn as a non-issue**:
`gate_command.go:169` operates on an explicit `EnvAllow` list, so only a gate
command that allow-lists a governor key could change digest, and the strip set is
no longer shrinking anyway.

### F6 — stale prose in RETAINED code the plan missed

**All six verified by reading them.** Same defect class as §3.11, so the same rule
applies: generated or explanatory text that describes deleted code is a
correctness defect, not a nit.

| # | Location | What is false after the deletion |
|---|---|---|
| a | `internal/pylib/aitest/worker.py:299-315` | The AIRA-37 fork-handler audit names `aira_xdist_governor` as AIRA's one real `os.register_at_fork` registrant and repeats the `-p no:` guidance. After deletion AIRA's Python has **zero** such registrants — rewrite the audit to say so; the stdlib half stays. |
| b | `internal/runner/confine_reserve.go:67` | "which the pytest governor treats as fail-open" — name the caller generically. |
| c | `internal/runner/confine_linux.go:985`, `:1012` | "already computed for the RAM governor a few lines up"; "the governor's runtime directory". |
| d | `internal/daemon/confine_reaper.go:138` | "It is now the majority of the ledger by count on a busy box" — falsified the moment per-test reservations stop. The AIRA-68 carve-out itself stays. |
| e | `internal/core/skill_test.go:521-527` | Both the `-p no:aira_xdist_governor` expectation (§3.11) and the comment justifying `"adds no slice-ledger charge"` by "a registered aira_xdist_governor still charges the ledger per test". The *expectation* `"adds no slice-ledger charge"` stays; only its stale justification goes. |
| f | `internal/pylib/env.go`, `internal/runner/env.go` | The `*Governor*` identifiers — renamed per F5. |

### F7 — §3.8's signature cleanup was incomplete

**Verified.** `AppendConfineChildEnvironment` also loses `slice` (its sole use is
`env.go:182` → `AIRA_GOVERNOR_SLICE`) alongside `runtimeDir`, `diagnostics`,
`delegateRAM`, `reserveCommand` and `memoryDefault` — leaving
`(env []string, scopeID string)`.

Removing the `runtimeDir` early return (`env.go:147`) is a **second** benign
behaviour improvement alongside the one §3.8 already records: the scope id now
also exports on a `RuntimeDir`-less launch. Stated here so neither is mistaken for
an accident.

Consequently `AppendChildEnvironment` reduces to a pure strip, so it is
**deleted** and its two callers call `pylib.StripCoordinationEnvironment`
directly — except `runner_linux.go:307`, which becomes a **second** strip of an
environment already stripped at `:286` and is therefore removed outright rather
than rewritten. Any `runtimeDir`/`diagnostics` plumbing (`inputRuntimeDir`,
`sidecarRuntimeDir`, `sidecarRuntimeRunner`) left with no consumer on the **run**
path is deleted with it; `ConfineRequest.RuntimeDir` **stays**, because
`AppendAitestChildEnvironment` still needs it (`confine_linux.go:993`).

### F8 — §10's count was low

**Verified.** `governor.go:163-197` also reads `AIRA_GOVERNOR_RAM_LOW_MARK`,
`AIRA_GOVERNOR_RAM_HIGH_MARK` and `AIRA_GOVERNOR_RAM_SKIP_BOUND`, and
`server.go:296` reads `AIRA_SCHED_MODE`. **Thirteen** environment variables are
retired, not nine. The four extra are daemon-side and die with the file.

### F9 — `admitAvailable`'s test loses no unique coverage

Noted; it also exercised the +1-prospective-job headroom term through a second
seam, and that math stays covered by the grant-path tests the deleted test's own
comment cross-references.

### Gate answers to §11's open questions — all five ADOPTED

1. **Delete `admitAvailable`** — sole production caller verified as
   `governor.go:388`; a retained helper documented as "the governor's read-only
   advisory" would be exactly the stale claim this repo treats as a defect.
2. **`confine-reserve` KEPT** — confirmed. Spec §3.8 says "the pytest **call
   sites**" verbatim; the `parentScopeID` machinery is load-bearing for drain
   convergence (`admit.go:224-232`) and AIRA-68's reaper carve-out.
3. **Keep the Slice 1 spec** — confirmed. `ConfineCPUWeightAging` is live retained
   code; slice1 is its only design record. Spec §3.8's "three specs" is wrong by
   one, and the deviation is recorded rather than silently taken.
4. **Rename `admit_governor_contention_test.go`** — yes. It never touches
   `governorSet`; the stale name is precisely what would get a live AIRA-59
   regression deleted by a future cleanup.
5. **Dropping `AIRA_SCHED_MODE` validation silently is acceptable, with no
   warning** — the only in-tree readers are the two being deleted, nothing in
   install or the unit templates sets it, and warning about a subsystem that no
   longer exists is itself clutter. Recorded in the PR description and §3.4.
