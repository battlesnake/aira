# AIRA whale-run confinement face — design

Status: PLAN v2 (Sol plan-review r1 CHANGES-NEEDED + Fable code-gate r1
GATE-PASS-conditional folded; **owner decisions 2026-08-21**: (1) the slice/RAM
limit is **system-wide, not per-project** — the goal is to stop dev work from any
project/session from OOMing the box, modelled on agentmux's `whale.slice`; (2)
admission is **simple** — check the slice's RAM usage vs the slice's RAM cap,
nothing more). Milestone task #54. Constrained by no-compat (AIRA is not live).

## 1. The model (whale today, and what AIRA adds)

agentmux confines heavy jobs with **one machine-wide user slice** `whale.slice`
(hard `MemoryMax` sized to leave desktop headroom — `--memory-max` > installed >
2/3 physical RAM; `MemoryHigh = Max − 2G`; `MemorySwapMax=2G`;
`CPUWeight/IOWeight=50`; `+memory` propagated so the cap is enforced). `whale-run
<cmd>` runs the job as a transient **scope** under that slice via
`systemd-run … --property=OOMPolicy=kill … sh -c 'echo 500 >
/proc/self/oom_score_adj; exec nice -n 19 ionice -c 3 "$@"'`. It **caps + kills
but does not admit** — a too-big job still starts and may OOM before the cap
bites; whale relies on the watchdog + oomd (layers 2–3) to catch that.

AIRA already has the missing half — **admit-before-launch** (#29 RAM-aware, #50
peak-RSS reserve) — and owns cgroup-v2 scopes (clone3 + CLONE_INTO_CGROUP), but
it currently **sets no limits or priorities at all** (map: no
`memory.oom.group`/`memory.max`/`oom_score_adj`/`nice`/`ionice` writes; the cap is
assumed external). This milestone folds whale's layer-1 confinement into AIRA: a
`whale-run`-fast verb that runs a heavy command **admitted-first** in a scope
under the **machine-wide capped slice**, with the per-scope OOM-kill + deprioritise
that whale applies. The hard slice `MemoryMax` + OOM behaviour is the owner's kept
last-resort backstop; admit-first keeps jobs off it.

## 2. Scope (v1) — machine-wide slice, admit on slice-vs-cap, reuse whale.slice

- **Machine-wide slice, project-less.** `aira confine -- <argv…>` works in **any
  directory** (no `.aira/config` required), confining under one machine-wide
  slice. The slice is a **machine-level default** — `whale.slice` initially —
  overridable by `--slice`/`AIRA_CONFINE_SLICE`; there is no per-project slice
  here. (AIRA owning its own `aira.slice` + an `aira install` face that sizes and
  delegates it — replacing whale.slice — is the explicit follow-up, §6.)
- **Simple admission = slice usage vs slice cap** (owner decision 2). Admit when
  the slice's `memory.max − memory.current ≥ reserve` (exactly #29
  `admission_linux.go`), with the #50 peak-RSS reserve estimate. Cross-session
  fairness via the daemon queue (#40) when it is up so concurrent `confine` jobs
  from different sessions don't collectively over-commit the shared slice; the
  per-slice **flock** fallback self-gates when the daemon is down. **No host-PSI /
  host-`MemAvailable` gate and no fail-closed refusal** — the conservatively-sized
  slice cap is the host-safety boundary, and the watchdog + oomd (layers 2–3)
  remain the extra host-level net, unchanged (see §3 for the deliberate
  simplification vs Sol's host-headroom finding).
- **A new slim entry point, not `aira run`.** `confine` composes the runner's
  quartet — `backend.Probe` → `admit` → `backend.Create` → `exec.Cmd{SysProcAttr:
  {UseCgroupFD,CgroupFD}}` → `Start`, then release-admission-after-Start — with
  `aira time`'s **foreground** machinery (inherit stdio, forward signals, map exit
  status incl. 128+signal). It writes **no ledger/run-record**, does no telemetry
  or git-context, and synthesises a non-ledger scope id — so it is `whale-run`-fast
  (Launch itself is not trimmable; the pieces are). A **per-call** runner/backend
  is built so `--slice` takes effect per invocation.
- **Daemon-optional.** `confine` **bypasses the mandatory `ensure-scope` daemon
  exchange** (which otherwise hard-fails client verbs when the daemon is down,
  `dispatcher.go:147-158`) so it always runs; the daemon is used only as the
  admission-fairness optimisation, with the flock fallback otherwise.
- **CPU governor** env (`AIRA_CPU_SLOTS_DIR`, …) is injected into the child (as
  `run`/`time` do) — confined heavy jobs are exactly the pytest fleets the #49
  governor caps, so they compose.

## 2.1 Confinement writes (per scope), honestly verified

Applied to each `.aira-RUN-N` scope after `backend.Create`, before `Start`
(Fable live-probed these succeed unprivileged under whale.slice):

- **`memory.oom.group = 1`** on the scope. This is the cgroup-v2 memcg
  guarantee that an OOM *inside the scope* kills the whole scope's tasks together
  (no half-dead fleets). It is described accurately as **the narrower guarantee**
  it is (Sol P1): it is *not* a full synonym for systemd `OOMPolicy=kill` — it has
  `oom_score_adj=-1000` exemptions and does not reproduce the manager-level
  reaction; the systemd-oomd layer remains the broader net.
- **`oom_score_adj = 500`** on the leader + **`nice -n 19` + `ionice -c 3`**,
  applied by an **exec-self confine-setup helper** (Sol P1): AIRA re-execs itself
  in a `--confine-setup` mode that sets `oom_score_adj`/`setpriority`/`ioprio_set`,
  reports success through a **handshake pipe**, then `exec`s the target. AIRA
  **claims enforcement only if the handshake confirms it** — never a best-effort
  `|| true` that silently no-ops. (Go has no safe arbitrary pre-exec callback;
  exec-self is the cgo-free, `/bin/sh`-free, verifiable pattern.)
- These writes need the `+memory` controller delegated on the parent; whale.slice
  provides it (propagation, not a `Delegate=` on the slice — Fable §1 correction).
  Absent delegation, the `memory.oom.group` write fails and is reported
  **`unenforced`**, never silently assumed.

## 3. Honesty and the deliberate simplification

- **Gate = slice-vs-cap** (owner). If the slice's `memory.max` is a finite number,
  admit against it. If it is `max`/unreadable (the slice is not actually capped —
  which must not happen in the deployed model where whale.slice/aira.slice carries
  the cap), admission is honestly **`unevaluated`** and the job launches; the
  confinement writes still apply best-effort and the enforcement status is
  surfaced. AIRA never fakes a cap it did not read.
- **Owner-accepted simplification vs Sol's host-headroom P0.** Sol argued
  `slice.max − slice.current` can admit while the *host* is near-OOM. The owner's
  design accepts this: the machine-wide slice cap is **sized conservatively**
  (whale: 2/3 physical RAM) precisely so "fits in the slice" ≈ "won't OOM the
  host", and the watchdog + oomd remain the host-level backstops. v1 therefore
  gates on slice-vs-cap only; a host-`MemAvailable`/PSI co-gate is recorded as a
  clean future option, not built.
- **Never blocks** (owner's earlier ask): admission only ever *waits* up to
  `AdmissionMaxWait`, then proceeds; daemon-down degrades to flock; there is no
  path that fails a launch because coordination was unavailable.

## 4. Faces

CLI-only in v1 (a shell-confinement primitive: inherits stdio, propagates exit
status — not a structured `core.Do` result). `RouteClient` + daemon-optional. A
dispatch-table descriptor is added so generated help lists it. No MCP/Skill (an
agent wanting a *recorded* run uses `aira run`; `confine` is "just run this heavy
thing safely, anywhere").

## 5. Testing (real-cgroup, honest; gated by AIRA_REAL_CGROUP=1)

- **`memory.oom.group=1` written** on the scope (read back) under a `+memory`-
  delegated parent; an OOM in one child **kills the whole scope** (two-child
  fixture, one OOMs → both die).
- **oom_score_adj/nice/ionice via the handshake**: the setup helper reports
  success and `/proc/<pid>/oom_score_adj`==500 (inherited by a child); a forced
  handshake failure → reported `unenforced`, enforcement **not** claimed.
- **Admit slice-vs-cap**: slice free < reserve → waits; ≥ reserve → proceeds;
  daemon-down still admits via flock; an uncapped slice → `unevaluated`, launches.
- **Never-blocks**: a timeout proceeds (launches), never errors.
- **Foreground semantics**: child exit code + a killing signal surface as the
  verb's exit status; stdio is inherited.
- **No ledger**: `confine` writes no RunRecord/ledger events (assert untouched) —
  distinguishing it from `aira run`.
- **Project-less**: runs with no `.aira/config` present.

## 6. Deferrals (each its own milestone)

1. **AIRA owns the slice** — `aira install` writing an `aira.slice` (sized
   `MemoryMax`/`MemoryHigh`/`MemorySwapMax`, `CPUWeight/IOWeight`) + the `+memory`
   delegation drop-in (root), modelled on agentmux's installer and AIRA's
   machine-wide CPU-slots pool. Replaces the whale.slice dependency; lets
   whale.slice retire.
2. Optional host-`MemAvailable`/PSI co-gate (Sol's host-headroom option).
3. Per-run `memory.max`/`memory.high` scope caps (#50 deferral #3).
4. Fold whale layers 2–3 (watchdog, oomd) into AIRA.
5. Migrate `whale-run` call-sites + a `whale-run`→`aira confine` shim.

## 7. Risks

- **External whale.slice dependency** — v1 depends on an externally-managed slice
  for the cap + delegation; preflighted each launch and reported honestly;
  removed by deferral #1.
- **`memory.oom.group` narrower than `OOMPolicy=kill`** — described accurately;
  oomd remains the broader net (§2.1).
- **Foreground stdio/exit + the setup-helper handshake** are the main
  implementation care (§5 covers both).
- **Host-headroom simplification** — owner-accepted (§3); watchdog/oomd remain.

## 8. Two-loop plan

Sol r1 + Fable r1 folded → v2 with owner decisions. Owner reviews this committed
spec; a Sol r2 confirms the folds. Then Terra build (TDD, self-review) → Sol
build-review → Sol confirm → Opus real-HW verify (build/vet/`CGO_ENABLED=0`/test
×2/`-race`, real-cgroup cases proven red) → merge.
