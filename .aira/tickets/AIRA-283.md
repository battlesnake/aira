---
{"schema":1,"id":"AIRA-283","project":"aira","title":"CPU slots per core: persisted ratio (aira install --cpu-slots-per-core=R, default 2) and --aitest-workers=auto resolving to the ceiling","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.29","labels":[],"hold":false,"relations":[]}
---


## Request (deploy, relaying the owner, 2026-10-08)

Owner: "worker count: this would be set on aira, not on pytest invocations." fastest.ee's GCP merge gate runs aira in a container with `aira install --ci=shim` (aira v0.22, sha-pinned); the aitest pool is 1249s of a 1282s gate.

(a) Persisted setting (e.g. `aira install --ci=shim --cpu-slots-per-core=R`) making the daemon CPU ceiling R x NumCPU; default stays 2. Today it is hardcoded in `cpuCeiling()` (internal/daemon/admit.go). Open: how a persisted value reaches a shim daemon started at container entry; per-install state file beside the mode file (confine_mode.go).
(b) `--aitest-workers=auto` resolves to the ceiling (or a documented fraction) instead of `os.cpu_count()`, so callers keep passing `auto`. Open: aitest must ask the daemon; fallback when unreachable = cpu_count; full ceiling vs a fraction (each worker adds the 512M RAM floor, so RAM may bind first).

Evidence gate: deploy is measuring a hard-coded 2 x nproc run first (per-leg timings, whether workers still wait on CPU vs RAM). If RAM is the limit, a higher ratio buys nothing; challenge pass must ask that first. Overlaps AIRA-279 (flag, `many`, jobserver); keep the ceiling ratio here. Daemon admission code => two-loop. On release, send deploy the tag and per-arch sha256.
