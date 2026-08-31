---
{"schema":1,"id":"AIRA-24","project":"aira","title":"Admission saturation-wait UX: queue position/ETA, faster-fail, clearer reject signal","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","dogfood","ux"],"hold":false,"relations":[]}
---
Reported by a dogfooding session (altium via subpipe) 2026-08-31: `aira confine --memory-max 32G --memory-reserve 512M -- make merge-gate` sat in admission-wait 1785s (~30 min) under genuine slice saturation (siblings ~40G/63G), then hard-rejected `E_ADMIT_SATURATED` (exit 1) with ZERO tests run — plus a "is it stuck/running/dead?" ambiguity because the job vanished from `aira confine --list` on reject with no distinct signal.

This is GENUINE reserve over-subscription (the slice was really full of sibling-gate anon), NOT the AIRA-21 cache-inflation false-stall — a distinct problem. The bounded-wait-then-honest-reject is BY DESIGN (#71 made it bounded vs infinite hang; #67 keeps Σreserve ≤ cap-headroom), so waiting itself is correct. The friction is the UX of a long blind wait + a silent hard reject:

1. NO visibility while waiting — the waiter can't see its queue position, the reserves ahead of it, or an ETA. #73 added a slice-reserve summary to `--list` ("why is it waiting"); extend it to surface the WAITER's own position / bytes-ahead / rough ETA (client-side periodic line already exists from #71: "waiting for memory admission (reserve X, waited Ns)" — add position/ahead).
2. 30 min is a long time to discover you won't get in. Consider a configurable / shorter faster-fail default, or an explicit `--admit-timeout`/`--no-wait` so a caller can choose fail-fast over a long hopeful wait.
3. Hard-reject signal is easy to miss — the job vanishes from `--list` with only exit 1 + the E_ code. Make the reject louder/distinct (it already exits 1 with E_ADMIT_SATURATED; the ask is a clearer terminal signal so an agent doesn't mistake reject for "still running").
4. Consider an admitted-with-backpressure mode (admit but throttle) as an alternative to head-of-line blocking for big reservations — heavier design, lower priority.

relates: #71 (admission hang diagnostic), #73 (--list reserve summary), #67 (reserve ledger), AIRA-4 (backfill). Owner-facing UX polish; not a correctness bug.
