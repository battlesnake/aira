# AIRA-281: `confine --summary-file`: one JSON line per job

Status: PLAN v1 (2026-10-10), for plan review (Sol `high`, Gemini) and the Fable plan gate.
Ticket: AIRA-281 (P2, milestone v0.29). It blocks AIRA-282 (the shared `aira.mk` confine-step
macro, held until this ships). Requested by field/Stoner and spice/flavour, whose Makefile CI is
blocked on it. Client-side only: no daemon change, no wire change, protocol stays 14.

## 1. Problem, in plain terms

A CI Makefile runs many steps under `aira confine`, often with `make -j`. For each step it wants
to record what happened (did it run, its exit code, how it ended, how much memory and CPU it
used, how long it took) in a form a script can read. Today the only per-step record is the
trailer, one stderr text line of `key=value` facets. Makefiles must capture stderr and `sed` it,
the trailer is interleaved with the job's own stderr, a job that was refused before running
prints a different line (`ran=no ...`), and the exit code and wall time are not on the trailer
at all. `--json` is refused on the foreground launch because stdout belongs to the job.

In ci-shim mode (`aira install --ci=shim`, used inside CI containers) it is worse: there is no
cgroup, so the trailer reports `peak-rss=unevaluated cpu=unevaluated`, and `confine --dump`
returns no history (see 9.1). A CI run therefore has no per-step numbers at all.

The ask: `aira confine --summary-file <path>` appends one JSON line per job to `<path>`, safely
when many jobs share the file, including a line for a job that was refused and never ran. A
value AIRA could not measure is reported as `unevaluated`, never as 0.

## 2. Greenfield minimum

The challenge question was: is the minimum just a JSON rendering of the existing trailer facets?

Almost. The minimum is a JSON projection of the same `runner.ConfineStatus` struct that the
trailer is rendered from, not of the rendered trailer text, plus the few facts the trailer does
not carry:

- `ran` and the refusal `code`. These are on the separate never-ran line today (AIRA-147).
- `exit`, the exit status `aira confine` returns. Today that is only `$?`.
- The job identity a cache needs: an argv hash and an optional caller-supplied tree hash.

The plan adds two measurements on top of the minimum. Each has a specific justification:

- Wall time (`wall_us`, plus a `wall=` trailer facet). Both requesters asked for it, it is valid
  in every mode, and it costs two timestamps.
- Three `rusage_*` fields from the `wait4` the supervisor already makes. Without them a ci-shim
  step has no CPU or memory number at all, which is the spice ask. They are new fields with
  their own names, so they never change the meaning of an existing one (see 3.3).

The up-front open (3.4) is also beyond the bare minimum. Without it, a mistyped directory is
discovered only after a 20-minute job has finished, and its line is then lost. The open is one
call, and it also removes the path race at write time.

Everything else is left out on purpose (section 7).

### Rejected alternatives

| Alternative | Why rejected |
|---|---|
| Foreground `--json` on stdout or stderr | stdout belongs to the job. On stderr the JSON interleaves with the job's own stderr under `make -j`. A file is the only channel that is per-step and append-safe. |
| Parse the rendered trailer back into JSON | Lossy: bytes are rendered as `4G` and CPU as a duration string. The trailer is a projection, so project the struct again instead. |
| Marshal `ConfineStatus` as it is (it already has json tags) | `omitempty` turns an unevaluated nil into a missing key, which is the silence the trailer discipline exists to prevent. It exposes internal fields (escape evidence, nanosecond durations), it has no `ran`, `exit` or `code`, and it would tie the CI schema to internal refactors. |
| Environment variable `AIRA_CONFINE_SUMMARY_FILE` | Every nested `aira confine` would inherit it and append lines for inner jobs under the outer job's file. The channel is implicit. The flag is explicit, and AIRA-282's macro supplies it. |
| Open the file by path at the end of the job | A bad path is found only after the job has run. The up-front open refuses before admission and holds the fd, so a rename of the directory during the job cannot misdirect the line. |
| The daemon writes the line | The daemon never touches caller paths (the `confine --dump` precedent). It also adds a wire change and a daemon dependency for a client-side fact. |
| `flock` around the write | Not needed. One `write(2)` on an `O_APPEND` fd to a regular file on a local Linux filesystem does not interleave with other writers. A lock adds blocking and behaves inconsistently on NFS. |
| AIRA computes the tree hash (`git write-tree`) | That is judgement (which tree? dirty files? untracked files?) plus a git dependency. AIRA stays a primitive: the caller supplies it and AIRA treats it as opaque. |
| Generic `--summary-field k=v` (repeatable) | Unbounded schema and unbounded line size. Two requesters asked for exactly one key. |
| Tree hash in the file name only | Works for one hash per run, but not for a per-step input hash, and the line loses its key when files are concatenated. Callers can still do this as well. |
| Wrap each target in GNU `/usr/bin/time` to get rusage | Needs an extra binary in every CI image and produces a second output per step that must be joined to the summary by hand. AIRA already calls `wait4`. |
| `wait4` `ru_maxrss` reported as `peak_rss_bytes` in ci-shim mode | Ruled out by AIRA-121 gate C10. `ru_maxrss` is the largest single process, not the simultaneous total of the process tree, so it under-reads parallel builds. It gets its own explicitly named key instead. |
| `wait4` CPU reported on the trailer's `cpu=` in ci-shim mode | `cpu=` is documented, in the Skill text and the dispatch-table help, as the whole-subtree cgroup counter. If its meaning depended on the mode, one field would mean two things. The rusage CPU gets its own keys. |
| JSON `null` for unevaluated | The ticket and every trailer use the word `unevaluated`, so one vocabulary is kept. Equality checks (`.exit == 0`) are false for both `null` and the string. A numeric `>` in jq selects the string, which is a visible false positive, whereas `null` would silently drop the row. |

## 3. Design

### 3.1 CLI surface

Two new valued launch options. Both are refused in the management form (`--list`, `--kill` and
the others) by `parseConfineManagementArgs`, which already rejects unknown options.

- `--summary-file <path>`. A value is required, and an empty string is refused. The CLI makes
  the path absolute with `filepath.Abs` when it builds the request (`runConfineCommand`), so a
  detached supervisor and the foreground path name the same file. The runner refuses a relative
  path (`E_CONFINE_ARGUMENT_INVALID`), so the absolute-path rule is enforced in the runner and
  does not depend on the CLI remembering it.
- `--summary-tree-hash <value>`. Requires `--summary-file`; without it the option is refused,
  the same pattern as `--stdin-connect` requiring `--detach`. The value is opaque: AIRA copies it
  verbatim and never computes or checks it. Rules: non-empty; at most 128 bytes; only letters,
  digits and `._:+/=-`. That covers hex, base64, base64url and `sha256:<hex>`, and no allowed
  character needs JSON escaping. An empty value is refused rather than treated as absent: a
  macro that wants the hash to be optional omits the flag (noted for AIRA-282).

Validation lives in one exported function, `runner.ValidateConfineSummaryTreeHash`. It is called
from `parseConfineArgs` for the synchronous refusal and again from the runner funnel, so the two
checks cannot drift apart (the same approach as `parseConfineJobBound`). All refusals use
`E_CONFINE_ARGUMENT_INVALID` (exit 2). No new error code is added.

There is no environment-variable form, for the reason given in section 2.

### 3.2 The record: schema 1

There is one JSON object per line, terminated by exactly one `\n`, with the keys in the fixed
order of the struct. Every measured value is either its established type or the exact string
`"unevaluated"`. A key that cannot apply at all (nothing ran, or no tree hash was supplied) is
omitted. This follows the trailer's existing rule: `container=` is absent unless a container was
detected, and `cap-source=` is absent unless a cap was enforced.

| Key | Present | Type | Source | `"unevaluated"` when |
|---|---|---|---|---|
| `schema` | always | int `1` | constant `runner.ConfineSummarySchema` | never |
| `written_at` | always | RFC 3339 UTC string with nanoseconds | the funnel's clock at write time | never |
| `ran` | always | bool | `err == nil` from `confine()` (the AIRA-147 invariant: an error return means the target never executed) | never |
| `name` | always | string | `status.Name` (normalised, set after validation) | refused before identity validation |
| `owner` | always | string | new `status.Owner`, set beside `Name` | the same |
| `argv_sha256` | always | 64 lowercase hex | sha256 of `request.Argv` joined by NUL, the caller's argv before any container injection | never |
| `tree_hash` | only if supplied | string | `request.SummaryTreeHash`, verbatim | not applicable |
| `slice` | always | string | `status.Slice` | empty |
| `containment` | always | string | `status.Containment` (`enforced` or `advisory(ci-shim,no-cgroup,no-kill-backstop)`) | empty |
| `admission` | always | string | `AdmissionState`, falling back to `Admission` (the same derivation as `FormatConfineNeverRan`, through one shared helper) | both empty |
| `reserve_bytes` | always | int | `status.ReserveBytes` | `<= 0` |
| `code` | only when `ran:false` | string | `confineErrorCode(err)` | the error carries no code |
| `exit` | only when `ran:true` | int | `result.Exit`, which is what `aira confine` exits with: the job's own status passed through, `128+N` if it was killed by a signal, `3` if the wait status could not be decoded (and then `terminated_by` reads `unevaluated`) | never |
| `terminated_by` | only when `ran:true` | string | `status.TerminatedBy` | empty |
| `wall_us` | only when `ran:true` | int | new `status.WallUS`: from the release write to the reap (the same start as `--timeout`; it excludes the admission wait and the setup handshake) | not established |
| `peak_rss_bytes` | only when `ran:true` | int | `status.PeakRSS` (cgroup `memory.peak` for the whole subtree) | nil; always nil in ci-shim mode, by C10 |
| `cpu_user_us`, `cpu_sys_us` | only when `ran:true` | int | `status.CPUUser` and `CPUSys` (cgroup `cpu.stat` for the whole subtree) | nil; always nil in ci-shim mode |
| `rusage_user_us`, `rusage_sys_us` | only when `ran:true` | int | new `status.RusageUserUS` and `RusageSysUS` from `wait4` | no `ProcessState` |
| `rusage_maxrss_largest_process_bytes` | only when `ran:true` | int | new `status.RusageMaxRSS` = `ru_maxrss * 1024` | no `ProcessState`, or `<= 0` |

Schema rule: consumers ignore unknown keys. Adding a key does not change `schema`. Removing a
key, renaming it or changing what it means does bump `schema`.

What the `rusage_*` fields mean (also written into the help text and the Skill text): they
cover the job's process and every descendant that some process waited for. Descendants that
were orphaned or daemonised are missing, so the CPU figures are a lower bound. `maxrss` is the
peak of the single largest of those processes, not the tree total. Both include the AIRA setup
shim that `exec`s the target, which sets a floor of a few MiB and a few ms. The builder
measures that floor and records it in the PR. These fields are populated in both modes, since
the same `wait4` happens in both, so the keys never depend on the mode. They never reach the
reserve estimator (invariant 6).

### 3.3 New measurements on `ConfineStatus`

All new fields are pointers with json tags, because AIRA-22's detached record stores the struct.
Nil means not established.

- `Owner string`. Set at `confine_linux.go:502` beside `Name`, so both the shim and real paths
  carry it.
- `WallUS *int64`. Both launch paths take `releasedAt := time.Now()` immediately after a
  successful release write (`confine_linux.go` about line 1300, `confine_shim_linux.go` about
  line 480). `waitConfineCommand` stamps `ReapedAt` as soon as `cmd.Wait()` returns. Wall time is
  `ReapedAt.Sub(releasedAt)` on the monotonic clock. A value `<= 0` is left nil, matching the
  `PeakRSS` clamp.
- `RusageUserUS`, `RusageSysUS`, `RusageMaxRSS *int64`. `confineTermination`
  (`confine_linux.go:2431`) gains `ReapedAt time.Time` and `Rusage *syscall.Rusage`.
  `waitConfineCommand` fills both on every return arm, including the undecoded arm, because
  `cmd.ProcessState` can still be set there. A helper, `applyConfineRusage(&status, term)`, is
  called from both paths next to the existing `TerminatedBy` assignment. A CPU value of 0 is kept
  as a real observation, as with cgroup CPU.
- The trailer gains `wall=<duration>` (or `wall=unevaluated`) on every trailer for a job that
  ran, rendered in `FormatConfineStatus` directly after `cpu=`. The ticket's addition asks for
  this in all modes. The `rusage_*` fields are not rendered on the trailer, which stays focused
  on its existing facets. The never-ran trailer is unchanged.

`readCgroupUsage`, `reportPeak`, `ConfinePeakReport` and the shim's C10 refusal are not touched.

### 3.4 Write semantics

The funnel in `runner.Confine` (`confine.go:915`) is the one path every launch goes through:
real, ci-shim, the non-Linux stub and the detached supervisor. That is why AIRA-147 put the
never-ran trailer there. The new flow:

1. If `request.SummaryFile != ""`, check that the path is absolute and validate the tree hash.
   Then call `openConfineSummaryFile(path)` before calling `confine()`, so before admission and
   before any daemon contact. On failure, return `E_CONFINE_ARGUMENT_INVALID: --summary-file
   <path>: <cause>`. The never-ran trailer is printed as usual. No job starts and no line can be
   written.
2. Call `result, err := confineImpl(ctx, request)`. `confineImpl` is a package variable bound to
   `confine`; it is a test seam.
3. If `err != nil`, print the never-ran trailer (unchanged).
4. If a summary fd is held, `line, fmtErr := FormatConfineSummary(request, result, err,
   time.Now())`, then `appendConfineSummary(f, line)`, then close the fd. If formatting or
   writing fails, print one stderr line, `confine: summary-file=unwritten path=<p> cause=<...>`.
   That is a fixed token, matchable as a fixed string like `ran=no`. It is a diagnostic, not an
   error return.
5. Return `result, err` unchanged. The summary never changes the exit code (invariant 4).

`openConfineSummaryFile` lives in the new `confine_summary_linux.go`. It calls
`unix.Open(path, O_WRONLY|O_APPEND|O_CREAT|O_NONBLOCK|O_CLOEXEC, 0o666)`, so the umask applies
exactly as for the shell's `>>`, then `fstat`. A file that is not a regular file is closed and
refused. `O_NONBLOCK` is there so that a FIFO at the path returns at once (either `ENXIO`, or it
opens and is then refused as not regular) instead of blocking the launch while it waits for a
reader. `install.go:1996` uses the same approach. Symlinks are followed, as `>>` follows them,
because the path is the caller's own. `O_CLOEXEC` keeps the fd out of the job. The `!linux` stub
in `confine_stub.go` refuses with `E_CONFINE_UNAVAILABLE`, which is the code confine already
returns there.

`appendConfineSummary` makes exactly one `write(2)` through `f.SyscallConn()`, deliberately not
`os.File.Write`, which loops after a short write and so issues a second syscall. A short write is
reported as `summary-file=unwritten` with "short write n of m bytes; the file may now hold a
truncated line". The write goes through a package seam,
`writeConfineSummaryFn func(fd uintptr, line []byte) (int, error)`, so tests can count and fail
writes.

Line size: `FormatConfineSummary` uses `json.Encoder` with `SetEscapeHTML(false)`, which
terminates the line with exactly one `\n` and never emits a raw newline inside a string. If the
line is longer than `confineSummaryMaxLine = 4096` it is refused (`summary-file=unwritten`,
"line exceeds 4096 bytes") rather than written. The worst case for legitimate inputs is about
1.3 KiB: name 100, owner 64, tree hash 128, a 255-byte slice, the fixed vocabularies, and ten
numbers of up to 20 digits. Only an arbitrarily long `--slice` on a never-ran line can exceed the cap. That
is accepted and written down (8(e)).

Atomicity claim, stated exactly: on Linux, an `O_APPEND` write to a regular file on a local
filesystem (ext4, xfs, btrfs, tmpfs, overlayfs on top of those) moves the offset and writes the
bytes under the inode lock, so concurrent appenders' lines do not interleave. This is a Linux
property, not a POSIX guarantee for regular files, and it does not hold on NFS (8(a)).

### 3.5 Interplay

- `--detach`: allowed. `SummaryFile` and `SummaryTreeHash` are exported `ConfineRequest` fields,
  so they travel in the control file. The supervisor calls `Confine`, which opens the file
  before `BeforeAdmit`. An unwritable path therefore reaches the launcher through the ready pipe
  with its own code, and the launcher exits 2 synchronously, exactly as the foreground form
  does. The line is written by the supervisor when the job ends, and any `summary-file=unwritten`
  diagnostic goes to the job's captured stderr (`aira confine-log --stream err`). A reader of the
  file right after `--detach` returns sees no line yet. That follows from `--detach` and is
  documented in the help text.
- `--fail-fast`: the trigger writes `ran:true` with its own non-zero exit. A running victim
  writes `ran:true, terminated_by:"failfast-cancelled"`. A sibling refused by the tripped latch
  writes `ran:false, code:"E_ADMIT_FAILFAST_TRIPPED", admission:"failfast_tripped"`
  (`admission_linux.go:503`). The summary is written in the funnel after `maybeSendFailfastTrip`
  has run inside `confineShim`, and the ordering does not matter.
- `--require-admission` refusals, saturation, `E_ADMIT_TOO_LARGE`, the VRAM refusals and every
  other error that reaches the funnel write a `ran:false` line with their code.
- Nested confines: there is no inheritance (no environment variable). An inner `aira confine`
  writes a line only if its own flag asks for one.
- Supervisor interrupted (Ctrl-C to the `make` process group): the supervisor tears down and
  returns normally, so the line carries `terminated_by:"supervisor-signal:SIGINT"`. A supervisor
  that is SIGKILLed writes nothing.

Rule for readers, stated in the help text: a missing line means no outcome was recorded. It never
means a pass.

### 3.6 Help, Skill, MCP

Generated help comes from the dispatch table, so `internal/core/core.go:1927` (the `confine`
entry) gains:

- `stringSpec("summary_file", ...)` and `stringSpec("summary_tree_hash", ...)`, plus `Usage` and
  the `Run` stub's argument touches.
- `boolSpec("fail_fast", ...)`. It is missing today even though the parser accepts it (found
  while planning). It is fixed in the same change because the new parity test below would
  otherwise fail.

`confine` is CLI-only (`MCPTool == ""`, `Include == false`), so no MCP schema changes, and this
plan says so on purpose. `cmd/aira/option_suggest.go`'s `confineLaunchValuedOptions` gains both
options. The pinned Usage string in `cmd/aira/confine_test.go:747` is updated. The Skill guide
paragraph (`internal/core/skill.go:320`) gains two sentences: use `--summary-file` in CI instead
of parsing the trailer; in ci-shim mode `peak_rss`/`cpu` are `unevaluated` by design, and the
`rusage_*` fields are a per-process lower bound. It also mentions the `wall=` facet. The Skill
change needs a separate `aira skill install` at cutover.

### 3.7 Exact change list

| File | Change |
|---|---|
| `internal/runner/confine.go` | `ConfineRequest.SummaryFile`, `SummaryTreeHash`. `ConfineStatus.Owner`, `WallUS`, `RusageUserUS`, `RusageSysUS`, `RusageMaxRSS`. The `Confine` funnel steps from 3.4. The `confineImpl` seam. `wall=` in `FormatConfineStatus`. A shared admission-facet helper used by `FormatConfineNeverRan` and the summary. |
| `internal/runner/confine_summary.go` (new, portable) | `ConfineSummarySchema`, `confineSummaryMaxLine`, `ValidateConfineSummaryTreeHash`, the `confineSummaryRecord` struct, `FormatConfineSummary`, the `unevaluated`-or-value helper. |
| `internal/runner/confine_summary_linux.go` (new) | `openConfineSummaryFile`, `appendConfineSummary`, `writeConfineSummaryFn`. |
| `internal/runner/confine_stub.go` | `!linux` stubs for the two functions above. |
| `internal/runner/confine_linux.go` | `confineTermination.ReapedAt` and `Rusage`. `waitConfineCommand` fills them. `applyConfineRusage`. `releasedAt` and `WallUS` in `confineWithDeps`. `Status.Owner` at line 502. |
| `internal/runner/confine_shim_linux.go` | `releasedAt`, `WallUS` and `applyConfineRusage` beside the `TerminatedBy` assignment. The C10 comment gains a sentence pointing to the separately named `rusage_*` fields. |
| `cmd/aira/main.go` | The `parseConfineArgs` checks (tree hash needs the file; the validator; non-empty values). `filepath.Abs` transcription in `runConfineCommand`. |
| `cmd/aira/option_suggest.go` | Both options. |
| `internal/core/core.go`, `internal/core/skill.go` | 3.6. |

## 4. Invariants

1. One line per funnel call. Every `Confine` call that holds a summary fd makes at most one
   `write(2)`, of one complete line of at most 4096 bytes ending in `\n`, and makes it after the
   job's outcome is final.
2. `ran` is true exactly when `err == nil`. The `ran:false` shape has `code` and none of the
   ran-only keys; the `ran:true` shape has `exit` and no `code`.
3. No fabricated zero. Every measured value comes from its source or is the string
   `"unevaluated"`. Nil, a counter that is `<= 0` where 0 is impossible, and an empty string all
   become `"unevaluated"`, never `0` or `""`.
4. The summary never changes `Confine`'s return value. The exit code is passed through exactly as
   before (AIRA-138 §5.4); a failed write is reported on stderr only.
5. A bad path is refused before admission. An unopenable or non-regular summary path refuses the
   launch before `confine()` runs: no admission, no child, no daemon contact.
6. `rusage` stays separate. The `rusage_*` values never enter `PeakRSS`, `CPUUser` or `CPUSys`,
   `reportPeak`, or the trailer's `peak-rss=` and `cpu=`.
7. The runner only ever writes to an absolute path, so a detached supervisor writes to the same
   file as the foreground path.
8. Nothing is inherited. The summary path and tree hash are never put in the child's environment.
9. Parser and dispatch table agree. Every launch option the parser accepts appears in the
   dispatch-table Args and the Usage string, and the reverse.

## 5. Tests (TDD; each must fail against the wrong implementation)

All tests carry `verifies: AIRA-281`. Test runs go through `whale-run` / `aira confine`, and the
gate's exact command is `aira confine -- make ci`.

| # | Test (package) | Pins | Mutation that must turn it RED |
|---|---|---|---|
| 1 | Format: never-ran shape (`runner`) | the key set is exactly the always-present keys plus `code`; no `exit`, `wall_us` or `peak_rss_bytes` | always emitting `exit` (0) |
| 2 | Format: ran with nil counters | `peak_rss_bytes`, `cpu_*_us`, `rusage_*` and `wall_us` are the string `"unevaluated"` | rendering `*p` or 0; dropping the key when nil (`omitempty`) |
| 3 | Format: established values | raw integers (bytes and µs), not formatted text | rendering through `FormatConfineBytes` or `Duration.String` |
| 4 | Format: `argv_sha256` | known vector for `["sh","-c","echo hi"]` (sha256 of the NUL-joined argv) | joining with spaces; hashing the post-injection argv |
| 5 | Format: worst case | max-length name, owner and tree hash, a 255-byte slice and every number at `MaxInt64` gives a line of 4096 bytes or less, with exactly one `\n`, at the end; an oversized slice is refused, not written | dropping the cap check; using `json.Marshal` with a manual `\n` that is then lost |
| 6 | `tree_hash` | absent when unset; verbatim when set; validator refuses empty, 129 bytes, a space, `"` and `\n` | `omitempty` removed; alphabet check removed |
| 7 | Funnel: up-front refusal (`confineImpl` seam) | a missing directory, a directory path, a FIFO with no reader (run under a 2 s test timeout) and a FIFO with a reader are all refused with `E_CONFINE_ARGUMENT_INVALID`; `confineImpl` is never called | opening at the end (the seam gets called); dropping `O_NONBLOCK` (the test times out); dropping the regular-file check (the FIFO with a reader is accepted) |
| 8 | Funnel: relative path | refused; `confineImpl` not called | accepting a relative path |
| 9 | Funnel: one write | `writeConfineSummaryFn` called exactly once with the whole line | writing the body and `\n` separately; `os.File.Write` with a retry loop |
| 10 | Funnel: write failure | the seam returns `EIO` or a short write; result and error are unchanged (`Exit` 0 stays 0, `err` stays nil); stderr has `summary-file=unwritten` | propagating the error; changing `Exit` |
| 11 | Funnel: ran and never-ran via the seam | a canned `(result, nil)` gives `ran:true` with `exit`; a canned `(status, E_ADMIT_FAILFAST_TRIPPED)` gives `ran:false` with that code and `admission:"failfast_tripped"` | deriving `ran` from `Exit` |
| 12 | Shim, real launch (`confine_shim_linux_test.go` harness, whose `reportPeak` is a panic seam) | a child that burns about 200 ms of CPU and touches 64 MiB gives `rusage_user_us` of at least 100000, `rusage_maxrss_largest_process_bytes` of at least 64 MiB, `wall_us` above 0, `peak_rss_bytes` and `cpu_user_us` `"unevaluated"`, and the advisory `containment`; the run does not panic | forgetting `*1024` (KiB); copying rusage into `PeakRSS` or `CPUUser`; `RUSAGE_SELF` of the supervisor |
| 13 | Real path wall clock (`confine_linux_test.go` deps seam) | the `deps.admit` seam sleeps 1 s and the target is `true`: `WallUS` is under 1 s; the target is `sleep 0.3`: `WallUS` is at least 300 ms | starting the clock before admission (or at invocation). Stamping the reap late is prevented by construction instead: `ReapedAt` is set inside `waitConfineCommand` on the statement after `cmd.Wait()`, and test 15 reads it from there. |
| 14 | Trailer `wall=` | nil gives `wall=unevaluated`; 1.5 s gives `wall=1.5s`, placed after `cpu=` | omitting the facet when nil |
| 15 | Undecoded wait | `waitConfineCommand` on a command whose `Wait` returns a non-`ExitError` still fills `Rusage` when `ProcessState` is set, and returns exit 3; the summary has `exit:3, terminated_by:"unevaluated"` | `Rusage` filled only on the decoded arms |
| 16 | CLI parse (`cmd/aira`) | `--summary-tree-hash` without `--summary-file` is refused; an empty `--summary-file` is refused; `--summary-file` with `--list` is refused | removing the requires-check |
| 17 | CLI transcription (`runConfined` seam) | `--summary-file rel.jsonl` becomes `filepath.Join(cwd, "rel.jsonl")` | passing it through raw |
| 18 | Parity (`cmd/aira`) | every name in `confineLaunchOptionNames()` appears in the dispatch Args (dashes as underscores) and in `Usage`, and the reverse (excluding `argv`) | removing any spec, including `fail_fast`, which is RED today |
| 19 | Detached end to end (`confine_detach_linux_test.go` harness) | a detached job with a summary file: no line before it finishes, exactly one `ran:true` line after; a detached launch with an unwritable path exits 2 synchronously with no record of a running job | the supervisor resolving a relative path against its own cwd; opening after `BeforeAdmit` |
| 20 | Stress (evidence, not a mutation guard) | 32 helper processes each append 200 lines of about 3.5 KiB to one file; every line parses and the count is 6400 | n/a (probabilistic; test 9 is the deterministic guard) |

## 6. Expected yield

- Unblocks field/Stoner and spice/flavour Makefile CI. Each step writes one machine-readable
  line, and the `sed` on the trailer goes away.
- ci-shim steps go from zero per-step numbers to wall time, waited-tree CPU and largest-process
  RSS, all clearly labelled. Cgroup-mode steps gain wall time.
- A ready primitive for AIRA-282's macro (`--name`, `--memory-reserve`, `--require-admission`,
  `--summary-file`, optional `--summary-tree-hash`), and for repo-side resumable gates keyed on
  `(name, argv_sha256, tree_hash)` with a pass of `ran && exit == 0`. AIRA replays nothing.
- A latent documentation gap is fixed: `--fail-fast` is missing from the generated help and the
  Skill text.

## 7. Deferred (written down on purpose)

| Deferred | Why |
|---|---|
| `admission_waited_us` | `AdmissionWaitedMS` is a non-pointer `int64`, so 0 cannot be told apart from "not measured". Reporting it honestly needs an "established" bit. Not asked for. |
| `scope_id`, `signature`, `cap`, deadline and exclusive facets | Not asked for. The trailer still carries them. Add keys under schema 1 when someone needs them. |
| A tree-total memory peak without cgroups (polling `/proc` and summing RSS) | Sampling machinery, and it misses short peaks. Cap sizing comes from a cgroup-mode run (field's decision). |
| Feeding `wait4` samples to the estimator | Rejected permanently (AIRA-121 C10). |
| Lines for refusals before the funnel | The CLI's own argument refusals (bad `--memory-max`, owner, VRAM) happen before a request exists, and so do detached-supervisor failures before `Confine` (control file, record store). Those steps exit non-zero, and a missing line never reads as a pass. |
| NFS, FUSE, FIFO or `/dev/stderr` targets | Regular files on a local filesystem only (3.4). |
| `waitConfineCommand` discarding a real `ProcessState` exit when `Wait` returns an output-copy error (it reports exit 3) | Existing behaviour seen while planning. It cannot happen from the CLI, where stdout is an `*os.File`. To be filed as its own ticket, not fixed here. |
| AIRA-282 macro | A separate ticket, held until this ships. |

## 8. Risks and known limits

- (a) Atomicity is a property of Linux on local filesystems. On NFS, concurrent appends from
  different hosts can interleave. Documented in the help text. Not detected (a `statfs` magic
  check is machinery nobody has asked for).
- (b) The `wall=` facet changes every trailer for a job that ran. Tests that pin full trailer
  strings need updating, and external parsers keyed on the trailer may care. The release notes
  for deploy and speed list the new facet. The release is a client drop-in, with protocol 14
  unchanged.
- (c) `rusage_*` could be misread as tree totals. The key names, help text and Skill text all
  say what they are, and they never appear on the trailer or in the estimator. The setup-shim
  floor is measured and recorded.
- (d) The fd is held for the job's whole life, including the admission wait: one fd per waiting
  job. If the file is unlinked or rotated mid-job, the line goes to the old inode and is lost.
  This is documented.
- (e) An absurdly long `--slice` on a never-ran launch can exceed the 4096-byte cap. The line is
  then refused with `summary-file=unwritten`, not truncated.
- (f) A SIGKILLed supervisor writes no line. That is the stated reader rule (3.5).

## 9. Answered questions

### 9.1 Is the ci-shim `--dump` writing 0 records the same defect as AIRA-280?

No. AIRA-280 is a transport failure (`E_DAEMON_UNAVAILABLE: EOF`, exit 4) on the box daemon.
spice's container dump succeeded and was empty, which is expected:

- `Admissions` rows are the `confine_peak_history` samples (`internal/daemon/ci_dump.go`), and
  ci-shim mode never reports a peak, by C10.
- `Waiters` only lists admissions still live when the dump is taken, and there are none once the
  steps have finished.

The per-step record CI needs is this ticket's summary line, not the dump. (Assumption, not
verified: spice's dump exited 0. If it did not, it is AIRA-280's defect and that ticket should
note it.)

### 9.2 Why the ci-shim trailer reports `cap=unevaluated`

This is by design (`confine_shim_linux.go:70`). The shim budget is the ledger's number, not a
cap the kernel enforces on the job, and printing it as `cap=` would claim a limit that nothing
enforces. It is unchanged here.
