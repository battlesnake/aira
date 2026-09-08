# `aira container run` — a restricted, admission-gated podman launcher

Status: **plan — not yet gated, not yet built.** Owner asked for a design,
not a build, matching tonight's AIRA-176 / AIRA-180 / AIRA-194 treatment.
Ticket: **AIRA-197**.

## 0. Problem, stated precisely

Owner (2026-09-09), verbatim:

> "Perhaps aira should have a subcommand for launching containers too (via
> podman, so aira can get them into the correct slice and stuff like that)?
> So agents can launch containers without having to care about docker/podman
> and stuff like that, the SKILL/MCP just gives them an interface with a
> vastly reduced set of options available (env/workdir/entrypoint/argv/
> mounts?), and the same interface as confine/run/kill for monitoring/
> signalling and injecting/closing stdin?"

The request bundles three separable asks, and this plan keeps them separate
because they have very different risk profiles:

1. **Placement + accounting** — a container's memory must land inside
   `aira.slice`, be charged to the admission ledger, and be killed with its
   job.
2. **An ergonomics face** — an agent should not have to know podman.
3. **Monitoring/control parity** — the same handle-based read/write/kill
   surface `confine`/`run` already offer.

### 0.1 One correction to the framing, made before anything is built on it

The brief states as a confirmed fact that "containers launched from inside a
confine job currently escape aira.slice's accounting entirely." **That is
true for docker and for every undetected invocation form, and false for the
one narrow case AIRA already handles.** AIRA-102 (merged 2026-09-05, commit
`ad94224`, PR #48) shipped exactly this: `internal/runner/container.go:428`
injects `--cgroups=split` into a detected `podman run`, nesting the container
as a real cgroup descendant of the job's own scope, and
`internal/core/skill.go:331` documents both halves ("Containers under
confine"). Building a new verb on the premise that *nothing* exists would
duplicate a shipped, twice-build-reviewed mechanism.

**What genuinely does escape — the real, still-open gap this ticket should
target:**

- **Every `docker run`.** Structural and unfixable from here: dockerd is a
  separate system-managed daemon, so the container is created in a different
  cgroup tree entirely (`container.go:29-31`, facet
  `ContainerPlacementDockerNotContained` at `container.go:58`).
- **Every podman invocation AIRA-102's detector deliberately does not
  match.** Detection fires *only* when `base(argv[0])` is literally `podman`
  and `argv[1]` is literally `run` (`container.go:19-31`). `podman-compose`,
  `podman container run`, any global-flag form (`podman --remote run`),
  `sudo podman run`, and anything inside a shell string (`sh -c "podman run
  …"`) get no injection **and no warning**. `skill.go:331` says so
  explicitly: *"do not read silence as 'this container is fine'."*
- **Every podman invocation not under `aira confine` at all.** Verified
  first-hand on this box: `podman info` reports cgroup manager **systemd**,
  and this session's own cgroup is
  `/user.slice/user-1000.slice/user@1000.service/app.slice/…`. With the
  default `--cgroups=enabled` and the systemd manager, podman asks the user
  session's systemd to create a transient scope for the container — placed
  under the user session, *not* under the caller's cgroup. So a bare `podman
  run` from an agent's shell lands outside `aira.slice` entirely, and no
  reserve, no cap and no `cgroup.kill` reaches it.
- **A caller who supplies their own placement flag.** If the argv already
  carries `--cgroups`, `--cgroup-parent` or `--pod`, AIRA injects nothing and
  reports that it cannot establish containment (`container.go:141`
  `NestsInJobScope`, facets at `container.go:55-58`).

So the honest problem statement for AIRA-197 is: **AIRA-102 made the
*narrowest possible* container invocation accountable, and deliberately
refused to widen the detector. A first-class launch verb is the way to widen
coverage without widening a fragile string-matcher** — because a verb AIRA
*constructs* the argv for cannot be mis-detected at all.

## 1. What already exists to build on (verified from source)

**Container analysis, pure and portable.** `internal/runner/container.go`
(609 lines) already owns: runtime detection, a boundary-proved memory-flag
scan, `NestsInJobScope()` (`:141`) as the single nesting predicate, `Inject`
(`:408`) which builds the effective argv, `ResolveReserve` (`:470`) which
decides whether a declared container limit changes the ledger charge, and a
6 MiB injection floor (`containerMemoryFloor`, `:47`) measured against both
runtimes. A new verb reuses this file rather than reimplementing it.

**Podman's actual placement surface, verified first-hand** (`podman run
--help`, podman **4.9.3** on this box — not taken from the AIRA-102 plan's
prose):

| flag | meaning |
|---|---|
| `--cgroups string` | `"enabled"`\|`"disabled"`\|`"no-conmon"`\|`"split"` (default `enabled`) |
| `--cgroup-parent string` | "Optional parent cgroup for the container" |
| `--cgroupns string` | cgroup namespace to use |
| `--cgroup-conf strings` | configure cgroup v2 (key=value) |
| `-m, --memory <number>[<unit>]` | memory limit |

**AIRA's own measurements against that surface** (AIRA-102 plan,
`docs/superpowers/specs/2026-09-05-aira102-container-integration-plan.md:32-41`,
recorded as live probes on podman 4.9.3 rootless — I did **not** re-run
them; see §5 open question 1):

- **F1** — `podman run --cgroups=split` places the container at
  `<confine-scope>/[runtime/]*libpod-payload-<id>`: a genuine cgroup
  descendant of the job's scope, charged to that job's `memory.max`.
- **F2** — split relocates the job's *own* processes one level deeper per
  `podman run`, so confine then reports `scope-integrity=migrated`.
- **F3** — `--cgroups=split` is mutually exclusive with `--cgroup-parent`.
- **F5** — scope teardown *does* kill the nested container (`cgroup.kill` is
  recursive); podman's state DB is left stale (`podman ps` shows `Up`).
- **F6** — `--cgroup-parent=aira.slice` works, but makes the container a
  **sibling** bound only by the whole-slice aggregate cap, and it **survives
  the job's exit**. This is the "cosmetic placement" failure mode the brief
  asks about by name.
- **F9** — `--cgroup-manager=cgroupfs --cgroup-parent=<scope path>` **fails**
  (OCI runtime error) *because the scope leaf already held processes*:
  cgroup v2's no-internal-process rule blocks enabling `memory` in a
  non-empty cgroup's `subtree_control`.
- **F11** — after split, the job's leaf `cgroup.procs` reads `POPULATED 0`
  for a *running* job, which AIRA-74's post-restart reserve reconstruction
  skips.

**The placement primitive AIRA already uses for its own children.**
`internal/runner/confine_linux.go:1174` sets
`SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}` — Go's binding to
`clone3(CLONE_INTO_CGROUP)`, which places the child in the named cgroup
*atomically at clone time, before exec*. This is why `--cgroups=split` works
at all: podman (rootless, forking the container itself) nests relative to the
cgroup **it is already running in**, and `CgroupFD` put it there. Docker
cannot use this path in principle — the CLI is not the container's parent.

**The drain-then-delegate primitive that makes F9's failure conditional, not
fundamental.** `BootstrapAitestSupervisor`
(`internal/runner/aitest_bootstrap_linux.go:16-37`) relocates the supervisor
PID out of the outer scope into a child, **then** writes `+memory` to the
outer scope's `cgroup.subtree_control` — its doc comment names the exact rule
F9 tripped over: *"cgroup v2 forbids a cgroup from delegating controllers to
children while it still holds member processes of its own."* And
`CreateWorkerScope` (`internal/runner/worker_scope_linux.go:57`) then creates
one empty, capped child per aitest worker (`memory.max`,
`memory.swap.max=0`, `memory.oom.group=1`). **AIRA already builds
kernel-enforced, empty, sized sub-scopes under a job's outer scope.** That is
directly relevant to §2.

**The restricted-argument precedent — and its honest limits.**
`confine`'s dispatch entry (`internal/core/core.go:1718-1752`) and
`parseConfineArgs` (`cmd/aira/main.go:788-839`, allow-listed via
`confineLaunchOptionValueless`/`confineLaunchOptionTakesValue` with a
did-you-mean suggestion) are the house pattern for *a closed vocabulary of
AIRA-owned control options plus an opaque trailing `argv`*. `run` is the same
shape (`core.go:1520-1552`). **But in both, the closed vocabulary constrains
only AIRA's containment; the wrapped program itself is unconstrained.** The
only place AIRA narrows a *sub-surface* is gate's command checker:
`env-allow` is default-deny (`core.go:2028`), and `resolveCommandCwd`
(`internal/store/gate_command.go:194-214`) symlink-resolves both the root and
the candidate, `filepath.Rel`s them, and refuses any `..`-escape with
`E_GATE_INVALID: command cwd escapes evaluation root`.

**Monitoring/control, as it actually stands.** There is **no kind-agnostic
job record**. `RunRecord` (`internal/runner/types.go:117`) and
`ConfineDetachRecord` (`internal/runner/confine_detach.go:77`) are two
parallel, purpose-built types with different output shapes (`OutputRefs` map
vs. flat `StdoutPath`/`StderrPath`) and different state machines. The verbs
are split across layers too: `run-log`/`run-input` (`core.go:1884`,
`core.go:1678`) are full `core.Do` dispatch verbs with MCP tools
(`aira_run_output`, `aira_run_input`); `confine-status`/`confine-list`/
`confine-kill` (`core.go:1786`, `:1818`, `:1826`) are registered for help and
tool-schema purposes but their `Run` closures return
`E_CONFINE_UNAVAILABLE` — the real implementation lives in `cmd/aira/main.go`
against the daemon transport. **`confine-log` and `confine-input` do not
exist**; AIRA-196 is the (filed, unbuilt) ticket to add them.

## 2. Placement: can podman be told to put a container inside `aira.slice`, for real?

Three candidate mechanisms, ranked. **This is the section a gate should push
hardest on**, because the difference between a real reservation and a
cosmetic one lives entirely here.

### 2.1 Option A — reuse `--cgroups=split` under a real confine scope (recommended for v1)

`aira container run` creates a normal confine scope exactly as `aira confine`
does today (real admission, real `memory.max`, real `oom_score_adj`,
`CgroupFD` placement of the podman CLI at `confine_linux.go:1174`), then
launches an AIRA-*constructed* `podman run --cgroups=split …` argv into it.

- **Real, not cosmetic.** F1: the container is a kernel-enforced descendant of
  a scope whose `memory.max` came from the job's own admission grant. The
  slice ledger charge is the job's single existing charge — the container's
  memory is *already inside* what the job reserved, which is precisely why
  `ResolveReserve` (`container.go:470`) declines to raise it a second time.
- **Kill and teardown already work.** F5: `cgroup.kill` is recursive, so
  `confine --kill` and scope teardown reach the container.
- **Zero new placement machinery.** The whole mechanism is already built,
  reviewed and shipped.
- **What it costs.** F2's per-run `runtime/` nesting (job reports
  `scope-integrity=migrated`), F4's `ENOTEMPTY` scope-removal lag, and F11's
  `POPULATED 0` reserve-reconstruction gap are inherited **as-is**. None is
  new; all must be named in the verb's own docs rather than rediscovered.

### 2.2 Option B — an AIRA-created, empty, pre-sized container sub-scope + `--cgroup-parent`

The interesting possibility the AIRA-102 record does **not** close. F9 failed
for a *stated, conditional* reason — the target leaf held processes — and
`BootstrapAitestSupervisor` (§1) is the proof AIRA already knows how to
dissolve exactly that condition: drain the leaf, delegate `+memory`, create
an empty capped child (`CreateWorkerScope`), then hand podman
`--cgroup-parent=<that empty child>`.

If it works, it is strictly better than A: the container gets its **own**
named, individually-sized cgroup that AIRA created and can read, cap, and
kill independently of the job's other processes, with no `runtime/`
relocation of the job's own tree (no F2, no F4, no F11).

**It is a hypothesis, not a finding.** Three things are unverified and must be
measured before this option is chosen:

1. Whether rootless podman with the **systemd** cgroup manager honours a
   `--cgroup-parent` pointing at a cgroupfs path AIRA created (as opposed to
   a systemd slice unit name). The AIRA-102 probe used
   `--cgroup-manager=cgroupfs` precisely to sidestep this, and *that* is what
   hit F9.
2. Whether switching to `--cgroup-manager=cgroupfs` per-invocation has side
   effects on the user's podman state (F9 noted it mutates `subtree_control`
   as a side effect) or splits podman's own bookkeeping across managers.
3. Whether the container survives the job's exit, as it does under F6's
   sibling placement. If AIRA owns the parent cgroup it can `cgroup.kill` it
   on teardown regardless — but that must be demonstrated, not assumed.

**Recommendation: build A in v1, and file B as a measured follow-up** with a
committed reproduction, rather than gating v1 on an unproven mechanism.

### 2.3 Option C — `--cgroup-parent=aira.slice` (rejected)

F6 measured it: the container becomes a **sibling** under the slice, bound
only by the aggregate cap, and it survives the job's exit. It would look
placed while being unreserved, unkilled and unattributed — the exact cosmetic
outcome the brief warns against. Reject explicitly so nobody re-proposes it.

### 2.4 What makes the reservation *real* rather than cosmetic

Four properties, each of which the verb must be able to attest or report
`unevaluated`:

1. **Admitted before launch.** The container's memory is inside a scope whose
   `memory.max` came from a daemon admission grant (or a caller-declared
   `--memory-max`). If admission is refused, nothing is launched and `ran=no`
   is printed, exactly as confine does today.
2. **Bounded by the kernel, not by podman.** The job's scope cap is an
   ancestor bound; F8 showed a container `-m` above it is accepted and the
   ancestor binds. AIRA reports both numbers and never claims the smaller is
   the one the caller set.
3. **Killed with the job.** F5, via recursive `cgroup.kill`.
4. **Attested, not asserted.** Following `container.go:50`'s discipline, the
   trailer names the **action taken** (`split-injected`), never an outcome
   AIRA did not observe (`nested`). A new verb *can* do better than
   AIRA-102 here — because it controls the launch, it can **read back** the
   payload cgroup's `memory.max` after start (F7 confirmed the value lands
   there) and upgrade the facet from `asked-for` to `observed`. Whether v1
   does that read-back is §5 open question 3.

## 3. Proposed design

### 3.1 Verb shape

**`aira container run [AIRA options] --image IMG [-- <argv…>]`**, CLI-first,
implemented alongside confine in `cmd/aira/main.go` against the same daemon
transport, with a dispatch-table entry in `internal/core/core.go` for
generated help and the agent guide.

It is **podman-only, by construction**. Docker is not a supported backend
and the verb refuses rather than silently degrading: there is no honest way
to place a dockerd-spawned container in `aira.slice` (§0.1), so offering
docker here would be the "looks confined and is not" failure AIRA-102 exists
to prevent. If podman is absent, the verb refuses with a stable code — it
never falls back to docker.

The verb **wraps confine's own launch path**, so every existing bound is
inherited verbatim rather than reimplemented: `--memory-reserve`,
`--memory-max`, `--memory-high`, `--timeout`, `--cpu-timeout`,
`--admit-timeout`, `--name`, `--owner`, `--slice`, `--detach`. That reuse is
the single most important architectural decision in this plan, and the
`architectural-simplicity` rule points straight at it.

### 3.2 The restricted argument surface — and why it is a *new class* of interface

The owner's proposed vocabulary is `env / workdir / entrypoint / argv /
mounts`, plus `--image`. **This is architecturally different from every
existing AIRA verb, and the plan says so rather than claiming precedent.**
`confine` and `run` wrap *any* argv with zero opinion about what the wrapped
program may do; their closed option sets constrain only AIRA's own
containment. This verb instead constrains **the capabilities of the wrapped
tool** — no `--privileged`, no `--cap-add`, no `--device`, no `--network`, no
`--userns`, no arbitrary `-v`, no `--security-opt`, no raw `--cgroups`/
`--cgroup-parent`/`--pod` (which would defeat §2 placement outright).

Proposed v1 surface:

| option | shape | notes |
|---|---|---|
| `--image` | string, required | passed verbatim; AIRA does not parse, pull or validate it |
| `--entrypoint` | string | maps to podman `--entrypoint` |
| `--workdir` | string | maps to podman `--workdir`; a path *inside* the container, so not subject to §3.3 |
| `--env K=V` | repeatable | **default-deny**, following gate's `env-allow` (`core.go:2028`): the container's environment is *not* inherited from the caller unless named |
| `--mount SRC:DST[:ro]` | repeatable | §3.3 |
| `-- <argv…>` | opaque trailing | the command inside the container |

Enforcement follows `parseConfineArgs` (`main.go:788-839`) exactly:
allow-listed names, refusal of anything else, did-you-mean suggestion
(AIRA-182). The default-deny environment is a deliberate departure from
`aira run`'s inherit-plus-override model, and it is the *safer* default for a
container that may outlive the caller's shell.

**The escape hatch question is a real fork and is deferred to the gate**
(§5 open question 4). A verb with no way to pass an unlisted podman flag will
eventually be worked around with a bare `podman run`, which is worse than a
narrow, loudly-labelled passthrough. But an unrestricted
`--podman-arg=<raw>` re-admits `--cgroups`, `--privileged` and `--network`
through the back door and hollows the whole design out. Neither is obviously
right; this plan refuses to settle it unilaterally.

### 3.3 Mount safety — reuse gate's containment discipline, do not invent one

`--mount SRC:DST[:ro]` is the one option that grants the container reach into
the host filesystem, and it is where a "vastly reduced set of options" can
still be catastrophic. There is **no existing generalized path-attestation
primitive** to reuse — `confine --owner` (`core.go:1722`) is a cooperative
*identity* string for kill/steal arbitration, not a path fence.

The closest real precedent is `resolveCommandCwd`
(`internal/store/gate_command.go:194-214`), and v1 should mirror it exactly:

1. `filepath.EvalSymlinks` **both** the containment root and the resolved
   mount source (symlink resolution before the comparison is the whole point
   — a symlink out of the root is the obvious bypass).
2. `filepath.Rel(resolvedRoot, resolvedSource)`; refuse when the result is
   `..` or is `..`-prefixed.
3. `os.Stat` the result and refuse a non-existent source, rather than letting
   podman create it.
4. Refuse with a stable code — `E_CONTAINER_MOUNT_ESCAPES_ROOT` or the
   established `E_..._INVALID` shape — naming the resolved path, never a
   silently-dropped mount.

**What the containment root should be is an open question** (§5 open question
2). Candidates: the project worktree root (`.aira/config`'s directory) — but
the verb is otherwise project-less like confine, and `skill.go` states
plainly that confine launch/list/kill are "machine-local and project-less";
the caller's `--workdir`/cwd; or an explicit `--mount-root`. Picking wrong
makes the verb either unusable inside `~/tmp` scratch work or a fence with a
gate that anybody can move.

Two further mount rules v1 should adopt and a gate should confirm:

- **`:ro` is the default**, `:rw` explicit. A read-only mount is the common
  case (source in, artefacts out via one named writable path) and the
  asymmetry of blast radius justifies the asymmetry of defaults.
- **Refuse the obvious host-control sources unconditionally** — `/`,
  `/proc`, `/sys`, `/dev`, `/run`, the podman/docker socket paths, and the
  user's `~/.ssh`, `~/.config` and `~/.claude` — *even if* they sit inside a
  permissive containment root. A `--mount /var/run/podman/podman.sock:…`
  hands the container the runtime itself, which is a full escape of
  everything above.

### 3.4 Admission and accounting

No new ledger path, no second daemon lease. The container job is **one**
admission, taken by the wrapping confine launch, exactly as today:

- Caller-declared `--memory-max`/`--memory-reserve` → authoritative, unchanged.
- Otherwise → the daemon's history-derived estimate for the job's resource
  signature.
- The container gets `--memory=<declared cap>` injected only when the caller
  **declared** one, never when AIRA merely estimated it. `container.go:408`'s
  doc comment records exactly why this distinction is load-bearing: injecting
  a resolved estimate was a build-review **P0** that produced a container
  OOM-killed forever inside a limit AIRA invented, with a kill invisible to
  the job's own trailer, and therefore **no self-healing**. That reasoning
  transfers to this verb unchanged and must not be re-litigated.
- The 6 MiB floor (`containerMemoryFloor`, `container.go:47`) applies: below
  it, podman refuses to start the container at all, so AIRA declines to
  inject and says so.

**One genuinely new accounting concern this verb introduces:** the signature
used for the peak-RSS estimate. Under AIRA-102 the signature is derived from
the wrapped argv, whose first token is `podman` — so *every* container run on
this box shares one history bucket, and the estimate is a mean of unrelated
workloads. A first-class verb can do better by signing on the **image plus
entrypoint** rather than on `podman`. Worth doing, but it is a change to
`runner.ResourceSignature` semantics and therefore §5 open question 5.

### 3.5 Monitoring, signalling, stdin — extend confine's verbs, add no new job kind

**The research's recommendation, which I endorse with one amendment.** The
recommendation was: make the container launch an *argv choice* under `aira
confine --detach`, so it produces an ordinary `ConfineDetachRecord`
(`confine_detach.go:77`) and rides the existing verbs with zero extension —
and explicitly avoid a second record shape or kind-tagged polymorphism, which
would fight `architectural-simplicity`.

That is right, and §3.1's "wrap confine's own launch path" is exactly it: the
container job **is** a confine job, whose argv AIRA happened to construct.
`confine --status`, `confine --list` and `confine --kill` therefore work on
day one with no changes, and `ResolveConfineDetachStatus`
(`confine_detach.go:300`) needs no new selector.

**My amendment: the stdin half of the owner's ask does not exist yet, on
either side.** The owner asks for "injecting/closing stdin", but
`ConfineDetachRecord` has no stdin conduit at all — stdin is hardwired to
`devnull`, and `confine-log`/`confine-input` are unbuilt (AIRA-196, filed
2026-09-09). So:

- **AIRA-197 does not build a stdin conduit.** It **depends on AIRA-196**,
  and inherits `confine-log`/`confine-input` for free the moment that lands.
  Building a container-specific stdin path first would create the second
  parallel record type both this plan and AIRA-196 exist to avoid.
- **AIRA-197 must not talk to podman's own `logs`/`attach` API.** That would
  bypass confine's file-capture and `cgroup.kill` model and force the
  kind-tagged polymorphism the research warns against. Output capture is
  confine's existing `StdoutPath`/`StderrPath`, full stop.
- **Signalling stays `cgroup.kill`, never `podman stop`.** F5 established the
  recursive kill reaches the container; `podman stop` would leave the job's
  own supervisor untouched and split the kill across two authorities.

**One honest consequence to document, not paper over:** F5 also found podman
leaves its state DB stale after a `cgroup.kill` — `podman ps` shows `Up` for
a container whose processes are gone. AIRA's answer is authoritative about
*processes and memory*; podman's `ps` is not, after an AIRA kill. Say so in
the verb's own help text rather than letting an agent discover it.

### 3.6 MCP and Skill integration

Standard dispatch-table pattern, with one deliberate asymmetry that follows
existing precedent rather than inventing a policy:

- **`container run` is CLI-only** — dispatch-table entry registered so it
  appears in generated help and the agent guide, `Include` unset so it is
  never an MCP tool. This matches `confine` (`core.go:1718`, whose `Run`
  returns `E_CONFINE_UNAVAILABLE`), `confine-status` (`:1786`) and `drain`,
  and `core.go:1760` records the reason: a foreground, connection-bound
  launch has no honest request/response form — a tool could only return
  before the job began (a fabricated success) or block a dispatcher for the
  length of the job.
- **Read and terminal verbs are MCP tools**, matching `confine-list`
  (`core.go:1821`, `aira_confine_list`, `SafetyRead`, `Include: true`) and
  `confine-kill` (`:1831`, `aira_confine_kill`, `SafetyExecute`,
  `Destructive: true`, `Include: true`). If a container-specific listing verb
  is wanted at all (§5 open question 6), it takes that shape.
- **SKILL text** extends `internal/core/skill.go:331`'s existing "Containers
  under confine" section rather than adding a competing one. The section
  already tells agents "use `podman run` for anything that needs to be
  contained"; the amendment is to point at `aira container run` as the
  supported way to do that without knowing podman, while **keeping** the
  existing warning that the raw-`podman-run`-under-confine path has a
  deliberately narrow detector.
- **No JSON/MCP schema work** beyond the dispatch entry: the pattern is
  already automatic from the arg specs.

### 3.7 Explicitly not in v1

- Docker support of any kind (§3.1).
- `--network` in any form. A container with host networking is a different
  security posture and needs its own design.
- Pods, compose, `build`, `exec`, image pulling/management, or any podman
  verb other than `run`.
- Option B's AIRA-created container sub-scope (§2.2) — a measured follow-up.
- Any stdin conduit (§3.5) — AIRA-196's job.
- Any auto-adjustment of the caller's declared limits.

## 4. Honesty obligations

Following `container.go:50`'s stated discipline, every facet names an action
AIRA took or an observation it made, never an outcome it assumed:

- `container=podman:split-injected` (or `:parent-placed` under a future
  Option B) — what AIRA asked for.
- A **separate** facet for what AIRA *observed*, if §5 question 3 is answered
  yes: the payload cgroup's read-back `memory.max` and its path.
- `unevaluated` wherever a read failed — never a zero, never a silent
  omission.
- The verb must never print a containment claim on a path where placement was
  not established. An absent or pre-2.0 podman rejects `--cgroups=split` and
  exits; that is a loud failure, and the trailer must not have already
  claimed nesting.
- `ran=no` semantics on a refused admission are inherited from confine
  verbatim (`skill.go`'s "ran=no appears on no other line").

## 5. Open questions for the plan-gate (deliberately not settled here)

1. **Re-verification of F1/F3/F6/F9.** Those measurements are recorded in-repo
   as live probes on podman 4.9.3 rootless (AIRA-102 plan line 14) and I
   verified podman 4.9.3 and its flag surface first-hand, but I did **not**
   re-run the probes. A gate that intends to build Option B specifically must
   re-measure F9 under the drain-then-delegate precondition (§2.2), because
   that is the finding this plan reinterprets.
2. **The mount containment root** (§3.3) — worktree root, cwd, or an explicit
   `--mount-root`? Interacts directly with the verb's project-less-ness.
3. **Post-launch read-back of the payload cgroup** (§2.4) — does v1 upgrade
   the facet from "asked for" to "observed", and what does it report when the
   read races a fast-exiting container?
4. **Escape hatch or none** (§3.2) — a labelled `--podman-arg` passthrough,
   an allow-list extension mechanism, or a hard refusal that accepts agents
   will sometimes fall back to bare `podman run`?
5. **Resource signature** (§3.4) — does the verb sign on image+entrypoint
   rather than on `podman`, and is that a `runner.ResourceSignature` change
   or a verb-local one?
6. **Does a `container list` verb exist at all**, or does `confine --list`
   simply show container jobs like any other confine job (with the container
   facet rendered)? The latter is simpler and is my inclination.
7. **Detached container lifetime.** `skill.go:331` warns that a podman
   container started with `-d` under confine is killed when the job exits.
   Under this verb `-d` is not in the vocabulary at all — but `aira container
   run --detach` means "the *job* is detached", which is a different thing,
   and the naming collision will confuse someone. Worth a decision on wording.
8. **Ordering against AIRA-196.** §3.5 makes AIRA-197 depend on it for the
   stdin/log half. A gate should confirm that ordering rather than letting
   AIRA-197 build a stopgap.

## 6. Status

Plan only. Nothing is built; no code was changed by this document. AIRA-197 is
filed against it, related to AIRA-196 (stdin/log dependency) and AIRA-102 (the
shipped narrow mechanism this widens).
