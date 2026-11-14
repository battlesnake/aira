---
{"schema":1,"id":"AIRA-331","project":"aira","title":"confine cold-start cap: size an unseen argv at max(1 GiB, coarse-key prediction) (owner decision, measured trade)","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":"v0.41","labels":["rant-triage"],"hold":true,"relations":[]}
---
From the AIRA-329 diagnosis (docs/dev/aira-329-cold-start-diagnosis.md). 4.7% of 19,899 cold first runs since 2026-10-20 are OOM-killed at the 1 GiB default (AIRA-304). Using the AIRA-323 key column, sizing an exact-argv-with-no-history job at max(1 GiB, max x 1.25 of the coarse-key history) plausibly avoids ~34% of those kills, at the cost of a larger cap (median 2.9x the real peak) for ~30% of cold runs. Re-opens AIRA-304: OWNER DECISION. Needs AIRA-323.
