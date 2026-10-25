---
{"schema":1,"id":"AIRA-318","project":"aira","title":"Peak-history estimator learns from cgroup memory.peak, which counts page cache: a cache-heavy job ratchets its reserve 15% per run","status":"planned","kind":"bug","severity":"P3","assignee":null,"milestone":"later","labels":["admission","confine"],"hold":true,"relations":[]}
---
Captured from the AIRA-314 plan review/gate (2026-10-25). Pre-existing, not built by AIRA-314.

The per-signature estimator (estimate:max, and AIRA-314's estimate:own-max floor) is fed peak_rss = the scope's memory.peak (internal/runner/usage_linux.go readCgroupUsage). memory.peak counts charged page cache, and the kernel holds memory.current at memory.max by reclaim, so a run that reads many files records a peak near its cap however little anonymous memory it needs. The next estimate is 1.15x that peak, the next run's cap is that, its peak lands at the new cap, and so on: +15% per run toward E_ADMIT_TOO_LARGE. An exit-0 row proves the job fit within its cap, not that the peak is uncensored.

Measured by the AIRA-314 plan review on a copy of the live state.db: 897 of 65,863 unpinned clean rows sit at >=95% of their daemon-reserve cap (the n>=3 ratchet today); of the signatures AIRA-314's floor raises at 1-2 samples, about 258 have their largest clean peak at >=90% of that run's cap (most from --memory-reserve pins). Reproduction: docs/dev/aira-314-p90-index-measurement-repro.sh (at-cap counts). AIRA-314 brings this ratchet in two runs earlier (from n=1, starting from a pinned figure); it does not create it.

Out of scope for AIRA-314; a later design question (e.g. estimate from anonymous memory / memory.stat anon+kernel, or discount a peak at the cap). Owner decision before any build.

Example: pinned --memory-reserve 16G cache-heavy job, real anonymous need 2G, records peak 16G. With AIRA-314, run 2 gets 18.4G, run 3 21.2G, ...; before AIRA-314 the same climb starts at run 4.
