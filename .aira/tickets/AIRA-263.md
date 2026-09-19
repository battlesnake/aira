---
{"schema":1,"id":"AIRA-263","project":"aira","title":"Clarify @aira_cpu docstring: N = real CPU cores demanded, not concurrency slots","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":"v0.19","labels":["aitest"],"hold":false,"relations":[]}
---
The aira_cpu registration + reader docstrings (internal/pylib/aitest/__init__.py) say "peak internal fork width" / "peak CPU demand IS N cores" — correct, but they don't spell out the GIL/lock-wait EXCLUSION, and that ambiguity bit deploy (a careful reader) when marking the FIRST real @aira_cpu tests.

Canonical semantic (confirmed to deploy 2026-09-18, who pasted it into fastest.ee's pytest.ini): N = the peak count of execution streams (processes OR threads) SIMULTANEOUSLY RUNNABLE AND CPU-BOUND, each genuinely occupying a core = real peak CPU demand. NOT a concurrency-slot request. Workers that are GIL-serialised or BLOCKED WAITING on a lock (e.g. 16 SQLite BEGIN IMMEDIATE racers — one holds the write lock, 15 sleep on busy_timeout) are ~1 core → N=1 / no mark. Marking such a test higher over-declares phantom cores → starves its own admission + displaces N-1 cores of siblings (the over-reservation failure the ledger design guards against).

FIX (docstring-only, drop-in): tighten the aira_cpu addinivalue_line registration string AND _aira_cpu_cores_for_item docstring to state the CPU-bound-only / exclude-GIL-and-lock-waiters rule explicitly. No behaviour change.

BATCH: ride the next aitest change (batch-small-tasks-per-release) — do NOT cut a standalone release for a docstring. Ping deploy when it ships so their pytest.ini stays in sync (they were told "later drop-in").
