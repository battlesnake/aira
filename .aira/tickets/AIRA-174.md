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
