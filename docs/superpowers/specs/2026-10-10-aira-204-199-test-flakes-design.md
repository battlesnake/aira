# AIRA-204 + AIRA-199: remove six merge-gate flake classes

Status: PLAN v1 (2026-10-10). Test hardening, one change. Tickets: AIRA-204 (P1, four
witnessed flakes, each a different defect class) and AIRA-199 (P2, a test hands a callee
its own descriptor). Two flakes seen this week join them: `internal/daemon`
`TestExclusiveIsNotGrantedWhileALeaseIsHeld` and `internal/runner`
`TestM20RealForcedQuiescePreservesLeaderExitAndIsNotClean`. Base: master `d13ef3b`. Both
tickets report `ready:true`.

**Production files touched: two, each by one test seam that is unset in production
(`internal/daemon/admit.go` + `server.go`, `internal/runner/detach_linux.go` +
`runner_linux.go`).** Neither flake is a product race (measured, section 1). The seams get
their own review pass (section 8). Everything else is `_test.go` plus one committed
measurement script.

## 1. Problem, in plain terms, and what still reproduces

The pre-push gate (`make ci` -> `go test ./... -count=1`) runs once with no retry. Each
flake below fails a correct tree under CPU contention and passes alone. Every one was
re-checked on `d13ef3b`; ticket line numbers are stale and the current ones are used.

How it was measured: `docs/dev/aira204-loadloop.sh` (committed with this spec) runs a
compiled test binary with `-test.count=N -test.v`, pinned with `taskset` to a CPU list
shared with N busy-loop burners, all under `aira confine`. "2 CPUs + 6 burners" means
`taskset -c 2,3` for the test and six burners (Go then sets GOMAXPROCS=2 from the
affinity). Raw logs: `~/tmp/aira-flakes/runs/*.log` (scratch; the script reproduces
them). "Prototype" numbers come from a throwaway worktree (`~/tmp/aira-flakes/diag-wt`)
holding the proposed change; the builder re-measures on committed code.

| Id | Test (file) | Defect class | Still reproduces? (evidence) | Verdict |
|---|---|---|---|---|
| A | `TestMCPConfineKillOutsideProjectKeepsOwnershipAndStealChecks` (`cmd/aira/dispatcher_inprocess_test.go:1018`) | Readiness poll on the wrong signal | **Not reproduced**: 0/40 (plain, 2 CPUs + 6 burners). By code: the poll at `:1057-1077` breaks when the list output contains the name and `session-a`, both decoded from the scope DIRECTORY name, which exists before the child is placed; the `--steal` kill at `:1082` then gates on `cgroup.events` (`internal/runner/confine_manage_linux.go:766-770`, `U_CONFINE_NOT_LAUNCHED ... retry`). The no-steal call at `:1078` is refused at `:729-731` before that gate. Three historical sightings (RANT-9, RANT-27, AIRA-35). | Build. Evidence is a predicate unit test plus code reading, not a load rate |
| B | `tui_smoke_test.go` (ticket names `TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight`), and the same shapes across the TUI tests | Three classes: (1) a short inner `t.Fatal` nested inside a retrying poll; (2) fixed sub-second backstops on expected events; (3) `time.Sleep` used to order an injected key before a later UI read | **Yes.** `-race`, 2 CPUs + 6 burners, 4 TUI tests x 100: **9/400 fail**: `:87 keypress deadlocked` x4, `:529 renderer update queue deadlocked` x1, `:297 read dispatch count=0` x2, `:272 confirmation default focus is not Cancel (Confirm=false Cancel=false app=*tview.Button)` x2. Whole TUI set (`^(TestTUI|TestBoard|TestTop|TestOverview|TestPump)`), 20 rounds: 6 failures (`tui_board_smoke_test.go:156/162/168` "Home/Down/End" x4, `:87` x2) and then **a panic that killed the whole `cmd/aira` binary**: nil `paletteConfirmForm` at `tui_smoke_test.go:341`, reached because `waitForSimulationText` (`:558-573`) returns silently on timeout. Deterministic: one 300 ms UI stall queued before the probe -> 17/20 fail at the `:87` site. | Build |
| C | `TestRealRunTimeoutKillsProcessGroup` (`internal/gitremote/exec_test.go:15`) | The subject's own 50 ms timeout is also the whole budget for the test's precondition (sh must start, trap TERM, fork, print two pids) | **Yes.** 1/60 (plain, 2 CPUs + 6 burners): `exec_test.go:30: pid output=""`. Deterministic: `sleep 0.2;` before the trap -> 10/10 fail. | Build |
| D | `TestRealCgroupDynamicChargeTracksMemoryCurrentAndFreesAWaiter` (`internal/daemon`) | Unsettled fixture | **Moot.** The file was deleted with the feature in `9a2f65c` (admission-counter S1: delete the AIRA-29 dynamic live-charge). | Drop |
| E | `TestExclusiveIsNotGrantedWhileALeaseIsHeld` and 10 siblings (`internal/daemon/admit_exclusive_test.go`, `worker_admit_exclusive_test.go`) | The test seeds its precondition while the queue's own evaluator goroutine is live | **Yes.** Witnessed `~/tmp/batch-final-ci.log:11-12` (2026-10-10 14:30): `admit_exclusive_test.go:317: ... expected the waiter to stay queued, got state=1 outcome="immediate"`. `-race`, 2 CPUs + 6 burners, `^TestExclusive` x 2000: **9/24000** across 4 tests (`:165` x6, `:179`, `:224`, `:317`), all `outcome="immediate"`. Plain, 16 CPUs, no burners: 2/36000. Deterministic: a 20 ms gap between the first enqueue and `seedRunningLease` -> 50/50 fail. | Build |
| F | `TestM20RealForcedQuiescePreservesLeaderExitAndIsNotClean` (`internal/runner/detach_linux_test.go:1192`) | One fixture duration plays two roles: the wait the test needs to EXPIRE and the backstop it needs NOT to expire | **Yes.** Witnessed `~/tmp/batch-final-ci2.log:18-19`: `detach outcome={record:0xc0002b9b08 err:0xc000387300}` (pointers; the message hides the error). Plain, 2 CPUs + 6 burners: 3/200. A diagnostic build printing the error: 6/300, every one `U_RUN_CAPTURE_INCOMPLETE: scope did not become empty`. | Build |
| 199 | `TestM20bSupervisorRejectsMalformedSidecarBeforeOpeningProject` (`cmd/aira/m20b_test.go:14`) | Test passes `readyW.Fd()` to a callee that closes it; `readyW`'s finalizer later closes the number again | **Yes.** `-race`, 16 CPUs, `^TestM20b` x 400: **8/400** `m20b_test.go:54: EOF`. Deterministic: two `runtime.GC()` calls plus a 20 ms window between building the arguments and `runSupervisor` -> 50/50 EOF. Cross-test probe (the ticket's "third instance", `TestCLIRunRealCgroupOrClearSkip` failing `fork/exec /usr/bin/git: bad file descriptor`): interleaved with m20b, 300 rounds, -race -> 0/600. | Build; the CLIRun attribution stays unevaluated |

### Mechanisms, established

- **A.** Scope directory exists -> list shows the name and owner -> poll breaks -> the
  child is not yet in `cgroup.procs` -> the steal kill returns `U_CONFINE_NOT_LAUNCHED`.
  The record field that answers "placed?" already exists:
  `ConfineRecord.SubtreePopulated` (`confine_manage.go:62`), read from the same
  `cgroup.events` source the kill gate uses, `nil` when unestablished.
- **B(3).** tview's loop is one `select` over `a.events` (keys) and `a.updates`
  (`QueueUpdate`) (`tview@v0.42.0/application.go:391-510`). Keys are FIFO among
  themselves; a key and a later `QueueUpdate` are NOT ordered. A test that injects a key,
  sleeps 20 ms, then reads state through `QueueUpdate` can read the state from before the
  key. The `:272` and board `:156/162/168` failures are reads of exactly that kind.
  The panic is established only from its stack: `waitForSimulationText("Type RANT-7")`
  returned without the text and `checkDisabled` (`:341`) dereferenced a nil
  `paletteConfirmForm` on the UI goroutine. The likely route (inferred, not traced) is
  the same ordering: `tabToPaletteSubmit` (`:429-446`) reads focus, Tabs, sleeps, reads
  again; a read that overtakes its Tab sends a second Tab, the stale Tab then moves focus
  off Submit, and Enter lands on Cancel.
- **E.** `enqueueAdmitInternal` creates a queue, starts `go queue.runEvaluator()`
  (`admit.go:2003`) and kicks it (`queue.signal()`, `:2154`). The evaluator grants the
  exclusive waiter on an empty slice before the test calls `seedRunningLease` or sets
  `outstandingJobs = 1`. `evaluate()` (`admit_exclusive_test.go:97-101`) retires the
  goroutine, but only from its first call, which is after the seeding.
  `pruneAdmitQueue` (`admit.go:2718`) deletes an emptied queue, so the next enqueue
  births a fresh queue with a live evaluator: `TestRepeatedExclusiveRequestAndAbandonNeverWedgesTheSlice`
  case 1 re-enters the race on every round.
- **F.** `forceDetachedQuiesce` uses `r.grace` twice: the natural-quiesce wait
  (`detach_linux.go:591`, which must expire so the forced path runs) and the post-
  `cgroup.kill` emptiness confirmation (`:646`, which must not expire). The test sets
  `r.grace = 100ms` for the first role. Measured kill-to-empty time in the diagnostic
  build, n=300, 2 CPUs + 6 burners: **p50 10.2 ms, p90 11.1 ms, p99 100.8 ms, max
  240.3 ms**. 100 ms is inside the tail.
- **199.** As the ticket says; confirmed by the forced-GC repro. `supervisor_relay_store_test.go:51-58`
  already has the right helper (`dupTestFD`); `m20b_test.go` is the only remaining
  raw-fd hand-off in `cmd/aira` (grep of `.Fd()` in test files).

### Not product races

- **E (by code reading):** production has one writer per queue (its evaluator) and no external party seeds
  ledger state between enqueue and the first pass. The race exists only between a test's
  hand-seeding and the evaluator.
- **F (measured):** production never sets `Config.Grace` (grep: only tests do); the default is 2 s
  (`runner_linux.go:188-190`), over 8x the measured maximum kill-to-empty time (240 ms)
  under 2-CPU contention. A slower case still reports honestly (`U_RUN_CAPTURE_INCOMPLETE`),
  never a fake pass.

## 2. Greenfield minimum, and rejected alternatives

Solving each alone, from nothing:

- **A:** wait for the fact the next step needs ("the record with exactly this name is
  populated"), not for a proxy (the directory exists).
- **B:** wait for the event, never for a time: a UI read either completes or the loop is
  deadlocked (a generous backstop says which); an injected key is followed by a barrier
  that proves its handler ran, never a sleep.
- **C:** start the subject's deadline after the precondition is observed (the pid line),
  still as a deadline.
- **E:** a test that drives admission passes by hand must be the queue's only evaluator
  from the moment the queue exists.
- **F:** give the "must expire" wait and the "must not expire" backstop separate values.
- **199:** give the callee a descriptor it may own; close your own ends yourself.

The plan is this minimum. Two small test seams (E, F) are the only production lines.

| Item | Alternative | Why rejected |
|---|---|---|
| A | Retry the kill on `U_CONFINE_NOT_LAUNCHED` | Legitimate (the message says "retry") but it waits on the error rather than the fact, and a retry loop around the subject can hide a kill gate that never opens. Waiting for `subtree_populated` asserts the precondition, then calls the subject once. |
| A | Substring `subtree_populated":true` in the raw output | The test launches into the real machine-wide `aira.slice`; a neighbour's populated scope satisfies it. Parse and match the exact name. |
| B | Just raise the 100 ms / 1 s constants | Leaves the nested-fatal and key-ordering classes in place; the panic case is an ordering bug that no constant fixes. |
| B | Delete the inner timeout arm | Leaves an unbounded receive: a real deadlock becomes a 20-minute package hang. The arm stays, as a liveness backstop. |
| B | Replace the inner `t.Fatal` with `continue` (the ticket's proposal) | Spawns a fresh `QueueUpdate` goroutine every 100 ms on a slow loop and loses the "deadlocked" diagnosis. One wait per read with a generous backstop is simpler and keeps the diagnosis. |
| B | Per-site screen-text predicates instead of a key barrier | Several texts are true before and after the key, so the wait passes vacuously. Example, from the code (not run): after Space-at-Cancel at `:637-642`, `paletteCancelAction` shows the palette LIST, not the dashboard (`tui.go`, `showPaletteList`), and the tab row holding `"1:Tickets"` stays outside the centred 80x22 page on the 110x36 screen, so the `:642` wait passes whether or not the modal closed. A barrier proves the key was handled; the assertion then reads state. |
| C | `context.WithCancel` / `WithCancelCause(ctx, DeadlineExceeded)` armed on the pid line | `realRun` returns `ctx.Err()`; both give `context.Canceled`, so `:22`'s `errors.Is(..., DeadlineExceeded)` goes red every run. A test-local context whose `Err()` is `DeadlineExceeded` keeps the assertion meaning "realRun returns the context's error". |
| C | Raise the 50 ms | Any fixed value is still the precondition's whole budget. |
| E | Rewrite the 11 affected tests to establish the precondition first (a granted "keeper" lease before the exclusive request, as `admit_exclusive_identity_test.go` already does) | No production change, and it would replace the phantom `outstandingJobs = 1` scalars with real leases. But it is 11 test rewrites, several of which change what the test exercises (every round of `TestRepeatedExclusiveRequestAndAbandonNeverWedgesTheSlice` case 1 would need its own keeper), and nothing stops the next evaluate-driven test from reintroducing the race. The seam removes the class at its source in one place. Kept as a deferral (real leases instead of phantom scalars), not needed for the flake. |
| E | Pre-register an evaluator-less queue from test code | `pruneAdmitQueue` deletes it when it empties, and the next enqueue births a live one; it also duplicates the queue constructor in test code. |
| F | `r.grace = testdeadline.Wait(r.grace)` (the precedent at `detach_linux_test.go:1080`) | Phase 1 always waits the full grace (the `sleep 30` never exits), so every run costs >= 5 s, 20 s under `-race`. |
| F | `r.grace = time.Second` (the fixture default) | +0.9 s per run, and a fixed unscaled 1 s backstop on a kernel latency whose measured maximum is already 240 ms on 2 CPUs: the same class with a wider margin. |
| F | Use one post-kill bound for every `waitEmpty` site in the runner | Not witnessed elsewhere; production uses 2 s everywhere. Minimal change: the one site this test exercises. |
| 199 | `runtime.KeepAlive(readyW)` | Measured insufficient in the ticket (1 failure in 400) and does nothing for the later cross-test close. |
| all | A lint/CI guard against raw `time.After` | AIRA-204 already decided against it (about half the raw sites are correct negative waits; the allowlist is the complexity). |

## 3. Design

### 3.1 A: `cmd/aira/dispatcher_inprocess_test.go`

- New test helper `confineKillReadiness(output []byte, name string) (listed, placed bool, err error)`:
  decode the MCP `tools/call` envelope (`result.structuredContent.data`) into
  `runner.ConfineListResult`; collect the records whose `Name == name` (exact equality).
  None -> `(false, false, nil)`. More than one -> `err` naming the count (ambiguous; the
  kill selector would be refused `E_SELECTOR_AMBIGUOUS`). One -> `listed = true`,
  `placed = rec.SubtreePopulated != nil && *rec.SubtreePopulated`. Undecodable output ->
  `err`. `nil` is "not established", never "placed".
- The poll at `:1057-1077`: same loop, now on `confineKillReadiness`; `err` -> `t.Fatal`.
  Backstop `testdeadline.Wait(5*time.Second)`. On expiry: never `listed` ->
  `cgrouptest.SkipOrFailRealCgroup` (the environment could not run the job: unevaluated,
  as today); `listed` but never `placed` -> `t.Fatalf` with the last record (the launch
  was admitted and its scope exists, so a missing placement is a product failure, not an
  environment gap). The `done` early-exit arm stays. The 20 ms poll pacing stays (it is
  pacing, not synchronisation).
- `:1086-1090`: `time.After(3*time.Second)` -> `testdeadline.After(3*time.Second)`.

### 3.2 B: the TUI tests (`cmd/aira`)

New file `cmd/aira/tui_sync_test.go` holding the shared helpers (moved out of
`tui_smoke_test.go`, where `waitForSimulationText` and `simulationTextOnUI` live today and
are also used by the board tests):

- `onUI[T any](t *testing.T, runtime *tuiRuntime, f func() T) T`: `go
  runtime.app.QueueUpdate(...)`, wait for the result with
  `testdeadline.After(time.Second)` as a liveness backstop; on expiry `t.Fatal("the tview
  loop did not run a queued update: deadlocked")`. `onUIDraw` is the same with
  `QueueUpdateDraw`, for the reads that need a draw.
- `eventuallyOnUI(t, runtime, budget, cond func() bool, format, args...)`:
  `testdeadline.Eventually` whose condition is evaluated through `onUI`. A slow round
  trip is waited for; only a loop that never answers trips the inner backstop.
- Key barrier. `trackKeys(runtime) *keyTracker` wraps the application's input capture
  (installed by `buildWidgets` / `buildBoardWidgets` / `buildOverviewWidgets` in the
  constructor, `tui.go:301`, `tui_board.go:282`, `tui_board_overview.go:79`) with a
  counter of key events that reached it; it must be called after the constructor and
  before `run()`. `(*keyTracker).press(t, runtime, screen, keys...)` injects the keys,
  waits with `testdeadline.Eventually` until the counter reaches the number of keys
  injected so far, then does one `onUI` round trip, so the last key's handler has
  returned before `press` returns. Keys are FIFO, so this proves every earlier key ran.
- `waitForSimulationText` keeps its signature and its 10 s budget, now
  `testdeadline.Wait(10*time.Second)`, and **fails** with the last screen text on expiry
  instead of returning it silently.

Rules applied to every TUI test file in `cmd/aira` (`tui_smoke_test.go`,
`tui_board_smoke_test.go`, `tui_board_overview_smoke_test.go`,
`tui_board_humanize_smoke_test.go`, `tui_top_test.go`, `tui_execute_test.go`):

1. Every wait for an expected event uses `testdeadline` (`After`, `Wait`, `Eventually`,
   or the helpers above). This covers the initial-draw and fetch-started waits, the quit
   waits (`2-5 s`), the 1 s polls at `tui_smoke_test.go:166/279/289/450/517`, the 1-2 s
   poll in `tui_top_test.go:723`, the board `readBoard*` helpers (`:74-125`), and the
   positive 1 s waits in `tui_execute_test.go`.
2. No `t.Fatal` on a short inner timeout inside a retrying loop: `tui_smoke_test.go:76-90`
   becomes `eventuallyOnUI` reading `state.Panels[viewTickets]` until `Dirty`, then the
   existing `InFlight && Dirty && Generation == 1` assertion; `:517-533` the same for the
   three panel statuses; `tui_top_test.go:723-737` (200 ms inner fatal) the same for the
   top-bar text.
3. Every `InjectKey` whose effect a later read, `QueueUpdate` or key depends on goes
   through `press`; the `time.Sleep` after it is deleted. This includes
   `tabToPaletteSubmit` (press Tab, then read focus), the Enter-at-Cancel at `:257-258`
   before the second `openPaletteEntryOnUI`, the Escape at `:356-363`, the Tab/Enter
   pairs at `:269-275` and `:629-632`, and the board `l`/Down/Home/End/Up/`f`/`/` keys
   (`tui_board_smoke_test.go:143-171`, `:209`, `:252`, `:282-287`, `:312-317`,
   `:396-419`, `:461`, and the same shapes in the overview and humanize files).
4. A "must not happen" check after a key (`y` at `:252-256`, form-submit Enter at
   `:245-251`, Enter at default Cancel at `:257-261`, Space at `:637-641`, `y` with the
   wrong id at `:349-352`) runs after `press` and asserts the synchronous state on the UI
   goroutine: `state.PaletteDispatching == false` and the confirmation still in the
   expected state. The dispatcher-count check stays as a labelled negative wait (`//
   negative wait:` comment; contention can only make it pass, never fail).
5. Screen-text waits that are true before the action are replaced by a state predicate:
   `:642` asserts `state.PaletteConfirm == nil` and no dispatch after Space-at-Cancel.
6. `:293-295` (unbounded receive) -> `onUI`.

The assertions' meaning is unchanged: every test checks the same property, and now fails
only when the property is false or the loop is truly stuck.

### 3.3 C: `internal/gitremote/exec_test.go`

- Test-local type `armedDeadline` implementing `context.Context`: embeds
  `context.Background()` (so `Deadline()` and `Value()` are the background's);
  `Done()` returns a channel closed by `expire()` (once); `Err()` returns
  `context.DeadlineExceeded` after expiry and `nil` before.
- `realRun(ctx, runRequest{..., LiveStdout: w})` where `w` is a small writer that, on the
  first `'\n'`, records the time and calls `expire()`. `realRun` already plumbs
  `LiveStdout` into its stdout drain (`exec.go:41-44`, captured tail first, then live), so
  the pid line is in `result.Stdout` before the deadline fires.
- Backstop: `time.AfterFunc(testdeadline.Wait(5*time.Second), ...)` that marks
  "backstop fired" and expires the context, so a child that never prints still ends the
  subject; the test then fails "pid line never observed", never passes.
- The script gains a fixed `sleep 0.2;` before `trap` (child-side startup delay, a
  fixture, not synchronisation): a regression to a deadline armed at start goes RED on
  every run instead of under load only.
- Assertions: `errors.Is(result.Err, context.DeadlineExceeded)` (unchanged); backstop did
  not fire; two pids parsed; bounded cancellation now measured from expiry:
  `testdeadline.Exceeded(time.Since(expiredAt), 5*time.Second)` fails the test (realRun's
  worst case after expiry is 3 x `processGrace`; the expected path is one 2 s TERM grace
  then KILL, because the trap makes both processes ignore TERM); each pid reaches `ESRCH`
  within `testdeadline.Wait(time.Second)` (`testdeadline.Eventually`), replacing the fixed
  1 s poll (an orphaned `sleep` is reaped by init or a subreaper, a latency the test does
  not own).

### 3.4 D: dropped (section 7)

### 3.5 E: `internal/daemon`

- `server.go`, in the "Test seams" block: `admitManualEvaluation bool`, with a comment:
  test seam; when true a slice queue born in `enqueueAdmitInternal` gets no evaluator
  goroutine, its `stopped` channel is closed at birth, and the test drives every pass;
  production never sets it.
- `admit.go:2003`: `if s.admitManualEvaluation { close(queue.stopped) } else { go
  queue.runEvaluator() }`. Nothing else changes: `queue.signal()` writes into the
  buffered kick channel that nobody reads; `pruneAdmitQueue` closes `stop` as before;
  `stopEvaluator` returns at once on the closed `stopped`.
- Set it in the three constructors used by evaluate-driven tests:
  `exclusiveTestServer` (`admit_exclusive_test.go:143`), `workerExclusiveLedgerServer`
  (`worker_admit_exclusive_test.go:23`), `exclusiveWireServer`
  (`admit_exclusive_identity_test.go:29`). Not in `admitTestServer` (tests such as
  `TestAdmitFIFOOrderAndMidstreamArrival` rely on its live evaluator through `waitAdmitGrant`) and not in
  `startExclusiveTestDaemon` (real daemon; `NewServer` directly).
- `evaluate()`/`stopEvaluator()` keep their bodies; the doc comment of `stopEvaluator` is
  corrected to say the single-evaluator property now holds from the queue's birth for
  servers built by those constructors.

Prototype (same edits in the throwaway worktree): `-race`, 2 CPUs + 6 burners, the 27
exclusive and worker-exclusive tests x 2000: **0/54000**; the 20 ms-gap variant 50/50
pass; whole `internal/daemon` under `-race` once: PASS; the mutation
`sliceProvablyEmpty -> return true` still REDs `TestExclusiveIsNotGrantedWhileALeaseIsHeld`.

### 3.6 F: `internal/runner`

- `runner_linux.go`, `Runner` struct: `quiesceConfirmBound time.Duration`, with a comment:
  test seam; the bound on the emptiness confirmation after the forced `cgroup.kill` in
  `forceDetachedQuiesce`; zero (always, in production) means `r.grace`, today's value.
- `detach_linux.go:646`: `waitEmpty(ctx, scope, confirm)` where `confirm` is
  `r.quiesceConfirmBound` when positive, else `r.grace`. Line 591 is unchanged.
- `detach_linux_test.go:1194`: keep `r.grace = 100 * time.Millisecond` (the wait that
  must expire; a negative wait, contention only makes it more certain) and add
  `r.quiesceConfirmBound = testdeadline.Wait(time.Second)` (the backstop; returns as soon
  as the scope is empty, so it costs nothing on a pass).
- The failure message at `:1197-1198` prints `outcome.err` and `*outcome.record` (today it
  prints two pointers, which is why the witnessed failure carried no diagnosis).

Prototype: plain, 2 CPUs + 6 burners, x 300: **0/300**, wall 108 s (104 s before).

### 3.7 AIRA-199: `cmd/aira/m20b_test.go`

- Pass `dupTestFD(t, readyW)` and `dupTestFD(t, ackR)` (the existing helper,
  `supervisor_relay_store_test.go:51`): the callee owns the duplicates. `defer
  readyW.Close()`, and close `readyW` explicitly right after `runSupervisor` returns and
  before `Decode`, so a supervisor that wrote nothing gives an immediate `EOF` instead of a
  read that blocks while the test still holds a write end.
- Permanent exposure window between building the arguments and calling `runSupervisor`:
  `runtime.GC()` twice, then a 20 ms window for the finalizer goroutine. It can only make
  the wrong shape fail (measured 50/50 EOF); it cannot fail the right one.
- Delete `itoaFD` (`:67`), the only raw-fd hand-off helper in the package.
- The `ackR` duplicate is not closed by the callee on this path (it returns before
  `SuperviseRequest`) and leaks one descriptor per run: accepted, as in
  `supervisor_relay_store_test.go:428`, because closing a raw int the callee may also
  close is this ticket's bug.

Prototype: plain + forced GC 50/50 pass; `-race` x 400: 0/400.

## 4. Invariants

1. No test in scope waits for an expected event on a fixed wall-clock bound: every such
   wait is a `testdeadline` backstop (>= 5 s, scaled x4 under `-race`), and on a passing
   run returns as soon as the event happens.
2. No retrying loop contains a short inner `t.Fatal`.
3. No `time.Sleep` orders a test action after an injected key or an asynchronous effect;
   a barrier or a predicate does. The only remaining sleeps in scope are labelled
   negative waits or poll pacing.
4. A negative wait can only false-pass under contention, never false-fail.
5. A fixture duration the test needs to expire is never also a backstop it needs not to
   expire (F; C's subject deadline is armed only after its precondition).
6. A test never hands a callee the number of a descriptor whose `*os.File` it still owns.
7. A test that drives admission passes by hand is its queue's only evaluator from the
   queue's birth.
8. Unevaluated stays unevaluated: an environment that cannot run a real-cgroup test
   skips (or fails under `AIRA_REAL_CGROUP=1`) as today; a failure after the environment
   is established (A: scope listed but never placed) is a hard failure.
9. With both seams unset, production behaviour is unchanged (same call, same bound).
10. Every test still fails against the wrong implementation (section 5 mutations).

## 5. Tests (TDD; each must fail against the wrong implementation)

Each new or changed test is first shown RED: against today's test shape via the
deterministic variant named here, or against the named mutation. Record the RED and the
GREEN (exit code and counts) in the PR.

**A.** `TestConfineKillReadinessRequiresAPopulatedRecordForTheExactName` (unit, no
cgroup): outputs built with the production encoder (`toolResponse(1, core.Response{OK:
true, Code: "OK", Data: runner.ConfineListResult{...}})`, then `json.Marshal`), cases:
no record -> not listed; ours with `subtree_populated` nil -> listed, not placed; ours
false -> not placed; neighbour `mcp-e2e-x-other` populated true and ours false -> not
placed; ours true -> placed; two records named ours -> error; garbage -> error.
Mutations: today's substring predicate (nil and false cases RED); `strings.Contains`
name match (neighbour case RED); nil read as true (RED); duplicates ignored (RED).
The e2e keeps its shape; under load it must stay 0/40 (it was 0/40 before, so the load
run is a no-regression check, not evidence of the fix).

**B.**
- `TestTUIKeyBarrierReturnsOnlyAfterTheHandlerRan`: block the UI loop with a
  `QueueUpdate` that signals it started and then waits on a channel released by a
  goroutine 50 ms later; `press` one key; immediately after `press` returns, the tracker
  counter is 1. Mutation: `press` that only injects -> counter 0 (RED, deterministic: the
  loop is blocked).
- `TestTUIUIReadsTolerateASlowLoop`: block the loop for 300 ms (after the
  started-handshake), then run the `TestTUIKeypressAndQuit...` probe through
  `eventuallyOnUI`. Today's probe shape (100 ms inner fatal): RED (17/20 measured without
  the handshake; 20/20 expected with it).
- `waitForSimulationText` on a needle that never appears fails with the screen dump.
  Mutation: return silently (RED; this is what let the panic through).
- Mutations against the product, each must RED: `r` no longer marks the panel dirty
  (`TestTUIKeypressAndQuit...`); `y` dispatches from the confirmation (confirmation test:
  `PaletteDispatching` true after `press`); Escape does not cancel the destructive
  confirmation; Space at default focus dispatches; board Home does not select the first
  card (`TestBoardHomeEndUpdateSelection`).
- Load: the four-test set (`^(TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight|TestTUIRendersTruncatedErrorAndUnevaluatedStates|TestTUIPaletteOpensOnColon|TestTUIConfirmationRequiresFocusedConfirmEnterAndDispatchesOnce)$`)
  x 100 and the whole TUI set x 20, both `-race`, 2 CPUs + 6 burners: before 9/400 and
  6 + a package panic; after 0 failures, no panic.

**C.** The rewritten `TestRealRunTimeoutKillsProcessGroup` (with the permanent pre-trap
delay) is RED against today's shape (deadline armed at start): 10/10 measured. Mutations
of `realRun` (`exec.go`), each RED: TERM/KILL sent to the leader pid instead of the
process group (`sleep` survives: ESRCH wait fails); no SIGKILL escalation (both ignore
TERM: ESRCH wait fails); return the wait error instead of `ctx.Err()` (`errors.Is`
fails); `LiveStdout` not plumbed to the drain (backstop fires: "pid line never
observed"). Load: before 1/60; after 0/60.

**E.**
- `TestManualEvaluationQueueIsBornWithoutAnEvaluator`: with `admitManualEvaluation`,
  enqueue an exclusive waiter on an empty slice; a labelled 50 ms negative wait; the
  waiter is still `admitQueued`, not granted; `stopEvaluator` returns at once; one
  `evaluate` grants it. Mutations: ignore the flag (the background pass grants it within
  the window: RED; under extreme load this can only false-pass); start the evaluator but
  close `stopped` (RED, same reason); neither (`stopEvaluator` waits out its 5 s+
  backstop: RED).
- Existing: `sliceProvablyEmpty -> return true` still REDs
  `TestExclusiveIsNotGrantedWhileALeaseIsHeld` (checked on the prototype).
- Deterministic: the 20 ms-gap variant: before 50/50 RED, after 50/50 GREEN.
- Load: `-race`, 2 CPUs + 6 burners, `^TestExclusive` x 2000: before 9/24000; after 0.
  Then the whole `internal/daemon` package once under `-race`.

**F.**
- `TestForcedQuiesceConfirmationUsesItsOwnBound` (no real cgroup): a memory runner and a
  fake scope that reports populated until its `Kill()` has been called and then for 3
  more `Empty()` reads; `r.grace = time.Nanosecond`; `r.quiesceConfirmBound =
  testdeadline.Wait(time.Second)`; call `forceDetachedQuiesce` directly. It returns
  `(nil, nil)` and the ledger holds `quiesce-forced` with `ScopeKill.Completed`.
  Mutation: ignore the seam (confirm with `r.grace`) -> `U_RUN_CAPTURE_INCOMPLETE` (RED;
  the 1 ns deadline wins against the 10 ms tick on all but a vanishing fraction of
  runs).
- The real-cgroup test keeps every existing assertion (status EXITED, leader exit 0
  preserved, `QuiesceForced`, `U_RUN_QUIESCE_FORCED`, not clean, never
  `ScopeDescendantKilled`). Load: before 3/200 and 6/300; after 0/300.

**199.** The forced-GC exposure window makes the test RED against the old shape on every
run (50/50 measured). Mutation: pass `readyW.Fd()` again -> RED. Load: `-race` x 400:
before 8/400; after 0/400.

**Gate.** `aira confine -- make ci` (the gate's exact command), then `go test -race` on
`cmd/aira`, `internal/daemon`, `internal/runner`, `internal/gitremote`. The load runs
above, one at a time, each with `aira confine --name <unique>`, recording `rc`, pass,
fail and the failure lines, before and after, in the PR.

## 6. Expected yield

| Flake | Before (measured) | Expected after |
|---|---|---|
| E exclusive | 9/24000 (`-race`, 2 CPUs + 6 burners); witnessed once in `make ci` on 2026-10-10 | 0 (prototype 0/54000) |
| F M20 forced quiesce | 3/200, 6/300 (2 CPUs + 6 burners); witnessed once on 2026-10-10 | 0 (prototype 0/300) |
| 199 m20b | 8/400 (`-race`) | 0 (prototype 0/400 with forced GC) |
| B TUI | 9/400 + a package-killing panic (`-race`, 2 CPUs + 6 burners) | 0 failures, no panic |
| C gitremote | 1/60 (2 CPUs + 6 burners) | 0 |
| A MCP kill | 0/40 (not reproduced; three historical sightings) | 0; the race window is closed by construction |

The merge gate stops refusing correct pushes for these six classes; one TUI failure can
no longer take down every `cmd/aira` test with it; and the M20 failure, if it ever
recurs, prints its error instead of two pointers. Cost on a passing run: C +0.2 s; the
others unchanged (the backstops return as soon as their event happens).

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| D (`TestRealCgroupDynamicCharge...`) | Its test and feature were deleted in `9a2f65c`. Dropped from AIRA-204. |
| `TestCLIRunRealCgroupOrClearSkip` `bad file descriptor` (AIRA-199 amendment) | Not reproduced: interleaved with m20b, `-race`, 300 rounds -> 0/600. The fix removes the only known in-package closer of a test-owned descriptor, so the attribution is plausible but **unevaluated**; it is not claimed fixed. |
| Production finding: `cmd/aira/main.go:660-664` | With an unparseable `--ready-fd` (or none), `strconv.Atoi` returns 0 and `writeSupervisorFailure(readyFD, ...)` writes to and **closes fd 0**; it should use `readyForFailure` (`supervisorReadyFD` returns -1 for an unparseable value). No test reaches it in-process today. A product bug, not a flake; suggest a ticket. |
| Phantom `outstandingJobs = 1` setups in the exclusive tests | The S14 comments prefer real leases (`seedRunningLease`). With E's seam they are no longer racy; converting them is a test-quality change, not a flake fix. |
| Other short-grace real-cgroup fixtures in `internal/runner` (`memory_cap_linux_test.go:294` grace 100 ms, `livetee_test.go:507` grace 200 ms) | Same single-knob shape is possible; not witnessed; production grace is 2 s. Recorded. |
| Raw waits in `cmd/aira` outside the TUI tests and test A (`dispatcher_inprocess_test.go` others, `watch_test.go`, `drain_hold_test.go`, `confine_test.go`) and in `internal/store`, `internal/install`, `internal/core` | Not witnessed; AIRA-204 records them as not scheduled. |
| A lint guard against raw `time.After` | Decided against in AIRA-204. |

## 8. Risks

- **Production seams (E, F).** Two fields and two `if`s in `internal/daemon/admit.go` and
  `internal/runner/detach_linux.go` (declarations in `server.go` and `runner_linux.go`).
  Unset in production; the default branch is today's statement. Route these four files
  to a separate review pass: check that production never sets either field (grep), that
  the default path is unchanged, and that `admitManualEvaluation` cannot leave a
  production queue without an evaluator.
- **E coverage.** A test built on one of the three constructors that relied on a
  background grant would now wait for its backstop and fail loudly (not silently). The
  prototype ran the whole daemon package under `-race` once with no failure.
- **Key barrier (B).** It depends on the application-level input capture installed at
  construction. If a future change re-installs the capture after the tracker wraps it,
  `press` never completes and fails on its backstop (loud). The barrier pin test covers
  the helper itself.
- **Stricter TUI waits.** `waitForSimulationText` now fails on timeout; a caller that
  relied on the silent return will fail where it used to carry on. That is intended (it
  is how the nil-form panic happened); expect a few call sites to need a correct needle.
- **Time to report a real hang.** Backstops move from 0.1-3 s to >= 5 s (x4 under
  `-race`). A genuine deadlock is reported a few seconds later; the AIRA-20 trade,
  already accepted. `make ci`'s 20-minute package timeout is unaffected.
- **Measurement load on a shared box.** Load runs pin burners to two CPUs, run one at a
  time, under `aira confine`, and are stopped with `aira confine --kill <name>`.

## 9. Measurement reproduction

```bash
cd <worktree>
mkdir -p ~/tmp/aira-flakes/bin
aira confine --name <unique> -- go test -race -c -o ~/tmp/aira-flakes/bin/daemon.race.test ./internal/daemon
aira confine --name <unique> -- docs/dev/aira204-loadloop.sh excl-race "$PWD/internal/daemon" 2,3 6 \
    ~/tmp/aira-flakes/bin/daemon.race.test '^TestExclusive' 2000
```

Same pattern for each row of section 6: `cmd/aira` (`aira.test`, `aira.race.test`),
`internal/runner` (`runner.test`), `internal/gitremote` (`gitremote.test`), with the
regex, CPU list, burner count and count given there. The deterministic variants (20 ms
gap, pre-trap sleep, 300 ms UI stall, forced GC) are one-line edits of the test named in
section 5.
