---
{"schema":1,"id":"AIRA-157","project":"aira","title":"Accepted asymmetry: the post-block insufficient-samples fallback carries no sample count","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","confine","honesty"],"hold":false,"relations":[]}
---

AIRA-149 deferral **F8**, filed as a RECORDED decision, pinned green by a test.

After AIRA-149 the same 1-2 sample signature reports two different labels:

    with an OOM record   fallback:insufficient-samples:n=1,oom-on-record
    without one          fallback:insufficient-samples          (bare, no n=)

because the first goes through `resolveAdmitReserve`'s history-block `basis`
local and the second exits at a POST-BLOCK `return` with its own literal.

Accepted rather than fixed: the bare label is IMPRECISE but NOT FALSE — the
reason really was insufficient samples — and threading a sample count through
four more `return` statements is plumbing the architectural-simplicity rule
refuses for no honesty gain.

The boundary is executable, not merely documented:
`TestPostBlockInsufficientSamplesFallbackIsUnchanged`
(`internal/daemon/admit_oom_basis_test.go`) pins the bare label, so a later
"tidy-up" that threads the local through the post-block returns fails a test
instead of silently changing a fourth operator-facing string.
