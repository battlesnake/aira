# AIRA whale-run confinement face — design

Status: PLAN v1 (pre-review). Milestone task #54. Owner-selected (2026-08-21)
after TUI v2 (#53): fold the agentmux `whale-run` heavy-job confinement into
AIRA, so heavy commands run **admitted-first** in an AIRA-managed, memory-capped
cgroup scope with a **last-resort OOM-kill backstop kept** (owner: "keep an OOM
kill limit for the slice as an absolute last-resort … so whale can move to aira
eventually"). Constrained by the no-compat rule (AIRA is not live).

## 1. What whale-run does today, and what AIRA already has

`whale-run <cmd>` (`~/repos/claude/cmd/agentmux/whale.go`) runs
`systemd-run --user --scope --slice=whale.slice --property=OOMPolicy=kill --
sh -c 'echo 500 > /proc/self/oom_score_adj; exec nice -n 19 ionice -c 3 "$@"'`.
So a transient **scope** under a hard-capped **slice** (`whale.slice`:
`MemoryMax`, `MemorySwapMax=2G`, `CPUWeight/IOWeight=50`, `Delegate` so the cap is
enforced), with per-scope **OOM-kill** (`OOMPolicy=kill` ⇒ one OOM tears the whole
scope down, no half-dead fleets) + deprioritisation. It is dead-simple,
low-latency, daemon-free, and **catches nothing on the way in — it just caps and
kills** (there is no admission; a job that won't fit still starts and may OOM).

AIRA's runner already does the harder half: it creates an owned cgroup-v2 scope
(`.aira-RUN-N` under a configured parent) and places the child via
**clone3 + CLONE_INTO_CGROUP** (`cgroup_linux.go:189-193`,
`runner_linux.go:423`), captures `memory.peak`, kills via `cgroup.kill`, and —
the value whale lacks — **admits before launch** (RAM-aware `#29`
`admission_linux.go:103`, peak-RSS reserve estimate `#50`
`core.go:1499-1514`), fail-open when the daemon is down (flock fallback
`admission_linux.go:118-221`).

**The gap (headline map finding):** AIRA **sets no limits or priorities** — it
writes no `memory.max`/`memory.high`, no `memory.oom.group`, no `oom_score_adj`,
no `nice`/`ionice`. The cap it admits against is presumed to be provided by an
external systemd slice. **So the hard slice cap + OOM-kill do not exist inside
AIRA today; whale.slice provides them.**

## 2. Scope (v1) — reuse the external capped slice; add the confinement value

v1 does **not** make AIRA install or own a slice (AIRA has no install face and
writes no systemd/delegation today — that is a large, root-touching follow-up).
Instead v1 **reuses an external capped+delegated slice** as the confinement
parent — `whale.slice` initially, via the runner's **already-working**
`run.cgroup_parent`/`run.slice` config (`project.go:64-74`, used exactly this way
by #50). The hard `MemoryMax` + `+memory` delegation stay owned by that external
slice — **that is the owner's last-resort backstop, unchanged.** AIRA adds:

1. **A slim confinement verb** — `aira confine [--slice S] [--name N] -- <argv…>`
   (name TBD in review; `aira confine`/`aira sh`/`aira x` are candidates). It
   launches `<argv>` through the runner's minimal quartet — `backend.Probe` →
   `r.admit` → `backend.Create` → `exec.Cmd{SysProcAttr:{UseCgroupFD,CgroupFD}}` →
   `Start` (`runner_linux.go:322-465`) — **without** the heavy per-run ledger
   event chain (`starting`/`scope-created`/`running`/`wait-observed`), telemetry
   wiring, or git-context stamping that `aira run` does. This keeps it
   whale-run-fast; `aira time` (`command.go:301`) is the lightness precedent.
   Exit code and signal are propagated; stdio is inherited (a foreground wrapper,
   not a captured run).
2. **Per-scope confinement writes**, applied right after `backend.Create`
   (`runner_linux.go` ~412, before `Start`) where delegation permits:
   - **`memory.oom.group = 1`** on the `.aira-RUN-N` scope — the cgroup-v2
     equivalent of `OOMPolicy=kill`: an OOM in the scope kills the *whole* scope.
     **This is the OOM-kill backstop, now at scope granularity.**
   - **`oom_score_adj = 500`** on the leader (child self-writes `/proc/self/
     oom_score_adj` before exec — the proven whale pattern — so it is set before
     the process can grow) and inherited by descendants.
   - **`nice -n 19` + `ionice -c 3`** so the desktop preempts cleanly (applied via
     the same pre-exec step, or `setpriority`/`ioprio_set`).
   These are **best-effort and honest**: if the parent lacks `+memory` delegation
   the `memory.oom.group` write fails and is reported as `unenforced`, never
   silently assumed (mirrors #50's honest-nil-peak discipline).
3. **Admit-first as the primary mechanism.** The verb admits via #29 (+#50
   reserve) before launching, so a job that won't fit **waits** rather than
   starting and OOMing. Admission is **fail-open** (owner: never block a heavy
   command): daemon-down → flock fallback; flock timeout/unevaluated → launch
   anyway, confined — the slice cap + `memory.oom.group` are the net. A job never
   fails to run because admission could not be established; it only ever *waits*
   up to `AdmissionMaxWait`, then proceeds confined.

The composition: **admit-first keeps you off the cap in normal operation; the
external slice `MemoryMax` + per-scope `memory.oom.group` are the absolute last
resort.** Exactly the owner's model.

## 3. Honesty and failure modes

- **Uncapped / undelegated parent** — if `run.slice`'s `memory.max` is `max`/
  unreadable or `+memory` is not delegated, admission is honestly `unevaluated`
  and the job launches ungated (as today), and the `memory.oom.group` write is
  reported `unenforced`. The verb **surfaces the enforcement status** (capped &
  delegated vs not) rather than implying a backstop that is not there.
- **Daemon down** — confinement + OOM-group still apply (cgroup writes, not
  daemon-dependent); admission degrades to the flock fallback. Fully functional.
- **Bootstrap** — building AIRA itself uses `whale-run`. v1 is **additive**:
  `whale-run` keeps working unchanged; `aira confine` is a new, parallel path.
  Migrating call-sites (and optionally making the `whale-run` shim `exec aira
  confine` when AIRA is installed) is a later, separate step — no chicken-and-egg.

## 4. Faces

CLI-only in v1 (it is a shell-confinement primitive; it inherits stdio and
propagates exit status — not a structured `core.Do` result). It is a
`RouteClient` verb (executes client-side like `run`/`time`; needs the daemon only
for cross-client admission fairness, and degrades without it). No MCP/Skill
surface in v1 (an agent that wants a *recorded* run uses `aira run`; `aira
confine` is the lightweight "just run this heavy thing safely" path). The
dispatch-table descriptor is added so generated help lists it.

## 5. Testing (real-cgroup, honest)

Real-cgroup tests gated by `cgrouptest.IsolatedScopeParent` + `AIRA_REAL_CGROUP=1`
(the #33 harness), each a discriminating case:
- **`memory.oom.group=1` is written** on the scope when the parent delegates
  `+memory` (read it back); and an OOM in a child kills the whole scope (a
  two-child fixture where one OOMs → both die), proving the backstop.
- **`oom_score_adj=500`** is set on the leader (read `/proc/<pid>/oom_score_adj`)
  and inherited by a child.
- **admit-first gates**: with a slice whose free < reserve, `aira confine` waits;
  with free ≥ reserve it proceeds. Fail-open: daemon-down still admits via flock;
  an unevaluable slice launches ungated (not blocked).
- **honest unenforced**: a parent without `+memory` delegation → the oom.group
  write is reported `unenforced` and the run is not claimed as capped (red vs a
  silent assume-enforced).
- **low overhead / no ledger**: `aira confine` writes no RunRecord/ledger events
  (assert the ledger is untouched) — distinguishing it from `aira run`.
- **exit-status + signal propagation**: the child's exit code and a killing
  signal surface as the verb's exit status.
Pure/unit: the argv/priority/oom-group plumbing where separable from real cgroups.

## 6. Deferrals (explicit follow-ups, each its own milestone)

1. **AIRA owns the slice** — an `aira install` face writing an `aira.slice`
   (`MemoryMax`, `MemorySwapMax`, `CPUWeight/IOWeight`) + the `+memory`
   delegation drop-in (root), modelled on whale's install and on AIRA's existing
   machine-wide CPU-slots pool (`daemon/cpuslots.go`). This is the phase where
   AIRA fully owns the cap and whale.slice can retire.
2. **Per-run `memory.max`/`memory.high` scope caps** (#50 deferral #3) — a
   per-command cap in addition to the slice cap.
3. **Fold whale layers 2–3** — the bypass-job watchdog and the systemd-oomd
   backstop — into AIRA.
4. **Migrate `whale-run` call-sites** + the `whale-run`→`aira confine` shim.

## 7. Risks

- **Reusing whale.slice couples v1 to an external unit** — acceptable and
  intended (it is the owner's kept backstop); AIRA reads its cap for admission
  and writes only within its own child scope. The AIRA-owned-slice follow-up
  removes the coupling.
- **`memory.oom.group` needs `+memory` delegation** — handled by reusing the
  already-delegated whale.slice; honestly reported `unenforced` otherwise.
- **Foreground stdio/exit semantics** — v1 is a thin foreground wrapper; getting
  signal/exit propagation right is the main implementation care (covered by §5).
- **Scope creep into the install/root work** — explicitly fenced to the deferrals.

## 8. Two-loop plan

Sol plan-review (inline) + Fable code-gate (verify the quartet-reuse, the
`memory.oom.group`/delegation facts, the fail-open admission, and that no
install/root work sneaks in) → fold → owner reviews the committed spec → Terra
build (TDD, self-review) → Sol build-review → Sol confirm → Opus real-HW verify
(build/vet/`CGO_ENABLED=0`/test ×2/`-race`, real-cgroup cases proven red) → merge.
