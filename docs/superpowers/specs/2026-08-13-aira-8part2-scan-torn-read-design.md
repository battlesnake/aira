# #8-part2 — crash-recovery scan torn-read race (honest, never a fake fail / false-pass / fake-zero)

Status: PLAN v4 (incorporates Sol plan-review r1–r3). r3: defer recordRebuildFinding too;
relationIndexDivergence hidden rescan → one coherent Ready snapshot; handler shapes; live-query
readers (List/Search/ListFindings) = reviewer-accepted residual D5. Awaiting Sol re-review → gate → build.
Branch `codex-aira-8part2` off master `4374992`. Task #8 part 2 (part 1 landed `8183345`).
Class: **crash-recovery correctness → the two-loop is mandatory** (CLAUDE.md).

## 0. The bug

`(*Store).Rebuild` (store.go:1382) rebuilds the SQLite index by scanning the **live working tree**;
`Ready` (relation_ready.go:378) does its OWN live scans. The scan readers do `Lstat`+single
`os.ReadFile` (query.go:352, finding.go:72, requirement.go:96). AIRA's own writes are atomic
(`writeAtomic`, store.go:2076-2125), so only an **external non-atomic writer** — `git
checkout`/`merge`/`rebase`, a non-atomic editor — racing a scan tears a read. Three honesty
violations result today:
- **Fake FAIL:** a torn read that fails to parse → a spurious `E_CONFIG_INVALID`/`E_FINDING_INVALID`/
  `E_REQUIREMENT_INVALID` scan finding (store.go:2565-2578; finding.go:457-472; requirement.go:53-68),
  and — since scan reconciliation findings are **upsert-only with no prune** (finding.go:528;
  Rebuild deletes only `subtype='review'`, store.go:1552) — the spurious finding is **permanent**.
- **Readiness FALSE-PASS (Sol P0-1):** `Ready` scans tickets once (relation_ready.go:385) then
  `scanStoredRelationsAt` scans AGAIN and **discards its exclusions** (relation_ready.go:40). If the
  relation scan tears and misses a `blocks` edge while the ticket scan was clean, `graphUnevaluated`
  stays false (relation_ready.go:429) → a blocked ticket appears **ready**.
- **Query FAKE-ZERO (Sol P0-2):** Rebuild **unconditionally** deletes+rebuilds the relations/FTS/
  requirements projections (store.go:1539-1549). If an inconclusive entity is dropped, list/search/
  count (search.go:98-114, query.go:241-254, finding.go:258-279) return it as **absent** — a fake
  zero.

AIRA's rule: a check that cannot establish its result reports **unevaluated**, never a fake pass,
zero, **or fail**.

## 0.1 Product decision — WORKING-TREE authority (owner-delegated; recorded)

Crash-recovery recovers from the **working tree**, not the committed/staged tree (Sol-verified: Rebuild
enumerates+reads live roots, store.go:1451-1459/2543-2565; no HEAD/index authority exists). Rationale:
AIRA's architecture is *an index rebuildable from git-file content in the working tree*, and agents'
**uncommitted** tickets/findings/requirements are live coordination state; a committed/staged-snapshot
fix would drop them. So the fix keeps working-tree semantics and makes a torn scan *honest*.

## 0.2 Design pivot from v1 (why abort-on-inconclusive, not exclude-and-continue)

v1 excluded an inconclusive file and continued — Sol showed that produces a readiness false-pass
(P0-1) and query fake-zeros (P0-2), because a *partial* index/graph is itself dishonest. The correct
invariant: **a scan that cannot be established produces NOTHING (it does not mutate the index and does
not answer from a partial scan) — it reports a transient, retryable `unevaluated`, preserving the prior
last-known-good index.** This matches AIRA's ethos at the right granularity (the whole scan, like
`U_TRACE_UNSCANNED`/`U_GATE_EVIDENCE_UNAVAILABLE`), and — because the mutation only ever runs on a
fully-stable scan — it makes the scan-finding **self-heal safe to fix here** (D1 folds in).

## 1. Scope

**IN:**
1. **`stableReadFile`** — read a file twice and byte-compare (content, not mtime/size — a same-size
   rewrite keeps both; mirrors `captureTraceSnapshot` traceability.go:81 / `discoverGates`
   gate_index.go:103). **Bounded retry** (re-read up to N times with a small backoff): a transient
   external write completes in µs, so the retry almost always reaches a stable read and the scan
   proceeds normally with the file's *true post-write* content (valid or malformed). ENOENT at the
   `Lstat` OR either read (vanished mid-scan) → inconclusive, never a hard error. `O_NOFOLLOW` on the
   reads (Sol P1: a symlink swapped after `Lstat` must not be followed).
2. **Persistently-inconclusive read → ABORT before mutation.** The scan phase (read-only) reads all
   files via `stableReadFile`; if any file is still inconclusive after the bounded retry, Rebuild
   returns a transient **`U_INDEX_UNESTABLISHED`** (retryable) BEFORE touching any projection — the
   prior index is left intact, and **no** scan finding is recorded. A stable read (valid or malformed)
   proceeds exactly as today.
3. **Genuinely-malformed *stable* file STILL reports** (`E_*_INVALID`) — byte-identical reads parse
   malformed both times; the fix must not mask real corruption (false-negative guard).
4. **`Ready` (Sol P0-1/P0-2):** scan tickets ONCE and derive both ticket rows and relations from that
   single snapshot (remove the second `scanStoredRelationsAt` scan) — union-of-exclusions is
   insufficient because two individually-stable scans can read different atomic versions. A torn read
   in that one scan → `U_RELATION_GRAPH_UNESTABLISHED` (never a false-pass).
5. **Scan-finding self-heal (Sol P1; folds in D1 safely):** on a fully-stable scan, **full-replace**
   the worktree's `scan:` reconciliation findings (delete the worktree's scan reconciliation rows, then
   re-insert the current scan's set) so a fixed file's finding CLEARS — and any pre-existing spurious
   torn-read finding is repaired. Safe because the replace only runs when the scan is fully stable
   (an inconclusive scan aborts at IN-2 before this point, so a torn read can never erase a genuine
   finding).
6. Deterministic tests that genuinely FAIL against today's single-read code (Sol P2 — see §4).

**Honesty-contract boundary (this increment).** Sol's review established that torn-read/version-skew
is *systemic*: every live-scan consumer is independently vulnerable. This increment makes the
**index-reconstruction (`Rebuild`) and the readiness graph (`Ready`, incl. `relationIndexDivergence`)**
torn-read-safe and coherent — the two consumers where a torn read causes a fake-FAIL, a false-PASS, or
a corrupted stored index. The **direct live-entity-query readers** (`List`/records query.go:241-254,
`Search` search.go:98-114, `ListFindings` finding.go:258-279) each do their OWN live scan and can still
omit a single inconclusive entity → a fake-zero *for that query* (D5). That is a genuinely separate,
broader change (a shared coherent-snapshot layer for every reader) and is a **written-down,
reviewer-accepted residual**, not a silent gap — filed as D5. Rationale for the split: these are
individual-entity reads (not graph integrity), the window is the same narrow external-writer race, and
the stored index they'd otherwise be bypassed by is now torn-read-safe.

**OUT (written-down deferrals / follow-ups):**
- **D5 — coherent-snapshot layer for the direct live-query readers** (`List`/`Search`/`ListFindings`):
  a concurrent external write racing one of these live queries can drop the inconclusive entity to
  absent (fake-zero for that query). Fix = route every live reader through one torn-read-safe coherent
  snapshot (or propagate a retryable `U_`). Reviewer-accepted residual of this increment; filed.
- **D2 — per-entity carry-forward of prior projection rows across an inconclusive scan.** Superseded:
  abort-on-inconclusive (IN-2) preserves the WHOLE prior index, so no entity silently drops. A
  finer-grained "update the clean entities, freeze only the raced one" is a future optimisation, not a
  correctness need.
- **D3 — genuine non-torn IO errors (EACCES) classification.** Today finding/requirement scanners turn
  ANY read error into `E_*_INVALID` (finding.go:458, requirement.go:54). This fix distinguishes only
  *inconclusive* (torn/vanished) from *stable*; a genuine EACCES keeps today's behaviour. Re-classing
  EACCES as `unevaluated` is a separate honesty pass; filed.
- **D4 — ticket-directory enumeration second-check** (store.go:2543 single `os.ReadDir`). A file added
  after `ReadDir` is picked up next Rebuild (benign); a file removed is handled by IN-1 (vanished →
  inconclusive → abort). A full listing second-check is deferred.

## 2. Design

### 2.1 `stableReadFile(path) (data []byte, outcome readOutcome, err error)`
`readOutcome ∈ {stable, inconclusive}`. Loop up to N (e.g. 4) attempts: `openat`+read twice with
`O_NOFOLLOW`, keeping the readers' existing regular-file guard; if the two byte-slices are equal →
`stable`. If they differ, or ENOENT/removal is seen at `Lstat` or a read → retry after a small backoff;
after N attempts still unequal/vanished → `inconclusive`. A genuine IO error (EACCES, etc.) → `err`
(today's behaviour, D3). No sleep-free busy loop; backoff is bounded and tiny (Rebuild is not hot).

### 2.2 Rebuild (store.go:1382) — genuinely read-only scan, then mutate
Sol P0-1/r3: Rebuild mutates BEFORE the scan today (`replayUnjournaledEvents` store.go:1388;
`markWorktreeActive` store.go:1456; scan findings store.go:1463-1482 / finding.go:528; AND
`recordRebuildFinding` for invalid git roots store.go:1515-1518, own tx store.go:1244). So "abort →
intact" requires restructuring:
- **Guarantee scope:** an abort leaves the **projections and reconciliation findings** exactly as they
  were (last-known-good). The pre-scan `replayUnjournaledEvents` (idempotent journal replay,
  store.go:1166-1182) and `markWorktreeActive` are allowed before the abort: replay re-runs to the same
  state, and `markWorktreeActive` only touches worktree `active`/`updated_at` metadata (store.go:1754-
  1759) — NOT the entity projections. The plan explicitly accepts + tests that `updated_at` may advance
  on an aborted Rebuild (metadata, not a projection lie); everything else is untouched.
- **Phase A (scan) is READ-ONLY over projections/findings:** `scanTickets`/`scanFindingFiles`/
  `scanRequirements` use `stableReadFile` and collect `valid`, genuine-malformed `findings`, invalid-
  git-root findings, and the `inconclusive` set **in memory** — NO `recordScanFinding` AND NO
  `recordRebuildFinding` DB write during the scan (move BOTH the incremental recording at
  store.go:1463-1482 and the `E_GIT_SCAN` recording at store.go:1515-1518 into Phase B).
- **If `inconclusive` non-empty → return `U_INDEX_UNESTABLISHED`** (transient) BEFORE Phase B — the
  ticket/finding/relation projections and the scan reconciliation findings are untouched (prior
  last-known-good), and NO finding is recorded.
- **Phase B (mutate; only on a fully-stable scan):** the existing full-replace of projections
  (store.go:1539-1549), the deferred recording of genuine findings, AND the scan-finding self-heal:
  ```sql
  DELETE FROM findings WHERE project_id=? AND worktree_id=? AND subtype='reconciliation'
    AND substr(finding_key,1,?) = ?   -- ? = len('scan:'+worktree+':'), 'scan:'+worktree+':'
  ```
  (Sol P1: an exact `scan:<worktree>:` prefix — NOT project/worktree/subtype alone, which would clobber
  the compute/flaky/conservation reconciliation rows at compute.go:328 / testreport.go:507.) Then
  re-insert the current scan's findings. Safe because it runs only post-stable-scan.

### 2.3 Ready (relation_ready.go:378) — ONE shared snapshot (Sol P0-2)
"Union the exclusions" is insufficient: two individually-STABLE scans can read DIFFERENT atomic
versions of a ticket (the ticket scan at store.go:385 sees A with a `blocks` edge; the separate
`scanStoredRelationsAt` at store.go:416 reads A after an atomic rewrite WITHOUT it) → a false-pass with
NO inconclusive read. Fix: **scan tickets ONCE and derive both the ticket rows AND the relations from
that single snapshot** (relations are embedded in the parsed ticket, relation_ready.go:16-40) — remove
the second `scanStoredRelationsAt` scan. Any inconclusive read in that one scan →
`U_RELATION_GRAPH_UNESTABLISHED` (relation_ready.go:429/542-556). Never compute readiness from two
skewed scans or a partial graph.
- **`relationIndexDivergence` (Sol r3 P0-2) is a HIDDEN third scan:** called at relation_ready.go:420,
  it internally re-runs `scanStoredRelationsAt` (relation_ready.go:317-320) → another live ticket
  scan. Refactor it to **accept the canonical relations from the single snapshot** rather than
  rescanning. `relationIndexFindingKind` (relation_ready.go:356-364) rereads owner files to classify
  divergence — it must consume the same snapshot bytes (or, since it only downgrades a stale derived
  row to a **warning** that never fails/blocks a canonical-ready ticket per relation_ready.go:430-434,
  an inconclusive read there degrades to that same warning, never a false-pass). The invariant: **every
  live read in the `Ready` path comes from ONE coherent snapshot.**

### 2.4 Caller contract — U_INDEX_UNESTABLISHED as a structured UNEVALUATED result (Sol P1)
Returning the code as a bare error yields `CheckReport{}`+error, not the UNEVALUATED report/data
contract (`Check` maps only `isIntegrityError` today, check.go:188-193; the core `reconcile --rebuild`
path returns the raw error, core.go:1134-1143). Exact handler shapes (Sol r3 P1-2):
- **`Check`** branches explicitly on a `U_INDEX_UNESTABLISHED` Rebuild return BEFORE returning
  `CheckReport{}` — producing an **unevaluated** report (the ticket/relation/readiness dimensions are
  `unevaluated`, never a partial pass).
- **`reconcile`/rebuild handler** returns `handlerData{Verdict:"unevaluated", Data:…}` (a structured
  SUCCESSFUL response per core.go:353-363), NOT `handlerData.Code=U_…` (which yields `OK:false`,
  core.go:338-339).
- **All non-`U_INDEX_UNESTABLISHED` Rebuild failures stay HARD errors** (unchanged).
Faces render the transient code as a retryable `unevaluated`. Tested in both directions.

## 3. §1b — resolutions (r1 incorporated)

- **R1 (content compare + retry).** Byte compare, not mtime/size; bounded retry resolves transient
  tears to ground truth so the common case is invisible; persistent tear → abort.
- **R2 (abort, not partial — Sol P0-1/P0-2).** An inconclusive scan mutates nothing and answers
  nothing from a partial scan; it returns `U_INDEX_UNESTABLISHED`/`U_RELATION_GRAPH_UNESTABLISHED`.
  Eliminates the readiness false-pass and the query fake-zero.
- **R3 (never hard-fail on torn/vanished — Sol P1).** ENOENT at `Lstat` or read → inconclusive (retry
  → abort), never a returned hard error. `O_NOFOLLOW` closes the symlink-swap gap. EACCES stays a real
  error (D3).
- **R4 (never mask real corruption).** A stable malformed file reports `E_*_INVALID` (test, false-neg
  direction). The retry resolves to the file's true post-write content; it does not retry a stable
  malformed file into "inconclusive".
- **R5 (self-heal safe — Sol P1).** Full-replace of `scan:` findings runs only on a fully-stable scan
  (abort precedes it), so it repairs pre-existing spurious findings AND clears fixed-file findings
  without a torn read ever erasing a genuine one.
- **R6 (torn-but-parseable caught).** Byte compare catches any difference, so a torn read that parses
  is inconclusive too — no relation poisoning.
- **R7 (accepted residual).** A writer that pauses between the two reads with byte-identical partial
  content is indistinguishable from stable (same residual as the existing gate/trace tear-checks);
  sub-millisecond, external non-atomic writers do not pause mid-write. Written down.
- **R8 (Rebuild scan phase is read-only — Sol P0-1).** No `recordScanFinding`/projection write during
  Phase A; findings collected in memory, recorded only in Phase B on a stable scan. Pre-scan
  `replayUnjournaledEvents`/`markWorktreeActive` are idempotent, so running-then-aborting is safe
  (asserted + tested for idempotency across an abort).
- **R9 (Ready uses ONE snapshot — Sol P0-2).** Ready derives ticket rows AND relations from a single
  ticket scan; the second `scanStoredRelationsAt` is removed. Closes the two-stable-scans version-skew
  false-pass, not just the inconclusive case.
- **R10 (structured unevaluated contract — Sol P1).** `U_INDEX_UNESTABLISHED` surfaces as an
  UNEVALUATED report/handler result (Check + reconcile), never a bare error or partial pass.
- **R11 (exact self-heal predicate — Sol P1).** DELETE targets only `subtype='reconciliation'` rows
  whose `finding_key` has the exact `scan:<worktree>:` prefix; compute/flaky/conservation rows are
  untouched.
- **R12 (all pre-projection Rebuild writes deferred — Sol r3 P0-1).** BOTH `recordScanFinding` AND
  `recordRebuildFinding` (E_GIT_SCAN invalid-root, store.go:1515) are collected in memory and recorded
  only in Phase B. The abort-intact guarantee is scoped to projections + reconciliation findings;
  `markWorktreeActive`'s `updated_at` may advance on an abort (metadata, accepted + tested).
- **R13 (ONE coherent snapshot for the whole Ready path — Sol r3 P0-2).** `Ready` +
  `relationIndexDivergence` + `relationIndexFindingKind` all consume a single ticket snapshot; no
  hidden `scanStoredRelationsAt` rescan. An inconclusive read anywhere in that path →
  `U_RELATION_GRAPH_UNESTABLISHED`, and an index-divergence-only inconclusive degrades to the existing
  warning that never fails/blocks a canonical-ready ticket (relation_ready.go:430-434).
- **R14 (live-query residual is a stated boundary — Sol r3 P1).** `List`/`Search`/`ListFindings` remain
  outside this increment's honesty contract (D5), a reviewer-accepted written-down residual, with a
  test documenting the boundary.

## 4. Tests (Sol P2 — must FAIL against today's single-read code)

To exercise TODAY's bug the seam must make the TWO reads observe DIFFERENT bytes (a genuine tear): a
`scanReadHook`/injected reader that returns **payload A on read-invocation 1 and payload B on
invocation 2** (a per-invocation sequencer, synchronized), then stabilises. On today's single-read path
the reader sees one torn payload → records a finding (test asserts today FAILS); on the fixed
double-read the two differing reads → `inconclusive`. A one-time "swap at read start" is NOT a valid
discriminator (both new reads would see identical post-swap bytes → a stable malformed file, not a
tear) — the sequencer is mandatory.
1. **Torn ticket → no fake finding + no index mutation (fail-before):** valid committed ticket; hook
   swaps bytes at read time → today records a spurious `E_CONFIG_INVALID`; fixed → Rebuild returns
   `U_INDEX_UNESTABLISHED`, records NO finding, and the prior index is unchanged.
2. **Readiness false-pass prevented (Sol P0-1):** ticket B `blocked-by` A; a torn read of A's relation
   during `Ready` → today B can show ready; fixed → `U_RELATION_GRAPH_UNESTABLISHED`, B not ready.
3. **Query fake-zero prevented (Sol P0-2):** an entity present in the prior index; a torn scan during
   Rebuild → the entity is NOT dropped to absent (Rebuild aborted; prior rows intact / list returns
   the entity or a `U_`), never an empty result.
4. **Stable malformed file STILL reports (false-negative guard):** malformed ticket/finding/
   requirement, no hook (byte-identical) → `E_*_INVALID` recorded.
5. **Vanished mid-scan → no hard error:** hook deletes the file between reads → inconclusive → Rebuild
   returns `U_INDEX_UNESTABLISHED` (today ENOENT-hard-fails).
6. **Self-heal (Sol P1):** a genuinely-malformed file records `E_CONFIG_INVALID`; fix the file; a
   clean Rebuild → the finding is CLEARED (fails today — no prune).
7. **Retry resolves a transient tear invisibly:** a single mid-read swap that then stabilises → the
   retry reaches a stable read → normal Rebuild, no `U_`, correct final content.
8. **Ready path uses ONE snapshot (Sol r3 P0-2):** a scan-count hook asserts the whole `Ready` path
   (rows + relations + `relationIndexDivergence`) performs exactly ONE ticket scan — proving the hidden
   `scanStoredRelationsAt` rescan is gone (fails against today's multi-scan code).
9. **Abort-intact for pre-scan mutations (Sol r3 P0-1):** an earlier invalid git root plus a later
   inconclusive worktree → Rebuild returns `U_INDEX_UNESTABLISHED` and records NO `E_GIT_SCAN` finding
   (proves `recordRebuildFinding` was deferred); a second clean Rebuild then records it.
10. **Live-query boundary (D5, documented):** a test asserting the CURRENT `List`/`Search` behaviour on
    a torn live read, marking it the reviewer-accepted residual so a future D5 fix has a pinned starting
    point (not a fail-before test — it documents the accepted boundary).

## 5. Files

- `internal/store/store.go` (or `scan_read.go`): `stableReadFile` + `scanReadHook`; Rebuild two-phase
  + scan-finding full-replace.
- `internal/store/query.go`, `finding.go`, `requirement.go`: readers use `stableReadFile`; scanners
  propagate the inconclusive outcome.
- `internal/store/relation_ready.go`: unify Ready's scans + propagate inconclusive.
- New stable code `U_INDEX_UNESTABLISHED` registered in the code catalog.
- `internal/store/*_test.go`: §4 suite. Follow-ups filed for D2/D3/D4.

## 6. Risks / expected yield

1. **False-negative (masking corruption)** — highest; guarded by R4 + test 4.
2. **Readiness false-pass / query fake-zero** — the Sol-found P0s; eliminated by abort-not-partial
   (R2) + tests 2, 3.
3. **`U_INDEX_UNESTABLISHED` surfacing to users** — mitigated by the bounded retry (transient tears
   resolve invisibly, test 7); a persistent external racer is pathological and the transient signal is
   honest.
4. **Self-heal correctness** — the full-replace must not erase a genuine finding; safe because it runs
   only post-stable-scan (R5), guarded by test 6.

## 7. Deferrals (filed)

- D2 per-entity carry-forward (superseded by abort-on-inconclusive; future optimisation).
- D3 EACCES/genuine-IO-error re-classification to unevaluated.
- D4 ticket-directory enumeration second-check.
