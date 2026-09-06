---
{"schema":1,"id":"AIRA-118","project":"aira","title":"TestM20LauncherDefersACKAndBoundsReadiness/handle_before_ack flakes under machine saturation — 1s Eventually budget for a subprocess ACK","status":"in-review","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["flake","runner"],"hold":false,"relations":[]}
---
Observed 2026-09-06 in the `pre-push` `make ci` gate for AIRA-73's PR (a change that touches ZERO files in internal/runner).

`internal/runner/detach_linux_test.go:313` — the subtest waits with
`testdeadline.Eventually(t, time.Second, ...)` for a fake supervisor subprocess to
write its ACK file, then asserts the contents. On timeout `ack` is still nil, so the
failure surfaces as the confusing `ack=""` rather than as a deadline message.

Evidence it is a wall-clock flake, not a regression:

- The failing run executed under extreme saturation: its own confine trailer reports
  `admission=waited`, peak RSS at 100% of the reserved cap, with 13 queued waiters
  and the slice held exclusively by another session's job.
- The same tree passed `go test ./...` with exit 0 three times (including twice after
  rebases) under normal load.
- `go test ./internal/runner -run TestM20LauncherDefersACKAndBoundsReadiness -count=5`
  passes 5/5 in isolation.
- The AIRA-73 diff touches no file under internal/runner.

This is the class AIRA-20 has been hardening (wall-clock-tight test deadlines) and is
the kind of flake that blocks -race CI re-add. Two candidate fixes: scale the budget
the way AIRA-20's other backstops were scaled, and make the assertion report the
deadline rather than an empty string so the next occurrence is self-describing.

## Resolution (2026-09-06, branch `aira118-launcher-ack-flake`)

The report's own diagnosis was wrong in an instructive way, and chasing only the
timeout would have left the actual defect in place.

### It was not the deadline — it was a lost race inside the test harness

`ack=""` is reported at the CONTENT check (`detach_linux_test.go:313`), and the
deadline path cannot reach that line: `testdeadline.Eventually` fails through
`tb.Fatalf`, which calls `runtime.Goexit`, so a missed deadline ends the subtest at
the `Eventually` call with `fake supervisor did not observe ACK` and never runs the
content check at all. Reaching `ack=""` therefore requires `os.ReadFile` to have
SUCCEEDED and returned zero bytes.

It could, and here is how. The fake supervisor published its ACK observation with
`os.WriteFile(path, []byte(value), 0o600)`. That opens `O_CREATE|O_TRUNC` and only
then writes, so between those two syscalls the destination file EXISTS and is EMPTY.
The launcher-side assertion polls that path every 2ms (`testdeadline.PollInterval`)
and stops at its first successful read. A reader that lands in the window reads `""`
and the subtest fails with exactly the reported message. Saturation is what makes the
window wide — a child descheduled between open and write under memory-pressure stall
holds it open for as long as it is stalled — which is why the flake showed up on a
box at `admission=waited` with 13 queued waiters and never in isolation. This is a
genuine, if test-only, atomicity bug, not a wall-clock budget being a hair too small.

Reproduced deterministically before fixing (window held open by expanding
`os.WriteFile` into its create/write pair with a 200ms sleep between them): FAILED
3 runs out of 3, `detach_linux_test.go: ack=""` — the reported symptom, verbatim.

### What changed

1. **The publish is now atomic** (`publishM20Ack`): the value is staged in a
   dot-prefixed temp file in the destination's own directory and `rename(2)`d into
   place. A reader now sees either nothing or the finished value, with no
   intermediate state to catch, at any stall length. The staging name is distinct
   from the destination, so the subtest's earlier "no ACK before the handle
   completes" `os.Stat` still correctly sees `ErrNotExist`.

2. **The regression test is a real reproduction, not a coincidence.** A new
   `AIRA_M20_ACK_STALL` env seam holds the publish open at the moment the value is
   written and not yet published, and the subtest sets it to 200ms. The subtest
   already does the whole real dance — the production launcher, a real detached
   subprocess over the real ready/ACK pipes — so this makes the atomicity of the
   publish a decided question rather than a race the assertion loses only on a
   saturated box. Non-porosity is measured, not asserted: against a mutant that
   restores the non-atomic publish at the same seam the subtest FAILS 5/5
   (`ACK observation = "", want "1"`); against the shipped publish it PASSES 5/5.
   The stall costs the suite 200ms once and lives entirely inside the child, so
   contention can only lengthen the window the test wants open.

3. **The backstop is now sized as a backstop.** The old `time.Second` was never a
   considered value: it is below `testdeadline.MinBackstop`, so it silently
   inherited the package's 5s floor — a floor meant for sub-second intervals, not a
   budget for a subprocess stalled under memory pressure. It is now
   `defaults.detachReadyTimeout` (60s), which is not a number picked by feel but the
   budget the LAUNCHER ITSELF allows for the other direction of this same handshake
   across this same process boundary; the ACK direction is the same class of event
   and gets the same budget. This is a liveness backstop, not a latency assertion —
   what is under test is that the ACK byte comes back complete, never how fast — so
   on a passing run the timer never fires and the size costs nothing. (Contrast the
   sibling `readiness timeout cancels` subtest, where the budget IS the property
   under test and is correctly capped below the alternative it must exclude; that
   one is untouched.)

4. **Both failure directions are now self-describing.** A missed deadline reports the
   path, the effective deadline, the pre-scale budget and the scale in force —
   verified by mutation:
   `fake supervisor published no ACK observation at <path> within 1m0s (1m0s before scaling by 1)`.
   A wrong value reports what it wanted and names both causes that can produce it.

### Checked and deliberately not changed

`confine_detach_linux_test.go:212` publishes a child-written marker with the same
non-atomic `os.WriteFile`, but its only reader (`:298`) is an existence check that
never reads the content, so an empty observation there is harmless. Left alone rather
than swept up: no defect, and out of this ticket's scope.

### Gate

    aira confine -- go build ./...                              exit 0
    aira confine -- go vet ./...                                exit 0
    AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1   exit 0
