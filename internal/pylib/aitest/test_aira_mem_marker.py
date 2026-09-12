"""AIRA-230 (v0.7 S1, slice v7-2): the aira_mem marker reader + nodeid->bytes map.

The reader is a DOCUMENTED-INERT foundation in S1 (plan D5): the map is built at
collection but has no admission consumer yet -- per-class worker sizing reads it
in S2. These tests pin the reader's contract (the shared size grammar, the
bare-int str() path, the AIRA-223 warn-once-never-silent discipline, the
registration-before-the-early-return ordering, and get_closest_marker
closest-wins) so S2 can build on a stable, tested primitive.
"""

import pytest

from aitest import _aira_mem_bytes_for_item
from aitest.supervisor import Supervisor, _DEFAULT_ANNOTATION_BYTES


def _getitems(pytester, source):
    """Collect real pytest Item objects for `source`, with the aitest plugin
    registered in the inner session so @pytest.mark.aira_mem is a registered
    marker there (dogfoods the registration path and avoids pytest's own
    unknown-mark warning polluting captured stderr)."""
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    return pytester.getitems(source)


def _nid(items, name):
    """The nodeid of the collected item whose test function is `name`."""
    for item in items:
        if item.nodeid.endswith("::" + name):
            return item.nodeid
    raise KeyError("no collected item named %r in %r" % (name, [i.nodeid for i in items]))


def _aira_mem_warning_lines(err):
    """The stderr lines that are THIS reader's own aira_mem warning (AIRA-223),
    as distinct from any other 'aira aitest:' line (e.g. a malformed
    AIRA_AITEST_DEFAULT_BYTES) or pytest's own unknown-mark warning."""
    return [line for line in err.splitlines() if "aira aitest:" in line and "aira_mem" in line]


def test_size_string_args_parse_to_bytes(pytester):
    """The three canonical size-string forms all parse through the SAME grammar
    as AIRA_AITEST_ESTIMATED_BYTES (_parse_estimated_bytes). This is also the
    v7-2 MUTATION witness: dropping the parse-and-store line (every nodeid ->
    default) reds these annotated assertions, since none equals the default."""
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem("512M")
        def test_half_gig():
            pass

        @pytest.mark.aira_mem("1.5G")
        def test_one_and_a_half():
            pass

        @pytest.mark.aira_mem("512MiB")
        def test_mib():
            pass
    ''')
    sup = Supervisor()
    sup.collect(items)
    m = sup.aira_mem_bytes

    assert m[_nid(items, "test_half_gig")] == 512 << 20
    assert m[_nid(items, "test_one_and_a_half")] == (3 << 30) // 2  # 1.5 GiB floored
    assert m[_nid(items, "test_mib")] == 512 << 20
    # None of these annotated values may coincide with the default, or a
    # "silently fell to default" mutant would pass this test (porous).
    assert _DEFAULT_ANNOTATION_BYTES not in (512 << 20, (3 << 30) // 2)


def test_bare_int_arg_is_bytes(pytester):
    """spec 4.1: 'a bare int is bytes'. _parse_estimated_bytes strips a STRING,
    so a bare-int marker argument must be str()-ed before parsing -- dropping
    the str() raises AttributeError on .strip(). 1 GiB as a bare int is chosen
    so it cannot be confused with the 256 MiB default."""
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem(1073741824)  # 1 GiB, as a bare int
        def test_bare_int():
            pass
    ''')
    sup = Supervisor()
    sup.collect(items)

    assert sup.aira_mem_bytes[_nid(items, "test_bare_int")] == 1 << 30


def test_unannotated_item_takes_the_default(pytester):
    items = _getitems(pytester, '''
        def test_plain():
            pass
    ''')
    sup = Supervisor()
    sup.collect(items)

    assert sup.aira_mem_bytes[_nid(items, "test_plain")] == _DEFAULT_ANNOTATION_BYTES


def test_default_bytes_env_override_uses_the_shared_grammar(pytester, monkeypatch):
    """AIRA_AITEST_DEFAULT_BYTES overrides the 256 MiB default via the SAME size
    grammar (here 128M != the default, so a no-op mutant of the override is
    caught)."""
    monkeypatch.setenv("AIRA_AITEST_DEFAULT_BYTES", "128M")
    items = _getitems(pytester, '''
        def test_plain():
            pass
    ''')
    sup = Supervisor()
    sup.collect(items)

    assert sup.aira_mem_bytes[_nid(items, "test_plain")] == 128 << 20


def test_no_argument_marker_warns_once_and_defaults(pytester, capsys):
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem()
        def test_noarg():
            pass
    ''')
    capsys.readouterr()  # discard anything the inline collection produced
    sup = Supervisor()
    sup.collect(items)

    assert sup.aira_mem_bytes[_nid(items, "test_noarg")] == _DEFAULT_ANNOTATION_BYTES
    lines = _aira_mem_warning_lines(capsys.readouterr().err)
    assert len(lines) == 1, lines
    assert "default" in lines[0]


def test_extra_argument_marker_warns_once_and_defaults(pytester, capsys):
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem("512M", "1G")
        def test_two_args():
            pass
    ''')
    capsys.readouterr()
    sup = Supervisor()
    sup.collect(items)

    assert sup.aira_mem_bytes[_nid(items, "test_two_args")] == _DEFAULT_ANNOTATION_BYTES
    lines = _aira_mem_warning_lines(capsys.readouterr().err)
    assert len(lines) == 1, lines


def test_keyword_argument_marker_warns_and_defaults(pytester, capsys):
    """A kwarg form (aira_mem(size="512M")) has zero POSITIONAL args, so it hits
    the arity branch; the warning names kwargs so the user is not left confused
    about why a value that 'looks set' was ignored. Kwargs are deliberately NOT
    accepted (not in the plan)."""
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem(size="512M")
        def test_kwarg():
            pass
    ''')
    capsys.readouterr()
    sup = Supervisor()
    sup.collect(items)

    assert sup.aira_mem_bytes[_nid(items, "test_kwarg")] == _DEFAULT_ANNOTATION_BYTES
    lines = _aira_mem_warning_lines(capsys.readouterr().err)
    assert len(lines) == 1, lines
    assert "keyword" in lines[0].lower()


def test_invalid_size_string_warns_once_and_defaults(pytester, capsys):
    """'4X' is genuinely unparseable. (NOTE: the plan's example '4GB' is NOT --
    _SIZE_UNITS has GB == 1<<30, so '4GB' is a valid 4 GiB; see the report's
    deviation note. '4X' is in _parse_estimated_bytes's own known-malformed
    set.)"""
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem("4X")
        def test_bad_unit():
            pass
    ''')
    capsys.readouterr()
    sup = Supervisor()
    sup.collect(items)

    assert sup.aira_mem_bytes[_nid(items, "test_bad_unit")] == _DEFAULT_ANNOTATION_BYTES
    lines = _aira_mem_warning_lines(capsys.readouterr().err)
    assert len(lines) == 1, lines
    assert "4X" in lines[0]


def test_get_closest_marker_is_closest_wins_class_vs_function(pytester):
    """A function-level aira_mem overrides a class-level one (get_closest_marker
    semantics), and an unmarked method in the same class inherits the class
    marker. Values are 1G (class) / 512M (function), both distinct from the
    256 MiB default, so neither assertion can pass by accidentally reading the
    default."""
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem("1G")
        class TestBig:
            @pytest.mark.aira_mem("512M")
            def test_override(self):
                pass

            def test_inherits(self):
                pass
    ''')
    sup = Supervisor()
    sup.collect(items)
    m = sup.aira_mem_bytes

    assert m[_nid(items, "test_override")] == 512 << 20   # function marker wins
    assert m[_nid(items, "test_inherits")] == 1 << 30      # inherits class marker


def test_strict_markers_suite_stays_green_under_a_plain_pytest_run(pytester):
    """The ordering pin: the marker must be registered BEFORE pytest_configure's
    early return, so a plain `pytest` run (no --aitest-workers, which hits that
    early return) on an aira_mem-annotated suite stays green under
    --strict-markers. If the registration moves after the early return, this run
    returns before registering the marker and --strict-markers rejects
    @pytest.mark.aira_mem -> ret != 0."""
    pytester.makeconftest('pytest_plugins = ("aitest",)')
    pytester.makepyfile('''
        import pytest

        @pytest.mark.aira_mem("512M")
        def test_ok():
            assert True
    ''')

    result = pytester.runpytest("--strict-markers")

    assert result.ret == 0, "a --strict-markers run on an aira_mem suite must be green"


def test_helper_returns_a_warning_for_a_malformed_marker_and_none_otherwise(pytester):
    """_aira_mem_bytes_for_item's (bytes, warning) contract directly: a valid
    marker and an unannotated item both return warning=None; a malformed marker
    returns default plus a ready-to-write warning string. This is the unit the
    collect() loop writes at most once per nodeid."""
    items = _getitems(pytester, '''
        import pytest

        @pytest.mark.aira_mem("512M")
        def test_good():
            pass

        def test_plain():
            pass

        @pytest.mark.aira_mem("4X")
        def test_bad():
            pass
    ''')
    by_name = {item.nodeid.rsplit("::", 1)[-1]: item for item in items}
    default = 256 << 20

    value, warning = _aira_mem_bytes_for_item(by_name["test_good"], default)
    assert value == 512 << 20 and warning is None

    value, warning = _aira_mem_bytes_for_item(by_name["test_plain"], default)
    assert value == default and warning is None

    value, warning = _aira_mem_bytes_for_item(by_name["test_bad"], default)
    assert value == default
    assert warning is not None and "aira_mem" in warning
