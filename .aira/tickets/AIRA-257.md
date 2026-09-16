---
{"schema":1,"id":"AIRA-257","project":"aira","title":"aira board: 't' shows a ticket's title/description rewritten in plain English (Deepseek), cached on disk","status":"done","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["board","tui"],"hold":false,"relations":[]}
---

In `aira board`, pressing `t` on the selected ticket shows its title and
description rewritten from dense "claude-ish" into plain, human-readable English,
so a person can skim the board at a glance. Pressing `t` again toggles back to the
original — the original is always one keystroke away — and the pane/overlay label
it "plain-English (AI)" so it is never mistaken for the real ticket text. It works
in both the info pane and the expand overlay.

The rewrite is produced on demand by shelling out to the agentmux LLM gateway
(Deepseek by default — cheap, reliable, no rate limit) and cached on disk, keyed by
a content hash of the ticket's id+title+body, so it is instant next time and
re-translates only when the ticket text changes.

This is a deliberate, scoped exception to "AIRA is primitives, not judgement": it
lives ENTIRELY in the TUI face — core.Do / store / daemon never call an LLM. A
failed call renders "translation unavailable (CODE)", never a fabricated rewrite
(spec §14 honesty). The translate runs on its own goroutine (never the shared
fetch-worker pool, which a ~90s LLM call would stall); a superseded request's
reply is dropped by ticket id, and the held rewrite is cleared whenever the
selection moves to a different ticket so a stale rewrite never renders beside
refreshed metadata.
