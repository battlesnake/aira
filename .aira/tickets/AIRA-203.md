---
{"schema":1,"id":"AIRA-203","project":"aira","title":"aira install does not restart the daemon when only the binary changed, so a correctly-run install can leave the old image serving","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

`aira install` does not restart the daemon when only the binary changed, so a correctly-run install can leave the old image serving
**kind** bug · **severity** P2 · **closes** RANT-25 (half) · **do after T2a**

**SYMPTOM.** `install.sh` (9 lines) does `install bin/aira ~/.local/bin/aira` — a new inode — then `~/.local/bin/aira install`. The unit's `ExecStart=/home/mark/.local/bin/aira daemon serve` is unchanged, so the unit text is byte-identical and the running daemon keeps executing the old, now-unlinked image. No documented reinstall step corrects for it.

**ROOT CAUSE.** `internal/install/install.go:1154-1163` restarts only under `if daemonPresent && daemonChanged`, where `daemonChanged` means the **unit file content** changed. The comment is right about the case it considers (a byte-identical convergence re-run must not bounce a live daemon) and never considers that ExecStart's target may have been replaced underneath it. `ServiceIdentityMatches` (`internal/daemon/service.go:38-74`) compares socket paths, not build identity, so the one identity predicate in the tree is blind to this by construction.

**PROPOSED FIX.** Restart when the ExecStart binary's **identity** changed, comparing the daemon-reported revision (T2a) against the installing binary's own — the fail-closed test. Do **not** make `/proc/<pid>/exe` reading `(deleted)` the primary detector: it fires only when the old inode was unlinked, so a `cp`-over-same-inode misses silently, which is the shape AIRA exists to refuse. The unit already carries `Restart=always`, so an unconditional `systemctl --user try-restart` when the installed bytes changed is the cheap alternative and should be weighed explicitly. Preserve the property the current comment defends: unit unchanged **and** binary unchanged must still issue no restart. **State the drain question:** a restart drops in-flight admission leases and forces reserve-ledger reconstruction; `aira drain` exists for exactly this and the 2026‑09‑08 drain-mode plan's open Question 4 ("whether install.sh's deploy sequence should recommend/wrap `aira drain`") is directly adjacent — cite and link it rather than bouncing the machine-wide single writer with no word about the jobs it is serving.

**HOW TO TEST.** With the existing `SystemctlRun` fake: byte-identical unit + differing daemon binary identity must record `systemctl --user restart aira-daemon.service` (fails today — `daemonChanged` is false). False-pass twin: identical unit **and** identical binary must record no restart.

---
