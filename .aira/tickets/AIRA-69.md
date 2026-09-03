---
{"schema":1,"id":"AIRA-69","project":"aira","title":"cgrouptest scopes land on the shared production aira.slice when run inside a confine job","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["daemon","dogfood","testing"],"hold":false,"relations":[]}
---
## Symptom (found during the AIRA-67 investigation, confirmed live)

`internal/store/cgrouptest_linux.go:50` computes `host := filepath.Dir(current)` to find where to place a test cgroup. When the test binary itself is running directly inside a confine job's scope (i.e. `go test` invoked under `aira confine`, which this project's own CLAUDE.md mandates for all heavy commands), `current` is already somewhere under `aira.slice`, so `host` resolves to `aira.slice` itself — meaning the test's throwaway `.aira-test-*` scope is created as a direct SIBLING of live production job scopes, not in an isolated location.

Confirmed live: `.aira-test-TestRealPytestAitestEndToEndRealDaemonAndCgroupPassFailOnly-1224572474` was observed sitting alongside genuine production confine scopes on `aira.slice`.

## Impact

These test scopes are visible to `aira confine --list`, counted by the admission reserve scan/reconstruction, and swept by the orphan reaper — i.e. they participate in the exact same production accounting as real jobs, despite being disposable test fixtures. At minimum this pollutes diagnostic output and admission accounting with noise; depending on timing it could also contribute additional (if usually small and short-lived) pressure to whatever is causing AIRA-68's ledger leak.

## Suggested direction

Not investigated in depth. The test helper should resolve an isolated test-scope root independent of whatever slice the test process itself happens to be running under — e.g. anchor to a fixed, dedicated test-slice path rather than deriving one from the current process's own cgroup location, so a test run under `aira confine` doesn't nest its scopes inside production accounting.
