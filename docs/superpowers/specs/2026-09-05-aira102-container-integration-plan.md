# AIRA-102 — `aira confine` container integration (podman transparent nesting + docker sanity shim)

**Status:** plan **v4 — BUILT, build-reviewed, amended.** v3 was gate-passed and implemented; Sol's adversarial BUILD review returned **BLOCK** (3×P1, 2×P2) and every finding is folded in below (§3.2, §3.5, §3.10, §3.11 and §8).

**Status history:** plan **v3 — GATE-PASSED (with changes).** v1 was GATE-FAILed by both lineages (Sol 4×P0; Fable 4×MUST-FIX). v2 cleared every MUST-FIX; Fable's v2 re-gate returned **GATE-PASS-WITH-CHANGES** with 12 text/precision items, all folded in here. DeepSeek was **unavailable** (`agentmux ask` exit 4, twice) — recorded, not a blocking gate.
**Ticket:** AIRA-102 (P1). Owner-directed two-part remedy.
**Branch:** `aira102-container-integration`, off `origin/master` @ **`ea82cb8`** (rebased mid-plan onto AIRA-101 `--exclusive`, PR #46). **All line citations below are post-rebase.**
**Author:** Opus, grounded on **live empirical probes** (podman 4.9.3 rootless, cgroup v2, systemd cgroup manager, real `aira.slice`) — every §2 claim was measured.

**What v2 changed.** Both lineages independently found the same four load-bearing defects in v1: the `max()` ledger raise was **wrong** in three distinct ways and is replaced by a per-runtime rule plus an explicit specification table (§3.5); the argv scan could **act wrongly** rather than decline and gains a boundary proof (§3.2); `podman:nested` **asserted an outcome nothing verifies** and is renamed to the action performed (§3.7); §3.10 swapped a false claim for **silence** and now names the descendant OOM (§3.10). Fable additionally found that a **running** split job reads `POPULATED 0` and is skipped by AIRA-74's reserve reconstruction (§3.12), and that `-d`/`--detach` is a silent trap (§3.11).

---

## 1. Problem

`aira confine -- docker run ...` confines only the docker **CLI client**; the container is spawned by the system-managed `dockerd` and never enters `aira.slice`. Rootless `podman run` **can** be contained, but only if the caller hands it cgroup flags by hand. Two gaps: podman containment requires caller knowledge AIRA should supply itself, and a `docker run` under confine looks confined and is not.

---

## 2. Empirical findings (measured live, 2026-09-05)

Probes ran under real `aira confine` against the real `aira.slice`; podman had **zero** containers before and after; the machine's 33 **docker** containers were never touched.

| # | Finding |
|---|---|
| **F1** | `podman run --cgroups=split` places the container at `<confine-scope>/[runtime/]*libpod-payload-<id>` — a genuine cgroup **descendant of THIS confine job's own scope**, charged to that job's `memory.max`. **Per-job nesting works.** |
| **F2** | Split relocates the job's **own** processes into `<cur>/runtime`, one level deeper per `podman run` (measured `/runtime`, `/runtime/runtime`, `/runtime/runtime/runtime` after 3 runs). Confine then reports `scope-integrity=migrated`. |
| **F3** | `--cgroups=split` is **mutually exclusive** with `--cgroup-parent` (`cannot specify --cgroup-mode=split with a cgroup-parent`). |
| **F4** | Split leaves a `runtime` child cgroup; `Scope.Remove()`'s `os.Remove` then fails `ENOTEMPTY`, so the scope dir lingers until AIRA-36's recursive reaper sweeps it (supervisor-dead + grace + no lease). Measured. |
| **F5** | Scope teardown **does** kill the nested container (`cgroup.kill` is recursive) — the container pid was gone after exit. Podman's state DB is left stale (`podman ps` shows `Up` for the dead container). |
| **F6** | The fallback `--cgroup-parent=aira.slice` works but the container is a **sibling** under the slice, bound only by the 64G aggregate cap, and **survives** the job's exit. |
| **F7** | **podman, measured *under* `--cgroups=split`** (v2 wrongly hedged that it was not): `--memory=<bare bytes>` accepted, units case-insensitive and binary; `--memory=1048576` (1 MiB) **fails** (`container init was OOM-killed`), 4 MiB and 6 MiB succeed; and with `-m 64m` the **payload cgroup's own `memory.max` read back `67108864`** on the host side — so `--memory` demonstrably lands on the container's cgroup in split mode. |
| **F12** | **docker, measured 2026-09-05** (`docker create` only — never started, unique names, all removed; the machine's 33 real containers untouched): `--memory=67108864` (bare bytes) **ACCEPTED**, as are `64m`/`64M`/`4G`; `--memory=1048576` (1 MiB) **REJECTED**. So the bare-byte injection format is confirmed on **both** runtimes rather than assumed on the docker one, and **6 MiB is the correct common floor** — podman tolerates 4 MiB, docker does not (its documented minimum is 6 MB). |
| **F8** | `-m 8g` inside a 2G confine scope is **accepted** and runs; the kernel ancestor cap binds. A container limit above the job limit is **not** a contradiction. |
| **F9** | `--cgroup-manager=cgroupfs --cgroup-parent=<scope path>` **fails** (OCI runtime error) from a scope still holding processes (no-internal-process rule blocks enabling `memory` in `subtree_control`). Not an alternative to split; also mutates `subtree_control` as a side effect. |
| **F10** | Pre-existing honesty defect: `confine_linux.go:993` derives `oom` from the **hierarchical** `memory.events` `oom_kill`, so a descendant's own-cap OOM prints `confine: job OOM-killed at its memory cap 2G` on a job that exited 0. Measured. |
| **F11** | (Fable 4, code-grounded) After split every process lives in `<scope>/runtime` or the payload, so `confine --list`'s leaf `cgroup.procs` count (`confine_manage_linux.go:107-110 → rendered at cmd/aira/main.go:2596-2600`) reads **`POPULATED 0` for a RUNNING job**, and the daemon's post-restart reserve reconstruction (`admit.go:1488-1496`, explicitly a "v2 item") **skips leaf-empty scopes** — reopening AIRA-74's over-admission window for the job's lifetime. `--kill` is safe (`confine_manage_linux.go:452-468` is subtree-aware); the reaper is safe (supervisor alive). |

**F1 answers the ticket's open question: per-job nesting IS achievable**, via `--cgroups=split` rather than `--cgroup-parent`; F9 closes the only alternative.

---

## 3. Design

### 3.1 Detection — deliberately narrow (portable, pure)

New portable file `internal/runner/container.go`. Detection fires **only** when `base(argv[0])` is **exactly** `docker` or `podman` **and** `argv[1]` is exactly `run`.

**Explicitly undetected, and therefore un-warned** (stated in code doc, SKILL text and docs — never assumed wider): `compose`/`podman-compose`/`docker compose`, `exec`, `play`, `pod`, `build`; `docker container run` / `podman container run`; **any global-flag form** (`docker --context X run`, `podman --remote run`, `podman --cgroup-manager=cgroupfs run`) — these get no injection **and no docker warning**; `podman-remote`; `sudo podman run`; and **any** shell-wrapped invocation (`sh -c "docker run …"`). Recorded as **L5**. A `podman-docker` shim installed as `/usr/bin/docker` is classified as **docker** — conservative (we never claim containment), and worth one sentence.

### 3.2 Memory-flag scan — conservative, boundary-proved, ambiguity-honest

One left-to-right pass over `argv[2:]`, answering **two** questions at different strictness.

**`Present`** (over-inclusive, safe direction) — is any memory-limit-shaped token anywhere after `run`? Set by `--memory`, `--memory=…`, `-m`, `-m<x>`, or an ambiguous single-dash cluster (below). `--memory-swap`, `--memory-reservation`, `--memory-swappiness` are **excluded** (both forms). Over-detection only ever makes AIRA **decline to inject**.

**`Established` + `Bytes`** (strict) — the value is known **only** when *all* hold:
1. exactly **one** occurrence;
2. the value parses via the shared `parseMemorySize`;
3. no ambiguity marker;
4. the **option-region boundary proof** holds (below);
5. the value is **> 0** — `-m 0` means *unlimited* to docker and must never become `caller=0`.

Otherwise `Established=false` with a stated, **token-naming** reason (`unevaluated:ambiguous-token=-itm`), reported as `unevaluated`. **Never guessed.**

**Boundary proof (new in v2 — closes Sol P0-3 / Fable MF1).** Both reviewers produced argv where the v1 rule *acted wrongly* rather than declining: `docker run --rm qemu-image qemu-system-x86_64 -m 4G` (qemu's own RAM flag) and `docker run alpine echo --memory=8g` would `Establish` a limit that does not exist, raise the ledger for a phantom, print `caller=4G` as a fact, and fire §3.6's loud line on a meaningless comparison. So: **stop establishing at the first token that is neither `-`-prefixed nor immediately preceded by a `-`-prefixed token** — the earliest position at which the image can appear. A memory-shaped token at or after that point is `Present` + `unevaluated:after-image-candidate`.

**End-of-options marker (build review, Sol P1).** A standalone `--` is pflag's definitive end-of-options marker, so the token after it is unambiguously the image and everything beyond is the container's command. Without honouring it, `docker run -- alpine -m 8g` **established** the container's own `-m` and charged the ledger 8 GiB — a wrong reservation, not a decline.

**Placement flags are boundary-gated too (build review, Sol P1).** `podman run alpine echo --cgroup-parent=x` is the container's argument; treating it as the caller's placement choice silently withheld containment from a job that never asked for it. A genuine placement flag always precedes the image, so gating cannot miss a real one.

*Residual, stated (L9):* `docker run --rm alpine -m 4g` still establishes, because `alpine` follows the `-`-prefixed `--rm` and is indistinguishable from that flag's value without a full arity table (explicitly out of scope).

**Short-flag classification.** For a single-dash token, by its second character:
- a known **value-taking** short (`e v p w u l h a c`) → that flag with an attached value (`-v/home/mark:/x`, `-eTERM=xterm`); sets **neither** `Present` nor `Established`.
- a cluster of known **boolean** shorts containing `m` (e.g. `-itm`) → **ambiguous**: `Present=true`, `Established=false`, reason names the token.
- **anything else** (second character in neither list — the fallback Fable note 9 required be defined): **ambiguous (`Present`) if the token contains `m`, otherwise ignored.** Unknown never silently means "no memory flag here".

**Cgroup-flag matching is exact-token, never prefix (Fable note 12):** `--cgroups`, `--cgroups=`, `--cgroup-parent`, `--cgroup-parent=` only. A prefix match would also catch `--cgroupns` and podman's `--cgroup-conf` and withhold split for no reason.

The same pass also records `--pod` / `--pod-id-file` (treated **as** a caller cgroup flag — pod cgroup-parent versus split was not measured, so AIRA injects nothing and reports) and `-d` / `--detach` (§3.11).

### 3.3 Part 1 — podman

At launch construction (immediately before `confineSetupArgv`, after `scopeMemoryMax` resolves — the only point at which the daemon-estimate cap of `:853` is known), build a **fresh** argv slice (never `append(argv[:2], …)` onto the caller's backing array) inserting at **index 2** (verified safe for both runtimes: pflag, flag position directly after the subcommand):

- **Placement.** Caller set none of `--cgroups` / `--cgroup-parent` / `--pod` → inject **`--cgroups=split`**. Otherwise inject **nothing** (F3) and report their choice without claiming containment.
- **Memory.** Confine has resolved `scopeMemoryMax > 0`, scan says `Present=false`, and `scopeMemoryMax >= 6 MiB` (F7 floor) → inject `--memory=<scopeMemoryMax bare bytes>`. Below the floor, inject nothing and say why.

The `--cgroup-parent=aira.slice` fallback is deliberately **not** implemented: nesting works and the fallback is strictly weaker (F6).

Under `--delegate-ram`, `scopeMemoryMax` is always resolved (`:792-800`), so the injected `--memory` is the delegate ceiling. Consistent with the ancestor bound.

### 3.4 Part 2 — docker

No placement flag exists. Docker gets the same memory injection rule, the §3.5 ledger raise, and an **unconditional** warning line before the child starts, for **every** detected `docker run` regardless of what was injected or reserved:

> `confine: docker run detected — the container runs under the system docker daemon, OUTSIDE aira.slice. AIRA cannot contain it: nothing here bounds it against the slice cap, and this remains true whatever confine injected or reserved. Use 'podman run' for kernel-enforced containment.`

This is the requirement that the shim must not become the trap it counters.

### 3.5 Ledger charge — per-runtime rule and the specification table (v2 rewrite)

**v1's `reserve = max(reserve, containerBytes); pinned = true` was wrong in three ways**, found independently by both lineages against `confine_linux.go:489-503`, `:853` and `internal/daemon/admit.go:922`:
(a) *podman over-book* — `--memory-max 2G` + `podman run -m 8G` charges the shared ledger 8G while the kernel binds the nested container at 2G (F8): 6 GiB of admission charged for memory the job cannot use, the AIRA-62 shape;
(b) *not a max at all on the unpinned path* — `reserve` at `:495` is the 4G client **hint**, not the daemon's grant; pinning makes the daemon return `pinned:client` **verbatim** (`admit.go:922`), *replacing* a possibly larger history estimate;
(c) *undisclosed scope-cap leak* — with `declaredReserve=false` and a daemon grant, `:853` sets `scopeMemoryMax = admission.reserve`, so the container's number silently becomes the **job's** cap and prints as `scope-memory.max=enforced=X`.

The rule is now split by runtime:

**Podman — never raise the ledger *when the container actually nests*.** The skip is keyed on **nesting**, not on the runtime (build review, Sol P1): if the caller supplied their own `--cgroup-parent`/`--cgroups`/`--pod`, AIRA injects no placement flag and the container goes wherever they sent it — `podman run --cgroup-parent=aira.slice` makes it a **sibling** of this job's scope, outside the job's reservation. Such a container is charged exactly like a docker escapee, and AIRA does not claim the kernel binds it. `ContainerPlan.NestsInJobScope()` is the single predicate; it also gates the "the kernel binds the container at Y" advisory and the detached-container warning.

**When it does nest:** The container is a cgroup descendant of the job's own scope (F1), so its memory is **already** inside whatever the job reserved. There is only ever *one* charge per job, so the defect is **not** double-booking (Fable note 5): a raise would either **over-book past a binding cap** — charging for memory the kernel forbids the job to use — or, on the unpinned path, **replace** the daemon's history estimate with a client-pinned number. Neither is wanted, so AIRA reports the caller's limit and acts on it not at all. Fable confirmed the unpinned-uncapped case does **not** need the raise: on the daemon path cap == charge and stays consistent; on the fallback path it is the same guess-charged uncapped shape every job gets today.

> *Stated consequence:* on an unpinned, uncapped podman job the daemon's history estimate becomes the scope cap (`:853`), and pre-feature history for that signature recorded **CLI-only** peaks — so the first nested run can be OOM-killed at that cap. It self-heals (the `oom` flag bumps `EstimateMemoryReserve`, `resource_estimate.go:56`, to headroom), and §3.10's corrected advisory now names the cause and recommends `--memory-max`. Documented, not silent.

**Docker — raise only when the request is not already pinned.** If the caller declared confine limits, their number is authoritative and AIRA reports the disagreement (§3.6) rather than overriding it. If unpinned and the container limit is `Established`, set `reserve = max(hint, containerBytes)` and pin.

> *Stated consequences:* pinning **replaces** the daemon's history estimate for this job (basis `pinned:client`) — acceptable here because a `docker run` signature's history is CLI-only (tiny), so the raise essentially always exceeds it. The pinned value also becomes the **docker CLI's** scope `memory.max` via `:853`, printing `scope-memory.max=enforced=<container bytes>` for a client that will never use it — harmless, but stated because it reads as though confine capped something meaningful. And the charge reserves slice budget for memory that lives **outside** the slice: the deliberate trade the ticket asked for (machine-level headroom over slice utilisation), named as such.

**`!DelegateRAM` guard** on the docker raise preserves AIRA-62.

**No second daemon lease.** Folding into the job's own single admission avoids a second blocking path and deadlock surface.

**Resulting (ledger charge, scope cap) — this table is the specification:**

| runtime | caller-declared confine limit? | admission path | ledger charge | scope `memory.max` |
|---|---|---|---|---|
| podman **nested**, caller placed nothing | yes (`--memory-max`/`--memory-reserve`) | any | declared (unchanged) | declared (unchanged) |
| podman **nested** | no | daemon grant | daemon estimate (unchanged) | daemon estimate (unchanged) |
| podman **nested** | no | fallback / timeout / unevaluated | flock free-memory check against the client hint; **no ledger charge** | uncapped (unchanged) |
| podman **caller-placed** (not nested) | either | any | **treated as docker** — the container is not inside this job's reservation | as the docker rows |
| docker | yes | any | declared (unchanged) | declared (unchanged) |
| docker | no, container limit `Established` | daemon grant | `max(hint, containerBytes)`, pinned | same value (via `:853`) |
| docker | no, container limit `Established` | fallback / timeout / unevaluated | flock free-memory check against the **raised** number; **no ledger charge** — the raise gates that check and can delay the launch up to the admission timeout, then launch **uncapped** | uncapped |
| either | container limit not `Established` | any | unchanged | unchanged |

**Docker raise when the container limit is *below* the hint** (Fable note 6): the charge becomes the 4 GiB hint, pinned — an over-book of `(hint − containerBytes)` relative to the container. Accepted deliberately, so the `:853` cap leak can never cap the docker CLI's own scope below 4 GiB. Stated, not silent.

**Facet honesty (Fable SHOULD-FIX 1).** `:reserved` is keyed on the **daemon-grant predicate `:853` already uses** — admitted **and** `admission.lock == nil && admission.release != nil` — **never** on `admission.state`. The flock fallback returns state `immediate`/`waited` *with* a lock and basis `fallback:daemon-unavailable` (`admission_linux.go:241-288`); it is a slice **free-memory check**, not a ledger charge, so reporting `:reserved` from the state would claim a charge that never happened. Everything else is `:reserve-requested`. Table rows 3 and 6 are therefore worded as a free-memory check, and row 6 additionally notes that the **raised** docker number gates that check and can delay the launch up to the admission timeout before launching **uncapped**.

### 3.6 Disagreement policy — the owner's explicit open question

**Decision: never refuse the launch. Report both numbers, and warn loudly in *both* false-belief directions.** Fable reviewed this directly and found it sound and not a rationalisation, *conditional* on the reported numbers being true (§3.2, §3.5) and on symmetry of loudness.

1. **Fail-closed governs *claims*, not the user's command.** The rule is that a check which cannot establish its result reports `unevaluated` rather than a fake pass. A disagreement is not an unestablished check: both numbers are exact and printable.
2. **For podman the two limits are not in conflict.** The job cap is a kernel-enforced *ancestor* bound and the container limit a nested sub-limit; the smaller binds automatically (F8, measured). Refusing would refuse a correct configuration.
3. **For docker they are not two answers to one question.** Confine's cap governs the CLI client's cgroup in `aira.slice`; docker's `-m` governs a container cgroup in the system tree.
4. **Precedent in this code path.** `ResolveConfineReserve` already *resolves* a reserve-versus-cap mismatch (`confine.go:91-94`) rather than refusing. Confine refuses only when a value is **unusable** or containment would be **silently** lost — neither applies, and nothing here is silent.
5. **Consistency.** The plan launches when only one side specifies a limit; the both-sides case is strictly *more* informed, so refusing only there would be arbitrary.

**Sol's dissent, and why it is not adopted.** Sol argued (P0) that for docker, when the container's limit *exceeds* confine's, refusing prevents the escaped container from existing, so v1's "zero safety benefit" claim is false. That is a fair hit, and v1's fourth bullet is **deleted** accordingly. Refusal is still not adopted: confine's own limit is usually **not caller-declared** — it is an AIRA-resolved p90 estimate or the 4 GiB no-history default. Refusing an explicit `docker run -m 8g` because an *estimate* came out at 1.4G would refuse on a number the caller never chose, and would fire constantly. The genuine "caller declared two conflicting numbers" case is narrow; there AIRA reports both loudly and lets the caller decide. Adopting the rule would also make AIRA assert authority over a container it has just finished stating it cannot contain.

**Both loud lines** (v1 had only the first):
- docker, container limit > confine's: `confine: docker --memory=<X> exceeds this job's own limit <Y>; the container is NOT bound by <Y>, nor by aira.slice.`
- podman, container limit > the job cap: `confine: container --memory=<X> exceeds this job's cap <Y> (<basis>); the kernel binds the container at <Y>.`
  The `<basis>` is required (Fable note 7): `Y` is very often an AIRA-*resolved* estimate (`estimate:p90-prior`, `estimate:oom`) rather than anything the caller declared, and a line that presents an estimate as "this job's cap" without saying where the number came from invites the same false belief it exists to correct. The basis string already exists on the status as `ReserveBasis`.

**AIRA never rewrites a caller's explicit `--memory`.**

### 3.7 Trailer facets — visible, never silent

Two new `ConfineStatus` fields (json-tagged, so AIRA-22's durable record keeps them), rendered by `FormatConfineStatus` **only when a runtime was detected**, so every existing trailer stays byte-identical.

`container=`
- **`podman:split-injected`** — AIRA injected `--cgroups=split`. **v2 rename** (Sol P1 / Fable MF3): `podman:nested` asserted an outcome nothing verifies — an absent or pre-2.0 podman rejects the flag and exits, and the trailer would still have read `nested`. Every other facet names verified evidence (`scope=placed` follows a real `Members()` check; `oom.group=set` follows the write). This one now names the action actually performed; its doc states that when podman honours it, the container is a descendant of this scope bound by its `memory.max`.
- `podman:caller-split` — the caller passed `--cgroups=split` themselves.
- `podman:caller-cgroup` — caller set `--cgroups`/`--cgroup-parent`/`--pod`; AIRA injected nothing and claims nothing.
- `docker:not-contained` — outside `aira.slice` entirely, whatever else was done.

`container-memory=`
- `injected=<bytes>` | `caller=<bytes>` | `caller=unevaluated:<reason-with-token>` | `caller=<bytes>:reserve-requested` | `caller=<bytes>:reserved` | `caller=<bytes>:reserve-skipped:<declared|delegate-ram|podman>` | `not-injected:below-runtime-minimum` | `none`

(`:reserve-skipped:podman`, not `:podman-nested` — "nested" is the outcome word removed from the `container=` facet above, and it must not reappear here (Fable note 8).)

A caller can tell from the trailer alone whether AIRA **requested real containment** (`podman:split-injected`), did **best-effort accounting** (`docker:not-contained` + `:reserved`), or **could establish nothing** (`caller=unevaluated:…`).

### 3.8 Signature stability

Verified by both reviewers against `confine_linux.go:497/501` and `resource_estimate.go:18-27`: the signature is computed from the **original** argv before any injection, and only a launch-local copy is mutated. History **keys** are unchanged. The **value** distribution does fork, and §3.5 states it rather than presenting stability as a pure win.

### 3.9 Podman absence

Nothing probes for podman. If it is absent the wrapped command fails on its own exactly as today; AIRA claims nothing and adds no failure mode.

### 3.10 F10 — OOM attribution (v2: no false claim **and** no silence)

v1 proposed swapping the hierarchical counter for `LocalOOM()`. Both reviewers found that insufficient: `LocalOOM()` is also true when an **ancestor** slice limit fired (`usage_linux.go:93-96`), so "at its memory cap" would still misattribute an AIRA-27 slice-collateral kill; and suppressing the line alone leaves a container OOM producing **exit 137 and nothing** (podman's own clean exit gives `terminated-by=normal`, and `formatConfineTerminationAdvisory (:1699)` only fires on the unattributed-SIGKILL verdict).

So, three mutually exclusive operator lines, each stating exactly what its evidence establishes:
- **(a) own limit fired** — `LocalOOM()` returns `(killed=true, evaluated=true)` **and** the local `oom` max-breach declaration is **positively** > 0. An unreadable counter does **not** qualify (build review, Sol P1: an earlier build accepted `!ownEvaluated || own`, which asserted "at its memory cap" from a counter that established nothing). Such a run is not left silent — line (c) covers it with the same actionable advice minus the claim, and is therefore allowed to speak on the `oom` verdict, where `formatConfineOOMLimitAdvisory` is deliberately quiet → the existing `confine: job OOM-killed at its memory cap <cap>` line, unchanged. Stays gated on `scopeMemoryMax > 0` (it names the cap).
- **(b) descendant OOM** — hierarchical `oom_kill > 0` **and** `LocalOOM()` returns exactly `(killed=false, evaluated=true)` **and** local `oom == 0` → `confine: memory.events records N OOM kill(s) beneath this scope (a descendant's own limit, e.g. a container's --memory); the OOM killer killed nothing belonging to this scope itself.`
- **(c) attribution unestablished** — hierarchical `> 0` and `LocalOOM()` is `evaluated=false` → `confine: an OOM kill occurred under this scope; whose limit could not be established.`

**Two precision requirements from Fable SHOULD-FIX 2, both load-bearing:**
- Lines **(b) and (c) must NOT inherit the reserve advisory's `scopeMemoryMax > 0` gate** (`formatConfineReserveAdvisory`, `:1130-1131`, returns `""` for an uncapped scope). Otherwise a container OOM at its own `-m` inside an **uncapped** job is silent again — reintroducing exactly the silence this section exists to remove. They are therefore emitted from their own site, not from inside the reserve advisory.
- (b)'s condition is spelled as `LocalOOM() == (killed=false, evaluated=true)`; the phrase "local evaluated-false" is ambiguous and reads as the *unreadable* case. Its wording says what the counters establish ("the OOM killer killed nothing belonging to this scope itself") rather than "this scope's own limit did not fire", because a max-breach can be **declared while killing nothing** (`usage_linux.go:65-67`), so the local `oom == 0` gate is required for the stronger claim.

Never silence, never a fabricated attribution.

`deps.reportPeak(..., oom)` keeps the **hierarchical** counter — an estimate-quality signal, not an operator claim. **Stated consequence:** a container OOM at a caller's small `-m` marks the signature's history `oom`, and `EstimateMemoryReserve` (`resource_estimate.go:56`) bumps future estimates to headroom. Deferred deliberately (L6): re-deciding peak-RSS history semantics is out of scope with an accounting worktree in flight.

Both reviewers agree including F10 is **not** scope creep: this feature turns a rare false claim into a routine one.

### 3.11 Detached containers (`-d` / `--detach`) — Sol P0-4 / Fable 6

Two problems, one cheap answer. (a) Under split the container is **killed** when the job exits (F5) — a behaviour change — and `attestScopeTeardown`'s identity snapshot is leaf-only, so `DescendantKilled` never fires and the kill leaves **no trace** on the trailer. (b) For docker, the §3.5 reservation is released when the CLI exits while the daemon-owned container keeps running.

Detect `-d`/`--detach` with the over-inclusive `Present`-style scan and print, before launch:
- podman: `confine: this container will be killed when the confine job exits.`
- docker: `confine: this container outlives the confine job; any reservation made here is released when the job exits and does not cover the container's lifetime.`

Recorded as **L7**.

### 3.12 `confine --list` populated count — F11 / Fable MF4

`confine --list` renders **leaf** `cgroup.procs` under a column named `POPULATED`, so a **running** split job displays `0` — a direct honesty defect this feature would make routine.

**The fix is smaller than v2 assumed, and lives in the FACE.** AIRA-101 (now on master) already collects `ConfineRecord.SubtreePopulated` from `cgroup.events populated` (`confine_manage.go:47`, populated at `confine_manage_linux.go:125`) for the identical aitest-drain reason. It is simply **never rendered**: `cmd/aira/main.go:2596-2600` prints `record.Populated` only. So §3.12 is a **face-only** change, and the **data fields are left alone** — the reaper (`confine_manage.go:195`) and the daemon (`admit.go:1494`) deliberately consume `Populated`'s leaf semantics and must keep them.

Per Fable's recommendation and AIRA's no-backwards-compat rule: rename the column **`POPULATED` → `LEAF-PROCS`** and add a **`LIVE`** column (`yes`/`no`/`unevaluated`, from `SubtreePopulated`). The binding requirement, whatever the presentation: **a live split job must never render as a bare `0` under a column named `POPULATED`.**

The daemon-side half — `admit.go:1488-1496` skipping leaf-empty scopes in AIRA-74's post-restart reserve reconstruction — is **deliberately not touched** (**L8**).

---

## 4. Safety and invariants

- Nothing is injected into an argv AIRA did not detect under §3.1's two-token rule.
- A caller's explicit flag is **never** overridden or rewritten.
- The `Present` scan may only ever cause AIRA to **decline**; the §3.2 boundary proof is what makes that true for `Established` too.
- No value is guessed; ambiguity → `unevaluated` with a token-naming reason.
- The ledger is **never** charged for podman, and never raised over a caller-declared confine limit.
- A charge is reported as `:reserved` only when admission granted it.
- No new blocking path: no second lease, no capability probe, no extra `exec`.
- Existing trailers unchanged when no runtime is detected.
- **Docker is never reported as contained, in any code path.**
- The injected argv is a fresh slice; the durable record keeps the caller's original argv.

---

## 5. Known limitations, written down and accepted

- **L1 (F2):** each split run nests the job's own processes one level deeper. Bounded by path length, not policy. Podman's behaviour.
- **L2 (F2):** such a job reads `scope-integrity=migrated`, not `contained` — the leader is no longer a *direct* member. Containment is preserved (`witnessedEscape:1430-1438` → `pathEqualOrUnder`, so `<scope>/runtime` is not an escape). Precedent: aitest's `.aira-supervisor`. Cost: the facet is diluted for every podman job.
- **L3 (F5), observed at scale during the build:** podman's state DB shows a scope-killed container as `Up`, and because the scope kill pre-empts `--rm`, the records **accumulate**. Measured: the build's own test runs left **40** stale `alpine` entries in `podman ps -a` that had to be cleared by hand (`podman rm -f -a`), from a store that held zero beforehand. Outside AIRA's control — the container is killed by the kernel via `cgroup.kill`, so podman never gets to run its own cleanup — but anyone running many containers under confine should expect to prune the podman store periodically, exactly as `~/.local/state/aira/confine/` already needs pruning.
- **L4 (F4):** the leftover `runtime` cgroup blocks in-process rmdir; AIRA-36's reaper removes the subtree (up to ~7 min later). Deliberately not duplicated in-process.
- **L5 (§3.1):** rootful `sudo podman run`, `docker container run`, and all global-flag forms are undetected — no injection **and no warning**.
- **L6 (§3.10):** `reportPeak`'s hierarchical OOM flag unchanged, with its estimate-inflation consequence named.
- **L7 (§3.11):** detached-container lifetime versus job lifetime; warned, not fixed.
- **L8 (§3.12):** the daemon's post-restart reserve reconstruction (`admit.go:1488-1496`) skips a running split job because it gates on the leaf `Populated`. **This is a pre-existing, already-written-down deferral, not something this feature creates** — the code says verbatim "Subtree-aware liveness for adopted is a v2 item" and declares its own error direction safe ("under-counted → over-admit … never worse" than a fully forgotten pre-restart ledger). This feature only makes it more frequent. Fable reviewed this and accepted the deferral on that basis. The fix is now **mechanical** — gate adoption on `SubtreePopulated == true` alongside leaf `> 0`; the field is already on the record — but it is daemon admission surface with AIRA-103 in flight, so it is **ticketed as a follow-up**, not done here.
- **L9 (§3.2):** `docker run --rm alpine -m 4g` still establishes a phantom limit; distinguishing it needs a full flag-arity table, out of scope.

---

## 6. Tests (TDD) — anti-porous by construction

**Portable pure-function tests** (`container_test.go`):
- Detection positive/negative incl. every §3.1 undetected form.
- Scan established: `--memory=4g`, `--memory 4g`, `-m4g`, `-m 4g`, `--memory=4294967296`, `--memory=64M`, mixed case.
- Scan **unevaluated**: `-itm 4g`, two occurrences, trailing `-m`, `--memory=1p`, `--memory=garbage`, **`-m 0`**.
- **Boundary-proof negatives (the reviewers' counterexamples, load-bearing):** `docker run --rm qemu-image qemu-system-x86_64 -m 4G`, `docker run alpine echo --memory=8g`, `python -m http.server` — each must be `Present=true, Established=false`, producing **no** ledger raise and **no** `caller=<bytes>` facet.
- Short-flag refinement: `-v/home/mark:/x`, `-eTERM=xterm` set **neither** flag.
- Exclusions: `--memory-swap=2g`, `--memory-reservation=1g`, `--memory-swappiness=0`, both forms. Cgroup-flag matching does **not** fire on `--cgroupns`/`--cgroup-conf`.
- Injection asserts the **exact full argv**; every "not injected" case asserts argv **byte-identical** to input; an **aliasing test** asserts the caller's `request.Argv` backing array is untouched.

**`confineWithDeps`-level composition tests** with the existing fake admit/scope deps — these are what can catch the §3.5 defects; pure-helper tests cannot see the `:853` leak. Each asserts the `(Status.ReserveBytes, Status.ScopeMemoryMax)` pair for a table row:
Assertions are **exact equalities, never inequalities** (Fable note 12) — an inequality passes against an implementation that raises to the wrong number:
- podman + `--memory-max 2G` + `-m 8g` → charge **== 2G**, scope **== 2G** (must not become 8G).
- podman unpinned + `-m 8g` → charge **==** the no-container baseline, exactly.
- docker + `--memory-max 2G` + `-m 8g` → charge == 2G, scope == 2G, **and** the docker disagreement line emitted.
- docker unpinned + `-m 8g`, daemon-grant path → charge == 8G, scope == 8G.
- docker unpinned + `-m 8g`, timeout/unevaluated path → facet `:reserve-requested`, **not** `:reserved`.
- **docker unpinned + `-m 8g`, flock-`immediate` path** → facet `:reserve-requested`. This is the case Fable SHOULD-FIX 1 exists for: the fallback reports state `immediate` *with* a lock, so a `:reserved` keyed on `admission.state` would claim a ledger charge that never happened. This test is what makes the daemon-grant predicate load-bearing.
- **docker unpinned + `-m 512M`** (container limit *below* the 4 GiB hint) → charge == 4G, pinned (the accepted over-book of §3.5).
- delegate-ram + docker `-m 8g` → charge **==** baseline, facet `:reserve-skipped:delegate-ram`.

**Advisory-line tests:** the podman loud line (including its `<basis>`), the docker loud line, both `-d`/`--detach` lines, and the unconditional docker warning.

**§3.10 in both directions:** `{OOMKill:1, OOMKillLocal:0, OOMGroupKillLocal:0}` must **not** produce the own-cap line and **must** produce the descendant line; `{OOMKillLocal:1, local oom>0}` must produce the own-cap line; `{OOMKill:1, locals nil}` must produce the unevaluated line.

**Trailer:** every facet value renders; a status with **no** detected runtime renders byte-identically to a **literal expected string**, not the function's own prior output.

**Unconditional docker warning** asserted on the neither-side-specifies case (the ticket's own bullet 4).

**Real-podman / real-cgroup tests** (build-tagged, skipped when podman or a usable slice is absent — never a fabricated pass):
- The container's cgroup path is a **descendant of that job's own scope**, and the test **must fail if AIRA injected nothing** (assert the produced argv *and* the observed nesting — the v1 test could have passed with the test itself supplying the flag).
- The payload cgroup's `memory.max` **equals the injected bytes** — the assumption the whole podman memory story rests on. (F7 already measured this by hand; the test makes it a standing guarantee.)
- A container is asserted **live** before teardown, then absent after.
- `confine --list` reports **`LIVE=yes`** for a running split job (§3.12) — not merely "non-zero populated", which the leaf count can never satisfy.
- Isolation: unique `--name`, `--rm`, tiny `alpine`, **podman only, never docker**, nothing enumerates or touches containers the test did not create.

**Full suite:** `aira confine -- go test ./...` and `go vet ./...`, exit codes recorded exactly, serialised.

---

---

## 6b. AIRA-101 interaction (checked, post-rebase)

Fable verified against the rebased tree: AIRA-101's exclusive gate computes `liveScopes` from **`SubtreePopulated`** (`admit.go:1479`), so a running split job counts as **live** and **cannot fake an exclusive grant** — the one interaction that could have been a safety hole is already closed by AIRA-101's own design. Its `confine_linux.go` hunks (`:615-660`, `:952-985`, `:1105`) do **not** touch the reserve-raise or injection seams this change uses.

Its `exclusive=` facet is also the precedent §3.7 follows: rendered only when the feature was requested, "absent entirely when `--exclusive` was not asked for", so ordinary trailers stay byte-identical.

---

## 7. Deferrals (explicit)

Bare-slice fallback not implemented (§3.3); compose/shell-wrapped/global-flag forms out of scope (§3.1, L5); `reportPeak`'s hierarchical OOM flag (L6); split depth growth (L1); detached-container lifetime (L7); daemon-side reserve reconstruction (L8, follow-up ticket); §3.2's residual phantom (L9); no podman capability probe — a pre-2.0 podman fails loudly with an unrecognised-flag error and the trailer names the injected flag.
