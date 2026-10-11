# AIRA v0.32 small-bugs batch: discovery log flood, check, ledger diagnosis, init, advisories, text

Status: PLAN v3 (2026-10-11; v2 revised after the second plan review, see section 10, Review log;
v2 was v1 revised after the first).
Batch of ready bug/chore tickets in one change (owner rule: batch small independent fixes,
one review and test cycle). Base: master `d13ef3b` (v0.31 + ticket
close), protocol 15.

In this batch: AIRA-209, AIRA-218, AIRA-188, AIRA-217, AIRA-216, AIRA-193, AIRA-227,
AIRA-219, AIRA-231, AIRA-263, AIRA-255. Dropped: AIRA-244 (already fixed). AIRA-175: no
production change (it does not reproduce: the rebuild inside `check` already re-points the
row; measured, section 1); one regression pin only.
All thirteen candidates report `ready:true` in `aira ready <id>` (checked 2026-10-10).

**Correctness-sensitive items, two-loop adversarial build review: AIRA-209 (daemon discovery
loop), AIRA-218 (ledger read classification), AIRA-188 (init refusal and prefix claiming),
AIRA-217 (what the OOM advisory may claim).** The rest are message, text or
telemetry fixes.

## 1. Problem, in plain terms, and what still reproduces

Every ticket was re-checked against `d13ef3b`. Ticket line numbers are stale; current ones are
used below.

| Ticket | Plain problem | Still reproduces? | Evidence (2026-10-10) | Verdict |
|---|---|---|---|---|
| AIRA-209 | The daemon re-tries every registry entry whose worktree was deleted, every pass, forking git three times and sleeping 75 ms each time, and logs one line each time. | Yes, measured, worse than filed | `journalctl --user -u aira-daemon.service --since -10min`: 5,480 lines, 5,471 (99.8 %) are `registry discovery skip ... E_NOT_PROJECT`. One pass = 1,363 skips over 194 distinct (worktree, root) keys; **all 194 roots are absent on disk** (`[ -e ]` false for every one). A pass now takes about 120 s (about 88 ms per entry), so the loop runs nearly back to back: about 4,100 git forks per pass. `registry.jsonl` is 2,247 records (AIRA-88's premise was 131). Code: `internal/daemon/discovery.go:46-50` (no stat, no latch), `internal/app/project.go:158` -> `gitValue` retries. | Build |
| AIRA-175 | `aira check` decides "this allocated ID has a ticket file" by looking only at the absolute path recorded for the allocation, which points into whichever worktree a rebuild scanned last; the ticket says deleting that worktree fails the check although the file is in the checkout being checked. | No (measured) | `Check` calls `s.Rebuild` (`internal/store/check.go:245`) before the allocation loop, and Rebuild re-points every `allocated` ticket row to the scanned file (`internal/store/store.go:3110`, guarded `state='allocated' AND path<>?`); every registered worktree that exists is scanned, the checking one included. Probe (throwaway test, deleted): worktrees a, b, c share one state; a mints `FEE-BL-1`, b imports it, c holds a copy; the row is forced to `state='allocated', path=<b's file>`; b's directory is removed; `c.Check` -> `allocated-id-file: pass`, no `E_ID_UNRESOLVED`, row re-pointed to c's file. Live: the root checkout's only `allocated-id-file` failures are AIRA-198 and AIRA-240, which have no file on any branch (`git log --all -- .aira/tickets/AIRA-{198,240}.md` empty): true positives. | Drop (no build); add the probe as a regression pin, section 3.2 |
| AIRA-218 | A ledger record carrying a field this build does not know (another aira build wrote it) is reported as corruption, and the schema-version check that exists for that case can never run because the strict decode fails first. | Yes, by code | `internal/runner/ledger.go:309` `DisallowUnknownFields`; `ledgerRecordDefect` (`:389`) tests `decodeErr` before `SchemaVersion`. `ledgerSchema` is still `1` (`:25`). AIRA-172 already degrades `check`/`reconcile` to `unevaluated`, so only the diagnosis remains. | Build (diagnosis only; see 3.3) |
| AIRA-188 | `aira init` on a new project with no `--prefixes` silently claims the prefix `AIRA`, which is exclusive machine-wide and already owned by this repo, so it fails everywhere else with a confusing ownership conflict. | Yes, by code | `internal/app/project.go:473-476`: `if len(prefixes) == 0 { prefixes = []string{"AIRA"} }`. `docs/project-lifecycle.md:5` says prefixes are optional; `README.md:24` shows bare `aira init`. | Build |
| AIRA-217 | After an OOM at an AIRA-estimated cap, confine says "The kill is now recorded ... re-run and the next admission is sized higher", even when it has no confirmation that the daemon recorded the kill (the report errored, timed out, or was never sent). | Yes, by code | `internal/runner/confine_linux.go:1431` `_ = deps.reportPeak(...)` discards the error; `ReportPeakSample` (`internal/runner/admission_linux.go:797-840`) also errors when the reply is lost after the daemon already committed the sample (`internal/daemon/confine_report.go:99-105` writes before replying), so an error means "unconfirmed", not "not recorded"; `formatConfineReserveAdvisory` (`:1690`) has no recorded parameter; the claim is in both daemon-reserve arms (`:1747`, `:1768`) and the unevaluated arm's "re-running ... admits at a higher reserve" (`:1773`). An acknowledgement alone does not establish the resize either: `RecordConfinePeak` stores an OOM with a nil peak (`internal/store/confine_peak_history.go:99-113`, a non-positive peak becomes NULL), and admission escalates only when `stats.OOMCount > 0 && stats.MaxOOMPeak > 0` (`internal/daemon/admit.go:1393`); an escalation above the ceiling is clamped to the fit or refused (`admit.go:1430-1496`). | Build |
| AIRA-216 | Ticket and finding files are written with `json.Marshal`, which turns `<`, `>`, `&` in a title into `\u003c` etc., so a status-only move rewrites the title bytes. | Yes, measured | 15 ticket files carry the escapes today (`grep -l '\\u003c\|\\u003e\|\\u0026' .aira/tickets/*.md`, e.g. AIRA-182 `--reserve -\u003e --memory-reserve`). Code: `internal/domain/ticket.go:561`, `finding.go:282`, `requirement.go:101`. | Build |
| AIRA-193 | The admission-wait line prints its live totals as raw byte counts beside a rounded ceiling, so "am I near the ceiling?" cannot be read at a glance. | Yes, measured | This plan's own commit hook waited for admission and printed, 15 s apart: `49744607K already granted across 9 admitted jobs / 50048M slice ceiling`, then `51098094182 already granted across 10 admitted jobs / 49984M slice ceiling`, with `system MemAvailable 57704764K` and `own reserve resolves to 1510111K`: three unit systems on one line. Code: `internal/runner/confine_queue_position_linux.go:209, 269, 279, 322-326` and `confine_linux.go:714-721` use `FormatConfineBytes`, which falls back to raw bytes for any non-round value; the file's own ACCEPTED GAP comment (`:245-262`) defers to this ticket. | Build |
| AIRA-227 | (1) After a daemon upgrade, a long-lived client (an `aira mcp` server) fails every call with `E_DAEMON_PROTOCOL: daemon protocol is N, client requested M` and no hint that this process is the stale part. (2) `aira install` as root in a container says only "run 'sudo aira install' from an active login session", never `--ci=shim`. | Yes, by code | (1) `cmd/aira/dispatcher.go:664-666` returns the daemon's frame unchanged when the daemon is newer. (2) `internal/install/install.go:662` `sudoIdentityHint`, used by `validateSudoIdentity` (`:723-736`), which a root container without `SUDO_USER` reaches through `runRootInstall`. | Build |
| AIRA-219 | The agent guide says every heavy command MUST be confined, with no exception for a command already inside a confine scope; the runtime warning says the opposite (drop the inner wrapper). | Yes, partly moot | `internal/core/skill.go:320` has no nesting carve-out (no "outermost" / "already inside" text anywhere in the file). The warning (`cmd/aira/confine_nested.go:57-62`) never says that the parent's own grant is part of what the nested request must fit beside. **Moot part:** the ticket's `--admit-timeout` remedy and "names the budget" request; `--admit-timeout` was removed in S13 and the admission wait is unbounded (client sends no `max_wait_ms`, `internal/runner/admission_linux.go:397`), which makes the nested case sharper: a nested request that cannot fit beside its own parent now waits indefinitely instead of failing after 30 min. | Build (text only) |
| AIRA-231 | aitest's per-pool budget gauge never records anything on a real run, because the worker cap arrives as a string and the code checks `isinstance(..., int)`. | Yes, measured | Probe: `Supervisor()._observe_worker_usage({"scope": <dir with memory.peak>, "memory_max": "104857600"})` leaves `_pool_budget = None`; with the int `104857600` it is set. Code: `internal/pylib/aitest/supervisor.py:2250`. Porous fixtures: `test_supervisor.py:3650-3660` (`_retirable_state` default int) and `:3703`. | Build |
| AIRA-263 | The `aira_cpu` marker text does not say that only CPU-bound, simultaneously runnable streams count (a test whose workers wait on a lock or the GIL is about one core). | Yes, by reading | `internal/pylib/aitest/__init__.py:45-50` (marker registration) and `:134-137` (`_aira_cpu_cores_for_item` docstring). | Build (text) |
| AIRA-255 | aitest comments still say `@aira_mem` drives no admission sizing; since S2b it sizes every worker. | Yes, by reading | `__init__.py:73-74` ("wired in Task 17 ... inert default"; `pytest_runtestloop` exists at `:233`), `__init__.py:90-93`, `supervisor.py:892-894`, `supervisor.py:1138-1142` (all "DOCUMENTED-INERT"). The live consumer is `reservation_need` (`supervisor.py:1178`) via `_need_for` (`:973`). | Build (text) |
| AIRA-244 | `aira import --tickets` rejected the extractor's `origin` field. | No: fixed | `9e210d7` added `Origin string json:"origin"` (`internal/store/import_tickets.go:84`) with a create-and-refresh test (`import_tickets_test.go:113-160`); in every tag since v0.10. | Drop (close as done) |

## 2. Greenfield minimum, and rejected alternatives

If each problem had to be solved alone, from nothing:

- **209:** before running git discovery on a registry root, check that the directory exists;
  print a skip line only when that entry's reason changes, and the pass summary only when its
  counts change.
- **175:** nothing: the rebuild `check` already runs re-points the row (measured).
- **218:** when a strict decode fails, decode the whole payload once more without the
  unknown-field rule (one value, nothing after it); if that succeeds, say the record carries
  an unrecognised field (and check its schema version first).
- **188:** a new project with no prefixes is refused, naming `--prefixes` / `--prefix`.
- **217:** claim the kill is recorded only when the daemon acknowledged a report that carried
  a peak (the only record admission escalates from), and never promise that the next run is
  sized higher; otherwise claim nothing about recording and point at `aira confine --budget`,
  which shows what was recorded.
- **216:** one encoder with HTML escaping off, used by the three front-matter writers.
- **193:** render every byte figure on the admission-wait line in one unit, the nearest MiB
  rule the owner already chose for `aira top`.
- **227:** append the remedy to the two messages.
- **219:** add the carve-out sentence; add one clause to the warning.
- **231:** convert the cap to an int once, and make the fixtures strings like a real grant.
- **263, 255:** edit the text.

The plan below is that minimum, plus two consolidations that delete code: one MiB formatter
shared by `aira top` and the wait line (193), and one cap conversion in
`_observe_worker_usage` instead of two (231).

| Item | Alternative | Why rejected |
|---|---|---|
| 209 | Prune dead breadcrumbs from `registry.jsonl` | `store.go` writes the breadcrumb before the DB transaction precisely because a stale breadcrumb is recoverable evidence; AIRA-88 decided append-only. Its premise (131 lines) has expired (2,247 now): an owner question, section 7, not part of this fix. |
| 209 | Latch a dead root (never retry) | Breaks `TestRunRegistryDiscoveryRetriesTransientEntry`, which requires a transiently failing root to be retried and covered. The stat costs microseconds, so retrying an absent root forever is free once git is not forked. |
| 209 | Stat inside `discoverProjectAt`'s production branch only (the ticket's option b) | Leaves every hook-driven test unable to observe the short-cut, so the "git discovery not called for an absent root" test cannot be written. Option (a) with real-directory fixtures keeps every test meaningful. |
| 209 | Add a log level and demote the line to debug | There is no log-level concept in `internal/daemon`; inventing one is more machinery than a transition map, and the first occurrence of each reason is real information. |
| 209 | Leave the aggregate "discovered/registered/skipped" line printing every pass (v2, the ticket's wording) | Once passes are fast it prints every 30 s with identical counts on this box (`discovered 0 new / 2250 registered, 1363 skipped`, about 2,880 lines a day), again most of the journal. Print-on-change loses no information: every change of count is still printed. The daemon's liveness is systemd's, not this line's. |
| 175 | v1's own-checkout fallback in the allocation loop | No reproduction: `check`'s own rebuild already re-points the row (probe, section 1). A fallback would add a second resolution path that can only ever pass what the first already passes, plus its twin tests. |
| 175 | Bound `check` to the invoking worktree only (the ticket's "second finding") | Changes what `check` covers across all dimensions; the ticket calls it an owner decision. Section 7. |
| 175 | Delete stale `tickets` rows in Rebuild (the 2026-09-09 amendment) | A separate data-model change to the rebuild; no live finding needs it today. Section 7. |
| 218 | A new stable code (e.g. `E_LEDGER_SHAPE_UNRECOGNISED`) | Exit class stays 4 either way (the ledger could not be read), so a code buys only a word, at the cost of a catalogue entry and three degrade sites (`core.go:2241`, `:2295`, `check.go:683`). The owner's standing simplicity rule ("keep the primitive, document the gap") and the ticket's own wont-fix note point the same way. Recorded as an open question for the owner. |
| 218 | Prefix-match `json: unknown field` in the error text | Fragile against Go's wording; the second lenient decode is the structural test. |
| 218 | Two decodes for every record | The ledger is read on every run and reconcile. The second decode runs only on the failure path. |
| 218 | Bump `ledgerSchema` now and add a struct-fingerprint test | A bump makes every older build that is still installed somewhere report the shared ledger as corrupt, for a shape that is compatible. With the lenient re-decode, an additive field is already named precisely without a bump. The doc comment says when a bump is right (an incompatible change). |
| 188 | Default to a prefix derived from the slug or directory | Still a silent claim of a machine-wide exclusive name the caller did not choose, and directory names collide. |
| 188 | Telemetry-only mode with zero prefixes (`--tickets none`) | A new project mode read by several verbs; belongs with AIRA-190. Section 7. |
| 217 | Retry, spool, or widen the 250 ms report deadline | The ticket rules these out; the fix is to stop asserting what the client cannot establish. |
| 217 | Say "the kill was NOT recorded" on a report error (v1) | False negative: the reply can be lost after the daemon committed the sample, and with no signature no report is sent at all. An error establishes only "unconfirmed". |
| 217 | A `peak-report=recorded\|failed` trailer facet | Optional in the ticket; changes the trailer peers parse. Section 7. |
| 217 | One unconditional hedged sentence, no acknowledgement flag (review, v3) | Smaller, but every OOM would then lose the zero-action remedy (re-run the identical command) and send the operator to `--budget`, although the client holds the one signal that decides it in the common case: an acknowledged report carrying a peak. The flag is one boolean computed at the existing call site. The unevaluated arm, where the cap's origin is unknown anyway, does take the unconditional wording. |
| 216 | Rewrite the 15 escaped files now | Not needed: they parse, and each unescapes on its next write. |
| 216 | Also fix `internal/gate/gate.go:225` (`json.MarshalIndent`, same escaping) | Gate files are bound by `definition_digest` in attestations and proofs (`internal/store/gate_eval.go`), so a byte change there has consequences this batch has not reviewed. New observation, section 7. |
| 193 | One-decimal GiB (`51.7G`) | The owner chose whole MiB for `aira top` (`4ea6b86`, authored by the owner); one rule for both faces. |
| 193 | Change `FormatConfineBytes` itself | It renders the trailer's `cap=`, `peak-rss=`, `effective=` facets, where exact units are correct (the ticket forbids changing it). |
| 227 | Detect a stale binary (compare `/proc/self/exe` with the file on PATH) | More machinery than needed: naming the process, its path and its protocol, plus the remedy, is enough to act on. |
| 227 | Detect "inside a container" before choosing the install hint | Naming both remedies in one sentence needs no detection and is never wrong. |
| 219 | Make a nested confine draw on the parent's grant | The AIRA-187 decision: a warning, not grant inheritance (architectural simplicity). |

## 3. Design

### 3.1 AIRA-209: no git fork for an absent root, one log line per change of reason

`internal/daemon/discovery.go`, `discoverRegistryPass`:

- After the covered/failed check and before `s.discoverProjectAt`, `os.Stat(entry.Root)`. If
  the error satisfies `errors.Is(err, fs.ErrNotExist)`: `skipped++`, report the stat error
  through the transition logger (below), `continue`. Any other stat result (exists, or a
  different error) proceeds exactly as today. No latch: the next pass stats again, so a root
  that reappears is discovered.
- New field on `Server` (next to `discoveryFailed`, `server.go:69-72`, guarded by `s.mu`):
  `discoverySkipLogged map[string]string`, key `entry.WorktreeID + "\x00" + entry.Root`,
  value the last logged reason text.
- `logRegistryDiscoverySkip(entry, err)` becomes a `Server` method that logs only when the
  key is absent or its stored text differs from `err.Error()`, then stores the text. Every
  existing skip site (git discovery error, identity mismatch, `ScopeFromProject` error,
  `storeForScope` error) goes through it, so all reasons are transition-logged.
- On the success path (the entry is covered), delete its key, so a later failure logs again.
  No separate recovery line: the aggregate line's `discovered N new` already reports it.
- The aggregate line (`discovery.go:80-82`) keeps its text and its existing condition
  (`discovered > 0 || skipped > 0`) and is additionally printed only when the
  (discovered, registered, skipped) triple differs from the last one printed. One more `Server`
  field beside the map, `discoveryLastSummary [3]int`, under `s.mu`. A pass whose triple
  repeats prints nothing; the first pass after a daemon start always prints (the zero triple
  never matches a printable one).
- Fixture change in `TestDiscoverRegistryPassSkipsBadEntriesAndGuardsIdentity`
  (`discovery_test.go:141-142`): `/reused` becomes a real directory under `t.TempDir()` so the
  identity-mismatch branch is still reached through the hook (otherwise the stat would skip it
  and the test would pass vacuously); `/missing` becomes an absent path under `t.TempDir()`.
  The test also asserts the hook was called for the reused root.

Effect on the box: 1,363 skip lines per pass become 194 lines once per daemon start, and the
summary line prints once per change of counts instead of every pass; git forks for absent
roots drop from about 4,100 per pass to 0; a pass takes milliseconds.

### 3.2 AIRA-175: no build; pin the behaviour that already holds

No production change. One regression test in `internal/store` (the section 1 probe, made
permanent beside `TestImportRebuildResolvesCrossWorktreeAdoptedAllocation`): three
worktrees sharing one state; the allocation row is `allocated` and points at worktree b's
file; b's directory is removed; `Check` from c, which holds the file, reports
`allocated-id-file: pass`, no `E_ID_UNRESOLVED`, and the row now points at c's file. It
passes today; it fails if the rebuild re-point (`store.go:3110`) is removed. Note for the
record: `Check` is not read-only (it runs reconcile and `Rebuild`, which can write
projections and recovery receipts). Requirement allocation rows have no equivalent re-point
(Rebuild's requirement loop, `store.go:3124-3184`, upserts the projection but never updates
the allocation's path), and a requirement row CAN stay `allocated` beside its file: the crash
window between the file write and `markRequirementMaterialised` (`requirement.go:275`), which
`TestCheckResolvesCrashWindowRequirementAllocation` (`check_requirement_test.go:86`) covers in
the allocating worktree. Whether such a row resolves from another worktree after the
allocating one is removed is unevaluated, and the missing re-point is a possible false-fail
direction (`E_ID_UNRESOLVED` for a requirement file present in the checking worktree). Not
built here (no reproduction, and it needs a crash plus a removed worktree); recorded in
section 7 with its one-statement fix.

### 3.3 AIRA-218: say what an unknown ledger field is; make the schema arm reachable

`internal/runner/ledger.go`, `(*ledger).read` (`:307-319`) and `ledgerRecordDefect`:

- Hot path unchanged: one strict decode (`DisallowUnknownFields`) per record, then the
  existing trailing-payload check.
- Only when the strict decode fails: decode the same payload again into a fresh
  `ledgerEvent` without `DisallowUnknownFields`, and require the decoder to reach `io.EOF`
  after that one value (the same trailing-content rule the strict path applies). The lenient
  result counts as "decoded" only if both hold.
  - lenient decode or its EOF check fails -> today's message, `payload did not decode as a
    ledger event: <strict error>`;
  - lenient decoded and `SchemaVersion != ledgerSchema` -> `schema_version is X, want Y
    (<strict error, which names the field>)` (the arm is now reachable for a record that also
    carries new fields; the strict error is appended so the field name is not lost, including
    for a record with no `schema_version`, which reads as 0). A strictly decoded record with a
    wrong schema keeps today's message unchanged;
  - lenient decoded with the expected schema -> `record carries a field this aira build does
    not recognise (<strict error, which names the field>); possibly a record shape from a
    different aira build: run one aira build on this machine`. It claims nothing about who
    wrote the record (a matching checksum shows only that payload and checksum agree) and
    never says "newer".
- Code stays `E_JOURNAL_CORRUPT`, exit 4, so the AIRA-172 degrade sites are untouched (`check`
  and `reconcile` still grade `run-ledger: unevaluated`) and the accept/reject decision is
  unchanged: only the message differs. `ledgerRecordIdentity` uses the lenient event only
  when it counts as decoded, so run id and sequence are then reported as "decoded".
- `ledgerSchema` doc comment: bump only for an incompatible record change (a field whose
  meaning changes, or one removed); an additive `omitempty` field needs no bump, because an
  older build names the field it does not know.
- Update `ledgerRecordDefect`'s comment (`:378-388`), which says a newer binary's field
  "reads as `json: unknown field`".

### 3.4 AIRA-188: a new project must name its prefixes

`internal/app/project.go`, `PrepareInit`, non-adopt branch (`:473-476`): replace the default
with a refusal, before any file is written or any prefix claimed:

`E_CONFIG_INVALID: a new project needs at least one ID prefix: pass --prefixes <P,...> or
--prefix <P> (repeatable), for example --prefix DEMO. Prefixes are exclusive across every
project on this machine, so AIRA does not choose one for you.` (The CLI folds repeated
`--prefix` into `prefixes`, `cmd/aira/main.go:885-889`; the docs and help use `--prefix`.)

- Code `E_CONFIG_INVALID` (exit 2): the code `validateConfig` already uses for the same
  condition (`config has no prefixes`, `:586-588`); one code for one fact.
- The adopt path (a committed `.aira/config`) is unchanged: its prefixes come from the file.
- Both faces reach it: the CLI and MCP `init` go through the daemon's `server.go:1189`
  `app.PrepareInit` or `dispatcher.go:916` `app.Init`.
- Docs: `README.md:24` becomes `aira init --prefixes FOO   # adopt this repo (FOO: your ID
  prefix)`; `docs/project-lifecycle.md:5-9` says the prefix is required for a new project and
  explains why; the help example at `internal/core/core.go:2439` uses `DEMO` instead of
  `AIRA` (this machine's AIRA prefix is owned).

### 3.5 AIRA-217: the OOM advisory claims recording only for an acknowledged report with a peak, and never promises "higher"

`internal/runner/confine_linux.go`:

- `peakAcknowledged := false`; inside the existing `if signature != "" &&
  shouldRecordConfinePeak(...)` block, `peakAcknowledged = deps.reportPeak(...) == nil`
  (`ReportPeakSample` returns nil only on the daemon's `OK` reply, sent after
  `RecordConfinePeak` commits). Not attempted (no signature) is not acknowledged.
- `formatConfineReserveAdvisory` gains `peakAcknowledged bool` and derives one local:
  `recorded := peakAcknowledged && peakRSS != nil && *peakRSS > 0`. The peak term is needed
  because the daemon stores an OOM with an unknown peak as NULL and admission escalates only
  from `MaxOOMPeak > 0` (`admit.go:1393`): an acknowledged report without a peak is recorded
  but drives no resize. The peak the advisory sees is the same `usage.PeakRSS` the report
  carried.
- Even `recorded` does not establish "sized higher": the escalation can be clamped to the
  slice's fit or refused `E_ADMIT_TOO_LARGE` (`admit.go:1430-1496`). So no arm says "sized
  higher" any more.
  - daemon-reserve, ordinary arm, `recorded`: `... The kill and its peak are recorded against
    this command's signature: RE-RUN THE IDENTICAL COMMAND and admission sizes the next run
    from that record on its own, or pin --memory-reserve <suggested> now to skip the cycle. If
    an identical re-run is admitted at the same cap and killed again, that is a genuine bug
    worth reporting.` (Today's text with "is sized higher" replaced and "admitted at" added:
    a clamped or refused re-run is not one admitted at the same cap.)
  - daemon-reserve, ordinary arm, not `recorded`: `... AIRA could not confirm that this kill
    was recorded with its peak against the command's signature, so do not rely on the next
    admission being sized from it: run aira confine --budget to see what was recorded, or pin
    --memory-reserve <suggested> to size the re-run yourself.` No "genuine bug worth
    reporting" clause.
  - daemon-reserve, over-ceiling arm (`suggested >= sliceCapBytes`), `recorded`: today's text,
    byte-identical (it already names both outcomes, fitted or refused, and claims no
    "higher").
  - over-ceiling arm, not `recorded`: `... so no pin is offered. AIRA could not confirm that
    this kill was recorded with its peak, so automatic resizing is unevaluated: run aira
    confine --budget to see what was recorded; split heavy work, or run where the slice is
    larger.` It never names a pin (the AIRA-184 BLOCK).
  - unevaluated cap source, one wording whatever the flag (the cap's origin is unknown, so
    the record's effect is too): the clause "if AIRA estimated it, re-running the identical
    command admits at a higher reserve" becomes "if AIRA estimated it, run aira confine
    --budget to see whether this kill was recorded against the command, which is what lets
    admission size an identical re-run from it".
  - `--memory-max` / `--memory-reserve` arms and the near-cap line: unchanged (they assert no
    recording).

### 3.6 AIRA-216: front matter without HTML escaping

`internal/domain`: one unexported helper `marshalFrontmatter(v any) ([]byte, error)`:
`json.NewEncoder(&buf)`, `SetEscapeHTML(false)`, `Encode(v)`, then
`bytes.TrimRight(buf.Bytes(), "\n")` (the encoder appends a newline that `json.Marshal` does
not; the caller adds `\n---\n`). Used by `RenderTicket` (`ticket.go:561`), `RenderFinding`
(`finding.go:282`) and `RenderRequirement` (`requirement.go:101`, consistency only: its
fields carry no free text).

Safety, checked: neither `Ticket` nor `findingFrontmatter` contains a field with a custom
`MarshalJSON` (the package's two marshalers are on `HeldLease` and `Lease`, `ticket.go:395,
525`), so no inner marshaler re-escapes. No reader compares file bytes with render output
(`check.go` has no `RenderTicket` reference; every caller listed by
`grep RenderTicket\|RenderFinding\|RenderRequirement internal/store` is a write path that
CASes on the old bytes). No migration: escaped files keep parsing and unescape on their next
write.

### 3.7 AIRA-193: one unit on the admission-wait line

- Move `topFormatMegabytes` (`cmd/aira/tui_top.go:171`) to `internal/runner` as
  `FormatConfineMegabytes(value int64) string` (unchanged rule: nearest MiB, half up, `"0M"`
  for `value <= 0`); `aira top`'s call sites in `cmd/aira/tui_top.go` call it; the cmd/aira
  copy and its test move to runner. One implementation for both faces.
- The rounding is made overflow-safe while moving it: today's `(value+mib/2)/mib`
  (`tui_top.go:176`) overflows for `value > MaxInt64 - 2^19` and prints a negative figure.
  New form: `q, r := value/mib, value%mib; if r >= mib/2 { q++ }` (same result everywhere
  else; `q++` cannot overflow, since `q <= 2^43 - 1`).
- Every byte figure on the admission-wait line uses it: the head reserve and declared VRAM
  (`confine_linux.go:714-721`), the MemAvailable note (`confine_queue_position_linux.go:193`),
  `queued ahead` (`:209`), `already granted` and `slice ceiling` in `confineHeldNote`
  (`:269, 279`), and both figures in `confineOwnReserveNote` (`:322-326`). The two
  established-zero literals change from `"0B"` to `"0M"`: `ahead` (`:207`, head of the queue)
  and `held` (`:267`, an established empty slice: `0M already granted across 0 admitted
  jobs`). Existing guards (`heldEstablished`, `ceilingBytes > 0`) are unchanged, so an
  unestablished figure is still omitted, never printed as `0M`.
- Delete the ACCEPTED GAP block in `confineHeldNote`'s comment (`:245-262`), replaced by one
  line naming AIRA-193.
- Example, the ticket's live line: `5502M queued ahead, 51684M already granted across 5
  admitted jobs / 52608M slice ceiling`.
- Unchanged: `FormatConfineBytes` and the trailer; the terminal `E_ADMIT_SATURATED` message
  and `confine --list` (section 7).

### 3.8 AIRA-227: both errors name their fix

1. `cmd/aira/dispatcher.go`, `exchangeWithReplacement`, the `daemon.ProtocolVersion <
   response.Proto` branch (`:664-666`): before returning, append to `response.Error`:
   `; this aira process (pid <os.Getpid()>, <os.Executable() or "unevaluated">, build
   <buildid.Current()>) speaks protocol <ProtocolVersion>, older than the daemon: it is
   running an older aira than the daemon. Restart this process (an `aira mcp` server keeps
   the binary it started with until it is restarted), or put the installed aira first on
   PATH; `aira version` compares the two.` Code, exit and `Proto` are unchanged. The
   client-newer branch (daemon replacement) is unchanged.
2. `internal/install/install.go:662`: `sudoIdentityHint` gains a second door: `run 'sudo aira
   install' from an active login session as a non-root account; in a container or on a host
   without systemd, run 'aira install --ci=shim' instead (or --ci=auto, which chooses it when
   no systemd user manager is reachable)`. Every use of the constant inherits it.

### 3.9 AIRA-219: wrap once, at the outside

- `internal/core/skill.go:320`, after the MUST sentence: `A command already running inside an
  \`aira confine\` scope is already confined and capped by that scope (memory accounting is
  hierarchical): wrap ONCE, at the outermost heavy command, and do not wrap its children
  again. A nested \`aira confine\` is a separate admission request; on the parent's slice it
  must fit beside every reservation already granted there, its parent's included, and
  admission has no time limit; it prints a warning saying so.`
- `cmd/aira/confine_nested.go`, `nestedConfineWarning`: add `When it requests the parent's
  slice, it must fit beside every reservation already granted there, the parent's own
  included; admission has no time limit, so a request that cannot fit beside its parent can
  wait until it is cancelled.` The warning still fires for a different `--slice` (its existing
  clauses hold there, `confine_nested.go:43-46`); the new sentence is conditional, so it is
  true in both cases. The exemptions (empty parent id, `--exclusive`) are unchanged.

### 3.10 AIRA-231: the pool budget gauge reads string caps

`internal/pylib/aitest/supervisor.py`, `_observe_worker_usage`: convert `memory_max` once at
the top (`int(str(v).strip())`, `None` on `TypeError`/`ValueError`, an existing int passes
through), and use that one value both for the `_pool_budget` update (`> 0` only) and for the
AIRA-230 measurement record (`:2271-2277`, whose own conversion is deleted). Delete the
"string-cap behaviour is AIRA-231, out of this slice's scope" comment. Fixtures:
`_retirable_state` default `memory_max="104857600"` (`test_supervisor.py:3650`) and the
direct call at `:3703` use strings, like a real grant.

### 3.11 AIRA-263: `aira_cpu` counts CPU-bound streams only

`internal/pylib/aitest/__init__.py`: the marker registration string (`:45-50`) and the
`_aira_cpu_cores_for_item` docstring (`:134-137`) state: N is the peak number of execution
streams (processes or threads) that are simultaneously runnable and CPU-bound, each really
occupying a core. Streams serialised by the GIL or blocked on a lock (for example 16 SQLite
writers where one holds the write lock) count as about one core: mark N=1 or leave unmarked.
Over-declaring starves the test's own admission and displaces N-1 cores of other workers. No
behaviour change.

### 3.12 AIRA-255: remove the stale "inert" comments

Replace the four stale sites (`__init__.py:73-74`, `:90-93`; `supervisor.py:892-894`,
`:1138-1142`) with the S2b mechanism: `reservation_need = worker overhead
(AIRA_AITEST_WORKER_OVERHEAD_BYTES, default 512M) + the aira_mem value when annotated, else
0`, consumed by `_need_for`; `AIRA_AITEST_ESTIMATED_BYTES` does not size workers.
`internal/core/skill.go`'s aitest text is already correct and is not touched.

## 4. Invariants

1. (209) Registry discovery never runs git for a root that `os.Stat` reports absent. A root
   that exists is discovered exactly as today, every pass (no latch).
2. (209) For one (worktree id, root) key, a skip line is printed only when the reason text
   differs from the last one printed for that key; a key that is covered is forgotten. The
   `skipped` count still counts every skipped entry every pass. The summary line is printed
   only when its (discovered, registered, skipped) triple differs from the last one printed.
3. (175) No production change; `check`'s rebuild keeps re-pointing an `allocated` ticket row
   to a live file (pinned by a test).
4. (218) The set of ledger records accepted is unchanged; only the message of a rejection
   changes. A strictly valid record is decoded once.
5. (218) A ledger message never claims who wrote a record or that it was newer; the
   unknown-field wording is used only when the whole payload is exactly one JSON value that
   decodes leniently.
6. (188) A fresh init with no prefixes writes no file and claims no prefix.
7. (217) The OOM advisory asserts that a kill was recorded only when `reportPeak` returned
   nil (the daemon's acknowledgement) for this run AND the run's peak was known and positive;
   otherwise it asserts neither recorded nor not-recorded. No arm promises that the next run
   is sized higher, and the over-ceiling arm never names a pin.
8. (216) Rendered front matter contains no `\u003c`, `\u003e` or `\u0026`; for content without
   those characters the rendered bytes are identical to today's; a render followed by a parse
   round-trips.
9. (193) Every byte figure on the admission-wait line is in whole MiB, established zeros
   included (`0M`); the trailer and every other surface keep `FormatConfineBytes`. An
   unestablished figure is omitted, never `0M`. The MiB formatter never prints a negative
   figure for a positive input (no overflow up to `MaxInt64`).
10. (231) `_pool_budget` is set from a real (string) grant and stays `None` for a cap that
    does not parse; never a fabricated 0.
11. (227, 219, 263, 255) Codes, exits and behaviour unchanged; only text.

## 5. Tests (TDD; each must fail against the wrong implementation)

Record each test RED on `d13ef3b` (or RED against the named mutation where the bug cannot
fail today). Each listed mutation must turn at least one test RED.

**AIRA-209** (`internal/daemon/discovery_test.go`, the existing `captureDaemonLog` helper):
- `TestDiscoveryLogsAMissingRootOncePerTransition`: one entry whose root is an absent path
  under `t.TempDir()`; three passes -> exactly one skip line, `skipped == 1` on every pass
  (RED today: three lines).
- `TestDiscoverySkipsGitDiscoveryForAnAbsentRoot`: hook counter 0 after two passes over an
  absent root (RED today: 2).
- Transition, one entry over successive passes: root absent (reason A) twice -> one line;
  root created as a plain directory and the hook fails with reason B -> exactly one more line;
  hook returns the project -> the entry is covered and its key is gone from
  `discoverySkipLogged` (read under `s.mu`).
- Two entries with the same worktree id and different roots, both absent -> two lines (key
  includes the root).
- `TestRunRegistryDiscoveryRetriesTransientEntry` passes unmodified.
- The converted `TestDiscoverRegistryPassSkipsBadEntriesAndGuardsIdentity` asserts the hook
  was called for the reused root.
- Summary line: three passes over one absent root -> exactly one `registry discovery:`
  summary line; a fourth pass after a second absent entry is appended to the registry
  (registered and skipped change) -> exactly one more (RED today: three, then four).
- Mutations: log every pass (first test RED); stat after the hook (second RED); key on
  worktree id only (two-roots RED); never delete the key on success (transition test RED);
  latch an absent root (the transition test's last step never reaches the hook, RED); print
  the summary every pass (summary test RED); never print it again after the first (summary
  test's fourth pass RED).

**AIRA-175** (`internal/store`): the regression pin of section 3.2 (passes today). Mutation:
drop the re-point `UPDATE` in Rebuild (RED).

**AIRA-218** (`internal/runner/ledger_corrupt_diagnostics_test.go`, synthetic ledgers in
`t.TempDir()`):
- schema 1 record plus an unknown field -> `E_JOURNAL_CORRUPT`, message names the field and
  says "does not recognise", does not contain "did not decode", "newer" or "written by"
  (RED today);
- schema 2 record plus an unknown field -> `schema_version is 2, want 1` and the field name
  (RED today: "did not decode");
- record with no `schema_version` plus an unknown field -> `schema_version is 0, want 1` and
  the field name (pins the precedence choice: schema first, field name kept);
- schema 2 record with no unknown field -> exactly today's `schema_version is 2, want 1`
  (strict path unchanged);
- malformed JSON with a valid checksum -> `payload did not decode` (unchanged);
- schema 1 record plus an unknown field, followed by trailing content in the same payload
  (a second JSON value; garbage bytes) -> `payload did not decode`, not the unknown-field
  wording;
- the other four defects keep their messages; valid ledgers still read (existing tests).
- `internal/store/check_run_ledger_test.go`: a ledger with an unknown field grades
  `run-ledger: unevaluated` and every other dimension exactly as a healthy ledger
  (regression guard; passes today).
- Mutations: drop the lenient re-decode (first two RED); check the schema after the
  unknown-field branch (second and third RED); drop the appended strict error from the schema
  arm (second and third RED); drop the lenient EOF check (trailing-content case RED);
  classify with the lenient decode on the hot path and accept unknown fields (a strict-reject
  test RED). Coverage gap, accepted: that the lenient
  decode runs only on the failure path is a code-reading item for the build review.

**AIRA-188** (`internal/app/project_test.go`, `internal/daemon/adopt_test.go`):
- fresh git repo, `Init` with no prefixes -> error code `E_CONFIG_INVALID`, message contains
  `--prefixes` and `--prefix <P>`, no `.aira/config` written (RED today: succeeds with AIRA);
- daemon face: `server.bootstrap` on a fresh repo with no prefixes -> refused and no
  `prefix_ownership` row for `AIRA` (RED today);
- adopt path with a committed config and no `--prefixes` -> succeeds (existing tests).
- Mutations: restore the default (both RED); refuse in the adopt path too (adopt tests RED).

**AIRA-217** (`internal/runner`):
- `formatConfineReserveAdvisory` table, full expected strings. The existing goldens are
  updated, not kept: the daemon-reserve ordinary rows and the unevaluated row change text
  (they carried the "sized higher" / "admits at a higher reserve" promise); the over-ceiling
  rows with `peakAcknowledged: true` and a known peak stay byte-identical. New rows:
  - ordinary arm and over-ceiling arm, `peakAcknowledged: false`, known peak -> hedged text;
  - ordinary arm and over-ceiling arm, `peakAcknowledged: true`, peak `nil` -> the same
    hedged text (acknowledged, but a record admission cannot escalate from);
  - unevaluated arm with each flag value -> the same string.
  Every row asserts no "sized higher" and no "admits at a higher reserve"; every hedged row
  asserts no "The kill and its peak are recorded", no "NOT recorded"/"was not recorded", no
  "genuine bug worth reporting", and contains `aira confine --budget`; the over-ceiling hedged
  rows assert no `--memory-reserve` pin.
- Plumbing, through `confineWithDeps`, own-cap OOM at a daemon-reserve cap:
  - `deps.reportPeak` returns an error -> the advisory carries the hedged text (RED against a
    build that ignores the result);
  - acknowledgement lost after persistence: the stub appends the sample to a fake history,
    then returns `context.DeadlineExceeded` -> the history holds the sample AND the advisory
    carries the hedged text, never a not-recorded claim;
  - `deps.reportPeak` returns nil with a known peak -> the recorded text;
  - `deps.reportPeak` returns nil with the peak unreadable (usage `PeakRSS == nil`) -> the
    hedged text;
  - no signature (an argv `ResourceSignature` rejects, if the existing deps can reach the
    OOM path with one) -> `reportPeak` is not called and the advisory carries the hedged text.
    If no fixture can reach it, it is a recorded coverage gap: the `false` default of the flag
    is read in the build review.
- Mutations: `peakAcknowledged = true` unconditionally (plumbing RED); drop the peak term
  from `recorded` (nil-peak rows RED); invert the flag (goldens RED); name a pin in the
  over-ceiling hedged arm (RED); say "was not recorded" in a hedged arm (RED); keep "sized
  higher" in the recorded arm (golden RED).

**AIRA-216** (`internal/domain`):
- rendered bytes for a ticket titled `planned -> done & <id> handling`: no `\u003c`,
  `\u003e`, `\u0026`; the literal title is present; `ParseTicket` round-trips (RED today on
  the bytes); the same for `RenderFinding` with `WaiverReason` `<a> & <b>`.
- delimiter bytes, asserted directly (`ParseTicket` tolerates JSON whitespace, so a
  round-trip cannot catch an extra newline): for a ticket, a finding and a requirement, the
  rendered bytes start with `---\n{`, the header JSON is a single line, and it is followed by
  exactly `}\n---\n` (no `}\n\n---\n`); for content with no `<`, `>`, `&`, a golden of
  today's rendered bytes is unchanged.
- Mutations: `SetEscapeHTML(true)` (bytes RED); omit the trailing-newline trim (delimiter
  test RED).

**AIRA-193** (`internal/runner`, `cmd/aira`):
- `FormatConfineMegabytes`: zero, negative, exact, round-down, round-up, large (the moved
  `aira top` test); boundary: `MaxInt64` -> `8796093022208M`, `MaxInt64 - 2^19 + 1` (the
  first value today's form overflows) -> `8796093022208M`, and exactly half a MiB above a
  whole MiB rounds up (RED today on the two boundary rows: negative output).
- queue line: held 54194584616 B over 5 jobs, ceiling 52608 MiB, ahead 5769372876 B ->
  contains `5502M queued ahead, 51684M already granted across 5 admitted jobs / 52608M slice
  ceiling` (RED today); own-reserve over-ceiling note in M; head reserve `12288M` for a 12 GiB
  reserve.
- established empty slice (held 0 B over 0 jobs) -> `0M already granted across 0 admitted
  jobs` (RED today: `0B`); head of the queue -> `0M queued ahead` (RED today: `0B`);
- unestablished held pair or ceiling -> the clause is omitted, as today.
- trailer unchanged: an existing `FormatConfineStatus` golden with `cap=enforced(64G)` still
  passes.
- `aira top` render test (`TestTopRuntimeRendersAndQuits`) still shows `65536M`/`28672M`.
- Mutations: leave one figure on `FormatConfineBytes` (RED); leave either zero literal at
  `"0B"` (RED); change `FormatConfineBytes` instead (trailer golden RED); restore
  `(value+mib/2)/mib` (boundary RED); round half down (`r > mib/2`, half-MiB row RED).

**AIRA-227** (`cmd/aira/dispatcher_inprocess_test.go`, `internal/install`):
- exchange returns `Proto: daemon.ProtocolVersion+1, Code: E_DAEMON_PROTOCOL` -> response code
  and exit unchanged, error contains the test binary's `os.Executable()` path, `protocol
  <ProtocolVersion>` and `Restart this process` (RED today);
- client-newer case: the existing replacement test passes unchanged;
- root install with no `SUDO_USER` -> error contains `--ci=shim` (RED today).
- Mutations: drop the suffix (first case RED); drop the `--ci=shim` door (install case RED).

**AIRA-219** (`cmd/aira/confine_nested_test.go`, `internal/core/skill_test.go`):
- `nestedConfineWarning("...", "<parent>")` contains `When it requests the parent's slice`
  and `no time limit` (RED today); exemption tests unchanged;
- generated skill text contains `outermost` and `already running inside` (RED today).

**AIRA-231** (`internal/pylib/aitest/test_supervisor.py`, run by
`TestRealPytestAitestPackageUnitTests` in `make ci`):
- string `memory_max` grant -> `_pool_budget == 104857600` (RED today: `None`);
- `memory_max="abc"` -> `_pool_budget is None`, `_pool_peak_records` cap `None`;
- the existing retire-path tests, now with string fixtures, still assert the budget.
- Mutation: restore `isinstance(memory_max, int)` (RED).

**AIRA-263, AIRA-255**: text only. Coverage gap, accepted: no test (a prose pin cannot fail
against wrong prose); reviewed by reading.

Gate: `aira confine -- make ci` (the gate's exact command, foreground on a quiet box), plus
`go test -race ./internal/daemon/... ./internal/store/...`.

## 6. Expected yield

- The daemon journal becomes readable again: about 99.8 % of its lines (5,471 of 5,480 in 10
  minutes) disappear; watchdog, slice-ceiling and OOM-steer lines are no longer buried. About
  4,100 git forks per pass and a near-continuous discovery loop stop.
- AIRA-175 closes with a pin instead of new code (its mechanism is already handled).
- A ledger record with an unrecognised field is named as such in one read, with the remedy.
- `aira init` can no longer claim `AIRA` by accident on another repository.
- An OOM advisory no longer promises a resized re-run it cannot confirm, so an operator is
  not sent into a repeat kill and then told to report it as an AIRA bug.
- Status moves stop rewriting title bytes; a stale MCP server and a root container install
  each name their fix; the guide and the warning agree on nested confine; aitest's pool
  budget reaches `confine-report`; the aitest docs match the code.

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| AIRA-244 | Fixed in `9e210d7` (v0.10). Close as done. |
| AIRA-175: bound `check` to the invoking worktree; Rebuild deleting stale `tickets` rows | Owner decision and a data-model change respectively (the ticket says so). |
| AIRA-175: requirement allocation rows have no rebuild re-point, so a crash-window requirement row (`allocated` beside its file) whose allocating worktree is later removed can fail `allocated-id-file` from a worktree that holds the file (possible false-fail, unevaluated); `allocated-id-file` runs on unreconciled rows when `check`'s rebuild itself fails | Not reproduced (needs a crash in the write-to-materialise window plus a removed worktree; a failed rebuild already fails or unevaluates `rebuild-integrity`). The fix, if wanted, is the ticket loop's guarded re-point `UPDATE` (`store.go:3110`) mirrored into the requirement loop, with a test like section 3.2's. Suggest a ticket. |
| `check`'s 7 `E_RELATION_INDEX_DIVERGENCE` and 2 `E_FINDING_INDEX_DIVERGENCE` on the root checkout today | Not triaged; they may be a stale index or another false-positive class. Suggest a ticket. |
| AIRA-209: `registry.jsonl` growth (2,247 records; AIRA-88 decided append-only on a 131-record premise) | AIRA-88's decision stands until the owner revisits it; open question. |
| AIRA-218: a distinct code; the ~13 replay-integrity `E_JOURNAL_CORRUPT` sites without record coordinates (`ledger.go:211-225`, `:461-495`, `internal/store/gate_audit.go:69-83`) | Owner question / coverage gap recorded (RANT-22 recurs only on a replay failure). |
| AIRA-188: telemetry-only projects (zero prefixes, a named mode) and the enable-tickets transition hazard | A new project mode read by several verbs; belongs with AIRA-190. |
| AIRA-217: `peak-report=acknowledged\|unconfirmed` trailer facet | Optional; changes a parsed surface. |
| AIRA-217: the ordinary arm's pin checked against the slice's whole cap, not cap minus headroom | The existing ACCEPTED GAP (`confine_linux.go`, AIRA-184), unchanged. |
| AIRA-216: `internal/gate/gate.go:225` gate files escape `&`/`<`/`>` too | Gate files are digest-bound in attestations; needs its own review. Suggest a ticket. |
| AIRA-193: `E_ADMIT_SATURATED` terminal message and `confine --list` figures | Different surfaces, not asked for. |
| AIRA-219: the ticket's `--admit-timeout` remedy and "name the budget" | Moot since S13 (no admission timeout exists). |
| AIRA-227: confine/worker-admit skew messages | Already carry a remedy (`worker_admit_client_linux.go:431-436`). |

## 8. Risks

- **Discovery (209).** A root that exists but is not a project still forks git every pass
  (rare; logged once). The summary line no longer prints every pass, so a journal reader no
  longer sees a per-pass heartbeat; any change of counts is still printed. On each daemon
  start the 194 dead keys log once (a burst, then silence). Review: the map and the last
  summary are read and written under `s.mu` only.
- **Init (188).** Any script running bare `aira init` on a new repository now fails with exit
  2 and the remedy. No compat obligation (owner rule); the README and lifecycle doc are
  updated. Repositories with a committed config are unaffected.
- **Byte change (216).** The first write to each of the 15 escaped ticket files changes its
  title bytes once (an expected one-time diff). `aira import --tickets` re-run over a ticket
  whose title holds `<`, `>` or `&` reports `refreshed`, not `unchanged`, once. fastest.ee's
  backlog import is the one peer that re-imports; mention it in the release notes.
- **Wait-line format (193).** Peers that parse the admission-wait line see `M` figures
  instead of raw bytes; it is a human progress line, not a contract (the trailer is
  unchanged). A pinned round reserve now reads in MiB on the head line too: `--memory-reserve
  12G` echoes `12288M` (release notes).
- **Message text (217, 218, 219, 227).** Codes and exits unchanged; tests pinning exact text
  are updated, not added beside.
- **Daemon restart.** 209, 188 and 216 run inside the daemon; `aira install` restarts it
  when the binary changed (AIRA-203, v0.31). Skill reinstall needed for 219.

## 9. Build order and release notes

Build in this order (independent; correctness-sensitive first so the two-loop review has the
most time): 209, 218, 188, 217, 175 (pin only), 216, 193, 227, 219, 231, 263, 255. Proto unchanged
(15). Daemon restart via `aira install`; `aira skill install ~/.claude/skills/aira --force`
for the skill text; the embedded aitest pylib changes (231, 263, 255) ship with the binary.
Release notes also name: the admission-wait line's figures are whole MiB (a pinned
`--memory-reserve 12G` echoes `12288M`); the daemon's discovery summary line prints on change
only; the OOM advisory no longer promises a higher reserve and points at `aira confine
--budget` when it cannot confirm the record. Close AIRA-244 as done. AIRA-175: close the path-lookup report as not reproducing (pin
landed); whether to keep it open for its two owner decisions (section 7) is an owner question. Notify deploy and speed per the release rule; tell deploy that the
`@aira_cpu` text changed (AIRA-263; their `pytest.ini` copy) and that bare `aira init` on a
new repository is now refused.

## 10. Review log

### Plan v1 -> v2

Two plan reviews (one BLOCK, one PASS_WITH_CHANGES). Each claim was checked against the code.

| # | Claim | Verdict | Evidence / change |
|---|---|---|---|
| R1.1 | AIRA-175: `Check` runs `Rebuild` before the allocation loop and Rebuild re-points `allocated` rows, so the fallback likely passes today; require a RED repro or remove. Requirements lack the re-point. `Check` is not write-free. | Accepted | `check.go:245` then the loop at `:262`; `store.go:3110` re-point; no re-point in the requirement loop (`store.go:3130-3180`). Probe (throwaway `zz_` test, deleted) did not reproduce: `allocated-id-file: pass`, row re-pointed to the checking worktree. Fallback removed; regression pin added (3.2); "check writes nothing" removed. |
| R1.2 | AIRA-217: an error from `ReportPeakSample` can follow a persisted sample; say "could not confirm", not "nothing recorded". Test acknowledgement loss after persistence. | Accepted | `confine_report.go:99-105` commits before replying; `admission_linux.go:830-833` errors on a lost reply. Flag renamed `peakAcknowledged`; hedged wording (3.5); lost-ack plumbing test added. |
| R1.3 | AIRA-218: a checksum shows agreement, not authorship; a single lenient `Decode` accepts trailing content. | Accepted | `ledger.go:303-319`: the strict path checks trailing content only after a valid decode. Wording no longer names the writer; the lenient decode must reach EOF; trailing-content test and mutation added (3.3). |
| R1.4 / R2.2 | AIRA-219: the warning fires for a different `--slice` too, so "must fit beside every reservation on the slice" is false there; "waits indefinitely" must be conditional. | Accepted | `confine_nested.go:43-56`. Both texts now say "when it requests the parent's slice" and "can wait until it is cancelled" (3.9). |
| R1.5 | Tests can falsely pass: `ParseTicket` tolerates whitespace; the established-zero held literal is `0B`; keep 175 twins. | Accepted (175 part moot) | `ticket.go:593-605` (JSON decoder, then `\n---\n` marker). Delimiter-bytes test added (216); `0B` -> `0M` for both zero literals, `:207` and `:267`, with tests (193). 175 has no fallback, so no twins. |
| R1.6 | Remaining minimum sound; trim speculative machinery and prose. | Accepted | 175's fallback (the speculative part) removed; v1 prose for it cut. |
| R2.1 | AIRA-217 wording false-negative; `signature==""` skips the report. | Accepted | Same as R1.2; the "not attempted" case is unacknowledged and uses the same hedge. |
| R2.3 | AIRA-188 refusal should name `--prefix` as well as `--prefixes`. | Accepted | `cmd/aira/main.go:885-889` folds repeated `--prefix`; message and test updated (3.4). |
| R2.4 | AIRA-218 is the one removable item; keep it, confirm single decode on the hot path. | Accepted as a build-review item | Already invariant 4; the lenient decode is on the failure path only. |
| R2.5 | `check.go:303` drifted to `:306`. | Moot | The 175 section that cited it is gone. |


### Plan v2 -> v3

Two plan reviews (PASS_WITH_CHANGES, PASS). Each claim was checked against the code at
`9233601`.

| # | Claim | Verdict | Evidence / change |
|---|---|---|---|
| R3.1 (P1) | AIRA-217: an acknowledgement does not establish "next admission sized higher": `RecordConfinePeak` accepts an OOM with a nil peak, admission escalates only from `MaxOOMPeak > 0`. Keeping every golden keeps false claims; test acknowledged-with-unreadable-peak and no-signature. | Accepted | `confine_peak_history.go:109-112` (non-positive peak -> NULL), `admit.go:1393` (`stats.OOMCount > 0 && stats.MaxOOMPeak > 0`); also the clamp/refusal at `admit.go:1430-1496`. "Recorded" now needs acknowledgement AND a known peak; no arm says "sized higher"; the ordinary and unevaluated goldens change; nil-peak and no-signature tests added (3.5, section 5). |
| R3.2 (P2) | AIRA-175: "no live path leaves a requirement `allocated` beside its file" is false; `TestCheckResolvesCrashWindowRequirementAllocation` covers that window. Describe cross-worktree requirement resolution as unevaluated; possible false-fail. | Accepted | `check_requirement_test.go:86`; Rebuild's requirement loop (`store.go:3124-3184`) has no re-point. 3.2 and section 7 corrected; the one-statement fix recorded as a suggested ticket, not built. |
| R3.3 (P2) | AIRA-193: `topFormatMegabytes`'s `(value+mib/2)/mib` overflows near `MaxInt64`. | Accepted | `tui_top.go:176`; overflows for `value > MaxInt64 - 2^19`. Quotient/remainder rounding and boundary tests added (3.7, section 5). |
| R3.4 (info) | AIRA-217: an unconditional hedge without the acknowledgement flag is the from-scratch minimum; keep the flag only if confirmed-record feedback matters. | Partly accepted | The unevaluated arm takes one unconditional wording. The flag is kept for the daemon-reserve arms: it is the signal that keeps the zero-action re-run remedy in the common case (section 2 table). |
| R3.5 (info) | Requirement rendering under AIRA-216 is optional consistency work. | Noted, kept | One call-site change keeps all three front-matter writers on one encoder; its delimiter test is shared with the other two. |
| R3.6 (P3) | AIRA-218 is the one removable item (ticket offers wont-fix; today's message already names the field). | Kept; owner question stands | `ledger.go:392` does print `json: unknown field "x"` today. What the build adds is the unreachable schema arm made reachable and the remedy sentence; the drop remains an owner question. |
| R3.7 (P3) | AIRA-217: the hedged arms should point at `aira confine --budget`, which classifies a recorded OOM under-provisioned on the first occurrence. | Accepted | `skill.go:321` documents exactly that. Every hedged arm and the unevaluated arm name it (3.5). |
| R3.8 (P3) | AIRA-209: the summary line still prints every ~30 s with identical counts (~2,880 lines/day). | Accepted | `discovery.go:80` prints whenever `skipped > 0`, which holds every pass while dead roots are registered. Summary now print-on-change (3.1, invariant 2, tests); deviates from the ticket's "leave it", which predates fast passes. |
| R3.9 (P3) | AIRA-218: a record with no `schema_version` plus an unknown field would report only `schema_version is 0, want 1`, losing the field name; pin it. | Accepted, improved | The lenient-path schema message now appends the strict error, so the field name is kept; test pins the schema-0 case and that the strict-path schema message is unchanged (3.3, section 5). |
| R3.10 (P3) | AIRA-193: a pinned `--memory-reserve 12G` now echoes `12288M`; release notes. | Accepted | Already a test (section 5); now in Risks and release notes. |
| R3.11 (info) | Static-review limits; verified claims (209 re-measured, 244 fixed, 216 safety, hook-test fixtures). | Noted | No change. |
