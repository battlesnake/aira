# AIRA simplification programme — sequencing the PR #7 and PR #12 proposals against the open backlog

Status: plan (v3 — after two first-hand verification passes against master `9a65d47` and one
adversarial review round (Codex/Sol) that returned BLOCK and overturned two of v2's
conclusions. Changelog in §11.)
Reviews being sequenced: `docs/superpowers/reviews/2026-09-03-subprocess-slice-management-review.md`
(PR #7, open, base `994abee`) and `docs/superpowers/reviews/2026-09-04-whole-project-simplification-review.md`
(PR #12, open, base `9a65d47`).
Backlog snapshot: `.aira/tickets/AIRA-1..77` at `9a65d47`; 42 tickets `planned`, 33 `done`, 2 `retired`.
**Drifted during authoring — see §12** (AIRA-72 landed; AIRA-78..81 filed; candidate 45 revised).
Owner instruction: *"plan out how we can do this work and in general simplify/remove stuff.
Especially removing dead code that is not likely to be activated by work landing later
from the backlog."*
Touches (this document only): `docs/superpowers/plans/`. **No code, schema or file is deleted
by this plan. It decides what may be deleted, by whom, in what order, and at what rigor.**

---

## 0. Verdict

The two reviews propose, between them, **80 discrete deletion or restructuring candidates**.
Cross-referencing every one against the 42 open tickets, the committed design documents and
`CLAUDE.md` changes the picture materially — in both directions:

| Verdict | Count | Meaning |
|---|---:|---|
| **CUT** | 48 | No open ticket touches it; no design document commits to it. Safe to remove in its phase. |
| **KEEP / DEFER** | 17 | A named open ticket, or a design invariant, depends on / extends / would populate it. |
| **UNCERTAIN** | 10 | Cannot be settled from tickets and design docs alone. Owner's explicit call. |
| **ALREADY LANDED** | 4 | Shipped in `9a65d47` (AIRA-58/59) after PR #7's base commit. Strike from the inventory. |
| **Not proposed for removal** | 1 | Listed so it is not "simplified" by mistake (the backfill freeze). |

Four findings dominate, and each is the kind of thing a bulk "delete everything with zero
rows" sweep would get wrong:

1. **PR #7 is partly stale, and one of its proposals is now a regression.** It was written
   against `994abee`; `9a65d47` (AIRA-58 + AIRA-59) landed *after* it. Verified in source:
   `runnerAdmitWaitCap` **no longer exists as a symbol**, replaced by one shared
   `runner.AdmitWaitCeiling = 24h` (`internal/runner/confine.go:40`) enforced as a *refusal*
   at four sites; queue introspection in `--list` shipped; and the family P5(2) belongs to was
   **explicitly rejected with recorded reasoning**. What remains of P3 is the `worker-admit`
   bound — which **AIRA-63 says must stay**, which is a refusal rather than a clamp
   (`internal/daemon/worker_admit.go:355-358` against `admit.go:37`), and which a guard test
   pins to exactly 30 minutes (`internal/daemon/admit_freeze_test.go:449-453`). Deleting it is
   not a simplification.

2. **The gate audit ledger's durable HEAD is not HMAC theatre — it is the only truncation
   detector, and AIRA-56's soundness rests on it.** PR #12 P2(b) proposes dropping "the durable
   `HEAD` with its own tag" along with the HMAC layer. But a hash chain detects *modification*
   and cannot detect *tail truncation*; the durable head is what closes that.
   `docs/superpowers/specs/2026-08-11-aira-m10a-gates-plan.md:210-217` is explicit: *"a
   **durable, authenticated head/commit marker** written+fsynced after each append …
   **a valid-suffix truncation (records removed from the tail) is detected because the head no
   longer matches the last chained record** — it is `E_JOURNAL_CORRUPT`, never a silent
   reversion."* **AIRA-56** asks precisely "does the ledger hold prior gate activity?" — against
   a truncated ledger that reads "this project never used gates", which is the fabricated-green
   class the whole gate subsystem exists to prevent. **Cut the HMAC tag, the key file and the
   nonce scan; keep the durable head watermark.** (§3.5, §5.5)

3. **The largest single deletion is already a ticket, that ticket's precondition may be
   unachievable as written, and the review over-reads its own authority.** PR #7's P1 *is*
   **AIRA-33**, which requires "AIRA's own dogfood suite has run clean on aitest" first —
   but **AIRA has no Python dogfood suite**: verified, the `Makefile` test targets are pure
   Go and there is no repo-root pytest configuration. AIRA-33 also requires a grep sweep
   before touching `governor.go` ("unconfirmed, **not assumed**"), undone. And the aitest
   spec §3.8 authorises deleting *"the pytest **call sites** of `aira confine-reserve`"* and
   *"`governor.go`'s **park/active-set scheduler**"* — not the verb, not the file. (§3.1)

4. **The zero-row tables are not one population.** `supervisor_leases` has zero rows and
   **AIRA-22 will make it load-bearing**. `area_hints` has zero rows but two production readers
   and a `CLAUDE.md` Phase-1 commitment. `findings` has zero rows because the loop docs never
   route agents to `aira find add` — a documentation bug, not a dead feature. `compute_events`
   has zero rows because `aira run --tool` is unused, **not** because its capture hook is
   missing (the hook exists at `run_wiring.go:139`). Meanwhile `quota_snapshots`, the five gate
   projection tables, and the six unread gate definition fields are genuinely dead. §5 separates
   them individually so they cannot be swept up together.

Recommended sequencing (§7): **Phase 0** (AIRA-76 first, then mechanical honesty wins) →
**Phase 1** (four ticketed P0/P1 correctness fixes that must precede restructuring of their
own code) → **Phase 2** (the AIRA-33 authorised deletion) → **Phase 3** (store one-truth +
gate shrink) → **Phase 4** (CLI codegen) → **Phase 5** (admission structural). Phases 3 and 5
run concurrently in separate worktrees; Phase 4 must follow Phase 2.

---

## 1. Method, and what "verified" means here

The owner's instruction turns on a distinction that cannot be made from row counts: a table
with zero rows tonight is either dead forever or freshly built and not yet exercised. The
only way to tell them apart is to read what the backlog and the merged plans intend next. So:

- **Every one of the 42 open tickets was read in full**, not by title. Status lives in JSON
  frontmatter (`.aira/tickets/AIRA-N.md`); `planned` is the open state.
- **Every proposal and every named deletion candidate in both reviews** was extracted into the
  §4 inventory — the six headline proposals *and* every smaller finding naming something as
  unused, dead, write-only, or a removal candidate. 80 rows.
- A verdict is **KEEP / DEFER only when a specific ticket or a merged plan is named.**
  "It feels useful" appears nowhere in this document.
- A verdict is **CUT only when** (a) no open ticket touches it, (b) no committed design
  document, merged plan or `CLAUDE.md` clause commits to future use, and (c) no landed
  milestone visibly intends to populate it.
- **UNCERTAIN is used deliberately and is not a hedge.** Seven items genuinely cannot be
  settled without the owner. Guessing either way on those is exactly the failure the two
  reviews were commissioned to stop.

**Confidence grading**, matching both reviews' convention: **HIGH** — stated directly in a
cited ticket, merged plan, or verified source line. **MEDIUM** — derived from two documents
each stating half of it. **LOW** — inferred; flagged, and never the basis for a CUT.

### 1.1 What was verified first-hand, and what was taken on the reviews' word

This plan's load-bearing claim is the backlog cross-reference, so the ticket side is
entirely first-hand. The reviews' own source claims were **not** re-derived wholesale — both
are careful, graded, spot-checked documents and re-doing their work is not this plan's job.

But **every source fact on which a verdict here turns was independently re-checked against
master `9a65d47`** by two dedicated verification passes (§10 lists what they established).
Those facts are marked **[v]** in §4. Facts taken on a review's word are marked **[r]**, and
**no CUT verdict rests on an `[r]` fact alone**.

Two of the five corrections that resulted reversed a verdict; one narrowed a proposal's
authority; two corrected reasoning without changing a verdict. They are recorded in §11.

---

## 2. The backlog as it actually stands

42 open tickets, grouped by what they touch — the cross-reference index the rest of the
document uses.

**Admission / ledger / confine (12):** AIRA-16, 22, 23, 24, 25, 26, 28, 29, 52, 62, 68, 70.
**aitest / worker-admit (13):** AIRA-32, 33, 35, 37, 39, 40, 41, 42, 43, 44, 45, 63, 64.
**Gates (3):** AIRA-56, 60, 72.
**Store / faces / honesty (6):** AIRA-66, 69, 73, 74, 75, 76.
**Test / CI hygiene (4):** AIRA-17, 20, 34, 65.
**Docs / skill contract (4):** AIRA-23, 27-residue, 71, 77.

Two ticket-hygiene facts matter to anyone reading `aira ls` during this programme:

- **AIRA-58 and AIRA-59 are still `planned` in frontmatter but shipped in `9a65d47`**, and
  their bodies record deployment. PR #12 §7.3 spotted this and called it "the ticket
  lifecycle's own dogfood evidence". **Action: transition both to `done` before anyone plans
  against this backlog again** — two stale open P1s in the admission area are precisely what
  would make a future agent re-do PR #7's P3.
- **AIRA-52's own final section retracts its field evidence** ("zero confirmed real-world
  firings … one concrete data point that is evidence *against* it firing"). It remains a real
  source-confirmed defect, but it must not be sized as a reported bug.

---

## 3. How the backlog changes each headline proposal

The owner's specific worry — that a proposal's savings partly come from deleting something a
ticket needs — is real in five of the six. Each is quantified.

### 3.1 PR #7 P1 — delete the xdist governance stack (~2,000 prod + ~2,300 test lines)

**Scope change: three hard preconditions added, one authority narrowed, one component split
out to UNCERTAIN. The headline line count survives; the risk profile does not.**

This proposal *is* **AIRA-33** ("aitest Slice 4 — retire aira_xdist_governor / governor-slot
/ daemon governor.go"), so the deletion is authorised backlog work rather than a review's
unilateral suggestion. But AIRA-33 and the aitest spec carry conditions PR #7 omits.

**(a) The stated blocker may be unachievable as written. [v]** AIRA-33: *"Delete … once AIRA's
own dogfood suite has run clean on aitest. … **Blocked by Slice 2** (needs AIRA's own suite
migrated and clean)."* Slice 2 is **AIRA-31**, `done`. But verification found **AIRA has no
Python dogfood suite to migrate**: the `Makefile` test targets are `go test ./...` and
friends; `.github/workflows/ci.yml` runs `fmt-check`/`vet`/`build`/`test` only; the only
`conftest.py` files in the repo are `internal/pylib/aitest/conftest.py` (contents:
`pytest_plugins = ["pytester"]`) and its `testdata/` sibling; there is no repo-root
`pytest.ini`/`pyproject.toml`/`setup.cfg`/`tox.ini`. **The precondition needs reinterpreting
before it can be discharged** — candidate readings: "aitest's own Python suite runs clean
under aitest", or "a consumer project has completed the migration". This is question 8 in §8.

**(b) The spec authorises less than the review deletes. [v]** aitest design §3.8, verbatim
(`2026-09-01-aitest-design.md:350-355`):

> "**Deleted outright** (no back-compat burden — AIRA has no users/data):
> `internal/pylib/aira_xdist_governor`, `aira governor-slot`
> (`internal/runner/governor_slot.go`), **the pytest call sites of `aira confine-reserve`**,
> **`internal/daemon/governor.go`'s park/active-set scheduler**, and the three `2026-08-30`
> scheduler-slice specs."

Note "the pytest **call sites** of `aira confine-reserve`" — not the verb — and "`governor.go`'s
**park/active-set scheduler**" — not the file. PR #7 deletes the verb and the file. Both are
defensible (verified: the plugin at `aira_xdist_governor/__init__.py:353` is the sole caller
of `confine-reserve`), but they are *widenings* of the authorised scope and should be recorded
as such rather than inherited silently.

**(c) The grep sweep is a stated precondition, and it is not done. [v]** aitest §6:

> "Whether any other AIRA workload still has a legitimate use for `internal/daemon/governor.go`'s
> CPU park/active-set machinery once `aitest` ships … **confirm with a grep sweep for other
> callers before deleting it in Slice 4, rather than assuming from this spec alone.**"

**(d) The split.** `daemon/governor.go` + `cpuslots.go` (782 lines) is graded separately
(candidate 4, **UNCERTAIN**) because two open tickets name cross-job CPU scheduling as
unfinished: **AIRA-17** ("Depends on Slice 2 landing — **the daemon scheduler + governor socket
are the substrate**") and **AIRA-64** (filed explicitly as "a signal for whatever the next
slice-scheduling milestone is"). Neither *reads* the code, and after candidates 1–3 it has no
client at all — so keeping it means keeping 782 unreachable lines. Deleting is defensible
under `architectural-simplicity`; but it closes a door on two open tickets, and this
repository's own rule is that "coverage gaps are written down and accepted by reviewers,
never silent". **Owner tick, with AIRA-17 and AIRA-64 amended to record the deletion and the
git sha.**

**(e) A documentation dependency. [v]** `README.md:97` advertises the governor as a current
feature: *"The pytest plugin cooperates with the daemon scheduler between tests, so several
sessions can all run `-n auto` while the daemon controls the active worker set."* Deleting the
scheduler makes the README false. Add the edit to Phase 2's deliverable.

**Tickets this proposal closes or re-scopes** (each verified against the ticket body):

| Ticket | Effect | Why |
|---|---|---|
| AIRA-25 | **closes — superseded** | Per-test peak/delta ledger split; per-test leases are the thing being deleted. AIRA-29 already records it as "now SUBSUMED". |
| AIRA-65 | **closes — superseded** | Its subject is `_stop_reservation`'s 1.0s budget in `aira_xdist_governor/__init__.py` and a test in `pytest_integration_test.go`. Both deleted. |
| AIRA-77 | **closes — superseded** | "`--delegate-ram` arms both the xdist governor and aitest's caps" — impossible once one of the two is gone. |
| AIRA-66 | **re-scopes** | Loses one of its two `go:embed all:` directives; the `aitest` one remains. |
| AIRA-26 | **closes — likely superseded (MEDIUM)** | Its subject is N xdist workers each importing simultaneously. aitest *forks*, so post-import pages are shared copy-on-write and the N×baseline overshoot does not arise the same way. Owner tick, not an assumption. |
| AIRA-17 | **needs explicit resolution** | Its problem (pre-plugin execnet bootstrap) is xdist-shaped; its stated substrate is deleted by (d). Close as superseded or re-scope to aitest — but do not leave it open pointing at deleted code. |

**Cross-project precondition — HIGH, and the biggest operational risk in the programme.** The
plugin ships *embedded in the `aira` binary* (`internal/pylib/extract.go:29`, `//go:embed
all:aira_xdist_governor` [v]) and is extracted per machine, so deleting it removes it for every
consumer. PR #7 flags this in passing: *"If any fastest-ee `conftest.py` still registers
`aira_xdist_governor`, it must be removed first or it will spawn a verb that no longer
exists."* **AIRA-77** confirms a live consumer mid-migration: *"Relevant to any project (like
fastest-ee) mid-transition from xdist to aitest, where **both plugins may legitimately be
present during the migration window**."* This is coordination with another project's session,
not a code change, and it belongs in Phase 2's checklist rather than being discovered on
deploy night.

### 3.2 PR #7 P3 — one timeout, honoured end to end

**Scope change: substantially landed; the residue is a proposed regression. Struck except for
one non-deletion idea.**

`9a65d47` (AIRA-58) shipped this proposal's substance: *"One shared `runner.AdmitWaitCeiling =
24h` used by the CLI, the runner and the daemon; all three clamps removed. An over-ceiling
request is now **refused** and told the bound, never silently substituted."* Verified [v]:
`runnerAdmitWaitCap` no longer exists as a symbol (only two prose comment mentions survive);
`runner.AdmitWaitCeiling` is declared at `internal/runner/confine.go:40`; the four enforcement
sites (`admission_linux.go:132-136`, `confine_reserve.go:70-71`, `cmd/aira/main.go:917-919`,
`daemon/admit.go:29`) are all refusals; and `admission_linux.go:266` puts `maxWait` on the wire
unclamped. That also discharges PR #7's own §5 B1, which AIRA-58's resolution credits to three
independent review lineages.

What remains is the `worker-admit` bound, and it must **not** be deleted. Three reasons, all
verified:

1. **It is no longer a clamp.** The 30-minute value lives at `internal/daemon/admit.go:37`
   (`workerAdmitWaitCeilingMs`, with a rationale comment at `:30-36`) and is enforced as a
   **refusal** by `validateWorkerAdmitArgs` at `internal/daemon/worker_admit.go:355-358`. PR #7
   P3's premise — a silent clamp at `worker_admit.go:349-351` — no longer describes the code.
2. **AIRA-63 says it stays**, and says why: *"`workerAdmitConnection` has **no such gate** —
   `admitSlots` appears nowhere in `internal/daemon/worker_admit.go` … `worker-admit` therefore
   keeps its own 30-minute ceiling … and `TestWorkerAdmitCeilingStaysBelowTheSharedAdmitCeiling`
   fails if someone later 'unifies' the two for consistency. **That test is a guard, not a
   fix.**"*
3. **The guard is real and pins the exact value** [v]: `internal/daemon/admit_freeze_test.go:449`,
   asserting both `workerAdmitWaitCeilingMs < admitWaitCeilingMs` and
   `workerAdmitWaitCeilingMs == 30m`. AIRA-58's resolution adds the second, independent reason:
   worker-admit's client "wraps any non-OK response as `E_CONFINE_UNAVAILABLE`, which makes the
   aitest supervisor disable daemon admission and run **unconfined**."

**Verdict: P3 is struck except for its one genuinely-open idea** — the "queued {reserve, basis,
position}" ack frame, which is not a deletion at all but the implementation of **AIRA-24**'s
asks (1) and (3). It moves to that ticket.

### 3.3 PR #7 P5 — fairness freeze

**Scope change: entirely landed or explicitly refuted. Struck.**

- P5(1) "remove per-test waiters from the queue" — subsumed by P1 / AIRA-33.
- P5(2) "proportional grace `max(5min, requested/10)`" — AIRA-59 shipped a *different,
  reviewed* bound (`admitFreezeMaxHold`, a queue-level duty cycle) and recorded why the 60s
  grace is deliberately untouched: *"The duty bound subsumes it, and raising it would weaken
  the head-of-line protection this subsystem currently gets right."* The size-scoped variant
  was rejected with an argument: *"large-head starvation comes **specifically** from small
  waiters, since they are the only ones that fit in the crumbs the head is accumulating."*
  Re-proposing a grace change now relitigates a reviewed decision without new evidence.
- P5(3) "show the queue" — shipped: *"`confine --list` now reports queued waiters and freeze
  phase (read in one locked snapshot with the ledger, so the summary cannot contradict
  itself)."*

### 3.4 PR #12 P1 — generate the CLI from `ArgSpec` (−1,800 prod lines)

**Scope change: two carve-outs and one ordering constraint. Line count roughly preserved;
risk profile is not.**

**Carve-out A — `worker-admit` and `aitest-bootstrap` (KEEP / DEFER).** These are two of the
eleven verbs P1 folds into the dispatch table with a new `Face` field (verified absent from
the table [v]; CLI-only at `cmd/aira/main.go:139/146` and `:513/516`). **Five open tickets are
actively rewriting their CLI surface and response contract:**

- **AIRA-42** — redesign the Go-CLI→Python classification boundary as structured `k=v` instead
  of substring-matched prose. This *changes what `worker-admit` prints*.
- **AIRA-43** — no test pins the granted-path lease-hold contract; the fix builds real-cgroup
  CLI-subprocess tests.
- **AIRA-45** — the `E_DAEMON_PROTOCOL` classifier bucket conflates two conditions.
- **AIRA-37** — `runWorkerAdmitCommand` sets no context deadline.
- **AIRA-44** — `runAitestBootstrapCommand` discovers the wrong outer scope.

All five land in `cmd/aira/main.go` and the Python classifier. Folding those two verbs into a
generated parser simultaneously is a guaranteed collision on the repo's highest-churn file
(`main.go`, 63 commits). **Either land AIRA-42/43/45 first, or explicitly exclude these two
verbs from the codegen pass.**

**Carve-out B — `main.go:857-859` is AIRA-62, an open P1 correctness bug, and must land
first.** P1 rewrites exactly those lines inside a 1,800-line refactor. AIRA-62 is the "asked
for X, silently got Y, told nothing" class: *"`if maximum > 0 { reserve, reservePinned =
maximum, true }` — unconditional, no delegate-ram guard … an explicit `--memory-reserve` must
never be silently discarded."* Its body warns the fix "is a memory-accounting change in the
**over-commit direction** … That safety argument deserves its own adversarial review rather
than riding along in a PR already spanning daemon, runner and CLI." Riding it inside the CLI
codegen instead is strictly worse. **AIRA-62 first, standalone, full two-loop;** the codegen
then encodes the resulting rule as an `ArgSpec` flag-combination constraint.

**Ordering constraint — settle P5's `time` decision first.** P1 replaces `parseTimeArgs` with
a generic parser; P5 may delete the `time` verb entirely. Wrong order builds then deletes the
same code. (Related [v]: the command-telemetry spec explicitly reserved `exec`/`aira_exec` for
`internal/interp` and named the verb `time` for that reason — deleting `time` should not
re-open that naming question by accident.)

Not affected, and this is the proposal's best justification: the stub verbs, unreachable
`buildRequest` cases, `parseInstallDescriptorArgs`, `argAccessor.reads`, the ignored
`--gate_id`/`--canary_id` flags and the eight verbs missing from `help` have **no open ticket
and no design commitment**. All CUT. (Verified detail worth carrying: `install` is *both*
early-intercepted at `main.go:58` **and** a dispatch stub at `core.go:1766` — the one verb in
both lists.)

### 3.5 PR #12 P2 — shrink gates to the proven core (−2,000 prod lines)

**Scope change: one deletion must be split (a durable-HEAD carve-out), one is blocked behind an
open P0, two become owner forks rather than deletions, one had its ticket effect mis-stated,
one field group moves to UNCERTAIN, and one deletion needs a replacement rather than a removal.
The realistic saving is below −2,000 lines but not dramatically so.**

**v2 ERROR, CORRECTED — the "merged plan retains ratchet and fixture canaries" reading was an
over-read.** v2 of this document flipped P2(d) and P2(f) to KEEP on the strength of two lines in
`docs/plans/2026-09-03-aira53-54-gate-honesty-plan.md` (merged `cf81344`): *"Ratchet gates remain
hand-authored"* (`:334`) and *"Fixture canaries (seed file trees) remain hand-authored; only
mutation canaries are materializable from flags"* (`:335`). **Verified in context, both sit under
a `## Deferrals (explicit)` heading whose subject is what `gate add`/`set` **flags** can
materialise** — the neighbouring bullets are "No `--name`, `--lane`, `--max-age-secs` … flag
family" and the `E_GATE_*` code registration. They mean *"you must still hand-edit the file;
the new flags will not generate these"*, **not** *"this feature must never be removed"*. A
scope deferral in a flag-surface ticket is not a veto on a later architectural deletion.

The correction matters beyond these two rows, so it is stated as a method rule: **a merged
plan's "deferrals" section records what that ticket did not do, which is weaker evidence of
future intent than a ticket that asks for the thing.** v2 treated the two as equivalent. They
are not. (§5.8 records this as the one place this document got the owner's own question wrong.)

**Revised — P2(d), the ratchet kind: UNCERTAIN, coupled to candidate 62.** No open ticket, no
merged plan protecting it, and zero test reports to ratchet over. What remains on the keep side
is design §119 (*"Ratchet gates compute over a **durable, pinned** baseline … A missing baseline
reads `unevaluated` (never a vacuous pass)"*) and the fact that its data source is unwired
rather than absent. **Decide it with candidate 62, not alone**: if test reports get a producer,
the ratchet has a baseline within a milestone; if they do not, both go.

**Revised — P2(f), the `fixture` canary mode: UNCERTAIN, an explicit owner fork.** Open
**AIRA-60** is genuinely about the fixture seed — `Seed.Files`/`Seed.Path`, the `.git/config`
`core.fsmonitor` vector, and `safeFixturePath` at `gate_eval.go:491`/`:500` — so **PR #12's
claim that AIRA-60 is "unaffected" is false in both directions.** But an open ticket *to
validate a feature's inputs* is not by itself a reason to keep the feature: deleting the mode
resolves AIRA-60 as superseded, exactly as this plan already does for AIRA-65. The two coherent
positions are **keep the mode and land AIRA-60**, or **cut the mode and close AIRA-60 as
superseded**. What is not coherent is PR #12's position — cut the mode and call AIRA-60
unaffected.

**BLOCKED — P2(e), collapse the manual-attestation ritual, must follow AIRA-72.** AIRA-72 is an
open **P0**: the subject digest for manual, ratchet and dimension gates covers only tracked
`*.go` plus `.aira/requirements/*.md`, so on a non-Go repository *"a stored `pass` from a
manual attestation or a ratchet ships and then simply never invalidates"*. P2(g) *is* AIRA-72.
P2(e) reshapes the very record AIRA-72 must re-bind. Landing a schema reshape on top of an
unfixed fabricated-green P0 is the AIRA-53/54 pattern again. **AIRA-72 first**, or fold P2(e)
into AIRA-72's plan. (Manual attestation itself is design-committed: §97 *"un-attested manual
→ `unevaluated`; attested → `pass`"*, §121 waiver-acceptance attestations — so P2(e) is a
*simplification of the ritual*, never a removal of the checker.)

**REPLACEMENT, NOT DELETION — P2(a)'s `gates` table half.** The five projection tables
(`gate_results`, `gate_proofs`, `gate_attestations`, `gate_baselines`, `gate_baseline_active`)
are confirmed write-only in non-test code [v] and are a clean CUT. But the `gates` table has
**exactly one production reader** — `internal/store/rant.go:510`, validating a `rant --gate`
ref — and the same merged Sept-3 plan **withdrew its own deferral of the projection rebuild**
precisely because of it (`:244-253`):

> "v1 deferred the SQLite gate-projection rebuild. Review correctly objected: a successful
> `add` would be immediately unusable by another documented operation, because `rant --gate
> <id>` validates the ref against the `gates` table (rant.go:510, verified). `add`/`set`
> therefore call `rebuildGateProjection` after a successful write, and the returned result
> carries an honest three-state `IndexStatus`."

So the `gates` table can be replaced by a filesystem existence read, but **the replacement must
preserve the honest three-state `IndexStatus`** the merged plan shipped, and `rebuildGateProjection`
cannot simply be deleted without giving `gate add`/`set` an equivalent. Candidate 41 is CUT
*with a named replacement obligation*, not a bare removal.

**MOVED TO UNCERTAIN — `AppliesTo.{LifecycleStep,Ticket,Milestone,Labels,Paths}`**, five of
P2(c)'s nine dead fields. The review's own framing is the reason: *"Today every gate applies to
every ticket … **Either the selector is implemented or it is removed**; a definition file
describing behaviour that does not exist is the AIRA-53 class."* That is a fork, not a
deletion, and it interacts with **AIRA-56**. The remaining six fields (`Enabled`, `Advisory`,
`FailureGuidance`, `Manual.{Role,EvidenceKinds,PromptID}`, `ProofPolicy.RequireCurrentCanary`,
`CanaryDeclaration.Cadence`) have no such fork and are CUT.

**SPLIT — P2(b) must not delete the durable HEAD.** The HMAC tag, `hmac.key` and the O(n)
nonce-uniqueness scan go. **The durable head/commit marker stays** (its own HMAC tag can go with
the rest). PR #12 lists it among the layers to drop; that is wrong, and the reason is in the
gate spec, `2026-08-11-aira-m10a-gates-plan.md:210-217`:

> "a **durable, authenticated head/commit marker** written+fsynced after each append.
> Verification checks the chain from genesis to the durable head; **a valid-suffix truncation
> (records removed from the tail) is detected because the head no longer matches the last
> chained record** — it is `E_JOURNAL_CORRUPT`, never a silent reversion to an older record."

A hash chain proves that the records present are unmodified and correctly linked. It **cannot**
prove that records are not missing from the tail — for that you need an out-of-band watermark.
This is not a theoretical distinction here: **AIRA-56**'s entire mechanism is "does the ledger
hold prior `result`/`proof-of-fire` records for this project?", and a truncated ledger answers
*no*, which AIRA-56 would read as "this project never used gates" — a fabricated green in the
exact class AIRA-53/54/72 were filed for. The HMAC *tag* on the head is the part that proves
nothing extra on a single-user box; the head itself is load-bearing.

The ledger, the hash chain, the durable head, and the `result` / `proof-of-fire` record kinds
**must survive**, because **AIRA-56**'s only sound direction reads them:

> "Use durable evidence of prior gate activity rather than the current filesystem state: the
> authenticated gate audit ledger … If the ledger holds prior `result` or `proof-of-fire`
> records for this project but discovery now yields zero gates, the gate set regressed."

and the merged gate-honesty plan `:328` records the same deferral to AIRA-56. That is
compatible with P2(b), but it is a written constraint on the implementation, not something
AIRA-56's builder should discover afterwards.

### 3.6 PR #12 P3 — one truth per entity in the store (−1,800 prod lines)

**Scope change: four of its seven sub-proposals are already open tickets, and one diverges
from its ticket's stated preference.**

| Sub-proposal | Ticket | Effect |
|---|---|---|
| P3(a) FTS half | **AIRA-74** (P1, open) | The proposal and the ticket **disagree on the fix**. |
| P3(d) journal purity | **AIRA-75** (P2, open) | Ticket is open on *whether* it is a gap at all. |
| P3(e) one writer | *unfiled* (PR #12 B5) | File it — and see §5.1, AIRA-22 raises its priority. |
| P3(f) outbox resolution | **AIRA-73** (P1, open) | Ticket explicitly defers to this plan. |

The divergence deserves plain statement. **AIRA-74** asks for *"incremental indexing, or at
minimum a per-project rather than machine-wide lock"*. **P3(a)** proposes the opposite: accept
that the index is rebuilt per query and make it an in-memory table, deleting the persistent
`search_fts` and the machine-wide lock. Both fix the contention; they are different products.
**This is an owner call inside AIRA-74, not something a simplification programme decides
unilaterally** — candidate 49, UNCERTAIN.

**AIRA-73** is unusually cooperative and tells this plan what to do:

> "Needs tracing to find where resolution SHOULD be written (or whether the outbox mechanism
> itself needs reconsidering — see PR #12's broader 'one truth per entity' proposal, which
> recommends deleting several write-only tables; **this may be one of them**, or it may need an
> actual fix if the outbox mechanism is meant to stay)."

Answer, to be written back into the ticket: **the outbox stays.** PR #12 §3.1 keeps the write
protocol verbatim ("Keep all of it — §4 P3 is about what sits *beside* it, not this"), and
`CLAUDE.md` names crash recovery as a two-loop-mandatory class. `outbox` is the crash-recovery
intent log, not a write-only projection. Only `resolution` lacks a writer. AIRA-73 is a bug fix
at full rigor, not a deletion.

P3(b) (seven counter tables → one) and P3(c) (delete the migrations) have **no open ticket and
an explicit owner rule behind them** — the standing "spend ZERO effort on data-migrations… just
define the new shape" and `aira-not-live-no-compat`. Both CUT. But P3(b) is **ID and sequence
allocation**, which `CLAUDE.md` names by hand as full-two-loop work; its low line count must
not be mistaken for low risk (§6).

---

## 4. The inventory — every candidate, verdict, reasoning

This table is the deliverable. `[v]` = the source fact behind the verdict was independently
re-checked against `9a65d47` for this plan; `[r]` = taken on the review's word (never the sole
basis for a CUT).

### 4.1 PR #7 — subprocess / slice management

| # | Candidate | Verdict | Reasoning (one line) |
|---|---|---|---|
| 1 | `internal/pylib/aira_xdist_governor/` (435 py lines) | **CUT** | Authorised by **AIRA-33** and aitest §3.8 `[v]`; aitest replaces it. Preconditions in §3.1 — especially the cross-project `conftest.py` check. |
| 2 | `aira confine-reserve` verb (`runner/confine_reserve*.go` + dispatch) | **CUT (widening — record it)** | Sole caller is the deleted plugin (`__init__.py:353`) `[v]`. §3.8 authorises deleting the *call sites*; deleting the verb goes further and should be recorded as a decision. Closes **AIRA-65**; removes AIRA-25's subject. |
| 3 | `aira governor-slot` relay (`runner/governor_slot*.go`) | **CUT** | Per-worker relay for the deleted plugin; named verbatim in **AIRA-33** and §3.8. |
| 4 | `daemon/governor.go` + `cpuslots.go` (782 lines) | **UNCERTAIN** | **AIRA-17** names "the daemon scheduler + governor socket" as its substrate; **AIRA-64** is filed as input to the next scheduler milestone. Neither reads it, and it has no client after 1–3 — but deleting closes a door on two open tickets, `README.md:97` advertises it `[v]`, and AIRA-33's grep sweep ("unconfirmed, **not assumed**") is undone. **Owner tick + amend both tickets with the git sha.** |
| 5 | `AIRA_GOVERNOR_*` / `AIRA_TEST_MEM_*` / `AIRA_CONFINE_RESERVE_CMD` env plumbing (`pylib/env.go:74,96,150-167`) | **CUT** | Coordinates for 1–3 `[v]`. Closes **AIRA-77** (both-plugins double-arming). |
| 6 | `governor` verb in `server.go`; `admitAvailable`; `governor.signal()` hook | **CUT with #4** | Only consumer of `admitAvailable` is the governor. Falls with #4, not before. |
| 7 | `internal/pylib/pytest_integration_test.go` (928 test lines) | **CUT** | e2e for the deleted plugin; it is also `aira_xdist_governor`'s *only* test coverage `[v]`. Removes the **AIRA-65** flake and one **AIRA-20** `-race` hardening target. |
| 8 | The three 2026-08-30 scheduler-slice spec *implementations* | **CUT with #4** | Same body of code; keep the specs as history per the repo's documentation convention. |
| 9 | Flock fallback: `admitWithFlock` (`admission_linux.go:155`), `tryAdmissionLock` (`:797`), `admissionLockDir`, `admissionResult.lock` | **CUT** | No open ticket depends on it; **AIRA-16(1)** wants its uncapped class gone; AIRA-58's resolution records that unrecognised daemon codes still route into it, "turning a refusal into an unaccounted, uncapped launch". Partially narrowed in `9a65d47` — the *pinned* branch (`confine_linux.go:664`) no longer requires `lock == nil`, but the *unpinned* branch (`:676`) still does `[v]`. **Highest-value remaining PR #7 item.** |
| 10 | "timed out → launch anyway" mapping (`confine_linux.go:529-530`) | **CUT with #9** | Same path; the daemon reports the identical condition as terminal `E_ADMIT_SATURATED`. |
| 11 | Client clamp `runnerAdmitWaitCap = 30m` | **ALREADY LANDED** | Symbol no longer exists `[v]`; replaced by `runner.AdmitWaitCeiling` (`confine.go:40`), enforced as a refusal. |
| 12 | Daemon clamp `admitWaitCapMs` | **ALREADY LANDED** | Same `[v]`; `daemon/admit.go:29` derives from the shared ceiling. Refusal code `E_ADMIT_WAIT_TOO_LONG` chosen so it cannot degrade into #9. |
| 13 | `worker-admit` 30-minute bound | **KEEP / DEFER — AIRA-63** | Deliberately split, and **already a refusal, not a clamp** (`admit.go:37` → `worker_admit.go:355-358`) `[v]`. A guard test pins the exact value (`admit_freeze_test.go:449-453`) `[v]`. **Deleting this is a regression, not a simplification.** Revisit only after AIRA-63 gives worker-admit a concurrency bound. |
| 14 | In-memory `outstanding`/`adopted` ledger pair → scan-derived charge | **KEEP / DEFER — AIRA-29, AIRA-68** | AIRA-29 (the vehicle PR #7 P4 itself names) is explicitly **ON HOLD** by owner decision pending aitest, with a twice-reviewed plan banked at `aira29-dynamic-reserve`. AIRA-68 is an open **P0** ledger reserve leak with unknown root cause; restructuring the ledger first destroys the evidence. |
| 15 | `admit_reconstruction*` (restart adoption) | **KEEP / DEFER — with #14** | #74's machinery; falls only if the scan becomes the ledger. |
| 16 | Stale-lease sweep (`releaseStaleGrantedLeasesPass`) | **KEEP / DEFER — with #14** | AIRA-49's fix, and a candidate mitigation for the open **AIRA-68** leak. |
| 17 | `freshConfineOwner` + registry merge (owner encoded in scope dirname) | **KEEP / DEFER — AIRA-52** | Do it *as* AIRA-52 — small, independent, retires the ticket. PR #7 P4 correctly calls it "the first slice". Note AIRA-52's own retraction of its field evidence (§2). |
| 18 | The backfill freeze itself | **not proposed for removal** | PR #7 §4 P5 argues to keep it; AIRA-59 bounded it. Listed so nobody "simplifies" it. |
| 19 | Proportional backfill grace replacing the flat 60s | **ALREADY LANDED / REFUTED** | AIRA-59 shipped `admitFreezeMaxHold` and recorded why the 60s grace is deliberately unchanged and why size-scoped freezing is backwards (§3.3). |
| 20 | Queue introspection in `--list` | **ALREADY LANDED** | AIRA-59 suggestion (4), shipped with a locked snapshot. |
| 21 | Watchdog `whale-watchdog` systemd interlock (`watchdog.go:886, 892, 926`) | **CUT — Tier B, not C** | Whale removed from code and machine (#55); no ticket. Also a live hazard (B8: a D-Bus round trip inside the kill path on a thrashing box degrades the killer to observe) `[v]`. **It edits the live watchdog kill path — the one component that SIGKILLs real processes on a shared desktop — so it does not qualify as mechanical.** |
| 22 | Watchdog PSI read paths (`readHostPressureFull` `:110`, `eventWithPSI` `:297`) | **UNCERTAIN** | **AIRA-16(2)** asks for a slice-internal-pressure trigger, "`aira.slice memory.current` near `memory.max`, **or slice PSI**". The code being deleted reads *host* PSI for event decoration — a different file and purpose — so this is probably safe, but it is the only PSI parser in the tree `[v]`. **Recommend: cut the decoration plumbing, keep or re-derive the ~20-line parser.** Owner's call. |
| 23 | Watchdog audit events into every project journal → one daemon log | **KEEP / DEFER — AIRA-75** | Same change as PR #12 P3(d). AIRA-75 is open and explicitly undecided on whether it is "a deliberate omission … or a real gap". Do it under the ticket. |
| 24 | `main.go:857-859` unconditional `reserve = --memory-max` | **KEEP / DEFER — AIRA-62** | Not a deletion; an open **P1** whose own body demands a standalone adversarial review. Must precede PR #12 P1 (§3.4). |
| 25 | `@dr` adoption special case in `admit.go` | **KEEP / DEFER — with #14** | PR #7 conditions it on P4 landing. AIRA-28's banked Approach A uses a versioned `@drc-` marker; do not churn the marker vocabulary while AIRA-29 is on hold (§5.7). |
| 26 | Two launch paths (`aira run` vs `aira confine`) — direction only | **KEEP / DEFER — AIRA-22** | PR #7 P8 names AIRA-22 (`confine --detach`) as the forcing function. AIRA-22 requires `run --detach`/`__supervise`/`LaunchDetached` to stay (§5.1). Multi-milestone; out of this programme. |
| 27 | `E_ADMIT_TOO_LARGE` headroom asymmetry (B7) | **KEEP / DEFER — with #14** | Known and documented in #74; P4 removes the distinction. LOW; not worth a standalone change. |
| 28 | `--list` reports `0 granted` when the queue is empty (B9) | **UNCERTAIN** | Cosmetic, and `9a65d47`'s `--list` rework touched this code. `[r]` — **re-verify against current master before filing.** |

### 4.2 PR #12 — everything else

| # | Candidate | Verdict | Reasoning (one line) |
|---|---|---|---|
| 29 | `parseArgs` + `allowed` map + inline boolean/list flag lists | **CUT** | Hand transcription of `ArgSpec`; no ticket. Subject to §3.4's carve-outs. |
| 30 | `buildRequest` (660 lines) | **CUT** | Same. |
| 31 | Per-verb parsers `run`/`time`/`git`/`confine*` | **CUT, ordered** | `time`'s is contingent on candidate 61's decision; `confine*`'s on **AIRA-62** landing first. |
| 32 | `parseInstallDescriptorArgs` (unreachable) | **CUT** | `install` is intercepted at `main.go:58` `[v]`. No ticket. |
| 33 | `buildRequest` cases `init` and `confine` (unreachable) | **CUT** | Built by hand elsewhere. No ticket. `[r]` |
| 34 | `argAccessor.reads` (recorded, never consumed) | **CUT** | No ticket, no reader. `[r]` |
| 35 | The six `E_*_UNAVAILABLE` dispatch stubs (`core.go:716, 1717, 1731, 1739, 1751, 1766`) | **CUT (5 of 6)** | `confine`, `confine-list`, `confine-kill`, `install`, `eject` become real `Face`-tagged entries `[v]`. `confine-reserve`'s stub disappears with candidate 2. Note `install` is uniquely *both* stub and early-intercept. |
| 36 | MCP zero-default + coercion special-case lists | **CUT** | Derivable from `ArgSpec.Kind`; ~80 lines of hand-copied `buildRequest`. |
| 37 | The eleven out-of-table verbs → real entries with a `Face` field | **KEEP / DEFER (2 of 11)** | `worker-admit` and `aitest-bootstrap` carved out — **AIRA-37/42/43/44/45** are rewriting their surface (§3.4). The rest (`daemon`, `mcp`, `skill`, `tui`, `watch`, `__supervise`, `__confine-setup`, `__slice-anchor`) proceed; `governor-slot` is deleted by candidate 3 instead. All eleven verified absent from the table `[v]`. |
| 38 | `applyDispatchMetadata` merged into table entries | **CUT** | The `panic("missing dispatch metadata")` is itself evidence they should be one struct. |
| 39 | `routing.Classify` / `StoreFreeCarved` / `Live` lists → descriptor fields | **CUT** | Same declaration, three copies. |
| 40 | Five write-only gate projection tables + their `rebuildGateProjection` writes + six `projectOwnershipTables` entries | **CUT** | Confirmed write-only in non-test code `[v]` (`gate_index.go:225/236/247/256-258` write; only tests SELECT). No ticket reads them — **AIRA-56** uses the *ledger*, not the projection. Also closes **B7** (a corrupt gate ledger currently aborts ticket `reconcile`). |
| 41 | The `gates` table itself (one reader: `rant.go:510`) | **CUT — with a named replacement obligation** | Replace with a filesystem existence read, **but the merged Sept-3 plan added the projection rebuild to `gate add`/`set` precisely for this reader and shipped an honest three-state `IndexStatus`** (`:244-253`) `[v]`. The replacement must preserve that result contract; `rebuildGateProjection` cannot simply vanish (§3.5). |
| 42 | Gate ledger HMAC tag + `hmac.key` + O(n) nonce scan | **CUT — with a KEEP carve-out; Tier A** | The tag proves what the hash chain already proves on a single-user box (M10 §4.4 concedes the adversary). **But the durable `HEAD` is NOT part of the cut** — it is the only truncation detector (`m10a-gates-plan.md:210-217`, enforced at `gate_audit.go:433-445`) `[v]`, and a truncated ledger reads to **AIRA-56** as "never used gates", i.e. a fabricated green. Ledger, chain, durable head, and the `result` / `proof-of-fire` record kinds all survive (§3.5). |
| 43 | Six dead gate definition fields (`Enabled`, `Advisory`, `FailureGuidance`, `Manual.{Role,EvidenceKinds,PromptID}`, `ProofPolicy.RequireCurrentCanary`, `CanaryDeclaration.Cadence`) | **CUT** | Validated and digested, never read `[r]`; `W_GATE_DISABLED` catalogued and never emitted. No ticket, no plan retains them. |
| 44 | `AppliesTo.{LifecycleStep,Ticket,Milestone,Labels,Paths}` | **UNCERTAIN** | The review's own fork: implement the selector or delete it. Interacts with **AIRA-56** (when a gate set constrains readiness). Owner call. |
| 45 | Ratchet kind (`gate_ratchet.go`, baseline tables, `baseline-pin`/`baseline-show`, `synthetic-ratchet` canary, `ratchet-status` gauge) | **KEEP / DEFER — AIRA-78 (P0)** *(revised in §12)* | v2 read the merged plan's *"Ratchet gates remain hand-authored"* as retention; that line is a **flag-surface deferral**, not a keep decision (§3.5) `[v]`. v3 therefore graded this UNCERTAIN. **§12 supersedes both: `AIRA-78`, filed during this document's authoring, is an open P0 fabricated-pass bug *in `evaluateRatchet`*, with a fix direction and an explicit "needs its own adversarial loop".** That is an open ticket depending on the ratchet existing. Also design §119. |
| 46 | Manual-attestation two-record ritual → one `attest --verdict` record | **KEEP / DEFER — AIRA-72** | Open **P0** re-binds the same record's subject digest; AIRA-72 first, or fold this into its plan (§3.5). The *checker* is design-committed (§97, §121); only the ritual is a simplification target. Also fixes **B11**. |
| 47 | `fixture` canary mode + `copyFixtureSeed` | **UNCERTAIN — owner fork** | Open **AIRA-60** is genuinely about the fixture seed path, so **PR #12's "AIRA-60 unaffected" is false** `[v]`. But a ticket to validate a feature's inputs is not itself a reason to keep the feature — cutting the mode closes AIRA-60 as superseded, as this plan already does for AIRA-65. Two coherent positions: **keep + land AIRA-60**, or **cut + close AIRA-60**. PR #12's own position (cut, and call AIRA-60 unaffected) is not one of them (§3.5). |
| 48 | `tickets`/`relations`/`requirements` projection tables, `checkStaleIndex`, divergence checks, `W_STALE_INDEX`, four `check` dimensions | **CUT** | No open ticket reads them; the read path is the working-tree scan by the deliberate #8-part2 decision. **High rigor** — touches `Rebuild` and `Check` (§6). |
| 49 | Persistent `search_fts` + machine-wide `search-rebuild.lock` | **UNCERTAIN — AIRA-74** | The ticket asks for *incremental indexing*; the proposal asks for *in-memory rebuild-per-query*. Both fix the contention; they are different products (§3.6). Owner call inside AIRA-74. |
| 50 | Seven counter tables → one `counters(project_id, kind, next)` | **CUT** | No ticket; design §6 names one `seq` authority. **Full two-loop — this is ID/seq allocation** (§6). |
| 51 | Seven `ensure*` migrations, `schema_ownership.go`, pre-M9/pre-M21 compat branches | **CUT** | Backed by the owner's standing no-compat rule and `aira-not-live-no-compat`. Replace with `schema_version` + a loud `E_SCHEMA_INVALID: delete state.db`. |
| 52 | Watchdog + rant journal purity | **KEEP / DEFER — AIRA-75** | Same as candidate 23; one ticket, one fix, both reviews' finding. |
| 53 | Detached supervisor writing `state.db` directly (third writer) | **CUT the escape hatch — KEEP the machinery; Tier A** | Verified `[v]`: `runSupervisor` opens its own store (`main.go:387`) and reaches `AddTestReport`/`AddComputeEvent` via `run_wiring.go:261/272/391`. Fold through the store-op relay (~50 lines; closes B5 and the D5↔D7b mutual deferral). **Tier A, not B: it changes detached-run settlement, which is crash-recovery-shaped.** **See §5.1 — AIRA-22 makes this path load-bearing, so do it now while it is small.** No ticket; file one. |
| 54 | `outbox.resolution` never written | **KEEP / DEFER — AIRA-73** | The outbox **stays** (crash-recovery core; PR #12 §3.1 keeps it verbatim); only the missing writer is the bug (§3.6). Full rigor. |
| 55 | Duplicated primitives: `stableReadFile` ×4, SHA-256 ×4, git runners ×3, `syncDir`/`syncGateDir`, three query mini-grammars, `Options` vs `ScopeOptions`, `OpenReadOnly` duplication | **CUT** | Mechanical, no ticket. **Constraint: `stableReadFile` carries the #8-part2 honesty semantics — dedup must not weaken torn-read handling or the `inconclusive` outcome.** One of the three query grammars (`command.go:161`) dies with candidate 61. |
| 56 | Dead face symbols (`runTUIWithScreen`, `executeLaunchOnUI`, `submitPalette`, `paletteConfirmForm`, `inlineStageConfirm`, `viewEvents`, `NewWithRunnerInput`, `ValidSafetyClass`, `minSamples`) | **CUT** | Traced dead `[r]`. |
| 56b | `runMCP` and `GenerateSkill` specifically | **CUT** | v2 held these pending an entry-point check; the check is done `[v]`. The live CLI calls `runMCPWithDispatcher` and `runSkill` (`cmd/aira/main.go:74-78`); `runMCP` is an unused wrapper (`cmd/aira/mcp_project.go:17-19`) and `runSkill` calls `GenerateSkillArtifacts`, not the `GenerateSkill` alias (`cmd/aira/skill.go:16-18,51`; `internal/core/skill.go:106-109`). **AIRA-71 edits generated content, not these wrappers.** |
| 57 | `dispatchClient`'s twin branches + double Core construction when `outputCap>0` | **CUT** | No ticket. |
| 58 | Three git-discovery copies (`Discover`, `DiscoverBootstrap`, `PrepareInit`) → one | **CUT** | No ticket. `PrepareInit` serves `init`: collapse, do not delete. |
| 59 | Two state-dir resolvers + three `WorktreeScope` builders | **CUT** | They disagree when `HOME` is unset — a latent bug, not just duplication. |
| 60 | Response-consistency validation in `write_relay_store.go:119-254` | **CUT** | Validating that the same binary, over a 0600 socket in the user's own runtime dir, returned self-consistent counts. **Keep the size bounds** (`boundedCheckReport`, `maxRelayedTestResults`) — memory, not trust. |
| 61 | Command events: `aira time`, `aira commands`, `command_events`, `command-latency` gauge, the third query grammar (~940 lines) | **UNCERTAIN — owner decision** | Zero rows, zero open tickets, and design §12/Phase 4 is discharged, not forward-looking `[v]`. **But `docs/dev/agentic-development-loop.md:44-50` still instructs every agent to "Run ordinary development commands through `aira time -- <cmd>` … so the retained command-frequency and latency evidence reflects the loop"** `[v]`, while `CLAUDE.md:70-73` mandates `aira confine`, and nothing reconciles them. `internal/core/command.go:342` is the sole producer `[v]`. **Recommendation: take the dogfood option** — record one `command_event` from `aira confine`, which is what actually runs. Owner decides. |
| 62 | Test reports table **vs** the cell-level flaky classifier — **split** | **UNCERTAIN (table) / CUT (classifier)** | v2 upgraded this to KEEP on design §120; **that was wrong** `[v]`. The `tests-green` predicate parses one run inline (`gate_command.go:145-159`, `classifyTestsGreen` → `gate.ParseGoTestJSONV1`) and the command-gate design says so explicitly: *"`tests-green` is evaluated over exactly one command run. **It does not compare against a baseline or an archive**"* (`m10b-command-gates-design.md:244`). So the DB sink is **not** a precondition of the gate rule. What remains: `run --report` writes the table (`run_wiring.go:261`), and candidate 45's baselines would need it. **The flaky classifier has no ticket, no producer and no gate dependency — CUT it.** The table is an owner fork with 45. **Correction to the review's premise:** aitest does *not* emit JUnit itself — it replays pytest `TestReport` objects into pytest's stock `junitxml` plugin `[v]`, so the dogfood wire is "`--junitxml` + an ingest step", not "already emits". |
| 63 | Findings (`aira find add`, `.aira/findings/`, waivers, JSONL import) | **KEEP — and fix the docs, not the code** | Zero rows, and verification found the reason: **`aira find add` and `aira review` appear nowhere in `agentic-development-loop.md`, `review-and-merge-policy.md` or `CLAUDE.md`** `[v]`. The verb is design-committed (§16: *"The agent reports the verdict via `aira find add … --source codex`"*) and is design pain #2. **The honest fix is a documentation change routing the loop to it, not a deletion.** |
| 64 | Compute events | **UNCERTAIN — decide with AIRA-22 / run-convergence** | v2's rationale ("the capture hook was never built") is **wrong** `[v]`: `wireRunCompute` is live and writes a ComputeEvent whenever `aira run --tool` is set (`run_wiring.go:120-139`, `:359-402`). Zero rows means **the `aira run` surface itself is unused** — which is the same fact candidate 26/§5.1 is about, and **AIRA-22** (`confine --detach`) is the ticket most likely to change it. Assess jointly with run-convergence, not as an orphaned telemetry sink. |
| 65 | Quota snapshots + the `quota` verb + its gauge | **CUT** | Design §134 verbatim: *"`aira quota` records **opt-in** provider-supplied `QuotaSnapshot`s … AIRA records what an ingester hands it, never scrapes"* `[v]`. Zero rows, no producer, no open ticket, no motivating incident, Phase 4 discharged. **The clearest telemetry cut.** |
| 66 | Insights gauge registrations with no producer | **CUT (registration only)** | Register only gauges whose source table has a producer. **Keep the registry and the `unevaluated` discipline** — that is design §17 working, and `aira insights` is a live dogfood surface. Follows 61/62/64/65. |
| 67 | `rant_git_context` EAV table → inline columns; `rant_context_refs` → JSON | **CUT (tidy)** | Rant is genuinely used (17 rants, 102 context rows). No ticket. **Keep redaction (`secure_delete`), idempotency keys, the append-only review trigger.** |
| 68 | aitest activation contract (`skill.go:326` vs `confine_linux.go:757-778`) | **KEEP / DEFER — AIRA-71** | Open **P0**, filed, and it needs an owner decision between "fix the code" and "fix the doc" — with **AIRA-77** complicating the doc option. Not a deletion. |
| 69 | `go:embed all:` baking `__pycache__` / `.pytest_cache` into the binary | **KEEP / DEFER — AIRA-66** | Open ticket; broaden to "embed only what ships" per P7(ii). Candidate 1 removes one of the two `all:` directives (`extract.go:29`) `[v]`. |
| 70 | The Go test that shells out to run the whole Python suite a second time | **CUT (restructure)** | No ticket. Interacts with **AIRA-20** (`-race` re-add) and dies partly with candidate 7. |
| 71 | `internal/store` → `internal/runner` `Execution` seam types | **CUT (move)** | Layering violation across five production files; retype with store-owned types. Medium risk — it is the command-gate seam. |
| 72 | `internal/store`'s git shelling (`discoverWorktrees`, `runGit`, `validGitRoot`) | **CUT (move)** | Belongs in `app` or `gitcontext`. |
| 73 | `domain` → `gitcontext` → `gitremote` import chain for one function (`RedactURL`) | **CUT (move)** | A 1,166-line network client imported for one helper. |
| 74 | `internal/core/command.go` importing `pylib` (`command.go:170`) | **CUT** | Dies with candidate 5 or 61, whichever lands first `[v]`. |
| 75 | 176-entry `ExitCodes` catalog → leaf `internal/codes` + a produced/catalogued test | **CUT (move) — sequence with AIRA-45** | Closes **B9**. **AIRA-45** concerns error-code classification granularity in the same area; sequence so the new test does not encode the bug AIRA-45 fixes. |
| 76 | `Check` pre-seeds fourteen dimensions as `pass` | **CUT (fix, ~5 lines) — Tier B** | **AIRA-54** (done) fixed one dimension by hand. Seeding `unevaluated` makes that bug shape unrepresentable for all fourteen. **Raised from Tier C on review: it changes the default of every honesty dimension at once.** Not Tier A, and the reason is recorded rather than assumed: the failure direction of a botched seed change is a **false-`unevaluated`** (loud, and caught by a per-dimension test), not a false-`pass`. Tier B **with a mandatory test per dimension asserting the `pass` path still reaches `pass`.** |
| 77 | Unbounded growth: `registry.jsonl`, `common/aira/locks/`, `pylib/<hash>/` extraction dirs | **CUT (add pruning)** | No ticket; none dangerous; all "noticed in a year". One ticket, one fix. |
| 78 | Dead/unreachable code list (`Store.pathLock`, `sortReports`, `recordScanFinding`, `hasGateContent`, `FlakyCellStateSummary`, `ListComputeSpendByPhase`, `GateAudit.Verify`, `GateAuditRecords`, `copyAsOf`, `PathTier`, `Store.gitDir`, `cappedWhaleOnDisk`, `parseInstalledMemoryHigh`, `inspectExistingPath`, residual whale coexistence checks, `scrubEnv(_, true)`, three no-op branches) | **CUT — with three exceptions** | Traced `[r]`. **Exceptions: `hasGateContent` is named in AIRA-56's analysis as the unsound predicate** — delete it *as part of* AIRA-56's fix so the reasoning is not lost; **`FlakyCellStateSummary`** goes with the flaky classifier (candidate 62) rather than as a stray dead symbol; **`GateAudit.Verify`/`GateAuditRecords`** must be re-checked against candidate 42's durable-head carve-out and AIRA-56's reader before removal — they are the plausible home of the truncation check. |
| 79 | Two byte-size grammars (`app.parseByteCount` vs `runner.ParseMemorySize`) | **CUT** | **AIRA-13 (done)** fixed the flag side and left the config side; `memory_headroom: "1.5G"` is still refused. Unify under candidate 29. |

### 4.3 Unfiled findings — file these before Phase 1

Eight of PR #12's findings have no ticket. Two are operationally serious on a shared box:

| Finding | Suggested severity | Why it matters now |
|---|---|---|
| **B10** — `replaceOlderDaemon` restarts the shared `aira-daemon.service` from any client whose `ProtocolVersion` exceeds the daemon's; a worktree-built binary running `aira list` will do it | **P1** | Same blast-radius class as `CLAUDE.md`'s hard rules on `systemctl --user stop aira.slice`. **Every worktree in this programme builds its own binary. File and mitigate before Phases 2–5 spawn parallel worktrees.** Its second half (`internal/runner`'s hand-copied `runnerDaemonProtocolVersion`, unpinned by any test) interacts with **AIRA-45**. |
| **B12** — routed verbs share one 30-second connection deadline; a slow `import` or `gate attest` commits and then fails the response write → `RequestOutcomeUnknown` | **P2** | Store-ops got a daemon-owned deadline; routed verbs did not. AIRA-18 was the same class. |
| **B5** — detached supervisor is a third production writer | **P2** | Candidate 53; priority raised by **AIRA-22** (§5.1). |
| **B8 / P9** — `check` pre-seeds `pass` | **P2** | Candidate 76. |
| **B9** — catalogued-vs-produced code drift | **P2** | Candidate 75. |
| **B13** — unbounded growth | **P3** | Candidate 77. |
| **B14** — dead code | **P3** | Candidate 78. |
| **B15** — two byte grammars | **P3** | Candidate 79. |

---

## 5. Explicit call-outs — dead-looking today, reactivated by known work

**This section is the owner's specific concern.** Everything here has zero rows, no obvious
caller, or no recent use, and would look like an obvious deletion to anyone reading a row
count. Each has named backlog work or a merged decision that makes it live. **Do not
bulk-delete any of these.**

### 5.1 The detach / supervisor machinery — zero rows, and AIRA-22 makes it the main path

`supervisor_leases`: **0 rows** machine-wide. `aira run --detach` is unused. PR #12 B5 even
says so: "Zero `supervisor_leases` rows suggest detach is unused today, which is the moment to
fix it."

**AIRA-22** (open, P2) is a plan to graft that exact machinery onto `confine`:

> "`aira run --detach` (M20) already solves survival: `LaunchDetached` spawns a `__supervise`
> shim with `SysProcAttr{Setsid:true}`, redirects stdio to durable run-id-keyed files, and
> returns so the shim is reparented to init/subreaper and survives the session. **Graft that
> onto confine.**"

Its motivating incident is real and expensive: *"a ~1hr `make merge-gate` under `aira confine
--delegate-ram` DIED at 98% (~50 min of work lost) when the launching Claude session hit its
usage limit"*. PR #7 P8 independently names AIRA-22 as the forcing function for converging the
two launch paths. `README.md:70` also commits to detach publicly, and design §153/§190 make the
supervisor shim a Phase-5 deliverable with its own D5 spec `[v]`.

**Therefore `LaunchDetached` (`runner/detach_linux.go:20`), the `__supervise` verb
(`main.go:68`), the run-id-keyed capture files, and the `supervisor_leases` table (written at
`store/supervisor_lease.go:173/204/271/344/459`, read at `:108/:407` and `lifecycle.go:244`)
are KEEP** `[v]`. And candidate 53 becomes *more* urgent, not less: folding the supervisor's
writes through the relay is ~50 lines today against an unused path, and a rewrite later
against the main path.

### 5.2 Area hints — zero rows, two production readers, a Phase-1 commitment

`area_hints`: **0 rows** — but **not zero readers** `[v]`: `internal/store/area.go:470`
(`liveAreaClaims`, reached from `Touch` and `areaOverlapWarnings`) and
`internal/store/review_tier.go:389` (`TicketAreaGlobs`). They are wired into `check` (the
`area-overlap` dimension, `check.go:379`) and into `review` (`core.go:841-859`,
`pathsSource = "area-hints"`) — though **not** into `ready`/`claim`, contrary to a natural
assumption.

`CLAUDE.md` names them in the MVP: *"Phase 1 is coordination MVP: … relations, leases, **area
hints**, and the blocked-by ready queue."* Design §110 adds the deliberate scope: *"AIRA itself
is advisory-only — hard area-leases are a decided non-goal."* Neither review proposes deleting
them; this entry exists so a "delete zero-row subsystems" pass does not. **KEEP.**

### 5.3 Requirements + traceability — 7 rows, and the only supported `check-dimension` gate

`requirements`: 7 rows, one import, zero on this repo; `covers:`/`verifies:` currently yield
`U_TRACE_UNSCANNED`. Two reasons to keep, the second stronger than the first:

- `CLAUDE.md`: *"Phase 0 uses `covers:` … and `verifies:` … as a traceability convention. The
  enforcing fail-closed graph check arrives in Phase 3; it must not be invented as a vacuous
  Phase 0 gate."* (Phase 3 is now merged.)
- **The merged Sept-3 gate-honesty plan makes traceability the *only* supported
  `check-dimension` gate** `[v]`: *"`--checker check-dimension` accepts **only** `--dimension
  traceability` … any other value would create a gate that can never evaluate."* Deleting
  traceability strands that checker.

Note the distinction that matters for candidate 48: `trackedTracePaths` (the traceability scan)
is **not** the `requirements` SQLite projection. Candidate 48 deletes the projection; the scan
stays — and **AIRA-72** is about to change its consumer anyway.

### 5.4 Test reports — a near-miss in the other direction, recorded honestly

`test_reports`: **0 rows**. v2 of this document upgraded it to KEEP, arguing that design §120
makes a parsed test report a hard precondition of the `tests-green` gate rule:

> "**`tests-green` from a run's exit code requires exit==0 *and* a parsed §13 report with
> test-count > 0** … the exit code alone would forge exactly the gate this section exists to
> prevent."

**That inference was wrong, and the review caught it** `[v]`. The rule is real, but it is
satisfied by an *inline* parse of the one run, not by the DB table: `classifyTestsGreen`
(`internal/store/gate_command.go:145-159`) calls `gate.ParseGoTestJSONV1` on the command's own
output, and the command-gate design says so in as many words
(`2026-08-11-aira-m10b-command-gates-design.md:244`): *"`tests-green` is evaluated over exactly
one command run. **It does not compare against a baseline or an archive.**"*

So the honest position is a **split**:

- **The `test_reports` table — UNCERTAIN.** Its live producer is `aira run --report`
  (`run_wiring.go:261`), and candidate 45's ratchet baselines would need it. Decide with 45.
- **The cell-level flaky classifier — CUT.** No ticket, no producer, no gate dependency.

One correction to PR #12's own premise survives `[v]`: **aitest does not emit JUnit XML
itself.** It replays pytest `TestReport` objects into pytest's stock
`junitxml`/`terminalreporter` plugins (`supervisor.py:1031/1045`, `worker.py:349`), writes no
XML, and no Go code injects `--junitxml`. So P5's "aitest already emits JUnit; pipe it to
`test-report add --format junit`" overstates it — the wire is `--junitxml <path>` **plus** an
ingest step. Still cheap; plan it as two steps, not one pipe.

**Why this is in §5 rather than buried in the table:** it is the mirror image of the section's
theme. §5 exists to stop dead-looking code being cut when a ticket needs it; this entry is a
case where a *design sentence* made live-looking code out of a genuinely unexercised table.
Both errors are cheap to make from a document read alone, and both are caught the same way —
by opening the code the sentence is supposed to describe.

### 5.5 The gate audit ledger — no reader today, and the durable HEAD is load-bearing

Gate ledger records have no consumer outside `rebuildGateProjection`, which candidate 40
deletes — making the whole ledger look deletable. **AIRA-56** is why it is not: its entire
suggested direction is to read prior `result` / `proof-of-fire` records to distinguish "this
project never used gates" from "this project's gates were deleted", and the merged Sept-3 plan
`:328` deferred to AIRA-56 on exactly that basis `[v]`.

**And one layer inside the "HMAC theatre" is not theatre.** PR #12 P2(b) groups the durable
`HEAD` with the tag, the key file and the nonce scan. The first is different in kind: a hash
chain proves the records present are unmodified and correctly linked, but **cannot** prove
records are not missing from the tail. The durable head is the truncation detector, and the
gate spec says so (`m10a-gates-plan.md:210-217`, enforced at `gate_audit.go:433-445`) `[v]`.

Compose that with AIRA-56 and the stakes are concrete: AIRA-56 asks *"does the ledger hold
prior gate activity?"* A truncated ledger answers **no**, AIRA-56 concludes *"this project
never used gates"*, and `ready` goes green on a project whose gates were silently lost — the
fabricated-green class AIRA-53, AIRA-54 and AIRA-72 were all filed for. **Cut the tag, the key
file and the nonce scan; keep the durable head.** Candidate 42 is Tier A for this reason.

### 5.6 `daemon/governor.go` — no client after Phase 2, two open tickets naming it as substrate

Candidate 4, restated because it is the one place where "delete it, restore from git if needed"
and "an open ticket names it" genuinely collide. **AIRA-17** calls the daemon scheduler its
substrate; **AIRA-64** is filed as input to the next scheduler milestone; `README.md:97`
advertises it. Neither ticket reads the code. Deleting is defensible under
`architectural-simplicity`; keeping 782 unreachable lines is not. **But it must be an owner
decision recorded in both tickets and in the README**, not a line in a deletion PR.

### 5.7 The `--delegate-ram` / `@dr` / `@drc-` marker vocabulary — while AIRA-29 is on hold

Candidates 14–17 and 25 all touch the reserve ledger's identity markers. **AIRA-28's built,
verified, undeployed Approach A is banked at `aira27-delegate-bound @ f356622`** and uses a
versioned `@drc-` scope-id marker; **AIRA-29's twice-reviewed plan is banked at
`aira29-dynamic-reserve`** and is explicitly **ON HOLD by owner decision**. Two banked branches
depend on this vocabulary. Do not churn it as a tidy-up.

### 5.8 Fixture canaries and the ratchet — where this document itself got it wrong

**Recorded as a method warning, not a finding.** v2 of this plan claimed these two were
"deliberately retained one day earlier" by `docs/plans/2026-09-03-aira53-54-gate-honesty-plan.md`
(merged `cf81344`), on the strength of two lines:

> `:334` — "Ratchet gates remain hand-authored."
> `:335` — "Fixture canaries (seed file trees) remain hand-authored; only mutation canaries are
> materializable from flags."

**Read in context, that was an over-read** `[v]`. Both lines sit under a `## Deferrals
(explicit)` heading whose subject is *what the new `gate add`/`set` flags can materialise* —
their neighbours are "No `--name`, `--lane`, `--max-age-secs`, or `applies_to` flag family" and
a note about `E_GATE_*` code registration. They say **"you must still hand-edit the file"**,
not **"this feature must never be removed"**. A scope deferral inside a flag-surface ticket is
not a veto on a later architectural deletion.

The two candidates therefore move to **UNCERTAIN** (candidates 45, 47) as owner forks, not to
KEEP. What genuinely survives is narrower and still worth having: **PR #12's claim that AIRA-60
is "unaffected" by deleting the fixture mode is false** — AIRA-60 is entirely about fixture
seed paths — so whoever cuts the mode must close AIRA-60 as superseded rather than leave it
open pointing at deleted code.

**The method rule this yields, and the reason it is kept in the document rather than quietly
fixed:** when hunting for "would future work reactivate this?", the evidence classes are not
equal.

| Evidence | Strength |
|---|---|
| An open ticket that *asks for* the thing | Strong — KEEP |
| An open ticket that *builds on* the thing | Strong — KEEP |
| A design invariant the code actually implements (verified in source) | Strong — KEEP |
| A design *sentence* not traced to code | **Weak — verify before relying on it** (§5.4 is the case where this failed in the other direction) |
| A merged plan's *deferrals* section | **Weak — it records what one ticket did not do, not what the project intends** |

v2 treated the last two as strong and got two verdicts wrong in one direction and one wrong in
the other. Both errors came from reading documents about code instead of the code. Anyone
executing a phase of this plan should assume the same failure is available to them.

---

## 6. Rigor classification

`CLAUDE.md` mandates the full two-loop for "ID allocation, crash recovery, lease CAS, and other
correctness-critical work", and allows a lighter path for "purely trivial documentation or
mechanical changes". Deleting an unused SQLite table and restructuring CLI dispatch are not the
same risk. **Line count is not the axis.**

### Tier A — full two-loop (plan → external plan-review → Fable gate → TDD build → adversarial build-review → Opus verify)

| Item | Why Tier A |
|---|---|
| **50** — seven counters → one | **ID and sequence allocation**, named verbatim in `CLAUDE.md`. ~60 lines of change, maximum rigor. |
| **48** — delete the three projections, `Rebuild`'s projection half, four `check` dimensions | Touches `Rebuild`, the **crash-recovery** path, and `Check`'s honesty surface. A wrong deletion here fabricates a `pass`. |
| **54 / AIRA-73** — outbox resolution | **Crash recovery**, and the partial unique index it releases can permanently brick a ticket path. |
| **9 + 10** — remove the flock fallback | Changes launch containment under daemon outage. AIRA-58's resolution shows how a "safe-looking" change here turns a refusal into an uncapped launch. |
| **14–17** — cgroup tree as ledger (only if un-held) | **Lease CAS + crash recovery**, both named. AIRA-29's own plan needed three lineages and two gates. |
| **AIRA-62** (24) | Memory accounting in the over-commit direction; the ticket itself demands a standalone adversarial review. |
| **AIRA-71** (68), **AIRA-72** (46/g) | Open **P0**s in the fabricated-green class the project has spent evenings hunting. |
| **AIRA-68** | Open **P0**, root cause unknown. |
| **29–39** — the CLI codegen as a whole | Front door of six faces; a silently-dropped argument is the AIRA-53/AIRA-57 class. M8b's standing Sol P0 applies: *"invokable/installable must be proven by an **E2E test, not parity alone**."* The real-entrypoint tests in `main_test.go`/`skill_test.go` are the regression suite and must stay green throughout. |
| **42** — gate ledger reshape | *(raised from Tier B on review)* It changes the evidence layer that decides whether a gate's green is real, and the durable-head carve-out (§5.5) is exactly the kind of subtlety a lighter pass drops. Fabricated-green class. |
| **53** — fold the supervisor's writes through the relay | *(raised from Tier B on review)* ~50 lines, but they are detached-run **settlement**: the path that decides whether a completed detached run's evidence is durably recorded. Crash-recovery-shaped, and `AIRA-22` is about to make it the main path. |

### Tier B — two-loop, single external reviewer + Opus verify

Candidates **1–8** (the AIRA-33 deletion — large, but of a superseded substrate, with a
cross-project precondition), **40, 41, 43** and **46** (gate shrink; 46 rides AIRA-72's rigor),
**51** (migrations — deleting every upgrade path, gated on the loud `E_SCHEMA_INVALID`
replacement being correct), **55** (primitive dedup — Tier B *because* `stableReadFile` carries
torn-read honesty semantics), **71** (the store→runner seam retype), **21** *(raised from Tier C
on review — it edits the live watchdog kill path, the one component that SIGKILLs real
processes on a shared desktop)*, **76** *(raised from Tier C — it changes the default of all
fourteen honesty dimensions at once; see the candidate row for why it is not Tier A)*, and
**61/62/64/65** once the owner decides.

### Tier C — lighter path (`docs/dev/agentic-development-loop.md`), single worktree, tests still required

Candidates **32–34**, **56**, **56b**, **57–60**, **67**, **70**, **72–75**, **77–79**,
**AIRA-76** (the hook fix), and filing the eight unfiled findings.

---

## 7. Phasing

Each phase names its worktree, its gate, and what it must not start before.

### Phase 0 — Unblock the toolchain, then the mechanical honesty wins

*Rigor: Tier C. One worktree. Hours, not days.*

1. **AIRA-76 first, before anything else.** `.githooks/pre-commit` runs `make fmt-check vet
   build` **unconfined against the shared root checkout**, so every commit in every worktree in
   this programme fails on another session's unformatted file and needs `--no-verify` — which
   is why this plan's own commit uses it. Fix `ROOT_DIR` to `git rev-parse --show-toplevel` of
   the committing worktree and wrap the heavy targets in `aira confine --`, or drop the hooks
   for CI. **Highest leverage-per-line item in the document.**
2. Transition **AIRA-58** and **AIRA-59** to `done` (§2).
3. File the eight unfiled findings (§4.3). **B10 first** — parallel worktrees each build a
   binary that can restart the shared daemon.
4. Candidates **78** (minus its three exceptions), **56**, **56b**, **57**, **58**, **59**,
   **60**, **67**, **77**, **79**. *(Candidate 76 moved to Phase 3b and candidate 21 to Phase 5a
   on review — both were raised to Tier B and no longer belong in a mechanical pass.)*

### Phase 1 — Ticketed correctness fixes that must precede restructuring of their own code

*Rigor: Tier A each. Each is its own PR.*

| Order | Ticket | Must precede | Owner decision needed? |
|---|---|---|---|
| 1 | **AIRA-71** (P0, aitest activation) | Phase 2 (its answer interacts with the plugin deletion) | **Yes** — fix the code or fix the doc; AIRA-77 complicates option 2 |
| 2 | ~~**AIRA-72** (P0, subject digest)~~ — **DISCHARGED**, merged as PR #13 (`723728f`) during this document's authoring | Phase 3a (candidates 42, 46) | No — but see §12: it spawned AIRA-78/79/80/81 |
| 3 | **AIRA-62** (P1, `--memory-max` collapse) | Phase 4 (candidates 24, 31) | No |
| 4 | **AIRA-68** (P0, ledger reserve leak) | Phase 5 (candidates 14–17) | No |

**Parallelism here is conditional, not free** *(review finding)*. AIRA-71's "fix the code"
option edits `internal/runner/confine_linux.go:757-778`, and AIRA-62's own body permits moving
its rule *into the runner* rather than the CLI — so the two can collide in the same file
depending on which approach each takes. **Resolve AIRA-71's owner decision (code vs doc) and
AIRA-62's placement decision (CLI vs runner) BEFORE branching**, then apply §7.1's matrix. If
both land in the runner, serialise them.

### Phase 2 — The authorised deletion (AIRA-33 / PR #7 P1)

*Rigor: Tier B, with external review for the cross-project blast radius.
Worktree: `aira33-xdist-retirement`.*

**Preconditions, all recorded as done before the first deletion:**

- [ ] **Reinterpret AIRA-33's "AIRA's own dogfood suite has run clean on aitest"** — AIRA has
      no Python dogfood suite (§3.1a, verified). Owner picks the substitute criterion (§8 Q8).
- [ ] The **grep sweep** for a non-pytest caller of the CPU park/active-set machinery —
      AIRA-33 and aitest §6: "unconfirmed, **not assumed**".
- [ ] **Owner tick on candidate 4** (`daemon/governor.go`), with **AIRA-17** and **AIRA-64**
      amended to record the deletion and the git sha (§5.6).
- [ ] **AIRA-17** and **AIRA-26** explicitly closed-as-superseded or re-scoped (§3.1).
- [ ] **Cross-project coordination**: confirm no consumer's `conftest.py` still registers
      `aira_xdist_governor` (AIRA-77 names fastest-ee as mid-migration).
- [ ] Record the two **widenings** beyond aitest §3.8's letter (the `confine-reserve` verb, the
      whole `governor.go` file) as decisions (§3.1b).

**Deliverable:** candidates 1–8 (4/6/8 gated on the owner tick), plus the `README.md:97` edit.
**Closes** AIRA-25, AIRA-65, AIRA-77; **re-scopes** AIRA-66 and **AIRA-20**; **resolves**
AIRA-17 and AIRA-26.

*AIRA-20 added on review:* it names `internal/runner/governor_slot_test.go:453
TestGovernorSlotReconnectsWithSameUUID` as one of its three `-race` hardening targets, and
Phase 2 deletes that file. AIRA-20 must be re-scoped, **not** closed — its two other named
targets (`internal/daemon/watch_test.go:105`, `admission_linux_test.go:871`) and its broader
objective (re-enable the `-race` CI job) survive, and §9 risk 6 makes that objective a
precondition for Phases 3 and 5.

### Phase 3 — Store one-truth + gate shrink

*Rigor: Tier A for 48/50/54, Tier B for the rest. Two worktrees, sequenced not parallel.*

**3a — gates** (`aira-gate-shrink`): candidates 40, 41 (with its replacement obligation), 42
(Tier A, with the durable-head carve-out), 43, then 46 after AIRA-72. **Candidates 45 and 47
are struck from this phase** — they are owner forks (§8), not deletions. Do 3a first — candidate
40 removes a `Rebuild` call site 3b also edits. Note the merged gate plan `:446` already records
a merge conflict with the AIRA-55 worktree in these files; check for live siblings before
branching.

**3b — store** (`aira-store-one-truth`): candidates 48, 50, 51, 55, 76, then the ticketed
54 (AIRA-73) and 49 (AIRA-74, after the owner resolves the incremental-vs-rebuild fork).

**Moved out of Phase 3 on review:** candidate **53** (supervisor relay fold) and candidate
**52**/AIRA-75 (watchdog journal) both touch `internal/daemon` and `internal/runner`, which is
Phase 5's territory — leaving them here breaks the Phase 3 ∥ Phase 5 disjointness claim.
**53 → Phase 5a** (it is detach/runner work and Tier A); **52/AIRA-75 → Phase 5a** (it is the
watchdog).

**3a and 3b share `store.go`'s `Rebuild` and `schema_ownership.go`.** Sequence 3a → 3b, or
accept one rebase. With 53 and 52 moved out, Phase 3 (`internal/store`, `internal/gate`) and
Phase 5 (`internal/daemon`, `internal/runner`, `cmd/aira/main.go`) are genuinely disjoint —
**but verify against §7.1's matrix at branch time, not from this sentence.**

### Phase 4 — CLI codegen (PR #12 P1)

*Rigor: Tier A. One worktree. Must follow Phases 1 and 2.*

Blocked on: Phase 2 (deletes `confine-reserve` and `governor-slot`, verbs Phase 4 would
otherwise generate parsers for), **AIRA-62** (candidate 24), and the owner's candidate-61
decision (whether `time` still exists). Carve out `worker-admit` and `aitest-bootstrap` (§3.4)
unless AIRA-42/43/45 have landed. Deliverable: candidates 29–39, 79. The real-entrypoint E2E
tests are the gate.

### Phase 5 — Admission structural

*Rigor: Tier A. Worktree: `aira-admission-structural`. Parallel with Phase 3.*

**5a — remove the flock fallback** (candidates 9, 10). Independent of everything else, the
highest remaining PR #7 value, and it shrinks **AIRA-16**'s scope. Start once Phase 1's
AIRA-68 work has established whether the leak is fallback-related. Also here, moved from
Phase 0/3 on review: candidate **21** (watchdog whale interlock, Tier B), candidate **52** /
**AIRA-75** (watchdog journal), candidate **53** (supervisor relay fold, Tier A), and candidate
**17** (dirname-encoded owner / AIRA-52 — independent and tiny).

**5b — cgroup tree as ledger** (candidates 14–16, 25, 27) — **only when the owner un-holds
AIRA-29**, built as AIRA-29's vehicle with its banked plan as the base, per PR #7 P4.

**5c — layering moves** (candidates 71–75), last, because they touch package boundaries every
other phase is editing inside. Candidate **75** sequences with **AIRA-45** (§4.2).

### Phase 6 — Telemetry decisions (owner)

Not code work until §8's questions are answered. Then: candidate 65 (quota) CUT regardless; 61
and 64 per the decision; 62 keeps and gets its producer wired; 66 follows all of them.

### 7.1 File-touch matrix (build it before branching, not after)

*Added on review — v2 asserted disjointness in prose and was wrong twice.* The claim "these two
phases are parallel" must be checked against this table, and the table must be regenerated once
the owner's §8 decisions fix each ticket's approach (several tickets can land in either of two
files).

| Phase / item | Primary files | Collides with |
|---|---|---|
| Ph0 mechanical | `.githooks/*`, `cmd/aira/tui*.go`, `internal/app`, `.aira/tickets/*` | — |
| Ph1 AIRA-71 | `internal/core/skill.go` **or** `internal/runner/confine_linux.go` | **AIRA-62 (if runner)**, Ph5a |
| Ph1 AIRA-72 | `internal/store/gate_eval.go`, `traceability.go` | Ph3a |
| Ph1 AIRA-62 | `cmd/aira/main.go` **or** `internal/runner/confine_linux.go` | **AIRA-71 (if runner)**, Ph4 |
| Ph1 AIRA-68 | `internal/daemon/admit.go` | Ph5a/5b |
| Ph2 xdist | `internal/pylib/**`, `internal/runner/governor_slot*`, `internal/daemon/governor.go`, `cpuslots.go`, `cmd/aira/main.go` (dispatch), `README.md` | **Ph4** (verb table), Ph5a (`admit.go` hook) |
| Ph3a gates | `internal/gate/**`, `internal/store/gate_*.go` | Ph3b (`Rebuild`), Ph1 AIRA-72 |
| Ph3b store | `internal/store/{store,query,check,search,relation_ready,schema_ownership}.go` | Ph3a (`Rebuild`) |
| Ph4 codegen | `cmd/aira/{main,mcp,dispatcher}.go`, `internal/core/{core,routing}.go` | **Ph2**, Ph1 AIRA-62, AIRA-37/42/43/44/45 |
| Ph5a admission | `internal/runner/{admission_linux,confine_linux}.go`, `internal/daemon/{admit,watchdog}.go`, `internal/store/watch.go`, `cmd/aira/main.go` (supervisor) | Ph1 AIRA-68/71/62, **Ph4** |
| Ph5b ledger | `internal/daemon/admit.go`, `confine_manage*.go` | Ph5a |
| Ph5c layering | package boundaries across `store`/`runner`/`domain`/`core` | **everything** — do last |

### What must NOT run in parallel

- Phase 4 with anything that adds or removes a verb (Phases 2, 3a, 6) — **and note Phase 2 and
  Phase 5a both touch `cmd/aira/main.go` too**.
- Phase 3a with Phase 3b (shared `Rebuild`, shared schema-ownership list).
- Phase 1's AIRA-71 with AIRA-62 **if both choose the runner** (see the matrix).
- Phase 5c with anything.
- Any two items in `cmd/aira/main.go` — the repo's highest-churn file and the meeting point of
  AIRA-37/42/43/44/45/62 and candidates 29–39.

### 7.2 Every CUT candidate is assigned exactly once

*Added on review — v2 left candidates 67 and 70–75 inventoried but unscheduled, and scheduled
candidate 79 twice.*

Ph0: 56, 56b, 57, 58, 59, 60, 67, 77, 78, 79 · Ph2: 1–8 · Ph3a: 40, 41, 42, 43, 46 ·
Ph3b: 48, 50, 51, 55, 76 · Ph4: 29–39 · Ph5a: 9, 10, 17, 21, 52, 53 · Ph5b: 14–16, 25, 27 ·
Ph5c: 70, 71, 72, 73, 74, 75 · Ph6 (after §8): 61, 62, 64, 65, 66 · ticketed, own PRs:
13, 23, 24, 26, 37, 49, 54, 68, 69.
**Struck as already landed or not proposed:** 11, 12, 18, 19, 20. **Owner forks, unscheduled:**
4, 22, 28, 44, 45, 47.

---

## 8. Decisions this plan cannot make

Ten UNCERTAIN candidates plus two process forks. Each is phrased to be answerable without
re-reading the reviews. Questions 10–12 were added after review overturned v2's attempt to
settle them.

1. **`daemon/governor.go` + `cpuslots.go` (782 lines, candidate 4).** After Phase 2 it has no
   client. Delete now and record the git sha in AIRA-17, AIRA-64 and the README, or keep it
   unreachable pending an aitest-era scheduler? *(Recommendation: delete + record.
   `architectural-simplicity` is explicit that keeping machinery for a hypothetical consumer is
   the wrong trade.)*
2. **AIRA-17 and AIRA-26 (§3.1).** Both are xdist-substrate-shaped. Close as superseded, or
   re-scope to aitest? *(Recommendation: AIRA-26 close — aitest forks, so the N×import overshoot
   does not arise. AIRA-17: owner's call; its field data came from an external project still on
   xdist.)*
3. **Command telemetry (candidate 61).** Wire `aira confine` to record one `command_event`, or
   delete `time`/`commands`/`command_events` and ~940 lines? Note `agentic-development-loop.md:44-50`
   currently instructs every agent to use `aira time` while `CLAUDE.md` mandates `aira confine`
   — **one of those two documents is wrong either way.** *(Recommendation: wire it.)*
4. **Compute events (candidate 64).** The capture hook exists and fires on `aira run --tool`;
   zero rows means **`aira run` itself is unused**. So this is really a question about the
   `run` surface: does AIRA-22 / PR #7's P8 convergence make `run` live again, or is `run`
   itself the thing to retire? *(No recommendation — it is upstream of candidate 26.)*
5. **`AppliesTo.*` gate selector fields (candidate 44).** Implement the selector or delete the
   fields? Interacts with AIRA-56.
6. **AIRA-74's fix direction (candidate 49).** Incremental FTS indexing (the ticket's ask) or
   in-memory rebuild-per-query (the review's proposal)?
7. **Watchdog PSI (candidate 22).** Delete the host-PSI decoration outright, or keep the parser
   for AIRA-16(2)'s slice-pressure trigger?
8. **AIRA-33's precondition (§3.1a).** "AIRA's own dogfood suite has run clean on aitest" cannot
   be discharged as written — AIRA has no Python dogfood suite. What is the substitute
   criterion? *(Candidates: aitest's own Python suite green under aitest; or a named consumer
   project has completed its migration.)*
9. **Cross-project coordination for Phase 2 (§3.1).** When does it happen, and who tells the
   consumer session? This is not a code decision and has no owner today.
10. **Ratchet gates (candidate 45) + the `test_reports` table (candidate 62), as one
    decision.** Wire a producer (`--junitxml` from an aitest run, then `test-report add`) and
    keep both, or cut both? The ratchet's only baseline source is that table, so they stand or
    fall together. *(No recommendation — v2's attempt to settle this on design §120 was
    refuted; see §5.4.)* **The flaky classifier is CUT either way.**
11. **The `fixture` canary mode (candidate 47).** Keep it and land **AIRA-60**'s
    declaration-time path validation, or cut it and close AIRA-60 as superseded? Both are
    coherent; PR #12's "cut it, AIRA-60 unaffected" is not (§3.5).
12. **`aira run` itself (upstream of candidates 26, 62, 64).** Q4 above is really this question.
    If `run` converges into `confine` per PR #7 P8 / AIRA-22, its telemetry sinks live; if
    `run` is retired, three candidates resolve at once. **Answer this before Phase 6.**

---

## 9. Risks, and what this plan could be wrong about

1. **PR #7 may be stale in ways this plan has not caught.** It was verified stale for P3, P5 and
   B1 by reading AIRA-58/59's resolutions and re-checking master. Other PR #7 line references are
   against `994abee`, and `9a65d47` touched several of those files. **Every PR #7 candidate must
   be re-confirmed against current master at the start of its phase, not trusted from the
   review.** Candidate 28 is explicitly flagged for this.
2. **The `[r]`-marked facts are the reviews', not re-derived.** Both reviews are careful and
   graded, and no CUT verdict here rests on an `[r]` fact alone — but a review's grep can be
   wrong, and a deletion PR must re-run it.
3. **"No open ticket touches it" is not "nothing touches it".** A consumer project, a shell
   script, or an unfiled intention can depend on a verb. §5.8 shows the same failure in a second
   form: a *merged plan* can retain something no ticket mentions. The Phase 2 cross-project check
   and the merged-plan sweep are mitigations, not proofs.
4. **The dogfood-vs-freeze decisions (§8, Q3–Q4) rest on four weeks of data from one machine.**
   Zero rows in four weeks is weak evidence about a design's future, which is why they are
   questions rather than verdicts.
5. **Phase sequencing assumes Phase 1 actually lands first.** If AIRA-62 slips and Phase 4 starts
   anyway, the codegen re-encodes the bug. §7's dependency table is the guard; it is only as good
   as the discipline applied to it.
6. **This plan proposes deleting roughly 8,000–10,000 production lines across six phases** —
   a large fraction of a codebase whose test suite is its only safety net, and whose `-race` CI
   job is currently **disabled** (**AIRA-20**, open). **Landing AIRA-20's wall-clock-tight test
   hardening and re-enabling `-race` is a strong precondition for Phases 3 and 5.** Flagged here
   rather than as a phase because whether it blocks is the owner's call.
7. **The verdicts are a snapshot, and this document has already been wrong in both directions.**
   v2 wrongly kept two gate features on a misread deferrals section (§5.8) and wrongly kept the
   test-reports table on an untraced design sentence (§5.4). Both were caught only by opening
   the code. **Anyone executing a phase must re-verify its candidates' verdicts in source, not
   inherit them from this table** — and should assume the same failure mode is available to
   them.
8. **Rigor grades moved under review, which means they can move again.** Four items were
   raised a tier after one adversarial pass (21, 42, 53, 76). A single reviewer found four; the
   population of mis-graded items is probably not zero now.

---

## 10. What the two verification passes established

Both passes were read-only against `9a65d47`; no tests were run and nothing was restarted.

**Pass 1 (source facts behind verdicts).** `runnerAdmitWaitCap` no longer exists;
`runner.AdmitWaitCeiling = 24h` at `confine.go:40` with four refusal sites; worker-admit's
30-minute bound is a refusal at `worker_admit.go:355-358` against `admit.go:37`, guarded by
`admit_freeze_test.go:449-453` pinning the exact value. aitest writes **no** TestReports to the
store — every `AddTestReport` production call site is `run_wiring.go`, `core.go:1200`,
`storeops.go:228` or the relay shim; aitest's "TestReport" is pytest's own object. AIRA's suite
is pure Go (`Makefile`, CI, no repo-root pytest config). The five gate projection tables have no
non-test reader; `gates` has exactly one (`rant.go:510`). The detach machinery and
`supervisor_leases` exist and are wired as described. `aira time` is the sole command-event
producer (`command.go:342`). Area hints have two production readers (`area.go:470`,
`review_tier.go:389`) but are not wired into `ready`/`claim`. Six dispatch stubs; eleven
out-of-table verbs; `install` is in both lists. The flock fallback survives with its pinned
branch narrowed (`confine_linux.go:664` vs `:676`). The whale interlock and PSI helpers are
present. `internal/query/` and `internal/interp/` do not exist. Line counts confirmed
(`store` 17,662; `runner` 11,588; `cmd/aira` 7,807; `daemon` 7,207; `core` 4,534; `gate` 1,021).

**Pass 2 (design-document commitments).** The design spec is self-declared "Built. Phases 0–5
all implemented and merged" — so §9/§12/§13/§16/§17 describe built subsystems rather than
promising future ones, which *narrows* the "a design doc commits to this" defence. The genuine
forward commitments are: the merged gate-honesty plan (retaining hand-authored ratchet and
fixture canaries, deferring `ready`'s fix to AIRA-56); aitest §3.8/§5/§6 (the governor deletion
and its two preconditions); `agentic-development-loop.md:44-50` (still instructing `aira time`);
`CLAUDE.md`'s Phase-3 traceability clause; and `README.md:144` ("advisory for now"). Quota is
"opt-in" verbatim. `find add`/`aira review` appear in no loop document. `exec`/`aira_exec`
remains spec-reserved for `internal/interp`. `README.md:97` advertises the daemon scheduler.

---

## 11. Changelog

**v1 — initial draft.** Inventory of 79 candidates; 42 open tickets read in full and
cross-referenced; a fourth verdict class (ALREADY LANDED) added on discovering that PR #7's base
commit predates `9a65d47`.

**v2 — after two first-hand verification passes against master.** Five material corrections,
two of which reverse a verdict:

- **REVERSED — candidate 47 (`fixture` canary mode): CUT → KEEP / DEFER.** The gate-honesty plan
  merged as `cf81344` on 2026-09-03 states "Fixture canaries (seed file trees) remain
  hand-authored". PR #12's claim that AIRA-60 is "unaffected" is also wrong — the fixture seed is
  AIRA-60's entire subject. (§3.5, §5.8)
- **REVERSED — candidate 45 (ratchet kind): UNCERTAIN → KEEP / DEFER.** Same merged plan:
  "Ratchet gates remain hand-authored." It refused to build a flag surface while deliberately
  keeping the checker. (§3.5, §5.8)
- **UPGRADED — candidate 62 (test reports): UNCERTAIN → KEEP.** Design §120 makes a parsed test
  report a hard precondition of the `tests-green` gate rule PR #12 keeps, and §119 pins ratchet
  baselines to them. Also corrected the review's "aitest already emits JUnit" — it replays into
  pytest's stock plugin and writes no XML. (§5.4)
- **NARROWED — candidate 41 (`gates` table): CUT → CUT with a named replacement obligation.**
  The same merged plan *withdrew* its deferral of the projection rebuild specifically because
  `rant --gate` reads that table, and shipped a three-state `IndexStatus` the replacement must
  preserve. (§3.5)
- **CORRECTED — candidate 13's premise.** PR #7 P3 describes a silent clamp at
  `worker_admit.go:349-351`; the code is a refusal at `:355-358` against `admit.go:37`. The
  verdict (KEEP / DEFER on AIRA-63) is unchanged but now rests on verified lines and a guard test
  that pins the value exactly. (§3.2)

Also in v2: candidate 63's reasoning corrected (findings are zero-row because the loop docs never
route agents to `find add` — verified — so the fix is a documentation change); §5.2 corrected
(area hints have two production readers, and are wired into `check`/`review` but *not*
`ready`/`claim`); §5.3 strengthened (traceability is the only supported `check-dimension` gate);
§3.1 gained three preconditions (AIRA-33's blocker is unachievable as written; the aitest spec
authorises less than PR #7 deletes; `README.md:97` must be edited); candidate 56b split out;
candidate 78 gained a third exception; §8 grew from seven questions to nine; §9 gained risk 7.

**v3 — after an adversarial plan-review (Codex/Sol) returned BLOCK with eight findings.** Every
finding was re-verified in source before acceptance; none was taken on the reviewer's word.
Seven accepted in full, one accepted in part.

- **[P0] ACCEPTED — candidate 42 would have deleted the ledger's only truncation detector.**
  PR #12 groups the durable `HEAD` with the HMAC layers; a hash chain cannot detect tail
  truncation, and `m10a-gates-plan.md:210-217` says the durable head is what does. Composed with
  **AIRA-56** — which asks "does the ledger hold prior gate activity?" — a truncated ledger
  reads as "never used gates", a fabricated green. **Durable head carved out of the cut;
  candidate 42 raised to Tier A.** (§3.5, §5.5)
- **[P0] ACCEPTED — v2's KEEP for test reports rested on an untraced design sentence.** Design
  §120's rule is satisfied by an inline parse (`gate_command.go:145-159` →
  `gate.ParseGoTestJSONV1`), and the command-gate design states outright that `tests-green`
  *"does not compare against a baseline or an archive"* (`m10b:244`). **Candidate 62 split:
  table → UNCERTAIN (decide with 45), flaky classifier → CUT.** (§5.4)
- **[P1] ACCEPTED — v2's flagship "merged plan retains ratchet and fixture canaries" was an
  over-read.** Both quoted lines sit under `## Deferrals (explicit)`, a section about what the
  new `gate add` **flags** can materialise, not about what may be deleted. **Candidates 45 and
  47 → UNCERTAIN owner forks.** §5.8 rewritten as a method warning with an evidence-strength
  table, and kept in the document rather than quietly fixed. The one durable piece: PR #12's
  claim that AIRA-60 is "unaffected" by cutting the fixture mode is still false.
- **[P1] ACCEPTED — candidate 56b flipped to CUT.** The live CLI calls `runMCPWithDispatcher`
  and `runSkill`; `runMCP` is an unused wrapper and `runSkill` calls `GenerateSkillArtifacts`,
  not `GenerateSkill`. AIRA-71 edits generated content, not these wrappers.
- **[P1] ACCEPTED — the phasing was not merge-safe.** Phase 1's disjointness is conditional on
  AIRA-71's and AIRA-62's approach decisions; Phase 3 held daemon/runner work belonging to
  Phase 5; candidate 79 was scheduled twice; candidates 67 and 70–75 were never scheduled.
  **Added §7.1 (file-touch matrix) and §7.2 (every CUT assigned exactly once); moved 21, 52,
  53 to Phase 5a and 76 to Phase 3b; added Phase 5c for the layering moves.**
- **[P1] ACCEPTED — candidate 64's rationale was factually wrong.** `wireRunCompute` is live
  (`run_wiring.go:120-139`); zero rows means `aira run --tool` is unused, not that the hook is
  missing. Rewritten and folded into new §8 Q12 (the `run` surface itself).
- **[P1] ACCEPTED IN PART — rigor grades.** Raised 42 and 53 to Tier A, 21 to Tier B.
  **Declined Tier A for candidate 76** (`check` seeding), with the reason recorded rather than
  asserted: a botched seed change fails in the **false-`unevaluated`** direction, which is loud
  and testable, not the false-`pass` direction Tier A exists for. **Tier B with a mandatory
  per-dimension test.**
- **[P2] ACCEPTED — AIRA-20 missing from Phase 2's ticket-resolution list.** It names
  `governor_slot_test.go:453` as a `-race` hardening target and Phase 2 deletes that file.
  **Re-scoped, not closed** — its two other targets and its CI objective survive.

Counts after v3: 80 candidates — **48 CUT, 17 KEEP / DEFER, 10 UNCERTAIN, 4 ALREADY LANDED,
1 not proposed.** §8 grew from nine questions to twelve; §9 gained risk 8.

---

## 12. Addendum — backlog drift during authoring (2026-09-04, same night)

**§9 risk 7 says the verdicts are a snapshot. That risk fired before this document was
committed**, so the drift is recorded here rather than folded silently into the tables above.

While this plan was being written, another session landed **AIRA-72** (merged as PR #13,
`723728f`) and filed four new gate tickets. The backlog snapshot in §2 is `AIRA-1..77`;
these are `AIRA-78..81`, all `planned`, all in `internal/store/gate_*`.

| Ticket | Sev | Subject | Effect on this plan |
|---|---|---|---|
| **AIRA-78** | **P0** | `evaluateRatchet` binds its verdict to a working-tree subject digest but selects test reports by `git HEAD`; on a dirty tree it **mints a fresh pass from evidence that does not describe the subject** | **Changes candidate 45 from UNCERTAIN to KEEP / DEFER.** An open P0 with a fix direction and "needs its own adversarial loop" is an open ticket depending on the ratchet. Also raises §8 Q10's stakes. |
| **AIRA-79** | P2 | Subject digest fails closed on a tracked submodule — an accepted, pinned false-fail regression from AIRA-72 | Phase 3a must not "fix" `U_GATE_EVIDENCE_UNAVAILABLE` by narrowing the digest back; that is the AIRA-72 bug. |
| **AIRA-80** | P1 | `EvaluateDimension` digests one read of the tree and evaluates another | Touches `gate_eval.go`, which candidates 42/46 also touch. Sequence. |
| **AIRA-81** | P2 | Canary re-materialization drops tracked-but-ignored files, so a canary can **fire for the wrong reason and still mint proof-of-fire** | Concerns the *mutation* canary — the part PR #12 §3.4 keeps as the proven core — and is adjacent to candidate 47's fork. |

**Four consequences, in priority order:**

1. **Phase 1's AIRA-72 item is discharged.** The dependency "AIRA-72 before candidates 42/46"
   is satisfied. Phase 1 is now three tickets, not four.
2. **Candidate 45 (ratchet) moves to KEEP / DEFER**, and §8 Q10 must be re-read with AIRA-78 in
   hand: the question is no longer "does anyone want the ratchet?" but "fix the open P0 in it,
   or delete the feature and close the P0 as superseded?" That is still an owner fork, but it is
   a different one, and deleting now would close a P0 by deletion — which this project's honesty
   rules make an explicit decision, never a side effect.
3. **Phase 3a is no longer the quiet corner it looked like.** Four gate tickets (78, 79, 80, 81)
   now live in `internal/store/gate_eval.go`, `gate_ratchet.go`, `gate_subject.go` and the canary
   path — the same files candidates 40–43, 46 and 47 edit. **Add the AIRA-72 follow-up wave to
   §7.1's matrix and re-check Phase 3a's collision set before branching.** Phase 3a probably
   needs to follow that wave rather than race it.
4. **The gate subsystem is the most active area in the repo tonight, not a dead one.** PR #12's
   framing — 4,069 lines "for a feature with zero definitions on any project" — was accurate as
   a row count and is now misleading as a description of intent: AIRA-53, 54, 55, 56, 60, 72, 78,
   79, 80 and 81 are all gate work, and four landed or were filed in the last day. **This does
   not overturn P2's deletions** (the write-only projection, the HMAC tag, the six unread
   definition fields have no ticket and remain CUT) **but it does mean Phase 3a is contended
   work, and its schedule should be agreed with whoever is running the gate-honesty wave.**

**Method note.** This addendum exists because the drift was detected by re-listing
`.aira/tickets/` after the plan was written, not by anything in the plan's own process. **Any
phase of this programme should re-run that listing at branch time**; the §2 snapshot has a
shelf life measured in hours on this repo.
