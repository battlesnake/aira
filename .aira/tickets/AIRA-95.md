---
{"schema":1,"id":"AIRA-95","project":"aira","title":"MCP server has no per-request deadline/cancellation -- a wedged daemon stalls every subsequent tool call for up to 6 minutes with no SIGINT recourse","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["daemon","mcp","reliability"],"hold":false,"relations":[]}
---
Found by a `/code-review high` pass launched during AIRA-84 (Fix 4, the symmetric daemon deadline-policy seam, PR #25) — the review completed but its findings routed to the coordinating session rather than back into the build agent that launched it (a nested-agent-dispatch notification-routing quirk), so this was recovered and posted to the PR as a comment; filed here as its own ticket since it stands on its own regardless of what lands in that PR.

## The gap

`cmd/aira/mcp.go`'s `Serve` loop is strictly serial: one JSON-RPC request runs to completion before the next line is even read (`mcp.go:278-303`). It is started with `context.Background()` (`cmd/aira/main.go:75`), and that same deadline-less, cancellation-less context flows straight into `dispatcher.Dispatch(requestContext, scope, request)` (`cmd/aira/mcp_project.go:73`).

AIRA-84 (Fix 4) introduced a real, deliberate `ResponseWait` bound (6 minutes) that the client side now falls back to whenever the caller declares no context deadline of its own — which the MCP path never does. So against a wedged (not crashed, not OOMed — genuinely hung) daemon, a single routed MCP tool call now blocks for up to 6 minutes, and because the loop is serial, every subsequent MCP tool call from the same agent session is blocked behind it for that same window. Unlike a CLI invocation, there is no SIGINT recourse: the MCP server is a background subprocess of the agent host, not something an interactive terminal can Ctrl-C.

This is a real behavior change AIRA-84 introduced without reasoning about it — its own plan (section 4.2) and ticket only reason about the CLI face. Before AIRA-84, this same scenario hit the old (buggy, dishonest) 30-second connect-deadline reuse and failed faster, just with a wrong/unclear reason. AIRA-84 fixed the honesty problem (no more OUTCOME_UNKNOWN from a stale deadline) but, for MCP specifically, made the failure mode slower and unrecoverable rather than fast and wrong.

## Suggested direction, not decided/built

Two candidates, not mutually exclusive:
1. A per-request deadline at the MCP dispatch seam (`mcp_project.go:73`) — bounds each call without a new config knob, but the "right" number is a judgement call: too short risks failing legitimate long-running MCP calls (a real `gate attest`, `reconcile --rebuild`) prematurely; too long doesn't meaningfully improve on the current 6-minute exposure.
2. Propagate real signal-based cancellation into the MCP server's context, matching the pattern other entry points already use (`cmd/aira/tui.go:80`, `main.go:1008`, `main.go:1164`, `cmd/aira/daemon_command.go:43` all install a signal handler; the MCP entrypoint at `main.go:75` does not). This is more architecturally consistent than picking an arbitrary timeout, and gives at least an external recourse (a signal sent to the MCP host process) even though there is still no interactive terminal to send it from directly.

Not scoped, not built. Whichever direction is chosen needs its own two-loop given it touches daemon-facing MCP request handling.
