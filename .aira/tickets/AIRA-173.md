---
{"schema":1,"id":"AIRA-173","project":"aira","title":"TestRunInputServerACKReconnectAppendAndExplicitClose flakes under full-suite load with a broken-pipe write to the run-input socket","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["flaky","runner","tests"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-173","to":"AIRA-174"}]}
---
Filed rather than re-run until green, following the AIRA-106/AIRA-112 precedent.

`make ci` in the pre-push hook failed on PR #105's branch:

```
--- FAIL: TestRunInputServerACKReconnectAppendAndExplicitClose (0.00s)
    run_input_server_linux_test.go:287: write unix @->/home/mark/tmp/aira-d6-run-input-tests/
    case-440245273/inputs/RUN-1-a00e713dcb7caf578bc81c5d.sock: write: broken pipe
FAIL	aira/internal/runner	133.373s
```

A write to the run-input unix socket lost the race with the server closing its
end. The test fails in 0.00s, so nothing timed out; the peer was simply gone by
the time the client wrote.

## Evidence that this is not PR #105's

- that branch touches **zero** files under `internal/runner`
  (`git diff --name-only origin/master...HEAD | grep -c internal/runner` → 0);
- the named test passes **5/5** in isolation on the branch
  (`go test ./internal/runner/ -run <name> -count=5` → exit 0);
- the branch's own full suite passed **twice** standalone before the push —
  `AIRA_REAL_CGROUP=1 go test ./... -count=1` exit 0, and plain
  `go test ./... -count=1 -timeout 20m` exit 0.

## What is NOT established

A clean-`origin/master` (`2364a17`) full-suite run in a throwaway detached
worktree passed (exit 0), so there is **no clean-master reproduction** — one
green run is not evidence of absence for an intermittent failure, and this
ticket does not claim one. Two pre-push runs on the branch failed; the first
one's output was truncated before the test name was captured, so it is not known
whether it was this same test.

The honest statement is therefore: an intermittent failure of unmeasured
frequency, in a package the triggering branch does not touch.

## Where to look

`internal/runner/run_input_server_linux_test.go:287`. The ACK/reconnect handshake
means the client may legitimately observe the server's close between two writes;
the question is whether the TEST tolerates that (it should synchronise on the ACK
rather than assume the peer is still there) or whether the SERVER closes earlier
than the protocol allows. Decide which before touching either — a retry loop
added to the test would hide a real server-side early close, which is the failure
mode worth catching.

## Frequency first

Before changing anything, measure: run `internal/runner` under the same
whole-suite contention (all packages in parallel, `-count=N`) and record the
failure rate. A fix with no before/after rate is not evidence.

---

# Investigation (2026-09-08)

## 1. Measured frequency

Reproduced. The lever is CPU oversubscription, not the suite as such: the race
needs the client goroutine descheduled between `connect()` and its first
`write()`. Committed harness: `docs/dev/aira-173-flake-rate-repro.sh` (N busy
loops + `go test -run <name> -count=N`; run it under `aira confine`). It is
shown to be capable of failing — the clean-master row below was produced by the
committed script itself, not by a private variant.

| condition | executions | failures | rate |
|---|---|---|---|
| natural load (no stressor; box already at load ~47), `GOMAXPROCS` default and 1, 2, 4 | 8,000 | **0** | <0.013% |
| 32 CPU burners | 20,000 | **4** | 0.020% |
| 64 CPU burners | 30,000 | **3** | 0.010% |
| 32 CPU burners, **clean `origin/master` `c30c074`** | 20,000 | **2** | 0.010% |
| **stressed total** | **70,000** | **9** | **0.013%** (1 in ~7,800) |

**The clean-master reproduction this ticket recorded as NOT established now
is.** The last row ran in a throwaway detached worktree at `origin/master`
`c30c074`, with no part of the fix present, and reproduced the same signature —
so the defect is master's, confirming the filing's reasoning that PR #105 (which
touches no file under `internal/runner`) was only the messenger.

All 9 failures carry the identical signature — no second failure mode:

```
run_input_server_linux_test.go:287: write unix @->…/inputs/RUN-1-<nonce>.sock: write: broken pipe
```

The *same* race also fires much more often one step later, on the read, where
the helper already tolerated it: instrumenting the existing BUSY-retry branch
counted **99 transient BUSY retries in 10,000 executions (~1%)** under the
32-burner condition. So the underlying reconnect race is routine (~1 in 100) and
the flake is its ~1-in-70 sub-window where the refusal beats the client's write.

**Accepted gap:** the whole-suite *per-run* rate is not directly measured. A
full-suite run executes this test once, so at p≈1.4e-4 it would take thousands
of suite runs to estimate; the burner condition is a proxy and may under- or
over-state the real figure. It is the failure *mechanism* that is established
below, not a whole-suite probability.

## 2. Mechanism (traced from source, then proven)

Line 287 is `second := dialRunInputHello(...)`; both it and `dialRunInputRawHello`
call `t.Helper()`, so a `t.Fatal` inside them reports at line 287. The reported
error is the **HELLO write of the reconnect**, not a data write.

The sequence, all within contract:

1. `handle()` answers `first`'s half-close with the final ACK and returns
   (`run_input_server_linux.go:210-212`). The writer slot is released **after**
   that, in the deferred `p.state.CompareAndSwap(1, 0)` (`:181`).
2. The test reads that ACK and immediately reconnects. The kernel completes
   `connect()` regardless of the plane's application-level state.
3. `acceptLoop` accepts while `state` is still 1, so the CAS at `:139` fails and
   it calls `reject(conn, "E_RUN_INPUT_BUSY")`, which **writes the error frame
   and closes** (`:159-171`).
4. The client, descheduled between `connect()` and its write, now writes HELLO
   to a closed peer → `EPIPE`.

Proven, not inferred: a probe holding the writer slot and letting the
reject-and-close complete before writing HELLO reproduces
`write unix @->…sock: write: broken pipe` byte-for-byte, and the
`E_RUN_INPUT_BUSY` frame the server wrote is **still readable** off the socket
afterwards (a Unix stream keeps what the peer wrote before closing). That second
fact is what makes an evidence-based fix possible instead of a blind retry.

## 3. Verdict on the ticket's question

**Not a server-side early close.** The refuse-and-close is the design's own
`E_RUN_INPUT_BUSY` path (§2.4 "concurrent writers are refused"), and the
release-after-ACK ordering is deliberate — the ACK is deliberately sent *before*
`closeInput` for a different documented race (`run_input_server_linux.go:238-241`).
Releasing the slot before the final ACK would only narrow the window, never close
it: the client cannot learn the release time from the wire. That is exactly why
both the client and the test helper already carry a bounded BUSY retry.

**It is an incomplete client-side tolerance** — and the flake was pointing at a
real one in *production* code, not only in the test:

`connectRunInput` (`run_input_linux.go:158-162`) treated **any** HELLO write
error as `E_RUN_INPUT_OUTCOME_UNKNOWN` and returned immediately. So the
documented BUSY retry ("the single-writer slot is released asynchronously … a
fast sequential reconnect can transiently race it", `:30-35`) was silently
bypassed whenever that same race landed on the write instead of the read. Worse,
the code it returned is **dishonest**: at HELLO time no DATA frame has been sent,
so no byte can have reached the child's stdin. The design defines
`OUTCOME_UNKNOWN` as precisely the *delivery ambiguity* after a connection drops
before a final ACK — there is none here. The pre-fix error string says so itself:
`E_RUN_INPUT_OUTCOME_UNKNOWN: … broken pipe (committed=0)`. §2.4 also states a
`run-input` racing teardown is refused `E_RUN_INPUT_CLOSED`/`UNREACHABLE` —
never `OUTCOME_UNKNOWN`.

## 4. Fix

`classifyRunInputHelloWriteError` (shared by the client and the test helper):
on a failed HELLO write, read the refusal the server already queued and report it
under the server's own code; if nothing explains the close, report
`E_RUN_INPUT_UNREACHABLE` with committed 0 — the spec's "socket is unreachable —
dead/gone" — never a fabricated unknown.

- Production: a zero-committed BUSY now re-enters the existing bounded retry
  whichever side of the write it lands on. Retrying is safe here and cannot
  duplicate stdin bytes: it happens strictly before `Input` streams any DATA, on
  a fresh connection.
- Test helper: `dialRunInputRawHello` returns the classified refusal instead of
  failing on the bare write error; `dialRunInputHello` retries **only** a proven
  zero-committed BUSY, within the budget it already had.

The server is unchanged.

**This is deliberately not the retry loop the ticket warned against.** A retry is
taken only on a refusal frame the server actually sent, decoded, and only when it
is a zero-committed BUSY. A peer that closes without a frame → `UNREACHABLE`,
terminal. Any other refusal (`CLOSED`, `FOREIGN_OWNER`, …) → that code, terminal,
first attempt. A genuine early close therefore still fails, loudly and
immediately; regression tests pin both directions.

## 5. Verification

Regression tests (all four failed or would have been meaningless before the fix;
the fix was written only after watching them fail):

- `TestRunInputClientRetriesBusyThatBeatTheHelloWrite` — the race, retried into a
  full successful send. Before: `OUTCOME_UNKNOWN`, 1 dial.
- `TestRunInputClientReportsUnreachableWhenHelloWriteFindsNoRefusal` — the
  honesty claim; asserts the code is *not* `OUTCOME_UNKNOWN`. Before: it was.
- `TestRunInputClientDoesNotRetryNonBusyRefusalThatBeatTheHelloWrite` — the
  anti-blind-retry discriminator (`CLOSED`, `FOREIGN_OWNER` stay terminal, 1 dial).
- `TestRunInputServerBusyRefusalOutlivesTheCloseThatRacesTheHelloWrite` — pins the
  real-server property the fix depends on (refusal written *before* the close and
  still readable after it). **Passed against the unmodified server**, which is the
  positive evidence that the server needed no change.

The first three are made deterministic with a real `socketpair` whose peer has
already refused and closed, so they do not depend on scheduling.

Before/after under the identical stressors:

| | before | after |
|---|---|---|
| 32 burners × 20,000 | 4 | **0** |
| 64 burners × 30,000 | 3 | **0** |
| total | 7 / 50,000 | **0 / 50,000** |

Expected count after the fix had nothing changed was 7; observing 0 gives a
one-sided Poisson p ≈ 9e-4. (The clean-master row in §1 is a separate before-fix
sample and is excluded from this paired comparison.)

Gate, exact exit codes: `go build ./...` **0**; `go vet ./...` **0**;
`AIRA_REAL_CGROUP=1 go test ./... -count=1` **0** (zero `FAIL` lines,
`internal/runner` ok 131.4s). All under `aira confine`.

Unrelated pre-existing finding, not fixed here: the module does not cross-build
to `GOOS=darwin`/`windows` — `internal/pylib/extract.go:110` uses the Linux-only
`unix.Renameat2`/`RENAME_NOREPLACE`. Identical on untouched `master`; the release
target is a single static Linux binary.

## Review (Fable build-review gate) — MERGED

PR #107 merged as `7401e8d` (head `1021be2`). Reviewer's own verification, not
the builder's narrative:

- **Mechanism read from source, confirmed.** `handle()` writes the final ACK
  (`run_input_server_linux.go:210-212`) and releases the single-writer slot only
  in its deferred `CompareAndSwap(1, 0)` (`:181`); a reconnect accepted in that
  window fails `acceptLoop`'s `CompareAndSwap(0, 1)` (`:139`) and `reject()`
  writes the BUSY frame then closes (`:159-171`). The pre-fix client
  (`run_input_linux.go`, old `:158-162`) returned `E_RUN_INPUT_OUTCOME_UNKNOWN`
  on *any* HELLO write error, bypassing the BUSY retry its own comment at
  `:30-35` documents. Both halves of the ticket's diagnosis hold.
- **The fix is the right shape.** Server untouched. `classifyRunInputHelloWriteError`
  retries only what the read side already retried — a refusal frame the server
  actually wrote and the client decoded, code `E_RUN_INPUT_BUSY` — on a fresh
  connection before any DATA frame, so no stdin byte can be duplicated. A peer
  that closed without a frame is `E_RUN_INPUT_UNREACHABLE` with committed 0,
  terminal on the first attempt; `CLOSED`/`FOREIGN_OWNER` stay terminal. This is
  not the symptom-retry the filing warned against.
  `TestRunInputServerBusyRefusalOutlivesTheCloseThatRacesTheHelloWrite` pins the
  real-server property (refusal written before the close, readable after) and
  passes on the unmodified server.
- **Test-helper migration checked for new porosity.** Only one caller moved to
  `mustDialRunInputRawHello` (the foreign/steal case: first connection on a fresh
  plane, cannot race BUSY). Every BUSY-expecting test uses a fake `net.Pipe`
  server or a direct dial, so none newly fatals on the race being fixed.
- **Tests run by the reviewer:** the four new tests plus the flaky test and the
  auth/steal test, `-count=30 -race`, under `aira confine`: `ok`, exit 0.
- **Measurements verified on disk, not taken on trust.** `~/tmp/aira173/`:
  `stress-32burn.log` 4 FAIL / `stress-64burn.log` 3 FAIL (before, private
  harness identical in substance to the committed script; elapsed 133 s / 269 s
  consistent with 20,000 / 30,000), `after-32burn.log` and `after-64burn.log`
  `ok` (110 s / 225 s); every failure line the single `:287 … broken pipe`
  signature. `~/tmp/aira173-repro/run-32burners-20000.log`: 2 FAIL at clean
  `c30c074` from the committed script. `probeD.log` holds the cumulative
  read-side transient-BUSY counter behind the ~1% figure. The natural-load
  8,000-execution row cannot be re-derived from its logs (no `-v`, counts not
  recorded); it is the zero row and nothing rests on it.
- **Reviewer's own replication with the committed script**, both under
  `aira confine`, 32 burners × 20,000: fixed branch `1021be2` → **0 failures**
  (go test exit 0); clean `origin/master` `8aa18f5` in a throwaway detached
  worktree → **3 failures**, same signature. The harness fails in the
  reviewer's hands on unfixed code, so the 0 on the fix is not vacuous.
- **Merge gate:** PR CI `build + vet + gofmt`, `test`, `race` all SUCCESS at
  `1021be2`; the confined pre-push `make ci` (fmt-check, vet, build, full
  test) on this record's push is the post-merge full-suite run — the hook is
  fail-closed, so this record reaching `origin/master` is itself the exit-0
  evidence (reviewer's push log: `~/tmp/aira173-review/push.log`).

Recorded, non-blocking:

1. **Pre-existing asymmetry left in place — follow-up AIRA-174.** The read side
   of HELLO (`readRunInputResponse(conn, 0)`) still reports
   `E_RUN_INPUT_OUTCOME_UNKNOWN` with committed 0 when the peer closes without a
   frame *after* a successful HELLO write (e.g. `closeTerminal` racing the
   handshake). By this ticket's own reasoning no DATA has been sent, so that is
   the same fabricated unknown and should be `E_RUN_INPUT_UNREACHABLE`. Not
   introduced here and not on the flake's path; deliberately not widened in
   this PR.
2. **One sentence in §3 is overstated.** "Releasing the slot before the final
   ACK would only narrow the window, never close it" is not true for the
   *sequential*-reconnect case: an ordering of release → ACK → close would make
   a reconnect made on that ACK happen-after the release. It would not remove
   BUSY from a genuinely concurrent writer, so the client-side classification
   is required regardless, and the server ordering is left as is on purpose
   (keep the primitive; the retry already existed for exactly this race). Noted
   so the sentence is not later read as a proof.
3. Accepted gap carried forward: the whole-suite per-run rate is a proxy
   (burner condition), not a measurement; the mechanism is what is established.
4. The darwin/windows cross-build finding is pre-existing and unrelated.
5. Closing this ticket exposed two dogfood frictions, filed as RANT-25 (the
   installed binary predates AIRA-170's P3 tier, so `aira create` refused the
   follow-up; AIRA-172 item 1) and RANT-26 (`mv planned -> done` is refused,
   so every done record in this repository hand-edits the status).
