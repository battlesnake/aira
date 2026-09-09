# Proposal: the `aira cloud` command family

- **Status: PROPOSED — NOT accepted, NOT built, NOT active.** Nothing in this document is
  implemented; no `aira cloud` verb exists in the binary; no ticket has been accepted for it.
  This is a design put to the owner for a decision (see [Open questions](#open-questions-the-owner-must-settle)).
  It records interview evidence and an adversarial review so the decision can be made on more than
  one session's opinion. **Do not cite this as an as-built or accepted design.** If the design here
  is ever accepted, the accepted portion moves to a dated spec under
  [`docs/superpowers/specs/`](../superpowers/specs/) and this file is superseded.
- **Date:** 2026-09-09.
- **Naming:** the working name during discovery was "cloud offload". That name is **dropped
  deliberately**: this is *not* a transparent `run`/`confine` substitution that silently sends a
  local job to the cloud when the box is contended. It is a **separate, first-class command
  family** — `aira cloud …` — for launching and monitoring cloud jobs *with safeguards*, orthogonal
  to `aira run` and `aira confine`. Wherever the word "offload" survives in the scratch record, read
  "the `aira cloud` command family".
- **Provenance:** driven by interviews with three sibling sessions/projects (`spice`/flavour-kernel,
  `field`/stoner, `deploy`/fastest-ee), a read-only survey of the AIRA source and this machine's
  live GCP state, an independent architecture review (DeepSeek-v4-pro), and an adversarial
  build-review of the flag surface. The raw discovery material is working scratch and is not part of
  the repository record.

---

## 1. The problem, and the finding that reframes it

Three sibling projects on this machine each want to run heavy jobs on GCP — each in a **different
repo** and a **different GCP project**. Their real, measured workloads:

| | **spice** / flavour-kernel | **field** / stoner | **deploy** / fastest-ee |
|---|---|---|---|
| Headline job | `cargo test --workspace`, ~3 min, dozens/session | cross-ISA bitwise check, minutes, on demand | `make merge-gate`, 35–40 min, several/day |
| GCP project | `flavour-bench-fee`, us-central1-a | `stoner-bench-fee`, us-central1 | `fastest-ee`, europe-west1 |
| Backend | GCE VM + IAP tunnel + ssh | GCE VM (committed gcloud scripts) | **GCP Batch**, arm64 n4a-standard-16 |
| Payload | **dirty working tree** (rsync) | **a prebuilt aarch64 binary** | **git bundle** + a 19 GB disk-snapshot mount |
| Container image | none | none | Artifact Registry, repo-specific |

**The reframe.** AIRA already does cloud — the *receiving* half is shipped and merged. `ci-shim`
mode (AIRA-121/123/129) exists by name for GCP Batch / AWS Batch/Fargate / Cloud Build / k8s Jobs;
`aira confine -- make merge-gate` runs verbatim inside a Batch container today; AIRA-122 ships the
arm64 release asset; `aira git clone|fetch|push|ls-remote` is an existing bounded network verb. The
word "offload" appears nowhere in the repo. So the question is not *"should AIRA touch the cloud"* —
it already does — but *which half of the client side to build, and how much*.

---

## 2. What the three projects converged on — and what they did not

**Converged (the guarantees):**

1. **A cloud-enforced, absolute deadline is THE guarantee.** field: *"aira confine's value isn't
   that it runs things, it's that a runaway dies in-slice instead of taking the desktop with it. The
   cloud equivalent of that guarantee is the DEADLINE."* Their single dealbreaker is *"if it can
   leak a VM."* Both GCE users independently learned that `--max-run-duration` RESETS on stop/start,
   so only an absolute `--termination-time` + `--instance-termination-action=DELETE` is safe.
2. **Per-repo GCP identity, pinned loudly.** Three repos, three projects, separated on the owner's
   explicit instruction.
3. **No service-account keys, ever** — verified: no SA key files exist under `~/.config`; only 12h
   impersonated tokens.
4. **AIRA must not own the infrastructure** — IAM, buckets, service accounts, terraform, the runner
   image, the machine type/zone, benchmark methodology, or the *meaning* of the result. All three,
   unprompted.
5. **Credential/stream handling in shell is a proven, repeating footgun** — three projects hit three
   distinct token-into-a-text-stream / verify-one-authenticate-as-another bugs. A compiled binary
   that owns the credential step once, and never puts a token on a stream, cannot have any of them.

**Diverged (the mechanics):** unit of work (spice needs a warm *session*; deploy a detached *job*;
field ships an *artefact*), payload (dirty tree vs git bundle + 19 GB snapshot vs prebuilt binary),
and backend (GCE+ssh vs Batch). **The convergence is entirely in the guarantees; the divergence is
entirely in the mechanics.** So a single `aira offload -- <argv>` verb cannot serve all three, and
AIRA should own the guarantees and touch none of the mechanics.

---

## 3. The recommended shape: a ledger + invariants, not an executor

The design that **emerged from** the adversarial review is **not** a cloud executor — note it was
produced in *response* to that review (which broke an earlier bracket-executor design) and has not
itself been sent back to the peers for attack. It is a small command family that *brackets* each repo's own committed launch script — the way `aira confine` brackets a
local job — owning only what is general across all three:

```
aira cloud register --target T (--expect D | --deadline D) [--name N] [--ticket ID] [--json]
    → prints a handle AND the absolute UTC termination instant to use.
      REFUSES if the resolved bound exceeds the effective ceiling (§4), BEFORE the resource exists.
aira cloud confirm  <handle> --resource ID
aira cloud release  <handle> --outcome pass|fail|unevaluated|preempted|deadline_exceeded [--receipt PATH]
aira cloud list     [--target T] [--all] [--json]     # project-less, cross-session
aira cloud audit    [--target T] [--json]             # per-target verdict; unlabelled-first; worst-state-wins
```

The repo's own script changes by ~three lines: register before it creates a resource, pass AIRA's
absolute instant to `--termination-time`, confirm the resource id, release at the end. **The script
keeps its own EXIT trap, its own credentials, its own argv, its own teardown** — every "AIRA must
not own this" is satisfied by construction rather than by discipline.

`aira cloud run --target T --expect D [--receipt P] -- <argv...>` is defined in this proposal as
**sugar** over the primitive (it would be built, not that it exists today) (register → exec with SIGTERM-and-grace, never `confine`'s hard kill → release), for the
simple synchronous case. Because it is sugar over verbs a script can call directly, its failure
modes degrade to "do it yourself" rather than being load-bearing.

**Why not a full executor** (`aira cloud` drives GCP via the API itself): it would need two backends
(GCE, Batch) with no way to infer which, three payload transports, and a disk-snapshot mount model;
it would make AIRA a provisioner, which is the CI-system non-goal in substance; and it loses the
property every peer valued — that the thing which *creates* cloud resources stays a reviewed,
committed, per-repo script. This is a genuine fork for the owner (§ Open questions).

### The safeguards AIRA owns

- **The deadline ledger.** A cloud resource cannot be registered without an absolute termination
  instant; `aira cloud list` is the `confine --list` analogue — a **machine-wide, cross-session**
  view of what every session is paying for right now, which no single repo can build for itself.
  (This cross-session view was the win `spice` rated above everything.)
- **The credential invariant.** Resolve the per-repo identity, verify it, print it and its real
  `expires_in` as structured data, hand the token only as a **file path — never on a stream**,
  refuse if `CLOUDSDK_AUTH_ACCESS_TOKEN` is set (gcloud prefers that value over an
  `access_token_file` path — verify-one/authenticate-as-another), and ship a `--dry-run` that prints
  the exact provider invocation and touches nothing.
- **The work receipt.** The job WRITES an opaque token + a declared KIND; AIRA records it verbatim,
  never interprets it, and renders no `pass`/`fail` without one — reporting `unevaluated` instead.
  Four states: present-nonzero / present-EMPTY / present-but-UNREADABLE / ABSENT (empty and absent
  both read `unevaluated`; unreadable is an infrastructure fault with its own code). It is an
  **honesty aid, not an integrity mechanism** (`echo 1 > receipt` is undetectable) and must be
  documented as such.
- **The provider audit.** Its PRIMARY output is resources carrying *no* AIRA label (the leaks that
  never registered), not ledger diffs — a ledger-vs-label reconciliation only finds what already
  agreed to be found. Worst-state-wins **per target**: any `unevaluated` resource ⇒ that target's
  verdict is `unevaluated`, never `clean`; and if the audit itself fails (e.g. an expired
  credential) it reports `unevaluated`, never `clean`.

### Honest status against the three projects (the design is NOT three-way endorsed)

The convergence in §2 is on the *guarantees*, established in the initial interviews. The recommended
*shape* in §3 is the designer's synthesis and has **not** been endorsed by all three:

- **field / stoner** — the closest to a yes. Said *"I'd move both my GCP scripts under it"*,
  conditional on three fixes (worst-state-wins audit, the absurd-deadline slack, and the receipt
  primitive), which are reflected above. Their dealbreaker (a leaked VM) is *surfaced* by the ledger
  and bounded by the deadline; *fully* closing it still needs the audit (§6.3).
- **spice / flavour-kernel** — one win delivered (the machine-wide cross-session view, which they
  rated above everything), and their actual headline problem — the warm session-scoped workspace to
  kill per-job cold start — explicitly **not** addressed here (deferred, below).
- **deploy / fastest-ee** — **95% of the stated value, and the design is NOT validated by them.**
  They supplied the initial interview and later independently verified the v0.4 arm64 asset and that
  AIRA-220's `granted_established` is a no-op for their gate — but they **never replied to the
  adversarial round and never endorsed the register/confirm/release shape**. Everything about their
  workload here is the designer's reading of their committed scripts, not their agreement. Their
  Batch estate is applied and exercised but **not yet trusted** (recorded as 35 jobs, zero ever
  green at survey time), and their repo has no `.aira/` directory today.

So a reader must not take §2's convergence as agreement on §3's shape — especially not from the
project that is most of the value.

---

## 4. The deadline ceiling — a transplant, not an invention

The owner's ask was "a sane default termination deadline, overridable per-project and per-call, with
a ceiling." AIRA already ships exactly this three-layer shape for admission waits, verified in the
tree: `runner.DefaultConfineAdmissionWait = 30m` (default), `--admit-timeout` (override),
`runner.AdmitWaitCeiling = 24h` (ceiling), refused with a dedicated `E_ADMIT_WAIT_TOO_LONG` that the
daemon re-derives rather than trusting from the CLI. So this is `--admit-timeout`'s discipline
applied to a different clock.

```
L0 built-in     default 1h ; ceiling 24h (unraisable — aligned with runner.AdmitWaitCeiling so ONE 24h number exists)
L1 machine      aira install --cloud-deadline-max 8h    (idiom precedent: install already carries --memory-max / --slice-ceiling)
L2 project      .aira/config: cloud.deadline_default / cloud.deadline_max
L3 call         --deadline / --expect

Rule: a CEILING may only narrow inward; a DEFAULT may move either way within its layer's ceiling.
effective_max = min(L0_ceiling, L1, L2_max, session_remaining)
--deadline > effective_max  →  REFUSE (name the binding layer), never silently clamp.
```

`--expect <duration>` derives the deadline from a *declared expectation* (bound = expect × margin),
so when it is used, an absurd default is not merely visible but not the default. **Honest limit:**
`--expect` is OPTIONAL, and the raw `--deadline` path still accepts any value up to the 24h ceiling
regardless of the job's real length (`--deadline 24h` on a 3-minute job passes every check and leaks
for a day). Recording deadline-minus-now slack surfaces that, but it is a mitigation, not a cure.
**Open sub-point on the margin:**
AIRA's existing history-derived margin is ×1.15 (a 15% bump tuned for peak-RSS over ≥3 samples); that
is far too tight for a human-declared wall-clock expectation with a long tail, so the cloud margin
must be its own constant with its own justification, calibrated from observed cloud runtimes once any
exist — **not** the memory estimator's 1.15 borrowed.

24h as the unraisable maximum is double-sourced (it reuses the existing `AdmitWaitCeiling` so AIRA
keeps ONE 24h number rather than two that drift; and it matches `spice`'s own in-production
`readonly MAX_TTL_HOURS=24`, whose comment reads *"A typo in --ttl must not be able to leave a box
running for a week."*).

---

## 5. Per-repo config seam

`deploy`'s six live keys generalise to exactly six concepts: **project, region, artefact-store,
identity, image, and zero-or-more read-only data mounts** (the mounts are the one a naive design
omits and deploy could not work without). All values are resource **names**; **no credentials in it,
ever** — *"a config file that could carry a credential eventually will."* Note `.aira/config` is
strictly-typed with `DisallowUnknownFields` and a pinned schema (`internal/app/project.go`), so a
per-repo cloud descriptor needs a typed Go struct field, not a config convention; and it could be
generated from `terraform output` rather than hand-maintained. **`aira cloud` must be project-less**
(a hand-added `RouteClient` case), because fastest-ee — 95% of the value — has no `.aira/` directory
and its descriptor lives deliberately outside the repo.

---

## 6. Honest weaknesses (from the adversarial review — `spice` and `field` responded and confirmed these; `deploy` did not reply)

1. The ledger deadline is what AIRA was *told*, not what reached the provider (deploy's real deadline
   lives inside generated Batch JSON AIRA never sees); the audit's "live past ledger deadline" check
   is what catches divergence, not registration alone.
2. For GCP Batch the absolute-deadline invariant degrades to a relative `maxRunDuration`, so a long
   queue delay can outlive the ledger instant. Not fixable from AIRA's side.
3. **The audit has no durable credential.** Verified: no SA key files under `~/.config`, ADC is an
   interactive refresh token. On day one the audit reads `unevaluated`, and under worst-state-wins
   that is the top-line verdict — *"a signal that is always yellow is a signal nobody reads."*
4. The receipt is forgeable — an honesty aid, not a guarantee.
5. Bypassable in one line: a script calling gcloud with the human ADC directly gets none of this.
6. Half-adoption is worse than none: a script that registers but never writes a receipt produces
   rows that read `unevaluated` forever.

---

## 7. Scope test against AIRA's own doctrine

- **The spec permits it:** AIRA is declared the authority for *"leases … compute, timing, test
  outcomes, and runs — across worktrees and (later) machines; a recorder and orchestrator, not a
  judge."* A cloud **lease**, a verified **identity**, and a recorded **outcome** are in scope by
  name. A cloud **executor** that provisions VMs and runs the merge-gate is the CI-system non-goal
  the spec says AIRA is *not*.
- **The README forbids it, absolutely:** README.md:13/:87/:132 promise *"No cgo, no network, nothing
  in the cloud … None of this leaves the machine … machine-local and single-user by design: no
  server, no network."* Nothing adjudicates the tension — but README:87's own escape hatch is the
  exact shape everything here fits: *"the only remotes AIRA ever reaches are the git remotes you
  explicitly hand it"* → explicit user-supplied target, no discovery, no phone-home. **This
  contradiction is an owner decision and blocks even the thin version.**
- Cloud work appears in **zero** of the (then) 219 tickets and 36 rants; every remedy anyone has
  asked for is local. This demand surfaced only through interviews.

---

## Open questions the owner must settle

1. **The README-vs-spec contradiction (§7).** Even the thin ledger needs the README's absolute
   "nothing in the cloud" reconciled with the spec's "(later) machines" and README:87's own
   "only what you explicitly hand it".
2. **The fork (§3):** ledger + invariants only (recommended), ledger now + one opinionated
   GCE-session executor later, or reject. The recommendation is ledger-first.
3. **The durable non-interactive credential (§6.3)** — none exists on this machine, and the audit
   (the part that makes the ledger honest rather than confidently-wrong) cannot run without one.
   Solve it, or accept a ledger-only version whose audit reads `unevaluated`.
4. **Is a cheaper local fix the better first move?** The incident that first made `spice` ask for
   cloud was an AIRA admission bug — a 14×-over `--memory-reserve` caused a 30-minute
   `E_ADMIT_SATURATED` wait and a spawned-then-deleted GCE box for a job whose real peak RSS was
   294 MB (filed as RANT-38). Reserve-sizing help attacks the same pain, costs less, and needs no
   cloud posture change. It is also a standing warning never to wire this to an admission-pressure
   trigger.

---

## Deferrals (explicitly not in this proposal)

- The warm session-scoped GCE workspace `spice` most wants (their per-job cold-start dealbreaker) —
  a separate decision on its own merits, *after* any ledger exists.
- Any contention/admission trigger — the owner ruled offload is not a transparent fallback, and
  RANT-38 is independent evidence against it.
- Fan-out / multi-worker sweeps (field's tracked "Distributed" item), retry, and cost derivation
  (an open question in the top-level spec's §21) — all out of scope here.
