# #8-part2 — coherent, torn-read-safe working-tree scans (never a fake fail / false-pass / fake-zero)

Status: PLAN v7 — BUILD-READY (Sol plan-review r1–r6; design confirmed sound). Central scanner fix with
a signature-level `inconclusive` outcome → compiler-enforced per-caller completeness across ~20 read +
mutation sites (incl. lease/area gates via `ticketExists`). This is a LARGE, systemic, correctness-
critical milestone — the build is a dedicated effort, not a marathon-tail rush. Branch
`codex-aira-8part2` off master `4374992`.
Task #8 part 2 (part 1 — ParseTicket/ParseRequirement trailing content — landed `8183345`).
Class: **crash-recovery + readiness correctness → the two-loop is mandatory** (CLAUDE.md).

## 0. The bug (systemic)

AIRA rebuilds its index and answers readiness/checks by scanning the **live working tree**. The shared
readers do `Lstat`+single `os.ReadFile` (`readRegularTicket` query.go:352, `readRegularFinding`
finding.go:72, `readRegularRequirement` requirement.go:96), and `scanTickets` (store.go:2543) takes a
single `os.ReadDir`. AIRA's own writes are atomic (`writeAtomic` store.go:2076-2125), so only an
**external non-atomic writer** — `git checkout`/`merge`/`rebase`, a non-atomic editor — racing a scan
tears a read or changes the directory mid-scan. Three honesty violations result:
- **Fake FAIL:** a torn read that fails to parse → a spurious, and (since scan reconciliation findings
  are upsert-only with no prune, finding.go:528 / Rebuild deletes only `subtype='review'`
  store.go:1552) **permanent** `E_CONFIG_INVALID`/`E_FINDING_INVALID`/`E_REQUIREMENT_INVALID`.
- **Readiness FALSE-PASS:** `Ready` performs MULTIPLE independent live scans (relation_ready.go:385,
  416, and `relationIndexDivergence`→`scanStoredRelationsAt` at :318, plus `relationIndexFindingKind`
  reread at :357). Two individually-STABLE scans can read DIFFERENT atomic versions (ticket-scan sees A
  with `blocks B`; a later scan reads A rewritten without it), OR a torn read, OR a directory-add race
  (a newly added ticket declaring `blocks B` arrives after the single `os.ReadDir`) → a blocked ticket
  appears **ready** with no exclusion.
- **Query FAKE-ZERO:** the full index rebuild unconditionally deletes+rebuilds projections
  (store.go:1539-1549); List/Search/ListFindings scan live (query.go:241, search.go:98, finding.go:259)
  — a torn/vanished entity silently becomes an **absent** result.

**Completeness is COMPILER-ENFORCED (the key structural property).** `stableReadFile` and the scanners
gain a new `inconclusive` outcome in their RETURN SIGNATURE (e.g. `readRegular*` → `(data, outcome,
err)`; scanners → an added `inconclusive` set/flag). Changing the signature makes EVERY call site a
compile error until it explicitly handles the new outcome — so no caller can be silently missed (this
is why the endless round-by-round caller discovery converges: the type system enumerates the set). The
build handles each per a small **contract**: a **read** caller maps `inconclusive` → its
`U_INDEX_UNESTABLISHED`/`unevaluated` dimension; a **mutation** caller returns `U_INDEX_UNESTABLISHED`
**before any write**.

**Known caller surface (from a full grep; the compiler will confirm exhaustiveness):** Rebuild
(store.go:1459/1474/1484/2565), Ready (relation_ready.go:40/146/182/225/231/318/357/385/416/677), Check
ALL dimensions (check.go:116/123/239/270/349/378/420), List/records + `exactRecord`/`show`
(query.go:241/260/265/309-314), `derivedRelationViewsWithWarnings` (relation_ready.go:243→90 via
`exactRecord` and `Relations`), Search (search.go:98/102), ListFindings (finding.go:259), the mutation
callers `Link`/`Unlink`/`Relations`/ticket-update/finding+requirement CRUD (relation_ready.go:121/160,
store.go:1029, finding.go:104/156/181, requirement.go:153), AND the indirect mutation gates via
`ticketExists` (query.go:309-314): `Touch` (area.go:378) and lease `Claim`/`Release`/`Heartbeat`
(lease.go:184/383/478) — all abort before their writes on inconclusive.

AIRA's rule: a check that cannot establish its result reports **unevaluated** — never a fake pass,
zero, **or fail**.

## 0.1 Product decision — WORKING-TREE authority (owner-delegated; recorded)

Recovery reads the **working tree**, not the committed/staged tree (Sol-verified: Rebuild enumerates+
reads live roots; no HEAD/index authority exists). AIRA's architecture is *an index rebuildable from
git-file content in the working tree*, and agents' **uncommitted** entities are live coordination
state; a committed-snapshot fix would drop them. So the fix keeps working-tree semantics and makes a
raced scan **honest** (candidate B of `~/tmp/aira-design/8part2-rebuild-race-analysis.md`).

**Accepted policy boundary — the atomic-external-writer contract (Sol build-review P1-2).** The
double-read tear-check detects a file whose bytes CHANGE between the two observations. It cannot detect
a writer that leaves a file at a **stable partial state** (truncate-then-stall for longer than the
bounded retry window): both reads see the same partial bytes and the read is accepted as stable. This
residual is **inherent to working-tree authority** — the only ways to eliminate it are (a) require the
committed/staged tree as the recovery source (rejected in §0.1), or (b) require every external writer to
replace files atomically, which AIRA cannot enforce on `git`/editors/other tools. AIRA therefore adopts
the same assumption its OWN writes satisfy (`writeAtomic` — temp+rename) and that the existing
`captureTraceSnapshot`/`discoverGates` tear-checks already rely on: **external writers of `.aira/**`
files are expected to write atomically; a non-atomic external writer that leaves a stable-partial file
is outside AIRA's honesty contract.** This is an explicitly accepted, reviewer-acknowledged residual
(peer of the runner descendant-escape residual, task #20), documented on `stableReadFile` and pinned by
a documenting test. A stable-partial that is malformed self-heals when the writer completes (the scan
self-heals scan findings); a stable-partial that parses is indistinguishable from the file's true
current state.

## 0.2 Approach — a central torn-read-safe scan contract, per-caller inconclusive handling

Rather than a bespoke fix per site, make the SHARED readers/scanners coherent, and give every caller a
single new outcome to handle:
1. **`stableReadFile`** replaces the single `os.ReadFile` in the three `readRegular*` helpers:
   double-read + byte-compare (content, not mtime/size — a same-size rewrite keeps both; mirrors
   `captureTraceSnapshot` traceability.go:81 / `discoverGates` gate_index.go:103), **bounded retry**
   (transient external writes complete in µs, so the retry almost always reaches a stable read and the
   scan proceeds with the file's TRUE post-write content), **`O_NOFOLLOW`** (a symlink swapped after
   `Lstat` must not be followed), and ENOENT at `Lstat` OR either read (vanished mid-scan) →
   **inconclusive**, never a hard error. A genuine IO error (EACCES) stays an error (D3).
2. **Directory second-check** in `scanTickets`/`scanFindingFiles`/`scanRequirements`: re-`ReadDir`
   after the per-file reads and compare the filename set (as gate_index.go:88-100 does); a change →
   the scan is **inconclusive** (closes the directory-add false-pass, Sol r4 P0 — D4 is IN, not
   deferred). Per-file torn/vanished reads also mark the scan inconclusive.
3. The scanners return an explicit **`inconclusive`** signal (distinct from valid + from a genuine
   `E_*_INVALID` finding). NO scanner fabricates a finding or hard-errors for a torn/vanished read.

## 1. Scope

**IN:**
1. `stableReadFile` (double-read + retry + `O_NOFOLLOW` + ENOENT-inconclusive) in the three
   `readRegular*` readers.
2. Directory second-check + per-file-inconclusive propagation in `scanTickets`/`scanFindingFiles`/
   `scanRequirements`; a distinct `inconclusive` return; NEVER a fabricated finding or hard error for a
   torn/vanished read.
3. **Genuinely-malformed *stable* file STILL reports** `E_*_INVALID` (byte-identical reads parse
   malformed both times — false-negative guard; the fix must not mask real corruption).
4. **Per-caller inconclusive handling:**
   - **Rebuild:** two-phase. Phase A (scan) is READ-ONLY over projections+reconciliation findings —
     collect valid/malformed/invalid-root findings AND the inconclusive set **in memory** (defer BOTH
     `recordScanFinding` store.go:1463-1482 AND `recordRebuildFinding` E_GIT_SCAN store.go:1515). If
     inconclusive → return transient **`U_INDEX_UNESTABLISHED`** before Phase B; prior index intact.
     Phase B (only on a fully-stable scan) runs the projection full-replace, the deferred finding
     recording, AND the scan-finding self-heal — **all in ONE `withImmediate` transaction**
     (store.go:1535-1739 style; transaction-local insert helpers, Sol r4 P1) so a crash can't pair a
     new projection with stale findings.
   - **Ready:** compute readiness from ONE coherent ticket snapshot — remove the second
     `scanStoredRelationsAt` (relation_ready.go:416), and refactor `relationIndexDivergence`
     (:317-320) and `relationIndexFindingKind` (:356-364) to consume that snapshot (no hidden rescan,
     no raw owner-file reread). Any inconclusive read anywhere in the path →
     `U_RELATION_GRAPH_UNESTABLISHED` (the existing honest surface, relation_ready.go:429/542-556); an
     index-divergence-only inconclusive degrades to the existing non-blocking warning (:430-434).
   - **Check — ALL live-scan dimensions (Sol r5 P0):** not only `checkStaleIndex` (check.go:349) and
     `checkAllocatedRequirementFile` (check.go:378), but also `relationIndexDivergence` (check.go:116),
     `findingIndexDivergence` (check.go:123), `relationFindings` (~check.go:270), and `checkDuplicateIDs`
     (check.go:420) — which reach `scanStoredRelationsAt`/`scanFindingFiles`/`scanTickets`. EACH threads
     the `inconclusive` result → marks its dimension `unevaluated` (never a partial-graph false-pass,
     never a fabricated divergence finding, never a raced-file omission/hard-error). Check converts a
     `U_INDEX_UNESTABLISHED` from Rebuild into an unevaluated report (branch before `CheckReport{}`,
     check.go:188-193).
   - **List/Search/ListFindings:** map the scanners' inconclusive signal → a retryable
     `U_INDEX_UNESTABLISHED` result (never an empty/fake-zero list). Central fix → small per-caller
     branch; the earlier D5 residual is CLOSED, not deferred.
   - **Mutation callers (Sol r5 P1) — abort before `materialiseIntent`:** `Link` (relation_ready.go:121
     →`scanStoredRelations`:90, mutates at :160-165), `Unlink`, `Relations`, ticket updates
     (store.go:1029), and the finding/requirement CRUD read-backs (finding.go:104/156/181,
     requirement.go:153, relation_ready.go:146/182/225) must, on an inconclusive scan/read, RETURN
     `U_INDEX_UNESTABLISHED` (retryable) BEFORE any write — a mutation must never authorise a
     duplicate/wrong-side decision from partial/torn data. Documented as an operation-level retry
     contract; the caller retries once the external write settles.
5. **Self-heal (folds in the discovered no-prune gap):** on a fully-stable scan, full-replace the
   worktree's `scan:` reconciliation findings — `DELETE ... WHERE project_id=? AND worktree_id=? AND
   subtype='reconciliation' AND substr(finding_key,1,?)=?` with the exact `scan:<worktree>:` prefix
   (Sol: never the compute/flaky/conservation rows at compute.go:328 / testreport.go:507) — then
   reinsert the current set, in the Phase-B transaction. Repairs pre-existing spurious findings AND
   clears fixed-file findings.
6. **Contract shapes:** `reconcile`/rebuild handler returns `handlerData{Verdict:"unevaluated"}`
   (structured success, core.go:353-363), NOT `handlerData.Code` (which yields `OK:false`,
   core.go:338-339). All NON-`U_INDEX_UNESTABLISHED` Rebuild failures stay hard errors.
7. New stable code `U_INDEX_UNESTABLISHED` registered in the code catalog.
8. Deterministic tests that FAIL against today's single-read/single-listing code (§4).

**OUT (written-down deferrals / follow-ups):**
- **D3 — genuine non-torn IO errors (EACCES).** Today finding/requirement scanners turn ANY read error
  into `E_*_INVALID` (finding.go:458, requirement.go:54); this fix distinguishes only inconclusive
  (torn/vanished). Re-classing EACCES as `unevaluated` is a separate honesty pass; filed.
- **D2 — per-entity carry-forward.** Superseded by abort-on-inconclusive (the whole prior index is
  preserved); a finer-grained "freeze only the raced entity" optimisation is future work.

## 2. §1b — resolutions (r1–r4 incorporated)

- **R1** content-compare + bounded retry; transient tears resolve to ground truth invisibly, persistent
  tear → inconclusive.
- **R2** an inconclusive scan mutates nothing and answers nothing from a partial scan — it returns a
  transient `U_INDEX_UNESTABLISHED`/`U_RELATION_GRAPH_UNESTABLISHED` (eliminates false-pass + fake-zero).
- **R3** ENOENT at `Lstat`/read → inconclusive (never a hard error); `O_NOFOLLOW` closes the symlink
  swap; EACCES stays a real error (D3).
- **R4** a stable malformed file still reports (retry never turns a stable malformed file inconclusive).
- **R5** self-heal full-replace runs only post-stable-scan, in the Phase-B transaction — repairs+clears
  without ever erasing a genuine finding.
- **R6** byte-compare catches torn-but-parseable reads (no relation poisoning).
- **R7 (accepted residual)** a writer paused between the two reads with byte-identical partial content
  is indistinguishable from stable (same residual as the existing gate/trace tear-checks); sub-ms.
- **R8** Rebuild Phase A is read-only over projections+findings; BOTH `recordScanFinding` and
  `recordRebuildFinding` deferred; pre-scan `replayUnjournaledEvents`/`markWorktreeActive` are
  idempotent (only `markWorktreeActive.updated_at` may advance on an abort — metadata, accepted+tested).
- **R9** the WHOLE `Ready` path uses ONE coherent snapshot (no `scanStoredRelationsAt`/divergence/
  finding-kind rescan).
- **R10** directory second-check is IN (Sol r4 P0 — the add race is a Ready false-pass, not benign).
- **R11** Phase B is ONE atomic transaction (projection + deferred findings + self-heal).
- **R12** exact `scan:<worktree>:` self-heal predicate; other reconciliation codes untouched.
- **R13** Check's own live readers (`checkStaleIndex`, `checkAllocatedRequirementFile`) are in the
  stable-read contract (inconclusive → unevaluated, never fake-fail/hard-error).
- **R14** List/Search/ListFindings map inconclusive → `U_INDEX_UNESTABLISHED` (fake-zero closed).
- **R15 (ALL Check dimensions — Sol r5 P0).** `relationIndexDivergence`/`findingIndexDivergence`/
  `relationFindings`/`checkDuplicateIDs` (check.go:116/123/270/420) each thread inconclusive → their
  dimension `unevaluated`; never a partial-graph false-pass, fabricated divergence, or raced-file
  omission/hard-error.
- **R16 (mutation callers abort — Sol r5 P1).** `Link`/`Unlink`/`Relations`/ticket-update/finding+
  requirement CRUD read-backs return `U_INDEX_UNESTABLISHED` BEFORE `materialiseIntent` on an
  inconclusive read — a write never proceeds from partial/torn data.
- **R17 (compiler-enforced completeness — Sol r6).** The `inconclusive` outcome is added to the shared
  readers'/scanners' RETURN SIGNATURES, so every call site is a compile error until handled — this
  closes the caller set by construction. Additionally-named sites from r6: `check.go:239`
  (allocated-id-file dimension) → unevaluated; `exactRecord`/`show` + `Relations` →
  `derivedRelationViewsWithWarnings` (relation_ready.go:243→90) → `U_`/unevaluated, never a partial
  relation view; the indirect mutation gates via `ticketExists` (query.go:309-314) — `Touch`
  (area.go:378) and lease `Claim`/`Release`/`Heartbeat` (lease.go:184/383/478) — abort before their
  writes. `Relations` covers BOTH its reads (relation_ready.go:225 and :231-243).

## 3. Tests (must FAIL against today's code)

The seam must make the two reads observe DIFFERENT bytes: an injected reader returning payload A on
read-invocation 1 and B on invocation 2 (a per-invocation sequencer, synchronized), then stabilising.
A one-time "swap at read start" is NOT valid (both new reads would see identical post-swap bytes).
1. **Torn ticket → no fake finding + no mutation:** hook alternates bytes → today records
   `E_CONFIG_INVALID`; fixed → Rebuild returns `U_INDEX_UNESTABLISHED`, no finding, prior index intact.
2. **Readiness false-pass prevented (torn):** B `blocked-by` A; torn read of A in the Ready scan →
   fixed: `U_RELATION_GRAPH_UNESTABLISHED`, B not ready.
3. **Readiness false-pass prevented (directory-add race, Sol r4 P0):** a ticket declaring `blocks B` is
   added after the first `os.ReadDir` → the second-check detects the set change → inconclusive → B not
   ready (fails today).
4. **Query fake-zero prevented:** List/Search over a torn entity → `U_INDEX_UNESTABLISHED`, never an
   empty result.
5. **Stable malformed file STILL reports** (`E_*_INVALID`) — no hook (false-negative guard).
6. **Vanished mid-scan → no hard error:** hook deletes the file between reads → inconclusive → `U_`.
7. **Self-heal:** a genuine `E_CONFIG_INVALID`; fix the file; a clean Rebuild CLEARS it (fails today).
8. **Ready uses ONE snapshot:** a scan-count hook asserts the Ready path does exactly one ticket scan
   and no raw owner-file reread (Sol r4 — proves `relationIndexDivergence` rescan is gone).
9. **Abort-intact for pre-scan mutations:** an earlier invalid git root + a later inconclusive worktree
   → `U_INDEX_UNESTABLISHED`, NO `E_GIT_SCAN` recorded; explicit replay/active idempotency assertions;
   a second clean Rebuild records `E_GIT_SCAN`.
10. **Check live reader torn (Sol r4 P1):** `checkStaleIndex`/`checkAllocatedRequirementFile` torn read
    → unevaluated dimension, never a fake `E_CONFIG_INVALID`/`E_REQUIREMENT_INVALID` or hard error.
11. **Retry resolves a transient tear invisibly:** one mid-read swap that stabilises → normal Rebuild.
12. **Phase-B atomicity:** an injected failure between projection-replace and finding-record leaves the
    prior consistent state (no new-projection/stale-findings pairing).
13. **Check dimensions torn (Sol r5 P0):** a torn read during `relationIndexDivergence`/
    `findingIndexDivergence`/`relationFindings`/`checkDuplicateIDs` → that dimension is `unevaluated`,
    never a false-pass/fabricated-divergence/omission (a discriminator per dimension).
14. **Mutation pre-write abort (Sol r5 P1):** a torn relation scan during `Link` → `U_INDEX_
    UNESTABLISHED` and NO `materialiseIntent` write (fails today — Link would authorise from partial
    data); same for a ticket-update read-back.

## 4. Files

- `internal/store/store.go` (or `scan_read.go`): `stableReadFile` + `scanReadHook`; `scanTickets`
  directory second-check + inconclusive; Rebuild two-phase + atomic Phase-B + self-heal.
- `internal/store/query.go`, `finding.go`, `requirement.go`: `readRegular*` use `stableReadFile`;
  scanners' inconclusive; List/ListFindings map it.
- `internal/store/relation_ready.go`: one coherent Ready snapshot (remove second scan; refactor
  divergence + finding-kind).
- `internal/store/search.go`: map inconclusive.
- `internal/store/check.go`: `checkStaleIndex`/`checkAllocatedRequirementFile` stable-read + inconclusive;
  Check converts `U_INDEX_UNESTABLISHED` to an unevaluated report.
- `internal/core/…`: reconcile handler returns structured unevaluated.
- Code catalog: register `U_INDEX_UNESTABLISHED`.
- `internal/store/*_test.go`: §3 suite. Follow-ups filed for D2/D3.

## 5. Risks / expected yield

1. **Masking real corruption** — highest; a stable malformed file must still report (R4, test 5).
2. **Readiness false-pass / query fake-zero** — the Sol-found P0s; eliminated by abort-not-partial (R2)
   + directory second-check (R10) + one Ready snapshot (R9); tests 2, 3, 4, 8.
3. **Breadth** — ~10 call sites route through shared readers; the central fix + one per-caller branch
   contains it, but every caller must be updated (missing one re-opens a hole). Test coverage per site.
4. **`U_INDEX_UNESTABLISHED` UX** — the bounded retry makes transient tears invisible (test 11); a
   persistent external racer is pathological and the transient signal is honest.
5. **Phase-B atomicity** — one transaction (R11, test 12).

## 6. Deferrals (filed)

- D2 per-entity carry-forward (superseded by abort-on-inconclusive).
- D3 EACCES/genuine-IO-error re-classification to unevaluated.
