# AIRA v0.32 small-bugs batch: discovery log flood, check, ledger diagnosis, init, advisories, text

Status: PLAN v1 (2026-10-10). Batch of ready bug/chore tickets in one change (owner rule: batch
small independent fixes, one review and test cycle). Base: master `d13ef3b` (v0.31 + ticket
close), protocol 15.

In this batch: AIRA-209, AIRA-175, AIRA-218, AIRA-188, AIRA-217, AIRA-216, AIRA-193,
AIRA-227, AIRA-219, AIRA-231, AIRA-263, AIRA-255. Dropped: AIRA-244 (already fixed).
All thirteen candidates report `ready:true` in `aira ready <id>` (checked 2026-10-10).

**Correctness-sensitive items, two-loop adversarial build review: AIRA-209 (daemon discovery
loop), AIRA-175 (a check verdict can move from fail to pass), AIRA-218 (ledger read
classification), AIRA-188 (init refusal and prefix claiming).** The rest are message, text or
telemetry fixes.

## 1. Problem, in plain terms, and what still reproduces

Every ticket was re-checked against `d13ef3b`. Ticket line numbers are stale; current ones are
used below.

| Ticket | Plain problem | Still reproduces? | Evidence (2026-10-10) | Verdict |
|---|---|---|---|---|
| AIRA-209 | The daemon re-tries every registry entry whose worktree was deleted, every pass, forking git three times and sleeping 75 ms each time, and logs one line each time. | Yes, measured, worse than filed | `journalctl --user -u aira-daemon.service --since -10min`: 5,480 lines, 5,471 (99.8 %) are `registry discovery skip ... E_NOT_PROJECT`. One pass = 1,363 skips over 194 distinct (worktree, root) keys; **all 194 roots are absent on disk** (`[ -e ]` false for every one). A pass now takes about 120 s (about 88 ms per entry), so the loop runs nearly back to back: about 4,100 git forks per pass. `registry.jsonl` is 2,247 records (AIRA-88's premise was 131). Code: `internal/daemon/discovery.go:46-50` (no stat, no latch), `internal/app/project.go:158` -> `gitValue` retries. | Build |
| AIRA-175 | `aira check` decides "this allocated ID has a ticket file" by looking only at the absolute path recorded for the allocation, which points into whichever worktree a rebuild scanned last. When that worktree is deleted, the check fails although the file is in the checkout being checked. | Filed symptoms: no. Mechanism: yes, by code (latent) | Live `aira check` on the root checkout: `ticket-file-integrity: pass` (the AIRA-117 sorted-labels flag is gone); `allocated-id-file: fail` with only AIRA-198 and AIRA-240, which have **no file on any branch** (`git log --all -- .aira/tickets/AIRA-{198,240}.md` empty): true positives. But the rows for AIRA-56/140/150 now point at `/home/mark/claude/aira-worktrees/aira-tickets-sf/...` (read-only sqlite query), a live sibling worktree; the rebuild re-point (`internal/store/store.go:3110`) moves an `allocated` row to the last scanned worktree. `check.go:303` `os.Lstat(path)` is the only lookup, so deleting `aira-tickets-sf` turns all three into `E_ID_UNRESOLVED` while the root checkout holds the files. | Build, narrowed to the path lookup |
| AIRA-218 | A ledger record carrying a field this build does not know (another aira build wrote it) is reported as corruption, and the schema-version check that exists for that case can never run because the strict decode fails first. | Yes, by code | `internal/runner/ledger.go:309` `DisallowUnknownFields`; `ledgerRecordDefect` (`:389`) tests `decodeErr` before `SchemaVersion`. `ledgerSchema` is still `1` (`:25`). AIRA-172 already degrades `check`/`reconcile` to `unevaluated`, so only the diagnosis remains. | Build (diagnosis only; see 3.3) |
| AIRA-188 | `aira init` on a new project with no `--prefixes` silently claims the prefix `AIRA`, which is exclusive machine-wide and already owned by this repo, so it fails everywhere else with a confusing ownership conflict. | Yes, by code | `internal/app/project.go:473-476`: `if len(prefixes) == 0 { prefixes = []string{"AIRA"} }`. `docs/project-lifecycle.md:5` says prefixes are optional; `README.md:24` shows bare `aira init`. | Build |
| AIRA-217 | After an OOM at an AIRA-estimated cap, confine says "The kill is now recorded ... re-run and the next admission is sized higher", even when the report to the daemon failed and nothing was recorded. | Yes, by code | `internal/runner/confine_linux.go:1431` `_ = deps.reportPeak(...)` discards the error; `formatConfineReserveAdvisory` (`:1690`) has no recorded parameter; the claim is in both daemon-reserve arms (`:1747`, `:1768`) and the unevaluated arm's "re-running ... admits at a higher reserve" (`:1773`). | Build |
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
  print a skip line only when that entry's reason changes.
- **175:** when the recorded path of an allocation is gone, look for the same entity file in
  the checkout being checked before calling the allocation unresolved.
- **218:** when a strict decode fails, decode once more without the unknown-field rule; if
  that succeeds, the record is well-formed JSON with a field this build does not know: say
  so (and check its schema version first).
- **188:** a new project with no prefixes is refused, naming `--prefixes`.
- **217:** keep the result of the peak report and choose the sentence from it.
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
| 209 | Also make the aggregate "discovered/registered/skipped" line transition-only | The ticket asks to leave it; it is the one per-pass heartbeat. Its cadence rises from one per ~150 s to one per 30 s (2 lines/min), stated under Risks. |
| 175 | Heal the allocation row (re-point it) inside `check` | `check` is read-only; writing derived state from a report verb is wrong, and `--rebuild` already re-points. |
| 175 | Bound `check` to the invoking worktree only (the ticket's "second finding") | Changes what `check` covers across all dimensions; the ticket calls it an owner decision. Section 7. |
| 175 | Delete stale `tickets` rows in Rebuild (the 2026-09-09 amendment) | A separate data-model change to the rebuild; no live finding needs it today. Section 7. |
| 218 | A new stable code (e.g. `E_LEDGER_SHAPE_UNRECOGNISED`) | Exit class stays 4 either way (the ledger could not be read), so a code buys only a word, at the cost of a catalogue entry and three degrade sites (`core.go:2241`, `:2295`, `check.go:683`). The owner's standing simplicity rule ("keep the primitive, document the gap") and the ticket's own wont-fix note point the same way. Recorded as an open question for the owner. |
| 218 | Prefix-match `json: unknown field` in the error text | Fragile against Go's wording; the second lenient decode is the structural test. |
| 218 | Two decodes for every record | The ledger is read on every run and reconcile. The second decode runs only on the failure path. |
| 218 | Bump `ledgerSchema` now and add a struct-fingerprint test | A bump makes every older build that is still installed somewhere report the shared ledger as corrupt, for a shape that is compatible. With the lenient re-decode, an additive field is already named precisely without a bump. The doc comment says when a bump is right (an incompatible change). |
| 188 | Default to a prefix derived from the slug or directory | Still a silent claim of a machine-wide exclusive name the caller did not choose, and directory names collide. |
| 188 | Telemetry-only mode with zero prefixes (`--tickets none`) | A new project mode read by several verbs; belongs with AIRA-190. Section 7. |
| 217 | Retry, spool, or widen the 250 ms report deadline | The ticket rules these out; the fix is to stop asserting what did not happen. |
| 217 | A `peak-report=recorded\|failed` trailer facet | Optional in the ticket; changes the trailer peers parse. Section 7. |
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
- The aggregate line (`discovery.go:80-82`) is unchanged.
- Fixture change in `TestDiscoverRegistryPassSkipsBadEntriesAndGuardsIdentity`
  (`discovery_test.go:141-142`): `/reused` becomes a real directory under `t.TempDir()` so the
  identity-mismatch branch is still reached through the hook (otherwise the stat would skip it
  and the test would pass vacuously); `/missing` becomes an absent path under `t.TempDir()`.
  The test also asserts the hook was called for the reused root.

Effect on the box: 1,363 skip lines per pass become 194 lines once per daemon start; git forks
for absent roots drop from about 4,100 per pass to 0; a pass takes milliseconds.

### 3.2 AIRA-175: an allocation is materialised if its file is in this checkout

`internal/store/check.go`, the allocated-id-file loop (`:262-340`):

- After the kind/path agreement check (`:286-293`) and before the kind branch, resolve the
  path to inspect: if `os.Lstat(recorded)` fails with `os.ErrNotExist`, use
  `s.entityPathForKind(normaliseKind(kind), id)` (`allocation_kind.go:35`, the checking
  worktree's own `.aira/tickets/<ID>.md` or `.aira/requirements/<ID>.md`) when it differs
  from the recorded path. Any other `Lstat` error keeps today's hard error.
- Everything downstream is unchanged and runs on the resolved path: the requirement branch
  (`checkAllocatedRequirementFile`), `Lstat`, `readRegularTicket`, the parse, and the
  `ticket.ID != id` comparison. So the fallback passes only when the own-tree file exists,
  parses, and carries exactly this ID; otherwise the finding is the same `E_ID_UNRESOLVED`
  (or parse finding) as today.
- No write, no row change. `--rebuild`'s re-point (`store.go:3110`) is untouched.

### 3.3 AIRA-218: say what an unknown ledger field is; make the schema arm reachable

`internal/runner/ledger.go`, `(*ledger).read` (`:307-315`) and `ledgerRecordDefect`:

- Hot path unchanged: one strict decode (`DisallowUnknownFields`) per record.
- Only when the strict decode fails: decode the same payload again into a fresh
  `ledgerEvent` without `DisallowUnknownFields`.
  - lenient decode also fails -> today's message, `payload did not decode as a ledger event:
    <strict error>` (genuine malformation);
  - lenient succeeds and `SchemaVersion != ledgerSchema` -> `schema_version is X, want Y`
    (the arm is now reachable for a record that also carries new fields);
  - lenient succeeds with the expected schema -> `record carries a field this aira build does
    not recognise (<strict error, which names the field>); its checksum verified, so the
    bytes are intact: it was written by an aira build with a different ledger record shape;
    run one aira build on this machine (reinstall)`. Never "newer": an unknown field does not
    establish that.
- Code stays `E_JOURNAL_CORRUPT`, exit 4, so the AIRA-172 degrade sites are untouched and the
  accept/reject decision is bit-for-bit unchanged (only the message differs). The identity
  salvage (`ledgerRecordIdentity`) uses the lenient-decoded event when the lenient decode
  succeeded, so run id and sequence are reported in full rather than "partially decoded".
- `ledgerSchema` doc comment: bump only for an incompatible record change (a field whose
  meaning changes, or one removed); an additive `omitempty` field needs no bump, because an
  older build names the field it does not know. A bump makes every existing record
  unreadable to the new build unless the reader is taught the old schema, so it is never
  done casually.
- Update `ledgerRecordDefect`'s comment (`:378-388`), which currently says a newer binary's
  field "reads as `json: unknown field`".

### 3.4 AIRA-188: a new project must name its prefixes

`internal/app/project.go`, `PrepareInit`, non-adopt branch (`:473-476`): replace the default
with a refusal, before any file is written or any prefix claimed:

`E_CONFIG_INVALID: a new project needs at least one ID prefix: pass --prefixes <P,...> (for
example --prefixes DEMO). Prefixes are exclusive across every project on this machine, so
AIRA does not choose one for you.`

- Code `E_CONFIG_INVALID` (exit 2): the code `validateConfig` already uses for the same
  condition (`config has no prefixes`, `:586-588`); one code for one fact.
- The adopt path (a committed `.aira/config`) is unchanged: its prefixes come from the file.
- Both faces reach it: the CLI and MCP `init` go through the daemon's `server.go:1189`
  `app.PrepareInit` or `dispatcher.go:916` `app.Init`.
- Docs: `README.md:24` becomes `aira init --prefixes FOO   # adopt this repo (FOO: your ID
  prefix)`; `docs/project-lifecycle.md:5-9` says the prefix is required for a new project and
  explains why; the help example at `internal/core/core.go:2439` uses `DEMO` instead of
  `AIRA` (this machine's AIRA prefix is owned).

### 3.5 AIRA-217: the OOM advisory says whether the kill was recorded

`internal/runner/confine_linux.go`:

- `peakRecorded := false`; inside the existing `if signature != "" &&
  shouldRecordConfinePeak(...)` block, `peakRecorded = deps.reportPeak(...) == nil`. A report
  that was never attempted is not recorded.
- `formatConfineReserveAdvisory` gains `peakRecorded bool`. With `true`, every output is
  byte-identical to today. With `false`:
  - daemon-reserve, ordinary arm: `... The kill could NOT be recorded against this command's
    signature (the report to the daemon failed), so the next admission is NOT sized from this
    kill: pin --memory-reserve <suggested> to size the re-run yourself.` No "genuine bug worth
    reporting" clause (a dropped report is exactly what makes a repeat kill).
  - daemon-reserve, over-ceiling arm (`suggested >= sliceCapBytes`): `... so no pin is
    offered. The kill could NOT be recorded against this command's signature, so the next
    admission is NOT sized from this kill: split heavy work, or run where the slice is
    larger.` It never names a pin (the AIRA-184 BLOCK) and makes no claim about what admission
    will do with a re-run (the client cannot establish it; same bound as AIRA-184).
  - unevaluated cap source: the clause "if AIRA estimated it, re-running the identical
    command admits at a higher reserve" becomes "if AIRA estimated it, the kill could not be
    recorded, so a re-run is NOT sized from this kill". (Departure from the ticket's scope
    note, which predates the current wording: this arm now asserts the recording's
    consequence.)
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
  `FormatConfineMegabytes(value int64) string` (unchanged rule: nearest MiB, `"0M"` for
  `value <= 0`); `aira top`'s call sites in `cmd/aira/tui_top.go` call it; the cmd/aira copy
  and its test move to runner. One implementation for both faces.
- Every byte figure on the admission-wait line uses it: the head reserve and declared VRAM
  (`confine_linux.go:714-721`), the MemAvailable note (`confine_queue_position_linux.go:193`),
  `queued ahead` (`:209`; the known-zero literal becomes `"0M"`), `already granted` and
  `slice ceiling` in `confineHeldNote` (`:269, 279`), and both figures in
  `confineOwnReserveNote` (`:322-326`). Existing guards (`> 0`, `heldEstablished`,
  `ceilingBytes > 0`) are unchanged, so an unestablished figure is still omitted, never
  printed as `0M`.
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
  again. A nested \`aira confine\` is a separate admission request that must fit beside every
  reservation already granted on the slice, its own parent's included, and admission waits
  with no time limit; it prints a warning saying so.`
- `cmd/aira/confine_nested.go`, `nestedConfineWarning`: add `It must fit beside every
  reservation already granted on the slice, including the parent's own, and an admission wait
  has no time limit (it ends on a grant or Ctrl-C), so if it cannot fit next to its parent
  even on an otherwise idle slice it waits indefinitely.` The exemptions (empty parent id,
  `--exclusive`) are unchanged.

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
   `skipped` count still counts every skipped entry every pass.
3. (175) `allocated-id-file` passes for an allocation only if a file at the recorded path, or
   (only when the recorded path is absent) at this checkout's own entity path, parses and
   carries exactly that ID. `check` writes nothing.
4. (218) The set of ledger records accepted is unchanged; only the message of a rejection
   changes. A strictly valid record is decoded once.
5. (218) A ledger message never claims the writer was newer, and never calls bytes corrupt
   that passed the checksum and decode as JSON.
6. (188) A fresh init with no prefixes writes no file and claims no prefix.
7. (217) The OOM advisory asserts that a kill was recorded only when `reportPeak` returned
   nil for this run, and the over-ceiling arm never names a pin.
8. (216) Rendered front matter contains no `\u003c`, `\u003e` or `\u0026`, and a render
   followed by a parse round-trips.
9. (193) Every byte figure on the admission-wait line is in whole MiB; the trailer and every
   other surface keep `FormatConfineBytes`. An unestablished figure is omitted, never `0M`.
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
- Mutations: log every pass (first test RED); stat after the hook (second RED); key on
  worktree id only (two-roots RED); never delete the key on success (transition test RED);
  latch an absent root (the transition test's last step never reaches the hook, RED).

**AIRA-175** (`internal/store`, a check fixture with an allocation row whose recorded path is
in a removed directory):
- own checkout holds a valid ticket file for the ID -> `allocated-id-file: pass`, no
  `E_ID_UNRESOLVED` (RED today);
- own checkout lacks the file -> `fail`, `E_ID_UNRESOLVED` (false-pass twin);
- own checkout's file carries a different ID -> `fail`, `allocation file contains <X>`;
- own checkout's file is unparseable -> the same finding as today for an unparseable file;
- requirement kind: the same four cases through `checkAllocatedRequirementFile`;
- recorded path present (another live worktree) -> resolved there, as today.
- Mutations: accept the fallback on existence alone (different-ID case RED); fall back on any
  `Lstat` error (a fixture with an unreadable parent directory keeps today's hard error);
  fall back for tickets only (requirement case RED).

**AIRA-218** (`internal/runner/ledger_corrupt_diagnostics_test.go`, synthetic ledgers in
`t.TempDir()`):
- schema 1 record plus an unknown field -> `E_JOURNAL_CORRUPT`, message names the field and
  says "different ledger record shape" and "checksum verified", does not contain "did not
  decode" or "newer" (RED today);
- schema 2 record plus an unknown field -> `schema_version is 2, want 1` (RED today: "did not
  decode");
- malformed JSON with a valid checksum -> `payload did not decode` (unchanged);
- the other four defects keep their messages; valid ledgers still read (existing tests).
- `internal/store/check_run_ledger_test.go`: a ledger with an unknown field grades
  `run-ledger: unevaluated` and every other dimension exactly as a healthy ledger
  (regression guard; passes today).
- Mutations: drop the lenient re-decode (first two RED); check the schema after the
  unknown-field branch (second RED); classify with the lenient decode on the hot path and
  accept unknown fields (a strict-reject test RED). Coverage gap, accepted: that the lenient
  decode runs only on the failure path is a code-reading item for the build review.

**AIRA-188** (`internal/app/project_test.go`, `internal/daemon/adopt_test.go`):
- fresh git repo, `Init` with no prefixes -> error code `E_CONFIG_INVALID`, message contains
  `--prefixes`, no `.aira/config` written (RED today: succeeds with AIRA);
- daemon face: `server.bootstrap` on a fresh repo with no prefixes -> refused and no
  `prefix_ownership` row for `AIRA` (RED today);
- adopt path with a committed config and no `--prefixes` -> succeeds (existing tests).
- Mutations: restore the default (both RED); refuse in the adopt path too (adopt tests RED).

**AIRA-217** (`internal/runner`):
- `formatConfineReserveAdvisory` table: every existing golden gains `peakRecorded: true` and
  is byte-identical; new `false` rows for the daemon-reserve ordinary arm, the over-ceiling
  arm and the unevaluated arm: full expected strings; each asserts no "The kill is now
  recorded" and no "genuine bug worth reporting"; the over-ceiling `false` row asserts no
  `--memory-reserve` pin.
- Plumbing, through `confineWithDeps`: `deps.reportPeak` returns an error, usage reports an
  own-cap OOM at a daemon-reserve cap -> the printed advisory carries the not-recorded text
  (RED against a build that ignores the result).
- Mutations: `peakRecorded = true` unconditionally (plumbing RED); invert the flag (goldens
  RED); name a pin in the over-ceiling `false` arm (RED).

**AIRA-216** (`internal/domain`):
- rendered bytes for a ticket titled `planned -> done & <id> handling`: no `\u003c`,
  `\u003e`, `\u0026`; the literal title is present; `ParseTicket` round-trips (RED today on
  the bytes); the same for `RenderFinding` with `WaiverReason` `<a> & <b>`.
- Mutations: `SetEscapeHTML(true)` (bytes RED); omit the trailing-newline trim (round-trip
  RED: a blank line before `---`).

**AIRA-193** (`internal/runner`, `cmd/aira`):
- `FormatConfineMegabytes`: zero, negative, exact, round-down, round-up, large (the moved
  `aira top` test).
- queue line: held 54194584616 B over 5 jobs, ceiling 52608 MiB, ahead 5769372876 B ->
  contains `5502M queued ahead, 51684M already granted across 5 admitted jobs / 52608M slice
  ceiling` (RED today); own-reserve over-ceiling note in M; head reserve `12288M` for a 12 GiB
  reserve.
- unestablished held pair or ceiling -> the clause is omitted, as today.
- trailer unchanged: an existing `FormatConfineStatus` golden with `cap=enforced(64G)` still
  passes.
- `aira top` render test (`TestTopRuntimeRendersAndQuits`) still shows `65536M`/`28672M`.
- Mutations: leave one figure on `FormatConfineBytes` (RED); change `FormatConfineBytes`
  instead (trailer golden RED).

**AIRA-227** (`cmd/aira/dispatcher_inprocess_test.go`, `internal/install`):
- exchange returns `Proto: daemon.ProtocolVersion+1, Code: E_DAEMON_PROTOCOL` -> response code
  and exit unchanged, error contains the test binary's `os.Executable()` path, `protocol
  <ProtocolVersion>` and `Restart this process` (RED today);
- client-newer case: the existing replacement test passes unchanged;
- root install with no `SUDO_USER` -> error contains `--ci=shim` (RED today).
- Mutations: drop the suffix (first case RED); drop the `--ci=shim` door (install case RED).

**AIRA-219** (`cmd/aira/confine_nested_test.go`, `internal/core/skill_test.go`):
- `nestedConfineWarning("...", "<parent>")` contains `parent's own` and `no time limit`
  (RED today); exemption tests unchanged;
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
- `allocated-id-file` stops flipping to `fail` whenever the worktree a rebuild last scanned is
  deleted, so its two genuine findings (AIRA-198, AIRA-240) stay visible.
- A ledger written by a different aira build is diagnosed in one read, with the remedy.
- `aira init` can no longer claim `AIRA` by accident on another repository.
- An OOM advisory no longer sends an operator to re-run a job that will be killed again and
  then report it as an AIRA bug.
- Status moves stop rewriting title bytes; a stale MCP server and a root container install
  each name their fix; the guide and the warning agree on nested confine; aitest's pool
  budget reaches `confine-report`; the aitest docs match the code.

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| AIRA-244 | Fixed in `9e210d7` (v0.10). Close as done. |
| AIRA-175: bound `check` to the invoking worktree; Rebuild deleting stale `tickets` rows | Owner decision and a data-model change respectively (the ticket says so). |
| `check`'s 7 `E_RELATION_INDEX_DIVERGENCE` and 2 `E_FINDING_INDEX_DIVERGENCE` on the root checkout today | Not triaged; they may be a stale index or another false-positive class. Suggest a ticket. |
| AIRA-209: `registry.jsonl` growth (2,247 records; AIRA-88 decided append-only on a 131-record premise) | AIRA-88's decision stands until the owner revisits it; open question. |
| AIRA-218: a distinct code; the ~13 replay-integrity `E_JOURNAL_CORRUPT` sites without record coordinates (`ledger.go:211-225`, `:461-495`, `internal/store/gate_audit.go:69-83`) | Owner question / coverage gap recorded (RANT-22 recurs only on a replay failure). |
| AIRA-188: telemetry-only projects (zero prefixes, a named mode) and the enable-tickets transition hazard | A new project mode read by several verbs; belongs with AIRA-190. |
| AIRA-217: `peak-report=recorded\|failed` trailer facet | Optional; changes a parsed surface. |
| AIRA-216: `internal/gate/gate.go:225` gate files escape `&`/`<`/`>` too | Gate files are digest-bound in attestations; needs its own review. Suggest a ticket. |
| AIRA-193: `E_ADMIT_SATURATED` terminal message and `confine --list` figures | Different surfaces, not asked for. |
| AIRA-219: the ticket's `--admit-timeout` remedy and "name the budget" | Moot since S13 (no admission timeout exists). |
| AIRA-227: confine/worker-admit skew messages | Already carry a remedy (`worker_admit_client_linux.go:431-436`). |

## 8. Risks

- **Discovery (209).** A root that exists but is not a project still forks git every pass
  (rare; logged once). The aggregate line's cadence rises to one per 30 s (2 lines/min)
  because passes become fast. On each daemon start the 194 dead keys log once (a burst, then
  silence). Review: the map is read and written under `s.mu` only.
- **Check verdict (175).** The change can move `allocated-id-file` from fail to pass. The
  fallback still requires a parsed file with exactly the ID, so it cannot pass an allocation
  with no file; reviewers should probe for a false pass (symlinks: the own-tree path goes
  through the same `Lstat`/`readRegularTicket` rules as the recorded path).
- **Init (188).** Any script running bare `aira init` on a new repository now fails with exit
  2 and the remedy. No compat obligation (owner rule); the README and lifecycle doc are
  updated. Repositories with a committed config are unaffected.
- **Byte change (216).** The first write to each of the 15 escaped ticket files changes its
  title bytes once (an expected one-time diff). `aira import --tickets` re-run over a ticket
  whose title holds `<`, `>` or `&` reports `refreshed`, not `unchanged`, once. fastest.ee's
  backlog import is the one peer that re-imports; mention it in the release notes.
- **Wait-line format (193).** Peers that parse the admission-wait line see `M` figures
  instead of raw bytes; it is a human progress line, not a contract (the trailer is
  unchanged).
- **Message text (217, 218, 219, 227).** Codes and exits unchanged; tests pinning exact text
  are updated, not added beside.
- **Daemon restart.** 209, 175, 188 and 216 run inside the daemon; `aira install` restarts it
  when the binary changed (AIRA-203, v0.31). Skill reinstall needed for 219.

## 9. Build order and release notes

Build in this order (independent; correctness-sensitive first so the two-loop review has the
most time): 209, 175, 218, 188, 217, 216, 193, 227, 219, 231, 263, 255. Proto unchanged
(15). Daemon restart via `aira install`; `aira skill install ~/.claude/skills/aira --force`
for the skill text; the embedded aitest pylib changes (231, 263, 255) ship with the binary.
Close AIRA-244 as done. Notify deploy and speed per the release rule; tell deploy that the
`@aira_cpu` text changed (AIRA-263; their `pytest.ini` copy) and that bare `aira init` on a
new repository is now refused.
