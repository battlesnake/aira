---
{"schema":1,"id":"AIRA-52","project":"aira","title":"confine --list owner reverts to unknown after a daemon restart","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["confine","daemon","dogfood"],"hold":false,"relations":[]}
---
## Symptom

A confine job launched with a correctly-resolved owner (explicit `--owner`, or `AIRA_CONFINE_OWNER` set inline on the same launch command — both confirmed correctly wired client-side in `resolveConfineOwner`, cmd/aira/main.go) shows `owner unknown` in `aira confine --list` if the daemon restarts while the job is still running. This was reported independently by two peer sessions during dogfooding, each certain they had set ownership correctly on the exact launch command.

## Root cause (confirmed via source read, not speculation)

The daemon's restart-adoption path (`internal/daemon/admit.go`, roughly lines 600-690, the AIRA-74 reserve-reconstruction logic) rebuilds only AGGREGATE `adopted`/`adoptedJobs` reserve scalars from a live `ListConfines` cgroup scan after a restart. It never recreates individual `queue.waiters` entries. Owner is tracked exclusively on `admitWaiter.owner` (see `activeConfines`/`freshConfineOwner` in `internal/daemon/confine_manage.go`), which lives only in the in-memory waiters slice — wiped on every daemon restart. A job whose lifetime spans a restart is correctly re-counted for reserve/admission-fairness purposes, but has no waiter entry at all afterward, so `freshConfineOwner` can never find one and permanently falls back to `ConfineUnknownOwner` for the rest of that job's life.

## Correction — the mechanism is confirmed, but is NOT confirmed to be what the two reporters actually hit

The original version of this ticket claimed the two reporters' specific `owner unknown` cases were caused by the restart-adoption gap above. That claim does not hold up and should not be trusted: a peer session (`field`) pointed out the much simpler, more likely explanation for most real-world `owner unknown` sightings — the launching session simply never set `AIRA_CONFINE_OWNER` (or `--owner`) in the first place, so there was never an owner to lose. `field` confirmed this was true for their own case by checking `AIRA_CONFINE_OWNER` directly (unset).

Attempting to distinguish the two causes from one session's `aira confine --list` snapshot (ages relative to the two observed restarts at 13:02:46 and 14:19:01) produced two jobs launched ~24s apart, both apparently spanning the 14:19:01 restart by that arithmetic — one with a known owner, one `unknown`. If a restart reliably wiped owner, both should have gone unknown; they didn't. That is weak evidence (age-from-restart arithmetic has no better than ~seconds precision and no confirmed launch timestamps), but it point the same direction as `field`'s direct evidence: plain "never set at launch" is at least as likely, probably more likely, as the dominant real-world cause, and may be the *only* cause anyone has actually hit — the restart-adoption gap may be a real but so-far-unobserved edge case.

**What is and isn't proven:**
- PROVEN (source read): the restart-adoption path never rebuilds per-job `waiters` entries, so owner cannot survive a restart for a job admitted before it. This is a real gap in the code, independent of whether anyone has hit it yet.
- NOT PROVEN: that either original reporter's case was actually caused by this gap rather than by never setting the variable. Neither reporter's launch-time environment was captured before the fact, so this can't be checked after the fact.

Before investing in a fix, whoever picks this up should get a clean repro: launch a job with `AIRA_CONFINE_OWNER` verified non-empty via a printed echo in the exact same command, restart the daemon while it is still running, and confirm `owner` goes from known to unknown on that specific scope ID.

**Control arm — confirmed (`field`, same session, live check).** `AIRA_CONFINE_OWNER=stoner-field aira confine -- sleep 25`, listed while live: showed `OWNER stoner-field` in the same `--list` output as three other sessions' `unknown` scopes, same pool, same instant. This confirms the whole non-restart path — client-side env resolution, wire transport, daemon-side waiter storage, and the `--list` display — works correctly end-to-end when the variable is actually set at launch. So a genuine `unknown` on a job that provably *was* launched with the variable set can be trusted as real case (2) evidence, not measurement noise.

**Treatment arm — deliberately not run.** Completing the repro requires restarting the aira daemon while a job is live, which is a shared-resource action affecting every session on the box (same blast-radius class as stopping the slice — see this project's hard rule against casual shared-daemon/slice operations). Do not fire this off opportunistically. Whoever runs it should pick a quiet moment and ideally coordinate/announce first, the same way you would before stopping `aira.slice`.

**A plausible case-(1) confound worth ruling out first:** in an agent-harness environment where each shell tool call can run in a fresh shell, `export AIRA_CONFINE_OWNER=...` in one call followed by `aira confine` in a *separate* call can silently lose the export (only cwd persists across calls in some harnesses, not arbitrary shell state) — producing an unowned job that looks, from the outside, identical to the daemon bug this ticket describes. A session convinced it "set it correctly" may have actually hit this instead. Worth checking for this pattern specifically before concluding a real restart-adoption repro is needed.

Without the treatment-arm repro, this ticket should be treated as "confirmed latent gap, unconfirmed real-world impact" rather than an explained bug.

## Final state of the investigation (settled)

`split` (the session behind scope `CONFINE-@dr-job-2258854-dl5n2xhzppqs`, job name `inc23a-reverify`, the specific job discussed above) went back through their own saved artifacts and retracted the original report:

- Their launch script did correctly `export AIRA_CONFINE_OWNER=inc23a-reverify` before invoking confine.
- They hold no saved `--list` output actually showing `owner unknown` for this job. The only "unknown" string in their logs is `reserve …/unknown` from an `E_ADMIT_SATURATED` message — an available-RAM field, unrelated to ownership — almost certainly the source of the original conflation.
- Scope `2258854` / `dl5n2xhzppqs` is confirmed (by the owner-string naming convention, unique to their script) to be their reverify job, and it shows a correctly known owner in the 14:31:22 snapshot cited above, which spans the 14:19:01 restart.

So the actual evidence tally is: **zero confirmed real-world firings of the restart-adoption gap, and one concrete data point (2258844's own job) that is evidence *against* it firing** (owner survived a restart it should, per the code-level mechanism, have been vulnerable to). The restart-adoption gap remains a real, source-confirmed defect in `internal/daemon/admit.go`'s reconstruction path — worth fixing on its own merits since it's a genuine hole in what a shared-daemon restart preserves — but there is currently no evidence it has ever actually produced a user-visible `unknown` for anyone. Treat severity/priority accordingly: this is "fix because the code is wrong," not "fix because it broke something observed."

The treatment-arm repro (launch a known-owner job, deliberately restart the daemon, confirm owner flips to unknown) is still the only way to move this from "latent" to "confirmed," and per `field`'s point it should be scheduled deliberately at a quiet moment on the shared daemon, not run opportunistically.

## Impact

Cosmetic/diagnostic only today — owner plays no role in either reap pass (`reapOrphanedScopesPass` gates on scope-emptiness + PID-liveness + lease state; `releaseStaleGrantedLeasesPass` gates on `grantedAt` age). But it degrades the `--kill`/`--steal` UX (an owner check that should let you kill your own job without `--steal` silently can't, forcing an unnecessary `--steal` — observed directly by one of the reporting sessions) and undermines trust in the `--list` output on any machine where the daemon restarts somewhat often (upgrades, crashes, or — as seen this session — another session's deploy step).

## Suggested direction (not yet designed)

The reconstruction scan already reads each live scope's directory name and memory.max; it has no way to recover owner from cgroupfs alone since owner was never persisted outside daemon memory. A fix likely needs the launching client to write a small persisted marker (e.g. an owner file inside the scope directory, or a daemon-side sidecar record keyed by scope ID) that the restart-adoption scan can read back, rather than trying to reconstruct owner from cgroup state that never carried it. Given AIRA has no live users/compat constraints, this is a free redesign — worth sizing against architectural-simplicity before committing to a specific persistence mechanism.
