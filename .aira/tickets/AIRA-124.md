---
{"schema":1,"id":"AIRA-124","project":"aira","title":"Exclusive-admission refusal exits 1 while the same condition via E_ADMIT_SATURATED exits 4","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty"],"hold":false,"relations":[]}
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
