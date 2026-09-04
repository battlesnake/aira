---
{"schema":1,"id":"AIRA-88","project":"aira","title":"Three machine-local stores grow without bound: registry.jsonl, common/aira/locks/, and the pylib extraction directories","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["housekeeping","pylib","store"],"hold":false,"relations":[]}
---
PR #12 finding **B13** / plan candidate **77**, filed by the simplification programme's
Phase 0 (plan §4.3). Source-verified against master `22cedd6`.

Suggested severity in the plan is **P3** ("noticed in a year", none dangerous); this
repository's ticket schema has no P3, so it is filed P2 at the bottom of the P2 band.

## The three sites

1. **`registry.jsonl`** — append-only, never compacted. Written beside `state.db`
   (`internal/store/store.go:382`, `internal/app/project.go:232`/`:327`,
   `internal/daemon/paths.go:284`).
2. **`<common>/aira/locks/`** — `internal/store/store.go:2915` names a lock file
   `path-<digest of triple>.lock`, one per distinct path triple, created on demand
   (`store.go:593` makes the directory) and **never removed**. The file count grows with the
   number of distinct paths ever locked, not with the number currently in use. Two fixed
   siblings (`finding-rebuild.lock`, `requirement-rebuild.lock`) are bounded and fine.
3. **`pylib/<content-hash>/` extraction directories** — `internal/pylib/extract.go:32
   ExtractPyLib` and `:52 ExtractAitest` publish the embedded sidecar under a content hash.
   Every binary version that has ever run on the machine leaves its own tree behind.

## Why file it rather than fix it in passing

None of the three is dangerous, and none has a failure mode beyond disk use and directory
noise — which is exactly why it needs a ticket instead of a drive-by: a pruner that deletes
the *wrong* lock file or the *live* extraction directory is a real bug in service of a
cosmetic gain. The pylib pruner in particular must not delete the tree a concurrently
starting process is about to exec from, and the lock pruner must not remove a file another
worktree holds — on a machine that habitually runs several agent sessions at once.

## Direction

One ticket, one fix, one pass of real rigor: bound each of the three with a pruner that proves
the thing it deletes is dead (as the AIRA-72-era orphaned-scope reaper does — positive proof
of every facet, not an age heuristic alone), or record an explicit decision that a given one
stays unbounded.

Rigor: not Tier C. The plan lists candidate 77 in its mechanical Phase 0 sweep on the strength
of it being small; on inspection the deletions are concurrency-sensitive, so the sweep filed
this ticket instead of implementing it.

## Resolution (2026-09-04, backlog-remediation Phase 0, plan §2) — no code, three explicit decisions

This ticket's own Direction offered two acceptable outcomes per site: a pruner
that proves what it deletes is dead, **or** "record an explicit decision that a
given one stays unbounded". All three take the second, and the reason is the one
the ticket itself gives: each pruner is concurrency-sensitive on a machine that
habitually runs several agent sessions at once, and none of the three has a
failure mode beyond disk use. A pruner that deletes the *live* extraction
directory or another worktree's held lock file is a real bug bought for a
cosmetic gain.

Measured on this machine, read-only, 2026-09-04 — so these are decisions against
numbers, not against an assumption:

### Site 1 — `registry.jsonl`: stays append-only. No compaction.

**131 lines / 39 KB**, accumulated over the project's whole life. At that rate the
file needs no bound this decade, and compaction of an append-only registry is
exactly the kind of rewrite that races other sessions.

### Site 2 — `<common>/aira/locks/`: stays unbounded. No pruner.

**113 files / 24 KB** in this repository's common directory. The count grows with
the number of *distinct paths ever locked*, not with concurrent use, and each
file is empty — 24 KB is the directory's own block overhead, not content. A
pruner here has the worst risk/benefit of the three: it must prove no other
worktree holds the lock, on a shared machine, to reclaim kilobytes.

### Site 3 — `pylib/<content-hash>/`: closes as a consequence of AIRA-66, and re-measured.

**Before:** 95 directories / 27 MB, spanning 2026-08-20 to 2026-09-04, against
**14 commits that changed the embedded Python sources in that same window.** The
excess is what AIRA-66 predicted: `go:embed all:` included
`aitest/.pytest_cache/v/cache/{nodeids,lastfailed}`, which change on *every*
pytest run, so the content hash — and therefore the published directory — churned
with test activity rather than with source content. (Evidence, not proof:
concurrent worktrees on different branches also produce distinct content states.
But 95 against 14 is not explained by branch variation alone.)

**After AIRA-66:** the embedded tree is exactly the ten tracked `*.py` sources, so
the digest changes only when those sources change. The store stays unbounded by
design; what is removed is the driver that made it grow for non-reasons. Stated
honestly rather than as a fix: existing directories are not reclaimed by this
change, and nothing prunes them — the growth *rate* collapses to the rate of real
source changes, which is the bound this ticket needed.

AIRA-88 -> done. No code in this ticket; the only code involved is AIRA-66's.
