# AIRA-102 — `aira confine` container integration (podman transparent nesting + docker sanity shim)

**Status:** plan v1 — for plan-review (Fable gate, Sol + DeepSeek orthogonal lineages).
**Ticket:** AIRA-102 (P1). Owner-directed two-part remedy, see the ticket's "Owner-directed remedy (2026-09-05)".
**Branch:** `aira102-container-integration`, off `origin/master` @ `ee23834`.
**Author:** Opus, grounded on **live empirical probes on this machine** (podman 4.9.3 rootless, cgroup v2, systemd cgroup manager, real `aira.slice`) — every claim in §2 was measured, not reasoned.

---

## 1. Problem

`aira confine -- docker run ...` confines only the docker **CLI client**; the container is spawned by the system-managed `dockerd` and never enters `aira.slice` (AIRA-102's original finding, unchanged). Rootless `podman run` **can** be contained, but only if the caller knows to hand it a `--cgroup-parent` by hand. Two gaps:

1. Podman containment requires caller knowledge AIRA should supply itself.
2. A `docker run` under confine looks confined and is not — the exact "silent success masking failure" trap.

---

## 2. Empirical findings (measured live, 2026-09-05)

All probes ran under real `aira confine` against the real `aira.slice`; podman had **zero** containers before and after (the machine's 33 **docker** containers were never touched).

| # | Finding |
|---|---|
| **F1** | `podman run --cgroups=split` places the container at `<confine-scope>/[runtime/]*libpod-payload-<id>` — a genuine cgroup **descendant of THIS confine job's own scope**. Its memory is charged to that job's `memory.max`. **Per-job nesting works.** |
| **F2** | Split relocates the confine job's **own** processes into `<cur>/runtime`, one level deeper per `podman run` in the same job (measured: `/runtime`, `/runtime/runtime`, `/runtime/runtime/runtime` after 3 runs). Confine then reports `scope-integrity=migrated`. |
| **F3** | `--cgroups=split` is **mutually exclusive** with `--cgroup-parent`: podman refuses with `cannot specify --cgroup-mode=split with a cgroup-parent`. |
| **F4** | Split leaves a `runtime` child cgroup behind; `Scope.Remove()` → `os.Remove` fails `ENOTEMPTY` (error already discarded), so the scope dir lingers. **AIRA-36's recursive orphan reaper (already on master) removes such subtrees on its sweep.** `confine --list` shows the lingering scope with `POPULATED 0` — honest, not a phantom running job. Measured. |
| **F5** | Scope teardown **does** kill the nested container (`cgroup.kill` is recursive) — verified: the container's pid was gone after the confine job exited. Podman's own state DB is left stale (`podman ps` reports `Up` for the dead container until podman reconciles). |
| **F6** | The fallback `--cgroup-parent=aira.slice` works (ticket-verified): container is a **sibling** of confine scopes under the slice, bound only by the 64G aggregate cap, and **survives** the confine job's exit. |
| **F7** | `--memory=<bare byte count>` is accepted by podman. Units are case-insensitive and binary (`64M` == `64m` == 64 MiB), matching AIRA's own `parseMemorySize`. **`--memory=1048576` (1 MiB) fails** — `container init was OOM-killed (memory limit too low?)`. 4 MiB and 6 MiB succeed. |
| **F8** | `-m 8g` inside a 2G confine scope is **accepted** and runs; the kernel's ancestor cap still binds. A container limit above the job limit is therefore **not** a contradiction. |
| **F9** | `--cgroup-manager=cgroupfs --cgroup-parent=<scope path>` **fails** (`OCI runtime error`) from a scope that still holds processes: the no-internal-process rule blocks enabling `memory` in `subtree_control`. It only succeeded in a probe where a prior `--cgroups=split` had *already* drained the scope. So it is **not** an alternative to split, and it mutates the scope's `subtree_control` as a side effect. |
| **F10** | **Honesty defect, pre-existing, amplified by this change.** `confine_linux.go:993` derives its `oom` flag from the **hierarchical** `memory.events` `oom_kill`, so a *descendant's own-cap* OOM makes confine print `confine: job OOM-killed at its memory cap 2G` — a false claim. Measured: a container OOM-killed at its own 1 MiB `-m` produced exactly that line on a job that exited 0. AIRA already has `cgroupUsage.LocalOOM()`, which distinguishes these correctly. |

**F1 answers the ticket's open question: per-job cgroup-parent nesting IS achievable**, via `--cgroups=split` rather than `--cgroup-parent`. F9 closes the only alternative. So Part 1 delivers real per-job nesting, not the bare-slice fallback.

---

## 3. Design

### 3.1 Detection — deliberately narrow (portable, pure)

New portable file `internal/runner/container.go`. Detection fires **only** when:

- `base(argv[0])` (text after the last `/`) is **exactly** `docker` or `podman`, **and**
- `argv[1]` is exactly `run`.

Everything else is out of scope and stays out: `compose` / `podman-compose` / `docker compose`, `exec`, `play`, `pod`, `podman-remote` (base name differs), `sudo podman run` (argv[0] is `sudo`), and **any** invocation where the runtime is inside an opaque shell string (`sh -c "docker run ..."`). This is stated in the code doc, the SKILL text, and the trailer docs — never left to be assumed wider.

### 3.2 Memory-flag scan — conservative, ambiguity-honest (portable, pure)

One left-to-right pass over `argv[2:]`. It answers **two separate questions**, deliberately with different strictness:

- **`Present`** (over-inclusive, safe direction) — is any memory-limit-shaped token anywhere after `run`? Set by `--memory`, `--memory=…`, `-m`, `-m<x>`, or a single-dash cluster containing `m`. `--memory-swap`, `--memory-reservation`, `--memory-swappiness` are explicitly **excluded** (both `=` and space forms). Over-detection only ever makes AIRA **decline to inject** and say so — it can never break a command or fabricate a limit.
- **`Established` + `Bytes`** (strict) — the value is known **only** when there is exactly **one** occurrence, its value parses via the existing shared `parseMemorySize`, and no ambiguity marker was seen. Otherwise `Established=false` with a stated `Reason`, reported as `unevaluated`. **Never guessed.**

Ambiguity markers (each → `Present=true, Established=false`): more than one occurrence; a single-dash cluster containing `m` that does not start `-m` (e.g. `-itm 4g` — the value position is not determinable without a full CLI parser); a `-m`/`--memory` as the last token with no value; a value `parseMemorySize` rejects (e.g. `1p`, which docker's own parser accepts and AIRA's does not — reported unevaluated rather than guessed).

**Stated, accepted limitation:** the scan does not locate the image argument, so a memory-flag-shaped token in the *container's own* command (`podman run alpine prog -m 4`) is counted as `Present`. The consequence is conservative — AIRA declines to inject and reports it — never a wrong limit. Determining the image would require a full docker/podman flag-arity table, which the ticket explicitly rules out.

The same pass records whether the caller already set `--cgroups` / `--cgroup-parent` (any form), and the literal value of `--cgroups` when given.

### 3.3 Part 1 — podman

At launch construction (immediately before `confineSetupArgv`, after `scopeMemoryMax` is resolved), insert flags **at index 2** (directly after `run`, always a valid flag position for both runtimes):

- **Placement.** If the caller set neither `--cgroups` nor `--cgroup-parent` → inject **`--cgroups=split`**. Per F1 the container becomes a descendant of this job's own scope and is bound by its `memory.max`. If the caller set either → **inject nothing** (F3: split and `--cgroup-parent` are mutually exclusive, so injecting would break their command) and report their choice rather than claiming containment.
- **Memory.** If confine has a resolved `scopeMemoryMax > 0`, the scan says `Present=false`, and `scopeMemoryMax >= 6 MiB` (F7's runtime floor, docker's documented minimum) → inject `--memory=<scopeMemoryMax as bare bytes>`. Below the floor, inject nothing and say why.

**No `--cgroup-parent=aira.slice` fallback is used**, because the nested form works. The bare-slice form is documented in the ticket as the weaker alternative and is deliberately *not* implemented: it would place the container outside the job's own reservation and let it survive the job (F6).

### 3.4 Part 2 — docker

Docker gets **no placement flag** — there is nothing to place it into (the original finding stands). It gets:

- **Memory injection**, on the same rule as podman: confine has a limit, the caller has none → inject `--memory=<scopeMemoryMax>`. This aligns the container's *own* cap with what the caller asked confine for. It is **not** containment and is never reported as such.
- **Ledger reservation**, when the caller *does* declare a limit and it is `Established` (§3.5).
- **An unconditional warning line**, emitted before the child starts, for **every** detected `docker run` regardless of what was injected:

  > `confine: docker run detected — the container runs under the system docker daemon, OUTSIDE aira.slice. AIRA cannot contain it: nothing here bounds it against the slice cap, and this remains true whatever confine injected or reserved. Use `podman run` for kernel-enforced containment.`

  This is the requirement that the shim must not become the trap it counters.

### 3.5 Ledger reservation from a caller-declared container limit

Applies to **both** runtimes. Immediately **after** `reserve, pinned := ResolveConfineReserve(request)` (so the `declaredReserve` / `declaredReserveBytes` provenance captured above it is untouched):

```
if plan.Detected && plan.Memory.Established && !request.DelegateRAM && plan.Memory.Bytes > reserve {
        reserve, pinned = plan.Memory.Bytes, true
}
```

- **`max`, never replace.** A raise can only ever charge *more*, so a tiny container limit can never shrink the job's reserve and starve the CLI client. This is what makes the rule safe without a magic floor.
- **`!DelegateRAM` guard** preserves AIRA-62's invariant: a delegate-ram job's pinned reserve is deliberately small framework overhead, and raising it would double-book the per-test reservations. Reported as skipped, with the reason.
- **No second `confine-reserve` lease.** Folding the charge into the job's own single admission is strictly simpler than acquiring a second daemon lease while holding the first (which would add a new blocking path and a deadlock surface for no accounting benefit). Architectural simplicity: one ledger charge per job stays one ledger charge per job.
- The trailer states plainly that a docker reservation is **machine-level accounting only** — the container's memory is not inside the slice it was charged against.

### 3.6 Disagreement policy — the owner's explicit open question

**Decision: never refuse the launch. Report both numbers, and warn loudly in the one case that implies a false belief.** Reasoning, weighed against this project's fail-closed discipline:

1. **Fail-closed governs *claims*, not the user's command.** AIRA's rule is that a check which cannot establish its result reports `unevaluated` rather than a fake pass. A disagreement is not an unestablished check: both numbers are known exactly and can be printed exactly. The honesty obligation is discharged by stating them, not by refusing.
2. **For podman the two limits are not in conflict at all.** With split-nesting the job cap is a kernel-enforced *ancestor* bound and the container limit is a nested sub-limit; whichever is smaller binds, automatically and correctly. F8 measured `-m 8g` inside a 2G scope running fine and still bounded. Refusing a "disagreement" would refuse a correct, kernel-enforced composition.
3. **For docker they are not two answers to one question.** Confine's cap governs the CLI client's cgroup in `aira.slice`; docker's `-m` governs a container cgroup in the system tree. Refusing would assert a relationship — and an authority over the container — that AIRA demonstrably does not have.
4. **Refusal has a real cost and zero safety benefit.** The container escapes `aira.slice` whether or not the command runs. Blocking it contains nothing; it only breaks a workflow and pushes callers off the shim that exists to inform them.
5. **Existing precedent in this very code path.** `ResolveConfineReserve` already *resolves* a reserve-versus-cap mismatch ("a declared reserve LARGER than the cap is lowered to the cap — still exact, never under-booked") rather than refusing. Confine refuses only when a value is **unusable** (sub-minimum declared reserve) or when containment would be **silently** lost. Neither applies: the value is usable and nothing here is silent.

**The one loud case.** When docker's declared limit **exceeds** confine's own limit, a caller most plausibly believes the smaller confine cap binds the container. It does not. An extra line names it explicitly:

> `confine: docker --memory=<X> exceeds this job's own limit <Y>; the container is NOT bound by <Y>, nor by aira.slice.`

AIRA **never rewrites a caller's explicit `--memory`** in any case.

### 3.7 Trailer facets — visible, never silent (the `terminated-by=` precedent)

Two new `ConfineStatus` fields, with json tags so AIRA-22's durable detached record keeps them, rendered by `FormatConfineStatus` **only when a runtime was detected** (so every existing trailer is byte-identical):

`container=<value>`
- `podman:nested` — `--cgroups=split` injected; the container is a descendant of **this job's** scope, bound by its `memory.max`. **Real containment.**
- `podman:nested:caller` — the caller themselves passed `--cgroups=split`.
- `podman:caller-cgroup` — the caller set `--cgroups`/`--cgroup-parent`; AIRA injected no placement flag and does **not** claim containment.
- `docker:not-contained` — outside `aira.slice` entirely; no containment, whatever else was done.

`container-memory=<value>`
- `injected=<bytes>` — AIRA supplied the container's own cap from confine's limit.
- `caller=<bytes>` — the caller's own limit, parsed exactly.
- `caller=unevaluated:<reason>` — a memory flag is present; AIRA declined to guess its value.
- `caller=<bytes>:reserved` — the caller's limit raised this job's ledger reserve.
- `caller=<bytes>:reserve-skipped:delegate-ram` — raise withheld per §3.5.
- `not-injected:below-runtime-minimum` — confine's limit is under the 6 MiB runtime floor (F7).
- `none` — neither side specified a limit.

A caller can therefore tell from the trailer alone whether **real containment** happened (`podman:nested`), **best-effort accounting** happened (`docker:not-contained` + `caller=…:reserved`), or **nothing could be established** (`caller=unevaluated:…`).

### 3.8 Signature stability

`request.ResourceSignature` is computed from the **original** argv at line 497, before any injection. Injection happens at the launch site only. Peak-RSS history keys are therefore unchanged by this feature — a job's history does not fork the day AIRA starts injecting a flag.

### 3.9 Podman absence

Nothing probes for podman. If podman is not installed the wrapped command fails on its own, exactly as it does today; AIRA claims nothing. Injection adds a flag to a command that was going to fail anyway. **No new failure mode is introduced for a machine without podman.**

### 3.10 F10 — the false "job OOM-killed" claim

**In scope, minimal fix.** `confine_linux.go:993` becomes `oom` from `usage.LocalOOM()` for the **operator-facing reserve advisory only**, so a descendant's own-cap OOM no longer produces the false `confine: job OOM-killed at its memory cap <job cap>` line. The machinery already exists and is already documented for exactly this distinction.

`deps.reportPeak(..., oom)` keeps the **hierarchical** counter unchanged — that is an estimate-quality signal, not an operator claim, and re-deciding peak-RSS history semantics is out of scope with a live accounting worktree in flight. Recorded as a deferral, not silently.

Justification for including it: this change makes a nested descendant cap **routine** inside a confine job, turning a rare pre-existing false claim into a common one. Shipping the feature that causes it while leaving the false claim in place would be the exact failure this project's review policy exists to prevent.

---

## 4. Safety and invariants

- **Nothing is injected into an argv that AIRA did not detect** under §3.1's two-token rule.
- **A caller's explicit flag is never overridden or rewritten** — not `--memory`, not `--cgroups`, not `--cgroup-parent`.
- **Over-detection is the only permitted error direction** for the `Present` scan: it can make AIRA decline to act, never act wrongly.
- **No value is ever guessed.** Ambiguity → `unevaluated` with a stated reason.
- **A ledger raise can only increase the charge** (`max`), so it can never under-book, and can never shrink a job's own cap.
- **No new blocking path**: no second daemon lease, no capability probe, no extra `exec`.
- **Existing trailers are unchanged** when no runtime is detected — the two facets render only on detection.
- **Docker is never reported as contained**, in any code path, whatever was injected or reserved.

---

## 5. Known limitations, written down and accepted

- **L1 (F2):** each `podman run --cgroups=split` in one confine job nests the job's own processes one cgroup level deeper. Bounded by path length, not policy. A job running very many containers *sequentially* nests deeply. Podman's own behaviour; not correctable from AIRA without reimplementing split.
- **L2 (F2):** such a job reads `scope-integrity=migrated` rather than `contained`, because the leader is no longer a *direct* member of the scope. Containment is preserved and the escape detector is correct (`witnessedEscape` uses `pathEqualOrUnder`, so a move into a descendant is not an escape). Precedent: aitest's `.aira-supervisor` relocation already produces this reading.
- **L3 (F5):** podman's state DB can show a scope-killed container as `Up` until podman reconciles. Outside AIRA's control.
- **L4 (F4):** the leftover `runtime` cgroup blocks confine's in-process rmdir; AIRA-36's recursive reaper (already on master) removes the subtree on its sweep, and `confine --list` shows `POPULATED 0` meanwhile. **Deliberately not duplicated in-process** — architectural simplicity; the reaper is the designed backstop for nested scopes.
- **L5:** rootful `sudo podman run` is not detected (argv[0] is `sudo`) and would resolve against the system manager anyway.
- **L6:** `reportPeak`'s hierarchical OOM flag is unchanged (§3.10) — deferred, not silent.

---

## 6. Tests (TDD)

**Portable unit tests** (`container_test.go`, run everywhere):
- Detection: `podman run` / `docker run` / `/usr/bin/podman run` fire; `podman-remote run`, `docker compose`, `podman pod`, `sudo podman run`, `sh -c "docker run …"`, `podman build`, bare `podman` all do **not**.
- Memory scan, value established: `--memory=4g`, `--memory 4g`, `-m4g`, `-m 4g`, `--memory=4294967296`, `--memory=64M`, mixed case.
- Memory scan, `Present` but **unevaluated**: `-itm 4g` (cluster), two occurrences, trailing `-m` with no value, `--memory=1p`, `--memory=garbage`.
- Memory scan **not** tripped by `--memory-swap=2g`, `--memory-reservation=1g`, `--memory-swappiness=0` (both forms).
- Caller cgroup flags detected in all four forms; `--cgroups=split` distinguished from other values.
- Injection construction: index-2 placement; nothing injected when the caller has a cgroup flag; nothing injected below the 6 MiB floor; docker never receives a placement flag.
- **Anti-porous:** each injection test asserts the **exact full argv** produced, so a test cannot pass against an implementation that injects the wrong flag, the wrong value, or at the wrong position. Each "not injected" test asserts argv is **byte-identical** to the input.
- Reserve raise: raises on `>`, does not lower on `<`, skipped under `DelegateRAM`, skipped when `Established=false`.
- `FormatConfineStatus`: every facet value renders; **a status with no detected runtime renders byte-identically to today** (regression guard against trailer drift).

**Real-cgroup / real-podman tests** (build-tagged like the existing real-cgroup suite, skipped when podman or a usable slice is absent — never a fabricated pass):
- A `podman run --cgroups=split` job's container cgroup path is a **descendant of that job's own scope** (the F1 claim, asserted rather than assumed).
- Confine's own resolved `scopeMemoryMax` is injected as `--memory` when the caller gave none.
- Scope teardown leaves no live container process (F5).
- **Isolation:** unique `--name` per test, `--rm`, tiny `alpine`, short-lived; podman only, never docker; nothing enumerates or touches containers the test did not create.

**Full suite:** `aira confine -- go test ./...` plus `go vet ./...`, exit codes recorded exactly, serialised (never concurrent with another heavy job).

---

## 7. Deferrals (explicit, not silent)

- The bare-slice `--cgroup-parent=aira.slice` fallback is **not** implemented (§3.3) — nesting works, and the fallback is strictly weaker.
- `compose` / `podman-compose` / shell-wrapped invocations: out of scope by ticket direction.
- `reportPeak`'s hierarchical OOM flag (§3.10 / L6).
- Depth growth under repeated split runs (L1): not mitigated.
- No podman capability probe (§3.9): a podman too old for `--cgroups=split` (pre-2.0, 2020) fails loudly with an unrecognised-flag error, and the trailer names the injected flag. Chosen over an extra `exec` on every launch.
