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

## Follow-up corroboration (qual, 2026-09-08, after the ticket was filed)

qual pinpointed the exact misleading phrase, distinct from "no data at
all": position 1 with `0B queued ahead` is the literal state the queue is
in *by definition* at the head — the arithmetic isn't wrong, but "queued
ahead" reads as "nothing blocking you" when the real blocker (the running
set's held reserve) is a different, unmentioned population. Their
suggested minimal shape: put both populations in the same line, e.g.
`queue position 1 of 9, 0B queued ahead, held by running: 52G / cap 64G`
— so position-1-with-nothing-ahead reads correctly as "your request is
fine, wait" instead of "your request will never fit". Live reproduction
pasted at time of writing: `waited 795s, queue position 1 of 9, 0B queued
ahead` against 34G MemAvailable and a 44G request, mid-wait, deliberately
not guessed down this time. Consistent with the fix already scoped above
(the `SliceReserve` fields are already in hand at the call site) — this
just sharpens the wording target for whoever implements it.

## Follow-up reproduction (split, 2026-09-08) — the downstream consequence when it never resolves

A merge-gate leg's 617 MiB request (≈1% of a 63232M slice) waited the full
30 minutes and hit `E_ADMIT_SATURATED`, surfacing to the leg runner as
`Error 4`/`exit=2` — indistinguishable, downstream, from a genuinely
failing `go test`. Only the `E_ADMIT_SATURATED` line one screen up (not
repeated in any summary) tells the two apart; a caller grepping leg exit
codes cannot. This is the same root cause as the rest of this ticket, seen
from the other end: not just "the wait message doesn't say what blocks
me", but "and if it never resolves, the outcome collapses into a generic
non-zero exit indistinguishable from a real failure". Relevant to whoever
implements this ticket's fix: the terminal `ran=no` trailer (AIRA-147)
already exists precisely to make this greppable — the gap split is
describing is a caller (their own merge-gate leg runner) not consuming
that signal into its own leg summary, which is downstream-project scope,
not an AIRA fix. Noted here only because it's useful corroborating
context for why the wait-line clarity in this ticket matters (catching the
starvation *before* it resolves into a saturated timeout is strictly
better than any downstream attribution fix).

Split also raised, then explicitly declined to request a ticket for: a
617 MiB request starving 30 minutes while ~75G was already granted against
a 63232M ceiling reads, on the surface, like "small requests aren't
prioritised" — but the slice was already over its ceiling by the full
grant total (three running jobs holding ~75G against 63232M), so there was
zero headroom for a request of *any* size, not evidence of an
admission-ordering/fairness defect. This is the same already-tracked gap
as [[AIRA-178]] (no live actuator reclaims capacity from already-admitted,
still-growing jobs) and [[AIRA-177]] (narrows how far over-ceiling the
slice can drift at admission time, doesn't close it). No new ticket
filed for this.
