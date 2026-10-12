# AIRA goal-a small-faces batch: enum dispatch, TUI shutdown, confine listing and trailer, gate escaping

Status: PLAN v1 (2026-10-12). Batch of small independent tickets in one change (owner rule:
batch small fixes, one review and test cycle). Tickets: AIRA-195 (listing-signal half only),
AIRA-208, AIRA-210, AIRA-253, AIRA-293, AIRA-294, AIRA-299. Base: master `4098ad9`, whose code
is identical to tag `v0.34` (`ec16b22`); the commits since touch only `.aira/` ticket files.

**Correctness-sensitive items: AIRA-210 (a new check on every dispatched request, on every
face) and AIRA-253 (the TUI shutdown path, shared by `aira tui`, `top`, `board`).** They get
the two-loop adversarial build review. The rest are small face fixes.

## 1. Problem, in plain terms, and what still reproduces

Every item was re-checked against `4098ad9`. Probes ran a binary built from that commit in a
throwaway detached worktree, with `HOME`/`XDG_*` pointed at a private directory under `~/tmp`
and a private daemon (never the box daemon). Ticket line numbers are stale; the current ones
are used below.

| Ticket | Still reproduces? | Evidence (2026-10-12) | Verdict |
|---|---|---|---|
| AIRA-195 (listing half) A nested confine's work outlives a killed outer scope, and `confine --list` cannot tell it from a healthy job | Yes, by code | AIRA-183 (PR #115) added `supervisor_live` to each record (`internal/runner/confine_manage_linux.go:256-268`, a `kill(pid,0)` probe that runs for every scope). But the LIVE column answers `yes` for any populated subtree **before** it looks at the supervisor (`cmd/aira/main.go:3747-3761`), and `aira top`'s cell does the same (`cmd/aira/tui_top.go:1004-1009`). A running scope whose supervisor is dead therefore renders `LIVE=yes`, the same as a healthy job. `--list --json` already carries `supervisor_live:false`. Only the table and `top` hide it. The premise still holds: a nested confine still moves to a sibling scope (`cmd/aira/confine_nested.go:9`). | Build (display only) |
| AIRA-208 `cpu-weight=aging` carries no number; a mistyped schedule is dropped silently | Yes, measured | The build trailer from this planning session printed `... priorities=applied cpu-weight=aging scope-memory.max=...`. The facet value is the two-valued enum (`internal/runner/confine.go:168-173`, rendered at `:1109-1113`). `confineCPUWeightConfig` (`internal/runner/confine_linux.go:2194-2222`) throws the curve away after arming (`:846-851`), and `if parsed, err := parseCPUWeightSchedule(raw); err == nil` (`:2202`) drops a bad schedule with no trace. | Build |
| AIRA-210 Declared closed enums are enforced on no CLI path, and differently on each face | Yes, measured | 41 dispatch args declare `ArgSpec.Enum` (enumerated by a probe over `New(nil).verbs`: 12 are `subverb` positionals, 2 are `install`). `(*Core).Do` (`internal/core/core.go:560-576`) calls `spec.Run` with no enum check. Measured on the CLI: `aira mv PRB-1 in_progress` gives `E_TRANSITION_INVALID: planned -> in_progress is not a legal transition; ...`, exit **1**. `aira rant "x" --severity P1` gives `E_RANT_INVALID: severity is invalid`. `aira req ls --status Bogus` is **accepted** (rc 0). Over MCP the same faults give a JSON-RPC `-32602` with `E_ARGUMENT_INVALID: argument "severity" is outside its closed enum`, naming no value and no set, and `"severity":"Annoyance"` is refused there while the CLI accepts `--severity Annoyance` (rc 0). `aira set PRB-2 status=in_progress` also gives `E_TRANSITION_INVALID` (exit 1), from `internal/store/store.go:2328-2332`. | Build |
| AIRA-253 A cancel that lands before `app.Run()` has a screen leaves a live screen that nothing stops | Yes, measured | `coordinateShutdown` (`cmd/aira/tui.go:517-528`) calls `app.Stop()` from its own goroutine. tview v0.42 `Stop()` returns at once when `a.screen == nil` (pre-Run), and when a screen was injected it discards that screen. `Run()` then makes a **new** screen (`application.go:279-300`), and nothing will ever stop it. Probe (in-process: injected SimulationScreen, ctx cancelled, coordinator run to completion before `Run`, `TERM` unresolvable): `Run returned err=couldn't open terminfo ($TERM) file ...`. The injected screen was gone and Run tried to bring up a real terminal. In production (no injected screen) the pre-Run `Stop()` is a no-op, and `Run` brings up a screen whose pump, coordinator and signal loop have all exited. | Build |
| AIRA-293 `confine --help` lists `confine-status`, but that spelling is refused | Yes, measured | `aira confine --help` lists `"verb":"confine-status"` (`confineHelpVerbs`, `cmd/aira/main.go:1529`). `aira confine-status` gives `E_UNKNOWN_VERB: unknown verb "confine-status"`, rc 2, from `buildRequest`'s default arm (`:3253`). The other four listed spellings (`confine-list/-kill/-budget/-dump`) are routed (`:375-403`). | Build (accept it) |
| AIRA-294 `__supervise` with an unparseable `--ready-fd` writes to fd 0 and closes it | Yes, by code | `runSupervisor` (`cmd/aira/main.go:661-664`): `strconv.Atoi("x")` returns `0` with an error, and the malformed-arguments branch calls `writeSupervisorFailure(readyFD=0, ...)`, which wraps fd 0, writes JSON to it and closes it (`:752-762`). A **missing** `--ready-fd` takes the same path (`Atoi("")` is `0`). `readyForFailure := supervisorReadyFD(argv)` (`:642`, returns -1 here) is already in scope. | Build |
| AIRA-299 Gate files escape `&`, `<`, `>` | Yes, measured | `aira gate add probe-gate ... --argv 'test 1 -lt 2 && echo "<ok>"'` wrote `"test 1 -lt 2 && echo \"<ok>\""` into `.aira/gates/probe-gate.json`. `RenderGate` uses `json.MarshalIndent` (`internal/gate/gate.go:225`). The canary writer `renderCanary` does the same (`internal/store/gate_write.go:23`), and canary `mutation_content` bodies are source code, full of `&` and `<`. No committed gate or canary file exists in aira or fastest-ee (checked), so no file on disk changes. | Build (gate and canary files) |

Nothing is dropped whole. Sub-items dropped or replaced are listed in section 7.

## 2. Greenfield minimum, and rejected alternatives

The bare-minimum build for each problem, solved alone and from nothing:

- **195:** the data exists (`supervisor_live`). Give "running, but its supervisor is gone" its
  own LIVE word in the one table and the one `top` cell that hide it.
- **208:** carry the curve that was actually installed into the trailer facet that already
  exists, and name a rejected schedule there. No new surface.
- **210:** one check, in the one place every core-routed request passes (`Do`), driven by the
  declarations that already exist. It folds case, rewrites to the declared spelling, and
  refuses naming the value and the set. Then delete the per-face copies.
- **253:** call `Stop()` only from inside tview's event loop. A queued update runs only once
  `Run()` is driving a live screen, so it cannot be lost to an early cancel. It also never runs
  after a failed screen init.
- **293:** route the hyphenated spelling to the function the flag spelling already uses.
- **294:** use the already-computed `readyForFailure` in the one branch that uses the
  unparsed value.
- **299:** the AIRA-216 encoder (`SetEscapeHTML(false)`) in the two file writers. Leave the
  digests alone.

The plan below is this minimum. Its consolidations delete code: four enum copies are removed;
the subverb `"list"` handler arms become dead and are removed; one domain message formatter
replaces two.

| Item | Alternative | Why rejected |
|---|---|---|
| 195 | Auto-kill or auto-reap a populated scope whose supervisor is dead | Judgement: the ticket and the project rule forbid deciding a running job is disposable. The listing reports; the operator acts (`confine --kill <scope-id>` already works on it). |
| 195 | Let supervisor liveness override population (render `orphaned` for populated scopes) | `orphaned` already means "empty, dead supervisor, the reaper removes it, nothing to kill" (AIRA-183 legend). Reusing it for a running scope would tell an operator there is nothing to kill when there is. A new word is needed. |
| 195 | A new JSON field | `supervisor_live` is already in `--list --json`. Only the human faces lack it. |
| 208 | Separate `cpu-weight-curve=` facet | Two facets for one control. Keeping the enum as the leading token of the existing facet (`aging(...)`) leaves every substring/prefix reader working (checked: in-repo tests use `strings.Contains(..., "cpu-weight=aging")`; fastest-ee has one fixture string and no parser of this facet). |
| 208 | List every step in the facet | Long, and the default curve is documented (section 3.2). The endpoints plus `wall=` answer RANT-32's question for the default. A custom schedule is the caller's own input. |
| 208 | Refuse a bad schedule | The Slice-1 plan decided "never inert-by-accident": keep the compiled default. Only the silence changes. |
| 208 | Report each step's write outcome | Steps are fail-open writes to an open scope fd. Recording each needs a result channel out of the decay goroutine for a failure seen nowhere. The facet claims "this curve was installed (its first write succeeded) and armed", no more. Written down as a gap. |
| 208 | An extra diagnostics line for a rejected schedule | The trailer already goes to stderr. One place, not two. |
| 208 | `CPU-WT` column on `confine --list` | Deferred by the ticket. |
| 210 | Widen the five subverb declarations with `"list"` (the ticket's suggestion for handler-accepted values) | Subverb enum values become MCP operations (`cmd/aira/mcp.go:138-146`), so `aira_finding`, `aira_spend`, `aira_quota`, `aira_test_report` and `aira_commands` would each gain a duplicate `list` operation, and help would advertise both spellings. Instead the CLI builders, which already lower-case the subverb, also map `list` to `ls`. No CLI spelling that works today is refused. |
| 210 | Keep the MCP check and make its text identical | Two copies of one rule, which is the defect. |
| 210 | Check in `daemonDispatcher.Dispatch` | It misses in-process `Do` callers (tests, the daemon's own handling). `Do` is the only caller of `spec.Run` (grep: `core.go:573`). |
| 210 | A second call for `confine-log` (the one enum-bearing verb answered client-side, never via `Do`, `cmd/aira/dispatcher.go:183-185`) | Its runner check (`internal/runner/confine_log.go:96`) already names the value and explains the set, and every face reaches it through the same dispatcher arm, so faces agree. A second call site for one verb is not worth it. Recorded as the one exception. |
| 210 | Reorder `MoveTicket` (`internal/store/mutate.go:69`, the ticket's `:58`) | Its only caller is the `mv` handler, which sits behind `Do`, so the new check refuses first. The same misreport on `set status=` comes from the shared write path `UpdateTicketContent` (`store.go:2328-2332`), which is fixed instead (3.3). |
| 210 | Delete the ten handler-side `strings.ToLower(subverb)` folds | Not enforcement copies. After `Do` canonicalises they are no-ops. Deleting them changes no behaviour and adds risk if a future path reaches a handler another way. |
| 210 | Exact-case matching | Would refuse `--severity Annoyance`, `--format GO-JSON` and `rant LS`, which work today (measured for the first). |
| 253 | Retry `Stop()` until `Run()` returns | Polling. It also does not fix the injected-screen case: the first pre-Run `Stop()` already discarded the screen, so `Run()` tries to open a real terminal before any retry lands. That is the probe's failure, and on a developer's tty it would grab the terminal. |
| 253 | Check `ctx.Err()` before `Run()` | A window remains between the check and `Run` taking tview's lock. Not deterministic. |
| 253 | Create and `Init` the screen ourselves, then `SetScreen` | A pre-Run `Stop()` still discards it, and `Run` makes another. |
| 253 | A draw hook that signals "screen up" | The board's `SetBeforeDrawFunc` can short-circuit the draw, and it needs composing with that hook. More code than the queued stop, for the same effect. |
| 293 | Stop listing `confine-status` | That entry carries the only `confine --status [...]` usage line in `confine --help`. Removing it hides `--status`. Accepting the spelling matches the four siblings' "both spellings work" discipline (AIRA-201). |
| 299 | Also switch `DigestGate`/`DigestCanary` to the new encoder | Changes every `definition_digest` and `declaration_digest`, so every existing attestation and proof would read stale (`internal/store/gate_eval.go:838-856`). |
| 299 | Rewrite existing gate files | None exist in aira or fastest-ee. `ParseGate` reads both forms. |

## 3. Design

### 3.1 AIRA-195 (listing half): `LIVE=unsupervised`

`cmd/aira/main.go`:

- New constant `confineLiveUnsupervised = "unsupervised"` beside `confineLiveIdle` and
  `confineLiveOrphaned` (`:3724-3725`).
- `confineLiveStatus` (`:3747`): when `SubtreePopulated` is true, return
  `confineLiveUnsupervised` if `SupervisorLive != nil && !*SupervisorLive`, else `"yes"` as
  today. Unpopulated, nil-population and nil-supervisor arms are unchanged. Order of questions:
  population first (nil means unevaluated), then supervisor. The populated arm is refined
  exactly as AIRA-183 refined the empty arm.
- `confineLiveLegend` (`:3766`) gains a third clause, printed only when a row shows the word:
  `unsupervised = processes are still running in the scope but its supervisor PID is gone, so
  nothing is waiting on the job or will report its outcome (for example an outer confine was
  killed around a nested one); it runs until it exits or is killed`. (The legend makes no
  claim about the admission reserve; whether the ledger still books it is not read here.)
- The doc comment records why: `supervisor_live` is false only on `ESRCH`, so the word is
  established when shown. A recycled PID can make a dead supervisor read alive, which shows
  `yes` (no claim). That is the AIRA-183 caveat, unchanged. A live admission lease with a dead
  reading is already turned into `unevaluated` at the scan (`confine_manage_linux.go:260-262`),
  so it also shows `yes`.

`cmd/aira/tui_top.go`, `topLiveCell` (`:1004`): after `pending`, return
`confineLiveUnsupervised` for a populated subtree whose supervisor reads dead. The comment at
`:993-1003` is corrected. Its reasoning ("a scope whose supervisor has died simply stops
changing and then disappears") holds for an empty scope only; a running orphan keeps changing
and is never reaped.

JSON output is unchanged. No new probe, no new field, no actuator.

### 3.2 AIRA-208: the installed curve on the trailer

`internal/runner/confine_linux.go`:

- `cpuWeightConfig` gains `ScheduleRejected bool`. In `confineCPUWeightConfig` (`:2201-2205`), a
  non-empty `AIRA_CONFINE_CPUWEIGHT_SCHEDULE` that `parseCPUWeightSchedule` refuses sets it.
  The steps stay the compiled default, as today.
- New method `(c cpuWeightConfig) curve() string`: `fmt.Sprintf("%d->%d@%s", c.Start,
  c.Steps[len(c.Steps)-1].Weight, c.Steps[len(c.Steps)-1].After)`, plus `",schedule-rejected"`
  when `ScheduleRejected`. It is built from the **post-clamp** steps that are written, and the
  last step's weight is the floor (`:2219`). Durations print with `time.Duration.String()`, as
  the deadline facets do (`confine.go:1238`), so the default is `100->10@30m0s`.
- Arming (`:846-851`): when the first write succeeds, also set
  `result.Status.CPUWeightCurve = config.curve()`.

`internal/runner/confine.go`:

- `ConfineStatus` gains `CPUWeightCurve string \`json:"cpu_weight_curve,omitempty"\`` beside
  `CPUWeight` (`:278`). `cpu_weight` keeps its two-valued enum.
- `FormatConfineStatus` (`:1109-1113`): `cpu-weight=aging(<curve>)`. When `CPUWeight` is
  `aging` and the curve is empty (a record written by an older binary), render
  `cpu-weight=aging(unevaluated)`, never a guessed curve. `unavailable` is unchanged and carries
  no parentheses.

Docs: `internal/core/skill.go`, next to the trailer facets (`:329`), one sentence. Every confine
scope ages `cpu.weight` from 100 to 10 over 30 minutes (steps at 10s, 30s, 1m, 5m, 10m, 30m).
The trailer's `cpu-weight=aging(START->FLOOR@WHEN)` is the curve actually installed, with
`,schedule-rejected` when `AIRA_CONFINE_CPUWEIGHT_SCHEDULE` did not parse and the default ran.
Weight arbitrates between scopes only, so workers inside one scope share equally.
`AIRA_CONFINE_CPUWEIGHT_FLOOR=100` turns aging off. The aitest worker scopes' static floor
write (`worker_scope_linux.go:139`) is unchanged and not reported (deferred).

### 3.3 AIRA-210: one enum check in `Do`

**Shared formatter.** `internal/domain` gains `EnumValueError(code, subject, value string,
allowed []string) error`, which returns `"<code>: <subject> %q is invalid; allowed: a, b, c"`.
It is the exact shape of today's `invalidFieldError` (`ticket.go:209-211`), which is deleted.
Its three callers (`:227/:230/:233`) pass `CodeTicketInvalid, "ticket "+field`, so their text is
byte-identical to today (pinned).

**The check.** `internal/core` (new file `enum_args.go`): `canonicalEnumArgs(spec verbSpec, in
map[string]any) (map[string]any, error)`. For each `spec.Args` entry with `len(Enum) > 0` whose
key is present in `in` with a non-nil value:

- not a `string`: refuse, rendering the value with `fmt.Sprint`;
- `strings.TrimSpace(v) == ""`: leave it if `!Required` (absent semantics, as MCP's rule today,
  `mcp.go:624-626`); refuse if `Required`;
- otherwise the first member equal under `strings.EqualFold` to the trimmed value is written
  back in its **declared** spelling; no match: refuse.

A refusal is `domain.EnumValueError("E_ARGUMENT_INVALID", spec.Name+" "+arg.Name, raw,
arg.Enum)`. `raw` is the value exactly as received (untrimmed, unfolded), `spec.Name` is the
canonical verb, and `arg.Name` is the declared name (as MCP names it). The function writes into
a shallow copy, so the caller's map is never mutated. Measured examples:
`E_ARGUMENT_INVALID: mv status "in_progress" is invalid; allowed: draft, planned, in-progress,
in-review, done, retired, superseded` and `E_ARGUMENT_INVALID: rant severity "P1" is invalid;
allowed: papercut, annoyance, blocker`.

`(*Core).Do` (`core.go:560`): right after the spec lookup, call it. On error, return
`Response{Code: "E_ARGUMENT_INVALID", Error: err.Error(), Exit: 2}` before any accessor is built
or any handler runs. Otherwise use the returned map for `newArgAccessor`. `Do` is the only
caller of `spec.Run`, and it runs wherever a core-routed request is served (daemon or client
route), so every face inherits it.

**Deletions (the copies).**

- `cmd/aira/mcp.go:623-629`: the enum block in `decodeMCPValue`. The type checks stay. An MCP
  enum fault now reaches `Do` and comes back as an ordinary tool result (`isError:true`) with
  the shared text, not a JSON-RPC `-32602`. The tool schema keeps advertising `enum`.
- `cmd/aira/tui_palette.go:174-176`: the palette's pre-check. A bad value is submitted, and the
  palette shows `Do`'s refusal.
- `internal/domain/rant.go:149-151`: the severity fold and check. `AddRant`'s only caller is the
  rant handler behind `Do` (grep). The SQLite `CHECK` (`store.go:938`) remains the storage
  backstop. The rant declaration (`core.go:794`) is built from `domain.RantSeverity*`
  constants, so the domain constants and the declaration are one list.
- `mv`'s hand-written status list (`core.go:1686`) becomes `domain.AllowedStatusStrings()...`
  (same order).

**No CLI spelling is narrowed.** `cmd/aira/main.go` `buildRequest`: in the `find` (`:2884`),
`spend` (`:2958`), `quota` (`:2990`) and `test-report` (`:3037`) arms, which lower-case
`positional[0]` and accept `list` today, the subverb is mapped `list -> ls` before it is stored.
The `"list"` arms in the core handlers become unreachable and are removed: rant `:839`, find
`:1130`, spend `:1289`, quota `:1363`, test-report `:1396`, `command.go:348`. MCP never sends
`list` (operations come from the declarations), and the CLI `rant` arm already maps it.

**Declaration audit (constraint b).** Before the check is added, the builder records, for each
of the 41 declarations, the handler-accepted set against the declared set, in the build record.
Planning checked `commands.by` (store `valid` map = declaration), `run.report_stream`
(`run_wiring.go:318-339` = declaration), `test-report.format` (`testreport_parse.go:236-238`),
`create.kind/severity`, `mv.status`, `rant.severity`, `link/unlink.kind` (all 11 declared kinds
are known; the five inverse kinds are accepted by the vocabulary and refused by the handler with
AIRA-291's forward-command message, which must survive), and the subverbs. The only mismatch
found is the `list` alias, handled above. If the audit finds another, widen the declaration.

**Non-dispatch paths keep their own checks, with the shared text.**

- `internal/store/store.go:2328-2332` (`UpdateTicketContent`): move the transition check
  **after** `domain.RenderTicket` (`:2333`), which validates the ticket. `aira set X
  status=in_progress` then says `E_TICKET_INVALID: ticket status "in_progress" is invalid;
  allowed: ...` (exit 2), and `E_TRANSITION_INVALID` is left for a legal token on a forbidden
  edge. `MoveTicket`'s own pre-check (`mutate.go:69`) stays: it is what refuses a same-status
  `mv`.
- `internal/domain/finding.go:182-184`: split the collapsed `finding enum is invalid` into three
  `EnumValueError("E_FINDING_INVALID", "finding severity|verdict|disposition", ...)` calls
  (`:209` included). These are reached by findings import, which bypasses dispatch.
- `internal/install/install.go:482-488`: install is intercepted before any request exists
  (`main.go:77`), so its parser stays the enforcer. Its two messages become
  `domain.EnumValueError(CodeArgumentInvalid, "install --watchdog"|"install --slice-ceiling",
  value, []string{"off","observe","enforce"})`, which names the value.
- `confine-log --stream` keeps its runner check (section 2).

### 3.4 AIRA-253: stop from inside the event loop

`cmd/aira/tui.go`, `coordinateShutdown` (`:517-528`). After the existing waits (executor,
pump) and the existing `appRunDone` pre-check, replace `r.app.Stop()` with
`go r.app.QueueUpdate(r.app.Stop)`, and return.

- Cancelled before `Run()` has a screen: the update waits in tview's buffered `updates` queue,
  and `Run()` creates its screen (or uses the injected one, which is no longer discarded),
  draws, enters its loop, and runs `Stop()`. `Run()` returns `nil`. Deterministic exit, no dead
  screen.
- Cancelled while `Run()` is live (q, SIGINT, SIGTERM): same as today. The stop now runs on the
  loop goroutine instead of the coordinator's.
- `Run()` failed at screen init (AIRA-134): the loop never runs, so `Stop()` never touches the
  uninitialised screen, whatever the timing. The pre-check still skips the queue in the common
  order (Run returned before the cancel), so no goroutine is left waiting.
- `Run()` exits by itself after the update is queued (for example tview's built-in Ctrl-C
  `Stop`): the helper goroutine stays blocked on `QueueUpdate`'s reply in a process that is
  exiting. This is the same accepted, abandonable pattern as the pump (`:418-424`).

The doc comment is rewritten. `appRunDone` remains, as the pre-check only; its "Stop is unsafe
after Run" role is now structural.

Test seam: `tuiRuntime.beforeAppRun func()`, nil in production and set only by tests. It is
called in `run()` between `go r.coordinateShutdown()` and `r.app.Run()`, in the same style as
the existing `queueUpdateStarted` seam.

### 3.5 AIRA-293: accept `aira confine-status`

`cmd/aira/main.go`:

- The `allowed` map (`:941-947`) gains `"confine-status": {"owner": true}`.
- The confine-family routing (beside `:375`) gains: `if verb == "confine-status"`, refuse more
  than one positional with `E_CONFINE_ARGUMENT_INVALID: confine-status takes at most one
  selector`; else set `options["status-selector"] = positional[0]` if present, and
  `return runConfineStatusCommand(ctx, options, renderJSON, stdout, stderr)`. This is the
  exact call the flag form makes (`:313-315`), which reads only `status-selector` and `owner`.
- `cmd/aira/scope_dir.go:154`: add `confine-status` to `verbAcceptsScopeDir`'s project-less
  list, as `confine --status` refuses the override today.

### 3.6 AIRA-294: never report on an fd that was not parsed

`cmd/aira/main.go:663`: `writeSupervisorFailure(readyForFailure, ...)` in the malformed-numbers
branch. The later calls (`:668`, `:677`, `:690`, `:706`) run only after a successful,
non-negative parse, so `readyFD` is correct there (audited). No other change.

### 3.7 AIRA-299: literal `&`, `<`, `>` in gate and canary files

`internal/gate`: new `marshalIndentLiteral(v any) ([]byte, error)`: a `json.Encoder` with
`SetEscapeHTML(false)` and `SetIndent("", "  ")`, with the trailing newline trimmed, so the
output equals `json.MarshalIndent` except for the three characters. It is exported as
`MarshalIndentLiteral` for the store. `RenderGate` (`gate.go:225`) uses it, as does
`renderCanary` (`internal/store/gate_write.go:23`). `DigestGate` and `DigestCanary`
(`digest.go:19-41`) are **not** touched.

**Effect on existing digests, stated:**

1. `definition_digest` and `declaration_digest` do not change. Both hash `json.Marshal` of the
   parsed struct (`digest.go:24,31`), with default escaping, independent of the file's bytes.
   `ParseGate` decodes `&` and `&` to the same Go string. Existing attestations and proofs
   stay valid. Pinned by golden digests computed on `4098ad9`.
2. Subject tree digests. Gate and canary files are inside every gate subject (only
   `.aira/tickets/` and `.aira/findings/` are excluded, `internal/gate/canary.go:227`). A gate or
   canary containing `&<>` gets new bytes the next time `gate add`/`gate set` rewrites it, and
   that changes the subject digest, as any edit to that file does today. Results for the old
   subject then no longer apply, by design. The upgrade itself rewrites nothing, and no such
   file exists in aira or fastest-ee.
3. The rebuildable `gates.definition_json` index text changes form at the next reindex. Nothing
   compares it (grep: only the `INSERT`, `gate_index.go:302`).

There is no compatibility obligation (AIRA not live). This section exists so the reader knows
the attestation chain is untouched.

## 4. Invariants

1. Every enum-declared argument that reaches `Do` is, before any handler runs, one of three
   things: absent (or empty and optional); a case-insensitive member rewritten to its declared
   spelling; or refused `E_ARGUMENT_INVALID` naming the verb, the argument, the value as typed
   and the whole declared set.
2. One fault, one refusal text, on the CLI, MCP and TUI faces (the text is `Do`'s). `confine-log
   --stream` is the documented exception: its own text, the same on every face.
3. No CLI spelling accepted on `4098ad9` for a subverb is refused (`list` is preserved). For
   non-subverb enums the CLI now refuses exactly what MCP refused, except that matching is
   case-insensitive on both.
4. Rant severity has one list: the declaration is built from the domain constants, and a test
   pins the SQLite `CHECK` to it.
5. `E_TRANSITION_INVALID` is returned only for two known statuses on a forbidden edge.
6. `LIVE=unsupervised` appears only when the subtree is populated **and** the supervisor read
   `ESRCH`. An unestablished supervisor makes no claim (`yes`). The JSON is unchanged.
7. The `cpu-weight` facet reports the curve actually installed (after clamping and the floor
   overwrite), only when the first write succeeded. A rejected schedule is named, and the
   compiled default still runs. An unknown curve reads `unevaluated`.
8. The TUI coordinator never calls `app.Stop()` outside tview's event loop. A cancellation at any
   time ends `Run()`, and a `Run()` that failed at screen init is never stopped.
9. `aira confine-status [sel]` and `aira confine --status [sel]` give byte-identical stdout,
   stderr and exit for the same arguments and environment.
10. `__supervise` never writes to or closes an fd it did not parse as a non-negative
    `--ready-fd`.
11. Gate and canary files contain `&`, `<`, `>` literally. `DigestGate`/`DigestCanary` output is
    byte-identical to `4098ad9` for the same definition.

## 5. Tests (TDD; each must fail against the wrong implementation)

Write each test first and record it RED on `4098ad9` (or RED against the named mutation where
the bug cannot fail today). Each mutation listed must turn at least one test RED. Every listed
test runs in `make ci`.

**AIRA-210** (`internal/core`, `cmd/aira`, `internal/domain`, `internal/store`):

- `TestEveryDeclaredEnumIsEnforcedAtDispatch`: walk `New(nil).verbs`. For each enum-bearing
  arg, `Do` with only `{arg: "__not-a-member__"}` must return `E_ARGUMENT_INVALID`, exit 2, with
  text containing `spec.Name`, `arg.Name`, `"__not-a-member__"` and every declared value. A nil
  store makes any handler call panic, so the test also proves no handler ran. RED today for all
  41. Mutations: skip `subverb` args (12 RED); check only `Required` args (RED); compare
  case-sensitively (next test RED).
- `TestEnumValuesCanonicalisedAtDispatch` (unit on `canonicalEnumArgs`): for every declared
  value `v`, `strings.ToUpper(v)` and `" "+v+" "` come back as exactly `v`. The input map is
  unchanged. An optional empty value passes untouched; a required empty value is refused
  (`mv` status `""`). A non-string is refused. The echo is raw: `"BLOCKERX"` is quoted as
  `"BLOCKERX"`, not `"blockerx"`. Mutations: no write-back; echo the folded value; skip empty
  values even when required.
- `TestUpperCaseEnumValuesStillAccepted` (CLI, in-process dispatcher): `rant "x" --severity
  Annoyance` stores `annoyance`; `rant LS`; `test-report ls --format GO-JSON`; `find list`,
  `spend list`, `quota list`, `test-report list`, `rant list` all succeed. Write it **before**
  the check and record it GREEN; it must stay GREEN. Also `mv X IN-PROGRESS` moves to
  `in-progress` (RED today; widening). Mutation: drop the CLI `list -> ls` mapping (four RED).
- CLI messages: `mv X in_progress` gives the full text in 3.3 and exit 2 (today: exit 1,
  `E_TRANSITION_INVALID`). `rant "x" --severity P1` names `P1` and the three severities. `req ls
  --status Bogus` is refused (today rc 0). `link A blockz B` gives `E_ARGUMENT_INVALID` exit 2.
  `link A blocked-by B` still gives AIRA-291's forward-command refusal (twin: the check must
  accept inverse kinds).
- `TestEnumRefusalIdenticalAcrossFaces`: the same faults (`rant` severity `P1`, `mv` status
  `in_progress`, `create` severity `p9`) through the CLI JSON envelope, the MCP `tools/call`
  result and the TUI palette submission give byte-identical `error` strings. Mutations: keep
  MCP's `decodeMCPValue` enum block (MCP returns `-32602`, RED); keep the palette check (RED).
- `TestRantSeverityOneList`: the rant declaration's enum equals `{papercut, annoyance, blocker}`
  built from the `domain.RantSeverity*` constants, and the `rants` table SQL from
  `sqlite_master` contains `CHECK(severity IN ('',` plus those values in order.
- `set X status=in_progress` gives `E_TICKET_INVALID: ticket status "in_progress" is invalid;
  allowed: ...`, exit 2 (RED today). `set X status=done` from planned still gives
  `E_TRANSITION_INVALID` with the legal-next list (twin). Mutation: leave the transition check
  before `RenderTicket` (RED).
- Ticket messages are byte-identical to today: `create --severity P9` gives `E_TICKET_INVALID:
  ticket severity "P9" is invalid; allowed: P0, P1, P2, P3` (pins the formatter refactor).
- Findings import with severity `P9`, verdict `maybe`, disposition `gone`: each refusal names its
  field, value and set (RED today: `finding enum is invalid`).
- `install --watchdog bogus` names `"bogus"` and the three modes (RED today).

**AIRA-253** (`cmd/aira`):

- `TestTUICancelBeforeRunStillExits`: SimulationScreen runtime, `t.Setenv("TERM",
  "aira-test-no-such-terminal")`. Precondition: `tcell.NewScreen()` must fail; if it does not,
  `t.Skip` with an `unevaluated:` reason, never a pass. `beforeAppRun = func() { cancel();
  <-r.coordDone }`. `run()` must return `nil` within 10 s. RED on master: the coordinator's
  pre-Run `Stop()` discards the injected screen, `Run` tries a real terminal and returns the
  terminfo error (measured).
- Mutations: restore the direct `r.app.Stop()` (RED). `TestTUIScreenInitFailureExitsCleanlyWithoutPanic`
  (AIRA-134, subprocess) and the existing smoke and quit tests stay GREEN.
- Accepted gap: the production variant (no injected screen, real terminal) cannot be reached
  safely in CI without allocating a pty. The in-process test exercises the same coordinator
  decision.

**AIRA-195** (`cmd/aira`):

- Table test on `confineLiveStatus`: populated plus supervisor dead gives `unsupervised`;
  populated plus alive gives `yes`; populated plus nil gives `yes`; the four existing rows
  unchanged. Same table on `topLiveCell` (plus `pending`).
- `renderConfineListResponse` with one unsupervised row prints the `unsupervised =` legend
  clause, and without one does not. The JSON for that record is byte-identical to today's.
- Mutations: ignore `SupervisorLive` when populated (RED); treat nil as dead (RED); legend
  printed unconditionally (RED).

**AIRA-208** (`internal/runner`):

- `FormatConfineStatus` with aging and curve `100->10@30m0s` contains
  `cpu-weight=aging(100->10@30m0s)` (RED today). Aging with an empty curve gives
  `cpu-weight=aging(unevaluated)`. Unavailable gives `cpu-weight=unavailable` with no parentheses.
- `confineCPUWeightConfig` with `SCHEDULE=garbage`: `ScheduleRejected` is true **and** the steps
  equal the clamped compiled default. The curve ends `,schedule-rejected` (first half RED today;
  second half pins the fail-safe).
- `SCHEDULE=1s:100,2s:50`, no FLOOR: curve `100->10@2s` (the installed floor overwrite).
  Mutation: build the curve from the unclamped parse (RED).
- `FLOOR=100`: every step weight is 100 and the curve is `100->100@30m0s` (the kill switch, at
  config level).
- `confineWithDeps` with a fake `writeScopeCPUWeight`: a successful first write sets the curve;
  a failed one leaves `unavailable` and no curve. Mutation: set the curve unconditionally (RED).
- A status JSON round trip keeps `cpu_weight:"aging"` and adds `cpu_weight_curve`.

**AIRA-293** (`cmd/aira`):

- Parity: with `XDG_RUNTIME_DIR` at a temp dir, `confine-status` against `confine --status`,
  `confine-status nosuch` against `confine --status nosuch`, and `--owner` with a bad value, each
  piped and on a terminal. All give byte-identical stdout, stderr and exit (RED today:
  `E_UNKNOWN_VERB`).
- `confine-status a b` is refused `E_CONFINE_ARGUMENT_INVALID`.
- `TestEveryConfineHelpVerbIsRouted`: for each verb in `confineHelpVerbs`, an invocation with an
  injected stub dispatcher never yields `E_UNKNOWN_VERB`. This generalises: a future
  listed-but-unrouted verb fails it.

**AIRA-294** (`cmd/aira`, not parallel):

- Save fd 0 with `syscall.Dup(0)`, then `syscall.Dup3(pipeW, 0, 0)` (`Dup3` exists on linux
  amd64 and arm64; `Dup2` does not on arm64). Restore in `t.Cleanup`. Call
  `runSupervisor(["--control","c","--ready-fd","x","--ack-fd","4"], io.Discard)`. Afterwards
  `fcntl(0, F_GETFD)` succeeds, and after restoring fd 0 and closing every write end the pipe
  reads 0 bytes. Same with `--ready-fd` absent. Both RED today (fd 0 closed, JSON written).
- Twin: `--ready-fd <pipe fd> --ack-fd x` writes the JSON failure to that pipe. Mutation: pass
  `-1` in that branch (RED).

**AIRA-299** (`internal/gate`, `internal/store`):

- `RenderGate` on a definition whose argv, name and description hold `a && b < c > d` writes
  those characters literally, and the bytes contain no `&`, `<`, `>` (RED
  today). The output equals `json.MarshalIndent` for a definition with no such characters
  (byte-for-byte; guards indentation and newlines).
- `renderCanary` with `mutation_content` `if a < b && c > d {}`: same (RED today).
- Golden digests: for a fixed definition and a fixed canary containing `&<>`, `DigestGate` and
  `DigestCanary` equal hex constants computed on `4098ad9` **before** any edit (record the
  command in the test comment). `ParseGate` of the old escaped bytes and of the new bytes are
  deep-equal, with equal digests. Mutation: switch `DigestGate` to the literal encoder (golden
  RED).

Gate: `aira confine -- make ci` (the gate's exact command), plus `go test -race ./cmd/aira/...
./internal/core/...`.

## 6. Expected yield

- A mistyped token anywhere (`mv X in_progress`, `--severity P1`, `--kind tickets`) is refused
  naming what was typed and what is allowed, in the same words on every face. 41 declarations
  stop being decoration on the CLI. `req ls --status Bogus` no longer silently returns nothing.
- `E_TRANSITION_INVALID` (exit 1) again means only "a forbidden move", not "I could not read
  your status".
- An operator who kills an outer gate can find its still-running nested legs: `confine --list`
  and `aira top` say `unsupervised` instead of `yes`.
- The next RANT-32-style question ("did aging starve this job?") is answerable from the CI log:
  the trailer carries the curve, and a mistyped schedule is visible.
- `aira tui/top/board` cannot be left on a dead screen by an early Ctrl-C or SIGTERM.
- `confine --help` no longer lists a spelling that fails.
- `__supervise` can no longer close its own stdin on a malformed call.
- Gate and canary files are readable, and diff cleanly, with shell and source text in them.

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| AIRA-195 remainder: a faster reaper state for "populated, supervisor dead", group-kill reaching self-confined siblings, AIRA-194's zero-reserve orchestrator | Out of the brief (listing signal only). The ticket stays open for them; this batch removes AIRA-194's "orphans are invisible" precondition only for the human faces (JSON already had it). |
| `CPU-WT` column on `confine --list`; aitest worker scopes' static floor on any surface; a `cpu_weight` key in the AIRA-281 summary file | Ticket defers the column; the other two are new surfaces. |
| AIRA-208: per-step write outcomes | Accepted gap (2). |
| AIRA-210: deleting handler-side subverb folds; `confine-log --stream` routed through the shared check | Section 2. |
| AIRA-210 (ticket's `mutate.go:58` reorder) | Replaced by the `UpdateTicketContent` reorder, the shared write path. |
| Skill text says ci-shim trailers read `cpu-weight=unevaluated` (`skill.go:327`); by code the shim shares `confineWithDeps`'s initial status (`confine_linux.go:462-466`) and never sets the weight, so it prints `unavailable` | Observed by code reading only, not run in a shim. The 3.2 sentence states `unavailable` for cpu-weight. The other facets in that sentence are unverified; suggest a ticket. |
| AIRA-253 production variant under a real pty | Accepted test gap (section 5). |

## 8. Risks

- **Exit and code changes (210).** `mv` with a bad status and `link`/`unlink` with an unknown
  kind move from exit 1 (`E_TRANSITION_INVALID`, `E_RELATION_INVALID`) to exit 2
  (`E_ARGUMENT_INVALID`). `set status=<bad>` moves from exit 1 to exit 2 (`E_TICKET_INVALID`).
  Rant severity, grep kind/by, and the TUI palette move to `E_ARGUMENT_INVALID` (exit 2
  throughout). Peers were not grepped for these exact codes. They are usage errors that only a
  typo reaches.
- **MCP error channel (210).** An enum fault becomes a tool result with `isError:true` instead of
  a JSON-RPC `-32602`. MCP clients show both. Any test pinning `outside its closed enum` is
  updated.
- **CLI tightening and widening (210).** Non-subverb enums are now enforced on the CLI (MCP
  already did), and matching is case-insensitive on both (`create --severity p1` is now `P1`).
  The audit (3.3) is the guard against refusing a value a handler accepts.
- **Version skew (210).** `Do` runs in the daemon for daemon-routed verbs. A new client with an
  old daemon sends MCP enum faults to handlers (today's CLI behaviour) until the daemon
  restarts. An old client with a new daemon would have `find list` refused, because the old CLI
  sends `list`. `aira install` restarts the daemon when its binary changes (AIRA-203, v0.31), so
  the window is the cutover only. Protocol unchanged.
- **Trailer text (208).** `cpu-weight=aging` becomes `cpu-weight=aging(100->10@30m0s)`. Prefix
  and substring readers are unaffected. A reader matching `cpu-weight=aging ` with a trailing
  space would miss: none found in aira, fastest-ee or stoner.
- **TUI shutdown (253).** The stop moves to the loop goroutine. Review should confirm that no
  path blocks the event loop forever while a stop is queued; the executor and pump are joined
  before queuing, as today.
- **Skill text** (208's sentence) needs a separate `aira skill install ~/.claude/skills/aira
  --force` at release.

## 9. Stable codes

No new codes. `E_ARGUMENT_INVALID` (exit 2, already catalogued, already MCP's code) is now
emitted by `Do` for every declared-enum fault on every face. `E_TICKET_INVALID`,
`E_FINDING_INVALID` and `E_INSTALL_ARGUMENT_INVALID` keep their codes with value-naming text.
`E_TRANSITION_INVALID` keeps its code, with a narrower meaning (invariant 5).
`confine-status` refusals reuse `E_CONFINE_ARGUMENT_INVALID` and the codes `confine --status`
already returns. AIRA-195 adds the LIVE token `unsupervised`. AIRA-208 adds the facet suffix
`(START->FLOOR@WHEN[,schedule-rejected])` and the JSON key `cpu_weight_curve`.

## 10. Release notes (for the build)

Proto unchanged. A daemon restart is required for 210's `Do` check on daemon-routed verbs;
`aira install` performs it. Skill reinstall is required (208's sentence). Notify deploy and
speed, naming these changes:

- the `cpu-weight=` facet format;
- `LIVE=unsupervised`;
- exit 1 to exit 2 for `mv`, `link` and `set status` token typos;
- MCP enum faults now arriving as tool errors.
