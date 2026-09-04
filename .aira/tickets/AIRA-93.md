---
{"schema":1,"id":"AIRA-93","project":"aira","title":"aira reconcile --rebuild fails with E_JOURNAL_CORRUPT: duplicate project/seq assigned two different ticket targets","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","journal","reconcile"],"hold":false,"relations":[]}
---
Independently confirmed twice tonight (AIRA-68 build agent, then the AIRA-68 verify agent re-running it fresh): `aira reconcile --rebuild` fails with real exit 4 and `E_JOURNAL_CORRUPT: duplicate project/seq …/1 has target AIRA-1 and LIFE-1` — the same (project, seq) pair in the shared journal was assigned two different ticket targets across two different projects (AIRA and LIFE), and the rebuild path treats that as fatal corruption rather than reconciling it.

Neither agent touched the shared journal to produce this — it pre-existed both investigations and is unrelated to AIRA-68s own diff. Plain `aira reconcile` (without --rebuild) succeeds and the git-file ticket content is correct/authoritative, so this is not currently causing visible data loss — but it does mean `W_STALE_INDEX` cannot be cleared via a full rebuild, and the derived SQLite index cannot currently be regenerated from scratch on this machine if it were ever needed (crash recovery, corruption, a schema migration).

Needs investigation: how a single (project, seq) journal slot came to be claimed by two different projects ticket allocator concurrency bug, a manual ID collision from before ID allocation was centralized, or something else and then a decision on whether --rebuild should refuse-and-report (current behaviour, at least honest) or have a defined resolution path for a proven duplicate-seq collision.

## Root cause found, code fix landed; the file surgery is DEFERRED, not done (2026-09-04, backlog-remediation Phase 0, plan section 2)

### Root cause: an inherited GIT_DIR, the one path AIRA-46 did not reach

`internal/app`'s `gitValue` → `runGitRevParse` invokes `git -C <dir> rev-parse`
with the **ambient environment**. `-C <dir>` names the directory explicitly, but
an inherited `GIT_DIR` (or `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`,
…) **overrides** it, so project discovery silently resolves a DIFFERENT
repository than the one it was asked about — and every id derived from that
(project id, worktree id, and therefore the common-dir receipts and journal keyed
by them) is written under the wrong project.

That is exactly the shape of the two intruding receipts: both were written from
`/tmp/Test.../` working directories yet carry THIS project's id, so they claimed
this project's `seq` 1 and 3. AIRA-46 scrubbed the same variables in
`.githooks/common.sh`, but only for the shell hooks; the binary's own
git-invoking helper was never covered.

**Reproduced, not inferred.** `TestGitDiscoveryIgnoresAnInheritedGitDir`
(`internal/app/git_env_test.go`) creates two real repositories, resolves
`--git-common-dir` for one, then sets `GIT_DIR` to the other and resolves again.
Mutation-verified: with the scrub removed, the second call returns the DECOY's
common dir. That is the bug, on demand.

### Landed

- `runGitRevParse` scrubs every `GIT_*` variable from the subprocess environment.
  All of them, not an allowlist: git keeps adding environment-driven overrides,
  discovery needs none of them, and an allowlist silently regrows the hole the
  next time git ships another one.
- A `TestMain` in `internal/app` unsets any inherited `GIT_*` before the package's
  tests run, so a hook-launched or exported-environment test run cannot be fooled
  either.
- The refusal message now names the **conflicting allocation path** and points at
  `<common>/aira/receipts.jsonl`. The old message ("duplicate project/seq …/1
  has target AIRA-1 and LIFE-1") gave a reader no way to tell which entry was the
  intruder; the path makes it immediate.

### DEFERRED to the coordinating session: removing the two stale receipt lines

**Re-verified fresh, 2026-09-04**, and the plan's identification holds exactly.
`/home/mark/claude/aira/.git/aira/receipts.jsonl`, 96 lines, two duplicate
(project, seq) pairs, both under project
`21fe1460451e6d4048d00eb991d6792b572d010c12af6d60600480583d31095a`:

| line | id | seq | path | timestamp | action |
|---|---|---|---|---|---|
| 1 | `AIRA-1` | 1 | `/home/mark/claude/aira/.aira/tickets/AIRA-1.md` | 2026-08-27T20:19:52Z | keep |
| **46** | `LIFE-1` | 1 | `/tmp/TestInitAdoptsCommittedFilesRebuildsAndClearsTombstone3` | 2026-09-02T13:33:49Z | **delete** |
| 3 | `AIRA-3` | 3 | `/home/mark/claude/aira/.aira/tickets/AIRA-3.md` | 2026-08-27T20:22:30Z | keep |
| **47** | `AIRA-2` | 3 | `/tmp/TestSkillExamplesReachCoreFromRun2289567622/001/.aira/t` | 2026-09-02T13:33:59Z | **delete** |

**Not executed by this build agent, deliberately.** `.git/aira/receipts.jsonl`
lives in the repository's SHARED common directory, used by every worktree and
every session on this machine, and is appended under `receipts.jsonl.lock`. The
plan's own procedure requires notifying concurrent sessions before and after; a
build sub-agent has no peer-coordination channel to notify on, and at the time of
this work `aira confine --list` showed 23 admitted jobs from sibling agents. The
plan also orders it "land the scrub and guard FIRST, before touching the receipts
file, or the collision that produced the stale receipts can recur mid-procedure"
— which this commit does. Doing the surgery blind, live, from a sub-agent is the
shared-namespace blast-radius class this project has hard rules against.

Procedure for whoever runs it (unchanged from the plan): take
`receipts.jsonl.lock`; back the file up to `~/tmp`; delete exactly the two
entries identified above (match on id+seq+timestamp, not line number alone, in
case the file has grown); run `aira reconcile --rebuild` to verify
`E_JOURNAL_CORRUPT` is gone; notify concurrent sessions before and after.

Ticket stays OPEN for that step.

### Build-review (Sol, 2026-09-04) — the scrub was too narrow, and is widened

Sol found the fix covered only the ONE path the ticket names, while the same
inherited-`GIT_DIR` override reaches every other place AIRA shells out to git.
Confirmed against source and fixed: the scrub is now a shared
`gitcontext.ScrubbedEnvironment()` used by **all six** production git call sites:

| site | what an inherited GIT_DIR would have done |
|---|---|
| `internal/app/project.go` `runGitRevParse` | the original defect: discovery resolves another repository |
| `internal/app/project.go` `PrepareInit` | reads `HEAD:.aira/config` out of another repository |
| `internal/store/testreport.go` `gitValue` | test-report and ratchet provenance names another repository's HEAD |
| `internal/store/store.go` `runGit` | every generic git probe answers about another repository |
| `internal/store/gate_eval.go` canary `git init` / `git add -A` | **the worst case: `add -A` stages the scratch tree into the INHERITED index** |
| `internal/daemon/eject.go` status probe | eject's cleanliness check reads another repository's status |

The helper lives in `internal/gitcontext` — a leaf both `internal/app` and
`internal/store` already import — so there is one implementation rather than a
copy per layer, and its unit tests live with it.

**Accepted tradeoff, recorded because Sol was right that the old comment
overclaimed.** The old comment said "discovery needs none of them". A few `GIT_*`
variables are legitimate configuration rather than an override —
`GIT_CONFIG_GLOBAL` (which can carry a `safe.directory` entry),
`GIT_CEILING_DIRECTORIES`, `GIT_DISCOVERY_ACROSS_FILESYSTEM`. Dropping them makes
discovery MORE permissive, not wrong, in every case except a caller who put
`safe.directory` only in a `GIT_CONFIG_GLOBAL` file — who then gets git's
dubious-ownership refusal, a loud diagnosable failure rather than a silently
wrong answer. That is the direction AIRA prefers, and it is why the blanket form
is kept over an allowlist that would need curating forever. Now stated in the
helper's doc comment instead of asserted away.
