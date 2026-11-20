---
{"schema":1,"id":"AIRA-366","project":"aira","title":"test: commands parity real-socket test cannot discriminate a daemon that drops the query argument (store holds a single command event)","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
Found by AIRA-362 mutation M7a: dropping Args in the daemon's routed path left TestCommandsRoutedDaemonParityAndPairedDrillRealSocket green. Add a non-matching second row so the test discriminates; show the mutation goes red.
