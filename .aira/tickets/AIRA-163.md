---
{"schema":1,"id":"AIRA-163","project":"aira","title":"A slice can now admit a job it will certainly OOM-kill (the band above the degenerate floor)","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-163","to":"AIRA-153"}]}
---

AIRA-153 deferral **G2**, accepted deliberately and written down rather than
left silent.

`runner.SliceFittedReserve` returns 0 below `runner.MinPinnedScopeCap` (1 MiB),
so a degenerately small slice still refuses terminally with `required` and
`cap_minus_headroom` populated. Above that floor there is a band —
`FIT(ceiling)` above 1 MiB but below whatever the job actually needs — in which
AIRA now ADMITS a job that will certainly be OOM-killed, where before AIRA-153 it
refused the job outright.

Why that is not a silent under-provision:

- the kill is attributed to the command's own signature;
- `terminated-by=oom` and exit `137` are reported, and the trailer's
  `cap-source=auto:daemon-reserve` says AIRA chose the number, not the operator;
- the next admission either escalates within the slice or refuses terminally
  once the recorded peak reaches `FIT(ceiling)`.

Why it is not fixed: refusing inside that band would require a judgement about
"how big a real job is", which *AIRA is primitives, not judgement* forbids. A
refusal is also not a safer outcome than an attributed OOM, because the OOM feeds
the ladder and the refusal does not.

Recorded for evidence. Reopen only with a rule that does not require that
judgement.
