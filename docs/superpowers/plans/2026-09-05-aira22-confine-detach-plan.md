# AIRA-22 — session-independent `aira confine --detach` + durable owner-keyed status

Ticket: `.aira/tickets/AIRA-22.md` (P2, `feature`, labels `confine detach dogfood
survivability`). Tier A in
`docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md` §4: full two-loop,
"converge onto the existing `LaunchDetached` machinery rather than building a
parallel detach path", "new durable-status surface".

Base: `origin/master` @ `c4813db` (Phase 0, Fixes 1–5, Phase 2 stage A / AIRA-85
relay-writes and Fix 2's structured outcome channel all merged).

---

## 1. Problem, restated against current source

A ~1 hour `make merge-gate` under `aira confine --delegate-ram` died at 98% when
the launching Claude session paused. Confirmed by code read at `c4813db`:

1. **The supervisor is a plain foreground descendant.** `confineWithDeps` builds
   the setup child with `SysProcAttr{UseCgroupFD, CgroupFD}` and *no* `Setsid`
   (`internal/runner/confine_linux.go:865`). `aira confine` itself is just
   `runConfineCommand` → `runner.Confine` on the CLI's own goroutine
   (`cmd/aira/main.go:1019`). It is in the launching shell's session and process
   group.
2. **`nohup` cannot help.** `confineSignalSource` notifies on `SIGINT` and
   `SIGTERM` only (`confine_linux.go:1449`); SIGHUP is not handled, so `nohup`'s
   SIGHUP-ignoring buys nothing, and the harness that pauses a session does not
   send SIGHUP anyway.
3. **A caught signal tears the whole scope down.** The handler calls
   `cleanup()` → `cleanupConfineScope` → `scope.Kill()` (`confine_linux.go:697-698`,
   `:1696-1707`).
4. **Output is the launching session's fds.** `cmd.Stdin/Stdout/Stderr` come
   straight from `request.Stdin/Stdout/Stderr`, i.e. the terminal
   (`confine_linux.go:815-821, :863`). Nothing is captured anywhere.
5. **There is no durable record and no status verb.** The daemon's only record is
   the connection-scoped, in-memory `admitWaiter` (`internal/daemon/confine_manage.go:15-32`);
   when the supervisor dies the connection closes and the scope leaves `--list`.
   `parseConfineManagementArgs` accepts only `--list` and `--kill`
   (`cmd/aira/main.go:902-950`).

So a long confined job cannot outlive its launching session, and even a *finished*
job's result is unrecoverable across a pause.

## 2. Scope

**In scope**

- `aira confine --detach [...] -- <argv>`: a `setsid`'d, reparented supervisor
  decoupled from the launching session, with the launcher returning promptly.
- A durable, per-job record store under the XDG state home holding `record.json`
  plus captured `stdout` and `stderr`.
- `aira confine --status <name|supervisor-pid|scope-id>` (`--json` supported),
  reporting `running` / `finished` / `outcome-unknown` with no fabricated outcome.
- Convergence: one shared detach-control-file primitive and one shared
  `setsid` shim-spawn shape used by both `run --detach` and `confine --detach`.
- Face/doc updates: `internal/core/core.go` confine usage + arg specs,
  `internal/core/skill.go` (its current prose asserts "`aira confine` has no
  native `--detach` of its own yet" — that sentence becomes **false** and must
  be replaced, not merely appended to).

**Out of scope, with reasons (§7 expands)**

- Any `internal/daemon` change (see §3.5: none is required, and the reason is a
  design property, not an omission).
- Record-store pruning/retention (matches `run --detach`'s existing unpruned
  output dir; §7).
- MCP exposure of `--status` (§3.4).
- Attaching to a detached job's live output (`tail -f` on the printed path is the
  primitive; a follow verb is `run-log`'s job, not confine's).

## 3. Design

### 3.1 Convergence with `LaunchDetached`, and where it stops

`LaunchDetached` (`internal/runner/detach_linux.go:20-124`) is the right shape and
the wrong body. Its shape:

```
write control file -> pipe(ready) -> spawn /proc/self/exe __supervise
  with Setsid:true, stdio=/dev/null, ExtraFiles=[readyW, ackR]
  -> read one JSON ready message -> ack
```

Its body is bound to `Runner`: `r.outputDir`, `r.ledger`, `RunRecord`,
`reserveID`, the supervisor lease, `r.append(ledgerEvent{...})`. `aira confine`
is deliberately **project-less and daemon-optional** — it has no ledger, no run
id, no project. Copying `launchDetachedValidated` into confine would import all
of that.

So convergence is at the **primitives**, and it is real code sharing, not
imitation:

- `writeDetachControl` / `consumeDetachControl` (`detach_control.go`) are
  generalised to `writeControlValue(dir, pattern string, value any) (string, error)`
  and `consumeControlValue(path string, into any) error` — same temp-file +
  `Chmod(0600)` + `Sync` + `syncDir` + `DisallowUnknownFields` + trailing-data
  rejection behaviour, byte-for-byte. The existing two functions become one-line
  wrappers, so the run path's behaviour is unchanged and its existing tests keep
  proving it.
- The shim spawn becomes `spawnDetachedShim(selfPath string, argv []string,
  extra []*os.File) (*exec.Cmd, error)` — `Setsid: true`, stdio to `/dev/null`,
  `go cmd.Wait()`. `LaunchDetached` and the new confine launcher both call it, so
  "detached means setsid + no inherited stdio" has exactly one definition.
- `detachSignal` / `detachReadyMessage` (`detach_protocol.go`) are reused as-is
  for the ready channel. `detachReadyMessage.ID` carries the scope id.

**The ack is kept too** (revised in plan-review round 1; a first draft dropped it,
and all three review lineages rejected that independently). Its purpose is not
"destroying a job the operator asked for" — it is keeping the launcher's *report*
and the supervisor's *action* consistent. Without it, a ready-message read that
times out, or a launcher killed between ready and printing, prints
`E_CONFINE_DETACH_FAILED` while the setsid'd supervisor goes on to admit and run
an hour-long job under the operator's owner and reserve, with the handle never
delivered to anyone. That is a job the user was told did not start. The M20
detach design states the same property
(`docs/superpowers/specs/2026-08-15-aira-m20-detach-shim-design.md:139-166`).

The confine ack is *cheaper* than the run one, because of where it sits (§3.6):
the supervisor blocks for it immediately before admission, when it has created
nothing but its own record directory — no scope, no admission charge, no child.
Cancellation is therefore "write a terminal `cancelled` record and exit", not a
teardown.

### 3.2 Who mints the scope id — load-bearing

`confineScopeID(name, owner, delegateRAM)` embeds `os.Getpid()`
(`confine_linux.go:1105`). That PID is not decoration; three subsystems read it
back out of the scope directory name:

- `aira confine --kill <pid>` selector matching (`confine_manage_linux.go:376-381`),
- `ConfineRecord.SupervisorPID` in `--list` (`:92`),
- the orphan reaper's `supervisorDead(*record.SupervisorPID)` predicate
  (`confine_manage.go:158`).

If the *launcher* minted the id, the embedded PID would belong to a process that
exits seconds later. `--kill <pid>` would be unusable, `--list` would show a dead
supervisor for a live job, and the reaper would consider the scope orphaned the
moment it is transiently empty. **Therefore the detached supervisor mints its own
scope id**, before it opens the record store, and passes it into `Confine`.

**The override is package-private and fully bound** (revised in plan-review round
1; all three lineages rejected the first draft's "exported field, syntax check
only"). `ConfineRequest.ScopeID` is exported and currently assigned only *inside*
`confineWithDeps:509`; no production caller sets it (verified: the only
`ConfineRequest{... ScopeID: ...}` literals are in
`confine_queue_position_linux_test.go`). Exporting a *settable* pre-mint would let
any caller name a scope after a foreign PID, a foreign owner, or the wrong
delegate-RAM class — and `validConfineScopeID` cannot catch any of that, because
the grammar accepts any canonical pid/stamp/owner/`@dr` combination
(`confine.go:487-533`). The daemon binds only name and owner at admit
(`internal/daemon/admit.go:1430-1463`) and a non-special refusal routes the client
to the flock fallback (`admission_linux.go:400-450`) — i.e. an *unaccounted* launch
in a misnamed scope.

So: a new **unexported** `ConfineRequest.presetScopeID` field, settable only from
within package `runner` (the detached supervisor lives there), plus a fail-closed
parity check binding it to the request:

```go
scopeID := request.presetScopeID
if scopeID == "" {
    scopeID = confineScopeID(name, owner, request.DelegateRAM)
} else if err := bindConfineScopeID(scopeID, name, owner, request.DelegateRAM); err != nil {
    return result, fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: %w", err)
}
```

`bindConfineScopeID` requires: the id parses; its embedded pid equals
`os.Getpid()` (the entire §3.2 argument, now enforced rather than assumed); its
embedded name equals the *defaulted* name; its embedded owner equals the request
owner (absent for unknown/empty); and `IsDelegateRAMScopeID(id) == DelegateRAM`.
Never silently re-mint: a re-mint would put the job in a scope directory the
record does not name, which is exactly the "record and reality disagree" failure
this ticket exists to end.

`normalizeConfineIdentity(request) (name, owner string, err error)` is the single
place name-defaulting (`""` → `"job"`), owner-defaulting (`""` → `unknown`) and
identity validation happen; `confineWithDeps` and the supervisor's
`MintConfineScopeID` both call it, so the two cannot drift and no parity *test* is
needed to hold them together (a shared normaliser is stronger than a test).

### 3.3 The record store

Root: `<XDG state home>/aira/confine/`, i.e. `~/.local/state/aira/confine/`,
added to `daemon.Paths` as `ConfineStateDir` alongside `LeaseStateDir` and passed
into the request by the CLI (`runner` must not import `daemon`; the existing
`RuntimeDir`/`AdmitSocketPath` fields set the precedent).

Deliberately **state home, not `RuntimeDir`**: `RuntimeDir` resolves under
`XDG_RUNTIME_DIR`, which is tmpfs — i.e. RAM. Capturing an hour of a merge gate's
stdout into RAM inside a project whose entire purpose is bounding RAM would be
self-defeating.

Per job: `<root>/<scope-id>/` with `record.json`, `stdout`, `stderr`, and
`supervisor.log`. The scope id is validated with `validConfineScopeID` before it
is ever joined into a path (it cannot contain `/`, and the parser is
canonical-round-trip strict), so the store cannot be steered outside its root.

```go
type ConfineDetachRecord struct {
    Schema     int            `json:"schema"`      // ConfineDetachSchema = 1
    ScopeID    string         `json:"scope_id"`
    Name       string         `json:"name"`
    Owner      string         `json:"owner"`
    Slice      string         `json:"slice,omitempty"`   // resolved; empty before the launch gate
    CapBytes   int64          `json:"cap_bytes,omitempty"`
    Argv       []string       `json:"argv"`
    Cwd        string         `json:"cwd"`
    EnvDigest  string         `json:"env_digest,omitempty"` // digest, never the environment itself
    Supervisor PIDIdentity    `json:"supervisor"`  // PID + StartTick + BootID
    Phase      string         `json:"phase"`       // starting | admitting | running
    StartedAt  string         `json:"started_at"`
    AdmittingAt string        `json:"admitting_at,omitempty"`
    RunningAt  string         `json:"running_at,omitempty"`
    EndedAt    string         `json:"ended_at,omitempty"`
    Terminal   bool           `json:"terminal"`
    Exit       *int           `json:"exit,omitempty"`
    ErrorCode  string         `json:"error_code,omitempty"`
    Error      string         `json:"error,omitempty"`
    Status     *ConfineStatus `json:"status,omitempty"`
    StdoutPath string         `json:"stdout_path"`
    StderrPath string         `json:"stderr_path"`
    SupervisorLogPath string  `json:"supervisor_log_path"`
}
```

The environment is stored as a **digest only**, never verbatim: a confine job's
environment routinely carries credentials, and this store is a durable file the
whole ticket exists to make long-lived.

**Store hardening** (plan-review round 1, raised by two lineages). The record
directory is created with `os.Mkdir(dir, 0o700)` — *not* `MkdirAll` — so an
existing directory is a hard error rather than a silent adoption of someone
else's; the root is `MkdirAll(root, 0o700)`. Every file inside is opened
`unix.Openat` **relative to the directory fd** with `O_NOFOLLOW|O_CLOEXEC` and
mode `0600`, mirroring the fd-anchored discipline the confine kill/reap paths
already use (`confine_manage_linux.go:99-106, :212-217`). The atomic record write
is `Unlinkat(tmp)` (ignoring ENOENT) → `Openat(tmp, O_CREAT|O_EXCL|O_WRONLY|O_NOFOLLOW)`
→ write → `Fsync` → `Renameat(dirfd, tmp, dirfd, "record.json")` → `Fsync(dirfd)`.
Readers open `record.json` `O_NOFOLLOW` too.

**`supervisor.log` and the limits of "every exit path writes a terminal record".**
`spawnDetachedShim` gives the supervisor `/dev/null` on fd 2, so a Go runtime
`fatal error` — a panic on a non-main goroutine, which skips every deferred
writer — would vanish without trace *and* leave a permanently non-terminal record.
The supervisor therefore `dup2`s its own fd 2 onto `supervisor.log` as soon as the
record directory exists. The terminal-record guarantee is then stated honestly:
deferred writers cover every ordinary and panicking return from the main
supervisor goroutine; a runtime-fatal crash or SIGKILL leaves a non-terminal
record, which `--status` reports as `outcome-unknown` — correctly — with
`supervisor.log` naming the cause.

`ConfineStatus` gains `json:` tags (it is marshalled nowhere today, so this is
additive). Storing the structured status rather than the rendered trailer keeps
`FormatConfineStatus` the single operator-facing projection — `--status` renders
the stored facets through it, so a detached job's trailer and a foreground job's
trailer cannot drift.

Writes are atomic: `record.json.tmp-<pid>` → `Sync` → `Rename` → `syncDir`. A
reader therefore never sees a half-written record; a crash mid-write leaves the
previous complete record.

`Terminal` is the single bit meaning "the supervisor wrote an outcome". `Exit` is
nil when `Confine` returned an error before the child ran (uncapped slice,
admission rejected, scope creation failed) — in that case `ErrorCode`/`Error`
carry it and `Exit` stays absent rather than becoming a fabricated 0 or 1.

### 3.4 `--status` resolution — the honesty core

```
record unreadable / malformed                  -> outcome-unknown (reason: record unreadable)
terminal record present                        -> finished   (exit / error_code / status facets)
non-terminal + identity matches + phase=running    -> running
non-terminal + identity matches + phase<running    -> admitting
non-terminal + identity does NOT match         -> outcome-unknown
non-terminal + identity UNVERIFIABLE           -> outcome-unknown
```

**`admitting` is a separate state from `running`, not a nicety** (plan-review
round 1, raised by all three lineages). A live supervisor proves only that the
supervisor is alive. Admission on a contended slice legitimately queues for tens
of minutes (default max wait 30m, ceiling 24h — `confine_linux.go:1026-1028`,
`confine.go:42`), and a queued waiter is not `admitGranted`, so
`aira confine --list` shows nothing for it either
(`internal/daemon/confine_manage.go:25`). Reporting `running` there would tell an
operator watching a 40-minute-old job something *false*, not merely incomplete.
The `phase` field is written by the two launch-gate callbacks in §3.6 — it is
observed, never inferred.

A record that exists but cannot be read or parsed is `outcome-unknown` with a
reason, **never** `E_CONFINE_NOT_FOUND`: "I cannot tell you" and "there is no such
job" are different answers and only one of them is true.

Identity is the full `PIDIdentity` triple, compared exactly as the run path does
it: a different `BootID` means the machine rebooted (the job is gone), a different
`StartTick` means the PID was reused, and an unreadable `/proc/<pid>/stat` means
*unevaluated* — which resolves to `outcome-unknown`, never to `running`. The
whole point of the verb is to let a resumed session "pick up the RESULT instead
of re-running", so the one thing it must never do is imply a result it does not
have. `outcome-unknown` carries a reason string naming which facet failed, and
the captured `stdout`/`stderr` paths are printed in every state, because partial
output is real evidence even when the outcome is not.

**Selector semantics** mirror `--kill`: exact match on scope id, name, or
supervisor pid, and matching is **scoped to the caller's owner** (resolved by the
same `resolveConfineOwner` the management verbs already use) so that one session's
`--name gate` cannot be made ambiguous by another session's. Residual ambiguity —
the same owner re-using a name across sequential detached jobs — is **refused**
with `E_SELECTOR_AMBIGUOUS` (CLAUDE.md: "Ambiguous selectors are refused"), and
the message lists every candidate newest-first with its `started_at` and resolved
state, so the refusal is one copy-paste away from the answer. `--status` with no
selector lists the caller's records instead of refusing, which is the natural
companion to refusing ambiguity.

**Exit code of `aira confine --status`** reports the *status query*, not the job:
`0` for `running`/`finished`, `3` for `outcome-unknown` (`U_CONFINE_OUTCOME_UNKNOWN`),
`2` for a selector miss (`E_CONFINE_NOT_FOUND`) or ambiguity. The job's own exit
code is printed and carried in `--json`, never overloaded onto the query's exit
channel — two meanings on one channel is precisely the honesty failure this
project forbids. The help text says this outright.

**`--status` is CLI-direct, not daemon-routed**, unlike `--list`/`--kill`. Those
two cross the dispatcher because they need the daemon's in-memory admit registry
and ledger snapshot; `--status` reads a plain filesystem artifact and needs
neither. Routing it through the daemon would make the survivability feature
depend on the component most likely to have been restarted during exactly the
long pause it exists to survive. Consequently it is an option of the CLI-only
`confine` verb, not a new dispatcher verb, and it is not an MCP tool — an agent
reaches it through the skill's `Bash(aira *)` allowance like every other confine
invocation.

### 3.5 Why there is **no** daemon change (ticket item 3, answered)

The ticket proposes "daemon adoption: ... a detached job needs a persisted record
that outlives the supervisor, so the reaper/ledger keep accounting it correctly."
Read against the daemon at `c4813db`, no daemon change is required, and the
reason is structural rather than an omission:

- The detached supervisor holds its admit connection for its *entire* lifetime,
  exactly as a foreground supervisor does (`confine_linux.go:593-595`,
  `:878-880`). While the job runs, the daemon's accounting is byte-identical.
- Because the **supervisor** mints the scope id (§3.2), the restart-surviving
  carriers the daemon already relies on stay correct: AIRA-52's owner suffix,
  AIRA-74's reserve reconstruction from live scopes, and AIRA-72's orphan reaper
  PID-death predicate all read the *detached* supervisor's identity, which is the
  one that matters.
- If the detached supervisor is SIGKILLed, the outcome is exactly today's
  foreground outcome — lease released on connection close, scope reaped by the
  AIRA-72 sweep once empty + PID-dead + aged.

What the ticket actually wanted from "a persisted record" is the *client-side*
record in §3.3 — a durable answer to "what happened to my job" — not a new
daemon-side lifetime. Building daemon persistence here would add machinery for a
property the existing design already has, against the standing "keep AIRA
architecturally simple; prefer keep-the-primitive + document-the-gap" rule.

**Named precisely rather than implied away** (plan-review round 1, raised by two
lineages): there *is* a real gap here, it is **pre-existing**, and this milestone
neither closes nor widens it. If a confine supervisor — foreground or detached —
is SIGKILLed, its `cleanup()` never runs, so the child keeps running orphaned in
the scope while the daemon releases the connection-held reserve on peer close
(`internal/daemon/admit.go:758-772`). AIRA-74's ≤1s adoption scan re-adopts the
still-populated scope and reconstructs the reserve, so accounting self-heals;
`--status` reports `outcome-unknown`, which is true. Closing the residual window
properly is a daemon-side lease-persistence milestone in the AIRA-72/74 family,
and it applies identically to today's foreground form. Reviewers verified this
enumeration across nine daemon assumptions (lease-as-connection, adoption scan,
reaper PID predicate, AIRA-49 empty-scope reclaim, AIRA-52 owner binding, the
global/per-slice waiter bounds, the AIRA-24 probe's separate connection, daemon
restart, and delegate-ram/aitest scope-id keying) and found none that a detached
supervisor breaks.

### 3.6 The launch gate — where "ready" is sent, and what it means

This is the section plan-review round 1 changed most. The first draft sent ready
**before** calling `Confine`, which turned every synchronous launch failure into
`exit 0, poll later`: empty argv, a bad `--memory-reserve`, an unresolvable or
uncapped slice, a failed probe, a failed delegation — all of which
`runConfineCommand` reports *synchronously* as exit 2 or 4 today
(`cmd/aira/main.go:1019-1023`, checks at `confine_linux.go:396-467`). Detaching
must not weaken the exit contract for failures that are known before the job could
possibly have started.

**Two callbacks on `ConfineRequest`**, both `json:"-"`, both nil for a foreground
launch (which is therefore byte-for-byte unchanged):

- `BeforeAdmit func(ConfineLaunchInfo) error` — invoked once, immediately before
  `deps.admit` (`confine_linux.go:579`), i.e. after argv/reserve/owner validation,
  slice resolution, the probe, the finite-cap precondition, delegation, and the
  scope-id mint, and before the only unbounded-wait step there is. It carries the
  *resolved* slice name, effective cap, scope id and supervisor identity. A
  non-nil return aborts the launch **before admission, before scope creation,
  before any child** — nothing to tear down.
- `OnPlaced func(ConfineLaunchInfo)` — invoked once where placement is already
  proven (`confine_linux.go:920`, `result.Status.Scope = ConfineScopePlaced`),
  which is the first instant at which "running" is a fact. No error return: there
  is nothing left to abort.

Launcher (`runner.LaunchConfineDetached`):

1. Validate `SelfPath` and `DetachStateDir` (else `E_CONFINE_DETACH_FAILED` —
   never a silent fallback to a foreground run).
2. `writeControlValue` the `ConfineRequest` (its `io` fields are `json:"-"`, as
   `Request`'s already are) into the state dir.
3. `pipe(ready)`, `pipe(ack)`; `spawnDetachedShim(self, ["__confine-supervise",
   "--control", p, "--ready-fd", "3", "--ack-fd", "4"], [readyW, ackR])`.
4. Read one ready message, bounded (default 10s). An error message carries the
   supervisor's *own* code — so `aira confine --detach -- ...` against an uncapped
   slice exits 4 with `E_CONFINE_UNAVAILABLE`, exactly as the foreground form does.
5. Print the handle, flush, and **only then** write the ack byte. A failure to
   deliver the handle (closed stdout, launcher killed) leaves the ack unwritten.
6. Return `{ScopeID, RecordDir, StdoutPath, StderrPath, SupervisorLogPath,
   Slice, SupervisorPID}`.

Supervisor (`runner.SuperviseConfineDetached`, hidden `__confine-supervise` verb):

1. `consumeControlValue` the request (the control file is removed on read,
   exactly as the run path does).
2. `signal.Ignore(SIGHUP)` — see §3.7.
3. Mint the scope id (§3.2); `os.Mkdir` the record dir; `dup2` fd 2 onto
   `supervisor.log`; open the capture files; write the non-terminal record with
   `phase=starting`.
4. Call `runner.Confine` with the private pre-set scope id, `Stdin = /dev/null`,
   `Stdout`/`Stderr` = the capture files, and the two callbacks.
   - `BeforeAdmit`: record `phase=admitting` + resolved slice/cap; send ready
     `{ID: scopeID}`; block reading exactly one ack byte; on EOF/short read,
     return an error ⇒ the launch is abandoned before admission and the record
     goes terminal with `U_CONFINE_DETACH_CANCELLED`.
   - `OnPlaced`: record `phase=running` + `running_at`.
5. If `Confine` returned before `BeforeAdmit` ever fired, send the ready message
   carrying that error's code (mirroring `SuperviseRequest`'s
   `signal.sentAlready()` guard) so the launcher never waits out its timeout on a
   failure the supervisor already knows.
6. Write the terminal record: exit code (absent whenever `Confine` returned an
   error), error code/message, and the `ConfineStatus` facets.

**What ready now means: every precondition the foreground form checks
synchronously has passed, and the job is about to enter admission.** It still does
*not* mean the job succeeded — admission and the run itself are unbounded — so the
launcher's exit 0 is accompanied by wording that says so:

```
confine: detached scope CONFINE-gate-31337-abc@sess-a on aira.slice (supervisor pid 31337)
confine:   stdout <root>/<id>/stdout
confine:   stderr <root>/<id>/stderr
confine: the job's exit code is NOT known yet — this exit 0 means the supervisor started.
confine: poll it with: aira confine --status CONFINE-gate-31337-abc@sess-a
```

This is the feature's single largest false-pass risk and the wording is
load-bearing; a test pins it (§4).

### 3.7 Signals, stdin, and interaction with existing verbs

- The supervisor is `setsid`'d, so it has no controlling terminal and is not in
  the launcher's process group: neither the terminal's Ctrl-C nor a
  `kill -- -<launcher pgid>` reaches supervisor or job.
- **`SIGHUP` is ignored by the detached supervisor** (`signal.Ignore`), and the
  disposition is inherited by the job. `confineSignalSource` notifies only
  `SIGINT`/`SIGTERM`, so an explicit `kill -HUP` would otherwise *default-terminate*
  the supervisor with no teardown — orphaning a running job and freezing its record
  non-terminal forever. Ignoring it is both the survivable and the honest choice
  for a job whose entire purpose is outliving hangups; `aira confine --kill` and
  `SIGTERM` remain the supported ways to stop one.
- **"Session-independent" is bounded, and the help says so.** `setsid` decouples
  from the TTY and process group. It does not survive a full logout: the daemon
  socket lives under `XDG_RUNTIME_DIR` and `aira.slice` is a user-manager unit, so
  without `loginctl enable-linger` (which `aira install` does not set) a logout
  still takes the job. Identical exposure to `run --detach`; named as a gap
  (§7) rather than silently implied away.
- An explicit `SIGTERM`/`SIGINT` to the detached supervisor still tears the job
  down through the unchanged AIRA-70 handler, and the record's stored status
  carries `terminated-by=supervisor-signal:SIGTERM`.
- `aira confine --kill <sel>` is unchanged and still works on a detached job: it
  cgroup-kills the scope, the supervisor observes the SIGKILL, and the record
  goes terminal with `terminated-by=unattributed-sigkill`. (That verdict is
  correct and already documented: from the supervisor's evidence an external
  cgroup kill and a self-SIGKILL are indistinguishable.)
- Detached stdin is `/dev/null`: a job that reads stdin sees EOF immediately.
  Stated in the help, because "it worked in the foreground" is otherwise a trap.
- `--detach` is rejected with a targeted message when no `--` target is given
  (rather than falling through to "requires exactly one of --list/--kill/--status").
- `--detach` + `--list`/`--kill`/`--status` is an argument error; `--steal` stays
  `--kill`-only.

## 4. Changes, file by file

### `internal/runner/detach_control.go`
Generalise to `writeControlValue(dir, pattern string, value any) (string, error)`
and `consumeControlValue(path string, into any) error`; keep
`writeDetachControl`/`consumeDetachControl`/`ConsumeDetachControl` as wrappers.
No behaviour change.

### `internal/runner/detach_linux.go`
Extract `spawnDetachedShim`; `LaunchDetached` calls it. No behaviour change (the
ack pipe stays a `LaunchDetached`-local concern and is passed in `extra`).

### `internal/runner/confine.go` (portable)
`ConfineRequest`: `json:"-"` on `Stdin`/`Stdout`/`Stderr`; new `DetachStateDir
string`, unexported `presetScopeID string`, and the two `json:"-"` callbacks
`BeforeAdmit func(ConfineLaunchInfo) error` / `OnPlaced func(ConfineLaunchInfo)`.
**No `Detach bool` field** — deliberately: `admitConfine` transcribes
`ConfineRequest` fields onto a `runner.Request`, and `Request.Detach` arms
`checkDetachAdmission`, which dereferences `r.ledger` — nil in confine's
project-less admitter, i.e. a panic mid-admission inside the detached supervisor.
Detachedness is carried by the *entry point*, not by the request, so the trap
cannot exist. `ConfineStatus`: `json:` tags. New: `ConfineLaunchInfo`,
`ConfineDetachRecord`, `ConfineDetachState`, `ConfineDetachStatus`,
`ConfineDetachLaunch`, `MintConfineScopeID`, `normalizeConfineIdentity`,
`bindConfineScopeID`, `FormatConfineDetachStatus`, and the new codes
`E_CONFINE_DETACH_FAILED` / `U_CONFINE_DETACH_CANCELLED` /
`U_CONFINE_OUTCOME_UNKNOWN`.

### `internal/runner/confine_detach.go` (portable)
The pure half, so every liveness/ambiguity/terminal/unreadable branch is
unit-testable with no cgroups and no processes:
`ResolveConfineDetachStatus(records []ConfineDetachRecord, selector, callerOwner
string, alive func(PIDIdentity) (bool, bool))`, the state vocabulary, and the
human/JSON projections.

### `internal/runner/confine_detach_linux.go`
The privileged half: the fd-anchored record store (`O_NOFOLLOW`, `0700`/`0600`,
`Mkdir` not `MkdirAll`, `Renameat`-atomic writes), `LaunchConfineDetached`,
`SuperviseConfineDetached`, and the real `confineSupervisorAlive` (PID identity
via `processStartTick` + `currentBootID`).

### `internal/runner/confine_linux.go`
Honour the private pre-minted scope id with the full `bindConfineScopeID` parity
check (§3.2); invoke `BeforeAdmit` immediately before `deps.admit` and `OnPlaced`
at the proven-placement point (§3.6); `normalizeConfineIdentity` replaces the
inline name/owner defaulting. Nothing else changes — the signal handler, the
trailer, admission, and the cap logic are untouched.

### `internal/runner/confine_stub.go`
Non-Linux stubs for the two new exported Linux entry points.

### `internal/daemon/paths.go`
`Paths.ConfineStateDir = filepath.Join(stateDir, "confine")`.

### `cmd/aira/main.go`
`__confine-supervise` hidden verb next to `__supervise`; `detach` accepted in
`parseConfineArgs`; `status` accepted (and `detach` rejected with a targeted
message) in `parseConfineManagementArgs`; `runConfineCommand` branches to a new
`runConfineDetachCommand`; new `runConfineStatusCommand` with human and `--json`
rendering; `var launchConfineDetached = runner.LaunchConfineDetached` for
injection, mirroring `runConfined`.

### `internal/core/core.go`
`confine` spec usage gains `[--detach]` and a `detach` bool arg spec. `--status`
gets its **own** `confine-status` spec mirroring `confine-list`/`confine-kill`
(Usage `confine --status [<selector>] [--json]`, `Include:false` because it is
CLI-only, no `MCPTool`) rather than being an argument on a spec whose `argv` is
required — otherwise the generated usage line is misleading. `Include` is
computed at `core.go:2101` and must exclude it; `internal/core/routing.go:47,111`
list it alongside the other CLI-only confine verbs;
`dispatch_metadata_test.go:431`'s verb list gains it.

### `internal/core/skill.go`
Replace the now-false "no native `--detach` of its own yet ... backgrounding via
the calling harness is the current workaround" sentence with the real verb, its
`--status` follow-up, the "exit 0 means the supervisor started" caveat, and the
`/dev/null` stdin note.

### `internal/store/check.go`
`"E_CONFINE_DETACH_FAILED": 4, "U_CONFINE_DETACH_CANCELLED": 3,
"U_CONFINE_OUTCOME_UNKNOWN": 3` — mirroring `E_RUN_DETACH_FAILED: 4` /
`U_RUN_DETACH_CANCELLED: 3`.

### Tests

Five tests in the first draft were named porous by review and are replaced below,
not merely renamed.

Portable / no privilege (`confine_detach_test.go`, `main_test.go`, `confine_test.go`):

1. `TestConfineDetachRecordWriteIsAtomicAcrossAFailureBetweenWriteAndRename` —
   a seam fails after the tmp write and before the rename; the reader still sees
   the *previous complete* record, and a leftover `*.tmp` is ignored. (Replaces a
   probabilistic "reader sees only complete records" race.)
2. `TestConfineDetachRecordFilesAreOwnerOnlyAndNotSymlinkFollowing` — modes
   `0700`/`0600`; a pre-planted symlink at `record.json`/`stdout` is refused, not
   written through; a pre-existing record dir is refused (`Mkdir`, not `MkdirAll`).
3. `TestResolveConfineDetachStatusNeverFabricatesAnOutcome` — table over
   {terminal, alive+phase=running, alive+phase=admitting, dead, pid-reused,
   other-boot, liveness-unevaluated, record-unreadable} ⇒ {finished, running,
   admitting, outcome-unknown×5}.
4. `TestResolveConfineDetachStatusRefusesAnAmbiguousSelectorAndNamesTheCandidates`
   — including that owner scoping removes cross-owner ambiguity, and that the
   message carries each candidate's `started_at` and state.
5. `TestResolveConfineDetachStatusMatchesScopeIDNameAndSupervisorPID`.
6. `TestConfineDetachStoreRejectsAnInvalidScopeID` (`../`, `/`, empty,
   non-canonical stamp, uppercase base36).
7. `TestBindConfineScopeIDRefusesAForeignPIDNameOwnerOrDelegateClass` — the four
   spoof directions individually, each asserting the *launch is refused*, not just
   that a helper returns an error. (Replaces the syntax-only test and the
   mint/defaulting parity test, which a shared normaliser makes unnecessary.)
8. `TestConfineWithDepsRefusesAPreSetScopeIDMintedByAnotherProcess` — the same
   check exercised through `confineWithDeps`, proving no child is started.
9. `TestControlValueRoundTripsAConfineRequestAndPreservesTheRunControlContract` —
   confine request round-trip **plus** explicit assertions that the generalised
   helper still rejects unknown fields and trailing data, removes the file on
   read, and creates it `0600`.
10. CLI arg tests: `--detach` needs `--`; `--detach` alone gets the targeted
    message; `--status` is exclusive with `--list`/`--kill`; `--steal` is
    `--kill`-only; `--json` allowed with `--status`, still refused for a launch;
    `--status` with no selector is a listing, not an error.
11. `TestConfineDetachLaunchOutputSaysTheExitCodeIsNotKnownYet` — pins §3.6's
    wording *and* that the launcher prints the **resolved** slice (not the
    requested one) and never prints an exit code.
12. `TestConfineDetachPropagatesASynchronousLaunchFailureExitCode` — the
    supervisor reports `E_CONFINE_UNAVAILABLE` through the ready channel and the
    launcher exits 4, identical to the foreground form. (This is the P1-1
    regression the first draft would have introduced.)
13. `TestConfineStatusExitCodeReportsTheQueryNotTheJob` — a `finished` job with
    exit 7 still exits 0; `outcome-unknown` exits 3; not-found exits 2.
14. `TestConfineRequestHasNoDetachFieldReachingTheAdmitRequest` — pins that
    `admitConfine` never sets `Request.Detach` (the nil-ledger panic trap).

Linux, no cgroups (`confine_detach_linux_test.go`, via the package `TestMain`
helper modes):

15. `TestDetachedSupervisorSurvivesAGroupKillOfItsLauncher` — the launcher runs
    as a subprocess with `Setpgid`; the test sends `SIGHUP`, `SIGINT` and finally
    `SIGKILL` to the **whole launcher process group**; the supervisor must still
    be alive and its job must still be producing output afterwards. (Replaces the
    porous "SIGKILL the launcher pid" test, which any reparented child survives
    with or without `Setsid`.)
16. `TestLaunchConfineDetachedReportsAShimThatNeverReportsReadyAndTheShimStops` ⇒
    `E_CONFINE_DETACH_FAILED` **and** the supervisor observes the missing ack and
    exits without launching, leaving a terminal `U_CONFINE_DETACH_CANCELLED`
    record — no ghost job behind a reported failure.
17. `TestSuperviseConfineDetachedWritesATerminalRecordOnEveryReturnPath` —
    ordinary error, panic on the main goroutine, and a `Confine` error before the
    launch gate; `Exit` absent whenever `Confine` returned an error.
18. `TestConfineSupervisorAliveDistinguishesLiveReusedAndUnreadablePIDs` — a real
    probe against self, against a reaped child pid, and against an unreadable
    `/proc` entry. (The pure resolver test above uses an injected prober; this one
    pins the prober itself.)

Linux, real cgroups (`cgrouptest.SkipOrFailRealCgroup`):

19. `TestConfineDetachEndToEndSurvivesTheLauncherAndCapturesTheOutcome` — detached
    `/bin/sh -c 'sleep 1; echo out; echo err >&2; exit 7'`, launched from a
    subprocess launcher that is group-killed the moment it returns; poll to
    terminal; assert `Exit==7`, `stdout` contains `out`, `stderr` contains `err`
    **and** the `terminated-by=normal` trailer, and `--status` renders `finished`.
20. `TestConfineListAndKillTargetTheDetachedSupervisorPID` — `confine --list`
    reports `SupervisorPID` equal to the live `__confine-supervise` pid (the §3.2
    invariant, previously untested), `confine --kill <that pid>` succeeds, and the
    record goes terminal with `terminated-by=unattributed-sigkill`.

## 5. False-pass directions the build review must probe

1. A detached launch that reports success while the job never started (shim
   exec failure, control file unreadable, state dir unwritable).
2. `--status` reporting `running` for a supervisor that is gone — especially the
   PID-reuse and post-reboot cases, and the "cannot read `/proc`" case.
3. `--status` reporting `finished`/`exit 0` from a *non-terminal* record.
4. The launcher's exit 0 being read as job success (wording test, §4.10).
5. A pre-minted scope id being silently re-minted, decoupling record from scope.
6. A record written non-atomically and read half-formed.
7. Path traversal through the selector or scope id into the record store.
8. The `detach_control.go` generalisation silently changing the run path's
   control-file contract (`DisallowUnknownFields`, trailing-data rejection,
   removal-on-read, 0600).
9. The supervisor exiting without writing a terminal record on any path
   (including the `Confine` error paths and panics).
10. Capture files inheriting the launching terminal instead of the store.

## 6. Questions put to review, and the answers received (round 1)

1. **No ack** (§3.1) — **rejected by all three lineages.** The ack is kept; §3.1
   and §3.6 rewritten.
2. **No daemon change** (§3.5) — **upheld** (Fable enumerated the nine daemon
   assumptions and found none broken), with §3.5 reframed to name the
   *pre-existing* orphaned-supervisor gap it inherits rather than implying there
   is none.
3. **`--status` CLI-direct** (§3.4) — **upheld** for the stated reason.
4. **Status exit code reports the query** (§3.4) — **upheld**; it matches
   `get`/`confine-list` and the `U_`→3 / `E_`→2/4 catalogue.

## 7. Deferrals (named, not silent)

- **Record-store retention and capture size.** Nothing prunes
  `<state>/aira/confine/`, and captured output is uncapped. This matches
  `run --detach`'s output dir, so it adds no new *class* of growth — but the
  honest consequence is stated rather than glossed: if the disk fills, the
  terminal-record write itself can fail, and `--status` will then report
  `outcome-unknown` for a job that in fact ran. `supervisor.log` records the
  ENOSPC when it can. A follow-up ticket for retention + an optional capture cap
  is recommended in the PR body.
- **Full logout survival** needs `loginctl enable-linger`; `aira install` does not
  set it and this milestone does not change that (§3.7).
- **MCP exposure of `--status`.** CLI-only, like `confine` itself; agents reach it
  through the skill's `Bash(aira *)` allowance.
- **Live output following.** Print the paths; `tail -f` is the primitive.
- **`--detach` for `confine-reserve` / worker-admit.** Not asked for, not useful.
- **A daemon-side persisted lease for an orphaned supervisor** (§3.5) — a
  pre-existing gap shared with the foreground form, out of scope here.

## 8. Expected yield

One P2 survivability feature closed; the "`aira confine` has no native `--detach`"
caveat removed from the agent guide; and the first durable, session-independent
answer to "what happened to my confined job", which is a precondition for running
hour-long gates from an agent session at all.

## 9. Deviation record

Deviations from this plan taken during the build are appended here.

## 10. Plan-review round 1 — findings and resolutions

Three orthogonal lineages reviewed plan v1 against the real source. **All three
returned a blocking verdict**: Codex/Sol `BLOCK` (6×P1), Fable `GATE-FAIL`
(5×P1, 10×P2), DeepSeek `BLOCK` (2×P0, 3×P1, 2×P2). Every finding and its
disposition:

| # | Finding | Raised by | Resolution |
|---|---|---|---|
| 1 | Ready sent before validation ⇒ every synchronous launch failure becomes `exit 0, poll later`, weakening the exit contract the foreground form already has | Fable P1-1, DeepSeek P0-1 | **Accepted.** `BeforeAdmit` gate; ready now means "every synchronous precondition passed". §3.6, tests 12. |
| 2 | Dropping the ack lets the launcher report `E_CONFINE_DETACH_FAILED` while a ghost job runs | Sol P1-2, Fable P1-2 | **Accepted.** Ack kept; unacked supervisor cancels before admission. §3.1, §3.6, test 16. |
| 3 | A caller-supplied scope id is only syntax-checked ⇒ forged PID/owner/delegate class, unaccounted launch via the flock fallback | Sol P1-1, Fable P1-3 | **Accepted.** Field made package-private **and** fully bound by `bindConfineScopeID`. §3.2, tests 7–8. |
| 4 | `running` over-claims for a job still queued in admission (up to 24h) | Sol P1-4, Fable P1-4, DeepSeek | **Accepted.** `admitting` is a distinct observed state, written by the two callbacks. §3.4. |
| 5 | Record store under-specified: symlinks, permissions, predictable tmp name, concurrent writers | Sol P1-5, Fable | **Accepted.** `0700`/`0600`, `Mkdir` not `MkdirAll`, fd-anchored `O_NOFOLLOW`, `Renameat`. §3.3, test 2. |
| 6 | "Owner-keyed" not delivered; name reuse permanently ambiguous | Sol P1-6, Fable P2-5 | **Accepted.** Owner-scoped matching, candidate-listing ambiguity message, no-selector listing. §3.4, tests 4, 10. |
| 7 | `ConfineRequest.Detach` would be transcribed into `Request.Detach` ⇒ nil-ledger panic in `checkDetachAdmission` | Fable P2-3 | **Accepted.** The field is not added at all. §4, test 14. |
| 8 | Supervisor fd 2 is `/dev/null` ⇒ runtime-fatal crashes vanish and the record stays non-terminal with no cause | Fable P2-4 | **Accepted.** `supervisor.log` + `dup2`; terminal-record guarantee restated with its true scope. §3.3. |
| 9 | An explicit `kill -HUP` default-terminates the supervisor with no teardown | Fable P2-10 | **Accepted.** `signal.Ignore(SIGHUP)` in the detached supervisor only. §3.7. |
| 10 | "Session-independent" is bounded — no `enable-linger` | Fable P2-2 | **Accepted** as a documented gap. §3.7, §7. |
| 11 | Generated help: `--status` as an arg on a spec whose `argv` is required is misleading | Fable P2-6 | **Accepted.** Separate `confine-status` spec. §4. |
| 12 | Unreadable/corrupt record must be `outcome-unknown`, not `NOT_FOUND` | Fable P2-7 | **Accepted.** §3.4, test 3. |
| 13 | Two copies of name/owner defaulting held together only by a test | Fable P2-8 | **Accepted.** One `normalizeConfineIdentity`; the parity test is deleted rather than kept. §3.2. |
| 14 | `Exit` nil rule imprecise (`abortStarted` returns an error after the child started) | Fable P2-1 | **Accepted.** Rule is "absent whenever `Confine` returned an error". §3.6, test 17. |
| 15 | Record must carry argv/cwd/env/timestamps to be useful to a resumed session | DeepSeek P1 | **Accepted**, with env stored as a **digest** — never verbatim, because the store is durable and environments carry credentials. §3.3. |
| 16 | Porous tests: survival test (12), atomicity (1), mint parity (6), id syntax (7), late-shim fate (13), terminal-record coverage (14), real PID probing (2), real end-to-end launcher death (15) | Sol P2, Fable P1-5 | **Accepted.** All replaced; §4's list is renumbered 1–20 with the replacements marked. |
| 17 | Unbounded capture ⇒ ENOSPC ⇒ terminal-record write fails ⇒ `outcome-unknown` for a job that ran | DeepSeek P1 | **Deferred, named.** Consistent with `run --detach`; consequence stated in §7 rather than solved with new machinery. |
| 18 | Daemon restart breaks the socket lease while `--status` still says `running` | DeepSeek P0-2 | **Not a defect of this change.** `--status` speaks about the job, not the reservation; AIRA-74 re-adopts the live scope within ≤1s. Reasoning recorded in §3.5. |
| 19 | `--kill` against a detached job unspecified | DeepSeek P1 | Already §3.7; now **tested** (test 20). |
| 20 | Ready read has no timeout / fds not CLOEXEC | DeepSeek P2 | Timeout was already specified (10s). CLOEXEC on the inherited ready/ack fds added, mirroring `RunConfineSetup`. |

