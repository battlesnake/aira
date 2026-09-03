---
{"schema":1,"id":"AIRA-50","project":"aira","title":"runner.IsDelegateRAMScopeID missing from the non-Linux build stub (confine_manage_stub.go)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["build","non-linux","runner"],"hold":false,"relations":[]}
---
Surfaced by AIRA-49's build-review while spot-checking that the new `ReapScopeIfEmpty` stub (added to `internal/runner/confine_manage_stub.go` for `!linux` builds) actually gets a non-Linux build past the daemon package. It does — but `internal/daemon/admit.go:674` calls `runner.IsDelegateRAMScopeID` (real implementation only in `internal/runner/confine_manage_linux.go:400`), which has no `!linux` stub at all, so any non-Linux build of `internal/daemon` still fails on `undefined: runner.IsDelegateRAMScopeID`, independent of and pre-existing before AIRA-49.

Confirmed pre-existing (not introduced by AIRA-49 or any other recent change) — reproduced identically on a detached `origin/master` worktree before AIRA-49's own commits.

**DONE (2026-09-03, master `9e92d68`, direct commit — trivial/mechanical, no design work needed, per this project's lighter-path allowance).** Added a `!linux` stub returning `false` (the real implementation is pure string matching against a marker only the Linux confine launch path ever mints, so `false` is the only correct answer off Linux, not a fudge). Verified via `aira confine -- go build ./...` and `go vet ./internal/runner/...`, both clean.

Not independently re-verified via a full non-Linux cross-compile (the same pre-existing `internal/pylib/extract.go` `unix.Renameat2` blocker AIRA-49's build-review already documented prevents that without a separate workaround) — correctness verified by inspection instead (a 3-line, dependency-free function matching the required signature exactly).

relates AIRA-49.
