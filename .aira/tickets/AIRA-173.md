---
{"schema":1,"id":"AIRA-173","project":"aira","title":"TestRunInputServerACKReconnectAppendAndExplicitClose flakes under full-suite load with a broken-pipe write to the run-input socket","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["flaky","runner","tests"],"hold":false,"relations":[]}
---
Filed rather than re-run until green, following the AIRA-106/AIRA-112 precedent.

`make ci` in the pre-push hook failed on PR #105's branch:

```
--- FAIL: TestRunInputServerACKReconnectAppendAndExplicitClose (0.00s)
    run_input_server_linux_test.go:287: write unix @->/home/mark/tmp/aira-d6-run-input-tests/
    case-440245273/inputs/RUN-1-a00e713dcb7caf578bc81c5d.sock: write: broken pipe
FAIL	aira/internal/runner	133.373s
```

A write to the run-input unix socket lost the race with the server closing its
end. The test fails in 0.00s, so nothing timed out; the peer was simply gone by
the time the client wrote.

## Evidence that this is not PR #105's

- that branch touches **zero** files under `internal/runner`
  (`git diff --name-only origin/master...HEAD | grep -c internal/runner` → 0);
- the named test passes **5/5** in isolation on the branch
  (`go test ./internal/runner/ -run <name> -count=5` → exit 0);
- the branch's own full suite passed **twice** standalone before the push —
  `AIRA_REAL_CGROUP=1 go test ./... -count=1` exit 0, and plain
  `go test ./... -count=1 -timeout 20m` exit 0.

## What is NOT established

A clean-`origin/master` (`2364a17`) full-suite run in a throwaway detached
worktree passed (exit 0), so there is **no clean-master reproduction** — one
green run is not evidence of absence for an intermittent failure, and this
ticket does not claim one. Two pre-push runs on the branch failed; the first
one's output was truncated before the test name was captured, so it is not known
whether it was this same test.

The honest statement is therefore: an intermittent failure of unmeasured
frequency, in a package the triggering branch does not touch.

## Where to look

`internal/runner/run_input_server_linux_test.go:287`. The ACK/reconnect handshake
means the client may legitimately observe the server's close between two writes;
the question is whether the TEST tolerates that (it should synchronise on the ACK
rather than assume the peer is still there) or whether the SERVER closes earlier
than the protocol allows. Decide which before touching either — a retry loop
added to the test would hide a real server-side early close, which is the failure
mode worth catching.

## Frequency first

Before changing anything, measure: run `internal/runner` under the same
whole-suite contention (all packages in parallel, `-count=N`) and record the
failure rate. A fix with no before/after rate is not evidence.
