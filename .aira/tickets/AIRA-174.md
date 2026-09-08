---
{"schema":1,"id":"AIRA-174","project":"aira","title":"run-input HELLO read side still reports E_RUN_INPUT_OUTCOME_UNKNOWN for a peer that closed without a frame before any DATA was sent","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["runner"],"hold":false,"relations":[]}
---
Found by the AIRA-173 build-review (Fable, PR #107, merged `7401e8d`). The ID was
allocated by `aira id`; the file is hand-written because the installed binary
predates AIRA-170/171 (see RANT-25).

AIRA-173 fixed the WRITE side of the HELLO handshake: a failed HELLO write is now
classified by reading the refusal the server already queued, and a peer that
closed without a frame is reported `E_RUN_INPUT_UNREACHABLE` with committed 0 —
because no DATA frame has been sent at HELLO time, so no byte can have reached
the child's stdin and there is no delivery ambiguity for `OUTCOME_UNKNOWN` to
describe (D6 design §2.3).

The READ side of the same handshake still has the pre-fix shape. In
`connectRunInput` (`internal/runner/run_input_linux.go`) a HELLO write that
succeeds is followed by `readRunInputResponse(conn, 0)`; on any read error — EOF
from a peer that closed without a frame, e.g. `closeTerminal` racing the
handshake, or the accept loop's frame-less close when the plane went terminal
under the accept — `readRunInputResponse` returns `E_RUN_INPUT_OUTCOME_UNKNOWN`
with `Committed` 0. By AIRA-173's own reasoning that is the same fabricated
unknown: nothing has been sent, the outcome is known-empty, and the design's code
for a dead/gone socket is `E_RUN_INPUT_UNREACHABLE`.

Not on the AIRA-173 flake path: that race lands on the write, or on the read as a
*decoded* BUSY frame, which is handled. Deliberately not widened in PR #107.

## Expected fix

At HELLO time only, classify a frame-less close on the read as
`E_RUN_INPUT_UNREACHABLE` committed 0. A decoded refusal frame keeps the server's
own code, and a zero-committed BUSY keeps the bounded retry. Regression test
mirroring `TestRunInputClientReportsUnreachableWhenHelloWriteFindsNoRefusal`
(`internal/runner/run_input_hello_race_linux_test.go`) with a peer that READS the
HELLO and then closes without answering.

The mid-stream and CLOSE-time `OUTCOME_UNKNOWN` paths are correct and must stay:
once a DATA frame has been sent the ambiguity is real.

---

# Resolution (2026-09-08)

## 1. Mechanism, read from source

`connectRunInput` (`run_input_linux.go`) calls `readRunInputResponse(conn, 0)`
after a successful HELLO write. That helper wraps **any** `readRunInputFrame`
failure as `E_RUN_INPUT_OUTCOME_UNKNOWN` with the last committed count — 0 here.
The server has three ways to leave a HELLO unanswered with no frame at all:

- `acceptLoop`'s post-CAS recheck closes bare when the plane went terminal under
  the accept (`run_input_server_linux.go:148-151`);
- `reject` closes bare when no reject slot is free (`:168-170`);
- `closeTerminal` closes the claimed connection mid-handshake (`:270-283`).

None of them is on the AIRA-173 flake's path (that race lands on the write, or on
the read as a *decoded* BUSY frame), which is why PR #107 left this alone. The
defect is the same one AIRA-173 fixed one step earlier: at HELLO time no DATA
frame has been sent, so no byte can have reached the child's stdin and the
committed count is known to be 0. `OUTCOME_UNKNOWN` is defined as the delivery
ambiguity of bytes that may or may not have landed (D6 §2.4); asserting it here
fabricates an unknown out of a known-empty outcome. The pre-fix error string said
so itself, and in the malformed-frame case said it twice over:
`E_RUN_INPUT_OUTCOME_UNKNOWN: E_RUN_INPUT_PROTOCOL: frame exceeds maximum size
(committed=0)`.

## 2. Fix

`readRunInputResponse` is split into a shared `interpretRunInputResponse` (a
frame that ARRIVED → ACK count, or the server's own refusal code) and two thin
readers that differ only in how they classify a frame that never arrived:

- `readRunInputHelloResponse` — transport failure → `E_RUN_INPUT_UNREACHABLE`
  committed 0. New; used only by `connectRunInput`.
- `readRunInputResponse` — unchanged `E_RUN_INPUT_OUTCOME_UNKNOWN` with the last
  proven committed count. Still used by the mid-stream ACK read, the CLOSE ACK
  read, and the final ACK read, where the ambiguity is real.

The server is unchanged, and so is every decoded-frame path: a zero-committed
BUSY on the HELLO read still re-enters the existing bounded retry, and every
other refusal (`CLOSED`, `FOREIGN_OWNER`, …) is still terminal on the first
attempt. `UNREACHABLE` is not retried, so an unexplained close costs exactly one
dial.

**Two deliberate decisions beyond the ticket's literal wording**, both recorded
rather than slipped in:

1. **Not only a frame-less *close*.** Any transport failure on the HELLO read is
   classified `UNREACHABLE`, including the handshake-deadline timeout against a
   peer that accepts and then stays silent. The reasoning is identical (nothing
   was sent, so nothing can be ambiguous) and it is the true mirror of the
   write side, which likewise does not sort write failures by kind. The
   alternative — an error-taxonomy allowlist that keeps `OUTCOME_UNKNOWN` for
   timeouts — would preserve a fabricated unknown *and* add a branch, against
   the standing simplicity rule. This changes one existing expectation:
   `TestRunInputClientSilentServerHandshakeDeadlineFires` now asserts
   `E_RUN_INPUT_UNREACHABLE` committed 0 instead of `E_RUN_INPUT_OUTCOME_UNKNOWN`.
   Its load-bearing assertions — that the deadline actually fires and the client
   returns bounded — are untouched, so it still fails if the timeout stops
   firing. **Operator-visible consequence:** that case's exit code moves 3 →
   4 (`internal/codes/codes.go:280-282`), which is the accurate bucket for a
   dead or wedged socket.
2. **`UNREACHABLE` does not swallow a determinate verdict.**
   `readRunInputFrame` returns its own typed `E_RUN_INPUT_PROTOCOL` for an
   oversized or malformed frame; the peer demonstrably answered, so that keeps
   its code. Without this the fix would trade one wrong code for another in that
   sub-case. `classifyRunInputHelloWriteError` deliberately keeps its simpler
   shape: there the primary failure is already established (the write got EPIPE,
   so the peer IS gone) and the frame read is only a speculative attempt to
   explain it, so undecodable bytes correctly fall back to `UNREACHABLE` with the
   write error. It is not the same situation and not an asymmetry to fix.

## 3. Verification

TDD: both new tests were written first and watched fail against the unmodified
client — `E_RUN_INPUT_OUTCOME_UNKNOWN` in both, which is the exact defect.

- `TestRunInputClientReportsUnreachableWhenHelloReadFindsNoFrame` — the mirror of
  `TestRunInputClientReportsUnreachableWhenHelloWriteFindsNoRefusal`. Its peer is
  a real `socketpair` that READS the HELLO and then closes without answering, so
  the write provably succeeds and the failure lands on the read; scheduling plays
  no part. The test asserts the peer consumed the HELLO, which is the
  discriminator against silently re-testing the write side.
- `TestRunInputClientKeepsTheProtocolVerdictOnAMalformedHelloAnswer` — pins
  decision 2 above, so the new `UNREACHABLE` branch cannot widen unnoticed.

Untouched paths that keep the fix from degrading into "everything at HELLO is
unreachable", all still green: `TestRunInputClientRetriesTransientBusyOnceWithoutResending`
(a BUSY frame on the read is still retried into a full send),
`TestRunInputClientNonBusyHelloErrorNotRetried` (a FOREIGN_OWNER frame is still
terminal), `TestRunInputClientDroppedBeforeFinalACKIsOutcomeUnknownWithoutRetry`
(the mid-stream ambiguity is still `OUTCOME_UNKNOWN` with committed 4), and the
four AIRA-173 write-side tests.

Those ten tests also ran `-count=20 -race` under `aira confine`: `ok`, exit 0.

Gate, exact exit codes, all under `aira confine`:
`go build ./...` **0**; `go vet ./...` **0**;
`AIRA_REAL_CGROUP=1 go test ./... -count=1` **0** (zero `FAIL` lines;
`internal/runner` ok 149.2s; log kept at `~/tmp/aira174-fullsuite.log`).
`gofmt -l` on the three changed files is empty.

## 4. Accepted gaps

1. **No real-server test for the frame-less close.** AIRA-173 could pin its
   server-side dependency (a refusal survives the close) because a busy plane
   refuses deterministically. The three bare-close paths here are not
   deterministically constructible: two need a genuine race window inside
   `acceptLoop`, and the third needs eight concurrently stuck reject slots
   against a server that writes and releases immediately. A scheduling-dependent
   test for them would be a new flake, which is what this pair of tickets exists
   to remove. The paths are cited from source above and in the code comment; the
   fix depends on **no** server property, only on the classification of a read
   failure the client already observes — unlike AIRA-173, whose fix did.
2. **No frequency measurement, and none is claimed.** Unlike AIRA-173 this was
   not filed from an observed flake; it is a code-honesty defect found by
   reading, so there is no before/after rate to report and none is implied.
3. The darwin/windows cross-build finding recorded on AIRA-173 is pre-existing
   and unrelated; still not fixed here.
