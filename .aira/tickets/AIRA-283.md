---
{"schema":1,"id":"AIRA-283","project":"aira","title":"CPU slots per core: persisted ratio (aira install --cpu-slots-per-core=R, default 2) and --aitest-workers=auto resolving to the ceiling","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":"v0.29","labels":[],"hold":false,"relations":[]}
---


## Request (deploy, relaying the owner, 2026-10-08)

Owner: "worker count: this would be set on aira, not on pytest invocations." fastest.ee's GCP merge gate runs aira in a container with `aira install --ci=shim` (aira v0.22, sha-pinned); the aitest pool is 1249s of a 1282s gate.

(a) Persisted setting (e.g. `aira install --ci=shim --cpu-slots-per-core=R`) making the daemon CPU ceiling R x NumCPU; default stays 2. Today it is hardcoded in `cpuCeiling()` (internal/daemon/admit.go). Open: how a persisted value reaches a shim daemon started at container entry; per-install state file beside the mode file (confine_mode.go).
(b) `--aitest-workers=auto` resolves to the ceiling (or a documented fraction) instead of `os.cpu_count()`, so callers keep passing `auto`. Open: aitest must ask the daemon; fallback when unreachable = cpu_count; full ceiling vs a fraction (each worker adds the 512M RAM floor, so RAM may bind first).

Evidence gate: deploy is measuring a hard-coded 2 x nproc run first (per-leg timings, whether workers still wait on CPU vs RAM). If RAM is the limit, a higher ratio buys nothing; challenge pass must ask that first. Overlaps AIRA-279 (flag, `many`, jobserver); keep the ceiling ratio here. Daemon admission code => two-loop. On release, send deploy the tag and per-arch sha256.

## Triage 2026-10-12 (goal: clear remaining known issues)

**Disposition:** done. **State on master v0.34:** already-fixed.

Nothing left to build: (a) is merged and live; (b) was deliberately dropped by the owner on evidence. Only action is bookkeeping: close the ticket as done (status planned -> done) with a note that part b was dropped per owner option A.

**Evidence:** Part (a) shipped via PR #189 (merge d916ddc, commits 0ccdd80 etc.): `aira install --cpu-slots-per-core=R` (internal/install/install.go:455-473, cmd/aira/main.go:994), persisted in mode file (internal/install/mode.go), read once in Serve via resolveCPUSlotsPerCore (internal/daemon/shim.go:291, server.go:352), and cpuCeiling() = ratio x NumCPU (internal/daemon/admit.go:2568-2574); tests internal/daemon/cpu_slots_per_core_test.go, internal/install/cpu_slots_per_core_test.go. Part (b) (--aitest-workers=auto resolving to the ceiling) was implemented then REVERTED (6dcb93e, 3d481e7) by owner option A on 2026-10-09 after deploy measured no gain from 2 workers/CPU; spec v3 (docs/superpowers/specs/2026-10-08-aira-283-cpu-slots-per-core-design.md:3) records part b DROPPED and the skill states auto ignores the ratio. The ticket file .aira/tickets/AIRA-283.md still says status "planned", milestone v0.29: stale, never closed.
