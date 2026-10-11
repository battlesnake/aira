# AIRA-204 + AIRA-199: remove six merge-gate flake classes

Status: PLAN v1.3 (2026-10-11; v1.2 applied plan review 1, v1.3 applies plan review 2,
both in section 10; section 10.3 lists what the build already on this branch must change
to match). Test hardening, one change. Tickets: AIRA-204 (P1, four
witnessed flakes, each a different defect class) and AIRA-199 (P2, a test hands a callee
its own descriptor). Two flakes seen this week join them: `internal/daemon`
`TestExclusiveIsNotGrantedWhileALeaseIsHeld` and `internal/runner`
`TestM20RealForcedQuiescePreservesLeaderExitAndIsNotClean`. Base: master `d13ef3b`. Both
tickets report `ready:true`.

**Production code: four files, three changes.** (1) E: one test seam, unset in
production (`internal/daemon/admit.go` + `server.go`). (2) F: the post-kill emptiness
confirmation in `internal/runner/detach_linux.go` gets its own named 2 s bound instead of
reusing the configurable grace; identical in production, where the grace is always 2 s
(no seam). (3) B: `cmd/aira/tui.go` makes the destructive-confirmation form one row
taller, a **visible product fix**: today the form is one row short and, with focus on
Cancel, the "Type <ID>:" input is scrolled out of view (section 1, B). Neither E nor F is
a product race (section 1). All four files get their own review pass (section 8).
Everything else is `_test.go` plus one committed measurement script.

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
| A | `TestMCPConfineKillOutsideProjectKeepsOwnershipAndStealChecks` (`cmd/aira/dispatcher_inprocess_test.go:1018`) | Readiness poll on the wrong signal | **Not reproduced**: 0/40 (plain, 2 CPUs + 6 burners). By code: the poll at `:1057-1077` breaks when the list output contains the name and `session-a`, both decoded from the scope id, which is listed before the child is placed (as a `Pending` row from the daemon's admit registry before the scope is on disk, `internal/runner/confine_shim_manage.go:21`, then as the on-disk directory); the `--steal` kill at `:1082` then gates on `cgroup.events` (`internal/runner/confine_manage_linux.go:766-770`, `U_CONFINE_NOT_LAUNCHED ... retry`). The no-steal call at `:1078` is refused at `:729-731` before that gate. Three historical sightings (RANT-9, RANT-27, AIRA-35). | Build. Evidence is a predicate unit test plus code reading, not a load rate |
| B | `tui_smoke_test.go` (ticket names `TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight`), and the same shapes across the TUI tests | Three classes: (1) a short inner `t.Fatal` nested inside a retrying poll; (2) fixed sub-second backstops on expected events; (3) `time.Sleep` used to order an injected key before a later UI read | **Yes.** `-race`, 2 CPUs + 6 burners, 4 TUI tests x 100: **9/400 fail**: `:87 keypress deadlocked` x4, `:529 renderer update queue deadlocked` x1, `:297 read dispatch count=0` x2, `:272 confirmation default focus is not Cancel (Confirm=false Cancel=false app=*tview.Button)` x2. Whole TUI set (`^(TestTUI|TestBoard|TestTop|TestOverview|TestPump)`), 20 rounds: 6 failures (`tui_board_smoke_test.go:156/162/168` "Home/Down/End" x4, `:87` x2) and then **a panic that killed the whole `cmd/aira` binary**: nil `paletteConfirmForm` at `tui_smoke_test.go:341`, reached because `waitForSimulationText` (`:558-573`) returns silently on timeout. Deterministic: one 300 ms UI stall queued before the probe -> 17/20 fail at the `:87` site. | Build |
| C | `TestRealRunTimeoutKillsProcessGroup` (`internal/gitremote/exec_test.go:15`) | The subject's own 50 ms timeout is also the whole budget for the test's precondition (sh must start, trap TERM, fork, print two pids) | **Yes.** 1/60 (plain, 2 CPUs + 6 burners): `exec_test.go:30: pid output=""`. Deterministic: `sleep 0.2;` before the trap -> 10/10 fail. | Build |
| D | `TestRealCgroupDynamicChargeTracksMemoryCurrentAndFreesAWaiter` (`internal/daemon`) | Unsettled fixture | **Moot.** The file was deleted with the feature in `9a2f65c` (admission-counter S1: delete the AIRA-29 dynamic live-charge). | Drop |
| E | `TestExclusiveIsNotGrantedWhileALeaseIsHeld` and 10 siblings (`internal/daemon/admit_exclusive_test.go`, `worker_admit_exclusive_test.go`) | The test seeds its precondition while the queue's own evaluator goroutine is live | **Yes.** Witnessed `~/tmp/batch-final-ci.log:11-12` (2026-10-10 14:30): `admit_exclusive_test.go:317: ... expected the waiter to stay queued, got state=1 outcome="immediate"`. `-race`, 2 CPUs + 6 burners, `^TestExclusive` x 2000: **9/24000** across 4 tests (`:165` x6, `:179`, `:224`, `:317`), all `outcome="immediate"`. Plain, 16 CPUs, no burners: 2/36000. Deterministic: a 20 ms gap between the first enqueue and `seedRunningLease` -> 50/50 fail. | Build |
| F | `TestM20RealForcedQuiescePreservesLeaderExitAndIsNotClean` (`internal/runner/detach_linux_test.go:1192`) | One fixture duration plays two roles: the wait the test needs to EXPIRE and the backstop it needs NOT to expire | **Yes.** Witnessed `~/tmp/batch-final-ci2.log:18-19`: `detach outcome={record:0xc0002b9b08 err:0xc000387300}` (pointers; the message hides the error). Plain, 2 CPUs + 6 burners: 3/200. A diagnostic build printing the error: 6/300, every one `U_RUN_CAPTURE_INCOMPLETE: scope did not become empty`. | Build |
| 199 | `TestM20bSupervisorRejectsMalformedSidecarBeforeOpeningProject` (`cmd/aira/m20b_test.go:14`) | Test passes `readyW.Fd()` to a callee that closes it; `readyW`'s finalizer later closes the number again | **Yes.** `-race`, 16 CPUs, `^TestM20b` x 400: **8/400** `m20b_test.go:54: EOF`. Deterministic: two `runtime.GC()` calls plus a 20 ms window between building the arguments and `runSupervisor` -> 50/50 EOF. Cross-test probe (the ticket's "third instance", `TestCLIRunRealCgroupOrClearSkip` failing `fork/exec /usr/bin/git: bad file descriptor`): interleaved with m20b, 300 rounds, -race -> 0/600. | Build; the CLIRun attribution stays unevaluated |

### Mechanisms, established

- **A.** Admission is granted -> list shows the name and owner (first as a `Pending`
  registry row, `mergeConfineRegistry`, with `populated` named unevaluated and no
  directory on disk; then from the scope directory once `confine_linux.go:831` creates
  it) -> poll breaks -> the child is not yet in `cgroup.procs` -> the steal kill returns
  `U_CONFINE_NOT_LAUNCHED`. So "listed" proves admission, not that the scope exists and
  not that containment works. The record field that answers "placed?" already exists:
  `ConfineRecord.SubtreePopulated` (`confine_manage.go:62`), read from the same
  `cgroup.events` source the kill gate uses, `nil` when unestablished (always `nil` on a
  `Pending` row and on an on-disk scope whose `cgroup.events` cannot be read).
- **B(3).** tview's loop is one `select` over `a.events` (keys) and `a.updates`
  (`QueueUpdate`) (`tview@v0.42.0/application.go:391-510`). Keys are FIFO among
  themselves; a key and a later `QueueUpdate` are NOT ordered. A test that injects a key,
  sleeps 20 ms, then reads state through `QueueUpdate` can read the state from before the
  key. The `:272` and board `:156/162/168` failures are reads of exactly that kind.
  The panic is established from its stack: `checkDisabled` (`:341`) dereferenced a nil
  `paletteConfirmForm` on the UI goroutine after `waitForSimulationText("Type RANT-7")`
  returned without the text. Two separate facts sit behind it:
  - **The needle was never on screen, on any run.** Every passing run of
    `TestTUIDestructiveConfirmationRequiresExactResolvedID` on `d13ef3b` took 10.28-11.39 s
    (11 runs, `~/tmp/aira-flakes/runs/tuiall-before-race.log`) against 0.44-1.21 s after
    the change (20 runs, `after-B-tuiall-race.log`): the 10 s wait expired silently every
    time. Cause (found by the build once the wait failed loudly, commit `e5c1223`):
    `showPaletteConfirmation` (`tui.go:1110`) sizes the form `GetFormItemCount()+5` rows,
    one short of border 2 + padding 2 + item + gap + buttons; with focus starting on
    Cancel, tview scrolls the form and the "Type <ID>:" row is not drawn. A product bug
    (the operator cannot see the field they must fill), fixed with `+6`. So plan v1.2's
    explanation (an ordering race made the text late) was wrong: the wait never
    synchronised anything.
  - **The form was nil: the confirmation was never built.** Enter did not reach Submit.
    The likely route (inferred, not traced) is the key/read ordering above:
    `tabToPaletteSubmit` (`:429-446`) reads focus, Tabs, sleeps, reads again; a read that
    overtakes its Tab sends a second Tab, the stale Tab then moves focus off Submit, and
    Enter lands elsewhere. With the old silent wait the test reached the dereference
    10 s later regardless.
- **E.** `enqueueAdmitInternal` creates a queue, starts `go queue.runEvaluator()`
  (`admit.go:2003`) and kicks it (`queue.signal()`, `:2154`). The evaluator grants the
  exclusive waiter on an empty slice before the test calls `seedRunningLease` or sets
  `outstandingJobs = 1`. `evaluate()` (`admit_exclusive_test.go:97-101`) retires the
  goroutine, but only from its first call, which is after the seeding.
  `pruneAdmitQueue` (`admit.go:2718`) deletes an emptied queue, so the next enqueue
  births a fresh queue with a live evaluator: `TestRepeatedExclusiveRequestAndAbandonNeverWedgesTheSlice`
  case 1 re-enters the race on every round.
- **F.** The detached quiesce path uses `r.grace` twice: the natural-quiesce wait in the
  caller (`detach_linux.go:591`, which must expire so `forceDetachedQuiesce` runs) and the
  post-`cgroup.kill` emptiness confirmation inside `forceDetachedQuiesce` (`:646`, which
  must not expire). The test sets
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
- **F:** give the "must expire" wait and the "must not expire" bound separate values.
- **199:** give the callee a descriptor it may own; close your own ends yourself.

The concrete minimum: an exact-name population predicate (A); one bounded UI-read helper
and a key-completion barrier (B); cancellation armed after the pid line (C); one evaluator
per queue (E); separate expire/confirm bounds (F); explicit descriptor ownership (199).
The key barrier at its minimum is one sentinel and one acknowledgement: `press` is only
ever called serially, so v1.2's per-press sequence numbers and waiter map are dropped
(section 3.2). Three parts of the plan go beyond the minimum and are chosen, not required:

- **B's breadth.** The minimum converts the witnessed sites (`tui_smoke_test.go` `:87`,
  `:272`, `:297`, `:341`/`tabToPaletteSubmit`, `:529`, and the board `:156/162/168`).
  The plan applies the same rules to every same-shape site in the six TUI files, because
  the class is identical and, with the helpers in place, each conversion is a one-line
  change; leaving known instances of a measured class costs a later batch.
- **The load matrix.** The minimum evidence is a deterministic RED/GREEN per item. The
  before/after load runs are corroboration of the measured rates, not the proof.
- **C's permanent `sleep 0.2` before the trap.** Regression scaffolding: without it, a
  regression to a deadline armed at start is RED only under load. Kept: it is the only
  thing that makes that regression RED on every run, for 0.2 s per run.

E's seam versus a granted "keeper" lease is a real trade (table below): the keeper needs
no production line, the seam needs fewer fixture edits and removes the class at its
source. The plan keeps the seam (one bool, default branch is today's statement). It is
the only test seam in production code; F needs none (section 3.6).

| Item | Alternative | Why rejected |
|---|---|---|
| A | Retry the kill on `U_CONFINE_NOT_LAUNCHED` | Legitimate (the message says "retry") but it waits on the error rather than the fact, and a retry loop around the subject can hide a kill gate that never opens. Waiting for `subtree_populated` asserts the precondition, then calls the subject once. |
| A | Substring `subtree_populated":true` in the raw output | The test launches into the real machine-wide `aira.slice`; a neighbour's populated scope satisfies it. Parse and match the exact name. |
| B | Just raise the 100 ms / 1 s constants | Leaves the nested-fatal and key-ordering classes in place; the panic case is an ordering bug that no constant fixes. |
| B | Delete the inner timeout arm | Leaves an unbounded receive: a real deadlock becomes a 20-minute package hang. The arm stays, as a liveness backstop. |
| B | Replace the inner `t.Fatal` with `continue` (the ticket's proposal) | Spawns a fresh `QueueUpdate` goroutine every 100 ms on a slow loop and loses the "deadlocked" diagnosis. One wait per read with a generous backstop is simpler and keeps the diagnosis. |
| B | A key barrier that counts keys reaching the capture and waits for "count == keys sent through `press`" (plan v1) | Porous: any key injected outside `press` (typed text at `tui_smoke_test.go:240-242`, `:264-266`, `:325-327`, `:626-628`, the repeat Enters at `:276-278`, board `:285`, `:315`) inflates the count, so a later `press` can return before its own key ran. It also needed a final `onUI` round trip to prove the handler (not just the capture) had returned. The sentinel barrier (section 3.2) proves both by FIFO order with no counting. |
| B | Per-site screen-text predicates instead of a key barrier | Several texts are true before and after the key, so the wait passes vacuously. Example, from the code (not run): after Space-at-Cancel at `:637-642`, `paletteCancelAction` shows the palette LIST, not the dashboard (`tui.go`, `showPaletteList`), and the tab row holding `"1:Tickets"` stays outside the centred 80x22 page on the 110x36 screen, so the `:642` wait passes whether or not the modal closed. A barrier proves the key was handled; the assertion then reads state. |
| C | `context.WithCancel` / `WithCancelCause(ctx, DeadlineExceeded)` armed on the pid line | `realRun` returns `ctx.Err()`; both give `context.Canceled`, so `:22`'s `errors.Is(..., DeadlineExceeded)` goes red every run. A test-local context whose `Err()` is `DeadlineExceeded` keeps the assertion meaning "realRun returns the context's error". |
| C | Raise the 50 ms | Any fixed value is still the precondition's whole budget. |
| E | Rewrite the 11 affected tests to establish the precondition first (a granted "keeper" lease before the exclusive request, as `admit_exclusive_identity_test.go` already does) | No production change, and it would replace the phantom `outstandingJobs = 1` scalars with real leases. But it is 11 test rewrites, several of which change what the test exercises (every round of `TestRepeatedExclusiveRequestAndAbandonNeverWedgesTheSlice` case 1 would need its own keeper), and nothing stops the next evaluate-driven test from reintroducing the race. The seam removes the class at its source in one place. Kept as a deferral (real leases instead of phantom scalars), not needed for the flake. |
| E | Pre-register an evaluator-less queue from test code | `pruneAdmitQueue` deletes it when it empties, and the next enqueue births a live one; it also duplicates the queue constructor in test code. |
| F | `r.grace = testdeadline.Wait(r.grace)` (the precedent at `detach_linux_test.go:1080`) | Phase 1 always waits the full grace (the `sleep 30` never exits), so every run costs >= 5 s, 20 s under `-race`. |
| F | `r.grace = time.Second` (the fixture default) | +0.9 s per run, and a fixed unscaled 1 s backstop on a kernel latency whose measured maximum is already 240 ms on 2 CPUs: the same class with a wider margin. |
| F | A test seam `quiesceConfirmBound` on `Runner` (plan v1.2; built in `7890531`), set by the test to `testdeadline.Wait(time.Second)` | Production would carry a field only tests set, and the dual role would stay in the product (the confirmation still defaults to the configurable grace). A named constant at the one site gives identical production behaviour (production never sets `Config.Grace`, so `r.grace` is always 2 s: `runner_linux.go:188-190`, only `runner.Config{}` literal outside tests is `internal/app/project.go:305`, no `Grace`), removes the seam, and removes the dual role in the product. Cost: the real-cgroup test runs on the product's unscaled 2 s bound (8x the measured 240 ms maximum); a kernel slower than that fails production identically and is reported honestly. Plan review 2 (Sol P2) |
| F | Pass `0` and rely on `waitEmpty`'s `timeout <= 0 -> 2 s` default (`cgroup_linux.go:396-397`) | Same behaviour, but a magic zero hides the decision; a named constant says it |
| F | Use one post-kill bound for every `waitEmpty` site in the runner | Not witnessed elsewhere; production uses 2 s everywhere. Minimal change: the one site this test exercises. |
| A | Treat every listing that names no record (`{}`, a JSON-RPC `error`, any `ok:false`) as `absent` (built in `e5c1223`) | Porous: a broken MCP reply then never exercises ownership or the kill and ends as an environmental skip. Only identified environmental answers are unevaluated (section 3.1); anything else fails. Plan review 2 (High) |
| 199 | `runtime.KeepAlive(readyW)` | Measured insufficient in the ticket (1 failure in 400) and does nothing for the later cross-test close. |
| all | A lint/CI guard against raw `time.After` | AIRA-204 already decided against it (about half the raw sites are correct negative waits; the allowlist is the complexity). |

## 3. Design

### 3.1 A: `cmd/aira/dispatcher_inprocess_test.go`

- New test helper `confineKillReadiness(output []byte, name string) (confineReadiness, runner.ConfineRecord, error)`:
  decode and **validate** the MCP `tools/call` envelope before reading any record. The
  envelope is the production encoder's (`toolResponse`, `cmd/aira/mcp.go:643`): `jsonrpc`
  `"2.0"`, a `result` holding `structuredContent` (a `core.Response`: `ok`, `code`,
  `data`). The reply is classified by its `code`, and only answers the code base already
  names as environmental are unevaluated:

  | Reply | Where it comes from | Result |
  |---|---|---|
  | `ok:true`, `code:"OK"`, `data.scopes` a JSON array (possibly empty) | a listing was produced | classify records (below) |
  | `ok:true`, `code:"UNEVALUATED"` (`data.verdict:"unevaluated"`) | slice directory unreadable (`confine_manage_linux.go:169-171`), ci-shim with no daemon (`dispatcher.go:258-266`), non-Linux (`confine_manage_stub.go:15-16`); returned by the daemon (`internal/daemon/confine_manage.go:156-157`) and the daemon-down fallback (`dispatcher.go:290-291`) alike | `readinessUnlistable` |
  | `ok:false`, `code:"E_CONFINE_UNAVAILABLE"` | the slice cannot be resolved (`confine_manage_linux.go:62-67`, `internal/daemon/confine_manage.go:124`) | `readinessUnlistable` |
  | `ok:false`, `code` one of `E_DAEMON_UNAVAILABLE`, `E_DAEMON_TIMEOUT`, `E_DAEMON_BUSY` | a transport failure between the client and the test's private daemon (`transportErrorResponse`, `dispatcher.go:214-215`, `:688-694`); transient under load, so fatal would be a new false failure | `readinessUnlistable` (the poll retries; only a persistent one reaches the expiry, as unevaluated) |
  | anything else: undecodable, `{}`, a JSON-RPC `error` member, no `result.structuredContent`, an empty `code`, any other code, `OK` without a `scopes` array | not an answer this test knows to be environmental | `err` |

  The helper's caller also treats a non-zero `runMCPWithDispatcher` exit as `err` (today
  the poll discards it with `_ =`). For an `OK` reply, collect the records whose
  `Name == name` (exact equality). More than one -> `err` naming the count (ambiguous;
  the kill selector would be refused `E_SELECTOR_AMBIGUOUS`). Otherwise one of five
  record states, kept distinct because they mean different things (with
  `readinessUnlistable` below them all, six states ordered by strength):

  | State | Record | Means |
  |---|---|---|
  | `readinessUnlistable` | (no listing) | the environment cannot list confines |
  | `readinessAbsent` | none | not admitted (or already gone) |
  | `readinessPending` | `Pending: true` | admitted; the scope is not on disk |
  | `readinessUnreadable` | on disk, `SubtreePopulated == nil` | the scope exists; population not established |
  | `readinessEmpty` | on disk, `*SubtreePopulated == false` | the scope exists and is readable; observed empty |
  | `readinessPlaced` | on disk, `*SubtreePopulated == true` | ready: the kill's gate is open |

  Only `readinessPlaced` breaks the poll. `nil` is never read as placed, and `Pending` is
  never read as on disk.
- The poll at `:1057-1077`: same loop, now on `confineKillReadiness`; `err` -> `t.Fatal`.
  The `done` arm stays and is the environment probe: a launch that returns an error means
  the environment could not run a contained job -> `cgrouptest.SkipOrFailRealCgroup`
  (unevaluated, as today); a launch that returns nil -> `t.Fatal` (exited before the
  kill, as today). Backstop `testdeadline.Wait(5*time.Second)`. Order within each
  iteration: list and classify; break on `readinessPlaced`; a non-blocking receive on
  `done`; then the deadline check. On expiry, **receive from `done` once more
  (non-blocking) before deciding**, so a launcher error that lands between the earlier
  receive and the deadline check is still read as the environment probe it is, never as
  a product failure. Then the decision uses the strongest state seen during the poll,
  not "listed":
  - `readinessEmpty` was observed -> `t.Fatalf` with that record. Containment capability
    is established separately from listing: the scope directory exists, its
    `cgroup.events` is readable, and the launcher (which returns an error when it cannot
    place the child) has not failed. A job that never enters a working scope is then a
    product failure. (Limit: a scheduling stall longer than the scaled backstop between
    scope creation and child placement would also land here; section 4.)
  - only `readinessUnlistable`, `readinessAbsent`, `readinessPending` or
    `readinessUnreadable` -> `cgrouptest.SkipOrFailRealCgroup` naming the last state
    (containment was never established: unevaluated, not a product verdict).

  The 20 ms poll pacing stays (it is pacing, not synchronisation).
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
- Key barrier (sentinel). `trackKeys(runtime) *keyTracker` wraps the application's input
  capture (installed by `buildWidgets` / `buildBoardWidgets` / `buildOverviewWidgets` in
  the constructor, `tui.go:301`, `tui_board.go:282`, `tui_board_overview.go:79`); it must
  be called after the constructor and before `run()`. `(*keyTracker).press(t, screen,
  keys...)` injects the keys, then injects one sentinel key through the SAME path
  (`screen.InjectKey`, never `app.QueueEvent`, which would bypass the screen's queue and
  could overtake keys still in it): `tcell.KeyRune` with one fixed supplementary
  private-use rune (`U+F0000`). The wrapper recognises the sentinel, sends on the
  tracker's acknowledgement channel (`chan struct{}`, buffer 1) and returns `nil` (the
  sentinel never reaches a primitive); every other key goes to the wrapped capture
  unchanged. `press` waits for that acknowledgement with a
  `testdeadline.After(time.Second)` backstop ("the tview loop did not reach the key
  barrier: deadlocked"). No sequence numbers and no waiter map (plan v1.2 had both;
  built in `864aefa`): `press` and `barrier` are called serially from one goroutine at a
  time (the pin's `press` runs in a goroutine, but nothing else presses meanwhile), each
  waits for its own sentinel before returning, so at most one sentinel is ever in
  flight; a `press` that times out fails the test, so no stale acknowledgement can be
  read by a later one. The doc comment states "not safe for concurrent use". Why this proves completion: screen events
  are FIFO into `a.events`, and tview's loop (`tview@v0.42.0/application.go:391-450`)
  runs one key event to completion (capture, root handler, draw) before taking the next.
  So when the sentinel reaches the capture, every key injected before it, by `press` or
  by a raw `InjectKey`/`InjectKeyBytes`, has had its handler return. No counting, and no
  dependence on which keys went through `press`.
  Keys that stop or suspend the loop (`q`, the execute launcher's run/confirm that
  suspends) are injected raw and followed by the existing `done` (or resume) wait: a
  sentinel after them would never be processed and its backstop would report a false
  "deadlocked".
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
3. Every `InjectKey` whose effect a later read or `QueueUpdate` depends on is followed by
   a barrier before that read: it goes through `press`, or (typed text) is injected raw
   and covered by the next `press` or by `tracker.barrier(t)` (a sentinel with no keys),
   since the sentinel proves every earlier key ran. The `time.Sleep` after it is
   deleted. This includes
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
only when the property is false, the loop is truly stuck, or a stall outlasts the scaled
backstop (>= 1 s inner round trip floored to 5 s, x4 under `-race`; section 4).

7. **The destructive-confirmation form height (production, `tui.go:1110`).**
   `TestTUIDestructiveConfirmationRequiresExactResolvedID` waits for the "Type RANT-7"
   field itself after the form is built (an `eventuallyOnUI` on `paletteConfirmForm !=
   nil`, then `waitForSimulationText`). With the now-loud wait it is expected to fail on
   every run against `GetFormItemCount()+5` (the base timings in section 1 show the text
   never appeared; the build reports the same in `e5c1223`, not re-run for this
   revision) and to pass with `+6`. That is the pin for the product fix (mutation: `+5`
   -> RED; the builder records the counts; section 5).

### 3.3 C: `internal/gitremote/exec_test.go`

- Test-local type `armedDeadline` implementing `context.Context`: embeds
  `context.Background()` (so `Deadline()` and `Value()` are the background's);
  `Done()` returns a channel closed by `expire()` (once); `Err()` is derived from that
  same channel (`select { case <-done: return context.DeadlineExceeded; default: return
  nil }`), never from a flag written after the close: `realRun` reads `ctx.Err()`
  after `<-ctx.Done()` fires (`exec.go:51` waits on it, `:63` reads `ctx.Err()`), so `Err()` must be non-nil the
  instant `Done()` is closed. **`expire(cause)` records which event expired the context**:
  inside the same `once` that closes the channel it stores the cause (`"pid line"` or
  `"backstop"`) and the expiry time, so exactly one cause wins and `expiredAt` is the
  instant of the actual expiry, not of a later event.
- `realRun(ctx, runRequest{..., LiveStdout: w})` where `w` is a small writer that, on the
  first non-blank live write (a complete pid line: the live face sits behind
  `lineRedactor`), calls `expire("pid line")`. `realRun` already plumbs `LiveStdout` into
  its stdout drain (`exec.go:41-44`, captured tail first, then live), so the pid line is
  in `result.Stdout` before the deadline fires.
- Backstop: `time.AfterFunc(testdeadline.Wait(5*time.Second), func() {
  expire("backstop") })`, so a child that never prints still ends the subject. The timer
  is stopped when `realRun` returns. **The test fails unless the winning cause is
  `"pid line"`.** Checking only that a pid line was seen is porous (plan review 2,
  Medium; the build at `f20d597` has exactly that hole): if the backstop fires after
  the trap but before `printf`, the children ignore TERM, the line is still printed
  after the expiry, the writer records a time, and the test passes although
  cancellation began before its precondition.
- The script gains a fixed `sleep 0.2;` before `trap` (child-side startup delay, a
  fixture, not synchronisation): a regression to a deadline armed at start goes RED on
  every run instead of under load only.
- Assertions: `errors.Is(result.Err, context.DeadlineExceeded)` (unchanged); the winning
  cause is `"pid line"`; two pids parsed; bounded cancellation now measured from expiry:
  `testdeadline.Exceeded(time.Since(expiredAt), 3*processGrace+time.Second)` fails the
  test. The budget covers realRun's whole worst case after expiry, which is three
  `processGrace` waits (`exec.go:14`): TERM then up to 2 s for the wait (`:55`), KILL then
  up to 2 s (`:59`), then up to 2 s for the drains (`:71`). The expected path is shorter:
  the trap makes both processes ignore TERM, so one 2 s grace, then KILL reaps both and
  the drains finish at once. (Plan v1.2 and today's test used 5 s, under the 6 s worst
  case; a slow-but-correct drain could fail it.) Each pid reaches `ESRCH`
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
  (`admit_exclusive_identity_test.go:29`). Consumers, all evaluate-driven:
  `admit_exclusive_test.go`, `worker_admit_exclusive_test.go`,
  `admit_exclusive_identity_test.go` (its one `waitAdmitGrant`, `:242`, runs on
  `startExclusiveTestDaemon`, not on these constructors) and `admit_drain_reason_test.go`
  (`exclusiveWireServer` at `:132`, `:203`; 10 `evaluate(` calls at `d13ef3b`, `grep -o`, no background-grant
  wait). The builder re-greps for constructor consumers before the change. Not in `admitTestServer` (tests such as
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

No test seam (plan v1.2's `quiesceConfirmBound` field is dropped; plan review 2, Sol P2).

- `detach_linux.go`: a named constant `forcedQuiesceConfirmBound = 2 * time.Second`,
  commented: the bound on the emptiness confirmation after the forced `cgroup.kill` in
  `forceDetachedQuiesce`; deliberately independent of the configurable grace, which is
  the wait BEFORE the kill; equal to the production grace (`runner_linux.go:188-190`;
  production never sets `Config.Grace`: the one `runner.Config{}` literal outside tests,
  `internal/app/project.go:305`, has no `Grace`), so production behaviour is unchanged.
- `detach_linux.go:646`: `waitEmpty(ctx, scope, forcedQuiesceConfirmBound)`. Line 591
  is unchanged (`r.grace`, the natural-quiesce wait). The `quiesce-forced` record's
  `ScopeKill.GraceMS` (`:663`) keeps recording `r.grace`: it is the grace allowed before
  the forced kill (line 591's wait, the same meaning `GraceMS` has at `:561` and in
  `runner_linux.go`, the grace before escalation), not the post-kill confirmation bound.
  The constant's comment says so.
- Other tests that call `forceDetachedQuiesce` with a small `Grace`
  (`detach_linux_test.go:775-905`, `Grace: 10ms`) are unaffected on a pass: their scopes
  empty on the first read after a successful kill, and the kill-failed case takes the
  `else` branch that never calls `waitEmpty`. The builder re-greps for callers.
- `detach_linux_test.go:1194`: keep `r.grace = 100 * time.Millisecond` (the wait that
  must expire: a negative wait, which contention makes more certain **up to a 30 s
  stall**: if supervision stalls longer than the `sleep 30` descendant lives, the
  descendant exits by itself, the scope quiesces naturally, and the test fails because
  no quiesce was forced; section 4). The confirmation now runs on the product's own
  unscaled 2 s bound, which returns as soon as the scope is empty, so it costs nothing on
  a pass. It is not scaled for `-race`: it is the product's bound, and a kill-to-empty
  slower than 2 s fails production identically (reported `U_RUN_CAPTURE_INCOMPLETE`).
- The failure message at `:1197-1198` prints `outcome.err` and `*outcome.record` (today it
  prints two pointers, which is why the witnessed failure carried no diagnosis).

Prototype (seam shape, confirmation bound `testdeadline.Wait(time.Second)` = 5 s, so a
looser bound than the constant's 2 s; the builder re-measures the constant): plain,
2 CPUs + 6 burners, x 300: **0/300**, wall 108 s (104 s before). The measured
kill-to-empty maximum (240 ms) is under the constant by 8x.

### 3.7 AIRA-199: `cmd/aira/m20b_test.go`

- READY: pass `dupTestFD(t, readyW)` (the existing helper,
  `supervisor_relay_store_test.go:51`): the callee owns the duplicate. No `defer
  readyW.Close()`: the test closes `readyW` exactly once, explicitly (below). If the test
  fails before that close, the `*os.File` finalizer closes it, which is safe because
  nothing else ever held that number.
- ACK: no pipe at all. On this path `runSupervisor` only parses `--ack-fd` (it must be a
  nonnegative integer, `main.go:661-663`) and rejects the malformed wiring sidecar
  (`:674-678`) before `SuperviseRequest`, the only consumer of the ACK descriptor. Pass
  the placeholder `--ack-fd 2147483647`: nonnegative, so it passes the parse, and above
  the kernel's descriptor ceiling (`fs.nr_open` cannot exceed `sysctl_nr_open_max` =
  `INT_MAX & -BITS_PER_LONG` = 2147483584 on 64-bit Linux, `fs/file.c`; recalled, not
  re-read; the builder confirms), so it can never name an
  open descriptor even if a regression wrote to or closed it. This removes the `ackR`/
  `ackW` pipe and the leaked duplicate plan v1 accepted.
- Right after `runSupervisor` returns and before `Decode`, close `readyW` and **assert the
  close succeeds**: `if err := readyW.Close(); err != nil { t.Fatalf("the callee closed a
  descriptor this test owns: %v", err) }`. This is the pin: a callee that closed the
  test's own descriptor makes the close fail with `EBADF` on every run, with no GC timing
  involved. It also means a supervisor that wrote nothing gives an immediate `EOF`
  instead of a read that blocks while the test holds a write end.
- Delete `itoaFD` (`:67`), the only raw-fd hand-off helper in the package.

Prototype (plan v1 shape, ACK also duplicated; the placeholder ACK is not yet measured
and the builder re-measures it): the dup shape with a forced-GC window 50/50 pass and
`-race` x 400 0/400;
the close assertion with the mutation "pass `readyW.Fd()` again" 5/5 RED
(`m20b_test.go:55: the callee closed a descriptor this test owns: close |1: bad file
descriptor`) and without it 5/5 GREEN.

## 4. Invariants

1. No test in scope waits for an expected event on a fixed wall-clock bound: every such
   wait is a `testdeadline` backstop (>= 5 s, scaled x4 under `-race`), and on a passing
   run returns as soon as the event happens.
2. No retrying loop contains a short inner `t.Fatal`.
3. No `time.Sleep` orders a test action after an injected key or an asynchronous effect;
   a barrier or a predicate does. The only remaining sleeps in scope are labelled
   negative waits, poll pacing, or deliberate fault-injection delays (C's child-side
   `sleep 0.2`, the barrier pin's bounded hold, the 300 ms UI stall) that can only make
   the wrong shape fail; each carries a comment saying which.
4. A negative wait can only false-pass under contention, never false-fail.
5. A fixture duration the test needs to expire is never also a backstop it needs not to
   expire (F; C's subject deadline is armed only after its precondition).
6. A test never hands a callee the number of a descriptor whose `*os.File` it still owns.
7. A test that drives admission passes by hand is its queue's only evaluator from the
   queue's birth.
8. Unevaluated stays unevaluated: an environment that cannot run a real-cgroup test
   skips (or fails under `AIRA_REAL_CGROUP=1`) as today; a failure after the environment
   is established (A: the scope was on disk and read empty, the launcher did not fail,
   and the child never entered it) is a hard failure. Listing alone (a `Pending` row, or
   an on-disk scope whose population cannot be read) establishes nothing.
9. With E's seam unset, and with production's grace (always 2 s), production behaviour
   is unchanged (same call, same bound). The one visible production change is the
   destructive-confirmation form's height (`tui.go:1110`, one row taller).
10. Every test still fails against the wrong implementation (section 5 mutations).

**Scope of these guarantees (plan review 2).** "Cannot false-fail" in this plan means
"cannot false-fail unless a stall outlasts a bound", not "never":

- A positive wait false-fails only when a stall outlasts its scaled backstop (>= 5 s, x4
  under `-race`). Specific cases: A's expiry verdict (a stall between scope creation and
  child placement longer than the backstop reads as `readinessEmpty`, a hard failure);
  F's forced path (a supervision stall longer than the `sleep 30` descendant's life
  quiesces the scope naturally and the test fails); F's confirmation runs on the
  product's unscaled 2 s bound; C's cancellation budget is 7 s, unscaled base.
- A bounded negative window (B's 200 ms barrier-pin hold and 300 ms UI stall, E's 50 ms
  window, C's `sleep 0.2`) can false-pass under contention. So a mutation that REDs
  through such a window is **evidence, not deterministic proof**: the builder records
  the mutation runs as counts (for example 20/20), and a GREEN mutation run is
  re-run, not explained away.

## 5. Tests (TDD; each must fail against the wrong implementation)

Each new or changed test is first shown RED: against today's test shape via the
deterministic variant named here, or against the named mutation. Record the RED and the
GREEN (exit code and counts) in the PR.

**A.** `TestConfineKillReadinessRequiresAPopulatedRecordForTheExactName` (unit, no
cgroup): outputs built with the production encoder (`toolResponse(1, core.Response{OK:
true, Code: "OK", Data: runner.ConfineListResult{...}})`, then `json.Marshal`); the
`Pending` row is built by the production `ShimConfineList` from a registry entry, so its
shape is the real one; the environmental replies are built the same way
(`core.Response{OK: true, Code: "UNEVALUATED", Data: runner.ConfineListResult{Verdict:
"unevaluated", ...}}`, and the production `confineClientError` of an
`E_CONFINE_UNAVAILABLE` error). Cases: no record -> absent; ours `Pending` -> pending;
ours on disk with `subtree_populated` nil -> unreadable; ours false -> empty; neighbour
`mcp-e2e-x-other` populated true and ours false -> empty; ours true -> placed; two
records named ours -> error; `UNEVALUATED` -> unlistable; `E_CONFINE_UNAVAILABLE` ->
unlistable; `E_DAEMON_TIMEOUT` -> unlistable; and each of these -> error: garbage, `{}`, a JSON-RPC `error` reply (built
with the production `protocolResponse`), a `result` with no `structuredContent`, an
empty `code`, `ok:false` with another code (for example `E_DAEMON_PROTOCOL`), `OK` with
no `scopes` array. A second table pins the expiry decision (`readinessIsProductFailure`
over every state below placed): unlistable, absent, pending, unreadable -> skip-or-fail;
empty -> hard fail. Mutations, each RED: today's substring predicate (pending, nil and
false cases); `strings.Contains` name match (neighbour case); nil read as true; pending
read as on disk (pending case, and the expiry table); "listed -> hard fail" (pending and
unreadable rows of the expiry table); duplicates ignored; **envelope not validated (the
`e5c1223` shape: every non-`OK` reply read as absent) -> the `{}`, protocol-error and
other-code rows**. The expiry ordering (drain `done` before deciding) is a three-line
straight-line sequence checked in review, not by a test.
The e2e keeps its shape; under load it must stay 0/40 (it was 0/40 before, so the load
run is a no-regression check, not evidence of the fix).

**B.**
- `TestTUIKeyBarrierReturnsOnlyAfterEveryEarlierHandlerReturned`: the test holds a key
  HANDLER, not the loop. Before `trackKeys`, it installs an inner capture: on key `X` it
  signals `entered`, then holds until `pressReturned` is closed or a bounded hold
  elapses (200 ms, a labelled negative window), then sets `handledX`; on key `Y` it sets
  `handledY`. Then: a RAW `screen.InjectKey(X)` (not through `press`), and in a goroutine
  `press(Y)`, after which the goroutine records `doneAtReturn = handledX && handledY` and
  closes `pressReturned`. Assert, after `pressReturned` (testdeadline backstop), that
  `doneAtReturn` is true.
  Correct barrier: cannot false-fail. The sentinel is behind `X` and `Y` in one FIFO
  queue, and the loop finishes `X`'s handler (after the hold) and `Y`'s before it takes
  the sentinel, so `press` returns only after both flags are set.
  Mutations, each RED: `press` that only injects (returns while `X` is held:
  `doneAtReturn` false, and its close of `pressReturned` ends the hold early); a barrier
  that signals on the first key it sees rather than its sentinel (returns at `X`'s
  capture, before `X`'s handler); the plan-v1 counter barrier without its final round
  trip (count 1 is reached at `X`'s capture). The plan-v1 counter barrier WITH its round
  trip REDs only when tview selects that round trip ahead of `Y` (a `select` between
  ready channels is random), so it is not a required mutation; it is why v1 was
  replaced. Honest limit: the RED is not strictly
  deterministic. A mutated `press` false-passes only if its goroutine is descheduled
  for the whole 200 ms hold between returning and reading the flags; it can only
  false-pass, never false-fail, and the builder records the mutation runs (e.g. 20/20).
- `TestTUIUIReadsTolerateASlowLoop`: block the loop for 300 ms (after the
  started-handshake), then run the `TestTUIKeypressAndQuit...` probe through
  `eventuallyOnUI`. Today's probe shape (100 ms inner fatal): RED (17/20 measured without
  the handshake; 20/20 is an expectation with it, not a measurement). This stall is a
  fault injection, so the RED depends on the 300 ms block outlasting the 100 ms inner
  timeout, which it does unless the probe goroutine is descheduled for 200 ms; again a
  false-pass direction only.
- `waitForSimulationText` on a needle that never appears fails with the screen dump.
  Mutation: return silently (RED; this is what let the panic through).
- Mutations against the product, each must RED: `r` no longer marks the panel dirty
  (`TestTUIKeypressAndQuit...`); `y` dispatches from the confirmation (confirmation test:
  `PaletteDispatching` true after `press`); Escape does not cancel the destructive
  confirmation; Space at default focus dispatches; board Home does not select the first
  card (`TestBoardHomeEndUpdateSelection`); the confirmation form back at
  `GetFormItemCount()+5` (`TestTUIDestructiveConfirmationRequiresExactResolvedID`: the
  "Type RANT-7" wait fails; the production fix's pin).
- Barrier simplification (single sentinel, one acknowledgement channel): the barrier pin
  above and its three required mutations are unchanged and must still RED.
- Load: the four-test set (`^(TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight|TestTUIRendersTruncatedErrorAndUnevaluatedStates|TestTUIPaletteOpensOnColon|TestTUIConfirmationRequiresFocusedConfirmEnterAndDispatchesOnce)$`)
  x 100 and the whole TUI set x 20, both `-race`, 2 CPUs + 6 burners: before 9/400 and
  6 + a package panic; after 0 failures, no panic.

**C.** The rewritten `TestRealRunTimeoutKillsProcessGroup` (with the permanent pre-trap
delay) is RED against today's shape (deadline armed at start): 10/10 measured. Mutations
of `realRun` (`exec.go`), each RED: TERM/KILL sent to the leader pid instead of the
process group (`sleep` survives: ESRCH wait fails); no SIGKILL escalation (both ignore
TERM: ESRCH wait fails); return the wait error instead of `ctx.Err()` (`errors.Is`
fails); `LiveStdout` not plumbed to the drain (backstop wins: "expired by backstop, not
by the pid line"). Check of the test's own guard (a one-off variant, not committed): put
`sleep 1;` between the `trap` and the `printf` and shorten the backstop to 0.5 s, so the
backstop expires the context after the trap and before the line, and the line is still
printed (TERM is ignored). The winning-cause assertion must RED; the `f20d597` guard
("a pid line was seen") passes on this variant, which is the plan-review-2 hole. Load:
before 1/60; after 0/60.

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
  more `Empty()` reads (about 30 ms of the 10 ms tick, inside the 2 s constant);
  `r.grace = time.Nanosecond`; no seam to set; call `forceDetachedQuiesce` directly. It
  returns `(nil, nil)` and the ledger holds `quiesce-forced` with `ScopeKill.Completed`
  and `ScopeKill.GraceMS` equal to `r.grace` (the pre-kill grace, unchanged meaning).
  Mutation: confirm with `r.grace` again (today's line) -> `U_RUN_CAPTURE_INCOMPLETE`
  (RED; the 1 ns deadline wins against the 10 ms tick on all but a vanishing fraction of
  runs). The test name stays; its comment names the constant instead of the seam.
- The real-cgroup test keeps every existing assertion (status EXITED, leader exit 0
  preserved, `QuiesceForced`, `U_RUN_QUIESCE_FORCED`, not clean, never
  `ScopeDescendantKilled`). Load: before 3/200 and 6/300; after 0/300.

**199.** The pin is the close assertion. Mutation: pass `readyW.Fd()` again -> RED on
every run (5/5 measured on the plan-v1 shape; the builder re-measures with the
placeholder ACK). The old test shape is RED under a forced-GC
window (two `runtime.GC()` plus 20 ms before `runSupervisor`: 50/50 EOF measured); that
window is a measurement only and is not committed. Load: `-race` x 400: before 8/400;
after 0/400.

**Gate.** `aira confine -- make ci` (the gate's exact command), then `go test -race` on
`cmd/aira`, `internal/daemon`, `internal/runner`, `internal/gitremote`. The load runs
above, one at a time, each with `aira confine --name <unique>`, recording, before and
after, in the PR: the script's exit status and its `expected`, `evaluated`, `fail`,
`skip` and `aborted` counts, and the failure lines. A load result is stated as
`fail/evaluated`; a run with `evaluated = 0` (all skipped, for example A without the
installed helper) is **unevaluated**, never "0 failures", and a run with `aborted > 0`
(panic or timeout) is incomplete and reported as such.

## 6. Expected yield

"Before" counts are measured (`fail/evaluated`). "After" is an **expectation** unless it
says "prototype", which is a measurement on the throwaway worktree, not on committed code.
A finite clean sample after the change supports "no failure observed in N runs", not
"cannot fail"; the deterministic RED/GREEN pairs in section 5 are the proof of each fix.

| Flake | Before (measured) | After |
|---|---|---|
| E exclusive | 9/24000 (`-race`, 2 CPUs + 6 burners); witnessed once in `make ci` on 2026-10-10 | expected 0; prototype 0/54000 |
| F M20 forced quiesce | 3/200, 6/300 (2 CPUs + 6 burners); witnessed once on 2026-10-10 | expected 0; prototype (seam shape, 5 s confirmation bound, not the 2 s constant) 0/300 |
| 199 m20b | 8/400 (`-race`) | expected 0; prototype (v1 shape, ACK duplicated) 0/400 |
| B TUI | 9/400 + a package-killing panic (`-race`, 2 CPUs + 6 burners) | expected 0 failures, no panic (not prototyped) |
| C gitremote | 1/60 (2 CPUs + 6 burners) | expected 0 (not prototyped) |
| A MCP kill | 0/40 (not reproduced; three historical sightings) | expected 0; the window is closed by construction, and the load run is a no-regression check only |

The merge gate stops refusing correct pushes for these six classes; one TUI failure can
no longer take down every `cmd/aira` test with it; and the M20 failure, if it ever
recurs, prints its error instead of two pointers. An operator confirming a destructive
palette action can see the "Type <ID>:" field they must fill. Cost on a passing run: C
+0.2 s; `TestTUIDestructiveConfirmationRequiresExactResolvedID` about 10 s faster (its
silent 10 s wait is gone); the others unchanged (the backstops return as soon as their
event happens).

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| D (`TestRealCgroupDynamicCharge...`) | Its test and feature were deleted in `9a2f65c`. Dropped from AIRA-204. |
| `TestCLIRunRealCgroupOrClearSkip` `bad file descriptor` (AIRA-199 amendment) | Not reproduced: interleaved with m20b, `-race`, 300 rounds -> 0/600. The fix removes the only known in-package closer of a test-owned descriptor, so the attribution is plausible but **unevaluated**; it is not claimed fixed. |
| Production finding: `cmd/aira/main.go:660-664` | With an unparseable `--ready-fd` (or none), `strconv.Atoi` returns 0 and `writeSupervisorFailure(readyFD, ...)` writes to and **closes fd 0**; it should use `readyForFailure` (`supervisorReadyFD` returns -1 for an unparseable value). No test reaches it in-process today. A product bug, not a flake: filed held as **AIRA-294** (P3, bug) in this batch, not built here. |
| Phantom `outstandingJobs = 1` setups in the exclusive tests | The S14 comments prefer real leases (`seedRunningLease`). With E's seam they are no longer racy; converting them is a test-quality change, not a flake fix. |
| Other short-grace real-cgroup fixtures in `internal/runner` (`memory_cap_linux_test.go:294` grace 100 ms, `livetee_test.go:507` grace 200 ms) | Same single-knob shape is possible; not witnessed; production grace is 2 s. Recorded. |
| Raw waits in `cmd/aira` outside the TUI tests and test A (`dispatcher_inprocess_test.go` others, `watch_test.go`, `drain_hold_test.go`, `confine_test.go`) and in `internal/store`, `internal/install`, `internal/core` | Not witnessed; AIRA-204 records them as not scheduled. |
| A lint guard against raw `time.After` | Decided against in AIRA-204. |

## 8. Risks

- **Production changes (E, F, B).** Four files: E's one field and one `if`
  (`internal/daemon/server.go`, `admit.go`); F's named constant at one call site
  (`internal/runner/detach_linux.go`; `runner_linux.go` is no longer touched, and the
  build's `quiesceConfirmBound` is removed); B's form height (`cmd/aira/tui.go:1110`).
  Route these four files to a separate review pass: production never sets
  `admitManualEvaluation` (grep) and it cannot leave a production queue without an
  evaluator; the default E path is today's statement; F's constant equals the only
  grace production ever uses, and `GraceMS` keeps its meaning; the `tui.go` change is a
  visible layout fix (one more form row inside the same 90x26 centred modal) and nothing
  else, checked against the other confirmation shapes that share
  `showPaletteConfirmation`: the details pane above the form (proportional height) gets
  one row less, and a non-destructive confirmation (no input item) gets one blank row.
- **E coverage.** A test built on one of the three constructors that relied on a
  background grant would now wait for its backstop and fail loudly (not silently). The
  prototype ran the whole daemon package under `-race` once with no failure.
- **Key barrier (B).** It depends on the application-level input capture installed at
  construction. If a future change re-installs the capture after the tracker wraps it,
  the sentinel is never recognised, so `press` fails on its backstop (loud). The barrier
  pin test covers the helper itself. The sentinel must never be a key the TUI binds; a
  supplementary private-use rune is not typeable and no handler matches it.
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

The script prints `rc expected evaluated fail skip aborted` and exits with the test
binary's status when that is non-zero, 3 when nothing was evaluated, 4 when runs are
missing (panic or timeout), and 0 only when every expected run was evaluated or skipped
and the binary exited 0 (plan v1's script returned 0 even when the binary failed).
Same pattern for each row of section 6: `cmd/aira` (`aira.test`, `aira.race.test`),
`internal/runner` (`runner.test`), `internal/gitremote` (`gitremote.test`), with the
regex, CPU list, burner count and count given there. The deterministic variants (20 ms
gap, pre-trap sleep, 300 ms UI stall, forced GC on the old m20b shape) are one-line edits
of the test named in section 5. The prototype and diagnostic edits used for this plan
are saved, mixed, in `~/tmp/aira-flakes/prototype-and-diag.diff` (scratch, for the
builder's reference only).

## 10. Review log

### 10.1 Plan review 1

Two independent read-only reviews of plan v1.1, both PASS_WITH_CHANGES.
Each claim was checked against the code on this branch before it was accepted.

| # | Finding | Check | Disposition |
|---|---|---|---|
| R1-1 (high) | A: "listed" proves neither directory existence nor working containment; `mergeConfineRegistry` adds named, owned `Pending` rows before the scope is on disk. Distinguish pending / unreadable / empty / populated; base skip versus hard failure on separately established capability | True: `confine_shim_manage.go:21-53` builds `Pending: true` rows with `populated` unevaluated; `ListConfines` merges them (`confine_manage_linux.go:365`). Plan v1's "scope DIRECTORY name" mechanism was wrong | Accepted. Section 3.1 now has five states; only `placed` breaks the poll; on expiry only an observed-empty on-disk scope (with the launcher not failed) is a hard failure, everything else is skip-or-fail. Mechanism text, invariant 8 and the A unit tests updated |
| R1-2 (medium) | B: the barrier pin checks that the capture ran, not that the handler completed; removing the final round trip still passes; the 50 ms release makes the "deterministic" RED scheduler-dependent | True for the v1 pin (the counter increments at capture entry, before the handler) | Accepted, and the barrier itself was replaced (see R2-1). The pin now holds a handler and checks completion of every earlier handler at `press` return; the RED is stated as near-deterministic with a false-pass-only limit |
| R1-3 (medium) | `docs/dev/aira204-loadloop.sh` exits 0 when the binary fails; zero failures with zero evaluations proves nothing | True: the script ended with `grep ... \|\| true` after saving `rc` | Accepted and fixed in the script: it counts expected runs from `-test.list`, reports `expected/evaluated/fail/skip/aborted`, exits with the binary's status, 3 when nothing was evaluated, 4 when runs are missing. Section 5 Gate and section 9 updated; section 6 labels "after" values as expectations or prototype measurements |
| R1-4 (medium) | 199: the ACK duplicate is leaked for nothing; `runSupervisor` only parses the ACK number before rejecting the wiring | True: `main.go:660-678`; the ACK descriptor's only consumer is `SuperviseRequest` (`:712`), after the rejection | Accepted. ACK is the placeholder `2147483647` (nonnegative, above any possible descriptor); no ACK pipe, no leak |
| R1-5 (low) | "The plan is this minimum" overstates; the broad TUI conversion and the load matrix are extra hardening; keeper versus seam for E | Fair | Accepted. Section 2 lists the concrete minimum and names B's breadth and the load matrix as chosen extras with their reason; the E trade is stated, the seam kept (one bool, default branch is today's statement; fewer fixture edits) |
| R1-6 (info) | Core claims check out; a finite sample supports "not observed", not proof | Agreed | Section 6 says so |
| R2-1 (P2) | B: the counter barrier is porous; raw `InjectKey` inflates the count so a later `press` can return before its own key ran | True: `tui_smoke_test.go:240-242`, `:264-266`, `:276-278`, `:325-327`, `:626-628` and board `:285`, `:315` inject raw keys | Accepted with a simpler fix than proposed: instead of routing every injection through the tracker and banning raw `InjectKey`, `press` injects a sentinel key through the same screen queue and waits for it. tview runs each key event to completion before taking the next (`application.go:391-450`, checked), so the sentinel proves every earlier key, raw or not, was handled. No counter, no ban, no final round trip. The pin includes a raw key then a `press`, as the review asked |
| R2-2 (P3) | C: `Err()` must be derived from the `Done` channel | True: `exec.go:51` waits on `ctx.Done()` and `:60`, `:63` then read `ctx.Err()` | Accepted (section 3.3) |
| R2-3 (P3) | F: `GraceMS: r.grace` at `detach_linux.go:663` drifts from the bound actually used | Checked: `GraceMS` records the grace allowed BEFORE the kill (line 591's natural-quiesce wait, still `r.grace`), the same meaning as at `:561` and in `runner_linux.go` (`termGrace`, the grace before escalation). It is not the post-kill confirmation bound | Rejected: recording the confirmation bound there would change the field's meaning. The seam comment says `GraceMS` stays the pre-kill grace. Also corrected plan v1's wording: line 591 is in the caller, not in `forceDetachedQuiesce` |
| R2-4 (P3) | 199: `defer readyW.Close()` plus the asserted close double-closes | True | Accepted: no defer; one explicit asserted close (the finalizer covers an early failure, safely) |
| R2-5 (P3) | E: `admit_drain_reason_test.go` is an unlisted consumer of `exclusiveWireServer` | True: `:132`, `:203`; no background-grant wait (v1.2 said 11 `evaluate()` calls; there are 10, corrected in v1.3) | Accepted: listed in section 3.5 with the other consumers |
| R2-6, R2-7 (info) | Line references, measurements and necessity verified | — | No change |

Script check after the R1-3 fix (`internal/gitremote` binary, no burners, each under
`aira confine`): a passing run (count 2) -> `expected=2 evaluated=2 fail=0 skip=0
aborted=0`, exit 0; a regex matching nothing -> exit 2 before any burner starts; a run
killed by `-test.timeout 1ms` -> `evaluated=0 aborted=1`, exit 2 (the binary's own
status). The `evaluated = 0` with exit-0 binary path (exit 3) was not exercised. `shellcheck`:
clean.

### 10.2 Plan review 2

Two independent read-only reviews of plan v1.2 (with the build at `e5c1223` on this
branch), both PASS_WITH_CHANGES. Reviewer 1 (findings 2-1..2-6) and Sol (S-1..S-6). Each
claim was checked against the code (base `d13ef3b` for line numbers, HEAD `e5c1223` for
the build) before it was accepted.

| # | Finding | Check | Disposition |
|---|---|---|---|
| 2-1 (high) | A: `confineKillReadiness` (`dispatcher_inprocess_test.go:1150`, built) reads `{}`, missing fields and any protocol error as `absent`, so a broken MCP reply ends as an environmental skip without exercising ownership or the kill | True. `e5c1223` deleted the envelope checks; its own table pins `ok:false code:"E_X"` and a JSON-RPC `error` as absent, and `{}` unmarshals to no scopes. The poll also discards the MCP exit status | Accepted. Section 3.1 validates the envelope and classifies by `code`: only the identified environmental answers (`UNEVALUATED`, `E_CONFINE_UNAVAILABLE`, and the three transport codes from the test's private daemon, which are transient under load) become a new weakest state `readinessUnlistable`; everything else is an error. Unit cases and the "envelope not validated" mutation added (section 5) |
| 2-2 (medium) | C: the backstop records nothing; a pid line delivered after a backstop expiry satisfies the assertions | True: `exec_test.go:86` is `time.AfterFunc(..., deadline.expire)`; the only guard is `trigger.at().IsZero()`, set by any later line. Plan v1.2 had promised a "backstop fired" mark | Accepted. `expire(cause)` records the winning cause and the expiry time inside the one `once`; the test fails unless the pid line won (section 3.3), with a one-off variant that REDs the new guard and passes the old one (section 5) |
| 2-3 (medium) | The production-scope claim is false: `cmd/aira/tui.go:1110` changes the confirmation form height; revisit the explanation for the missing "Type ID" text | True (`git diff d13ef3b HEAD --stat`: `tui.go` 2 +-). And the base timings settle the explanation: every passing run of the destructive test took 10.28-11.39 s on `d13ef3b` (the silent 10 s wait expired every time), 0.44-1.21 s after | Accepted. Header and section 8 list the `tui.go` fix as a visible product change with its own review; section 1's B mechanism now says the text was never on screen (a layout bug, on every run) and keeps the nil form's ordering route as inferred; section 3.2 rule 7 and a section 5 mutation pin the fix |
| 2-4 (medium) | Qualify the false-failure guarantees: backstops still permit false failures under long stalls; F's finite `sleep 30` can exit first; bounded windows and fault-injection mutations can false-pass, so RED runs are evidence | True | Accepted. Section 4 gains "Scope of these guarantees"; section 3.6's "contention only makes it more certain" is bounded at a 30 s stall; section 3.1 names A's long-stall case; section 3.2's closing sentence names the backstop |
| 2-5 (low) | The from-scratch minimum is smaller; broad TUI conversion and load matrix are optional; the serial barrier could use one sentinel acknowledgement instead of sequence numbers and a waiter map; C's permanent startup delay is optional scaffolding | Barrier: true, `press`/`barrier` are only called serially and each waits for its own sentinel (`tui_sync_test.go:100-150`, built) | Barrier: accepted, simplified to one sentinel rune and one buffered acknowledgement channel (section 3.2). Breadth and load matrix: already chosen extras. C's delay: kept as a chosen extra (section 2), since it is the only thing that makes the "deadline armed at start" regression RED on every run, for 0.2 s |
| 2-6 (info) | Pending-row semantics, tview ordering, E's lifecycle, F's grace/default, READY ownership, seams unset in production, script status handling verified; measurements and mutation outcomes not re-verified | — | No change |
| S-1 (P2) | F's seam is avoidable: a fixed post-kill bound (production's 2 s) gives identical production behaviour, removes the field, and removes the dual role in the product | True: `waitEmpty` maps `timeout <= 0` to 2 s (`cgroup_linux.go:396-397`); production's grace is 2 s (`runner_linux.go:188-190`); the only `runner.Config{}` outside tests (`internal/app/project.go:305`) sets no `Grace`. The other forced-quiesce unit tests do not reach `waitEmpty` with a stuck scope | Accepted, with a named constant `forcedQuiesceConfirmBound` rather than a magic `0` (section 3.6, table in section 2). `runner_linux.go` leaves the change set. The trade (the real-cgroup test runs on the unscaled 2 s product bound) is stated |
| S-2 (P3) | A: on expiry with `readinessEmpty`, drain `done` once more before deciding | Partly: in the build the `done` receive immediately precedes the deadline check in the same iteration, so the gap is the two statements between them, not a 20 ms poll gap. Still nonzero, and the plan did not fix the order | Accepted: section 3.1 specifies the in-iteration order and a final non-blocking receive before the verdict |
| S-3 (P3) | `admit_drain_reason_test.go` has 10 `evaluate(` calls, not 11 | True (`git show d13ef3b:... \| grep -o 'evaluate(' \| wc -l` = 10) | Accepted: corrected in section 3.5 and in the R2-5 row |
| S-4 (P3) | C: worst case after expiry is 3 x `processGrace` = 6 s, but the budget is 5 s | True: `exec.go:14`, `:55`, `:59`, `:71` | Accepted: budget `3*processGrace + time.Second` and the path is named (section 3.3) |
| S-5 (P3) | File the `main.go:660-664` product bug as a held ticket rather than "suggest a ticket" | True: `strconv.Atoi` yields 0 on error, `writeSupervisorFailure` (`main.go:752-762`) wraps, writes and closes that fd; `readyForFailure` (`:642`, from `supervisorReadyFD`, `:739-750`) returns -1 | Accepted: filed **AIRA-294** (held, P3 bug); section 7 updated |
| S-6 (P3) | Verification record of every cited file:line at `d13ef3b` | — | No change |

### 10.3 What the build on this branch must change to match v1.3

The build (`ee653a4..e5c1223`) predates this revision. The build-fix pass changes:

1. **A** (`cmd/aira/dispatcher_inprocess_test.go`): restore and extend envelope
   validation per section 3.1 (`readinessUnlistable`, the code table, MCP exit status
   checked); flip the built table's `not ok` and `protocol error` rows to errors; add
   the section 5 rows; final non-blocking `done` receive before the expiry verdict.
2. **B** (`cmd/aira/tui_sync_test.go`): single sentinel rune and one buffered
   acknowledgement channel; delete `seq` and `waiters`; doc comment says not safe for
   concurrent use. Record the `+5` mutation RED for the `tui.go` fix.
3. **C** (`internal/gitremote/exec_test.go`): `expire(cause)` with the winning cause
   recorded inside the `once`; assert the pid line won; budget
   `3*processGrace + time.Second`; run the one-off guard check from section 5.
4. **F** (`internal/runner`): remove `quiesceConfirmBound` from `runner_linux.go` and
   `detach_linux.go`; add `forcedQuiesceConfirmBound`; drop the field's use in
   `detach_linux_test.go`; re-measure the real-cgroup test under load (section 6 row).
