---
{"schema":1,"id":"AIRA-281","project":"aira","title":"confine --summary-file \u003cpath\u003e: append one JSON line per job (name, owner, cmd/hash, exit, ran, admission, terminated-by, peak-rss, reserve, wall/cpu, optional tree hash)","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.29","labels":[],"hold":false,"relations":[{"kind":"blocks","from":"AIRA-281","to":"AIRA-282"}]}
---


## Request (field/Stoner, 2026-10-08; blocks Stoner + flavour Makefile CI)

Foreground `confine --json` is refused and the trailer is one text line of `key=value` facets, so Makefiles must sed it. Add `--summary-file <path>`: append ONE JSON line per job (append-safe under `make -j`), with at least: name, owner, command (or its hash), exit code, ran, admission, terminated-by, peak-rss, reserve, wall time, cpu time, and an optional caller-supplied tree hash. A never-ran job (refused admission) still writes a line (ran=false). Unreadable fields are `unevaluated`, never 0. Challenge first: is the greenfield minimum just a JSON rendering of the existing trailer facets? Resumable-gate caching stays in the repo (aira does not replay passes).

## Addition (spice/flavour CI via field, 2026-10-08)

In `aira install --ci=shim` mode inside a container the trailer reports peak-rss, cpu and cap as `unevaluated` (terminated-by=normal), and `aira confine --dump` wrote 0 records in spice's container test, so in CI there is nothing to summarise. Asks: (1) peak memory and wall time per step in ci-shim mode (peak may need a no-cgroup source, e.g. rusage of the waited child; else honest `unevaluated`); (2) wall time in the trailer generally (a `wall=` facet, all modes); (3) the shared confine macro include (AIRA-282). spice keeps make concurrency low itself until CPU gating (AIRA-279) exists. Check whether the shim `--dump` emitting 0 records is the same defect as AIRA-280.

## Shim peak decision (field, 2026-10-08)

ci-shim peak-rss is `unevaluated` BY DESIGN (confine_shim_linux.go:509-524, AIRA-121 gate C10): wait4 `ru_maxrss` is the largest single process, not the simultaneous tree total like cgroup `memory.peak`, so it under-reads parallel builds, and must never enter the cgroup-derived reserve history. For this ticket: include wait4 wall and cpu in shim mode, and optionally `maxrss` as a SEPARATE, explicitly labelled field (e.g. `maxrss-largest-process`), never reported as the tree peak and never fed to reserve learning. Owners should size caps from a cgroup-mode run.
