# Traceability for every project: discovery and shelved proposal

Status: SHELVED (owner, 2026-11-14). Nothing is built. This records what was learned so the idea
can be picked up later without repeating the discovery. A held ticket points here.

## 1. The question

Can `aira` give any project an easy traceability check? The four checks the owner named:

1. every unit of production code traces to a requirement;
2. every test traces to a requirement;
3. every unit of code has an associated test;
4. every requirement row is met by production code AND by tests that cite it.

## 2. What aira does today (read from the code, not run on a real project)

- Requirements are files `.aira/requirements/<ID>.md` with a closed status (`built`, `partial`,
  `designed`, `planned`, `boundary`, `retired`, `superseded`), managed by `aira req
  add|ls|show|set|import`. Design: `docs/superpowers/specs/2026-08-09-aira-m9-traceability-design.md`.
- `aira check` has a `traceability` dimension (`internal/store/traceability.go`). It parses Go with
  `go/parser`; `covers: ID[, ID]` counts in a production file, `verifies: ID` in a `_test.go`; the
  wrong kind in the wrong file type is ignored. Results: a citation to a missing requirement is
  `E_TRACE_DANGLING` (fails the check); a `built` or `partial` requirement with no `covers` is
  `W_TRACE_UNCOVERED`, a `built` one with no `verifies` is `W_TRACE_UNVERIFIED` (warnings, advisory);
  an empty registry or a scan failure is `unevaluated`, never a pass. `aira insights` carries a live
  `traceability-status` gauge.
- Gaps against the four checks: it never lists code or tests that lack a citation (checks 1-3 unmet);
  check 4 is existence-of-a-citation only; it reads Go only; one `verifies:` anywhere credits the whole
  requirement; it does not know whether the cited test passed.

## 3. Discovery: what each project said (2026-11-14)

Five peer sessions answered the same five questions (languages and where requirements live, existing
tracing, which checks they would use, constraints, input preference).

| Project | Shape | Would use | Would not use | Input preference |
|---|---|---|---|---|
| space (adapter) | Go, Python, shell, Terraform; requirements are prose, a decision register and tickets | check 4; check 2 for orphan tests | 1 and 3 (most of the repo is Terraform/shell/CI config; "owner said X" rows are not testable) | manifest it writes; include/exclude globs; a per-row opt-in |
| spice (flavour kernel) | Rust, Python, TS; the spec is an external oracle (recorded fixtures), not a requirements list | checks 2 and 4, citing fixture rows | 1 and 3 | manifest; explicit scan roots only (a vendored LGPL tree must never be walked) |
| field (stoner) | Rust, Python, shell, TS; a 15-row table with Implemented-by and Verified-by cells | check 4 in path-resolving form (every cited path and `path::test_name` exists; `built` names at least one test); dangling citations | 1, 2, 3 at unit grain | the table itself is the manifest; resolve by cheap text lookup; annotation scanning optional |
| ci-build (tooling) | Go `clopstrace`, `implements:`/`verifies:` tags, joins `go test -json` | 1, 2, 4 as failures on dangling or missing cites | 3 hard, or any uncited-code count as blocking from day one | one reusable tool driven by per-project config |
| speed (fastest.ee) | Python `check_traceability.py`, 1,000+ rows | the richest; wants aira to own the graph checks | n/a | per-language extractors feeding aira, plus a test-outcome join |

Common ground: check 4 and dangling/retired-citation failures are wanted by everyone. Checks 1 and 3
are noise at unit grain for most repos. Three of five want a language-neutral manifest their own
tooling produces; none want aira scanning comments in four languages.

Specific asks worth keeping:
- Resolve `path` and `path::test_name` citations by a configurable regex (Rust `fn name` under
  `#[test]`, Python `def test_`, any language).
- Show "verified only by an ignored or skip-guarded test" as a separate class from a passing gate test
  (field, speed).
- Let the "requirement" side be any registry, for example diagnostic codes with a test that triggers
  each (field).
- A per-project exemption list with a reason per entry, and a committed known-gaps baseline (one
  `file:id` per line, deleting a line tightens it) with no stored state of its own (ci-build, speed).
- Exit codes 0 pass, 1 fail, 2 usage, 3 unevaluated; an empty registry must never read green
  (ci-build). Plain text one finding per line is enough; no JUnit/SARIF asked for.
- `aira req import` needs `requirement_prefixes: ["STONR"]` hand-added to `.aira/config` (no CLI flag).
  That friction is real and independent of everything else here.
- Never compile or run the test suite in the check; a text scan is fine (field).

## 4. What fastest.ee's tooling teaches

`fastest_ee/tools/engine_dev/check_traceability.py` (stdlib Python, about 410 core lines of 1,380) is the
most developed mechanism. Worth copying: the registry grammar (one pipe table per id prefix; ids
`[A-Z]{2,}-\d+[a-z]?`; status a single bare word, or `superseded by <ID>`; half-edited status cells
rejected), canonical-case ids, duplicate-id detection, and fail-closed graph checks in both
directions (a built row must name existing code that cites it back; every cite resolves to a live
row; citing a retired row fails with a "retarget or remove" remedy).

Its recorded false-pass traps, to avoid:
- **Cited, not passed.** A skipped, xfailed or never-collected test still counts as verifying.
- Whole-file regex: `verifies: X` in a comment or string literal counts.
- One citation in a test file credits the whole requirement; a `covers:` on a module that does not
  implement the requirement passes.
- Scan roots are a hand-listed tuple; a new tree is silently unscanned.
- About 43% of production modules carry no citation and nothing reports them.
- Incidents: a two-letter id regex dropped three-letter families; a half-edited status cell read as
  live; two worktrees allocated the same new id and the duplicates merged cleanly.

## 5. The cross-repo study and the pending decision

ci-build wrote `~/heap/trace-reuse/PROPOSAL.md` (a local file outside this repo, 2026-11-13): the
family already has four mechanisms with nearly identical rules (status enum, markdown registry,
`covers:`/`implements:` and `verifies:` tags, dangling fails, `built` needs code plus test). It
recommends ONE implementation, Python 3 stdlib, in `shared-crates/standards/trace/` beside `fc-lint`,
vendored by sha pin where there is no submodule (option A); alternatives were extending `clopstrace`
in Go (B) and a Rust crate (C). Its checks C1-C7 are: unique ids; status grammar; a built row names
existing code; every marker resolves to a live row; a built row has a verifier; with run results,
each verifier of a built row passed (no results file means unevaluated); an optional glob rule that
every source file carries a marker. It lists "a rewrite of aira's checker" under do-not-build, and
the owner decision on its home and language is still pending (as of 2026-11-14).

## 6. Option considered for aira: a manifest-first checker inside `aira check`

Not adopted, shelved. Recorded as the leading candidate.

Premise: the project owns extraction; aira owns the graph checks. Greenfield minimum: a list of
citation records `{requirement id, kind covers|verifies, path, optional symbol}` plus the requirement
registry.

Edge sources, all feeding one graph: (a) the Implemented-by and Verified-by cells of a registry
table; (b) a manifest file the project's own tooling writes; (c) the existing Go comment scan.

Phases:
- **S1.** Resolve every cited path and `path::symbol` (git file list plus a per-language regex),
  dangling/retired failures with remedy text, `built` needs at least one resolvable code and test
  citation, explicit include and exclude roots, requirement prefixes from config (fix the
  `requirement_prefixes` friction), a committed known-gaps file as baseline, exit-code semantics above.
- **S2.** Join citations to `aira test-report` (ingests `go test -json` and JUnit with per-test
  results) so only a passing test earns credit and skipped or ignored tests are reported as their own
  class. Risk: matching a citation's test name to a report's test name; pytest JUnit carries no file
  attribute, so that join is approximate.
- **S3, opt-in per directory.** List uncited code and tests (checks 1-3), advisory against the
  baseline, never blocking from day one.

Arguments for aira owning it: it already has the requirement entity, the check dimension, gates and
ratchets, and test-report ingestion; it is one static Go binary, so no project image needs a new
runtime (stoner's image has no Python); the owner's direction (2026-11-13, via ci-build) is one
reusable tool driven by per-project config. Arguments against: it widens aira's scope; a separate
shared tool would keep aira unchanged; existing mechanisms (speed's, tooling's) already run and could
simply be kept.

What a rewrite should not do: per-language parsers or plugin APIs in aira, a database or daemon for
baselines, the dead-code advisory, docs-hub or payments gates (these stay in fastest-ee).

## 7. Open questions for when this is revisited

1. Owner decision: aira-owned manifest consumer (section 6) versus ci-build's shared Python tool
   (section 5)? Or both, with aira consuming the other tool's output?
2. Where does the registry live for projects with none (space, spice)? Nodes in `.aira/requirements`
   with an opt-in flag per row, or a table read directly?
3. Stable test identity: what names do citations use so they join to `aira test-report` rows
   (`go test` names, pytest node ids, cargo test paths)?
4. Does the baseline reuse `aira gate` ratchet machinery or stay a plain file?
5. Should `aira check` exit non-zero on warnings, or only on failures? (Not verified: what exit code a
   warning-only traceability result gives today.)

## 8. How to resume

Read this file, then `docs/superpowers/specs/2026-08-09-aira-m9-traceability-design.md` and
`internal/store/traceability.go`. Re-ask the five peer sessions only if their projects have changed.
If the owner decides to proceed: write a plan spec, create held tickets under a later milestone, run
the full two-loop (the check dimension touches the `aira check` verdict contract).
