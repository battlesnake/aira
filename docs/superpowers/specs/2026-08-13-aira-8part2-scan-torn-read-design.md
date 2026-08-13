# #8-part2 — crash-recovery scan torn-read race (honest, never a fake fail)

Status: PLAN (awaiting Sol plan-review → gate → build). Branch `codex-aira-8part2` off master `4374992`.
Task: #8 part 2 (part 1 — ParseTicket/ParseRequirement trailing-content — landed `8183345`).
Class: **crash-recovery correctness → the two-loop adversarial process is mandatory** (CLAUDE.md).

## 0. The bug

`(*Store).Rebuild` (store.go:1382) reconstructs the SQLite index by scanning the **live working
tree**. Each scan reader does `Lstat` then a single `os.ReadFile`:
- tickets: `readRegularTicket` (query.go:352-360) → `domain.ParseTicket`
- findings: `readRegularFinding` (finding.go:72-80) → `domain.ParseFinding`
- requirements: `readRegularRequirement` (requirement.go:96-104) → `domain.ParseRequirement`

AIRA's own writes are **atomic** (`writeAtomic`, store.go:2076-2125: temp+O_EXCL+fsync+rename+
dir-fsync), so AIRA never tears its own files. But an **external non-atomic writer** — `git
checkout`/`merge`/`rebase` rewriting `.aira/**` files, or a non-atomic editor save — racing Rebuild
can be observed mid-write: a partial/empty read, or a torn-but-still-parseable read. The
consequence today:
- a torn read that fails to parse → a **spurious `E_CONFIG_INVALID` / `E_FINDING_INVALID` /
  `E_REQUIREMENT_INVALID`** scan finding (store.go:2565-2578; finding.go:457-472; requirement.go:53-68);
- a torn-but-parseable ticket → **poisoned relation projection** (relations are embedded in the
  parsed ticket, relation_ready.go:16-40, store.go:1598-1609);
- a file **removed** between `ReadDir` and its read → `os.ReadFile` ENOENT → the scanner returns the
  error → **Rebuild hard-fails** (store.go:2570-2572, 1460-1461).

This is a **fake fail** — precisely what AIRA's honesty rule forbids ("a check that cannot
establish its result reports unevaluated, never a fake pass or zero" — and never a fake *fail*).

## 0.1 Product decision — WORKING-TREE authority (owner-delegated; recorded)

Crash-recovery recovers from the **working tree**, not the committed/staged tree. Rationale: AIRA is
machine-local coordination whose architecture is an *index rebuildable from git-file content in the
working tree*; agents create tickets/findings/requirements as **uncommitted** working-tree files that
are live coordination state. A committed/staged-authority fix (a pinned `git ls-files`/`cat-file`
snapshot) would drop uncommitted state on recovery — a fundamental departure. So the fix preserves
working-tree semantics and makes torn reads *honest*, not authoritative-elsewhere. (This resolves the
A-vs-B question in `~/tmp/aira-design/8part2-rebuild-race-analysis.md`: candidate **B**.)

## 1. Scope

**IN:**
1. A shared **stable-read** helper: read the file twice and byte-compare; if the two reads differ, or
   the file vanished between reads, the read is **inconclusive** (mirrors the existing tear-checks
   `captureTraceSnapshot` traceability.go:54-85 and `discoverGates` gate_index.go:88-105, which
   compare **content** — not mtime/size, since a same-size rewrite keeps both).
2. The three scan readers use it. An **inconclusive** read (torn or vanished-mid-scan) yields a
   distinct sentinel — NOT a parse failure and NOT a hard error.
3. On inconclusive: **never fabricate a fail finding** and **never hard-fail Rebuild**. The file is
   **excluded from this scan** (unknown-this-scan), resolved on the next clean Rebuild. Tickets reuse
   the existing `excludedTicketPaths` boundary → readiness honestly reports
   `U_RELATION_GRAPH_UNESTABLISHED` (relation_ready.go:385-429, 542-556). Findings/requirements
   inconclusive files are skipped this scan (transient, self-healing).
4. A **genuinely-malformed committed file** (byte-identical across both reads) STILL parses malformed
   both times and STILL reports its `E_*_INVALID` finding — the fix must not mask real corruption.
5. Deterministic tests that induce a torn read via a mid-scan write hook (mirroring the existing
   `traceabilitySnapshotHook` seam), proving fail-before / pass-after.

**OUT (written-down deferrals / discovered adjacent issues, filed as follow-ups):**
- **D1 — scan-finding self-heal gap (discovered, pre-existing):** scan reconciliation findings are
  upsert-only (finding.go:528) with NO prune; Rebuild deletes only `subtype='review'` (store.go:1552),
  never scan reconciliation rows. So a *genuinely* malformed file that is later **fixed** keeps its
  finding forever (the clean scan just makes no new `recordScanFinding` call). This is independent of
  the torn-read race (it affects genuine findings too) and is a broader reconciliation change
  (full-replace with an inconclusive carve-out). **Filed as a separate ticket; NOT in this scope.**
  Note: because of D1, the torn-read fix deliberately records **no** finding (not even a `U_`) for an
  inconclusive file — persisting one would inherit D1's no-prune permanence (Terra's warning).
- **D2 — carry-forward prior projection for an inconclusive file:** this plan lets an inconclusive
  file transiently drop from any full-replaced projection until the next clean Rebuild (honest —
  the file's state is genuinely unknown this scan — and self-healing). Preserving the exact prior
  index/relation rows across an inconclusive scan is a larger reconciliation change; deferred.
- **D3 — directory-enumeration second-check:** the ticket scan enumerates via a single `os.ReadDir`
  (store.go:2543), unlike gates/traceability which second-check the listing. A file *added* after
  `ReadDir` is simply picked up next Rebuild (benign). A file *removed* after `ReadDir` is handled by
  IN-3 (vanished → inconclusive, not a hard error). A full listing second-check is deferred.

## 2. Design

### 2.1 `stableReadFile`
```
// stableReadFile reads path twice and returns the bytes only if both reads are byte-identical.
// It returns (data, true, nil) on a stable read; (nil, false, nil) when the file is mid-write
// (reads differ) or vanished between reads (ENOENT) — an INCONCLUSIVE read, never an error to the
// caller. Genuine IO errors (permissions, etc.) are returned as errors as today.
func stableReadFile(path string) (data []byte, stable bool, err error)
```
- Two `os.ReadFile` (preserve the existing regular-file/`O_NOFOLLOW` guards the readers already
  apply). Compare with `bytes.Equal`.
- ENOENT on either read (removed mid-scan) → `stable=false, err=nil` (inconclusive, not a hard error).
- Content differs → `stable=false, err=nil` (mid-write).
- Both identical → `stable=true`.
- Cost: one extra read per file per Rebuild. Rebuild is not a hot path; correctness over a 2× read.
  (Documented.)

### 2.2 Reader integration
- `readRegularTicket`/`readRegularFinding`/`readRegularRequirement` gain an inconclusive result path
  (return a sentinel or `(nil, inconclusive)`), replacing their single `os.ReadFile`.
- `scanTickets` (store.go:2543): on inconclusive → `exclude(path)` (adds to `excludedTicketPaths`) and
  `continue` — **no** `scanFinding`/append. A genuine `E_CONFIG_INVALID` (stable malformed) path is
  unchanged. A raw IO error is still returned as today.
- `scanFindingFiles` / `scanRequirements`: on inconclusive → skip the file (not indexed, no finding).
- No scanner returns a hard error for a torn/vanished read.

### 2.3 Honesty framing
A torn/vanished read means the scan **cannot establish** the file's state this pass → it is excluded
(unknown-this-scan), never a fabricated fail, never an erased prior truth via a new finding. This is
identical in spirit to `U_TRACE_UNSCANNED` / `U_GATE_EVIDENCE_UNAVAILABLE`. Readiness already surfaces
excluded tickets honestly as `U_RELATION_GRAPH_UNESTABLISHED`.

## 3. §1b — pre-empted resolutions (anticipating Sol)

- **R1 (content compare, not mtime/size).** A same-size in-place rewrite keeps mtime granularity and
  size; only a byte compare is sound. Matches gate_index.go:103 / traceability.go:81.
- **R2 (never fabricate, never erase).** Inconclusive → record NO finding (avoids inheriting D1's
  no-prune permanence) AND leave prior state untouched via a new finding. The stable-malformed path is
  untouched, so real corruption still reports (test-guarded, both directions).
- **R3 (never hard-fail Rebuild on a vanished/torn file).** ENOENT/torn is inconclusive, not an error;
  a raw IO error (EACCES etc.) is still an error. Distinguish precisely.
- **R4 (self-healing, honest transient).** An inconclusive file resolves on the next clean Rebuild;
  the transient exclusion (and any transient projection drop, D2) is honest, not a fake state.
- **R5 (torn-but-parseable is still caught).** The byte compare catches ANY difference, so a torn read
  that happens to parse (poisoning relations) is inconclusive too — not just unparseable tears.
- **R6 (scope discipline).** D1 (self-heal prune) and D2 (carry-forward) are genuinely separate,
  larger changes; filed, not silently skipped (CLAUDE.md: coverage gaps written down, never silent).

## 4. Tests

TDD, fail-before/pass-after. Induce a deterministic torn read with a mid-scan write hook.
- **Add a `scanReadHook` seam** (mirroring `traceabilitySnapshotHook`) fired between the two reads in
  `stableReadFile`, so a test can replace the file's bytes exactly between read 1 and read 2.
1. **Torn ticket → no spurious finding + excluded (fail-before):** a valid committed ticket; the hook
   rewrites it to different bytes between the two reads → the read is inconclusive → assert NO
   `E_CONFIG_INVALID` finding recorded AND the ticket is in `excludedTicketPaths` (readiness →
   `U_RELATION_GRAPH_UNESTABLISHED`). Against today's code this FAILS (a spurious finding appears).
2. **Torn-but-parseable ticket → no relation poisoning:** the hook rewrites to *different but valid*
   bytes → inconclusive → excluded → relations NOT indexed from the torn read.
3. **Genuinely-malformed committed file (stable) → STILL reports (false-negative guard):** a malformed
   ticket/finding/requirement, no hook (byte-identical reads) → the `E_*_INVALID` finding IS recorded.
   Proves the fix does not mask real corruption.
4. **Vanished mid-scan → no hard Rebuild error:** the hook deletes the file between the two reads →
   inconclusive → the file is skipped/excluded and Rebuild SUCCEEDS (today it ENOENT-hard-fails).
5. **Findings + requirements torn → no spurious `E_FINDING_INVALID`/`E_REQUIREMENT_INVALID`**, same
   shape as (1).
6. **Self-heal-across-clean-Rebuild for the torn case:** after (1), a second clean Rebuild (no hook)
   re-reads the now-stable valid file → it is indexed, not excluded, and still no spurious finding.

## 5. Files

- `internal/store/store.go` (or a small `scan_read.go`): `stableReadFile` + the `scanReadHook` seam;
  `scanTickets` inconclusive handling.
- `internal/store/query.go`: `readRegularTicket` inconclusive path.
- `internal/store/finding.go`, `internal/store/requirement.go`: reader inconclusive paths + scanner skips.
- `internal/store/*_test.go`: the §4 tests (a torn-read hook-driven suite).
- `docs/.../2026-08-13-aira-8part2-scan-torn-read-design.md` (this file).
- Follow-up tickets filed for D1 (scan-finding self-heal prune) and D2 (carry-forward).

## 6. Risks / expected yield

1. **False-negative (masking real corruption)** — the highest risk: the fix must never turn a
   genuinely-malformed *stable* file into "inconclusive". Byte-identical reads of a malformed file
   parse malformed both times → reported. Test 3 guards this in the false-negative direction.
2. **Distinguishing inconclusive from a real IO error** (R3) — ENOENT/torn ≠ EACCES. Precise handling
   + test 4.
3. **The two-read window is not infinitely wide** — a writer paused *between* the two reads with
   byte-identical partial content is theoretically indistinguishable from a stable file (same residual
   as the existing gate/trace tear-checks). Written-down accepted residual; the window is
   sub-millisecond and external non-atomic writers do not pause mid-write.

## 7. Deferrals (filed as follow-up tickets)

- D1: scan-finding self-heal — full-replace/prune stale scan reconciliation findings so a fixed file's
  finding clears (pre-existing, affects genuine findings; independent of the torn-read race).
- D2: carry-forward prior projection across an inconclusive scan (avoid the transient drop).
- D3: ticket-directory enumeration second-check.
