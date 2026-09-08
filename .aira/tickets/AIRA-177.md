---
{"schema":1,"id":"AIRA-177","project":"aira","title":"aira install should default the slice-ceiling to enforce, not observe","status":"planned","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","install"],"hold":false,"relations":[]}
---

Owner decision (2026-09-08): "slice ceiling should be enforced by default (and
admission gated on available capacity), otherwise what's the point of it?"

This resolves a question AIRA-106 explicitly left open rather than defaulted:
"The observe→enforce rollout sequencing (how long to run in observe before
flipping to enforce, and who decides that) — flag for the owner rather than
default." That flag is being answered now.

## Why observe is the current default (record the prior reasoning, don't
## silently overwrite it)

`internal/install/install.go:44-46` — `defaultDaemonSubsystemMode =
"observe"`, shared today by both the watchdog and the slice-ceiling install
defaults. The comment at the slice-ceiling call site (`:552-561`) is explicit
about why: "observe applies nothing to admission (it samples, publishes and
reports the ceiling it WOULD apply)... while `enforce` -- a real capacity
reduction on this box -- stays an explicit operator decision." That was a
deliberate AIRA-106 choice, not an oversight, made so a first install could
never silently change what the machine admits.

## Why it changes now

Live evidence, gathered today: on the owner's own box, live confine scope
CAPS summed to ~81.6 GiB against 78 GiB total RAM (an `--allow-overcommit`
configuration with a 128G aggregate bound), while the slice-ceiling throttle
that would have shrunk the effective admission ceiling to ~52.8 GiB based on
real host-wide `MemAvailable` was sitting in `observe` -- computing the
number, not applying it. A peer session ("wt") reported real OOMs the same
day. An admission gate that does not gate admission by default provides
diagnostic value only, not the protection it exists to provide; the owner's
own framing is the ticket title.

## Scope

**Only the `aira install`-time DEFAULT for slice-ceiling.** Verified from
source, must NOT change:

- The watchdog's own default stays `observe` (`defaultDaemonSubsystemMode`
  is currently shared by both subsystems' defaults — this ticket needs a
  new, separate constant for slice-ceiling specifically, not a blanket flip
  of the shared one).
- The DAEMON binary's own raw env-var default (when started outside the
  installed systemd unit, and in every test) stays `off`, per the existing
  comment at `install.go:558-559` -- only `aira install`'s freshly-rendered
  unit picks a different default.
- **Existing installs must not be silently upgraded.** `install.go:538`
  already reads back a currently-installed mode from the managed unit and
  preserves it across a re-install with no explicit flag
  (`installedSliceCeilingModeRE`) -- verify this property explicitly with a
  test (an already-`observe` install, re-run with no `--slice-ceiling` flag,
  must stay `observe`) so this ticket cannot accidentally make the change
  retroactive.

  **Corrected 2026-09-08 by the AIRA-177 plan gate (plan revision 2, §1.4):**
  this bullet originally read "A truly fresh install (no existing managed
  unit) is the only population that picks up the new default." That is FALSE.
  `resolveDaemonModes` takes the installed unit's *content*, never its
  presence, and `installedEnvironmentValue` (`install.go:103-115`) returns
  `""` — indistinguishable from "no unit" — for an absent slice-ceiling line
  (every unit rendered by a pre-AIRA-106 binary), an unrecognised value, and a
  multi-assignment or reset `Environment=` line. All of those fall through to
  the ship default too. The population that picks up the new default is
  therefore: **no `--slice-ceiling` flag AND no managed `aira-daemon.service`
  carrying a mode `validDaemonMode` accepts.** The plan decides that case
  deliberately (accept the fall-through to `enforce`; §0.2 bounds the
  consequence to a longer admission wait, never a refusal) and pins it with
  tests; the preservation requirement above is unchanged and still required.
- `--slice-ceiling observe`/`off` remain fully supported, explicit opt-outs
  for anyone who wants the old behaviour.

## Not done by this ticket, deliberately

Flipping THIS machine's already-installed slice-ceiling mode from `observe`
to `enforce` is explicitly NOT part of this ticket -- the owner asked to wait
until the slice is quieter (or have it monitored) before applying that here,
given live admitted jobs on a shared box right now. That is a live-machine
operational decision, tracked separately from the code-level default change
this ticket makes.

## Suggested verification

- Verify from source (not assumed) that `enforce` mode genuinely gates
  admission on the computed ceiling -- i.e. `sliceCeilingDesired`'s dynamic
  figure actually becomes the effective admission ceiling `admitConnection`
  enforces in `enforce` mode, not merely a log line, before this ticket's
  premise is taken as given.
- A real-cgroup or unit test exercising: fresh install (no existing unit) +
  no `--slice-ceiling` flag → `enforce`; existing `observe` install +
  re-run with no flag → stays `observe`; explicit `--slice-ceiling observe`
  on a fresh install → `observe` (opt-out still works).

## Resolution (2026-09-08 — built; PR open, NOT merged, NOT deployed)

The frontmatter `status` is deliberately left at `planned`: this branch is not
merged, and `planned` → `in-review` is not a legal single transition
(`domain.ValidateTransition`), so the status is moved by whoever merges rather
than forged here.

Built to plan revision 2
(`docs/superpowers/plans/2026-09-08-aira177-slice-ceiling-enforce-default-plan.md`),
which was plan-gate approved. Branch `aira177-slice-ceiling-enforce-default`.
**Not merged by the builder; not deployed to this box (D1 stands).**

### What was built

One constant split into two, in `internal/install/install.go`:

- `defaultWatchdogMode = "observe"` (the rename of `defaultDaemonSubsystemMode`
  — mechanical, three occurrences, no test or cross-package reference; the old
  name asserted "every daemon subsystem defaults to this", which stopped being
  true here and would have silently handed its default to the next subsystem
  that grows an install flag);
- `defaultSliceCeilingMode = "enforce"` (new).

`resolveDaemonModes` now reads one constant per subsystem. **Nothing else in the
resolution changed**: the explicit flag still wins, preservation still runs
BEFORE the ship default, and the daemon binary's own env default for an unset
`AIRA_DAEMON_SLICE_CEILING_MODE` is still `off`.

The premise the ticket asked to be *verified rather than assumed* was verified by
reading the chain, and one wording correction is recorded: the throttled ceiling
is applied at `evaluateAdmitQueue` (`admit.go:2536` → `checkedAvailable` and
`oversubscriptionLimit`), **not** in `admitConnection`. That placement is
deliberate and unchanged — `admitConnection`'s terminal `E_ADMIT_TOO_LARGE`,
`resolveAdmitReserve`'s clamp and `resolveDelegateRAMScopeCeiling` all keep
reading the raw configured maximum. The consequence matters for this ticket:
defaulting to `enforce` **cannot convert any workload into a hard failure**; its
worst case is a longer wait on the existing bounded, fair, visible queue.

Four comments that became false with the flip were rewritten rather than left:
`resolveDaemonModes`'s doc comment and `installedEnvironmentValue`'s (both
asserted the fall-through was conservative, which is now true only for the
watchdog), the mirror of the first in `daemon_service_test.go`, and
`internal/daemon/paths.go:138`'s "never on by default" clause — which is now a
product-level misstatement, though what that function itself decides (the raw
env default, `off`) is unchanged. `internal/core/core.go`'s `--slice-ceiling`
description now reads "omitted keeps the installed value, or enforce when no
installed value is readable". **No behaviour changes under `internal/daemon/`.**

### The population that actually changes, decided rather than inherited

Per §1.4 of the plan and the correction already recorded in this ticket's Scope
section: `resolveDaemonModes` takes the installed unit's *content*, never its
presence, so a managed unit with no slice-ceiling line, an unrecognised value, or
a multi-assignment/reset `Environment=` line all reach the ship default too. All
of them are now `enforce`, **deliberately** — bounded to a longer wait by the
placement above, opt-out-able with one durable command — and pinned by rows f, g
and h of `TestResolveDaemonModesFreshInstallDefaultsSliceCeilingToEnforce` so a
future change has to edit an assertion and say why. The rejected alternative
(thread `daemonPresent` through as a third resolution state) is deferral D6.

### Tests, written first and RED before the constant split

- `TestResolveDaemonModesPreservesInstalledModes` — amended: the ship-default
  assertion now pins `watchdog == observe && sliceCeiling == enforce` in ONE
  assertion, so a blanket flip in either direction cannot pass.
- `TestResolveDaemonModesFreshInstallDefaultsSliceCeilingToEnforce` — new; an
  8-row table over the whole input space, `want watchdog` filled on every row.
- `TestInstallFreshDaemonUnitEnforcesSliceCeilingByDefault` — new; end to end
  through `runInstall`, asserting the rendered unit.
- `TestInstallReinstallDoesNotUpgradeObserveSliceCeiling` — new; the
  retroactive-flip non-regression, seeding `observe` (the one value defaulting
  cannot manufacture) and asserting the side effects too: no unit rewrite, no
  daemon restart.
- `TestInstallDaemonConcurrentModeChangeSurvivesTheLock` — amended; its
  slice-ceiling direction is inverted to `enforce`→`observe`, because the new
  default made its old direction stop discriminating.

**Two recorded coverage losses, not silent.** This change made two existing
AIRA-106 preservation tests stop discriminating in their slice-ceiling half,
because they seed `enforce` — now also the default. The concurrent-lock test was
repaired by inverting it; `TestInstallDaemonReinstallPreservesModes` was left
seeded as `enforce` with a comment recording the loss and naming its replacement,
so the gap stays written down rather than being rediscovered.

Non-porosity evidence (each wrong implementation actually run, not asserted):
W1 no-change → `TestResolveDaemonModesPreservesInstalledModes`, T2 rows a/f/g/h
and the fresh-install render test go RED. W2 blanket flip → the same two plus
seven of the eight T2 rows via the watchdog column (row f is the honest
exception: its installed unit declares a watchdog mode, so preservation supplies
it and the default is never reached). W3 default hoisted above preservation →
T2b, T2c, the reinstall test and the concurrent-lock test; the parse-time variant
of W3 → `TestParseInstallWatchdogFlags`. W4 observe silently enforcing →
`TestSliceCeilingModeGating` (the other two tests named in the plan for that row
pin the positive direction and the wait-not-refuse premise; they do not
discriminate W4, and are not counted as if they did).

### Not done

D1 — flipping **this** machine's installed mode — is untouched, as the ticket
requires. The live unit was read read-only only: it still declares
`AIRA_DAEMON_SLICE_CEILING_MODE=observe` at mtime 2026-09-06 19:08, so it takes
the preservation path and a flagless `install.sh` deploy of this change leaves it
on `observe`. That precondition must be re-verified read-only immediately before
any deploy; if the line is ever absent or unparseable, a flagless deploy would
land on `enforce` — D1 by accident rather than by decision — and the deploy
should stop there instead.
