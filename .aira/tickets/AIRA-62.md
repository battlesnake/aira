---
{"schema":1,"id":"AIRA-62","project":"aira","title":"confine CLI forces reserve = --memory-max even for --delegate-ram, silently overriding --memory-reserve","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","daemon","dogfood"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-62","to":"AIRA-28"}]}
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

## DONE — PR #17, merged `a79d621` (2026-09-04)

Took the **second** suggested direction: the decision moved entirely into the runner. The CLI
lines are DELETED, not guarded — a face transcribes what the operator typed and never resolves
it. The rule is now an exported pure `runner.ResolveConfineReserve`
(`internal/runner/confine.go`), one decision site. Plan + both review records:
`docs/superpowers/plans/2026-09-04-aira62-memory-max-reserve-collapse-plan.md`.

Line numbers had shifted since filing (Phase 0): the CLI hunk was `main.go:906-908`, the runner
guard `confine_linux.go:493-496`.

**Confirmed WORSE than filed.** This ticket describes the explicit-`--memory-reserve` case. The
same three lines also made `--delegate-ram --memory-max N` *alone* charge N, so the accidental
over-reservation never needed `--memory-reserve` to be present. Both are fixed. The documented
non-delegate up-charge (`skill.go:318`) is preserved exactly, and the cap written to the scope
is never altered — only the charge.

**Why the runner's guard was dead:** `cmd/aira/main.go` holds the *only* non-test producer of a
`ConfineRequest` (confine has no MCP tool). Three runner subtests asserting the correct
delegate-ram behaviour therefore passed while the product did the opposite — a porous-test-at-
the-wrong-layer case. They are load-bearing now, and new CLI-boundary tests close the layer gap
that hid it.

**The over-commit safety argument this ticket demanded.** Sol plan-review BLOCKed on it and
RELEASED the block at build-review. Decisive evidence is [[AIRA-28]]'s own body: it lists
*"`--memory-max` on delegate now charges N"* among **its own** deliverables — coherent there
only because that design *also* sets scope `memory.max` == the charged reserve — and AIRA-28 is
SHELVED by owner pivot to [[AIRA-29]], itself owner-held ON HOLD. So the 32G charge was one
accidental cell of a declined design, binding only jobs whose operator typed an optional flag,
while every delegate job without `--memory-max` already booked 512M against the same ceiling.
Its harm is realised three times: [[AIRA-24]] (1785s wait then `E_ADMIT_SATURATED`, zero tests
run), [[AIRA-59]] (machine-wide stall from double-booking), [[AIRA-67]]. Residual recorded on
AIRA-28.

**Tests:** CLI transcription+resolution table (9 rows incl. the `AIRA_CONFINE_RESERVE`
contract), runner resolution table at the `deps.admit` seam (9 rows incl. value-ordering cases
where the charge legitimately exceeds the cap — safe direction, pinned not clamped), resolver
edge values (7 cases), and a wire-frame test driving the real `admitConfine` over a real socket.
Both review lineages flagged a possible P0 that the admit frame might carry the RAW rather than
the resolved `pinned`; refuted in source and pinned by that wire test. Mutation-verified in both
directions.

**Verification:** GH Actions `build+vet+gofmt` and `test` both pass on the merged commit; local
`./internal/runner` with `AIRA_REAL_CGROUP=1` exit 0; local full `make ci` exit 0 on the
production code as merged.

**Unblocks** simplification-programme Phase 4 (CLI codegen, candidates 24/31) — and the codegen
now has *nothing* to encode for this rule, since no policy remains in the CLI.

**Left for the owner (deliberately not done here):** `internal/core/skill.go:318`'s
"UP-CHARGES … to N" is imprecise. The rule SETS the charge to the cap, so a declared reserve
*larger* than the cap is lowered to it (still exact, never under-booked, since the scope cannot
exceed its own `memory.max`). Pre-existing, incomplete rather than false, and shipped-doc edits
were an explicit deferral of this plan.
