---
{"schema":1,"id":"AIRA-68","project":"aira","title":"Admission ledger reserve leak: ~60GB granted across 23 \"admitted\" jobs, only 3 actually live","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["admission","daemon","dogfood"],"hold":false,"relations":[]}
---
## Symptom (found during the AIRA-67 investigation, independently re-verified live)

`aira confine --list` right now: `63138336K granted / 61952M ceiling across 23 admitted jobs` — but only 3 scopes actually exist and are live. This is worse than when first noticed minutes earlier (48GB/14 jobs), i.e. actively growing, not a one-off snapshot artifact.

## Why this matters, urgently

The slice is at ~99.5% of its entire ceiling committed to reserves that don't correspond to any real running job. This directly causes the admission saturation and `reserve-basis=fallback:daemon-unavailable` events several sessions hit tonight (including the job investigated in AIRA-67, immediately before it died) — real jobs with genuinely small requests cannot get admitted because the ledger believes the slice is nearly full, when the actual live footprint is a small fraction of that.

## Relationship to prior work

AIRA-49 (lease-TTL reclaim sweep) and AIRA-74 (restart reserve-ledger reconstruction) both addressed related but distinct failure modes — stuck leases past a TTL, and losing the ledger across a daemon restart. This is a separate, still-open leak: reserves that were legitimately granted are not being released when their owning job actually exits, accumulating over the daemon's uptime rather than at restart time or via TTL expiry.

## Suggested direction (not investigated in depth — flagging for the two-loop, not prescribing)

Likely candidates worth checking first: whether `releaseAdmitWaiter`/the outstanding-reserve accounting has a path where a job's exit is never observed (e.g. a connection close that isn't detected, or a release RPC that's dropped/never sent — note AIRA-67 found related silent-failure modes on the confine-kill dispatch path, which may or may not be connected here); whether the AIRA-74 "adopted" reconstruction only runs at daemon start and never re-validates itself against still-current live state during normal uptime; and whether any of tonight's own dogfooding (many confine jobs launched and killed by build agents, including some that hit connection/auth interruptions) is what's actually populating this specific instance of the leak, as opposed to it being a rare, hard-to-trigger path.

A live restart will very likely clear the immediate leak (reconstruction rebuilds from actual live scopes, so ghost reserves with no backing scope won't be recounted) — but that's a mitigation, not a fix, since whatever causes reserves to leak during normal uptime will presumably resume leaking afterward.
