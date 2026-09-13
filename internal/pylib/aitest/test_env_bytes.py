"""Unit test for supervisor._env_bytes, the shared byte-count env override.

Relocated from test_outer_cap_guard.py when the v7-1 outer-cap guard was excised
(v0.7 S2a Task 6): _env_bytes SURVIVES the excision — it still backs the per-worker
default size knob (AIRA_AITEST_DEFAULT_BYTES, supervisor.py) — so its parsing
contract keeps a home. The env name here is a real surviving knob rather than the
deleted AIRA_AITEST_OUTER_CAP_* names; _env_bytes is generic over the name."""

import aitest.supervisor as supervisor_module


def test_env_bytes_accepts_bare_zero_and_warns_on_garbage(monkeypatch, capsys):
    name = "AIRA_AITEST_DEFAULT_BYTES"
    # A bare "0" must parse to 0, not silently fall back to the default — a
    # deterministic "no charge" setting a pinning caller relies on.
    monkeypatch.setenv(name, "0")
    assert supervisor_module._env_bytes(name, 8 << 20) == 0
    monkeypatch.setenv(name, "32M")
    assert supervisor_module._env_bytes(name, 0) == 32 << 20
    monkeypatch.delenv(name, raising=False)
    assert supervisor_module._env_bytes(name, 99) == 99  # unset -> default, silent
    assert capsys.readouterr().err == ""
    # A NON-EMPTY typo warns and keeps the default (AIRA-223), never silent.
    monkeypatch.setenv(name, "12x")  # x is not a valid unit
    assert supervisor_module._env_bytes(name, 99) == 99
    assert "not a valid size" in capsys.readouterr().err
