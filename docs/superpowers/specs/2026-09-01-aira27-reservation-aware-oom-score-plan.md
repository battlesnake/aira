# AIRA-27 fix (b): reservation-aware oom_score — protect airtight jobs from delegate-ram over-commit collateral

**Status:** plan (pre-review)
**Ticket:** AIRA-27 (P1)
**Branch:** `aira27-oom-score`
**Author:** Opus, grounded on the AIRA-27 understand pass (3 readers) + the Q1–Q3 design dialogue.

## 1. Problem

When `aira.slice` over-commits and hits its `memory.max`, the slice has **no
`memory.oom.group`**, so the kernel picks a per-task victim by ~RSS badness — and every
confined process carries a **flat `oom_score_adj=500`** (`confine_linux.go:34`, written once
at scope start, inherited by descendants). So the victim is whichever confined task the
kernel lands on, **not** the one that caused the over-commit. Well-behaved neighbours die:
subpipe's airtight non-delegate merge-gate (5×), money's 10 MB non-delegate waiter.

**Who causes the over-commit (from the understand pass, code-verified):** NON-delegate
scopes are **airtight** — the CLI up-charges the reserve to `--memory-max` and the core sets
`memory.max = reserve` (`main.go:796-798`, `confine_linux.go:458-461`), so their RSS can
never exceed their reserve. **DELEGATE-RAM** scopes are the over-committers — `memory.max`
is the 48 GiB scope-ceiling (NOT admission-bounded), and the per-test gate **fails open**
under contention (runs ungoverned), so `Σ(delegate RSS)` is unbounded by admission.

So the goal: **make the kernel's OOM victim selection prefer the over-committing
delegate-ram scopes over the airtight non-delegate jobs and light waiters**, without
touching admission or memory limits (this is victim-ORDER only), and without eroding the
desktop protection the flat 500 provides (confined ≥ 500 > host 0).

## 2. Design — two options; recommend Option A (static, simple), defer Option B (dynamic)

### Option A (RECOMMENDED) — static class-based `oom_score_adj`

Set the scope's `oom_score_adj` by **class** at scope start (no daemon machinery):
- **Non-delegate (airtight)** scopes keep `500` — provably can't over-commit; the class we
  most want to PROTECT.
- **Delegate-ram** scopes get a **higher** value (proposed `800`, `confineDelegateOOMScoreAdj`)
  — the only class that can over-commit (unbounded ceiling + fail-open gate).

Under a slice-level (or global) OOM the kernel scores each **task** `badness ≈ RSS +
adj·total/1000` and kills the max; because every scope carries `memory.oom.group=1` (slice
= 0), killing that one task promotes to a **whole-scope** kill (`mem_cgroup_get_oom_group`,
`confine_linux.go:579-581`). So raising delegate-ram to a higher adj is a **strong BIAS**
toward killing a delegate scope — **not an absolute guarantee**: at this slice a 300-pt delta
≈ ~19 GiB RSS-equivalent, decisive over LIGHT/small airtight jobs and over host (all confined
stay `≥ 500 > 0`, desktop protection preserved), and the scope-group-kill means picking ANY
delegate worker releases the whole over-committing scope at once. **Honest limitation:**
selection is per-PROCESS, so a delegate suite's aggregate is spread thin across many small
workers, and a LARGE single airtight process (≳ the delta-equivalent GiB fatter than every
delegate task) can still out-score the bias and be picked. So Option A reliably protects
**light** airtight jobs (money's 10 MB waiter) and biases hard toward delegate victims, but
does NOT robustly protect a **large** airtight job (subpipe's 32 GiB gate) — that needs the
**structural fix** (bound the delegate aggregate so no slice-OOM fires at all; next milestone,
per the owner's decision). Env overrides (`AIRA_CONFINE_OOM_SCORE_ADJ` / `..._DELEGATE`) are
**validated + clamped** so `1000 ≥ delegate > non-delegate ≥ 500` (a value < 500 would erode
the desktop floor; an out-of-range value is rejected at argv-build, not silently written).

- **Rationale for the class proxy:** we can only *guarantee* airtightness for non-delegate
  (RSS ≤ reserve by kernel cap). A delegate scope, even a "well-behaved" one, *can*
  over-commit (fail-open), so as a class it is the correct sacrifice under a slice OOM that
  delegate over-commit caused. This is a strict improvement over the flat 500 (which
  sacrifices airtight jobs equally), and it is what the reported incidents needed
  (subpipe/money's victims were both non-delegate → protected; the over-committers were
  delegate → preferred).
- **Where:** `confine_linux.go` — replace the flat `confineOOMScoreAdj=500` at the write
  site (`:1257`, argv at `:1139`, setup helper) with a class-dependent value derived from
  `request.DelegateRAM`. Both values are compile-time consts, overridable via env for
  tuning (`AIRA_CONFINE_OOM_SCORE_ADJ` / `..._DELEGATE`).
- **Cost:** none beyond a const + a branch. No daemon loop, no races, no `/proc` writes, no
  `CAP_SYS_RESOURCE`, no anonymous-lease attribution problem.
- **Limitation (documented):** does NOT differentiate a compliant delegate suite from an
  over-committing one — both are `700`; RSS badness is the only within-class discriminator.
  On an all-delegate box a compliant-but-large suite could be sacrificed for another's
  over-commit. Acceptable first cut; Option B is the precise follow-up.

### Option B (DEFERRED) — dynamic reservation-compliance re-writer

A daemon periodic loop compares each scope's live RSS to its **held reserve** and RAISES an
over-scope's `oom_score_adj` above 500 (scaled by over-fraction, capped at 1000), lowering
back to 500 when it returns within reserve. More precise (differentiates compliant vs
over-committing delegate scopes). Deferred because:
- **Blocked by anonymous leases:** the per-test confine-reserve leases carry **no scope_id**
  (deliberately, to avoid the N-workers-one-scope collision) — so the daemon cannot attribute
  held per-test reserves to a delegate scope to compute its compliance. Option B first needs
  the per-test leases tagged with their scope id (a confine-reserve wire change).
- **New daemon machinery** (a scan loop like the reaper), **races** (fork inherits the
  current adj; a scope that just crossed its reserve keeps the stale adj until the next scan
  — a bounded wrong-victim window), and `/proc/<pid>/oom_score_adj` writes across a scope's
  live pids (permission: raising is always allowed; keep ≥ 0 so no `CAP_SYS_RESOURCE`).
- Cuts against the architectural-simplicity rule for a residual that Option A already
  largely covers.

## 3. Safety

- **Victim-ORDER only.** No change to admission, the reserve ledger, `memory.max`,
  `memory.high`, or `oom.group`. A wrong adj can only change *which* confined task the kernel
  kills under an OOM that was going to happen anyway — never causes an OOM, never affects a
  non-OOM path.
- **Desktop protection preserved/strengthened:** all confined tasks stay `≥ 500 > 0` (host),
  and delegate jobs (the risky class) become *more* preferred victims than host processes —
  aligned with the existing desktop-protection intent of `adj=500`.
- **Fail-safe:** if the class value is unset/unwritten, the behaviour is the current flat 500.
- **Not a substitute for bounding the over-commit** (AIRA-25/26 / a delegate aggregate cap);
  this only limits the *collateral* when an over-commit does happen. Paired with the swap
  bump (2→8 GiB, `43ab203`) as defense-in-depth.

## 4. Tests

1. **Class assignment** (runner real-cgroup): a non-delegate confine writes
   `oom_score_adj=500` to its setup child; a `--delegate-ram` confine writes `800`; both are
   inherited by descendants (read `/proc/<child>/oom_score_adj`, via the existing REAL
   `RunConfineSetup` real-cgroup pattern at `confine_linux_test.go:1789` — a delegate-ram
   real-cgroup variant launches daemonless via `delegateRAMScopeFallback()`, no daemon
   needed). **MUST-FIX porous fixture (Fable):** `runConfineTestSetup`
   (`detach_linux_test.go:36-49`) currently DISCARDS the parsed `oomAdj` and hardcodes
   `500\n` — so any class-value assertion routed through it is vacuous. Make the fixture
   write the PARSED value so the delegate class is observable.
2. **Ordering + clamp** (unit): the delegate value > the non-delegate value > 0 (host); env
   overrides are validated + clamped to `1000 ≥ delegate > non-delegate ≥ 500` at argv-build
   (an out-of-range or inverted value is rejected, not silently written / not left
   `priorities=unverified`).
3. **No side effects**: the reserve/admission/`memory.max` paths are byte-identical (the
   change is isolated to the oom_score_adj value); confirm the confine trailer
   (`priorities=applied` unchanged) and that admission tests are untouched. Existing pins
   `confine_linux_test.go:197/1319-1338/1794-1804` are all non-delegate → stay `500`.
4. **Fan-out honesty test** (Sol): a realistic cross-scope case — one non-delegate airtight
   process vs a delegate suite spread over many small workers — documenting that the bias +
   scope-group-kill picks the delegate scope for MODERATE airtight sizes, and (honestly) that
   a sufficiently large single airtight process is not protected. This test PINS the bias's
   documented boundary, it does not assert a false guarantee.
5. Full daemon + runner suites green under `aira confine`, `-race` clean.

## 5. Rollout

Client/runner-side only (the confine setup helper writes the adj) → **no daemon restart**;
binary rebuild + swap + `aira skill install`. Deploy watched. **Owner-gated** (this spec is
brought back for the owner's go before building, per the AIRA-27 decision). After deploy,
**notify all fastest.ee sessions** (owner instruction).

## 6. Deferrals

Option B (dynamic reservation-compliance re-writer) + its prerequisite (scope_id on per-test
confine-reserve leases); bounding the delegate aggregate at source (dynamic scope cap /
fail-closed per-test gate — the AIRA-25/26 per-worker-admission territory); a slice-level
`oom.group` is explicitly NOT pursued (it would group-kill the whole slice = every session).
