# Backlog remediation plan

**Date:** 2026-09-04
**Status:** DRAFT — pending Fable plan-review
**Input:** [`docs/superpowers/reviews/2026-09-04-fable-backlog-simplification-sweep.md`](../reviews/2026-09-04-fable-backlog-simplification-sweep.md)
(four parallel Fable-model cluster reviews of all 49 open tickets, each verified
against current source, not trusted from ticket text)

## 0. Owner instruction this plan operationalizes

> Write a plan for dealing with all of this... Where we can fix things by
> deleting/simplifying/unifying stuff, it's preferred over adding more
> complexity.

Every phase below is ordered by that preference: deletions and unifications come
first and are the largest phase by ticket count; genuinely irreducible individual
engineering comes last. Two P1s (AIRA-28/AIRA-29) are resolved by a *decision*
this plan makes explicitly, not by more code — flagged so Fable and the owner can
push back on that call specifically.

## 1. Scope

48 of the 49 open tickets as of `18d0e93`. **AIRA-91 is explicitly out of scope**
— it has its own live, dedicated investigation running in a separate worktree
right now (`../aira-91-root-cause`, agent `aa9faa2d3b1328a2b`, coordinating with
peer session `qual` on a live repro), its root cause is still unknown, and mixing
it into a batch-execution plan would either stall this plan on an open
investigation or tempt a rushed fix. It gets its own plan once root-caused.

Three more tickets are consequently **blocked, no action in this plan**:
AIRA-32 and AIRA-33 wait on AIRA-91 directly (§8 of the sweep found AIRA-33's
precondition has *hardened* since this afternoon, not softened — fastest-ee had
to pin three legs back onto the very stack AIRA-33 wants to delete, specifically
because of AIRA-91). Five more (AIRA-17, 25, 26, 65, 77) have **no separate
action** — they are scoped work the sweep found already lives entirely inside
AIRA-33's deletion (the xdist stack); they close automatically when AIRA-33 lands
and would be wasted, throwaway effort if actioned now.

That leaves **43 tickets with real action in this plan**, sequenced into three
phases by risk tier, in the order this plan's preference puts them.

## 2. Phase 0 — Deletion, simplification, and stale-ticket closure (Tier C)

Mechanical, low-risk, no design judgement beyond what the sweep already
established. One worktree, sequential commits (small diffs, fast to review),
`make ci` after each. No daemon-protocol or admission-path changes live here —
everything in this phase is either dead code, a reporting/config fix, or a
narrow, previously-scoped change with a single clear caller.

| Ticket | Action |
|---|---|
| AIRA-89 | Delete 4 confirmed-dead symbols (`hasGateContent`§, `FlakyCellStateSummary`, `GateAudit.Verify`, `GateAuditRecords`) — re-verify zero callers immediately before deleting, don't trust the sweep's snapshot across the time gap. |
| AIRA-66 | Replace both `go:embed all:` directives (aitest, xdist-governor) with explicit declared file lists; convert the existing presence-only embed test to an equality check against the declared manifest. |
| AIRA-88 | Site 3 (pylib extraction-dir growth) closes as a *consequence* of AIRA-66 — re-measure after, no separate code. Sites 1–2 (registry.jsonl, lock-file inodes): record "stays append-only / stays unbounded, both bounded in practice" as the resolution; no code. |
| AIRA-35 | Stop arming `memory.high` on aitest worker scopes (drop the wire field, the grant line, the client struct field, the `worker_scope_linux.go` guard). Change `worker.py`'s watermark read from `memory.high` to `memory.max` with the percentage folded in. |
| AIRA-44 | Add `AIRA_AITEST_OUTER_SCOPE` at launch (the launcher already has `scope.Reference()` in hand); bootstrap uses it instead of self-discovering via `CurrentCgroupPath()`. Make the membership guard idempotent for "already in `<outer>/.aira-supervisor`". |
| AIRA-52 | Encode confine job owner into the scope directory name (delimiter not in the existing owner charset); delete `freshConfineOwner` and the in-memory registry merge it exists for. |
| AIRA-56 | Delete `hasGateContent` (dead by AIRA-89, don't double-count — land together). `ready` surfaces the existing `U_GATE_SET_EMPTY` primitive as an advisory finding rather than gaining new ledger-memory state. |
| AIRA-74 | Replace the machine-wide flock + full-rebuild write-transaction with a per-query `TEMP` FTS table on its own connection. |
| AIRA-75 | Stop minting project ticket-sequence numbers for daemon telemetry events (`AppendWatchdogEvent`, `rant.redacted`); they become a daemon log line instead. The "voids gap-detection" premise in the original ticket is false — no gap-detection mechanism exists to void — say so in the resolution. |
| AIRA-93 | Scrub `GIT_*` in `gitValue`/`runGitRevParse` (the binary's own git-invoking helper — the hooks were already scrubbed by AIRA-46, this is the one path AIRA-46 didn't reach); add a `TestMain` guard against inherited `GIT_DIR`; one-time-delete the two stale foreign receipts identified by seq/timestamp. Update the refusal message to print the offending receipt path. |
| AIRA-60 | Collapse `safeFixturePath`/`safeSnapshotPath`/`safeMutationPath` into one exported `gate.SafeRelativePath`; use it in the canary seed-loop path validation that was missing it. Net negative lines. |
| AIRA-69 | Add a 3-line assert in the cgrouptest helper refusing to place a scope directly under `aira.slice`; no further action (impact was confirmed cosmetic, not the leak risk originally suspected). |
| AIRA-82 | Re-scope only in this phase (update the ticket's own framing — it is not fabricated metadata, the daemon correctly records the cwd it was given). The actual small fix (an explicit per-call scope override on MCP/CLI) moves to Phase 2 since it touches face-layer code, not a pure deletion. |
| AIRA-37 | Re-scope: 4 of 6 original sub-items are already fixed (by AIRA-30 and AIRA-92) — close those as stale with a pointer to the commits that fixed them. The 2 genuine residues (an atfork docstring, a worker over-spawn counter) move to Phase 2 as small individual fixes. |
| AIRA-83(a) | Delete the `replaceOlderDaemon` restart branch entirely; return the existing error instead. (AIRA-83's other half, (b), is a Phase 1 structural fix — see below; land (a) here since it's a pure deletion independent of (b).) |
| AIRA-64 | Close with a one-line doc note ("heavy corpora want a quiet slice or a longer `PYTEST_TIMEOUT`"); no code. Its scheduler-relevant half is recorded against AIRA-33/governor-deletion, not built. |
| AIRA-34 | Correct the ticket's stale line references; keep as documented, known behaviour (real production consumer exists on exactly one path; not worth ~10 lines of narrowing until that path starts gating on it). No code change. |
| AIRA-79 | Explicitly deferred, no code (no submodule-bearing AIRA project exists to justify the ~20 lines yet) — recorded here, not silently dropped from the plan. |

**Verification for this phase:** `make ci` after each commit; a self-review pass
per CLAUDE.md's lighter path is sufficient for the pure deletions (AIRA-89, 66,
88, 60, 69) since they remove code with zero live callers, verified fresh. The
five that touch runtime behaviour even slightly (AIRA-35, 44, 52, 56, 74, 75,
93, 83(a)) get one Sol/Codex build-review pass each before merge — Tier C in
this project's own grading, not Tier A, but not zero-review either given they
touch the daemon/runner/aitest hot paths.

## 3. Phase 1 — Structural fixes (Tier A, full two-loop)

Five fixes close or moot roughly 13 tickets between them. **Sequencing matters
more than parallelism here** — see the file-touch matrix below. Fix 3 (gates)
is fully independent of the other four and can build concurrently; fixes 1, 2,
4, and 5 all touch `internal/daemon/admit.go` and/or
`internal/runner/*admission*`/`*confine*` files and **must land sequentially**,
each rebasing onto the previous, to avoid four PRs racing to modify the same
functions. This is the single highest-value planning decision in this document
— get Fable to check it specifically.

### File-touch matrix

| Fix | Primary files | Overlaps with |
|---|---|---|
| 1. Worker-admit ledger = cgroup tree | `internal/daemon/worker_admit.go`, `internal/runner/worker_scope_linux.go` | 5 (admit.go territory, adjacent not identical) |
| 2. Structured daemon↔client outcome codes | `internal/runner/worker_admit_client_linux.go`, `cmd/aira/main.go`, `internal/pylib/aitest/supervisor.py`, `internal/daemon/worker_admit.go` | 1 (same file), 4 (protocol.go/main.go territory) |
| 3. Captured-subject gate evaluator | `internal/store/gate_eval.go`, `gate_command.go`, `gate_ratchet.go`, `internal/gate/canary.go`, `traceability.go` | none — independent subsystem |
| 4. Symmetric deadline-policy seam | `internal/daemon/server.go`, `internal/daemon/protocol.go` | 2 (main.go/protocol.go territory) |
| 5. AIRA-28/29 decision + build | `internal/daemon/admit.go` | 1 |

**Order: 1 → 5 → 2 → 4, with 3 running concurrently in its own worktree from
the start.** Reasoning: 1 and 5 both touch `admit.go`'s charge/reserve logic
directly and are causally related (5's dynamic-reserve build is explicitly the
generalization of the restart-adoption charging rule that 1 also touches) — do
them back to back on one branch stack. 2 depends on nothing from 1/5 but shares
`worker_admit.go`, so land it after that file has settled. 4 touches
`protocol.go`, which 2 also touches on the client side (`main.go`) — land last
to avoid rebasing a deadline-seam change under a still-moving classification
change.

### 3.1 Fix 1 — Worker-admit ledger tracks the real cgroup tree

**Closes AIRA-39, AIRA-41; moots AIRA-63.**

Replace `workerJobState.grants`' pure in-memory accounting with `committed =
Σ memory.max` over existing `.aira-worker-*` children of the outer scope,
scanned from cgroupfs the same way the outer slice ledger already does (post-#74).
Close the grant→scope-creation window: the daemon creates the worker scope
itself under `job.mu` rather than trusting the CLI to create it after the grant
is recorded (the reaper already rmdirs orphaned scopes, so cgroupfs mutation
from the daemon side has precedent). Ownership tracking (`workerScopeOwner`)
becomes unnecessary — the sum is over the scope, not over who asked for it.
AIRA-63's concurrency-bound question becomes moot once "granted" and "capped
scope exists" are the same fact.

**This fix does not need to explain AIRA-91** and must not be sold as doing so
— the sweep found it predicts the wrong exit code (137/signal-derived, not 0)
for what's actually been observed. It's justified on its own: a real,
independently-confirmed restart-survivability gap in the RAM-safety ledger.

### 3.2 Fix 5 — Close AIRA-28 as superseded; unblock and build AIRA-29

**Owner-facing call, not a code-only decision — flagged explicitly.** AIRA-28
("bound the delegate aggregate, airtight") and AIRA-29 ("charge by live
`memory.current`, utilization-first") answer the same root cause from opposite
philosophies, and the daemon already runs both models side by side today (the
restart-adoption path is, in effect, AIRA-29's rule already; the steady-state
path is what AIRA-28 wants replaced). The sweep's read: the owner chose
utilization over airtightness once already (AIRA-28's own ticket text, dated
2026-09-01). This plan proposes: close AIRA-28 as superseded-by-decision
(keep its branch/spec as reference, don't discard the analysis), unhold and
build AIRA-29 as the Phase 5b "cgroup tree as ledger" design, sized smaller
than its original framing since the restart-adoption path is already most of
the prototype. **If Fable or the owner disagrees with treating this as settled,
this is the one item in Phase 1 to escalate rather than silently proceed on.**

Building AIRA-29 also gives AIRA-16's second half (a slice-internal watchdog
trigger) and AIRA-24's fourth ask (saturation-wait UX tied to real headroom) a
natural landing point — noted, not built as part of this fix; they stay
individually scoped in Phase 2/Phase 0 as already assigned.

### 3.3 Fix 2 — Structured daemon↔client outcome channel

**Closes/moots AIRA-42, AIRA-45, most of AIRA-87; closes AIRA-83(b).**

Emit `AdmitResponse.State` (and the daemon's existing `reject:`/`fallback:`
convention) as a stable code / `state=` k=v line across the wire, replacing the
prose that gets flattened and re-derived by six-times-patched substring
matching in `aitest/supervisor.py`. Python matches the enum by exact value;
anything unrecognised is an explicit error, never silently "unavailable" —
this directly fixes the AIRA-83(b) mismatch (protocol-version skew currently
misclassified as a sizing error) as one instance of the same channel. For
AIRA-87 (exit-code catalogue drift, confirmed in both directions): declare each
exit code once in a leaf package next to the code it exits with, so drift
becomes structurally impossible rather than something a test has to police —
this closes most but not all of AIRA-87 (the two AIRA-42/45-classifier
crossovers close fully; a residual few unrelated drift instances the sweep
found, e.g. `E_RANT_REDACTED`/`W_GATE_PROOF_EXPIRING`, get swept up by the same
mechanical declare-once move but aren't separately designed here).

### 3.4 Fix 4 — Symmetric deadline-policy seam

**Closes AIRA-84.** One deadline-policy seam applied to both `exchange()`
(client) and `serveConnection()` (daemon): a short connect/parse deadline, with
the actual response wait driven by the caller's context/signal or a
verb-declared budget instead of the same 30-second constant re-used for
everything. This is the same class already fixed twice tonight (AIRA-18,
AIRA-92) — worth naming as a durable convention in the commit, not just a
one-off patch, since a `forbidigo` rule (Phase 2, tooling) will hold it there
going forward.

### 3.5 Fix 3 — Captured-subject type for gate evaluators (parallel track)

**Closes AIRA-80, AIRA-81.** Direct sibling of tonight's AIRA-72 fix, applied
to the two lanes AIRA-72 didn't touch. Capture the subject tree once
(`stableSubjectEntries`, already exists) and change every evaluator's
signature from "takes a root path, re-reads it" to "takes an
already-captured subject" — removes a redundant tree-read (dimension lane) and
a redundant re-index round-trip that currently silently drops
tracked-but-gitignored files (canary/mutation lane), confirmed as a live false
"canary fired" hazard. Add the generic property test proposed in the sweep:
for every gate/canary kind, mutating any tracked file (including a
gitignore-matched one, a mode bit, a symlink target) invalidates a stored pass
— one test instead of one per kind.

**AIRA-78 is contingent, not included in this fix's scope.** It shares the same
invariant but its evidence side needs a schema change to test reports (zero
rows in production today) or the ratchet gate kind needs to be deleted with
them — that's a second owner-facing call this plan does not make for Fable.
Recorded as a decision point in §5, not actioned in Phase 1.

## 4. Phase 2 — Individual fixes (Tier B)

Narrow, real, no clustering or deletion angle — reported honestly as such by
the sweep rather than forced into a pattern. Independent of each other and of
Phase 1 (different files), so these can build in parallel once Phase 0 is
merged. Ordered here by size, smallest first.

| Ticket | Fix |
|---|---|
| AIRA-23 | Replace the literal `"unknown"` owner fallback with a stable, non-colliding identity (e.g. `cwd:<basename>`). |
| AIRA-82 | Add an explicit per-call scope override on the MCP/CLI faces (the actual code half of the re-scoped ticket; Phase 0 already corrected its framing). |
| AIRA-37 residue #1 | Fix the stale atfork docstring in `worker.py`. |
| AIRA-37 residue #2 | Add the missing decrement to `supervisor.py`'s worker-dispatch queue check (currently `if not self.queue: break` with no counter, allowing over-spawn). |
| AIRA-70 | Reorder the OOM-advisory gate to check `oom` before `scopeMemoryMax <= 0` (the value is already computed, just dropped by the presentation gate); add the two missing log lines (signal handler, `confine-kill` dispatch) and a `terminated-by=` trailer field. |
| AIRA-40 | Replace pipe-EOF worker-liveness inference with a `pidfd` in the same `select()` set. |
| AIRA-43 | Add the missing granted-path stdin-hold test harness (sequence after Phase 1 Fix 2, since the output shape it tests changes there). |
| AIRA-24 | Add queue position to the admission saturation-wait message (the other three asks in this ticket are already landed). |
| AIRA-85 | Fold the detached supervisor's `state.db` writes into the existing relay path (`AddTestReport`/`AddComputeEvent` already implement the daemon side); name the structural gap ("nothing enforces single-writer" beyond convention) explicitly in the commit even though closing it fully is Phase 3 tooling (see §6). |
| AIRA-86 | Seed all fourteen honesty dimensions `unevaluated`, never `pass`, at every seed site identified in the sweep (`check.go`, `gate_eval.go` ×2, `gate_ratchet.go`); add the per-dimension pass tests. |
| AIRA-20 | Package-wide pass replacing fixed wall-clock test deadlines with condition-based waiting or an `AIRA_TEST_DEADLINE_SCALE`-driven helper, across `internal/runner` then `internal/daemon`. Skip `governor_slot_test.go` (deleted by AIRA-33 once that lands) rather than hardening tests about to be removed. Restore the `-race` CI job once the sweep is clean. |
| AIRA-22 | `confine --detach` — genuine feature, not cleanup. Converge onto the existing `LaunchDetached` machinery rather than building a parallel detach path. Full two-loop per CLAUDE.md's ID/crash-recovery/lease-CAS bar, since it's new durable-status surface. |

## 5. Decision points this plan surfaces rather than resolves silently

1. **AIRA-28 vs AIRA-29** (§3.2) — this plan proposes closing 28 as
   superseded and building 29. Flagged for explicit Fable/owner sign-off.
2. **AIRA-78's ratchet gate** — keep (schema change to give test reports a
   producer, then apply the Fix-3 pattern to it) or delete the ratchet kind
   entirely (zero production rows exist for it today, per the sweep). Not
   actioned in this plan either way; recommend a follow-up ticket that names
   this fork explicitly rather than leaving AIRA-78 open indefinitely as a P0
   nobody is working.
3. **AIRA-85's structural gap** ("nothing enforces single-writer beyond
   convention") is only partially closed by folding in the detached
   supervisor's writes (Phase 2). A `depguard`/`forbidigo` rule forbidding
   `store.Open(` outside `internal/daemon` (§6) closes the class; whether to
   also add a runtime flock assertion is left as a follow-up, not required for
   this plan's scope.

## 6. Tooling/lint follow-ups (apply once, benefit forever)

Six concrete prevention mechanisms came out of the sweep, independently
rediscovered by more than one cluster — that repetition is itself signal these
are real classes, not coincidences. These are cheap relative to the fixes above
and should land as part of Phase 2 (they're small, mechanical, and this plan's
own stated preference is unification over repeated one-off vigilance):

1. **Substring-matched error classification** — semgrep rule flagging
   `strings.(Contains|HasPrefix|HasSuffix)($ERR.Error(), ...)` in Go (require
   `errors.Is`/`errors.As`/a typed code instead) and its Python mirror
   (`"E_..." in message`).
2. **Unbounded blocking I/O** — semgrep for Python
   (`$P.stdout.readline()`/`$P.wait()`/`os.waitpid($PID, 0)` with no
   `timeout=`) and Go (`$CONN.Read/Write(...)` with no deadline set and not
   inside `context.WithTimeout`); `forbidigo` restricting
   `SetDeadline`/`SetReadDeadline`/`SetWriteDeadline` to one seam file (created
   by Phase 1 Fix 4).
3. **Ledger drift from the kernel object it tracks** — one generic property
   test per ledger (worker-admit's, the slice's) against a fake cgroupfs:
   charged total never falls below the sum of actually-existing capped
   children, for any interleaving of grant/create/release/kill/restart/rmdir.
4. **Digest one snapshot, evaluate another** — the generic gate/canary
   property test from Phase 1 Fix 3, applied across every kind via
   `discoverGates()`.
5. **"Default is green"** — a review-checklist grep
   (`grep -n '"pass"'` near any verdict/dimension seeding site) rather than a
   tool; cheap enough that automating it would be over-engineering per this
   plan's own stated preference.
6. **Non-hermetic `go:embed`** — the equality test from Phase 0's AIRA-66 fix
   *is* the prevention mechanism; no separate tooling needed.

## 7. Explicitly not touched by this plan

- **AIRA-91, AIRA-32, AIRA-33** — separate investigation track (§1).
- **AIRA-17, 25, 26, 65, 77** — no separate action; auto-resolve when AIRA-33
  lands (§1).
- **AIRA-78, AIRA-79** — deferred with reasons stated (§4, §5), not silently
  dropped.

## 8. Risks

- **Phase 1 sequencing is the load-bearing planning decision in this document.**
  If Fable finds the 1→5→2→4 order wrong, or finds an overlap this plan
  missed, that changes worktree assignment for the whole phase — check it
  first, before reviewing anything else.
- **AIRA-28/29 closure is a judgement call presented as settled.** If it's
  wrong, closing AIRA-28 loses a real airtight-guarantee design that took real
  thought to write, even if its branch is kept as reference.
- **Volume.** 43 tickets is comparable in scale to tonight's earlier
  simplification-programme execution. Phase 0 and Phase 2 parallelize safely
  (mostly disjoint files); Phase 1 does not (see file-touch matrix) — a
  workflow that tries to run all of Phase 1 concurrently will produce merge
  conflicts on `admit.go`/`worker_admit.go`, not just wasted work.
- **Shared machine.** Every Phase 1 fix touches code that every concurrent
  session's `aira` usage depends on. Each needs the same two-loop rigor as
  tonight's AIRA-58/59/68/72/92 work, deployed only after genuine merge+review,
  never mid-build from a worktree binary (per the standing B10/AIRA-83
  mitigation already in place).
