---
{"schema":1,"id":"AIRA-371","project":"aira","title":"admission: shortest-expected-job-first order among queued waiters that fit (from per-command wall_ms history), with aging; DESIGN FIRST, measure after the lane acceptance screen","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":"v0.47","labels":["backlog"],"hold":true,"relations":[]}
---
Owner 2026-11-22: 'shortest job first for the CPU queue: yes, if you think it will improve things.'

## Judgement (coordinator)
Likely helps mean wait (classic result) when the queue is long and expected runtimes differ a lot, and the daemon already has the input: confine-report stores wall_ms per command signature (AIRA-323). BUT:
1. HOLD until the small-job lane 7-day acceptance screen (due about 2026-11-26) is read. Admission order is what that screen measures (baselines 63.9 s overall, 49.7 s small, 113.3 s large wait per job); changing order now confounds it. The lane is already a size-based shortest-first approximation, so the SJF gain over it is unmeasured.
2. The queue today is held by the RAM booking ledger, not CPU slots (see AIRA-370), so 'CPU queue' really means the single admission queue; SJF orders waiters that fit, it does not create capacity.
3. Needs: a duration estimate per waiter (history by signature; unknown = median, never 0); AGING so a long job cannot starve (the existing head budget, freeze hold and AIRA-307/309 rules already encode fairness and must stay the outer rule); pushed jobs still go first; sub-reservations (aitest worker claims) stay exempt.
4. Measure first, in the style of AIRA-350: replay the recorded queue (arrival, reserve, wall_ms) offline against FIFO, lane-only and lane+SJF and report mean and p95 wait; build only if the replay shows a real gain (say >= 15% mean wait with p95 for large jobs not worse than +50%).
Do not build until the owner clears the hold after the acceptance screen.
