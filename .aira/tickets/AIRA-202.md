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

---

## Build correction — 2026-09-09

**This ticket's stated premise was half wrong, in both directions, and the
measured position is neither.** The body said the release binary already carries
`vcs.revision` so no build-flag work is needed. Verified by direct measurement:

| Build | `vcs.revision` |
|---|---|
| Installed `~/.local/bin/aira` | present (`4b751d3`, `vcs.time`, `vcs.modified=false`) |
| Release CI artifacts (fresh clone) | present |
| **Build made inside a linked git worktree** | **absent** |

Go does not record VCS information for a build whose `.git` is a FILE rather
than a directory — a linked worktree — and it omits it **silently**: no stamp,
no warning, and no error even under an explicit `-buildvcs=true` (confirmed,
exit 0 with no vcs entries, while `git rev-parse HEAD` in the same directory
resolves fine).

That is not a corner case here. CLAUDE.md requires all AIRA development to
happen in worktrees, so **the entire dev-build population is unstamped**. An
unestablished identity is therefore a first-class, explained answer rather than
an error condition, and `internal/buildid` names the worktree cause specifically
so a developer does not read it as a defect in the verb.

The `-ldflags -X` question is genuinely moot for the installed and released
binaries, as the ticket said — but not for the reason it gave, and not for dev
builds, where no flag can help.

**Corrections carried into the implementation:**
- The two-registry trap was real. `version` is intercepted beside `help` in
  `cmd/aira/main.go`, BEFORE `buildRequest` — whose own enumerated switch raises
  `E_UNKNOWN_VERB` before `core.Do` is reached. A dispatch-table entry alone
  would have left `aira version` broken while `--version` worked, so the test
  drives `version`, `--version` and `-v`.
- `version` also needed a `daemonDispatcher` routing arm. Without one it
  reproduced AIRA-201 exactly — the client arm opened a project store with the
  empty scope and refused `E_CONFIG_INVALID: scope options are incomplete`.
  Observed live before the arm was added.
- An OLD daemon self-identifies by not knowing the verb. That inference is now
  reported as such, worded as an inference, rather than surfacing a bare
  `E_CONFIG_INVALID` the reader cannot interpret.
- MCP `serverInfo`'s hardcoded `"m8a"` now reports the same identity.

**Accepted gap, stated rather than half-built:** `version` does NOT appear in
`aira help`, because it is intercepted before the dispatch table. Listing it
means a dispatch-table descriptor, which pulls in MCP tool generation and the
skill manifest golden (`cmd/aira/skill_test.go` pins 77 actions;
`internal/core/skill.go` hard-errors on a listed descriptor missing
Summary/Safety/Example). That is a real change with its own review surface and
was deliberately not bundled into this fix. `--version`/`-v` are conventional
enough for a human; an agent reading the generated guide will not find it.
Worth a follow-up decision.
