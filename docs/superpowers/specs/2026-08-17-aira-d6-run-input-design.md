# D6 — run-input (live stdin push to a detached run) — design / plan

**Status:** v1 — DRAFT for Sol plan-review.
**Milestone:** Phase 5 · D6 (task #42). Follows D1–D5 (all merged; master `f8c4412`).
**Loop:** plan → Sol plan-review (rounds → APPROVE-PLAN) → Fable plan-gate → Terra build →
Opus real-HW verify → Sol build-review (rounds → RESOLVED) → merge. Correctness-critical (a live
fd/bytes channel + backpressure + teardown races): the two-loop adversarial process is mandatory.

---

## 1. Goal, scope, and the honest boundary

### 1.1 Goal
Deliver `aira run-input` — a control plane to stream stdin **bytes** to an already-running
**detached** subprocess, as deferred by spec §14 line 156 ("aira run-input (live stdin push) …
requires a per-run control plane (a socket/FIFO on the shim) … lands once the shim exists"). Today
a detached child's fd0 is an opened `--stdin` file or `/dev/null` — never a pipe — and there is **no
bidirectional client→supervisor channel** (kill is delivered by the kernel `cgroup.kill` + a
ledger-polled kill-intent flag, which cannot carry bytes). D6 adds the missing per-run channel.

### 1.2 Design shape (forced by the spec + code)
- The channel is a **per-run Unix socket hosted BY the supervisor shim**, reached **directly** by
  the `run-input` client — **daemonless** (spec §14: detach must work with no daemon; the daemon
  has no supervisor-push transport anyway — it is strictly supervisor→daemon).
- A detached run must **opt in at launch** (`aira run --detach --stdin-connect`): its child's fd0
  becomes a **pipe** whose write end the supervisor retains, and the supervisor hosts the socket +
  a splice loop that copies incoming bytes into that pipe. A run NOT launched in connect mode has
  fd0 wired to a file/`/dev/null` and cannot be fed (`run-input` → `E_RUN_INPUT_UNAVAILABLE`).
- `aira run-input RUN-n` (a new `run-*` verb → `RouteClient`, client-side) streams its own stdin to
  the run's socket; `--close` sends EOF (closes the child's fd0 write end).

### 1.3 v1 scope (tight — YAGNI; spec says "no dated pain motivates it")
IN: the `--stdin-connect` launch mode (fd0 pipe + supervisor-hosted socket + splice loop);
`run-input`/`run-input --close`; same-uid auth; serial single-active writer; teardown on
terminal/kill; backpressure that never wedges supervision.

OUT (explicit deferrals, §6): live-stream capture to `<id>.in` (`--store-stdin` stays launch-time
only); concurrent interleaved multi-writer; daemon-routed input; foreground `run-input` (foreground
already takes `--stdin` at launch); interactive TTY/approval prompts (spec §14 line 157, out of scope).

---

## 2. Design

### 2.1 The socket: location, creation, discovery, auth
- **Path.** The runs output dir lives under the git `CommonDir` (possibly long / networked), which
  can exceed the ~108-byte `sun_path` limit, so the input socket does NOT live there. It mirrors the
  daemon's scheme: `$XDG_RUNTIME_DIR/aira/<state-id>/inputs/<run-id>.sock`, in a `0700` dir, socket
  `0600`. The **supervisor** creates it (bind+listen) after `Start`, before the `running` event;
  the resolved absolute path is recorded in the `running` `RunRecord` as a new field
  `InputSocket string` (json omitempty). The **client** reads the record (`show RUN-n`, carved) →
  `InputSocket` → dials it. Recording the path (vs re-deriving) avoids a derivation mismatch and
  makes "connect mode was requested" observable.
- **Auth (machine-local single-user threat model, per D5 §2.1).** `SO_PEERCRED`: the peer uid must
  equal the supervisor's euid; the `0700` dir + `0600` socket already restrict to the owner. Same-user
  input-injection is out of the threat model (a same-user process can already `cgroup.kill` the run
  or write the ledger). Owner-scoping like `run-kill`'s `CallerOwner` is noted as a possible
  addition but not required in v1; stated, not hidden.
- **Absence/stale.** If the run was not launched `--stdin-connect`, `InputSocket` is empty →
  `run-input` fails `E_RUN_INPUT_UNAVAILABLE` (no fabricated success). If the socket exists but the
  dial fails (supervisor dead/gone), `run-input` reports `E_RUN_INPUT_UNREACHABLE` and suggests
  `reconcile` — never silently drops the bytes.

### 2.2 The child's fd0 pipe + the splice loop (supervisor side)
At launch, when `req.StdinConnect`, instead of `setupStdin` wiring fd0 to a file/`/dev/null`
(`detach_linux.go:298-313`), the supervisor:
1. Creates `inputR, inputW := os.Pipe()`; `cmd.Stdin = inputR`; retains `inputW`.
2. After the `running` event + `closeFiles()`, closes its copy of `inputR` (the child holds the
   dup'd read end) and starts a dedicated **input-serve goroutine** concurrent with the existing
   `cmd.Wait()` goroutine (`detach_linux.go:349-357`).
3. The input-serve goroutine `Accept()`s on the socket, **SERIALLY** (one active connection at a
   time; a second concurrent dial gets `E_RUN_INPUT_BUSY` and closes — no interleaved bytes). For
   the active connection it reads framed input (§2.3) and `io.Copy`s DATA bytes into `inputW`; a
   CLOSE frame closes `inputW` (child sees EOF) and marks input permanently closed (subsequent
   connects → `E_RUN_INPUT_CLOSED`).

**Teardown (the correctness crux — must not wedge supervision or leak the child).** The input-serve
goroutine and `inputW` are owned by a `context` cancelled the instant the run reaches ANY terminal
path (normal exit, timeout kill, forced quiesce, cancelled). On cancel: close the listener (unblocks
`Accept`), close the active conn, and close `inputW` (so a child still reading fd0 sees EOF rather
than a half-open pipe). The `cmd.Wait()` select (`detach_linux.go:358-388`) is UNCHANGED and never
blocks on the input path; a blocked splice-`Write` (child stopped reading, pipe full) is confined to
the input-serve goroutine and is released by the terminal `inputW.Close()`. The socket file is
`unlink`ed at terminal.

### 2.3 The wire protocol (client ↔ supervisor)
A minimal length-prefixed frame on the socket (mirrors the D4 admit framing idiom
`writeRunnerAdmitFrame`): `[1-byte opcode][4-byte BE length][payload]`.
- `OP_DATA` (payload = raw stdin bytes, binary-safe): spliced into `inputW`.
- `OP_CLOSE` (length 0): closes `inputW` (child EOF), marks input closed.
- Response: a single trailing status frame per connection — `OK`, or a stable code
  (`E_RUN_INPUT_BUSY` / `E_RUN_INPUT_CLOSED` / a write error if the child's read end vanished).
The client streams its own stdin as `OP_DATA` frames (bounded chunk size), then either leaves the
connection (stream paused; more `run-input` calls may follow) or, with `--close`, sends `OP_CLOSE`.
**Closing the CONNECTION is NOT EOF** — the supervisor keeps `inputW` open across connections;
only `OP_CLOSE` gives the child EOF. This lets multiple sequential `run-input` calls append to one
stdin stream.

### 2.4 Backpressure, ordering, EOF (honest semantics)
- **Ordering.** Serial single-active writer (§2.2) ⇒ bytes reach fd0 in exactly the order the active
  connection sent them; no interleaving. Concurrent writers are refused (`E_RUN_INPUT_BUSY`), not
  merged. Sequential `run-input` calls append in call order.
- **Backpressure.** The child's fd0 is a real pipe (64 KiB kernel buffer). If the child stops
  reading, the pipe fills and the supervisor's splice `Write` blocks → the supervisor stops reading
  the socket → the client's `Write` blocks. This is **correct end-to-end backpressure to the
  client** (contrast the OUTPUT side, which is deliberately non-blocking "always drains", §14 l.149;
  input has the opposite, intended hazard). It never blocks supervision (§2.2 teardown).
- **EOF.** Explicit only (`--close` → `OP_CLOSE`). A run that never receives `OP_CLOSE` keeps fd0
  open until terminal, at which point teardown closes `inputW` (the child, if still reading, sees
  EOF as the run ends). No implicit EOF on connection close.

### 2.5 Launch validation + the new mode
- New `Request.StdinConnect bool` (CLI `--stdin-connect`, detach-only). Reconcile with the existing
  guards (`runner_linux.go:207-209`, `:259-266`): `--stdin-connect` is **mutually exclusive** with
  `--stdin <file>` / `--stdin -` / `--pty` / `--no-stdin` (one stdin source), and requires
  `--detach` (foreground already streams `--stdin` at launch; `E_RUN_ARGUMENT_INVALID` otherwise).
- `--store-stdin` + `--stdin-connect` → rejected in v1 (`E_RUN_ARGUMENT_INVALID`, "live-input
  capture is not yet supported") — capture of a live stream is a deferral (§6), not silently dropped.

### 2.6 Daemonless floor + the daemon's (non-)role
`run-input` is entirely client↔supervisor over the per-run socket; the **daemon is not involved**
(it has no supervisor-push channel, and `run-*` is `RouteClient`). So `run-input` works exactly the
same daemon-up or daemon-down — it does not regress the daemonless guarantee that kill/launch keep.
The D5 supervisor lease is orthogonal (liveness evidence), not a prerequisite for input.

---

## 3. Invariants (Sol plan-review + build-review check both directions)
1. **No fabricated delivery.** `run-input` to a non-connect run → `E_RUN_INPUT_UNAVAILABLE`; to a
   dead/gone supervisor → `E_RUN_INPUT_UNREACHABLE`; after `OP_CLOSE` → `E_RUN_INPUT_CLOSED`. Bytes
   are never silently dropped or a success faked.
2. **Ordering.** Serial single-active writer; concurrent connect → `E_RUN_INPUT_BUSY`; no interleave.
3. **Teardown never wedges supervision.** A blocked splice-`Write` (full pipe, child not reading) is
   confined to the input-serve goroutine and released by terminal `inputW.Close()`; `cmd.Wait()` /
   kill / timeout paths are unchanged and never block on input.
4. **No orphan / no half-open.** At every terminal path the listener + active conn + `inputW` close
   and the socket file is unlinked; a child still reading fd0 sees EOF, not a wedged pipe.
5. **Explicit EOF only.** Connection close ≠ EOF; only `OP_CLOSE` closes the child's fd0 write end.
6. **Auth.** `SO_PEERCRED` uid == supervisor euid; `0700` dir + `0600` socket; same-user only.
7. **Binary-safe.** DATA frames carry arbitrary bytes verbatim (no text assumptions).
8. **Daemonless.** Identical behaviour daemon-up or down; the daemon is never on the input path.
9. **Backpressure is honest.** A slow/stopped child backs pressure up to the client's `Write`; the
   run is never killed for it, and input is never buffered unboundedly in the supervisor.

## 4. Tests (discriminators — must FAIL against the wrong impl; [[two-loop-porous-tests]])
- **Real-cgroup e2e (Opus HW):** `run --detach --stdin-connect cat` → `run-input RUN-n` streams
  "hello\n" → the child's `<id>.out` shows "hello" (proves bytes reach fd0); `run-input --close` →
  `cat` sees EOF and exits 0 (proves explicit EOF); a binary payload round-trips byte-identical.
- **Serial writer:** a second concurrent `run-input` while one is active → `E_RUN_INPUT_BUSY`, the
  first stream uncorrupted (discriminator: an interleaving impl fails).
- **Non-connect run:** `run-input` on a run launched WITHOUT `--stdin-connect` → `E_RUN_INPUT_
  UNAVAILABLE` (not a hang, not a fake success).
- **Teardown:** kill a run mid-input (child not reading, pipe full, splice blocked) → the kill
  completes, the run terminalizes, no leaked goroutine/fd (goroutine-count + a bounded-time assert);
  the socket file is unlinked. Discriminator: an impl that joins the splice before kill deadlocks.
- **EOF discipline:** stream, drop the connection WITHOUT `--close`, reconnect, stream more → the
  child receives the concatenation (proves connection-close ≠ EOF).
- **Auth:** `SO_PEERCRED` reject a different uid (seam-injected); socket mode is `0600`.
- **Validation (unit):** `--stdin-connect` without `--detach`, or with `--stdin`/`--pty`/`--store-
  stdin` → `E_RUN_ARGUMENT_INVALID`.
- **Framing (unit):** partial/one-byte-at-a-time DATA/CLOSE frames reassemble (io.ReadFull idiom);
  a malformed opcode → a stable error, connection closed, `inputW` untouched.

## 5. Faces / plumbing
- `Request.StdinConnect bool`; `RunRecord.InputSocket string` (omitempty). CLI `--stdin-connect` +
  the `run-input` verb (`run-input RUN-n [--close]`) across CLI + MCP + dispatch tables + generated
  help. `run-input` is `RouteClient` (carved), reads the record client-side, dials the socket.
- New codes: `E_RUN_INPUT_UNAVAILABLE` / `E_RUN_INPUT_UNREACHABLE` / `E_RUN_INPUT_CLOSED` /
  `E_RUN_INPUT_BUSY` (+ exit-code mapping in `check.go`).
- Supervisor: the input-serve goroutine + `inputW` lifecycle in `detach_linux.go`
  launchDetachedValidated (a new `stdinConnect` branch at the fd-wiring site, cancelled at every
  terminal path). Non-Linux stub parity (`runner_stub.go`).

## 6. Deferrals + honest coverage gaps
- **Live-input capture** (`--store-stdin` + `--stdin-connect`): rejected in v1; a growing-append
  `<id>.in` tee is a follow-on.
- **Concurrent interleaved multi-writer:** v1 is serial single-active; a merge/mux policy is deferred.
- **Daemon-routed input:** impossible without a new supervisor-push transport; v1 is direct + daemonless.
- **Foreground `run-input`:** foreground takes `--stdin` at launch; a live foreground channel is deferred.
- **Owner-scoped auth** (like kill's `CallerOwner` + `--steal`): v1 is same-uid; noted.

## 7. Sol build-review checklist (seed)
1. Does any terminal/kill/timeout path block on the input goroutine or a full pipe? (must not).
2. Is `inputW` closed on EVERY terminal path (no half-open pipe, no leaked fd/goroutine)?
3. Is connection-close distinct from `OP_CLOSE` (no implicit EOF)?
4. Is the writer genuinely serial (no interleave; `E_RUN_INPUT_BUSY` on concurrent connect)?
5. Are non-connect / unreachable / closed runs reported with stable codes, never a fake success?
6. `SO_PEERCRED` same-uid enforced; socket `0600` in a `0700` dir; binary-safe DATA.
7. Daemon-up vs daemon-down identical (daemon never on the path).
