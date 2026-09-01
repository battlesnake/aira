---
{"schema":1,"id":"AIRA-27","project":"aira","title":"Admission reserves but doesn't ENFORCE → slice over-commit OOMs well-behaved confined neighbours (flat oom_score_adj=500)","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","dogfood","oom","shared-slice"],"hold":false,"relations":[]}
---
Reported convergently by two dogfooding sessions 2026-09-01 (subpipe/altium inc-4b + money), confirmed by slice telemetry.

ROOT CAUSE: confine admission RESERVES slice RAM but does not ENFORCE that a job stays within its reservation. The `--memory-max N --memory-reserve small` combo reserves `small` in the ledger while the scope's memory.max is set to the explicit N — so the job can balloon to N at runtime while admission only accounted `small`. Σ(actual RSS) then exceeds Σ(reserved) → aira.slice hits its own memory.max (64G) → a SLICE-level OOM fires. Because every confined process is launched with a FLAT `oom_score_adj=500` (confine_linux.go, oomAdj), the kernel's victim selection does NOT prefer the over-subscriber — a WELL-BEHAVED job within its reservation is just as likely (often the unlucky) victim. Its scope's `oom.group=1` then group-kills the whole innocent scope.

EVIDENCE:
- subpipe: a `make merge-gate` under a PROPER whole-job reservation (not the under-reserve anti-pattern) was OOM-killed 5× at the engine leg; a plain liveness probe SURVIVED the same window; engine.log reached ~73% before a slice-wide kill; completed clean on the first try AFTER the slice drained (same reservation). `--list` showed ~63G granted / ~62G ceiling across 20-28 jobs incl. multiple sibling engine gates. ~6h of retries lost.
- money: a LIGHT `aira confine --memory-max 512M -- bash <poll>` using ~10MB died with `scope-integrity=unverified` (NOT descendant-killed = not ITS own scope OOM) — killed externally by the slice-level OOM picking a confined victim. money initially suspected a banned `systemctl stop aira.slice`; REFUTED (slice continuously active since the 19:19 WSL reboot, no stop/watchdog/confine-kill event).
- slice telemetry: aira.slice memory.events `max=1053918` (hit its cap constantly), `oom=83`, `oom_kill=327`, `oom_group_kill=23`. Confine oom_score_adj=500 makes confined jobs the PREFERRED slice-OOM victims.

CLARIFY (both peers conflated it): confined jobs are WATCHDOG-exempt but NOT exempt from the slice's own cgroup memory.max OOM — adj=500 makes them the preferred targets.

FIXES (subpipe's, both sound):
(a) ENFORCE reserve ≈ cap (the ROOT fix): reject or up-charge `--memory-max` > the admission reserve so a scope physically cannot exceed what it reserved (set scope memory.max = the charged reserve, or charge reserve = --memory-max). Then Σ(actual) ≤ Σ(reserve) ≤ cap−headroom by construction — the slice cannot over-commit, so the slice-level OOM cannot fire. Note: a plain `--memory-reserve R` (no --memory-max) ALREADY caps the scope at R (confine_linux.go:588) — the hole is the explicit `--memory-max` override.
(b) RESERVATION-AWARE oom_score (defense-in-depth): scale oom_score_adj by reservation-compliance — a scope over its reserve should be a higher-priority OOM victim than one within it, so the over-subscriber is killed, not the well-behaved neighbour. Replaces the flat 500.

SEVERITY P1: a shared-slice hazard that destroys well-behaved neighbours' long gates under contention (data loss, no queue to notice), triggered by a common anti-pattern. relates: AIRA-24 (the --memory-max-big+--memory-reserve-small under-reserve footgun), AIRA-12/#67 (governed OOM / reserve ledger), AIRA-16 (watchdog). This is the RAM-bound-enforcement piece of the cooperative-scheduler direction.
