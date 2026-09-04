---
{"schema":1,"id":"AIRA-88","project":"aira","title":"Three machine-local stores grow without bound: registry.jsonl, common/aira/locks/, and the pylib extraction directories","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["housekeeping","pylib","store"],"hold":false,"relations":[]}
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
