---
{"schema":1,"id":"AIRA-181","project":"aira","title":"Admission-wait progress line reports the QUEUE, never the RUNNING reserve that is the actual blocker at position 1","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","ux"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-181","to":"AIRA-186"},{"kind":"relates","from":"AIRA-193","to":"AIRA-181"}]}
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

## Review (Fable build-review gate) — MERGED

PR #116 merged as `b186877` (2026-09-09), together with [[AIRA-186]] as one
change (branch `aira181-186-admission-reporting`, tip `2c1f83a`). Everything
below is the reviewer's own verification, not the builder's transcript.

- The wait line now carries both populations, each under its own noun:
  `queue position 1 of 9 by enqueue order, 0B queued ahead, 52G already granted
  across 9 admitted jobs / 64G slice ceiling`. Vocabulary is `confine --list`'s
  own summary line (`cmd/aira/main.go:3018`), so the two surfaces cross-check.
  The `:181-184` warning is answered rather than restated: "queued ahead" and
  "already granted" cannot be read as the same figure, and the larger one is no
  longer missing. Verified from source that `GrantedBytes = outstanding +
  adopted` sums only GRANTED, accounted waiters (`admit.go:2721`) — never the
  queue — and that `CeilingBytes` is cap-minus-headroom for `totalJobs+1`, the
  same term the evaluator applies (`admit.go:2642-2644`).
- Established-zero is printed (`0B ... across 0 admitted jobs`) and an
  unestablished pair is withheld; the pair is refused together on a negative
  ledger figure. A zero `CeilingBytes` is rendered as an absence — a genuinely
  collapsed ceiling is thereby conflated with an unreadable one, but the
  AIRA-103 pressure clause on the same line still names the throttle, so this
  is a residual, not a misstatement.
- Gates, reviewer's own runs on `2c1f83a`, confined, exact exit codes: gofmt
  `0`; `aira confine -- go test ./... -count=1` exit **0** (14 packages ok, 0
  FAIL lines; the run itself queued behind a 17-deep admission line on the
  pre-PR daemon and printed the old line — `queue position 5 of 17,
  11066370866 queued ahead` with no granted-set figure — a live specimen of the
  gap). CI: build+vet+gofmt pass, test pass, race pass.
- Porosity, reviewer's own mutants (all killed on the intended assertion, tree
  restored clean): held clause reading `aheadBytes` instead of `heldBytes` →
  `TestConfineQueueNoteNamesTheReserveHeldByAdmittedJobs` fails on `0B already
  granted`; launch path passing `0` instead of the printed reserve →
  `TestConfineAdmissionWaitLineDoesNotRepeatAPinnedReserve` fails on `resolves
  to 4G`; daemon reporting the ahead-sum instead of the matched waiter's own
  reserve → daemon test fails `2G, want 3G`.

ACCEPTED GAPS (recorded, not silent):

1. Live ledger totals render as raw byte counts (`54194584616 already
   granted ... / 52608M slice ceiling`) because `FormatConfineBytes` uses
   exact divisors. The category error is fixed; the at-a-glance comparison is
   not as good as it should be. [[AIRA-193]] (P3) carries the unit decision,
   which also moves AIRA-24's existing "queued ahead" figure.
2. The clause names GRANTED reserve, not live usage. `checkedAvailable`
   (`admit.go:2805`) charges `max(outstanding, memory.current − reclaimable)`,
   so when admitted jobs have grown past their grants ([[AIRA-178]]) a small
   request can still wait against a line reading, say, `52G granted / 64G
   ceiling`. `SliceCurrentBytes` is already on the wire; adding it is a
   wording decision for the same line AIRA-193 redesigns, not a defect in what
   these tickets asked for.
