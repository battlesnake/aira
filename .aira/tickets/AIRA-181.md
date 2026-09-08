---
{"schema":1,"id":"AIRA-181","project":"aira","title":"Admission-wait progress line reports the QUEUE, never the RUNNING reserve that is the actual blocker at position 1","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","ux"],"hold":false,"relations":[]}
---

Four independent peer sessions hit the same misreading tonight
(2026-09-08): wt, subpipe, split, and (in effect) qual. All four sat at
queue position 1 with "0B queued ahead" for many minutes — in split's case,
this led them to kill a job that was in fact correctly waiting on 3 running
jobs holding ~20G reserve. wt and subpipe each watched the queue GROW behind
them (5→14, 5→12) while never draining through. qual reported the adjacent
gap directly: no way to ask "what reserve would currently admit" before
choosing a number — dropped from a correct 40G to an incorrect 30G, which
then genuinely OOM-killed with zero test results.

## Verified from source

`confineQueueNote` (`internal/runner/confine_queue_position_linux.go:98-186`)
composes the wait line's queue clause from `confineQueuePosition.aheadBytes`
— and the comment there is explicit and self-aware about the exact
limitation: "the figure counts ONLY the queued waiters in front — the
reserve already GRANTED to running jobs is much larger and is not in it; a
bare 'reserved ahead' invites reading it as 'the memory standing between me
and admission', which it is not" (`:181-184`). That is precisely the
misreading all four sessions independently produced tonight — a real,
predicted-by-the-code's-own-comment UX gap, not a defect in the queue
arithmetic itself (position and queued-ahead are both correct).

**The data to close the gap is already fetched at this call site.** The
probe already unmarshals the full `ConfineListResult` and reads
`result.SliceReserve` (`confine_queue_position_linux.go:265-268`), the same
struct whose `GrantedBytes`/`CeilingBytes`/job-count fields already back
`confine --list`'s own "slice reserve: X granted / Y ceiling across N
admitted jobs" summary line (AIRA-73, `internal/daemon/confine_manage.go
:184-201`). Composing "blocked on Xg reserved by N running scopes" into
the per-waiter wait line needs no new daemon round trip or wire field —
only using a value already in hand at the point the line is built.

## Not designed here

Exact wording (the comment at `:181-184` explains why a bare "reserved
ahead" number is dangerous — any fix must not reintroduce that exact
ambiguity between queued-ahead and running-held reserve) and whether to
also answer qual's adjacent "what would currently admit" question (a
`Drilldown`-style figure a caller could check BEFORE submitting, e.g. "the
largest reserve that would admit immediately right now is Yg") are left
for whoever picks this up — the latter may be better served by AIRA-180's
resource-budget recommendation surface than by this wait-line change, since
it is a different moment (pre-submit vs. mid-wait). [[AIRA-180]]
