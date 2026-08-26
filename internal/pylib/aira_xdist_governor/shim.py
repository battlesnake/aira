"""Optional annotation shim for projects that do not require the AIRA plugin."""


def aira_mem(size):
    """Return an active pytest marker only when the plugin is registered."""
    from . import _plugin_active

    if _plugin_active:
        import pytest

        return pytest.mark.aira_mem(size)

    def inert(function):
        return function

    return inert
