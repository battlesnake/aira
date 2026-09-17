"""AIRA-261: the aira_cpu marker reader + the CPU dimension of the admission fit.

aira_cpu declares a test's peak internal fork width as an ABSOLUTE core count (unlike
aira_mem's floor+increment). These tests pin the reader's contract (positive-integer
grammar, the AIRA-223 warn-once-never-silent discipline) and the supervisor-side cpu
consumer: collect() builds cpu_need; the fit is 2-D (a cpu=1 worker is NEVER handed a
cpu=N>1 test, or the daemon's cpu ledger — charged 1 core for that worker — would
under-count the test's real fork width and oversubscribe the box); _smallest_ready and
_pool_covers_the_queue both account for cpu.
"""

import pytest

from aitest import _aira_cpu_cores_for_item
from aitest.supervisor import Supervisor, _DEFAULT_CPU_CORES


def _getitems(pytester, source):
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    return pytester.getitems(source)


def _item(items, name):
    for item in items:
        if item.nodeid.endswith("::" + name):
            return item
    raise KeyError("no collected item named %r in %r" % (name, [i.nodeid for i in items]))


# --- the reader ------------------------------------------------------------

def test_positive_integer_arg_is_the_core_count(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_cpu(4)
        def test_x(): pass
    ''')
    value, warning = _aira_cpu_cores_for_item(_item(items, "test_x"), _DEFAULT_CPU_CORES)
    assert value == 4 and warning is None


def test_int_string_arg_parses(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_cpu("2")
        def test_x(): pass
    ''')
    value, warning = _aira_cpu_cores_for_item(_item(items, "test_x"), _DEFAULT_CPU_CORES)
    assert value == 2 and warning is None


def test_unannotated_item_takes_the_default_silently(pytester):
    items = _getitems(pytester, 'def test_x(): pass')
    value, warning = _aira_cpu_cores_for_item(_item(items, "test_x"), _DEFAULT_CPU_CORES)
    assert value == _DEFAULT_CPU_CORES and warning is None


@pytest.mark.parametrize("arg", [0, -1, 2.5, "four", "1.5"])
def test_malformed_arg_warns_and_takes_the_default(pytester, arg):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_cpu(%r)
        def test_x(): pass
    ''' % (arg,))
    value, warning = _aira_cpu_cores_for_item(_item(items, "test_x"), _DEFAULT_CPU_CORES)
    assert value == _DEFAULT_CPU_CORES, "a malformed aira_cpu must fall back to the default"
    assert warning is not None and "aira_cpu" in warning, "and it must WARN, never silently"


def test_bool_arg_is_rejected(pytester):
    # bool is an int subclass; aira_cpu(True) must NOT be read as 1 core silently.
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_cpu(True)
        def test_x(): pass
    ''')
    value, warning = _aira_cpu_cores_for_item(_item(items, "test_x"), _DEFAULT_CPU_CORES)
    assert value == _DEFAULT_CPU_CORES and warning is not None


def test_multiple_positional_args_warns(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_cpu(2, 3)
        def test_x(): pass
    ''')
    value, warning = _aira_cpu_cores_for_item(_item(items, "test_x"), _DEFAULT_CPU_CORES)
    assert value == _DEFAULT_CPU_CORES and warning is not None


# --- collect() builds the cpu_need map ------------------------------------

def test_collect_builds_cpu_need(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_cpu(4)
        def test_big(): pass
        def test_small(): pass
    ''')
    sup = Supervisor()
    sup.collect(items)
    assert sup.cpu_need[_item(items, "test_big").nodeid] == 4
    assert sup.cpu_need[_item(items, "test_small").nodeid] == _DEFAULT_CPU_CORES
    assert sup._cpu_need_for(_item(items, "test_big").nodeid) == 4


# --- the 2-D fit (bytes AND cpu) ------------------------------------------

def test_largest_fitting_excludes_a_test_needing_more_cores_than_the_budget():
    sup = Supervisor()
    # "b" has the LARGER byte need, so the largest-BYTES-first ordering alone would pick it
    # -- the cpu filter is the ONLY reason "a" wins for a 1-core worker (drop the cpu clause
    # and this reds, picking "b").
    sup.reservation_need = {"a": 100, "b": 200}
    sup.cpu_need = {"a": 1, "b": 4}
    sup.queue = ["a", "b"]
    # A 1-core worker must NOT be handed "b" (4 cores), even though "b" is the larger byte fit.
    assert sup._largest_fitting(1000, 1, pop=False) == "a"
    # A 4-core budget fits both; now "b" (larger bytes) wins on the byte ordering.
    assert sup._largest_fitting(1000, 4, pop=False) == "b"


def test_largest_fitting_is_none_when_cpu_never_fits():
    sup = Supervisor()
    sup.reservation_need = {"a": 100}
    sup.cpu_need = {"a": 8}
    sup.queue = ["a"]
    assert sup._largest_fitting(1000, 4, pop=False) is None


def test_smallest_ready_carries_the_cpu_need():
    sup = Supervisor()
    sup.reservation_need = {"a": 300, "b": 100}
    sup.cpu_need = {"a": 1, "b": 4}
    sup.queue = ["a", "b"]
    # smallest BYTES is "b" (100), and its cpu_need (4) rides along for the blocking claim.
    assert sup._smallest_ready() == ("b", 100, 4)


# --- _pool_covers_the_queue is 2-D ----------------------------------------

def test_pool_cover_requires_a_cpu_fitting_idle_worker():
    sup = Supervisor()
    sup.reservation_need = {"a": 100}
    sup.cpu_need = {"a": 4}
    sup.queue = ["a"]
    # An idle worker with plenty of bytes but only 1 core does NOT cover a 4-core test:
    # growth is still worthwhile, so cover is False.
    sup.workers = {1: {"in_flight": None, "reservation": 1000, "cpu": 1}}
    assert sup._pool_covers_the_queue() is False
    # A 4-core idle worker DOES cover it.
    sup.workers = {1: {"in_flight": None, "reservation": 1000, "cpu": 4}}
    assert sup._pool_covers_the_queue() is True


def test_pool_cover_matches_distinct_workers_across_both_dims():
    sup = Supervisor()
    sup.reservation_need = {"big": 900, "wide": 100}
    sup.cpu_need = {"big": 1, "wide": 4}
    sup.queue = ["big", "wide"]
    # Two idle workers: one big-byte/1-core, one small-byte/4-core. A distinct fitting
    # worker exists for each queued test (big→byte worker, wide→cpu worker), so covered.
    sup.workers = {
        1: {"in_flight": None, "reservation": 1000, "cpu": 1},
        2: {"in_flight": None, "reservation": 200, "cpu": 4},
    }
    assert sup._pool_covers_the_queue() is True
    # Drop the wide-cpu worker: "wide" (4 cores) can no longer be placed → not covered.
    sup.workers = {1: {"in_flight": None, "reservation": 1000, "cpu": 1}}
    assert sup._pool_covers_the_queue() is False
