# AIRA v0.31 small-fixes batch: install restart, oversized daemon replies, help, CLI streams

Status: PLAN v2 (2026-10-10; v1 plus the plan-review fixes in section 10). Batch of ready bug/UX tickets in one change (owner rule:
batch small independent fixes, one review and test cycle). Tickets: AIRA-203, AIRA-242,
AIRA-280, AIRA-211, AIRA-212, AIRA-215, AIRA-207, AIRA-214. Base: master `fab2647`.

**Correctness-sensitive items: AIRA-203 (install restarts the machine daemon) and AIRA-280
(daemon reply path and a paged wire shape for two verbs).** They get the two-loop
adversarial build review; the rest are CLI-face fixes.

## 1. Problem, in plain terms, and what still reproduces

Every ticket was re-checked against master `fab2647` (the installed client and daemon are
both `fab2647`, per `aira version`). Ticket line numbers are stale; the current ones are
used below.

| Ticket | Still reproduces? | Evidence (2026-10-10) | Verdict |
|---|---|---|---|
| AIRA-203 install leaves the old daemon running when only the binary changed | Yes, by code | `internal/install/install.go:1220` restarts only when the unit text changed; the `else if daemonPresent` arm (1224-1237) restarts only for a changed CPU-slots ratio (AIRA-283). Nothing compares the running daemon's binary with the one the unit names. | Build |
| AIRA-242 `--dump` times out on the 250 ms admit deadline | No: fixed | `37e823d` (in v0.8 and every later tag) gave dump and budget their own 30 s `dumpHistoryTimeout` (`internal/daemon/admit.go:41-63`), pinned by `confine_history_deadline_test.go`. | Drop (close as done); its pin is porous, see AIRA-280 |
| AIRA-280 `--dump` returns `E_DAEMON_UNAVAILABLE: EOF` | Yes, measured | `aira confine --dump ~/tmp/x.jsonl` -> rc 4, `E_DAEMON_UNAVAILABLE: EOF`, no file. **Different root cause from 242**, see 3.2. `aira confine --budget --json` now fails the same way (rc 4, EOF); the ticket's "budget works" was true on 10-08 and is not now. | Build (dump and budget) |
| AIRA-211 `<verb> --help` refused; `-h` swallowed; no did-you-mean | Yes, measured | `aira rant --help`, `aira list --help` -> `E_SELECTOR_INVALID: option --help requires a value`; `aira run-log --help`, `aira time --help` -> `E_RUN_ARGUMENT_INVALID`; `aira help list` prints all 52 verbs. `aira confine --help` already works (AIRA-243). | Build |
| AIRA-212 `skill install --force` with no dir creates `./--force/` | Yes, by code | `cmd/aira/skill.go:25-30`: `["install","--force"]` has length 2, so `--force` becomes the directory. Not probed (it writes). | Build |
| AIRA-215 `E_TRANSITION_INVALID` does not list legal next statuses | Yes, by code | `internal/domain/ticket.go:186-187`; the spec table (`2026-08-08-aira-phase1-design.md:385`) still lacks `done -> in-progress`. | Build |
| AIRA-207 confine argument refusal goes only to stdout when piped | Yes, measured (partly moot) | `aira confine --memory-reserve 4Q -- true`, `--timeout 300`, `--bogus` with stdout piped: JSON on stdout, stderr 0 bytes, rc 2. Sub-item (2) is moot: `--admit-timeout` was removed in S13 (`d2a32db`). Sub-item (4) is already fixed: aitest now parses sizes with units and warns on a bad value (`internal/pylib/aitest/__init__.py:502-560`). A third shape exists: `--vram 4Q` is refused later, in `runConfineCommand`, as plain stderr with no `ran=no` line. | Build (items 1 and 3 only) |
| AIRA-214 `confine --list` prints a table to a pipe | Yes, measured | `aira confine --list \| head` and `aira confine-list \| head` print `NAME  OWNER ...`; `aira confine --status` piped prints a text line. | Build |

## 2. Greenfield minimum, and rejected alternatives

If each problem had to be solved alone, from nothing:

- **203:** before finishing, ask "would a restart run different bytes from the ones running
  now?" and restart if yes. That is one comparison: the executable of the process
  `systemctl restart` would replace (the service's `MainPID`, `/proc/<pid>/exe`) against
  the file the unit's `ExecStart=` names.
- **280:** a reply can be larger than one 16 MiB frame. Minimum: (a) never close the
  connection silently: say "reply too large" with a stable code; (b) let the two verbs whose
  reply grows without bound (they return the whole machine-wide history) return it in
  pages, which the client joins, reading the history as a stream so the daemon never holds
  more than one page.
- **211:** one check before any verb parser: `aira <verb> --help` or `-h` prints that verb's
  entry from the existing help table.
- **212:** treat `--force` as a flag wherever it appears; refuse a directory that starts
  with `-`.
- **215:** build the message from the transition table that is already in scope (enumerate
  and sort its keys; no new function).
- **207:** write the refusal to stderr as well, as the same `ran=no` line an in-flight
  refusal prints.
- **214:** pass the "stdout is not a terminal" decision the rest of the CLI already uses
  into the confine management renderers.

The plan below is this minimum plus two consolidations that delete code rather than add it
(one help renderer instead of a confine-only one; one never-ran refusal helper for the
confine launch form).

| Item | Alternative | Why rejected |
|---|---|---|
| 203 | Compare build revisions (`aira version`, AIRA-202) | A worktree build has no VCS stamp (`buildid` reports `unevaluated`), and two builds of one revision from different dirty trees share a revision. Content comparison has neither gap. |
| 203 | Detect `/proc/<pid>/exe` ending in `(deleted)` | An indirect signal: it fires whenever the old inode was unlinked, including when `install.sh` re-copies identical bytes (a new inode every run), so it would restart on every convergence run; and it cannot see an in-place rewrite. Content is the direct signal. |
| 203 | Compare inode numbers only | `install.sh` always copies a new inode (`install bin/aira ...`), so a rebuild of identical bytes would restart the daemon on every run, which the existing code comment forbids. Same-inode is kept as a fast path only. |
| 203 | Use the daemon lock's PID (`daemon.Status`) as the process to inspect | `Status` reports `Running` only with a positive PID and a matching boot id, so a held lock with missing or corrupt metadata reads "not running" and would skip the check (plan review). It also names the lock holder, which need not be the process `systemctl restart` replaces. The service's `MainPID` is exactly that process. |
| 203 | Always `try-restart` in `install.sh` | Bounces the machine daemon on every convergence run, and does nothing for `aira install` run on its own. |
| 203 | Wrap the restart in `aira drain` | A drain waits for every running job to finish; far too heavy for a binary swap. A restart is already job-safe: admitted scopes survive and clients re-declare their leases (ARDR, v0.6). The unit-change restart path has done this since AIRA-62. |
| 280 | Raise `MaxFrameBytes` | Moves the cliff: the history has no total cap (130,729 rows on 2026-10-10, first row 2026-08-26). The daemon also builds the whole reply in memory before sending; its unit caps it at `MemoryHigh=768M`/`MemoryMax=1G`. |
| 280 | Split one large reply across several frames (chunked transport) | Fixes every verb at once but keeps the daemon's per-request memory unbounded, and changes the framing every verb uses. Paging bounds daemon memory per request. |
| 280 | A fixed subject count per page (v1: 1,000 subjects read in one CTE query, then cut at 4 MiB) | Plan review: 1,000 subjects of up to 20 rows each with long signatures can exceed any memory budget before the byte cut runs, and a "returned `limit` subjects" rule cannot tell whether more remain (an extra empty page). Streaming rows and stopping at a subject boundary bounds memory by bytes directly and knows exactly whether another subject exists. |
| 280 | Daemon writes the dump file itself | The daemon would write into the caller's filesystem path; design §12 deliberately has the CLI write it (and in ci-shim the paths differ). |
| 280 | Cap or age out the history table | Changes what the admission estimator learns from (AIRA-213 option D). An owner decision; recorded under Deferrals. |
| 280 | Show only the top N budget subjects | Which subjects matter is a judgement; AIRA reports, it does not choose. |
| 280 | Always page (no opt-in) | An older client that does not know about pages would write the first page as if it were the whole dump: a silent partial result. Opt-in paging makes both version-skew directions fail loudly instead, so no protocol bump is needed. |
| 211 | Patch `--help` into each of the twelve parsers | Twelve copies of one rule. One pre-parse check covers them all. |
| 211 | Treat `--help`/`-h` anywhere before `--` as help | `-h` is a legal option value (`aira gate add ... --argv df --argv -h`, `--body -h`); reading it as help would turn a plausible registration into an exit-0 no-op. The generic parser can tell a value from a name, so it gets a precise rule (3.3). |
| 212 | Give `skill install` a default directory | A separate design question (ticket says so). |
| 214 | Add a `--human` flag | Not asked for; a terminal keeps the table. |

## 3. Design

### 3.1 AIRA-203: restart the daemon when its running binary is not the installed one

`internal/install/install.go`, `Install` (real mode), after `enable --now`:

- New dependency seam on `installDeps`: `sameFileContent func(a, b string) (bool, error)`.
  Default implementation (new, in `internal/install`): open both paths; if `os.SameFile`
  on the two `Stat` results, return true; if sizes differ, false; otherwise compare the
  bytes in 64 KiB chunks. Any open/read error is returned as an error (never "same").
- New helper `liveDaemonBinaryStale(d installDeps, executable string) (stale bool, reason
  string)`. The process inspected is the service's main process, the one `systemctl
  restart` replaces, read through the existing `d.run` seam exactly as
  `verifyDaemonReachable` (install.go:2146) already reads it:
  `systemctl --user show -p MainPID --value <unit>`. Not the daemon lock: `daemon.Status`
  (paths.go:524) sets `Running` only for a positive lock PID with a matching boot id, so a
  held lock with missing or corrupt metadata would read as "not running" and skip the check
  (plan review, verified).
  - the `show` command fails, or its value is not an integer -> stale, reason "the running
    daemon's binary is unevaluated: MainPID is unreadable: <detail>";
  - `MainPID <= 0` -> stale, reason "the running daemon's binary is unevaluated: the
    service reports no main process". (`enable --now` has just run, so a healthy service
    has one. A restart of a unit with no process is a start: harmless, and logged.)
  - `d.sameFileContent("/proc/<MainPID>/exe", executable)`: error -> stale, reason
    "the running daemon's binary is unevaluated: <err>"; false -> stale, reason "the running
    daemon's binary differs from <executable>"; true -> not stale.
  There is no "daemon not running" short cut: a service that `enable --now` just started
  has a `MainPID` whose executable is the current file, so the content check itself returns
  "not stale".
- The `else if daemonPresent { ... }` block (install.go:1224-1237) becomes one block that
  collects reasons from `liveDaemonBinaryStale` and from the existing
  `liveCPUSlotsPerCoreStale`, and issues **one** `systemctl --user restart` when any reason
  exists, logging `aira-daemon.service: restarted: <reasons joined by "; ">`. The
  `daemonPresent && daemonChanged` arm is unchanged (it already restarts; the binary check
  is not consulted there, so there is never a second restart).
- Unevaluated means restart. Reason: the cost of a needless restart is a short admission
  blip with jobs kept (the restart path that already exists); the cost of a missed one is
  every client failing `E_DAEMON_PROTOCOL` until someone restarts by hand, which is what
  happened on 2026-10-10. The log line names the reason, so it is never a silent pass.
- The binary is the one that has just been copied into place and is running `install`
  (`install.sh` copies `bin/aira` to `~/.local/bin/aira`, then runs
  `~/.local/bin/aira install`), so the release that ships this fix already restarts the old
  daemon on its own first install. No manual restart is needed for that upgrade.
- Dry run (install.go:1003-1022) gains one line: `planned: restart a present daemon whose
  running binary differs from <executable>`.
- The `executable` compared is the same absolute path rendered into `ExecStart=`
  (install.go:974-998), so the question asked is exactly "would a restart run different
  bytes".

Measured: overwriting a running executable in place is refused on this kernel
(`cp` over a running binary -> `Text file busy`, 6.18 WSL2), so the bytes at the path can
change only through a new inode; the content comparison does not depend on that.

### 3.2 AIRA-280 (and the AIRA-242 close): oversized replies and paged history

**Root cause, established.** `confine-dump` and `confine-budget` return the whole
machine-wide `confine_peak_history` in one reply. On 2026-10-10 that table holds 130,729
rows over 85,954 distinct (kind, signature) subjects (80,884 `confine`, 5,070
`pytest-worker`); 78,747 of those subjects have fewer than 3 samples. A SQLite `json_object` estimate
of the dump's rows sums to 60.4 MB (Go's `omitempty` encoding is smaller, but signatures
alone are 4.9 MB plus roughly 200 bytes of fixed fields per row, so well above 16 MiB:
estimate, not measured on the Go encoder). `writeFrame` (`internal/daemon/protocol.go:384`)
refuses a payload over `MaxFrameBytes` (16 MiB) **before writing any byte**; `reply`
(`deadlines.go:157`) returns false; `serveConnection` closes the connection; the client
reads EOF and reports `E_DAEMON_UNAVAILABLE: EOF`, exit 4. Nothing is logged. This is the
part AIRA-242's comment left "NOT established". AIRA-242's deadline fix is real and stays;
its regression tests call the handlers directly and never send the reply over a
connection, so they passed while the verb was dead.

**(a) Loud refusal for any oversized post-handler reply.** There are three post-handler
response writers, and all three refuse an oversized payload before writing any byte:
`reply` (`deadlines.go:157`, every verb's normal answer), `replyStoreOp` (`deadlines.go:164`,
whose `writeResponse` refuses a body over `StoreOpBodyMax` at `protocol.go:430`, before
`writeFrame`) and `watch`'s direct `writeFrame` (`server.go:937`). Change:

- `writeFrame` and `writeResponse` return a typed error, `*frameTooLargeError{What string;
  Size, Limit uint64}`, when they refuse for size (and only then; every other failure is
  unchanged).
- One shared helper, `(s *Server) writeOrRefuseTooLarge(conn net.Conn, write func() error)
  bool`, used by `reply`, `replyStoreOp` and the watch arm (which keeps its own
  `watchWriteTimeout` deadline: each caller stamps its deadline before calling the helper,
  so the helper never chooses a deadline). On a `frameTooLargeError` it writes
  `errorFrame(CodeResponseTooLarge, "E_DAEMON_RESPONSE_TOO_LARGE: the <what> is N bytes, over
  the L-byte limit; nothing was sent")` instead. Because the refusal happened before any
  byte, the fallback frame is the first and only frame on the connection, and the client's
  `readResponse` reads it as a normal error (an `errorFrame` has `BodyLen` 0, so the store-op
  reader accepts it too).
- **Return value:** the helper returns `true` whenever the original write or the fallback
  write succeeded, and `false` only when the bytes actually sent failed. `serveConnection`
  stores it in `wrote`, so its panic-recovery writer (`server.go:733`) can never write a
  second frame after the fallback.
- New stable code `E_DAEMON_RESPONSE_TOO_LARGE`, exit 4, catalogued in
  `internal/codes/codes.go`.
- Out of scope, stated: the handshake rejections (`server.go:760-790`: short frame, bad
  frame, protocol mismatch, foreign state id) and the panic-recovery frame also call
  `writeFrame` directly. Each writes a fixed, short error string under the handshake
  deadline, so none can exceed the limit; they keep the direct call (deadlines.go:151-156
  explains why they deliberately avoid `reply`).

**(b) Opt-in paging for `confine-dump` and `confine-budget`.**

- Store, `internal/store/confine_peak_history.go`: new streaming reader
  `StreamResourceBudgetSubjects(ctx, afterKind, afterSignature string,
  take func(ResourceBudgetSubjectRows) bool) (more bool, err error)`. One query, no `LIMIT`,
  consumed row by row:

      SELECT kind, signature, peak_rss, oom, budget, budget_basis, at
      FROM confine_peak_history WHERE (kind, signature) > (?, ?)
      ORDER BY kind, signature, at DESC, rowid DESC

  (served by the existing `confine_peak_history_kind_signature` index, store.go:1320). The
  loop groups consecutive rows into one subject (the row scan and the nullable-field
  handling are the existing `scanResourceBudgetRows` logic, refactored into a per-row
  function both readers share; `peak_rss`/`budget` that are NULL or 0 stay `nil`, never 0).
  When a row of the next subject arrives, the finished subject is passed to `take`: `true`
  keeps going; `false` stops at once, closes the rows, and returns `more = true` (the
  subject just offered exists and was not taken). End of rows returns the last subject to
  `take` and then `more = false`. So `more` is exact: it is `true` if and only if a subject
  after the last one taken existed at read time, and the daemon holds at most the subjects
  it took plus the one in flight (at most `confinePeakHistoryLimit` = 20 rows). An empty
  cursor starts from the beginning (every row has a non-empty signature, by the table's
  CHECK). Ordering is SQLite BINARY collation, which matches Go's bytewise string
  comparison used by the client's progress check. A subject is never split across pages.
  `ResourceBudgetSubjects` (the unpaged reader) stays for the unpaged path.
- Wire: a request opts in with `paged: true` plus `after_kind`, `after_signature` (both
  empty on the first page, both set after). Exactly one set, or an unknown `after_kind`, is
  refused `E_CONFINE_ARGUMENT_INVALID`. Without `paged` the handlers behave exactly as
  today (single reply, now failing loudly via (a) when too large). Results gain
  `Next *runner.ConfineHistoryCursor` (`{kind, signature}`, `json:"next,omitempty"`) on
  `runner.ConfineDumpResult` and `runner.ConfineBudgetResult`; present exactly when at
  least one more subject existed when the page was read.
- Daemon, `internal/daemon/ci_dump.go` and `confine_budget.go`: when paged, call
  `StreamResourceBudgetSubjects` under `dumpHistoryTimeout` with a `take` that converts the
  offered subject to the verb's own wire rows (dump: admission rows; budget: one
  `ClassifyResourceBudget` verdict row), measures their `json.Marshal` length, and accepts
  the subject only if the running total stays within `confineHistoryPageBytes` (4 MiB) or
  nothing has been taken yet. The first page's budget starts already charged with the
  marshalled size of the dump's `Waiters` and `Queues` (live state, sent only on the first
  page), so the 4 MiB covers everything variable in the reply; the envelope's fixed fields
  are a few hundred bytes, far inside the 12 MiB of headroom to the 16 MiB frame. There is
  no subject-count cap: the byte budget is the only bound, and streaming makes it the
  memory bound too. A page always holds at least one subject (a single subject larger than
  a frame then fails loudly via (a): a documented limit, since a signature is bounded only
  by the 16 MiB request frame). `Next` = the last subject taken, set exactly when the store
  reports `more`. Budget: each page sorted as today.
- Client, `cmd/aira/dispatcher.go`, `daemonDispatcher.dispatchConfineManagement`: for
  `confine-dump` and `confine-budget`, send paged requests and join the pages into one
  result before returning, so the CLI renderers, the dump file writer and the MCP face
  (which uses the same dispatcher) are unchanged. Rules: any non-OK or `unevaluated` page
  is returned as the whole answer (no partial result; the daemon-down fallbacks keep
  working on page 1); a `Next` that is not strictly greater than the previous cursor is
  `E_DAEMON_PROTOCOL` ("confine history page did not advance"); a page without `Next` ends
  the loop, including an empty one (possible only if history rows were deleted between two
  page reads), which is an ordinary end, not an error. Rows are joined as decoded, so a
  `nil` measurement or an `unevaluated` verdict row stays exactly that in the joined result
  (never 0, never a pass). Budget rows from all pages are re-sorted with one shared comparator
  (`store.SortConfineBudgetRows`, sharing its direction-rank function with
  `SortResourceBudgetVerdicts`, so the order equals today's unpaged order).
- Version skew, no protocol bump: an old daemon ignores `paged` and returns everything
  with no `Next` (one exchange; still EOF if too large, as today). An old client never sends
  `paged` and gets the single reply, which now fails loudly instead of silently.
- Not a point-in-time snapshot: pages are separate reads. A subject first recorded during
  the dump, sorting before the cursor, is absent; each subject's samples are read whole at
  its own page's instant. Documented on the result types.
- `admit.go`'s `dumpHistoryTimeout` comment is updated: the EOF is now explained (AIRA-280).

### 3.3 AIRA-211: one `--help` for every verb

`cmd/aira/main.go`, `runWithInputDispatcher`:

- **One pre-parse check, first thing** (before the `install`, `daemon`, `mcp`, `skill`
  intercepts at main.go:65-92): strip `--scope-dir`/`--json` with the existing
  `removeScopeDir`/`removeJSON` to find the verb. The pre-parse ignores an error from
  `removeScopeDir` (a malformed `--scope-dir`, scope_dir.go:88) and simply falls through,
  so the existing refusal at main.go:110-113 still fires; it never prints help for a
  request that also carries a malformed option. If the token right after the verb is
  exactly `--help` or `-h`, and the verb has entries in the help table, print them and exit
  0. Entries per verb (`helpEntriesFor`): `confine` -> the existing `confineHelpVerbs`
  family; `worktree` -> `worktree-register` and `worktree-audit`; aliases through
  `core.CanonicalVerb` (`new`, `ls`, `get`); otherwise the verb's own entry. A verb with no
  entry (skill, top, board, tui, mcp, daemon, version, watch, worker-admit, confine-report,
  drain-hold) falls through to its current behaviour. Only the token after the verb is
  read, so `aira confine -- printf %s --help` is untouched.
- **Generic parser** (`parseArgs`' own loop, main.go:784-893): `--help` met where an option
  NAME is expected, or `-h` met where a POSITIONAL is expected, returns a help sentinel
  that the caller renders as above. A value position is never inspected, so
  `create T --body -h` and `gate ... --argv -h` keep `-h` as a value. This closes
  `create Fix the bug -h` (today a ticket titled "Fix the bug -h") and
  `list --by status --help`.
- One renderer, `renderVerbHelp(entries, renderJSON, stdout, stderr)`, used by the
  pre-parse, the sentinel, `aira help <verb>`, and `runConfineHelpCommand` (which becomes a
  call to it). Piped output is the JSON envelope, like `aira help`.
- `aira help <verb>` prints only that verb's entries; an unknown verb is refused
  `E_UNKNOWN_VERB: no verb named "<x>"`; more than one argument is refused
  `E_SELECTOR_INVALID`. `aira -h` joins `aira --help` (main.go:157).
- Did-you-mean: the generic "not valid" refusal appends
  `optionDidYouMean(name, sortedKeys(allowed[verb]))`, the helper confine already uses
  (option_suggest.go:76). It never names an option the table refuses, and names nothing
  when nothing is close.
- Stable codes at the source: `E_SELECTOR_INVALID: option --%s requires a value` and
  `E_SELECTOR_INVALID: option --%s is not valid for %s` (same code the remap at main.go:196
  produces today; now explicit). `run-*` verbs keep `E_RUN_ARGUMENT_INVALID`.
- Delete the dead `verb == "git"` arms at main.go:802-804 and 887-889 (git is delegated at
  main.go:781 and never reaches them).
- Not done: `--tags`/`--refs` aliases (the ticket warns an alias without folding silently
  drops values; the suggestion covers the case).

### 3.4 AIRA-212: `skill install` flag handling

`cmd/aira/skill.go`, `runSkill`: for `install`, strip every `--force` from the remaining
arguments (setting `force`), require exactly one remaining argument, and refuse one that
begins with `-` with `usage: aira skill install <dir> [--force] (<dir> is required; a
directory really named like a flag can be written ./-name)`, exit 2. No default target,
no routing through `parseArgs`. Doc fixes: `docs/superpowers/specs/2026-08-29-aira-memory-
accounting-rework-design.md:271` -> `aira skill install ~/.claude/skills/aira --force`;
`.aira/tickets/AIRA-92.md:147` gets a bracketed note that the recorded spelling installed
into `./--force/` (AIRA-212) rather than being rewritten. (No stray `--force` directory
exists in `~`, `~/claude` or `~/claude/aira`, checked.)

### 3.5 AIRA-215: name the legal next statuses

`internal/domain/ticket.go`, `ValidateTransition`: the local `allowed` table stays as it
is. On a refusal, `successors, known := allowed[from]`; the message lists the keys of
`successors`, sorted. No new function. Refusals:

- known, non-terminal: `E_TRANSITION_INVALID: planned -> done is not a legal transition;
  from planned the legal next statuses are: in-progress, retired, superseded` (sorted
  alphabetically, so stable across runs);
- terminal: `E_TRANSITION_INVALID: retired -> done is not a legal transition; retired is
  terminal and has no legal next status`;
- unknown `from`: `E_TRANSITION_INVALID: <from> -> <to>: <from> is not a known status`
  (never described as terminal).

Code and exit unchanged; the `planned -> done` edge stays refused (decided in the ticket).
Docs: the phase-1 spec's `done` row becomes `done -> in-progress | retired | superseded`;
`docs/dev/agentic-development-loop.md` gains one paragraph: the close path is `aira mv <id>
in-progress`, `in-review`, `done`, in that order, and a ticket file's `status` is never
hand-edited; plus one sentence that the pre-push hook runs the full `make ci` on every
push, docs-only included, because tests read repository content files.

### 3.6 AIRA-207: confine refusals always reach stderr

`cmd/aira/main.go`:

- New helper `refuseConfineLaunch(stderr io.Writer, err error) int`: prints
  `runner.FormatConfineNeverRan(runner.ConfineStatus{}, err)` then the error text (the same
  two lines, in the same order, that an in-flight refusal prints today, e.g. `--name
  bad/name`), and returns the code's exit.
- New predicate `isConfineLaunch(args []string) bool`: `args[0]` is `confine` (the
  hyphenated `confine-*` verbs are management, never launches) and no management flag
  (`--list`, `--kill`, `--status`, `--budget`, `--dump`, with or without `=value`) appears
  before the first `--`. It reads tokens only, so it works before (and when) option parsing
  fails. Management requests never get a `ran=no` line: nothing was being launched.
- Refusals in `runWithInputDispatcher` that return before `runConfineCommand` is reached
  write the extra stderr lines when `isConfineLaunch(args)`:
  - the global scope refusals: the malformed `--scope-dir` (main.go:110-113) and the
    unresolvable scope directory (main.go:128-131) (`confine` accepts `--scope-dir`,
    scope_dir.go:154, so the "not valid for" refusal at 117-126 cannot fire for it);
  - the parse-error path (main.go:179-200);
  - the explicit `--json is not valid for confine` refusal (main.go:247-250), which today
    renders JSON to stdout only.
  In each, stdout is unchanged (the JSON envelope when `renderJSON`); stderr gets the
  `ran=no` line, and also the error text when `render` would not already have written it
  there (i.e. when rendering JSON).
- Every refusal in `runConfineCommand` that returns before the launch (the
  `parseScopeMemoryOptions`, `--memory-reserve`, bound, `--owner` and `--vram` arms, about
  six sites from main.go:1456) goes through the helper, so a script has one fixed string,
  `ran=no`, for "this confine launch never ran", whatever layer refused it.
- Help text (`internal/core/core.go`, confine ArgSpecs): `--timeout` and `--cpu-timeout`
  say "a Go duration with a unit, e.g. 90s, 10m, 1h30m; a bare number is refused";
  `--memory-reserve` says "a bare number is bytes" (confirmed in
  `internal/runner/memory_size.go:46`).

### 3.7 AIRA-214: confine management output is JSON on a pipe

`cmd/aira/main.go`: pass `renderJSON` instead of `jsonOutput` to
`runConfineStatusCommand`, `runConfineDumpCommand` (success summary),
`runConfineManagementCommand` (list, kill, budget) and the hyphenated
`confine-list`/`-kill`/`-budget`/`-dump` arms (main.go:251-343). The launch form's
"`--json` is not valid" check keeps the explicit flag. `confine-log`/`confine-input` are
untouched (their byte-transparent contract). Correct the comment at main.go:98-108 to name
only the forms that really reject `--json`. Skill text (`internal/core/skill.go:320`): "read
the printed `exit=` (or, with `--json` or from a pipe, the `state`, `exit` and
`error_code` fields) rather than `$?`". Update the committed measurement script
`docs/dev/aira68-ledger-sample.sh`, which parses the table from `$(aira confine --list)`, to
read the JSON `slice_reserve` fields instead.

One peer script **executes** a piped management form (plan review, verified):
`fastest-ee/deploy/ci/internal/jobspec/runner/in-container-gate.sh:234` runs
`aira confine --list 2>&1 || true` every 5 s during a gate into a snapshot log (stdout is a
file, so that log becomes JSON envelopes instead of tables). No data is lost: line 237
already writes `aira confine --list --json` to `aira-ledger-snapshots.tsv`, and line 344's
`aira confine --budget --json` is unchanged in format. The deploy notice names this script
and line. The other fastest-ee hits (`gate_mem_probe.sh:105`, `measure_corpus_scan_rss.py:54`,
`mg_confine.sh:179`, `merge_gate.sh:846`) are comments or `echo` text; stoner has none.

## 4. Invariants

1. A unit-unchanged, binary-unchanged install issues no restart. A binary that differs, or
   cannot be compared, gets exactly one restart, with the reason logged. Never two restarts
   in one install.
2. No post-handler daemon reply (`reply`, `replyStoreOp`, the `watch` answer) is ever
   dropped silently: one over its size limit is answered with
   `E_DAEMON_RESPONSE_TOO_LARGE`, as the only frame on the connection, and never followed
   by a second frame. (Handshake rejections and the panic frame are fixed short strings,
   outside this invariant by construction.)
3. A paged dump or budget is either complete or not produced: no partial file, no partial
   result, in either version-skew direction.
4. Joined pages equal the unpaged result (same rows, same order) when the history does not
   change during the read.
5. Daemon memory per paged request is bounded by one page: the rows taken (at most 4 MiB
   marshalled, including first-page live state, or one subject) plus the one subject in
   flight (at most 20 rows). It never depends on how many subjects the history holds.
6. `--help`/`-h` never reaches a verb handler and never changes an option value or argv
   after `--`.
7. A suggestion never names an option the parser would refuse.
8. Every refusal of a `confine` launch request (as `isConfineLaunch` defines it), from any
   layer (global scope checks, the parser, the `--json` check, `runConfineCommand`), writes
   a `ran=no` line to stderr, whatever stdout is. A management request never gets one.
9. A transition refusal never claims a non-terminal or unknown status is terminal.
10. `skill install` never creates a directory whose name begins with `-`.
11. A paged reply carries `Next` if and only if another subject existed after its last one
    when it was read, so the client never needs a trailing empty page; an empty page
    without `Next` still ends the join cleanly.
12. The daemon binary check inspects the process `systemctl restart` would replace (the
    service `MainPID`), never a PID that could be unset while a daemon runs.

## 5. Tests (TDD; each must fail against the wrong implementation)

Write each test first and record it RED on `fab2647` (or RED against the named mutation
where the bug cannot fail today). Mutations below must each turn at least one test RED.

**AIRA-203** (`internal/install`, existing `run` fake answering `systemctl --user show -p
MainPID --value` as in `install_test.go:438`, and the `daemonStatus` fake, as in
`cpu_slots_per_core_test.go`):
- unit unchanged, MainPID 4242, contents differ -> exactly one `systemctl --user restart
  aira-daemon.service`, log names the binary (RED today);
- unit unchanged, contents equal -> no restart (false-pass twin);
- compare error -> one restart, log says unevaluated; `show MainPID` fails -> same;
  MainPID `abc` -> same; MainPID `0` -> same;
- MainPID differs from the lock PID (lock fake returns `Running: false, PID: 0`, the
  corrupt-metadata shape `Status` can really produce) -> the check still runs on the
  MainPID and restarts when contents differ. Mutation: read the PID from `daemonStatus`
  -> RED;
- binary and ratio both stale -> exactly one restart naming both reasons;
- unit changed and binary differs -> exactly one restart;
- the fake asserts it was called with `/proc/<MainPID>/exe` and the absolute
  `ExecStart` path;
- real `sameFileContent`: same file, hard link, identical copy -> true; same size different
  bytes, different size -> false; missing file -> error;
- dry run prints the planned restart line.
- Mutations: restart unconditionally; treat a compare error as equal; compare the
  executable with itself; issue a restart per reason; skip the check when the ratio check
  already restarted.

**AIRA-280** (`internal/daemon`, `internal/store`, `cmd/aira`):
- oversized replies, one connection-level test per writer, each through `serveConnection`
  on a real `net.Pipe`/socket pair, with the client reading via the production reader:
  (i) a `Handle` reply over `MaxFrameBytes` -> the client reads
  `E_DAEMON_RESPONSE_TOO_LARGE` (RED today: EOF); (ii) a store-op whose response body
  exceeds `StoreOpBodyMax` (`replyStoreOp`) -> same, read by `readResponse` (RED today);
  (iii) a `watch` answer over the limit -> same (RED today). Each also asserts that exactly
  one frame was sent (the connection reaches EOF after it). Mutations: drop the fallback in
  the shared helper (all three RED); route only `reply` through it (ii and iii RED). The
  helper's return value is pinned by a direct unit test: `true` after a successful fallback
  write, `false` when the fallback write itself fails on a closed connection (mutation:
  return the original write's result -> RED).
- store streaming reader: cursor is exclusive; whole subjects; `take` returning false stops
  with `more = true`; the end of rows gives `more = false`, including when the last subject
  is exactly the last one taken (mutation: report `more` from "took N subjects" -> RED);
  empty cursor starts at the first subject; mixed-case and non-ASCII signatures order the
  same in SQL and in Go; a NULL `peak_rss` and a NULL `budget` come back `nil` (mutation:
  scan into plain int64 -> RED). Mutation: `>=` instead of `>` (duplicate subject). A
  `take` that refuses the second subject on a 1,000-subject fixture is offered exactly two
  subjects (mutation: keep offering after `false` -> RED). **Coverage gap, accepted:** no
  test can tell the streaming reader from one that buffers every row and then offers them
  one by one, since both produce the same calls; invariant 5's memory bound is therefore a
  code-reading item for the build review (one `rows.Next` loop, no slice of all rows, no
  `LIMIT`-less buffering helper).
- handlers, paged: every page's rows plus first-page live state marshal to at most 4 MiB
  unless the page holds one subject; `Next` present exactly when more remain; a history
  whose subject count makes the last page exactly full ends with a `Next`-less page, not an
  extra empty one; waiters and queues only on the first page; an unpaged request is
  unchanged. Mutations: drop the byte cut (a long-signature fixture then overflows the
  frame); omit `Next` on a cut page (rows go missing); send waiters on every page
  (duplicates); leave live state out of the first page's budget (a fixture with a large
  waiter set then exceeds 4 MiB -> RED).
- equivalence: joined pages equal the unpaged result for a fixture whose under-provisioned
  subjects fall on different pages. Mutation: no client re-sort.
- end to end (in-process daemon over its socket, the existing `seedLargeConfinePeakHistory`
  volume or a smaller long-signature seed that exceeds 16 MiB): `confine --dump` writes
  every row and `confine --budget --json` returns every subject (both RED today: EOF).
  Replaces the porous handler-only assertion as AIRA-242's real pin; the deadline tests stay.
- client loop: a `Next` that does not advance -> `E_DAEMON_PROTOCOL`, no file, bounded by a
  test timeout (mutation: no progress check hangs); an error on page 2 -> that error, no
  file; `unevaluated` on page 1 -> unchanged unevaluated output; a reply without `Next` to
  a paged request (old daemon) -> one exchange, complete; an empty page without `Next`
  after a full page -> the join ends, no `E_DAEMON_PROTOCOL`, and the dump file is written
  with the earlier pages' rows (mutation: treat an empty page as a protocol error -> RED).
- honesty through the join: a budget fixture with an `unevaluated` subject on page 1 and a
  subject with no usable peak and no recorded budget (nil `ObservedMax`, nil `Budget`) on
  page 2, and one with established values on page 2, round-trip through the joined result
  with `unevaluated` kept, `nil` kept as absent (not `0`), and the established values
  unchanged; the same for the dump's nullable `peak_rss`/`budget` columns in the written
  file (mutation: join into a struct with non-pointer fields -> RED).

**AIRA-211** (`cmd/aira`):
- table: `<verb> --help` and `<verb> -h` for rant, run-log, run, time, git, install,
  confine (no `--`), drain, list, create, worktree -> exit 0, output contains that verb's
  usage and not an unrelated verb's summary; `create`/`create -h` use an injected
  dispatcher that fails the test if called.
- `parseArgs("confine", ["--","printf","%s","--help"])` keeps `--help` in the child argv;
  the pre-parse helper returns false for it. Mutation: scan every token -> RED.
- generic sentinel: `list --by status --help` -> help; `create Fix it -h` -> help, no
  dispatch; `create T --body -h` reaches the handler with body `-h`; gate `--argv -h` is
  kept. Mutation: treat value positions as help -> RED.
- `aira help list` -> exactly one entry; `aira help nosuch` -> `E_UNKNOWN_VERB`, exit 2;
  `aira help list ready` -> refused.
- `rant --tags x` -> `E_SELECTOR_INVALID: option --tags is not valid for rant (did you mean
  --tag?)`; `rant --zzz x` -> no "did you mean". A multi-match case asserts the exact,
  sorted text (mutation: map-order vocabulary flakes).

**AIRA-212** (`cmd/aira/skill_test.go`, each in its own `t.TempDir()` as cwd, with a
filesystem assertion): `install --force` -> exit 2, no `version:` on stdout,
`os.Stat("--force")` is NotExist; `install --` -> exit 2, no `--` directory;
`install --force <dir>` and `install <dir> --force` -> exit 0, both files present; with a
differing `SKILL.md` already in `<dir>`, both orders overwrite it (first test that ever
runs `force=true`); `install <dir>` without `--force` over a differing file -> refused.
Mutations: drop the dash guard; honour `--force` only in third position.

**AIRA-215** (`internal/domain`): full-string asserts for `(planned, done)`,
`(draft, done)`, `(done, planned)`; `(retired, done)` says terminal and contains no
"legal next statuses:" list; unknown `from` is not called terminal; each assert run twice
in one test (map-order mutation flakes).

**AIRA-207** (`cmd/aira/confine_test.go`, stdout a `bytes.Buffer`): `--memory-reserve 4Q`,
`--timeout 300`, `--bogus` -> exit 2, stdout still the JSON envelope, stderr contains
`runner.ConfineNeverRanFacet` and `E_CONFINE_ARGUMENT_INVALID` (RED today: stderr empty);
`--vram 4Q` -> stderr contains `ran=no` (RED today); `confine --json -- true` -> stdout
the JSON envelope, stderr contains `ran=no` (RED today); `confine --scope-dir= -- true`
(malformed) and `confine --scope-dir <missing dir> -- true` -> stderr contains `ran=no`
(RED today); management twins `confine --list --bogus`, `confine --budget --scope-dir=` and
`confine-list --bogus` -> refused as today, stderr contains no `ran=no`;
`confine -- printf --list` refused for another reason is still a launch (a `--list` after
`--` is the child's). Mutations: write stderr only when not rendering JSON; key the line on
`verb == "confine"` instead of `isConfineLaunch` (management twins RED); scan past `--` in
`isConfineLaunch` (the last case RED).

**AIRA-214** (`cmd/aira`, stub dispatcher, stdout a `bytes.Buffer`, no `--json`):
`confine --list`, `confine-list`, `confine --budget`, `confine --kill x` -> stdout parses
as the envelope with `code` `OK` (RED today: first byte `N`); `confine --status` -> JSON;
`--dump` success summary -> JSON. Companion: `renderConfineListResponse` and the budget
renderer still produce the table when called directly. Mutation: thread `renderJSON` into
list only.

Gate: `aira confine -- make ci` (the gate's exact command), plus `go test -race` on
`internal/daemon` and `cmd/aira`.

## 6. Expected yield

- Every future binary install takes effect without a manual daemon restart (removes the
  2026-10-10 `E_DAEMON_PROTOCOL` failure and the release-notes "restart the daemon" trap).
- `confine --dump` and `confine --budget` work again on the box (both rc 4 today) and stay
  working as history grows; any other oversized reply now names itself.
- Agents can discover any verb's usage with `--help`; mistyped options get a suggestion;
  `-h` can no longer become part of a ticket title.
- Scripts reading stderr see every confine refusal; piped management output parses as
  JSON; the status graph is readable from the error.

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| AIRA-213 (estimator subject: argv vs argv+directory) | Owner decision, per the ticket. |
| Bounding `confine_peak_history` in aggregate (130,729 rows, 85,954 subjects of which 78,747 have fewer than 3 samples, since 2026-08-26; nothing caps the total) | Changes what the estimator learns from; belongs with AIRA-213 option D. Paging removes the outage without deciding it. Suggest a held ticket. |
| `aira confine-status` is listed by `aira confine --help` but the hyphenated spelling is refused `E_UNKNOWN_VERB` (measured) | New observation, not a batch ticket; suggest a ticket. |
| ci-shim `--stage=start` binary-staleness line | The shim daemon is per-container and starts fresh; no observed staleness. |
| `aira install --status` "daemon binary: current / differs / unevaluated" line | Not needed to fix the bug; small follow-up if wanted. |
| `--help` for verbs outside the help table (skill, top, board, tui, mcp, daemon, version, watch, worker-admit, confine-report, drain-hold) | They have no table entry to print; `aira mcp --help` still starts the MCP server (pre-existing). |
| `--help` after other options inside the twelve sub-parsers (confine already handles it) | The common spelling (`aira <verb> --help`) is covered once; per-parser handling would recreate the twelve copies. |
| MCP page size for `aira_confine_budget` | The MCP face gets the joined result through the dispatcher, then its existing output cap; a per-call page argument is not needed for the outage. |
| AIRA-207 sub-items (2) and (4) | Moot (`--admit-timeout` removed in S13) and already fixed (aitest size parsing), respectively. |
| AIRA-227 (skew error wording) | Adjacent, not in the requested batch. |

## 8. Risks

- **Install restarts (203).** An install now restarts the daemon whenever the binary
  changed. That is the same restart an operator must do by hand today, through the path the
  unit-change case already uses (admitted jobs keep running; clients re-declare leases;
  mid-admission requests see a short blip, per the 2026-09-13 restart-care note). Run box
  installs at a quiet moment, as now. An unreadable comparison also restarts, logged.
- **Wire shape (280).** New optional request fields and a new result field on two verbs;
  protocol version stays 14 because paging is opt-in (skew matrix in 3.2). The daemon half
  needs the daemon restart, which 203 now performs. Reviewers should check the cursor
  arithmetic, the byte cut, and that no path returns a partial result.
- **Output format change (214).** Piped `confine --list/--budget/--status/--kill` becomes
  JSON. One committed script is updated. Peers' scripts were grepped: fastest-ee's
  `in-container-gate.sh:234` executes `aira confine --list` into a snapshot log, which
  becomes JSON (no data lost: line 237 already records `--json`); every other fastest-ee hit
  is a comment or `echo` text; stoner has none. Release notes and the deploy notice name
  that script.
- **Paging cost (280).** Each subject's verdict or rows are marshalled once to measure and
  again in the frame (at most 4 MiB per page, so small). fastest-ee's in-container gate runs
  `aira confine --budget --json` under a 60 s timeout against its per-container daemon,
  whose history is short; paging adds one exchange per 4 MiB.
- **Help interception (211).** A literal `-h` positional in the generic parser is now help,
  not data (`aira grep -h` cannot search for "-h"); accepted, like any CLI.
- **Message text (215, 211).** Codes and exits unchanged; tests that pin exact message
  text may need updating (none found for 215).
- **Skill text changes** (207 help text, 214 status clause) need a separate `aira skill
  install ~/.claude/skills/aira --force` at release.

## 9. Release notes (for the build)

Proto unchanged (14). Daemon restart required for 280's daemon half; `aira install`
performs it, including on this release's own first install, because `install.sh` copies the
new binary into place and then runs that binary's `install` (verify on the box: the install
log shows `aira-daemon.service: restarted: the running daemon's binary differs ...`, and
`aira version` reports the same build for client and daemon). Only a binary copied in
without running `aira install` still needs a manual
`systemctl --user restart aira-daemon.service`. Skill reinstall required. Close
AIRA-242 as done (deadline fix `37e823d`; remaining EOF was AIRA-280). Notify deploy and
speed per the release rule, naming the piped-JSON change and
`fastest-ee/deploy/ci/internal/jobspec/runner/in-container-gate.sh:234` (its snapshot log
becomes JSON envelopes).

## 10. Review log

Plan v1 was reviewed by two independent passes (both `PASS_WITH_CHANGES`). Every claim was
checked against the code at `fab2647` before it was applied.

| # | Finding (severity) | Checked | Outcome |
|---|---|---|---|
| R1.1 | Paged read of 1,000 whole subjects can exhaust daemon memory before the 4 MiB cut; count live state too (P1) | True: v1 materialised up to 1,000 x 20 rows with unbounded signatures before measuring | Accepted. 3.2(b) now streams rows (`StreamResourceBudgetSubjects`) and stops at a subject boundary; the first page's budget includes `Waiters`/`Queues`; the subject cap is deleted (bytes are the only bound). Invariant 5 restated; the buffering-vs-streaming difference is an accepted, written-down coverage gap for code review. |
| R1.2 / R2.2 | `replyStoreOp` (`deadlines.go:164`, body refused at `protocol.go:430`) and watch's direct `writeFrame` (`server.go:937`) bypass the fallback; `reply` must report `wrote=true` after the fallback (P1/P2) | True | Accepted. One shared helper for all three post-handler writers, each keeping its own deadline; return value specified; three connection-level tests. Handshake and panic frames stay direct, with the reason stated (fixed short strings); invariant 2 scoped to post-handler replies. |
| R1.3 | Lock-PID-zero case unreachable: `daemon.Status` sets `Running` only for a positive PID and matching boot id, so a held lock with bad metadata skipped the check (P1) | True (`paths.go:524-534`) | Accepted. 3.1 inspects the service `MainPID` (already read by `verifyDaemonReachable`), the process `systemctl restart` replaces; unreadable or zero MainPID is unevaluated and restarts. The "not running -> not stale" short cut is deleted. New invariant 12. |
| R1.4 / R2.3 | The `--json is not valid for confine` refusal (`main.go:247-250`) and the global scope refusals bypass the `ran=no` paths; do not tag management requests (P2) | True | Accepted. New token-only predicate `isConfineLaunch`; the scope refusals (110-113, 128-131), parse errors and the `--json` refusal write `ran=no` only for launches. This also fixes v1's parse-error rule, which keyed on `verb == "confine"` and would have tagged `confine --list --bogus`. |
| R1.5 | The "first install still needs a manual restart" release note is wrong: `install.sh` copies the new binary, then runs it (P2) | True (`install.sh`) | Accepted. 3.1 and section 9 corrected; a manual restart is needed only when a binary is copied in without `aira install`. |
| R1.6 | Keep the minimum smaller: no new `legalSuccessors`; resolve "returned `limit` subjects" permitting an extra empty page (P3) | True | Accepted. 3.5 enumerates the existing local table. `Next` is now exact (invariant 11); an empty `Next`-less page still ends the join cleanly (tested). The single help renderer is kept, because it deletes `runConfineHelpCommand` rather than adding code. |
| R1.7 | Preserve nullable measurements and `unevaluated` through the join; test unknown vs established (info) | Field types checked (`ConfineBudgetRow` uses pointers) | Accepted as a test (5, "honesty through the join"). |
| R2.1 | `in-container-gate.sh:234` executes `aira confine --list` into a log every 5 s; v1's "comments and echo only" was false (P2) | True (fastest-ee working tree) | Accepted. 3.7, risks and release notes name the script; no data lost (line 237 already uses `--json`). |
| R2.4 | Exact-multiple page counts give an extra empty page; test that an empty `Next`-less page ends the loop (P3) | True for v1 | Accepted; resolved by exact `Next` plus the test. |
| R2.5 | Pre-parse must ignore a `removeScopeDir` error and fall through (P3) | True (`scope_dir.go:88`) | Accepted; stated in 3.3. |
| R2.7 | "Dual page bound (1,000 subjects + 4 MiB) is justified" (P3) | Superseded | Not applied: R1.1 showed the subject cap neither bounds memory nor tells whether more remain; streaming removes the reason for it. |
| R2.7 | Builder should confirm `aira ready` reports `ready:true` for all eight tickets before claiming | Not checked in this plan revision | Accepted as a build-start step (the CLAUDE.md "Start here" rule already requires it). |

Nothing was rejected outright, and neither review raised a design fork for the owner.
