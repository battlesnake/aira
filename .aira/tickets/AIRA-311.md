---
{"schema":1,"id":"AIRA-311","project":"aira","title":"confine: shorter admission-wait line with job name + front command; actionable over-reserve notice","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.38","labels":[],"hold":false,"relations":[]}
---
Owner 2026-10-24. (1) The over-reserve notice (AIRA-310) is ignored by agents: reword as an imperative ACTION REQUIRED naming the fix. (2) The queued-wait line is too long and omits the job name: shorten, name the job, and print the minimal command a human runs to push it to the front (aira confine --front <pid> --steal).
