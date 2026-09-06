---
{"schema":1,"id":"AIRA-124","project":"aira","title":"Exclusive-admission refusal exits 1 while the same condition via E_ADMIT_SATURATED exits 4","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
---
Filed as an AIRA-107 review follow-up. Out of that ticket's scope:
`E_ADMIT_EXCLUSIVE_ACTIVE` is not one of the eleven codes AIRA-107 decided, and
AIRA-107 did not move it.

`E_ADMIT_EXCLUSIVE_ACTIVE` is catalogued at 1 (AIRA-101 — "another benchmark
holds the slice, retry later", `internal/codes/codes.go`). AIRA-107 decided
`E_ADMIT_SATURATED` at 4 as host capacity exhaustion. But an *ordinary* job that
waits out its admission budget behind an exclusive holder is rejected with
`E_ADMIT_SATURATED`, whose message is literally "the slice is held exclusively by
another job for benchmarking; retry when it finishes"
(`internal/runner/admission_linux.go:551`, `rejection.Exclusive == "held"`). The
`"draining"` arm at `:556` has the same shape.

So one machine condition — an exclusive benchmark holds the slice — reaches a
caller as exit 1 down one path and exit 4 down the other, and an agent branching
on the exit alone cannot tell they are the same thing. Neither number is wrong in
isolation; the split is.

Decide one of:

- give the exclusive-held / draining rejections their own code, bucketed with
  `E_ADMIT_EXCLUSIVE_ACTIVE`;
- move `E_ADMIT_EXCLUSIVE_ACTIVE` to 4 with the capacity family; or
- record why the two paths legitimately differ.

Whichever is chosen, pin it in `internal/codes/produced_test.go` so it cannot
drift back.

## Resolution

Option two: **`E_ADMIT_EXCLUSIVE_ACTIVE` moves from bucket 1 to bucket 4**,
alongside `E_ADMIT_SATURATED`. AIRA is pre-release, so unifying costs nothing and
justifying the split would leave the defect in place.

The two codes report the same underlying condition — an exclusive job holds the
slice, temporary capacity exhaustion cured by waiting — down two paths, and an
agent branching on the exit status alone (the thing the buckets exist for) must
see one number for one condition. 4 already means "retry when the box is free".

A third path confirms 4 rather than 1: the runner wraps this daemon refusal as
`E_CONFINE_UNAVAILABLE`, which is catalogued at 4, so the confine CLI already
exited 4 for this event while the catalogue published 1 for the code that caused
it.

`U_ADMIT_EXCLUSIVE_UNESTABLISHED` is deliberately **not** moved. It is a
different claim — the daemon could not establish that the slice is empty, which
is not "the slice is busy" — and stays unevaluated at 3.

Changes:

- `internal/codes/codes.go` — the entry moves to 4, with the AIRA-101 reasoning
  it replaces recorded inline rather than deleted.
- `internal/codes/produced_test.go` — new
  `TestExclusiveAdmissionRefusalSharesTheCapacityBucket` pins it. The pin is
  written as an *equality* between `E_ADMIT_EXCLUSIVE_ACTIVE` and
  `E_ADMIT_SATURATED` as well as a literal 4, so it fails in both drift
  directions, checks `E_CONFINE_UNAVAILABLE` (the exit the face actually
  produces), and asserts `U_ADMIT_EXCLUSIVE_UNESTABLISHED` stays 3 — a lazy
  "move the exclusive codes to 4" fails that last arm.
- `docs/superpowers/plans/2026-09-05-aira101-confine-exclusive-plan.md` — the
  one doc line asserting the old bucket is marked superseded.

No production behaviour changes: the message text, the terminal-refusal routing
and the daemon protocol are untouched. Only the published exit contract moves.

Non-porousness verified by reverting the catalogue entry to 1 with the new test
in place: it failed on three separate assertions (literal, `ExitForCode`, and the
`E_ADMIT_SATURATED` equality).

Verification, exit codes recorded:

- `aira confine -- go build ./...` — exit 0
- `aira confine -- go vet ./...` — exit 0
- `AIRA_REAL_CGROUP=1 aira confine -- go test ./... -count=1` — exit 0
