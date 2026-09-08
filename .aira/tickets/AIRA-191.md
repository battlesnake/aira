---
{"schema":1,"id":"AIRA-191","project":"aira","title":"confine --list has no per-scope reserve field -- only cap, which does not sum to the slice's own granted total and misleads under --delegate-ram","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":null,"labels":["confine","observability"],"hold":false,"relations":[]}
---

Peer report (ems, 2026-09-08/09), verified from source.

## Verified from source

Per-scope, `confine --list --json` exposes only `Cap` (`internal/runner/
confine_manage.go:50`, `memory.max` read live from the cgroup, i.e. the
hard ceiling) and `RSSBytes` (current usage) -- no per-scope granted/
reserve field. `GrantedBytes` (`confine_manage.go:189`) exists only as a
SLICE-WIDE aggregate on `ConfineSliceReserve`, with an explicit comment
that it and `Jobs` are "TOTALS over three structurally different
categories" (`:225`) -- there is no reverse mapping from that total back
to which scope holds how much of it.

## The gap this creates

Reported measurement: 83.2 GiB summed across scope `cap` values against a
50.2 GiB slice-wide `granted_bytes` -- caps and granted reserves are
genuinely different numbers (a `--delegate-ram` scope's cap is not its
charged reserve), so when the slice IS over-granted, nothing in the
listing lets an operator attribute the pressure to a specific job; `cap`
is the only per-scope number available and it is exactly the field that
misleads under `--delegate-ram`. Reporter's own concrete incident: read
another session's 45 GiB `cap` as their held reserve, when their actual
pinned reserve was 512M.

## Not designed here

Whether a per-scope `reserve_bytes` field is cheap to compute and expose
(the admission ledger already tracks a charged reserve per admitted
waiter/job server-side -- whether that number is already available at
listing time or needs a new round trip is unverified here) is left for
whoever picks this up. Not built, not investigated beyond confirming the
field is genuinely absent.

## Related

Confirms (independently, via ems relaying split's finding) the same
nested-confine-competes-with-its-own-parent shape already tracked as
[[AIRA-187]] -- no new ticket for that half.
