---
{"schema":1,"id":"AIRA-9","project":"aira","title":"Racy daemon reaper test double-closes a channel under load (TestPeriodicReaperContinuesAfterSweepError)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":false,"relations":[]}
---
internal/daemon/server_test.go:774 reapScope closes 'second' on the 2nd call, but runReaper's 1ms ticker can fire a 3rd sweep before cancel() lands -> close of closed channel -> panic. Pre-existing (master); surfaced by a loaded make test during the AIRA-6/AIRA-2 gate. Fix: switch-guard so only call #2 closes.
