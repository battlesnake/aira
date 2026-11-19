---
{"schema":1,"id":"AIRA-358","project":"aira","title":"test infra: safe-now speed batch (one-scan ticket-files test 70s to 1s, testdeadline.Wait negative waits, wrong-string runner helper, pylib reap timeout, fts seed in one txn, busy_timeout, hermetic docker) each mutation-checked","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
From the 2026-11-19 test-suite audit; synthesis and raw data in ~/tmp/aira-testaudit-out/ (worktree ~/tmp/aira-testaudit). Timing changes must show by mutation or negative control that the test still fails against a wrong implementation.
