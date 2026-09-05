---
{"schema":1,"id":"AIRA-102","project":"aira","title":"Docker containers structurally escape aira.slice — no admission accounting, no aggregate memory cap","status":"in-progress","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["confine","docker","dogfood","memory-safety"],"hold":false,"relations":[]}
---
Prompted by the owner asking whether dockerised jobs can be placed into `aira.slice` for accounting/memory limits, and independently confirmed by peer session `field` from a live benchmark (structural reasoning) and by the coordinating session (empirical test against this actual machine). This ticket records the safety finding; the accounting/benchmarking question itself is a separate, harder design question left for a follow-up decision (see bottom).

## The finding, verified empirically on this machine (2026-09-05)

Docker's cgroup driver here is **systemd** (`docker info` → `Cgroup Driver: systemd`), and `dockerd` runs as root under the **system** manager's tree (`/system.slice/docker.service` — PID 1's systemd, not the user session's). `aira.slice` is a **user**-session slice, delegated to and managed by `user@1000.service`'s own systemd instance (`/user.slice/user-1000.slice/user@1000.service/aira.slice`). These are two different cgroup-delegation authorities; a system-managed daemon has no natural path into a user-delegated subtree via systemd's own unit model.

Consequence, confirmed by direct test:

- `aira confine -- docker run ...` only confines the `docker` **CLI client** — a thin process that just talks to the daemon's socket. The actual container process is spawned by containerd as a child of `dockerd`'s own cgroup, never a descendant of anything `aira confine` placed. Confining the client changes nothing about where the container's memory is charged.
- `docker run --cgroup-parent=<raw path into aira.slice>` fails outright: `runc create failed: ... invalid slice name` — the systemd driver requires a slice **unit name**, not a filesystem path, and resolves it against the **system** manager.
- `docker run --cgroup-parent=aira.slice` (bare name) **appears to succeed and is actively misleading**: the system manager, finding no such system-level unit, silently **creates a brand-new top-level `/aira.slice`** — same name, completely unrelated cgroup, no relation to and no cap inherited from AIRA's real (user-session) `aira.slice`. The container's own `--memory` flag is honoured (Docker sets that on the container's own cgroup regardless), but the aggregate slice has no cap, and AIRA's daemon has no visibility into it at all: not in the admission ledger, not in any `aira confine --list` accounting, not reachable by any of tonight's worker-admit or CPU-slots governance. A confusingly same-named decoy slice was left behind by this exact test (`/sys/fs/cgroup/aira.slice`, 0 tasks, harmless but real — left in place; a `systemctl stop aira.slice` to clean it up is correctly refused by this session's own tooling as indistinguishable from the banned "stop the shared real aira.slice" operation, so it wants a human's `sudo systemctl stop aira.slice` run directly, understanding this targets the SYSTEM unit, not the real user-session slice of the same name).

## Why this is a real, current exposure, not a hypothetical

**34 containers are running on this machine right now**, none inside `aira.slice`: per-workspace `postgres`/`minio` pairs (many "agent-*" and "social-*" prefixed sets, several 4 days old — look like standing dev-environment services, not transient jobs), a `buildx` builder, and at least one live benchmark container (`fastest-rf-solve:spike`, matching `field`'s own reported RF-solver benchmark work). None of this is bounded by AIRA's 64G ceiling or visible to its admission accounting. A runaway container — a benchmark that leaks, a build that balloons — can OOM the desktop directly, which is precisely the failure class the whole `aira confine`/watchdog/oomd-tuning effort exists to prevent, and none of it reaches containers at all.

## Not decided here — three directions, increasing effort, not scoped or built

1. **Document + advise only.** Containers are out of AIRA's containment scope; anyone running one should set their own `--memory`/`--cpuset-cpus` on the `docker run` invocation directly and manually leave headroom below AIRA's ceiling. Zero new code. Leaves the exposure real, just informed.
2. **Detect-and-warn.** `aira confine` sniffs its own argv for `docker run`/`docker compose` (and maybe `docker exec`) and prints an honest warning that confinement will not reach the container. Cheap, does not fix the exposure, but stops it from being silently assumed covered — the same "must-know, fail-visible rather than silently degrade" principle `field` argued for on AIRA-101.
3. **A real companion mechanism.** The only way to get an actual kernel-enforced cap on dockerised work requires either (a) switching Docker's cgroup driver machine-wide to `cgroupfs` so `--cgroup-parent` can take a raw path AIRA creates and manages directly under its own tree — a disruptive, machine-wide `dockerd` reconfiguration that would restart every container on the box (the 34 running now, several apparently standing dev infrastructure) — not something to do casually on a shared machine; or (b) AIRA creates and owns its own **system-level** companion slice (a genuinely new unit, distinct name to avoid the exact collision this test just caused) that `docker run --cgroup-parent=<that-name>` can legitimately target, with `aira install` doing the (already-privileged) systemd unit installation and AIRA's admission ledger extended to also scan and charge against that second slice. Real, buildable, but a second cap domain to design and reconcile with the existing one, not a small change.

## A working alternative found and verified: rootless Podman (2026-09-05)

Owner asked whether a different runtime could be used to RUN containers
(keeping `docker build` for prod parity) specifically so confinement works.
Installed `podman` (4.9.3, Ubuntu universe) and tested empirically —
**it works, for the structural reason Docker cannot**: Podman has no
persistent root daemon; `podman run` (rootless, as the invoking UID) forks
the container process directly and, even with the same `systemd` cgroup
manager Docker uses, resolves `--cgroup-parent` against the **user**
systemd manager (`user@1000.service`) because Podman itself runs
unprivileged in that session — the same manager that already owns the real
`aira.slice`. No cross-authority-domain mismatch, therefore no decoy.

Verified directly:
- `podman run -d --cgroup-parent=aira.slice --memory=64m alpine sleep 30` landed at
  `/user.slice/user-1000.slice/user@1000.service/aira.slice/libpod-<id>.scope`
  — a REAL child of the real slice, `memory.max=67108864` matching the flag exactly.
- The "build with docker, run with podman" workflow works: `docker save
  <image> | podman load` transfers a docker-built image into podman's
  separate image store (podman's own `docker-daemon:` transport hit an
  unrelated API-version mismatch between this podman/docker pair — 1.41 vs
  minimum 1.44 — but save/load is the portable, version-independent path
  and is standard practice anyway). Ran the transferred image's real
  entrypoint under podman with `--cgroup-parent=aira.slice`; it executed
  the actual application code (hit an unrelated missing-input error,
  confirming the code ran, not a compatibility failure).

**What this gets for free, with zero AIRA code changes:** kernel-enforced
CONTAINMENT — a podman container launched this way is a genuine cgroup
descendant of `aira.slice`, so its memory rolls into the slice's aggregate
`memory.current` and cannot collectively exceed the slice's `memory.max`/
`memory.high`, enforced by the kernel regardless of any userspace code.

**What still needs AIRA-side work, not built here:**
1. AIRA's admission scanning (the code that decides whether to admit a NEW
   job) does not yet recognize `libpod-*.scope`/`libpod-conmon-*.scope`
   children the way it recognizes `.aira-worker-*`/`.aira-CONFINE-*` —
   whether the EXISTING raw-`memory.current` safety checks already catch
   this incidentally (AIRA-29 notes some admission paths already charge
   `max(effectiveCurrent, ...)` against real slice usage) needs verifying
   against current source before claiming coverage either way.
2. `--cgroup-parent=aira.slice` places a container as a SIBLING of any
   `aira confine` job's own scope, capped only by the slice's full 64G
   ceiling — not nested inside one specific job's own `--memory-max`
   reservation. Whether `--cgroup-parent` can target a job's own confine
   scope directly (so a container counts against THAT job's admitted
   grant specifically, not just the slice at large) was not tested here.
3. No CLI/wrapper convenience exists yet (e.g. `aira confine` deriving and
   exporting the right `--cgroup-parent` value for a job automatically,
   or a documented recipe) — right now this requires a caller to know the
   bare slice name and construct the invocation by hand.

This changes the option menu above: a fourth, much less disruptive
direction now exists — recommend/wire Podman as the supported runtime for
anything that needs AIRA's containment, leaving Docker (and its 34
currently-running containers) completely undisturbed, rather than either
touching Docker's global cgroup driver or standing up a second slice.

## Owner-directed remedy (2026-09-05) — two-part, now being built

**Part 1 — Podman transparent integration, the preferred path.** When
`aira confine [flags] -- podman run ...` is detected, `aira confine`
manages podman as necessary so containment "just works" without the
caller needing to know about `--cgroup-parent` or any podman-specific
detail: derive and inject the right `--cgroup-parent` automatically
(ideally nested inside THAT job's own confine scope, not just the bare
slice — resolve the open question above about whether podman/systemd
support that nesting; fall back to the bare-slice form, already verified
working, if per-job nesting proves infeasible), and reconcile podman's own
`--memory`/`-m` the same way Part 2 describes for docker.

**Part 2 — Docker sanity shim, for stragglers who still invoke `docker run`
directly.** This does NOT fix the structural escape (a docker container
still will not be nested in `aira.slice` — that finding is unchanged) —
it is explicitly a best-effort consistency/accounting improvement, and its
own output must say so plainly rather than imply real containment (the
exact "silent success masking failure" trap `field` warned about must not
be recreated by this shim itself). When `aira confine [flags] -- docker
run ...` is detected:
- confine has an explicit memory limit, docker's own argv has none →
  inject the equivalent `--memory=<value>` into the wrapped docker
  invocation, so at least the container's own individual cap matches what
  the caller asked confine for.
- docker's argv already specifies `--memory`/`-m`, confine has no explicit
  limit → use that value to inform an admission-ledger reservation (the
  existing `confine-reserve`-shaped mechanism, or its internal equivalent),
  so AIRA's own accounting is not blind to a footprint it already knows
  about.
- both specify limits and they disagree → needs a plan decision: this
  project's own fail-closed-over-fake-pass discipline argues for refusing
  rather than silently picking one, but the plan should weigh that against
  usability and say which it chose and why.
- neither specifies a limit → still emit the same honest warning as an
  otherwise-undetected `docker run` would get, since injecting nothing
  changes nothing about the underlying escape.

**Both parts need a real design pass, not just described here — flagged as
non-trivial themselves:**
- Parsing another program's argv for its memory flags accurately (`--memory=4g`
  vs `--memory 4g` vs `-m4g` vs `-m 4g`, flags before/after other tokens,
  not misparsing an unrelated flag's value) without building a full
  docker/podman CLI parser — the plan should pick a deliberately narrow,
  conservative approach and report `unevaluated`/refuse on genuine
  ambiguity rather than guess, matching this project's existing honesty
  discipline elsewhere.
- Scope is `docker run` / `podman run` only — `compose`/`podman-compose`
  and any invocation where the runtime is hidden inside an opaque shell
  string (`sh -c "docker run ..."`) are explicitly OUT of scope; detection
  only fires when `docker`/`podman` is literally the wrapped argv's own
  `argv[0]` after confine's `--` separator.
- Whatever confine does here must be visible on its own trailer output
  (what runtime was detected, what was injected or reserved, or that
  nothing could be established) — matching the `terminated-by=` precedent
  (AIRA-70/91 Part A) for "the tool tells you honestly what it actually
  did," not a silent behind-the-scenes rewrite.

## Relation to AIRA-101 and AIRA-100

Distinct from **AIRA-100** (build subprocesses spawned transiently *inside* a test body, invisible to aitest's worker-pool governance) — this is about **standalone, often long-lived** containers with no governing job at all, and the exposure is unbounded aggregate rather than one over-permissive worker. Also relevant to **AIRA-101** (exclusive slice access for benchmarking): even a working exclusive-slice mechanism cannot and will not exclude a running container, since containers are structurally outside `aira.slice` regardless — AIRA-101's own design should say this plainly rather than let a user assume exclusivity covers containers too.
