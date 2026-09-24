---
{"schema":1,"id":"AIRA-273","project":"aira","title":"confine estimator: a BIMODAL-memory command is OOM-killed repeatedly — 1.5x-per-kill creep + 20-row window forgets the big mode","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":[],"hold":true,"relations":[]}
---

REPORTED BY qual (2026-09-24/25), with a second instance from gap. qual has worked around it on the fastest-ee side (explicit `--memory-reserve 1G` pin, branch qual-mlint-reserve), so this is NOT blocking — held for a later release.

## Symptom (reported)
fastest-ee's mechanical-lint gate `python3 -m fastest_ee.tools.quality.lint_gate range <base>` is BIMODAL: ~15 MB when the diff has no lintable files, ~417 MiB when it fires shellcheck/trivy/tflint/hadolint. Trailer 2026-09-24: `reserve=17880678 reserve-basis=estimate:max=15548416,n=20,f=115 … OOM-killed at its memory cap 17460K` — 20 cheap runs of history, a 17 MB cap for an expensive run. gap separately saw the cap climb 74→111→167→250 MB over consecutive kills against a ~450 MB real peak.

## Mechanism (VERIFIED against the code 2026-09-25, not just the report)
1. History is a ROLLING WINDOW of the last 20 rows per (kind, signature): `confinePeakHistoryLimit = 20` (internal/store/confine_peak_history.go:13), evicted on every insert by `ORDER BY at DESC LIMIT 20`.
2. The ordinary estimate is `max(peak) × 1.15` over that window (`estimate:max=…,f=115`).
3. After a kill, the reserve is `1.5 × MaxOOMPeak` (internal/daemon/admit.go ~:1391, `escalated += escalated/2`). A kill AT the cap records a peak ≈ the cap (the data is CENSORED — the true need is only known to be > cap), so the reserve creeps up 1.5× per kill. From 17 MB to a 417 MB need is log1.5(24) ≈ 8 kills (gap's 74 MB start ≈ 4–5).
4. Once learned, a successful 417 MB run keeps the estimate high — but only while it is in the last 20 rows. The common case (a diff with no lintable files) produces 20 consecutive cheap rows, which evict every big and every OOM row; the estimate falls back to ~17 MB and the WHOLE kill cycle restarts on the next lint-firing run. That is the "repeatedly".
5. Each kill is a spurious RED gate leg, not merely a retry.

## Candidate fixes (simplest first — pick at build time, challenge-pass first)
A. ESCALATE FURTHER ON A CENSORED KILL: `escalated = max(1.5 × MaxOOMPeak, p90-prior)` (the machine-wide prior `admitPeakP90`, ~1.4 GB today). One line; monotone (never lower than today); cuts ~8 kills to ~1 per cycle when the prior covers the need. Over-reserves the cheap mode while the OOM row is in the window — the safe direction for a packing reserve.
B. REMEMBER THE BIG MODE LONGER: exempt OOM rows (or the high-water mark) from the 20-row eviction, or age them by time rather than by count, so 20 cheap runs cannot erase the evidence. Stops the restart cycle; trades away some of the window's "a command that genuinely shrank can shrink its reserve" property — needs a stated decay rule.
C. (qual's (b)) detect bimodality from spread and refuse to let `estimate:max` undercut the p90-prior. REJECTED by default under the simplicity rule — statistics machinery for what A+B cover — unless A+B measurably fail.

A alone does not stop the restart cycle (4); B alone still costs ~8 kills per learn. A+B together is probably the minimum that meets the problem; confirm against a replay of qual's trailer history before building.

## Tests to write (when built)
- A signature with 20 cheap rows then one kill at a small cap: next reserve ≥ p90-prior (A). Mutation: drop the max() → 1.5×cap → reds.
- 20 cheap rows AFTER a big success/kill: the big evidence is still honoured (B). Mutation: restore count-only eviction → reds.
- A shrinking single-mode command still shrinks under B's decay rule (no permanent ratchet).
