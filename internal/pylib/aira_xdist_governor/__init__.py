"""Cooperative cross-process pytest CPU governor distributed by AIRA."""

import fcntl
import gc
import math
import os
import random
import sys
import time

import pytest


_DEFAULT_POLL_INTERVAL = 0.75
_DEFAULT_MAX_WAIT = 300.0
_logged_failure = False


def _log_once(message):
    global _logged_failure
    if _logged_failure:
        return
    _logged_failure = True
    try:
        sys.stderr.write("aira CPU governor disabled: %s\n" % (message,))
    except Exception:
        pass


def _setting(name, default, *, allow_zero):
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    value = float(raw)
    if not math.isfinite(value) or value < 0 or (value == 0 and not allow_zero):
        raise ValueError("%s must be a finite %s number" % (
            name,
            "non-negative" if allow_zero else "positive",
        ))
    return value


def _visible_slots(directory):
    with os.scandir(directory) as entries:
        return [
            entry.path
            for entry in entries
            if entry.name.startswith("slot-") and entry.is_file(follow_symlinks=False)
        ]


def _try_slots(paths):
    random.shuffle(paths)
    for path in paths:
        # O_CLOEXEC prevents an exec'ed descendant retaining the lock. A
        # multiprocessing fork can still inherit it and pin the advisory slot
        # beyond the item; callers should avoid forking while an item is gated.
        descriptor = os.open(path, os.O_RDWR | os.O_CLOEXEC)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return descriptor
        except BlockingIOError:
            os.close(descriptor)
    return None


def _acquire_slot():
    directory = os.environ.get("AIRA_CPU_SLOTS_DIR")
    if not directory:
        return None
    poll_interval = _setting(
        "AIRA_CPU_POLL_INTERVAL", _DEFAULT_POLL_INTERVAL, allow_zero=False
    )
    max_wait = _setting("AIRA_CPU_MAX_WAIT", _DEFAULT_MAX_WAIT, allow_zero=True)
    deadline = time.monotonic() + max_wait
    collected = False
    while True:
        paths = _visible_slots(directory)
        if not paths:
            return None
        descriptor = _try_slots(paths)
        if descriptor is not None:
            return descriptor
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            _log_once("maximum slot wait elapsed; running ungoverned")
            return None
        if not collected:
            gc.collect()
            collected = True
        jittered = poll_interval * random.uniform(0.8, 1.2)
        time.sleep(min(remaining, jittered))


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_protocol(item, nextitem):
    """Hold one kernel flock across setup, call, and teardown for an item."""
    descriptor = None
    try:
        try:
            descriptor = _acquire_slot()
        except Exception as exc:
            _log_once(exc)
        yield
    finally:
        if descriptor is not None:
            try:
                os.close(descriptor)
            except Exception as exc:
                _log_once(exc)
