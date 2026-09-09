---
{"schema":1,"id":"AIRA-199","project":"aira","title":"cmd/aira m20b test hands its own os.File fds to a callee that closes them, so the finalizer double-closes a recycled descriptor -- a -race CI flake and a package-wide cross-test hazard","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["ci","flake","test-hygiene"],"hold":false,"relations":[]}
---
Found while gating AIRA-196 (PR #123, 2026-09-09). The `race` CI job failed
on a branch whose own changes were green, with a single unrelated failure:

```
--- FAIL: TestM20bSupervisorRejectsMalformedSidecarBeforeOpeningProject (0.00s)
    m20b_test.go:54: EOF
```

It is NOT a data race and NOT caused by that branch. It reproduces on
pristine `origin/master` (c402ef3), measured:

| tree | change | `go test ./cmd/aira/ -race -run TestM20b -count=400` |
| --- | --- | --- |
| aira196 branch | none | 4 failures |
| master c402ef3 | none | 3 failures |
| master c402ef3 | `runtime.KeepAlive(readyW)` | 1 failure |
| master c402ef3 | pass a `syscall.Dup` of each fd | **0 failures** |

## Root cause, from source

`cmd/aira/m20b_test.go:44-47` hands the supervisor the fd numbers of the
test's OWN `*os.File` pipe ends:

```go
exit := runSupervisor([]string{..., "--ready-fd", itoaFD(readyW), "--ack-fd", itoaFD(ackR), ...}, nil)
```

`itoaFD` (`m20b_test.go:67`) is `file.Fd()` — it returns the descriptor but
the `*os.File` keeps OWNERSHIP of it. The callee then closes it:
`writeSupervisorFailure` (`cmd/aira/main.go:612-622`) wraps the number in
`os.NewFile` and calls `f.Close()`.

So after `runSupervisor` returns, the test's `readyW` is an `*os.File`
holding a descriptor number that is closed and free for reuse. `readyW` is
never referenced again and has no `defer Close()`, so it becomes garbage
and `os.File`'s finalizer closes that number a SECOND time — whenever the
GC happens to get to it. Two failure modes follow:

1. The finalizer fires DURING `runSupervisor`, before the write. Every write
   end is then closed and the test's own `Decode` gets EOF. `KeepAlive`
   alone fixes this one, which is why it cut the rate but did not clear it.
2. The finalizer fires LATER, after the number has been recycled — into a
   subsequent iteration's `readyW`, or into any other open file in the test
   binary. That is a cross-test descriptor close: at `-count=1` it can break
   a LATER test in `cmd/aira` rather than this one, which makes it a
   package-wide hazard rather than one flaky test.

`-race` raises the rate by allocating far more (more GC, sooner finalizers),
which is why it surfaces in the `race` job and not in `test`.

## Fix

Give the callee a descriptor it may own — `syscall.Dup(int(readyW.Fd()))` —
and let the test close its own ends deterministically. Measured 400/400
green on master. `runtime.KeepAlive` is NOT sufficient (measured) and does
not address failure mode 2 at all.

While there, audit `cmd/aira` for the same shape: `supervisor_relay_store_test.go:441`
passes a `readyFD` int and should be checked for who owns and closes it.

## Why it was not fixed inside AIRA-196

It is pre-existing, in a different subsystem, and a feature PR is the wrong
place to carry an unrelated fd-ownership fix. The AIRA-196 `race` job was
re-run and passed cleanly (run 34294346701), so the merge gate was honest;
this ticket carries the diagnosis and the measured fix so the flake is not
merely forgotten.
