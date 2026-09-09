# AIRA-196 — `confine-log` / `confine-input`: `run --detach`'s read/write ergonomics for `confine --detach`

Status: plan (implemented on `aira196-confine-log-input`).

## Problem

`aira run --detach` has `run-log` (tail/offset/follow, by run-id) and
`run-input` (inject stdin, close stdin, ownership-guarded). `aira confine
--detach` — which shares the same `spawnDetachedShim` and already captures
stdout/stderr durably — has neither. The owner needs `confine`'s admission /
`--delegate-ram` / aitest semantics AND those ergonomics, so "use `aira run`
instead" is not the fix.

## Scope

1. `aira confine-log <name|scope-id|supervisor-pid> [--stream out|err]
   [--follow] [--from N] [--tail N] [--full] [--grep PATTERN] [--owner ID]
   [--json]`.
2. `aira confine-input <name|scope-id|supervisor-pid> [--close] [--owner ID]
   [--steal]`, over a stdin conduit added to `confine --detach` behind a new,
   **default-off** `--stdin-connect` flag.
3. `--grep PATTERN` added to **both** `confine-log` and the existing `run-log`.
4. Both verbs addressed only by the job's own handle; MCP + Skill via the
   dispatch table.

## Invariants

- **I1 (stdin default-off).** A detached confine job's stdin is `/dev/null`
  unless the launcher passed `--stdin-connect`. No socket is created, no pipe is
  opened, and the durable record's `stdin_connect` stays false. A live but
  unused stdin pipe would be a new hang class for every job that never expected
  input.
- **I2 (`--stdin-connect` requires `--detach`).** A foreground `confine` already
  has a real stdin; accepting the flag there and ignoring it would be a silently
  discarded request. Refused at parse time.
- **I3 (read side is daemon-free).** `confine-log` reads the durable filesystem
  record and the captured file, with no daemon round-trip — the same property
  `confine --status` is built on (AIRA-22 survivability: the verb that exists to
  survive a long pause must not depend on the component most likely to have been
  restarted during it). `confine-input` necessarily needs the **supervisor**
  alive (it dials that process's socket); it still needs no daemon.
- **I4 (no fabricated output).** An unreadable or absent capture file is
  `U_CONFINE_LOG_UNAVAILABLE`, never an empty successful read. The chunk always
  reports the job's own resolved state alongside the bytes.
- **I5 (grep never lies about the cursor).** `--grep` filters the LINES of the
  window that was read; `offset`/`next_offset`/`total_bytes` keep describing the
  underlying file so `--from`/`--follow` paging still works, and the chunk
  carries `filtered: true` plus the pattern so a caller can never mistake a
  filtered body for the whole window.
- **I6 (ownership).** Selector resolution reuses `ResolveConfineDetachStatus`
  verbatim: a name/pid selector is scoped to the caller's owner, a scope id is
  globally addressable, ambiguity is refused with the candidates listed.
  `confine-input` additionally passes the caller owner in the wire HELLO, so a
  foreign job refuses unless `--steal`.

## Two decisions the ticket left open

**`--stream` defaults to `out`.** A confine job captures out and err as separate
files and has no merged stream, so `run-log`'s "refuse when the stream is
ambiguous" would refuse every bare `aira confine-log <job>`. The defence against
"I read stdout and the failure was on stderr" is that every reply NAMES the
stream it read (and its state, `truncated` and `filtered`), plus prose in the
Skill saying `err` is a separate file — not a refusal to answer.

**`--full` waives the face's observation cap, on `confine-log` only.**
`run-log`'s identically named field is currently inert — `Runner.ReadOutput`
never reads `OutputRequest.Full`. It is NOT given this meaning there, because
`internal/core/run_wiring.go` and `internal/store/gate_command.go` both pass
`Full: true` alongside a real cap they depend on; making `Full` waive the cap
would silently remove the run-report size bound. So: the new verb's flag is
true, the old verb's inertness is recorded here rather than propagated or
silently changed. Shipping a third flag that does nothing was the option
rejected.

## Reuse decisions

- `Runner.ReadOutput`'s byte window (open / stat / tail-vs-offset / seek /
  bounded read / truncation) is **factored out** into `readCapturedFile`
  (`internal/runner/captured_output.go`), which takes a plain path plus
  offset/tail/cap and returns a fault class each verb maps onto its own error
  vocabulary. `ReadOutput` keeps its run-record concerns (run id, stream
  selection from `OutputRefs`, output state, follow-until-terminal, run
  telemetry) and calls the shared reader. `confine-log` does the same with its
  own record. Nothing is duplicated.
- `run-input`'s conduit is **reused as-is**: the same `runInputPlane` server, the
  same framed protocol, the same `E_RUN_INPUT_*` code vocabulary. `Runner.Input`
  is split into `dialRunInput` + `streamRunInput`, and `ConfineInput` calls the
  same two. A parallel confine-only code vocabulary was rejected: the wire
  server emits `E_RUN_INPUT_*` codes, and translating them at the boundary would
  be two names for one fact plus a real mistranslation risk.

## Tests (the load-bearing ones)

- `--stdin-connect` off ⇒ record `stdin_connect=false`, `input_socket=""`, no
  socket on disk, and the child's stdin reads EOF immediately (`/dev/null`),
  proven end-to-end through the real supervisor.
- `--stdin-connect` on ⇒ socket exists, `confine-input` delivers bytes the child
  actually reads back, `--close` EOFs it.
- `confine-input` against a job that did NOT opt in ⇒ `E_RUN_INPUT_UNAVAILABLE`,
  never a hang.
- `--grep` filters a multi-line capture, keeps the cursor on the underlying
  file, and reports `filtered`.
- `confine-log` reads identically while the job runs and after it finished.
- Ownership: a foreign name selector is refused; the scope id is not.
- `--stdin-connect` without `--detach` is refused at parse time.
