import pytest

pytest_plugins = ["pytester"]


@pytest.fixture(autouse=True)
def _no_inherited_batch_cap(monkeypatch):
    """AIRA-372: aira's own aitest tests are hermetic to an exported
    AIRA_AITEST_BATCH_MAX_TESTS (an A/B run sets it for a whole shell). Tests that
    want batching set it themselves."""
    monkeypatch.delenv("AIRA_AITEST_BATCH_MAX_TESTS", raising=False)
