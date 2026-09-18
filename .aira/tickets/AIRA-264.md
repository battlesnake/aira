---
{"schema":1,"id":"AIRA-264","project":"aira","title":"Don't learn memory-reserve estimates from failed (non-OOM nonzero-exit) runs","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.19","labels":["confine"],"hold":false,"relations":[]}
---
Owner (2026-09-19): aira must NOT use ram/cpu/time measurements from a FAILED run (nonzero exit) to adjust limits for subsequent runs. Owner decision: KEEP OOM kills (exit 137) + clean successes; EXCLUDE only non-OOM workload failures (nonzero exit, crashes, timeouts, Ctrl-C).

## Finding (7-reader audit)
Only RAM is learned into limits today. @aira_cpu/@aira_time are DECLARED, not learned (cpu/time auto-learn deferred v0.18+) — so this rule has nothing to bite on for cpu/time now; set the convention that those learned paths carry the run outcome from day one when built.

Two RAM learners both currently ingest non-OOM nonzero-exit runs:
- STORE A (PRIMARY, machine-wide daemon admission): confine_peak_history. Written from every confine teardown at internal/runner/confine_linux.go:1393 (reportPeak), guarded ONLY by signature!="". The ConfinePeakReport + confine-report wire (admission_linux.go:769) + table (store.go:873) carry only {peak_rss, oom} — exit code is IN SCOPE at the record site (confine_linux.go:1339/1345) but dropped. Readers ConfinePeakHistory→EstimateMemoryReserve (next-run reserve) + ConfinePeakP90 (machine-wide prior) aggregate over ALL peak_rss>0 rows, no outcome filter.
- STORE B (secondary, opt-in aira run via c.memoryEstimate): runs table, reader internal/runner/peak_rss_history.go:40 filters status IN ('exited','oom-killed') — excludes killed/cancelled/lost, but 'exited' includes any exit code (runner_linux.go:915: nonzero exit stays StatusExited, tagged E_RUN_FAILED). exit_code lives in record_json, not a projected column.

## OOM must be kept (self-heal)
OOM (SIGKILL/137) is nonzero-exit but resource-meaningful: admit.go:1339 escalates next reserve to 1.5×MaxOOMPeak; EstimateMemoryReserve bumps to headroom on OOMCount>0. Real-cgroup fixtures assert this chain. So the filter is OUTCOME-based, not exit-code-based.

## Fix
- Store A: send-side gate at confine_linux.go teardown — record the peak only when `oom || (clean normal exit AND exitCode==0)`; skip reportPeak for non-OOM failures. No wire/schema change. (Note: also drops failed runs from the advisory --budget gauge, which is fine/consistent.)
- Store B: tighten the reader so a nonzero-exit 'exited' row doesn't count (keep 'oom-killed'); via json_extract(record_json,'$.exit_code')=0 or a projected exit_code column — pick the simplest that the write site supports.
- Regression tests at each hop; OOM-escalation tests stay green. Verify: a non-OOM nonzero-exit confine run leaves NO confine_peak_history row; a clean run + an OOM run both DO.

Two-loop: Opus builds TDD, Fable reviews (detached worktree).
