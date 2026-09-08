# A "drain" admission mode — stop new admissions, queue rather than reject, reusing exclusive mode's mechanics

Status: **plan revision 2 — not yet gated, not yet built.** Revision 1 was
reviewed by Astra (Codex, orthogonal plan-review) and returned
NEEDS_REVISION with four real P1 findings, all against the original §3.2
(a detached `start`/`status`/`release` lifecycle). This revision responds
by substantially shrinking v1's scope rather than patching around each
finding individually — see §3 and the revision note at the end of each
finding in §3.5.

## 0. Problem, stated precisely

Owner (2026-09-08): "Perhaps it would be useful if there was a way for us
to 'drain' the slice so we can wait for it to drain and not admit any new
jobs (just leave them queued). Would be useful for when we need to deploy
updates. We already have the exclusive mode, so can probably re-use logic
written for that."

This is directly relevant to tonight's own standing task: this session has
spent the whole night watching `aira confine --list` by hand, waiting for
the box to go quiet enough to flip the slice-ceiling default to `enforce`
(AIRA-177, merged) — exactly the manual "wait for quiet" ritual a drain
mode would automate.

## 1. What already exists to build on (verified from source, Astra-checked)

**Exclusive mode is almost exactly this feature already, and the hunch to
reuse it is correct — confirmed by Astra's independent re-derivation, not
just the original research.** `aira confine --exclusive -- <argv>`
(`cmd/aira/main.go:781-798`, wire field `admitRequest.Exclusive`) does,
today, precisely: wait for the slice to be **provably empty** of every
other admitted job (`sliceProvablyEmpty`, `internal/daemon/admit.go:513`),
while **blocking every other queued waiter from being granted** during
that wait (`exclusiveGate.blocks`, `admit.go:717-750`) — without touching
already-**running** jobs, which finish untouched. This is drain, verbatim.

**The reusable primitives, precisely (Astra confirmed all of these against
source independently):**

- `sliceProvablyEmpty` + the `evaluateAdmitQueue` liveScopes scan — "is the
  slice currently empty of other jobs". Astra's correction: the underlying
  cgroupfs scan defaults to a **1-second** interval, independent of the
  250ms evaluator tick (`admit.go:48,2248`) — softened from the original
  plan's blanket "250ms" claim.
- `exclusiveGate`/`exclusiveGateLocked` (`admit.go:419-457,2167,2195`) — a
  **derived, never-persisted** state machine (`draining` → `held`),
  race-free under `queue.mu`, with `CodeAdmitExclusiveActive` refusing a
  second concurrent holder (at most one at a time, slice-wide).
- **Connection-liveness-bound release.** `admitConnection`'s `defer
  release()` fires on every connection-close path (`admit.go:334-337,
  2035-2051`), so a crashed/killed/Ctrl-C'd requester's socket EOF
  instantly clears the hold. Proven live by
  `admit_exclusive_unwedge_test.go` (SIGKILL the holder → slice unwedges).
  **This property holds only for as long as the daemon process itself
  survives — see §3.4 for what happens when it doesn't.**
- Existing bounds: `admitExclusiveWaitCeilingDefault = 30 * time.Minute`
  caps how long a draining-but-not-yet-held request may wait; default
  confine admission wait more generally is **30 minutes**, not seconds —
  Astra's correction to a claim implicit in the original review request,
  cited here so it isn't repeated (`internal/runner/confine_linux.go:1614`).
- Reporting precedent: `ConfineSliceReserve.Exclusive` is a nilable
  `*ConfineExclusiveState{State, Name, Owner, ScopeID, WaitingJobs,
  SinceMS}` (`internal/runner/confine_manage.go:410-418,512-522`), rendered
  in `confine --list` text, `aira top`'s footer tag, and passed through
  JSON/MCP automatically from the same struct.

## 2. What a drain is actually protecting against (verified, recalibrates urgency)

A daemon restart or binary swap does **not** kill already-admitted confine
jobs — Astra independently re-confirmed this. Each confine job is a
separate cgroup scope launched directly by the `aira confine` client
process itself (`exec.CommandContext` with `SysProcAttr{UseCgroupFD: true,
CgroupFD: scope.FD()}`, `internal/runner/confine_linux.go:1087,1170`),
never a daemon child; losing the admission lease mid-run is reported, but
the job is not killed (`confine_linux.go:743`). A daemon restart drops the
AF_UNIX admission socket briefly — Astra's correction: reconstruction
after restart is driven by re-scanning live cgroups (`admit.go:2431`), not
by replaying persisted admission leases, and the exact downtime is not a
guaranteed "~1s" the way the original plan implied; it is "briefly, by
design, with reconstruction after".

**So drain is not about protecting running jobs from being killed by a
deploy — it already isn't at risk.** What it actually buys: a clean,
uncontended window to observe/validate a change, avoiding a race where a
config/default change lands mid-contention, and general deploy hygiene —
not a correctness requirement. Stated plainly for whoever picks this up:
this is a quality-of-life / operational-confidence tool, not a safety
mechanism, and must not be sold as one.

## 3. Proposed shape — v1 substantially narrowed after Astra's review

### 3.1 v1 does NOT generalise the gate, add a lifecycle verb, or add a release-ownership mechanism

Revision 1 proposed a detached `aira drain start/status/release` lifecycle
with its own gate-generalisation (hold-for-job vs hold-for-drain) and its
own release-ownership guard. Astra found four real, independent P1 defects
in that shape (§3.5). Rather than patch each one, **v1 abandons the
detached lifecycle entirely** and ships the smallest thing that delivers
the owner's actual ask, by staying inside the already-tested exclusive-mode
mechanism instead of building beside it:

**`aira drain wait [--timeout DURATION] [--reason TEXT]`** — thin CLI sugar
that issues the *exact same* admission request as `aira confine
--exclusive -- <argv>` today, where `<argv>` is a real internal placeholder
binary launched through the **real `runner.Confine` scope-creation path**
(`internal/runner/confine_linux.go:790,1170` — genuinely admitted, genuinely
scoped, genuinely charged, exactly like any other exclusive job) that
simply blocks on SIGINT/SIGTERM/timeout instead of running a user command.
**Astra's second-pass finding, now fixed: this is committed to as the only
v1 shape** — "the CLI process itself blocks directly, without going
through `runner.Confine`" is explicitly ruled out, since that path would
skip real scope creation and isn't equivalent. Release is Ctrl-C (or the
operator's shell sending SIGTERM/SIGINT) or the timeout firing, in the
**same foreground, connection-bound process** the whole time — identical
safety profile to `--exclusive` today, because it *is* `--exclusive`
today, wearing a friendlier front end.

**`--timeout` semantics, precisely (Astra's second-pass finding: the
original wording was ambiguous and needed disambiguating).** `--timeout`
governs only the **held** duration, once admitted — it is a thin wrapper
over the existing `ConfineRequest.Timeout`, which itself excludes
admission/setup time (`confine.go:563`). Admission wait is a *separate*,
already-existing budget, defaulting to 30 minutes
(`confine_linux.go:1614`) unless the caller also passes the existing
`--admit-timeout`. So `aira drain wait --timeout 10s` can still spend up
to ~30 minutes *waiting to be admitted* before its 10-second hold even
starts — this must be stated explicitly in the tool's own `--help` text
and documentation, not left implicit, so an operator doesn't read
`--timeout 10s` as "give up entirely after 10 seconds".

`--reason TEXT` is small but, per Astra's second-pass finding, **is not
free**: it cannot be smuggled through the existing `Name` field, which
rejects spaces/colons (`confine.go:1145`) and must match the scope ID
(`admit.go:3203`) — a human-readable label like "deploy: slice-ceiling
flip" cannot satisfy either constraint. It needs its own optional wire
field, which means a small, additive change to `admit.go`'s strict
argument validator (`admit.go:3050`) to accept it — **corrected scope: not
"zero changes to admit.go", but "no *gate/behaviour* changes to admit.go",
only an additive validator change for one optional metadata field.**
Surfacing it in `aira top`'s footer (currently only `"EXCLUSIVE
"+state`, `tui_top.go:965`) needs a small renderer change too if that
surface is in scope; `confine --list`'s exclusive-line rendering is the
minimum v1 target regardless.

**Deploy usage pattern this enables:** operator runs `aira drain wait
--reason "deploy"` in one pane; watches it transition draining → held
(same rendering as today's exclusive state); does the deploy steps in
another pane/session; Ctrl-C's the `drain wait` process when done. No new
failure mode beyond what `--exclusive` already has, tested, tonight.

### 3.2 What is explicitly deferred, not built in v1

A detached, scriptable lifecycle (`start` that survives its own invoking
shell, `status`/`release` from a different process/session) remains a
real, plausible v2 if the foreground tool proves the concept useful in
practice — but is **not** in this ticket's scope, precisely because
Astra's P1 #3/#4 findings (detached-orphan risk, release-ownership
ambiguity) are genuine, unsolved design problems, not implementation
details. Building v1 foreground-only sidesteps them entirely rather than
attempting to solve them speculatively.

### 3.3 Timeout behaviour for other, ordinary queued waiters — unchanged

Other sessions' queued jobs keep their own `--admit-timeout`-governed
deadlines during a drain and are rejected with the existing, already-honest
`Exclusive: "draining"/"held"` reason if the drain outlasts them — exactly
today's `--exclusive` behaviour, unmodified. No new daemon-side
timer-suppression machinery in v1.

### 3.4 Daemon restart during a held drain — documented, not solved (Astra P1 #1)

If the deploy itself requires restarting the daemon (as AIRA-177's own
slice-ceiling mode flip does), the drain necessarily ends at that point:
`s.admitQueues` is wiped on daemon restart by design (`admit.go:334-337`,
fail-open), and this applies uniformly to a held drain exactly as it does
to every other admission waiter today — no new gap, but also no exemption.
**Operational guidance, not code:** sequence a daemon-restarting step as
the *last* action inside a drain window, and treat the restart itself as
ending the drain's guarantee — the same "connection-blip, fall back to
flock, reconstruct after" behaviour every other admission path already has
and already accepts. A drain cannot make a daemon-restarting deploy step
itself atomic; it only removes contention *before* that step.

### 3.5 Astra's other findings and how v1 disposes of each

- **P1 #2 (queue overflow / 256-entry cap falls back to flock, bypassing
  the gate):** an existing, shared limitation of the reused mechanism
  (`CodeBusy`, `admit.go:2170`; flock fallback, `admission_linux.go:230,
  619`), not new to drain. v1 does not claim an absolute guarantee — §2 is
  revised to state plainly that drain is best-effort contention reduction,
  not a hard bound, and this cap is the concrete reason why.
- **P1 #3 (detached orphan risk) / P1 #4 (release-ownership ambiguity):**
  both dissolve by construction in the foreground-only v1 (§3.1/§3.2) —
  there is no detached helper and no separate release verb to guard.
- **P2 #5 (gate-generalisation conceals real complexity — nesting
  exemptions, worker-admission exemptions, RAM/job-count charging for a
  scopeless hold):** moot in v1 — no gate generalisation, no scopeless
  hold. `aira drain wait` requests `Exclusive: true` exactly as `--exclusive`
  does today, with a real (placeholder) scope behind it, so every existing
  exemption/charging path applies unmodified.
- **P2 #6 (message attribution):** addressed by `--reason` (§3.1) — a drain
  used via this tool renders its own label rather than reusing exclusive
  mode's generic wording.
- **P3 (factual corrections — scan interval, restart reconstruction
  mechanism, "~1s" overstatement):** folded into §1/§2 above.
- **Second-pass P2 (subprocess commitment + `--timeout` ambiguity;
  `--reason` wire-field cost):** both fixed in §3.1 above. Astra's
  second-pass review otherwise confirmed the foreground design has no
  remaining orphan/release/restart/overflow/charging gap.

## 4. Reporting — no new struct, one new optional wire field

No `ConfineDrainState`/`Drain` field is needed. `confine --list` already
renders exclusive state correctly; the change is a new optional `Reason
string` field (distinct from `Name` — see §3.1, `Name` cannot carry
free-text) threaded through `admitRequest`/`ConfineExclusiveState`, with a
small additive change to `admit.go`'s argument validator (`admit.go:3050`)
to accept it, so `confine --list` can render something legible ("held by
'deploy: slice-ceiling flip' (mark)") instead of a generic placeholder
name. Surfacing `Reason` in `aira top`'s footer additionally needs a small
change to its renderer (`tui_top.go:965`, currently prints only
`"EXCLUSIVE "+state`) — real but small, one conditional append matching
the pattern already there. JSON/MCP need no separate schema work — same
struct, same tag discipline, already-confirmed dispatch-table passthrough.

## 5. Open questions for the plan-gate (deliberately not settled here)

1. Exact verb name/home (`aira drain wait` vs. e.g. `aira confine
   --exclusive --hold` as a documented idiom with no new top-level verb at
   all — the smallest possible version of this ticket is *pure
   documentation plus the `--reason` label*, with no new subcommand).
2. Whether `aira top`'s footer needs the `Reason` append in v1, or whether
   `confine --list`'s rendering alone is sufficient for the first cut.
3. Whether/when a v2 detached lifecycle (§3.2) gets picked up, and what
   would need to be true first (real usage of the v1 foreground tool
   surfacing a concrete need) — explicitly not decided here.
4. Whether `install.sh`'s own deploy sequence should recommend/wrap `aira
   drain wait` around itself once this ships — likely yes as documentation,
   out of scope for this ticket to decide unilaterally.

Resolved by Astra's second pass (no longer open): the placeholder is a
real subprocess through `runner.Confine`, never the CLI process blocking
directly (§3.1); `--reason` is a genuinely new wire field, not a reuse of
`Name` (§3.1/§4).

## 6. Status

Revision 3, incorporating both rounds of Astra's plan-review, returned
**PASS** on Astra's third pass. **v1 is now BUILT and in review** on branch
`aira185-drain-wait` — see AIRA-185's own build record for what landed and
for the live evidence.

Two of §5's open questions were settled by the build, and both are recorded
here rather than left implicit:

1. **Verb name/home:** `aira drain wait`, a new top-level verb, registered in
   the dispatch table so it appears in generated help and the agent guide, but
   deliberately CLI-only (`Include` unset, no MCP tool) like `confine`,
   `confine-reserve` and `confine-status`. A foreground, connection-bound hold
   has no honest request/response form: a tool could only return before the
   hold began — a fabricated success — or block a dispatcher for up to half an
   hour.
2. **`aira top`'s footer:** INCLUDED in v1. It is one conditional append
   matching the pattern already at `tui_top.go:965`, absent when no reason was
   given, so the footer is byte-identical for every `aira confine --exclusive`.

Question 3 (a v2 detached lifecycle) stays deferred and undecided, as §3.2
requires. Question 4 (`install.sh` wrapping itself in a drain) was NOT decided
by this build: `install.sh` is untouched, and the drain guidance added to the
generated SKILL/agent guide is operator documentation, not a change to any
deploy sequence.

The status line above was also the plan's own honesty obligation: leaving
"not yet built" standing after the build would have made this document
confidently wrong about the one fact it is most likely to be read for.
