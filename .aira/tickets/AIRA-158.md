---
{"schema":1,"id":"AIRA-158","project":"aira","title":"The AIRA-67 design spec's basis vocabulary bullet predates the AIRA-149 tokens","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["docs"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F9**, filed as a RECORDED decision.

`docs/superpowers/specs/2026-08-25-confine-estimate-reserve-design.md:143`
enumerates the reserve-basis vocabulary and does not list `,oom-on-record` or
`,ceiling-clamped`.

Deliberately NOT edited by AIRA-149: that document is a dated milestone design
record, and this repo treats shipped specs as HISTORY rather than as living
reference. The authoritative live surface is the generated agent guide
(`internal/core/skill.go`), which AIRA-149 DID update, pinned by
`internal/core/skill_test.go`.

Named here so a reader meets a decision rather than an omission. If the project
later decides dated specs should be amended in place, this is a one-line change.
