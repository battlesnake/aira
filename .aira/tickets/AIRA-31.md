---
{"schema":1,"id":"AIRA-31","project":"aira","title":"aitest Slice 2 — output fidelity: JUnit XML, coverage combine, TestReport replay","status":"done","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["aitest","pytest","reporting"],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-31","to":"AIRA-32"},{"kind":"blocks","from":"AIRA-31","to":"AIRA-33"},{"kind":"relates","from":"AIRA-31","to":"AIRA-51"}]}
---
Spec: docs/superpowers/specs/2026-09-01-aitest-design.md (§3.2, §5 Slice 2).

Wire full xdist-equivalent output fidelity: supervisor replays worker-streamed
TestReport objects into its own real pytest hooks (reusing pytest's junitxml
and terminalreporter plugins unmodified, xdist's own proven pattern) and
coverage.py parallel-mode combine. Blocked by Slice 1 (needs the fork/
admission/dispatch loop to attach reporting to). Slice 1 (AIRA-30) merged+
deployed 2026-09-02.

REAL EXTERNAL DEMAND (2026-09-03): fastest-ee-dc (peer session extending
aitest into fastest-ee's scripts/merge_gate.sh) was blocked on this
specifically -- their scripts/leg_verdict.sh parses JUnit-XML-shaped output
to classify a test leg's verdict, and misclassified a failing aitest run as
blank because Slice 1 only emits plain terminal pass/fail/unevaluated lines.

**DONE + DEPLOYED (2026-09-03, master via PR #4, squash commit `05e005a`).**
Full two-loop, unusually deep given the risk class (silently-wrong test
reporting is worse than a crash -- nobody notices):

**Plan review — 6 full rounds across two independent lineages** (Codex/Sol,
a Claude Fable reviewer), each catching a real, distinct bug before any code
was written:
- JSON silently losing pytest's tuple-typed fields (longrepr/location/
  sections) -- would crash junitxml's own `assert isinstance(report.longrepr,
  tuple)` on any real skip/failure. Live-probe-verified; fixed with a
  collision-safe tag/untag codec.
- A twice-crashed test going completely missing from JUnit XML (no
  `<testcase>` element at all, not even a failure).
- `COVERAGE_PROCESS_START`'s bare auto-start not firing for a forked (not
  exec'd) worker, and independently, conflicting with pytest-cov's own active
  collector -- resolved by having aitest own NOTHING about coverage lifecycle:
  just save whatever Coverage.current() already is before a forked child's
  os._exit(), gated on run:parallel so a non-parallel setup's data is never
  destroyed.
- **pytest's fd-capture mechanism (FDCaptureBase) silently cross-contaminating
  captured output between concurrent forked workers** -- the most subtle
  finding, live-verified with real cross-test data bleeding (one test
  capturing 73 lines, 55 "foreign"). Would have shipped silently-WRONG
  `<system-out>`/`<system-err>` content, invisible in Slice 1 only because it
  discarded captured output entirely.
- A crash/retry race that could double-count a result if events were staged
  before a crash; fixed via validate-the-whole-batch-before-replaying-any-of-it.
- Wire-format ambiguity, per-event nodeid validation, and report-shape
  correctness (a literal `outcome="unevaluated"` is silently ignored by
  junitxml -- must be `outcome="failed"` with an honest message).

**Build, then adversarial build-review** (independent from the
implementation): verdict FIXABLE, no correctness bug found. The build's OWN
end-to-end testing caught a 7th real bug the plan's own literal instruction
would have introduced: `pm.unregister(terminalreporter)` breaks pytest's rich
assertion-diff rendering (`get_terminal_writer()` asserts that plugin is
non-None) -- silently destroying real failure diagnostics, exactly the
wrong-data failure class this whole slice exists to prevent. Fixed by muting
the writer instead of unregistering the plugin; the build-review independently
re-verified this is correct (byte-identical rich-diff output between plain and
aitest-driven runs under --color=yes, not just "doesn't crash"). Mutation
testing (11 mutants) found 3 load-bearing invariants the test suite didn't
actually pin -- all closed before merge.

Verified: `aira confine --memory-reserve 3G -- make ci` all 13 packages ok.
`aira confine --memory-reserve 768M -- python3 -m pytest -q
internal/pylib/aitest/` -- 105 passed. Real `pytest --cov` regression test
(coverage/pytest-cov installed for this specifically). Real fd-capture
cross-contamination regression test with a deterministic barrier. Real
`--color=yes -vv` rich-diff fidelity test.

DEPLOYED: binary rebuilt (no functional Go-side change -- this slice is pure
Python -- rebuilt for consistency), atomic swap, daemon restarted, confirmed
active. The actual deploy that matters for this slice is the source tree
itself, already current via the merge.

Several small, deliberate gaps documented in `internal/pylib/aitest/README.md`
(-x/--maxfail silently ignored, pytest_warning_recorded not forwarded, log_cli
worker lines dropped, look-ahead dispatch deferred, record_xml_attribute/
--setup-show lost in a worker) -- same class of limitation xdist workers
already have, not new regressions.

Live dogfood finding, unrelated to correctness, filed separately as AIRA-51:
the "waiting for admission" progress line can show a reserve estimate that
disagrees with what's actually granted, and a no-history argv signature can
queue 20+ minutes even when the suite's real footprint is small.

relates AIRA-30, AIRA-51.
