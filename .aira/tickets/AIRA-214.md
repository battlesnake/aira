---
{"schema":1,"id":"AIRA-214","project":"aira","title":"confine's management verbs print a human ASCII table to a pipe, so aira confine --list | json.load raises","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

confine's management verbs print a human ASCII table to a pipe, so `aira confine --list | json.load` raises
**kind** bug · **severity** P3 · **closes** critic §6.1 orphan (RANT-4 remainder), carries RANT-35's doc clause

**SYMPTOM.** Confirmed live by me today: `aira confine --list` piped to a non-TTY without `--json` emits `NAME  OWNER  SUPERVISOR-PID  SCOPE-ID  LIVE  LEAF-PROCS  RSS  AGE  RESERVE  CAP …`. Every other verb piped emits the JSON envelope since AIRA-57, whose own owner direction says "piped, redirected, or not a TTY at all — the common case for an agent invoking this CLI via a subprocess — default to JSON". AIRA-57's ticket carries no deferral for confine, so this is an unrecorded incompleteness, not an accepted scope cut.

**ROOT CAUSE.** `cmd/aira/main.go:213-221` passes the explicit `jsonOutput` — not `main.go:109`'s TTY-aware `renderJSON` — into the confine status/management commands; `main.go:287-294` does the same for the hyphenated `confine-list`/`-kill`/`-budget`; `dispatchConfineManagementRequest` selects the human renderers on `!jsonOutput` at `main.go:2161` and `:2164`. The exclusion is justified by the comment at `main.go:105-108` — "confine and friends (which reject `--json` outright)" — which is **false**: `main.go:213`'s guard is `jsonOutput && !management`, so every management form accepts `--json`, and `internal/core/core.go:1942` documents `--json` in confine-list's own usage.

**PROPOSED FIX.** Thread `renderJSON` (not `jsonOutput`) into the confine-list / confine-budget / confine `--status` render decision, so a pipe gets the envelope and a terminal keeps the table. Leave the launcher form alone (it genuinely rejects `--json`) and leave `confine-log`/`confine-input` alone (they inherit run-log's deliberate byte-transparency contract: stdout = raw bytes, stderr = JSON metadata). Correct the `main.go:105-108` comment to name only the forms that really reject `--json`. **Fold in RANT-35's one doc clause:** `internal/core/skill.go:318` steers a `--status` poller to "read the printed `exit=`" — i.e. into a grep-and-parse dance — and never mentions that `--status --json` returns `state`, `exit` and `error_code` as fields. One clause removes most of that friction with no new machinery.

**HOW TO TEST.** A CLI-level test invoking `confine --list` with stdout as a `*bytes.Buffer` (never a terminal) and no `--json` against a stub dispatcher returning a populated `ConfineListResult`, asserting `json.Unmarshal` of stdout yields `Code "OK"`; fails today because the first byte is `N`. Mirror for `confine-budget`. A companion negative test calling `renderConfineListResponse` directly (as `cmd/aira/confine_test.go:303` already does) keeps the table for the human direction, so the fix cannot be faked by deleting the renderer.

---
