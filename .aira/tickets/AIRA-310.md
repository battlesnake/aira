---
{"schema":1,"id":"AIRA-310","project":"aira","title":"confine: on exit, hint on stderr when peak RAM/VRAM was far below an explicit (pinned) reservation","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.38","labels":[],"hold":false,"relations":[]}
---


Spec (owner-decided 2026-10-13):
- Fires when a job ends normally (not failed/killed), non-CI mode, and peak RAM < 60% of an explicitly pinned
  reservation (--memory-reserve) of at least 2 GiB. Same rule for peak VRAM vs a declared --vram.
- Unpinned (history-estimated) reservations never hint: nothing for the agent to adjust.
- stderr line after the trailer: "confine: hint: peak 310M of 4096M reserved (8%); consider --memory-reserve 512M".
  Suggested value = peak + 50% headroom, rounded up.
- Silent in ci-shim mode; opt-out AIRA_CONFINE_NO_HINTS=1.

Build notes (2026-10-13):
- RAM only. confine measures no per-job peak VRAM: `--vram` is admission accounting
  only (no VRAM cgroup, nothing sampled) and per-process VRAM reads [N/A] on WSL2,
  so a VRAM hint would fabricate its peak. Not built; documented in the skill text
  and the --memory-reserve help.
- "Pinned" = the declared `--memory-reserve` (the existing `declaredReserve`
  provenance), and only when the reserve actually charged equals it (with
  `--memory-max` the ledger charges the max: no hint). A reserve pinned only by an
  inherited `AIRA_CONFINE_RESERVE` gets no hint: every nested confine shares the
  variable, so one per-job number is the wrong advice (build review P3).
- No hint for a container outside the job scope (docker, or podman with a
  caller-chosen placement): peak-rss is then the client alone and the advice would
  under-charge the shared ledger (build review P1). Podman with the injected
  `--cgroups=split` nests in the scope and still hints.
- The percentage is truncated, so a peak just under 60% never reads "(60%)"; the
  owner's example line therefore prints 7% for 310M of 4096M (build review P3).
- Suggestion = ceil(1.5 x peak) rounded up to a whole 256M, floor 256M; no hint
  when that suggestion is not below the reserve. Threshold strict: peak*5 < reserve*3.
- Opt-out: AIRA_CONFINE_NO_HINTS exactly "1". Real path only (the ci-shim path
  has no peak and the function also requires containment=enforced).
