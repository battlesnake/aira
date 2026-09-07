---
{"schema":1,"id":"AIRA-155","project":"aira","title":"The in-wait confine queue-position line does not surface a resolved reserve equal to the ceiling","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","honesty"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F6**.

`internal/runner/confine_queue_position_linux.go` prints, every 15s, lines like
`queue position 1 of 1 by enqueue order, 0B queued ahead`. That line was and
remains LITERALLY TRUE — it was the terminal message that lied, and AIRA-149
fixed that one. No change was made here.

The improvement available: adding "your resolved reserve is the whole admission
ceiling" to each tick would surface the AIRA-150 condition within 15 seconds
instead of at the end of the wait. That is a separate diagnostic decision about
what the in-wait line is for, and it needs the resolved reserve and ceiling
plumbed to the progress renderer, which today they are not.
