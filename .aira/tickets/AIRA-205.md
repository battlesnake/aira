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
