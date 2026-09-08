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
  retroactive. A truly fresh install (no existing managed unit) is the only
  population that picks up the new default.
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
