# AIRA-31: aitest Slice 2 — JUnit XML, coverage combine, TestReport replay — Implementation Plan (v7)

**v6/v7 changelog:** Two independent lineages' third-round confirmations of v5's fd-capture fix, both PASS WITH FIXES, no new P0s, converging on largely the same remaining gaps. Sol (v6): `default=repr` was wrong for non-JSON `user_properties` — pytest's real junitxml applies `str()`, not `repr()`, to property values, so `repr()` would have silently produced different XML for any object whose `__str__`/`__repr__` differ — changed to `default=str`; and mandated (not left optional) that the final synthesis pass iterate `self.items_by_nodeid`, not `self.results`'s own keys, since only the former can catch a nodeid missing from `self.results` entirely. Fable (v7), independently confirming the same `items_by_nodeid` requirement: found the plan's child-init placement hint ("right after `place_self` returns") pointed at `fork_worker`, used ONLY by the confined `spawn_worker` path — `_spawn_fallback_worker` never calls it, and is exactly the path Fable's own live probe used to first find the bug, so the hint as written would have left fallback workers still contaminated; corrected to one helper at the top of `run_worker_loop`, the single real convergence point for both fork sites (confirmed against the actual call sites). Also added: `start`/`stop` timestamps on the synthesized report (`--durations`/junit `time=` read them directly). Fable's mechanism re-verification (a full line-by-line trace against real installed pytest `capture.py` source, since its own live re-probe was blocked by admission contention on this machine — noted as its own dogfood-friction finding) found the fd-capture fix design itself fully correct and complete as written, no changes needed to the sequence itself.

**v4 changelog:** Sol's round-2 confirmation of v3 rendered PASS WITH FIXES (no more BLOCKs — the coverage-ownership-free design and crash-atomic staging were both confirmed correct against real source and a live probe). Five precise fixes applied: (1) the tuple-tagging codec's marker was not collision-safe against a real dict that happens to be shaped like the marker — fixed with a proper escaping scheme, and now applied to whole events (including `logstart`/`logfinish` locations), not just report `data`; (2) the `{`-prefix wire discriminator is ambiguous against a legally-`{`-prefixed nodeid — replaced with an explicit sentinel; (3) every staged event's own nodeid must match `in_flight`, not just the plain line's; (4) the synthesized-unevaluated-report fix only covered the twice-crashed case — `_fail_queue_too_large`'s separate unevaluated-marking path needed the same treatment, via one centralized helper; (5) the synthesized report must use `outcome="failed"` with an honest message, since a literal `outcome="unevaluated"` is not a real pytest outcome and junitxml silently ignores it — the original design would not actually have worked. Also added a real `pytest --cov` regression test (a spy test alone can't prove the pytest-cov integration) and a `junit_logging` config note for the e2e test.

**v5 changelog — a genuinely new, previously-undiscovered P0, live-verified, plus precision fixes:** Fable's confirmation review of v3 found something no prior round caught: pytest's own fd-capture mechanism (`FDCaptureBase`) creates ONE `TemporaryFile` per stream (stdout/stderr) in the PARENT process at session start and `dup2`s it onto real fd 1/2. `os.fork()` shares the underlying open file description — INCLUDING its current offset — across every forked worker, so every worker's captured output writes into the SAME shared file at whatever position sibling workers have left it. Live-verified with the real plugin, 4 concurrent workers each printing 20 tagged lines: one test's captured output contained 73 lines (only 18 its own, 55 stolen from siblings), another captured zero of its own. This would have shipped SILENTLY WRONG `<system-out>`/`<system-err>` content and "Captured stdout" sections attributed to the wrong test — not a crash, wrong data, exactly the failure class this whole feature exists to prevent, and invisible in Slice 1 only because Slice 1 discards captured output entirely. Fixed in Task 1 (below) by re-initializing per-process capture in the child immediately after fork, before any test runs — using the specific, verified-safe capturemanager sequence Fable's own live probe confirmed does not steal a sibling's in-flight output. Also fixed: a coverage-save guard (only save when the active instance's own `run:parallel` option is true — Fable verified a non-parallel setup lets the child's save DESTROY the parent's already-flushed data by writing to the identical filename); `json.dumps(..., default=repr)` so a non-JSON-serializable `user_properties` value can never crash a worker mid-test; `_exit_child` widened to `except BaseException`/`finally` so it always actually exits even if `coverage.save()` itself raises something unusual; and the unevaluated-report synthesis redesigned from "hunt down and centralize every call site that can produce one" (v4's approach, which Fable found still missed a third site) to a single, structurally complete final pass after `supervisor.run()` returns — synthesize for every collected item with no replayed report, tracked via one "replayed" set, regardless of which code path left it that way.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give aitest full xdist-equivalent output fidelity — real JUnit XML, real per-test terminal progress, real coverage combine — by streaming each worker's real pytest `TestReport` objects back to the supervisor and replaying them into the supervisor's own real pytest hooks, faithfully enough that a real assertion failure's traceback, captured output, and skip/xfail reason all survive the round-trip, and without ever silently duplicating or dropping a result.

**Architecture:** Same shape as v2 (JSON-prefixed event lines staged per in-flight nodeid, replayed only once that nodeid's existing plain-text result line is confirmed, discarded on crash) — but v3 fixes two things v2 got wrong and simplifies a third that both reviewers converged on as over-engineered:

1. **JSON does not preserve pytest's tuple fields.** `pytest_report_to_serializable`'s output has tuple-typed fields (`longrepr`, `location`, `sections` entries) that plain `json.dumps`/`json.loads` silently turns into lists — and `pytest_report_from_serializable` does NOT restore them, because that restoration only exists on execnet's own typed wire format (which xdist uses), not on bare JSON. junitxml and terminalreporter both `assert isinstance(report.longrepr, tuple)` internally — replaying an untagged round-trip crashes the whole run on the first real skip or failure. v3 tags tuples explicitly through the JSON boundary (a small wrapper, verified live — see Task 1).
2. **A synthesized report for the terminal "unevaluated" outcome.** v2's crash-atomicity fix (stage, discard-on-crash) correctly stops a retried attempt's events from being replayed twice — but it does not, on its own, make the FIRST crashed attempt's outcome visible anywhere. If a nodeid crashes twice (Slice 1's existing requeue-once exhausted) and both attempts' staged batches are discarded, junitxml ends up with NO `<testcase>` element for that nodeid at all — a real test run, silently missing from the report. v3 synthesizes one honest report through the same replay path for this terminal case (mirroring xdist's own `handle_crashitem` precedent), never a silent gap.
3. **Coverage: don't bootstrap or own it — just make sure it survives the fork and the `os._exit`.** v1/v2 both tried to originate a coverage config and drive `process_startup()`/`combine()` from inside aitest. Both reviewers found this either doesn't work for a forked worker at all (v1) or actively conflicts with `pytest-cov`'s own already-running collector when `--cov` is in play (v2). v3 does neither: aitest makes NO assumption about how (or whether) coverage was started, and takes on exactly one small, narrow responsibility — in every path where a forked worker process is about to call `os._exit()`, if `coverage.Coverage.current()` is non-`None` (something, whatever it was, is measuring in this process), stop and save it first, since `os._exit()` is the one thing guaranteed to skip coverage's own normal atexit save. Whoever started coverage (`pytest-cov`, a bare `coverage run`, or nothing at all) remains entirely responsible for combining — aitest never calls `combine()` itself.

**What changed, across review rounds — four real, distinct P0s found and fixed:**

- **v1** relied on bare `COVERAGE_PROCESS_START`/`.pth` auto-start alone (doesn't fire for a forked, not exec'd, worker) and replayed events as they arrived (not crash-atomic). Sol/Codex: BLOCK on both.
- **v2** fixed crash-atomicity via staging (Sol's round-2 confirmation: "the original crash/retry double-count race is closed" — this specific mechanism is UNCHANGED in v3, it was correct) and fixed the raw fork-measurement gap via coverage.py's own `patch = fork, _exit` config directive (verified live: a forked child under that config correctly produces valid coverage data, confirmed by both this session's own probe AND, independently, Sol's). But Sol's round-2 review still found a NEW P0: calling `coverage.process_startup()` unconditionally from `pytest_runtestloop` can start a SECOND, conflicting `Coverage` instance alongside whatever `pytest-cov` itself already started for `--cov`, with incompatible measurement settings risking silently-incomplete combined data. Fable, independently reviewing v1's design (its review landed after v2 was already drafted, but its findings apply to the coverage approach generally, not just v1's specific bare-env-var version), found the SAME class of problem from source: `pytest-cov`'s own `pytest_runtestloop` wrapper calls `combine()` (with `keep=False`, i.e. it DELETES the per-worker data files) after aitest's own loop returns — so aitest calling combine first, as v1/v2 both did, would race pytest-cov's own cleanup and could destroy data out from under it. Fable also independently confirmed the fork/`.pth` gap and — critically — found that even a NAIVE `patch=fork,_exit`-free child-side `Coverage.current().stop().save()` (no config origination at all) is coverage.py's OWN documented, simpler answer (`sqldata.py`'s "Looks like we forked!" auto-detection already handles the data-file identity problem once the child explicitly saves). v3 adopts this simpler, config-free, ownership-free design.
- **Fable also found the tuple-serialization bug independently, via a live probe** (a real skipped test's `longrepr` round-tripped through JSON came back as a list; `junitxml.py`'s `append_skipped` immediately `assert isinstance(report.longrepr, tuple)`-crashed on it) and the missing-synthesized-report-for-unevaluated gap, both fixed in this v3.
- **Sol's round-2 review, confirming v2's crash-atomicity fix, added one more precise requirement**: validate/deserialize the ENTIRE staged event batch before replaying the first real hook call, not dispatch-and-replay-as-you-go through the batch — otherwise a malformed LATER event in an otherwise-valid batch could leave earlier events already replayed with no way to un-replay them. v3's Task 2 incorporates this.
- Fable additionally flagged (not blocking, but must be handled, not ignored): the forked child's COW-inherited `terminalreporter` plugin is still registered and would print its own per-test progress line directly to the shared terminal — combined with the supervisor's own replay-driven `terminalreporter` output, every test's progress would print TWICE. v3's Task 1 unregisters it in the child. Also: the existing Go-side end-to-end test (`internal/pylib/pytest_aitest_e2e_test.go`) asserts on Slice 1's plain `"<nodeid> <outcome>"` terminal lines and `"0 unevaluated"` text — v3's Task 4 explicitly decides what happens to those assertions now that real terminalreporter output exists too, rather than leaving it for an implementer to discover as a surprise test failure.

**Verified design basis:** `pytest_report_to_serializable`/`pytest_report_from_serializable` are core pytest hooks (verified against real installed source; cite hook NAMES in code comments, never line numbers, which drift by version and were wrong in more than one of this plan's own earlier drafts). Deserialization on xdist's controller side happens in `xdist/workermanage.py`'s `process_from_remote`, not `dsession.py` (which only replays the already-deserialized report). xdist also forwards `pytest_runtest_logstart`/`pytest_runtest_logfinish` separately — this plan forwards them too.

**Spec:** `docs/superpowers/specs/2026-09-01-aitest-design.md` §3.2 and §5 Slice 2.

## Global Constraints

- No cgo; pure Python, unaffected on the Go side.
- Every heavy command MUST be run via `aira confine --`.
- Correctness-critical work: full two-loop before merge. Silently-wrong test reporting is worse than a crash.
- Do NOT change the existing plain "nodeid outcome[recycle_suffix]" result line's wire format or handling.
- Do NOT replay ANY staged event into the supervisor's real pytest hooks before (a) the whole batch for that nodeid has been successfully parsed/deserialized in full, AND (b) that nodeid's plain result line has been confirmed received. Both conditions, not either alone.
- Do NOT round-trip a `TestReport`'s serialized form through bare `json.dumps`/`json.loads` without the tuple-tagging wrapper (Task 1) — this WILL crash on any real skip/failure/xfail, verified.
- Do NOT have aitest originate a coverage config, call `coverage.process_startup()`, or call `coverage combine()`/`Coverage().combine()` anywhere. Its only coverage-related responsibility is: in a forked worker about to `os._exit()`, save whatever `coverage.Coverage.current()` already is, if it is not `None`.
- Do NOT add a `pytest-xdist` dependency.

---

## File Structure

- `internal/pylib/aitest/worker.py` — tuple-tagging JSON codec; extend report/event capture; unregister `terminalreporter` in the forked child before running any test; centralize the child's `os._exit()` path through one helper that saves active coverage first.
- `internal/pylib/aitest/supervisor.py` — stage events per in-flight nodeid; validate/deserialize the whole batch before replaying any of it; replay only once the result line confirms; on the terminal unevaluated outcome (second crash), synthesize and replay one honest report instead of leaving the nodeid unreported.
- `internal/pylib/aitest/__init__.py` — no coverage-related changes at all in v3 (the whole point of the simplification); still needs `Supervisor` threaded a `pytest.Config`/hook-caller reference, as in v2.
- Tests: `test_worker.py`, `test_supervisor.py`, plus the end-to-end layer — explicitly reconciled with Slice 1's existing Go-level plain-output assertions in Task 4.

## Task 1: Worker side — tuple-safe serialization, terminalreporter suppression, centralized coverage-safe exit

**Files:**
- Modify: `internal/pylib/aitest/worker.py`
- Test: `internal/pylib/aitest/test_worker.py`

**Interfaces:**
- Produces: `run_one` returns `(outcome, events)` as before; a new small tuple-tagging codec (`_tag_tuples`/`_untag_tuples`, or equivalent naming — implementer's choice) used when building the JSON payload for each event; a new `_exit_child(code)` helper replacing every bare `os._exit(...)` call in the forked-child code path (worker.py:123, and — since this task owns the shared helper — supervisor.py's four sites at lines ~527/528/605/606 should import and use it too; coordinate with Task 2 on this, it is a worker.py-owned helper regardless of which file's code calls it).

- [ ] **Step 1: Write the failing tests first**

```python
def test_json_tuple_codec_round_trips_a_real_serialized_skip_report_with_isinstance_tuple_intact(pytester):
    pytester.makepyfile("""
        import pytest
        def test_it():
            pytest.skip("because")
    """)
    # ... collect the real item via this file's existing pytester convention ...
    outcome, events = run_one(item)
    assert outcome == "skipped"
    report_event = next(e for e in events if e["kind"] == "report" and e["data"] is not None and e["data"].get("when") == "call")

    wire = json.loads(json.dumps(_tag_tuples(report_event["data"])))
    restored_data = _untag_tuples(wire)
    replayed = item.config.hook.pytest_report_from_serializable(config=item.config, data=restored_data)

    assert isinstance(replayed.longrepr, tuple)
    # This is the exact assertion junitxml.py's append_skipped makes internally
    # (verified against real installed pytest source) -- if this passes, a
    # real junitxml plugin run against this replayed report will not crash.


def test_run_one_serialized_reports_preserve_a_real_failures_traceback_and_captured_output(pytester):
    # Same as v2's Task 1 test -- a real assertion failure with custom
    # message, stdout marker, stderr marker -- but now round-tripped
    # through the REAL tag/untag codec end to end (not the bare dict),
    # asserting replayed.longreprtext / capstdout / capstderr as before.


def test_run_worker_loop_unregisters_terminalreporter_before_running_any_test(pytester, capsys):
    # Fable's finding: the forked child's COW-inherited terminalreporter
    # would otherwise print its own progress line directly to the shared
    # terminal, duplicating the supervisor's own replay-driven output.
    # Run a real test through the worker loop in-process (this file's
    # existing convention) and assert pytest's OWN default per-test
    # progress marker (whatever terminalreporter would normally emit)
    # does NOT appear in captured stdout/stderr from the worker side.


def test_exit_child_saves_active_coverage_before_exiting(monkeypatch):
    # Fake coverage.Coverage.current() to return a spy object; call
    # _exit_child via a monkeypatched os._exit that raises SystemExit
    # instead of actually exiting (so the test can observe what happened
    # before it); assert spy.stop() and spy.save() were both called,
    # in that order, before the (faked) exit.


def test_exit_child_exits_normally_when_no_coverage_is_active(monkeypatch):
    # coverage.Coverage.current() returns None (or coverage isn't
    # importable at all -- handle ImportError too, since this must work
    # in a project that never installed coverage.py) -- assert it exits
    # cleanly with zero errors, no crash from a missing coverage module.


def test_exit_child_saving_a_real_pytest_cov_active_instance_does_not_disturb_the_parents_own_collector(pytester):
    # Sol round-2, required: a spy-based test alone cannot prove this --
    # it needs pytest-cov actually active. Run a real pytest sub-process
    # (pytester or a real subprocess invocation, whichever this project's
    # coverage/pytest-cov test conventions elsewhere already establish --
    # if none exist yet, this is the first) with `--cov` AND
    # `--aitest-workers=2` against a small real fixture suite, and assert:
    # (a) the run completes without error, (b) the PARENT process's own
    # pytest-cov-owned Coverage instance still completes its own combine
    # normally afterward (its usual `.coverage`-shaped output exists and
    # is non-empty), (c) lines exercised ONLY inside a forked worker
    # (not the supervisor/collection phase) appear as covered in the
    # final combined data -- proving the child's stop()/save() on the
    # COW-shared pytest-cov Coverage object correctly mutated only the
    # forked copy, per Sol's source-verified finding, without this
    # plan's own docs merely asserting that verification happened
    # somewhere else.
    #
    # Document explicitly, in this task's code comments (not left
    # implicit): a project using a bare `coverage run -m pytest` (no
    # pytest-cov, no parallel-mode config) rather than `--cov` gets NO
    # correctness guarantee from this mechanism on its own -- without
    # coverage's own parallel-mode / suffixed-data-file setting, the
    # child and parent can both write to the SAME data file and overwrite
    # each other. aitest deliberately owns none of that configuration
    # (see this task's "own nothing" design) -- a project wanting correct
    # coverage across aitest workers is responsible for its OWN
    # parallel-mode-aware coverage config, exactly as it already would be
    # for xdist.


def test_exit_child_still_exits_if_coverage_save_itself_raises(monkeypatch):
    # A spy whose .save() raises -- assert _exit_child still calls the
    # real os._exit with the requested code regardless (best-effort save,
    # must NEVER prevent the process from actually exiting).
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Tuple-tagging codec — **collision-safe (Sol round-2 fix)**: the plan's first draft used a bare `{"__tuple__": True, "items": [...]}` marker, which is NOT injective — a real report field (a plugin-added `user_properties` entry, e.g.) that happens to be a dict shaped exactly like that marker would be silently misinterpreted as a tuple on decode. The corrected version below ESCAPES any real dict that already contains a marker key, so the mapping is truly one-to-one in both directions, verified live (round-tripping both an ordinary nested tuple/list/dict structure AND a deliberately adversarial dict containing a real key equal to the marker, confirming the adversarial case decodes back to the exact same real dict, not a tuple):

```python
_TUPLE_MARKER = "__aitest_tuple__"
_ESCAPED_MARKER = "__aitest_escaped__"


def _tag_tuples(obj):
    if isinstance(obj, tuple):
        return {_TUPLE_MARKER: [_tag_tuples(x) for x in obj]}
    if isinstance(obj, list):
        return [_tag_tuples(x) for x in obj]
    if isinstance(obj, dict):
        tagged = {k: _tag_tuples(v) for k, v in obj.items()}
        if _TUPLE_MARKER in obj or _ESCAPED_MARKER in obj:
            # A REAL dict that happens to already look like one of our
            # markers must be escaped, or decoding it would silently turn
            # it into a tuple (or unwrap a fake "escape") instead of the
            # real dict it actually is. Wrapping is recursive-safe: an
            # already-escaped dict containing another marker-shaped dict
            # gets escaped again, one layer per occurrence, and _untag_tuples
            # peels exactly one layer per marker it encounters.
            return {_ESCAPED_MARKER: tagged}
        return tagged
    return obj


def _untag_tuples(obj):
    if isinstance(obj, dict):
        if _ESCAPED_MARKER in obj and len(obj) == 1:
            return {k: _untag_tuples(v) for k, v in obj[_ESCAPED_MARKER].items()}
        if _TUPLE_MARKER in obj and len(obj) == 1:
            return tuple(_untag_tuples(x) for x in obj[_TUPLE_MARKER])
        return {k: _untag_tuples(v) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_untag_tuples(x) for x in obj]
    return obj
```

Apply `_tag_tuples`/`_untag_tuples` to the **whole event dict**, not just a report event's `data` payload — `logstart`/`logfinish` events carry their own `location` tuples (Sol round-2: "preserving logstart/logfinish locations") and need the identical treatment, not just report events.

`run_worker_loop` writes each event line with the explicit `_EVENT_LINE_PREFIX` sentinel defined in Task 2 (do not use a bare `"{"`-starts-with check anywhere — Sol round-2 found this is ambiguous against a legal pytest nodeid), followed by `json.dumps(_tag_tuples(event), default=str)`, one per line, before the existing unchanged plain result line. The `default=str` fallback is required, not optional (v5, Fable's finding, verified live; `str` not `repr` per Sol round-3 — pytest's real installed junitxml plugin applies `str(propvalue)` to a property value, not `repr(propvalue)`, so `default=repr` would silently produce DIFFERENT XML output than a native, non-aitest run for any object whose `__str__`/`__repr__` differ): a test that calls `record_property("x", <some non-JSON-serializable object>)` produces a `user_properties` value `json.dumps` would otherwise raise `TypeError` on — inside the child's own hookimpl, which propagates out through `run_one`'s broad `except BaseException: _exit_child(70)` and crashes the WORKER (not just that one event), turning a genuinely PASSING test into "unevaluated" after the retry also crashes the same way. Falling back to `str()` here matches junitxml's own native behavior exactly, rather than merely being "a safe fallback".

`_exit_child(code)`:

```python
def _exit_child(code):
    try:
        import coverage
        current = coverage.Coverage.current()
        if current is not None:
            current.stop()
            if current.get_option("run:parallel"):
                current.save()
    except BaseException:
        # Best-effort only -- this must never prevent the process from
        # exiting. BaseException (v5, Fable's finding), not Exception: this
        # is called from except BaseException handlers whose entire point
        # is never falling through into the child's COW copy of the
        # supervisor's own control flow -- a KeyboardInterrupt/SystemExit/
        # MemoryError raised inside coverage's own save() must not escape
        # this function and defeat that.
        pass
    finally:
        os._exit(code)
```

Replace `worker.py:123`'s `os._exit(70)` with `_exit_child(70)`. Coordinate with Task 2 to replace `supervisor.py`'s four analogous sites the same way (import `_exit_child` from `aitest.worker`, matching how `_RECYCLE_SUFFIX`/`fork_worker`/`run_worker_loop` are already imported there).

**Child-init location — corrected, required (v6, Fable round-3 FIX 1):** the plan previously hinted this belongs "right after `place_self` returns in the child" — but `place_self`/`fork_worker` are used ONLY by `spawn_worker`'s confined path (`supervisor.py:505`). `_spawn_fallback_worker` (`supervisor.py:582`) is a SEPARATE, bare `os.fork()` site that never calls `fork_worker`/`place_self` at all — and it is exactly the path Fable's own live probe used to first discover the fd-capture bug (4 fallback workers). Following the old hint literally would leave fallback workers still contaminated, i.e. would NOT fix the exact scenario that revealed the bug. **Both `spawn_worker` (line 525) and `_spawn_fallback_worker` (line 603) call `run_worker_loop` — confirmed the single real convergence point.** Implement ALL child-init (terminalreporter unregistration AND fd-capture re-init) as one helper called at the very TOP of `run_worker_loop` (`worker.py:198`, before its `for line in pipe_in:` loop), not tied to either fork site individually:

```python
def _init_forked_child(config):
    pm = config.pluginmanager
    terminalreporter = pm.get_plugin("terminalreporter")
    if terminalreporter is not None:
        pm.unregister(terminalreporter)
    capman = pm.get_plugin("capturemanager")
    if capman is not None:
        old = capman._global_capturing
        if old is not None:
            capman._global_capturing = None
            old.stop_capturing()
        capman.start_global_capturing()
        capman.suspend_global_capture()
```

Run this **unconditionally, every time** — do NOT gate it on the active capture method being `"fd"` specifically (Fable round-3: `sys`/`tee-sys`/`no` capture modes are unaffected by the underlying bug since they use in-memory, COW-private buffers rather than a shared fd-backed file, but the sequence above is a correct no-op-equivalent for those modes too via their own `stop_capturing()`/`start_global_capturing()` machinery — an implementer "optimizing" this into a mode-conditional branch would add risk for no benefit). Call it once, at the top of `run_worker_loop`, obtaining `config` from `next(iter(items_by_nodeid.values())).config` (or thread it through explicitly if that reads more cleanly given the surrounding code — implementer's choice) — the in-process `run_worker_loop` tests then cover BOTH fork sites structurally, by construction, rather than needing two separate near-duplicate test setups.

Do NOT use `capman.stop_global_capturing()` (the public method) for this — Fable's own probe found its `pop_outerr_to_orig()` snaps the SHARED (still fork-inherited) capture file, which would steal whatever output sibling workers have already written into it mid-test (a real, live risk: `_replace_worker` forks a NEW worker while OTHER workers are still mid-test). The sequence above discards the fork-inherited (shared, corrupt-by-construction) capture object without reading it, then has `start_global_capturing()` create genuinely fresh, per-process capture files, which is what actually fixes the cross-contamination — verified live (4 concurrent workers each printing 20 tagged lines; without this fix, one test's captured output contained 55 lines stolen from siblings; with it, each worker's own capture is isolated). `--capture=sys`/`tee-sys`/`no` are unaffected by this bug in the first place (in-memory per-process buffers, not shared fd-backed files) — this fix is specifically for the (pytest's own) default fd-capture mode.

**Coverage-save guard (v5, Fable's finding):** in `_exit_child`, only call `.save()` (after `.stop()`) when the active `Coverage` instance's own `run:parallel` option is true (`current.get_option("run:parallel")` — coverage sets this automatically whenever a real `data_suffix` is in play, which is always true for `pytest-cov` and for a project's own explicit `parallel = true`). Skip the save (do nothing beyond `.stop()`, or skip entirely — implementer's judgment, document whichever) when it is false: a non-parallel setup (bare `coverage run` with no `parallel`/suffix config) would otherwise make the child choose the SAME data filename as the parent, and the child's own first-use `erase()` would destroy whatever the parent had already flushed — verified live (a measurable coverage regression, not just wasted work).

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): tuple-safe report serialization, coverage-safe child exit, terminal suppression (AIRA-31)"`

## Task 2: Supervisor side — validate-then-stage-then-replay, synthesized report for the unevaluated case

**Files:**
- Modify: `internal/pylib/aitest/supervisor.py`
- Test: `internal/pylib/aitest/test_supervisor.py`

**Interfaces:**
- Consumes: Task 1's `_untag_tuples`, `_exit_child`. Needs supervisor-level `pytest.Config`/hook-caller access, threaded through as in prior plan drafts.
- Produces: real `pytest_runtest_logreport`/`logstart`/`logfinish` calls, fired only once per test, only after (a) the WHOLE staged batch for that nodeid parses/deserializes successfully AND every event's own nodeid matches `state["in_flight"]` (Sol round-2 — not just the plain line's nodeid, EVERY staged event's nodeid must match; a well-formed event for the WRONG nodeid, e.g. from the AIRA-40-class inherited-fd surface, must go through the same crash path as a malformed event) and (b) the plain result line for that same nodeid is confirmed; PLUS one structurally-complete final pass in `run()` (v5's redesign, see below) that synthesizes and replays an honest report for every nodeid that ends the whole run still `"unevaluated"` and was never actually replayed — covering every current and future way that can happen, not a per-site list.

**Wire-format discriminator fix (Sol round-2):** the prior draft distinguished a JSON event line from the existing plain result line by checking `line.startswith("{")`. A pytest nodeid CAN legally contain `{` as its first character in principle (an unusual but valid parametrize ID), so this is ambiguous. Use an explicit, unambiguous magic prefix instead — e.g. a fixed sentinel `_EVENT_LINE_PREFIX = "\x01"` (or any byte/short string this project's nodeid grammar can never legally start with — verify against pytest's actual nodeid character rules rather than assuming, and pick accordingly) prepended to every JSON event line on the worker side and checked for explicitly (not inferred from JSON-parseability) on the supervisor side; the existing plain result line is emitted with no prefix at all, exactly as today.

- [ ] **Step 1: Write the failing tests first**

```python
def test_drain_worker_validates_the_whole_batch_before_replaying_any_of_it(...):
    # Sol round-2's precise requirement. Fake pipe: a VALID logstart, a
    # VALID report event, then a MALFORMED/unrecognized-kind event, then
    # the plain result line. Assert: the spy plugin received ZERO
    # replayed events (not even the two valid ones that came before the
    # malformed one) -- validation of the ENTIRE batch happens before any
    # real hook is called, not dispatch-as-you-parse.


def test_drain_worker_stages_and_replays_a_fully_valid_batch_only_after_the_result_line(...):
    # Same as v2's positive case: logstart, 3x report, logfinish, THEN
    # the plain result line -- assert no replay before the result line,
    # full ordered replay once it arrives, then self.results[nodeid] set
    # exactly as today.


def test_drain_worker_discards_staged_events_on_a_crash_before_the_result_line(...):
    # v2's critical regression test, unchanged in intent: crash before
    # the result line -> zero replay, existing requeue-once path fires,
    # a later successful retry's OWN events replay exactly once.


def test_run_synthesizes_and_replays_an_honest_report_for_a_twice_crashed_nodeid(...):
    # Fable's original finding, Sol round-2's report-shape correction, v5's
    # redesign to a post-run final pass. Simulate the existing Slice 1
    # requeue-once path exhausting: crash on attempt 1 (discard staged
    # events, requeue), crash again on attempt 2 (no more retries --
    # existing code sets self.results[nodeid] = "unevaluated", unchanged).
    # Run the WHOLE supervisor.run() to completion (not just _drain_worker
    # in isolation, since the synthesis now happens in run()'s own final
    # pass) and assert exactly ONE real report was replayed via the spy
    # plugin for that nodeid -- not zero (Fable's original gap), not two
    # (double-counting either crashed attempt) -- with outcome == "failed"
    # (NOT the literal string "unevaluated", which junitxml silently
    # ignores) and a longrepr string that plainly says this was never
    # actually observed to pass or fail. Also assert a logstart/logfinish
    # pair fired for it.


def test_run_synthesizes_a_report_for_every_never_dispatched_nodeid_after_fail_queue_too_large(...):
    # v5: the SAME post-run final pass (not a separate per-site helper)
    # must cover _fail_queue_too_large's own unevaluated-marking (nodes
    # still queued, never even dispatched, after a permanent daemon
    # sizing rejection) -- run a scenario that triggers it mid-run, let
    # run() complete, and assert the spy plugin received exactly one
    # synthesized outcome="failed" report per affected nodeid.


def test_run_synthesizes_a_report_even_for_a_result_defaulted_by_init_pys_own_fallback(...):
    # v5, Fable's third-site finding: whatever produces __init__.py's own
    # results.get(nodeid, "unevaluated") default must ALSO be covered --
    # this is exactly why v5 moved to a single post-run pass over every
    # collected nodeid rather than chasing individual call sites. Find
    # this specific fallback's real trigger condition before writing the
    # test (do not guess it), reproduce it, and assert the same one-report
    # guarantee holds.


def test_drain_worker_still_handles_the_existing_result_line_format_unmodified(...):
    # Re-confirm every existing Slice 1 wire-format test still passes
    # verbatim.
```

- [ ] **Step 2: Run to verify failure**

- [ ] **Step 3: Implement**

Extend `_drain_worker`'s event-line branch (matched via `_EVENT_LINE_PREFIX`, never a bare `"{"` check): append the raw parsed-but-not-yet-untagged event to `state["pending_events"]` (reset at dispatch time, per v2's design — find the exact `state["in_flight"] = nodeid` site). Do NOT call `_replay_event` here. **An event line arriving while `state["in_flight"] is None`, or a `json.loads` failure on an event line at receive time, must ALSO take the crash path immediately (v5, Fable's precision fix) — never staged silently for later, even though the batch-validation step below would eventually catch a staged-but-invalid entry; catching it as early as possible keeps the crash-diagnostic message accurate about WHERE things went wrong.** At the point the existing code currently does `self.results[nodeid] = outcome` (after the plain line's `nodeid == state["in_flight"]` check passes): first validate/deserialize the ENTIRE `state["pending_events"]` batch (untag tuples, resolve each `"kind"`, call `pytest_report_from_serializable` for report events, confirm each report's own `"$report_type" == "TestReport"` marker — v5, Fable's finding: pytest's own serialization includes this, checking it catches a malformed/foreign payload earlier and more precisely than waiting for a downstream `AttributeError` — and confirm EVERY event's own carried nodeid — not just the plain line's — equals `state["in_flight"]`) into a fully-materialized list — if ANY event in the batch fails this step, treat it exactly like the existing malformed-line crash path (log, `_handle_worker_exit`, no replay of anything from this batch) — only once the WHOLE batch is confirmed valid, replay each materialized item in order via the real hook calls (adding each report's nodeid to `self._replayed_nodeids`, see below), THEN clear `state["pending_events"]`, THEN proceed with the existing unchanged `self.results[nodeid] = outcome` / recycle / retire logic.

**One structurally complete final pass, not per-site centralization (v5 — Fable round-2 found v4's "hunt down every call site" approach still missed a third site: the `results.get(nodeid, "unevaluated")` default read elsewhere in `__init__.py`, beyond the two v4 already found in `_handle_worker_exit` and `_fail_queue_too_large`).** Rather than continuing to hunt for every current AND future place that could leave a nodeid unevaluated, track which nodeids actually had a real report replayed (a `self._replayed_nodeids = set()`, added to whenever `_replay_event` fires a real `"report"`-kind hook call — NOT for synthesized ones, to avoid a synthesized report making a nodeid look "replayed" to this same check). After `run()`'s existing dispatch loop completes (immediately before `run()` returns `self.results`, wherever that currently is — do not disturb the existing loop's own logic), do ONE pass — **iterating over `self.items_by_nodeid` (the full universe of collected items — Sol round-3, mandatory, not a choice: iterating `self.results`'s own keys instead cannot possibly catch a nodeid that's MISSING from `self.results` entirely, which is exactly the `results.get(nodeid, "unevaluated")`-default case that motivated this whole redesign in the first place — that gap would survive completely intact if this pass iterates the wrong collection)** — for each nodeid, read `self.results.get(nodeid, "unevaluated")`; if that is `"unevaluated"` and the nodeid is NOT in `self._replayed_nodeids`: synthesize and replay one honest `outcome="failed"` report for it (see Report shape below; if a caller-supplied reason string is available for this specific nodeid — the crash message, or `_fail_queue_too_large`'s own daemon-rejection reason — preserve it via a small `self._unevaluated_reasons = {}` dict populated at each production site, so the synthesized message can be specific rather than generic), then add it to `self._replayed_nodeids`. This single site is complete by construction regardless of how many distinct code paths can produce "unevaluated" — current (`_handle_worker_exit`, `_fail_queue_too_large`, the `__init__.py` default) or future.

Also fire the corresponding `logstart`/`logfinish` hooks for each synthesized nodeid (Fable: terminal `-v`/`[100%]` progress depends on these too, not just the final report), using the real collected `Item`'s own `nodeid`/`location`/`keywords` (the supervisor already holds these from `collect()`).

**Report shape (Sol round-2 — the previous draft's plan was underspecified here and would not actually have worked):** pytest's `TestReport.outcome` is only ever meaningfully one of `"passed"/"failed"/"skipped"` to junitxml/terminalreporter — a literal `outcome="unevaluated"` is not a real pytest outcome and is simply IGNORED by junitxml's own rendering (silently, which is exactly the failure mode this fix exists to prevent). Synthesize the report as `outcome="failed"`, `when="call"` (or a clearly-synthetic non-call phase if that renders more honestly in this project's actual junitxml output — verify against real output, implementer's call), with a `longrepr` string that says plainly this was never actually observed to pass or fail (e.g. `"unevaluated: %s" % reason`, where `reason` is whatever the caller already has — the crash message, or `_fail_queue_too_large`'s own daemon-rejection reason) — mirroring xdist's own precedent for exactly this situation (`handle_crashitem`: a synthetic failure report, not a fabricated pass, whenever a real report can never arrive). Construct it directly as a Python object matching whatever fields `pytest_runtest_logreport`/junitxml actually read (verify against real installed pytest source which fields are load-bearing, do not guess) — it never came from a worker pipe, so there's no need to round-trip it through the JSON codec at all. **Set `start`/`stop` explicitly on the constructed `TestReport` (v6, Fable round-3 FIX 2 — `TestReport.__init__` accepts them) — e.g. both equal to the synthesis moment, or `start` at original dispatch time if that's cheaply available — rather than leaving them at whatever zero/default value the constructor would otherwise use, since `--durations` and junit's own `time=` attribute both read these fields directly and a garbage/zero value there is a small but real, easily-avoided fidelity gap.**

- [ ] **Step 4: Run to verify pass**

- [ ] **Step 5: Commit** — `git commit -m "feat(aitest): validate-then-replay staged reports, synthesize an honest report for the unevaluated case (AIRA-31)"`

## Task 3: End-to-end verification, including the Go-side test reconciliation

**Files:**
- New or extended real-pytest-driven test (`pytester`) for JUnit XML fidelity.
- `internal/pylib/pytest_aitest_e2e_test.go` — Fable's finding: this file's existing assertions on Slice 1's plain `"<nodeid> <outcome>"` terminal lines and `"0 unevaluated"` text need an explicit decision now that real `terminalreporter` output exists too (via replay) alongside them.

**Interfaces:**
- Consumes: everything from Tasks 1-2.

- [ ] **Step 1: Decide the Go e2e test's fate, explicitly, before touching anything else**

Read `internal/pylib/pytest_aitest_e2e_test.go`'s exact assertions on Slice 1's plain-text lines. Decide (and document the decision in this task, do not leave it implicit): does Slice 1's own `print("%s %s" % (nodeid, outcome))`-style terminal output (if any remains in `__init__.py`/`supervisor.py` after Tasks 1-2) stay for a machine-parseable summary independent of full terminalreporter fidelity, or does it get removed now that real terminalreporter output covers the same information more completely? Whichever is chosen, update the Go test's assertions to match reality rather than leaving them to fail as a surprise. (Leaning toward: keep Slice 1's plain lines as a cheap, terminalreporter-independent sanity signal the Go e2e layer can keep depending on, and treat real terminalreporter/junitxml output as strictly additive — but this is not settled here on purpose; it needs an explicit implementer decision informed by actually running both and looking at the output, not a guess made in this planning document.)

- [ ] **Step 2: Write the failing end-to-end test first**

A fixture suite: a passing test, a skipped test, an xfail, a real assertion failure with a custom message printing to both stdout and stderr, and a setup-error case — **plus at least one test where several workers print CONCURRENTLY-overlapping chatty output** (v5: this is the direct end-to-end regression case for Fable's fd-capture P0 — without it, Task 3's fixture is exactly the "cannot catch it" case Fable's review called out, since a single quiet test never exercises cross-worker capture contamination at all). **Test precision (Sol round-3):** pass `--capture=fd` explicitly on this run (do not rely on it being pytest's current default, which could change or be overridden by a project's own config) and give the chatty tests a barrier/deterministic interleaving (e.g. each worker writes a distinct marker, then waits on a shared file/event before writing its bulk chatty output, so the contamination this test exists to catch would reproduce reliably rather than depending on incidental OS scheduling timing) rather than hoping concurrent prints happen to overlap. **Both runs (plain and aitest-driven) MUST pass `--junit-logging=all` (or `=system-out`/whatever exact option name this project's installed pytest version uses — verify, pytest's default `junit_logging` setting emits NO `<system-out>`/`<system-err>` elements at all, which would make this test's own captured-output assertions fail for a reason having nothing to do with this plan's fix, per Sol round-2).** Run both plainly and with `--aitest-workers=2 --junit-xml=<path>`. Assert the two XML outputs agree on: total counts per outcome, exact test-name set, AND (this is what Task 1's tuple fix and Task 2's validated-replay exist to guarantee) that the failing test's `<failure>`/`<system-out>`/`<system-err>` elements contain the SAME real diagnostic content in both — for the chatty-concurrent tests specifically, assert each one's captured output contains ONLY its own lines and nothing from any sibling — and that the skip/xfail elements are present and well-formed (not silently absent, which is exactly what the un-fixed tuple bug would have produced — a crash, not just missing content, so also assert the aitest-driven run's exit code and full run don't crash). When comparing the two XML trees, normalize (strip or ignore) `time=`/`timestamp=`/`hostname=` attributes first (v5, Fable's finding) — these legitimately differ between any two real runs and are not part of what this plan's fidelity guarantee covers.

- [ ] **Step 3: Run to verify failure**

- [ ] **Step 4-6:** No new implementation expected if Tasks 1-2 are correct; a failure here is a real gap in whichever task owns it.

- [ ] **Step 7: Full confined verification** — `aira confine -- make ci`, plus `aira confine -- python3 -m pytest -q internal/pylib/aitest/`

- [ ] **Step 8: Commit** — `git commit -m "test(aitest): end-to-end JUnit XML fidelity proof + Go e2e reconciliation (AIRA-31)"`

## Deferred / explicitly out of scope

- Look-ahead dispatch / session-scoped fixture reuse across tests on the same worker.
- `-x`/`--maxfail` (`session.shouldfail`) is set by a replayed report per pytest's own hook implementation, but nothing in `Supervisor.run`'s dispatch loop currently checks it, so it is silently ignored under aitest. Document this as a known, deferred gap (not fixed in this plan) rather than leaving it undiscovered.
- xdist's `rep.node = node` worker-identity bookkeeping — still not copied reflexively.
- Coverage combine itself, branch-coverage config compatibility, and any interaction with a project's own custom `.coveragerc` — explicitly not this plan's concern per the simplified Task 1 design; whoever owns coverage in a given project (pytest-cov or a bare `coverage run`) owns all of that, unchanged, same as it would for any other pytest plugin's workers.
- `pytest_warning_recorded` is not forwarded from worker to supervisor (v5, Fable's finding — xdist does forward it); combined with the child's own terminalreporter being unregistered, a test-time warning is silently absent from the parent's warnings summary under aitest. Document as deferred, not fixed here.
- `log_cli` live logging: the forked child's `LoggingPlugin` still retains its own reference to the (unregistered-for-progress-printing, but not otherwise disabled) terminalreporter and may still write live log lines to the shared terminal directly from the child. Not fixed in this plan; document the interaction.
- The plain terminal SUMMARY LINE aitest already prints (`__init__.py`, Slice 1) and the real terminalreporter's own summary (now populated via replay) can disagree in wording for a synthesized-unevaluated nodeid — the plain line says "unevaluated", the replayed summary counts it as failed/error (per the Report shape decision above, deliberately, since that is the only way to make it visible in JUnit at all). State this explicitly in Task 3's own test/documentation rather than let it read as an inconsistency bug.
