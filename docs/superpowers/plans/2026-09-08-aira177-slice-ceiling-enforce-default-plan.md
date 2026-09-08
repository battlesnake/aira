# AIRA-177 — `aira install` defaults the slice ceiling to `enforce`

Status: **plan revision 2. No implementation has begun; this commit changes only
this document and the ticket's scope sentence.**

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

### What revision 2 changed, and why

Revision 1 was gated FAIL on one code-grounded gap, found independently by both
review lineages: its verdict paragraph claimed a blast radius —
*"a first install on a box with no managed `aira-daemon.service`"* — that
`resolveDaemonModes` provably does not deliver, and no test in the plan pinned
the population that actually changes. Revision 1's own I5 stated the correct,
wider population; the two contradicted each other. Revision 2:

1. decides and records that wider population explicitly (**§1.4**), corrects the
   verdict paragraph below to match I5, and amends the ticket's own scope
   sentence, since the ticket is the ledger;
2. adds the three T2 rows (f, g, h) that pin the decision, which nothing in
   revision 1 discriminated;
3. records a **second** newly-introduced porosity revision 1 missed —
   `TestInstallDaemonConcurrentModeChangeSurvivesTheLock` (**R7**, fixed by
   **T7**);
4. restates **I3** in terms of the `""` sentinel rather than a non-existent
   early return — the wrong mechanism was exactly why the gap was invisible;
5. names the three real discriminators in the **W4** non-porosity row instead of
   "re-run the existing tests";
6. makes **D1** checkable: it names `install.sh`'s flagless `aira install` as the
   real deploy path, the live-unit precondition that keeps this box on `observe`
   through it, and the read-only pre-deploy re-verification;
7. resolves **I7** against the stale `internal/daemon/paths.go:138` comment, and
   fills in the T2 `want watchdog` column, the flag-description wording, and a
   line on the three dated records that state the old default.

**Verdict, up front (corrected in revision 2).** One constant splits into two.
`aira install`'s ship default for the slice ceiling becomes `enforce`; the
watchdog's stays `observe`; the daemon binary's own env default stays `off`; and
the existing-install preservation path — the property that makes this change
non-retroactive for every install that declares a readable mode — is
structurally untouched. **The population whose behaviour changes is exactly the
one I5 names: no `--slice-ceiling` flag AND no managed `aira-daemon.service`
carrying a mode `validDaemonMode` accepts.** That is wider than "a fresh box":
it also covers a managed unit with no slice-ceiling line at all, one carrying an
unrecognised value, and one carrying a multi-assignment or reset `Environment=`
line. §1.4 decides that case deliberately rather than inheriting it. Every other
input (a re-install of a unit declaring a valid mode, an explicit flag, a shim
install, a hand-started daemon, every test) renders exactly the byte it renders
today.

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
| preservation runs BEFORE the ship default, and the default is reached only when the flag was absent AND no *valid* installed value was read | `install.go:531-573` (`resolveDaemonModes`) | yes |
| `installedSliceCeilingModeRE` reads `Environment=AIRA_DAEMON_SLICE_CEILING_MODE=…` back from the installed unit, last-assignment-wins, quoted form accepted | `install.go:94`, `:99-115` | yes |
| an unrecognised installed value falls back to the ship default rather than propagating or failing | `install.go:537-541` + `validDaemonMode` | yes |
| **`installedEnvironmentValue` returns `""` — indistinguishable from "no unit" — for an absent line, a multi-assignment line, and an `Environment=` reset** | `install.go:103-115` | yes; the doc comment says so explicitly |
| **the pre-AIRA-106 managed daemon unit has NO slice-ceiling line: `@SLICE_CEILING_MODE@` first appears in `7ecaf8a` (AIRA-106)** | `git show ee6e214:internal/install/assets/aira-daemon.service.in` — watchdog mode + interval + `XDG_STATE_HOME` only | yes |
| "absent means preserve" is carried by the `""` zero value across three hops: parse, `runInstall`/`runUserInstall`, and the root re-exec | `install.go:160-179`, `:781-787`, `:732-733` | yes |
| `sliceCeilingGiven()` is literally `o.sliceCeiling != ""` — one sentinel for three distinct inputs (see I3) | `install.go:179` | yes |
| the installed daemon unit is read at `runUserInstall` and only when the file exists and carries the managed marker; a marker-less foreign unit is refused, not read; presence is captured separately as `daemonPresent` | `install.go:801-823` (`daemonPresent` at `:802`, assigned `:814`), `readRegularUnitAt` `:1876-1899` | yes |
| `daemonPresent` is NOT passed to `resolveDaemonModes` — the function cannot distinguish "no unit" from "unit present, value unreadable" | `install.go:531` signature, callers `:867`, `:964` | yes |
| the mode is re-resolved BENEATH the install lock from `preResolve` (the caller's explicit-only options) and re-rendered if it differs | `install.go:945-977` | yes |
| the rendered unit line the default lands in | `internal/install/assets/aira-daemon.service.in:11` | yes |
| the daemon binary's own env default is `off` for an unset/empty `AIRA_DAEMON_SLICE_CEILING_MODE` | `internal/daemon/paths.go:142-153` | yes |
| the ci-shim daemon forces all three subsystem modes off in its own environment and never reaches `resolveDaemonModes` (`runInstall` returns at `:771-773`) | `internal/install/mode.go:300-312`, `install.go:767-773` | yes |
| a degenerate policy PARKS the subsystem (mode forced to `off`, refusal logged with the offending numbers) instead of freezing admission | `internal/daemon/server.go:502-521`, `sliceceiling.go:316-341` | yes |
| **`install.sh:9` runs `~/.local/bin/aira install` with NO flags — this is the real deploy path (see D1)** | `install.sh:1-9` | yes |
| **the live unit on this box declares `AIRA_DAEMON_SLICE_CEILING_MODE=observe` (line 11) and `AIRA_DAEMON_WATCHDOG_MODE=enforce` (line 9), mtime 2026-09-06 19:08** | `~/.config/systemd/user/aira-daemon.service`, read read-only | yes |

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
**§1.4's decision leans on this paragraph**: it is what bounds the consequence
of defaulting an unreadable unit to `enforce` to a longer wait rather than a
refusal.

---

## 1. Scope

### 1.1 In scope

**The `aira install`-time ship default for the slice-ceiling mode, and the
comments that describe it.** Concretely:

**Behaviour — three edits in `internal/install/install.go`:**

1. `install.go:42-46` — split the shared constant:
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
3. `install.go:562` — `opts.sliceCeiling = defaultSliceCeilingMode`, with the
   AIRA-106 comment block at `:554-560` **rewritten, not deleted**: it must keep
   AIRA-106's reasoning as recorded history and state the owner decision that
   reversed it, so the next reader sees a reversal rather than an absence.

**Comments and operator text that become FALSE with the flip — all four must
change in the same commit (§1.4 is why):**

4. `install.go:527-530`, `resolveDaemonModes`'s own doc comment: *"an installed
   value that is not a recognised mode is IGNORED rather than propagated or
   refused … and the ship default is the safe answer."* After this change the
   fall-through is to the **active** mode, not the conservative one. It must say
   so — that ignoring an unreadable value now lands on `enforce`, that §0.2
   bounds that to a longer wait, and that this is deliberate (§1.4) — instead of
   claiming a conservatism it no longer has.
5. `install.go:103-108`, `installedEnvironmentValue`'s doc comment: *"reads as
   absent, which falls back to the ship default rather than to a wrong value."*
   Same inversion, same fix. For the watchdog the sentence stays true; the
   comment must stop asserting it for both.
6. `internal/install/daemon_service_test.go:104-105`, the mirror of (4) in the
   existing test: *"the ship default is the safe answer."* Same rewrite.
7. `internal/core/core.go:1817`, the only surface that describes what an omitted
   flag does (verified: the string appears in exactly one file, no golden and no
   generated-doc copy):
   ```go
   stringSpec("slice_ceiling", false, false,
       "Dynamic slice-ceiling mode; omitted keeps the installed value, or enforce when no installed value is readable",
       "off", "observe", "enforce"),
   ```
   The wording is deliberately *"when no installed value is readable"*, not
   *"on a first install"*: under §1.4 the second is the false claim the gate
   caught in revision 1's own verdict paragraph. The watchdog's description at
   `:1815` is left alone — its default did not change.

**One comment under `internal/daemon/` (see I7):**

8. `internal/daemon/paths.go:138` — *"It ships observe-then-enforce, never on by
   default"* becomes a product-level misstatement once `aira install` defaults
   it to `enforce`. That clause is corrected; the sentence two lines later about
   the DAEMON's own env default staying `off` is true and stays. No code under
   `internal/daemon/` changes. (`sliceceiling.go:652`, `:829` and
   `confine_manage.go:196` refer to the rollout ladder conceptually and remain
   accurate; they are left alone.)

**The ticket, because it is the ledger:**

9. `.aira/tickets/AIRA-177.md:56-57` — *"A truly fresh install (no existing
   managed unit) is the only population that picks up the new default"* is
   false for the reason §1.4 gives. Amended in this plan's own commit to state
   I5's population, with a pointer to §1.4. The preceding requirement in that
   bullet — that an already-`observe` install re-run with no flag stays
   `observe` — is unchanged and is still pinned by T2b and T4.

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
| the watchdog's install default (`observe`) | `install.go:552` | its own constant; pinned by T1 and T3 in the **same assertion** as the slice ceiling, so a blanket flip cannot pass; and by the `want watchdog` column on every T2 row |
| the daemon binary's raw env default (`off`) for an unset variable | `daemon/paths.go:142-153` | not touched; `install` renders an explicit `Environment=` line, so the two are independent by construction. Pinned by the untouched `TestSliceCeilingEnvParsing` (`daemon/sliceceiling_test.go:827`) |
| every existing install **whose managed unit declares a mode `validDaemonMode` accepts**, on re-install with no flag | `install.go:537-541` | preservation runs before the default and is structurally unchanged; T2b/T2c/T4/T7. The units this qualifier excludes are §1.4's decision, not an oversight |
| `--slice-ceiling observe` / `off` as explicit opt-outs | `install.go:439-443`, `:537` | explicit flag still wins; T2d/T2e |
| the root re-exec not forging an omitted flag | `install.go:732-733` | untouched; `TestReexecDoesNotForwardUngivenDaemonModes` re-run unchanged |
| the ci-shim install | `mode.go:300-312`, `install.go:767-773` | never reaches `resolveDaemonModes`; forced `off` |
| the ceiling formula, damping, quantisation, basis, TTL hold | `daemon/sliceceiling.go` | no behaviour change under `internal/daemon/`; the only edit there is the I7 comment |
| the two application sites, and the terminal / scope-sizing sites that must stay on the raw maximum | `admit.go:2536`, `confine_manage.go:174`; `admitConnection`, `resolveAdmitReserve`, `resolveDelegateRAMScopeCeiling`, `evaluateWorkerAdmit` | no change; §0.2; pinned by the W4 tests named in §5.1 |
| any wire field, response shape, error code or log line | — | no change |
| this machine's already-installed mode | live box | deferral D1; this ticket ships code, it does not deploy |

### 1.4 DECISION — what a flagless install does when a managed unit exists but declares no readable slice-ceiling mode

This is the gap revision 1 asserted away, and it is the ticket's own
load-bearing non-regression, so it is decided here on the record rather than
inherited from the code's shape.

**The mechanism.** `resolveDaemonModes(opts, installedDaemonUnit)` takes the unit
*content*, never its *presence*. `daemonPresent` exists — `install.go:802`,
assigned at `:814` — but is not passed. `installedEnvironmentValue`
(`install.go:103-115`) returns `""` for **four** distinct inputs that the
function therefore cannot tell apart:

| input | reachable how |
| --- | --- |
| no managed unit at all | a genuinely fresh box |
| a managed unit with **no slice-ceiling line** | any unit rendered by a pre-AIRA-106 binary — verified: `git show ee6e214:internal/install/assets/aira-daemon.service.in` has watchdog lines only; `@SLICE_CEILING_MODE@` first appears in `7ecaf8a` |
| a line with an **unrecognised value** (`…=paranoid`) | permanent: any hand-edit, or a unit written by a newer binary with a wider vocabulary |
| a **multi-assignment or reset** `Environment=` line | permanent: `Environment=FOO=1 AIRA_DAEMON_SLICE_CEILING_MODE=observe` is not parsed; the existing test pins exactly this for the watchdog (`daemon_service_test.go:121`) |

All four fall through `!validDaemonMode` to the ship default at `:561-562` —
which this ticket makes `enforce`.

**The decision: accept it. All four land on `enforce`, deliberately, and it is
pinned by test.** Recorded reasoning:

- The consequence is bounded by §0.2 to a longer queue wait on a fair, visible,
  bounded queue — never a refusal, never a hard failure. The escalation is real
  but it cannot break a workload.
- The project's no-compat rule (`aira-not-live-no-compat`) applies squarely to
  the pre-AIRA-106-unit case: AIRA has no external installs, and the only box
  carrying a real unit carries a **valid** `observe` line (verified read-only,
  §0 table), so it takes the preservation path, not this one.
- For the two hand-edit cases the fall-through is arguably the *better* answer
  once `enforce` ships: an operator who wrote an unparseable line stated no
  readable preference, and the shipped posture is the honest thing to give them.
  Whatever they meant, `--slice-ceiling observe|off` is a one-command, durable
  opt-out (I3).
- The rejected alternative is option (i): thread `daemonPresent` into
  `resolveDaemonModes` as a third state and fall back to `observe` when a unit
  is present but unreadable. It is implementable — the signal exists — but it
  changes the function's signature to introduce a *third* resolution state for
  one subsystem while the watchdog keeps two, for a population that is empty on
  every real box and whose worst case is a longer wait. That is exactly the
  per-feature complexity stacking `architectural-simplicity` forbids: the rule
  there is "keep the primitive + document the gap". This section is that
  document, and T2f/g/h are the pins that stop it from being only prose.

**Consequences carried elsewhere in this plan:** the corrected verdict paragraph
above; I3's restatement; I5 unchanged (it was always right); the four comment
rewrites in §1.1 items 4–7, which today all assert a conservatism the fall-through
no longer has; the ticket amendment, §1.1 item 9; and T2 rows f, g, h.

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

**Three dated records state the old default and are deliberately left as
history**, per this project's convention that a dated ticket or design doc
records what was decided *then*: `.aira/tickets/AIRA-106.md:59`
("default `observe`"), and the AIRA-106 design doc
`docs/superpowers/specs/2026-09-06-aira106-two-parameter-slice-ceiling-design.md:411`
and `:529`. Named here so a future reader who greps for the default and finds
three sources saying `observe` can see that the divergence is intentional and
where the live answer lives (§1.1 items 3 and 7). The undated, currently-true
surfaces — the constant, `resolveDaemonModes`'s comment, the flag description,
the `paths.go` comment and this ticket — are all corrected in scope above.

---

## 3. Invariants

- **I1.** The watchdog's freshly-installed mode is `observe`, before and after,
  and is decided by an identifier no slice-ceiling code path reads.
- **I2.** `sliceCeilingModeFromEnv()` returns `off` for an unset or empty
  `AIRA_DAEMON_SLICE_CEILING_MODE`. Unchanged. A daemon started outside the
  installed unit — `aira daemon serve` by hand, the dispatcher's
  `/proc/self/exe daemon` fallback, every unit test — is byte-identical.
- **I3.** For every input where the installed managed unit declares a slice-ceiling
  mode `validDaemonMode` accepts and no `--slice-ceiling` flag was given, the
  rendered unit declares **the installed mode**, whatever the ship default is.
  *The mechanism is the `""` sentinel, not control flow:* `resolveDaemonModes`
  has **no early return**. `sliceCeilingGiven()` is exactly
  `o.sliceCeiling != ""` (`install.go:179`), so the preservation branch writing
  the installed mode into `opts.sliceCeiling` at `:539` makes the later
  `if !opts.sliceCeilingGiven()` at `:561` false, and the default assignment is
  skipped. That one sentinel therefore conflates *explicit flag* with
  *preserved value*, and — load-bearingly — *no unit* with *unit present but
  value unreadable*; the second conflation is §1.4's decision and is precisely
  what revision 1's "returns before `:561`" wording hid.
- **I4.** An explicit `--slice-ceiling <mode>` produces exactly `<mode>`, with or
  without an installed unit.
- **I5.** The population whose rendered byte changes is exactly: no
  `--slice-ceiling` flag ∧ no managed `aira-daemon.service` carrying a mode
  `validDaemonMode` accepts. Nothing else. This is wider than "a fresh box" by
  the three states §1.4 enumerates, and I5 — not the fresh-box phrasing — is the
  claim this plan makes and tests.
- **I6.** The pre-lock and under-lock resolutions remain the same function over
  the same explicit-only `preResolve` options, so the new default cannot make
  the two passes disagree and trigger a spurious re-render/restart.
- **I7.** No **behaviour** change under `internal/daemon/`: no function, gate,
  formula or default in that package is modified. The one edit there is the
  stale comment clause at `paths.go:138` (§1.1 item 8), which this change would
  otherwise make a product-level misstatement. Revision 1 stated I7 as "no file
  under `internal/daemon/` changes", which would have left that comment wrong.
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
load-bearing non-regression: guarded structurally by I3 and pinned by T2b, T4
and T7, and T4 asserts the *side effects* (no unit rewrite, no daemon restart),
not just the rendered string. Its exact boundary is I5, and the population on
the wrong side of that boundary is R8.

**R3 — an existing AIRA-106 test becomes half-porous.**
`TestInstallDaemonReinstallPreservesModes` (`daemon_service_test.go:213-245`)
seeds `sliceCeiling: "enforce"` and asserts `enforce` survives a flagless
re-install. Once `enforce` is the ship default, that half of the assertion
**passes whether preservation works or not** — a broken preservation would
default to the same value. The watchdog and interval halves still discriminate.
This is a real, newly-introduced hole in an existing load-bearing test, and the
mitigation is T4, which seeds `observe` — the one value that cannot be produced
by defaulting. The plan requires a comment at that test recording the loss, so
the porosity is written down rather than left for someone to rediscover.

**R7 — a SECOND existing AIRA-106 test becomes half-porous, same class as R3.**
(Numbered 7 because revision 2 found it; placed here, out of numeric order,
because it is R3's twin and the two must be read together.) Found by the
revision-1 gate, missed by revision 1.
`TestInstallDaemonConcurrentModeChangeSurvivesTheLock`
(`daemon_service_test.go:261-305`) seeds both modes `observe`, has a concurrent
writer swap the unit to `enforce` inside `d.flock`, and asserts the final unit
carries `AIRA_DAEMON_SLICE_CEILING_MODE=enforce`. Against W3 — the default
hoisted above preservation — a flagless install resolves `enforce` from the ship
default both pre-lock and under-lock, and that assertion **passes anyway**. The
slice-ceiling half stops discriminating W3 the moment the default flips. (It
still catches its own target defect, a missing under-lock re-resolve, because
that would yield `observe`; and the watchdog half is unaffected.) Mitigation is
T7, which inverts the slice-ceiling direction so both W3 *and* the original
defect go RED. Recorded rather than silently fixed, because "the change made an
existing load-bearing test stop discriminating" is the false-pass direction §7
tells the build reviewer to press hardest, and it has now happened twice.

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

**R8 — the §1.4 escalation is real, and is accepted rather than denied.** A
managed unit that declares no readable slice-ceiling mode is upgraded to
`enforce` by a flagless install. §1.4 records why that is the chosen answer, §0.2
bounds it to a longer wait, and T2f/g/h pin it so a future change cannot flip it
back without saying so. The honest statement of the residual risk is: on a box
whose unit was hand-edited into an unparseable state, a routine deploy changes
the admission posture. This plan asserts that is acceptable and visible (the
rendered unit says `enforce` in plain text, and `aira install --status` reports
the installed facet), not that it cannot happen.

---

## 5. Tests

TDD: T1's amendment, T2 and T7's amendment are written first and must be RED
against `origin/master` before the constant splits. The RED-first claim starts
from a **recorded green baseline**, re-run at the start of implementation and
recorded with its exit code, not assumed:

```sh
aira confine -- go test ./internal/install/ -run 'Install|ResolveDaemonModes|Parse|Reexec|Render' -count=1
aira confine -- go test ./internal/daemon/ -run 'SliceCeiling' -count=1
```

**T1 — `TestResolveDaemonModesPreservesInstalledModes` (amend, `internal/install/daemon_service_test.go:100-103`).**
The first-install case currently asserts `watchdog == "observe" && sliceCeiling == "observe"`.
Amend to `watchdog == "observe" && sliceCeiling == "enforce"`, keeping both in
**one** assertion with a message that says the two defaults are deliberately
different. Deliberately not a deletion: this line is the existing pin on the
ship defaults, and it must keep pinning them. The comment at `:104-105` is
rewritten per §1.1 item 6 in the same edit.

**T2 — new `TestResolveDaemonModesFreshInstallDefaultsSliceCeilingToEnforce`
(`internal/install/daemon_service_test.go`).** A table at the deciding seam,
`resolveDaemonModes`, covering the whole input space of §1.3 **and §1.4**.
Rows f, g and h are new in revision 2 and are the pins on the §1.4 decision;
nothing in revision 1 — not T2a–e, not T4, not the W3 row — discriminated there.

| case | flag | installed unit declares | want slice ceiling | want watchdog |
| --- | --- | --- | --- | --- |
| a — fresh install | absent | (no unit) | `enforce` | `observe` |
| b — **the critical non-regression** | absent | `observe` | `observe` | `observe` |
| c — an off box stays off | absent | `off` | `off` | `observe` |
| d — explicit opt-out on a fresh box | `observe` | (no unit) | `observe` | `observe` |
| e — explicit beats installed | `off` | `enforce` | `off` | `observe` |
| f — **§1.4: a pre-AIRA-106 unit, watchdog lines only, no slice-ceiling line** | absent | `AIRA_DAEMON_WATCHDOG_MODE=observe` and nothing else | `enforce` | `observe` |
| g — **§1.4: an unrecognised value** | absent | `AIRA_DAEMON_SLICE_CEILING_MODE=paranoid` | `enforce` | `observe` |
| h — **§1.4: a multi-assignment line**, mirroring the existing watchdog row at `:121` | absent | `Environment=FOO=1 AIRA_DAEMON_SLICE_CEILING_MODE=observe` | `enforce` | `observe` |

Filling `want watchdog` on every row (revision 1 left b–e blank) costs nothing
and widens W2's blast radius from T1/T3 to the whole table. Rows f, g and h each
carry a comment naming §1.4, so a future reader sees an accepted decision rather
than an accident — and a future change that prefers option (i) has to edit an
assertion and say why.

**T3 — new `TestInstallFreshDaemonUnitEnforcesSliceCeilingByDefault`
(`internal/install/daemon_service_test.go`).** End to end through
`newFakeInstall` + `runInstall(d, installOpts{memoryMax: "16G"})` against a
clean state, asserting the **rendered** `aira-daemon.service` contains both
`AIRA_DAEMON_SLICE_CEILING_MODE=enforce` and `AIRA_DAEMON_WATCHDOG_MODE=observe`.
T2 pins the decision; this pins that the decision reaches the file, through
`renderDaemonUnit`'s own `validDaemonMode` guard on both modes
(`install.go:1434` + `:1441`/`:1444`, calling `validDaemonMode` at `:484-486`)
and the under-lock re-resolve (`:963-977`, from the `preResolve` captured at
`:866` before the pre-lock resolve at `:867`).

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

**T7 — `TestInstallDaemonConcurrentModeChangeSurvivesTheLock` (amend,
`internal/install/daemon_service_test.go:261-305`), the fix for R7.** Invert the
slice-ceiling direction only:

- seed the first install with `sliceCeiling: "enforce"` (watchdog stays
  `observe`);
- the concurrent writer inside `d.flock` swaps
  `AIRA_DAEMON_SLICE_CEILING_MODE=enforce` → `observe` (and keeps the watchdog's
  `observe` → `enforce` swap unchanged);
- assert the final unit carries `AIRA_DAEMON_SLICE_CEILING_MODE=observe` and
  `AIRA_DAEMON_WATCHDOG_MODE=enforce`.

Both directions then discriminate: the test's own target defect — resolving only
before the lock — yields the pre-swap `enforce` and goes RED, and W3 — the
default hoisted above preservation — also yields `enforce` and goes RED. The
existing guards stay (`if concurrent == string(installed)` still trips if the
seed does not carry both values, and `if !swapped` still fails if the lock was
never taken). A comment records why the two halves now run in opposite
directions, so nobody "tidies" it back to symmetry. This test moves out of T6.

**T5 — re-run unchanged, as guards on the "absent" sentinel:**
`TestParseInstallWatchdogFlags` (`:63-67`: an omitted flag must still parse to
`""` for watchdog, slice ceiling and interval, or preservation can never fire)
and `TestReexecDoesNotForwardUngivenDaemonModes` (`:136-151`: the
`sudo aira install` path must still not forge `--slice-ceiling`
(`install.go:732-733`), or every sudo install would turn "absent" into an
explicit `enforce` and defeat I3).

**T6 — re-run unchanged, as the I1/I2 guards:**
`TestInstallDaemonIdempotentAndWatchdogChangeRestarts`,
`TestRenderDaemonUnitIsManagedBoundedAndIndependent` (install side) and
`TestSliceCeilingEnvParsing` (`internal/daemon/sliceceiling_test.go:827-831` —
sets the variable to `""` and asserts `sliceCeilingOff`, the daemon's own
default).

### 5.1 Non-porosity — each new/edited test run against a wrong implementation

Per `docs/adversarial-verification.md`, a test that cannot go RED proves
nothing. Required evidence, recorded in the build report as a table:

| wrong implementation | must go RED |
| --- | --- |
| W1: no change at all (default stays `observe`) | T1, T2a, T2f, T2g, T2h, T3 |
| W2: blanket flip of the shared constant (watchdog also `enforce`) | T1, T3, every T2 row via its `want watchdog` column |
| W3: the default applied BEFORE preservation (assignment hoisted above the installed-unit read, or pre-filled in `parseInstallArgs`) | T2b, T2c, T4, T5, **T7** |
| W4: `sliceCeilingEffectiveMaximum` gated on `Mode != off` instead of `== enforce` (i.e. observe silently enforcing) | `TestSliceCeilingModeGating`, `TestSliceCeilingIsKeyedByCanonicalSlicePath`, `TestSliceCeilingThrottleReachesCapacityOnly` |

Coverage accounted honestly rather than left implicit: **T2d and T2e appear in
no W row.** They pin I4 (an explicit flag wins, with and without an installed
unit) and are not discriminators for W1–W4 — no wrong implementation in this
table can break them, because every one of them leaves the explicit-flag branch
alone. They are kept because I4 is a stated invariant, not because they add
non-porosity evidence, and the build report should say so rather than count them.

**W4's three named discriminators**, verified to exist and to discriminate — a
"re-run the existing tests" row without names would be vacuous:

- `TestSliceCeilingModeGating` (`internal/daemon/sliceceiling_test.go:363-388`)
  publishes a THROTTLED snapshot with `Mode = sliceCeilingObserve` and asserts
  `admitEffectiveMaximum("/slice", 64<<30) == 64<<30`. The fixture's throttled
  ceiling is below 64 GiB, so a `Mode != off` gate returns the sub-64G ceiling
  and the assertion fails. This is the direct W4 discriminator.
- `TestSliceCeilingIsKeyedByCanonicalSlicePath` (`:392-406`) pins the positive
  direction: `Mode = sliceCeilingEnforce` with a throttled 8 G ceiling yields 8 G.
- `TestSliceCeilingThrottleReachesCapacityOnly` (`:911`) pins §0.2 end to end: a
  16 G reserve under a 4 G enforce ceiling **waits** (no immediate frame) and is
  granted once the ceiling lifts — i.e. the throttle reaches capacity, not the
  terminal sites, which is the premise this ticket's safety argument rests on.

This ticket changes no line these three cover; they are named so the build
report states *which* tests carry the premise rather than gesturing at a package.

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

Note that none of these touch `~/.config/systemd/user`: `newFakeInstall`
(`internal/install/install_test.go:330-353`) uses `t.TempDir()` as `HOME`. The
test suite therefore cannot change this machine's installed mode, which is what
makes D1 a genuine deferral rather than an accident of timing.

---

## 6. Deferrals

- **D1 — flipping THIS machine's installed slice-ceiling mode from `observe` to
  `enforce`.** Explicitly excluded by the ticket: the owner asked to wait until
  the slice is quieter, or to have it monitored, given live admitted jobs on a
  shared box. It is one command (`aira install --slice-ceiling enforce`) and no
  rebuild, and it is a live-machine operational decision tracked separately.

  **This deferral is only real if the deploy path preserves.** Revision 1 left
  that assumed; revision 2 makes it checkable, because the deploy path is not a
  hypothetical: `install.sh:9` runs `~/.local/bin/aira install` **with no
  flags**, so a post-merge deploy of this very change re-runs the install that
  would apply the new default.

  1. What keeps this box on `observe` through it is preservation (I3), and
     nothing else — the live unit
     `~/.config/systemd/user/aira-daemon.service` carries a valid
     `Environment=AIRA_DAEMON_SLICE_CEILING_MODE=observe` on line 11 (verified
     read-only at plan time, mtime 2026-09-06 19:08; the sole drop-in
     `aira-daemon.service.d/sched-mode.conf` sets only `AIRA_SCHED_MODE=enforce`
     and does not touch the ceiling).
  2. **Precondition, re-verified read-only in the build/deploy report
     immediately before running `install.sh`:** that line is still present and
     still a value `validDaemonMode` accepts. If it is not — §1.4 — the flagless
     deploy will land on `enforce`, which is D1 happening by accident rather
     than by the owner's decision. In that case stop and raise it, do not
     proceed.
  3. **The deploy must not pass `--slice-ceiling`** in any form. Passing it —
     even `--slice-ceiling observe` — would forge the explicit flag and hide
     whether preservation actually works on the real path.
  4. After the deploy, re-read the unit read-only and record the line it carries.
     `observe` is the pass; `enforce` means preservation did not hold on the real
     path and is a P0 finding for this ticket, not a successful D1.

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
- **D6 — option (i): teaching `resolveDaemonModes` to distinguish "no unit" from
  "unit present, value unreadable".** Rejected for this ticket in §1.4 on
  `architectural-simplicity` grounds, with the decision pinned by T2f/g/h rather
  than left implicit. Recorded as a deferral rather than a closed door: if the
  unreadable-unit population ever stops being empty on real boxes, the signal
  (`daemonPresent`, `install.go:802`/`:814`) is already there and the change is
  one parameter plus a third state.

---

## 7. Review

Two-loop per `CLAUDE.md`: this plan to an orthogonal plan-review lineage and a
plan gate before implementation; adversarial build review examining both
false-fail and false-pass directions before merge, with every confirmed
counterexample becoming a regression test. The false-pass direction to press
hardest is R3 **and R7**: this change makes *two* existing preservation tests
stop discriminating, one of which revision 1 missed entirely. A reviewer should
check that T4 and T7 actually replace what was lost rather than merely
resembling it, and should independently re-derive §1.4's four-input claim from
`installedEnvironmentValue` rather than take it from this document — that claim
is the one revision 1 got wrong.
