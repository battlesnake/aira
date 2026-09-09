---
{"schema":1,"id":"AIRA-223","project":"aira","title":"AIRA_AITEST_ESTIMATED_BYTES silently ignores a size suffix and gives you the 512M default with no warning","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","footgun"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-224","to":"AIRA-223"}]}
---
From the `deploy` session, fastest-ee. Not found by being bitten — found reading your own guidance, which documents it as a known trap:

    "note it takes a PLAIN INTEGER BYTE COUNT, not a size suffix: AIRA_AITEST_ESTIMATED_BYTES=4294967296
     is 4 GiB, whereas a 4G-style value is silently ignored and you get the 512M default with NO
     warning (only an out-of-range integer warns)"

The asymmetry is the bug: an out-of-range INTEGER warns, but a well-formed-looking SIZE STRING does not. Those are the same user mistake and the one that looks more correct is the one that goes silent.

WHY IT MATTERS MORE THAN A TYPO. Every other size-taking surface in aira accepts suffixes — `--memory-reserve 8G`, `--memory-max 48G`, `--slice-ceiling`. A user who has typed `8G` at aira four times that day types it a fifth, and gets a 512M per-worker backstop on a suite they had just decided needs 4 GiB. The failure is not an error; it is a suite that runs with 8x too little headroom per worker, so workers get OOM-killed in their own sub-scopes, tests get requeued, and a second kill reports them `unevaluated`. That reads as flaky tests, not as a config mistake, and the actual cause is invisible.

fastest-ee already carries a whole shell script (`scripts/aitest_engine_estimated_bytes.sh`) whose reason for existing is to be the ONE home for this value and to validate it, precisely because a malformed read here degraded silently. That is a workaround for this defect living in a downstream repo.

SUGGESTED FIX: warn (or refuse) on a value that is not a plain integer, exactly as you already do for an out-of-range one. If accepting suffixes is cheap, accept them — but the silent path is the part worth removing either way.

## Build review record — DONE (2026-09-09)

Both asks taken: a new `_parse_estimated_bytes` (internal/pylib/aitest/__init__.py) accepts a
1024-based size suffix (K/KB/KiB … T/TB/TiB, case-insensitive, decimal mantissa floored) matching
Go `runner.parseMemorySize`, AND warns on a non-empty unparseable value instead of the silent 512M
fallback. Unset stays silent. Guidance (`internal/core/skill.go`) corrected in the same change;
`skill_test.go` pins the suffix support and forbids the stale "silently ignored" / "PLAIN INTEGER
BYTE COUNT" wording (AIRA-219-class contradiction avoided).

Tests (verifies: AIRA-223): parametrised suffix acceptance + decimal mantissa, a loud warning
naming the offending value on six malformed inputs, and unset-is-silent; RED before, green after.

Review: parser traced against the Go reference across every edge case (4G, 1.5G, 1GiB, 2gb, 0, 4X,
"4 G", 1.2.3, G, 0x10, 1024, -5) — matches, with malformed-non-empty and negative now warning
rather than silently defaulting (an improvement; the pre-existing "-5 → default" test stays green
because it asserts the value, not silence). A DeepSeek second opinion was requested but the sidecar
was unavailable (exit 4); recorded rather than blocked on, per the not-a-gate rule, with the
self-review standing in.
