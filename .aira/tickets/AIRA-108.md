---
{"schema":1,"id":"AIRA-108","project":"aira","title":"confine-reserve --pinned --max-wait not honoured: 52min hang past a 300s declared bound, 51.6GB free","status":"in-review","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["admission","confine","dogfood"],"hold":false,"relations":[]}
---
Reported by peer session `split` (fastest-ee, `make test-engine`), 2026-09-06.

## The incident

`make test-engine` (`uv run pytest -q -p aitest -n 0 --aitest-workers=auto`) stalled at 99% progress, log idle for 740s. The stuck process, confirmed still running via `ps`:

```
aira confine-reserve --bytes 1047035904 --pinned --signature pytest:tools/correctness/test_dispatch_coverage.py::test_board_temp_range_has_a_reader_and_is_not_on_the_allowlist --slice aira.slice --max-wait 300s
```

**The anomaly, precisely stated:** this process had been alive for ~3118s (52 minutes) at the time split observed it — over **10x** its own declared `--max-wait 300s`. `MemAvailable` was 51.6GB at the time (no real memory pressure that would justify a legitimate long admission wait). So this was not a slow-but-honest wait; the process failed to honour its own declared bound and exit, one way or another.

split recovered by killing their own confine supervisor (`kill -INT`, scope cleaned correctly per `terminated-by=supervisor-signal:SIGINT`), never touched the shared slice, and re-ran successfully. Not reproduced on demand — a single live observation, credible and precise (exact command, exact timings), but not yet independently confirmed.

## What this call is, precisely — traced against source, not assumed

`aira confine-reserve --pinned` is the per-test RAM-governor helper built for AIRA-69 (extends the CPU governor to bound RAM: `internal/runner/confine_reserve*.go`, `runConfineReserveCommand` in `cmd/aira/main.go`). The `-p aitest -n 0` invocation shape strongly suggests this was actually spawned by the **legacy `aira_xdist_governor` plugin's** `_acquire_reservation` (`internal/pylib/aira_xdist_governor/__init__.py`), which is registered by many fastest-ee conftests whenever `AIRA_PY_LIB` is set — regardless of whether aitest is ALSO active for the same run (the exact double-arming shape AIRA-77 documented and closed as "redundant reservation, not correctness-critical, until AIRA-33 deletes the plugin"). **AIRA-33 (retiring that exact plugin) is being built right now** — once it lands, this specific CALLER goes away. That does not necessarily close the underlying question below.

## The open question this ticket is actually about

Independent of which caller triggered it: does the `confine-reserve` CLI/daemon path have a genuine bug where a `--pinned --max-wait N` request can fail to honour its own declared bound and hang indefinitely past it? Candidate mechanisms, not confirmed, for whoever investigates:

1. **CLI-side enforcement gap.** `internal/runner/admission_linux.go`'s `admitThroughDaemon` derives its transport deadline directly from the requested `maxWait` (`AIRA-58`'s own fix, `admission_linux.go:341-358`) — in principle this should tear the connection down at `maxWait + admitTransportGrace` regardless of daemon behaviour. Trace whether this path is actually reached for a `--pinned` reserve request, or whether `confine-reserve` routes through a different code path with a similar-looking but distinct enforcement gap.
2. **Post-admission hang.** AIRA-65 (closed tonight as not-needed) already traced that the reserve helper's SIGINT/SIGTERM handling correctly tears down via `WorkerAdmitLease.Close()` — but that's the *signal-delivery* path. This incident shows a *natural-timeout* hang, which is a different code path (whatever fires when `admissionMaxWait` is naturally exceeded, not when an external signal arrives) — confirm that path is actually exercised and correct, not just the signal path.
3. **A genuinely granted-but-never-read response**, e.g. the caller (Python governor plugin) itself blocking on `process.stdout.readline()` unboundedly (the exact shape AIRA-91's own investigation found in a *different* aitest code path, `Supervisor.acquire_worker()`) — meaning the Go CLI process may have correctly exited or been ready to, while ITS OWN stdout was never drained by the Python side, keeping the process from ever completing an OS-level `wait()`. This would make it a caller-side bug, not a daemon/CLI one.

Not established which of these (if any) is the actual mechanism — that tracing is this ticket's scope.

## Why this matters even after AIRA-33 lands

If the root cause is (1) or (2) above — a genuine CLI/daemon-side enforcement gap in `confine-reserve --pinned --max-wait` — it is latent for ANY future caller of that primitive, not just the plugin being deleted tonight. If it is (3), it's specific to the plugin AIRA-33 removes and this ticket can close as moot once that lands and is confirmed to be the mechanism. Recommend not closing this ticket on AIRA-33 landing alone — confirm which mechanism it actually was first.

## Confirming data point (split, 2026-09-06) — narrows candidate (3)

split confirmed the legacy governor was genuinely double-armed on their run (their worktree is pre-AIRA-33, off #1172; `git grep aira_xdist_governor` hits 10 fastest-ee conftests including the one that fired). More importantly: the wedged process was a **single PID** (1264500) alive for the full ~52 minutes, with `--max-wait 300s` on its own argv — not a re-spawn loop (which would show a fresh PID every ≤300s). One process outliving its own declared bound by ~10x is real evidence of a genuine wait-bound enforcement gap, independent of the caller.

**This does not fully rule out candidate (3) by itself**, though — a single long-lived PID is also consistent with the Go process finishing its wait correctly and then blocking indefinitely on a `write()` to stdout that nothing is reading (an undrained pipe blocks the writer regardless of how correctly the wait itself was bounded). Asked split, if they catch a live repro again before killing it, to check `/proc/<pid>/status` (State field) and `/proc/<pid>/fd/1` (what stdout is connected to) — this distinguishes "still executing its own wait logic" from "finished, blocked writing an unread result" cleanly. split is actively re-running the same suite on the pre-AIRA-33 base right now and offered to hold a wedged process alive rather than killing it, so this may get a live, inspectable repro rather than a second after-the-fact report.

## Candidate (3) ruled out; candidate (1)/(2) confirmed live (coordinating session, 2026-09-06)

split caught a wedged instance LIVE and held it rather than killing it (pid 1736618, `confine-reserve --pinned --max-wait 300s`, at 506s elapsed — 200s past its own bound). Both sessions inspected the real process directly via `/proc` (this machine, no namespacing):

- `readlink /proc/1736618/fd/1` = a pipe **with a live reader** (the pytest process) — rules out candidate (3) (blocked-write-to-undrained-pipe) outright.
- `/proc/1736618/wchan` = `futex_do_wait` for the main thread; all 7 OS threads checked via `/proc/<pid>/task/*/wchan` — six on `futex_do_wait`, one on `anon_pipe_read` (almost certainly the known stdin-EOF-watcher goroutine from AIRA-65's own trace — benign, expected, not itself a symptom). Nothing blocked on real I/O, nothing spinning.
- `SigBlk`/`SigCgt` in `/proc/1736618/status` look like ordinary Go-runtime signal setup, nothing abnormal.

**Conclusion: this is a genuine, confirmed-live Go-side wait-bound enforcement gap** — every goroutine is correctly parked in its own logic (not stuck on I/O, not a caller-side pipe stall), but whatever timer/deadline is supposed to fire at the declared `--max-wait` is not firing. Independent of AIRA-33's governor deletion; latent for any future `--pinned` caller.

Asked split to send SIGQUIT to the held process before killing it (Go's runtime dumps every goroutine's exact stack on an uncaught SIGQUIT) — if captured, that will name the precise function/line each goroutine is parked at, turning this from "confirmed gap, mechanism unknown" into an exact fix location. Not yet received at the time of this note; update this section if/when it arrives, otherwise the diagnosis above stands on its own as sufficient to prioritise and scope the fix.

## Not reproduced by the coordinating session independently

Both observations are split's own live runs, inspected jointly in real time rather than reproduced from scratch by this session — the process itself was directly and independently examined via `/proc` (not just split's report of it), which is why the diagnosis above is stated as confirmed rather than merely credible.

## Correction: AIRA-33 does not and cannot unblock fastest-ee's own conftests

split confirmed fastest-ee `origin/master` (#1175, `7b5785cb5`) still has all its original `aira_xdist_governor` conftest registrations — AIRA-33 only deleted the plugin from the `aira` repo itself. That is by design (the owner's decision was "delete the AIRA-side plugin now, accept fastest-ee migrates its own conftests on its own timeline"), but it means AIRA-33 landing does **not** make this wedge stop recurring on fastest-ee — whoever owns those conftests still needs to do that migration (SKILL.md's guard, `lite/conftest.py:784-803`, is the reference to copy). Immediate workaround given to split: `-p no:aira_xdist_governor` on the pytest invocation, which disables just that plugin without touching `AIRA_PY_LIB` or aitest's own RAM governance.

## ROOT CAUSE FOUND (split, SIGQUIT goroutine dump on the held live instance, 2026-09-06)

split captured a full goroutine dump before killing the held wedge (pid 1736618). It names the exact location:

```
goroutine 1 [select]:
  runtime.selectgo()               src/runtime/select.go:351
  /home/mark/claude/aira/cmd/aira/main.go:1295   <-- the un-firing select
  /home/mark/claude/aira/cmd/aira/main.go:191
  /home/mark/claude/aira/cmd/aira/main.go:49
  /home/mark/claude/aira/cmd/aira/main.go:31 (main)
  runtime.main()                    proc.go:285
```

The **main** goroutine (the whole process, not a background worker) is parked in a `select` at `cmd/aira/main.go:1295` (entered via `main.go:31 → 49 → 191 → 1295`, with a companion helper goroutine created around `main.go:1291-1292` in the same block). That select's case set does not include a case that fires on the request's own `--max-wait` deadline — no `time.After(maxWait)` / context-timeout branch reaches it — so it blocks on grant-or-cancel forever regardless of the declared bound. The value IS parsed correctly (it's right there on the process's own argv); it simply never reaches the select that would need to race against it. **Whoever builds the fix: verify this precisely against current source (line numbers will have drifted with tonight's other changes) rather than trusting the numbers verbatim, but this is the exact function to start from — no need to re-trace admission_linux.go/confine_reserve_linux.go from scratch.** The correct fix almost certainly reuses the pattern `internal/runner/admission_linux.go`'s own poll loop already uses for exactly this (a timer/context case derived from the request's `MaxWait`), rather than inventing a new mechanism.

**Secondary finding, same dump:** SIGQUIT printed the goroutine dump but did **not** terminate the process afterward — it was still alive post-dump, killed separately by split's own supervisor. Suggests either a missing `os.Exit` after the runtime's own quit-dump, or a `signal.Notify` handler swallowing `SIGQUIT` before the runtime's default handler gets it. Worth a mention (or a separate small ticket) in whatever build closes this — not confirmed to be the same root cause, may be independent.

## Resolution (2026-09-06)

**The reported mechanism was not found. `--max-wait` is honoured.** Both states of
the helper were reproduced live on this machine, against the real running daemon,
real cgroups and a real wall clock, and the `/proc` evidence this ticket rests on
turns out to describe the *granted* state, not a stuck wait.

### What `--max-wait` actually bounds

It bounds the **admission wait and nothing else**. Once granted, the helper holds
its reservation until its stdin closes — for the whole life of the test it was
granted for. That is the committed contract, stated verbatim in the AIRA-69
design spec §4 (`docs/superpowers/specs/2026-08-26-pytest-ram-weighted-governor-design.md:76-89`):
*"On GRANT it prints one line to stdout … and **holds the connection open,
blocking on stdin, until stdin closes / it is signalled**"*. So "alive past
`--max-wait`" is not, by itself, evidence of anything.

### The two states, measured — and how to tell them apart

| | waiting for admission | granted, holding the lease |
|---|---|---|
| goroutine 1 | `[IO wait]` → `net.(*conn).Read` ← `io.ReadAtLeast` | `[select]` in `main.runConfineReserveCommand` |
| marker thread | `do_epoll_wait`, syscall `281` (`epoll_pwait`) on fd 4 | `anon_pipe_read`, syscall `0` (`read`) **arg0 = `0x0` = fd 0 = stdin** |
| the other's marker | **no** `anon_pipe_read`, nothing reading fd 0 | **no** epoll thread |
| outlives `--max-wait`? | **no** — exits at the bound, `rc=4`, stdout empty | **yes, by design** — until stdin EOF |

Measured: a contended reservation with `--max-wait 10s` exited at **10.01 s**,
`rc=4`, `E_ADMIT_SATURATED`, stdout empty. A granted one with the same bound was
still alive at 5 s, 15 s and 30 s and exited 0 the instant its stdin closed.

Both the earlier `/proc` reading (*"six on `futex_do_wait`, one on
`anon_pipe_read`"*, no epoll thread) and the later SIGQUIT dump are the **granted**
column. The dump's own `[select]` at `cmd/aira/main.go` is the post-grant hold:
every earlier return in `runConfineReserveCommand` exits the process, so reaching
that select requires a successful grant and the `granted reserve=…` line already
written — and the "companion helper goroutine created in the same block" is the
`io.Copy(io.Discard, stdin)` goroutine, which those very lines create. The note
above dismissing the `anon_pipe_read` thread as *"benign, expected"* inverted its
meaning: that goroutine cannot exist before the grant, so its presence is
positive evidence the wait had already completed.

The three candidate paths were traced and are correct: `admitThroughDaemon`
applies `conn.SetDeadline(now + maxWait + 1s)` unconditionally before writing the
request and clears it only after a validated grant; `confine-reserve` has no wait
loop of its own (its 2-attempt retry fires only on an *immediate* too-large
rejection); and the daemon independently selects on
`max_wait_ms − since(enqueued)`, answering `E_ADMIT_SATURATED` on expiry.

**Stated honestly:** this establishes what the inspected process was doing at
inspection time and that the mechanism named here is not present in the code. It
does **not** prove a late grant was historically impossible for pid 1736618 — no
present-tense inspection can. The likely story remains a caller whose test
stopped progressing while the helper faithfully held a valid reservation.

### ⚠️ The fix directed above would have been a serious regression

The "ROOT CAUSE FOUND" section directs adding *"a timer/context case derived from
the request's `MaxWait`"* to that select. Applied literally:

```
--- FAIL: TestConfineReserveGrantOutlivesTheDeclaredBound
    a GRANTED reservation exited (<nil>) 2.014624618s after its grant, at or about
    its own 2s admission bound. --max-wait bounds ADMISSION ONLY; expiring a
    granted holder un-reserves a test that is still running
```

Every per-test RAM reservation would be silently released at 300 s while its test
was still running, and the ledger would then advertise capacity those tests are
using — the aggregate over-admission class AIRA-67 exists to prevent. A
regression test now fails that change.

### What was actually wrong, and was fixed

**D1 — the reservation ledger was diagnosis-hostile.** `aira confine --list`
reported the whole scope-less population as one opaque aggregate
(`5 scope-less reservations 5751380K`) with no signature, age or state, and a
granted helper is byte-identical in `ps` to a waiting one. No AIRA surface could
distinguish them, so the only inference left was the wrong one. This was the
**second** false P0 from this exact blind spot — AIRA-68's own comment
(`internal/runner/confine_manage.go`) records the first ("23 admitted jobs, only
3 live scopes"; 20 were healthy per-test reservations). `confine --list` now
names each one:

```
  of which: 0 confine scopes 0B, 2 scope-less reservations 1200M, 0 adopted scopes 0B
  (a scope-less reservation has no cgroup scope, so it never appears in the table above)
    state=holding held=6s reserve=1000M signature=pytest:tools/correctness/test_dispatch_coverage.py::test_board_temp_range_has_a_reader_and_is_no…
    state=holding held=4s reserve=200M signature=pytest:tools/fast/test_quick.py::test_a
```

Longest-held first, capped at 10 with an honest `… and N further reservations not
listed`; the client-supplied signature is bounded where it is retained and
escaped where it is printed. A daemon that predates the field sends no rows and
the CLI says so rather than implying the population was enumerated and empty.

**D2 — the bound had no real-time coverage at all.** Every prior test drove a
`net.Pipe` daemon that answers instantly, so the regression this ticket alleges
would have been invisible to the whole suite — which is why the claim could not
be refuted from inside the repository. Added, all real-cgroup / real-daemon /
real-subprocess / real-clock: a saturated wait must end at the bound with an
empty stdout; a **black-hole listener** test isolating the *client* transport
deadline (this ticket's own candidate 1) from the daemon timer; and a granted
hold must outlive the bound and release promptly on stdin EOF.

### Secondary finding: SIGQUIT — does not reproduce

Measured directly, stdin deliberately left open: SIGQUIT dumps 22 goroutines and
the process exits with status 2 in **0.10 s**. `signal.NotifyContext` here
registers only SIGINT and SIGTERM, so SIGQUIT keeps its default disposition and
nothing swallows it. No code change. What was observed was real, but it was not
this process declining to die — most likely the signal and the liveness check
addressed different pids. Recorded as not-reproduced rather than fixed.

### Follow-up left open (deliberately not built)

Two reviewers named the **unbounded lifetime of a granted scope-less reservation**
as the residual hazard: unlike a confine scope it has no cgroup artifact, so
AIRA-72's reaper and the stale-lease TTL are both structurally blind to it, and a
wedged caller pins slice reserve for as long as it lives. No TTL or reaper was
built: it would kill legitimately long tests, the reservation is already released
deterministically when its holder exits or closes stdin, and
`architectural-simplicity` says to expose the fact first and decide policy with
the data. D1 is that exposure. Also deferred: naming the holder pid via
`SO_PEERCRED` (new peer-credential plumbing; signature + age already identifies
it).

Also worth knowing before touching the hold: the release signal is the write end
of the helper's stdin pipe closing **everywhere**, so any descendant that
inherited it keeps the reservation alive after the original parent exits.

### Verification

`make ci` (fmt-check, vet, build, `go test ./... -count=1 -timeout 20m`) — exit
**0**, all packages ok. Thirteen mutants were applied and every one is killed by
at least one test, including: deleting the client transport deadline; disabling
the daemon wait timer; deleting the post-grant stdin hold; the fix this ticket
directed; deleting the row emission; capping before sorting; dropping the
signature escaping and the wire bound; and removing the bound's production call
site. Two of those mutants first **survived** and the tests were strengthened
until they did not — the all-zeros ledger check (vacuous, because
`pruneAdmitQueue` deletes an empty queue) and the signature bound asserted only
against its own helper.
