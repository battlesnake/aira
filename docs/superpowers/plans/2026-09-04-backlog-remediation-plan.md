# Backlog remediation plan

**Date:** 2026-09-04, executed 2026-09-05
**Status:** EXECUTED AND DEPLOYED. Approved via 4 automated Fable review
rounds (GATE-PASS-WITH-CHANGES each round, converging 16 → 17 → 9 → 10
required changes) plus two manual revision passes; see git history for full
detail. Every Phase 0 row, all five Phase 1 structural fixes, and all nine
Phase 2 items are merged to master and independently verified. Binary
rebuilt, skill reinstalled, daemon restarted — the daemon correctly
re-adopted in-flight jobs via cgroupfs reconstruction (AIRA-74), no
disruption. AIRA-91 Part B remains an explicit, unbuilt owner decision (§5
item 5). AIRA-20 stays open for its `-race`-restoration half alone, blocked
on AIRA-33/AIRA-91 per its own ticket. One real incident during execution,
corrected and documented on PR #36: PR #35 (Fix 5) was squash-merged from a
stale branch snapshot mid-race with a still-working build agent, silently
dropping the critical AIRA-91 fix; caught by independent re-verification
against the merged artifact rather than trusting the PR description, and
fixed before deploy.
**Input:** [`docs/superpowers/reviews/2026-09-04-fable-backlog-simplification-sweep.md`](../reviews/2026-09-04-fable-backlog-simplification-sweep.md)
(four parallel Fable-model cluster reviews of all 49 open tickets, each verified
against current source, not trusted from ticket text)

## 0. Owner instruction this plan operationalizes

> Write a plan for dealing with all of this... Where we can fix things by
> deleting/simplifying/unifying stuff, it's preferred over adding more
> complexity.

Every phase below is ordered by that preference: deletions and unifications come
first and are the largest phase by ticket count; genuinely irreducible individual
engineering comes last. One P1, AIRA-28, is resolved by a *decision* this plan
makes explicitly, not by more code — flagged so Fable and the owner can push back
on that call specifically. Its sibling P1, AIRA-29, is **not** resolved here:
plan-review found its build mis-sized as a Phase 1 item, so it is pulled out into
its own follow-on milestone (§3.2, §7) once a hard ship-together precondition and
its hold reason are answered explicitly. A third open P1, AIRA-73
(`outbox.resolution` is never written), is added to this plan's scope in Phase 2
(§4) — plan-review found it missing from the original draft entirely.

## 1. Scope

48 of the 49 open tickets as of `18d0e93`. **AIRA-91's investigation is explicitly
out of scope for this plan — but, corrected this revision, it is no longer an
open investigation.** Refreshed to current master (`2af6d66`): AIRA-91's root
cause is **CLOSED** (`01335b4`, `2af6d66`) — `systemd-oomd` SIGKILLs
(`cgroup.kill`) the *whole* `.aira-CONFINE-*` scope under real memory-pressure
PSI contention, a real exit **137**, never exit 0. AIRA's own aitest worker
scopes are deliberately sized into sustained `memory.high` reclaim throttling
(`worker_admit.go:285`) by design, which generates the PSI pressure
`systemd-oomd` kills on; the only post-run diagnostic AIRA has,
`formatConfineReserveAdvisory` (`confine_linux.go:872-890`), fires on
`memory.events` `oom_kill>0` and is structurally blind to a userspace
`cgroup.kill`, so the trailer prints identically to a clean run. Remaining
work per the ticket is explicitly split into **Part A (build — the trailer
attribution fix, unified with AIRA-70 this revision, §3.6) and Part B (owner
decision — oomd-vs-admission policy, §5 item 5)**; neither is decided or
built as a silent side effect of this backlog sweep, and Part B needs
explicit owner sign-off before anything is built. AIRA-91 itself stays out of
this plan's batch execution as a ticket to close (its fix directions are the
scoped items above), not because its cause is still unknown.

Two more tickets are consequently **blocked, no action in this plan**:
AIRA-32 and AIRA-33 wait on AIRA-91 — **restated this revision for the closed
root cause, not the old "open investigation" framing**: their precondition is
now **AIRA-91 Part A built + fastest-ee re-verified + the `FASTEST_NO_AITEST=1`
pin removed**. **AIRA-32 carries an additional precondition, added this
revision — a prior draft attributed its block to the AIRA-91/AIRA-33 chain
alone, which is incomplete:** its own ticket text names "Blocked by Slice 2"
(AIRA-31, now done), and its scope includes the `memory.high`-crossing
recycle watermark that AIRA-35/§5 item 5's Part B decision changes — so
AIRA-32 is additionally blocked on **Part B decided + AIRA-35 landed** (§2,
§5 item 5), on top of the AIRA-33/fastest-ee precondition shared with
AIRA-33 above. Both tickets are still blocked, but for concrete, now-named
reasons rather than an unresolved investigation. Five more (AIRA-17, 25, 26, 65, 77) have **no
separate action** — four of them (AIRA-17, 26, 65, 77) are scoped work the
sweep found already lives entirely inside AIRA-33's deletion (the xdist stack).
The fifth,
**AIRA-25**, is attributed differently on plan-review: its scope is the daemon
confine-admission ledger split in `admit.go` (AIRA-29 territory) plus the xdist
per-test delta (AIRA-33's), and AIRA-29's own ticket body records it as
subsumed — so it closes via whichever of AIRA-29 or AIRA-33 lands first, not
purely inside AIRA-33's deletion as originally framed. All five close
automatically without separate action in this plan and would be wasted,
throwaway effort if actioned now.

That leaves **41 tickets dispositioned by this plan** (49 open − 1 AIRA-91
investigation-track − 2 blocked − 5 no-separate-action = 41; a prior draft of
this plan miscounted this as 43). **Correction, this revision: "41 with real
action" over-claimed and read against §7 inconsistently.** The 41 split: **39
get a phase assignment with a stated action** (19 in Phase 0, **11 in Phase 1,
9 in Phase 2 — corrected this revision from a prior draft's "10 in Phase 1, 10
in Phase 2" after Fix 5 (§3.6) moved AIRA-70 out of Phase 2 and into Phase 1**:
Phase 1 = AIRA-39, 41, 63, 42, 45, 87, 84, 80, 81, 60, 70 (11 tickets); Phase 2
= AIRA-73, 16, 40, 43, 24, 85, 86, 20, 22 (9 tickets); **AIRA-82 and AIRA-37
are counted once, in Phase 0's nineteen, for this tally** — each also gets a
real code fix under Phase 2 (§4: AIRA-82's scope override, AIRA-37's two
residues), but Phase 0's action for both is re-scoping the ticket text, not
the fix itself, so counting them once in Phase 0 here is not a drop of their
Phase 2 work, just this tally's home for each — total still 39 (19+11+9) —
of the Phase-0 nineteen, three — AIRA-79, AIRA-34, AIRA-64 —
are explicitly "no code, documented reason" rather than a build, not
double-counted as code work) and **2 are decision-only, not built anywhere in
this plan** — AIRA-29 and AIRA-78 (§3.2, §5) — which is why §7 correctly
lists both as "not touched"; that is the same 2 tickets counted once each, by
disposition, not a contradiction between the two sections. The 39 phase-actioned
tickets are sequenced into three phases by risk tier, in the order this plan's
preference puts them.

## 2. Phase 0 — Deletion, simplification, and stale-ticket closure (Tier C)

Mechanical, low-risk, no design judgement beyond what the sweep already
established. One worktree, sequential commits (small diffs, fast to review),
`make ci` after each. **Correction, this revision: this phase is not free of
daemon-protocol/admission-path touches — the prior draft's "No daemon-protocol
or admission-path changes live here" was false.** AIRA-35 edits the
`WorkerAdmitResponse` wire struct (`internal/daemon/worker_admit.go:39`) and
its grant line (`:285-287`), plus the client lease struct
(`internal/runner/worker_admit_client_linux.go:20,46,118`) and
`cmd/aira/main.go:1080` — exactly Fix 1/Fix 2's files (§3 file-touch matrix);
AIRA-52+23 edits `internal/daemon/admit.go:1367` (the owner arg). Both are
small, narrowly-scoped changes within an otherwise-mechanical ticket, not a
structural admission redesign — but they are real touches to files Phase 1
depends on, so **Fixes 1, 2, and 4 (§3) do not start until this phase,
excluding AIRA-35, has merged** (previously only Fix 3 said so — see §3's
Order paragraph). **AIRA-35 itself is carved out of that precondition, added
this revision:** AIRA-35's own row below is gated on the §5 item 5 owner
decision, and transitively parking a P0 fix (Fix 1, AIRA-39) behind that
undated policy call is wrong — so AIRA-35 does not block Fixes 1, 2, or 4; it
lands separately, as its own small commit, once §5 item 5 is decided.
Everything else in this phase is either dead code, a reporting/config fix, or
a narrow, previously-scoped change with a single clear caller. **AIRA-83(a)
lands first, before every other commit in this phase** — it's the mitigation
that makes a stray worktree-binary invocation non-fatal for the rest of this
phase and the whole programme, so nothing daemon-touching should land ahead of
it.

| Ticket | Action |
|---|---|
| AIRA-83(a) | **Lands first, before any other commit in this phase** — see intro above. Delete only the `ServiceIdentityMatches` → `systemctl --user restart aira-daemon.service` branch of `replaceOlderDaemon` (`cmd/aira/dispatcher.go:590-597`); return the existing "re-run aira install" error in its place. The unmanaged `daemon.Stop` branch is the client's own ad-hoc daemon and stays, per the ticket's own direction #1 — do not delete it. **Fold in AIRA-83 item 3 in the same commit:** `runnerDaemonProtocolVersion = 5` (`internal/runner/admission_linux.go:79`) is hand-copied from `daemon.ProtocolVersion` — derive it from that constant, or pin them equal in a test, before Phase 1 Fix 2 changes the wire shape. (AIRA-83's other half, (b), is a Phase 1 structural fix — see Fix 2.) |
| AIRA-28 | Close as superseded-by-decision: the ticket body already reads "SHELVED — SUPERSEDED BY AIRA-29" and the `supersedes` relation is already recorded (owner decision dated 2026-09-01). This commit only records the status transition — flagged for explicit sign-off below and in §5, since it retires a real airtight-guarantee design even though the decision itself was already made. AIRA-29's build is **not** part of this plan — see §3.2. |
| AIRA-89 | Delete 4 confirmed-dead symbols (`hasGateContent`§, `FlakyCellStateSummary`, `GateAudit.Verify`, `GateAuditRecords`) — re-verify zero callers immediately before deleting, don't trust the sweep's snapshot across the time gap. |
| AIRA-66 | Replace both `go:embed all:` directives (aitest, xdist-governor) with explicit **glob patterns** (e.g. `aitest/*.py`, which matches `__init__.py` directly while excluding `__pycache__`, `.pytest_cache`, `README.md` and `testdata` by construction) rather than a hand-listed file manifest — `go:embed` already excludes `.`- and `_`-prefixed entries during directory walks, so a glob only needs to stop short of non-code files. Make the equality test's oracle `git ls-files` of the package (one source of truth) instead of a second hand-maintained list. Decide explicitly whether co-located `test_*.py`/`conftest.py` files stay embedded or move to a `tests/` subdirectory, since a glob can't exclude them from `aitest/*.py` — resolve this before writing the glob, not after. **Correction, this revision:** the current tests `TestEmbeddedPyLibIncludesImportPackageAndDocumentation`/`TestEmbeddedAitestIncludesImportPackageAndDocumentation` (`internal/pylib/extract_test.go:14-36`) assert `.gitignore` and `README.md` are embedded, and `extract.go:26-28,41-44` calls `.gitignore` "the extraction hygiene file" — but no Go consumer of the extracted `.gitignore`/`README.md` exists, and the globs above silently drop both. **Decided here, not left implicit: those two tests are replaced by the `git ls-files`-oracle equality test above, and `.gitignore`/`README.md` are intentionally no longer shipped** in the embedded copy — they are source-hygiene files, not runtime dependencies of the extracted package. (If the hygiene file is wanted in the embedded copy after all, add explicit `aitest/.gitignore`/`aira_xdist_governor/.gitignore` patterns alongside the `*.py` glob instead — but that is not this plan's default.) |
| AIRA-88 | Site 3 (pylib extraction-dir growth) closes as a *consequence* of AIRA-66 — re-measure after, no separate code. Sites 1–2 (registry.jsonl, lock-file inodes): record "stays append-only / stays unbounded, both bounded in practice" as the resolution; no code. |
| AIRA-35 | **Re-tiered this revision — no longer a mechanical Tier-C commit, gated behind an owner decision.** AIRA-91's confirmed mechanism (§1) step 2 is exactly this site: `memoryHigh := estimatedBytes*4/5` (`worker_admit.go:285`) sizes every aitest worker scope into sustained `memory.high` reclaim throttling by design — the PSI pressure `systemd-oomd` kills on. AIRA-91's own ticket lists "raising/removing memory.high" as an AIRA-91 Part B candidate and states Part B "needs explicit owner sign-off before any of it is built" (§5 item 5); this plan has no such sign-off recorded, so **AIRA-35 does not start in Phase 0 until §5 item 5 (AIRA-91 Part B) is explicitly decided by the owner.** Once decided in favor of removing worker-scope `memory.high`, the mechanical change is unchanged from the prior draft: stop arming `memory.high` on aitest worker scopes (drop the wire field, the grant line, the client struct field, the `worker_scope_linux.go` guard); change `worker.py`'s watermark read from `memory.high` to `memory.max` with the percentage folded in; **the same PR must amend `docs/superpowers/specs/2026-09-01-aitest-design.md`'s worker-scope containment section** (which currently cites `memory.high` on worker scopes as a spec requirement, per `worker_scope_linux.go:14-31`) — per this repo's CLAUDE.md the design spec is authoritative and cannot be left silently inconsistent with the code. **Sequencing carve-out, added this revision — resolves a conflict plan-review found with §3's "Fixes 1, 2, and 4 start once Phase 0 has merged" statement:** AIRA-35 is **not** part of the precondition for Fixes 1, 2, and 4 (§3 Order paragraph, file-touch matrix row 0) — gating Fix 1 (which closes P0 AIRA-39) transitively on AIRA-35's own undated §5 item 5 owner decision would block a P0 fix on an unscheduled policy call, which this plan does not accept. Fixes 1, 2, and 4 depend only on **Phase 0 excluding AIRA-35**. AIRA-35 itself lands as its own small commit whenever §5 item 5 is decided, rebased onto whichever of Fix 1 or Fix 2 has landed by then — its diff is small enough (the wire field, grant line, client struct field, the `main.go:1080` argument, the `worker_scope_linux.go` guard, `worker.py`'s watermark read, and the spec amendment above) that landing it late and rebasing is trivial regardless of timing. |
| AIRA-44 | Add `AIRA_AITEST_OUTER_SCOPE` at launch (the launcher already has `scope.Reference()` in hand); bootstrap uses it instead of self-discovering via `CurrentCgroupPath()`. Make the membership guard idempotent for "already in `<outer>/.aira-supervisor`". |
| AIRA-52 + AIRA-23 | **One identity decision, not two independent tickets — land together** (AIRA-23 moved here from Phase 2 on plan-review: it's the same charset question as AIRA-52's delimiter). `runner.ValidateConfineIdentity` (`internal/runner/confine.go:215-223`) restricts owners to `[A-Za-z0-9_.-]`. Decide the stable owner-identity format first: replace AIRA-23's literal `"unknown"` owner fallback with a stable, non-colliding identity (e.g. `cwd:<basename>`, which needs the charset widened to allow `:` or an equivalent substitute), then pick AIRA-52's scope-directory-name delimiter so it doesn't collide with that format. Delete `freshConfineOwner` and the in-memory registry merge it exists for once the scope directory name is authoritative. |
| AIRA-56 | Delete `hasGateContent` (dead by AIRA-89, don't double-count — land together). `ready` surfaces the existing `U_GATE_SET_EMPTY` primitive as an advisory finding rather than gaining new ledger-memory state. |
| AIRA-74 | Replace the machine-wide flock + full-rebuild write-transaction with a per-query `TEMP` FTS table on its own connection. **Scope stated explicitly, this revision:** this removes the machine-wide lock and the persistent-table maintenance surface, but **retains a full per-project index build on every query** — it does not implement incremental indexing. The ticket asked for incremental indexing "or at minimum a per-project lock"; this plan takes the per-query-TEMP-table option, simpler than either, and **explicitly declines incremental indexing for simplicity, accepting the per-query build cost** as the tradeoff — recorded here so the resolution isn't read as closing the "rebuilds on every query" half of the ticket, which it does not. **Shared-machine risk, added this revision (see also §8):** the machine-wide flock this fix removes is currently the **only** serialisation of full FTS rebuilds — without it, N concurrent `aira grep` calls from different sessions each build their own independent in-memory FTS index in parallel, with no bound on N. **Required before merge:** measure the per-query build cost (wall time + peak RSS) on the largest project on this machine, record the numbers in the PR, and state the accepted bound explicitly. If that cost is material at realistic concurrency, the fallback is a **per-project lock** (the ticket's own "at minimum" option) — not incremental indexing, which stays out of scope either way. |
| AIRA-75 | Stop minting project **journal/event** sequence numbers (not "ticket-sequence numbers" — that was imprecise) for daemon telemetry events (`AppendWatchdogEvent`, `rant.redacted`). **Correction:** "stop minting a sequence number" and "become a log line instead" are different changes — `AppendWatchdogEvent` (`internal/store/watch.go:11`) exists to broadcast host-level kill decisions into each ready project's `aira watch` stream, and demoting it to a daemon log line would silently remove that visibility. This plan resolves the conflation by keeping the watch-visible event and instead making it unjournaled by design (no project journal seq), since — as the ticket already says — no gap-detection mechanism exists to void by skipping the seq; say so in the resolution. |
| AIRA-93 | Scrub `GIT_*` in `gitValue`/`runGitRevParse` (the binary's own git-invoking helper — the hooks were already scrubbed by AIRA-46, this is the one path AIRA-46 didn't reach); add a `TestMain` guard against inherited `GIT_DIR`. **Land the scrub and guard first**, before touching the receipts file, or the collision that produced the stale receipts can recur mid-procedure. **This is not a plain mechanical commit:** the two stale receipts live in `.git/aira/receipts.jsonl` — the repository's shared git common dir, used by all worktrees and sessions on this machine, appended under `receipts.jsonl.lock`. Take the lock, back up the file to `~/tmp` first, delete exactly the two identified lines (name them by seq/timestamp in the commit message), run `aira reconcile --rebuild` to verify, and notify concurrent sessions before and after. Update the refusal message to print the offending receipt path. **Exact lines identified fresh, this revision** (`.git/aira/receipts.jsonl`, re-read 2026-09-04): **line 46** — `id:"LIFE-1"`, `seq:1`, path `/tmp/TestInitAdoptsCommittedFilesRebuildsAndClearsTombstone…`, timestamp `2026-09-02T13:33:49`; **line 47** — `id:"AIRA-2"`, `seq:3`, path `/tmp/TestSkillExamplesReachCoreFromRun…`, timestamp `2026-09-02T13:33:59`. Both collide with the project's own real `seq:1`/`seq:3` receipts at **lines 1 and 3** (`AIRA-1`, `AIRA-3`). **Note: the second leaked test (`TestSkillExamplesReachCoreFromRun`) differs from the one an earlier sweep pass named** — the commit message must name both lines/tests precisely as re-verified here, not repeat the earlier identification unchecked. |
| AIRA-69 | Add a 3-line assert in the cgrouptest helper refusing to place a scope directly under `aira.slice`; no further action (impact was confirmed cosmetic, not the leak risk originally suspected). |
| AIRA-82 | Re-scope only in this phase (update the ticket's own framing — it is not fabricated metadata, the daemon correctly records the cwd it was given). The actual small fix (an explicit per-call scope override on MCP/CLI) moves to Phase 2 since it touches face-layer code, not a pure deletion. |
| AIRA-37 | Re-scope: 4 of 6 original sub-items are already fixed (by AIRA-30 and AIRA-92) — close those as stale with a pointer to the commits that fixed them. The 2 genuine residues (an atfork docstring, a worker over-spawn counter) move to Phase 2 as small individual fixes. |
| AIRA-64 | **SUPERSEDED mid-execution, this revision — no longer a doc-note closure.** The owner directly requested (relayed via peer session `money`, live evidence: an 82-minute engine leg and 44 phantom failures under real contention) that aitest become contention-aware about spurious pytest-timeout failures. Severity raised P2→P1. **If this row is reached by a build agent before this correction is read, do NOT close with a doc note** — leave AIRA-64 open/planned and move on; the real fix (CPU-concurrency governance for aitest workers, since aitest currently has none — the only cooperative CPU/RAM governor in this codebase is xdist-only and permanently disabled inside forked aitest workers per AIRA-92's own investigation) is out of scope for this plan's Phase 0 and is queued as dedicated follow-on work once this execution effort has bandwidth. See the ticket itself for the full corrected mechanism and options. |
| AIRA-34 | Correct the ticket's stale line references; keep as documented, known behaviour (real production consumer exists on exactly one path; not worth ~10 lines of narrowing until that path starts gating on it). No code change. |
| AIRA-79 | Explicitly deferred, no code (no submodule-bearing AIRA project exists to justify the ~20 lines yet) — recorded here, not silently dropped from the plan. |
| AIRA-33 + AIRA-32 (ticket text only) | **Added this revision, per plan-review.** AIRA-33's ticket still reads "Blocked by Slice 2 (needs AIRA's own suite migrated and clean)" — AIRA-31 (Slice 2) is done, so the ticket currently states a satisfied precondition, which is stale and misleading to anyone reading the ticket directly rather than this plan. Same class of correction as the AIRA-82/37/34 re-scopes above. Update AIRA-33's ticket text to name its real, current precondition: "AIRA-91 Part A built + fastest-ee re-verified + the `FASTEST_NO_AITEST=1` pin removed" (§1). Update AIRA-32's ticket text to name its precondition as that plus "Part B decided + AIRA-35 landed" (§1, §5 item 5). No code; a ticket-text-only commit, landed alongside the other re-scopes. |
| AIRA-78 (ticket text only) | **Added this revision, alongside the §5 item 2 executor default.** Record on AIRA-78's own ticket why this P0 is not being built in this plan: the default is to delete the ratchet gate kind (zero production rows, consistent with §0's deletion preference), with the fallback being a schema change to give test reports a producer and then apply the Fix-3 pattern — so the severity is not silently parked. No code; a ticket-text-only commit. |

**Verification for this phase:** `make ci` after each commit; a self-review pass
per CLAUDE.md's lighter path is sufficient for the pure deletions (AIRA-89, 66,
88, 69) since they remove code with zero live callers, verified fresh (AIRA-60
is no longer a Phase 0 pure deletion — it's folded into Phase 1 Fix 3, §3.5).
The eight that touch runtime behaviour even slightly (AIRA-35, 44, 52/23, 56,
74, 75, 93, 83(a)) get one Sol/Codex build-review pass each before merge — Tier
C in this project's own grading, not Tier A, but not zero-review either given
they touch the daemon/runner/aitest hot paths. (AIRA-35 additionally does not
start until the §5 item 5 owner sign-off lands — see its table entry above;
it is listed here for when it does start, not as already clear to build.) (A prior draft of this plan said
"five"; it listed eight tickets — corrected.)

**Heavy-command discipline, mandated explicitly this revision — this repo's
CLAUDE.md requires it but a prior draft of this plan never stated it, for
this phase or any other.** Every executor/sub-agent brief for this plan,
across Phase 0, Phase 1, and Phase 2, must prefix every `make ci`/`go
test`/`go build` invocation with `aira confine -- ` (the project's memory-
confinement wrapper), so a runaway build or test process dies in its own
cgroup scope instead of threatening the shared machine. Suite runs must be
**serialised** across whichever worktrees are running in parallel (Phase 0's
sequential-commit worktree and Phase 2's independent-ticket worktrees) —
never run several `go test ./...` invocations concurrently on this shared
box: the real-cgroup tests place scopes directly under `aira.slice`
(AIRA-69), and each suite run is RAM-heavy in its own right. If a job needs
to be stopped, `kill <PID>` (or `kill -INT <PID>`) the `aira confine`
process that was started for it — **never** `systemctl --user stop
aira.slice` (or `aira-daemon.service`, outside a deliberate, coordinated
Phase 0/1 deploy step, §8 — corrected this revision: §8's restart policy now
also schedules a Phase 0 restart for AIRA-52+23/AIRA-75, not only Phase 1),
which would hit every other session on the machine.

## 3. Phase 1 — Structural fixes (Tier A, full two-loop)

Four structural fixes (down from the five in a prior draft — AIRA-29's build
has been pulled out into its own follow-on milestone entirely, see §3.2) close
or moot roughly 13 tickets between them, **plus a fifth Tier-A item added this
revision** (Fix 5, §3.6: AIRA-70 unified with AIRA-91 Part A, the confine
trailer's kill-attribution surface — re-tiered up from Phase 2 because AIRA-91
Part A is P0, §4). **Sequencing matters more than parallelism here** — see the
file-touch matrix below. **Corrected this revision: it is not only Fix 3 that
depends on Phase 0.** Phase 0's AIRA-35 and AIRA-52+23 touch the exact files
Fixes 1 and 2 depend on (`worker_admit.go`, `worker_admit_client_linux.go`,
`main.go`, `admit.go` — §2), so **Fixes 1, 2, and 4 all start only once Phase 0
excluding AIRA-35 has merged** — carved out this revision (§2, AIRA-35 row):
AIRA-35 is itself gated on the §5 item 5 owner decision, and transitively
blocking Fix 1 (P0 AIRA-39) on that undated policy call is wrong, so AIRA-35
is not part of this precondition and instead lands as its own small commit,
rebased onto whichever of Fix 1/Fix 2 has landed, once Part B is decided. Fix
3 (gates) is **no longer gated on Phase 0 at all, corrected this revision** —
see its own section (§3.5) for why the file-overlap that used to justify the
gate no longer holds; it now starts immediately in its own worktree, subject
only to the §5 item 2 owner-decision default. Fixes 1, 2, and 4 all touch
`internal/daemon/worker_admit.go` and/or `internal/runner/*admission*`/
`*confine*`/`protocol.go` files — **corrected this revision: none of them
touch `internal/daemon/admit.go`**, see the file-touch matrix below — and
**must land sequentially**, each rebasing onto the previous, to avoid PRs
racing to modify the same functions. This is the single highest-value planning
decision in this document — get Fable to check it specifically.

### File-touch matrix

| Fix | Primary files | Overlaps with |
|---|---|---|
| 0. Phase 0 (§2), for reference — not a Phase 1 fix, but its files are what Fixes 1/2/4 depend on, added this revision | `internal/daemon/worker_admit.go:39,285-287`, `internal/runner/worker_admit_client_linux.go:20,46,118`, `cmd/aira/main.go:1080`, `internal/runner/worker_scope_linux.go`/`worker_scope_stub.go` (AIRA-35 — **carved out of the Fixes 1/2/4 precondition this revision, see the Order paragraph below and §2's AIRA-35 row**); `internal/daemon/admit.go:1367`, **`internal/daemon/confine_manage.go:33`
(`freshConfineOwner` definition, corrected this revision from a prior
draft's `internal/runner/confine_manage.go:33-60`, which is a different
file — that range there is the unrelated `ConfineListResult`/
`ConfineSliceReserve` struct) and `:164` (its only production caller, inside
the confine-kill dispatch)**, `internal/runner/confine_linux.go:966-968`, `internal/runner/confine.go:215-223` (AIRA-52+23); `cmd/aira/main.go:1019`, `internal/runner/confine_linux.go:753` (`AppendAitestChildEnvironment`, defined in `internal/pylib/env.go:51` — added this revision, precision correction), `internal/runner/aitest_bootstrap_linux.go` (AIRA-44); `internal/runner/admission_linux.go:79` (AIRA-83(a)) | 1, 2, 4 — depend on Phase 0 **excluding AIRA-35** merging first (see the Order paragraph below); AIRA-35 lands separately once §5 item 5 is decided |
| 1. Worker-admit ledger = cgroup tree (+ AIRA-63 gate) | `internal/daemon/worker_admit.go`, `internal/runner/worker_scope_linux.go`, `cmd/aira/main.go:1080`, `internal/runner/worker_admit_client_linux.go` | 2 — the last two files are Fix 2's (§3.1); this makes the 1 → 2 order load-bearing, not just convenient. **Neither Fix touches `internal/daemon/admit.go`** — corrected this revision; a prior draft's prose wrongly implied otherwise (see intro paragraph above and the §3.2/§4 AIRA-24 correction). |
| 2. Structured daemon↔client outcome codes | `internal/daemon/worker_admit.go` (the `WorkerAdmitResponse` type lives here, not in `protocol.go` — corrected from a prior draft), `internal/runner/worker_admit_client_linux.go`, `cmd/aira/main.go`, `internal/pylib/aitest/supervisor.py`, and — because this fix bumps the wire protocol version — `internal/daemon/protocol.go:21` (`ProtocolVersion = 5`) and, unless AIRA-83 item 3 (§2) already derives it from that constant, `internal/runner/admission_linux.go:79` | 1 (same worker-admit files, above); **4, corrected this revision — the real overlap is `protocol.go:21`, not `main.go`: Fix 4 edits `protocol.go`'s `exchange()` (`:337-341`) and `server.go:527`, the same file this fix bumps the version constant in** |
| 3. Captured-subject gate evaluator (+ AIRA-60, AIRA-86 seed sites) | `internal/store/gate_eval.go`, `gate_command.go`, `gate_ratchet.go`, `internal/gate/canary.go`, `traceability.go`, **`gate_subject.go` (`stableSubjectEntries`, `:160` — added this revision, was missing from the primary file list despite being this fix's core capture primitive)** | Phase 0 AIRA-60 (`gate_command.go:358`, `canary.go:189`, `gate_eval.go:535`) and Phase 2 AIRA-86 (`gate_eval.go:99`, `gate_eval.go:597`, `gate_ratchet.go:80`) share these exact files — folded in below, not independent (§3.5) |
| 4. Symmetric deadline-policy seam | `internal/daemon/server.go:527,155,668`, `internal/daemon/protocol.go` (`exchange()`, `:337-341`) — **`server.go:155,668` added this revision, the seam's actual in-scope boundary (§3.4)** | 2, corrected this revision — the shared file is `protocol.go:21` (above), not `main.go` |
| 5. Confine trailer kill-attribution (AIRA-70 + AIRA-91 Part A) — added this revision, §3.6 | `internal/runner/confine_linux.go` (`FormatConfineStatus`, `formatConfineReserveAdvisory:872-890`, the signal handler), `internal/daemon/confine_manage.go:160-172` (`confine-kill` dispatch — **precision correction, this revision:** `confine_manage.go:141-152` is the `--list` summary struct, not the kill dispatch; the finding that no `log.` call exists anywhere in the file still holds either way) | Phase 0's AIRA-44 and AIRA-52+23 share `confine_linux.go` — starts once Phase 0 has merged; **also shares `internal/daemon/confine_manage.go` with Phase 0's AIRA-52+23, added this revision** (AIRA-52's `:164` `freshConfineOwner`-caller deletion and this fix's new log line at `:160-172` are the same block — the existing "Fix 5 starts after Phase 0" ordering already covers it, named explicitly here per plan-review); independent of the 1 → 2 → 4 chain (touches neither `worker_admit.go` nor `admit.go`) and, **corrected this revision, no longer "same as Fix 3"** — Fix 3 itself now starts immediately, not gated on Phase 0 (§3.5) |

**Order: Phase 0 excluding AIRA-35 merges first — corrected this revision, all
of Fixes 1, 2, and 4 depend on Phase 0 files (row 0 above), not only Fix 3 as
a prior draft had it; AIRA-35 itself is carved out of this precondition (§2,
row 0) and lands separately once §5 item 5 is decided. Then 1 → 2 → 4
sequentially. Fix 3 starts immediately, in its own worktree, not gated on
Phase 0 at all (corrected this revision, §3.5) — subject only to the §5 item 2
owner-decision default. Fix 5 starts once Phase 0 has merged, in its own
worktree, same as before.** (A prior draft
ordered this 1 → 5 → 2 → 4 with a "Fix 5" for AIRA-28/29; plan-review found
that wrong on two counts — AIRA-29's real footprint is far larger than a
single `admit.go` change and it carries a hard precondition this plan hadn't
engaged, and it didn't belong ahead of the smaller, better-defined Fixes 2 and
4 in a serial chain. Resolved in the reviewer's favor: AIRA-28's closure is
now a Phase 0 status-change, not part of this chain, and AIRA-29's build is a
separate follow-on milestone, not part of this chain either — see §3.2 and
§7. The "Fix 5" number is reused this revision for the unrelated,
independent AIRA-70/AIRA-91-Part-A item, §3.6 — not a re-instatement of the
old AIRA-28/29 slot.) Reasoning for what remains: 1 and 2 both touch
`worker_admit.go`, and Fix 1 moving worker-scope creation into the daemon
removes the CLI-side call Fix 2 patches (`main.go`,
`worker_admit_client_linux.go`) — land 2 after 1 has settled. 4 touches
`protocol.go` at `exchange()`/`server.go:527`; 2 touches `protocol.go:21` (the
version bump) — corrected this revision from "main.go/protocol.go territory":
the real overlap is the shared file, not a shared function — land 4 last to
avoid rebasing a deadline-seam change under a still-moving classification
change.

### 3.1 Fix 1 — Worker-admit ledger tracks the real cgroup tree

**Closes AIRA-39, AIRA-41, AIRA-63** (AIRA-63 corrected below — a prior draft
claimed it was mooted, which doesn't hold).

Replace `workerJobState.grants`' pure in-memory accounting with `committed =
Σ memory.max` over existing `.aira-worker-*` children of the outer scope,
scanned from cgroupfs the same way the outer slice ledger already does (post-#74).
Close the grant→scope-creation window: the daemon creates the worker scope
itself under `job.mu` rather than trusting the CLI to create it after the grant
is recorded (the reaper already rmdirs orphaned scopes, so cgroupfs mutation
from the daemon side has precedent). **Mandated, this revision, per plan-review
and the owner's deletion/unification preference: the daemon-side creation
MUST call the existing `runner.CreateWorkerScope`
(`internal/runner/worker_scope_linux.go:17`, which already writes
`memory.max`, the `memory.high` guard, and `memory.oom.group=1`) — no second,
daemon-local scope-creation implementation is written.** The daemon package
already imports `internal/runner` (`runner.WorkerScopeChildPath` at
`worker_admit.go:284`, `runner.KillConfine` at `confine_manage.go:163`), so
this is a straightforward call, not a new dependency. Ownership tracking
(`workerScopeOwner`) becomes unnecessary — the sum is over the scope, not
over who asked for it.

**Cgroupfs scan cadence, bounded explicitly this revision:**
`evaluateWorkerAdmit` runs once per poll per waiter (default 200ms,
`worker_admit.go:381`) under `job.mu` — an unbounded per-poll `Σ memory.max`
walk over every `.aira-worker-*` child would be the same CPU-regression class
AIRA-61 already found and fixed once (O(tree) scan → 25–65% CPU). This fix
uses a **per-outer-scope cached child sum**, refreshed at most once per
second (the same pattern #74 already uses for `ListConfines`), invalidated
synchronously on the daemon's own create/release of a worker scope (never on
an externally-observed change) — not a scan on every poll. Name this choice
explicitly in the PR and add the CPU-regression risk, with AIRA-61 as the
precedent, to §8.
**Fail-closed rule, denial class specified this revision:** if the daemon's
`mkdir` or `memory.max` write for the new worker scope fails, no grant is
recorded and the response is a denial — never a grant paired with a missing
scope. **The example this section previously gave — "the supervisor hasn't
yet enabled `cgroup.subtree_control`" — is not a real scenario in the normal
flow, and is dropped:** `BootstrapAitestSupervisor`
(`internal/runner/aitest_bootstrap_linux.go:14-31`) already enables
`cgroup.subtree_control` before any worker-admit call runs, so a daemon-side
`mkdir`/`memory.max`-write failure at admit time is not transient — it means
the daemon's cgroupfs access is broken. The denial **must** therefore be
`reject:worker-scope-create-failed:<errno>` (terminal — routes to
`WorkerAdmitRequestTooLarge`, and the queue is marked `unevaluated`, around
`worker_admit.go:416-428` — corrected this revision, `supervisor.py:652-659`
is the classifier, not the queue-marking site), **never** a `fallback:`-
prefixed reason: **reworded this revision for mechanism accuracy** —
`supervisor.py`'s classifier treats any denial message *lacking* the literal
substring `reject:` — including, but not specifically because of, a
`fallback:`-prefixed one — as retriable (`:652-659`, `:1064`) and retries it
indefinitely; a daemon-side cgroupfs failure emitted without `reject:` would
silently stall every aitest run on the machine forever, not just the job
that hit it. Note for the builder:
`~/.config/systemd/user/aira-daemon.service` carries no
`ProtectControlGroups`/cgroup sandboxing, so a daemon-side `mkdir` under a
user-delegated outer scope is expected to work in the normal case — the
`reject:` class above covers only the residual failure where it doesn't.

Moving worker-scope creation into the daemon also removes the CLI-side
`runner.CreateWorkerScope` call (`cmd/aira/main.go:1080` — **precision
correction, this revision: a prior draft cited `:1081` here; §2 and matrix
row 0 already had the correct `:1080`**) and changes the
lease shape in `internal/runner/worker_admit_client_linux.go` — both are Fix 2
files, which is exactly why the 1 → 2 ordering above is load-bearing, not just
a convenient default.

**AIRA-63, corrected:** a prior draft claimed this fix "moots AIRA-63." It
doesn't follow — AIRA-63 is about `workerAdmitConnection` (begins at
`worker_admit.go:364`, corrected from a prior draft's `:356-430`) retaining a
connection and a polling goroutine per waiter with no `admitSlots` gate,
which a ledger-source change alone does not touch. Resolved here instead of left open: gate `workerAdmitConnection` on the
existing `admitSlots` semaphore as part of this fix — a deletion of an
asymmetry (waiters already respect `admitSlots` everywhere else), consistent
with the owner's stated preference for deletion over new machinery — and close
AIRA-63 as part of this fix rather than leaving it in Phase 2.

**AIRA-63's outcome-channel sequencing, fixed this revision:** gating
`workerAdmitConnection` on `admitSlots` emits a new busy-shaped denial on the
worker-admit path *before* Fix 2's structured outcome channel exists to carry
it — a real hazard, not a hypothetical one. **Classifier mechanism, corrected
this revision — the prior wording implied `supervisor.py` recognises a
`fallback:` prefix specifically; it does not.** `supervisor.py`'s classifier
(`:652-659`) routes on the presence or absence of the literal substring
`reject:` in a "worker-admit denied" message: `reject:`-containing messages
raise the terminal `WorkerAdmitRequestTooLarge`; **any other** "worker-admit
denied" message — including one with no recognised prefix at all — falls
through to the retriable `WorkerAdmitDenied` path, not to
`WorkerAdmitUnavailable` (that classification is reached by a different
condition entirely — an unrecognised message shape, not a denial). So the fix
works because a `fallback:`-prefixed reason simply isn't `reject:`-prefixed,
not because the classifier specifically recognises `fallback:` as meaningful.
**Resolved: Fix 1 emits the busy outcome as a denial whose reason is NOT
`reject:`-prefixed** (e.g. `worker-admit denied:
fallback:admit-slots-saturated`, keeping the `fallback:` prefix as a
human-readable convention, not a matched token) — no dependency on Fix 2's
structured channel is introduced, and Fix 2 can later fold this into its
stable `state=` k=v convention without a behavior change. `_wait_for_admission_or_disable`
(`:1064`) retries such a denial indefinitely with periodic stderr warnings —
this is the intended behaviour for transient slot saturation, bounded by
slots freeing up, not a hang. **Ticket follow-through, stated
explicitly rather than left implicit:** once `workerAdmitConnection` is
bounded by `admitSlots`, `workerAdmitWaitCeilingMs` *may* be unified with the
shared `runner.AdmitWaitCeiling` and
`TestWorkerAdmitCeilingStaysBelowTheSharedAdmitCeiling`
(`admit_freeze_test.go:449`) relaxed accordingly — **not
done in this fix**; the split ceiling stays as today's deliberate, documented
asymmetry until a follow-up explicitly revisits it.

**Release path, corrected this revision — the daemon-side rmdir this section
previously proposed is deleted; the primary release path already exists and
needs no new code.** Under `committed = Σ memory.max` over existing children,
a worker scope that dies without being `rmdir`'d keeps charging against the
ledger — the safe direction (over-charging, not under-charging).

**Primary release** is `supervisor.py`'s existing `_retire_worker`
(`internal/pylib/aitest/supervisor.py:1040-1062`): it reaps the worker first
(`_reap_child`), closes the admit relay stdin (lease close →
`releaseWorkerGrant`), then `os.rmdir(grant["scope"])`. Because the worker is
reaped before the rmdir, the scope is already empty by the time it runs — no
daemon-side path is needed for this case.

A prior draft of this section proposed the daemon remove the scope itself on
lease-close, justified by the claim that "the outer confine scope's own
teardown is daemon-driven post-#74." **That claim is false, and the proposal
is deleted along with it:** outer-scope teardown is `attestScopeTeardown` in
the runner (`internal/runner/confine_linux.go:856`) — client-side, not
daemon-driven; the daemon only ever `rmdir`s orphaned scopes via the #72
reaper (`internal/daemon/eject.go:325`). A daemon-side rmdir-on-lease-close
would also be actively wrong in the AIRA-41 case (the relay is killed but the
worker is still alive): the cgroup is still populated, so the rmdir would
fail with `EBUSY` — the scope correctly keeps charging until the worker is
actually reaped, which is the behavior this design wants, not a bug to route
around.

**Backstops:** the outer confine scope's own client-side teardown on job
exit, and the #72 reaper for a supervisor crash mid-retire.

**AIRA-41 closes by construction under this design:** a killed relay no
longer silently frees the ledger, because the ledger charges the scope
itself rather than tracking the relay connection, and the scope is only
removed once the worker inside it has actually been reaped.

**This fix does not need to explain AIRA-91** and must not be sold as doing so
— the sweep found it predicts the wrong exit code (137/signal-derived, not 0)
for what's actually been observed. It's justified on its own: a real,
independently-confirmed restart-survivability gap in the RAM-safety ledger.

### 3.2 AIRA-28/AIRA-29 — decision now, build later (split out of the Phase 1 chain)

**Resolution note:** a prior draft of this plan called this "Fix 5" and put it
in position 2 of the 1 → 5 → 2 → 4 chain. Plan-review found that mis-sized:
AIRA-29's real footprint is far larger than a single `admit.go` change, it
carries a hard precondition this plan hadn't engaged, and it doesn't belong
ahead of the smaller, better-defined Fixes 2 and 4 in a serial chain. Resolved
in the reviewer's favor — the underlying claims about AIRA-29's scope were
wrong. It's now split into (a) a Phase 0 status-change commit and (b) a
separate follow-on milestone, neither of which is part of the Phase 1 chain
above.

**(a) Close AIRA-28 as superseded — Phase 0, status-change only (see §2
table).** AIRA-28's own ticket body already reads "SHELVED — SUPERSEDED BY
AIRA-29" and the `supersedes` relation is already recorded; the owner decision
was made 2026-09-01. This is recording a decision already made, not making a
new one — still flagged below for explicit sign-off since it retires a real
airtight-guarantee design (keep its branch/spec as reference, don't discard
the analysis).

**(b) AIRA-29's build is NOT part of this plan — it is its own follow-on
milestone with its own re-based plan.** The AIRA-29 spec (branch
`aira29-dynamic-reserve`, docs-only, three commits, not on `master`) declares a
hard **ship-together precondition** — quoted accurately this revision, the OR
clause matters: **"Building Slice 1 requires Slice 2 in the same deploy, OR
explicit owner acceptance of an aggressive-`memory.high` interim"** (fork D of
that spec). Slice 1 alone (charge by live `memory.current`) is a strict safety
*regression* for the airtight non-delegate class on its own — it must ship
together with Slice 2, or the owner must explicitly accept the interim. So
"sized smaller than its original framing" must never be read as "Slice 1
alone." Its real footprint, corrected from the single-file
`internal/daemon/admit.go` a prior draft claimed: `admit.go` charge logic, a
periodic `memory.high` re-writer plus the `oom_score` seam in
`confine_linux.go`, a **new** recursive subtree PID walker in
`cgroup_linux.go`, a daemon-wide compliance loop, `install.go`'s `memory.high`
formula, and removal of the xdist plugin's per-test RAM lease (which collides
with AIRA-33's deletion — coordinate, don't duplicate). **Its hold reason is
body text only, not the front-matter `hold` field** (the ticket's front matter
reads `hold:false`; the hold is stated in prose) — must be answered explicitly
before unholding, not silently dropped: the utilization problem AIRA-29
addresses persists for non-delegate confine jobs independent of aitest, and
the parts of the original design that really were xdist-specific are deleted
by AIRA-33 anyway — so the hold no longer applies, but the follow-on plan must
say so, not just unhold. **New item to answer before unholding, added this
revision:** AIRA-91's now-closed root cause (§1) directly undercuts this
spec's own §3.5 load-bearing mechanism — a periodic `memory.high =
effectiveCharge` re-writer on every live scope, deliberately keeping a
well-sized worker in sustained reclaim throttling — because AIRA-91 proves
that exact `memory.high` reclaim-throttling is the PSI source that trips
`systemd-oomd`. The follow-on plan must reconcile §3.5 with whatever the
AIRA-91 Part B decision turns out to be (§5 item 5) before this design can be
treated as safe to build, not just re-scope the ticket's other stale claims.

AIRA-16's second half (a slice-internal watchdog trigger) is **re-pointed this
revision at the AIRA-91 Part B decision (§5 item 5), not at the AIRA-29
follow-on** — it is the same design space (who kills what under
slice-internal pressure: `systemd-oomd` or the AIRA watchdog), and Part B is
the nearer-term, explicitly-required owner decision; see §4 and §7 for the
corrected pointers. AIRA-24's fourth ask (saturation-wait UX tied to real
headroom) does **not** wait on AIRA-29: the other three asks in AIRA-24 are
already landed, and the queue-position ask is actioned directly in Phase 2
(§4), after **Phase 0** — corrected this revision, there is no "Phase 1
admit.go chain" (§3 file-touch matrix: no Phase 1 fix touches `admit.go`);
AIRA-24 depends on Phase 0's AIRA-52+23 (`admit.go:1367`) and AIRA-44
(`confine_linux.go`) instead. (A prior draft said both stayed "individually
scoped in Phase 2/Phase 0 as already assigned" — false, since neither was
actually assigned anywhere; these are the corrected pointers.)

**If Fable or the owner disagrees with treating AIRA-28's closure as settled,
or with pulling AIRA-29's build out of this plan's chain, these are the two
items in this section to escalate rather than silently proceed on.**

### 3.3 Fix 2 — Structured daemon↔client outcome channel

**Closes/moots AIRA-42, AIRA-45; closes AIRA-83(b). AIRA-87 is addressed by a
separate mechanical follow-on PR, not folded into this one — see below (a
prior draft folded it directly into this fix, which plan-review found would
make the Tier-A PR unreviewable).**

The `WorkerAdmitResponse` type this fix restructures lives in
`internal/daemon/worker_admit.go`, mirrored in
`internal/runner/worker_admit_client_linux.go` — not in `protocol.go` (matrix
above corrected accordingly). Because this changes the wire shape of that
response, **it bumps `daemon.ProtocolVersion`**, which forces a coordinated
daemon restart and a re-extracted pylib (`aitest`'s embedded copy) — call this
out explicitly in the PR description and in the deploy step (§8).

**One structured channel, not two — stated explicitly this revision, since a
prior wording read as keeping the old prose convention alongside a new
field.** Emit `AdmitResponse.State` as `state=<enum>` plus
`reason=<class>:<detail>`, where `reject:`/`fallback:` become the exact-match
`<class>` value of the `reason` field, not a substring anyone parses out of
free text. `supervisor.py`'s six-times-patched substring matching on the raw
message is **deleted, not supplemented** — Python matches `state`/`reason`
class by exact enum value, and anything unrecognised is an explicit error,
never silently "unavailable". If any prose-substring matching on the message
text survives after this fix in any code path, Fix 2 has not actually closed
the class it exists to close. This directly fixes the AIRA-83(b) mismatch
(protocol-version skew currently misclassified as a sizing error) as one
instance of the same channel.

**AIRA-87 follow-on PR (lands after Fix 2, not inside it).** Move
`store.ExitCodes` (`internal/store/check.go:14`) into a leaf package, per the
ticket's own item 1, and add the produced-vs-catalogued test. Sequenced
strictly after Fix 2 because it touches every `store.ExitForCode` call site
across `cmd/aira` and `internal/core` (`response_contract.go`, `skill.go`) —
folding a repo-wide mechanical move into the same PR as the Tier-A daemon↔
client wire-shape change would make that PR unreviewable. Per AIRA-87's own
note, the produced-vs-catalogued test must not encode the bucketing AIRA-45
changes. This closes most of AIRA-87: the two AIRA-42/45-classifier crossovers
close fully via Fix 2 itself; a residual few unrelated drift instances the
sweep found (e.g. `E_RANT_REDACTED`/`W_GATE_PROOF_EXPIRING`) are swept up by
this follow-on PR's declare-once move but aren't separately designed here.

### 3.4 Fix 4 — Symmetric deadline-policy seam

**Closes AIRA-84.** One deadline-policy seam applied to both `exchange()`
(client) and `serveConnection()` (daemon): a short connect/parse deadline, with
the actual response wait driven by the caller's context/signal or a
verb-declared budget instead of the same 30-second constant re-used for
everything. This is the same class already fixed twice tonight (AIRA-18,
AIRA-92) — worth naming as a durable convention in the commit, not just a
one-off patch, since a `forbidigo` rule (Phase 2, tooling) will hold it there
going forward.

**Closed site list, corrected this revision — a prior wording put
`storeops.go:191` in scope for the wrong reason.** The AIRA-84 ticket names
only `server.go:527` (the connect deadline) and its `:668` routed write; this
plan widens that to the client `exchange()` (`protocol.go:337-341`, `:337`
for the deadline itself). Four constants total, each stated explicitly so
"symmetric" has a defined boundary:
- **In scope:** `server.go:527` (the connect deadline this fix replaces),
  `server.go:668` (the routed write — this is what needs the actual seam,
  since it currently writes under the same stale connect-time deadline),
  `protocol.go:337` (client `exchange()`'s mirrored deadline), and
  `server.go:155` (`storeOpWriteTimeout`, the store-op *write* deadline —
  this is the genuine "same treatment as store-ops" precedent AIRA-84's own
  text points at: the socket-level `SetReadDeadline(time.Time{})` /
  `SetWriteDeadline(storeOpWriteTimeout)` pattern at `server.go:548-551`,
  which the routed path lacks and this fix gives it).
- **Out of scope:** `internal/daemon/storeops.go:191` and `server.go:153`
  (`storeOpAppendTimeout`) — **corrected, this revision: these are
  `context.WithTimeout` execution-*budget* fallbacks for an op's own work,
  not socket deadlines, and `:191` is reached only when
  `storeOpAppendTimeout`/`storeOpHeavyTimeout` are configured `≤0`; both
  default to positive values at `server.go:153-155`, so `:191` is
  effectively dead in the normal case.** They answer "how long may this
  operation run," not "how long may the connection sit idle," and folding
  them into the connection-deadline seam would conflate two different
  budgets. `cmd/aira/watch.go:18` (`watchExchangeTimeout`) stays **out of
  scope** as already stated: `aira watch` is a long-lived streaming
  connection by design, not a request/response round-trip, so the same
  short-connect/verb-declared-budget shape does not apply without its own
  separate design.

Add the in-scope files (`server.go:527,155,668`, `protocol.go:337`) to the
§3 file-touch matrix's Fix 4 row.

### 3.5 Fix 3 — Captured-subject type for gate evaluators (+ AIRA-60, AIRA-86 seed sites)

**Closes AIRA-80, AIRA-81, AIRA-60; closes the `gate_eval.go`/`gate_ratchet.go`
portion of AIRA-86 (its remaining `check.go:130` seed site stays a standalone
Phase 2 item, §4).** **Gate corrected this revision — the Phase-0 gate a
prior revision imposed here is spurious and is dropped:** that gate was
justified by AIRA-60 and AIRA-86 sharing this fix's exact files, but AIRA-60
and AIRA-86's `gate_eval.go`/`gate_ratchet.go` sites are folded **into** this
fix rather than staying separate Phase 0/Phase 2 items (below) — once folded
in, no *remaining* Phase 0 item touches any of this fix's six files
(`gate_eval.go`, `gate_command.go`, `gate_ratchet.go`, `canary.go`,
`traceability.go`, `gate_subject.go` — corrected this revision from "five",
matching the matrix's addition of `gate_subject.go` above): AIRA-89 only
deletes symbols in `gate_index.go`,
`gate_audit.go`, `testreport.go`, and AIRA-56 only touches
`relation_ready.go`/`gate_index.go` — none of Fix 3's files. **Fix 3 therefore
starts immediately, in its own worktree, not gated on Phase 0 at all** — its
only remaining gate is the §5 item 2 owner-decision default (proceed at Tier
A unless the owner objects, §5). This fix now folds in Phase 0's AIRA-60 and
Phase 2's AIRA-86 seed sites, which share this fix's exact files
(`gate_command.go:358`, `canary.go:189`, `gate_eval.go:535` for AIRA-60;
`gate_eval.go:99`, `gate_eval.go:597`, `gate_ratchet.go:80` for AIRA-86). That
overlap makes the matrix's original "overlaps with: none" wrong; folding all
three into one PR is exactly the kind of unification the owner's stated
preference favors over three separate touches to the same files.

Direct sibling of tonight's AIRA-72 fix, applied to the two lanes AIRA-72
didn't touch. Capture the subject tree once (`stableSubjectEntries`, already
exists) and change every evaluator's signature from "takes a root path,
re-reads it" to "takes an already-captured subject" — removes a redundant
tree-read (dimension lane) and a redundant re-index round-trip that currently
silently drops tracked-but-gitignored files (canary/mutation lane), confirmed
as a live false "canary fired" hazard. In the same PR: collapse
`safeFixturePath`/`safeSnapshotPath`/`safeMutationPath` into one exported
`gate.SafeRelativePath` (AIRA-60) and use it in the canary seed-loop path
validation that was missing it (net negative lines); and seed the
`gate_eval.go`/`gate_ratchet.go` honesty-dimension sites `unevaluated` rather
than `pass` (AIRA-86's non-`check.go` sites). Add the generic property test
proposed in the sweep, sharing one "stored pass invalidated by any mutation"
property across all three tickets: for every gate/canary kind, mutating any
tracked file (including a gitignore-matched one, a mode bit, a symlink target)
invalidates a stored pass — one test instead of one per kind, and it also
covers the seed-site fix (an `unevaluated` seed can never masquerade as an
invalidated `pass`).

**AIRA-78 is contingent, not included in this fix's scope.** It shares the same
invariant but its evidence side needs a schema change to test reports (zero
rows in production today) or the ratchet gate kind needs to be deleted with
them — that's a second owner-facing call this plan does not make for Fable.
Recorded as a decision point in §5, not actioned in Phase 1.

**Production-footprint note, added this revision — see §5 item 2 for the full
disposition question.** A fresh read-only count of
`~/.local/state/aira/state.db` (2026-09-04) shows the *entire* gate
subsystem — not just AIRA-78's ratchet kind — has **zero rows** in
production: `gates`, `gate_results`, `gate_proofs`, `gate_attestations`,
`gate_baselines`, `gate_baseline_active`, `test_reports`, and
`test_report_results` are all empty. Fix 3 remains justified on its own
honesty-defect merits (AIRA-80/81 are real, and the fix is net-negative
lines), but the owner should see this fact — a Tier-A two-loop refactor of a
subsystem nobody currently uses — before Tier-A review capacity is committed
to it; §5 item 2 asks the question explicitly.

### 3.6 Fix 5 — Confine trailer kill-attribution (AIRA-70 unified with AIRA-91 Part A)

**Closes AIRA-70 and AIRA-91 Part A together — added and unified this
revision.** The prior draft carried AIRA-70 as a standalone Phase 2 (Tier B)
item (§4) and treated AIRA-91 as wholly out of scope (§1). Both are now wrong:
AIRA-91's root cause is closed (§1), and its Part A fix direction is the
*exact same* confine-trailer kill-attribution surface AIRA-70 already
targets — `FormatConfineStatus`, `formatConfineReserveAdvisory`
(`confine_linux.go:872-890`). Building two separate trailer changes would
duplicate work and risk two differently-shaped `terminated-by=`-style fields
landing out of sync; unified here into one fix instead. **Re-tiered Tier A
(full two-loop) rather than Phase 2 Tier B**, since AIRA-91 Part A is P0.

**One `terminated-by=` facet, four values** — reconciling AIRA-91's own fix
direction with AIRA-70's original three-way ask:
- `oom_kill > 0` → the existing kernel-OOM advisory (unchanged).
- signalled with `oom_kill == 0` → **new, the AIRA-91 gap**: report plainly as
  an **external whole-cgroup kill**, naming the realistic sources
  (`systemd-oomd`, `cgroup.kill`, `aira confine --kill`) — today the trailer
  prints identically to a clean run in exactly this case.
- SIGINT/SIGTERM delivered to the confine supervisor itself → log the signal
  and its forwarding in the supervisor's own handler (AIRA-70 finding #1; no
  log line exists today).
- an external `aira confine --kill` from another session → log the killer's
  identity in the daemon's `confine-kill` dispatch
  (`internal/daemon/confine_manage.go:160-172` — **precision correction, this
  revision:** `:141-152` is the `--list` summary struct, not the kill
  dispatch; AIRA-70 finding #2; no `log.Printf` (or any `log.` call) exists
  anywhere in the file today).

**Do not build two trailer changes** — this is the one place both tickets'
fix directions land. Tier A: full two-loop per this project's ID/crash-
recovery/lease-CAS bar (correctness-adjacent daemon/supervisor signal-handling
work), the same rigor AIRA-70's own ticket already asks for.

**Verification, corrected this revision — the probe is not actually committed
anywhere, contrary to this repo's CLAUDE.md committed-reproduction rule.**
`~/tmp/aira91-probe/` (a Makefile plus `selfkill`/`groupkill`/`groupkill_dr`/
`hog` logs) exists on this machine but is untracked (`git ls-files` finds
nothing under it; only `.aira/tickets/AIRA-91.md:460` and this plan mention
it). **Ordering, corrected this revision (see §8):** the probe commit is
split out and lands *before* Fix 1, not inside Fix 5's own PR, so Fix 1's
deploy is never silently gated on Fix 5 merging first — either the probe
Makefile itself under a repo path, or an equivalent Go test in
`internal/runner` that writes to a confine scope's `cgroup.kill`, committed
as its own small tests-only commit ahead of both Fix 1 and Fix 5. **Fix 5's
PR then extends that already-committed probe** with the assertion that the
new `terminated-by=` field reports `external-cgroup-kill` against it — so the
two-loop reviewers can reproduce the verification without depending on an
uncommitted `~/tmp` directory. Once committed, confirm the
`groupkill`/`groupkill_dr` probes report `external-cgroup-kill`, and the
`selfkill` probe continues to report
the correct path (kernel-OOM advisory or signal-forwarding, per which probe
targets which mechanism).

**Sequencing:** shares `confine_linux.go` with Phase 0's AIRA-44 and
AIRA-52+23 (§2, §3 file-touch matrix) — starts once Phase 0 has merged.
**Corrected this revision: no longer "same as Fix 3"** — Fix 3 itself now
starts immediately, not gated on Phase 0 (§3.5); Fix 5 still waits on Phase 0
and builds concurrently with Fix 3 (and Fixes 1/2/4) in its own worktree,
independent of the 1 → 2 → 4 `worker_admit.go`/`admit.go` chain.

## 4. Phase 2 — Individual fixes (Tier B)

Narrow, real, no clustering or deletion angle — reported honestly as such by
the sweep rather than forced into a pattern. **Not fully independent of Phase
1**, despite a prior draft's framing — corrected, explicit dependency list:
AIRA-24 (queue position comes from the daemon admit queue,
`internal/daemon/admit.go`, and the client progress line at
`internal/runner/confine_linux.go:539-542`) lands after **Phase 0**, not "the
Phase 1 admit.go chain" — **corrected this revision: no Phase 1 fix touches
`admit.go`** (§3
file-touch matrix); AIRA-24 waits on Phase 0's AIRA-52+23 (`admit.go:1367`)
and AIRA-44 (`confine_linux.go`) instead; **AIRA-40 and AIRA-37 residue #2
land after Fix 2 (shared supervisor.py classifier); no live branch carries
unmerged supervisor.py work** — corrected this revision: a prior draft
required rebasing onto the live `investigate-aira91-92-aitest-contention`
branch, claiming it "currently carries +409 lines in
`internal/pylib/aitest/supervisor.py`"; verified false at master `2af6d66`
(`git diff master investigate-aira91-92-aitest-contention --
internal/pylib/aitest/supervisor.py` is 0 lines — that branch's single commit,
AIRA-92, is already on master as `ac901cb` and was deployed as `7472b48`; the
branch is 21 commits behind master and carries no unmerged work); AIRA-20 (wall-clock waits
across 27 test files in `internal/runner` and `internal/daemon`) lands
**last**, after every Phase 1 fix's test changes, so it isn't hardening tests a
later fix immediately touches again, and additionally depends on AIRA-33
landing (or a quarantine) for its own `-race` restoration — see its table
entry below; AIRA-22 lands after AIRA-85 and Fix 2 (they share
`detach_linux.go` and `main.go`); AIRA-43 lands after Fix 2, as already
stated. **AIRA-70 has moved out of this phase this revision** — unified with
AIRA-91 Part A and re-tiered Tier A in Phase 1 (§3.6), since AIRA-91 Part A is
P0. The remainder (AIRA-73, AIRA-16 first half, AIRA-82, AIRA-37 residue #1,
AIRA-86 remainder) are genuinely independent of Phase 1 and of each other, and
can build in parallel once Phase 0 is merged. Ordered here by size, smallest
first, within that constraint.

| Ticket | Fix |
|---|---|
| AIRA-73 | **P1, added on plan-review — missing from a prior draft entirely. Default flipped to deletion this revision — see §5 item 4 for the reasoning and full file list.** `outbox.resolution` is never written — only `resolution IS NULL` predicates exist, at `internal/store/finding.go:61`, `requirement.go:282`, `store.go:803,1755,1898,1933,2051`, `import_requirements.go:361,384,447`, `lifecycle.go:290`. **Default: delete** the outbox-resolution mechanism (the column, its predicates, and now-dead references) under PR #12's "one truth per entity" proposal, unless the owner objects (§5 item 4). **Fallback if the owner keeps the mechanism:** add the missing write path instead. Production `outbox` has 0 rows with `resolution IS NOT NULL` and 0 unmaterialised rows (read-only count, 2026-09-04) — either option is data-safe today. |
| AIRA-16 (first half) | **Added on plan-review — a prior draft left this dangling.** Stop blanket-exempting a genuinely-uncapped `.aira` scope in the watchdog's offender predicate (`watchdog.go:344`, `:553`, `:825`). **Second half (a slice-internal trigger), re-pointed this revision:** deferred to the AIRA-91 Part B decision (§5 item 5) — same design space, who kills what under slice-internal pressure, `systemd-oomd` or the AIRA watchdog — not to the AIRA-29 follow-on milestone as a prior draft had it; not built here either way (§3.2, §7). |
| AIRA-82 | Add an explicit per-call scope override on the MCP/CLI faces (the actual code half of the re-scoped ticket; Phase 0 already corrected its framing). |
| AIRA-37 residue #1 | Fix the stale atfork docstring in `worker.py`. |
| AIRA-37 residue #2 | Add the missing decrement to `supervisor.py`'s worker-dispatch queue check (currently `if not self.queue: break` with no counter, allowing over-spawn). |
| AIRA-40 | Replace pipe-EOF worker-liveness inference with a `pidfd` in the same `select()` set. |
| AIRA-43 | Add the missing granted-path stdin-hold test harness (sequence after Phase 1 Fix 2, since the output shape it tests changes there). |
| AIRA-24 | Add queue position to the admission saturation-wait message (the other three asks in this ticket are already landed). |
| AIRA-85 | Fold the detached supervisor's `state.db` writes into the existing relay path (`AddTestReport`/`AddComputeEvent` already implement the daemon side); name the structural gap ("nothing enforces single-writer" beyond convention) explicitly in the commit even though closing it fully is Phase 3 tooling (see §6). |
| AIRA-86 (remainder) | Seed all fourteen honesty dimensions `unevaluated`, never `pass`, at the one seed site that stays standalone here: `check.go:130`; add the per-dimension pass tests. Its `gate_eval.go` ×2 and `gate_ratchet.go` seed sites move into Phase 1 Fix 3 instead (§3.5) — corrected on plan-review, since they share Fix 3's files and its "stored pass invalidated by any mutation" property test. |
| AIRA-20 | Package-wide pass replacing fixed wall-clock test deadlines with condition-based waiting or an `AIRA_TEST_DEADLINE_SCALE`-driven helper, across `internal/runner`, `internal/daemon`, then **`internal/pylib`** (added this revision — precision correction below). Skip `governor_slot_test.go` (deleted by AIRA-33 once that lands) rather than hardening tests about to be removed. Restore the `-race` CI job once the sweep is clean. **Dependency named explicitly, this revision:** restoring `-race` CI is contingent on `TestRealPytestRAMForkDoesNotPinHelperStdin` (AIRA-65, on the xdist-governor path — **precision correction: this test lives in `internal/pylib/pytest_integration_test.go:606`, not `internal/runner`/`internal/daemon` as a prior draft implied; `internal/pylib` is added above to the packages this pass covers because of it**) being gone or quarantined — it is deleted only when AIRA-33 lands, and AIRA-33 is itself **blocked** on AIRA-91 (§1: Part A built + fastest-ee re-verified + the `FASTEST_NO_AITEST=1` pin removed). Until then, restoring `-race` leaves that one known load-flaky test inside the suite the restored job would otherwise declare clean — either wait for AIRA-33, or explicitly quarantine that one test (e.g. `t.Skip` pointing at AIRA-65/33) before restoring `-race`, and say so in the PR rather than restoring CI silently with a known flake still inside it. |
| AIRA-22 | `confine --detach` — genuine feature, not cleanup. Converge onto the existing `LaunchDetached` machinery rather than building a parallel detach path. Full two-loop per CLAUDE.md's ID/crash-recovery/lease-CAS bar, since it's new durable-status surface. |

## 5. Decision points this plan surfaces rather than resolves silently

1. **AIRA-28 vs AIRA-29** (§3.2) — corrected on plan-review from a prior
   draft's "closing 28 and building 29" framing. This plan proposes closing
   AIRA-28 as superseded-by-decision as a Phase 0 status-change commit (the
   decision itself was already made by the owner on 2026-09-01; only the
   transition is missing). AIRA-29's build is **not** part of this plan — it
   ships as its own follow-on milestone once its ship-together (Slice 1 +
   Slice 2) precondition and its hold reason are answered explicitly (§3.2,
   §7). Both halves are flagged for explicit Fable/owner sign-off.
2. **AIRA-78's ratchet gate, and the wider gate subsystem's production
   footprint — widened this revision.** A fresh read-only count of
   `~/.local/state/aira/state.db` (2026-09-04) shows **zero rows** in
   `gates`, `gate_results`, `gate_proofs`, `gate_attestations`,
   `gate_baselines`, `gate_baseline_active`, `test_reports`, and
   `test_report_results` — not just the ratchet kind AIRA-78 is about, the
   **whole gate subsystem has no production consumer today.** That means
   Fix 3 (§3.5) — a Tier-A two-loop refactor — is proposed for a subsystem
   nobody currently uses. **Not actioned in this plan either way; the owner
   should decide, before Tier-A effort is spent, whether Fix 3 (a) runs now
   at Tier A as planned, (b) is deferred until some gate is actually
   configured, or (c) — per this plan's own deletion preference — the
   ratchet kind (and possibly more of the subsystem) is deleted now instead
   of fixed.** This is not a reason to block Fix 3 — AIRA-80/81 are real
   honesty defects independent of production usage, and the fix is
   net-negative lines (§3.5) — but the owner should see the zero-row fact
   first. AIRA-78's own fork (keep, with a schema change giving test reports
   a producer, then apply the Fix-3 pattern to it; or delete the ratchet
   kind entirely) is a narrower instance of this same question. **Executor
   default, added this revision, so this decision point does not silently
   block §3.5's schedule (which starts Fix 3 immediately, not gated on Phase
   0 any more — §3.5):** default = **(a)** — Fix 3 proceeds at Tier A as
   scheduled unless the owner objects. AIRA-80/81 are real P1 honesty defects
   independent of whether the subsystem has a production consumer, and the
   fix is net-negative lines, so proceeding is the safe default while the
   owner's answer on (b) deferral or (c) deletion is pending; if the owner
   answers (b) or (c) before Fix 3's worktree starts, that answer overrides
   this default. **A second, narrower point, added this revision — corrected
   from an "executor default" framing that would have contradicted §1, §3.5
   and §7's own "AIRA-78 is decision-only, not built" statements: AIRA-78
   itself is P0, and the default above only answers whether Fix 3's general
   Tier-A work proceeds — it does not answer AIRA-78's own keep-vs-delete
   fork, which had been left unaddressed, parking a P0 with no recorded
   reasoning at all.** **Recommended disposition, recorded on the ticket
   (§2's new Phase 0 row), awaiting explicit owner sign-off — NOT actioned
   anywhere in this plan's build (consistent with §1, §3.5, §7):** delete the
   ratchet gate kind (the narrow form of option (c) above; consistent with
   §0's deletion preference). **Fallback if the owner keeps it instead:**
   apply the Fix-3 captured-subject pattern to it once a test-report producer
   exists — the original, larger option. **Either way, Phase 0's ticket-text
   commit records on AIRA-78 itself why this P0 is not being built now** ("no
   producer → latent, not live") so the severity is not silently ignored by a
   reader of the ticket alone — but building either the deletion or the
   fallback remains gated on the owner's answer, exactly like AIRA-28/29 and
   AIRA-91 Part B elsewhere in this plan, not scheduled as a default the
   executor proceeds on.
3. **AIRA-85's structural gap** ("nothing enforces single-writer beyond
   convention") is only partially closed by folding in the detached
   supervisor's writes (Phase 2, AIRA-85). **Correction, this revision — the
   proposed enforcement rule targeted the wrong constructor and is dropped:**
   the daemon opens the DB via `store.OpenDB` (`internal/daemon/server.go:268`),
   not `store.Open`; the CLI fallback (`cmd/aira/dispatcher.go:639`) and the
   detached supervisor (`cmd/aira/main.go:387`) both legitimately go through
   `app.OpenWithDiagnostics` → `store.Open` (`internal/app/project.go:227,384`)
   as their own correct bootstrap path. A rule forbidding `store.Open(`
   outside `internal/daemon` would fail on day one against that legitimate
   path, and would not even catch the supervisor's actual defect (it opens
   the DB correctly; the defect is writing directly afterward instead of
   relaying through the daemon). **This plan drops the enforcement-rule idea
   for this item** in favor of AIRA-85's direct fix (relay the supervisor's
   writes, §4) plus a review-checklist note recording the residual gap
   ("nothing prevents a *future* direct write path outside the daemon;
   caught by code review, not a test") — consistent with §6's item-11
   correction and this plan's own preference for the simpler form when a
   mechanical rule would otherwise be either vacuous or wrong. Whether to add
   a runtime flock assertion instead remains a follow-up, not required for
   this plan's scope.
4. **AIRA-73's outbox-resolution mechanism** (§4) — added on plan-review.
   **Executor default, added this revision, so this decision point does not
   silently block §4's schedule (which previously said "add the write path"
   as if that were already decided):** default = **the deletion candidate**,
   per §0's stated owner preference for deleting/simplifying over adding —
   drop `outbox.resolution` and its now-pointless `resolution IS NULL`
   predicates (`internal/store/finding.go:61`, `requirement.go:282`,
   `store.go:803,1755,1898,1933,2051`,
   `import_requirements.go:361,384,447`, `lifecycle.go:290`) under PR #12's
   "one truth per entity" proposal, unless the owner objects. **Fallback:**
   if the owner wants to keep the mechanism, build the missing write path
   instead (§4's original framing, now the fallback rather than the
   default). Production `outbox` has **0 rows with `resolution IS NOT
   NULL`** and **0 unmaterialised rows** (read-only count, 2026-09-04) — so
   either option is data-safe today, which is why this plan can default to
   deletion without risk to live data. Not decided here; left for explicit
   sign-off alongside item 1, but §4's execution does not block on that
   sign-off arriving first — see the default above.
5. **AIRA-91 Part B — oomd-vs-admission policy — added this revision.**
   AIRA-91's root cause (§1) is that AIRA's own oomd enablement
   (`ManagedOOMMemoryPressure=kill` on `user-1000.slice`, tightened
   `ManagedOOMMemoryPressureLimit=40%`, `ManagedOOMPreference=avoid` on
   `session.slice` — AIRA installs this itself,
   `internal/install/install.go:967-970`) kills the highest-PSI-pressure
   cgroup, while aitest worker scopes are deliberately sized into sustained
   `memory.high` reclaim throttling (`worker_admit.go:285`) — the two
   AIRA-owned mechanisms actively conflict. Candidates, each trading one
   failure mode for another (per the ticket): `ManagedOOMPreference=avoid` on
   `aira.slice` itself; raising/removing worker `memory.high` so a
   well-sized worker isn't permanently in reclaim (this is what AIRA-35, §2,
   is now contingent on); restoring stock oomd thresholds specifically for
   `aira.slice`. **This needs explicit owner sign-off before any of it is
   built** — the ticket states this explicitly; it is not a
   Fable-plan-review-and-proceed decision the way most of this plan's other
   items are. AIRA-16's second half (a slice-internal watchdog trigger, §4)
   is re-pointed at this same decision rather than at the AIRA-29 follow-on
   milestone — it is the same design space: who kills what under
   slice-internal pressure, `systemd-oomd` or the AIRA watchdog.

## 6. Tooling/lint follow-ups (apply once, benefit forever)

Six concrete prevention mechanisms came out of the sweep, independently
rediscovered by more than one cluster — that repetition is itself signal these
are real classes, not coincidences. **Correction, this revision: not all six
default to landing as Phase 2 code.** Items 3, 4, and 6 are cheap relative to
the fixes above and land as part of Phase 2 (they're small, mechanical, and
already implemented inside the fixes that produce them); item 5 is a
checklist practice, not code, by design. **Items 1 and 2 default to the same
checklist form as item 5 (below), opt-in only if the owner wants them
automated** — consistent with this plan's own stated preference for
unification and the simpler form over new machinery.

**Enforcement correction (a prior draft would have shipped a vacuous gate):**
`make ci` runs `fmt-check vet build test` with no lint step; `make lint` only
runs `golangci-lint` when it's on `PATH` and otherwise prints "lint skipped"
(non-failing); `.golangci.yml` has no `forbidigo`/`depguard` rules configured;
and `semgrep` is not present in this repo at all. As originally written, items
1 and 2 below would have enforced nothing — this repo's CLAUDE.md forbids
inventing a vacuous gate. **Preference correction, this revision:** per the
owner's stated preference (§0) and this plan's own item-5 reasoning (a
checklist grep beats new machinery when the check is cheap enough), **items 1
and 2 below are opt-in owner decisions, defaulting to the review-checklist
grep form** — this plan does not commit to building three new custom
AST-walking Go tests plus Python grep tests as a side effect of a backlog
sweep. **If the owner opts in to automating either one instead**, it must
still honor the enforcement correction: every automated rule is either (i)
added to the `ci` target as a fail-if-unavailable lint step, so a missing
tool fails the build instead of skipping silently, or (ii) implemented as a
plain Go test (a grep/AST check in-package) that needs no new tool at all.
**`semgrep` is not introduced** by this plan; it is not proposed as a side
effect of the backlog sweep unless the owner separately opts in.

1. **Substring-matched error classification — opt-in, this revision.**
   Default: a review-checklist grep
   (`strings\.\(Contains\|HasPrefix\|HasSuffix\)\(.*\.Error\(\)` over
   `*.go`, `"E_` in message strings over `*.py`), same form as item 5 below.
   **If the owner opts in to automating
   it**, build a plain Go test: an AST/grep check (e.g. a `go/ast` walk, or a
   regex test over `git ls-files '*.go'`) failing on
   `strings.(Contains|HasPrefix|HasSuffix)($ERR.Error(), ...)` outside test
   files, requiring `errors.Is`/`errors.As`/a typed code instead, plus a
   matching Python test greping for `"E_..." in message`. If built, it must
   remain genuinely enforcing — runs as part of `go test ./...` (already in
   `make ci`), never a `.golangci.yml`-only rule `make lint` can skip
   silently (the vacuous-gate correction above still applies).
2. **Unbounded blocking I/O — opt-in, this revision, same correction as item
   1.** Default: a review-checklist grep for `\.Read\(|\.Write\(` calls with
   no nearby `SetDeadline`/`context.WithTimeout`, and
   `stdout.readline()`/`\.wait()`/`os.waitpid` calls with no `timeout=`. **If
   the owner opts in to automating it**, build the same AST-walking Go test
   plus Python grep test a prior draft proposed: a Go test walking the AST
   for `$CONN.Read/Write(...)` with no deadline set and not inside
   `context.WithTimeout`, and a Python test greeping for
   `$P.stdout.readline()`/`$P.wait()`/`os.waitpid($PID, 0)` with no
   `timeout=`. Optionally also add a `forbidigo` rule to `.golangci.yml`
   restricting `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` to one seam
   file (created by Phase 1 Fix 4) — but since `make lint` currently skips
   silently when `golangci-lint` is absent, any such rule must be paired with
   a `make ci`-gated step that fails (never skips) when the binary is
   unavailable, so it actually enforces rather than being best-effort.
3. **Ledger drift from the kernel object it tracks** — one generic property
   test per ledger (worker-admit's, the slice's) against a fake cgroupfs:
   charged total never falls below the sum of actually-existing capped
   children, for any interleaving of grant/create/release/kill/restart/rmdir.
   Runs under `go test`, already enforcing.
4. **Digest one snapshot, evaluate another** — the generic gate/canary
   property test from Phase 1 Fix 3, applied across every kind via
   `discoverGates()`. Runs under `go test`, already enforcing.
5. **"Default is green"** — a review-checklist grep
   (`grep -n '"pass"'` near any verdict/dimension seeding site) rather than a
   tool; cheap enough that automating it would be over-engineering per this
   plan's own stated preference.
6. **Non-hermetic `go:embed`** — the equality test from Phase 0's AIRA-66 fix
   *is* the prevention mechanism; no separate tooling needed.

**Dropped, this revision:** a prior draft of this section carried a
`depguard`/`forbidigo` rule forbidding `store.Open(` outside
`internal/daemon`. §5 item 3's correction found that rule targets the wrong
constructor — the daemon actually opens via `store.OpenDB`, not `store.Open`,
and `store.Open` has legitimate non-daemon callers through
`internal/app.OpenWithDiagnostics` (the CLI fallback and the detached
supervisor both use it correctly) — so the rule would neither enforce
correctly nor catch the actual AIRA-85 defect. No replacement rule is
proposed in its place; see §5 item 3 for the resolution (AIRA-85's direct
fix, §4, plus a review-checklist note).

## 7. Explicitly not touched by this plan

- **AIRA-91, AIRA-32, AIRA-33 — restated this revision, root cause now closed
  (§1).** AIRA-91's root cause is closed, not an open investigation any more;
  its Part A fix is now **built by this plan** (unified with AIRA-70, §3.6) —
  not "not touched" — and its Part B is a decision point this plan surfaces
  but does not resolve (§5 item 5). AIRA-32 and AIRA-33 remain blocked, on
  the concrete precondition restated in §1 (Part A built + fastest-ee
  re-verified + `FASTEST_NO_AITEST=1` pin removed), not on an unresolved
  investigation.
- **AIRA-17, 26, 65, 77** — no separate action; auto-resolve when AIRA-33
  lands (§1).
- **AIRA-25** — no separate action, but corrected on plan-review: it
  auto-resolves via whichever of AIRA-29 or AIRA-33 lands first (its scope is
  the daemon confine-admission ledger split in `admit.go`, AIRA-29 territory,
  plus the xdist per-test delta, AIRA-33's) — not purely inside AIRA-33's
  deletion as a prior draft had it (§1).
- **AIRA-29** — added on plan-review: pulled out of this plan's Phase 1 chain
  entirely; ships as its own follow-on milestone with a re-based plan once the
  ship-together (Slice 1 + Slice 2) precondition and the hold reason are
  answered explicitly (§3.2).
- **AIRA-16 (second half only)** — added on plan-review; **re-pointed this
  revision**: a slice-internal watchdog trigger, deferred to the AIRA-91
  Part B decision (§5 item 5) — not the AIRA-29 follow-on milestone as a
  prior draft had it, since it's the same design space (who kills what under
  slice-internal pressure, `systemd-oomd` or the AIRA watchdog). Its first
  half is actioned in Phase 2 (§4).
- **AIRA-78, AIRA-79** — deferred with reasons stated (§4, §5), not silently
  dropped.

## 8. Risks

- **Fix 1's per-poll cgroupfs scan is a CPU-regression risk — added this
  revision, promised by §3.1 but missing from here until now.** AIRA-61
  precedent: a ~2ms O(tree) sweep on every poll produced 25–65% supervisor
  CPU before it was fixed (`af407be`). Mitigation, per §3.1: a
  per-outer-scope child-sum cache refreshed at most once per second,
  invalidated only on the daemon's own create/release of a worker scope —
  never a scan on the 200ms poll path. The PR must demonstrate the poll loop
  performs no cgroupfs walk on the cached path (a benchmark or a test
  asserting call count, not just a code read).
- **Phase 1 sequencing is the load-bearing planning decision in this document.**
  If Fable finds the 1→2→4 order wrong, or finds an overlap this plan missed,
  that changes worktree assignment for the whole phase — check it first,
  before reviewing anything else. (Corrected from a prior draft's "1→5→2→4":
  the AIRA-28/29 "Fix 5" is no longer in this chain, §3.2. **Note, this
  revision:** "Fix 5" is reused as a name for the unrelated, independent
  AIRA-70/AIRA-91-Part-A item, §3.6 — it is not the same item and not part of
  the 1→2→4 chain either.) **Carve-out, added this revision:** the "Phase 0"
  precondition above excludes AIRA-35 — gating Fix 1 (which closes P0
  AIRA-39) on AIRA-35's own §5 item 5 owner decision would transitively block
  a P0 fix on an undated policy call, so AIRA-35 lands as its own small
  commit once that decision is made, rebased onto whichever of Fix 1/Fix 2
  has landed by then (§2, row 0 of the file-touch matrix).
- **AIRA-28/29 closure is a judgement call presented as settled.** If it's
  wrong, closing AIRA-28 loses a real airtight-guarantee design that took real
  thought to write, even if its branch is kept as reference.
- **Volume.** 41 tickets (corrected from a prior draft's miscounted 43, §1) is
  comparable in scale to tonight's earlier simplification-programme execution.
  Phase 0 and Phase 2 parallelize safely (mostly disjoint files); Phase 1 does
  not (see file-touch matrix) — a workflow that tries to run all of Phase 1
  concurrently will produce merge conflicts on `worker_admit.go` (corrected
  this revision: not `admit.go`, which no Phase 1 fix touches — Phase 0's
  AIRA-52+23 is the one that touches `admit.go`, §2), not just wasted work.
- **Shared machine.** Every Phase 1 fix touches code that every concurrent
  session's `aira` usage depends on. Each needs the same two-loop rigor as
  tonight's AIRA-58/59/68/72/92 work, deployed only after genuine merge+review,
  never mid-build from a worktree binary (per the standing B10/AIRA-83
  mitigation already in place).
- **AIRA-91 PSI-profile hazard — rewritten this revision.** The root cause is
  now closed (§1), so this is no longer about masking an unknown bug or
  protecting a live investigation — the investigation worktrees' job is done.
  AIRA-91's confirmed trigger is worker-scope `memory.high` sustained reclaim
  throttling (`worker_admit.go:285`), the exact PSI pressure `systemd-oomd`
  kills on. Phase 0's AIRA-35 changes exactly that site (removing the
  `memory.high` arming, contingent on the §5 item 5 Part B sign-off) and Fix 1
  changes the worker-admit ledger and worker-scope creation path around it —
  both alter the confirmed trigger's PSI profile. **Neither deploy is gated on
  live-investigation coordination any more; instead, each must be verified
  against the synthetic repro the ticket already built**
  (`~/tmp/aira91-probe/`: `selfkill`/`groupkill`/`groupkill_dr` probes
  reproducing the real incident artifact on 12/13 trailer fields) — **not yet
  committed to the repository as of this revision. Corrected, §3.6: the probe
  (or an equivalent Go test) is committed as its own small commit *before*
  Fix 1, not inside Fix 5's PR**, so this verification is actually
  reproducible by the two-loop reviewers from the start of Phase 1, not
  dependent on an uncommitted `~/tmp` directory on one machine, and Fix 1's
  own deploy is never silently gated on Fix 5 merging first — confirm the new
  `terminated-by=` field (§3.6) correctly reports `external-cgroup-kill`
  against the `groupkill` probe, and confirm AIRA-35's `memory.high` removal
  doesn't change the PSI profile in a way the probe can't already
  characterize, before either deploy ships to the shared daemon.
- **AIRA-74 removes the only serialisation of full FTS rebuilds — added this
  revision.** The machine-wide flock AIRA-74 deletes (§2) is currently the
  sole bound on concurrent full-project FTS index builds on this shared
  machine; its per-query-`TEMP`-table replacement lets N concurrent `aira
  grep` calls across sessions each build their own index in parallel, with no
  measured bound. This plan requires the per-query cost (wall time + peak
  RSS) be measured on the largest project here and recorded in the PR before
  merge; if material at realistic concurrency, the fallback is a per-project
  lock, not incremental indexing (§2).
- **Daemon-restart cadence on a shared machine — corrected this revision:
  "batch to one restart per phase" contradicted the Phase 1 serial chain and
  is replaced with one explicit per-fix policy.** Phase 0's AIRA-52+23 and
  AIRA-75, plus every Phase 1 fix, touch code the running daemon serves —
  each restart re-runs #74's reserve-ledger reconstruction and interrupts
  every session's admission, so cadence matters. A flat "one restart per
  phase" is wrong for a serial chain whose first fix closes a live P0
  (AIRA-39) and whose second requires an atomic reinstall+restart (below) —
  stating one blanket policy for both would either delay a P0 fix
  unnecessarily or under-specify the protocol-bump deploy. **Policy, stated
  explicitly:** Fix 1 (§3.1) deploys on its own restart as soon as it merges
  — it closes P0 AIRA-39, and its correctness is verified against the
  committed probe before that restart, not after. **Ordering fix, added this
  revision:** the probe commit is split out of Fix 5 and lands as its own
  small commit (tests only, `internal/runner`) *before* Fix 1's PR, rather
  than waiting for Fix 5 (which starts at the same time as Fix 1 and could
  plausibly merge after it, silently gating Fix 1's deploy on an unrelated
  fix) — Fix 5's own PR then extends that already-committed probe with the
  `terminated-by=` assertion (§3.6). Fixes 2 and 4
  (§3.3, §3.4) deploy together as one atomic reinstall+restart (Fix 2 bumps
  the protocol version; see the dedicated bullet below). Fix 3 (store code,
  runs inside the DB-owning daemon) and Fix 5's daemon-side log line (§3.6)
  also need a restart and ride the Fix 2+4 restart rather than getting a
  fourth one of their own. Phase 0's AIRA-52+23 and AIRA-75 restart once,
  batched together, before Phase 1 starts. Client-only changes (e.g. an
  AIRA-27-shaped fix with no daemon-side change) need no restart at all.
  **"Quiet," defined concretely rather than left as a vague target:** `aira
  confine --list` shows **0 populated scopes and 0 queued waiters** — not
  "low activity". At the time this correction was written, the live machine
  showed 6 populated scopes (including two `@dr-` delegate-RAM suites
  roughly an hour into their run) and 10 admitted jobs — a concrete
  illustration that "quiet" needs checking, not assuming. **Name the notify
  channel and step explicitly:** post to the same peer-coordination channel
  this project's sessions already use for cross-session messages, once
  immediately before each restart (giving concurrent sessions a chance to
  finish an in-flight admission-sensitive operation) and once immediately
  after (confirming the daemon is back and reachable). Roll back, on any
  restart, by swapping back the backed-up prior binary and restarting again
  — **corrected, this revision: not "the same procedure AIRA-28's own
  deploy record already documents."** AIRA-28's own ticket records this
  procedure was *written* but its build was "BUILT + VERIFIED but NOT
  deployed" — the procedure itself was never actually executed. **Cite the
  AIRA-27 deploy record instead** (`2e8f237`, DONE + DEPLOYED 2026-09-01:
  class-based `oom_score_adj`, client/runner-side only, no daemon restart
  needed for that particular change) — the swap-binary-and-restart rollback
  shape is the one actually used across this project's daemon-touching
  deploys (e.g. AIRA-68).
- **Fix 2's protocol bump needs an atomic reinstall+restart — added this
  revision.** Because Fix 2 bumps `daemon.ProtocolVersion`, every session's
  already-installed `~/.local/bin/aira` binary mismatches the new daemon the
  instant it restarts, until that session reinstalls. The deploy step must
  **reinstall the PATH binary and restart the daemon as one atomic step**
  (not restart-then-reinstall-later), and must land **after AIRA-83(a) (§2)
  has already merged and deployed** — with (a) in place, a stale client that
  hits a version-mismatched daemon refuses cleanly ("re-run aira install")
  instead of falling into the now-deleted `systemctl --user restart
  aira-daemon.service` self-heal branch and restarting the shared daemon out
  from under other sessions mid-deploy.
