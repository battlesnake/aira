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

### 0.2 A second owner requirement, added 2026-09-09 after the first draft

Owner, verbatim:

> "the command for running containers (via podman) should transparently map
> the aira socket in too, so jobs in the container can use aira run/confine
> and get correct results (--delegate-ram also working fine)."

This is **inward** coordination — AIRA reaching *into* the container — where
everything above §0.2 is **outward** placement. It is investigated in full in
§3.7, and its headline is that the requirement splits into a half with a
concrete mechanism and a half that is structurally blocked by cgroup-namespace
isolation. That split is load-bearing for what v1 may honestly promise, so it
is stated in §3.7 before any of the detail.

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

### 3.7 Mapping the AIRA socket into the container (owner requirement §0.2)

**Headline finding, stated first because it is not the answer the requirement
hoped for.** The requirement is *two* asks wearing one sentence, and they have
opposite outcomes:

- **`aira run`, and every daemon-side verb, are reachable from inside the
  container with a concrete, fully-specified mechanism** (§3.7.1). The socket
  path is deterministic, bind-mountable, and derived from nothing the container
  cannot be told. This part is a real v1 requirement and is specified below.
- **Nested `aira confine` inside the container is structurally blocked** under
  podman's cgroup-v2 default `--cgroupns=private`, and so is everything
  `--delegate-ram` actually *buys* (§3.7.2). Confine does all of its cgroup work
  client-side; inside a private cgroup namespace the client cannot see, name, or
  write the host cgroup it would have to be placed in. **This is not a bug to
  route around in v1 and it is not fixable by mounting the socket.**

The blocked half fails *honestly* — `E_CONFINE_UNAVAILABLE` with reason
`slice-not-found` (`internal/runner/confine_linux.go:505-513`), i.e. a refusal to
launch, never a fabricated containment claim. That is the correct behaviour and
it is why this section does not propose a workaround: a container job that
*silently* fell back to an unconfined launch would be exactly the "looks confined
and is not" failure §3.1 rejects docker for.

**Everything below was established by reading source. No podman container was
launched: this environment can run static analysis and can read the installed
podman's flag surface, but cannot exercise a container's cgroup namespace.**
The three questions that genuinely require a live container are isolated in
§3.7.3 and are *not* asserted either way here.

#### 3.7.1 Verified: the socket is reachable, and the mechanism is concrete

**Socket path predictability — verified, and better than hoped.**
`daemon.PathsFromEnvironment` (`internal/daemon/paths.go:478-509`) derives the
entire daemon identity from exactly three inputs: `XDG_STATE_HOME` (defaulting to
`$HOME/.local/state`, `:482-484`), `XDG_RUNTIME_DIR`, and `HOME`. The socket is
`<XDG_RUNTIME_DIR>/aira/<sha256(canonical state home)[:16]>/daemon.sock`
(`:498-507`). **There is no uid, hostname, PID, boot id or cgroup input** — so the
path is fully deterministic and can be bind-mounted at an identical path inside
the container. Four verified caveats, each of which the verb must handle rather
than discover:

1. **Canonicalisation must agree.** `canonicalPath` (`paths.go:511-541`)
   `EvalSymlinks`es the state home before hashing. If the container sees a
   *different resolved* path for the same state home, the stateID hash diverges
   and the client dials a socket that does not exist. Bind-mounting the resolved
   host path at the same path inside is the fix, and it is a requirement, not a
   convenience.
2. **Podman does not inherit the caller's environment**, so the verb must inject
   `XDG_RUNTIME_DIR`, `XDG_STATE_HOME` and `HOME` explicitly. **This collides
   head-on with §3.2's default-deny `--env`** (the rule at plan §3.2, modelled on
   gate's `env-allow`, `core.go:2028`). The resolution is an *AIRA-owned
   exception*, not a hole in the rule: these three are injected by AIRA as part
   of the socket wiring, are not caller-supplied, and are not caller-overridable.
   A caller `--env XDG_RUNTIME_DIR=…` must be **refused**, not merged — otherwise
   the caller can redirect the client at an arbitrary socket.
3. **Permissions constrain the container's user.** The socket is chmod `0600`
   (`internal/daemon/server.go:446`) inside a `0700` runtime dir (`:374-377`).
   Rootless podman maps in-container root to the host caller's uid, so the
   default works; **`--user <non-root>` maps to a subuid and gets `EACCES`.** If
   the verb ever grows a `--user` option it must refuse it alongside socket
   passthrough, or say plainly that the socket will be unreachable.
4. **The AF_UNIX 107-byte path cap still applies** inside the container exactly
   as outside (`server.go:263-265`, checked at `:367`), and the bind-mount does
   not shorten it.

**The container must be prevented from auto-spawning a daemon — this is a real
hazard, not a hypothetical.** `cmd/aira/dispatcher.go:510-531` unlinks a socket it
judges stale (`:524`) and then forks one (`:529`, `spawnDaemon` at `:83-105`).
Inside a container `daemon.ServiceIdentityMatches` fails (no user systemd), so it
falls through to `exec.Command("/proc/self/exe", "daemon")` (`:95`) — which, with
the host state home bind-mounted, would **unlink the host's live socket and start
a second owner of the host's `state.db`**. The single-writer daemon design is the
whole point of AIRA's DB ownership, so the verb must guarantee the in-container
client never takes this path. **There is no existing no-spawn knob** — a grep of
the `AIRA_DAEMON_*` surface (`internal/daemon/paths.go:110-402`) finds none — so
one must be added, and adding it is a *prerequisite* of this feature rather than
a detail of it. Fail-closed: absent the knob, the container must not get the
socket.

**Binary availability — a currently-broken assumption, found by checking rather
than assuming.** CLAUDE.md's "one static Go binary" is the **release target, not
the installed artifact**. `install.sh:5` is a bare `go build -o bin/ ./cmd/aira/...`
with **no `CGO_ENABLED=0`**; only `make dist` (`Makefile:56`) sets it. Verified
against the artifact actually on this box: `file ~/.local/bin/aira` reports
*"dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2"*. **Bind-mounting
today's installed `aira` into an alpine/musl or distroless image fails at exec.**
So the mechanism is: inject a `CGO_ENABLED=0` build (or the `make dist` artifact),
and version-match it to the running host daemon — a client/daemon protocol skew
inside a container is a debugging trap an agent has no way to diagnose from
inside.

**What this buys, precisely.** `aira run` is **scope-less** — it takes a daemon
admission but mints no cgroup scope, stated in-source at
`internal/runner/confine_reserve_linux.go:102-103` ("`aira run` is also
scope-less"). Every scope-less, daemon-side verb therefore works over a mounted
socket *unchanged*: tickets, leases, gates, findings, `run`, and admission
itself. **That is the majority of the owner's ask and it is genuinely
achievable.**

#### 3.7.2 Structurally blocked: nested `confine`, and what `--delegate-ram` buys

**Confine does all of its cgroup work client-side.** `unifiedMount()` parses
`/proc/self/mountinfo` for the cgroup2 mount (`internal/runner/cgroup_linux.go:32-56`);
`currentCgroupPath()` reads the `0::` line of `/proc/self/cgroup` (`:66-78`);
`resolveSlicePathAt` then **walks upward from that path looking for a directory
literally named `aira.slice`** (`internal/runner/admission_linux.go:957-990`, the
walk at `:973-981`). `Probe` mkdirs a probe dir and opens `cgroup.kill`
(`cgroup_linux.go:117-147`), `Create` mkdirs `.aira-<id>` (`:173-208`), and
placement is `clone3(CLONE_INTO_CGROUP)` via
`SysProcAttr{UseCgroupFD, CgroupFD}` (`confine_linux.go:1174`). None of this is
delegated to the daemon, so a mounted socket does not help it.

Under a **private cgroup namespace** — podman's cgroup-v2 default, subject to
§3.7.3 question 1 — `/proc/self/cgroup` reads `0::/` and the container's own
cgroup *is* the mount root. The upward walk then runs exactly once, at the mount
root, finds no `aira.slice`, and returns `slice-not-found`
(`admission_linux.go:960`), and confine refuses with `E_CONFINE_UNAVAILABLE`
(`confine_linux.go:505-513`). **`--cgroups=split` does not change this**: split
decides where *podman* puts the container, not what the container's own namespace
shows.

Two further blockers, each independently sufficient:

- **`clone3` under container seccomp.** `clone3Available()`
  (`cgroup_linux.go:108-115`) requires errno `EINVAL`; container seccomp profiles
  have historically stubbed `clone3` to `ENOSYS`, which makes `Probe` fail with
  *"clone3 unavailable or denied"* (`cgroup_linux.go:143`). Unverified for
  podman's default profile — §3.7.3 question 3.
- **PID-namespace-local identities.** Scope ids and detach records embed
  `os.Getpid()` (`confine_linux.go:1767`, `:1809`;
  `confine_detach_linux.go:578`). Under a PID namespace these are
  namespace-local, so a scope minted inside would not match host-side
  `confine --list`/`--kill` or the orphan reaper. Even if the cgroup visibility
  problem were solved, the identity layer would need its own design.

**`--delegate-ram` inherits exactly this split.** The good news:
`resolveDelegateRAMScopeCeiling` (`internal/daemon/admit.go:1908-1941`) is
**purely daemon-side** — it reads the request signature and the peak history and
subtracts headroom, and never touches the caller's own scope. So the *ceiling
computation* transfers over a mounted socket unchanged. The bad news: everything
the ceiling *buys* is cgroup-local. The client writes the ceiling as its own
`memory.max`; `BootstrapAitestSupervisor`
(`internal/runner/aitest_bootstrap_linux.go:37`) relocates the supervisor PID and
writes `subtree_control`; and the daemon returns a **host-absolute `ScopePath`**
(`internal/runner/worker_admit_client_linux.go:80`,
`internal/runner/worker_scope_linux.go:57`) that the in-container worker would
have to write itself into. So "`--delegate-ram` also working fine" is **not**
achievable in v1 as a *confinement* guarantee; only its advisory ledger half is.

**And there is no existing channel to reuse even if the rest worked.**
`AIRA_CONFINE_SCOPE_ID` and `AIRA_CONFINE_PARENT_SLICE` are published by
`AppendConfineChildEnvironment` (`internal/pylib/env.go:193-207`, published at
`confine_linux.go:1112`) — but to the *podman CLI process*, and podman drops them
at the container boundary. Worse, `ConfineParentSliceEnv`
(`internal/pylib/env.go:105`) is by construction a **host-absolute cgroup path**
(`confine_linux.go:1102-1112`), meaningless inside a private cgroupns. And
`InheritedConfineScopeID` is consumed only by `confine-reserve`
(`confine_reserve_linux.go:105`), never by an ordinary nested `confine` — so
there is no existing "my outer scope" channel a plain nested confine could
reuse, and inventing one is a design change, not a wiring change.

#### 3.7.3 The three questions this environment cannot answer

These require a live podman container and are **not** settled here in either
direction. A gate must not read §3.7.2's blocked verdict as covering them.

1. **Does podman 4.9.3 on this box actually default cgroup-v2 containers to
   `--cgroupns=private`?** The flag exists (`podman run --help`, §1's table) and
   private is the documented v2 default, but the *effective* default under this
   box's systemd cgroup manager was not observed. If it is `host`, §3.7.2's first
   blocker weakens and the whole picture changes.
2. **Does `--cgroupns=host` also yield a *writable* `/sys/fs/cgroup` showing host
   paths?** Confine needs to `mkdir` a scope and write `cgroup.procs`, not merely
   read. `/sys/fs/cgroup` is typically mounted read-only in a container without
   `--privileged`, which would leave confine blocked even with a host cgroupns —
   but this was **not** verified, and asserting it either way would be exactly the
   fabrication this project forbids.
3. **Does podman's default seccomp profile permit `clone3`?** Determines whether
   `Probe` fails before the cgroup question is even reached.

A fourth, cheaper question rides along: whether the bind-mounted socket is
reachable at all in practice, which questions 1-3 do not gate and which a
one-container smoke test settles.

#### 3.7.4 The overlooked asset, and the design fork it opens

**AIRA-121 already built a container mode**, and this plan nearly missed it.
`ConfineModeShim` is documented verbatim as *"the container shim (AIRA-121): no
systemd, no delegated cgroup subtree, no per-job scope, and therefore NO kill
backstop. Admission is an advisory in-daemon RAM ledger and nothing more"*
(`internal/runner/confine_mode.go:26-29`), with the sentinel slice
`ShimConfineSlice = "ci-shim"` chosen so that *"a stray real cgroup read against
it fails loudly rather than resolving somewhere plausible"* (`:32-38`), and a
closed budget-provenance enum (`:76-92`) whose weakest member is annotated with a
container hazard already learned: *"/proc/meminfo is not namespaced on the
runtimes in question, so inside a container it reports the HOST's memory"*
(`:89-92`). Shim mode also deliberately publishes **neither** confine coordinate
(`internal/runner/confine_shim_linux.go:270`, `:292`) — consistent with §3.7.2.

**This is exactly the honest shape §3.7.2's blocked half needs: admission without
a kill backstop, labelled as such.** But it is a **whole-install** mode, read from
`<state>/aira/install-mode.json` (`confine_mode.go:123-124`), and the daemon must
agree via `AIRA_DAEMON_CONFINE_MODE` (`internal/daemon/shim.go:271-283`, set by
`internal/install/mode.go:305`). **A container bind-mounting the host state home
therefore reads `real-slice` and takes the failing real path.**

So the fork, stated plainly rather than resolved: making nested confine work
honestly inside the container means a **per-process** shim signal rather than a
per-install one — an injected env marker that makes *this* process take the shim
path while the host install stays `real-slice`. That is a real change to
AIRA-121's mode model with its own honesty surface (a shim job must never be
listed or counted as if it had a kill backstop), and it is **§5 open question 11**,
not something this plan settles.

#### 3.7.5 What v1 therefore promises, and what it must refuse to promise

- **Required in v1:** bind-mount the socket and its runtime dir at the identical
  resolved path; inject `XDG_RUNTIME_DIR`/`XDG_STATE_HOME`/`HOME` as AIRA-owned
  env, refusing any caller override of those three; inject a `CGO_ENABLED=0`
  version-matched `aira` binary; and **guarantee the in-container client cannot
  spawn a daemon** (new fail-closed no-spawn knob, a prerequisite).
- **Required in v1, as honesty:** the verb's own help and the SKILL text must say
  that inside the container `aira run` and the coordination verbs work, and
  `aira confine` **refuses** with `E_CONFINE_UNAVAILABLE`/`slice-not-found`
  rather than silently running unconfined. An agent that hits the refusal must be
  able to read why without a source dive.
- **Must not be promised in v1:** nested `aira confine`, nested `--delegate-ram`
  as a containment guarantee, and any claim that a container's own sub-jobs are
  individually confined. Confinement of the container as a whole is §2's job and
  is unaffected — the container's memory is inside the *outer* job's scope
  regardless of what the client inside can see.

### 3.8 Explicitly not in v1

- Docker support of any kind (§3.1).
- `--network` in any form. A container with host networking is a different
  security posture and needs its own design.
- Pods, compose, `build`, `exec`, image pulling/management, or any podman
  verb other than `run`.
- Option B's AIRA-created container sub-scope (§2.2) — a measured follow-up.
- Any stdin conduit (§3.5) — AIRA-196's job.
- Any auto-adjustment of the caller's declared limits.
- **Nested `aira confine` inside the container, and `--delegate-ram` as an
  in-container containment guarantee** (§3.7.2). The socket passthrough itself
  *is* in v1; what it can honestly carry is bounded by §3.7.5.
- A per-process `ConfineModeShim` signal (§3.7.4) — a real design fork, filed as
  §5 open question 11.

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
- **The socket passthrough must never imply confine parity.** If the verb
  reports that the AIRA socket is mounted, that facet names *socket
  reachability* and nothing else. Nested `aira confine` inside the container
  refuses with `E_CONFINE_UNAVAILABLE`/`slice-not-found`
  (`confine_linux.go:505-513`, `admission_linux.go:960`), and that refusal is
  the honest outcome (§3.7.2) — the verb must not present a mounted socket as
  "confine works in here", and must not offer any fallback that runs the
  in-container job unconfined while looking confined.

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
9. **The three empirical container questions** (§3.7.3), which this
   static-analysis-only environment could not answer and which no amount of
   further source reading will settle: podman 4.9.3's *effective* cgroupns
   default for v2 under the systemd cgroup manager; whether `--cgroupns=host`
   yields a **writable** `/sys/fs/cgroup` (as opposed to the read-only mount I
   believe is typical but did **not** verify); and whether podman's default
   seccomp profile permits `clone3`. A gate should require a live one-container
   probe with a committed reproduction, exactly as §5 question 1 requires for
   F1/F3/F6/F9 — not a source argument.
10. **The no-spawn prerequisite** (§3.7.1). Mounting the socket without a
    fail-closed way to stop the in-container client forking
    `/proc/self/exe daemon` (`cmd/aira/dispatcher.go:529`, `:95`) risks a
    **second owner of the host `state.db`**. No such knob exists today. Is it
    a new `AIRA_DAEMON_*` env, a client flag, or a mode inferred from the
    mounted-socket wiring — and does it land in AIRA-197 or as its own ticket
    that AIRA-197 depends on, the way §3.5 depends on AIRA-196?
11. **Per-process `ConfineModeShim`** (§3.7.4). AIRA-121's shim is exactly the
    honest "admission ledger, no kill backstop" shape the container needs, but
    it is a whole-install mode (`confine_mode.go:123-124`) that a container
    sharing the host state home reads as `real-slice`. Making it per-process is
    a real change to AIRA-121's model with its own honesty surface — a shim job
    must never be listed or counted as though it had a kill backstop. In scope
    for AIRA-197, a follow-up, or rejected?
12. **The AIRA-owned env exception vs. §3.2's default-deny** (§3.7.1). Injecting
    `XDG_RUNTIME_DIR`/`XDG_STATE_HOME`/`HOME` is required for the socket to
    resolve, and a caller override of any of the three can redirect the client
    at an arbitrary socket. This plan proposes refusing such an override rather
    than merging it. A gate should confirm that refusal — and confirm the wider
    principle, since it is the first AIRA-owned exception to a default-deny env
    surface and every later exception will cite it.

## 6. Status

Plan only. Nothing is built; no code was changed by this document. AIRA-197 is
filed against it, related to AIRA-196 (stdin/log dependency) and AIRA-102 (the
shipped narrow mechanism this widens).

Amended 2026-09-09 with the owner's socket-passthrough requirement (§0.2, §3.7).
That investigation was **static analysis only** — every claim in §3.7 carries a
file:line, and the three questions needing a live container are quarantined in
§3.7.3 and §5 question 9 rather than answered optimistically. Its headline: the
socket half is achievable with a concrete mechanism; nested `confine` and
`--delegate-ram`-as-containment are structurally blocked by cgroup-namespace
isolation, and v1 must refuse rather than degrade.
