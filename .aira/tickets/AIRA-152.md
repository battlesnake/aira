---
{"schema":1,"id":"AIRA-152","project":"aira","title":"Candidate: make the OOM escalation a replacement rather than a max with the unpinned default","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F3**, with the cost analysis so a future session does not
re-derive it.

Making the escalation a REPLACEMENT rather than `max(estimate, 1.5x OOM peak)`
when no usable ordinary estimate exists would fix the sizing half of AIRA-149's
facet 1 exactly and remove most of AIRA-150's reachability.

**Cost.** In AIRA-149's measured example it sizes the next attempt at 84 MiB
instead of 4 GiB for a job that needs ~320 MiB, so the self-heal takes SEVERAL
MORE OOM KILLS to converge instead of one. The current `max()` encodes a
defensible rule — never size the next attempt below what an unpinned client
asked for — and replacing it trades operator-visible OOM kills for slice
efficiency.

It would also require restating AIRA-128's shipped claim ("the very next run
succeeds") as a convergence ladder and retuning its real-cgroup fixture
(`internal/daemon/confine_oom_selfheal_real_cgroup_linux_test.go`).

A real design question with a real cost either way; not a by-product of an
honesty fix.
