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
