# AIRA-106 — Two-parameter dynamic slice ceiling

Status: plan **v3** — built. v1 gated **PASS-WITH-CHANGES** by Fable (no P0,
5×P1, 8×P2) and **PASS-WITH-CHANGES** by DeepSeek-V4-pro (2×P1, 5×P2),
independently. v2 re-gated **PASS-WITH-CHANGES** by Fable (no P0, 1×P1, 4×P2):
four of the five v1 P1s confirmed RESOLVED, one PARTIAL (§4.1's install fix was
defeated on the `sudo` path) and one PARTIAL (§6's substituted evidence did not
measure the claim). §0.1 lists every change and which reviewer required it.
Ticket: `.aira/tickets/AIRA-106.md`
Refines: AIRA-103 (`docs/superpowers/specs/2026-09-05-aira103-dynamic-slice-ceiling-design.md`)
Depends on: AIRA-33 (governor deletion) — landed as `6611240`; this plan is written
against `origin/master@f1f699a` and reintroduces no governor reference.
Related: AIRA-91 Part B, AIRA-102, AIRA-73, AIRA-64/65 (the memory watchdog whose
kill band §2.5 now bounds this subsystem against).

---

## 0. Scope in one paragraph

AIRA-103's actuator is **not touched**. The in-process, non-kernel-enforced
capacity throttle — published by the `sliceceiling` subsystem and applied at
exactly the two capacity sites `admitEffectiveMaximum` documents
(`evaluateAdmitQueue` → `checkedAvailable`, `admit.go:1568`, and
`confineManagement` → `CeilingBytes`, `confine_manage.go:114`; AIRA-33 deleted the
third) — stays byte-for-byte as built; two adversarial review rounds already
rejected a kernel-enforced write and nothing here revisits that. What changes is
**the arithmetic that produces the published number**, from AIRA-103's single
blended headroom to the owner's explicit two-parameter model; the two parameters
become configurable; and the mechanism is taken out of `mode=off` on this machine
via the existing mode ladder.

### 0.1 v1 → v2 changes

| # | change | required by |
|---|---|---|
| 1 | `freeMin`'s relationship to the watchdog's kill band is stated (§2.5), validated (refuse `freeMin < watchdogLowMemAvailable`), pinned by a test, and put to the owner in §6 | Fable P1(a) |
| 2 | §6's enforce criterion restated against evidence that actually exists (`confine_peak_history`), not a per-grant log line that does not | Fable P1(b) |
| 3 | `aira install` mode preservation: an omitted `--watchdog`/`--slice-ceiling` now defaults to the value in the installed unit, not to `observe`. **Live evidence: this bug has already silently reverted the watchdog to `observe` on this box.** | Fable P1(c) |
| 4 | v1's test 5 (the `min`-placement identity) dropped as vacuous; replaced by a machine-bound damping case | Fable P1(d) |
| 5 | §7.2's real-cgroup bound corrected for quantisation and the slab term: `published ≥ sliceAnon − quantum`, with the `available > 0` clause given its true precondition | Fable P1(e) **and** DeepSeek P1 (independently) |
| 6 | §2.2's `outstanding` qualifier corrected — `outstanding ≥ effectiveCurrent` is the NORMAL case under AIRA-67, not the exception | Fable P2 |
| 7 | §2.4 refuses on `MemTotal − reserveMax <= admitSliceHeadroomBase`, not merely `<= 0`; `validSliceCeilingDeps` bounds restated | Fable P2; DeepSeek P2 |
| 8 | `Basis` given an explicit comparison point (raw, pre-quantisation) and a tie rule | Fable P2; DeepSeek P2 |
| 9 | The observe-mode line chooses its cause clause per basis instead of appending a second cause | Fable P2 |
| 10 | Warm-up / TTL-expiry dropping the static term recorded as an accepted gap (§9) | Fable P2 |
| 11 | The "reserve inside the `(machineTerm − headroom, memory.max − headroom]` band waits forever" gap recorded (§9) | Fable P2 |
| 12 | The cheaper honesty alternative Fable named recorded as considered-and-rejected (§3) | Fable P2 |
| 13 | Test-fixture `memTotal` pinned ≥ 78 GiB and the `reserveMax = 0` equivalence given its precondition | Fable P2 |
| 14 | `MemTotal` read-once staleness recorded as a residual (§9) | DeepSeek P2 |

### 0.2 v2 → v3 changes

| # | change | required by |
|---|---|---|
| 15 | §4.1's install fix completed: an absent mode option is the ZERO VALUE from argv to render, `runInstall`/`runUserInstall` no longer pre-default it, and `reexecRequestFor` forwards a mode flag ONLY when given — without which `sudo aira install` (the path install recommends) always forged an explicit flag and the preservation could never fire. `--watchdog-interval` preserved the same way. | Fable r2 P1 |
| 16 | §6's enforce criterion measures the right thing: the subsystem's own log line now prints `sliceAnon` beside the effective ceiling (one field, no new logging — both values are already in hand at publish time), and the criterion is stated on that pair. `MAX(peak_rss)` was a per-JOB figure against a slice-AGGREGATE ceiling, from a table pruned to 20 rows per signature. | Fable r2 P2 |
| 17 | §7.2 cites the existing signal test's actual `MemAvailable` model (`simulatedTotal − outside − sliceAnon`, not `− memory.current`); the negative control is specified to sustain the violation for more than a full damping window and to assert on the PUBLISHED snapshot; the Claim gains its `machineTerm` precondition and the corollary says "suffices", not "requires". | Fable r2 P2 |
| 18 | §3 gains rows for observe+empty basis and launcher+empty basis; the observe line now distinguishes throttled from unthrottled instead of asserting a cause for a ceiling nothing reduced. | Fable r2 P2 |
| 19 | §2.4's `freeMin` bound becomes `>= MemTotal − admitSliceHeadroomBase` (the same 2 GiB band already refused for `reserveMax`); §2.5 records that the throttle's steady state sits below the watchdog's 16 GiB *recover* threshold, so a tripped watchdog will not emit `recovered` while AIRA sits at capacity. | Fable r2 P2 |
| 20 | Test 1 gains a **sub-quantum-difference** row — the only case that separates a basis decided on raw figures from one decided after quantisation. Test 5's "RED against a machine term that damps" claim removed as vacuous (a constant cannot damp). | Fable r2 P1(d) note; own red-check |

---

## 1. The owner's decision, verbatim

> "Currently we specify to leave 16GB for the system. Instead, we should specify
> a maximum amount to leave and an amount to leave free — so 'leave 16GB on the
> table' and 'leave 8GB free', meaning the slice would take
> min(total-16GB, free-8GB)"

Read as two independent bounds:

- **`reserveMax`** (default **16 GiB**) — *leave this much on the table.* A
  static upper bound on how much of the machine the slice may ever claim,
  regardless of how idle everything else is. Condition-independent.
- **`freeMin`** (default **8 GiB**) — *leave this much genuinely free.* A dynamic
  floor: the slice may grow until doing so would push system-wide `MemAvailable`
  below this.

Units: the owner wrote "GB"; this plan uses **GiB** (`16<<30`, `8<<30`) because
every other memory constant in this codebase is binary and the shared parser
`runner.ParseMemorySize` (`internal/runner/memory_size.go:44-56`) treats
`16G`/`16GB`/`16GiB` as synonyms for `16<<30`. The distinction is immaterial to
the intent and is recorded so nobody later reads `16<<30` as a transcription
error.

---

## 2. The formula

```
current      = min(currentBefore, currentAfter)                                  // AIRA-103 torn-read guard, unchanged
sliceAnon    = max(0, current - inactive_file - active_file - slab_reclaimable)  // unchanged
affordable   = MemAvailable + sliceAnon                                          // unchanged
                                                                                 //
machineTerm  = MemTotal - reserveMax                        // NEW: "leave 16G on the table"
pressureTerm = affordable - freeMin                         // "leave 8G free", == AIRA-103's own
                                                            //    desired with reserve := freeMin
desired      = min(machineTerm, pressureTerm)
ceiling      = clamp(quantise_down(damp(desired)), 0, live memory.max)           // unchanged
```

### 2.1 `pressureTerm` is AIRA-103's existing formula, unchanged

The ticket asks that `currentSliceUsage` in the dynamic term be *reused*, not
rederived. It already is, exactly:

```
currentSliceUsage + (MemAvailable - freeMin)
  == sliceAnon + MemAvailable - freeMin
  == affordable - freeMin
  == sliceCeilingDesired(memAvailable, current, reclaimable, reserve := freeMin)
```

So the dynamic term is a **rename of the existing function's parameter**, not new
arithmetic. `readSliceCeilingParts`, `sliceCeilingAnon`, the torn-read guard, the
max-over-3-samples damping, the 256 MiB quantisation, the TTL hold and the
`unevaluated` contract are all untouched.

**Why `currentSliceUsage` must be `sliceAnon` and not raw `memory.current`.** This
is the one place a naive reading of the owner's words would produce a wrong
mechanism, so it is stated explicitly. If the slice's own **page cache** grows by
X, `memory.current` rises by X but `MemAvailable` does **not** fall (the kernel
counts reclaimable file pages as available). Using raw `memory.current` would
raise the published ceiling by X for free — permissive, and re-opening exactly
Finding B of AIRA-103. Using `sliceAnon` keeps both invariances the existing
tests already pin:

| slice grows by X as… | `MemAvailable` | `sliceAnon` | `pressureTerm` |
|---|---|---|---|
| anonymous memory | −X | +X | **unchanged** |
| page cache | 0 | 0 | **unchanged** |
| reclaimable slab | ≈0 (`MemAvailable` credits most of global `SReclaimable`) | 0 | **≈ unchanged** |
| *external* footprint | −X | 0 | **−X** |

The slab row is approximate on purpose: `si_mem_available` credits reclaimable
slab above the watermark, so slice slab growth is *nearly* invisible to
`MemAvailable` while `sliceCeilingAnon` subtracts it exactly. Any residual is in
the **restrictive** direction. AIRA-103 measured the live magnitude at 0.36 GiB;
it is recorded, not modelled (§9).

### 2.2 What the composition with `checkedAvailable` actually yields

`admitEffectiveMaximum`'s output reaches admission only through
`checkedAvailable(current, maximum, reclaimable, outstanding, headroom)`
(`admit.go:1694`, single production call site `admit.go:1588`), which computes
`ceiling := maximum - headroom` and charges `max(outstanding, current -
reclaimable)`. Note `reclaimable` there is `inactive_file + active_file` only —
`readSliceMemory` deliberately discards `slab_reclaimable` (`admit.go:2119-2122`)
— whereas the ceiling's `sliceAnon` subtracts it.

**When the charge is `effectiveCurrent`** (`outstanding <= current − file`),
substituting the published ceiling for `maximum`:

```
available = (sliceAnon + MemAvailable - freeMin) - headroom - (current - inactive_file - active_file)
          = MemAvailable - freeMin - headroom - slab_reclaimable
```

which is exactly the owner's intent as an admission rule — *"admit new work until
system MemAvailable would fall to `freeMin`"* — falling out of the existing
composition rather than being asserted.

**When the charge is `outstanding`** — which is the NORMAL case, not the
exception: under AIRA-67 a granted reserve *is* the job's scope `memory.max`, so
`Σ granted` routinely exceeds what those jobs have actually touched — `available`
is smaller by exactly `outstanding − effectiveCurrent`. That is the reservation
ledger doing its job (a reservation is a commitment, not an observation) and it is
**restrictive**, so every bound stated in this document holds in both cases.

**Consequence, stated up front (the AIRA-103 §3.1 obligation, re-run for the new
numbers).** Measured on this box while writing this plan (`/proc/meminfo` and
`aira.slice`'s own `memory.current`/`memory.stat`/`memory.max`):

```
MemTotal          82359904 kB  = 78.54 GiB      memory.max      68719476736 = 64.00 GiB
MemAvailable      46046796 kB  = 43.91 GiB      memory.current  22110003200 = 20.59 GiB
                                                inactive+active_file        =  8.70 GiB
                                                slab_reclaimable            =  0.33 GiB
sliceAnon = 20.59 - 8.70 - 0.33 = 11.57 GiB     affordable = 43.91 + 11.57 = 55.48 GiB
```

| | AIRA-103 (shipped, `off`) | AIRA-106 |
|---|---|---|
| static term | — | `78.54 − 16` = **62.54 GiB** |
| dynamic term | `55.48 − 16` = 39.48 GiB | `55.48 − 8` = **47.48 GiB** |
| published (÷ `min`, quantised down, clamped to 64 GiB) | **≈ 39.25 GiB** | **≈ 47.25 GiB** (basis `system-pressure`) |

The new formula is **~8 GiB more permissive** at this load, which is precisely the
outcome the owner asked for when rejecting "flip to enforce as-is (~38–43 GiB
effective)". The static term does not bind here; it would only bind on a
substantially idler box (`affordable > 70.5 GiB`). These figures are re-measured
at implementation time and recorded in the ticket Resolution.

### 2.3 Where the `min` goes, and why the damping is unaffected

`machineTerm` is derived from `MemTotal`, which is read **once at startup**
(`server.go:391`) and does not change. Therefore, for a window of samples
`t2_1..t2_n`:

```
max_i( min(t1, t2_i) )  ==  min( t1, max_i(t2_i) )
```

and both `sliceCeilingQuantizeDown` and the clamp to `memory.max` are monotone
non-decreasing, so **every** placement of the `min` produces a byte-identical
published figure. The implementation takes the after-the-window form because:

- the window keeps damping **only the pressure signal**, which is the only thing
  that is noisy — a constant needs no hysteresis;
- the window's samples keep their existing meaning, so every existing damping,
  TTL, hold and partial-window test stays valid unchanged;
- **it makes the binding term knowable at publish time in one comparison**, which
  §3 needs.

Because the placement is provably immaterial, **no test asserts the identity** —
a test that cannot fail against any implementation proves nothing. What *is*
tested is the observable damping behaviour in the new machine-bound regime
(§7.1 test 5).

The damping guarantees survive: settled at `machineTerm`, one low pressure sample
cannot pull `max(window)` below `t1`, so lowering still needs the whole window to
agree; one recovering sample restores `max(window)` above `t1` and the published
figure returns to `machineTerm` at once.

### 2.4 Degenerate configuration is refused, never silently clamped

The published ceiling is useless — worse, silently freezing — if the static term
is at or below the admission headroom, because `checkedAvailable` returns 0
whenever `maximum <= headroom` (`admit.go:1695`) and the headroom base is 2 GiB
(`admit.go:49`). So the wiring refuses, logging every number involved, when:

| condition | why |
|---|---|
| `MemTotal` unreadable | existing refusal (`server.go:391-398`), unchanged |
| `reserveMax >= MemTotal - admitSliceHeadroomBase` | `enforce` would pin admission at zero forever |
| `freeMin >= MemTotal - admitSliceHeadroomBase` | the same band, from the other side: the dynamic term could then exceed `sliceAnon` by at most the headroom, so `available` is zero forever |
| `freeMin < watchdogLowMemAvailable` | §2.5 — the throttle's target state would sit inside the watchdog's kill band |
| `reserveMax < 0` or `freeMin < 0` | parse-time rejection (`E_CONFIG_INVALID`) |

`reserveMax = 0` ("AIRA may have the whole machine") and `freeMin = 0` are
*syntactically* legal but `freeMin = 0` is refused by the watchdog-band rule
above; `reserveMax = 0` is accepted and simply makes `machineTerm` non-binding on
any box whose `memory.max` is below `MemTotal`. The subsystem parks with an
explicit log line in every refusal case — the same shape as the existing
`MemTotal unreadable` refusal — because an unusable capacity policy must be
visible, not silently applied.

`validSliceCeilingDeps` changes from `reserve > 0` to
`memTotal > 0 && reserveMax >= 0 && freeMin >= 0 && machineTerm > 0`; the policy
relationships above are checked at wiring, where `MemTotal` is known, and the
deps guard is the last-line structural check.

Deliberately **no** silent small-machine clamp. AIRA-103 used
`min(MemTotal/4, 16 GiB)` to stop `enforce` pinning a small box near zero; that
blend is exactly what the owner replaced, and re-introducing it as a hidden floor
would override an explicitly configured number with a policy nobody asked for.
Sizing for machines below ~32 GiB is an explicit deferral (§9): the defaults are
the owner's own numbers for *this* machine, and an operator on a small box sets
the two environment variables or the subsystem refuses to start.

### 2.5 `freeMin` and the memory watchdog's kill band — the invariant AIRA-103 had

AIRA-103's reserve was `min(MemTotal/4, 16 GiB)`, and §3 of that design made the
coincidence with `watchdogRecoverMemAvailable` (`watchdog.go:24`, 16 GiB) an
explicit invariant: *"the throttle's TARGET state must be one in which the
watchdog is quiescent, or the two subsystems disagree about whether the machine is
healthy."* Replacing the reserve with the owner's `freeMin = 8 GiB` **deletes that
invariant**, and v1 of this plan did so without saying so. Stated plainly:

- `watchdogLowMemAvailable = 8 GiB` (`watchdog.go:23`) is the **trip** threshold —
  the watchdog SIGKILLs an uncapped heavy `claude` descendant below it.
- `watchdogRecoverMemAvailable = 16 GiB` (`watchdog.go:24`) is the **recover**
  threshold. The latch is not one-shot: `criticalRun` resets above the trip
  (`watchdog.go:105`, `:276-278`), so it re-fires on every sustained dip.
- Under `enforce` with the owner's numbers, the throttle's steady state is
  `MemAvailable ≈ freeMin + headroom` = 8 GiB + 2 GiB base + 64 MiB/job ≈
  **10 GiB** — i.e. **2 GiB above the trip**, where AIRA-103 had ~10 GiB.

Two consequences, both handled rather than hidden:

1. **A configured `freeMin` below 8 GiB would place the throttle's target inside
   the kill band.** That is refused at wiring (§2.4), so the two subsystems can
   never be configured into disagreement. The default 8 GiB is accepted exactly
   because it is the boundary, not inside it.
2. **The remaining margin is thin and is made of an unrelated admission
   constant.** The 2 GiB that separates the throttle's steady state from the
   watchdog's trip is `admitSliceHeadroomBaseDefault`, which nobody would connect
   to this knob. (That constant has no environment or config override —
   `NewServer` sets it from the constant and only tests change it — so the
   refusal rule above may use the constant directly and stay deterministic.)
3. **The steady state also sits below the watchdog's *recover* threshold**
   (16 GiB, `watchdog.go:24`). Once the watchdog trips for any reason it will not
   emit `recovered` while AIRA is sitting at its throttled capacity. That gate is
   telemetry-level only (`watchdog.go:105`; the kill decision re-evaluates from
   the trip threshold), so nothing is left latched, but it is a standing
   disagreement between the two subsystems about whether the machine is healthy
   and the owner should see it.

Both are put to the owner in §6 beside the `enforce` decision — *not* changed
unilaterally, because 8 GiB is the owner's own number and this plan's job is to
implement it, not to overrule it.

---

## 3. Honesty: which term bound the ceiling

The formula change breaks a claim three shipped operator surfaces currently make.
Today `State == throttled` means `published < maximum` (`sliceceiling.go:409-411`)
and every surface attributes that to **external memory pressure**
(`main.go:2665`, `confine_queue_position_linux.go:141`). With `machineTerm` in the
`min`, this box's *idle* state is `62.5 GiB` published against a `64 GiB`
`memory.max` — permanently "throttled" **for a reason that is not pressure**. Left
alone, `confine --list` and a blocked launcher's own waiting line would assert a
false cause, which is the class of dishonesty AIRA's own rules forbid.

The fix is **one measured fact**, not a verdict and not a new mechanism: the
snapshot records which term produced the published figure.

```go
const (
    sliceCeilingBasisMachine  = "machine-reserve"  // MemTotal - reserveMax bound it
    sliceCeilingBasisPressure = "system-pressure"  // the dynamic term bound it
)
```

Definition, made total so no state is ambiguous:

- Computed from the **raw, pre-quantisation** values: `machineTerm <=
  max(window)` ⇒ `machine-reserve`, else `system-pressure`. Pre-quantisation
  because two different raw values can quantise to the same figure, which would
  make a post-quantisation comparison report the wrong cause.
- **Ties go to `machine-reserve`** — it is the one an operator can actually change.
- Set **only** when `State == throttled`. When `published == maximum` neither term
  bound the ceiling (the `memory.max` clamp did), and the basis is an honest empty
  string; `unevaluated` and the partial window publish no ceiling at all and
  therefore no basis.
- Carried through the TTL hold beside `lastState`, so a held snapshot neither
  fabricates a basis nor drops one.
- One new wire field `CeilingBasis string \`json:"ceiling_basis,omitempty"\``,
  absent when the subsystem is off — the "off adds nothing to the wire" claim
  AIRA-103 ships on is preserved.

Rendering (wording only; no new numbers on the wire):

| surface | basis | line |
|---|---|---|
| `confine --list` throttled | pressure | unchanged: `…reduced below the 64G configured ceiling by memory used OUTSIDE the slice…` |
| `confine --list` throttled | machine | `…reduced below the 64G configured ceiling to keep part of this machine outside the slice…` |
| `confine --list` throttled | empty/unknown | `…reduced below the 64G configured ceiling…` (no cause claimed) |
| `confine --list` observe, throttled | pressure | `…would be effective by memory used OUTSIDE the slice (observe mode, not applied)` |
| `confine --list` observe, throttled | machine | `…would be effective to keep part of this machine outside the slice (observe mode, not applied)` — the cause clause is **chosen**, never appended, so the line never states two causes |
| `confine --list` observe, throttled | empty/unknown | `…would be effective (observe mode, not applied)` — no cause claimed |
| `confine --list` observe, **unthrottled** | empty | `slice ceiling: 64G configured; not reduced (observe mode, not applied)` — nothing reduced the ceiling, so no cause and no counterfactual figure. (The pre-AIRA-106 line asserted "under system memory pressure" here too, for a ceiling that is not reduced at all.) |
| blocked launcher (`confine_queue_position_linux.go:140`) | pressure | unchanged, incl. the `MemAvailable` figure |
| blocked launcher | machine | `, slice ceiling reduced to keep memory outside the slice for the rest of the machine` — and **no** `MemAvailable` figure, because it is not the cause |
| blocked launcher | empty/unknown | `, slice ceiling reduced below the configured ceiling` — no cause, no figure |

**Alternatives considered and rejected**, recorded so the choice is auditable:

- *Reuse the existing `Reason`/`CeilingReason` string.* Rejected: `Reason` means
  "why this sample could not be established" and is rendered only for
  `unevaluated`/held snapshots; overloading it would make two unrelated facts
  share one field and would change what a held snapshot's `— holding: …` suffix
  says.
- *Drop the cause from all three lines and say only "reduced".* Rejected: the
  attribution is the operationally load-bearing half — "AIRA is capped by policy"
  and "the box is under external pressure" demand different responses.
- *Redefine `State == throttled` to mean "the pressure term bound it", and put
  `min(memory.max, machineTerm)` on the wire as the static ceiling instead*
  (Fable's suggestion). This keeps every existing "by memory used OUTSIDE the
  slice" line true with no basis, no hold-carry and no tie rule, at the same
  one-field wire cost. Rejected on balance because it silently changes what
  `CeilingStaticBytes` means — the number an operator reads as "what I configured"
  would become a derived policy figure — and `throttled` would then no longer mean
  `published < maximum`, which is what both the drain-state line
  (`main.go:2683`) and `sliceCeilingEffectiveMaximum`'s deliberate
  state-independence are written against. Recorded as a genuinely close call.

---

## 4. Configuration surface

Chosen: **environment variables, mirroring `AIRA_DAEMON_SLICE_CEILING_MODE`
exactly**, plus (for the mode only) an `aira install` flag that writes it into the
unit — which is how the watchdog is already configured (`install.go:227`,
`aira-daemon.service.in:9`, root re-exec pass-through `install.go:408`).

| name | default | validation |
|---|---|---|
| `AIRA_DAEMON_SLICE_CEILING_MODE` | `off` (unchanged) | existing |
| `AIRA_DAEMON_SLICE_CEILING_INTERVAL` | `2s` (unchanged) | existing |
| `AIRA_DAEMON_SLICE_CEILING_RESERVE_MAX` | `16GiB` | `runner.ParseMemorySize`; `>= 0`; §2.4 bounds at wiring |
| `AIRA_DAEMON_SLICE_CEILING_FREE_MIN` | `8GiB` | `runner.ParseMemorySize`; `>= 0`; §2.4 + §2.5 bounds at wiring |
| `aira install --slice-ceiling off\|observe\|enforce` | installed value, else `observe` | mirrors `--watchdog` (as fixed by §4.1) |

Both new variables are parsed **only when the subsystem is wanted**, inside the
existing `sliceCeilingConfigFromEnv` (`paths.go:159-172`), preserving its
deliberate divergence from the watchdog: with the mode `off`, a typo in a
slice-ceiling variable must not refuse to start the daemon, because "off is
exactly today's behaviour" is the claim this subsystem ships on.
`TestSliceCeilingOffIgnoresAnInvalidInterval` is extended to cover both.

**Why no `--slice-ceiling-reserve-max` / `--slice-ceiling-free-min` install
flags.** The mode has a rollout ladder an operator walks (`off → observe →
enforce`), which is what earns `--watchdog`'s precedent; the two sizing parameters
have no ladder and correct defaults. Adding two more flags, two more template
placeholders, two more root re-exec pass-throughs and their tests is machinery for
a value nobody on this machine will change. An operator who must change them uses
a systemd drop-in, and the exact recipe goes in the ticket Resolution.

**Why `--memory-max` is not the answer to `reserveMax`.** They are different
questions on purpose. `aira install --memory-max` sets the **kernel-enforced**
static cap on the slice; `reserveMax` is what the **advisory** ceiling refuses to
exceed even when the kernel would allow it. On this box they currently disagree
(64 GiB configured vs a 62.5 GiB static term), and that disagreement is the whole
point — the owner is asking for the second number without moving the first.

### 4.1 A live install bug this ticket must not double

`parseInstallArgs` defaults `opts.watchdog = "observe"` (`install.go:227`) and
`renderDaemonUnit` substitutes it unconditionally (`install.go:894`). **Nothing
reads the installed unit's mode back** — in contrast to `MemoryMax`, which
`computeMemoryLimits` explicitly preserves via `parseInstalledValue`
(`install.go:743`). So **every `aira install` re-run with no `--watchdog` flag
silently rewrites the watchdog's mode to `observe`.**

This is not hypothetical. Measured on this box, now:

```
~/.config/systemd/user/aira-daemon.service:  Environment=AIRA_DAEMON_WATCHDOG_MODE=observe
systemctl --user show aira-daemon.service -p Environment:
    AIRA_DAEMON_MANAGED=1 AIRA_DAEMON_WATCHDOG_MODE=observe
    AIRA_DAEMON_WATCHDOG_INTERVAL=2s XDG_STATE_HOME=/home/mark/.local/state
    AIRA_SCHED_MODE=enforce
```

The only drop-in (`aira-daemon.service.d/sched-mode.conf`) sets `AIRA_SCHED_MODE`
only. The project record says the watchdog was flipped to `enforce` on
2026-08-25 and is the machine's sole live memory killer. **It is running in
`observe` today**, consistent with a later `aira install` (an AIRA-27 or AIRA-33
deploy) having reverted it.

Mirroring `--watchdog` as §4 proposes would inherit this hazard and double it —
and the next unrelated deploy after the owner's §6 `enforce` flip would silently
undo it, contradicting "enforce is one command, no rebuild". So this plan fixes
it rather than reproducing it. **The zero value must survive four hops or the fix
is cosmetic** (the first draft fixed only the last two, and `sudo aira install` —
the path install itself recommends — defeated it):

1. `parseInstallArgs` no longer pre-fills `watchdog`/`watchdogInterval`; an
   option not given stays the zero value.
2. `runInstall` / `runUserInstall` no longer overwrite `""` with `"observe"`
   before the unit is read.
3. `reexecRequestFor` forwards `--watchdog` / `--watchdog-interval` /
   `--slice-ceiling` **only when given**. Previously it appended `--watchdog`
   unconditionally, so the re-exec'd unprivileged install always saw an explicit
   flag and no preservation rule downstream could ever fire.
4. `runUserInstall` retains the **daemon unit's content** (it was read for
   presence and discarded) and `resolveDaemonModes` fills each unset value from
   it via `parseInstalledValue` + `installedWatchdogModeRE` /
   `installedWatchdogIntervalRE` / `installedSliceCeilingModeRE`, falling back to
   `observe` (and 2s) only when no managed unit declares a usable one. An
   installed value that is not a recognised mode is ignored rather than
   propagated or refused, so a hand-edited or newer-vocabulary unit cannot make a
   later install fail.

Exactly the `MemoryMax` precedent, applied to both modes. No new machinery.

Pinned by four tests: absent options parse to the zero value; `resolveDaemonModes`
preserves / is overridden by an explicit flag / falls back / ignores garbage;
`reexecRequestFor` forwards no ungiven mode flag and every given one; and
end-to-end, a flagless re-install after `--watchdog enforce --slice-ceiling
enforce` leaves both modes intact, rewrites nothing and does **not** restart the
daemon.

**The live watchdog mode is NOT changed by this ticket.** Restoring it is a deploy
action, and this session does not deploy or restart services. It is raised to the
owner in the Resolution and filed as its own ticket (allocated with `aira id`,
never hand-picked).

---

## 5. Removing the AIRA-103 reserve derivation

Deleted outright, not deprecated (AIRA has no users and no compatibility
obligation):

- `sliceCeilingReserve(memTotal)` — `min(MemTotal/4, 16 GiB)` (`sliceceiling.go:225`)
- `clampSliceCeilingReserve(memTotal)` (`:627`)
- `deps.reserve` (`:133`) → replaced by `deps.memTotal`, `deps.reserveMax`,
  `deps.freeMin`
- `sliceCeilingReserveMax = 16<<30` as *that* policy's constant; the name is
  retired and two new, separately-documented defaults take its place.
- `TestSliceCeilingReserveFollowsInstallHeadroomPolicy` (`sliceceiling_test.go:425`)
  — the policy it pins no longer exists. **It is replaced, not merely deleted**,
  by a test pinning the invariant that survives: `sliceCeilingFreeMinDefault >=
  watchdogLowMemAvailable` (§2.5), plus §7.1 tests 6 and 8.

`internal/install`'s own `min(total/4, 16 GiB)` slice sizing (`install.go:746`) is
**untouched**: it sizes the static cap and is a separate question (§4).

---

## 6. Rollout: out of `off`

The ticket is explicit that the owner's answer was a formula refinement, not
permission to leave the mechanism dormant, and equally explicit that it goes
"observe first, then enforce, matching the existing mode ladder".

**This ticket ships the `observe` flip and puts the `enforce` flip to the owner.**

- `aira install --slice-ceiling` defaults to `observe` (subject to §4.1's
  preservation rule), so the coordinating session's deploy (`aira install` +
  daemon restart) puts the live daemon into `observe`. Observe applies nothing to
  admission: it samples, publishes, logs, and renders `would be effective` on
  `confine --list`. Capacity is untouched.
- The daemon's own env default stays `off`, so every unit test, every dev daemon
  and every daemon not started from the installed unit is unchanged. This keeps
  AIRA-103's "off is exactly today's behaviour" claim intact and avoids silently
  turning the subsystem on inside the test suite. The two are not competing
  sources of truth: the env var is the daemon's only input, and the unit is the
  only thing that sets it on this machine.
- `enforce` is one command with **no rebuild**: `aira install --slice-ceiling
  enforce` (re-renders the unit, reloads, restarts — `install.go:652-676`).

**The enforce criterion, put to the owner rather than defaulted**, and stated
against evidence that both exists and measures the claim. Two earlier versions did
not: v1 referred to a per-grant log line `admit.go` does not emit (the only
reserve-naming lines are the fairness-freeze transitions at `:1679`/`:1682`, armed
only during a freeze); v2 substituted `MAX(peak_rss)` from `confine_peak_history`,
which is a **per-job** figure being compared against a **slice-aggregate** ceiling,
from a table pruned to the newest 20 rows per signature.

The apples-to-apples pair is *the ceiling that would have been applied* against
*what the slice as a whole was holding at that moment*. Both are in hand at
publish time, so the subsystem's existing rate-limited line carries `sliceAnon`
beside the effective figure — one more field on a line that already prints
`MemAvailable`, not new logging:

```
aira daemon: slice ceiling throttled (observe): 50734301184 effective / 68719476736 configured,
  sliceAnon=12420772120 MemAvailable=47151919104 bound-by=system-pressure
  reserveMax=17179869184 freeMin=8589934592 memTotal=84336541696
```

> Flip to `enforce` once the daemon has run `observe` across at least one full
> heavy-load cycle — concretely ≥24 h of uptime including at least one period of
> ≥8 concurrent confine jobs — and **every logged line shows `effective` above the
> `sliceAnon` logged with it**, with margin:
>
> ```sh
> journalctl --user -u aira-daemon.service | grep 'slice ceiling' \
>   | awk '{ for (i=1;i<=NF;i++) if ($i ~ /^sliceAnon=/) print $6, $i }'
> ```
>
> A line where `effective` approaches `sliceAnon` is one where `enforce` would
> have closed admission with the slice already at that size — the thing the
> rollout is trying to find out before it happens.

**Second question for the owner, from §2.5:** `freeMin = 8 GiB` puts the
throttle's steady state ~2 GiB above the memory watchdog's SIGKILL trip and ~6 GiB
*below* its recover threshold, where AIRA-103's 16 GiB reserve put it exactly at
recover. 8 GiB is the owner's own number and is implemented as given; the owner may
want 16 GiB, or may want the margin stated and accepted. One environment variable,
no rebuild.

**Third, unrelated to this ticket's subject but found by it (§4.1):** the live
`aira-daemon.service` on this box declares `AIRA_DAEMON_WATCHDOG_MODE=observe`
while the project record says the watchdog was flipped to `enforce` on 2026-08-25.
This ticket fixes the mechanism that reverts it but deliberately does **not**
restart or reconfigure the live service. `aira install --watchdog enforce` restores
it; the owner should decide and the coordinating session should apply it.

---

## 7. Tests

TDD, red-before-green, in `internal/daemon/sliceceiling_test.go` and
`internal/daemon/sliceceiling_real_cgroup_linux_test.go`. Every existing AIRA-103
test stays and must stay green — its fixture gains `memTotal`/`reserveMax` in
place of `reserve`.

**The AIRA-103 arithmetic is recovered exactly by `reserveMax = 0` *provided*
`MemTotal >= memory.max`**, since `machineTerm = MemTotal` then never binds below
the clamp. The unit fixture therefore pins `memTotal = 78 GiB` against its
`maximum = 64 GiB` (v1 left this unstated, and a fixture `memTotal` of 48 GiB —
the real-cgroup test's `simulatedTotal` — would have turned
`TestSliceCeilingNeverExceedsConfiguredMaximum` and
`…HoldPreservesAnUnthrottledState` red). The equivalence is itself asserted, so
the refactor cannot silently change the dynamic term.

### 7.1 New unit tests

1. **`TestSliceCeilingTakesTheMinimumOfBothTerms`** — table over five regimes:
   machine-bound, pressure-bound, exactly equal, both above `memory.max`, and a
   **sub-quantum difference** (machine `38G+200M` vs pressure `38G+100M`, both
   quantising to `38G`). Each asserts the published ceiling **and** `Basis`.
   *RED against a single-term formula in either direction*, and the last row is
   the only one *RED against a `Basis` decided after quantisation* — verified by
   running both wrong implementations, which fail exactly those rows.
2. **`TestSliceCeilingMachineTermIsIndependentOfPressure`** — with `machineTerm`
   binding, moving `MemAvailable` up and down by 8 GiB does not move the published
   ceiling. *RED against an implementation that folds `reserveMax` into the
   dynamic term (e.g. `affordable − reserveMax − freeMin`).*
3. **`TestSliceCeilingPressureTermMatchesAira103WithFreeMin`** — for a matrix of
   inputs with `reserveMax = 0` and `memTotal >= maximum`, the new evaluation
   equals the AIRA-103 arithmetic `affordable − freeMin`. *RED against any
   accidental change to the reused term.*
4. **`TestSliceCeilingBasisIsEmptyWhenUnthrottled`** and
   **`TestSliceCeilingHoldPreservesBasis`** — §3's totality contract: never a
   fabricated cause, never a dropped one, nothing on an unevaluated or
   partial-window snapshot.
5. **`TestSliceCeilingDampingAsymmetryUnderTheMachineTerm`** — extends the
   existing `…DampingAsymmetryAndPartialWindow` into the new regime: settled at
   `machineTerm`; one pressure sample below it ⇒ published unchanged; three ⇒
   published drops to the pressure figure and `Basis` flips to `system-pressure`;
   one recovering sample ⇒ back to `machineTerm` at once with `Basis` back to
   `machine-reserve`. *RED against an implementation with no static term at all
   (the settled ceiling would be the 42 GiB pressure figure) and against a `Basis`
   that does not follow the crossover.* It is deliberately **not** claimed to be
   red against applying the `min` per-sample: §2.3 proves that byte-identical, and
   "RED against a machine term that damps" would be vacuous because a constant
   cannot damp. (v1 proposed instead a test of the `min`-placement identity
   itself; that could not have been red against anything and is dropped.)
6. **`TestSliceCeilingDefaultsAreSixteenAndEightGiB`** — the owner's numbers,
   pinned as constants, **and** `sliceCeilingFreeMinDefault >=
   watchdogLowMemAvailable` (§2.5, replacing the deleted AIRA-103 policy test).
7. **`TestSliceCeilingSizingEnvParsing`** — `16G`/`8GiB`/`0`/bare bytes accepted;
   garbage and negatives rejected with `E_CONFIG_INVALID`; and neither is parsed
   at all when the mode is `off`.
8. **`TestSliceCeilingRefusesADegenerateSizing`** — each §2.4 row: `reserveMax >=
   MemTotal − admitSliceHeadroomBase`, `freeMin >= MemTotal`, `freeMin <
   watchdogLowMemAvailable`. Each ⇒ the subsystem does not start and says which
   number is at fault. *RED against a guard that only checks `reserveMax >=
   MemTotal`, which leaves the 2 GiB band that freezes admission silently.*
9. **`TestConfineListReportsTheCeilingBasis`** (extends the existing
   `TestConfineListReportsTheCeilingFromTheDaemon`, `sliceceiling_test.go:854`) —
   the rendered wordings of §3's table, that the observe line states exactly one
   cause, and that a machine-reserve basis does **not** print a `MemAvailable`
   cause on the blocked-launcher line.

Unchanged-and-must-stay-green, listed because they are the porosity guards:
`…InvariantToSliceOwnGrowth`, `…TracksExternalPressure`, `…TornReadIsNeverPermissive`,
`…DampingAsymmetryAndPartialWindow`, `…HoldsThenExpires`,
`…ThrottleReachesCapacityOnly`, `…DoesNotReachTheOOMEscalationClamp`,
`…IsKeyedByCanonicalSlicePath`, `…SnapshotIsALeafUnderConcurrentEvaluation`,
`…RaiseStillPublishesAfterTheGovernorWakeIsGone`,
`…DoesNotAuthoriseANewlyRaisedMaximum`.

Plus `internal/install/daemon_service_test.go`: §4.1's preservation rule for both
`--watchdog` and `--slice-ceiling`, and the new `@SLICE_CEILING_MODE@`
substitution.

### 7.2 Real-cgroup safety bound — the ticket's explicit requirement

The brief requires "a real-cgroup test proving the new formula never shrinks below
current real usage, with a negative control proving the test fixture can actually
detect a failure". The naive phrasing of that bound is **false**, and both
reviewers caught it independently, so it is stated precisely:

> **Claim.** Against a real cgroup holding a real running process, with
> `MemAvailable − freeMin >= quantum` **and `machineTerm >= sliceAnon`**, the
> published ceiling is **at least the slice's own real `sliceAnon`** — so
> admission never closes merely because the slice is holding what it already
> holds. The `quantum` term is load-bearing:
> `published = quantise_down(min(machineTerm, sliceAnon + (MemAvailable − freeMin)))`,
> so for `0 <= MemAvailable − freeMin < quantum` the round-down alone puts
> `published` **below** `sliceAnon` and the naive bound is not provable. Weakened
> form, valid for all `MemAvailable >= freeMin`: `published >= sliceAnon − quantum`.
>
> **Corollary, one-directional.** `checkedAvailable` charges
> `current − inactive_file − active_file = sliceAnon + slab_reclaimable`, so
> `MemAvailable − freeMin > slab_reclaimable + quantum + headroom` **suffices**
> for `available > 0`. (It is not claimed to be necessary.)

**`TestSliceCeilingRealCgroupNeverShrinksBelowRealUsage`** — reuses
`newCeilingCgroupFixture` (isolated `os.MkdirTemp` cgroup, never `aira.slice`),
with `MemAvailable` modelled as `max(0, simulatedTotal − simulatedOutside −
sliceAnon)` exactly as the existing signal test does. **`sliceAnon`, not
`memory.current`**: `si_mem_available` counts reclaimable file pages as available
wherever they live, so modelling against `memory.current` would treat the slice's
own page cache as consumed memory — the exact mistake this signal exists to avoid.
Modelled rather than measured because this box carries dozens of live confine
jobs, so a real machine-level reading moves by gigabytes between samples. The
bound is therefore proved **under the modelled `MemAvailable` against the kernel's
real per-cgroup accounting** — the same honest scoping the existing signal test
already states.

1. Baseline; then `fixture.grow(t, "anon")` and `fixture.grow(t, "file")` so the
   real cgroup holds real anonymous **and** real page-cache pages.
2. At every step, with `simulatedOutside` held so `MemAvailable − freeMin >= 1
   GiB` (explicitly, not incidentally): read the fixture's real `slab_reclaimable`
   and require it below the margin, then assert
   `published >= sliceCeilingAnon(real current, real reclaimable)` **and**
   `checkedAvailable(realCurrent, published, realFileReclaimable, 0, 0) > 0`.
3. Assert `memory.max` byte-identical, `oom_kill` unchanged, and the helper alive
   throughout — AIRA-103's safety clauses, re-run under the new formula.
4. Assert the `machineTerm` half too: with `reserveMax` set so
   `MemTotal − reserveMax` sits **below** the fixture's real `sliceAnon`, the
   published ceiling *does* drop below real usage and `available` goes to 0 —
   which is correct (the slice is already over its policy share) and, critically,
   *still* leaves `memory.max` and `oom_kill` untouched. This is the case where
   "never shrinks below real usage" is deliberately **not** an invariant, and
   saying so in the test is the difference between a proof and a slogan.

**`TestSliceCeilingRealCgroupUsageBoundHarnessDetectsAViolation`** — the
**negative control for this specific claim**, distinct from AIRA-103's existing
`…HarnessDetectsALimitWrite` (which validates the no-write actuator, not the
bound). It drives the *same* fixture through the *same* `ceilingBoundProbe` helper
— sharing the probe is what makes it a control rather than a parallel test — with
`MemAvailable` pushed **below `freeMin`**, and asserts the probe **does** report
`published < sliceAnon` and `available == 0` on the PUBLISHED snapshot (never on a
raw `desired`, which would gut the control). The violation is sustained for more
than a full damping window on purpose: the published figure is the max over
`sliceCeilingSamples`, so a single low sample would leave the previous, higher
ceiling standing and the control would report a pass it had not earned. Without
this control, "the ceiling stayed above real usage" could be true of a fixture
that could never have observed the opposite.

**Red-check performed, not assumed.** Both real-cgroup additions were run against
two deliberately wrong implementations: with the static term deleted, the machine
half fails (`basis="system-pressure"`, want `machine-reserve`); with
`sliceCeilingAnon` replaced by raw `memory.current`, the signal test fails on the
slice's own anon growth. Recorded in the ticket Resolution.

All real-cgroup tests skip on a host without cgroup-v2 delegation and hard-fail
under `AIRA_REAL_CGROUP=1`, per `cgrouptest.SkipOrFailRealCgroup`.

### 7.3 Whole suite

`aira confine -- go build ./...`, `aira confine -- go vet ./...`,
`aira confine -- go test ./...`, then `AIRA_REAL_CGROUP=1 aira confine -- go test
./internal/daemon/ -run SliceCeilingReal`. One at a time, exit codes recorded
exactly, never inferred from truncated output.

---

## 8. Files

Modified:
- `internal/daemon/sliceceiling.go` — `sliceCeilingPolicy{memTotal, reserveMax,
  freeMin}`; `sliceCeilingDesired` becomes the two-term `min`; `Basis` on the
  snapshot and in the hold; `deps.reserve` → the policy; `realSliceCeilingDeps`
  and `validSliceCeilingDeps` updated; `sliceCeilingReserve` /
  `clampSliceCeilingReserve` deleted; `logSliceCeiling` names both parameters and
  the basis.
- `internal/daemon/paths.go` — `sliceCeilingReserveMaxFromEnv`,
  `sliceCeilingFreeMinFromEnv`, folded into `sliceCeilingConfigFromEnv`.
- `internal/daemon/server.go` — wiring: build the policy, refuse degenerate
  configuration (§2.4, §2.5).
- `internal/daemon/confine_manage.go` — `CeilingBasis` on the reply.
- `internal/runner/confine_manage.go` — the new wire field.
- `internal/runner/confine_queue_position_linux.go` — basis-aware wording.
- `cmd/aira/main.go` — basis-aware `confine --list` wording.
- `internal/install/install.go` + `internal/install/assets/aira-daemon.service.in`
  — `--slice-ceiling`, `@SLICE_CEILING_MODE@`, root re-exec pass-through
  (`:408`), `--status` mutation-option guard, and §4.1's installed-mode
  preservation for **both** mode flags.
- tests: `internal/daemon/sliceceiling_test.go`,
  `internal/daemon/sliceceiling_real_cgroup_linux_test.go`,
  `internal/install/daemon_service_test.go`.
- `docs/superpowers/specs/2026-09-05-aira103-dynamic-slice-ceiling-design.md` — a
  header note that §3's reserve derivation is superseded by this document.
- `.aira/tickets/AIRA-106.md`, `.aira/tickets/AIRA-103.md`.

New: this document.

**No governor reference is introduced anywhere** (AIRA-33).

---

## 9. Explicit deferrals and accepted gaps

- **Kernel enforcement of the reduced ceiling.** Still deferred, on AIRA-103's
  Findings A–C. Not revisited here.
- **Small-machine sizing (< ~32 GiB).** The defaults are the owner's numbers for
  this machine; a smaller box configures the two variables or the subsystem
  refuses to start (§2.4). No hidden clamp.
- **Install flags for `reserveMax`/`freeMin`** (§4) — env vars and a drop-in only.
- **Warm-up and TTL expiry drop the static term too**, though it needs no sample:
  a partial window and an expired hold publish `unevaluated`, so
  `admitEffectiveMaximum` hands back the raw `memory.max` for ~3 intervals at
  startup and after every hold expiry. On this box that is 64 GiB instead of
  62.5 GiB for ~6 s — small, and in the permissive direction. Accepted rather than
  fixed: a "static-only" published state would be new machinery for 1.5 GiB.
- **A reserve inside `(machineTerm − headroom, memory.max − headroom]` waits
  forever.** Both `E_ADMIT_TOO_LARGE` checks correctly use the static maximum
  (AIRA-103 Finding A), so such a request is accepted, can never fit
  `checkedAvailable` under `enforce`, and eventually times out as
  `E_ADMIT_SATURATED` "slice contended" — permanent by policy, reported as
  transient contention. The band is 1.5 GiB on this box and no real job requests
  a 63 GiB reserve. Terminality is deliberately left untouched (Finding A,
  simplicity); the `Basis = machine-reserve` launcher line is what makes the cause
  visible if it ever happens.
- **`MemTotal` is read once at startup** and never re-read. On a host that hotplugs
  or balloons memory the static term would go stale. Not modelled; recorded.
- **Swap** remains the largest residual (~6.5 GiB live, essentially all outside
  the slice), in the **permissive** direction, unchanged from AIRA-103 §9.
- **`slab_reclaimable`'s restrictive residual** (0.36 GiB) between the ceiling's
  `sliceAnon` and admission's AIRA-21 discount — §2.2, recorded, not closed.
- **The `enforce` flip itself, and the `freeMin`-vs-watchdog-band margin** — §6,
  both put to the owner.
- **Restoring the live watchdog mode** that §4.1 shows was silently reverted — a
  deploy action for the coordinating session, filed as its own ticket.

## 10. Risks

| risk | mitigation |
|---|---|
| `currentSliceUsage` read as raw `memory.current`, re-opening Finding B | §2.1; the existing page-cache invariance test is RED against it and stays valid at `reserveMax = 0` |
| the new static term makes an idle box permanently "throttled" and three operator lines assert a false cause | §3 basis + reworded lines + test 9 |
| the throttle's steady state sits in the watchdog's kill band | §2.5; refused below 8 GiB; margin stated and put to the owner |
| a degenerate `reserveMax` silently freezes admission (incl. the 2 GiB headroom band) | §2.4 refuse-and-log + test 8 |
| the refactor from one `reserve` to two parameters silently changes the dynamic term | test 3 pins equivalence at `reserveMax = 0`, `memTotal >= maximum` |
| the real-cgroup bound is stated in a form that is false at the quantisation boundary | §7.2 states it with the `quantum` and `slab` terms and holds a 1 GiB margin in the fixture |
| `aira install` silently reverts a mode on the next deploy | §4.1 preserves the installed value for both flags, pinned by test |
| `observe` default in the installed unit surprises the owner | observe applies nothing to admission; enforce is a separate, flagged decision (§6) |
| a sibling ticket lands in `sliceceiling.go`/`admit.go` first | re-check `origin/master` and rebase immediately before merge |
