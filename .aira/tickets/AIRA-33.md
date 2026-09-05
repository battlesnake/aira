---
{"schema":1,"id":"AIRA-33","project":"aira","title":"aitest Slice 4 — retire aira_xdist_governor / governor-slot / daemon governor.go","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","cleanup","pytest"],"hold":false,"relations":[]}
---
Spec: docs/superpowers/specs/2026-09-01-aitest-design.md (§3.8, §5 Slice 4, §6).

Delete internal/pylib/aira_xdist_governor, aira governor-slot
(internal/runner/governor_slot.go), pytest call sites of aira confine-reserve,
and internal/daemon/governor.go's park/active-set scheduler + the three
2026-08-30 scheduler-slice specs, once AIRA's own dogfood suite has run clean
on aitest. Before deleting governor.go: grep-sweep for any non-pytest caller
of the CPU park/active-set machinery (spec §6 flags this as unconfirmed, not
assumed). Blocked by Slice 2 (needs AIRA's own suite migrated and clean).

## Precondition restated (2026-09-04, backlog-remediation Phase 0, plan section 2) — text only

**The stated blocker is satisfied and therefore misleading.** This ticket reads
"Blocked by Slice 2 (needs AIRA's own suite migrated and clean)". Slice 2 is
AIRA-31, which is **done**. A reader coming to this ticket directly — rather than
through the backlog-remediation plan — would conclude it is ready to build. It is
not.

**The real, current precondition** is the one the plan records in section 1:

> AIRA-91 Part A built + fastest-ee re-verified + the `FASTEST_NO_AITEST=1` pin
> removed.

AIRA-91's root cause is closed (`systemd-oomd` `cgroup.kill`s the whole confine
scope under sustained `memory.high` reclaim pressure — a real exit 137, never
exit 0). Its **Part A** — the confine trailer's kill-attribution fix, unified with
AIRA-70 — is Phase 1 Fix 5 of that plan. Until Part A ships and fastest-ee runs
clean on aitest without the `FASTEST_NO_AITEST=1` pin, AIRA's own dogfood
evidence for "the suite has run clean on aitest" does not exist, and deleting the
xdist stack would remove the fallback before its replacement is trusted.

The pre-deletion grep-sweep for a non-pytest caller of the CPU park/active-set
machinery (spec section 6 flags it as unconfirmed, not assumed) is still required
and still unrun.

Knock-on, unchanged: AIRA-17, AIRA-26, AIRA-65 and AIRA-77 close automatically
with this deletion and need no separate action; AIRA-25 closes via whichever of
AIRA-29 or this ticket lands first. AIRA-20's `-race` CI restoration additionally
depends on this ticket landing (or on quarantining
`TestRealPytestRAMForkDoesNotPinHelperStdin`, `internal/pylib/pytest_integration_test.go:606`).

## Resolution (2026-09-06) — DONE, merged

**Owner override of the stated precondition.** The section above says the real
precondition is "AIRA-91 Part A built + fastest-ee re-verified + the
`FASTEST_NO_AITEST=1` pin removed". The owner directed this build to **delete
now regardless of fastest-ee's own migration timing**, accepting a narrow
accounting regression on the three still-pinned fastest-ee legs: they fall back
to flat 512 MB whole-job charging until fastest-ee migrates them onto aitest on
its own schedule. That regression is **explicitly accepted, not a blocker**, and
is recorded here rather than discovered later.

**The pre-deletion grep sweep spec §6 asks for was RUN, result: no non-pytest
caller.** The daemon `governor` verb (`server.go` dispatch) had exactly one
in-tree client, `runner.GovernorSlot`, whose only caller was the `governor-slot`
CLI verb, whose only caller was `aira_xdist_governor/__init__.py`. `governorSet`
and every helper were unexported and confined to `internal/daemon`; its only
other entry points were `signal()` (two call sites, both analysed) and the
`AIRA_SCHED_MODE` startup wiring.

### Deleted (whole files)

```
internal/daemon/governor.go                       (755)
internal/daemon/governor_test.go                  (626)
internal/runner/governor_slot.go                  (394)
internal/runner/governor_slot_linux.go            ( 26)
internal/runner/governor_slot_stub.go             (  5)
internal/runner/governor_slot_test.go             (466)
internal/pylib/aira_xdist_governor/{__init__.py,shim.py,README.md,.gitignore}
internal/pylib/pytest_integration_test.go         (959)
internal/pylib/reservation_sizing_test.go         ( 78)
docs/superpowers/specs/2026-08-30-scheduler-slice2-daemon-active-set-plan.md
docs/superpowers/specs/2026-08-30-scheduler-slice3-ram-aware-admission-plan.md
```

`governor_slot_linux.go`/`_stub.go` were **not** in the brief's list; they exist
solely to define `setGovernorParentDeathSignal()` for `governor_slot.go` and must
go with it or the package fails to compile.

### Unwired

`cmd/aira/main.go` (the `governor-slot` verb end to end: dispatch, arg parsing,
`parseGovernorSlotArgs`, `runGovernorSlotCommand`, and the stderr line that named
the CPU governor); `cmd/aira/scope_dir.go` + its test table; `internal/daemon/
server.go` (`governor` field, construction, `AIRA_SCHED_MODE` parse, capacity/mode
update, drain stop, verb dispatch, two handler-enumeration comments); `admit.go`
(`s.governor.signal()` in `afterAdmitRelease`, and the now-dead `admitAvailable`
+ its lock-order comment); `sliceceiling.go` (both `signalGovernor` call sites,
the deps field, its construction, and `admitEffectiveMaximum`'s three-sites doc);
`deadlines.go`; `internal/pylib/env.go` (nine exported keys; signature collapsed
to `AppendConfineChildEnvironment(env, scopeID)`; `AppendChildEnvironment`,
`extractForChild`, `childEnvFailureOnce`, `DefaultTestMemoryReserve` deleted);
`internal/pylib/extract.go` (the `aira_xdist_governor` embed, `embeddedRoot`,
`embeddedPyLib`, `ExtractPyLib`); `internal/runner/runner_linux.go` (the second,
now-redundant strip and the sidecar injection; `SidecarRuntimeDir`);
`internal/core/command.go` (`sidecarRuntimeRunner`, `sidecarRuntimeDir`, and
`effectiveCommandEnvironment`'s `runtimeDir`/`diagnostics` parameters);
`internal/core/skill.go`; `README.md`; `.github/workflows/ci.yml`.

**`internal/core/core.go` and `routing.go` were NOT touched** — contrary to the
brief's conditional listing. `governor-slot` is one of the eleven out-of-table
verbs and was never in the verb table or the routing switch (`grep -n governor`
on both returns nothing).

### The `sliceceiling.go` wake-path question — VERIFIED IN SOURCE, NOT ASSUMED

The brief required this be proved. It was derived independently three times: by
the plan, by the Fable plan-gate, and by me directly against source before
merging. **Resolution: nothing else needs waking.** Evidence:

1. `sliceQueue.kick` is now the **only** kick channel in `internal/daemon`
   (`admit.go:482` field, `:1267` make, `:1330` receive, `:1342` send). The
   governor's own `kick` went with `governor.go`.
2. `sliceQueue.runEvaluator` (`admit.go:1318-1338`) selects over `q.kick`,
   `ticker.C` and `q.stop`, and the `ticker.C` case calls `q.signal()` — so the
   queue re-evaluates itself every `q.poll` (`defaultAdmitPollInterval = 250ms`,
   `paths.go:43`) with no external kick at all.
3. `sliceceiling.go` **never** called `queue.signal()`; before this change its
   only wake call was `deps.signalGovernor()`. The queue's observation of a raise
   was always ticker-driven.
4. Grant delivery to a blocked `admit` handler is `close(waiter.grantedCh)` at
   `admit.go:1556`/`:1643` (both inside `evaluateAdmitQueue`, i.e. on that ticker)
   and `:1722` (`timeoutAdmitWaiter`, driven by the handler's own timer). The
   handler at `:1174` selects `grantedCh` / its own deadline / `stopping` /
   peer-ctx. Nothing waits on a governor signal.
5. `worker-admit` waits on `time.After(sleep)` (`worker_admit.go:989-990`) with no
   kick channel, and `admitEffectiveMaximum` is deliberately never applied to
   `evaluateWorkerAdmit` — so the ceiling does not gate it in the first place.
6. The daemon's only `sync.Cond` is `projectCond` (`eject.go:128` Wait, `:161`
   Broadcast), unrelated to admission or the ceiling.

Every remaining consumer of a ceiling raise is poll-driven at 200-250 ms or is a
request-driven read (`confineManagement`). AIRA-103's raise/unthrottle path still
publishes; `TestSliceCeilingRaiseStillPublishesAfterTheGovernorWakeIsGone` pins
that the raised snapshot still reaches `deps.publish`.

### Kept deliberately (checked against overreach)

- `internal/daemon/cpuslots.go` `desiredCPUSlots` — shared with AIRA-64's
  `cpuslots_gate.go`; doc comment corrected off "the daemon governor's active-set
  capacity".
- `internal/daemon/admit_governor_contention_test.go` — AIRA-59's fairness-freeze
  regression, never touched `governorSet`. **Renamed** to
  `admit_small_reservation_contention_test.go` /
  `TestSmallPinnedReservationsAreNotStalledByALargeNeighbourHead`, because the
  stale name is precisely what would get a live regression deleted by a future
  governor cleanup.
- `aira confine-reserve` and `internal/runner/confine_reserve*.go` — spec §3.8
  says "the pytest **call sites** of `aira confine-reserve`", not the verb, and
  the `parentScopeID` machinery it drives is load-bearing for drain convergence
  (`admit.go:224`) and AIRA-68's reaper carve-out. Recorded as an explicit
  non-deletion, so `internal/core/core.go`/`routing.go` stayed untouched too.
- `AIRA_CONFINE_SCOPE_ID` — `runner.InheritedConfineScopeID` reads it and
  `confine_reserve_linux.go` uses it as `ParentScopeID`; dropping it would have
  silently turned every sub-reservation into a fresh slice charge.
- `scheduler-slice1-cpuweight-aging-plan.md` — spec §3.8's "three specs" is
  **wrong by one**: slice 1 documents `ConfineCPUWeightAging` and the `+cpu`
  delegation, which are live retained code. Kept, with a status banner; the
  umbrella scheduler design got one too.

### Deliberate behaviour changes, stated rather than slipped in

1. **`AIRA_SCHED_MODE` startup validation removed.** A malformed value used to
   make `Serve` return an error and the daemon refuse to start; it is now ignored.
   Correct — it configured only the deleted scheduler — and nothing in install or
   the unit templates sets it.
2. **The scope id now exports on a `RuntimeDir`-less launch.** It used to be
   gated behind a successful sidecar extraction and a non-empty `RuntimeDir`,
   neither of which had anything to do with it. Pinned by
   `TestConfineExportsTheScopeIDWithoutARuntimeDir`.
3. **The nine retired env keys stay in the STRIP set while never being set
   again.** A child of a still-running *pre-deletion* delegate job can still
   inherit a live `AIRA_PY_LIB` pointing at an extant extraction dir; carrying
   that into a guarded conftest is a stale-plugin import path. Identifiers renamed
   so the set is not a lie: `coordinationEnvironmentKeys`,
   `IsCoordinationEnvironmentKey`, `StripCoordinationEnvironment`,
   `runner.StripCoordinationEnv`.
4. **`--delegate-ram` no longer per-test-governs an xdist suite.** Nothing in this
   repo does any more; for fastest-ee this is the owner-accepted flat-512 MB
   fallback.

### Open questions in the brief, and how each resolved

| Question | Resolution |
|---|---|
| Does the ceiling raise/unthrottle path still wake what needs waking? | **Proved: nothing else needs waking** (evidence above), not assumed. |
| Widen/delete `confine-reserve`? | **KEPT.** Spec deletes call sites, not the verb; its daemon machinery is referenced by landed AIRA-68/101 work. Hence `core.go`/`routing.go` untouched. |
| Is dropping `AIRA_PY_LIB` (short-circuiting fastest-ee's conftest guards) intended and safe? | **Both.** The guards are `if not aira_py_lib: return` inside `try/except Exception: pass`, so unset means immediate return, no import, no exception — it is the documented off-switch, fail-open by construction. |
| Delete the three 2026-08-30 scheduler-slice specs? | **Two, not three.** Slice 1 documents live retained code; deviation recorded above. |
| Rename `admit_governor_contention_test.go`? | **Yes**, judged warranted (see above), though the brief made it optional. |
| Consistency with AIRA-65 / AIRA-77 resolutions? | **Consistent, no action.** AIRA-65 (close-not-needed) rests on "slated for deletion by AIRA-33" and carries a "reopen if AIRA-33 is ABANDONED" residual, which landing discharges; its requested doc follow-up (stop citing AIRA-65 in `ci.yml:62-66`) is done. AIRA-77 (close-superseded-by-AIRA-33) says the double-arming "remains live in source until AIRA-33 deletes the plugin, with the SKILL.md guard as the interim" — the plugin is deleted and the interim guard prose removed in the same change. Neither ticket file modified. |

### Two-loop

**Fable plan-gate: GATE-PASS-WITH-CONDITIONS**, no blocking findings
(`docs/superpowers/reviews/2026-09-05-fable-aira33-plan-gate.md`). It independently
re-derived the wake-path proof across eight consumers and returned **HOLDS**. Four
conditions + five findings, all verified against source and folded into plan v2
§12: (C1) new test 3 does **not** fail against master, so it is labelled
forward-only rather than given a fabricated failure record; (C2) the proposed
`ExtractPyLib` test retarget would have produced a line-for-line duplicate of
`TestExtractAitestIsIdempotent` — deleted instead; (C3) deleting
`TestReleaseAdmitWaiterOnlySignalsGovernor` would have dropped the **only** guard
on the release→queue kick (the e2e tests set `admitPollInterval = 5ms` and stay
green on the ticker alone, so release-to-grant latency could silently degrade to
250 ms polling) — `TestAfterAdmitReleaseKicksTheQueue` added to carry it;
(C4) the planned step order did not compile, since `governor.go` dereferences
`s.governor`.

**Adversarial build-review (Codex/Sol, read-only over the real tree): BLOCK on 5
findings.** Every one was re-verified in source before acting; all five were real
and all five are fixed:

1. **P1 — `README.md` still prescribed `@pytest.mark.aira_mem("2G")`.** I had
   rewritten the paragraph above it and missed the code block. A user marking a
   2 GiB test would get the 512 MB default, an OOM, a retry, then `unevaluated`.
   Replaced with the `AIRA_AITEST_ESTIMATED_BYTES` guidance that is actually read.
2. **P2 — `internal/core/skill.go` "without the slice accounting for it at all"**
   was misleading: cgroup memory is hierarchical, so worker growth *does* land in
   the slice's `memory.current` and *does* shrink the headroom later admissions
   see. Rewritten to say precisely that nothing is *reserved* for that growth.
3. **P2 — `internal/runner/confine.go` `DefaultDelegateRAMOverhead`'s rationale**
   still justified itself by "the suite delegates RAM accounting to its per-test
   reservations" and by double-booking them in `queue.outstanding`. That caller no
   longer exists; the constant and value are unchanged, the reason restated as
   aitest's per-worker containment.
4. **P2 — porous test.** The inherited half of
   `TestCoordinationEnvironmentIsStrippedAndKeptOutOfTheDigest` asserted only
   absences and digest equality, both of which an EMPTY environment would satisfy.
   Added a control-variable witness plus an explicit empty-digest check.
5. **P2 — a FALSE claim in a new test's own comment.** I had written that the
   publish and the wake call "shared one `if`". They did not: `publishSliceCeiling`
   was called *before* the signal branch, and
   `TestSliceCeilingSubQuantumMovementIsIgnored` already asserts a multi-quantum
   raise. Comment corrected to state the test's real, narrower added value.

Sol's own verdict line: **WAKE-PATH: HOLDS**, independently re-derived.

### Verification — exact exit codes

| Command | Exit |
|---|---|
| `make ci` (gofmt-check + vet + build + `go test ./...` `-count=1 -timeout 20m`) | **0** |
| `go vet ./...` | **0** |
| `go build ./...` | **0** |
| `make fmt-check` | **0** |
| `AIRA_REAL_PYTEST=1 go test ./internal/pylib/` | **0** |
| `AIRA_REAL_CGROUP=1 go test ./internal/runner/ ./internal/daemon/ ./cmd/aira/` | **0** |

All packages `ok`; no skips claimed as passes.

**One honest caveat, measured not assumed.** Running the real-pytest tier *nested
inside* an `aira confine --delegate-ram` job fails
`TestRealPytestAitestPackageUnitTests` and
`TestRealPytestAitestEndToEndRealDaemonAndCgroupPassFailOnly` — the inner aitest
bootstrap finds its supervisor in neither the outer scope nor `.aira-supervisor`
and falls back to unconfined. This is **pre-existing and unrelated to AIRA-33**:
the identical command on a detached `6f1d35a` master baseline worktree fails the
identical two tests. Under a non-nested `aira confine --memory-reserve 6G` the
whole tier is exit 0. Cross-compile failures for darwin/windows
(`unix.Renameat2`, `syscall.Kill`) are likewise pre-existing on master; the
release target is one static Linux binary.

### Yield

60 files, **+1,585 / −5,292** (net −3,707). One CLI verb, one daemon verb, one
daemon subsystem and **thirteen** environment variables retired (nine child-side,
plus `AIRA_GOVERNOR_RAM_LOW_MARK`/`_HIGH_MARK`/`_SKIP_BOUND` and `AIRA_SCHED_MODE`
daemon-side — the brief's count of nine was low). Three false statements removed
from agent-facing generated guidance, now enforced by
`TestSkillNamesNothingFromTheRetiredXdistGovernor`, a whole-document scan of both
the SKILL and the agent guide.
