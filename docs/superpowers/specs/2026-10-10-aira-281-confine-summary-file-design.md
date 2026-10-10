# AIRA-281: `confine --summary-file`: one JSON line per job

Status: PLAN v2 (2026-10-10). v1 had two plan reviews (both PASS_WITH_CHANGES); v2 applies the
accepted findings (review log, section 10) and goes to the Fable plan gate.
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

Almost. The from-scratch minimum is an explicit file flag, an early open of a regular file, one
bounded append, and a JSON projection of the same `runner.ConfineStatus` struct that the trailer
is rendered from (not of the rendered trailer text), plus the few facts the trailer does not
carry and the ticket asks for by name:

- `ran` and the refusal `code`. These are on the separate never-ran line today (AIRA-147).
- `exit`, the job's exit status as the foreground form returns it. Today that is only `$?`.
- "command (or its hash)": an argv hash. And the ticket's optional caller-supplied tree hash.
- wall time and cpu time.

The plan adds, on top of that minimum, only what the ticket's addition asks for by name:

- A `wall=` trailer facet in all modes (addition item 2). It costs two timestamps.
- Three `rusage_*` fields from the `wait4` the supervisor already makes (addition item 1 and the
  shim peak decision). Without them a ci-shim step has no CPU or memory number at all. They are
  new fields with their own names, so they never change the meaning of an existing one (3.3).

The up-front open (3.4) is part of the minimum, not an extra: without it a mistyped directory is
discovered only after a 20-minute job has finished, and its line is then lost.

v1 carried three more things the minimum does not need; v2 removes them (review log): a
`written_at` timestamp, a 4096-byte line cap, and an exhaustive parser-to-help parity test.

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
| A `pass` (or `cacheable`) field computed by AIRA | Judgement. Which outcomes count as a reusable pass is the repo's policy. AIRA records `ran`, `exit` and `terminated_by` and documents the safe predicate (3.2.1). |
| A `written_at` timestamp (v1 had it) | Not asked for. Attempt order is already the line order in a per-run file (3.2.1), and a timestamp makes every record nondeterministic in tests. |
| A 4096-byte line cap (v1 had it) | Machinery with no consumer. `O_APPEND` atomicity on a local regular file does not depend on length, every validated input is bounded, and the only unbounded input (`--slice`) cannot resolve at absurd lengths anyway. The single-`write(2)` rule stays, because a retried short write could interleave with another writer. |
| Generic `--summary-field k=v` (repeatable) | Unbounded schema and unbounded line size. Two requesters asked for exactly one key. |
| Tree hash in the file name only | Works for one hash per run, but not for a per-step input hash, and the line loses its key when files are concatenated. Callers can still do this as well. |
| Wrap each target in GNU `/usr/bin/time` to get rusage | Needs an extra binary in every CI image and produces a second output per step that must be joined to the summary by hand. AIRA already calls `wait4`. |
| `wait4` `ru_maxrss` reported as `peak_rss_bytes` in ci-shim mode | Ruled out by AIRA-121 gate C10. `ru_maxrss` is the largest single process, not the simultaneous total of the process tree, so it under-reads parallel builds. It gets its own explicitly named key instead. |
| `wait4` CPU reported on the trailer's `cpu=` in ci-shim mode | `cpu=` is documented, in the Skill text and the dispatch-table help, as the whole-subtree cgroup counter. If its meaning depended on the mode, one field would mean two things. The rusage CPU gets its own keys. |
| JSON `null` for unevaluated | The ticket and every trailer use the word `unevaluated`, so one vocabulary is kept. Equality checks (`.exit == 0`) are false for both `null` and the string. A numeric comparison needs a type guard either way (3.2.1). |

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
from `parseConfineArgs` for the synchronous refusal, and again from the runner funnel. The funnel
check is unconditional: a request that carries `SummaryTreeHash` without `SummaryFile` is refused
there too, so a programmatic caller gets the same answer as the CLI (the same approach as
`parseConfineJobBound`). All refusals use `E_CONFINE_ARGUMENT_INVALID` (exit 2). No new error
code is added.

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
| `exit` | only when `ran:true` | int | `result.Exit`: the job's exit status as the foreground form returns it (under `--detach` the launcher itself exits 0 on the handle report; this is the job's status from the supervisor). The job's own status passed through, `128+N` if it was killed by a signal, `3` if the wait status could not be decoded (and then `terminated_by` reads `unevaluated`) | never |
| `terminated_by` | only when `ran:true` | string | `status.TerminatedBy` (`normal`, `oom`, `supervisor-signal:<SIG>`, ...) | empty |
| `wall_us` | only when `ran:true` | int | new `status.WallUS`: from the release write to the return of `cmd.Wait()` (3.3); excludes the admission wait and the setup handshake | not established |
| `peak_rss_bytes` | only when `ran:true` | int | `status.PeakRSS` (cgroup `memory.peak` for the whole subtree) | nil; always nil in ci-shim mode, by C10 |
| `cpu_user_us`, `cpu_sys_us` | only when `ran:true` | int | `status.CPUUser` and `CPUSys` (cgroup `cpu.stat` for the whole subtree) | nil; always nil in ci-shim mode |
| `rusage_user_us`, `rusage_sys_us` | only when `ran:true` | int | new `status.RusageUserUS` and `RusageSysUS` from `wait4` | no `ProcessState` |
| `rusage_maxrss_largest_process_bytes` | only when `ran:true` | int | new `status.RusageMaxRSS` = `ru_maxrss * 1024` | no `ProcessState`, or `<= 0` |

A CPU value of 0 (cgroup or rusage) is a real observation and is written as the number `0`.

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

#### 3.2.1 Reading the file (help text and Skill text)

AIRA guarantees one attempted append per job that reaches the funnel, not one valid line. The
reader rules follow from that, and they are written into the help text and the Skill text:

- A missing line means no outcome was recorded. It never means a pass. (A SIGKILLed supervisor,
  a refusal before the funnel (section 7), or a failed write all leave no line.)
- A malformed line is an unknown outcome. A failed write can leave a truncated line with no
  trailing `\n`; the next job's append then lands on the end of it, so one failed write can
  damage the following record too. A reader skips a line that does not parse and treats it as
  unknown, never as a pass.
- Select the current attempt: use one fresh file per CI run (the caller removes or truncates it
  when the run starts; AIRA only appends), so an older attempt's success cannot be in it. A
  consumer that caches passes treats a file containing any malformed line as unable to vouch
  for a pass, because the damaged line could be the current attempt of any step.
- A reusable pass needs `ran == true and exit == 0 and terminated_by == "normal"`. `exit == 0`
  alone is not enough: a job that traps SIGTERM and exits 0 after the supervisor was signalled
  reads `exit:0, terminated_by:"supervisor-signal:SIGTERM"` (the shape pinned by
  `TestConfinePeakReportWithheldWhenSupervisorSignalCaughtAndChildExitsZero`).
- Guard numeric comparisons by type. In jq a string sorts above every number, so
  `.peak_rss_bytes > 1e9` is true for `"unevaluated"`. Write
  `(.peak_rss_bytes|type) == "number" and .peak_rss_bytes > 1e9`.

### 3.3 New measurements on `ConfineStatus`

All new fields are pointers with json tags, because AIRA-22's detached record stores the struct.
Nil means not established.

- `Owner string`. Set at `confine_linux.go:502` beside `Name`, so both the shim and real paths
  carry it.
- `WallUS *int64`. Both launch paths take `releasedAt := time.Now()` immediately after a
  successful release write (`confine_linux.go` about line 1300, `confine_shim_linux.go` about
  line 480). `waitConfineCommand` stamps `WaitReturnedAt` on the statement after `cmd.Wait()`
  returns. Wall time is `WaitReturnedAt.Sub(releasedAt)` on the monotonic clock. A value `<= 0`
  is left nil, matching the `PeakRSS` clamp.

  What `cmd.Wait()` returning means, stated exactly, because it differs by mode. Go's
  `Cmd.Wait` reaps the process and then waits for its own output-copy goroutines. In ci-shim
  mode the job's stdout and stderr are `*os.File` pipe ends (`shimChildStream`), so Go copies
  nothing and `Wait` returns at the reap. On the real path the job's stderr is the
  `confineLockedWriter` wrapper (`confine_linux.go:783`, assigned at `:1198`), so Go copies it
  through a pipe and `Wait` returns only once that copy has drained. A descendant that keeps
  the job's stderr open after the job exits therefore extends `wall_us` on the real path until
  it closes it. The field is named and documented as "release to wait completion", not as the
  reap, and the help text says so. The real-path wait itself is unchanged.
- `RusageUserUS`, `RusageSysUS`, `RusageMaxRSS *int64`. `confineTermination`
  (`confine_linux.go:2431`) gains `WaitReturnedAt time.Time` and `Rusage *syscall.Rusage`.
  `waitConfineCommand` (`confine_linux.go:2636`) fills both on every return arm, including the
  undecoded arm, because `cmd.ProcessState` is still set when `Wait` returns an output-copy
  error. A helper, `applyConfineRusage(&status, term)`, is called from both paths next to the
  existing `TerminatedBy` assignment.
- The trailer gains `wall=<duration>` (or `wall=unevaluated`) on every trailer for a job that
  ran, rendered in `FormatConfineStatus` directly after `cpu=`. The ticket's addition asks for
  this in all modes. The `rusage_*` fields are not rendered on the trailer, which stays focused
  on its existing facets. The never-ran trailer is unchanged.

`readCgroupUsage`, `reportPeak`, `ConfinePeakReport` and the shim's C10 refusal are not touched.

### 3.4 Write semantics

The funnel in `runner.Confine` (`confine.go:915`) is the one path every launch goes through:
real, ci-shim, the non-Linux stub and the detached supervisor. That is why AIRA-147 put the
never-ran trailer there. The new flow:

1. Validate the tree hash unconditionally (3.1). If `request.SummaryFile != ""`, check that the
   path is absolute, then call `openConfineSummaryFile(path)` before calling `confine()`, so
   before admission and before any daemon contact. On any of these failures, set
   `result.Status.Slice` to the attempted slice (`request.Slice`, or `DefaultConfineSlice` when
   empty), exactly as every early abort in `confineWithDeps` does (`confine_linux.go:468-494`),
   and return `E_CONFINE_ARGUMENT_INVALID: --summary-file <path>: <cause>`. The never-ran
   trailer is printed as usual, with the real slice. No job starts and no line can be written.
2. Call `result, err := confineImpl(ctx, request)`. `confineImpl` is a package variable bound to
   `confine`; it is a test seam.
3. If `err != nil`, print the never-ran trailer (unchanged).
4. If a summary fd is held, `line, fmtErr := FormatConfineSummary(request, result, err)`, then
   `appendConfineSummary(fd, line)`, then close the fd. If formatting or writing fails, print
   one stderr line, `confine: summary-file=unwritten path=<p> cause=<...>`. That is a fixed
   token, matchable as a fixed string like `ran=no`. It is a diagnostic, not an error return.
5. Return `result, err` unchanged. The summary never changes the exit code (invariant 4).

`openConfineSummaryFile` lives in the new `confine_summary_linux.go` and returns a raw `int`
fd. It calls `unix.Open(path, O_WRONLY|O_APPEND|O_CREAT|O_NONBLOCK|O_CLOEXEC, 0o666)`, so the
umask applies exactly as for the shell's `>>`, then `unix.Fstat`. A file that is not a regular
file is closed and refused. `O_NONBLOCK` is there so that a FIFO at the path returns at once
(either `ENXIO`, or it opens and is then refused as not regular) instead of blocking the launch
while it waits for a reader; `internal/install/install.go:1996-1999` uses `O_NONBLOCK` for the
same reason. Unlike that call this one has no `O_NOFOLLOW` and does create: symlinks are
followed, as `>>` follows them, because the path is the caller's own. `O_CLOEXEC` keeps the fd
out of the job. The fd is deliberately never wrapped in `os.NewFile` (Go would try to register
a non-blocking fd with its poller), so it is closed with `unix.Close`. The `!linux` stub in
`confine_stub.go` refuses with `E_CONFINE_UNAVAILABLE`, which is the code confine already
returns there.

`appendConfineSummary` makes exactly one `unix.Write(fd, line)` through a package seam,
`writeConfineSummaryFn func(fd int, line []byte) (int, error)`, so tests can count and fail
writes. It never retries: a retried short write could interleave with another writer. A short
write is reported as `summary-file=unwritten` with "short write n of m bytes; the file may now
hold a truncated line that also damages the next appended line" (3.2.1).

Line shape: `FormatConfineSummary` uses `json.Encoder` with `SetEscapeHTML(false)`, which
terminates the line with exactly one `\n` and never emits a raw newline inside a string. There
is no length cap (review log). The legitimate worst case is about 1.3 KiB: name 100, owner 64,
tree hash 128, a 255-byte slice, the fixed vocabularies, and ten numbers of up to 20 digits.

Atomicity claim, stated exactly: on Linux, an `O_APPEND` write to a regular file on a local
filesystem (ext4, xfs, btrfs, tmpfs, overlayfs on top of those) moves the offset and writes the
bytes under the inode lock, so concurrent appenders' lines do not interleave. This is a Linux
property, not a POSIX guarantee for regular files, and it does not hold on NFS (8(a)). It says
nothing about a short write (disk full, file-size limit), which 3.2.1 covers.

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
  returns normally, so the line carries `terminated_by:"supervisor-signal:SIGINT"`, whatever the
  job's exit. A supervisor that is SIGKILLed writes nothing.

### 3.6 Help, Skill, MCP

Generated help comes from the dispatch table, so `internal/core/core.go:1927` (the `confine`
entry) gains:

- `stringSpec("summary_file", ...)` and `stringSpec("summary_tree_hash", ...)`, plus `Usage` and
  the `Run` stub's argument touches. The `summary_file` help text carries the 3.2.1 reader rules
  in short form and the "release to wait completion" meaning of `wall`.
- `boolSpec("fail_fast", ...)` and `[--fail-fast]` in `Usage`. Both are missing today even
  though the parser accepts the flag (found while planning). A one-line help fix, batched here.

`confine` is CLI-only (`MCPTool == ""`, `Include == false`), so no MCP schema changes, and this
plan says so on purpose. `cmd/aira/option_suggest.go`'s `confineLaunchValuedOptions` gains both
options. The pinned Usage string in `cmd/aira/confine_test.go:747` is updated.

The Skill guide (`internal/core/skill.go`, the outcome paragraphs at about lines 328-330) gains:

- Use `--summary-file` in CI instead of parsing the trailer, with the 3.2.1 reader rules (fresh
  file per run, missing or malformed line is unknown, the three-part pass predicate, the jq type
  guard).
- In ci-shim mode `peak_rss_bytes` and `cpu_*_us` are `unevaluated` by design, and the
  `rusage_*` fields are a per-process lower bound.
- The `wall=` facet.
- One sentence on `--fail-fast`, which the Skill text never mentions today: in a CI shim
  cohort, the first failing job aborts its queued siblings (`E_ADMIT_FAILFAST_TRIPPED`) and
  cancels running ones (`terminated-by=failfast-cancelled`); on a real slice it never trips.
  The builder takes the exact wording from the v0.26 help and AIRA-247 spec.

The Skill change needs a separate `aira skill install` at cutover.

### 3.7 Exact change list

| File | Change |
|---|---|
| `internal/runner/confine.go` | `ConfineRequest.SummaryFile`, `SummaryTreeHash`. `ConfineStatus.Owner`, `WallUS`, `RusageUserUS`, `RusageSysUS`, `RusageMaxRSS`. The `Confine` funnel steps from 3.4, including the attempted-slice assignment on a funnel refusal. The `confineImpl` seam. `wall=` in `FormatConfineStatus`. A shared admission-facet helper used by `FormatConfineNeverRan` and the summary. |
| `internal/runner/confine_summary.go` (new, portable) | `ConfineSummarySchema`, `ValidateConfineSummaryTreeHash`, the `confineSummaryRecord` struct, `FormatConfineSummary`, the `unevaluated`-or-value helper. |
| `internal/runner/confine_summary_linux.go` (new) | `openConfineSummaryFile` (raw fd), `appendConfineSummary`, `writeConfineSummaryFn`. |
| `internal/runner/confine_stub.go` | `!linux` stubs for the two functions above. |
| `internal/runner/confine_linux.go` | `confineTermination.WaitReturnedAt` and `Rusage`. `waitConfineCommand` fills them on every arm. `applyConfineRusage`. `releasedAt` and `WallUS` in `confineWithDeps`. `Status.Owner` at line 502. |
| `internal/runner/confine_shim_linux.go` | `releasedAt`, `WallUS` and `applyConfineRusage` beside the `TerminatedBy` assignment. The C10 comment gains a sentence pointing to the separately named `rusage_*` fields. |
| `cmd/aira/main.go` | The `parseConfineArgs` checks (tree hash needs the file; the validator; non-empty values). `filepath.Abs` transcription in `runConfineCommand`. |
| `cmd/aira/option_suggest.go` | Both options. |
| `internal/core/core.go`, `internal/core/skill.go` | 3.6, including the `--fail-fast` spec, Usage token and Skill sentence. |

## 4. Invariants

1. At most one attempted append per funnel call. Every `Confine` call that holds a summary fd
   makes at most one `write(2)`, of one complete line ending in exactly one `\n`, after the
   job's outcome is final. It is never retried. A failed or short write is reported on stderr
   and leaves the outcome unknown (3.2.1); AIRA does not promise a valid line.
2. `ran` is true exactly when `err == nil`. The `ran:false` shape has `code` and none of the
   ran-only keys; the `ran:true` shape has `exit` and `terminated_by` and no `code`.
3. No fabricated zero. Every measured value comes from its source or is the string
   `"unevaluated"`. Nil, a counter that is `<= 0` where 0 is impossible, and an empty string all
   become `"unevaluated"`, never `0` or `""`. An observed CPU 0 stays the number 0.
4. The summary never changes `Confine`'s return value. The exit code is passed through exactly as
   before (AIRA-138 §5.4); a failed write is reported on stderr only.
5. A bad path is refused before admission. An unopenable or non-regular summary path, a relative
   path, or an invalid or orphaned tree hash refuses the launch before `confine()` runs: no
   admission, no child, no daemon contact. The refusal's never-ran trailer names the real slice.
6. `rusage` stays separate. The `rusage_*` values never enter `PeakRSS`, `CPUUser` or `CPUSys`,
   `reportPeak`, or the trailer's `peak-rss=` and `cpu=`.
7. The runner only ever writes to an absolute path, so a detached supervisor writes to the same
   file as the foreground path.
8. Nothing is inherited. The summary path and tree hash are never put in the child's environment.
9. `terminated_by` is written whatever the exit code: an exit of 0 never implies `normal`.

## 5. Tests (TDD; each must fail against the wrong implementation)

All tests carry `verifies: AIRA-281`. Test runs go through `whale-run` / `aira confine`, and the
gate's exact command is `aira confine -- make ci`.

| # | Test (package) | Pins | Mutation that must turn it RED |
|---|---|---|---|
| 1 | Format: never-ran shape (`runner`) | the key set is exactly the always-present keys plus `code`; no `exit`, `terminated_by`, `wall_us` or `peak_rss_bytes` | always emitting `exit` (0) |
| 2 | Format: ran with nil counters | `peak_rss_bytes`, `cpu_*_us`, `rusage_*` and `wall_us` are the string `"unevaluated"`; a CPU pointer to 0 is the number `0` | rendering `*p` or 0; dropping the key when nil (`omitempty`); clamping CPU 0 to `unevaluated` |
| 3 | Format: established values | raw integers (bytes and µs), not formatted text | rendering through `FormatConfineBytes` or `Duration.String` |
| 4 | Format: `argv_sha256` | known vector for `["sh","-c","echo hi"]` (sha256 of the NUL-joined argv) | joining with spaces; hashing the post-injection argv |
| 5 | Format: line shape | max-length name, owner and tree hash, a 255-byte slice and every number at `MaxInt64`, and a name containing `<` and `&`: the line parses, has exactly one `\n` at the end and no other, and `<` is not escaped | `json.Marshal` with a manual `\n` that is then lost; `SetEscapeHTML` left on |
| 6 | `tree_hash` | absent when unset; verbatim when set; validator refuses empty, 129 bytes, a space, `"` and `\n` | `omitempty` removed; alphabet check removed |
| 7 | Funnel: up-front refusal (`confineImpl` seam) | a missing directory, a directory path, a FIFO with no reader (run under a 2 s test timeout) and a FIFO with a reader are all refused with `E_CONFINE_ARGUMENT_INVALID`; `confineImpl` is never called; the never-ran trailer reads `slice=<the requested slice>`, not `slice=unevaluated` | opening at the end (the seam gets called); dropping `O_NONBLOCK` (the test times out); dropping the regular-file check (the FIFO with a reader is accepted); omitting the attempted-slice assignment |
| 8 | Funnel: relative path; orphaned tree hash | a relative `SummaryFile` is refused; a request with `SummaryTreeHash` and no `SummaryFile` is refused; `confineImpl` is not called in either case | accepting a relative path; gating the tree-hash check on `SummaryFile != ""` |
| 9 | Funnel: one write | `writeConfineSummaryFn` called exactly once with the whole line, on the fd `openConfineSummaryFile` returned | writing the body and `\n` separately; a retry loop after a short write |
| 10 | Funnel: write failure | the seam returns `EIO`, or a short write of n < m; result and error are unchanged (`Exit` 0 stays 0, `err` stays nil); stderr has `summary-file=unwritten`; on the short write the seam is still called exactly once | propagating the error; changing `Exit`; retrying the remainder |
| 11 | Funnel: ran and never-ran via the seam | a canned `(result, nil)` gives `ran:true` with `exit`; a canned `(status, E_ADMIT_FAILFAST_TRIPPED)` gives `ran:false` with that code and `admission:"failfast_tripped"` | deriving `ran` from `Exit` |
| 12 | Shim, real launch (`confine_shim_linux_test.go` harness, whose `reportPeak` is a panic seam) | a child that burns about 200 ms of CPU and touches 64 MiB gives `rusage_user_us` of at least 100000, `rusage_maxrss_largest_process_bytes` of at least 64 MiB, `wall_us` above 0, `peak_rss_bytes` and `cpu_user_us` `"unevaluated"`, and the advisory `containment`; the run does not panic | forgetting `*1024` (KiB); copying rusage into `PeakRSS` or `CPUUser`; `RUSAGE_SELF` of the supervisor |
| 13 | Real path wall clock (`confine_linux_test.go` deps seam) | (a) the `deps.admit` seam sleeps 1 s and the target is `true`: `WallUS` is under 1 s. (b) the target is `sleep 0.3`: `WallUS` is at least 300 ms. (c) the target backgrounds `sh -c 'sleep 0.3' >&2` holding stderr and exits at once: `WallUS` is at least 300 ms, pinning the documented "release to wait completion" meaning on the real path | starting the clock before admission (or at invocation); stamping before `cmd.Wait()` returns (arm c); the release stamp moved after the wait |
| 14 | Trailer `wall=` | nil gives `wall=unevaluated`; 1.5 s gives `wall=1.5s`, placed after `cpu=` | omitting the facet when nil |
| 15 | Output-copy error (`waitConfineCommand`, real `exec.Cmd`) | a command `sh -c 'echo x >&2; exit 0'` whose `Stderr` is a writer that returns an error: `Wait` returns the copy error with `ProcessState` set; the function returns exit 3, `Decoded:false`, and a non-nil `Rusage`; the summary from that termination has `exit:3, terminated_by:"unevaluated"` and numeric `rusage_*` | `Rusage` filled only on the decoded arms |
| 16 | CLI parse (`cmd/aira`) | `--summary-tree-hash` without `--summary-file` is refused; an empty `--summary-file` is refused; `--summary-file` with `--list` is refused | removing the requires-check |
| 17 | CLI transcription (`runConfined` seam) | `--summary-file rel.jsonl` becomes `filepath.Join(cwd, "rel.jsonl")` | passing it through raw |
| 18 | Help (`cmd/aira`) | the confine dispatch Args contain `summary_file`, `summary_tree_hash` and `fail_fast`; the pinned Usage string (`confine_test.go:747`) contains `--summary-file`, `--summary-tree-hash` and `--fail-fast` | removing any of the three specs or Usage tokens (`fail_fast` is RED today) |
| 19 | Detached end to end (`confine_detach_linux_test.go` harness) | a detached job with a summary file: no line before it finishes, exactly one `ran:true` line after; a detached launch with an unwritable path exits 2 synchronously with no record of a running job | the supervisor resolving a relative path against its own cwd; opening after `BeforeAdmit` |
| 20 | Trapped SIGTERM (`confine_linux_test.go`, the deps-seam shape of `TestConfinePeakReportWithheldWhenSupervisorSignalCaughtAndChildExitsZero`) | with a summary file, a child that traps SIGTERM and exits 0 after the supervisor is signalled writes `ran:true, exit:0, terminated_by:"supervisor-signal:SIGTERM"` | rendering `terminated_by` as `normal` when `exit == 0`; omitting `terminated_by` when `exit == 0` |
| 21 | Stress (evidence, not a mutation guard) | 32 helper processes each append 200 lines of about 1.3 KiB (the worst-case record size) to one file; every line parses and the count is 6400 | n/a (probabilistic; test 9 is the deterministic guard) |

## 6. Expected yield

- Unblocks field/Stoner and spice/flavour Makefile CI. Each step writes one machine-readable
  line, and the `sed` on the trailer goes away.
- ci-shim steps go from zero per-step numbers to wall time, waited-tree CPU and largest-process
  RSS, all clearly labelled. Cgroup-mode steps gain wall time.
- A ready primitive for AIRA-282's macro (`--name`, `--memory-reserve`, `--require-admission`,
  `--summary-file`, optional `--summary-tree-hash`), and for repo-side resumable gates keyed on
  `(name, argv_sha256, tree_hash)` with a pass of `ran && exit == 0 && terminated_by ==
  "normal"`, read from a fresh per-run file (3.2.1). AIRA replays nothing and computes no pass.
- A latent documentation gap is fixed: `--fail-fast` is missing from the generated help, the
  Usage string and the Skill text; 3.6 adds all three.

## 7. Deferred (written down on purpose)

| Deferred | Why |
|---|---|
| `admission_waited_us` | `AdmissionWaitedMS` is a non-pointer `int64`, so 0 cannot be told apart from "not measured". Reporting it honestly needs an "established" bit. Not asked for. |
| `scope_id`, `signature`, `cap`, deadline and exclusive facets | Not asked for. The trailer still carries them. Add keys under schema 1 when someone needs them. |
| A `written_at` timestamp | Not asked for (section 2). Add under schema 1 if a consumer needs it. |
| An exhaustive parser-to-help parity test over `confineLaunchOptionNames()` | It would have caught the `fail_fast` gap, but it is separable from this ticket. Test 18 pins the three names this change touches. A candidate small follow-up. |
| A tree-total memory peak without cgroups (polling `/proc` and summing RSS) | Sampling machinery, and it misses short peaks. Cap sizing comes from a cgroup-mode run (field's decision). |
| Feeding `wait4` samples to the estimator | Rejected permanently (AIRA-121 C10). |
| Lines for refusals before the funnel | The CLI's own argument refusals (bad `--memory-max`, owner, VRAM) happen before a request exists, and so do detached-supervisor failures before `Confine` (control file, record store). Those steps exit non-zero, and a missing line never reads as a pass. |
| NFS, FUSE, FIFO or `/dev/stderr` targets | Regular files on a local filesystem only (3.4). |
| `waitConfineCommand` discarding a real `ProcessState` exit when `Wait` returns an output-copy error (it reports exit 3) | Existing behaviour seen while planning. v1 said it could not happen from the CLI; that was wrong. On the real path the job's stderr is the `confineLockedWriter` wrapper, so Go copies it through a pipe, and a write error on the supervisor's own stderr (a full disk behind `2>file`, for example) surfaces as a copy error. The summary reports it honestly (`exit:3`, `terminated_by:"unevaluated"`, test 15), so this change does not make it worse. Fixing the exit-code loss is its own ticket, not done here. |
| AIRA-282 macro | A separate ticket, held until this ships. Notes for it: omit `--summary-tree-hash` rather than pass an empty one; create a fresh summary file per run. |

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
- (e) A short write (disk full, `RLIMIT_FSIZE`) leaves a truncated line, and the next append
  concatenates onto it, so one failure can make two records unreadable. Accepted: it is
  reported on the failing job's stderr, and the reader rule treats both as unknown (3.2.1).
  Writing a repair `\n` would be a second, unordered write and is not done.
- (f) `wall_us` means "release to wait completion", which on the real path includes draining
  the job's stderr (3.3). A descendant holding stderr open lengthens it. Documented, pinned by
  test 13(c); in ci-shim mode it is the reap.
- (g) A jq reader without the type guard can flag `"unevaluated"` as exceeding a numeric
  limit. That is a visible false positive (the row is shown, not dropped), and the help and
  Skill text give the guard.
- (h) A SIGKILLed supervisor writes no line. That is the stated reader rule (3.2.1).

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

This is by design (`confine_shim_linux.go:73`). The shim budget is the ledger's number, not a
cap the kernel enforces on the job, and printing it as `cap=` would claim a limit that nothing
enforces. It is unchanged here.

## 10. Review log

Plan v1 had two reviews, both PASS_WITH_CHANGES. Each finding was checked against the code
before it was applied.

| # | Finding | Checked against | Decision |
|---|---|---|---|
| R1.1 | `ran && exit == 0` accepts a job that traps SIGTERM and exits 0; consumers must select the current attempt | `confine_linux_test.go:1392` (child traps TERM, `Exit` 0, `terminated-by=supervisor-signal:SIGTERM`) | Accepted. The documented pass predicate adds `terminated_by == "normal"` (3.2.1, section 6); invariant 9; test 20. Current-attempt rule: one fresh file per run, malformed lines void a pass (3.2.1). AIRA still computes no pass field (judgement). |
| R1.2 | `cmd.Wait()` returns after Go's output-copy goroutines, not at the reap; v1's claim that copy errors cannot happen from the CLI is false | `confine_linux.go:783` (stderr wrapped in `confineLockedWriter`), `:1198` (assigned to `cmd.Stderr`); `shimChildStream` passes `*os.File` in ci-shim mode | Accepted. `ReapedAt` renamed `WaitReturnedAt`; `wall_us` documented as release to wait completion, with the per-mode difference stated (3.3, 8(f)); test 13(c) pins it; test 15 now drives a real erroring stderr writer; the section 7 row is corrected. |
| R1.3 | A short write without `\n` damages the next record too; do not promise one valid line | Linux `O_APPEND` semantics | Accepted. Invariant 1 now promises one attempted append; reader rule for malformed lines (3.2.1); risk 8(e). |
| R1.4 | Scope can shrink: cache hashes, `written_at`, trailer changes, exhaustive parity are separable | The ticket text (`.aira/tickets/AIRA-281.md`) | Partly accepted. Removed `written_at` and the exhaustive parity test (test 18 narrowed; parity deferred). Kept `argv_sha256` (ticket: "command (or its hash)"), `tree_hash` (ticket: "optional caller-supplied tree hash") and `wall=` (ticket addition item 2), because the ticket asks for them by name. |
| R1.5 | Keep nil as `unevaluated` and observed CPU 0 as 0; document numeric type guards | jq ordering (strings sort above numbers) | Accepted. Type-guard rule in 3.2.1 and the help/Skill text; CPU 0 arm added to test 2; risk 8(g). |
| R1.6 | Other claims verified | n/a | No change. |
| R2.1 | Section 6 claims the `--fail-fast` Skill gap is fixed but nothing schedules it | `grep fail-fast internal/core/skill.go` and `core.go`: no hits | Accepted. 3.6 adds the Skill sentence and the Usage token; 3.7 lists them. |
| R2.2 | The 4096-byte line cap has no consumer | 3.4 atomicity argument; inputs are bounded | Accepted. Cap, its test arm and v1 risk (e) removed; the single-`write(2)`, no-retry rule kept. |
| R2.3 | Tree-hash validation in the funnel is gated on `SummaryFile != ""` | v1 3.1 vs 3.4 step 1 | Accepted. The funnel validates the tree hash unconditionally; a hash without a file is refused (3.1, 3.4, test 8). |
| R2.4 | A funnel refusal prints `slice=unevaluated` although the slice is known | `confine_linux.go:468-494` set `result.Status.Slice = attemptedSlice` on each early abort | Accepted. Funnel step 1 does the same; test 7 pins it. |
| R2.5 | `exit` wording is wrong under `--detach` | `confine_detach_linux.go:763-764` | Accepted. Reworded to the job's status as the foreground form returns it. |
| R2.6 | Line references drift | `confine_shim_linux.go:73` (cap comment), `skill.go:328-330`, `internal/install/install.go:1996-1999` (`O_RDONLY|O_NONBLOCK|O_NOFOLLOW`) | Accepted. References corrected; 3.4 now says only `O_NONBLOCK` is shared with install, and why this open follows symlinks and creates. |
| R2.7 | Use the raw fd from `unix.Open` with one `unix.Write`, not `os.NewFile` | Go's poller registration of non-blocking fds | Accepted. 3.4 holds a raw fd; the seam takes an `int` fd. |
