---
{"schema":1,"id":"AIRA-170","project":"aira","title":"Seven tickets on master carry severity P3, which domain.validSeverity rejects: aira show/link/rant --ref refuse them with E_CONFIG_INVALID 'ticket enum is invalid'","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["data-model","tickets"],"hold":false,"relations":[]}
---
Found while closing AIRA-165/166 after PR #104 (merged `34ea0b0`).

`internal/domain/ticket.go` `validSeverity` accepts only `P0|P1|P2`, and `aira create`'s spec offers the same three. But the repo's own tickets AIRA-162, 163, 164, 165, 166, 167, 168 (all hand-written at the AIRA-153 close-out, c19d117 and earlier) carry `"severity":"P3"`. Consequences, each reproduced on master at `34ea0b0`:

- `aira show AIRA-165` → `E_CONFIG_INVALID: ticket enum is invalid` (exit 2)
- `aira link AIRA-169 relates AIRA-165` → the same
- `aira rant ... --ref ticket:AIRA-165` → `E_RANT_REF_INVALID: reference does not exist in this project` — the ticket exists on disk; the loader refused it
- `aira link AIRA-169 relates AIRA-151` (a P2 ticket) → OK, which isolates the cause to the severity value

Two separate defects:

1. The domain enum and the owner's practice disagree. Either P3 is a legitimate severity (the owner uses it for accepted deferrals and doc nits, so probably yes) and `validSeverity` should accept it, or the tickets are wrong and `aira check`/`reconcile` should REPORT the drift. Today the reader path refuses what the git file carries, silently from the point of view of anyone who only writes files — a value the file accepts and the tool refuses is the porous kind of enum.
2. The error is misattributed and unspecific. `E_CONFIG_INVALID` says the CONFIG is invalid; nothing in `.aira/config` is. The message names neither the field (`severity`), the value (`P3`), nor the allowed set, and on the rant path it degrades further to "reference does not exist", which is false. A refusal on a ticket's own field should carry a ticket-shaped code and name field, value and allowed set.

Until fixed, AIRA-169's relation to AIRA-165 had to be hand-written into its frontmatter because the tool could not record it.
