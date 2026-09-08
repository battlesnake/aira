# AIRA-177 — `aira install` defaults the slice ceiling to `enforce`

Status: **plan revision 1. No implementation has begun; this commit changes only
this document.**

Ticket: `.aira/tickets/AIRA-177.md` (P1, `admission`/`confine`/`install`).
Reverses one open question left by `.aira/tickets/AIRA-106.md` ("The
observe→enforce rollout sequencing … flag for the owner rather than default").
Branch: `aira177-slice-ceiling-enforce-default`, from `origin/master` at
`f6bb575` (the two commits after `11dba98` file the ticket and move a relation;
they touch no code).

**The direction is the project owner's, taken 2026-09-08, and is not
re-litigated here:** *"slice ceiling should be enforced by default (and
admission gated on available capacity), otherwise what's the point of it?"*
§2 records AIRA-106's contrary reasoning as history, because a deliberate prior
decision must be reversed on the record, not silently overwritten.

**Verdict, up front.** One constant splits into two. `aira install`'s ship
default for the slice ceiling becomes `enforce`; the watchdog's stays `observe`;
the daemon binary's own env default stays `off`; and the existing-install
preservation path — the property that makes this change non-retroactive — is
structurally untouched. **The only population whose behaviour changes is a first
install on a box with no managed `aira-daemon.service` and no `--slice-ceiling`
flag.** Every other input (a re-install, an explicit flag, a shim install, a
hand-started daemon, every test) renders exactly the byte it renders today.

This changes the default posture of the machine-wide admission gate every
`aira confine` job passes through, so it is the full two-loop per `CLAUDE.md`,
not the light path.

---

## 0. Source read, at the commit this plan was written against

Every line below was read fresh at `f6bb575` (== `origin/master`; every source
file cited is byte-identical at `11dba98`, the commit the worktree was first cut
from). Nothing in this plan rests on recollection or on the ticket's own summary.

| Fact | Where | Confirmed |
| --- | --- | --- |
| `defaultDaemonSubsystemMode = "observe"`, one constant, comment says "Observe for both memory subsystems" | `internal/install/install.go:42-46` | yes |
| its only two consumers: the watchdog ship default and the slice-ceiling ship default | `install.go:552`, `install.go:562` | yes |
| zero other references anywhere, tests included (`grep -rn defaultDaemonSubsystemMode internal/`) | — | yes, 3 hits: the declaration and those two |
| no identifier `defaultWatchdogMode` exists in `internal/install` | — | yes, 0 hits |
| the slice-ceiling default's justification comment ("`enforce` — a real capacity reduction on this box — stays an explicit operator decision"; "The DAEMON's own env default stays `off`") | `install.go:554-560` | yes |
| preservation runs BEFORE the ship default, and the default is reached only when the flag was absent AND no valid installed value was read | `install.go:531-573` (`resolveDaemonModes`) | yes |
| `installedSliceCeilingModeRE` reads `Environment=AIRA_DAEMON_SLICE_CEILING_MODE=…` back from the installed unit, last-assignment-wins, quoted form accepted | `install.go:94`, `:99-115` | yes |
| an unrecognised installed value falls back to the ship default rather than propagating or failing | `install.go:537-541` + `validDaemonMode` | yes |
| "absent means preserve" is carried by the `""` zero value across three hops: parse, `runInstall`/`runUserInstall`, and the root re-exec | `install.go:160-179`, `:781-787`, `:732-733` | yes |
| the installed daemon unit is read at `runUserInstall` and only when the file exists and carries the managed marker; a marker-less foreign unit is refused, not read | `install.go:803-823`, `readRegularUnitAt` `:1876-1899` | yes |
| the mode is re-resolved BENEATH the install lock from `preResolve` (the caller's explicit-only options) and re-rendered if it differs | `install.go:945-977` | yes |
| the rendered unit line the default lands in | `internal/install/assets/aira-daemon.service.in:11` | yes |
| the daemon binary's own env default is `off` for an unset/empty `AIRA_DAEMON_SLICE_CEILING_MODE` | `internal/daemon/paths.go:142-153` | yes |
| the ci-shim daemon forces all three subsystem modes off in its own environment and never reaches `resolveDaemonModes` | `internal/install/mode.go:300-312`, `install.go:767-773` | yes |
| a degenerate policy PARKS the subsystem (mode forced to `off`, refusal logged with the offending numbers) instead of freezing admission | `internal/daemon/server.go:502-521`, `sliceceiling.go:316-341` | yes |

### 0.1 The premise the ticket asked to be verified, not assumed

The ticket requires that `enforce` genuinely gates admission on the computed
ceiling "not merely a log line", **before** that is treated as established.
Verified, end to end, by reading the chain:

1. `sliceCeilingDesired(memAvailable, current, reclaimable, freeMin)` computes
   the pressure term (`sliceceiling.go:254-260`).
2. `evaluateSliceCeiling` takes the max over the 3-sample window, applies
   `min(machineTerm, …)`, quantises down to 256 MiB, clamps to the live
   `memory.max`, and publishes `sliceCeilingSnapshot{Mode, State, Ceiling, …}`
   (`sliceceiling.go:513-571`).
3. `Server.admitEffectiveMaximum(path, maximum)` looks that snapshot up under a
   leaf RWMutex and returns `sliceCeilingEffectiveMaximum(snapshot, maximum)`
   (`sliceceiling.go:746-752`).
4. **`sliceCeilingEffectiveMaximum` returns the published `Ceiling` only when
   `snapshot.Mode == sliceCeilingEnforce`** and the state is established;
   otherwise it returns the raw `maximum` unchanged (`sliceceiling.go:765-773`).
   *This one condition is the entire difference between observe and enforce.*
5. In the queue evaluator, `effectiveMaximum := s.admitEffectiveMaximum(queue.path, maximum)`
   (`admit.go:2536`) feeds **two** real gates in the same pass:
   - `available := checkedAvailable(current, effectiveMaximum, reclaimable, outstanding+adopted, headroom)`
     (`admit.go:2580`), and a waiter with `waiter.reserve > available` is **not
     granted** — it stays queued, latches contention, and arms the AIRA-59
     fairness freeze (`admit.go:2599-2620`);
   - `oversubLimit := s.oversubscriptionLimit(effectiveMaximum)` (`admit.go:2541`),
     the AIRA-114 aggregate bound, taken from the same figure so the two cannot
     disagree.
6. `confineManagement` reports the same throttled figure as `CeilingBytes`
   (`confine_manage.go:173-191`), which is a reporting surface, not a second
   gate.

**Premise: CONFIRMED.** In `enforce`, the dynamic figure is the number
`checkedAvailable` divides up, so a machine under external memory pressure
admits strictly less. In `observe`, step 4 discards it and the same code admits
exactly what it admits today. The flip is therefore the whole mechanism, and
AIRA-106's "observe applies nothing to admission" is literally true.

### 0.2 One correction to the ticket's own wording, recorded rather than followed

The ticket's suggested verification says the ceiling "becomes the effective
admission ceiling **`admitConnection`** enforces". **That is not where it
applies, and it must not be made to apply there.** `admitEffectiveMaximum` is
deliberately kept out of `admitConnection`'s own ceiling, which decides the
TERMINAL `E_ADMIT_TOO_LARGE`, `resolveAdmitReserve`'s OOM-escalation clamp, and
`resolveDelegateRAMScopeCeiling` — all three of which decide something durable
about a job rather than whether there is room right now
(`sliceceiling.go:722-745`, `admit.go:2529-2535`). A job too large for the
throttled ceiling **waits**; it is never refused terminally, and its own scope
`memory.max` is still sized from the real configured ceiling.

The consequence for this ticket is favourable and worth stating plainly: turning
`enforce` on by default cannot convert any workload into a hard failure. The
worst case it can produce is a longer queue wait — the same outcome as ordinary
contention, on the path that already has a bounded, fair, visible wait
(AIRA-71's progress line). This plan does not change that placement, and any
future proposal to extend the throttle to the terminal sites is out of scope.

---

## 1. Scope

### 1.1 In scope

**The `aira install`-time ship default for the slice-ceiling mode, and nothing
else.** Concretely, three edits in one file:

1. `internal/install/install.go:42-46` — split the shared constant:
   ```go
   // The mode a freshly-installed daemon unit declares for the memory WATCHDOG
   // when the operator gives no flag AND no managed unit already declares one.
   // Observe: it measures and reports without killing anything.
   defaultWatchdogMode = "observe"
   // AIRA-177. The same, for the slice ceiling — and deliberately NOT the same
   // value. See resolveDaemonModes for why the two diverged.
   defaultSliceCeilingMode = "enforce"
   ```
2. `install.go:552` — `opts.watchdog = defaultWatchdogMode`.
3. `install.go:554-563` — `opts.sliceCeiling = defaultSliceCeilingMode`, with
   the AIRA-106 comment block **rewritten, not deleted**: it must keep AIRA-106's
   reasoning as recorded history and state the owner decision that reversed it,
   so the next reader sees a reversal rather than an absence.

Plus the operator-facing description of the flag, which is the only surface that
describes what an omitted flag does (`internal/core/core.go:1817`):

```go
stringSpec("slice_ceiling", false, false,
    "Dynamic slice-ceiling mode; omitted keeps the installed value, or enforce on a first install",
    "off", "observe", "enforce"),
```

The watchdog's own description at `:1815` is left alone — its default did not
change. (Verified there is no golden-file or help-text test to update: the
string appears in exactly one file.)

### 1.2 The rename, and why the alternative was rejected

The ticket permits either "its own unrenamed constant or a clearly-separated
one". This plan **renames** `defaultDaemonSubsystemMode` → `defaultWatchdogMode`.

The name asserts a property — *every* daemon subsystem defaults to this — that
becomes false the instant the two diverge. Leaving a constant named "daemon
subsystem mode" holding `"observe"` while only the watchdog reads it is a live
trap for the third subsystem that already exists (`AIRA_DAEMON_OOM_STEER_MODE`,
forced off today at `mode.go:311` but with no install flag yet): whoever adds
its install flag would reach for the generically-named constant and silently
inherit a default nobody chose for it. The rename is mechanical and total — three
occurrences, no test references, no exported identifier, no cross-package use —
so it carries none of the risk that usually argues for leaving a name alone.

Rejected alternative: keep `defaultDaemonSubsystemMode` for the watchdog and add
`defaultSliceCeilingMode` beside it. Smaller diff, but it leaves the misleading
name in place permanently, which is the same "silent inheritance" failure class
this ticket is fixing at the level of behaviour.

### 1.3 Explicitly NOT in scope — verified must-not-change list

| Must not change | Where | How this plan guarantees it |
| --- | --- | --- |
| the watchdog's install default (`observe`) | `install.go:552` | its own constant; pinned by T1 and T3 in the **same assertion** as the slice ceiling, so a blanket flip cannot pass |
| the daemon binary's raw env default (`off`) for an unset variable | `daemon/paths.go:142-153` | not touched; `install` renders an explicit `Environment=` line, so the two are independent by construction. Pinned by the untouched `TestSliceCeilingEnvParsing` (`daemon/sliceceiling_test.go:827`) |
| every existing install, on re-install with no flag | `install.go:537-541` | preservation runs before the default and is structurally unchanged; T2b/T2c/T4 |
| `--slice-ceiling observe` / `off` as explicit opt-outs | `install.go:439-443`, `:537` | explicit flag still wins; T2d/T2e |
| the root re-exec not forging an omitted flag | `install.go:732-733` | untouched; `TestReexecDoesNotForwardUngivenDaemonModes` re-run unchanged |
| the ci-shim install | `mode.go:300-312`, `install.go:767-773` | never reaches `resolveDaemonModes`; forced `off` |
| the ceiling formula, damping, quantisation, basis, TTL hold | `daemon/sliceceiling.go` | no daemon-package change at all |
| the two application sites, and the terminal / scope-sizing sites that must stay on the raw maximum | `admit.go:2536`, `confine_manage.go:174`; `admitConnection`, `resolveAdmitReserve`, `resolveDelegateRAMScopeCeiling`, `evaluateWorkerAdmit` | no change; §0.2 |
| any wire field, response shape, error code or log line | — | no change |
| this machine's already-installed mode | live box | deferral D1; this ticket ships code, it does not deploy |

---

## 2. Why AIRA-106 chose `observe`, and why that is being reversed

Recorded so the reversal is legible, not silent.

AIRA-106 shipped `observe` because "a first install can never change what the
machine does" (`install.go:42-46`), and because `enforce` is "a real capacity
reduction on this box". Both statements are still true. What changed is the
owner's judgement of the trade: measured on the owner's box on 2026-09-08, live
confine scope caps summed to ~81.6 GiB against 78 GiB of RAM (an
`--allow-overcommit` configuration with a 128 G aggregate bound) while the
throttle that would have reduced the effective admission ceiling to ~52.8 GiB
sat in `observe`, computing the number and applying nothing. A peer session
reported real OOMs the same day.

AIRA-106's own proposed exit criterion was "≥24 h of `observe` uptime including
a period of ≥8 concurrent confine jobs, with every logged `slice ceiling` line
showing `effective` comfortably above the `sliceAnon` logged beside it". What is
**verified** here is the first clause and the box's posture, read from the live
unit: `~/.config/systemd/user/aira-daemon.service` declares
`AIRA_DAEMON_SLICE_CEILING_MODE=observe` and was last written **2026-09-06
19:08** — so the mode has been declared `observe` for ~48 h, and the ~81.6 GiB
of live caps above is what that observation produced. (The same unit declares
`AIRA_DAEMON_WATCHDOG_MODE=enforce`, so AIRA-111's revert has been repaired.)
What is **not** verified here is the journal clause: this plan does not claim to
have audited every logged `slice ceiling` line, and a reviewer who wants that
evidence should run
`journalctl --user -u aira-daemon.service | grep 'slice ceiling'` rather than
take it from this document. The owner's decision does not rest on it — the
measured cap-versus-RAM overcommit above is the evidence that moved it — but the
distinction between "measured" and "assumed" is recorded rather than blurred.

The residual safety argument stands on §0.2: enforce can only make a job wait,
never fail.

---

## 3. Invariants

- **I1.** The watchdog's freshly-installed mode is `observe`, before and after,
  and is decided by an identifier no slice-ceiling code path reads.
- **I2.** `sliceCeilingModeFromEnv()` returns `off` for an unset or empty
  `AIRA_DAEMON_SLICE_CEILING_MODE`. Unchanged. A daemon started outside the
  installed unit — `aira daemon serve` by hand, the dispatcher's
  `/proc/self/exe daemon` fallback, every unit test — is byte-identical.
- **I3.** For every input where the installed managed unit declares a valid
  slice-ceiling mode and no `--slice-ceiling` flag was given, the rendered unit
  declares **the installed mode**, whatever the ship default is. The default is
  unreachable in that state (`install.go:537-541` returns before `:561`).
- **I4.** An explicit `--slice-ceiling <mode>` produces exactly `<mode>`, with or
  without an installed unit.
- **I5.** The population whose rendered byte changes is exactly: no
  `--slice-ceiling` flag ∧ no managed `aira-daemon.service` carrying a valid
  mode. Nothing else.
- **I6.** The pre-lock and under-lock resolutions remain the same function over
  the same explicit-only `preResolve` options, so the new default cannot make
  the two passes disagree and trigger a spurious re-render/restart.
- **I7.** No file under `internal/daemon/` changes.
- **I8.** No behaviour change to a running daemon: the mode is read at
  `Serve`-time from the environment. An install that changes the unit content
  restarts the daemon (existing behaviour); an install that does not, does not.

---

## 4. Risks

**R1 — a fresh install now really reduces admitted capacity on a box nobody
tuned.** Accepted, and this is the ticket's whole point. Bounded by three
existing mechanisms, all verified: a degenerate `reserveMax`/`freeMin` pair
parks the subsystem with the offending number named rather than freezing the
queue (`server.go:508-516`, `sliceceiling.refusal()`); the throttle can only
make a waiter wait, never fail (§0.2); and `--slice-ceiling observe|off` remains
a one-command opt-out that survives every subsequent re-install (I3).

**R2 — the retroactive flip.** The failure that would make this ticket harmful
is an existing `observe` box being silently upgraded by an unrelated deploy —
the exact defect class AIRA-106 fixed at `install.go:531-573`. This is the
load-bearing non-regression: guarded structurally by I3 and pinned by T2b and
T4, and T4 asserts the *side effects* (no unit rewrite, no daemon restart), not
just the rendered string.

**R3 — an existing AIRA-106 test becomes half-porous.**
`TestInstallDaemonReinstallPreservesModes` (`daemon_service_test.go:213`) seeds
`sliceCeiling: "enforce"` and asserts `enforce` survives a flagless re-install.
Once `enforce` is the ship default, that half of the assertion **passes whether
preservation works or not** — a broken preservation would default to the same
value. The watchdog and interval halves still discriminate. This is a real,
newly-introduced hole in an existing load-bearing test, and the mitigation is
T4, which seeds `observe` — the one value that cannot be produced by defaulting.
The plan requires a comment at that test recording the loss, so the porosity is
written down rather than left for someone to rediscover.

**R4 — enforce is inert for the first few samples after a start.** The window is
partial for `sliceCeilingSamples`(3) intervals, and
`sliceCeilingEffectiveMaximum` returns the raw maximum while `State ==
unevaluated` (`sliceceiling.go:766`). So a freshly installed box does not clamp
instantly. Pre-existing, unchanged by this ticket, named here so nobody reads
"enforce by default" as "gated from the first millisecond".

**R5 — `freeMin = 8 GiB` sits ~2 GiB above the watchdog's SIGKILL trip.** AIRA-106
raised this to the owner as an open question and it was left at the owner's own
number. Turning enforce on by default makes the steady state it targets the
*normal* state rather than a hypothetical one, which raises the stakes of that
question without changing its answer. Deferral D2, not scope creep: it is one
environment variable and no rebuild.

**R6 — a mechanical rename hiding a behaviour change.** Mitigated by the counted
reference audit in §1.2 (3 occurrences, 0 in tests) and by T1/T3 asserting both
defaults in one place, so a rename that accidentally pointed the watchdog at the
new constant fails immediately.

---

## 5. Tests

TDD: T1's amendment and T2 are written first and must be RED against
`origin/master` before the constant splits.

**T1 — `TestResolveDaemonModesPreservesInstalledModes` (amend, `internal/install/daemon_service_test.go:100-103`).**
The first-install case currently asserts `watchdog == "observe" && sliceCeiling == "observe"`.
Amend to `watchdog == "observe" && sliceCeiling == "enforce"`, keeping both in
**one** assertion with a message that says the two defaults are deliberately
different. Deliberately not a deletion: this line is the existing pin on the
ship defaults, and it must keep pinning them.

**T2 — new `TestResolveDaemonModesFreshInstallDefaultsSliceCeilingToEnforce`
(`internal/install/daemon_service_test.go`).** A table at the deciding seam,
`resolveDaemonModes`, covering the whole input space of §1.3:

| case | flag | installed unit declares | want slice ceiling | want watchdog |
| --- | --- | --- | --- | --- |
| a — fresh install | absent | (no unit) | `enforce` | `observe` |
| b — **the critical non-regression** | absent | `observe` | `observe` | — |
| c — an off box stays off | absent | `off` | `off` | — |
| d — explicit opt-out on a fresh box | `observe` | (no unit) | `observe` | — |
| e — explicit beats installed | `off` | `enforce` | `off` | — |

**T3 — new `TestInstallFreshDaemonUnitEnforcesSliceCeilingByDefault`
(`internal/install/daemon_service_test.go`).** End to end through
`newFakeInstall` + `runInstall(d, installOpts{memoryMax: "16G"})` against a
clean state, asserting the **rendered** `aira-daemon.service` contains both
`AIRA_DAEMON_SLICE_CEILING_MODE=enforce` and `AIRA_DAEMON_WATCHDOG_MODE=observe`.
T2 pins the decision; this pins that the decision reaches the file, through
`renderDaemonUnit`'s own `validDaemonMode` check and the under-lock re-resolve.

**T4 — new `TestInstallReinstallDoesNotUpgradeObserveSliceCeiling`
(`internal/install/daemon_service_test.go`), the replacement for R3.** Install
once with an explicit `sliceCeiling: "observe"`, snapshot `state.writes` and
clear `state.commands`, then `runInstall(d, installOpts{memoryMax: "16G"})` with
no mode flags. Assert all three:
1. the unit still declares `AIRA_DAEMON_SLICE_CEILING_MODE=observe`;
2. `state.writes` is unchanged — a silent upgrade would rewrite the unit;
3. no `systemctl --user restart aira-daemon.service` in `state.commands` — a
   silent upgrade would bounce the live daemon into the new mode.

Assertions 2 and 3 are what make this a non-regression rather than a string
coincidence, and they mirror `TestInstallDaemonReinstallPreservesModes`'s own
side-effect assertions.

**T5 — re-run unchanged, as guards on the "absent" sentinel:**
`TestParseInstallWatchdogFlags` (an omitted flag must still parse to `""`, or
preservation can never fire) and `TestReexecDoesNotForwardUngivenDaemonModes`
(the `sudo aira install` path must still not forge `--slice-ceiling`, or every
sudo install would turn "absent" into an explicit `enforce` and defeat I3).

**T6 — re-run unchanged, as the I1/I2 guards:**
`TestInstallDaemonIdempotentAndWatchdogChangeRestarts`,
`TestInstallDaemonConcurrentModeChangeSurvivesTheLock`,
`TestRenderDaemonUnitIsManagedBoundedAndIndependent` (install side) and
`TestSliceCeilingEnvParsing` (`internal/daemon/sliceceiling_test.go:827` — the
daemon's own `off` default).

### 5.1 Non-porosity — each new/edited test run against a wrong implementation

Per `docs/adversarial-verification.md`, a test that cannot go RED proves
nothing. Required evidence, recorded in the build report as a table:

| wrong implementation | must go RED |
| --- | --- |
| W1: no change at all (default stays `observe`) | T1, T2a, T3 |
| W2: blanket flip of the shared constant (watchdog also `enforce`) | T1, T3 |
| W3: the default applied BEFORE preservation (assignment hoisted above the installed-unit read, or pre-filled in `parseInstallArgs`) | T2b, T2c, T4, T5 |
| W4: `sliceCeilingEffectiveMaximum` gated on `Mode != off` instead of `== enforce` (i.e. observe silently enforcing) | existing `internal/daemon` ceiling tests — re-run to confirm they still discriminate, since this ticket's premise (§0.1) rests on that one condition |

### 5.2 Commands, with exit codes recorded exactly

All heavy commands under `aira confine`, serialised, per `CLAUDE.md`:

```sh
aira confine -- go build ./...
aira confine -- go vet ./...
aira confine -- go test ./internal/install/ -run 'Install|ResolveDaemonModes|Parse|Reexec|Render' -count=1
aira confine -- go test ./internal/daemon/ -run 'SliceCeiling' -count=1
aira confine -- go test ./...
```

No `-race` (AIRA-20). No real-cgroup test is required: this ticket changes a
rendered string, not the cgroup interaction — and claiming a real-cgroup
verification it does not need would be the false-confidence this project's
review policy exists to catch. Exit codes recorded per command; `pass`, `fail`
and `unevaluated` distinguished; no green claimed from truncated output.

---

## 6. Deferrals

- **D1 — flipping THIS machine's installed slice-ceiling mode from `observe` to
  `enforce`.** Explicitly excluded by the ticket: the owner asked to wait until
  the slice is quieter, or to have it monitored, given live admitted jobs on a
  shared box. It is one command (`aira install --slice-ceiling enforce`) and no
  rebuild, and it is a live-machine operational decision tracked separately.
  This ticket ships the default; it deploys nothing.
- **D2 — `freeMin`'s 2 GiB margin above the watchdog trip** (R5). AIRA-106's
  second question to the owner, still open, now more load-bearing. Needs its own
  ticket if the owner wants 16 GiB; it is an environment variable, not code.
- **D3 — the accepted pre-lock `MemoryMax` window** (AIRA-106 design doc §9).
  Untouched and unaffected: the two mode options already re-resolve under the
  lock, which is what this change relies on.
- **D4 — an install flag for `AIRA_DAEMON_OOM_STEER_MODE`.** Named only because
  §1.2's rename is partly motivated by it. Not built here.
- **D5 — extending the throttle to the terminal / scope-sizing sites.** Rejected
  by design in AIRA-103 and re-confirmed in §0.2. Out of scope permanently
  unless a ticket argues it afresh.

---

## 7. Review

Two-loop per `CLAUDE.md`: this plan to an orthogonal plan-review lineage and a
plan gate before implementation; adversarial build review examining both
false-fail and false-pass directions before merge, with every confirmed
counterexample becoming a regression test. The false-pass direction to press
hardest is R3: the change makes an existing preservation test stop
discriminating, and a reviewer should check that T4 actually replaces what was
lost rather than merely resembling it.
