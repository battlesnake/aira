---
{"schema":1,"id":"AIRA-302","project":"aira","title":"aira confine --kill <own named job> refused E_CONFINE_OWNER_UNVERIFIED with owner=@cwd-X caller=@cwd-X (dogfood friction from v0.34 review)","status":"retired","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":[],"hold":true,"relations":[]}
---

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** retired. **State on master v0.34:** still-open.

The refusal is the intended ownership-guard behaviour from AIRA-23. Relaxing it would reintroduce the cross-session kill hazard, so the behaviour must not change, and "fixing" it would be a correctness regression. The real cause of the friction is that the dogfooding session had no AIRA_CONFINE_OWNER set, which the skill text already documents. Close as wontfix (working as designed). If the owner wants the message friendlier, that is a one-line optional extension of the existing error string, not a mechanism.

**Evidence:** The ticket file (.aira/tickets/AIRA-302.md) has only a title and no body; no commit mentions AIRA-302. The refusal is the designed AIRA-23/AIRA-52 behaviour, not a defect. internal/runner/confine_manage_linux.go:729 (and confine_shim_manage.go:133) refuses unless BOTH the scope owner and the caller are attested and equal. ConfineOwnerIsAttested (internal/runner/confine.go:823) is false for any '@'-prefixed inferred owner, so owner=@cwd-X caller=@cwd-X is refused on purpose. The rationale is at confine.go:785-795 and confine_manage_linux.go:717-726: two sessions sharing one directory infer the same string, so honouring it would let either kill the other's job without --steal. AIRA-23.md "Resolution" says "An inferred owner does not open the guard even against an identical caller identity." internal/core/skill.go:320 already tells agents that an '@'-owner "never counts as ownership, so killing such a job always needs --steal even from the same directory. Set AIRA_CONFINE_OWNER to get a real, attested owner". Only the error text is unexplained: it says "pass --steal to override" and never says why identical owners were refused.
