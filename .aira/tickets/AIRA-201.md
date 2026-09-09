---
{"schema":1,"id":"AIRA-201","project":"aira","title":"aira confine --budget is unreachable: RouteClient falls through to a project store open and always returns E_CONFIG_INVALID","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-213","to":"AIRA-201"}]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

`aira confine --budget` is unreachable: RouteClient falls through to a project store open and always returns E_CONFIG_INVALID
**kind** bug · **severity** P1 · **closes** no rant directly (found in triage; blocks T17)

**SYMPTOM.** Reproduced by me today against the current binary and daemon:
```
$ aira confine --budget
E_CONFIG_INVALID: scope options are incomplete
```
`aira confine --list` from the same directory works and returns live jobs, so this is verb-specific, not transport or daemon availability. AIRA-180's Face 2 is 100 % dead on both shipped faces (CLI and `aira_confine_budget`), while `internal/core/skill.go:319` actively instructs agents to run it.

**ROOT CAUSE.** `cmd/aira/dispatcher.go:142` is `if canonical == "confine-list" || canonical == "confine-kill" { return d.dispatchConfineManagement(...) }` — `confine-budget` is missing. `internal/core/routing.go:51-53` classifies confine-budget as RouteClient (it is listed in the confine family), so Dispatch falls through to `dispatchClient` carrying the empty `daemon.WorktreeScope{}` that confine-management requests hand in, and the client opens a project store with incomplete scope options: `internal/store/store.go:580`, the only source of that string in the tree. The daemon's own handler at `internal/daemon/server.go:785` is therefore dead code.

**PROPOSED FIX.** Add `|| canonical == "confine-budget"` at `cmd/aira/dispatcher.go:142`. Do not instead remove confine-budget from routing.go's confine-family case — the exception would be invisible at the routing table.

**HOW TO TEST.** Dispatch `confine-budget` through `daemonDispatcher` with an empty `daemon.WorktreeScope{}` and assert it is sent as a daemon frame returning `ConfineBudgetResult`, not answered client-side. Fails today with E_CONFIG_INVALID. Mirror on the MCP face. Add a routing assertion pinning `server.go:785` as reachable so it cannot silently become dead code again.

**Second symptom, same class, cause UNEVALUATED:** `aira insights show resource-budget` returns exit 3 UNEVALUATED "machine-wide usage history is unavailable to this scope" (`internal/store/resource_budget.go:386-390`, the `s.owner == nil` branch) — a *different* path, untraced. Both documented routes to AIRA-180's data are dead; that is why this is P1. **Do not carry the earlier "stale daemon, a restart would settle it" explanation** — the daemon maps the current binary's inode, other daemon-backed verbs work, and I reproduced the failure today after that restart.

---

---

## Build correction — 2026-09-09

**The proposed one-line fix was incomplete, and shipping it alone would have been
worse than the defect.** `dispatchConfineManagement`'s daemon-down fallback
special-cases `confine-list` and lets everything else fall through to
`KillConfine` (cmd/aira/dispatcher.go). Adding `confine-budget` to the routing
arm without a second branch would have turned a read-only budget report into a
kill attempt carrying an empty selector whenever the daemon was unreachable.

The fix is therefore two parts: the routing arm, plus an explicit
`confine-budget` branch placed BEFORE the ci-shim block that returns
`UNEVALUATED` with a reason. A budget is a comparison against the daemon's own
peak-RSS history; the cgroup directory the fallbacks enumerate carries none, so
answering from it could only fabricate.

**A second defect was introduced by the fix itself and caught in self-review.**
`renderConfineBudgetResponse` prints `no usage history recorded yet` whenever
`Subjects` is empty — and the new unevaluated result is also empty. So "the
daemon is unreachable" rendered as "we looked and there is nothing", which is a
worse failure than the `E_CONFIG_INVALID` it replaced. The renderer now tests
`Verdict == "unevaluated"` first, keyed on the verdict rather than on the reason
string so a future path that forgets its reason still cannot fall through to the
wrong sentence.

`ConfineBudgetResult` gained a `Reason` field, following
`ConfineListResult`'s existing convention.

**Verified live** against the running daemon: the verb now returns real subject
rows for the first time. Three tests, all confirmed failing against the
unfixed code: routing (asserts the frame actually leaves for the daemon, so a
client-side answer that happened to succeed cannot pass it), the never-kills
twin, and the render twin.

Still unevaluated: the second symptom in the original body — why
`aira insights show resource-budget` returns `UNEVALUATED` via
`internal/store/resource_budget.go`'s `s.owner == nil` branch. Different path,
not traced, not fixed here.
