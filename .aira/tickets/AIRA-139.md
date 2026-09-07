---
{"schema":1,"id":"AIRA-139","project":"aira","title":"TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission fails when run twice in a row (self-interaction, not contention)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","flake","testing"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-139","to":"AIRA-149"}]}
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

## RESOLVED — it is not a second-invocation sensitivity at all

The kernel-resource hypothesis above is **wrong, and so is the framing**. There
is no leaked watch, no leaked fd, no process-global state, and nothing that a
second `Server` cannot reclaim. The two runs are not different: **BOTH of them
sit on the same ungrantable knife edge, and the first one wins a coin flip.**

Established by instrumenting the daemon's own admission evaluator and reserve
resolver (temporary `log.Printf` under `AIRA_DEBUG_ADMIT=1`, removed before
commit) and reading the real numbers off the failing run.

### The mechanism, end to end

1. Phase 3's confine request is **UNPINNED**, so by `ResolveConfineReserve` the
   reserve it carries to the daemon is *exactly* `runner.DefaultConfineMemoryReserve`
   = 4 GiB. That is not an operator figure and never can be: any explicit
   `--memory-reserve`, any `--memory-max`, and `--delegate-ram` all set
   `pinned`, and a pinned request returns from `resolveAdmitReserve` at its
   first line. An unpinned reserve is the blind 4 GiB constant, always.

2. In `resolveAdmitReserve`, the target signature's history after the phase-2
   OOM is exactly ONE observation. Measured:

       stats={TotalCount:1 SampleCount:1 PeakMax:56360960 OOMCount:1 MaxOOMPeak:56360960}
       req.reserve=4294967296  ceiling=1031798784  estimate=4294967296 basis="fallback:insufficient-samples"

   One sample yields no usable ordinary estimate, so `reserve` is still the
   blind 4 GiB. The OOM branch then takes `max(reserve, 1.5 * MaxOOMPeak)` --
   and 1.5 x 56360960 = 84541440 (80 MiB) does not come close to 4 GiB, so the
   escalation changes nothing. The value returned under basis
   `estimate:oom-escalated` is the untouched client default.

3. 4 GiB exceeds the fixture ceiling, so the branch's too-large clamp fires:
   `reserve = ceiling = 1031798784` -- the ceiling **exactly**, to the byte
   (1 GiB slice - 32 MiB base - 8 MiB supervisor headroom).

4. `checkedAvailable` computes `available = ceiling - max(current - reclaimable,
   outstanding)`. A reserve equal to the ceiling is therefore grantable **only
   while the slice's own charge reads byte-exact zero**.

5. And that is the entire flake. Measured, same binary, back-to-back:

       iteration 1 (PASSES): ... current=40960 -> 4096 (x11) -> current=0  => avail == reserve  => GRANTED
       iteration 2 (FAILS):  ... current=8192  -> 4096 (x107, ~30s)        => avail == reserve-4096 => never granted

   One residual 4 KiB page on an otherwise empty slice is the whole difference
   between pass and fail. The queue diagnostic in the original report was
   telling the truth all along: "queue position 1 of 1 by enqueue order, 0B
   queued ahead" -- nothing was contending; the request simply could not fit a
   ceiling it was itself equal to.

The "first run passes, second fails" pattern that made this look like a
second-invocation sensitivity was luck, not structure: iteration 1 was ALSO
stuck on that wait, for ~4 of its 4.79s, and merely happened to catch a poll
where the slice read zero. That also explains every disproof already recorded
above -- workload size, MemAvailable settling and daemon-goroutine teardown are
all irrelevant to whether one page is still charged at the instant a 1s poll
looks, and it explains why *file order* mattered without any test actually
interfering with another.

### The fix (test-only)

`internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go`:

- `oomSelfHealSliceMax` is now **derived** rather than picked:
  `runner.DefaultConfineMemoryReserve + (2 << 30)` (6 GiB). The fixture budget
  is a LIMIT, not an allocation -- the workloads still touch a few hundred MiB
  -- so its only job is to place the ceiling, and it must sit clear of the
  unpinned default or every unpinned resolution reaching the escalation branch
  lands exactly on that ceiling. Phase 3 now resolves to 4 GiB UNCLAMPED
  against a 6.4e9 ceiling with ~2 GiB spare, measured granted `admission=immediate`.
- New `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` pins the invariant at unit
  cost with no cgroup: it drives the real `resolveAdmitReserve` with the exact
  history the cold-start OOM leaves (one sample, one OOM) and requires the
  resolution to clear the fixture ceiling by at least one whole target
  workload. Mutation-checked: restoring `oomSelfHealSliceMax = 1 << 30` turns it
  RED with `phase-3 reserve=1031798784 leaves 0 below the fixture ceiling
  1031798784, want at least 335544320`.
- The header records a second accepted coverage gap that AIRA-139 **named
  rather than introduced**: phase 3 pins the escalation's ATTRIBUTION (the
  basis, reachable only via this signature's own OOM record), not the escalated
  VALUE, which is and was the client default. Driving the arithmetic for real
  would need an OOM peak above 2.7 GiB. It stays pinned at unit cost by
  `TestConfineEstimatorAndOOMEscalationClamp` and
  `TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins`.

### Why no production change was made

The clamp-to-ceiling behaviour is DELIBERATE and already documented in
`TestSliceCeilingDoesNotReachTheOOMEscalationClamp` ("clamps back to 64G --
accepted, so the request waits on the throttle"). Changing the resolution
policy so the blind default stops dominating the escalation would drop phase
3's cap from the ceiling to ~80 MiB, below the 320 MiB the workload needs --
i.e. it would break the AIRA-128 self-heal property this fixture exists to
prove, and it is a sizing decision for the machine-wide admission gate, not
something to slip into a P2 flake ticket. What the investigation DID surface
about production is written up separately as **AIRA-149** rather than being
fixed here or left implicit.

### Verification

- `-run 'TestRealOOMAttributes...$' -count=5`: 5/5 PASS, 0.52-0.62s each
  (against 4.79s + a 30s hang before). Deterministic, and the ~4s of wait that
  even the "passing" run was burning is gone.
- Self-run `-count=2` with the AIRA-133 neighbour: PASS.
- The ORIGINAL AIRA-133 blocking scenario reproduced deliberately by renaming
  `oom_cap_source_real_cgroup_linux_test.go` to sort BEFORE this file:
  `-count=2` PASS both iterations. **The AIRA-133 file-name mitigation is
  therefore no longer load-bearing.** It is left in place (the name is fine on
  its own merits and renaming it back is pointless churn), but it is no longer
  the thing holding the package green.
- `AIRA_REAL_CGROUP=1 go test ./internal/daemon/ -count=1`: ok, 70.0s.
- `make ci` (fmt-check, vet, build, `go test ./... -count=1 -timeout 20m`):
  exit 0, every package ok.

All runs under `aira confine`.

## Fable build-review record (2026-09-07) — MERGE

PR #99 merged as `3f318d4` (`--merge`). Reviewed head `1aa6413` against
`origin/master` (merge-base `f570a35`; master had moved to `52290de` by merge
time and the merge was clean). Scope from `git show --stat`, not the
narrative: exactly three files — this ticket, `AIRA-149.md` (new), and
`internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go`. No
production code touched; no `AIRA_DEBUG_ADMIT` instrumentation left behind.

Mechanism verified from source, independently of the PR text:

1. `runner.ResolveConfineReserve` (`confine.go:103`) leaves an unpinned
   request at `DefaultConfineMemoryReserve` (4 GiB); `resolveAdmitReserve`
   (`admit.go:1453`) returns pinned requests at its first line, and for one
   OOM sample `EstimateMemoryReserve` yields no usable estimate, so the OOM
   branch's `max` keeps 4 GiB and the clamp at `admit.go:1499-1500` pins it
   to `ceiling` exactly. `admitConnection` passes
   `ceiling = maximum - admitSliceHeadroom(outstanding+1)` (`admit.go:1701-1709`),
   which is what the new unit test models. The evaluator grants iff
   `waiter.reserve <= checkedAvailable(...)` (`admit.go:2246,2263`), i.e.
   `reserve <= ceiling - max(current - reclaimable, outstanding)`. So a
   reserve equal to the ceiling needs a byte-exact-zero charge. Arithmetic
   checks: 1 GiB - 32 MiB - 8 MiB = 1031798784; 6 GiB - 40 MiB = 6400507904;
   slack 2105540608 >= 335544320.
2. The instrumented logs under `~/tmp/aira139/` carry exactly those numbers
   (`reserve=1031798784`, `current=4096`, `reclaimable=0`, `outstanding=0`,
   `avail=1031794688`; post-fix `ceiling=6400507904`, `reserve=4294967296`
   unclamped, `admission=immediate`). `run5.log` is the 5/5 at 0.52-0.62s;
   `pkg-real.log` the 70.0s package run; `ci.log` every package ok.
3. Non-porousness and causality reproduced, not trusted. In a detached
   throwaway worktree at `1aa6413` with ONLY `oomSelfHealSliceMax` reverted to
   `int64(1 << 30)`: `TestOOMSelfHealFixtureStaysOffTheCeilingClamp` FAILS
   with the exact recorded message (exit 1), and the real-cgroup fixture at
   `-count=3` reproduces the flake — iteration 1 PASS after a 9.74s wait on
   the edge, iterations 2 and 3 FAIL `E_ADMIT_SATURATED` at 30.29s/30.44s
   (exit 1). On the PR head the same `-count=3` is 3/3 PASS at 0.49-0.50s.

Gates run here, exact exit codes, all under `aira confine`:
`TestOOMSelfHealFixtureStaysOffTheCeilingClamp` + the two referenced
arithmetic tests — exit 0; real-cgroup fixture `-count=3` — exit 0;
`AIRA_REAL_CGROUP=1 go test ./internal/daemon/ -count=1` — ok 67.7s, exit 0;
`go vet ./internal/daemon/` — exit 0; `gofmt -l` on the changed file — clean.
`aira get` parses both tickets; AIRA-149 collides with nothing on
`origin/master` or any other remote branch.

Accepted, noted, not blocking:

- The "coin flip / luck, not structure" framing slightly overclaims. The
  failure MECHANISM (reserve == ceiling, so any residual charge is fatal) is
  proven, but the data — the builder's `run2`/`run3` and my mutant run alike —
  shows a reliable order effect: the first fixture slice in a process decays
  to `current=0`, later ones sit at 4 KiB for the whole wait. WHY the residual
  is stickier after the first iteration was not identified. It is a non-file
  charge (`reclaimable` = `inactive_file + active_file` read 0, so not page
  cache). Not load-bearing: with the derived budget the wait would need a
  residual above ~2 GiB. Recorded as a named residual rather than a claim.
- The "renamed file sorts first, `-count=2` PASS" check has no log under
  `~/tmp/aira139/`; the `-count=3` runs here (iterations 2-3 are "not first
  in process", the actual trigger) cover the same mechanism.
- `ci.log` records every package `ok` and no `FAIL`, but no literal exit code
  line; the daemon package was re-run here as the merge gate.
- The unit test models the raw `maximum`, not `admitEffectiveMaximum`
  (`sliceceiling.go:746`, min with the published ceiling snapshot). A fixture
  slice path has no snapshot, so `effmax == max` in every log; irrelevant
  here, named so nobody reads the unit test as covering the throttle.
