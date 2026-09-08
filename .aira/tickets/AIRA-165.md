---
{"schema":1,"id":"AIRA-165","project":"aira","title":"E_ADMIT_TOO_LARGE prints raw bytes and names no escape hatch, on a now-narrower population","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-165","to":"AIRA-153"},{"kind":"relates","from":"AIRA-165","to":"AIRA-151"}]}
---

AIRA-151 deferral **G3**, carried forward by AIRA-153 deferral **G6** and now
given its own ticket rather than living inside another ticket's resolution.

The operator-facing message is
`E_ADMIT_TOO_LARGE: required=%d cap_minus_headroom=%d basis=%s`
(`internal/daemon/admit.go`), in raw bytes, naming no escape hatch. Two
complications are why neither AIRA-151 nor AIRA-153 touched it:

1. the string is shared with PINNED requests, where "pin `--memory-reserve`" is
   exactly the wrong advice;
2. `cap_minus_headroom` is the ENTRY ceiling, a transient that moves by one
   per-job headroom term for every job outstanding at arrival, so a request
   marginally over it is refused with advice not to retry even though the
   ceiling may rise once a neighbour finishes.

AIRA-153 made the population NARROWER and more uniform, which is what makes the
case analysis tractable now. After it, a terminal refusal can only be:

- an over-ceiling per-signature ESTIMATE (this command's own measurement);
- an OOM ESCALATION whose recorded peak is at or above `FIT(ceiling)`;
- a PINNED request above the ceiling.

"Pin at or below `cap_minus_headroom`" is correct advice for the first two and
wrong for the third, so the case analysis the deferral was taken for is still
required — but it is now a three-way split with no blind-prior case in it.

`TestTooLargeRefusalMessageNamesBothNumbersAndTheBasis` pins the current string,
so a successor starts from recorded evidence rather than a paraphrase.
