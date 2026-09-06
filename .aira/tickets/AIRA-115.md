---
{"schema":1,"id":"AIRA-115","project":"aira","title":"confine-reserve defaults its slice instead of inheriting the parent job's, mis-attributing sub-reservations","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["admission","confine","confine-reserve","accounting"],"hold":false,"relations":[]}
---
Found during the AIRA-29 adversarial build review (Sol), then ground-checked in the source.
PRE-EXISTING; AIRA-29 neither introduced nor worsened it, but it is now written down.

`confineReserve` (`internal/runner/confine_reserve_linux.go`) inherits the parent job's SCOPE
ID from the environment (`InheritedConfineScopeID()`) but defaults its SLICE independently to
`DefaultConfineSlice` when the caller passes none. So a parent job confined to a non-default
slice can have its per-test `aira confine-reserve` sub-reservations register against
`aira.slice` instead — charging a slice whose cgroup does not hold that memory, while the
slice that does hold it never sees the reservation.

Consequences:

- The reserving slice is over-charged for memory it is not hosting, so healthy jobs there can
  wait behind a phantom reservation.
- The hosting slice under-counts, though its own physical `memory.current` term still charges
  the real usage.

Why AIRA-29 does not make it worse: AIRA-29 excludes a parent scope from dynamic charging
when it has live sub-reservations IN THE SAME QUEUE, which is exactly the queue where the
double-book would occur. In the split-slice case no single queue double-books — the queue
with no child charge is precisely the one where charging the parent its live usage is
correct. Before AIRA-29 the same split existed with the parent's frozen reserve.

Likely fix: have `confine-reserve` inherit the parent's resolved slice from the environment
alongside its scope id, and refuse rather than silently default when a parent scope id is
present but its slice cannot be established (the AIRA-58 rule).
