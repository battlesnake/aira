# `aira install` — AIRA-owned confinement slice (aira.slice)

> **ACTIVE — building (owner, 2026-08-24).** Owner reactivated #55 with a
> **concurrent-first, then-redirect** migration (superseding the earlier "aira.slice
> *replaces* whale.slice" deferral): install `aira.slice` as an **independent
> top-level sibling** to `whale.slice` and **run both concurrently** for a
> time-scoped interim (owner explicitly accepts the two-cap overcommit / OOM risk),
> then later **redirect `agentmux whale` (whale-run) into `aira confine`** and retire
> whale.slice, leaving `aira.slice` the sole pool. This is exactly the sibling shape
> §3/§4 already describe, so this plan's overcommit/`--allow-overcommit` machinery and
> the aira→whale fallback are **kept, not obsolete** — the concurrent interim IS the
> intended state, with `--allow-overcommit` its owner-accepted opt-in. **Milestone
> scope (build + throwaway-slice tests only; NO live-host mutation):** `aira install`
> + `aira.slice` + the delegation anchor + confine default→aira.slice. **Explicit
> sequels, NOT built here (§9):** the agentmux `whale-run → aira confine` redirect (a
> change in the agentmux repo, hook `whale.go:80-89`/`:117-118`), and actually
> running the live `aira install` on this box. §0's agentmux claims below were
> re-verified against `/home/user/claude/claude` by code-reading (2026-08-24) — all
> grounded, two nuances folded in (see §0).

Status: PLAN v4 — re-cut 2026-08-24 for the owner's concurrent-first/then-redirect
migration (the DEFERRED banner's replacement-model framing is withdrawn; the sibling
§3/§4 machinery stands). Carries forward the v3 Fable code-grounded GATE-PASS basis:
folds Sol plan-review r1+r2, Gemini r1, Fable's 5 must-fix items, and the empirical
systemd-255 findings (`Delegate=` on a `[Slice]` ignored; manual subtree_control
delegation wiped by daemon-reload; `daemon-reload` alone applies a changed slice
`MemoryMax`), and the owner's directive to mirror `agentmux install`'s
baked-into-binary pattern (§0 re-verified against live agentmux code 2026-08-24).
**Re-gate required** for the v4 framing before build. Milestone #55. Successor to
`aira confine` ([2026-08-21](2026-08-21-aira-whale-confinement-face-design.md),
merged `e9e83c4`).

Owner selected this thread (2026-08-22) — the whale endgame: AIRA installs and owns
its own capped systemd **user** slice, mirroring how `agentmux install` bakes the
`whale.slice` unit into the binary and materialises it with no separately-shipped
files.

## 0. Blueprint from agentmux (verified by code-reading `~/tools`)

`agentmux install` already owns `whale.slice` end to end; AIRA mirrors the proven
parts and drops the rest:

- **Baked-in unit** — one `//go:embed assets` FS + a named-constant `assetBytes()`
  accessor; the slice is a template `assets/whale.slice.in` with `@MEMMAX@`/
  `@MEMHIGH@` placeholders substituted by `strings.ReplaceAll` (not `text/template`).
- **Sizing** — `--memory-max` > value already on disk > **⅔ of `/proc/meminfo`
  MemTotal**; `MemoryHigh = MemoryMax − 2G`; validated `^[0-9]+G$`, floor 4G.
- **Seam-injected installer** — every os/exec side effect is a field on an
  `installDeps` struct, so the whole flow is unit-testable offline.
- **Idempotence by content-comparison** — on-disk bytes vs the rendered asset;
  equal → "up to date", differ → rewrite. No version string.
- **Delegation** — agentmux does NOT poke `subtree_control`; it (a) *reads*
  `user@.service/cgroup.controllers` to detect the `memory` controller, and (b) if
  absent, writes a **root** drop-in `/etc/systemd/system/user@.service.d/…=Delegate=yes`;
  non-root without it warns "cap NOT enforced". whale.slice's own subtree_control is
  populated durably only because `whale-run` uses `systemd-run --scope` (systemd-
  managed accounted children).

**Re-verified against live agentmux code 2026-08-24 (`/home/user/claude/claude/cmd/agentmux/`).**
All five claims above grounded: embed `//go:embed assets`→`assetsFS` (`assets.go:10-11`),
template `whale.slice.in` with `@MEMHIGH@`/`@MEMMAX@` via `strings.ReplaceAll`
(`install.go:570-573`); sizing `computeMemoryMax` (`install.go:504-519`) — flag > on-disk
`parseInstalledMemoryMax` > `⅔·MemTotal`, high=max−2G, regex `^[0-9]+G$` (`install.go:479`),
floor 4G (`validateMemoryMax:559-561`); seam struct **`installDeps`** (`install.go:605-633`);
delegation detect `memoryControllerDelegated` (`install.go:1469-1480`) + root drop-in
`/etc/systemd/system/user@.service.d/10-agentmux-delegate.conf` = `[Service]\nDelegate=yes`
(no `subtree_control` write anywhere); whale-run launch `systemd-run --user --quiet --scope
--unit=whale-run-<name>.scope --slice=whale.slice --property=OOMPolicy=kill --` (`whale.go:80-89`).
**Two nuances folded in:** (i) agentmux's `whale.slice.in` has **no `MemoryAccounting=`** line
(MemoryMax implies accounting) — AIRA keeps `MemoryAccounting=yes` explicit on both units
because the anchor service depends on accounting; harmless. (ii) agentmux writes the slice
**unconditionally** (deterministic render → byte-identical) and content-compares only its
/etc drop-ins (`bytes.Equal`); **AIRA content-compares the slice too** before writing, so an
unchanged cap triggers no needless `daemon-reload` and `--status` can honestly report "up to
date" — a deliberate improvement, not a divergence in behaviour.

AIRA copies the embed/render/size/idempotence/seam pattern verbatim in spirit.
**In scope, deliberately narrower than agentmux:** ONLY the confinement slice +
its delegation + `aira install --status`. **Out:** tmux/statusline/CLAUDE.md
wiring, the watchdog + oomd units, binary/daemon install, and any `uninstall`
(agentmux ships none — neither do we yet).

## 1. The delegation problem confine creates — and the fix (empirically verified)

Measured on the target host (systemd 255), a throwaway slice:

- `Delegate=` on a `[Slice]` → **"setting not supported for this unit type,
  ignoring"**. So it is NOT put on the unit (Gemini r1 was wrong here; Fable's
  earlier confine-spec note was right).
- With `MemoryMax=` set, the slice's `cgroup.controllers` is `memory pids`
  (available) but its `cgroup.subtree_control` is **empty**. Writing `+memory` to
  subtree_control works and a direct-mkdir child then gets `memory.max` +
  `memory.oom.group` — **but a `systemctl --user daemon-reload` RESETS
  subtree_control to empty and the running child's `memory.max` VANISHES.**
- A **systemd-managed** memory-accounted child (a `systemd-run --scope`) keeps
  `subtree_control = memory pids` **across daemon-reload**. That is exactly why
  whale.slice's delegation survives while idle.

Confine (#54, merged) creates its scope by **direct mkdir + CLONE_INTO_CGROUP**
(for atomic placement + the two-way gate) — it is not systemd-managed, so a
self-owned aira.slice would lose its delegation on the next reload, silently
stripping the cap off a *running* confined job. Two mitigations, both included:

1. **Durable anchor (install-time), MANDATORY + supervised (Sol r2).** `aira
   install` installs a tiny persistent systemd-managed unit
   `aira-slice-keepalive.service` (`Slice=aira.slice`, `MemoryAccounting=yes`,
   `Restart=always`, `ExecStart=<abs-binary> __slice-anchor`), so systemd keeps
   `memory` durably enabled in aira.slice's subtree_control across every reload.
   `__slice-anchor` is a hidden `aira` verb that blocks on a signal (no `/bin/sleep`
   dependency; cgo-free). The anchor is **not optional**: `install` starts it and
   verifies it is active + delegation is present, failing (`E_INSTALL_UNAVAILABLE`)
   if it cannot; `Restart=always` keeps it continuously alive. Because the anchor
   holds `memory` in subtree_control **continuously**, the residual
   verify→`CLONE_INTO_CGROUP` micro-race in mitigation 2 is **benign** — delegation
   never drops while the anchor lives, so a reload in that window cannot strip a
   just-placed child. (A later consolidation may fold this role into the AIRA daemon
   running inside aira.slice.) Negligible idle cost.
2. **Runtime invariant (confine, per Sol).** Treat delegation as a runtime fact,
   not an install-time one: before creating a scope, confine ensures `+memory` on
   the parent's subtree_control if `memory` is in `cgroup.controllers`; then reads
   `subtree_control` back AND relies on the fact that the freshly-created child must
   expose `memory.max` + `memory.oom.group` (the merged `writeConfineOOMGroup`
   already Openat's `memory.oom.group` on the scope FD → ENOENT → `confineUnavailable`
   when delegation is stripped), so a delegation gap **fails closed**
   (`E_CONFINE_UNAVAILABLE`; extends the existing contract, no change to the two-way
   gate). **Scope of this protection (Fable):** the runtime invariant closes only
   the **launch-time** window — a `daemon-reload` *between* confine runs. It does
   NOT protect a job already running: a reload *during* a live confined job strips
   `memory.max` off the running child and nothing re-checks it. **The anchor
   (mitigation 1) is the SOLE mid-run protection** — this is why it is included, not
   optional. Wiring: `ensureDelegation(parent)` is a new injected `confineDeps`
   step invoked after `resolveSlicePath` and before `backend.Create` — NOT inside
   `Probe`/`ensureParent`, which is shared with `run` and cannot detect memory
   delegation (its mkdir+`cgroup.kill` probe succeeds without `+memory`).

**User-manager delegation** (memory in `user@.service`): mirror agentmux's
DETECTION only — read `…/user@<uid>.service/cgroup.controllers`. Present → proceed
(the dev host is already delegated by agentmux). Absent → **fail with a clear,
actionable `E_INSTALL_DELEGATION`** ("memory controller not delegated to your user
manager; enable it — e.g. run `agentmux install`, or add a `Delegate=yes` drop-in
on user@.service — and re-login, then re-run"). **The root `/etc` delegation
drop-in is DEFERRED (Sol r2):** `systemctl --user` run as root targets root's own
manager, not the invoking user, and a system drop-in cannot make delegation
available to the *already-running* user session (re-login required); a correct
privilege split (agentmux does a sudo re-exec as the target user, selecting its
UID/home/`$XDG_RUNTIME_DIR`) is more than v1 needs. **v1 is user-scope only** — it
detects delegation and refuses honestly when absent, never a misleading half-root
path.

Only `+memory` is needed (confine writes `memory.oom.group`/reads `memory.max`);
confine sets no `pids.*`. **We do NOT enable `+pids`** (Sol r1: `TasksMax=infinity`
does not guarantee the pids controller; do not overclaim). `TasksMax=infinity`
stays on the slice unit (systemd applies it directly; no subtree write needed).

## 2. The embedded unit

`assets/aira.slice.in`, rendered by `strings.ReplaceAll`:

```ini
[Unit]
Description=AIRA confinement slice — bounds memory-heavy jobs run via `aira confine`.
# Managed by `aira install` (baked into the aira binary). Do not edit by hand;
# re-run `aira install --memory-max=…` to change the cap.

[Slice]
MemoryAccounting=yes
MemoryHigh=@MEMHIGH@
MemoryMax=@MEMMAX@
MemorySwapMax=2G
CPUWeight=50
IOWeight=50
TasksMax=infinity
```

Per-scope `oom_score_adj`/oom.group are applied by confine (invalid on `[Slice]`).
Sizing precedence + validation mirror agentmux exactly (`⅔ RAM`, `−2G` high,
`^[0-9]+G$`, floor 4G). `--memory-high` may override the derived high mark
(validated `high ≤ max`).

## 2a. The anchor unit (durable delegation)

`assets/aira-slice-keepalive.service.in`, rendered with the **absolute** binary
path from `os.Executable()` at install time (Fable — a bare `aira`/`sleep` name
would break under a moved/rebuilt binary):

```ini
[Unit]
Description=AIRA slice delegation anchor — keeps memory delegated to aira.slice.
# aira-managed: aira-slice-keepalive.service

[Service]
Slice=aira.slice
MemoryAccounting=yes
ExecStart=@AIRABIN@ __slice-anchor
Restart=always
```

`@AIRABIN@` → `os.Executable()` at install. `aira __slice-anchor` is a hidden verb
(argv[0] pre-dispatch interception, like `__confine-setup`) that blocks on SIGTERM
and does nothing else — a near-zero idle process whose only job is to be a
systemd-managed memory-accounted member so systemd keeps `memory` durably in
aira.slice's `subtree_control` across `daemon-reload`. **Binary-staleness hazard
(Fable):** if the aira binary is later moved/rebuilt elsewhere, the anchor's
`ExecStart` path can go stale and the anchor dies on the next boot — silently
reopening the mid-run reload window. `aira install --status` MUST therefore surface
"anchor: active" vs "anchor points at a missing binary (re-run aira install)", and
re-running `install` re-renders the current path. (A later consolidation folds the
anchor role into the AIRA daemon if it comes to run inside aira.slice.)

## 2b. AIRA-managed marker + safe file mutation (Sol r1)

Every file AIRA writes carries a first-line marker comment
(`# aira-managed: <unit>`); `aira install` and any future removal act ONLY on files
carrying it (never clobber a hand-written or foreign unit of the same name — it
errors instead). `mkdir -p ~/.config/systemd/user` first (may not exist — Gemini
r1). **Hardened mutation (Sol r2):** operate directory-fd-relative — `openat` the
unit dir once, validate it is a real directory owned by the invoking user, and do
all reads/writes relative to that fd with `O_NOFOLLOW` (no `lstat`-then-`rename`
TOCTOU, no symlink follow). Write to a temp file in the same dir, `fsync`, then
`renameat` over the target atomically; on any error unlink the temp (no partial
leftovers). Reject a pre-existing target that is a symlink / non-regular / not
user-owned / lacks the marker. Serialise concurrent `install` runs with an
advisory `flock` on a lockfile in the unit dir so two installs cannot interleave
their reload/write.

## 3. Coexistence with whale.slice — the owner-accepted concurrent interim (Sol r2)

**This concurrent state IS the intended v4 interim** (owner, 2026-08-24): aira.slice
and whale.slice run side by side, `whale-run` jobs on whale.slice and `aira confine`
jobs on aira.slice, until the agentmux redirect (§9) retires whale.slice. The owner
has **explicitly accepted the time-scoped overcommit / OOM risk** of two live caps —
so `--allow-overcommit` is the expected install path here, not a rare edge.

aira.slice is an **independent top-level** user slice (the "self-owned" shape the
owner asked for, mirroring whale.slice's own independence and matching the
"similar-approach-to-agentmux" directive). Two independently-capped slices can sum
past the VM's RAM, so saturating BOTH `whale-run` and `aira confine` at once could
overcommit and OOM the host. A warning does not *prevent* that (Sol r2: acceptable
only as an explicitly-accepted product risk, not a safety resolution). So even under
the owner's blanket acceptance the risk stays a **tool-enforced, explicit opt-in** —
a fresh operator (or a future automated caller) never creates a silent overcommit:

- **`aira install` detects a capped `whale.slice` and, by default, REFUSES** to
  create a second overcommitting cap — exiting `E_INSTALL_OVERCOMMIT` with a clear
  explanation (the two caps sum past physical RAM; the safe end-state is the
  whale-run→confine migration) and the exact opt-in flag to proceed.
- **`aira install --allow-overcommit`** is the explicit acknowledgement that
  proceeds anyway, recording the acceptance in the install output/status. This is
  the tool-enforced form of "owner-approved product risk" Sol r2 requires — no
  silent overcommit is ever created.
- With no capped whale.slice present, `install` proceeds normally (aira.slice is
  then the only cap — no overcommit possible).
- **Sizing (interim, this host).** The derive-if-unspecified default is `⅔·MemTotal`
  (mirrors agentmux). Note whale.slice is a hardcoded 64G — that is `⅔` of the **96G
  physical** box, but inside the **80G WSL VM** `/proc/meminfo` MemTotal ≈ 80G, so the
  auto-derived default would be ≈53G, NOT 64G. **Recommended interim install:**
  `aira install --memory-max=64G --allow-overcommit`, so aira.slice matches whale's
  pool and the eventual single-pool end-state (whale retired → aira.slice the sole 64G
  pool) needs no re-tuning. `--status` always shows the coexistence + opt-in state.
- **`run` is unaffected (Fable):** the `run` verb's admission slice comes from
  project config (`run.slice`), not confine's default; §4 changes only `confine`.
  Existing configs pointing `run.slice` at whale.slice keep working unchanged.

The fully-safe resolutions (nesting aira.slice under whale.slice for one shared
ceiling, or migrating whale-run to retire whale.slice) remain the recorded end-
state (§9) — but v1 no longer *silently* permits overcommit: it is refused unless
explicitly accepted, which discharges Sol r1's nesting/migration objection for the
interim.

## 4. confine default → aira.slice, strict fallback (Sol r1)

Resolution precedence: `--slice` > `$AIRA_CONFINE_SLICE` > `aira.slice` (if it
resolves to a *valid* AIRA-owned, active, capped, delegated slice) > `whale.slice`
(if valid) > `E_CONFINE_UNAVAILABLE: aira.slice not found (run 'aira install')`.

- Fallback to whale.slice happens ONLY on *definite absence* of aira.slice. If
  aira.slice exists but is inactive / uncapped / undelegated / permission-denied,
  confine **fails on AIRA** with that reason — it does not silently switch slices
  (masking a broken install is dishonest and could run less-confined than intended).
- An explicit `--slice`/`$AIRA_CONFINE_SLICE` **never** falls back — the operator's
  choice is honoured or fails.
- "Resolves to a valid slice" is defined precisely: the unit path exists under the
  user manager, the cgroup is active, `memory.max` is finite (the existing
  effective-cap check), and `memory` is delegated to it.

**Mechanics (Fable, code-grounded).** The probing precedence is linux-only and must
live in the linux `confineWithDeps` path behind a **new injectable dep** — NOT in
the portable `ResolveConfineSlice` (`confine.go`, no build tag; `confine_stub.go`
`//go:build !linux` must keep compiling). `ResolveConfineSlice` is reduced to
flag/env detection (explicit `--slice`/`$AIRA_CONFINE_SLICE` → returned verbatim,
never probed/fallen-back); the aira→whale default resolution + validity probe run
in the linux path using `resolveSlicePath` (`admission_linux.go`) +
`effectiveConfineCap` + a **new** `cgroup.controllers` delegation read the repo
lacks. "Definite absence" = the existing `slice-not-found` reason (a stopped slice
has no cgroup dir). Because the honest slice name is only known after probing,
`result.Status.Slice` stamping (today at the top of `confineWithDeps`) moves to
after resolution. `TestResolveConfineSlicePrecedence` (which hardcodes the
whale.slice default) is rewritten (fine under the not-live/no-compat rule); the
`confineDeps` seam keeps the other confine tests intact.

## 5. Faces

- `aira install [--memory-max=SZ] [--memory-high=SZ] [--allow-overcommit] [--dry-run]` — render +
  atomically write aira.slice + the anchor unit (+ marker), `systemctl --user
  daemon-reload`, `start` the anchor (which pulls in the slice), enable `+memory`
  on subtree_control, then **verify**: slice active, `memory` delegated,
  subtree_control shows `memory`, a probe child exposes `memory.max`. Idempotent by
  content-comparison; a changed cap rewrites the unit + `daemon-reload`, which
  applies the new cap to the live slice (§6 — no `set-property`). `--dry-run` prints
  the rendered units + planned actions, writes nothing, invokes no systemctl.
- `aira install --status` — per-facet honesty: unit present (+ marker ok); slice
  active; MemoryMax/High (live, from `systemctl --user show`); `memory` delegated
  (from live cgroup.controllers/subtree_control); anchor active (vs stale-binary);
  whale.slice coexistence + `--allow-overcommit` state. Anything unreadable →
  `unevaluated`, never a fake ok.
- New dispatch-table descriptor (generated help), `RouteClient`, no MCP tool. Uses
  `systemctl --user` as a subprocess + direct file writes; no dbus, no new deps, no
  cgo. Safety class: `SafetyExecute` (Fable — it matches the other subprocess-
  spawning verbs confine/run/git/time, and being `RouteClient` it is auto-excluded
  from the TUI mutate palette and rejected if it ever reaches the daemon, so it is
  mechanically confirmation-safe either way).

**Dispatch mechanics (Fable, code-grounded).** "Project-less + daemon-optional" is
deliverable ONLY via confine-style **main.go pre-dispatch interception** (intercept
`install` and the hidden `__slice-anchor` on argv before `Dispatch`, exactly as
`confine`/`__confine-setup` are). Routing `install` through the dispatcher's
`RouteClient` path is wrong — that path still does a daemon `ensure-scope` exchange
and `app.Discover` project discovery, breaking both properties. Generalize the
hardcoded `Include = name != "confine"` (`core.go`) to also include `install` (so
generated help lists it) without an MCP tool — `applyDispatchMetadata` panics on a
missing metadata entry, so `install` needs its metadata + a `parseArgs`
allowed-flag entry (or a dedicated parser) like confine. `E_INSTALL_*` errors are
formatted `"CODE: message"` so `store.ErrorCode` prefix-parses them (as
`confineUnavailable` does).

**Code home (Fable).** Put the installer logic + `//go:embed assets` in a new
`internal/install` package with the seam-injected `installDeps` struct (every
os/exec side effect a field, mirroring agentmux); `cmd/aira/main.go` stays a thin
face. Precedent for embed: `internal/pylib` already `//go:embed`s a directory.

## 6. Update semantics — daemon-reload only, single source of truth (Sol r2, measured)

The unit file is the SOLE source of truth. A cap change is: atomically rewrite the
unit → `systemctl --user daemon-reload`. **Measured on systemd 255:** editing a
live slice's `MemoryMax` in the unit and running `daemon-reload` *alone* applies the
new limit to the live cgroup (1G→2G, `memory.max` updated to 2147483648) with **no
`set-property`**. So we do NOT use `systemctl set-property` (Sol r2: it writes a
runtime drop-in — a divergent second config source that can override the embedded
unit on later reloads, and leaves disk/live divergence on partial failure). One
path, no drop-in, no dual source. After reload, `install`/`--status` read back the
live `memory.max` and confirm it equals the declared cap; on mismatch they report
honestly (`unevaluated`/failed), never claiming a change that did not take. A slice
is never "restarted" (its properties are live), so there is no active-job restart
hazard — simpler than agentmux's service updates.

## 7. Tests (TDD; real-systemd gated by `AIRA_REAL_SYSTEMD=1`, throwaway slice name)

- **Pure:** sizing (MemTotal→max/high, precedence, `high≤max`, floor, regex);
  render substitution; marker presence; resolution precedence incl. strict
  no-fallback on a *present-but-broken* aira.slice and never-fallback on explicit
  `--slice`. Proven red against the wrong impl.
- **Seam-injected install:** `--dry-run` writes nothing / invokes no systemctl;
  idempotent second run reports "up to date" (content-equal); a changed
  `--memory-max` reports the delta + re-renders + `daemon-reload`s (no
  `set-property`); a foreign (marker-less) unit of the same name is refused, not
  clobbered; a symlink/non-regular target is rejected; a capped whale.slice without
  `--allow-overcommit` → `E_INSTALL_OVERCOMMIT` (nothing written).
- **Delegation (temp-dir cgroup mock):** enable writes `+memory` given `memory` in
  cgroup.controllers; hard error when absent; **reload-strip regression** — after a
  simulated subtree_control reset, confine's runtime verify fails closed (target
  never runs) rather than launching uncapped.
- **Real-systemd (gated):** install into `aira-test-<pid>.slice` (NEVER the real
  aira.slice/whale.slice), assert active + `memory` delegated + a confine scope
  gets `memory.oom.group`; the anchor keeps delegation across a `daemon-reload`;
  then stop + clean the throwaway. **Both** unit names (the slice AND the anchor
  service) must be parametrised through the installer seam so the fixed
  `aira-slice-keepalive.service` name never leaks into a throwaway-slice test
  (Fable). Skips cleanly with no user manager (`E_INSTALL_UNAVAILABLE`; mention
  `loginctl enable-linger` for headless — Gemini r1).

## 8. Errors

`E_INSTALL_UNAVAILABLE` (no user manager / systemctl failure / no `$XDG_RUNTIME_DIR`),
`E_INSTALL_ARGUMENT_INVALID` (bad size / high>max), `E_INSTALL_DELEGATION` (memory
controller not delegated to the user manager), `E_INSTALL_OVERCOMMIT` (a capped
whale.slice exists and `--allow-overcommit` was not given). Stable, actionable.

## 9. Deferrals (recorded, not built)

**The two owner-directed sequels to THIS milestone (do next, in order):**

1. **Run the live install.** After merge, actually run `aira install --memory-max=64G
   --allow-overcommit` on this box to create the real aira.slice + anchor (the
   milestone's tests only ever touch a throwaway `aira-test-<pid>.slice`). This
   mutates the user's real systemd units, so it is an outward, confirm-first step —
   surfaced to the owner, not done unattended by the build.
2. **Redirect `agentmux whale` → `aira confine`** (in the agentmux repo, NOT AIRA).
   Replace the `systemd-run --user --quiet --scope … --slice=whale.slice …` argv
   built by `buildWhaleRunArgv` (`cmd/agentmux/whale.go:80-89`, dispatched at
   `whale.go:117-118` via `execWhaleRun`) with an exec of `aira confine --slice
   aira.slice -- <cmd>`. `aira confine` is project-less + daemon-optional, so the
   redirect needs no running AIRA daemon. Preserve whale-run's deprioritisation
   (`nice -n 19 ionice -c 3`, `oom_score_adj` bias) — confine already applies its
   own per-scope oom.group + deprioritisation, so verify parity before cutting over.
   Once redirected and whale.slice is idle, retire/alias whale.slice → aira.slice
   becomes the sole 64G pool (fully resolves §3's overcommit; the single-slice size
   already matches, §3 sizing).

**Later / independent:** fold whale watchdog + systemd-oomd (the watchdog itself
already landed in AIRA, #59 — this is the oomd unit + the interlock flip);
`aira install --remove`/uninstall; the daemon-as-anchor consolidation; a system
(root) slice for cross-user confinement; per-run scope sub-caps (already shipped for
confine/run in #57 — this is the install-slice interaction).
