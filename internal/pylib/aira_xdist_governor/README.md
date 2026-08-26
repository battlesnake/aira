# AIRA pytest CPU and RAM governor

This opt-in pytest plugin caps cooperating workers across independent test
sessions using the slot files created by the AIRA daemon. It is advisory and
fail-open: it does not provide fairness, and an unavailable governor never
prevents a test from running.

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

`AIRA_CPU_POLL_INTERVAL` controls the randomized polling interval in seconds
(default `0.75`). `AIRA_CPU_MAX_WAIT` bounds a wait in seconds (default `300`),
after which the item runs ungoverned. Acquisition happens before pytest setup,
call, and teardown, so the wait is excluded from their reported durations. A
thread-method whole-item timeout can still include it; the maximum wait bounds
that advisory edge.

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

The size grammar is exactly `[0-9]+[KMGkmg]?` (`4G`, `512M`, or bare bytes;
not `4GB` or `512MiB`). Unmarked tests use the nonzero pinned
`AIRA_TEST_MEM_DEFAULT`. Reservations are advisory and fail open, while the
delegate suite remains contained by `memory.oom.group` and the finite slice cap.
