---
{"schema":1,"id":"AIRA-20","project":"aira","title":"Harden wall-clock-tight tests to re-enable a -race CI job","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["ci","flaky","testing"],"hold":false,"relations":[]}
---
The first GitHub Actions CI run (2026-08-30) showed build+test green but the `-race` job failed — NOT on any data race (0 `WARNING: DATA RACE`), but on wall-clock latency assertions that don't survive -race's slowdown on a shared CI runner:
- internal/daemon/watch_test.go:105 TestWatchReturnsConcurrentEventWithinPollInterval — `event latency=124ms poll=30ms` (asserts an event arrives within the 30ms poll interval; under -race it took 124ms).
- TestWatchShutdownTerminalDrainAndOverflowBoundaries/concurrent_event — same watch poll-interval class.
- internal/daemon/admission_linux_test.go:871 TestAdmissionT11KillAndReconcileDoNotTakeAdmissionLock — `Kill/Reconcile blocked on the admission lock` (a latency/"not blocked within" assertion inflated by -race).

These pass `make race` on a fast box + `make test` (no -race) everywhere; they only fail full-suite `-race` on a slow runner. The repo gate is `make ci` (no -race), so CI now runs build+vet+gofmt+test only (the `race` job was removed — see .github/workflows/ci.yml). Fix per the AIRA no-timing-flakes discipline: replace the fixed wall-clock thresholds with condition-based waiting / generous env-scalable deadlines (or gate the wall-clock assertion behind a non-race fast-path), so a `-race` CI job can be re-added and pass reliably on shared runners. Then restore the race job in ci.yml.
