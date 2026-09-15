---
{"schema":1,"id":"AIRA-244","project":"aira","title":"aira import --tickets must accept the extractor's 'origin' field: rawTicketRow (import_tickets.go:67) uses DisallowUnknownFields and has no 'origin' field, so the fastest.ee extractor's per-row \"origin\":\"fastest-ee-backlog-export\" makes EVERY row fail json:unknown-field → whole import rejected. Found via subpipe's AIRA-237 schema sign-off (2007-row export). Fix: add Origin string json:\"origin\" to rawTicketRow (accept + ignore; disappear-scoping uses the events table, not this field). Add a test: a row carrying origin parses OK. BATCH with AIRA-242/243 for v0.8.","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---

