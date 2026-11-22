---
{"schema":1,"id":"AIRA-372","project":"aira","title":"aitest: consecutive-chunk batching, real nextitem inside a chunk so shared fixtures are set up once per chunk (RANT-156 slice 1; default off, A/B before flipping)","status":"in-review","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
Source: fastest-ee 'speed' session design hand-over (RANT-156, relayed 2026-11-22; design note /home/mark/tmp/aitest-batching/2026-11-22-aitest-fixture-batching-design.md), reduced by the challenge pass recorded in the spec this ticket lands (docs/superpowers/specs/2026-11-22-aitest-fixture-batching-design.md).

Problem: worker.run_one passes nextitem=None, so every test tears down all shared (module/class/session) fixtures. On fastest-ee's engine suite module-fixture self-time is 4,556 of 27,468 test-seconds (17%).

Slice 1 (the greenfield minimum, about 100 lines of product code): the supervisor cuts the final post-modifyitems item order into chunks of consecutive items (cap AIRA_AITEST_BATCH_MAX_TESTS, 1 = off = byte-identical to today; uniform (need, cpu, time, failfast) tuple per chunk; module-aware cut), dispatches a chunk to one worker, the worker runs member i with nextitem = member i+1 and the last with None; pytest's own SetupState decides what stays alive. Retirement is decided BEFORE a member runs (existing _should_recycle) and that member runs with nextitem=None, so no out-of-protocol teardown exists. Per-item acks unchanged; a crash requeues the unstarted rest as single items and a recycle requeues it as one chunk, neither spending attempts. No sharing key, no history, no new markers, no statm watermark.

Land first: the spec (plan gate), then the build. Deferred pieces are in the held follow-up ticket.
