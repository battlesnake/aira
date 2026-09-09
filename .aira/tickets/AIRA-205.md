---
{"schema":1,"id":"AIRA-205","project":"aira","title":"make fmt-check gofmts 8,681 .go files belonging to 24 other agents' worktrees, so any concurrent session's mid-edit file can fail the owner's commit","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

`make fmt-check` gofmts 8,681 `.go` files belonging to 24 other agents' worktrees, so any concurrent session's mid-edit file can fail the owner's commit
**kind** bug · **severity** P2 · **closes** critic §6.2 orphan (RANT-10 and RANT-17 reviewers, independently)

**SYMPTOM.** This is the exact AIRA-76 symptom — a commit failing on an unrelated session's file — reachable through a path AIRA-76 never touched. Measured by me today in the primary checkout: `ls -d .claude/worktrees/*/` = **24**; `.go` files under `.claude/` = **8,681**; total `.go` files the fmt-check glob feeds to `gofmt` = **9,222**. 94 % of the files the owner's pre-commit formats belong to other sessions. Reviewers measured the cost at ~9.7 s CPU / 172 MB per hook run, currently reporting zero unformatted files — so the exposure is confirmed and not presently firing.

**ROOT CAUSE.** `Makefile:21`:
```
@files="$$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './.worktrees/*')"; \
```
`./.claude/worktrees/*` is not excluded. That directory is hidden from git only by `.git/info/exclude`, a local exclude the Makefile's exclusion list never caught up with. `go vet ./...` and `go build ./...` correctly skip them (each nested worktree carries its own `go.mod`); only the find-based fmt-check reaches in.

**PROPOSED FIX.** Either add `-not -path './.claude/*'`, or — better, and the reason to prefer it — derive the file list from `git ls-files '*.go'`, so the gitignore/exclude set is the single oracle and the list cannot drift again the next time a tool plants a nested checkout.

**HOW TO TEST.** A fixture holding a deliberately unformatted `.go` file under `.claude/worktrees/<other>/` must not fail `make fmt-check`; a deliberately unformatted tracked file still must. The first fails against the current Makefile.

---

---

## Build correction — 2026-09-09

**The write path matters more than the read path this ticket described.** `fmt`
runs `gofmt -w` AND `goimports -w` over the same glob, so the defect was not
merely that `fmt-check` judged other sessions' files — `make fmt` was
**rewriting** 24 concurrently-running agents' in-progress files underneath them.

**Three sites, not one.** The expression was duplicated at `fmt`'s gofmt, `fmt`'s
goimports and `fmt-check`. Three copies drifting is precisely how `./.claude`
came to be excluded from none of them, so the fix folds them into a single
`GO_SRC_FIND` variable rather than editing three literals.

Exclusion is `./.claude/*` rather than `./.claude/worktrees/*`: everything under
that directory is harness state, never project source, and the broader form
needs no revisiting when the harness adds a sibling directory.

**Measured, root checkout:** 9,222 files selected before, 541 after — 94%, as
predicted. Note the reduction is invisible from a feature worktree, which has no
nested `.claude/worktrees`; it only shows in the checkout the agents nest under.

The test extracts the find expression from the real Makefile and RUNS it against
a fixture tree, because the defect was a path-matching one and asserting on the
Makefile's text would pass against an exclusion that does not actually match. It
asserts every expression it finds rather than a fixed count, so a future target
that hand-rolls its own find is caught too, and it fails on zero matches so a
renamed variable cannot read as a pass. Confirmed failing against the unfixed
Makefile on all three sites, with the vendor/.worktrees cases passing throughout
— i.e. the fixture proves the harness is not vacuous.
