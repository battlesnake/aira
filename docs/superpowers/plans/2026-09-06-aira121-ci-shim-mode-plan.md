# AIRA-121 — `install --ci=shim`: honest degradation for systemd/cgroup-unavailable containers

Status: **v1**, for plan review
Ticket: `.aira/tickets/AIRA-121.md` (nine numbered requirements; 1–5 original
scoping, 6–9 consumer-verified by peer session `deploy`)
Branch: `aira121-ci-shim-mode` off `origin/master` `568b6e8` (AIRA-120 landed;
no rebase needed)
Related: AIRA-120 (`--ci` free-RAM slice ceiling, the flag this extends),
AIRA-123 (the follow-up that supersedes requirement 7's interim behaviour)

---

## 0. The one-sentence design

Shim mode is **not a second confinement mechanism**: it is the existing
admission ledger with three of its inputs re-sourced (slice path → a sentinel,
`memory.max` → a recorded container budget, cgroup scan → a true empty result),
plus a launch path that skips every cgroup step *up front* and reports one new,
always-established `containment` facet whose value is `advisory` instead of
`enforced`.

Everything else — the flag surface, the reserve resolution, the AIRA-67
peak-RSS estimator, the queue, the fairness freeze, the signal forwarder, the
peak reporting — is reused unchanged. **No new ledger data structure is
introduced.** That is the whole point of §4, and it is what keeps this inside
the project's architectural-simplicity rule rather than stacking a
per-environment kludge.

---

## 1. Scope, non-scope, and the two things this plan deliberately does not build

### In scope

| Req | Deliverable | Section |
|-----|-------------|---------|
| 1 | Install-time capability probe; explicit mode resolution; mode always reported | §2, §3 |
| 2 | `confine`/`run` launch with **no** cgroup step attempted | §5 |
| 3 | Advisory-only in-daemon RAM-budget ledger + honest reporting everywhere | §4, §6 |
| 4 | Container RAM budget source, with the choice documented | §4.2 |
| 5 | aitest's daemon-down fallback keeps working, proven end-to-end | §7 |
| 6 | Existing resource flags parse and run, never reject | §5.4 |
| 7 | `--delegate-ram` does **not** export `AIRA_AITEST_LIB` in shim mode (interim) | §5.5 |
| 8 | Process-group signal forwarding + documented escape exception | §5.6 |
| 9 | `install` splits into build-time and start-time stages; daemon never blocks exit | §3 |

### Out of scope (from the ticket, restated so nobody re-opens it)

Making the real cgroup slice work inside a container by privilege escalation,
`--cgroupns=host`, or provider-specific delegation coaxing. The shim's job is
to **degrade honestly**, not to chase real containment into every runtime.

### Two things this plan deliberately does not build

1. **AIRA-123's degraded `worker-admit`.** Requirement 7 is explicitly INTERIM.
   This plan ships the "do not export `AIRA_AITEST_LIB`" behaviour *and* leaves
   a one-function seam (§5.5) so AIRA-123 flips a single predicate. It does not
   build a ledger-only worker-admit backend.
2. **aitest `auto` worker sizing consulting the shim ledger.** Requirement 5
   names this as a reasonable, separate enhancement that is not required for
   this ticket. Recorded as a follow-up on AIRA-123, not done here.

---

## 2. The capability probe (requirement 1)

### 2.1 What it answers, and why it is decisive on exactly one question

New file `internal/install/capability.go`. One exported type, one pure-ish
function over the existing `installDeps` seams (`d.run`, `d.readFile`, `d.stat`),
so it is injectable and unit-testable without a container:

```go
// CapabilityReport records the facts a mode decision is made from, SEPARATELY.
// Each field is an established fact or an explicit "unevaluated" reason; none
// is a blended boolean, because the report is printed to an operator and
// recorded durably (§3.3) and a blob cannot be argued with later.
type CapabilityReport struct {
    SystemdUserManager  string // "reachable" | "absent" | "unreachable: <reason>"
    CgroupV2Unified     string // "present" | "absent" | "unevaluated: <reason>"
    OwnCgroupPath       string // from /proc/self/cgroup, or ""
    OwnCgroupMemoryMax  string // "max" | "<bytes>" | "unevaluated: <reason>"
    MemTotalBytes       int64  // 0 when unevaluated
}

func ProbeCapability(d installDeps) CapabilityReport
```

**The mode decision reads exactly one field: `SystemdUserManager`.** That is
deliberate, and it is a fact about this codebase rather than a simplification:
every step of the real install is downstream of a working systemd user manager
— the `aira.slice` unit, `daemon-reload`, `enable --now` on the anchor,
`Delegate=`-derived memory delegation, and `aira-daemon.service` itself. There
is nothing the other three facts could add to the *decision*; they are recorded
because they are what an operator needs to understand the decision, and because
`OwnCgroupMemoryMax` and `MemTotalBytes` feed the budget resolution in §4.2.

Probe mechanism for the decisive field: `d.run(timeoutArgv("systemctl", "--user",
"is-system-running"))`, wrapped in the existing `timeoutArgv` helper so a hung
D-Bus cannot hang a `docker build`. Its *exit code is ignored* — `degraded` and
`running` and `starting` are all "reachable"; what matters is whether the user
manager answered at all. `exec: "systemctl": executable file not found` →
`absent`; any other failure → `unreachable: <err>`, distinguished in the report.
Both resolve to shim under `--ci=auto` (§2.2), because neither can run the real
install — but the *reason* is never lost.

**Deliberately not probed: an actual cgroup `mkdir`.** A write probe would be
the only conclusive test of delegation, but it is intrusive (it creates a
directory in someone else's cgroup tree), it is redundant behind the systemd
gate, and it would have to be undone on a path where the undo can fail. Read
`cgroup.controllers` and record it; do not write. Recorded as an accepted
limitation: a box with a working systemd user manager but a broken delegation
still fails the *real* install, loudly, exactly as it does today — the shim
does not exist to paper over that.

### 2.2 Flag surface: `--ci`, `--ci=shim`, `--ci=auto`

`parseInstallArgs` today has `--ci` in the **valueless** branch. It moves to a
branch that accepts an optional value:

| Invocation | Meaning |
|---|---|
| `--ci` | **Unchanged AIRA-120 behaviour.** Real install, MemoryMax from a MemAvailable snapshot. No probe. Fails on a systemd-less box exactly as it does today. |
| `--ci=shim` | Force shim mode. No probe needed for the decision; the probe still runs and is recorded as evidence. |
| `--ci=auto` | Run the probe. `SystemdUserManager == "reachable"` → the `--ci` path; anything else → shim. |
| `--ci=<other>` | `E_INSTALL_ARGUMENT_INVALID: --ci must be given bare, or as shim or auto` |

**Why bare `--ci` is not made auto-detecting.** The ticket says "decide which at
build time, but whichever is chosen, ALWAYS report explicitly which mode
actually got installed; never let two boxes running nominally 'the same'
install command end up silently different." A bare `--ci` that silently changes
meaning depending on the host is precisely the shape that rule forbids, and it
would also retroactively change a flag that landed three commits ago. So bare
`--ci` stays deterministic, `--ci=auto` is the opt-in to host-dependence, and
**both** paths print the resolved mode (§3.4). An operator who wants
host-dependence asks for it by name.

`--ci=shim` inherits AIRA-120's mutual exclusion with `--memory-max`? **No** —
and this is a decision, not an oversight. In shim mode `--memory-max` does not
size a slice unit (there is no unit); it would size the *ledger budget*. Rather
than overload one flag with two meanings, `--ci=shim` **refuses** `--memory-max`
with a message naming the shim budget source instead:
`E_INSTALL_ARGUMENT_INVALID: --memory-max sizes the aira.slice unit, which
ci-shim mode does not create; the shim ledger budget is read from the
container's own cgroup memory.max or MemTotal (see --status)`. One meaning per
flag; no silent winner.

---

## 3. Splitting `install` into build-time and start-time (requirement 9)

### 3.1 The consumer's shape

`aira` is baked into a Docker image at build time (`docker build`: no network
guarantee, no systemd, no daemon, no `/sys/fs/cgroup` write access, layers
cached and replayed), and jobs then run in fresh containers from that image.
The current `runUserInstall` interleaves read → compute → render → publish →
`daemon-reload` → `enable --now` → linger → daemon-reachability in one
straight line, so nothing in it is safe in a build layer.

### 3.2 The split

New flag `--stage=build|start` on `aira install`, defaulting to **both, in
order** — so today's `aira install` is byte-for-byte today's behaviour and no
existing caller changes.

`runUserInstall` is refactored into three parts, and the refactor is the bulk of
the install-side diff:

```go
// Pure-ish: reads state, resolves modes, computes limits, renders units.
// No writes outside the plan, no systemd, no network, no daemon contact.
func planInstall(d installDeps, opts installOpts) (installPlan, error)

// Build stage: everything that only places bytes on disk.
//   real mode: mkdir unit dir, take the install lock, publish the managed
//              slice/anchor/daemon units, write install-mode.json
//   shim mode: write install-mode.json (mode, probe report, budget, budget
//              source, HOME, uid, aira version, timestamp). Nothing else.
func applyInstallBuild(d installDeps, plan installPlan) error

// Start stage: everything that starts or contacts something.
//   real mode: daemon-reload, enable --now anchor, delegation, verify limits,
//              displace incumbent, publish+enable daemon unit, linger,
//              waitDaemonReachable
//   shim mode: spawn the detached shim daemon (§3.5), wait for its socket
func applyInstallStart(d installDeps, plan installPlan) error
```

The install lock (`.aira-install.lock`, `flock(LOCK_EX)`) is taken **inside each
stage**, not around `planInstall`. A combined run takes it twice; that is
correct rather than merely acceptable, because the two stages are already
separated by a `docker build`/`docker run` boundary in the split case, so the
combined case must not depend on a stronger property than the split case has.
AIRA-106's under-lock re-resolve of the daemon modes moves into
`applyInstallBuild` unchanged.

### 3.3 The recorded plan: `install-mode.json`

Written by the build stage, at `<Paths.StateHome>/install-mode.json`, mode
`0600`, atomically (temp + rename in the same directory):

```json
{
  "schema": 1,
  "mode": "ci-shim",
  "aira_version": "<build version>",
  "recorded_at": "2026-09-06T11:04:22Z",
  "home": "/home/runner",
  "uid": 1000,
  "resolved_by": "--ci=auto (probe)",
  "capability": { "systemd_user_manager": "absent", "cgroup_v2_unified": "present",
                  "own_cgroup_path": "/", "own_cgroup_memory_max": "34359738368",
                  "mem_total_bytes": 67374264320 },
  "shim_budget_bytes": 34359738368,
  "shim_budget_source": "cgroup-memory-max",
  "shim_cgroup_path": "/sys/fs/cgroup"
}
```

It has **three** readers, and having exactly one durable source of truth is what
makes it impossible for them to disagree about the mode:

1. `aira install --stage=start` — refuses (`E_INSTALL_UNAVAILABLE`) when the file
   is absent, or when `home`/`uid` differ from the running environment. A start
   stage must never *re-resolve* a mode the build stage already resolved: that is
   the "two boxes silently different" failure with the two boxes being the same
   box at two points in time.
2. The daemon — via the start stage, which transcribes the budget into the
   daemon child's environment (§3.5). The daemon does **not** read the file
   itself; it stays configured exactly the way every other daemon subsystem is
   (§4.1).
3. `aira confine` / `aira run` — read it once per process, cached, to resolve
   their own mode (§5.1). **Absent → real mode**, so every existing installed
   box is untouched.

`aira install --status` prints the recorded mode and budget source (§3.4).

### 3.4 Always report the resolved mode

Both stages, both modes, on both the `--dry-run` and the real path:

```
install mode: ci-shim (resolved by --ci=auto; systemd user manager: absent)
shim ledger budget: 32.00GiB (34359738368 bytes) from the container's own cgroup memory.max
containment: advisory — no cgroup scope is created and no kill backstop exists
```

and on the real path, symmetrically:

```
install mode: real-slice (resolved by --ci)
containment: enforced — per-job cgroup scope under aira.slice
```

Requirement 1's "never let two boxes end up silently different" is satisfied
structurally: there is no path through `install` that does not print one of
these two lines.

### 3.5 Spawning the shim daemon without blocking container exit (requirement 9, test i)

`applyInstallStart` in shim mode spawns `aira daemon serve` with, in order:

- `SysProcAttr{Setsid: true}` — a new **session**. This detaches it from the
  entrypoint's process group and from any controlling terminal, so an entrypoint
  that signals its own group does not take the daemon with it, and — the point
  that matters for §5.6 — the daemon is never inside a workload's process group.
- `cmd.Stdin = /dev/null`; `cmd.Stdout = cmd.Stderr =` an append-opened
  `<StateHome>/shim-daemon.log`. **Never an inherited pipe.** A backgrounded
  process holding the write end of the entrypoint's stdout pipe is the classic
  reason `docker run` appears to hang after the workload exits: the pipe never
  reaches EOF. This single line is the substance of test (i).
- `cmd.Env` carrying `AIRA_DAEMON_MANAGED=1` (so `runDaemonCommand` does not try
  `systemctl --user start` and defer to a service that does not exist),
  `AIRA_DAEMON_CONFINE_MODE=shim`, `AIRA_DAEMON_SHIM_BUDGET_BYTES`,
  `AIRA_DAEMON_SHIM_BUDGET_SOURCE`, `AIRA_DAEMON_SHIM_CGROUP_PATH`, and
  `AIRA_DAEMON_WATCHDOG_MODE=off` + `AIRA_DAEMON_SLICE_CEILING_MODE=off` +
  `AIRA_DAEMON_OOM_STEER_MODE=off` (all three are machine-wide cgroup/PSI
  mechanisms that would either be inert or misfire against a host's numbers seen
  through a container's `/proc`; forcing them off is honest, not conservative).
- `cmd.Start()`, then **`cmd.Process.Release()` and no `cmd.Wait()`**. The
  installer process exits immediately; the daemon is reparented to the
  container's init. Reaping it when PID 1 exits is init's job and is explicitly
  out of scope per the ticket; *not being waited on while the container runs* is
  in scope and is exactly what `Release()` + no-`Wait` + no-inherited-pipe give.
- Then a **bounded readiness wait on the socket**, reusing `waitDaemonReachable`
  (≤10s). This waits on a *socket*, never on the process. If it never comes up,
  the start stage reports it and exits non-zero — a container whose ledger is
  dead should fail loudly at start rather than silently run every job ungated.

---

## 4. The RAM-budget ledger (requirements 3 and 4)

### 4.1 Shape: no new data structure

The claim to defend in review: **the existing `sliceQueue` already is the
ledger this ticket asks for, and it never touches a cgroup except through three
injectable seams.** Those three seams already exist for the daemon's own tests:

| Seam (on `Server`) | Real mode | Shim mode |
|---|---|---|
| `admitResolveSlice(slice) (path, ok, reason)` | `resolveAdmitSlicePath` | `resolveShimSlicePath` — returns the constant sentinel `"ci-shim"` for **any** requested slice name |
| `admitReadMemory(path) (cur, max, reclaimable, ok, reason)` | `readSliceMemory` | `s.readShimMemory` (§4.3) |
| `admitConfineScan(path) (ConfineListResult, error)` | `runner.ListConfines` | a scanner returning an **empty but successful** result |

So the additions to `Server` are two fields and nothing else:

```go
confineMode confineMode // "real" | "shim"; from Paths, from the daemon env
shimBudget  shimBudget  // {Bytes int64; Source string; CgroupPath string; At time.Time}
```

set in `NewServer` from `Paths`, which reads and validates the three
`AIRA_DAEMON_SHIM_*` variables in `PathsFromEnv` with the established
`E_CONFIG_INVALID` idiom (`paths.go` already does exactly this for the watchdog,
the slice ceiling, the OOM steerer and four admission tunables). A budget of
zero or a source string outside the closed set is a config error at daemon
start, not a runtime surprise.

**What is stored where, precisely.** The *durable* record is
`install-mode.json` (§3.3), written once at image-build time. The *daemon's*
copy is the two `Server` fields, set once at start from its environment. The
*live* accounting is `sliceQueue.outstanding` / `outstandingJobs` — already
in-memory, already the single ledger, unchanged. There is no third place.

### 4.2 The budget source, and why (requirement 4)

Resolved **once, at build-stage install time**, recorded in
`install-mode.json`, in this precedence:

1. **`cgroup-memory-max`** — the container's own cgroup `memory.max`, read at
   the path derived from `/proc/self/cgroup` (under `cgroupns=private`, the
   normal container case, that is `/sys/fs/cgroup` itself). Used when it is
   readable **and finite**.
2. **`meminfo-memtotal`** — `MemTotal` from `/proc/meminfo`, when `memory.max`
   is `max` or unreadable. Recorded as the distinctly weaker source it is.
3. **Neither → `aira install --ci=shim` FAILS** with `E_INSTALL_UNAVAILABLE`,
   mirroring AIRA-120's `resolveCIMemoryMax` refusal for an unevaluated
   MemAvailable. **A silently-ungated shim must never exist**; failing at
   install is one loud failure in one place, versus a per-job wedge later.

**The documented choice the ticket asks for: why the container's `memory.max`
and not AIRA-120's MemAvailable.** Three reasons, all specific to a container:

- `/proc/meminfo` is **not namespaced** on the runtimes in question (GCP Batch,
  AWS Batch, Fargate, k8s, plain containerd) unless something like lxcfs is
  interposed. Inside a 32 GiB container on a 256 GiB host, MemAvailable reports
  the *host's* free memory. Using it as the ceiling would over-book the
  container by an order of magnitude — the exact failure the ledger exists to
  prevent.
- `memory.max` is "what am I actually allowed": it is the number the container
  runtime will OOM-kill against. Booking against anything else is booking
  against a limit nobody enforces.
- AIRA-120 uses MemAvailable because it sizes a slice that must **coexist with
  the rest of a shared desktop**: the free-RAM snapshot is a politeness bound. A
  Batch container is single-tenant — the whole container is the workload's — so
  the ceiling is the container's limit, and *current usage* is subtracted at
  admission time by the existing `checkedAvailable`, not baked into the ceiling.
  Same primitive, different environment, different correct answer.

Live memory pressure is therefore not lost, it moves: it is `current` (§4.3),
re-read every admission pass, which is where the real path reads it too.

### 4.3 `readShimMemory`: how the ledger is read at admission time

```go
func (s *Server) readShimMemory(string) (cur, max, reclaimable int64, ok bool, reason string)
```

The `path` argument is ignored (it is the `"ci-shim"` sentinel). Behaviour by
recorded source:

- **`cgroup-memory-max`**: delegate to the existing `readSliceMemory` on the
  recorded `shim_cgroup_path`. That already parses `memory.current`,
  `memory.max` and the `memory.stat` file-LRU discount, so `current` and
  `reclaimable` are real kernel numbers and nothing is reimplemented. Then
  `max = min(read max, s.shimBudget.Bytes)` — the recorded budget is a bound,
  never a raise, so a runtime that widened the container's limit after install
  cannot silently widen the ledger.
- **`meminfo-memtotal`**: `current = MemTotal - MemAvailable`, `reclaimable = 0`,
  `max = s.shimBudget.Bytes`. `reclaimable` is deliberately zero: MemAvailable
  already credits reclaimable page cache, so applying AIRA-21's discount on top
  would double-count it in the permissive direction.
- **Any read failure**: `ok = false`. The existing behaviour then applies
  verbatim — fail **closed**, waiters stay queued, each waiter's own `maxWait`
  still fires `E_ADMIT_SATURATED`. Identical honesty to the real path, with no
  new code.

Everything downstream is untouched: `admitEffectiveMaximum`,
`admitSliceHeadroom` (base + per-job headroom still applies — same
conservatism), `checkedAvailable`, `resolveAdmitReserve` (the **AIRA-67
per-signature peak-RSS estimator**, which reads a SQLite projection and never
touches a cgroup — requirement 3's "reusing the EXISTING machinery" is satisfied
by *not editing it*), the fairness freeze, `E_ADMIT_TOO_LARGE`.

### 4.4 The four accounting facets that must change, and why each is *true* in shim mode

This is where a careless implementation would fabricate a pass. Each is stated
as a fact, not a workaround:

1. **`adopted` / `adoptedJobs` = 0 from a *successful* scan.** There are no
   cgroup scopes, so zero adopted reserve is the true reading, not a failed one.
   The shim scanner returns an empty `ConfineListResult` with `err == nil`.
2. **`liveScopesKnown` must be FALSE, and `--exclusive` is refused.** With a
   successful empty scan, `sliceProvablyEmpty` would return true and grant
   exclusivity on fabricated emptiness — an *unconfined* job would be told it
   was running alone. So the shim scanner marks liveness unknown, and
   `admitConnection` refuses an exclusive request in shim mode with the existing
   `CodeAdmitExclusiveUnestablished` plus a shim-specific reason. That code's
   established meaning — "an empty slice could not be established" — is exactly,
   literally true here, so no new error code is minted (one vocabulary per
   primitive, per CLAUDE.md).
3. **`capAggregateKnown` = false.** AIRA-114's over-subscription bound sums
   per-scope `memory.max` values; there are no scopes and no caps, so the sum
   cannot be established. Its documented direction is **fail-open** — an unknown
   aggregate withholds nothing — which is correct: in shim mode the reserve
   check is the only gate, and pretending to a second one would overclaim.
4. **AIRA-74's post-restart reserve reconstruction cannot work.** It rebuilds
   each in-flight job's reserve from its scope's `memory.max`; with no scopes
   there is nothing to rebuild from. **Stated residual, not fixed:** a shim
   daemon restart forgets the ledger and can over-admit until the in-flight jobs
   finish. Accepted because (a) a container's daemon does not normally restart
   within a job's lifetime, (b) the guarantee is advisory anyway — losing it
   costs one job rerun on a single-tenant box, and (c) the alternative is a
   second, shim-only persistence mechanism, which is precisely the per-feature
   complexity stacking the architectural-simplicity rule forbids. It goes in the
   plan's residuals, in `--status`, and in the SKILL text.

### 4.5 `worker-admit` in shim mode

`workerAdmitConnection` answers `{state: "unevaluated", reason: "ci-shim: no
cgroup sub-scope is available"}` — the same response shape it already produces
for an unreadable slice, which aitest's existing daemon-down/no-grant fallback
already consumes. No aitest change, and AIRA-123's job becomes "turn this one
`unevaluated` into a ledger-only advisory grant", which is a small, local edit.

---

## 5. `confine` in shim mode (requirements 2, 6, 7, 8)

### 5.1 Mode resolution on the client side

`internal/runner/confine_mode.go`: `ResolveConfineMode()` reads
`install-mode.json` once per process (`sync.Once`, cached) and returns
`confineModeReal` on absence, on a parse failure, or on an unrecognised mode
string. Every existing installed box therefore behaves identically, and an
unreadable record degrades to the *stricter* path, which fails closed at the
finite-cap gate rather than launching an unconfined job while claiming
containment.

**The client and the daemon read the same record** (the daemon via the env its
start stage transcribed from that record), so they cannot disagree about the
mode. A `AIRA_CONFINE_MODE` env override is deliberately **not** added: it would
be a second source of truth, and the failure it enables — a client in shim mode
against a real-mode daemon, launching uncontained while the ledger books a real
scope — is exactly the class this record exists to prevent.

### 5.2 Where the branch goes in `confineWithDeps`

`confineWithDeps` currently runs, in order: argv/cap/reserve validation →
identity normalisation → slice resolution → `backend.Probe` → `readCap` +
finite-cap refusal → `ensureDelegation` → reserve resolution → signature →
scope-id mint → `BeforeAdmit` → admission → `backend.Create` → signal handler →
launch with `SysProcAttr{UseCgroupFD}` → placement proof → wait → `readUsage` →
`reportPeak` → teardown attestation.

The shim branch is taken **after identity normalisation and before slice
resolution**, into `confineShim` (new file
`internal/runner/confine_shim_linux.go`).

**Shared with the real path, by calling the same functions** — not by copying
them: `normalizeConfineIdentity`, `validateScopeMemoryCap`, the
`MinPinnedScopeCap` refusal, `ResolveConfineReserve`, `PlanContainerIntegration`
(a `docker` argv in a shim container is still a `docker` argv),
`ResourceSignature`, `confineScopeID`/`bindConfineScopeID` (the scope id is still
minted — it is the admission key and the peak-RSS record key, and `--list`
identifies jobs by it), `BeforeAdmit`/`OnPlaced`, `deps.admit` plus the
admit-wait progress goroutine, `forwardConfineSignals`, `waitConfineCommand`,
`classifyConfineTermination`, `reportPeak`.

**Skipped entirely, up front, never attempted-and-failed** (requirement 2):
`resolveDefaultConfineSlice` / `resolveSlicePath`, `backend.Probe`,
`deps.readCap` and the finite-cap refusal, `deps.ensureDelegation`,
`backend.Create`, `writeOOMGroup`, `memory.swap.max`, the CPU-weight aging
control, `UseCgroupFD` placement and its proof, the AIRA-20 descendant-escape
attestation, `cleanupConfineScope`, `readCgroupUsage`.

Because the branch is above slice resolution, **no cgroup syscall is issued at
all** in shim mode — which is what makes test (a) checkable by counting syscalls
rather than by reading log text.

### 5.3 Peak-RSS feedback in shim mode

`readCgroupUsage` reads the scope's `memory.peak`; there is none. Without a
replacement the AIRA-67 estimator would never learn and would sit on the
machine-wide prior forever, which would make requirement 3's "reusing the
existing estimator" hollow.

Replacement: `wait4`'s `ru_maxrss` for the direct child, taken from
`cmd.ProcessState.SysUsage()`, reported through the unchanged `reportPeak` with
a **distinct provenance marker** so a shim-derived sample is never confused with
a cgroup-derived one. Its limit is stated rather than glossed: `ru_maxrss` is the
maximum RSS of the child **and its reaped descendants**, not a
simultaneous-total like `memory.peak`, so a job whose peak comes from many
concurrent children is under-measured. That is an under-estimate in the
permissive direction, so it is recorded in the residuals and surfaced in the
provenance rather than presented as equivalent.

### 5.4 The flag surface stays exactly as it is (requirement 6)

`cmd/aira/main.go`'s `parseConfineArgs` is **not touched**, and neither is
`ConfineRequest`. `--memory-max`, `--memory-high`, `--memory-reserve`,
`--delegate-ram`, `--slice`, `--name`, `--owner`, `--admit-timeout` all parse
exactly as today. `aira confine --memory-max 32G --memory-reserve 512M -- make
merge-gate` — the consumer's real merge-gate invocation — runs unchanged.

Requirement 6 is met **structurally**, not by a promise: the shim branch is
*below* the parser and *below* `ResolveConfineReserve`, so there is no location
in the code where a shim-specific rejection could be written without an obvious,
reviewable step backwards.

What each flag *means* in shim mode, stated precisely because "inert" is too
coarse and would be dishonest for two of them:

| Flag | Shim-mode effect |
|---|---|
| `--memory-reserve N` | **Live.** Pins the ledger charge to N, exactly as on the real path. |
| `--memory-max N` | **Live as a ledger declaration, inert as a cgroup write.** `ResolveConfineReserve` still sets the reserve to N for a non-delegate job (portable code, untouched), so the job books N against the container budget. No `memory.max` is written because there is no scope. |
| `--memory-high N` | **Fully inert.** It has no meaning other than a cgroup write. Accepted, validated, ignored. |
| `--delegate-ram` | **Live** for the pinned framework-overhead reserve and the scope-ceiling resolution; its `AIRA_AITEST_LIB` export is conditioned out — see §5.5. |
| `--exclusive` | **Refused** (§4.4 item 2). Not a resource flag, and degrading it silently is what AIRA-101 exists to forbid. |

`--memory-max` remaining live as a declaration is worth calling out in review:
it is what makes shim mode's ledger actually bind for the consumer's merge gate,
and it is why "accept-and-ignore" in the ticket's wording must be read as
"accept, and do not attempt the cgroup write", not as "discard".

### 5.5 `AIRA_AITEST_LIB` and the AIRA-123 seam (requirement 7 — INTERIM)

Today `confine_linux.go` calls `pylib.AppendAitestChildEnvironment` inside
`if request.DelegateRAM`, with an else-arm that unconditionally calls
`pylib.StripAitestEnvironment`. The change is to make that condition a named,
single-purpose predicate in the **portable** file `internal/runner/confine.go`:

```go
// AitestBackendCanFunction reports whether aitest's per-worker RAM containment
// backend can actually work for a launch in this mode. It is the ONE gate on
// publishing the AIRA_AITEST_* coordinates to a child.
//
// A consumer's conftest.py uses the PRESENCE of AIRA_AITEST_LIB alone as the
// guard that activates the aitest plugin. So exporting it where worker-admit
// cannot place a worker in a nested cgroup sub-scope would activate aitest,
// aitest would attempt per-worker cgroup admission that structurally cannot
// succeed, and a heavy suite would run under an apparent governance mechanism
// with no backstop -- "invisible until something OOMs".
//
// INTERIM, by explicit ticket decision (AIRA-121 req 7). AIRA-123 extends
// worker-admit to a degraded ledger-only admission mode with no cgroup
// sub-scope, honestly reported as advisory. When it lands, THIS FUNCTION is
// what changes -- the rule becomes "export if the degraded backend can work in
// this mode", not a flat never. Nothing else at either call site moves.
func AitestBackendCanFunction(mode confineMode) bool { return mode == confineModeReal }
```

Both launch sites call it. In shim mode the else-arm runs for **both** delegate
and non-delegate launches, so the child's environment is actively **stripped**
of any inherited `AIRA_AITEST_*` — not merely "not set". That distinction is
load-bearing: a shim-mode `aira confine` nested inside some outer aitest-enabled
process must not inherit a live `AIRA_AITEST_LIB` pointing at an extraction
directory, which is precisely the stale-coordinate resurrection the existing
unconditional strip was added to prevent.

`AIRA_CONFINE_SCOPE_ID` is likewise **not** exported in shim mode (there is no
scope), so `InheritedConfineScopeID` finds nothing and `confine-reserve`
sub-reservations correctly do not attach.

Test (f) proves this by inspecting the **actual child environment** — the child
is a helper binary that dumps `os.Environ()` — never by inspecting the
flag-parsing path, exactly as the ticket demands.

### 5.6 Process-group signal forwarding (requirement 8)

**What is actually true today, restated so the change is scoped correctly.** The
real path already forwards SIGINT/SIGTERM to its immediate child via
`child.Signal(received)` in `forwardConfineSignals`; it does *not* eat the
signal. `cgroup.kill`'s distinct value on the real path is reaching
**descendants** a single-PID `Signal()` cannot — a forked or reparented
grandchild. Shim mode has no `cgroup.kill`, so that class is the gap, and it is
narrower than it was originally framed but real.

**The change.**

- The shim launch sets `SysProcAttr{Setpgid: true}`, so the child becomes the
  leader of a new process group whose pgid equals its pid.
- `confineCommand` gains a `signal(os.Signal) error` method:
  - real path: `cmd.Process.Signal(sig)` — today's behaviour, byte-identical;
  - shim path: `unix.Kill(-pgid, sig)`.
- `forwardConfineSignals` calls `command.signal(received)` instead of
  `child.Signal(received)`. One method, two implementations, **no mode branch
  inside the forwarder** — so the real path cannot regress through this change.
- Because there is no `cgroup.kill` teardown, the shim path adds a bounded
  escalation after forwarding: wait the existing 2s teardown budget (the same
  constant `waitEmpty` already uses — reused, not a new tunable) for the group to
  exit, then `unix.Kill(-pgid, SIGKILL)`.

**Two correctness details that must be in the implementation and in review.**

1. `Setpgid` puts the child *outside* the supervisor's process group. A terminal
   Ctrl-C signals the foreground process group, so it now reaches the supervisor
   only, and the forwarder becomes the sole delivery path to the job. That is
   the intended design and is what test (g) exercises — but it means a bug in
   the forwarder is now a *total* loss of Ctrl-C rather than a partial one, so
   the test asserts delivery to the grandchild, not merely to the child.
2. A pgid is only valid until the leader is reaped. Signal delivery is gated by
   the same `runEnded` cut-off the real path already has, **and** never issued
   after `cmd.Wait()` has returned, so a recycled pgid can never be signalled.

**The documented exception, stated plainly and tested as a negative.** A
descendant that calls `setsid()`, or is double-forked into a new session or
process group, has left the group and is **unreachable** by `kill(-pgid, …)`.
No non-cgroup mechanism reaches it. Shim mode therefore does *not* have parity
with `cgroup.kill`, and every place that says so — `FormatConfineStatus`, the
`--status` text, SKILL.md — says it in those words. Test (g) asserts the escape
rather than papering over it, in the same spirit as AIRA-70's documented sub-2ms
scope-escape gap.

Concretely for GCP Batch, which is why this matters: Batch sends SIGTERM on job
timeout and on preemption. Without group forwarding, a workload's children never
see it and lose whatever graceful teardown they had until Batch's harder kill
lands later.

### 5.7 `aira run`

Test (a) says "confine/**run** jobs execute successfully", so `runner_linux.go`
gets the same treatment: the same early mode branch, skipping scope creation,
using the **shared** `shimLaunchAttrs` + group-signal helpers from §5.6, with its
cgroup-derived telemetry facets reporting their established unevaluated values
and the same `containment: advisory` projection.

**Recorded decision point, not a silent trim.** `run` carries a much larger
surface than `confine` (project ledger, telemetry, PTY, `--detach`). If
integration proves to need more than the shared helper, `run` is deferred to a
follow-up ticket and this change ships confine-only — and that fork is written
down in the ticket resolution and reported to the owner, never quietly taken.

---

## 6. Honest reporting: the `containment` facet (requirement 3)

One new field on `ConfineStatus`, with exactly **two** values, both always
established, so it can never render as a fake zero or an absent facet:

```go
type ConfineContainment string

const (
    ConfineContainmentEnforced ConfineContainment = "enforced" // per-job cgroup scope + cgroup.kill
    ConfineContainmentAdvisory ConfineContainment = "advisory" // ci-shim: ledger admission only
)
```

`FormatConfineStatus` — already the single operator-facing projection — renders
`containment=enforced` or
`containment=advisory(ci-shim,no-cgroup,no-kill-backstop)`. Because it is one
projection, the foreground trailer, `confine --list`, `confine --status`, the
AIRA-22 detached-job record and the TUI all get it without further work, which
is what makes them impossible to drift apart. That is test (c).

Every cgroup-derived facet keeps its **existing** unevaluated value in shim mode
rather than gaining a new spelling — one vocabulary per primitive:

| Facet | Shim value | Why |
|---|---|---|
| `Slice` | `"ci-shim"` | Distinguishable from any real slice name at a glance |
| `Cap` | `ConfineCapUnevaluated` | No cap was established |
| `CapBytes` | `0` | **Never the budget.** The budget is the *ledger's* number, not this scope's enforced cap; putting it here would be the single most misleading value in the change |
| `Scope` | `ConfineScopeUnverified` | No placement to verify |
| `OOMGroup` | `ConfineOOMGroupUnverified` | No `memory.oom.group` |
| `ScopeIntegrity` | its unevaluated value | Nothing to attest |
| `ScopeSwapCap` | `ConfineSwapCapUnevaluated` | No `memory.swap.max` |
| `CPUWeight` | `ConfineCPUWeightUnavailable` | No `cpu.weight` |
| `Priorities` | `ConfinePrioritiesApplied` when applied | `nice`/`ionice` genuinely work without cgroups — a real fact, kept |
| `Admission` / `ReserveBytes` / `ReserveBasis` | fully meaningful | The ledger admission really happened |

`confine --list`'s existing `slice reserve: <granted>/<ceiling> across N job(s)`
summary line gains the mode: `slice reserve: … (ci-shim advisory budget from
container cgroup memory.max)`.

This follows the project's own established idiom for this class of trade-off —
aitest's "mark unevaluated rather than run unconfined silently" — rather than
inventing a new one, as requirement 3 instructs.

---

## 7. aitest under shim mode (requirement 5)

**No aitest code changes.** Two paths, both proven rather than assumed:

- **Requirement 7's path (the shipped default).** `AIRA_AITEST_LIB` is unset, so
  a consumer's `conftest.py` guard falls through and the suite runs under plain
  pytest / pytest-xdist. A RAM-blind backend beats a broken RAM-aware one.
- **The fallback path (proving req 5's claim).** With the aitest coordinates set
  by hand — as the existing aitest tests already do — the plugin activates,
  `worker-admit` answers `unevaluated` (§4.5), and aitest's existing
  daemon-down/no-grant fallback completes the whole suite via its bare
  `os.fork()` pool with **one** honest warning, no cgroup placement attempted,
  and time/test-count worker recycling still governing (RAM-watermark recycling,
  the only piece needing a granted scope, skipped). That is test (d).

The follow-up — letting aitest's `auto` worker sizing consult the shim ledger
when a daemon *is* present — is explicitly not done here and is noted on
AIRA-123.

---

## 8. Test plan

TDD throughout: every test below must be written to **fail against the current
tree or against the plausible wrong implementation**, and that counterexample is
recorded in the test's own doc comment. Heavy commands run under
`aira confine -- `; the real-cgroup tier runs under `AIRA_REAL_CGROUP=1`.

| # | Ticket test | File | What makes it non-porous |
|---|---|---|---|
| **a** | Probe forced to fail → shim mode; plain-process daemon; confine/run succeed with **no cgroup scope created** | `internal/install/ci_shim_mode_test.go`, `internal/runner/confine_shim_linux_test.go` | Injected `installDeps.run` fails `systemctl --user`; asserts `mode=="ci-shim"` in `install-mode.json`, that **no** systemd argv was ever run (the fake records every `d.run`), and — the load-bearing half — that the confine launch issued **zero** `mkdir`/`openat` against any cgroup path, via a `confineDeps` whose `newBackend` **panics if called**. A "we attempted and it failed gracefully" implementation therefore fails the test, which is exactly requirement 2's distinction. |
| **b** | Ledger gates admission: job 2 queues while job 1 has booked the budget, and is admitted when job 1 completes | `internal/daemon/shim_ledger_test.go` | Real `Server` with `confineMode=shim` and a small injected budget; job 1 pins a reserve covering it; job 2's `grantedCh` must **not** close within the poll window (asserted by a timed negative), then must close after job 1's release. The negative half is what fails against a no-op ledger; a wrong implementation that admits everything passes only the positive half. |
| **c** | `--list`/`--status` report advisory containment, distinguishably from the real-slice case | `internal/runner/confine_status_test.go` | Table over `FormatConfineStatus`: shim status must contain `containment=advisory` **and** must not contain `cap=enforced`; the real status must contain `containment=enforced`. Plus an assertion that `CapBytes==0` in shim mode even when a budget is configured — the specific fabrication §6 forbids. |
| **d** | A real aitest suite completes in shim mode end-to-end | `internal/pylib/pytest_aitest_shim_e2e_test.go` + `internal/pylib/aitest/test_supervisor.py` | Runs a real pytest suite against a shim-mode daemon whose `worker-admit` answers `unevaluated`; asserts exit 0, **all** tests collected and reported, exactly **one** fallback warning, and **no** `admit` subprocess placement — mirroring the existing `test_daemon_down_fallback_completes_suite_with_one_warning_no_admit_subprocess`, which is the proof it is a real end-to-end run and not a stub. |
| **e** | `--memory-max/--memory-reserve/--delegate-ram` all parse and run in shim mode | `internal/runner/confine_shim_linux_test.go` | Runs the consumer's literal invocation shape (`--memory-max 32G --memory-reserve 512M -- <helper>`) end to end in shim mode and asserts exit 0. Second assertion, the one that catches a lazy "ignore them all": `--memory-max 32G` without `--delegate-ram` must produce a **ledger charge of 32G** (read off the admit request), proving the flag is live-as-a-declaration per §5.4, not discarded. |
| **f** | Shim `--delegate-ram` does **not** set `AIRA_AITEST_LIB` in the child env | `internal/runner/confine_shim_env_test.go` | The child is a helper that dumps `os.Environ()` to a file; the test asserts the **actual child environment** has no `AIRA_AITEST_*` key. Includes an inheritance case: the supervisor is given a live `AIRA_AITEST_LIB` in its own env and the child must still not have it (proves the *strip*, not merely the not-set). Ticket-mandated: never inspect the flag path alone. |
| **g** | Group signal reaches a grandchild; documented exception for a detached descendant | `internal/runner/confine_shim_signal_linux_test.go` | Child forks a grandchild that writes a marker file on SIGTERM; the test signals the **confine supervisor** and asserts the grandchild's marker appears. Fails against today's single-PID `child.Signal`, which is the required counterexample. Second, **negative** case: a grandchild that `setsid()`s first must **not** receive it — the escape is asserted, matching the documented exception rather than hiding it. Third: SIGKILL escalation fires after the grace when the group ignores SIGTERM. |
| **h** | `docker build`-shaped build stage: no network, no daemon, no systemd; start stage is what launches the daemon | `internal/install/install_stage_test.go` | Runs the **real binary** as a subprocess with `PATH` stripped of `systemctl`/`dbus`, `HOME` in a temp dir, and no daemon running; asserts `--stage=build` exits 0 and writes `install-mode.json`; asserts **no socket exists and no daemon process was started** after the build stage (the half that fails against a build stage that quietly starts things); then asserts `--stage=start` is what creates the socket. Plus: `--stage=start` with no recorded plan must fail `E_INSTALL_UNAVAILABLE`. |
| **i** | The container exits promptly when the workload exits, not blocked on the background daemon | `internal/install/shim_daemon_detach_test.go` | Models the container entrypoint: a parent process starts the shim daemon via the start stage, then runs a short workload, then exits. The test reads the **parent's stdout pipe to EOF** with a deadline — an inherited pipe held by the daemon is exactly what would hang this — and asserts EOF and parent reap within a few seconds while the daemon is **still alive**. That last conjunct is what stops the test being satisfied by simply killing the daemon. |

**Additional regression tests not enumerated in the ticket but required by
decisions taken above** (each guards a specific way this change could go wrong):

- Real mode is unchanged: the full existing confine/admit suites pass with no
  `install-mode.json` present, and a separate test asserts
  `ResolveConfineMode()` returns `real` for absent / malformed / unknown-mode
  records (§5.1's fail-to-stricter rule).
- `--exclusive` in shim mode is refused with `CodeAdmitExclusiveUnestablished`,
  never granted — the §4.4 fabrication guard.
- `--ci=shim` with no readable `memory.max` and no `MemTotal` **fails install**
  rather than installing an ungated shim (§4.2 precedence step 3).
- `readShimMemory` clamps to the recorded budget when the live `memory.max` is
  larger (a runtime that widened the limit must not widen the ledger).
- `install-mode.json` `home`/`uid` mismatch at `--stage=start` is refused.

### Green gate before the PR (exact exit codes recorded, never inferred)

```
aira confine -- go build ./...
aira confine -- go vet ./...
AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1
```

---

## 9. Files touched

| File | Change |
|---|---|
| `internal/install/capability.go` | **new** — `CapabilityReport`, `ProbeCapability` |
| `internal/install/mode.go` | **new** — `install-mode.json` read/write, atomic |
| `internal/install/install.go` | `--ci=<value>`, `--stage`, split into `planInstall`/`applyInstallBuild`/`applyInstallStart`, shim daemon spawn, mode reporting, `--status` additions |
| `internal/runner/confine.go` | `ConfineContainment` + facet, `AitestBackendCanFunction`, `FormatConfineStatus` |
| `internal/runner/confine_mode.go` | **new** — `ResolveConfineMode`, cached record read |
| `internal/runner/confine_shim_linux.go` | **new** — `confineShim` launch path |
| `internal/runner/confine_linux.go` | mode branch; `confineCommand.signal`; `forwardConfineSignals` call-site change; `AitestBackendCanFunction` at the env site |
| `internal/runner/runner_linux.go` | same mode branch for `aira run` (§5.7) |
| `internal/daemon/paths.go` | `AIRA_DAEMON_CONFINE_MODE` / `_SHIM_BUDGET_BYTES` / `_SHIM_BUDGET_SOURCE` / `_SHIM_CGROUP_PATH` validation |
| `internal/daemon/server.go` | `confineMode`, `shimBudget`; shim wiring of the three seams |
| `internal/daemon/shim.go` | **new** — `resolveShimSlicePath`, `readShimMemory`, the empty-but-successful scanner |
| `internal/daemon/admit.go` | shim `--exclusive` refusal; `liveScopesKnown`/`capAggregateKnown` in shim mode |
| `internal/daemon/worker_admit.go` | shim `unevaluated` response |
| `internal/core/skill.go` | shim-mode text: advisory containment, the setsid exception, `AIRA_AITEST_LIB` interim |
| `cmd/aira/main.go` | `confine --list` summary line gains the mode. **`parseConfineArgs` untouched** (requirement 6). |

---

## 10. Risks and residuals, written down and accepted

1. **A shim daemon restart forgets the ledger** and can over-admit until
   in-flight jobs finish (§4.4 item 4). AIRA-74's reconstruction has nothing to
   reconstruct from. Accepted; advisory guarantee; no second persistence
   mechanism.
2. **`ru_maxrss` under-measures a many-concurrent-children job** relative to
   `memory.peak` (§5.3), so the AIRA-67 estimator learns a permissive number in
   shim mode. Surfaced through a distinct provenance marker rather than blended
   with cgroup-derived samples.
3. **A `setsid`/double-forked descendant escapes the group signal** (§5.6). No
   non-cgroup mechanism reaches it. Documented in three places and asserted by a
   negative test.
4. **No kill backstop at all.** A job that exceeds its booked reserve is not
   killed; on a single-tenant CI box that costs one job rerun rather than
   collateral damage to a shared session — the owner's own framing via `deploy`,
   and the reason advisory admission is worth having.
5. **`aira run` may prove larger than the shared helper** (§5.7). Explicit
   decision point: defer `run` to a follow-up and ship confine-only, reported,
   never silently trimmed.
6. **`/proc/meminfo` in the `meminfo-memtotal` fallback is host-wide** on most
   runtimes. That fallback is only reached when the container declares no
   memory limit — in which case the machine genuinely is the budget — but it is
   recorded as the weaker source and printed as such.

---

## 11. Expected yield

- A GCP Batch (or AWS Batch / Fargate / k8s Job / container CI) image can bake
  `aira` at `docker build` time and run jobs in fresh containers with **no
  `if CI` branching in any recipe** — the consumer's existing
  `aira confine --memory-max 32G --memory-reserve 512M -- make merge-gate`
  invocation runs verbatim.
- Over-subscription of a container's RAM is genuinely prevented by ledger
  admission, using the same AIRA-67 estimator the real path uses.
- Batch's SIGTERM on timeout/preemption reaches the workload's descendants, so
  graceful teardown survives.
- Every weakening relative to the real slice is named at the point of use, in
  the project's existing unevaluated/advisory vocabulary, so nothing claims a
  containment guarantee it does not have.
