---
{"schema":1,"id":"AIRA-16","project":"aira","title":"Watchdog belt-and-braces: don't blanket-exempt a genuinely-uncapped AIRA scope + slice-internal-pressure trigger","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---
Follow-up to AIRA-15 (delegate-ram scope-cap fix). Two defense-in-depth items the AIRA-15 plan-review (Fable P2-f) deferred here to keep AIRA-15 focused: (1) the memory watchdog force-exempts EVERY .aira cgroup component (watchdog.go:344,:553,:825 — 'uncapped' is forced false for AIRA paths), which is only sound IF every AIRA scope is capped. AIRA-15 caps delegate-ram scopes, but RESIDUAL uncapped AIRA classes remain BY DESIGN (non-delegate flock-fallback + daemon-unevaluated scopes get no reserve-cap and no delegate-ram default) — an accepted gap until this lands. Fix: stop blanket-exempting a genuinely-UNCAPPED (.aira memory.max==max) scope; such a scope is a valid watchdog target (the offender). (2) The watchdog only trips on host MemAvailable, so it is BLIND to a slice-INTERNAL OOM (aira.slice at its 64G memory.max while the box still has ~14G free — the AIRA-15 collateral scenario). Add a slice-internal-pressure trigger (aira.slice memory.current near memory.max, or slice PSI) that proactively kills the biggest offending scope before the kernel's blind process-level slice-OOM hits a capped neighbour. Together these + AIRA-15 close the collateral-OOM class more fully; AIRA-15 alone kills the single-runaway case. Related: AIRA-15, [[AIRA-12]], the cooperative-scheduler Slice 3 (RAM-aware admission = the aggregate bound).

UPDATE 2026-09-05 — HALF (1) IS BUILT (backlog-remediation plan §4, Tier B). The blanket .aira- exemption is gone from all three sites: the classifier now decides `uncapped` purely from the finite-memory.max ancestry walk (classifyWatchdogCgroup, extracted from watchdogCgroupForPID as the testable seam), selectOffender's Uncapped predicate is just cgroup.uncapped, and revalidateWatchdogTarget no longer skips a still-uncapped .aira- target as 'cgroup-now-capped'; hasAIRAComponent is DELETED, so no site can re-introduce a name-based exemption without re-adding it. Nothing changes for a scope that IS bounded: an aira confine job under aira.slice (finite cap is a launch precondition, confine_linux.go:1476) and aitest's deliberately-uncapped .aira-supervisor/.aira-worker-* children of a capped outer scope (worker_admit.go:74) still classify capped via the finite ancestor. The residual uncapped AIRA class this actually unblocks, verified against source rather than taken from the AIRA-15 text: `aira run` .aira-<id> scopes, which join the name onto the CALLER'S ambient cgroup with NO finite-cap precondition (runner_linux.go:966; --memory-max optional) — conditionally, per build-review: run from inside a confine job (the normal path) such a scope inherits the finite ancestor and stays exempt, so only `aira run` from a bare shell under an uncapped user.slice is newly reachable. The confine unpinned-reserve fallback named in the original text (confine_linux.go:662) is uncapped only at the SCOPE level and is still bounded by aira.slice, so it was and remains exempt, correctly. ACCEPTED RESIDUAL surfaced by build-review and recorded in the spec rather than fixed: a LIVE confine job whose aira.slice memory.max transiently reads 'max' (slice re-creation after systemd GC, or a unit reload dropping MemoryMax) was protected only by the deleted name conjunct and is now a valid victim in that window — accepted because the only fix is a name exemption, i.e. this bug, and because during that window the job genuinely is unbounded. Design spec §2 predicate 1 carries the amendment. STILL OPEN: half (2), the slice-internal-pressure trigger — deliberately NOT built here; deferred to the AIRA-91 Part B owner decision (who kills what under slice-internal pressure: systemd-oomd or the AIRA watchdog), since building one answer would pre-empt that decision. This ticket stays open on half (2) alone.

## Note (2026-09-05): AIRA-103 shares this watchdog's MemAvailable primitive

Status-transition note only. **Half (2) — the slice-internal-pressure trigger —
remains OPEN and unbuilt, and this ticket is not reopened or rescoped.**

AIRA-103 (dynamic slice ceiling) is now a **sibling consumer of
`readMemAvailable`/`parseMemAvailable`** (`internal/daemon/watchdog.go:142`,
`:150`), reusing them rather than adding a second `/proc/meminfo` reader — so a
change to those functions now moves two subsystems. AIRA-103's system reserve is
`min(MemTotal/4, 16 GiB)`, the owner's existing `aira install` headroom policy,
which on a large box equals `watchdogRecoverMemAvailable`; that coincidence is a
deliberate sanity check (the throttle's *target* state must be one in which this
watchdog is quiescent), so retuning the kill thresholds should be checked against
it.

**It does not touch half (2), and does not disturb this ticket's recorded
accepted residual.** AIRA-103 is a preventive *admission* throttle responding to
memory consumed OUTSIDE the slice; half (2) is a *kill* decision under
slice-INTERNAL pressure. AIRA-103 writes no cgroup attribute at all — in
particular it never writes `aira.slice`'s `memory.max`, so `classifyWatchdogCgroup`
sees exactly what it saw before and the transiently-non-finite-`memory.max` window
recorded above is neither widened nor narrowed by it. (An earlier plan revision
that *would* have written that file was rejected partly for that reason.)

Cross-reference for the primitive split:
`docs/superpowers/specs/2026-08-23-aira-memory-watchdog-design.md` §10.
