---
{"schema":1,"id":"AIRA-58","project":"aira","title":"confine --admit-timeout is silently clamped to a hardcoded 30-minute daemon-side ceiling","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["confine","daemon","dogfood"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-59","to":"AIRA-58"}]}
---
## Symptom

Reported by peer session `qual`, reproduced twice independently: `aira confine --delegate-ram --memory-max 32G --memory-reserve 512M --admit-timeout 2h -- make merge-gate` rejected both times at exactly 30m0s with `E_ADMIT_SATURATED: confine: admission rejected after 30m0s — slice contended, no memory admission within the wait (reserve 32G/unknown)`, despite the explicit `--admit-timeout 2h`. Box was genuinely contended at the time (not a case of correctly timing out at the right value) — the wait was cut short at 30 minutes regardless of what was requested.

## Root cause (verified by direct source read)

The client-side flag threading is correct: `cmd/aira/main.go` parses `--admit-timeout` into `admitTimeout` (~line 860-867) and sets `AdmissionMaxWait: admitTimeout` on the confine request (~line 882); `internal/runner/confine_linux.go:831-833` correctly uses `request.AdmissionMaxWait` (falling back to a 30-minute default only when unset/zero).

The bug is server-side: `internal/daemon/admit.go:25` declares `admitWaitCapMs int64 = 30 * 60 * 1000` — a hardcoded, uncondition al, non-configurable 30-minute ceiling. The client's requested `max_wait_ms` is silently clamped to this at TWO call sites — `internal/daemon/admit.go:905-913` and `internal/daemon/worker_admit.go:349-350` — both doing the same `if maxWait > admitWaitCapMs { maxWait = admitWaitCapMs }`, with no error, no warning, and nothing in the response indicating the requested value was overridden. A caller who explicitly asks for `--admit-timeout 2h` on a genuinely contended shared box gets silently bounced at 30 minutes instead, then has to notice the discrepancy themselves and retry in a loop — exactly what `qual` had to do as a workaround.

## Why this matters

This is the same "asked for X, silently got Y, told nothing" dishonesty pattern this project has already found and fixed once tonight (AIRA-51's admission-wait message). `--admit-timeout` is a caller-configurable knob with no documented ceiling anywhere in its usage string (`confine [--slice S] [--name N] [--owner ID] [--memory-reserve S] [--memory-max S] [--memory-high S] [--admit-timeout D] [--delegate-ram] -- <argv...>`) — a caller has no way to know their request was overridden short of independently timing the rejection.

## Suggested direction

At minimum, either honor the requested `max_wait_ms` up to some genuinely large sanity ceiling (if an upper bound is wanted at all, it should be far more generous than 30 minutes given this project's own admission waits routinely exceed that under real contention — this session alone saw waits past 800s and 1300s tonight), or if a hard operational ceiling is genuinely desired, surface it honestly: reject the request up front with a clear error naming the actual ceiling (so the caller learns synchronously, not after silently waiting the wrong duration), or include the effective/clamped value in the rejection response instead of echoing back a number that implies the caller's own requested duration was honored. Check whether `admitWaitCapMs` should simply be raised, or made configurable, rather than assuming 30 minutes is intentional — nothing in the surrounding code comments explains why that specific figure was chosen.

## Resolution

Fixed on branch `aira58-59-admission-fairness`, together with AIRA-59 (the two
compound and could not safely ship apart — see below).

**There were THREE silent clamps, not one.** This ticket stated the client-side
threading was correct, having traced `internal/runner/confine_linux.go:831-833`.
That was wrong, and it was the most important finding of the work:
`internal/runner/admission_linux.go:82` declared its own private
`runnerAdmitWaitCap = 30 * time.Minute` and clamped `max_wait_ms` at `:252-255`
**before the request was ever sent**, with the transport deadline derived from
the clamped value. A daemon-only fix would therefore have left
`--admit-timeout 2h` still arriving as 30m on the wire while every daemon-side
test passed — a plausible-looking non-fix. Found independently by three review
lineages and verified in source.

**Fix.** One shared `runner.AdmitWaitCeiling = 24h` used by the CLI, the runner
and the daemon; all three clamps removed. An over-ceiling request is now
**refused** and told the bound, never silently substituted:
- CLI: `--admit-timeout` is range-checked at parse time
  (`E_CONFINE_ARGUMENT_INVALID`), so the caller learns before any round-trip.
- Runner: `Runner.admit` enforces the ceiling before either admission path,
  because with the daemon down `admitWithFlock` would otherwise wait the raw
  requested duration.
- Daemon: refuses with a new stable terminal code `E_ADMIT_WAIT_TOO_LONG`.

**The refusal code had to be new, and that is the subtle part.** The runner
routes every code it does not explicitly recognise through `fail()` into the
flock fallback, which launches the job **outside the daemon ledger**. Refusing
with a generic `E_PROTOCOL` would therefore not have refused anything — it would
have turned a refusal into an unaccounted, uncapped launch: strictly worse than
the clamp it replaced. The runner now treats the new code as terminal *before*
parsing any payload, so even a malformed rejection body cannot degrade into a
fallback launch.

**`worker-admit` deliberately differs.** It keeps a 30-minute ceiling and refuses
with the existing `E_DAEMON_PROTOCOL`. Two reasons, both verified: it has no
`admitSlots` concurrency bound (so a 24h ceiling would permit unbounded retained
connections — filed as AIRA-63), and its client wraps any non-OK response as
`E_CONFINE_UNAVAILABLE`, which makes the aitest supervisor disable daemon
admission and run **unconfined**. It already classifies `E_DAEMON_PROTOCOL` as
permanent, so refusing with that code fails closed.

**Note on the reported reserve.** The repro showed `(reserve 32G/unknown)`
despite `--memory-reserve 512M`. That is a separate defect, now filed as
**AIRA-62**: `cmd/aira/main.go:857-859` unconditionally overwrites an explicit
`--memory-reserve` with `--memory-max`, even for `--delegate-ram`.

Plan and full review history:
`docs/superpowers/specs/2026-09-03-aira58-59-admission-wait-and-freeze-plan.md`.

## Deployed

Binary rebuilt from merged master (`9a65d47`) and the daemon restarted the same night, after holding the redeploy during a separate live-incident investigation (AIRA-67) so the restart wouldn't destroy forensic state. AIRA-67 concluded the incident was an external kill, unrelated to this subsystem, clearing the way to deploy. The temporary `AIRA_DAEMON_ADMIT_BACKFILL_GRACE=2h` mitigation applied earlier that night was removed before this restart — it's superseded by this fix's own dedicated, reviewed bounding mechanism (`admitFreezeMaxHold`), and keeping it would have run the new freeze logic outside the configuration it was actually tested under.
