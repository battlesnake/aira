"""AIRA-343: the indexed ready queue picks EXACTLY what the old full-scan picker picked.

The old picker (a full scan of a plain list for every dispatch, then an O(n) list.remove)
is kept below VERBATIM as a test-only oracle, `_OldPicker`. The differential test drives
the real Supervisor and the oracle through the same random operation sequences -- peeks,
pops, next_nodeid, requeues, arbitrary removes, appends, terminal drains, queue and map
reassignments, budget changes -- and after EVERY operation compares the returned value,
the full queue contents (in order) and the attempts map.

The benchmark at the bottom is opt-in (AIRA_AITEST_BENCH=1): it measures both pickers on
the same synthetic 65k queue and prints the numbers. It asserts nothing about time in the
gate, where the box's load would make a timing assertion flaky.

    AIRA_AITEST_BENCH=1 PYTHONPATH=internal/pylib \\
        pytest -q -s internal/pylib/aitest/test_ready_queue.py -k bench
"""

import os
import random
import time

import pytest

from aitest.supervisor import Supervisor, _DEFAULT_CPU_CORES, _DEFAULT_TIME_COST


class _OldPicker:
    """The pre-AIRA-343 ready-queue code, copied verbatim (comments trimmed) from
    supervisor.py: a plain list, a full scan per pick, list.remove/pop(0)/insert(0)."""

    def __init__(self, queue, reservation_need, cpu_need, time_cost, overhead):
        self.queue = list(queue)
        self.reservation_need = reservation_need
        self.cpu_need = cpu_need
        self.time_cost = time_cost
        self._worker_overhead_bytes = overhead
        self.attempts = {}
        self.results = {}

    def _need_for(self, nodeid):
        return self.reservation_need.get(nodeid, self._worker_overhead_bytes)

    def _cpu_need_for(self, nodeid):
        return self.cpu_need.get(nodeid, _DEFAULT_CPU_CORES)

    def _time_for(self, nodeid):
        return self.time_cost.get(nodeid, _DEFAULT_TIME_COST)

    def _largest_fitting(self, budget, cpu_budget, *, pop):
        best = None
        best_key = None
        for index, nodeid in enumerate(self.queue):
            need = self._need_for(nodeid)
            if need > budget or self._cpu_need_for(nodeid) > cpu_budget:
                continue
            key = (self._time_for(nodeid), need, -index)
            if best_key is None or key > best_key:
                best = nodeid
                best_key = key
        if best is None:
            return None
        if pop:
            self.queue.remove(best)
            self.attempts[best] = self.attempts.get(best, 0) + 1
        return best

    def _smallest_ready(self):
        best = None
        best_need = None
        for nodeid in self.queue:
            need = self._need_for(nodeid)
            if best_need is None or need < best_need:
                best = nodeid
                best_need = need
        if best is None:
            return None, 0, _DEFAULT_CPU_CORES
        return best, best_need, self._cpu_need_for(best)

    def next_nodeid(self):
        if not self.queue:
            return None
        nodeid = self.queue.pop(0)
        self.attempts[nodeid] = self.attempts.get(nodeid, 0) + 1
        return nodeid

    def requeue_once(self, nodeid):
        if self.attempts.get(nodeid, 0) >= 2:
            return False
        self.queue.insert(0, nodeid)
        return True

    def _fail_queue_terminal(self, reason):
        while self.queue:
            nodeid = self.queue.pop(0)
            self.results.setdefault(nodeid, "unevaluated")


# Tie-heavy value pools: few distinct times and needs, so the (time, need, FIFO) tiebreaks
# and the cross-class comparisons are exercised constantly rather than by luck.
_NEEDS = (100, 100, 100, 300, 300, 700, 1500)
_CPUS = (1, 1, 1, 2, 4)
_TIMES = (1, 1, 1, 1, 2, 5, 5, 40)


def _random_maps(rng, names, overhead):
    """Need/cpu/time maps over `names`, leaving some nodeids ABSENT from each map so the
    fallbacks (overhead bytes, default cores, default time) take part in the order."""
    reservation_need = {n: rng.choice(_NEEDS) for n in names if rng.random() < 0.85}
    cpu_need = {n: rng.choice(_CPUS) for n in names if rng.random() < 0.8}
    time_cost = {n: rng.choice(_TIMES) for n in names if rng.random() < 0.7}
    return reservation_need, cpu_need, time_cost


def _random_budget(rng):
    budget = rng.choice((0, 99, 100, 150, 300, 301, 700, 1000, 1500, 10_000, 250.5))
    cpu_budget = rng.choice((0, 0.5, 1, 1, 2, 3, 4, 8))
    return budget, cpu_budget


def _apply_maps(sup, old, maps, overhead):
    reservation_need, cpu_need, time_cost = maps
    # Separate dict objects for the two sides: the real side must not see the oracle's
    # mutations, and vice versa (neither mutates in place today, but the test must not
    # depend on that).
    sup.reservation_need = dict(reservation_need)
    sup.cpu_need = dict(cpu_need)
    sup.time_cost = dict(time_cost)
    sup._worker_overhead_bytes = overhead
    old.reservation_need = dict(reservation_need)
    old.cpu_need = dict(cpu_need)
    old.time_cost = dict(time_cost)
    old._worker_overhead_bytes = overhead


def _check(sup, old, got, want, trail):
    assert got == want, "pick diverged after %r: new=%r old=%r" % (trail[-8:], got, want)
    assert list(sup.queue) == old.queue, "queue diverged after %r" % (trail[-8:],)
    assert len(sup.queue) == len(old.queue)
    assert bool(sup.queue) == bool(old.queue)
    assert sup.attempts == old.attempts, "attempts diverged after %r" % (trail[-8:],)


def _run_sequence(seed, ops=160):
    rng = random.Random(seed)
    n = rng.choice((0, 1, 2, 5, 12, 40, 90))
    names = ["pkg/test_m%d.py::t%d" % (i % 3, i) for i in range(n)]
    # A duplicate nodeid now and then: the old list allowed it, and remove() then takes
    # the FIRST occurrence -- the new queue must agree.
    if names and rng.random() < 0.15:
        names.insert(rng.randrange(len(names) + 1), rng.choice(names))
    overhead = rng.choice((100, 300, 512))
    maps = _random_maps(rng, names, overhead)
    sup = Supervisor()
    old = _OldPicker(names, *maps, overhead)
    _apply_maps(sup, old, maps, overhead)
    # The production order (collect) and the test-double order (queue first, maps after)
    # must both work: alternate which comes first.
    if rng.random() < 0.5:
        sup.queue = list(names)
    else:
        sup.queue = list(names)
        _apply_maps(sup, old, maps, overhead)
    in_flight = []  # popped nodeids a crash could requeue
    fresh = 0
    trail = []
    for _ in range(ops):
        op = rng.random()
        if op < 0.30:
            budget, cpu_budget = _random_budget(rng)
            trail.append(("pop", budget, cpu_budget))
            got = sup._largest_fitting(budget, cpu_budget, pop=True)
            want = old._largest_fitting(budget, cpu_budget, pop=True)
            if got is not None:
                in_flight.append(got)
        elif op < 0.50:
            budget, cpu_budget = _random_budget(rng)
            trail.append(("peek", budget, cpu_budget))
            got = sup._largest_fitting(budget, cpu_budget, pop=False)
            want = old._largest_fitting(budget, cpu_budget, pop=False)
        elif op < 0.58:
            trail.append(("next_nodeid",))
            got = sup.next_nodeid()
            want = old.next_nodeid()
            if got is not None:
                in_flight.append(got)
        elif op < 0.70 and in_flight:
            nodeid = in_flight.pop(rng.randrange(len(in_flight)))
            trail.append(("requeue", nodeid))
            got = sup.requeue_once(nodeid)
            want = old.requeue_once(nodeid)
        elif op < 0.75 and old.queue:
            # The empty-pool bootstrap's exceeds-ceiling path: remove one queued nodeid.
            nodeid = rng.choice(old.queue)
            trail.append(("remove", nodeid))
            sup.queue.remove(nodeid)
            old.queue.remove(nodeid)
            got = want = None
        elif op < 0.80:
            nodeid = "pkg/fresh.py::f%d" % fresh if rng.random() < 0.8 or not names else rng.choice(names)
            fresh += 1
            trail.append(("append", nodeid))
            sup.queue.append(nodeid)
            old.queue.append(nodeid)
            got = want = None
        elif op < 0.84:
            trail.append(("smallest_ready",))
            got = sup._smallest_ready()
            want = old._smallest_ready()
        elif op < 0.88:
            # Map reassignment AFTER the queue exists (tests do this; collect() assigns the
            # queue before the maps): the index must follow the live maps.
            everything = names + ["pkg/fresh.py::f%d" % i for i in range(fresh)]
            maps = _random_maps(rng, everything, overhead)
            if rng.random() < 0.3:
                overhead = rng.choice((100, 300, 512, 2000))
            trail.append(("remap", overhead))
            _apply_maps(sup, old, maps, overhead)
            got = want = None
        elif op < 0.91:
            # Queue reassignment: a shuffled subset of everything seen.
            pool = list(old.queue) + in_flight
            rng.shuffle(pool)
            pool = pool[: rng.randrange(len(pool) + 1)] if pool else []
            trail.append(("reassign", len(pool)))
            sup.queue = list(pool)
            old.queue = list(pool)
            got = want = None
        elif op < 0.92:
            trail.append(("terminal",))
            sup._fail_queue_terminal("test terminal")
            old._fail_queue_terminal("test terminal")
            got = want = None
            assert {k: v for k, v in sup.results.items()} == old.results
        else:
            trail.append(("contains",))
            probe = rng.choice(names + ["pkg/absent.py::x"]) if names else "pkg/absent.py::x"
            got = probe in sup.queue
            want = probe in old.queue
        _check(sup, old, got, want, trail)
    return sup


@pytest.mark.parametrize("block", range(6))
def test_indexed_queue_matches_the_old_full_scan_picker(block):
    # 6 x 80 seeds x 160 ops: ~77k compared operations, a few seconds in total.
    for seed in range(block * 80, (block + 1) * 80):
        _run_sequence(seed)


def test_drain_to_empty_matches_the_old_picker_on_one_long_queue():
    # One realistic-shaped queue drained to the end by a mix of worker sizes, the way the
    # pool drains it: every pop compared, including the lazy-deletion tail where classes
    # empty out.
    rng = random.Random(4242)
    names = ["pkg/test_m%d.py::t%d[%d]" % (i // 50, i, i % 7) for i in range(3000)]
    maps = _random_maps(rng, names, 512)
    sup = Supervisor()
    old = _OldPicker(names, *maps, 512)
    sup.queue = list(names)
    _apply_maps(sup, old, maps, 512)
    workers = [(1500, 4), (700, 2), (300, 1), (100, 1), (10_000, 8)]
    while old.queue:
        budget, cpu_budget = rng.choice(workers)
        got = sup._largest_fitting(budget, cpu_budget, pop=True)
        want = old._largest_fitting(budget, cpu_budget, pop=True)
        assert got == want
        if got is not None and rng.random() < 0.02:
            assert sup.requeue_once(got) == old.requeue_once(got)
    assert list(sup.queue) == [] and sup.attempts == old.attempts


# --- the named invariants, each pinned on its own (readable failures) -------------------

def _sup(queue, need, cpu=None, time_cost=None):
    sup = Supervisor()
    sup.reservation_need = need
    sup.cpu_need = cpu or {}
    sup.time_cost = time_cost or {}
    sup.queue = queue
    return sup


def test_requeued_nodeid_goes_ahead_of_every_equal_key_test():
    # FIFO survives the head insert: requeue_once puts the nodeid at index 0, so on an
    # all-equal key it is the next pick, ahead of tests queued before it was first popped.
    sup = _sup(["a", "b", "c"], {"a": 1, "b": 1, "c": 1})
    assert sup._largest_fitting(10, 1, pop=True) == "a"
    assert sup._largest_fitting(10, 1, pop=True) == "b"
    assert sup.requeue_once("b") is True
    assert list(sup.queue) == ["b", "c"]
    assert sup._largest_fitting(10, 1, pop=False) == "b"


def test_a_removed_then_requeued_nodeid_does_not_revive_its_stale_entry():
    # Lazy deletion is judged by the ENTRY, not by "is this nodeid queued": "a" leaves via
    # pop (its old heap entry is stale) and comes back via requeue at the head. Exactly one
    # live entry must exist: two pops must not both return "a".
    sup = _sup(["a", "b"], {"a": 5, "b": 1}, time_cost={"a": 9})
    assert sup._largest_fitting(10, 1, pop=True) == "a"
    sup.requeue_once("a")
    assert sup._largest_fitting(10, 1, pop=True) == "a"
    assert sup._largest_fitting(10, 1, pop=True) == "b"
    assert sup._largest_fitting(10, 1, pop=True) is None
    assert list(sup.queue) == []


def test_remove_then_pick_skips_the_removed_test():
    sup = _sup(["big", "small"], {"big": 900, "small": 100})
    sup.queue.remove("big")
    assert sup._largest_fitting(1000, 1, pop=False) == "small"


def test_map_reassignment_after_the_queue_is_seen_by_the_next_pick():
    sup = _sup(["a", "b"], {"a": 100, "b": 300})
    assert sup._largest_fitting(1000, 1, pop=False) == "b"
    sup.reservation_need = {"a": 400, "b": 300}
    assert sup._largest_fitting(1000, 1, pop=False) == "a"
    sup.time_cost = {"b": 7}
    assert sup._largest_fitting(1000, 1, pop=False) == "b"
    sup.cpu_need = {"b": 4}
    assert sup._largest_fitting(1000, 1, pop=False) == "a"


def test_an_overhead_only_reassignment_is_seen_by_the_next_pick():
    # "a" is absent from reservation_need, so its need is the worker overhead. Reassigning
    # ONLY the overhead (no map changes) must re-index: the differential test's remap op
    # always reassigns the maps too, so it cannot tell whether the overhead is watched.
    sup = _sup(["a", "b"], {"b": 300})
    sup._worker_overhead_bytes = 100
    assert sup._largest_fitting(1000, 1, pop=False) == "b"  # builds the index: a=100, b=300
    sup._worker_overhead_bytes = 400
    assert sup._largest_fitting(1000, 1, pop=False) == "a"
    assert sup._largest_fitting(350, 1, pop=False) == "b", "a (400) no longer fits 350"


def test_the_queue_still_reads_as_a_list():
    sup = _sup(["a", "b"], {})
    assert sup.queue == ["a", "b"]
    assert sup.queue != ["b", "a"]
    assert "a" in sup.queue and "z" not in sup.queue
    sup.queue.append("c")
    assert sup.queue.pop(0) == "a"
    assert sup.queue.pop() == "c"
    assert list(sup.queue) == ["b"]
    with pytest.raises(ValueError):
        sup.queue.remove("zz")
    sup.queue.pop()
    with pytest.raises(IndexError):
        sup.queue.pop(0)


# --- benchmark (opt-in) ---------------------------------------------------------------

_BENCH = os.environ.get("AIRA_AITEST_BENCH", "").strip() not in ("", "0")


def _bench_queue(n, seed=1):
    """A queue shaped like fastest.ee's engine leg (speed's bench_dispatch.py: 65,164
    parametrised nodeids, 512 MiB overhead, ~1% carrying an aira_time weight of 10..466),
    plus a realistic mix of reservation/cpu classes (most tests unannotated; a few
    @aira_mem / @aira_cpu marks)."""
    rng = random.Random(seed)
    overhead = 512 << 20
    names = ["fastest_ee/libs/engine/test_mod_%d.py::test_case_%d[param-%d]" % (i // 40, i, i % 7) for i in range(n)]
    reservation_need = {}
    cpu_need = {}
    time_cost = {}
    for name in names:
        r = rng.random()
        reservation_need[name] = overhead + (0 if r < 0.9 else rng.choice((256, 512, 1024, 2048)) << 20)
        cpu_need[name] = 1 if rng.random() < 0.95 else rng.choice((2, 4, 8))
        time_cost[name] = rng.randint(10, 466) if rng.random() < 0.01 else 1
    return names, reservation_need, cpu_need, time_cost, overhead


def _time_picks(picker, budgets, k):
    # Every timed call must hand out a test: a call that finds nothing does no pop and
    # would make a per-dispatch average look cheaper than a real dispatch.
    picked = []
    start = time.perf_counter()
    for i in range(k):
        budget, cpu_budget = budgets[i % len(budgets)]
        picked.append(picker._largest_fitting(budget, cpu_budget, pop=True))
    elapsed = time.perf_counter() - start
    assert None not in picked, "a timed pick returned no test"
    return elapsed / k, picked


@pytest.mark.skipif(not _BENCH, reason="benchmark: set AIRA_AITEST_BENCH=1 to run")
def test_bench_dispatch_old_vs_new():
    import sys
    # A big worker (fits every class) and the mixed sizes a heterogeneous pool hands out.
    budgets = [(1 << 40, 64), (1 << 40, 64), (768 << 20, 1), (3 << 30, 4)]
    for n in (65164, 32000, 8000):
        names, need, cpu, tcost, overhead = _bench_queue(n)
        old = _OldPicker(names, need, cpu, tcost, overhead)
        sup = Supervisor()
        sup._worker_overhead_bytes = overhead
        sup.reservation_need, sup.cpu_need, sup.time_cost = need, cpu, tcost
        sup.queue = list(names)
        build_start = time.perf_counter()
        sup._largest_fitting(0, 0, pop=False)  # builds the index (once per queue/map set)
        build = time.perf_counter() - build_start
        classes = len({(need[x], cpu[x]) for x in names})
        # The SAME number of successful dispatches for both pickers, from the same queue
        # and budgets (the old picker's cost sets k: ~25 ms per pick at 65k).
        k = 200 if n > 50000 else 400 if n > 20000 else 1000
        old_s, old_picked = _time_picks(old, budgets, k)
        new_s, new_picked = _time_picks(sup, budgets, k)
        assert new_picked == old_picked, "the two pickers must hand out the same tests"
        sys.stdout.write(
            "python %s queue=%d classes=%d: old %.3f ms/dispatch, new %.4f ms/dispatch "
            "over %d successful dispatches each (index build %.1f ms once)\n"
            % (sys.version.split()[0], n, classes, old_s * 1e3, new_s * 1e3, k, build * 1e3)
        )
        if n == 65164:
            assert new_s < 0.1e-3, "acceptance: under 0.1 ms per dispatch at a 65k queue"
            # _pool_covers_the_queue (called every loop iteration via _maybe_grow_pool) needs
            # no index: its len(queue) > idle-workers guard returns before any sort while the
            # queue is longer than the pool. Measured here with 64 idle workers.
            sup.workers = {i: {"in_flight": None, "reservation": 1 << 40, "cpu": 64} for i in range(64)}
            start = time.perf_counter()
            for _ in range(2000):
                assert sup._pool_covers_the_queue() is False
            sys.stdout.write(
                "python %s queue=%d: _pool_covers_the_queue %.4f ms/call (64 idle workers)\n"
                % (sys.version.split()[0], len(sup.queue), (time.perf_counter() - start) / 2000 * 1e3)
            )
            sup.workers = {}
    # The full drain at 65k, new picker only (the old one would take ~15 minutes).
    names, need, cpu, tcost, overhead = _bench_queue(65164)
    sup = Supervisor()
    sup._worker_overhead_bytes = overhead
    sup.reservation_need, sup.cpu_need, sup.time_cost = need, cpu, tcost
    sup.queue = list(names)
    start = time.perf_counter()
    picks = 0
    while sup.queue:
        budget, cpu_budget = budgets[picks % len(budgets)]
        if sup._largest_fitting(budget, cpu_budget, pop=True) is not None:
            picks += 1
        elif sup._largest_fitting(1 << 40, 64, pop=True) is not None:
            picks += 1
    sys.stdout.write(
        "python %s full drain of 65164: %.2f s total supervisor pick time (%d picks)\n"
        % (sys.version.split()[0], time.perf_counter() - start, picks)
    )
