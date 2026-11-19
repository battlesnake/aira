---
{"schema":1,"id":"AIRA-364","project":"aira","title":"flaky: TestLiveTeeCancelDuringNormalJoinKeepsCaptureComplete fixed 50 ms sleep fails under load (internal/runner/livetee_test.go:593)","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.46","labels":["backlog"],"hold":false,"relations":[]}
---
Failed once under load in the 2026-11-19 gate. Replace the fixed sleep with an event/condition the test can wait on; show by mutation that the test still fails against a wrong implementation.
