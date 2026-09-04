# AIRA-62 — `confine` CLI collapses `--memory-reserve` into `--memory-max`

Plan v2 (Tier A, simplification programme Phase 1 item 3). Base: `d878d9a`.
Branch: `aira62-memory-max-collapse`.

v2 folds the two plan-review lineages recorded in §8: Codex/Sol (BLOCK) and
DeepSeek-V4-pro (APPROVE-WITH-CHANGES). Gemini was unavailable (two consecutive
transport failures); noted, not a blocking gate.

## 1. Verified current state

Line numbers in the ticket were taken before Phase 0 (`d878d9a`) landed. Re-derived at HEAD:

| Ticket says | Actually at HEAD |
|---|---|
| `cmd/aira/main.go:857-859` | `cmd/aira/main.go:906-908` |
| `internal/runner/confine_linux.go:459-461` | `internal/runner/confine_linux.go:493-496` |

`cmd/aira/main.go:906-908`, inside `runConfineCommand`, after `--memory-reserve` /
`AIRA_CONFINE_RESERVE` have been parsed into `reserve`/`reservePinned`:

```go
if maximum > 0 {
    reserve, reservePinned = maximum, true
}
```

`internal/runner/confine_linux.go:479-496`:

```go
declaredReserve := request.MemoryReservePinned && request.MemoryReserve > 0
declaredReserveBytes := request.MemoryReserve
pinned := request.MemoryReservePinned || reserve > 0
if reserve <= 0 {
    if request.DelegateRAM {
        reserve = DefaultDelegateRAMOverhead   // 512 MiB
        pinned = true
    } else {
        reserve = DefaultConfineMemoryReserve  // 4 GiB
    }
}
if !request.DelegateRAM && request.ScopeMemoryMax > 0 {
    reserve = request.ScopeMemoryMax
    pinned = true
}
```

Verified facts:

- `grep 'ConfineRequest{' --include=*.go | grep -v _test` returns **exactly one** non-test
  producer: `cmd/aira/main.go:930`. `confine` has no MCP tool (`cmd/aira/confine_test.go:399`
  asserts `descriptor.MCPTool == ""`), so the CLI is the sole path into `runner.Confine`.
  The runner's `!request.DelegateRAM` guard is therefore **unreachable in production**: by the
  time it runs, `MemoryReserve` already equals `ScopeMemoryMax` and the branch is a no-op.
- `MinPinnedScopeCap` (`internal/runner/confine.go:55`) is `1 << 20`, identical to the CLI's own
  `--memory-reserve` floor (`main.go:898`, `parseConfineArgs` `main.go:700-708`). No divergence
  is created by letting a declared reserve reach the runner unmodified.
- `AIRA_CONFINE_RESERVE` (distinct from `AIRA_CONFINE_RESERVE_CMD`) is read **only** at
  `main.go:892`, is not exported to confined children, and is not in `internal/pylib/env.go`'s
  allowlist. It is an operator-set ambient value.

### 1a. The four-cell truth table

`R` = declared `--memory-reserve` (or `AIRA_CONFINE_RESERVE`), `M` = `--memory-max`.
"Charge" = the reserve handed to `deps.admit`, i.e. what the daemon ledger books.

| # | flags | today (CLI substitutes) | runner alone would give | correct |
|---|---|---|---|---|
| 1 | non-delegate, `R` only | charge `R`, cap `R` | charge `R`, cap `R` | same |
| 2 | non-delegate, `M` only | charge `M`, cap `M` | charge `M`, cap `M` | same |
| 3 | non-delegate, `R`+`M` | charge `M`, cap `M` | charge `M`, cap `M` | same |
| 4 | delegate, `R` only | charge `R`, cap = learned ceiling | charge `R`, cap = learned ceiling | same |
| 5 | delegate, `M` only | **charge `M`**, cap `M` | charge 512M overhead, cap `M` | **runner** |
| 6 | delegate, `R`+`M` | **charge `M`**, cap `M` | charge `R`, cap `M` | **runner** |

Value-ordering rows, added on review (Sol P1 #4 — the six rows above test flag *presence*, not
relative magnitude):

| # | flags | today | after | direction |
|---|---|---|---|---|
| 5a | delegate, `M` = 256M (< 512M overhead) | charge 256M, cap 256M | charge 512M, cap 256M | charge **rises** above the cap |
| 6a | delegate, `R` = 8G, `M` = 2G | charge 2G, cap 2G | charge 8G, cap 2G | charge **rises** above the cap |
| 6b | delegate, `R` = 512M, `M` = 32G | charge 32G, cap 32G | charge 512M, cap 32G | charge falls (the ticket's repro) |

Rows 5a and 6a book *more* than the scope can consume — the **safe (over-booking) direction**,
and in 6a it is precisely what the operator asked for. Neither is clamped nor refused
(`CLAUDE.md` / architectural-simplicity: no new machinery for a case that already fails safe),
but both are **pinned by tests** so the behaviour is known rather than accidental. Note 5a is
not new: a delegate job with no `--memory-max` and a small learned ceiling already charges the
512M overhead against a smaller cap.

Rows 1–4 are unchanged by this fix. Rows 5, 6 and 6b are the bug.

Row 3 is **deliberate, documented behaviour**, not a second bug:
`internal/core/skill.go:318` ships this sentence to every agent —
*"Note `--memory-max N` on a non-delegate job UP-CHARGES the admission reserve to N (it does NOT
let you 'cap high, reserve low' — that combination over-reserves, it never under-reserves)."*
`.aira/tickets/AIRA-24.md:15` records the same semantics as a correction. It is also required
for containment: a non-delegate scope may genuinely grow to `memory.max` and nothing else
reserves on its behalf, so booking less than the cap would under-book the shared ledger.
**Row 3 is preserved exactly.**

### 1b. The green tests that prove nothing

`internal/runner/confine_linux_test.go` already contains three subtests asserting rows 4–6
correctly, and they pass today:

- `:1030` *"explicit smaller max wins without charging it as the reserve"* — asserts
  `admittedReserve == DefaultDelegateRAMOverhead` with `ScopeMemoryMax: 2<<30`.
- `:1043` *"explicit larger max wins over a smaller learned ceiling"* — asserts
  `admittedReserve == DefaultDelegateRAMOverhead` with `ScopeMemoryMax: 32<<30`.
- `:1066` *"no explicit reserve pins a small framework overhead not the unpinned estimate"*.

They pass because they call `confineWithDeps` directly with a hand-built `ConfineRequest`
that **no CLI invocation can produce**. This is the porous-test shape the project has been
hunting all evening: correct assertions, at a layer the product never reaches in that state.
The fix makes them load-bearing; the new CLI-boundary tests close the layer gap they left.

## 2. Root cause

Two layers decide the same value. The upper layer is a *thin face* that should only transcribe
what the operator typed, but it applies policy first and unconditionally, so the lower layer's
delegate-ram carve-out is dead. `CLAUDE.md`: *"The core is one downward-layered implementation
behind `core.Do`; CLI, MCP, Skill, daemon, and TUI are thin faces."*

## 3. Chosen fix: one decision site, in the runner

**Delete `cmd/aira/main.go:906-908`.** Nothing replaces it. The CLI passes exactly what the
operator typed; the runner becomes the single place that resolves a reserve.

Alongside it, **extract the resolution into an exported pure function**
`runner.ResolveConfineReserve(ConfineRequest) (reserve int64, pinned bool)` in the portable
`internal/runner/confine.go`, called from `confineWithDeps` in place of lines 481-496. This is a
pure move, not new policy. Both review lineages asked for it independently (Sol P1, DeepSeek
P1): without it the CLI test can only assert *transcription*, and the composition
"what the CLI builds → what the ledger is charged" is untested. With it, the CLI test resolves
its captured real request through the same production function the runner uses.

`declaredReserve` / `declaredReserveBytes` (`confine_linux.go:479-480`) stay **inline** and
unmoved. They deliberately read the ORIGINAL request before `MemoryReservePinned` is widened,
and the 20-line comment above them is load-bearing provenance reasoning; moving them would put
that reasoning at risk for no gain, since they are consumed at `:665`, not by admission.

Rejected alternative — mirror the `!DelegateRAM` guard in the CLI (the ticket's first suggested
direction). It fixes rows 5/6 but leaves the same rule transcribed in two layers, which is the
defect class that produced this bug, and it is the wrong shape for Phase 4: a generated
`ArgSpec` parser can express a flag-combination *constraint* but cannot express
`reserve = maximum` value substitution. With the policy wholly in the runner, Phase 4's codegen
has nothing to encode — the simplest outcome, and it discharges the plan's
§3.4 Carve-out B ordering requirement.

Placement decision for the programme's §7.1 matrix: **AIRA-62 lands in `cmd/aira/main.go` only**
(a 3-line deletion). It does not touch `internal/runner/confine_linux.go`, so it cannot collide
with AIRA-71 (already merged as `5ddedff`, and it chose the *documentation* fix in
`internal/core/skill.go` regardless).

### 3.1 Considered and declined: a stderr notice on the row-3 up-charge

The ticket says *"an explicit `--memory-reserve` must never be silently discarded — refuse the
combination or honour it, but do not substitute."* For rows 5/6 this fix **honours** it. For
row 3 the substitution remains, so the instruction's letter would suggest adding a notice.

Declined, because it is not silent:

- `internal/core/skill.go:318` documents the up-charge in the installed agent guide.
- `internal/runner/confine.go:229` prints `reserve=<resolved>` (with `reserve-basis=`) in the
  operator-facing trailer on every run.
- `internal/runner/confine_linux.go:551` prints the pinned reserve every 15s while admission
  waits — the AIRA-58/59 fix for exactly this "what am I actually waiting for" question.

`CLAUDE.md` / [[architectural-simplicity]] is a hard rule against stacking machinery for an
honesty win that is already delivered. Recorded as a decision, not an oversight.
**Reviewer question Q1: do you agree row 3 needs no new output?**

### 3.2 The over-commit safety argument the ticket demands

The ticket flags that this is *"a memory-accounting change in the over-commit direction … only
safe if the per-test children genuinely cover the suite's usage — which is not true when the
pytest RAM governor is disabled, or when the delegate-ram payload is not pytest at all."*

Sol **BLOCKed** on exactly this, demanding "an explicit owner-approved overcommit policy/bound"
before landing, and correctly noted my v1 draft misattributed the structural fix to AIRA-16
(a *watchdog* ticket). The correct ticket is **AIRA-28**. Corrected, and it turns out to be
decisive against the block. The argument that this is safe to land:

1. **Today's charge is not a bound.** It applies only when an operator happens to type an
   *optional* `--memory-max`. Every delegate job without that flag — including the documented,
   recommended aitest and pytest invocations (`skill.go:320`) — already books 512M against a
   learned or 48G-fallback ceiling (`confine_linux.go:685-690`). A constraint that binds only
   the subset of jobs whose operator typed an optional flag bounds nothing in aggregate.
2. **It is one accidental cell of a *shelved* design.** `.aira/tickets/AIRA-28.md` ("Bound the
   delegate-ram aggregate so `aira.slice` can never over-commit — structural fix, whole-suite
   airtight charge") lists among **its own** deliverables: *"explicit wire charge-field;
   `--memory-max` on delegate now charges N"*. In AIRA-28 that cell is coherent because the
   same design sets *scope `memory.max` == the charged reserve*, unifying charge and cap.
   Today's code has the charge **without** the unification and **only** for flag-typers.
   AIRA-28 was built and verified, then **SHELVED** by owner pivot to AIRA-29 (dynamic reserve)
   because the airtight model under-utilises the machine. So the behaviour Sol wants preserved
   is half of a design the owner deliberately declined.
3. **Sol's precondition is owner-held territory this ticket must not pre-empt.** AIRA-28 is
   shelved; AIRA-29 that supersedes it is explicitly **ON HOLD** by owner decision pending
   aitest (programme plan §4.1 candidate 14). Blocking a filed P1 bugfix on an aggregate bound
   the owner has put on hold freezes it indefinitely — and the programme plan schedules AIRA-62
   to land in **Phase 1**, with Phase 4 blocked on it (§3.4 Carve-out B).
4. **The accidental mitigation is net-harmful, with three recorded incidents.** AIRA-24: 1785s
   wait then `E_ADMIT_SATURATED`, zero tests run. AIRA-59: a machine-wide admission stall from
   delegate merge-gates double-booking their whole 32G scope cap *on top of* their per-test
   `confine-reserve` children — the exact double-book the 512M overhead exists to prevent.
   AIRA-67: a SIGKILLed merge-gate, whose body already names AIRA-62 as probable context. A
   silent 64× over-reservation that saturates a shared 63G slice used by ~11 concurrent
   sessions is a worse, and *realised*, failure mode than the mitigated accounting gap.
5. **Live mitigations for the residual already exist and are deployed.** AIRA-27's class-based
   `oom_score_adj` (delegate scopes are the preferred victim at 800, airtight jobs protected at
   500), the MemAvailable-authoritative memory watchdog running in `enforce`, and per-scope
   `memory.oom.group` so a runaway kills its own scope. AIRA-16(2)'s slice-internal-pressure
   trigger is the open belt-and-braces item.
6. **An operator who wants airtight accounting has a supported, documented route**, in the same
   `skill.go:318` paragraph: drop `--delegate-ram` and use
   `aira confine --memory-reserve R -- <cmd>`, hard-capped at `R`.

**Disposition of Sol's BLOCK: overridden, with the reason recorded rather than assumed.**
The residual is real, pre-existing, mitigated, and owned by AIRA-28/AIRA-29. This plan appends
that residual to AIRA-28's body as an input to the owner's aggregate-bound decision (§6), which
is the honest discharge — not a new bound invented inside a P1 bugfix.

**Answer to Q2** (is any workload's 32G charge load-bearing rather than accidental?): Sol says
yes — mixed `make merge-gate` targets, projects with the legacy governor disabled, non-pytest
payloads, and aitest can all be using it as their only aggregate bound. Accepted as true *and*
as not decisive: they are equally unbounded today whenever `--memory-max` is omitted, so the
protection they are relying on is one they cannot rely on. Recorded on AIRA-28.

## 4. Invariants

- **I1** Rows 1–4 of §1a are identical before and after **in resolved, observable behaviour** —
  the reserve and `pinned` handed to admission, the scope `memory.max` written, the trailer's
  `reserve=`, and the scope id. *Narrowed on review (Sol P2):* they are **not** byte-identical
  at the CLI boundary. For rows 2 and 3 the raw `ConfineRequest.MemoryReserve` /
  `.MemoryReservePinned` do change (row 2: was `M`/`true`, now `0`/`false`; row 3: was
  `M`/`true`, now `R`/`true`), because the CLI now transcribes rather than pre-resolves. That is
  safe **only** because the runner overwrites both fields at `confine_linux.go:504-505` *before*
  admission, and `admitConfine` (`:929-937`) builds its `Request` from the **overwritten**
  `request.MemoryReservePinned`, which `admission_linux.go:360` then puts on the wire as
  `"pinned": !req.DaemonEstimateMemory || req.MemoryReservePinned`. Both lineages raised this as
  a possible P0 (Sol #1, DeepSeek P0/Q4: "daemon may estimate instead of honoring M"). **Read in
  source and refuted** — but it is the single most fragile link in the fix, so I2 pins it.
- **I2** For rows 5/6, the reserve reaching `deps.admit` is the declared `--memory-reserve` if
  given, else `DefaultDelegateRAMOverhead`; and `MemoryReservePinned` is true in both cases.
- **I3** For rows 5/6, the scope `memory.max` written is still the explicit `--memory-max`
  (`confine_linux.go:685` only supplies a ceiling when there is no explicit one). The fix
  changes the *charge*, never the *cap*.
- **I4** `--memory-reserve` below 1 MiB is still refused, by `parseConfineArgs` and by
  `confine_linux.go:401`, with `E_CONFINE_ARGUMENT_INVALID`.
- **I5** `AIRA_CONFINE_RESERVE` still supplies a pinned reserve when `--memory-reserve` is
  absent, and `--memory-reserve` still beats it.

## 5. Tests (TDD — each written to fail first)

New/changed, in order:

1. `cmd/aira/confine_test.go` — **rewrite** `TestConfinePinnedReserveFlagEnvironmentAndMemoryMaxPrecedence`.
   It currently asserts, at the mocked `runConfined` boundary, that
   `--memory-reserve 12M --memory-max 16M` yields `MemoryReserve == 16M`. That assertion *is*
   the bug, pinned. Replace it with a **transcription-fidelity** table over all six rows of
   §1a asserting `MemoryReserve`, `MemoryReservePinned`, `ScopeMemoryMax` and `DelegateRAM` are
   exactly what the operator typed. This is a rewrite of a load-bearing test, so it is called
   out explicitly rather than slipped in: the invariant it used to pin (row 3's end-to-end
   16M charge) is not dropped — it moves to test 2 below, where it is actually decided.
   The rewritten test still fails against the unfixed code (rows 5/6 and row 3 all carry
   `MemoryReserve == ScopeMemoryMax`), so it is not a masking rewrite.
2. `internal/runner/confine_linux_test.go` — new
   `TestConfineReserveResolutionAcrossDelegateAndMemoryMax`: the same six rows driven through
   `confineWithDeps` with an injected `deps.admit`, asserting the reserve **and**
   `request.MemoryReservePinned` handed to admission, and the `memory.max` written. Rows 1–4
   are the I1 regression net; rows 5/6 are the fix. Row 3 is where the up-charge invariant now
   lives.
3. `cmd/aira/confine_test.go` — new
   `TestConfineDelegateRAMWithMemoryMaxDoesNotChargeTheCap`: the narrowest possible
   ticket-shaped regression, driving the exact reproduction from the ticket body
   (`--delegate-ram --memory-max 32G --memory-reserve 512M`) and resolving the **captured real
   request** through `runner.ResolveConfineReserve` — the production function — to assert the
   ledger charge is `512<<20`, not `32<<30`. This is the composition proof both lineages
   demanded; a mocked-`runConfined` assertion alone would only prove transcription.
4. `internal/runner/admission_linux_test.go` — new
   `TestConfineDelegateRAMWithMemoryMaxPutsOverheadNotCapOnTheAdmitWire`: drive the **real**
   `admitConfine` against a `net.Pipe()` fake daemon (the existing pattern at
   `confine_linux_test.go:786-800`) and assert the decoded request frame carries
   `reserve == DefaultDelegateRAMOverhead`, `pinned == true` and `delegate_ram == true`. This is
   the I1/I2 net for the `:504-505 → :931 → :360` chain — the fragile link neither reviewer
   could rule out from the plan text alone.

Mutation testing (mandatory, per brief): reintroduce `if maximum > 0 { reserve, reservePinned
= maximum, true }` in a throwaway copy, confirm tests 1 and 3 fail; separately flip
`!request.DelegateRAM` to `true` at `confine_linux.go:493` and confirm test 2 fails. Record
exact exit codes for both directions.

## 6. Explicit deferrals

- Row 3's up-charge semantics — unchanged, and documented as deliberate (§1a).
- Delegate-ram's structural accounting gap — **AIRA-28** (shelved) / **AIRA-29** (owner ON
  HOLD), out of scope (§3.2). This PR **appends the residual to AIRA-28's body** so the owner's
  aggregate-bound decision carries it as an input; it does not decide it.
- Rows 5a/6a's over-booking — pinned by tests, not clamped (§1a).
- `aira run --memory-max`'s own reserve treatment — a different launch path
  (`internal/runner/runner_linux.go`), not touched by this ticket. Checked and not obviously
  broken; if a reviewer disagrees it becomes a new ticket, not a scope extension.
- The Phase 4 `ArgSpec` encoding of this rule — nothing left to encode (§3).
- **`skill.go:318`'s "UP-CHARGES … to N" is imprecise (build-review P2, pre-existing).** The
  rule SETS the charge to the cap: it raises it in the case the sentence describes (reserve
  below cap — "you cannot cap high and reserve low"), but a declared reserve *larger* than the
  cap is lowered to the cap. That is still exact and never under-books, since the scope cannot
  exceed its own `memory.max`, and the case is now pinned by a test row. The shipped sentence is
  incomplete rather than false, and it is pre-existing. `ResolveConfineReserve`'s own doc comment
  is corrected here; changing the shipped agent guide is left for the owner, since §6 deferred
  doc edits and the generated `SKILL.md` has its own assertions.
- No other `SKILL.md` / `README.md` text changes: `skill.go:318`'s sentence is about the
  **non-delegate** case and stays true; `skill.go:320` already tells aitest users no
  `--memory-reserve` is needed and that a delegate job pins 512M by itself, which this fix
  makes true for the `--memory-max` case too. **Reviewer question Q3: does any shipped doc
  string become false after this fix?**

### 6.1 Interaction audit (Sol's "an interaction I have missed is the most valuable finding")

Every consumer of the two fields whose CLI-boundary values change (rows 2 and 3), traced in
source at `d878d9a`:

| Consumer | Reads | Verdict |
|---|---|---|
| `admitConfine` `:929-937` → `admission_linux.go:360` wire `pinned` | the **resolved** `request.MemoryReservePinned` (overwritten at `:493`) | unchanged; pinned by §5 test 4 |
| admission-wait diagnostic `:538` | the local resolved `pinned` and `reserve` | unchanged |
| `result.Status.ReserveBytes` / trailer `confine.go:229` | `admission.reserve` else the resolved `reserve` | unchanged |
| `MinPinnedScopeCap` refusal `:401-403` | the **raw** request, before resolution | see below |
| `declaredReserve` / `declaredReserveBytes` `:478-479` | the **raw** request | see below |
| `confineScopeID` `:497` | `Name`, `DelegateRAM` | untouched |
| scope `memory.max` write `:682-694` | `ScopeMemoryMax`, `declaredReserve`, `admission.*` | see below |

The two raw-field readers are both safe, and for the same structural reason:

- **`declaredReserve` (`:653`, `:664`)** — both branches are gated on `scopeMemoryMax <= 0`,
  i.e. they run **only when no `--memory-max` was given**. The deleted CLI substitution fired
  **only when `maximum > 0`**. The two conditions are mutually exclusive, so the changed
  provenance can never reach either branch. Verified by reading `:653` and `:664`.
- **`MinPinnedScopeCap` (`:401`)** — refuses a *declared* reserve below 1 MiB. Post-fix a
  declared reserve reaches it unmodified instead of being replaced by `maximum`. That cannot
  newly refuse anything, because `parseConfineArgs` (`main.go:700-708`) and `runConfineCommand`
  (`:898`) both already reject `--memory-reserve` below the *same* 1 MiB bound at parse time,
  before the runner is called. `MinPinnedScopeCap == 1 << 20 == ` the CLI floor.

**Conclusion: the only behavioural change is the delegate-ram ledger charge (rows 5, 6, 6b).**

## 7. Risks

| Risk | Mitigation |
|---|---|
| A masking test rewrite hides a regression (the exact P0 shape build-review caught in #20) | §5 test 1 is declared as a rewrite; its dropped invariant is re-pinned in test 2 at the layer that decides it; the rewritten test is verified to fail against the unfixed code |
| Under-booking the ledger for a non-pytest `--delegate-ram` payload | Pre-existing for every delegate job without `--memory-max`; documented; AIRA-16 (§3.2) |
| Merge collision on `cmd/aira/main.go`, the repo's highest-churn file | 3-line deletion, no other Phase-1 item touches `runConfineCommand` |
| The runner's guard has its own latent defect that was masked by the CLI | §5 test 2 exercises it directly for the first time from nine angles |
| The resolved `pinned` fails to reach the daemon, so a row-2/3 job gets a history estimate instead of its declared `M` | §5 test 4 asserts the decoded admit wire frame (I1) |

## 8. Plan-review record

Two independent lineages. Gemini attempted twice, both transport failures — recorded as
unavailable, not as a pass.

### Codex / GPT-5.6-Sol — **BLOCK**

| Finding | Disposition |
|---|---|
| P0 — over-commit: a 32G-cap delegate job books 512M; per-test gate fails open; non-pytest legs ungoverned; demands an owner-approved bound first | **Overridden with reasons recorded** (§3.2, six points). The decisive one is Sol's own P1 below: the structural ticket is AIRA-28, whose *shelved* design owns this cell, and whose successor AIRA-29 is owner-held ON HOLD |
| P1 — AIRA-16 is a watchdog ticket, not the structural accounting fix; AIRA-28/AIRA-29 are | **ACCEPTED**, and it inverted the P0. §3.2 and §6 corrected |
| P1 — the CLI test mocks `runConfined`, so it proves transcription, not charging; demands a CLI→runner→admit-frame test | **ACCEPTED**. `ResolveConfineReserve` extraction (§3) + tests 3 and 4 (§5) |
| P1 — the six rows test flag presence, not value ordering; adds delegate `M < 512M` and `R > M` | **ACCEPTED**. Rows 5a/6a added (§1a), documented as the safe over-booking direction, pinned by tests, not clamped |
| P2 — "byte-identical" is false at the CLI boundary for rows 2–3 | **ACCEPTED**. I1 narrowed to resolved/observable behaviour |
| P2 — do not lose the environment contract in the test rewrite | **ACCEPTED**. Flag-over-env, env-only, and delegate+env rows retained in §5 test 1 |
| P2 — runner placement is correct; amend the programme plan's claim that codegen must encode an `ArgSpec` combination constraint | **ACCEPTED** (§3, §6) |
| Q1 — yes, warn before admission on the row-3 up-charge | **DECLINED**, see §3.1; Sol's premise ("a post-exit trailer may never print after rejection") is true, but the `E_ADMIT_SATURATED` message itself names the reserve (`confine_linux_test.go:798` pins it) and the 15s wait line prints it while queued |

### DeepSeek-V4-pro — **APPROVE-WITH-CHANGES**

| Finding | Disposition |
|---|---|
| P0 — ensure the admit frame consumes the **resolved** `pinned`, not raw `request.MemoryReservePinned`; row 2 flips after deletion | **REFUTED IN SOURCE** — `confine_linux.go:505` overwrites the field before `admitConfine` reads it at `:931`. Independently raised by Sol. Pinned anyway by §5 test 4 |
| P1 — Sol's "bound" is not a real bound; AIRA-28 owns it; don't block | **ACCEPTED**, matches §3.2 |
| P1 — add the value-ordering rows; document safe over-booking, don't clamp | **ACCEPTED**, as above |
| P1 — extract `ResolveConfineReserve` and test frame construction with the resolved tuple | **ACCEPTED**, §5 tests 3 and 4 |
| P2 — no row-3 stderr warning; existing surfaces suffice | **ACCEPTED**, §3.1 |

Where the lineages disagreed (land vs block), the disagreement is resolved on evidence neither
had: `.aira/tickets/AIRA-28.md`'s own deliverable list and its SHELVED status.

### 8.1 Build-review record (Sol, on the implemented diff)

**VERDICT: APPROVE-WITH-CHANGES. BLOCK-STATUS: RELEASED** — *"AIRA-28 explicitly assigns
delegate max-charging to the shelved airtight design, while AIRA-29 records the owner's
non-airtight pivot; source also confirms resolved fields are overwritten before real admission
reads them."* Both plan-review P0s (over-commit, and the raw-vs-resolved `pinned`) are therefore
discharged by the reviewer that raised them.

**POROSITY (its own words):** *"The CLI test is not logically circular because it compares
against literals, but it proves parser-plus-resolver composition, not actual admission; the
runner and wire tests separately cover that handoff."*

| P2 finding | Disposition |
|---|---|
| `skill.go:318` "UP-CHARGES to N" is false when reserve > max | **ACCEPTED in part** — `ResolveConfineReserve`'s doc comment corrected; the shipped guide left to the owner (§6) |
| Resolver edge equivalence is porous: pinned-zero, negative reserve, negative max all pass every table row | **ACCEPTED** — `TestResolveConfineReserveEdgeValues`, 7 direct cases |
| The ticket-repro test's name over-claims ("DoesNotChargeTheCap") since `runConfined` is mocked | **ACCEPTED** — renamed `...ResolvesTheReserveNotTheCap`, with a comment stating what it does and does not prove and pointing at the wire test |
| The wire test's 10s select only starts after `confineWithDeps` returns, so a protocol hang waits the 30-minute default | **ACCEPTED** — the test sets `AdmissionMaxWait: 15s`. Sol ran it 20x with no flake observed |

### 8.2 Mutation testing (throwaway copy under `~/tmp`, both directions)

| Mutation | Result |
|---|---|
| **1** — reintroduce `if maximum > 0 { reserve, reservePinned = maximum, true }` in `cmd/aira/main.go` | 6 subtests FAIL. The composition assertion reports `ledger charge=34359738368` — 32G, the ticket's exact symptom |
| **2** — drop `!request.DelegateRAM` from `ResolveConfineReserve` (the same bug relocated into the resolver) | FAILS at every layer: the two PRE-EXISTING delegate-ram subtests, the new runner table (4 rows), the wire-frame test (2 rows), AND the CLI composition tests (4 rows) |

Mutation 2 failing the CLI tests is the answer to the porosity question: because the CLI test
resolves through the production function rather than a restatement of it, a bug moved into the
runner still fails at the CLI seam.
