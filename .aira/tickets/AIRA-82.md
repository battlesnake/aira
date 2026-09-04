---
{"schema":1,"id":"AIRA-82","project":"aira","title":"Rants filed from a feature worktree are misattributed to master's git context","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","honesty","rant"],"hold":false,"relations":[]}
---
Found live while dogfooding during AIRA-71's fix (RANT-18, filed from worktree `/home/mark/claude/aira-aitest-skill`). Its recorded `git_context` reads `worktree_path=/home/mark/claude/aira`, `head_ref=refs/heads/master`, `head_hash=9a65d47` — the shared root, not the actual worktree the rant was filed from. Same "confidently wrong recorded metadata" class as AIRA-71 itself (a mechanism reporting something specific and provably incorrect rather than failing honestly). Not investigated further — needs tracing to wherever rant capture resolves its git context (likely resolving from the daemon's own process cwd/registry entry rather than the calling client's actual worktree) and a fix so a rant's recorded provenance matches where it was actually filed from.

## Re-scoped (2026-09-04, backlog-remediation Phase 0, plan section 2) — text only, the fix is Phase 2

**"Confidently wrong recorded metadata" is the wrong frame, and this correction
is the point of the re-scope: nothing fabricates anything.** The daemon records
faithfully what the FACE handed it.

Verified against source: `stampGitContext` (`cmd/aira/dispatcher.go:691`)
resolves the git context from the `daemon.WorktreeScope` it is given, and that
scope comes from `scopeForCWD` -> `app.Discover(ctx, cwd)` — the *calling
process's* working directory. For a CLI invocation inside a worktree that is
correct. For a long-lived face whose process cwd is not the caller's worktree —
the MCP server being the obvious one — every request carries that process's cwd,
so a rant filed "from" a feature worktree is recorded against whatever directory
the face happens to live in. The mechanism is a missing per-call scope, not a
fabricated one.

**The real fix therefore moves to Phase 2** (plan section 4): an explicit
per-call scope override on the MCP/CLI faces. It touches face-layer code, not a
pure deletion, which is why it is not in this phase.

**Second candidate mechanism, worth ruling out first when that fix is built.**
AIRA-93 (landed in this same PR) found that `app.Discover`'s git helper inherited
`GIT_DIR` from its environment, and an inherited `GIT_DIR` OVERRIDES the explicit
`git -C <dir>` — resolving a *different repository* than the directory asked
about. That produces exactly this ticket's symptom (`worktree_path=` the shared
root, `head_ref=refs/heads/master`, filed from a feature worktree) and it is
demonstrated by a mutation test in `internal/app/git_env_test.go`. That path is
now scrubbed, so whoever builds the Phase 2 fix should re-check whether RANT-18's
misattribution is still reproducible at all before designing around the scope
override alone.

## Built (2026-09-05, backlog-remediation Phase 2, plan section 4)

**Second mechanism ruled out first, as the re-scope asked.** `runGitRevParse`
(`internal/app/project.go:768`) now scrubs `GIT_*` before every `git -C <dir>
rev-parse`, so the AIRA-93 environment-inheritance path can no longer resolve a
different repository. The remaining mechanism is the one named above and it is
still fully reproducible from source: the MCP face resolved every request's
scope with `scopeForCWD(ctx, ".", paths)` (`cmd/aira/mcp_project.go:63`), where
`"."` is the MCP *server process's* working directory. An MCP request carries no
directory of its own, so a long-lived server launched in one directory scopes —
and git-stamps — every call against that directory regardless of where the
caller is working. Reproduced as a regression test, not asserted from the ticket:
`TestMCPScopeDirOverridesTheFaceProcessWorkingDirectory` drives the real MCP face
with the server cwd in one worktree and asserts the wire frame's scope and
stamped `worktree_path`/`head_hash`.

**Fix: one explicit per-call scope override, identical on both faces.**

- CLI: `aira --scope-dir <dir> <verb> ...` (also `--scope-dir=<dir>`).
- MCP: a `scope_dir` string argument on every project-scoped tool.

It overrides the INPUT to discovery, never its result — project identity,
worktree identity and git context are still read from git and `.aira/config`
under that directory — so the override cannot name a project that is not really
there. Design notes live in `cmd/aira/scope_dir.go`.

Honesty properties, each with its own test:

- An unusable override (missing path, or a file) is **refused** with the path it
  was given, never silently replaced by the process cwd — a silent fallback
  would reproduce this very misattribution.
- A verb/tool that resolves **no** project scope (`confine*`, `governor-slot`,
  `aitest-bootstrap`, `worker-admit`, `help`; MCP `aira_eject`,
  `aira_confine_list`, `aira_confine_kill`) **refuses** the override rather than
  accepting and discarding it, and the project-less MCP tools do not advertise
  it at all.
- `scope_dir` is a face argument and never enters `core.Request`, so CLI/MCP
  request parity is unchanged (asserted directly, plus the existing skill-parity
  tests now assert no example decodes an override).
- The CLI resolves a directory at four sites (general scope, `init` bootstrap,
  `eject` default project, `init` path relativisation) and MCP at three; every
  one honours the override — each is pinned by a mutation.

One deliberate asymmetry between the faces, because the faces are asymmetric: a
**relative** value is accepted on the CLI (the process cwd IS the caller's own
directory, so `../sibling` means what the caller thinks) and **refused on MCP**
(the caller cannot see the server's cwd — that is the whole defect — so a
relative override would resolve against the very directory it is trying to
escape). Likewise `eject` accepts the override on the CLI, which defaults its
project selector from a discovered project, and not on MCP, which performs no
discovery for it.

**Build review** (independent, DeepSeek v4-pro, verdict BLOCK with 6 findings;
Gemini unavailable — free-tier daily quota exhausted). Three findings were
confirmed against source and fixed, each with a test and a mutation:

1. **A relative `scope_dir` on MCP was accepted and resolved against the server's
   own cwd** — the ticket's own failure class re-entering through the fix.
   Now refused; MCP takes absolute paths only.
2. **`import` with a relative `file` plus an override read the FACE's copy of
   that file and filed it against ANOTHER project** — a silently wrong file, not
   merely a wrong scope. This ambiguity is created by the override, so it is now
   refused on MCP (`refuseAmbiguousImportPath`) and still allowed on the CLI,
   where "this file here, into that project" is unambiguous.
3. **`--scope-dir --json ls` swallowed `--json` as the directory value.** Now an
   option-like next token is reported as a missing value, matching the rule
   `parseArgs` already applies to every other option; `--scope-dir=<value>`
   remains available for a directory that really starts with `--`.

Three findings were examined and **not** actioned, with reasons:

4. *"MCP should REQUIRE `scope_dir` rather than defaulting to the server cwd."*
   Rejected as a contract change beyond this ticket: it would break every
   existing MCP caller and the common single-repo case where the server cwd is
   the project. **Accepted, recorded limitation:** an agent that does not pass
   `scope_dir` still gets the old behaviour — this is an opt-in override, not a
   mandate. If dogfooding shows agents routinely omit it, making it required (or
   defaulting it from a launch-time environment variable) is the escalation.
5. *"Stopping the strip at a bare `--` for all verbs diverges from `removeJSON`'s
   verb-keyed carve-out."* True but produces no behaviour change: a bare `--`
   already errors in `parseArgs` for every verb that has no delimiter, and
   mirroring the verb-keyed form would break when the flag PRECEDES the verb —
   which is exactly mutation M8's failure.
6. *"TOCTOU between `os.Stat` and use."* Real but immaterial: the `Stat` only
   improves the error message; a directory that vanishes afterwards fails
   honestly at `git -C <dir> rev-parse`. It is not a security gate.

**Also pre-existing, not introduced here:** `aira mcp`/`daemon`/`skill` parse no
argv at all, so a `--scope-dir` passed to those subcommands is ignored — the same
is already true of `--json`.

**Mutation testing: 16 mutations, all killed** (three rounds). The first round
found two genuinely porous areas — the strip-order claim and the `init`/`eject`
discovery sites had no covering test at all — both closed before review.
