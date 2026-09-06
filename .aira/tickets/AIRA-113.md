---
{"schema":1,"id":"AIRA-113","project":"aira","title":"Dynamic per-scope oom_score_adj steering for the residual aggregate-full slice OOM","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","deferred-from-aira29","oom","scheduler"],"hold":false,"relations":[]}
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

## Resolution (in-review)

Built per AIRA-29 §3.6, sequenced after AIRA-114's aggregate bound. Branch
`aira113-dynamic-oom-score-steering`, off `ba83566`.

### What the mechanism actually is

A daemon subsystem (`internal/daemon/oomsteer.go`) on its own **250 ms** cadence. Each tick
it reads the slice's `memory.current` and `memory.stat` and asks one question: is the
aggregate genuinely full? Only when it is does it snapshot the admission ledger, read one
`memory.current` per charged scope, and raise a scope whose live usage is past its ledger
charge to `oom_score_adj` 1000 — restoring it to its AIRA-27 class baseline when it comes
back inside its charge, when the slice drains, or when it leaves the ledger.

The cadence is the reason this could not be a term in `evaluateAdmitQueue`, exactly as §3.6
said: inside that pass the charge is derived from the same reading the trigger would use, so
the trigger is near-inert there. `AIRA_DAEMON_OOM_STEER_INTERVAL` therefore **refuses** any
value at or above `admitConfineScanIntervalDefault` rather than accepting a cadence the loop
cannot outrun, and a test pins that.

### The arithmetic that makes this worth building

`oom_score_adj` is worth `adj/1000` of MACHINE total in kernel badness. On a 64 GiB box the
delegate class's 800 outweighs the non-delegate 500 by ~19 GiB of virtual badness, so a
compliant `--delegate-ram` suite at 20 GiB (score 71) outscores a non-delegate job that has
burst to 30 GiB past what admission accounts for (score 62) — the kernel kills the
COMPLIANT neighbour. At 1000 the offender scores 94 and is picked. The static class bias
alone cannot express "this one is the offender", which is why a graduated or class-preserving
band was rejected: a raise that stays under the neighbouring class's baseline cannot flip the
victim, and flipping the victim is the whole requirement.

### Decisions taken, with reasons

1. **Two-level, not proportional.** The kernel already ranks by RSS; the adj only has to
   lift a proven offender above the compliant band, after which the kernel's own RSS
   ordering breaks ties between offenders. A proportional curve is more machinery for a
   weaker steer.
2. **Never below a scope's own class baseline**, whatever `steeredAdj` is configured to.
   AIRA-27's bias is containment this may sharpen and must never weaken; a misconfigured
   steer value under a class baseline is clamped up, not applied, and a test pins it.
3. **Fullness on the NON-reclaimable footprint** (`sliceCeilingAnon`), not raw
   `memory.current`: page cache is dropped before any OOM, so steering on raw usage would
   raise the adj of healthy jobs during every large build. With hysteresis (90% enter, 80%
   exit) so a scope cannot flap between 500 and 1000 several times a second.
4. **Against the kernel-enforced `memory.max`, deliberately NOT `admitEffectiveMaximum`.**
   AIRA-103's published ceiling is a figure admission believes in; the OOM this steers is a
   kernel event at the real cap. The two gates measure different things and are allowed to
   disagree here.
5. **The budget sums an aitest parent's `confine-reserve` children into the parent.** A
   `--delegate-ram` suite's own waiter charges the pinned framework overhead while its
   `memory.current` is HIERARCHICAL and already contains everything its per-test
   sub-reservations allocated. Without this the most compliant job on the machine reads as
   the offender on every full slice — the AIRA-29 build-review double-book, from the other
   direction. Pinned by its own test and its own mutant.
6. **Recursive subtree walker** (`runner.SetSubtreeOOMScoreAdj`), because `Members()` is
   leaf-only and cgroup-v2's no-internal-process rule means a scope with children has no
   pids of its own — a leaf-only walker would steer exactly zero processes for the aitest
   population most likely to be over-committing. Real-cgroup test with processes in the
   scope root, a child and a grandchild.
7. **PID-reuse guard**: each write re-checks `/proc/<pid>/cgroup` for the scope directory as
   a whole path element before writing, so a pid recycled between the `cgroup.procs` read
   and the write is skipped rather than handed a 1000 it never earned. Fail-closed.
8. **Modes off/observe/enforce, default OFF**, like the watchdog and the slice ceiling: this
   writes `/proc` for processes it does not own, so "off is exactly today's behaviour" must
   stay true, and a malformed interval must not refuse to start a daemon that never asked
   for the subsystem. **Deploy is the coordinating session's, not this build's.**

### Tests

The load-bearing one is `TestRealCgroupOOMSteerFlipsTheFavouredVictimToTheOffender`: a real
cgroup-v2 slice with a real `memory.max`, two real `.aira-CONFINE-*` scopes of DIFFERENT
AIRA-27 classes, real allocating processes placed before they allocate, the production
readers, the production ledger snapshot and the production `/proc` walker. The scenario is
stacked AGAINST the fix — the compliant neighbour is the BIGGER scope — and the BEFORE state
is read out of `/proc` and shown to favour the compliant one, so the AFTER assertion is not a
tautology. It then asserts the restore against the same live processes.

Everything else is the seam tier plus a real-cgroup walker test. **Mutation battery: 18
deliberate breakages, 18 caught, 0 survivors.** The battery found one genuine gap — removing
the PID-reuse guard failed no test — fixed by a second commit rather than written down as
accepted.

### Residuals, stated rather than papered over

- Adopted (post-daemon-restart) scopes are not steered: the ledger keeps them as an
  aggregate, not per scope, and an adopted scope's charge is re-derived from its own
  `memory.current` every pass, so it cannot read as over-budget by construction.
- A scope raised and still alive when the daemon stops keeps 1000 for the rest of its life.
  Safe direction (it demonstrably outran its accounting), but a real asymmetry.
- A scope whose charge is still its frozen estimate rather than an observation can read as
  over-budget while it ramps — the sub-second window between a grant and the first
  admission scan. Self-corrects within one scan interval, and both the raise and the restore
  are logged.
- Only the default confine slice is steered, exactly as AIRA-103's ceiling governs only that
  one.

### Deferred, deliberately

No `aira install --oom-steer` flag or doctor line was added, unlike `--watchdog` and
`--slice-ceiling`. The subsystem ships default-off and an operator enables it with a systemd
drop-in — which, per AIRA-111, actually SURVIVES a later `aira install` where a baked unit
value does not. Adding the flag means the unit template, the doctor report and their tests,
and belongs to the rollout change rather than this one.

### Verification

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0

### Done

**PR / merge**: https://github.com/battlesnake/aira/pull/62, merged as merge commit
`47061ed0c75fa84b6efc4fafc1fef5f1a88bd70f` into `master` (branch tip `61e6a02`).

**Independent build review (Fable gate)**: re-ran from a clean detached worktree —
`go build ./...` exit 0, `go vet ./...` exit 0, `AIRA_REAL_CGROUP=1 go test ./... -count=1`
exit 0 (14 packages), plus `-race` on the steer tests; all real-cgroup steer tests ran (none
skipped). An independent 13-mutant battery (no-raise, leaf-only walker, no children sum, no
class floor, no pid guard, raw-current fullness, no hysteresis, no restore-on-leave, frozen
reserve budget, 1s interval accepted, raise-everyone-when-full, hardcoded-500 restore,
restore-on-unevaluated) was caught 13/13. Verdict MERGE. Non-blocking P3 notes: the COST
comment says the ledger is snapshotted only when full but `budgets()`/`classAdj` run every
tick (cheap, comment inaccurate); for a memcg OOM the kernel's badness scale is the OOMing
cgroup's `memory.max`, not MemTotal (direction of the flip unaffected); the steer is
edge-triggered, so a process that rewrites its own adj after a raise keeps it until the next
transition (forks inherit, so narrow). Accepted coverage gap, shared with the ceiling and
watchdog: no `Serve`-level test that the enforce env var starts the loop.
