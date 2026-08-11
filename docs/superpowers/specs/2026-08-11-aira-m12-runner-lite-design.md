# AIRA M12 — runner-lite design

- **Status:** plan (design only; no implementation in M12 spec)
- **Date:** 2026-08-11
- **Branch:** `codex-aira-m12` off `master`
- **Prerequisites:** Phase-1 store/reconcile and stable response contracts; the
  M8b descriptor-generated CLI/MCP/Skill surfaces; the Phase-3 execution seam.
- **Companion:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md),
  especially §5.2, §14, and §20.

This is the milestone design for AIRA's small, foreground-only subprocess
runner. It specifies the boundary, launch and capture protocol, cgroup safety
boundary, run record, durable recovery story, faces, stable outcomes, and TDD
matrix. It does not implement Go code.

## 0.1 Accepted deviations, recorded on the record

These are deliberate M12 decisions and boundaries, not missing work:

- M12 is **daemonless**. A foreground caller owns the child, drains its pipes,
  waits for it, and writes the terminal record. The daemon is **only** the
  Phase-5 detached-run supervisor. M12 has no detach mode and does not create
  a supervisor merely to make foreground launch work.
- M12 captures to files and makes those files the live observation source.
  It does not implement Phase-5 tee-to-a-live-reader-with-supervisor. A
  `run-log --follow` reader can observe the files while the foreground caller
  is still running, but the child is never allowed to depend on that reader.
- M12 includes `--merge` because faithful cross-stream ordering is a capture
  contract, not a presentation filter. It uses an exec-time `dup2` topology;
  it is not a post-hoc concatenation of two files.
- M12 does not wire runs automatically to telemetry, test reports, gates, or
  `tests-green`. A future consumer may use a M12 run only when its terminal
  status and exit evidence satisfy the honesty rules below.
- M12 has no live stdin push. `--stdin` is a launch-time source only;
  `run-input` is deferred until the Phase-5 supervisor/control-plane work.
- The recommended durability boundary is an append-only common-dir run
  ledger with a SQLite projection. This intentionally elevates the small
  lifecycle record above ordinary retention-capped telemetry so DB loss and
  an interrupted foreground parent cannot manufacture a green result. The
  architect must still confirm this against the whole-product “Run metadata is
  DB-only telemetry” default; the conflict is listed explicitly in §12.

Any implementation that changes one of these boundaries must add an equivalent
accepted-deviation section to its implementation plan and name the affected
invariant and test.

## 1. Scope

M12 provides exactly four connected capabilities:

1. **Launch** one foreground subprocess using a configurable launch prefix,
   faithful cwd and environment, and no interactive approval prompt.
2. **Capture** raw stdout and stderr to per-run files while always draining the
   child pipes, with an opt-in kernel-faithful merged stream.
3. **Kill safely** through an AIRA-owned cgroup-v2 scope containing the launch
   prefix and its descendants.
4. **Inspect** the captured bytes with `run-log`, including bounded follow
   semantics that never silently truncate output.

The M12 success condition is stronger than “a command ran”: an agent can
identify the exact argv/cwd/environment identity, locate the raw output, kill
the whole owned scope, and distinguish an established exit 0 from a killed,
lost, or capture-corrupted result.

### 1.1 Explicit non-goals / deferrals

- No `--detach`, background ownership, or daemon requirement.
- No `run-input` or any live stdin push after launch.
- No supervisor shim for detached execution. The Phase-5 daemon/shim choice is
  not pulled into M12; the daemon remains exclusively the detached-run
  supervisor by owner decision.
- No live tee to a terminal, agent stream, socket, or MCP subscription. File
  capture plus `run-log --follow` is the M12 observation model.
- No `--realtime`, `--pty`, or buffering-control modes.
- No rusage-at-exit, peak RSS, OOM classification, or cgroup telemetry.
- No automatic ComputeEvent, test-report, gate, ratchet, or `tests-green`
  wiring. A run record may be referenced later, but M12 does not create those
  records or verdicts.
- No shell interpretation of the target argv or launch prefix: no pipes,
  redirects, glob expansion, command substitution, or implicit shell.
- No interactive approval or confirmation prompts. If policy or permissions
  prevent a launch, AIRA returns a stable failure before launching.
- No promise that capture files survive machine loss or arbitrary retention;
  they are machine-local blobs, not git content and not SQLite blobs.
- No claim that a process which escaped an explicitly approved future
  unscoped fallback can be killed safely. M12's default is fail-closed.

## 2. Terms, identities, and closed states

### 2.1 Run identity

A **run** is one AIRA launch attempt and its process tree, not merely a PID.
The identity binds:

- an immutable run ID;
- the exact target argv and effective launch-prefix argv;
- the effective cwd and environment digest;
- the AIRA-owned cgroup scope reference; and
- the output-file set and lifecycle ledger.

The PID is only an ephemeral handle. Every PID used for a later operation is
validated with its recorded process start identity and the cgroup reference;
PID reuse must never redirect `run-kill` to another process.

### 2.2 Closed run status

`status` is a closed enum:

| Status | Meaning |
|---|---|
| `starting` | Run record exists and launch preparation is in progress; no child result is established. |
| `running` | The child/prefix process was launched and its scope is expected to contain live processes. |
| `exited` | AIRA reaped the launched process and established its normal exit code or terminating signal. |
| `killed` | AIRA requested a scope kill, or killed the scope after a capture failure; no normal child exit code is implied. |
| `lost` | AIRA cannot establish the terminal process result or the capture/ledger protocol became incomplete. |

`starting` and `running` are open states. `exited`, `killed`, and `lost` are
terminal. A terminal record is immutable; a later reconcile may append a
correction/finding, but it must not rewrite `lost` to `exited` without an
authoritative wait result.

Normal signal termination is represented as `exited` with `exit_code` absent
and `signal` set. AIRA does not invent a shell-style numeric code. An AIRA
scope kill is `killed`, not a child exit 0 or a fabricated signal result.

## 3. Launch contract

### 3.1 Effective argv and launch-prefix

The launch is the concatenation:

```
effective_argv = launch_prefix_without_its_optional_separator + target_argv
```

The default prefix is the configured design default from §14:

```
agentmux whale run --
```

An empty configured prefix means a bare target exec. The default does not make
agentmux a dependency: if the configured prefix is unavailable, launch fails
honestly; AIRA does not silently fall back to bare execution. A prefix such as
`agentmux` or `whale` may create another wrapper/scope, but AIRA's outer
scope remains authoritative for `run-kill`.

The prefix is represented as argv tokens after one explicit config parse, not
as a shell string. A configured final standalone `--` is a delimiter and is
removed once. A prefix with ambiguous or repeated leading delimiters is
invalid rather than guessed. The target argv is a non-empty token list and is
never reparsed.

The CLI parser has a separate launch-tail rule: the first standalone `--`
after `run` options terminates AIRA option parsing, and every following token
is target argv verbatim, including tokens beginning with `--`. A target
argument cannot accidentally become an AIRA option. A missing delimiter,
empty target, malformed prefix, or attempt to make the generic parser infer
shell syntax is `E_RUN_ARGUMENT_INVALID`/`E_RUN_PREFIX_INVALID`, not a best
effort launch.

MCP supplies `argv` as an array, so it has no delimiter ambiguity. It may
request the configured prefix or an explicitly permitted per-run prefix using
the same token validation. Neither face can request a shell wrapper implicitly.

### 3.2 cwd and environment fidelity

- If cwd is omitted, use the caller's current cwd exactly; do not silently
  substitute the project root. If supplied, require an existing directory and
  pass it unchanged to the child.
- The effective environment begins with the caller's environment in its
  existing key/value form. Explicit environment additions/replacements, if
  exposed by the selected face, are applied as exact `KEY=VALUE` entries;
  empty values are valid. AIRA does not scrub `PATH`, rewrite `PWD`, or
  inject an interactive shell.
- The child inherits no interactive approval channel. With no `--stdin`, its
  stdin is a closed/null input source. With `--stdin <file>`, AIRA opens the
  named file read-only and streams it at launch. With `--stdin -`, AIRA reads
  the caller's stdin during the foreground launch. Neither form creates a
  post-launch input endpoint.
- The Run record stores an environment digest, never the environment values.
  The digest binds the execution identity without making routine secrets part
  of the run ledger. The exact canonicalisation is an architect decision in
  §12; until fixed, no consumer may compare digests as if they were stable.

### 3.3 No approval prompt

`aira run` is a non-interactive primitive. It does not ask whether the command
is safe, whether to create a cgroup, or whether to use the configured prefix.
Permission, cwd, cgroup, file, and argv failures are returned with stable
codes. An agent or project harness may decide whether to invoke the primitive;
the runner does not add an untestable prompt path.

## 4. Capture and I/O protocol

Capture is the delicate part of M12. The file is the authoritative byte stream;
terminal presentation is not evidence.

### 4.1 Separate streams: default

For a normal run, AIRA creates two files before launch:

```
RUN-n.out   raw child stdout bytes
RUN-n.err   raw child stderr bytes
```

The child receives separate stdout and stderr pipes. AIRA owns both read ends
and continuously drains both until EOF, writing the bytes directly and in
order received to their corresponding files. A live `run-log` reader is
entirely optional: no absence, slowness, disconnect, or MCP backpressure may
stop the drain or block the child.

The files are binary-safe. No UTF-8 coercion, newline conversion, trimming,
redaction, line buffering, or text filtering occurs in the capture path. The
record includes byte counts and completion/error markers so a reader can tell
complete output from an incomplete file.

### 4.2 `--merge`: real kernel ordering

`--merge` changes the child fd topology before exec. AIRA creates one capture
pipe/file path and performs the equivalent of `dup2(stderr, stdout)` in the
child setup, so writes from both descriptors enter one kernel stream. The
single raw file is:

```
RUN-n.log
```

This is the only M12 merged-output contract. AIRA must not run two readers and
concatenate `RUN-n.out` and `RUN-n.err` afterward; that cannot recover their
cross-stream order. In merged mode the output refs contain `log` only, and
`out`/`err` are absent rather than aliases that imply separate fidelity.

`--merge` is incompatible with any future separate-stream-only option. It is
valid with `--follow`, because follow reads the one append-only log. It does
not imply a PTY or terminal escape injection.

### 4.3 Drain, write, and disk-full honesty

The capture workers have independent stream errors and a shared run failure
signal:

1. Read raw bytes until EOF.
2. Write every byte to its assigned file, handling short writes.
3. Treat `ENOSPC`, quota exhaustion, write failure, close failure, and required
   sync failure as a capture failure with a stable code.
4. Do not acknowledge bytes that were not durably accepted by the file
   protocol. Do not silently discard the remainder, substitute a zero-length
   file, or report complete output.
5. On a capture failure, request an AIRA-owned scope kill if processes remain,
   continue only the bounded cleanup needed to avoid a pipe deadlock, and
   terminally record the capture error. If a wait result was not established,
   status is `killed` or `lost`, never `exited` with a fake 0.

If the child exits before a disk-full error is observed, the child exit code
may still be recorded as factual, but the run carries
`capture_complete=false` and the stable disk/capture failure code. Such a run
is not eligible for a future tests-green consumer. A capture-reader crash,
missing EOF, or other failure that prevents AIRA from establishing the
capture protocol makes status `lost` (or `killed` when AIRA's scope kill is
the established terminal action) and removes any unproven exit code.

The terminal response fails with `E_RUN_OUTPUT_DISK_FULL` for disk-full and
`E_RUN_CAPTURE_FAILED` for another capture failure. The raw prefix that did
make it to disk remains inspectable and is marked partial; there is no silent
truncation.

### 4.4 stdin secrecy posture

`store-stdin` is off by default. `--stdin` controls the launch source, not
whether input is persisted. Only an explicit `--store-stdin` requests:

```
RUN-n.in
```

and an input output-ref. AIRA must reject `--store-stdin` without a concrete
launch-time source rather than capturing an implicit interactive stream. The
stdin capture is raw and binary-safe, but its secret-bearing nature is called
out in the record and retention policy.

There is no M12 `run-input`, FIFO, socket, or MCP argument that can push bytes
after launch.

## 5. Scoped cgroup-v2 kill

### 5.1 Ownership rule

Every successfully launched M12 run has one AIRA-owned cgroup-v2 scope. The
scope contains the actual launch-prefix process and therefore the target and
its descendants. AIRA never derives kill authority from a prefix-specific
name such as `whale.slice`, never kills a shared slice, and never assumes that
the main PID is the whole run.

Nested cgroups made by a prefix remain inside AIRA's scope subtree where the
delegation rules allow them; killing the AIRA parent scope must cover the
subtree. A prefix that deliberately moves a process outside the owned
subtree is not a supported escape hatch and must be surfaced as a scope
integrity failure if detectable.

### 5.2 Creation and delegation mechanism

The platform adapter is Linux cgroup-v2 only for M12 and uses pure Go plus
syscalls/filesystem operations; it does not use cgo, libsystemd, or a shell
helper. The design requires:

1. Detect a unified cgroup-v2 mount and a configured/delegated parent in which
   the AIRA process can create a child cgroup and write the required control
   files.
2. Create a unique per-run child scope directory under that parent, with
   restrictive permissions and a recorded scope identity/path.
3. Start the launch-prefix process directly into that cgroup using the
   strongest kernel-supported child-placement mechanism available to the
   implementation (the preferred mechanism is a syscall path that accepts a
   cgroup fd at child creation). A post-start move that leaves a window for
   descendants to escape is not sufficient as the only mechanism.
4. Verify the launched process identity and cgroup membership before declaring
   the run `running`.
5. On `run-kill`, signal the scope's members with a bounded TERM grace period,
   then use the cgroup-v2 whole-scope kill operation for the final guarantee.
   The operation is idempotent and must cover descendants created after the
   first signal.
6. After the cgroup reports empty, remove the per-run scope. Removal is
   cleanup, not evidence of exit success; an absent scope with no wait record
   means `lost`.

The exact parent path, delegation setup, minimum kernel/syscall support, and
whether an administrator/systemd user delegation is required are intentionally
not hidden. They are load-bearing open decisions in §12. M12 must probe them
and return a stable result, not print an implementation detail and proceed.

### 5.3 cgroup-v2 unavailable: fail closed

If cgroup-v2 is not mounted, the parent is not delegated, creation or child
placement cannot be verified, or the required whole-scope kill operation is
unavailable, M12 **does not launch by default**. It returns
`E_RUN_SCOPE_UNAVAILABLE`/`E_RUN_SCOPE_INVALID` with infrastructure exit class
4. There is no silent process-group or main-PID fallback, because that would
contradict the run-kill contract for `setsid`, double-fork, and descendants.

If the architect later wants an explicitly opt-in unscoped mode, it must be a
separate state and contract (`scope=unscoped`, a warning/failure code, and
`run-kill` refusing to claim descendant safety). It is not part of M12's
default runner-lite scope and cannot be smuggled in as “fallback”.

## 6. Exit-code fidelity, verdicts, and honesty

### 6.1 The authoritative result

The child wait result, not the last log line and not the AIRA process's own
exit alone, establishes the Run's `exit_code`:

- normal child exit: record its exact non-negative integer;
- signal termination: record `signal`, leave `exit_code` absent;
- AIRA cgroup kill: status `killed`, `exit_code` absent unless a prior wait
  record already established a normal exit before the kill linearized;
- lost wait, capture, or terminal-record evidence: status `lost`,
  `exit_code` absent.

`exit_code == 0` is therefore meaningful only when status is `exited`, the
terminal record is complete, and capture errors are absent. A response or
consumer must never turn `running`, `killed`, `lost`, missing, or partial data
into an exit 0. This is the boundary required by future tests-green wiring:
M12 does not wire that gate, but it must not make a false green input possible.

The AIRA response exit is an AIRA exit *class*, not a promise to mirror an
arbitrary child's numeric exit. Normal child non-zero is returned as a stable
run-failure class while the exact child code remains in `data.run.exit_code`.
MCP receives both values. A future shell-pass-through policy must be an
explicit face decision, not an accidental consequence of `Response.Exit`.

### 6.2 Run outcome table

| Situation | Run status | Stable code | Exit class | Honest interpretation |
|---|---|---|---:|---|
| Child exits 0, complete capture and terminal record | `exited` | `OK` | 0 | Established success; no gate is implied. |
| Child exits non-zero | `exited` | `E_RUN_FAILED` | 1 | Established child failure; exact child code is retained. |
| AIRA scope kill completes | `killed` | `E_RUN_KILLED` | 1 | Explicitly killed; no exit 0 or inferred child code. |
| Wait/terminal result cannot be established | `lost` | `U_RUN_EXIT_UNKNOWN` | 3 | Unevaluated; never green. |
| Capture writer hits disk full | `killed`/`exited` | `E_RUN_OUTPUT_DISK_FULL` | 4 | Output is partial/error-marked; even a known child code is not clean evidence. |
| Capture reader/protocol crashes before capture is established | `killed`/`lost` | `E_RUN_CAPTURE_FAILED` | 4 | Exit status is not established by inference; no clean run evidence. |
| cgroup boundary unavailable before launch | no launched run | `E_RUN_SCOPE_UNAVAILABLE` | 4 | Fail closed; no unsafe execution occurred. |
| Requested run ID/output cannot be found | n/a | `E_RUN_NOT_FOUND` | 2 | Invocation/selector failure. |
| Output was evicted or unreadable | terminal status unchanged | `U_RUN_OUTPUT_UNAVAILABLE` | 3 | Metadata may remain, but output-dependent evaluation is unevaluated. |

The exact code names are proposed additions to the single
`internal/store.ExitCodes` catalog. No runner adapter may invent aliases.
`OK` is the normal response code; `U_*` is never converted to warning/pass or
exit 0. If an implementation needs a more specific code, it must preserve the
same meaning and exit class in the generated response contract.

### 6.3 Stable code registration

The implementation plan must add at least these entries to
`internal/store.ExitCodes` and therefore to `core.ResponseContract()`:

| Family | Codes | Exit class |
|---|---|---:|
| Invocation | `E_RUN_ARGUMENT_INVALID`, `E_RUN_PREFIX_INVALID`, `E_RUN_CWD_INVALID`, `E_RUN_ENV_INVALID`, `E_RUN_STDIN_INVALID`, `E_RUN_NOT_FOUND` | 2 |
| Established operation failure | `E_RUN_FAILED`, `E_RUN_KILLED` | 1 |
| Capture/scope infrastructure | `E_RUN_OUTPUT_OPEN`, `E_RUN_OUTPUT_DISK_FULL`, `E_RUN_CAPTURE_FAILED`, `E_RUN_SCOPE_UNAVAILABLE`, `E_RUN_SCOPE_INVALID`, `E_RUN_LAUNCH_FAILED` | 4 |
| Unevaluated/lost | `U_RUN_EXIT_UNKNOWN`, `U_RUN_OUTPUT_UNAVAILABLE`, `U_RUN_RECONCILE_REQUIRED` | 3 |

The `E_RUN_LAUNCH_FAILED` classification must distinguish “AIRA could not
start the requested argv” from a child that started and returned non-zero;
neither may be represented as success. Existing `E_DB_*`, `E_JOURNAL_*`,
`E_RECEIPT_IO`, and `E_INTERNAL` remain the infrastructure fallbacks for
store/ledger failures not specific to a runner condition.

## 7. Run data model and durability

### 7.1 Run record

The closed, versioned Run record is the protocol object, whether its authority
is ultimately the common-dir ledger or another durable home:

| Field | Contract |
|---|---|
| `schema_version` | Versioned record format; unknown versions are not silently parsed. |
| `id` | Stable run identity; exact allocator scheme remains open in §12. |
| `argv` | Exact target token array, binary-safe as the transport permits. |
| `cwd` | Effective absolute/normalised launch directory, with no semantic substitution. |
| `env_digest` | Digest of the effective environment; values are not persisted in the run record. |
| `launch_prefix` | Exact effective prefix token array, including its configured identity but not a shell string. |
| `cgroup_scope` | Opaque scope reference/path, parent identity, and placement verification state. |
| `started_at`, `ended_at` | Wall-clock timestamps; lifecycle ordering comes from record sequence, not wall time. |
| `status` | Closed enum from §2.2. |
| `exit_code` | Exact normal child code when established; absent otherwise. |
| `signal` | Terminating signal when independently established; absent for AIRA-killed/lost results without wait evidence. |
| `output_refs` | `out` + `err`, or `log`, and optional `in`; each has path/opaque ref, byte count, digest if complete, and capture state. |
| `capture_complete` | Explicit all-stream completion boolean; false/unknown is not a green result. |
| `stdin_stored` | Whether `RUN-n.in` exists; false by default. |
| `scope_kill` | Requested/started/completed, grace outcome, and actor/at when applicable. |
| `error_codes` | Stable ordered codes and machine-readable error markers; no driver-string parsing by consumers. |
| `pid_identity` | Ephemeral launch PID plus process start identity while useful; never the sole kill authority. |

The record does not contain telemetry, rusage, a test report, gate verdict,
secret environment values, or an implicit “green” field.

### 7.2 Proposed durability split

The recommended M12 layout is:

- **Run ledger — common-dir audit class:** append-only, canonical records under
  the machine's git common-dir AIRA area. It contains `starting`/`running`,
  terminal lifecycle facts, capture error facts, scope refs, and record
  digests. It is the authority for reconstructing whether AIRA established an
  exit result. The ledger is machine-local and outside the commit graph.
- **Run output — machine-local files:** a gitignored run directory, with
  `RUN-n.out`, `RUN-n.err`, `RUN-n.log`, and optional `RUN-n.in`. Raw bytes are
  not in git, the ledger, or SQLite. Output refs point to these files and
  retain completion/size/digest metadata.
- **SQLite — rebuildable projection:** indexed Run rows, current status
  queries, selector lookup, and output metadata. SQLite is not allowed to
  mint a terminal success or repair a missing ledger record.

This follows AIRA's content-truth/rebuildable-index layering for lifecycle
facts, while keeping high-volume bytes out of both content and DB. It is a
deliberate exception to the top-level operational-telemetry default and is
therefore an architect decision, not an unmentioned implementation detail.

If the architect instead chooses DB-authoritative run metadata, the design
must specify an equally durable crash/DB-loss protocol before implementation;
“reconcile from a PID” is not enough to reconstruct a missing wait status and
must produce `lost`, never exit 0.

### 7.3 Write and crash protocol

The proposed foreground sequence is:

1. Validate argv, prefix, cwd, env, stdin, merge mode, and cgroup capability.
2. Allocate a run ID and create the per-run output directory/files without
   exposing the run as successful. Record `starting` durably.
3. Create and verify the private cgroup scope; append its opaque reference.
4. Start the launch prefix/target into that scope, record PID identity, and
   append `running` only after membership and pipe setup are verified.
5. Drain stdout/stderr (or the merged pipe) independently to files while a
   wait operation observes the child. Write capture error markers immediately
   and never overwrite already-recorded bytes.
6. Reap the launch process, wait for all capture readers to reach EOF, close and
   sync output files, and verify the scope is empty.
7. Append exactly one terminal `exited`, `killed`, or `lost` ledger record with
   exit/signal, capture state, error codes, timestamps, and output refs. Remove
   the empty scope after the record has the required facts.
8. Update/rebuild the SQLite projection from the ledger. A projection failure
   does not turn a factual terminal record into a fake success.

Crash windows are explicit:

- before child launch: `starting` with no child is a failed launch or an
  incomplete record, not a running/successful run;
- after child launch but before terminal record: reconcile may find a live
  scope and leave `running`, or an empty scope and mark `lost`; it may not
  infer exit 0;
- after terminal ledger append but before DB projection: replay the ledger;
- after DB loss: rebuild the projection from the ledger and output refs;
- after ledger corruption/loss: report stable journal/reconcile failure or
  `U_RUN_RECONCILE_REQUIRED`; never manufacture a terminal result.

`run-kill` uses the durable scope ref, not only the PID, so a separate AIRA
process can kill a still-live foreground run after its original caller has
failed. If the parent died and no one reaped the child, a later observer may
only be able to establish `lost`; the minimal-supervisor question is open in
§12.

## 8. `run-log` protocol

### 8.1 CLI view and follow

`aira run-log <run-id>` views the captured output. The proposed options are:

```
--stream out|err|merged
--follow
--from <byte-offset>
--tail <bytes>
--full
```

Default stream selection follows the record: separate runs require an explicit
stream or a deterministic two-stream summary; merged runs select `log`.
`--full` is opt-in for potentially large output. CLI raw output is emitted as
bytes, not decoded text; status/metadata belongs in the structured response or
diagnostic channel so binary output is not corrupted.

`--follow` reads the append-only capture file, emits new bytes, and polls the
run ledger/status until terminal. It needs no daemon and does not hold a child
pipe. If the run becomes `lost`, the follow ends with that status and a stable
non-success result. If output is partial, it says so. A follow reader ending
does not close or influence the writer.

How long a follow may wait, polling interval, and whether a bounded timeout is
required are open in §12; no choice may change the no-daemon guarantee.

### 8.2 MCP output and overflow discipline

`aira_run_output` returns structured chunks, not an unbounded JSON string:

```
{ run_id, stream, encoding:"base64", offset, next_offset,
  total_bytes, bytes, complete, truncated, output_state,
  run_status, error_codes }
```

Base64 is used so arbitrary bytes survive JSON/MCP transport. The server
applies a byte cap before serialisation. If more bytes remain, it sets
`truncated=true`, returns `next_offset`, and reports the total/remaining
range; it never silently clips the tail. Clients request subsequent chunks by
offset. A missing, evicted, partial, or unreadable file is an explicit output
state and `U_RUN_OUTPUT_UNAVAILABLE`, not an empty successful response.

For `follow=true`, MCP returns a bounded current chunk and cursor. A client
polls again with `from=next_offset`; a long-lived MCP stream is not part of
M12. An implementation may wait briefly for new bytes, but it must bound the
wait and describe the returned cursor/status.

## 9. Faces and dispatch integration

All three operations are entries in the same dispatch table used by
`core.Do`, the CLI parser, MCP schema generation, Skill artifacts, help, and
the generated agent guide. No face owns launch, capture, cgroup, or response
logic.

### 9.1 CLI

The M12 CLI surface is:

| Command | Purpose | Safety |
|---|---|---|
| `aira run [options] -- <argv...>` | Launch a foreground run and return the Run record/result. | execute |
| `aira run-kill <run-id>` | Kill the entire AIRA-owned cgroup scope. | execute |
| `aira run-log <run-id> [options]` | View/follow previously captured output. | read |

`run` options are limited to the M12 contract: configured/per-run prefix,
cwd, effective environment overrides if enabled, `--merge`, launch-time
`--stdin`, and explicit `--store-stdin`. There is no detach, report,
telemetry, PTY, or live-input option; following is owned by `run-log`.

The CLI parser must special-case the target delimiter as described in §3.1;
the generic “an option beginning with `--` needs a value” path cannot consume
the child argv. Existing grouped-verb parsing remains the pattern for
canonical subverbs, but these three names must remain unambiguous in help and
dispatch.

### 9.2 MCP

M12 adds exactly these tools:

- `aira_run` — launch; `argv` is an explicit string array and options mirror
  the CLI contract;
- `aira_run_output` — view/follow bounded raw bytes with cursor and explicit
  truncation metadata;
- `aira_run_kill` — kill by run ID and return the resulting status/evidence.

They are three separate generated MCP tools, not private MCP-only handlers.
Each operation's M8b metadata declares summary, safety class, arguments,
requiredness, enums, and canonical examples. The tool response retains the
Run record/result and stable code; MCP transport errors cannot erase a
run-level `lost`, `killed`, or disk-full outcome.

### 9.3 Descriptor metadata and parity

The dispatch descriptors must add `execute` to the closed safety-class
vocabulary and label subprocess launch/kill with it, rather than labelling an
arbitrary subprocess as a harmless read. The generated response contract must
include all `E_RUN_*`, `U_RUN_*`, and the existing shared codes.

Parity tests must prove:

- CLI `run` and MCP `aira_run` create the same canonical `core.Request` for
  equivalent argv/options;
- CLI `run-log` and MCP `aira_run_output` select the same stream/offset/follow
  semantics;
- CLI `run-kill` and MCP `aira_run_kill` select the same run identity and kill
  operation;
- generated schemas/help/Skill examples expose no deferred Phase-5 option;
- the real CLI, MCP, and core paths produce equivalent success/failure data,
  not merely equivalent parser structs.

## 10. Invariants

1. **AIRA owns the scope.** Every launched M12 run has one verified private
   cgroup-v2 scope containing the prefix/target tree, or launch fails closed.
2. **Kill is whole-scope.** `run-kill` never kills only the main PID and never
   names a shared prefix-owned slice.
3. **No unsafe fallback is silent.** Missing cgroup-v2/delegation/placement or
   whole-scope kill support is a stable failure; any future unscoped mode must
   be explicit and non-green.
4. **Exact argv survives.** Target tokens, including `--`-prefixed tokens,
   reach the child without shell reparsing or option loss.
5. **cwd/env fidelity.** The child receives the selected cwd and effective
   environment; the record binds them with a digest and does not claim a
   different cwd/env.
6. **No prompt.** The runner never waits for an interactive approval or
   hidden stdin response.
7. **Every pipe drains.** Capture continues to files whether or not a live
   `run-log` reader exists; a reader cannot create child backpressure.
8. **Raw bytes remain raw.** Capture and MCP base64 transport do not corrupt,
   decode, trim, reorder, or text-normalise bytes.
9. **Merge means dup2.** Merged output comes from one exec-time kernel stream,
   never post-hoc file concatenation.
10. **Disk-full is visible.** Any incomplete write/close/sync marks the run
    with a stable error and prevents clean-run consumers from treating it as
    success.
11. **stdin is opt-in to storage.** Launch input may be supplied without being
    stored; input persistence requires explicit `--store-stdin`.
12. **Exit 0 is earned.** Only `exited` + established `exit_code=0` + complete
    capture + complete terminal ledger can represent a clean run.
13. **Lost never becomes zero.** A missing wait, crashed capture protocol,
    removed scope without a wait record, or DB/ledger ambiguity is `lost`/
    unevaluated, never inferred success.
14. **Terminal facts are immutable.** Reconcile can add findings or rebuild a
    projection, but cannot invent or rewrite an exit result.
15. **DB is not authority if the ledger decision stands.** SQLite loss or a
    stale projection is repaired from the durable lifecycle record; a DB row
    alone cannot mint a Run.
16. **Output truncation is explicit.** CLI and MCP identify partial ranges,
    caps, offsets, eviction, and read errors; an empty/short response never
    masquerades as complete output.
17. **No deferred feature leaks in.** M12 descriptors expose no detach,
    run-input, telemetry/gate auto-wiring, live supervisor tee, PTY, rusage,
    or report option.
18. **Faces cannot drift.** CLI, MCP, Skill, help, stable codes, and examples
    come from the same dispatch/response contracts.

## 11. Tests (TDD; every confirmed counterexample becomes a regression test)

### 11.1 Launch and argument fidelity

1. Bare prefix launches the exact argv and empty prefix means no wrapper.
2. Default prefix is tokenised as `agentmux whale run` with one delimiter and
   the target is appended exactly once.
3. A configured prefix with shell metacharacters is passed as tokens or
   rejected; no shell expands it.
4. CLI parsing preserves a child argument beginning with `--` after the one
   launch delimiter and rejects missing/ambiguous delimiter forms.
5. MCP argv arrays and equivalent CLI argv construct byte-for-byte equivalent
   canonical requests.
6. Cwd is exact, a nonexistent cwd returns `E_RUN_CWD_INVALID`, and no
   project-root substitution occurs.
7. Inherited and explicit environment values reach the child; empty values
   survive; the record contains only the agreed digest.
8. Launch never presents an approval prompt and a command needing interactive
   approval receives closed/null stdin unless `--stdin` was supplied.
9. Prefix-not-found, target-not-found, invalid env, and invalid stdin produce
   stable codes and no launched scope.

### 11.2 Capture correctness and I/O

10. stdout and stderr are captured separately with exact bytes, including NULs,
    invalid UTF-8, long lines, no final newline, and interleaved writes.
11. A run with no `run-log` reader drains enough output to let a large writer
    finish; there is no pipe/backpressure stall.
12. A slow/disconnected follow reader does not alter capture bytes or child
    completion.
13. `--merge` uses one stream and preserves a fixture's kernel write ordering;
    separate files are not produced or concatenated as a substitute.
14. Merged binary output is returned identically by CLI and base64-decoded MCP.
15. Short writes, close errors, sync errors, quota/ENOSPC, and output-open
    failures produce the exact stable capture code, partial markers, and no
    clean success.
16. Disk-full handling drains/cleans without deadlock, kills the remaining
    scope when required, and never reports a fake exit 0.
17. stdin is absent/closed by default; file and `-` sources work only at
    launch; `--store-stdin` alone is rejected; stored input is exact and
    explicitly referenced.

### 11.3 cgroup scope and kill

18. A run's cgroup is private, recorded, verified before `running`, and removed
    only after it is empty.
19. A prefix wrapper plus child plus grandchild all appear inside the AIRA
    scope, including a child that calls `setsid`.
20. `run-kill` kills the whole scope, not just the main PID; descendants cannot
    keep running through the grace/final-kill race.
21. Killing an already exited run is idempotent and does not change an
    established terminal record; kill-versus-exit races have deterministic
    linearisation and no fake exit code.
22. PID reuse and a stale PID cannot make `run-kill` target an unrelated
    process; scope identity remains the authority.
23. Missing cgroup-v2 mount, missing delegation, denied creation, unavailable
    child placement, and unavailable whole-scope kill all fail closed with
    stable infrastructure outcomes and no unsafe launch.
24. Scope cleanup failure is visible and does not make a complete run appear
    incomplete or vice versa.

### 11.4 Exit honesty and recovery

25. Child exit 0 records exact zero only after wait, full drain, output close,
    and terminal ledger completion.
26. Child exit 7 records `status=exited`, `exit_code=7`, `E_RUN_FAILED`, and
    never collapses the child code to zero.
27. AIRA scope kill records `status=killed`, stable kill code, and no invented
    normal exit code.
28. Parent/capture/wait failure records `lost` or the explicitly established
    killed state; no missing result becomes `exited/0`.
29. A running child after the foreground parent dies remains killable by its
    cgroup ref; once its scope empties without a wait record, reconcile marks
    the result lost rather than successful.
30. A DB projection dropped after terminal ledger append rebuilds the exact
    Run and status from the ledger.
31. A crash is injected at every lifecycle write boundary; reconcile repairs
    only the projection/documented derived side and emits stable findings for
    incomplete authority records.
32. A malformed, reordered, or missing ledger record returns journal/reconcile
    failure or `U_RUN_RECONCILE_REQUIRED`, never a green record.
33. Output-file eviction/unreadability preserves metadata but returns explicit
    `U_RUN_OUTPUT_UNAVAILABLE` for output-dependent reads.

### 11.5 `run-log` and overflow

34. CLI view supports exact stream selection, byte offsets, tail/full policy,
    and binary output without metadata bytes contaminating stdout.
35. CLI follow observes appended data without a daemon, terminates at terminal
    state, and reports lost/partial output honestly.
36. MCP output caps are deterministic; every clipped response includes
    `truncated`, total/returned bytes, next offset, and output state.
37. Repeated MCP offset requests reconstruct exactly the stored byte stream;
    an empty response is distinguishable from EOF and unavailable output.
38. MCP follow is bounded/cursor-based and never becomes an unbounded JSON
    response or hidden subscription.

### 11.6 Faces, descriptors, and static build

39. Dispatch metadata covers `run`, `run-kill`, and `run-log` with correct
    safety, examples, requiredness, and no Phase-5 args.
40. Generated MCP tool list contains exactly `aira_run`, `aira_run_output`,
    and `aira_run_kill` in addition to existing tools; no aliases leak.
41. CLI↔core↔MCP parity covers launch, merge, stdin, output cursor/follow, and
    kill arguments using real entrypoints.
42. Stable code/exit mapping appears identically in CLI, MCP, Skill, and guide
    response contracts; `U_*` exits 3 and never pass/0.
43. The ordinary foreground path works with no daemon and no network.
44. The static, no-cgo build remains valid; Linux cgroup support is isolated
    behind the platform boundary and unsupported platforms fail honestly.

## 12. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Prefix creates a nested/shared scope and kill reaches too little or too much | AIRA creates/verifies its own private parent scope; kill uses that scope recursively, never a prefix name; test wrapper/grandchild/setsid cases. |
| Post-start cgroup movement races with an early child | Require a verified child-placement mechanism with no descendant escape window; otherwise fail closed. |
| cgroup permissions vary across desktops/containers | Probe delegation before launch, document the required parent, and return stable infrastructure failure rather than pretending PID kill is scoped. |
| A foreground parent dies while the child survives | Persist scope refs before launch, make another AIRA process able to kill the scope, and classify an unreaped result as `lost`; keep the minimal-supervisor question open. |
| A pipe reader blocks behind a live consumer | No child-facing live tee exists in M12; drain workers write files independently of `run-log`. |
| Separate streams are incorrectly merged later | `--merge` changes fd topology before exec; tests assert ordering and refs. |
| Disk-full produces partial but green output | Treat every write/close/sync failure as a stable run error, mark partial, and exclude it from clean-run consumers. |
| Binary output is corrupted by text/MCP handling | Raw file bytes, CLI byte output, MCP base64 with offsets and counts. |
| stdin leaks secrets or hangs on a prompt | Closed/null stdin by default, launch-only explicit source, `store-stdin` opt-in, no prompt path. |
| DB loss invents or erases a run result | Common-dir lifecycle authority/projection replay proposal; missing wait evidence becomes lost, not exit 0. |
| Output retention makes `run-log` lie | Keep metadata and explicit eviction state; return `U_RUN_OUTPUT_UNAVAILABLE`, never an empty successful log. |
| CLI/MCP/Skill options drift into Phase 5 | One dispatch table, generated schemas/help, parity tests, and a descriptor test that rejects deferred options. |
| Child non-zero and AIRA failure exits are confused | Preserve exact child `exit_code` in Run data and use stable AIRA exit classes for the face; document the distinction. |

## 13. Open decisions for the architect

These are intentionally not resolved unilaterally. Each changes a load-bearing
part of the M12 contract.

1. **cgroup-v2 layout and delegation assumptions:** What parent hierarchy may
   AIRA create under on supported machines? Is a systemd user delegation an
   allowed prerequisite, or must M12 support a directly delegated subtree?
   Which kernel minimum and child-placement syscall path are supported, and
   which cgroup files are required for recursive kill and cleanup?
2. **Launch-prefix default:** Is `agentmux whale run --` the shipped default
   exactly, including its one trailing delimiter, and is an unavailable
   default a launch failure rather than a bare-exec fallback? Is per-run
   prefix override allowed, or config-only?
3. **Run ID scheme:** Should IDs use the AIRA allocator plus a run prefix, or
   a local `RUN-n` counter? How are IDs collision-safe across worktrees, and
   does the filename remain `RUN-n.out` when the public ID is not `RUN-n`?
4. **Environment digest:** What canonical byte representation is hashed?
   Sorted `KEY=VALUE` entries, duplicate-key rejection, locale/encoding rules,
   and whether any volatile variables are excluded must be fixed before
   consumers compare lane identity.
5. **Disk-full detection:** Is an immediate write/close/sync error sufficient,
   or must M12 preflight quota/free space and/or use filesystem project quotas?
   What durability level does “written” mean before the terminal record is
   allowed to claim `capture_complete`?
6. **`run-log --follow` without a daemon:** What polling interval and maximum
   wait/timeout should the CLI use? Does `--follow` run until terminal by
   default, or require an explicit timeout? How does a follow detect a lost
   parent while the cgroup is still live?
7. **Minimal supervisor for foreground kill-after-parent-exit:** Is the
   no-supervisor foreground rule absolute, accepting `lost` after parent death,
   or is a tiny non-detached reaper allowed solely to preserve wait/kill
   ownership? Any supervisor here must not quietly become Phase-5 detach or
   change the owner decision that the daemon is only the detached-run
   supervisor.
8. **Run lifecycle authority:** Does the proposed common-dir audit ledger
   become the accepted M12 exception, or does the architect require DB-only
   metadata with a different durable crash protocol? If common-dir is chosen,
   what record authentication/locking/fsync rules and retention apply?
9. **Capture-file retention/eviction:** What directory, byte cap, TTL, and
   eviction order apply? Are active-run and live-follow files protected? Does
   eviction delete raw data or only compress it, and which metadata proves the
   file was evicted rather than never captured?
10. **MCP/CLI response exit policy:** Should `aira run` return AIRA class 1
    for a known child non-zero, mirror the child's numeric exit for CLI only,
    or expose both through a future explicit mode? The Run record must retain
    exact child status under all choices.
11. **Environment option surface:** Are explicit env overrides part of M12,
    or is faithful inheritance plus a configured environment the whole scope?
    If overrides are included, what syntax and secret-handling guidance does
    each face use?
12. **Run output default selection:** For separate out/err, should
    `run-log <id>` require `--stream`, present a structured two-stream view,
    or choose stdout by default? The choice must not imply cross-stream order.
