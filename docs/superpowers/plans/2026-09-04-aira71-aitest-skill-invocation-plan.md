# AIRA-71 — SKILL.md's recommended aitest invocation is wrong; fix the documentation

**Ticket:** AIRA-71 (P0, bug, `aitest`/`confine`/`honesty`/`skill`)
**Branch:** `aira71-aitest-skill-fix` off `9a65d47`
**Related:** AIRA-77 (P2, `--delegate-ram` arms both xdist governor and aitest)
**Authoritative spec:** `docs/superpowers/specs/2026-09-01-aitest-design.md`

**Revision history.** Rev 1 claimed `worker-admit` grants double-book the slice
admission ledger — **false**, withdrawn in §2c (caught by the Codex/Sol plan
review, confirmed by an independent source trace). Rev 2 overstated two further
things — that a tight outer cap trips `memory.oom.group`, and that direction 1
could not fix the gap in any form — both corrected here in §2b/§2c after the
Fable plan gate (GATE-PASS-WITH-CHANGES). Rev 3 is the gated plan.

## 1. The defect

`internal/core/skill.go:326` generates, into the installed agent-facing
`SKILL.md`, this instruction:

> Launch the whole invocation under a plain `aira confine -- pytest
> --aitest-workers=auto ...` — no `--delegate-ram`, no `--memory-reserve`, no
> per-test `aira_mem()` markers.

`internal/runner/confine_linux.go:757-778` wires aitest's four coordinates
(`AIRA_AITEST_LIB`, `AIRA_AITEST_WORKER_ADMIT_CMD`, `AIRA_AITEST_BOOTSTRAP_CMD`,
`AIRA_AITEST_MAX_WORKERS_FALLBACK`) **only** when `request.DelegateRAM` is true,
and unconditionally strips them otherwise. The documented invocation therefore
guarantees aitest never receives what it needs. The deployed
`~/.claude/skills/aira/SKILL.md` carries this text verbatim (verified). A
repo-wide grep confirms `skill.go:326` is the only place carrying it.

### 1a. What actually happens today — exact, not assumed

Two outcomes, depending on how the project registers the plugin:

- **If the project registers aitest via `AIRA_AITEST_LIB`** (the documented
  pattern, §1b), stripping that variable means the plugin is never put on
  `sys.path` and never registered, so pytest rejects `--aitest-workers` as an
  **unrecognized argument**. The run fails loudly — the *good* outcome.
- **If aitest is independently importable/registered**, `Supervisor.bootstrap()`
  finds `AIRA_AITEST_BOOTSTRAP_CMD` unset (`supervisor.py:186-189`), calls
  `_disable_daemon`, and emits one stderr line
  `"aira aitest: ... -- falling back to n_workers<=N, UNCONFINED (no per-worker
  RAM containment)"` (`supervisor.py:209-216`). Because
  `AIRA_AITEST_MAX_WORKERS_FALLBACK` was also stripped, N defaults to **1**
  (`supervisor.py:172`), and the pool is `min(worker_count, N)`
  (`supervisor.py:1197`).

Two corrections to the ticket's framing, recorded rather than repeated: the
degradation is **not silent** (there is a visible stderr warning), and the
fallback is **one** uncontained worker, not `min(requested, NumCPU)`. SKILL.md's
own sentence describing the fallback as `n_workers <= min(requested, NumCPU)` is
true only when the coordinates *are* wired — i.e. only under `--delegate-ram`,
the very flag it tells agents to omit.

### 1b. A second, independent activation gap in the same paragraph

Nothing on the Go side sets `PYTHONPATH` or `PYTEST_ADDOPTS`. Supplying
`AIRA_AITEST_LIB` alone does **not** register the plugin. The project must add a
conftest snippet, and it **must be guarded on the variable being set** — the
pattern is `internal/pylib/aitest/testdata/conftest.py:5-6,19-23`:

```python
import os, sys, importlib
_lib = os.environ.get("AIRA_AITEST_LIB")
if _lib:                      # guard is load-bearing, see below
    if _lib not in sys.path:
        sys.path.insert(0, _lib)
    importlib.import_module("aitest")
    pytest_plugins = ("aitest",)
```

The guard is not decoration: an unguarded snippet would raise
`ModuleNotFoundError` on any plain `pytest` or CI run without aira, breaking the
paragraph's own promise that "opting in is purely additive". With the guard, a
non-delegate launch fails loudly with "unrecognized arguments" (§1a) rather than
half-running.

`skill.go:326` documents no registration step at all, so even the corrected
invocation would not produce a working aitest run for a reader following the
skill literally. Fixing the flag while leaving this out would make the "fix"
itself dishonest, so it is in scope.

## 2. Direction taken — fix the documentation, not the code

The ticket offers two directions. **Direction 1 (decouple the coordinates from
`request.DelegateRAM`) is rejected**, with the narrow variant of it recorded in
§2e rather than left for a later reader to rediscover as an omission.

### 2a. The authoritative design spec requires it

`2026-09-01-aitest-design.md:144-149`, §3.3:

> **Outer scope** — the `aitest` run launches exactly like any other job under
> `aira confine --delegate-ram` today: unchanged slice-level
> `Σgranted ≤ cap−headroom`, unchanged `oom_score_adj=800` delegate class.

The code implements the spec. SKILL.md contradicts the spec. Changing the code
to match SKILL.md would invert which artifact is authoritative.

### 2b. `worker-admit` requires a finite outer `memory.max`; only `--delegate-ram` guarantees one on every path

The hard precondition is a *finite, delegated outer scope*, not the
`DelegateRAM` boolean as such. The daemon fails closed without it:
`readSliceMemory` returns reason `"unbounded"` on a scope whose `memory.max`
reads `"max"` (`internal/daemon/admit.go:1273-1275`), `evaluateWorkerAdmit` turns
that into `unevaluated` rather than a grant (`worker_admit.go:162-165` — honest,
no fake pass), and the supervisor force-classifies that specific reason as
permanently non-retriable (`supervisor.py:358-359`), landing in the whole-run
unconfined fallback. Matches spec §3.7 lines 327-337.

Which launch shapes guarantee finiteness (`confine_linux.go:635-694`):

| Launch shape | outer `memory.max` |
|---|---|
| `--delegate-ram` | **always finite** — `admission.scopeCeiling`, else `delegateRAMScopeFallback()` (48 GiB, `:983-989`), else hard `E_CONFINE_UNAVAILABLE` (`:685-693`) |
| non-delegate + `--memory-max` | finite — `scopeMemoryMax` from `request.ScopeMemoryMax` (`:635`), written at `:694`; the reserve is up-charged to match at `:492-496` |
| non-delegate + declared `--memory-reserve` | finite (`:665-667`) |
| non-delegate + real daemon grant | finite, `= admission.reserve` (`:676-678`) — the daemon-resolved value: signature history, else machine-wide p90 prior, else the client's 4 GiB (`admit.go:343-410`, prior at `:395-400`) |
| **non-delegate, unpinned, flock-fallback / timeout / unevaluated** | **uncapped** — deliberately (`:668-675`) |

`--delegate-ram` is the only shape finite **by construction on every path**. A
plain `aira confine -- pytest --aitest-workers=auto` is finite only when the
daemon answers; on the fallback paths it is uncapped and aitest permanently
loses containment. So a *blanket* direction 1 would not close the gap — it would
hand the supervisor working coordinates attached to a scope that is sometimes
uncapped, relocating the containment loss behind a path that *looks* governed.
(This is narrower than rev 2's claim that direction 1 could not work at all; see
§2e.)

A correction to a plausible over-claim: cgroup **delegation is not gated on
`DelegateRAM`**. `ensureDelegation` runs on every confine launch
(`confine_linux.go:453`) and `aitest-bootstrap` writes `+memory` to the outer
scope's `subtree_control` itself (`aitest_bootstrap_linux.go:77-79`). A
non-delegate scope *can* host nested sub-scopes. The blockers are the
finite-cap guarantee and the env gate, not the cgroup mechanics.

### 2c. Sizing: the non-delegate cap is the wrong shape — and worker RAM is not slice-accounted at all

**Rev 1 claimed `worker-admit` grants double-book the slice admission ledger.
That is false and is withdrawn.** `evaluateWorkerAdmit`
(`internal/daemon/worker_admit.go:157-288`) consults no reserve ledger — it reads
`memory.current`/`memory.max`/`memory.stat` from cgroupfs plus its own
in-process `workerJobState.grants`. Nothing reaches `queue.outstanding`. The
`DefaultDelegateRAMOverhead` comment (`confine.go:18-23`) concerns the legacy
per-test `confine-reserve` path and does not transfer to aitest.

**The flip side is itself agent-relevant and must be documented.** Under
`--delegate-ram` the slice ledger holds only the 512 MiB framework overhead
(`confine_linux.go:487`) for the whole suite, while
`resolveDelegateRAMScopeCeiling` (`admit.go:422-455`) bounds the outer cap at
`cap − headroom` with no availability subtraction. Workers may therefore grow to
that ceiling **entirely unaccounted at the slice level** — strictly less
slice-accounted than the legacy governor's per-test `confine-reserve`, which at
least charged the ledger. SKILL.md's current wording "admits it individually
through the daemon" reads as ledger-accounted admission and must be corrected:
worker admission is against the **job's own outer ceiling**, not the shared
slice ledger, so the confinement section's existing "NOT airtight" caveat
applies to aitest with *more* force, not less.

On sizing: `CreateWorkerScope` creates `.aira-worker-N` **directly beneath the
outer scope** (`worker_scope_linux.go:33-38`), `.aira-supervisor` its sibling.
cgroup v2 accounting is hierarchical, so the outer `memory.max` is the aggregate
ceiling over supervisor plus every worker; spec §4 line 387 makes "per-worker
`memory.max` ≤ the outer scope's own cap" an invariant.

**Corrected consequence** (rev 2 overstated this): a tight outer cap does *not*
trip `memory.oom.group`. The aggregate guard (`worker_admit.go:214-281`, spec
§3.3 lines 165-183) exists precisely so Σ(granted caps) + supervisor live usage
never exceeds the outer cap under any finite ceiling. The real failure modes
under a tight non-delegate cap are **fewer workers**, or
`reject:exceeds-ceiling` → `WorkerAdmitRequestTooLarge` → `_fail_queue_too_large`
marking the whole remaining queue **unevaluated** (`supervisor.py:218-231`). The
"wrong shape" conclusion stands; the OOM-kill claim does not.

### 2d. Direction 2 needs no new flag and no new reserve value

`aira confine --delegate-ram -- pytest --aitest-workers=auto ...` with **no**
`--memory-reserve` already does the right thing — verified: no reserve given
pins `DefaultDelegateRAMOverhead`, 512 MiB (`confine_linux.go:482-491`). So
SKILL.md's claims "no `--memory-reserve`, no per-test `aira_mem()` markers"
stay **true and unchanged**; only the "no `--delegate-ram`" clause is false.

### 2e. Alternatives considered and deferred

- **Narrow direction 1 — wire coordinates when `DelegateRAM || declaredReserve`.**
  A single condition change at `confine_linux.go:757` would make
  `aira confine --memory-reserve 16G -- pytest --aitest-workers=auto` work, and
  it would be *airtight*: finite on every path (`:665-667`), fully
  ledger-charged, protected (non-delegate) `oom_score_adj` class. §2c's
  "wrong shape" argument applies to the **unpinned** reserve, not a declared
  one, and `aitest-bootstrap` does not check any delegate-scope marker
  (`aitest_bootstrap_linux.go:29-79`). **Deferred, not dismissed.** Trade-off:
  it buys an airtight variant at the cost of the operator hand-sizing the whole
  suite aggregate, and adds a second supported launch shape for a doc fix. The
  agent-facing default should need no aggregate sizing. Recorded here so a
  later reader finds it considered rather than missed; a follow-up ticket is
  the right home if the airtight variant is wanted.
- **A dedicated `--aitest` flag.** Would imply everything `--delegate-ram`
  already implies (small pinned framework reserve, guaranteed finite ceiling,
  delegate `oom_score_adj` class) — an alias, not a capability. Rejected per
  architectural-simplicity: keep the primitive, correct the documentation.

## 3. Scope

### In scope

1. **`internal/core/skill.go:326`** — rewrite the generated aitest paragraph to:
   - recommend `aira confine --delegate-ram -- pytest --aitest-workers=auto ...`;
   - keep "no `--memory-reserve`, no per-test `aira_mem()` markers" (still true)
     and say *why* delegate-ram is required (the outer scope must have a
     guaranteed finite cap; workers are nested sub-scopes beneath it);
   - state plainly that **without `--delegate-ram` the coordinates are stripped**,
     degrading to a **single** uncontained worker with one stderr warning, or
     failing outright with "unrecognized arguments" when registered per §1b — so
     an agent can recognise the old form in an existing script as broken;
   - document the **guarded conftest registration step** (§1b), without which
     `--aitest-workers` is not a valid option at all;
   - **correct the accounting claim** (§2c): worker admission is against the
     job's own outer ceiling, **not** the shared slice ledger — the slice holds
     only the 512 MiB framework overhead — so the "NOT airtight" caveat in the
     confinement section applies to aitest with more force;
   - add the **AIRA-77 caveat, actionable**: under `--delegate-ram`,
     `AIRA_TEST_MEM_GOVERNOR=1` is set unconditionally (`env.go:153`), arming the
     legacy `aira_xdist_governor` plugin's per-test reservations *inside* every
     aitest worker (`worker.py:393` runs its `pytest_runtest_protocol` wrapper;
     the hookwrapper at `__init__.py:388-389` has no xdist-worker gate). Note
     that `AIRA_GOVERNOR=off` disables only the governor's **CPU** checkpoint
     (`__init__.py:164`), not its RAM reservations (`:338-340`). Two actions
     that do work: don't register `aira_xdist_governor` in the conftest, or
     block it per invocation with `-p no:aira_xdist_governor` (it is registered
     by module name).

2. **Tests** (TDD, written first):
   - `internal/core/skill_test.go` — assertions **scoped to the aitest section
     only**. Load-bearing: `skill.go:324` (the confinement section) already
     contains `--delegate-ram` repeatedly, so a whole-document `Contains` (the
     existing style at `skill_test.go:60-64`) would pass vacuously. The
     extractor must **fail closed**: assert the `## Running pytest suites with
     aitest` heading was found and the section is non-empty, otherwise a renamed
     heading makes the negative assertion vacuously true. Pin the exact
     recommended command `aira confine --delegate-ram -- pytest
     --aitest-workers=auto`, and pin the forbidden phrase as the exact
     backticked string ``no `--delegate-ram` `` — a loose negative match would
     false-fail on the new text's legitimate "without `--delegate-ram`".
   - `internal/runner/confine_linux_test.go` — a **non-delegate** launch with a
     **populated `RuntimeDir`** delivers **none of all four** `AIRA_AITEST_*`
     keys. The existing
     `TestConfineNonDelegateLaunchStripsInheritedAitestEnvironment`
     (`:1496-1501`) passes `RuntimeDir: ""`, and `AppendAitestChildEnvironment`
     early-returns after stripping on empty `runtimeDir` (`env.go:53`), so it
     **cannot distinguish** the two directions. **Anti-porosity requirement:**
     the new test must *also* positively assert that `AIRA_PY_LIB` (set
     unconditionally, `env.go:140`) and `AIRA_CONFINE_SCOPE_ID` **were**
     delivered on the same launch — otherwise an `extractForChild` failure makes
     `appendChildEnvironment` early-return (`env.go:130-139`) and the four-key
     absence assertion passes for the wrong reason, leaving it no better than
     the old test.
   - A paired **delegate-ram** test with a populated `RuntimeDir` asserting all
     four keys *are* delivered with a non-empty `AIRA_AITEST_LIB`, so the pair
     brackets the gate in both the false-fail and false-pass directions. It
     needs the `deps.writeScopeMemoryCap` stub used by
     `TestConfineInjectsDaemonGovernorEnvironment` (`:1469`), and should set
     `XDG_DATA_HOME` to `t.TempDir()` since extraction writes there
     (`extract.go:52-70`).

3. **AIRA-77 trace recorded** — the ticket says "not investigated further", and
   this change's own recommendation is what makes AIRA-77 bite, so the verified
   trace must be attached to it.

   **Delivered as a structured finding, not a body edit** (wording revised after
   the final gate flagged the original as undelivered). Ticket markdown is
   untracked in the root checkout and absent from every feature worktree, so a
   body edit cannot ride in this PR. The trace is instead recorded via
   `aira finding add` against AIRA-77 — AIRA's own primitive for durable
   evidence, queryable and attached to the ticket — plus an
   `AIRA-71 relates AIRA-77` link. The finding also corrects AIRA-77's own
   framing: the two mechanisms charge **different** ledgers, so the cost is
   redundant reservation and slice contention, not one ledger double-booked.

4. **`AIRA_AITEST_ESTIMATED_BYTES` units** (added after the final gate). The
   variable is parsed `int(raw)` (`aitest/__init__.py:141-146`); a `4G`-style
   value raises `ValueError` and silently falls back to the 512M default with
   **no** warning, while only out-of-range integers warn. Every other size in
   SKILL.md accepts K/M/G, so naming this variable without its units would
   reproduce this ticket's own defect class. The default and the plain-integer
   requirement are now documented and test-pinned. Making the malformed case
   warn is code, and is correctly out of scope here.

### Explicitly out of scope (deferred, written down, never silent)

- **No change to `confine_linux.go`.** The `if DelegateRAM { Append } else
  { Strip }` gate is correct. The unconditional strip guarding stale inherited
  coordinates (`:766-777`, from a prior Fable build-review) is preserved exactly.
- **AIRA-77 is documented and traced, not fixed.** The real resolution is spec
  §3.8's deletion of `aira_xdist_governor`.
- **The narrow `DelegateRAM || declaredReserve` variant** (§2e) — deferred with
  its trade-off recorded.
- **No argv-sniffing of `--aitest-workers` in `confine`.** Rejected as a
  runner-layer dependency on a pytest-specific flag.

## 4. Invariants preserved

- The stale-coordinate strip on every non-delegate launch is unchanged; the
  shape stays strip-then-append for nested delegate launches and strip for
  nested non-delegate ones, so no leak is reintroduced.
- `AppendAitestChildEnvironment` remains strip-first on every path.
- No admission, ledger, cgroup, or scope-cap behaviour changes. This is a
  generated-text change plus tests pinning existing behaviour.
- The skill version is a content hash (`skill_test.go:343-367`) and no golden
  pins this paragraph, so no existing test breaks on the reworded text.

## 5. Risks

- **Live-deployed artifact.** The generated SKILL.md must be redeployed
  (`aira skill install <dir> --force`) before agents see the fix; until then the
  wrong text stays live. Reported explicitly, not assumed.
- **Test brittleness.** Section-scoped assertions pin short, meaning-bearing
  strings, so ordinary rewording survives but the specific regression does not.
- **Recommending `--delegate-ram` where both plugins load** (AIRA-77) —
  mitigated by the actionable inline caveat.

## 6. Expected yield

The generated and deployed agent guidance stops instructing every agent into a
shape that cannot work; the paragraph stops overstating how much slice-level
accounting aitest workers get; and the doc↔`DelegateRAM` contract becomes
test-pinned in both directions so it cannot silently re-drift.
