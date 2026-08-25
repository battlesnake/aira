# AIRA install owns systemd-oomd + memory delegation (whale layer-3 migration)

Status: PLAN — the final phase of the whale→AIRA migration (owner, 2026-08-25: "this
aira-owned resource control should completely replace whale, carry over all the lessons,
migrate everything, then retire whale completely from agentmux and the system"). This
milestone moves whale's **layer 3 (systemd-oomd) + memory delegation** into `aira install`.
Safety-critical (root `/etc`, desktop OOM protection) → full two-loop. The agentmux teardown
+ system whale retirement are the FOLLOW-ON milestone (this must ship + be verified FIRST, so
there is never a window with the desktop unprotected).

## 1. What whale does, and what is already migrated

whale (`~/tools/whale/README.md`) is four layers; AIRA already owns three:

| whale layer | AIRA equivalent | status |
|---|---|---|
| 1. `whale.slice` + `whale-run` (capped slice + confinement, per-scope oom.group + oom_score_adj) | `aira.slice` + `aira confine` | DONE (#54/#55) |
| 2. `whale-watchdog` (PSI/meminfo killer) | AIRA enforce watchdog | DONE (#59/#64/#65, live) |
| 4. kernel OOM killer biased by `oom_score_adj=500` | `aira confine` sets oom_score_adj | DONE (#54) |
| **3. systemd-oomd drop-ins (root /etc)** | **— none —** | **THIS MILESTONE** |
| **memory delegation (`Delegate=yes` on user@.service)** | aira install only *checks* it | **THIS MILESTONE** |

AIRA install today is **user-level only** (`internal/install/install.go`: aira.slice in
`~/.config/systemd/user`, the anchor, the daemon service; `requireUserMemoryDelegation`
*checks* delegation and errors "run `agentmux install`" when absent). It has **no root/sudo
phase**. To fully replace whale, aira install must PROVIDE layer 3 + delegation.

## 2. The /etc drop-ins to migrate (verbatim behaviour, aira-owned names, lessons carried)

Ported from `~/tools/cmd/agentmux/assets/`, renamed off "whale", lesson-comments
preserved (and `whale.slice`→`aira.slice` in the prose):

| new aira asset | installed path | content | lesson |
|---|---|---|---|
| `oomd/oomd-overrides.conf` | `/etc/systemd/oomd.conf.d/oomd-overrides.conf` | `[OOM] DefaultMemoryPressureLimit=40%`, `DefaultMemoryPressureDurationSec=10s`, `SwapUsedLimit=100%` | **2026-05-28**: a 50% swap rule killed the desktop over system-service swap; don't re-enable without making system.slice swap-kill-eligible |
| `oomd/user-service-oomd.conf` | `/etc/systemd/system/user@.service.d/50-aira-oomd.conf` | `[Service] ManagedOOMMemoryPressureLimit=40%` | overrides Ubuntu's 50% so the global 40% applies (50- sorts after 10-) |
| `oomd/user-slice-oomd.conf` | `/etc/systemd/system/user-<uid>.slice.d/oomd.conf` | `[Slice] ManagedOOMMemoryPressure=kill` | extend oomd from user@.service up to the whole user-<uid>.slice |
| `oomd/session-slice-oomd-protect.conf` | `/etc/systemd/user/session.slice.d/oomd-protect.conf` | `[Slice] ManagedOOMPreference=avoid` | protect desktop services (dbus/pipewire/gpg-agent) from being the victim |
| `user-service-delegate.conf` | `/etc/systemd/system/user@.service.d/10-aira-delegate.conf` | `[Service] Delegate=yes` | **cgroup-v2 cap unenforced without delegation**; applies only at next user@.service (re)start → re-login/reboot |

After writing changed drop-ins: system `systemctl daemon-reload` + `systemctl restart
systemd-oomd` (for the oomd ones). The delegation drop-in is NOT restarted (Delegate= applies
at the next user@.service start — re-login/reboot), same as whale.

## 3. Root/user phase model — mirror agentmux's proven one (do not reinvent)

Port the structure of `agentmux runInstall` (`~/tools/cmd/agentmux/install.go:737`):

- **Non-root `aira install`** (the common case): run the existing user-level install (aira.slice
  + anchor + daemon service, unchanged). Then a **read-only** oomd/delegation staleness check:
  if the memory controller is not delegated OR any oomd drop-in is missing/stale, print exactly
  ONE non-blocking warning — `run 'sudo aira install' to apply the /etc oomd + delegation
  drop-ins, then re-login` — and mark the summary `oomd/delegation: needs sudo`. The user
  install still SUCCEEDS (layers 1-2 are user-level and already work).
- **Root `sudo aira install`**: `validateSudoIdentity` (consistent SUDO_USER/-UID/-GID, else
  hard-fail) → resolve target user → verify `/run/user/<uid>` session + a target-readable
  re-exec binary BEFORE any /etc write (fail-fast, leave /etc untouched) → install the
  missing/stale /etc drop-ins as root (oomd always; the **delegation drop-in gated on the
  runtime enforcement check** — modern systemd delegates memory by default, so only write it
  when the controller is NOT already delegated) → each drop-in self-contained (stage → install
  → its own system `daemon-reload`; oomd ones also `restart systemd-oomd`) → **re-exec the user
  phase as the target user** (`AIRA_INSTALL_REEXEC=1`) so layers 1-2 install regardless → surface
  a failed oomd/delegation activation as a non-zero exit AFTER the user phase ran.
- Enable user-linger as root before the re-exec (authoritative, no polkit), matching #62.

Reuse the existing aira install idempotence machinery (content-compare, only rewrite +
daemon-reload when changed; hardened atomic write; `openExistingUnitDirectory`/`publishManagedUnit`)
for the /etc drop-ins too — extended to `/etc` paths (root-owned, 0644).

## 4. Honesty + status (unchanged AIRA principles)

- The install summary reports memory-cap enforcement HONESTLY: `enforced` (controller delegated)
  vs `not enforced — run 'sudo aira install', then re-login`. Never claim enforced when the
  runtime cgroup check says otherwise.
- oomd drop-in status: `installed` / `up to date` / `needs sudo` / `daemon-reload failed`. A
  file written but a failed `daemon-reload`/`restart` is a non-zero exit, never a silent green
  (mirror agentmux's "masking" guard).
- Staleness is content-compare; an already-correct /etc is a no-op (no reload, no restart).

## 5. Tests (TDD; pure via the install deps seam)

The install deps seam (`installDeps` — `geteuid`, `lookupUID`, `stat`, `writeFile`, `run`,
`reexec`, `readFile`, `getenv`, `mkdirTemp`) already exists; extend the fakes for the root
paths. Cover:
- **Drop-in content**: each of the five assets renders + installs to the right path with the
  exact expected content (incl. the lesson comments).
- **Root phase**: `sudo` path validates identity, fails fast on missing session / unreadable
  exec BEFORE any /etc write; writes oomd drop-ins; writes delegation ONLY when not-delegated;
  re-execs the user phase; a failed daemon-reload/restart → non-zero exit (not masked).
- **Non-root phase**: does the user install, emits exactly ONE needs-sudo warning when
  oomd/delegation stale, SUCCEEDS regardless; emits NOTHING when already enforced + oomd current.
- **Idempotence**: a second run with everything current writes nothing, reloads nothing.
- **Delegation gating**: delegated-by-default → no delegation drop-in written; not-delegated →
  written + honest not-enforced-until-relogin status.
- **Honesty**: summary never reports enforced when the controller check is false.
- `go build ./... && go vet ./... && go test ./internal/install/ -race` green; full `make test`.

## 6. Migration ordering + deferrals (the follow-on milestone)

- **This milestone ships first + is DEPLOYED with `sudo aira install` + VERIFIED** (aira now
  writes the /etc oomd + delegation drop-ins; `oomd.conf.d`/`user@.service.d` show aira-owned
  files; systemd-oomd active with the 40% thresholds; aira.slice still enforced). During the
  overlap BOTH aira's and agentmux's drop-ins exist — harmless (both set the same values;
  duplicate `Delegate=yes` / `40%` merge idempotently).
- **DEFERRED to the follow-on milestone (agentmux teardown + system retirement):** strip ALL
  whale + memory/oomd/delegation from agentmux (`whale.slice`, `whale-watchdog`, the wrappers,
  the `whale` subcommand + `whale.go`, `installClaudePolicy`, the whale.slice MemoryMax
  machinery, `oomdBackstop`/`memoryDelegationBackstop` + their assets), add a cleanup that
  removes the retired agentmux/whale units + /etc drop-ins, rebuild + reinstall agentmux; then
  remove the lingering `whale.slice` / `whale-watchdog.service` system units. Only after AIRA is
  verified owning oomd+delegation, so there is never an unprotected window.
- **Out of scope**: the shared-slice aggregate-contention problem (multiple sessions' jobs
  summing > 64 GB → cap OOM) surfaced by the merge-gate kills — a real but separate issue (the
  AIRA watchdog was exonerated: 0 enforce events). Tracked separately.
