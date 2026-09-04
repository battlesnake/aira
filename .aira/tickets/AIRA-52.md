---
{"schema":1,"id":"AIRA-52","project":"aira","title":"confine --list owner reverts to unknown after a daemon restart","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["confine","daemon","dogfood"],"hold":false,"relations":[]}
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

## Resolution (2026-09-04, backlog-remediation Phase 0, plan §2) — landed with AIRA-23

One identity decision, not two tickets: AIRA-23 picks what an unclaimed owner IS,
AIRA-52 picks where an owner LIVES, and the second constrains the first (the
value has to be safe as a scope-directory-name component).

### Where owner lives: the scope directory name

The scope id gains an optional `@<owner>` suffix —
`CONFINE-[@dr-]<name>-<pid>-<stamp>[@<owner>]` — for exactly the reason the
delegate-RAM `@dr` marker already lives there, and which that marker's own
comment already states: **the cgroup directory name is the only carrier that
survives a daemon restart.** Owner used to exist solely on the in-memory
`admitWaiter`; the restart-adoption path rebuilds aggregate reserve scalars from
a live cgroup scan and never recreates per-job waiters, so a job whose lifetime
spanned a restart lost its owner permanently.

`@` is unambiguous as a delimiter: neither `--name` nor a caller-supplied owner
may contain it, and the only other `@` is the fixed `@dr` marker immediately
after the `CONFINE-` prefix, which the parser strips first. An unknown owner is
encoded as the ABSENCE of a suffix, never as `@unknown`, so "nobody claimed this"
can never be confused with a claim — and an id minted before this change parses
identically.

Deleted, as the plan asked: `Server.freshConfineOwner`, the
`runner.ConfineOwnerLookup` type, `KillConfine`'s `freshOwner` parameter, and the
registry's role as an ownership source. `ConfineRegistryEntry` is now just
`{ScopeID}` — its only remaining job is surfacing an admitted-but-not-yet-on-disk
scope as a Pending row. `mergeConfineRegistry` lost its conflict/agreement dance
(two waiters claiming one scope id with different owners collapsing to
"unknown"): one scope id can only decode to one owner by construction.

The daemon's `confineScopeIDPattern` and `confineAdmitScopeName` were widened and
taught to drop the tail respectively — both are on the admission wire path and
would otherwise have rejected every owned scope id as non-canonical.

### What an unclaimed owner is (AIRA-23)

The fallback is no longer the literal `"unknown"`. It is
`InferConfineOwner(cwd)` → `@cwd-<sanitised basename>`. AIRA-23's reported
incident was a session about to `pgrep`-kill two SIBLING sessions' jobs, all
showing OWNER "unknown", saved only by inspecting each process's cwd by hand — so
cwd is precisely the discriminator `--list` was missing.

**It is marked, and marked means never attested.** AIRA-23 requires the default
not weaken the kill guard, and a bare cwd-derived identity would: two sessions in
one directory infer the same string, so honouring it would let either kill the
other's job with no `--steal`. So:

- the inferred form carries `ConfineInferredOwnerPrefix` (`@`), which is OUTSIDE
  the caller-supplied identity alphabet — an inferred owner is therefore
  unforgeable, on the command line and on the wire;
- `ConfineOwnerIsAttested` reports false for empty, `"unknown"`, and anything
  `@`-prefixed, and the kill guard requires BOTH sides attested. An inferred
  owner does not open the guard even against itself.

Net effect on the guard: strictly unchanged. Net effect on `--list`: it now says
where a job came from instead of "unknown".

The charset was NOT widened to admit `:` (the plan's `cwd:<basename>` sketch
offered "or an equivalent substitute"). `:` has systemd-unit-name meaning and
would have needed a new alphabet; `@` was already reserved, already unusable in a
caller identity, and already the scope-id marker character. A `maxConfineOwnerLen`
of 64 was added so the worst-case directory name
(`.aira-CONFINE-@dr-<name×100>-<pid×7>-<stamp×13>@<owner×64>`) stays well inside
`NAME_MAX`.

### Tests

- `TestConfineKillOwnerSurvivesADaemonWithNoMemoryOfTheJob` — AIRA-52's
  regression test: a scan-only scope, empty registry, no daemon memory at all
  (exactly the post-restart state), and the owner can still kill its own job with
  no `--steal`.
- `TestConfineKillRefusesAnInferredOwnerWithoutSteal` — AIRA-23's safety
  boundary: the inferred owner IS surfaced by `--list`, and does NOT open the
  guard even for an identical caller identity.
- `TestConfineKillTakesOwnerFromTheScopeIDNotTheRegistry` — replaces
  `TestConfineKillUsesFreshRegistryOwnerNotListSnapshot`, whose premise (a stale
  registry owner beaten by a fresh daemon lookup) no longer exists.
- `TestConfineOwnerDerivationChain` extended: the chain's tail is the marked
  inference, it is not attested, and supplying it explicitly is REFUSED.

### The treatment-arm repro is still not run, and is no longer needed to justify this

This ticket's own investigation ended at "confirmed latent gap, unconfirmed
real-world impact", with the repro deliberately not run because it requires
restarting the shared daemon at a quiet moment. That is unchanged: nothing here
was verified against a live restart. The fix is justified exactly as the ticket
says it should be — "fix because the code is wrong" — and the regression test
reproduces the post-restart state (no daemon memory of the job) directly, which
is what the mechanism actually depends on, without touching the shared daemon.

`make ci`: exit 0.

### Build-review (Sol, 2026-09-04) — one P0 and one P1 folded in

- **P0, FIXED — admission did not bind the persisted owner to the claimed one.**
  The daemon validated `scope_id` and `owner` independently, so a client could
  send `scope_id=CONFINE-job-1-a@victim` with `owner=me`: admission accepted it,
  and after the next restart the scan decoded `victim` as an ATTESTED owner
  nobody had claimed. The inverse (a real `owner` with no tail) was accepted too
  and silently degraded to `unknown` the moment the daemon forgot it. Making the
  scope id the durable ownership record without binding it at the trust boundary
  is the whole defect this ticket is about, reintroduced one layer up.
  `validateAdmitArgs` now refuses any request whose embedded tail DISAGREES with
  the claimed owner. The binding is deliberately **asymmetric**: a MISSING tail
  is accepted, because it means the client persisted no claim at all — the
  pre-AIRA-52 behaviour, where the daemon accounts the owner in memory, the job
  reads as unowned after a restart, and the kill guard therefore demands
  `--steal`. Refusing that case would buy no safety and would hard-break every
  session whose installed `~/.local/bin/aira` predates this change the moment the
  daemon restarts, with no protocol-version bump to signal it. Covered by
  `TestValidateAdmitArgsBindsTheEmbeddedOwnerToTheClaimedOwner`, which asserts
  both directions (three impersonating shapes refused, four legitimate shapes —
  including the stale-client no-tail case — accepted, so it cannot pass by
  refusing everything).
- **P1, FIXED by DELETION — two parsers, two languages.** The daemon carried its
  own `confineScopeIDPattern` regex restating the scope-id grammar beside
  runner's parser, and they accepted different sets: the regex allowed a zero or
  overflowing pid, an uppercase base-36 stamp and an unbounded owner, all of
  which the scanner's parser rejects — so an id the daemon admitted could be
  invisible to every scan, adoption pass and reaper. Rather than syncing them,
  the second one is gone: `parseConfineScopeID`, `validConfineScopeID`,
  `IsDelegateRAMScopeID` and `delegateRAMScopeIDMarker` moved from
  `confine_manage_linux.go` into the PORTABLE `confine.go` (they are pure string
  manipulation, exactly as `IsDelegateRAMScopeID`'s own stub comment already
  said), an exported `runner.ParseConfineScopeID` was added, and the daemon calls
  it. `confineScopeIDPattern`, `confineAdmitScopeName` and the
  `IsDelegateRAMScopeID` stub are deleted. The surviving parser was also made
  CANONICAL rather than merely permissive — the pid and stamp must round-trip
  through `strconv.Format*` — because the regex was the STRICTER side on those
  (`strconv.ParseInt` accepts an uppercase base-36 stamp, a sign and leading
  zeros; `confineScopeID` can mint none of them). Unifying on the looser grammar
  would have widened what admission accepts, so it was tightened instead.
- **P1, FIXED — the regression tests hand-built owner-bearing ids**, so they
  could not have caught a mint/parse disagreement.
  `TestConfineScopeIDRoundTripsEveryOwnerForm` now exercises the production
  `confineScopeID` -> `parseConfineScopeID` path across attested, inferred,
  delegate-RAM, dashed-name, `@dr`-lookalike-name, unknown and empty owners, and
  `TestConfineScopeIDRefusesAnOversizedOwnerTail` pins the NAME_MAX bound at its
  boundary in both directions.
