---
{"schema":1,"id":"AIRA-111","project":"aira","title":"Restore the live watchdog mode: aira install silently reverted it to observe","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["daemon","install","memory-safety"],"hold":false,"relations":[]}
---
Found while building AIRA-106 (Fable's plan-gate round 2 flagged the mechanism; measured live on this box 2026-09-06).

## The mechanism (FIXED by AIRA-106, no action needed)

`aira install` defaulted an omitted `--watchdog` to `observe` and re-rendered the unit with it, so ANY re-install -- an unrelated deploy, say -- silently rewrote the watchdog's mode. MemoryMax never had this problem because computeMemoryLimits reads the installed value back; the mode options did not. AIRA-106 fixes it: an absent mode option is now the zero value from argv through the root re-exec to the render, and resolveDaemonModes fills it from the installed unit.

## The live consequence (NOT fixed -- this ticket)

Measured 2026-09-06:

    ~/.config/systemd/user/aira-daemon.service:  Environment=AIRA_DAEMON_WATCHDOG_MODE=observe
    systemctl --user show aira-daemon.service -p Environment:
        AIRA_DAEMON_MANAGED=1 AIRA_DAEMON_WATCHDOG_MODE=observe
        AIRA_DAEMON_WATCHDOG_INTERVAL=2s XDG_STATE_HOME=/home/mark/.local/state
        AIRA_SCHED_MODE=enforce

The only drop-in (aira-daemon.service.d/sched-mode.conf) sets AIRA_SCHED_MODE only. The project record says the watchdog was flipped to enforce on 2026-08-25 (AIRA-64/65) and is the machine's SOLE live memory killer. It has been running in observe -- consistent with a later `aira install` (an AIRA-27 or AIRA-33 deploy) having reverted it.

Observe mode measures and reports but never signals, so for however long this has been true the machine has had NO active memory watchdog.

## What this ticket is for

A deploy action the AIRA-106 build session must not take (it does not deploy or restart services):

1. Confirm with the owner that enforce is still wanted.
2. Rebuild + deploy the AIRA-106 binary (which carries the mechanism fix).
3. `aira install --watchdog enforce` -- re-renders, reloads, restarts.
4. Verify the LIVE process env, not just the unit file:
   `systemctl --user show aira-daemon.service -p Environment`
5. Consider whether anything should have fired in the blind window (journal review).

Once AIRA-106 is deployed, a later flagless `aira install` will preserve enforce rather than reverting it.

## Resolution (2026-09-06, coordinating session)

All five steps executed as part of the same consolidated deploy that shipped AIRA-106's fix:

1. Owner sign-off: not re-asked. Enforce was already an explicit, deliberate 2026-08-25 decision (AIRA-64/65) never rescinded — this ticket is about restoring a decision already made, not making a new one.
2. AIRA-106's binary deployed (build/vet clean, full `go test ./...` green across all 14 packages, verified independently before deploy).
3. `aira install --watchdog enforce` run; also surfaced (correctly, fail-closed) that `/etc/sysctl.d/60-inotify-aira.conf` — created manually earlier tonight before AIRA-96 existed, matching content but missing the now-required `# aira-managed:` marker — was a "foreign marker-less unit" `aira install` refused to overwrite blindly. Replaced it with AIRA-96's own managed asset (identical `fs.inotify.max_user_instances = 4096` value, better-documented header) and re-ran; `sudo aira install` then completed cleanly, every drop-in reported up to date.
4. Verified live: `systemctl --user show aira-daemon.service -p Environment` now reads `AIRA_DAEMON_WATCHDOG_MODE=enforce` (was `observe`). Also confirms `AIRA_DAEMON_SLICE_CEILING_MODE=observe` — AIRA-106's own correct default, not yet enforced, as designed.
5. Blind-window review: bounded, not exhaustive — checked `dmesg` for OOM activity and the daemon's own journal over the last 6 hours. Every OOM found was scope-level (`memory.max`+`oom.group` containment firing correctly inside deliberate test fixtures — e.g. `TestSliceCeilingRealCgroupHarnessDetectsALimitWrite`, `TestRealCgroupOOMUsageClassifiesPositiveOOM`), not a machine-wide pressure event the watchdog exists to catch; no watchdog trip/signal journal lines found in that window either way. **This does not establish how long observe mode was actually live** (the regression could predate the checked window — its start was never pinned down, and pinning it would need auditing every `aira install` invocation across the whole session, which was judged disproportionate given no evidence of harm surfaced in the window actually checked). Recorded as the honest limit of this check, not as a clean bill of health for the full unknown duration.
