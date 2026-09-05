---
{"schema":1,"id":"AIRA-43","project":"aira","title":"no test pins the granted-path lease-hold contract between the CLI and the daemon","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","test-coverage"],"hold":false,"relations":[]}
---
Found by Fable build-review (final gate, AIRA-30).

**The gap.** `runWorkerAdmitCommand`'s granted path (print the grant line,
hold stdin open as the daemon-side lease, `lease.Close()` on stdin EOF,
`cmd/aira/main.go`) has NO test anywhere that would fail if this contract
regressed in either direction -- despite `supervisor.py` explicitly
documenting this contract as confirmed and load-bearing (its malformed-
grant deadlock fix, AIRA-38 Sol review wave, depends on knowing exactly
when the relay does and does not exit).

**Failure scenarios, both currently invisible to every existing test:**
- Regression A: the stdin-hold block is dropped and the CLI exits right
  after printing `granted`. The daemon's dies-with-socket lease then
  releases the ledger entry while the worker is still alive, so
  Σ(granted caps) silently under-counts and the aggregate guard
  (`evaluateWorkerAdmit`) is defeated -- over-admission up to an
  outer-scope `memory.oom.group` kill.
- Regression B: the CLI never exits on stdin EOF. `_retire_worker`'s
  `wait(timeout=5)` swallows the resulting `TimeoutExpired`
  (supervisor.py) and orphaned relay processes accumulate holding live
  grants.

Under BOTH regressions every existing test stays green: `cmd/aira/
worker_admit_test.go` covers argument parsing and the floor only; the
daemon boundary test (`worker_admit_cli_boundary_test.go`) covers denied/
timeout only (granted needs a real cgroup for `CreateWorkerScope`, which
that file's fixture doesn't set up); `worker_admit_client_linux_test.go`
covers `RequestWorkerAdmit` (the Go client function), not the CLI
subprocess loop; every Python supervisor test stubs the CLI with a
Python script that never exercises the real binary's own hold/release
behavior.

**Candidate approach.** Extend `worker_admit_cli_boundary_test.go`'s
existing real-daemon-fixture pattern with a real, delegated cgroup scope
(`internal/cgrouptest`, same technique the real-cgroup e2e tests already
use) so the granted path runs against the real `CreateWorkerScope`; hold
the CLI's stdin open via a pipe, assert it does NOT exit while held,
close it, assert prompt exit AND that the daemon-side ledger entry is
actually released (e.g. a second identical request against the same
committed-cap ceiling can now be granted). Deliberately not built inline
during the final gate response pass -- real-cgroup test infrastructure
setup is enough work to warrant its own focused round rather than being
rushed alongside a dozen other findings.

relates AIRA-30, AIRA-38, AIRA-42.

## Built (2026-09-05, backlog-remediation Phase 2, plan section 4)

`TestWorkerAdmitCLIHoldsTheGrantUntilStdinClosesAndThenExits`
(`internal/daemon/worker_admit_cli_granted_linux_test.go`) drives the real
`aira worker-admit` binary as a subprocess against a real in-process daemon and
a REAL delegated cgroup — the combination no existing test had, and the reason
this contract went unpinned. It follows the candidate approach above: the
daemon's own `CreateWorkerScope` and `scanWorkerScopeChildren` seams stay at
their production defaults against a `cgrouptest` outer scope, and only the two
memory READINGS are stubbed, so the admission arithmetic is deterministic while
every cgroup object the contract is about is real.

Four phases, each pinning one thing:

1. **The grant.** Exactly one structured stdout line, `state=granted
   class=granted`, whose `scope`/`worker_id`/`memory_max`/`memory_high` name a
   cgroup that really carries those limits (`memory.max`, `memory.high`
   page-floored as `writeScopeMemoryCap` verifies, `memory.oom.group=1`).
2. **Regression A.** The relay does not exit while its stdin is open — and,
   separately, the daemon still holds the granted connection (observed through
   the shared `admitSlots` token `workerAdmitConnection` holds for a granted
   connection's whole lifetime). The second assertion is not redundant: a relay
   that closed its lease and only THEN blocked on stdin frees the grant
   daemon-side exactly as Regression A does while looking correct from outside.
3. **Regression B.** Closing stdin makes it exit promptly, exit 0, having
   written nothing more to stdout and nothing at all to stderr — the last part
   being what supervisor.py's malformed-grant ordering fix is premised on.
4. **AIRA-41.** Closing the lease releases NO ledger capacity; only removing
   the scope does.

**The ticket's own premise was stale and is corrected rather than reproduced.**
Regression A above is described as defeating the ledger, and the candidate
approach proposes asserting that closing the lease lets an identical second
request through. That was true of the pre-AIRA-39/41 in-memory grants map and
is now false: the ledger is Σ `memory.max` over the outer scope's real
`.aira-worker-*` children, so closing a lease frees nothing and only
`_forget_worker_scope`'s rmdir does. Asserting the ticket's version would have
failed against a correct build. The live consequence of Regression A is instead
the daemon-side connection (and its admission slot) being released while the
worker is alive, plus supervisor.py's retirement signal reaching a process that
is already gone — which is what phase 2 pins. Phase 4 pins the real invariant.

Root cause of the stale premise found and fixed: `runner.WorkerAdmitLease`'s doc
comment still claimed "the daemon frees the ledger entry when it detects the
peer disconnect" (`internal/runner/worker_admit_client_linux.go`), which is what
AIRA-43 restated as fact. Corrected in place.

**Mutation-tested, five mutations, all killed** (production source restored to
pristine after each; re-verified after the reader restructure below):

| Mutation | Caught by |
|---|---|
| A — stdin-hold block deleted (the ticket's Regression A) | phase 2, "the relay exited while its stdin was still open" |
| B — stdin EOF never observed (the ticket's Regression B) | phase 3, "did not exit within 30s of its stdin closing" |
| C — `lease.Close()` moved before the stdin block; relay stays alive | phase 2's `admitSlots` assertion, and ONLY that one |
| D — daemon rmdirs the scope on peer disconnect (the pre-AIRA-41 shape) | phase 4, "a closed lease must not release the ledger" |
| E — reported `memory_high` drifts from the value written to the cgroup | phase 1, "the granted line names limits the kernel does not hold" |

**A real data race in the first draft of this test, caught by `-race` rather
than reasoned about:** the drain goroutine and the test goroutine both touched
one `bufio.Reader`. Restructured so a single goroutine owns the reader for its
whole life and reports its two events over two buffered channels; that also
bounds the first-line read, which was previously unbounded.

## Build-review response (independent adversarial pass, PASS-WITH-CHANGES)

An independent reviewer re-verified the build in its own throwaway worktree
(new test `-count=5` stable, `-run TestWorkerAdmitCLI -race -count=3` clean, no
leaked cgroups or stray relay processes even after a deliberately wedged relay),
confirmed the stale-premise correction above from source, and wrote four
mutations of its own. Two of its mutations were KILLED and add coverage the
build did not claim — notably deleting the daemon's own post-grant park fails
phase 2's `admitSlots` assertion, so that half catches a DAEMON-side regression,
not only the CLI-side one. It also raised one P1 and three P2s, all findings
about honesty rather than about the code, and all answered below rather than
argued with.

**Two claims in the original write-up were wrong and are corrected.**

1. *"Nothing existing reaches the granted CLI path"* — **false**, and reworded
   to "nothing existing ASSERTS it". `internal/pylib/pytest_aitest_e2e_test.go`'s
   real-daemon-and-cgroup cases pass the freshly built binary as
   `AIRA_AITEST_WORKER_ADMIT_CMD` and do execute the real granted relay. The
   substantive claim survives — the reviewer applied mutation A there and those
   tests stayed green, because every assertion they make is satisfied by a relay
   that exits immediately after printing its grant — but "reaches" and "asserts"
   are different claims and the stronger one was not true.
2. *Phase 4 pins AIRA-41* — **overstated**.
   `TestWorkerAdmitLedgerKeepsChargingAfterRelayCloses`
   (`internal/daemon/worker_admit_test.go:596`, already carrying
   `verifies: AIRA-41`) already asserts the invariant, against a stubbed
   `workerScopeTree` and with no connection ever opened. Phase 4's real
   contribution is narrower and is now stated as such: the real cgroup tree plus
   the real `workerAdmitConnection` peer-disconnect path, which is why mutation
   D is caught here and nowhere else.

**Accepted coverage gaps, named rather than left silent.**

- **Phase 2's hold assertion is bounded by the dwell, not by stdin EOF.** It
  proves the lease is still held at t=1s; a regression that releases it LATER
  survives. The reviewer demonstrated this: adding
  `case <-time.After(2 * time.Second)` to the daemon's post-grant park leaves the
  test green, and real aitest workers hold grants for minutes. Lengthening the
  dwell only moves the threshold, and the alternative is a production seam that
  announces lease state — machinery this project does not add for a
  telemetry-grade signal. Documented on the constant and accepted.
- **`lease.Close()` on the granted path is not pinned.** Deleting it leaves the
  test green: process exit closes the socket fd anyway, so phase 3's
  `admitSlots → 0` poll cannot distinguish the explicit call from kernel fd
  teardown. The test pins the observable, not the call.
- **`admitSlots` is an AIRA-63 implementation detail rather than a declared
  contract**, so a future change that released the slot before parking on the
  lease would fail phase 2 without a real regression. Used because it is the only
  daemon-side observable for "this lease is still held"; the reviewer verified it
  is sound today (exactly two acquire sites, one connection in this test, the
  slot taken synchronously long before the grant frame, and `len()` on a channel
  is not race-instrumented).
- **Phase 1's `memory.max` row is a plain equality check**, not a floor test:
  `request` is a whole number of MiB and already page-aligned. Only the
  `memory.high` row exercises the page floor (reported 26843545, kernel 26841088).
  Stated in the comment so a later reader does not assume both are load-bearing.
- **The `estimatedBytes*4/5` watermark formula itself is unpinned** — a formula
  change moves the reported value and the cgroup value together. Only
  `0 < memory_high < memory_max` is asserted, which is what spec 3.3 actually
  requires.
- **The test skips on a host without real cgroup-v2 delegation**, per the
  standing `cgrouptest` policy, and hard-fails there under `AIRA_REAL_CGROUP=1`.

**Noted, not fixed here, out of this ticket's scope:** `.aira/tickets/AIRA-41.md`
is still `"status":"planned"` although its fix is built and
`worker_admit_test.go:591` has carried `verifies: AIRA-41` since before this
change. This commit adds a second such marker. Flagged for the backlog owner
rather than closing another ticket as a silent side effect.
