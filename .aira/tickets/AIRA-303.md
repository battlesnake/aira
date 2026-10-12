---
{"schema":1,"id":"AIRA-303","project":"aira","title":"A test writes s.jsonl into the repo root (leaked from AIRA-281 confine summary-file tests?)","status":"done","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":[],"hold":true,"relations":[]}
---

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** done. **State on master v0.34:** already-fixed.

Not a test leak: no current test can produce this file, and its content does not match the production summary format. Nothing to build; close the ticket and delete the untracked stray (`rm s.jsonl` in the repo root). A .gitignore entry or test-side cwd guard would be machinery for a one-time stray and is not warranted.

**Evidence:** Ticket has only a title (no body/comments), hold=true. The leaked /home/mark/claude/aira/s.jsonl (untracked, mtime 2026-10-10 11:10:56, 70 bytes) holds `{"run":2,"name":"other","ran":true,"exit":0,"terminated_by":"normal"}`. That line cannot come from the shipped AIRA-281 writer: the line struct at internal/runner/confine_summary.go:66-85 always emits `schema`, `owner`, `argv_sha256`, `slice`, `containment`, `admission`, `reserve_bytes` and has no `run` key, so this is a hand-written/early-draft probe line (mtime sits between bca9ae8 10:03 and dd8fe51 12:13 on the AIRA-281 branch). No Go/Python/shell file in the repo contains the literal `s.jsonl` (grep), and every summary test uses t.TempDir() paths (confine_summary_linux_test.go:230,320; cmd/aira/confine_summary_cli_test.go:79). Measured: ran `aira confine --name tri-303 -- go test -count=1 -run Summary ./internal/runner/ ./cmd/aira/` (both ok); s.jsonl mtime and md5 unchanged afterwards. No test writes it; it is a one-off stray from the builder/reviewer session.
