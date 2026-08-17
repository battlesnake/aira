# D6 — run-input (live stdin push to a detached run) — design / plan

**Status:** v5 — Sol plan-review 4 rounds → APPROVE-PLAN. Fable plan-gate pending.
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

### 2.1 The socket: location, BIND-BEFORE-START, discovery, auth
- **Path + sun_path pre-validation (Sol r1 #1, #6).** The runs output dir is under the git
  `CommonDir` (possibly long / networked, and can exceed the ~108-byte `sun_path` limit), so the
  socket does NOT live there. It reuses the **daemon's pinned path resolution** (`internal/daemon/
  paths.go` — which already handles `$XDG_RUNTIME_DIR` unset via a pinned per-user fallback under a
  stable state-id root, so a `Setsid`'d shim with no `XDG_RUNTIME_DIR` still resolves it):
  **`<runtime-root>/aira/<state-id>/inputs/<run-id>-<nonce>.sock`**, `0700` dir, `0600` socket. The
  **full `sun_path` length is validated BEFORE `Start`** (`E_RUN_INPUT_PATH_TOO_LONG` fails the launch
  cleanly — no child is created that would then wait on an unfeedable pipe).
- **Nonce-qualified path — no unlink-stale race (Sol r2 #4).** The path carries a fresh per-launch
  `<nonce>` (like the detach control file's nonce), so two supervisors NEVER share a socket path and
  the bind never unlinks a peer's reachable socket. The supervisor `bind`+`listen`+`chmod 0600` its
  own fresh path (no pre-unlink), and unlinks ONLY that exact path at its own teardown. A crashed
  supervisor leaves one nonce-stale file (harmless cruft; the client always uses the record's
  current `InputSocket`); a stale file is never mistaken for live and never unlinked after a mere
  failed dial (staleness is proven by the ledger's terminal/liveness state, §2.1 discovery, not by a
  dial). The client reads `InputSocket` from the record and dials exactly that path.
- **BIND BEFORE START (Sol r1 #1).** `bind`+`listen`+`chmod` happen **before `Start`**; only once the
  listener is up does the supervisor `Start` the child (fd0 = the pipe). `InputSocket` (with nonce)
  is written into the `starting`/`running` record so it is durable + observable.
- **Post-`Start` failure teardown — bounded, never blocks return (Sol r1 #1, r2 #5).** A single
  pre-`Start` `defer` runs the full teardown on every subsequent return path: close input plane, then
  **kill the child with a bounded, escalating policy** — `cgroup.kill` if the scope is usable, else a
  direct `SIGKILL` to the leader PID / process group (verified against `scope.Members()` where
  possible), with a **bounded reap wait**; if the reap cannot be proven complete it records
  `U_RUN_RECONCILE_REQUIRED` (honest, never claims a clean kill) and returns rather than blocking
  launch indefinitely — reusing M20's `forceDetachedQuiesce` honesty (never claims an unprovable
  descendant-kill). A child is never left alive on an unreachable pipe, and launch never hangs.
- **Discovery — classify from the ledger status FIRST, then dial (Sol r1 #6, r2 #6):** the client
  reads the record and decides in this order (using a **persisted `RunRecord.StdinConnect bool`
  independent of `InputSocket`** — Sol r3 #2 — so "connect-mode but not-yet-bound" is distinct from
  "not connect-mode"; a terminal run is never mislabeled unreachable):
  1. run **not connect-mode** (`StdinConnect == false`) → `E_RUN_INPUT_UNAVAILABLE`.
  2. run **terminal** (exited/killed/lost/cancelled) → `E_RUN_INPUT_CLOSED` (the run ended; input is
     moot — do NOT dial). A terminal run has no server to answer, so classifying it before dialing
     avoids a false `UNREACHABLE`.
  3. connect-mode but **no `InputSocket` yet** (starting / not yet `running`) → `E_RUN_INPUT_NOT_READY`.
  4. **`running` with `InputSocket`** → **dial**. Success → serve; server replies closed
     (post-`OP_CLOSE`) → `E_RUN_INPUT_CLOSED`; **dial fails** (nominally running but the supervisor's
     socket is unreachable — dead/gone) → `E_RUN_INPUT_UNREACHABLE` (suggest `reconcile`). No path
     silently drops bytes.
- **Auth (Sol r1 #7) — transport AND logical owner.** (a) Transport: `SO_PEERCRED` peer uid ==
  supervisor euid; `0700` dir + `0600` socket. (b) **Logical owner, mirroring `run-kill`:** the
  server enforces `CallerOwner` — a `run-input` from a foreign AIRA owner is refused
  (`E_RUN_INPUT_FOREIGN_OWNER`), overridable with `--steal`, exactly as `killPolicy{Enforce,
  CallerOwner}` + `ForeignOwnerError` (`runner_linux.go:1709`). The client passes its owner identity
  in the connect handshake; the D5 lease token is NOT used (it authenticates the supervisor to the
  daemon, not a client to the supervisor — wrong credential).

### 2.2 The child's fd0 pipe + the splice loop (supervisor side)
Order of operations when `req.StdinConnect` (replacing the file/`/dev/null` fd0 wiring at
`detach_linux.go:298-313`):
1. Validate `sun_path` length (§2.1) — else fail the launch before any child exists.
2. `inputR, inputW := os.Pipe()`; `cmd.Stdin = inputR`. Resolve the fresh nonce path and
   `bind`+`listen`+`chmod 0600` — **NO pre-unlink of any existing path (Sol r2 #4, r3)**; on the
   (astronomically unlikely) `EADDRINUSE`, regenerate the nonce and retry. The teardown `unlink`s
   ONLY this listener's own recorded path. **A single `defer` (installed here, before `Start`) tears
   the input plane + child down on EVERY subsequent return path** (Sol r1 #1, #2): close listener +
   active conn, close `inputW`, `unlink` this path; on a post-`Start` error also the bounded
   escalating kill+reap (§2.1).
3. `Start` the child. Close the parent's `inputR` copy (the child holds the dup'd read end).
4. A `running`-append failure (`detach_linux.go:337-344`) is now a full teardown+kill, not just a
   flagged running-failure — the child must not be left alive on an orphaned input plane.
5. Start the **input-serve goroutine** concurrent with the `cmd.Wait()` goroutine
   (`detach_linux.go:349-357`).

**Serve loop (Sol r1 #4, r2 #2) — dedicated acceptor + claim-slot-BEFORE-HELLO:** a dedicated
**acceptor goroutine** loops on `Accept()` (never blocked on a connection's I/O). For each accepted
connection it does an atomic **CAS to claim the single writer slot** *before* reading anything:
- **Claim fails** (another writer active, or input already `OP_CLOSE`d/terminal) → the rejection
  reply (`E_RUN_INPUT_BUSY` / `E_RUN_INPUT_CLOSED`) is written **under a short write deadline in a
  bounded reject worker** (best-effort), then the connection is closed — the **acceptor loop never
  blocks writing to a non-reading rejected peer** (Sol r3 #3), so a third connection still gets a
  prompt answer. At most ONE connection is ever being READ at a time (a slow/idle client cannot
  exhaust fds/goroutines), and a second writer never sits silently in the backlog.
- **Claim succeeds** → the acceptor hands the connection to the (single) active handler, which reads
  `OP_HELLO` under a **bounded HELLO deadline** (a stuck claimant is force-released on timeout →
  `E_RUN_INPUT_PROTOCOL`), runs the `CallerOwner` check, then reads bounded fully-read frames (§2.3),
  splicing DATA into `inputW` and ACKing committed bytes. `OP_CLOSE` closes `inputW` (child EOF) +
  marks input permanently closed. **The writer slot is released on EVERY handler exit** (auth
  failure, protocol error, HELLO timeout, `CloseWrite`, disconnect) so the next `run-input` can
  claim it; only `OP_CLOSE`/terminal makes the closed state sticky.

**Close the input plane after the LEADER exits, BEFORE `waitEmpty` (Sol r1 #2 — the descendant
deadlock).** `inputW` is closed **immediately after the leader's `cmd.Wait()` returns**, ahead of
`forceDetachedQuiesce`/`waitEmpty`. Otherwise a descendant that inherited fd0 and is blocked reading
would wait for EOF while the supervisor waits for an empty scope — a deadlock. Closing `inputW` right
after the leader exit lets such a descendant see EOF and drain, so quiescence can complete. The
listener + socket are also closed/unlinked at that point (no new input after the leader is gone).

**Teardown never wedges supervision.** The `cmd.Wait()` select (`detach_linux.go:358-388`) is
UNCHANGED and never blocks on input. A blocked splice-`Write` (child stopped reading, pipe full) is
confined to the input-serve goroutine and released by `inputW.Close()` (leader-exit or the
pre-`Start` defer). No terminal/kill/timeout path joins the splice before closing `inputW`.

### 2.3 The wire protocol (client ↔ supervisor) — ACKed committed bytes (Sol r1 #3)
Length-prefixed frames on the socket (D4 `writeRunnerAdmitFrame`/`io.ReadFull` idiom):
`[1-byte opcode][4-byte BE length][payload]`, **length validated against a max-frame cap BEFORE
allocation** (a `>cap` or a nonzero-length `OP_CLOSE` → `E_RUN_INPUT_PROTOCOL`, connection closed,
`inputW` untouched).
- `OP_HELLO` (owner identity) — the first frame; the server runs the `CallerOwner` check (§2.1).
- `OP_DATA` (raw stdin bytes, binary-safe): the server splices into `inputW`, then replies
  `OP_ACK{committed}` = the cumulative count of bytes it has DURABLY written to `inputW`. The client
  advances its offset by the ACK, so it always knows exactly how many bytes landed — never a blind
  retry.
- `OP_CLOSE` (length 0): closes `inputW` (child EOF), marks input closed; server replies `OP_ACK`
  then closes.
- **Request termination:** the client signals end-of-request with `CloseWrite` (half-close) on the
  connection; the server drains remaining frames, sends a final `OP_ACK`/status, then closes.

**Outcome codes (honest; NOT auto-retryable — Sol r2 #1).** A clean full ACK → success. A mid-stream
child read-end close (write to `inputW` returns `EPIPE`/`EBADF`) → `E_RUN_INPUT_CLOSED` **with the
committed byte count** so the caller knows the prefix that landed. A connection that drops before a
final ACK → `E_RUN_INPUT_OUTCOME_UNKNOWN` (the last ACK is the last *proven* committed prefix, but
bytes after it MAY have committed with the ACK lost). **A stdin byte stream is not idempotent**, so
the client does NOT auto-resume/retry — auto-resending "un-ACKed" bytes could DUPLICATE committed
bytes. `run-input` reports `OUTCOME_UNKNOWN` + the last proven committed count and exits non-zero;
resuming is an explicit operator decision, never automatic. (`E_RUN_INPUT_PARTIAL` is the
determinate case: the connection stayed up and the server reported some-but-not-all bytes committed
before a child-close — the committed count is exact.) v1 adds no request-id/offset dedup layer; that
(safe automatic resume) is a deferral (§6).

**Closing the CONNECTION is NOT EOF** — the supervisor keeps `inputW` open across connections; only
`OP_CLOSE` gives the child EOF. Sequential `run-input` calls append to one stdin stream in call
order.

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
  open until the leader exits, at which point teardown closes `inputW` (a child still reading sees
  EOF as the run ends). No implicit EOF on connection close.
- **Child-exit / kill mid-splice (Sol r1 #2, #3).** If the child (or a descendant holding fd0)
  closes its read end while a splice `Write` is in flight, the write returns `EPIPE`/`EBADF`; the
  serve loop maps this to `E_RUN_INPUT_CLOSED` (with the committed count) and stops — it never
  panics the supervisor or fakes success. A `run-input` racing an in-progress kill either lands on
  the still-open `inputW` (ACKed) or, once the leader has exited and `inputW` is closed, is refused
  `E_RUN_INPUT_CLOSED`/`E_RUN_INPUT_UNREACHABLE` — never a silent drop.

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
1. **Bind before Start; no orphan.** The socket is `sun_path`-validated + bound + listening BEFORE
   `Start`; a bind/path failure fails the launch with no child created. Every post-`Start` failure
   (incl. `running`-append) tears down input + kills + reaps the child — never leaves a child alive
   on an unreachable pipe.
2. **Close `inputW` after the leader exits, before `waitEmpty`.** A descendant inheriting fd0 sees
   EOF and drains, so quiescence never deadlocks on the input plane.
3. **No fabricated delivery; ACKed bytes.** Non-connect → `E_RUN_INPUT_UNAVAILABLE`; not-yet-running
   → `NOT_READY`; dead/stale → `UNREACHABLE`; post-`OP_CLOSE`/child-EPIPE → `CLOSED`; ambiguity →
   `PARTIAL`/`OUTCOME_UNKNOWN` with the committed count. Every accepted byte is ACKed only after it
   is durably written to `inputW`. Bytes are never silently dropped or success faked.
4. **Ordering + continuous-accept BUSY.** One atomically-claimed active writer; a second connection
   is accepted and IMMEDIATELY refused `E_RUN_INPUT_BUSY` (never left in the backlog); no interleave.
5. **Teardown never wedges supervision.** A blocked splice-`Write` (full pipe, child not reading) is
   confined to the input-serve goroutine and released by `inputW.Close()`; `cmd.Wait()` / kill /
   timeout paths are unchanged and never join the splice.
6. **No half-open; socket unlinked.** Listener + active conn + `inputW` close and the socket is
   unlinked by the pre-`Start` teardown defer on every return path; a child still reading sees EOF.
7. **Explicit EOF only.** Connection close ≠ EOF; only `OP_CLOSE` closes the child's fd0 write end.
8. **Auth — transport + owner.** `SO_PEERCRED` uid == supervisor euid; `0700` dir + `0600` socket;
   AND `CallerOwner` refusal (`E_RUN_INPUT_FOREIGN_OWNER`, `--steal` overrides), mirroring `run-kill`.
9. **Binary-safe.** DATA frames carry arbitrary bytes verbatim; frame length capped before alloc.
10. **Daemonless.** Identical behaviour daemon-up or down; the daemon is never on the input path.
11. **Backpressure is honest.** A slow/stopped child backs pressure up to the client's `Write`; the
   run is never killed for it, and input is never buffered unboundedly in the supervisor.

## 4. Tests (discriminators — must FAIL against the wrong impl; [[two-loop-porous-tests]])
- **Real-cgroup e2e (Opus HW):** `run --detach --stdin-connect cat` → `run-input RUN-n` streams
  "hello\n" → the child's `<id>.out` shows "hello" (proves bytes reach fd0); `run-input --close` →
  `cat` sees EOF and exits 0 (proves explicit EOF); a binary payload round-trips byte-identical.
- **Serial writer:** a second concurrent `run-input` while one is active → `E_RUN_INPUT_BUSY`, the
  first stream uncorrupted (discriminator: an interleaving impl fails).
- **Non-connect run:** `run-input` on a run launched WITHOUT `--stdin-connect` → `E_RUN_INPUT_
  UNAVAILABLE` (not a hang, not a fake success).
- **Descendant-EOF deadlock (Sol r1 #2, r2 #3, real-cgroup):** a DETERMINISTIC fork helper whose
  **leader exits while a descendant blocks on inherited fd0** — e.g. `python3 -c 'import os,sys;
  \nif os.fork():\n os._exit(0)\nsys.stdin.read()'` (parent/leader `os._exit(0)` immediately; the
  forked child keeps fd0 and blocks in `stdin.read()` until EOF). NOT `sh -c '... & cat'` (the shell
  leader waits for a foreground `cat`, so the leader does not exit first — that would false-pass).
  In connect mode: the run must still terminalize in bounded time because leader-exit closes
  `inputW` → the descendant EOFs and drains → `waitEmpty` completes. Discriminator: an impl that
  closes `inputW` only at terminal (after `waitEmpty`) deadlocks (bounded-time fail).
- **Bind-before-Start (Sol r1 #1):** a sun_path-too-long / bind failure fails the launch with NO
  child spawned (assert no ledger `running`, no orphan scope member).
- **Teardown:** kill a run mid-input (child not reading, pipe full, splice blocked) → the kill
  completes, the run terminalizes, no leaked goroutine/fd (goroutine-count + bounded-time assert);
  the socket file is unlinked. Discriminator: an impl that joins the splice before kill deadlocks.
- **ACK/partial (Sol r1 #3):** the client's ACK offset equals bytes written to fd0; a child that
  closes fd0 mid-stream → `E_RUN_INPUT_CLOSED` with the exact committed count (not a fake success).
- **EOF discipline:** stream, drop the connection WITHOUT `--close`, reconnect, stream more → the
  child receives the concatenation (proves connection-close ≠ EOF).
- **Continuous-accept BUSY (Sol r1 #4, r3 #3):** a second concurrent connect is answered
  `E_RUN_INPUT_BUSY` promptly (bounded-time), the first stream uncorrupted; AND with a rejected peer
  that NEVER READS its reply, a THIRD connection must still get a prompt answer (proves the acceptor
  doesn't block on a rejection write).
- **Discovery bit (Sol r3 #2):** a connect-mode run in `starting` (no `InputSocket` yet) →
  `E_RUN_INPUT_NOT_READY` (not `UNAVAILABLE`); a non-connect run → `UNAVAILABLE`; a terminal
  connect-mode run → `CLOSED` (classified from status without dialing).
- **Auth:** `SO_PEERCRED` reject a different uid (seam-injected); a foreign `CallerOwner` →
  `E_RUN_INPUT_FOREIGN_OWNER`, `--steal` overrides; socket mode `0600`.
- **Validation (unit):** `--stdin-connect` without `--detach`, or with `--stdin`/`--pty`/`--store-
  stdin` → `E_RUN_ARGUMENT_INVALID`. MCP over-cap base64 `data` → a stable argument error.
- **Framing (unit):** partial/one-byte-at-a-time frames reassemble (io.ReadFull); a `>cap` length
  or nonzero-length `OP_CLOSE` → `E_RUN_INPUT_PROTOCOL`, connection closed, `inputW` untouched.

## 5. Faces / plumbing
- `Request.StdinConnect bool`; `RunRecord.StdinConnect bool` (persisted, drives discovery §2.1
  independent of the socket path) + `RunRecord.InputSocket string` (omitempty). CLI
  `--stdin-connect` + the `run-input` verb across dispatch tables + generated help. `run-input` is
  `RouteClient` (carved), reads the record client-side, dials the socket, passes its owner in `OP_HELLO`.
- **CLI face:** `aira run-input RUN-n [--close] [--steal]` streams its own stdin as `OP_DATA`
  frames; `--close` sends `OP_CLOSE`; `--steal` overrides the `CallerOwner` refusal (mirrors
  `run-kill`).
- **MCP face (Sol r1 #5) — NOT stdin streaming:** an MCP invocation's stdin IS the MCP transport,
  so it cannot stream it. MCP `run-input` takes a **bounded base64 `data` arg** (≤ the max-frame
  cap) + optional `close bool` — a one-shot bounded write, not a stream. Larger-than-cap → a stable
  argument error. (Streaming stays CLI-only in v1.)
- **Codes** (+ `check.go` exit mapping): `E_RUN_INPUT_UNAVAILABLE` (not connect-mode) /
  `E_RUN_INPUT_NOT_READY` (not yet running) / `E_RUN_INPUT_UNREACHABLE` (dial/stale) /
  `E_RUN_INPUT_CLOSED` (OP_CLOSE'd or child EPIPE) / `E_RUN_INPUT_BUSY` (another active writer) /
  `E_RUN_INPUT_FOREIGN_OWNER` (owner refusal, `--steal` overrides) / `E_RUN_INPUT_PARTIAL` +
  `E_RUN_INPUT_OUTCOME_UNKNOWN` (delivery ambiguity, with committed count) / `E_RUN_INPUT_PROTOCOL`
  (bad frame) / `E_RUN_INPUT_PATH_TOO_LONG` (launch-time `sun_path`).
- Supervisor: the input-serve goroutine + `inputW` lifecycle in `detach_linux.go`
  launchDetachedValidated (a new `stdinConnect` branch: bind-before-Start, pre-Start teardown defer,
  close-inputW-after-leader-exit). Socket path resolution reuses `internal/daemon/paths.go`.
  Non-Linux stub parity (`runner_stub.go`).

## 6. Deferrals + honest coverage gaps
- **Live-input capture** (`--store-stdin` + `--stdin-connect`): rejected in v1; a growing-append
  `<id>.in` tee is a follow-on.
- **Concurrent interleaved multi-writer:** v1 is serial single-active; a merge/mux policy is deferred.
- **Daemon-routed input:** impossible without a new supervisor-push transport; v1 is direct + daemonless.
- **Foreground `run-input`:** foreground takes `--stdin` at launch; a live foreground channel is deferred.
- **Safe automatic resume** (request-id/offset/dedup so a lost-ACK `OUTCOME_UNKNOWN` can auto-resend
  without duplication) is deferred; v1 surfaces `OUTCOME_UNKNOWN` for an explicit operator decision.
  (Owner-scoped `CallerOwner` + `--steal` auth is NORMATIVE in v1, §2.1 — not deferred.)

## 7. Sol build-review checklist (seed)
1. Socket bound+listening BEFORE `Start`; bind/path failure spawns no child; every post-`Start`
   failure tears down input + kills + reaps (no orphan on an unreachable pipe).
2. Is `inputW` closed right after the LEADER `Wait` (before `waitEmpty`), so a descendant on fd0
   can't deadlock quiescence? Is it closed on EVERY return path (pre-`Start` teardown defer)?
3. Does any terminal/kill/timeout path block on the input goroutine or a full pipe? (must not).
4. ACK reports only durably-written bytes; child EPIPE → `CLOSED` with committed count; dropped
   connection → `OUTCOME_UNKNOWN`/`PARTIAL`; no blind-retry hint, no fake success.
5. Continuous accept + atomic single writer; a second connection promptly `E_RUN_INPUT_BUSY` (not
   stuck in backlog); frame length capped before alloc; nonzero `OP_CLOSE` rejected.
6. Connection-close distinct from `OP_CLOSE` (no implicit EOF).
7. Non-connect / not-ready / unreachable / closed reported with stable codes, never a fake success.
8. `SO_PEERCRED` same-uid AND `CallerOwner` refusal (`--steal` override); socket `0600` in `0700`;
   binary-safe DATA; MCP is bounded base64 (never reads MCP stdin).
9. Daemon-up vs daemon-down identical (daemon never on the path).
