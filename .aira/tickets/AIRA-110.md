---
{"schema":1,"id":"AIRA-110","project":"aira","title":"aira confine scopes: memory.max does not bound swap, so a confined job's cap is not the bound it appears to be","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","cgroup","confine","oom"],"hold":false,"relations":[]}
---
Found while measuring AIRA-35 (aitest worker scopes), and deliberately NOT
fixed there: the same latent property applies to every `aira confine` scope
on the machine, which is a materially larger blast radius than one ticket's
worth of change.

## The property

cgroup-v2's `memory.max` bounds a cgroup's *memory* (anon + file + kernel),
not memory + swap. Swap is bounded separately by `memory.swap.max`, which
defaults to `max` (inherited, i.e. unbounded up to the nearest ancestor's
limit). **No production AIRA code path writes `memory.swap.max` on any
scope** — a repo-wide grep for it returns only `_test.go` files.

`aira.slice` itself carries `MemorySwapMax=8G`
(`~/.config/systemd/user/aira.slice`), so the aggregate is bounded, but each
individual confine scope's `scope-memory.max` is not the containment bound
its own status line implies: a job that exceeds its granted reserve is
reclaimed into swap rather than being killed by its scope, until the shared
8 GiB slice-wide swap budget is exhausted.

## Measured (AIRA-35's probe, this host: WSL2 6.18.33.2, 20 GiB swap active)

A 32 MiB-capped scope with `memory.oom.group=1`, running a process that
allocates and touches 512 MiB:

- swap uncapped: **never OOM-killed, child exits 0**, ~520 MiB written to
  swap (identical at 256 MiB cap / 1 GiB allocation: exits 0, 820 MiB
  swapped; identical for a slow 8 MiB-per-100 ms leaker).
- `memory.swap.max=0` on the scope: `oom_group_kill` in 0.03-0.48 s.

## Why this matters beyond aitest

1. The reserve ledger and `Σgranted ≤ cap−headroom` accounting are stated
   over `memory.max`. A job whose real footprint escapes into swap is not
   bounded by the number the ledger reserved for it.
2. `confine_peak_history` / `resolveAdmitReserve` size future reserves from
   observed peak RSS. A job that swaps records a *deflated* peak, so the
   estimate for the next run of the same signature is systematically low —
   a feedback loop toward under-reservation.
3. The confine status line reports `scope-memory.max=enforced=N`, which is
   true as written but reads as a containment guarantee it does not make on
   a host with swap.

## Not obviously "just set it to 0"

Unlike an aitest worker (a disposable pytest process whose OOM the design
already treats as normal + requeue-once), a confined job is arbitrary user
work — a build, a test suite, a training run. Killing one that would have
completed by swapping is a behaviour change with real cost, and the right
answer may be a per-job policy (`--swap-max`, default inherit) rather than a
blanket 0. That design call is exactly why this is its own ticket rather
than a line in AIRA-35.

relates AIRA-35, AIRA-29, AIRA-67.
