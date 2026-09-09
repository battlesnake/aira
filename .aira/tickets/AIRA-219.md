---
{"schema":1,"id":"AIRA-219","project":"aira","title":"AIRA's own guidance contradicts itself on nested confine: skill.go's \"MUST confine\" has no already-in-a-scope carve-out, while the AIRA-187 runtime warning tells you to drop the inner wrapper","status":"planned","kind":"chore","severity":"P3","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

AIRA's own guidance contradicts itself on nested confine: `skill.go`'s "MUST confine" has no already-in-a-scope carve-out, while the AIRA-187 runtime warning tells you to drop the inner wrapper
**kind** chore · **severity** P3 · **closes** RANT-31

**SYMPTOM.** An agent following AIRA's agent guide wraps `make <gate>` in `aira confine` and — because the guide says every memory-heavy command MUST be run under `aira confine`, with no exception for a command already inside a scope — also wraps each heavy recipe line. The inner call is a **sibling** scope (`internal/runner/cgroup_linux.go:192` joins `<slicePath>/.aira-<id>`, and `confine_linux.go:515` binds the backend to the resolved slice) requesting independent admission behind the parent's still-held grant. Under contention it can burn its whole admission budget (30 min by default, `internal/runner/confine.go:58`, applied at `confine_linux.go:1765-1769`) and die `E_ADMIT_SATURATED` with `ran=no`, having executed nothing, while the parent blocks on it holding the reserve.

**ROOT CAUSE (wording, not mechanism).** `internal/core/skill.go:318` says any memory-heavy command MUST be run under `aira confine`, with no nesting carve-out anywhere in the file — its one nesting-adjacent sentence points the *right* way (`aira confine --delegate-ram -- make <gate>` covers the pytest legs it spawns, i.e. wrap once at the outside). `cmd/aira/confine_nested.go:24` ships the remedy "drop the inner wrapper and let the child inherit the parent's scope by ordinary fork/exec" — correct (cgroup accounting is hierarchical, so nothing is left unconfined) but reading as a contradiction to anyone who has only read the rule. `internal/daemon/admit.go:344-346` states the collision in AIRA's own voice. And the warning itself (`confine_nested.go:57-62`) says only that the nested launch "queues independently", never the sharper fact — that the parent's own held grant is part of the headroom the child must fit under — and names no budget.

**PROPOSED FIX (text only; no behaviour change).** Add one clause to `skill.go:318`: a command already running inside a confine scope is already confined and capped by that scope — wrap once, at the outermost heavy command; if a nested wrapper is genuinely wanted, bound it with `--admit-timeout`, which accepts anything down to 1 ms today. Extend the AIRA-187 warning to name (a) that the parent's own granted reserve is part of the headroom this request must fit under and (b) the budget in force, rendered from `runner.DefaultConfineAdmissionWait` or the caller's `--admit-timeout` rather than restated as a literal (the drift AIRA-185 removed for `drain wait`).

**HOW TO TEST.** `cmd/aira/confine_nested_test.go` already asserts on the returned line: assert `nestedConfineWarning` mentions the parent's grant consuming shared headroom **and** contains `runner.DefaultConfineAdmissionWait.String()` — both fail today. Keep the existing exemption tests green (empty parent scope id, `--exclusive`) so the exemption is not widened. Add a skill-text assertion that the confine section states the carve-out; fails today, since no such string exists.

**Explicitly not proposed** — see §6.5.

---

