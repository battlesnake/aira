---
{"schema":1,"id":"AIRA-69","project":"aira","title":"cgrouptest scopes land on the shared production aira.slice when run inside a confine job","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["daemon","dogfood","testing"],"hold":false,"relations":[]}
---
## Symptom (found during the AIRA-67 investigation, confirmed live)

`internal/store/cgrouptest_linux.go:50` computes `host := filepath.Dir(current)` to find where to place a test cgroup. When the test binary itself is running directly inside a confine job's scope (i.e. `go test` invoked under `aira confine`, which this project's own CLAUDE.md mandates for all heavy commands), `current` is already somewhere under `aira.slice`, so `host` resolves to `aira.slice` itself — meaning the test's throwaway `.aira-test-*` scope is created as a direct SIBLING of live production job scopes, not in an isolated location.

Confirmed live: `.aira-test-TestRealPytestAitestEndToEndRealDaemonAndCgroupPassFailOnly-1224572474` was observed sitting alongside genuine production confine scopes on `aira.slice`.

## Impact

These test scopes are visible to `aira confine --list`, counted by the admission reserve scan/reconstruction, and swept by the orphan reaper — i.e. they participate in the exact same production accounting as real jobs, despite being disposable test fixtures. At minimum this pollutes diagnostic output and admission accounting with noise; depending on timing it could also contribute additional (if usually small and short-lived) pressure to whatever is causing AIRA-68's ledger leak.

## Suggested direction

Not investigated in depth. The test helper should resolve an isolated test-scope root independent of whatever slice the test process itself happens to be running under — e.g. anchor to a fixed, dedicated test-slice path rather than deriving one from the current process's own cgroup location, so a test run under `aira confine` doesn't nest its scopes inside production accounting.

## Resolution (2026-09-04, backlog-remediation Phase 0, plan section 2)

**The stated impact does not hold against current source, and that is the main
finding here.** This ticket says the test scopes "are visible to
`aira confine --list`, counted by the admission reserve scan/reconstruction, and
swept by the orphan reaper -- i.e. they participate in the exact same production
accounting as real jobs". Re-verified: they do not, and cannot.

All three of those paths funnel through one function,
`listConfines` (`internal/runner/confine_manage_linux.go`), whose very first act
is `strings.HasPrefix(entry.Name(), ".aira-CONFINE-")`:

- `confine --list` calls it directly;
- the admission reserve reconstruction calls `runner.ListConfines`
  (`internal/daemon/admit.go:867`);
- the orphan reaper calls `runner.ReapOrphanedConfineScopes`
  (`internal/daemon/confine_reaper.go:62`), which calls
  `listConfinesWithDeps` itself.

`IsolatedScopeParent` names its scopes `.aira-test-<TestName>-<random>`, which
matches none of them. So a test scope is invisible to every production
accounting path, has never been counted in a reserve, and has never been a reap
candidate. The residual is exactly what the plan already suspected: the directory
physically sits under the capped `aira.slice`, so its processes count against the
kernel's slice cap (which is correct -- those processes really are using that
RAM) and it shows up in a directory listing. Cosmetic, as recorded.

### What landed instead of the placement change

A **3-line assert** in `IsolatedScopeParent` refusing a test-scope prefix that
could collide with `.aira-CONFINE-`. That pins the property that actually keeps
these scopes out of production accounting, in the one place that decides it.

**The ticket's suggested direction -- anchor to a dedicated test-slice path
instead of deriving one from the current cgroup -- is deliberately NOT taken**,
and refusing outright would be worse than the problem:

- Refusing to run when `host` is `aira.slice` would SKIP every real-cgroup test
  whenever the suite runs under `aira confine`, i.e. in the workflow this
  project's own CLAUDE.md mandates. Trading a cosmetic listing entry for the
  silent loss of real containment coverage is a bad trade.
- Placing the parent under `current` instead of `filepath.Dir(current)` breaks
  the `+memory` tests by construction: cgroup-v2 forbids enabling
  `subtree_control` on a cgroup that holds member processes, and `current` holds
  the test process. The existing comment already says exactly this.
- Anchoring outside `aira.slice` would put throwaway test cgroups OUTSIDE the
  machine-wide memory cap -- a real safety regression in exchange for tidier
  output.

Accepted gap, stated rather than implied: test scopes remain siblings of
production scopes on `aira.slice` and remain visible to `ls`. They are contained
by the same cap, and no production accounting path can see them.

AIRA-69 -> done.
