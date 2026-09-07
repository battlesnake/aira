---
{"schema":1,"id":"AIRA-139","project":"aira","title":"TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission fails when run twice in a row (self-interaction, not contention)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

Discovered while building AIRA-133 (aira top's OOM-cap-provenance ticket): a
new real-cgroup daemon test
(`internal/daemon/confine_cap_source_real_cgroup_linux_test.go`,
`TestOOMTrailerDistinguishesAnEstimatedCapFromAnOperatorSuppliedOne`) happens
to sort alphabetically right before
`internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go`'s
`TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` (AIRA-128,
`1df8968`), and the neighbour started failing whenever both ran in the same
`go test ./internal/daemon/` invocation.

## What was actually wrong (it is NOT the new test's fault)

Initial hypothesis was resource contention: the new test does three real OOM
kills, which could plausibly perturb host memory/reclaim state long enough to
flip a neighbouring real-cgroup test's admission wait (this project has
precedent for exactly that shape — see the AIRA-128 fixture's own comment
about the 320 MiB fixture "next door"). That hypothesis was **disproved**:

- Shrinking the new test's workload sizes 5x (40/120 MiB → 8/24 MiB) had
  **zero** effect — still failed.
- A settle-wait polling host `/proc/meminfo` MemAvailable back toward its
  pre-test baseline before returning also had **zero** effect.
- The decisive test: running
  `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` against
  **itself**, `-count=2`, with NO other test in the run at all — first
  iteration passes (~3.5s), second iteration **fails** at exactly its own
  `AdmissionMaxWait: 30 * time.Second` bound
  (`confine_oom_selfheal_real_cgroup_linux_test.go:148`).

This proves the bug is a **self-interaction**: this test (or a fixture helper
it shares, most likely `newOOMSelfHealSlice` / the `server.admitPriorAt`
cache-reset dance, or something in `Server`'s admission-ceiling machinery that
is not fully independent between two `NewServer` instances created back to
back in the same process) leaves some state that a SECOND real-cgroup
admission-wait test — including itself — cannot cleanly proceed past. It has
nothing to do with AIRA-133's new test specifically; that test was only the
first thing to expose it by sorting next to it in file order.

## Reproduction

```
cd internal/daemon
AIRA_REAL_CGROUP=1 aira confine -- go test ./internal/daemon/ \
  -run 'TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission$' \
  -v -count=2
```

Exact failure (second iteration only):

```
confine_oom_selfheal_real_cgroup_linux_test.go:211: second confine run:
E_ADMIT_SATURATED: confine: admission rejected after 30s — slice contended,
no memory admission within the wait (reserve 4G/unknown) (stderr "confine:
waiting for memory admission on
/sys/fs/cgroup/.../.aira-test-TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission-.../slice
(requested reserve 4G, unpinned ... waited 15s, queue position 1 of 1 by
enqueue order, 0B queued ahead)\n")
--- FAIL: TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission (30.29s)
```

Note "queue position 1 of 1 by enqueue order, 0B queued ahead" — the second
run is NOT waiting behind anything in its own fixture's queue. Whatever is
blocking admission is not queue contention; it is the ceiling/ escalation
computation itself never resolving to a grantable size within the bound, on
the SECOND real launch of a fresh `NewServer`+throwaway-slice pair in the same
test binary process.

## Suggested next step

Bisect within `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission`
and its shared helpers (`newOOMSelfHealSlice`, `testPaths`, `startServer`, the
`server.admitPriorMu`/`admitPriorAt` reset) for anything that is
PROCESS-GLOBAL rather than per-`Server`/per-test — a package-level cache, a
well-known on-disk path not covered by `t.Setenv("XDG_STATE_HOME", ...)`, or a
kernel-level resource (a `/sys/fs/cgroup` mount option, an inotify watch limit,
a leftover cgroup a previous run's cleanup did not fully reap) that a first
real launch in the process consumes and a second cannot get back. Given this
reproduces test-self-vs-self with zero involvement of any other file, the
right entry point is `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission`
run alone at `-count=2`, not a wider bisection across the package.

This is not currently blocking anything: it was worked around for AIRA-133 by
confirming (not by fixing) that AIRA-133's own new test is unrelated, and by
not otherwise touching test ordering. It surfaces only when
`go test ./internal/daemon/ ...` happens to run this test a second time, or
run another real-cgroup-admission-waiting test immediately after it, which was
not previously exercised.

## Update — a working, non-fixing mitigation, and a narrowed root cause

AIRA-133's PR could not land at all through this repo's own pre-push hook
(`make test` = `go test ./...`) while this bug fired deterministically: its
new real-cgroup test in `internal/daemon` happened to sort alphabetically
BEFORE this ticket's test, and — confirmed by direct experiment — this
ticket's test fails reliably whenever it is NOT the first real `Server`+slice
sequence started in the test binary process, and passes reliably whenever it
IS first, regardless of what ran before it or how large that predecessor's own
workload was.

**Mitigation shipped in AIRA-133** (not a fix for this ticket): its new file
was named `oom_cap_source_real_cgroup_linux_test.go` specifically so it sorts,
and therefore runs, AFTER `confine_oom_selfheal_real_cgroup_linux_test.go` —
letting this ticket's test go first, where it is reliably fine. Confirmed
clean 2/2 full-package runs after the rename. This is fragile in principle
(any future file that happens to sort earlier, or any reordering of Go's own
file-compilation convention, could reintroduce the failure with a different
victim) and should not be treated as a real fix.

**Narrowed root cause.** Ruled out by direct experiment (not assumed):
- NOT the new test's memory footprint (shrunk 5x, no effect).
- NOT host-wide MemAvailable settling (a bounded poll-wait had no effect).
- NOT any `internal/daemon` package-level `var` — the only two are
  `sync.Once` guards for LOG DEDUP (`admitExclusiveCeilingWarnOnce`,
  `sliceMemoryStatDegradeOnce`), neither touches admission/ceiling decisions.
- NOT `t.TempDir()` / `XDG_STATE_HOME` / `XDG_RUNTIME_DIR` collision —
  `shortRuntimeDir` uses `os.MkdirTemp` (genuinely unique per call), and
  `testPaths`'s `t.TempDir()` base is unique per test.
- NOT daemon-goroutine teardown timing — `startServer`'s `t.Cleanup` blocks
  (bounded 5s) on `<-done` after `cancel()`, so `server.Serve(ctx)` has fully
  returned before the next test in the package starts.

What is confirmed: the failure is a real `E_ADMIT_SATURATED` after the full
30s `AdmissionMaxWait`, with the admission queue reporting "position 1 of 1,
0B queued ahead" — i.e. it is NOT waiting behind a queued reservation. The
next candidate worth checking (not yet investigated): a kernel-level resource
that a first real Server/daemon instantiation in the process consumes and a
second cannot fully get back on the SAME timescale — inotify/fsnotify watch
descriptors on cgroup `memory.events` (if the daemon watches it for OOM
detection), a per-user inotify instance limit, or open file descriptors on
`/sys/fs/cgroup` paths from the first daemon's still-closing watchers. This
would explain "not about memory pressure or state, but a second-invocation
sensitivity" cleanly, and is a concrete, checkable next step (e.g.
`ls /proc/<pid>/fd | wc -l` and `find /proc/<pid>/fdinfo -name 'inotify'`
across the two Server instances, or straceing the second admission wait to
see what syscall it is actually blocked on).
