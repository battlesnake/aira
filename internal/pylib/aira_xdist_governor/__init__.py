"""Cooperative cross-process pytest CPU and RAM governor distributed by AIRA."""

import atexit
import ctypes
import fcntl
import gc
import math
import os
import random
import re
import selectors
import subprocess
import sys
import time

import pytest


_DEFAULT_POLL_INTERVAL = 0.75
_DEFAULT_MAX_WAIT = 300.0
_DEFAULT_AFTER_TEST_GC_INTERVAL = 10.0
_DEFAULT_GROWTH_HEADROOM = 512 << 20
_GRANT_READ_GRACE = 2.0
# re.ASCII keeps case-folding ASCII-only, matching Go's byte-wise strings.ToUpper;
# without it re.IGNORECASE would fold Unicode look-alikes (e.g. U+212A KELVIN SIGN
# → "k") that Go rejects, diverging the two parsers.
_MEMORY_SIZE = re.compile(r"^([0-9]+)(?:\.([0-9]+))?(B|KIB|KB|K|MIB|MB|M|GIB|GB|G|TIB|TB|T)?$", re.IGNORECASE | re.ASCII)
_MEMORY_SCALE = {"": 1, "B": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}
_GRANT_LINE = re.compile(r"^granted reserve=([1-9][0-9]*) basis=pinned:client$")
_logged_failures = set()
_held_slot_descriptors = set()
_held_reservation_streams = set()
_standing_reservation = None
_standing_reservation_bytes = 0
_plugin_active = False
_last_after_test_gc = time.monotonic()


def _release_slot(descriptor):
    _held_slot_descriptors.discard(descriptor)
    os.close(descriptor)


def _close_inherited_slots():
    global _standing_reservation, _standing_reservation_bytes
    descriptors = tuple(_held_slot_descriptors)
    _held_slot_descriptors.clear()
    for descriptor in descriptors:
        try:
            os.close(descriptor)
        except OSError:
            pass
    # Reservation stdin is a BufferedWriter (subprocess.PIPE). Close the STREAM
    # OBJECT (its write buffer is always empty — we only close it for EOF, never
    # write), so the inherited copy is released with a single clean fd close and
    # the child's finalizer cannot double-close a since-reused fd.
    streams = tuple(_held_reservation_streams)
    _held_reservation_streams.clear()
    for stream in streams:
        try:
            stream.close()
        except Exception:
            pass
    _standing_reservation = None
    _standing_reservation_bytes = 0


os.register_at_fork(after_in_child=_close_inherited_slots)


def _log_once(message, *, domain="CPU", disabled=True):
    # `disabled=False` is an advisory note where governance CONTINUES (e.g. an
    # invalid marker that falls back to the pinned default); it must NOT claim the
    # governor was disabled (honesty), and is deduped separately so it can never
    # suppress a later genuine "disabled" message.
    key = domain if disabled else domain + ":note"
    if key in _logged_failures:
        return
    _logged_failures.add(key)
    try:
        status = "disabled" if disabled else "note"
        sys.stderr.write("aira %s governor %s: %s\n" % (domain, status, message))
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


def _malloc_trim():
    """Return glibc's free allocator arenas to the OS when available."""
    try:
        ctypes.CDLL("libc.so.6").malloc_trim(0)
    except Exception:
        # This is intentionally best-effort: musl, non-Linux Python builds, and
        # libc implementations without malloc_trim must never affect a test.
        pass


def _collect_and_trim():
    gc.collect()
    _malloc_trim()


def _visible_slots(directory):
    with os.scandir(directory) as entries:
        indexed = []
        for entry in entries:
            if not entry.name.startswith("slot-"):
                raise RuntimeError("unexpected entry in CPU slot population")
            suffix = entry.name[len("slot-") :]
            if not suffix.isdigit() or str(int(suffix)) != suffix:
                raise RuntimeError("malformed CPU slot name")
            if not entry.is_file(follow_symlinks=False):
                raise RuntimeError("CPU slot is not a regular file")
            indexed.append((int(suffix), entry.path))
    if not indexed:
        return []
    indexed.sort()
    if [index for index, _ in indexed] != list(range(len(indexed))):
        raise RuntimeError("incomplete CPU slot population")
    return [path for _, path in indexed]


def _try_slots(paths):
    random.shuffle(paths)
    for path in paths:
        descriptor = os.open(path, os.O_RDWR | os.O_CLOEXEC)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            _held_slot_descriptors.add(descriptor)
            return descriptor
        except BlockingIOError:
            os.close(descriptor)
        except Exception:
            os.close(descriptor)
            raise
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
            _collect_and_trim()
            collected = True
        jittered = poll_interval * random.uniform(0.8, 1.2)
        time.sleep(min(remaining, jittered))


def _parse_memory_size(raw):
    # Mirrors runner.ParseMemorySize exactly: [0-9]+(\.[0-9]+)? with an optional
    # 1024-based unit, case-insensitive, every spelling a synonym (4G == 4GB ==
    # 4GiB). Decimal fractions are floored to whole bytes without float math.
    match = _MEMORY_SIZE.fullmatch(raw) if isinstance(raw, str) else None
    if match is None:
        raise ValueError(
            "memory size must be [0-9]+(\\.[0-9]+)? with an optional K/M/G/T (x i x B) unit"
        )
    unit = (match.group(3) or "").upper()
    scale = unit[0] if unit[:1] in ("K", "M", "G", "T") else ""
    multiplier = _MEMORY_SCALE[scale]
    value = int(match.group(1)) * multiplier
    fraction = match.group(2) or ""
    if fraction:
        value += (int(fraction) * multiplier) // (10 ** len(fraction))
    # The grammar matches runner.ParseMemorySize; the marker deliberately parts
    # from it on ONE point — a zero per-test estimate is meaningless, so "0" (which
    # the Go parser accepts as an unset-cap sentinel) falls to the pinned default here.
    if value <= 0 or value > (1 << 63) - 1:
        raise ValueError("memory size must be a positive int64 byte count")
    return value


def _memory_estimate(item):
    default_raw = os.environ.get("AIRA_TEST_MEM_DEFAULT", "")
    try:
        default = _parse_memory_size(default_raw)
    except Exception as exc:
        _log_once("invalid AIRA_TEST_MEM_DEFAULT %r: %s; running ungoverned" % (default_raw, exc), domain="RAM")
        return None
    marker = item.get_closest_marker("aira_mem")
    if marker is None:
        return default
    try:
        if len(marker.args) != 1 or marker.kwargs:
            raise ValueError("aira_mem requires exactly one positional value")
        return _parse_memory_size(marker.args[0])
    except Exception as exc:
        _log_once("invalid aira_mem marker on %s: %s; using pinned default" % (item.nodeid, exc), domain="RAM", disabled=False)
        return default


def _growth_headroom():
    raw = os.environ.get("AIRA_TEST_MEM_GROWTH_HEADROOM", "")
    if raw == "":
        return _DEFAULT_GROWTH_HEADROOM
    return _parse_memory_size(raw)


def _read_rss_bytes():
    """Read this worker's resident RSS from Linux /proc/self/statm."""
    with open("/proc/self/statm", encoding="ascii") as source:
        fields = source.read().split()
    if len(fields) < 2:
        raise RuntimeError("/proc/self/statm has no resident field")
    resident_pages = int(fields[1])
    if resident_pages < 0:
        raise RuntimeError("/proc/self/statm resident field is negative")
    page_size = os.sysconf("SC_PAGE_SIZE")
    if page_size <= 0:
        raise RuntimeError("SC_PAGE_SIZE is not positive")
    return resident_pages * page_size


def _desired_standing_reservation(item):
    estimate = _memory_estimate(item)
    if estimate is None:
        return None
    # aira_mem is an absolute peak estimate. The worker's cumulative resident
    # footprint is the other candidate, with the configurable transient-growth
    # headroom applied only to that measured path.
    return max(estimate, _read_rss_bytes() + _growth_headroom())


def _read_grant(process, timeout):
    descriptor = process.stdout.fileno()
    os.set_blocking(descriptor, False)
    selector = selectors.DefaultSelector()
    selector.register(descriptor, selectors.EVENT_READ)
    deadline = time.monotonic() + timeout
    payload = bytearray()
    try:
        while b"\n" not in payload and len(payload) <= 4096:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("bounded daemon admission wait elapsed")
            if not selector.select(remaining):
                raise TimeoutError("bounded daemon admission wait elapsed")
            chunk = os.read(descriptor, 4096 - len(payload) + 1)
            if not chunk:
                break
            payload.extend(chunk)
    finally:
        selector.close()
    if b"\n" not in payload or payload.count(b"\n") != 1:
        raise RuntimeError("reserve helper exited without a complete grant")
    line = payload[:-1].decode("utf-8", "strict")
    match = _GRANT_LINE.fullmatch(line)
    if match is None:
        raise RuntimeError("reserve helper returned an invalid grant")
    return int(match.group(1))


def _grant_ready(process):
    selector = selectors.DefaultSelector()
    try:
        selector.register(process.stdout.fileno(), selectors.EVENT_READ)
        return bool(selector.select(0))
    finally:
        selector.close()


def _stop_reservation(process, *, abort=False):
    if process is None:
        return
    stdin = process.stdin
    if stdin is not None:
        _held_reservation_streams.discard(stdin)
        try:
            stdin.close()
        except Exception:
            pass
    if process.stdout is not None:
        try:
            process.stdout.close()
        except Exception:
            pass
    if abort:
        try:
            process.terminate()
        except Exception:
            pass
    try:
        process.wait(timeout=1.0)
    except subprocess.TimeoutExpired:
        process.terminate()
        try:
            process.wait(timeout=1.0)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def _start_reservation(bytes_to_hold):
    command = os.environ.get("AIRA_CONFINE_RESERVE_CMD", "")
    if not command:
        _log_once("AIRA_CONFINE_RESERVE_CMD is unset; running ungoverned", domain="RAM")
        return None
    process = None
    try:
        process = subprocess.Popen(
            [
                command, "confine-reserve", "--bytes", str(bytes_to_hold), "--pinned",
                "--signature", "pytest:worker:" + str(os.getpid()),
                "--max-wait", "%gs" % (_DEFAULT_MAX_WAIT,),
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=None,
            close_fds=True,
        )
        _held_reservation_streams.add(process.stdin)
        if not _grant_ready(process):
            _collect_and_trim()
        granted = _read_grant(process, _DEFAULT_MAX_WAIT + _GRANT_READ_GRACE)
        return process, granted
    except Exception as exc:
        _log_once("%s; running ungoverned" % (exc,), domain="RAM")
        _stop_reservation(process, abort=True)
        return None


def _acquire_standing_reservation(item):
    """Grow the one worker lease, replacing it without an under-reserve gap."""
    global _standing_reservation, _standing_reservation_bytes
    if not os.environ.get("AIRA_TEST_MEM_GOVERNOR"):
        return
    desired = _desired_standing_reservation(item)
    if desired is None or desired <= _standing_reservation_bytes:
        return
    replacement = _start_reservation(desired)
    if replacement is None:
        return
    process, granted = replacement
    previous = _standing_reservation
    # The daemon can clamp an over-ceiling request. Store its actual grant (not
    # the request), and only release the old lease after the new one is live.
    _standing_reservation = process
    _standing_reservation_bytes = granted
    _stop_reservation(previous)


def _release_standing_reservation():
    global _standing_reservation, _standing_reservation_bytes
    reservation = _standing_reservation
    _standing_reservation = None
    _standing_reservation_bytes = 0
    try:
        _stop_reservation(reservation)
    except Exception as exc:
        _log_once(exc, domain="RAM")


atexit.register(_release_standing_reservation)


def pytest_configure(config):
    global _plugin_active
    _plugin_active = True
    config.addinivalue_line(
        "markers", "aira_mem(size): absolute per-test peak RSS estimate, e.g. 4G / 512MB / 1.5GB / 4GiB (1024-based)"
    )


def pytest_sessionfinish(session, exitstatus):
    _release_standing_reservation()


def pytest_unconfigure(config):
    _release_standing_reservation()


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_protocol(item, nextitem):
    """Hold CPU leases and ratchet the optional worker RAM lease."""
    global _last_after_test_gc
    descriptor = None
    try:
        try:
            descriptor = _acquire_slot()
        except Exception as exc:
            _log_once(exc)
        try:
            _acquire_standing_reservation(item)
        except Exception as exc:
            _log_once("%s; running ungoverned" % (exc,), domain="RAM")
        yield
        try:
            interval = _setting(
                "AIRA_TEST_AFTER_TEST_GC_INTERVAL", _DEFAULT_AFTER_TEST_GC_INTERVAL, allow_zero=True
            )
            now = time.monotonic()
            if interval != 0 and now - _last_after_test_gc >= interval:
                _collect_and_trim()
                _last_after_test_gc = now
        except Exception as exc:
            _log_once("%s; skipping periodic post-test gc" % (exc,), domain="GC", disabled=False)
    finally:
        if descriptor is not None:
            try:
                _release_slot(descriptor)
            except Exception as exc:
                _log_once(exc)
