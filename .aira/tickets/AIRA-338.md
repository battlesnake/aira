---
{"schema":1,"id":"AIRA-338","project":"aira","title":"claim/heartbeat on an expired lease return bare E_LEASE_EXPIRED: name the holder and the remedy (--steal)","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.43","labels":[],"hold":true,"relations":[]}
---


Filed from the 2026-11-14 rant triage (see docs/plans/2026-11-14-backlog-plan.md). Surface: daemon. Size: S. Correctness-critical (two-loop): yes.

NOTE (plan): this touches the lease CAS return path (internal/store/lease.go:275-276 returns a bare sentinel inside the CAS). Build it as its own PR with Opus building and Fable reading that hunk specifically; if naming the holder cannot be done without widening the CAS, drop it to 'later' (the bare code is honest, only unhelpful).

## Problem
`aira claim X` without --steal on an expired held lease, including from the same worktree that held it, returns `{code:E_LEASE_EXPIRED, error:E_LEASE_EXPIRED}` with no holder, age or remedy (internal/store/lease.go:275-276). `aira heartbeat X` on a lapsed or freed lease does the same (lease.go:438,449,468). Agents cannot tell that `--steal` is the designed recovery (RANT-56).

## Evidence
lease.go:31 errors.New("E_LEASE_EXPIRED"); lease.go:275-276 bare return on !steal; core.go:1520-1530/1546-1560 pass it through; m3_test.go:132 pins the scenario.

## Minimum fix (from-scratch minimum)
Wrap the sentinel with remedy text, keeping the code: in Claim, `fmt.Errorf("%w: the lease on %s lapsed (holder worktree %s, actor %s%s); re-take it with `aira claim %s --steal` (journaled as lease.steal)", ErrLeaseExpired, ...)`, saying 'this worktree' when row.worktree_id == s.worktreeID. In Heartbeat, `%w: the lease on %s is not live (lapsed or released); re-take it with `aira claim %s` (add --steal if it is still recorded as held)`. ErrorCode() takes the text before ':' (check.go:810-822), so the code and exit are unchanged. Deliberately NOT included: letting a same-worktree caller re-take a lapsed lease without --steal (that would hide a lapse from the journal, and the spec makes steal the logged CAS), and any E_LEASE_HELD wording change.

## Tests
Store test: an expired lease claimed without --steal gives ErrorCode == E_LEASE_EXPIRED, errors.Is(err, ErrLeaseExpired), and a message containing '--steal' and the ticket id. The same-worktree case says 'this worktree'. Heartbeat on an expired lease and on a released one both carry the re-claim remedy. Existing m3_test.go:129-144 stay green.

## Files
- internal/store/lease.go
- internal/store/m3_test.go
