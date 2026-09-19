---
{"schema":1,"id":"AIRA-264","project":"aira","title":"Don't learn memory-reserve estimates from failed (non-OOM nonzero-exit) runs","status":"in-progress","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.19","labels":["confine"],"hold":false,"relations":[]}
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

## Build record (Opus, 2026-09-19)
**Store A (PRIMARY).** Extracted a pure predicate `shouldRecordConfinePeak(oom, term, supervisorSignal, exitCode)` (confine_linux.go) = `oom || (term.Decoded && !term.Signaled && supervisorSignal==nil && exitCode==0)`, and gated the reportPeak call at confine_linux.go:1383 on it (`signature!="" && shouldRecordConfinePeak(...)`). The clean arm is EXACTLY `classifyConfineTermination(...)==ConfineTerminatedNormal && exitCode==0`: when !Signaled, `killed` is false so the OOM/deadline/unattributed-SIGKILL arms cannot fire — proven by an oracle table test that cross-checks the predicate against the classifier across all usage/deadline values, so the two cannot drift. No wire/schema change; all values already in scope at the report site (deadline classification is computed later and is not needed). Kept `!term.Signaled` explicitly (redundant with exit==0 only via waitConfineCommand's 128+sig encoding) so the predicate doesn't secretly depend on that encoding; pinned by a synthetic "signalled with a zero exit is still excluded" oracle row.

**Store B (secondary).** Chose a PROJECTED `exit_code` INTEGER column over `json_extract` (no json_extract precedent in the codebase; `project()` at ledger.go:620 is a FULL REBUILD that replays all events, so the column back-fills from record_json for historical rows on the next projection — old clean successes stay usable, old failures/unknown-exit rows are excluded). Added to the ensureRunColumn map + INSERT/ON-CONFLICT + a `nullableInt(*int)` helper (RunRecord.ExitCode is *int). Reader (peak_rss_history.go) usable-sample predicate tightened to `(status='exited' AND exit_code=0) OR status='oom-killed'`; a NULL exit_code is conservatively excluded (exit_code=0 is false for NULL). OOMCount + TotalCount unchanged.

**Verification.** 4 mutations each confirmed to RED the intended test and reverted: (1) delete the call-site guard → call-site test reds; (2) drop `!term.Signaled` → oracle test reds (via the synthetic row); (3) revert the reader to `status IN (...)` → filter test reds (SampleCount 2→4, PeakMax 250→6000); (4) drop the projection exit_code write → filter test reds (clean row NULL→excluded, SampleCount 2→1). Full runner package green. **The OOM self-heal real-cgroup tests RAN FOR REAL on this box (not skipped) and PASS** — `TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission` exercises the OOM→confine_peak_history→escalation chain through the real confine teardown that now carries the gate, so OOM learning is preserved end-to-end (measured, not inferred from make ci).

## Two behaviour consequences (correct, documented — not regressions)
- **The machine-wide p90 prior pool shrinks.** ConfinePeakP90 needs ≥3 rows/signature; a signature that mostly FAILS (e.g. a make ci that's been red all week) now drops below 3 and leaves the pool, so on a dogfood box a new command's cold-start prior can move or fall to `fallback:no-history`. This is exactly "don't learn a prior from failed runs."
- **`aira confine --budget` also loses failed-run samples** (same confine_peak_history rows). Advisory-only (adjusts no limit), and resource_budget.go already partitions on the OOM bit for the lowering direction, so this is consistent.

## Convention for future cpu/time auto-learn (AIRA-260, deferred)
When @aira_cpu/@aira_time gain measured auto-learn, carry the run OUTCOME into the learned store from day one and apply the SAME keep-OOM-exclude-workload-failure filter — a failed run's measured cpu/time is as untrustworthy as its peak RSS.
