---
{"schema":1,"id":"AIRA-202","project":"aira","title":"Nothing reports which commit the running aira client or daemon was built from, and MCP serverInfo answers with a hardcoded stale \"m8a\"","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-203","to":"AIRA-202"}]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

Nothing reports which commit the running `aira` client or daemon was built from, and MCP `serverInfo` answers with a hardcoded stale "m8a"
**kind** feature · **severity** P2 · **closes** RANT-14, RANT-25 (half), RANT-23 (half)

**SYMPTOM.** `aira version` and `aira --version` both exit 2 with `E_UNKNOWN_VERB`; there is no `-v`. The one place aira answers a version question answers it wrongly: `cmd/aira/mcp.go:340` returns `"serverInfo": {"name":"aira","version":"m8a"}` — a Milestone-8a label frozen since 2026‑08‑09, roughly 1077 commits and three release tags ago. That is a fabricated confident answer where the CLI at least errors honestly. Operationally this is the corpus's most expensive single gap: RANT-20, RANT-22, RANT-23 and RANT-25 were wholly or partly artifacts of an unlabelled stale binary, and RANT-29 and RANT-19 each reported a message master already carried.

**ROOT CAUSE.**
- No `version` verb in `internal/core/core.go`'s dispatch table, **and** — the part the earlier draft got wrong — the CLI never consults that table for an unknown verb: `cmd/aira/buildRequest` (`cmd/aira/main.go:2400`) carries its own independently-enumerated ~40-arm switch whose `default` at `main.go:3085` raises `E_UNKNOWN_VERB` before `core.Do` is reached. **Registering the verb in the dispatch table alone leaves `aira version` broken.**
- `cmd/aira/main.go:132` pre-dispatches only `help`/`--help`, so `--version` is parsed as a verb.
- The data already exists and needs no build-flag change: a `make dist` binary built with the release `-trimpath -ldflags "-s -w"` still carries `vcs.revision`, `vcs.time`, `vcs.modified` and a `+dirty` pseudo-version, readable via `runtime/debug.ReadBuildInfo()`. **Do not carry the "there is no symbol for `-X` to target, so the fix is larger" framing** — it is backwards; the missing thing is a reporting surface.
- The stale component that actually decides semantics is usually the **daemon**: `create`/`show`/`link`/`rant` default to RouteDaemon and execute inside it, so the daemon's compiled-in `internal/domain` decides whether e.g. P3 is legal. The only staleness detector on that path is the `ProtocolVersion` handshake (`internal/daemon/protocol.go:83`, currently 9), bumped only for frame-shape changes — a pure semantic change like AIRA-170 is invisible to it by construction.

**PROPOSED FIX.** A read-only `version` verb reporting, for the client, `vcs.revision`/`vcs.time`/`vcs.modified`, and for the daemon the same three over the existing request path, with the two marked as diverged when they differ. Report `unevaluated` — never a placeholder, never a fabricated tag — when `ReadBuildInfo` returns `ok=false`. Intercept `--version`/`-v` alongside `--help` at `main.go:132` (cheaper than a `buildRequest` arm: version resolves no project and needs no store) **or** add the `buildRequest` case — state which. Replace the `"m8a"` literal at `mcp.go:340` with the same value so the faces cannot disagree again. An optional `-X` release-tag stamp is a separate, smaller question; do not make it a precondition.

**HOW TO TEST.** (1) `Core.Do{Verb:"version"}` returns OK with the test binary's own vcs stamp; fails today with E_UNKNOWN_VERB. (2) A `cmd/aira` test that `aira version` **and** `aira --version` both exit 0 — the two-registry trap means a fix can pass one and fail the other. (3) With build info absent, the report is the literal `unevaluated`, not a placeholder. (4) `mcp_test.go`: `serverInfo["version"]` equals the same value, not a literal; fails today. (5) **Builder warning:** `cmd/aira/skill_test.go:206` pins 77 actions and `internal/core/skill.go:113-118` hard-errors on a listed descriptor missing Summary/Safety/Example — decide and state whether `version` is `Include:true` (golden → 78, full metadata required) or `Include:false` like `install`, or `aira skill install` fails at runtime rather than a test failing at build.

---
