---
{"schema":1,"id":"AIRA-196","project":"aira","title":"aira confine --detach has no confine-log/confine-input companions -- token-efficient read/write access to a detached confine job, by handle, with confine's own admission/delegate-ram/aitest semantics intact","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","detach","observability"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-197","to":"AIRA-196"}]}
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
