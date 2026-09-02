---
{"schema":1,"id":"AIRA-43","project":"aira","title":"no test pins the granted-path lease-hold contract between the CLI and the daemon","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","test-coverage"],"hold":false,"relations":[]}
---
Found by Fable build-review (final gate, AIRA-30).

**The gap.** `runWorkerAdmitCommand`'s granted path (print the grant line,
hold stdin open as the daemon-side lease, `lease.Close()` on stdin EOF,
`cmd/aira/main.go`) has NO test anywhere that would fail if this contract
regressed in either direction -- despite `supervisor.py` explicitly
documenting this contract as confirmed and load-bearing (its malformed-
grant deadlock fix, AIRA-38 Sol review wave, depends on knowing exactly
when the relay does and does not exit).

**Failure scenarios, both currently invisible to every existing test:**
- Regression A: the stdin-hold block is dropped and the CLI exits right
  after printing `granted`. The daemon's dies-with-socket lease then
  releases the ledger entry while the worker is still alive, so
  Σ(granted caps) silently under-counts and the aggregate guard
  (`evaluateWorkerAdmit`) is defeated -- over-admission up to an
  outer-scope `memory.oom.group` kill.
- Regression B: the CLI never exits on stdin EOF. `_retire_worker`'s
  `wait(timeout=5)` swallows the resulting `TimeoutExpired`
  (supervisor.py) and orphaned relay processes accumulate holding live
  grants.

Under BOTH regressions every existing test stays green: `cmd/aira/
worker_admit_test.go` covers argument parsing and the floor only; the
daemon boundary test (`worker_admit_cli_boundary_test.go`) covers denied/
timeout only (granted needs a real cgroup for `CreateWorkerScope`, which
that file's fixture doesn't set up); `worker_admit_client_linux_test.go`
covers `RequestWorkerAdmit` (the Go client function), not the CLI
subprocess loop; every Python supervisor test stubs the CLI with a
Python script that never exercises the real binary's own hold/release
behavior.

**Candidate approach.** Extend `worker_admit_cli_boundary_test.go`'s
existing real-daemon-fixture pattern with a real, delegated cgroup scope
(`internal/cgrouptest`, same technique the real-cgroup e2e tests already
use) so the granted path runs against the real `CreateWorkerScope`; hold
the CLI's stdin open via a pipe, assert it does NOT exit while held,
close it, assert prompt exit AND that the daemon-side ledger entry is
actually released (e.g. a second identical request against the same
committed-cap ceiling can now be granted). Deliberately not built inline
during the final gate response pass -- real-cgroup test infrastructure
setup is enough work to warrant its own focused round rather than being
rushed alongside a dozen other findings.

relates AIRA-30, AIRA-38, AIRA-42.
