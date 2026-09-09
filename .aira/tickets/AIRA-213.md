---
{"schema":1,"id":"AIRA-213","project":"aira","title":"Owner decision: is a confine estimator subject the argv alone, or argv + directory?","status":"planned","kind":"spike","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","rant-triage"],"hold":false,"relations":[]}
---
> Filed from the 2026-09-09 global rant triage (35 rants, adversarially reviewed).
> Evidence below survived an independent refutation pass; claims that did not are
> recorded as dropped in the triage record and deliberately absent here.

Owner decision: is a confine estimator subject the argv alone, or argv + directory?
**kind** spike · **severity** P2 · **closes** RANT-34, RANT-29 (fragmentation half) · **blocked on T1**

**SYMPTOM.** AIRA ships both keying behaviours today and which one a job gets is an accident of whether its launcher names a path in argv. `aira confine -- make test` typed by hand in any of this repo's worktrees is one shared key — the documented, intended property (`internal/core/skill.go:319`). AIRA's own hooks (`.githooks/common.sh:62`, `exec aira confine -- make -C "$root" "$@"`) put `$root` in the argv, and `ResourceSignature` (`internal/runner/resource_estimate.go:69-79`) joins the effective argv verbatim — so hook runs are already keyed **per worktree**, and `git worktree list` reports 96. Two rants ask for opposite fixes: RANT-29 wants the hooks changed so all worktrees share one key; RANT-34 wants a directory term added so they stop sharing. Neither can be filed without settling the other.

**WHAT THE CODE ALREADY SETTLES — do not re-argue.**
1. A per-directory key buys **nothing** while a key is cold: with no history the reserve comes from `estimate:p90-prior`, a **machine-wide** p90 (`internal/store/confine_peak_history.go:200-228`), and until a key has 3 samples of its own the two schemes are byte-identical.
2. Splitting keys makes cold start worse twice: 3 cold runs per worktree instead of 3 for the fleet, **and** it degrades the prior itself — `ConfinePeakP90` counts only signatures with `COUNT(peak_rss)>=3` and contributes one peak_max each, so splitting removes signatures from that population and multiplies near-duplicates among survivors.
3. **Sharing biases the estimate UP, not down.** The estimator is MAX + 15 %, not a mean (`resource_estimate.go:124`, `peak := stats.PeakMax`; SQL `MAX(CASE WHEN peak_rss>0 ...)` at `confine_peak_history.go:181`). So sharing costs over-reservation and queueing — RANT-29's actual symptom — and does not average a large worktree down. **RANT-34's stated mechanism is wrong here and must not be reproduced.**
4. **The real under-cap hazard is retention, not keying.** Only the newest 20 rows per (kind, signature) survive (`confine_peak_history.go:13` and the retention DELETE), so 20 small runs after one large run evict the large sample and PeakMax collapses — and for a non-delegate admitted unpinned job the reserve **is** the hard `memory.max` (`confine_linux.go:983-985`), so the next large run is OOM-killed rather than merely delayed.
5. **RANT-34's fallback ask is already shipped.** `internal/runner/confine.go:933-937` prints `reserve-basis=` whenever a reserve exists, and the learned bases embed the sample count (`estimate:max=%d,n=%d,f=115`). RANT-29's own captured trailer proves it. The residual is narrow: the trailer never names the **subject** the number was learned against.

**THE DECISION.** Whether "same argv, different tree" is one subject or many. No reading of the code decides it.

**OPTIONS.** **A** — keep argv-only, fix the hook to stop forking the key by accident: `.githooks/common.sh:62` becomes `cd "$root" && exec aira confine -- make "$@"`. Behaviour-preserving (no `Chdir`/`cmd.Dir` on the foreground confine path, so it inherits the caller's cwd) and AIRA-76 is preserved because `$root` still comes from `git rev-parse --show-toplevel`. **B** — add a directory term (RANT-34's ask); costs the machine-wide prior, contradicts `skill.go:319`, and makes A a prerequisite rather than a no-op. **C** — name the subject in the trailer; the only part of RANT-34's ask that is both absent and uncontentious, compatible with A and B. **D** — treat retention as the real knob (raise or adapt the 20-row window, or record the observed spread).

**RECOMMENDATION: A + C now, D measured, B refused** — B costs the one thing every cold key depends on to buy accuracy nobody can currently measure. **Fix the instrument first (T1), collect a week of `confine --budget`, then decide.**

**HOW TO TEST.** A: `internal/install/githooks_test.go:242-247` pins `wantTarget := "-C " + fx.worktree + " fmt-check vet build"` — that assertion fails against the change and must become a cwd assertion (the fixture's fake `make` **already** logs `make cwd: $PWD` at `githooks_test.go:121` and nothing asserts on it, so no shim change is needed), keeping AIRA-76's negative assertion. Add a signature-level test: two fixture worktrees running the same hook produce one `ResourceSignature`. C: a `FormatConfineStatus` test asserting the trailer names the subject. D: a store test writing 1 large then 20 small observations and asserting what PeakMax should be under the chosen policy.

**Evidence hygiene.** Do not file RANT-29's draft as written: its "every worktree cold-starts at the 4 GiB default … ~6.5×" is the wrong branch (the p90 prior fires first; measured ~1.04 GiB), and its claim that fragmentation is "the direct cause" of the 12-deep queue wait is unproven — that job had `n=20` and was not cold-starting. The sharper, provable symptom is the **pre-push** shape: 120 distinct `make -C <path> ... ci` signatures, 65 % below the 3-sample threshold, max recorded peak 1360 MiB against a ~1.2 GiB prior grant, **2 recorded OOMs**.

---
