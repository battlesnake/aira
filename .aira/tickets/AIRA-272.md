---
{"schema":1,"id":"AIRA-272","project":"aira","title":"aitest Phase C: measured-profile estimation — feed prior-run peak_rss into _need_for and wall-time into _time_for (priority mark \u003e measured-profile \u003e default)","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["aitest","scheduler"],"hold":true,"relations":[]}
---
Follow-on to AIRA-261 (done), which shipped the DECLARED-mark consumers ONLY: _need_for = declared @aira_mem else worker overhead (supervisor.py:978), _cpu_need_for = declared @aira_cpu else default (:984), _time_for = declared @aira_time else 1 (:990). The AIRA-260 design envisioned mark -> MEASURED prior-run -> neutral, but the measured tier was deferred.

PROBLEM (measured by deploy 2026-09-20, fastest-ee full merge-gate on n4a-highcpu 2GB/vCPU): with no @aira_time AND no @aira_mem, _time_for == 1 for all tests -> the LPT primary key is constant -> collapses to (need DESC, FIFO); and need == overhead for all -> pure FIFO (pytest collection order). The engine aitest pool then packs in collection order, not size/time order -> slow ramp + non-deterministic plateau (hc-64: ramped to ~50 workers over 40% of span, plateaued 54/64; hc-16: core-util collapsed 82->52% under RAM pressure). std (4GB/vCPU) hides it; highcpu (2GB/vCPU) exposes it. Phase-B LPT+BFD is correct but DORMANT without any ranking signal.

PHASE C = wire a MEASURED profile as the middle tier (mark > measured-profile > default) in _need_for / _cpu_need_for / _time_for:
- profile = map nodeid -> {peak_rss_bytes, wall_seconds, cpu_cores}, supplied as a LOCAL file. deploy generates it from its own trace archive / prior run; AIRA never reads GCS (same constraint as the deferred @aira_time duration file). aira reads a local path (config/flag/env).
- _time_for: mark else profile.wall else 1. _need_for: overhead + (mark else (profile.peak - overhead) else 0). The 2-D fit + LPT order are UNCHANGED; only the estimate SOURCE gains a tier.
- The AIRA-259 trace (AIRA_AITEST_MEASURE_DIR) already records peak_rss_bytes (supervisor.py:2319) + per-worker spans; the gaps are (a) per-TEST attribution (a worker is recycled across tests) and (b) feeding it back. Ownership: deploy generates the per-nodeid profile; aira reads it + applies the tier.

WHERE THE PROFILE LIVES (deploy asked): committed per-suite manifest vs run-dir cache. Recommend a deploy-supplied local file (committed per-suite manifest preferred) over a run-dir cache: reproducible + deterministic on a FRESH CI runner (directly fixes deploy hc-64 non-determinism), reviewable, diffable, matches AIRA git-file-content ethos. A cache is zero-maintenance but cold-FIFO on the first run of every fresh runner. aira just reads a path; deploy owns freshness.

PARTIAL-ANNOTATION GOTCHA (path 1, mark only slow tests): aira_time is ORDER-ONLY (reserves nothing, gates no admission — supervisor.py:989), so partial @aira_time is SAFE and activates LPT for the tail immediately. BUT the highcpu RAM-PRESSURE packing (hc-16/64) is a _need_for (RAM-fit) problem, NOT ordering — partial aira_time alone will NOT fix it; it needs @aira_mem or measured peak_rss. Partial @aira_mem UNDER-declares (unannotated -> flat overhead floor) -> real-pressure/OOM risk, so measured peak_rss (this ticket) is the safer RAM-axis fix.

PRECEDENT (not reused per-test): the confine side already learns a per-signature reserve from peak-RSS history (confine_peak_history, #67) — but keyed per confine-command-signature, machine-wide, NOT per aitest nodeid.

HELD pending: (a) deploy profile-file contract (format + generation + local path), (b) owner prioritization, (c) challenge/simplify + two-loop (touches the dispatch/pick loop -> Opus builds, Fable reviews).
