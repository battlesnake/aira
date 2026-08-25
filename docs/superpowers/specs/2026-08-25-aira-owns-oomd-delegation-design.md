# AIRA install owns systemd-oomd + memory delegation (whale layer-3 migration)

Status: PLAN v3 (builder-ready) — v2 folded Sol+Fable GATE-FAIL (always-own-delegation, honest
tri-state enforcement, relax the 4 strict checks, unique /etc names, marker/ownership policy,
seam extension). v3 folds the re-gate: Sol GATE-FAIL→one P1 (privilege drop via fork/exec
`SysProcAttr.Credential`, not parent `setresuid`) + Fable GATE-PASS-WITH-NITS "ready to build"
(root `--dry-run` forwarding, in-dest-dir `publishManagedUnit` staging, `E_INSTALL_DELEGATION`
registry drop). Both gates now pass. Final phase of whale→AIRA (owner 2026-08-25: AIRA fully replaces whale, carry all
lessons, then retire whale from agentmux + system). Safety-critical (root /etc, desktop OOM
protection) → full two-loop. The agentmux teardown + system whale retirement are the FOLLOW-ON
milestone (this ships + is verified FIRST; the desktop is never unprotected).

## 1. What whale does, and what is already migrated

whale (`~/tools/whale/README.md`) = four layers; AIRA owns three:

| whale layer | AIRA equivalent | status |
|---|---|---|
| 1. capped slice + confinement (per-scope oom.group + oom_score_adj) | `aira.slice` + `aira confine` | DONE |
| 2. PSI/MemAvailable watchdog | AIRA enforce watchdog (live) | DONE |
| 4. kernel-OOM oom_score_adj bias | `aira confine` | DONE |
| **3. systemd-oomd /etc drop-ins** | **— none —** | **THIS MILESTONE** |
| **memory delegation (`Delegate=yes`)** | aira install only *checks* it (hard-fails) | **THIS MILESTONE** |

Confirmed against code: `internal/install/install.go` `runInstall` (249-488) is user-level only
(`~/.config/systemd/user` at 262); no `geteuid()==0` branch, no sudo/re-exec, no `/etc` write;
`requireUserMemoryDelegation` (730-740) is check-only and HARD-errors `E_INSTALL_DELEGATION`
"run `agentmux install`" (737).

## 2. The five /etc drop-ins — aira-UNIQUE names, lessons carried (Fable P1-b)

Ported from `~/tools/cmd/agentmux/assets/`; ALL FIVE get aira-unique filenames so during
the overlap they COEXIST with agentmux's (never overwrite them); prose de-whaled; lessons kept:

| aira asset | installed path (aira-unique) | content | agentmux file it coexists with |
|---|---|---|---|
| `oomd/oomd-overrides.conf` | `/etc/systemd/oomd.conf.d/aira-oomd.conf` | `[OOM] DefaultMemoryPressureLimit=40%`, `DefaultMemoryPressureDurationSec=10s`, `SwapUsedLimit=100%` | `whale-overrides.conf` |
| `oomd/user-service-oomd.conf` | `/etc/systemd/system/user@.service.d/50-aira-oomd.conf` | `[Service] ManagedOOMMemoryPressureLimit=40%` | `50-whale.conf` |
| `oomd/user-slice-oomd.conf` | `/etc/systemd/system/user-<uid>.slice.d/50-aira-oomd.conf` | `[Slice] ManagedOOMMemoryPressure=kill` | agentmux's `oomd.conf` (**different name** — Fable P1-b) |
| `oomd/session-slice-oomd-protect.conf` | `/etc/systemd/user/session.slice.d/50-aira-oomd-protect.conf` | `[Slice] ManagedOOMPreference=avoid` | agentmux's `oomd-protect.conf` (**different name** — Fable P1-b) |
| `oomd/user-service-delegate.conf` | `/etc/systemd/system/user@.service.d/10-aira-delegate.conf` | `[Service] Delegate=yes` | `10-agentmux-delegate.conf` |

**Lessons preserved verbatim in the comments:** the `SwapUsedLimit=100%` block keeps the
2026-05-28 rationale (a 50% swap rule killed the desktop when system services filled swap; don't
re-enable without making system.slice swap-kill-eligible); `session.slice avoid` keeps the
protect-dbus/pipewire/gpg-agent rationale; the delegate drop-in keeps "cgroup-v2 cap unenforced
without delegation; applies at next user@.service (re)start → re-login/reboot".

Activation (Sol P1): after writing changed drop-ins — `systemctl daemon-reload` (unit drop-ins);
`systemctl restart systemd-oomd` (oomd.conf.d + the oomd unit drop-ins); **never** restart
`user@<uid>.service`; delegation reported PENDING until logout/login or reboot.

## 3. Own delegation UNCONDITIONALLY + prove enforcement (Sol P0×2, Fable P1-d)

- **Always install `10-aira-delegate.conf`** on the root path, regardless of the current runtime
  delegation state. The controller may be delegated today ONLY because of agentmux's
  `10-agentmux-delegate.conf` (`agentmux/install.go:1394`); gating aira's write on "already
  delegated" means aira never takes ownership, so the follow-on teardown removes agentmux's
  drop-in and delegation is lost at next login. AIRA must own its own delegation drop-in outright.
- **Honest tri-state enforcement status** (never "enforced" from a mere successful write):
  `active` = the live `user@<uid>.service/cgroup.controllers` contains `memory` AND
  `aira.slice/memory.max` reads back the intended cap; `pending re-login` = the delegate drop-in
  is installed but the controller is not yet delegated this session (expected — `Delegate=`
  applies at next `user@.service` start); `not installed` = no drop-in and not delegated. The
  install summary + `aira install --status` report this tri-state (Fable P2-3); `sudo aira
  install --dry-run` lists the planned /etc writes.

## 4. Relax the delegation-STRICT user phase to honest-degrade (Fable P1-c)

aira's user phase currently HARD-FAILS without delegation at four points —
`requireUserMemoryDelegation` (304, 730-740), `enableMemoryDelegation` (408, 1006-1032),
`verifyLiveLimits` (411), the anchor delegated-controller probe (418). That makes the ported
"re-exec the user phase so layers 1-2 install regardless" IMPOSSIBLE on a not-yet-delegated box
(fresh machine, or post-teardown before re-login): the re-exec'd child would abort having
installed nothing. Change these four from hard-fail to **honest-degrade**: still install the
user-level units (aira.slice, anchor, daemon service), but when the controller is not delegated,
skip the live-enforcement asserts and report `not enforced — run 'sudo aira install', then
re-login` (tri-state `pending`), exit 0 for that expected transitional state. This preserves
honesty (never claims enforced when it isn't) while letting aira bootstrap delegation on a fresh
machine. (`E_INSTALL_DELEGATION` hard-fail is removed; the "run agentmux install" hint becomes
"run sudo aira install".) The strict aira.slice-not-whale.slice fallback refusal from #55 is
UNCHANGED — only the delegation-absent path degrades instead of aborting. **Removing the
`E_INSTALL_DELEGATION` code (Fable P2-3) must also drop its registry entry
`"E_INSTALL_DELEGATION": 4` at `internal/store/check.go:59`** (the fail-closed code-registry test
will otherwise catch the orphan) — or keep the code defined-but-unused if any other site emits it
(grep first).

## 5. Root/user phase model — port agentmux's, with the seam + privilege-drop specced

Mirror `agentmux runInstall` (`~/tools/cmd/agentmux/install.go:737`):

- **Seam extension (Fable P1-a):** aira's `installDeps` lacks `lookupUser`/`lookupUID` and a
  stdio-streaming `reexec` (its `run` captures stdout). Add `lookupUser`, `lookupUID`, `reexec`
  fields (+ fakes) via the existing `fillInstallDeps` reflection seam (490-511).
- **Non-root `aira install`**: the (now degrade-tolerant §4) user install, then a read-only
  oomd/delegation staleness check; if the controller isn't delegated OR any aira drop-in is
  missing/stale, print exactly ONE non-blocking warning "run 'sudo aira install' to apply the
  /etc oomd + delegation drop-ins, then re-login" and mark the summary; still exit 0.
- **Root `sudo aira install`** — harden the privilege transition (Sol P1): resolve identity via
  passwd from `SUDO_UID` (reject a direct/ambiguous root invocation with no SUDO_*), require
  agreeing `SUDO_USER`/`SUDO_UID`/`SUDO_GID`, validate `/run/user/<uid>` exists AND is owned by
  <uid>, sanitize the environment. **Drop privileges via the fork/exec credential path, not
  parent `setresuid` (Sol re-gate P1):** resolve the target's supplementary groups while still
  privileged (`getgrouplist`/`user.Lookup`), then spawn the re-exec with
  `exec.Cmd{SysProcAttr: &syscall.SysProcAttr{Credential: &syscall.Credential{Uid, Gid, Groups}}}`
  so the fork'd child applies uid/gid/groups atomically pre-exec (single-threaded, no Go
  multithreaded thread-credential hazard); the parent stays root and waits. Inherit only stdio
  (`Stdout`/`Stderr` = os.Stdout/Stderr; every other fd CLOEXEC), fail-closed on any error. Never
  do user-file ops as root in-process. Order: fail-fast (session + target-readable re-exec
  binary) BEFORE any /etc write → install the aira-named /etc drop-ins (oomd always; **delegation
  ALWAYS** per §3) → re-exec the user phase as the target user (`AIRA_INSTALL_REEXEC=1`) so the
  user-level layers install → surface a failed oomd/delegation activation as a NON-ZERO exit
  AFTER the user phase ran (agentmux's anti-masking ordering). Under `--dry-run`, forward
  `--dry-run` to the child (its early-return makes the re-exec a safe no-op that prints the
  planned user-level writes) — never re-exec a REAL user install under a dry run (Fable P2-1).
- Enable user-linger as root BEFORE the re-exec (authoritative, no polkit); the re-exec child
  does read-only `lingerReport` via the `AIRA_INSTALL_REEXEC` sentinel, NOT another
  `enable-linger`, and wraps any `loginctl` in `timeout` (Fable P2-2).
- **Re-exec argv (Fable P2-1):** put env AFTER `sudo`/the drop (env_reset), passing
  `HOME`/`XDG_RUNTIME_DIR`/`DBUS_SESSION_BUS_ADDRESS` (the child needs them for
  `installHome` + `systemctl --user` + daemon socket identity) and forwarding
  `--memory-max`/`--memory-high`/`--watchdog`/`--watchdog-interval`/`--allow-overcommit`.

**Transactional /etc writes (Sol P1; mechanism per Fable P2-2):** "staging/validation" = render +
validate all five drop-ins IN MEMORY in a pre-flight pass (fail before any write if any renders
wrong); the atomic write itself is the existing `publishManagedUnit` per destination directory —
its temp file lives in the DESTINATION dirfd and uses same-dir `renameat` (same-FS guaranteed,
no cross-dir EXDEV), root-owned, non-symlink target, mode 0644, fsync. A write / daemon-reload /
restart failure reports EXPLICIT partial installation with a non-zero exit — never "up to date".
Ordered user daemon-reload/restart so the running aira-daemon never observes a half-installed
user unit.

## 6. Marker + ownership policy for aira's /etc files (Fable P1-e)

Reuse `publishManagedUnit`/`openExistingUnitDirectory`/`readManagedUnitAt` for /etc, extended:
targets are **root-owned (uid 0)** (the FD validations use uid 0, not the target uid); each aira
/etc drop-in carries the aira managed-unit marker comment (systemd `.conf` files allow `#`
comments, so the marker is inert). Because §2 gives aira UNIQUE filenames, **aira only ever reads
/writes its OWN aira-named files** — it never adopts, rewrites, or marker-rejects agentmux's
foreign marker-less files (those are the teardown's job). The idempotence flock moves to a
root-writable path for the /etc phase (the user-unit-dir flock at 359-369 is user-owned).

## 7. Tests (TDD; pure via the extended deps seam)

- **Drop-in content + path**: each of the five assets renders + installs to its aira-unique path
  with the exact expected content INCLUDING the lesson comments.
- **Invariant pins (Sol P2)**: assert the oomd asset literally contains `SwapUsedLimit=100%` and
  `session.slice` `ManagedOOMPreference=avoid` (so a later edit that restores the desktop-killing
  50% swap rule fails RED) — the #64 threshold-pin pattern.
- **Always-own-delegation (P0)**: the root phase writes `10-aira-delegate.conf` EVEN WHEN the
  controller is already delegated (fake: delegated) — RED against a gated-on-enforcement impl.
- **Honest tri-state**: delegated live cgroup → `active` + memory.max readback asserted;
  drop-in-written-but-not-delegated → `pending re-login` (never `active`); neither → `not
  installed`. Summary never reports `active` when the controller check is false.
- **Degrade-tolerant user phase (P1-c)**: not-delegated → user units still install, exit 0,
  status `pending` (RED against the current hard-fail at 304/408/411/418).
- **Root privilege/fail-fast**: reject ambiguous root (no SUDO_*); fail-fast on missing/foreign
  `/run/user/<uid>` or unreadable re-exec binary BEFORE any /etc write; a failed
  daemon-reload/restart → non-zero exit (not masked as up-to-date).
- **Idempotence**: a second run with all five current + delegated writes nothing, reloads nothing.
- **Coexistence**: aira's unique-named drop-ins never touch agentmux's foreign files.
- `go build ./... && go vet ./... && go test ./internal/install/ ./cmd/aira/ -race` green; full `make test`.

## 8. Migration ordering + deferrals (follow-on milestone)

- **This milestone ships first, is DEPLOYED with `sudo aira install`, and VERIFIED** owning
  oomd+delegation: `oomd.conf.d/aira-oomd.conf` + the four `*aira*` unit drop-ins present;
  `systemd-oomd` active with the 40% thresholds; the tri-state reports `active` (or `pending`
  until re-login); aira.slice still enforced (memory.max readback). During overlap BOTH aira's
  and agentmux's drop-ins exist — harmless because they set IDENTICAL values (Sol P1: the
  follow-on must verify EFFECTIVE MERGED config with `systemd-analyze cat`/`oomctl`, not file
  presence, before removing agentmux's, then daemon-reload/restart-oomd).
- **DEFERRED (follow-on):** strip ALL whale + memory/oomd/delegation from agentmux (whale.slice,
  whale-watchdog, wrappers, the `whale` subcommand + `whale.go`, `installClaudePolicy`, the
  whale.slice MemoryMax machinery, `oomdBackstop`/`memoryDelegationBackstop` + assets), add a
  cleanup removing the retired agentmux units + /etc drop-ins, rebuild + reinstall agentmux; then
  remove the lingering `whale.slice`/`whale-watchdog.service` system units. Only after AIRA is
  verified owning oomd+delegation (no unprotected window).
- **Out of scope**: the shared-slice aggregate-contention OOM (multiple sessions summing > 64 GB)
  from the merge-gate kills — real but separate (the watchdog was exonerated, 0 enforce events).
