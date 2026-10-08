---
{"schema":1,"id":"AIRA-281","project":"aira","title":"confine --summary-file \u003cpath\u003e: append one JSON line per job (name, owner, cmd/hash, exit, ran, admission, terminated-by, peak-rss, reserve, wall/cpu, optional tree hash)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.29","labels":[],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-281","to":"AIRA-282"}]}
---


## Request (field/Stoner, 2026-10-08; blocks Stoner + flavour Makefile CI)

Foreground `confine --json` is refused and the trailer is one text line of `key=value` facets, so Makefiles must sed it. Add `--summary-file <path>`: append ONE JSON line per job (append-safe under `make -j`), with at least: name, owner, command (or its hash), exit code, ran, admission, terminated-by, peak-rss, reserve, wall time, cpu time, and an optional caller-supplied tree hash. A never-ran job (refused admission) still writes a line (ran=false). Unreadable fields are `unevaluated`, never 0. Challenge first: is the greenfield minimum just a JSON rendering of the existing trailer facets? Resumable-gate caching stays in the repo (aira does not replay passes).
