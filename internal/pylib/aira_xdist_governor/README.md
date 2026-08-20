# AIRA pytest CPU governor

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
