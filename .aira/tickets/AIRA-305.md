---
{"schema":1,"id":"AIRA-305","project":"aira","title":"Flaky: TestVRAMContendedJobIsHeldNotRefused reads the queue count before the saturated reply's queue is registered","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.35","labels":[],"hold":true,"relations":[]}
---


Seen once in the pre-push `make ci` of AIRA-304 (daemon package, 190 s run, box heavily loaded):
`vram_admission_test.go:165: a contended (not refused) VRAM job must ENQUEUE; 0 queue(s)`. The same
test passed 30/30 in isolation and in an earlier full `make ci` on the identical tree, so it is a
timing race: the test reads `len(server.admitQueues)` right after the non-blocking saturated reply
frame, but the queue entry can already be gone (or not yet registered) by then. Fix by asserting on
an event the server signals rather than sampling the registry after the reply.
