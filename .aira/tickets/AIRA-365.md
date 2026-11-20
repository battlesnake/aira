---
{"schema":1,"id":"AIRA-365","project":"aira","title":"flaky: internal/pylib TestRealPytestAitestParentKillLeavesNoOrphanPostRestart failed once under a contended slice ('process survived the parent kill ... pid reuse, or a real leak')","status":"done","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
Seen in the AIRA-361 builder's full gate on 2026-11-19 (exit 2); re-run alone with -count=3 passed, and the final gate passed. Decide: genuine race in the test's liveness check (poll until the process is gone, bounded) or a real leak. Known gap in the same area: a wrapper-only SIGKILL (AIRA-357 header).
