# AIRA pytest scheduler and RAM governor

This opt-in pytest plugin cooperates with AIRA's daemon-owned scheduler across
independent test sessions. It is advisory and fail-open: if the scheduler or
its relay is unavailable, tests continue ungoverned rather than being blocked.

Add this import-time-safe snippet to the project's `conftest.py`:

```python
try:
    import importlib
    import os
    import sys

    if aira_py_lib := os.environ.get("AIRA_PY_LIB"):
        sys.path.insert(0, aira_py_lib)
        importlib.import_module("aira_xdist_governor")
        pytest_plugins = ("aira_xdist_governor",)
except Exception:
    pass
```

With `AIRA_PY_LIB` unset, the snippet is a no-op. The broad exception handler
is intentional: extraction, importing, and registration must all fail open.

## Daemon scheduler relay

When `AIRA_GOVERNOR` is not `off` and `AIRA_GOVERNOR_CMD` is set, each pytest
worker starts the command as `governor-slot`. The relay acquires the worker's
daemon scheduler connection before `pytest_runtest_protocol` yields, then sends
roughly ten-second checkpoints between tests. A scheduler park therefore occurs
before pytest setup/call/teardown timers begin and before the plugin reserves
RAM for the next test.

`AIRA_CONFINE_SCOPE_ID` identifies the confined scope used by the relay. The
normal AIRA launcher sets `AIRA_GOVERNOR`, `AIRA_GOVERNOR_CMD`, and this scope
ID; `AIRA_GOVERNOR=off` is the explicit kill switch.

`AIRA_GOVERNOR_MAX_WAIT` is a Go duration accepted by the relay, with a
generous default of `300s`. It bounds every daemon reply, including initial
worker acquisition and checkpoints. Invalid or non-positive values use the
compiled `300s` default. If it elapses, or the daemon fails, the relay emits
`continue` immediately and attempts future reconnects only in the background.
The worker proceeds ungoverned for that interval instead of hanging the suite.
This also means a worker parked for longer than the bound becomes ungoverned;
that is expected to be rare because min-share rotation normally reaches parked
workers within the active workers' test durations.

## RAM-weighted suites

For RAM-weighted suites, launch under a small pinned framework reserve and
delegate per-test RAM admission:

```sh
aira confine --delegate-ram --memory-reserve 512M -- pytest -n 16
```

Annotate heavy tests with the optional shim; it is inert when this plugin is
not registered:

```python
from aira_xdist_governor.shim import aira_mem

@aira_mem("4G")
def test_large_corpus():
    ...
```

The size is `[0-9]+` with an optional 1024-based unit — `K`/`KB`/`KiB`,
`M`/`MB`/`MiB`, `G`/`GB`/`GiB`, `T`/`TB`/`TiB`, or a bare `B` —
case-insensitive, every spelling a synonym (`4G` == `4GB` == `4GiB`). Integer
only (write `1536M`, not `1.5G`). Unmarked tests use the nonzero pinned
`AIRA_TEST_MEM_DEFAULT`. The marker is an absolute peak-RSS estimate. For each
test the worker reads `/proc/self/statm` and holds one reservation sized to
`max(aira_mem, RSS + growth_headroom)`: the default growth headroom is 512 MiB
and `AIRA_TEST_MEM_GROWTH_HEADROOM` overrides it with the same size syntax.
The reservation is released after the test. Reservations are advisory and fail
open, while the delegate suite remains contained by `memory.oom.group` and the
finite slice cap.
