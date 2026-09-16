---
{"schema":1,"id":"AIRA-258","project":"aira","title":"aira board: translation rewrites the FULL ticket into readable technical English (undergrad engineer / technical manager), not a short summary","status":"done","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["board","tui"],"hold":false,"relations":[]}
---

Follow-up to AIRA-257 (the `aira board` `t` plain-English translation). The owner
asked for the rewrite to target a technically literate reader — an undergraduate
engineer/scientist or a technical manager — and to produce the FULL description
rewritten into readable prose, NOT a short summary. A short lead-in summary is only
acceptable prefixed above the full rewritten text, never as a replacement.

Change is prompt-only (plus a self-documenting flag tidy): `boardHumanizePrompt` now
names that audience, asks for the full rewrite with all detail preserved (jargon,
shorthand, and bare ticket-ID references unpacked), and drops the old "1 to 3 short
sentences" instruction. Verified empirically on a real 1KB ticket: the new prompt
returns a full, structured rewrite (~1.6× the source, jargon unpacked) where the old
prompt compressed it into a two-sentence summary. Also spelled the translator flag as
`--concise=false` instead of the `--raw` alias (identical behaviour) so it is
self-evident the verdict-first "concise" preamble is disabled — the owner had queried
whether concise mode was in use (it was not; `--raw` is the documented alias for
`--concise=false`).

