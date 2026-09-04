# Fix 5 — confine trailer kill-attribution (AIRA-70 unified with AIRA-91 Part A)

Plan for §3.6 of `docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md`.
Tier A (full two-loop): correctness-adjacent daemon/supervisor signal-handling work.

**Revision 3** — two plan-review rounds folded, both DeepSeek-pro, the second
red-teamed against the first's fixes.

- **Round 1** (one P0, two P1, two P2, all accepted): the backlog plan's
  `external-cgroup-kill` **overclaims** and becomes `unattributed-sigkill`
  (§3.1, §8); `oom` moves ahead of `supervisor-signal` in the classifier; the
  candidates line is phrased as candidates; the trailer test gains rows; the
  exit-code table drift is corrected.
- **Round 2** (two P0, one P1, all accepted): the `normal` branch ran ahead of
  the supervisor-signal branch, so a child that *caught* our forwarded SIGTERM
  and exited cleanly would print `terminated-by=normal` (§3.2); the plan did not
  say how the main goroutine is guaranteed to see the handler goroutine's write
  (§3.3); and test 4 as specified was timing-dependent (§4, Tests).

Codex/Sol and Gemini were both unavailable for these reviews (usage credits
exhausted / free-tier quota exhausted, one retry each); recorded rather than
silently skipped, and noted as a real reduction in review independence — both
rounds came from the same model family.

## 1. Problem, restated against current source

A confined job that dies of SIGKILL prints a trailer byte-identical to a clean
run's. `confineWithDeps` (`internal/runner/confine_linux.go:848-875`) computes
`result.Exit = waitConfineCommand(cmd)` — an `int`, `128+signal` for a signalled
child — then prints `FormatConfineStatus(result.Status)` followed by an optional
`formatConfineReserveAdvisory(...)` line. Neither carries the *cause* of death:

- `FormatConfineStatus` (`internal/runner/confine.go:349-405`) renders eleven
  facets, none of them terminal. It never sees the wait status at all.
- `formatConfineReserveAdvisory` (`confine_linux.go:878-896`) is the only place
  a kill is ever mentioned, and it is **doubly gated**: it returns `""` unless
  `scopeMemoryMax > 0`, and its OOM branch needs `oom` — derived at
  `confine_linux.go:854` as `usage.OOMKill != nil && *usage.OOMKill > 0`.

So all four of these produce identical output today:

| what actually happened | exit | `oom_kill` | trailer today |
|---|---|---|---|
| clean run | 0 | 0 | status line, no advisory |
| kernel OOM at the scope cap | 137 | >0 | status line + OOM advisory |
| kernel OOM, **no scope cap set** | 137 | >0 | status line, **no advisory** (AIRA-70 #3) |
| external `cgroup.kill` (systemd-oomd / `aira confine --kill` / direct write) | 137 | **0** | status line, **no advisory** (AIRA-91 Part A) |
| supervisor got SIGINT/SIGTERM, forwarded it | 137 **or** 143/130 | usually unreadable | status line, **no advisory** (AIRA-70 #1) |

**Exit code on the supervisor-signal row is deliberately given as a range**
(plan-review P2, accepted): `onSignal` runs `cleanup()`, whose `scope.Kill()`
writes `cgroup.kill` (SIGKILL) while `forwardConfineSignals` separately forwards
the original SIGTERM/SIGINT to the child. The two race, so the child's wait
status is 137 *or* 143/130 depending on which lands first. Every test on this
path must therefore assert on the **facet**, never on the exit code.

The committed reproduction `internal/runner/external_cgroup_kill_probe_linux_test.go`
(landed ahead of this fix as `2751ad3`, per §8's ordering correction) already
pins the mechanism for row 4: an external whole-cgroup kill SIGKILLs every
process in the scope while `memory.events` `oom_kill` stays at **zero**, because
a userspace `cgroup.kill` is not a memcg OOM event. Its second test asserts the
gap as it stands (`advisory == ""`) and says, in a comment, that Fix 5 closes
this **on the status line via a new `terminated-by=` facet, not by changing the
advisory**. This plan honours that.

Two further attribution holes AIRA-70 records, both verified still open against
current source:

- **No log line in the supervisor's signal handler.** `forwardConfineSignals`
  (`confine_linux.go:1337-1360`) forwards the signal and calls `onSignal()`
  (which sets `interrupted` and runs `cleanup()`, `confine_linux.go:609-612`).
  Nothing is written anywhere.
- **No log line in the daemon's `confine-kill` dispatch.**
  `internal/daemon/confine_manage.go` contains no `log.` call at all (checked:
  the file does not import `log`). The `confine-kill` case is at `:131-141` —
  **plan-text drift noted:** the backlog plan says `:160-172` and AIRA-70's
  ticket says `:141-152`; the file is 155 lines long, so both are stale. The
  finding itself (no logging anywhere in the file) holds exactly as stated.

## 2. Scope

**In scope**

1. A new `ConfineStatus.TerminatedBy` facet, rendered by `FormatConfineStatus`
   as ` terminated-by=<value>`, always present.
2. A pure classifier, `classifyConfineTermination`, that maps (wait status,
   scope usage, supervisor-received signal) to that value.
3. Wait-status plumbing: `waitConfineCommand` returns the decoded termination
   witness alongside the exit code it already returns.
4. Supervisor-signal witness + the missing supervisor log line (AIRA-70 #1).
5. The missing daemon `confine-kill` log line (AIRA-70 #2).
6. Extending the committed AIRA-91 probe with the `external-cgroup-kill`
   assertion.

**Out of scope, deliberately**

- `formatConfineReserveAdvisory` is **unchanged**, per §3.6 ("the existing
  kernel-OOM advisory (unchanged)"). AIRA-70's ticket asks for a companion fix
  to its `scopeMemoryMax <= 0` gate; that ask is **satisfied by the new facet
  instead** — `terminated-by=oom` prints whether or not a cap was configured, so
  the uncapped-OOM case (row 3 above) is no longer silent. Changing the advisory
  as well would put two differently-shaped statements of the same fact on the
  same trailer and would break the committed probe's assertion 2, which the
  probe's own comment asks to be changed only deliberately. Recorded here as an
  accepted, written-down deviation from AIRA-70's suggested direction, not a
  silent drop.
- AIRA-91 Part B (the `memory.high` / oomd policy fork) — owner decision,
  explicitly out of scope (§1 of the backlog plan, AIRA-91 ticket line 519).
- `internal/runner/usage_linux.go`'s `classifyOOMKilled` (the `aira run` Status
  path) is a different surface and is not touched.

## 3. The vocabulary

`terminated-by=` takes one of:

| value | meaning | evidence required |
|---|---|---|
| `normal` | the child exited of its own accord (any exit code, 0 or not) | decoded wait status, not signalled |
| `oom` | the kernel's OOM killer killed something in this scope | `memory.events` readable and `oom_kill > 0` |
| `supervisor-signal:SIGINT` / `:SIGTERM` | the confine supervisor itself received that signal and tore the job down | our own handler fired |
| `child-signal:<NAME>` | the child died of some other signal (SIGSEGV, SIGABRT, SIGPIPE…) | signalled, not SIGKILL |
| `unattributed-sigkill` | SIGKILL that neither this supervisor nor this scope's own OOM counter accounts for | signalled SIGKILL, `oom_kill` readable and `== 0`, no supervisor signal |
| `unevaluated` | cannot be established | wait status undecodable, **or** SIGKILL with `oom_kill` unreadable |

### 3.1 The headline value is `unattributed-sigkill`, not `external-cgroup-kill`

**Deviation from §3.6 of the backlog plan, taken on a plan-review P0.** The
backlog plan, AIRA-91's ticket, and the committed probe's own comment all name
this value `external-cgroup-kill` and ask the trailer to "report it plainly as
an **external whole-cgroup kill**". Reviewed against what the classifier can
actually establish, that overclaims in two separate directions:

- **It is not necessarily whole-cgroup.** An external `kill -9` aimed at the
  job's leader PID alone produces the identical signature (SIGKILL,
  `oom_kill == 0`).
- **It is not necessarily external.** A job that SIGKILLs *itself* — `kill -9
  $$`, `timeout -s KILL`, a harness killing its own leader — produces the
  identical signature too. AIRA-91's own probe set has a `selfkill` target, so
  this is a known, real shape, not a hypothetical.
- **It is not necessarily not-an-OOM.** An ancestor-cgroup OOM increments
  `oom_kill` on the *ancestor*: `memcg_memory_event` propagates **upward**, so a
  slice-level OOM leaves this scope's counter at zero. This is exactly AIRA-70's
  finding #3 in its most surprising form.

Asserting one named mechanism out of that candidate set is precisely the
fabricated-certainty this project's honesty contract forbids ("a check that
cannot establish its result reports `unevaluated`, never a fake pass"). What the
classifier *can* establish, positively and completely, is: **the job died of
SIGKILL; this supervisor did not send it; this scope's own `memory.events`
records no OOM kill.** `unattributed-sigkill` says exactly that and nothing
more — and it echoes AIRA-70's own title ("A confined job's SIGKILL is
unattributable"). The candidate mechanisms are then named *as candidates* on the
accompanying line, which is where AIRA-91's "name the realistic sources" ask is
actually met.

The committed probe's comment (`external_cgroup_kill_probe_linux_test.go:35-40`)
predicts the other name; it also says its assertions are to be "update[d]
deliberately" when Fix 5 lands. This is that deliberate update, recorded here
and in the commit message rather than made quietly.

### 3.2 Classification order

First match wins:

```
1. wait status not decodable                       -> unevaluated
2. SIGNALLED and oom_kill readable and > 0         -> oom
3. supervisor received a signal during the run     -> supervisor-signal:<NAME>
4. not signalled                                   -> normal
5. signal is not SIGKILL                           -> child-signal:<NAME>
6. oom_kill not readable                           -> unevaluated
7. (SIGKILL, oom_kill == 0)                        -> unattributed-sigkill
```

- **2 before 3** (round-1 P1; revision 1 had these the other way round). A
  readable, positive `oom_kill` is a hard kernel-recorded fact, and it can never
  have been caused by our own teardown: `cleanup()` kills via `cgroup.kill`,
  which — the whole premise of AIRA-91's probe — does **not** increment
  `oom_kill`. So a positive counter on a *signalled* child always means a
  genuine OOM, and the earlier ordering would have suppressed a real OOM behind
  `supervisor-signal:` whenever the job was OOM-killed a moment before the
  operator's Ctrl-C. On the ordinary supervisor-signal path `cleanup()` has
  already removed the scope directory, so branch 2 finds nothing readable and
  falls through to 3 as intended.
- **Branch 2 requires `signalled`, not just `oom_kill > 0`.** `memcg_memory_event`
  propagates **upward**, so a descendant cgroup the job created inside its own
  scope, OOM-killed at its own limit, increments *our* scope's counter while our
  leader exits perfectly normally (`pytest` losing one worker and exiting 1 is
  the concrete case). Classifying that as `terminated-by=oom` would report a
  termination that did not happen. The `oom` boolean feeding
  `formatConfineReserveAdvisory` is unchanged and still speaks for that case
  where a cap was set.
- **3 before 4** (round-2 P0; revision 2 had `not signalled -> normal` first). A
  child may *catch* the forwarded SIGTERM/SIGINT and exit non-signalled — the
  normal, well-behaved case for a test runner with a shutdown handler. With
  `normal` first, the trailer would say the job ended of its own accord when the
  operator's Ctrl-C is precisely what ended it. The supervisor's own handler
  firing is a positively witnessed local fact and outranks the wait status's
  shape.
- **3 before 5/7** for the same reason: if we tore the job down, we say so,
  whichever of the forwarded signal and our `cgroup.kill` won the race (§1).
- **5 before 7** is honesty-critical. A test binary dying of SIGSEGV is a
  common, boring event; sweeping it into the SIGKILL bucket would be an actively
  misleading falsehood. `cgroup.kill` and `memory.oom.group` deliver SIGKILL and
  nothing else, so restricting branch 7 to SIGKILL costs nothing real.
- **6 before 7** is this project's `unevaluated` rule: an unreadable
  `memory.events` (memory controller not delegated, scope already removed) means
  OOM and an unattributed SIGKILL are indistinguishable. Reaching branch 7 from
  an unread counter would be a fabricated zero.

Keeping one honest value with its ambiguity written down is preferred here over
new machinery that tries to disambiguate (`[[architectural-simplicity]]`); §6
records what that machinery would have to be.

### 3.3 "During the run": how the supervisor-signal witness is synchronised

Branch 3 says *during the run*, and that qualifier is load-bearing (round-2 P0).
`stopSignalHandler` is deferred to function return, so the handler stays live
through `readUsage`, `reportPeak` and `attestScopeTeardown` — a window of up to
several seconds *after* the child is already dead. A signal arriving in that
window did not terminate anything, and must not be reported as though it had.

- The handler records the **first** signal it receives (first, not last: the
  first is the one that caused the teardown; anything after is noise) into a
  `sync.Mutex`-guarded field, **before** calling `cleanup()`, and writes its log
  line there too. Writing before `cleanup()` matters twice over: `cleanup()`
  blocks for up to 2s in `waitEmpty`, and it is the step that actually kills the
  job.
- The main goroutine takes a **snapshot under the same mutex immediately after
  `waitConfineCommand` returns**, before anything else, and classifies from the
  snapshot. Signals arriving later cannot retroactively relabel a completed run.
- **Why the snapshot is guaranteed to see a teardown-causing signal**, given
  that `forwardConfineSignals`'s stop function only closes a channel and never
  joins its goroutine: the handler's mutex release strictly precedes
  `cleanup()`, which strictly precedes the `cgroup.kill`/forwarded signal that
  kills the child, which strictly precedes the child's exit, which strictly
  precedes `cmd.Wait()` returning, which strictly precedes the snapshot's `Lock`.
  A `sync.Mutex` `Lock` that begins after a prior `Unlock` has completed is
  ordered after it, so the snapshot observes the write. The converse direction is
  correct by construction too: if the handler has *not* yet written, it has not
  yet started the teardown, so the child's death (if any) genuinely was not ours,
  and reading a zero witness is the right answer, not a stale one.
- The remaining window — a signal arriving between the child's own death and
  `cmd.Wait()` returning, of the order of microseconds — is accepted and
  recorded in §6. In that window the operator really did signal a job that was
  still nominally running, so `supervisor-signal:` is at worst imprecise about
  causality, never fabricated.

## 4. Changes, file by file

### `internal/runner/confine.go` (portable)

- Add `TerminatedBy string` to `ConfineStatus`.
- Add the unexported constants for the fixed values
  (`confineTerminatedNormal`, `…OOM`, `…UnattributedSIGKILL`, `…Unevaluated`)
  and the two prefixes (`supervisor-signal:`, `child-signal:`).
- `FormatConfineStatus`: append ` terminated-by=<value>`, defaulting an empty
  field to `unevaluated`, positioned at the end of the line (after the
  scope-memory facets) so existing `strings.Contains` assertions are unaffected
  and the newest facet reads last.
- Keep the classifier itself in the portable file if it needs no
  Linux-only types; it needs `syscall.WaitStatus`, so it lives in
  `confine_linux.go` and the *value constants* live here, next to the formatter
  that renders them. (Same split the file already uses for
  `parseConfineScopeID`: pure string values portable, syscall-shaped logic
  Linux-only.)

### `internal/runner/confine_linux.go`

- `waitConfineCommand(cmd) int` becomes
  `waitConfineCommand(cmd) (int, confineTermination)` where

  ```go
  type confineTermination struct {
      Decoded  bool           // the wait status was decoded at all
      Signaled bool
      Signal   syscall.Signal
  }
  ```

  The existing `return 3` fallback (wait error that is not an `*exec.ExitError`,
  or a `Sys()` that is not a `syscall.WaitStatus`) sets `Decoded: false`.
- `forwardConfineSignals`'s `onSignal func()` becomes `onSignal func(os.Signal)`.
  In `confineWithDeps` the callback records the **first** received signal into a
  mutex-guarded field and writes the AIRA-70 #1 line to `diagnostics`, **both
  before** the existing `interrupted.Store(true)` / `cleanup()` work — see §3.3
  for why the order and the mutex are load-bearing rather than decorative. The
  line:

  ```
  confine: received SIGTERM; killing scope <scope-id> on <slice> and forwarding to the confined job
  ```

  Written exactly once (guarded by the same first-signal latch), through the
  already-locked `confineLockedWriter`, so it cannot interleave with child
  output. **Ordering hazard, and how it is removed rather than guarded:**
  `diagnostics` is currently assigned at `confine_linux.go:720`, *after* the
  signal handler is installed at `:608`, so a handler that captured it would
  capture a nil writer on a path no ordinary test exercises. The fix is to
  **hoist the three-line `diagnostics` assignment above the scope creation at
  `:575`**, unchanged in content — a pure move with no behaviour change and no
  early return crossed — so the handler closes over a live writer for its whole
  lifetime. `admitDiag` (`:507`) stays as it is; it runs before the scope exists
  and has no locking need.
- `classifyConfineTermination(term confineTermination, usage cgroupUsage, supervisorSignal os.Signal) string` —
  the pure classifier, table above, no I/O.
- At the trailer site (`:848-875`): capture the termination witness from
  `waitConfineCommand`, snapshot the supervisor-signal witness under its mutex
  **on the very next line** (§3.3), and set `result.Status.TerminatedBy` from the
  classifier before `FormatConfineStatus` is called.
- Emit one extra line for the `unattributed-sigkill` verdict only — the "name
  the realistic sources" half of AIRA-91 Part A, phrased as **candidates**, not
  as an assertion (plan-review P1, accepted):

  ```
  confine: the job was SIGKILLed by something this supervisor cannot attribute: it sent no signal itself, and this scope's memory.events records no OOM kill. Candidates: an external whole-cgroup kill (systemd-oomd under PSI pressure, another session's `aira confine --kill`, a direct cgroup.kill write), an ancestor-cgroup OOM whose counter lands on the ancestor, an external kill -9, or the job killing itself.
  ```

  Emitted from its own small formatter so it is unit-testable, and printed as a
  separate line from `formatConfineReserveAdvisory`, whose cap gate and OOM
  wording stay untouched.

### `internal/daemon/confine_manage.go`

- After a successful `KillConfine` in the `confine-kill` case, one line
  (AIRA-70 #2), following the file-external `log.Printf("aira daemon: …")`
  convention used by `admit.go`/`governor.go`/`confine_reaper.go`:

  ```go
  log.Printf("aira daemon: confine-kill: killer=%s steal=%v target-scope=%s target-name=%s target-owner=%s slice=%s",
      callerOwner, steal, result.ScopeID, result.Name, result.Owner, path)
  ```

  Success only. A refused kill already returns a `E_CONFINE_OWNER_UNVERIFIED`
  error to its caller and kills nothing; the ticket asks for a line "every time
  it actually kills something".

### Tests

New/extended, all TDD (test first, watch it fail against current source):

1. `TestClassifyConfineTermination` — table over all seven branches, including
   both `unevaluated` routes and the SIGSEGV-is-not-a-SIGKILL case.
2. `TestFormatConfineStatusReportsTerminatedByFacet` — rendering, incl. the
   empty-field default.
3. `TestConfineTrailerReportsTerminationFacet` — unit-level, through
   `confineWithDeps` with `confineUnitDeps`, driving a **real child** to each
   terminal shape and asserting on the captured `Stderr`. **Five sub-cases, not
   one** (round-1 P2 — a trailer test with only the SIGKILL row passes against a
   classifier that hardcodes one verdict): `/bin/true` (`normal`);
   `/bin/false` (`normal` too — a non-zero exit is still a normal exit, and this
   row stops anyone conflating the facet with success); `sh -c 'kill -s SEGV $$'`
   (`child-signal:SIGSEGV`); `sh -c 'kill -s KILL $$'` with `readUsage`
   returning `OOMKill: 0` (`unattributed-sigkill` **plus** the candidates line);
   and the same target with `readUsage` returning a nil `OOMKill`
   (`unevaluated`, **and no candidates line**).

   **Every one of these targets terminates itself**, so no sub-case depends on
   the test delivering a signal at the right moment and none can flake (round-2
   P1). Using a self-inflicted SIGKILL to exercise `unattributed-sigkill` is
   also the honest thing: a self-kill genuinely *is* unattributable, and is one
   of the candidates §3.1 names. The real external-`cgroup.kill` mechanism is
   covered by test 6 against a real cgroup.
4. `TestConfineTrailerReportsSupervisorSignal` — the one test that must deliver
   a signal mid-run, so it is written to be deterministic rather than timed
   (round-2 P1): the target is `sh -c 'echo ready > "$1"; exec sleep 60' sh
   <marker>` (the `exec` matters — it leaves exactly one process, so nothing is
   orphaned when the leader dies), and a goroutine polls for `<marker>` before
   pushing SIGTERM into the injected `deps.signalSource` channel. Marker
   polling is the idiom the existing
   `TestConfineSignalHandlerInstalledBeforeStartCleansScope` already uses.
   Asserts `terminated-by=supervisor-signal:SIGTERM` and the handler's line, and
   **never the exit code** (§1's race). Note that `confineFakeScope.Kill()` does
   not kill the real child, so in this unit test the death comes from the
   forwarded SIGTERM — the production path additionally has `cgroup.kill` racing
   it, which is exactly why the exit code is not asserted.
5. `TestConfineTrailerReportsOOM` — `readUsage` returning `OOMKill: 1` on a
   self-SIGKILLed child yields `terminated-by=oom` and **no** candidates line,
   with `ScopeMemoryMax` unset so the row also pins AIRA-70 finding #3: the
   uncapped OOM is now reported on the status line even though
   `formatConfineReserveAdvisory` stays silent. A second sub-case — `/bin/true`
   with `OOMKill: 1` — pins the branch-2 `signalled` requirement (§3.2): a
   descendant's OOM must not relabel a clean exit as `terminated-by=oom`.
6. `internal/runner/external_cgroup_kill_probe_linux_test.go` — **extend**, do
   not re-commit: a third test running the same `externalCgroupKillProbe`
   helper and asserting the real wait status + real `memory.events` classify as
   `unattributed-sigkill` and render into the status line. This is the plan's
   named reviewer-reproducible verification. The existing two tests' comments
   are updated to the corrected vocabulary (§3.1).
7. `internal/daemon` — a test that the `confine-kill` dispatch logs the killer
   on success (capture `log` output), and does not log when the kill fails.

## 5. False-pass directions the build review must probe

- A classifier that returns `unattributed-sigkill` for **everything** would pass
  a single-row probe extension. Test 3's four sub-cases, test 5, and the SIGSEGV
  and `unevaluated` rows of test 1 are the guards.
- A `TerminatedBy` that is set but never rendered (or rendered from a stale
  zero value) — tests 2, 3 and 5 must assert on the *formatted line*, not the
  struct field alone.
- The candidates line printing on a verdict other than `unattributed-sigkill`
  (test 3's `unevaluated` sub-case and test 5 are the negative assertions).
- The supervisor log line firing twice on a repeated signal, or firing on a
  signal that arrives before `diagnostics` exists (nil write — removed by the
  hoist in §4, and the build review should confirm the hoist actually happened
  rather than a nil-check being bolted on).
- The daemon log line firing on a refused kill (a "killed" claim for a kill that
  did not happen — exactly the fabricated-outcome class AIRA-68's populated-gate
  exists to prevent).
- **Mutation checks the build review must actually run**, each expected to turn
  a specific test red: swap classifier branches 5↔7 (test 1's SIGSEGV row);
  swap 2↔3 (test 5); swap 3↔4 (test 4's caught-signal shape); drop the
  `signalled` requirement from branch 2 (test 5's `/bin/true` sub-case); delete
  branch 6 so an unreadable counter falls through to 7 (test 3's `unevaluated`
  sub-case); make `FormatConfineStatus` ignore `TerminatedBy` (test 2).

## 6. Deferrals

- Distinguishing a whole-cgroup kill from a single-PID external `kill -9`, and
  either from a self-inflicted SIGKILL (§3.1, accepted ambiguity — named on the
  candidates line instead). Disambiguating would need the scope monitor's poll
  history to witness whether every member died at once, which is timing-derived
  evidence and exactly the machinery `[[architectural-simplicity]]` says not to
  stack on a telemetry-only signal.
- Attributing an ancestor-slice OOM (counter lands on the ancestor). Would need
  the daemon to read `aira.slice`'s own `memory.events` and correlate by time;
  named as a candidate instead.
- The microsecond window in §3.3: a supervisor signal arriving between the
  child's own death and `cmd.Wait()` returning is reported as
  `supervisor-signal:`. Closing it would need the wait and the signal handler to
  share a select, i.e. a rewrite of the wait path for a window no operator can
  observe.
- Any change to `formatConfineReserveAdvisory` (§2).
- AIRA-91 Part B (owner decision).

## 7. Expected yield

Closes AIRA-70 and AIRA-91 Part A. Every confined job's death becomes
attributable to one of five named causes or honestly reported as `unevaluated`,
and the two kill paths that left no trace anywhere now write one log line each.

## 8. Deviation record

Two things this plan does differently from §3.6 of the backlog plan, both
deliberate and both argued above rather than dropped quietly:

1. **`unattributed-sigkill` replaces `external-cgroup-kill`** as the facet value
   for AIRA-91's case (§3.1), on a plan-review P0. The behaviour §3.6 asked for
   — a trailer that no longer reads like a clean run, naming the realistic
   sources — is delivered in full; only the claim strength changes, from
   asserting one mechanism to naming the candidate set. Anything downstream that
   greps for the old token (nothing does today outside the probe's own comment)
   would need updating.
2. **`formatConfineReserveAdvisory` is not changed**, so AIRA-70's companion ask
   (its `scopeMemoryMax <= 0` gate) is met by `terminated-by=oom` printing
   unconditionally instead (§2). Test 5 pins that.
