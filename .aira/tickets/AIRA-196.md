---
{"schema":1,"id":"AIRA-196","project":"aira","title":"aira confine --detach has no confine-log/confine-input companions -- token-efficient read/write access to a detached confine job, by handle, with confine's own admission/delegate-ram/aitest semantics intact","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","detach","observability"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-197","to":"AIRA-196"}]}
---

Owner request (2026-09-09), following directly from tonight's harness-
interference discussion (`aira confine --detach` as the pattern to protect
a long-running job from the Claude Code harness's own background-job
guard, which several peer sessions hit tonight as `terminated-by=
supervisor-signal`). Owner: "Could we have an option for the aira owning
process to capture (stdout+stderr) and make it accessible via aira
subcommands for common access patterns (--tail[=n] --head[=m] --follow
--grep=\"...\"), plus the ability to inject input and to close stdin? ...
just nice helper commands for reading/streaming the job's file in a
token-efficient way, without needing to use the filename directly (just
the job's id)." Then, on learning `aira run --detach` already has exactly
this (`run-log`/`run-input`) while `aira confine --detach` does not:
**"we do also want confine's admission / limiting / aitest semantics
too"** -- i.e. the fix is NOT "use `aira run` instead", it is bringing
`aira confine --detach` up to the same read/write ergonomics `aira run
--detach` already has, since `--delegate-ram`/`aitest` integration is
confine-specific (`internal/core/core.go:1718`'s dispatch entry for
`confine` carries `--delegate-ram`; `run`'s does not).

## What already exists, verified from source

- **`aira run-log <run-id> [--stream out|err|merged] [--follow] [--from
  N] [--tail N] [--full]`** (`internal/core/core.go:1883-1891`,
  `MCPTool: "aira_run_output"`) -- tail, byte-offset, follow, stream
  selection, all by run-id. No `--grep` support (a real, if small, gap
  even here).
- **`aira run-input <run-id> [--close] [--steal]`** (`core.go:1678`,
  `MCPTool` implied by naming convention) -- inject stdin bytes, close
  stdin, ownership-guarded with `--steal` override.
- **Both already work for a job launched via `aira run --detach`**, which
  shares the exact same `spawnDetachedShim` (Setsid, session-independent,
  harness-immune) as `aira confine --detach`
  (`internal/runner/detach_linux.go:46,49`, comment: "spawnDetachedShim
  is shared with `confine --detach` (AIRA-22)").
- **`aira confine --detach` already captures stdout/stderr durably**
  (`StdoutPath`/`StderrPath` in the status record,
  `internal/runner/confine_detach_linux.go:179-180,250-251`) -- the DATA
  exists, only the read/write CLI surface over it is missing.
- **`aira confine --detach`'s wrapped child stdin is unconditionally
  `devnull`** (`confine_detach_linux.go:665`:
  `request.Stdin, request.Stdout, request.Stderr = devnull, job.stdout,
  job.stderr`) -- there is no existing conduit to inject input into a
  detached confine job's stdin at all, unlike `run --detach`'s
  `StdinConnect` (a per-run socket, `internal/runner/types.go:180,238`,
  `run_input_protocol.go`).

## Scope

1. **`confine-log <name|scope-id|supervisor-pid> [--stream out|err]
   [--follow] [--from N] [--tail N] [--full] [--grep PATTERN] [--owner
   ID] [--json]`** -- mirror `run-log`'s design and, where reasonable,
   its underlying implementation (the byte-offset/tail/follow reading
   logic should be shared, not reimplemented, if it is not already
   generic over "a durable captured-output file" vs. "a run record"
   specifically -- investigate before assuming). New: `--grep PATTERN`,
   which `run-log` also lacks -- add it to BOTH verbs for consistency
   rather than only the new one, since the owner asked for it explicitly
   and it is a real gap in the existing verb too. Selector matches
   `confine --status`'s existing selector shape (name, supervisor PID, or
   scope ID) for consistency, not a new identifier scheme.
2. **`confine-input <name|scope-id|supervisor-pid> [--close] [--owner
   ID] [--steal]`** -- requires adding a stdin conduit to `aira confine
   --detach`'s launch path (a per-job socket or named pipe, matching
   `run --detach`'s `StdinConnect` precedent rather than inventing a
   different mechanism) that defaults OFF (stdin stays `devnull` unless
   the launcher opts in, e.g. a new `--stdin-connect` flag on `confine
   --detach` itself, mirroring `run`'s own flag) -- a live, unused stdin
   pipe must never become a new hang class for a job that never expected
   input.
3. Read/write access must be addressable purely by the confine job's own
   handle (name/scope-id/supervisor-pid) -- never require the caller to
   know or construct the underlying file path, matching the owner's own
   stated goal ("without needing to use the filename directly").
4. MCP/Skill integration via the standard dispatch-table pattern
   (confirmed reusable all night), matching `run-log`'s/`run-input`'s own
   `MCPTool` registration shape.

## Not designed here

Whether `confine-input`'s new stdin conduit should be a raw AF_UNIX
socket (matching `run`'s own `StdinConnect` precedent exactly) or a named
pipe, and the exact ownership/ratchet semantics if the daemon or
supervisor restarts mid-connection, are left for whoever builds this --
`run`'s own `StdinConnect` implementation is the concrete precedent to
follow, not to reinvent from scratch.

## Done — PR #123 merged (8bb2759), 2026-09-09

Built on `aira196-confine-log-input`; the build agent was interrupted by a
session usage limit mid-rebase, so this record covers a from-scratch
completeness review of what was on the branch, not a sign-off on a build
already known finished.

**State found.** Two commits, complete and internally coherent: the feature
plus a self-review fix. The branch had been rebased onto `0548c19` with
conflicts resolved but never pushed, so `origin` still carried the
pre-rebase pair. The rebase resolution had also left commit 1's Skill
action-count assertions at the wrong value (74, corrected to 76 in commit
2) — real, but self-cancelling across the pair. Rebased onto `c402ef3`
(nine more master commits, ten conflicting files: `main.go`,
`mcp_project.go`, `mcp_test.go`, `scope_dir.go`, `scope_dir_test.go`,
`routing.go`, `skill.go`, `dispatch_metadata_test.go`, both `skill_test.go`),
resolved as unions with `confine-budget`/`worker-peak`/`worktree-*` from
master; action count settled at 77 (75 + this ticket's two).

**Verdict: MERGE.** Every scope item is built, and built the way the ticket
asked rather than around it.

### Scope, checked item by item

1. `confine-log <selector> [--stream out|err] [--follow] [--from N]
   [--tail N] [--full] [--grep PATTERN] [--owner ID] [--json]` — all
   present. The reading logic is genuinely SHARED, not copied:
   `Runner.ReadOutput`'s byte window was factored out into
   `internal/runner/captured_output.go` (`readCapturedFile`), which takes a
   path plus offset/tail/cap and returns a fault class each verb maps onto
   its own vocabulary. `ReadOutput` keeps its run-record concerns and calls
   it. That is the ticket's "investigate before assuming" answered by
   refactoring rather than duplicating.
2. `confine-input <selector> [--close] [--owner ID] [--steal]` over a new
   stdin conduit behind `--stdin-connect`, default OFF, `--detach`-only.
   `run-input`'s conduit is reused whole — same `runInputPlane`, same
   framed protocol, same `E_RUN_INPUT_*` codes; `Runner.Input` split into
   `dialRunInput` + `streamRunInput`, both called by `ConfineInput`. No
   parallel code vocabulary invented, which was the right call: the server
   a confine job runs IS `runInputPlane`.
3. Handle-only addressing — `ConfineDetachStatusFor` (the same selector
   `confine --status` takes). No path is ever required or accepted.
4. MCP + Skill via the dispatch table: `aira_confine_log` /
   `aira_confine_input`, `SafetyRead` / `SafetyExecute`, project-less on
   both faces, and the generated Skill prose rewritten to teach the new
   verbs and the `/dev/null`-by-default rule.

`--grep` was added to `run-log` too, as the ticket required.

### Gate (this machine, `aira confine`)

| gate | exit |
| --- | --- |
| `go build ./...` | 0 |
| `go vet ./...` | 0 |
| `gofmt -l cmd internal` | 0, no files |
| targeted `internal/runner` (18 tests, incl. real-cgroup) | 0 |
| targeted `cmd/aira` | 0 |
| full `go test ./... -count=1` | 0, all 17 packages ok |
| CI `build + vet + gofmt` / `test` / `race` | all SUCCESS |

### False-pass direction: five mutations, all killed

- `if request.StdinConnect` → unconditional: `TestDetachedConfineStdinIsDevnullWithoutStdinConnect`
  FAILS on its deadline ("the supervisor never exited") — the job is `cat`,
  so an always-wired pipe reproduces the exact production hang the default
  exists to prevent. The load-bearing invariant is genuinely pinned.
- `--grep` made a no-op in the shared reader: 3 tests fail.
- `stdinCarriesJobInput` gate removed: `TestConfineInputNeverForwardsTheMCPProtocolStream`
  fails — the JSON-RPC transport would have been streamed into the job.
- `confineLogCaptureComplete` collapsed into `confineLogFollowDone`: 2 tests
  fail; an outcome-unknown job would have been reported `complete`.
- **`ReadOutput` silently dropping `Grep`: SUITE STAYED GREEN.** A real
  porosity found in review — `cmd/aira`'s `TestRunLogAcceptsGrep` proved
  only that the flag parses and is declared, nothing that `run-log`'s own
  read path applies it. Fixed in-review (`ccc19cd`): `TestReadOutputAppliesGrepAndReportsIt`
  drives the real `ReadOutput` and pins application, `Filtered`/`Grep`
  reporting, cursor-describes-the-file, and `E_RUN_ARGUMENT_INVALID` on an
  unparseable pattern. Re-mutated after: the new test fails. Gap closed.

### Live verification (not just tests)

Built the branch binary and drove the real CLI against `aira.slice`:

- **Default OFF, proven directly.** A detached job launched with no flag
  printed `/proc/self/fd/0 -> /dev/null`, `cat` saw EOF and exited 0, the
  durable record carries no `stdin_connect`/`input_socket`, and
  `confine-input` refused it `E_RUN_INPUT_UNAVAILABLE` naming
  `--stdin-connect` (exit 1) instead of dialling or waiting.
- **Opt-in works.** With `--stdin-connect`: `/proc/self/fd/0 -> pipe:[…]`,
  `confine-input <name>` delivered bytes the job echoed, `--close` EOF'd it
  and the job exited 0, and the socket did not outlive its supervisor.
- `--stdin-connect` without `--detach` refused at parse time, exit 2.
- `--follow` returned when the job finished (3s, not a spin); `--grep`
  filtered with `filtered:true` and `next_offset`/`total_bytes` still
  describing the whole file; `--json`, `--tail`, `--stream err` all correct.

### Decisions inherited from the plan, reviewed and accepted

- **`--stream` defaults to `out`.** A confine job has no merged capture, so
  `run-log`'s refuse-when-ambiguous would refuse every bare invocation.
  Defensible because every reply NAMES the stream it read; the Skill says
  stderr is a separate file.
- **`--full` waives the face cap on `confine-log` only.** `run-log`'s
  identically named field is inert today, and `core/run_wiring.go` and
  `store/gate_command.go` pass `Full: true` alongside a cap they depend on,
  so giving it that meaning there would silently remove the run-report size
  bound. Recorded rather than propagated — correct.
- **`run-input` keeps the same MCP-stdin shape `confine-input` was fixed
  for.** Out of scope; recorded in `9a4d519` rather than changed silently.
  Worth its own ticket if MCP `run-input` is ever used without `data`.

### Accepted gaps (non-blocking, written down)

- A bad option on `confine-log`/`confine-input` reports `E_SELECTOR_INVALID`,
  not `E_CONFINE_ARGUMENT_INVALID`. Verified pre-existing and shared with
  `confine-list`/`confine-kill` (measured on the shipped binary), so it is
  family-consistent, not a regression this ticket introduced.
- Between the record being written `running` and `inputPlane.serve()`
  starting the accept loop there is a sub-millisecond window in which a
  `confine-input` connect is queued in the listener backlog rather than
  accepted. It cannot hang or fail: the listener exists from plane creation,
  and if the supervisor exits first `closeTerminal` closes it, so the client
  gets an error, never a wait.
- Commit `98ddf55` alone (mid-PR) carries the wrong Skill action count, from
  the interrupted rebase. The merged tree is correct; `git bisect` across
  that one commit would not build green.
- The `race` CI job failed once on an UNRELATED pre-existing flake
  (`TestM20bSupervisorRejectsMalformedSidecarBeforeOpeningProject`, EOF). It
  reproduces on pristine master (3 failures / 400 under `-race`) and is a
  descriptor-ownership bug in that test, not a data race and not this
  ticket's doing. Diagnosed, measured and filed with a proven fix as
  **AIRA-199**; the job was re-run and passed cleanly.
