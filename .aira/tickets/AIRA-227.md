---
{"schema":1,"id":"AIRA-227","project":"aira","title":"Unhelpful errors on daemon/client protocol skew and on install-refusal in a container: neither points at its fix","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["dogfood","dx","install","mcp"],"hold":false,"relations":[]}
---
From RANT-40 (dogfood, 2026-09-09), CI-image work. Two error-message clarity papercuts; the reporter judged neither worth its own ticket, but item 1 has since bitten a SECOND session (speed) blocking direct MCP filing, so it is filed as one small ticket.

1. PROTOCOL SKEW: after a daemon upgrade, every aira MCP call fails `E_DAEMON_PROTOCOL: daemon protocol is 9, client requested 8`. The MCP server pins its own aira client to whatever was on PATH when it started, so it stays stale until someone restarts it. The error gives no hint that the fix is "restart your MCP server" or "the aira on your PATH is stale". FIX: name the client binary path and its revision in the message, and state the remedy. (This has blocked at least two sessions from filing/querying via MCP.)

2. INSTALL REFUSAL: `aira install` refuses with "run sudo aira install from an active login session as a non-root account" — the right refusal, but identical whether you are root in a container with no systemd, or a non-root user lacking a login session. In a container the actionable answer is `--ci=shim` (and `--ci=auto` already does the right thing silently). FIX: when refusing inside a container / no-systemd context, point at --ci.

refs: RANT-40. Relates to AIRA-203 (install does not restart the daemon when only the binary changed) — same "stale image serving" family.
