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
  waits for it, and writes the terminal record. There is **no supervisor**:
  the daemon is **only** the Phase-5 detached-run supervisor. If the
  foreground parent dies, the run is `lost`, while its durable cgroup scope
  reference remains operator-killable by run ID.
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
- The durability boundary is an append-only **common-dir run ledger** with a
  SQLite projection. This intentionally elevates the small lifecycle record
  above ordinary retention-capped telemetry so DB loss and an interrupted
  foreground parent cannot manufacture a green result. Capture files are
  machine-local files, never ledger or SQLite blobs.
- The launch prefix default is **empty**, meaning a bare exec. `agentmux` and
  `whale` are optional configured prefixes only; an arbitrary
  work-submitting/migrating prefix is outside the M12 contract. The residual
  guarantee is bounded to AIRA's owned cgroup subtree (§5.4).
- The only supported scope prerequisites are a cgroup-v2 unified mount, a
  delegated writable parent, `clone3` with `CLONE_INTO_CGROUP`, and writable
  `cgroup.kill`, on kernel 5.14 or newer. Every capability is probed before
  launch; any missing capability hard-fails with `E_RUN_SCOPE_UNAVAILABLE`.
- A daemonless observation limit is accepted for scope migration: if the
  launch process atomically migrates out of its cgroup scope and exits within
  the observation window, a foreground runner without a supervisor cannot
  reliably distinguish that history from a clean exit. The reaped launch
  process exit code remains factual, but the invariant bounded here is the
  detectable scope-integrity guarantee (ordinary observable migrations are
  still surfaced as `E_RUN_SCOPE_MIGRATION`/`handoff-unverified`). A later
  supervised mode can close this residual window; M12 deliberately has no
  supervisor.
- Run IDs use a local `RUN-n` counter, not the git ticket allocator. The
  environment digest uses the specified length-prefixed encoding (§3.2),
  disk-full is detected from `ENOSPC`/capture write failures and requires
  successful close+fsync for `capture_complete`, and `run-log --follow` polls
  approximately every 200 ms until terminal.
- The run face returns an AIRA exit class; the exact child exit code is always
  preserved in the Run record and response data. It is not replaced by the
  AIRA class.

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
the whole owned scope, and distinguish an established launch-process exit 0
from a killed, lost, scope-integrity, or capture-corrupted result. M12 reports
the **launch process's own wait status**. It does not claim that arbitrary
descendant work outside the owned subtree succeeded.

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
- No claim that a deliberately migrated descendant can be killed safely or
  that its work succeeded. M12 has no unscoped fallback; its containment and
  clean-success guarantee is bounded to the AIRA-owned cgroup subtree.

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

The default prefix is **empty**, so the default is a bare target exec. A
configured prefix such as `agentmux whale run --` is optional; `agentmux` and
`whale` are examples of configured wrappers, not AIRA dependencies. If a
configured prefix is unavailable, launch fails honestly; AIRA does not
silently fall back to bare execution. A configured prefix is in-contract only
when it remains inside AIRA's scope and does not submit work elsewhere or
deliberately migrate descendants. Arbitrary work-submitting prefixes are
outside the M12 contract. AIRA's outer scope remains authoritative for
`run-kill`, but that authority is bounded to its owned subtree (§5.4).

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
  Canonical encoding is length-prefixed and byte-oriented: after rejecting
  duplicate keys, sort entries by key and concatenate, for each entry,
  `uvarint(len(key)) || key || uvarint(len(value)) || value`; hash the exact
  concatenation with SHA-256. This prevents delimiter collisions from LF,
  NUL, or `=` in testable byte inputs. A NUL in an actual OS environment
  entry is rejected as an invalid launch environment, but the encoder's unit
  contract still covers arbitrary bytes.

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
5. `capture_complete=true` is permitted only after every capture file has
   reached EOF, closed successfully, and passed the required fsync. On a
   capture failure, request an AIRA-owned scope kill if processes remain,
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

### 4.5 Launch exit versus lingering pipe holders

The launch process's wait does not by itself complete a run. A descendant may
inherit stdout/stderr and keep a pipe open after the launch process exits. M12
uses the finite `run.capture_descendant_grace` parameter (default: 2 seconds,
configurable before launch):

1. Record the launch process's exact wait result, but continue draining and
   writing while waiting for pipe EOF and scope quiescence.
2. During the grace, continue reading all available bytes. A live reader is
   irrelevant; the capture workers remain the drain authority.
3. If the scope or pipe holders remain when the grace expires, invoke the
   terminal scope-kill protocol (§6.3), verify the scope is empty, and continue
   draining until EOF or until a capture failure makes further persistence
   impossible. After a capture failure, bytes may be discarded solely to
   unblock the pipe, with `capture_complete=false` and an explicit marker.
4. Preserve the launch process's exit code X, but add
   `E_RUN_DESCENDANT_KILLED` and `scope_integrity=descendant-killed` when an
   in-scope descendant had to be killed. This is not a clean run even if all
   bytes eventually close and fsync successfully.
5. If the scope empties but a pipe remains open, or a known member disappears
   into another cgroup, stop waiting after the same finite bound, mark
   `handoff-unverified`/`migrated`, and never claim clean success. An escaped
   process is outside AIRA's kill guarantee.

This timeout prevents a descendant-held fd from turning foreground completion
into an unbounded wait while preserving the no-backpressure drain rule.

## 5. Scoped cgroup-v2 kill

### 5.1 Ownership rule

Every successfully launched M12 run has one AIRA-owned cgroup-v2 scope. The
scope contains the actual launch-prefix process and therefore the target and
its descendants. AIRA never derives kill authority from a prefix-specific
name such as `whale.slice`, never kills a shared slice, and never assumes that
the main PID is the whole run.

`clone3` with `CLONE_INTO_CGROUP` closes the placement race, and ordinary
`setsid`, double-fork, and reparent operations do not escape a cgroup. That is
not an assertion that a privileged or delegated descendant cannot deliberately
write itself into another cgroup, invoke `systemd-run`, or create a nested
container. The M12 containment and kill guarantee is **bounded to AIRA's
owned cgroup subtree**. The reported wait result is the launch process's own
result; AIRA never claims that an escaped descendant tree succeeded.

A detectable migration, or an empty scope with an unverifiable handoff, is a
scope-integrity finding and is not a silent clean run. A configured prefix is
therefore in-contract only when it does not submit work elsewhere or migrate
descendants. The empty/bare default sharply limits this residual threat, but
cannot eliminate a deliberately privileged escape that AIRA cannot observe.

### 5.2 Creation and delegation mechanism

The platform adapter is Linux cgroup-v2 only for M12 and uses pure Go plus
syscalls/filesystem operations; it does not use cgo, libsystemd, a shell, or a
helper that moves a process after launch. M12 supports **exactly** this
containment capability set:

1. A cgroup-v2 unified mount is present.
2. A delegated writable parent is available to AIRA, including permission to
   create the per-run child and inspect its membership/events.
3. `clone3` is available and accepts `CLONE_INTO_CGROUP` with the scope fd,
   providing placement at child creation. The minimum supported kernel is
   5.14.
4. The created scope exposes a writable `cgroup.kill` and AIRA can use it for
   whole-scope kill.

AIRA probes **each** capability before launch. It then reserves the run ID,
creates the private scope, durably records its scope reference (§7.3), and
only then invokes `clone3` with `CLONE_INTO_CGROUP`. The launched process and
all ordinary descendants inherit the scope. A post-start fork+move,
process-group, main-PID, `setsid`, or other fallback path does not exist.

On `run-kill`, AIRA records durable kill intent, uses a bounded TERM grace
period for observed members, then uses writable `cgroup.kill` for the final
whole-scope action. It verifies the scope is empty before recording `killed`.
After any terminal operation, an empty scope is removed. Removal is cleanup,
not evidence of exit success; an absent scope with no authoritative wait
record means `lost` or a scope-integrity outcome.

### 5.3 cgroup-v2 unavailable: fail closed

If **any one** of the unified-mount, delegated-parent, clone3/
`CLONE_INTO_CGROUP`, kernel-minimum, scope-creation, or writable
`cgroup.kill` probes fails, M12 returns `E_RUN_SCOPE_UNAVAILABLE` with
infrastructure exit class 4 and launches nothing. There is no fork+move,
process-group, main-PID, systemd-scope, or other unsafe fallback path anywhere
in M12. An unscoped mode would be a different milestone and is not implied by
this design.

### 5.4 Scope-integrity result

The scope guarantee has a bounded threat model. `scope_integrity` is closed:
`contained | handoff-unverified | migrated | descendant-killed`. Clean
success requires `contained`; `handoff-unverified` is used when the scope
empties without enough evidence to distinguish normal descendant exit from an
unobservable handoff, and `migrated` is used when a known member is observed
outside the owned subtree. Both are surfaced as stable scope-integrity
findings and prevent clean success. `descendant-killed` records the honest
outcome when the launch process exited but lingering in-scope descendants had
to be killed after the bounded pipe-drain grace.

## 6. Exit-code fidelity, verdicts, and honesty

### 6.1 The authoritative result

The launch-process wait result, not the last log line and not the AIRA
process's own exit alone, establishes the Run's `exit_code`:

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
MCP receives both values. This is resolved policy for M12; there is no
shell-pass-through mode.

### 6.2 Run outcome table

| Situation | Run status | Stable code | Exit class | Honest interpretation |
|---|---|---|---:|---|
| Child exits 0, complete capture and terminal record | `exited` | `OK` | 0 | Established success; no gate is implied. |
| Child exits non-zero | `exited` | `E_RUN_FAILED` | 1 | Established child failure; exact child code is retained. |
| AIRA scope kill completes | `killed` | `E_RUN_KILLED` | 1 | Explicitly killed; no exit 0 or inferred child code. |
| Wait/terminal result cannot be established | `lost` | `U_RUN_EXIT_UNKNOWN` | 3 | Unevaluated; never green. |
| Capture writer hits disk full | `killed`/`exited` | `E_RUN_OUTPUT_DISK_FULL` | 4 | Output is partial/error-marked; even a known child code is not clean evidence. |
| Capture reader/protocol crashes before capture is established | `killed`/`lost` | `E_RUN_CAPTURE_FAILED` | 4 | Exit status is not established by inference; no clean run evidence. |
| Scope handoff or known migration is detected | `exited` | `E_RUN_SCOPE_HANDOFF`/`E_RUN_SCOPE_MIGRATION` | 4 | Launch exit may be factual; descendant success and containment are not established. |
| Lingering in-scope descendant is killed after grace | `exited` | `E_RUN_DESCENDANT_KILLED` | 4 | Launch exit is factual; scope cleanup was forced and the run is not clean. |
| cgroup boundary unavailable before launch | no launched run | `E_RUN_SCOPE_UNAVAILABLE` | 4 | Fail closed; no unsafe execution occurred. |
| Requested run ID/output cannot be found | n/a | `E_RUN_NOT_FOUND` | 2 | Invocation/selector failure. |
| Output was evicted or unreadable | terminal status unchanged | `U_RUN_OUTPUT_UNAVAILABLE` | 3 | Metadata may remain, but output-dependent evaluation is unevaluated. |

The exact code names are required additions to the single
`internal/store.ExitCodes` catalog. No runner adapter may invent aliases.
`OK` is the normal response code; `U_*` is never converted to warning/pass or
exit 0. If an implementation needs a more specific code, it must preserve the
same meaning and exit class in the generated response contract.

### 6.3 Terminal-state arbitration

The foreground waiter and `run-kill` are concurrent writers. Exactly one
terminal state may win. Each run has a per-run lock (or an equivalent durable
compare-and-swap on the lifecycle state), and the terminal record has a
unique terminal slot:

1. The waiter publishes a `wait-observed` result through the per-run lock/CAS
   immediately at reap, before doing capture finalisation. That publication
   is the wait-result ordering fact and is also durably appended. If it has
   reaped a normal exit before a kill intent linearizes, it owns the terminal
   slot and appends `exited` once, even if terminal-file finalisation is still
   pending.
2. `run-kill` acquires the same lock and, before sending TERM or invoking
   `cgroup.kill`, appends and fsyncs a durable `kill-intent` record. The
   successful durable append is the kill linearization point. If the waiter
   already published a wait result whose reap preceded that point, `exited`
   stands and kill reports the race without changing the terminal record.
3. Otherwise the kill intent blocks the waiter from committing `exited`.
   `run-kill` performs the bounded grace and whole-scope `cgroup.kill`, then
   verifies the scope is empty. Only after both conditions does it acquire the
   terminal slot and append `killed` once.
4. If AIRA dies after kill intent but before terminal completion, reconcile
   resumes the intent and completes the kill/empty-scope proof, or records
   `lost`/`U_RUN_RECONCILE_REQUIRED` if that proof cannot be established. It
   never appends `killed` merely because intent exists.
5. Clause 3 governs an intent that has something to signal. If instead the kill
   finds the scope already empty **before any signal is sent** — the whole-scope
   membership read returns nothing, so the bounded grace and `cgroup.kill` are
   never performed — and the launch that published the intent holds kernel proof
   that the leader was already dead at that instant, then the intent is
   dispositioned **not-executed** and the waiter's wait result stands: the run
   commits `exited` with the child's real exit evidence, and the published
   intent is retained on the record as published-and-not-executed. It is never
   `completed`, and `killed` is never appended, because no signal was delivered
   and clause 4's prohibition is unchanged. Only the launch that itself created
   the intent may disposition it; an intent adopted from another actor falls
   under clause 3. Whether the child's exit fell before or after the deadline
   instant is unestablished and is asserted in neither direction, so no timeout
   error code is appended either (AIRA-126).

The lock/CAS and ledger uniqueness check reject a second terminal append as a
journal integrity failure. No path appends both `exited` and `killed`, and no
path fabricates an exit code for a kill or lost outcome — nor discards a real
one for a kill that provably never happened.

### 6.4 Stable code registration

The implementation plan must add at least these entries to
`internal/store.ExitCodes` and therefore to `core.ResponseContract()`:

| Family | Codes | Exit class |
|---|---|---:|
| Invocation | `E_RUN_ARGUMENT_INVALID`, `E_RUN_PREFIX_INVALID`, `E_RUN_CWD_INVALID`, `E_RUN_ENV_INVALID`, `E_RUN_STDIN_INVALID`, `E_RUN_NOT_FOUND` | 2 |
| Established operation failure | `E_RUN_FAILED`, `E_RUN_KILLED` | 1 |
| Capture/scope infrastructure | `E_RUN_OUTPUT_OPEN`, `E_RUN_OUTPUT_DISK_FULL`, `E_RUN_CAPTURE_FAILED`, `E_RUN_SCOPE_UNAVAILABLE`, `E_RUN_SCOPE_INVALID`, `E_RUN_SCOPE_HANDOFF`, `E_RUN_SCOPE_MIGRATION`, `E_RUN_DESCENDANT_KILLED`, `E_RUN_LAUNCH_FAILED` | 4 |
| Unevaluated/lost | `U_RUN_EXIT_UNKNOWN`, `U_RUN_OUTPUT_UNAVAILABLE`, `U_RUN_RECONCILE_REQUIRED` | 3 |

The `E_RUN_LAUNCH_FAILED` classification must distinguish “AIRA could not
start the requested argv” from a child that started and returned non-zero;
neither may be represented as success. Existing `E_DB_*`, `E_JOURNAL_*`,
`E_RECEIPT_IO`, and `E_INTERNAL` remain the infrastructure fallbacks for
store/ledger failures not specific to a runner condition.

## 7. Run data model and durability

### 7.1 Run record

The closed, versioned Run record is the protocol object and is authoritative in
the common-dir ledger:

| Field | Contract |
|---|---|
| `schema_version` | Versioned record format; unknown versions are not silently parsed. |
| `id` | Stable local `RUN-n` identity allocated by the run counter, not the git ticket allocator. |
| `argv` | Exact target token array, binary-safe as the transport permits. |
| `cwd` | Effective absolute/normalised launch directory, with no semantic substitution. |
| `env_digest` | Digest of the effective environment; values are not persisted in the run record. |
| `launch_prefix` | Exact effective prefix token array, including its configured identity but not a shell string. |
| `cgroup_scope` | Opaque scope reference/path, parent identity, and placement verification state. |
| `started_at`, `ended_at` | Wall-clock timestamps; lifecycle ordering comes from record sequence, not wall time. |
| `status` | Closed enum from §2.2. |
| `scope_integrity` | Closed result from §5.4: `contained`, `handoff-unverified`, `migrated`, or `descendant-killed`. |
| `exit_code` | Exact normal child code when established; absent otherwise. |
| `signal` | Terminating signal when independently established; absent for AIRA-killed/lost results without wait evidence. |
| `output_refs` | `out` + `err`, or `log`, and optional `in`; each has path/opaque ref, byte count, digest if complete, and capture state. |
| `capture_complete` | Explicit all-stream completion boolean; false/unknown is not a green result. |
| `capture_forced_closed` | Whether the descendant grace expired and scope kill was needed to reach pipe EOF; distinct from byte completeness. |
| `stdin_stored` | Whether `RUN-n.in` exists; false by default. |
| `scope_kill` | Requested/started/completed, grace outcome, and actor/at when applicable. |
| `kill_intent` | Durable intent sequence and completion/empty-scope proof; it arbitrates the terminal slot. `not_executed` (§6.3(5)) marks an intent that was published and then provably delivered no signal: an `exited` record carrying it means the deadline DID fire and killed nothing, not that no deadline was reached. |
| `error_codes` | Stable ordered codes and machine-readable error markers; no driver-string parsing by consumers. |
| `pid_identity` | Ephemeral launch PID plus process start identity while useful; never the sole kill authority. |

The record does not contain telemetry, rusage, a test report, gate verdict,
secret environment values, or an implicit “green” field.

### 7.2 Durability split

The decided M12 layout is:

- **Run ledger — common-dir audit class:** append-only, canonical records under
  the machine's git common-dir AIRA area. It contains `starting`/`running`,
  terminal lifecycle facts, capture error facts, scope refs, and record
  digests. It is the authority for reconstructing whether AIRA established an
  exit result. The local `RUN-n` counter reservation is lock-protected in this
  same machine-shared run area and is outside the git commit graph.
- **Run output — machine-local files:** a gitignored run directory, with
  `RUN-n.out`, `RUN-n.err`, `RUN-n.log`, and optional `RUN-n.in`. Raw bytes are
  not in git, the ledger, or SQLite. Output refs point to these files and
  retain completion/size/digest metadata.
- **SQLite — rebuildable projection:** indexed Run rows, current status
  queries, selector lookup, and output metadata. SQLite is not allowed to
  mint a terminal success or repair a missing ledger record.

This follows AIRA's content-truth/rebuildable-index layering for lifecycle
facts, while keeping high-volume bytes out of both content and DB. It is the
decided M12 exception to the top-level operational-telemetry default.

### 7.3 Write and crash protocol

The common-dir ledger uses canonical record framing mirroring AIRA's journal:

```
uvarint(payload_length) || canonical_payload || sha256(canonical_payload)
```

The length prefix is unambiguous, the payload has canonical field ordering,
and the checksum covers the complete payload. Every append is lock-protected,
written, fsynced, and followed by an fsync of the ledger directory. A torn
length/payload/checksum frame is not skipped: the ledger is corrupt and the
runner fails closed. The per-run terminal slot is unique in the ledger.

The foreground sequence is:

1. Validate argv, prefix, cwd, env, stdin, merge mode, and all four cgroup
   capabilities. Do not allocate a child or enter a fallback path on failure.
2. Reserve the next local `RUN-n` counter value and the intended unique scope
   reference. Append and fsync a `scope-reserved`/`starting` record **before
   creating a child**. Create output files and fsync their directory.
3. Create the private cgroup scope. Append and fsync `scope-created` with the
   actual scope reference **before `clone3`**. A crash after scope creation is
   therefore recoverable from a durable orphan-scope record, not an untracked
   live child.
4. Create the pipes and invoke `clone3` with `CLONE_INTO_CGROUP` and the scope
   fd. Record PID identity and append/fsync `running` only after membership and
   fd setup are verified.
5. Drain stdout/stderr (or the merged pipe) independently to files while a
   wait operation observes the launch process. Write capture error markers
   immediately and never overwrite already-recorded bytes.
6. Reap the launch process. Apply the finite descendant pipe grace (§4.5),
   close and fsync output files, and verify scope state. If kill is needed,
   use the terminal arbitration protocol (§6.3).
7. Append and fsync exactly one terminal `exited`, `killed`, or `lost` record
   with exit/signal, scope integrity, capture state, error codes, timestamps,
   and output refs. Fsync the ledger directory, then remove the verified-empty
   scope. Scope removal never substitutes for the terminal record.
8. Update/rebuild the SQLite projection from the ledger. A projection failure
   does not turn a factual terminal record into a fake success.

Crash and reconcile windows are explicit:

- after ID reservation or scope reservation: reconcile can finish/clean an
  empty orphan scope; a live scope is surfaced as killable by its run ID;
- after scope creation but before `clone3`: the durable scope-created record
  identifies the empty orphan for cleanup;
- after `clone3` but before `running`: the durable scope ref permits reconcile
  to find a live child; absent wait evidence is never exit 0;
- after child launch but before terminal record: reconcile finds a live scope
  and reports it killable, or an empty scope and records `lost`/scope
  integrity, but never infers exit 0;
- after terminal ledger append but before DB projection: replay the ledger;
- after DB loss: rebuild the projection from the ledger and output refs;
- after a torn/partial terminal frame: report `U_RUN_RECONCILE_REQUIRED`/
  `lost`, never green;
- after ledger loss or checksum corruption: fail closed with the existing
  journal/receipt infrastructure code. SQLite cannot repair or override the
  missing authoritative ledger.

Reconcile enumerates all durable scope references, not just DB rows. A live
scope whose run has no terminal record because the foreground parent died is
reported as an orphan/killable run; an operator uses `run-kill <run-id>` (or
the equivalent explicit reconcile kill action) to complete the cgroup kill.
Reconcile does not silently kill every orphan and does not mark one clean.
Once the scope is empty without a wait record, the run remains `lost` or has a
scope-integrity finding.

## 8. `run-log` protocol

### 8.1 CLI view and follow

`aira run-log <run-id>` views the captured output. The options are:

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
run ledger/status approximately every **200 ms** until terminal. It needs no
daemon and does not hold a child pipe. If the run becomes `lost`, the follow
ends with that status and a stable non-success result. If output is partial,
it says so. A follow reader ending does not close or influence the writer.
There is no follow timeout in M12: it polls until terminal or caller
cancellation, and cancellation does not alter the run.

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
   cgroup-v2 scope containing the launch process/target tree, or launch fails
   closed; the guarantee is bounded to that subtree.
2. **Kill is whole-scope.** `run-kill` never kills only the main PID and never
   names a shared prefix-owned slice.
3. **Exact capability set.** Unified cgroup-v2, delegated writable parent,
   kernel >=5.14, clone3+`CLONE_INTO_CGROUP`, and writable `cgroup.kill` are
   all required and individually probed before launch.
4. **No unsafe fallback exists.** M12 has no fork+move, process-group,
   main-PID, systemd-scope, or other fallback path.
5. **Bounded containment claim.** `setsid`, double-fork, and reparent do not
   escape; deliberate cgroup migration can. A detectable migration or
   unverifiable handoff is a scope-integrity finding and never clean success.
6. **Exact argv survives.** Target tokens, including `--`-prefixed tokens,
   reach the child without shell reparsing or option loss.
7. **cwd/env fidelity.** The child receives the selected cwd and effective
   environment; the record binds them with a digest and does not claim a
   different cwd/env.
8. **Length-prefixed env identity.** Duplicate keys are rejected and the
   sorted-key uvarint length/key/value encoding is hashed exactly; delimiters
   cannot create digest collisions.
9. **No prompt.** The runner never waits for an interactive approval or
   hidden stdin response.
10. **Every pipe drains.** Capture continues to files whether or not a live
   `run-log` reader exists; a reader cannot create child backpressure.
11. **Raw bytes remain raw.** Capture and MCP base64 transport do not corrupt,
   decode, trim, reorder, or text-normalise bytes.
12. **Merge means dup2.** Merged output comes from one exec-time kernel stream,
   never post-hoc file concatenation.
13. **Disk-full is visible.** Any `ENOSPC` or incomplete write/close/sync marks
    the run with a stable error; `capture_complete` requires EOF, successful
    close, and successful fsync.
14. **Descendant grace is finite.** A launch exit with lingering pipe holders
    waits only `run.capture_descendant_grace`, then whole-scope-kills and
    continues draining honestly; it never hangs on pipe EOF.
15. **Disk/capture errors are non-green.** Any incomplete capture marks the run
    with a stable error and prevents clean-run consumers from treating it as
    success.
16. **stdin is opt-in to storage.** Launch input may be supplied without being
    stored; input persistence requires explicit `--store-stdin`.
17. **Exit 0 is earned.** Only `exited` + established launch-process
    `exit_code=0` + contained scope + complete capture + complete terminal
    ledger can represent a clean run.
18. **Lost never becomes zero.** A missing wait, crashed capture protocol,
    removed scope without a wait record, or DB/ledger ambiguity is `lost`/
    unevaluated, never inferred success.
19. **One terminal winner.** A per-run lock/CAS, durable pre-signal kill
    intent, and one linearization rule permit exactly one terminal record;
    `exited` wins only if its wait was published before kill linearization,
    otherwise `killed` requires cgroup.kill completion plus empty scope.
20. **Terminal facts are immutable.** Reconcile can add findings or rebuild a
    projection, but cannot invent or rewrite an exit result.
21. **Ledger is durable authority.** Length/checksum framing, record fsync, and
    directory fsync protect the common-dir ledger; a torn terminal record is
    lost/unevaluated and ledger corruption fails closed.
22. **DB is not authority.** SQLite loss is rebuilt from the ledger; a DB row
    alone cannot mint a Run or repair a missing authoritative record.
23. **Orphans remain killable.** Durable scope refs are enumerated by
    reconcile; a parent-death live scope is surfaced by run ID for explicit
    `run-kill`, not silently abandoned or auto-marked clean.
24. **Scope handoff is visible.** A migrated/unknown descendant or an empty
    scope with unverifiable handoff produces a scope-integrity code/finding;
    AIRA reports only the launch process's wait status.
25. **Output truncation is explicit.** CLI and MCP identify partial ranges,
    caps, offsets, eviction, and read errors; an empty/short response never
    masquerades as complete output.
26. **No deferred feature leaks in.** M12 descriptors expose no detach,
    run-input, telemetry/gate auto-wiring, live supervisor tee, PTY, rusage,
    or report option.
27. **Faces cannot drift.** CLI, MCP, Skill, help, stable codes, and examples
    come from the same dispatch/response contracts.

## 11. Tests (TDD; every confirmed counterexample becomes a regression test)

### 11.1 Launch and argument fidelity

1. Bare prefix launches the exact argv and empty prefix means no wrapper.
2. The default is empty/bare; a configured `agentmux whale run --` prefix is
   used only when explicitly configured and is appended exactly once.
3. A configured prefix with shell metacharacters is passed as tokens or
   rejected; no shell expands it, and an arbitrary work-submitting prefix is
   outside the in-contract guarantee.
4. CLI parsing preserves a child argument beginning with `--` after the one
   launch delimiter and rejects missing/ambiguous delimiter forms.
5. MCP argv arrays and equivalent CLI argv construct byte-for-byte equivalent
   canonical requests.
6. Cwd is exact, a nonexistent cwd returns `E_RUN_CWD_INVALID`, and no
   project-root substitution occurs.
7. Inherited and explicit environment values reach the child; empty values
   survive; duplicate keys are rejected; the record contains only the agreed
   digest.
7a. The canonical environment encoder uses sorted-key
    `uvarint(len(key))||key||uvarint(len(value))||value` entries and SHA-256;
    environments containing LF, NUL, or `=` in test byte inputs have distinct
    digests and cannot collide.
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
16. Disk-full/ENOSPC handling drains/cleans without deadlock, requires
    successful close+fsync for `capture_complete`, kills the remaining scope
    when required, and never reports a fake exit 0.
16a. A launch process that exits while a descendant holds a capture fd waits
    only `run.capture_descendant_grace`, then whole-scope-kills, keeps draining
    to avoid deadlock, and records launch exit X plus the forced descendant
    outcome/capture state.
17. stdin is absent/closed by default; file and `-` sources work only at
    launch; `--store-stdin` alone is rejected; stored input is exact and
    explicitly referenced.

### 11.3 cgroup scope and kill

18. A run's cgroup is private, recorded, verified before `running`, and removed
    only after it is empty.
19. A configured in-contract wrapper plus child plus grandchild all appear
    inside the AIRA scope, including a child that calls `setsid`.
19a. A fixture that deliberately migrates a known member to another delegated
     cgroup produces `E_RUN_SCOPE_MIGRATION`; an empty scope with an
     unobservable handoff produces `E_RUN_SCOPE_HANDOFF`; neither is clean.
20. `run-kill` kills the whole scope, not just the main PID; descendants cannot
    keep running through the grace/final-kill race.
21. Killing an already exited run is idempotent and does not change an
    established terminal record; kill-versus-exit races have deterministic
    linearisation and no fake exit code.
22. PID reuse and a stale PID cannot make `run-kill` target an unrelated
    process; scope identity remains the authority.
23. Missing cgroup-v2 unified mount fails closed with
    `E_RUN_SCOPE_UNAVAILABLE` and no launch.
23a. A non-delegated/unwritable parent fails closed with the same code and no
     launch.
23b. Missing/denied `clone3` or `CLONE_INTO_CGROUP` fails closed with the same
     code and no launch.
23c. Kernel below 5.14 fails closed with the same code and no launch.
23d. Missing/denied writable `cgroup.kill` fails closed with the same code and
     no launch.
23e. Scope creation or membership verification failure fails closed with the
     same code and no launch; no process-group or PID fallback is attempted.
24. Scope cleanup failure is visible and does not make a complete run appear
    incomplete or vice versa.

### 11.4 Exit honesty and recovery

25. Child exit 0 records exact zero only after wait, full drain, output close,
    successful fsync, contained scope, and terminal ledger completion.
26. Child exit 7 records `status=exited`, `exit_code=7`, `E_RUN_FAILED`, and
    never collapses the child code to zero.
27. AIRA scope kill records `status=killed`, stable kill code, and no invented
    normal exit code.
28. Parent/capture/wait failure records `lost` or the explicitly established
    killed state; no missing result becomes `exited/0`.
28a. If waiter and `run-kill` race, the per-run lock/CAS and durable kill
     intent produce exactly one terminal record: pre-intent published wait
     wins; otherwise killed requires cgroup.kill plus empty-scope proof.
29. A running child after the foreground parent dies remains killable by its
    cgroup ref; once its scope empties without a wait record, reconcile marks
    the result lost rather than successful.
29a. Parent death after ID/scope reservation is reconciled as an empty orphan
     or recoverable scope reference, never as a run success.
29b. Parent death after scope creation but before clone3 is reconciled and
     cleans the empty scope without losing the durable reservation.
29c. Parent death after clone3 but before `running` leaves a durable live
     scope killable by run ID and no fabricated exit.
29d. Parent death during `running`/capture leaves the live scope killable and
     the eventual result lost unless a wait record exists.
29e. Parent death after wait but before the terminal append preserves the
     wait only if durably published; otherwise reconcile reports lost.
29f. Parent death after kill intent causes reconcile to complete the kill and
     empty-scope proof or report `U_RUN_RECONCILE_REQUIRED`, never infer killed.
30. A DB projection dropped after terminal ledger append rebuilds the exact
    Run and status from the ledger.
31. Every ledger frame has canonical length/checksum validation; a torn
    payload/checksum or partial terminal frame fails closed as
    `U_RUN_RECONCILE_REQUIRED`/lost, and each record plus its directory was
    fsynced before the next launch step.
32. A crash is injected at every lifecycle write boundary; reconcile repairs
    only the projection/documented derived side and emits stable findings for
    incomplete authority records.
33. A scope is reserved and durably recorded before clone3; an orphan scope
    after a crash is discoverable and recoverable from the ledger.
34. A malformed, reordered, or missing ledger record returns journal/reconcile
    failure or `U_RUN_RECONCILE_REQUIRED`, never a green record.
35. Output-file eviction/unreadability preserves metadata but returns explicit
    `U_RUN_OUTPUT_UNAVAILABLE` for output-dependent reads.

### 11.5 `run-log` and overflow

36. CLI view supports exact stream selection, byte offsets, tail/full policy,
    and binary output without metadata bytes contaminating stdout.
37. CLI follow observes appended data without a daemon, polls approximately
    every 200 ms until terminal, and reports lost/partial output honestly.
38. MCP output caps are deterministic; every clipped response includes
    `truncated`, total/returned bytes, next offset, and output state.
39. Repeated MCP offset requests reconstruct exactly the stored byte stream;
    an empty response is distinguishable from EOF and unavailable output.
40. MCP follow is bounded/cursor-based and never becomes an unbounded JSON
    response or hidden subscription.

### 11.6 Faces, descriptors, and static build

41. Dispatch metadata covers `run`, `run-kill`, and `run-log` with correct
    safety, examples, requiredness, and no Phase-5 args.
42. Generated MCP tool list contains exactly `aira_run`, `aira_run_output`,
    and `aira_run_kill` in addition to existing tools; no aliases leak.
43. CLI↔core↔MCP parity covers launch, merge, stdin, output cursor/follow, and
    kill arguments using real entrypoints.
44. Stable code/exit mapping appears identically in CLI, MCP, Skill, and guide
    response contracts; `U_*` exits 3 and never pass/0.
45. The ordinary foreground path works with no daemon and no network.
46. The static, no-cgo build remains valid; Linux cgroup support is isolated
    behind the platform boundary and unsupported platforms fail honestly.

## 12. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Prefix creates a nested/shared scope and kill reaches too little or too much | Empty/bare is the default; configured prefixes are contract-bound; AIRA creates/verifies its private parent and reports migration/handoff instead of claiming escaped work succeeded. |
| A descendant deliberately migrates out of the scope | `clone3` closes ordinary placement races, but the threat model is bounded; detectable migration and unverifiable handoff are stable scope-integrity findings, never clean success. |
| cgroup permissions vary across desktops/containers | Probe unified mount, delegated parent, kernel/clone3 placement, and writable `cgroup.kill` separately; any failure returns `E_RUN_SCOPE_UNAVAILABLE` with no launch. |
| A foreground parent dies while the child survives | No supervisor; durable pre-clone scope refs let reconcile surface the live scope as killable by run ID, while missing wait evidence remains `lost`. |
| Waiter and `run-kill` race | Per-run lock/CAS plus durable pre-signal kill intent gives one linearization point and exactly one terminal record. |
| A pipe reader blocks behind a live consumer | No child-facing live tee exists; drain workers persist independently, and a finite 2-second descendant grace leads to whole-scope kill and continued draining. |
| Separate streams are incorrectly merged later | `--merge` changes fd topology before exec; tests assert ordering and refs. |
| Disk-full produces partial but green output | `ENOSPC`/write errors are stable failures; `capture_complete` requires EOF, successful close, and fsync, and partial output is excluded from clean-run consumers. |
| Binary output is corrupted by text/MCP handling | Raw file bytes, CLI byte output, MCP base64 with offsets and counts. |
| stdin leaks secrets or hangs on a prompt | Closed/null stdin by default, launch-only explicit source, `store-stdin` opt-in, no prompt path. |
| DB loss invents or erases a run result | Common-dir framed/checksummed ledger is authoritative; projection replay rebuilds DB, while ledger loss/corruption fails closed and missing wait evidence becomes lost. |
| Ledger crash leaves an untracked scope | Reserve ID/scope ref and fsync the ledger before scope creation/clone3; reconcile enumerates durable orphan refs and both ledger/file directories are fsynced. |
| Environment delimiter collision changes identity | Sorted-key uvarint length/key/value encoding plus SHA-256 and duplicate-key rejection; collision fixtures cover LF/NUL/`=`. |
| Output retention makes `run-log` lie | Keep metadata and explicit eviction state; return `U_RUN_OUTPUT_UNAVAILABLE`, never an empty successful log. |
| CLI/MCP/Skill options drift into Phase 5 | One dispatch table, generated schemas/help, parity tests, and a descriptor test that rejects deferred options. |
| Child non-zero and AIRA failure exits are confused | Preserve exact launch-process `exit_code` in Run data and use resolved stable AIRA exit classes for the face. |

## 13. Resolved architect decisions applied

The following decisions are closed for this design and must not be reopened by
an implementation plan:

1. **Scope capability:** M12 requires a cgroup-v2 unified mount, delegated
   writable parent, kernel 5.14+, clone3 with `CLONE_INTO_CGROUP`, and writable
   `cgroup.kill`; each is probed independently and any failure hard-fails with
   `E_RUN_SCOPE_UNAVAILABLE`.
2. **Threat model and prefix:** the default prefix is empty/bare. `agentmux`
   and `whale` are optional configured prefixes; arbitrary work-submitting or
   migrating prefixes are out of contract. Containment and clean success are
   bounded to AIRA's owned subtree, with migration/handoff findings.
3. **Run IDs:** use a local `RUN-n` counter, not the git allocator.
4. **Environment identity:** use sorted-key length-prefixed uvarint encoding
   followed by SHA-256, rejecting duplicate keys.
5. **Lifecycle authority:** use the common-dir framed/checksummed ledger as
   authority, SQLite as a rebuildable projection, and machine-local capture
   files outside both.
6. **Ledger durability:** reserve the ID and durable scope reference before
   clone3; fsync every ledger record and its directory; torn/corrupt ledger
   data fails closed.
7. **Terminal arbitration:** a per-run lock/CAS and durable pre-signal
   kill-intent record provide one linearization point and exactly one terminal
   record.
8. **Supervisor ownership:** M12 has no supervisor. Parent death yields
   `lost`; reconcile enumerates the durable scope and exposes it for explicit
   `run-kill`.
9. **Descendant completion:** `run.capture_descendant_grace` is finite and
   defaults to 2 seconds; lingering pipe holders trigger whole-scope kill and
   honest forced-cleanup metadata.
10. **Capture durability:** `ENOSPC` and write/close/fsync errors fail the run;
    `capture_complete` requires EOF, successful close, and successful fsync.
11. **Follow and face exits:** `run-log --follow` polls about every 200 ms
    until terminal; CLI/MCP return AIRA exit classes while preserving the
    exact launch-process child exit code in Run data.
12. **Retention boundary:** M12 does not silently evict or compress capture
    bytes. Any external retention action must leave an explicit unavailable
    state and `U_RUN_OUTPUT_UNAVAILABLE`; active runs and follow readers are
    not candidates for external deletion.
