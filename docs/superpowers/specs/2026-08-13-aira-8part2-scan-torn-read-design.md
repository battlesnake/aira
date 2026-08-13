# #8-part2 — crash-recovery scan torn-read race (honest, never a fake fail / false-pass / fake-zero)

Status: PLAN v2 (incorporates Sol plan-review r1: 2×P0 + 2×P1 + P2). Awaiting Sol re-review → gate → build.
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
4. **`Ready` (Sol P0-1):** unify its scans so a torn read in EITHER the ticket or the relation scan
   propagates to `graphUnevaluated` → `U_RELATION_GRAPH_UNESTABLISHED` (never a false-pass). Scan once
   and share the snapshot, or union the exclusions from `scanStoredRelationsAt`.
5. **Scan-finding self-heal (Sol P1; folds in D1 safely):** on a fully-stable scan, **full-replace**
   the worktree's `scan:` reconciliation findings (delete the worktree's scan reconciliation rows, then
   re-insert the current scan's set) so a fixed file's finding CLEARS — and any pre-existing spurious
   torn-read finding is repaired. Safe because the replace only runs when the scan is fully stable
   (an inconclusive scan aborts at IN-2 before this point, so a torn read can never erase a genuine
   finding).
6. Deterministic tests that genuinely FAIL against today's single-read code (Sol P2 — see §4).

**OUT (written-down deferrals / follow-ups):**
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

### 2.2 Rebuild (store.go:1382) two-phase
- **Phase A (scan, read-only):** run `scanTickets`/`scanFindingFiles`/`scanRequirements` using
  `stableReadFile`. Collect `valid`, genuine-malformed `findings`, and an `inconclusive` set.
- **If `inconclusive` non-empty → return `U_INDEX_UNESTABLISHED`** (transient, retryable) — do NOT
  enter the mutation transaction. Prior index intact.
- **Phase B (mutate, as today) + self-heal:** full-replace projections (store.go:1539-1549) AND
  full-replace the worktree's `scan:` reconciliation findings (IN-5). Genuine findings recorded.

### 2.3 Ready (relation_ready.go:378)
Make the ticket + relation scans share one stable snapshot (or union both exclusion sets). Any
inconclusive read → `graphUnevaluated=true` → `U_RELATION_GRAPH_UNESTABLISHED` (the existing honest
surface, relation_ready.go:429/542-556). Never compute readiness from a partial graph.

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

## 4. Tests (Sol P2 — must FAIL against today's single-read code)

To exercise TODAY's bug, drive a concurrent write synchronized with the reader via a test seam placed
so it fires for BOTH the current single read and the new double read (e.g. a `scanReadHook` invoked at
the START of a read, which the test uses to swap the file bytes; on today's single-read path this
produces the torn finding, and the test asserts that today FAILS).
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
