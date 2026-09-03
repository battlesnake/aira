---
{"schema":1,"id":"AIRA-62","project":"aira","title":"confine CLI forces reserve = --memory-max even for --delegate-ram, silently overriding --memory-reserve","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","daemon","dogfood"],"hold":false,"relations":[]}
---
## Symptom

`aira confine --delegate-ram --memory-max 32G --memory-reserve 512M` charges the admission ledger **32G**, not the 512M the caller explicitly asked for. Same dishonesty class as AIRA-58: asked for X, silently got Y, told nothing.

## Root cause (verified in source)

`cmd/aira/main.go:857-859`:

```go
if maximum > 0 {
    reserve, reservePinned = maximum, true
}
```

Unconditional, no delegate-ram guard, and it runs **after** `--memory-reserve` is parsed, overwriting it. Because it happens in the CLI, the runner's own guard at `internal/runner/confine_linux.go:459-461` (`if !request.DelegateRAM && request.ScopeMemoryMax > 0`) is **dead for every CLI caller** — the value has already been substituted upstream.

## Evidence this is what was observed live

- AIRA-58's reproduction was rejected with `(reserve 32G/unknown)` despite passing `--memory-reserve 512M`.
- AIRA-59's observed ledger read `33280M granted` = 32768M + 512M exactly.

So the giant head waiters that triggered the AIRA-59 machine-wide stall were delegate-ram merge-gates reserving their whole scope cap **in addition to** their per-test `confine-reserve` children — precisely the double-booking the delegate-ram overhead reserve exists to prevent (`confine_linux.go:449-454`).

## Why this was not fixed alongside AIRA-58/59

It is a memory-accounting change in the **over-commit direction**: correcting it drops the ledger charge for such a job from 32G to 512M. That is only safe if the per-test children genuinely cover the suite's usage — which is not true when the pytest RAM governor is disabled, or when the delegate-ram payload is not pytest at all. That safety argument deserves its own adversarial review rather than riding along in a PR already spanning daemon, runner and CLI.

## Suggested direction

Apply the same `!DelegateRAM` condition in the CLI that the runner already has, or move the decision entirely into the runner so there is one place that decides it. Either way, an explicit `--memory-reserve` must never be silently discarded — refuse the combination or honour it, but do not substitute.

Discovered during AIRA-58/AIRA-59 (plan `docs/superpowers/specs/2026-09-03-aira58-59-admission-wait-and-freeze-plan.md` §2.2).
