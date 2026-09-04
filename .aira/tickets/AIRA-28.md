---
{"schema":1,"id":"AIRA-28","project":"aira","title":"Bound the delegate-ram aggregate so aira.slice can never over-commit (structural fix, whole-suite airtight charge)","status":"superseded","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","delegate-ram","oom","shared-slice"],"hold":false,"relations":[{"kind":"supersedes","from":"AIRA-29","to":"AIRA-28"},{"kind":"relates","from":"AIRA-62","to":"AIRA-28"}]}
---
The STRUCTURAL follow-up to AIRA-27 Option A (class-based oom_score_adj, which is a bias not a bound — a large airtight neighbour can still be out-scored under a delegate over-commit). Removes the delegate-aggregate over-commit itself so no delegate-aggregate slice-OOM fires at all.

ROOT: a --delegate-ram confine job charges the ledger only ~512MiB overhead but its scope memory.max is a containment ceiling (history peak×1.15, else 48G) resolved INDEPENDENTLY of that charge and never summed into the ledger. So Σ(delegate memory.max) ≫ sliceCap while the ledger sees Σ(512MiB); the only bound on Σ(delegate RSS) is the per-test gate, which FAILS OPEN under contention.

DESIGN (Approach A, spec docs/superpowers/specs/2026-09-01-aira27-structural-delegate-aggregate-bound-plan.md, three-lineage reviewed + Fable re-gate FOLDS-SOUND): charge the ledger the history-sized whole-suite reserve (the value resolveDelegateRAMScopeCeiling already computes) instead of the 512MiB overhead, and set scope memory.max == that charged reserve — routing delegate-ram through the airtight non-delegate invariant. Then Σ(charged) ≤ cap−headroom by construction ⇒ Σ(memory.max) ≤ cap−headroom ⇒ no delegate-aggregate over-commit, kernel-enforced (fail-closed, independent of the fail-open per-test gate); a suite over its reserve self-OOMs its own oom.group scope (contained).

FOLDED P0s: (1) Slice-3 RAM-ordering made INERT for whole-charged workers (else admitAvailable≈0 steady state collapses every governed suite to 1 worker, ~16× regression) — owner-approved to make inert, keep code. (2) #74 adoption at FULL cap via a versioned @drc- scope-id marker (else restart under-counts → over-admits). (3) daemon-outage fail-closed: remove the flock fallback for DelegateRAM entirely (new whole-charged launch refuses if daemon down). Cold-start = A1 (charge suiteBase + W×perWorkerDefault, self-OOM+peak×1.5 ratchet, stays airtight — owner-chosen). Plus explicit wire charge-field; --memory-max on delegate now charges N; larger delegate-suite safety margin.

**RESIDUAL RECORDED BY [[AIRA-62]] (2026-09-04, PR #17 `a79d621`) — an input to this ticket's
aggregate-bound decision, not a change to it.** AIRA-62 removed a CLI bug that made
`--delegate-ram --memory-max N` charge the ledger N (and silently discard an explicit
`--memory-reserve`). That charge was, by accident, a partial and unchosen implementation of
**this ticket's own deliverable** *"explicit wire charge-field; `--memory-max` on delegate now
charges N"* — but without the half that makes it coherent here (scope `memory.max` == the
charged reserve), and applying only to jobs whose operator happened to type an optional flag.
Every delegate job WITHOUT `--memory-max` already charged the 512M overhead against the same
generous ceiling, so it bounded nothing in aggregate; and it was doing real harm
([[AIRA-24]] 1785s wait then `E_ADMIT_SATURATED` with zero tests run, [[AIRA-59]] machine-wide
stall from double-booking the cap on top of the per-test children, [[AIRA-67]]).

So after AIRA-62 the delegate-ram over-commit exposure is UNIFORM: every delegate job charges
its declared `--memory-reserve` else 512M, whatever its ceiling. **Σ(delegate memory.max) ≫
sliceCap remains true and remains unbounded — this ticket's ROOT paragraph above is unchanged
and undischarged.**

The Sol review lineage BLOCKed AIRA-62 on precisely this, demanding an owner-approved
over-commit bound first, and released the block once AIRA-28's own body showed the bound was
never in force. Its accepted counter-argument, recorded here verbatim as the case for doing
this ticket: *mixed `make merge-gate` targets, projects with the legacy governor disabled,
non-pytest delegate payloads, and aitest can all be relying on that charge as their only
shared-ledger aggregate bound.* True — and they could not have relied on it, since it vanished
whenever `--memory-max` was omitted. Live mitigations meanwhile remain a BIAS, not a bound:
[[AIRA-27]]'s class-based `oom_score_adj`, the MemAvailable watchdog in enforce, and per-scope
`memory.oom.group`.

Needs a DAEMON RESTART (unlike Option A). relates AIRA-27 (Option A, done), AIRA-25 (peak/delta ledger, opposite direction), AIRA-26/AIRA-17 (a scope-start charge also gates cold-start import overshoot).

**SHELVED — SUPERSEDED BY [[AIRA-29]] (owner pivot 2026-09-01, utilisation).** Approach A (airtight whole-suite charge) was BUILT + VERIFIED but NOT deployed: at the deploy checkpoint the owner observed the live slice was half-idle (CPU + RAM) while jobs waited — a non-delegate merge-gate reserved 33.6G / used 2.6G, proving the airtight reserve-the-peak-hold-for-lifetime model UNDER-UTILISES the machine. AIRA-28 fixes over-COMMIT but by the same lever WORSENS under-UTILISATION. Owner chose DYNAMIC RESERVE (track-actual, AIRA-29) instead — charge live memory.current + headroom, re-evaluated, so the ledger reflects real usage. The Approach-A build stays on branch `aira27-delegate-bound` @ `f356622` as a reference (the review/deploy-gate caught the wrong direction before prod — same win as the Slice-2 stop). Original build record below.

**(HISTORICAL) BUILD DONE + VERIFIED (2026-09-01, branch `aira27-delegate-bound` @ `f356622`).** Full two-loop: spec v2.1 (Sol+DeepSeek+Fable plan-review → 3 P0 folded → Fable re-gate FOLDS-SOUND) → Terra build (`d0a08b5`; DelegateRAMChargeExplicit field + gofmt by Opus, Terra sandbox couldn't build/commit) → adversarial build-review (6 dims, 5 CLEAN incl. adoption-release/invariant/governor-inert/non-delegate-boundary; 1 P1 porous test FIXED+mutation-verified `f356622`; 1 P2 refuted → documented accepted gap) → Opus verify PASS. Gates: `make ci` exit 0, `-race` clean (daemon+runner). Live smoke pending deploy. **Deploy owner-gated: owner chose "deploy when slice quiet" (2026-09-01) — slice was ~99.6% reserved by 2 big non-delegate neighbour jobs (30G+36G); quiet-poll `b64rwfp2w` armed (deploy when granted<20G).** Deploy = merge→master, build binary, swap `~/.local/bin/aira`, `systemctl --user restart aira-daemon.service` (#74 reconstructs; legacy `@dr-` adopt current+margin, new `@drc-` whole-charged), smoke (delegate reserve==memory.max), notify all 11 sessions. Rollback = swap backed-up binary + restart.

## Status transition (backlog remediation, Phase 0)

`planned` → **`superseded`**. This commit records a transition, it does not make a
decision: the owner's pivot to AIRA-29 is dated 2026-09-01, the ticket body has
read "SHELVED — SUPERSEDED BY AIRA-29" since then, and the `supersedes` relation
(AIRA-29 → AIRA-28) is already in this ticket's own front matter. Only the status
field was left behind, so the ticket kept appearing in the open backlog and in
ready-queue arithmetic as a live P1.

Flagged for explicit owner sign-off in the plan (§5 item 1) because it retires a
real airtight-guarantee design, not because the transition is in doubt. The
Approach-A build is preserved on branch `aira27-delegate-bound` @ `f356622` and
the analysis stays in the body above — nothing is discarded by this transition.
AIRA-29's own build is explicitly NOT part of the backlog-remediation plan; it
ships as its own follow-on milestone once its ship-together (Slice 1 + Slice 2)
precondition and its hold reason are answered (plan §3.2).
