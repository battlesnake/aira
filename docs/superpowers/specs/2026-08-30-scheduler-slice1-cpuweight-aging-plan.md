# Scheduler Slice 1 — cgroup cpu.weight aging (AIRA-14 residual fix)

- **Status:** DONE + MERGED (`3708af5`) + DEPLOYED + live-verified 2026-08-30. Closes **AIRA-14**.
  Owner-approved design (scheduler spec §3.1: cpu.weight decay).
- **Live verify:** a fresh confine scope carries `cpu.weight=100`, the parent's
  `cgroup.subtree_control` now propagates `cpu memory pids` (the `+cpu` delegation repair the
  Sol/Fable plan-review flagged as the #59 inert-class risk — proven live NOT inert), and the trailer
  honestly reports `cpu-weight=aging`. Binary swapped (supervisor/client-side only, no daemon restart);
  old binary backed up at `~/tmp/aira-pre-slice1`.
- **Two-loop:** Sol+Fable plan-review BLOCK (cpu not delegated → v2 folds `+cpu` best-effort +
  honesty facet + fail-open FD writer + named curve). Terra build → Sol build-review: implementation
  CORRECT, BLOCK on 4 POROUS tests → Terra hardened all 4 (revert-checked) → Opus verified
  discriminating + live. Mitigates mixed-age contention (the observed AIRA-14 case); simultaneous-fresh
  remains the daemon scheduler (Slice 2).
- Closed **AIRA-14** — the AIRA-11 CPU-side bootstrap residual (best-effort-7 fixed the I/O; the
  interpreter/import CPU still starves at a flat share under peak contention). Shipped first, kept the
  flock; the daemon scheduler (Slices 2–3) comes later.

## Problem

`ionice` best-effort-7 (AIRA-11) un-starved the execnet/import *I/O*, but under heavy multi-session
contention a fresh worker's bootstrap CPU (interpreter spin-up + thousands of imports) still starves:
every confined scope sits at the default cgroup `cpu.weight` (100), so with N contending scopes each
gets ~1/N of CPU and a *new* bootstrap gets no more than an old long-runner. Result: intermittent
`-n auto` bootstrap blanks (AIRA-14).

## Design (owner-decided: cgroup cpu.weight, not nice)

The confine supervisor **decays the scope's `cpu.weight`** over the job's lifetime:

- **Young = full share, old = yields.** A scope launches at `cpu.weight = 100` (the default, equal to
  the desktop's cgroups — NOT boosted above it, so a fresh heavy job never out-competes the desktop)
  and **decays toward a low floor** (e.g. `10`) over an aging schedule (`10s, 30s, 1m, 5m, 10m, 30m`).
  A young/bootstrapping scope at 100 therefore out-competes *long-running* scopes that have decayed to
  10 — so a fresh bootstrap gets CPU that a merge-gate hours in has yielded. `nice` stays 19 (threads
  share the scope's weight allocation; no per-descendant re-nicing — one cgroup knob covers workers
  execnet forks later).
- **Mechanism:** the supervisor (already alive for the job's duration) runs a timer goroutine that
  writes `<scope>/cpu.weight` at each aging step. A single small cgroup-file write per step; cheap.
  Set the initial weight at scope creation (before exec) so bootstrap starts at 100.
- **Honest scope:** this helps when contention is from *long-running* jobs (they decay, freeing CPU
  for young bootstraps) — the common case (various' "dozens of sibling worktrees", mixed ages). It
  does NOT help when contention is from many *simultaneously-fresh* scopes (all at 100) — that residual
  is the daemon scheduler's cooperative admission (Slice 2). State this; do not over-claim elimination.

## Invariants

- Never boost a young scope's `cpu.weight` **above** the desktop default (100) — desktop protection.
- The floor is > 0 (cpu.weight range is [1,10000]); a decayed scope still makes progress
  (anti-starvation of long jobs — they slow, never halt).
- Aging is monotone down; never re-raise a scope's weight (a long job doesn't reset to young).
- Fail-open: if the cpu.weight write fails (no cpu controller, permission), the job runs UNGOVERNED at
  whatever weight it has — never abort the launch. cgroup `cpu` controller may be absent; tolerate it.
- Crash-safe by construction: the weight lives on the scope cgroup; a dead supervisor just stops
  decaying (the scope keeps its last weight until reaped). No ledger, no reconstruction.

## Open decisions for the plan-review

- Exact start weight (100 vs a modest boost) + floor (10?) + the decay curve/steps — validate the
  young-weight desktop impact and that the decay is aggressive enough to matter under contention.
- Whether the initial weight is written by the child (RunConfineSetup, alongside nice/ionice) or the
  parent supervisor; the decay is the supervisor's.
- Config knobs (`AIRA_CONFINE_CPUWEIGHT_START/FLOOR/SCHEDULE`) vs compiled constants; env with a
  compiled-in default (never uncapped-behaviour on parse error).
- Does the aging apply to ALL confined jobs or only `--delegate-ram` suites? Lean: all (a long
  non-suite job should also yield); confirm no surprise for short one-shot commands.

## Test plan

- Unit: the aging schedule maps age→weight monotonically down to the floor; a parse-failed config
  falls back to the compiled default; weight is clamped to [1,10000].
- Real-cgroup: a fresh scope's `cpu.weight` == start; after an induced age step the supervisor has
  written the decayed value; a cpu-controller-absent scope runs fail-open (launch succeeds, no error).
- Discriminating: revert (no decay) → the "aged scope has floor weight" test fails.

## Deploy

Client/supervisor-side only (no daemon protocol change) → binary rebuild + swap, NO daemon restart.
Notify sessions (machine-wide confine behaviour: long jobs now yield CPU to fresh ones). Field-signal:
AIRA-14 bootstrap blanks under contention should drop further (not necessarily to zero — Slice 2 is
the simultaneous-fresh case).

## Plan-review round 1 — Sol BLOCK (Fable gate pending)

- **P0-1 cpu controller not delegated (confine_linux.go:258-300,372,463; install/assets/aira.slice.in:13).**
  Only `memory` is delegated to scopes; `aira.slice`'s `CPUWeight=50` weights the slice vs its
  SIBLINGS, not its scopes. A scope's `cpu.weight` write is commonly `ENOENT` → the mechanism is
  silently inert. → **v2: add a best-effort `+cpu` delegation (write `cgroup.subtree_control` on the
  scope's parent) before Create; every failure ignored/recorded, NEVER routed through the fail-closed
  `ensureDelegation`.** (Prereq: aira.slice must itself carry the cpu controller — check the slice unit
  delegation; may need `Delegate=` to include cpu, an install touch.)
- **P0-2 no supervisor for detached scopes (confine_linux.go:304-688; detach_linux.go:159-555).**
  Foreground `Confine` lives through `waitConfineCommand:661` (has the timer seam); the detached shim
  launches via `Runner` and never calls `confineWithDeps`/`RunConfineSetup`. → **v2: scope Slice 1 to
  foreground `aira confine` explicitly** (the pytest suites are foreground), OR factor a shared
  parent-side aging helper into both launch paths incl. `launchDetachedValidated`. Lean: foreground-only
  now; detached aging = follow-up.
- **P1 honesty + model.** cgroup model is CORRECT (sibling 100 vs 10 ≈ 10:1 of the parent's contested
  share; nice-19 distributes WITHIN the 100-weight scope, doesn't erase its share). But "100 = desktop"
  is wrong — the whole `aira.slice` is weight 50; and a young scope among many SIMULTANEOUS young scopes
  still gets 1/N. → v2: don't claim "closes" AIRA-14 (it's an honest mitigation vs LONG-running/decayed
  contention only; simultaneous-fresh is Slice 2); require a real contention/bootstrap regression test,
  not merely a weight-file assertion.
- **P1 residuals.** floor>0 is a positive proportional share for a FINITE runnable set — not
  anti-starvation under an unbounded stream of young scopes; a crashed supervisor freezes a scope at
  its last (possibly young=high) weight. → v2: state both as Slice-1 residuals.
- **P2 write site.** Write the initial weight PARENT-side right after `Create`, before `Start` (it owns
  the scope FD, fails open); keep `RunConfineSetup` child-local (nice/ionice only).

## v2 direction (fold round 1; finalise after Fable)

Enable `+cpu` delegation best-effort (the load-bearing prereq — verify aira.slice delegates cpu, add
if not); write the initial `cpu.weight` PARENT-side after Create; decay in the FOREGROUND-confine
supervisor only (Slice 1 scoped to `aira confine`; detached = follow-up); honest wording (mitigation,
not "close"); a real contention regression test; state the residuals (unbounded-young stream,
crashed-supervisor-freeze). If cpu delegation proves un-addable cheaply, reconsider whether Slice 1 is
worth shipping ahead of the daemon scheduler (Slice 2) — surface to owner.

## Plan-review round 1 — Fable GATE-FAIL (folds Sol) → v2 buildable

Fable confirmed the mechanism/seams/invariants/honesty sound and Slice 1 worth shipping (the observed
AIRA-14 profile IS mixed-age contention this fixes; simultaneous-fresh → Slice 2). GATE-FAIL solely on:

- **P0 — nothing enables `+cpu` on the scope's parent → cpu.weight often absent (the #59 inert class).**
  Live: `aira.slice/cgroup.subtree_control` FLAPS `memory pids` ↔ `cpu memory pids` (systemd re-realizes
  it); `cgroup.controllers` reliably has `cpu` (from `CPUWeight=50`), so the grant exists but nothing
  propagates it. Both delegation writers enable only `+memory` (`ensureConfineDelegation`
  confine_linux.go:258-293, re-asserted per-launch *because* systemd rewrites subtree_control;
  install.go:1498-1513). Tests green on a cpu-enabled box, silently inert elsewhere. → **v2: extend
  `ensureConfineDelegation` to also best-effort assert `+cpu` (cpu missing from `cgroup.controllers`, or
  the write failing → aging-`unavailable`, NEVER a new E_CONFINE_UNAVAILABLE; `+memory` stays
  fail-closed). Safe: aira.slice has no direct member processes (anchor in its own child). Real-cgroup
  test: a fresh scope HAS `cpu.weight` after the step.**
- **P1 — honesty facet.** `FormatConfineStatus` (confine_linux.go:684) must gain `cpu-weight=aging|
  unavailable` so a controller-absent/EACCES host is distinguishable (repo rule: unestablishable →
  `unavailable`, never a fake pass). Tests: fresh scope → `aging`; controller-absent → `unavailable`.
- **P1 — fail-OPEN decay writer, not the fail-closed model.** `writeScopeMemoryCap` (:839) is fail-CLOSED
  (caller aborts at :527-529). Use a DISTINCT FD-anchored writer like `writeScopeMemoryValue` (:864,
  `unix.Openat(scope.FD(),…)` — path-free, immune to scope-recreation races) tolerating
  ENOENT/ENODEV/EACCES mid-decay (teardown races the timer). Stop-and-JOIN the decay goroutine before
  `cleanupConfineScope`/`scope.Remove` — reuse the `monitorStop`/`monitorResult` (:644-674) seam.

## v2 — buildable

- **Delegation (P0):** `ensureConfineDelegation` best-effort `+cpu` (in addition to `+memory`); failure →
  aging-unavailable, not fatal.
- **Write:** PARENT-side initial `cpu.weight` between `backend.Create` (:463) and start (race-free; the
  child clones into the scope via CgroupFD :588). start=100 is the kernel default so it's semantically a
  no-op — keep it only to drive the honesty facet. Keep `RunConfineSetup` child-local (nice/ionice).
- **Decay:** a supervisor timer goroutine (foreground confine lives through `waitConfineCommand:661` for
  the whole job) writes the scope `cpu.weight` per step via the FD-anchored fail-open writer; stopped+
  joined before scope teardown.
- **Curve (P2, named):** `100→70→50→30→20→10` at `10s,30s,1m,5m,10m,30m`. Rationale: AIRA-14 contenders
  are merge-gate legs *minutes* old; by 1–5m they are 30→20 so a fresh 100 wins a solid majority share
  (fresh = 100/(100+Σdecayed)). Config `AIRA_CONFINE_CPUWEIGHT_{START,FLOOR,SCHEDULE}` with compiled
  defaults (parse error → compiled default, never inert-by-accident). Clamp [1,10000]; monotone-down.
- **Facet (P1):** `cpu-weight=aging|unavailable` in the confine trailer.
- **Honest scope + residuals (state all):** helps vs LONG-running/decayed contention (the observed
  case), NOT simultaneous-fresh (Slice 2); floor is a positive proportional share for a FINITE runnable
  set, not anti-starvation under an unbounded young stream; a SIGKILLed supervisor freezes a scope at its
  last (maybe young) weight; `aira run`/`--detach` scopes (per-project `run.cgroup_parent`) are NOT aged
  by Slice 1 — if parked under aira.slice they are never-decaying weight-100 siblings (accepted gap);
  weight arbitrates BETWEEN scopes only — a suite's 16 xdist workers inside one scope still share equally
  (intra-scope out of scope). Confine is foreground-only (Fable-verified) → no detached-confine gap.
- **Test plan:** real-cgroup fresh-scope-has-cpu.weight-after-delegation + facet=aging; controller-absent
  → fail-open launch + facet=unavailable; aging schedule monotone→floor (discriminating: revert decay →
  aged-scope-floor test fails); FD-anchored writer tolerates a removed scope (no abort). Fable: "fit to
  build" with these folded. Deploy: binary swap, NO daemon restart.
