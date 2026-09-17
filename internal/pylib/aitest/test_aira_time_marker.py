"""AIRA-261 Phase B: the aira_time marker reader + LPT (longest-processing-time-first) order.

aira_time is a RELATIVE, unitless scheduling-cost rank (default 1). It reserves nothing and
gates no admission -- it ONLY re-keys the ready-queue pick so the dispatcher hands out the
longest-time FITTING test first, shortening the drain tail. These tests pin the reader's
contract (positive-integer grammar, AIRA-223 warn-never-silent) and the LPT order (heavy-time
first, RAM bin-pack secondary, FIFO tertiary; byte-identical with zero marks; fit BEFORE order).
"""

import pytest

from aitest import _aira_time_for_item
from aitest.supervisor import Supervisor, _DEFAULT_TIME_COST


def _getitems(pytester, source):
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    return pytester.getitems(source)


def _item(items, name):
    for item in items:
        if item.nodeid.endswith("::" + name):
            return item
    raise KeyError("no collected item named %r in %r" % (name, [i.nodeid for i in items]))


# --- the reader ------------------------------------------------------------

def test_positive_integer_arg_is_the_cost(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_time(50)
        def test_x(): pass
    ''')
    value, warning = _aira_time_for_item(_item(items, "test_x"), _DEFAULT_TIME_COST)
    assert value == 50 and warning is None


def test_unannotated_takes_the_default_silently(pytester):
    items = _getitems(pytester, 'def test_x(): pass')
    value, warning = _aira_time_for_item(_item(items, "test_x"), _DEFAULT_TIME_COST)
    assert value == _DEFAULT_TIME_COST and warning is None


@pytest.mark.parametrize("arg", [0, -1, 2.5, "slow"])
def test_malformed_arg_warns_and_takes_the_default(pytester, arg):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_time(%r)
        def test_x(): pass
    ''' % (arg,))
    value, warning = _aira_time_for_item(_item(items, "test_x"), _DEFAULT_TIME_COST)
    assert value == _DEFAULT_TIME_COST, "a malformed aira_time must fall back to the default"
    assert warning is not None and "aira_time" in warning, "and it must WARN, never silently"


def test_non_finite_float_warns_not_crashes(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_time(float("inf"))
        def test_x(): pass
    ''')
    value, warning = _aira_time_for_item(_item(items, "test_x"), _DEFAULT_TIME_COST)
    assert value == _DEFAULT_TIME_COST and warning is not None


# --- collect() + accessor --------------------------------------------------

def test_collect_builds_time_cost(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_time(50)
        def test_slow(): pass
        def test_fast(): pass
    ''')
    sup = Supervisor()
    sup.collect(items)
    assert sup.time_cost[_item(items, "test_slow").nodeid] == 50
    assert sup.time_cost[_item(items, "test_fast").nodeid] == _DEFAULT_TIME_COST
    assert sup._time_for(_item(items, "test_slow").nodeid) == 50


# --- LPT ordering ----------------------------------------------------------

def test_picks_longest_time_first_not_largest_bytes():
    sup = Supervisor()
    # "b" is the LARGER byte fit, so without LPT the byte order picks it. But "a" has the
    # higher aira_time, so LPT picks "a" first. Drop the time primary key and this reds ("b").
    sup.reservation_need = {"a": 100, "b": 300}
    sup.cpu_need = {"a": 1, "b": 1}
    sup.time_cost = {"a": 50, "b": 1}
    sup.queue = ["a", "b"]
    assert sup._largest_fitting(1000, 4, pop=False) == "a"


def test_ties_on_time_fall_back_to_largest_bytes():
    sup = Supervisor()
    # Equal time: the RAM bin-pack secondary key picks the larger bytes ("b").
    sup.reservation_need = {"a": 100, "b": 300, "c": 100}
    sup.cpu_need = {"a": 1, "b": 1, "c": 1}
    sup.time_cost = {"a": 1, "b": 1, "c": 1}
    sup.queue = ["a", "b", "c"]
    assert sup._largest_fitting(1000, 4, pop=False) == "b"


def test_zero_time_marks_is_byte_identical_to_today():
    sup = Supervisor()
    sup.reservation_need = {"a": 100, "b": 300, "c": 200}
    sup.cpu_need = {"a": 1, "b": 1, "c": 1}
    # No time_cost entries -> _time_for defaults to 1 -> order collapses to (need DESC, FIFO),
    # exactly today's behaviour (matches test_supervisor's largest-fitting expectations).
    sup.queue = ["a", "b", "c"]
    assert sup._largest_fitting(250, 4, pop=False) == "c"   # largest need that fits 250
    assert sup._largest_fitting(1000, 4, pop=True) == "b"   # largest need overall
    assert sup.queue == ["a", "c"], "pop removes it"
    assert sup.attempts["b"] == 1, "pop must still apply the crash-retry attempts increment"


def test_fit_filter_runs_before_the_time_order():
    sup = Supervisor()
    # "heavy" has the highest time but exceeds the byte budget -> excluded by the fit filter,
    # so the lighter fitting "light" is picked and never wedged behind the unfittable heavy one.
    sup.reservation_need = {"heavy": 5000, "light": 100}
    sup.cpu_need = {"heavy": 1, "light": 1}
    sup.time_cost = {"heavy": 99, "light": 1}
    sup.queue = ["heavy", "light"]
    assert sup._largest_fitting(1000, 4, pop=False) == "light"
    # cpu filter too: a high-time test needing more cores than the budget is excluded.
    sup.reservation_need = {"heavy": 100, "light": 100}
    sup.cpu_need = {"heavy": 8, "light": 1}
    assert sup._largest_fitting(1000, 4, pop=False) == "light"


def test_fifo_on_equal_time_and_need():
    # The TERTIARY key, pinned by name (the compat guarantee's tie-break, not fixture
    # accident): equal aira_time AND equal reservation_need → the EARLIEST-queued wins.
    sup = Supervisor()
    sup.reservation_need = {"first": 100, "mid": 100, "last": 100}
    sup.cpu_need = {"first": 1, "mid": 1, "last": 1}
    sup.time_cost = {"first": 5, "mid": 5, "last": 5}
    sup.queue = ["first", "mid", "last"]
    assert sup._largest_fitting(1000, 4, pop=False) == "first"
    sup._largest_fitting(1000, 4, pop=True)  # pop "first"
    assert sup._largest_fitting(1000, 4, pop=False) == "mid", "next-earliest at the tie wins"


# --- reader edge cases (AIRA-223 emission + bool/multi-arg) -----------------

def test_collect_emits_the_time_warning_once(pytester, capsys):
    # A malformed aira_time must WARN via collect()'s sys.stderr.write, not merely return a
    # warning string nobody prints. Delete the emission in collect() and this reds.
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_time(0)
        def test_bad(): pass
    ''')
    capsys.readouterr()  # discard collection output
    sup = Supervisor()
    sup.collect(items)
    err = capsys.readouterr().err
    time_lines = [l for l in err.splitlines() if "aira aitest:" in l and "aira_time" in l]
    assert len(time_lines) == 1, time_lines
    assert "default" in time_lines[0]
    assert sup.time_cost[_item(items, "test_bad").nodeid] == _DEFAULT_TIME_COST


def test_bool_arg_is_rejected(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_time(True)
        def test_x(): pass
    ''')
    value, warning = _aira_time_for_item(_item(items, "test_x"), _DEFAULT_TIME_COST)
    assert value == _DEFAULT_TIME_COST and warning is not None


def test_multiple_positional_args_warns(pytester):
    items = _getitems(pytester, '''
        import pytest
        @pytest.mark.aira_time(50, 60)
        def test_x(): pass
    ''')
    value, warning = _aira_time_for_item(_item(items, "test_x"), _DEFAULT_TIME_COST)
    assert value == _DEFAULT_TIME_COST and warning is not None
