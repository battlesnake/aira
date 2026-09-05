---
{"schema":1,"id":"AIRA-100","project":"aira","title":"test-lite-slowbuild's heavy build subprocesses (uv wheel builds, docker build) are structurally outside aitest worker-pool CPU governance","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","confine","dogfood"],"hold":false,"relations":[]}
---
Reported by peer session `split` (fastest-ee, NF-35 relocation work) — related to but distinct from AIRA-64.

## Incident

`make test-lite-slowbuild` OOM-killed 3 times across 3 attempts (progress 100%/41%/73%), all confirmed as pure `systemd-oomd` PSI-pressure kills with 0 test failures each time:
1. `aira confine --memory-max 12G` — hit its own cap during teardown at the 12G peak.
2. `aira confine --memory-max 24G` — killed at 41%, well under its own cap, with 58G machine-wide free (a PSI-pressure kill under real contention, not a hard cap breach).
3. `aira confine --delegate-ram --memory-max 24G` — killed at 73%.

## Two distinct findings

1. **This specific recipe never engages aitest at all.** `Makefile:350` (fastest-ee) is `cd lite && FASTEST_RUN_SLOWBUILD=1 uv run pytest -q` — runs pytest directly with the lite project's own `-n auto` addopts, never shelling `pytest_worker_flags.sh` the way `test-lite`/`test-engine`/`merge_gate` do. This is a fastest-ee-side routing gap, not necessarily an AIRA defect, but worth knowing when reasoning about AIRA-64's real-world coverage: not every aitest-eligible recipe actually reaches aitest today.

2. **The structural point, relevant regardless of (1) being fixed.** This recipe's heaviest work is not pytest workers — it's build subprocesses spawned from inside test bodies (`uv` wheel builds, `docker build` for on-prem-bundle/desktop-matrix/docker-context images). Those are plain `subprocess`/`docker build` spawns, not pytest workers. **aitest's fork+admit worker pool structurally cannot govern them even if this recipe were correctly wired through aitest** — any CPU-slot mechanism hooking into aitest's worker spawn/retire path (e.g. whatever AIRA-64 builds) only ever covers aitest worker CPU, never arbitrary subprocess CPU a test body happens to launch.

## Why this is a separate ticket from AIRA-64

AIRA-64 is scoped to CPU-concurrency governance for aitest's own worker pool. This is a different, broader problem: governing (or at least making admission-aware of) arbitrary heavy subprocess spawns from *inside* a confined job, which aitest's worker-level mechanism cannot reach by construction. Conflating the two would blow AIRA-64's scope.

## Not decided here

split's own framing: "is 'run slowbuild on a quiet slice' the accepted answer, or is this worth wiring a real fix?" Candidates, not scoped or built:
1. Accept as a documented limitation — heavy build-subprocess recipes want a quiet slice, same disposition as AIRA-64's own original (superseded) framing before the owner escalation.
2. A more general mechanism: something inside `aira confine` itself that can account for or bound descendant subprocess CPU/RAM regardless of whether they're pytest workers, aitest workers, or arbitrary build tooling — a genuinely different, likely much larger piece of work than AIRA-64.
3. fastest-ee-side: serialize/queue heavy build-subprocess-spawning recipes specifically, independent of any AIRA-side fix.

Not urgent per split ("not blocking me — I route packaging-closure relocations through the aitest-governed `test_packaging.py` path and accept the full slowbuild cost proportionately"). Recorded so the finding and the scope boundary aren't lost.
