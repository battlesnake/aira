"""AIRA-229 (v0.7 S1, slice v7-1): the client-side aggregate outer-cap Σ-guard.

The shipped v0.6 daemon deleted the outer-cap aggregate scan (worker_admit.go
:463-466) and relies on the outer scope's own memory.oom.group as the ONLY bound
on Σ(worker caps) <= outer-cap -- i.e. a whole-suite oom.group kill is the backstop.
This guard restores a client-side aggregate bound so the supervisor refuses an
over-admitting spawn BEFORE it is forked, and the outer oom.group never fires.

These are the DETERMINISTIC (no OOM race) tests: the ancestry walk, the two
dispositions (Σ_live==0 terminal / Σ_live>0 skip-tick), mode-dependence, and the
mutation guard (the guard admits at most floor((cap - margin - allowance)/request)
concurrent workers). The real-cgroup "outer oom.group never fires" e2e is the
complementary branch-exit gate (it cannot run in ci-shim CI)."""

import os

import pytest

import aitest.supervisor as supervisor_module
from aitest.supervisor import (
    Supervisor,
    WorkerAdmitDenied,
    WorkerAdmitOuterCapExceeded,
    WorkerAdmitTerminal,
)


class _AcquireReached(Exception):
    """Sentinel: raised by a stubbed acquire_worker to prove the guard let the
    spawn PROCEED to the (real, worker-granting) admission call it guards."""


def _make_cgroup(path, memory_max=None, controllers=True):
    """Write a fake cgroup2 directory: a real one always has cgroup.controllers,
    and has memory.max iff +memory is delegated. The guard reads only these two
    files, so a file tree is a faithful stand-in for the ancestry walk without a
    real cgroup mount."""
    os.makedirs(str(path), exist_ok=True)
    if controllers:
        with open(os.path.join(str(path), "cgroup.controllers"), "w") as handle:
            handle.write("cpuset cpu memory pids\n")
    if memory_max is not None:
        with open(os.path.join(str(path), "memory.max"), "w") as handle:
            handle.write("%s\n" % memory_max)


def _real_mode_supervisor(outer_scope, *, base=0, per_relay=0, margin=0):
    sup = Supervisor()
    sup.admission_mode = supervisor_module._ADMISSION_SUB_SCOPE
    sup.daemon_available = True
    sup.outer_scope = str(outer_scope)
    sup._outer_cap_base = base
    sup._outer_cap_per_relay = per_relay
    sup._outer_cap_margin = margin
    return sup


def _add_live_worker(sup, memory_max, pid=None):
    if pid is None:
        pid = 100000 + len(sup.workers)
    # memory_max is a STRING in a real enforced grant (the worker-admit outcome
    # parser stores every field as a string). Use str() here so these fixtures
    # match the real grant shape -- an int-only fixture silently hid a guard that
    # was inert on every real run (Σ_live always 0), caught by the real-cgroup e2e.
    sup.workers[pid] = {"grant": {"scope": "/fake/worker-%d" % pid, "memory_max": str(memory_max)},
                        "in_flight": None}
    return pid


# --------------------------------------------------------------------------
# effective-outer-cap ancestry walk (mirrors confine_linux.go effectiveCapFrom)
# --------------------------------------------------------------------------

def test_effective_outer_cap_uncapped_child_bounded_by_capped_parent(tmp_path):
    # slice (memory.max=1000) / outer (memory.max="max"): the uncapped child is
    # still bounded by the capped parent, exactly as memory.max is hierarchical.
    _make_cgroup(tmp_path / "slice", memory_max=1000)
    _make_cgroup(tmp_path / "slice" / "outer", memory_max="max")
    sup = _real_mode_supervisor(tmp_path / "slice" / "outer")
    assert sup._effective_outer_cap() == 1000


def test_effective_outer_cap_is_the_minimum_over_the_ancestry(tmp_path):
    _make_cgroup(tmp_path / "slice", memory_max=1000)
    _make_cgroup(tmp_path / "slice" / "outer", memory_max=500)
    sup = _real_mode_supervisor(tmp_path / "slice" / "outer")
    assert sup._effective_outer_cap() == 500


def test_effective_outer_cap_read_error_keeps_walking(tmp_path):
    # A level with NO memory.max file (like the real cgroup2 root) is
    # "not finite, keep walking", not a hard stop.
    _make_cgroup(tmp_path / "slice", memory_max=1000)
    _make_cgroup(tmp_path / "slice" / "outer", memory_max=None)  # controllers, no memory.max
    sup = _real_mode_supervisor(tmp_path / "slice" / "outer")
    assert sup._effective_outer_cap() == 1000


def test_effective_outer_cap_none_when_no_finite_cap_anywhere(tmp_path):
    _make_cgroup(tmp_path / "slice", memory_max="max")
    _make_cgroup(tmp_path / "slice" / "outer", memory_max="max")
    sup = _real_mode_supervisor(tmp_path / "slice" / "outer")
    assert sup._effective_outer_cap() is None


def test_effective_outer_cap_walk_stops_below_a_non_cgroup_parent(tmp_path):
    # The parent that lacks cgroup.controllers (tmp_path itself here) terminates
    # the walk: a memory.max placed ABOVE it must never be read.
    _make_cgroup(tmp_path, memory_max=1, controllers=False)  # NOT a cgroup dir
    _make_cgroup(tmp_path / "outer", memory_max=4242)
    sup = _real_mode_supervisor(tmp_path / "outer")
    assert sup._effective_outer_cap() == 4242


# --------------------------------------------------------------------------
# Σ_live derived at check time from the live workers map
# --------------------------------------------------------------------------

def test_sum_live_worker_caps_skips_fallback_and_ledger_only():
    sup = Supervisor()
    sup.workers[1] = {"grant": None}                                   # fallback (unconfined)
    sup.workers[2] = {"grant": {"worker_id": "7", "reserved": "500"}}  # ledger-only (no memory_max)
    # Real enforced grants carry memory_max as a STRING -- the guard must count it.
    sup.workers[3] = {"grant": {"scope": "/a", "memory_max": "300"}}   # enforced
    sup.workers[4] = {"grant": {"scope": "/b", "memory_max": "200"}}   # enforced
    sigma, n = sup._sum_live_worker_caps()
    assert sigma == 500
    assert n == 2


# --------------------------------------------------------------------------
# the two dispositions
# --------------------------------------------------------------------------

def test_guard_terminal_when_pool_empty_and_request_over_cap(tmp_path):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    with pytest.raises(WorkerAdmitOuterCapExceeded) as exc:
        sup._would_breach_outer_cap(2000)
    assert isinstance(exc.value, WorkerAdmitTerminal)  # routed to _fail_queue_terminal
    assert "outer-cap-exceeded" in str(exc.value)


def test_guard_skip_tick_when_pool_nonempty_and_next_would_breach(tmp_path):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    # Seven live workers of 100 each: Σ_live=700, allowance=100 -> 700+100+100=900
    # == cap-margin, still fits. The EIGHTH is fine too (see the deterministic
    # test); pack the pool right to the edge so the next spawn breaches.
    for _ in range(8):
        _add_live_worker(sup, 100)
    with pytest.raises(WorkerAdmitDenied) as exc:
        sup._would_breach_outer_cap(100)
    # A skip-tick denial, NOT a terminal (a retirement will free room).
    assert not isinstance(exc.value, WorkerAdmitTerminal)
    assert "outer-cap" in str(exc.value)


def test_guard_skip_tick_warns_once_that_pool_is_outer_cap_bounded(tmp_path, capsys):
    # The Σ_live>0 skip-tick is otherwise silent (the growth path swallows the
    # denial). A pool capped BELOW the requested worker count by the outer cap is a
    # governance fact the run must not stay silent about -- say it exactly once.
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    sup._run_worker_count = 20
    for _ in range(8):
        _add_live_worker(sup, 100)
    for _ in range(3):
        with pytest.raises(WorkerAdmitDenied):
            sup._would_breach_outer_cap(100)
    err = capsys.readouterr().err
    assert err.count("pool bounded by the OUTER-SCOPE cap") == 1
    assert "requested 20" in err


def test_env_bytes_accepts_bare_zero_and_warns_on_garbage(monkeypatch, capsys):
    # per_relay=0 (a bare "0") is how the branch-exit gate pins K exactly; it must
    # parse to 0, not silently fall back to the default.
    monkeypatch.setenv("AIRA_AITEST_OUTER_CAP_ALLOWANCE_PER_RELAY", "0")
    assert supervisor_module._env_bytes("AIRA_AITEST_OUTER_CAP_ALLOWANCE_PER_RELAY", 8 << 20) == 0
    monkeypatch.setenv("AIRA_AITEST_OUTER_CAP_MARGIN", "32M")
    assert supervisor_module._env_bytes("AIRA_AITEST_OUTER_CAP_MARGIN", 0) == 32 << 20
    monkeypatch.delenv("AIRA_AITEST_OUTER_CAP_MARGIN", raising=False)
    assert supervisor_module._env_bytes("AIRA_AITEST_OUTER_CAP_MARGIN", 99) == 99  # unset -> default, silent
    assert capsys.readouterr().err == ""
    # A NON-EMPTY typo warns and keeps the default (AIRA-223), never silent.
    monkeypatch.setenv("AIRA_AITEST_OUTER_CAP_MARGIN", "12x")  # x is not a valid unit
    assert supervisor_module._env_bytes("AIRA_AITEST_OUTER_CAP_MARGIN", 99) == 99
    assert "not a valid size" in capsys.readouterr().err


def test_guard_retirement_frees_room(tmp_path):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    for _ in range(8):
        _add_live_worker(sup, 100)
    with pytest.raises(WorkerAdmitDenied):
        sup._would_breach_outer_cap(100)
    # A worker retires (removed from the live map before _replace_worker spawns):
    sup.workers.pop(next(iter(sup.workers)))
    assert sup._would_breach_outer_cap(100) is None  # room freed, spawn proceeds


# --------------------------------------------------------------------------
# mode-dependence (D4)
# --------------------------------------------------------------------------

def test_guard_unevaluated_and_proceeds_in_ci_shim_mode(tmp_path, capsys):
    # ledger-only/advisory: no outer cgroup, no oom.group -> the hazard does not
    # exist; the guard must PROCEED (never refuse), even for an absurd request.
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    sup.admission_mode = supervisor_module._ADMISSION_LEDGER_ONLY
    assert sup._would_breach_outer_cap(10 ** 12) is None
    assert capsys.readouterr().err == ""  # silent: this is a supported mode, not a fault


def test_guard_real_mode_no_finite_cap_warns_once_then_proceeds(tmp_path, capsys):
    # Real (cgroup-sub-scope) mode with no finite cap anywhere is impossible by
    # precondition (confine + bootstrap both refuse an uncapped outer scope). If
    # it is ever seen: WARN ONCE, then PROCEED -- never a refuse-all, never silent.
    _make_cgroup(tmp_path / "outer", memory_max="max")
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    assert sup._would_breach_outer_cap(10 ** 12) is None
    first = capsys.readouterr().err
    assert "no finite memory.max" in first
    # A second (and third) check does not repeat the warning.
    assert sup._would_breach_outer_cap(10 ** 12) is None
    assert capsys.readouterr().err == ""


# --------------------------------------------------------------------------
# deterministic mutation guard: the guard admits AT MOST
# floor((outer_cap - margin - allowance)/request) concurrent live workers.
# With per_relay=0 the allowance is the constant `base`, so the bound is exact
# and the mutant that drops the Σ_live term admits MORE than it (reds the loop
# safety bound) WITHOUT relying on any OOM actually firing.
# --------------------------------------------------------------------------

def _admit_until_refused(sup, request, safety=1000):
    admitted = 0
    while admitted < safety:
        try:
            sup._would_breach_outer_cap(request)
        except (WorkerAdmitTerminal, WorkerAdmitDenied):
            break
        _add_live_worker(sup, request)
        admitted += 1
    return admitted


def test_guard_admits_at_most_floor_bound(tmp_path):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, per_relay=0, margin=100)
    outer_cap, margin, allowance, request = 1000, 100, 100, 100
    expected = (outer_cap - margin - allowance) // request  # == 8
    assert expected == 8
    admitted = _admit_until_refused(sup, request, safety=expected + 50)
    # The mutant that computes the guard WITHOUT Σ_live admits every request
    # (the condition becomes the constant `allowance + request <= cap - margin`),
    # so it runs to the safety bound and this exact-count assertion reds.
    assert admitted == expected


def test_allowance_grows_with_live_relays(tmp_path):
    # per_relay > 0: each live relay charges the uncapped .aira-supervisor, so the
    # pool the guard admits is STRICTLY smaller than with per_relay=0. Proves the
    # N_live x per_relay term is live, not decorative.
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    without = _admit_until_refused(
        _real_mode_supervisor(tmp_path / "outer", base=100, per_relay=0, margin=100), 100)
    with_relay = _admit_until_refused(
        _real_mode_supervisor(tmp_path / "outer", base=100, per_relay=20, margin=100), 100)
    assert with_relay < without


# --------------------------------------------------------------------------
# chokepoint: the guard is consulted at the TOP of spawn_worker, before any
# worker-granting relay is forked.
# --------------------------------------------------------------------------

def test_spawn_worker_guard_short_circuits_before_acquire_when_terminal(tmp_path, monkeypatch):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    monkeypatch.setattr(sup, "acquire_worker",
                        lambda *a, **k: (_ for _ in ()).throw(_AcquireReached()))
    with pytest.raises(WorkerAdmitOuterCapExceeded):
        sup.spawn_worker(2000)
    assert sup.workers == {}  # nothing forked, nothing registered


def test_spawn_worker_guard_lets_ci_shim_reach_acquire(tmp_path, monkeypatch):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    sup.admission_mode = supervisor_module._ADMISSION_LEDGER_ONLY
    monkeypatch.setattr(sup, "acquire_worker",
                        lambda *a, **k: (_ for _ in ()).throw(_AcquireReached()))
    with pytest.raises(_AcquireReached):
        sup.spawn_worker(10 ** 12)  # guard proceeds; the sentinel proves acquire was reached


# --------------------------------------------------------------------------
# disposition wiring through the growth path (no real fork: the guard raises
# before acquire_worker/fork_worker in spawn_worker).
# --------------------------------------------------------------------------

def test_try_grow_one_routes_terminal_to_fail_queue(tmp_path, monkeypatch):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    sup.queue = ["t1", "t2"]
    monkeypatch.setattr(sup, "_probe_available", lambda: (10 ** 12, 1024))
    sup._run_estimated_bytes = 2000  # over the cap; Σ_live==0 -> terminal
    assert sup._try_grow_one() is False
    assert sup.queue == []                          # drained by _fail_queue_terminal
    assert sup.results["t1"] == "unevaluated"
    assert sup.results["t2"] == "unevaluated"


def test_try_grow_one_skips_tick_without_draining_queue(tmp_path, monkeypatch):
    _make_cgroup(tmp_path / "outer", memory_max=1000)
    sup = _real_mode_supervisor(tmp_path / "outer", base=100, margin=100)
    sup.queue = ["t1", "t2"]
    for _ in range(8):
        _add_live_worker(sup, 100)
    live_before = set(sup.workers)
    monkeypatch.setattr(sup, "_probe_available", lambda: (10 ** 12, 1024))
    sup._run_estimated_bytes = 100  # Σ_live>0 breach -> WorkerAdmitDenied -> skip
    assert sup._try_grow_one() is False
    assert sup.queue == ["t1", "t2"]                # NOT drained -- a retirement frees room
    assert set(sup.workers) == live_before          # pool keeps its live workers
    assert sup.results == {}
