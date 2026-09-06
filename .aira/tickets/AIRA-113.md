---
{"schema":1,"id":"AIRA-113","project":"aira","title":"Dynamic per-scope oom_score_adj steering for the residual aggregate-full slice OOM","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","deferred-from-aira29","oom","scheduler"],"hold":false,"relations":[]}
---
Deferred from AIRA-29 (dynamic reserve), with reasoning recorded in
`docs/superpowers/specs/2026-09-06-aira29-dynamic-reserve-plan.md` §3.6 and endorsed at P2
by the plan-gate reviewer.

AIRA-29's banked v3 plan proposed a daemon-wide loop that raises a bursting scope's
`oom_score_adj` toward 1000 on an `RSS - effectiveCharge` baseline and restores it when the
scope falls back within its charge, so that the residual aggregate-full slice OOM (§4e of
that plan) kills the job outrunning its accounting rather than a compliant neighbour.

**Why it was not built with AIRA-29:**

1. The owner's named containment already exists and is deployed — AIRA-27's class steering
   (non-delegate 500, delegate 800), set by the confined child at exec and inherited by
   descendants. AIRA-29 does not weaken it.
2. The trigger is near-inert inside the existing <=1s scan: the charge is computed from the
   same reading, and `rss <= peakSoFar` by the ratchet, so `rss - charge > 0` is reachable
   only in the narrow window where `memory.current` transiently exceeds `memory.max`. To
   catch the population it is aimed at, the loop must run FASTER than the charge refresh and
   read RSS between refreshes — a new daemon subsystem.
3. It also needs a recursive child-cgroup pid walker that does not exist (`Members()` is
   leaf-only), per-pid `/proc/<pid>/oom_score_adj` writes at >1 Hz, and a restore-down
   re-walk, with real-cgroup tests in both directions.
4. AIRA-29's `growth` margin term catches the same population one interval earlier, at the
   ledger, with no new subsystem.

**Established while evaluating it, so a future build does not have to re-derive it:** a
uid-1000 process with `CapEff: 0` CAN lower another process's `oom_score_adj` through
`/proc/<pid>/oom_score_adj` (probed: 0 -> 700 -> 300, all permitted). The `CAP_SYS_RESOURCE`
restriction applies only to the legacy `/proc/<pid>/oom_adj` file. So restore-down is
FEASIBLE; the reason to defer is proportionality, not permission.

Revisit if the residual aggregate-full slice OOM is ever observed in the field picking a
compliant victim.
