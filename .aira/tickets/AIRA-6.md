---
{"schema":1,"id":"AIRA-6","project":"aira","title":"Consider: reduce whole-job reserve for gate-style jobs (make merge-gate reserves 37.9G vs 22G used)","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---
aira confine -- make merge-gate takes one whole-job p90-peak reserve (37.9G held vs 22G actual RSS); the pytest RAM governor cannot delegate because confine wraps make, not pytest. Consider: nested --delegate-ram for gate-internal pytest, or tuning the whole-job estimate down, or documenting the trade. Design consideration, not committed. Raised 2026-08-27.


---

## Amendment — 2026-09-09 global rant triage

**Owner decision still owed: a headroom epsilon on the reserve→cap CONVERSION.**

RANT-2 asked for "possibly a headroom margin on the reserve-as-cap". That is *not* the 15 % at
`internal/runner/resource_estimate.go:12`, which is applied inside `EstimateMemoryReserve` to the
**estimate**, before the number ever becomes a reserve. The conversion itself carries zero margin:
`internal/runner/confine_linux.go:983-985` assigns `scopeMemoryMax = admission.reserve` byte-for-byte,
and `:1002-1007` writes exactly that as the kernel `memory.max`.

This is unbuilt **and** unconsidered — `grep -l "reserve-as-cap" .aira/tickets/*.md` returns nothing,
and AIRA-184's "Not designed here" weighed only auto-retry vs a sharper message. It is directly
implicated in RANT-29, where a pre-commit hook died **8192 bytes — two pages — over** a cap set equal
to its booked reserve.

Either answer is acceptable and should be recorded here:

- **Refuse it** — byte-equal `memory.max` IS the airtight property being sold, and a job that exceeds
  its booked reserve should die rather than over-commit the slice; or
- **Want it** — `memory.max = booked_reserve × (1+ε)` with the ledger still booking `reserve`. Test:
  a `confine_linux` assertion that `ScopeMemoryMax > admission.reserve` on the
  `ConfineCapSourceDaemonReserve` path, which fails against the current byte-equal assignment.

What is **not** acceptable is closing it as already-built. That was the first triage pass's error.
