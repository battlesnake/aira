---
{"schema":1,"id":"AIRA-197","project":"aira","title":"aira container run -- a restricted, admission-gated podman launcher so agents get containers into aira.slice without knowing podman, with the AIRA socket mapped in (run works; nested confine is cgroup-namespace blocked)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","container","daemon","podman"],"hold":false,"relations":[]}
---

Owner request (2026-09-09), verbatim: "Perhaps aira should have a
subcommand for launching containers too (via podman, so aira can get them
into the correct slice and stuff like that)? So agents can launch
containers without having to care about docker/podman and stuff like
that, the SKILL/MCP just gives them an interface with a vastly reduced
set of options available (env/workdir/entrypoint/argv/mounts?), and the
same interface as confine/run/kill for monitoring/signalling and
injecting/closing stdin?"

Second owner requirement, added 2026-09-09, verbatim: "the command for
running containers (via podman) should transparently map the aira socket in
too, so jobs in the container can use aira run/confine and get correct
results (--delegate-ram also working fine)."

**Design plan:
`docs/superpowers/plans/2026-09-09-aira-container-launch-plan.md`.**
Plan only -- nothing built, matching how [[AIRA-176]], [[AIRA-180]] and
[[AIRA-194]] were handled tonight.

## The gap, stated honestly

The framing that "containers launched from inside a confine job escape
aira.slice's accounting entirely" is TRUE for docker and for every
undetected invocation form, and FALSE for the one narrow case
[[AIRA-102]] already ships: `internal/runner/container.go:428` injects
`--cgroups=split` into a detected `podman run`, nesting the container as
a kernel-enforced cgroup descendant of the job's own scope, documented at
`internal/core/skill.go:331`. What genuinely still escapes: every `docker
run` (structural -- dockerd owns a different cgroup tree,
`container.go:29-31`); every podman form the deliberately narrow detector
misses (`podman-compose`, `podman container run`, `sudo podman run`, `sh
-c "podman run ..."` -- no injection AND no warning); every podman run
not under `aira confine` at all (verified first-hand: `podman info`
reports the **systemd** cgroup manager on this box, so a default
`--cgroups=enabled` container is placed in a transient scope under the
user session, not under the caller's cgroup); and any caller who supplies
their own `--cgroups`/`--cgroup-parent`/`--pod`.

So the real value of a first-class verb is not "build placement" -- that
exists -- it is that **an argv AIRA constructs cannot be mis-detected**,
which widens coverage without widening a fragile string-matcher.

## Design shape

- **Podman-only by construction.** Docker is refused, never silently
  degraded: there is no honest way to place a dockerd-spawned container in
  `aira.slice`.
- **Placement v1 = reuse `--cgroups=split` under a real confine scope**
  (AIRA-102's shipped mechanism, F1/F5 measured). `--cgroup-parent=aira.slice`
  is REJECTED outright -- F6 measured it as a sibling under the slice,
  bound only by the aggregate cap and surviving the job's exit: cosmetic
  placement, exactly what the owner's ask warns against.
- **A measured follow-up option is identified, not assumed.** AIRA-102's F9
  (`--cgroup-manager=cgroupfs --cgroup-parent=<scope>` fails) failed for a
  CONDITIONAL reason -- the target leaf held processes, tripping cgroup v2's
  no-internal-process rule -- and `BootstrapAitestSupervisor`
  (`internal/runner/aitest_bootstrap_linux.go:16-37`) already dissolves
  exactly that condition (drain the leaf, delegate `+memory`, create an
  empty capped child via `CreateWorkerScope`). An AIRA-created, pre-sized
  container sub-scope may therefore be achievable and strictly better. It is
  a HYPOTHESIS requiring fresh measurement, deferred out of v1.
- **Restricted argument surface** (`--image`, `--entrypoint`, `--workdir`,
  `--env` default-deny, `--mount`, opaque trailing argv), enforced like
  `parseConfineArgs` (`cmd/aira/main.go:788-839`). The plan names this
  explicitly as a NEW CLASS of interface, not a precedent-following one:
  confine/run constrain only AIRA's own containment and wrap any argv with
  zero opinion; this verb constrains the wrapped tool's own capabilities
  (no `--privileged`, `--network`, `--cap-add`, raw `--cgroups`).
- **Mount safety reuses gate's containment discipline**
  (`internal/store/gate_command.go:194-214`): symlink-resolve both root and
  source, `filepath.Rel`, refuse any `..`-escape -- plus `:ro` by default and
  an unconditional refusal list (`/`, `/proc`, `/sys`, `/dev`, `/run`, the
  podman socket, `~/.ssh`, `~/.claude`) that holds even inside a permissive
  root. There is no existing generalized path-attestation primitive to reuse;
  `confine --owner` is a cooperative identity string, not a path fence.
- **Monitoring/control adds NO new job kind.** The container job IS a confine
  job whose argv AIRA constructed, so it produces an ordinary
  `ConfineDetachRecord` (`internal/runner/confine_detach.go:77`) and
  `confine --status/--list/--kill` work unchanged. Kill stays `cgroup.kill`
  (recursive, F5), never `podman stop`. Podman's own logs/attach API is
  explicitly forbidden -- it would bypass confine's capture + kill model and
  force the kind-tagged polymorphism this design exists to avoid.
- **The stdin half is NOT built here.** `ConfineDetachRecord` has no stdin
  conduit and `confine-log`/`confine-input` do not exist; this ticket
  DEPENDS on [[AIRA-196]] and inherits them when it lands, rather than
  building a stopgap that becomes a second parallel record type.
- **MCP/Skill: standard dispatch table.** `container run` is CLI-only
  (`Include` unset) for the reason recorded at `internal/core/core.go:1760`
  -- a foreground connection-bound launch has no honest request/response
  form. Read/terminal verbs take `confine-list`/`confine-kill`'s shape
  (`core.go:1821,1831`). SKILL text extends the existing "Containers under
  confine" section rather than competing with it.
- **Accounting rule inherited verbatim from AIRA-102's build-review P0:**
  inject `--memory` only from a CALLER-DECLARED limit, never an AIRA
  estimate -- injecting a resolved estimate produced a container OOM-killed
  forever inside a limit AIRA invented, with the kill invisible to the job's
  trailer and therefore no self-healing (`container.go:408`).
- **Socket passthrough (owner requirement 2) SPLITS -- half achievable, half
  structurally blocked.** Plan §3.7, static analysis only, every claim cited.
  ACHIEVABLE with a concrete mechanism: the daemon socket path is fully
  deterministic (`internal/daemon/paths.go:478-509` derives it from
  `XDG_STATE_HOME`/`XDG_RUNTIME_DIR`/`HOME` alone -- NO uid, hostname, pid or
  cgroup input), so bind-mounting it at the identical *resolved* path
  (`canonicalPath` EvalSymlinks-es before hashing, `paths.go:511-541`) plus
  injecting those three vars as AIRA-OWNED, caller-unoverridable env makes
  `aira run` and every scope-less daemon-side verb work unchanged (`aira run`
  is scope-less per `confine_reserve_linux.go:102-103`). Two prerequisites
  found by checking rather than assuming: `install.sh:5` has NO
  `CGO_ENABLED=0` (verified: the installed binary is dynamically linked
  against glibc; only `Makefile:56` builds static), so the injected binary
  needs a static, version-matched build; and the in-container client MUST be
  blocked from auto-spawning a daemon -- `cmd/aira/dispatcher.go:510-531`
  unlinks a "stale" socket and forks `/proc/self/exe daemon` (`:95`), which
  with the host state home mounted would unlink the host's live socket and
  start a SECOND owner of `state.db`. No no-spawn knob exists today.
- **BLOCKED, stated plainly rather than papered over: nested `aira confine`
  inside the container, and `--delegate-ram` as a containment guarantee.**
  Confine does ALL cgroup work client-side -- `/proc/self/mountinfo`
  (`cgroup_linux.go:32-56`), the `0::` line of `/proc/self/cgroup` (`:66-78`),
  then an upward walk for a directory literally named `aira.slice`
  (`admission_linux.go:957-990`). Under podman's cgroup-v2 default
  `--cgroupns=private` that walk starts at the namespace root, finds nothing,
  and confine REFUSES with `E_CONFINE_UNAVAILABLE`/`slice-not-found`
  (`confine_linux.go:505-513`). `--cgroups=split` does not help: it decides
  where podman puts the container, not what the container's namespace shows.
  Two further independent blockers: `clone3Available()` needs errno EINVAL
  (`cgroup_linux.go:108-115`) and container seccomp has historically stubbed
  `clone3` to ENOSYS; and scope ids embed `os.Getpid()`
  (`confine_linux.go:1767,1809`), namespace-local under a PID ns. For
  `--delegate-ram` the ceiling COMPUTATION is purely daemon-side and transfers
  (`internal/daemon/admit.go:1908-1941`), but everything it buys is
  cgroup-local (client-written `memory.max`;
  `aitest_bootstrap_linux.go:37`; a host-absolute `ScopePath` at
  `worker_admit_client_linux.go:80`). The refusal is the HONEST outcome and v1
  must not degrade to a silent unconfined launch.
- **Overlooked asset + a real design fork.** AIRA-121 already shipped a
  container mode: `ConfineModeShim` is documented as "no per-job scope, and
  therefore NO kill backstop. Admission is an advisory in-daemon RAM ledger"
  (`internal/runner/confine_mode.go:26-29`) -- exactly the honest shape the
  blocked half needs. But it is a WHOLE-INSTALL mode read from
  `<state>/aira/install-mode.json` (`:123-124`), so a container sharing the
  host state home reads `real-slice` and takes the failing real path. Making
  it PER-PROCESS is a genuine design fork, deferred to the gate.

## Open questions left for the gate

Re-measurement of F1/F3/F6/F9 (recorded as live probes, not re-run here;
podman 4.9.3 and its flag surface WERE verified first-hand); the mount
containment root, which collides with the verb's project-less-ness; whether
v1 reads back the payload cgroup's `memory.max` to upgrade the trailer facet
from "asked for" to "observed"; escape hatch or none for unlisted podman
flags; whether the resource signature moves from `podman` to image+entrypoint
(today every container run on the box shares one estimate bucket); whether a
`container list` verb exists at all or `confine --list` suffices; the
`--detach` naming collision (job-detached vs podman `-d`); and confirmation
of the [[AIRA-196]] ordering dependency.

From the socket-passthrough investigation, which was STATIC ANALYSIS ONLY
(no container was launched -- this environment can read podman's flag surface
but cannot exercise a container's cgroup namespace), three questions remain
GENUINELY OPEN and no further source reading will settle them: whether podman
4.9.3 on this box EFFECTIVELY defaults cgroup-v2 containers to
`--cgroupns=private` under the systemd cgroup manager; whether
`--cgroupns=host` also yields a **writable** `/sys/fs/cgroup` (confine must
mkdir a scope and write `cgroup.procs`, not merely read -- a read-only mount
is believed typical without `--privileged` but was NOT verified); and whether
podman's default seccomp profile permits `clone3`. A gate should require a
live one-container probe with a committed reproduction, not a source
argument. Plus: where the fail-closed no-spawn knob lands (own ticket that
AIRA-197 depends on, the way §3.5 depends on [[AIRA-196]]?); whether a
per-process `ConfineModeShim` signal is in scope, a follow-up, or rejected;
and confirmation of the AIRA-owned env exception -- injecting
`XDG_RUNTIME_DIR`/`XDG_STATE_HOME`/`HOME` is the first exception to a
default-deny env surface and every later one will cite it.
